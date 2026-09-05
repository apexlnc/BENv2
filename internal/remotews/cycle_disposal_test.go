package remotews_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
)

func endedCycles(t *testing.T, p *remotews.Provider) []core.WorkspaceRef {
	t.Helper()
	refs, err := p.EndedCycles(context.Background())
	if err != nil {
		t.Fatalf("EndedCycles: %v", err)
	}
	return refs
}

func endedWorkspace(ref core.WorkspaceRef) core.Workspace {
	return core.Workspace{Key: ref.Key, WorkspacePaths: core.WorkspacePaths{Path: ref.Path}}
}

// The old address is a record of its own, not a breadcrumb inside the cycle
// that replaced it. A fresh provider can therefore enumerate and discharge it
// while the replacement remains live and pinned.
func TestReapprovalPersistsAnIndependentDisposalAcrossRestart(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)

	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatalf("BeginClaimBase after reapproval: %v", err)
	}
	refs := endedCycles(t, r.provider)
	if len(refs) != 1 || refs[0].Path != first.Path || refs[0].Identifier != issueID {
		t.Fatalf("ended cycles = %+v, want the first cycle %s", refs, first.Path)
	}

	next := r.restart()
	second := next.mustPrepare(1, 21)
	if second.Path == first.Path {
		t.Fatalf("replacement reused old address %s", first.Path)
	}
	refs = endedCycles(t, next.provider)
	if len(refs) != 1 || refs[0].Path != first.Path {
		t.Fatalf("ended cycles after restart = %+v, want %s", refs, first.Path)
	}
	if err := next.provider.CompleteEndedCycle(ctx, endedWorkspace(refs[0])); err != nil {
		t.Fatalf("CompleteEndedCycle(old): %v", err)
	}
	if refs := endedCycles(t, next.provider); len(refs) != 0 {
		t.Fatalf("ended cycles after confirmation = %+v, want empty", refs)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 || calls[0].Claim.Epoch != 100 ||
		calls[0].Outcome != remotews.OutcomeRevoked || !calls[0].Quiet {
		t.Fatalf("disposals = %+v, want one quiet on_revoked call for approval 100", calls)
	}

	got, found, err := next.provider.ResolveWorkspace(ctx, next.issue)
	if err != nil || !found || got.Path != second.Path || got.ClaimEpoch != 21 {
		t.Fatalf("replacement after old disposal = %+v, found %t, err %v; want %+v", got, found, err, second)
	}
	base, err := next.provider.ClaimBase(ctx, next.issue)
	if err != nil || base.State != core.ClaimBasePinned || base.Epoch != 21 {
		t.Fatalf("replacement claim base = %+v, err %v; want pinned epoch 21", base, err)
	}
}

// A disposal record is self-addressed, so a valid record copied in from another
// workflow has a valid filename too. The provider must still refuse it before
// either enumeration or direct completion can hand its repository to the
// backend disposer.
func TestForeignRepositoryDisposalFailsClosed(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	disposals, err := r.store.CycleDisposals()
	if err != nil || len(disposals) != 1 {
		t.Fatalf("cycle disposals = %+v, err %v; want one", disposals, err)
	}
	disposal := disposals[0]
	if gone, err := r.store.DeleteCycleDisposal(disposal.Address()); err != nil || !gone {
		t.Fatalf("DeleteCycleDisposal = %t, %v; want true, nil", gone, err)
	}
	disposal.Cycle.Repository = "foreign/widgets"
	if err := r.store.SaveCycleDisposal(disposal); err != nil {
		t.Fatalf("installing foreign disposal: %v", err)
	}

	if refs, err := r.provider.EndedCycles(ctx); !errors.Is(err, remotews.ErrCycleState) {
		t.Errorf("EndedCycles = %+v, %v; want ErrCycleState", refs, err)
	}
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(disposal.Ref())); !errors.Is(err, remotews.ErrCycleState) {
		t.Errorf("CompleteEndedCycle = %v; want ErrCycleState", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 0 {
		t.Fatalf("foreign disposal reached the backend policy: %+v", calls)
	}
}

// A policy that deliberately keeps the old allocation is complete too. Its
// record remains as the durable address of what was retained, but it leaves the
// outstanding set and is never offered again after a restart.
func TestRetainedOldCyclePolicyIsNotRepeatedAfterRestart(t *testing.T) {
	r := newRig(t)
	r.disposer.setDisposition(remotews.DispositionRetained)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	ref := endedCycles(t, r.provider)[0]
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)); err != nil {
		t.Fatalf("CompleteEndedCycle: %v", err)
	}
	if refs := endedCycles(t, r.provider); len(refs) != 0 {
		t.Fatalf("retained policy still reports outstanding work: %+v", refs)
	}
	disposals, err := r.store.CycleDisposals()
	if err != nil || len(disposals) != 1 || disposals[0].Disposition != "retained" ||
		disposals[0].Address() != first.Path {
		t.Fatalf("retained address = %+v, err %v; want %s", disposals, err, first.Path)
	}
	retargeted := disposals[0]
	retargeted.ReplacementApproval = 300
	wantErr(t, r.store.SaveCycleDisposal(retargeted), remotews.ErrCycleState)
	if refs := endedCycles(t, r.restart().provider); len(refs) != 0 {
		t.Fatalf("restart reoffered retained policy: %+v", refs)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 || calls[0].Claim.Epoch != 100 {
		t.Fatalf("retained policy calls = %+v, want exactly one", calls)
	}
}

