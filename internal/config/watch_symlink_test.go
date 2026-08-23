package config

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// projectConfigMap builds kubelet's atomic-writer layout for a projected volume
// and returns the mount directory and the user-visible path inside it.
//
//	mount/..<ts>/WORKFLOW.md     the payload
//	mount/..data -> ..<ts>       the generation pointer
//	mount/WORKFLOW.md -> ..data/WORKFLOW.md
//
// Reproduced rather than approximated with a plain symlink, because the whole
// defect lives in which entry the update touches: a plain symlink test would
// pass against the pre-#158 code.
func projectConfigMap(t *testing.T, content string) (mount, path string) {
	t.Helper()
	mount = t.TempDir()
	gen := filepath.Join(mount, "..2026_08_18_00_00_00.111")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen, "WORKFLOW.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Base(gen), filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(mount, "WORKFLOW.md")
	if err := os.Symlink(filepath.Join("..data", "WORKFLOW.md"), path); err != nil {
		t.Fatal(err)
	}
	return mount, path
}

// swapConfigMap performs kubelet's update: a new generation directory, a
// `..data_tmp` symlink, an atomic rename onto `..data`, and **the obsolete
// generation removed**. The user-visible WORKFLOW.md symlink is deliberately not
// touched — that is exactly what kubelet does, and what the name match cannot
// see.
//
// The removal is not decoration. Measured on darwin/kqueue, renaming a symlink
// onto an existing name in a watched directory surfaces no event at all, and the
// detection then has nothing to hang off; the removal is what produces one.
// Linux/inotify reports the rename and does not need it. Since kubelet performs
// both in a single pass, omitting the removal here would make the fixture pass
// on the deployment platform and hang on the development one — for a reason that
// is the fixture's, not the code's.
func swapConfigMap(t *testing.T, mount, ts, content string) {
	t.Helper()
	gen := filepath.Join(mount, ".."+ts)
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen, "WORKFLOW.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(mount, "..data_tmp")
	if err := os.Symlink(filepath.Base(gen), tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(mount)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "..") && e.Name() != filepath.Base(gen) {
			if err := os.RemoveAll(filepath.Join(mount, e.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func startWatchAt(t *testing.T, path string) *Watcher[*testRuntime] {
	t.Helper()
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce:     testDebounce,
		BuildRuntime: noRuntime,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	return w
}

// A ConfigMap-mounted workflow reloads when the projection is replaced.
//
// Before #158 this could not fire on Linux: kubelet swaps the `..data` symlink
// and leaves the entry named WORKFLOW.md alone, so no event carries the watched
// name. The read-back is part of the assertion — it separates "the watcher
// missed a change" from "nothing changed".
func TestWatchReloadsWhenAProjectedConfigMapIsReplaced(t *testing.T) {
	mount, path := projectConfigMap(t, validMinimal)
	w := startWatchAt(t, path)

	if got := w.Snapshot().Definition.Config.Limits.MaxTurns; got != DefaultMaxTurns {
		t.Fatalf("max_turns = %d before the swap, want the default %d", got, DefaultMaxTurns)
	}

	swapConfigMap(t, mount, "2026_08_18_00_05_00.222", changedPrompt)

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != changedPrompt {
		t.Fatalf("the projection did not actually change through the symlink; the test proves nothing")
	}

	waitFor(t, "the projected ConfigMap swap to be picked up", func() bool {
		return w.Snapshot().Definition.Config.Limits.MaxTurns == 9
	})
	if err := w.Snapshot().Blocked; err != nil {
		t.Errorf("a valid reload must not block dispatch: %v", err)
	}
}

// A projection is replaced as often as its ConfigMap is edited, so the resolved
// path has to be stored on each detection rather than compared against the one
// captured at Watch time. Comparing against the original passes the first swap
// and silently ignores every one after it.
func TestWatchReloadsOnEverySuccessiveProjectionSwap(t *testing.T) {
	mount, path := projectConfigMap(t, validMinimal)
	w := startWatchAt(t, path)

	swapConfigMap(t, mount, "2026_08_18_00_05_00.222", changedPrompt)
	waitFor(t, "the first swap", func() bool {
		return w.Snapshot().Definition.Config.Limits.MaxTurns == 9
	})

	swapConfigMap(t, mount, "2026_08_18_00_10_00.333", validMinimal)
	waitFor(t, "the second swap, which a one-shot resolution would miss", func() bool {
		return w.Snapshot().Definition.Config.Limits.MaxTurns == DefaultMaxTurns
	})
}

// The second clause must not turn every neighbour into a reload. BEN's own
// WORKFLOW.md sits at a repository root, so "any event in the directory
// reloads" would re-read and rebuild on every source edit.
func TestWatchIgnoresOtherFilesBesideAProjectedConfigMap(t *testing.T) {
	mount, path := projectConfigMap(t, validMinimal)
	w := startWatchAt(t, path)

	for _, name := range []string{"README.md", "WORKFLOW.md.bak", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(mount, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(6 * testDebounce)

	if got := w.reloadCount(); got != 0 {
		t.Errorf("%d reloads triggered by unrelated files beside a projected config", got)
	}
}

// The resolved path is reported once at startup, so an operator can see what is
// actually being watched instead of inferring it from a reload that never
// arrives.
func TestWatchReportsASymlinkedPathAtStartup(t *testing.T) {
	_, path := projectConfigMap(t, validMinimal)

	logs := &lockedBuffer{}
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce:     testDebounce,
		Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		BuildRuntime: noRuntime,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	if got := logs.String(); !strings.Contains(got, "path is a symlink") {
		t.Errorf("startup did not report the symlinked path: %q", got)
	}
}

// For a path that is not itself a symlink, the resolution is stable, so the
// second clause never fires and the ordinary cases are untouched.
//
// Compared against EvalSymlinks rather than against the path: on macOS a
// t.TempDir() sits under /var, which is itself a symlink to /private/var, so an
// entirely ordinary file resolves to a different string. That is exactly why the
// startup notice keys on Lstat of the path — see TestWatchDoesNotReportAnOrdinaryPath.
func TestAnOrdinaryPathResolvesStably(t *testing.T) {
	w, path, _ := startWatch(t, validMinimal)

	want, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if w.resolved != want {
		t.Errorf("resolved = %q, want %q", w.resolved, want)
	}
}

// The startup notice is about the config path being a symlink, not about an
// ancestor being one. Without that distinction every macOS run announces a
// symlinked config, which trains an operator to ignore the one line that matters.
func TestWatchDoesNotReportAnOrdinaryPath(t *testing.T) {
	path := writeWorkflow(t, validMinimal)

	logs := &lockedBuffer{}
	w, err := Watch(context.Background(), path, WatchOptions[*testRuntime]{
		Debounce:     testDebounce,
		Logger:       slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		BuildRuntime: noRuntime,
	})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	if got := logs.String(); strings.Contains(got, "path is a symlink") {
		t.Errorf("an ordinary path was reported as symlinked: %q", got)
	}
}
