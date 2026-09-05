package orchestrator

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

type sigKind int

const (
	sigTick sigKind = iota
	sigReconciled
	sigTickResult
	sigClaimed
	sigApproved
	sigClaimBaseBegun
	sigPrepared
	sigStarted
	sigRunEvent
	sigEventsClosed
	sigHandleDone
	sigProbed
	sigSummarized
	sigVerified
	sigStopped
	sigTimer
	sigTimerFetched
	sigEffectDone
	sigOwedConfirmed
	sigClaimAnchor
	sigDoneOwnership
	sigHeldConfirmed
	sigHeldHistory
	sigCycleDisposed
	sigHeldReleased
	sigParkedConfirmed
	sigAdopt
	sigShutdown
	sigRecovered
	sigRecoveryScan
	sigSwept
	sigSweepReserve
	sigSweepRelease
	sigMarkerCleared
	sigCycleScan

	// numSigKinds bounds the kinds above; it is not one of them. Its only use is
	// sizing Orchestrator.applied, so a kind added without a line here is a
	// compile error rather than an out-of-range write.
	numSigKinds
)

// signal is the only way anything reaches the authority loop. Workers do the
// I/O and post outcomes; the loop does the deciding (SPEC §9.1).
type signal struct {
	kind  sigKind
	issue string
	// token identifies the owner this result was started for — the run
	// record's or the held claim's, both drawn from one sequence (see
	// newToken). Every result bound to one carries it.
	token int
	// generation additionally scopes a result to one *attempt* of a record: a
	// worker or timer from a retry or continuation that has been overtaken
	// must not act. See deliverable.
	generation int

	err error

	// sigReconciled
	sweep     sweepResult
	refreshed map[string]refreshResult
	// parked names the records the sweep read was made *for*, each with what the
	// moment it was issued says about the labels it can be trusted for. A record
	// that parked while the read was out is deliberately absent: the response
	// predates its park entirely (SPEC §9.8).
	parked []parkedWant
	// sigTickResult. revision scopes the whole result to the configuration in
	// force when its reads began: a reload adopted since then supersedes it.
	candidates []core.Issue
	revision   uint64

	// sigAdopt carries one identity-changing publication awaiting the loop's
	// verdict. It belongs to no issue and no record.
	adopt *adoption
	// sigShutdown carries the channel closed once the drain completes. Like
	// adopt, it belongs to no issue: it is a question about the whole record set.
	drained chan struct{}

	// sigClaimed
	verified bool
	// sigApproved, and the same three on sigTimerFetched: SPEC §9.5's facts and
	// the label set the approving instant is computed over, all captured from
	// one configuration snapshot so a reload cannot date an approval against a
	// required set nobody dispatched under (SPEC §5.4).
	history        []core.ClaimEvent
	approval       core.ContentApproval
	requiredLabels []string
	claimPrincipal string
	// Claim-base observations accompany the operations whose result depends on
	// them: a failed begin/prepare, a timer re-dispatch, and the verifier gate.
	// They are kept distinct from err because an unreadable or contradictory
	// epoch is a sticky safety verdict, not an ordinary retryable I/O failure.
	claimBase            core.ClaimBase
	claimBaseErr         error
	runMarker            core.RunMarker
	markerErr            error
	epochFault           bool
	epochDetail          string
	claimEpochReadFailed bool
	// sigPrepared
	workspace core.Workspace
	facts     core.LocalBranchFacts
	// sigStarted
	handle core.RunHandle
	// sigRunEvent
	event core.Event
	// sigEventsClosed
	sawTerminal bool
	// sigProbed
	probe core.Termination
	// sigSummarized
	attemptFacts core.AttemptFacts
	// sigVerified
	verify VerifyResult
	// sigStopped
	termination core.Termination
	// sigTimer / sigTimerFetched
	timer     timerKind
	refetched *core.Issue
	// sigEffectDone
	effect string
	// sigClaimAnchor, and the re-derived cycle on sigHeldHistory
	anchor int64
	// cycleSuperseded says the provider's retained workspace cycle is not the one
	// the standing approval selects — a required label withdrawn and re-applied
	// since the claim was retained (cycleApprovalSource, SPEC §6.7).
	//
	// cycleApprovalErr is that read failing, carried apart from `err` because they
	// are different questions with different consequences: the history read failing
	// retries on the next revision bump, while this one must not let the caller
	// re-baseline the revision it rode in on — that would spend the trigger that
	// buys the next read.
	cycleSuperseded  bool
	cycleApprovalErr error
	// issueRevision is the §8.3 change token the two answers above were computed
	// against, so a held record is baselined to what was actually asked about
	// rather than to whatever the record holds when the answer lands.
	issueRevision string
	// sigRecovered carries a retried §9.10 classification and the facts it was
	// reached from. Both are pointers because a zero recoveryVerdict is `unknown`,
	// which is not a verdict — nil says the reads failed and there is nothing to
	// apply. The facts travel with it because the verdict alone cannot name the
	// pull request its milestone comment needs.
	recovery      *recoveryVerdict
	recoveryReads *recoveryFacts
	// sigSwept carries one §9.10 step 5 pass's outcome.
	sweepResult sweepResultSet
	// sigSweepReserve carries one workspace's reservation request, awaiting the
	// loop's verdict. sigSweepRelease drops the reservation and needs only `issue`.
	sweepClaim *sweepClaim
	// sigMarkerCleared carries the removal a worker performed, identified by the
	// pending clear itself: there may be one per store for the same issue, and the
	// identifier alone cannot say which of them answered.
	clear *pendingClear
	// sigParkedConfirmed. Scopes the read the way `revision` scopes a dispatch
	// read: it records that the record owed no `ben:*` projection when the Get went
	// out, which is what makes the label set it comes back with a statement about
	// this park rather than about a write still queued behind it. Asked at issue
	// time because that write can land while the read is in flight, leaving the
	// record owing nothing and the answer still predating it.
	labelsSettled bool
	// sigCycleScan, and sigClaimBaseBegun's immediate read after the provider may
	// have created an obligation.
	cycleRead       bool
	cycleRefs       []core.WorkspaceRef
	cycleWorkspaces Workspaces
	cycleScanErr    error
	cycleScanSeq    uint64
	cycleScanOK     bool
}

