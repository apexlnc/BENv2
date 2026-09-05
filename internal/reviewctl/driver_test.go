package reviewctl

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// The whole loop, in the order #11 specifies it: code → machine review →
// revision → clean review → human review, with no bot approval anywhere and
// the human's label surviving every revision round.
func TestBoundedLoopEndToEnd(t *testing.T) {
	f := newForge()
	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested, review.VerdictClean}}
	d := newDriver(t, f, rev)

	// Round one: changes requested.
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("round one: %v", err)
	}
	if got := f.issue.Assignees; len(got) != 0 {
		t.Fatalf("assignees = %v, want the claim handed back", got)
	}
	if !f.hasLabel(fxQueue) {
		t.Fatal("the human's required label was removed on a revise route")
	}
	if routes := f.routes(); len(routes) != 1 || routes[0].Outcome != review.OutcomeRevise {
		t.Fatalf("routes = %+v, want one revise", routes)
	}

	// BEN reclaims, revises, and publishes a new head under a new occurrence.
	f.reclaim(head2)

	// Round two: clean.
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("round two: %v", err)
	}
	if f.hasLabel(fxQueue) {
		t.Fatal("a clean review left the issue dispatchable")
	}
	if !f.hasLabel(fxHumanLabel) {
		t.Fatal("the informational label was not added")
	}
	if got := f.issue.Assignees; len(got) != 1 || got[0] != fxPrincipal {
		t.Fatalf("assignees = %v, want the terminal route to leave BEN's claim to BEN (SPEC §9.8)", got)
	}
	routes := f.routes()
	if len(routes) != 2 || routes[1].Outcome != review.OutcomeHumanReview {
		t.Fatalf("routes = %+v, want a revise then a human-review", routes)
	}
	if routes[0].Occurrence == routes[1].Occurrence {
		t.Error("both rounds recorded the same occurrence")
	}
	if routes[0].Head != head1 || routes[1].Head != head2 {
		t.Errorf("routed heads = %s, %s; want %s then %s", routes[0].Head, routes[1].Head, head1, head2)
	}

	if rev.calls != 2 {
		t.Errorf("the reviewer ran %d times, want one per round", rev.calls)
	}
	assertNoApproval(t, f)
}

// Delivery is at-least-once and the sweep re-drives everything, so the second
// and third look at a settled cycle must change nothing at all.
func TestRedeliveryIsIdempotent(t *testing.T) {
	f := newForge()
	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested}}
	d := newDriver(t, f, rev)

	for i := 0; i < 3; i++ {
		if err := d.Run(context.Background()); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}

	if rev.calls != 1 {
		t.Errorf("the reviewer ran %d times, want once for one occurrence", rev.calls)
	}
	if n := f.countCalls("PublishReview"); n != 1 {
		t.Errorf("published %d reviews, want 1", n)
	}
	if n := f.countCalls("Unassign:" + fxPrincipal); n != 1 {
		t.Errorf("unassigned %d times, want 1", n)
	}
	if routes := f.routes(); len(routes) != 1 {
		t.Errorf("routes = %+v, want exactly one", routes)
	}
}

