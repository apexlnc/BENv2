package orchestrator

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The tick, in the order SPEC §9.4 states it: reconcile (step 1), then
// revalidate and dispatch (steps 2–3).
//
// The order carries information — reconciliation is what frees slots — so a
// dispatch that decided first would decide against a world about to change.
// Both halves need holding: reconciliation "always runs", which has to survive a
// slow tracker and not just a failing config, and dispatch may not overtake it,
// including on the replacement cycle a superseded read triggers. The last test
// is the other side of capacity: §9.5 caps live agent *processes*, and
// verification is not one.

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

// SPEC §9.5: "`limits.max_concurrent_agents` caps live agent processes — the
// one scarce resource." Verification is not one. It runs only after the process
// domain is confirmed quiet (§7.5), reads git and the tracker, and can be slow —
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

	// Reaching the verifier at all means the run ended and its execution domain
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
