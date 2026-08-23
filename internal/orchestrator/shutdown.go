package orchestrator

import (
	"context"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Graceful shutdown (SPEC §11, §9.8 as amended 2026-08-12).
//
// The rule is short and the whole design follows from it: **shutdown initiates
// no new release and no new terminal projection.** It stops dispatch, interrupts
// every in-flight run, waits for confirmed termination wherever a handle exists,
// and lands the effects already ordered. Whatever claim or label has durably
// landed stays standing, and §9.10 recovery resumes the work at the next start.
//
// What that rules out is worth stating, because both alternatives look
// reasonable from inside a single record. Releasing each confirmed stop strands
// the issue: a `ben:*` label excludes it from dispatch (BUILD.md decision 8), so
// an unclaimed labelled issue is one no daemon will pick up and only a human can
// clear. Clearing the label too discards what §9.10 classifies from — the
// ordered label events and the claim-establishing assignment — so the issue
// reads as never-claimed, `attempt` restarts at 1, and a re-dispatch at the
// pinned base can walk past commits that were never pushed.
//
// The waiting is not symmetrical with that restraint, and deliberately so. A
// possibly-live process is the one thing a restart cannot reason about (§9.8),
// so the drain does insist on a confirmed termination — and BEN does not bound
// how long that takes. The supervisor does, through TimeoutStopSec: a daemon
// that cannot confirm is a daemon whose agent survived SIGKILL, and inventing a
// deadline here would mean exiting while a process may still hold the worktree,
// which is the trade §9.8 refuses everywhere else.

// Shutdown stops dispatch and winds the loop down to quiescence.
//
// It returns when nothing is left that a restart could not reconstruct: every
// run's process group confirmed gone, and every tracker write the loop had
// already ordered landed. It does not stop the loop — the caller cancels the
// context afterwards, and not before, because the context passed to
// AgentRunner.Start descends from it and cancelling it is exactly the abandonment
// this method exists to prevent.
//
// Safe to call more than once and from any goroutine; every caller is released
// by the same drain.
func (o *Orchestrator) Shutdown(ctx context.Context) error {
	ack := make(chan struct{})
	o.send(ctx, signal{kind: sigShutdown, drained: ack})
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		// The caller gave up. The loop keeps draining — it has no way to undo an
		// interrupt already sent, and abandoning the wait must not also abandon
		// the processes it was waiting on.
		return ctx.Err()
	}
}

// Draining reports whether shutdown has begun. `ben status` renders it, and it
// is what tells an operator that an unmoving queue is a daemon on its way out
// rather than a daemon that is stuck.
func (o *Orchestrator) Draining() bool {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.drainingPublished
}

// onShutdown takes the loop over. It is one signal rather than a flag a caller
// sets, because "no new work from here" and "here is the set of work there is"
// have to be one step: read from another goroutine, a record claimed between the
// two would never be interrupted at all.
func (o *Orchestrator) onShutdown(ctx context.Context, s signal) {
	if s.drained != nil {
		o.drainWaiters = append(o.drainWaiters, s.drained)
	}
	if !o.draining {
		o.draining = true
		o.mu.Lock()
		o.drainingPublished = true
		o.mu.Unlock()
		o.log.Info("shutdown: dispatch stopped, interrupting in-flight runs",
			"runs", len(o.records), "held_claims", len(o.held))
	}
	o.driveShutdown(ctx)
}

