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

// The attempt-outcome record (#60). What is asserted here is that every
// dispatched attempt produces exactly one record, that the record describes
// *that* attempt rather than the record's running state, and that the four
// questions the ticket names are answerable from what it carries.

// testAgent is what every harness bundle declares it runs. Both fields are
// non-empty, so a record that carried the zero descriptor is visibly wrong
// rather than merely empty.
var testAgent = core.AgentDescriptor{Kind: "fake-agent", Model: "fake-model"}

func TestAPublishedAttemptRecordsItsWholeOutcome(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s1", Continuation: "s1"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 900, OutputTokens: 30, CostUSD: 0.25}},
				{Type: core.EventSucceeded},
			}
		},
	})
	h.WaitState("1", StateDone)
	waitFor(t, "the attempt outcome to be recorded", func() bool { return len(h.o.Attempts.For("1")) == 1 })

	o := h.o.Attempts.For("1")[0]
	if o.Attempt != 1 {
		t.Errorf("attempt = %d, want 1", o.Attempt)
	}
	if o.Agent != testAgent {
		t.Errorf("agent = %+v, want %+v", o.Agent, testAgent)
	}
	if !o.Ran {
		t.Error("ran = false, want true — a process was started")
	}
	if o.FailureReason != "" {
		t.Errorf("failure_reason = %q, want none", o.FailureReason)
	}
	if o.Verdict != VerdictPublished {
		t.Errorf("verdict = %s, want published", o.Verdict)
	}
	if want := (core.Usage{InputTokens: 900, OutputTokens: 30, CostUSD: 0.25}); o.Usage != want {
		t.Errorf("usage = %+v, want the run's own %+v", o.Usage, want)
	}
	if o.RunID == "" {
		t.Error("run_id is empty; nothing joins this record to its transcript or its §9.11 edges")
	}
	if o.StartedAt.Before(epoch) || o.EndedAt.Before(o.StartedAt) {
		t.Errorf("span = %s..%s, want a forward interval taken from the clock", o.StartedAt, o.EndedAt)
	}
}

// The trap this test exists for: Record.FailureReason is sticky across a retry
// by design, so a record that read it off the field would report the *previous*
// attempt's crash on the attempt that recovered from it — and the log would then
// hold a run that both failed and published.
func TestARetryRecordsTwoOutcomesAndTheSecondIsNotTheFirstsFailure(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return []core.Event{
					{Type: core.EventStarted, SessionID: "s1", Continuation: "s1"},
					{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.1}},
					{Type: core.EventFailed, Reason: core.FailureCrashed},
				}
			}
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s2", Continuation: "s2"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 500, OutputTokens: 20, CostUSD: 0.4}},
				{Type: core.EventSucceeded},
			}
		},
	})
	h.WaitState("1", StateBackoff)
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(11 * time.Second)
	h.WaitState("1", StateDone)
	waitFor(t, "both attempt outcomes to be recorded", func() bool { return len(h.o.Attempts.For("1")) == 2 })

	got := h.o.Attempts.For("1")
	if got[0].FailureReason != core.FailureCrashed || got[0].Verdict != VerdictUnknown {
		t.Errorf("attempt 1 = %s/%s, want crashed and no verdict", got[0].FailureReason, got[0].Verdict)
	}
	if got[1].FailureReason != "" {
		t.Errorf("attempt 2 failure_reason = %q, want none — that is attempt 1's, carried by a sticky field", got[1].FailureReason)
	}
	if got[1].Verdict != VerdictPublished {
		t.Errorf("attempt 2 verdict = %s, want published", got[1].Verdict)
	}
	// Usage is per attempt, unlike the §9.9 running total it is drawn from.
	if got[0].Usage.CostUSD != 0.1 || got[1].Usage.CostUSD != 0.4 {
		t.Errorf("costs = %v, %v; want each attempt's own rather than a running total",
			got[0].Usage.CostUSD, got[1].Usage.CostUSD)
	}
	if got[0].Attempt == got[1].Attempt {
		t.Errorf("both records claim attempt %d", got[0].Attempt)
	}
	if got[0].RunID == got[1].RunID {
		t.Errorf("both records claim run id %s", got[0].RunID)
	}
}

