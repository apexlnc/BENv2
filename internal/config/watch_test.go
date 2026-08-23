package config

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDebounce is short enough to keep tests quick and long enough to still
// coalesce the burst an atomic save produces.
const testDebounce = 20 * time.Millisecond

// atomicSave replaces the file the way an editor does: write a temp file in
// the same directory, then rename it over the target. This is the case a
// single-file watch misses (SPEC §5.4).
func atomicSave(t *testing.T, path, content string) {
	t.Helper()
	// Unique per save: concurrent savers must not collide on one temp name.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".WORKFLOW.md.*.swp")
	if err != nil {
		t.Fatal(err)
	}
	tmp.Close()
	tmpName := tmp.Name()
	if err := os.WriteFile(tmpName, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		t.Fatal(err)
	}
}

// testRuntime stands in for the adapter set. It records the definition it was
// built from, which is what makes the pairing invariant assertable at all.
type testRuntime struct {
	builtFrom *WorkflowDefinition
	changed   AdapterChange
	// serial distinguishes generations, so "carried forward" can be asserted as
	// identity rather than inferred from equal contents.
	serial int
}

// noRuntime is the builder a test passes when it does not care what the runtime
// is — stated rather than defaulted, since Watch requires one.
func noRuntime(_ context.Context, def *WorkflowDefinition, prev *testRuntime, changed AdapterChange) (*testRuntime, error) {
	serial := 1
	if prev != nil {
		serial = prev.serial + 1
	}
	return &testRuntime{builtFrom: def, changed: changed, serial: serial}, nil
}

// pairedWith reports whether this runtime was built from a definition the given
// one can legitimately be paired with: every configuration slice an adapter
// binds is identical, so no adapter is operating under a definition it was not
// constructed and checked Ready against (SPEC §5.7).
//
// Deliberately not `builtFrom == def`, which is false in the legitimate
// carry-forward case: a limits-only edit publishes a new definition and rebuilds
// nothing, and must, or every unrelated knob would churn the adapters.
func (r *testRuntime) pairedWith(def *WorkflowDefinition) bool {
	return r != nil && !adapterChange(r.builtFrom, def).Any()
}

// startWatch starts a watcher on a fresh copy of content and stops it on
// cleanup.
func startWatch(t *testing.T, content string) (*Watcher[*testRuntime], string, *lockedBuffer) {
	t.Helper()
	path := writeWorkflow(t, content)

	logs := &lockedBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: testDebounce, Logger: logger, BuildRuntime: noRuntime})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w, path, logs
}

// startWatchWithWindow is startWatch with the settling window under the
// test's control instead of the clock's.
func startWatchWithWindow(t *testing.T, content string, window debounceTimer) (*Watcher[*testRuntime], string) {
	t.Helper()
	path := writeWorkflow(t, content)

	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce:     testDebounce,
		BuildRuntime: noRuntime,
		newTimer:     func() debounceTimer { return window },
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w, path
}

// lockedBuffer lets the watcher goroutine log while the test reads.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor polls until cond holds, failing the test rather than hanging.
//
// The deadline is a liveness bound, not a timing assertion: a watcher that
// never applies the change fails at any deadline, so a generous one hides
// nothing and only costs time on a test that was going to fail anyway. It has
// to absorb a real filesystem event reaching a goroutine that a loaded machine
// may not schedule promptly — at 2s this was itself an intermittent failure
// under parallel load (#39).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const changedPrompt = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: claude-code
limits:
  max_turns: 9
deployment:
  mode: attended
