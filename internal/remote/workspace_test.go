package remote_test

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The workspace lifecycle: idempotent per claim cycle, pinned to an immutable
// profile revision, and untouchable while a run's termination is unconfirmed.

// Every workspace verb is idempotent over one claim cycle, because BEN retries
// them across restarts with no memory of whether the previous call landed —
// which is the situation SPEC §9.10 is written for.
func TestWorkspaceVerbsAreIdempotentPerClaimCycle(t *testing.T) {
	rig := newRig(t)
	ws := rig.backend.Workspaces()
	ctx := context.Background()
	req := remote.AcquireRequest{Claim: rig.claim, Branch: testBranch, BaseSHA: testBaseSHA}

	first, err := ws.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	second, err := ws.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	if first != second {
		t.Fatalf("two Acquires produced two workspaces:\n %+v\n %+v", first, second)
	}

	attached, err := ws.Attach(ctx, rig.claim)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if attached != first {
		t.Errorf("Attach returned %+v, want the acquired identity %+v", attached, first)
	}

	// Suspension releases the warm sandbox and keeps the identity, so a later
	// Acquire resumes rather than rebuilds.
	if err := ws.Suspend(ctx, rig.claim); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if err := ws.Suspend(ctx, rig.claim); err != nil {
		t.Fatalf("second Suspend: %v", err)
	}
	if !rig.backend.Suspended(rig.claim) {
		t.Fatal("the workspace is not suspended")
	}
	resumed, err := ws.Acquire(ctx, req)
	if err != nil {
		t.Fatalf("Acquire after Suspend: %v", err)
	}
	if resumed != first {
		t.Errorf("resuming a suspended workspace produced %+v, want %+v", resumed, first)
	}

	if err := ws.Delete(ctx, rig.claim); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := ws.Delete(ctx, rig.claim); err != nil {
		t.Errorf("a repeated Delete = %v, want nil — a crash between disposal steps leaves exactly this", err)
	}
	if _, err := ws.Attach(ctx, rig.claim); !errors.Is(err, remote.ErrNoWorkspace) {
		t.Errorf("Attach after Delete = %v, want %v", err, remote.ErrNoWorkspace)
	}
}

// A different claim cycle is a different workspace, even for the same issue.
//
// The whole reason the key is the cycle and not the issue: a workspace acquired
// under the previous claim carries the previous verification base (SPEC §6.2), so
// reusing it would hand a fresh claim a tree pinned to work nobody re-approved.
func TestANewClaimEpochIsANewWorkspace(t *testing.T) {
	rig := newRig(t)
	ws := rig.backend.Workspaces()
	ctx := context.Background()

	first, err := ws.Acquire(ctx, remote.AcquireRequest{Claim: rig.claim, Branch: testBranch, BaseSHA: testBaseSHA})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	next := rig.claim
	next.Epoch++
	second, err := ws.Acquire(ctx, remote.AcquireRequest{Claim: next, Branch: testBranch, BaseSHA: "0000000000000000000000000000000000000002"})
	if err != nil {
		t.Fatalf("Acquire for the next epoch: %v", err)
	}
	if first.SandboxID == second.SandboxID {
		t.Errorf("both claim cycles got sandbox %q", first.SandboxID)
	}
	if second.BaseSHA == first.BaseSHA {
		t.Error("the new claim's workspace carries the previous claim's verification base")
	}
}

// Idempotence is for one exact acquisition request, not merely for anything
// carrying the same claim address. The real backend binds branch and base on
// first acquisition; the contract fake must refuse the same drift or a consumer
// can pass here and fail only when it reaches production.
func TestOneClaimCycleRefusesADifferentBranchOrBase(t *testing.T) {
	rig := newRig(t)
	ws := rig.backend.Workspaces()
	ctx := context.Background()
	req := remote.AcquireRequest{Claim: rig.claim, Branch: testBranch, BaseSHA: testBaseSHA}
	if _, err := ws.Acquire(ctx, req); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	for _, tc := range []struct {
		name string
		req  remote.AcquireRequest
	}{
		{name: "branch", req: remote.AcquireRequest{Claim: rig.claim, Branch: "ben/other", BaseSHA: testBaseSHA}},
		{name: "base", req: remote.AcquireRequest{Claim: rig.claim, Branch: testBranch, BaseSHA: "0000000000000000000000000000000000000002"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ws.Acquire(ctx, tc.req); !errors.Is(err, remote.ErrClaimMismatch) {
				t.Fatalf("Acquire after %s drift = %v, want %v", tc.name, err, remote.ErrClaimMismatch)
			}
		})
	}
}