// sweepClaim is one workspace's reservation request, answered on the authority
// goroutine. See Orchestrator.reserveDisposal.
type sweepClaim struct {
	identifier string
	// granted is buffered so the loop never blocks on a worker that has stopped
	// listening — the same rule adoption.ack follows, for the same reason.
	granted chan bool
}

type timerKind int

const (
	timerBackoff timerKind = iota
	timerContinuation
)

// String names the timer for `ben status` (SPEC §10.3). The two are not
// interchangeable to a reader: one is a retry waiting out a failure, the other
// is a session about to be resumed with work still to publish (§9.6).
func (k timerKind) String() string {
	switch k {
	case timerBackoff:
		return "backoff"
	case timerContinuation:
		return "continuation"
	default:
		return "unknown"
	}
}

// sweepResult is one HeldClaims read (SPEC §9.8). read says the worker
// actually made it: a record converted while the tick was out has not been
// swept yet, and a zero value would otherwise read as "absent from the list"
// and buy a confirming Get it has done nothing to earn.
type sweepResult struct {
	read   bool
	issues []core.Issue
	err    error
}

// byID indexes the response for the two sweeps that read it. Presence is a fact
// both of them turn on, and it means the same thing to each: the principal holds
// this issue, and these are its state and labels as the tracker states them.
func (res sweepResult) byID() map[string]core.Issue {
	seen := make(map[string]core.Issue, len(res.issues))
	for _, iss := range res.issues {
		seen[iss.Identifier] = iss
	}
	return seen
}

// refreshResult is one tracked issue as the tracker now reports it.
type refreshResult struct {
	issue *core.Issue
	err   error
}

func (o *Orchestrator) handle(ctx context.Context, s signal) {
	// Re-driven after every signal, not on a ticker of its own: a drain
	// completes when the last worker reports back, and the report *is* a signal.
	// A no-op until shutdown has begun.
	defer o.driveShutdown(ctx)
	if s.kind == sigClaimBaseBegun && s.cycleRead {
		// Counted when the remote provider's mutation was started. Do this before
		// record lookup: the owning record may have been forgotten while the I/O
		// was out, but the global scan barrier still has to be released and its
		// durable obligations still have to be adopted.
		if o.cycleMutationsInFlight <= 0 {
			o.cycleScanFailed = true
			o.log.Error("ended-cycle mutation accounting underflow; blocking dispatch")
		} else {
			o.cycleMutationsInFlight--
		}
		fresh := s.cycleScanSeq == o.cycleMutationSeq
		if s.cycleScanErr != nil {
			// A refusal is sticky even when its generation is old.
			s.cycleScanOK = o.applyCycleScan(
				s.cycleWorkspaces, s.cycleRefs, s.cycleScanErr, false,
			)
		} else {
			// A complete local read is enough for this claim-base transition to
			// continue: PrepareClaim re-reads the same directory under the provider's
			// cycle lock before it can prune pins. Only a current snapshot may update
			// the loop's ownership set, though.
			s.cycleScanOK = true
			if fresh {
				// Mutation-local reads may add obligations but never clear a refusal;
				// only the periodic scan starts behind the no-mutation barrier.
				o.applyCycleScan(
					s.cycleWorkspaces, s.cycleRefs, s.cycleScanErr, false,
				)
			} else {
				// A successful old snapshot is ignored wholesale: disposal
				// confirmation or a later begin may have removed an obligation while
				// this directory read was out.
			}
		}
	}

	switch s.kind {
	case sigTick:
		o.onTick(ctx)
	case sigShutdown:
		// Not keyed to an issue: like sigAdopt, it is a question about the whole
		// record set, answered here because here is where that set is owned.
		o.onShutdown(ctx, s)
	case sigReconciled:
		o.onReconciled(ctx, s)
	case sigTickResult:
		o.onTickResult(ctx, s)
	case sigAdopt:
		// Not keyed to an issue: it is a question about the whole record set, and
		// it is answered here because here is where that set is owned.
		o.onAdopt(s.adopt)
	case sigHeldConfirmed:
		// Held-claim results are keyed to a held record, not a run record:
		// after conversion there is no run record to find.
		o.onHeldConfirmed(ctx, s)
	case sigHeldHistory:
		o.onHeldHistory(ctx, s)
	case sigCycleDisposed:
		// Keyed to an issue but to neither owner: an ended workspace cycle outlives
		// the record or held claim that noticed it (cycle.go).
		o.onCycleDisposed(ctx, s)
	case sigHeldReleased:
		o.onHeldReleased(ctx, s)
	case sigSwept:
		// Not keyed to an issue: it is one pass over the workspace directory.
		o.onSwept(s)
	case sigSweepReserve:
		// Keyed to an issue but never to a *record* — the whole question is whether one
		// exists — so it cannot go through the lookup below.
		o.onSweepReserve(s.sweepClaim)
	case sigSweepRelease:
		o.onSweepRelease(s.issue)
	case sigMarkerCleared:
		// Keyed to a pending clear rather than to a record: by the time a marker
		// removal answers, the record that owed it is usually gone — which is the
		// whole reason the retry does not live on one (see freeWorkspaceMarker).
		o.onMarkerCleared(ctx, s)
	case sigRecoveryScan:
		// Not keyed to an issue: it is the §9.10 step 1 candidate read for the whole
		// principal, redone after a startup scan that failed.
		o.onRecoveryScan(ctx, s)
	case sigCycleScan:
		// A provider-local durable-state read, keyed to no run record.
		o.onCycleScan(ctx, s)
		// sigParkedConfirmed is keyed to a run record and falls through to the
		// default, unlike the three above: a parked record is still in the machine.
	default:
		r, ok := o.records[s.issue]
		if !ok {
			// The record was released or forgotten while this was in flight.
			return
		}
		if !deliverable(r, s) {
			return
		}
		o.handleRecord(ctx, r, s)
	}
}

