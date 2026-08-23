package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The SPEC §9.10 step 5 workspace sweep, and the §9.8 residue it deals with: the
// pass that accounts for every directory no record owns. What it may delete is
// the narrow half — a workspace whose issue the tracker says is terminal and
// whose run is confirmed gone — and everything else here is what it may *not*
// touch: a workspace a record holds, a failure's retained worktree (§6.4), a run
// that may still be live. The rest is what the pass costs: it runs off the
// authority goroutine, reserves what it is about to destroy, paces itself
// against the §8.5 request budget and rotates so no tail starves, and defers per
// workspace rather than re-listing the directory.
//
// Split out of recover_test.go by #160. The §9.10 fixtures these share with the
// rest of the family — `harness.restart`, `newProber`, `groupGone`/`groupAlive`,
// `incompleteEvidence` — stay in recover_test.go, which owns them;
// `deferredResidue` and `rootedDefinition` are sweep-only and moved here with
// their tests.

// The workspace of an issue whose claim was released *with the workspace kept* is
// not disposed by a later recovery pass. §6.4: failures keep the workspace, and
// that record is gone by the next start, so any rule keyed on ownership would
// delete exactly the evidence it retains.
func TestARetainedFailureWorkspaceSurvivesALaterRecovery(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		// A non-retryable reason (core.FailureReason.Retryable), so the attempt goes
		// straight to `failed` rather than through backoff.
		script: func(core.RunSpec, int) []core.Event {
			return fake.Fail(core.FailureBudgetExceeded)
		},
		runGone: groupGone,
	})
	// `failed` releases the claim and keeps the workspace — by not disposing at all
	// (enterFailed) — and then forgets the record.
	h.WaitGone("1")
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Fatalf("disposals = %+v, want none: a failure keeps its workspace (SPEC §6.4)", got)
	}
	if len(h.Workspaces.Prepares("1")) == 0 {
		t.Fatal("nothing prepared a workspace, so there is none to protect")
	}

	// A restart with nothing assigned: the issue is not a candidate at all, so
	// nothing recovery does may touch its workspace.
	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("recovery disposed a retained failure's workspace: %+v — §6.4 keeps it for debugging", got)
	}
}

// SPEC §9.10 step 5 and B10's acceptance: the workspaces of terminal-state issues
// are swept at startup.
//
// The case that makes it necessary is the one no candidate can reach: an issue that
// failed, had its claim released, and was then closed while BEN was down. It never
// comes back from ClaimedByPrincipal, so nothing in the classification pass
// accounts for its workspace — and §6.4 keeps a *failure's* workspace, so the
// daemon cannot simply delete what it does not recognize either. The evidence is a
// recorded owner plus a tracker read.
func TestRecoverySweepsTheWorkspaceOfAnIssueClosedWhileDown(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		// A non-retryable failure: `failed` releases the claim and keeps the
		// workspace, then forgets the record.
		script: func(core.RunSpec, int) []core.Event {
			return fake.Fail(core.FailureBudgetExceeded)
		},
		runGone: groupGone,
	})
	h.WaitGone("1")
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Fatalf("disposals = %+v, want none yet: a failure keeps its workspace", got)
	}

	// A human closes the issue while the daemon is down. It is no longer assigned,
	// so recovery will not see it as a candidate at all.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	got := h.Workspaces.Disposals("1")
	if len(got) != 1 {
		t.Fatalf("disposals = %+v, want exactly one — §9.10 step 5 sweeps terminal workspaces", got)
	}
	if got[0].Keep {
		t.Error("the sweep kept the workspace of a closed issue")
	}
}

