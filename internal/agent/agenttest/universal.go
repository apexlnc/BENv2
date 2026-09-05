package agenttest

import (
	"context"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/runnertest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Universal adapts this local runner contract to the substrate-neutral
// RunHandle contract. Both production local adapters call runnertest.Run over
// this value unchanged; only their native stream encoder differs.
func Universal(c Contract) runnertest.Contract {
	return runnertest.Contract{
		Name:          c.Name,
		SessionID:     c.Fake.SessionID(),
		FailureReason: core.FailureCrashed,
		Start: func(t *testing.T, scenario runnertest.Scenario) core.RunHandle {
			return startUniversal(t, c, scenario)
		},
	}
}

func startUniversal(t *testing.T, c Contract, scenario runnertest.Scenario) core.RunHandle {
	t.Helper()
	script := scriptUniversalLive
	opts := Options{}
	switch scenario {
	case runnertest.ScenarioSuccess:
		script = scriptUniversalOK
	case runnertest.ScenarioFailure:
		script = scriptUniversalFail
	case runnertest.ScenarioLive:
	case runnertest.ScenarioUnstoppable:
		var release atomic.Bool
		opts.Signal = func(pgid int, sig syscall.Signal) error {
			if sig == 0 || release.Load() {
				return SignalGroup(pgid, sig)
			}
			return nil
		}
		raw := c.start(t, c.runner(t, script, nil, opts), c.spec(t, core.RunLimits{}))
		t.Cleanup(func() {
			release.Store(true)
			raw.Stop(context.Background(), core.StopDiscard)
		})
		return &releaseAfterStop{RunHandle: raw, release: &release}
	default:
		t.Fatalf("unknown universal scenario %q", scenario)
	}
	return c.start(t, c.runner(t, script, nil, opts), c.spec(t, core.RunLimits{}))
}

type releaseAfterStop struct {
	core.RunHandle
	release *atomic.Bool
}

func (h *releaseAfterStop) Stop(ctx context.Context, mode core.StopMode) core.Termination {
	termination := h.RunHandle.Stop(ctx, mode)
	h.release.Store(true)
	return termination
}
