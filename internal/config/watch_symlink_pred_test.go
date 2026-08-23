package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

// The reload tests beside this file prove the whole pipeline fires, but they
// cannot see a *spurious* concern: apply() returns early when nothing moved, so
// an over-eager predicate costs a re-parse that reloadCount never records. These
// exercise the predicate directly, which is where the cost and the state live.
//
// A bare struct rather than a started watcher: concerns reads only path and
// resolved, and calling it on a running watcher would race that watcher's own
// goroutine for the field it updates.
func probe(t *testing.T, path string) *Watcher[*testRuntime] {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return &Watcher[*testRuntime]{path: filepath.Clean(path), resolved: resolved}
}

func ev(name string) fsnotify.Event {
	return fsnotify.Event{Name: name, Op: fsnotify.Create}
}

// An unrelated sibling must not concern the watcher — before or after a swap.
//
// The "after" half is the one that matters: a predicate that compares against
// the resolution captured at Watch time and never stores the new one keeps
// reporting a difference forever, so every subsequent event in the directory
// re-reads and re-parses the file.
func TestConcernsIgnoresSiblingsBeforeAndAfterASwap(t *testing.T) {
	mount, path := projectConfigMap(t, validMinimal)
	w := probe(t, path)

	if w.concerns(ev(filepath.Join(mount, "NOTES.md"))) {
		t.Fatal("a sibling concerned the watcher before any swap")
	}

	swapConfigMap(t, mount, "2026_08_18_00_05_00.222", changedPrompt)
	if !w.concerns(ev(filepath.Join(mount, "..data"))) {
		t.Fatal("the swap did not concern the watcher")
	}

	if w.concerns(ev(filepath.Join(mount, "NOTES.md"))) {
		t.Error("a sibling concerned the watcher after the swap: the resolution was compared but not stored, so every later event now re-reads the file")
	}
}

// One swap concerns the watcher exactly once. A second call with nothing changed
// in between must be false, which is the same property from the other side.
func TestConcernsReportsASwapOnceOnly(t *testing.T) {
	mount, path := projectConfigMap(t, validMinimal)
	w := probe(t, path)

	swapConfigMap(t, mount, "2026_08_18_00_05_00.222", changedPrompt)
	if !w.concerns(ev(filepath.Join(mount, "..data"))) {
		t.Fatal("the swap did not concern the watcher")
	}
	if w.concerns(ev(filepath.Join(mount, "..data"))) {
		t.Error("the same swap concerned the watcher twice")
	}
}

// The indirection is not Kubernetes-shaped and the fix must not be either. Here
// the intermediate link is named `current`, as a release-directory layout or a
// dotfiles checkout would name it. A predicate keyed on the literal `..data`
// passes every ConfigMap test and fails this one.
func TestConcernsFollowsAnOrdinarySymlinkIndirection(t *testing.T) {
	mount := t.TempDir()
	for _, v := range []struct{ dir, body string }{{"v1", validMinimal}, {"v2", changedPrompt}} {
		d := filepath.Join(mount, v.dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "WORKFLOW.md"), []byte(v.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("v1", filepath.Join(mount, "current")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(mount, "WORKFLOW.md")
	if err := os.Symlink(filepath.Join("current", "WORKFLOW.md"), path); err != nil {
		t.Fatal(err)
	}

	w := probe(t, path)

	tmp := filepath.Join(mount, "current.tmp")
	if err := os.Symlink("v2", tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, filepath.Join(mount, "current")); err != nil {
		t.Fatal(err)
	}

	if !w.concerns(ev(filepath.Join(mount, "current"))) {
		t.Error("repointing an ordinary intermediate symlink did not concern the watcher")
	}
}

// A path that cannot be resolved is no information, not a change. Clobbering the
// stored resolution with the empty result reports a change that did not happen,
// and then loses the ability to detect the real one.
func TestConcernsKeepsTheLastResolutionWhenThePathCannotBeResolved(t *testing.T) {
	mount, path := projectConfigMap(t, validMinimal)
	w := probe(t, path)
	before := w.resolved

	// Break the chain: the user-visible link now points through a missing
	// generation pointer, which is what a half-applied update looks like.
	if err := os.Remove(filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}

	if w.concerns(ev(filepath.Join(mount, "..data"))) {
		t.Error("an unresolvable path was reported as a change")
	}
	if w.resolved != before {
		t.Errorf("resolved = %q after a failed resolution, want it left at %q", w.resolved, before)
	}

	// And the real change that follows is still detected.
	if err := os.Symlink("..2026_08_18_00_05_00.222", filepath.Join(mount, "..data")); err != nil {
		t.Fatal(err)
	}
	gen := filepath.Join(mount, "..2026_08_18_00_05_00.222")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gen, "WORKFLOW.md"), []byte(changedPrompt), 0o644); err != nil {
		t.Fatal(err)
	}
	if !w.concerns(ev(filepath.Join(mount, "..data"))) {
		t.Error("the change after a failed resolution was missed")
	}
}

// An event naming a link on our chain concerns the watcher even though the path
// no longer resolves to anything.
//
// This is the only clause that sees a removed intermediate link: resolution fails,
// so `moved` stays false by design — the baseline must survive a transient
// failure — and the event names neither the config path nor its last known target.
// Without it the daemon keeps serving a configuration whose file is gone, where
// §5.4 requires the failed reload to block dispatch.
func TestConcernsRecognisesAKnownChainLink(t *testing.T) {
	root, path := releaseLayout(t, map[string]string{"v1": validMinimal})
	current := filepath.Join(root, "releases", "current")

	w := probe(t, path)
	_, links, _ := walkChain(path)
	w.links = links
	baseline := w.resolved

	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	// The event carries the spelling the watch reached the directory by, which is
	// the one the walk recorded.
	var named string
	for _, l := range links {
		if filepath.Base(l) == "current" {
			named = l
		}
	}
	if named == "" {
		t.Fatalf("walkChain did not record the intermediate link: %v", links)
	}

	if !w.concerns(ev(named)) {
		t.Error("removing an intermediate link did not concern the watcher")
	}
	if w.resolved != baseline {
		t.Errorf("resolved = %q after an unresolvable removal, want the baseline %q left standing", w.resolved, baseline)
	}
}

// The name match is untouched: an event on the path itself still concerns the
// watcher without consulting the resolution at all.
func TestConcernsStillMatchesTheNameDirectly(t *testing.T) {
	path := writeWorkflow(t, validMinimal)
	w := probe(t, path)

	if !w.concerns(ev(path)) {
		t.Error("an event naming the config file did not concern the watcher")
	}
}
