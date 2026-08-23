package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// A lost claim must leave the issue exactly as it was found. §9.2's trigger
// for queued → claimed is "Claim() verified by read-back", and that
// transition projects ben:claimed — so projecting before the claim verifies
// would leave a state label on an issue we do not own, and §8.3 excludes any
// issue carrying one. The loser of a race would block it for everyone.
func TestLostClaimProjectsNothing(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.ClaimVerified = func(string) bool { return false }
		},
	})
	h.WaitGone("1")

	calls, labels, comments := h.Tracker.Snapshot()
	if got, ok := labels["1"]; ok {
		t.Errorf("projected %q for a claim that never verified; §8.3 would then block the issue for everyone", got)
	}
	if got := comments["1"]; len(got) != 0 {
		t.Errorf("commented %v on an issue we do not own", got)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "label ") || strings.HasPrefix(c, "comment ") {
			t.Errorf("wrote %q for an unverified claim; calls = %v", c, calls)
		}
	}
	if got := h.o.Transitions.For("1"); len(got) != 0 {
		t.Errorf("logged %v for a claim that never landed", got)
	}
}

// A record with a Prepare or Start in flight may not be forgotten: the
// worker's result would land with nobody to own the workspace it created or
// the process it launched.
func TestExitWaitsForInFlightPrepare(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once

	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(*fake.Tracker) {},
		prepareGate: func() { once.Do(func() { <-release }) },
	})

	// The first Prepare is blocked. Close the issue and tick: reconciliation
	// wants to exit, but must not drop the record yet.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	if h.stateOf("1") == "" {
		close(release)
		t.Fatal("the record was forgotten while Prepare was still running; its workspace would leak")
	}

	// Let Prepare finish. The workspace it produces must be disposed of, not
	// abandoned.
	close(release)
	h.WaitGone("1")

	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the workspace the late Prepare produced to be cleaned up", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want 1", n)
	}
}

// SPEC §9.8 asks whether the issue is still *ours*, not whether it has
// exactly one assignee. A human who unassigns BEN and takes the issue leaves
// one assignee and no claim.
func TestRoutableChecksAssigneeIdentity(t *testing.T) {
	tests := []struct {
		name      string
		assignees []string
		want      bool
	}{
		{"our claim alone", []string{fake.DefaultPrincipal}, true},
		{"case-insensitive", []string{strings.ToUpper(fake.DefaultPrincipal)}, true},
		{"a human replaced us", []string{"a-human"}, false},
		{"a human joined us", []string{fake.DefaultPrincipal, "a-human"}, false},
		{"nobody", nil, false},
	}
	o := idleOrchestrator(t, fake.NewTracker())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := core.Issue{Identifier: "1", State: "open", Labels: []string{"ben-queue"}, Assignees: tt.assignees}
			if got := o.routable(o.configNow(), issue); got != tt.want {
				t.Errorf("routable(%v) = %v, want %v", tt.assignees, got, tt.want)
			}
		})
	}
}

// The same, end to end: a human who takes the issue over is noticed and the
// run is stopped, workspace kept.
func TestHumanReplacingTheClaimStopsTheRun(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = []string{"a-human"} })
	h.Tick()
	h.WaitGone("1")

	if got := h.Workspaces.Disposals("1"); len(got) != 1 || !got[0].Keep {
		t.Errorf("disposals = %+v, want the workspace kept for an unroutable issue", got)
	}
}

// SPEC §9.8: the adapter states absence with core.ErrIssueNotFound. Treating
// it as a failed read would leave a deleted issue running forever.
func TestDeletedIssueIsNotTreatedAsATransientFailure(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.Delete("1")
	h.Tick()
	h.WaitGone("1")

	// And released *nothing*. This used to assert one release, which encoded the
	// old behaviour rather than the requirement: the adapter answers 404 for an
	// issue the tracker no longer has, an owed write that errors is retried every
	// tick, and a record whose release never lands is never forgotten — so the
	// ask that looked like tidiness was what kept a deleted issue's slot for the
	// life of the process. The claim died with the issue; there is nothing to
	// drop.
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
	// The workspace still goes: forgetting must not drop the effects queued ahead
	// of it (finishNow owes the disposal first).
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %v, want the deleted issue's workspace disposed", got)
	}
}

// The forget is *owed*, not immediate, and this is the difference.
//
// A disposal that fails is retried from the head of the owed queue until it
// lands (owed.go), and the record has to outlive it to be the thing retrying.
// Forgetting the moment the issue is found gone drops the queue: the in-flight
// disposal still runs, so a passing one hides the bug entirely, and only a
// *failing* one shows that nothing is left to retry it. The worktree would leak
// silently until §9.10's next startup sweep.
func TestAGoneIssueStillRetriesItsDisposal(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)
	// Persistently, not once: the assertion is that the record keeps retrying,
	// and an exact disposal count would be measuring the tick cadence instead.
	h.Workspaces.SetFailDispose(errors.New("worktree busy"), false)

	h.Tracker.Delete("1")
	h.Tick()

	// Retained across the failure — it is the thing that owes the retry.
	tried := len(h.Workspaces.Disposals("1"))
	if tried == 0 {
		t.Fatal("the deleted issue's workspace was never disposed")
	}
	if got := h.stateOf("1"); got == "" {
		t.Fatal("the record was forgotten while its disposal was still owed; nothing is left to retry it")
	}

	h.Tick()
	if got := len(h.Workspaces.Disposals("1")); got <= tried {
		t.Errorf("disposals = %d, want the failed one retried on the next tick (was %d)", got, tried)
	}
	if got := h.stateOf("1"); got == "" {
		t.Fatal("the record left while its workspace was still there")
	}

	// And it leaves once the workspace actually goes.
	h.Workspaces.SetFailDispose(nil, false)
	h.Tick()
	h.WaitGone("1")
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
}

// Tracker writes still owed when the issue vanishes are discarded one by one as
// they reach the head, and the local work beside them still runs.
//
// The distinction is the whole of the rule, and a mistake in either direction is
// silent: keeping a tracker write blocks everything behind it forever, and
// dropping a local one leaks a worktree and skips the §6.5 hook with no failed
// effect to say so.
//
// The fixture leaves a `ben:running` projection permanently failing, so the run
// finishes and parks behind it — at the moment the deletion is noticed the record
// owes that write, the park's own label and comment, the after-run hook, and then
// its cleanup.
func TestAGoneIssueDiscardsTrackerWritesAndKeepsLocalWork(t *testing.T) {
	h := start(t, harnessOpts{
		issues:   []core.Issue{fake.Issue("1", epoch)},
		withHook: true,
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no evidence"}, nil
		}),
		beforeStart: func(tr *fake.Tracker) {
			// ben:claimed still lands — the record cannot reach `running` without
			// it — and ben:running never does, so it stays at the head while
			// everything the run owes afterwards queues behind it.
			tr.FailLabel = func(_ string, label core.StateLabel) error {
				if label == core.StateLabelRunning {
					return errors.New("502 from the tracker")
				}
				return nil
			}
		},
	})
	h.WaitState("1", StateNeedsReview)

	h.Tracker.Delete("1")
	h.Tick()
	h.WaitGone("1")

	// The local half ran.
	if n := h.Hooked.AfterRunCount("1"); n != 1 {
		t.Errorf("after_run hook fired %d time(s), want 1: it was queued behind the writes that had to be discarded", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %v, want the deleted issue's workspace disposed", got)
	}
	// And the tracker half left nothing standing to retry.
	if n := h.Tracker.ReleaseAttempts("1"); n != 0 {
		t.Errorf("attempted %d release(s) on an issue the tracker no longer has", n)
	}
}

// One cleanup sequence, however many ticks pass — the other half of `gone` being
// an ordered exit.
//
// Asserted on the owed queue because nothing else can see it. A record that
// reconciliation keeps re-deciding appends a fresh disposal and forget every
// tick, and *none of the extras ever run*: the forget at the front drops the
// record and the rest die with it. So the disposal count is one per tick either
// way, and the only visible symptom is a queue that grows without bound on a
// record that can sit for hours behind a failing disposal.
func TestAGoneIssueOwesOneCleanupSequence(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)
	// Held failing, so the record stays put and every tick gets its chance to
	// queue another round.
	h.Workspaces.SetFailDispose(errors.New("worktree busy"), false)

	h.Tracker.Delete("1")
	for range 4 {
		h.Tick()
	}

	got := h.owedAfterStop("1")
	want := []string{"dispose workspace", effectForget}
	if !slices.Equal(got, want) {
		t.Errorf("owed = %v, want exactly %v: reconciliation re-decided a record it had already resolved", got, want)
	}
}