// A pinned profile revision the backend can no longer serve is refused, never
// substituted.
//
// Substituting is the silent form of the hazard the field exists to close: the
// sandbox id would still match while the world it names had changed, and "attach
// to the same world" would be unprovable.
func TestAPinnedProfileRevisionIsRefusedRatherThanSubstituted(t *testing.T) {
	rig := newRig(t)
	ws := rig.backend.Workspaces()
	ctx := context.Background()

	if _, err := ws.Acquire(ctx, remote.AcquireRequest{
		Claim: rig.claim, Branch: testBranch, BaseSHA: testBaseSHA, ProfileRevision: "a-revision-that-never-existed",
	}); err == nil {
		t.Fatal("a fresh Acquire accepted a profile revision the backend does not have")
	}

	id, err := ws.Acquire(ctx, remote.AcquireRequest{Claim: rig.claim, Branch: testBranch, BaseSHA: testBaseSHA})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if id.ProfileRevision != testProfile {
		t.Fatalf("ProfileRevision = %q, want %q", id.ProfileRevision, testProfile)
	}
	// Reattaching under the recorded revision is the ordinary restart, and must
	// return the same workspace.
	again, err := ws.Acquire(ctx, remote.AcquireRequest{
		Claim: rig.claim, Branch: testBranch, BaseSHA: testBaseSHA, ProfileRevision: id.ProfileRevision,
	})
	if err != nil {
		t.Fatalf("Acquire pinned to the recorded revision: %v", err)
	}
	if again != id {
		t.Errorf("pinned Acquire returned %+v, want %+v", again, id)
	}
	// And under a revision that is not this workspace's, it refuses.
	if _, err := ws.Acquire(ctx, remote.AcquireRequest{
		Claim: rig.claim, Branch: testBranch, BaseSHA: testBaseSHA, ProfileRevision: "profile-rev-2",
	}); err == nil {
		t.Error("a pinned Acquire accepted a revision that is not this workspace's")
	}
}

// The gate: nothing touches a workspace until the run in it is confirmed gone.
func TestAWorkspaceIsNotTouchedUntilTerminationIsConfirmed(t *testing.T) {
	quiet := remote.Status{Phase: remote.PhaseQuiet, Domain: remote.DomainStateQuiet, Reachable: true}
	for _, tc := range []struct {
		name   string
		status remote.Status
		refuse bool
	}{
		{"a live run", remote.Status{Phase: remote.PhaseRunning, Reachable: true}, true},
		{"a signaled run", remote.Status{Phase: remote.PhaseSignaled, Reachable: true}, true},
		{"an unreachable backend", remote.Status{}, true},
		{"a confirmed-quiet run", quiet, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t)
			ws := rig.backend.Workspaces()
			ctx := context.Background()
			req := remote.AcquireRequest{Claim: rig.claim, Branch: testBranch, BaseSHA: testBaseSHA}
			if _, err := ws.Acquire(ctx, req); err != nil {
				t.Fatalf("Acquire: %v", err)
			}

			_, reacquireErr := remote.Reacquire(ctx, ws, req, tc.status)
			disposeErr := remote.Dispose(ctx, ws, rig.claim, tc.status)
			if tc.refuse {
				if !errors.Is(reacquireErr, remote.ErrNotQuiet) {
					t.Errorf("Reacquire = %v, want %v", reacquireErr, remote.ErrNotQuiet)
				}
				if !errors.Is(disposeErr, remote.ErrNotQuiet) {
					t.Errorf("Dispose = %v, want %v", disposeErr, remote.ErrNotQuiet)
				}
				if rig.backend.Deleted(rig.claim) {
					t.Error("the workspace was deleted although the run's termination is unconfirmed")
				}
				return
			}
			if reacquireErr != nil {
				t.Errorf("Reacquire over a confirmed-quiet run = %v, want nil", reacquireErr)
			}
			if disposeErr != nil {
				t.Errorf("Dispose over a confirmed-quiet run = %v, want nil", disposeErr)
			}
			if !rig.backend.Deleted(rig.claim) {
				t.Error("Dispose reported success and deleted nothing")
			}
		})
	}
}
