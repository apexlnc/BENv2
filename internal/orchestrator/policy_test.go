package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// SPEC §9.6: min(10s · 2^(attempt−1), max) plus deterministic jitter. The
// sequence is pinned exactly — a jitter that drifted would be a jitter tests
// could not reproduce, which is the property the spec asks for by name.
func TestBackoffSequenceIsExactlyReproducible(t *testing.T) {
	const max = 5 * time.Minute

	var first []time.Duration
	for attempt := 1; attempt <= 6; attempt++ {
		first = append(first, backoffDelay("42", attempt, max))
	}

	// Same inputs, same outputs — every time, in any order.
	for range 100 {
		for attempt := 1; attempt <= 6; attempt++ {
			if got := backoffDelay("42", attempt, max); got != first[attempt-1] {
				t.Fatalf("attempt %d gave %v then %v; the jitter is not deterministic", attempt, first[attempt-1], got)
			}
		}
	}

	// The envelope: each delay within ±20% of the doubling schedule, capped.
	for attempt := 1; attempt <= 6; attempt++ {
		want := baseBackoff << (attempt - 1)
		if want > max {
			want = max
		}
		lo, hi := time.Duration(float64(want)*0.8), time.Duration(float64(want)*1.2)
		if got := first[attempt-1]; got < lo || got > hi {
			t.Errorf("attempt %d delay %v is outside [%v, %v] around the %v schedule", attempt, got, lo, hi, want)
		}
	}

	// The cap holds however far the attempts run.
	if got := backoffDelay("42", 40, max); got > time.Duration(float64(max)*1.2) {
		t.Errorf("attempt 40 delay %v escaped the cap %v", got, max)
	}
}

// The jitter spreads by issue, which is what de-synchronizes two daemons
// racing the same queue.
func TestBackoffJitterDiffersByIssue(t *testing.T) {
	const max = time.Hour
	seen := map[time.Duration]string{}
	collisions := 0
	for i := range 50 {
		id := fmt.Sprint(i)
		d := backoffDelay(id, 1, max)
		if other, ok := seen[d]; ok {
			collisions++
			t.Logf("issues %s and %s share delay %v", other, id, d)
		}
		seen[d] = id
	}
	if collisions > 2 {
		t.Errorf("%d of 50 issues collided on the same delay; the spread is too narrow", collisions)
	}
}

// SPEC §9.8: an unconfirmed stop retains the claim. A possibly-alive process
// must never share a workspace with a replacement, so nothing is released,
// nothing is disposed, and the stop is retried on a later tick.
func TestUnconfirmedStopRetainsTheClaim(t *testing.T) {
	h := start(t, harnessOpts{
		issues:          []core.Issue{fake.Issue("1", epoch)},
		hang:            true,
		script:          startedOnly,
		stopUnconfirmed: true,
	})

	h.WaitState("1", StateRunning)

	// A human closes the issue: reconciliation should stop the run.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	handle := h.Runner.LastHandle()
	if handle.StopCount() == 0 {
		t.Fatal("reconciliation never asked the run to stop")
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times after an unconfirmed stop; the claim must be retained", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposed %+v after an unconfirmed stop", got)
	}
	// Still tracked, so no replacement can be dispatched onto the workspace.
	found := false
	for _, s := range h.o.Status() {
		if s.Identifier == "1" {
			found = true
		}
	}
	if !found {
		t.Error("the record was dropped, which would let a replacement share the workspace")
	}

	// Retried next tick. Two barriers, and the first is the one that makes the
	// tick meaningful: the Stop answer has to be *applied* before the tick
	// that is supposed to re-drive it, because `beginStop` refuses while one is
	// in flight — a tick inside that window schedules nothing, and `onStopped`
	// then clears the flag without scheduling a tick of its own, so there is no
	// later event for a passive wait to observe (#106's review). `StopCount`
	// cannot stand in for it: the fake counts the call at entry.
	h.waitStopApplied(1)
	before := handle.StopCount()
	h.Tick()
	// And the retry itself is waited for rather than read straight after the
	// tick: an unconfirmed stop writes no transition, so `Tick`'s settle is not a
	// barrier on this fact either.
	waitFor(t, "the unconfirmed stop to be retried on the next tick", func() bool {
		return handle.StopCount() > before
	})

	// And once it confirms, the exit completes — with the *retried* stop's answer
	// acknowledged first, the same rule one ladder later (waitStopApplied).
	h.waitStopApplied(2)
	h.Runner.SetStopTermination(core.TerminationConfirmed)
	h.Tick()
	h.WaitGone("1")
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times after a confirmed stop, want 1", n)
	}
}