// A genuine read failure is the opposite case and must stay transient:
// everything keeps running and the refresh is retried.
func TestRefreshFailureKeepsEverythingRunning(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.SetFailGet(errors.New("502 from the tracker"))
	h.Tick()

	if got := h.stateOf("1"); got != StateRunning {
		t.Errorf("state = %q after a failed refresh, want it still running", got)
	}
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times on a failed read", n)
	}
}

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

// A release the tracker refused is retried; the record — and therefore the
// claim — is not forgotten on the strength of one failed write.
func TestFailedReleaseIsRetried(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureAuth) },
		beforeStart: func(tr *fake.Tracker) {
			tr.SetFailRelease(errors.New("503 from the tracker"))
		},
	})
	h.WaitState("1", StateFailed)

	waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })
	if h.stateOf("1") == "" {
		t.Fatal("the record was forgotten before the release succeeded; the claim would be stranded")
	}

	h.Tracker.SetFailRelease(nil)
	h.Tick()
	h.WaitGone("1")
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("succeeded releases = %d, want 1", n)
	}
}

// SPEC §9.4 step 2 is *defensive revalidation*, not a flag read: a watch that
// missed an event would otherwise dispatch stale configuration forever.
func TestPreflightRevalidatesAndAdoptsTheNewDefinition(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	var next *config.WorkflowDefinition

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			mu.Lock()
			defer mu.Unlock()
			calls++
			return h.publishDef(next, nil)
		},
	})

	waitFor(t, "preflight to run", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls > 0
	})

	// A reload the watch missed, discovered by revalidation.
	reloaded := definition(t, "3", "  max_retry_backoff_ms: 99000\n")
	mu.Lock()
	next = reloaded
	mu.Unlock()

	h.Tick()
	waitFor(t, "the revalidated definition to be adopted", func() bool {
		return h.o.MaxRetryBackoffMS() == 99000
	})
}

// The zero VerifyResult must never reach `done`. §9.7's "a verification that
// could not be completed is never success" was true of the *error* path only;
// a verifier that returned a zero value with a nil error published a run
// nobody checked, because VerdictPublished used to be iota. VerdictUnknown
// takes that slot now, so the irreversible verdict requires saying so.
//
// Signed off on #45, where verify's own enum was built this way and B08's
// disagreed: "Published is the irreversible outcome and must require explicit
// construction."
func TestTheZeroVerifyResultParksInsteadOfPublishing(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			// A verifier that forgot to say anything, and did not fail either.
			return VerifyResult{}, nil
		}),
	})
	h.WaitState("1", StateNeedsReview)

	// Every irreversible consequence of `done` is what this is protecting.
	if got := h.Tracker.Milestones("1"); containsMilestone(got, core.MilestonePublished) {
		t.Errorf("milestones = %v; a run nobody verified was announced as published", got)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposals = %+v; the workspace was thrown away on an unstated verdict", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNeedsReview {
		t.Errorf("label = %q, want ben:needs-review", got)
	}
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; the claim converted to a held `done` record", got)
	}

	// And the operator is told what actually happened. Falling through to the
	// contradicted arm would report evidence against an agent that was never
	// checked.
	_, _, comments := h.Tracker.Snapshot()
	var detail string
	for _, c := range comments["1"] {
		if c.Milestone == core.MilestoneNeedsReview {
			detail = c.Detail
		}
	}
	if !strings.Contains(detail, "no usable verdict") {
		t.Errorf("needs-review detail = %q, want it to name the unstated verdict", detail)
	}
	if strings.Contains(detail, "contradicts") {
		t.Errorf("needs-review detail = %q; nothing was checked, so nothing was contradicted", detail)
	}
}

// SPEC §9.8: a parked issue that a human closed is finished, not held. The
// terminal check has to come before the unpark check, or the claim and
// workspace are retained forever.
func TestClosedParkedIssueIsCleanedUp(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictContradicted, Detail: "no commits"}, nil
		}),
	})
	h.WaitState("1", StateNeedsReview)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	h.WaitGone("1")

	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want the claim dropped once the parked issue closed", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the workspace cleaned up", got)
	}
}

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

// ClaimPrincipal is required: without it §9.8 cannot tell our own claim from
// a human who replaced it, and the check would silently degrade to counting
// assignees.
func TestNewRequiresAClaimPrincipal(t *testing.T) {
	def := definition(t, "3", "")
	_, err := New(Config{Runtime: newTestSource(def, &Bundle{
		Definition: def,
		Tracker:    fake.NewTracker(),
		Workspaces: fake.NewWorkspaces(),
		Runner:     fake.NewRunner(),
		Verifier:   alwaysPublished,
	})})
	if err == nil {
		t.Fatal("New accepted an empty ClaimPrincipal")
	}
	if !strings.Contains(err.Error(), "ClaimPrincipal") {
		t.Errorf("error = %v, want it to name ClaimPrincipal", err)
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

// A tracker write the queue refused must stay owed, not vanish. Every write
// a record owes is evidence §9.10 will read back, so "attempted once, logged
// on failure" is not good enough — the release in particular would strand the
// claim.
//
// Driven against a constructed-but-not-running orchestrator: New starts no
// goroutines, so the effect queue can be saturated and drained by hand.
func TestRefusedTrackerWriteStaysOwed(t *testing.T) {
	tracker := fake.NewTracker(fake.Issue("1", epoch))
	o, _ := idleWithSource(t, tracker)

	r := &Record{Issue: fake.Issue("1", epoch), State: StateFailed}
	o.records["1"] = r

	// Saturate: nothing is consuming the queue, so the next enqueue is
	// dropped exactly as it would be under a wedged tracker.
	for len(o.effects) < cap(o.effects) {
		o.effects <- func(context.Context) {}
	}

	o.release(t.Context(), r, "failed")
	if !r.owesAnything() {
		t.Fatal("the release was dropped rather than kept owed; the claim would be stranded")
	}
	if r.owedInFlight {
		t.Fatal("marked in flight on an effect the queue refused; no completion signal will ever clear it")
	}
	if _, ok := o.records["1"]; !ok {
		t.Fatal("the record was forgotten while it still owed the tracker a write")
	}

	// With room again, the retry re-queues it.
	<-o.effects
	o.retryPendingExits(t.Context())
	if !r.owedInFlight {
		t.Fatal("the retry did not re-queue the owed write once the queue had room")
	}
}

// SPEC §9.10 re-dispatches "assigned, no ben:* label" at attempt 1 on the
// stated grounds that label projection precedes preparing, "so no attempt can
// have run". Starting the agent while the label write is still queued would
// make that false, and recovery would re-run an attempt it believes never
// happened.
func TestNoAttemptStartsBeforeTheClaimLabelLands(t *testing.T) {
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() { <-gate })
		},
	})

	waitFor(t, "the claim to verify", func() bool { return h.stateOf("1") == StateClaimed })
	// The label write is blocked. Nothing may have started.
	time.Sleep(20 * time.Millisecond)
	if n := h.Workspaces.PrepareCount("1"); n != 0 {
		t.Fatalf("prepared %d attempts before ben:claimed landed", n)
	}
	if n := h.Runner.StartCount(); n != 0 {
		t.Fatalf("started %d runs before ben:claimed landed — a crash here would look like §9.10's unprojected claim", n)
	}

	close(gate)
	h.WaitState("1", StateDone)
	if n := h.Runner.StartCount(); n != 1 {
		t.Errorf("started %d runs once the label landed, want 1", n)
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

// A runner event queued before a decision must not undo it: a crash report
// landing after a confirmed budget park would otherwise move needs-review
// back to backoff.
func TestLateRunnerEventCannotUndoAPark(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
				{Type: core.EventFailed, Reason: core.FailureCrashed},
			}
		},
	})
	h.WaitState("1", StateNeedsReview)

	time.Sleep(30 * time.Millisecond)
	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Errorf("state = %q; a late crash event undid the park", got)
	}
	if got := h.o.Transitions.Path("1"); containsState(got, StateBackoff) {
		t.Errorf("path = %v, want no backoff after a budget park", got)
	}
}

