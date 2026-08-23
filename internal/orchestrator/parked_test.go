package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// parkedHarness runs n issues to `needs-review` and leaves them there. The
// verifier contradicts every claim, which is the park that costs no human
// gesture to arrange.
func parkedHarness(t *testing.T, n int) *harness {
	t.Helper()
	var issues []core.Issue
	for i := 1; i <= n; i++ {
		issues = append(issues, fake.Issue(fmt.Sprint(i), epoch.Add(time.Duration(i)*time.Minute)))
	}
	h := start(t, harnessOpts{
		concurrency: fmt.Sprint(n + 1),
		issues:      issues,
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no commits"}, nil
		}),
	})
	for i := 1; i <= n; i++ {
		h.WaitState(fmt.Sprint(i), StateNeedsReview)
	}
	// The park's own projection, so the records are eligible for the label rule
	// (parkedWant.labelsSettled) rather than waiting on a write.
	h.WaitEffects(n)
	return h
}

// SPEC §9.8 as amended (#36): the per-tick cost of refreshing parked records
// does not scale with how many there are. The parked set grows with human review
// latency — the same quantity that ruled out a `Get` per held claim — and one
// conditional list read already carries every fact the parked rules need.
func TestReconcileCostDoesNotScaleWithParkedRecords(t *testing.T) {
	cost := func(parked int) (sweeps, gets, histories int) {
		h := parkedHarness(t, parked)
		baseSweep, baseGet, baseHist := h.Tracker.HeldReads(), h.Tracker.GetReads(), h.Tracker.HistoryReads()
		for range 3 {
			h.Tick()
		}
		return h.Tracker.HeldReads() - baseSweep,
			h.Tracker.GetReads() - baseGet,
			h.Tracker.HistoryReads() - baseHist
	}

	one, oneGet, oneHist := cost(1)
	many, manyGet, manyHist := cost(6)

	if many != one {
		t.Errorf("6 parked records cost %d sweep reads, 1 costs %d; the sweep scales with the parked set", many, one)
	}
	// Comparing N against 1 is blind to a constant multiplier: a duplicated read
	// costs twice at every size and the ratio still matches. Pin the absolute
	// count too — three ticks, three sweep reads.
	if one != 3 {
		t.Errorf("3 ticks cost %d sweep reads, want exactly 3 — one per tick", one)
	}
	// The mechanism, not just the totals. A Get per parked record is what this
	// ticket removes: those reads run sequentially in one worker, so they also put
	// N round trips ahead of every running record's refresh in the same pass.
	if oneGet != 0 || manyGet != 0 {
		t.Errorf("per-issue reads over idle ticks: %d for 1 parked record, %d for 6 — the refresh is one list read, not a Get per record",
			oneGet, manyGet)
	}
	if oneHist != 0 || manyHist != 0 {
		t.Errorf("history reads over idle ticks: %d and %d; the parked rules read state and labels, never the log", oneHist, manyHist)
	}
}

// The absence path is the only other request the parked rules can spend, so the
// bound has to hold there too — and it is where the first version of this change
// failed: every missing record launched its own concurrent Get, so a human
// unassigning BEN from six parked issues cost six reads on the tick that noticed.
func TestParkedAbsenceConfirmationsAreBoundedPerTick(t *testing.T) {
	const parked = 6
	h := parkedHarness(t, parked)
	// Every one of them absent from the sweep read at once: one gesture, six
	// records.
	for i := 1; i <= parked; i++ {
		h.Tracker.Mutate(fmt.Sprint(i), func(iss *core.Issue) { iss.Assignees = nil })
	}
	base := h.Tracker.GetReads()

	before := h.applied(sigReconciled)
	h.Tick()
	waitFor(t, "the tick's reconciliation", func() bool { return h.applied(sigReconciled) > before })
	h.settle(len(h.o.Transitions.Entries()))

	if got := h.Tracker.GetReads() - base; got != parkedConfirmationsPerTick {
		t.Errorf("%d parked records absent at once cost %d confirming read(s) in one tick, want %d",
			parked, got, parkedConfirmationsPerTick)
	}

	// Deferred, not dropped: later ticks take the rest, so the whole set still
	// resolves without any of it costing one tick's worth of requests.
	for range parked + 2 {
		h.Tick()
	}
	waitFor(t, "every absent record to be resolved", func() bool {
		for i := 1; i <= parked; i++ {
			if h.stateOf(fmt.Sprint(i)) != "" {
				return false
			}
		}
		return true
	})
	if got := h.Tracker.GetReads() - base; got > parked {
		t.Errorf("confirming reads = %d for %d records, want at most one apiece", got, parked)
	}
}