// deliverable reports whether a result that has landed on an issue belongs to
// the record now tracking it. Two questions, and every result answers the
// first:
//
//  1. **Is it this record's at all?** The token identifies one tenure of
//     ownership over the issue (see newToken). Generation cannot answer this:
//     it counts attempts *within* a record and restarts at zero for the next
//     record on the same issue, so a released, reopened and re-dispatched
//     issue produces a fresh record whose generation matches a straggler
//     exactly rather than failing to match it.
//
//     Record.pending keeps most workers from outliving their record — that is
//     what it is for — but it is not a general guarantee. Timers do not hold
//     it, because a backoff or continuation wait is not work anybody is
//     waiting on; neither do the conversion reads, because a `done` record has
//     to stay releasable while they are out. Rather than track which paths are
//     currently covered, every record-bound result carries the token.
//
//  2. **Is it this attempt's?** Generation, for the results that are scoped to
//     one attempt: a timer or worker from a retry or continuation that has
//     since been overtaken must not act.
//
// The exceptions to the second question are the results that belong to the
// record rather than to an attempt — the writes it owes, the read that asks
// whether their issue still exists, and the reads that convert it — for which
// generation would be wrong in the other direction: it bumps on every retry, and
// dropping an owed write's completion would stall the queue behind it forever.
func deliverable(r *Record, s signal) bool {
	if s.token != r.token {
		return false
	}
	switch s.kind {
	// sigParkedConfirmed belongs here for the same reason the conversion reads do:
	// it is the record's question, not an attempt's. A parked record has no attempt
	// in flight, and generation moves the moment the unpark this read might order
	// takes effect — which would discard the very result that ordered it.
	case sigEffectDone, sigOwedConfirmed, sigClaimAnchor, sigDoneOwnership, sigRecovered, sigParkedConfirmed:
		return true
	default:
		return s.generation == r.generation
	}
}

func (o *Orchestrator) handleRecord(ctx context.Context, r *Record, s signal) {
	switch s.kind {
	case sigClaimed:
		o.onClaimed(ctx, r, s)
	case sigRecovered:
		o.onRecovered(ctx, r, s)
	case sigApproved:
		o.onApproved(ctx, r, s)
	case sigClaimBaseBegun:
		o.onClaimBaseBegun(ctx, r, s)
	case sigPrepared:
		o.onPrepared(ctx, r, s)
	case sigStarted:
		o.onStarted(ctx, r, s)
	case sigRunEvent:
		o.onRunEvent(ctx, r, s)
	case sigEventsClosed:
		o.onEventsClosed(ctx, r, s)
	case sigHandleDone:
		o.onHandleDone(ctx, r)
	case sigProbed:
		o.onProbed(ctx, r, s)
	case sigSummarized:
		o.onSummarized(ctx, r, s)
	case sigVerified:
		o.onVerified(ctx, r, s)
	case sigStopped:
		o.onStopped(ctx, r, s)
	case sigTimer:
		o.onTimer(ctx, r, s)
	case sigTimerFetched:
		o.onTimerFetched(ctx, r, s)
	case sigClaimAnchor:
		o.onClaimAnchor(ctx, r, s)
	case sigDoneOwnership:
		o.onDoneOwnership(ctx, r, s)
	case sigParkedConfirmed:
		o.onParkedConfirmed(ctx, r, s)
	case sigEffectDone:
		o.onEffectDone(ctx, r, s)
	case sigOwedConfirmed:
		o.onOwedConfirmed(ctx, r, s)
	}
}