---
Reworded: {{ issue.title }}.
`

// SPEC §5.4 makes the parent-directory watch normative, so assert it
// structurally rather than only through behavior. Watching the file itself
// happens to survive an atomic save on kqueue platforms, so the behavioral
// test below cannot catch the mistake everywhere — this one can.
// The exact-count half of this assertion was relaxed by #158: a symlinked config
// path is watched along its whole chain, and where a spelling and its resolution
// differ — every path under a temp directory on macOS, since /var is a symlink to
// /private/var — the config's own directory legitimately appears under both. What
// this test exists to catch is unchanged and is still checked exactly: the watch
// is on directories, never on the file.
func TestWatchRegistersTheParentDirectory(t *testing.T) {
	w, path, _ := startWatch(t, validMinimal)

	watched := w.fsw.WatchList()
	dir := filepath.Dir(path)
	if !slices.Contains(watched, dir) {
		t.Fatalf("watching %v, want the parent directory %q among them", watched, dir)
	}
	for _, got := range watched {
		if got == path {
			t.Fatalf("watching the file itself (%q) — a single-file watch dies on the atomic save that replaces the inode", got)
		}
		fi, err := os.Stat(got)
		if err != nil || !fi.IsDir() {
			t.Fatalf("watching %q, which is not a directory (err=%v)", got, err)
		}
	}
}

// B03 acceptance: an editor-style atomic save (write temp + rename) triggers
// a reload. A watch on the file itself would follow the replaced inode and
// never fire (SPEC §5.4).
func TestWatchReloadsOnAtomicSave(t *testing.T) {
	w, path, _ := startWatch(t, validMinimal)

	if got := w.Snapshot().Definition.Config.Limits.MaxTurns; got != DefaultMaxTurns {
		t.Fatalf("max_turns = %d before the edit, want the default %d", got, DefaultMaxTurns)
	}

	atomicSave(t, path, changedPrompt)

	waitFor(t, "the atomic save to be picked up", func() bool {
		return w.Snapshot().Definition.Config.Limits.MaxTurns == 9
	})
	if err := w.Snapshot().Blocked; err != nil {
		t.Errorf("a valid reload must not block dispatch: %v", err)
	}
	if got := w.Snapshot().Definition.PromptTemplate; !strings.Contains(got, "Reworded") {
		t.Errorf("prompt template not reloaded: %q", got)
	}
}

// An in-place write is the other editing style, and must work too.
func TestWatchReloadsOnInPlaceWrite(t *testing.T) {
	w, path, _ := startWatch(t, validMinimal)

	if err := os.WriteFile(path, []byte(changedPrompt), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the in-place write to be picked up", func() bool {
		return w.Snapshot().Definition.Config.Limits.MaxTurns == 9
	})
}

// B03 acceptance: an invalid reload leaves the in-flight config object
// untouched, sets the dispatch-blocked flag, and logs; fixing the file clears
// it without a restart (SPEC §5.4).
func TestWatchInvalidReloadBlocksDispatchAndRecovers(t *testing.T) {
	w, path, logs := startWatch(t, validMinimal)
	before := w.Snapshot().Definition

	atomicSave(t, path, "tracker: [not, a, map]\n")

	waitFor(t, "the invalid reload to block dispatch", func() bool {
		return w.Snapshot().Blocked != nil
	})

	// The definition in-flight runs are holding is untouched — same pointer,
	// not merely an equal value.
	if w.Snapshot().Definition != before {
		t.Error("an invalid reload replaced the last-known-good definition")
	}
	// The log is emitted after the state is published, so wait for it rather
	// than assuming the two land together.
	waitFor(t, "the loud operator error", func() bool {
		return strings.Contains(logs.String(), "level=ERROR")
	})

	// Fixing the file clears the block without a restart.
	atomicSave(t, path, changedPrompt)
	waitFor(t, "the fix to unblock dispatch", func() bool {
		return w.Snapshot().Blocked == nil
	})
	if got := w.Snapshot().Definition.Config.Limits.MaxTurns; got != 9 {
		t.Errorf("max_turns = %d after recovery, want the repaired value 9", got)
	}
	waitFor(t, "the recovery to be announced", func() bool {
		return strings.Contains(logs.String(), "dispatch unblocked")
	})
}

// Every load-time refusal is a blocked dispatch, not a crash and not a
// silent stale config (SPEC §5.4, §5.7).
func TestWatchBlocksOnEveryKindOfInvalidReload(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr error
	}{
		{"non-map front matter", "---\n- a\n- b\n---\nbody\n", ErrFrontMatterNotMap},
		{"missing front matter", "just a prompt\n", ErrMissingFrontMatter},
		{"empty prompt body", "---\ntracker:\n  kind: github\n---\n", ErrEmptyPrompt},
		{"unknown key", strings.Replace(validMinimal, "agent:", "agnet:\n  kind: x\nagent:", 1), nil},
		{"unsupported version", strings.Replace(validMinimal, "---\n", "---\nversion: 99\n", 1), nil},
		{"unknown template variable", strings.Replace(validMinimal, "issue.title", "issue.titel", 1), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, path, _ := startWatch(t, validMinimal)
			good := w.Snapshot().Definition

			atomicSave(t, path, tt.content)
			waitFor(t, "dispatch to block", func() bool { return w.Snapshot().Blocked != nil })

			if w.Snapshot().Definition != good {
				t.Error("last-known-good was replaced by an invalid reload")
			}
			if tt.wantErr != nil && !errors.Is(w.Snapshot().Blocked, tt.wantErr) {
				t.Errorf("blocked with %v, want %v", w.Snapshot().Blocked, tt.wantErr)
			}
		})
	}
}

// A deleted file is an invalid reload like any other: block, keep serving the
// last-known-good, recover when it returns. Never crash (SPEC §5.4).
func TestWatchSurvivesFileDeletion(t *testing.T) {
	w, path, _ := startWatch(t, validMinimal)
	good := w.Snapshot().Definition

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the deletion to block dispatch", func() bool { return w.Snapshot().Blocked != nil })
	if !errors.Is(w.Snapshot().Blocked, ErrMissingWorkflowFile) {
		t.Errorf("blocked with %v, want ErrMissingWorkflowFile", w.Snapshot().Blocked)
	}
	if w.Snapshot().Definition != good {
		t.Error("a deleted file must not disturb the last-known-good")
	}

	atomicSave(t, path, changedPrompt)
	waitFor(t, "the restored file to unblock dispatch", func() bool { return w.Snapshot().Blocked == nil })
}

// fakeWindow is the settling window on virtual time. Sleeping past a real one
// and counting reloads is a race, not a check: on a loaded machine the first
// event's window can expire before the last write of a burst lands, and the
// count then measures the machine rather than the watcher (#39). Here the test
// owns the clock, so "every event pushed the window out" and "one window
// closed over the whole burst" are facts.
type fakeWindow struct {
	c chan time.Time

	mu       sync.Mutex
	now      time.Duration
	deadline time.Duration
	// resets and closes are counters rather than an is-open flag, so that
	// "a window is open" is never transiently false while an event the burst
	// is still delivering is being handled — which the test would race.
	resets int
	closes int
}

func newFakeWindow() *fakeWindow { return &fakeWindow{c: make(chan time.Time)} }

func (f *fakeWindow) C() <-chan time.Time { return f.c }

func (f *fakeWindow) Reset(d time.Duration) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	open := f.resets > f.closes
	f.resets++
	f.deadline = f.now + d
	return open
}

// resetCount is how many events have opened or pushed out the window.
func (f *fakeWindow) resetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resets
}

// advance moves virtual time forward, delivering the end of the window if it
// has now been reached, and reports whether it did. The send is unbuffered, so
// a true return means the watch goroutine has already taken it — the test
// never sleeps to find out.
func (f *fakeWindow) advance(d time.Duration) bool {
	f.mu.Lock()
	f.now += d
	closing := f.resets > f.closes && f.now >= f.deadline
	if closing {
		f.closes = f.resets
	}
	f.mu.Unlock()
	if closing {
		f.c <- time.Time{}
	}
	return closing
}

// The burst an atomic save produces must settle into one reload, not one per
// filesystem event (SPEC §5.4). Half a debounce passes between writes, so the
// burst spans four windows' worth of virtual time and the run is only quiet
// because each event pushed the window out; the one window that finally closes
// applies the whole burst once.
//
// Each write waits for its own event before the next, so the reset count is a
// floor on events actually seen — an unread event can be coalesced by the
// kernel, and a burst that reached the watcher pre-merged would prove nothing.
func TestWatchDebouncesABurst(t *testing.T) {
	window := newFakeWindow()
	w, path := startWatchWithWindow(t, validMinimal, window)

	const writes = 8
	for i := range writes {
		seen := window.resetCount()
		if err := os.WriteFile(path, []byte(changedPrompt), 0o644); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "write "+strconv.Itoa(i+1)+" to reach the watcher", func() bool {
			return window.resetCount() > seen
		})
		if window.advance(testDebounce / 2) {
			t.Fatalf("the settling window closed during the burst, after write %d; an event must push it out", i+1)
		}
	}
	if got := w.reloadCount(); got != 0 {
		t.Fatalf("%d reloads while the window was still open; an event must not apply on its own", got)
	}

	// Nothing more arrives, so the window closes — once.
	if !window.advance(testDebounce) {
		t.Fatal("the settling window never closed; the burst would never be applied")
	}
	waitFor(t, "the closed window to be applied", func() bool { return w.reloadCount() > 0 })

	if got := window.resetCount(); got < writes {
		t.Errorf("%d events for %d writes; the burst reached the watcher pre-coalesced, so one reload proves less than it looks", got, writes)
	}
	if got := w.reloadCount(); got != 1 {
		t.Errorf("%d reloads for one burst of %d writes; the debounce did not coalesce them", got, writes)
	}
	if got := w.Snapshot().Definition.Config.Limits.MaxTurns; got != 9 {
		t.Errorf("max_turns = %d, want 9 — the one reload must apply the burst's final content", got)
	}
}

// Startup with an invalid config refuses to start — there is no
// last-known-good to fall back to (SPEC §5.7).
func TestWatchRefusesAnInvalidStartingConfig(t *testing.T) {
	path := writeWorkflow(t, "---\n- not a map\n---\nbody\n")

	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: testDebounce, BuildRuntime: noRuntime})
	if err == nil {
		w.Close()
		t.Fatal("Watch started on an invalid config; startup must refuse (SPEC §5.7)")
	}
	if !errors.Is(err, ErrFrontMatterNotMap) {
		t.Errorf("error = %v, want ErrFrontMatterNotMap", err)
	}
}

// The §9.4 backstop: a reload that the watch missed is still caught before
// dispatch. Simulated by editing a file the watcher cannot see change —
// here, by never letting the event through at all.
func TestRevalidateCatchesAMissedEvent(t *testing.T) {
	path := writeWorkflow(t, validMinimal)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: time.Hour, BuildRuntime: noRuntime})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	// An hour-long debounce means the watch will not act within the test.
	atomicSave(t, path, changedPrompt)
	if got := w.Snapshot().Definition.Config.Limits.MaxTurns; got != DefaultMaxTurns {
		t.Fatalf("the watch acted despite the long debounce; max_turns = %d", got)
	}

	if snap := w.Revalidate(context.Background()); snap.Blocked != nil {
		t.Fatalf("Revalidate: %v", snap.Blocked)
	}
	if got := w.Snapshot().Definition.Config.Limits.MaxTurns; got != 9 {
		t.Errorf("max_turns = %d after Revalidate, want the missed edit picked up", got)
	}
}

// Revalidate is the preflight check, so it must report the block the same way
// DispatchBlocked does — and blocking must not disturb the last-known-good.
func TestRevalidateReportsAndClearsTheBlock(t *testing.T) {
	path := writeWorkflow(t, validMinimal)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: time.Hour, BuildRuntime: noRuntime})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	good := w.Snapshot().Definition

	atomicSave(t, path, "---\n- not a map\n---\nbody\n")
	if snap := w.Revalidate(context.Background()); !errors.Is(snap.Blocked, ErrFrontMatterNotMap) {
		t.Fatalf("Revalidate blocked = %v, want the load error", snap.Blocked)
	}
	if !errors.Is(w.Snapshot().Blocked, ErrFrontMatterNotMap) {
		t.Errorf("Snapshot().Blocked = %v, want it to agree with what Revalidate returned", w.Snapshot().Blocked)
	}
	if w.Snapshot().Definition != good {
		t.Error("Revalidate replaced the last-known-good with an invalid config")
	}

	atomicSave(t, path, changedPrompt)
	if snap := w.Revalidate(context.Background()); snap.Blocked != nil {
		t.Fatalf("Revalidate after the fix: %v", snap.Blocked)
	}
	if w.Snapshot().Blocked != nil {
		t.Error("the block outlived the fix")
	}
}

// A standing error is logged once, not once per tick: Revalidate runs every
// dispatch cycle, and repeating the same line would bury the transition that
// matters (SPEC §5.4 — the durable surface is the blocked state).
func TestRepeatedFailuresLogOncePerTransition(t *testing.T) {
	path := writeWorkflow(t, validMinimal)
	var logs lockedBuffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: time.Hour, Logger: logger, BuildRuntime: noRuntime})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	atomicSave(t, path, "---\n- not a map\n---\nbody\n")
	for range 5 {
		w.Revalidate(context.Background())
	}

	if got := strings.Count(logs.String(), "level=ERROR"); got != 1 {
		t.Errorf("logged %d errors for one standing failure, want 1:\n%s", got, logs.String())
	}
	if w.Snapshot().Blocked == nil {
		t.Error("the block lapsed while the file was still broken")
	}
}

// reloadRecorder captures what OnReload was told.
type reloadRecorder struct {
	mu       sync.Mutex
	calls    []AdapterChange
	prev     []*testRuntime
	failNext error
}

func (r *reloadRecorder) build(_ context.Context, def *WorkflowDefinition, prev *testRuntime, changed AdapterChange) (*testRuntime, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, changed)
	r.prev = append(r.prev, prev)
	if r.failNext != nil {
		// The builder owns disposal of anything it constructed before failing,
		// and returns nothing: a runtime handed back alongside an error must not
		// be publishable.
		return nil, r.failNext
	}
	serial := 1
	if prev != nil {
		serial = prev.serial + 1
	}
	return &testRuntime{builtFrom: def, changed: changed, serial: serial}, nil
}

// reloads is every call after the one Watch makes for itself. The startup build
// goes through the same seam now, so a test asking "what did this reload
// rebuild?" has to say which call it means.
func (r *reloadRecorder) reloads() []AdapterChange {
	got := r.seen()
	if len(got) == 0 {
		return nil
	}
	return got[1:]
}

// prevSeen is what each call was handed as the runtime in force.
func (r *reloadRecorder) prevSeen() []*testRuntime {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]*testRuntime(nil), r.prev...)
}

func (r *reloadRecorder) seen() []AdapterChange {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]AdapterChange(nil), r.calls...)
}

func (r *reloadRecorder) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failNext = err
}

// B03 acceptance (assembly decision 13): a reload that changes an adapter's
// provider block must reconstruct that adapter and re-check Ready before it
// is used — so the watcher has to say *which* adapter moved, and must not
// claim one moved when it did not.
func TestReloadReportsWhichAdapterConfigChanged(t *testing.T) {
	swapTrackerRepo := strings.Replace(changedPrompt, "acme/widgets", "acme/gadgets", 1)
	swapAgentKind := strings.Replace(changedPrompt, "claude-code", "codex-exec", 1)
	addRequiredLabel := strings.Replace(changedPrompt, `["ben"]`, `["ben", "ready"]`, 1)

	tests := []struct {
		name string
		to   string
		want AdapterChange
	}{
		// The prompt and the orchestrator's own knobs are nobody's provider
		// block; rebuilding an adapter for them would be churn.
		{"prompt only", strings.Replace(changedPrompt, "Reworded", "Reworded again", 1), AdapterChange{}},
		{"limits only", strings.Replace(changedPrompt, "max_turns: 9", "max_turns: 5", 1), AdapterChange{}},
		{"tracker provider block", swapTrackerRepo, AdapterChange{Tracker: true}},
		// Core-owned tracker fields count too: Structural spans both, so a
		// new block checked against stale core fields is the silent
		// hot-reload bug decision 13 names (SPEC §5.7).
		{"tracker required_labels", addRequiredLabel, AdapterChange{Tracker: true}},
		{"agent kind", swapAgentKind, AdapterChange{Agent: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &reloadRecorder{}
			path := writeWorkflow(t, changedPrompt)
			w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: testDebounce, BuildRuntime: rec.build})
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			defer w.Close()
			initial := w.Snapshot().Definition

			atomicSave(t, path, tt.to)
			waitFor(t, "the reload to be adopted", func() bool { return w.Snapshot().Definition != initial })

			if !tt.want.Any() {
				// Nothing an adapter is bound to moved, so the builder must not
				// have been asked to rebuild anything beyond startup's own build.
				if got := rec.reloads(); len(got) != 0 {
					t.Errorf("BuildRuntime called with %+v for a change no adapter is bound to", got)
				}
				return
			}
			waitFor(t, "the rebuild", func() bool { return len(rec.reloads()) > 0 })
			if got := rec.reloads()[0]; got != tt.want {
				t.Errorf("changed = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// An adapter that cannot be rebuilt under the new configuration is a failed
// reload: dispatch blocks and the last-known-good stands, exactly as a parse
// failure would (assembly decision 13).
func TestReloadBlocksWhenAdapterRebuildFails(t *testing.T) {
	rec := &reloadRecorder{}
	notReady := errors.New("tracker not ready: 401 from GitHub")

	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: testDebounce, BuildRuntime: rec.build})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	good := w.Snapshot()
	// Armed after startup: a builder that cannot answer at startup is a refusal
	// to start, which is a different test (SPEC §5.7).
	rec.fail(notReady)

	atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets", "acme/gadgets", 1))
	waitFor(t, "the rebuild failure to block dispatch", func() bool { return w.Snapshot().Blocked != nil })

	if !errors.Is(w.Snapshot().Blocked, notReady) {
		t.Errorf("blocked with %v, want the rebuild error", w.Snapshot().Blocked)
	}
	if got := w.Snapshot(); got.Definition != good.Definition || got.Runtime != good.Runtime {
		t.Error("a reload whose adapter could not be rebuilt was adopted anyway")
	}

	// Recovering the adapter adopts the reload without a restart.
	rec.fail(nil)
	if snap := w.Revalidate(context.Background()); snap.Blocked != nil {
		t.Fatalf("Revalidate after recovery: %v", snap.Blocked)
	}
	after := w.Snapshot()
	if got := after.Definition.Config.Tracker.Provider["repo"]; got != "acme/gadgets" {
		t.Errorf("repo = %v after recovery, want the new block adopted", got)
	}
	if !after.Runtime.pairedWith(after.Definition) {
		t.Error("recovery published a definition the live runtime was not built from")
	}
}

// Revalidate runs every dispatch cycle. If it treated every pass as a reload
// it would rebuild adapters on a tick timer.
func TestRevalidateDoesNotRebuildOnAnUnchangedFile(t *testing.T) {
	rec := &reloadRecorder{}
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: time.Hour, BuildRuntime: rec.build})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	before := w.Snapshot().Definition

	for range 5 {
		if snap := w.Revalidate(context.Background()); snap.Blocked != nil {
			t.Fatalf("Revalidate: %v", snap.Blocked)
		}
	}
	if got := rec.reloads(); len(got) != 0 {
		t.Errorf("BuildRuntime called %d extra times for an unchanged file: %+v", len(got), got)
	}
	if w.Snapshot().Definition != before {
		t.Error("an unchanged file replaced the definition; in-flight holders would see churn")
	}
}

// closed reports whether a wake channel has fired, without blocking.
func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// The configuration is pushed, not only polled — and the push is the revision
// advancing and the wake firing, which is what a reader carrying a revision can
// act on.
//
// §5.4 blocks new dispatches the moment validation fails, while a polling caller
// is a whole interval away from asking and the reads it began under the valid
// config are already out. Told at the transition, it can invalidate them. So a
// raised block has to advance the revision even though it adopts no new
// definition — it is the case with nothing else to signal it.
func TestARaisedBlockAdvancesTheRevisionAndWakesReaders(t *testing.T) {
	path := writeWorkflow(t, validMinimal)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: testDebounce, BuildRuntime: noRuntime,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	// Startup publishes revision 1 and signals nothing: the caller was handed
	// this state by Watch itself.
	first, wake := w.RuntimeSource().Load()
	if first.Revision != 1 {
		t.Fatalf("startup revision = %d, want 1 — zero is never published, so a zero snapshot stays detectable", first.Revision)
	}
	if closed(wake) {
		t.Error("startup fired the change wake; there is no reader yet and nothing has moved")
	}

	atomicSave(t, path, "tracker: [not, a, map]\n")
	waitFor(t, "the raised block", func() bool { return w.Snapshot().Blocked != nil })

	blocked, _ := w.RuntimeSource().Load()
	if blocked.Revision != first.Revision+1 {
		t.Errorf("revision = %d after a raised block, want %d — reads begun under the valid configuration would still believe they may dispatch",
			blocked.Revision, first.Revision+1)
	}
	if !closed(wake) {
		t.Error("the raised block did not fire the wake held by a reader from the previous revision")
	}
	if blocked.Definition != first.Definition || blocked.Runtime != first.Runtime {
		t.Error("a blocked reload replaced the last-known-good definition or runtime")
	}
}

// Revalidate runs every dispatch cycle. Advancing the revision per pass would
// make it a heartbeat rather than a transition — and a caller discards work
// whose revision no longer matches, so it would discard reads nothing had
// superseded, on every tick.
func TestAnUnchangedFileDoesNotAdvanceTheRevision(t *testing.T) {
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: noRuntime,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	before, wake := w.RuntimeSource().Load()
	for range 5 {
		if snap := w.Revalidate(context.Background()); snap.Revision != before.Revision {
			t.Fatalf("revision = %d on an unchanged file, want %d", snap.Revision, before.Revision)
		}
	}
	if closed(wake) {
		t.Error("an unchanged file fired the change wake; a ticker would spin at the speed of the poll")
	}
}

// The same fault reported twice is one state observed twice. Re-versioning it
// would discard reads for a fact that has not moved — but the fresher wording is
// still recorded, because this is the record of the configuration and it should
// say what is currently wrong.
func TestARepeatedFailureRefreshesTheWordingWithoutReversioning(t *testing.T) {
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: noRuntime,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	if err := os.WriteFile(path, []byte("---\n- not a map\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	firstFault := w.Revalidate(context.Background())
	if firstFault.Blocked == nil {
		t.Fatal("Revalidate accepted an invalid file")
	}

	// A different fault in the same file: still one broken state, so the
	// revision holds while the message moves.
	if err := os.WriteFile(path, []byte("just a prompt with no front matter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := w.Revalidate(context.Background())
	if second.Revision != firstFault.Revision {
		t.Errorf("revision = %d for a second fault in a file already broken, want %d held",
			second.Revision, firstFault.Revision)
	}
	if second.Blocked == nil || second.Blocked.Error() == firstFault.Blocked.Error() {
		t.Errorf("blocked = %v, want the fresher fault recorded", second.Blocked)
	}
}

// The transient half-written file: a load fails, blocking dispatch, and the next
// read finds the file back to what we already had. Nothing was adopted, so the
// lift is the only thing to signal — and it has to be signalled, or a caller
// stays blocked on a fault that is over.
func TestClearingATransientBlockAdvancesTheRevision(t *testing.T) {
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce: time.Hour, BuildRuntime: noRuntime,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	good, _ := w.RuntimeSource().Load()

	if err := os.WriteFile(path, []byte("tracker: [not, a, map]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if snap := w.Revalidate(context.Background()); snap.Blocked == nil {
		t.Fatal("Revalidate accepted an invalid file")
	}
	blocked, wake := w.RuntimeSource().Load()

	// Settled back to the file already in force: the transaction reports
	// "nothing moved", so the lift travels the clearBlock path rather than an
	// adoption.
	if err := os.WriteFile(path, []byte(changedPrompt), 0o644); err != nil {
		t.Fatal(err)
	}
	lifted := w.Revalidate(context.Background())
	if lifted.Blocked != nil {
		t.Fatalf("the block outlived the file returning to normal: %v", lifted.Blocked)
	}
	if lifted.Revision != blocked.Revision+1 {
		t.Errorf("revision = %d after the lift, want %d — a caller holding the blocked revision would never re-read",
			lifted.Revision, blocked.Revision+1)
	}
	if !closed(wake) {
		t.Error("the lifted block did not fire the wake")
	}
	if lifted.Definition != good.Definition || lifted.Runtime != good.Runtime {
		t.Error("clearing a transient block replaced the definition or runtime; nothing had moved")
	}
}

// Adopting an adapter-changing reload with nobody to rebuild the adapter
// would satisfy assembly decision 13 silently and wrongly, so the hook is
// required rather than defaulted.
func TestWatchRequiresTheRuntimeBuilder(t *testing.T) {
	path := writeWorkflow(t, validMinimal)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: testDebounce})
	if err == nil {
		w.Close()
		t.Fatal("Watch accepted a nil BuildRuntime; a definition would then be published with no adapters bound to it")
	}
	if !errors.Is(err, ErrNoRuntimeBuilder) {
		t.Errorf("error = %v, want ErrNoRuntimeBuilder", err)
	}
}

// SPEC §5.4: never crash. internal/template's load-time filter probe
// deliberately lets engine panics propagate, and hands B03 the job of
// containing them on the reload path — a panic there would take the daemon
// down with in-flight runs attached.
func TestReloadContainsAPanic(t *testing.T) {
	path := writeWorkflow(t, validMinimal)
	logs := &lockedBuffer{}
	started := false
	boom := func(_ context.Context, def *WorkflowDefinition, _ *testRuntime, _ AdapterChange) (*testRuntime, error) {
		// Startup must survive, so only a reload panics: the first call is the
		// one Watch itself makes.
		if started {
			panic("engine incompatibility")
		}
		started = true
		return &testRuntime{builtFrom: def}, nil
	}
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce:     time.Hour,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
		BuildRuntime: boom,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()
	good := w.Snapshot().Definition

	// A panic anywhere in the reload transaction is contained the same way.
	atomicSave(t, path, strings.Replace(validMinimal, "acme/widgets", "acme/gadgets", 1))
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("the panic escaped Revalidate and would have killed the daemon: %v", r)
			}
		}()
		if snap := w.Revalidate(context.Background()); snap.Blocked == nil {
			t.Error("a contained panic must still be a failed reload")
		}
	}()

	if !errors.Is(w.Snapshot().Blocked, ErrReloadPanic) {
		t.Errorf("blocked with %v, want ErrReloadPanic", w.Snapshot().Blocked)
	}
	if w.Snapshot().Definition != good {
		t.Error("a panicking reload disturbed the last-known-good")
	}
	if !strings.Contains(logs.String(), "stack") {
		t.Errorf("a contained panic logged no stack, leaving nothing to diagnose:\n%s", logs.String())
	}
}

// A reload is a transaction — read, rebuild, commit — and its steps are not
// atomic. Unserialized, a slow rebuild of an older version can commit after a
// newer one and install a definition the adapters no longer match.
func TestReloadsCommitInOrder(t *testing.T) {
	path := writeWorkflow(t, changedPrompt)

	var mu sync.Mutex
	var inFlight, maxInFlight int
	slowRebuild := func(_ context.Context, def *WorkflowDefinition, _ *testRuntime, changed AdapterChange) (*testRuntime, error) {
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()

		time.Sleep(20 * time.Millisecond) // a Ready() check hitting the network

		mu.Lock()
		inFlight--
		mu.Unlock()
		return &testRuntime{builtFrom: def, changed: changed}, nil
	}

	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: testDebounce, BuildRuntime: slowRebuild})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	// Drive the watch goroutine and Revalidate at each other while the
	// tracker config keeps moving.
	var wg sync.WaitGroup
	for i := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			atomicSave(t, path, strings.Replace(changedPrompt, "acme/widgets", "acme/repo"+strconv.Itoa(i), 1))
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.Revalidate(context.Background())
		}()
	}
	wg.Wait()

	// The writers are done, but the last save may have landed after the last
	// Revalidate read the file, leaving a debounce still pending — so the
	// question below has no answer yet. One synchronous pass settles it:
	// apply serializes on applyMu, so this cannot return until any
	// transaction already under way has committed, and any that starts
	// afterwards reads the same settled file. Waiting on the wall clock
	// instead is the race this test was accidentally asserting through (#39).
	if snap := w.Revalidate(context.Background()); snap.Blocked != nil {
		t.Fatalf("Revalidate: %v", snap.Blocked)
	}

	mu.Lock()
	peak := maxInFlight
	mu.Unlock()
	if peak > 1 {
		t.Errorf("%d reload transactions overlapped; a slow rebuild can commit over a newer one", peak)
	}

	// Whatever landed last must be what the file says — not an older version
	// that finished rebuilding later.
	final := mustLoadForTest(t, path)
	if got, want := w.Snapshot().Definition.Config.Tracker.Provider["repo"], final.Config.Tracker.Provider["repo"]; got != want {
		t.Errorf("committed repo = %v, want %v — a stale reload won the race", got, want)
	}
}

func mustLoadForTest(t *testing.T, path string) *WorkflowDefinition {
	t.Helper()
	def, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return def
}

// SPEC §5.8: provenance drives redaction. Replacing a literal secret with a
// $VAR that resolves to the same string changes nothing in the config and
// everything in where the value came from, so skipping that reload would keep
// `config effective` printing a secret the file no longer spells out.
func TestReloadAdoptsAProvenanceOnlyChange(t *testing.T) {
	const literal = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
    token: s3cret
  required_labels: ["ben"]
agent:
  kind: claude-code
deployment:
  mode: attended
---
Do {{ issue.title }}.
`
	t.Setenv("BEN_TEST_TOKEN", "s3cret")
	indirect := strings.Replace(literal, "token: s3cret", "token: $BEN_TEST_TOKEN", 1)

	path := writeWorkflow(t, literal)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: time.Hour, BuildRuntime: noRuntime})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close()

	if got := w.Snapshot().Definition.Provenance["tracker.provider.token"].Source; got != SourceFile {
		t.Fatalf("token provenance = %q before the edit, want %q", got, SourceFile)
	}

	// Same resolved value, different origin.
	atomicSave(t, path, indirect)
	if snap := w.Revalidate(context.Background()); snap.Blocked != nil {
		t.Fatalf("Revalidate: %v", snap.Blocked)
	}

	origin := w.Snapshot().Definition.Provenance["tracker.provider.token"]
	if origin.Source != SourceEnv {
		t.Errorf("token provenance = %q, want %q — the reload was discarded as a no-op and `config effective` would still print the secret",
			origin.Source, SourceEnv)
	}
	if !slices.Equal(origin.EnvVars, []string{"BEN_TEST_TOKEN"}) {
		t.Errorf("EnvVars = %v, want [BEN_TEST_TOKEN]", origin.EnvVars)
	}
}

