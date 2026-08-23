package fake

// These stay in the *internal* test package, unlike tracker_test.go beside them:
// they assert on the change log the tracker records, and one of them needs
// containsFold. #131 moved the external tests to package fake_test; both can live
// in one directory, and this is the half that legitimately reaches inside.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// epochForTest is a fixed creation time, so nothing here depends on when the test
// runs.
var epochForTest = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

// The tracker's change log is what SPEC §9.10 classifies from, so its fidelity to
// the real adapter is a correctness concern rather than a convenience (AGENTS.md).
// These tests pin the fake's half; the adapter's half is pinned independently in
// github's TestSetStateLabelsLogsTheAddBeforeTheRemove, because a fake and an
// adapter that only agree with each other can both be wrong.

func history(t *testing.T, tr *Tracker, id string) []string {
	t.Helper()
	events, err := tr.ClaimHistory(context.Background(), core.Issue{Identifier: id})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	var out []string
	var lastID int64
	for i, ev := range events {
		if ev.ID == 0 {
			t.Errorf("event %+v carries no id; §9.10 anchors the claim cycle on one", ev)
		}
		if i > 0 && ev.ID <= lastID {
			t.Errorf("event %d id %d does not follow %d; ClaimHistory promises (at, id) order",
				i, ev.ID, lastID)
		}
		lastID = ev.ID
		out = append(out, string(ev.Kind)+":"+ev.Subject)
	}
	return out
}