// An attempt that never launched is still an attempt: it spent a §9.6 budget,
// and a failure histogram that omitted the launch errors would look complete.
func TestAnAttemptThatNeverLaunchedIsStillRecorded(t *testing.T) {
	h := start(t, harnessOpts{
		issues:     []core.Issue{fake.Issue("1", epoch)},
		prepareErr: errors.New("worktree is locked"),
	})
	// `failed` releases the claim and the record leaves the machine, so the
	// outcome is what this waits on rather than a state that does not linger.
	waitFor(t, "the attempt outcome to be recorded", func() bool { return len(h.o.Attempts.For("1")) == 1 })
	if got := h.o.Transitions.Path("1"); !containsState(got, StateFailed) {
		t.Fatalf("path = %v, want it through failed", got)
	}

	got := h.o.Attempts.For("1")[0]
	if got.Ran {
		t.Error("ran = true, but no process was ever started")
	}
	if got.FailureReason != core.FailureLaunchError {
		t.Errorf("failure_reason = %q, want launch_error", got.FailureReason)
	}
	if got.Usage != (core.Usage{}) {
		t.Errorf("usage = %+v, want none", got.Usage)
	}
	if got.StartedAt.IsZero() {
		t.Error("started_at is zero; the attempt was dispatched and has a duration")
	}
}

// A breached §9.9 budget parks the run, and the record has to name why: it is
// the one failure reason the orchestrator raises rather than the agent.
func TestABudgetBreachIsRecordedAsBudgetExceeded(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		hang:        true,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 10, OutputTokens: 1, CostUSD: 2}},
			}
		},
	})
	h.WaitState("1", StateNeedsReview)
	waitFor(t, "the attempt outcome to be recorded", func() bool { return len(h.o.Attempts.For("1")) == 1 })

	got := h.o.Attempts.For("1")[0]
	if got.FailureReason != core.FailureBudgetExceeded {
		t.Errorf("failure_reason = %q, want budget_exceeded", got.FailureReason)
	}
	if got.Usage.CostUSD != 2 {
		t.Errorf("cost = %v, want the usage that breached the cap", got.Usage.CostUSD)
	}
}

// A run the daemon stopped because its issue went terminal is §7.3's `killed`,
// "deliberate stop" — not an empty reason, which is what a verification error
// records and means something else entirely.
func TestAnAttemptStoppedByAnExitIsRecordedAsKilled(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: startedOnly,
		hang:   true,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	h.WaitGone("1")
	waitFor(t, "the attempt outcome to be recorded", func() bool { return len(h.o.Attempts.For("1")) == 1 })

	got := h.o.Attempts.For("1")[0]
	if got.FailureReason != core.FailureKilled {
		t.Errorf("failure_reason = %q, want killed", got.FailureReason)
	}
	if !got.Ran {
		t.Error("ran = false, but the attempt had a live process when it was stopped")
	}
}

// The continuation track re-dispatches the same record, so each session needs a
// record of its own — that is what separates "one ticket, two sessions" from
// "one ticket, one session" in the aggregate.
func TestTheContinuationTrackRecordsEachSession(t *testing.T) {
	var mu sync.Mutex
	verdicts := []Verdict{VerdictIncomplete, VerdictPublished}
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			mu.Lock()
			defer mu.Unlock()
			v := verdicts[0]
			if len(verdicts) > 1 {
				verdicts = verdicts[1:]
			}
			return VerifyResult{Verdict: v, PRURL: "https://example.test/pull/1"}, nil
		}),
	})
	waitFor(t, "the first session's outcome", func() bool { return len(h.o.Attempts.For("1")) == 1 })
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the continuation timer was never armed")
	}
	h.Clock.Advance(11 * time.Second)
	h.WaitState("1", StateDone)
	waitFor(t, "both sessions' outcomes", func() bool { return len(h.o.Attempts.For("1")) == 2 })

	got := h.o.Attempts.For("1")
	if got[0].Verdict != VerdictIncomplete || got[1].Verdict != VerdictPublished {
		t.Errorf("verdicts = %s, %s; want incomplete then published", got[0].Verdict, got[1].Verdict)
	}
	if got[0].FailureReason != "" || got[1].FailureReason != "" {
		t.Errorf("reasons = %q, %q; a clean exit that has not published yet is not a failure",
			got[0].FailureReason, got[1].FailureReason)
	}
}