// The bound is one confirmation per tick, so **fairness is part of the bound**:
// with a single slot, whoever takes it first every tick starves everyone else.
//
// The offer order must not be what supplies that fairness. Map iteration order is
// unspecified rather than guaranteed random — the runtime happens to randomize it,
// nothing in the language says so, and this function takes the order from its
// caller anyway. So the fixture hands it a **stable** order, which is legal, and
// the record at the head has an absence that never confirms: exactly the shape that
// held the slot forever before the rotation existed.
//
// White-box because the property is about which record is *offered* the slot, and
// the loop only exposes what came of it.
func TestParkedAbsenceConfirmationsRotateUnderAStableOfferOrder(t *testing.T) {
	const parked = 4
	o := idleOrchestrator(t, fake.NewTracker())
	var wants []parkedWant
	for i := 1; i <= parked; i++ {
		id := fmt.Sprint(i)
		o.records[id] = &Record{Issue: issueFixture(id), State: StateNeedsReview, token: o.newToken()}
		wants = append(wants, parkedWant{id: id, labelsSettled: true})
	}
	// Absent from the response, all of them: one human unassigning a backlog.
	empty := sweepResult{read: true}

	offered := map[string]int{}
	for range parked {
		o.sweepParked(t.Context(), wants, empty, o.configNow())
		for id, r := range o.records {
			if !r.absenceInFlight {
				continue
			}
			offered[id]++
			// The confirmation comes back a failure, which is what makes this the
			// starvation case: the record is eligible again on the very next tick.
			o.onParkedConfirmed(t.Context(), r, signal{
				kind: sigParkedConfirmed, issue: id, token: r.token,
				err: errors.New("502 from the tracker"),
			})
		}
	}

	for i := 1; i <= parked; i++ {
		if got := offered[fmt.Sprint(i)]; got != 1 {
			t.Errorf("issue %d was offered the confirmation slot %d times over %d ticks, want exactly 1: %v",
				i, got, parked, offered)
		}
	}
}

// The same property end-to-end, and the reason it matters: a record whose absence
// cannot be confirmed must not stop the others from resolving. This one is a
// demonstration rather than the anchor — the rotation test above is what fails
// deterministically when the cursor goes away — because a randomized offer order
// gives the others a turn often enough to pass it by luck.
func TestAnUnconfirmableAbsenceDoesNotStarveTheOthers(t *testing.T) {
	const parked = 4
	h := parkedHarness(t, parked)
	for i := 1; i <= parked; i++ {
		h.Tracker.Mutate(fmt.Sprint(i), func(iss *core.Issue) { iss.Assignees = nil })
	}
	// Issue 1's confirmation never lands; the rest are ordinary unassignments.
	h.Tracker.FailGetFor = func(identifier string) error {
		if identifier == "1" {
			return errors.New("502 from the tracker")
		}
		return nil
	}

	for range parked * 3 {
		h.Tick()
	}

	waitFor(t, "every confirmable absence to resolve", func() bool {
		for i := 2; i <= parked; i++ {
			if h.stateOf(fmt.Sprint(i)) != "" {
				return false
			}
		}
		return true
	})
	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Errorf("state = %q for the record whose absence cannot be confirmed, want it held at %q",
			got, StateNeedsReview)
	}
}

// A closed issue disposes its workspace, and an unassigned one owes no release.
// They are independent facts about one read, and the first version of this change
// let the second hide the first: a human who resolves an issue by closing it and
// dropping the assignment left a worktree behind, because assignment was checked
// first and answered with keep=true.
func TestAClosedAndUnassignedParkedIssueStillDisposesItsWorkspace(t *testing.T) {
	h := parkedHarness(t, 1)
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.State = "closed"
		i.Assignees = nil
	})

	h.Tick()
	h.WaitGone("1")

	got := h.Workspaces.Disposals("1")
	if len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want exactly one with keep=false: the issue is terminal", got)
	}
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s); the assignment that was the claim is already gone", n)
	}
}

