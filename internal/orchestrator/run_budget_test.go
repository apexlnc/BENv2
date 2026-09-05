package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The run budgets (SPEC §9.9 cost, §9.6 turns) and the park at the end of one.
// Not the §8.5 per-tick tracker request budget, which is budget_test.go.
//
// Two of the three ways into needs-review are exhausted bounds, so the park and
// the human gesture that undoes it are one subject: a re-queue restores the
// budgets — otherwise the next attempt re-parks immediately and the gesture
// means nothing — while leaving the history it does not own, which is what the
// next prompt reports as run.previous_outcome. The breach itself is a stop, so it
// is retried until termination is confirmed, and the parked record it leaves
// behind still has to reconcile.

// SPEC §9.9 + §9.8: a budget stop whose termination is unconfirmed is retried
// on later ticks. Nothing in the tracker changes — the issue is active and
// routable — so reconciliation alone would never revisit it.
func TestUnconfirmedBudgetStopIsRetried(t *testing.T) {
	h := start(t, harnessOpts{
		issues:          []core.Issue{fake.Issue("1", epoch)},
		extraConfig:     "  max_cost_usd: 1.0\n",
		hang:            true,
		stopUnconfirmed: true,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
			}
		},
	})

	waitFor(t, "the budget stop to be attempted", func() bool {
		hd := h.Runner.LastHandle()
		return hd != nil && hd.StopCount() > 0
	})
	handle := h.Runner.LastHandle()
	// The answer before the tick, then the retry waited for: `beginStop` refuses
	// while a ladder is in flight, and an unconfirmed stop writes no transition
	// for `Tick`'s settle to see either (#106's review).
	h.waitStopApplied(1)
	before := handle.StopCount()

	h.Tick()
	waitFor(t, "the unconfirmed budget stop to be retried on the next tick", func() bool {
		return handle.StopCount() > before
	})

	// The retried stop's answer acknowledged first, the same rule one ladder later
	// (waitStopApplied).
	h.waitStopApplied(2)
	h.Runner.SetStopTermination(core.TerminationConfirmed)
	h.Tick()
	h.WaitState("1", StateNeedsReview)
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposed %+v; a budget breach keeps the workspace", got)
	}
}

// A parked record must keep reconciling. onStopped left `stopping` set, and
// exiting() suppresses reconciliation for anything exiting — so a
// budget-parked issue that a human then closed, or re-queued, was ignored
// forever.
func TestBudgetParkedRecordStillReconciles(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		hang:        true,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
			}
		},
	})
	h.WaitState("1", StateNeedsReview)
	h.WaitEffects(1)

	// A human re-queues by removing the state label (SPEC §9.8).
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = []string{"ben-queue"} })
	h.Tick()
	h.WaitState("1", StateBackoff)
}

// The same record, closed instead of re-queued: it must be cleaned up rather
// than held.
func TestBudgetParkedRecordCleansUpWhenClosed(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		hang:        true,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
			}
		},
	})
	h.WaitState("1", StateNeedsReview)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	h.WaitGone("1")
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want the claim dropped once the parked issue closed", n)
	}
}

// SPEC §9.8 (2026-08-08 amendment, #38): a human re-queue **restores the run
// budgets**. Two of the three ways into needs-review are exhausted bounds —
// budget_exceeded (§9.9) and max_turns (§9.6) — so a park that carried them
// forward would re-park on the next attempt and the human gesture would mean
// nothing.
//
// Asserted from behavior rather than from the record's fields, which the loop
// goroutine owns: the re-queued attempt has to survive a cost that the
// unrestored budget would have parked it for a second time.
func TestUnparkRestoresTheRunBudgets(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return []core.Event{
					{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
					{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
				}
			}
			// Well inside the cap on its own, and over it if the parked
			// attempt's 2.0 is still on the record.
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s2", Continuation: "s2"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 0.5}},
				{Type: core.EventSucceeded},
			}
		},
	})
	h.WaitState("1", StateNeedsReview)
	h.WaitEffects(1)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = []string{"ben-queue"} })
	h.PollNow()
	h.WaitState("1", StateBackoff)

	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(11 * time.Second)
	h.WaitState("1", StateDone)
}

