package harness

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// A boot identity that is empty on a supported platform would silently disarm
// the reuse guard: a marker with no boot could never be shown stale, so every
// dead pgid would read as possibly-live forever (SPEC §9.10).
func TestBootIdentityIsResolvable(t *testing.T) {
	if got := bootID(); got == "" {
		t.Error("bootIdentity() = \"\": §9.10's marker cannot distinguish a pgid " +
			"from this boot from a reused one, so no evidence can ever prove a run gone")
	}
	// Stable at the source, not merely cached: a marker written at launch has to
	// still match at recovery, and those are two different processes, so
	// sync.OnceValue proves nothing about it. Compare the cached value against a
	// fresh read of the platform.
	if fresh := bootIdentity(); fresh != bootID() {
		t.Errorf("bootIdentity() = %q on a re-read, cached %q: a marker written at "+
			"launch would not match at recovery on the same boot", fresh, bootID())
	}
}

// The evidence must name a group the kernel agrees exists, or recovery's probe
// is asking about the wrong thing.
func TestOnRunReceivesTheLiveGroup(t *testing.T) {
	var got core.RunEvidence
	handle := startSleeper(t, func(e core.RunEvidence) error {
		got = e
		return nil
	})
	defer handle.Stop(context.Background(), core.StopDiscard)

	if got.Scheme != core.RunEvidenceLocal {
		t.Errorf("Scheme = %q, want %q", got.Scheme, core.RunEvidenceLocal)
	}
	if got.Boot == "" {
		t.Error("Boot is empty: the pgid cannot be bound to this boot")
	}
	pgid := atoiOrFatal(t, got.ID)
	// Signal 0 delivers nothing and reports existence — the same question
	// recovery asks, so this is the evidence being usable, not merely present.
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Errorf("kill(-%d, 0) = %v: the recorded group is not the live one", pgid, err)
	}
}

// The rule the whole seam rests on: once a process exists, Start returns a
// handle. A sink failure is a live run whose evidence could not be recorded —
// the caller must still be able to stop it (SPEC §7.4, §9.10).
func TestSinkFailureStillReturnsAHandleAndEndsTheRun(t *testing.T) {
	handle := startSleeper(t, func(core.RunEvidence) error {
		return errors.New("marker upgrade failed")
	})
	if handle == nil {
		t.Fatal("Start returned no handle after the process existed: a live group " +
			"with nobody to stop it")
	}
	// The run is failed through the ordinary ladder rather than left running.
	select {
	case <-handle.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("run did not end after its evidence could not be recorded")
	}
	var terminal core.Event
	for e := range handle.Events() {
		if IsTerminal(e.Type) {
			terminal = e
		}
	}
	if terminal.Type != core.EventFailed || terminal.Reason != core.FailureLaunchError {
		t.Errorf("terminal event = %v/%v, want failed/%v",
			terminal.Type, terminal.Reason, core.FailureLaunchError)
	}
}

// A nil sink is the probe/test path and must not be a launch failure.
func TestNilSinkLaunchesNormally(t *testing.T) {
	handle := startSleeper(t, nil)
	defer handle.Stop(context.Background(), core.StopDiscard)
	if handle == nil {
		t.Fatal("Start with no sink returned no handle")
	}
}

