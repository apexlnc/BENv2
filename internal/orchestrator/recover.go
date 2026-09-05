package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Recovery's I/O half (SPEC §9.10). recover_classify.go decides what the facts
// mean; this file gathers them, and turns each verdict into a local record and
// the tracker writes it owes.
//
// The invariant the whole file serves: **at the moment the first tick fires,
// every issue assigned to the claim principal is already accounted for by a
// local record.** A candidate with no record is dispatchable, so one left
// unclassified is a candidate the tick can put a second agent onto. Adoption is
// therefore the linearization point — a candidate is recovered when its record
// exists, not when its label or comment lands — and the owed writes may still be
// draining after Recover returns.

// ErrNotRecovered is Run's refusal to start a loop that has not recovered.
//
// Loud rather than implicit, because both halves of the seam matter. Recovery
// has a soft-failure mode §6.4 names — a tracker that will not answer at startup
// is a warning, not a fatal error — so the caller has to be able to see it; and a
// caller that simply forgot would otherwise get a daemon that duplicate-
// dispatches every issue it already holds, which is the zero-value-means-safe
// mistake #53 removed from core.Termination.
var ErrNotRecovered = errors.New("orchestrator: Run before Recover — every claim the principal holds must be accounted for before the first tick can dispatch (SPEC §9.10)")

// Recover reconstructs the world from the tracker, git, and the run markers,
// before the loop starts (SPEC §9.10).
//
// It runs on the caller's goroutine with no loop running, which is what lets it
// touch records and held claims directly: during recovery there is no other
// mutator, so the single-writer discipline holds trivially. Every write it
// orders is an ordinary owed effect, drained by Run's effect queue afterwards.
//
// The error it returns is §6.4's warn-and-continue made visible: a candidate read
// that failed means this process cannot account for what it holds, and the caller
// decides whether to start anyway. Recovery is still marked done — Run's refusal
// is about *forgetting*, not about failing.
func (o *Orchestrator) Recover(ctx context.Context) error {
	o.recovered.Store(true)

	cur := o.configNow()
	var cycleErr error
	if source, ok := cur.Runtime.Workspaces.(endedCycleSource); ok {
		refs, err := source.EndedCycles(ctx)
		if err != nil {
			cycleErr = fmt.Errorf("recovery ended-cycle disposal read: %w", err)
			o.applyCycleScan(cur.Runtime.Workspaces, nil, err, true)
		} else {
			o.applyCycleScan(cur.Runtime.Workspaces, refs, nil, true)
		}
	} else {
		o.cycleScanFailed = false
	}
	if o.cfg.FailureReasons == nil {
		// §9.10 step 6's cap, said out loud. Without this line an unimplemented
		// dependency and a genuine cold start produce the identical comment —
		// "the reason did not survive the restart" — and only one of them is a
		// legitimate degradation. The other is a missing capability wearing its
		// sentence.
		o.log.Warn("no transition-log reader is configured: this daemon cannot report the failure reason of a run that ended before a restart, " +
			"and every recovered `failed` comment will say the reason did not survive (SPEC §9.10 step 6)")
	}
	if o.cfg.RunGone == nil {
		// The other capability whose absence is not a fact. An identified marker
		// stays possibly-live forever without a prober, so a workspace with a run
		// that really did die is never freed and its issue never converges.
		o.log.Warn("no run prober is configured: a workspace whose marker identifies a previous run can never be confirmed free, " +
			"so such issues are retained rather than resumed (SPEC §9.10 step 4)")
	}

	candidates, err := cur.Runtime.Tracker.ClaimedByPrincipal(ctx)
	if err != nil {
		// Warn and continue (SPEC §6.4). There is nothing to classify and nothing
		// to guess: an unread candidate set is not an empty one, so no record is
		// created and no claim is touched. Dispatch will still only claim issues
		// the adapter calls dispatchable, which excludes everything assigned.
		//
		// But the scan must be *redone*, and this is the flag that makes a tick do
		// it. Without one, nothing ever looks again: the unclassified-record retry
		// cannot help, because a scan that failed produced no records to retry, and
		// §8.3 excludes assigned issues from the ordinary Fetch — so every claim
		// this principal holds would sit untouched and unworked until somebody
		// restarted the daemon. That is the silent-cap shape with a longer fuse.
		o.scanOwed = true
		// And with it the sweep, which never ran: it is sequenced after the
		// classification pass because it needs the ownership set that pass produces,
		// so a scan that failed takes step 5 down with it. Left owed rather than
		// skipped for the life of the process.
		o.sweepPassOwed = true
		// The advisory the config watcher reads, refreshed here because Recover is the
		// one mutator that runs before any loop turn can do it. Without this line the
		// watcher would be told this daemon is quiescent for as long as it takes the
		// first tick to land, and AdoptIdentity would refuse the candidate it built on
		// the strength of that answer.
		o.publishIdentityWork()
		if !o.logCredentialFailure("recovery could not read the claims this principal holds; starting without reconstructing them, and retrying on later ticks",
			"", err, "principal", cur.Runtime.ClaimPrincipal) {
			o.log.Warn("recovery could not read the claims this principal holds; starting without reconstructing them, and retrying on later ticks",
				"principal", cur.Runtime.ClaimPrincipal, "error", err)
		}
		return errors.Join(cycleErr, fmt.Errorf("recovery candidate read: %w", err))
	}

	if cycleErr != nil {
		// The unreadable record cannot say which candidate it owns. Hold the whole
		// candidate set unclassified and owe step 1 again; adopting any one could
		// resume a replacement while its old cycle was being deleted. The durable
		// scan blocks ordinary dispatch until it succeeds, then the candidate retry
		// reconstructs these records before dispatch reopens.
		o.scanOwed = true
		// Step 5 needs the same ownership set. Leave the whole pass owed; a
		// successful durable scan and candidate retry unblock it in that order.
		o.sweepPassOwed = true
		o.sweepBundle = cur.Runtime
		o.publishIdentityWork()
		return cycleErr
	}
	o.log.Info("recovering", "candidates", len(candidates), "principal", cur.Runtime.ClaimPrincipal)
	for _, issue := range candidates {
		o.recoverCandidate(ctx, issue, cur)
	}
	// Inline, because there is no loop yet: Recover runs on the caller's goroutine
	// before Run, so the rule the retry path obeys — never block the authority
	// goroutine — has nothing to protect here.
	res := o.runSweep(ctx, sweepPlan{full: true, owned: o.ownedIssues(), cur: cur})
	o.sweepPassOwed, o.sweepDeferred, o.sweepSkipped = res.listFailed, res.deferred, res.skipped
	o.sweepCursor = res.cursor
	if o.sweepPassOwed || len(o.sweepDeferred) > 0 || len(o.sweepSkipped) > 0 {
		o.sweepBundle = cur.Runtime
	}
	o.publishIdentityWork()
	return cycleErr
}

// The §9.10 step 5 sweep: dispose the workspaces of issues in a terminal tracker
// state.
//
// Terminal, established by asking, not inferred from absence. §6.4 keeps a
// failure's workspace and a `failed` verdict releases the claim, so "no record owns
// it" and "not assigned to this principal" both describe a kept failure exactly as
// well as a merged issue's residue — and the case that makes this pass necessary is
// precisely the one neither can see: an issue that failed, was released, and was
// then closed while BEN was down. It never appears in ClaimedByPrincipal again, so
// no candidate accounts for it.
//
// Four things are left in place, each on positive grounds: a workspace a live
// record owns; one whose issue is unknown, because nothing records whose it is
// (core.WorkspaceRef); one whose issue is still open, which may be the failure §6.4
// retains; and one whose run marker is not absent, which is the precondition step 5
// names — "left in place and swept once that run is confirmed gone".
//
// The last of those is why this is not a one-shot. What it defers is remembered
// *per workspace* and re-examined, so a run that ends converges with no human while
// a workspace nothing can resolve does not drag the rest of the directory through a
// tracker read every tick.

// maxSweepExaminations bounds how many workspaces one §9.10 step 5 pass asks the
// tracker about.
//
// The bound is the whole reason step 5 cannot starve the rest of the tick. A
// workspace whose issue cannot be read stays deferred, so a directory holding enough
// of them re-examines all of them every tick — and the GitHub adapter admits 39
// ordinary billed requests per window (its `ordinaryPerTick`), which reconciliation's
// Get-per-tracked-issue, the held-claim list and the candidate fetch all draw on too.
// Unbounded, the sweep spends the window, and every read behind it is refused: the
// pass makes no progress *and* takes reconciliation down with it.
//
// Eight leaves the ordinary window overwhelmingly to the work a daemon exists to do,
// and still drains a full window's worth of deferred workspaces in a handful of ticks.
// It is deliberately not configurable: it bounds a cost, and an operator who raised it
// would be buying slower reconciliation with cleanup nobody is waiting on.
const maxSweepExaminations = 8

// sweepPlan is one pass's work, snapshotted on the loop and executed off it.
type sweepPlan struct {
	// refs are the workspaces to examine; meaningful only when full is false.
	refs []core.WorkspaceRef
	// full says list the directory rather than re-examining a remembered set.
	full bool
	// owned are the issues a record or held claim is holding, matched by *issue*:
	// the refs carry the identifier, the orchestrator has no business deriving a key
	// (SPEC §6.3), and it also covers records whose workspace never resolved — an
	// unclassified candidate has an empty Workspace.Key but is emphatically holding
	// a directory.
	//
	// It is a snapshot, and a snapshot of loop-owned state goes stale the moment the
	// pass starts doing I/O. It is therefore the cheap first filter and not the
	// authority: `live` says which of the two a pass has to be.
	owned map[string]bool
	// live says a loop is running behind this pass, so every workspace it is about
	// to touch must be reserved on the authority goroutine first (reserveDisposal).
	// False only for the inline startup pass, where Recover's own goroutine is the
	// single mutator and `owned` cannot move under it — and where sending a
	// reservation to a loop that does not exist yet would simply hang.
	//
	// Set by beginSweep rather than by its callers: every pass that reaches a worker
	// is one whose snapshot can go stale, and there is exactly one that cannot.
	live bool
	// cursor is the workspace key this pass resumes from, so that the bound above
	// rotates rather than truncates. Empty starts at the beginning.
	cursor string
	cur    snapshot
}

