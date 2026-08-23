package orchestrator

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The SPEC §9.10 run marker, which marker.go owns since #157: the file that says
// a workspace may hold a live agent, written before a launch and removed only
// once that run is confirmed gone. One asymmetry runs through every test here —
// only proof of absence frees a workspace, so an unreadable marker, an
// unconfirmed stop and a probe that errored all mean "possibly live" — and one
// hazard: a removal that failed is owed, and an owed removal must never delete a
// *newer* run's marker.
//
// Split out of recover_test.go by #160. The §9.10 fixtures these share with the
// rest of the family — `harness.restart`, `newProber`, `groupGone`/`groupAlive`,
// `incompleteEvidence`, `harness.waitReleased` — stay in recover_test.go, which
// owns them.

// The #53 split's half that belongs to this ticket: a workspace whose previous
// run may still be live is never reattached. B12 owns the full kill -9
// regression; this owns the verdict.
//
// Parameterized over every answer that is not proof, because that asymmetry *is*
// the contract: `true` frees the workspace, and everything else — a group still
// there, a probe that errored, no prober at all — means possibly live. §7.5 says
// only ESRCH proves disappearance, and the cost of the two mistakes is not
// comparable: a wrong `false` costs a retained claim and another tick, a wrong
// `true` puts a second agent in a live worktree.
func TestAPossiblyLiveMarkerIsNeverReattached(t *testing.T) {
	probeFailed := errors.New("EPERM: not ours to signal")
	for _, tc := range []struct {
		name  string
		probe func(core.RunEvidence) (bool, error)
	}{
		{name: "the group is still there", probe: groupAlive},
		{
			name:  "the probe errored, which is not an answer",
			probe: func(core.RunEvidence) (bool, error) { return false, probeFailed },
		},
		{
			// An error dominates whatever bool comes with it. A prober that returned
			// `true, err` and was believed would free a workspace on a read that
			// failed — the exact shape §9.10 forbids everywhere else.
			name:  "the probe errored while also claiming the group is gone",
			probe: func(core.RunEvidence) (bool, error) { return true, probeFailed },
		},
		{
			// A capability absence is not a fact either. Nil is a daemon that cannot
			// ask, and one that treated it as "free" would reattach every workspace
			// on a substrate it does not know how to probe.
			name:  "there is no prober at all",
			probe: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, harnessOpts{
				issues:  []core.Issue{fake.Issue("1", epoch)},
				script:  startedOnly,
				hang:    true,
				runGone: groupGone,
			})
			h.WaitState("1", StateRunning)
			preparesBefore := len(h.Workspaces.Prepares("1"))
			startsBefore := h.Runner.StartCount()
			if m, ok := h.Workspaces.RunMarkerFor("1"); !ok || m.State != core.RunMarkerIdentified {
				t.Fatalf("marker = %+v (present=%v); this test needs an identified marker to probe", m, ok)
			}

			// The group outlived the daemon, which every attempt's own process group
			// makes reachable under kill -9 (harness run.go, Setpgid).
			probe := newProber(tc.probe)
			if err := h.restart(harnessOpts{runGone: probe.probe, verifier: incompleteEvidence}); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			// Retained: the claim stands, the workspace is neither disposed nor
			// reused, and no agent is dispatched. The question is asked again later.
			h.Tick()
			if got := h.Tracker.ReleaseCount("1"); got != 0 {
				t.Errorf("released %d times; a possibly-live workspace releases nothing", got)
			}
			if got := h.Workspaces.Disposals("1"); len(got) != 0 {
				t.Errorf("disposed %+v; a possibly-live workspace is not disposed", got)
			}
			if got := len(h.Workspaces.Prepares("1")); got != preparesBefore {
				t.Errorf("prepared %d more times; a possibly-live workspace is never reattached",
					got-preparesBefore)
			}
			if got := h.Runner.StartCount(); got != startsBefore {
				t.Errorf("started %d more runs; the whole point is that no second agent enters a live worktree",
					got-startsBefore)
			}
			// The tracker repair still happened: the precondition governs the
			// workspace alone, so a possibly-live one must not suppress the
			// projection §9.10 owes (step 4).
			if got := h.Tracker.Label("1"); got != core.StateLabelClaimed {
				t.Errorf("label = %q, want ben:claimed — tracker-only repair is not suppressed by the "+
					"workspace precondition", got)
			}

			// And it converges once the group is confirmed gone, with no human — the
			// reason option 1 was chosen over accept-and-park.
			probe.set(groupGone)
			h.Tick()
			h.WaitState("1", StateBackoff)
		})
	}
}

