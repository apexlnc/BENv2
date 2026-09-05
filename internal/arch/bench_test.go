package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// #62's last acceptance criterion as a property of the build: "no randomized
// production routing, dual dispatch on one issue, or new runtime decision depends
// on benchmark telemetry".
//
// The first two are refusals internal/bench itself makes — it dispatches nothing,
// and a manifest recording two cells on one issue fails to load. The third cannot
// be asserted by a package about itself: it is a statement about everything *else*
// in the module, and the only place it holds still is the import graph. A daemon
// that cannot see the measurement cannot decide from it, whoever edits which
// package next.
//
// Both directions are checked, because they fail differently. Reachability is the
// prohibition. The dependency surface is the shape that keeps internal/bench
// honest about what it is: a reader of files, with no client of the outside world
// in it.

// benchPkg is the measurement package, module-relative.
const benchPkg = "internal/bench"

// daemonPkg is the binary a deployment runs.
const daemonPkg = "cmd/ben"

// benchDeps is everything a non-test file of internal/bench may import beyond the
// standard library.
//
// state is the source — the attempt-outcome log #60 delivered — and core is the
// shared vocabulary. Nothing else: no tracker (so it cannot write to a forge), no
// orchestrator (so it holds no piece of the loop), no workspace, no harness.
// Widening this list is a decision about what a benchmark is, which is why it is
// a decision somebody has to make here.
var benchDeps = map[string]string{
	"internal/state": "the attempt-outcome log is the measurement source (#60, #62)",
	"internal/core":  "the closed enums a record is spelled in (SPEC §6-8)",
}

// No package the daemon links may import the measurement.
//
// Transitive, over the whole module: `cmd/ben` importing internal/orchestrator
// importing internal/bench is the failure this exists for, and a rule about
// direct imports would not see it.
func TestTheDaemonCannotReachTheBenchmark(t *testing.T) {
	root := moduleRoot(t)
	graph, err := importGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := graph[benchPkg]; !ok {
		t.Fatalf("%s is not in the import graph; this test is checking nothing", benchPkg)
	}
	if _, ok := graph[daemonPkg]; !ok {
		t.Fatalf("%s is not in the import graph; this test is checking nothing", daemonPkg)
	}

	if path := pathTo(graph, daemonPkg, benchPkg); path != nil {
		t.Errorf("%s reaches %s: %s\n"+
			"benchmark telemetry must not be able to become a runtime decision (#62). The comparison "+
			"is a separate command (cmd/benchreport) for exactly this reason.",
			daemonPkg, benchPkg, strings.Join(path, " -> "))
	}
}

// The measurement's own dependency surface.
func TestTheBenchmarkImportsOnlyItsSource(t *testing.T) {
	root := moduleRoot(t)
	imports, err := packageImports(root, benchPkg, false)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, imp := range imports {
		rel, ok := moduleRelative(imp)
		if !ok {
			continue // stdlib, or a third-party dep boundaryOwners already governs
		}
		seen++
		if _, allowed := benchDeps[rel]; !allowed {
			t.Errorf("%s imports %s, which is not one of its permitted sources %v. A benchmark that "+
				"can reach the loop or a forge adapter is no longer only a reader of files (#62)",
				benchPkg, rel, sortedKeys(benchDeps))
		}
	}
	if seen == 0 {
		t.Errorf("%s imports nothing from this module, so its permitted-source list is inert", benchPkg)
	}
}

// Exactly one package imports the measurement, and it is the command that exists
// to.
//
// Driven by the graph rather than by a list of importers: the failure is a
// *second* importer, whichever package grows one, and a new binary that reads a
// benchmark is a decision to record here.
func TestOnlyTheReportCommandImportsTheBenchmark(t *testing.T) {
	root := moduleRoot(t)
	graph, err := importGraph(root)
	if err != nil {
		t.Fatal(err)
	}
	var importers []string
	for pkg, deps := range graph {
		if pkg == benchPkg {
			continue
		}
		for _, dep := range deps {
			if dep == benchPkg {
				importers = append(importers, pkg)
				break
			}
		}
	}
	sort.Strings(importers)
	want := []string{"cmd/benchreport"}
	if strings.Join(importers, ",") != strings.Join(want, ",") {
		t.Errorf("packages importing %s = %v, want %v", benchPkg, importers, want)
	}
}

