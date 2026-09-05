package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// VerifyResult is what the §9.7 check concluded, plus what the publish
// milestone needs to say.
type VerifyResult struct {
	Verdict Verdict
	// PRURL is the published pull request, set when VerdictPublished.
	PRURL string
	// Detail is one operator-facing line, carried into a needs-review
	// comment.
	Detail string
}

// beginClaim starts a fresh dispatch: queued → claimed, then the claim write
// and its read-back verification in a worker (SPEC §8.4).
func (o *Orchestrator) beginClaim(ctx context.Context, issue core.Issue, cur snapshot) {
	r := &Record{
		Issue: issue,
		State: StateQueued,
		// The definition dispatch accepted this issue under. It is what the
		// record holds until it launches, at which point beginStart replaces it
		// with whatever is in force then (§5.4 gives launches to the reload) —
		// so this is never the definition a run executes under, only the one
		// that decided the run should exist.
		Definition: cur.Definition,
		Attempt:    1,
		UpdatedAt:  o.clock.Now(),
		token:      o.newToken(),
		instance:   o.instance,
	}
	o.records[issue.Identifier] = r
	o.publish(r)

	// The record stays in queued until the claim verifies. §9.2's trigger for
	// queued → claimed is "Claim() verified by read-back", and the transition
	// projects ben:claimed — so projecting first would leave a state label on
	// an issue we do not own, and §8.3 excludes any issue carrying one. A
	// lost race would block the issue for everyone, permanently.
	gen, token := r.generation, r.token
	// The tracker dispatch decided under, used to completion. The claim is a
	// write, so it must land through the adapter whose principal the decision was
	// made against.
	tracker := cur.Runtime.Tracker
	r.pending++
	go func() {
		verified, err := tracker.Claim(ctx, issue)
		o.send(ctx, signal{kind: sigClaimed, issue: issue.Identifier, generation: gen, token: token, verified: verified, err: err})
	}()
}

func (o *Orchestrator) onClaimed(ctx context.Context, r *Record, s signal) {
	r.pending--
	if o.finishIfRequested(ctx, r) {
		return
	}
	if s.err != nil {
		if errors.Is(s.err, core.ErrClaimNotAttempted) {
			// The adapter can prove it never wrote: a spent request budget, a
			// standing rate-limit refusal, a claim it never began. There is no
			// assignment to unwind, and the release below is itself a write —
			// paying it here would spend the write capacity the refusal was
			// reporting gone, and hold a §9.5 concurrency slot for an issue this
			// daemon does not own.
			//
			// Nothing was projected (see beginClaim), so the issue is left exactly
			// as it was found and an ordinary poll dispatches it again.
			if !o.logCredentialFailure("claim was refused before any assignment; leaving the issue queued",
				r.Issue.Identifier, s.err) {
				o.log.Info("claim was refused before any assignment; leaving the issue queued",
					"issue", r.Issue.Identifier, "error", s.err)
			}
			o.forget(r.Issue.Identifier)
			return
		}
		// An error is not the same answer as a refusal, and only one of them
		// comes with the unwind guarantee. `false, nil` is the adapter saying
		// it released whatever partial claim it made; an *error* is the
		// adapter saying it could not finish — and its two riskiest paths, an
		// unverifiable read-back and an unorderable race, both attempt a
		// release and both return a joined error when that release fails too
		// (github Claim). That error is precisely the case where an
		// assignment is standing and nobody is tracking it.
		//
		// Forgetting here would leave assigned-with-no-state-label, which
		// §9.10 step 3 reads as published-awaiting-review and never touches
		// again — "the worst outcome available", in the adapter's own words.
		// So keep the record and owe a Release: idempotent, scoped to our own
		// principal (§9.10), retried every tick until the tracker confirms it,
		// and a harmless no-op on the paths that never assigned anything. The
		// orchestrator cannot tell those apart from the paths that did, and
		// this is the direction that fails closed.
		if !o.logCredentialFailure("claim failed; releasing any assignment it may have left",
			r.Issue.Identifier, s.err) {
			o.log.Warn("claim failed; releasing any assignment it may have left",
				"issue", r.Issue.Identifier, "error", s.err)
		}
		o.release(ctx, r, "claim could not be verified: "+s.err.Error())
		return
	}
	if !s.verified {
		// A claim that did not stick is not ours to work. Nothing was
		// projected and the adapter has already released whatever partial
		// claim it made (SPEC §8.4), so the issue is left exactly as it was
		// found.
		o.forget(r.Issue.Identifier)
		return
	}

	// The claim is ours; the content it approves is the remaining question
	// (SPEC §9.5). The record stays in `queued` until it is answered, for the
	// same reason it stayed there until the claim verified: `queued → claimed`
	// projects ben:claimed, and an issue that is about to park for reapproval
	// should not first be announced as claimed and commented on.
	r.claimVerified = true
	o.beginApproval(ctx, r)
}

// beginApproval reads the two facts §9.5's check needs and reports them back.
//
// Two reads, one worker. The change log dates the approving instant — every
// `labeled` event is already projected onto core.ClaimEvent and ClaimHistory is
// already the cache-bypassing read — and the content read states the bytes and
// when they were last edited, together, so the pin is taken from the same
// response the check was made against.
//
// After the claim rather than before it: parking for reapproval requires owning
// the issue. A check that refused before the claim would have to leave the issue
// unclaimed and unlabelled, which is the queue handing it straight back on the
// next poll.
func (o *Orchestrator) beginApproval(ctx context.Context, r *Record) {
	if r.approvalInFlight || r.exiting() || o.draining {
		return
	}
	r.approvalInFlight = true
	gen, token := r.generation, r.token
	issue := r.Issue
	// One snapshot, captured at entry and carried to the verdict. The change log
	// and the content must come from the adapter whose principal holds the claim,
	// and the label set the approving instant is computed over must be the one
	// that adapter was built from — a reload landing mid-read would otherwise date
	// the approval against a required set nobody dispatched under (SPEC §5.4).
	cur := o.configNow()
	tracker := cur.Runtime.Tracker
	required := cur.Definition.Config.Tracker.RequiredLabels
	principal := cur.Runtime.ClaimPrincipal
	r.pending++
	go func() {
		s := signal{
			kind: sigApproved, issue: issue.Identifier, generation: gen, token: token,
			requiredLabels: required, claimPrincipal: principal,
		}
		// Both reads classify absence themselves (#134), which is what this path
		// needs and used to have to buy with a third read. The change log is the
		// first to fail for a deleted issue, and its answer says which failure it
		// is: a `queued` record is the one shape reconciliation never revisits
		// (§9.8 refreshes running and parked), so an error nothing could route on
		// held the claim and the §9.5 slot for the life of the process.
		history, err := tracker.ClaimHistory(ctx, issue)
		if err == nil {
			s.history = history
			s.approval, err = readApproval(ctx, tracker, issue)
		}
		s.err = err
		o.send(ctx, s)
	}()
}

// onApproved applies §9.5 to a fresh claim.
//
// Three outcomes, and the difference between the second and the third is the
// difference between a question BEN could not ask and an answer it does not
// like. A failed read is retried with the claim retained (retryPendingExits); a
// stated refusal parks for a human. Neither releases, and neither dispatches.
func (o *Orchestrator) onApproved(ctx context.Context, r *Record, s signal) {
	r.pending--
	r.approvalInFlight = false
	if o.finishIfRequested(ctx, r) {
		return
	}
	if r.exiting() || o.draining || r.State != StateQueued {
		return
	}
	if errors.Is(s.err, core.ErrIssueNotFound) {
		// Absent is a fact, not a read that failed — the same distinction §9.6's
		// re-fetch and §9.8's refresh both make. Retrying it would retry forever,
		// and a `queued` record is the one shape reconciliation never revisits
		// (§9.8 refreshes running records and sweeps parked ones), so nothing else would
		// notice: the §9.5 slot would be held for the life of the process by an
		// issue that no longer exists.
		//
		// The fact reaches here already classified: both reads the check makes
		// name one issue, and the adapter maps their not-found answer onto the
		// sentinel (core.ErrIssueNotFound).
		r.markGone()
		o.finishNow(ctx, r, false, "issue is gone from the tracker")
		return
	}
	if s.err != nil {
		// Could not ask. Fail closed and try again next tick rather than guess
		// in either direction — "assume unedited" is the guess this ticket
		// exists to remove (SPEC §9.5, §9.10).
		if !o.logCredentialFailure("reading the approval facts; holding the claim and retrying next tick",
			r.Issue.Identifier, s.err) {
			o.log.Warn("reading the approval facts; holding the claim and retrying next tick",
				"issue", r.Issue.Identifier, "error", s.err)
		}
		return
	}

	approved, err := checkApproval(s.history, s.requiredLabels, s.approval)
	if err != nil {
		// The claim is retained and the workspace does not exist yet: parking is
		// the whole of the effect. Reapproval is a labeler re-applying a required
		// label (§6.7), which the next dispatch decision reads afresh.
		o.enterNeedsReview(ctx, r, "content approval: "+err.Error(), "")
		return
	}
	// Pinned from the read the check was made against, and only now: the content
	// is admissible *because* the same response established that nothing has
	// edited it since the approving instant. BEN never reconstructs a historical
	// body (SPEC §9.5).
	r.pin(approved)
	epoch := claimCycleAnchor(s.history, s.claimPrincipal)
	if epoch <= 0 {
		// Claim verified current assignment, but the ordered history cannot name
		// the positive tracker event that established it. Epoch zero authorizes
		// nothing; this is a stated safety refusal rather than a base guessed from
		// the branch or the clock (SPEC §8.2, §9.5).
		o.enterEpochFault(ctx, r, "claim epoch: current assignment has no positive establishing event ID")
		return
	}
	r.claimEpoch = epoch
	o.beginClaimBase(ctx, r)
}