// The marker's lifetime is the fact it stands in for: written before the launch,
// removed only when the group is confirmed gone. Both halves are load-bearing —
// written after the launch, a crash in the window leaves a live group with no
// marker; removed on anything less than confirmed, a live worktree reads as free.
func TestTheRunMarkerIsWrittenBeforeTheLaunchAndClearedOnlyWhenQuiet(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	if len(h.Workspaces.MarkerWrites) == 0 {
		t.Fatal("no run marker was written before the launch; a workspace with no marker reads as free")
	}
	if got := h.Workspaces.MarkerClears; len(got) != 0 {
		t.Errorf("the marker was cleared while a run was live: %v", got)
	}
	// The upgrade landed, which is what turns "something may be live here" into a
	// question a later daemon can actually ask.
	m, ok := h.Workspaces.RunMarkerFor("1")
	if !ok || m.State != core.RunMarkerIdentified {
		t.Fatalf("marker = %+v (present=%v), want identified once the run exists", m, ok)
	}
	if m.Evidence.Scheme != core.RunEvidenceLocal || m.Evidence.ID == "" || m.Evidence.Boot == "" {
		t.Errorf("evidence = %+v; a bare id cannot be asked about across a reboot", m.Evidence)
	}
}

func TestTheRunMarkerIsClearedOnAConfirmedTermination(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		runGone: groupGone,
	})
	h.WaitState("1", StateDone)

	if got := h.Workspaces.MarkerClears; len(got) == 0 {
		t.Error("the marker was never cleared for a run that finished; the next start would park the issue")
	}
	if m, ok := h.Workspaces.RunMarkerFor("1"); ok {
		t.Errorf("marker %+v survives a confirmed termination", m)
	}
}

// An unconfirmed stop must not free the workspace. This is the run-time half of
// the same asymmetry recovery applies: only proof of absence frees anything.
func TestAnUnconfirmedStopDoesNotClearTheMarker(t *testing.T) {
	h := start(t, harnessOpts{
		issues:          []core.Issue{fake.Issue("1", epoch)},
		script:          startedOnly,
		hang:            true,
		stopUnconfirmed: true,
		runGone:         groupGone,
	})
	h.WaitState("1", StateRunning)

	// A human closes the issue, so reconciliation stops the run — and the stop
	// comes back unconfirmed.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	if got := h.Workspaces.MarkerClears; len(got) != 0 {
		t.Errorf("the marker was cleared on an unconfirmed termination: %v — the workspace would read as free "+
			"while a process may still be writing to it", got)
	}
	if m, ok := h.Workspaces.RunMarkerFor("1"); !ok || m.State != core.RunMarkerIdentified {
		t.Errorf("marker = %+v (present=%v), want it standing until the group is confirmed gone", m, ok)
	}
}

// A marker read that failed is not an absence either — the one reading that
// would hand a possibly-live worktree to a second agent.
func TestAnUnreadableMarkerRetainsRatherThanFreeing(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	h.Workspaces.SetFailMarkerRead(errors.New("runs/ is not readable"))
	startsBefore := h.Runner.StartCount()
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, tracked := h.o.records["1"]; !tracked {
		t.Fatal("the candidate was dropped; one with no record is dispatchable")
	}
	if got := h.Runner.StartCount(); got != startsBefore {
		t.Errorf("started %d runs against a workspace nobody could ask about", got-startsBefore)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposed %+v on an unreadable marker", got)
	}
	// It is a retained candidate rather than a park: nothing was projected, because
	// "we could not look" is not a verdict about the issue.
	if got := h.Tracker.Label("1"); got != core.StateLabelRunning {
		t.Errorf("label = %q, want ben:running untouched", got)
	}
}

