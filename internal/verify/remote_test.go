package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

const (
	remoteIssue      = "42"
	remoteKey        = "42"
	remoteEpoch      = int64(1)
	remoteBranchName = "ben/42"
	targetBranch     = "main"
)

// remoteFixture is one claim, its world, and a checker over both.
//
// The claim is recorded and nothing else: every case says for itself what the
// canonical remote and the forge then hold, because that is the whole of what a
// verdict may depend on.
type remoteFixture struct {
	mirror  *fake.Mirror
	forge   *fake.Forge
	checker *RemoteChecker
	claim   core.RemoteClaimRef
	base    string
}

// newRemoteFixture records the claim after running seed, which is how a case
// says what the canonical remote already held when the claim was taken — the
// handed-off branch of an earlier cycle, say. Everything after the claim is
// arranged by the case itself.
func newRemoteFixture(t *testing.T, seed ...func(*fake.Mirror)) *remoteFixture {
	t.Helper()
	f := &remoteFixture{
		mirror: fake.NewMirror(),
		forge:  fake.NewForge(),
		claim:  core.RemoteClaimRef{Issue: remoteIssue, Key: remoteKey, Epoch: remoteEpoch},
	}
	for _, s := range seed {
		s(f.mirror)
	}
	claim, err := f.mirror.RecordClaim(context.Background(), f.claim)
	if err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	f.base = claim.BaseSHA
	if f.checker, err = NewRemote(f.mirror, f.mirror, f.forge, RemoteExpectation{
		Repository: fake.DefaultRepository,
	}); err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	return f
}

// publish is the ordinary good case: the run pushed a commit on top of the pin
// and opened a pull request proposing exactly it.
func (f *remoteFixture) publish() (head string, pr core.RemotePR) {
	head = f.mirror.Commit(remoteBranchName)
	pr = f.forge.Open(core.RemotePR{
		HeadBranch: remoteBranchName, HeadSHA: head, BaseBranch: targetBranch,
		LinkStatus: core.RemoteIssueLinkStated, LinkedIssues: []string{remoteIssue},
	})
	return head, pr
}

func (f *remoteFixture) run() core.RemoteRunRef {
	return core.RemoteRunRef{Claim: f.claim, Run: "run-1", Verification: "verify-1"}
}

func (f *remoteFixture) verify(t *testing.T) (RemoteResult, error) {
	t.Helper()
	return f.checker.Verify(context.Background(), core.Issue{Identifier: remoteIssue}, f.run())
}

// rebound publishes honestly and then has the fact source answer under a
// different binding — the one channel through which a stale observation could
// reach a verdict.
func rebound(bend func(*core.RemoteRunRef)) func(*remoteFixture) {
	return func(f *remoteFixture) {
		f.publish()
		f.mirror.RewriteFacts = func(facts core.RemotePublishFacts) core.RemotePublishFacts {
			bend(&facts.Run)
			return facts
		}
	}
}

// misanswered publishes honestly and then has the forge answer a question
// nobody asked.
func misanswered(bend func(*core.RemotePR)) func(*remoteFixture) {
	return func(f *remoteFixture) {
		f.publish()
		f.forge.RewritePR = func(pr core.RemotePR) core.RemotePR {
			bend(&pr)
			return pr
		}
	}
}