// onTick starts the §9.4 sequence. The reads happen in workers so a slow
// tracker delays the tick's decisions without stalling runner events or
// budget enforcement.
//
// Two workers, not one, and the dependency between them runs one way only.
//
// Reconciliation must not wait on dispatch: §9.4 step 1 is unconditional —
// "**Reconcile** — always runs, even when validation fails" — and that has to
// survive a slow tracker, not just a failing config. Sharing a worker put it
// behind the fetch, and sharing an in-flight flag put every *later* tick's
// behind it too, so one hung `Fetch` meant no closed, deleted or taken-over
// issue was noticed again while its agent kept running.
//
// Dispatch, though, may and must wait on reconciliation, because §9.4 orders
// step 1 before step 2 and the order carries information: reconciliation is
// what frees slots. Started concurrently, the fetch can answer first and
// dispatch then decides against a world reconciliation is about to change —
// seeing the cap full, declining to dispatch, and leaving queued work for the
// next poll. So the dispatch reads are kicked off by onReconciled rather than
// here.
func (o *Orchestrator) onTick(ctx context.Context) {
	o.openRequestWindow()
	o.beginCycleScan(ctx)
	// Before the reads, so a verification waiting on a credential is retried on
	// the same tick it would otherwise have idled through. It rides the poll
	// tick rather than §9.8's attempt backoff because nothing about the attempt
	// is being retried — the run is over and its evidence is on git (SPEC §9.7,
	// amendment 13).
	o.driveVerifyRetry(ctx)
	o.beginReconcile(ctx)
}

// openRequestWindow gives the tracker its per-tick API request budget and logs
// what the tick that just ended spent (SPEC §8.5, §10.3).
//
// Discovered rather than required, like afterRunner: a tracker that meters its
// own API cost gets its window opened by the one component that knows where a
// tick begins, and a tracker with nothing to meter is unaffected.
//
// It is first in the tick deliberately — before the reads it pays for — and the
// report is therefore the *previous* tick's. That is the honest boundary: reads
// this tick issues are still out when the next one opens, so a window is "the
// requests made between two ticks" rather than "the requests one tick made", and
// pretending otherwise would need the loop to wait for its own workers. Nothing
// about visibility waits on this: a refusal already surfaced at the call site as
// an error the caller logged and retried.
func (o *Orchestrator) openRequestWindow() {
	snap := o.configNow()
	budget, ok := snap.Runtime.Tracker.(core.RequestBudget)
	if !ok {
		return
	}
	interval := time.Duration(snap.Definition.Config.Polling.IntervalMS) * time.Millisecond
	spent := budget.BeginTick(interval)
	if spent.Refused > 0 || spent.Deferred > 0 {
		// Not an error the loop can act on — the reads that were refused are
		// retried on later ticks by the same paths that retry a tracker read
		// that failed (§9.8). Loud because a tick that cannot afford its own
		// reconciliation is a daemon running slower than it looks.
		o.log.Warn("tracker calls crossed its per-tick API request budget; work was refused or deferred to later ticks",
			"billed", spent.Billed, "unbilled", spent.Unbilled,
			"late_billed", spent.LateBilled, "late_unbilled", spent.LateUnbilled,
			"pending", spent.Pending, "refused", spent.Refused, "deferred", spent.Deferred)
		return
	}
	o.log.Debug("tracker api requests last tick",
		"billed", spent.Billed, "unbilled", spent.Unbilled,
		"late_billed", spent.LateBilled, "late_unbilled", spent.LateUnbilled,
		"pending", spent.Pending, "deferred", spent.Deferred)
}

// beginReconcile issues the §9.8 reads: one Get per running record, and one
// conditional list read serving the held set and the parked records together.
func (o *Orchestrator) beginReconcile(ctx context.Context) {
	if o.reconcileInFlight {
		// The previous tick's reads have not come back. Skipping is right:
		// queueing them would pile requests onto a tracker already slow.
		return
	}
	o.reconcileInFlight = true

	// Snapshot what needs refreshing before handing off; the worker must not
	// read loop-owned state.
	type want struct {
		id    string
		issue core.Issue
	}
	var wants []want
	var parked []parkedWant
	for id, r := range o.records {
		// Running, per §9.8 — and that set is *locked*, not a default. The spec
		// names it exactly, so adding a state here is a §9.8 amendment and needs
		// the sign-off any other one does. `queued` is the tempting one: such a record
		// holds a claim and a §9.5 slot while the content check runs, and nothing
		// refreshes it. It stays out, and the one thing that needed noticing — an
		// issue deleted under that check — is answered where the failure is: the
		// change-log read the check begins with states absence itself
		// (core.ErrIssueNotFound).
		switch r.State {
		// StateClaimed is included: a claim whose label write is still
		// retrying can sit here for several ticks, and the issue can go
		// terminal in that window (SPEC §9.8).
		case StateClaimed, StateRunning, StatePreparing, StateVerifying:
			wants = append(wants, want{id, r.Issue})
		case StateNeedsReview:
			// Parked records are classified from the sweep read instead (§9.8 as
			// amended, parked.go). Skipped here only when there is nothing a
			// response could tell us: an ordered exit or a drain already owns the
			// record, or its confirming Get is out and decides.
			if !r.exiting() && !r.absenceInFlight {
				parked = append(parked, parkedWant{id: id, labelsSettled: !r.owesProjection()})
			}
		}
	}
	// One read for both sets, and only when something reads it: a result carrying
	// no read must not be mistaken for one that found nothing (see sweepResult).
	sweepNeeded := len(o.held) > 0 || len(parked) > 0
	// One tracker for the whole pass, captured here and used to completion. A
	// reload landing mid-pass must not have the first Get answered by one adapter
	// and the next by another: the pass would describe a world that never existed,
	// and §9.8's one conditional list read per tick would become two.
	tracker := o.bundle().Tracker
	go func() {
		res := signal{kind: sigReconciled, refreshed: map[string]refreshResult{}, parked: parked}
		for _, w := range wants {
			issue, err := tracker.Get(ctx, w.id)
			res.refreshed[w.id] = refreshResult{issue: issue, err: err}
		}
		// One conditional list read for the held set and the parked records
		// together, however large either grows (SPEC §9.8).
		if sweepNeeded {
			issues, err := tracker.HeldClaims(ctx)
			res.sweep = sweepResult{read: true, issues: issues, err: err}
		}
		o.send(ctx, res)
	}()
}

