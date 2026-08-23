package orchestrator

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The §6.5 after_run hook: one per attempt that ran a process, and none for an
// attempt that never launched one. B05 keeps hooks off the three-method core
// interface, so the loop discovers the capability — which makes "did it fire, and
// exactly once" a question about the loop's accounting of attempts rather than
// about the workspace. Retries and continuations each prepare again and so each
// earn their own; a parked record that a human later closes does not.

// SPEC §6.5: the after-run hook fires when an attempt ends. B05 keeps it off
// the three-method core interface, so the loop discovers it.
func TestAfterRunHookFiresWhenAnAttemptEnds(t *testing.T) {
	h := start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("1", epoch)},
		withHook: true,
	})
	h.WaitState("1", StateDone)

	waitFor(t, "the after-run hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })
	if got := h.Hooked.AfterRunCount("1"); got != 1 {
		t.Errorf("after_run fired %d times, want 1", got)
	}
}

// SPEC §6.5: one after_run per attempt that ran a process — including the
// retries and continuations that go on to prepare again, and not twice for a
// parked record that is later closed.
func TestAfterRunFiresOncePerAttempt(t *testing.T) {
	t.Run("retry gets its own", func(t *testing.T) {
		h := start(t, harnessOpts{
			issues:   []core.Issue{fake.Issue("1", epoch)},
			withHook: true,
			script: func(_ core.RunSpec, attempt int) []core.Event {
				if attempt == 1 {
					return fake.Fail(core.FailureCrashed)
				}
				return fake.Succeed("s")
			},
		})
		if !h.Clock.BlockUntilWaiters(2) {
			t.Fatal("the backoff timer was never armed")
		}
		waitFor(t, "the first attempt's hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })

		h.Clock.Advance(11 * time.Second)
		h.WaitState("1", StateDone)
		waitFor(t, "the second attempt's hook", func() bool { return h.Hooked.AfterRunCount("1") == 2 })
	})

	t.Run("continuation gets its own", func(t *testing.T) {
		var mu sync.Mutex
		verdict := VerdictIncomplete
		h := start(t, harnessOpts{
			issues:   []core.Issue{fake.Issue("1", epoch)},
			withHook: true,
			script:   func(core.RunSpec, int) []core.Event { return fake.Succeed("s") },
			verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
				mu.Lock()
				defer mu.Unlock()
				return VerifyResult{Verdict: verdict}, nil
			}),
		})
		waitFor(t, "the first attempt's hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })

		if !h.Clock.BlockUntilWaiters(2) {
			t.Fatal("the continuation timer was never armed")
		}
		mu.Lock()
		verdict = VerdictPublished
		mu.Unlock()
		h.Clock.Advance(2 * time.Second)
		h.WaitState("1", StateDone)
		waitFor(t, "the continuation's hook", func() bool { return h.Hooked.AfterRunCount("1") == 2 })
	})

	t.Run("parked then closed fires once", func(t *testing.T) {
		h := start(t, harnessOpts{
			issues:   []core.Issue{fake.Issue("1", epoch)},
			withHook: true,
			verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
				return VerifyResult{Verdict: VerdictContradicted, Detail: "no commits"}, nil
			}),
		})
		h.WaitState("1", StateNeedsReview)
		waitFor(t, "the parked attempt's hook", func() bool { return h.Hooked.AfterRunCount("1") == 1 })

		h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
		h.Tick()
		h.WaitGone("1")

		time.Sleep(20 * time.Millisecond)
		if got := h.Hooked.AfterRunCount("1"); got != 1 {
			t.Errorf("after_run fired %d times for one attempt", got)
		}
	})

	t.Run("an attempt that never launched gets none", func(t *testing.T) {
		h := start(t, harnessOpts{
			issues:      []core.Issue{fake.Issue("1", epoch)},
			withHook:    true,
			beforeStart: func(*fake.Tracker) {},
			failStart:   errors.New("harness binary missing"),
		})
		h.WaitGone("1")
		if got := h.Hooked.AfterRunCount("1"); got != 0 {
			t.Errorf("after_run fired %d times for an attempt that never started a process", got)
		}
	})
}