// A run confirmed gone frees the workspace, and the marker that said otherwise
// comes off *here*. Left standing it is a lie with a delay in it: this pass
// resumes the issue, and the next start reads the stale marker as unknown_launch
// and parks what this one already fixed.
func TestConfirmedAbsenceClearsTheMarker(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)
	if m, ok := h.Workspaces.RunMarkerFor("1"); !ok || m.State != core.RunMarkerIdentified {
		t.Fatalf("marker = %+v (present=%v); this test needs one to clear", m, ok)
	}

	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateBackoff)

	if m, ok := h.Workspaces.RunMarkerFor("1"); ok {
		t.Errorf("marker %+v survived a run recovery proved gone; the next start would park this issue", m)
	}
}

// If the marker cannot be removed, the workspace is not treated as free: the read
// is retried rather than proceeding on a state that could not be written.
func TestAMarkerThatCannotBeClearedRetainsTheCandidate(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	h.Workspaces.SetFailMarker(errors.New("state directory is read-only"))
	startsBefore := h.Runner.StartCount()
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, tracked := h.o.records["1"]; !tracked {
		t.Fatal("the candidate was dropped; one with no record is dispatchable")
	}
	if got := h.Runner.StartCount(); got != startsBefore {
		t.Errorf("started %d runs on a workspace whose marker could not be cleared", got-startsBefore)
	}

	// It converges once the write lands.
	h.Workspaces.SetFailMarker(nil)
	h.Tick()
	h.WaitState("1", StateBackoff)
}

// A launch that never happened leaves a marker nothing else will ever clear:
// beginStart writes it before trying, and an attempt with no process has no group
// to confirm gone, so confirmQuiet is not on its path.
func TestAFailedLaunchClearsItsOwnMarker(t *testing.T) {
	h := start(t, harnessOpts{
		issues:    []core.Issue{fake.Issue("1", epoch)},
		failStart: errors.New("exec: no such file"),
		runGone:   groupGone,
	})
	// `failed` releases the claim, so the record is dropped once the release lands.
	h.WaitGone("1")

	if len(h.Workspaces.MarkerWrites) == 0 {
		t.Fatal("no marker was written before the launch; the fixture is not exercising the window")
	}
	if m, ok := h.Workspaces.RunMarkerFor("1"); ok {
		t.Errorf("marker %+v survived a launch that never happened; the next start would park this issue", m)
	}
}

// A run marker that could not be cleared is retried, rather than logged and left.
//
// Leaving it is fail-closed but strictly worse than the truth: the next start
// reads a stale marker as unknown_launch and parks an issue whose run this daemon
// watched finish. The fault is usually transient — a full disk, a remounted state
// directory — so one retry per tick is the cheap side of the trade.
func TestAMarkerClearThatFailedIsRetriedOnLaterTicks(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	// The clear fails at the moment the run is confirmed gone.
	h.Workspaces.SetFailMarker(errors.New("state directory is read-only"))
	// A human closes the issue, so reconciliation stops the run — and the stop is
	// confirmed, which is the moment the marker must come off. It also releases the
	// claim and forgets the record, which is the whole reason the retry cannot live
	// on one.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	h.waitReleased("1", 1)

	if m, ok := h.Workspaces.RunMarkerFor("1"); !ok || m.State != core.RunMarkerIdentified {
		t.Fatalf("marker = %+v (present=%v); this fixture needs the clear to have failed", m, ok)
	}

	// The write becomes possible again and a later tick lands it, with no restart
	// and no operator.
	h.Workspaces.SetFailMarker(nil)
	h.Tick()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := h.Workspaces.RunMarkerFor("1"); !ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	m, _ := h.Workspaces.RunMarkerFor("1")
	t.Errorf("marker %+v still stands after the write became possible; the next start would park this issue", m)
}