// The sweep asks, and what it does with each answer is the whole safety argument.
func TestTheSweepLeavesEveryWorkspaceItCannotProveTerminal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(h *harness)
		// wantSwept says whether the workspace should be disposed by recovery.
		wantSwept bool
		// wantReported says the workspace was counted as one nothing accounts for,
		// rather than merely surviving the terminal path.
		wantReported bool
	}{
		{
			name:      "a closed issue's workspace is swept",
			setup:     func(h *harness) { h.Tracker.Mutate("9", func(i *core.Issue) { i.State = "closed" }) },
			wantSwept: true,
		},
		{
			// §6.4 keeps a failure's workspace for debugging, and a failure is open.
			name:  "an open issue's workspace is kept, because it may be a kept failure",
			setup: func(h *harness) {},
		},
		{
			// Nothing records whose it is, so nothing can ask whether it is terminal.
			name: "a workspace with no owner record is left alone",
			setup: func(h *harness) {
				h.Tracker.Mutate("9", func(i *core.Issue) { i.State = "closed" })
				h.Workspaces.ForgetOwner("9")
			},
			// Asserted on the reason, because "not swept" is reachable by accident: an
			// unowned ref pushed down the terminal path would fail to resolve a
			// workspace and be left in place anyway, passing this row while having
			// asked the tracker about an issue called "".
			wantReported: true,
		},
		{
			// The workspace precondition, named by step 5 outright.
			name: "a closed issue whose run is not confirmed gone is left in place",
			setup: func(h *harness) {
				h.Tracker.Mutate("9", func(i *core.Issue) { i.State = "closed" })
				h.Workspaces.SetRunMarker("9", core.RunMarker{State: core.RunMarkerUnknownLaunch})
			},
			wantSwept: false,
		},
		{
			// A read that failed is not a terminal issue, and this is a disposal.
			name: "a tracker that will not answer leaves the workspace alone",
			setup: func(h *harness) {
				h.Tracker.Mutate("9", func(i *core.Issue) { i.State = "closed" })
				h.Tracker.SetFailGet(errors.New("tracker unavailable"))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, harnessOpts{runGone: groupGone})
			// A workspace on disk for an issue nothing holds. The queue label is
			// stripped so the loop cannot simply dispatch it — an unassigned, labelled,
			// open issue is ordinary work, and its workspace would then be disposed by
			// the `done` path rather than by the sweep, which is a different mechanism
			// answering a different question.
			residue := fake.Issue("9", epoch)
			residue.Labels = nil
			residue.Dispatchable = false
			h.Tracker.Set(residue)
			if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: "9"}, 1); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			tc.setup(h)

			if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			// Asserted on the sweep's own action, not on the disposal count: several
			// paths dispose a workspace, and this test is about exactly one of them.
			swept := len(h.Logs.find("swept the workspace of a terminal issue")) > 0
			if swept != tc.wantSwept {
				t.Errorf("swept = %v, want %v (disposals: %+v)", swept, tc.wantSwept, h.Workspaces.Disposals("9"))
			}
			if swept != (len(h.Workspaces.Disposals("9")) > 0) {
				t.Errorf("the sweep said %v but disposals are %+v", swept, h.Workspaces.Disposals("9"))
			}
			reported := len(h.Logs.find("nothing records which issue they belong to")) > 0
			if reported != tc.wantReported {
				t.Errorf("reported as unaccounted = %v, want %v — a directory whose issue is unknown must be "+
					"counted as such, not pushed down a path that asks the tracker about an empty identifier",
					reported, tc.wantReported)
			}
		})
	}
}

// A workspace a record still owns is never swept, including one whose workspace
// never resolved — an unclassified candidate has an empty Workspace.Key and is
// emphatically holding a directory.
func TestTheSweepNeverTouchesAWorkspaceARecordHolds(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	// Closed *and* unclassifiable: the sweep must not read "terminal" and act while
	// the classification pass is still holding the issue out of dispatch.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tracker.SetFailHistory(errors.New("tracker unavailable"))

	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, tracked := h.o.records["1"]; !tracked {
		t.Fatal("the unclassified candidate has no record")
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("swept a workspace an unclassified record holds: %+v — the classification is what "+
			"decides, and it has not finished", got)
	}
}

// SPEC §9.10 step 5 is a question asked repeatedly, not once: a workspace whose run
// may still be live is "left in place and swept once that run is confirmed gone".
//
// One pass at startup answers it only for the runs that had already ended by then,
// and never for the case the precondition exists to handle.
func TestTheTerminalSweepRetriesUntilTheRunIsConfirmedGone(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})

	// A closed issue whose workspace carries a marker identifying a run that is
	// still going. Nothing else will ever look at it: it is not a candidate, so the
	// classification pass never sees it.
	residue := fake.Issue("9", epoch)
	residue.Labels = nil
	residue.Dispatchable = false
	residue.State = "closed"
	h.Tracker.Set(residue)
	if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: "9"}, 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.Workspaces.SetRunMarker("9", core.RunMarker{
		State:    core.RunMarkerIdentified,
		Evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "999", Boot: "boot-1"},
	})

	probe := newProber(groupAlive)
	if err := h.restart(harnessOpts{runGone: probe.probe}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := h.Workspaces.Disposals("9"); len(got) != 0 {
		t.Fatalf("swept a workspace whose run is not confirmed gone: %+v", got)
	}
	if len(h.Logs.find("probing again on later ticks")) == 0 {
		t.Error("nothing said the question would be asked again; a one-shot sweep never reaches this workspace")
	}

	// The run ends. No restart, no human: the next tick's probe answers, and the
	// marker comes off with the workspace.
	probe.set(groupGone)
	h.tickUntil("the workspace to be swept once its run is confirmed gone", func() bool {
		return len(h.Workspaces.Disposals("9")) > 0
	})
	if m, ok := h.Workspaces.RunMarkerFor("9"); ok {
		t.Errorf("marker %+v survived the sweep; the run was proven gone", m)
	}
}