// The crash matrix. Each row is the forge as an interrupted run left it, and
// the assertion is that resuming completes the cycle without repeating a
// mutation or manufacturing a second review.
func TestResumesFromEveryInterruption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		verdict review.Verdict
		// interrupt leaves the forge in the mid-cycle state.
		interrupt func(*fakeForge, int64)

		wantReviewer  int
		wantPublished int
		wantUnassign  int
		wantRemove    int
		wantOutcome   review.Outcome
	}{
		{
			name:          "nothing done yet",
			verdict:       review.VerdictChangesRequested,
			interrupt:     func(*fakeForge, int64) {},
			wantReviewer:  1,
			wantPublished: 1,
			wantUnassign:  1,
			wantOutcome:   review.OutcomeRevise,
		},
		{
			name:    "review published, routing not started",
			verdict: review.VerdictChangesRequested,
			interrupt: func(f *fakeForge, occ int64) {
				f.seedReview(occ, fxEpoch, head1, review.VerdictChangesRequested)
			},
			wantReviewer: 0,
			wantUnassign: 1,
			wantOutcome:  review.OutcomeRevise,
		},
		{
			name:    "legacy review published before the approval field existed",
			verdict: review.VerdictChangesRequested,
			interrupt: func(f *fakeForge, occ int64) {
				f.seedReview(occ, fxEpoch, head1, review.VerdictChangesRequested)
				last := &f.reviews[len(f.reviews)-1]
				last.Body = strings.Replace(last.Body, " approval=900", "", 1)
			},
			wantReviewer: 0,
			wantUnassign: 1,
			wantOutcome:  review.OutcomeRevise,
		},
		{
			name:    "unassignment landed, marker did not",
			verdict: review.VerdictChangesRequested,
			interrupt: func(f *fakeForge, occ int64) {
				f.seedReview(occ, fxEpoch, head1, review.VerdictChangesRequested)
				_ = f.Unassign(context.Background(), fxIssue, fxPrincipal)
			},
			wantReviewer: 0,
			wantUnassign: 0, // beyond the one the interruption performed
			wantOutcome:  review.OutcomeRevise,
		},
		{
			name:    "BEN already reclaimed under a newer epoch",
			verdict: review.VerdictChangesRequested,
			interrupt: func(f *fakeForge, occ int64) {
				f.seedReview(occ, fxEpoch, head1, review.VerdictChangesRequested)
				_ = f.Unassign(context.Background(), fxIssue, fxPrincipal)
				f.events = append(f.events, review.Event{
					ID: f.id(), Type: review.EventAssigned, Actor: fxTracker,
					Assignee: fxPrincipal, CreatedAt: f.tick(),
				})
				f.issue.Assignees = []string{fxPrincipal}
			},
			wantReviewer: 0,
			wantUnassign: 0,
			wantOutcome:  review.OutcomeRevise,
		},
		{
			name:    "BEN reclaimed and pushed before route-marker recovery",
			verdict: review.VerdictChangesRequested,
			interrupt: func(f *fakeForge, occ int64) {
				f.seedReview(occ, fxEpoch, head1, review.VerdictChangesRequested)
				_ = f.Unassign(context.Background(), fxIssue, fxPrincipal)
				f.events = append(f.events, review.Event{
					ID: f.id(), Type: review.EventAssigned, Actor: fxTracker,
					Assignee: fxPrincipal, CreatedAt: f.tick(),
				})
				f.issue.Assignees = []string{fxPrincipal}
				f.pr.Head = head2
			},
			wantReviewer: 0,
			wantUnassign: 0,
			wantOutcome:  review.OutcomeRevise,
		},
		{
			name:    "clean review published, revocation not started",
			verdict: review.VerdictClean,
			interrupt: func(f *fakeForge, occ int64) {
				f.seedReview(occ, fxEpoch, head1, review.VerdictClean)
			},
			wantReviewer: 0,
			wantRemove:   1,
			wantOutcome:  review.OutcomeHumanReview,
		},
		{
			name:    "revocation landed, marker did not",
			verdict: review.VerdictClean,
			interrupt: func(f *fakeForge, occ int64) {
				f.seedReview(occ, fxEpoch, head1, review.VerdictClean)
				_ = f.RemoveLabel(context.Background(), fxIssue, fxQueue)
			},
			wantReviewer: 0,
			wantRemove:   0,
			wantOutcome:  review.OutcomeHumanReview,
		},
		{
			name:    "revocation landed and a human has already reapproved",
			verdict: review.VerdictClean,
			interrupt: func(f *fakeForge, occ int64) {
				f.seedReview(occ, fxEpoch, head1, review.VerdictClean)
				_ = f.RemoveLabel(context.Background(), fxIssue, fxQueue)
				f.events = append(f.events, review.Event{
					ID: f.id(), Type: review.EventLabeled, Actor: "a-human", Label: fxQueue, CreatedAt: f.tick(),
				})
				f.issue.Labels = append(f.issue.Labels, fxQueue)
			},
			wantReviewer: 0,
			wantRemove:   0,
			wantOutcome:  review.OutcomeHumanReview,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForge()
			occ := f.latestOccurrence(t)
			tc.interrupt(f, occ)

			before := map[string]int{
				"PublishReview": f.countCalls("PublishReview"),
				"Unassign":      f.countCalls("Unassign:" + fxPrincipal),
				"RemoveLabel":   f.countCalls("RemoveLabel:" + fxQueue),
			}

			rev := &scriptedReviewer{verdicts: []review.Verdict{tc.verdict}}
			if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}

			if rev.calls != tc.wantReviewer {
				t.Errorf("the reviewer ran %d times, want %d", rev.calls, tc.wantReviewer)
			}
			if got := f.countCalls("PublishReview") - before["PublishReview"]; tc.wantReviewer > 0 && got != tc.wantPublished {
				t.Errorf("published %d further reviews, want %d", got, tc.wantPublished)
			} else if tc.wantReviewer == 0 && got != 0 {
				t.Errorf("published %d further reviews, want none — the review already exists", got)
			}
			if got := f.countCalls("Unassign:"+fxPrincipal) - before["Unassign"]; got != tc.wantUnassign {
				t.Errorf("performed %d further unassignments, want %d", got, tc.wantUnassign)
			}
			if got := f.countCalls("RemoveLabel:"+fxQueue) - before["RemoveLabel"]; got != tc.wantRemove {
				t.Errorf("performed %d further revocations, want %d", got, tc.wantRemove)
			}
			routes := f.routes()
			if len(routes) != 1 {
				t.Fatalf("routes = %+v, want exactly one", routes)
			}
			if routes[0].Outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", routes[0].Outcome, tc.wantOutcome)
			}
			if routes[0].Occurrence != occ {
				t.Errorf("route occurrence = %d, want the interrupted one %d", routes[0].Occurrence, occ)
			}
			assertNoApproval(t, f)
		})
	}
}

