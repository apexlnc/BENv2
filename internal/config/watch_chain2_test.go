package config

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
)

// linkedDirLayout puts the config file inside a directory that is *itself* the
// swapped link — the config path is `current/WORKFLOW.md`:
//
//	root/current -> root/v1
//	root/v1/WORKFLOW.md
//
// filepath.Dir(path) is therefore `root/current`, and fsnotify pins that watch to
// whatever it resolved to when it was added.
func linkedDirLayout(t *testing.T, bodies map[string]string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	for name, body := range bodies {
		d := filepath.Join(root, name)
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
	return root, filepath.Join(root, "current", "WORKFLOW.md")
}

// The new generation's directory must join the watch set even when the config's
// own directory resolves to the old one.
//
// Deduplicating the final target's directory against filepath.Dir(path) looks
// right — they are the same directory — but the *watch* on the config's own
// directory is bound to the inode it resolved to at Add time. Once `current`
// moves to v2, that watch still reports v1, and nothing at all watches v2.
func TestSyncChainWatchesTheNewTargetWhenTheConfigDirectoryIsItselfALink(t *testing.T) {
	root, path := linkedDirLayout(t, map[string]string{"v1": validMinimal, "v2": changedPrompt})
	w := chainProbe(t, path)

	// Asserted on w.chain — what is watched *beyond* the base watch — and not on
	// fsw.WatchList(). The watch list holds the string `.../current`, and resolving
	// that at assertion time yields whichever generation the link points at now,
	// so a stale watch pinned to v1 reads as coverage of v2. That is precisely the
	// defect, and an assertion that resolves the recorded spelling cannot see it.
	holds := func(dir string) bool {
		want, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(w.chain, func(got string) bool {
			r, err := filepath.EvalSymlinks(got)
			return err == nil && r == want
		})
	}

	if !holds(filepath.Join(root, "v1")) {
		t.Fatalf("v1 is not watched at startup: chain=%v", w.chain)
	}

	repoint(t, filepath.Join(root, "current"), filepath.Join(root, "v2"))
	w.syncChain()

	if !holds(filepath.Join(root, "v2")) {
		t.Errorf("v2 did not join the watch set after the swap: chain=%v — the base watch is still pinned to v1, so an in-place edit under v2 would be missed", w.chain)
	}
}

// Removing an intermediate link is a change to the configuration, and §5.4
// requires a reload that cannot read its file to block dispatch rather than to
// pass unnoticed.
//
// The link's removal resolves to nothing, so `moved` is false by design — the
// baseline must survive a transient failure — and the event names neither the
// config path nor its target. Recognising the link itself is the only thing that
// sees it.
//
// Linux only, for the reason given on
// TestWatchReloadsWhenAnIntermediateLinkOutsideTheDirectoryMoves: kqueue does not
// reliably report a symlink appearing or disappearing inside a watched directory.
// The two halves run everywhere — TestConcernsRecognisesAKnownChainLink for the
// predicate, TestSyncChainKeepsWatchesWhileTheChainIsBroken for the watch set —
// and this asserts that they meet.
func TestWatchBlocksWhenAnIntermediateLinkIsRemoved(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("kqueue does not reliably report a symlink removed inside a watched directory (GOOS=%s)", runtime.GOOS)
	}
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal})
	current := filepath.Join(root, "releases", "current")
	w := startWatchAt(t, path)

	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the removed intermediate link to block dispatch", func() bool {
		return w.Snapshot().Blocked != nil
	})

	// And restoring it clears the block without a restart. This is what a walk
	// that pruned on a failed resolution would break: the directory holding the
	// missing link would have left the watch set, so its return would be
	// invisible and the block permanent.
	if err := os.Symlink("v1", current); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "restoring the link to clear the block", func() bool {
		return w.Snapshot().Blocked == nil
	})
}

// A failed walk reports what it could not see, and syncChain must not read that
// as a shorter chain.
func TestWalkChainReportsAnUnresolvedChain(t *testing.T) {
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal})

	if _, _, resolved := walkChain(path); !resolved {
		t.Error("a fully resolvable chain reported unresolved")
	}

	if err := os.Remove(filepath.Join(root, "releases", "current")); err != nil {
		t.Fatal(err)
	}
	dirs, _, resolved := walkChain(path)
	if resolved {
		t.Error("a broken chain reported resolved")
	}
	if len(dirs) == 0 {
		t.Error("a broken chain reported no directories at all")
	}
}

// The directory holding a removed link stays watched, so its return is seen.
func TestSyncChainKeepsWatchesWhileTheChainIsBroken(t *testing.T) {
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal})
	releases := filepath.Join(root, "releases")
	w := chainProbe(t, path)

	// Compared resolved: the walk records the spelling it reached a directory by,
	// which on macOS is /private/var/... for anything found by following a link.
	holdsReleases := func() bool {
		want, err := filepath.EvalSymlinks(releases)
		if err != nil {
			t.Fatal(err)
		}
		return slices.ContainsFunc(w.chain, func(got string) bool {
			r, err := filepath.EvalSymlinks(got)
			return err == nil && r == want
		})
	}
	if !holdsReleases() {
		t.Fatalf("the directory holding the link is not watched to begin with: %v", w.chain)
	}

	if err := os.Remove(filepath.Join(releases, "current")); err != nil {
		t.Fatal(err)
	}
	w.syncChain()

	if !holdsReleases() {
		t.Errorf("chain = %v after the link was removed, want %s retained — otherwise restoring the link is invisible and the block is permanent", w.chain, releases)
	}
	if !slices.ContainsFunc(w.links, func(l string) bool { return filepath.Base(l) == "current" }) {
		t.Errorf("links = %v, want the removed link retained so its return is recognised", w.links)
	}
}
