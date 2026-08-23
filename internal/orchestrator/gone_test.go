package orchestrator

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// An issue that has left the tracker (SPEC §9.8): core.ErrIssueNotFound, not a
// failed read. The distinction is the file — a deletion mistaken for a transient
// failure leaves the record running forever, and a transient failure mistaken
// for a deletion tears down a live run — so the last test here is the negative
// case that fixes the boundary.
//
// `gone` is an *ordered* exit rather than an immediate one. The tracker writes
// the record owes are discarded as they reach the head (there is nothing left to
// write to), the local work beside them still runs, and the record outlives both
// because it is the thing retrying them. Taken over by a human is claim_test.go;
// closed is policy_test.go and parked_test.go.
//
// The second half of the file is how the fact is *learned* when no read is
// looking (#142, absence.go). A write cannot say it — its 404 also means a
// missing sub-resource (#134) — and the record whose write is failing is often
// one nothing refreshes: `queued`, `done`, or any record already on an ordered
// exit. So a failing tracker write buys one confirming `Get`, on a budget, and
// only that read may conclude anything.

// SPEC §9.8: the adapter states absence with core.ErrIssueNotFound. Treating
// it as a failed read would leave a deleted issue running forever.
func TestDeletedIssueIsNotTreatedAsATransientFailure(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.Delete("1")
	h.Tick()
	h.WaitGone("1")

	// And released *nothing*. This used to assert one release, which encoded the
	// old behaviour rather than the requirement: the adapter answers 404 for an
	// issue the tracker no longer has, an owed write that errors is retried every
	// tick, and a record whose release never lands is never forgotten — so the
	// ask that looked like tidiness was what kept a deleted issue's slot for the
	// life of the process. The claim died with the issue; there is nothing to
	// drop.
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
	// The workspace still goes: forgetting must not drop the effects queued ahead
	// of it (finishNow owes the disposal first).
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %v, want the deleted issue's workspace disposed", got)
	}
}

// The forget is *owed*, not immediate, and this is the difference.
//
// A disposal that fails is retried from the head of the owed queue until it
// lands (owed.go), and the record has to outlive it to be the thing retrying.
// Forgetting the moment the issue is found gone drops the queue: the in-flight
// disposal still runs, so a passing one hides the bug entirely, and only a
// *failing* one shows that nothing is left to retry it. The worktree would leak
// silently until §9.10's next startup sweep.
func TestAGoneIssueStillRetriesItsDisposal(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)
	// Persistently, not once: the assertion is that the record keeps retrying,
	// and an exact disposal count would be measuring the tick cadence instead.
	h.Workspaces.SetFailDispose(errors.New("worktree busy"), false)

	h.Tracker.Delete("1")
	h.Tick()

	// Retained across the failure — it is the thing that owes the retry.
	tried := len(h.Workspaces.Disposals("1"))
	if tried == 0 {
		t.Fatal("the deleted issue's workspace was never disposed")
	}
	if got := h.stateOf("1"); got == "" {
		t.Fatal("the record was forgotten while its disposal was still owed; nothing is left to retry it")
	}

	h.Tick()
	if got := len(h.Workspaces.Disposals("1")); got <= tried {
		t.Errorf("disposals = %d, want the failed one retried on the next tick (was %d)", got, tried)
	}
	if got := h.stateOf("1"); got == "" {
		t.Fatal("the record left while its workspace was still there")
	}

	// And it leaves once the workspace actually goes.
	h.Workspaces.SetFailDispose(nil, false)
	h.Tick()
	h.WaitGone("1")
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
}

