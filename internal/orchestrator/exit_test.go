package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// Leaving (SPEC §9.8, §9.10). A record on its way out still owns a workspace, a
// claim, and whatever writes have not landed yet, so the exit is ordered and the
// record is what orders it: forgetting on the strength of one failed write drops
// the queue that was going to retry it, and there is then nothing left to notice
// the worktree leaked.
//
// The other half is what an exiting record must *not* do — a timer armed by the
// attempt being wound up may not start another run against a claim that may
// already be released. Deletion is gone_test.go; the retained `done` claim is
// held_test.go; drain is shutdown_test.go.

// A record with a Prepare or Start in flight may not be forgotten: the
// worker's result would land with nobody to own the workspace it created or
// the process it launched.
func TestExitWaitsForInFlightPrepare(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once

	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(*fake.Tracker) {},
		prepareGate: func() { once.Do(func() { <-release }) },
	})

	// The first Prepare is blocked. Close the issue and tick: reconciliation
	// wants to exit, but must not drop the record yet.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	if h.stateOf("1") == "" {
		close(release)
		t.Fatal("the record was forgotten while Prepare was still running; its workspace would leak")
	}

	// Let Prepare finish. The workspace it produces must be disposed of, not
	// abandoned.
	close(release)
	h.WaitGone("1")

	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the workspace the late Prepare produced to be cleaned up", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want 1", n)
	}
}

// A tracker write the queue refused must stay owed, not vanish. Every write
// a record owes is evidence §9.10 will read back, so "attempted once, logged
// on failure" is not good enough — the release in particular would strand the
// claim.
//
// Driven against a constructed-but-not-running orchestrator: New starts no
// goroutines, so the effect queue can be saturated and drained by hand.
func TestRefusedTrackerWriteStaysOwed(t *testing.T) {
	tracker := fake.NewTracker(fake.Issue("1", epoch))
	o, _ := idleWithSource(t, tracker)

	r := &Record{Issue: fake.Issue("1", epoch), State: StateFailed}
	o.records["1"] = r

	// Saturate: nothing is consuming the queue, so the next enqueue is
	// dropped exactly as it would be under a wedged tracker.
	for len(o.effects) < cap(o.effects) {
		o.effects <- func(context.Context) {}
	}

	o.release(t.Context(), r, "failed")
	if !r.owesAnything() {
		t.Fatal("the release was dropped rather than kept owed; the claim would be stranded")
	}
	if r.owedInFlight {
		t.Fatal("marked in flight on an effect the queue refused; no completion signal will ever clear it")
	}
	if _, ok := o.records["1"]; !ok {
		t.Fatal("the record was forgotten while it still owed the tracker a write")
	}

	// With room again, the retry re-queues it.
	<-o.effects
	o.retryPendingExits(t.Context())
	if !r.owedInFlight {
		t.Fatal("the retry did not re-queue the owed write once the queue had room")
	}
}

// SPEC §6.6 keeps the workspace for forensics and hands it back alongside the
// error. Dropping the reference would leak the directory: the record would
// have nothing to dispose when it later exits.
func TestWorkspaceReturnedWithAnErrorIsStillTracked(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		prepareErr:  errors.New("after_create hook failed"),
		prepRetry:   func(error) bool { return true },
		beforeStart: func(*fake.Tracker) {},
	})
	h.WaitState("1", StateBackoff)

	// The record knows about the workspace the failed Prepare left behind.
	var branch string
	for _, s := range h.o.Status() {
		if s.Identifier == "1" {
			branch = s.Branch
		}
	}
	if branch == "" {
		t.Fatal("the workspace returned alongside the error was discarded; nothing would ever dispose it")
	}

	// And it is disposed when the record exits.
	h.Tracker.Delete("1")
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(11 * time.Second)
	h.WaitGone("1")

	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the kept workspace disposed on exit", got)
	}
}

// A timer armed by an attempt that is being wound up must not start another
// run: the record is on its way out, and the claim may already be released.
// Driven through the continuation track, whose timer is armed while the
// record sits in verifying — a state reconciliation does revisit.
func TestStaleTimerCannotRestartAnExitingRecord(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Succeed("s") },
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictIncomplete}, nil
		}),
		beforeStart: func(tr *fake.Tracker) {
			// The release never confirms, so the record stays exiting.
			tr.SetFailRelease(errors.New("503 from the tracker"))
		},
	})
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the continuation timer was never armed")
	}

	// The issue closes: reconciliation begins the exit while that timer is
	// still pending.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.PollNow()
	waitFor(t, "the exit to begin", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// And then it reopens, so the re-fetch the timer does will find an issue
	// that looks perfectly dispatchable. Only the exiting guard stops it.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "open" })

	before := h.Runner.StartCount()
	h.Clock.Advance(2 * time.Second)
	time.Sleep(50 * time.Millisecond)

	if got := h.Runner.StartCount(); got != before {
		t.Errorf("started %d runs (was %d); a stale timer restarted a record whose release is still pending", got, before)
	}
}