type failCycleSaveStore struct {
	remotews.Store
	mu   sync.Mutex
	fail bool
	err  error
}

func (s *failCycleSaveStore) SaveCycle(c remotews.Cycle) error {
	s.mu.Lock()
	fail, err := s.fail, s.err
	s.mu.Unlock()
	if fail {
		return err
	}
	return s.Store.SaveCycle(c)
}

func (s *failCycleSaveStore) allow() {
	s.mu.Lock()
	s.fail = false
	s.mu.Unlock()
}

// The obligation is published before the replacement. A crash in that window
// leaves the old cycle live, so disposal refuses until replay finishes the
// replacement instead of deleting the cycle both records currently name.
func TestDisposalWaitsForAnInterruptedReplacementWrite(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)

	boom := errors.New("replacement cycle write failed")
	store := &failCycleSaveStore{Store: r.store, fail: true, err: boom}
	r = r.restartWithStore(store)
	r.cycles.set(200)
	if err := r.begin(21); !errors.Is(err, boom) {
		t.Fatalf("BeginClaimBase = %v, want %v", err, boom)
	}
	refs := endedCycles(t, r.provider)
	if len(refs) != 1 || refs[0].Path != first.Path {
		t.Fatalf("ended cycles = %+v, want interrupted obligation %s", refs, first.Path)
	}
	wantErr(t, r.provider.CompleteEndedCycle(ctx, endedWorkspace(refs[0])), remotews.ErrCycleReplacementPending)
	if calls := r.disposer.Calls(); len(calls) != 0 {
		t.Fatalf("disposal ran before the replacement was durable: %+v", calls)
	}
	base, err := r.provider.ClaimBase(ctx, r.issue)
	if err != nil || base.State != core.ClaimBasePinned || base.Epoch != 11 {
		t.Fatalf("interrupted replacement left base %+v, err %v; want live epoch 11", base, err)
	}

	store.allow()
	if err := r.begin(21); err != nil {
		t.Fatalf("BeginClaimBase retry: %v", err)
	}
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(refs[0])); err != nil {
		t.Fatalf("CompleteEndedCycle after replacement: %v", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 || calls[0].Claim.Epoch != 100 {
		t.Fatalf("disposals = %+v, want old cycle exactly once", calls)
	}
}

// ReplacementApproval is recovery intent, not the old obligation's identity.
// If approval advances again while the old cycle is still live, replay must
// retarget that one obligation before publishing the latest replacement.
func TestInterruptedReplacementAdvancesToTheLatestReapproval(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)

	boom := errors.New("replacement cycle write failed")
	store := &failCycleSaveStore{Store: r.store, fail: true, err: boom}
	r = r.restartWithStore(store)
	r.cycles.set(200)
	if err := r.begin(21); !errors.Is(err, boom) {
		t.Fatalf("first replacement = %v, want %v", err, boom)
	}

	// The interrupted A -> B transition still names A as live. A later observed
	// approval C therefore replaces the recovery intent on A rather than creating
	// a second obligation or leaving the transition permanently wedged.
	r = r.restartWithStore(store)
	r.cycles.set(300)
	store.allow()
	if err := r.begin(31); err != nil {
		t.Fatalf("latest replacement: %v", err)
	}
	disposals, err := r.store.CycleDisposals()
	if err != nil || len(disposals) != 1 || disposals[0].Address() != first.Path ||
		disposals[0].ReplacementApproval != 300 {
		t.Fatalf("cycle disposals = %+v, err %v; want one retargeted %s -> 300", disposals, err, first.Path)
	}
	base, err := r.provider.ClaimBase(ctx, r.issue)
	if err != nil || base.State != core.ClaimBasePending || base.Epoch != 31 {
		t.Fatalf("latest replacement base = %+v, err %v; want pending epoch 31", base, err)
	}
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(disposals[0].Ref())); err != nil {
		t.Fatalf("CompleteEndedCycle: %v", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 || calls[0].Claim.Epoch != 100 {
		t.Fatalf("disposals = %+v, want old cycle exactly once", calls)
	}
}

// A replacement that can be read after a failed directory sync is not yet a
// durable reason to destroy the old address. Completion re-publishes the current
// snapshot under the per-key transaction and refuses before policy if that sync
// cannot be confirmed.
func TestOldDisposalRequiresADurableReplacementSnapshot(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	ref := endedCycles(t, r.provider)[0]

	boom := errors.New("replacement directory sync failed")
	store := &failCycleSaveStore{Store: r.store, fail: true, err: boom}
	r = r.restartWithStore(store)
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)); !errors.Is(err, boom) {
		t.Fatalf("CompleteEndedCycle = %v, want replacement durability failure %v", err, boom)
	}
	if calls := r.disposer.Calls(); len(calls) != 0 {
		t.Fatalf("policy ran without a durable replacement: %+v", calls)
	}

	store.allow()
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)); err != nil {
		t.Fatalf("CompleteEndedCycle after replacement sync: %v", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 || calls[0].Claim.Epoch != 100 {
		t.Fatalf("policy calls = %+v, want the old cycle once", calls)
	}
}

// A legacy pin has no target branch. If reapproval overtakes its v2 upgrade
// while that upgrade is pending, the outgoing projection must restore the v1
// record shape rather than either refusing the obligation or inventing a target.
func TestReapprovalPreservesAnOutgoingLegacyPinInTheDisposal(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	cycle, err := r.store.LoadCycle(first.Key)
	if err != nil {
		t.Fatal(err)
	}
	cycle.Version = 1
	cycle.TargetBranch = ""
	if err := r.store.SaveCycle(cycle); err != nil {
		t.Fatal(err)
	}

	r = r.restart()
	if err := r.begin(12); err != nil {
		t.Fatalf("BeginClaimBase(legacy upgrade): %v", err)
	}
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatalf("BeginClaimBase(reapproval): %v", err)
	}
	disposals, err := r.store.CycleDisposals()
	if err != nil || len(disposals) != 1 {
		t.Fatalf("cycle disposals = %+v, err %v; want one", disposals, err)
	}
	old := disposals[0].Cycle
	if old.Version != 1 || old.Epoch != 11 || old.BaseSHA != first.BaseSHA || old.TargetBranch != "" {
		t.Fatalf("legacy disposal cycle = %+v, want epoch 11's targetless pin", old)
	}
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(disposals[0].Ref())); err != nil {
		t.Fatalf("CompleteEndedCycle(legacy): %v", err)
	}
}

