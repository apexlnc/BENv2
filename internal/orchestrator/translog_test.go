package orchestrator

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// recordingSink is a TransitionSink a test can read back and make fail.
type recordingSink struct {
	mu      sync.Mutex
	entries []TransitionEntry
	// accepted are the entries Append returned success for. Held apart from
	// entries because a sink that saw a record and refused it has not persisted
	// it, and a test asserting nothing was lost must count the second.
	accepted []TransitionEntry
	err      error
	block    chan struct{}
}

func (s *recordingSink) Append(e TransitionEntry) error {
	if s.block != nil {
		<-s.block
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, e)
	if s.err != nil {
		return s.err
	}
	s.accepted = append(s.accepted, e)
	return nil
}

func (s *recordingSink) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *recordingSink) got() []TransitionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TransitionEntry(nil), s.entries...)
}

func (s *recordingSink) wrote() []TransitionEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]TransitionEntry(nil), s.accepted...)
}

// lockedBuilder is a strings.Builder the daemon's goroutines write and the test
// reads. The old strings.Builder raced once the writer retried.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Every entry the in-memory tail holds reaches the sink, in the same order.
// Order is the whole content of "append-only": a log whose records arrived out
// of sequence describes a state machine that never ran.
func TestEveryTransitionReachesTheSinkInOrder(t *testing.T) {
	sink := &recordingSink{}
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("7", epoch)},
		verifier:    alwaysPublished,
		transitions: sink,
	})
	h.WaitState("7", StateDone)
	h.Stop()

	want := h.o.Transitions.Entries()
	if len(want) == 0 {
		t.Fatal("the run made no transitions")
	}
	got := sink.got()
	if len(got) != len(want) {
		t.Fatalf("sink got %d entries, the in-memory tail holds %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: sink has %+v, tail has %+v", i, got[i], want[i])
		}
	}
}

// A wedged sink must not stall the authority goroutine, and — the review
// finding — must not drop entries when the backlog grows. §9.11 says *every*
// transition appends, and a bounded queue answers a wedged disk by deciding
// which transitions did not happen, choosing exactly the ones written during
// the incident whose log somebody will later read. There is no in-memory
// consolation: `ben status` reads the file.
func TestAWedgedSinkNeverBlocksTheLoopAndNeverDrops(t *testing.T) {
	var out lockedBuilder
	sink := &recordingSink{block: make(chan struct{})}
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("7", epoch)},
		verifier:    alwaysPublished,
		transitions: sink,
		logTo:       &out,
	})
	// The loop reached its terminal state with the sink's first Append still
	// blocked: nothing about persistence gates a transition.
	h.WaitState("7", StateDone)
	if got := h.o.Transitions.Path("7"); len(got) < 2 {
		t.Fatalf("the loop made no progress while the sink was blocked: %v", got)
	}

	// Far past what any bounded queue would have held.
	extra := backlogWarn * 4
	for i := range extra {
		h.o.Transitions.append(TransitionEntry{
			Issue: "7", From: StateBackoff, To: StateBackoff, Reason: strconv.Itoa(i),
		})
	}
	if !strings.Contains(out.String(), "accumulating in memory") {
		t.Error("a growing backlog said nothing")
	}

	close(sink.block)
	h.Stop()

	// Every entry the in-memory tail holds reached the sink, backlog included.
	if got, want := len(sink.got()), len(h.o.Transitions.Entries()); got != want {
		t.Errorf("sink got %d entries, the tail holds %d — %d were dropped", got, want, want-got)
	}
}

// A sink that fails is retried, not written off. The previous version logged
// once and moved on, which loses the record permanently: there is no later
// moment at which it can be reconstructed, because the tracker projection does
// not carry the §7.3 reason (§9.10 step 6).
func TestASinkErrorIsRetriedNotDropped(t *testing.T) {
	var out lockedBuilder
	sink := &recordingSink{err: errors.New("disk full")}
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("7", epoch)},
		verifier:    alwaysPublished,
		transitions: sink,
		logTo:       &out,
	})
	h.WaitState("7", StateDone)

	if !strings.Contains(out.String(), "could not persist a transition") {
		t.Errorf("a failing sink was silent; log was:\n%s", out.String())
	}
	if got := h.o.Transitions.Path("7"); len(got) < 2 {
		t.Errorf("the loop stopped making transitions when the sink failed: %v", got)
	}

	// The disk comes back. Nothing was discarded while it was gone.
	sink.setErr(nil)
	h.Stop()
	if got, want := len(sink.wrote()), len(h.o.Transitions.Entries()); got != want {
		t.Errorf("after the sink recovered it holds %d entries, the tail holds %d", got, want)
	}
}

