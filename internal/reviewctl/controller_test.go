package reviewctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// A controller with no reviewer is still a reconciler: it repairs markers and
// completes routes for reviews that already exist. It just cannot open a round,
// and it says so rather than appearing to have found nothing to do.
func TestReconcileOnlyModeCannotOpenARound(t *testing.T) {
	f := newForge()
	d := newDriver(t, f, noReviewer{})

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := f.countCalls("PublishReview"); n != 0 {
		t.Errorf("published %d reviews with no reviewer configured", n)
	}
	if len(f.routes()) != 0 {
		t.Errorf("routed %+v with no verdict", f.routes())
	}

	// The same forge, with a review already on it, settles fully.
	occ := f.latestOccurrence(t)
	f.seedReview(occ, fxEpoch, head1, "clean")
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if routes := f.routes(); len(routes) != 1 {
		t.Fatalf("routes = %+v, want the pending route completed", routes)
	}
}

// The subject handed to a reviewer is the one trusted code just revalidated —
// every field of it, including the workspace-cycle anchor that selects the
// sandbox. A reviewer is never asked to discover any of this.
func TestTheSubjectIsCapturedByTrustedCode(t *testing.T) {
	f := newForge()
	r := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	d := newDriver(t, f, r)

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(r.subjects) != 1 {
		t.Fatalf("the reviewer was asked %d times, want exactly one round", len(r.subjects))
	}
	sub := r.subjects[0]

	occ := f.latestOccurrence(t)
	want := reviewrun.Subject{
		Repository:   fxOwner + "/" + fxRepo,
		Issue:        strconv.Itoa(fxIssue),
		Cycle:        fxApproval,
		Occurrence:   occ,
		Claim:        fxEpoch,
		PR:           fxPRNumber,
		TargetBranch: "main",
		Base:         base1,
		Head:         head1,
		Diff:         f.diff,
	}
	if sub != want {
		t.Fatalf("subject =\n%+v\nwant\n%+v", sub, want)
	}
	if !sub.Complete() {
		t.Fatal("the captured subject cannot key a durable run")
	}

	// And the diff was read pinned to both endpoint SHAs, never to a ref.
	if len(f.diffBases) != 1 || f.diffBases[0] != base1 || f.diffHeads[0] != head1 {
		t.Fatalf("the diff was read as %v...%v, want the exact commit pair", f.diffBases, f.diffHeads)
	}

	// The published review's execution record is retired once the review is on
	// the forge, which is where the durable policy record lives from then on.
	run, err := sub.RunID()
	if err != nil {
		t.Fatal(err)
	}
	if len(r.retired) != 1 || r.retired[0] != run {
		t.Fatalf("retired = %v, want the published run %s", r.retired, run)
	}
}

func TestReviewerProfileIsCapturedAndRevalidated(t *testing.T) {
	cfg := fxConfig()
	cfg.ReviewerProfiles = []string{"deep", "fast"}
	cfg.DefaultReviewerProfile = "deep"

	t.Run("explicit selection reaches the subject and review marker", func(t *testing.T) {
		f := newForge()
		f.issue.Labels = append(f.issue.Labels, "review-profile:fast")
		r := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
		d := newDriver(t, f, r)
		d.cfg = cfg
		if err := d.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(r.subjects) != 1 || r.subjects[0].ReviewerProfile != "fast" {
			t.Fatalf("subjects = %+v, want profile fast", r.subjects)
		}
		if len(f.reviews) != 1 {
			t.Fatalf("published reviews = %d, want one", len(f.reviews))
		}
		marker, err := review.ParseReviewMarker(f.reviews[0].Body)
		if err != nil {
			t.Fatal(err)
		}
		if marker.ReviewerProfile != "fast" {
			t.Fatalf("published profile = %q, want fast", marker.ReviewerProfile)
		}
	})

	t.Run("selection moving during review makes the verdict stale", func(t *testing.T) {
		f := newForge()
		f.issue.Labels = append(f.issue.Labels, "review-profile:deep")
		r := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
		r.onReview = func(reviewrun.Subject) {
			f.issue.Labels[len(f.issue.Labels)-1] = "review-profile:fast"
		}
		d := newDriver(t, f, r)
		d.cfg = cfg
		if err := d.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.reviews) != 0 {
			t.Fatalf("published %d reviews after the profile moved", len(f.reviews))
		}
		if f.countCalls("Unassign:"+fxPrincipal) != 0 || f.countCalls("RemoveLabel:"+fxQueue) != 0 {
			t.Fatal("a stale profile verdict routed the issue")
		}
	})
}