// A verification that could not be completed fails closed (SPEC §9.7), and the
// record says so by carrying neither a §7.3 reason nor a verdict: the agent did
// not fail, and nothing was concluded about what it produced.
func TestAVerificationErrorRecordsNeitherReasonNorVerdict(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{}, errors.New("origin unreachable")
		}),
	})
	h.WaitState("1", StateNeedsReview)
	waitFor(t, "the attempt outcome to be recorded", func() bool { return len(h.o.Attempts.For("1")) == 1 })

	got := h.o.Attempts.For("1")[0]
	if got.FailureReason != "" || got.Verdict != VerdictUnknown {
		t.Errorf("got %q/%s, want neither a reason nor a verdict", got.FailureReason, got.Verdict)
	}
}

// A drain ends the attempt it interrupts rather than pausing it: §9.10 resumes
// the *issue*, as a new attempt, which is why finishSuspended fires the §6.5
// after-run hook there. So the outcome is recorded — otherwise a SIGTERM costs
// the attempt's duration, its usage and its reason, on the one shutdown an
// operator is most likely to be investigating.
func TestADrainRecordsTheAttemptItInterrupts(t *testing.T) {
	sink := &recordingAttemptSink{}
	var h *harness
	h = start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("1", epoch)},
		attempts: sink,
		hang:     true,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 700, OutputTokens: 40, CostUSD: 0.9}},
			}
		},
		// The drain's interrupt would otherwise race the script's second event.
		stopGate: func() { holdStopUntilAccounted(h, 2)() },
	})
	h.WaitState("1", StateRunning)

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned")
	}
	h.Stop()

	got := h.o.Attempts.For("1")
	if len(got) != 1 {
		t.Fatalf("recorded %d outcomes for an interrupted attempt, want 1: %+v", len(got), got)
	}
	if got[0].FailureReason != core.FailureKilled {
		t.Errorf("failure_reason = %q, want killed", got[0].FailureReason)
	}
	if got[0].Verdict != VerdictUnknown {
		t.Errorf("verdict = %s; the drain drops the outcome rather than routing it", got[0].Verdict)
	}
	if !got[0].Ran || got[0].Usage.CostUSD != 0.9 {
		t.Errorf("record = %+v, want the run's own usage", got[0])
	}
	// And it reaches the disk: the flush after Run returns is what makes the
	// records of a shutdown — the ones somebody is about to go looking for —
	// complete.
	if entries := sink.entries(); len(entries) != 1 {
		t.Errorf("the sink received %d outcomes, want the interrupted attempt's", len(entries))
	}
}

