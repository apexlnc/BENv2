package orchestrator

import (
	"fmt"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// allStates is every state, for exhaustive pairing.
var allStates = []State{
	StateQueued, StateClaimed, StatePreparing, StateRunning, StateVerifying,
	StateBackoff, StateDone, StateNeedsReview, StateFailed,
}

// legalEdges restates SPEC §9.2 independently of the implementation's map.
// Restating rather than iterating legalTransitions is the point: a test that
// read the same table it checks would pass no matter what the table said.
var legalEdges = map[string]bool{
	"queued→claimed":         true,
	"claimed→preparing":      true,
	"preparing→running":      true,
	"preparing→backoff":      true,
	"preparing→failed":       true,
	"running→verifying":      true,
	"running→backoff":        true,
	"running→failed":         true,
	"running→needs-review":   true,
	"verifying→done":         true,
	"verifying→preparing":    true,
	"verifying→needs-review": true,
	"backoff→preparing":      true,
	"backoff→backoff":        true,
	"needs-review→backoff":   true,

	// Added by the 2026-08-13 amendment (#49). §9.5's content-bound approval
	// parks an issue whose content moved after the approving label, and the park
	// has to be legal from wherever a dispatch decision is made: the claim
	// read-back leaves the record in `queued`, and the §9.6 re-fetch reaches it
	// from `backoff` on the failure track and from `verifying` on the
	// continuation track. The third is already above.
	"queued→needs-review":  true,
	"backoff→needs-review": true,

	// The 2026-08-19 amendment (#156, amendment 12): an unknown or permanent
	// credential failure in workspace preparation, or in minting the publish
	// credential before the launch. A misconfiguration fails identically every
	// time, so parking costs a human's attention now rather than the same
	// attention three attempts later.
	"preparing→needs-review": true,

	// The kill edge. SPEC §9.2's row reads, verbatim:
	//
	//	| *any non-terminal* | failed | Kill (`failed(killed)`) — legal from
	//	every non-terminal state |
	//
	// Non-terminal, not active. The two differ by exactly one state and this
	// table used to say "active", which is how the implementation and its
	// independent restatement came to share a misreading: `needs-review` is
	// *parked*, has an edge back to `backoff`, and is the state a daemon
	// shutting down is most likely to be holding, because parked runs pile up
	// waiting for humans. preparing→failed and running→failed are already
	// above, from the enumerated rows.
	"queued→failed":       true,
	"claimed→failed":      true,
	"verifying→failed":    true,
	"backoff→failed":      true,
	"needs-review→failed": true,
}

func edge(from, to State) string { return string(from) + "→" + string(to) }

// The map is closed: every pair of states is either in §9.2 or an error.
// This is both halves of the acceptance criterion — every legal edge is
// asserted legal, and the fuzz over all 81 pairs asserts everything else errs.
func TestTransitionMapIsClosed(t *testing.T) {
	for _, from := range allStates {
		for _, to := range allStates {
			name := edge(from, to)
			want := legalEdges[name]
			if got := Allowed(from, to); got != want {
				t.Errorf("Allowed(%s) = %v, want %v", name, got, want)
			}
		}
	}
}

// Every legal edge must be reachable through the transition function, and
// every illegal one must produce the loud error §9.2 requires.
func TestTransitionRefusesIllegalEdges(t *testing.T) {
	h := start(t, harnessOpts{})

	for _, from := range allStates {
		for _, to := range allStates {
			r := &Record{Issue: issueFixture("t"), State: from}
			err := h.o.transition(t.Context(), r, to, "test")

			if legalEdges[edge(from, to)] {
				if err != nil {
					t.Errorf("transition %s returned %v, want it applied", edge(from, to), err)
				}
				if r.State != to {
					t.Errorf("transition %s left the record in %q", edge(from, to), r.State)
				}
				continue
			}

			var ill *IllegalTransitionError
			if !asIllegal(err, &ill) {
				t.Errorf("transition %s returned %v, want *IllegalTransitionError", edge(from, to), err)
				continue
			}
			if r.State != from {
				t.Errorf("a refused transition %s still moved the record to %q", edge(from, to), r.State)
			}
		}
	}
}

// The error names the edge, so an operator reading the log knows which
// invariant broke.
func TestIllegalTransitionErrorNamesTheEdge(t *testing.T) {
	err := &IllegalTransitionError{Issue: "7", From: StateDone, To: StateRunning}
	got := err.Error()
	for _, want := range []string{"7", "done", "running", "§9.2"} {
		if !containsAll(got, want) {
			t.Errorf("error %q does not mention %q", got, want)
		}
	}
}

// An illegal transition must not be silently swallowed into the log either:
// nothing is appended for an edge that did not happen.
func TestRefusedTransitionIsNotLogged(t *testing.T) {
	h := start(t, harnessOpts{})
	r := &Record{Issue: issueFixture("9"), State: StateDone}

	if err := h.o.transition(t.Context(), r, StateRunning, "test"); err == nil {
		t.Fatal("done → running was accepted")
	}
	if got := h.o.Transitions.For("9"); len(got) != 0 {
		t.Errorf("logged %v for a refused transition", got)
	}
}

// SPEC §9.11: every applied transition is one append-only entry carrying who
// and why.
func TestTransitionLogRecordsActorAndReason(t *testing.T) {
	h := start(t, harnessOpts{issues: []core.Issue{fake.Issue("1", epoch)}})
	h.Tick()
	h.WaitState("1", StateDone)

	entries := h.o.Transitions.For("1")
	if len(entries) == 0 {
		t.Fatal("nothing was logged")
	}
	for _, e := range entries {
		if e.Actor != "test-host/test-key" {
			t.Errorf("entry %+v has actor %q, want the daemon identity", e, e.Actor)
		}
		if e.Reason == "" {
			t.Errorf("entry %s→%s has no reason", e.From, e.To)
		}
		if e.TS.IsZero() {
			t.Errorf("entry %s→%s has no timestamp", e.From, e.To)
		}
	}
	if got := fmt.Sprint(h.o.Transitions.Path("1")); got != "[queued claimed preparing running verifying done]" {
		t.Errorf("path = %s", got)
	}
}

func asIllegal(err error, target **IllegalTransitionError) bool {
	if err == nil {
		return false
	}
	ill, ok := err.(*IllegalTransitionError)
	if ok {
		*target = ill
	}
	return ok
}

// The closed map has to be closed from both directions. `!from.Terminal()` was
// true of every value that is not one of the two terminal states — including
// near misses of them — so a typo'd or future state silently acquired a legal
// edge into failed, and §9.2's "loud error, not a no-op" would never fire for
// the case that most needs it.
func TestTheKillEdgeRefusesStatesTheMapDoesNotKnow(t *testing.T) {
	for _, s := range []State{"unknown", "", "DONE", "verifying ", "Needs-Review"} {
		t.Run(string(s), func(t *testing.T) {
			if Allowed(s, StateFailed) {
				t.Errorf("Allowed(%q → failed) = true; membership is the question, not non-membership of the complement", s)
			}
			if Allowed(s, StateBackoff) {
				t.Errorf("Allowed(%q → backoff) = true", s)
			}
			if Allowed(StateRunning, s) {
				t.Errorf("Allowed(running → %q) = true", s)
			}
		})
	}
}
