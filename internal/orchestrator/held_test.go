package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// doneHarness drives n issues to done, leaving n held-claim records.
//
// Nothing scripts the assignment: the fake records it when the claim lands,
// as the real tracker does. A fixture that had to plant the anchor by hand
// would agree with an implementation that never established one.
func doneHarness(t *testing.T, n int) *harness {
	t.Helper()
	var issues []core.Issue
	for i := 1; i <= n; i++ {
		issues = append(issues, fake.Issue(fmt.Sprint(i), epoch.Add(time.Duration(i)*time.Minute)))
	}
	h := start(t, harnessOpts{concurrency: fmt.Sprint(n + 1), issues: issues})
	for i := 1; i <= n; i++ {
		h.WaitState(fmt.Sprint(i), StateDone)
	}
	waitFor(t, "the held set to fill", func() bool { return h.o.HeldCount() == n })
	return h
}

// SPEC §9.8: a merged — or manually closed — issue is released by the next
// sweep, on the list response alone. The daemon stays up across the close: a
// fixture that restarted would be asserting the old, restart-coupled
// behavior (BUILD.md B08).
func TestHeldClaimIsReleasedWhenTheIssueCloses(t *testing.T) {
	h := doneHarness(t, 1)
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Fatalf("released %d times at done; the claim is retained while the PR awaits review", n)
	}
	// Everything the run itself owed the change log: §9.5's approving instant at
	// the claim, and the claim-cycle anchor at done. The assertion below is about
	// what the *sweep* costs, so it is measured from here rather than from a
	// fixed count that a new read on the dispatch path would silently change.
	base := h.Tracker.HistoryReads()

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	waitFor(t, "the sweep to release the claim", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	// An ordinary close is settled by the list response; history is for the
	// revision-bump path only.
	if got := h.Tracker.HistoryReads(); got != base {
		t.Errorf("history reads = %d, want no more than the %d the run already made: an ordinary close is settled by the list response", got, base)
	}
}

// The case current state cannot show: closed and reopened between two
// sweeps. The issue reads `open` when the sweep looks, and only the log still
// says it was closed inside this claim cycle. Released, and the reopened
// issue re-enters the queue as new work at attempt 1.
func TestHeldClaimClosedAndReopenedIsStillReleased(t *testing.T) {
	h := doneHarness(t, 1)
	base := h.Tracker.HistoryReads()

	// Both happened between two sweeps; the issue reads open again and the
	// revision moved. The close and the reopen share a timestamp second, so
	// an implementation triggering on updated_at cannot see either.
	h.Tracker.AppendHistory("1",
		core.ClaimEvent{Kind: core.ClaimEventClosed, At: epoch},
		core.ClaimEvent{Kind: core.ClaimEventReopened, At: epoch},
	)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "closed-then-reopened" })

	// Dispatch is held off for the release tick, and only for it. The moment
	// the claim is released this issue is dispatchable again — which is the
	// second half of this very test — and §9.4 runs dispatch after
	// reconciliation *within the same tick*. A re-dispatch there runs the issue
	// back to `done` and converts a second held record, which restores both
	// gauges the next three lines read: the conversion buys its own anchor
	// read, and HeldCount returns to 1. Neither assertion can name the record
	// it means, so the fixture separates the two halves instead of racing them.
	resume := make(chan struct{})
	var once sync.Once
	h.Tracker.SetFetchGate(func() { <-resume })
	h.Tick()

	waitFor(t, "the sweep to release on the log", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	if got := h.Tracker.HistoryReads() - base; got != 1 {
		t.Errorf("history reads = %d for one moved revision, want exactly 1", got)
	}
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })

	// Unassigned and unlabelled is dispatchable: it comes back as new work,
	// not as a continuation of the run that published the PR.
	h.Tracker.SetFetchGate(nil)
	once.Do(func() { close(resume) })
	h.Tick()
	waitFor(t, "the reopened issue to be re-dispatched", func() bool { return h.Runner.StartCount() == 2 })
	for _, s := range h.o.Status() {
		if s.Identifier == "1" && s.Attempt != 1 {
			t.Errorf("re-dispatched at attempt %d, want new work at attempt 1", s.Attempt)
		}
	}
}

// The revision triggers and never decides: a bump that turns out to be a
// comment costs one read and changes nothing.
func TestHeldClaimRevisionBumpWithoutACloseKeepsTheRecord(t *testing.T) {
	h := doneHarness(t, 1)
	base := h.Tracker.HistoryReads()

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "someone-commented" })
	h.Tick()

	waitFor(t, "the history read", func() bool { return h.Tracker.HistoryReads() == base+1 })
	time.Sleep(20 * time.Millisecond)
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times on a revision bump that was not a close", n)
	}
	if got := h.o.HeldCount(); got != 1 {
		t.Errorf("held count = %d, want the record kept", got)
	}

	// Re-baselined: the same revision does not buy a second read.
	h.Tick()
	time.Sleep(20 * time.Millisecond)
	if got := h.Tracker.HistoryReads() - base; got != 1 {
		t.Errorf("history reads = %d since the bump; the revision was not re-baselined", got)
	}
}

// SPEC §9.8: the sweep's cost does not scale with the held set. Held claims
// grow with human review latency, which is the one quantity the daemon does
// not control.
func TestSweepCostDoesNotScaleWithHeldClaims(t *testing.T) {
	cost := func(held int) (sweeps, histories, gets int) {
		h := doneHarness(t, held)
		baseSweep, baseHist, baseGet := h.Tracker.HeldReads(), h.Tracker.HistoryReads(), h.Tracker.GetReads()
		for range 3 {
			h.Tick()
		}
		return h.Tracker.HeldReads() - baseSweep,
			h.Tracker.HistoryReads() - baseHist,
			h.Tracker.GetReads() - baseGet
	}

	one, oneHist, oneGet := cost(1)
	many, manyHist, manyGet := cost(6)

	if many != one {
		t.Errorf("6 held claims cost %d sweep reads, 1 costs %d; the sweep scales with the held set", many, one)
	}
	if manyHist != oneHist {
		t.Errorf("6 held claims cost %d history reads, 1 costs %d — that is the O(held) cost the sweep exists to avoid",
			manyHist, oneHist)
	}
	// Comparing N against 1 is blind to a constant multiplier: a duplicated
	// read costs twice at every size and the ratio still matches. Pin the
	// absolute count too — three ticks, three sweep reads.
	if one != 3 {
		t.Errorf("3 ticks cost %d sweep reads, want exactly 3 — one per tick", one)
	}
	if oneHist != 0 {
		t.Errorf("%d history reads over idle ticks; history is for the revision-bump path only", oneHist)
	}
	// The mechanism, not just the totals: the sweep is a list read. A Get per
	// held claim is O(held) *per tick* against a budget that a few tens of
	// open PRs would exhaust (SPEC §8.5, decision 14).
	if oneGet != 0 || manyGet != 0 {
		t.Errorf("per-issue reads over idle ticks: %d for 1 held claim, %d for 6 — the sweep is one list read, not a Get per record",
			oneGet, manyGet)
	}
}

// The absence path is the other request the held rules can spend, so the bound
// has to hold there too (#148) — and it is where the sweep's own shape was still
// reachable: every held claim missing from one response launched its own
// concurrent Get, so a human unassigning the principal across a review backlog
// cost a read per claim on the tick that noticed.
//
// The case that produces one absence produces all of them, which is why this is
// the same cost decision as the sweep read's and not a rarer one: the held set
// grows with the review backlog, and one gesture spans the backlog.
func TestHeldAbsenceConfirmationsAreBoundedPerTick(t *testing.T) {
	// One tick's confirming reads for n held claims, every one of them absent from
	// the sweep response.
	cost := func(n int) int {
		h := doneHarness(t, n)
		// The assignment *is* the claim (SPEC §8.3), so one gesture over the whole
		// backlog looks like this from here: an assignee-filtered list that no
		// longer returns any of them.
		for i := 1; i <= n; i++ {
			h.Tracker.Mutate(fmt.Sprint(i), func(iss *core.Issue) { iss.Assignees = nil })
		}
		base := h.Tracker.GetReads()
		h.Tick()
		// Waited for positively: the confirming read is issued off the loop, so
		// counting without waiting for one would pass by arriving early.
		waitFor(t, "the tick's confirming read", func() bool { return h.Tracker.GetReads() > base })
		h.settle(len(h.o.Transitions.Entries()))
		return h.Tracker.GetReads() - base
	}

	one, many := cost(1), cost(6)
	if many != one {
		t.Errorf("6 held claims absent at once cost %d confirming read(s) in one tick, 1 costs %d; the absence path scales with the held set",
			many, one)
	}
	// The equality above is the bound, and it is what a budget somebody raised to
	// 6 fails. This pins the absolute number, which is what a budget of 0 — an
	// absence nothing ever confirms — would fail.
	if one != heldConfirmationsPerTick {
		t.Errorf("one absent held claim cost %d confirming read(s) in a tick, want %d",
			one, heldConfirmationsPerTick)
	}
}

// Deferred, not dropped. The bound spreads the absent set over ticks rather than
// dropping any of it: every claim still resolves, at one read apiece, and none of
// them is released — the principal is not assigned, so there is nothing of ours
// for a release to remove (SPEC §9.8).
func TestHeldAbsencesDeferredByTheBoundStillResolve(t *testing.T) {
	const held = 6
	h := doneHarness(t, held)
	for i := 1; i <= held; i++ {
		h.Tracker.Mutate(fmt.Sprint(i), func(iss *core.Issue) { iss.Assignees = nil })
	}
	base := h.Tracker.GetReads()

	for range held + 2 {
		h.Tick()
	}

	waitFor(t, "every absent claim to resolve", func() bool { return h.o.HeldCount() == 0 })
	if got := h.Tracker.GetReads() - base; got > held {
		t.Errorf("confirming reads = %d for %d held claims, want at most one apiece", got, held)
	}
	for i := 1; i <= held; i++ {
		if n := h.Tracker.ReleaseCount(fmt.Sprint(i)); n != 0 {
			t.Errorf("issue %d released %d times; a claim that is not ours is dropped, not released", i, n)
		}
	}
}

// The bound is one confirmation per tick, so **fairness is part of the bound**:
// with a single slot, whoever takes it first every tick starves everyone else.
//
// The offer order must not be what supplies that fairness. Map iteration order is
// unspecified rather than guaranteed random — the runtime happens to randomize it,
// and nothing in the language promises it. So the fixture hands the sweep a
// **stable** order, which is legal, and every confirmation comes back a failure:
// exactly the shape in which the record at the head held the only slot forever.
//
// White-box because the property is about which claim is *offered* the slot, and
// the loop only exposes what came of it. The parked half's twin
// (TestParkedAbsenceConfirmationsRotateUnderAStableOfferOrder) is the same
// property over the other set, and both rest on the same `rotate`.
func TestHeldAbsenceConfirmationsRotateUnderAStableOfferOrder(t *testing.T) {
	const held = 4
	o := idleOrchestrator(t, fake.NewTracker())
	for i := 1; i <= held; i++ {
		id := fmt.Sprint(i)
		o.held[id] = &heldClaim{issue: issueFixture(id), cycleAnchor: int64(i), token: o.newToken()}
	}
	// A read that returned nothing: all of them absent, in one response.
	empty := sweepResult{read: true}

	offered := map[string]int{}
	for range held {
		o.offerHeldConfirmations(t.Context(), o.sweepHeld(t.Context(), empty, o.configNow()))
		for id, h := range o.held {
			if !h.inFlight {
				continue
			}
			offered[id]++
			// The confirmation comes back a failure, which is what makes this the
			// starvation case: the claim is eligible again on the very next tick.
			o.onHeldConfirmed(t.Context(), signal{
				kind: sigHeldConfirmed, issue: id, token: h.token,
				err: errors.New("502 from the tracker"),
			})
		}
	}

	for i := 1; i <= held; i++ {
		if got := offered[fmt.Sprint(i)]; got != 1 {
			t.Errorf("issue %d was offered the confirmation slot %d times over %d ticks, want exactly 1: %v",
				i, got, held, offered)
		}
	}
}

