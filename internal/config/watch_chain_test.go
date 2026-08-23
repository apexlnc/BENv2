package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"

	"github.com/fsnotify/fsnotify"
	"testing"
)

// releaseLayout builds the shape a release directory or a dotfiles checkout has,
// with the intermediate link **outside** the config file's own directory:
//
//	cfg/WORKFLOW.md -> releases/current/WORKFLOW.md
//	releases/current -> releases/v1
//
// Swapping `releases/current` produces no event in cfg/ at all, so a watcher
// holding only cfg/ never runs its resolution and reports nothing. This is the
// case the predicate tests could not establish: they call concerns directly and
// so assume an event arrives.
func releaseLayout(t *testing.T, bodies map[string]string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	cfg := filepath.Join(root, "cfg")
	releases := filepath.Join(root, "releases")
	if err := os.MkdirAll(cfg, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range bodies {
		d := filepath.Join(releases, name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("v1", filepath.Join(releases, "current")); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(cfg, "WORKFLOW.md")
	if err := os.Symlink(filepath.Join(releases, "current", "WORKFLOW.md"), path); err != nil {
		t.Fatal(err)
	}
	return root, path
}

// repoint moves a symlink the way `ln -sfn` does: unlink, then re-create.
//
// Deliberately not the atomic write-temp-and-rename form. That is what kubelet
// does, and it is covered by the projected-ConfigMap tests — where the generation
// directory kubelet also removes supplies a second event. On its own, renaming a
// symlink onto an existing name in a watched directory is not reliably reported
// by kqueue: measured on darwin, the same test passes at -count=1 and fails
// within ten runs. Both forms occur in the wild, this one is what a release
// script uses, and it is the one whose events do not depend on the platform.
func repoint(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

// Repointing an intermediate link that lives outside the config's own directory
// still reloads. Watching only filepath.Dir(path) receives no event here, so the
// resolution never runs and nothing fires.
//
// Linux only, and the reason is fsnotify's rather than ours. Measured on darwin:
// replacing a symlink inside a watched directory is not reliably reported by
// kqueue in either form — the atomic rename passes at -count=1 and fails within
// ten runs, and unlink-then-symlink fails outright. BEN's deployment platform and
// CI are both Linux, so this runs where it is meaningful and is skipped where it
// would only assert the platform.
//
// It is not the only cover for this path: TestSyncChainDropsDirectoriesThatLeaveTheChain
// proves the directory holding the intermediate link joins the watch set, and
// TestConcernsFollowsAnOrdinarySymlinkIndirection proves a resolution change
// through such a link is concerning. Both run everywhere. What this test adds,
// and what those cannot, is that the two meet — that an event really arrives.
func TestWatchReloadsWhenAnIntermediateLinkOutsideTheDirectoryMoves(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("kqueue does not reliably report a symlink replaced inside a watched directory; see the comment above (GOOS=%s)", runtime.GOOS)
	}
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal, "v2": changedPrompt})
	w := startWatchAt(t, path)

	repoint(t, filepath.Join(root, "releases", "current"), "v2")

	waitFor(t, "the intermediate link swap to be picked up", func() bool {
		return w.Snapshot().Definition.Config.Limits.MaxTurns == 9
	})
}

// The real file edited in place under a link moves no path at all: the event
// names the target, not us, and the resolution is unchanged.
func TestWatchReloadsWhenTheResolvedTargetIsEditedInPlace(t *testing.T) {
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal})
	w := startWatchAt(t, path)

	target := filepath.Join(root, "releases", "v1", "WORKFLOW.md")
	if err := os.WriteFile(target, []byte(changedPrompt), 0o644); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "an in-place edit of the resolved target to be picked up", func() bool {
		return w.Snapshot().Definition.Config.Limits.MaxTurns == 9
	})
}

