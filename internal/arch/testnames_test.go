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

// The three forms a doc.go coverage map cites a test in. All three are read,
// because this repository's two largest maps are written in different ones and
// the checker that only knew the first was green over the second for its whole
// life (#244).
var (
	// A test file beside its own package: a tab-indented line whose first token
	// is the file name.
	//
	// Read from the entries alone, and not from every `*_test.go` a doc.go
	// mentions, because the looser reading has to be wrong.
	// internal/orchestrator/doc.go names regression_test.go in prose on purpose
	// — to say what used to be there and is deliberately gone — and a check that
	// cannot tell a map entry from a historical reference would demand the
	// deleted file back.
	docBesideEntry = regexp.MustCompile(`(?m)^//\t([a-z_0-9]+_test\.go)\s`)

	// A test file at a path, which is how a map entry points outside its own
	// package. The slash is required for the reason above: it is what separates
	// a citation from prose about a file that is deliberately gone.
	docPathEntry = regexp.MustCompile(`\b[a-z_0-9]+(?:/[a-z_0-9]+)*/[a-z_0-9]+_test\.go\b`)

	// A test function, cited by name. internal/integration/doc.go's §12.3 map is
	// written entirely in these — space-indented, at cross-package paths — which
	// is precisely what docBesideEntry cannot see.
	docFuncEntry = regexp.MustCompile(`\bTest[A-Z][A-Za-z0-9_]*`)

	// A test function declaration, as the module declares it.
	testFuncDecl = regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]*)\s*\(`)
)

// docTestCitations counts one doc.go's citations by form.
type docTestCitations struct{ beside, paths, funcs int }

// docTestMapAnchors is every doc.go that cites this repository's tests, and the
// exact number of citations of each form its map currently yields.
//
// Per file and per form, rather than one module-wide count (#244). A single
// total is satisfied by whichever file happens to parse: internal/orchestrator's
// fourteen tab-indented entries met the old `seen < 5` on their own while
// internal/integration/doc.go — the largest map in the repo — scored zero, and
// the check stayed green over a row that had never named a real test. Per form
// for the same reason one file cannot vouch for another: integration's map is
// seventeen function names and eight paths, and one combined count either form
// alone could meet would not notice the other extractor going dark.
//
// Exact, because slack is another fail-open parser: changing TestFoo to TsetFoo
// makes the function extractor miss one citation entirely, and a lower bound
// would still pass. Adding or removing a citation therefore updates this anchor
// deliberately in the same review. A zero is a form that file does not use.
var docTestMapAnchors = map[string]docTestCitations{
	"internal/bench/doc.go":        {beside: 2, paths: 1},
	"internal/integration/doc.go":  {funcs: 17, paths: 8},
	"internal/orchestrator/doc.go": {beside: 14},
}

// A doc.go that maps a package's tests is making a claim that rots silently:
// nothing about renaming or merging a test reminds anyone to revisit a paragraph
// in a different file, and a citation that no longer resolves is worse than no
// map, because it sends the reader looking for something that is not there.
//
// One direction only. Everything a map names must exist; the converse is not
// asserted, because such a map deliberately lists the cross-cutting subjects and
// not a whole directory. So this cannot see a new test that went undocumented.
func TestDocMapsCiteTestsThatExist(t *testing.T) {
	found, _, err := staleDocCitations(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range found {
		t.Error(v)
	}
}

// The vacuity guard, which is the half that decides whether the test above
// means anything.
//
// Both directions are asserted: every listed map still matches its anchor, and
// every doc.go that cites a test at all is listed. Without the second, a new
// coverage map is unguarded from the day it lands — which is exactly how
// internal/integration/doc.go arrived.
func TestEveryDocTestMapMatchesItsAnchor(t *testing.T) {
	_, seen, err := staleDocCitations(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range docTestMapAnchorFindings(seen, docTestMapAnchors) {
		t.Error(finding)
	}
}

func docTestMapAnchorFindings(seen, anchors map[string]docTestCitations) (found []string) {
	for file, want := range anchors {
		got, ok := seen[file]
		if !ok {
			found = append(found, fmt.Sprintf("%s cites no test at all; its map is gone, or the parse that reads it is", file))
			continue
		}
		for _, form := range []struct {
			name      string
			got, want int
		}{
			{"beside-file entries", got.beside, want.beside},
			{"path citations", got.paths, want.paths},
			{"function citations", got.funcs, want.funcs},
		} {
			if form.got != form.want {
				found = append(found, fmt.Sprintf(
					"%s yields %d %s, but its anchor records %d; update the anchor if the map changed intentionally, otherwise its parse has broken",
					file, form.got, form.name, form.want))
			}
		}
	}
	for file := range seen {
		if _, ok := anchors[file]; !ok {
			found = append(found, fmt.Sprintf(
				"%s cites a test but has no count anchor; add it, so a broken parse over this file cannot hide behind another file's entries", file))
		}
	}
	return found
}

// A one-citation loss must trip the anchor. This is the exact partial-vacuity
// shape a lower bound permits when a typo stops matching an extractor at all.
func TestDocTestMapAnchorIsARealTrigger(t *testing.T) {
	seen := map[string]docTestCitations{"doc.go": {funcs: 1}}
	anchors := map[string]docTestCitations{"doc.go": {funcs: 2}}
	found := docTestMapAnchorFindings(seen, anchors)
	if len(found) != 1 || !strings.Contains(found[0], "yields 1 function citations") {
		t.Fatalf("findings = %v, want the one-citation loss", found)
	}
}

// staleDocCitations reports every doc.go citation of a test that does not
// resolve, and how many of each form it read per file — the second is what keeps
// the caller from passing vacuously.
func staleDocCitations(root string) (found []string, seen map[string]docTestCitations, err error) {
	testFiles, testFuncs, err := moduleTests(root)
	if err != nil {
		return nil, nil, err
	}
	seen = map[string]docTestCitations{}
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		file := filepath.ToSlash(rel)
		counts := seen[file]

		for _, m := range docBesideEntry.FindAllStringSubmatch(string(body), -1) {
			counts.beside++
			if _, err := os.Stat(filepath.Join(filepath.Dir(path), m[1])); err != nil {
				found = append(found, fmt.Sprintf(
					"%s: the map sends the reader to %s, which is not beside it", file, m[1]))
			}
		}
		for _, m := range docPathEntry.FindAllString(string(body), -1) {
			counts.paths++
			switch matches := testFiles[m]; len(matches) {
			case 0:
				found = append(found, fmt.Sprintf(
					"%s: the map sends the reader to %s, which no test file in this module resolves to", file, m))
			case 1:
			default:
				found = append(found, fmt.Sprintf(
					"%s: the map sends the reader to %s, which resolves to %d test files: %s",
					file, m, len(matches), strings.Join(matches, ", ")))
			}
		}
		for _, m := range docFuncEntry.FindAllString(string(body), -1) {
			counts.funcs++
			switch matches := testFuncs[m]; len(matches) {
			case 0:
				found = append(found, fmt.Sprintf(
					"%s: the map cites %s, which no test file in this module declares", file, m))
			case 1:
			default:
				found = append(found, fmt.Sprintf(
					"%s: the map cites %s, which is declared by %d test files: %s",
					file, m, len(matches), strings.Join(matches, ", ")))
			}
		}

		if counts != (docTestCitations{}) {
			seen[file] = counts
		}
		return nil
	})
	return found, seen, err
}

// moduleTests indexes one module's tests the two ways a map cites them: every
// path-suffix of every test file, and every test function name. Each key keeps
// all of its declarations: a citation that resolves twice is ambiguous rather
// than valid.
//
// Suffixes because a cross-package citation is written from wherever the reader
// is standing — internal/integration/doc.go says orchestrator/transitions_test.go
// — and demanding the module-relative path would make the map less readable to
// buy nothing. Function names are matched module-wide for the same reason, but
// only a unique match resolves: duplicate test names already exist across this
// module, and collapsing them to one bit lets an unrelated test validate a
// stale citation.
func moduleTests(root string) (files, funcs map[string][]string, err error) {
	files, funcs = map[string][]string{}, map[string][]string{}
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
		if !strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		elements := strings.Split(filepath.ToSlash(rel), "/")
		file := filepath.ToSlash(rel)
		for i := range elements {
			suffix := strings.Join(elements[i:], "/")
			files[suffix] = append(files[suffix], file)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range testFuncDecl.FindAllStringSubmatch(string(body), -1) {
			funcs[m[1]] = append(funcs[m[1]], file)
		}
		return nil
	})
	return files, funcs, err
}

// The same shape as the landfill control, over all three citation forms: one
// synthetic tree where each form has an entry that resolves, one that does not,
// and the module-wide forms have one that resolves ambiguously. A checker whose
// findings are always empty is indistinguishable from a repository with nothing
// wrong in it, and #244 is what that looks like.
func TestStaleDocCitationsIsARealTrigger(t *testing.T) {
	root := t.TempDir()
	doc := "// Package x.\n" +
		"//\n" +
		"//\tclaim_test.go       exists beside it, and must not be reported\n" +
		"//\tvanished_test.go    does not, and must be\n" +
		"//\n" +
		"// Cross-package: other/present_test.go resolves, other/ambiguous_test.go\n" +
		"// resolves twice, and other/absent_test.go does not. By name: TestPresent\n" +
		"// is declared, TestAmbiguous is declared twice, and TestAbsent is not.\n" +
		"package x\n"
	writeFile(t, filepath.Join(root, "pkg", "doc.go"), doc)
	writeFile(t, filepath.Join(root, "pkg", "claim_test.go"), "package x\n")
	writeFile(t, filepath.Join(root, "other", "present_test.go"),
		"package other\n\nimport \"testing\"\n\nfunc TestPresent(t *testing.T) {}\n")
	for _, prefix := range []string{"one", "two"} {
		writeFile(t, filepath.Join(root, prefix, "other", "ambiguous_test.go"),
			"package other\n\nimport \"testing\"\n\nfunc TestAmbiguous(t *testing.T) {}\n")
	}

	found, seen, err := staleDocCitations(root)
	if err != nil {
		t.Fatal(err)
	}
	want := docTestCitations{beside: 2, paths: 3, funcs: 3}
	if got := seen["pkg/doc.go"]; got != want {
		t.Fatalf("read %+v citations, want %+v", got, want)
	}
	if len(found) != 5 {
		t.Fatalf("reported %d findings, want the three absent and two ambiguous citations: %v", len(found), found)
	}
	all := strings.Join(found, "\n")
	for _, wantFinding := range []string{
		"vanished_test.go",
		"other/ambiguous_test.go, which resolves to 2 test files",
		"other/absent_test.go",
		"TestAmbiguous, which is declared by 2 test files",
		"TestAbsent",
	} {
		if !strings.Contains(all, wantFinding) {
			t.Errorf("no finding contains %q: %v", wantFinding, found)
		}
	}
}