// A projection adds before it removes, so an interrupted one leaves two `ben:*`
// labels standing rather than none. Removing first would leave none, and no state
// label on an assigned issue reads as published-awaiting-review — a run abandoned
// as if it had succeeded (SPEC §9.10 step 3).
func TestSetStateLabelsRecordsTheAddBeforeTheRemove(t *testing.T) {
	tr := NewTracker(Issue("1", epochForTest))
	ctx := context.Background()
	issue := core.Issue{Identifier: "1"}

	if _, err := tr.Claim(ctx, issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	for _, label := range []core.StateLabel{core.StateLabelClaimed, core.StateLabelRunning} {
		if err := tr.SetStateLabels(ctx, issue, label); err != nil {
			t.Fatalf("SetStateLabels(%s): %v", label, err)
		}
	}

	// The queue label the issue was filed with comes first: upstream records the
	// install-time labels, because GitHub does (Tracker.install).
	want := "[labeled:ben-queue assigned:ben-bot labeled:ben:claimed labeled:ben:running unlabeled:ben:claimed]"
	if got := fmt.Sprint(history(t, tr, "1")); got != want {
		t.Errorf("history = %v, want %s", got, want)
	}
}

// Re-projecting a label already standing writes no event, because GitHub's label
// add is idempotent and writes no timeline entry. It is what makes §9.10 step 4's
// "every recovery verdict re-issues its projection" free rather than a log full of
// churn a later classification has to read past.
func TestReprojectingAStandingLabelRecordsNothing(t *testing.T) {
	tr := NewTracker(Issue("1", epochForTest))
	ctx := context.Background()
	issue := core.Issue{Identifier: "1"}

	if err := tr.SetStateLabels(ctx, issue, core.StateLabelClaimed); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	before := len(history(t, tr, "1"))
	if err := tr.SetStateLabels(ctx, issue, core.StateLabelClaimed); err != nil {
		t.Fatalf("re-projecting: %v", err)
	}
	if got := len(history(t, tr, "1")); got != before {
		t.Errorf("re-projecting added %d events; an idempotent add writes none", got-before)
	}
}

// InterruptStateLabels is the crash between the two writes, and it must leave the
// world a crash leaves: the new label on, the old one still standing, and one
// event for the half that landed.
func TestInterruptStateLabelsLeavesTwoStanding(t *testing.T) {
	tr := NewTracker(Issue("1", epochForTest))
	ctx := context.Background()
	issue := core.Issue{Identifier: "1"}

	if err := tr.SetStateLabels(ctx, issue, core.StateLabelClaimed); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	tr.InterruptStateLabels("1", core.StateLabelNeedsReview)

	got, err := tr.Get(ctx, "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !containsFold(got.Labels, "ben:claimed") || !containsFold(got.Labels, "ben:needs-review") {
		t.Errorf("labels = %v, want both standing", got.Labels)
	}
	want := "[labeled:ben-queue labeled:ben:claimed labeled:ben:needs-review]"
	if h := fmt.Sprint(history(t, tr, "1")); h != want {
		t.Errorf("history = %v, want %s — only the add landed", h, want)
	}
}

// A close and a reopen land in the change log, because that is the only place the
// close survives once the reopen has moved the state back. §9.10 gate 1 and the
// §9.8 sweep both classify from the event.
func TestMutateRecordsClosesAndReopens(t *testing.T) {
	tr := NewTracker(Issue("1", epochForTest))
	tr.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	tr.Mutate("1", func(i *core.Issue) { i.State = "open" })
	// A mutation that does not move the state records nothing.
	tr.Mutate("1", func(i *core.Issue) { i.Revision = "someone-commented" })

	want := "[labeled:ben-queue closed: reopened:]"
	if got := fmt.Sprint(history(t, tr, "1")); got != want {
		t.Errorf("history = %v, want %s", got, want)
	}
	if got, err := tr.Get(context.Background(), "1"); err != nil || got.State != "open" {
		t.Fatalf("state = %v (err %v); the fixture is only interesting while the reopen stands", got, err)
	}
}

// ClaimedByPrincipal is unfiltered by design: the claims most in need of cleanup
// are exactly the ones that have left the queue partition (SPEC §9.10 step 1).
func TestClaimedByPrincipalIsUnfiltered(t *testing.T) {
	tr := NewTracker(
		Issue("1", epochForTest),
		Issue("2", epochForTest),
		Issue("3", epochForTest),
	)
	ctx := context.Background()
	for _, id := range []string{"1", "2"} {
		if _, err := tr.Claim(ctx, core.Issue{Identifier: id}); err != nil {
			t.Fatalf("Claim(%s): %v", id, err)
		}
	}
	// One closed, one stripped of its queue labels: both still carry our claim.
	tr.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	tr.Mutate("2", func(i *core.Issue) { i.Labels = nil })

	got, err := tr.ClaimedByPrincipal(ctx)
	if err != nil {
		t.Fatalf("ClaimedByPrincipal: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("returned %d candidates, want 2 — the read is unfiltered by state and by label", len(got))
	}
	// Counted apart from the sweep read, whose one-request-per-tick cost a recovery
	// read must not be able to satisfy.
	if tr.ClaimedReads() != 1 || tr.HeldReads() != 0 {
		t.Errorf("claimed reads = %d, held reads = %d; the two postures are counted separately",
			tr.ClaimedReads(), tr.HeldReads())
	}
}

// The fake refuses what the adapter refuses. A comment the tracker can never take
// is an owed write retried forever, with everything behind it queued — so a fake
// that accepted one would let the orchestrator build it and call the result a pass.
func TestCommentRefusesAnIncompletePayload(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		comment core.MilestoneComment
		// setup leaves the change log carrying the anchor this milestone needs.
		setup func(*Tracker)
	}{
		{
			name:    "published with no pull request link",
			comment: core.MilestoneComment{Milestone: core.MilestonePublished},
			setup: func(tr *Tracker) {
				_ = tr.SetStateLabels(ctx, core.Issue{Identifier: "1"}, core.StateLabelClaimed)
				_ = tr.SetStateLabels(ctx, core.Issue{Identifier: "1"}, core.StateLabelNone)
			},
		},
		{
			name:    "failed with neither a reason nor the disclaimer",
			comment: core.MilestoneComment{Milestone: core.MilestoneFailed},
			setup: func(tr *Tracker) {
				_ = tr.SetStateLabels(ctx, core.Issue{Identifier: "1"}, core.StateLabelFailed)
			},
		},
		{
			name: "failed with both a reason and the disclaimer",
			comment: core.MilestoneComment{
				Milestone: core.MilestoneFailed, Reason: core.FailureStalled, ReasonUnavailable: true,
			},
			setup: func(tr *Tracker) {
				_ = tr.SetStateLabels(ctx, core.Issue{Identifier: "1"}, core.StateLabelFailed)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewTracker(Issue("1", epochForTest))
			tc.setup(tr)
			err := tr.Comment(ctx, core.Issue{Identifier: "1"}, tc.comment)
			if !errors.Is(err, ErrMilestonePayload) {
				t.Fatalf("Comment = %v, want ErrMilestonePayload", err)
			}
			if got := tr.CommentsFor("1"); len(got) != 0 {
				t.Errorf("a refused comment was still recorded: %+v", got)
			}
		})
	}
}

// A milestone with no label transition to anchor it is refused rather than posted
// against occurrence zero, which is how two unrelated postings would collapse into
// one (github milestoneOccurrence).
func TestCommentRefusesAnUnanchoredMilestone(t *testing.T) {
	tr := NewTracker(Issue("1", epochForTest))
	err := tr.Comment(context.Background(), core.Issue{Identifier: "1"},
		core.MilestoneComment{Milestone: core.MilestoneClaimed})
	if !errors.Is(err, ErrNoMilestoneOccurrence) {
		t.Fatalf("Comment = %v, want ErrNoMilestoneOccurrence", err)
	}
}

// Idempotent per *occurrence*, not per kind: a human re-queue removes the label,
// and the next parking is a new occurrence that owes its own comment.
func TestCommentIsIdempotentPerOccurrenceNotPerKind(t *testing.T) {
	ctx := context.Background()
	tr := NewTracker(Issue("1", epochForTest))
	issue := core.Issue{Identifier: "1"}
	park := core.MilestoneComment{Milestone: core.MilestoneNeedsReview}

	if err := tr.SetStateLabels(ctx, issue, core.StateLabelNeedsReview); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	for range 3 {
		if err := tr.Comment(ctx, issue, park); err != nil {
			t.Fatalf("Comment: %v", err)
		}
	}
	if got := len(tr.CommentsFor("1")); got != 1 {
		t.Fatalf("posted %d comments for one occurrence, want 1", got)
	}

	// A human re-queues, and the issue is parked again: a second occurrence.
	if err := tr.SetStateLabels(ctx, issue, core.StateLabelNone); err != nil {
		t.Fatalf("clearing the label: %v", err)
	}
	if err := tr.SetStateLabels(ctx, issue, core.StateLabelNeedsReview); err != nil {
		t.Fatalf("re-parking: %v", err)
	}
	if err := tr.Comment(ctx, issue, park); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if got := len(tr.CommentsFor("1")); got != 2 {
		t.Errorf("posted %d comments over two parkings, want 2 — keying on the kind alone would "+
			"suppress the second, and a park with no explanation is what §9.10 forbids", got)
	}
}

// ClaimHistory promises a log already ordered by (At, ID) (core.ClaimEvent), and a
// fixture that scripts a *dated* event is the case that can break it: the tracker's
// own clock knows nothing about that date, so the next ordinary write would be
// stamped with a time before the thing it happened after.
//
// A caller that derived the claim cycle from such a log would read a live
// assignment as superseded — §9.10 step 2 anchors on the most recent standing
// `assigned`, and "most recent" is meaningless in an unordered log.
func TestHistoryStaysOrderedWhenAFixtureScriptsDatedEvents(t *testing.T) {
	ctx := context.Background()
	tr := NewTracker(Issue("1", epochForTest))

	// A human's action, dated after the issue was filed. Relative to the issue's own
	// creation rather than to TrackerEpoch: the labels an issue is installed with are
	// dated at CreatedAt, so a script dated before *that* is the fixture putting its
	// own log out of order, which is not what this is about.
	scripted := epochForTest.Add(72 * time.Hour)
	tr.SetHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventUnassigned, Actor: "a-human", Subject: "someone-else", At: scripted, ID: 50,
	})

	// Then the daemon claims it. That write must not sort before the script.
	if _, err := tr.Claim(ctx, core.Issue{Identifier: "1"}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := tr.SetStateLabels(ctx, core.Issue{Identifier: "1"}, core.StateLabelClaimed); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}

	events, err := tr.ClaimHistory(ctx, core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	var lastAt time.Time
	var lastID int64
	for i, ev := range events {
		if i > 0 && ev.At.Before(lastAt) {
			t.Errorf("event %d (%s) is dated %v, before the event that precedes it (%v) — "+
				"ClaimHistory promises (At, ID) order", i, ev.Kind, ev.At, lastAt)
		}
		if i > 0 && ev.ID <= lastID {
			t.Errorf("event %d id %d does not follow %d", i, ev.ID, lastID)
		}
		lastAt, lastID = ev.At, ev.ID
	}
	// And the claim that followed a scripted event is dated at least as late as it,
	// which is what makes the anchor derivable at all.
	if events[len(events)-1].At.Before(scripted) {
		t.Errorf("the last event is dated %v, before the scripted %v", events[len(events)-1].At, scripted)
	}
}