// driveShutdown is the drain, re-run after every signal so it notices completion
// without a ticker of its own.
//
// Idempotent by construction: marking a record suspended twice is a no-op, the
// owed queue drives its own head, and beginStop owns the handle's single ladder
// slot. That is what lets it be called from the tick, from every result that
// lands, and from the signal that started it, without any of them needing to
// know about the others.
func (o *Orchestrator) driveShutdown(ctx context.Context) {
	if !o.draining {
		return
	}
	for _, r := range o.records {
		if r.orderedExit() {
			// An exit the loop decided on before the signal. Shutdown *completes*
			// these rather than replacing them (SPEC §9.8 as amended): the claim
			// was already being let go, and suspending the record instead would
			// strand it. retryPendingExits re-drives the stop and the release on
			// each tick; all the drain owes it is the owed-write pass below and
			// the patience to wait.
			o.driveOwed(ctx, r)
			continue
		}
		r.suspended = true
		if r.handle == nil && r.pending == 0 {
			// **The backstop that records a drained attempt's outcome (#60).** No
			// handle *and no worker still out* means nothing further happens to
			// this attempt in this process:
			// the drain routes nothing, and §9.10 resumes the *issue* as a new
			// attempt — which is the same reasoning finishSuspended already gives
			// for firing the §6.5 after-run hook there. §7.3's `killed` is what
			// happened to it, and the verdict is unknown because whatever it
			// reported was dropped rather than routed.
			//
			// Here because this site is *reachable in every case and provably
			// last*. Every route to a suspended record with no handle passes
			// through it: a run interrupted and reaped (finishSuspended clears the
			// handle, and this is deferred after the signal that did so), an
			// attempt suspended mid-Prepare that never had one, and a verdict
			// discarded in `verifying` whose handle was already gone. The drain
			// cannot complete without another pass through here, since drained()
			// is checked below.
			//
			// **It is the default reason, not the only one.** `killed` describes an
			// attempt the *drain* ended, and one route reaches this pass having
			// already decided otherwise: an attempt that ended on its own while the
			// §9.6 prior-attempt account was being read (endAttemptSuspended, #61).
			// That site states the reason it had — `crashed`, `budget_exceeded` —
			// and recordAttempt's once-only guard is what makes the pair
			// deterministic rather than a race: it runs inside the signal handler,
			// this pass is deferred after it, so the specific reason lands and this
			// one fills the gap it left. Anything reaching here undecided is
			// genuinely the drain's to name.
			//
			// **`pending` is half the condition, not a tidy-up.** A `Start` still
			// in flight has no handle *yet* — onStarted adopts it — so recording on
			// the handle alone freezes the row a moment before the launch it is
			// describing, and the record then claims `ran=false` and no usage about
			// a process that ran and spent. The worker is what says the attempt's
			// story is not finished; drained() refuses to complete on the same
			// field, so waiting costs nothing and cannot strand the record.
			o.recordAttempt(r, core.FailureKilled, VerdictUnknown)
		}
		// Everything the loop had already ordered — a label projection, a
		// milestone, a release reconciliation decided before the signal — is
		// re-driven until it lands. This is the "completes the effects already
		// ordered" half, and it is the half a restart cannot redo: §9.10 reads
		// which label a transition removed, so a projection abandoned halfway
		// leaves recovery reading a state this daemon never reached.
		o.driveOwed(ctx, r)
		if r.handle != nil && !r.groupGone {
			// StopInterrupt, not StopDiscard. Discard is deliberately less
			// patient — the harness cuts the ladder's grace to a tenth — and
			// impatience is the wrong trade when the only answer worth having is
			// a confirmed one. It also aborts event emission, which would
			// truncate the verbatim record §7.2 requires of a run that is being
			// interrupted rather than thrown away.
			o.beginStop(ctx, r, core.StopInterrupt)
		}
	}
	if !o.drained() {
		return
	}
	for _, w := range o.drainWaiters {
		close(w)
	}
	o.drainWaiters = nil
}

// drained reports whether the loop owes the outside world nothing further.
//
// Three conditions, and each names something a restart could not reconstruct:
//
//   - **A worker still out.** Its result would land on a record nobody owns —
//     and a Start in flight is the sharp case, since abandoning it leaves a
//     process running in a worktree with no handle anywhere to stop it.
//   - **A process group not confirmed gone.** §9.8's rule, and the one thing the
//     drain insists on rather than merely tidies.
//   - **A tracker write still owed.** The projection is what §9.10 classifies
//     from; a half-written one is a state this daemon never reached.
//
// Deliberately *not* a condition: an unrouted verdict, an unfired after-run hook,
// or a held claim awaiting a human. Those are work the next start reconstructs or
// a human resolves, and waiting on them would make shutdown depend on a review
// queue.
func (o *Orchestrator) drained() bool {
	for _, r := range o.records {
		switch {
		case r.pending > 0, r.probeInFlight, r.stopInFlight:
			return false
		case r.handle != nil && !r.handleDone:
			// Done, not merely a confirmed group. Stop answers about the process
			// *group*; Done additionally means the process has been reaped and
			// its transcript flushed and closed (SPEC §7.2, §7.4). Exiting on the
			// group answer alone truncates the verbatim record of the run we just
			// interrupted — the same cost that ruled StopDiscard out one line
			// above, arrived at from the other end.
			//
			// Bounded, because the group is confirmed gone by the time this is
			// the only thing left: a reaped leader is one whose Wait has
			// returned. #79's unbounded case is a leader that survives, and that
			// shows up as an unconfirmed stop instead.
			return false
		case r.handle != nil && !r.groupGone:
			// A live process group, and the drain's whole reason for waiting. It
			// is load-bearing for the records driveShutdown does *not* re-arm —
			// the ordered exits, whose stop is re-driven by retryPendingExits on
			// the tick instead. Between those ticks stopInFlight is false, so
			// without this clause a reaped leader with surviving descendants
			// reads as drained and the daemon leaves while a process still holds
			// the worktree, which is the one thing §9.8 refuses.
			//
			// It was briefly a comment with no `return`, and nothing failed —
			// which is what an assertion missing from the suite looks like from
			// inside the code.
			return false
		case r.owesAnything() || r.owedInFlight:
			return false
		}
	}
	for _, h := range o.held {
		// A held claim owns no process and no workspace, so the only thing it
		// can owe is a release reconciliation ordered before the signal — and the
		// confirming Get that can resolve one the tracker will never accept, which
		// the drain keeps offering for exactly that reason (#135, held.go). A
		// releasing entry that could never be dropped would otherwise hold the
		// drain open until the supervisor's TimeoutStopSec ended it.
		if h.inFlight || h.releasing {
			return false
		}
	}
	for _, p := range o.markerClears {
		// A run-marker removal *executing* right now. Waiting for it is what the
		// synchronous version gave for free, and losing it is not cosmetic: the
		// markers cleared on this path carry no evidence — a launch that never
		// happened, a drain's own interrupt — so one that survives the exit is read
		// as unknown_launch at the next start and parks for a human an issue §9.10
		// would otherwise have resumed by itself.
		//
		// In flight only, never merely owed. A removal is one bounded local write, so
		// this waits for at most one of them per clear; a clear that keeps *failing*
		// stays owed and would block the drain for as long as the state directory
		// stayed unwritable, which is the trade §9.8 refuses — whatever has landed
		// durably stays standing, and recovery reads it back.
		if p.inFlight {
			return false
		}
	}
	return true
}

