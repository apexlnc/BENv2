package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// Shutdown is the one path where the safe direction is *inaction*: it stops
// dispatch, interrupts what is running, waits for confirmed termination, and
// then leaves everything else exactly as it found it (SPEC §9.8, §11 as amended
// 2026-08-12). So most of these assert an absence, and every one of them uses a
// barrier rather than a sleep — Shutdown's own return is the barrier for "the
// drain is complete", which is what makes an absence checked afterwards mean
// something.

// shutdown drains and fails the test rather than hanging.
func (h *harness) shutdown() {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.o.Shutdown(ctx); err != nil {
		h.t.Fatalf("Shutdown: %v (states: %+v)", err, h.o.Status())
	}
}

// The acceptance criterion, end to end: SIGTERM during an active run interrupts
// the agent, and the claim survives.
func TestShutdownInterruptsTheRunAndLeavesTheClaimStanding(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		// The agent is still going when the signal lands: `started` and then
		// nothing. A script that reported success would have left `running`
		// before the test got there, and the interrupt would be asserted against
		// a run that had already finished.
		script: startedOnly,
		hang:   true,
	})
	h.WaitState("7", StateRunning)

	h.shutdown()

	// Interrupt, not discard. Discard cuts the ladder's grace to a tenth and
	// aborts event emission, which truncates the verbatim record §7.2 requires
	// of a run that is being interrupted rather than thrown away.
	handle := h.Runner.LastHandle()
	if got := handle.Stops(); len(got) != 1 || got[0] != core.StopInterrupt {
		t.Errorf("stops = %v, want exactly one StopInterrupt", got)
	}

	// Nothing was released, disposed or projected onward. This is the whole of
	// the amendment: what has landed stays landed, and §9.10 resumes from it.
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release", n)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; shutdown disposes nothing", got)
	}
	if got := h.Tracker.Label("7"); got != core.StateLabelRunning {
		t.Errorf("state label = %q, want it left standing at %q for §9.10", got, core.StateLabelRunning)
	}
	if got := h.issueAssignees("7"); len(got) != 1 || got[0] != fake.DefaultPrincipal {
		t.Errorf("assignees = %v, want the claim retained", got)
	}
	// And it did not route the run's outcome into a terminal state on the way
	// out: `failed`/`needs-review`/`done` are all projections shutdown must not
	// make.
	for _, s := range h.o.Transitions.Path("7") {
		if s == StateDone || s == StateFailed || s == StateNeedsReview {
			t.Errorf("path = %v; shutdown routed the run to a terminal state", h.o.Transitions.Path("7"))
			break
		}
	}
}

// An unconfirmed termination is exactly where shutdown must not exit: a process
// that may still hold the worktree is the one thing a restart cannot reason
// about (SPEC §9.8). The drain re-asks on every tick and returns only when the
// answer changes.
func TestShutdownWaitsForAConfirmedTerminationAndReProbes(t *testing.T) {
	h := start(t, harnessOpts{
		issues:          []core.Issue{fake.Issue("7", epoch)},
		script:          startedOnly,
		hang:            true,
		stopUnconfirmed: true,
	})
	h.WaitState("7", StateRunning)

	// confirmed is flipped by *this* goroutine, and the drain records what it saw
	// at the moment it returned. That is a happens-before the test controls, not
	// a poll of "has it returned yet": checking the channel after a barrier races
	// the send, and a drain that gave up on the first unconfirmed answer would
	// slip through whenever the send lost that race.
	var confirmed atomic.Bool
	type outcome struct {
		err           error
		beforeConfirm bool
	}
	done := make(chan outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := h.o.Shutdown(ctx)
		done <- outcome{err: err, beforeConfirm: !confirmed.Load()}
	}()

	// The drain has asked at least twice, proving that an unconfirmed answer did
	// not make it give up. Suspended runs are re-driven by driveShutdown after
	// every signal; unlike an ordered exit below, this path does not depend on
	// the next tick to start another ladder.
	h.waitStops(2)
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times while unconfirmed; §9.8 retains the claim", n)
	}

	// The group finally dies.
	confirmed.Store(true)
	h.Runner.SetStopTermination(core.TerminationConfirmed)
	h.Tick()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Shutdown: %v", got.err)
		}
		if got.beforeConfirm {
			t.Error("Shutdown returned while the process group was still unconfirmed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return once the termination was confirmed")
	}
}