// Tracker writes still owed when the issue vanishes are discarded one by one as
// they reach the head, and the local work beside them still runs.
//
// The distinction is the whole of the rule, and a mistake in either direction is
// silent: keeping a tracker write blocks everything behind it forever, and
// dropping a local one leaks a worktree and skips the §6.5 hook with no failed
// effect to say so.
//
// The fixture leaves a `ben:running` projection permanently failing, so the run
// finishes and parks behind it — at the moment the deletion is noticed the record
// owes that write, the park's own label and comment, the after-run hook, and then
// its cleanup.
func TestAGoneIssueDiscardsTrackerWritesAndKeepsLocalWork(t *testing.T) {
	h := start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("1", epoch)},
		withHook: true,
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no evidence"}, nil
		}),
		beforeStart: func(tr *fake.Tracker) {
			// ben:claimed still lands — the record cannot reach `running` without
			// it — and ben:running never does, so it stays at the head while
			// everything the run owes afterwards queues behind it.
			tr.FailLabel = func(_ string, label core.StateLabel) error {
				if label == core.StateLabelRunning {
					return errors.New("502 from the tracker")
				}
				return nil
			}
		},
	})
	h.WaitState("1", StateNeedsReview)

	h.Tracker.Delete("1")
	h.Tick()
	h.WaitGone("1")

	// The local half ran.
	if n := h.Hooked.AfterRunCount("1"); n != 1 {
		t.Errorf("after_run hook fired %d time(s), want 1: it was queued behind the writes that had to be discarded", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %v, want the deleted issue's workspace disposed", got)
	}
	// And the tracker half left nothing standing to retry.
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
}

// One cleanup sequence, however many ticks pass — the other half of `gone` being
// an ordered exit.
//
// Asserted on the owed queue because nothing else can see it. A record that
// reconciliation keeps re-deciding appends a fresh disposal and forget every
// tick, and *none of the extras ever run*: the forget at the front drops the
// record and the rest die with it. So the disposal count is one per tick either
// way, and the only visible symptom is a queue that grows without bound on a
// record that can sit for hours behind a failing disposal.
func TestAGoneIssueOwesOneCleanupSequence(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)
	// Held failing, so the record stays put and every tick gets its chance to
	// queue another round.
	h.Workspaces.SetFailDispose(errors.New("worktree busy"), false)

	h.Tracker.Delete("1")
	for range 4 {
		h.Tick()
	}

	got := h.owedAfterStop("1")
	want := []string{"dispose workspace", effectForget}
	if !slices.Equal(got, want) {
		t.Errorf("owed = %v, want exactly %v: reconciliation re-decided a record it had already resolved", got, want)
	}
}

// The deletion race a claim can lose, end to end: `Fetch` answers from a listing
// the tracker may already have moved past, so the issue can go between the
// candidate read and the assignment request.
//
// What follows is all correct and still ends in a wedge. The claim's 404 comes
// from the assignment request itself, so the adapter cannot promise nothing
// landed and returns a joined error with no core.ErrClaimNotAttempted (#140);
// onClaimed therefore owes a Release rather than forgetting, because
// assigned-with-no-state-label is what §9.10 step 3 never revisits. That release
// 404s every tick — and nothing ever classifies it, because a write may not
// (#134) and §9.8 refreshes neither `queued` records nor ordered exits. Measured
// before the fix: `state="queued" releaseAttempts=6` after five further ticks,
// one per tick, unbounded, holding the §9.5 slot for the life of the process.
func TestAClaimThatLostTheDeletionRaceDoesNotHoldItsSlot(t *testing.T) {
	claiming := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch), fake.Issue("2", epoch.Add(time.Minute))},
		concurrency: "1",
		// Held inside the claim *write*, so the deletion lands between the candidate
		// read and the assignment — the only way this fixture is physically honest.
		// Deleting before dispatch would model a Fetch that returned an issue
		// already gone, which is a different race and a weaker one.
		beforeStart: func(tr *fake.Tracker) {
			tr.SetClaimGate(func() {
				once.Do(func() { close(claiming) })
				<-release
			})
		},
	})
	select {
	case <-claiming:
	case <-time.After(2 * time.Second):
		t.Fatal("the claim write was never attempted")
	}

	h.Tracker.Delete("1")
	// Closed rather than sent to: it releases this claim and every later one, so
	// issue 2's claim below is not held by the same gate.
	close(release)

	// The unwinding release goes out and fails, which is the state the wedge
	// starts from. Acknowledged rather than timed, so the tick below is provably
	// the one that has something to confirm.
	waitFor(t, "the unwinding release to fail", func() bool {
		return h.Tracker.ReleaseAttempts("1") > 0 && h.applied(sigEffectDone) > 0
	})
	if h.stateOf("1") == "" {
		t.Fatal("the record was forgotten while its release was still owed; nothing would be left to retry it")
	}

	h.Tick()
	h.WaitGone("1")

	// The retry stopped, rather than the record merely having been dropped: the
	// count is read after the record is gone and must not move again.
	attempts := h.Tracker.ReleaseAttempts("1")
	h.Tick()
	h.Tick()
	if got := h.Tracker.ReleaseAttempts("1"); got != attempts {
		t.Errorf("release attempts = %d after two further ticks, want it to stop at %d", got, attempts)
	}
	// From the Get, and only the Get (#134): the write's own refusal never said
	// this, and asking is what the ticket buys.
	if n := h.Tracker.GetReadsFor("1"); n == 0 {
		t.Error("nothing ever read the issue back; the absence was concluded from a write's refusal")
	}

	// The slot is free, and the proof is the next issue taking it. A bare "issue 1
	// is untracked" would pass for a record that was never created.
	h.Tick()
	h.WaitState("2", StateDone)
}

