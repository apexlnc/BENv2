package airlock

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// Workspaces is remote.WorkspaceBackend over Airlock sandboxes.
//
// Four verbs over one noun, each idempotent per claim cycle, and the
// idempotence is Airlock's rather than a retry loop's: createSandbox is keyed,
// and suspend, resume and delete name a target state, so repeating any of them
// converges. What this type adds is the durable claim → sandbox binding Airlock
// deliberately does not offer (Store), and the two comparisons that make a
// reattach provable rather than hopeful — owner and pinned profile revision.
type Workspaces struct {
	client  *client
	store   Store
	profile string
	binding SubstrateBinding
	settle  time.Duration
	poll    time.Duration
	retain  Retention
	sleep   func(context.Context, time.Duration) error
}

// Retention is the sandbox lifetime policy, in the two dimensions Airlock lets
// a client request and clamps to the profile's ceilings.
//
// Both are *ceilings BEN asks for*, not guarantees: the resolved values come
// back on the sandbox and the profile's maxima win. They exist because the
// scarce resource on this substrate is a backend allocation, and a daemon that
// crashes between a claim's attempts would otherwise leave a warm sandbox and
// a persistent volume allocated with nothing left to release them.
type Retention struct {
	// IdleSuspend is the idle window after which the control plane releases
	// compute. Zero leaves the profile's default.
	IdleSuspend time.Duration
	// DeleteAfterIdle is the idle window after which the control plane deletes
	// the sandbox and destroys its volume. Zero leaves the profile's default.
	// Retained storage is the expensive resource, which is why the contract
	// makes this the ceiling that most needs to bind.
	DeleteAfterIdle time.Duration
	// OnSuccess, OnFailure, OnRevoked and OnShutdown are what Complete does
	// with the workspace once a run is confirmed quiet.
	OnSuccess  Disposal
	OnFailure  Disposal
	OnRevoked  Disposal
	OnShutdown Disposal
}

// Disposal is the closed set of things BEN may do with a claim's workspace when
// the claim ends. The zero value retains, which is the only safe default: a
// disposal nobody configured must not destroy a tree somebody may still need.
type Disposal uint8

const (
	// DisposalRetain leaves the sandbox as it is — warm, allocated, and
	// costing. For forensics after a failed attempt, mirroring §6.4's local
	// keep-the-worktree rule.
	DisposalRetain Disposal = iota
	// DisposalSuspend releases compute and keeps the volume, so a later
	// Acquire on the same claim resumes rather than rebuilds.
	DisposalSuspend
	// DisposalDelete destroys compute and the persistent volume. The only
	// disposal that loses work, and the only one whose completion BEN waits to
	// see confirmed.
	DisposalDelete
)

func (d Disposal) String() string {
	switch d {
	case DisposalRetain:
		return "retain"
	case DisposalSuspend:
		return "suspend"
	case DisposalDelete:
		return "delete"
	default:
		return fmt.Sprintf("Disposal(%d)", uint8(d))
	}
}