// waitStops blocks until the run's handle has been asked to stop at least n
// times, which is the only fact that distinguishes "re-probed" from "asked once
// and gave up".
//
// **Asked, not answered**, and the difference is load-bearing. The fake records
// the call at entry — deliberately, so a test can catch a ladder standing in a
// gate — so n stops here does not mean n answers have reached the loop, and the
// nth may still be in flight when this returns. On an ordered exit, whose next
// ladder is driven only by a tick, the caller therefore needs
// `h.waitStopApplied(n)` as well: `beginStop` owns the handle's single slot and
// refuses while one is in flight, so a tick inside that window asks nothing.
// With a manual clock nothing later re-drives the ordered exit and the test hangs
// to its deadline on a fact nothing will produce (#116; the rule itself is stated
// once, on waitStopApplied). Suspended runs are different: driveShutdown is
// deferred after every signal and re-arms them immediately.
//
// For an ordered exit this helper is *less* exposed to that than a bare barrier,
// which is why the hazard is only the tick after it: the loop below advances the
// clock itself, so a tick wasted inside the in-flight window is recovered by the
// next iteration.
func (h *harness) waitStops(n int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if handle := h.Runner.LastHandle(); handle != nil && len(handle.Stops()) >= n {
			return
		}
		// Ordered exits retry on the tick. Suspended runs may re-arm sooner, but
		// advancing the clock is harmless and lets this helper serve both paths.
		if h.Clock.BlockUntilWaiters(1) {
			h.Clock.Advance(time.Duration(h.def.Config.Polling.IntervalMS) * time.Millisecond)
		}
		time.Sleep(time.Millisecond)
	}
	got := 0
	if handle := h.Runner.LastHandle(); handle != nil {
		got = len(handle.Stops())
	}
	h.t.Fatalf("the run was stopped %d times, want at least %d", got, n)
}

// "Stop dispatch" is the other half, and it is asserted as an absence over
// several ticks rather than at one instant: a daemon that merely dispatched
// slower would pass a single-moment check.
func TestShutdownStopsDispatch(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		script: startedOnly,
		hang:   true,
	})
	h.WaitState("7", StateRunning)

	h.shutdown()
	before := claimCalls(h)

	// The candidate appears *after* the drain, and there is a free slot for it.
	// Both matter: a fixture whose spare issue was queued from the start is one
	// the concurrency cap keeps out on its own, and it passes against a daemon
	// that never stopped dispatching — which is what the first two versions of
	// this test did.
	h.Tracker.Set(fake.Issue("9", epoch.Add(time.Minute)))

	for range 3 {
		h.Tick()
	}
	if got := claimCalls(h); got != before {
		t.Errorf("claims went from %d to %d after the drain; dispatch did not stop", before, got)
	}
	if h.stateOf("9") != "" {
		t.Errorf("issue 9 was dispatched during shutdown: %v", h.o.Status())
	}
}

// A launch already in flight when the signal lands is the sharp case: the handle
// must be adopted — a process nobody holds a handle for is a process nothing can
// stop — and then interrupted, never abandoned.
func TestShutdownDuringStartAdoptsTheHandleAndInterruptsIt(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		script: startedOnly,
		hang:   true,
		prepareGate: func() {
			if once {
				return
			}
			once = true
			close(started)
			<-release
		},
	})

	<-started
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	// Released only once the drain has begun. Closing it earlier would race the
	// shutdown signal itself, and the test would sometimes be asserting about an
	// ordinary launch.
	h.waitDraining()
	close(release)

	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned")
	}

	// Either the launch was gated before it happened, or it happened and was
	// interrupted. Both are correct; what is not correct is a process left
	// running. Asserted as the disjunction rather than by picking a winner,
	// because which side wins is a scheduling detail.
	if handle := h.Runner.LastHandle(); handle != nil {
		if got := handle.Stops(); len(got) == 0 {
			t.Error("a run was started during shutdown and never interrupted")
		} else if got[0] != core.StopInterrupt {
			t.Errorf("stops = %v, want StopInterrupt", got)
		}
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release", n)
	}
}