// sweepResultSet is what a pass reports back.
type sweepResultSet struct {
	// deferred are the workspaces whose answer may still change: a run not yet
	// confirmed gone, a read that failed. Keyed by workspace key.
	deferred map[string]core.WorkspaceRef
	// skipped are the workspaces the pass did not examine because a record or held
	// claim owned the issue, keyed by *identifier* so retrySweep can find one from the
	// record set. Not a variety of `deferred`: nothing is owed about them while the
	// owner is there, and everything is owed the moment it goes.
	skipped map[string]core.WorkspaceRef
	// cursor is where the next pass resumes, set when the examination bound was
	// reached and empty when the pass got all the way round. It is what makes the
	// bound a rotation rather than a permanent floor under the sorted tail.
	cursor string
	// listFailed says the directory could not be read, so the *pass* is owed rather
	// than any particular workspace.
	listFailed bool
}

// beginSweep starts a §9.10 step 5 pass in a worker.
//
// In a worker because the pass is I/O all the way down — a directory listing, a
// tracker Get per unaccounted workspace, a liveness probe, a git resolve, a disposal
// that runs before_remove hooks — and the authority goroutine is the one goroutine
// in BEN that must never block on any of them. On the loop, a slow Get or a hook
// with a sleep in it stalls runner events, budget enforcement and shutdown: a live
// agent's problem caused by startup hygiene.
func (o *Orchestrator) beginSweep(ctx context.Context, plan sweepPlan) {
	o.sweepInFlight = true
	plan.live = true
	go func() {
		o.send(ctx, signal{kind: sigSwept, sweepResult: o.runSweep(ctx, plan)})
	}()
}

// reserveDisposal asks the authority goroutine whether a §9.10 step 5 pass may
// touch this issue's workspace, and takes the reservation that keeps the answer
// true until the pass is done with it.
//
// It exists because plan.owned is a snapshot taken before a directory listing, a
// tracker Get, a liveness probe, a git resolve and a disposal that runs hooks. All
// of that time is time in which the issue can reopen, be claimed and reach
// Prepare — and a worker still holding the old answer then deletes a live attempt's
// worktree, which is precisely the outcome §9.10's precondition exists to make
// impossible, reached from the cleanup side instead of the launch side.
//
// The reservation covers the *whole* examination and not only the Dispose call,
// because the reads decide what gets deleted and one of them writes: resolveMarker
// removes the marker of a run it has proved gone, and a marker read for the
// previous run followed by a removal after a new one has written its own is the
// same hazard with a different file. Under one reservation the pass either owns the
// question from end to end or never asks it.
//
// A refusal is not an error and not an abandonment: it means something owns the
// issue now, so the answer belongs to whatever owns it, and this workspace is
// deferred and asked about again.
func (o *Orchestrator) reserveDisposal(ctx context.Context, identifier string) (release func(), ok bool) {
	c := &sweepClaim{identifier: identifier, granted: make(chan bool, 1)}
	o.send(ctx, signal{kind: sigSweepReserve, sweepClaim: c})
	select {
	case granted := <-c.granted:
		if !granted {
			return nil, false
		}
		return func() { o.send(ctx, signal{kind: sigSweepRelease, issue: identifier}) }, true
	case <-ctx.Done():
		return nil, false
	}
}

// onSweepReserve is the loop's half of reserveDisposal: the ownership test and the
// reservation, in one turn, so nothing can claim the issue in between.
func (o *Orchestrator) onSweepReserve(c *sweepClaim) {
	c.granted <- o.grantDisposal(c.identifier)
}

// grantDisposal decides one reservation. Loop-owned state, read at the only point
// where reading it is sound.
func (o *Orchestrator) grantDisposal(identifier string) bool {
	if o.draining {
		// A drain initiates no new cleanup. retrySweep already refuses to start a
		// pass, and this is the same refusal for the pass that was already going.
		return false
	}
	if _, tracked := o.records[identifier]; tracked {
		return false
	}
	if _, retained := o.held[identifier]; retained {
		return false
	}
	if o.endedCycleOwed(identifier) {
		// An ended workspace cycle whose disposal is still owed owns this issue as
		// surely as a record does, and it can outlive every record (#252). Two
		// completions over one cycle is the collision this reservation exists to
		// prevent, arrived at from the side that has no record to notice.
		//
		// ownedIssues excludes it from the pass as well, and the redundancy is this
		// reservation's whole reason: that set is a *snapshot* taken before the pass
		// went out, so an obligation registered while it was out — a held claim
		// dropped on a confirming read, which is exactly how these become ownerless
		// — is invisible to it and visible here. Neither can be mutated away alone
		// against a test today; both can, which is what the coverage is of.
		return false
	}
	if o.sweepDisposing[identifier] {
		// Unreachable while one pass runs at a time (sweepInFlight), and left in
		// because the alternative to stating it is two workers sharing one directory.
		return false
	}
	o.sweepDisposing[identifier] = true
	return true
}

// onSweepRelease drops a reservation once the pass is done with the workspace.
func (o *Orchestrator) onSweepRelease(identifier string) {
	delete(o.sweepDisposing, identifier)
}

// runSweep executes one pass. Off the authority goroutine; touches no loop state.
func (o *Orchestrator) runSweep(ctx context.Context, plan sweepPlan) sweepResultSet {
	out := sweepResultSet{
		deferred: map[string]core.WorkspaceRef{},
		skipped:  map[string]core.WorkspaceRef{},
	}

	// **Every** pass lists, including a targeted one, and the two halves of a pass are
	// rationed differently because they cost differently. The listing is a *local*
	// directory read; the expense step 5 has to bound is the tracker Get behind each
	// examined ref, and `plan.refs` is what bounds that.
	//
	// A targeted pass that trusted its remembered refs instead would re-examine
	// directories that are no longer there — a handed-back workspace whose owner
	// disposed it on the way out is the common case, not the exotic one — spending a
	// Get and a redundant disposal on each. The listing is the only authority on what
	// exists, so a ref it does not report is resolved by absence.
	listed, err := plan.cur.Runtime.Workspaces.ListWorkspaces(ctx)
	if err != nil {
		// Escalated to a whole pass rather than half-applied. onSwept leaves both sets
		// untouched on this path, which is what stops a failed listing from reading as
		// "nothing is owned and nothing is deferred any more".
		o.log.Warn("could not list workspaces to sweep; asking again on later ticks", "error", err)
		out.listFailed = true
		return out
	}

	want := make(map[string]bool, len(plan.refs))
	for _, ref := range plan.refs {
		want[ref.Key] = true
	}

	var unowned int
	var candidates []core.WorkspaceRef
	for _, ref := range listed {
		switch {
		case ref.Identifier == "":
			unowned++
		case plan.owned[ref.Identifier]:
			// Not examined, because the record that owns the issue is trusted to deal with
			// the directory — and not every exit does: a record dropped as `gone` releases
			// nothing and disposes nothing. Reported rather than merely skipped, so that
			// when the owner goes the ref is handed back (retrySweep) instead of vanishing
			// with it.
			out.skipped[ref.Identifier] = ref
		case !plan.full && !want[ref.Key]:
			// A targeted pass. This workspace is not what it was asked about, and asking
			// the tracker about it is the cost the deferred set exists to avoid.
		default:
			candidates = append(candidates, ref)
		}
	}
	if plan.full && unowned > 0 {
		// Only worth saying for a pass that was looking at the whole directory; a
		// targeted one walks past these without considering them.
		o.log.Info("workspaces left in place: nothing records which issue they belong to, so nothing can say "+
			"they are terminal (SPEC §6.4 keeps a failure's workspace)", "count", unowned)
	}

	// Round-robin from where the last pass stopped, and examine at most
	// maxSweepExaminations of them.
	//
	// The rotation is the half that matters. The listing is key-ordered, so a bound
	// applied from the top would spend every window on the same prefix: with enough
	// persistently deferred workspaces — a tracker that will not answer about them, and
	// 39 is all it takes to exhaust GitHub's ordinary window — the sorted tail is never
	// reached at all, and no amount of waiting changes that. Resuming past the last one
	// examined gives every workspace the front of a window in turn, so a directory
	// nothing can resolve costs a bounded slice of each tick instead of starving the
	// rest of the set forever.
	start := 0
	for i, ref := range candidates {
		// The first key at or past the cursor; none means the set has shrunk past it, and
		// starting over is the answer that visits everything.
		if plan.cursor == "" || ref.Key >= plan.cursor {
			start = i
			break
		}
	}
	examined := 0
	for ; examined < len(candidates) && examined < maxSweepExaminations; examined++ {
		ref := candidates[(start+examined)%len(candidates)]
		if o.sweepOneWorkspace(ctx, ref, plan) {
			out.deferred[ref.Key] = ref
		}
	}
	if examined < len(candidates) {
		// The bound stopped this pass short. Everything it did not reach is still owed —
		// retained rather than reported resolved, which is the difference between a pass
		// that is *paced* and one that silently drops what it ran out of room for.
		out.cursor = candidates[(start+examined)%len(candidates)].Key
		for i := examined; i < len(candidates); i++ {
			ref := candidates[(start+i)%len(candidates)]
			out.deferred[ref.Key] = ref
		}
		o.log.Info("workspace cleanup paced: examined some of the workspaces owed a look and will resume "+
			"from the next on a later tick (SPEC §9.10 step 5, §8.5)",
			"examined", examined, "outstanding", len(candidates)-examined, "resume_from", out.cursor)
	}
	return out
}