// The same property end-to-end, and the reason it matters: a claim whose absence
// cannot be confirmed must not stop the others from resolving. A demonstration
// rather than the anchor — the rotation test above is what fails deterministically
// when the cursor goes away — because a randomized offer order gives the others a
// turn often enough to pass this one by luck.
func TestAnUnconfirmableHeldAbsenceDoesNotStarveTheOthers(t *testing.T) {
	const held = 4
	h := doneHarness(t, held)
	for i := 1; i <= held; i++ {
		h.Tracker.Mutate(fmt.Sprint(i), func(iss *core.Issue) { iss.Assignees = nil })
	}
	// Issue 1's confirmation never lands; the rest are ordinary unassignments.
	h.Tracker.FailGetFor = func(identifier string) error {
		if identifier == "1" {
			return errors.New("502 from the tracker")
		}
		return nil
	}

	for range held * 3 {
		h.Tick()
	}

	waitFor(t, "every confirmable absence to resolve", func() bool {
		for i := 2; i <= held; i++ {
			// The published snapshot is dropped with the held record, so an empty
			// state is how a resolved claim reads from outside the loop.
			if h.stateOf(fmt.Sprint(i)) != "" {
				return false
			}
		}
		return true
	})
	if got := h.stateOf("1"); got != StateDone {
		t.Errorf("state = %q for the claim whose absence cannot be confirmed, want it retained at %q",
			got, StateDone)
	}
}

// The tick's sweep read is made only when there is something to sweep, and a
// result that carries no read must not be mistaken for one that found
// nothing: a record converted while the tick was out would read as absent
// from the list and buy a confirming Get it has done nothing to earn.
func TestATickThatMadeNoSweepReadSweepsNothing(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker(fake.Issue("1", epoch)))
	o.held["1"] = &heldClaim{issue: issueFixture("1"), cycleAnchor: 40, token: 1}
	o.reconcileInFlight = true

	// A tick whose worker was dispatched before the record existed: no sweep
	// read, so sweepResult is zero.
	o.onReconciled(t.Context(), signal{kind: sigReconciled})

	if h := o.held["1"]; h.inFlight {
		t.Error("an unread sweep was treated as an empty one, and the record was confirmed against a Get")
	}
}

// Mirrors the unroutable rule and §9.10 gate 4: an issue stripped of its
// required labels has left the workflow, so the retained claim has no
// standing.
func TestHeldClaimStrippedOfLabelsIsReleased(t *testing.T) {
	h := doneHarness(t, 1)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	h.Tick()

	waitFor(t, "the sweep to release", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })

	// And it does not come straight back. Releasing an issue that has left the
	// label partition makes it unassigned, which is one of §8.3's conditions —
	// but not the only one, and the release exists precisely because the issue
	// is no longer the workflow's. A dispatch that picked it up here would undo
	// the sweep's verdict within the same tick.
	h.Tick()
	if got := h.Runner.StartCount(); got != 1 {
		t.Errorf("started %d runs; the de-labelled issue was re-dispatched after its claim was released", got)
	}
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; the released issue was picked up and retained again", got)
	}
}

// Absence of a fact is never evidence: an assignee-filtered list cannot
// separate "the principal was unassigned" from consistency lag, so absence is
// confirmed with one Get before anything is dropped.
func TestHeldClaimAbsentFromTheSweepIsConfirmed(t *testing.T) {
	t.Run("confirmed gone is dropped, with nothing to release", func(t *testing.T) {
		h := doneHarness(t, 1)
		h.Tracker.Delete("1")
		h.Tick()

		waitFor(t, "the record to be dropped", func() bool { return h.o.HeldCount() == 0 })
		if n := h.Tracker.ReleaseCount("1"); n != 0 {
			t.Errorf("released %d times for an issue that was already gone", n)
		}
	})

	t.Run("a lagging read drops nothing", func(t *testing.T) {
		h := doneHarness(t, 1)
		// Absent from the list, but the Get still says it is ours: exactly
		// what read-your-writes lag looks like from here.
		lagging := fake.Issue("1", epoch)
		lagging.Assignees = []string{fake.DefaultPrincipal}
		h.Tracker.SetGetResult("1", &lagging)
		h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })
		h.Tick()

		time.Sleep(20 * time.Millisecond)
		if got := h.o.HeldCount(); got != 1 {
			t.Errorf("held count = %d; a claim was dropped on an absence the Get contradicted", got)
		}
		if n := h.Tracker.ReleaseCount("1"); n != 0 {
			t.Errorf("released %d times on a lagging read", n)
		}
	})
}

// Sweep-read failure keeps every record and retries next tick.
func TestSweepFailureKeepsEveryHeldRecord(t *testing.T) {
	h := doneHarness(t, 3)
	h.Tracker.SetFailHeldClaims(errors.New("502 from the tracker"))
	gets := h.Tracker.GetReads()
	h.Tick()

	if got := h.o.HeldCount(); got != 3 {
		t.Errorf("held count = %d after a failed sweep, want all 3 kept", got)
	}
	// Kept because the read failed, not because a per-record Get went and
	// checked. Treating a failed read as an empty list keeps every record
	// too — by way of confirming each one's absence, which is the O(held)
	// cost this sweep exists to avoid, spent on every flaky tick.
	if got := h.Tracker.GetReads() - gets; got != 0 {
		t.Errorf("a failed sweep spent %d per-issue reads; a failed read is a reason to do nothing", got)
	}

	h.Tracker.SetFailHeldClaims(nil)
	h.Tracker.Mutate("2", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the retried sweep", func() bool { return h.Tracker.ReleaseCount("2") == 1 })
}

// A close from a *previous* claim cycle says nothing about the current one
// (SPEC §9.10 step 2). Without a real anchor every historical close would
// satisfy the check and release a live claim.
func TestHistoricalCloseDoesNotReleaseTheCurrentClaim(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		// The issue was closed and reopened before this daemon ever claimed
		// it. The claim the fake records on Claim orders after these.
		beforeStart: func(tr *fake.Tracker) {
			tr.SetHistory("1",
				core.ClaimEvent{Kind: core.ClaimEventClosed, ID: 2},
				core.ClaimEvent{Kind: core.ClaimEventReopened, ID: 3},
			)
		},
	})
	h.WaitState("1", StateDone)
	waitFor(t, "the held record", func() bool { return h.o.HeldCount() == 1 })

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "moved" })
	h.Tick()

	waitFor(t, "the history read", func() bool { return h.Tracker.HistoryReads() >= 2 })
	time.Sleep(20 * time.Millisecond)
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times on a close from an earlier claim cycle", n)
	}
	if got := h.o.HeldCount(); got != 1 {
		t.Errorf("held count = %d, want the record kept", got)
	}
}

// A held release the tracker refused is retried on the next sweep. The entry
// has to outlive the failed write: a claim standing with nothing tracking it
// blocks the issue for everyone, including us (SPEC §8.3).
func TestFailedHeldReleaseIsRetried(t *testing.T) {
	h := doneHarness(t, 1)
	h.Tracker.SetFailRelease(errors.New("503 from the tracker"))

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	if got := h.o.HeldCount(); got != 1 {
		t.Fatalf("held count = %d after a failed release; the claim would stand with nothing tracking it", got)
	}

	h.Tracker.SetFailRelease(nil)
	h.Tick()
	waitFor(t, "the retried release", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
}

// The ownership boundary: the run record owns the issue until every write
// `done` ordered has landed, and only then converts.
//
// Converting at the `done` verdict instead would hand the claim to a record
// that cannot retry those writes — and a sweep that released and dropped it a
// tick later would take the unposted publish comment with it.
func TestConversionWaitsForTheDoneEffects(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			// ben:claimed, ben:running, then the clear at done — which is the
			// first of the writes the done record owes.
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)
	// A tick while the queue is held open: conversion is driven from here as
	// well as from the landing write, so without it the assertion below would
	// pass on an implementation that converts too early simply because
	// nothing had asked it to yet.
	h.Tick()

	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d while the done record still owes the tracker its writes", got)
	}
	if got := h.Tracker.Milestones("1"); containsMilestone(got, core.MilestonePublished) {
		t.Fatalf("milestones = %v; the fixture did not actually hold the queue open", got)
	}

	close(gate)
	waitFor(t, "the conversion once the writes landed", func() bool { return h.o.HeldCount() == 1 })
	waitFor(t, "the publish comment", func() bool {
		return containsMilestone(h.Tracker.Milestones("1"), core.MilestonePublished)
	})
}

// The anchor is the assignment that established the claim cycle, so a log
// that shows none means we hold nothing: no record to build, no claim to
// release, and the run record dropped rather than left in done forever.
//
// The alternative — build the record anyway and carry a zero anchor — is the
// shape that made the sweep accept every close ever recorded on the issue.
func TestUnassignedAtDoneIsNotRetained(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	// A human took the issue off us while the run was finishing — recorded on
	// the log, which is what the anchor read consults. Held open until it is
	// there, so the read cannot happen first.
	h.Tracker.AppendHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventUnassigned, Subject: fake.DefaultPrincipal, At: epoch,
	})
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })
	close(gate)

	h.WaitGone("1")
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; a held record was built on a claim the log says we do not hold", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times; there was no assignment of ours to remove", n)
	}
}

// SPEC §9.10 step 2 anchors the claim cycle on the assignment that
// established it, so a record cannot be built without one. A history read
// that failed is retried; the claim stays retained meanwhile, which is what
// `done` asks for anyway — what waits is only its release.
func TestAnchorReadFailureRetainsTheClaimAndRetries(t *testing.T) {
	verifyGate := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			<-verifyGate
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
	})
	// Installed after the claim, not in beforeStart: the change log is read twice
	// in a run — §9.5's approving instant at the claim, and the anchor at done —
	// and failing the first would park the issue for reapproval instead of
	// exercising the anchor path this test is about.
	h.WaitState("1", StateVerifying)
	h.Tracker.SetFailHistory(errors.New("502 from the tracker"))
	close(verifyGate)
	h.WaitState("1", StateDone)

	time.Sleep(20 * time.Millisecond)
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; a record was built without an anchor, which accepts every historical close", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times; the claim is retained while the anchor is unresolved", n)
	}
	if got := h.stateOf("1"); got != StateDone {
		t.Errorf("state = %q, want the run record kept until it can convert", got)
	}

	h.Tracker.SetFailHistory(nil)
	h.Tick()
	waitFor(t, "the retried anchor read to convert", func() bool { return h.o.HeldCount() == 1 })
}

// One owner, from the other side: an issue whose claim we still hold is not
// dispatched, even if the tracker calls it dispatchable. The held record is
// dropped only once the release is confirmed, and until then a second run
// would be operating on an issue that already has one.
func TestHeldIssueIsNotRedispatched(t *testing.T) {
	h := doneHarness(t, 1)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Dispatchable = true })
	h.Tick()

	time.Sleep(20 * time.Millisecond)
	if got := h.Runner.StartCount(); got != 1 {
		t.Errorf("started %d runs, want the held issue left alone", got)
	}
	if got := h.o.HeldCount(); got != 1 {
		t.Errorf("held count = %d, want the record kept", got)
	}
}