// A verdict is a route, and routing is what a drain must not do. The record
// stays in `verifying`, its label stands, and the evidence the verdict was read
// from — git and the tracker — outlives the process anyway.
func TestShutdownDoesNotRouteAVerdictItWasAlreadyWaitingOn(t *testing.T) {
	gate := make(chan struct{})
	reached := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			if !once {
				once = true
				close(reached)
				<-gate
			}
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
	})

	<-reached
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	h.waitDraining()
	close(gate)

	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned; it must wait for the verifier but not act on it")
	}

	for _, s := range h.o.Transitions.Path("7") {
		if s == StateDone {
			t.Fatalf("path = %v; shutdown published a verdict", h.o.Transitions.Path("7"))
		}
	}
	if got := h.Tracker.Milestones("7"); len(got) > 0 && got[len(got)-1] == core.MilestonePublished {
		t.Error("shutdown posted the publish milestone")
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; the `done` path disposes and shutdown does not take it", got)
	}
}

// The one thing shutdown *must* finish. §9.10 classifies from which label a
// transition removed, so a projection abandoned halfway leaves recovery reading
// a state this daemon never reached — the single failure a restart cannot
// repair by looking at git.
func TestShutdownLandsTheWritesItHadAlreadyOrdered(t *testing.T) {
	release := make(chan struct{})
	blocked := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		script: startedOnly,
		hang:   true,
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() {
				if once {
					return
				}
				once = true
				close(blocked)
				<-release
			})
		},
	})

	<-blocked
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()

	select {
	case <-drained:
		t.Fatal("Shutdown returned with a label write still owed")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned once the write could land")
	}
	if got := h.Tracker.Label("7"); got == core.StateLabelNone {
		t.Error("the state label never landed; §9.10 has nothing to classify from")
	}
}

// Shutdown is idempotent and every caller is released by one drain. A daemon
// whose supervisor sends SIGTERM twice must not start a second drain, and the
// second caller must not wait forever on an ack the first consumed.
func TestShutdownIsIdempotentAcrossCallers(t *testing.T) {
	h := start(t, harnessOpts{issues: []core.Issue{fake.Issue("7", epoch)}, script: startedOnly, hang: true})
	h.WaitState("7", StateRunning)

	const callers = 3
	errs := make(chan error, callers)
	for range callers {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs <- h.o.Shutdown(ctx)
		}()
	}
	for range callers {
		select {
		case err := <-errs:
			if err != nil {
				t.Fatalf("Shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a concurrent Shutdown caller was never released")
		}
	}
	if got := h.Runner.LastHandle().Stops(); len(got) != 1 {
		t.Errorf("stops = %v; three callers must drive one drain", got)
	}
}

// A shutdown with nothing running returns without touching the tracker, and
// without waiting on anything.
func TestShutdownWithNothingRunningIsClean(t *testing.T) {
	h := start(t, harnessOpts{})
	before, _, _ := h.Tracker.Snapshot()
	h.shutdown()
	after, _, _ := h.Tracker.Snapshot()
	if len(after) != len(before) {
		t.Errorf("shutdown issued %d tracker calls with nothing running", len(after)-len(before))
	}
	if !h.o.Draining() {
		t.Error("Draining() = false after a completed shutdown")
	}
}

// Draining is what tells an operator that an unmoving queue is a daemon on its
// way out rather than a daemon that is stuck — so it must be true from the
// moment dispatch stops, not only once the drain finishes.
func TestDrainingIsVisibleWhileTheDrainIsStillWaiting(t *testing.T) {
	h := start(t, harnessOpts{
		issues:          []core.Issue{fake.Issue("7", epoch)},
		script:          startedOnly,
		hang:            true,
		stopUnconfirmed: true,
	})
	h.WaitState("7", StateRunning)

	if h.o.Draining() {
		t.Fatal("Draining() = true before any signal")
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.o.Shutdown(ctx)
	}()

	h.waitStops(1)
	if !h.o.Draining() {
		t.Error("Draining() = false while the drain is waiting on an unconfirmed group")
	}
	h.Runner.SetStopTermination(core.TerminationConfirmed)
}

// claimCalls counts the claim writes the tracker has seen. The read is over
// recorded *writes* rather than over state, so a dispatch that claimed and then
// unwound is still counted.
func claimCalls(h *harness) int {
	calls, _, _ := h.Tracker.Snapshot()
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, "claim ") {
			n++
		}
	}
	return n
}

// waitDraining blocks until shutdown has been applied by the loop. It is the
// barrier a gated test needs: releasing a gate before the signal has landed
// would put the work *ahead* of the drain, which is a different scenario and
// usually the one that passes.
func (h *harness) waitDraining() {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if h.o.Draining() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatal("the drain never began")
}

