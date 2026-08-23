package orchestrator

import (
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
