package fake

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/mirror/mirrortest"
)

// The fake fact source is held to the same contract as the real store, because
// every test of the v2 checker is written against this one. A fake that
// answered where internal/mirror refuses would make the checker's whole suite
// agree with a verifier that skips the refusal.
func TestMirrorMeetsTheFactSourceContract(t *testing.T) {
	mirrortest.Contract(t, func(t *testing.T) mirrortest.Harness {
		m := NewMirror()
		return mirrortest.Harness{
			Store:   m,
			Commit:  func(_ *testing.T, branch string) string { return m.Commit(branch) },
			Rewrite: func(_ *testing.T, branch string) string { return m.Rewrite(branch) },
			Delete:  func(_ *testing.T, branch string) { m.DeleteBranch(branch) },
		}
	})
}

// The real mirror proves the durable claim record and pin before it obtains a
// credential or contacts the canonical remote. The fake must fail in that order
// too: callers retry credential failures but park a lost claim pin, so reversing
// them changes orchestration rather than only the error text.
func TestMirrorChecksTheClaimBeforeAnInjectedRemoteFailure(t *testing.T) {
	m := NewMirror()
	ref := core.RemoteClaimRef{Issue: "42", Key: "42", Epoch: 1}
	run := core.RemoteRunRef{Claim: ref, Run: "run-1", Verification: "verify-1"}
	remoteErr := errors.New("canonical remote unavailable")
	calls := 0
	m.FailFacts = func(core.RemoteRunRef) error {
		calls++
		return remoteErr
	}

	facts, err := m.RemoteFacts(context.Background(), run)
	if err == nil || errors.Is(err, remoteErr) {
		t.Fatalf("RemoteFacts for unrecorded claim = %+v, %v; want the claim refusal before the remote failure", facts, err)
	}
	if calls != 0 {
		t.Fatalf("remote failure seam called %d times for an unrecorded claim, want none", calls)
	}

	if _, err := m.RecordClaim(context.Background(), ref); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	if _, err := m.RemoteFacts(context.Background(), run); !errors.Is(err, remoteErr) {
		t.Fatalf("RemoteFacts for recorded claim = %v, want %v", err, remoteErr)
	}
	if calls != 1 {
		t.Fatalf("remote failure seam called %d times after the claim was recorded, want once", calls)
	}
}

// Re-recording one epoch is a local read of its existing pin. A remote failure
// can stop a new epoch from pinning, but cannot retroactively make an already
// durable RecordClaim fail.
func TestMirrorIdempotentRecordDoesNotReachAnInjectedRemoteFailure(t *testing.T) {
	m := NewMirror()
	first := core.RemoteClaimRef{Issue: "42", Key: "42", Epoch: 1}
	want, err := m.RecordClaim(context.Background(), first)
	if err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	remoteErr := errors.New("canonical remote unavailable")
	calls := 0
	m.FailRecord = func(core.RemoteClaimRef) error {
		calls++
		return remoteErr
	}

	got, err := m.RecordClaim(context.Background(), first)
	if err != nil || got != want {
		t.Fatalf("idempotent RecordClaim = %+v, %v; want %+v, nil", got, err, want)
	}
	if calls != 0 {
		t.Fatalf("remote record seam called %d times for an idempotent record, want none", calls)
	}

	second := core.RemoteClaimRef{Issue: "42", Key: "42", Epoch: 2}
	if _, err := m.RecordClaim(context.Background(), second); !errors.Is(err, remoteErr) {
		t.Fatalf("RecordClaim for a new epoch = %v, want %v", err, remoteErr)
	}
	if calls != 1 {
		t.Fatalf("remote record seam called %d times for a new epoch, want once", calls)
	}
}

// The forge's own two rules, neither of which the contract suite above covers:
// it answers about one branch of one repository, and it refuses to choose
// between two candidates.
func TestForgeReads(t *testing.T) {
	ctx := context.Background()
	f := NewForge()
	mine := f.Open(core.RemotePR{HeadBranch: "ben/42", HeadSHA: "abc", BaseBranch: "main"})
	f.Open(core.RemotePR{HeadBranch: "ben/43", HeadSHA: "def", BaseBranch: "main"})
	f.Open(core.RemotePR{HeadBranch: "ben/44", HeadSHA: "ghi", BaseBranch: "main", Repository: "github.test/other/repo"})

	tests := []struct {
		name       string
		repository string
		branch     string
		want       *core.RemotePR
	}{
		{name: "the branch's own pull request", repository: DefaultRepository, branch: "ben/42", want: &mine},
		{name: "a branch with none", repository: DefaultRepository, branch: "ben/99"},
		{name: "another repository's", repository: DefaultRepository, branch: "ben/44"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := f.RemotePR(ctx, core.RemotePRQuery{Repository: tt.repository, Branch: tt.branch})
			if err != nil {
				t.Fatalf("RemotePR: %v", err)
			}
			switch {
			case tt.want == nil && got != nil:
				t.Fatalf("RemotePR = %+v, want nil", got)
			case tt.want != nil && got == nil:
				t.Fatalf("RemotePR = nil, want #%d", tt.want.Number)
			case tt.want != nil && got.Number != tt.want.Number:
				t.Errorf("RemotePR = #%d, want #%d", got.Number, tt.want.Number)
			}
		})
	}

	t.Run("two candidates refuse rather than choose", func(t *testing.T) {
		f.Open(core.RemotePR{HeadBranch: "ben/42", HeadSHA: "xyz", BaseBranch: "main"})
		if _, err := f.RemotePR(ctx, core.RemotePRQuery{Repository: DefaultRepository, Branch: "ben/42"}); !errors.Is(err, core.ErrRemotePRAmbiguous) {
			t.Fatalf("RemotePR error = %v, want core.ErrRemotePRAmbiguous", err)
		}
	})
}
