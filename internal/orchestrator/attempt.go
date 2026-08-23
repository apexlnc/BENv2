package orchestrator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The attempt-outcome record (#60): one structured entry per dispatch, emitted
// where the attempt ends.
//
// §9.11's log records *edges* and §10.3's run records the *present*. Neither
// answers the questions week two of dogfooding asks — what fraction of tickets
// land on attempt 1, what a completed ticket costs, which failure reason
// dominates, how long an attempt takes — because every one of those is a fact
// about a whole attempt, and an attempt only becomes a whole at its last edge.
// `core.Usage` in particular already crosses the boundary, is spent on the §9.9
// cost cap, and is then discarded.
//
// Nothing reads it back. §9.10's statelessness invariant is untouched: this is a
// readout, not a store, and a daemon that finds an empty state dir behaves
// exactly as one that finds a full one.

// AttemptOutcome is one finished attempt, as the loop knows it.
//
// No struct tags, for TransitionEntry's reason: the on-disk shape belongs to the
// state dir, which this package must not import, and a second set of tags here
// would be a second answer to what the file looks like.
type AttemptOutcome struct {
	Issue string
	// Attempt and Turns are the §9.6 budgets as they stood when the attempt
	// ended. Attempt is not a count of this issue's records — §9.6's attempt
	// floor can start a fresh claim at 2.
	Attempt int
	Turns   int
	// RunID is the §10.3 correlation handle this attempt shares with its log
	// lines, its §9.11 edges and its transcript.
	RunID string
	// Agent names what ran, so a later comparison between adapters is a query
	// rather than a new pipeline (#62). It comes from the bundle, which is where
	// the definition and the adapters are already bound together — the loop never
	// sees a provider block (SPEC §3.6).
	Agent core.AgentDescriptor

	// StartedAt is when the attempt was dispatched — the entry to `preparing`,
	// not the launch. The worktree preparation and the §9.7 evidence check are
	// part of what a queue's throughput is made of and part of what an operator
	// waits through, so they are inside the span rather than either side of it.
	StartedAt time.Time
	EndedAt   time.Time
	// Ran reports that a process was actually started. An attempt that died
	// preparing its worktree is still an attempt — it spent a §9.6 budget and it
	// belongs in the failure histogram — but it has no duration or usage worth
	// averaging.
	Ran bool

	// FailureReason is the §7.3 verdict the attempt ended on, empty when it did
	// not fail. Verdict is what §9.7's evidence check made of a run that claimed
	// success, VerdictUnknown when verification never ran.
	//
	// Both, because neither implies the other: a `succeeded` event with an
	// `incomplete` verdict is the continuation track, and it is neither a failure
	// nor a finished ticket.
	FailureReason core.FailureReason
	Verdict       Verdict

	// Usage is what this attempt reported (SPEC §7.2), accumulated per attempt —
	// unlike the §9.9 cap, which is cumulative over the issue. Zero cost against
	// non-zero tokens means the adapter reports no price, which core.Usage names
	// as an ordinary answer.
	Usage core.Usage
}

// AttemptSink is the durable half of the attempt log — the state dir. Nil leaves
// outcomes in memory only, which is what every orchestrator test that is not
// about persistence runs with.
//
// The consequence of nil is worth stating because it is *not* the transition
// sink's: nothing in BEN reads an attempt outcome back, so a nil sink costs
// telemetry and costs no decision. A daemon with one behaves identically.
type AttemptSink interface {
	// Append records one outcome, on TransitionSink.Append's contract: an error
	// means it was not recorded, and ErrSinkUnwritable means retrying is futile.
	Append(AttemptOutcome) error
}

// AttemptLog is the in-memory tail plus the durable append behind it, on
// TransitionLog's shape and for the same reasons.
type AttemptLog struct {
	mu       sync.Mutex
	outcomes []AttemptOutcome

	queue durableQueue[AttemptOutcome]
}

func (l *AttemptLog) attach(sink AttemptSink, log *slog.Logger) {
	l.queue.noun, l.queue.nouns = "attempt outcome", "attempt outcomes"
	l.queue.attrs = func(o AttemptOutcome) []any {
		return []any{"issue", o.Issue, "attempt", o.Attempt, "run_id", o.RunID}
	}
	if sink == nil {
		return
	}
	l.queue.attach(sink.Append, log)
}

func (l *AttemptLog) append(o AttemptOutcome) {
	l.mu.Lock()
	l.outcomes = append(l.outcomes, o)
	l.mu.Unlock()
	l.queue.enqueue(o)
}

func (l *AttemptLog) persist(ctx context.Context) { l.queue.persist(ctx) }

func (l *AttemptLog) flush() { l.queue.flush() }

// Outcomes returns a copy of the outcomes recorded so far.
func (l *AttemptLog) Outcomes() []AttemptOutcome {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]AttemptOutcome(nil), l.outcomes...)
}