// The same guarantee under AppendHistory, which dates an event *without* moving the
// tracker's clock — so the per-issue clamp is the only thing holding the order.
func TestHistoryStaysOrderedAfterADatedAppend(t *testing.T) {
	ctx := context.Background()
	tr := NewTracker(Issue("1", epochForTest))

	if _, err := tr.Claim(ctx, core.Issue{Identifier: "1"}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// A human closes it, dated days later. AppendHistory takes the caller's time as
	// given and leaves the clock where it was.
	late := epochForTest.Add(96 * time.Hour)
	tr.AppendHistory("1", core.ClaimEvent{Kind: core.ClaimEventClosed, Actor: "a-human", At: late})

	// The daemon then releases. Stamped with the clock, that write would sort before
	// the close it followed.
	if err := tr.Release(ctx, core.Issue{Identifier: "1"}); err != nil {
		t.Fatalf("Release: %v", err)
	}

	events, err := tr.ClaimHistory(ctx, core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	last := events[len(events)-1]
	if last.Kind != core.ClaimEventUnassigned {
		t.Fatalf("last event is %s, want the release", last.Kind)
	}
	if last.At.Before(late) {
		t.Errorf("the release is dated %v, before the close it followed (%v) — a log in this order "+
			"makes `most recent standing assignment` meaningless (SPEC §9.10 step 2)", last.At, late)
	}
}

func TestExternalUnassignmentMovesStateAndHistoryTogether(t *testing.T) {
	ctx := context.Background()
	tr := NewTracker(Issue("1", epochForTest))
	tr.ClaimBy("1", "controller-bot")
	tr.UnassignBy("1", "CONTROLLER-BOT")

	issue, err := tr.Get(ctx, "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if containsFold(issue.Assignees, "controller-bot") {
		t.Errorf("assignees = %v, want the external assignment removed", issue.Assignees)
	}
	events, err := tr.ClaimHistory(ctx, core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	last := events[len(events)-1]
	if last.Kind != core.ClaimEventUnassigned || last.Subject != "CONTROLLER-BOT" {
		t.Errorf("last event = %+v, want the same external unassignment", last)
	}
}

// A scripted date moves the tracker's clock, so what it records *next* — for any
// issue — is dated coherently with it. Without that, one issue's history can be
// scripted into next week while another's ordinary writes are still stamped at the
// epoch, and a fixture reasoning about the two together is reasoning about a world
// no tracker produces.
func TestAScriptedDateMovesTheClockForEveryIssue(t *testing.T) {
	ctx := context.Background()
	tr := NewTracker(Issue("1", epochForTest), Issue("2", epochForTest))

	scripted := epochForTest.Add(72 * time.Hour)
	tr.SetHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventAssigned, Actor: "a-human", Subject: "someone-else", At: scripted, ID: 50,
	})

	// A different issue, with an empty history: nothing to clamp against, so the
	// clock is the only thing that can date this coherently.
	if _, err := tr.Claim(ctx, core.Issue{Identifier: "2"}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	events, err := tr.ClaimHistory(ctx, core.Issue{Identifier: "2"})
	if err != nil {
		t.Fatalf("ClaimHistory: %v", err)
	}
	claim := events[len(events)-1]
	if claim.Kind != core.ClaimEventAssigned {
		t.Fatalf("last event is %s, want the claim (events: %+v)", claim.Kind, events)
	}
	if claim.At.Before(scripted) {
		t.Errorf("issue 2's claim is dated %v, before a human action the fixture already scripted (%v)",
			claim.At, scripted)
	}
}
