package verify

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// Selection is the one decision that must not be influenced by a run, so it is
// asserted from both directions: the right verifier is chosen, and the wrong one
// is refused rather than substituted.

func remoteRun() core.RemoteRunRef {
	return core.RemoteRunRef{
		Claim:        core.RemoteClaimRef{Issue: remoteIssue, Key: remoteKey, Epoch: remoteEpoch},
		Run:          "run-1",
		Verification: "verify-1",
	}
}

// A local attempt reaches the local checker and nothing else. Asserted by
// driving it: the v1 seams record their calls, and the remote fixture's would
// record one too if selection had gone the other way.
func TestALocalAttemptIsVerifiedLocally(t *testing.T) {
	w := &fakeWorkspaces{facts: published()}
	tr := &fakeTracker{pr: openPR()}
	local := newChecker(t, w, tr)
	rf := newRemoteFixture(t)

	pub, err := SelectPublication(local, rf.checker, Attempt{Issue: core.Issue{Identifier: "7"}, Workspace: ws()})
	if err != nil {
		t.Fatalf("SelectPublication: %v", err)
	}
	got, err := pub.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Verdict != VerdictPublished {
		t.Errorf("Verify = %s (%q), want published", got.Verdict, got.Detail)
	}
	if got.RemoteFacts != (core.RemotePublishFacts{}) {
		t.Errorf("a local publication carried remote facts: %+v", got.RemoteFacts)
	}
	if w.calls != 1 || tr.calls != 1 {
		t.Errorf("the local seams were called %d and %d times, want 1 each", w.calls, tr.calls)
	}
	if len(rf.mirror.Requests) != 0 {
		t.Error("a local attempt reached the remote fact source")
	}
}

// A remote attempt reaches the remote checker, and the local seams are never
// touched — the workspace on disk is not evidence about a run that never used
// it, and a local verifier pointed at one would read a branch nobody wrote.
func TestARemoteAttemptIsVerifiedRemotely(t *testing.T) {
	w := &fakeWorkspaces{facts: published()}
	tr := &fakeTracker{pr: openPR()}
	local := newChecker(t, w, tr)
	rf := newRemoteFixture(t)
	head, _ := rf.publish()
	run := remoteRun()

	pub, err := SelectPublication(local, rf.checker, Attempt{Issue: core.Issue{Identifier: remoteIssue}, Remote: &run})
	if err != nil {
		t.Fatalf("SelectPublication: %v", err)
	}
	got, err := pub.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Verdict != VerdictPublished {
		t.Errorf("Verify = %s (%q), want published", got.Verdict, got.Detail)
	}
	wantFacts := core.RemotePublishFacts{
		Run: run, Repository: fake.DefaultRepository, Branch: remoteBranchName,
		BaseSHA: rf.base, RemoteHead: head, Fetched: true, DescendsBase: true,
		ObservedAt: fake.MirrorEpoch,
	}
	if got.RemoteFacts != wantFacts {
		t.Errorf("remote audit facts = %+v, want %+v", got.RemoteFacts, wantFacts)
	}
	if w.calls != 0 || tr.calls != 0 {
		t.Errorf("a remote attempt reached the local seams %d and %d times, want none", w.calls, tr.calls)
	}
	if len(rf.mirror.Requests) != 1 {
		t.Errorf("the remote fact source was asked %d times, want once", len(rf.mirror.Requests))
	}
}

// The sharp version of the same rule: a local workspace whose git facts say
// "published" cannot rescue a remote run that published nothing.
//
// This is the shape a forged sandbox filesystem would take if the local checker
// were ever reachable for a remote attempt — a worktree that looks like finished
// work, on a daemon whose canonical remote carries none of it. The verdict comes
// from the canonical remote, and the local seams are not asked.
func TestALocalWorkspaceCannotRescueARemoteRun(t *testing.T) {
	w := &fakeWorkspaces{facts: published()}
	tr := &fakeTracker{pr: openPR()}
	local := newChecker(t, w, tr)
	rf := newRemoteFixture(t) // nothing published on the canonical remote
	run := remoteRun()

	pub, err := SelectPublication(local, rf.checker, Attempt{Issue: core.Issue{Identifier: remoteIssue}, Remote: &run})
	if err != nil {
		t.Fatalf("SelectPublication: %v", err)
	}
	got, err := pub.Verify(context.Background())
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got.Verdict != VerdictContradicted {
		t.Fatalf("Verify = %s (%q), want contradicted", got.Verdict, got.Detail)
	}
	if w.calls != 0 || tr.calls != 0 {
		t.Errorf("the local seams were consulted (%d, %d) about a remote run", w.calls, tr.calls)
	}
}

func TestSelectPublicationRefusals(t *testing.T) {
	local := newChecker(t, &fakeWorkspaces{facts: published()}, &fakeTracker{pr: openPR()})
	mirror := fake.NewMirror()
	remote, err := NewRemote(mirror, mirror, fake.NewForge(), RemoteExpectation{Repository: fake.DefaultRepository})
	if err != nil {
		t.Fatalf("NewRemote: %v", err)
	}
	run := remoteRun()

	tests := []struct {
		name    string
		local   *Checker
		remote  *RemoteChecker
		attempt Attempt
		want    error
	}{
		{
			// The one that matters. Falling back to the local checker here would
			// verify a remote run against a workspace it never touched, which is
			// the sandbox verifying itself with extra steps.
			name:    "a remote attempt with no remote verifier",
			local:   local,
			attempt: Attempt{Issue: core.Issue{Identifier: remoteIssue}, Remote: &run},
			want:    ErrNoRemoteVerifier,
		},
		{
			name:    "a local attempt with no local verifier",
			remote:  remote,
			attempt: Attempt{Issue: core.Issue{Identifier: "7"}, Workspace: ws()},
			want:    ErrNoLocalVerifier,
		},
		{
			name:    "an attempt that names both substrates",
			local:   local,
			remote:  remote,
			attempt: Attempt{Issue: core.Issue{Identifier: remoteIssue}, Workspace: ws(), Remote: &run},
			want:    ErrAmbiguousSubstrate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SelectPublication(tt.local, tt.remote, tt.attempt)
			if !errors.Is(err, tt.want) {
				t.Fatalf("SelectPublication = %v, %v; want %v", got, err, tt.want)
			}
			if got != nil {
				t.Error("a refused selection returned a verifier")
			}
		})
	}

	t.Run("an attempt with no issue", func(t *testing.T) {
		if got, err := SelectPublication(local, remote, Attempt{Workspace: ws()}); err == nil {
			t.Fatalf("SelectPublication = %v, want a refusal", got)
		}
	})
}

// A remote verdict routes exactly as a local one does: the evidence moved, the
// state machine did not (SPEC §9.2, §9.6).
func TestRemoteResultProjectsOntoTheRoutingShape(t *testing.T) {
	f := newRemoteFixture(t)
	_, pr := f.publish()
	got, err := f.verify(t)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	want := Result{Verdict: VerdictPublished, PRURL: pr.URL}
	if got.Result() != want {
		t.Errorf("Result() = %+v, want %+v", got.Result(), want)
	}
}
