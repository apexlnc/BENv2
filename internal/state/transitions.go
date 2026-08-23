package state

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// transitions.jsonl — SPEC §9.11's append-only transition log, one JSON object
// per line, oldest first. The durability machinery is jsonl.go's; this file is
// the record and the two questions asked of it.
//
// It has two consumers and they want different things. `ben status` renders the
// tail (§10.3). Recovery reads exactly one fact from it: the §7.3 reason behind
// this issue's last failure, which §9.10 step 6 names as the only route by which
// that reason survives a restart at all.

// transitionNoun names this log in the errors jsonl.go raises about it.
const transitionNoun = "transition"

// Transition is one line of the log.
//
// The first six fields are §9.11's tuple verbatim. The last two are not in that
// tuple and are here because §9.10 step 6 cannot be implemented without one of
// them, and §10.3's correlation attrs cannot be joined without the other.
type Transition struct {
	TS    time.Time `json:"ts"`
	Issue string    `json:"issue"`
	From  string    `json:"from"`
	To    string    `json:"to"`
	Actor string    `json:"actor"`
	// Reason is §9.11's reason: the *trigger* for the edge, which for most of
	// the §9.2 map is the map's own text. It is not the §7.3 failure taxonomy —
	// see FailureReason, and mind that the two sit side by side.
	Reason string `json:"reason"`

	// RunID ties this edge to the attempt that caused it, to the daemon log
	// lines that carry the same value, and to the run record in runs.json
	// (§10.3). Empty on edges that belong to no attempt.
	RunID string `json:"run_id,omitempty"`
	// FailureReason is the closed §7.3 verdict, and is set only on the edges a
	// failure caused.
	//
	// §9.11's tuple has one "reason" and B08 spent it on the trigger, which is
	// the right thing for a state machine's log to record. But §9.10 step 6
	// requires the *§7.3* reason to be recoverable from this log — it is the
	// stated route by which a `failed` milestone comment can name why, on the
	// ordinary same-host restart — and the trigger text cannot be parsed back
	// into a closed enum. So the taxonomy is carried as itself, in its own
	// field, and never inferred from Reason.
	FailureReason core.FailureReason `json:"failure_reason,omitempty"`
}

// TransitionWriter appends to the log. One per daemon; see appendLog for what
// holding it open and syncing every record buy.
type TransitionWriter struct{ *appendLog }

// AppendTransitions opens the log for appending, creating it if needed, after
// repairing an incomplete trailing record (see openAppendLog).
func (d Dir) AppendTransitions() (*TransitionWriter, error) {
	if err := d.Prepare(); err != nil {
		return nil, err
	}
	log, err := openAppendLog(d.TransitionsPath(), transitionNoun)
	if err != nil {
		return nil, err
	}
	return &TransitionWriter{appendLog: log}, nil
}

// Append writes one record durably, and either the record is in the file
// afterwards or nothing of it is (see appendLog.appendRecord).
func (w *TransitionWriter) Append(t Transition) error {
	body, err := json.Marshal(t)
	if err != nil {
		return fmt.Errorf("state: encoding a transition: %w", err)
	}
	return w.appendRecord(body, t.Issue)
}

// TransitionReader reads the log. It holds no file handle: every call opens,
// reads and closes, so a reader can outlive, precede, or run alongside the
// daemon that writes it without either holding the other open.
type TransitionReader struct{ path string }

// ReadTransitions names the log for reading.
func (d Dir) ReadTransitions() TransitionReader {
	return TransitionReader{path: d.TransitionsPath()}
}

// Tail returns the last n records, oldest first, and how many the log holds.
// n <= 0 returns everything.
//
// The total comes back because a caller rendering a window needs to say what it
// is a window *of*, and the only other way to learn it is to ask for everything —
// which is the thing this bound exists to prevent. One streaming pass answers
// both: memory is O(n), not O(file), over a log that v1 never rotates (§10.3
// leaves sinks and rotation to the supervisor, which is true of the daemon's
// stdout and not of this file).
//
// An absent log is ErrNoState, not an empty tail: `ben status` says "no
// transitions have been recorded here" rather than "this daemon has made no
// transitions", and those are different claims.
func (r TransitionReader) Tail(n int) ([]Transition, int, error) {
	var (
		ring  []Transition
		total int
	)
	err := walkLog(r.path, transitionNoun, func(t Transition) {
		total++
		ring = append(ring, t)
		if n > 0 && len(ring) > n {
			ring = ring[1:]
		}
	})
	if err != nil {
		return nil, 0, err
	}
	return ring, total, nil
}

// LastFailure reports the §7.3 failure recorded on this issue's most recent
// transition into the state named by `failed`, and whether one was found.
//
// This is the whole of SPEC §9.10 step 6's read. The three outcomes are
// deliberately distinct and a caller MUST keep them apart:
//
//   - found — the reason survived; recovery names it in the `failed` comment.
//   - not found, no error — a log was read and this issue's reason is not in it.
//     That is step 6's blessed degradation: the comment says the reason did not
//     survive the restart (core.MilestoneComment.ReasonUnavailable).
//   - error — including ErrNoState. We could not ask. Reporting this as "not
//     found" would state a fact about the run that nothing established, which is
//     §9.10's governing rule ("the absence of a fact is never evidence") applied
//     to the log itself.
//
// Scope, stated because the caller has to decide whether it is enough: this is
// the last `failed` edge *in this log*, and the log has no notion of a claim
// cycle (§9.10 step 2). On the row that reads it — `ben:failed` standing with
// the release unlanded — the last failure is the one that raised the standing
// label. A caller that wants to prove that compares core.RunFailure.At against
// its claim-cycle anchor.
func (r TransitionReader) LastFailure(issue string) (core.RunFailure, bool, error) {
	var (
		out   core.RunFailure
		found bool
	)
	err := walkLog(r.path, transitionNoun, func(t Transition) {
		if t.Issue != issue || t.To != failedState || t.FailureReason == "" {
			return
		}
		out = core.RunFailure{At: t.TS, Reason: t.FailureReason, Detail: t.Reason}
		found = true
	})
	if err != nil {
		return core.RunFailure{}, false, err
	}
	return out, found, nil
}

// failedState is §9.2's terminal `failed`, spelled here rather than imported.
//
// internal/state must not import the orchestrator: the loop is what will want to
// import a state dir, and a package cannot be on both ends of that. The string
// is the file format's, and orchestrator.StateFailed is pinned to it by a test
// on the writing side, where a rename would otherwise pass silently.
const failedState = "failed"