// The guard asks whether the *state label* has landed, not whether the record owes
// anything at all. A milestone comment is a different write on a different §8.5
// allowance, and it can fail indefinitely — so gating on the whole owed queue
// meant one wedged comment suppressed every human re-queue for the life of the
// record, with `ben:needs-review` standing on the issue the whole time.
func TestAWedgedMilestoneCommentDoesNotSuppressTheUnpark(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no commits"}, nil
		}),
		beforeStart: func(tr *fake.Tracker) {
			tr.FailComment = func(_ string, m core.Milestone) error {
				if m == core.MilestoneNeedsReview {
					return errors.New("502 from the tracker")
				}
				return nil
			}
		},
	})
	h.WaitState("1", StateNeedsReview)
	// The projection landed; only the comment behind it is stuck.
	waitFor(t, "the park's state label", func() bool {
		return h.Tracker.Label("1") == core.StateLabelNeedsReview
	})

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = []string{"ben-queue"} })
	h.Tick()

	h.WaitState("1", StateBackoff)
}

// The read is made for the parked records themselves, not only as a side effect
// of a held claim being retained: a daemon whose only tracked work is parked has
// an empty held set, and before #36 made no sweep read at all.
func TestParkedRecordsAloneStillEarnTheSweepRead(t *testing.T) {
	h := parkedHarness(t, 2)
	if got := h.o.HeldCount(); got != 0 {
		t.Fatalf("held count = %d, want the fixture to have parked records and no held claims", got)
	}
	base := h.Tracker.HeldReads()

	h.Tick()

	if got := h.Tracker.HeldReads() - base; got != 1 {
		t.Errorf("sweep reads = %d for a tick with parked records and no held claims, want 1", got)
	}
}

// The unpark, decided from the sweep response and nothing else.
func TestParkedRecordUnparksFromTheSweepResponse(t *testing.T) {
	h := parkedHarness(t, 1)
	baseGet := h.Tracker.GetReads()

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = []string{"ben-queue"} })
	h.Tick()

	h.WaitState("1", StateBackoff)
	if got := h.Tracker.GetReads() - baseGet; got != 0 {
		t.Errorf("spent %d per-issue read(s) noticing a label a human removed; the list response carries labels", got)
	}
}

// The terminal rule, likewise — and it has to stay ahead of the unpark rule, or
// a closed parked issue keeps its claim and workspace forever.
func TestClosedParkedIssueIsFinishedFromTheSweepResponse(t *testing.T) {
	h := parkedHarness(t, 1)
	baseGet := h.Tracker.GetReads()

	// Closed *and* stripped of its state label, which is the ordering trap: a
	// human resolving an issue by hand does both, and the unpark rule would
	// re-dispatch it.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.State = "closed"
		i.Labels = []string{"ben-queue"}
	})
	h.Tick()
	h.WaitGone("1")

	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want the claim dropped once the parked issue closed", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the workspace cleaned up", got)
	}
	if got := h.Tracker.GetReads() - baseGet; got != 0 {
		t.Errorf("spent %d per-issue read(s) noticing a close; the list response carries state", got)
	}
}

// SPEC §9.8, §9.10: absence from an assignee-filtered list is not evidence. It
// cannot separate an unassignment from consistency lag, so it buys one Get and
// decides from that.
func TestParkedRecordAbsentFromTheSweepIsConfirmedNotActedOn(t *testing.T) {
	h := parkedHarness(t, 1)
	// Absent from the list read, still ours according to Get: the lag case, which
	// an assignee-filtered response cannot tell from an unassignment.
	fresh := fake.Issue("1", epoch)
	fresh.Assignees = []string{fake.DefaultPrincipal}
	fresh.Labels = []string{"ben-queue", "ben:needs-review"}
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })
	h.Tracker.SetGetResult("1", &fresh)
	baseGet := h.Tracker.GetReads()

	h.Tick()
	waitFor(t, "the confirming Get", func() bool { return h.Tracker.GetReads() == baseGet+1 })
	h.settle(len(h.o.Transitions.Entries()))

	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Errorf("state = %q after an absence the Get contradicted, want it left at %q", got, StateNeedsReview)
	}
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an absence, before confirming it", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposals = %+v; an unconfirmed absence disposed a workspace", got)
	}
}