// A Prepare in flight holds the drain open, and the workspace it produces is
// left prepared rather than launched into.
//
// Both halves matter and they pull in opposite directions. Returning while the
// worker is out abandons a worktree nobody owns; launching into it spends an
// attempt on a run the drain would immediately have to interrupt, and — worse —
// creates a process after the daemon has decided to stop creating them.
func TestShutdownWaitsForAPrepareAndThenLaunchesNothing(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		script: startedOnly,
		hang:   true,
		prepareGate: func() {
			if once {
				return
			}
			once = true
			close(reached)
			<-release
		},
	})

	<-reached
	var confirmed atomic.Bool
	type outcome struct {
		err          error
		beforeUnGate bool
	}
	done := make(chan outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := h.o.Shutdown(ctx)
		done <- outcome{err: err, beforeUnGate: !confirmed.Load()}
	}()

	h.waitDraining()
	// Everything the drain could possibly do it has now done, except wait: the
	// only outstanding thing is the Prepare this gate holds.
	confirmed.Store(true)
	close(release)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Shutdown: %v", got.err)
		}
		if got.beforeUnGate {
			t.Error("Shutdown returned with a Prepare still in flight; its workspace would have had no owner")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned after the Prepare completed")
	}

	if n := h.Runner.StartCount(); n != 0 {
		t.Errorf("started %d agent(s) after the drain began; shutdown creates no processes", n)
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release", n)
	}
}

// The fresh-claim evidence read is part of Prepare's asynchronous operation:
// it owns the prepared workspace until sigPrepared lands. A drain that begins
// while PrepareWithLocalFacts is still returning its pre-hook snapshot must
// therefore wait for the whole operation, then suspend ahead of both the
// attempt-floor mutation and the launch. This is the continuity seam shared by
// §9.6 and §9.8 (#94, #101).
func TestShutdownDuringAttemptFloorReadSuspendsWithoutLaunching(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	h := start(t, harnessOpts{
		issues:     []core.Issue{fake.Issue("7", epoch)},
		legacyBase: fake.DefaultBaseSHA,
		prepareFacts: func(core.Workspace) (core.LocalBranchFacts, error) {
			close(reached)
			<-release
			return core.LocalBranchFacts{Head: advancedHeadSHA, DescendsBase: true}, nil
		},
	})

	<-reached
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- h.o.Shutdown(ctx)
	}()
	h.waitDraining()

	select {
	case err := <-done:
		t.Fatalf("Shutdown returned while pre-hook local evidence was still in flight: %v", err)
	default:
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned after pre-hook local evidence completed")
	}

	if n := h.Runner.StartCount(); n != 0 {
		t.Errorf("started %d agent(s) after the drain began; shutdown creates no processes", n)
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release", n)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; the prepared workspace must remain attached for §9.10", got)
	}
	if got := h.Tracker.Label("7"); got != core.StateLabelClaimed {
		t.Errorf("state label = %q, want it left standing at %q", got, core.StateLabelClaimed)
	}
	status := h.o.Status()
	if len(status) != 1 || status[0].State != StatePreparing || status[0].Attempt != 1 {
		t.Errorf("status = %+v, want suspended preparing attempt 1", status)
	}
}

// Reconciliation must not re-decide a suspended record. The issue going terminal
// mid-drain is the case that matters: the ordinary rule is "stop the run, dispose
// the workspace, release" (SPEC §9.8), and every one of those three is something
// shutdown does not do.
//
// Left for §9.10 rather than handled here: the tracker still says the issue is
// closed at the next start, and recovery reads that with the whole table
// available to it, where a drain has one rule and no time.
func TestReconciliationDoesNotTearDownASuspendedRecord(t *testing.T) {
	h := start(t, harnessOpts{
		issues:          []core.Issue{fake.Issue("7", epoch)},
		script:          startedOnly,
		hang:            true,
		stopUnconfirmed: true, // keeps the drain open so ticks keep happening
	})
	h.WaitState("7", StateRunning)

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = h.o.Shutdown(ctx)
	}()
	h.waitDraining()

	// The world moves under the drain: a human closes the issue.
	h.Tracker.Mutate("7", func(i *core.Issue) { i.State = "closed" })
	h.waitStops(3) // three ticks' worth of reconciliation have now run

	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; a suspended record is not reconciliation's to exit", n)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; a suspended record is not reconciliation's to exit", got)
	}
	if got := h.Tracker.Label("7"); got != core.StateLabelRunning {
		t.Errorf("state label = %q, want it left standing at %q", got, core.StateLabelRunning)
	}
	h.Runner.SetStopTermination(core.TerminationConfirmed)
}

