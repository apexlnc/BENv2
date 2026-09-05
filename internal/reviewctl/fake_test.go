package reviewctl

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// The fake forge models GitHub's *effects*, not just its reads: a write lands
// as the artifact and the change-log event a real write would produce, because
// every recovery rule in #11 is read back off exactly those. A fake that
// accepted an unassignment without recording an `unassigned` event would let
// the repair path pass while never being exercised (AGENTS.md, Conventions).
const (
	fxOwner      = "acme"
	fxRepo       = "ben"
	fxIssue      = 11
	fxPrincipal  = "ben-claim-bot"
	fxTracker    = "ben-tracker-bot"
	fxController = "ben-review-bot"
	fxQueue      = "ben-queue"
	fxHumanLabel = "human-review"
	fxPRNumber   = 42
	fxPRURL      = "https://github.com/acme/ben/pull/42"
	fxApproval   = int64(900)
	fxEpoch      = int64(7001)
)

func sha(seed string) string { return strings.Repeat(seed, 40/len(seed)+1)[:40] }

var (
	head1 = sha("a1")
	head2 = sha("b2")
	head3 = sha("c3")
	base1 = sha("e5")
	base2 = sha("f6")
)

type fakeForge struct {
	issue      review.Issue
	comments   []review.Comment
	events     []review.Event
	pr         *review.PullRequest
	reviews    []review.Review
	diff       string
	diffBases  []string
	diffHeads  []string
	candidates []int

	now    time.Time
	nextID int64
	calls  []string

	// onDiff runs when the diff is read, which is the window in which the
	// world can move under a reviewer.
	onDiff func(*fakeForge)
	err    map[string]error
}

func newForge() *fakeForge {
	start := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	f := &fakeForge{
		now:    start,
		nextID: 1000,
		diff:   "diff --git a/x b/x\n",
		issue: review.Issue{
			Number:    fxIssue,
			Labels:    []string{fxQueue},
			Assignees: []string{fxPrincipal},
		},
		pr: &review.PullRequest{
			Number: fxPRNumber, URL: fxPRURL, Branch: "ben/11",
			Head: head1, Base: "main", BaseSHA: base1, Body: "Fixes #11",
		},
		err: map[string]error{},
	}
	f.events = []review.Event{
		{ID: fxApproval, Type: review.EventLabeled, Actor: "a-human", Label: fxQueue, CreatedAt: start},
		{ID: fxEpoch, Type: review.EventAssigned, Actor: fxTracker, Assignee: fxPrincipal, CreatedAt: start.Add(time.Second)},
	}
	f.publishMilestone()
	return f
}

func (f *fakeForge) tick() time.Time {
	f.now = f.now.Add(time.Minute)
	return f.now
}

func (f *fakeForge) id() int64 {
	f.nextID++
	return f.nextID
}

// publishMilestone is BEN's side of the contract: the `unlabeled` transition
// that clears BEN's state projection anchors a published occurrence, and the
// milestone comment carries it (SPEC §8.4).
func (f *fakeForge) publishMilestone() int64 {
	occ := f.id()
	f.events = append(f.events, review.Event{
		ID: occ, Type: review.EventUnlabeled, Actor: fxTracker, Label: "ben:claimed", CreatedAt: f.tick(),
	})
	f.comments = append(f.comments, review.Comment{
		ID:     f.id(),
		Author: fxTracker,
		Body: "**BEN published a pull request.**\n\n- pull request: " + fxPRURL + "\n- daemon: `ben@canary`\n" +
			fmt.Sprintf("<!-- ben:milestone kind=published occurrence=%d -->\n", occ),
		CreatedAt: f.tick(),
	})
	return occ
}

// reclaim is BEN's side of a revise route: it observes the unassignment, is
// reassigned by its own claim act, pushes a new head and publishes again.
func (f *fakeForge) reclaim(head string) {
	f.events = append(f.events, review.Event{
		ID: f.id(), Type: review.EventAssigned, Actor: fxTracker, Assignee: fxPrincipal, CreatedAt: f.tick(),
	})
	f.issue.Assignees = []string{fxPrincipal}
	f.pr.Head = head
	f.publishMilestone()
}

func (f *fakeForge) humanReapprove() int64 {
	f.events = append(f.events, review.Event{
		ID: f.id(), Type: review.EventUnlabeled, Actor: "a-human", Label: fxQueue, CreatedAt: f.tick(),
	})
	var kept []string
	for _, label := range f.issue.Labels {
		if !strings.EqualFold(label, fxQueue) {
			kept = append(kept, label)
		}
	}
	f.issue.Labels = kept
	epoch := f.id()
	f.events = append(f.events, review.Event{
		ID: epoch, Type: review.EventLabeled, Actor: "a-human", Label: fxQueue, CreatedAt: f.tick(),
	})
	f.issue.Labels = append(f.issue.Labels, fxQueue)
	return epoch
}