// onSwept applies a pass's result on the authority goroutine.
func (o *Orchestrator) onSwept(s signal) {
	o.sweepInFlight = false
	if s.sweepResult.listFailed {
		o.sweepPassOwed = true
		return
	}
	o.sweepPassOwed = false
	o.sweepDeferred = s.sweepResult.deferred
	o.sweepCursor = s.sweepResult.cursor
	// The workspaces this pass left to their owners, carried so that a record dropped
	// without disposing does not take the last account of one with it. Replaced whole
	// rather than merged: this pass listed or re-examined exactly these refs, and an
	// entry it did not report is one that is no longer there.
	//
	// An owner that vanished *while the pass was out* is safe here. The pass skipped
	// that ref because the ownership snapshot said it was owned, so the ref is in this
	// result — and retrySweep asks the record set, not the snapshot, when it decides
	// what to hand back.
	o.sweepSkipped = s.sweepResult.skipped
	if len(o.sweepDeferred) == 0 && len(o.sweepSkipped) == 0 {
		// Nothing left to ask about, so nothing is bound to that bundle any more.
		o.sweepBundle = nil
	}
}

// retrySweep re-drives §9.10 step 5 while anything is outstanding.
//
// Three kinds of outstanding, and they cost differently. A whole *pass* is owed when
// one never ran — the candidate scan it follows failed — or when the directory could
// not be listed. Individual *workspaces* are deferred when their own answer may
// still change, and only those are re-examined: one workspace nobody can resolve
// must not drag every other directory through a tracker read every tick, which is
// the recurring cost the unknown-launch case is excluded from in the first place.
// And a *skipped* workspace becomes owed the moment its owner goes, which is the
// handback below.
func (o *Orchestrator) retrySweep(ctx context.Context) {
	if o.sweepInFlight || o.draining {
		return
	}
	if o.scanOwed || o.cycleScanFailed {
		// The ownership set is what keeps this from deleting a live orphan's branch,
		// and it is produced by the classification pass. While the candidate scan is
		// still owed there has been no such pass, so every workspace would look
		// unowned — the reading §6.4 forbids, arrived at from the other direction.
		return
	}
	if !o.sweepPassOwed && len(o.sweepDeferred) == 0 && !o.sweepHandbackOwed() {
		return
	}

	cur := o.configNow()
	if o.sweepBundle != nil && o.sweepBundle.Identity() != cur.Runtime.Identity() {
		// The *identity* moved, so these refs name directories under a root this
		// daemon no longer addresses: re-examining them through the current provider
		// would ask about paths somewhere else entirely and dispose whatever happened
		// to share a key under the new root.
		//
		// Keyed on the identity and not on the bundle pointer, because a reload that
		// rebuilds the adapters without moving workspace.root — a hook edit, a
		// credential rotation — leaves every one of these refs addressable exactly as
		// before. Dropping them there traded per-workspace deferrals for a full pass,
		// which is the recurring tracker cost the deferred set exists to avoid, and it
		// did so on the reloads §5.4 says are never refused, so it happened routinely.
		//
		// Reaching this at all is now a fail-closed backstop rather than the ordinary
		// path: step 5's unfinished work is identity work (identityWorkOutstanding), so
		// AdoptIdentity refuses a root change while any of it stands. What is left here
		// is a publication that never went through that barrier — the first one, which
		// has no previous runtime to compare, or a caller that wired none — and for
		// those the debt is genuinely unreachable and saying so is the honest answer.
		o.log.Warn("abandoning deferred workspace cleanup: a reload moved the workspace identity it was "+
			"found under, so those directories are no longer addressable from here (SPEC §9.10 step 5)",
			"workspaces", len(o.sweepDeferred), "skipped", len(o.sweepSkipped))
		o.sweepDeferred, o.sweepSkipped = nil, nil
		o.sweepBundle, o.sweepPassOwed = nil, true
	}

	owned := o.ownedIssues()
	// The handback: a workspace a pass left to its owner, whose owner has since gone.
	//
	// Decided here rather than pushed by `forget`, and that is what keeps it cheap and
	// unforgettable at once. The ownership set is being read on the line above anyway,
	// so the question costs nothing; and because nothing has to *remember* to hand a
	// ref back, no exit path can omit it — which matters, since the paths that drop a
	// record without disposing its workspace are exactly the ones that cannot be
	// enumerated safely.
	//
	// One ref, not a full pass. Re-owing a listing on every record removal was the
	// first answer and it was far too expensive: `forget` covers routine releases and
	// claims never attempted, so every completed issue bought a pass, and every pass
	// spends a tracker Get per unowned workspace — the kept failures §6.4 retains for a
	// human, the unknown_launch residues step 5 deliberately keeps out of the deferred
	// set. At steady throughput that is O(residue) reads per tick, which is the
	// recurring cost the deferral and the §8.5 budget exist to avoid.
	//
	// A ref whose directory the owner's own exit disposed costs one examination and
	// then leaves: an already-gone worktree disposes idempotently (workspace Dispose,
	// prune-and-retry).
	for id, ref := range o.sweepSkipped {
		if owned[id] {
			continue
		}
		delete(o.sweepSkipped, id)
		if o.sweepDeferred == nil {
			o.sweepDeferred = map[string]core.WorkspaceRef{}
		}
		o.sweepDeferred[ref.Key] = ref
	}

	plan := sweepPlan{cur: cur, owned: owned, cursor: o.sweepCursor}
	if o.sweepPassOwed {
		plan.full = true
	} else {
		for _, ref := range o.sweepDeferred {
			plan.refs = append(plan.refs, ref)
		}
		sort.Slice(plan.refs, func(i, j int) bool { return plan.refs[i].Key < plan.refs[j].Key })
	}
	o.sweepBundle = cur.Runtime
	o.beginSweep(ctx, plan)
}

// ownedIssues is the set a sweep must not touch. Loop-owned, so it is snapshotted
// before a pass is handed to a worker.
//
// Three owners, not two. An ended workspace cycle whose disposal is still owed
// owns its issue the same way — and it is the only one of the three that can
// exist with no record and no held claim behind it (#252, dropHeld), which is
// exactly the shape a set built from those two would miss.
func (o *Orchestrator) ownedIssues() map[string]bool {
	owned := make(map[string]bool, len(o.records)+len(o.held)+len(o.endedCycles))
	for id := range o.records {
		owned[id] = true
	}
	for id := range o.held {
		owned[id] = true
	}
	for _, c := range o.endedCycles {
		owned[c.issue] = true
	}
	return owned
}

// sweepOneWorkspace disposes one workspace whose issue no longer justifies it,
// and reports whether the answer may still change.
//
// "No longer justifies it" is terminality on every substrate, plus one more fact
// where a workspace outlives its claim — see the open-issue case below.
//
// `deferred` is the difference between "nothing to do here" and "not yet": a run
// that may still be live ends, and step 5 says the workspace is "swept once that
// run is confirmed gone" — so the question has to be asked again rather than
// abandoned after one pass.
func (o *Orchestrator) sweepOneWorkspace(ctx context.Context, ref core.WorkspaceRef, plan sweepPlan) (deferred bool) {
	cur := plan.cur
	issue := core.Issue{Identifier: ref.Identifier}
	fresh, err := cur.Runtime.Tracker.Get(ctx, ref.Identifier)
	switch {
	case errors.Is(err, core.ErrIssueNotFound):
		// Deleted or transferred. The issue will never be active again, which is
		// terminal by every reading that matters here.
	case err != nil:
		// §6.4: warn and continue. A read that failed is not a terminal issue, and
		// this is a *disposal* — the direction where guessing costs unpushed commits.
		// Deferred rather than dropped: the tracker answering is what changes it.
		if !o.logCredentialFailure("could not ask whether a workspace's issue is terminal; leaving it in place and asking again",
			ref.Identifier, err, "workspace", ref.Key) {
			o.log.Warn("could not ask whether a workspace's issue is terminal; leaving it in place and asking again",
				"issue", ref.Identifier, "workspace", ref.Key, "error", err)
		}
		return true
	case o.active(cur.Definition, *fresh):
		// Still open. It may be a kept failure awaiting a human, which §6.4 retains
		// precisely so somebody can look at it. Not deferred: an issue that is open
		// is a settled answer, and it will come back through the ordinary pass if it
		// closes.
		//
		// **Unless the workspace outlives its claim and the issue has left the label
		// partition.** On that substrate this is not a pause in the cycle, it is the
		// end of one: a reapproval addresses a different sandbox, so nothing will
		// ever attach to this one again (#252, remotews.BeginClaimBase).
		//
		// It is here rather than only on the loop's own obligation because this is
		// the single route that obligation cannot survive a crash on. Every other one
		// leaves the tracker claim standing, so §9.10 step 1 enumerates it and the
		// verdict is re-derived; this one has the claim *unassigned* in the same read
		// that ended the cycle, so a restart can rediscover it nowhere else. Re-derived
		// from the tracker rather than written down, which is §9.10's own posture.
		//
		// Local providers are unaffected, and not by omission: §9.10 gate 4 *keeps* a
		// local worktree when its issue leaves the partition, and the predicate is
		// false for a provider with no end-of-cycle policy.
		if !cyclesOutliveClaims(cur.Runtime.Workspaces) || o.hasRequiredLabels(cur.Definition, *fresh) {
			return false
		}
		o.log.Info("sweeping a workspace whose approval was withdrawn: its cycle has ended and nothing "+
			"will attach to it again", "issue", ref.Identifier, "workspace", ref.Key)
	}

	// Reserved here. Everything above *decided*; everything below **acts** —
	// resolveMarker removes the marker of a run it has proved gone, and Dispose removes
	// the directory — and the loop is the only place that can say whether acting is
	// still this pass's to do.
	//
	// After the terminality read rather than before it, and that ordering is what keeps
	// the reservation off dispatch's back. A reserved issue is one dispatch will not
	// claim; an issue this pass has just been told is terminal is one dispatch would not
	// claim anyway. Reserving ahead of the `Get` instead put *every* unaccounted
	// workspace's issue out of dispatch's reach for the length of a tracker round trip,
	// including the ordinary case of a released claim being re-queued — which then lost
	// a poll interval to a pass that was never going to touch it.
	//
	// It is still the ownership authority, not a formality: the read above can be
	// arbitrarily stale, so an issue that reopened and was claimed in the meantime is
	// caught here, where the record set actually is.
	if plan.live {
		release, ok := o.reserveDisposal(ctx, ref.Identifier)
		if !ok {
			// Something owns the issue now — it reopened and was claimed, or a candidate
			// scan adopted it — so the workspace is not this pass's to touch. Deferred
			// rather than dropped, and it costs nothing: the next pass's ownership filter
			// skips an owned issue before any I/O, so this entry leaves the deferred set
			// of its own accord once whatever took it settles.
			o.log.Info("leaving a workspace to the record that now owns its issue; asking again on later ticks",
				"issue", ref.Identifier, "workspace", ref.Key)
			return true
		}
		defer release()
	}

	// The workspace precondition, which step 5 names outright — and asked through
	// the same resolution recovery uses for a candidate, so an *identified* marker
	// is probed rather than assumed live. Without the probe a run that ended before
	// this daemon started would keep its workspace forever: nothing else ever looks
	// at it, because its issue is no longer a candidate.
	marker, err := o.resolveMarker(issue, core.Workspace{Key: ref.Key}, cur)
	switch {
	case err != nil:
		o.log.Warn("could not resolve a workspace's run marker; leaving it in place and asking again",
			"issue", ref.Identifier, "workspace", ref.Key, "error", err)
		return true
	case marker == recoveryMarkerPossiblyLive:
		// The one state that converges on its own: the run ends, and the next pass
		// probes again. Warned rather than logged quietly, because until it does this
		// is disk BEN is holding for a process it did not start.
		o.log.Warn("a terminal issue's workspace is left in place: a previous run is not confirmed gone; "+
			"probing again on later ticks (SPEC §9.10 step 5)",
			"issue", ref.Identifier, "workspace", ref.Key)
		return true
	case marker == recoveryMarkerUnknownLaunch:
		// No answer is coming for this one — the launch outcome is unknowable — so
		// retrying it would spend a tracker read per tick forever to be told the same
		// thing. Reported once and left for the human §9.10 sends it to.
		o.log.Warn("a terminal issue's workspace is left in place: its run marker carries no evidence, "+
			"so the launch outcome is unknown and cannot resolve without a human (SPEC §9.10 step 4)",
			"issue", ref.Identifier, "workspace", ref.Key)
		return false
	case marker != recoveryMarkerFree:
		return true
	}

	ws, ok, err := cur.Runtime.Workspaces.ResolveWorkspace(ctx, issue)
	switch {
	case err != nil:
		o.log.Warn("could not resolve a terminal issue's workspace; asking again on later ticks",
			"issue", ref.Identifier, "workspace", ref.Key, "error", err)
		return true
	case !ok:
		// No authorizing pinned epoch/base pair — which is a statement about the
		// safety record, not the directory ListWorkspaces just found. A workspace
		// whose authority is gone is residue with nothing left to verify
		// against, so returning here would mean nothing ever removes it: no candidate
		// accounts for it and no pin will ever appear.
		ws = core.Workspace{Key: ref.Key, WorkspacePaths: core.WorkspacePaths{Path: ref.Path}}
	}
	var disposeErr error
	if lifecycle, ok := cur.Runtime.Workspaces.(workspaceLifecycleCompleter); ok {
		disposeErr = lifecycle.CompleteEndedCycle(ctx, ws)
	} else {
		disposeErr = cur.Runtime.Workspaces.Dispose(ctx, ws, false)
	}
	if disposeErr != nil {
		o.log.Warn("disposing a terminal issue's workspace; leaving it in place and trying again",
			"issue", ref.Identifier, "workspace", ref.Key, "error", disposeErr)
		return true
	}
	o.log.Info("swept the workspace of a terminal issue", "issue", ref.Identifier, "workspace", ref.Key)
	return false
}

