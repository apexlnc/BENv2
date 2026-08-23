package orchestrator

import (
	"context"
	"sort"
)

// Learning that an issue is gone from a write that cannot land.
//
// #134 settled the half that says what a **write** may conclude: nothing. A
// write's 404 can mean the issue, and it can equally mean a label, an assignee or
// a comment target the request named — so classifying one would drop a claim that
// may still be standing. Absence is stated by a *read*, and by the one read whose
// not-found means one thing: `Get` (SPEC §9.8, core.ErrIssueNotFound).
//
// This file is the other half: a caller that needs the answer after a failed
// write asks. Without it a tracker write for a deleted issue is retried from the
// head of the owed queue every tick, forever, and nothing else ever revisits the
// record — §9.8's refresh set is locked to running-ish states, its sweep to
// parked ones, and `reconcile` skips any record already on an ordered exit. The
// measured shape is a claim that lost the deletion race between the candidate
// read and the write (#142): the unwinding release 404s every tick and the
// record holds a §9.5 concurrency slot for the life of the process.
//
// The verdict it reaches routes through the machinery that already exists rather
// than a second exit of its own: `markGone` makes the record an ordered exit and
// lets onEffectDone discard the writes that can never land, and `stopAndFinish`
// is the same exit `reconcile` and the parked sweep take for the same fact.

// owedConfirmationsPerTick bounds what the confirmation may cost, and it is the
// number §9.8's parked rules already settled on, for the same argument.
//
// A failing write is one request per tick per record — that is the cost §8.5
// bounds — and a confirmation beside every failure doubles it exactly when the
// tracker is least able to serve it. Failures also arrive together: a tracker
// refusing writes refuses them for every record at once, so "one per failure" is
// O(records) on the tick that notices. Nothing here is urgent — the record is
// stuck either way — so K of them resolving over K ticks costs nothing that
// matters, and any constant above one is a number nobody can defend.
const owedConfirmationsPerTick = 1

// offerOwedConfirmations spends the tick's budget over the records whose
// head-of-line tracker write failed.
func (o *Orchestrator) offerOwedConfirmations(ctx context.Context) {
	if o.draining {
		// A departing daemon spends no request it does not owe (SPEC §8.5), and
		// there is nothing for this one to change: shutdown lands the writes it can
		// and §9.10 re-derives every verdict from the tracker at the next start.
		// Same rule, and the same reason, as the held sweep's refusal to classify.
		return
	}
	var candidates []*Record
	for _, r := range o.records {
		// gone is already the answer; absenceInFlight is this record's one Get,
		// which the parked sweep may equally have out (see Record.absenceInFlight).
		// The queue is non-empty by construction wherever the flag is set — it names
		// the head — and stated here because confirmOwedAbsence reads that head.
		if r.owedWriteFailed && len(r.owed) > 0 && !r.gone && !r.absenceInFlight {
			candidates = append(candidates, r)
		}
	}
	picked, cursor := rotate(candidates, recordIdentifier, o.owedCursor, owedConfirmationsPerTick)
	o.owedCursor = cursor
	for _, r := range picked {
		o.confirmOwedAbsence(ctx, r)
	}
	if deferred := len(candidates) - len(picked); deferred > 0 {
		// Not a failure: the writes keep retrying and each later tick takes the
		// next records. Reported because a silent cap reads as "covered everything"
		// (SPEC §8.5's accounting is the same idea).
		o.log.Info("records with a failing tracker write await an absence confirmation on later ticks",
			"confirming", len(picked), "deferred", deferred, "cursor", o.owedCursor)
	}
}

// confirmOwedAbsence spends the one `Get` that can answer what the failed write
// could not.
func (o *Orchestrator) confirmOwedAbsence(ctx context.Context, r *Record) {
	id, token := r.Issue.Identifier, r.token
	// The write this read was issued for, carried so the operator log can name it
	// — and read here rather than on the way back, where the head may already have
	// been retired.
	what := r.owed[0].what
	r.absenceInFlight = true
	tracker := o.bundle().Tracker
	go func() {
		fresh, err := tracker.Get(ctx, id)
		o.send(ctx, signal{
			kind: sigOwedConfirmed, issue: id, token: token,
			refetched: fresh, err: err, effect: what,
		})
	}()
}