// A synthetic tree carrying both answers, for the reason every walk in this
// package has one: a reachability check that only ever runs over a repository
// where it passes cannot be distinguished from a check that reads nothing.
func TestTheReachabilityCheckIsARealTrigger(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  bool
	}{
		{
			name: "the daemon reaching the benchmark through the loop",
			files: map[string]string{
				"cmd/ben/main.go":               "package main\n\nimport _ \"example.com/outer/internal/orchestrator\"\n",
				"internal/orchestrator/loop.go": "package orchestrator\n\nimport _ \"example.com/outer/internal/bench\"\n",
				"internal/bench/cohort.go":      "package bench\n",
			},
			want: true,
		},
		{
			// #243: the bridge the walk used to skip. The edge into the testdata
			// package was built and the node never visited, so the search stopped
			// one hop short of the measurement.
			name: "the daemon reaching the benchmark through a testdata package",
			files: map[string]string{
				"cmd/ben/main.go":                 "package main\n\nimport _ \"example.com/outer/internal/x/testdata/y\"\n",
				"internal/x/testdata/y/helper.go": "package y\n\nimport _ \"example.com/outer/internal/bench\"\n",
				"internal/bench/cohort.go":        "package bench\n",
			},
			want: true,
		},
		{
			// The same bridge by the route that survives the import rule above: a
			// test file may name a testdata package, and importGraph counts test
			// files on purpose.
			name: "a test file bridging the daemon's own package to the benchmark",
			files: map[string]string{
				"cmd/ben/main.go":                 "package main\n\nimport _ \"example.com/outer/internal/x\"\n",
				"internal/x/x.go":                 "package x\n",
				"internal/x/x_test.go":            "package x\n\nimport _ \"example.com/outer/internal/x/testdata/y\"\n",
				"internal/x/testdata/y/helper.go": "package y\n\nimport _ \"example.com/outer/internal/bench\"\n",
				"internal/bench/cohort.go":        "package bench\n",
			},
			want: true,
		},
		{
			name: "the same packages with the loop importing the state dir instead",
			files: map[string]string{
				"cmd/ben/main.go":               "package main\n\nimport _ \"example.com/outer/internal/orchestrator\"\n",
				"internal/orchestrator/loop.go": "package orchestrator\n\nimport _ \"example.com/outer/internal/state\"\n",
				"internal/state/state.go":       "package state\n",
				"internal/bench/cohort.go":      "package bench\n",
			},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"), "module example.com/outer\n\ngo 1.26\n")
			for name, body := range tc.files {
				writeFile(t, filepath.Join(root, filepath.FromSlash(name)), body)
			}
			graph, err := importGraph(root)
			if err != nil {
				t.Fatal(err)
			}
			// The synthetic module's own path, not this repository's: moduleRelative
			// is what the real test uses, and it is pinned to this module by design.
			path := pathTo(graph, daemonPkg, benchPkg)
			if got := path != nil; got != tc.want {
				t.Fatalf("reachable = %v (%v), want %v", got, path, tc.want)
			}
		})
	}
}

// modulePath is this module's import path prefix. Stated rather than read from
// go.mod: every rule in this package is expressed as a path within *this* module
// (see isModuleRoot), and this is the same commitment spelled for imports.
const modulePath = "github.com/srhg-ai-7cef3f93/ben/"

// moduleRelative turns an import path into a module-relative package directory,
// and reports whether it belongs to this module at all.
func moduleRelative(importPath string) (string, bool) {
	rel, ok := strings.CutPrefix(importPath, modulePath)
	return rel, ok
}

// importGraph maps every package directory in a module to the module-relative
// packages it imports, test files included.
//
// Test files count. An orchestrator test that imported the benchmark would be
// asserting something about the loop from measurement data, which is the same
// coupling the rule forbids, arriving by a route nobody reviews as production
// code.
//
// Packages under testdata count too, for the same reason and a sharper one
// (#243). They are importable by module path, so they are graph *nodes*, not
// just edge targets: skipping them still built the edge into a testdata package
// and then never visited it, which ended the search one hop short of
// `internal/x_test.go -> internal/x/testdata/y -> internal/bench`. A helper a
// test file may legitimately import is exactly where that bridge would sit.
func importGraph(root string) (map[string][]string, error) {
	graph := map[string][]string{}
	// The synthetic trees in the trigger test use their own module path, so the
	// prefix is discovered per walk rather than fixed: whatever sits before
	// `internal/` or `cmd/` in an import is that module's name.
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (ignoredImportDir(d.Name()) || isModuleRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkgDir := filepath.ToSlash(rel)
		if _, ok := graph[pkgDir]; !ok {
			graph[pkgDir] = nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if dep, ok := withinModule(ip); ok {
				graph[pkgDir] = append(graph[pkgDir], dep)
			}
		}
		return nil
	})
	return graph, err
}

// withinModule strips whatever module prefix an import carries and reports
// whether what is left names a package of the module being walked — `internal/…`
// or `cmd/…`. Prefix-agnostic so the synthetic trigger tree, whose module is
// example.com/outer, is read the same way this repository is.
func withinModule(importPath string) (string, bool) {
	for _, top := range []string{"/internal/", "/cmd/"} {
		if i := strings.Index(importPath, top); i >= 0 {
			return importPath[i+1:], true
		}
	}
	return "", false
}

// packageImports returns one package's imports. withTests includes _test.go
// files.
func packageImports(root, pkgDir string, withTests bool) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(pkgDir)))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		if !withTests && strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, filepath.FromSlash(pkgDir), e.Name()),
			nil, parser.ImportsOnly)
		if err != nil {
			return nil, err
		}
		for _, imp := range file.Imports {
			out = append(out, strings.Trim(imp.Path.Value, `"`))
		}
	}
	return out, nil
}

// pathTo returns a shortest import path from one package to another, or nil.
// The path itself, not a boolean: a failure has to say which edge to cut.
func pathTo(graph map[string][]string, from, to string) []string {
	type step struct {
		pkg  string
		path []string
	}
	seen := map[string]bool{from: true}
	queue := []step{{pkg: from, path: []string{from}}}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		deps := append([]string(nil), graph[cur.pkg]...)
		sort.Strings(deps) // a stable answer where several routes exist
		for _, dep := range deps {
			if dep == to {
				return append(cur.path, dep)
			}
			if seen[dep] {
				continue
			}
			seen[dep] = true
			queue = append(queue, step{pkg: dep, path: append(append([]string(nil), cur.path...), dep)})
		}
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The permitted-source list says why each entry is there, so the next person
// widening it has to write one too.
func TestEveryPermittedSourceCarriesItsReason(t *testing.T) {
	for pkg, why := range benchDeps {
		if strings.TrimSpace(why) == "" {
			t.Errorf("benchDeps[%q] has no reason recorded", pkg)
		}
		if _, err := os.Stat(filepath.Join(moduleRoot(t), filepath.FromSlash(pkg))); err != nil {
			t.Errorf("benchDeps names %s, which is not a package here: %v", pkg, err)
		}
	}
	if fmt.Sprint(sortedKeys(benchDeps)) == "[]" {
		t.Error("benchDeps is empty, so the surface check permits nothing and would fail on any import")
	}
}