// SPEC §7.4 + §7.5 + §9.8, the same rule for a run nobody stopped: a liveness
// failure is retryable, and the retry *reattaches the same worktree* (§6.2), so
// it must not begin until that attempt's execution domain is confirmed quiet.
//
// The two facts are separate and only one of them is in the event.
// `failed(stalled)` says the runner gave up on the attempt; it says nothing
// about whether the substrate made the domain quiet, and the harness's liveness
// teardown has no channel to say so — it discards that operation's answer on
// purpose, because the fact worth acting on is whether the domain is quiet *now*
// (harness expire, core.Termination). So the loop asks that question itself,
// with `Stop`, and an unconfirmed answer retains everything: the claim, the
// record, the workspace, and the §9.5 slot.
//
// Every assertion below is barriered on an observed stop count or state. A
// sleep would prove nothing here: "no retry happened yet" and "no retry will
// ever happen" look identical from a timer.
func TestUnconfirmedDomainAfterALivenessFailureBlocksTheRetry(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureStalled) },
		// The run ends here, so `stopUnconfirmed` needs its pair: without it the
		// domain is quiet the moment the process is, the pre-Done probe says so,
		// and the outcome routes before the teardown below is ever asked for
		// (harnessOpts.stopUnconfirmed, #100). Measured at 11 failures in 800
		// under parallel load, reported as "timed out waiting for the domain to
		// be probed" — the barrier was sound and the fixture was not.
		domainMembers:   true,
		stopUnconfirmed: true,
	})

	// The run reported and ended, and the loop asked about the domain rather
	// than acting on the event.
	waitFor(t, "the domain to be observed after the run ended", func() bool {
		hd := h.Runner.LastHandle()
		return hd != nil && hd.StopCount() > 0
	})
	handle := h.Runner.LastHandle()
	// StopInterrupt, not StopDiscard: the run is over and its transcript is
	// §7.2's obligation, so the probe must not be the impatient mode that
	// abandons the reader (harness confirmQuiet, fake Handle.Stops).
	if got := handle.Stops(); len(got) == 0 || got[0] != core.StopInterrupt {
		t.Errorf("probe modes = %v, want it to begin with StopInterrupt", got)
	}
	if got := h.stateOf("1"); got != StateRunning {
		t.Errorf("state = %q, want %q: a held outcome does not move the record", got, StateRunning)
	}

	// Retried on the next tick — reconciliation alone would never revisit this
	// issue, which is active, routable and unchanged on the tracker.
	//
	// The answer first, then the tick: `confirmQuiet` refuses while a ladder is in
	// flight, so a tick inside that window asks nothing and `onStopped` then
	// clears the flag without scheduling one (#106's review). Then the re-probe is
	// waited for rather than read straight after the tick, because `Tick` returns
	// on 20 ms of transition-log quiet and an unconfirmed answer writes no
	// transition at all.
	h.waitStopApplied(1)
	before := handle.StopCount()
	h.Tick()
	waitFor(t, "the unconfirmed group to be re-probed on the next tick", func() bool {
		return handle.StopCount() > before
	})

	// And in the meantime nothing has touched the workspace the possibly-live
	// process is sitting in. Prepare is the one that matters: §6.2 reattaches
	// rather than recreating, so a second one puts two agents in one worktree.
	if got := h.Workspaces.PrepareCount("1"); got != 1 {
		t.Errorf("Prepare called %d times; the retry entered a workspace that may still have an agent in it", got)
	}
	if got := h.Runner.StartCount(); got != 1 {
		t.Errorf("Start called %d times, want 1", got)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposed %+v under a possibly-live process", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times; an unconfirmed group retains the claim", n)
	}

	// Once the domain is confirmed quiet the held outcome routes, and the retry
	// finally enters the worktree it had to wait for. The re-probe's own answer is
	// acknowledged first, the same rule one ladder later (waitStopApplied).
	h.waitStopApplied(2)
	h.Runner.SetStopTermination(core.TerminationConfirmed)
	h.Tick()
	h.WaitState("1", StateBackoff)
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(11 * time.Second)
	waitFor(t, "the retry to start", func() bool { return h.Runner.StartCount() == 2 })
	if got := h.Workspaces.PrepareCount("1"); got != 2 {
		t.Errorf("Prepare called %d times after the domain was confirmed quiet, want 2", got)
	}
}

