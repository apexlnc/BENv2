package orchestrator

import "context"

// Tracker writes are part of the state machine, not decoration on it.
// SPEC §9.10 reconstructs the world by reading them back — and its table
// turns on *which* label is standing, distinguishing ben:failed from
// ben:needs-review from ben:claimed/running from none. A write that is
// attempted once and logged on failure leaves a record whose tracker-visible
// shape disagrees with its local state, which is precisely the disagreement
// recovery cannot detect.
//
// So every write a record owes lives on the record until the tracker accepts
// it: ordered, retried each tick, and never dropped. Ordering is head-of-line
// per record, which is also what B04's milestone markers need — each anchors
// on the label transition that precedes it.
//
// This replaces a flag per write. Three review rounds added one each
// (projecting, releaseInFlight, afterRunFired…), and every one of them was a
// different answer to the same question.
type owedEffect struct {
	// what names the effect for logs, and gates the transitions that wait on
	// one (see afterEffect).
	what string
	// target says where the effect lands, which is the whole of what a vanished
	// issue changes about it. A required argument rather than a field with a
	// default, so the choice is made at every site; the two behaviours it selects
	// are pinned by TestAPendingTrackerWriteDoesNotStrandAGoneIssue (discarded)
	// and TestAGoneIssueStillRetriesItsDisposal (kept and retried).
	target effectTarget
	// projection marks the one kind of write that makes the tracker's *label set*
	// disagree with this record's state (SPEC §9.3). §9.8's parked label rule is
	// gated on it: while one is owed, a poll response carries the labels from
	// before the transition, and the rule would read BEN's own unlanded write as a
	// human's gesture.
	//
	// A marked field rather than `len(r.owed) > 0`, because the queue holds
	// milestone comments and local effects too and none of them touch a label. The
	// difference is not cosmetic: comments and disposals can fail indefinitely, and
	// a record whose comment is wedged would otherwise never see a human's
	// re-queue at all.
	projection bool
	do         func(context.Context, *Orchestrator) error
}

// effectTarget distinguishes the two kinds of thing a record can owe.
type effectTarget uint8

const (
	// effectTracker writes to the tracker: a state label, a milestone comment,
	// the release. Retried until it lands, because §9.10 reconstructs the world
	// from what BEN projected and a dropped write leaves recovery reading a state
	// this daemon never reached.
	//
	// Impossible for an issue the tracker no longer has, and that is the one case
	// where retrying is wrong rather than careful: every attempt is a 404, the
	// head never retires, and everything queued behind it — the disposal, the
	// forget — never runs. onEffectDone discards those instead.
	//
	// Discarded as each reaches the head, rather than swept from the queue when
	// absence is learned. A sweep would save one 404 per queued write, which is
	// worth nothing against what it risks: it has to tell these from the local
	// effects beside them, and a sweep that got that wrong would silently drop a
	// disposal or a §6.5 hook — a leaked worktree, with no failed effect to say
	// so. One rule, applied where the answer already is.
	effectTracker effectTarget = iota
	// effectLocal touches only this host: a workspace, a §6.5 hook, the record
	// set. An issue's absence from the tracker says nothing about any of them, so
	// they still have to land.
	effectLocal
)

// Effect names that gate a transition or an exit.
const (
	effectClaimLabel       = "project ben:claimed"
	effectAbandonClaimBase = "abandon pending claim base"
	effectRelease          = "release claim"
	// effectForget is the exit for a record with no claim left to drop: the
	// tracker positively says the issue is gone, or that the principal it was
	// assigned to is no longer us. It writes nothing, and exists only to take the
	// release's place in the owed queue, so the record is forgotten after the
	// effects ahead of it land rather than instead of them.
	effectForget = "forget the gone issue"
)

// owe appends a write the record must land, and starts it if the record is
// idle.
func (o *Orchestrator) owe(ctx context.Context, r *Record, what string, target effectTarget, do func(context.Context, *Orchestrator) error) {
	o.oweEffect(ctx, r, owedEffect{what: what, target: target, do: do})
}