// A launch that never happened clears its marker even when an exit or a drain
// overtakes the result. Both of those return before the failure branch, so a clear
// that lived there was skipped on exactly the paths a crash is most likely to
// interleave with.
func TestAFailedLaunchClearsItsMarkerEvenWhenAnExitOvertakesIt(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	h := start(t, harnessOpts{
		issues:    []core.Issue{fake.Issue("1", epoch)},
		failStart: errors.New("exec: no such file"),
		// The launch is held open, so the decision below lands while it is still out.
		startGate: func() { <-release },
		runGone:   groupGone,
	})
	waitFor(t, "the launch to be in flight", func() bool { return h.Runner.StartCount() == 0 && len(h.Workspaces.MarkerWrites) > 0 })

	// A human closes the issue: reconciliation orders an exit, which finishIfRequested
	// applies the moment the launch reports back — ahead of the failure branch.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	unblock()

	h.WaitGone("1")
	if m, ok := h.Workspaces.RunMarkerFor("1"); ok {
		t.Errorf("marker %+v survived a launch that never happened; the next start would park this issue "+
			"even though no process ever existed", m)
	}
}

// A run marker that could not be cleared is retried against the provider that
// wrote it, not whichever is in force when the retry runs.
//
// A marker is a file under one provider's root, and AdoptIdentity permits an
// identity change — a new workspace.root — as soon as no record is outstanding,
// which the paths that clear markers are about to make true by being forgotten.
// Clearing the same key under the new root would remove nothing and leave the
// original standing forever: a workspace parked at every future start, for a run
// this daemon watched finish.
func TestAPendingMarkerClearGoesBackToItsOwnProvider(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	// The clear fails when the run is confirmed gone.
	h.Workspaces.SetFailMarker(errors.New("state directory is read-only"))
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()
	h.waitReleased("1", 1)
	original := h.Workspaces
	if m, ok := original.RunMarkerFor("1"); !ok || m.State != core.RunMarkerIdentified {
		t.Fatalf("marker = %+v (present=%v); this fixture needs the clear to have failed", m, ok)
	}

	// A reload installs a *different* provider — a new workspace.root, which is what
	// AdoptIdentity allows once nothing is outstanding.
	replacement := fake.NewWorkspaces()
	reloaded := *h.Bundle
	reloaded.Workspaces = replacement
	h.Source.publish(h.def, &reloaded, nil)

	// The write becomes possible again. The retry must land on the original.
	original.SetFailMarker(nil)
	h.tickUntil("the pending clear to land", func() bool {
		_, still := original.RunMarkerFor("1")
		return !still
	})
	if got := replacement.MarkerClears; len(got) != 0 {
		t.Errorf("the retry cleared %v under the new provider; the marker it was retrying lives under the old one", got)
	}
}

// A clear left pending must never delete a *newer* run's marker.
//
// Run A's clear fails; the issue is re-queued and run B starts, writing its own
// marker at the same key. A's retry would remove B's — and a crash after that reads
// the workspace as free and puts a second agent into it, which is the exact failure
// the precondition exists to prevent.
func TestAPendingClearNeverErasesANewerRunsMarker(t *testing.T) {
	// The marker failure has to be in place before run A *ends*, and the first
	// attempt is dispatched by the startup tick — so the first prepare is held open
	// long enough to arm it.
	ready := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		// Attempt 1 fails retryably, so the record reaches backoff and a second
		// attempt follows on the same workspace. No `hang`: the first run has to
		// *end* for its group to be confirmed gone, which is what triggers the clear.
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureStalled)
			}
			return startedOnly(core.RunSpec{}, attempt)
		},
		prepareGate: func() { <-ready },
		runGone:     groupGone,
	})

	// Only the *removal* fails: a launch must still be able to write its own marker,
	// which is the whole shape of this hazard.
	h.Workspaces.SetFailMarkerClear(errors.New("state directory is read-only"))
	close(ready)
	waitFor(t, "run A's clear to be pending", func() bool {
		return len(h.Logs.find("could not clear the run marker")) > 0
	})

	// Run B is held open from here, so its marker stays live: a run that ends clears
	// its own marker, correctly, and would make the assertion below unfalsifiable.
	h.Runner.SetHangAfterScript(true)

	// Run B starts while the clear is *still* pending — which is the hazard: the
	// removal is owed against a key a new run is about to own. Its BeginRun succeeds
	// regardless, since only the removal is failing.
	h.tickUntil("run B to start", func() bool { return h.Runner.StartCount() >= 2 })
	waitFor(t, "run B's marker", func() bool {
		m, ok := h.Workspaces.RunMarkerFor("1")
		return ok && m.State == core.RunMarkerIdentified
	})

	waitFor(t, "the stale clear to be abandoned", func() bool {
		return len(h.Logs.find("abandoning a pending run-marker clear")) > 0
	})

	// Abandoned rather than merely deferred. The removal is possible again now, so a
	// clear still on the queue would succeed — and delete the marker of the run that
	// is presently going, which is what a crash then reads as a free workspace.
	h.Workspaces.SetFailMarkerClear(nil)
	for range 3 {
		h.Tick()
	}
	m, ok := h.Workspaces.RunMarkerFor("1")
	if !ok {
		t.Fatal("a stale clear removed the live run's marker; a crash now would put a second agent " +
			"into that worktree (SPEC §9.10)")
	}
	if m.State != core.RunMarkerIdentified {
		t.Errorf("marker = %+v, want the live run's", m)
	}
}