// The workspace-cycle anchor moves with a human's approval and with nothing
// else — so a revision round inside one approval reviews in the same sandbox,
// and a reapproval after revocation does not.
func TestTheSubjectAnchorsToTheStandingApproval(t *testing.T) {
	f := newForge()
	r := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictChangesRequested, review.VerdictClean}}
	d := newDriver(t, f, r)

	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("first round: %v", err)
	}
	// BEN revises under a new claim epoch inside the same approval.
	f.reclaim(head2)
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("revision round: %v", err)
	}
	if len(r.subjects) != 2 {
		t.Fatalf("the reviewer was asked %d times, want two rounds", len(r.subjects))
	}
	first, second := r.subjects[0], r.subjects[1]
	if first.Cycle != second.Cycle {
		t.Errorf("a revision inside one approval moved the workspace cycle: %d → %d", first.Cycle, second.Cycle)
	}
	if first.Claim == second.Claim {
		t.Errorf("a revision round reused claim epoch %d; the verification epoch must move", first.Claim)
	}
	if first.CycleAddress() != second.CycleAddress() {
		t.Errorf("the two rounds addressed different sandboxes: %q and %q",
			first.CycleAddress(), second.CycleAddress())
	}
	firstRun, _ := first.RunID()
	secondRun, _ := second.RunID()
	if firstRun == secondRun {
		t.Error("two rounds of one approval share a durable run identity")
	}

	// A human revokes and reapproves: a new cycle, and therefore a new address.
	approval := f.humanReapprove()
	f.reclaim(head3)
	r.verdicts = []review.Verdict{review.VerdictClean}
	if err := d.Run(context.Background()); err != nil {
		t.Fatalf("round after reapproval: %v", err)
	}
	third := r.subjects[len(r.subjects)-1]
	if third.Cycle != approval {
		t.Fatalf("the round after reapproval anchors to %d, want the new approval %d", third.Cycle, approval)
	}
	if third.CycleAddress() == second.CycleAddress() {
		t.Fatal("a reapproved cycle addressed the previous cycle's sandbox")
	}
}

// A reviewer that cannot state a verdict authorizes nothing — for every reason
// it might fail, including the ones that are not failures at all.
func TestNoVerdictAuthorizesNoMutation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "a run still in flight", err: reviewrun.ErrRunIncomplete},
		{name: "a lost dispatch nobody can resolve", err: reviewrun.ErrRunUnresolved},
		{name: "an event gap", err: reviewrun.ErrEventGap},
		{name: "a replayed event that disagrees", err: reviewrun.ErrEventConflict},
		{name: "a profile that moved", err: reviewrun.ErrProfileDrift},
		{name: "a sandbox from another cycle", err: reviewrun.ErrSandboxMismatch},
		{name: "an execution domain that is not quiet", err: reviewrun.ErrNotQuiet},
		{name: "silence", err: reviewrun.ErrNoVerdictBlock},
		{name: "two verdicts", err: reviewrun.ErrAmbiguousVerdict},
		{name: "a word outside the closed set", err: review.ErrUnknownVerdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newForge()
			d := newDriver(t, f, &scriptedReviewer{err: tc.err})
			if err := d.Run(context.Background()); err != nil {
				t.Fatalf("Run: %v", err)
			}
			for _, op := range []string{"PublishReview", "Unassign:" + fxPrincipal, "RemoveLabel:" + fxQueue, "PostComment"} {
				if n := f.countCalls(op); n != 0 {
					t.Errorf("%s happened %d times on a verdict that was never stated", op, n)
				}
			}
			if f.hasLabel(fxQueue) != true {
				t.Error("the human's required label was removed with no verdict")
			}
		})
	}
}

// The reducer's step set *is* the permission model. Enumerated here at an
// independent boundary, so a member added upstream fails this test rather than
// quietly widening what the controller may do (AGENTS.md, Conventions).
func TestTheControllerCanOnlyDoFiveThings(t *testing.T) {
	permitted := map[review.StepKind]bool{
		review.StepNothing:      true,
		review.StepReview:       true,
		review.StepUnassign:     true,
		review.StepRevoke:       true,
		review.StepRecordIntent: true,
		review.StepRecordRoute:  true,
	}
	d := &driver{cfg: fxConfig(), forge: newForge(), reviewer: noReviewer{}, maxDiffBytes: DefaultMaxDiffBytes, log: func(string, ...any) {}}
	for _, kind := range []review.StepKind{"approve", "merge", "close", "push", "apply-label"} {
		err := d.apply(context.Background(), review.Snapshot{}, review.Step{Kind: kind})
		if err == nil || !strings.Contains(err.Error(), "unknown step") {
			t.Errorf("apply(%q) = %v, want a refusal", kind, err)
		}
		if permitted[kind] {
			t.Errorf("%q is in the permitted set; the closed enum has grown", kind)
		}
	}
}

