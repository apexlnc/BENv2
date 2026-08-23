package orchestrator

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// TransitionEntry is one line of the append-only transition log (SPEC §9.11).
// Milestone comments are this log's tracker-visible projection at the
// load-bearing transitions (SPEC §8.4).
//
// Deliberately carries no struct tags. The on-disk format is the state dir's
// (internal/state), which this package must not import — the loop is what a
// state dir imports, not the other way round — and a second set of tags here
// would be a second answer to what the file looks like.
type TransitionEntry struct {
	TS    time.Time
	Issue string
	From  State
	To    State
	Actor string
	// Reason is §9.11's reason: the trigger for the edge, which for most of
	// the §9.2 map is that map's own text.
	Reason string

	// RunID is the attempt this edge belongs to — the §10.3 correlation handle
	// shared with the daemon's log lines and the child's BEN_RUN_ID. Empty on
	// edges that belong to no attempt.
	RunID string
	// FailureReason is the closed §7.3 verdict, and only ever the one the
	// transition's own cause supplied.
	//
	// It is *not* read off Record.FailureReason, which is sticky by design —
	// "the §7.3 verdict of the most recent failure, if any", surviving into
	// the retry that follows. Stamping that field on every edge would put
	// `crashed` on the `preparing` and `done` of the attempt that recovered
	// from it, and a log that records a reason for a transition no failure
	// caused is worse than one that records none: §9.10 step 6 reads this
	// field, and would name it.
	FailureReason core.FailureReason
}

// FailureReasonReader is the whole of what SPEC §9.10 step 6 needs from the
// §9.11 transition log: the failure reason of one issue's last run, if it
// survived.
//
// Deliberately this narrow, and spelled in B11's own return type so that
// state.TransitionReader satisfies it structurally — no shim in the assembly,
// which is where a mistranslation between two spellings of "the last failure"
// would be invisible. B11 owns the log's persistence and its `ben status`
// rendering ([decided 2026-08-12]); recovery is a *reader* and must not come to
// depend on the file format, the tail length, or anything else that belongs to
// the writer.
//
// Three answers, not two, and the third is the point. `ok` false is "the log was
// read and does not carry one" — §9.10's blessed degraded path, and a legitimate
// verdict. A non-nil error is "we could not read the log", which is the *absence
// of a read* and must never collapse into the first: §9.10's rule is that the
// absence of a fact is never evidence, and a corrupt or unreadable state file
// reported as "no reason survived" would publish that sentence on a host where
// the reason was sitting right there. Recovery retries instead.
type FailureReasonReader interface {
	LastFailure(identifier string) (core.RunFailure, bool, error)
}

// LastFailure reports the §7.3 reason of the most recent transition into
// `failed` for an issue.
//
// This is the *in-memory tail*, which after a restart is empty by construction —
// so it answers false, and §9.10 step 6 then requires the comment to say the
// reason did not survive. The durable reader is the state dir's
// (internal/state), and the assembly wires that one; this implementation exists
// so the loop's own tests have a reader at all.
//
// The error is always nil here, because a slice cannot fail to be read. It is in
// the signature for the persistent reader, where a truncated or corrupt file is a
// real outcome — putting it there now is what stops that reader from being
// written against a two-valued contract it would have to squeeze an error into.
func (l *TransitionLog) LastFailure(identifier string) (core.RunFailure, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(l.entries) - 1; i >= 0; i-- {
		e := l.entries[i]
		if e.Issue == identifier && e.To == StateFailed && e.FailureReason != "" {
			return core.RunFailure{At: e.TS, Reason: e.FailureReason, Detail: e.Reason}, true, nil
		}
	}
	return core.RunFailure{}, false, nil
}

// TransitionSink is the durable half of the §9.11 log — B11's state dir. A nil
// sink leaves the log in-memory only, which is what the orchestrator's own
// tests run with and what §9.10 step 6 already describes the consequence of:
// the failure reason does not survive a restart.
type TransitionSink interface {
	// Append records one entry. An error means the entry was **not** recorded:
	// the caller retries, so a sink that can fail after committing part of a
	// record has to undo that part before returning (see
	// state.TransitionWriter.Append).
	//
	// A sink that can no longer accept anything at all reports ErrSinkUnwritable
	// rather than an ordinary error.
	Append(TransitionEntry) error
}

// TransitionLog is the in-memory tail of the append-only log, plus the durable
// append behind it. The loop needs somewhere to append and tests need somewhere
// to read; `ben status` and §9.10 step 6 need it to outlive the process.
type TransitionLog struct {
	mu      sync.Mutex
	entries []TransitionEntry

	// queue is the off-loop durable writer, shared with the attempt-outcome log
	// (durable.go). Set once, before Run, and never afterwards.
	queue durableQueue[TransitionEntry]
}

func (l *TransitionLog) attach(sink TransitionSink, log *slog.Logger) {
	l.queue.noun, l.queue.nouns = "transition", "transitions"
	l.queue.attrs = func(e TransitionEntry) []any {
		return []any{"issue", e.Issue, "from", e.From, "to", e.To}
	}
	if sink == nil {
		return
	}
	l.queue.attach(sink.Append, log)
}

func (l *TransitionLog) append(e TransitionEntry) {
	l.mu.Lock()
	l.entries = append(l.entries, e)
	l.mu.Unlock()
	l.queue.enqueue(e)
}

func (l *TransitionLog) persist(ctx context.Context) { l.queue.persist(ctx) }

func (l *TransitionLog) flush() { l.queue.flush() }

// Entries returns a copy of the log so far.
func (l *TransitionLog) Entries() []TransitionEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]TransitionEntry(nil), l.entries...)
}

// For returns the entries for one issue, in order.
func (l *TransitionLog) For(identifier string) []TransitionEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []TransitionEntry
	for _, e := range l.entries {
		if e.Issue == identifier {
			out = append(out, e)
		}
	}
	return out
}

// Path returns the states an issue passed through, for readable assertions.
func (l *TransitionLog) Path(identifier string) []State {
	var out []State
	for i, e := range l.For(identifier) {
		if i == 0 {
			out = append(out, e.From)
		}
		out = append(out, e.To)
	}
	return out
}