// SPEC §9.2 names two cases that stay restart-coupled by design. A test
// asserting release on a tick for either would encode a guarantee the design
// does not make (BUILD.md B08).
func TestRestartCoupledCasesAreNotReleasedOnATick(t *testing.T) {
	t.Run("a PR closed unmerged leaves no event on the issue", func(t *testing.T) {
		h := doneHarness(t, 1)
		base := h.Tracker.HistoryReads()
		// Rejecting a PR neither closes the issue nor touches it at all.
		h.Tick()
		h.Tick()

		time.Sleep(20 * time.Millisecond)
		if n := h.Tracker.ReleaseCount("1"); n != 0 {
			t.Errorf("released %d times with nothing observable on the issue", n)
		}
		if got := h.Tracker.HistoryReads() - base; got != 0 {
			t.Errorf("%d history reads with no revision moved", got)
		}
		if got := h.o.HeldCount(); got != 1 {
			t.Errorf("held count = %d, want the claim standing until a restart reclassifies it", got)
		}
	})

	t.Run("a close the revision projection cannot express", func(t *testing.T) {
		h := doneHarness(t, 1)
		// The close and the reopen are on the log, but the projection they
		// move — state, state_reason, updated_at — reads the same as before,
		// so the token does not change and the sweep never learns to look.
		h.Tracker.AppendHistory("1",
			core.ClaimEvent{Kind: core.ClaimEventClosed, At: epoch},
			core.ClaimEvent{Kind: core.ClaimEventReopened, At: epoch},
		)
		h.Tick()

		time.Sleep(20 * time.Millisecond)
		if n := h.Tracker.ReleaseCount("1"); n != 0 {
			t.Errorf("released %d times on a close the sweep has no way to observe", n)
		}
		if got := h.o.HeldCount(); got != 1 {
			t.Errorf("held count = %d, want the claim standing until §9.10 gate 1 reads the log", got)
		}
	})
}

// Every async held result is keyed to the cycle token it was started under.
// The in-flight guard stops two operations overlapping *within* a cycle; the
// token is what stops one crossing a cycle boundary, after the issue was
// released, reopened, re-dispatched and held again.
//
// Driven against a constructed-but-not-running orchestrator: New starts no
// goroutines, so the handlers can be called directly without racing the loop
// that owns this state.
func TestStaleHeldResultsDecideNothing(t *testing.T) {
	current := 7
	stale := signal{issue: "1", token: current - 1}

	unassigned := issueFixture("1")
	notOurs := &unassigned

	cases := []struct {
		name string
		call func(*Orchestrator, signal)
		s    signal
	}{
		{
			name: "a release confirmed for the previous cycle",
			call: func(o *Orchestrator, s signal) { o.onHeldReleased(t.Context(), s) },
			s:    stale,
		},
		{
			name: "an absence confirmed for the previous cycle",
			call: func(o *Orchestrator, s signal) { o.onHeldConfirmed(t.Context(), s) },
			s:    signal{issue: stale.issue, token: stale.token, refetched: notOurs},
		},
		{
			name: "a history read from the previous cycle",
			call: func(o *Orchestrator, s signal) { o.onHeldHistory(t.Context(), s) },
			s:    signal{issue: stale.issue, token: stale.token, refetched: notOurs, verified: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := idleOrchestrator(t, fake.NewTracker(fake.Issue("1", epoch)))
			o.held["1"] = &heldClaim{
				issue: issueFixture("1"), revision: "current", cycleAnchor: 40,
				token: current, inFlight: true,
			}

			tc.call(o, tc.s)

			h, ok := o.held["1"]
			if !ok {
				t.Fatal("a result from a previous held cycle dropped the current cycle's claim")
			}
			if !h.inFlight {
				t.Error("a result from a previous held cycle cleared the current cycle's in-flight operation")
			}
			if h.releasing || h.revision != "current" {
				t.Errorf("a result from a previous held cycle decided for the current one: releasing=%v revision=%q",
					h.releasing, h.revision)
			}
		})
	}

	// The positive control: the same result, keyed to the current cycle.
	t.Run("the current cycle's release is honored", func(t *testing.T) {
		o := idleOrchestrator(t, fake.NewTracker(fake.Issue("1", epoch)))
		o.held["1"] = &heldClaim{issue: issueFixture("1"), token: current, inFlight: true, releasing: true}

		o.onHeldReleased(t.Context(), signal{issue: "1", token: current})

		if _, ok := o.held["1"]; ok {
			t.Fatal("a confirmed release did not drop the held record")
		}
	})
}

// The handoff gate is rechecked after the anchor read, not only before it.
//
// A tick that snapshotted this issue while it was still `verifying`
// reconciles it once it has reached `done`, and can decide there that the
// issue went terminal or unroutable — queueing a release on the record. A
// conversion built on a read that predates that decision deletes the release
// with the record.
func TestConversionYieldsToAReleaseDecidedWhileTheAnchorReadWasOut(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker(fake.Issue("1", epoch)))
	r := &Record{Issue: fake.Issue("1", epoch), State: StateDone}
	o.records["1"] = r
	// The anchor read is out: it started when the record owed nothing.
	r.convertInFlight = true

	// Meanwhile, reconciliation finds the issue unroutable and exits it.
	o.release(t.Context(), r, "issue is no longer routable")

	o.onClaimAnchor(t.Context(), r, signal{issue: "1", anchor: 42})

	if len(o.held) != 0 {
		t.Error("converted over a release already begun; the held record has no memory of it")
	}
	if _, ok := o.records["1"]; !ok {
		t.Fatal("the run record was deleted with its release still owed, and nothing re-drives an owed write for a record that is gone")
	}
	if !r.owesAnything() {
		t.Error("the owed release was dropped")
	}
}

// The same race end to end, and the consequence the recheck prevents: a
// release that failed once is retried from the run record, so a claim the
// tracker refused to drop is not left standing.
//
// An unroutable *assignee set* is invisible to the sweep — the sweep read is
// assignee-filtered, so being in it is the only assignee question it can
// answer — which is why losing this release strands the claim until a
// restart rather than being re-derived a tick later.
func TestAReleaseRacedByTheConversionIsStillRetried(t *testing.T) {
	verifyGate, getGate, historyGate := make(chan struct{}), make(chan struct{}), make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			<-verifyGate
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
		beforeStart: func(tr *fake.Tracker) {
			tr.SetGetGate(func() { <-getGate })
			tr.SetFailRelease(errors.New("503 from the tracker"))
		},
	})
	h.WaitState("1", StateVerifying)
	// After the claim: the §9.5 approving-instant read uses the same change log,
	// and gating it in beforeStart would hold the record in `queued` instead of
	// the conversion this test races.
	h.Tracker.SetHistoryGate(func() { <-historyGate })

	// A tick snapshots the issue while it is verifying, and its read stalls.
	h.Tick()
	waitFor(t, "the tick's refresh read", func() bool { return h.Tracker.GetReads() > 0 })

	// A human co-assigns themselves: active, but no longer ours alone.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = []string{fake.DefaultPrincipal, "a-human"} })

	// The run finishes and its writes land, so the conversion begins — and
	// stalls on the anchor read.
	close(verifyGate)
	h.WaitState("1", StateDone)
	waitFor(t, "the anchor read", func() bool { return h.Tracker.HistoryReads() > 0 })

	// Now the stalled tick lands: reconciliation queues the release, and the
	// tracker refuses it.
	close(getGate)
	waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// And only now does the anchor read return.
	close(historyGate)

	h.Tracker.SetFailRelease(nil)
	h.Tick()
	waitFor(t, "the release to be retried", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; an issue being released is not a retained claim", got)
	}
}

// A history read that shows no assignment is the absence of a fact, and
// §9.10 never reads that as evidence: a lagging or truncated change log looks
// exactly like a human unassignment. Confirmed against current assignment,
// and while the issue is still ours the claim is retained rather than
// dropped.
func TestAnchorlessHistoryOnAnAssignedIssueRetainsTheClaim(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	// The log answers, and does not carry the assignment that established the
	// claim — while the issue itself is still assigned to the principal.
	h.Tracker.SetHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventLabeled, Subject: "ben-queue", ID: 9, At: epoch,
	})
	close(gate)

	// Wait on the anchor read, which happens either way, so what follows is
	// the verdict and not a race with it.
	waitFor(t, "the anchor read", func() bool { return h.Tracker.HistoryReads() > 0 })
	time.Sleep(20 * time.Millisecond)
	if got := h.stateOf("1"); got != StateDone {
		t.Errorf("state = %q; the claim was dropped on a log that simply had not caught up", got)
	}
	if got := h.Tracker.GetReads(); got == 0 {
		t.Error("nothing confirmed the claim against current assignment; the log's silence was taken as the answer")
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times without evidence that the claim was not ours", n)
	}
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; a record was built without the anchor it needs", got)
	}

	// Held, not wedged: once the log catches up the conversion completes.
	h.Tracker.AppendHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventAssigned, Subject: fake.DefaultPrincipal, At: epoch,
	})
	h.Tick()
	waitFor(t, "the retried anchor read to convert", func() bool { return h.o.HeldCount() == 1 })
}

// SPEC §9.10 step 2: evidence means evidence dated after the claim-establishing
// event, and anything earlier belongs to a previous cycle. A human who
// unassigns and reassigns the principal between two sweeps starts a new one,
// so the stored anchor is stale — and releasing on the old cycle's close would
// drop an assignment a human had just made, putting the issue back in the
// queue on top of a PR that already exists.
// Asserted in both directions on the same shape. "Does not release" alone
// passes for any implementation that cannot derive an anchor at all and so
// never releases anything — the close *after* the reassignment is what says
// the rule still works, and that the first case was withheld for the reason
// it names.
func TestACloseIsJudgedAgainstTheAssignmentNowStanding(t *testing.T) {
	reassign := []core.ClaimEvent{
		{Kind: core.ClaimEventUnassigned, Subject: fake.DefaultPrincipal, At: epoch},
		{Kind: core.ClaimEventAssigned, Subject: fake.DefaultPrincipal, At: epoch},
	}
	closeAndReopen := []core.ClaimEvent{
		{Kind: core.ClaimEventClosed, At: epoch},
		{Kind: core.ClaimEventReopened, At: epoch},
	}

	t.Run("a close before the reassignment does not release the new claim", func(t *testing.T) {
		h := doneHarness(t, 1)
		// Closed and reopened inside the first cycle, then taken away and
		// given back — which is where the current cycle starts.
		h.Tracker.AppendHistory("1", append(closeAndReopen, reassign...)...)
		h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "reassigned" })
		h.Tick()

		waitFor(t, "the history read", func() bool { return h.Tracker.HistoryReads() > 1 })
		time.Sleep(20 * time.Millisecond)
		if n := h.Tracker.ReleaseCount("1"); n != 0 {
			t.Errorf("released %d times on a close that predates the assignment now standing", n)
		}
		if got := h.o.HeldCount(); got != 1 {
			t.Errorf("held count = %d, want the reassigned claim kept", got)
		}
	})

	t.Run("a close after it still releases", func(t *testing.T) {
		h := doneHarness(t, 1)
		h.Tracker.AppendHistory("1", append(reassign, closeAndReopen...)...)
		h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "reassigned-then-closed" })

		// Dispatch is held off for the release tick. The issue is open and
		// still labelled, so releasing it makes it dispatchable again, and
		// §9.4 runs dispatch after reconciliation inside the same tick — a
		// re-dispatch there runs it back to `done` and converts a second held
		// record, restoring the count this asserts on.
		resume := make(chan struct{})
		var once sync.Once
		t.Cleanup(func() { once.Do(func() { close(resume) }) })
		h.Tracker.SetFetchGate(func() { <-resume })
		h.Tick()

		waitFor(t, "the sweep to release on the log", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
		waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	})
}