// A claim whose label write is still retrying can sit in claimed for several
// ticks. If the issue goes terminal in that window, the eventual projection
// must not start work on it.
func TestClaimProjectedAfterTheIssueClosedStartsNothing(t *testing.T) {
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) { tr.SetLabelGate(func() { <-gate }) },
	})
	waitFor(t, "the claim to verify", func() bool { return h.stateOf("1") == StateClaimed })

	// The issue closes while the projection is stuck.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	close(gate)
	h.WaitGone("1")

	if n := h.Workspaces.PrepareCount("1"); n != 0 {
		t.Errorf("prepared %d attempts for an issue that closed before the claim projected", n)
	}
	if n := h.Runner.StartCount(); n != 0 {
		t.Errorf("started %d runs on a closed issue", n)
	}
}

// SPEC §5.4 blocks *new dispatches* while a reload is invalid. A backoff or
// continuation re-dispatch is a new launch under exactly the configuration
// that failed to validate.
func TestBlockedReloadAlsoHoldsTimerRedispatch(t *testing.T) {
	var mu sync.Mutex
	var block error
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("s")
		},
		blocked: func() error {
			mu.Lock()
			defer mu.Unlock()
			return block
		},
	})
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}

	// The config goes invalid before the backoff fires.
	mu.Lock()
	block = errors.New("workflow reload failed")
	mu.Unlock()
	// PollNow, not Tick: advancing a whole poll interval would sweep past the
	// backoff timer before the block was even observed.
	h.PollNow()

	h.Clock.Advance(11 * time.Second)
	time.Sleep(40 * time.Millisecond)
	if n := h.Runner.StartCount(); n != 1 {
		t.Fatalf("started %d runs; a backoff re-dispatch must not launch under a config that failed to validate", n)
	}

	// Fixing it lets the retry through.
	mu.Lock()
	block = nil
	mu.Unlock()
	h.PollNow()
	h.Clock.Advance(20 * time.Second)
	h.WaitState("1", StateDone)
}

// SPEC §6.6 keeps the workspace for forensics and hands it back alongside the
// error. Dropping the reference would leak the directory: the record would
// have nothing to dispose when it later exits.
func TestWorkspaceReturnedWithAnErrorIsStillTracked(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		prepareErr:  errors.New("after_create hook failed"),
		prepRetry:   func(error) bool { return true },
		beforeStart: func(*fake.Tracker) {},
	})
	h.WaitState("1", StateBackoff)

	// The record knows about the workspace the failed Prepare left behind.
	var branch string
	for _, s := range h.o.Status() {
		if s.Identifier == "1" {
			branch = s.Branch
		}
	}
	if branch == "" {
		t.Fatal("the workspace returned alongside the error was discarded; nothing would ever dispose it")
	}

	// And it is disposed when the record exits.
	h.Tracker.Delete("1")
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(11 * time.Second)
	h.WaitGone("1")

	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the kept workspace disposed on exit", got)
	}
}

// SPEC §9.4 asks for defensive revalidation before each dispatch cycle, and a
// backoff re-dispatch is one. Trusting the last poll's verdict leaves a
// window a whole poll interval wide: here the config goes invalid *after* the
// last poll and before the timer fires, so only a self-revalidating timer
// path can see it.
func TestTimerRedispatchRevalidatesForItself(t *testing.T) {
	var mu sync.Mutex
	var block error
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("s")
		},
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			mu.Lock()
			defer mu.Unlock()
			return h.publishDef(nil, block)
		},
	})
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}

	// No poll between here and the timer firing — the last one saw a healthy
	// config.
	mu.Lock()
	block = errors.New("workflow reload failed")
	mu.Unlock()

	h.Clock.Advance(11 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if n := h.Runner.StartCount(); n != 1 {
		t.Fatalf("started %d runs; the timer path launched under a config it never revalidated", n)
	}
}

// A timer armed by an attempt that is being wound up must not start another
// run: the record is on its way out, and the claim may already be released.
// Driven through the continuation track, whose timer is armed while the
// record sits in verifying — a state reconciliation does revisit.
func TestStaleTimerCannotRestartAnExitingRecord(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Succeed("s") },
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			return VerifyResult{Verdict: VerdictIncomplete}, nil
		}),
		beforeStart: func(tr *fake.Tracker) {
			// The release never confirms, so the record stays exiting.
			tr.SetFailRelease(errors.New("503 from the tracker"))
		},
	})
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the continuation timer was never armed")
	}

	// The issue closes: reconciliation begins the exit while that timer is
	// still pending.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.PollNow()
	waitFor(t, "the exit to begin", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })

	// And then it reopens, so the re-fetch the timer does will find an issue
	// that looks perfectly dispatchable. Only the exiting guard stops it.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "open" })

	before := h.Runner.StartCount()
	h.Clock.Advance(2 * time.Second)
	time.Sleep(50 * time.Millisecond)

	if got := h.Runner.StartCount(); got != before {
		t.Errorf("started %d runs (was %d); a stale timer restarted a record whose release is still pending", got, before)
	}
}

// SPEC §5.2: run.previous_outcome is what the next attempt is told about the
// last one. A re-queue restores the budgets (§9.8) but must not rewrite the
// history — a budget-exceeded retry told "succeeded" would be working from a
// false account of why it is there.
func TestRequeueKeepsThePriorFailureInThePrompt(t *testing.T) {
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		extraConfig: "  max_cost_usd: 1.0\n",
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{CostUSD: 2.0}},
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
	waitFor(t, "the re-queued attempt to start", func() bool { return h.Runner.StartCount() == 2 })

	prompt := h.Runner.Prompts()[1]
	if strings.Contains(prompt, "succeeded") {
		t.Errorf("the re-queued attempt is told the previous outcome succeeded:\n%s", prompt)
	}
	if !strings.Contains(prompt, string(core.FailureBudgetExceeded)) {
		t.Errorf("the prompt does not name why the run was parked:\n%s", prompt)
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

// --- review round 7 ---

// SPEC §8.4 + §9.10 step 3: an error from Claim is not the same answer as a
// refusal, and only the refusal carries the unwind guarantee. The adapter's two
// riskiest paths — an unverifiable read-back and an unorderable race — both try
// to release and both return a *joined error* when that release also fails, so
// an error is precisely the case where an assignment may be standing.
//
// Forgetting there leaves assigned-with-no-state-label, which §9.10 step 3
// classifies as published-awaiting-review and never touches again: the issue is
// undispatchable by anyone, forever, with no PR behind it.
func TestAClaimThatErroredIsReleasedRatherThanForgotten(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetClaimError(errors.New("verifying claim: 502; unwinding unverifiable claim: 502"))
		},
	})

	waitFor(t, "the release of a claim that may have landed", func() bool {
		return h.Tracker.ReleaseCount("1") == 1
	})
	h.WaitGone("1")

	if got := h.issueAssignees("1"); len(got) != 0 {
		t.Errorf("assignees = %v; a failed claim stranded its assignment", got)
	}
	// Nothing was ever projected, so the issue is left exactly as it was found
	// — which is the shape a human, or this daemon's next tick, can pick up.
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Errorf("label = %q, want none for a claim that never verified", got)
	}
}

// The third answer, and the one the §8.5 request budget makes necessary: an
// error the adapter can prove it reached before writing anything
// (core.ErrClaimNotAttempted). There is no assignment to unwind, and the release
// is itself a write — so paying it here would spend the write capacity whose
// exhaustion is the usual reason for the refusal, and hold a §9.5 concurrency
// slot for an issue this daemon does not own.
func TestAClaimRefusedBeforeWritingIsNotReleased(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetClaimError(fmt.Errorf("%w: per-tick GitHub request budget spent", core.ErrClaimNotAttempted))
		},
	})
	h.WaitGone("1")

	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times; the adapter promised there was nothing to undo", n)
	}
	if got := h.issueAssignees("1"); len(got) != 0 {
		t.Errorf("assignees = %v; the refusal promised no assignment was written", got)
	}
	// Left exactly as it was found, so an ordinary poll can dispatch it again
	// once the budget or the server's Retry-After allows.
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Errorf("label = %q, want none for a claim that never began", got)
	}
}

// The other half, and the reason the two answers are handled separately: a
// claim the adapter *refused* has already been unwound (`false, nil` is it
// saying so), so releasing again would spend a request to undo nothing.
func TestARefusedClaimIsNotReleasedAgain(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.ClaimVerified = func(string) bool { return false }
		},
	})
	h.WaitGone("1")

	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times; the adapter had already unwound this claim (SPEC §8.4)", n)
	}
}

