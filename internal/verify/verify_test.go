package verify

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

type fakeWorkspaces struct {
	facts core.PublishFacts
	err   error
	calls int
}

func (f *fakeWorkspaces) PublishFacts(context.Context, core.Workspace) (core.PublishFacts, error) {
	f.calls++
	return f.facts, f.err
}

type fakeTracker struct {
	pr    *core.PR
	err   error
	calls int
}

// FindPR returns exactly what the case supplied. It deliberately does not
// fill in Branch from the argument: a fake that helpfully completes the field
// under test agrees with the bug, which is how the missing branch check
// survived the first round of these tests.
func (f *fakeTracker) FindPR(context.Context, core.Issue, string) (*core.PR, error) {
	f.calls++
	return f.pr, f.err
}

const (
	base   = "1111111111111111111111111111111111111111"
	commit = "2222222222222222222222222222222222222222"
	other  = "3333333333333333333333333333333333333333"
)

func ws() core.Workspace {
	return core.Workspace{
		WorkspacePaths: core.WorkspacePaths{Path: "/tmp/ben/7"},
		Key:            "7", Branch: "ben/7", BaseSHA: base, TargetBranch: targetBranch,
	}
}

func openPR() *core.PR {
	return &core.PR{
		Number: 12, URL: "https://example.test/pull/12", State: "open",
		Branch: "ben/7", BaseBranch: targetBranch,
	}
}

// published is the facts shape where both git legs hold, so the tracker leg
// decides.
func published() core.PublishFacts {
	return core.PublishFacts{
		Head: commit, RemoteHead: commit, DescendsBase: true, RemoteProbed: true, RemoteHasHead: true,
	}
}

func newChecker(t *testing.T, w *fakeWorkspaces, tr *fakeTracker) *Checker {
	t.Helper()
	c, err := New(w, tr)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// The evidence-to-verdict map (SPEC §9.7). Each case is one shape of the three
// legs; the verdict is what the evidence supports, never what to do about it.
func TestVerifyVerdicts(t *testing.T) {
	tests := []struct {
		name        string
		facts       core.PublishFacts
		pr          *core.PR
		want        Verdict
		wantPRURL   string
		wantTracker int // FindPR calls: the git legs must settle their own cases
	}{
		{
			name:        "all three legs hold",
			facts:       published(),
			pr:          openPR(),
			want:        VerdictPublished,
			wantPRURL:   "https://example.test/pull/12",
			wantTracker: 1,
		},
		{
			name:  "no branch: the run left nothing",
			facts: core.PublishFacts{},
			want:  VerdictContradicted,
		},
		{
			// B09 acceptance 2: a success claim with zero commits behind it.
			name:  "branch still at its claim-time base",
			facts: core.PublishFacts{Head: base, DescendsBase: true},
			want:  VerdictContradicted,
		},
		{
			// B09 acceptance 3: the force-push shape. Commits exist, but the
			// base is not in their ancestry, so they are not evidence of this
			// daemon's work — even with a full remote branch and an open PR.
			name:  "branch does not descend from the base",
			facts: core.PublishFacts{Head: commit, RemoteHead: commit, RemoteProbed: true, RemoteHasHead: true},
			pr:    openPR(),
			want:  VerdictContradicted,
		},
		{
			name:  "committed but never pushed",
			facts: core.PublishFacts{Head: commit, DescendsBase: true, RemoteProbed: true},
			want:  VerdictIncomplete,
		},
		{
			name:  "pushed partially: origin does not carry the local head",
			facts: core.PublishFacts{Head: commit, RemoteHead: other, DescendsBase: true, RemoteProbed: true},
			want:  VerdictIncomplete,
		},
		{
			// B09 acceptance 1: the push landed, PR creation did not. The
			// continuation track re-dispatches to finish publishing (§9.6).
			name:        "pushed but no open pull request",
			facts:       published(),
			pr:          nil,
			want:        VerdictIncomplete,
			wantTracker: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &fakeWorkspaces{facts: tt.facts}
			tr := &fakeTracker{pr: tt.pr}
			got, err := newChecker(t, w, tr).Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.Verdict != tt.want {
				t.Errorf("Verdict = %s, want %s (detail: %q)", got.Verdict, tt.want, got.Detail)
			}
			if got.PRURL != tt.wantPRURL {
				t.Errorf("PRURL = %q, want %q", got.PRURL, tt.wantPRURL)
			}
			// Every verdict but published must explain itself: the line becomes
			// the needs-review comment an operator reads first (§8.4).
			if (got.Detail == "") != (tt.want == VerdictPublished) {
				t.Errorf("Detail = %q for verdict %s", got.Detail, got.Verdict)
			}
			if tr.calls != tt.wantTracker {
				t.Errorf("FindPR calls = %d, want %d — the git legs settle their own cases", tr.calls, tt.wantTracker)
			}
		})
	}
}