// A publication may overtake marker recovery: BEN can observe the completed
// unassignment, reclaim, and publish O2 before the controller records O1. The
// driver must repair O1 first, then advance normally to O2 in the same run.
func TestRepairsAnOvertakenOccurrenceBeforeAdvancing(t *testing.T) {
	f := newForge()
	first := f.latestOccurrence(t)
	f.seedReview(first, fxEpoch, head1, review.VerdictChangesRequested)
	if err := f.Unassign(context.Background(), fxIssue, fxPrincipal); err != nil {
		t.Fatal(err)
	}
	f.reclaim(head2)
	second := f.latestOccurrence(t)

	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	routes := f.routes()
	if len(routes) != 2 {
		t.Fatalf("routes = %+v, want the repaired O1 route followed by O2", routes)
	}
	if routes[0].Occurrence != first || routes[0].Outcome != review.OutcomeRevise || routes[0].Head != head1 {
		t.Errorf("first route = %+v, want O1 revise at head1", routes[0])
	}
	if routes[1].Occurrence != second || routes[1].Outcome != review.OutcomeHumanReview || routes[1].Head != head2 {
		t.Errorf("second route = %+v, want O2 human-review at head2", routes[1])
	}
	if rev.calls != 1 || len(rev.heads) != 1 || rev.heads[0] != head2 {
		t.Errorf("reviewer calls = %d at %v, want only O2 at head2", rev.calls, rev.heads)
	}
}