// A settled release is an owed write, not a conclusion of the current list
// response. Gating its retry on that read would leave decided claims standing
// for as long as the tracker kept failing an unrelated request.
func TestASettledReleaseRetriesThroughASweepReadFailure(t *testing.T) {
	h := doneHarness(t, 1)
	h.Tracker.SetFailRelease(errors.New("503 from the tracker"))
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// The verdict is settled. Now the list read starts failing, and the write
	// that was already decided must not wait on it.
	h.Tracker.SetFailHeldClaims(errors.New("502 from the tracker"))
	h.Tracker.SetFailRelease(nil)
	h.Tick()

	waitFor(t, "the settled release to retry", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
}

// The other half of a settled verdict: it is retried, and it is not
// re-derived. A record on its way out must not keep buying reads to re-answer
// a question that has been answered — the sweep's cost bound is over the
// whole held set, releasing records included.
func TestASettledReleaseIsNotReDerived(t *testing.T) {
	h := doneHarness(t, 1)
	h.Tracker.SetFailRelease(errors.New("503 from the tracker"))

	// Settled by the log: closed inside this claim cycle, then reopened, so
	// the issue still reads open and still carries its labels.
	h.Tracker.AppendHistory("1",
		core.ClaimEvent{Kind: core.ClaimEventClosed, At: epoch},
		core.ClaimEvent{Kind: core.ClaimEventReopened, At: epoch},
	)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "closed-then-reopened" })
	h.Tick()
	waitFor(t, "the release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// Something else happens on the issue while the release is retrying — a
	// comment, say. The revision moves again.
	reads := h.Tracker.HistoryReads()
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "someone-commented" })
	h.Tick()
	h.Tick()

	time.Sleep(20 * time.Millisecond)
	if got := h.Tracker.HistoryReads() - reads; got != 0 {
		t.Errorf("%d history reads for a record already releasing; the verdict was settled two ticks ago", got)
	}
	if got := h.o.HeldCount(); got != 1 {
		t.Fatalf("held count = %d; the release was refused, so the record must outlive it", got)
	}

	h.Tracker.SetFailRelease(nil)
	h.Tick()
	waitFor(t, "the settled release to land", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
}

// #135. The gap the two rules above leave between them: a settled release stays
// owed and is retried, and the sweep refuses to re-derive it — so if the issue
// goes away after the release is ordered, the write fails forever, the held entry
// is never dropped, and the write's own failure cannot say why.
//
// It cannot, and that is the whole design: a write's 404 can equally mean a label,
// an assignee or a comment target the request named (#134). The fake models this
// exactly — Release for an absent issue returns an error that does *not* wrap
// core.ErrIssueNotFound, while Get's does — so a fixture cannot accidentally prove
// this by way of a classification the real tracker never offers.
func TestAnUnlandableHeldReleaseIsConfirmedGoneAndForgotten(t *testing.T) {
	h := doneHarness(t, 1)

	// Ordered against an issue that is still there — a human closed it — and
	// refused once, which is all it takes for the write to be something that
	// needs explaining.
	h.Tracker.SetFailRelease(errors.New("503 from the tracker"))
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// Now the issue goes away. With the injected failure cleared, the write still
	// fails — because the issue is gone — and still proves nothing.
	h.Tracker.Delete("1")
	h.Tracker.SetFailRelease(nil)

	h.tickUntil("the confirmed-gone claim to be forgotten", func() bool { return h.o.HeldCount() == 0 })

	// The retries stop with the entry: nothing tracks the issue any more.
	attempts := h.Tracker.ReleaseAttempts("1")
	for range 3 {
		h.Tick()
	}
	if got := h.Tracker.ReleaseAttempts("1"); got != attempts {
		t.Errorf("release attempts went %d → %d after the claim was forgotten; the write is still owed",
			attempts, got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times for an issue the tracker no longer has", n)
	}
}

// The other absence a confirming read can state: the issue is there, but the
// claim is not. The claim *is* the assignment (SPEC §8.3), so a principal that is
// no longer assigned has nothing left for a release to remove — and the release,
// which is what is failing, is in no position to say so.
//
// Reached only through this path, deliberately: the record is releasing, so the
// assignee-filtered sweep read that no longer returns it classifies it under no
// rule at all (sweepHeld).
func TestAnUnlandableHeldReleaseIsForgottenWhenThePrincipalIsUnassigned(t *testing.T) {
	h := doneHarness(t, 1)
	h.Tracker.SetFailRelease(errors.New("403 from the tracker"))
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// A human takes the assignment back while the write is still being refused.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })

	h.tickUntil("the claim to be forgotten", func() bool { return h.o.HeldCount() == 0 })
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times; a claim that is not ours is dropped, not released", n)
	}
}

// A confirmation that retires an unlandable release must leave no write from
// that held cycle behind. The release runs on the serial effect queue, so
// "in-flight" includes work accepted by the queue but not started yet. Dropping
// the record while that write is pending lets the issue be assigned again, then
// lets the old cycle's write remove the new assignment; the result token rejects
// only the stale completion, not the write itself.
func TestAConfirmedHeldReleaseLeavesNoWriteThatCanCrossClaimCycles(t *testing.T) {
	tracker := fake.NewTracker(fake.Issue("1", epoch))
	tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })
	o := idleOrchestrator(t, tracker)
	h := &heldClaim{
		issue: issueFixture("1"), cycleAnchor: 40, token: o.newToken(),
		releasing: true, releaseFailed: true, why: "issue went terminal",
	}
	o.held["1"] = h

	// Drive the same release/confirmation offer order as one reconciliation
	// turn. With no effect worker, any accepted release stays visibly queued.
	o.onReconciled(t.Context(), signal{kind: sigReconciled})
	if !h.inFlight || len(o.effects) != 0 {
		t.Fatal("the failed release was offered no confirmation")
	}
	fresh, err := tracker.Get(t.Context(), "1")
	if err != nil {
		t.Fatal(err)
	}
	o.onHeldConfirmed(t.Context(), signal{
		kind: sigHeldConfirmed, issue: "1", token: h.token, refetched: fresh,
	})
	if _, ok := o.held["1"]; ok {
		t.Fatal("the confirmed-unassigned held claim was not forgotten")
	}

	// This is a new claim cycle. Execute anything the old one left on the
	// serial queue; it must not be able to remove this assignment.
	tracker.Mutate("1", func(i *core.Issue) {
		i.Assignees = []string{fake.DefaultPrincipal}
	})
	select {
	case queued := <-o.effects:
		queued(t.Context())
	default:
	}
	latest, err := tracker.Get(t.Context(), "1")
	if err != nil {
		t.Fatal(err)
	}
	if !containsFold(latest.Assignees, fake.DefaultPrincipal) {
		t.Fatal("a release queued for the forgotten held cycle removed the new assignment")
	}
}

// The two answers that are not evidence. A read that could not be made is not an
// absence (SPEC §9.10), and a read that says the claim is still ours is a positive
// answer against dropping it — the write is failing for some reason of its own,
// and the claim it would release is still standing.
func TestAnUnlandableHeldReleaseSurvivesAnInconclusiveConfirmation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*fake.Tracker)
	}{
		{
			name:    "the confirming read cannot be made",
			arrange: func(tr *fake.Tracker) { tr.SetFailGet(errors.New("502 from the tracker")) },
		},
		{
			// Nothing to arrange: the claim landed, the release keeps being
			// refused, so the principal is still assigned.
			name:    "the issue is still assigned to the principal",
			arrange: func(*fake.Tracker) {},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := doneHarness(t, 1)
			h.Tracker.SetFailRelease(errors.New("503 from the tracker"))
			h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
			h.Tick()
			waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })
			tc.arrange(h.Tracker)

			attempts := h.Tracker.ReleaseAttempts("1")
			for range 3 {
				h.Tick()
			}
			if got := h.o.HeldCount(); got != 1 {
				t.Fatalf("held count = %d; an inconclusive confirmation is not a reason to forget a claim", got)
			}
			if got := h.Tracker.ReleaseAttempts("1"); got <= attempts {
				t.Errorf("release attempts stayed at %d over three ticks; a retained claim's release keeps retrying",
					got)
			}

			// And the obligation was retained, not merely deferred: once the
			// tracker takes the write, it lands.
			h.Tracker.SetFailGet(nil)
			h.Tracker.SetFailRelease(nil)
			h.tickUntil("the settled release to land", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
			waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
		})
	}
}

// The sweep read is neither the release's precondition nor its confirmation's.
// Gating either on it would put an already-decided write, and the one read that
// can retire it, behind an unrelated request that may never succeed again.
func TestAFailedSweepGatesNeitherTheOwedReleaseNorItsConfirmation(t *testing.T) {
	h := doneHarness(t, 1)
	h.Tracker.SetFailRelease(errors.New("503 from the tracker"))
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// From here the list read never succeeds again. The confirming Get still
	// runs, says the claim is ours, and its inconclusive answer re-drives the
	// release immediately — consecutively rather than beside it.
	h.Tracker.SetFailHeldClaims(errors.New("502 from the tracker"))
	h.Tracker.SetFailRelease(nil)

	reads := h.Tracker.GetReads()
	attempts := h.Tracker.ReleaseAttempts("1")
	h.tickUntil("the release to land through a failing sweep", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	if got := h.Tracker.GetReads(); got <= reads {
		t.Errorf("confirming reads stayed at %d while the sweep read was failing; the confirmation was gated on it", got)
	}
	if got := h.Tracker.ReleaseAttempts("1"); got <= attempts {
		t.Errorf("release attempts stayed at %d while the sweep read was failing; the write was gated on it", got)
	}
}

// One budget, not two. The absent set and the unlandable releases ask the same
// question of the same read, so §9.8 pays for one answer per tick however many
// records are asking — and the rotation is what stops either set holding the slot
// (see TestReleaseConfirmationsShareTheHeldConfirmationRotation for that half).
func TestHeldConfirmationsShareOneBudgetAcrossAbsencesAndReleases(t *testing.T) {
	h := doneHarness(t, 2)

	// Issue 2 is a settled release the tracker keeps refusing. Issue 1 is an
	// ordinary absence: unassigned, so the assignee-filtered sweep read no longer
	// returns it. Two roads to one question.
	h.Tracker.SetFailRelease(errors.New("503 from the tracker"))
	h.Tracker.Mutate("2", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "issue 2's first release attempt", func() bool { return h.Tracker.ReleaseAttempts("2") > 0 })
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })

	// Nothing else spends a Get on this tick: both records are held, so §9.8's
	// per-record refresh set is empty.
	base := h.Tracker.GetReads()
	h.Tick()
	// Waited for positively: the confirming read is issued off the loop, so
	// counting without waiting for one would pass by arriving early.
	waitFor(t, "the tick's confirming read", func() bool { return h.Tracker.GetReads() > base })
	h.settle(len(h.o.Transitions.Entries()))
	if got := h.Tracker.GetReads() - base; got != heldConfirmationsPerTick {
		t.Errorf("one absent claim and one unlandable release cost %d confirming read(s) in a tick, want %d: the releases have a budget of their own",
			got, heldConfirmationsPerTick)
	}

	// And neither starves the other: both resolve, at one read apiece per turn.
	h.Tracker.Delete("2")
	h.Tracker.SetFailRelease(nil)
	h.tickUntil("both claims to resolve", func() bool { return h.o.HeldCount() == 0 })
}

// The fairness half, white-box for the reason its absence-only twin above is
// (TestHeldAbsenceConfirmationsRotateUnderAStableOfferOrder): the property is
// about which claim is *offered* the one slot, and the loop only exposes what came
// of it.
//
// With a single slot shared by two candidate sets, a rotation that walked one set
// before the other would starve the second indefinitely — and an unlandable
// release is the permanent candidate that would do the starving, since it stays
// eligible for as long as the tracker keeps refusing it.
func TestReleaseConfirmationsShareTheHeldConfirmationRotation(t *testing.T) {
	const held = 4
	o := idleOrchestrator(t, fake.NewTracker())
	for i := 1; i <= held; i++ {
		id := fmt.Sprint(i)
		h := &heldClaim{issue: issueFixture(id), cycleAnchor: int64(i), token: o.newToken()}
		// Interleaved by identifier, which is what the rotation sorts on, so a
		// cursor that walked one set and then the other fails visibly.
		if i%2 == 0 {
			h.releasing, h.releaseFailed, h.why = true, true, "issue went terminal"
		}
		o.held[id] = h
	}
	// A read that returned nothing. The releasing records are absent from it too,
	// and are candidates for a reason that has nothing to do with it.
	empty := sweepResult{read: true}

	offered := map[string]int{}
	for range held {
		o.offerHeldConfirmations(t.Context(), o.sweepHeld(t.Context(), empty, o.configNow()))
		var out int
		for id, h := range o.held {
			if !h.inFlight {
				continue
			}
			out++
			offered[id]++
			// This test owns the offer seam, not the I/O driver. Model the failed
			// answer and the failed retry it would re-drive by making the claim
			// eligible again; that permanent eligibility is the starvation case.
			h.inFlight = false
			if h.releasing {
				h.releaseFailed = true
			}
		}
		if out != heldConfirmationsPerTick {
			t.Fatalf("%d confirmations offered in one tick, want %d: the two candidate sets are not sharing one budget",
				out, heldConfirmationsPerTick)
		}
	}

	for i := 1; i <= held; i++ {
		if got := offered[fmt.Sprint(i)]; got != 1 {
			t.Errorf("issue %d was offered the confirmation slot %d times over %d ticks, want exactly 1: %v",
				i, got, held, offered)
		}
	}
}

