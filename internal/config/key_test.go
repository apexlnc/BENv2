package config

import (
	"os"
	"path/filepath"
	"testing"
)

// KeyFor is what names the state directory of SPEC §10.3, and `ben status` has
// to find that directory for a daemon whose WORKFLOW.md is currently broken —
// which is the state an operator is most likely to be inspecting. A key
// obtainable only through a successful Load would make the status surface
// unavailable exactly when it is wanted.
func TestKeyForNeedsNeitherAReadableNorAValidFile(t *testing.T) {
	dir := t.TempDir()

	missing := filepath.Join(dir, "WORKFLOW.md")
	key, err := KeyFor(missing)
	if err != nil {
		t.Fatalf("KeyFor on a path with no file: %v", err)
	}
	if key == "" {
		t.Fatal("KeyFor returned an empty key")
	}

	// The same path, now holding a file Load refuses: the key must not move,
	// because the daemon that wrote the state directory computed it from the
	// path and nothing else.
	if err := os.WriteFile(missing, []byte("not a workflow"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(missing); err == nil {
		t.Fatal("Load accepted a file with no front matter")
	}
	again, err := KeyFor(missing)
	if err != nil {
		t.Fatalf("KeyFor on an invalid file: %v", err)
	}
	if again != key {
		t.Errorf("KeyFor moved when the file's contents changed: %q then %q", key, again)
	}
}

// Load routes through KeyFor rather than deriving a key of its own. Two
// derivations is a daemon writing to one state directory while `ben status`
// reads another, and nothing would report the disagreement.
func TestLoadsKeyIsKeyFors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(validMinimal), 0o644); err != nil {
		t.Fatal(err)
	}
	def, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want, err := KeyFor(path)
	if err != nil {
		t.Fatalf("KeyFor: %v", err)
	}
	if def.Key != want {
		t.Errorf("Load key = %q, KeyFor = %q", def.Key, want)
	}
}

// A relative path names the same workflow as the absolute one it resolves to:
// the key is the state directory's name, and two names for one workflow would
// be two state directories.
func TestKeyForIsPathAbsolute(t *testing.T) {
	dir := t.TempDir()
	abs := filepath.Join(dir, "WORKFLOW.md")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(wd) })

	fromAbs, err := KeyFor(abs)
	if err != nil {
		t.Fatal(err)
	}
	fromRel, err := KeyFor("WORKFLOW.md")
	if err != nil {
		t.Fatal(err)
	}
	if fromAbs != fromRel {
		t.Errorf("KeyFor(%q) = %q but KeyFor(\"WORKFLOW.md\") = %q", abs, fromAbs, fromRel)
	}
}