// No-progress has no review for its own occurrence, so the terminal intent is
// the only durable record of its original subject. Once revocation lands, a
// human reapproval and a moved PR must not turn that completed stop into a new
// review of a different head.
func TestNoProgressIntentSurvivesReapprovalAndSubjectMovement(t *testing.T) {
	f := newForge()
	firstDriver := newDriver(t, f, &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested}})
	if err := firstDriver.Run(context.Background()); err != nil {
		t.Fatalf("first route: %v", err)
	}

	f.reclaim(head1) // no commit: this occurrence must stop as no-progress
	occurrence := f.latestOccurrence(t)
	claim := review.ClaimEpoch(review.SortEvents(f.events), fxPrincipal)
	f.seedIntent(occurrence, claim, head1, review.OutcomeNoProgress)
	if err := f.RemoveLabel(context.Background(), fxIssue, fxQueue); err != nil {
		t.Fatal(err)
	}
	f.events = append(f.events, review.Event{
		ID: f.id(), Type: review.EventLabeled, Actor: "a-human", Label: fxQueue, CreatedAt: f.tick(),
	})
	f.issue.Labels = append(f.issue.Labels, fxQueue)
	f.pr.Head = head2
	f.pr.BaseSHA = base2

	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if rev.calls != 0 {
		t.Fatalf("reviewer ran %d times; the completed no-progress route must only repair its marker", rev.calls)
	}
	routes := f.routes()
	if len(routes) != 2 || routes[1].Occurrence != occurrence ||
		routes[1].Outcome != review.OutcomeNoProgress || routes[1].Head != head1 {
		t.Fatalf("routes = %+v, want the original head1 no-progress route repaired", routes)
	}
	if !f.hasLabel(fxQueue) {
		t.Error("marker recovery removed the human's later reapproval")
	}
}

// An intent is only authority over the approval epoch it observed. If a human
// withdraws that approval and applies a new one before controller revocation,
// recovery records the old terminal route without removing the fresh label.
func TestPendingIntentPreservesFreshHumanApproval(t *testing.T) {
	f := newForge()
	if err := newDriver(t, f, &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested}}).
		Run(context.Background()); err != nil {
		t.Fatalf("first route: %v", err)
	}

	f.reclaim(head1)
	occurrence := f.latestOccurrence(t)
	claim := review.ClaimEpoch(review.SortEvents(f.events), fxPrincipal)
	f.seedIntent(occurrence, claim, head1, review.OutcomeNoProgress)
	f.humanReapprove()
	f.pr.Head = head2
	f.pr.BaseSHA = base2

	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if rev.calls != 0 {
		t.Fatalf("reviewer ran %d times; the intent must only finish its old route", rev.calls)
	}
	if got := f.countCalls("RemoveLabel:" + fxQueue); got != 0 {
		t.Fatalf("recovery removed the queue label %d times, want 0", got)
	}
	routes := f.routes()
	if len(routes) != 2 || routes[1].Occurrence != occurrence ||
		routes[1].Outcome != review.OutcomeNoProgress || routes[1].Head != head1 {
		t.Fatalf("routes = %+v, want the original no-progress route", routes)
	}
	if !f.hasLabel(fxQueue) {
		t.Fatal("recovery removed the fresh human approval")
	}
}

func TestPreIntentReapprovalStaysOutsideTheOldOccurrence(t *testing.T) {
	f := newForge()
	if err := newDriver(t, f, &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested}}).
		Run(context.Background()); err != nil {
		t.Fatalf("first route: %v", err)
	}

	f.reclaim(head1)
	occurrence := f.latestOccurrence(t)
	freshApproval := f.humanReapprove()
	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if rev.calls != 0 {
		t.Fatalf("reviewer ran %d times; the unchanged head must stop as no-progress", rev.calls)
	}
	intents := f.intents()
	if len(intents) != 1 || intents[0].marker.Occurrence != occurrence ||
		intents[0].marker.Approval != fxApproval || intents[0].marker.Approval == freshApproval {
		t.Fatalf("intents = %+v, want occurrence %d bound to original approval %d", intents, occurrence, fxApproval)
	}
	if got := f.countCalls("RemoveLabel:" + fxQueue); got != 0 {
		t.Fatalf("recovery removed the queue label %d times, want 0", got)
	}
	routes := f.routes()
	if len(routes) != 2 || routes[1].Occurrence != occurrence || routes[1].Outcome != review.OutcomeNoProgress {
		t.Fatalf("routes = %+v, want the old occurrence routed to no-progress", routes)
	}
	if !f.hasLabel(fxQueue) {
		t.Fatal("recovery removed the fresh human approval")
	}
}