// A candidate scan that failed takes step 5 down with it — the sweep is sequenced
// after the classification pass because it needs the ownership set — so the sweep
// has to be owed too, not skipped for the life of the process.
func TestAFailedScanLeavesTheSweepOwedRatherThanSkipped(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	residue := fake.Issue("9", epoch)
	residue.Labels = nil
	residue.Dispatchable = false
	residue.State = "closed"
	h.Tracker.Set(residue)
	if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: "9"}, 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	h.Tracker.SetFailClaimedByPrincipal(errors.New("tracker unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone, recoverErr: true}); err == nil {
		t.Fatal("the startup scan was supposed to fail")
	}
	if got := h.Workspaces.Disposals("9"); len(got) != 0 {
		t.Fatalf("swept without an ownership set: %+v — the pass that produces it never ran", got)
	}

	// Still failing: the sweep stays owed and does not run on a set no pass produced.
	h.Tick()
	if got := h.Workspaces.Disposals("9"); len(got) != 0 {
		t.Fatalf("swept while the candidate scan was still owed: %+v — every workspace looks unowned "+
			"until the classification pass has run", got)
	}

	h.Tracker.SetFailClaimedByPrincipal(nil)
	h.tickUntil("the deferred sweep to run", func() bool {
		return len(h.Workspaces.Disposals("9")) > 0
	})
}

// A workspace whose base pin is gone is still a directory, and ListWorkspaces
// already found it. `ok=false` is a statement about the *pin*; returning on it
// would mean nothing ever removes the workspace, because no candidate accounts for
// it and no pin will ever appear.
func TestATerminalWorkspaceWithNoBasePinIsStillSwept(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	residue := fake.Issue("9", epoch)
	residue.Labels = nil
	residue.Dispatchable = false
	residue.State = "closed"
	h.Tracker.Set(residue)
	if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: "9"}, 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// The pin is gone — out-of-band surgery, a base repository restored from an
	// older backup — while the directory remains.
	h.Workspaces.ForgetBasePin("9")

	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := h.Workspaces.Disposals("9"); len(got) != 1 {
		t.Fatalf("disposals = %+v, want one: a pinless workspace is residue nothing else will remove", got)
	}
}

// One workspace nobody can resolve must not put the whole directory through a
// tracker read every tick. Deferrals are per workspace, so an unknown-launch
// directory is examined once and the rest are not re-listed on its account.
func TestAnUnresolvableWorkspaceDoesNotReSweepTheRest(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})

	// Two terminal residues: one whose launch outcome is unknowable, and one whose
	// run is still going. Only the second can converge.
	for _, id := range []string{"8", "9"} {
		iss := fake.Issue(id, epoch)
		iss.Labels = nil
		iss.Dispatchable = false
		iss.State = "closed"
		h.Tracker.Set(iss)
		if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: id}, 1); err != nil {
			t.Fatalf("Prepare(%s): %v", id, err)
		}
	}
	h.Workspaces.SetRunMarker("8", core.RunMarker{State: core.RunMarkerUnknownLaunch})
	h.Workspaces.SetRunMarker("9", core.RunMarker{
		State:    core.RunMarkerIdentified,
		Evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "999", Boot: "boot-1"},
	})

	probe := newProber(groupAlive)
	if err := h.restart(harnessOpts{runGone: probe.probe}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Issue 8 is asked about exactly once: no answer is coming for an unknown
	// launch, so it is reported and dropped from the deferred set.
	before := h.Tracker.GetReads()
	for range 3 {
		h.Tick()
	}
	after := h.Tracker.GetReads()
	if after-before > 3 {
		t.Errorf("the sweep spent %d tracker reads over 3 ticks; only the one workspace that can still "+
			"converge should be re-examined", after-before)
	}
	// The sweep's own line, not resolveMarker's — both mention an unknown launch,
	// and only one of them is about a workspace being left in the directory.
	if got := len(h.Logs.find("cannot resolve without a human")); got != 1 {
		t.Errorf("the unknown-launch workspace was reported %d times; no answer is coming for it, "+
			"so it is said once and dropped from the deferred set", got)
	}

	// And the one that can converge still does.
	probe.set(groupGone)
	h.tickUntil("the resolvable workspace to be swept", func() bool {
		return len(h.Workspaces.Disposals("9")) > 0
	})
	if got := h.Workspaces.Disposals("8"); len(got) != 0 {
		t.Errorf("swept a workspace whose launch outcome is unknown: %+v", got)
	}
}