// And if it never comes back, shutdown still ends — and says what it could not
// write, by count. A daemon that will not exit because a disk is full is worse
// than one that exits saying so.
func TestShutdownGivesUpLoudlyRatherThanHanging(t *testing.T) {
	var out lockedBuilder
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("7", epoch)},
		verifier:    alwaysPublished,
		transitions: &recordingSink{err: errors.New("disk full")},
		logTo:       &out,
	})
	// Shortened so the test does not spend the production budget proving the
	// production budget expires. Set before Stop, which is what runs the flush.
	h.o.Transitions.queue.flushBudget = 50 * time.Millisecond
	h.WaitState("7", StateDone)
	h.Stop() // Stop fails the test if Run does not return

	if !strings.Contains(out.String(), "gave up persisting transitions") {
		t.Errorf("shutdown discarded entries without naming them; log was:\n%s", out.String())
	}
}

// The §7.6 child environment is a closed set. Asserted as the whole map rather
// than key by key: a test that checks the keys it knows about cannot see one
// added, and every key here is a name the agent's own tooling may depend on.
func TestTheChildEnvironmentIsExactlyTheRunsCoordinates(t *testing.T) {
	h := start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("7", epoch)},
		verifier: alwaysPublished,
	})
	h.WaitState("7", StateDone)

	spec, ok := h.Runner.LastSpec()
	if !ok {
		t.Fatal("no run was launched")
	}
	want := map[string]bool{
		"BEN_ISSUE": true, "BEN_ATTEMPT": true, "BEN_RUN_ID": true,
		"BEN_WORKSPACE": true, "BEN_BRANCH": true,
	}
	for k, v := range spec.Env {
		if !want[k] {
			t.Errorf("unexpected child variable %s=%q; §7.6 reserves BEN_ to the orchestrator, so adding one is a contract change", k, v)
		}
		if v == "" {
			t.Errorf("%s is empty, so the agent cannot use it", k)
		}
		delete(want, k)
	}
	for k := range want {
		t.Errorf("%s is missing from the child environment", k)
	}
}

// The run id names one attempt and changes with it, which is what makes it
// usable as a correlation handle at all.
func TestRunIDIsStableWithinAnAttemptAndChangesBetweenThem(t *testing.T) {
	r := &Record{Issue: core.Issue{Identifier: "acme/widgets#7"}, instance: "abc", token: 3}
	first := r.runID()
	if first != r.runID() {
		t.Error("runID is not stable within one attempt")
	}
	if strings.ContainsAny(first, "/#") {
		t.Errorf("runID %q carries characters that surprise a shell or a filename", first)
	}
	r.generation++
	if second := r.runID(); second == first {
		t.Errorf("runID %q is unchanged across attempts, so two attempts share one identity", second)
	}
	// A different record for the same issue is a different run, even at the same
	// generation — that is what token is for.
	if (&Record{Issue: r.Issue, instance: "abc", token: 4}).runID() ==
		(&Record{Issue: r.Issue, instance: "abc", token: 3}).runID() {
		t.Error("two records for one issue share a runID")
	}
}

// Review finding: token restarts at 1 on every daemon start, so without a
// daemon-instance component two different attempts either side of a restart both
// read as `7-1.0` — in a log that outlives the process and is the whole reason
// the handle is written down.
func TestRunIDsDoNotCollideAcrossDaemonRestarts(t *testing.T) {
	issue := core.Issue{Identifier: "7"}
	// Two daemons, each on its first record's first attempt: identical token and
	// generation by construction, which is exactly the restart case.
	first := &Record{Issue: issue, instance: "inst-a", token: 1}
	second := &Record{Issue: issue, instance: "inst-b", token: 1}
	if first.runID() == second.runID() {
		t.Errorf("both daemons named their first attempt %q; a run id written to a durable log must not repeat across restarts", first.runID())
	}
}

// And the instance is minted per run of the daemon, so two orchestrators built
// from the same configuration do not share one.
func TestEachDaemonGetsItsOwnInstance(t *testing.T) {
	h := start(t, harnessOpts{issues: []core.Issue{fake.Issue("7", epoch)}})
	if h.o.Instance() == "" {
		t.Fatal("the orchestrator has no instance")
	}
	// The harness states it so run ids are assertable; production derives it
	// from the start instant, which is what makes two runs differ.
	cfg := Config{Runtime: h.Source, Clock: h.Clock, DaemonID: "d"}
	a, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	h.Clock.Advance(time.Second)
	b, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if a.Instance() == b.Instance() {
		t.Errorf("two daemon runs share instance %q", a.Instance())
	}
}