// SPEC §9.4 step 1: "Reconcile — always runs, even when validation fails." The
// guarantee has to survive a slow tracker, not just a failing config. Sharing
// one worker put reconciliation behind the candidate fetch, and sharing one
// in-flight flag put every *later* tick's reconciliation behind it too — so a
// single hung Fetch meant no closed, deleted or taken-over issue was ever
// noticed again, while its agent kept running against a workspace nobody was
// going to reclaim.
func TestReconciliationRunsWhileTheCandidateFetchHangs(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)

	// From here the candidate read never returns. The tick that wedges it must
	// come *before* the issue closes, or the wedging tick's own reconciliation
	// would notice the close and the fixture would prove nothing about later
	// ticks — which is where the failure actually lived.
	h.Tracker.SetFetchGate(func() { <-release })
	fetchesBefore := h.Tracker.FetchReads()
	h.PollNow()

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.PollNow()

	h.WaitGone("1")
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want the closed issue's claim dropped", n)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 {
		t.Errorf("disposals = %+v, want the workspace cleaned up", got)
	}

	// The fixture is only meaningful if the fetch really is stuck: exactly one
	// read went in and never came back.
	if got := h.Tracker.FetchReads() - fetchesBefore; got != 1 {
		t.Errorf("candidate reads = %d while the fetch was gated, want exactly the one that hung", got)
	}
	unblock()
}

// SPEC §9.6 gives the continuation token to the continuation track alone —
// "re-dispatch **with the continuation token**" is written of the clean exit
// and of nothing else. The failure track follows a session that crashed,
// stalled or timed out, and handing that one back to `--resume` re-enters the
// state that just failed. Its context arrives through the prompt instead:
// `attempt` and `run.previous_outcome`.
func TestTheFailureTrackDoesNotResumeTheSessionThatFailed(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return []core.Event{
					{Type: core.EventStarted, SessionID: "s1", Continuation: "s1"},
					{Type: core.EventFailed, Reason: core.FailureCrashed},
				}
			}
			return fake.Succeed("s2")
		},
	})
	h.WaitState("1", StateBackoff)

	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(2 * time.Minute)
	waitFor(t, "the retry", func() bool { return h.Runner.StartCount() == 2 })

	got := h.Runner.Continuations()
	if len(got) < 2 {
		t.Fatalf("continuations = %q, want two runs", got)
	}
	if got[1] != "" {
		t.Errorf("the retry resumed %q; a crashed session is not a session to resume", got[1])
	}
	// The retry is still told what happened — through the template surface
	// §9.6 names, not through the token.
	if prompts := h.Runner.Prompts(); len(prompts) < 2 || !strings.Contains(prompts[1], "previous outcome crashed") {
		t.Errorf("retry prompt does not carry the previous outcome: %q", prompts[len(prompts)-1])
	}
}

// SPEC §9.8: the anchor exists to date a *close event* against the current
// claim cycle. A terminal issue is not an event — it is the state now, and now
// needs no dating. Retaining the claim while the history read is retried defers
// a verdict the very next sweep would reach from a list response saying exactly
// the same thing (classifyHeld), and a claim held on an issue nobody will
// reopen is held forever.
func TestATerminalIssueReleasesEvenWithoutAClaimAnchor(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	// The log cannot say which event established the claim, and the issue has
	// gone terminal — the PR merged — while we were asking.
	h.Tracker.SetHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventLabeled, Subject: "ben-queue", ID: 9, At: epoch,
	})
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	close(gate)

	waitFor(t, "the release on the terminal fact", func() bool {
		return h.Tracker.ReleaseCount("1") == 1
	})
	h.WaitGone("1")
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; a record was built for a claim that was just released", got)
	}
}

// SPEC §9.5: "`limits.max_concurrent_agents` caps live agent processes — the
// one scarce resource." Verification is not one. It runs only after the process
// group is confirmed gone (§7.5), reads git and the tracker, and can be slow —
// it probes origin and calls FindPR. Counting it capped concurrency on
// something that is not executing, so a slow tracker starved dispatch entirely
// while no agent was running at all.
func TestVerificationDoesNotHoldAnAgentSlot(t *testing.T) {
	var entered atomic.Int32
	gate := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(unblock)

	h := start(t, harnessOpts{
		concurrency: "1",
		issues: []core.Issue{
			fake.Issue("1", epoch),
			fake.Issue("2", epoch.Add(time.Hour)),
		},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			entered.Add(1)
			<-gate
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
	})

	// Reaching the verifier at all means the run ended and its process group
	// was confirmed gone, so what follows is about the slot and not a race
	// with the agent still holding it.
	waitFor(t, "verification to begin", func() bool { return entered.Load() == 1 })
	if got := h.Runner.StartCount(); got != 1 {
		t.Fatalf("started %d runs with max_concurrent_agents=1", got)
	}

	h.PollNow()
	waitFor(t, "the next issue to dispatch", func() bool { return h.Runner.StartCount() == 2 })
	unblock()
}

// SPEC §5.4: a valid reload "applies to future dispatch, retry scheduling,
// reconciliation, hooks, and launches". Reconciliation happens on a tick and
// nowhere else, so an interval captured once at startup makes the operator's
// edit load, validate, log as adopted — and change nothing.
func TestAReloadedPollingIntervalTakesEffect(t *testing.T) {
	h := start(t, harnessOpts{})
	const startup = config.DefaultPollingIntervalMS * time.Millisecond
	const reloaded = 300 * time.Second

	// Slower, so the old interval firing a tick is observable as a failure
	// rather than as timing noise.
	carrier := h.Tracker.FetchReads()
	h.Source.reload(definition(t, "3", "polling:\n  interval_ms: 300000\n"), nil)

	// The reload wakes the ticker, so the tick that carries it in arrives with
	// no clock advance at all — and the wait the ticker abandoned to take it is
	// still registered here, which is why both waits below are asked for by
	// duration rather than by count. Advancing before the re-arm would spend
	// the new interval's clock against a waiter that did not exist yet, and the
	// test would then sit out the difference.
	waitFor(t, "the tick that carries the reload", func() bool { return h.Tracker.FetchReads() > carrier })
	waitFor(t, "the ticker to re-arm at the reloaded interval", func() bool { return hasWait(h.Clock, reloaded) })
	before := h.Tracker.FetchReads()

	// The old interval must no longer be enough.
	h.Clock.Advance(startup)
	time.Sleep(50 * time.Millisecond)
	if got := h.Tracker.FetchReads(); got != before {
		t.Errorf("the daemon ticked %d more times at the old interval; the reload never reached the ticker", got-before)
	}

	// The new one is.
	h.Clock.Advance(reloaded - startup)
	waitFor(t, "a tick at the reloaded interval", func() bool { return h.Tracker.FetchReads() > before })
}

// --- review round 8 ---

// The other direction, and the one that hurts. Publishing the new cadence only
// changes what the *next* wait is armed with; a ticker already asleep on a five
// minute interval honours it once more, so 5m → 1s takes five minutes to take
// effect — the case an operator hits when they shorten the interval precisely
// because they want the daemon to react sooner.
func TestAReloadToAFasterIntervalDoesNotWaitOutTheOldOne(t *testing.T) {
	h := start(t, harnessOpts{extraConfig: "polling:\n  interval_ms: 300000\n"})

	// The startup tick has been and gone; the ticker is asleep for five minutes.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker never armed")
	}
	before := h.Tracker.FetchReads()

	h.Source.reload(definition(t, "3", "polling:\n  interval_ms: 1000\n"), nil)

	// No clock movement at all: the sleep is abandoned, not waited out.
	waitFor(t, "a tick without advancing past the old interval", func() bool {
		return h.Tracker.FetchReads() > before
	})

	// And the cadence that follows is the reloaded one.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker did not re-arm after the reload")
	}
	after := h.Tracker.FetchReads()
	h.Clock.Advance(time.Second)
	waitFor(t, "a tick at the reloaded interval", func() bool { return h.Tracker.FetchReads() > after })
}

// SPEC §9.4 orders reconcile (step 1) before dispatch (steps 2–3), and the
// order carries information: reconciliation is what frees slots. Run
// concurrently, the candidate fetch can answer first and dispatch then decides
// against a world reconciliation is about to change.
//
// Asserted on the read itself rather than on a slot handoff, because that is
// the part the ordering actually determines. An exit reconciliation begins here
// finishes on its own signals, and its record keeps its slot until it does —
// §9.5 working as intended, since a process that may still be alive is still a
// live agent process.
func TestTheCandidateFetchWaitsForReconciliation(t *testing.T) {
	hold := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(unblock)

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)

	// Reconciliation's reads are now held open. There is a tracked issue, so
	// the tick has a Get to make and cannot skip step 1.
	var gets atomic.Int32
	h.Tracker.SetGetGate(func() {
		if gets.Add(1) == 1 {
			<-hold
		}
	})
	before := h.Tracker.FetchReads()
	h.o.send(t.Context(), signal{kind: sigTick})

	waitFor(t, "reconciliation to start reading", func() bool { return gets.Load() == 1 })
	time.Sleep(50 * time.Millisecond)
	if got := h.Tracker.FetchReads(); got != before {
		t.Errorf("the candidate read began while reconciliation was still out (%d fetches); §9.4 puts step 1 first", got-before)
	}

	unblock()
	waitFor(t, "the candidate read once reconciliation lands", func() bool {
		return h.Tracker.FetchReads() > before
	})
}