// beginDispatchReads runs §9.4 steps 2 and 3: defensive config revalidation,
// then the candidate fetch it gates.
func (o *Orchestrator) beginDispatchReads(ctx context.Context) {
	if o.dispatchInFlight || o.reconcileInFlight || o.draining || o.cycleScanInFlight || o.cycleScanFailed {
		// Draining is a cost gate: a daemon on its way out should stop spending
		// tracker requests, while dispatch itself remains the claim-write gate for
		// a Fetch already in flight. Reconciliation and the durable cycle scan are
		// ordering gates. A candidate read may start only after both have answered;
		// whichever finishes second re-enters this method.
		return
	}
	o.dispatchInFlight = true
	// Captured before the reads, compared after: the whole result is scoped to
	// the configuration in force when it started. See onTickResult.
	rev := o.configNow().Revision
	go func() {
		// The revalidation runs in the worker because it reads the file. A
		// failure skips the fetch and the dispatch — never the reconciliation,
		// which is not behind either of them.
		res := signal{kind: sigTickResult, revision: rev}
		snap := o.revalidate(ctx)
		if snap.Blocked == nil {
			// The tracker the revalidation left in force, not the one captured
			// before it: this read decides what to dispatch, and dispatching is
			// the reload's (SPEC §5.4). Used to completion from here.
			candidates, err := snap.Runtime.Tracker.Fetch(ctx)
			res.candidates, res.err = candidates, err
		}
		o.send(ctx, res)
	}()
}

// retryPendingExits re-drives what must be retried rather than assumed:
// every tracker write a record still owes, a stop whose termination was
// never confirmed, and the handoff of a finished run to the held set.
// Reconciliation only revisits issues whose *tracker* state changed, so
// without this a budget-stopped run — active and routable, so nothing in the
// refresh disturbs it — would sit stopping forever.
func (o *Orchestrator) retryPendingExits(ctx context.Context) {
	for _, r := range o.records {
		// Any tracker write still owed — a projection, a milestone, the
		// release itself — is re-driven here until it lands.
		o.driveOwed(ctx, r)

		// A `done` record whose writes have landed converts to a held claim.
		// Nothing else moves it, so a refused or failed anchor read would
		// otherwise leave it parked in done for the life of the process.
		o.driveHold(ctx, r)
		if r.stopping && r.handle != nil {
			o.beginStop(ctx, r, core.StopDiscard)
		}
		// A verified claim whose §9.5 check could not be read. Reconciliation
		// does not refresh `queued` records — there is no run to stop — so
		// without this a change log or content read that failed once would leave
		// the claim held and the issue never dispatched or parked. beginApproval
		// owns the in-flight guard.
		if r.State == StateQueued && r.claimVerified {
			if r.claimEpoch > 0 {
				o.beginClaimBase(ctx, r)
			} else {
				o.beginApproval(ctx, r)
			}
		}
		if r.State == StateBackoff && r.claimBaseDispatch && r.claimEpoch > 0 {
			o.beginClaimBase(ctx, r)
		}
		// A finished run whose execution domain was not confirmed quiet: ask
		// again. Until it is, its outcome stays held (SPEC §7.5, §9.8). Both
		// this and the stop above are no-ops while teardown is already running
		// — beginStop owns that guard, since they share the one slot.
		o.confirmQuiet(ctx, r)
	}
}