// The other two budgets a re-queue restores, at the unit the loop cannot
// expose: turns, and the attempt base max_attempts is measured from. The
// prior outcome is deliberately *not* restored — it is what the next
// attempt's prompt reports as run.previous_outcome (SPEC §5.6).
func TestRestoreBudgetsResetsTurnsAndTheAttemptBase(t *testing.T) {
	def := definition(t, "3", "")
	o := &Orchestrator{cfg: Config{Runtime: newTestSource(def, &Bundle{Definition: def})}}
	max := o.limits().MaxAttempts
	r := &Record{
		Attempt: max + 4, Turns: o.limits().MaxTurns, costUSD: 12,
		FailureReason: core.FailureBudgetExceeded,
		lastOutcome:   string(core.FailureBudgetExceeded),
	}
	if o.attemptsRemain(r) || o.continuable(r) {
		t.Fatal("the fixture is not actually exhausted; the test would pass either way")
	}

	r.restoreBudgets()

	if r.Turns != 0 || r.costUSD != 0 {
		t.Errorf("turns=%d cost=%v after a re-queue; a max_turns or budget park would immediately re-park",
			r.Turns, r.costUSD)
	}
	if !o.attemptsRemain(r) {
		t.Errorf("attempt=%d base=%d: max_attempts is measured from the re-queue, not from the beginning of time",
			r.Attempt, r.attemptBase)
	}
	if !o.continuable(r) {
		t.Error("the continuation budget was not restored")
	}
	if r.FailureReason != core.FailureBudgetExceeded {
		t.Errorf("failure reason = %q; the budgets reset, the history does not (SPEC §5.6)", r.FailureReason)
	}
	if r.lastOutcome != string(core.FailureBudgetExceeded) {
		t.Errorf("last outcome = %q; the budgets reset, the prompt history does not (SPEC §5.6)", r.lastOutcome)
	}
}

// The two tests below are #236's, and they share the one situation the defect
// needs: a record carrying `budget_exceeded` while an attempt that has breached
// nothing is running.
//
// That combination is not exotic, it is what a resolved park *is*. §9.9 parks on
// the breach and records the reason; §9.8's human re-queue restores the budgets
// and deliberately keeps the reason, because it is what the next prompt reports
// as `run.previous_outcome` (SPEC §5.6, restoreBudgets); and beginPrepare does
// not clear it either. So from the second attempt's launch until it ends, the
// record says `budget_exceeded` and nothing about the attempt does.
//
// onStopped used to route a confirmed termination on that field, which made every
// ordered exit landing in that window a park — the one read three separate
// comments in this package already warn against (attempt.go's recordAttempt,
// translog.go, orchestrator.go's status projection). The cause is now carried by
// the stop that was ordered (Record.budgetStop).

// reparkedFixture drives that shape: attempt 1 breaches the §9.9 cap and parks,
// a human re-queues, and attempt 2 is left running and hung.
//
// It stops at `running` rather than asserting anything, because what the two
// tests differ on is the exit that arrives next: an issue that went terminal
// still has a claim to release, and one that was deleted has nothing left to
// write to at all.
func reparkedFixture(t *testing.T) *harness {
	t.Helper()
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		hang:        true,
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return []core.Event{
					{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
					{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
				}
			}
			// No usage at all: this attempt breaches nothing, so anything that
			// parks it is reading the attempt before it.
			return []core.Event{{Type: core.EventStarted, SessionID: "s2", Continuation: "s2"}}
		},
	})
	h.WaitState("1", StateNeedsReview)
	// The projection, not just the state. The re-queue below rewrites the label
	// set, and a park write still queued behind it would put `ben:needs-review`
	// straight back — leaving the record parked and the fixture asserting nothing.
	waitFor(t, "the park's ben:needs-review projection to land", func() bool {
		return h.Tracker.Label("1") == core.StateLabelNeedsReview
	})

	// A human re-queues by removing the state label (SPEC §9.8), which restores
	// the budgets and keeps the reason.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = []string{"ben-queue"} })
	h.PollNow()
	h.WaitState("1", StateBackoff)
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(11 * time.Second)
	h.WaitState("1", StateRunning)
	waitFor(t, "the re-dispatched attempt's ben:running projection to land", func() bool {
		return h.Tracker.Label("1") == core.StateLabelRunning
	})
	return h
}

// parkCount is how many times a §9.11 path entered needs-review. One is the
// legitimate §9.9 park; a second is the defect.
func parkCount(path []State) int {
	n := 0
	for _, s := range path {
		if s == StateNeedsReview {
			n++
		}
	}
	return n
}