// Fail closed (SPEC §9.7): a leg that cannot be read is not a leg that failed.
// Both seams' errors must surface, and neither may leave a Result a caller
// could mistake for success.
func TestVerifyFailsClosed(t *testing.T) {
	boom := errors.New("boom")

	tests := []struct {
		name string
		w    *fakeWorkspaces
		tr   *fakeTracker
	}{
		{"git evidence unreadable", &fakeWorkspaces{err: boom}, &fakeTracker{pr: openPR()}},
		{"tracker unreachable", &fakeWorkspaces{facts: published()}, &fakeTracker{err: boom}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := newChecker(t, tt.w, tt.tr).Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
			if !errors.Is(err, boom) {
				t.Fatalf("Verify error = %v, want it to wrap %v", err, boom)
			}
			if got.Verdict == VerdictPublished {
				t.Error("a failed verification reported published")
			}
			if got != (Result{}) {
				t.Errorf("Verify() = %+v alongside an error, want the zero Result", got)
			}
		})
	}
}

// The zero Result must not read as success. Callers get (Result, error) and the
// error is authoritative, but a verdict whose zero value means "published"
// would make fail-closed a matter of the caller's diligence rather than of
// this package's contract.
func TestZeroResultIsNotPublished(t *testing.T) {
	if (Result{}).Verdict == VerdictPublished {
		t.Fatal("the zero Verdict is VerdictPublished: an ignored error would read as published evidence")
	}
	if got := (Result{}).Verdict; got != VerdictUnknown {
		t.Errorf("zero Verdict = %s, want %s", got, VerdictUnknown)
	}
}

// FindPR promises open PRs only (SPEC §8.2) so that a rejected PR from an
// earlier attempt cannot satisfy leg 3. A closed one arriving here is a broken
// adapter: refuse rather than pick a reading.
func TestVerifyRefusesANonOpenPR(t *testing.T) {
	for _, state := range []string{"closed", "merged", ""} {
		t.Run("state="+state, func(t *testing.T) {
			pr := openPR()
			pr.State = state
			got, err := newChecker(t, &fakeWorkspaces{facts: published()}, &fakeTracker{pr: pr}).
				Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
			if !errors.Is(err, ErrPRNotOpen) {
				t.Fatalf("Verify() = %+v, %v; want %v", got, err, ErrPRNotOpen)
			}
			if got.Verdict == VerdictPublished {
				t.Error("a non-open PR was accepted as publish evidence")
			}
		})
	}
}

// FindPR is asked for one branch. A PR on another is a different piece of
// work: accepting it would let any unrelated open PR — a second daemon's, an
// earlier issue's — supply leg 3 for a branch that never got one.
func TestVerifyRefusesAPRForAnotherBranch(t *testing.T) {
	for _, branch := range []string{"ben/8", "main", ""} {
		t.Run("branch="+branch, func(t *testing.T) {
			pr := openPR()
			pr.Branch = branch
			got, err := newChecker(t, &fakeWorkspaces{facts: published()}, &fakeTracker{pr: pr}).
				Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
			if !errors.Is(err, ErrPRBranchMismatch) {
				t.Fatalf("Verify() = %+v, %v; want %v", got, err, ErrPRBranchMismatch)
			}
			if got.Verdict == VerdictPublished {
				t.Errorf("a PR on %q was accepted as evidence for %q", branch, ws().Branch)
			}
		})
	}
}