// beginClaimBase writes the provider-owned pending intent before any state
// projection. It is local durable I/O and therefore runs off the authority
// goroutine; failures retain the verified claim and are retried on a later tick.
func (o *Orchestrator) beginClaimBase(ctx context.Context, r *Record) {
	eligible := r.State == StateQueued || (r.State == StateBackoff && r.claimBaseDispatch)
	if r.claimBaseInFlight || r.exiting() || o.draining || !eligible || r.claimEpoch <= 0 {
		return
	}
	r.claimBaseInFlight = true
	gen, token := r.generation, r.token
	issue, epoch := r.Issue, r.claimEpoch
	workspaces := o.bundle().Workspaces
	cycleSource, cycleRead := workspaces.(endedCycleSource)
	var cycleSeq uint64
	if cycleRead {
		o.cycleMutationSeq++
		cycleSeq = o.cycleMutationSeq
		o.cycleMutationsInFlight++
	}
	r.pending++
	go func() {
		err := workspaces.BeginClaimBase(ctx, issue, epoch)
		var cycleRefs []core.WorkspaceRef
		var cycleErr error
		if cycleRead {
			cycleRefs, cycleErr = cycleSource.EndedCycles(ctx)
		}
		var state core.ClaimBase
		var stateErr error
		if err != nil {
			// Distinguish a failed write that left no contradiction (retry) from a
			// provider authority that names another epoch (sticky park). The read is
			// also what handles an ambiguous write that actually landed.
			state, stateErr = workspaces.ClaimBase(ctx, issue)
		}
		o.send(ctx, signal{
			kind: sigClaimBaseBegun, issue: issue.Identifier,
			generation: gen, token: token, err: err,
			claimBase: state, claimBaseErr: stateErr,
			cycleRead: cycleRead, cycleRefs: cycleRefs,
			cycleWorkspaces: workspaces, cycleScanErr: cycleErr, cycleScanSeq: cycleSeq,
		})
	}()
}

func (o *Orchestrator) onClaimBaseBegun(ctx context.Context, r *Record, s signal) {
	r.pending--
	r.claimBaseInFlight = false
	if o.finishIfRequested(ctx, r) {
		return
	}
	if r.exiting() || o.draining {
		return
	}
	if s.cycleRead && !s.cycleScanOK {
		// BeginClaimBase may already have installed the replacement. Do not
		// prepare it after its complete obligation-directory read was refused:
		// the loop cannot yet adopt the ownership fact, and the provider cannot
		// prove the full pin-retention set on that path either.
		return
	}
	if r.State != StateQueued && (r.State != StateBackoff || !r.claimBaseDispatch) {
		return
	}
	if s.err != nil {
		if errors.Is(s.claimBaseErr, core.ErrClaimTargetUnrecorded) &&
			legacyClaimBaseCanUpgrade(s.claimBase, r.claimEpoch) {
			o.log.Warn("upgrading a legacy claim target; holding the claim and retrying next tick",
				"issue", r.Issue.Identifier, "epoch", r.claimEpoch,
				"provider_epoch", s.claimBase.Epoch, "error", s.err)
			return
		}
		if s.claimBaseErr != nil {
			o.enterEpochFault(ctx, r, "claim epoch: pending intent is unreadable after initialization failed")
			return
		}
		switch {
		case claimBasePinsEpoch(s.claimBase, r.claimEpoch):
			// The authority says the operation completed despite the reported
			// error. Continue from the durable fact, not the syscall outcome.
		case s.claimBase.State == core.ClaimBaseAbsent,
			s.claimBase.State == core.ClaimBasePending && s.claimBase.Epoch == r.claimEpoch,
			claimBasePinsEpoch(s.claimBase, s.claimBase.Epoch) && s.claimBase.Epoch != r.claimEpoch:
			// Absence, our own pending intent, or a readable prior pin are the
			// expected states when initialization fails around its atomic replace.
			// None contradicts the current assignment; keep the claim unprojected
			// and retry the initialization.
			o.log.Warn("initializing the claim epoch; holding the claim and retrying next tick",
				"issue", r.Issue.Identifier, "epoch", r.claimEpoch,
				"provider_state", s.claimBase.State.String(), "provider_epoch", s.claimBase.Epoch,
				"error", s.err)
			return
		default:
			o.enterEpochFault(ctx, r, fmt.Sprintf(
				"claim epoch: initialization found contradictory provider state %+v", s.claimBase))
			return
		}
	}

	if r.State == StateBackoff {
		r.claimBaseDispatch = false
		r.Attempt++
		if err := o.transition(ctx, r, StatePreparing, legalTransitions[transition{StateBackoff, StatePreparing}]); err != nil {
			return
		}
		o.beginPrepare(ctx, r)
		return
	}

	if err := o.transition(ctx, r, StateClaimed, legalTransitions[transition{StateQueued, StateClaimed}]); err != nil {
		o.forget(r.Issue.Identifier)
		return
	}
	// The label write is awaited: preparing — and therefore the agent — may
	// not begin until the tracker carries ben:claimed (SPEC §9.10). The claim
	// milestone queues behind it, which is also the order B04's comment
	// markers need.
	// The label write is owed and awaited: preparing — and therefore the
	// agent — may not begin until the tracker confirms ben:claimed
	// (SPEC §9.10). The claim milestone queues behind it, which is also the
	// order B04's comment markers need.
	o.comment(ctx, r, core.MilestoneComment{Milestone: core.MilestoneClaimed})
}

// onClaimProjected runs when ben:claimed has actually landed.
func (o *Orchestrator) onClaimProjected(ctx context.Context, r *Record) {
	if r.State != StateClaimed || r.exiting() {
		// Reconciliation reached a verdict while the write was retrying.
		return
	}
	if err := o.transition(ctx, r, StatePreparing, legalTransitions[transition{StateClaimed, StatePreparing}]); err != nil {
		return
	}
	o.beginPrepare(ctx, r)
}

// beginPrepare asks the workspace provider for this attempt's worktree.
func (o *Orchestrator) beginPrepare(ctx context.Context, r *Record) {
	if o.draining {
		// A re-check at the door. Every caller — the claim path, the retry timer,
		// the continuation track — is already refused upstream by exiting() or by
		// onVerified's own suspended check, so removing this line fails no test.
		// It is kept because it is the door rather than a lock on one route to
		// it, and because the review of #101 found two routes I had missed by
		// reasoning about doors instead of about callers.
		return
	}
	// A fresh attempt: the after-run hook it may earn has not fired yet, no
	// process of its own has run or exited, and any verdict the last one
	// reported has already been routed.
	r.ranThisAttempt, r.afterRunFired = false, false
	r.eventsClosed, r.handleDone, r.domainQuiet, r.outcome = false, false, false, nil
	// Nothing has ordered this attempt stopped. Stated rather than left to the
	// stop that set it, because a §9.9 signal outliving its attempt is the whole
	// of #236: FailureReason beside it is sticky on purpose, and the difference
	// between the two is only visible if this line exists.
	r.budgetStop, r.finishAfterStop = false, false
	// The attempt-outcome record's per-attempt fields (#60). The start instant is
	// taken here, at the entry to `preparing`, rather than at the launch: the
	// worktree preparation is part of what a dispatch costs in wall clock, and an
	// attempt that never reaches `beginStart` has to have a duration too.
	//
	// The agent is taken here for the same reason and re-taken at the launch,
	// exactly as Definition is (see beginStart): an attempt that dies preparing
	// still names the adapter it was dispatched under, and one that launches names
	// the adapter it launched with.
	r.attemptStartedAt, r.attemptUsage, r.attemptRecorded = o.clock.Now(), core.Usage{}, false
	r.attemptAgent = o.bundle().Agent
	// And a fresh account to accumulate. previousAttempt is deliberately *not*
	// cleared: it is what this attempt's prompt is about to render, and the
	// composed string belongs to the attempt before it (SPEC §5.6, #61).
	r.outputTail, r.outputTotal = "", 0
	r.attemptFacts, r.attemptFactsRead = core.AttemptFacts{}, false
	r.summarizing, r.summarized, r.parkOnBudgetPending = false, false, false

	gen, token := r.generation, r.token
	issue, attempt, epoch := r.Issue, r.Attempt, r.claimEpoch
	// Captured at entry and used to completion. Prepare can take minutes — a
	// clone, a fetch, an after_create hook — and the hooks it fires are this
	// provider's: a hook edited during the call reaches the next Prepare, not
	// this one, because the scripts bound at New (SPEC §5.7) are what a readiness
	// verdict was given for.
	workspaces := o.bundle().Workspaces
	r.pending++
	go func() {
		var ws core.Workspace
		var facts core.LocalBranchFacts
		ws, facts, err := workspaces.PrepareClaim(ctx, issue, attempt, epoch)
		claimBase, claimBaseErr := workspaces.ClaimBase(ctx, issue)
		o.send(ctx, signal{
			kind: sigPrepared, issue: issue.Identifier, generation: gen, token: token,
			workspace: ws, facts: facts, err: err,
			claimBase: claimBase, claimBaseErr: claimBaseErr,
		})
	}()
}

func (o *Orchestrator) onPrepared(ctx context.Context, r *Record, s signal) {
	r.pending--
	if s.workspace.Path != "" {
		// Adopt it even alongside an error: the worktree provider returns the
		// workspace it kept for forensics (SPEC §6.6), and dropping it here
		// would leak the directory. Adopting before anything else can decide
		// to exit is also what lets a deferred finish dispose of it.
		r.Workspace = s.workspace
	}
	if o.finishIfRequested(ctx, r) {
		return
	}
	if o.draining {
		// Ahead of the failure branch, not after it. A Prepare that *failed*
		// routes through failAttempt, which transitions the record to backoff or
		// failed and then releases — a terminal projection and a release, both of
		// them things the drain must not make. Gating only the launch below left
		// exactly that path open: queued → claimed → preparing → failed, and the
		// claim gone with it.
		r.suspended = true
		return
	}
	if s.err != nil {
		if !claimBaseAllowsPrepare(s.claimBase, r.claimEpoch, s.claimBaseErr) {
			o.enterEpochFault(ctx, r, fmt.Sprintf(
				"claim epoch: prepare refused under provider state %+v: %v", s.claimBase, s.claimBaseErr))
			return
		}
		o.failLaunch(ctx, r, s.err, "preparing the workspace", o.prepRetryable(s.err))
		return
	}
	if s.claimBaseErr != nil || r.claimEpoch <= 0 || s.workspace.ClaimEpoch != r.claimEpoch ||
		!claimBaseAuthorizesWorkspace(s.claimBase, s.workspace) {
		o.enterEpochFault(ctx, r, fmt.Sprintf(
			"claim epoch: prepared workspace carries %d/%s/%s, provider returned %+v: %v; expected epoch %d and the same complete tuple",
			s.workspace.ClaimEpoch, s.workspace.BaseSHA, s.workspace.TargetBranch,
			s.claimBase, s.claimBaseErr, r.claimEpoch))
		return
	}
	if r.Attempt == 1 && (r.Workspace.PriorWork || s.facts.AdvancedPastBase(s.facts.BaseSHA)) {
		// This remains the first failure-budget dispatch of the fresh claim:
		// moving the baseline with the display floor keeps max_attempts
		// independent of history whose exact count did not survive (§9.6).
		// Local providers prove that history with branch facts; a remote
		// provider reports work already folded into its newly pinned base.
		r.Attempt = 2
		r.attemptBase = 1
	}
	o.beginStart(ctx, r)
}