// And the verdict once the Get agrees: the assignment *is* the claim (§8.3), so
// there is nothing to release and nothing left to track. A restart reaches the
// same place by a different route — §9.10 classifies from ClaimedByPrincipal,
// which would not return this issue either.
func TestParkedRecordConfirmedNotOursIsDroppedWithoutARelease(t *testing.T) {
	h := parkedHarness(t, 1)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = []string{"someone-else"} })

	h.Tick()
	h.WaitGone("1")

	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue assigned to somebody else", n)
	}
	// Kept, not deleted: work may be on the branch, and this is the same call
	// §9.8 makes for an issue that leaves the workflow.
	got := h.Workspaces.Disposals("1")
	if len(got) != 1 || !got[0].Keep {
		t.Errorf("disposals = %+v, want exactly one with keep=true", got)
	}
	// The state label is left standing. Clearing it is what discards the
	// recovery and attempt continuity §9.10 classifies from.
	if got := h.Tracker.Label("1"); got != core.StateLabelNeedsReview {
		t.Errorf("state label = %q, want ben:needs-review left where it was", got)
	}
}

// A deleted issue is absent from the list read too, and it is the one absence
// that ends the record. The adapter states it as a named error rather than as a
// nil issue, so "could not ask" stays distinct from "not there".
func TestParkedRecordWhoseIssueIsDeletedIsCleanedUp(t *testing.T) {
	h := parkedHarness(t, 1)
	h.Tracker.Delete("1")

	h.Tick()
	h.WaitGone("1")

	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the deleted issue's workspace disposed", got)
	}
}

// A confirming read that fails is a question BEN could not ask. Keep everything
// and ask again next tick.
func TestParkedRecordAbsenceThatCannotBeConfirmedChangesNothing(t *testing.T) {
	h := parkedHarness(t, 1)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })
	h.Tracker.SetFailGet(errors.New("502 from the tracker"))
	baseGet := h.Tracker.GetReads()

	h.Tick()
	waitFor(t, "the failed confirming Get", func() bool { return h.Tracker.GetReads() == baseGet+1 })
	h.settle(len(h.o.Transitions.Entries()))

	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Errorf("state = %q after a confirming read failed, want it left at %q", got, StateNeedsReview)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposals = %+v; a failed read decided something", got)
	}

	// And the question is asked again rather than assumed either way.
	h.Tracker.SetFailGet(nil)
	h.Tick()
	h.WaitGone("1")
}

// One confirming Get per record, not one per tick per record: the absence path
// is off the sweep's hot path precisely because it costs a request.
func TestParkedAbsenceConfirmationIsOneReadAtATime(t *testing.T) {
	h := parkedHarness(t, 1)
	release := make(chan struct{})
	h.Tracker.SetGetGate(func() { <-release })
	t.Cleanup(func() { close(release) })
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })
	baseGet := h.Tracker.GetReads()

	h.Tick()
	waitFor(t, "the confirming Get to be in flight", func() bool { return h.Tracker.GetReads() == baseGet+1 })
	for range 3 {
		h.Tick()
	}

	if got := h.Tracker.GetReads() - baseGet; got != 1 {
		t.Errorf("confirming reads = %d over four ticks, want 1 held open: a second every tick is the O(parked) cost this avoids", got)
	}
}

// A record can park while the tick's per-issue reads are still out, so
// reconcile() gets a refreshResult for a record that is now parked. Its verdicts
// belong to the sweep from that instant, and every per-issue rule is wrong for
// it: this fixture removes a required label, which for a live record means stop,
// keep the workspace and release — and for a parked one is the reapproval window
// §9.5 leaves open, where a labeler has removed a required label and not yet
// re-applied it.
// A shutdown that begins while the sweep read is out. The snapshot cannot know
// about it — it was taken before the signal — so the re-check at classification
// time is the one that holds, and a parked record is where it is easiest to miss:
// it owns no process and no workspace, so nothing about it looks like work in
// flight, and the unpark it would order is a projection and a re-dispatch in the
// middle of a drain that is supposed to initiate neither (SPEC §9.8, §11).
func TestADrainThatBeginsWhileTheSweepIsOutStopsTheUnpark(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker(fake.Issue("1", epoch)))
	r := &Record{Issue: issueFixture("1"), State: StateNeedsReview, token: o.newToken()}
	o.records["1"] = r
	// The snapshot was taken before any of this: eligible, and about to be
	// classified from a response that says a human removed the state label.
	eligible := []parkedWant{{id: "1", labelsSettled: true}}

	o.draining = true
	r.suspended = true

	fresh := issueFixture("1")
	o.sweepParked(t.Context(), eligible, sweepResult{read: true, issues: []core.Issue{fresh}}, o.configNow())

	if r.State != StateNeedsReview {
		t.Errorf("state = %q; the drain acted on a re-queue", r.State)
	}
	if r.owesAnything() {
		t.Errorf("owed = %d write(s); the drain projected a label", len(r.owed))
	}
}