// The closed map has to be closed from both directions. `!from.Terminal()` was
// true of every value that is not one of the two terminal states — including
// near misses of them — so a typo'd or future state silently acquired a legal
// edge into failed, and §9.2's "loud error, not a no-op" would never fire for
// the case that most needs it.
func TestTheKillEdgeRefusesStatesTheMapDoesNotKnow(t *testing.T) {
	for _, s := range []State{"unknown", "", "DONE", "verifying ", "Needs-Review"} {
		t.Run(string(s), func(t *testing.T) {
			if Allowed(s, StateFailed) {
				t.Errorf("Allowed(%q → failed) = true; membership is the question, not non-membership of the complement", s)
			}
			if Allowed(s, StateBackoff) {
				t.Errorf("Allowed(%q → backoff) = true", s)
			}
			if Allowed(StateRunning, s) {
				t.Errorf("Allowed(running → %q) = true", s)
			}
		})
	}
}

// --- review round 9 ---

// Reload is not the only way a definition reaches the loop. §5.4's defensive
// revalidation is there to catch the reload a missed watch event dropped —
// editor atomic saves are exactly what makes that likely — and it arrives
// through adopt, which published the cadence without waking anything. A
// 5m → 1s change discovered that way still waited out the five minutes.
func TestAPreflightReloadWakesTheTickerToo(t *testing.T) {
	slow := "polling:\n  interval_ms: 300000\n"
	fast := definition(t, "3", "polling:\n  interval_ms: 1000\n")

	var serveFast atomic.Bool
	h := start(t, harnessOpts{
		extraConfig: slow,
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			if serveFast.Load() {
				return h.publishDef(fast, nil)
			}
			return h.publishDef(nil, nil)
		},
	})
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker never armed")
	}

	before := h.Tracker.FetchReads()
	serveFast.Store(true)

	// One tick, no clock movement. Its revalidation finds the newer definition,
	// and the wait armed for five minutes must not survive it.
	h.PollNow()
	waitFor(t, "the tick the preflight reload woke", func() bool {
		return h.Tracker.FetchReads() >= before+2
	})

	// The other half, and the reason the wake is conditional: preflight returns
	// a definition on every tick whether or not anything moved. Waking on each
	// would tick, revalidate, adopt, wake and tick again — a spin bounded by
	// the tracker's latency rather than by the poll interval.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the ticker did not re-arm at the reloaded cadence")
	}
	settled := h.Tracker.FetchReads()
	time.Sleep(150 * time.Millisecond)
	if got := h.Tracker.FetchReads(); got != settled {
		t.Errorf("the daemon ticked %d more times without the clock moving; an unchanged definition woke the ticker", got-settled)
	}
}

// The fake recomputes §8.3's verdict after a release, and it had omitted two of
// the five conditions in as many review rounds — first the label partition,
// then open blockers. Both omissions have one consequence: an issue the daemon
// has just decided is not its work, and released for that reason, is handed
// straight back as eligible and re-run.
//
// Enumerated rather than patched a third time. The adapter computes all five
// per read (github eligibleIgnoringBlockers + hasOpenBlocker), so a fake that
// answers from fewer is not a fake of it, and the next omission should fail a
// test rather than reach a review.
func TestTheFakeRecomputesEveryDispatchableCondition(t *testing.T) {
	blocker := func(open bool) core.Blocker {
		return core.Blocker{Identifier: "9", State: map[bool]string{true: "open", false: "closed"}[open], Open: open}
	}
	for _, tc := range []struct {
		name  string
		after func(*core.Issue)
		want  bool
	}{
		{name: "nothing in the way", after: func(*core.Issue) {}, want: true},
		{name: "a closed blocker is not in the way", after: func(i *core.Issue) {
			i.Blockers = []core.Blocker{blocker(false)}
		}, want: true},
		{name: "an open blocker", after: func(i *core.Issue) { i.Blockers = []core.Blocker{blocker(true)} }},
		{name: "another party still assigned", after: func(i *core.Issue) {
			i.Assignees = append(i.Assignees, "a-human")
		}},
		{name: "issue no longer open", after: func(i *core.Issue) { i.State = "closed" }},
		{name: "required labels gone", after: func(i *core.Issue) { i.Labels = nil }},
		{name: "a ben:* state label stands", after: func(i *core.Issue) {
			i.Labels = append(i.Labels, "ben:needs-review")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tr := fake.NewTracker(fake.Issue("1", epoch))
			// As the tracker looks mid-run: claimed by us, and then whatever
			// the case is about.
			tr.Mutate("1", func(i *core.Issue) {
				i.Assignees = []string{fake.DefaultPrincipal}
				i.Dispatchable = false
				tc.after(i)
			})
			if err := tr.Release(t.Context(), issueFixture("1")); err != nil {
				t.Fatalf("Release: %v", err)
			}

			got, err := tr.Fetch(t.Context())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if len(got) != 1 {
				t.Fatalf("fetched %d issues, want 1", len(got))
			}
			if got[0].Dispatchable != tc.want {
				t.Errorf("dispatchable = %v, want %v — the released issue's §8.3 verdict",
					got[0].Dispatchable, tc.want)
			}
		})
	}
}

// --- review round 10 ---

// §5.4 reasons about reloads arriving. This is the one direction it never
// contemplates: a reload being *undone*.
//
// The dispatch reads revalidate the config and then fetch, and the fetch can
// outlast a human's edit. Delivered unconditionally, the definition the
// revalidation captured before the edit is re-adopted on arrival — rolling the
// daemon back to a configuration that has already been replaced, with no event
// to say so. Its candidates go the same way: selected under the old label
// partition and active states, they are the old definition's answer to "what is
// BEN's?" (§8.3) being spent on the new definition's slots.
func TestAStalePreflightResultDoesNotRollBackANewerReload(t *testing.T) {
	const (
		oldBackoff = 111000
		newBackoff = 222000
	)
	before := definition(t, "3", "  max_retry_backoff_ms: 111000\n")
	after := definition(t, "3", "  max_retry_backoff_ms: 222000\n")

	// hold releases the superseded read; parked keeps its replacement out of
	// the way, since a discard now asks for one immediately and that
	// replacement is entitled to dispatch.
	hold, parked := make(chan struct{}), make(chan struct{})
	var onceHold, onceParked sync.Once
	unblock := func() { onceHold.Do(func() { close(hold) }) }
	release := func() { onceParked.Do(func() { close(parked) }) }
	t.Cleanup(func() { unblock(); release() })
	var reads atomic.Int32

	var edited atomic.Bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
		// The file as revalidation finds it: the old definition until the human
		// saves, the new one after.
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			if edited.Load() {
				return h.publishDef(after, nil)
			}
			return h.publishDef(before, nil)
		},
	})
	waitFor(t, "the startup revalidation", func() bool { return h.o.MaxRetryBackoffMS() == oldBackoff })
	h.WaitState("1", StateRunning)

	// A tick whose revalidation captures the old definition and then wedges in
	// the fetch.
	h.Tracker.SetFetchGate(func() {
		if reads.Add(1) == 1 {
			<-hold
			return
		}
		<-parked
	})
	h.PollNow()

	// The human saves, and the watcher's reload is adopted while that fetch is
	// still out.
	edited.Store(true)
	h.Source.reload(after, nil)
	h.PollNow()
	waitFor(t, "the reload to be adopted", func() bool { return h.o.MaxRetryBackoffMS() == newBackoff })

	// Something for the superseded read to dispatch, or the candidate half of
	// this test proves nothing: issue 1 is already tracked, so a stale list
	// carrying only that one would be declined for a reason that has nothing to
	// do with the reload.
	h.Tracker.Set(fake.Issue("2", epoch.Add(time.Hour)))

	// The overtaken read lands.
	starts := h.Runner.StartCount()
	unblock()
	time.Sleep(150 * time.Millisecond)

	if got := h.o.MaxRetryBackoffMS(); got != newBackoff {
		t.Errorf("max_retry_backoff_ms = %d, want %d; a read that began before the reload undid it", got, newBackoff)
	}
	if got := h.Runner.StartCount(); got != starts {
		t.Errorf("dispatched %d runs from candidates the reload superseded", got-starts)
	}

	// Not vacuous: the replacement cycle reads config and candidates together,
	// and dispatches the very issue the superseded read was holding.
	release()
	waitFor(t, "the replacement cycle to dispatch", func() bool { return h.Runner.StartCount() > starts })
	if got := h.o.MaxRetryBackoffMS(); got != newBackoff {
		t.Errorf("max_retry_backoff_ms = %d after a fresh cycle, want %d", got, newBackoff)
	}
}