type failDisposalDeleteStore struct {
	remotews.Store
	mu   sync.Mutex
	fail bool
	err  error
}

type failAfterDispositionStore struct {
	remotews.Store
	mu   sync.Mutex
	fail bool
	err  error
}

func (s *failAfterDispositionStore) SetCycleDisposalDisposition(address, disposition string) error {
	if err := s.Store.SetCycleDisposalDisposition(address, disposition); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.fail {
		return nil
	}
	s.fail = false
	return s.err
}

// A disposition rename can be visible even when its directory sync reports a
// failure. The retry re-syncs that visible confirmation before dropping the pin,
// and does not send the already-confirmed policy operation again.
func TestVisibleDeletionConfirmationIsResyncedBeforeCleanup(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	ref := endedCycles(t, r.provider)[0]

	boom := errors.New("disposition directory sync failed")
	store := &failAfterDispositionStore{Store: r.store, fail: true, err: boom}
	r = r.restartWithStore(store)
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)); !errors.Is(err, boom) {
		t.Fatalf("CompleteEndedCycle = %v, want confirmation sync failure %v", err, boom)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 {
		t.Fatalf("policy calls after ambiguous confirmation = %+v, want one", calls)
	}
	disposals, err := r.store.CycleDisposals()
	if err != nil || len(disposals) != 1 || disposals[0].Disposition != "deleted" {
		t.Fatalf("visible confirmation = %+v, err %v; want deleted", disposals, err)
	}

	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)); err != nil {
		t.Fatalf("CompleteEndedCycle retry: %v", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 {
		t.Fatalf("confirmation retry repeated policy: %+v", calls)
	}
	if refs := endedCycles(t, r.provider); len(refs) != 0 {
		t.Fatalf("ended cycles after cleanup = %+v", refs)
	}
}

func (s *failDisposalDeleteStore) DeleteCycleDisposal(address string) (bool, error) {
	s.mu.Lock()
	fail, err := s.fail, s.err
	if fail {
		s.fail = false
	}
	s.mu.Unlock()
	if fail {
		return false, err
	}
	return s.Store.DeleteCycleDisposal(address)
}

// Backend confirmation is durable before the pin and obligation are removed.
// A restart in the cleanup window resumes local cleanup without issuing the
// on_revoked operation a second time.
func TestConfirmedDeletionResumesCleanupWithoutRepeatingThePolicy(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)

	boom := errors.New("obligation unlink failed")
	store := &failDisposalDeleteStore{Store: r.store, fail: true, err: boom}
	r = r.restartWithStore(store)
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	ref := endedCycles(t, r.provider)[0]
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)); !errors.Is(err, boom) {
		t.Fatalf("CompleteEndedCycle = %v, want cleanup failure %v", err, boom)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 || calls[0].Claim.Epoch != 100 {
		t.Fatalf("disposals before restart = %+v, want one for approval 100", calls)
	}
	disposals, err := r.store.CycleDisposals()
	if err != nil || len(disposals) != 1 || disposals[0].Disposition != "deleted" {
		t.Fatalf("durable confirmation = %+v, err %v; want one deleted record", disposals, err)
	}

	next := r.restart()
	if err := next.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)); err != nil {
		t.Fatalf("CompleteEndedCycle cleanup after restart: %v", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 {
		t.Fatalf("restart repeated an already confirmed policy: %+v", calls)
	}
	if refs := endedCycles(t, next.provider); len(refs) != 0 {
		t.Fatalf("ended cycles after resumed cleanup = %+v", refs)
	}
	got, found, err := next.provider.ResolveWorkspace(ctx, next.issue)
	if err != nil || !found || got.Path == first.Path {
		t.Fatalf("replacement after resumed cleanup = %+v, found %t, err %v", got, found, err)
	}
}

type controlledDisposer struct {
	mu            sync.Mutex
	calls         []disposeCall
	blockApproval int64
	release       <-chan struct{}
	entered       chan disposeCall
}

func newControlledDisposer(approval int64, release <-chan struct{}) *controlledDisposer {
	return &controlledDisposer{
		blockApproval: approval,
		release:       release,
		entered:       make(chan disposeCall, 8),
	}
}

func (d *controlledDisposer) Complete(
	ctx context.Context, claim remote.Claim, outcome remotews.Outcome, prev remote.Status,
) (remotews.Disposition, error) {
	call := disposeCall{Claim: claim, Outcome: outcome, Quiet: remote.MayReuse(prev)}
	d.mu.Lock()
	d.calls = append(d.calls, call)
	d.mu.Unlock()
	d.entered <- call
	if claim.Epoch == d.blockApproval && d.release != nil {
		select {
		case <-d.release:
		case <-ctx.Done():
			return remotews.DispositionDeleted, ctx.Err()
		}
	}
	return remotews.DispositionDeleted, nil
}

func (d *controlledDisposer) Calls() []disposeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]disposeCall(nil), d.calls...)
}