// suspendStopped completes a shutdown interrupt whose termination came back
// confirmed. It is onStopped's first branch, ahead of the ones that exit the
// record, because it must reach none of them.
//
// The group is gone, so the workspace has no process in it and the record's
// in-memory life is over — but nothing is released, disposed, projected or
// routed. The outcome the run reported is deliberately dropped rather than
// applied: routing it is what produces `done`, `needs-review` or a continuation,
// and each of those is a terminal projection or a new dispatch.
func (o *Orchestrator) suspendStopped(ctx context.Context, r *Record) {
	r.groupGone = true
	// Cleared even though the drain releases and projects nothing. The group is
	// confirmed gone, so the workspace genuinely is free, and a marker left
	// standing would make every gracefully stopped workspace read as
	// unknown_launch at the next start — parking for a human exactly the issues
	// §9.10 is meant to resume by itself.
	o.freeWorkspaceMarker(ctx, r)
	o.finishSuspended(ctx, r)
}

// finishSuspended completes a suspended record once both facts are in: the
// group is confirmed gone, and the process has been reaped with its transcript
// flushed (Done).
//
// Waiting for the second is what stops the daemon exiting mid-flush. The event
// pump is deliberately left running until here — it is what drains Events, and a
// harness whose consumer stopped reading parks with its record half written.
//
// The after-run hook fires here, and it is the one thing the drain *does* on the
// way out. §5.2.6 owes it to every attempt that ran a process whatever the
// outcome, and this attempt did run one: it is over, not paused, since §9.10
// resumes the issue as a new attempt. It is bounded by hooks.timeout_ms, which
// is why it can sit inside a drain at all — and it goes on the owed queue, so
// the drain waits for it like any other effect.
func (o *Orchestrator) finishSuspended(ctx context.Context, r *Record) {
	if !r.groupGone || (r.handle != nil && !r.handleDone) {
		return
	}
	if r.cancelRun != nil {
		r.cancelRun()
		r.cancelRun = nil
	}
	r.handle = nil
	r.outcome = nil
	o.attemptEnded(ctx, r)
	// And the outcome record, for the reason stated above the hook: this attempt
	// is over rather than paused, and §9.10 resumes the issue as a *new* one. §7.3's
	// `killed` — "deliberate stop" — is what happened to it, and the verdict is
	// unknown because the outcome it reported was dropped a few lines up rather
	// than routed. Without this a SIGTERM would silently cost the attempt's whole
	// duration, usage and reason (#60).
	// The attempt-outcome record is deliberately *not* written here, next to the
	// hook it belongs beside. driveShutdown writes it, and what makes that provable
	// is the ordering: this runs inside a signal handler, `handle` is cleared on the
	// line above, and driveShutdown is deferred after every signal — so the pass
	// that observes `handle == nil` is the very next thing to run, and the drain
	// cannot complete without it. A second call *here* would be unreachable by any
	// test, which is what an assertion missing from the suite looks like from
	// inside the code (see drained's third clause).
	//
	// Reachability is the whole test, and it is not the same as "only one site
	// records". endAttemptSuspended does too (#61), and it is reachable for the
	// reason this is not: it runs on a record the drain has *already* suspended, so
	// driveShutdown's pass has been and gone and its `killed` would otherwise be the
	// reason a decided attempt is filed under.
	o.log.Info("shutdown: run interrupted, process group gone and transcript closed",
		"issue", r.Issue.Identifier, "state", r.State, "attempt", r.Attempt)
}
