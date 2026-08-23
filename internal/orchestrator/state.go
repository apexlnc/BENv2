package orchestrator

import (
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// State is one of the nine states of SPEC §9.2. The run record carries
// attempt and failure reason as fields rather than multiplying states
// (SPEC §9.1).
type State string

const (
	StateQueued      State = "queued"
	StateClaimed     State = "claimed"
	StatePreparing   State = "preparing"
	StateRunning     State = "running"
	StateVerifying   State = "verifying"
	StateBackoff     State = "backoff"
	StateDone        State = "done"
	StateNeedsReview State = "needs-review"
	StateFailed      State = "failed"
)

// nonTerminalStates are the seven the kill edge is legal from: the six a run
// passes through, plus the parked needs-review.
//
// needs-review belongs here because §9.2 calls it *parked* rather than
// terminal, gives it an edge back to `backoff`, and writes the kill edge as
// "any **non-terminal** → failed". Reading "active" for "non-terminal"
// excluded exactly one state, and it is the state a daemon shutting down is
// most likely to be holding: parked runs accumulate waiting for humans.
//
// Enumerated, and never derived as `!terminal`. State is a string type, so a
// negation is true of every value that is not one of the two — `"unknown"`,
// `""`, `"DONE"`, `"verifying "` — and each of those would silently acquire a
// legal edge into failed. A closed map has to be closed from both directions:
// membership is the question, not non-membership of its complement.
var nonTerminalStates = map[State]bool{
	StateQueued:      true,
	StateClaimed:     true,
	StatePreparing:   true,
	StateRunning:     true,
	StateVerifying:   true,
	StateBackoff:     true,
	StateNeedsReview: true,
}

// NonTerminal reports whether the state is a known one a run can still leave,
// which is what §9.2's kill edge is legal from.
func (s State) NonTerminal() bool { return nonTerminalStates[s] }

// transition is one edge of the closed map.
type transition struct{ from, to State }

// legalTransitions is SPEC §9.2's table, verbatim and closed. The value is
// the trigger, which the transition log records as the reason (§9.11).
//
// The kill edge (*any non-terminal state* → failed) is not enumerated here;
// it is handled by Allowed.
var legalTransitions = map[transition]string{
	{StateQueued, StateClaimed}:        "selected by dispatch; claim verified by read-back",
	{StateClaimed, StatePreparing}:     "claim verified",
	{StatePreparing, StateRunning}:     "worktree ready, before_run passed, agent started",
	{StatePreparing, StateBackoff}:     "retryable prep or hook failure, attempts remain",
	{StatePreparing, StateFailed}:      "non-retryable prep failure or attempts exhausted",
	{StateRunning, StateVerifying}:     "runner reported succeeded",
	{StateRunning, StateBackoff}:       "retryable failure, attempts remain",
	{StateRunning, StateFailed}:        "non-retryable failure or attempts exhausted",
	{StateRunning, StateNeedsReview}:   "budget exceeded",
	{StateVerifying, StateDone}:        "publish evidence complete",
	{StateVerifying, StatePreparing}:   "clean exit without publish evidence; continuation re-dispatch",
	{StateVerifying, StateNeedsReview}: "evidence contradicts the claim, or max_turns exhausted",
	{StateBackoff, StatePreparing}:     "backoff fired; issue active and routable; slot free",
	{StateBackoff, StateBackoff}:       "backoff fired; no free slot; requeued",

	// Added to the §9.2 table by the 2026-08-08 amendment (#38): the human
	// unpark its own prose, §9.8 and two §9.10 passages already required.
	{StateNeedsReview, StateBackoff}: "human removed the state label; re-queued",

	// Added by the 2026-08-13 amendment (#49): §9.5's content-bound approval
	// parks an issue whose content moved after the approving label, or whose
	// edit the tracker cannot order against it. The park has to be legal from
	// wherever a dispatch decision is made — the claim read-back, and the §9.6
	// re-fetch on both retry tracks. `verifying → needs-review` is the third and
	// was already in the table.
	{StateQueued, StateNeedsReview}:  "content changed after the approving label; parked for reapproval",
	{StateBackoff, StateNeedsReview}: "content changed after the approving label; parked for reapproval",

	// Added by the 2026-08-19 amendment (#156, amendment 12): a credential
	// failure that is not explicitly transient is a misconfiguration, and a
	// misconfiguration fails identically on every retry. Parking costs a
	// human's attention; retrying it costs the same attention three attempts
	// later, with the operator error buried under two more launch failures.
	{StatePreparing, StateNeedsReview}: "unknown or permanent credential failure preparing the workspace or minting the publish credential",
}

// Allowed reports whether from → to is in the closed map.
func Allowed(from, to State) bool {
	if to == StateFailed && from.NonTerminal() {
		// Kill is legal from every non-terminal state (SPEC §9.2), which
		// includes the parked needs-review.
		return true
	}
	_, ok := legalTransitions[transition{from, to}]
	return ok
}

// IllegalTransitionError is the loud error an illegal edge earns. SPEC §9.2
// is explicit that this is a bug, not a no-op to swallow.
type IllegalTransitionError struct {
	Issue    string
	From, To State
}

func (e *IllegalTransitionError) Error() string {
	return fmt.Sprintf("illegal transition for issue %s: %s → %s is not in the SPEC §9.2 map", e.Issue, e.From, e.To)
}

// stateLabel projects a state onto its tracker label (SPEC §9.3). The
// transient distinctions — preparing, verifying, backoff — deliberately
// collapse onto ben:claimed: they belong in `ben status`, not in label churn.
func stateLabel(s State) core.StateLabel {
	switch s {
	case StateClaimed, StatePreparing, StateVerifying, StateBackoff:
		return core.StateLabelClaimed
	case StateRunning:
		return core.StateLabelRunning
	case StateNeedsReview:
		return core.StateLabelNeedsReview
	case StateFailed:
		return core.StateLabelFailed
	default:
		// queued has no label yet; done has had them removed.
		return core.StateLabelNone
	}
}