func waitDisposer(t *testing.T, d *controlledDisposer) disposeCall {
	t.Helper()
	select {
	case call := <-d.entered:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("disposer was not entered")
		return disposeCall{}
	}
}

// Once A has its own obligation, its backend delete no longer owns the live
// issue record. B can pin, finish, and be deleted first without either cleanup
// deleting the other's record or losing A's durable address.
func TestReplacementCanFinishWhileTheOldCycleDeleteIsInFlight(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(release) }) }()
	disposer := newControlledDisposer(100, release)
	r := newRig(t, func(o *remotews.Options) { o.Disposer = disposer })
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	old := endedCycles(t, r.provider)[0]

	oldDone := make(chan error, 1)
	go func() { oldDone <- r.provider.CompleteEndedCycle(ctx, endedWorkspace(old)) }()
	if call := waitDisposer(t, disposer); call.Claim.Epoch != 100 {
		t.Fatalf("blocked call = %+v, want approval 100", call)
	}

	second := r.mustPrepare(1, 21)
	if err := r.provider.CompleteEndedCycle(ctx, second); err != nil {
		t.Fatalf("CompleteEndedCycle(replacement): %v", err)
	}
	if _, found, err := r.provider.ResolveWorkspace(ctx, r.issue); err != nil || found {
		t.Fatalf("replacement after its delete = found %t, err %v; want absent", found, err)
	}
	refs := endedCycles(t, r.provider)
	if len(refs) != 1 || refs[0].Path != first.Path {
		t.Fatalf("old obligation while replacement is gone = %+v, want %s", refs, first.Path)
	}

	once.Do(func() { close(release) })
	if err := <-oldDone; err != nil {
		t.Fatalf("old deletion: %v", err)
	}
	if refs := endedCycles(t, r.provider); len(refs) != 0 {
		t.Fatalf("ended cycles after both deletes = %+v", refs)
	}
	calls := disposer.Calls()
	if len(calls) != 2 || calls[0].Claim.Epoch != 100 || calls[1].Claim.Epoch != 200 {
		t.Fatalf("disposals = %+v, want old then replacement exact addresses", calls)
	}
}