// The negative that fixes the boundary, and the one #134 is about: a write's
// refusal is never evidence of absence, however much it looks like one.
//
// Both rows fail the release with a 404 — the shape a deleted issue produces —
// and differ only in what the confirming read says. Neither may drop the record:
// the claim is real, and forgetting it strands the assignment on an issue that is
// still there.
func TestAFailingWriteNeverClassifiesTheIssueItself(t *testing.T) {
	tests := []struct {
		name string
		// failGet is what the confirming read answers with; nil is the issue
		// present, which is the fact the write's 404 disagrees with.
		failGet error
		wantGet bool
	}{
		{name: "the issue is still there", wantGet: true},
		{name: "the confirming read fails too", failGet: errors.New("502 from the tracker"), wantGet: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				issues: []core.Issue{fake.Issue("1", epoch)},
				script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureAuth) },
				beforeStart: func(tr *fake.Tracker) {
					// Unclassified, as the adapter leaves it: a write's 404 is
					// reported verbatim precisely because it cannot be read.
					tr.SetFailRelease(errors.New("404 Not Found"))
					tr.SetFailGet(tt.failGet)
				},
			})
			h.WaitState("1", StateFailed)
			waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

			for range 3 {
				h.Tick()
			}
			if got := h.stateOf("1"); got != StateFailed {
				t.Fatalf("state = %q; the record was dropped on a write's refusal", got)
			}
			if got := h.Tracker.GetReadsFor("1") > 0; got != tt.wantGet {
				t.Errorf("confirming Get spent = %v, want %v", got, tt.wantGet)
			}
			if got := h.Workspaces.Disposals("1"); len(got) != 0 {
				t.Errorf("disposals = %v; the exit ran on an issue nobody established is gone", got)
			}

			// And it leaves the ordinary way once the write can land, which is what
			// says the claim was still there to drop.
			h.Tracker.SetFailRelease(nil)
			h.Tick()
			h.WaitGone("1")
			if n := h.Tracker.ReleaseCount("1"); n != 1 {
				t.Errorf("succeeded releases = %d, want 1", n)
			}
		})
	}
}

// The §8.5 bound, stated as a test: **one** confirming Get per tick for the whole
// record set, and a rotation that gives every candidate a turn.
//
// The number matters because the thing it accompanies is already O(records): a
// tracker refusing writes refuses them for everyone at once, so a confirmation
// per failure would double the per-tick cost of exactly the outage that produced
// it. K records resolve over K ticks instead, which costs nothing — a wedged
// write is not urgent.
func TestAbsenceConfirmationsAreOnePerTickAndRotate(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{
			fake.Issue("1", epoch),
			fake.Issue("2", epoch.Add(time.Minute)),
			fake.Issue("3", epoch.Add(2*time.Minute)),
		},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureAuth) },
		beforeStart: func(tr *fake.Tracker) {
			tr.SetFailRelease(errors.New("404 Not Found"))
		},
	})
	for _, id := range []string{"1", "2", "3"} {
		h.WaitState(id, StateFailed)
	}
	waitFor(t, "every record's release to have failed once", func() bool {
		return h.Tracker.ReleaseAttempts("1") > 0 &&
			h.Tracker.ReleaseAttempts("2") > 0 && h.Tracker.ReleaseAttempts("3") > 0
	})
	// Nothing else reads an issue here: `failed` is outside §9.8's refresh set and
	// the sweep reads a list, so every Get below is a confirmation.
	if n := h.Tracker.GetReads(); n != 0 {
		t.Fatalf("Get reads before the first tick = %d, want 0", n)
	}

	for i := 1; i <= 3; i++ {
		h.Tick()
		waitFor(t, "the tick's confirmation to be handled", func() bool {
			return h.applied(sigOwedConfirmed) >= uint64(i)
		})
		if got := h.Tracker.GetReads(); got != i {
			t.Fatalf("Get reads after %d tick(s) = %d, want exactly one per tick", i, got)
		}
	}
	// Every candidate got its turn, rather than one record retaking the slot.
	for _, id := range []string{"1", "2", "3"} {
		if n := h.Tracker.GetReadsFor(id); n != 1 {
			t.Errorf("issue %s was confirmed %d time(s) in three ticks, want exactly 1: the rotation starved a record", id, n)
		}
	}
}

