package agenttest

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Domain is the adapter-contract execution domain. It drives real provider
// processes and streams while keeping OS-specific process-group mechanics out
// of the universal RunHandle cases and every production import path.
func Domain() harness.ExecutionDomain { return processDomain{} }

// SignalFunc is the process-group fault-injection seam used only by the local
// adapter contract. It is unreachable from the daemon's production graph.
type SignalFunc func(pgid int, sig syscall.Signal) error

type processDomain struct{ signal SignalFunc }

func (processDomain) Ready(context.Context) error { return nil }

type refusingDomain struct{ err error }

func (d refusingDomain) Ready(context.Context) error { return d.err }

func (d refusingDomain) Start(context.Context, harness.DomainLaunch) (harness.DomainRun, error) {
	return nil, d.err
}

func (d processDomain) Start(ctx context.Context, launch harness.DomainLaunch) (harness.DomainRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.Command(launch.Argv[0], launch.Argv[1:]...)
	cmd.Dir = launch.Dir
	cmd.Env = launch.Env
	cmd.Stdin = launch.Stdin
	cmd.Stdout = launch.Stdout
	cmd.Stderr = launch.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = launch.Timings.PostExitDrain
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	signal := d.signal
	if signal == nil {
		signal = SignalGroup
	}
	run := &processRun{
		pid: cmd.Process.Pid, signal: signal, timings: launch.Timings,
		done: make(chan struct{}),
	}
	go func() {
		_ = cmd.Wait()
		close(run.done)
	}()
	if launch.OnDomain != nil {
		evidence := core.RunEvidence{
			Scheme: "agenttest-domain-v1",
			Boot:   "agenttest-boot",
			ID:     strconv.Itoa(cmd.Process.Pid),
		}
		if err := launch.OnDomain(evidence); err != nil {
			run.stop(context.Background(), core.StopDiscard)
			return nil, fmt.Errorf("record test execution domain: %w", err)
		}
	}
	return run, nil
}

// SignalGroup exists only for process-backed adapter tests. The production
// local domain never calls it or falls back to it.
func SignalGroup(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }

type processRun struct {
	pid     int
	signal  SignalFunc
	timings harness.Timings
	done    chan struct{}
}

func (r *processRun) DirectDone() <-chan struct{} { return r.done }

func (r *processRun) Probe(ctx context.Context) core.Termination {
	if ctx.Err() != nil || !closed(r.done) || r.groupAlive() {
		return core.TerminationUnconfirmed
	}
	return core.TerminationConfirmed
}

func (r *processRun) Stop(ctx context.Context, mode core.StopMode) core.Termination {
	return r.stop(ctx, mode)
}

func (r *processRun) stop(ctx context.Context, mode core.StopMode) core.Termination {
	if r.Probe(ctx) == core.TerminationConfirmed {
		return core.TerminationConfirmed
	}
	grace := r.timings.StopGrace
	if mode == core.StopDiscard {
		grace = max(grace/10, 100*time.Millisecond)
	}
	_ = r.signal(r.pid, syscall.SIGTERM)
	if r.await(ctx, grace) {
		return core.TerminationConfirmed
	}
	_ = r.signal(r.pid, syscall.SIGKILL)
	if r.await(ctx, grace) {
		return core.TerminationConfirmed
	}
	return core.TerminationUnconfirmed
}

func (r *processRun) groupAlive() bool {
	err := r.signal(r.pid, syscall.Signal(0))
	return err == nil || !errors.Is(err, syscall.ESRCH)
}

func (r *processRun) await(ctx context.Context, grace time.Duration) bool {
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if closed(r.done) && !r.groupAlive() {
			return true
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			return closed(r.done) && !r.groupAlive()
		case <-ctx.Done():
			return closed(r.done) && !r.groupAlive()
		}
	}
}

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