// recoverCandidate resolves one candidate's facts and adopts the verdict.
func (o *Orchestrator) recoverCandidate(ctx context.Context, issue core.Issue, cur snapshot) {
	facts, err := o.recoveryFacts(ctx, issue, cur)
	if err != nil {
		// A read that failed is not an absence, and the two land in different
		// places (§9.10: "the absence of a fact is never evidence"). Reading a 502
		// as "no assignment event" parks a healthy published claim; reading it as
		// "not ours" dispatches a second agent. So classify nothing, and hold the
		// candidate out of dispatch until a later tick can ask again.
		if !o.logCredentialFailure("recovery could not classify a claim it holds; retaining it and retrying on later ticks",
			issue.Identifier, err) {
			o.log.Warn("recovery could not classify a claim it holds; retaining it and retrying on later ticks",
				"issue", issue.Identifier, "error", err)
		}
		o.adoptUnclassified(issue, cur)
		return
	}
	verdict, facts, err := o.classify(issue, facts, cur)
	if err != nil {
		o.log.Warn("recovery could not complete a classification; retaining the claim and retrying on later ticks",
			"issue", issue.Identifier, "error", err)
		o.adoptUnclassified(issue, cur)
		return
	}
	o.applyRecovery(ctx, issue, facts, verdict, cur)
}

// classify turns one candidate's facts into a verdict, including the one read that
// depends on which verdict it is.
//
// Shared by both entry points — the startup pass and the per-record retry — because
// the read below was added to one of them first and the other silently kept posting
// "the reason did not survive" from a log that had errored. A step that belongs to
// *classification* has to live where classification happens, not beside one caller.
func (o *Orchestrator) classify(issue core.Issue, facts recoveryFacts, cur snapshot) (recoveryVerdict, recoveryFacts, error) {
	if facts.preclassified {
		return facts.verdict, facts, nil
	}
	v := classifyRecovery(facts.candidate, facts.events, facts.verify.Verdict, facts.marker, cur.Runtime.ClaimPrincipal)

	// The §7.3 reason, read only where the verdict owes a `failed` comment.
	//
	// Last rather than first, because it is the narrowest read here: exactly one row
	// of the table consults it, and a transition log that cannot be read would
	// otherwise block every other verdict — terminal cleanup, an orphan resuming, a
	// needs-review repair — none of which mention a failure reason at all. A corrupt
	// state file is a bad reason to stop releasing merged issues.
	if v.milestone != core.MilestoneFailed {
		return v, facts, nil
	}
	failure, ok, err := o.lastFailure(issue, facts.events, cur.Runtime.ClaimPrincipal)
	if err != nil {
		// Not "the reason did not survive". That sentence is §9.10 step 6's blessed
		// degraded path and this is a failure to read, which produces the identical
		// comment and is honest in only one of the two cases.
		return recoveryVerdict{}, facts, fmt.Errorf("reading the transition log for the failure reason: %w", err)
	}
	facts.failure, facts.failureSurvived = failure, ok
	return v, facts, nil
}

// lastFailure asks the §9.11 transition log for this claim cycle's failure reason.
//
// A nil reader is not an error: it is the missing capability Recover warns about at
// startup, and the honest answer is that nothing carried the reason.
//
// The cycle check is the part that matters. The log has no notion of a claim cycle
// — state.TransitionReader.LastFailure says so and hands back the timestamp for
// exactly this — so "the last failed edge in this file" can belong to a *previous*
// tenure: an issue that failed, was re-queued by a human, ran again, and failed
// again with that second edge never persisted. Publishing the first reason as this
// failure's would be inventing one, which §9.10 step 6 forbids in the same breath
// as it forbids skipping the comment. Anything older than the claim-establishing
// assignment is therefore *not survived*, and the comment says so.
func (o *Orchestrator) lastFailure(issue core.Issue, events []core.ClaimEvent, principal string) (core.RunFailure, bool, error) {
	if o.cfg.FailureReasons == nil {
		return core.RunFailure{}, false, nil
	}
	failure, ok, err := o.cfg.FailureReasons.LastFailure(issue.Identifier)
	if err != nil || !ok {
		return core.RunFailure{}, false, err
	}
	anchor := claimCycleAnchorAt(events, principal)
	if anchor.IsZero() {
		// No dated anchor to compare against. §9.10 step 2's rule — evidence means
		// evidence dated after the claim-establishing event — cannot be applied, and
		// an unapplied rule is not a passed one.
		o.log.Warn("a failure reason survived but the claim cycle has no dated anchor to date it against; "+
			"reporting it as not survived rather than risking a previous cycle's reason",
			"issue", issue.Identifier)
		return core.RunFailure{}, false, nil
	}
	if !failure.At.After(anchor) {
		// After, not "not before". §9.10 step 2 says evidence means evidence dated
		// *after* the claim-establishing event, and equality is not that: tracker
		// timestamps are second-granularity (§8.4), so a failure sharing a second
		// with the assignment could have happened on either side of it. Equality
		// cannot establish order, and the direction that treats it as evidence is the
		// one that publishes a previous cycle's reason as this failure's.
		o.log.Info("the surviving failure reason is not dated after this claim cycle began; reporting it as not survived",
			"issue", issue.Identifier, "failed_at", failure.At, "cycle_began", anchor)
		return core.RunFailure{}, false, nil
	}
	return failure, true, nil
}

// claimCycleAnchorAt is claimCycleAnchor's timestamp: when the standing assignment
// to the principal was made. Zero when the log shows none, or when the event it
// found carries no time.
func claimCycleAnchorAt(events []core.ClaimEvent, principal string) time.Time {
	var at time.Time
	for _, ev := range events {
		if !equalFold(ev.Subject, principal) {
			continue
		}
		switch ev.Kind {
		case core.ClaimEventAssigned:
			at = ev.At
		case core.ClaimEventUnassigned:
			at = time.Time{}
		}
	}
	return at
}