// The confirmation asks one question and re-opens none. A settled release is
// settled: its reason is not re-derived, its revision is not re-baselined, and it
// buys no ClaimHistory read — which is the read the sweep would otherwise spend on
// a record whose revision has moved, every tick, for as long as the write kept
// failing.
//
// White-box because "no verdict changed" is a statement about the record, and the
// record is what the loop owns.
func TestConfirmingAnUnlandableReleaseReDerivesNothing(t *testing.T) {
	tracker := fake.NewTracker(fake.Issue("1", epoch))
	o := idleOrchestrator(t, tracker)
	h := &heldClaim{
		issue: issueFixture("1"), revision: "at-release", cycleAnchor: 40,
		token: o.newToken(), releasing: true, releaseFailed: true,
		why: "issue was closed inside this claim cycle",
	}
	o.held["1"] = h

	// Everything the sweep re-derives from: the issue is back in the response,
	// open, carrying its labels, with a revision that has moved since the verdict.
	fresh := issueFixture("1")
	fresh.Revision = "reopened-and-commented"
	absent := o.sweepHeld(t.Context(), sweepResult{read: true, issues: []core.Issue{fresh}}, o.configNow())
	if len(absent) != 0 {
		t.Fatalf("absent = %v; a record the response contains is not an absence", absent)
	}
	o.offerHeldConfirmations(t.Context(), absent)

	if !h.inFlight {
		t.Fatal("the unlandable release was offered no confirmation")
	}
	if len(o.effects) != 0 {
		t.Error("a settled release queued its write beside the confirmation")
	}
	if got := tracker.HistoryReads(); got != 0 {
		t.Errorf("history reads = %d; a record already releasing re-derives nothing", got)
	}

	// The answer that changes nothing: the claim is still ours.
	still := fresh
	still.Assignees = []string{fake.DefaultPrincipal}
	o.onHeldConfirmed(t.Context(), signal{
		kind: sigHeldConfirmed, issue: "1", token: h.token, refetched: &still,
	})

	switch {
	case o.held["1"] == nil:
		t.Fatal("the claim was dropped by a confirmation that said it is still ours")
	case !h.releasing:
		t.Error("the settled release was retired by a confirmation, not by the tracker taking the write")
	case h.why != "issue was closed inside this claim cycle":
		t.Errorf("reason = %q; a settled release reason is never re-derived", h.why)
	case h.revision != "at-release":
		t.Errorf("revision = %q; re-baselining it buys the history read this record must never spend", h.revision)
	}
	if !h.inFlight || len(o.effects) != 1 {
		t.Errorf("an inconclusive confirmation did not re-drive the release: inFlight=%v queued=%d",
			h.inFlight, len(o.effects))
	}
	if got := tracker.HistoryReads(); got != 0 {
		t.Errorf("history reads = %d after the confirmation; no ClaimHistory read may follow it", got)
	}
}

// SPEC §9.10 step 2 names the claim-establishing event as the *most recent*
// assignment with no later unassignment. Earliest-wins widens the cycle to
// admit closes that precede the assignment now standing.
func TestClaimCycleAnchorTakesTheMostRecentSurvivingAssignment(t *testing.T) {
	const me = fake.DefaultPrincipal
	assigned := func(id int64, who string) core.ClaimEvent {
		return core.ClaimEvent{Kind: core.ClaimEventAssigned, Subject: who, ID: id}
	}
	unassigned := func(id int64, who string) core.ClaimEvent {
		return core.ClaimEvent{Kind: core.ClaimEventUnassigned, Subject: who, ID: id}
	}
	closed := func(id int64) core.ClaimEvent {
		return core.ClaimEvent{Kind: core.ClaimEventClosed, ID: id}
	}

	cases := []struct {
		name       string
		events     []core.ClaimEvent
		wantAnchor int64
		wantClosed bool
	}{
		{name: "no events at all", wantAnchor: 0},
		{
			name:   "assigned then unassigned is no live claim",
			events: []core.ClaimEvent{assigned(10, me), unassigned(11, me)},
		},
		{
			name:       "reassigned after an unassignment anchors on the new one",
			events:     []core.ClaimEvent{assigned(10, me), closed(11), unassigned(12, me), assigned(13, me)},
			wantAnchor: 13,
			wantClosed: false,
		},
		{
			name:       "two assignments with no unassignment between them take the later",
			events:     []core.ClaimEvent{assigned(10, me), closed(11), assigned(12, me)},
			wantAnchor: 12,
			wantClosed: false,
		},
		{
			name:       "a close after the standing assignment is inside the cycle",
			events:     []core.ClaimEvent{assigned(10, me), assigned(12, me), closed(13)},
			wantAnchor: 12,
			wantClosed: true,
		},
		{
			name:       "another party's assignment is not ours",
			events:     []core.ClaimEvent{assigned(10, "a-human"), assigned(11, me), unassigned(12, "a-human")},
			wantAnchor: 11,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := claimCycleAnchor(tc.events, me)
			if got != tc.wantAnchor {
				t.Errorf("anchor = %d, want %d", got, tc.wantAnchor)
			}
			if closed := closedInCycle(tc.events, got); closed != tc.wantClosed {
				t.Errorf("closed in cycle = %v, want %v (anchor %d)", closed, tc.wantClosed, got)
			}
		})
	}
}

// A result that outlives the record it was started for must not land on the
// next record for the same issue.
//
// The conversion reads are the ones that can: they deliberately do not take
// `pending` — a `done` record has to stay releasable while they are out —
// so a release can forget the record under them. The issue is then reopened,
// re-dispatched, and carried to `done` again by a replacement. Both records
// are on their first attempt, so **both have generation zero**: generation
// counts attempts within a record and restarts, so it matches the stale
// result exactly. Only a token that never repeats separates them.
func TestAResultCannotLandOnTheNextRecordForTheIssue(t *testing.T) {
	setup := func(t *testing.T) (*Orchestrator, int, *Record) {
		t.Helper()
		o := idleOrchestrator(t, fake.NewTracker(fake.Issue("1", epoch)))
		departed := o.newToken()
		next := &Record{Issue: fake.Issue("1", epoch), State: StateDone, token: o.newToken()}
		o.records["1"] = next
		if next.generation != 0 {
			t.Fatal("the fixture must have both records on generation zero, or it proves nothing")
		}
		return o, departed, next
	}

	t.Run("a stale anchor does not convert it", func(t *testing.T) {
		o, departed, next := setup(t)

		o.handle(t.Context(), signal{kind: sigClaimAnchor, issue: "1", token: departed, anchor: 11})

		if h, ok := o.held["1"]; ok {
			t.Fatalf("converted on a departed record's read: the held claim carries cycle anchor %d, from a claim that no longer exists", h.cycleAnchor)
		}
		// The same result, keyed to the record that is actually here.
		o.handle(t.Context(), signal{kind: sigClaimAnchor, issue: "1", token: next.token, anchor: 42})
		if h, ok := o.held["1"]; !ok || h.cycleAnchor != 42 {
			t.Fatalf("held = %+v; the current record's own anchor did not convert it", h)
		}
	})

	t.Run("a stale ownership confirmation does not forget it", func(t *testing.T) {
		o, departed, next := setup(t)
		unassigned := issueFixture("1")

		o.handle(t.Context(), signal{kind: sigDoneOwnership, issue: "1", token: departed, refetched: &unassigned})

		if _, ok := o.records["1"]; !ok {
			t.Fatal("a departed record's ownership read dropped the current record, and with it the only thing tracking a live claim")
		}
		if next.convertInFlight {
			t.Error("fixture drift")
		}
	})

	t.Run("a stale write completion does not advance its queue", func(t *testing.T) {
		o, departed, next := setup(t)
		o.release(t.Context(), next, "issue went terminal")
		if !next.owedInFlight {
			t.Fatal("the fixture did not put a write in flight")
		}

		o.handle(t.Context(), signal{kind: sigEffectDone, issue: "1", token: departed, effect: effectRelease})

		if _, ok := o.records["1"]; !ok {
			t.Fatal("a departed record's write completion was read as this record's release landing, and forgot it with the release unsent")
		}
		if !next.owesAnything() || !next.owedInFlight {
			t.Errorf("owed=%v inFlight=%v; a departed record's completion advanced this record's queue",
				next.owesAnything(), next.owedInFlight)
		}
	})
}

// A timer armed by one record must not fire on the next record for the same
// issue.
//
// Timers are the other result that outlives its record: a backoff or
// continuation wait is not work anybody is waiting on, so it does not hold
// `pending`, and nothing stops the record being released while one is armed.
// Generation cannot separate them — it restarts at zero for the replacement,
// which then reaches the same generation by the same route — so the stale
// timer matches exactly and re-dispatches a run that is already under way.
func TestATimerCannotFireOnTheNextRecordForTheIssue(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		// Every run verifies incomplete, so every record arms a continuation
		// timer and reaches generation 1 by the same route.
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictIncomplete}, nil
		}),
	})
	waitFor(t, "the first record's run", func() bool { return h.Runner.StartCount() == 1 })
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the first record's continuation timer was never armed")
	}

	// A human takes the issue: active but not ours alone, so it is released
	// and the record is forgotten — with its timer still armed.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = []string{fake.DefaultPrincipal, "a-human"} })
	h.PollNow()
	h.WaitGone("1")

	// Time passes, so the two records' timers are not due at the same instant.
	h.Clock.Advance(500 * time.Millisecond)

	// The human hands it back, and it is dispatched again.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil; i.Dispatchable = true })
	h.PollNow()
	waitFor(t, "the replacement record's run", func() bool { return h.Runner.StartCount() == 2 })
	if !h.Clock.BlockUntilWaiters(3) {
		t.Fatal("the replacement's own continuation timer was never armed")
	}

	// The departed record's timer comes due. The replacement's, armed half a
	// second later, does not.
	h.Clock.Advance(600 * time.Millisecond)
	time.Sleep(50 * time.Millisecond)

	if got := h.Runner.StartCount(); got != 2 {
		t.Errorf("started %d runs; a timer armed by a released record re-dispatched its successor early", got)
	}
	for _, s := range h.o.Status() {
		if s.Identifier == "1" && s.Attempt != 1 {
			t.Errorf("attempt = %d; the departed record's timer spent one of the replacement's", s.Attempt)
		}
	}

	// The replacement's own timer still works.
	h.Clock.Advance(time.Second)
	waitFor(t, "the replacement's own continuation", func() bool { return h.Runner.StartCount() == 3 })
}

