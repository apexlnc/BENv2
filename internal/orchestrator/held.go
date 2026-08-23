package orchestrator

import (
	"context"
	"errors"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// A retained `done` claim has exactly one owner at a time.
//
// The run record owns the issue from dispatch until every write `done`
// ordered has landed — the cleared state label, the publish comment, the
// workspace disposal. Conversion then hands ownership to a held-claim record
// in a single loop step: the run record is removed there and only there, and
// the held record survives until the tracker confirms a release or confirms
// there is nothing of ours left to release. No path can drop one while the
// other still owes something, and nothing observes both.
//
// The boundary is the design. Two records for one issue is what let a
// confirmed release forget a run record that still owed a publish comment,
// and what let a stale read from a previous cycle drop a claim established by
// the next one.
//
// heldClaim is what remains after conversion: no workspace, no runner,
// nothing to stop (SPEC §9.2). It is not local state but a cache of a fact
// the tracker can enumerate, which is why recovery rebuilds the whole set
// rather than persisting it (SPEC §9.8; B10 adopts one at its two `done`
// verdicts).
type heldClaim struct {
	issue core.Issue
	// prURL is the pull request the claim is being held for (SPEC §9.8).
	prURL string
	// revision is the §8.3 change token last observed. It triggers a history
	// read and never decides anything by itself.
	revision string
	// cycleAnchor is the change-log id of the assignment that established
	// this claim, so a close is believed only when it falls inside this cycle
	// (SPEC §9.10 step 2). Nonzero by construction: convert refuses to build
	// a record without one, because a zero anchor accepts every historical
	// close.
	cycleAnchor int64
	// token names this held cycle, and every async result carries the token
	// it was started under. inFlight already stops two operations overlapping
	// *within* a cycle; the token is what stops one crossing a cycle
	// boundary. An issue can be released, reopened, re-dispatched and held
	// again, and a Get or a Release from the previous cycle must not decide
	// anything about the current one.
	token int
	// releasing is a settled verdict, retried until the tracker confirms it.
	// why is the reason, for the operator log.
	//
	// releaseFailed says the latest write attempt failed and has not yet been
	// offered the confirming read below. It is consumed when that read starts:
	// an inconclusive answer re-drives the release, and that attempt's own
	// failure earns another confirmation. A release nobody has refused yet has
	// nothing to explain, and spending a `Get` beside every ordinary release
	// would double the cost of the common path to answer a question it never
	// asks.
	releasing     bool
	releaseFailed bool
	why           string
	// inFlight is the one outstanding operation — a release write, a confirming
	// Get, or a history read. The confirmation may forget this record, so it can
	// never overlap the release it would cancel: a result token can discard a
	// stale write's completion, but it cannot unsend the write. Failed operations
	// are retried rather than assumed.
	inFlight bool
}

// readyToConvert is the handoff gate, and it is rechecked after **every**
// asynchronous step of the conversion, not only before the first.
//
// The ordering it enforces: `done` owes the tracker a cleared state label and
// a publish comment, and owes the provider a disposal. The run record is what
// carries and retries them. Converting first would move the claim to a record
// that cannot, and a sweep releasing a tick later would take the unsent
// comment with it.
//
// Rechecking is what makes that hold across a slow read. A tick that
// snapshotted this issue while it was still `verifying` reconciles it after
// it reaches `done`, and can decide there and then that the issue went
// terminal or unroutable — queueing a release on the record. Converting on a
// result that predates that decision deletes the release and its retry state:
// nothing re-drives it, and the sweep has no rule that re-derives it either,
// since an unroutable *assignee set* is invisible to an assignee-filtered
// list read. The claim would stand until a restart.
func (o *Orchestrator) readyToConvert(r *Record) bool {
	if r.State != StateDone || o.records[r.Issue.Identifier] != r {
		return false
	}
	// exiting() is the release already begun; owesAnything covers both its
	// write and the writes `done` itself ordered.
	return !r.exiting() && !r.owesAnything() && r.pending == 0 && !r.convertInFlight
}

// driveHold starts the conversion. Idempotent, and called both when the
// record's last owed write lands and on every tick — the second is what
// retries a read the tracker refused.
func (o *Orchestrator) driveHold(ctx context.Context, r *Record) {
	if !o.readyToConvert(r) {
		return
	}

	// One history read per issue that reaches `done` — on the done path,
	// never on the sweep path, whose cost must not grow with the held set
	// (SPEC §9.8).
	issue, token, id := r.Issue, r.token, r.Issue.Identifier
	// One bundle for the read and for the principal it is read against. The
	// anchor answers "which assignment established *our* claim cycle", so a
	// principal from a different snapshot than the history would answer a
	// question nobody asked.
	b := o.bundle()
	r.convertInFlight = true
	go func() {
		events, err := b.Tracker.ClaimHistory(ctx, issue)
		o.send(ctx, signal{
			kind: sigClaimAnchor, issue: id, token: token, err: err,
			anchor: claimCycleAnchor(events, b.ClaimPrincipal),
		})
	}()
}

func (o *Orchestrator) onClaimAnchor(ctx context.Context, r *Record, s signal) {
	r.convertInFlight = false
	if !o.readyToConvert(r) {
		// The record moved while the read was out. Whatever it is doing now
		// owns it; the next tick reconsiders the conversion if there is still
		// one to make.
		return
	}
	if errors.Is(s.err, core.ErrIssueNotFound) {
		// A read, and one that classifies itself (#134): ClaimHistory names the
		// issue and nothing else, so its not-found means the issue. There is no
		// claim to retain and nothing to release — the same verdict
		// onDoneOwnership reaches from its Get, arrived at one read earlier.
		//
		// Separated from the retry below because retrying is what a *deleted*
		// issue's read does forever: readyToConvert has already established the
		// record owes nothing, so nothing else would ever revisit it (SPEC §9.8
		// refreshes neither `done` nor an ordered exit).
		o.log.Info("dropping the finished run: the issue is gone from the tracker",
			"issue", r.Issue.Identifier)
		o.forget(r.Issue.Identifier)
		return
	}
	if s.err != nil {
		// Retried on the next tick. The claim stays retained meanwhile, which
		// is what `done` asks for anyway; what waits is only its release.
		if !o.logCredentialFailure("resolving the claim-cycle anchor; retrying next tick",
			r.Issue.Identifier, s.err) {
			o.log.Warn("resolving the claim-cycle anchor; retrying next tick",
				"issue", r.Issue.Identifier, "error", s.err)
		}
		return
	}
	if s.anchor == 0 {
		// The log does not show the assignment that established this claim.
		// That is the *absence* of a fact, which §9.10 never reads as
		// evidence: a lagging or truncated change log looks exactly like a
		// human unassignment. Ask the one question that has a positive
		// answer — is the principal assigned right now?
		o.confirmDoneOwnership(ctx, r)
		return
	}
	o.convertToHeld(r, s.anchor)
}

// confirmDoneOwnership settles a missing anchor against current assignment.
func (o *Orchestrator) confirmDoneOwnership(ctx context.Context, r *Record) {
	id, token := r.Issue.Identifier, r.token
	r.convertInFlight = true
	tracker := o.bundle().Tracker
	go func() {
		fresh, err := tracker.Get(ctx, id)
		o.send(ctx, signal{kind: sigDoneOwnership, issue: id, token: token, refetched: fresh, err: err})
	}()
}

func (o *Orchestrator) onDoneOwnership(ctx context.Context, r *Record, s signal) {
	r.convertInFlight = false
	if !o.readyToConvert(r) {
		return
	}
	id := r.Issue.Identifier
	// One record, one moment: the configuration in force now is what this
	// verdict is taken under (SPEC §5.4 gives reconciliation to the reload).
	def := o.definition()
	switch {
	case gone(refreshResult{issue: s.refetched, err: s.err}):
		// Deleted or transferred: no claim of ours survives it.
		o.log.Info("dropping the finished run: the issue is gone from the tracker", "issue", id)
		o.forget(id)
	case s.err != nil:
		if !o.logCredentialFailure("confirming the claim after an anchor-less history read; retrying next tick",
			id, s.err) {
			o.log.Warn("confirming the claim after an anchor-less history read; retrying next tick",
				"issue", id, "error", s.err)
		}
	case !containsFold(s.refetched.Assignees, o.bundle().ClaimPrincipal):
		// Positive evidence, from current state rather than from the log's
		// silence: the principal is not assigned. Nothing to retain, nothing
		// to release.
		o.log.Info("not retaining the claim: the principal is no longer assigned", "issue", id)
		o.forget(id)

	// The two facts below are the same Get's answer, and the anchor is not
	// needed to read either. The anchor exists to date a *close event* against
	// the current claim cycle — anything before it belongs to a previous one
	// (§9.10 step 2) — but these are not events. They are the issue's state
	// now, and now needs no dating.
	//
	// They are also exactly what the §9.8 sweep would decide, on its next
	// tick, from a list response saying the same thing (classifyHeld). So
	// retaining here is not caution; it is deferring a settled verdict to a
	// history read that may never resolve, while the claim it holds blocks the
	// issue for everyone.
	case !o.active(def, *s.refetched):
		o.log.Info("releasing the retained claim: the issue went terminal", "issue", id)
		o.release(ctx, r, "issue went terminal before the claim cycle could be anchored")
	case !o.hasRequiredLabels(def, *s.refetched):
		o.log.Info("releasing the retained claim: the issue left the workflow", "issue", id)
		o.release(ctx, r, "issue left the workflow's label partition before the claim cycle could be anchored")

	default:
		// Assigned, open, still in the partition — and the log cannot yet say
		// which event established the claim. *Now* fail closed: the claim is
		// retained and the anchor read is retried. Releasing on a log that has
		// not caught up would let a second daemon redo published work; holding
		// costs an issue that is undispatchable either way while its PR awaits
		// review.
		o.log.Warn("claim retained without a resolved cycle anchor; the change log shows no assignment for an issue that is assigned",
			"issue", id, "principal", o.bundle().ClaimPrincipal)
	}
}

// convertToHeld is the handoff, and the only place a `done` run record is
// removed.
func (o *Orchestrator) convertToHeld(r *Record, anchor int64) {
	id := r.Issue.Identifier
	o.held[id] = &heldClaim{
		issue:       r.Issue,
		prURL:       r.PRURL,
		revision:    r.Issue.Revision,
		cycleAnchor: anchor,
		token:       o.newToken(),
	}
	// The run record is gone from this instant; the held record is the single
	// owner. The published snapshot deliberately stays — `ben status` should
	// not lose an issue at the moment it starts awaiting review — and is
	// dropped with the held record.
	delete(o.records, id)
	o.publishHeld()
	o.log.Info("retaining the claim while the PR awaits review",
		"issue", id, "pr", r.PRURL, "anchor", anchor)
}

// heldConfirmationsPerTick bounds what confirmations may cost on the held path,
// and it is §9.8's parked number for §9.8's parked argument
// (parkedConfirmationsPerTick, and the owed-write budget in absence.go took it for
// a third reason).
//
// The case that produces one absence produces all of them: a human unassigning the
// principal across a review backlog is one gesture, and the held set grows with
// exactly that backlog. So K held claims absent from a single response cost K
// concurrent `Get`s on the tick that notices — the O(held) per-tick cost decision
// 14 exists to prevent, reached through the absence path rather than through the
// sweep read.
//
// K absences therefore resolve over K ticks. That is a behaviour change to a
// signed rule (§9.8: "confirm with one `Get` before acting"), and this is its whole
// cost: a held claim whose absence is real is released one tick later per record
// ahead of it in the rotation. Nothing about an absence is urgent — the issue is
// either no longer ours or no longer there, and either way the only thing waiting
// is the release of a claim on an issue a human has already moved on from.
//
// One, because "the same number of requests as one held claim" is the bound, and
// any constant above one is a number nobody can defend.
//
// It is the bound over **all** held-claim confirmations, not over absences alone:
// the settled releases that keep failing to land ask the same question of the same
// read (#135) and share this one budget through offerHeldConfirmations. A second
// constant for them would be a second budget, and the argument above — the
// backlog-wide gesture, the O(held) tick — is exactly the same for a tracker
// refusing writes across it.
const heldConfirmationsPerTick = 1

// sweepHeld applies the §9.8 held-claim sweep to one HeldClaims read.
//
// The read shape is the whole point: cost does not scale with the held set —
// one conditional list request per tick however long the review backlog
// grows, rather than a Get per held claim. Anything that reaches for history
// or a Get on every record every tick puts that cost straight back.
//
// The whole per-tick arithmetic of the held half, since the read shape is only the
// first term of it:
//
//   - **one** conditional `HeldClaims` read, shared with the parked sweep — and
//     already O(1) in the held set;
//   - **at most one** confirming `Get`, however many records are absent from that
//     response and however many settled releases are failing to land — one budget
//     over one rotation across both (heldConfirmationsPerTick,
//     offerHeldConfirmations);
//   - **one `ClaimHistory` per held claim whose `revision` moved** since the last
//     observation. Deliberately left as it is: §9.8 states and accepts this cost as
//     "one read per **observed change** to a held issue", an idle tick over any
//     number of records reads no history at all, and the ordinary close needs none
//     — so the term is O(changes), which is not the quantity this bound is about.
//     A backlog-wide gesture that bumps every revision at once does reach O(held)
//     here, one tick, once per gesture; bounding that is a second change to a
//     signed rule and wants its own decision, as this one did.
//
// A settled release's own *write* is outside all of this on purpose: it is an owed
// write, one per releasing record per tick, driven by retryHeldReleases rather than
// by this read (see its comment for why that coupling is refused). What it is not
// outside is the confirmation term above — a release that keeps failing asks the
// same question an absence does, and pays out of the same budget (#135).
//
// Which is why this returns the records the response did not contain rather than
// confirming them itself: a budget shared with a set this read knows nothing about
// cannot be spent while walking this one (offerHeldConfirmations).
func (o *Orchestrator) sweepHeld(ctx context.Context, res sweepResult, cur snapshot) []string {
	if o.draining {
		// No new classification while draining: every verdict it could reach is
		// a release, releaseHeld refuses those, and the reads it would spend to
		// get there are requests a departing daemon has no use for (§8.5). No
		// absences either, therefore — the only confirmation a drain may spend is
		// the one that completes a release it already ordered (#135).
		return nil
	}
	if res.err != nil {
		// Refresh failure → keep everything; retry next tick. Settled releases are
		// unaffected: retryHeldReleases drives those and it does not run from here,
		// and the confirmation that can resolve an unlandable one is offered
		// whether or not this read returned (offerHeldConfirmations).
		if !o.logCredentialFailure("held-claim sweep failed; retrying next tick", "", res.err) {
			o.log.Warn("held-claim sweep failed; retrying next tick", "error", res.err)
		}
		return nil
	}

	seen := res.byID()
	var absent []string
	for id, h := range o.held {
		_, present := seen[id]
		switch {
		case h.releasing:
			// A settled verdict is never re-derived: this read may no longer
			// say what the one that produced the verdict said, and starting a
			// history read against a record already on its way out would
			// spend a request to answer a question nobody is asking.
			//
			// Absent or present, therefore: the only question such a record has
			// left is whether the release it owes can ever land, which this
			// response cannot answer either way. offerHeldConfirmations owns it
			// (#135), and that is why a releasing record never reaches the
			// absence branch below — the two candidate sets are disjoint here.
		case h.inFlight:
			// A release, a history read or a confirming Get is out. Its result
			// decides; starting a second every tick is the O(held) cost this
			// sweep exists to avoid.
		case !present:
			// An assignee-filtered list cannot separate "the principal was
			// unassigned" from consistency lag, and absence of a fact is never
			// evidence (SPEC §9.10). Collected rather than confirmed here: the
			// budget is a decision about the whole set, so it cannot be spent
			// while walking it.
			//
			// By identifier, which is what the held set is keyed by and what every
			// rule here is threaded — rather than by record, whose cached issue
			// would be a second answer to the same question.
			absent = append(absent, id)
		default:
			// Free — the rules below read the response and spend no request — so
			// every record the response contains is classified every tick,
			// whatever the budget is doing.
			o.classifyHeld(ctx, id, h, seen[id], cur)
		}
	}
	return absent
}

// offerHeldConfirmations spends the tick's confirmation budget over every held
// claim that owes a confirming `Get`, in a rotation that gives each of them a turn
// within len(candidates) ticks.
//
// **Two candidate sets, one budget.** `absent` is what this tick's sweep read did
// not return. The second is the settled releases whose write keeps failing (#135):
// no read produces those and no read gates them, so they are collected here rather
// than passed in. They are disjoint by construction — sweepHeld classifies a
// releasing record under no other rule — so the rotation sees each identifier at
// most once, neither set can take the one slot twice in a tick, and neither can
// starve the other.
//
// Rotating on the *offer* rather than on the outcome is the whole point — a claim
// that keeps failing to confirm is precisely the case that must not hold the slot.
// See rotate for the rest of the reasoning; it is shared with the parked and
// owed-write budgets.
//
// **Not gated on the drain**, unlike sweepHeld above and offerOwedConfirmations. A
// confirmation for a release ordered *before* shutdown completes an effect the loop
// already ordered rather than initiating a new one (SPEC §9.8 as amended), and it
// is the only thing that can resolve one the tracker will never accept: `drained`
// waits on a releasing claim, so without it an issue deleted after its release was
// ordered holds the drain open for as long as the supervisor allows. It costs a
// departing daemon nothing it does not owe (§8.5) — a draining sweep classifies
// nothing and returns no absences, so this set is the only one there is.
func (o *Orchestrator) offerHeldConfirmations(ctx context.Context, absent []string) {
	candidates := make([]string, 0, len(absent)+len(o.held))
	candidates = append(candidates, absent...)
	for id, h := range o.held {
		// releaseFailed, not releasing: the tracker has refused this write at
		// least once and cannot say why, since a write's own error never
		// classifies (#134). The write must be completely out of flight before a
		// read that may retire it starts: dropping the record cannot cancel a
		// release already accepted by the serial effect queue.
		if h.releasing && h.releaseFailed && !h.inFlight {
			candidates = append(candidates, id)
		}
	}
	picked, cursor := rotate(candidates, sameIdentifier, o.heldCursor, heldConfirmationsPerTick)
	o.heldCursor = cursor
	for _, id := range picked {
		// Present by construction: the identifiers were read off o.held on this
		// same loop turn, and nothing between there and here can drop one.
		o.confirmHeldClaim(ctx, id, o.held[id])
	}
	if deferred := len(candidates) - len(picked); deferred > 0 {
		// Not a failure: the claims stay retained, their releases keep retrying,
		// and each later tick takes the next ones. Reported because a silent cap
		// reads as "covered everything" (SPEC §8.5's accounting is the same idea).
		o.log.Info("retained claims await confirmation on later ticks",
			"confirming", len(picked), "deferred", deferred, "cursor", o.heldCursor)
	}
}

// classifyHeld decides a held claim from an issue the response contains. Absence
// is handled by the caller, which owns the request budget it costs.
func (o *Orchestrator) classifyHeld(ctx context.Context, id string, h *heldClaim, fresh core.Issue, cur snapshot) {
	switch {
	case !o.active(cur.Definition, fresh):
		// The merge path and every manual close end here, on the list
		// response alone.
		o.releaseHeld(ctx, id, h, "issue went terminal")
	case !o.hasRequiredLabels(cur.Definition, fresh):
		// Mirrors the unroutable rule and §9.10 gate 4: the issue has left
		// the workflow, so the claim has no standing.
		o.releaseHeld(ctx, id, h, "issue left the workflow's label partition")
	case fresh.Revision != h.revision:
		// A close that a reopen has undone survives only in the log. The
		// revision triggers the read and never decides; the event decides.
		o.checkHeldHistory(ctx, id, h, fresh)
	default:
		h.issue = fresh
	}
}

// retryHeldReleases re-drives every settled release independently of the sweep.
//
// A release that has been decided is an owed write like any other, and the
// list read is not its precondition — gating the retry on that read would
// couple an already-decided write to an unrelated request, so a tracker
// failing the list call would leave settled claims standing for as long as it
// kept failing.
//
// Retried, and never abandoned on its own failure: what a refused write proves is
// nothing (#134). The claim leaves only on evidence — the tracker accepting the
// release, or a confirming `Get` saying there is no claim of ours left to release
// (offerHeldConfirmations).
//
// A tick that offers that Get does not also enqueue the release. If the answer is
// inconclusive, onHeldConfirmed re-drives the write immediately. This ordering is
// the liveness that a shared in-flight slot needs and the safety that two slots
// could not provide: a confirmation may forget the record, and forgetting cannot
// retract a release already waiting on the serial effect queue.
func (o *Orchestrator) retryHeldReleases(ctx context.Context) {
	for id, h := range o.held {
		if h.releasing {
			o.driveHeldRelease(ctx, id, h)
		}
	}
}

// releaseHeld settles the verdict; the entry lives until the tracker confirms
// it. Dropping first would lose the retry — a 503 or a refused enqueue would
// leave the claim standing with nothing tracking it, and a stranded claim
// blocks the issue for everyone including us (SPEC §8.3).
func (o *Orchestrator) releaseHeld(ctx context.Context, id string, h *heldClaim, why string) {
	if h.releasing {
		return
	}
	if o.draining {
		// Shutdown initiates no new release (SPEC §9.8 as amended), and a held
		// claim is where that is easiest to miss: it owns no process and no
		// workspace, so nothing about it looks like work in flight — but a
		// history read gated behind a slow tracker can land mid-drain and reach
		// a verdict, and this is the one place every such verdict passes
		// through. The evidence outlives the process: §9.10 rebuilds the held
		// set from ClaimedByPrincipal and reaches the same conclusion at the
		// next start.
		o.log.Info("not releasing a retained claim during shutdown; recovery will re-derive it",
			"issue", id, "reason", why)
		return
	}
	h.releasing = true
	h.why = why
	o.log.Info("releasing the retained claim", "issue", id, "reason", why)
	o.driveHeldRelease(ctx, id, h)
}

func (o *Orchestrator) driveHeldRelease(ctx context.Context, id string, h *heldClaim) {
	if h.inFlight {
		return
	}
	// On the serial effect queue, like every other tracker write: a release
	// that overtook this issue's last label write would leave the projection
	// disagreeing with the world §9.10 reads back.
	issue, token := h.issue, h.token
	tracker := o.bundle().Tracker
	h.inFlight = o.enqueue(ctx, func(ctx context.Context) {
		err := tracker.Release(ctx, issue)
		o.send(ctx, signal{kind: sigHeldReleased, issue: id, token: token, err: err})
	})
}

// confirmHeldClaim spends the one `Get` that answers what neither the sweep read
// nor a failed release write can: is there still a claim of ours on this issue?
//
// Absence from an assignee-filtered list cannot separate an unassignment from
// consistency lag, and a write's own 404 can equally mean a label, an assignee or
// a comment target the request named (#134). `Get` names the issue and nothing
// else, so its not-found means the issue (SPEC §9.8, core.ErrIssueNotFound).
//
// One read for both callers, deliberately. The two arrive at the same question
// from opposite directions — a claim missing from a list that should contain it,
// and a release the tracker will not take — and a second read shape for the second
// caller would be a second budget with it.
func (o *Orchestrator) confirmHeldClaim(ctx context.Context, id string, h *heldClaim) {
	issue, token := h.issue, h.token
	// One failure buys one confirmation. An inconclusive answer re-drives the
	// release, whose next failure sets this again; if enqueue itself is refused,
	// the next tick retries the write instead of spending Get after Get while the
	// effect queue is full.
	if h.releasing {
		h.releaseFailed = false
	}
	h.inFlight = true
	tracker := o.bundle().Tracker
	go func() {
		fresh, err := tracker.Get(ctx, issue.Identifier)
		o.send(ctx, signal{kind: sigHeldConfirmed, issue: id, token: token, refetched: fresh, err: err})
	}()
}

// checkHeldHistory spends one ClaimHistory read to ask whether the issue was
// closed inside this claim cycle — the close-then-reopen case that current
// state can no longer show.
//
// The cycle is re-derived from the same read rather than taken from the
// record. A human can unassign and reassign the principal between two sweeps,
// which starts a *new* claim cycle: the close that the old anchor admits
// belongs to the previous one, and §9.10 step 2 is explicit that earlier
// evidence "MUST NOT be read as current". Releasing on it would drop an
// assignment a human had just made and let the issue be dispatched a second
// time, over a PR that already exists.
func (o *Orchestrator) checkHeldHistory(ctx context.Context, id string, h *heldClaim, fresh core.Issue) {
	b := o.bundle()
	issue, token, principal := fresh, h.token, b.ClaimPrincipal
	h.inFlight = true
	go func() {
		events, err := b.Tracker.ClaimHistory(ctx, issue)
		anchor := claimCycleAnchor(events, principal)
		o.send(ctx, signal{
			kind: sigHeldHistory, issue: id, token: token, err: err,
			refetched: &issue, anchor: anchor, verified: closedInCycle(events, anchor),
		})
	}()
}

func (o *Orchestrator) onHeldReleased(_ context.Context, s signal) {
	id, h, ok := o.heldFor(s)
	if !ok {
		return
	}
	h.inFlight = false
	if s.err != nil {
		// Stays releasing, and the next tick tries again. The failure is also what
		// makes this claim a confirmation candidate: the write cannot say whether
		// there is still an issue, or a claim of ours on it, to release (#134,
		// #135). Latched rather than counted — one refusal is all it takes to make
		// the question worth asking, and the answer is what ends it.
		h.releaseFailed = true
		if !o.logCredentialFailure("releasing the retained claim; retrying next tick", id, s.err) {
			o.log.Error("releasing the retained claim; retrying next tick", "issue", id, "error", s.err)
		}
		return
	}
	o.log.Info("released the retained claim", "issue", id, "reason", h.why, "pr", h.prURL)
	o.dropHeld(id)
}

func (o *Orchestrator) onHeldConfirmed(ctx context.Context, s signal) {
	id, h, ok := o.heldFor(s)
	if !ok {
		return
	}
	h.inFlight = false
	// release_owed and reason name the settled release this read may have been
	// spent to resolve, so the two drops below say what became of it. Without
	// them the operator log ends on "retrying next tick" for a write that is
	// never attempted again.
	switch {
	case gone(refreshResult{issue: s.refetched, err: s.err}):
		// Deleted or transferred: there is nothing left to release.
		o.log.Info("dropping the retained claim: the issue is gone from the tracker",
			"issue", id, "release_owed", h.releasing, "reason", h.why)
		o.dropHeld(id)
		return
	case s.err != nil:
		// A read that failed is not an absence (SPEC §9.10). The claim stays, and
		// so does whatever it owes.
		if !o.logCredentialFailure("confirming the retained claim; retaining it", id, s.err) {
			o.log.Warn("confirming the retained claim; retaining it", "issue", id, "error", s.err)
		}
	case !containsFold(s.refetched.Assignees, o.bundle().ClaimPrincipal):
		// Someone took the assignment. Nothing of ours to release.
		o.log.Info("dropping the retained claim: the principal is no longer assigned",
			"issue", id, "release_owed", h.releasing, "reason", h.why)
		o.dropHeld(id)
		return
	case h.releasing:
		// Still ours. Nothing is re-derived and nothing re-baselined: the verdict
		// was settled before this read, and `why` is what it was settled with.
	default:
		// Still ours; the sweep read simply lagged. The revision is left
		// where it was on purpose — re-baselining it here would spend the
		// history read this record may still owe.
		h.issue = *s.refetched
	}
	if h.releasing {
		// The confirming read and the write are consecutive, never concurrent.
		// Re-drive here rather than waiting a tick: this is what lets them share
		// one slot without leaving the release idle after an inconclusive answer.
		o.driveHeldRelease(ctx, id, h)
	}
}

func (o *Orchestrator) onHeldHistory(ctx context.Context, s signal) {
	id, h, ok := o.heldFor(s)
	if !ok {
		return
	}
	h.inFlight = false
	if s.err != nil {
		// The revision still differs, so the next sweep asks again.
		if !o.logCredentialFailure("reading the retained claim's history; retrying next tick", id, s.err) {
			o.log.Warn("reading the retained claim's history; retrying next tick", "issue", id, "error", s.err)
		}
		return
	}
	if s.anchor == 0 {
		// The list read is assignee-filtered, so being on this path means the
		// issue *is* ours; a log that shows no assignment establishing it has
		// not caught up. No verdict, and no re-baseline of either the anchor
		// or the revision: the next sweep asks the same question again rather
		// than recording an answer nobody gave.
		o.log.Warn("held claim's history shows no assignment for an issue the sweep read returned as ours",
			"issue", id, "principal", o.bundle().ClaimPrincipal)
		return
	}
	if s.anchor != h.cycleAnchor {
		// A new claim cycle: unassigned and reassigned since the last read.
		o.log.Info("held claim re-anchored to a new claim cycle",
			"issue", id, "from", h.cycleAnchor, "to", s.anchor)
		h.cycleAnchor = s.anchor
	}
	if s.verified {
		o.releaseHeld(ctx, id, h, "issue was closed inside this claim cycle")
		return
	}
	// The bump was something else — a comment, a label, a re-assignment.
	// Re-baseline, so the same revision does not buy a second read.
	h.issue = *s.refetched
	h.revision = s.refetched.Revision
}

// heldFor resolves the record an async result belongs to. A result whose token
// has moved belongs to a previous held cycle and decides nothing.
func (o *Orchestrator) heldFor(s signal) (string, *heldClaim, bool) {
	h, ok := o.held[s.issue]
	if !ok || h.token != s.token {
		return "", nil, false
	}
	return s.issue, h, true
}

// dropHeld removes a held record, and with it the last thing tracking the
// issue. Only ever called on confirmed evidence: the tracker accepted the
// release, or said the claim is not ours to release.
func (o *Orchestrator) dropHeld(id string) {
	delete(o.held, id)
	o.mu.Lock()
	delete(o.published, id)
	o.heldCount = len(o.held)
	o.mu.Unlock()
}

func (o *Orchestrator) publishHeld() {
	n := len(o.held)
	o.mu.Lock()
	o.heldCount = n
	o.mu.Unlock()
}

// HeldCount reports how many retained claims are being swept, for
// `ben status` (SPEC §10.3).
func (o *Orchestrator) HeldCount() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.heldCount
}