// The three legs over daemon-side facts, one case per way the evidence can
// settle. Every arrangement is made in the world — the canonical remote and the
// forge — never in the checker, because the point of the v2 check is that the
// verdict is a function of what BEN can read for itself.
func TestRemoteVerify(t *testing.T) {
	tests := []struct {
		name string
		// arrange states what the canonical remote and the forge hold.
		arrange func(f *remoteFixture)
		want    Verdict
		// detail is a substring of the operator-facing line, asserted so a
		// verdict cannot be right for the wrong reason.
		detail  string
		wantErr error
	}{
		{
			name:    "a descendant, its exact head, and a matching open pull request",
			arrange: func(f *remoteFixture) { f.publish() },
			want:    VerdictPublished,
		},
		{
			name:    "a claim whose run published nothing",
			arrange: func(f *remoteFixture) {},
			want:    VerdictContradicted,
			detail:  "published nothing",
		},
		{
			name: "a force push onto unrelated history",
			arrange: func(f *remoteFixture) {
				f.mirror.Commit(remoteBranchName)
				head := f.mirror.Rewrite(remoteBranchName)
				f.forge.Open(core.RemotePR{HeadBranch: remoteBranchName, HeadSHA: head, BaseBranch: targetBranch})
			},
			want:   VerdictContradicted,
			detail: "history was rewritten",
		},
		{
			name: "a published branch with no open pull request",
			arrange: func(f *remoteFixture) {
				f.mirror.Commit(remoteBranchName)
			},
			want:   VerdictIncomplete,
			detail: "no open pull request",
		},
		{
			// The stale-pull-request and moving-head cases arrive here as one
			// shape and are treated as one, deliberately: from the daemon's side
			// they are indistinguishable — the forge proposes a commit the
			// canonical remote is not at — and there is no reading of the pair
			// that is safe to pass.
			name: "a pull request proposing a commit the canonical remote has moved past",
			arrange: func(f *remoteFixture) {
				stale := f.mirror.Commit(remoteBranchName)
				f.forge.Open(core.RemotePR{
					HeadBranch: remoteBranchName, HeadSHA: stale, BaseBranch: targetBranch,
					LinkStatus: core.RemoteIssueLinkStated, LinkedIssues: []string{remoteIssue},
				})
				f.mirror.Commit(remoteBranchName)
			},
			want:   VerdictContradicted,
			detail: "disagree",
		},
		{
			name: "a pull request from a fork",
			arrange: func(f *remoteFixture) {
				head := f.mirror.Commit(remoteBranchName)
				f.forge.Open(core.RemotePR{
					HeadBranch: remoteBranchName, HeadSHA: head, BaseBranch: targetBranch,
					HeadRepository: "github.test/someone/fork",
					LinkStatus:     core.RemoteIssueLinkStated, LinkedIssues: []string{remoteIssue},
				})
			},
			want:   VerdictContradicted,
			detail: "not from",
		},
		{
			name: "a pull request against another target branch",
			arrange: func(f *remoteFixture) {
				head := f.mirror.Commit(remoteBranchName)
				f.forge.Open(core.RemotePR{
					HeadBranch: remoteBranchName, HeadSHA: head, BaseBranch: "unprotected",
					LinkStatus: core.RemoteIssueLinkStated, LinkedIssues: []string{remoteIssue},
				})
			},
			want:   VerdictContradicted,
			detail: "targets unprotected",
		},
		{
			name: "a pull request the forge says closes another issue",
			arrange: func(f *remoteFixture) {
				head := f.mirror.Commit(remoteBranchName)
				f.forge.Open(core.RemotePR{
					HeadBranch: remoteBranchName, HeadSHA: head, BaseBranch: targetBranch,
					LinkStatus: core.RemoteIssueLinkStated, LinkedIssues: []string{"99"},
				})
			},
			want:   VerdictContradicted,
			detail: "closes [99]",
		},
		{
			name: "a pull request the forge says closes this issue",
			arrange: func(f *remoteFixture) {
				head := f.mirror.Commit(remoteBranchName)
				f.forge.Open(core.RemotePR{
					HeadBranch: remoteBranchName, HeadSHA: head, BaseBranch: targetBranch,
					LinkStatus: core.RemoteIssueLinkStated, LinkedIssues: []string{remoteIssue},
				})
			},
			want: VerdictPublished,
		},
		{
			name: "a forge that cannot finish enumerating",
			arrange: func(f *remoteFixture) {
				f.mirror.Commit(remoteBranchName)
				f.forge.FailPR = func(core.RemotePRQuery) error {
					return fmt.Errorf("listing page 2: %w", core.ErrRemotePRIncomplete)
				}
			},
			wantErr: core.ErrRemotePRIncomplete,
		},
		{
			name: "a forge with two open pull requests on the branch",
			arrange: func(f *remoteFixture) {
				head := f.mirror.Commit(remoteBranchName)
				f.forge.Open(core.RemotePR{HeadBranch: remoteBranchName, HeadSHA: head, BaseBranch: targetBranch})
				f.forge.Open(core.RemotePR{HeadBranch: remoteBranchName, HeadSHA: head, BaseBranch: targetBranch})
			},
			wantErr: core.ErrRemotePRAmbiguous,
		},
		{
			name: "a fact source serving a head it did not observe",
			arrange: func(f *remoteFixture) {
				f.publish()
				f.mirror.StaleFacts = true
			},
			wantErr: ErrRemoteMirrorStale,
		},
		{
			name:    "facts bound to another verification",
			arrange: rebound(func(run *core.RemoteRunRef) { run.Verification = "an-older-observation" }),
			wantErr: ErrRemoteFactsUnbound,
		},
		{
			name:    "facts bound to another attempt of the same claim",
			arrange: rebound(func(run *core.RemoteRunRef) { run.Run = "the-previous-attempt" }),
			wantErr: ErrRemoteFactsUnbound,
		},
		{
			name:    "facts bound to another claim epoch",
			arrange: rebound(func(run *core.RemoteRunRef) { run.Claim.Epoch = 2 }),
			wantErr: ErrRemoteFactsUnbound,
		},
		{
			name: "facts read from another repository",
			arrange: func(f *remoteFixture) {
				f.publish()
				f.mirror.RewriteFacts = func(facts core.RemotePublishFacts) core.RemotePublishFacts {
					facts.Repository = "github.test/someone/else"
					return facts
				}
			},
			wantErr: ErrRemoteRepositoryMismatch,
		},
		{
			name:    "a forge answering about another branch",
			arrange: misanswered(func(pr *core.RemotePR) { pr.HeadBranch = "ben/99" }),
			wantErr: ErrRemotePRContract,
		},
		{
			name:    "a forge answering with a merged pull request",
			arrange: misanswered(func(pr *core.RemotePR) { pr.State = "merged" }),
			wantErr: ErrRemotePRContract,
		},
		{
			name:    "a forge answering about another repository",
			arrange: misanswered(func(pr *core.RemotePR) { pr.Repository = "github.test/someone/else" }),
			wantErr: ErrRemotePRContract,
		},
		{
			name:    "a forge that does not state closing-issue linkage",
			arrange: misanswered(func(pr *core.RemotePR) { pr.LinkStatus = core.RemoteIssueLinkUnknown }),
			wantErr: ErrRemotePRContract,
		},
		{
			name:    "a forge that returns an invalid closing-issue status",
			arrange: misanswered(func(pr *core.RemotePR) { pr.LinkStatus = core.RemoteIssueLinkStatus(99) }),
			wantErr: ErrRemotePRContract,
		},
		{
			name:    "a forge that omits the pull request head",
			arrange: misanswered(func(pr *core.RemotePR) { pr.HeadSHA = "" }),
			wantErr: ErrRemotePRContract,
		},
		{
			name:    "a forge that returns an empty linked issue",
			arrange: misanswered(func(pr *core.RemotePR) { pr.LinkedIssues = []string{""} }),
			wantErr: ErrRemotePRContract,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newRemoteFixture(t)
			tt.arrange(f)
			got, err := f.verify(t)

			if tt.wantErr != nil || tt.want == VerdictUnknown {
				if err == nil {
					t.Fatalf("Verify = %+v, want a refusal", got)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Fatalf("Verify error = %v, want %v", err, tt.wantErr)
				}
				if got.Verdict != VerdictUnknown {
					t.Errorf("a refused verification returned verdict %s: verification that could not be "+
						"completed is never a verdict (SPEC §9.7)", got.Verdict)
				}
				return
			}
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if got.Verdict != tt.want {
				t.Fatalf("Verify = %s (%q), want %s", got.Verdict, got.Detail, tt.want)
			}
			if tt.detail != "" && !strings.Contains(got.Detail, tt.detail) {
				t.Errorf("detail = %q, want it to mention %q", got.Detail, tt.detail)
			}
			if tt.want == VerdictPublished {
				if got.PRURL == "" {
					t.Error("a published verdict carries no pull request link")
				}
				if got.Detail != "" {
					t.Errorf("a published verdict carries detail %q; the URL is the whole story", got.Detail)
				}
			}
			if got.Facts.Run != f.run() {
				t.Errorf("the result carries facts bound to %+v, want %+v", got.Facts.Run, f.run())
			}
		})
	}
}