// onReconciled applies §9.4 step 1. It is deliberately not gated on anything
// the dispatch half learned: this is the half that stops runs whose issues
// have gone away, and it must land whether or not a fetch ever does.
func (o *Orchestrator) onReconciled(ctx context.Context, s signal) {
	o.reconcileInFlight = false

	// One snapshot for the whole pass. §5.4 gives reconciliation to the reload,
	// and this is where it takes effect — but every issue in the pass has to be
	// judged against the same answer to "what is active, what is ours", or a
	// reload landing mid-pass leaves the verdicts describing two configurations.
	cur := o.configNow()
	o.reconcile(ctx, s.refreshed, cur)
	// The retained claims this tick's list response did not return. Collected
	// rather than confirmed inside the sweep: one budget pays for a confirmation
	// here and for the settled releases below, so it cannot be spent until both
	// candidate sets are known (offerHeldConfirmations).
	var absentHeld []string
	if s.sweep.read {
		absentHeld = o.sweepHeld(ctx, s.sweep, cur)
		o.sweepParked(ctx, s.parked, s.sweep, cur)
	}
	// The one question those releases cannot answer for themselves, and the one
	// offerOwedConfirmations asks below for a run record's owed writes: is there
	// still an issue, and a claim of ours on it, to release? Without it a release
	// the tracker can never accept is retried for the life of the process, its
	// held entry is never dropped, and a drain waiting on that entry never
	// completes (#135, held.go).
	o.offerHeldConfirmations(ctx, absentHeld)
	// Independent of the sweep read, and of whether one was made at all: a
	// settled release is an owed write, not a conclusion of this tick's list
	// response. After the confirmation offer so a selected read owns the record's
	// one operation slot; an inconclusive result re-drives its release directly.
	o.retryHeldExits(ctx)
	// After both, and before the record retries below, because it is what those
	// exits are waiting on: an ended workspace cycle's disposal is ordered ahead of
	// the tracker release that would give up the claim making it findable (#252,
	// cycle.go). Later in the tick than the sweep that settles the verdict, so the
	// ordinary case starts on the same tick it is decided.
	o.driveEndedCycles(ctx)
	o.retryPendingExits(ctx)
	// The one question those retries cannot answer for themselves: is the issue
	// still there? A write's own refusal never classifies (#134), and no read set
	// above covers a record whose exit is already ordered — so without this a
	// write that can never land is retried for the life of the process, holding
	// the record's §9.5 slot with it (absence.go).
	o.offerOwedConfirmations(ctx)
	// Run markers whose removal has not landed for a workspace already proven free.
	// Not on a record — the records these belonged to are usually gone by now, which
	// is exactly why the retry cannot live on one (see freeWorkspaceMarker).
	o.driveMarkerClears(ctx)
	// A candidate §9.10 could not account for owes a *read*, not a write, so
	// neither retry above reaches it. Without this the issues a briefly-down
	// tracker hid at startup would stay held and unworked for the life of the
	// process (see retryRecovery).
	o.retryRecovery(ctx)
	// §9.10 step 5 again, while anything it deferred can still change — a run that
	// may still be live ends, and the workspace is swept "once that run is confirmed
	// gone". A no-op when the last pass finished.
	o.retrySweep(ctx)

	// §9.4 step 2 follows step 1, and now actually does: the candidate read
	// starts against a world reconciliation has already been applied to. If a
	// previous tick's fetch is still out this is a no-op — reconciliation
	// having run is the point, and it just did.
	//
	// It bounds what the ordering buys, though. An exit reconciliation began
	// here — a stop, then a release — completes on its own signals, and its
	// record keeps its slot until it does. That is §9.5 working as intended: a
	// process that may still be alive is still a live agent process. What the
	// order removes is the *inversion*, where dispatch decided against state
	// older than the reconciliation that had already landed.
	o.beginDispatchReads(ctx)
}

func (o *Orchestrator) onTickResult(ctx context.Context, s signal) {
	o.dispatchInFlight = false

	// One read of the source, and everything below answers from it.
	cur := o.configNow()
	if cur.Revision != s.revision {
		// A reload was adopted while these reads were out, so every part of this
		// result predates it. The candidates go with it rather than being
		// salvaged: they were selected under the old definition's label partition
		// and active states, and dispatching them would spend the new
		// configuration's slots on the old one's answer to "what is BEN's?"
		// (§8.3, §5.4's "applies to future dispatch").
		//
		// This is also the path a revalidation takes when it is the thing that
		// found the edit — it publishes, so the revision it captured is already
		// behind. One extra tick, and the fetch it cost was skipped anyway
		// whenever the verdict was a block.
		o.log.Info("discarding dispatch reads that a reload overtook", "issue_count", len(s.candidates))
		// Replaced now, not at the next tick. The transition woke the ticker, but
		// that wake fired while this read was still out — so the tick it caused
		// found the dispatch half busy and did nothing, and the wake is spent.
		// Without asking again here, a reload meant to bring the next cycle
		// forward would instead cost the queue a full poll interval.
		//
		// A whole tick, not the dispatch half of one. §9.4 orders reconciliation
		// before dispatch and the order carries information (see onTick) —
		// starting the replacement fetch from here would let it decide against a
		// world the reconciliation it skipped was about to change, which is the
		// inversion that ordering exists to prevent. If reconciliation is already
		// in flight this tick is a no-op and the replacement follows from it,
		// which is the same guarantee arrived at the same way.
		o.onTick(ctx)
		return
	}

	// A revalidation that found a newer configuration applies to future dispatch,
	// retry scheduling and launches (SPEC §5.4). In-flight runs are untouched:
	// each attempt completes through the adapters it captured.
	//
	// This is the dispatch cycle's linearization point — the snapshot is carried
	// from here through eligibility, capacity and the claim, and no step below
	// re-reads the source.
	if err := cur.Blocked; err != nil {
		o.log.Warn("dispatch blocked by config validation; reconciliation still ran", "error", err)
		return
	}
	if s.err != nil {
		if !o.logCredentialFailure("fetching candidates: credential failure", "", s.err) {
			o.log.Error("fetching candidates", "error", s.err)
		}
		return
	}
	if o.cycleScanInFlight || o.cycleScanFailed {
		// This Fetch may have started before the obligation directory became
		// unreadable. It cannot authorize dispatch while BEN cannot say which
		// issues an independently durable disposal owns.
		return
	}
	o.dispatch(ctx, s.candidates, cur)
}