// For returns the outcomes for one issue, in order.
func (l *AttemptLog) For(identifier string) []AttemptOutcome {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []AttemptOutcome
	for _, o := range l.outcomes {
		if o.Issue == identifier {
			out = append(out, o)
		}
	}
	return out
}

// recordAttempt emits this attempt's outcome, exactly once, at the moment the
// attempt is over.
//
// **Its own guard, not attemptEnded's.** The §6.5 after-run hook is owed only to
// an attempt that had a worktree with a process in it; an outcome record is owed
// to every attempt that was *dispatched*, including one that died in Prepare —
// that attempt spent a §9.6 budget, and "which failure reason dominates" is
// answered wrongly by a log that omits the launch errors. Sharing the hook's
// `ranThisAttempt` guard would have dropped exactly those, silently, leaving a
// histogram that looked complete.
//
// **The reason and the verdict are arguments, never read off the record.**
// Record.FailureReason is sticky across a retry by design — it says what last
// went wrong, not what this attempt did — so an attempt that succeeded on the
// second go would inherit the first one's `crashed` and be counted as a failure
// that also published. The caller has just decided what ended this attempt; that
// is the value, and there is no second copy of it to drift.
//
// Called from every site that ends an attempt, which is every site that calls
// attemptEnded: the failure track, the verification verdict, a confirmed stop,
// an exit that overtook an attempt in flight, an attempt that ended while the §9.6
// prior-attempt account was being read (endAttemptSuspended), and the drain.
//
// The `attemptRecorded` guard therefore does more than make one route idempotent:
// the last two can both fire for one record, deliberately, and it is what makes the
// pair a *default* rather than a race — the site that knows the reason states it,
// and the drain's `killed` fills the gap only where nothing did (driveShutdown).
//
// **The drain is one of them**, which it briefly was not. A suspended record's
// attempt is over rather than paused — §9.10 resumes the *issue*, as a new
// attempt, which is exactly why finishSuspended fires the §6.5 after-run hook
// there. Excluding it meant a SIGTERM cost the attempt's whole duration, usage
// and reason, on the one shutdown an operator is most likely to be investigating.
func (o *Orchestrator) recordAttempt(r *Record, reason core.FailureReason, verdict Verdict) {
	if r.attemptRecorded || r.attemptStartedAt.IsZero() {
		// No start instant means no attempt was ever dispatched under this
		// record — a claim released before `preparing`, say. Nothing to describe.
		return
	}
	r.attemptRecorded = true
	o.Attempts.append(AttemptOutcome{
		Issue:         r.Issue.Identifier,
		Attempt:       r.Attempt,
		Turns:         r.Turns,
		RunID:         r.runID(),
		Agent:         r.attemptAgent,
		StartedAt:     r.attemptStartedAt,
		EndedAt:       o.clock.Now(),
		Ran:           r.ranThisAttempt,
		FailureReason: reason,
		Verdict:       verdict,
		Usage:         r.attemptUsage,
	})
}
