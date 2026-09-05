package fake_test

import (
	"context"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/runnertest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The local substrate runs the universal contract **unmodified** — the same
// suite internal/remote runs over the backend one (#192).
//
// It earns its place twice over. It is half of "run unchanged by the local fake
// and the new remote fake", which is what makes the contract universal rather
// than remote-shaped; and it holds this fake to the outcomes the real local
// adapters are held to by agenttest, which nothing did before. A fake whose Probe
// or Stop drifted from the handle contract used to be discoverable only through
// an orchestrator test failing for a reason that looked like the orchestrator's.
//
// An external test package because it exercises this one from outside, the way
// its consumers do.
func TestUniversalContract(t *testing.T) {
	runnertest.Run(t, runnertest.Contract{
		Name:          "local",
		SessionID:     "session-abc",
		FailureReason: core.FailureCrashed,
		Start:         startScenario,
	})
}

func startScenario(t *testing.T, s runnertest.Scenario) core.RunHandle {
	t.Helper()
	r := fake.NewRunner()
	switch s {
	case runnertest.ScenarioSuccess:
		r.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "session-abc", Continuation: "session-abc"},
				{Type: core.EventProgress, Text: "working"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 10, OutputTokens: 20}},
				{Type: core.EventSucceeded},
			}
		})
	case runnertest.ScenarioFailure:
		r.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "session-abc", Continuation: "session-abc"},
				{Type: core.EventFailed, Reason: core.FailureCrashed},
			}
		})
	case runnertest.ScenarioLive:
		// The stream stays open, so the run ends only when something stops it.
		r.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "session-abc", Continuation: "session-abc"}}
		})
		r.SetHangAfterScript(true)
	case runnertest.ScenarioUnstoppable:
		// The domain survives teardown, which is what makes Stop report
		// unconfirmed and the orchestrator retain the claim (SPEC §9.8).
		r.SetScript(func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "session-abc", Continuation: "session-abc"}}
		})
		r.SetHangAfterScript(true)
		r.SetStopTermination(core.TerminationUnconfirmed)
	default:
		t.Fatalf("unscripted scenario %q", s)
	}

	h, err := r.Start(context.Background(), core.RunSpec{
		Workspace: core.WorkspacePaths{Path: "/tmp/ws"},
		Prompt:    "do the thing",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })
	return h
}