// recoveryFacts is the three independent reads §9.10 classifies from, plus the
// workspace precondition. Every one of them is uncached: a conditional answer
// means "nothing changed since this daemon last looked", which no restart can
// give meaning to.
type recoveryFacts struct {
	candidate recoveryCandidate
	events    []core.ClaimEvent
	marker    recoveryMarkerState
	claimBase core.ClaimBase
	// preclassified is a claim-base gate verdict. It is set only after the
	// tracker-only gates have passed, and it bypasses the projection table so a
	// missing or contradictory epoch can never be rescued by old publish
	// evidence. A matching pending epoch also uses it to resume approval without
	// asking §9.7.
	preclassified bool
	verdict       recoveryVerdict
	// verify is the whole §9.7 result, not just its verdict. The PR link is what
	// the publish milestone *is* — the adapter refuses a published comment without
	// one (github renderMilestone) — so a verdict alone leaves recovery owing a
	// write that can never land, retrying forever, with the disposal and the
	// held-claim conversion queued behind it.
	verify VerifyResult
	// workspace is the one this issue's work lives in, zero until a matching
	// pinned claim epoch/base has authorized resolution. Resolved for every candidate rather than only the ones that
	// consult evidence, because the verdicts that *dispose* need it too — an
	// adopted record with no workspace makes gate 1's disposal a silent no-op, and
	// a leaked worktree is invisible until the disk fills.
	workspace core.Workspace
	// failure is the §7.3 reason of this issue's last run, and failureSurvived says
	// whether the log carried one. Filled in by recoverCandidate *after*
	// classification, and only for the one verdict that owes a `failed` comment — a
	// read that failed never reaches here, because "the log did not carry it" and
	// "we could not read the log" produce the same comment and only one of them is
	// honest (SPEC §9.10 step 6).
	failure         core.RunFailure
	failureSurvived bool
}

func (o *Orchestrator) recoveryFacts(ctx context.Context, issue core.Issue, cur snapshot) (recoveryFacts, error) {
	events, err := cur.Runtime.Tracker.ClaimHistory(ctx, issue)
	if err != nil {
		return recoveryFacts{}, fmt.Errorf("claim history: %w", err)
	}

	facts := recoveryFacts{
		candidate: recoveryCandidate{
			issue:       issue,
			active:      o.active(cur.Definition, issue),
			inPartition: o.hasRequiredLabels(cur.Definition, issue),
		},
		events: events,
	}

	// Gates 1–4 are tracker-only and outrank local safety state: a terminal,
	// contested, unaccountable, or out-of-partition claim keeps its established
	// §9.10 disposition even when the host has lost its claim-base record. The
	// ordinary run-marker precondition still applies before any release or
	// disposal, so those rows resolve the workspace and marker but never ask the
	// claim-base gate or §9.7.
	_, gated, anchor := classifyRecoveryTrackerGates(facts.candidate, events, cur.Runtime.ClaimPrincipal)
	if gated {
		ws, _, err := cur.Runtime.Workspaces.ResolveWorkspace(ctx, issue)
		if err != nil {
			return recoveryFacts{}, fmt.Errorf("resolving the workspace: %w", err)
		}
		facts.workspace = ws
		facts.marker, err = o.resolveMarker(issue, ws, cur)
		if err != nil {
			return recoveryFacts{}, fmt.Errorf("run marker: %w", err)
		}
		return facts, nil
	}

	// Read the marker *without probing it*. Pending plus any marker entry is an
	// ordering contradiction; resolving an identified marker first could clear
	// the very evidence that makes it one. Only an exact pinned epoch proceeds to
	// the ordinary marker resolver below.
	claimBase, claimBaseErr := cur.Runtime.Workspaces.ClaimBase(ctx, issue)
	rawMarker, markerErr := cur.Runtime.Workspaces.ReadRunMarkerFor(issue)
	facts.claimBase = claimBase
	gateVerdict, resolved := classifyRecoveryClaimBase(
		anchor, claimBase, claimBaseErr, rawMarker, markerErr, currentCycleRunningEvidence(events, anchor),
	)
	if resolved {
		facts.preclassified = true
		facts.verdict = gateVerdict
		return facts, nil
	}
	if markerErr != nil {
		return recoveryFacts{}, fmt.Errorf("run marker: %w", markerErr)
	}

	// Resolve the provider's workspace only after the atomic authority has
	// authorized this epoch. Its returned tuple is independently checked against
	// the authority before the verifier is reachable.
	ws, ok, err := cur.Runtime.Workspaces.ResolveWorkspace(ctx, issue)
	if err != nil {
		facts.preclassified = true
		facts.verdict = epochFaultRecovery(anchor, "pinned claim epoch cannot resolve its workspace")
		return facts, nil
	}
	if !ok || !claimBaseAuthorizesWorkspace(claimBase, ws) {
		facts.preclassified = true
		facts.verdict = epochFaultRecovery(anchor, "resolved workspace does not carry the pinned claim epoch/base/target")
		return facts, nil
	}
	facts.workspace = ws
	// The first probe is observational. An identified unanswered Start remains
	// possibly-live here; exact replay is a launch and cannot precede the §9.7
	// verdict below.
	facts.marker, err = o.resolveMarkerValue(issue, ws, rawMarker, cur)
	if err != nil {
		return recoveryFacts{}, fmt.Errorf("run marker: %w", err)
	}

	if facts.marker == recoveryMarkerUnknownLaunch {
		// An unknown launch is an unconditional park (classifyRecovery), so every
		// read below is spent on a verdict that cannot change. Worse than wasteful:
		// a transient git or tracker failure in one of them would error out of here
		// and leave the candidate unclassified, so the needs-review projection
		// §9.10 *requires* for this state would be deferred for as long as the fault
		// lasted — on the one state that has no answer coming and must reach a human.
		return facts, nil
	}

	// The §9.7 evidence read is spent only where the table consults it: an issue
	// gate 1 releases, or one whose projection decides without it, must not cost
	// a git probe and a FindPR. classifyRecovery is total over VerdictUnknown for
	// exactly the rows that never look (it is the blocked answer only where the
	// row *does* look), so asking the classifier what it needs is not an option —
	// the question is answered from the same events it will classify from.
	if recoveryNeedsEvidence(facts.candidate, events, cur.Runtime.ClaimPrincipal) {
		res, err := o.recoveryEvidence(ctx, issue, facts.workspace, cur)
		if err != nil {
			return recoveryFacts{}, fmt.Errorf("publish evidence: %w", err)
		}
		facts.verify = res
	}

	// Replay only once the read-only facts select the orphan/backoff route. A
	// published verdict finishes the existing work, and a verifier error has no
	// verdict at all; neither may start a process. The approval event id travels
	// with the call so the remote strategy can reject a superseded workspace
	// cycle even when the assignment itself never moved.
	approval, replay := recoveryStartReplayApproval(
		facts.candidate,
		events,
		facts.verify.Verdict,
		cur.Runtime.ClaimPrincipal,
		cur.Definition.Config.Tracker.RequiredLabels,
	)
	if replay && facts.marker == recoveryMarkerPossiblyLive && cur.Runtime.ResolveRun != nil {
		facts.marker, err = o.resolveAuthorizedMarkerValue(issue, ws, rawMarker, cur, approval)
		if err != nil {
			return recoveryFacts{}, fmt.Errorf("run marker: %w", err)
		}
	}
	return facts, nil
}

// recoveryNeedsEvidence reports whether the projection table will consult §9.7
// publish evidence for this candidate.
//
// It replays the same two facts the table branches on — the gates that resolve
// without evidence, and which label the projection leaves standing or last
// removed — rather than inspecting a verdict after the fact. Getting it wrong in
// the permissive direction costs two reads; getting it wrong in the other
// direction hands the classifier VerdictUnknown, which is `blocked`, so a
// candidate would be retained rather than misclassified.
func recoveryNeedsEvidence(candidate recoveryCandidate, events []core.ClaimEvent, principal string) bool {
	anchor := claimCycleAnchor(events, principal)
	if !candidate.active || closedInCycle(events, anchor) {
		return false // gate 1
	}
	if !solePrincipal(candidate.issue.Assignees, principal) &&
		arbitrateRecovery(events, candidate.issue.Assignees, principal) != recoveryArbitrationOurs {
		return false // gate 2, other or unorderable
	}
	if anchor == 0 || !candidate.inPartition {
		return false // gates 3 and 4
	}

	projection := replayRecoveryProjection(events, anchor)
	if projection.standing {
		return projection.effectiveKnown &&
			(projection.effective == core.StateLabelClaimed || projection.effective == core.StateLabelRunning)
	}
	if !projection.sawLabeled || !projection.lastWasRemoval || !projection.lastRemovedKnown {
		return false
	}
	return projection.lastRemoved == core.StateLabelClaimed || projection.lastRemoved == core.StateLabelRunning
}

// recoveryStartReplayApproval is the final authority check for an operation
// that can turn an unanswered backend request into a newly running agent. The
// pure classifier must first select backoff from a completed §9.7 verdict, and
// the projection must still describe the active attempt rather than a human
// re-queue. The returned required-label event identifies the current workspace
// cycle; remove-and-reapply changes it even when the assignment anchor does not.
func recoveryStartReplayApproval(
	candidate recoveryCandidate,
	events []core.ClaimEvent,
	evidence Verdict,
	principal string,
	requiredLabels []string,
) (int64, bool) {
	if classifyRecoveryFacts(candidate, events, evidence, principal).action != recoveryActionBackoff {
		return 0, false
	}
	anchor := claimCycleAnchor(events, principal)
	projection := replayRecoveryProjection(events, anchor)
	if !projection.standing || !projection.effectiveKnown {
		return 0, false
	}
	if projection.effective != core.StateLabelClaimed && projection.effective != core.StateLabelRunning {
		return 0, false
	}
	return approvalCycleAnchor(events, requiredLabels)
}

// recoveryEvidence asks the §9.7 checker the same question it answers at run
// time, against the workspace this issue's branch lives in.
//
// It goes through Verifier rather than growing its own copy of the three legs
// (the contract's reuse rule): the checker is what knows that a merged PR is not
// an open one, that a branch must descend from its pinned base, and that an
// error is never success.
func (o *Orchestrator) recoveryEvidence(ctx context.Context, issue core.Issue, ws core.Workspace, cur snapshot) (VerifyResult, error) {
	// Prepare is what reattaches the branch and mints the pin, and it is
	// deliberately not called here: recovery does no git of its own, and a Prepare
	// would run this attempt's hooks before any verdict said an attempt was owed.
	if ws.Path == "" {
		// The claim-base gate must authorize an exact pinned tuple before this
		// function. Treat a zero workspace as a broken caller invariant, never as
		// negative publish evidence that an older PR might complete around.
		return VerifyResult{}, fmt.Errorf("claim %s reached verification without an authorized workspace epoch/base/target tuple", issue.Identifier)
	}
	res, err := cur.Runtime.Verifier.Verify(ctx, issue, ws)
	if err != nil {
		// Fails closed by returning the error rather than a verdict: §9.7 says a
		// verification that cannot be completed is never success, and B09 already
		// refuses to answer VerdictUnknown.
		return VerifyResult{}, err
	}
	switch {
	case res.Verdict == VerdictUnknown:
		return VerifyResult{}, errors.New("publish-evidence check returned no verdict")
	case res.Verdict == VerdictPublished && res.PRURL == "":
		// The one field the published milestone cannot be written without. A checker
		// that answers published with no link has broken its own contract, and
		// accepting it here would convert a detectable bug into a comment the
		// tracker refuses forever (github renderMilestone).
		return VerifyResult{}, errors.New("publish-evidence check reported published with no pull request URL")
	}
	return res, nil
}

