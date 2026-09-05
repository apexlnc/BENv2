package remotews

import (
	"context"
	"errors"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// AfterRun fires the §6.5 after-run hook, discovered by the orchestrator rather
// than required of the seam (orchestrator.afterRunner).
//
// Its failure is logged and ignored, which is §5.2.6's containment for this
// phase and not a shortcut: the hook fires after every attempt whatever its
// outcome, and a failing one must not be able to change what the attempt was.
func (p *Provider) AfterRun(ctx context.Context, ws core.Workspace) {
	c, err := p.load(ws.Key, "")
	if err != nil {
		if !errors.Is(err, ErrNoCycle) {
			p.log.Warn("after_run hook skipped: the workspace cycle could not be read",
				"workspace", ws.Key, "error", err)
		}
		return
	}
	if err := p.runHook(ctx, c, remote.HookAfterRun, c.Attempt); err != nil {
		p.log.Warn("after_run hook failed", "issue", c.Issue, "error", err)
	}
}

// Dispose is SPEC §6.1's compatibility surface. The boolean distinguishes a
// published claim from a failed one; revocation and shutdown arrive through the
// outcome-specific methods below because the local contract cannot name them.
func (p *Provider) Dispose(ctx context.Context, ws core.Workspace, keep bool) error {
	outcome := OutcomePublished
	if keep {
		outcome = OutcomeFailed
	}
	return p.complete(ctx, ws, outcome, true)
}

// CompleteFailure applies the remote backend's failed-claim policy. Local
// providers do not implement this optional orchestrator seam and retain their
// existing §6.4 behavior.
func (p *Provider) CompleteFailure(ctx context.Context, ws core.Workspace) error {
	return p.complete(ctx, ws, OutcomeFailed, true)
}

// CompleteEndedCycle applies the configured `on_revoked` policy to a workspace
// cycle whose approval has ended, named by address.
//
// The address is not decoration: an independently durable obligation resolves
// through its own record, while an ordinary live-cycle completion must still
// match the current record. An unknown or empty address is ErrCycleMoved rather
// than authority to dispose whichever cycle happens to occupy the issue key.
func (p *Provider) CompleteEndedCycle(ctx context.Context, ws core.Workspace) error {
	if ws.Path == "" {
		return fmt.Errorf("%w: an ended cycle has no address", ErrCycleMoved)
	}
	unlock := p.completionLocks.lock(ws.Path)
	defer unlock()

	disposal, err := p.store.LoadCycleDisposal(ws.Path)
	switch {
	case err == nil:
		if err := p.validateDisposalRepository(disposal); err != nil {
			return err
		}
		if ws.Key != disposal.Cycle.Key {
			return fmt.Errorf("%w: address %s belongs to key %s, asked for %s",
				ErrCycleState, ws.Path, disposal.Cycle.Key, ws.Key)
		}
		return p.completeCycleDisposal(ctx, disposal)
	case !errors.Is(err, ErrNoCycleDisposal):
		return err
	default:
		return p.complete(ctx, ws, OutcomeRevoked, true)
	}
}

// completeCycleDisposal applies an independently durable obligation to the old
// cycle identity it contains. It never resolves through the live cycle record.
func (p *Provider) completeCycleDisposal(ctx context.Context, disposal CycleDisposal) error {
	if disposal.Disposition == dispositionRetained {
		// Repeating the value finishes a possibly ambiguous directory sync. Only
		// a durable confirmation may make the policy disappear from EndedCycles.
		return p.store.SetCycleDisposalDisposition(disposal.Address(), dispositionRetained)
	}

	// The obligation is written before the replacement cycle. A crash between
	// those writes is expected and must not turn the old cycle into both the live
	// record and the object being deleted. The BeginClaimBase retry finishes the
	// replacement; until then this remains owed.
	if err := p.store.WithCycle(disposal.Cycle.Key, func() error {
		current, err := p.loadForTransition(disposal.Cycle.Key, disposal.Cycle.Issue)
		switch {
		case errors.Is(err, ErrNoCycle):
			return nil
		case err != nil:
			return err
		case current.Address() == disposal.Address():
			return fmt.Errorf("%w: %s still names the cycle awaiting disposal",
				ErrCycleReplacementPending, disposal.Address())
		default:
			// A prior replacement write may be visible even though its directory
			// sync returned an error. Replacing the same validated snapshot and
			// syncing it here turns "different address" into durable evidence before
			// the old backend address can be destroyed.
			return p.store.SaveCycle(current)
		}
	}); err != nil {
		return err
	}

	if disposal.Disposition == dispositionDeleted {
		// As above, but before the pin is retired: a visible confirmation whose
		// directory sync failed is not yet the ordering fact cleanup requires.
		if err := p.store.SetCycleDisposalDisposition(disposal.Address(), dispositionDeleted); err != nil {
			return err
		}
	} else {
		if p.disposer == nil {
			return fmt.Errorf("%w: issue %s", ErrNoDisposer, disposal.Cycle.Issue)
		}
		prev, _, err := p.settle(ctx, disposal.Cycle)
		if err != nil {
			return err
		}
		if err := p.runHook(ctx, disposal.Cycle, remote.HookBeforeRemove, disposal.Cycle.Attempt); err != nil {
			p.log.Warn("before_remove hook failed; continuing with the disposal",
				"issue", disposal.Cycle.Issue, "error", err)
		}
		disposition, err := p.disposer.Complete(ctx, disposal.Cycle.Claim(), OutcomeRevoked, prev)
		if err != nil {
			return err
		}
		switch disposition {
		case DispositionRetained:
			// The policy has been applied exactly once for this process, while the
			// record and pin remain as the durable address of what it retained.
			return p.store.SetCycleDisposalDisposition(disposal.Address(), dispositionRetained)
		case DispositionDeleted:
			// Confirmation first. If local cleanup crashes, recovery sees
			// dispositionDeleted and resumes below without issuing a second backend
			// disposal.
			if err := p.store.SetCycleDisposalDisposition(disposal.Address(), dispositionDeleted); err != nil {
				return err
			}
		default:
			return fmt.Errorf("remotews: disposer returned unknown %s for issue %s",
				disposition, disposal.Cycle.Issue)
		}
	}

	// The backend has confirmed compute release, volume destruction and record
	// tombstoning. Only now may the verification pin and the obligation address
	// disappear, in that order.
	if err := p.discardPin(ctx, disposal.Cycle); err != nil {
		return err
	}
	gone, err := p.store.DeleteCycleDisposal(disposal.Address())
	if gone {
		if err != nil {
			// The process sees no obligation to retry. If the directory sync did not
			// survive a crash, the confirmed record reappears and startup enumerates
			// it; either outcome retains no unaddressed backend work.
			p.log.Warn("ended-cycle obligation was removed but its directory sync failed; recovery will retry if it reappears",
				"issue", disposal.Cycle.Issue, "address", disposal.Address(), "error", err)
		}
		return nil
	}
	return err
}

// CycleApproval reports the standing approval the workspace cycle recorded for
// this issue is anchored to, or zero when there is no record.
//
// A read, and nothing more. It exists because the loop must not answer "is this
// retained sandbox still the one the approval selects" for itself: the durable
// record is the authority on which cycle a sandbox belongs to, and an earlier
// version of #252 had the loop remember the approval it computed when the claim
// was converted — which a withdrawal *before* that conversion made wrong, since
// it remembered the new approval beside the old sandbox.
//
// The comparison stays with the caller, which has just read the change log for
// its own purposes and can compute the standing approval from it without a second
// tracker call.
func (p *Provider) CycleApproval(ctx context.Context, issue core.Issue) (int64, error) {
	key, err := p.key(issue)
	if err != nil {
		return 0, err
	}
	c, err := p.load(key, issue.Identifier)
	switch {
	case errors.Is(err, ErrNoCycle):
		return 0, nil
	case err != nil:
		return 0, err
	}
	return c.Approval, nil
}

// CompleteShutdown applies the shutdown policy without retiring the cycle or
// its verification pin. The tracker claim remains standing across shutdown, so
// even an operator-selected delete must leave enough daemon-side identity for
// startup recovery to reacquire the cycle deliberately.
func (p *Provider) CompleteShutdown(ctx context.Context, ws core.Workspace) error {
	return p.complete(ctx, ws, OutcomeShutdown, false)
}

// complete ends this daemon tenure's use of a remote workspace: the
// before_remove hook, then the configured retention policy, then — only when
// the workspace was actually deleted and the claim ended — the durable records.
//
// What is not a policy question is the gate: nothing is suspended or deleted
// while a run's termination is unconfirmed, and that refusal is applied by the
// disposer as well as by the settle below (remote.MayReuse, SPEC §9.8).
//
// The order is the contract. before_remove runs while the workspace still
// exists; the disposal happens next; the pin and the cycle record are retired
// last, and only after a delete has actually landed — a record removed over a
// retained or suspended workspace is a lease nothing will ever release, which
// docs/AIRLOCK.md names as the failure worse than a stale record.
func (p *Provider) complete(ctx context.Context, ws core.Workspace, outcome Outcome, endClaim bool) error {
	// A completion of the current cycle and a replacement both rewrite the one
	// live record. Hold the per-key transaction through backend confirmation so
	// BeginClaimBase cannot first turn this same cycle into a durable obligation,
	// then watch this path dispose it without confirming that obligation. Old-cycle
	// completion does not use this path: its independent record lets replacement
	// work proceed while the delete is in flight.
	return p.store.WithCycle(ws.Key, func() error {
		return p.completeCurrent(ctx, ws, outcome, endClaim)
	})
}

func (p *Provider) completeCurrent(ctx context.Context, ws core.Workspace, outcome Outcome, endClaim bool) error {
	c, err := p.load(ws.Key, "")
	switch {
	case errors.Is(err, ErrNoCycle):
		// Already disposed. A repeated disposal after a crash must not fail.
		return nil
	case err != nil:
		return err
	}
	// The record is resolved by workspace *key*, which is per issue and outlives
	// every cycle under it — so the record found here is not necessarily the cycle
	// the caller means. A completion owed for one cycle and applied after a
	// revocation and a reapproval would suspend or delete the *replacement's*
	// sandbox and retire its pin, which is the one disposal nobody asked for
	// (BeginClaimBase, #252).
	//
	// The address carries the approval anchor, so the caller already holds the
	// identity it means and this is a comparison rather than a new field.
	//
	// Unconditional, including the empty address. Every caller has one — a
	// provider-produced core.Workspace always carries Cycle.Address, whether it
	// came from a prepare, a resolve, or the §9.10 step 5 listing's Cycle.Ref — so
	// a key-only workspace is not a caller keeping an older contract, it is a
	// caller that cannot say which cycle it means, and the permissive reading of
	// that disposes whichever one happens to occupy the key.
	if ws.Path != c.Address() {
		return fmt.Errorf("%w: %s is owed for %q and the record now names %s",
			ErrCycleMoved, c.Key, ws.Path, c.Address())
	}
	if p.disposer == nil {
		return fmt.Errorf("%w: issue %s", ErrNoDisposer, c.Issue)
	}

	prev, _, err := p.settle(ctx, c)
	if err != nil {
		return err
	}
	if err := p.runHook(ctx, c, remote.HookBeforeRemove, c.Attempt); err != nil {
		// ContainmentIgnore: before_remove must not be able to prevent a disposal.
		// runHook has already logged it; reaching here at all would be a phase
		// misclassification rather than a hook failure.
		p.log.Warn("before_remove hook failed; continuing with the disposal", "issue", c.Issue, "error", err)
	}

	disposition, err := p.disposer.Complete(ctx, c.Claim(), outcome, prev)
	if err != nil {
		return err
	}
	switch disposition {
	case DispositionRetained:
		// The workspace is still addressable. Its cycle record is the only thing
		// that can name it for a human or a later assignment under this approval.
		return nil
	case DispositionDeleted:
		if !endClaim {
			return nil
		}
		return p.retireCurrent(ctx, c)
	default:
		return fmt.Errorf("remotews: disposer returned unknown %s for issue %s", disposition, c.Issue)
	}
}

// retire removes what a finished claim cycle leaves behind: the daemon-side pin
// and the cycle record, in that order.
//
// The pin first, because the record is what makes the pin findable. The reverse
// order leaves a pin in the trusted store that nothing names — harmless to
// correctness and impossible to clean up, since the only index into it is the
// record that was just removed.
func (p *Provider) retireCurrent(ctx context.Context, c Cycle) error {
	if err := p.discardPin(ctx, c); err != nil {
		return err
	}
	current, err := p.loadForTransition(c.Key, c.Issue)
	switch {
	case errors.Is(err, ErrNoCycle):
		return nil
	case err != nil:
		return err
	case current.Address() != c.Address():
		// Defensive even though complete holds the per-key transaction: a Store
		// implementation that violates WithCycle must still not delete live work.
		return nil
	default:
		return p.store.DeleteCycle(c.Key)
	}
}

// discardPin drops the claim's pin through the optional discard seam. A fact
// source that does not offer one keeps its pins, which costs a ref and answers
// the next cycle's RecordClaim identically (mirror.RecordClaim re-pins a new
// epoch).
func (p *Provider) discardPin(ctx context.Context, c Cycle) error {
	discarder, ok := p.base.(interface {
		Discard(ctx context.Context, ref core.RemoteClaimRef) error
	})
	if !ok {
		return nil
	}
	return discarder.Discard(ctx, c.ClaimRef())
}