// startSleeper launches a child that outlives the call, so the group is still
// there to be observed.
func startSleeper(t *testing.T, sink func(core.RunEvidence) error) core.RunHandle {
	t.Helper()
	handle, err := Start(context.Background(), Launch{
		Name:      "evidence-test",
		Argv:      []string{"/bin/sh", "-c", "sleep 30"},
		Env:       []string{"PATH=/usr/bin:/bin"},
		Dir:       t.TempDir(),
		Translate: func([]byte) []core.Event { return nil },
		Timings:   Timings{StopGrace: 50 * time.Millisecond, PostExitDrain: 50 * time.Millisecond},
		OnRun:     sink,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return handle
}

func atoiOrFatal(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		t.Fatalf("evidence ID %q is not a pgid: %v", s, err)
	}
	return n
}

// A verdict claimed after a terminal event has been published cannot replace it,
// so publication must not begin until the marker verdict is known. Without the
// gate a fast child reports `succeeded` while its workspace stays possibly-live
// with an un-upgraded marker — the run reads as finished and its worktree is
// reattachable (SPEC §9.10).
//
// The window is the sink's own cost: a real upgrade writes and fsyncs, so the
// sleep here is what makes the race deterministic rather than incidental.
func TestFastSuccessDoesNotOutrunSinkFailure(t *testing.T) {
	handle, err := Start(context.Background(), Launch{
		Name: "race",
		Argv: []string{"/bin/sh", "-c", "echo ok"},
		Env:  []string{"PATH=/usr/bin:/bin"},
		Dir:  t.TempDir(),
		Translate: func([]byte) []core.Event {
			return []core.Event{{Type: core.EventSucceeded}}
		},
		Timings: Timings{StopGrace: 20 * time.Millisecond, PostExitDrain: 20 * time.Millisecond},
		OnRun: func(core.RunEvidence) error {
			time.Sleep(50 * time.Millisecond)
			return errors.New("marker upgrade failed")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var terminal core.Event
	for e := range handle.Events() {
		if IsTerminal(e.Type) {
			terminal = e
		}
	}
	<-handle.Done()
	if terminal.Type != core.EventFailed || terminal.Reason != core.FailureLaunchError {
		t.Fatalf("terminal = %v/%v, want failed/launch_error: a run whose evidence was "+
			"never recorded was reported as a success", terminal.Type, terminal.Reason)
	}
}

// Concurrent launches each receive their own group's evidence, and Start never
// hands two of them the same id.
//
// Scope, stated because an earlier version of this comment overclaimed: this
// pins a *harness* property only. It builds the per-spec closure by hand, so it
// cannot catch a production regression that hoisted the binding to one shared
// identity-free sink — no adapter reads core.RunnerOptions.OnRun yet, so there
// is nothing to hoist. Enforcing that the adapters bind their RunSpec belongs to
// agenttest.Contract and lands with the marker store, whose wiring creates the
// thing to enforce.
func TestConcurrentLaunchesEachRecordTheirOwnGroup(t *testing.T) {
	type record struct {
		spec     core.RunSpec
		evidence core.RunEvidence
	}
	var mu sync.Mutex
	got := map[string]record{}

	var wg sync.WaitGroup
	for _, id := range []string{"issue-1", "issue-2", "issue-3"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spec := core.RunSpec{Workspace: core.WorkspacePaths{Path: filepath.Join(t.TempDir(), id)}}
			// Exactly how an adapter binds it: the spec is closed over, never
			// inferred from the evidence.
			handle, err := Start(context.Background(), Launch{
				Name:      "concurrent",
				Argv:      []string{"/bin/sh", "-c", "sleep 30"},
				Env:       []string{"PATH=/usr/bin:/bin"},
				Dir:       t.TempDir(),
				Translate: func([]byte) []core.Event { return nil },
				Timings:   Timings{StopGrace: 20 * time.Millisecond, PostExitDrain: 20 * time.Millisecond},
				OnRun: func(e core.RunEvidence) error {
					mu.Lock()
					defer mu.Unlock()
					got[id] = record{spec: spec, evidence: e}
					return nil
				},
			})
			if err != nil {
				t.Errorf("Start(%s): %v", id, err)
				return
			}
			handle.Stop(context.Background(), core.StopDiscard)
		}()
	}
	wg.Wait()

	if len(got) != 3 {
		t.Fatalf("recorded %d workspaces, want 3", len(got))
	}
	seen := map[string]string{}
	for id, r := range got {
		if filepath.Base(r.spec.Workspace.Path) != id {
			t.Errorf("%s recorded against workspace %q", id, r.spec.Workspace.Path)
		}
		if prev, dup := seen[r.evidence.ID]; dup {
			t.Errorf("%s and %s recorded the same group %q: one workspace's marker "+
				"carries another's run", id, prev, r.evidence.ID)
		}
		seen[r.evidence.ID] = id
	}
}

// The other side of the gate: a successful sink must not cost the run its own
// result. While the pump waits, the post-exit bound must not strand a reader
// whose collector is merely gated — for a child that exits faster than the
// upgrade takes, the stranded line is the terminal one, and a successful run
// reports failed/crashed.
//
// The sleep is the point: an instant sink passes this while the bug is present.
func TestSlowSuccessfulSinkKeepsTerminalOutput(t *testing.T) {
	handle, err := Start(context.Background(), Launch{
		Name: "slow-sink",
		Argv: []string{"/bin/sh", "-c", "echo ok"},
		Env:  []string{"PATH=/usr/bin:/bin"},
		Dir:  t.TempDir(),
		Translate: func([]byte) []core.Event {
			return []core.Event{{Type: core.EventSucceeded}}
		},
		Timings: Timings{StopGrace: 20 * time.Millisecond, PostExitDrain: 20 * time.Millisecond},
		OnRun: func(core.RunEvidence) error {
			time.Sleep(50 * time.Millisecond)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var terminal core.Event
	for e := range handle.Events() {
		if IsTerminal(e.Type) {
			terminal = e
		}
	}
	<-handle.Done()
	if terminal.Type != core.EventSucceeded {
		t.Fatalf("terminal = %v/%v, want succeeded: the marker gate cost the run its "+
			"own result", terminal.Type, terminal.Reason)
	}
}