// resolveMarker turns the provider's marker into the classifier's precondition,
// probing an identified run.
//
// The asymmetry is the contract: only positive proof of absence frees a
// workspace. A probe that errors, a scheme nobody recognizes, or no prober at all
// all mean possibly live — which retains the claim and costs a tick, where the
// other direction costs a second agent in a live worktree (SPEC §7.5, §9.10).
func (o *Orchestrator) resolveMarker(issue core.Issue, ws core.Workspace, cur snapshot) (recoveryMarkerState, error) {
	marker, err := cur.Runtime.Workspaces.ReadRunMarkerFor(issue)
	if err != nil {
		where := ws.Path
		if where == "" {
			where = "(no workspace resolved)"
		}
		// "We could not look" is not "there is nothing there". An unreadable
		// marker store is an environment fault, and the candidate is retained
		// unclassified rather than parked with a reason that misdescribes it.
		return recoveryMarkerUnresolved, fmt.Errorf("workspace %s: %w", where, err)
	}
	return o.resolveMarkerValue(issue, ws, marker, cur)
}

// resolveMarkerValue applies the ordinary §9.10 liveness precondition to a
// marker the caller already read. Claim-epoch recovery needs that split so it
// can inspect a pending epoch and the raw marker atomically in its reasoning:
// an identified entry must not be probed and cleared before the contradiction
// is classified.
func (o *Orchestrator) resolveMarkerValue(issue core.Issue, ws core.Workspace, marker core.RunMarker, cur snapshot) (recoveryMarkerState, error) {
	return o.resolveMarkerValueWith(issue, ws, marker, cur, o.cfg.RunGone)
}

// resolveAuthorizedMarkerValue selects the mutating resolver only after
// recoveryFacts has passed the tracker, pinned-epoch, §9.7 orphan/backoff and
// current-approval checks. All other marker reads use resolveMarkerValue and
// therefore cannot turn an unanswered request into a launch.
func (o *Orchestrator) resolveAuthorizedMarkerValue(
	issue core.Issue,
	ws core.Workspace,
	marker core.RunMarker,
	cur snapshot,
	approval int64,
) (recoveryMarkerState, error) {
	probe := func(evidence core.RunEvidence) (bool, error) {
		return cur.Runtime.ResolveRun(issue, evidence, approval)
	}
	return o.resolveMarkerValueWith(issue, ws, marker, cur, probe)
}

func (o *Orchestrator) resolveMarkerValueWith(
	issue core.Issue,
	ws core.Workspace,
	marker core.RunMarker,
	cur snapshot,
	probe func(core.RunEvidence) (bool, error),
) (recoveryMarkerState, error) {
	// Named in every diagnostic below, alongside the issue. A possibly-live
	// workspace is a thing an operator has to inspect — `ps` against the recorded
	// group, `git -C` against the worktree — and neither is addressable from an
	// issue number. Empty when no base pin stands, which is itself worth seeing.
	where := ws.Path
	if where == "" {
		where = "(no workspace resolved)"
	}

	switch marker.State {
	case core.RunMarkerAbsent:
		return recoveryMarkerFree, nil
	case core.RunMarkerUnknownLaunch:
		// Warn, not info. This is the state with no answer coming: it parks for a
		// human, and the human needs to be told which workspace to look in before
		// deciding whether anything is running in it.
		o.log.Warn("a run marker carries no evidence, so the launch outcome is unknown; parking for a human "+
			"(SPEC §9.10 step 4)",
			"issue", issue.Identifier, "workspace", where)
		return recoveryMarkerUnknownLaunch, nil
	case core.RunMarkerIdentified:
		if probe == nil {
			o.log.Warn("a run marker identifies a previous run and this daemon has no prober; treating the workspace as possibly live",
				"issue", issue.Identifier, "workspace", where,
				"scheme", marker.Evidence.Scheme, "run", marker.Evidence.ID)
			return recoveryMarkerPossiblyLive, nil
		}
		gone, err := probe(marker.Evidence)
		switch {
		case err != nil:
			o.log.Warn("probing a previous run; treating the workspace as possibly live",
				"issue", issue.Identifier, "workspace", where,
				"scheme", marker.Evidence.Scheme, "run", marker.Evidence.ID, "error", err)
			return recoveryMarkerPossiblyLive, nil
		case gone:
			// The proof §9.10 admits, so the marker comes off here — the same rule the
			// run-time path uses, that a marker is removed exactly when its run is
			// confirmed gone. Leaving it standing would be a lie with a delay in it:
			// this pass proceeds as though the workspace were free, and the *next*
			// start would read the stale marker as unknown_launch and park an issue
			// this one had already resumed.
			if err := cur.Runtime.Workspaces.ClearRunMarkerFor(issue); err != nil {
				// Not "free anyway". The workspace genuinely is, but the state that
				// says so could not be written, and proceeding would hand the next
				// start a marker describing a run this one proved dead. Retried like
				// any other failed read.
				return recoveryMarkerUnresolved, fmt.Errorf("workspace %s: clearing the marker of a run confirmed gone: %w", where, err)
			}
			return recoveryMarkerFree, nil
		default:
			// Warn, and repeated on every tick that asks again, because this is a
			// claim BEN is holding and an agent it cannot account for. §9.10 converges
			// here without a human, but "the daemon is waiting on a process it did not
			// start" is not an informational fact about a queue an operator is
			// watching — and a run that never dies waits forever, which only a log
			// line at this level will show.
			o.log.Warn("a previous run may still be live in this workspace; retaining the claim, dispatching nothing, "+
				"and probing again on later ticks (SPEC §9.10 step 4)",
				"issue", issue.Identifier, "workspace", where,
				"scheme", marker.Evidence.Scheme, "run", marker.Evidence.ID)
			return recoveryMarkerPossiblyLive, nil
		}
	default:
		// core.RunMarkerUnreadable and anything a future provider adds. Unresolved
		// yields `blocked`, never a workspace treated as free.
		return recoveryMarkerUnresolved, fmt.Errorf("workspace %s: run marker state %d is not one this daemon understands", where, marker.State)
	}
}

// applyRecovery turns one verdict into a local record and the writes it owes.
//
// Adoption is the linearization point (the contract's rule): every branch here
// leaves the issue owned by a record or a held claim before it returns, and the
// tracker writes drain afterwards on the ordinary effect queue. A branch that
// returned without adopting would leave a candidate dispatch could claim a second
// time.
func (o *Orchestrator) applyRecovery(ctx context.Context, issue core.Issue, facts recoveryFacts, v recoveryVerdict, cur snapshot) {
	log := o.log.With("issue", issue.Identifier, "action", v.action.String())

	switch v.action {
	case recoveryActionGone:
		// The issue is no longer on the tracker at all — deleted, or transferred to
		// another repository. There is no claim of ours left to release and nothing
		// to classify, so the record goes rather than being retried forever.
		log.Info("dropping a recovery candidate: the issue is gone from the tracker")
		o.forget(issue.Identifier)

	case recoveryActionBlocked:
		// The classifier itself could not resolve the facts — an unresolved marker,
		// or an evidence verdict that never arrived. Same treatment as a failed
		// read, for the same reason: it is ours and we could not account for it.
		log.Warn("recovery reached no verdict; retaining the claim and retrying on later ticks")
		o.adoptUnclassified(issue, cur)

	case recoveryActionWait:
		// A previous run may still be live in this workspace. Retain everything,
		// dispatch nothing, dispose nothing, release nothing — and still complete
		// the tracker repair the verdict underneath called for, because the
		// precondition governs the workspace alone (SPEC §9.10 step 4).
		r := o.adoptRecovered(issue, facts, v, cur, StateQueued)
		r.unclassified = true
		// Deliberately not repeating resolveMarker's sentence: that line owns the
		// diagnostic, at warning level and naming the workspace and the recorded
		// group. This one says what was *done* about it.
		log.Info("retaining the claim and the workspace; no agent dispatched")
		o.projectRecovery(ctx, r, facts, v)

	case recoveryActionApprove:
		// #15: killed after the assignment landed and before ben:claimed. Label
		// projection precedes `preparing` (§9.2, §9.3), so no attempt can have run —
		// attempt 1, and the ownership evidence is the assignment, not the label.
		//
		// Adopted as an *unapproved* claim rather than dispatched, which is what the
		// verdict now names: `queued` with the claim verified, exactly the shape
		// onClaimed leaves for beginApproval to drive (SPEC §9.5). That window —
		// assignment landed, first projection did not — is the window the content
		// check occupies, so dispatching from here would run content nobody
		// established was approved. It projects nothing and posts no milestone for
		// the same reason: announcing `ben:claimed` for a claim that may be about to
		// park for reapproval is the mistake onClaimed stopped making.
		r := o.adoptRecovered(issue, facts, v, cur, StateQueued)
		r.claimVerified = true
		log.Info("adopting an unprojected claim; the §9.5 content check decides what happens next",
			"attempt", r.Attempt)
		o.projectRecovery(ctx, r, facts, v)
		o.beginApproval(ctx, r)

	case recoveryActionBackoff:
		// An orphan, or a human re-queue. Work may already exist on the branch, so
		// the floor is attempt >= 2 and Prepare reattaches rather than force-creating
		// (§6.2, never -B). Recovery does no git itself.
		r := o.adoptRecovered(issue, facts, v, cur, StateClaimed)
		log.Info("re-entering backoff; the branch may already carry work", "attempt", r.Attempt)
		o.projectRecovery(ctx, r, facts, v)
		o.resumeRecoveredBackoff(ctx, r)

	case recoveryActionPark:
		r := o.adoptRecovered(issue, facts, v, cur, StateNeedsReview)
		r.epochFaulted = v.epochFault
		r.epochFaultDetail = v.detail
		if v.operatorErr {
			// §9.10 gate 3 and gate 2's unorderable fallback both refuse to guess,
			// and both owe a loud operator error rather than a quiet park.
			log.Error("parking for a human: recovery refuses to guess",
				"principal", cur.Runtime.ClaimPrincipal, "assignees", issue.Assignees)
		} else {
			log.Info("adopting a parked issue")
		}
		o.projectRecovery(ctx, r, facts, v)

	case recoveryActionHold:
		// Published-awaiting-review: finish the interrupted `done`. The claim is
		// retained as a held-claim record and §9.8's sweep releases it on the close.
		r := o.adoptRecovered(issue, facts, v, cur, StateDone)
		log.Info("finishing an interrupted done; retaining the claim while the PR awaits review")
		o.projectRecovery(ctx, r, facts, v)
		o.disposePublished(ctx, r)
		o.driveHold(ctx, r)

	case recoveryActionReleaseKeep:
		r := o.adoptRecovered(issue, facts, v, cur, StateQueued)
		o.projectRecovery(ctx, r, facts, v)
		log.Info("releasing our own assignment; keeping the workspace")
		o.abandonPendingClaimBase(ctx, r)
		o.completeRevocationOnly(ctx, r)
		o.release(ctx, r, "recovery: "+v.action.String())

	case recoveryActionReleaseDispose:
		r := o.adoptRecovered(issue, facts, v, cur, StateQueued)
		o.projectRecovery(ctx, r, facts, v)
		log.Info("releasing our own assignment; disposing the workspace")
		o.abandonPendingClaimBase(ctx, r)
		o.disposeRevoked(ctx, r, false)
		o.release(ctx, r, "recovery: "+v.action.String())

	default:
		// recoveryActionUnknown. The classifier is exhaustive, so reaching here is
		// a bug — and the fail-closed reading of a bug is still "do not dispatch".
		log.Error("recovery produced no action; retaining the claim unclassified")
		o.adoptUnclassified(issue, cur)
	}
}