// The §9.10 step 5 sweep runs off the authority goroutine.
//
// It is I/O all the way down — a listing, a tracker Get per unaccounted workspace, a
// probe, a git resolve, a disposal that runs hooks — and the loop is the one
// goroutine in BEN that must never block on any of them. Driven by wedging the
// *retry* pass inside its tracker read and checking that the loop still reaches a
// verdict: on the loop, that read would stall runner events, budgets and shutdown.
func TestTheTerminalSweepDoesNotBlockTheAuthorityGoroutine(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	// A terminal residue whose run is not confirmed gone, so the startup pass defers
	// it and a later tick re-examines it — which is the pass this test wedges.
	residue := fake.Issue("9", epoch)
	residue.Labels = nil
	residue.Dispatchable = false
	residue.State = "closed"
	h.Tracker.Set(residue)
	if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: "9"}, 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	h.Workspaces.SetRunMarker("9", core.RunMarker{
		State:    core.RunMarkerIdentified,
		Evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "999", Boot: "boot-1"},
	})

	if err := h.restart(harnessOpts{runGone: groupAlive, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	waitFor(t, "the startup pass to defer the residue", func() bool {
		return len(h.Logs.find("probing again on later ticks")) > 0
	})

	// From here every sweep Get blocks. The next tick's pass wedges in it.
	h.Tracker.SetGetGate(func() { <-release })

	// The discriminator is the *rest of the tick*: §9.4 runs reconciliation and then
	// the dispatch reads, and retrySweep sits between them. A candidate fetch landing
	// while the sweep's Get is outstanding is only possible if the sweep is not on
	// this goroutine — and a fetch is the cheapest thing to count that the loop
	// cannot fake.
	//
	// Asserted on a counter that must *advance*, not on a condition that might
	// already hold: the first version of this test checked the record had left
	// `running`, which recovery had already made true before the sweep ran at all, so
	// it passed against both spellings.
	before := h.Tracker.FetchReads()
	deadline := time.Now().Add(5 * time.Second)
	for h.Tracker.FetchReads() <= before && time.Now().Before(deadline) {
		h.Clock.BlockUntilWaiters(1)
		h.Clock.Advance(time.Duration(h.def.Config.Polling.IntervalMS) * time.Millisecond)
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.Tracker.FetchReads(); got <= before {
		t.Errorf("no candidate fetch completed while the sweep's tracker read was outstanding "+
			"(%d, was %d); step 5's I/O is on the authority goroutine, so a slow Get stalls the whole loop",
			got, before)
	}
	unblock()
}

// A §9.10 step 5 pass runs off the loop, so the ownership set it was handed is a
// *snapshot* — and between that snapshot and the disposal sits a directory listing, a
// tracker Get, a liveness probe, a git resolve and a disposal that runs hooks.
//
// All of that is time in which the issue can reopen, be claimed and reach Prepare.
// The pass would then delete a live attempt's worktree, which is exactly the outcome
// §9.10's workspace precondition exists to make impossible, reached from the cleanup
// side instead of the launch side. So the pass reserves each workspace on the
// authority goroutine, and dispatch declines to claim an issue whose workspace is
// reserved.
//
// Driven by wedging the pass *inside its liveness probe* — after the tracker Get has
// said the issue is terminal, and before the disposal. That is the window that
// matters: reserving is worth nothing if the reservation is dropped before the
// destructive step, and the probe is the last thing between the two.
func TestTheSweepReservesTheWorkspaceItIsAboutToDisposeOf(t *testing.T) {
	probing := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	h := start(t, harnessOpts{runGone: groupGone})
	deferredResidue(t, h, "9")

	// The startup pass defers the residue: its run is not confirmed gone, so a later
	// tick is what re-examines it, and that pass is the one this test wedges.
	probe := newProber(groupAlive)
	if err := h.restart(harnessOpts{runGone: probe.probe}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	waitFor(t, "the startup pass to defer the residue", func() bool {
		return len(h.Logs.find("probing again on later ticks")) > 0
	})

	// From here the next pass wedges in the probe, having already been told the issue
	// is closed. It answers `gone` on release, so it goes on to dispose.
	probe.set(func(core.RunEvidence) (bool, error) {
		once.Do(func() { close(probing) })
		<-release
		return true, nil
	})
	h.tickUntil("the pass to reach its liveness probe", func() bool {
		select {
		case <-probing:
			return true
		default:
			return false
		}
	})

	// The issue reopens and becomes dispatchable while the pass holds its workspace.
	h.Tracker.Set(fake.Issue("9", epoch))

	// Ticks land in the window. Dispatch fetches candidates and finds this one
	// eligible by every test it makes of the *tracker* — the reservation is the only
	// thing standing between it and a claim.
	for range 3 {
		h.Tick()
	}
	// One prepare, the fixture's own. A second means dispatch claimed the issue and
	// prepared a worktree that the disposal below is about to delete.
	if got := h.Workspaces.Prepares("9"); len(got) != 1 {
		t.Fatalf("prepares = %+v, want only the fixture's: dispatch claimed an issue whose workspace a "+
			"cleanup pass was holding, and the disposal that follows deletes a live attempt's worktree "+
			"(SPEC §9.10 step 5)", got)
	}
	if len(h.Workspaces.Disposals("9")) != 0 {
		t.Fatal("disposed before the probe answered; this test's window would not exist")
	}

	// The probe answers, and the pass finishes the disposal it had reserved for.
	unblock()
	waitFor(t, "the reserved disposal to complete", func() bool {
		return len(h.Workspaces.Disposals("9")) > 0
	})

	// And the reservation is released rather than leaked: the issue is open and
	// dispatchable, so the very next poll must be able to work it.
	h.tickUntil("the reopened issue to be dispatched once the pass let go", func() bool {
		return len(h.Workspaces.Prepares("9")) > 1
	})
}

// The reservation's clauses, each anchored on its own: a workspace is never reserved
// for an issue something already owns, nor twice, nor during a drain.
//
// A unit test over the decision rather than a race driven through the loop, because
// what is being pinned is a closed set of refusals — and a test that reached them by
// timing would prove only that one of them fired.
func TestADisposalIsNeverReservedForAnIssueSomethingOwns(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker())

	if !o.grantDisposal("1") {
		t.Fatal("refused an unowned issue; then no workspace is ever swept")
	}
	if o.grantDisposal("1") {
		t.Error("granted the same workspace twice; two passes would share one directory")
	}
	o.onSweepRelease("1")
	if !o.grantDisposal("1") {
		t.Error("still refused after the release; a reservation that never lifts stops dispatch for good")
	}
	o.onSweepRelease("1")

	o.records["2"] = &Record{Issue: core.Issue{Identifier: "2"}, State: StateRunning}
	if o.grantDisposal("2") {
		t.Error("granted for an issue a run record owns — the workspace has an agent in it")
	}
	o.held["3"] = &heldClaim{}
	if o.grantDisposal("3") {
		t.Error("granted for a retained `done` claim; §9.8 releases it on the close and its PR is awaiting review")
	}

	o.draining = true
	if o.grantDisposal("4") {
		t.Error("granted during a drain; shutdown initiates no new cleanup (SPEC §9.8 as amended)")
	}
}

// The other side of the reservation, and the one route by which a record can appear
// *after* one was granted: a retried candidate scan adopts unclassified records, and
// it must — a candidate with no record is dispatchable.
//
// So the verdict is held rather than applied. Applying it could put the record on the
// backoff track and have Prepare reattach a worktree the pass is about to delete;
// staying unclassified holds the issue out of dispatch, and retryRecovery asks again
// next tick.
func TestARecoveryVerdictIsHeldWhileACleanupPassHoldsTheWorkspace(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker())
	issue := core.Issue{Identifier: "1", State: "open", Labels: []string{"ben-queue"}}

	// In the order the race has: the pass reserves an unowned workspace, and only
	// then does a retried candidate scan discover the claim and adopt it.
	if !o.grantDisposal("1") {
		t.Fatal("refused an issue nothing owned yet; the window this test is about would not open")
	}
	r := &Record{Issue: issue, State: StateQueued, token: o.newToken(), recovered: true, unclassified: true}
	o.records["1"] = r

	// A verdict that would otherwise adopt the record into the failure track and
	// reattach the branch.
	verdict := recoveryVerdict{action: recoveryActionBackoff, attemptFloor: 2}
	facts := recoveryFacts{candidate: recoveryCandidate{issue: issue}}
	o.onRecovered(context.Background(), r, signal{
		kind: sigRecovered, issue: "1", token: r.token, revision: o.configNow().Revision,
		recovery: &verdict, recoveryReads: &facts,
	})

	if got := o.records["1"]; got != r || !got.unclassified {
		t.Fatalf("the record was reclassified while a cleanup pass held its workspace (%+v); Prepare would "+
			"reattach a worktree the pass then deletes", got)
	}
	if got := o.records["1"].State; got != StateQueued {
		t.Errorf("state = %q, want %q: an unclassified record is the one shape dispatch will not act on", got, StateQueued)
	}

	// And it is a hold, not a drop: once the pass lets go, the same verdict applies.
	o.onSweepRelease("1")
	o.onRecovered(context.Background(), r, signal{
		kind: sigRecovered, issue: "1", token: r.token, revision: o.configNow().Revision,
		recovery: &verdict, recoveryReads: &facts,
	})
	if got := o.records["1"]; got == nil || got.unclassified {
		t.Fatalf("the verdict never applied after the release (%+v); a held verdict that is never re-applied "+
			"is a claim held and never worked", got)
	}
}

// A live pass filters its refs through an ownership snapshot before any I/O, and a
// skipped ref is absent from the pass's result — including from `deferred`. That is
// sound only if the record that owns the issue deals with the directory, and not every
// exit does: `recoveryActionGone` drops a record outright, because there is no claim of
// ours left to release and nothing to classify, and it disposes nothing.
//
// So the workspace outlives the only thing accounting for it, with no deferral naming
// it and no pass owed. What closes the hole is the forget itself: the ownership set
// shrank, so the directory is worth listing again.
func TestAWorkspaceOutlivingARecordDroppedAsGoneIsStillSwept(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})

	// A claim this daemon holds, with a workspace on disk from a previous tenure.
	issue := fake.Issue("9", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: "9"}, 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Recovery cannot classify it — the change log will not answer — so it is adopted
	// unclassified, which is a record, which is enough to keep the startup pass off its
	// workspace.
	h.Tracker.SetFailHistory(errors.New("changelog unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("9", StateQueued)
	if got := h.Workspaces.Disposals("9"); len(got) != 0 {
		t.Fatalf("swept a workspace a record was holding: %+v", got)
	}

	// The claim goes away under us — a human took the issue, and closed it. The retried
	// classification drops the record outright rather than releasing or disposing.
	h.Tracker.SetFailHistory(nil)
	h.Tracker.Mutate("9", func(i *core.Issue) {
		i.Assignees, i.State, i.Dispatchable = nil, "closed", false
	})
	h.tickUntil("the record to be dropped as gone", func() bool { return h.stateOf("9") == "" })

	// Nothing accounts for the directory now, and nothing named it as deferred either.
	h.tickUntil("the orphaned workspace to be swept", func() bool {
		return len(h.Workspaces.Disposals("9")) > 0
	})
}

// A pass is paced, and the pacing rotates: no workspace sits behind the sorted prefix
// forever.
//
// A workspace whose issue cannot be read stays deferred, so a directory holding enough
// of them re-examines all of them every tick. The GitHub adapter admits 39 ordinary
// billed requests per window, shared with reconciliation's Get per tracked issue, the
// held-claim list and the candidate fetch — so an unbounded pass spends the window and
// every read behind it is refused. Bounding it from the top alone is no better: the
// listing is key-ordered, so the same prefix is examined every tick and the tail is
// never reached at all.
//
// Driven with more deferred workspaces than one pass may examine, and asserted on the
// two properties together: no tick spends more than the bound, and every workspace is
// eventually swept.
func TestTheSweepPacesItselfAndRotatesRatherThanStarvingTheTail(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})

	// Three times the per-pass bound, and *none* of them resolve: every run is still
	// going, so every one stays deferred and comes back on the next tick. That is the
	// shape both halves need. A fixture whose head drained would converge with or
	// without a cursor, and one that emptied would stop exercising the bound after a
	// tick or two.
	const residues = maxSweepExaminations * 3
	ids := make([]string, 0, residues)
	for i := range residues {
		// Zero-padded so the listing order is the numeric order, and "the tail" is a
		// stated set rather than whatever the string sort happened to produce.
		id := fmt.Sprintf("9%02d", i)
		ids = append(ids, id)
		iss := fake.Issue(id, epoch)
		iss.Labels, iss.Dispatchable, iss.State = nil, false, "closed"
		h.Tracker.Set(iss)
		if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: id}, 1); err != nil {
			t.Fatalf("Prepare(%s): %v", id, err)
		}
		h.Workspaces.SetRunMarker(id, core.RunMarker{
			State:    core.RunMarkerIdentified,
			Evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: id, Boot: "boot-1"},
		})
	}

	probe := newProber(groupAlive)
	if err := h.restart(harnessOpts{runGone: probe.probe}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// **Paced**: no pass spends more than the bound, however many are owed. Measured
	// between acknowledged passes rather than after a sleep — sigSwept's applied count
	// is what says the loop has finished with one (see Orchestrator.applied).
	//
	// Enough ticks to bring every workspace round at least once, so the rotation below
	// is asserted over a window the bound alone could not have covered.
	for range residues/maxSweepExaminations + 1 {
		before, swept := h.Tracker.GetReads(), h.applied(sigSwept)
		h.Tick()
		waitFor(t, "the pass to report back", func() bool { return h.applied(sigSwept) > swept })
		if got := h.Tracker.GetReads() - before; got > maxSweepExaminations {
			t.Fatalf("one pass spent %d tracker reads, want at most %d; step 5 can exhaust the ordinary "+
				"request window and refuse every read behind it (SPEC §8.5)", got, maxSweepExaminations)
		}
	}

	// **Rotating**: every workspace got a turn, not just the sorted prefix. Asserted on
	// which issues were asked about rather than on disposals, because nothing here can
	// be disposed — that is what keeps the prefix permanently in the way.
	for _, id := range ids {
		if h.Tracker.GetReadsFor(id) == 0 {
			t.Fatalf("workspace %s was never examined; a bound applied from the top of a key-ordered "+
				"listing is a permanent floor under the tail, not a pace (SPEC §9.10 step 5)", id)
		}
	}

	// And the pacing delays work rather than dropping it: the runs end, and every one
	// of them converges with no human.
	probe.set(groupGone)
	h.tickUntil("every deferred workspace to be swept", func() bool {
		for _, id := range ids {
			if len(h.Workspaces.Disposals(id)) == 0 {
				return false
			}
		}
		return true
	})
}