// The confirming Get carries the same label question as the sweep read, and it
// is asked at *its* issue time for the same reason: the projection can land while
// the read is in flight, leaving the record owing nothing and the answer still
// predating it. White-box, because that is a window of a few instructions.
func TestAConfirmingGetOlderThanTheProjectionDoesNotUnpark(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker(fake.Issue("1", epoch)))
	r := &Record{Issue: issueFixture("1"), State: StateNeedsReview, token: o.newToken()}
	o.records["1"] = r

	// Assigned and open, and carrying no ben:* label — which is what the tracker
	// says right up until this record's own park projection lands.
	fresh := issueFixture("1")
	fresh.Assignees = []string{fake.DefaultPrincipal}
	o.onParkedConfirmed(t.Context(), r, signal{
		kind: sigParkedConfirmed, issue: "1", token: r.token,
		refetched: &fresh, labelsSettled: false,
	})

	if r.State != StateNeedsReview {
		t.Errorf("state = %q; the confirmation unparked on a label set older than the park", r.State)
	}
	if r.orderedExit() {
		t.Error("the confirmation ordered an exit for an issue it found open and ours")
	}
}

// White-box, because the window is one signal wide: the refresh was issued for a
// `verifying` or `running` record and applied to a parked one.
func TestARecordThatParksWhileItsRefreshIsOutIsLeftToTheSweep(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker(fake.Issue("1", epoch)))
	r := &Record{Issue: issueFixture("1"), State: StateNeedsReview, token: o.newToken()}
	o.records["1"] = r

	// What a labeler mid-reapproval looks like: the required label is gone, and
	// has not been re-applied yet. For a live record this is stop, keep the
	// workspace, release.
	fresh := issueFixture("1")
	fresh.Labels = nil
	o.reconcile(t.Context(), map[string]refreshResult{"1": {issue: &fresh}}, o.configNow())

	if r.orderedExit() {
		t.Error("a per-issue refresh ordered an exit for a parked record; its verdicts are the sweep's")
	}
	if r.State != StateNeedsReview {
		t.Errorf("state = %q, want the record left parked", r.State)
	}
	if r.owesAnything() {
		t.Errorf("owed = %d write(s); the unroutable rule fired inside the reapproval window", len(r.owed))
	}
}

// The guard, and the bug it closes. Measured on the code this ticket changed:
// before the fold, a §9.5 drift park was re-queued by its own next refresh.
//
// That park is the sharp case because it is the only one that leaves *no* ben:*
// label standing — it comes from `queued`, where ben:claimed is owed only on the
// approved path — so a read composed before its projection lands answers "no
// state label", which is exactly what the unpark rule looks for.
func TestParkedRecordIsNotUnparkedByAReadThatPredatesItsOwnProjection(t *testing.T) {
	issue := fake.Issue("1", epoch)
	issue.Title, issue.Body = "approved title", "approved body"

	release := make(chan struct{})
	h := start(t, harnessOpts{
		definition: contentDefinition(t),
		issues:     []core.Issue{issue},
		beforeStart: func(tr *fake.Tracker) {
			tr.Edit("1", epoch.Add(time.Hour), func(c *core.IssueContent) { c.Body = "edited after approval" })
			// Hold every state-label write open. The park's own projection is the
			// first this issue would get.
			tr.SetLabelGate(func() { <-release })
		},
	})
	t.Cleanup(func() { close(release) })

	h.WaitState("1", StateNeedsReview)
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Fatalf("state label = %q; the fixture needs the projection still pending", got)
	}

	for range 3 {
		h.Tick()
	}

	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Errorf("state = %q: the sweep read a label set from before this record's own park and re-queued it", got)
	}
	if path := h.o.Transitions.Path("1"); containsState(path, StateBackoff) {
		t.Errorf("path = %v; the record unparked itself with no human touching the label", path)
	}
	if n := h.Runner.StartCount(); n != 0 {
		t.Errorf("started %d run(s); the re-queue dispatched content nobody reapproved", n)
	}
}

