package arch

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Two naming invariants #159 leaves behind, enforced here for the reason
// gomod_test.go gives for the version floor: the artifact arrives silently and no
// review round can report it.
//
// #159 deleted internal/orchestrator/regression_test.go — 53 tests across nine
// subjects. Nothing about that file arrived in one commit. A pile accumulates one
// test at a time, every individual addition reasonable, and there is no point at
// which a diff shows the file crossing from topic to landfill; the name is the
// only early signal, and a name is the one thing a reviewer reads past.

// Keep the durable rule no broader than that concrete promise. Filename
// heuristics cannot distinguish a landfill from a real subject: temp_dir_test.go
// and other_assignee_test.go, for example, are useful topic names.
const deletedLandfillTestFile = "internal/orchestrator/regression_test.go"

func TestDeletedLandfillTestFileStaysGone(t *testing.T) {
	found, err := landfillTestFiles(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range found {
		t.Error(v)
	}
}

// landfillTestFiles walks one module and reports the exact test landfill #159
// deleted. The root is a parameter and the findings are returned rather than
// reported, for the reason `violations` does the same: a walk that only ever runs
// over this repo and can only say "pass" pins nothing. See
// TestDeletedLandfillCheckIsARealTrigger.
func landfillTestFiles(root string) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			if ignoredPackageDir(d.Name()) {
				return filepath.SkipDir
			}
			if isModuleRoot(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		if filepath.ToSlash(rel) != deletedLandfillTestFile {
			return nil
		}
		found = append(found, fmt.Sprintf(
			"%s: #159 deleted this cross-topic landfill; give each test a subject file instead",
			deletedLandfillTestFile))
		return nil
	})
	return found, err
}

// One synthetic tree carrying both answers: the walk must flag the exact deleted
// landfill and leave legitimate topic names alone. Without the second half a
// filename heuristic could make the repository reject subjects such as temporary
// directories or another assignee; without the first the check could not fail.
func TestDeletedLandfillCheckIsARealTrigger(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		deletedLandfillTestFile,
		"internal/workspace/temp_dir_test.go",
		"internal/tracker/github/other_assignee_test.go",
		"internal/orchestrator/claim_test.go",
	} {
		writeFile(t, filepath.Join(root, filepath.FromSlash(name)), "package x\n")
	}

	found, err := landfillTestFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("reported %d findings, want exactly the deleted landfill file: %v", len(found), found)
	}
	if !strings.Contains(found[0], deletedLandfillTestFile) {
		t.Errorf("finding = %q, want it to name %s", found[0], deletedLandfillTestFile)
	}
}

// docMapEntry matches a map entry in a doc comment: a tab-indented line whose
// first token is a test file name.
//
// Read from the entries alone, and not from every `*_test.go` a doc.go mentions,
// because the looser reading has to be wrong. internal/orchestrator/doc.go names
// regression_test.go in prose on purpose — to say what used to be there and is
// deliberately gone — and a check that cannot tell a map entry from a historical
// reference would demand the deleted file back.
var docMapEntry = regexp.MustCompile(`(?m)^//\t([a-z_0-9]+_test\.go)\s`)

// A doc.go that maps a package's test files by name is making a claim that rots
// silently: nothing about renaming or merging a test file reminds anyone to
// revisit a paragraph in a different one, and a map entry that no longer resolves
// is worse than no map, because it sends the reader looking for something that is
// not there.
//
// One direction only. Every file an entry names must exist; the converse is not
// asserted, because such a map deliberately lists the cross-cutting files — the
// ones whose subject is not the name of a source file — and not a whole
// directory. So this cannot see a new test file that went undocumented, nor one
// named in the prose around the map.
func TestDocMapsNameTestFilesThatExist(t *testing.T) {
	found, seen, err := staleDocMapEntries(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range found {
		t.Error(v)
	}
	// The map is the whole subject, so a regex that had quietly stopped matching
	// would otherwise leave this passing on an empty set. internal/orchestrator's
	// map alone carries nine entries.
	if seen < 5 {
		t.Errorf("found %d doc.go map entries across the module; the parse has broken, and a"+
			" passing result here would mean nothing", seen)
	}
}

// staleDocMapEntries reports every doc.go map entry naming a test file that is
// not beside it, and how many entries it read — the second is what keeps the
// caller from passing vacuously.
func staleDocMapEntries(root string) (found []string, seen int, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			if ignoredPackageDir(d.Name()) {
				return filepath.SkipDir
			}
			if isModuleRoot(path) {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "doc.go" {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range docMapEntry.FindAllStringSubmatch(string(body), -1) {
			seen++
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), m[1])); err != nil {
				rel, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				found = append(found, fmt.Sprintf(
					"%s: the map sends the reader to %s, which is not beside it",
					filepath.ToSlash(rel), m[1]))
			}
		}
		return nil
	})
	return found, seen, err
}

// The same shape as the landfill control: one synthetic tree where a map entry
// resolves and another does not.
func TestStaleDocMapEntriesIsARealTrigger(t *testing.T) {
	root := t.TempDir()
	doc := "// Package x.\n" +
		"//\n" +
		"//\tclaim_test.go       exists, and must not be reported\n" +
		"//\tvanished_test.go    does not exist, and must be\n" +
		"package x\n"
	if err := os.WriteFile(filepath.Join(root, "doc.go"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "claim_test.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	found, seen, err := staleDocMapEntries(root)
	if err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("read %d entries, want both", seen)
	}
	if len(found) != 1 {
		t.Fatalf("reported %d findings, want exactly the missing one: %v", len(found), found)
	}
	if !strings.Contains(found[0], "vanished_test.go") {
		t.Errorf("finding = %q, want it to name vanished_test.go", found[0])
	}
}