// A pending clear must not outlive the moment a new attempt commits to writing its
// own marker — and `Start` can block for as long as a process takes to exec, so the
// window between the write and the launch reporting back is arbitrarily wide.
//
// Driven by holding `Start` open across a tick, which is what a slow exec looks
// like. Without the abandon happening in beginStart, that tick's retry deletes the
// marker the worker has already written and leaves a live process markerless.
func TestAPendingClearCannotDeleteAMarkerWhileStartIsInFlight(t *testing.T) {
	launched := make(chan struct{})
	release := make(chan struct{})
	var starts atomic.Int64
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	ready := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureStalled)
			}
			return startedOnly(core.RunSpec{}, attempt)
		},
		prepareGate: func() { <-ready },
		// Attempt 2's launch hangs inside Start, after its marker has been written.
		// Attempt 1 passes straight through: it has to run and fail for there to be
		// a clear pending at all.
		startGate: func() {
			if starts.Add(1) == 1 {
				return
			}
			once.Do(func() { close(launched) })
			<-release
		},
		runGone: groupGone,
	})

	// Only the removal fails, so attempt 1 leaves a clear pending while attempt 2
	// can still write its own marker.
	h.Workspaces.SetFailMarkerClear(errors.New("state directory is read-only"))
	close(ready)
	waitFor(t, "run A's clear to be pending", func() bool {
		return len(h.Logs.find("could not clear the run marker")) > 0
	})

	// Attempt 2's marker is written, and its Start is still out.
	h.Workspaces.SetFailMarkerClear(nil)
	h.tickUntil("attempt 2's launch to be in flight", func() bool {
		select {
		case <-launched:
			return true
		default:
			return false
		}
	})
	waitFor(t, "attempt 2's marker", func() bool {
		_, ok := h.Workspaces.RunMarkerFor("1")
		return ok
	})

	// Ticks land while the launch is still blocked. This is the window.
	for range 3 {
		h.Tick()
	}
	if _, ok := h.Workspaces.RunMarkerFor("1"); !ok {
		t.Fatal("a stale clear removed the marker of a run whose Start had not yet returned; the process " +
			"is live and its workspace now reads as free (SPEC §9.10)")
	}
	unblock()
}

// A run-marker removal runs off the authority goroutine.
//
// The real one unlinks a file and fsyncs its directory (workspace ClearRun), and
// every caller reaches it from a terminal event — a confirmed stop, a launch that
// never happened, a drain. Performed on the loop that put two fsyncs between a live
// agent's event and the loop that routes it, between the budget enforcer and a cost
// event, and inside every shutdown; and the retry pass put one there per pending
// clear per tick, for as long as the state directory stayed unwritable.
//
// Driven by wedging the removal and checking that the loop still reaches a verdict.
// Asserted on a counter that must *advance* — a candidate fetch, the cheapest thing
// the loop cannot fake — rather than on a condition that might already hold.
func TestAMarkerClearDoesNotBlockTheAuthorityGoroutine(t *testing.T) {
	clearing := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	// The first Prepare is held open long enough to arm the gate: the startup tick
	// dispatches immediately, and the run it starts is the one whose end clears the
	// marker.
	ready := make(chan struct{})
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		prepareGate: func() { <-ready },
		runGone:     groupGone,
	})
	h.Workspaces.SetMarkerClearGate(func() {
		once.Do(func() { close(clearing) })
		<-release
	})
	close(ready)

	waitFor(t, "the run to end and its marker removal to begin", func() bool {
		select {
		case <-clearing:
			return true
		default:
			return false
		}
	})

	before := h.Tracker.FetchReads()
	deadline := time.Now().Add(5 * time.Second)
	for h.Tracker.FetchReads() <= before && time.Now().Before(deadline) {
		h.Clock.BlockUntilWaiters(1)
		h.Clock.Advance(time.Duration(h.def.Config.Polling.IntervalMS) * time.Millisecond)
		time.Sleep(5 * time.Millisecond)
	}
	if got := h.Tracker.FetchReads(); got <= before {
		t.Errorf("no candidate fetch completed while a run-marker removal was outstanding "+
			"(%d, was %d); the removal is on the authority goroutine, so an unwritable state "+
			"directory stalls the whole loop", got, before)
	}
	unblock()
}