// A workspace a pass left to its owner becomes owed the moment the owner goes, and
// costs one examination rather than a full directory pass.
//
// Decided by asking the record set at dispatch rather than by `forget` pushing a flag.
// The ownership set is read there anyway, so the question is free — and nothing has to
// *remember* to hand a ref back, which matters because the exits that drop a record
// without disposing its workspace cannot be enumerated safely. Re-owing a whole pass
// instead was the first answer and cost O(residue) tracker reads per record exit, since
// every pass spends a Get on each unowned kept failure and unknown_launch residue.
func TestAWorkspaceSkippedBySweepIsHandedBackWhenItsOwnerGoes(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker())
	ref := core.WorkspaceRef{Key: "issue-9", Path: "/fake/workspaces/issue-9", Identifier: "9"}
	o.sweepSkipped = map[string]core.WorkspaceRef{"9": ref}
	o.sweepBundle = o.bundle()

	// While a record owns it there is nothing to do, and no pass runs on its account.
	o.records["9"] = &Record{Issue: core.Issue{Identifier: "9"}, State: StateRunning}
	if o.sweepOwed() {
		t.Error("a workspace whose owner is still there reads as owed; every reload would be deferred " +
			"for as long as any run held a directory")
	}
	o.retrySweep(context.Background())
	if o.sweepInFlight {
		t.Fatal("a pass was dispatched for a workspace whose owner is still there")
	}

	// The owner goes without disposing — which is what `recoveryActionGone` does.
	o.forget("9")
	if !o.sweepOwed() {
		t.Fatal("a skipped workspace whose owner has gone is not owed; nothing accounts for that directory " +
			"and nothing will ever look at it again")
	}
	o.retrySweep(context.Background())
	if !o.sweepInFlight {
		t.Fatal("no pass was dispatched for the handed-back workspace")
	}
	if _, still := o.sweepSkipped["9"]; still {
		t.Error("the ref stayed in the skipped set; it would be handed back again on every later tick")
	}
	if _, deferred := o.sweepDeferred[ref.Key]; !deferred {
		t.Error("the handed-back ref never reached the deferred set, so the pass was not asked about it")
	}
}