// oweProjection is owe for the state-label write, which is the one effect §9.8's
// parked label rule has to know about. A separate entry point rather than a
// parameter on owe, so the mark cannot be set by accident: a `ben:*` label reaches
// the tracker from here or not at all.
//
// Two callers, each the single point of its own kind. transitionCaused writes the
// label a §9.2 edge implies; projectRecovery writes the one a §9.10 verdict repairs,
// and cannot go through transitionCaused because recovery is not taking an edge — it
// restores the state the tracker already shows (adoptRecovered). A third is a
// decision rather than a detail, and TestEveryStateLabelWriteIsOwedAsAProjection is
// where it has to be made.
func (o *Orchestrator) oweProjection(ctx context.Context, r *Record, what string, do func(context.Context, *Orchestrator) error) {
	o.oweEffect(ctx, r, owedEffect{what: what, target: effectTracker, projection: true, do: do})
}

func (o *Orchestrator) oweEffect(ctx context.Context, r *Record, eff owedEffect) {
	r.owed = append(r.owed, eff)
	o.driveOwed(ctx, r)
}

// driveOwed starts the head of the record's queue if nothing is in flight.
// A queue that refuses the work leaves the effect where it is; the next tick
// tries again.
func (o *Orchestrator) driveOwed(ctx context.Context, r *Record) {
	if r.owedInFlight || len(r.owed) == 0 {
		return
	}
	eff := r.owed[0]
	if isExit(eff.what) && o.endedCycleOwed(r.Issue.Identifier) {
		// The one effect this record may not land yet, in *either* of its forms.
		// The release gives up the tracker claim, which is what makes the
		// obligation findable after a restart — §9.10 step 1 enumerates the claim
		// and recovery re-derives the same verdict from the same labels. The forget
		// gives up the record itself, which for a gone or lost claim is worse:
		// there is no claim to re-derive from, so the record is the last thing that
		// knows a sandbox needs releasing, and `drained` would have nothing left to
		// wait on (#252, cycle.go).
		//
		// Held here rather than refused inside the effect: `do` runs on the effect
		// queue's goroutine and the obligation set is loop-owned, so the question
		// can only be asked from here. Re-driven every tick by retryPendingExits and
		// immediately by clearEndedCycle, so nothing waits longer than the disposal.
		return
	}
	// Keyed to the record rather than the attempt: an owed write belongs to
	// the record, and the record can be forgotten while its completion is
	// still in flight.
	id, token := r.Issue.Identifier, r.token
	r.owedInFlight = o.enqueue(ctx, func(ctx context.Context) {
		err := eff.do(ctx, o)
		o.send(ctx, signal{kind: sigEffectDone, issue: id, token: token, err: err, effect: eff.what})
	})
}