// --- review round 11 ---

// The timer track re-fetches and revalidates on its own, and then *launches*.
// §5.4 says a reload applies to launches, so a re-fetch that began before the
// human saved must not be the thing that starts the next attempt: adopting the
// definition it captured rolls the daemon back, and preparing under it runs an
// agent on a configuration already replaced.
//
// Rearmed rather than dropped — the wait is not the record's fault, and a
// dropped timer strands it in backoff with nothing left to fire.
func TestASupersededTimerRefetchRearmsInsteadOfLaunching(t *testing.T) {
	const oldBackoff, newBackoff = 111000, 222000
	before := definition(t, "3", "  max_retry_backoff_ms: 111000\n")
	after := definition(t, "3", "  max_retry_backoff_ms: 222000\n")

	hold := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(unblock)

	var edited, gated atomic.Bool
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureCrashed) },
		revalidate: func(_ context.Context, h *harness) config.Snapshot[*Bundle] {
			if edited.Load() {
				return h.publishDef(after, nil)
			}
			return h.publishDef(before, nil)
		},
	})
	waitFor(t, "the startup revalidation", func() bool { return h.o.MaxRetryBackoffMS() == oldBackoff })
	h.WaitState("1", StateBackoff)

	// A record in backoff is neither refreshed nor swept by reconciliation (§9.8
	// covers running records and parked ones), so the only Get from here is the
	// timer's own.
	h.Tracker.SetGetGate(func() {
		if gated.CompareAndSwap(false, true) {
			<-hold
		}
	})
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(2 * time.Minute)
	waitFor(t, "the re-fetch to start", func() bool { return gated.Load() })

	// The human saves while that re-fetch is wedged. Reload notes it inline, so
	// the read is superseded the moment this returns.
	edited.Store(true)
	h.Source.reload(after, nil)

	unblock()
	time.Sleep(150 * time.Millisecond)
	if got := h.Runner.StartCount(); got != 1 {
		t.Errorf("started %d runs; the attempt launched from a re-fetch the save superseded", got)
	}

	// Not stranded, and not rolled back: the rearmed timer fires, and the
	// retry it launches runs under the definition now in force.
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the superseded timer did not rearm")
	}
	h.Clock.Advance(10 * time.Minute)
	waitFor(t, "the rearmed retry", func() bool { return h.Runner.StartCount() == 2 })
	waitFor(t, "the retry to run under the reloaded definition", func() bool {
		return h.o.MaxRetryBackoffMS() == newBackoff
	})
}

// Staging is when work started under the old definition stops being valid: the
// human has saved. Waiting for the loop to adopt leaves a window as long as a
// poll interval — and when the cadence did not change there is no wake to
// shorten it — in which a wedged candidate read still counts as current and
// dispatches under a configuration already replaced.
func TestAReloadInvalidatesReadsBeforeTheLoopAdoptsIt(t *testing.T) {
	const newBackoff = 222000
	after := definition(t, "3", "  max_retry_backoff_ms: 222000\n")

	// One gate per candidate read: the first is the one the save supersedes,
	// the second is the replacement cycle the reload wakes. Holding them apart
	// is what separates "the superseded read did not dispatch" — the property
	// — from "nothing dispatched", which is no longer true and should not be:
	// a reload is supposed to bring the next cycle forward.
	superseded, replacement := make(chan struct{}), make(chan struct{})
	var onceA, onceB sync.Once
	openSuperseded := func() { onceA.Do(func() { close(superseded) }) }
	openReplacement := func() { onceB.Do(func() { close(replacement) }) }
	t.Cleanup(func() { openSuperseded(); openReplacement() })
	var reads atomic.Int32

	h := start(t, harnessOpts{})
	h.Tracker.SetFetchGate(func() {
		if reads.Add(1) == 1 {
			<-superseded
			return
		}
		<-replacement
	})
	h.PollNow()
	waitFor(t, "the first candidate read to wedge", func() bool { return reads.Load() == 1 })

	// Something for the wedged read to dispatch, and a save that supersedes it.
	// The cadence is unchanged, so nothing about the *interval* wakes the
	// ticker: only the staging itself can invalidate the read.
	h.Tracker.Set(fake.Issue("1", epoch))
	h.Source.reload(after, nil)

	openSuperseded()
	waitFor(t, "the replacement cycle the reload woke", func() bool { return reads.Load() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := h.Runner.StartCount(); got != 0 {
		t.Errorf("dispatched %d runs from a read the human's save superseded", got)
	}

	// Not vacuous: the replacement cycle dispatches the same issue, under the
	// definition the loop has since adopted.
	openReplacement()
	waitFor(t, "the replacement cycle to dispatch", func() bool { return h.Runner.StartCount() == 1 })
	if got := h.o.MaxRetryBackoffMS(); got != newBackoff {
		t.Errorf("max_retry_backoff_ms = %d, want the reloaded %d", got, newBackoff)
	}
}

// The revision advance rule now belongs to the one writer, and is asserted there
// (config.TestAnUnchangedFileDoesNotAdvanceTheRevision,
// config.TestARepeatedFailureRefreshesTheWordingWithoutReversioning). What used
// to be tested here — a compare-and-write the loop performed against its own copy
// of the configuration — has no counterpart: there is one cell, the loop does not
// write it, and a read that arrives stale is discarded rather than recorded. The
// tests that pin *that* are TestAStalePreflightResultDoesNotRollBackANewerReload
// and TestAReloadInvalidatesReadsBeforeTheLoopAdoptsIt, which drive it through
// the loop rather than through a helper.

// The boundary as one table: every read that carries a configuration, against
// every kind of configuration event that can land while it is out.
//
// The three reads are the ones that revalidate and then *act* — the candidate
// poll (§9.4 steps 2–3) and the two timer tracks (§9.6), which re-fetch and
// then launch. The three events are what §5.4 distinguishes: a valid reload, a
// validation that has started failing, and a revalidation that found nothing
// new. The rule is one line — a read may be applied only if the configuration
// has not moved since it began — and the table is what says it holds on every
// path rather than on the one the last review happened to name.
func TestConfigurationBoundaryAcrossReadsAndEvents(t *testing.T) {
	invalid := errors.New("workflow.md: unknown key `retries`")

	// Each read is set up so that exactly one run start can follow it: the
	// poll dispatches a queued issue, and each timer track launches its next
	// attempt. So "did the superseded read act?" is one number either way.
	type track struct {
		name  string
		setup func(t *testing.T, h *harness) // leaves a read gated on the tracker's Get/Fetch
	}

	for _, tr := range []track{
		{
			name: "candidate poll",
			setup: func(t *testing.T, h *harness) {
				h.Tracker.Set(fake.Issue("2", epoch.Add(time.Hour)))
				h.PollNow()
			},
		},
		{
			name: "backoff re-fetch",
			setup: func(t *testing.T, h *harness) {
				h.WaitState("1", StateBackoff)
				if !h.Clock.BlockUntilWaiters(1) {
					t.Fatal("the backoff timer was never armed")
				}
				h.Clock.Advance(2 * time.Minute)
			},
		},
		{
			name: "continuation re-fetch",
			setup: func(t *testing.T, h *harness) {
				h.WaitState("1", StateVerifying)
				if !h.Clock.BlockUntilWaiters(1) {
					t.Fatal("the continuation timer was never armed")
				}
				h.Clock.Advance(2 * time.Second)
			},
		},
	} {
		for _, ev := range []struct {
			name    string
			apply   func(t *testing.T, h *harness, next *config.WorkflowDefinition)
			applied bool // may the superseded read still act?
		}{
			{
				name:  "a valid reload",
				apply: func(_ *testing.T, h *harness, next *config.WorkflowDefinition) { h.Source.reload(next, nil) },
			},
			{
				name: "validation starts failing",
				// Shaped exactly as the watcher delivers it: a failed reload
				// keeps the last-known-good definition and reports the block, so
				// what arrives is the *standing* definition paired with an error
				// — the one transition that announces itself with nothing new to
				// adopt.
				apply: func(_ *testing.T, h *harness, _ *config.WorkflowDefinition) { h.Source.reload(h.def, invalid) },
			},
			{
				name:    "revalidation found nothing new",
				apply:   func(*testing.T, *harness, *config.WorkflowDefinition) {},
				applied: true,
			},
		} {
			t.Run(tr.name+"/"+ev.name, func(t *testing.T) {
				// hold releases the read under test; parked keeps every later
				// read out of the way. A transition wakes the ticker on
				// purpose — the replacement cycle is the point — so without
				// parking, "did the superseded read act?" would be answered by
				// its replacement.
				hold, parked := make(chan struct{}), make(chan struct{})
				var onceHold, onceParked sync.Once
				unblock := func() { onceHold.Do(func() { close(hold) }) }
				t.Cleanup(func() { unblock(); onceParked.Do(func() { close(parked) }) })
				var gated atomic.Bool

				verdict := VerdictIncomplete
				h := start(t, harnessOpts{
					issues: []core.Issue{fake.Issue("1", epoch)},
					script: func(_ core.RunSpec, attempt int) []core.Event {
						if tr.name == "backoff re-fetch" && attempt == 1 {
							return fake.Fail(core.FailureCrashed)
						}
						return fake.Succeed("s")
					},
					verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
						return VerifyResult{Verdict: verdict}, nil
					}),
				})
				if tr.name == "candidate poll" {
					h.WaitState("1", StateVerifying)
				}

				// Wedge this track's read. Both timer tracks re-fetch by Get,
				// and neither state is refreshed by reconciliation, so the gate
				// catches the read under test and nothing else.
				gate := func() {
					if gated.CompareAndSwap(false, true) {
						<-hold
						return
					}
					<-parked
				}
				if tr.name == "candidate poll" {
					h.Tracker.SetFetchGate(gate)
				} else {
					h.Tracker.SetGetGate(gate)
				}
				tr.setup(t, h)
				waitFor(t, "the read under test to wedge", func() bool { return gated.Load() })
				starts := h.Runner.StartCount()

				ev.apply(t, h, definition(t, "3", "  max_retry_backoff_ms: 222000\n"))
				unblock()
				time.Sleep(150 * time.Millisecond)

				acted := h.Runner.StartCount() > starts
				if acted != ev.applied {
					t.Errorf("the read acted = %v, want %v: a read begun under one configuration may be applied only if it has not moved",
						acted, ev.applied)
				}
			})
		}
	}
}