// Close is idempotent, and cancelling the context stops the watcher too.
func TestWatchLifecycle(t *testing.T) {
	w, _, _ := startWatch(t, validMinimal)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w2, err := Watch(ctx, writeWorkflow(t, validMinimal), WatchOptions[*testRuntime]{Debounce: testDebounce, BuildRuntime: noRuntime})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	cancel()
	waitFor(t, "the cancelled watcher to stop", func() bool {
		select {
		case <-w2.done:
			return true
		default:
			return false
		}
	})
	if err := w2.Close(); err != nil {
		t.Fatalf("Close after cancel: %v", err)
	}
}

// Edits to neighbouring files in the watched directory are not our business.
func TestWatchIgnoresOtherFilesInTheDirectory(t *testing.T) {
	w, path, _ := startWatch(t, validMinimal)

	for _, name := range []string{"README.md", "WORKFLOW.md.bak", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(6 * testDebounce)

	if got := w.reloadCount(); got != 0 {
		t.Errorf("%d reloads triggered by unrelated files in the directory", got)
	}
}

// SPEC §5.2.9: the deployment declaration is process-lifetime. A reload that
// changes it is invalid, so it lands on §5.4's existing path — last-known-good
// kept, new dispatch blocked, restart required.
//
// Refused rather than adopted because it is not a fact about the workflow. It
// asserts how this process was launched and what the operator arranged around
// it; a running daemon cannot have been re-launched, and `attended` in
// particular asserts something about a human that editing a file does not make
// true.
func TestReloadRefusesToChangeTheDeploymentDeclaration(t *testing.T) {
	rec := &reloadRecorder{}
	path := writeWorkflow(t, changedPrompt)
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{Debounce: testDebounce, BuildRuntime: rec.build})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Close() //nolint:errcheck // Close is idempotent
	good := w.Snapshot()
	if good.Definition.Config.Deployment.Mode != DeploymentAttended {
		t.Fatalf("fixture declares %q; this test needs a known starting mode", good.Definition.Config.Deployment.Mode)
	}

	atomicSave(t, path, strings.Replace(changedPrompt,
		"deployment:\n  mode: attended",
		"deployment:\n  mode: risk-accepted\n  accepted_because: edited under a running daemon", 1))
	waitFor(t, "the declaration change to block dispatch", func() bool { return w.Snapshot().Blocked != nil })

	if !errors.Is(w.Snapshot().Blocked, ErrDeploymentChanged) {
		t.Errorf("blocked with %v, want ErrDeploymentChanged", w.Snapshot().Blocked)
	}
	if got := w.Snapshot(); got.Definition != good.Definition {
		t.Error("a changed deployment declaration was adopted")
	}
	// And the message names both ends, since an operator reading the journal has
	// to know which one is in force.
	if msg := w.Snapshot().Blocked.Error(); !strings.Contains(msg, "attended") || !strings.Contains(msg, "risk-accepted") {
		t.Errorf("refusal %q does not name both declarations", msg)
	}

	// Putting it back is not a second failure: the file now agrees with what is
	// running, which is §5.4's transient-failure case.
	atomicSave(t, path, changedPrompt)
	waitFor(t, "the block to lift once the file agrees again", func() bool { return w.Snapshot().Blocked == nil })
}