// An observation already in flight when the signal lands must not route the
// outcome it was asked about.
//
// This is what puts `suspended` inside exiting() rather than beside it. onProbed
// is gated on exiting(), and a confirmed probe is otherwise a licence to
// applyOutcome — which verifies, publishes, or re-dispatches a continuation, all
// of them things a drain must not do. The onStopped branch does not cover this:
// the probe is a different operation with a different landing site (#79).
func TestAProbeInFlightWhenTheSignalLandsRoutesNothing(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		// The stream closes with a terminal event, so an outcome is held; Done
		// stays open, so the quiescence question is a Probe rather than a Stop.
		holdDone: true,
		probeGate: func() {
			if once {
				return
			}
			once = true
			close(reached)
			<-release
		},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			t.Error("the drain routed a held outcome to verification")
			return VerifyResult{Verdict: VerdictContradicted}, nil
		}),
	})

	<-reached
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = h.o.Shutdown(ctx)
	}()
	h.waitDraining()
	close(release)

	// Two ticks' worth of loop turns after the probe landed: enough for a
	// routing bug to have routed.
	h.Tick()
	h.Tick()

	for _, st := range h.o.Transitions.Path("7") {
		if st == StateDone || st == StateNeedsReview || st == StateFailed {
			t.Fatalf("path = %v; the drain routed a held outcome", h.o.Transitions.Path("7"))
		}
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release", n)
	}
}

// A parked record has no handle, and that is what makes it the case where
// `suspended` has to be inside exiting() rather than merely checked at the
// stop.
//
// Every other suspended record is covered incidentally: the drain issues a stop
// for anything holding a handle, and `stopInFlight` then keeps reconciliation,
// the probe path and the conversion out on its own. A `needs-review` record has
// nothing to stop, so nothing sets that flag — and §9.8's unpark rule fires on
// the very next tick, projecting a label and scheduling a re-dispatch, in the
// middle of a shutdown that is supposed to be projecting nothing.
func TestAHumanUnparkDuringTheDrainIsNotActedOn(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no commits"}, nil
		}),
	})
	h.WaitState("7", StateNeedsReview)
	h.WaitEffects(1)

	// No handle anywhere, so the drain completes at once — and leaves the
	// daemon draining.
	h.shutdown()

	// A human clears the state label, which §9.8 would ordinarily read as a
	// re-queue.
	h.Tracker.Mutate("7", func(i *core.Issue) { i.Labels = []string{"ben-queue"} })
	for range 3 {
		h.Tick()
	}

	if got := h.stateOf("7"); got != StateNeedsReview {
		t.Errorf("state = %q after an unpark during the drain, want it left at %q", got, StateNeedsReview)
	}
	if got := h.o.Transitions.Path("7"); containsState(got, StateBackoff) {
		t.Errorf("path = %v; the drain acted on a re-queue and scheduled new work", got)
	}
	if n := h.Runner.StartCount(); n != 1 {
		t.Errorf("started %d runs; the drain re-dispatched", n)
	}
}

// Finding 1 (#101 review). Stopping the dispatch *reads* is not stopping
// dispatch. A Fetch already in flight when the signal landed returns
// afterwards — possibly long afterwards, since it is exactly the read a wedged
// tracker holds — and the claim, the label and the milestone all follow from
// it. The gate has to be on the write.
func TestAFetchInFlightWhenTheSignalLandsDispatchesNothing(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		script: startedOnly,
		hang:   true,
		beforeStart: func(tr *fake.Tracker) {
			tr.SetFetchGate(func() {
				if once {
					return
				}
				once = true
				close(reached)
				<-release
			})
		},
	})

	<-reached
	// A candidate the held fetch will report once it is let go.
	h.Tracker.Set(fake.Issue("9", epoch.Add(time.Minute)))

	h.shutdown()
	before := claimCalls(h)
	close(release)

	// Three ticks after the drain returned: the fetch has landed and anything it
	// was going to do has been done.
	for range 3 {
		h.Tick()
	}
	if got := claimCalls(h); got != before {
		t.Errorf("claims went from %d to %d; a fetch released after the drain still dispatched", before, got)
	}
	if h.stateOf("9") != "" {
		t.Errorf("issue 9 was claimed by a fetch that outlived the drain: %v", h.o.Status())
	}
}