// The no-op claim, which needs a world arranged before the claim was taken: the
// branch already stood at the pin, so a matching open pull request describes
// work from an earlier cycle and this run added nothing to it.
//
// The strongest form of the case, deliberately. Every leg but the first is
// satisfied — the branch is on the canonical remote, an open pull request
// proposes exactly its head, targets the right branch and lives in the right
// repository — and it is contradicted anyway. That is the difference the epoch
// pin buys: without it, an untouched publication is complete evidence for a
// claim whose run did nothing at all.
func TestANoOpClaimIsContradictedEvenWithAMatchingPullRequest(t *testing.T) {
	var handedOff string
	f := newRemoteFixture(t, func(m *fake.Mirror) { handedOff = m.Commit(remoteBranchName) })
	if f.base != handedOff {
		t.Fatalf("the claim pinned %s, the branch stood at %s: the fixture is not the case it claims",
			f.base, handedOff)
	}
	f.forge.Open(core.RemotePR{HeadBranch: remoteBranchName, HeadSHA: handedOff, BaseBranch: targetBranch})

	got, err := f.verify(t)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Verdict != VerdictContradicted {
		t.Fatalf("Verify = %s (%q), want contradicted", got.Verdict, got.Detail)
	}
	if !strings.Contains(got.Detail, "added no commits") {
		t.Errorf("detail = %q, want it to say the run added no commits", got.Detail)
	}
	if len(f.forge.Queries) != 0 {
		t.Error("the forge was asked about a claim the git legs already settled")
	}
}