// Acquire creates or resumes the workspace for a claim cycle.
//
// The order is the contract. The durable record — claim, branch, base, profile,
// the derived key and the credential's runtime principal binding — is written
// *before* createSandbox is attempted, and the exact credential that produced
// that binding is used for the request. The sandbox id and profile revision are
// written before the identity is returned and therefore before anything may run
// in it. A crash anywhere in between leaves a record naming both scopes the key
// must be replayed inside, rather than creating a second sandbox.
func (w *Workspaces) Acquire(ctx context.Context, req remote.AcquireRequest) (remote.Identity, error) {
	if !req.Claim.Valid() {
		return remote.Identity{}, fmt.Errorf("%w: %s is not a complete claim cycle", remote.ErrClaimMismatch, req.Claim)
	}
	if req.Branch == "" || req.BaseSHA == "" {
		return remote.Identity{}, fmt.Errorf("%w: %s has no branch or trusted base", remote.ErrIdentityMissing, req.Claim)
	}

	rec, err := loadBoundSandbox(w.store, w.binding, req.Claim)
	var createAuth keyedCredential
	switch {
	case err == nil:
		if rec.Branch != req.Branch || rec.BaseSHA != req.BaseSHA || rec.Profile != w.profile {
			// The same claim cycle asking for a different publication target,
			// verification base, or profile is not a retry of this one. Refusing
			// beats silently creating a second sandbox under a second key and
			// leaking the first.
			return remote.Identity{}, fmt.Errorf("%w: %s is recorded against a different branch, base or profile",
				remote.ErrClaimMismatch, req.Claim)
		}
		if rec.SandboxID == "" {
			createAuth, err = w.client.keyedAuth(ctx)
			if err != nil {
				return remote.Identity{}, err
			}
			if err := requirePrincipalBinding(rec.PrincipalBinding, createAuth.principalBinding, req.Claim.String()); err != nil {
				return remote.Identity{}, err
			}
		}
	case errors.Is(err, ErrNoSandboxRecord):
		createAuth, err = w.client.keyedAuth(ctx)
		if err != nil {
			return remote.Identity{}, err
		}
		rec = SandboxRecord{
			Version:           SandboxRecordVersion,
			Claim:             req.Claim,
			Substrate:         w.binding,
			PrincipalBinding:  createAuth.principalBinding,
			Branch:            req.Branch,
			BaseSHA:           req.BaseSHA,
			Profile:           w.profile,
			Key:               sandboxKey(req.Claim, req.Branch, req.BaseSHA, w.profile),
			CreateAttemptedAt: time.Now().UTC(),
		}
		if err := w.store.SaveSandbox(rec); err != nil {
			return remote.Identity{}, err
		}
	default:
		return remote.Identity{}, err
	}

	var sandbox Sandbox
	if rec.SandboxID == "" {
		sandbox, err = w.create(ctx, rec, createAuth)
		if err != nil {
			return remote.Identity{}, err
		}
	} else {
		// SandboxID is the permanent resource handle. Replaying create after
		// Airlock has forgotten its bounded idempotency record can allocate a
		// second sandbox before bind has a chance to notice, so a known handle is
		// always read directly.
		sandbox, err = w.get(ctx, rec.SandboxID)
		if err != nil {
			return remote.Identity{}, w.classify(rec, err)
		}
	}
	if err := w.bind(&rec, sandbox); err != nil {
		return remote.Identity{}, err
	}
	if err := w.checkPin(rec, req.ProfileRevision, sandbox); err != nil {
		return remote.Identity{}, err
	}
	sandbox, err = w.settleReady(ctx, sandbox)
	if err != nil {
		return remote.Identity{}, err
	}
	// Re-checked after settling: a resume that could not serve the pinned
	// revision fails loudly at Airlock, but a control plane that answered with a
	// different one anyway would otherwise be adopted here.
	if err := w.checkPin(rec, req.ProfileRevision, sandbox); err != nil {
		return remote.Identity{}, err
	}
	if learned, err := w.learnLimits(ctx, rec); err == nil {
		rec = learned
	}
	return rec.Identity(), nil
}

// learnLimits records the stdin envelope of the revision a sandbox is pinned
// to, when — and only when — the profile's current revision is that one.
//
// Airlock exposes a profile at its current revision and nothing else, while a
// sandbox stays on the revision it was created against for its whole life. So
// the only moment the pinned envelope is readable is while the two agree,
// which is every acquire until the operator rolls the profile forward, and the
// answer is written to the sandbox record so a later rollout cannot change
// what its runs are planned against (#284).
//
// A read that fails, or a current revision that is not the pin, leaves the
// record as it was. That is not fatal: the record is retried at every acquire,
// attach and start, and a prompt planned against an unknown envelope travels
// inline with the backend as its judge — the pre-#284 posture, whose refusal is
// now definite and surfaced rather than replayed.
func (w *Workspaces) learnLimits(ctx context.Context, rec SandboxRecord) (SandboxRecord, error) {
	if rec.Limits != nil || rec.SandboxID == "" || rec.ProfileRevision == "" {
		return rec, nil
	}
	var profile Profile
	err := w.client.do(ctx, request{
		method: "GET", path: "/v2/profiles/" + url.PathEscape(rec.Profile), out: &profile,
	})
	if err != nil {
		return rec, err
	}
	if profile.ProfileRevision != rec.ProfileRevision {
		return rec, fmt.Errorf("airlock: profile %s is at %s and %s is pinned at %s; its stdin envelope is not readable",
			rec.Profile, profile.ProfileRevision, rec.SandboxID, rec.ProfileRevision)
	}
	limits := profile.Limits
	rec.Limits = &limits
	// The first field an older reader does not have, so the record takes the
	// version that carries it: a rollback then refuses the record loudly
	// rather than planning against an envelope it cannot see.
	rec.Version = SandboxRecordVersion
	if err := w.store.SaveSandbox(rec); err != nil {
		return rec, err
	}
	return rec, nil
}