func TestReviewedTerminalRoutePreservesPostOccurrenceApproval(t *testing.T) {
	f := newForge()
	occurrence := f.latestOccurrence(t)
	claim := review.ClaimEpoch(review.SortEvents(f.events), fxPrincipal)
	f.seedReview(occurrence, claim, head1, review.VerdictClean)
	f.humanReapprove()

	rev := &scriptedReviewer{}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if rev.calls != 0 {
		t.Fatalf("reviewer ran %d times; the clean review is already durable", rev.calls)
	}
	if got := f.countCalls("RemoveLabel:" + fxQueue); got != 0 {
		t.Fatalf("review-backed recovery removed the queue label %d times, want 0", got)
	}
	routes := f.routes()
	if len(routes) != 1 || routes[0].Occurrence != occurrence || routes[0].Outcome != review.OutcomeHumanReview {
		t.Fatalf("routes = %+v, want the reviewed occurrence routed to human review", routes)
	}
	if !f.hasLabel(fxQueue) {
		t.Fatal("review-backed recovery removed the fresh human approval")
	}
}

func TestPublishedRevisionRoutePreservesPostOccurrenceApproval(t *testing.T) {
	f := newForge()
	occurrence := f.latestOccurrence(t)
	claim := review.ClaimEpoch(review.SortEvents(f.events), fxPrincipal)
	f.seedReview(occurrence, claim, head1, review.VerdictChangesRequested)
	f.humanReapprove()

	if err := newDriver(t, f, &scriptedReviewer{}).Run(context.Background()); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if got := f.countCalls("Unassign:" + fxPrincipal); got != 0 {
		t.Fatalf("stale revision review unassigned the replacement approval %d times", got)
	}
	routes := f.routes()
	if len(routes) != 1 || routes[0].Occurrence != occurrence || routes[0].Outcome != review.OutcomeRevise {
		t.Fatalf("routes = %+v, want the old occurrence recorded as revise without a new mutation", routes)
	}
	if !f.hasLabel(fxQueue) || len(f.issue.Assignees) != 1 || f.issue.Assignees[0] != fxPrincipal {
		t.Fatalf("stale output mutated the replacement approval: labels=%v assignees=%v", f.issue.Labels, f.issue.Assignees)
	}
}

// The two entry points differ only in what wakes the process, so they have to
// agree — the ticket makes this an acceptance criterion because a controller
// whose scheduled path behaves differently is one whose reconciliation cannot
// be trusted to repair the event path.
func TestEventAndScheduledPathsAgree(t *testing.T) {
	byEvent := newForge()
	if err := newDriver(t, byEvent, &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested}}).
		Run(context.Background()); err != nil {
		t.Fatalf("event path: %v", err)
	}

	scheduled := newForge()
	targets, err := scheduled.Candidates(context.Background(), fxQueue)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range targets {
		d := newDriver(t, scheduled, &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested}})
		d.cfg.Issue = n
		if err := d.Run(context.Background()); err != nil {
			t.Fatalf("scheduled path: %v", err)
		}
	}

	a, b := byEvent.routes(), scheduled.routes()
	if len(a) != 1 || len(b) != 1 || a[0] != b[0] {
		t.Fatalf("routes differ: event %+v, scheduled %+v", a, b)
	}
	if byEvent.hasLabel(fxQueue) != scheduled.hasLabel(fxQueue) ||
		len(byEvent.issue.Assignees) != len(scheduled.issue.Assignees) {
		t.Fatalf("issue state differs: event %+v, scheduled %+v", byEvent.issue, scheduled.issue)
	}
}

// A head that moves while the model is thinking must not be judged from the
// diff it read: the verdict is discarded, nothing is published, and the new
// head is reviewed instead.
func TestAMovingHeadIsReviewedAgain(t *testing.T) {
	f := newForge()
	moved := false
	f.onDiff = func(f *fakeForge) {
		if !moved {
			moved = true
			f.pr.Head = head2
		}
	}

	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean, review.VerdictClean}}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := f.countCalls("PublishReview"); n != 1 {
		t.Fatalf("published %d reviews, want exactly the one for the settled head", n)
	}
	if got := f.reviews[0].CommitID; got != head2 {
		t.Errorf("published review is bound to %s, want the current head %s", got, head2)
	}
	if routes := f.routes(); len(routes) != 1 || routes[0].Head != head2 {
		t.Fatalf("routes = %+v, want one for the current head", routes)
	}
}