// The credential case gets its own assertion: what matters is that the class
// survives, because §9.7's one exception to fail-closed — a transient credential
// failure retried in `verifying`, once per poll tick — is a decision the caller
// can only make from it.
func TestRemoteCredentialFailuresKeepTheirClass(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class core.CredentialErrorClass
	}{
		{"transient", core.CredentialTransient},
		{"permanent", core.CredentialPermanent},
		{"unknown", core.CredentialUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRemoteFixture(t)
			f.publish()
			f.mirror.FailFacts = func(core.RemoteRunRef) error {
				return &core.CredentialError{Class: tc.class, Authority: "octo:fetch"}
			}
			got, err := f.verify(t)
			if got.Verdict != VerdictUnknown {
				t.Fatalf("Verify = %s, want no verdict", got.Verdict)
			}
			class, ok := core.CredentialFailure(err)
			if !ok || class != tc.class {
				t.Fatalf("Verify error = %v, classified (%v, %v); want %v", err, class, ok, tc.class)
			}
		})
	}
}

// Nothing a run says reaches the verdict, because nothing a run says reaches the
// checker — and the assertion is about what the checker *asked for*, not only
// about what it concluded.
//
// A sandbox reporting triumphant success, a transcript describing a push, and a
// working tree that looks published are all consistent with this fixture: the
// canonical remote carries nothing. The verdict is contradicted, and everything
// the checker put to its fact source was the daemon's own binding — an issue, a
// claim epoch, an attempt, a verification. There is no field in that request a
// run could have filled, which is why a forged one has nothing to forge.
func TestAForgedSandboxSuccessChangesNothing(t *testing.T) {
	f := newRemoteFixture(t)
	if f.mirror.Head(remoteBranchName) != "" {
		t.Fatal("the fixture published something; this case asserts nothing")
	}

	got, err := f.verify(t)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Verdict != VerdictContradicted {
		t.Fatalf("Verify = %s (%q), want contradicted: the canonical remote carries nothing",
			got.Verdict, got.Detail)
	}
	if len(f.mirror.Requests) != 1 || f.mirror.Requests[0] != f.run() {
		t.Errorf("the fact source was asked %v, want exactly the daemon's own binding %+v",
			f.mirror.Requests, f.run())
	}
}

// The check reads the forge only once the daemon has already seen commits on the
// canonical remote. Leg order is cost as well as logic: a claim whose
// contradiction is visible in git must not depend on the forge to establish it,
// and must not spend a tracker request to be told so.
func TestTheForgeIsNotAskedAboutAClaimGitAlreadySettles(t *testing.T) {
	f := newRemoteFixture(t)
	if _, err := f.verify(t); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(f.forge.Queries) != 0 {
		t.Errorf("the forge was asked %d times about a claim with nothing on the canonical remote",
			len(f.forge.Queries))
	}
}

