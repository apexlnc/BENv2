package localdomain

import (
	"context"
	"errors"
	"time"
)

type supervisorState uint8

const (
	supervisorUnknown supervisorState = iota
	supervisorLive
	supervisorExited
	supervisorOldBoot
)

type cgroupState uint8

const (
	cgroupUnknown cgroupState = iota
	cgroupPopulated
	cgroupEmpty
	cgroupAbsent
	cgroupReplaced
)

type observation struct {
	supervisor supervisorState
	cgroup     cgroupState
	err        error
}

func decide(obs observation) (Termination, error) {
	if obs.supervisor == supervisorOldBoot {
		return TerminationConfirmed, nil
	}
	if obs.supervisor != supervisorExited {
		return TerminationUnconfirmed, obs.err
	}
	switch obs.cgroup {
	case cgroupPopulated:
		return TerminationUnconfirmed, errors.Join(ErrContainment, obs.err)
	case cgroupEmpty, cgroupAbsent, cgroupReplaced:
		return TerminationConfirmed, nil
	default:
		return TerminationUnconfirmed, obs.err
	}
}

type handleOps interface {
	observe(context.Context) observation
	term() error
	kill() error
}

// Handle owns observation and bounded teardown for one execution domain. Its
// backing cleanup record belongs to Manager and therefore outlives this value.
type Handle struct {
	ops     handleOps
	timings Timings
}

func newHandle(ops handleOps, timings Timings) *Handle {
	return &Handle{ops: ops, timings: timings.withDefaults()}
}

// Probe performs exactly one read-only observation.
func (h *Handle) Probe(ctx context.Context) (Termination, error) {
	if ctx.Err() != nil {
		return TerminationUnconfirmed, ctx.Err()
	}
	return decide(h.ops.observe(ctx))
}

// Stop observes, sends TERM only through the supervisor pidfd, then applies
// cgroup.kill and one final bounded observation window.
func (h *Handle) Stop(ctx context.Context, mode StopMode) (Termination, error) {
	status, err := h.Probe(ctx)
	if status == TerminationConfirmed {
		return status, nil
	}
	var actionErr error
	if termErr := h.ops.term(); termErr == nil {
		grace := h.timings.InterruptGrace
		if mode == StopDiscard {
			grace = h.timings.DiscardGrace
		}
		if status, observeErr := h.await(ctx, grace); status == TerminationConfirmed {
			return status, nil
		} else {
			err = errors.Join(err, observeErr)
		}
	} else {
		actionErr = errors.Join(actionErr, termErr)
	}
	if killErr := h.ops.kill(); killErr != nil {
		actionErr = errors.Join(actionErr, killErr)
	}
	status, observeErr := h.await(ctx, h.timings.KillGrace)
	if status == TerminationConfirmed {
		return status, nil
	}
	return TerminationUnconfirmed, errors.Join(err, actionErr, observeErr)
}

func (h *Handle) await(ctx context.Context, limit time.Duration) (Termination, error) {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	ticker := time.NewTicker(h.timings.PollInterval)
	defer ticker.Stop()
	var lastErr error
	for {
		status, err := decide(h.ops.observe(ctx))
		if status == TerminationConfirmed {
			return status, nil
		}
		lastErr = errors.Join(lastErr, err)
		select {
		case <-ticker.C:
		case <-deadline.C:
			status, err = decide(h.ops.observe(context.Background()))
			return status, errors.Join(lastErr, err)
		case <-ctx.Done():
			status, err = decide(h.ops.observe(context.Background()))
			return status, errors.Join(lastErr, ctx.Err(), err)
		}
	}
}