// SPEC §9.6 continuation track: a clean exit with no publish evidence
// re-dispatches with the continuation token, and the prompt tells the agent
// what happened.
func TestCleanExitWithoutEvidenceTakesTheContinuationTrack(t *testing.T) {
	var mu sync.Mutex
	verdict := VerdictIncomplete
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Succeed("session-A") },
		verifier: verifierFunc(func(_ context.Context, _ core.Issue, _ core.Workspace) (VerifyResult, error) {
			mu.Lock()
			defer mu.Unlock()
			return VerifyResult{Verdict: verdict, PRURL: "https://example.test/pull/1"}, nil
		}),
	})

	// The first attempt verifies incomplete and arms the ~1s continuation timer.
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the continuation timer was never armed")
	}

	mu.Lock()
	verdict = VerdictPublished
	mu.Unlock()

	h.Clock.Advance(2 * time.Second)
	h.WaitState("1", StateDone)

	if got := h.Runner.StartCount(); got != 2 {
		t.Fatalf("started %d runs, want a continuation re-dispatch", got)
	}
	// The token from the first session is carried into the second.
	if got := h.Runner.Continuations(); len(got) != 2 || got[1] != "session-A" {
		t.Errorf("continuations = %v, want the second run to resume session-A", got)
	}
	// SPEC §5.2: the prompt branches on attempt and previous_outcome.
	prompts := h.Runner.Prompts()
	if !strings.Contains(prompts[1], "Attempt 2") || !strings.Contains(prompts[1], "succeeded") {
		t.Errorf("continuation prompt does not say it is a continuation:\n%s", prompts[1])
	}
	if got := h.o.Transitions.Path("1"); !containsState(got, StateVerifying) {
		t.Errorf("path = %v", got)
	}
}

// SPEC §9.6: the continuation chain is bounded by max_turns, and running out
// parks rather than looping.
func TestContinuationExhaustionParks(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Succeed("s") },
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictIncomplete}, nil
		}),
	})

	// max_turns is 4: drive the chain until it parks.
	for range 8 {
		if h.stateOf("1") == StateNeedsReview {
			break
		}
		h.Clock.BlockUntilWaiters(2)
		h.Clock.Advance(2 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	h.WaitState("1", StateNeedsReview)

	if got := h.Runner.StartCount(); got > 5 {
		t.Errorf("started %d runs; max_turns=4 should have bounded the chain", got)
	}
	h.WaitEffects(1)
	if got := h.Tracker.Label("1"); got != core.StateLabelNeedsReview {
		t.Errorf("label = %q, want ben:needs-review", got)
	}
	// Claim and workspace are both retained when parked (SPEC §9.2).
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times while parked; needs-review retains the claim", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposed %+v while parked; needs-review keeps the workspace", got)
	}
}

// SPEC §9.9: usage past the cap stops the run and parks it, workspace kept.
func TestBudgetBreachParksTheRun(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		hang:        true,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 0.6}},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 0.7}},
			}
		},
	})

	h.WaitState("1", StateNeedsReview)

	if got := h.o.Transitions.Path("1"); got[len(got)-1] != StateNeedsReview {
		t.Errorf("path = %v, want it to end parked", got)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposed %+v; a budget breach keeps the workspace", got)
	}
	if h.Runner.LastHandle().StopCount() == 0 {
		t.Error("the run was not stopped")
	}
}

// SPEC §9.8: a parked issue whose state label a human removed re-enters
// backoff. §9.2's table omits this edge; the prose beside it requires it.
func TestHumanUnparkReentersBackoff(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no commits"}, nil
		}),
	})
	h.WaitState("1", StateNeedsReview)

	// The projection put ben:needs-review on the issue; a human takes it off.
	h.WaitEffects(1)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = []string{"ben-queue"} })
	h.Tick()

	h.WaitState("1", StateBackoff)
	if got := h.o.Transitions.Path("1"); !containsState(got, StateNeedsReview) || got[len(got)-1] != StateBackoff {
		t.Errorf("path = %v, want needs-review then backoff", got)
	}
}

// SPEC §9.8: a running issue that went terminal is stopped, disposed, and
// released.
func TestTerminalIssueIsDisposedAndReleased(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})

	h.WaitState("1", StateRunning)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	h.WaitGone("1")

	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want one non-keeping dispose", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want 1", n)
	}
}