// Finding 2 (#101 review). A Prepare that *failed* routes through failAttempt,
// which transitions to backoff or failed and then releases — a terminal
// projection and a release, from a path the launch gate never touched.
func TestAFailedPrepareDuringTheDrainDoesNotTerminalize(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues:     []core.Issue{fake.Issue("7", epoch)},
		prepareErr: errors.New("worktree add failed"),
		prepareGate: func() {
			if once {
				return
			}
			once = true
			close(reached)
			<-release
		},
	})

	<-reached
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	h.waitDraining()
	close(release)

	if err := <-drained; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for _, st := range h.o.Transitions.Path("7") {
		if st == StateFailed || st == StateBackoff || st == StateNeedsReview {
			t.Errorf("path = %v; a failed Prepare terminalized during the drain", h.o.Transitions.Path("7"))
			break
		}
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release", n)
	}
}

// Finding 3 (#101 review). The amendment says shutdown *completes* the effects
// already ordered — and a release reconciliation decided before the signal is
// one of them. Suspending the record instead strands a claim the daemon had
// already resolved to let go.
func TestAReleaseOrderedBeforeTheSignalStillCompletes(t *testing.T) {
	releaseStop := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		script: startedOnly,
		hang:   true,
		// The exit's ladder stands in this gate, so the signal lands while the
		// ordered exit is still in flight. Without it the stop confirms and the
		// release lands before shutdown is even called, and the test passes
		// against an implementation that suspends ordered exits — which is how
		// the first version of it did.
		stopGate: func() { <-releaseStop },
	})
	h.WaitState("7", StateRunning)

	// The issue goes terminal, so §9.8 orders stop → dispose → release.
	h.Tracker.Mutate("7", func(i *core.Issue) { i.State = "closed" })
	h.PollNow()
	waitFor(t, "the exit's stop", func() bool {
		hd := h.Runner.LastHandle()
		return hd != nil && hd.StopCount() >= 1
	})

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	h.waitDraining()
	close(releaseStop)

	if err := <-drained; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if n := h.Tracker.ReleaseCount("7"); n != 1 {
		t.Errorf("released %d times, want 1: the release was ordered before the signal", n)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the ordered disposal to have completed", got)
	}
}

// Finding 4 (#101 review). A held claim owns no process and no workspace, so
// nothing about it looks like work in flight — but a history read gated behind a
// slow tracker can land mid-drain and reach a release verdict.
func TestAHeldClaimVerdictReachedDuringTheDrainReleasesNothing(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("7", epoch)},
		verifier: alwaysPublished,
	})
	h.WaitState("7", StateDone)
	waitFor(t, "the held claim", func() bool { return h.o.HeldCount() == 1 })

	// The sweep will see a moved revision and spend a history read; that read is
	// what the gate holds, and a `closed` event inside the cycle is what would
	// otherwise settle a release.
	h.Tracker.SetHistoryGate(func() {
		if once {
			return
		}
		once = true
		close(reached)
		<-release
	})
	// **Open**, with a moved revision. A closed issue is settled by the list
	// response alone and never spends a history read, so the first version of
	// this test skipped itself — and a skip is a test that does not test. The
	// close-and-reopen case is the one §9.8 reads the log for, and it is the one
	// that can reach a verdict mid-drain.
	h.Tracker.AppendHistory("7", core.ClaimEvent{Kind: core.ClaimEventClosed, Actor: "human"})
	h.Tracker.Mutate("7", func(i *core.Issue) { i.Revision = "moved" })
	h.PollNow()

	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("the sweep never spent a history read, so this fixture proves nothing")
	}

	// The drain waits for the read it found in flight, so the gate has to be
	// released from *inside* the drain rather than after it — releasing
	// afterwards is a deadlock, and it is the shape the first version of this
	// test had.
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	h.waitDraining()
	close(release)

	if err := <-drained; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	for range 3 {
		h.Tick()
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release, held claims included", n)
	}
}