// The branch is derived from the claim, never taken from a fact source. Two
// computations of one name is the point: they must agree, and this is where a
// source that answered about a branch of its own choosing is caught.
func TestFactsAboutAnotherBranchRefuse(t *testing.T) {
	f := newRemoteFixture(t)
	f.publish()
	f.mirror.RewriteFacts = func(facts core.RemotePublishFacts) core.RemotePublishFacts {
		facts.Branch = "ben/somebody-else"
		return facts
	}
	got, err := f.verify(t)
	if !errors.Is(err, ErrRemoteBranchUnexpected) {
		t.Fatalf("Verify = %+v, %v; want ErrRemoteBranchUnexpected", got, err)
	}
}

// A verification whose issue and claim disagree is refused before anything is
// read. It is the one binding the caller supplies twice, so it is the one that
// can be checked without trusting either source.
func TestAMismatchedIssueRefuses(t *testing.T) {
	f := newRemoteFixture(t)
	f.publish()
	got, err := f.checker.Verify(context.Background(), core.Issue{Identifier: "99"}, f.run())
	if !errors.Is(err, ErrRemoteFactsUnbound) {
		t.Fatalf("Verify = %+v, %v; want ErrRemoteFactsUnbound", got, err)
	}
	if len(f.mirror.Requests) != 0 {
		t.Error("a mismatched verification reached the fact source")
	}
}

func TestRemoteVerificationReadsEachClaimsRetainedTarget(t *testing.T) {
	mirror := fake.NewMirror()
	forge := fake.NewForge()
	checker, err := NewRemote(mirror, mirror, forge, RemoteExpectation{Repository: fake.DefaultRepository})
	if err != nil {
		t.Fatal(err)
	}

	verifyClaim := func(issue, key, target string, epoch int64) {
		t.Helper()
		ref := core.RemoteClaimRef{Issue: issue, Key: key, Epoch: epoch}
		claim, err := mirror.RecordClaim(context.Background(), ref)
		if err != nil {
			t.Fatalf("RecordClaim(%s): %v", issue, err)
		}
		if claim.TargetBranch != target {
			t.Fatalf("claim %s target = %q, want %q", issue, claim.TargetBranch, target)
		}
		head := mirror.Commit(fake.RemoteBranch(key))
		forge.Open(core.RemotePR{
			HeadBranch: fake.RemoteBranch(key), HeadSHA: head, BaseBranch: target,
			LinkStatus: core.RemoteIssueLinkStated, LinkedIssues: []string{issue},
		})
		run := core.RemoteRunRef{Claim: ref, Run: "run-" + issue, Verification: "verify-" + issue}
		got, err := checker.Verify(context.Background(), core.Issue{Identifier: issue}, run)
		if err != nil || got.Verdict != VerdictPublished {
			t.Fatalf("Verify(%s) = %+v, %v; want published", issue, got, err)
		}
	}

	verifyClaim("41", "41", "main", 1)
	mirror.SetTargetBranch("release/v2")
	verifyClaim("42", "42", "release/v2", 1)

	first, err := mirror.Claim(context.Background(), core.RemoteClaimRef{Issue: "41", Key: "41", Epoch: 1})
	if err != nil || first.TargetBranch != "main" {
		t.Fatalf("first claim after selector movement = %+v, %v; want retained main", first, err)
	}
}

func TestNewRemoteRefusesAnIncompleteChecker(t *testing.T) {
	m, forge := fake.NewMirror(), fake.NewForge()
	full := RemoteExpectation{Repository: fake.DefaultRepository}
	tests := []struct {
		name   string
		claims RemoteClaimSource
		facts  RemoteFactSource
		prs    RemotePRs
		expect RemoteExpectation
	}{
		{name: "no claim source", facts: m, prs: forge, expect: full},
		{name: "no fact source", claims: m, prs: forge, expect: full},
		{name: "no forge", claims: m, facts: m, expect: full},
		{name: "no repository", claims: m, facts: m, prs: forge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := NewRemote(tt.claims, tt.facts, tt.prs, tt.expect); err == nil {
				t.Fatalf("NewRemote = %+v, want a refusal", got)
			}
		})
	}
}
