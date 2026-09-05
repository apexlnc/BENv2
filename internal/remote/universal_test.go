package remote_test

import (
	"context"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/runnertest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// The remote substrate runs the universal contract **unmodified** — the same
// suite internal/fake runs over the local one (#192).
//
// That is the acceptance criterion and also the only way the claim means
// anything: a "universal" contract each substrate got to adapt would be two
// suites that happen to share a name. Nothing here scripts a case; it supplies
// the four scenarios and lets runnertest decide what they must produce.
func TestUniversalContract(t *testing.T) {
	runnertest.Run(t, runnertest.Contract{
		Name:          "remote",
		SessionID:     testSession,
		FailureReason: core.FailureCrashed,
		Start:         startScenario,
	})
}

// startScenario builds a fresh substrate per case and scripts one scenario onto
// it.
//
// Fresh per case, because a Runner refuses a second dispatch for a claim cycle
// that already has one — the no-duplicate-dispatch rule — and a shared rig would
// make the second case fail for a reason that has nothing to do with what it
// asserts.
func startScenario(t *testing.T, s runnertest.Scenario) core.RunHandle {
	t.Helper()
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })

	// Scripted after the dispatch, which is when the run exists. The pump is
	// already blocked in Events by now and wakes on the first emission, so the
	// ordering below is what the consumer sees.
	switch s {
	case runnertest.ScenarioSuccess:
		rig.backend.Emit(rig.run,
			remotetest.Init(testSession),
			remotetest.Text("working"),
			remotetest.Usage(10, 20, 0.5),
			remotetest.Success())
		rig.backend.Quiet(rig.run)
	case runnertest.ScenarioFailure:
		rig.backend.Emit(rig.run,
			remotetest.Init(testSession),
			remotetest.Failure(core.FailureCrashed))
		rig.backend.Quiet(rig.run)
	case runnertest.ScenarioLive:
		rig.backend.Emit(rig.run, remotetest.Init(testSession))
	case runnertest.ScenarioUnstoppable:
		rig.backend.Emit(rig.run, remotetest.Init(testSession))
		// The backend cannot prove the run is gone, so a Stop reaches
		// PhaseSignaled and stops there — delivery, not termination.
		rig.backend.SetConfirmable(rig.run, false)
	default:
		t.Fatalf("unscripted scenario %q", s)
	}
	return h
}

// A run handle is a core.RunHandle, checked where the orchestrator would need it
// to be rather than only where the suite happens to use one.
var _ core.RunHandle = (*remote.Attempt)(nil)

// And a runner is a core.AgentRunner: the whole point of the composition is that
// assembly can hand this to a loop that knows nothing about substrates.
var _ core.AgentRunner = (*remote.Runner)(nil)