// #236, SPEC §9.8: an issue closed during the attempt that follows a §9.9 park
// is finished, not parked again.
//
// Every consequence asserted below followed from the one read: a `needs-review`
// projection and milestone posted onto a *closed* issue, the claim retained
// rather than released, the workspace kept rather than disposed, and the attempt
// filed under `budget_exceeded` when what ended it was a deliberate stop —
// §7.3's `killed`.
func TestAnOrderedExitAfterABudgetParkFinishesRatherThanReparking(t *testing.T) {
	h := reparkedFixture(t)

	// The §9.8 terminal verdict, mid-attempt.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	// The whole defect in one barrier: pre-fix the record parks a second time and
	// stays there, and WaitGone's failure prints the §9.11 path that says so.
	h.WaitGone("1")

	if got := parkCount(h.o.Transitions.Path("1")); got != 1 {
		t.Errorf("needs-review appears %d times in the §9.11 path %v, want 1: the exit was routed into §9.9's park",
			got, h.o.Transitions.Path("1"))
	}
	parks := 0
	for _, m := range h.Tracker.Milestones("1") {
		if m == core.MilestoneNeedsReview {
			parks++
		}
	}
	if parks != 1 {
		t.Errorf("posted %d needs-review milestones (%v), want the one the real park earned: the second asks a "+
			"human to review a closed issue", parks, h.Tracker.Milestones("1"))
	}
	if got := h.Tracker.Label("1"); got == core.StateLabelNeedsReview {
		t.Errorf("label = %q on a closed issue; the record left the machine, so nothing is waiting for a human", got)
	}

	// finishNow's own two effects, neither of which a park performs.
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want the claim dropped once the issue went terminal (SPEC §9.8)", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v; a terminal issue's workspace is disposed, not kept for a review that is not coming", got)
	}

	// And the attempt is filed under what actually ended it (SPEC §7.3, #60).
	outcomes := h.o.Attempts.For("1")
	if len(outcomes) != 2 {
		t.Fatalf("recorded %d attempt outcomes (%+v), want one per dispatch", len(outcomes), outcomes)
	}
	if got := outcomes[0].FailureReason; got != core.FailureBudgetExceeded {
		t.Errorf("attempt 1 reason = %q, want %q; the fixture is not the one this test is about",
			got, core.FailureBudgetExceeded)
	}
	if got := outcomes[1].FailureReason; got != core.FailureKilled {
		t.Errorf("attempt 2 reason = %q, want %q: it was stopped deliberately, and filing it as a breach it never "+
			"had puts a phantom in the failure histogram every later attempt is compared against",
			got, core.FailureKilled)
	}
}

// #236, the variant that does not heal: the same exit, for an issue that was
// *deleted*.
//
// `markGone` makes orderedExit() permanently true, so a record parked in
// needs-review with it set is unreachable by every route that would revisit one.
// Reconciliation skips exiting records; the parked sweep never lists it
// (beginReconcile); its owed tracker writes are discarded unattempted rather than
// confirmed (absence.go, offerOwedConfirmations skips `gone`); and no exit is
// queued, because the one that would have been was consumed by the park. The
// record, its §9.5 slot, its workspace and — because identityWorkOutstanding()
// counts records — every identity reload for the life of the process are held.
//
// So this asserts the release, in the sense the word has for an issue that no
// longer exists: no tracker write is owed or attempted, the workspace is swept,
// and the daemon can adopt a new identity again.
func TestADeletedIssueAfterABudgetParkReleasesTheRecord(t *testing.T) {
	h := reparkedFixture(t)

	// Deleted or transferred: Get answers core.ErrIssueNotFound (SPEC §9.8).
	h.Tracker.Delete("1")
	h.Tick()

	// Pre-fix this never returns: the record parks with `gone` set, and there is
	// no route back into anything that would revisit it.
	h.WaitGone("1")

	if got := parkCount(h.o.Transitions.Path("1")); got != 1 {
		t.Errorf("needs-review appears %d times in the §9.11 path %v, want 1: the record parked on an issue that "+
			"is gone, which is the state nothing can resolve", got, h.o.Transitions.Path("1"))
	}
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d releases; there is no issue to unassign, and an owed write that can never land is "+
			"retried every tick for the life of the process (SPEC §9.8)", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v; the issue is gone, so nobody is coming to review the worktree", got)
	}

	// The leak's furthest consequence, and the one an operator notices: a
	// lingering record refuses every identity reload (identityWorkOutstanding).
	// Driven through ticks because a marker removal owed by the exit is
	// identity work too, and it lands on one.
	moved := *h.Bundle
	moved.ClaimPrincipal = "someone-else"
	h.tickUntil("an identity change to be adopted once the deleted issue's record has gone", func() bool {
		return h.o.AdoptIdentity(context.Background(), h.Bundle, &moved, func() {}) == nil
	})
	if !h.o.IdentityQuiescent() {
		t.Error("the advisory still reports work outstanding after the record left; the config watcher would " +
			"rebuild a candidate on every tick to be refused on every tick")
	}
}

