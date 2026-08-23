package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// linkedAncestorLayout puts the config file under a directory that is *not* a link
// but is reached through one:
//
//	root/current -> root/v1
//	root/v1/config/WORKFLOW.md
//	root/v2/config/WORKFLOW.md
//
// The config path is `root/current/config/WORKFLOW.md`, so filepath.Dir(path) is
// `root/current/config`. Lstat reports that as an ordinary directory — Lstat
// follows every component but the last — so nothing about it looks special, while
// the watch on it is pinned to `v1/config` for the life of the process.
func linkedAncestorLayout(t *testing.T, bodies map[string]string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	for name, body := range bodies {
		d := filepath.Join(root, name, "config")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "WORKFLOW.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "v1"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, "current", "config", "WORKFLOW.md")
}

// The physical directory the file lives in must be watched by its physical name,
// even when the config's own directory is an ordinary directory reached through a
// link.
//
// Deduplicating by resolved path made `v1/config` look already-covered by
// `current/config`, and after the swap made `v2/config` look already-covered too —
// so nothing physical was ever watched and the base watch stayed pinned to
// `v1/config`.
func TestSyncChainWatchesThePhysicalTargetUnderASymlinkedAncestor(t *testing.T) {
	root, path := linkedAncestorLayout(t, map[string]string{"v1": validMinimal, "v2": changedPrompt})
	w := chainProbe(t, path)

	// Compared literally against the physical spelling. Resolving the recorded
	// spelling at assertion time is what hid this: `current/config` resolves to
	// whichever generation the link points at *now*, so a stale watch reads as
	// coverage of the new one.
	holds := func(dir string) bool {
		want, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(w.chain, want) || slices.Contains(w.chain, dir)
	}

	if !holds(filepath.Join(root, "v1", "config")) {
		t.Fatalf("v1/config is not watched by its physical name at startup: chain=%v", w.chain)
	}

	repoint(t, filepath.Join(root, "current"), filepath.Join(root, "v2"))
	w.syncChain()

	if !holds(filepath.Join(root, "v2", "config")) {
		t.Errorf("v2/config did not join the watch set after the swap: chain=%v — the base watch is still pinned to v1/config, so edits under v2/config would be missed", w.chain)
	}
}

// The same property for the plain case: an ordinary path must not accumulate a
// second entry for its own directory, whatever its ancestors look like.
func TestWalkChainDoesNotRepeatASpelling(t *testing.T) {
	path := writeWorkflow(t, validMinimal)
	dirs, _, _ := walkChain(path)

	if len(dirs) != len(slices.Compact(slices.Sorted(slices.Values(dirs)))) {
		t.Errorf("walkChain repeated a spelling for an ordinary path: %v", dirs)
	}
	if !slices.Contains(dirs, filepath.Dir(filepath.Clean(path))) {
		t.Errorf("walkChain(%s) = %v, missing the config's own directory", path, dirs)
	}
}