// A launch still in flight when the signal lands has no handle *yet* — onStarted
// adopts it — so an outcome recorded on the absence of a handle alone freezes the
// row a moment before the very launch it describes: `ran=false` and no usage,
// about a process that ran and spent.
//
// Both halves are asserted here, because the row is only right if both hold: the
// record has to wait for the pending Start, and the adopted run's usage events
// have to be accounted even though a suspended record never enters `running`.
func TestADrainRecordsALaunchThatWasStillInFlight(t *testing.T) {
	release := make(chan struct{})
	inStart := make(chan struct{})
	var once bool
	var h *harness
	h = start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 300, OutputTokens: 20, CostUSD: 0.9}},
			}
		},
		// Holds the launch open across the shutdown signal, which is the window
		// the whole test is about: a Start that has been asked for and has not
		// answered. The gate is released only once the drain has begun.
		startGate: func() {
			if once {
				return
			}
			once = true
			close(inStart)
			<-release
		},
		// And holds the drain's interrupt until those two events have been
		// accounted, so the assertion below is about the record rather than
		// about which of two goroutines won. See holdStopUntilAccounted.
		stopGate: func() { holdStopUntilAccounted(h, 2)() },
	})
	<-inStart

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
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
	h.Stop()

	got := h.o.Attempts.For("1")
	if len(got) != 1 {
		t.Fatalf("recorded %d outcomes, want 1: %+v", len(got), got)
	}
	if !got[0].Ran {
		t.Error("ran = false, but the launch this row was frozen ahead of returned a live handle")
	}
	if got[0].Usage.CostUSD != 0.9 {
		t.Errorf("cost = %v, want the $0.90 the adopted run reported; a suspended record "+
			"stays in `preparing`, and its spend must be accounted anyway", got[0].Usage.CostUSD)
	}
	if got[0].Usage.InputTokens != 300 || got[0].Usage.OutputTokens != 20 {
		t.Errorf("tokens = %d/%d, want the adopted run's 300/20",
			got[0].Usage.InputTokens, got[0].Usage.OutputTokens)
	}
	if got[0].FailureReason != core.FailureKilled {
		t.Errorf("failure_reason = %q, want killed", got[0].FailureReason)
	}
}

// A run already in `running` when the signal lands stays in `running` for the
// whole drain, so a usage event arriving afterwards reaches §9.9's cap check
// against a record the drain owns. Acting on it there sets `stopping`, which
// makes orderedExit() true — and an ordered exit is skipped by the one branch of
// driveShutdown that records a drained attempt, permanently, since nothing
// clears `stopping` on the suspended path. The attempt then vanishes from the
// log entirely.
//
// The cap is deliberately crossed *after* suspension and not before: a breach
// while the daemon is running normally must still park the run, and that is
// TestBudgetBreachParksTheRun's job.
func TestABudgetCrossedDuringTheDrainDoesNotSwallowTheOutcome(t *testing.T) {
	suspended := make(chan struct{})
	var h *harness
	h = start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		hang:        true,
		// The drain's interrupt waits for all three events, so the record is
		// written after the cap has been crossed rather than racing it.
		stopGate: func() { holdStopUntilAccounted(h, 3)() },
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 100, OutputTokens: 10, CostUSD: 0.9}},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 50, OutputTokens: 5, CostUSD: 0.5}},
			}
		},
		// The third event — the one that crosses $1.00 — is held until the drain
		// has taken the record over. Nothing else can stage that: a slice of
		// events either all lands before the signal or all after it.
		eventGate: func(i int) {
			if i == 3 {
				<-suspended
			}
		},
	})
	h.WaitState("1", StateRunning)
	waitFor(t, "the first usage event to be accounted", func() bool { return h.applied(sigRunEvent) >= 2 })

	drained := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		drained <- h.o.Shutdown(ctx)
	}()
	h.waitDraining()
	close(suspended)

	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never returned")
	}
	h.Stop()

	got := h.o.Attempts.For("1")
	if len(got) != 1 {
		t.Fatalf("recorded %d outcomes, want 1 — a §9.9 breach during the drain must not take the "+
			"record out of driveShutdown's suspension branch: %+v", len(got), got)
	}
	if got[0].FailureReason != core.FailureKilled {
		t.Errorf("failure_reason = %q, want killed: the daemon stopped this attempt, and the budget "+
			"never got to park it", got[0].FailureReason)
	}
	// Accounted whether or not it was acted on — that is the whole point of
	// keeping the bookkeeping above the state gate.
	if got[0].Usage.CostUSD != 1.4 {
		t.Errorf("cost = %v, want $1.40: both usage events happened and were billed", got[0].Usage.CostUSD)
	}
}