func claimBaseAllowsPrepare(state core.ClaimBase, epoch int64, readErr error) bool {
	if readErr != nil || epoch <= 0 || state.Epoch != epoch {
		return false
	}
	return state.State == core.ClaimBasePending ||
		claimBasePinsEpoch(state, epoch)
}

func claimBasePinsEpoch(state core.ClaimBase, epoch int64) bool {
	return epoch > 0 && state.State == core.ClaimBasePinned && state.Epoch == epoch &&
		state.BaseSHA != "" && state.TargetBranch != ""
}

func claimBaseAuthorizesWorkspace(state core.ClaimBase, ws core.Workspace) bool {
	return ws.ClaimEpoch > 0 && ws.BaseSHA != "" && ws.TargetBranch != "" &&
		claimBasePinsEpoch(state, ws.ClaimEpoch) &&
		state.BaseSHA == ws.BaseSHA && state.TargetBranch == ws.TargetBranch
}

func legacyClaimBaseCanUpgrade(state core.ClaimBase, epoch int64) bool {
	return epoch > 0 && state.State == core.ClaimBasePinned && state.Epoch > 0 &&
		state.Epoch != epoch && state.BaseSHA != "" && state.TargetBranch == ""
}

// prepRetryable classifies a Prepare failure. SPEC §9.2 has both a retryable
// prep edge (→ backoff) and a non-retryable one (→ failed), and §6.6 fails
// closed on ambiguity — so without a classifier, a prep failure never
// retries. B11's wiring supplies one that recognizes its provider's
// hook-failure sentinel; the orchestrator stays provider-agnostic.
func (o *Orchestrator) prepRetryable(err error) bool {
	if o.cfg.PrepRetryable == nil {
		return false
	}
	return o.cfg.PrepRetryable(err)
}

// beginStart renders the prompt and starts the agent.
//
// This is the launch, and §5.4 gives launches to the reload: the definition is
// taken here rather than carried from the claim, because Prepare can take
// minutes — a clone, a fetch, an after_create hook — and an edit saved during
// it would otherwise not reach the run it was plainly meant for. Everything
// downstream of this line reads r.Definition, so from here the attempt is
// pinned: §5.4's other half is that a reload never disturbs a run already
// going, and the budget enforced against a live run's cost events is this
// same snapshot (see maxCost).
func (o *Orchestrator) beginStart(ctx context.Context, r *Record) {
	if o.draining {
		// A workspace prepared just before the signal is left prepared. The
		// record keeps its claim and its label, and §9.10 resumes it — where
		// launching an agent the drain would immediately have to interrupt
		// spends an attempt to accomplish nothing.
		//
		// Also a re-check now: onPrepared refuses first, because it has to — the
		// failure branch it guards routes to `failed` and releases, and gating
		// only the launch left that path open (#101 review, finding 2).
		return
	}
	// The launch resolves both the definition and the runner from what is in
	// force *now*, together. Taking the runner from the dispatch instead would
	// launch through an adapter built for a configuration a human has already
	// replaced, while the limits and prompt below came from the new one — which is
	// exactly the mismatch this ticket exists to remove.
	cur := o.configNow()
	r.Definition = cur.Definition
	// Pinned from the same snapshot as the definition and the runner below, so
	// the attempt's outcome record names the adapter that actually launched it
	// rather than whichever one a later reload installed (#60).
	r.attemptAgent = cur.Runtime.Agent

	prompt, err := o.renderPrompt(r)
	if err != nil {
		// A render failure fails only this attempt (SPEC §5.7, "contained at
		// run"), and re-rendering the same template with the same inputs
		// would fail the same way, so it does not retry.
		o.failAttempt(ctx, r, core.FailureLaunchError, false, "rendering the prompt: "+err.Error())
		return
	}

	spec := core.RunSpec{
		// The whole path set, copied rather than picked apart: the provider
		// reports these together (SPEC §6.1) and an adapter may derive none of
		// them (SPEC §7.1), so assembling one attempt's paths from more than
		// one workspace record must not be expressible here.
		Workspace:    r.Workspace.WorkspacePaths,
		Prompt:       prompt,
		Continuation: r.Continuation,
		// SPEC §7.6: BEN_ is reserved to the orchestrator and RunSpec.Env may
		// carry nothing else. These are the run's own coordinates — what the
		// agent would otherwise have to parse back out of its prompt. No
		// provider block: it binds at New, so Ready cannot verify one
		// configuration while Start launches another (SPEC §7.1).
		Env: map[string]string{
			"BEN_ISSUE":   r.Issue.Identifier,
			"BEN_ATTEMPT": strconv.Itoa(r.Attempt),
			// The §10.3 correlation handle, so the one participant that is a
			// separate process can put it on anything it emits. Without it the
			// agent's own output is the only part of a run that cannot be joined
			// to the daemon's log, the transition log and the run record.
			"BEN_RUN_ID":    r.runID(),
			"BEN_WORKSPACE": r.Workspace.Path,
			"BEN_BRANCH":    r.Workspace.Branch,
		},
		Limits: core.RunLimits{
			StallTimeout:   time.Duration(r.Definition.Config.Limits.StallTimeoutMS) * time.Millisecond,
			AttemptTimeout: time.Duration(r.Definition.Config.Limits.AttemptTimeoutMS) * time.Millisecond,
			MaxTurns:       r.Definition.Config.Limits.MaxTurns,
			MaxCostUSD:     maxCost(r),
		},
	}

	gen, token := r.generation, r.token
	id, issue := r.Issue.Identifier, r.Issue
	runner, workspaces := cur.Runtime.Runner, cur.Runtime.Workspaces

	// Before the worker, not after the launch reports back. The worker writes this
	// attempt's marker and then calls Start, which can block for as long as a process
	// takes to exec — and a tick in that window would retry a clear owed against the
	// key the new marker now occupies, leaving a live process with no marker at all.
	// Abandoning here closes the window: nothing can be retried against a key this
	// turn has already committed to overwriting.
	//
	// Scoped to the provider about to write, because that is whose file is being
	// replaced. A clear owed against a *previous* root is about a different file
	// entirely, and forgetting it would strand a marker nothing ever removes.
	clearsInFlight := o.abandonPendingClears(id, markerStoreFor(cur.Runtime))
	r.pending++
	go func() {
		// Any removal that was already executing when this attempt abandoned it,
		// waited out before the write. Abandoning is a decision the loop takes and the
		// removal does not run there, so the two can overlap — and an `os.Remove`
		// landing after this `writeMarker` leaves a live process with no marker at all,
		// which is the deletion the abandon exists to prevent, one goroutine later.
		for _, done := range clearsInFlight {
			select {
			case <-done:
			case <-ctx.Done():
				o.send(ctx, signal{kind: sigStarted, issue: id, generation: gen, token: token,
					err: fmt.Errorf("waiting out a previous run marker removal: %w", ctx.Err())})
				return
			}
		}
		// §9.10's workspace precondition, written **before** the launch and durable
		// on return. The ordering is the whole design: the execution domain does
		// not exist until launch, so a marker written afterwards leaves a window
		// in which a crash strands a live domain with nothing recording it —
		// and a workspace with no marker reads as free, which is a second agent in a
		// worktree at the next start. Writing first inverts the failure: a crash in
		// the window leaves a marker carrying no evidence, which §9.10 parks for a
		// human rather than reusing.
		//
		// In the worker rather than on the loop because it costs an fsync, and I/O
		// on the authority goroutine blocks every run in flight. Nothing is lost by
		// moving it: the launch it must precede is in here too, so the ordering is
		// still a straight line.
		//
		// A marker that cannot be written fails the attempt instead of launching
		// anyway. Launching would put a run in a workspace nothing can account for
		// across a restart, which is exactly what this precondition exists to make
		// impossible. Retryable: it is a local write, so the fault is the state
		// directory rather than anything about this issue.
		if err := workspaces.BeginRunMarkerFor(issue); err != nil {
			o.send(ctx, signal{kind: sigStarted, issue: id, generation: gen, token: token,
				err: fmt.Errorf("recording the run marker before launch: %w", err)})
			return
		}
		handle, err := runner.Start(ctx, spec)
		o.send(ctx, signal{kind: sigStarted, issue: id, generation: gen, token: token, handle: handle, err: err})
	}()
}

func maxCost(r *Record) float64 {
	if c := r.Definition.Config.Limits.MaxCostUSD; c != nil {
		return *c
	}
	return 0
}

func (o *Orchestrator) onStarted(ctx context.Context, r *Record, s signal) {
	r.pending--
	if s.handle != nil {
		// Adopt the handle first: if an exit is already pending, it has to
		// stop this process rather than abandon it.
		r.handle = s.handle
	}
	if s.err != nil && s.handle == nil {
		// No unowned execution domain exists: Start may return without a handle
		// only before provider release or after its trusted domain was torn down.
		// The marker beginStart wrote before trying is now describing nothing.
		//
		// Cleared *here*, ahead of every branch below, because all of them can
		// return: a finish ordered by reconciliation, and a drain that suspends the
		// record, both leave the failure branch unreached. Nothing else will ever
		// clear it — this attempt has no domain to confirm quiet, so confirmQuiet is
		// not on its path — and what survives is the marker's "interrupted cleanup of
		// a launch that failed" reading, which parks the issue at the next start.
		o.freeWorkspaceMarker(ctx, r)
	}
	if o.finishIfRequested(ctx, r) {
		return
	}
	if o.draining {
		// The launch was already in flight when the signal landed. The handle is
		// adopted above whatever happens — a process nobody holds a handle for is
		// a process nothing can stop — and the record is suspended: no
		// transition, no projection, no routing. driveShutdown interrupts it on
		// this same turn.
		//
		// The pump still starts, when there is something to pump. It is what
		// drains Events and waits on Done, and a harness whose consumer stopped
		// reading parks with its transcript half written (SPEC §7.2) — so
		// skipping it would trade the record of this run for nothing.
		//
		// onRunEvent routes nothing it delivers, because the record never enters
		// `running` — but it does *account* what the run spends, which is not the
		// same thing. This run is billed like any other, and the attempt record it
		// is about to produce has to say so (#60).
		//
		// A launch that *failed* has no handle, and both of these are about a
		// process: pumping a nil handle dereferences it, and ranThisAttempt would
		// promise the §6.5 after-run hook a workspace no agent ever ran in.
		r.suspended = true
		if s.handle != nil {
			r.ranThisAttempt = true
			o.pumpRun(ctx, r, s.handle)
		}
		return
	}
	if s.err != nil {
		// The marker for a launch that never happened is already cleared above,
		// ahead of the branches that can return before this one.
		//
		// A publish credential is minted *before* the process exists, so a mint
		// failure lands here with no handle: no `launch_error` is recorded, no
		// agent ran, and the routing is the credential's rather than the
		// launch's (SPEC §9.8).
		o.failLaunch(ctx, r, s.err, "starting the agent", false)
		return
	}
	if err := o.transition(ctx, r, StateRunning, legalTransitions[transition{StatePreparing, StateRunning}]); err != nil {
		return
	}

	r.ranThisAttempt = true
	o.pumpRun(ctx, r, s.handle)
}