// reconcile applies §9.8 to the refreshed tracked issues.
func (o *Orchestrator) reconcile(ctx context.Context, refreshed map[string]refreshResult, cur snapshot) {
	for id, res := range refreshed {
		r, ok := o.records[id]
		if !ok || r.exiting() {
			continue
		}
		if r.State == StateNeedsReview {
			// The record parked while this pass was out, so its verdicts belong to
			// the sweep now (SPEC §9.8 as amended) — and every rule below is wrong
			// for a parked record: there is no run to stop, and the routability
			// check must not fire in the reapproval window §9.5 leaves open. Its own
			// classification follows on the next tick, which is the tick that reads
			// a response composed after the park.
			continue
		}
		if gone(res) {
			// Deleted or transferred. The adapter says so with a named error
			// rather than a nil issue, and it is a fact about the world, not
			// a failed read: treating it as transient would leave the run
			// going forever.
			r.markGone()
			o.stopAndFinish(ctx, r, false, "issue is gone from the tracker")
			continue
		}
		if res.err != nil {
			// Refresh failure → keep everything running; retry next tick.
			//
			// Including a credential failure of any class, deliberately: the
			// class routes §9.8's *attempt* retry and §9.7's verification, and
			// nothing here (SPEC §9.8, amendment 14). The loop cannot park what
			// it has not claimed, and a read it stopped retrying is a daemon
			// that has stopped noticing the world. What the class buys here is
			// severity — a permanent failure logs at error naming the authority,
			// so a wrong trust policy is read off the log rather than inferred
			// from a silent stall.
			if !o.logCredentialFailure("refreshing tracked issue: credential failure", id, res.err) {
				o.log.Warn("refreshing tracked issue", "issue", id, "error", res.err)
			}
			continue
		}
		fresh := *res.issue

		// Terminal comes first: a run whose issue a human closed is finished, and
		// its claim and workspace are no longer doing anything useful. The parked
		// half of §9.8 keeps the same order, for the same reason (applyParked).
		if !o.active(cur.Definition, fresh) {
			o.stopAndFinish(ctx, r, false, "issue went terminal")
			continue
		}

		if !o.routable(cur, fresh) {
			// Active but unroutable → stop, keep the workspace, release.
			o.stopAndFinish(ctx, r, true, "issue is no longer routable")
			continue
		}
		// Routing forward, content pinned. Reconciliation is not a dispatch
		// decision — it exists to notice that a run must stop — so it neither
		// checks the approval nor moves the pin, and a live run's next
		// continuation attempt renders the bytes its claim approved (SPEC §9.5).
		r.adoptRouting(fresh)
	}
}

// gone reports whether a refresh proved the issue absent, as opposed to
// failing to ask (SPEC §9.8).
func gone(res refreshResult) bool {
	return errors.Is(res.err, core.ErrIssueNotFound) || (res.err == nil && res.issue == nil)
}

// stopAndFinish begins a stop and, once termination is confirmed, disposes
// and releases. An unconfirmed stop retains the claim and retries next tick:
// a possibly-alive process must never share a workspace with a replacement
// (SPEC §9.8).
func (o *Orchestrator) stopAndFinish(ctx context.Context, r *Record, keepWorkspace bool, why string) {
	r.keepOnStop = keepWorkspace
	r.stopReason = why
	// An exit outranks §9.9's park, and this is where that is said for a ladder
	// already walking: a budget stop this exit overtook shares the one slot
	// (beginStop), so its answer comes back here — and onStopped must route it
	// out of the machine rather than into a park that keeps the claim, the
	// workspace and a `ben:needs-review` projection (SPEC §9.8, #236). The
	// `budgetStop` cause remains intact for the attempt record.
	r.finishAfterStop = true

	if r.handle != nil {
		r.stopping = true
		o.beginStop(ctx, r, core.StopDiscard)
		return
	}
	if r.pending > 0 {
		// A Prepare or a Start is in flight. Exiting now would drop the
		// record, and its result would land with nobody to own the workspace
		// it created or the process it launched. Record the intent; the
		// worker's signal completes it.
		r.finish = &finishRequest{keepWorkspace: keepWorkspace, why: why}
		return
	}
	o.finishNow(ctx, r, keepWorkspace, why)
}

// finishIfRequested completes a deferred exit once the work it was waiting on
// has reported back. It returns true when the record is on its way out and
// the caller should stop processing the signal.
func (o *Orchestrator) finishIfRequested(ctx context.Context, r *Record) bool {
	if r.finish == nil || r.pending > 0 {
		return false
	}
	req := r.finish
	r.finish = nil

	if r.handle != nil {
		// The work that landed was a Start: stop the process we now own
		// rather than abandon it.
		r.stopping = true
		o.beginStop(ctx, r, core.StopDiscard)
		return true
	}
	o.finishNow(ctx, r, req.keepWorkspace, req.why)
	return true
}