// SPEC §7.4: the terminal event is what a run *reported*, and the stream closing
// says only that the adapter has stopped talking. The adapter closes Events as
// soon as it has emitted the terminal event and reaps the process up to a stop
// grace later — ten seconds by default — so acting on the stream's end puts the
// next attempt into a worktree the previous process may still hold. §9.8 states
// that invariant for the stop path; it holds identically for a run that ended on
// its own, and the continuation track re-dispatches after about a second.
//
// What holds the line here is the domain-quiet question, asked as a Probe while the
// process is alive and as a Stop once Done has closed (#79) — never Done itself,
// which reports neither domain quiet nor permission to touch the workspace.
func TestNothingFollowsARunUntilItsProcessHasExited(t *testing.T) {
	var verifies atomic.Int32
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s"}, {Type: core.EventSucceeded}}
		},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifies.Add(1)
			return VerifyResult{Verdict: VerdictIncomplete}, nil
		}),
		withHook: true,
		// The process outlives its stream, as the real adapter's does.
		linger: true,
	})

	// The agent reported success, so the state has moved — that much is what
	// the terminal event decides.
	h.WaitState("1", StateVerifying)

	// The stream has ended and the agent has claimed success. The process has
	// not been reaped, so nothing that touches the workspace may begin.
	time.Sleep(50 * time.Millisecond)
	if got := verifies.Load(); got != 0 {
		t.Errorf("the §9.7 evidence check ran %d times against a workspace a live process still holds", got)
	}
	if got := h.Hooked.AfterRunCount("1"); got != 0 {
		t.Errorf("the after_run hook fired %d times before the process exited", got)
	}

	// Even once the continuation would be due.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker never armed")
	}
	h.Clock.Advance(2 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if got := h.Runner.StartCount(); got != 1 {
		t.Fatalf("started %d runs; a second agent was launched into a worktree the first process still holds", got)
	}

	// The process is reaped, and everything that was waiting on it proceeds.
	h.Runner.Handles[0].ReleaseProcess()
	waitFor(t, "the evidence check", func() bool { return verifies.Load() == 1 })
	waitFor(t, "the after_run hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the continuation timer was never armed")
	}
	h.Clock.Advance(2 * time.Second)
	waitFor(t, "the continuation", func() bool { return h.Runner.StartCount() == 2 })
}

// Once Events closes, the read-only Probe races the already-near Done edge. A
// natural completion must confirm quiet whether Probe answers first or Done
// overtakes it and requires Stop to make a fresh observation. This is the
// regression from #271: accepting only a previously signalled domain stranded
// an ordinary successful run forever when Done won that race.
func TestNaturalDomainQuietIsSchedulerIndependent(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*harnessOpts, <-chan struct{})
		beforeOpen func(*testing.T, *harness, *fake.Handle)
		wantProbe  bool
		wantStop   bool
	}{
		{
			name: "Probe wins before Done",
			configure: func(opts *harnessOpts, _ <-chan struct{}) {
				opts.holdDone = true
			},
			beforeOpen: func(t *testing.T, h *harness, run *fake.Handle) {
				t.Helper()
				waitFor(t, "the read-only quiet observation", func() bool {
					return h.applied(sigProbed) > 0
				})
				run.ReleaseDone()
			},
			wantProbe: true,
		},
		{
			name: "Done wins the Probe race",
			configure: func(opts *harnessOpts, open <-chan struct{}) {
				opts.probeGate = func() { <-open }
			},
			beforeOpen: func(t *testing.T, h *harness, _ *fake.Handle) {
				t.Helper()
				waitFor(t, "Done to overtake the blocked Probe", func() bool {
					return h.applied(sigHandleDone) > 0
				})
			},
			wantProbe: true,
			wantStop:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			open := make(chan struct{})
			var openOnce sync.Once
			release := func() { openOnce.Do(func() { close(open) }) }
			t.Cleanup(release)

			var verifies atomic.Int32
			opts := harnessOpts{
				issues: []core.Issue{fake.Issue("1", epoch)},
				verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
					verifies.Add(1)
					return VerifyResult{Verdict: VerdictIncomplete}, nil
				}),
			}
			tt.configure(&opts, open)
			h := start(t, opts)
			waitFor(t, "the run to start", func() bool { return h.Runner.StartCount() == 1 })
			run := h.Runner.LastHandle()

			tt.beforeOpen(t, h, run)
			release()
			waitFor(t, "the natural success to reach verification", func() bool {
				return verifies.Load() == 1
			})

			if got := run.ProbeCount() > 0; got != tt.wantProbe {
				t.Errorf("Probe called = %v, want %v", got, tt.wantProbe)
			}
			if got := run.StopCount() > 0; got != tt.wantStop {
				t.Errorf("Stop called = %v, want %v", got, tt.wantStop)
			}

			// A naturally quiet run must not leave the daemon unable to drain.
			h.shutdown()
		})
	}
}

// A live execution-domain member after direct execution ends is not a quiet
// workspace. The outcome remains held until bounded teardown confirms it.
func TestNothingFollowsARunUntilItsDomainIsConfirmedQuiet(t *testing.T) {
	t.Run("a run that reported success", func(t *testing.T) {
		var verifies atomic.Int32
		h := start(t, harnessOpts{
			issues: []core.Issue{fake.Issue("1", epoch)},
			script: func(core.RunSpec, int) []core.Event {
				return []core.Event{{Type: core.EventStarted, SessionID: "s"}, {Type: core.EventSucceeded}}
			},
			verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
				verifies.Add(1)
				return VerifyResult{Verdict: VerdictIncomplete}, nil
			}),
			withHook: true,
			// A member outlived direct execution, so the domain is not confirmed
			// quiet — what the adapter reports for a survivor. Both halves are
			// needed to say that; see harnessOpts.stopUnconfirmed.
			domainMembers:   true,
			stopUnconfirmed: true,
		})
		// The state moves on the terminal event, so this happens either way and
		// what follows is the verdict rather than a race with it.
		h.WaitState("1", StateVerifying)
		run := h.Runner.LastHandle()
		h.PollNow()
		h.waitForDomainQuestion(run)

		// The harness is reaped and the agent has claimed success. Nothing that
		// touches the workspace may begin while a descendant may still be in it.
		if got := verifies.Load(); got != 0 {
			t.Errorf("the §9.7 evidence check ran %d times against a workspace a surviving descendant may hold", got)
		}
		if got := h.Hooked.AfterRunCount("1"); got != 0 {
			t.Errorf("the after_run hook fired %d times before the domain was confirmed quiet", got)
		}
		if n := h.Tracker.ReleaseCount("1"); n != 0 {
			t.Errorf("released %d times; an unconfirmed termination retains the claim (SPEC §9.8)", n)
		}

		// The descendant exits, and the next tick's question is answered. Nothing is
		// in flight to spend this PollNow on: the barrier above acknowledged the
		// stop's answer, and only a tick or the `Done` edge starts another.
		h.Runner.SetStopTermination(core.TerminationConfirmed)
		h.PollNow()
		waitFor(t, "the evidence check", func() bool { return verifies.Load() == 1 })
		waitFor(t, "the after_run hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })

		// And only now can the continuation re-dispatch into that workspace.
		if !h.Clock.BlockUntilWaiters(2) {
			t.Fatal("the continuation timer was never armed")
		}
		h.Clock.Advance(2 * time.Second)
		waitFor(t, "the continuation", func() bool { return h.Runner.StartCount() == 2 })
	})

	t.Run("a run that crashed", func(t *testing.T) {
		h := start(t, harnessOpts{
			issues: []core.Issue{fake.Issue("1", epoch)},
			// A crash as an adapter actually reports one: SPEC §7.4 makes the
			// terminal event ground truth, so a process that exits without
			// declaring one is translated to failed(crashed) at the boundary
			// (claudecode exitEvent) rather than reaching the orchestrator as a
			// stream that just stops. Scripting the silent stream instead would
			// exercise the fail-safe in onEventsClosed and leave the branch that
			// runs in production — EventFailed — free to route immediately.
			script: func(core.RunSpec, int) []core.Event {
				return fake.Fail(core.FailureCrashed)
			},
			withHook: true,
			// As above: a domain member outlives direct execution, and Stop reports
			// what that means (see harnessOpts.stopUnconfirmed).
			domainMembers:   true,
			stopUnconfirmed: true,
		})
		waitFor(t, "the run to start", func() bool { return h.Runner.StartCount() == 1 })
		run := h.Runner.LastHandle()
		h.PollNow()
		h.waitForDomainQuestion(run)

		if got := h.stateOf("1"); got != StateRunning {
			t.Errorf("state = %q; the crash was routed while a descendant may still hold the workspace", got)
		}
		if got := h.Hooked.AfterRunCount("1"); got != 0 {
			t.Errorf("the after_run hook fired %d times before the domain was confirmed quiet", got)
		}

		// As in the sibling above: the barrier already acknowledged the stop's
		// answer, so nothing is in flight for this PollNow to be spent on.
		h.Runner.SetStopTermination(core.TerminationConfirmed)
		h.PollNow()
		h.WaitState("1", StateBackoff)
		waitFor(t, "the after_run hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })
	})

	// The two subtests above take whichever form of the question the scheduler
	// reaches first, and in practice that is almost always the post-`Done` Stop —
	// so the pre-`Done` Probe half of #79 was exercised in about 3% of runs and
	// pinned by none of them. `holdDone` forces it: `Done` cannot close until the
	// test says so, and §7.5 permits only a Probe before it.
	//
	// A crash rather than a success, because the negative has to be observable at
	// the instant the barrier releases. A routed crash transitions inside the
	// handler that routed it, so `stateOf` answers synchronously; a routed success
	// enqueues a verifier and its counter lags the decision by a scheduling
	// quantum, which is what made the same mutant escape 1 run in 20 when measured
	// through it (#106's review).
	t.Run("a run whose domain is observed before Done", func(t *testing.T) {
		h := start(t, harnessOpts{
			issues:          []core.Issue{fake.Issue("1", epoch)},
			script:          func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureCrashed) },
			withHook:        true,
			domainMembers:   true,
			stopUnconfirmed: true,
			holdDone:        true,
		})
		waitFor(t, "the run to start", func() bool { return h.Runner.StartCount() == 1 })
		run := h.Runner.LastHandle()

		// The observation, and its answer handled. Not waitForDomainQuestion: that
		// helper waits for the Stop, which cannot happen yet — here the point is
		// precisely what the loop does with a *probe* that came back unconfirmed.
		waitFor(t, "the pre-Done observation's answer to be handled", func() bool {
			return h.applied(sigProbed) > 0
		})
		// §7.5, the rule this staging exists to pin: before `Done` the question is
		// only ever asked with Probe. A Stop here could disturb execution that is
		// still flushing §7.2's transcript.
		if got := run.StopCount(); got != 0 {
			t.Errorf("stopped %d times before Done; the pre-Done question is Probe's alone (#79)", got)
		}
		if got := h.stateOf("1"); got != StateRunning {
			t.Errorf("state = %q; an unconfirmed observation routed the crash", got)
		}
		if got := h.Hooked.AfterRunCount("1"); got != 0 {
			t.Errorf("the after_run hook fired %d times on an unconfirmed observation", got)
		}

		// `Done` closes, so the question becomes a Stop — and an unconfirmed one
		// holds the outcome exactly as the observation did.
		run.ReleaseDone()
		h.waitStopApplied(1)
		if got := h.stateOf("1"); got != StateRunning {
			t.Errorf("state = %q; an unconfirmed stop routed the crash", got)
		}
		if got := h.Hooked.AfterRunCount("1"); got != 0 {
			t.Errorf("the after_run hook fired %d times on an unconfirmed stop", got)
		}

		// And only a confirmed answer releases it.
		h.Runner.SetStopTermination(core.TerminationConfirmed)
		h.PollNow()
		h.WaitState("1", StateBackoff)
		waitFor(t, "the after_run hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })
	})
}