// pumpRun drains the run's event stream and reports its two edges. Shared by the
// ordinary launch and by one adopted mid-drain, because both need the draining:
// the consumer is what keeps the harness writing its transcript, whatever the
// orchestrator intends to do with the events.
func (o *Orchestrator) pumpRun(ctx context.Context, r *Record, handle core.RunHandle) {
	runCtx, cancel := context.WithCancel(ctx)
	r.cancelRun = cancel

	gen, token := r.generation, r.token
	id := r.Issue.Identifier
	go func() {
		sawTerminal := false
		for {
			select {
			case <-runCtx.Done():
				return
			case ev, ok := <-handle.Events():
				if !ok {
					// Two edges, reported separately, because they permit
					// different things (#79).
					//
					// The stream closing says only that the adapter has nothing
					// further to say. It used to wait for Done before reporting
					// anything, which made progress depend on direct execution
					// ending: a failed teardown can leave it and its transcript
					// pipes live, so Done never closes and the record otherwise
					// sits in `running` with its outcome held,
					// unprobed and unlogged. Visibility cannot wait on that.
					o.send(ctx, signal{kind: sigEventsClosed, issue: id, generation: gen, token: token, sawTerminal: sawTerminal})

					// Done is the phase edge: after it, any remaining domain
					// member has outlived direct execution, so bounded teardown
					// is permissible. Before it, the only permissible
					// question is Probe. Waiting here also keeps the ordinary
					// run prompt — without this wake, a run whose domain is quiet
					// a moment after its stream closed would sit until the next
					// poll, 30 s by default.
					//
					// If direct execution never ends this waits for its life,
					// which is the honest shape: there is no phase edge
					// to report, and the probe path is already carrying the
					// record.
					select {
					case <-runCtx.Done():
					case <-handle.Done():
						o.send(ctx, signal{kind: sigHandleDone, issue: id, generation: gen, token: token})
					}
					return
				}
				if ev.Type == core.EventSucceeded || ev.Type == core.EventFailed {
					sawTerminal = true
				}
				o.send(ctx, signal{kind: sigRunEvent, issue: id, generation: gen, token: token, event: ev})
			}
		}
	}()
}

func (o *Orchestrator) onRunEvent(ctx context.Context, r *Record, s signal) {
	// Usage is accounted before the state gate below, and only usage.
	//
	// It is an *observation of spend*, not a routing input: the tokens were
	// bought and the money is gone whatever the record has since decided. The
	// gate exists to stop a late event **re-routing** a settled record, and it
	// does that job on the events that route. Applied to this one it silently
	// discarded real cost — sharply so for a run adopted during a drain, which
	// stays in `preparing` for its whole life (onStarted) and would report a
	// `ran=true` attempt that somehow spent nothing (#60).
	if s.event.Type == core.EventUsage && s.event.Usage != nil {
		r.attemptUsage.InputTokens += s.event.Usage.InputTokens
		r.attemptUsage.OutputTokens += s.event.Usage.OutputTokens
		r.attemptUsage.CostUSD += s.event.Usage.CostUSD
		// The §9.9 running total is per issue and is never reset, so it takes
		// this spend too. What it does *not* do here is act on it — that is the
		// switch below, behind the gate, because stopping a run is a decision and
		// this is bookkeeping.
		r.costUSD += s.event.Usage.CostUSD
	}
	if r.State != StateRunning {
		// A queued event from a run whose outcome is already decided —
		// notably a crash report arriving after a confirmed budget park,
		// which would otherwise move needs-review back to backoff.
		return
	}
	switch s.event.Type {
	case core.EventStarted:
		if s.event.Continuation != "" {
			r.Continuation = s.event.Continuation
		}
		// §10.3's third correlation attribute. Unlike the continuation token this
		// is never carried forward: it names the session that ran, so the next
		// attempt's belongs to the next attempt.
		r.SessionID = s.event.SessionID
		o.publish(r)

	case core.EventProgress:
		// The prior-attempt account's substance (SPEC §9.6, #61). Already redacted:
		// handle.emit covers core.Event.Text, which is what makes retaining it safe
		// rather than a second place credentials can land (SPEC §10.3).
		r.retainOutput(s.event.Text)

	case core.EventUsage:
		if s.event.Usage == nil {
			return
		}
		// Already accounted above, in both units: costUSD is the issue's running
		// total, which is what §9.9's cap is over, and attemptUsage is this
		// attempt's, which is what its outcome record reports (#60). What is left
		// here is the decision the cap drives.
		if cap := maxCost(r); cap > 0 && r.costUSD > cap && !r.stopping && !r.suspended {
			// SPEC §9.9: stop the run, park as needs-review, keep the
			// workspace. Not retryable — the budget will not un-breach.
			//
			// **Not while a drain owns the record**, and the accounting above
			// makes that reachable: a run already in `running` when the signal
			// landed stays in `running`, so a late usage event finds a suspended
			// record here. Every part of this branch is a thing the drain refuses
			// to do. Parking is a *routing decision* — it ends in
			// enterNeedsReview, a terminal projection §9.10 must reach for
			// itself. `StopDiscard` is less patient than the StopInterrupt the
			// drain deliberately chose, so it would truncate the §7.2 record of
			// the very run being wound down. And `stopping` makes orderedExit()
			// true, which takes the record out of the suspension branch of
			// driveShutdown — the **backstop this path depends on**, and one
			// nothing ever clears `stopping` to get back into.
			//
			// It depends on the backstop *alone*, which is why the guard is not
			// merely tidy. The other writer of an attempt outcome under a drain
			// records only for a record whose prior-attempt account read was out
			// (endAttemptSuspended, #61), and this record is still `running` with no
			// such read started — the account is read after the workspace falls
			// quiet. So nothing else will ever file this attempt, and the measured
			// cost of omitting this guard was every such attempt vanishing from the
			// log.
			// The two are not the same statement, and only the first is about
			// this stop. `budgetStop` is what onStopped classifies this stop by;
			// the reason and the outcome are the record's history, which survives
			// into the re-queue that follows (restoreBudgets, SPEC §5.6).
			r.stopping, r.budgetStop = true, true
			r.FailureReason = core.FailureBudgetExceeded
			r.lastOutcome = string(core.FailureBudgetExceeded)
			o.beginStop(ctx, r, core.StopDiscard)
		}

	case core.EventSucceeded:
		if r.stopping {
			return
		}
		// The state moves now — the agent has reported what it did, which is
		// §9.2's trigger — but the work that follows waits for the process to
		// actually be gone. See holdOutcome.
		if err := o.transition(ctx, r, StateVerifying, legalTransitions[transition{StateRunning, StateVerifying}]); err != nil {
			return
		}
		r.lastOutcome = "succeeded"
		o.holdOutcome(r, s.event, "agent run failed")

	case core.EventFailed:
		if r.stopping {
			return
		}
		r.FailureReason = s.event.Reason
		o.holdOutcome(r, s.event, "agent run failed")
	}
}

// holdOutcome records what a run reported. Nothing acts on it until the
// workspace it was produced in has no processes left in it.
//
// Three facts, and only the third is the one every caller needs. The adapter
// closes Events after its terminal event. Done adds the direct-execution and
// transcript phase edge, but says nothing about other members of the
// substrate-owned domain. Probe/Stop's confirmed verdict is the domain-quiet
// evidence the orchestrator must ask for (SPEC §7.5).
//
// It matters because everything a terminal event leads to touches that
// workspace: the §9.7 evidence check reads it, the §6.5 after-run hook runs
// against it, disposal removes it, and the continuation track re-dispatches
// into it about a second later. §9.8 states the invariant for the stop path —
// "a possibly-alive process must never share a workspace with a replacement" —
// and it holds just as much for a run that ended on its own.
func (o *Orchestrator) holdOutcome(r *Record, ev core.Event, detail string) {
	r.outcome = &runOutcome{event: ev, detail: detail}
}

// confirmQuiet asks whether the run's execution domain is quiet, and routes the
// held outcome once it is. Retried on later ticks while the answer is no, which is
// §9.8's posture for an unconfirmed stop applied to a natural end.
//
// *Which* question depends on how far the run has got, and the distinction is the
// whole of #79 (SPEC §7.5):
//
//   - Before Done, direct execution may still be flushing its transcript. Only
//     Probe is permissible: it is one read-only observation.
//   - After Done, remaining members have outlived direct execution, so the
//     teardown-capable Stop is permissible. A naturally empty domain still
//     short-circuits on its initial observation without signalling.
//
// Neither answer authorizes anything by itself: only a confirmed termination
// does, and applyOutcome is where that is enforced.
func (o *Orchestrator) confirmQuiet(ctx context.Context, r *Record) {
	if r.outcome == nil || !r.eventsClosed || r.domainQuiet || r.probeInFlight || r.stopInFlight || r.exiting() {
		return
	}
	if r.handle == nil {
		// No execution domain was started, so there is nothing to confirm. The marker
		// is still cleared: beginStart writes it before the launch, so a Start that
		// failed leaves one behind, and that is the "interrupted cleanup of a launch
		// that failed" reading §9.10 would otherwise park on.
		r.domainQuiet = true
		o.freeWorkspaceMarker(ctx, r)
		o.applyOutcome(ctx, r)
		return
	}
	if !r.handleDone {
		o.beginProbe(ctx, r)
		return
	}

	// StopInterrupt, not StopDiscard, for the grace rather than the mode's other
	// half: discard is deliberately *less patient*, and impatience is the wrong trade for a
	// question whose only answer worth having is a confirmed one. An unconfirmed
	// verdict costs a retained claim and another tick (SPEC §9.8), while waiting
	// costs nothing at all in the ordinary case, where Stop short-circuits on a
	// domain already observed quiet.
	//
	// Discard's own purpose — abandoning a reader nobody is coming back for — has
	// none here: `Done` has already closed, and the harness closes it only once
	// the transcript is written and closed as well, so there is no output left to
	// discard and §7.2's verbatim record is not this call's to protect.
	o.beginStop(ctx, r, core.StopInterrupt)
}