func TestVerifyBindsThePullRequestToTheClaimTarget(t *testing.T) {
	t.Run("wrong target is contradictory evidence", func(t *testing.T) {
		pr := openPR()
		pr.BaseBranch = "unprotected"
		got, err := newChecker(t, &fakeWorkspaces{facts: published()}, &fakeTracker{pr: pr}).
			Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
		if err != nil {
			t.Fatalf("Verify: %v", err)
		}
		if got.Verdict != VerdictContradicted || !strings.Contains(got.Detail, "targets unprotected, not main") {
			t.Fatalf("Verify = %+v, want wrong-target contradiction", got)
		}
	})

	t.Run("missing target fact is a broken tracker contract", func(t *testing.T) {
		pr := openPR()
		pr.BaseBranch = ""
		got, err := newChecker(t, &fakeWorkspaces{facts: published()}, &fakeTracker{pr: pr}).
			Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
		if !errors.Is(err, ErrPRTargetMissing) || got != (Result{}) {
			t.Fatalf("Verify = %+v, %v; want zero result and ErrPRTargetMissing", got, err)
		}
	})

	t.Run("missing workspace target is non-authorizing", func(t *testing.T) {
		workspace := ws()
		workspace.TargetBranch = ""
		workspaces := &fakeWorkspaces{facts: published()}
		tracker := &fakeTracker{pr: openPR()}
		got, err := newChecker(t, workspaces, tracker).
			Verify(context.Background(), core.Issue{Identifier: "7"}, workspace)
		if !errors.Is(err, ErrTargetBranchMissing) || got != (Result{}) {
			t.Fatalf("Verify = %+v, %v; want zero result and ErrTargetBranchMissing", got, err)
		}
		if workspaces.calls != 0 || tracker.calls != 0 {
			t.Fatal("a targetless workspace reached an evidence source")
		}
	})

	t.Run("ambiguity is never reduced to a candidate", func(t *testing.T) {
		tracker := &fakeTracker{err: core.ErrPRAmbiguous}
		got, err := newChecker(t, &fakeWorkspaces{facts: published()}, tracker).
			Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
		if !errors.Is(err, core.ErrPRAmbiguous) || got != (Result{}) {
			t.Fatalf("Verify = %+v, %v; want zero result and ErrPRAmbiguous", got, err)
		}
	})
}

// The provider skips the remote probe only when leg 1 has already failed, so
// facts that clear leg 1 without a probe are a contract violation. Reading the
// unprobed RemoteHead as "not pushed" would be absence standing in for
// evidence, and would route a published run to the continuation track.
func TestVerifyRefusesUnprobedRemoteFacts(t *testing.T) {
	facts := published()
	facts.RemoteProbed = false
	got, err := newChecker(t, &fakeWorkspaces{facts: facts}, &fakeTracker{pr: openPR()}).
		Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
	if !errors.Is(err, ErrRemoteUnprobed) {
		t.Fatalf("Verify() = %+v, %v; want %v", got, err, ErrRemoteUnprobed)
	}
}

// The predicate that gates the provider's probe and the switch that produces
// leg 1's detail lines are two readings of one rule (§9.7 leg 1). If they ever
// disagree, verify would reach leg 2 on facts whose remote half was never
// gathered. Pin the agreement rather than trusting the two to stay in step.
func TestLegOneAgreesWithTheProbeGate(t *testing.T) {
	// Every leg-1-failing shape, each with the remote half deliberately filled
	// in as though a probe had happened and found full publication.
	for name, facts := range map[string]core.PublishFacts{
		"no branch":        {RemoteHead: commit, RemoteProbed: true, RemoteHasHead: true},
		"still at base":    {Head: base, DescendsBase: true, RemoteHead: base, RemoteProbed: true, RemoteHasHead: true},
		"does not descend": {Head: commit, RemoteHead: commit, RemoteProbed: true, RemoteHasHead: true},
	} {
		t.Run(name, func(t *testing.T) {
			if facts.AdvancedPastBase(base) {
				t.Fatalf("%+v: AdvancedPastBase(%s) = true, but this shape fails leg 1", facts, short(base))
			}
			got, err := newChecker(t, &fakeWorkspaces{facts: facts}, &fakeTracker{pr: openPR()}).
				Verify(context.Background(), core.Issue{Identifier: "7"}, ws())
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.Verdict != VerdictContradicted {
				t.Errorf("Verdict = %s, want %s: AdvancedPastBase is false, so leg 1 must have settled it",
					got.Verdict, VerdictContradicted)
			}
		})
	}
}

func TestNewRequiresBothSeams(t *testing.T) {
	tests := []struct {
		name string
		w    Workspaces
		tr   Tracker
	}{
		{"no seams", nil, nil},
		{"no workspaces", nil, &fakeTracker{}},
		{"no tracker", &fakeWorkspaces{}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if c, err := New(tt.w, tt.tr); err == nil {
				t.Fatalf("New() = %+v, nil; want a refusal", c)
			}
		})
	}
}

func TestVerdictString(t *testing.T) {
	for _, tt := range []struct {
		v    Verdict
		want string
	}{
		{VerdictUnknown, "unknown"},
		{VerdictPublished, "published"},
		{VerdictIncomplete, "incomplete"},
		{VerdictContradicted, "contradicted"},
		{Verdict(9), "Verdict(9)"},
	} {
		if got := tt.v.String(); got != tt.want {
			t.Errorf("Verdict(%d).String() = %q, want %q", int(tt.v), got, tt.want)
		}
	}
}