// The two askers of §7.5's question share one handle, and reconciliation can
// answer a different question about the same record while the first is out: an
// issue that goes terminal takes the record over by stopping the very process
// the quiescence probe is asking about. The probe's answer arrives afterwards,
// and it must not restart a pipeline the record has already left — the §9.7
// evidence check above all, which reads a workspace this exit disposes.
// Driven with a group that outlives its process, so the operation in flight is
// the post-Done stop this test's gate is on. Before #79 an ordinary run reached
// that gate too; now it is settled by a probe first, and which side of Done the
// probe lands on is a race — so gating the stop on an ordinary run staged the
// interleaving only sometimes (about one run in ninety failed).
func TestAnExitDecidedDuringTheQuiescenceStopWins(t *testing.T) {
	var verifies atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	openGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(openGate)

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifies.Add(1)
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
		// Teardown stands in the gate for as long as the test needs, which is
		// the bounded window Stop opens in production.
		stopGate: func() { <-release },
		// And a domain member outlives direct execution, making teardown the
		// operation in flight rather than an observation.
		domainMembers: true,
	})

	// The agent claims success and its process is reaped, so the outcome is
	// held and the probe goes out.
	h.WaitState("1", StateVerifying)
	waitFor(t, "the post-Done cleanup", func() bool { return h.Runner.LastHandle().StopCount() == 1 })
	if got := verifies.Load(); got != 0 {
		t.Fatalf("the evidence check ran %d times before the workspace was confirmed quiet", got)
	}

	// The issue is closed while the question is out. Reconciliation stops the
	// run and exits — and asks no second ladder, because one is already walking.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.PollNow()
	if got := h.Runner.LastHandle().StopCount(); got != 1 {
		t.Errorf("Stop called %d times; a second teardown was started over the first", got)
	}

	openGate()
	h.WaitGone("1")

	if got := verifies.Load(); got != 0 {
		t.Errorf("the §9.7 evidence check ran %d times on an issue already on its way out", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want 1 for an issue that went terminal", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want the workspace disposed for a terminal issue", got)
	}
}

// The orchestrator's own fail-safe for a stream that ends without the terminal
// event §7.4 requires. No conforming adapter produces one — claudecode
// translates a silent exit to failed(crashed) at the boundary — so this is
// asserted at the unit rather than through a fixture that would be proving
// something about a runner that cannot exist. What it pins is that the
// fail-safe classifies *and holds*, exactly as the reported crash does.
func TestAStreamEndingWithoutATerminalEventIsHeldAsACrash(t *testing.T) {
	runner := fake.NewRunner()
	runner.SetHangAfterScript(true)
	runner.SetStopTermination(core.TerminationUnconfirmed)
	handle, err := runner.Start(context.Background(), core.RunSpec{})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	o := idleOrchestrator(t, fake.NewTracker())
	r := &Record{
		Issue: issueFixture("1"), State: StateRunning, Attempt: 1,
		Definition: o.definition(), handle: handle,
		Workspace: core.Workspace{WorkspacePaths: core.WorkspacePaths{Path: t.TempDir()}, Branch: "ben/1"},
	}
	o.records["1"] = r

	o.onEventsClosed(context.Background(), r, signal{kind: sigEventsClosed, issue: "1"})

	if r.outcome == nil || r.outcome.event.Reason != core.FailureCrashed {
		t.Fatalf("outcome = %+v, want a held failed(crashed)", r.outcome)
	}
	if r.State != StateRunning {
		t.Errorf("state = %q; the crash was routed before the execution domain was confirmed quiet", r.State)
	}
	// Asked, and asked with the operation the phase permits: before Done the
	// process may still be flushing its transcript, so the question is a
	// read-only Probe rather than bounded teardown (#79).
	if !r.probeInFlight {
		t.Error("nothing asked whether the execution domain was gone")
	}
	if r.stopInFlight {
		t.Error("the domain was torn down before Done; a probe is the only permissible question there")
	}
}

// The other half of the routing rule, at the unit: within one record, a
// result scoped to an attempt that has been superseded must not act.
//
// Asserted against the predicate rather than through a scenario. The routing
// rule is a pure function of (record, signal), and the alternative — driving
// a superseded attempt's result end to end — needs a run whose stream keeps
// producing events after its terminal one, which the §7.4 contract forbids.
// A fixture that did it anyway would be proving something about a runner that
// cannot exist.
func TestDeliverableSeparatesTenureFromAttempt(t *testing.T) {
	const tenure, attempt = 7, 3
	r := &Record{token: tenure, generation: attempt}

	cases := []struct {
		name string
		s    signal
		want bool
	}{
		{
			name: "an attempt-scoped result from this attempt",
			s:    signal{kind: sigTimer, token: tenure, generation: attempt},
			want: true,
		},
		{
			name: "an attempt-scoped result from a superseded attempt",
			s:    signal{kind: sigTimer, token: tenure, generation: attempt - 1},
		},
		{
			name: "an attempt-scoped result from a previous tenure that reached the same attempt",
			s:    signal{kind: sigTimer, token: tenure - 1, generation: attempt},
		},
		{
			name: "a record-scoped write completion, whatever attempt queued it",
			s:    signal{kind: sigEffectDone, token: tenure, generation: attempt - 2},
			want: true,
		},
		{
			name: "a record-scoped write completion from a previous tenure",
			s:    signal{kind: sigEffectDone, token: tenure - 1, generation: attempt},
		},
		{
			name: "a conversion read from this record",
			s:    signal{kind: sigClaimAnchor, token: tenure},
			want: true,
		},
		{
			name: "a conversion read from a previous tenure",
			s:    signal{kind: sigClaimAnchor, token: tenure - 1},
		},
		{
			name: "an ownership confirmation from a previous tenure",
			s:    signal{kind: sigDoneOwnership, token: tenure - 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deliverable(r, tc.s); got != tc.want {
				t.Errorf("deliverable = %v, want %v", got, tc.want)
			}
		})
	}
}

// The cleanup Done permits is bounded teardown, and the answer worth having is
// a confirmed one. Discard is less patient than interrupt, which trades a
// confirmed verdict for speed in the one place BEN is not in a hurry. (The
// question *before* Done is a read-only Probe: #79.) An unconfirmed answer costs
// a retained claim and another tick (§9.8); waiting costs nothing in the
// ordinary case, where Stop observes an already-quiet domain.
//
// The transcript argument that first chose this mode still holds, and #79 gave it
// teeth one layer down: a stop is only reached *after* Done, where the record is
// already written and closed, and everything before that edge is a read-only
// probe. The *choice* still has to be pinned somewhere, though:
// nothing else in this package fails when it flips back, and the reason it is
// right has changed twice now.
// Driven through a domain member that outlives direct execution, because that
// is now the case where Stop is the operation at all: after #79 an ordinary run
// is settled by the read-only probe, and whether a stop follows depends on which side of
// Done the probe landed — so asserting a stop there would be asserting on a race
// (it flaked about one run in ninety before this was pinned to the right path).
func TestThePostDoneCleanupInterruptsRatherThanDiscards(t *testing.T) {
	h := start(t, harnessOpts{issues: []core.Issue{fake.Issue("1", epoch)}, domainMembers: true})
	h.WaitState("1", StateDone)

	handle := h.Runner.LastHandle()
	if handle == nil {
		t.Fatal("no run was started")
	}
	if handle.ProbeCount() == 0 {
		t.Error("the domain was torn down without being observed first; before Done a probe is the only permissible question")
	}
	got := handle.Stops()
	if len(got) != 1 {
		t.Fatalf("stops = %v, want exactly one — the cleanup Done permits", got)
	}
	if got[0] != core.StopInterrupt {
		t.Errorf("the quiescence probe asked for %s, want %s: discard is the impatient mode, and an unconfirmed group costs a retained claim (SPEC §7.5, §9.8)",
			stopModeName(got[0]), stopModeName(core.StopInterrupt))
	}
}

// stopModeName names a StopMode for a failure message. core.StopMode is a bare
// int enum, and "asked for 1, want 0" tells a reader nothing about what broke.
func stopModeName(m core.StopMode) string {
	switch m {
	case core.StopInterrupt:
		return "StopInterrupt"
	case core.StopDiscard:
		return "StopDiscard"
	}
	return fmt.Sprintf("StopMode(%d)", int(m))
}

// A history read that shows no assignment answers nothing (SPEC §9.10), and
// the record must not record an answer nobody gave. Re-baselining the revision
// here would retire the question: the next sweep sees a revision it has
// already accounted for and never asks again, so a close that arrives while
// the log is lagging is never noticed and the claim stands until a restart.
func TestAnAnchorlessHeldHistoryDoesNotRebaselineTheRevision(t *testing.T) {
	h := doneHarness(t, 1)

	// The log no longer carries the assignment that established the claim —
	// a lagging or truncated read — while the issue is still ours, which is
	// what puts the sweep on the history path at all.
	h.Tracker.SetHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventLabeled, Subject: "ben-queue", At: epoch,
	})
	base := h.Tracker.HistoryReads()
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "closed-or-not-we-cannot-say" })

	// Two sweeps over the same unanswered question must ask it twice.
	h.Tick()
	waitFor(t, "the first history read", func() bool { return h.Tracker.HistoryReads() == base+1 })
	h.Tick()
	waitFor(t, "the question to be asked again", func() bool { return h.Tracker.HistoryReads() == base+2 })

	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times on a log that showed no live assignment; absence of a fact is not evidence", n)
	}
	if got := h.o.HeldCount(); got != 1 {
		t.Errorf("held count = %d, want the claim retained", got)
	}
}

// SPEC §9.8: at most one operation out per held record. The sweep runs every
// tick and human review latency is unbounded, so a record that starts a second
// read while the first is still out turns the one-list-read design back into a
// per-record cost — paid every tick, for as long as the tracker is slow.
func TestASweepStartsNoSecondOperationWhileOneIsOut(t *testing.T) {
	release := make(chan struct{})
	var gated sync.Once
	h := doneHarness(t, 1)
	h.Tracker.SetHistoryGate(func() { gated.Do(func() { <-release }) })
	t.Cleanup(func() { close(release) })

	base := h.Tracker.HistoryReads()
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "someone-commented" })

	// The read is counted before the gate, so this waits for one that is
	// standing in it.
	h.Tick()
	waitFor(t, "a history read to be in flight", func() bool { return h.Tracker.HistoryReads() == base+1 })

	// Further sweeps, with that read still unanswered. The revision still
	// differs, so nothing but the in-flight guard stops each one starting
	// another read.
	for range 3 {
		h.Tick()
	}
	if got := h.Tracker.HistoryReads() - base; got != 1 {
		t.Errorf("%d history reads for one held record with a read already out, want 1: the sweep's per-record cost is unbounded",
			got)
	}
}

// idleOrchestrator builds an orchestrator that is not running, for the paths
// whose state the loop goroutine would otherwise own.
func idleOrchestrator(t *testing.T, tracker *fake.Tracker) *Orchestrator {
	t.Helper()
	o, _ := idleWithSource(t, tracker)
	return o
}

// idleWithSource is idleOrchestrator plus the cell its configuration lives in, for
// a test that publishes a reload rather than only reading one.
func idleWithSource(t *testing.T, tracker *fake.Tracker) (*Orchestrator, *testSource) {
	t.Helper()
	return idleWithAdapters(t, tracker, fake.NewWorkspaces(), nil)
}

// idleWithAdapters is idleWithSource with the workspace provider and the §9.10 run
// prober named, for a test whose subject is what Recover leaves behind: both are
// fixtures the caller has to seed before the pass runs.
func idleWithAdapters(t *testing.T, tracker *fake.Tracker, workspaces Workspaces,
	runGone func(core.RunEvidence) (bool, error)) (*Orchestrator, *testSource) {
	t.Helper()
	def := definition(t, "3", "")
	src := newTestSource(def, &Bundle{
		Definition:     def,
		Tracker:        tracker,
		Workspaces:     workspaces,
		Runner:         fake.NewRunner(),
		Verifier:       alwaysPublished,
		ClaimPrincipal: fake.DefaultPrincipal,
	})
	o, err := New(Config{Runtime: src, Log: discardLogger(), RunGone: runGone})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return o, src
}

func containsMilestone(got []core.Milestone, want core.Milestone) bool {
	for _, m := range got {
		if m == want {
			return true
		}
	}
	return false
}