// seedReview plants a review the way an interrupted run would have left it:
// on the forge, bound to a commit, with no route recorded.
func (f *fakeForge) seedReview(occurrence, claim int64, head string, v review.Verdict) {
	var approval int64
	var standing bool
	var found bool
	for _, ev := range review.SortEvents(f.events) {
		if strings.EqualFold(ev.Label, fxQueue) {
			switch ev.Type {
			case review.EventLabeled:
				approval, standing = ev.ID, true
			case review.EventUnlabeled:
				standing = false
			}
		}
		if ev.ID == occurrence {
			found = true
			break
		}
	}
	if !found || !standing {
		approval = 0
	}
	m := review.ReviewMarker{
		Occurrence: occurrence, Claim: claim, Approval: approval,
		Head: head, Base: f.pr.BaseSHA, Verdict: v,
	}
	f.reviews = append(f.reviews, review.Review{
		ID: f.id(), Author: fxController, Body: review.ReviewBody(m, "seeded"),
		CommitID: head, State: review.ReviewStateCommented, SubmittedAt: f.tick(),
	})
}

func (f *fakeForge) seedIntent(occurrence, claim int64, head string, outcome review.Outcome) {
	var approval int64
	for _, ev := range review.SortEvents(f.events) {
		if ev.Type == review.EventLabeled && strings.EqualFold(ev.Label, fxQueue) {
			approval = ev.ID
		}
		if ev.ID == occurrence {
			break
		}
	}
	m := review.RouteIntentMarker{
		Occurrence: occurrence,
		Claim:      claim,
		Approval:   approval,
		Head:       head,
		Outcome:    outcome,
	}
	f.comments = append(f.comments, review.Comment{
		ID: f.id(), Author: fxController, Body: review.RouteIntentBody(m, "seeded"), CreatedAt: f.tick(),
	})
}

func (f *fakeForge) latestOccurrence(t *testing.T) int64 {
	t.Helper()
	trigger, ok := review.LatestPublished(fxConfig(), f.comments)
	if !ok {
		t.Fatal("the fixture carries no published milestone")
	}
	return trigger.Occurrence
}

func (f *fakeForge) record(op string) error {
	f.calls = append(f.calls, op)
	return f.err[op]
}

func (f *fakeForge) Issue(context.Context, int) (review.Issue, error) {
	return f.issue, f.record("Issue")
}

func (f *fakeForge) Comments(context.Context, int) ([]review.Comment, error) {
	return append([]review.Comment(nil), f.comments...), f.record("Comments")
}

func (f *fakeForge) Events(context.Context, int) ([]review.Event, error) {
	return review.SortEvents(f.events), f.record("Events")
}

func (f *fakeForge) PullRequest(context.Context, int) (*review.PullRequest, error) {
	if err := f.record("PullRequest"); err != nil {
		return nil, err
	}
	if f.pr == nil {
		return nil, nil
	}
	copied := *f.pr
	return &copied, nil
}

func (f *fakeForge) Reviews(context.Context, int) ([]review.Review, error) {
	return append([]review.Review(nil), f.reviews...), f.record("Reviews")
}

func (f *fakeForge) Diff(_ context.Context, base, head string) (string, error) {
	if err := f.record("Diff"); err != nil {
		return "", err
	}
	f.diffBases = append(f.diffBases, base)
	f.diffHeads = append(f.diffHeads, head)
	if f.onDiff != nil {
		f.onDiff(f)
	}
	return f.diff, nil
}

func (f *fakeForge) PublishReview(_ context.Context, _ int, commitID, body string) error {
	if err := f.record("PublishReview"); err != nil {
		return err
	}
	f.reviews = append(f.reviews, review.Review{
		ID: f.id(), Author: fxController, Body: body,
		CommitID: commitID, State: review.ReviewStateCommented, SubmittedAt: f.tick(),
	})
	return nil
}

func (f *fakeForge) Unassign(_ context.Context, _ int, login string) error {
	if err := f.record("Unassign:" + login); err != nil {
		return err
	}
	var kept []string
	for _, a := range f.issue.Assignees {
		if !strings.EqualFold(a, login) {
			kept = append(kept, a)
		}
	}
	f.issue.Assignees = kept
	f.events = append(f.events, review.Event{
		ID: f.id(), Type: review.EventUnassigned, Actor: fxController, Assignee: login, CreatedAt: f.tick(),
	})
	return nil
}