// A launch must not write its own marker while a removal it could only abandon *too
// late* is still executing.
//
// Abandoning is a decision the loop takes; the removal does not run there. So the two
// overlap, and an `os.Remove` landing after the launch's `writeMarker` leaves a live
// process with no marker at all — a workspace that reads as free with an agent in it,
// which is the one thing this precondition exists to prevent. The loop can drop a
// clear that has not started and cannot recall one that has, so the launch waits it
// out in its own worker instead.
//
// Driven by wedging run A's removal and letting attempt 2 reach its launch inside that
// window — which is where the abandon log line puts it.
func TestALaunchWaitsOutAClearItCouldOnlyAbandonTooLate(t *testing.T) {
	clearing := make(chan struct{})
	release := make(chan struct{})
	var once, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	ready := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		// Attempt 1 fails retryably, so a second attempt follows on the same
		// workspace. No `hang`: run A has to *end* for its group to be confirmed gone,
		// which is what orders the clear.
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureStalled)
			}
			return startedOnly(core.RunSpec{}, attempt)
		},
		prepareGate: func() { <-ready },
		runGone:     groupGone,
	})
	h.Workspaces.SetMarkerClearGate(func() {
		once.Do(func() { close(clearing) })
		<-release
	})
	close(ready)

	waitFor(t, "run A's marker removal to be executing", func() bool {
		select {
		case <-clearing:
			return true
		default:
			return false
		}
	})
	if got := h.Workspaces.MarkerWriteCount(); got != 1 {
		t.Fatalf("marker writes = %d, want run A's alone at this point", got)
	}
	// Run B is held open from here, so its marker stays live: a run that ends clears
	// its own marker, correctly, and would make the final assertion unfalsifiable.
	// After run A's clear, not before — run A has to end for there to be a clear at
	// all.
	h.Runner.SetHangAfterScript(true)

	// Attempt 2 reaches its launch inside the window, which the abandon says on the
	// loop, naming the removal it found already executing.
	h.tickUntil("attempt 2 to reach its launch and find the removal in flight", func() bool {
		for _, rec := range h.Logs.find("abandoning a pending run-marker clear") {
			if rec.Attrs["in_flight"] == "true" {
				return true
			}
		}
		return false
	})

	// Ticks land while the removal is still wedged. The launch is past the abandon and
	// must be waiting rather than writing.
	for range 3 {
		h.Tick()
	}
	if got := h.Workspaces.MarkerWriteCount(); got != 1 {
		t.Fatalf("marker writes = %d, want 1: attempt 2 wrote its marker while the removal it abandoned "+
			"was still executing, so that removal is about to delete it (SPEC §9.10)", got)
	}

	// The removal lands, and only then does the launch write.
	unblock()
	waitFor(t, "attempt 2's marker", func() bool {
		m, ok := h.Workspaces.RunMarkerFor("1")
		return ok && m.State == core.RunMarkerIdentified
	})
	// And it stays: nothing is owed against that key any more.
	for range 3 {
		h.Tick()
	}
	m, ok := h.Workspaces.RunMarkerFor("1")
	if !ok {
		t.Fatal("the abandoned removal deleted the live run's marker after all; a crash now would put a " +
			"second agent into that worktree (SPEC §9.10)")
	}
	if m.State != core.RunMarkerIdentified {
		t.Errorf("marker = %+v, want the live run's", m)
	}
}