// create replays the claim's createSandbox key. A 200 with
// `Idempotency-Replayed: true` and a 201 are the same answer to this caller —
// the same sandbox — which is why the header is not read: the guarantee lives in
// the key, and a client that branched on the header would have two code paths
// where the contract has one.
func (w *Workspaces) create(ctx context.Context, rec SandboxRecord, auth keyedCredential) (Sandbox, error) {
	if auth.token == "" {
		return Sandbox{}, fmt.Errorf("%w: %s has no credential snapshot for create", ErrSubstrateBinding, rec.Claim)
	}
	if err := requirePrincipalBinding(rec.PrincipalBinding, auth.principalBinding, rec.Claim.String()); err != nil {
		return Sandbox{}, err
	}
	if rec.CreateAttemptedAt.IsZero() {
		return Sandbox{}, fmt.Errorf("%w: %s has an old unanswered reservation with no replay fence; "+
			"reconcile idempotency key %s before retrying", ErrCreateReplayExpired, rec.Claim, rec.Key)
	}
	deadline := rec.CreateAttemptedAt.Add(idempotencyReplayWindow)
	if !time.Now().Before(deadline) {
		return Sandbox{}, fmt.Errorf("%w: %s may no longer replay idempotency key %s safely",
			ErrCreateReplayExpired, rec.Claim, rec.Key)
	}
	// Bound the client's internal retries too. A check only at Acquire's edge
	// would still allow its last retry to cross the server's expiry boundary.
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	body := createSandboxRequest{
		ProfileID: rec.Profile,
		// Opaque, caller-chosen, echoed back and recorded in audit metadata. The
		// claim cycle and branch only: identifiers the tracker already publishes,
		// which is the line the contract draws — labels are explicitly not a place
		// for secrets or prompt text.
		Labels: map[string]string{
			"ben.claim":  rec.Claim.String(),
			"ben.branch": rec.Branch,
		},
	}
	if secs := idleSeconds(w.retain.IdleSuspend); secs != nil {
		body.IdleSuspendAfterSeconds = secs
	}
	if secs := idleSeconds(w.retain.DeleteAfterIdle); secs != nil {
		body.DeleteAfterIdleSeconds = secs
	}
	var sandbox Sandbox
	err := w.client.do(ctx, request{
		method: "POST", path: "/v2/sandboxes", idem: rec.Key, body: body,
		authToken: auth.token, out: &sandbox,
	})
	if err != nil {
		return Sandbox{}, w.classify(rec, err)
	}
	return sandbox, nil
}

// bind persists Airlock's two opaque answers, and refuses to move them.
//
// A record whose sandbox id changed under an unchanged key is the failure this
// whole file is arranged around: it means the key's 24-hour replay window
// expired and a replay created a *second* sandbox, exactly as the contract
// warns. Adopting the new id would leak the first sandbox and its volume; the
// refusal parks the claim for a human instead.
func (w *Workspaces) bind(rec *SandboxRecord, sandbox Sandbox) error {
	if sandbox.SandboxID == "" || sandbox.ProfileRevision == "" {
		return fmt.Errorf("%w: %s answered with no sandbox id or profile revision",
			remote.ErrIdentityMissing, rec.Claim)
	}
	if rec.SandboxID != "" && rec.SandboxID != sandbox.SandboxID {
		return fmt.Errorf("%w: %s is recorded against another sandbox; the idempotency key's replay "+
			"window has expired and a replay created a second one", remote.ErrClaimMismatch, rec.Claim)
	}
	if rec.SandboxID == sandbox.SandboxID && rec.ProfileRevision == sandbox.ProfileRevision && rec.Owner == sandbox.Owner {
		return nil
	}
	if rec.ProfileRevision != "" && rec.ProfileRevision != sandbox.ProfileRevision {
		return fmt.Errorf("%w: %s pinned %s and the sandbox now reports %s",
			ErrProfileRevision, rec.Claim, rec.ProfileRevision, sandbox.ProfileRevision)
	}
	rec.SandboxID, rec.ProfileRevision, rec.Owner = sandbox.SandboxID, sandbox.ProfileRevision, sandbox.Owner
	return w.store.SaveSandbox(*rec)
}