func (o *Orchestrator) onOwedConfirmed(ctx context.Context, r *Record, s signal) {
	r.absenceInFlight = false
	id := r.Issue.Identifier
	switch {
	case r.gone:
		// Reconciliation or the parked sweep reached the same verdict while this
		// read was out. Whatever it ordered owns the record.
	case !r.owedWriteFailed:
		// The write landed while the read was out, so the question this read was
		// asked has answered itself: a write the tracker accepted is a write it had
		// an issue for. Acting on the response now would tear a record down on a
		// stale read, and if anything is still wedged the next tick offers again.
	case gone(refreshResult{issue: s.refetched, err: s.err}):
		o.log.Info("a tracker write is failing on an issue the tracker no longer has",
			"issue", id, "effect", s.effect)
		r.markGone()
		// The exit reconciliation and the parked sweep take for this fact, and for
		// the same reasons: a run that may still be going is stopped first, the
		// workspace is disposed, and the forget it owes in place of a release is
		// what finally drops the record — after the effects queued ahead of it, so
		// the local half still runs (owed.go).
		o.stopAndFinish(ctx, r, false, "issue is gone from the tracker")
		// The head is not in flight — it failed, and stayed there — so the discard
		// runs now rather than a tick later. It costs the one attempt the next tick
		// would have made anyway.
		o.driveOwed(ctx, r)
	case s.err != nil:
		// Could not ask. The write keeps retrying and so does this, exactly as
		// before: a read that failed is not an absence (SPEC §9.10).
		if !o.logCredentialFailure("confirming whether a failing tracker write's issue still exists; retrying next tick",
			id, s.err, "effect", s.effect) {
			o.log.Warn("confirming whether a failing tracker write's issue still exists; retrying next tick",
				"issue", id, "error", s.err)
		}
	default:
		// The issue is there, so the write is failing for some other reason and is
		// retried from the head as it was. Deliberately nothing else: this read was
		// issued for one question, and the record's routing is decided by §9.8's
		// refresh and sweep against responses scoped to *their* rules.
	}
}

// rotate picks up to n candidates from a set, starting just past the cursor and
// wrapping, and returns the cursor to carry into the next tick.
//
// A budget this small makes **fairness part of the bound**, and it is an explicit
// rotation rather than a property of the offer order. Map iteration order is
// *unspecified*, not guaranteed random: it is randomized by the current runtime,
// but nothing in the language promises it, and a caller may hand this a stable
// order for its own reasons. Under any stable order the first candidate whose
// confirmation keeps failing retakes the only slot every tick and every other one
// starves indefinitely — a claim that never resolves, on evidence nobody ever
// reads.
//
// The cursor advances on every pick whether or not the read it starts succeeds,
// which is the whole point. It is an identifier rather than an index because the
// set changes between ticks — a cursor naming a record that has since been
// dropped still positions correctly.
//
// Shared by all three confirmation budgets — this one, §9.8's parked absences
// (offerParkedConfirmations, parked.go) and its held ones
// (offerHeldConfirmations, held.go) — because it is one rule: the same
// starvation, reached the same way. Generic over the candidate rather than over
// `*Record` for the third of them: a held claim is not a record, and the sets are
// keyed the same way regardless (SPEC §9.8).
func rotate[T any](items []T, identifier func(T) string, cursor string, n int) ([]T, string) {
	if len(items) == 0 || n <= 0 {
		return nil, cursor
	}
	sort.Slice(items, func(i, j int) bool {
		return identifier(items[i]) < identifier(items[j])
	})
	// The first candidate after the cursor, wrapping.
	start := 0
	for i, it := range items {
		if identifier(it) > cursor {
			start = i
			break
		}
	}
	picked := make([]T, 0, min(n, len(items)))
	for i := range min(n, len(items)) {
		it := items[(start+i)%len(items)]
		cursor = identifier(it)
		picked = append(picked, it)
	}
	return picked, cursor
}

// The identifier projections rotate is used with. A record carries its issue; a
// held claim is offered by the key its set is stored under, which already is one
// — read from the map rather than from the claim's cached issue, so that the two
// cannot come to disagree about which issue a confirmation is for.
func recordIdentifier(r *Record) string { return r.Issue.Identifier }
func sameIdentifier(id string) string   { return id }