// The other half of that guard, and the pairing matters: gating the *whole*
// classification on a landed projection is the obvious reading, and it is wrong.
// Neither terminal state nor the assignment is a fact BEN's own writes can move,
// so a record whose projection is stuck retrying must still be classified for
// both. Otherwise a failing label write hides a closed — or deleted, which
// TestAGoneIssueDiscardsTrackerWritesAndKeepsLocalWork covers — issue for as long
// as it keeps failing, which is forever.
//
// Asserted on the owed queue, because with a tracker write stuck at the head
// nothing else can see the verdict: effects are head-of-line per record, so the
// release and the disposal this ordered wait behind the projection. That is the
// existing retry contract and not what this test is about — what it is about is
// whether the verdict was reached at all.
func TestAStuckProjectionDoesNotBlindTheParkedTerminalRule(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no commits"}, nil
		}),
		beforeStart: func(tr *fake.Tracker) {
			// ben:claimed and ben:running land — the record cannot reach the park
			// without them — and the park's own projection never does.
			tr.FailLabel = func(_ string, label core.StateLabel) error {
				if label == core.StateLabelNeedsReview {
					return errors.New("502 from the tracker")
				}
				return nil
			}
		},
	})
	h.WaitState("1", StateNeedsReview)
	// Refused for as long as the fixture stands, so the record owes a write that
	// can never land.
	if got := h.Tracker.Label("1"); got == core.StateLabelNeedsReview {
		t.Fatalf("state label = %q; the fixture needs the park's projection to keep failing", got)
	}

	before := h.applied(sigReconciled)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the reconciliation to be applied", func() bool { return h.applied(sigReconciled) > before })

	owed := h.owedAfterStop("1")
	var found bool
	for _, what := range owed {
		if what == effectRelease {
			found = true
		}
	}
	if !found {
		t.Errorf("owed = %v, want the release the terminal rule orders: the stuck projection hid the close", owed)
	}
}

// The zero VerifyResult must never reach `done`. §9.7's "a verification that
// could not be completed is never success" was true of the *error* path only;
// a verifier that returned a zero value with a nil error published a run
// nobody checked, because VerdictPublished used to be iota. VerdictUnknown
// takes that slot now, so the irreversible verdict requires saying so.
//
// Signed off on #45, where verify's own enum was built this way and B08's
// disagreed: "Published is the irreversible outcome and must require explicit
// construction."
func TestTheZeroVerifyResultParksInsteadOfPublishing(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			// A verifier that forgot to say anything, and did not fail either.
			return VerifyResult{}, nil
		}),
	})
	h.WaitState("1", StateNeedsReview)

	// Every irreversible consequence of `done` is what this is protecting.
	if got := h.Tracker.Milestones("1"); containsMilestone(got, core.MilestonePublished) {
		t.Errorf("milestones = %v; a run nobody verified was announced as published", got)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposals = %+v; the workspace was thrown away on an unstated verdict", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNeedsReview {
		t.Errorf("label = %q, want ben:needs-review", got)
	}
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; the claim converted to a held `done` record", got)
	}

	// And the operator is told what actually happened. Falling through to the
	// contradicted arm would report evidence against an agent that was never
	// checked.
	_, _, comments := h.Tracker.Snapshot()
	var detail string
	for _, c := range comments["1"] {
		if c.Milestone == core.MilestoneNeedsReview {
			detail = c.Detail
		}
	}
	if !strings.Contains(detail, "no usable verdict") {
		t.Errorf("needs-review detail = %q, want it to name the unstated verdict", detail)
	}
	if strings.Contains(detail, "contradicts") {
		t.Errorf("needs-review detail = %q; nothing was checked, so nothing was contradicted", detail)
	}
}

// SPEC §9.8: a parked issue that a human closed is finished, not held. The
// terminal check has to come before the unpark check, or the claim and
// workspace are retained forever.
func TestClosedParkedIssueIsCleanedUp(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no commits"}, nil
		}),
	})
	h.WaitState("1", StateNeedsReview)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	h.WaitGone("1")

	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want the claim dropped once the parked issue closed", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the workspace cleaned up", got)
	}
}

// A runner event queued before a decision must not undo it: a crash report
// landing after a confirmed budget park would otherwise move needs-review
// back to backoff.
func TestLateRunnerEventCannotUndoAPark(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
				{Type: core.EventFailed, Reason: core.FailureCrashed},
			}
		},
	})
	h.WaitState("1", StateNeedsReview)

	time.Sleep(30 * time.Millisecond)
	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Errorf("state = %q; a late crash event undid the park", got)
	}
	if got := h.o.Transitions.Path("1"); containsState(got, StateBackoff) {
		t.Errorf("path = %v, want no backoff after a budget park", got)
	}
}