// A pass completing must not lose a workspace whose owner went while it was out.
//
// The pass skipped that ref because its ownership *snapshot* said it was owned, so the
// result still reports it — and the handback decision is made against the record set,
// not against the snapshot. Deciding from the snapshot is the shape of the bug this
// round fixed one level up, where a completing pass discarded a re-owe.
func TestAPassDoesNotLoseAWorkspaceWhoseOwnerWentWhileItWasOut(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker())
	ref := core.WorkspaceRef{Key: "issue-9", Path: "/fake/workspaces/issue-9", Identifier: "9"}
	o.records["9"] = &Record{Issue: core.Issue{Identifier: "9"}, State: StateRunning}
	o.sweepInFlight = true

	// The owner goes while the pass is out.
	o.forget("9")
	o.onSwept(signal{kind: sigSwept, sweepResult: sweepResultSet{
		deferred: map[string]core.WorkspaceRef{},
		skipped:  map[string]core.WorkspaceRef{"9": ref},
	}})

	if !o.sweepOwed() {
		t.Error("the workspace was lost with the record that went while the pass was out; the pass reported " +
			"it as skipped, and by the time that landed nothing owned it")
	}
}

// deferredResidue leaves one terminal issue's workspace in the §9.10 step 5 deferred
// set: closed, prepared, and carrying a marker whose run the prober says is still
// going. Nothing else will ever look at it — it is not a candidate, so the
// classification pass never sees it — which is what makes it the shape every
// deferred-set test needs.
func deferredResidue(t *testing.T, h *harness, id string) {
	t.Helper()
	residue := fake.Issue(id, epoch)
	residue.Labels = nil
	residue.Dispatchable = false
	residue.State = "closed"
	h.Tracker.Set(residue)
	if _, err := prepareWorkspaceForTest(h.Workspaces, context.Background(), core.Issue{Identifier: id}, 1); err != nil {
		t.Fatalf("Prepare(%s): %v", id, err)
	}
	h.Workspaces.SetRunMarker(id, core.RunMarker{
		State:    core.RunMarkerIdentified,
		Evidence: core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "999", Boot: "boot-1"},
	})
}

