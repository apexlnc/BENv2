package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// The off-loop writer behind every append-only state-dir log the loop produces:
// the §9.11 transition log (translog.go) and the attempt-outcome log
// (attempt.go).
//
// One implementation, because the two need the same four properties and each of
// them is only worth having if it always holds. Records are queued rather than
// written inline, because the sink fsyncs and appends happen on the authority
// goroutine — the one goroutine in BEN that must never wait on a disk. They
// drain serially, because a log whose records arrived out of order is not an
// append-only log. A failed record stays at the head, because one that was
// skipped and carried past leaves a hole. And the queue is unbounded, because
// the alternative is deciding which records did not happen during exactly the
// incident whose log somebody will later read.

// ErrSinkUnwritable is a sink saying its failure is permanent — retrying will
// not help and would only spin. It is a distinct sentinel because the two
// answers lead somewhere different: an ordinary failure keeps the entry pending,
// and this one drops it, loudly, because nothing else is available.
//
// Stated here rather than imported from the state dir. The loop must not depend
// on a concrete file format — that is the whole reason the entry types and the
// on-disk records are separate — so the assembly translates this the same way it
// translates the entries (cmd/ben/statedir.go).
var ErrSinkUnwritable = errors.New("orchestrator: the sink can no longer accept entries")

// retryDelay is how long the durable writer waits before trying a failed append
// again.
//
// Real time rather than the injected Clock, deliberately. That clock exists so
// §9 *decisions* — backoff, timeouts, poll cadence — are stated rather than
// waited out, and is manual in tests. Retrying a disk write is not one of those:
// nothing in the state machine turns on it, and putting it on the manual clock
// would mean a test had to advance time to let a file be written.
const retryDelay = 100 * time.Millisecond

// backlogWarn is the pending depth at which the writer starts saying so. Not a
// limit — see durableQueue.pending.
const backlogWarn = 256

// failureLogInterval bounds how often a *continuing* failure is logged. The
// onset is always reported; this governs the repetitions.
const failureLogInterval = 30 * time.Second

// defaultFlushBudget bounds the shutdown drain: long enough to ride out a brief
// stall, short enough that a dead disk cannot hold the process open against a
// supervisor's TimeoutStopSec.
const defaultFlushBudget = 2 * time.Second

// durableQueue is the serial, retrying, order-preserving writer behind one log.
//
// nouns is how it names what it carries in an operator's log line: singular for
// one record, plural for a count of them. They are fields rather than a type
// parameter's name because these lines are read by a human at 3am, and "could
// not persist a T" is not a sentence.
type durableQueue[T any] struct {
	noun  string
	nouns string
	sink  func(T) error
	log   *slog.Logger
	// attrs are the per-record slog attributes identifying which record failed.
	attrs func(T) []any
	// flushBudget overrides defaultFlushBudget. Set only by tests, which would
	// otherwise spend the real budget proving that the real budget expires.
	flushBudget time.Duration

	// pending is the durable queue, and it is **unbounded on purpose**.
	//
	// §9.11 says every transition appends. A bounded queue answers a slow or
	// wedged disk by deciding which records did not happen — and the ones it
	// discards are those written during exactly the incident whose log somebody
	// will later read. Dropping is also unrecoverable in a way a delay is not:
	// there is no later moment at which the record can be reconstructed, because
	// the loop has moved on and the tracker projection does not carry the §7.3
	// reason (§9.10 step 6).
	//
	// So the bound is memory, and the backpressure is a log line. An entry is
	// ~200 bytes and transitions are minutes apart per issue, so a disk wedged
	// for a day costs kilobytes; a disk wedged long enough for this to matter has
	// already stopped the worktrees and the transcripts.
	mu      sync.Mutex
	pending []T
	// failingSince and lastReported rate-limit the retry log. See reportFailure.
	failingSince time.Time
	lastReported time.Time
	// wake is a capacity-1 signal rather than a queue: it says "there is work",
	// and the work itself lives in pending where it cannot be lost to a full
	// channel.
	wake     chan struct{}
	warnedAt int
}

// attach binds a sink. A nil sink leaves the queue inert: nothing is enqueued,
// and every method below is a no-op.
func (q *durableQueue[T]) attach(sink func(T) error, log *slog.Logger) {
	if sink == nil {
		return
	}
	q.sink = sink
	q.log = log
	q.wake = make(chan struct{}, 1)
}

func (q *durableQueue[T]) enqueue(e T) {
	if q.sink == nil {
		return
	}
	q.mu.Lock()
	q.pending = append(q.pending, e)
	depth, warn := len(q.pending), false
	// Warn once per doubling rather than per entry: a wedged disk would
	// otherwise turn one incident into thousands of identical lines.
	if depth >= backlogWarn && depth >= q.warnedAt*2 {
		q.warnedAt, warn = depth, true
	}
	q.mu.Unlock()

	if warn {
		q.log.Error("the "+q.noun+" log is not being written; entries are accumulating in memory",
			append([]any{"pending", depth}, q.attrs(e)...)...)
	}
	select {
	case q.wake <- struct{}{}:
	default: // a wake is already outstanding; the drain will see this entry
	}
}