// adoptRecovered creates the record that owns this issue from here.
//
// It sets State directly rather than transitioning into it: §9.2's map describes
// the edges a *running* daemon takes, and recovery is not taking an edge — it is
// restoring the state the tracker says the issue is already in. Routing a
// reconstructed `done` through queued → claimed → … would write a transition log
// of events that never happened and project four labels to reach the one that
// already stands.
func (o *Orchestrator) adoptRecovered(issue core.Issue, facts recoveryFacts, v recoveryVerdict, cur snapshot, state State) *Record {
	r := &Record{
		Issue: issue,
		State: state,
		// The authorized workspace this issue's work lives in. It is zero for a
		// pending or epoch-faulted claim because that state may not be projected as
		// a usable Workspace; §9.10 step 5 retains its listed directory while this
		// record owns it and takes cleanup back when the record exits.
		Workspace: facts.workspace,
		// The published PR, so the held-claim record the `done` verdict converts into
		// carries it (SPEC §9.8) and the milestone comment can name it.
		PRURL:      facts.verify.PRURL,
		Definition: cur.Definition,
		Attempt:    v.attemptFloor,
		claimEpoch: v.cycleAnchor,
		UpdatedAt:  o.clock.Now(),
		token:      o.newToken(),
		recovered:  true,
	}
	if v.attemptFloor >= 2 {
		// §9.6: the floor is a display and budget floor, not a recovered counter.
		// Recovery neither persists nor infers an exact prior count, so max_attempts
		// is measured from here while Attempt keeps reading as "work may already
		// exist" for the prompt (§5.6).
		r.attemptBase = v.attemptFloor - 1
	}
	o.records[issue.Identifier] = r
	o.publish(r)
	o.Transitions.append(TransitionEntry{
		TS: r.UpdatedAt, Issue: issue.Identifier, From: StateQueued, To: state,
		Actor: o.cfg.DaemonID, Reason: "recovered at startup: " + v.action.String(),
	})
	return r
}

// adoptUnclassified holds a candidate out of dispatch without classifying it.
//
// The one new mechanism §9.10's absence-vs-failure-to-ask rule needs, and it must
// not be a dropped record: a candidate with no record is dispatchable, so
// forgetting one we could not read would put a second agent onto an issue we
// hold. It is a record in a state the loop will not act on — StateQueued
// projects no label, is not in the reconciliation read's state set, and owes
// nothing — retried on later ticks like every other unfinished exit.
func (o *Orchestrator) adoptUnclassified(issue core.Issue, cur snapshot) {
	r := &Record{
		Issue:        issue,
		State:        StateQueued,
		Definition:   cur.Definition,
		UpdatedAt:    o.clock.Now(),
		token:        o.newToken(),
		recovered:    true,
		unclassified: true,
	}
	o.records[issue.Identifier] = r
	o.publish(r)
}

// projectRecovery orders the tracker repair a verdict carries.
//
// Both writes are unconditional where the verdict names them, and both are
// no-ops when the projection survived: SetStateLabels is a set-to (§9.3), and
// Comment is idempotent per milestone occurrence (§8.4). That idempotence is
// what makes step 4's "every recovery verdict re-issues the milestone comment
// for the state it lands in" free — and re-issuing is the *only* repair
// available, because the projection that owed the comment is already finished
// and no later transition re-attempts it.
func (o *Orchestrator) projectRecovery(ctx context.Context, r *Record, facts recoveryFacts, v recoveryVerdict) {
	if !v.project {
		return
	}
	issue, want := r.Issue, v.stateLabel
	o.oweProjection(ctx, r, "project "+labelName(want), func(ctx context.Context, o *Orchestrator) error {
		return o.bundle().Tracker.SetStateLabels(ctx, issue, want)
	})
	if v.milestone == "" {
		return
	}
	o.comment(ctx, r, o.recoveryComment(r, facts, v))
}

// recoveryComment builds the milestone comment a verdict re-issues.
//
// Two milestones need a fact beyond their own name, and both are refused by the
// adapter without it (github renderMilestone), which is why they are built from
// the facts rather than from the verdict alone:
//
//   - `published` needs the pull request link. A published comment with no link
//     asserts evidence it does not carry, so the adapter rejects it — and a
//     rejected write is owed forever, with the disposal and the held-claim
//     conversion queued behind it.
//   - `failed` needs the §7.3 reason, or an honest statement that it did not
//     survive. §9.10 step 6: take it from the local transition log when it is
//     there, never invent one, and never skip the comment — a `ben:failed` label
//     with no explanation is worse than an honest one.
func (o *Orchestrator) recoveryComment(r *Record, facts recoveryFacts, v recoveryVerdict) core.MilestoneComment {
	c := core.MilestoneComment{Milestone: v.milestone}
	switch v.milestone {
	case core.MilestonePublished:
		c.PRURL = facts.verify.PRURL
	case core.MilestoneFailed:
		if facts.failureSurvived {
			c.Reason = facts.failure.Reason
			// The operator-facing line the original failure carried, where there was
			// one: §9.10 owes the *reason*, and the detail beside it is what makes the
			// reconstructed comment as useful as the one that was lost.
			c.Detail = facts.failure.Detail
			r.FailureReason = facts.failure.Reason
			break
		}
		// The log was read and does not carry this run's reason, or there is no log
		// to read. The comment is identical either way, which is exactly why the
		// second case earns a startup warning of its own (see Recover). A log that
		// could not be *read* never arrives here — it is an error out of
		// recoveryFacts, and the candidate is retried.
		c.ReasonUnavailable = true
		c.Detail = "recovered after a restart; the failure reason did not survive"
	case core.MilestoneNeedsReview:
		// The operator-facing line the evidence produced, where there was one. A
		// park reached without an evidence read simply has none.
		c.Detail = facts.verify.Detail
		if v.detail != "" {
			c.Detail = v.detail
		}
	}
	return c
}

// resumeRecoveredBackoff puts an adopted orphan on the failure track.
//
// It transitions rather than assigning, because this *is* an edge the loop takes:
// the record was adopted in `claimed` to match the label already standing, and
// claimed → preparing → backoff is how it reaches a timer. Going straight to
// backoff would skip the state the tracker agrees with.
func (o *Orchestrator) resumeRecoveredBackoff(ctx context.Context, r *Record) {
	if err := o.transition(ctx, r, StatePreparing, "recovered orphan: resuming the failure track"); err != nil {
		return
	}
	// No §7.3 cause: this edge was taken by *recovery*, not by a failure. Stamping
	// one would put a reason on a transition no failure caused, which §9.10 step 6
	// would later read back and name (see TransitionEntry.FailureReason).
	o.enterBackoff(ctx, r, "recovered orphan: work may already exist on the branch (SPEC §9.10)", "")
}