// B08 acceptance (§8.4 known window): a human who assigns themselves inside
// the claim write→read-back window loses the assignment order, so the claim
// verifies and dispatch proceeds alongside them. Reconciliation must
// *detect* the unroutable assignee set on the next tick and begin the stop,
// workspace kept.
//
// Release is deliberately not asserted: §9.8 retains the claim whenever
// termination is unconfirmed and retries on later ticks, so demanding
// release-next-tick would encode a guarantee the design does not make.
func TestHumanCoAssigneeIsDetectedNextTick(t *testing.T) {
	h := start(t, harnessOpts{
		issues:          []core.Issue{fake.Issue("1", epoch)},
		hang:            true,
		script:          startedOnly,
		stopUnconfirmed: true,
	})

	h.WaitState("1", StateRunning)

	// The human arrives alongside our verified claim.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Assignees = append(i.Assignees, "a-human")
	})
	h.Tick()

	if h.Runner.LastHandle().StopCount() == 0 {
		t.Fatal("reconciliation did not begin a stop for an unroutable assignee set")
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposed %+v; an unroutable issue keeps its workspace", got)
	}
	// The human's assignment is never removed. Release only ever drops our
	// own principal, and here it has not even been attempted.
	assignees := h.issueAssignees("1")
	if !containsAll(strings.Join(assignees, ","), "a-human") {
		t.Errorf("assignees = %v, want the human's assignment untouched", assignees)
	}
}

// SPEC §9.1: the loop is the single mutator, and it stays correct under a
// storm of concurrent signals. Run under -race, this is the acceptance
// criterion for watcher/timer/runner contention.
func TestConcurrentSignalStorm(t *testing.T) {
	var issues []core.Issue
	for i := range 12 {
		issues = append(issues, fake.Issue(fmt.Sprint(i), epoch.Add(time.Duration(i)*time.Minute)))
	}
	h := start(t, harnessOpts{
		concurrency: "4",
		issues:      issues,
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt%3 == 0 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("s")
		},
	})

	var wg sync.WaitGroup
	// Ticks, reloads, and clock advances all at once.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				h.Clock.Advance(11 * time.Second)
				time.Sleep(time.Millisecond)
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Alternating clean and blocked, because both are Reload: the watcher
		// announces a raised block on the same call it announces an adoption,
		// and a block flipping under the loop is the storm's sharpest edge —
		// every flip re-versions the configuration and invalidates whatever
		// reads are out.
		for i := range 20 {
			var blocked error
			if i%2 == 1 {
				blocked = errors.New("workflow.md: unknown key `retries`")
			}
			h.Source.reload(h.def, blocked)
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 50 {
			h.o.Status()
			h.o.Transitions.Entries()
			time.Sleep(time.Millisecond)
		}
	}()
	wg.Wait()

	// The only assertion that matters here is that -race stayed quiet and the
	// loop is still answering.
	h.o.Status()
}

// SPEC §9.8 retries an unconfirmed stop on later ticks — which needs a handle
// to retry it on. The real claude-code handle closes its event stream even
// when termination is unconfirmed, so clearing the handle on stream close
// would leave the retry with nothing to call.
func TestUnconfirmedStopKeepsTheHandleAfterTheStreamCloses(t *testing.T) {
	h := start(t, harnessOpts{
		issues:          []core.Issue{fake.Issue("1", epoch)},
		hang:            true,
		script:          startedOnly,
		stopUnconfirmed: true,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the first stop attempt", func() bool {
		hd := h.Runner.LastHandle()
		return hd != nil && hd.StopCount() > 0
	})
	handle := h.Runner.LastHandle()
	// The answer before the tick, then the retry waited for, for the reasons the
	// sibling above states (#106's review). A retry that never comes fails here on
	// the handle it was owed, which is the defect this test exists for.
	h.waitStopApplied(1)
	before := handle.StopCount()

	h.Tick()
	waitFor(t, "the stop to be retried on the same handle after the stream closed", func() bool {
		return handle.StopCount() > before
	})

	// The retried stop's answer acknowledged first, the same rule one ladder later
	// (waitStopApplied).
	h.waitStopApplied(2)
	h.Runner.SetStopTermination(core.TerminationConfirmed)
	h.Tick()
	h.WaitGone("1")
}