// Revocation and reapproval while the model is running replaces the workspace
// cycle even when the claim and PR head have not moved. Output from the old
// sandbox has no authority in the replacement cycle.
func TestReapprovalWhileReviewingDiscardsTheOldCycleOutput(t *testing.T) {
	f := newForge()
	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	rev.onReview = func(reviewrun.Subject) {
		f.events = append(f.events,
			review.Event{ID: f.id(), Type: review.EventUnlabeled, Actor: "a-human", Label: fxQueue, CreatedAt: f.tick()},
			review.Event{ID: f.id(), Type: review.EventLabeled, Actor: "a-human", Label: fxQueue, CreatedAt: f.tick()},
		)
	}

	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rev.calls != 1 {
		t.Fatalf("reviewer calls = %d, want the one already in flight", rev.calls)
	}
	if got := f.countCalls("PublishReview"); got != 0 {
		t.Fatalf("published %d stale reviews after reapproval", got)
	}
	if len(f.issue.Assignees) != 1 || !f.hasLabel(fxQueue) {
		t.Fatalf("stale output mutated the replacement cycle: %+v", f.issue)
	}
}

// A pull request retarget can preserve the head while changing every byte in
// the three-dot comparison. The base SHA is therefore part of the subject,
// both during one reviewer run and in the durable review marker.
func TestAMovingBaseIsReviewedAgain(t *testing.T) {
	f := newForge()
	moved := false
	f.onDiff = func(f *fakeForge) {
		if !moved {
			moved = true
			f.pr.Base = "release"
			f.pr.BaseSHA = base2
		}
	}

	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean, review.VerdictClean}}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rev.calls != 2 {
		t.Fatalf("reviewer calls = %d, want the old-base verdict discarded and the new base reviewed", rev.calls)
	}
	if len(f.diffBases) != 2 || f.diffBases[0] != base1 || f.diffBases[1] != base2 {
		t.Fatalf("diff bases = %v, want the captured base then the retargeted base", f.diffBases)
	}
	if len(f.reviews) != 1 {
		t.Fatalf("published reviews = %d, want only the settled subject", len(f.reviews))
	}
	marker, err := review.ParseReviewMarker(f.reviews[0].Body)
	if err != nil {
		t.Fatal(err)
	}
	if marker.Base != base2 {
		t.Errorf("review base = %s, want %s", marker.Base, base2)
	}
}

func TestConfiguredMaxDiffBytesBoundsTheReviewerSubject(t *testing.T) {
	f := newForge()
	f.diff = strings.Repeat("0123456789", 20)
	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	d := newDriver(t, f, rev)
	d.maxDiffBytes = 37

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(rev.subjects) != 1 {
		t.Fatalf("review subjects = %d", len(rev.subjects))
	}
	got := rev.subjects[0].Diff
	if !strings.HasPrefix(got, f.diff[:37]) || !strings.Contains(got, "*** truncated:") {
		t.Fatalf("bounded diff = %q", got)
	}
	if strings.Contains(got, f.diff[:38]) {
		t.Fatal("reviewer received bytes beyond review.max_diff_bytes")
	}
}

// Publication is not the end of the race window: a push can land after the
// COMMENT review exists but before its route. The old review remains evidence
// for its head and consumes a round, but it must not suppress a review of the
// new current head under the same delivery occurrence.
func TestAHeadMovedAfterReviewPublicationIsReviewedAgain(t *testing.T) {
	f := newForge()
	occurrence := f.latestOccurrence(t)
	f.seedReview(occurrence, fxEpoch, head1, review.VerdictChangesRequested)
	f.pr.Head = head2

	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rev.calls != 1 || f.countCalls("PublishReview") != 1 {
		t.Fatalf("new head review calls = %d, publications = %d; want one of each", rev.calls, f.countCalls("PublishReview"))
	}
	if got := f.reviews[len(f.reviews)-1].CommitID; got != head2 {
		t.Errorf("new review commit = %s, want moved head %s", got, head2)
	}
	routes := f.routes()
	if len(routes) != 1 || routes[0].Head != head2 || routes[0].Outcome != review.OutcomeHumanReview {
		t.Fatalf("routes = %+v, want clean human-review route for the moved head", routes)
	}
}

