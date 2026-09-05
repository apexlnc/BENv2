package orchestrator

import (
	"context"
	"errors"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// ErrApprovalUnread is the change log failing to show the required-label set
// completely applied, so no approval can be named to compare against.
//
// It states what was *observed* and deliberately not what follows, because that
// differs by caller and reading it as one thing everywhere was a bug. From the
// held sweep the labels were just seen complete in a list response, so a log that
// disagrees is behind and the claim is retained. At conversion there has been no
// such read, and the likeliest cause is the opposite: the set really was removed.
// Settling that against current state is the caller's job (confirmDoneOwnership),
// and a caller that treated it as "ask again" would retry an issue that has left
// the workflow for the life of the process.
var ErrApprovalUnread = errors.New("orchestrator: the change log names no standing approval to compare against")

// The disposal a *workspace cycle* owes once the tracker ends it (#252).
//
// On a substrate whose workspace outlives its claim — `on_success: suspend` is
// what lets a reviewer resume the same sandbox (docs/REVIEW.md) — the tree is
// still allocated after `done` has disposed the claim and the claim is retained
// awaiting review. What ends the cycle is a tracker fact: the complete
// required-label set removed, or the issue gone terminal. After either, a
// reapproval mints a different cycle address, so nothing will attach to that
// sandbox again and only the configured `on_revoked` disposal can release it.
//
// **The obligation is keyed by exact cycle address rather than carried by
// whichever owner noticed.** Three different owners reach the same moment — a
// held claim whose sweep released it, a `done` run record whose claim-cycle
// anchor never resolved, and a provider record recovered after downtime — and
// one issue may owe more than one of them. An obligation that lives beside the
// owners cannot be forgotten by picking the wrong one or overwritten by the
// next cycle for the issue.
//
// Two properties follow, and both are load-bearing:
//
//   - **The tracker claim is not given up while one stands.** driveOwed holds a
//     record's release behind it and driveHeldExit holds a held claim's, so the
//     claim that makes this obligation re-derivable after a restart (§9.10 step 1
//     enumerates it; the sweep re-derives the same verdict from the same labels)
//     outlives an in-memory obligation. A provider-local durable obligation is
//     also enumerated directly before recovery classifies any claim.
//   - **It runs off both queues, under a bound.** A delete does not return until
//     compute release, volume destruction and record tombstoning each confirm,
//     which can be minutes; the orchestrator's effect queue is one serial worker
//     shared by every issue's label writes and every record's local effects, so a
//     disposal on it would hold all of them behind an unreachable control plane.
type endedCycle struct {
	// issue is the tracker identifier this cycle belonged to. Carried because the
	// set is keyed by cycle *address* — one issue can owe two at once — while every
	// ownership gate asks by issue (endedCycleOwed).
	issue string
	// workspace is the opaque cycle identity — see cycleWorkspace for what is
	// deliberately not in it.
	workspace core.Workspace
	// workspaces is the provider that issued the address. A non-identity reload
	// may replace the current bundle while a disposal is in flight; completing
	// through the captured provider keeps the address and its store together.
	workspaces Workspaces
	// why is the tracker fact that ended the cycle, for the operator log.
	why string
	// inFlight is the one outstanding disposal for this cycle. A second would be
	// a second delete against the same claim: idempotent at the backend, and
	// still two calls where the bound below promises one.
	inFlight bool
	// deferredCandidate says a §9.10 candidate scan found this issue assigned to
	// the principal again and declined to adopt it, because this obligation owns
	// it (onRecoveryScan). §8.3 excludes an assigned issue from the ordinary
	// Fetch, so nothing else will ever come back for that candidate — and
	// clearEndedCycle owes the scan again on the strength of this flag.
	//
	// A mark here rather than re-owing the scan on the spot, because `scanOwed`
	// means something else: "the pass never spoke", which gates §9.10 step 5's
	// cleanup for *every* workspace and is re-entered by the scan handler's own
	// tail. Setting it there both froze unrelated cleanup and span.
	deferredCandidate bool
}

// endedCyclesPerTick bounds how many ended workspace cycles may have a disposal
// *started* on one tick, and it is heldConfirmationsPerTick's number for
// heldConfirmationsPerTick's argument (SPEC §9.8, decision 14).
//
// The case that produces one produces all of them: a human clearing the queue
// label across a review backlog is one gesture, and the set of cycles it ends is
// exactly that backlog. Without a bound, K ended cycles cost K concurrent backend
// calls on the tick that notices them — and a control plane refusing them costs K
// again on every tick after that, forever.
//
// A bound on the **rate**, not on how many may be outstanding, and the difference
// is what stops one from blocking the rest. A concurrency bound of one reads as
// the tighter rule and is the wrong one: a single disposal against a control
// plane that is merely slow would hold the only slot, and every other ended cycle
// would wait behind a claim it has nothing to do with. A disposal already in
// flight is simply not a candidate here, so the next tick's turn goes to somebody
// else. What is outstanding is therefore bounded by the number of ended cycles
// rather than by a constant — each is one parked call for work genuinely owed,
// and none of them is a *new* request.
//
// K cycles therefore reclaim over K ticks, and a revocation settled between ticks
// waits until the next one. Nothing about that is urgent: the sandbox is idle by
// construction — its claim is published and its approval withdrawn — and what
// waits is the release of a claim on an issue a human has already moved on from.
//
// One, because "the same cost as one ended cycle" is the bound, and any constant
// above one is a number nobody can defend.
const endedCyclesPerTick = 1

// cyclesOutliveClaims reports whether this provider's workspaces survive the
// claims that ran in them — which is the same question as "does it have an
// end-of-cycle policy", asked where the answer is about the *workspace* rather
// than about the disposal.
//
// One predicate rather than a type assertion at each site, because the two sites
// use it for opposite-looking purposes and must not drift: cycleWorkspace decides
// whether there is an obligation to carry at all, and §9.10 step 5's sweep decides
// whether an issue that has merely left the label partition is a workspace it may
// dispose. Both are false for a local provider, and for the same reason.
func cyclesOutliveClaims(workspaces Workspaces) bool {
	_, ok := workspaces.(workspaceLifecycleCompleter)
	return ok
}

// cycleSuperseded reports whether the workspace cycle a provider holds for this
// issue is no longer the one the standing approval selects.
//
// Off the authority goroutine, in the worker that has just read the change log
// `standing` was computed from, because the answer is a durable provider read and
// the loop does no I/O.
//
// `false, err` on a refusal, and the caller must not treat that as "not
// superseded": an unreadable record is the absence of an answer rather than an
// answer (SPEC §9.10), and re-baselining on it would spend the one thing that
// buys the next read.
//
// **A standing approval of zero is one of those refusals**, and reading it as
// "unchanged" was a bug: it means the log names no approval to compare against,
// which is the absence of an answer rather than "the same one". What that absence
// *means* is the caller's to decide — see ErrApprovalUnread — and settling it as
// "unchanged" here would settle it forever, because the caller re-baselines the
// revision that would have bought the next read.
func cycleSuperseded(
	ctx context.Context, workspaces Workspaces, issue core.Issue, standing int64,
) (bool, error) {
	source, ok := workspaces.(cycleApprovalSource)
	if !ok {
		// No cycle outlives its claim here, so there is nothing to supersede. Not an
		// absence: the question does not apply to this provider at all.
		return false, nil
	}
	if standing <= 0 {
		return false, ErrApprovalUnread
	}
	recorded, err := source.CycleApproval(ctx, issue)
	if err != nil {
		return false, err
	}
	// Zero is a provider holding no cycle for this issue, which supersedes nothing.
	return recorded > 0 && recorded != standing, nil
}

// cycleWorkspace is what an ended-cycle obligation may retain about the
// workspace — and, on the local substrate, why there is no obligation at all.
//
// Two providers, two different facts. A local worktree is gone by the time any
// of this applies: §6.1's Dispose removed the directory, so the paths in
// core.Workspace would name something that is not there, and there is no
// allocation left to release. A remote workspace cycle survives its claim by
// design, so its identity is the only thing that can later name the allocation.
//
// Selected on the *provider*, not on a config flag, because the question is
// which contract the workspace was created under. And reduced rather than
// copied: the cycle key, its opaque address, the branch and the assignment
// epoch. Deliberately dropped are SharedGitDir, PrivateDir and BaseSHA — the
// first two are host paths that do not exist on a substrate BEN does not own,
// and the third is a verification fact whose only reader is the checker (#252
// gains "no filesystem or untrusted-run evidence surface").
func cycleWorkspace(workspaces Workspaces, ws core.Workspace) core.Workspace {
	if !cyclesOutliveClaims(workspaces) {
		return core.Workspace{}
	}
	if ws.Key == "" {
		return core.Workspace{}
	}
	return core.Workspace{
		WorkspacePaths: core.WorkspacePaths{Path: ws.Path},
		Key:            ws.Key,
		Branch:         ws.Branch,
		ClaimEpoch:     ws.ClaimEpoch,
	}
}

// oweEndedCycle records that this issue's workspace cycle has ended.
//
// Idempotent, and deliberately silent for a workspace that cannot own one: a
// local provider, or a claim that never pinned a base and so never acquired a
// sandbox. Callers pass the workspace they hold and let this decide, rather than
// each deciding for itself — the first version of this change had that test at
// one call site and the leak was at the other two.
//
// It registers and starts nothing. The tick's driver is the only thing that
// starts a disposal, which is what makes the bound a bound: a backlog-wide
// gesture settles K verdicts inside a single walk over the held set, and a call
// that started its own would spend the budget from inside the loop the budget is
// a decision about. It is also what orders the disposal after the §6.5 hook the
// same exit queued a moment earlier (owesBeforeExit).
// Keyed by **address**, not by issue, and that is the fix for a leak of its own.
// One issue can owe two of these at once: a cycle a reapproval replaced and the
// replacement's own end, which is an ordinary sequence — revoke, reapprove,
// dispatch, revoke again. Keyed by issue, the second registration was silently
// dropped as a duplicate and its sandbox left to nobody, while the first one's
// completion released the claim on both their behalf.
func (o *Orchestrator) oweEndedCycle(id string, workspaces Workspaces, ws core.Workspace, why string) {
	cycle := cycleWorkspace(workspaces, ws)
	if cycle.Key == "" || cycle.Path == "" {
		// No address is no cycle to name. A completion cannot be addressed without
		// one, and remotews refuses an unnamed cycle rather than disposing whichever
		// occupies the key.
		return
	}
	if _, owed := o.endedCycles[cycle.Path]; owed {
		return
	}
	if o.endedCycles == nil {
		o.endedCycles = map[string]*endedCycle{}
	}
	o.endedCycles[cycle.Path] = &endedCycle{
		issue: id, workspace: cycle, workspaces: workspaces, why: why,
	}
	o.log.Info("the workspace cycle has ended and owes its configured disposal",
		"issue", id, "workspace", cycle.Key, "address", cycle.Path, "reason", why)
}

// adoptDurableEndedCycles folds a provider's independently durable obligations
// into the same rotation as obligations observed in memory. Re-reading the
// directory is intentionally idempotent: the map is keyed by the exact cycle
// address and an in-flight entry remains the authority until its result lands.
func (o *Orchestrator) adoptDurableEndedCycles(workspaces Workspaces, refs []core.WorkspaceRef) {
	for _, ref := range refs {
		ws := core.Workspace{
			WorkspacePaths: core.WorkspacePaths{Path: ref.Path},
			Key:            ref.Key,
		}
		o.oweEndedCycle(ref.Identifier, workspaces, ws,
			"the approval was withdrawn and re-applied while BEN was not observing it")
	}
}

// beginCycleScan re-reads the provider's local obligation directory each tick.
// Startup and BeginClaimBase both read it at their own linearization points; this
// pass is the retry for a refused read and for a process that survived an
// ambiguous local write.
func (o *Orchestrator) beginCycleScan(ctx context.Context) {
	if o.cycleScanInFlight || o.draining || o.cycleMutationsInFlight > 0 {
		return
	}
	workspaces := o.bundle().Workspaces
	source, ok := workspaces.(endedCycleSource)
	if !ok {
		o.cycleScanFailed = false
		return
	}
	o.cycleScanInFlight = true
	seq := o.cycleMutationSeq
	go func() {
		refs, err := source.EndedCycles(ctx)
		o.send(ctx, signal{
			kind: sigCycleScan, cycleRead: true, cycleRefs: refs,
			cycleWorkspaces: workspaces, cycleScanErr: err, cycleScanSeq: seq,
		})
	}()
}

func (o *Orchestrator) onCycleScan(ctx context.Context, s signal) {
	o.cycleScanInFlight = false
	fresh := s.cycleScanSeq == o.cycleMutationSeq && o.cycleMutationsInFlight == 0
	if s.cycleScanErr != nil || fresh {
		// An error always closes the gate conservatively. A successful stale
		// snapshot is ignored wholesale: in particular, it may still contain an
		// obligation whose completion landed while the directory read was out.
		o.applyCycleScan(s.cycleWorkspaces, s.cycleRefs, s.cycleScanErr, fresh)
	}
	// Reconciliation and this local read start together. Whichever finishes
	// second starts dispatch, preserving §9.4's reconcile-before-fetch order while
	// making the obligation listing a fail-closed dispatch prerequisite.
	o.beginDispatchReads(ctx)
}

// applyCycleScan applies only a complete listing. A refusal leaves every known
// obligation in place and blocks new dispatch globally: an unreadable file does
// not reveal which issue needs the ownership gate.
func (o *Orchestrator) applyCycleScan(
	workspaces Workspaces, refs []core.WorkspaceRef, err error, mayClear bool,
) bool {
	if err != nil {
		o.cycleScanFailed = true
		o.log.Warn("could not read ended workspace-cycle disposals; blocking dispatch and retrying next tick",
			"error", err)
		return false
	}
	o.adoptDurableEndedCycles(workspaces, refs)
	if mayClear {
		o.cycleScanFailed = false
	}
	return true
}

// endedCycleOwed reports whether this issue's cycle still owes one.
//
// It is two things at once, and the second is the one that is easy to miss. It
// gates giving up the tracker claim, in both owners — and it is an **ownership
// claim on the issue**, because an obligation can outlive every owner
// (dropHeld). The durable identities are per cycle, but the tracker claim they
// hold is necessarily per issue.
//
// That second reading is what stops dispatch or release from overtaking cleanup.
// The provider resolves a durable old-cycle completion through its exact address,
// not through the live issue record, but the tracker claim still coordinates the
// issue's ownership: no new tenure begins and no recoverability anchor is given
// up until every address for the issue is confirmed.
// A scan rather than a lookup, because the set is keyed by cycle address and one
// issue can owe two. It is a handful of entries — bounded by the ended cycles
// awaiting confirmation — against a question asked per dispatch candidate, which
// is the same order the ownership set beside it already costs.
func (o *Orchestrator) endedCycleOwed(id string) bool {
	if o.cycleScanFailed {
		// A refused full-directory read cannot identify the issue its unreadable
		// entry owns. Treat every issue as possibly owned until a complete scan
		// answers, so the failure gates tracker-claim release as well as dispatch.
		return true
	}
	for _, c := range o.endedCycles {
		if c.issue == id {
			return true
		}
	}
	return false
}

// cycleEndedBy answers "do these facts end the workspace cycle" from one
// confirming read, and names the reason. It is the only place that question is
// answered, because getting it wrong is expressible in both directions.
//
// Three facts end a cycle, and the third is the one that is easy to reach past:
//
//   - The issue is **gone** — deleted, or transferred out of this repository. No
//     approval on this tracker will ever address this cycle again.
//   - The issue is **terminal**. §9.10 gate 1's verdict, and what merging a PR
//     carrying `Fixes #<n>` produces.
//   - The issue has left the **label partition**. A reapplication is a new
//     approval and therefore a different cycle address (remotews.BeginClaimBase).
//
// **Assignment is deliberately not one of them.** A claim handed back with every
// required label standing is the changes-requested route, and its cycle is alive
// for the next claim epoch to resume — disposing there would delete the tree the
// revision was going to be written in. That is why this is asked *before* the
// paths that drop a record on a disappearance: those check whether the claim is
// still ours, which is a different question, and an issue that is both closed and
// unassigned answers the wrong one first.
//
// A read that failed is not a fact and ends nothing (SPEC §9.10); the caller
// retains and asks again.
func (o *Orchestrator) cycleEndedBy(def *config.WorkflowDefinition, res refreshResult) (string, bool) {
	if gone(res) {
		return "issue is gone from the tracker", true
	}
	if res.err != nil || res.issue == nil {
		return "", false
	}
	switch {
	case !o.active(def, *res.issue):
		return "issue went terminal", true
	case !o.hasRequiredLabels(def, *res.issue):
		return "issue left the workflow's label partition", true
	}
	return "", false
}

// driveEndedCycles is the tick's one turn at starting a disposal, and the only
// thing that starts one.
//
// Two filters before the rotation, and neither is a budget:
//
//   - **In flight.** Its call is already out, so it is not a candidate for a new
//     one. This is what keeps a slow disposal from blocking the rest — the turn
//     goes to somebody else rather than being spent waiting on it.
//   - **Owing something before its exit.** The record's queue ahead of the exit is
//     where the §6.5 after_run hook and the claim-base abandon sit, and both touch
//     the workspace this is about to delete. They used to be sequenced by
//     head-of-line ordering because the disposal was on that queue; now that it is
//     not, this is what sequences them (owesBeforeExit). Deferred, never dropped.
//
// Rotating on the *offer* rather than on the outcome is the rest of it: a cycle
// whose control plane keeps refusing is precisely the one that must not take every
// turn, and without the cursor the lexicographically first candidate would — its
// disposal fails fast, so it is back in the candidate set before the next offer.
//
// **Not gated on the drain.** A disposal is an effect the loop already ordered,
// and `drained` waits for every one of them — so a draining daemon that stopped
// driving these would wait on work it had stopped doing, until the supervisor's
// TimeoutStopSec ended it. It initiates nothing new: every route that settles one
// of these verdicts refuses to during a drain.
func (o *Orchestrator) driveEndedCycles(ctx context.Context) {
	candidates := make([]string, 0, len(o.endedCycles))
	deferred := 0
	for address, c := range o.endedCycles {
		if c.inFlight {
			continue
		}
		if r, ok := o.records[c.issue]; ok && r.owesBeforeExit() {
			deferred++
			continue
		}
		candidates = append(candidates, address)
	}
	picked, cursor := rotate(candidates, sameIdentifier, o.disposalCursor, endedCyclesPerTick)
	o.disposalCursor = cursor
	for _, address := range picked {
		// Present by construction: read off o.endedCycles on this same loop turn.
		o.disposeEndedCycle(ctx, address, o.endedCycles[address])
	}
	if waiting := deferred + len(candidates) - len(picked); waiting > 0 {
		// Not a failure, and said out loud because a silent cap reads as "covered
		// everything": each of these keeps its obligation and takes its turn on a
		// later tick.
		o.log.Info("ended workspace cycles await disposal",
			"starting", len(picked), "waiting", waiting, "cursor", o.disposalCursor)
	}
}

func (o *Orchestrator) disposeEndedCycle(ctx context.Context, address string, c *endedCycle) {
	workspaces := c.workspaces
	if workspaces == nil {
		// Compatibility for tests and records constructed before obligations
		// captured their provider explicitly.
		workspaces = o.bundle().Workspaces
	}
	lifecycle, ok := workspaces.(workspaceLifecycleCompleter)
	if !ok {
		// Compatibility for an entry created without the captured provider. There
		// is no end-of-cycle surface that can discharge it, so drop it by name
		// rather than retrying against a stranger.
		o.log.Warn("dropping an ended workspace cycle's disposal: the workspace provider in force has no "+
			"end-of-cycle policy", "issue", c.issue, "workspace", c.workspace.Key)
		o.clearEndedCycle(ctx, address)
		return
	}
	ws := c.workspace
	c.inFlight = true
	go func() {
		err := lifecycle.CompleteEndedCycle(ctx, ws)
		o.send(ctx, signal{kind: sigCycleDisposed, issue: address, err: err})
	}()
}

// onCycleDisposed settles a disposal, and only a `nil` settles it.
//
// The provider does not report success until the backend has confirmed compute
// release, volume destruction and record tombstoning, so `nil` here is the one
// place BEN may stop owing it. Anything else keeps the obligation *and* the
// tracker claim that makes it findable: the claim is retained, the next tick
// offers this cycle another turn, and no other issue is affected — this call
// touched no shared queue.
//
// Deliberately not keyed by token, unlike every held-claim result. An obligation
// is keyed by exact cycle address, and the entry at that address is removed only
// here. A second cycle for the issue is a different entry, so a stale result
// cannot decide it; `inFlight` stops a second call for this address.
func (o *Orchestrator) onCycleDisposed(ctx context.Context, s signal) {
	c, owed := o.endedCycles[s.issue]
	if !owed {
		return
	}
	c.inFlight = false
	if s.err != nil {
		o.log.Error("completing the ended workspace cycle; retaining the claim and retrying on later ticks",
			"issue", c.issue, "address", s.issue, "workspace", c.workspace.Key, "reason", c.why, "error", s.err)
		return
	}
	// Invalidate any directory snapshot that began before this confirmation.
	// Such a snapshot can still contain the just-removed address and must not
	// recreate the loop-owned obligation after clearEndedCycle removes it.
	o.cycleMutationSeq++
	o.log.Info("completed the ended workspace cycle",
		"issue", c.issue, "address", s.issue, "workspace", c.workspace.Key, "reason", c.why)
	o.clearEndedCycle(ctx, s.issue)
	// Deliberately starts nothing else. Releasing the owner that was waiting on
	// *this* obligation is the whole job here; admitting the next cycle's disposal
	// would put an unbounded number of starts between two ticks, and — because a
	// refusal returns immediately — a spin against a backend that is already
	// failing. The tick is the one admission point (driveEndedCycles), which is the
	// cadence every other unlandable effect in the loop retries at.
}

// clearEndedCycle drops the obligation and releases whatever was waiting on it.
//
// Both owners are re-driven rather than one, and both calls are idempotent no-ops
// for an issue the other owns: a held claim and a run record never coexist for
// one issue (held.go), but which of the two is waiting is not something this
// needs to know.
func (o *Orchestrator) clearEndedCycle(ctx context.Context, address string) {
	c, owed := o.endedCycles[address]
	if !owed {
		return
	}
	id := c.issue
	if c.deferredCandidate {
		// A candidate scan found this issue assigned to us again and declined to
		// adopt it while this obligation owned it. Nothing else looks at an assigned
		// issue (§8.3), so the scan is owed once, here, on the event that unblocks
		// it — rather than on every tick until then.
		o.log.Info("re-reading the claims this principal holds: a candidate was held back by this "+
			"workspace cycle's disposal", "issue", id)
		o.scanOwed = true
	}
	delete(o.endedCycles, address)
	// Only once *every* obligation for this issue is gone: one issue can owe two,
	// and releasing the claim on the first would leave the second with nothing
	// holding the issue for it (endedCycleOwed).
	if o.endedCycleOwed(id) {
		return
	}
	if h, held := o.held[id]; held && h.releasing {
		o.driveHeldExit(ctx, id, h)
	}
	if r, ok := o.records[id]; ok {
		o.driveOwed(ctx, r)
	}
}