// Discarding a superseded read is only half of it. The record is still in
// backoff with its wait consumed, so a new one is armed — and §5.4 hands retry
// scheduling to the reload, so the new wait must be the new definition's.
//
// It compounds if it is not: the re-fetch that follows *this* wait is liable to
// be superseded too, and each time it re-arms the ceiling the operator has
// already replaced. An edit cutting five minutes to a millisecond would then
// never take hold at all, because the only path that ever adopts it is the one
// being skipped.
func TestASupersededReFetchReArmsUnderTheNewCeiling(t *testing.T) {
	hold := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(hold) }) }
	t.Cleanup(unblock)
	var gated atomic.Bool

	// The default ceiling is five minutes, so attempt 1 waits ~10s.
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("s")
		},
	})
	h.WaitState("1", StateBackoff)

	// Wedge the backoff re-fetch. Nothing else reads by Get here: a record in
	// backoff is not one reconciliation refreshes.
	h.Tracker.SetGetGate(func() {
		if gated.CompareAndSwap(false, true) {
			<-hold
		}
	})
	if !h.Clock.BlockUntilWaiters(1) {
		t.Fatal("the backoff timer was never armed")
	}
	h.Clock.Advance(2 * time.Minute)
	waitFor(t, "the backoff re-fetch to wedge", func() bool { return gated.Load() })

	// The operator cuts the ceiling while the read is out, then it returns.
	h.Source.reload(definition(t, "3", "  max_retry_backoff_ms: 1\n"), nil)
	unblock()

	// Asserted on the wait itself rather than by advancing the clock: any
	// advance large enough to fire the *old* ten-second wait would also fire
	// the new one, so the two are only distinguishable by asking how long was
	// asked for. A millisecond ceiling jitters to at most 1.2ms; the ticker's
	// own wait is thirty seconds, so nothing else can satisfy this.
	waitFor(t, "the re-fetch to re-arm under the new ceiling", func() bool {
		return hasWaitWithin(h.Clock, 2*time.Millisecond)
	})
	if got := h.stateOf("1"); got != StateBackoff {
		t.Errorf("state = %q, want %q: the superseded re-fetch should have waited again, not acted", got, StateBackoff)
	}
}

// §9.4 orders reconciliation before dispatch, and the order carries
// information: reconciliation is what frees slots, so a dispatch that decided
// first would decide against a world about to change. The replacement cycle a
// superseded read triggers is a dispatch cycle like any other and owes the same
// order — it is not a shortcut back to the candidate read.
func TestTheReplacementCycleStillWaitsOnReconciliation(t *testing.T) {
	fetchHold, getHold := make(chan struct{}), make(chan struct{})
	var onceFetch, onceGet sync.Once
	releaseFetch := func() { onceFetch.Do(func() { close(fetchHold) }) }
	releaseGet := func() { onceGet.Do(func() { close(getHold) }) }
	t.Cleanup(func() { releaseFetch(); releaseGet() })
	var fetchGated, getGated atomic.Bool

	// One running issue, so reconciliation has an issue to refresh by Get.
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)
	// A second, dispatchable issue: what a bypassing candidate read would find.
	h.Tracker.Set(fake.Issue("2", epoch.Add(time.Hour)))

	h.Tracker.SetFetchGate(func() {
		if fetchGated.CompareAndSwap(false, true) {
			<-fetchHold
		}
	})
	h.PollNow()
	waitFor(t, "the candidate read to wedge", func() bool { return fetchGated.Load() })

	// Now wedge reconciliation. Installed after the candidate read is already
	// out, so this tick's own refresh has been and gone and the gate catches
	// only what comes next.
	h.Tracker.SetGetGate(func() {
		if getGated.CompareAndSwap(false, true) {
			<-getHold
		}
	})

	// Supersede the wedged read, then let it return. Its replacement must get
	// no further than the reconciliation now blocked.
	fetches := h.Tracker.FetchReads()
	h.Source.reload(definition(t, "3", "  max_retry_backoff_ms: 222000\n"), nil)
	releaseFetch()

	waitFor(t, "the replacement cycle to reach reconciliation", func() bool { return getGated.Load() })
	time.Sleep(150 * time.Millisecond)

	if got := h.Tracker.FetchReads(); got != fetches {
		t.Errorf("candidate reads = %d, want %d: the replacement fetch began while the reconciliation before it was still blocked", got, fetches)
	}
	if got := h.stateOf("2"); got != "" {
		t.Errorf("issue 2 is %q, want untracked: it was dispatched by a cycle that skipped step 1", got)
	}

	// Let the loop finish so the harness can stop.
	releaseGet()
}

// A superseded observation records *nothing* — not its definition, not its
// verdict. That is the invariant asserted here, and it is asserted at
// noteConfigAt rather than through the loop because the loop is where the
// interleaving lives and an interleaving is not a thing a test can schedule.
//
// Which is why the check and the state it guards live in one cell with one
// writer: split across two, a stale read passes a check that was true a moment
// ago and then overwrites the state that superseded it — reinstating a definition
// a human has replaced and clearing a block that has just been raised. There is
// no second state to overwrite now, so the invariant holds by construction and
// what remains to test is that a stale read is *discarded*, which
// TestAStalePreflightResultDoesNotRollBackANewerReload drives through the loop.