// #135. The drain waits on a releasing held claim, and it is right to: the
// release was ordered before the signal, so completing it is what §9.8 as amended
// asks of shutdown. But an issue deleted after that order makes the write
// unlandable, and a write's own failure can never say so (#134) — so the entry
// could never be dropped and the drain would run until TimeoutStopSec ended it.
//
// The confirming Get resolves it, under the same at-most-one-per-tick bound: it
// completes an effect already ordered rather than initiating a new release, which
// is what makes it something a departing daemon may still spend (§8.5).
func TestShutdownResolvesAnUnlandableReleaseOrderedBeforeTheDrain(t *testing.T) {
	h := doneHarness(t, 1)

	// Ordered before the drain, and refused once.
	h.Tracker.SetFailRelease(errors.New("503 from the tracker"))
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	waitFor(t, "the release to be ordered and refused", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// And now it can never land.
	h.Tracker.Delete("1")
	h.Tracker.SetFailRelease(nil)

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	h.waitDraining()

	// Driven from here rather than waited on: the confirmation is a per-tick
	// offer like every other, and the poll cycle is what carries it. Shutdown's
	// own return is the barrier that says the drain completed.
	h.tickUntil("the unlandable release to resolve", func() bool { return h.o.HeldCount() == 0 })
	if err := <-drained; err != nil {
		t.Fatalf("Shutdown: %v (held claims: %d)", err, h.o.HeldCount())
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times for an issue the tracker no longer has", n)
	}
}

// Finding 5 (#101 review). Stop answers about the process *group*; Done
// additionally means the process is reaped and its transcript flushed and
// closed. Exiting on the group answer alone truncates the verbatim record of the
// run the daemon has just interrupted (SPEC §7.2).
func TestShutdownWaitsForTheTranscriptToClose(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		script: startedOnly,
		hang:   true,
		// The group goes quiet on the interrupt while the record is still being
		// written — the ordinary harness's shape, and the one a confirmed Stop
		// says nothing about.
		holdDone: true,
	})
	h.WaitState("7", StateRunning)

	var flushed atomic.Bool
	type outcome struct {
		err           error
		beforeFlushed bool
	}
	done := make(chan outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := h.o.Shutdown(ctx)
		done <- outcome{err: err, beforeFlushed: !flushed.Load()}
	}()

	// The interrupt lands and the group is confirmed gone; Done is still open.
	waitFor(t, "the interrupt", func() bool {
		hd := h.Runner.LastHandle()
		return hd != nil && hd.StopCount() >= 1
	})

	flushed.Store(true)
	h.Runner.LastHandle().ReleaseDone()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Shutdown: %v", got.err)
		}
		if got.beforeFlushed {
			t.Error("Shutdown returned before the run's transcript closed; the record would be truncated")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned after the transcript closed")
	}
}

// Finding 6 (#101 review). §5.2.6 owes after_run to every attempt that ran a
// process, whatever the outcome, and an interrupted attempt is over rather than
// paused — §9.10 resumes the issue as a *new* attempt. It is bounded by
// hooks.timeout_ms, which is what makes it something a drain can wait for.
func TestAnInterruptedAttemptStillFiresAfterRun(t *testing.T) {
	h := start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("7", epoch)},
		script:   startedOnly,
		hang:     true,
		withHook: true,
	})
	h.WaitState("7", StateRunning)

	h.shutdown()

	if got := h.Hooked.AfterRunCount("7"); got != 1 {
		t.Errorf("after_run fired %d times for an interrupted attempt, want 1 (SPEC §5.2.6)", got)
	}
	// And the drain waited for it: the hook rides the owed queue, so a drain
	// that returned first would have left it unfired above.
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; the hook is not a release", n)
	}
}

// Finding 7 (#101 re-review). A launch that *failed* answers with a nil handle,
// and the drain branch was pumping it: `handle.Events()` on a nil interface is a
// SIGSEGV that takes the whole daemon down on the way out.
//
// `ranThisAttempt` is the same mistake in slower form — it promises §6.5 an
// after-run hook over a workspace no agent ever ran in.
func TestAFailedStartDuringTheDrainDoesNotPumpANilHandle(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	var once bool
	h := start(t, harnessOpts{
		issues:    []core.Issue{fake.Issue("7", epoch)},
		failStart: errors.New("exec: claude: not found"),
		withHook:  true,
		startGate: func() {
			if once {
				return
			}
			once = true
			close(reached)
			<-release
		},
	})

	<-reached
	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	h.waitDraining()
	// The launch answers (nil, err) into a draining loop.
	close(release)

	if err := <-drained; err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	// A launch error is a §7.3 verdict, and routing it is a terminal projection.
	for _, st := range h.o.Transitions.Path("7") {
		if st == StateFailed || st == StateBackoff {
			t.Errorf("path = %v; a failed launch was routed during the drain", h.o.Transitions.Path("7"))
			break
		}
	}
	// No process ever ran in that workspace, so nothing is owed to it.
	if got := h.Hooked.AfterRunCount("7"); got != 0 {
		t.Errorf("after_run fired %d times for an attempt that never started a process", got)
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release", n)
	}
}