// applyOutcome routes a held outcome, once the workspace is quiet.
//
// The condition is a re-check, not the gate. What actually keeps a retry out of a
// possibly-live worktree is that both callers establish `domainQuiet` first —
// confirmQuiet only for a record that never started a process, onStopped only on
// a `Stop` that answered confirmed — so no reachable path can arrive here with it
// false, and no test can therefore catch its removal. It is written anyway
// because the invariant is worth more than the line: a third caller is where this
// would break, and it would break silently (SPEC §9.8).
func (o *Orchestrator) applyOutcome(ctx context.Context, r *Record) {
	if r.outcome == nil || !r.eventsClosed || !r.domainQuiet {
		return
	}
	if !r.summarized {
		// The prior-attempt account is read here, between the workspace falling
		// quiet and anything routing on the outcome (SPEC §9.6, #61). Both halves
		// of that position are load-bearing:
		//
		//   - After quiescence, so the commits being read are all of them and no
		//     process is still making more.
		//   - Before routing, so the read cannot race the retry it exists to
		//     inform. `enterBackoff` arms a timer measured in seconds and a local
		//     `git log` takes milliseconds, but "usually wins" is not a
		//     linearization; holding the outcome one more hop is.
		//
		// The held outcome is what makes this cheap: it stays on the record, and
		// this function is re-entered when the read reports (onSummarized).
		o.beginSummary(ctx, r)
		return
	}
	out := *r.outcome
	r.outcome = nil

	if out.event.Type == core.EventSucceeded {
		o.beginVerify(ctx, r)
		return
	}
	o.failAttempt(ctx, r, out.event.Reason, out.event.Reason.Retryable(), out.detail)
}

// onEventsClosed fires when the run's event stream closes. That is the adapter
// saying it has nothing further to report — not that direct execution reached
// Done, and not that its execution domain is quiet. Raw stdout EOF is a third
// fact again, downstream of this: the pump closes Events and keeps draining.
//
// The sawTerminal branch is a fail-safe for a violated RunHandle contract, not
// the crash path. SPEC §7.4 makes the terminal event ground truth and adapters
// translate at the boundary, so a process that exits saying nothing is
// synthesized into `failed(crashed)` by the adapter (claudecode exitEvent),
// arrives as an event, and is held by onRunEvent like any other. A stream that
// closed with *no* terminal event at all means an adapter broke its contract,
// which no conforming one does — so it is held rather than routed, on the same
// terms as everything else this workspace has to wait for.
func (o *Orchestrator) onEventsClosed(ctx context.Context, r *Record, s signal) {
	// The stream is over; direct execution and its domain may not be. Nothing may
	// touch this attempt's workspace on the strength of this fact alone — the
	// probe below is what settles that (confirmQuiet, applyOutcome).
	r.eventsClosed = true
	if r.suspended {
		// The stream is over and the drain is what decides what follows: nothing.
		// The handle stays so Done can still close and the transcript finish.
		return
	}
	if r.stopping {
		// The handle stays. claude-code closes its event stream even when
		// termination is unconfirmed, so dropping the handle here would leave
		// the next-tick retry (SPEC §9.8) with nothing to call Stop on.
		return
	}
	// The handle is deliberately kept until the domain is confirmed quiet: it is
	// the only route to a member that outlived direct execution, and an exit
	// arriving in this window has to be able to.
	//
	// An exit already under way is not a crash either.
	if r.releasing || r.finish != nil {
		return
	}
	if !s.sawTerminal {
		if r.State != StateRunning {
			return
		}
		o.holdOutcome(r, core.Event{Type: core.EventFailed, Reason: core.FailureCrashed},
			"the agent exited without a terminal event")
	}
	o.confirmQuiet(ctx, r)
}

// failAttempt routes a failed attempt onto the failure track (SPEC §9.6):
// retryable with attempts remaining goes to backoff, everything else fails.
func (o *Orchestrator) failAttempt(ctx context.Context, r *Record, reason core.FailureReason, retryable bool, detail string) {
	o.attemptEnded(ctx, r)
	// The reason is passed rather than left to be read off the field assigned on
	// the next line: that field is sticky across retries, and this one is about
	// this attempt (see recordAttempt).
	o.recordAttempt(r, reason, VerdictUnknown)
	r.FailureReason = reason
	r.lastOutcome = string(reason)
	r.recordAccount(r.lastOutcome)
	if retryable && o.attemptsRemain(r) {
		o.enterBackoff(ctx, r, detail, reason)
		return
	}
	o.enterFailed(ctx, r, detail, reason)
}

// enterBackoff takes the §7.3 cause rather than reading r.FailureReason because
// the two are not the same question on this edge: the field is sticky across
// retries and the human unpark reaches here with no failure at all (see
// transitionCaused).
func (o *Orchestrator) enterBackoff(ctx context.Context, r *Record, why string, cause core.FailureReason) {
	if err := o.transitionCaused(ctx, r, StateBackoff, why, cause); err != nil {
		return
	}
	// A new attempt is coming; whatever we asked the last one to do is over.
	r.stopping = false
	// The failure track does not resume. §9.6 gives the continuation token to
	// the continuation track alone — "re-dispatch **with the continuation
	// token**" is written of the clean exit and of nothing else — and the two
	// tracks are asking for different things. A continuation resumes a session
	// that ended tidily with work left to publish; a retry follows a session
	// that crashed, stalled or ran out of time, and handing that one back to
	// `--resume` re-enters the state that just failed. The retry's context
	// arrives through the prompt instead, where §9.6 puts it: `attempt` and
	// `run.previous_outcome`, the latter carrying the §7.3 reason.
	//
	// This also covers the human unpark, which lands here: a re-queue restores
	// the run budgets (§9.2), and resuming the session those budgets were
	// spent on would make the gesture half a reset.
	r.Continuation = ""
	// The session belongs to the attempt that announced it, and generation
	// turnover *is* the new attempt: runID changes on the next line, so anything
	// published from here on would otherwise pair a new run id with the previous
	// attempt's session. armTimer publishes immediately below (SPEC §10.3).
	r.SessionID = ""
	r.generation++
	delay := backoffDelay(r.Issue.Identifier, r.Attempt, time.Duration(o.limits().MaxRetryBackoffMS)*time.Millisecond)
	o.armTimer(ctx, r, timerBackoff, delay)
}

func (o *Orchestrator) enterFailed(ctx context.Context, r *Record, why string, cause core.FailureReason) {
	// The one edge §9.10 step 6 reads back: this is where a `failed` comment
	// reconstructed after a restart gets its reason from, so the taxonomy has to
	// be on the entry and not only in r.FailureReason, which does not outlive
	// the process.
	if err := o.transitionCaused(ctx, r, StateFailed, why, cause); err != nil {
		return
	}
	// The label blocks re-dispatch; the claim is released so a human can take
	// it (SPEC §9.2). The local workspace stays for forensics, while a remote
	// allocation applies its explicit on_failure policy below.
	o.comment(ctx, r, core.MilestoneComment{
		Milestone: core.MilestoneFailed,
		Reason:    r.FailureReason,
		Detail:    why,
	})
	o.completeFailure(ctx, r)
	o.release(ctx, r, "failed: "+why)
}

// enterNeedsReview parks a run. Most routes here are verification verdicts
// rather than failures and carry no §7.3 cause — only §9.9's budget breach
// does, which is why the parameter exists at all.
//
// A park is a *routing* decision — it keeps the claim and the workspace and
// waits for a human — so it is refused for a record whose exit is already
// ordered, on the same terms every other routing site refuses one (applyOutcome,
// confirmQuiet, the parked sweep). The guard is here rather than at any one
// caller because the state it prevents is a property of the pair rather than of
// the route that reached it: a `needs-review` record that is also `gone` or
// `claimLost` is permanently `exiting()`, so reconciliation skips it, the parked
// sweep filters it out, its owed tracker writes are discarded unattempted
// (absence.go) and no exit is ever queued — the record, its §9.5 slot, its
// workspace and every future identity reload are held for the life of the
// process (#236). Nothing is lost by refusing: the caller has decided this
// attempt is over and recorded it, and the exit owns what happens next.
func (o *Orchestrator) enterNeedsReview(ctx context.Context, r *Record, why string, cause core.FailureReason) {
	if r.orderedExit() {
		o.log.Info("not parking a record whose exit is already ordered; the exit owns it",
			"issue", r.Issue.Identifier, "detail", why, "reason", r.stopReason)
		return
	}
	if err := o.transitionCaused(ctx, r, StateNeedsReview, why, cause); err != nil {
		return
	}
	// Claim and workspace are both retained (SPEC §9.2).
	o.comment(ctx, r, core.MilestoneComment{
		Milestone: core.MilestoneNeedsReview,
		Detail:    why,
	})
}

// enterEpochFault is the sticky form of a needs-review park. The assignment is
// retained for diagnosis, but no gesture within that assignment can replace
// the missing epoch/base authority; only unassignment followed by a new claim
// establishes a new tracker-native epoch.
func (o *Orchestrator) enterEpochFault(ctx context.Context, r *Record, why string) {
	r.epochFaulted = true
	r.epochFaultDetail = why
	o.enterNeedsReview(ctx, r, why, "")
}

// armTimer schedules a backoff or continuation wake-up. The generation
// captured here is what makes a superseded timer harmless.
func (o *Orchestrator) armTimer(ctx context.Context, r *Record, kind timerKind, d time.Duration) {
	gen, token := r.generation, r.token
	id := r.Issue.Identifier
	// Advisory, for `ben status` (§10.3). Recorded here rather than computed by
	// the reader because the delay is deterministic-jittered per attempt (§9.6),
	// and re-deriving it outside the loop would be a second implementation of
	// the backoff formula, free to drift from the one that armed the timer.
	r.nextTimerAt, r.nextTimerKind = o.clock.Now().Add(d), kind
	o.publish(r)
	go func() {
		select {
		case <-ctx.Done():
		case <-o.clock.After(d):
			o.send(ctx, signal{kind: sigTimer, issue: id, generation: gen, token: token, timer: kind})
		}
	}()
}