func TestABaseMovedAfterReviewPublicationIsReviewedAgain(t *testing.T) {
	f := newForge()
	occurrence := f.latestOccurrence(t)
	f.seedReview(occurrence, fxEpoch, head1, review.VerdictChangesRequested)
	f.pr.Base = "release"
	f.pr.BaseSHA = base2

	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rev.calls != 1 || f.countCalls("PublishReview") != 1 {
		t.Fatalf("new-base review calls = %d, publications = %d; want one of each", rev.calls, f.countCalls("PublishReview"))
	}
	latest, err := review.ParseReviewMarker(f.reviews[len(f.reviews)-1].Body)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Base != base2 || latest.Head != head1 {
		t.Errorf("new review subject = base %s, head %s; want base %s, head %s", latest.Base, latest.Head, base2, head1)
	}
	if routes := f.routes(); len(routes) != 1 || routes[0].Outcome != review.OutcomeHumanReview {
		t.Fatalf("routes = %+v, want only the new-base clean verdict routed", routes)
	}
}

// A reviewer that states nothing authorizes nothing, and the run ends cleanly:
// the occurrence stays unrouted for the next sweep, and the human whose label
// is still standing is not told their CI is broken.
func TestNoVerdictPublishesAndRoutesNothing(t *testing.T) {
	f := newForge()
	rev := &scriptedReviewer{err: review.ErrNoVerdict}

	if err := newDriver(t, f, rev).Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := f.countCalls("PublishReview"); n != 0 {
		t.Errorf("published %d reviews on no verdict", n)
	}
	if len(f.routes()) != 0 {
		t.Errorf("routed %+v on no verdict", f.routes())
	}
	if !f.hasLabel(fxQueue) || len(f.issue.Assignees) != 1 {
		t.Errorf("the issue was mutated on no verdict: %+v", f.issue)
	}
}

// Three distinct reviewed heads reaches the cap, and a repeated head stops
// without spending another round at all.
func TestRoundCapAndNoProgress(t *testing.T) {
	t.Run("three heads then the cap", func(t *testing.T) {
		f := newForge()
		rev := &scriptedReviewer{verdicts: []review.Verdict{
			review.VerdictChangesRequested, review.VerdictChangesRequested, review.VerdictChangesRequested,
		}}
		d := newDriver(t, f, rev)

		for _, head := range []string{head2, head3} {
			if err := d.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			f.reclaim(head)
		}
		if err := d.Run(context.Background()); err != nil {
			t.Fatalf("final Run: %v", err)
		}

		if rev.calls != 3 {
			t.Errorf("the reviewer ran %d times, want the cap of 3", rev.calls)
		}
		outcomes := outcomesOf(f.routes())
		want := []review.Outcome{review.OutcomeRevise, review.OutcomeRevise, review.OutcomeRoundCap}
		if !sameOutcomes(outcomes, want) {
			t.Fatalf("outcomes = %v, want %v", outcomes, want)
		}
		if f.hasLabel(fxQueue) {
			t.Error("automation is still enabled after the cap")
		}
	})

	t.Run("a repeated head consumes no round", func(t *testing.T) {
		f := newForge()
		rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested}}
		d := newDriver(t, f, rev)

		if err := d.Run(context.Background()); err != nil {
			t.Fatalf("round one: %v", err)
		}
		f.reclaim(head1) // reclaimed, published again, but produced no commit

		if err := d.Run(context.Background()); err != nil {
			t.Fatalf("round two: %v", err)
		}
		if rev.calls != 1 {
			t.Errorf("the reviewer ran %d times, want 1 — a repeated head is not a round", rev.calls)
		}
		outcomes := outcomesOf(f.routes())
		if !sameOutcomes(outcomes, []review.Outcome{review.OutcomeRevise, review.OutcomeNoProgress}) {
			t.Fatalf("outcomes = %v, want revise then no-progress", outcomes)
		}
		intents := f.intents()
		if len(intents) != 1 {
			t.Fatalf("terminal intents = %+v, want one", intents)
		}
		if got := intents[0].marker; got.Outcome != review.OutcomeNoProgress || got.Head != head1 || got.Approval == 0 {
			t.Fatalf("terminal intent = %+v, want no-progress on %s", got, head1)
		}
		var revokedAt time.Time
		for _, ev := range f.events {
			if ev.Type == review.EventUnlabeled && strings.EqualFold(ev.Actor, fxController) && strings.EqualFold(ev.Label, fxQueue) {
				revokedAt = ev.CreatedAt
				break
			}
		}
		if revokedAt.IsZero() {
			t.Fatal("no controller revocation was recorded")
		}
		if !intents[0].createdAt.Before(revokedAt) {
			t.Errorf("terminal intent at %s did not precede revocation at %s", intents[0].createdAt, revokedAt)
		}
		if f.hasLabel(fxQueue) {
			t.Error("no progress left automation enabled")
		}
	})
}