// retryRecovery re-asks the questions a startup pass could not answer.
//
// Called from the tick, and a no-op in the ordinary case where the set is empty.
// It exists because `retryPendingExits` re-drives *writes* and stops, and an
// unclassified candidate owes neither: what it is missing is a read. Without this
// a tracker that was briefly down at startup would leave its issues held but
// never worked for the life of the process — the silent cap version of failing
// closed.
//
// One classification in flight per record, like every other asynchronous read
// (heldClaim.inFlight): the reads are per-candidate and a tick every poll
// interval would otherwise stack them.
func (o *Orchestrator) retryRecovery(ctx context.Context) {
	cur := o.configNow()
	o.retryRecoveryScan(ctx, cur)

	for _, r := range o.records {
		if !r.unclassified || r.recoverInFlight {
			continue
		}
		issue, token := r.Issue, r.token
		r.recoverInFlight = true
		// The revision these reads are taken under, compared when they land. An
		// identity reload adopted meanwhile rebinds the principal and the repository,
		// and a verdict reached against the old one is about a different world — see
		// onRecoveryScan for what that costs.
		rev := cur.Revision
		go func() {
			s := signal{kind: sigRecovered, issue: issue.Identifier, token: token, revision: rev}
			// The issue as the tracker reports it *now*, not as the failed pass saw
			// it. Minutes or hours can pass while a fault persists, and every gate
			// classifies from current state — whether it is closed, who is assigned,
			// which labels it carries. Re-deciding from the stale copy would produce
			// a verdict about a world that has moved, which is the mistake §9.10's
			// cache-bypass rule exists to prevent within a single pass.
			fresh, err := cur.Runtime.Tracker.Get(ctx, issue.Identifier)
			switch {
			case errors.Is(err, core.ErrIssueNotFound):
				// Deleted or transferred: no claim of ours survives it, and there is
				// nothing to classify or release. Reported as a verdict so the loop
				// drops the record rather than retrying forever.
				s.recovery, s.recoveryReads = &recoveryVerdict{action: recoveryActionGone}, &recoveryFacts{}
			case err != nil:
				s.err = fmt.Errorf("refetching the issue: %w", err)
			case !containsFold(fresh.Assignees, cur.Runtime.ClaimPrincipal):
				// The claim is no longer ours. A human took the issue, or released it,
				// while we were failing to classify it — and every §9.10 verdict is
				// about *our* claim, so there is nothing here to project or release.
				//
				// It has to be caught before classification rather than left to the
				// gates. An unassigned issue reaches gate 2 with no contender the log
				// can order, which is the unorderable fallback: BEN would park
				// `ben:needs-review` on an issue nobody holds and strand it, having
				// been told plainly that it is not ours.
				s.recovery, s.recoveryReads = &recoveryVerdict{action: recoveryActionGone}, &recoveryFacts{}
			default:
				facts, ferr := o.recoveryFacts(ctx, *fresh, cur)
				if ferr != nil {
					s.err = ferr
					break
				}
				v, facts, ferr := o.classify(*fresh, facts, cur)
				if ferr != nil {
					s.err = ferr
					break
				}
				s.recovery, s.recoveryReads = &v, &facts
			}
			o.send(ctx, s)
		}()
	}
}

// retryRecoveryScan redoes the §9.10 step 1 candidate read after a startup scan
// that failed, and adopts anything it finds that is not already owned.
//
// One in flight at a time, and it keeps trying until a scan *succeeds* — not until
// it returns something. An empty successful scan is an answer ("this principal
// holds nothing"); an error is not.
func (o *Orchestrator) retryRecoveryScan(ctx context.Context, cur snapshot) {
	if !o.scanOwed || o.scanInFlight || o.draining || o.cycleScanInFlight || o.cycleScanFailed {
		return
	}
	o.scanInFlight = true
	tracker, rev := cur.Runtime.Tracker, cur.Revision
	go func() {
		candidates, err := tracker.ClaimedByPrincipal(ctx)
		o.send(ctx, signal{kind: sigRecoveryScan, candidates: candidates, err: err, revision: rev})
	}()
}

// onRecoveryScan applies a retried candidate scan on the authority goroutine.
func (o *Orchestrator) onRecoveryScan(ctx context.Context, s signal) {
	o.scanInFlight = false
	cur := o.configNow()
	if cur.Revision != s.revision {
		// A reload was adopted while the scan was out, so its answer is about a
		// world that has been replaced. Discarding it is not caution: the read is
		// "every issue *this principal* holds", and an identity reload rebinds the
		// principal and the repository together (Bundle.Identity). Salvaging the
		// candidates would classify one repository's issue identifiers against
		// another's — the same identifier naming a different issue — and a verdict
		// from that projects labels and releases claims on whatever happens to
		// share the number.
		//
		// scanOwed stays set, so the next tick asks the new configuration the same
		// question. Nothing is lost but a poll interval.
		o.log.Info("discarding a recovery scan that a reload overtook", "issue_count", len(s.candidates))
		return
	}
	if s.err != nil {
		if !o.logCredentialFailure("re-reading the claims this principal holds; retrying next tick",
			"", s.err, "principal", cur.Runtime.ClaimPrincipal) {
			o.log.Warn("re-reading the claims this principal holds; retrying next tick", "error", s.err)
		}
		return
	}
	// The scan spoke, so it is not owed again. Whatever it returned is now held by
	// a record — classified or not — and the ordinary per-record retry takes over.
	o.scanOwed = false
	adopted, deferred := 0, 0
	for _, issue := range s.candidates {
		if _, tracked := o.records[issue.Identifier]; tracked {
			continue
		}
		if _, retained := o.held[issue.Identifier]; retained {
			continue
		}
		if o.endedCycleOwed(issue.Identifier) {
			// Reassigned to this principal while its previous workspace cycle is
			// still being released. Its exact backend address is independent of the
			// replacement, but the disposal still owns the issue until confirmation;
			// adopting would start a second tenure across that ownership boundary.
			//
			// Skipping is only safe because something comes back for it, and nothing
			// else would: §8.3 excludes an assigned issue from the ordinary Fetch, so
			// an assigned candidate this pass declined is one no read in the daemon
			// looks at again. Marked on the obligation rather than re-owing the scan
			// here — clearEndedCycle owes it when the disposal confirms, which is the
			// event that unblocks it. Re-owing it now would spend a
			// ClaimedByPrincipal read per tick until then, and the tail of this
			// function re-enters the scan directly, so it would spend them without
			// even waiting for a tick.
			for _, c := range o.endedCycles {
				if c.issue == issue.Identifier {
					c.deferredCandidate = true
				}
			}
			deferred++
			continue
		}
		// Held out of dispatch first, classified second. The record has to exist
		// before this turn ends — a candidate with no record is dispatchable, and
		// dispatch runs on this same goroutine — so the reads happen on the ordinary
		// retry path rather than inline here.
		o.adoptUnclassified(issue, cur)
		adopted++
	}
	o.log.Info("recovery scan succeeded on retry",
		"candidates", len(s.candidates), "newly_held", adopted, "deferred", deferred)
	o.retryRecovery(ctx)
}

// onRecovered applies a retried classification on the authority goroutine.
func (o *Orchestrator) onRecovered(ctx context.Context, r *Record, s signal) {
	r.recoverInFlight = false
	// One read of the source, carried through the whole turn. Re-reading it after
	// the check would let a reload land in between and apply an old verdict under a
	// new configuration — the check would pass against one snapshot and the record
	// be adopted against another, which is the thing the check exists to prevent.
	//
	// Deliberately *not* covered by a test, and said here because that is a claim
	// worth being able to check. The window is two consecutive statements on the
	// authority goroutine, so reaching it needs a hook keyed to this exact call
	// site: a reload forced into any earlier read is caught by the check below, and
	// one forced later is not in this function at all. A test that arranged neither
	// would pass against both spellings and read as coverage. The discipline that
	// makes this safe is structural instead — every decision takes one snapshot at
	// its linearization point and carries it (see configNow), which is the same rule
	// beginPrepare and beginStart already follow.
	cur := o.configNow()
	if cur.Revision != s.revision {
		// Superseded by a reload, and dropped rather than applied for the reason
		// onRecoveryScan gives: the principal and the repository move together, so a
		// verdict reached under the old identity is about a different issue that
		// happens to share an identifier. The record stays unclassified and the next
		// tick re-reads under what is now in force.
		o.log.Info("discarding a recovery classification that a reload overtook", "issue", s.issue)
		return
	}
	if !r.unclassified {
		// Something else took the record while the read was out. Whatever it is
		// doing now owns it.
		return
	}
	if o.sweepDisposing[r.Issue.Identifier] {
		// A step 5 pass is examining this issue's workspace and may be about to
		// remove it. This is the one route by which a record can appear *after* a
		// reservation was granted — a retried candidate scan adopts unclassified
		// records, and it must, since a candidate with no record is dispatchable — so
		// it is also where the second half of the reservation has to hold. Applying
		// the verdict now could put the record on the backoff track and have Prepare
		// reattach a worktree the pass then deletes.
		//
		// The record stays unclassified, which is what holds it out of dispatch, and
		// retryRecovery asks again next tick. Nothing is lost but a poll interval.
		o.log.Info("holding a recovery verdict: a workspace cleanup pass holds this issue's workspace",
			"issue", r.Issue.Identifier)
		return
	}
	if s.err != nil || s.recovery == nil || s.recoveryReads == nil {
		if !o.logCredentialFailure("re-classifying a retained claim; retrying next tick",
			r.Issue.Identifier, s.err) {
			o.log.Warn("re-classifying a retained claim; retrying next tick",
				"issue", r.Issue.Identifier, "error", s.err)
		}
		return
	}
	if s.recovery.action == recoveryActionBlocked {
		// Still no verdict. Stay unclassified rather than recording an answer
		// nobody gave.
		return
	}
	// The record is replaced rather than mutated: applyRecovery adopts a fresh one
	// with the state, attempt floor and owed writes the verdict calls for, and a
	// half-updated record is how a `done` verdict would come to own a workspace
	// the previous state had already disposed.
	o.forget(r.Issue.Identifier)
	// The issue the verdict was *reached from*, not the copy the failed pass left on
	// the record. They differ by however long the fault lasted, and the difference is
	// not cosmetic: a §9.10 dispatch renders the prompt from Issue.Title and
	// Issue.Body (SPEC §5.6), so adopting the stale one launches an agent against a
	// description a human has since rewritten.
	o.applyRecovery(ctx, s.recoveryReads.candidate.issue, *s.recoveryReads, *s.recovery, cur)
}

func (a recoveryAction) String() string {
	switch a {
	case recoveryActionBlocked:
		return "blocked"
	case recoveryActionWait:
		return "wait"
	case recoveryActionApprove:
		return "approve"
	case recoveryActionBackoff:
		return "backoff"
	case recoveryActionPark:
		return "park"
	case recoveryActionHold:
		return "hold"
	case recoveryActionReleaseKeep:
		return "release, keep workspace"
	case recoveryActionReleaseDispose:
		return "release, dispose workspace"
	case recoveryActionGone:
		return "issue gone from the tracker"
	default:
		return fmt.Sprintf("recoveryAction(%d)", uint8(a))
	}
}