// onTimer handles a fired backoff or continuation timer. Both re-fetch the
// issue by ID before acting: the world may have moved while we waited
// (SPEC §9.6, "backoff firing re-fetches the issue by ID first").
func (o *Orchestrator) onTimer(ctx context.Context, r *Record, s signal) {
	// The wake-up happened, so the status surface stops advertising it. Cleared
	// on arrival rather than after the re-fetch: from here the record is waiting
	// on a tracker read, not on a clock, and reporting a deadline that has
	// already passed is how a status surface teaches people to distrust it.
	r.nextTimerAt = time.Time{}
	o.publish(r)

	gen, id, kind, token := r.generation, r.Issue.Identifier, s.timer, r.token
	expectedEpoch := r.claimEpoch
	// The re-fetch is a dispatch decision like any other, and it launches an
	// attempt under whatever configuration it comes back to — so it is scoped the
	// same way the candidate reads are.
	rev := o.configNow().Revision
	issue := r.Issue
	go func() {
		snap := o.revalidate(ctx)
		out := signal{
			kind: sigTimerFetched, issue: id, generation: gen, token: token, timer: kind,
			revision: rev, requiredLabels: snap.Definition.Config.Tracker.RequiredLabels,
			claimPrincipal: snap.Runtime.ClaimPrincipal,
		}
		out.refetched, out.err = snap.Runtime.Tracker.Get(ctx, id)
		if out.err != nil {
			o.send(ctx, out)
			return
		}
		// A re-dispatch is a dispatch decision, so it owes §9.5's check like the
		// first one did: the pin proves the *bytes* were approved, and this proves
		// the approval still stands over the issue as it now reads. Read here
		// rather than at the verdict so the whole decision is made from one
		// snapshot of the world (SPEC §9.6's "re-fetches the issue by ID first").
		out.history, out.err = snap.Runtime.Tracker.ClaimHistory(ctx, issue)
		if out.err != nil {
			o.send(ctx, out)
			return
		}
		out.approval, out.err = readApproval(ctx, snap.Runtime.Tracker, issue)
		if out.err == nil && expectedEpoch > 0 {
			out.claimBase, out.claimBaseErr = snap.Runtime.Workspaces.ClaimBase(ctx, issue)
			if out.claimBaseErr == nil && out.claimBase.State == core.ClaimBasePending {
				out.runMarker, out.markerErr = snap.Runtime.Workspaces.ReadRunMarkerFor(issue)
			}
		}
		o.send(ctx, out)
	}()
}

// onTimerFetched applies the §9.6 re-fetch rules: absent → release; terminal
// → dispose and release; active and routable → dispatch, or requeue if no
// slot; otherwise release.
func (o *Orchestrator) onTimerFetched(ctx context.Context, r *Record, s signal) {
	if r.exiting() {
		// A release or stop is already under way. A timer armed by the
		// attempt that is being wound up must not start another one.
		return
	}
	// One read of the source; every question below answers from it.
	cur := o.configNow()
	if cur.Revision != s.revision {
		// A reload landed while this re-fetch was out, so what it saw predates
		// the configuration in force. Acting on it would decide this issue's fate
		// — terminal, unroutable, re-dispatched — against a configuration a human
		// has replaced, and §5.4 gives retry scheduling and launches to the reload.
		//
		// Rearmed rather than dropped: the wait is not the record's fault and
		// is not consumed, so the question is simply asked again under the
		// definition now in force. Dropping would strand the record in backoff
		// with no timer left to fire.
		// The next wait is computed from the definition now in force, which is
		// the one that superseded this fetch: §5.4 hands retry scheduling to the
		// reload, and rearm resolves the ceiling through it. That holds because
		// there is one configuration and no staging step — an operator cutting
		// the ceiling from 5m to 1s sees it on this very re-arm.
		o.log.Info("re-dispatch deferred: a reload overtook the re-fetch", "issue", r.Issue.Identifier)
		o.rearm(ctx, r, s.timer)
		return
	}
	if gone(refreshResult{issue: s.refetched, err: s.err}) {
		// Absent is a fact the adapter states with a named error; retrying
		// would retry forever (SPEC §9.6, §9.8).
		r.markGone()
		o.finishNow(ctx, r, false, "issue is gone from the tracker")
		return
	}
	if s.err != nil {
		// Could not ask. Keep the record and try again after another wait
		// rather than guessing in either direction.
		if !o.logCredentialFailure("re-fetching before re-dispatch", r.Issue.Identifier, s.err) {
			o.log.Warn("re-fetching before re-dispatch", "issue", r.Issue.Identifier, "error", s.err)
		}
		o.rearm(ctx, r, s.timer)
		return
	}
	switch {
	case !o.active(cur.Definition, *s.refetched):
		o.finishNow(ctx, r, false, "issue went terminal while waiting to re-dispatch")
		return
	case !o.routable(cur, *s.refetched):
		o.finishNow(ctx, r, true, "issue is no longer routable")
		return
	}

	// SPEC §9.5, before the routing view is adopted and before an attempt is
	// spent. An issue whose content moved after the approving instant is *not
	// dispatched*: it parks, keeping the claim and the workspace, and waits for a
	// labeler to re-apply a required label over the content they have now read.
	//
	// Ahead of the capacity and revalidation gates below on purpose. Those decide
	// whether this is a good moment to dispatch; this decides whether the issue
	// may be dispatched at all, and rearming instead would leave a record cycling
	// on a timer with a verdict already reached.
	approved, err := checkApproval(s.history, s.requiredLabels, s.approval)
	if err != nil {
		o.enterNeedsReview(ctx, r, "content approval: "+err.Error(), "")
		return
	}
	r.adoptRouting(*s.refetched)
	if approved.at.After(r.approvedAt) {
		// A labeler re-applied a required label, which moves the approving
		// instant and is the only act that reapproves (§6.7). The pin is taken
		// afresh against it — admissible for the same reason the first one was,
		// since the check that let us here established nothing has edited the
		// content since.
		//
		// Driven by the instant having moved, never by the gesture that got us
		// here: the §9.2 human re-queue resumes a parked run and approves
		// nothing, so a re-queue over drift nobody reapproved arrives at the
		// refusal above and parks again.
		//
		// The guard is a statement, not a behaviour, and the mutation ledger for
		// #49 says so: replacing it with an unconditional re-pin survives every
		// test, because after a passing check the content read *is* the pinned
		// content — nothing edited it since the instant it was pinned against,
		// and one response carried both facts. It stays for what it would cost to
		// lose: the day those two facts come from two reads, an unconditional
		// re-pin silently becomes "trust the latest read", which is the defect
		// this ticket closes.
		r.pin(approved)
	}

	if err := cur.Blocked; err != nil {
		// SPEC §5.4 blocks *new dispatches* while a reload is invalid, and
		// §9.4 asks for defensive revalidation before each dispatch cycle —
		// so the timer path revalidates for itself rather than trusting the
		// last poll's verdict, which can be a whole interval stale.
		o.log.Warn("re-dispatch held: dispatch is blocked by config validation",
			"issue", r.Issue.Identifier, "error", err)
		o.rearm(ctx, r, s.timer)
		return
	}
	if o.freeSlots(cur.Definition) <= 0 {
		// backoff → backoff: requeue with a fresh timer (SPEC §9.2). The
		// continuation track has no such edge, so it waits on its own timer
		// rather than changing state.
		if r.State == StateBackoff {
			if err := o.transition(ctx, r, StateBackoff, legalTransitions[transition{StateBackoff, StateBackoff}]); err != nil {
				return
			}
		}
		o.rearm(ctx, r, s.timer)
		return
	}

	anchor := claimCycleAnchor(s.history, s.claimPrincipal)
	if anchor <= 0 {
		o.enterEpochFault(ctx, r, "claim epoch: re-dispatch has no positive current assignment event")
		return
	}
	if r.claimEpoch == 0 {
		// This is the pre-first-attempt content-reapproval path: approval failed
		// before an epoch could be initialized, the human reapproved, and this
		// dispatch decision now has the ordered event needed to begin it. Stay in
		// backoff until the pending write is durable; its completion resumes the
		// prepare that this timer would otherwise start below.
		r.claimEpoch = anchor
		r.claimBaseDispatch = true
		r.Definition = cur.Definition
		o.beginClaimBase(ctx, r)
		return
	}
	if anchor != r.claimEpoch {
		o.enterEpochFault(ctx, r, fmt.Sprintf(
			"claim epoch: re-dispatch expected assignment %d, history names %d", r.claimEpoch, anchor))
		return
	}
	if s.claimBaseErr != nil {
		o.enterEpochFault(ctx, r, "claim epoch: provider state is unreadable before re-dispatch")
		return
	}
	switch s.claimBase.State {
	case core.ClaimBasePinned:
		if !claimBasePinsEpoch(s.claimBase, r.claimEpoch) ||
			(r.hasWorkspace() && !claimBaseAuthorizesWorkspace(s.claimBase, r.Workspace)) {
			o.enterEpochFault(ctx, r, fmt.Sprintf(
				"claim epoch: re-dispatch found pinned state %+v, expected epoch %d and the retained workspace pair",
				s.claimBase, r.claimEpoch))
			return
		}
	case core.ClaimBasePending:
		gate, resolved := classifyRecoveryClaimBase(
			anchor, s.claimBase, nil, s.runMarker, s.markerErr,
			currentCycleRunningEvidence(s.history, anchor),
		)
		switch {
		case !resolved:
			o.enterEpochFault(ctx, r, "claim epoch: pending state unexpectedly bypassed the re-dispatch gate")
			return
		case gate.action == recoveryActionBlocked:
			o.log.Warn("reading the run marker before re-dispatch; retaining the pending claim and retrying",
				"issue", r.Issue.Identifier, "marker_state", s.runMarker.State, "error", s.markerErr)
			o.rearm(ctx, r, s.timer)
			return
		case gate.action != recoveryActionApprove || gate.epochFault:
			o.enterEpochFault(ctx, r, "claim epoch: "+gate.detail)
			return
		case r.Workspace.ClaimEpoch != 0 || r.Workspace.BaseSHA != "" || r.Workspace.TargetBranch != "":
			// A path alone is the concrete provider's ordinary pre-pin error
			// return. Any part of the authority tuple conflicts with pending state.
			o.enterEpochFault(ctx, r, "claim epoch: pending state conflicts with a retained workspace epoch/base/target tuple")
			return
		}
	default:
		o.enterEpochFault(ctx, r, fmt.Sprintf(
			"claim epoch: re-dispatch found non-authorizing provider state %+v", s.claimBase))
		return
	}

	reason := legalTransitions[transition{StateBackoff, StatePreparing}]
	if r.State == StateVerifying {
		reason = legalTransitions[transition{StateVerifying, StatePreparing}]
		r.Turns++
	}
	r.Attempt++
	// The definition this re-dispatch was decided under. beginStart replaces it
	// at the launch itself, so a reload arriving during the retry's Prepare
	// still reaches the run — same rule as a first dispatch (SPEC §5.4).
	r.Definition = cur.Definition
	if err := o.transition(ctx, r, StatePreparing, reason); err != nil {
		return
	}
	o.beginPrepare(ctx, r)
}