// A read that fails is a failure, never an issue with fewer facts: the
// controller must not treat a 500 on the review list as "no reviews yet" and
// publish a second review of a head it has already judged.
func TestAFailedReadNeverRoutes(t *testing.T) {
	for _, op := range []string{"Issue", "Comments", "Events", "Reviews", "PullRequest"} {
		t.Run(op, func(t *testing.T) {
			f := newForge()
			f.err[op] = errors.New("boom")
			rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}

			err := newDriver(t, f, rev).Run(context.Background())
			if err == nil {
				t.Fatal("Run returned no error on a failed read")
			}
			if rev.calls != 0 || f.countCalls("PublishReview") != 0 || len(f.routes()) != 0 {
				t.Errorf("a failed read produced work: reviewer=%d published=%d routes=%+v",
					rev.calls, f.countCalls("PublishReview"), f.routes())
			}
		})
	}
}

// A dry run is what an operator points at production before trusting it, so it
// must reach a real decision and perform none of it.
func TestDryRunMutatesNothing(t *testing.T) {
	f := newForge()
	rev := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	d := newDriver(t, f, rev)
	d.dryRun = true

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, op := range f.calls {
		switch {
		case strings.HasPrefix(op, "Unassign"), strings.HasPrefix(op, "RemoveLabel"),
			strings.HasPrefix(op, "AddLabel"), op == "PublishReview", op == "PostComment":
			t.Errorf("a dry run performed %s", op)
		}
	}
	if rev.calls != 0 {
		t.Errorf("a dry run ran the reviewer %d times", rev.calls)
	}
}

// assertNoApproval is the invariant every scenario shares, asserted at the
// forge rather than in the reducer: whatever happened, nothing this controller
// published is an approval and nothing it wrote is a `ben:*` state label.
func assertNoApproval(t *testing.T, f *fakeForge) {
	t.Helper()
	for _, r := range f.reviews {
		if !strings.EqualFold(r.Author, fxController) {
			continue
		}
		if r.State != review.ReviewStateCommented {
			t.Errorf("the controller published a %s review", r.State)
		}
	}
	for _, op := range f.calls {
		if strings.HasPrefix(op, "AddLabel:") && strings.HasPrefix(strings.TrimPrefix(op, "AddLabel:"), "ben:") {
			t.Errorf("the controller wrote a state label: %s", op)
		}
		if op == "AddLabel:"+fxQueue {
			t.Error("the controller applied the required label — that is SPEC §9.5's approval act")
		}
	}
}

func outcomesOf(routes []review.RouteMarker) []review.Outcome {
	out := make([]review.Outcome, len(routes))
	for i, r := range routes {
		out[i] = r.Outcome
	}
	return out
}

func sameOutcomes(got, want []review.Outcome) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