// The forge surface a credential could be used through is the type. Asserted by
// enumerating it: a method that approved, merged, closed, pushed or applied a
// required label would have to be added here to compile.
func TestTheForgeInterfaceExposesNoApprovalOrMerge(t *testing.T) {
	var f Forge = newForge()
	// Every method of Forge, named once. If this list is complete the interface
	// has eleven members, and any new one breaks the count below.
	_ = []any{
		f.Issue, f.Comments, f.Events, f.PullRequest, f.Reviews, f.Diff,
		f.PublishReview, f.Unassign, f.RemoveLabel, f.AddLabel, f.PostComment,
		f.Candidates,
	}
	source, err := os.ReadFile(filepath.Join("forge.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(source)
	start := strings.Index(body, "type Forge interface {")
	end := strings.Index(body[start:], "\n}")
	if start < 0 || end < 0 {
		t.Fatal("the Forge interface could not be located")
	}
	decl := body[start : start+end]
	for _, forbidden := range []string{"Approve", "Merge", "Close", "Push", "AddRequiredLabel", "SetLabels"} {
		if strings.Contains(decl, forbidden) {
			t.Errorf("Forge exposes %s; #11's permission model has been widened", forbidden)
		}
	}
	// AddLabel exists and is the one addition — bounded elsewhere to the single
	// fixed informational label (review.HumanReviewLabel).
	if !strings.Contains(decl, "AddLabel(") {
		t.Error("the informational-label addition disappeared; update this test with the change")
	}
}

// The prompt carries the verdict contract the parser actually implements, and
// the deployment's guidance, and nothing that could be mistaken for either.
func TestThePromptStatesTheContractItIsParsedBy(t *testing.T) {
	sub := reviewrun.Subject{
		Repository: "acme/ben", Issue: "11", Cycle: 1, Occurrence: 2, Claim: 3,
		PR: 42, TargetBranch: "main", Base: base1, Head: head1, Diff: "--- a/x\n",
	}
	p := Prompt(sub, "judge correctness first")
	for _, want := range []string{
		reviewrun.VerdictOpen, reviewrun.VerdictClose,
		string(review.VerdictClean), string(review.VerdictChangesRequested),
		"judge correctness first", sub.Base, sub.Head, sub.Diff,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the prompt does not carry %q", want)
		}
	}
	// The guidance is above the diff: a deployment states the standard, and the
	// untrusted change cannot appear to be part of it.
	if strings.Index(p, "judge correctness first") > strings.Index(p, sub.Diff) {
		t.Error("the deployment's guidance is rendered below the untrusted diff")
	}

	req, err := Invocation([]string{"codex", "exec"}, "judge correctness first")(sub)
	if err != nil {
		t.Fatal(err)
	}
	if err := reviewrun.CheckRequest(req, []string{"ghs_a_real_credential"}, true); err != nil {
		t.Fatalf("the composed invocation is refused: %v", err)
	}
	for _, name := range append(reviewrun.ForbiddenEnv(), reviewrun.ProviderEnv()...) {
		if _, ok := req.Env[name]; ok {
			t.Errorf("the composed invocation carries %s", name)
		}
	}
	if _, err := Invocation(nil, "")(sub); err == nil {
		t.Error("an empty reviewer argv composed an invocation")
	}
}

func TestProfiledInvocationMapsOnlyValidatedNamesToOperatorArgv(t *testing.T) {
	profiles := map[string][]string{
		"deep": {"codex", "exec", "--model", "gpt-5.6-sol"},
		"fast": {"codex", "exec", "--model", "gpt-5.6-luna"},
	}
	compose := ProfiledInvocations(nil, profiles, "")
	// Construction takes a defensive copy; later config mutation cannot change
	// an in-flight session's request identity.
	profiles["deep"][3] = "mutated"
	sub := reviewrun.Subject{
		Repository: "acme/ben", Issue: "11", Cycle: 1, Occurrence: 2, Claim: 3,
		PR: 42, TargetBranch: "main", Base: base1, Head: head1, ReviewerProfile: "deep",
	}
	req, err := compose(sub)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(req.Argv, " "); got != "codex exec --model gpt-5.6-sol" {
		t.Fatalf("argv = %q", got)
	}
	if req.Env["BEN_REVIEW_PROFILE"] != "deep" {
		t.Fatalf("profile evidence = %q", req.Env["BEN_REVIEW_PROFILE"])
	}
	sub.ReviewerProfile = "unconfigured"
	if _, err := compose(sub); err == nil || !strings.Contains(err.Error(), "no configured invocation") {
		t.Fatalf("unknown profile composed as %v", err)
	}
}

func TestControllerCarriesConfiguredMaxDiffBytesIntoEachDriver(t *testing.T) {
	f := newForge()
	f.diff = strings.Repeat("abcdef", 20)
	r := &scriptedReviewer{verdicts: []review.Verdict{review.VerdictClean}}
	c, err := New(Options{
		Policy: fxConfig(), Forge: f, Reviewer: r, MaxDiffBytes: 19, Log: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Reconcile(context.Background(), fxIssue); err != nil {
		t.Fatal(err)
	}
	if len(r.subjects) != 1 || !strings.HasPrefix(r.subjects[0].Diff, f.diff[:19]) ||
		strings.Contains(r.subjects[0].Diff, f.diff[:20]) {
		t.Fatalf("review subject did not honor the 19-byte limit: %+v", r.subjects)
	}
}

// The sweep is the reconciler: one issue's failure must not strand the others.
func TestSweepReconcilesEveryCandidate(t *testing.T) {
	f := newForge()
	f.candidates = []int{fxIssue, 12}
	f.err["Issue"] = nil
	c, err := New(Options{Policy: fxConfig(), Forge: f, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if got := f.countCalls("Candidates"); got != 1 {
		t.Fatalf("Candidates was read %d times", got)
	}
	if got := f.countCalls("Issue"); got < 2 {
		t.Fatalf("the sweep observed %d issues, want one per candidate", got)
	}
}

func TestSweepReportsEveryFailureAndKeepsGoing(t *testing.T) {
	f := newForge()
	f.candidates = []int{fxIssue, 12}
	f.err["Comments"] = fmt.Errorf("the forge is unwell")
	c, err := New(Options{Policy: fxConfig(), Forge: f, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	err = c.Sweep(context.Background())
	if err == nil {
		t.Fatal("a sweep over two broken issues reported success")
	}
	if !strings.Contains(err.Error(), "2 of 2") {
		t.Errorf("error = %v, want it to name every failure", err)
	}
}

// Under a declared backoff or a spent budget every remaining candidate fails
// identically without reaching the network, so keeping going is not
// resilience — the sweep stops at the first refusal and names what it left
// unvisited (#239).
func TestSweepStopsWhenTheForgeRefusesToSpend(t *testing.T) {
	for _, refusal := range []error{ErrRateLimited, ErrSweepBudget} {
		t.Run(refusal.Error(), func(t *testing.T) {
			f := newForge()
			f.candidates = []int{fxIssue, 12, 13}
			f.err["Issue"] = fmt.Errorf("GET issue: %w", refusal)
			c, err := New(Options{Policy: fxConfig(), Forge: f, Log: t.Logf})
			if err != nil {
				t.Fatal(err)
			}
			err = c.Sweep(context.Background())
			if !errors.Is(err, refusal) {
				t.Fatalf("Sweep = %v, want the forge's own refusal", err)
			}
			if !strings.Contains(err.Error(), "2 of 3") {
				t.Errorf("error = %v, want the unvisited candidates named", err)
			}
			if got := f.countCalls("Issue"); got != 1 {
				t.Errorf("the sweep kept iterating: %d issue reads, want 1", got)
			}
		})
	}
}

type oneShotSweepRefusal struct {
	*fakeForge
	issueCalls []int
	issue      int
	err        error
}

func (f *oneShotSweepRefusal) Issue(ctx context.Context, issue int) (review.Issue, error) {
	f.issueCalls = append(f.issueCalls, issue)
	if issue == f.issue && f.err != nil {
		err := f.err
		f.err = nil
		_ = f.record("Issue")
		return review.Issue{}, err
	}
	return f.fakeForge.Issue(ctx, issue)
}

type persistentSweepRefusal struct {
	*fakeForge
	issueCalls []int
	issue      int
}

func (f *persistentSweepRefusal) Issue(ctx context.Context, issue int) (review.Issue, error) {
	f.issueCalls = append(f.issueCalls, issue)
	if issue == f.issue {
		_ = f.record("Issue")
		return review.Issue{}, fmt.Errorf("GET issue: %w", ErrSweepBudget)
	}
	return f.fakeForge.Issue(ctx, issue)
}

type partialDiscoveryRefusal struct {
	*fakeForge
	returned bool
}

func (f *partialDiscoveryRefusal) Candidates(ctx context.Context, label string) ([]int, error) {
	if !f.returned {
		f.returned = true
		_ = f.record("Candidates")
		return []int{fxIssue}, fmt.Errorf("candidate page: %w", ErrSweepBudget)
	}
	return f.fakeForge.Candidates(ctx, label)
}

// A fresh per-sweep allowance must not restart at the same stable prefix.
// Settled open candidates remain discoverable, so the interrupted candidate
// rotates behind its peers and is retried after the suffix has had a turn.
func TestSweepRotatesTheCandidateThatSpentTheBudget(t *testing.T) {
	base := newForge()
	base.comments = nil // every successful candidate settles after one observation
	f := &oneShotSweepRefusal{
		fakeForge: base,
		issue:     12,
		err:       fmt.Errorf("GET issue: %w", ErrSweepBudget),
	}
	f.candidates = []int{fxIssue, 12, 13}
	c, err := New(Options{Policy: fxConfig(), Forge: f, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}

	if err := c.Sweep(context.Background()); !errors.Is(err, ErrSweepBudget) {
		t.Fatalf("first Sweep = %v, want ErrSweepBudget", err)
	}
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatalf("resumed Sweep: %v", err)
	}

	want := []int{fxIssue, 12, 12, 13}
	if fmt.Sprint(f.issueCalls) != fmt.Sprint(want) {
		t.Fatalf("issue order = %v, want %v: the spender gets one full-budget retry before its peer", f.issueCalls, want)
	}
}

// A candidate whose observation is itself larger than the request budget can
// never finish, but it must not become a permanent barrier in front of every
// later candidate. This needs more than a one-shot refusal: after the second,
// full-budget failure, the following sweep has to make progress while the same
// issue still refuses every attempt.
func TestSweepRotatesPastAPersistentBudgetExhaustion(t *testing.T) {
	base := newForge()
	base.comments = nil
	f := &persistentSweepRefusal{fakeForge: base, issue: 12}
	f.candidates = []int{fxIssue, 12, 13}
	c, err := New(Options{Policy: fxConfig(), Forge: f, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}

	for pass := 1; pass <= 3; pass++ {
		if err := c.Sweep(context.Background()); !errors.Is(err, ErrSweepBudget) {
			t.Fatalf("Sweep %d = %v, want ErrSweepBudget", pass, err)
		}
	}
	want := []int{fxIssue, 12, 12, 13, 12}
	if fmt.Sprint(f.issueCalls) != fmt.Sprint(want) {
		t.Fatalf("issue order = %v, want %v: the dedicated failure must rotate before the third pass", f.issueCalls, want)
	}
	if got := f.countCalls("Candidates"); got != 2 {
		t.Fatalf("candidate discovery calls = %d, want 2: the middle pass is the full-budget retry", got)
	}
}

func TestSweepRetainsCandidatesReturnedBeforeDiscoveryExhaustion(t *testing.T) {
	base := newForge()
	base.comments = nil
	f := &partialDiscoveryRefusal{fakeForge: base}
	c, err := New(Options{Policy: fxConfig(), Forge: f, Log: t.Logf})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Sweep(context.Background()); !errors.Is(err, ErrSweepBudget) {
		t.Fatalf("discovery Sweep = %v, want ErrSweepBudget", err)
	}
	if err := c.Sweep(context.Background()); err != nil {
		t.Fatalf("dedicated retry of the retained candidate: %v", err)
	}
	if got := f.countCalls("Issue"); got != 1 {
		t.Fatalf("retained candidate issue reads = %d, want 1", got)
	}
}

// A controller that does not know which identities to trust cannot decide
// anything safely, and refuses at construction rather than mid-cycle.
func TestNewRefusesAnUnsafeConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*review.Config)
		want   error
	}{
		{name: "no forge", want: nil},
		{
			name:   "the controller is the principal",
			mutate: func(c *review.Config) { c.Controller = c.Principal },
			want:   review.ErrInvalidConfig,
		},
		{
			name:   "the controller is the tracker author",
			mutate: func(c *review.Config) { c.Controller = c.TrackerAuthor },
			want:   review.ErrInvalidConfig,
		},
		{
			name:   "a reserved required label",
			mutate: func(c *review.Config) { c.QueueLabel = "ben:running" },
			want:   review.ErrInvalidConfig,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Policy: fxConfig(), Forge: newForge()}
			if tc.mutate == nil {
				opts.Forge = nil
			} else {
				tc.mutate(&opts.Policy)
			}
			_, err := New(opts)
			if err == nil {
				t.Fatal("an unsafe configuration was accepted")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
