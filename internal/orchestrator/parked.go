package orchestrator

import (
	"context"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The parked half of the §9.8 sweep.
//
// A parked record waits for a human, so the set of them grows with review
// latency — the one quantity the daemon does not control, and the same shape
// that ruled out a `Get` per held claim (#27, BUILD decision 14). The two
// verdicts §9.8 reaches for a parked record are terminal state and a state
// label a human removed, and the sweep read answers both: `HeldClaims` is the
// principal's assignments in any state with any labels (§8.2), and a parked
// issue is assigned and labelled, so it is in that response with the two facts
// on it.
//
// What the fold buys is not only requests. The per-issue refreshes run
// sequentially in one worker (beginReconcile), so N parked records put N round
// trips ahead of every *running* record's refresh in the same pass, and the
// ticks that fire while the pass is out are dropped. A review backlog therefore
// slowed down the reconciliation of live runs.
//
// Held claims and parked records are swept separately, off one read, because
// they are different things with different owners: a held claim has no record,
// no workspace and nothing to stop, and its rules reach for history; a parked
// record is a run the machine still owns and its rules move it back into the
// state machine.

// parkedWant is one parked record the sweep read was issued for.
type parkedWant struct {
	id string
	// labelsSettled says the record owed no `ben:*` projection when the read was
	// issued, so a label set the response carries cannot be one from before this
	// record's own state-label write.
	//
	// The projection specifically, not `owesAnything`: the owed queue also holds
	// milestone comments and local effects, and a comment that keeps failing would
	// otherwise suppress every human re-queue for the life of the record.
	//
	// It gates the unpark rule and nothing else, because the unpark rule is the
	// only one that reads labels. Neither terminal state nor the assignment is a
	// fact BEN's own writes can move, so a record whose projection is stuck
	// retrying is still classified for both — which is what notices that its issue
	// was deleted while the write it owes can never land.
	labelsSettled bool
}

// parkedConfirmationsPerTick bounds what absences may cost, and it is the whole
// of §9.8's per-tick bound on the parked set: the sweep read is one request
// however many records are parked, and the confirming `Get` is the only other
// request the parked rules can spend.
//
// One, because "the same number of requests as one parked record" is the bound the
// ticket asks for, and any constant above one is a number nobody can defend. K
// absent records therefore resolve over K ticks rather than in one, which costs
// nothing that matters: an absence is either an issue that is no longer ours or one
// that no longer exists, and neither is urgent — while the alternative is a mass
// unassignment turning into K concurrent reads on the tick that notices it.
//
// A budget this small makes **fairness part of the bound**, and it is an explicit
// rotation (see rotate) rather than a property of the offer order — the same
// starvation the held and owed-write confirmation budgets have to avoid, so all
// three spend theirs the same way.
const parkedConfirmationsPerTick = 1

// sweepParked applies §9.8's parked rules to one HeldClaims read.
//
// Everything the snapshot said is re-checked here, because the read was out for
// a while: only the loop's own state at this instant says whether a verdict may
// still be reached.
func (o *Orchestrator) sweepParked(ctx context.Context, wants []parkedWant, res sweepResult, cur snapshot) {
	if res.err != nil {
		// Refresh failure → keep everything; retry next tick (SPEC §9.8).
		// Logged by sweepHeld, which reads the same failed response.
		return
	}
	seen := res.byID()
	var absent []*Record
	for _, w := range wants {
		r, ok := o.records[w.id]
		if !ok || r.exiting() || r.State != StateNeedsReview || r.absenceInFlight {
			// Forgotten, unparked, taken over by an exit or a drain, or already
			// waiting on the Get that decides. exiting() is what keeps a shutdown
			// from acting on a re-queue (SPEC §9.8 as amended, §11).
			continue
		}
		if _, present := seen[w.id]; !present {
			// An assignee-filtered list cannot separate "the principal was
			// unassigned" from consistency lag, and absence of a fact is never
			// evidence (SPEC §9.10). Collected rather than confirmed here: the
			// budget is a decision about the whole set, so it cannot be spent while
			// walking it.
			absent = append(absent, r)
			continue
		}
		// Free — the rules below read the response and spend no request — so every
		// present record is classified every tick, whatever the budget is doing.
		o.classifyParked(ctx, r, w, seen, cur)
	}
	o.offerParkedConfirmations(ctx, absent)
}

// offerParkedConfirmations spends the tick's confirmation budget over the absent
// set, in a rotation that gives every record a turn within len(absent) ticks.
//
// Rotating on the *offer* rather than on the outcome is the whole point — an
// absence that keeps failing to confirm is precisely the case that must not hold
// the slot. See rotate for the rest of the reasoning; it is shared with the held
// and owed-write confirmation budgets.
func (o *Orchestrator) offerParkedConfirmations(ctx context.Context, absent []*Record) {
	picked, cursor := rotate(absent, recordIdentifier, o.parkedCursor, parkedConfirmationsPerTick)
	o.parkedCursor = cursor
	for _, r := range picked {
		o.confirmParkedAbsence(ctx, r)
	}
	if deferred := len(absent) - len(picked); deferred > 0 {
		// Not a failure: the records are parked, they stay parked, and each later
		// tick takes the next ones. Reported because a silent cap reads as "covered
		// everything" (SPEC §8.5's accounting is the same idea).
		o.log.Info("parked records absent from the sweep read await confirmation on later ticks",
			"confirming", len(picked), "deferred", deferred, "cursor", o.parkedCursor)
	}
}

// classifyParked decides a parked record from an issue the response contains.
// Absence is handled by the caller, which owns the request budget it costs.
func (o *Orchestrator) classifyParked(ctx context.Context, r *Record, w parkedWant, seen map[string]core.Issue, cur snapshot) {
	fresh := seen[r.Issue.Identifier]
	// Both halves of the label question, and both are necessary. The first says
	// the response is not older than this record's park; the second says no
	// projection has been ordered since the read went out — which a park, unpark
	// and re-park inside one tick would do, leaving the response's label set
	// describing a park that has already been undone.
	o.applyParked(ctx, r, fresh, cur, w.labelsSettled && !r.owesProjection())
}

// applyParked is §9.8's two parked rules, against an issue the tracker has
// positively returned.
//
// Both read a positive fact off that issue: its state, and its label set. The
// second is the distinction the absence rule turns on — `ben:needs-review`
// missing from a label set the tracker just stated is evidence a human removed
// it, while an issue missing from a filtered list states nothing about its
// labels at all.
//
// labelsUsable is the caller's answer to "can this response's label set be about
// this park?" — false leaves the record where it is rather than guessing.
func (o *Orchestrator) applyParked(ctx context.Context, r *Record, fresh core.Issue, cur snapshot, labelsUsable bool) {
	if !o.active(cur.Definition, fresh) {
		// Terminal first, as it is for a live record: a closed needs-review issue
		// has been resolved by a human, and its claim and workspace are no longer
		// doing anything useful.
		o.stopAndFinish(ctx, r, false, "issue went terminal")
		return
	}

	// Routing only, and the routability check is deliberately absent: a parked
	// record must survive the window in which a labeler has removed a required
	// label and not yet re-applied it, which is what reapproval looks like from
	// here and is two writes rather than one (SPEC §9.5).
	r.adoptRouting(fresh)
	if !labelsUsable || hasStateLabel(fresh) {
		return
	}
	// A human re-queues by removing the state label (SPEC §9.2, §9.8), and the
	// re-queue restores the run budgets: two of the three ways into a park are
	// exhausted bounds, so a park that kept them would re-park on the next
	// attempt.
	//
	// The unpark does not re-pin. It resumes a parked run and approves nothing
	// (§6.7), so a re-queue over drift nobody reapproved reaches the §9.6
	// re-fetch's check and parks again.
	r.restoreBudgets()
	if r.epochFaulted {
		// The gesture still restores budgets, but it cannot create the safety fact
		// this park says is absent. Re-project the same state without traversing
		// backoff, so no timer, hook, marker, or agent can be reached under this
		// assignment (SPEC §9.2, §9.8).
		r.UpdatedAt = o.clock.Now()
		o.publish(r)
		issue := r.Issue
		o.oweProjection(ctx, r, "re-project ben:needs-review for epoch fault", func(ctx context.Context, o *Orchestrator) error {
			return o.bundle().Tracker.SetStateLabels(ctx, issue, core.StateLabelNeedsReview)
		})
		o.comment(ctx, r, core.MilestoneComment{
			Milestone: core.MilestoneNeedsReview,
			Detail:    r.epochFaultDetail,
		})
		return
	}
	// No §7.3 cause: the unpark is a human gesture, not a failure. Passing
	// r.FailureReason here would record the failure that parked the run as the
	// cause of the human undoing it.
	o.enterBackoff(ctx, r, "human removed the state label; budgets restored", "")
}

// confirmParkedAbsence spends the one Get §9.8 requires before an absence may
// drive anything.
func (o *Orchestrator) confirmParkedAbsence(ctx context.Context, r *Record) {
	id, token, settled := r.Issue.Identifier, r.token, !r.owesProjection()
	r.absenceInFlight = true
	tracker := o.bundle().Tracker
	go func() {
		fresh, err := tracker.Get(ctx, id)
		o.send(ctx, signal{
			kind: sigParkedConfirmed, issue: id, token: token,
			refetched: fresh, err: err, labelsSettled: settled,
		})
	}()
}

func (o *Orchestrator) onParkedConfirmed(ctx context.Context, r *Record, s signal) {
	r.absenceInFlight = false
	if r.exiting() || r.State != StateNeedsReview {
		// The record moved while the read was out. Whatever it is doing now owns
		// it.
		return
	}
	// One record, one moment: the configuration in force now is what this verdict
	// is taken under (SPEC §5.4 gives reconciliation to the reload).
	cur := o.configNow()
	id := r.Issue.Identifier
	switch {
	case gone(refreshResult{issue: s.refetched, err: s.err}):
		// Deleted or transferred, stated by the adapter as a named error rather
		// than inferred from a failed read.
		r.markGone()
		o.stopAndFinish(ctx, r, false, "issue is gone from the tracker")
	case s.err != nil:
		// Could not ask. Keep everything and ask again next tick.
		if !o.logCredentialFailure("confirming a parked record's absence from the sweep; retrying next tick",
			id, s.err) {
			o.log.Warn("confirming a parked record's absence from the sweep; retrying next tick",
				"issue", id, "error", s.err)
		}
	case !containsFold(s.refetched.Assignees, cur.Runtime.ClaimPrincipal):
		// Positive evidence, from current state rather than from a filtered
		// list's silence: the principal is not assigned. The assignment *is* the
		// claim (SPEC §8.3), so there is nothing to release and nothing to keep
		// tracking. A restart reaches the same verdict by a different route:
		// §9.10 classifies from ClaimedByPrincipal, which would not return this
		// issue at all. The state label is left standing either way — clearing it
		// is what discards the recovery and attempt continuity §9.10 classifies
		// from (SPEC §9.8 as amended).
		//
		// Two independent facts, and this read carries both. Whether the claim is
		// ours decides whether a release is owed; whether the issue is **terminal**
		// decides the workspace, exactly as it does for a record still assigned to
		// us. Reading only the first is how a closed-and-unassigned issue — one
		// gesture by a human resolving it — kept a worktree that §9.8 disposes.
		keep := o.active(cur.Definition, *s.refetched)
		why := "the principal is no longer assigned"
		if !keep {
			why = "issue went terminal and the principal is no longer assigned"
		}
		r.markClaimLost()
		o.log.Info("dropping the parked record; nothing of ours left to release",
			"issue", id, "reason", why, "workspace_kept", keep)
		o.stopAndFinish(ctx, r, keep, why)
	default:
		// Still ours; the sweep read simply lagged. The same rules, decided from
		// the Get that proved it — a confirmation that reached the record and then
		// declined to use what it read would spend the request twice.
		o.applyParked(ctx, r, *s.refetched, cur, s.labelsSettled && !r.owesProjection())
	}
}