// A budget stop has two independent facts: the cap caused the stop, and a later
// exit may own what happens after it. Losing the first while applying the second
// records the attempt as `killed`, corrupting the per-cause outcome histogram.
//
// The failing running projection makes the issue's deletion discoverable while
// the budget's stop ladder is held open. That absence confirmation is the route
// that can overtake an already-started stop: reconciliation correctly skips a
// record whose exit is already ordered.
func TestAnExitOvertakingABudgetStopPreservesTheAttemptCause(t *testing.T) {
	runningWrite := make(chan struct{}, 1)
	releaseRunningWrite := make(chan struct{})
	releaseStop := make(chan struct{})
	closeIfOpen := func(ch chan struct{}) {
		select {
		case <-ch:
		default:
			close(ch)
		}
	}
	t.Cleanup(func() {
		closeIfOpen(releaseRunningWrite)
		closeIfOpen(releaseStop)
	})

	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		hang:        true,
		stopGate:    func() { <-releaseStop },
		beforeStart: func(tr *fake.Tracker) {
			tr.FailLabel = func(_ string, label core.StateLabel) error {
				if label != core.StateLabelRunning {
					return nil
				}
				select {
				case runningWrite <- struct{}{}:
				default:
				}
				<-releaseRunningWrite
				return errors.New("502 from the tracker")
			}
		},
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
			}
		},
	})

	select {
	case <-runningWrite:
	case <-time.After(2 * time.Second):
		t.Fatal("the running projection was never attempted")
	}
	waitFor(t, "the budget stop to enter its held ladder", func() bool {
		handle := h.Runner.LastHandle()
		return handle != nil && handle.StopCount() == 1
	})

	// Acknowledge the failed write before spending the tick that confirms its
	// issue is absent. Without this barrier that tick may race ahead of the fact
	// it is supposed to investigate and prove nothing.
	effectsBefore := h.applied(sigEffectDone)
	closeIfOpen(releaseRunningWrite)
	waitFor(t, "the failed running projection to be handled", func() bool {
		return h.applied(sigEffectDone) > effectsBefore
	})

	h.Tracker.Delete("1")
	confirmationsBefore := h.applied(sigOwedConfirmed)
	h.Tick()
	waitFor(t, "the failed write's absence confirmation to order the exit", func() bool {
		return h.applied(sigOwedConfirmed) > confirmationsBefore
	})

	// The exit now owns the post-stop route, but the cap still owns the cause.
	closeIfOpen(releaseStop)
	h.WaitGone("1")

	outcomes := h.o.Attempts.For("1")
	if len(outcomes) != 1 {
		t.Fatalf("recorded %d attempt outcomes (%+v), want one", len(outcomes), outcomes)
	}
	if got := outcomes[0].FailureReason; got != core.FailureBudgetExceeded {
		t.Errorf("failure reason = %q, want %q: the later exit changed the stop's cause",
			got, core.FailureBudgetExceeded)
	}
	if got := parkCount(h.o.Transitions.Path("1")); got != 0 {
		t.Errorf("needs-review appears %d times in the §9.11 path %v, want 0: the exit must still own the route",
			got, h.o.Transitions.Path("1"))
	}
}

// The same cause/disposition ordering applies after the stop confirms. The
// budget park reads the branch before it routes, and an exit landing during
// that read must not relabel the already-ended attempt as `killed` merely
// because finishIfRequested reaches finishNow instead of parkOnBudget.
func TestAnExitOvertakingADeferredBudgetParkPreservesTheAttemptCause(t *testing.T) {
	reading := make(chan struct{}, 1)
	releaseRead := make(chan struct{})
	closeRead := func() {
		select {
		case <-releaseRead:
		default:
			close(releaseRead)
		}
	}
	defer closeRead()

	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		hang:        true,
		attemptFacts: func(core.Workspace) (core.AttemptFacts, error) {
			reading <- struct{}{}
			<-releaseRead
			return core.AttemptFacts{}, nil
		},
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
			}
		},
	})

	select {
	case <-reading:
	case <-time.After(2 * time.Second):
		t.Fatal("the confirmed budget stop never reached its branch-account read")
	}

	// The stop has confirmed, but its park is held behind AttemptFacts. The
	// terminal verdict therefore defers finishNow on that same pending worker.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	reconciledBefore := h.applied(sigReconciled)
	h.Tick()
	waitFor(t, "the terminal reconciliation to overtake the deferred park", func() bool {
		return h.applied(sigReconciled) > reconciledBefore
	})
	closeRead()
	h.WaitGone("1")

	outcomes := h.o.Attempts.For("1")
	if len(outcomes) != 1 {
		t.Fatalf("recorded %d attempt outcomes (%+v), want one", len(outcomes), outcomes)
	}
	if got := outcomes[0].FailureReason; got != core.FailureBudgetExceeded {
		t.Errorf("failure reason = %q, want %q: the later exit changed the already-confirmed stop's cause",
			got, core.FailureBudgetExceeded)
	}
	if got := parkCount(h.o.Transitions.Path("1")); got != 0 {
		t.Errorf("needs-review appears %d times in the §9.11 path %v, want 0: the exit must still own the route",
			got, h.o.Transitions.Path("1"))
	}
}