// The two questions, in the order the phases permit them (#79; SPEC §7.5).
//
// While the process is still alive after its stream closed, the orchestrator may
// only *observe*: a signal there would land on a harness still flushing its
// transcript, so the outcome is held on repeated probes and nothing is signalled
// at all. Once Done closes, direct execution is over — anything left in the
// domain has outlived it — so the question becomes a stop, and that is what
// releases the outcome.
func TestTheDomainIsObservedBeforeDoneAndTornDownAfterIt(t *testing.T) {
	var verifies atomic.Int32
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s"}, {Type: core.EventSucceeded}}
		},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifies.Add(1)
			return VerifyResult{Verdict: VerdictIncomplete}, nil
		}),
		withHook: true,
		// The process outlives its stream, as a real harness's does while it
		// finishes writing the transcript.
		linger: true,
	})
	h.WaitState("1", StateVerifying)
	handle := h.Runner.Handles[0]

	// Phase 1: observed, not touched. The probe answers unconfirmed because the
	// process is still there, and the outcome stays held.
	waitFor(t, "the first observation", func() bool { return handle.ProbeCount() > 0 })
	if got := handle.StopCount(); got != 0 {
		t.Fatalf("stopped %d times before Done; a signal there lands on a process still flushing its transcript", got)
	}
	if got := verifies.Load(); got != 0 {
		t.Errorf("the §9.7 evidence check ran %d times against a workspace a live process still holds", got)
	}
	if got := h.Hooked.AfterRunCount("1"); got != 0 {
		t.Errorf("the after_run hook fired %d times before the domain was confirmed quiet", got)
	}

	// And it keeps asking, which is the diagnosable half: the case this replaced
	// went silent instead.
	before := handle.ProbeCount()
	h.PollNow()
	waitFor(t, "the observation to be re-driven", func() bool { return handle.ProbeCount() > before })
	if got := handle.StopCount(); got != 0 {
		t.Fatalf("stopped %d times; re-probing must not escalate to signals before Done", got)
	}

	// Phase 2: the process is reaped, so the question becomes a stop — and the
	// stop is what lets everything waiting proceed.
	handle.ReleaseProcess()
	waitFor(t, "the evidence check", func() bool { return verifies.Load() == 1 })
	if got := handle.StopCount(); got == 0 {
		t.Error("nothing cleaned the domain after Done; an outliving member would be left running")
	}
	waitFor(t, "the after_run hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })
}

// A domain member that outlives direct execution is exactly what the post-Done
// stop exists for: Probe cannot clear it, and only Stop does (#79).
func TestADomainMemberThatOutlivesDirectExecutionIsTornDownByStop(t *testing.T) {
	var verifies atomic.Int32
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s"}, {Type: core.EventSucceeded}}
		},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifies.Add(1)
			return VerifyResult{Verdict: VerdictIncomplete}, nil
		}),
		domainMembers: true,
	})
	h.WaitState("1", StateVerifying)
	handle := h.Runner.Handles[0]

	// Done closes — direct execution is over — but the domain is not quiet, so the probe
	// keeps answering unconfirmed and the stop is what settles it.
	waitFor(t, "the domain to be cleaned", func() bool { return handle.StopCount() > 0 })
	waitFor(t, "the evidence check", func() bool { return verifies.Load() == 1 })
	if got := handle.ProbeCount(); got == 0 {
		t.Error("the domain was torn down without ever being observed first")
	}
}

// The same ordering rule, on the operation that is in flight on an ordinary run
// after #79: an exit decided while an *observation* is out wins, and it does not
// wait for that observation's answer — the stop it needs starts immediately, and
// the probe's late verdict cannot stand in for the stop's (see onProbed).
func TestAnExitDecidedDuringAnObservationWins(t *testing.T) {
	var verifies atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	openGate := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(openGate)

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifies.Add(1)
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
		probeGate: func() { <-release },
	})

	h.WaitState("1", StateVerifying)
	waitFor(t, "the observation", func() bool { return h.Runner.LastHandle().ProbeCount() == 1 })
	if got := verifies.Load(); got != 0 {
		t.Fatalf("the evidence check ran %d times before the workspace was confirmed quiet", got)
	}

	// The issue goes terminal while the observation is out. The exit stops the
	// run rather than waiting for an answer that is now moot.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.PollNow()
	waitFor(t, "the exit's stop", func() bool { return h.Runner.LastHandle().StopCount() == 1 })

	openGate()
	h.WaitGone("1")

	if got := verifies.Load(); got != 0 {
		t.Errorf("the §9.7 evidence check ran %d times on an issue already on its way out", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want 1 for an issue that went terminal", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want the workspace disposed for a terminal issue", got)
	}
}

// An exit decided while an observation is out must not be overridden by that
// observation's answer, even a *confirmed* one (#79 review).
//
// The confirmed case is the dangerous one and the reason for the guard: a probe
// that comes back "the domain is quiet" is telling the truth, and routing on it
// would be correct for a record that was staying. This one is leaving —
// reconciliation stopped it and owns the exit — so routing would run the §9.7
// evidence check and the after-run hook against a workspace the exit is about to
// dispose, and would clear the handle the exit's own stop still needs.
//
// Both gates are held so the interleaving is staged rather than raced: the
// observation is standing in one while the exit's ladder stands in the other.
func TestAConfirmedObservationDoesNotOverrideAPendingExit(t *testing.T) {
	var verifies atomic.Int32
	releaseProbe, releaseStop := make(chan struct{}), make(chan struct{})
	var probeOnce, stopOnce sync.Once
	openProbe := func() { probeOnce.Do(func() { close(releaseProbe) }) }
	openStop := func() { stopOnce.Do(func() { close(releaseStop) }) }
	t.Cleanup(openProbe)
	t.Cleanup(openStop)

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifies.Add(1)
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
		probeGate: func() { <-releaseProbe },
		stopGate:  func() { <-releaseStop },
		// The group goes quiet while the record is still being written, so the
		// observation can come back *confirmed* with the phase edge still open —
		// the only arrangement in which this hazard is reachable, since after Done
		// confirmQuiet's own guard already refuses an exiting record.
		holdDone: true,
	})

	h.WaitState("1", StateVerifying)
	handle := h.Runner.LastHandle()
	waitFor(t, "the observation", func() bool { return handle.ProbeCount() == 1 })

	// The issue goes terminal while the observation is out; the exit's teardown is
	// now standing in its own gate.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.PollNow()
	waitFor(t, "the exit's stop", func() bool { return handle.StopCount() == 1 })

	// The observation answers confirmed — the domain became quiet — and it must
	// change nothing. Deliberately *not* asserted against a
	// sleep here: "the loop has consumed sigProbed" is not observable from this
	// side, and under load the stop can finish first and take the record away,
	// which would let the buggy implementation pass. The assertions that follow
	// WaitGone are barrier-backed and catch it either way, and
	// TestOnProbedRefusesToRouteWhileAnExitIsPending is the deterministic barrier
	// for the handler itself.
	openProbe()

	// The exit completes on its own terms once its teardown is let out.
	openStop()
	h.WaitGone("1")
	if got := verifies.Load(); got != 0 {
		t.Errorf("the evidence check ran %d times for an issue that went terminal", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want 1", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want the workspace disposed once for a terminal issue", got)
	}
}

// The handler itself, driven synchronously — no scheduler, no sleep, no chance
// for the record to be taken away first (#79 review round 3).
//
// The end-to-end test around this rule can only assert after a barrier that lets
// the exit finish, so it cannot pin the instant that matters: an observation
// answering *confirmed* while the exit's stop is still out. This drives that
// instant directly, and it is what a mutation on the guard has to fail.
//
// Routing is detected through r.pending rather than a verifier call, because
// beginVerify increments it before it spawns anything — so the assertion needs
// no goroutine to have run.
func TestOnProbedRefusesToRouteWhileAnExitIsPending(t *testing.T) {
	newRecord := func(t *testing.T, o *Orchestrator) *Record {
		t.Helper()
		runner := fake.NewRunner()
		handle, err := runner.Start(context.Background(), core.RunSpec{})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		r := &Record{
			Issue: issueFixture("1"), State: StateVerifying, Attempt: 1,
			Definition: o.definition(), handle: handle,
			Workspace:     core.Workspace{WorkspacePaths: core.WorkspacePaths{Path: t.TempDir()}, Branch: "ben/1"},
			eventsClosed:  true,
			probeInFlight: true,
			outcome:       &runOutcome{event: core.Event{Type: core.EventSucceeded}},
		}
		o.records["1"] = r
		return r
	}
	confirmed := signal{kind: sigProbed, issue: "1", probe: core.TerminationConfirmed}

	// Between retries: the exit's last stop came back unconfirmed, so nothing is
	// in flight and the next tick owes another (retryPendingExits). This is the
	// state that isolates the load-bearing half of the guard — with a ladder also
	// out, `stopInFlight` would carry the case on its own and deleting
	// `r.exiting()` would still pass.
	t.Run("an exit is pending between retries", func(t *testing.T) {
		o := idleOrchestrator(t, fake.NewTracker())
		r := newRecord(t, o)
		r.stopping, r.stopInFlight = true, false

		o.onProbed(context.Background(), r, confirmed)

		if r.domainQuiet {
			t.Error("a confirmed observation set domainQuiet while the exit's stop was still out")
		}
		if r.outcome == nil {
			t.Error("the held outcome was routed on an observation the exit outranks")
		}
		if r.handle == nil {
			t.Error("the handle was cleared while a stop was in flight; the exit could not retry it")
		}
		if r.pending != 0 {
			t.Errorf("pending = %d; the §9.7 evidence check was started against a workspace the exit is about to dispose", r.pending)
		}
	})

	// And with a ladder actually walking, which is what the end-to-end test stages.
	t.Run("an exit's ladder is out", func(t *testing.T) {
		o := idleOrchestrator(t, fake.NewTracker())
		r := newRecord(t, o)
		r.stopping, r.stopInFlight = true, true

		o.onProbed(context.Background(), r, confirmed)

		if r.domainQuiet || r.outcome == nil || r.handle == nil || r.pending != 0 {
			t.Errorf("a confirmed observation acted while the exit's ladder was out: domainQuiet=%v outcome=%v handle=%v pending=%d",
				r.domainQuiet, r.outcome != nil, r.handle != nil, r.pending)
		}
	})

	// The positive control: without an exit, the same answer routes — so the tests
	// above cannot pass by the handler having become a no-op.
	//
	// Routing takes two hops now. The prior-attempt account is read between
	// quiescence and the route (#61), so a confirmed observation starts *that*, and
	// the evidence check follows when it reports.
	t.Run("nothing is leaving", func(t *testing.T) {
		o := idleOrchestrator(t, fake.NewTracker())
		r := newRecord(t, o)

		o.onProbed(context.Background(), r, confirmed)

		if !r.domainQuiet {
			t.Error("a confirmed observation did not record the domain as quiet")
		}
		if !r.summarizing || r.pending != 1 {
			t.Errorf("summarizing=%v pending=%d, want the prior-attempt account read started",
				r.summarizing, r.pending)
		}
		if r.outcome == nil {
			t.Error("the outcome routed before the account of the attempt had been read")
		}

		o.onSummarized(context.Background(), r, signal{kind: sigSummarized, issue: "1"})

		if r.outcome != nil {
			t.Error("the outcome was not routed once the account had been read")
		}
		if r.pending != 1 {
			t.Errorf("pending = %d, want the evidence check started", r.pending)
		}
	})
}

// SPEC §9.8: the anchor exists to date a *close event* against the current
// claim cycle. A terminal issue is not an event — it is the state now, and now
// needs no dating. Retaining the claim while the history read is retried defers
// a verdict the very next sweep would reach from a list response saying exactly
// the same thing (classifyHeld), and a claim held on an issue nobody will
// reopen is held forever.
func TestATerminalIssueReleasesEvenWithoutAClaimAnchor(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	// The log cannot say which event established the claim, and the issue has
	// gone terminal — the PR merged — while we were asking.
	h.Tracker.SetHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventLabeled, Subject: "ben-queue", ID: 9, At: epoch,
	})
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	close(gate)

	waitFor(t, "the release on the terminal fact", func() bool {
		return h.Tracker.ReleaseCount("1") == 1
	})
	h.WaitGone("1")
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; a record was built for a claim that was just released", got)
	}
}