// checkPin refuses a sandbox whose immutable revision is not the one the caller
// pinned. Empty asks for the backend's current revision, which is the
// fresh-claim case; a non-empty pin comes off the durable record after a restart
// and is the whole reason ProfileRevision exists.
func (w *Workspaces) checkPin(rec SandboxRecord, pinned string, sandbox Sandbox) error {
	if pinned != "" && pinned != sandbox.ProfileRevision {
		return fmt.Errorf("%w: %s pinned %s and the sandbox reports %s",
			ErrProfileRevision, rec.Claim, pinned, sandbox.ProfileRevision)
	}
	if rec.Owner != (Principal{}) && sandbox.Owner != (Principal{}) && rec.Owner != sandbox.Owner {
		return fmt.Errorf("%w: %s is recorded against %s/%s", ErrNotOwned, rec.Claim,
			rec.Owner.TenantID, rec.Owner.Subject)
	}
	return nil
}

// settleReady resumes a suspended sandbox and waits for a settling one, and
// refuses the three states that are verdicts rather than stages.
func (w *Workspaces) settleReady(ctx context.Context, sandbox Sandbox) (Sandbox, error) {
	deadline := time.Now().Add(w.settle)
	for {
		switch sandbox.State {
		case SandboxReady:
			return sandbox, nil
		case SandboxSuspended:
			resumed, err := w.resume(ctx, sandbox.SandboxID)
			if err != nil {
				return Sandbox{}, err
			}
			sandbox = resumed
			continue
		case SandboxFailed, SandboxDeleting, SandboxDeleted:
			return Sandbox{}, fmt.Errorf("%w: %s is %s", ErrSandboxUnusable, sandbox.SandboxID, sandbox.State)
		}
		if !sandbox.State.Settling() {
			return Sandbox{}, fmt.Errorf("%w: %s is %s", ErrSandboxUnusable, sandbox.SandboxID, sandbox.State)
		}
		if time.Now().After(deadline) {
			// Not a failure of the sandbox, and deliberately not fatal to the
			// claim: the record survives, so the next tick reattaches and waits
			// again rather than acquiring a second one.
			return Sandbox{}, fmt.Errorf("airlock: %s is still %s after %s", sandbox.SandboxID, sandbox.State, w.settle)
		}
		if err := w.sleep(ctx, w.poll); err != nil {
			return Sandbox{}, err
		}
		fresh, err := w.get(ctx, sandbox.SandboxID)
		if err != nil {
			return Sandbox{}, err
		}
		sandbox = fresh
	}
}

func (w *Workspaces) resume(ctx context.Context, id string) (Sandbox, error) {
	var sandbox Sandbox
	err := w.client.do(ctx, request{
		method: "POST", path: "/v2/sandboxes/" + url.PathEscape(id) + "/resume", out: &sandbox,
	})
	if hasCode(err, CodeProfileRevUnavailable) {
		// Not retryable by contract: an operator withdrew the pinned revision.
		// The claim parks rather than silently resuming on a newer world.
		return Sandbox{}, fmt.Errorf("%w: %s pinned a revision an operator has withdrawn: %w", ErrProfileRevision, id, err)
	}
	if err != nil {
		return Sandbox{}, err
	}
	return sandbox, nil
}

func (w *Workspaces) get(ctx context.Context, id string) (Sandbox, error) {
	var sandbox Sandbox
	err := w.client.do(ctx, request{
		method: "GET", path: "/v2/sandboxes/" + url.PathEscape(id), out: &sandbox,
	})
	if err != nil {
		return Sandbox{}, err
	}
	return sandbox, nil
}

// Attach returns the identity of an existing workspace without creating one.
//
// ErrNoWorkspace is a *fact* — nothing was ever acquired for this claim cycle,
// or the sandbox is gone — and every other error is "could not ask", which is
// not (SPEC §9.10). The two are separated here because they are the only two
// answers a restart may act on differently.
func (w *Workspaces) Attach(ctx context.Context, claim remote.Claim) (remote.Identity, error) {
	rec, err := loadBoundSandbox(w.store, w.binding, claim)
	if errors.Is(err, ErrNoSandboxRecord) {
		return remote.Identity{}, fmt.Errorf("%w: %s", remote.ErrNoWorkspace, claim)
	}
	if err != nil {
		return remote.Identity{}, err
	}
	if rec.SandboxID == "" {
		// A reservation with no sandbox: the record landed and createSandbox
		// never answered. Not a workspace, and specifically not one to attach
		// to — Acquire replays the key and resolves it.
		return remote.Identity{}, fmt.Errorf("%w: %s was reserved but never acquired", remote.ErrNoWorkspace, claim)
	}
	sandbox, err := w.get(ctx, rec.SandboxID)
	if err != nil {
		return remote.Identity{}, w.classify(rec, err)
	}
	if err := w.checkPin(rec, rec.ProfileRevision, sandbox); err != nil {
		return remote.Identity{}, err
	}
	if sandbox.State == SandboxDeleted {
		return remote.Identity{}, fmt.Errorf("%w: %s has been deleted", remote.ErrNoWorkspace, claim)
	}
	if learned, err := w.learnLimits(ctx, rec); err == nil {
		rec = learned
	}
	return rec.Identity(), nil
}