// Regression for the stale baseline: an event that matches by *name* must still
// refresh the resolved target.
//
// Retarget the config link directly — seen by name — and then move the file back
// to where it originally resolved. A baseline left at the original value compares
// equal to that rollback and ignores it, so the daemon keeps serving a definition
// the file no longer holds.
//
// Driven through concerns rather than through the event stream: the property is
// what the predicate does with the baseline, and asserting it through two
// consecutive link swaps would instead be asserting how quickly the platform
// re-registers a replaced directory entry. Measured on darwin/kqueue, that takes
// long enough that a second swap issued immediately after the first reload is
// missed — a fixture racing fsnotify, with nothing to say about this code.
func TestConcernsRefreshesTheBaselineOnANameMatch(t *testing.T) {
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal, "v2": changedPrompt})
	releases := filepath.Join(root, "releases")
	w := probe(t, path)
	original := w.resolved

	// Direct retarget, bypassing `current`. This is the event that names us.
	repoint(t, path, filepath.Join(releases, "v2", "WORKFLOW.md"))
	if !w.concerns(ev(path)) {
		t.Fatal("retargeting the config link was not seen by name")
	}
	if w.resolved == original {
		t.Fatal("the name match left the baseline at the previously resolved target")
	}

	// Back to where it started. Only a refreshed baseline can tell this apart
	// from no change at all.
	repoint(t, path, filepath.Join(releases, "current", "WORKFLOW.md"))
	if !w.concerns(ev(filepath.Join(releases, "current"))) {
		t.Error("the rollback to the originally resolved target was ignored: the baseline was never refreshed on the name match")
	}
	if w.resolved != original {
		t.Errorf("resolved = %q after the rollback, want %q", w.resolved, original)
	}
}

// chainProbe is a watcher with a real fsnotify watcher and **no goroutine**, so
// a test can drive syncChain itself. Starting one and then calling syncChain
// from the test would race the watch loop for the very fields the production
// code documents as single-goroutine.
func chainProbe(t *testing.T, path string) *Watcher[*testRuntime] {
	t.Helper()
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fsw.Close() })

	w := &Watcher[*testRuntime]{
		path: filepath.Clean(path),
		log:  slog.New(slog.DiscardHandler),
		fsw:  fsw,
	}
	w.resolved, _ = filepath.EvalSymlinks(w.path)
	if err := fsw.Add(filepath.Dir(w.path)); err != nil {
		t.Fatal(err)
	}
	w.syncChain()
	return w
}

// The watch set follows the chain rather than growing with it. syncChain is
// called directly, for the same reason as above: what is under test is the set
// it converges on, not how fast an event reaches it.
func TestSyncChainDropsDirectoriesThatLeaveTheChain(t *testing.T) {
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal})
	releases := filepath.Join(root, "releases")
	w := chainProbe(t, path)
	start := len(w.fsw.WatchList())

	for i := range 20 {
		d := filepath.Join(releases, "gen", string(rune('a'+i)))
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "WORKFLOW.md"), []byte(validMinimal), 0o644); err != nil {
			t.Fatal(err)
		}
		repoint(t, path, filepath.Join(d, "WORKFLOW.md"))
		w.syncChain()

		want, err := filepath.EvalSymlinks(d)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(w.fsw.WatchList(), func(got string) bool {
			r, err := filepath.EvalSymlinks(got)
			return err == nil && r == want
		}) {
			t.Fatalf("swap %d: %s did not join the watch set %v", i, d, w.fsw.WatchList())
		}
	}

	if got := len(w.fsw.WatchList()); got > start+1 {
		t.Errorf("watch list grew to %d from %d across 20 swaps: directories that left the chain are not being dropped", got, start)
	}
}

// walkChain collects our directory, each link's, and the final target's — and
// terminates on a loop rather than walking forever.
func TestWalkChainWalksLinksAndSurvivesALoop(t *testing.T) {
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal})

	// Compared by where each directory *is*: on macOS a temp path runs through
	// /var -> /private/var, so the walk legitimately reports resolved spellings
	// for everything it reached by following a link. Comparing strings would
	// assert the platform, not the behaviour.
	resolve := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatalf("EvalSymlinks(%s): %v", p, err)
		}
		return r
	}
	dirs, _, _ := walkChain(path)
	got := make([]string, 0, len(dirs))
	for _, d := range dirs {
		got = append(got, resolve(d))
	}

	for _, want := range []string{
		filepath.Join(root, "cfg"),
		filepath.Join(root, "releases"),
		filepath.Join(root, "releases", "v1"),
	} {
		if !slices.Contains(got, resolve(want)) {
			t.Errorf("walkChain(%s) = %v, missing %s", path, got, want)
		}
	}
	if slices.Contains(got, string(filepath.Separator)) {
		t.Errorf("walkChain returned the filesystem root: %v", got)
	}
	// Deliberately no assertion that a directory appears once by *resolved*
	// identity. #158's follow-up abandoned that invariant: the physical target must
	// be watched by its physical name even when the config's own spelling reaches it
	// through a link, so where the two differ both are entries. Spelling uniqueness
	// is asserted by TestWalkChainDoesNotRepeatASpelling.

	loopDir := t.TempDir()
	a, b := filepath.Join(loopDir, "a"), filepath.Join(loopDir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := walkChain(a); len(got) == 0 {
		t.Error("walkChain returned nothing for a looping chain; it must still yield the starting directory")
	}
}
