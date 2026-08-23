package integration

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
)

// SPEC §12.3-6: an invalid hot-reload blocks dispatch and spares in-flight runs.
//
// Both halves are asserted, and the second is the one worth being careful about.
// "Blocks dispatch" is easy to satisfy by accident — a daemon that had wedged
// entirely would pass it — so the scenario ends by fixing the file and watching
// the queue move again. Until then the live run is untouched: not stopped, not
// re-dispatched, its claim and its `ben:running` label standing, running under
// the definition its claim was taken against (§5.4: a reload never changes the
// ground under a live run).
//
// The reload path is real. A real file is edited by write-and-rename, which is
// the sequence an editor performs and the one §5.4's settling window exists for,
// a real fsnotify watch sees it, and the real strict loader refuses it.
func TestAnInvalidReloadBlocksDispatchAndSparesTheRunInFlight(t *testing.T) {
	wf := defaultWorkflow()
	// Two slots, so nothing below can be explained by the concurrency cap.
	wf.MaxConcurrent = 2

	h := startWatched(t, scenarioConfig{
		workflow: &wf,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s", Continuation: "s"}}
		},
		before: func(h *scenario) { h.Runner.SetHangAfterScript(true) },
	})
	live := h.waitRunning("7")

	// An operator saves a broken file: `max_turns` is no longer a number.
	h.rewrite(brokenLimits(wf))
	h.waitFor("the reload to be refused", func() bool { return h.watcher.Snapshot().Blocked != nil })

	// New work arrives while the configuration is broken. It is dispatchable by
	// every rule the tracker knows; the only thing standing between it and an
	// agent is §5.4.
	h.Tracker.Set(fake.Issue("8", epoch.Add(time.Minute)))
	h.ticks(3)

	if n := h.dispatches("8"); n != 0 {
		t.Errorf("prepared %d workspaces for issue 8 while dispatch was blocked (SPEC §5.4, §12.3-6)", n)
	}
	if h.hasRecord("8") {
		t.Errorf("issue 8 has a run record while the configuration is invalid; status: %+v", h.o.Status())
	}
	if calls, _, _ := h.Tracker.Snapshot(); slices.Contains(calls, "claim 8") {
		t.Errorf("issue 8 was claimed while dispatch was blocked; calls = %v", calls)
	}

	// The run in flight is spared: not interrupted, not re-decided, and still
	// holding everything it held before the edit.
	h.waitState("7", orchestrator.StateRunning)
	if n := live.StopCount(); n != 0 {
		t.Errorf("the live run was stopped %d times by a config edit; §5.4 spares in-flight runs", n)
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released the live run's claim %d times over an invalid reload", n)
	}
	if got := h.Tracker.Label("7"); got != core.StateLabelRunning {
		t.Errorf("state label = %q, want %q — an invalid reload changes no projection", got, core.StateLabelRunning)
	}
	if n := h.dispatches("7"); n != 1 {
		t.Errorf("prepared %d attempts for the live run; a blocked reload is not a retry", n)
	}

	// The operator fixes it. Dispatch resumes — which is what proves the block
	// above was the reload's doing and not a wedged daemon.
	h.rewrite(wf.render())
	h.waitFor("the reload to be accepted", func() bool { return h.watcher.Snapshot().Blocked == nil })
	h.ticks(2)
	h.waitFor("issue 8 to be dispatched once the configuration is valid again", func() bool {
		return h.dispatches("8") == 1
	})
}

// brokenLimits renders a workflow whose `limits.max_turns` is not a number, so
// the file parses as YAML and fails the loader's strict typing (SPEC §5.3).
//
// Broken at the *value* rather than by truncating the file, because the two are
// refused by different code and only this one proves the daemon is reading the
// new bytes: a half-written file is also what a rename-in-progress looks like,
// and a watcher that refused everything mid-write would pass a truncation test
// while never having parsed anything.
func brokenLimits(w workflow) string {
	const key = "  max_turns: "
	valid := w.render()
	if !strings.Contains(valid, key) {
		panic("the rendered workflow no longer carries " + key + ", so this fixture breaks nothing")
	}
	return strings.Replace(valid, key, key+"\"four\" #", 1)
}