// Suspend releases the warm sandbox while retaining the workspace identity.
//
// A no-op on an already-suspended claim, and a no-op on a claim with no record:
// both are the state the caller asked for. A `run_conflict` is passed through
// unchanged — the contract refuses to suspend over a non-terminal run precisely
// so a suspend can never be mistaken for a termination nobody asked for, which
// is the same rule remote.MayReuse states from BEN's side.
func (w *Workspaces) Suspend(ctx context.Context, claim remote.Claim) error {
	rec, err := loadBoundSandbox(w.store, w.binding, claim)
	if errors.Is(err, ErrNoSandboxRecord) {
		return nil
	}
	if err != nil {
		return err
	}
	if rec.SandboxID == "" {
		return nil
	}
	err = w.client.do(ctx, request{
		method: "POST", path: "/v2/sandboxes/" + url.PathEscape(rec.SandboxID) + "/suspend",
	})
	if hasCode(err, CodeInvalidStateTransition) {
		// The sandbox is somewhere suspension does not apply from — already
		// suspended is a 202, so this is deleting, deleted, or failed. Nothing to
		// release, and nothing lost.
		return nil
	}
	if err != nil {
		return w.classify(rec, err)
	}
	return nil
}

// Delete removes the workspace, and does not report success until Airlock's
// three evidence fields say the data is actually gone.
//
// The wait is the point. `202` moves the sandbox to `deleting` and the contract
// is explicit that deletion is not complete when it returns: until
// compute_released, volume_destroyed and record_tombstoned are each `confirmed`,
// a caller may not assume the volume is gone. Returning early would let BEN
// forget the record — and a forgotten record is a sandbox nothing will ever
// finish deleting, which on this substrate is a bill rather than a bug.
//
// The read *before* the delete is the other half, and it is not a duplicate of
// the caller's gate. `DELETE` is the one workspace verb Airlock does not refuse
// over a live run: suspend answers `run_conflict`, while delete moves the
// sandbox to `deleting` and marks whatever was executing in it `lost`. So the
// check below is BEN's own, and it is the only gate that covers a run BEN's
// claim journal cannot see — a review executing in the same workspace-cycle
// sandbox under its own run id (#252, internal/reviewrun). remote.Dispose's gate
// reads the *claim's* run status and would pass while a reviewer still held the
// domain.
//
// **Two facts, and the empty slot is only one of them.** Airlock releases the
// active slot when a run reaches a terminal state — whatever its evidence says —
// and in the same step moves the sandbox `ready -> failed` if that run's domain
// quiet was not confirmed. So a slot that is empty says every run *terminated*;
// it does not say the domain is quiet, and reading it as though it did is
// remote.MayReuse's fail-open mistake with an extra step. `failed` is the state
// that says nobody can attest what is still executing in there, and a volume
// destroyed under it is destroyed under a process that may still be writing.
//
// Refusing leaves such a sandbox for the profile's `delete_after_idle` window
// and for an operator, which is the same asymmetry §9.8 takes everywhere else: a
// refusal costs a retained allocation and another tick, and the other answer
// costs the volume.
func (w *Workspaces) Delete(ctx context.Context, claim remote.Claim) error {
	rec, err := loadBoundSandbox(w.store, w.binding, claim)
	if errors.Is(err, ErrNoSandboxRecord) {
		return nil
	}
	if err != nil {
		return err
	}
	if rec.SandboxID == "" {
		// The create may have committed and lost its response. With no resource
		// handle there is nothing safe to delete, and dropping the reservation
		// would make a live sandbox permanently invisible to BEN.
		return fmt.Errorf("%w: %s has an unanswered create and no sandbox id",
			ErrDeletionUnconfirmed, claim)
	}

	sandbox, err := w.get(ctx, rec.SandboxID)
	if err != nil {
		return w.classify(rec, err)
	}
	if err := w.checkPin(rec, rec.ProfileRevision, sandbox); err != nil {
		return err
	}
	if sandbox.Deletion != nil && sandbox.Deletion.Confirmed() {
		// A previous delete landed and its response was lost, or a later sweep is
		// replaying the same one. The evidence is already complete, so this is the
		// tombstone rather than a second request.
		return w.store.DeleteSandbox(claim)
	}
	if id := sandbox.ActiveRunID; id != nil && *id != "" {
		// A run still holds the slot. Retried rather than forced — it ends, and the
		// next tick asks again.
		return fmt.Errorf("%w: %s holds active run %s and deletion was requested",
			remote.ErrNotQuiet, claim, *id)
	}
	if sandbox.State == SandboxFailed {
		// The slot is empty and the domain is *not* attested quiet: this is where a
		// run that terminated without confirmed domain quiet leaves the sandbox. It
		// does not converge on its own, so it is reported at every attempt rather
		// than waited on silently.
		return fmt.Errorf("%w: %s is %s, so no run's execution domain in it is attested quiet",
			remote.ErrNotQuiet, claim, sandbox.State)
	}

	sandbox, err = w.deleteSandbox(ctx, rec)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(w.settle)
	for {
		if sandbox.Deletion != nil && sandbox.Deletion.Confirmed() {
			return w.store.DeleteSandbox(claim)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: %s is %s and its evidence is incomplete after %s",
				ErrDeletionUnconfirmed, rec.SandboxID, sandbox.State, w.settle)
		}
		if err := w.sleep(ctx, w.poll); err != nil {
			return err
		}
		fresh, err := w.get(ctx, rec.SandboxID)
		if hasCode(err, CodeNotFound) {
			// Cross-tenant denial is also 404 by contract. Absence is therefore
			// not positive deletion evidence, even after this daemon requested a
			// delete; retain the address so the still-live case cannot leak.
			return w.classify(rec, err)
		}
		if err != nil {
			return w.classify(rec, err)
		}
		sandbox = fresh
	}
}