func (o *Orchestrator) onEffectDone(ctx context.Context, r *Record, s signal) {
	r.owedInFlight = false
	if s.err != nil {
		if r.gone && len(r.owed) > 0 && r.owed[0].target == effectTracker {
			// The issue is gone, so this write can never land and retrying it
			// blocks the cleanup behind it forever. Discarding is not abandoning a
			// write that matters: there is no issue left for recovery to read a
			// projection from.
			o.log.Info("discarding a tracker write for an issue that is gone",
				"issue", r.Issue.Identifier, "effect", s.effect, "error", s.err)
			r.owed = r.owed[1:]
			r.owedWriteFailed = false
			o.driveOwed(ctx, r)
			return
		}
		// Stays at the head. Retried next tick rather than abandoned: the
		// tracker's version of this record is what recovery will read.
		//
		// A *tracker* write that stays is also the record's one reason to doubt the
		// issue still exists — the failure cannot say so itself (#134), so it is
		// recorded as a question for the confirming Get to answer (absence.go).
		// Nothing else will ask: a record whose exit is already ordered is skipped
		// by reconcile, and `queued` and `done` are outside §9.8's read sets
		// entirely.
		if len(r.owed) > 0 && r.owed[0].target == effectTracker {
			r.owedWriteFailed = true
		}
		// An owed write **stays owed across every credential class**, and this
		// is where that is decided by *not* deciding it: nothing above consults
		// the class. A permanent credential error must not discard an owed
		// write, because the claim it protects is still standing — dropping it
		// leaves assigned-with-no-state-label, which §9.10 step 3 never
		// revisits (SPEC §9.8, amendment 14). The class is read for severity
		// only, on the line below.
		if !o.logCredentialFailure("tracker write failed on a credential failure; the write stays owed and is retried next tick",
			r.Issue.Identifier, s.err) {
			o.log.Error("tracker write failed; retrying next tick",
				"issue", r.Issue.Identifier, "effect", s.effect, "error", s.err)
		}
		return
	}
	if len(r.owed) > 0 {
		r.owed = r.owed[1:]
	}
	// The head that failed has landed and left, so whatever it was is no longer
	// evidence about the issue's existence.
	r.owedWriteFailed = false
	o.afterEffect(ctx, r, s.effect)
	o.driveOwed(ctx, r)
	// A `done` record converts to a held claim the moment its last owed write
	// lands, rather than a tick later (see held.go). No-op for every other
	// state, and for a record afterEffect has just forgotten.
	o.driveHold(ctx, r)
}

// afterEffect advances the machine for the writes something was waiting on.
func (o *Orchestrator) afterEffect(ctx context.Context, r *Record, what string) {
	switch what {
	case effectClaimLabel:
		o.onClaimProjected(ctx, r)
	case effectRelease:
		o.log.Info("released claim", "issue", r.Issue.Identifier, "reason", r.stopReason)
		o.forget(r.Issue.Identifier)
	case effectForget:
		o.log.Info("dropping the record; there was no claim of ours left to release",
			"issue", r.Issue.Identifier, "reason", r.stopReason)
		o.forget(r.Issue.Identifier)
	}
}

// owesAnything reports whether the record still has writes the tracker has
// not accepted. A record with unlanded writes may not be forgotten.
func (r *Record) owesAnything() bool { return len(r.owed) > 0 }

// isExit reports whether an effect is the one that drops the record — the
// release, or the forget that stands in for it when there is no claim left to
// give up. Two spellings of one thing, so anything ordering against "the record
// leaves" asks here rather than naming one of them and missing the other.
func isExit(what string) bool { return what == effectRelease || what == effectForget }

// owesBeforeExit reports whether this record still owes something *ahead* of the
// exit that drops it.
//
// It is what an ended workspace cycle's disposal waits on (cycle.go). The queue
// ahead of an exit is where the §6.5 after_run hook and the claim-base abandon
// sit, and both touch the workspace the disposal is about to delete — head-of-line
// ordering used to sequence them because the disposal was on this queue too, and
// once it moved off, a delete could retire the cycle out from under a hook that
// had not run yet (remotews.AfterRun then skips it silently on ErrNoCycle).
//
// Deliberately not "owes anything": the exit itself is held *behind* the
// disposal, so waiting for an empty queue would be each waiting on the other.
func (r *Record) owesBeforeExit() bool {
	return len(r.owed) > 0 && !isExit(r.owed[0].what)
}

// owesForget reports whether the exit that drops this record is already queued,
// so a second finish does not append another (see finishNow).
func (r *Record) owesForget() bool {
	for _, eff := range r.owed {
		if eff.what == effectForget {
			return true
		}
	}
	return false
}

// owesProjection reports whether the tracker has not yet accepted a `ben:*` label
// write for this record — so its label set, however freshly read, may describe a
// state this record has already left (SPEC §9.3, §9.8).
//
// Scanning the queue rather than keeping a counter: head-of-line ordering means an
// effect ahead of the projection can hold it back, and the queue is the one place
// that fact lives.
func (r *Record) owesProjection() bool {
	for _, eff := range r.owed {
		if eff.projection {
			return true
		}
	}
	return false
}