// A clear owed against a *previous root* survives a new run's marker: only the
// current store's file is being replaced, and a clear against another root refers to
// a different file under a different path.
//
// Driven against an orchestrator that is *not running* (idleOrchestrator, which
// held_test.go already keeps for this). These are loop-owned fields, so calling the
// helpers from a test goroutine while the loop is going is a race — the detector said
// so on the first version — and the answer is to have no second goroutine rather than
// to lock what the loop owns exclusively.
//
// Recorded through rememberPendingClear rather than through clearMarkerWith, which
// also *starts* the removal in a worker: what is under test here is the matching
// rule, and a pending clear that is executing is a different state with its own
// answer (see TestALaunchWaitsOutAClearItCouldOnlyAbandonTooLate).
func TestANewMarkerDoesNotAbandonAClearOwedToAnotherRoot(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker())
	current := markerStore{ws: fake.NewWorkspaces(), root: "/srv/ben/current"}
	previous := markerStore{ws: fake.NewWorkspaces(), root: "/srv/ben/previous"}

	// One clear owed in each store, for the same issue.
	o.rememberPendingClear(&pendingClear{issue: core.Issue{Identifier: "1"}, store: previous})
	o.rememberPendingClear(&pendingClear{issue: core.Issue{Identifier: "1"}, store: current})
	if len(o.markerClears) != 2 {
		t.Fatalf("pending clears = %d, want one per store — a set keyed by issue alone drops the first",
			len(o.markerClears))
	}

	// A new run writes its own marker in the current store.
	if waits := o.abandonPendingClears("1", current); len(waits) != 0 {
		t.Errorf("waits = %d, want none: neither clear had started, so both could simply be dropped", len(waits))
	}

	if len(o.markerClears) != 1 {
		t.Fatalf("pending clears = %d, want the previous root's to survive", len(o.markerClears))
	}
	if o.markerClears[0].store.root != previous.root {
		t.Error("the surviving clear is not the previous root's; a clear against an old root refers to a " +
			"different file, and dropping it strands a marker nothing ever removes")
	}
}

// A removal that failed is retried on the next tick — the sentence the log promises —
// and not on the instant it failed.
//
// Re-driving the queue from onMarkerCleared unconditionally spends a worker and a log
// line per failure, continuously, for as long as the state directory stays unwritable.
// It also breaks shutdown outright: drained() refuses while a removal is executing, and
// the re-arm happens inside the very handler whose deferred driveShutdown makes that
// check, so the check never sees a turn with nothing in flight and the drain never
// completes.
func TestAFailedMarkerRemovalRetriesOnTheTickNotOnTheInstant(t *testing.T) {
	ready := make(chan struct{})
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		prepareGate: func() { <-ready },
		runGone:     groupGone,
	})
	// A launch that never happens leaves a marker describing nothing, and clearing it
	// is what fails — so the removal stays owed while the record itself terminalizes.
	h.Runner.SetFailStart(errors.New("exec: no such file or directory"))
	h.Workspaces.SetFailMarkerClear(errors.New("state directory is read-only"))
	close(ready)
	waitFor(t, "the removal to fail", func() bool {
		return len(h.Logs.find("could not clear the run marker")) > 0
	})

	// One attempt per tick, and no more. On a queue that re-drives itself this counter
	// runs into the thousands over the same span.
	before := h.Workspaces.MarkerClearAttempts()
	const ticks = 3
	for range ticks {
		h.Tick()
	}
	if got := h.Workspaces.MarkerClearAttempts() - before; got > ticks {
		t.Errorf("the failed removal was attempted %d times over %d ticks; it is retried on the tick, not "+
			"on the instant it failed (SPEC §9.10)", got, ticks)
	}

	// And the drain completes rather than being held open by a removal that re-arms
	// itself on every result.
	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done <- h.o.Shutdown(ctx)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown never completed while a run-marker removal kept failing; the drain waits for an " +
			"executing removal, so a re-arm on every result means it never sees one land")
	}
}