// §5.4 names five surfaces a valid reload applies to: dispatch, retry
// scheduling, reconciliation, hooks and launches. All of them have to read the
// *same* configuration, and read it as soon as it lands.
//
// The bug this pins is a second source. The loop kept a private copy of the
// definition and refreshed it from the versioned snapshot at the top of a tick,
// so between a reload and the next tick every question below answered under a
// configuration a human had already replaced. Nothing here ticks — that is the
// point: a policy that needs a tick to notice is one that spends a whole poll
// interval wrong, and the run outcomes and timers that drive most of these
// decisions do not wait for ticks.
func TestEveryPolicySurfaceReadsTheReloadImmediately(t *testing.T) {
	o, src := idleWithSource(t, fake.NewTracker())
	issue := core.Issue{
		Identifier: "1", State: "open",
		Labels:    []string{"ben-queue"},
		Assignees: []string{fake.DefaultPrincipal},
	}

	// Every field below is moved off the standing definition's value, and the
	// two tracker fields are moved so that this very issue changes verdict:
	// open is no longer an active state, and ben-queue is no longer the
	// partition.
	next := loadDefinition(t, `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["shipped"]
  active_states: ["in_progress"]
agent:
  kind: claude-code
limits:
  max_concurrent_agents: 1
  max_turns: 1
  max_attempts: 1
  max_retry_backoff_ms: 1000
polling:
  interval_ms: 5000
deployment:
  mode: attended
---
Work issue {{ issue.identifier }}.
`)
	standing := definition(t, "3", "")

	for _, tc := range []struct {
		surface string
		before  func() bool // holds under the harness definition
		after   func() bool // must hold once the reload lands
	}{
		{
			surface: "retry scheduling (max_attempts)",
			before:  func() bool { return o.attemptsRemain(&Record{Attempt: 2}) },
			after:   func() bool { return !o.attemptsRemain(&Record{Attempt: 2}) },
		},
		{
			surface: "the continuation budget (max_turns)",
			before:  func() bool { return o.continuable(&Record{Turns: 2}) },
			after:   func() bool { return !o.continuable(&Record{Turns: 2}) },
		},
		{
			surface: "retry scheduling (max_retry_backoff_ms)",
			before:  func() bool { return o.limits().MaxRetryBackoffMS == config.DefaultMaxRetryBackoffMS },
			after:   func() bool { return o.limits().MaxRetryBackoffMS == 1000 },
		},
		{
			surface: "capacity (max_concurrent_agents)",
			before:  func() bool { return o.freeSlots(o.definition()) == 3 },
			after:   func() bool { return o.freeSlots(o.definition()) == 1 },
		},
		{
			surface: "the ceiling `ben status` reports",
			before:  func() bool { return o.MaxRetryBackoffMS() == config.DefaultMaxRetryBackoffMS },
			after:   func() bool { return o.MaxRetryBackoffMS() == 1000 },
		},
		{
			surface: "the tick cadence",
			before: func() bool {
				d, _ := o.pollWait()
				return d == time.Duration(config.DefaultPollingIntervalMS)*time.Millisecond
			},
			after: func() bool {
				d, _ := o.pollWait()
				return d == 5*time.Second
			},
		},
		{
			surface: "reconciliation (active_states)",
			before:  func() bool { return o.active(o.definition(), issue) },
			after:   func() bool { return !o.active(o.definition(), issue) },
		},
		{
			surface: "routing (required_labels)",
			before:  func() bool { return o.routable(o.configNow(), issue) },
			after:   func() bool { return !o.routable(o.configNow(), issue) },
		},
		{
			surface: "held-claim policy (required_labels)",
			before:  func() bool { return o.hasRequiredLabels(o.definition(), issue) },
			after:   func() bool { return !o.hasRequiredLabels(o.definition(), issue) },
		},
	} {
		t.Run(tc.surface, func(t *testing.T) {
			// Back to the harness values first, so each surface starts from a
			// state where its `after` is genuinely false — a subtest that
			// inherited the reload would pass without proving anything.
			src.reload(standing, nil)
			if !tc.before() {
				t.Fatalf("%s does not hold under the standing definition; the reload below would prove nothing", tc.surface)
			}

			src.reload(next, nil)

			if !tc.after() {
				t.Errorf("%s still answers under the replaced definition: §5.4 gives it to the reload, and nothing here has ticked", tc.surface)
			}
		})
	}
}

// SPEC §5.4 gives *launches* to a valid reload, and Prepare is the long gap
// where one lands: a clone, a fetch and an after_create hook can run for
// minutes. A launch that used the definition dispatch had accepted would spend
// that whole window unable to see an edit plainly meant for it.
//
// The other half is in the same test, because they are one rule: once the run
// has started it keeps what it launched under, whatever arrives next.
func TestAReloadDuringPrepareReachesTheLaunch(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var preparing atomic.Bool

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
		prepareGate: func() {
			if preparing.CompareAndSwap(false, true) {
				<-release
			}
		},
	})
	waitFor(t, "the workspace prepare to be in flight", func() bool { return preparing.Load() })

	// The edit lands while the worktree is still being built.
	reloaded := definitionTurns(t, "3", "99", "  max_cost_usd: 7.5\n")
	h.Source.reload(reloaded, nil)
	unblock()
	h.WaitState("1", StateRunning)

	spec, ok := h.Runner.LastSpec()
	if !ok {
		t.Fatal("no run was started")
	}
	if spec.Limits.MaxTurns != 99 {
		t.Errorf("the launch carried max_turns = %d, want the reloaded 99: a reload during Prepare never reached the run it was meant for",
			spec.Limits.MaxTurns)
	}
	if spec.Limits.MaxCostUSD != 7.5 {
		t.Errorf("the launch carried max_cost_usd = %v, want the reloaded 7.5", spec.Limits.MaxCostUSD)
	}

	// The other half of the rule, at the unit the loop cannot expose: a started
	// run keeps what it launched under. The budget is the one thing a live
	// attempt still resolves from its own snapshot (onRunEvent → maxCost), so it
	// is what the assertion reads; r.Definition is loop-owned, and asking a
	// running harness for it from here would be the race this suite keeps
	// finding.
	idle, idleSrc := idleWithSource(t, fake.NewTracker())
	launched := &Record{Definition: reloaded}
	idleSrc.reload(definitionTurns(t, "3", "1", "  max_cost_usd: 0.01\n"), nil)
	if now := idle.limits().MaxCostUSD; now == nil || *now != 0.01 {
		t.Fatal("the second reload did not land; the assertion below would prove nothing")
	}
	if got := maxCost(launched); got != 7.5 {
		t.Errorf("the started attempt's budget moved to %v, want the 7.5 it launched under: §5.4 never disturbs a run already going", got)
	}
}

// An oversized prompt refuses the dispatch, and refuses it *before* an agent
// exists (SPEC §5.6, #50). The ceiling is on attacker-controlled token spend —
// an issue body is written by whoever filed the issue — so a run that started
// and was then judged too large would have already spent what the limit exists
// to bound.
//
// The refusal is a non-retryable launch_error: the same template and the same
// inputs render the same way, so a retry is a second identical refusal.
//
// This is the wiring #50's config-level tests cannot see. They prove
// WorkflowDefinition.RenderPrompt applies the configured ceiling; only here is
// it observable that the *orchestrator* renders through it, with the record's
// own definition snapshot.
func TestOversizedPromptRefusesTheDispatchBeforeLaunching(t *testing.T) {
	issue := fake.Issue("1", epoch)
	// The template emits the title, and the title is untrusted, so it renders
	// fenced (SPEC §5.6) — this is well past a 300-byte prompt either way.
	issue.Title = strings.Repeat("very long ticket title ", 40)

	h := start(t, harnessOpts{
		issues:      []core.Issue{issue},
		extraConfig: "  max_prompt_bytes: 300\n",
	})

	// A terminal record is released, so the log is what outlives it.
	h.WaitGone("1")
	if path := h.o.Transitions.Path("1"); !containsState(path, StateFailed) {
		t.Fatalf("path = %v, want it to reach %q", path, StateFailed)
	}
	if n := h.Runner.StartCount(); n != 0 {
		t.Errorf("started %d runs; an oversized prompt must be refused before an agent is launched", n)
	}

	// The reason survives in the log, so an operator reading it learns the
	// ceiling refused the prompt rather than meeting a mystery launch failure.
	var reasons []string
	for _, e := range h.o.Transitions.For("1") {
		reasons = append(reasons, e.Reason)
	}
	joined := strings.Join(reasons, " | ")
	if !strings.Contains(joined, "rendering the prompt") || !strings.Contains(joined, "300") {
		t.Errorf("transition reasons %q name neither the render nor the configured ceiling", joined)
	}
	// And it reaches the tracker as the §7.3 reason, which is what a human
	// unparking the issue reads.
	if calls := strings.Join(h.Tracker.Calls, " | "); !strings.Contains(calls, "comment 1="+string(core.MilestoneFailed)) {
		t.Errorf("tracker calls %q carry no failed milestone", calls)
	}
}