// rearm waits again without consuming an attempt.
func (o *Orchestrator) rearm(ctx context.Context, r *Record, kind timerKind) {
	d := continuationDelay
	if kind == timerBackoff {
		d = backoffDelay(r.Issue.Identifier, r.Attempt, time.Duration(o.limits().MaxRetryBackoffMS)*time.Millisecond)
	}
	o.armTimer(ctx, r, kind, d)
}

func (o *Orchestrator) beginVerify(ctx context.Context, r *Record) {
	gen, token := r.generation, r.token
	issue, ws := r.Issue, r.Workspace
	// One checker, captured at entry: it reads the worktree through a workspace
	// provider and the pull request through a tracker, and a pair drawn from two
	// bundles would read one repository's worktree and another's pull requests.
	bundle := o.bundle()
	verifier := bundle.Verifier
	workspaces := bundle.Workspaces
	tracker := bundle.Tracker
	principal := bundle.ClaimPrincipal
	r.pending++
	go func() {
		history, err := tracker.ClaimHistory(ctx, issue)
		if err != nil {
			o.send(ctx, signal{
				kind: sigVerified, issue: issue.Identifier, generation: gen, token: token,
				err:                  fmt.Errorf("reading the current claim epoch before verification: %w", err),
				claimEpochReadFailed: true,
			})
			return
		}
		anchor := claimCycleAnchor(history, principal)
		if anchor <= 0 || anchor != ws.ClaimEpoch {
			o.send(ctx, signal{
				kind: sigVerified, issue: issue.Identifier, generation: gen, token: token,
				epochFault: true,
				epochDetail: fmt.Sprintf("claim epoch: verification expected assignment %d, current history names %d",
					ws.ClaimEpoch, anchor),
			})
			return
		}
		claimBase, claimBaseErr := workspaces.ClaimBase(ctx, issue)
		if claimBaseErr != nil || !claimBaseAuthorizesWorkspace(claimBase, ws) {
			o.send(ctx, signal{
				kind: sigVerified, issue: issue.Identifier, generation: gen, token: token,
				epochFault: true,
				epochDetail: fmt.Sprintf("claim epoch: verification expected %d/%s/%s, provider returned %+v: %v",
					ws.ClaimEpoch, ws.BaseSHA, ws.TargetBranch, claimBase, claimBaseErr),
			})
			return
		}
		res, err := verifier.Verify(ctx, issue, ws)
		o.send(ctx, signal{kind: sigVerified, issue: issue.Identifier, generation: gen, token: token, verify: res, err: err})
	}()
}

func (o *Orchestrator) onVerified(ctx context.Context, r *Record, s signal) {
	r.pending--
	if o.finishIfRequested(ctx, r) {
		return
	}
	if r.suspended {
		// A verdict is a route — `done`, `needs-review`, or a continuation that
		// dispatches again — and routing is the one thing a drain must not do.
		// The record stays in `verifying`, its `ben:claimed` label stands, and
		// §9.10 reads that at the next start. Nothing is lost: the evidence the
		// verdict was read from is git and the tracker, and both outlive us.
		return
	}
	if s.epochFault {
		// The verifier was never called: the claim-scoped pair is the
		// prerequisite to asking §9.7, not another evidence leg inside it.
		o.attemptEnded(ctx, r)
		o.recordAttempt(r, "", VerdictUnknown)
		o.enterEpochFault(ctx, r, s.epochDetail)
		return
	}
	if errors.Is(s.err, core.ErrIssueNotFound) {
		// The mandatory current-epoch read names one issue, so not-found is the
		// positive terminal fact rather than a verification failure. There is no
		// claim left to release and no issue on which a needs-review projection
		// could land.
		r.markGone()
		o.finishNow(ctx, r, false, "issue disappeared before verification")
		return
	}
	if s.claimEpochReadFailed {
		// ClaimHistory is a tracker read. Every non-not-found failure retries on
		// the next poll tick regardless of credential class; it establishes no
		// epoch and the publish-evidence checker remains uncalled (§8.5, §9.7).
		if !o.logCredentialFailure("reading the current claim epoch before verification; retrying next tick",
			r.Issue.Identifier, s.err) {
			o.log.Warn("reading the current claim epoch before verification; retrying next tick",
				"issue", r.Issue.Identifier, "error", s.err)
		}
		r.verifyRetry = true
		return
	}
	if errors.Is(s.err, core.ErrPublishApprovalPending) {
		// Protected-file review is an unfinished trusted publication, not an
		// agent failure and not missing evidence. Keep the claim and attempt
		// intact, then ask the publisher again on a later poll. Its durable
		// operation key remains fixed while each retry receives a fresh backend
		// run identity, so an approval can never revive the terminal run that
		// first returned pending.
		o.log.Info("trusted publication is awaiting approval; retrying next tick",
			"issue", r.Issue.Identifier)
		r.verifyRetry = true
		return
	}
	if credentialTransient(s.err) {
		// Ahead of everything below, because everything below is final: the
		// attempt is ended, the account composed, the outcome recorded, and a
		// verdict routed. None of that may happen while the verdict is still
		// pending (SPEC §9.7, amendment 13).
		//
		// The record stays in `verifying` and retries on the next poll tick.
		// §9.7's fail-closed park covers evidence that contradicts or cannot be
		// established; a credential that could not be obtained establishes
		// neither, and the evidence itself is untouched on git and the tracker.
		o.logCredentialFailure("credential failure reading publish evidence; retrying next tick", r.Issue.Identifier, s.err)
		r.verifyRetry = true
		return
	}
	// The run is over however this verdict routes — including the
	// continuation track, which prepares again and so owes its own hook call.
	o.attemptEnded(ctx, r)
	// The account of the attempt that just ended, for whichever prompt comes next
	// (SPEC §5.6, §9.6). Composed for every verdict rather than per branch: a
	// `contradicted` park is re-queued by a human onto the failure track, and
	// `done` converts to a held claim that never renders another prompt, so the
	// only route with an argument against carrying one is the continuation track —
	// which drops it below, where the branch is taken.
	r.recordAccount(r.lastOutcome)
	if s.err != nil {
		// Fail closed: a verification that could not be completed is never
		// success (SPEC §9.7). No §7.3 reason and no verdict: the agent did not
		// fail, and nothing was concluded about what it produced.
		//
		// A *transient* credential failure never reaches here — it was routed
		// back into `verifying` above — so this covers the unknown and permanent
		// classes as well as every non-credential verification error, and parks
		// all of them exactly as §9.7 already did.
		o.logCredentialFailure("credential failure reading publish evidence; parking", r.Issue.Identifier, s.err)
		o.recordAttempt(r, "", VerdictUnknown)
		o.enterNeedsReview(ctx, r, "verification failed: "+s.err.Error(), "")
		return
	}
	o.recordAttempt(r, "", s.verify.Verdict)

	switch s.verify.Verdict {
	case VerdictPublished:
		if err := o.transition(ctx, r, StateDone, legalTransitions[transition{StateVerifying, StateDone}]); err != nil {
			return
		}
		r.PRURL = s.verify.PRURL
		o.comment(ctx, r, core.MilestoneComment{
			Milestone: core.MilestonePublished,
			PRURL:     s.verify.PRURL,
		})
		// Claim retained so the issue is not re-dispatched while the PR
		// awaits review (SPEC §9.2); workspace disposed. The record converts
		// into a held-claim record once these writes land — not before, since
		// it is what retries them — and the §9.8 sweep releases the claim
		// when the issue goes terminal, rather than leaving it for a restart.
		o.disposePublished(ctx, r)

	case VerdictIncomplete:
		if !o.continuable(r) {
			o.enterNeedsReview(ctx, r, "max_turns exhausted without published evidence", "")
			return
		}
		// Continuation track: re-check shortly, then re-dispatch through
		// preparing with the token (SPEC §9.6).
		// Generation turnover: a new attempt, so the session it will announce is
		// not the one that just ended. See enterBackoff.
		//
		// And no prior-attempt account: §9.6 gives this track the resume token, so
		// the session already holds its own history, and summarizing it into the
		// prompt would duplicate that into the context window the resume exists to
		// save. `run.previous_outcome` still says "succeeded", which is what tells
		// the agent it is continuing rather than recovering.
		r.forgetAccount()
		r.SessionID = ""
		r.generation++
		r.FailureReason = ""
		o.armTimer(ctx, r, timerContinuation, continuationDelay)

	case VerdictContradicted:
		detail := s.verify.Detail
		if detail == "" {
			detail = "publish evidence contradicts the agent's claim"
		}
		o.enterNeedsReview(ctx, r, detail, "")

	default:
		// VerdictUnknown, and anything a later verifier adds without teaching
		// this switch about it. Parking is the only safe route for a verdict
		// nobody stated: `done` clears the projection, posts the publish
		// milestone and disposes the workspace, and the continuation track
		// spends another attempt — while needs-review costs a human's
		// attention and keeps every fact intact (SPEC §9.7, "a verification
		// that could not be completed is never success").
		//
		// Named rather than left to fall through the contradicted arm, which
		// is what it used to do: an unstated verdict is not evidence against
		// the agent, and telling an operator their run "contradicts the
		// evidence" when nothing was checked sends them looking for a defect
		// in the wrong place.
		detail := "verification returned no usable verdict (" + s.verify.Verdict.String() + ")"
		if s.verify.Detail != "" {
			detail += ": " + s.verify.Detail
		}
		o.enterNeedsReview(ctx, r, detail, "")
	}
}

