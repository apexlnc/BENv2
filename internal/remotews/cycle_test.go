package remotews_test

import (
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
)

func TestCycleIdentityResumesASuspendedPublishedWorkspace(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	before := r.sandbox()
	claim := r.claim()
	if err := r.backend.Workspaces().Suspend(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if !r.backend.Suspended(claim) {
		t.Fatal("the published workspace was not suspended by the retained disposition")
	}

	got, target, err := r.provider.CycleIdentity(ctx, r.issue, claim.Epoch)
	if err != nil {
		t.Fatalf("CycleIdentity: %v", err)
	}
	if got != before {
		t.Fatalf("resumed identity = %+v, want %+v", got, before)
	}
	if target != "main" {
		t.Fatalf("resumed target = %q, want main", target)
	}
	if r.backend.Suspended(claim) {
		t.Fatal("CycleIdentity attached but did not resume the suspended sandbox")
	}
}

func TestCycleIdentityNeverResumesASupersededApproval(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	claim := r.claim()
	if err := r.backend.Workspaces().Suspend(ctx, claim); err != nil {
		t.Fatal(err)
	}
	acquires := r.backend.Acquires()

	// The tracker has accepted a replacement approval, but ordinary claim
	// assembly has not yet installed its new cycle record. The persisted @100
	// address is no longer standing and must not even be resumed transiently.
	r.cycles.set(200)
	if _, _, err := r.provider.CycleIdentity(ctx, r.issue, claim.Epoch); !errors.Is(err, remotews.ErrCycleState) {
		t.Fatalf("CycleIdentity after reapproval = %v, want ErrCycleState", err)
	}
	if got := r.backend.Acquires(); got != acquires {
		t.Fatalf("a superseded review cycle was resumed: acquires %d -> %d", acquires, got)
	}
	if !r.backend.Suspended(claim) {
		t.Fatal("the superseded sandbox was made executable again")
	}
}

func TestCycleIdentityNeverRecreatesADeletedPublishedWorkspace(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	claim := r.claim()
	acquires := r.backend.Acquires()
	if err := r.backend.Workspaces().Delete(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.provider.CycleIdentity(ctx, r.issue, claim.Epoch); !errors.Is(err, remote.ErrNoWorkspace) {
		t.Fatalf("CycleIdentity after delete = %v, want ErrNoWorkspace", err)
	}
	if got := r.backend.Acquires(); got != acquires {
		t.Fatalf("a deleted review cycle was reacquired: acquires %d -> %d", acquires, got)
	}
}

// The two clocks: the approval that selects a sandbox, and the assignment that
// pins a verification base. Every test here is about the boundary between them,
// because conflating them is the defect this strategy exists to avoid in both
// directions — one way discards a revision round's tree, the other way silently
// hands a new approval the previous cycle's.

// A controller-driven unassignment and reassignment inside one standing approval
// is an ordinary revision round: same sandbox, same pinned profile revision, a
// newly minted trusted base.
func TestReassignmentInsideOneApprovalReusesTheSandboxWithAFreshBase(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	if err := r.begin(11); err != nil {
		t.Fatalf("BeginClaimBase: %v", err)
	}
	first := r.mustPrepare(1, 11)
	if first.PriorWork {
		t.Fatal("the first claim epoch reports prior work")
	}
	firstSandbox := r.sandbox()

	// The run publishes, so the canonical remote moves. That movement is what
	// makes the second epoch's base different from the first's — and it is what a
	// sandbox keyed on the base would refuse to reattach over.
	published := r.mirror.Commit(remotews.Branch(first.Key))

	// The same approval; a new assignment event.
	if err := r.begin(12); err != nil {
		t.Fatalf("BeginClaimBase after reassignment: %v", err)
	}
	second := r.mustPrepare(1, 12)

	if second.BaseSHA == first.BaseSHA {
		t.Fatalf("the verification base did not move across the reassignment (still %s)", first.BaseSHA)
	}
	if second.BaseSHA != published {
		t.Fatalf("the reminted base is %s, want the canonical head %s", second.BaseSHA, published)
	}
	if second.ClaimEpoch != 12 {
		t.Fatalf("the workspace carries epoch %d, want 12", second.ClaimEpoch)
	}
	if !second.PriorWork {
		t.Fatal("the reassigned workspace did not report the earlier publication")
	}

	after := r.sandbox()
	if after.SandboxID != firstSandbox.SandboxID {
		t.Fatalf("the reassignment moved to sandbox %s, want the cycle's %s",
			after.SandboxID, firstSandbox.SandboxID)
	}
	if after.ProfileRevision != firstSandbox.ProfileRevision {
		t.Fatalf("the pinned profile revision moved from %s to %s",
			firstSandbox.ProfileRevision, after.ProfileRevision)
	}
	// The address the backend is keyed by carries the *cycle* base, unmoved. It
	// is what makes a reattach expressible at all: the real backend refuses an
	// acquire whose recorded branch, base or profile has changed under one claim
	// cycle (airlock.Workspaces.Acquire).
	if after.BaseSHA != first.BaseSHA {
		t.Fatalf("the sandbox address's base moved from %s to %s", first.BaseSHA, after.BaseSHA)
	}
	if second.Path != first.Path {
		t.Fatalf("the workspace address moved from %s to %s", first.Path, second.Path)
	}
	if got := r.backend.Acquires(); got != 2 {
		t.Fatalf("acquire was called %d times; both must address one cycle", got)
	}
	r.localIsUntouched()
}

// A successful publication commonly suspends rather than deletes. The cycle
// record must survive that policy: it carries the immutable acquisition base a
// reassignment under the same approval needs in order to reattach the real
// backend binding while minting a fresh verification base.
func TestReassignmentAfterASuspendedPublicationKeepsTheCycleIdentity(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.disposer.setDisposition(remotews.DispositionRetained)
	ctx := context.Background()

	if err := r.begin(11); err != nil {
		t.Fatalf("BeginClaimBase: %v", err)
	}
	first := r.mustPrepare(1, 11)
	firstSandbox := r.sandbox()
	published := r.mirror.Commit(remotews.Branch(first.Key))
	if err := r.provider.Dispose(ctx, first, false); err != nil {
		t.Fatalf("Dispose after publication: %v", err)
	}
	if _, found, err := r.provider.ResolveWorkspace(ctx, r.issue); err != nil || !found {
		t.Fatalf("suspended publication retained cycle = %v, %v; want true", found, err)
	}

	if err := r.begin(12); err != nil {
		t.Fatalf("BeginClaimBase after reassignment: %v", err)
	}
	second := r.mustPrepare(1, 12)
	if second.BaseSHA != published {
		t.Fatalf("new verification base = %s, want %s", second.BaseSHA, published)
	}
	if got := r.sandbox(); got.SandboxID != firstSandbox.SandboxID {
		t.Fatalf("reassignment attached sandbox %s, want %s", got.SandboxID, firstSandbox.SandboxID)
	}
	if got := r.sandbox().BaseSHA; got != first.BaseSHA {
		t.Fatalf("acquisition base moved to %s, want cycle base %s", got, first.BaseSHA)
	}
}

// Revocation followed by a new human approval is a new workspace cycle, and the
// retained sandbox from the previous one is unreachable from it — not by policy,
// but because its address is different.
func TestReapprovalDerivesANewCycleAndCannotAttachThePriorSandbox(t *testing.T) {
	t.Parallel()
	r := newRig(t)

	if err := r.begin(11); err != nil {
		t.Fatalf("BeginClaimBase: %v", err)
	}
	first := r.mustPrepare(1, 11)
	firstClaim := r.claim()
	firstSandbox := r.sandbox()
	r.mirror.Commit(remotews.Branch(first.Key))

	// The label was removed and a human applied it again: a later change-log
	// event, so a different anchor.
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatalf("BeginClaimBase after reapproval: %v", err)
	}
	second := r.mustPrepare(1, 21)
	if second.PriorWork {
		t.Fatal("a new approval inherited the superseded cycle's prior-work signal")
	}

	if second.Path == first.Path {
		t.Fatalf("the reapproval kept the previous cycle's address %s", first.Path)
	}
	secondClaim := r.claim()
	if secondClaim == firstClaim {
		t.Fatal("the reapproval kept the previous cycle's backend address")
	}
	if got := r.sandbox(); got.SandboxID == firstSandbox.SandboxID {
		t.Fatalf("the new cycle attached the retained sandbox %s", firstSandbox.SandboxID)
	}
	// And the previous cycle's sandbox is still there — the retention policy's to
	// release, never silently destroyed by somebody else's approval.
	if r.backend.Deleted(firstClaim) {
		t.Fatal("a new approval deleted the previous cycle's workspace")
	}
	r.localIsUntouched()
}

// A workspace cycle BEN cannot anchor is a sandbox BEN would have to guess at.
func TestBeginClaimBaseRefusesAnUnanchoredApproval(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.cycles.set(0)
	wantErr(t, r.begin(11), remotews.ErrApprovalUnknown)

	if _, err := r.provider.ClaimBase(context.Background(), r.issue); err != nil {
		t.Fatalf("ClaimBase after a refused begin: %v", err)
	}
}

// The §6.2 claim-base state machine, value for value with internal/workspace's.
func TestClaimBaseStateMachine(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()

	read := func() core.ClaimBase {
		t.Helper()
		state, err := r.provider.ClaimBase(ctx, r.issue)
		if err != nil {
			t.Fatalf("ClaimBase: %v", err)
		}
		return state
	}

	if got := read(); got.State != core.ClaimBaseAbsent {
		t.Fatalf("a fresh issue reads %s, want absent", got.State)
	}
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != core.ClaimBasePending || got.Epoch != 11 {
		t.Fatalf("after begin: %+v, want pending/11", got)
	}
	// Idempotent for the same epoch, and refused for another one while pending:
	// that shape needs a human rather than a guess about which assignment is
	// current.
	if err := r.begin(11); err != nil {
		t.Fatalf("a repeated begin for the same epoch: %v", err)
	}
	wantErr(t, r.begin(12), remotews.ErrCycleState)

	ws := r.mustPrepare(1, 11)
	pinned := read()
	if pinned.State != core.ClaimBasePinned || pinned.Epoch != 11 ||
		pinned.BaseSHA != ws.BaseSHA || pinned.TargetBranch != ws.TargetBranch {
		t.Fatalf("after prepare: %+v, want pinned/11/%s", pinned, ws.BaseSHA)
	}

	// A new assignment retains the outgoing pin, which is what lets §9.6 compare
	// against it before the new base is installed.
	if err := r.begin(12); err != nil {
		t.Fatal(err)
	}
	pending := read()
	if pending.State != core.ClaimBasePending || pending.Epoch != 12 ||
		pending.OutgoingEpoch != 11 || pending.OutgoingBaseSHA != ws.BaseSHA ||
		pending.OutgoingTargetBranch != ws.TargetBranch {
		t.Fatalf("after reassignment: %+v", pending)
	}

	// Abandoning rolls back to it rather than to absence.
	if err := r.provider.AbandonPendingClaimBase(ctx, r.issue); err != nil {
		t.Fatalf("AbandonPendingClaimBase: %v", err)
	}
	if got := read(); got.State != core.ClaimBasePinned || got.Epoch != 11 ||
		got.BaseSHA != ws.BaseSHA || got.TargetBranch != ws.TargetBranch {
		t.Fatalf("after abandon: %+v, want the outgoing pin restored", got)
	}
	// Pinned state is retained unchanged by a second abandon.
	if err := r.provider.AbandonPendingClaimBase(ctx, r.issue); err != nil {
		t.Fatal(err)
	}
	if got := read(); got.State != core.ClaimBasePinned || got.Epoch != 11 {
		t.Fatalf("abandoning a pinned record changed it: %+v", got)
	}
}

// A first claim abandoned before it ever pinned leaves nothing behind: no record,
// and therefore no workspace cycle for a later approval to inherit.
func TestAbandonBeforeAnyPinRetiresTheCycle(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	if err := r.provider.AbandonPendingClaimBase(ctx, r.issue); err != nil {
		t.Fatal(err)
	}
	if _, found, err := r.provider.ResolveWorkspace(ctx, r.issue); err != nil || found {
		t.Fatalf("ResolveWorkspace = %v, %v; want not found", found, err)
	}
	refs, err := r.provider.ListWorkspaces(ctx)
	if err != nil || len(refs) != 0 {
		t.Fatalf("ListWorkspaces = %v, %v; want empty", refs, err)
	}
}

// The pinned record is authority only while the trusted store still holds the
// commit it names. A pin that vanished must not authorize a verification whose
// leg 1 cannot be computed.
func TestClaimBaseRefusesAPinTheTrustedStoreHasLost(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)

	if err := r.mirror.Discard(ctx, core.RemoteClaimRef{Issue: issueID, Key: ws.Key, Epoch: 11}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.provider.ClaimBase(ctx, r.issue); err == nil {
		t.Fatal("a pinned record with no trusted pin was accepted")
	}
}

// ResolveWorkspace never asks the backend, so a claim whose sandbox is suspended
// or gone still has a workspace worth naming — which is what lets §9.7's evidence
// question be asked at all after a disposal.
func TestResolveWorkspaceDoesNotNeedTheBackend(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	if err := r.backend.Workspaces().Delete(ctx, r.claim()); err != nil {
		t.Fatal(err)
	}
	r.backend.SetUnreachable(true)

	got, found, err := r.provider.ResolveWorkspace(ctx, r.issue)
	if err != nil || !found {
		t.Fatalf("ResolveWorkspace = %+v, %v, %v", got, found, err)
	}
	if got.Key != ws.Key || got.ClaimEpoch != ws.ClaimEpoch || got.BaseSHA != ws.BaseSHA {
		t.Fatalf("ResolveWorkspace = %+v, want the recorded pair from %+v", got, ws)
	}
}

// The address is opaque, stable, and not a path. Stated as a test because the
// one thing a consumer must not do with it is treat it as a directory.
func TestTheWorkspaceAddressIsNotAPath(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	contains(t, ws.Path, "remote:")
	if ws.SharedGitDir != "" || ws.PrivateDir != "" {
		t.Fatalf("a remote workspace reported local directories: %+v", ws.WorkspacePaths)
	}
	if ws.Branch != remotews.Branch(ws.Key) {
		t.Fatalf("branch %q does not derive from key %q", ws.Branch, ws.Key)
	}
}

// A record written for one repository is never served to a strategy addressing
// another: a state directory shared by two workflows refuses rather than
// ordering one repository's work against the other's sandbox.
func TestARecordFromAnotherRepositoryIsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)

	other, err := remotews.New(remotews.Options{
		Repository:    "github.com/acme/other",
		GitRepository: "acme/other",
		Workspaces:    r.backend.Workspaces(), Processes: r.backend,
		Journals: r.journals, HookExec: r.backend,
		HookStore: remote.NewHookDirStore(t.TempDir()),
		Hooks:     remote.Hooks{Timeout: hookWindow},
		Base:      r.mirror, Cycles: r.cycles, Store: r.store,
		Disposer: r.disposer,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.ClaimBase(context.Background(), r.issue)
	wantErr(t, err, remotews.ErrCycleState)
}