// The same shape one state later, which is why the confirmation belongs to the
// owed queue and not to the claim path: a `done` record owes a cleared state
// label and a publish comment, and nothing refreshes it either — §9.8 lists
// running-ish states and sweeps parked ones, and `done` is in neither.
//
// The fixture leaves the cleared label permanently failing, so the publish
// comment, the disposal and the conversion all queue behind it. Deleting the
// issue there used to leave the record retrying that write for the life of the
// process, with the workspace still on disk behind it.
func TestAGoneIssueDoesNotStrandAFinishedRun(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.FailLabel = func(_ string, label core.StateLabel) error {
				if label == core.StateLabelNone {
					return errors.New("502 from the tracker")
				}
				return nil
			}
		},
	})
	h.WaitState("1", StateDone)

	h.Tracker.Delete("1")
	h.tickUntil("the finished run to be dropped", func() bool { return h.stateOf("1") == "" })

	// The local half still ran, and exactly once: `done` had already queued the
	// disposal, and the exit the confirmation orders must not queue a second one —
	// a Dispose of a directory the first call removed fails, and a failing local
	// effect is retried from the head forever.
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want exactly one disposal of the deleted issue's workspace", got)
	}
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
	// And it did not become a held claim on the way out: there is no issue left
	// for a PR to be reviewed against, and the sweep would ask about it every tick.
	if n := h.o.HeldCount(); n != 0 {
		t.Errorf("held claims = %d, want the deleted issue not retained", n)
	}
}

// The conversion's own read, which classifies itself and so needs no
// confirmation: ClaimHistory names one issue, so its not-found means the issue
// (#134, and the fake says so too).
//
// Reached when the deletion lands after `done` has written everything it owes —
// the one window where the record owes nothing, so the failing-write path above
// never fires. Retrying it was the wedge: readyToConvert has already established
// the record owes nothing, so nothing else would ever look at it again.
func TestAFinishedRunWhoseIssueVanishesBeforeConversionIsDropped(t *testing.T) {
	var reads atomic.Int64
	release := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		// The second history read is §9.7's mandatory current-epoch check; the
		// first is §9.5's approval check at dispatch. Deletion at either named read
		// is positive not-found evidence, never a reason to park a vanished issue.
		beforeStart: func(tr *fake.Tracker) {
			tr.SetHistoryGate(func() {
				if reads.Add(1) == 2 {
					<-release
				}
			})
		},
	})
	waitFor(t, "the verification claim-history read to be out", func() bool {
		return h.Tracker.HistoryReads() >= 2
	})

	h.Tracker.Delete("1")
	close(release)

	h.WaitGone("1")
	if n := h.o.HeldCount(); n != 0 {
		t.Errorf("held claims = %d, want nothing retained for an issue that is gone", n)
	}
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
}

// A genuine read failure is the opposite case and must stay transient:
// everything keeps running and the refresh is retried.
func TestRefreshFailureKeepsEverythingRunning(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.SetFailGet(errors.New("502 from the tracker"))
	h.Tick()

	if got := h.stateOf("1"); got != StateRunning {
		t.Errorf("state = %q after a failed refresh, want it still running", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times on a failed read", n)
	}
}