func (w *Workspaces) deleteSandbox(ctx context.Context, rec SandboxRecord) (Sandbox, error) {
	var sandbox Sandbox
	err := w.client.do(ctx, request{
		method: "DELETE", path: "/v2/sandboxes/" + url.PathEscape(rec.SandboxID), out: &sandbox,
	})
	if err != nil {
		return Sandbox{}, w.classify(rec, err)
	}
	return sandbox, nil
}

// classify turns the two ownership answers into one refusal a caller can park
// on.
//
// Airlock answers 404 across tenants and 403 inside the owning tenant,
// deliberately, so cross-tenant existence is not observable. From BEN's side
// they are the same fact about a record it holds: it names something this
// principal may not act on. A 404 for a claim whose record carries no sandbox id
// is a different thing entirely and never reaches here.
func (w *Workspaces) classify(rec SandboxRecord, err error) error {
	switch {
	case hasCode(err, CodeForbidden):
		return fmt.Errorf("%w: %s names sandbox %s: %w", ErrNotOwned, rec.Claim, rec.SandboxID, err)
	case hasCode(err, CodeNotFound) && rec.SandboxID != "":
		return fmt.Errorf("%w: %s names sandbox %s, which this principal cannot see: %w",
			ErrNotOwned, rec.Claim, rec.SandboxID, err)
	}
	return err
}

// seconds renders a duration for the contract's integer-second fields, and
// returns nil for "leave the profile's default".
//
// Rounded up, never down: every one of these is a window before something is
// taken away, and a 90-second request that resolved to 60 would cut the window
// shorter than the caller asked.
//
// No floor. These are BEN's own windows — the §7.5 stop grace, the attempt and
// stall limits, a hook's timeout — and they must reach Airlock as the domain
// BEN is already enforcing locally. The default stop grace is 10 seconds, so a
// floor here would make the remote ladder six times more patient than the local
// one for exactly the reason the two are supposed to agree.
func seconds(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	secs := int((d + time.Second - 1) / time.Second)
	return &secs
}

// idleSeconds renders the two sandbox idle windows, which — unlike everything
// seconds serves — carry the contract's own 60-second floor.
//
// Config refuses a shorter window rather than raising it (docs/AIRLOCK.md), so
// a value under the floor cannot arrive from a workflow file. The clamp covers
// the directly constructed backend (#192), where sending 4 would earn a 400
// naming a field the operator never wrote.
func idleSeconds(d time.Duration) *int {
	secs := seconds(d)
	if secs != nil && *secs < 60 {
		*secs = 60
	}
	return secs
}