// A disposal that still resolves through the live record is the other writer
// of that record. It holds the per-key transaction until confirmation, so a
// concurrent reapproval cannot manufacture a second obligation for work the
// first writer is already deleting or overwrite its cleanup.
func TestCurrentCycleDisposalSerializesWithAConcurrentReplacement(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(release) }) }()
	disposer := newControlledDisposer(100, release)
	r := newRig(t, func(o *remotews.Options) { o.Disposer = disposer })
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)

	disposed := make(chan error, 1)
	go func() { disposed <- r.provider.CompleteEndedCycle(ctx, first) }()
	waitDisposer(t, disposer)
	r.cycles.set(200)
	replaced := make(chan error, 1)
	go func() { replaced <- r.begin(21) }()
	select {
	case err := <-replaced:
		t.Fatalf("replacement returned before current-cycle disposal confirmed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	once.Do(func() { close(release) })
	if err := <-disposed; err != nil {
		t.Fatalf("current-cycle disposal: %v", err)
	}
	if err := <-replaced; err != nil {
		t.Fatalf("replacement after disposal: %v", err)
	}
	if refs := endedCycles(t, r.provider); len(refs) != 0 {
		t.Fatalf("replacement invented an obligation for an already deleted cycle: %+v", refs)
	}
	second := r.mustPrepare(1, 21)
	if second.Path == first.Path {
		t.Fatalf("replacement reused deleted address %s", first.Path)
	}
}

type observedSerialStore struct {
	remotews.Store
	mu       sync.Mutex
	attempts chan struct{}
}

func (s *observedSerialStore) WithCycle(_ string, fn func() error) error {
	s.attempts <- struct{}{}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

type blockingTrustedBase struct {
	remotews.TrustedBase
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (b *blockingTrustedBase) RecordClaimRetaining(
	ctx context.Context, ref core.RemoteClaimRef, retained []core.RemoteClaimRef,
) (core.RemoteClaim, error) {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
	case <-ctx.Done():
		return core.RemoteClaim{}, ctx.Err()
	}
	return b.TrustedBase.RecordClaimRetaining(ctx, ref, retained)
}

// Preparation and replacement both rewrite the live cycle snapshot. Stage the
// replacement at the store transaction while preparation is still pinning A:
// after the lock passes, A's complete pin must become the old obligation and B
// must remain the live pending record. Without the shared transaction either
// writer can publish its stale snapshot over the other.
func TestReplacementSerializesWithTheOldCyclePinWrite(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var serial *observedSerialStore
	var base *blockingTrustedBase
	r := newRig(t, func(o *remotews.Options) {
		serial = &observedSerialStore{Store: o.Store, attempts: make(chan struct{}, 8)}
		base = &blockingTrustedBase{
			TrustedBase: o.Base, entered: make(chan struct{}), release: release,
		}
		o.Store = serial
		o.Base = base
	})
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	<-serial.attempts // the initial pending write

	prepared := make(chan struct {
		ws  core.Workspace
		err error
	}, 1)
	go func() {
		ws, err := r.prepare(1, 11)
		prepared <- struct {
			ws  core.Workspace
			err error
		}{ws: ws, err: err}
	}()
	select {
	case <-base.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old-cycle pin did not reach the controlled write")
	}
	<-serial.attempts // preparation owns the transaction

	r.cycles.set(200)
	replaced := make(chan error, 1)
	go func() { replaced <- r.begin(21) }()
	select {
	case <-serial.attempts:
		// The replacement has reached the same transaction and is waiting on A.
	case <-time.After(5 * time.Second):
		t.Fatal("replacement did not reach the cycle transaction")
	}
	releaseOnce.Do(func() { close(release) })

	got := <-prepared
	if got.err != nil {
		t.Fatalf("PrepareClaim(old): %v", got.err)
	}
	if err := <-replaced; err != nil {
		t.Fatalf("BeginClaimBase(replacement): %v", err)
	}
	refs := endedCycles(t, r.provider)
	if len(refs) != 1 || refs[0].Path != got.ws.Path {
		t.Fatalf("ended cycles = %+v, want the prepared old cycle %s", refs, got.ws.Path)
	}
	state, err := r.provider.ClaimBase(context.Background(), r.issue)
	if err != nil || state.State != core.ClaimBasePending || state.Epoch != 21 {
		t.Fatalf("live replacement state = %+v, err %v; want pending epoch 21", state, err)
	}
}

// The old cycle's run journal is still its own liveness evidence after the live
// record moves. No policy call occurs until that exact journal reaches domain
// quiet; B's pending record is not evidence about A's process.
func TestOldCycleDisposalStillRequiresItsOwnDomainQuietEvidence(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	r.dispatch("run-a")
	r.backend.Complete("run-a")

	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	ref := endedCycles(t, r.provider)[0]
	wantErr(t, r.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)), remote.ErrNotQuiet)
	if calls := r.disposer.Calls(); len(calls) != 0 {
		t.Fatalf("policy ran over unconfirmed old process: %+v", calls)
	}
	r.backend.Quiet("run-a")
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(ref)); err != nil {
		t.Fatalf("CompleteEndedCycle after quiet: %v", err)
	}
	if calls := r.disposer.Calls(); len(calls) != 1 || !calls[0].Quiet || calls[0].Claim.Epoch != 100 {
		t.Fatalf("policy calls after quiet = %+v", calls)
	}
}