// Finding 8 (#101 re-review). driveShutdown deliberately does not re-arm an
// ordered exit — retryPendingExits drives those on the tick — so between ticks
// `stopInFlight` is false and it is `!groupGone` that has to hold the drain
// open. A reaped leader with surviving descendants is exactly that shape: Done
// has closed, and the group has not gone.
// The drain waits out a run-marker removal that is *executing*.
//
// Waiting for it is what the synchronous version gave for free, and losing it is not
// cosmetic. The markers cleared on this path carry no evidence — a drain's own
// interrupt, a launch that never happened — so one that survives the exit is read as
// unknown_launch at the next start, and §9.10 parks for a human an issue it would
// otherwise have resumed by itself.
//
// In flight only, never merely owed: a removal is one bounded local write, so this
// waits for at most one per clear, where waiting on a clear that keeps *failing* would
// hold the daemon open for as long as the state directory stayed unwritable — the
// trade §9.8 refuses everywhere else.
func TestTheDrainWaitsOutAMarkerRemovalAlreadyExecuting(t *testing.T) {
	clearing := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	// The drain's own interrupt is what confirms the group gone, and confirming it is
	// what clears the marker (suspendStopped). That removal wedges here.
	h.Workspaces.SetMarkerClearGate(func() {
		once.Do(func() { close(clearing) })
		<-release
	})

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- h.o.Shutdown(ctx)
	}()

	waitFor(t, "the drain's interrupt to reach the marker removal", func() bool {
		select {
		case <-clearing:
			return true
		default:
			return false
		}
	})

	// Ticks land while the removal is provably parked in the gate. Each is exactly
	// the event a drain re-examines itself on, so a drain that did not wait would have
	// completed on one of them.
	for range 3 {
		h.Tick()
	}
	select {
	case err := <-done:
		t.Fatalf("Shutdown returned (%v) while a run-marker removal was still executing; a truncated "+
			"removal leaves an evidence-less marker that parks the issue at the next start (SPEC §9.10)", err)
	default:
	}

	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown never returned once the removal landed")
	}
	if _, ok := h.Workspaces.RunMarkerFor("1"); ok {
		t.Error("the marker survived a completed drain; every gracefully stopped workspace would then read " +
			"as unknown_launch at the next start")
	}
}

func TestAnOrderedExitWithALiveGroupHoldsTheDrainOpen(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("7", epoch)},
		// The process ends and is reaped, so Done closes...
		script: func(core.RunSpec, int) []core.Event { return fake.Succeed("session-1") },
		// ...while its group outlives it, so only a confirmed Stop can settle it.
		descendants:     true,
		stopUnconfirmed: true,
	})
	h.WaitState("7", StateVerifying)

	// The issue goes terminal: §9.8 orders stop → dispose → release, and the
	// stop comes back unconfirmed.
	h.Tracker.Mutate("7", func(i *core.Issue) { i.State = "closed" })
	h.PollNow()
	waitFor(t, "the exit's stop", func() bool {
		hd := h.Runner.LastHandle()
		return hd != nil && hd.StopCount() >= 1
	})

	var confirmed atomic.Bool
	type outcome struct {
		err           error
		beforeConfirm bool
	}
	done := make(chan outcome, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		err := h.o.Shutdown(ctx)
		done <- outcome{err: err, beforeConfirm: !confirmed.Load()}
	}()

	// Two more ticks' worth of retries, each answering unconfirmed — and the
	// third answer acknowledged, so the tick below drives a fourth ladder rather
	// than landing inside the third's in-flight window (waitStopApplied).
	h.waitStops(3)
	h.waitStopApplied(3)

	confirmed.Store(true)
	h.Runner.SetStopTermination(core.TerminationConfirmed)
	h.Tick()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Shutdown: %v", got.err)
		}
		if got.beforeConfirm {
			t.Error("Shutdown returned while an ordered exit's process group was still live")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Shutdown did not return once the group was confirmed gone")
	}
	// And the exit it was waiting on completed, rather than being suspended.
	if n := h.Tracker.ReleaseCount("7"); n != 1 {
		t.Errorf("released %d times, want 1", n)
	}
}
