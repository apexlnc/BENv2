package remotews

import (
	"context"
	"errors"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// PrepareClaim acquires or reattaches the claim cycle's remote workspace, pins
// its verification base, restores the canonical branch, and runs the lifecycle
// hooks that gate the attempt.
//
// Nothing local is created: no worktree, no base repository, no private
// directory. What comes back is an opaque address and the claim-scoped
// epoch/base/target tuple the orchestrator compares before hooks and verification
// (SPEC §6.2, §9.7).
//
// The returned LocalBranchFacts are deliberately zero, and that is a statement
// rather than a gap. §9.6's local attempt floor describes a surviving worktree
// branch ahead of its claim-time base. On this substrate the new pin already
// contains whatever an earlier epoch in the same approval cycle published, so
// there is no local interval to report. Cycle.Workspace carries that distinct,
// provider-owned fact as Workspace.PriorWork instead: it raises presentation to
// a revision without pretending the folded-in commits are new publication
// evidence or reducing the fresh claim's failure budget.
func (p *Provider) PrepareClaim(
	ctx context.Context, issue core.Issue, attempt int, epoch int64,
) (core.Workspace, core.LocalBranchFacts, error) {
	if epoch <= 0 {
		return core.Workspace{}, core.LocalBranchFacts{}, fmt.Errorf("%w: claim epoch must be positive", ErrClaimEpoch)
	}
	key, err := p.key(issue)
	if err != nil {
		return core.Workspace{}, core.LocalBranchFacts{}, err
	}
	c, err := p.load(key, issue.Identifier)
	if err != nil {
		return core.Workspace{}, core.LocalBranchFacts{}, err
	}
	if c.Epoch != epoch {
		return core.Workspace{}, core.LocalBranchFacts{}, fmt.Errorf(
			"%w: issue %s is recorded for epoch %d, prepare asked for %d",
			ErrCycleState, issue.Identifier, c.Epoch, epoch)
	}

	// 1. The previous attempt's execution domain, before anything touches the
	// workspace. A possibly-live foreign process must never share one with a
	// replacement (SPEC §9.8), and "the stream ended" is not that fact.
	prev, hadRun, err := p.settle(ctx, c)
	if err != nil {
		return core.Workspace{}, core.LocalBranchFacts{}, err
	}

	// 2. The trusted base, pinned daemon-side and *before* the run can start
	// (#193). The cycle record's transition and the set of old pins it must retain
	// are read under the same per-key serialization as BeginClaimBase. Without it,
	// a concurrent replacement can durably owe this cycle after the prune set was
	// read and before RecordClaim removes its only pin.
	err = p.store.WithCycle(key, func() error {
		fresh, err := p.load(key, issue.Identifier)
		if err != nil {
			return err
		}
		if fresh.Address() != c.Address() || fresh.Epoch != epoch {
			return fmt.Errorf("%w: prepare for %s epoch %d was overtaken by %s epoch %d",
				ErrCycleMoved, c.Address(), epoch, fresh.Address(), fresh.Epoch)
		}
		disposals, err := p.disposalRecords()
		if err != nil {
			return err
		}
		retained := make([]core.RemoteClaimRef, 0, len(disposals))
		for _, disposal := range disposals {
			if disposal.Cycle.Key == key {
				retained = append(retained, disposal.Cycle.ClaimRef())
			}
		}
		pin, err := p.base.RecordClaimRetaining(ctx, fresh.ClaimRef(), retained)
		if err != nil {
			return fmt.Errorf("remotews: pinning the claim-time base for issue %s: %w", issue.Identifier, err)
		}
		if pin.Branch != fresh.Branch {
			return fmt.Errorf(
				"%w: the trusted base for issue %s is pinned on branch %q, this claim publishes on %q",
				ErrCycleState, issue.Identifier, pin.Branch, fresh.Branch)
		}
		c, err = p.pin(fresh, pin.BaseSHA, pin.TargetBranch, attempt)
		return err
	})
	if err != nil {
		return core.Workspace{}, core.LocalBranchFacts{}, err
	}

	// 3. The sandbox. From here a failure still reports the workspace, so the
	// claim's exit disposes it rather than leaking the lease — the local
	// provider's §6.6 rule, which returns the worktree it kept.
	created, err := p.acquire(ctx, c, prev, hadRun)
	if err != nil {
		return core.Workspace{}, core.LocalBranchFacts{}, err
	}
	ws := c.Workspace(created)

	// 4–6. after_create (only for a sandbox this prepare allocated), then the
	// remote-first restore, then before_run. The two workflow hooks keep §5.2.6's
	// order and containment; the restore sits between them because before_run may
	// legitimately depend on the tree, and the tree is not this claim's until the
	// restore has run.
	if created {
		if err := p.runHook(ctx, c, remote.HookAfterCreate, attempt); err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
	}
	if err := p.restore(ctx, c, attempt); err != nil {
		return ws, core.LocalBranchFacts{}, err
	}
	if err := p.runHook(ctx, c, remote.HookBeforeRun, attempt); err != nil {
		return ws, core.LocalBranchFacts{}, err
	}
	return ws, core.LocalBranchFacts{}, nil
}

// settle proves the previous attempt's run is over and retires its journal.
//
// It returns the fresh status so the acquire below can be gated on it at the
// boundary as well as here (remote.Reacquire), and whether there was a run at
// all — which is not the same as "quiet": a cycle that never dispatched has no
// evidence of quiet and needs none, while one that did must produce it.
//
// The ordinary path never reaches the interesting branch. ClearRunMarkerFor
// closes the journal as soon as the orchestrator confirms domain quiet, so a
// journal still standing here is a crash between those two points — exactly the
// case §9.10 is written for, and exactly the case where dispatching a
// replacement would put two agents in one sandbox.
func (p *Provider) settle(ctx context.Context, c Cycle) (remote.Status, bool, error) {
	journal, err := remote.OpenJournal(p.journals, c.Claim())
	switch {
	case errors.Is(err, remote.ErrNoRecord):
		return noRunStatus(), false, nil
	case err != nil:
		return remote.Status{}, false, err
	}
	rec := journal.Record()
	if !rec.Dispatched {
		// Reserved and never dispatched. The mark is written before the call, so
		// this is the one shape that positively means nothing was started.
		return noRunStatus(), false, p.retireRun(ctx, journal, rec.ProcessRef())
	}
	st, err := p.status(ctx, rec.ProcessRef())
	if err != nil {
		return remote.Status{}, true, err
	}
	if !remote.MayReuse(st) {
		return st, true, fmt.Errorf("%w: %s is %s and a further attempt would share its workspace",
			remote.ErrNotQuiet, c.Claim(), st.Phase)
	}
	if err := p.retireRun(ctx, journal, rec.ProcessRef()); err != nil {
		return st, true, err
	}
	return st, true, nil
}

// retireRun closes a finished run's journal and then its durable event inbox.
//
// That order, and not the other one. The journal is the address a restart
// attaches by; the inbox is the evidence a restart re-projects. Removing the
// inbox first would leave an addressable run whose accepted outcome had been
// thrown away — a terminal event BEN had committed to and could no longer
// deliver — while removing the journal first leaves at worst an orphan log that
// nothing addresses.
func (p *Provider) retireRun(ctx context.Context, journal *remote.Journal, ref remote.ProcessRef) error {
	if err := journal.Close(); err != nil {
		return err
	}
	if p.consumptions == nil {
		return nil
	}
	return p.consumptions.Discard(ctx, ref)
}

// noRunStatus is the answer for a claim cycle with no addressable run, and it is
// the one place this package states quiet rather than observing it.
//
// It is a statement about BEN's own durable state, not about the backend, and it
// is sound because of what the *absence of a journal* means here. A journal is
// written before any dispatch is attempted (remote.Journal's identity-before-the-
// act rule) and is removed in exactly one place — retireRun, which runs only
// after a fresh domain-quiet observation from a reachable backend. So no journal
// means one of two things, and neither has a run in it: nothing was ever
// dispatched for this cycle, or something was and its termination was positively
// confirmed before the record went.
//
// The alternative is not "safer". remote.MayReuse over a zero Status is false
// forever, so returning one would make the *first* attempt of every claim
// unable to acquire and every finished claim unable to dispose — a workspace and
// a backend lease held for the life of the deployment.
func noRunStatus() remote.Status {
	return remote.Status{
		Phase: remote.PhaseQuiet, Reachable: true,
		Stream: remote.StreamStateSealed, Process: remote.ProcessStateReaped,
		Domain: remote.DomainStateQuiet,
	}
}

// status reads a run's fresh status, treating only the backend's definitive
// "Start never crossed the acceptance fence" answer as quiet.
//
// It is the one place absence is read as a fact rather than as an unanswered
// question, and it is safe because of *which* absence it is: ErrNoProcess is the
// backend stating it never accepted the dispatch (remote.ErrNoProcess), not an
// unanswered Start and not a known run that returned 404. Those latter states
// wrap ErrNotQuiet: they retain the journal and take the orchestrator's
// retryable prepare path instead of terminally failing the claim.
func (p *Provider) status(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
	st, err := p.processes.Status(ctx, ref)
	switch {
	case errors.Is(err, remote.ErrNoProcess):
		return noRunStatus(), nil
	case errors.Is(err, remote.ErrProcessUnresolved), errors.Is(err, remote.ErrProcessUnavailable):
		return remote.Status{}, fmt.Errorf("%w: %s: %w", remote.ErrNotQuiet, ref, err)
	case err != nil:
		return remote.Status{}, err
	}
	return st, nil
}

// pin moves a pending record to pinned, and records which attempt is running.
//
// A pinned record whose base disagrees with the trusted store is a refusal: the
// pin is idempotent per epoch, so the two can only differ if something outside
// this package re-pinned a live claim.
func (p *Provider) pin(c Cycle, base, target string, attempt int) (Cycle, error) {
	if base == "" || target == "" {
		return Cycle{}, fmt.Errorf("%w: the trusted base for issue %s is empty", ErrCycleState, c.Issue)
	}
	if c.State == cyclePinned && (c.BaseSHA != base || c.TargetBranch != target) {
		return Cycle{}, fmt.Errorf("%w: issue %s is pinned at %s and the trusted store now reports %s",
			ErrCycleState, c.Issue, c.BaseSHA, base)
	}
	next := c
	next.State, next.BaseSHA, next.TargetBranch = cyclePinned, base, target
	next.OutgoingEpoch, next.OutgoingBaseSHA = 0, ""
	next.OutgoingTargetBranch = ""
	next.Attempt = attempt
	if next.CycleBaseSHA == "" {
		// The sandbox address is fixed the first time this cycle acquires one, and
		// the base is part of it. Recording it here — before the acquire — is what
		// lets a later assignment inside the same approval remint the verification
		// base and still reattach the same sandbox.
		next.CycleBaseSHA = base
	}
	if next == c {
		return c, nil
	}
	if err := p.store.SaveCycle(next); err != nil {
		return Cycle{}, err
	}
	return next, nil
}

// acquire creates or resumes the cycle's sandbox.
//
// The request carries the *cycle* base, not the epoch's: see Cycle.CycleBaseSHA.
// A backend refuses an acquire whose recorded branch, base or profile has moved
// under one claim cycle — which is the guarantee that keeps a reattach honest —
// so sending the verification base would turn every reassignment into a refusal
// and every revision round into a new sandbox.
func (p *Provider) acquire(ctx context.Context, c Cycle, prev remote.Status, hadRun bool) (bool, error) {
	before, err := p.backend.Attach(ctx, c.Claim())
	switch {
	case errors.Is(err, remote.ErrNoWorkspace):
		before = remote.Identity{}
	case err != nil:
		return false, err
	}

	req := remote.AcquireRequest{
		Claim:           c.Claim(),
		Branch:          c.Branch,
		BaseSHA:         c.CycleBaseSHA,
		ProfileRevision: before.ProfileRevision,
	}
	var id remote.Identity
	if hadRun {
		// Through Reacquire, so the quiet gate is applied at the boundary as well
		// as in settle. Belt and braces for airlock.Complete's reason: a future
		// caller reaching the backend directly still cannot resume a workspace out
		// from under a run.
		id, err = remote.Reacquire(ctx, p.backend, req, prev)
	} else {
		id, err = p.backend.Acquire(ctx, req)
	}
	if err != nil {
		return false, err
	}
	if !id.Complete() {
		return false, fmt.Errorf("%w: %s", remote.ErrIdentityMissing, c.Claim())
	}
	if before.SandboxID != "" && before.SandboxID != id.SandboxID {
		// An acquire that answered with a different sandbox than the one already
		// recorded for this cycle. Nothing in the contract permits it, and acting
		// on it would run this claim's revision in a tree BEN has never seen.
		return false, fmt.Errorf("%w: %s was attached to sandbox %s and acquired %s",
			remote.ErrClaimMismatch, c.Claim(), before.SandboxID, id.SandboxID)
	}
	return before.SandboxID == "", nil
}

// runHook fires one §5.2.6 lifecycle hook and applies its containment.
//
// The id names one firing, which is what makes a restarted prepare resolve the
// *same* firing instead of executing a mutation twice (remote.RunHook's durable
// record).
func (p *Provider) runHook(ctx context.Context, c Cycle, phase remote.HookPhase, attempt int) error {
	identity, err := p.identity(ctx, c)
	if err != nil {
		return err
	}
	err = remote.RunHook(ctx, p.hookExec, p.hookStore, remote.HookInvocation{
		Identity: identity, ID: hookID(c, phase, attempt), Phase: phase, Attempt: attempt,
		Hooks: p.hooks,
	})
	if remote.Aborts(phase, err) {
		return err
	}
	if err != nil {
		p.log.Warn("lifecycle hook failed; continuing (SPEC §5.2.6)",
			"issue", c.Issue, "phase", string(phase), "error", err)
	}
	return nil
}

// identity reads the backend's current identity for a cycle. Every hook and the
// restore are addressed by it, and it is read rather than remembered because the
// pinned profile revision is the backend's fact and a stale copy would address a
// world that has moved.
func (p *Provider) identity(ctx context.Context, c Cycle) (remote.Identity, error) {
	id, err := p.backend.Attach(ctx, c.Claim())
	if err != nil {
		return remote.Identity{}, err
	}
	if !id.Complete() {
		return remote.Identity{}, fmt.Errorf("%w: %s", remote.ErrIdentityMissing, c.Claim())
	}
	return id, nil
}

// hookID names one script firing durably, and the epoch is in it deliberately.
//
// A hook record is keyed by (claim, id), and the claim here is the *approval*
// cycle — which outlives an assignment. So a reassignment inside one approval
// runs attempt 1 a second time, and an id of phase-and-attempt alone would
// address the previous assignment's firing: the durable record would be found,
// its request digest would not match (the tree, and so the script, have moved),
// and every reassignment would abort on ErrHookMismatch instead of running.
//
// The digest still refuses a *changed* request under an id that matched, which
// is the case a restart hits when the canonical head moved while the daemon was
// down. That refusal is correct and stays: a restore that may already have run
// targeted a different commit, so the attempt is failed and a later one prepares
// afresh rather than resolving a firing that did something else.
func hookID(c Cycle, phase remote.HookPhase, attempt int) remote.HookID {
	return remote.HookID(fmt.Sprintf("%d-%s-%d", c.Epoch, phase, attempt))
}