// A record suspended before it ever launched has no process to wait on, so
// finishSuspended never reaches it. It is recorded at the suspension instead:
// §9.10 re-dispatches the issue, so this dispatch is over too.
func TestADrainRecordsAnAttemptSuspendedBeforeItLaunched(t *testing.T) {
	release := make(chan struct{})
	var once bool
	started := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
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
	h.Stop()

	got := h.o.Attempts.For("1")
	if len(got) != 1 {
		t.Fatalf("recorded %d outcomes, want the suspended dispatch's: %+v", len(got), got)
	}
	if got[0].Ran {
		t.Error("ran = true, but the drain landed before the launch")
	}
	if got[0].FailureReason != core.FailureKilled {
		t.Errorf("failure_reason = %q, want killed", got[0].FailureReason)
	}
}

// A reload that swaps the adapter reaches the *next* attempt's record, never the
// one already running. Reading the bundle at the outcome instead would attribute
// a finished run to a model configured after it finished, which is the one thing
// #62's comparison cannot survive.
func TestAReloadDoesNotRelabelALiveAttempt(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: startedOnly,
		hang:   true,
	})
	h.WaitState("1", StateRunning)

	// A new bundle, adapters and all, exactly as the watcher publishes one.
	next := *h.Bundle
	next.Agent = core.AgentDescriptor{Kind: "other-agent", Model: "other-model"}
	h.Source.publish(h.def, &next, nil)
	h.Tick()

	// End the run under the new configuration.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	h.WaitGone("1")
	waitFor(t, "the attempt outcome to be recorded", func() bool { return len(h.o.Attempts.For("1")) == 1 })

	if got := h.o.Attempts.For("1")[0].Agent; got != testAgent {
		t.Errorf("agent = %+v, want the one that launched it (%+v)", got, testAgent)
	}
}

// The durable half, on TransitionSink's contract: what the loop appends is what
// the sink is handed, in order, and the flush after Run returns is what makes
// the tail complete.
func TestOutcomesReachTheSinkInOrder(t *testing.T) {
	sink := &recordingAttemptSink{}
	h := start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("1", epoch)},
		attempts: sink,
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("s2")
		},
	})
	h.WaitState("1", StateBackoff)
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(11 * time.Second)
	h.WaitState("1", StateDone)
	h.Stop()

	got := sink.entries()
	if len(got) != 2 {
		t.Fatalf("the sink received %d outcomes, want 2: %+v", len(got), got)
	}
	if got[0].FailureReason != core.FailureCrashed || got[1].Verdict != VerdictPublished {
		t.Errorf("the sink received %+v, want the crash and then the publish", got)
	}
	if got[0].EndedAt.After(got[1].EndedAt) {
		t.Errorf("out of order: %s then %s", got[0].EndedAt, got[1].EndedAt)
	}
}

// holdStopUntilAccounted is a stopGate that keeps bounded teardown standing
// until the loop has applied `events` run events.
//
// It exists because the fake's script loop and the stop race *by design*: each
// send sits in a `select` against `stopped`, so a stop that lands mid-script
// drops the rest — which is the real adapter's behaviour, and a real outcome for
// an interrupted run. A test whose subject is what the record does with usage
// that *was* reported therefore has to put the two in order, or it asserts one
// side of a coin flip. It cost a CI failure that reproduced the very row this
// test was written to prevent.
//
// Deliberately no t.Fatal: this runs on the fake's Stop goroutine, where failing
// the test is not allowed. On timeout it simply lets the stop through, and the
// assertions report what actually happened.
func holdStopUntilAccounted(h *harness, events uint64) func() {
	return func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) && h.applied(sigRunEvent) < events {
			time.Sleep(time.Millisecond)
		}
	}
}

// recordingAttemptSink is the durable half, in memory.
type recordingAttemptSink struct {
	mu  sync.Mutex
	got []AttemptOutcome
}

func (s *recordingAttemptSink) Append(o AttemptOutcome) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, o)
	return nil
}

func (s *recordingAttemptSink) entries() []AttemptOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AttemptOutcome(nil), s.got...)
}