// Repeated reapprovals can leave two independently owned allocations for one
// issue. Deleting either record cannot consume or rename the other, and neither
// cleanup resolves through the live third cycle.
func TestTwoOldCyclesForOneIssueAreIndependent(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	second := r.mustPrepare(1, 21)
	r.cycles.set(300)
	if err := r.begin(31); err != nil {
		t.Fatal(err)
	}
	third := r.mustPrepare(1, 31)

	refs := endedCycles(t, r.provider)
	if len(refs) != 2 {
		t.Fatalf("ended cycles = %+v, want two", refs)
	}
	byPath := map[string]core.WorkspaceRef{}
	for _, ref := range refs {
		byPath[ref.Path] = ref
	}
	if _, ok := byPath[first.Path]; !ok {
		t.Fatalf("first obligation missing from %+v", refs)
	}
	if _, ok := byPath[second.Path]; !ok {
		t.Fatalf("second obligation missing from %+v", refs)
	}

	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(byPath[second.Path])); err != nil {
		t.Fatalf("complete second old cycle: %v", err)
	}
	refs = endedCycles(t, r.provider)
	if len(refs) != 1 || refs[0].Path != first.Path {
		t.Fatalf("after deleting second old cycle = %+v, want first", refs)
	}
	if err := r.provider.CompleteEndedCycle(ctx, endedWorkspace(byPath[first.Path])); err != nil {
		t.Fatalf("complete first old cycle: %v", err)
	}
	got, found, err := r.provider.ResolveWorkspace(ctx, r.issue)
	if err != nil || !found || got.Path != third.Path || got.ClaimEpoch != 31 {
		t.Fatalf("live third cycle = %+v, found %t, err %v; want %+v", got, found, err, third)
	}
	calls := r.disposer.Calls()
	if len(calls) != 2 || calls[0].Claim.Epoch != 200 || calls[1].Claim.Epoch != 100 {
		t.Fatalf("disposals = %+v, want approval 200 then 100", calls)
	}
}