// claimCycleAnchor is the change-log id of the claim-establishing event:
// SPEC §9.10 step 2's "**most recent** `assigned` event naming the principal
// with no later `unassigned` for it". Zero means the log shows no live
// assignment of ours — which is the absence of a fact, never a verdict on its
// own (§9.10); callers confirm it against current assignment.
//
// Most recent, not earliest. The two differ when the principal is assigned
// twice without an intervening unassignment, and the earlier one would widen
// the cycle to admit closes that precede the assignment now standing.
// ClaimHistory returns the log already ordered by (At, ID), so the last
// surviving assignment is the standing one.
func claimCycleAnchor(events []core.ClaimEvent, principal string) int64 {
	var anchor int64
	for _, ev := range events {
		if !equalFold(ev.Subject, principal) {
			continue
		}
		switch ev.Kind {
		case core.ClaimEventAssigned:
			anchor = ev.ID
		case core.ClaimEventUnassigned:
			anchor = 0
		}
	}
	return anchor
}

// closedInCycle reports whether the log carries a close inside the claim
// cycle the anchor names. A close from an earlier cycle says nothing about
// this one, which is why the anchor has to be real: anchor <= 0 would accept
// every close ever recorded on the issue.
func closedInCycle(events []core.ClaimEvent, anchor int64) bool {
	if anchor <= 0 {
		return false
	}
	for _, ev := range events {
		if ev.Kind == core.ClaimEventClosed && ev.ID >= anchor {
			return true
		}
	}
	return false
}

// hasRequiredLabels reports whether the issue is still inside the workflow's
// label partition. It is the half of routable() that applies to a held claim:
// the other half — the assignee check — is what the assignee-filtered sweep
// read already answers by including the issue at all.
func (o *Orchestrator) hasRequiredLabels(def *config.WorkflowDefinition, issue core.Issue) bool {
	for _, want := range def.Config.Tracker.RequiredLabels {
		if !containsFold(issue.Labels, want) {
			return false
		}
	}
	return true
}