// A removal's outcome must land on the obligation it was *for*.
//
// The sequence: run A's removal is executing; run B's launch abandons it too late and
// waits it out; B writes its marker, runs, and owes a removal of its own — all before
// the loop gets round to A's result. Reuse A's object for B's obligation and A's
// success retires it, leaving B's marker standing. A pre-evidence launch failure makes
// that permanent: the marker carries no evidence, so §9.10 reads unknown_launch and
// parks for a human, and nothing ever looks at that key again.
//
// Staged directly rather than raced, because the interleaving is between two workers
// and a loop and the point is that *no* interleaving can lose the obligation.
func TestAStaleClearResultDoesNotDiscardANewerObligation(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker())
	store := markerStore{ws: fake.NewWorkspaces(), root: "/srv/ben/root"}
	issue := core.Issue{Identifier: "1"}

	// Run A's removal is executing.
	a := &pendingClear{issue: issue, store: store}
	o.rememberPendingClear(a)
	a.inFlight, a.done = true, make(chan struct{})

	// Run B's launch overtakes it: too late to drop, so it is abandoned and awaited.
	if waits := o.abandonPendingClears("1", store); len(waits) != 1 {
		t.Fatalf("waits = %d, want the executing removal's; a launch that does not wait it out has its "+
			"own marker deleted", len(waits))
	}
	if !a.abandoned {
		t.Fatal("the executing removal was not marked abandoned")
	}

	// B writes its marker, and its launch fails — so a removal is owed for the
	// evidence-less marker B left behind, while A's removal is still executing.
	b := &pendingClear{issue: issue, store: store}
	o.rememberPendingClear(b)
	if len(o.markerClears) != 2 {
		t.Fatalf("pending clears = %d, want A's and B's: reusing A's entry hands A's result B's "+
			"obligation", len(o.markerClears))
	}
	// And B does not race A for the file. Asserted by driving the queue rather than by
	// asking clearExecutingFor, which answers correctly whether or not anything calls
	// it — the mutation ledger found exactly that gap.
	o.driveMarkerClears(context.Background())
	if b.inFlight {
		t.Error("B's removal started while A's was still executing; two workers unlinking one path")
	}

	// A's result finally lands, and retires A alone.
	o.onMarkerCleared(context.Background(), signal{kind: sigMarkerCleared, clear: a})

	if len(o.markerClears) != 1 {
		t.Fatalf("pending clears = %d, want B's to survive: A's success retired an obligation it was never "+
			"for, and the marker B wrote parks that issue at every future start (SPEC §9.10)",
			len(o.markerClears))
	}
	if got := o.markerClears[0]; got == a {
		t.Error("A's entry survived and B's was dropped")
	} else if got.abandoned {
		t.Error("B's obligation is marked abandoned; onMarkerCleared will retire it without removing anything")
	}
	// And it is driven rather than left waiting for a tick: its predecessor is gone.
	if !o.markerClears[0].inFlight {
		t.Error("B's removal was not started once A's landed; it would wait a whole poll interval")
	}
}

// The other half of the same rule, and the one a provider-instance comparison got
// wrong: a rebuilt provider over an unchanged workspace.root is the **same** marker
// store.
//
// A hook edit or a credential rotation rebuilds every adapter while keeping the root,
// and §5.4 gives both of those to the reload however busy the daemon is — so this is
// not an exotic sequence, it is what an operator saving WORKFLOW.md does. Two
// instances over one root write the very same file, so a clear owed to the old one
// survives a new run's marker write and its retry then deletes the marker of a
// running agent, which is the deletion the abandon exists to prevent.
func TestARebuiltProviderIsTheSameMarkerStore(t *testing.T) {
	o := idleOrchestrator(t, fake.NewTracker())
	const root = "/srv/ben/root"
	// Distinct instances, one root: what a rebuild produces.
	before := markerStore{ws: fake.NewWorkspaces(), root: root}
	after := markerStore{ws: fake.NewWorkspaces(), root: root}
	if before.ws == after.ws {
		t.Fatal("this test needs two distinct provider instances to be about anything")
	}

	o.rememberPendingClear(&pendingClear{issue: core.Issue{Identifier: "1"}, store: before})
	o.abandonPendingClears("1", after)

	if len(o.markerClears) != 0 {
		t.Fatalf("pending clears = %d, want the clear dropped: the rebuilt provider writes the same file, "+
			"so retrying the removal would delete the marker of the run that is presently starting",
			len(o.markerClears))
	}
}