func (f *fakeForge) RemoveLabel(_ context.Context, _ int, label string) error {
	if err := f.record("RemoveLabel:" + label); err != nil {
		return err
	}
	var kept []string
	for _, l := range f.issue.Labels {
		if l != label {
			kept = append(kept, l)
		}
	}
	f.issue.Labels = kept
	f.events = append(f.events, review.Event{
		ID: f.id(), Type: review.EventUnlabeled, Actor: fxController, Label: label, CreatedAt: f.tick(),
	})
	return nil
}

func (f *fakeForge) AddLabel(_ context.Context, _ int, label string) error {
	if err := f.record("AddLabel:" + label); err != nil {
		return err
	}
	f.issue.Labels = append(f.issue.Labels, label)
	f.events = append(f.events, review.Event{
		ID: f.id(), Type: review.EventLabeled, Actor: fxController, Label: label, CreatedAt: f.tick(),
	})
	return nil
}

func (f *fakeForge) PostComment(_ context.Context, _ int, body string) error {
	if err := f.record("PostComment"); err != nil {
		return err
	}
	f.comments = append(f.comments, review.Comment{
		ID: f.id(), Author: fxController, Body: body, CreatedAt: f.tick(),
	})
	return nil
}

func (f *fakeForge) Candidates(context.Context, string) ([]int, error) {
	if len(f.candidates) > 0 {
		return append([]int(nil), f.candidates...), f.record("Candidates")
	}
	return []int{fxIssue}, f.record("Candidates")
}

// routes returns every completed route the controller recorded, in order.
func (f *fakeForge) routes() []review.RouteMarker {
	var out []review.RouteMarker
	for _, c := range f.comments {
		if !strings.EqualFold(c.Author, fxController) {
			continue
		}
		if m, err := review.ParseRouteMarker(c.Body); err == nil {
			out = append(out, m)
		}
	}
	return out
}

type recordedIntent struct {
	marker    review.RouteIntentMarker
	createdAt time.Time
}

func (f *fakeForge) intents() []recordedIntent {
	var out []recordedIntent
	for _, c := range f.comments {
		if !strings.EqualFold(c.Author, fxController) {
			continue
		}
		if m, err := review.ParseRouteIntentMarker(c.Body); err == nil {
			out = append(out, recordedIntent{marker: m, createdAt: c.CreatedAt})
		}
	}
	return out
}

func (f *fakeForge) countCalls(op string) int {
	n := 0
	for _, c := range f.calls {
		if c == op {
			n++
		}
	}
	return n
}

func (f *fakeForge) hasLabel(label string) bool {
	for _, l := range f.issue.Labels {
		if l == label {
			return true
		}
	}
	return false
}

// scriptedReviewer answers with a fixed sequence of verdicts and records every
// subject it was handed — which is how "no duplicate review" and "the subject
// is bounded and exact" are both measured.
type scriptedReviewer struct {
	verdicts []review.Verdict
	err      error
	calls    int
	heads    []string
	subjects []reviewrun.Subject
	retired  []string
	onReview func(reviewrun.Subject)
}

func (r *scriptedReviewer) Review(_ context.Context, sub reviewrun.Subject) (review.Report, error) {
	r.calls++
	r.heads = append(r.heads, sub.Head)
	r.subjects = append(r.subjects, sub)
	if r.onReview != nil {
		r.onReview(sub)
	}
	if r.err != nil {
		return review.Report{}, r.err
	}
	if len(r.verdicts) == 0 {
		return review.Report{}, fmt.Errorf("the fixture ran out of verdicts at call %d", r.calls)
	}
	v := r.verdicts[0]
	r.verdicts = r.verdicts[1:]
	return review.Report{Verdict: v, Findings: "findings for " + sub.Head}, nil
}

func (r *scriptedReviewer) Retire(_ context.Context, sub reviewrun.Subject) error {
	run, err := sub.RunID()
	if err != nil {
		return err
	}
	r.retired = append(r.retired, run)
	return nil
}

func fxConfig() review.Config {
	return review.Config{
		Owner: fxOwner, Repo: fxRepo, Issue: fxIssue,
		Principal: fxPrincipal, TrackerAuthor: fxTracker, Controller: fxController,
		RequiredLabels: []string{fxQueue},
		QueueLabel:     fxQueue, AddHumanReviewLabel: true, RoundCap: 3,
	}
}

func newDriver(t *testing.T, f Forge, r Reviewer) *driver {
	t.Helper()
	return &driver{
		cfg: fxConfig(), forge: f, repository: fxOwner + "/" + fxRepo, reviewer: r,
		maxDiffBytes: DefaultMaxDiffBytes,
		log:          func(format string, args ...any) { t.Logf(format, args...) },
	}
}