// beginProbe observes the run's execution domain without touching it, and reports
// the answer back through the loop (#79).
//
// Its own in-flight slot, not the teardown's: a probe does not act, so it
// neither needs nor deserves the "one teardown at a time" guard, and holding
// that slot would let an outstanding observation delay the cleanup Done
// enables.
func (o *Orchestrator) beginProbe(ctx context.Context, r *Record) {
	if r.probeInFlight || r.handle == nil {
		return
	}
	r.probeInFlight = true
	gen, token := r.generation, r.token
	id := r.Issue.Identifier
	handle := r.handle
	go func() {
		term := handle.Probe(ctx)
		o.send(ctx, signal{kind: sigProbed, issue: id, generation: gen, token: token, probe: term})
	}()
}

// onProbed applies a read-only observation of the execution domain.
//
// A confirmed one is the same fact a confirmed stop reports, so it routes the
// same way. An unconfirmed one retains everything and says so, which is the
// difference this ticket exists for: the case that used to be silent now logs on
// every tick, like the descendant case always has (SPEC §9.8).
func (o *Orchestrator) onProbed(ctx context.Context, r *Record, s signal) {
	r.probeInFlight = false
	if r.exiting() || r.stopInFlight {
		// An exit was decided while this observation was out. The record is no
		// longer the quiescence question's to route: an exit outranks a held
		// outcome (see onStopped), and the outcome it would route reads a
		// workspace that exit is about to dispose — and clearing the handle would
		// leave its stop nothing to retry. The stop the exit owns is the
		// authority, so this answer is dropped: the handleDone rule below, one
		// decision earlier.
		//
		// `exiting()` is the load-bearing half, and it is load-bearing on its own:
		// an exit whose last stop came back unconfirmed sits between retries with
		// no teardown out at all, and that is the state a confirmed observation
		// would otherwise route (TestOnProbedRefusesToRouteWhileAnExitIsPending).
		// `stopInFlight` is a re-check that no reachable state needs — a stop
		// starts either from an exit, which the first term covers, or from
		// confirmQuiet, which needs handleDone and is therefore refused by the
		// branch below and by confirmQuiet's own guard. Removing it fails nothing
		// (measured, not assumed); it stays for the same reason applyOutcome keeps
		// its own re-check, and this note is here so the next reader does not have
		// to re-derive which half is which.
		return
	}
	if r.handleDone {
		// The phase edge overtook this observation. It cannot stand in for the
		// Stop that is now owed: this answer predates the phase edge and cannot
		// stand in for the fresh, teardown-capable observation. confirmQuiet asks
		// again through Stop.
		o.confirmQuiet(ctx, r)
		return
	}
	if s.probe != core.TerminationConfirmed {
		o.log.Warn("event stream closed but the execution domain is not confirmed quiet; holding the outcome and retrying next tick",
			"issue", r.Issue.Identifier)
		return
	}
	o.quiet(ctx, r)
}

// onHandleDone records the phase edge and immediately asks the question it
// enables.
//
// Immediately, not next tick: the poll interval is 30 s by default, and a run
// whose domain went quiet a moment after its stream closed would otherwise wait
// that out for nothing. This is also the path that permits teardown of a member
// which outlived direct execution, so deferring it defers cleanup too.
func (o *Orchestrator) onHandleDone(ctx context.Context, r *Record) {
	r.handleDone = true
	if r.suspended {
		// The drain's other half of the same fact. confirmQuiet is refused for a
		// suspended record (exiting), so without this the transcript closing
		// would go unnoticed and the drain would wait on it forever.
		o.finishSuspended(ctx, r)
		return
	}
	o.confirmQuiet(ctx, r)
}

// quiet records that the domain is quiet and routes what was waiting on it. One
// place, because both askers end here and the ordering rules below are the same
// either way.
func (o *Orchestrator) quiet(ctx context.Context, r *Record) {
	r.domainQuiet = true
	o.freeWorkspaceMarker(ctx, r)
	if r.cancelRun != nil {
		r.cancelRun()
		r.cancelRun = nil
	}
	r.handle = nil
	o.applyOutcome(ctx, r)
}

// beginStop asks the handle for bounded domain teardown and reports the result.
//
// One teardown at a time. Both askers — an exit stopping a live run, and the
// cleanup a finished one owes its workspace once Done has closed — put the same
// question to the same handle, so a second operation would buy nothing but a
// second answer to race the first with. An exit that overtakes a teardown
// therefore inherits its answer rather than asking again, and an unconfirmed one
// is re-driven next tick like any other (retryPendingExits).
//
// An *observation* is not teardown and does not share this slot (#79): an exit
// that overtakes one starts its stop immediately, and the probe's answer is
// dropped rather than mistaken for the stop's (onProbed).
func (o *Orchestrator) beginStop(ctx context.Context, r *Record, mode core.StopMode) {
	if r.stopInFlight {
		return
	}
	if r.handle == nil {
		// No execution domain was started, so there is nothing that could be live.
		// Stated rather than left to the field's zero value, which is
		// unconfirmed (SPEC §9.8): this is the one place entitled to say a
		// workspace is free without a probe, and it should have to say it.
		o.send(ctx, signal{kind: sigStopped, issue: r.Issue.Identifier, generation: r.generation, token: r.token, termination: core.TerminationConfirmed})
		return
	}
	r.stopInFlight = true
	gen, token := r.generation, r.token
	id := r.Issue.Identifier
	handle := r.handle
	go func() {
		term := handle.Stop(ctx, mode)
		o.send(ctx, signal{kind: sigStopped, issue: id, generation: gen, token: token, termination: term})
	}()
}

// onStopped applies a finished bounded teardown, for whichever asker is waiting on
// it — and, when several are, orders them.
//
// An exit outranks a held outcome. Reconciliation can decide an issue is gone,
// terminal or unroutable while the quiescence question is out, and it takes the
// record over by stopping the very process that question is about
// (stopAndFinish). What the run reported is moot by then: verification, a retry
// and a re-dispatch are all things that happen to a record that is *staying*,
// and the first of them reads a workspace this exit is about to dispose.
func (o *Orchestrator) onStopped(ctx context.Context, r *Record, s signal) {
	r.stopInFlight = false
	if r.suspended {
		// Shutdown's interrupt. It reaches none of the branches below: they
		// release, dispose, project and route, and a drain does none of those
		// (SPEC §9.8, §11 as amended). Unconfirmed is re-asked on the next tick
		// exactly as every other unconfirmed stop is, and the drain does not
		// complete until it comes back confirmed.
		if s.termination != core.TerminationConfirmed {
			o.log.Warn("shutdown: execution domain not confirmed quiet; retaining the claim and retrying next tick",
				"issue", r.Issue.Identifier)
			return
		}
		o.suspendStopped(ctx, r)
		return
	}
	if s.termination != core.TerminationConfirmed {
		// SPEC §9.8: an unconfirmed termination retains the claim. A
		// possibly-alive process must never share a workspace with a
		// replacement, so nothing is released, nothing is disposed, nothing
		// reads the workspace, and teardown is retried next tick.
		if r.exiting() {
			o.log.Warn("stop unconfirmed; retaining the claim and retrying next tick",
				"issue", r.Issue.Identifier)
		} else {
			o.log.Warn("event stream closed but the execution domain is not confirmed quiet; holding the outcome and retrying next tick",
				"issue", r.Issue.Identifier)
		}
		return
	}

	// The domain is quiet: the workspace has no attempt process left in it. That is the
	// one fact every asker wanted, whichever of them asked — a stop that ended a
	// live run, the cleanup a finished one owed after Done, or the observation
	// before it (onProbed).
	if !r.exiting() {
		// Nothing has asked this record to leave, so the teardown was the
		// quiescence question and what the run reported is still what happens
		// next.
		o.quiet(ctx, r)
		return
	}
	r.domainQuiet = true
	o.freeWorkspaceMarker(ctx, r)
	if r.cancelRun != nil {
		r.cancelRun()
		r.cancelRun = nil
	}
	r.handle = nil
	r.outcome = nil
	// The stop is over. Leaving this set would make exiting() true for the
	// life of the record, and reconciliation skips exiting records — so a
	// parked issue that a human then closed or re-queued would be ignored
	// permanently (SPEC §9.8).
	//
	// The §9.9 cause and the post-stop disposition go with it, read into the
	// branches below first: both belong to the stop that has just finished, and
	// this is the site that consumes them (Record.budgetStop and
	// Record.finishAfterStop).
	budgetStop := r.budgetStop
	budgetPark := budgetStop && !r.finishAfterStop
	r.stopping, r.budgetStop, r.finishAfterStop = false, false, false

	if budgetPark {
		// Before attemptEnded, deliberately. §9.9's park is the one attempt end that
		// reads the branch *after* this point rather than before it, and the §6.5
		// after-run hook runs against the same worktree under the same issue lock
		// (workspace AfterRun) — so queueing the hook first would leave the two
		// racing, and whichever won would decide whether the next attempt is told
		// about the agent's commits, the hook's, or neither (#61 re-review,
		// finding 4). parkOnBudget calls attemptEnded once the account is in hand,
		// which is the order every other route already has.
		o.parkOnBudget(ctx, r)
		return
	}
	o.attemptEnded(ctx, r)
	// Everything else that reaches here is an ordered exit ending a live run.
	// When that exit initiated the stop, §7.3's `killed` is the cause; when it
	// overtook a budget stop already in flight, the cap remains the cause and the
	// exit changes only the record's disposition.
	reason := core.FailureKilled
	if budgetStop {
		// The exit changes where the record goes, not why this process was
		// stopped. Keeping the cause makes the attempt log agree with the stop
		// the runner actually received.
		reason = core.FailureBudgetExceeded
	}
	o.recordAttempt(r, reason, VerdictUnknown)
	if r.pending > 0 {
		// A verifier may still be reading the workspace. Disposing under it
		// would race the evidence check (SPEC §9.7); defer to the signal that
		// reports the work back.
		r.finish = &finishRequest{keepWorkspace: r.keepOnStop, why: r.stopReason}
		return
	}
	o.finishNow(ctx, r, r.keepOnStop, r.stopReason)
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if equalFold(h, needle) {
			return true
		}
	}
	return false
}

func equalFold(a, b string) bool { return strings.EqualFold(a, b) }