// persist drains pending entries until ctx is cancelled, retrying what fails.
// flush is what makes the tail complete; this is only the steady state.
func (q *durableQueue[T]) persist(ctx context.Context) {
	if q.sink == nil {
		return
	}
	for {
		if !q.drain(ctx) {
			// Something is still pending and the sink refused it. Wait before
			// trying again rather than spinning on a full disk.
			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-q.wake:
		}
	}
}

// drain writes what is pending, in order, and reports whether it got to the end.
// A failed entry stays at the head: an append-only log that skipped a record it
// could not write and carried on with the next one is no longer append-only.
//
// ctx bounds it between entries, which is all it can bound. A single append that
// blocks inside the kernel is not interruptible from here, and abandoning one to
// carry on would put a second writer on a file the first is still in — so that
// case is bounded where every other unbounded wait in BEN is, by the
// supervisor's TimeoutStopSec (SPEC §9.8, deploy/ben.service).
func (q *durableQueue[T]) drain(ctx context.Context) bool {
	for {
		if ctx.Err() != nil {
			return false
		}
		q.mu.Lock()
		if len(q.pending) == 0 {
			q.warnedAt, q.failingSince = 0, time.Time{}
			q.mu.Unlock()
			return true
		}
		e := q.pending[0]
		q.mu.Unlock()

		err := q.sink(e)
		if errors.Is(err, ErrSinkUnwritable) {
			// Not retryable, by that sentinel's definition: every later attempt
			// would append past a fragment nothing can remove, so retrying is a
			// spin. The entry is dropped — once, and named, which is the honest
			// report of a loss this process cannot undo.
			q.log.Error("dropping a "+q.noun+": the log can no longer be appended to",
				append(q.attrs(e), "error", err)...)
			q.mu.Lock()
			q.pending = q.pending[1:]
			q.mu.Unlock()
			continue
		}
		if err != nil {
			q.reportFailure(e, err)
			return false
		}
		q.mu.Lock()
		q.pending = q.pending[1:]
		q.failingSince = time.Time{}
		q.mu.Unlock()
	}
}

// reportFailure logs a retryable append failure at most once per
// failureLogInterval.
//
// Unrated, this logged on every retry: at one attempt per retryDelay that is ten
// identical lines a second, which buries the once-per-doubling backlog warning
// beside it and turns one incident into a journal nobody can read. The first
// failure after a success always prints, so the onset is never the line that
// gets suppressed.
func (q *durableQueue[T]) reportFailure(e T, cause error) {
	now := time.Now()
	q.mu.Lock()
	say := q.failingSince.IsZero() || now.Sub(q.lastReported) >= failureLogInterval
	if q.failingSince.IsZero() {
		q.failingSince = now
	}
	since, depth := q.failingSince, len(q.pending)
	if say {
		q.lastReported = now
	}
	q.mu.Unlock()

	if !say {
		return
	}
	q.log.Error("could not persist a "+q.noun+"; will retry",
		append(q.attrs(e),
			"failing_for", now.Sub(since).Round(time.Second), "pending", depth, "error", cause)...)
}

// flush makes a last attempt at whatever is still pending, after the errgroup
// has returned — every append is provably over by then, so this cannot race one
// more arriving, and the records of a shutdown are the ones an operator asking
// why the daemon stopped will look for.
//
// It retries, but not forever: a daemon that will not exit because a disk is
// full is worse than one that exits saying what it could not write. That budget
// is the one place entries can be lost, and it says so, by count, at Error.
func (q *durableQueue[T]) flush() {
	if q.sink == nil {
		return
	}
	budget := q.flushBudget
	if budget <= 0 {
		budget = defaultFlushBudget
	}
	// A deadline the drain itself observes, rather than a sleep between whole
	// drains. A backlog of slow-but-succeeding writes would otherwise run to
	// completion however long it took — the shape a budget is supposed to
	// exclude, and the likelier one, since a disk that is merely slow keeps
	// accepting.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	for !q.drain(ctx) {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			lost := len(q.pending)
			q.mu.Unlock()
			if lost > 0 {
				// Named rather than counted silently: §9.10 step 6 will report
				// that the reason did not survive the restart, and this is the
				// line that says the reason is missing because we could not write
				// it rather than because this is a fresh host.
				q.log.Error("gave up persisting "+q.nouns+" at shutdown; they did not reach the state dir",
					"lost", lost, "after", budget)
			}
			return
		case <-time.After(retryDelay):
		}
	}
}