// rootedDefinition is the harness definition with workspace.root named, so a test
// can move the one field Bundle.Identity is about.
func rootedDefinition(t *testing.T, root string) *config.WorkflowDefinition {
	t.Helper()
	return definition(t, "3", "workspace:\n  root: "+root+"\n")
}

// Deferred sweep work is bound to the *identity* it was found under.
//
// An identity reload moves workspace.root (§10.1), so a ref carried across one names
// a path under a root this daemon no longer addresses — re-examining it through the
// current provider would ask about a directory somewhere else entirely, and dispose
// whatever happened to share a key under the new root. Dropped loudly, with a full
// pass owed under the new root.
//
// Reaching this is now a backstop rather than the ordinary path: step 5's unfinished
// work is identity work, so AdoptIdentity refuses a root change while it stands (see
// TestAnIdentityReloadIsRefusedWhileWorkspaceCleanupIsOutstanding). What is left is a
// publication that never went through that barrier, which is what publishing straight
// into the source models.
func TestDeferredSweepWorkIsAbandonedWhenItsRootMoves(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	deferredResidue(t, h, "9")

	if err := h.restart(harnessOpts{runGone: groupAlive}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	waitFor(t, "the residue to be deferred", func() bool {
		return len(h.Logs.find("probing again on later ticks")) > 0
	})

	// A reload moves workspace.root, and with it the identity.
	replacement := fake.NewWorkspaces()
	moved := rootedDefinition(t, "/srv/ben/moved")
	reloaded := *h.Bundle
	reloaded.Definition, reloaded.Workspaces = moved, replacement
	if reloaded.Identity() == h.Bundle.Identity() {
		t.Fatal("the reload did not move the identity, so this test is about nothing")
	}
	h.Source.publish(moved, &reloaded, nil)

	h.tickUntil("the deferred work to be abandoned", func() bool {
		return len(h.Logs.find("abandoning deferred workspace cleanup")) > 0
	})
	// And nothing was disposed under the new root on the strength of a ref found
	// under the old one.
	if got := replacement.Disposals("9"); len(got) != 0 {
		t.Errorf("disposed %+v under the new provider from a ref found under the previous one", got)
	}
}

// The complement, and the case the discarded pointer comparison got wrong: a reload
// that rebuilds the adapters without moving workspace.root leaves every deferred ref
// addressable exactly as before, so the promise §9.10 step 5 made about them still
// stands.
//
// Dropping them there was not merely wasteful. It traded per-workspace deferrals for
// a full directory pass — the recurring tracker cost the deferred set exists to avoid
// — and it did so on the reloads §5.4 never refuses, a hook edit or a credential
// rotation, so it happened whenever an operator saved the file.
func TestDeferredSweepWorkSurvivesAReloadThatKeepsTheRoot(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	deferredResidue(t, h, "9")

	probe := newProber(groupAlive)
	if err := h.restart(harnessOpts{runGone: probe.probe}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	waitFor(t, "the residue to be deferred", func() bool {
		return len(h.Logs.find("probing again on later ticks")) > 0
	})

	// A rebuilt bundle over the same root: a new definition pointer and the same
	// workspace.root, which is what a hook or credential edit produces.
	rebuilt := *h.Bundle
	rebuilt.Definition = definition(t, "3", "")
	if rebuilt.Identity() != h.Bundle.Identity() {
		t.Fatal("the rebuild moved the identity; this test needs a reload §5.4 gives to the daemon unconditionally")
	}
	h.Source.publish(rebuilt.Definition, &rebuilt, nil)
	h.Tick()

	if got := h.Logs.find("abandoning deferred workspace cleanup"); len(got) != 0 {
		t.Errorf("abandoned deferred cleanup over a reload that kept the root: %+v", got)
	}

	// And the promise is kept: the run ends, and the workspace it was holding is swept
	// with no human and no full pass.
	probe.set(groupGone)
	h.tickUntil("the deferred workspace to be swept through the rebuilt provider", func() bool {
		return len(h.Workspaces.Disposals("9")) > 0
	})
}
