package reviewctl

import (
	"context"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// blocked returns every blocked-review statement the controller posted.
func (f *fakeForge) blocked() []review.ReviewBlockedMarker {
	var out []review.ReviewBlockedMarker
	for _, c := range f.comments {
		if !strings.EqualFold(c.Author, fxController) {
			continue
		}
		if m, err := review.ParseReviewBlockedMarker(c.Body); err == nil {
			out = append(out, m)
		}
	}
	return out
}

// A reviewer the substrate refused to start is stated on the issue exactly
// once per refused head and reason, and is never a review or a route (#284).
// The canary's review loop sat silent for five hours on a 413; the human whose
// label was standing had no way to know the loop had not skipped the pull
// request.
func TestARefusedReviewerIsStatedOnceOnTheIssue(t *testing.T) {
	f := newForge()
	rev := &scriptedReviewer{err: &reviewrun.RefusedError{
		Reason: "payload_too_large", Detail: "inline stdin exceeds the profile's limit",
	}}
	d := newDriver(t, f, rev)

	for sweep := 1; sweep <= 3; sweep++ {
		if err := d.Run(context.Background()); err != nil {
			t.Fatalf("sweep %d: %v", sweep, err)
		}
	}
	if n := f.countCalls("PublishReview"); n != 0 {
		t.Fatalf("published %d reviews for a reviewer that never started", n)
	}
	if routes := f.routes(); len(routes) != 0 {
		t.Fatalf("routed %+v for a reviewer that never started", routes)
	}
	if !f.hasLabel(fxQueue) || len(f.issue.Assignees) != 1 {
		t.Fatalf("the issue was mutated: %+v", f.issue)
	}
	if n := f.countCalls("PostComment"); n != 1 {
		t.Fatalf("blocked-review statements posted = %d across three sweeps, want exactly one", n)
	}
	blocked := f.blocked()
	if len(blocked) != 1 {
		t.Fatalf("blocked markers = %+v, want one", blocked)
	}
	want := review.ReviewBlockedMarker{
		Occurrence: f.latestOccurrence(t), Claim: fxEpoch, Approval: fxApproval,
		Head: head1, Reason: "payload_too_large",
	}
	if blocked[0] != want {
		t.Fatalf("blocked marker = %+v, want %+v", blocked[0], want)
	}
	body := f.comments[len(f.comments)-1].Body
	if !strings.Contains(body, "inline stdin exceeds the profile's limit") || !strings.Contains(body, "not a verdict") {
		t.Fatalf("the statement does not tell the human what happened or what it is not:\n%s", body)
	}
	if rev.calls != 3 {
		t.Fatalf("the reviewer was offered the subject %d times, want once per sweep", rev.calls)
	}

	// A different reason is a different fact, and gets its own statement.
	rev.err = &reviewrun.RefusedError{Reason: "env_rejected", Detail: "a reserved environment key"}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.countCalls("PostComment"); n != 2 {
		t.Fatalf("statements after a changed reason = %d, want two", n)
	}

	// Once the substrate admits the run, the review proceeds and routes; the
	// statements are history and block nothing.
	rev.err = nil
	rev.verdicts = []review.Verdict{review.VerdictClean}
	if err := d.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := f.countCalls("PublishReview"); n != 1 {
		t.Fatalf("publications once admitted = %d, want one", n)
	}
	if routes := f.routes(); len(routes) != 1 || routes[0].Outcome != review.OutcomeHumanReview {
		t.Fatalf("routes once admitted = %+v, want one human-review", routes)
	}
	if n := f.countCalls("PostComment"); n != 3 {
		t.Fatalf("PostComment calls = %d; want the two statements plus the route", n)
	}
	assertNoApproval(t, f)
}

// A refusal the marker cannot carry — no reason, or one that is not a token —
// is still a refusal: it stops the round like any other unusable verdict and
// is logged, but nothing unparseable is posted under the controller's name.
func TestARefusalTheMarkerCannotCarryIsLoggedNotPosted(t *testing.T) {
	for name, err := range map[string]error{
		"bare":            reviewrun.ErrRunRefused,
		"not a token":     &reviewrun.RefusedError{Reason: "Payload Too Large"},
		"marker breaking": &reviewrun.RefusedError{Reason: "a-->b"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newForge()
			if err := newDriver(t, f, &scriptedReviewer{err: err}).Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			if n := f.countCalls("PostComment") + f.countCalls("PublishReview"); n != 0 {
				t.Fatalf("%d forge writes for a refusal the marker cannot carry", n)
			}
			if !f.hasLabel(fxQueue) || len(f.issue.Assignees) != 1 {
				t.Fatalf("the issue was mutated: %+v", f.issue)
			}
		})
	}
}

// The bound an operator writes is on the diff; the bound a substrate enforces
// is on the prompt. PromptCeiling is the honest conversion, and it has to count
// everything the composed prompt carries beyond the diff.
func TestPromptCeilingCountsWhatTheDiffBoundDoesNot(t *testing.T) {
	guidance := strings.Repeat("g", 2000)
	const maxDiff = 60000
	ceiling := PromptCeiling("acme/widgets", guidance, maxDiff)
	if ceiling <= maxDiff+len(guidance) {
		t.Fatalf("ceiling %d does not exceed diff bound %d plus guidance %d: the framing was not counted", ceiling, maxDiff, len(guidance))
	}
	// No real subject under the bound composes a larger prompt.
	sub := reviewrun.Subject{
		Repository: "acme/widgets", Issue: "11", PR: 42, Base: base1, Head: head1,
		Diff: BoundDiff(strings.Repeat("x", maxDiff+5000), maxDiff),
	}
	if got := len(Prompt(sub, guidance)); got > ceiling {
		t.Fatalf("a real prompt of %d bytes exceeds the ceiling %d", got, ceiling)
	}
	if narrower := PromptCeiling("a/b", "", maxDiff); narrower >= ceiling {
		t.Fatalf("a shorter repository and no guidance composed a ceiling of %d, not below %d", narrower, ceiling)
	}

	if got := BoundDiff("abc", 5); got != "abc" {
		t.Fatalf("BoundDiff under the bound = %q, want the diff untouched", got)
	}
	bounded := BoundDiff(strings.Repeat("y", 10), 5)
	if !strings.HasPrefix(bounded, "yyyyy") || strings.HasPrefix(bounded, "yyyyyy") || !strings.Contains(bounded, "truncated") {
		t.Fatalf("BoundDiff over the bound = %q; want five bytes and a stated truncation", bounded)
	}
	if len(bounded) != 5+len(diffTruncationNotice) {
		t.Fatalf("BoundDiff length = %d, want the bound plus the notice", len(bounded))
	}
}
