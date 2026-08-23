package orchestrator

import (
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The prompt as the *orchestrator* renders it, and the §5.6 ceiling that can
// refuse a dispatch outright.
//
// What a retry is told — the prior-attempt account and its byte budget — is
// summary_test.go, and #50's config-level tests already prove
// WorkflowDefinition.RenderPrompt applies the configured limit. What is only
// observable here is the wiring: that the loop renders through that limit, with
// the record's own definition snapshot, and that the refusal lands before an
// agent exists.

// An oversized prompt refuses the dispatch, and refuses it *before* an agent
// exists (SPEC §5.6, #50). The ceiling is on attacker-controlled token spend —
// an issue body is written by whoever filed the issue — so a run that started
// and was then judged too large would have already spent what the limit exists
// to bound.
//
// The refusal is a non-retryable launch_error: the same template and the same
// inputs render the same way, so a retry is a second identical refusal.
//
// This is the wiring #50's config-level tests cannot see. They prove
// WorkflowDefinition.RenderPrompt applies the configured ceiling; only here is
// it observable that the *orchestrator* renders through it, with the record's
// own definition snapshot.
func TestOversizedPromptRefusesTheDispatchBeforeLaunching(t *testing.T) {
	issue := fake.Issue("1", epoch)
	// The template emits the title, and the title is untrusted, so it renders
	// fenced (SPEC §5.6) — this is well past a 300-byte prompt either way.
	issue.Title = strings.Repeat("very long ticket title ", 40)

	h := start(t, harnessOpts{
		issues:      []core.Issue{issue},
		extraConfig: "  max_prompt_bytes: 300\n",
	})

	// A terminal record is released, so the log is what outlives it.
	h.WaitGone("1")
	if path := h.o.Transitions.Path("1"); !containsState(path, StateFailed) {
		t.Fatalf("path = %v, want it to reach %q", path, StateFailed)
	}
	if n := h.Runner.StartCount(); n != 0 {
		t.Errorf("started %d runs; an oversized prompt must be refused before an agent is launched", n)
	}

	// The reason survives in the log, so an operator reading it learns the
	// ceiling refused the prompt rather than meeting a mystery launch failure.
	var reasons []string
	for _, e := range h.o.Transitions.For("1") {
		reasons = append(reasons, e.Reason)
	}
	joined := strings.Join(reasons, " | ")
	if !strings.Contains(joined, "rendering the prompt") || !strings.Contains(joined, "300") {
		t.Errorf("transition reasons %q name neither the render nor the configured ceiling", joined)
	}
	// And it reaches the tracker as the §7.3 reason, which is what a human
	// unparking the issue reads.
	if calls := strings.Join(h.Tracker.Calls, " | "); !strings.Contains(calls, "comment 1="+string(core.MilestoneFailed)) {
		t.Errorf("tracker calls %q carry no failed milestone", calls)
	}
}