// Review finding: enterBackoff bumps generation — which changes the run id — and
// armTimer publishes immediately after. Clearing the session only before the
// *preparing* edge left the whole backoff wait publishing the new attempt's run
// id beside the previous attempt's session.
func TestBackoffNeverPublishesANewRunIDWithAnOldSession(t *testing.T) {
	h := start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("7", epoch)},
		verifier: alwaysPublished,
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return []core.Event{
					{Type: core.EventStarted, SessionID: "previous-session", Continuation: "previous-session"},
					{Type: core.EventFailed, Reason: core.FailureCrashed},
				}
			}
			return []core.Event{
				{Type: core.EventStarted, SessionID: "next-session", Continuation: "next-session"},
				{Type: core.EventSucceeded},
			}
		},
	})
	h.WaitState("7", StateBackoff)

	got, ok := h.o.StatusFor("7")
	if !ok {
		t.Fatal("no published snapshot for the backing-off record")
	}
	if got.SessionID != "" {
		t.Errorf("run %q is published with session %q while in backoff; the session belongs to the attempt that announced it",
			got.RunID, got.SessionID)
	}
	if got.NextTimerAt.IsZero() {
		t.Error("no backoff timer is published, so this test is not observing the window it is about")
	}
}

// Review finding, with the reviewer's own mutation: the budget bounded the
// sleeps *between* drains, and a drain of slow-but-succeeding writes ran to
// completion however long it took. That is the likelier shape, too — a disk that
// is merely slow keeps accepting.
func TestTheShutdownBudgetBoundsTheDrainItself(t *testing.T) {
	const (
		perWrite = 30 * time.Millisecond
		budget   = 20 * time.Millisecond
		queued   = 5
	)
	l := &TransitionLog{}
	l.attach(sinkFunc(func(TransitionEntry) error {
		time.Sleep(perWrite)
		return nil
	}), discardLog())
	l.queue.flushBudget = budget
	for i := range queued {
		l.append(TransitionEntry{Issue: strconv.Itoa(i), From: StateBackoff, To: StateBackoff})
	}

	start := time.Now()
	l.flush()
	elapsed := time.Since(start)

	// One write may already be in flight when the deadline passes: an Append
	// blocked inside the kernel is not interruptible from here, and abandoning it
	// would put a second writer on a file the first is still in.
	if max := budget + 2*perWrite; elapsed > max {
		t.Errorf("flush took %v against a %v budget; it drained %d slow writes instead of stopping", elapsed, budget, queued)
	}
}

// A sink that says its failure is permanent is not retried: every attempt would
// append past a fragment nothing can remove, so retrying is a spin. The entry is
// dropped, and each drop is named — the honest report of a loss this process
// cannot undo.
func TestAnUnwritableSinkDropsLoudlyRatherThanSpinning(t *testing.T) {
	var out lockedBuilder
	var calls atomic.Int64
	l := &TransitionLog{}
	l.attach(sinkFunc(func(TransitionEntry) error {
		calls.Add(1)
		return fmt.Errorf("%w: the tail could not be rolled back", ErrSinkUnwritable)
	}), slog.New(slog.NewTextHandler(&out, nil)))
	l.queue.flushBudget = time.Second

	for i := range 3 {
		l.append(TransitionEntry{Issue: strconv.Itoa(i), From: StateBackoff, To: StateBackoff})
	}
	start := time.Now()
	l.flush()

	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("flush took %v; an unwritable sink was retried rather than reported", elapsed)
	}
	if got := calls.Load(); got != 3 {
		t.Errorf("the sink was called %d times for 3 entries; a permanent failure must not be retried", got)
	}
	if n := strings.Count(out.String(), "dropping a transition"); n != 3 {
		t.Errorf("%d drops reported for 3 dropped entries:\n%s", n, out.String())
	}
}

// A continuing failure logged on every retry — ten lines a second at
// retryDelay — buries the backlog warning beside it and turns one
// incident into a journal nobody can read. The onset always prints; the
// repetitions are rated.
func TestAContinuingFailureIsLoggedOnceNotOnEveryRetry(t *testing.T) {
	var out lockedBuilder
	l := &TransitionLog{}
	l.attach(sinkFunc(func(TransitionEntry) error { return errors.New("disk full") }),
		slog.New(slog.NewTextHandler(&out, nil)))
	l.queue.flushBudget = 20 * retryDelay

	l.append(TransitionEntry{Issue: "7", From: StateBackoff, To: StateBackoff})
	l.flush()

	got := strings.Count(out.String(), "could not persist a transition")
	if got == 0 {
		t.Fatal("the onset of a failure was not reported at all")
	}
	if got > 2 {
		t.Errorf("%d retry lines for one continuing failure; the rate limit is not holding:\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), "gave up persisting transitions") {
		t.Error("the give-up line is missing")
	}
}

// sinkFunc is a TransitionSink from a function, for the tests above that drive
// the log directly rather than through a whole orchestrator.
type sinkFunc func(TransitionEntry) error

func (f sinkFunc) Append(e TransitionEntry) error { return f(e) }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