func (o *Orchestrator) finishNow(ctx context.Context, r *Record, keepWorkspace bool, why string) {
	o.attemptEnded(ctx, r)
	// An attempt still in flight when the exit was ordered — one abandoned in
	// `preparing`, most often — ends here, deliberately stopped (#60, SPEC §7.3).
	// The exception has already ended for a more specific reason: a confirmed
	// budget stop can be reading its prior-attempt account when this exit arrives.
	// That read defers the park, not the cause, so preserve `budget_exceeded` just
	// as endAttemptSuspended does when a drain lands in the same window.
	reason := core.FailureKilled
	if r.parkOnBudgetPending {
		r.parkOnBudgetPending = false
		reason = core.FailureBudgetExceeded
	}
	// A no-op for every other route in, since an attempt that reached its own
	// outcome recorded it there and this guard is idempotent.
	o.recordAttempt(r, reason, VerdictUnknown)
	o.abandonPendingClaimBase(ctx, r)
	o.disposeRevoked(ctx, r, keepWorkspace)
	if r.gone || r.claimLost {
		// Nothing to release: the claim died with the issue, or the assignment
		// that *was* the claim is already gone (SPEC §8.3). Asking the tracker
		// to unassign an issue it no longer has is a 404, an owed write that
		// errors is retried every tick, and a record whose release never lands is
		// never forgotten — so the issue that does not exist would hold a §9.5
		// concurrency slot for the life of the process. A release of an issue
		// somebody else now holds is the milder version: it succeeds, having
		// removed a principal that was not there (§8.4), which spends a write to
		// state something the tracker already said.
		//
		// Owed rather than forgotten outright, so the disposal queued above still
		// lands: effects are head-of-line per record, and forgetting drops the
		// queue with them. Same shape as the release it replaces — the record
		// leaves when its last owed effect does — and the same conclusion §9.10
		// reaches for a `done` claim whose issue is gone (onDoneOwnership).
		//
		// Once, for the reason the disposal above is once (Record.disposalOwed):
		// an exit that already owed a forget can be reached a second time when a
		// confirming Get finds the issue gone (absence.go), and a second forget is
		// an effect that could never run — the first one drops the record.
		if !r.owesForget() {
			o.owe(ctx, r, effectForget, effectLocal, func(context.Context, *Orchestrator) error { return nil })
		}
		return
	}
	o.release(ctx, r, why)
}

// dispatch selects and starts work (SPEC §9.5): FIFO by created_at, oldest
// first, identifier as tiebreak; only issues the adapter called dispatchable
// and that we are not already tracking; only while slots remain.
func (o *Orchestrator) dispatch(ctx context.Context, candidates []core.Issue, cur snapshot) {
	if o.draining {
		// The gate that matters, and the one beginDispatchReads is *not*. That
		// one stops new reads; this one stops the claim, which is the write. A
		// Fetch already in flight when the signal landed returns here afterwards
		// — possibly long afterwards, since it is the read a wedged tracker
		// holds — and would claim, project a label and post a milestone for an
		// issue this daemon is in no position to work on.
		o.log.Info("discarding dispatch candidates; the daemon is shutting down", "issue_count", len(candidates))
		return
	}
	eligible := make([]core.Issue, 0, len(candidates))
	for _, c := range candidates {
		if !c.Dispatchable {
			continue
		}
		if _, tracked := o.records[c.Identifier]; tracked {
			continue
		}
		if _, retained := o.held[c.Identifier]; retained {
			// We are still holding this issue's claim. The adapter's
			// dispatchable verdict already excludes any assignee, so this is
			// the second lock on the same door — but the door is a second run
			// on an issue that already has an owner, and the sweep drops the
			// held record only once the release is confirmed.
			continue
		}
		if o.endedCycleOwed(c.Identifier) {
			// At least one previous workspace cycle still owns cleanup for this
			// issue. Its provider address is independent of a replacement, but the
			// obligation is still an ownership claim: do not start a new tenure while
			// cleanup is unresolved. The next poll dispatches after all addresses
			// confirm.
			continue
		}
		if o.sweepDisposing[c.Identifier] {
			// A §9.10 step 5 pass holds this issue's workspace and may be about to
			// remove it: the issue was terminal when the pass began and has reopened
			// since. Claiming here would race a disposal against a Prepare over one
			// directory — the pass reserved precisely so that cannot happen, and this
			// is the reservation's other half. It lasts one pass over one workspace,
			// so the next poll dispatches the issue.
			continue
		}
		eligible = append(eligible, c)
	}
	sort.Slice(eligible, func(i, j int) bool {
		if !eligible[i].CreatedAt.Equal(eligible[j].CreatedAt) {
			return eligible[i].CreatedAt.Before(eligible[j].CreatedAt)
		}
		return eligible[i].Identifier < eligible[j].Identifier
	})

	for _, issue := range eligible {
		if o.freeSlots(cur.Definition) <= 0 {
			return
		}
		o.beginClaim(ctx, issue, cur)
	}
}

// active reports whether the issue's state is one the workflow works on.
func (o *Orchestrator) active(def *config.WorkflowDefinition, issue core.Issue) bool {
	states := def.Config.Tracker.ActiveStates
	if len(states) == 0 {
		states = []string{"open"}
	}
	return containsFold(states, issue.State)
}

// routable reports whether the issue still belongs to this daemon: required
// labels present, and assigned to our claim principal and nobody else.
//
// The identity check is the point. "Exactly one assignee" would accept a
// human who unassigned BEN and took the issue themselves — the run would
// carry on without the claim it depends on. A co-assignee is equally
// unroutable, which is the §8.4 known-window case §9.8 exists to catch.
func (o *Orchestrator) routable(cur snapshot, issue core.Issue) bool {
	for _, want := range cur.Definition.Config.Tracker.RequiredLabels {
		if !containsFold(issue.Labels, want) {
			return false
		}
	}
	// The principal from the same snapshot as the labels. Read separately, a
	// reload landing between them would answer "does this carry our labels" and
	// "is it still assigned to us" under two configurations — and the second
	// question decides whether a live run is torn down.
	return len(issue.Assignees) == 1 && equalFold(issue.Assignees[0], cur.Runtime.ClaimPrincipal)
}

func hasStateLabel(issue core.Issue) bool {
	for _, l := range issue.Labels {
		if len(l) >= 4 && equalFold(l[:4], "ben:") {
			return true
		}
	}
	return false
}
