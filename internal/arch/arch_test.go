// Package arch mechanically enforces the repo's structural invariants — the
// ones a reviewer would otherwise have to notice. Here: SPEC §3 invariant 6
// (normalized boundaries) at the import graph, where each third-party boundary
// dependency is owned by exactly one package and internal/core stays
// stdlib-only. Every third-party source import must match one ownership table;
// in gomod_test.go, the direct requirements provide a second, declaration-side
// anchor and the build's Go version floor is kept to its intended shape. When a
// ticket legitimately needs a new dep, record the decision in the tables below
// in the same PR with a comment citing the ticket (see AGENTS.md).
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

// boundaryOwners maps a third-party import path prefix to the single package
// prefix (module-relative, slash-separated) allowed to import it.
//
// Not the whole rule by itself. A table like this proves the entries it lists
// behave; it can say nothing about the dependency nobody listed, which is how
// golang.org/x/sync sat outside every boundary rule from the day it was added
// until #243. Two independent inputs now anchor the tables: importViolations
// refuses every third-party source import governed by neither table, while
// TestEveryDirectDependencyHasAnOwnershipDecision refuses every unclassified
// direct requirement in go.mod. The source rule deliberately does not trust an
// `// indirect` marker: adding an import does not rewrite go.mod until tidy runs.
var boundaryOwners = map[string]string{
	"gopkg.in/yaml.v3":             "internal/config",   // WORKFLOW.md front matter (SPEC §5.2)
	"github.com/fsnotify/fsnotify": "internal/config",   // hot reload (SPEC §5.4, B03)
	"github.com/osteele/liquid":    "internal/template", // prompt templates (SPEC §5.6, B02)
	"github.com/google/go-github":  "internal/tracker",  // GitHub adapter (SPEC §8, B04)
}

// unrestrictedDeps records the direct dependencies deliberately left unowned,
// and why.
//
// Ownership confines a *boundary*: a dependency that carries a foreign format, a
// wire type or an external system into the module, which SPEC §3.6 requires be
// translated in exactly one place. A dependency carrying none of those has no
// boundary to confine, and pinning it to an arbitrary sole owner would make the
// entries above mean less rather than more.
//
// The bar this map sets is the reason, not the entry. Its point is that a new
// dependency is a decision somebody writes down (AGENTS.md rule 4) instead of
// one that lands by nobody noticing.
var unrestrictedDeps = map[string]string{
	// The OS syscall extension has no foreign format, wire type, or external
	// system to translate. Harness owns Darwin boot-session observation (#8),
	// while localdomain owns Linux cgroup, namespace, pidfd, statx and openat2
	// operations (#274); forcing either through the other would invert their
	// boundary. Both remain behind substrate-specific packages.
	"golang.org/x/sys": "stdlib syscall extension used by the substrate-specific harness and local-domain boundaries (#8, #274)",
	// A Go-team extension of stdlib `sync`, with no foreign surface: errgroup
	// introduces no format, no wire type and no external system, so there is
	// nothing at a boundary for one owner to translate. SPEC Appendix A names it
	// apart from its four boundary deps, as the *lifecycle* idiom
	// (`signal.NotifyContext` + `errgroup.WithContext`) — process supervision,
	// which is cmd/ben's business as much as internal/orchestrator's, so naming
	// today's only caller as its owner would be a refusal the spec contradicts.
	// The packages that must refuse it already do, under stronger rules:
	// stdlibOnly and noThirdParty. Recorded as part of #243.
	"golang.org/x/sync": "stdlib-shaped concurrency with no foreign surface to confine; " +
		"SPEC Appendix A's lifecycle idiom, not one of its boundary deps (#243)",
}

// stdlibOnly lists package prefixes that must import nothing beyond the
// standard library — not even other packages of this module.
var stdlibOnly = []string{
	"internal/core",      // shared interfaces; everything depends on it, it depends on nothing (B01)
	"internal/gitremote", // shared URL syntax and the credential helper over it (#193, #230)
	"internal/scenario",  // #220's diagnostic schema/renderer must not learn authority or adapter behavior
}

// scenarioConsumer is #220's deliberately narrow execution binding. The
// schema and renderer are safe to reuse as data, but importing them into a
// production package would turn a developer diagnostic format into product
// surface. Only the integration test drives it, against the production loop.
const scenarioConsumer = "internal/integration"

// evidenceIsolation keeps the components that *produce publication evidence*
// from depending on the components a run can influence (SPEC §3.5, §9.7).
//
// It is a structural claim rather than a taste one, and it is the whole basis of
// v2 verification. A run that happens outside BEN authors everything inside its
// own sandbox — its response, its transcript, its filesystem, any git command
// executed through it — so the only thing separating evidence from assertion is
// *where the fact was read*. An import is how that separation is lost by
// accident: a fact source that reached the local workspace tree would be reading
// a directory an agent writes, and one that reached the agent packages would be
// one refactor away from reading a run's own report.
//
// Test files are exempt, deliberately and narrowly. internal/verify asserts that
// *workspace.Provider satisfies its v1 seam, which is a compile-time fact worth
// having and is exactly why that assertion lives in a _test.go file: the
// production import graph keeps the independence, and this rule is what now
// enforces what that comment claims.
var evidenceIsolation = map[string][]string{
	"internal/mirror": {"internal/workspace", "internal/agent", "internal/orchestrator"},
	"internal/verify": {"internal/workspace", "internal/agent", "internal/orchestrator"},
}

// noThirdParty lists package prefixes that may import the standard library and
// this module, and nothing else.
//
// Weaker than stdlibOnly and aimed at a different failure. The v2 substrate
// boundary is where a remote adapter's HTTP client, protobuf schema or cloud SDK
// would arrive, and #192's whole point is that it arrives *behind* this package
// rather than inside it: an interface here that mentioned a wire type would put
// a provider payload in front of the orchestrator, which is SPEC §3 invariant 6.
//
// The rule is expressed as "no third-party import" rather than as a named
// allowlist because it is a property of these packages, independent of the
// module-wide ownership decision. A dependency may be owned elsewhere or
// deliberately unrestricted and still be forbidden at this seam.
var noThirdParty = []string{
	"cmd/ticketprep",      // #222's developer command is an offline binding, not a forge or model client
	"internal/remote",     // the v2 execution-substrate seams and their fake (#192, #46)
	"internal/ticketprep", // #222's offline kernel may use shared Git syntax/invocation boundaries, but no forge or model client
	// The Airlock v2 backend and its contract fake (#194, #46). Listed for a
	// different reason from the one above: #192's boundary must not *mention* a
	// wire type, while this package is nothing but wire types — and the rule it
	// needs is that they are BEN's own, written against the frozen contract, and
	// not a generated client or an imported Airlock module. AGENTS.md rule 3
	// makes that a human sign-off; this makes it a failing test rather than a
	// review comment somebody has to remember to leave.
	"internal/airlock",
}

func TestImportBoundaries(t *testing.T) {
	found, err := violations(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range found {
		t.Error(v)
	}
}

// Every dependency the module declares directly carries a recorded decision:
// an owner, or a written reason it needs none.
//
// This is the declaration-side half, anchored at go.mod rather than at the
// tables. The source-side half lives in importViolations and classifies every
// third-party import independently of whether go.mod currently calls its module
// direct or indirect. Both are needed: a newly written import does not update
// go.mod until tidy runs, while a newly declared direct requirement may have no
// source import for this walk to observe yet.
func TestEveryDirectDependencyHasAnOwnershipDecision(t *testing.T) {
	path := filepath.Join(moduleRoot(t), "go.mod")
	gomod, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(directRequirements(string(gomod))) == 0 {
		t.Fatalf("%s: no direct requirements parsed; this test is checking nothing", path)
	}
	for _, gap := range ownershipGaps(string(gomod)) {
		t.Error(gap)
	}
}

// The other declaration-side direction, and the one the tables cannot report
// about themselves: an entry governing no direct requirement is a rule that has
// quietly stopped applying.
//
// Source imports are checked separately before tidy can rewrite go.mod. Once the
// module file is tidy, a dependency no longer required directly has no source
// import, so its ownership rule protects nothing and the next reader is owed its
// removal rather than its inheritance.
func TestOwnershipTablesGovernCurrentDependencies(t *testing.T) {
	root := moduleRoot(t)
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	required := directRequirements(string(gomod))

	for dep, owner := range boundaryOwners {
		if !governsARequirement(required, dep) {
			t.Errorf("boundaryOwners[%q] matches no direct requirement in go.mod (%v), so the rule is inert; "+
				"remove it with the dependency", dep, required)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(owner))); err != nil {
			t.Errorf("boundaryOwners[%q] names %s as its owner, which is not a package here: %v", dep, owner, err)
		}
	}
	for dep, why := range unrestrictedDeps {
		if !governsARequirement(required, dep) {
			t.Errorf("unrestrictedDeps[%q] matches no direct requirement in go.mod (%v), so the exemption is "+
				"inert; remove it with the dependency", dep, required)
		}
		if strings.TrimSpace(why) == "" {
			t.Errorf("unrestrictedDeps[%q] has no reason recorded; an exemption without one is the silent "+
				"state this table exists to replace", dep)
		}
	}
}

// The gap check's negative control: over a go.mod this repository does not have,
// because the one it does have is (by construction) complete.
func TestOwnershipGapIsARealTrigger(t *testing.T) {
	const header = "module example.com/m\n\ngo 1.26\n\n"
	for _, tc := range []struct {
		name  string
		gomod string
		want  string
	}{
		{
			name:  "a new dependency nobody classified",
			gomod: header + "require (\n\texample.org/cloudsdk v1.0.0\n)\n",
			want:  "example.org/cloudsdk",
		},
		{
			name:  "an owned dependency",
			gomod: header + "require (\n\tgopkg.in/yaml.v3 v3.0.1\n)\n",
		},
		{
			// The prefix is a path prefix: the table's key omits the major-version
			// element go.mod names.
			name:  "an owned dependency under a major-version suffix",
			gomod: header + "require (\n\tgithub.com/google/go-github/v90 v90.0.0\n)\n",
		},
		{
			name:  "a dependency exempted with a reason",
			gomod: header + "require (\n\tgolang.org/x/sync v0.18.0\n)\n",
		},
		{
			// This declaration-side gate deliberately ignores indirect entries.
			// Any actual source import is classified independently below.
			name:  "an indirect entry is outside the direct-declaration gate",
			gomod: header + "require (\n\texample.org/cloudsdk v1.0.0 // indirect\n)\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ownershipGaps(tc.gomod)
			if (len(got) > 0) != (tc.want != "") {
				t.Fatalf("ownershipGaps = %v, want any = %v", got, tc.want != "")
			}
			if tc.want != "" && !strings.Contains(got[0], tc.want) {
				t.Errorf("gap = %q, want it to name %s", got[0], tc.want)
			}
		})
	}
}

// ownershipGaps reports every direct requirement in gomod governed by anything
// other than exactly one entry across both ownership tables.
func ownershipGaps(gomod string) []string {
	return ownershipGapsWithTables(gomod, boundaryOwners, unrestrictedDeps)
}

func ownershipGapsWithTables(gomod string, owners, unrestricted map[string]string) []string {
	var found []string
	for _, module := range directRequirements(gomod) {
		matches := ownershipMatches(module, owners, unrestricted)
		switch len(matches) {
		case 0:
			found = append(found, fmt.Sprintf(
				"go.mod requires %s directly and records no ownership decision for it, so every package in "+
					"the module may import it. Add it to boundaryOwners with the single package allowed to, or "+
					"to unrestrictedDeps with the reason it needs no owner — in this PR, citing the ticket "+
					"(AGENTS.md rule 4, #243)",
				module))
		case 1:
			// Exactly one owner or reasoned exemption is the complete decision.
		default:
			found = append(found, fmt.Sprintf(
				"go.mod requires %s directly and matches %d ownership entries (%s); want exactly one "+
					"owner or reasoned exemption (AGENTS.md rule 4, #243)",
				module, len(matches), ownershipMatchNames(matches)))
		}
	}
	return found
}

type ownershipMatch struct {
	table  string
	prefix string
	value  string
}

// ownershipMatches returns every entry governing a module or import path. Keys
// are path prefixes, so a broad and a narrow entry can both match; sorting makes
// the resulting refusal independent of map iteration order.
func ownershipMatches(path string, owners, unrestricted map[string]string) []ownershipMatch {
	var matches []ownershipMatch
	for prefix, value := range owners {
		if hasPathPrefix(path, prefix) {
			matches = append(matches, ownershipMatch{table: "boundaryOwners", prefix: prefix, value: value})
		}
	}
	for prefix, value := range unrestricted {
		if hasPathPrefix(path, prefix) {
			matches = append(matches, ownershipMatch{table: "unrestrictedDeps", prefix: prefix, value: value})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].table != matches[j].table {
			return matches[i].table < matches[j].table
		}
		return matches[i].prefix < matches[j].prefix
	})
	return matches
}

func ownershipMatchNames(matches []ownershipMatch) string {
	names := make([]string, 0, len(matches))
	for _, match := range matches {
		names = append(names, fmt.Sprintf("%s[%q]", match.table, match.prefix))
	}
	return strings.Join(names, ", ")
}

// governsARequirement reports whether a table key covers any direct requirement.
func governsARequirement(required []string, dep string) bool {
	for _, module := range required {
		if hasPathPrefix(module, dep) {
			return true
		}
	}
	return false
}

// violations walks one module's tree and reports every import that breaks a
// rule above. The root is a parameter, and the findings are returned rather
// than reported, so the scoping rules can be exercised over a synthetic tree —
// see TestNestedModulesAreOutOfScope. Nothing else is testable about a walk
// that only ever runs over this repo and can only say "pass".
func violations(root string) ([]string, error) {
	fset := token.NewFileSet()
	var found []string

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && path != root {
			if ignoredImportDir(d.Name()) {
				return filepath.SkipDir
			}
			if isModuleRoot(path) {
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

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			found = append(found, importViolations(pkgDir, filepath.ToSlash(filepath.Base(path)), ip)...)
		}
		return nil
	})
	return found, err
}

// isModuleRoot reports a nested module, which `go list ./...` would not
// descend into either.
//
// This is not the rule that keeps agent worktrees out — those live under
// `.claude/`, which ignoredImportDir already skips by its leading dot (and so
// therefore does ignoredPackageDir), and removing this check leaves them
// skipped. It earns its place on a different
// argument: a nested module is a *separate* module, so its packages do not
// belong to this one's namespace, and every rule here is expressed as a path
// prefix within a single module. A copy of this repo at `wt/` would report
// `wt/internal/config` importing yaml as a boundary breach, because
// `wt/internal/config` is not under `internal/config` — a false failure about
// a file that is not part of the module being checked.
func isModuleRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

// ignoredImportDir reports a directory the import walks do not enter. Vendor is
// not this module's source. Dot- and underscore-prefixed directories are kept
// out because they include repository metadata and local planning trees that the
// go tool also leaves out of pattern expansion.
//
// This is not a claim that the latter two are unimportable. Like testdata, a
// package below `.hidden` or `_hidden` can be named by an explicit module-path
// import. importViolations therefore refuses those imports outright; that is the
// independent half that makes skipping metadata here safe. `testdata` differs:
// tests legitimately import helpers below it, so the import walks enter it and
// production imports are refused more narrowly (#243).
//
// A package under testdata compiles normally and is importable by module path,
// and this repository has one —
// internal/agent/claudecode/testdata/fakesrt, which fakesrt_test.go imports —
// so the route is proven, not hypothetical. Skipping those directories here let
// a package import yaml or a cloud SDK unseen, and dead-ended the reachability
// BFS one hop short of `cmd/ben -> …/testdata/x -> internal/bench`.
func ignoredImportDir(name string) bool {
	return name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

// ignoredPackageDir is the go tool's pattern-expansion scope: ignoredImportDir
// plus testdata. It stays the scope for the *source-audit* walks in this package
// — markdown cross-references, doc.go maps, test-file names, the operator-facing
// markers in canary_preflight_test.go — where "what `go test ./...` and a reader
// see" is the question being asked. The import walks ask a different one.
func ignoredPackageDir(name string) bool {
	return ignoredImportDir(name) || name == "testdata"
}

// underHiddenPackageDir reports a module-relative package directory with a dot-
// or underscore-prefixed path element. The go tool omits these directories from
// pattern expansion but resolves them when another package imports them by
// module path, so ignoredImportDir is safe only while such imports are refused.
func underHiddenPackageDir(pkgDir string) bool {
	for _, element := range strings.Split(pkgDir, "/") {
		if strings.HasPrefix(element, ".") || strings.HasPrefix(element, "_") {
			return true
		}
	}
	return false
}

// underTestdata reports a module-relative package directory sitting under a
// testdata element at any depth.
func underTestdata(pkgDir string) bool {
	for _, element := range strings.Split(pkgDir, "/") {
		if element == "testdata" {
			return true
		}
	}
	return false
}

func TestPackageDirScopes(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		wantSource, wantImport bool
	}{
		{name: ".claude", wantSource: true, wantImport: true},
		{name: ".git", wantSource: true, wantImport: true},
		{name: "_fixtures", wantSource: true, wantImport: true},
		{name: "vendor", wantSource: true, wantImport: true},
		// The one that differs: out of the go tool's pattern expansion, inside
		// the module's import namespace.
		{name: "testdata", wantSource: true},
		{name: "internal"},
		{name: "cmd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ignoredPackageDir(tc.name); got != tc.wantSource {
				t.Errorf("ignoredPackageDir(%q) = %v, want %v", tc.name, got, tc.wantSource)
			}
			if got := ignoredImportDir(tc.name); got != tc.wantImport {
				t.Errorf("ignoredImportDir(%q) = %v, want %v", tc.name, got, tc.wantImport)
			}
		})
	}
}

func TestUnderTestdata(t *testing.T) {
	for _, tc := range []struct {
		pkgDir string
		want   bool
	}{
		{"internal/agent/claudecode/testdata/fakesrt", true},
		{"internal/agent/claudecode/testdata", true},
		{"testdata/x", true},
		{"internal/agent/claudecode", false},
		// A path element, not a substring: these are ordinary packages.
		{"internal/testdataloader", false},
		{"internal/x/mytestdata", false},
	} {
		t.Run(tc.pkgDir, func(t *testing.T) {
			if got := underTestdata(tc.pkgDir); got != tc.want {
				t.Errorf("underTestdata(%q) = %v, want %v", tc.pkgDir, got, tc.want)
			}
		})
	}
}

func TestUnderHiddenPackageDir(t *testing.T) {
	for _, tc := range []struct {
		pkgDir string
		want   bool
	}{
		{"internal/x/.bridge", true},
		{"internal/x/_bridge", true},
		{".bridge/x", true},
		{"_bridge/x", true},
		{"internal/x/bridge", false},
		{"internal/x/my_bridge", false},
		{"internal/x/dot.bridge", false},
	} {
		t.Run(tc.pkgDir, func(t *testing.T) {
			if got := underHiddenPackageDir(tc.pkgDir); got != tc.want {
				t.Errorf("underHiddenPackageDir(%q) = %v, want %v", tc.pkgDir, got, tc.want)
			}
		})
	}
}

// A nested module is out of scope, and the fixture proves it is a real
// trigger rather than an inert one.
//
// The two subtests are one experiment run twice: the same directory tree,
// differing only in whether a `go.mod` sits at its root. Without the negative
// control the test would pass just as happily if the walk never reached the
// tree at all — the shape where a fixture exercises nothing and pins nothing.
func TestNestedModulesAreOutOfScope(t *testing.T) {
	// Deliberately not under a dotted or underscored name: ignoredPackageDir
	// would skip it by name, and this is a test of the other rule.
	const nested = "wt"

	for _, tc := range []struct {
		name       string
		writeGoMod bool
		wantFound  bool
	}{
		{
			name:       "a nested module is skipped",
			writeGoMod: true,
		},
		{
			// The control: identical files, no module boundary, so the walk
			// reaches them and the path-prefix rules misjudge them exactly as
			// described on isModuleRoot.
			name:      "the same tree without one is walked, and misreads",
			wantFound: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"), "module example.com/outer\n\ngo 1.26\n")

			// A file that is legitimate *in its own module* — internal/config
			// is yaml's owner there, as it is here — and a boundary breach the
			// moment its path is read as belonging to this module.
			writeFile(t, filepath.Join(root, nested, "internal", "config", "load.go"),
				"package config\n\nimport _ \"gopkg.in/yaml.v3\"\n")
			if tc.writeGoMod {
				writeFile(t, filepath.Join(root, nested, "go.mod"), "module example.com/nested\n\ngo 1.26\n")
			}

			found, err := violations(root)
			if err != nil {
				t.Fatalf("violations: %v", err)
			}
			if gotFound := len(found) > 0; gotFound != tc.wantFound {
				t.Fatalf("violations = %v, want any = %v", found, tc.wantFound)
			}
			if tc.wantFound && !strings.Contains(found[0], nested+"/internal/config") {
				t.Errorf("violation = %q, want it to name the nested path", found[0])
			}
		})
	}
}

// A package under testdata is inside the walk, and its imports are judged by the
// same rules as any other package's (#243).
//
// The control is the point. Before this change the walk skipped the directory
// outright, so every one of these files was invisible: a testdata package could
// import yaml, a cloud SDK, or anything else the rules forbid, and the scan
// reported nothing. Both subtests write the same file at two paths, differing
// only in whether one element is named testdata.
func TestTestdataPackagesAreInImportScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{name: "a package under testdata", dir: "internal/agent/testdata/helper"},
		{name: "the same package outside it", dir: "internal/agent/helper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"), "module example.com/outer\n\ngo 1.26\n")
			writeFile(t, filepath.Join(root, filepath.FromSlash(tc.dir), "helper.go"),
				"package helper\n\nimport _ \"gopkg.in/yaml.v3\"\n")

			found, err := violations(root)
			if err != nil {
				t.Fatalf("violations: %v", err)
			}
			if len(found) != 1 {
				t.Fatalf("reported %d findings, want exactly the yaml import: %v", len(found), found)
			}
			if !strings.Contains(found[0], tc.dir) {
				t.Errorf("finding = %q, want it to name %s", found[0], tc.dir)
			}
		})
	}
}

// A go.mod `// indirect` marker describes the module file left by the last tidy;
// it does not prove current source imports nothing from that module. This is the
// exact escape #243 must close: adding the already-indirect x/mod package to an
// orchestrator test leaves go.mod untouched, so the source walk itself must
// refuse the unclassified import.
func TestIndirectRequirementCannotEscapeSourceOwnership(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module "+strings.TrimSuffix(modulePath, "/")+"\n\n"+
		"go 1.26\n\nrequire golang.org/x/mod v0.29.0 // indirect\n")
	writeFile(t, filepath.Join(root, "internal", "orchestrator", "modfile_test.go"),
		"package orchestrator\n\nimport _ \"golang.org/x/mod/modfile\"\n")

	found, err := violations(root)
	if err != nil {
		t.Fatalf("violations: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("reported %d findings, want exactly the unclassified source import: %v", len(found), found)
	}
	if !strings.Contains(found[0], "matches neither boundaryOwners nor unrestrictedDeps") {
		t.Errorf("finding = %q, want the missing ownership decision", found[0])
	}
}

// The testdata import rule, over the import it forbids and the two it allows.
func TestTestdataImportRuleIsATrigger(t *testing.T) {
	const helper = modulePath + "internal/agent/claudecode/testdata/fakesrt"
	for _, tc := range []struct {
		name, pkgDir, file, importPath string
		want                           bool
	}{
		{
			name:   "production code naming a testdata package",
			pkgDir: "internal/agent/claudecode", file: "runner.go",
			importPath: helper,
			want:       true,
		},
		{
			name:   "the daemon naming one",
			pkgDir: "cmd/ben", file: "main.go",
			importPath: helper,
			want:       true,
		},
		{
			name:   "a test file naming one",
			pkgDir: "internal/agent/claudecode", file: "fakesrt_test.go",
			importPath: helper,
		},
		{
			name:   "a testdata package naming its own",
			pkgDir: "internal/agent/claudecode/testdata/fakesrtcmd", file: "main.go",
			importPath: helper,
		},
		{
			// A path element, not a substring.
			name:   "a package whose name merely contains the word",
			pkgDir: "internal/agent/claudecode", file: "runner.go",
			importPath: modulePath + "internal/testdataloader",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := importViolations(tc.pkgDir, tc.file, tc.importPath)
			if found := len(got) > 0; found != tc.want {
				t.Fatalf("importViolations(%q, %q, %q) = %v, want any = %v",
					tc.pkgDir, tc.file, tc.importPath, got, tc.want)
			}
		})
	}
}

// Dot- and underscore-prefixed directories are omitted from the import walk so
// it does not descend into repository metadata or local planning trees. That is
// only safe if the explicit imports the go tool still resolves are refused.
//
// The walk-level cases are the negative control: before this rule, the importer
// was visible, the helper directory was skipped, and both trees produced zero
// findings. The ordinary directory proves the same fixture is otherwise walked.
func TestHiddenPackageImportsCannotEscapeTheWalk(t *testing.T) {
	for _, tc := range []struct {
		name, dir, want string
	}{
		{
			name: "a dot-prefixed package is refused at its importer",
			dir:  ".bridge",
			want: "outside the import walk",
		},
		{
			name: "an underscore-prefixed package is refused at its importer",
			dir:  "_bridge",
			want: "outside the import walk",
		},
		{
			name: "the same ordinary package is walked",
			dir:  "bridge",
			want: "owned by internal/config",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, filepath.Join(root, "go.mod"),
				"module "+strings.TrimSuffix(modulePath, "/")+"\n\ngo 1.26\n")
			writeFile(t, filepath.Join(root, "cmd", "ben", "main.go"),
				"package main\n\nimport _ \""+modulePath+"internal/x/"+tc.dir+"\"\n")
			writeFile(t, filepath.Join(root, "internal", "x", tc.dir, "helper.go"),
				"package bridge\n\nimport _ \"gopkg.in/yaml.v3\"\n")

			found, err := violations(root)
			if err != nil {
				t.Fatalf("violations: %v", err)
			}
			if len(found) != 1 {
				t.Fatalf("reported %d findings, want exactly one: %v", len(found), found)
			}
			if !strings.Contains(found[0], tc.want) {
				t.Errorf("finding = %q, want it to contain %q", found[0], tc.want)
			}
		})
	}
}

func TestHiddenPackageImportRuleIsATrigger(t *testing.T) {
	for _, tc := range []struct {
		name, file, importPath string
		want                   bool
	}{
		{
			name:       "a dot-prefixed package",
			file:       "runner.go",
			importPath: modulePath + "internal/x/.bridge",
			want:       true,
		},
		{
			name:       "an underscore-prefixed package",
			file:       "runner.go",
			importPath: modulePath + "internal/x/_bridge",
			want:       true,
		},
		{
			name:       "test files do not bypass the refusal",
			file:       "runner_test.go",
			importPath: modulePath + "internal/x/.bridge",
			want:       true,
		},
		{
			name:       "a dot inside an ordinary element",
			file:       "runner.go",
			importPath: modulePath + "internal/x/dot.bridge",
		},
		{
			name:       "an underscore inside an ordinary element",
			file:       "runner.go",
			importPath: modulePath + "internal/x/my_bridge",
		},
		{
			name: "another module is outside this rule",
			file: "runner.go",
			// Use a classified dependency so the independent source-ownership
			// rule does not obscure what this case proves.
			importPath: "golang.org/x/sync/.bridge",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := importViolations("internal/agent/claudecode", tc.file, tc.importPath)
			if found := len(got) > 0; found != tc.want {
				t.Fatalf("importViolations(%q, %q) = %v, want any = %v",
					tc.file, tc.importPath, got, tc.want)
			}
		})
	}
}

// The source-side ownership rule is independent of go.mod and of every table's
// declaration-side completeness check. It must refuse an import nobody
// classified, while accepting each of the three namespaces and both deliberate
// third-party decisions.
func TestThirdPartySourceOwnershipDecisionIsATrigger(t *testing.T) {
	for _, tc := range []struct {
		name, pkgDir, file, importPath string
		want                           string
	}{
		{
			name:       "an unclassified third-party package",
			pkgDir:     "internal/orchestrator",
			file:       "modfile_test.go",
			importPath: "golang.org/x/mod/modfile",
			want:       "matches neither boundaryOwners nor unrestrictedDeps",
		},
		{
			name:       "an owned dependency at its owner",
			pkgDir:     "internal/config",
			file:       "load.go",
			importPath: "gopkg.in/yaml.v3",
		},
		{
			name:       "an owned dependency outside its owner",
			pkgDir:     "internal/orchestrator",
			file:       "tick.go",
			importPath: "gopkg.in/yaml.v3",
			want:       "owned by internal/config",
		},
		{
			name:       "an unrestricted dependency",
			pkgDir:     "internal/orchestrator",
			file:       "tick.go",
			importPath: "golang.org/x/sync/errgroup",
		},
		{
			name:       "a standard-library package",
			pkgDir:     "internal/orchestrator",
			file:       "tick.go",
			importPath: "net/http",
		},
		{
			name:       "a package in this module",
			pkgDir:     "internal/orchestrator",
			file:       "tick.go",
			importPath: modulePath + "internal/core",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := importViolations(tc.pkgDir, tc.file, tc.importPath)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("importViolations(%q, %q, %q) = %v, want none",
						tc.pkgDir, tc.file, tc.importPath, got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("importViolations(%q, %q, %q) = %v, want one containing %q",
					tc.pkgDir, tc.file, tc.importPath, got, tc.want)
			}
		})
	}
}

// A prefix table is not a longest-match routing table: broad and narrow entries
// are two ownership decisions even when their package owners nest. Before #243,
// boundaryDecision returned whichever map entry iteration found first, so both
// the declaration and source checks accepted this exact duplicate silently.
func TestOverlappingOwnershipPrefixesAreRejected(t *testing.T) {
	owners := map[string]string{
		"github.com/google/go-github":     "internal/tracker",
		"github.com/google/go-github/v90": "internal/tracker/github",
	}
	const imported = "github.com/google/go-github/v90/github"
	const gomod = "module example.com/m\n\ngo 1.26\n\n" +
		"require github.com/google/go-github/v90 v90.0.0\n"

	for _, tc := range []struct {
		name string
		got  []string
	}{
		{
			name: "a source import",
			got: sourceOwnershipViolations(
				"internal/tracker/github", "client.go", imported, owners, nil),
		},
		{
			name: "a direct requirement",
			got:  ownershipGapsWithTables(gomod, owners, nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.got) != 1 {
				t.Fatalf("findings = %v, want exactly the overlapping-prefix refusal", tc.got)
			}
			for _, want := range []string{
				"matches 2 ownership entries",
				`boundaryOwners["github.com/google/go-github"]`,
				`boundaryOwners["github.com/google/go-github/v90"]`,
			} {
				if !strings.Contains(tc.got[0], want) {
					t.Errorf("finding = %q, want it to contain %q", tc.got[0], want)
				}
			}
		})
	}
}

// The isolation rule, exercised where the repository cannot: over the import it
// forbids and over the exemption it grants. Without both, a rule that matched
// nothing and a rule that matched everything would look identical from the green
// run above.
func TestEvidenceIsolationIsATrigger(t *testing.T) {
	tests := []struct {
		name, pkgDir, file, importPath string
		want                           bool
	}{
		{
			name:   "a fact source reaching the local workspace tree",
			pkgDir: "internal/mirror", file: "git.go",
			importPath: modulePath + "internal/workspace",
			want:       true,
		},
		{
			name:   "a verifier reaching an agent adapter",
			pkgDir: "internal/verify", file: "remote.go",
			importPath: modulePath + "internal/agent/claudecode",
			want:       true,
		},
		{
			name:   "the same import from a test file",
			pkgDir: "internal/verify", file: "contract_test.go",
			importPath: modulePath + "internal/workspace",
		},
		{
			name:   "a package the rule does not name",
			pkgDir: "internal/orchestrator", file: "tick.go",
			importPath: modulePath + "internal/workspace",
		},
		{
			name:   "an allowed dependency",
			pkgDir: "internal/mirror", file: "git.go",
			importPath: modulePath + "internal/gitcmd",
		},
		{
			// The prefix is a path prefix, not a string one: a package whose name
			// merely starts with a forbidden one is a different package.
			name:   "a package sharing a prefix by spelling",
			pkgDir: "internal/mirror", file: "git.go",
			importPath: modulePath + "internal/workspaceish",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := importViolations(tt.pkgDir, tt.file, tt.importPath)
			if found := len(got) > 0; found != tt.want {
				t.Fatalf("importViolations(%q, %q, %q) = %v, want any = %v",
					tt.pkgDir, tt.file, tt.importPath, got, tt.want)
			}
		})
	}
}

func TestScenarioLabBoundariesAreTriggers(t *testing.T) {
	for _, tc := range []struct {
		name, pkgDir, file, importPath string
		want                           bool
	}{
		{
			name:       "the format cannot import authority code",
			pkgDir:     "internal/scenario",
			file:       "format.go",
			importPath: modulePath + "internal/orchestrator",
			want:       true,
		},
		{
			name:       "a daemon cannot import the diagnostic format",
			pkgDir:     "cmd/ben",
			file:       "main.go",
			importPath: modulePath + "internal/scenario",
			want:       true,
		},
		{
			name:       "production integration code cannot import the format",
			pkgDir:     scenarioConsumer,
			file:       "scenario.go",
			importPath: modulePath + "internal/scenario",
			want:       true,
		},
		{
			name:       "the integration test is the one execution binding",
			pkgDir:     scenarioConsumer,
			file:       "scenario_lab_test.go",
			importPath: modulePath + "internal/scenario",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := importViolations(tc.pkgDir, tc.file, tc.importPath)
			if found := len(got) > 0; found != tc.want {
				t.Fatalf("importViolations(%q, %q, %q) = %v, want any = %v",
					tc.pkgDir, tc.file, tc.importPath, got, tc.want)
			}
		})
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func sourceOwnershipViolations(pkgDir, file, importPath string, owners, unrestricted map[string]string) []string {
	if isStdlib(importPath) || isThisModule(importPath) {
		return nil
	}
	matches := ownershipMatches(importPath, owners, unrestricted)
	switch len(matches) {
	case 0:
		return []string{fmt.Sprintf(
			"%s/%s imports %q — this third-party source import matches neither boundaryOwners nor "+
				"unrestrictedDeps; record its single owner or its reasoned exemption in this PR "+
				"(AGENTS.md rule 4, #243)",
			pkgDir, file, importPath)}
	case 1:
		match := matches[0]
		if match.table == "boundaryOwners" && !hasPathPrefix(pkgDir, match.value) {
			return []string{fmt.Sprintf(
				"%s/%s imports %q — owned by %s/ only (SPEC §3.6; extend the allowlist deliberately if an adapter needs it)",
				pkgDir, file, importPath, match.value)}
		}
		return nil
	default:
		return []string{fmt.Sprintf(
			"%s/%s imports %q — this third-party source import matches %d ownership entries (%s); "+
				"want exactly one owner or reasoned exemption (AGENTS.md rule 4, #243)",
			pkgDir, file, importPath, len(matches), ownershipMatchNames(matches))}
	}
}

func importViolations(pkgDir, file, importPath string) []string {
	found := sourceOwnershipViolations(pkgDir, file, importPath, boundaryOwners, unrestrictedDeps)
	for _, pkg := range stdlibOnly {
		if hasPathPrefix(pkgDir, pkg) && !isStdlib(importPath) {
			found = append(found, fmt.Sprintf("%s/%s imports %q — %s must stay stdlib-only",
				pkgDir, file, importPath, pkg))
		}
	}
	for _, pkg := range noThirdParty {
		if hasPathPrefix(pkgDir, pkg) && !isStdlib(importPath) && !isThisModule(importPath) {
			found = append(found, fmt.Sprintf(
				"%s/%s imports %q — %s takes no third-party dependency (SPEC §3.6; a remote adapter's "+
					"wire types belong behind this boundary, not in it)",
				pkgDir, file, importPath, pkg))
		}
	}
	local, isLocal := moduleRelative(importPath)
	// Dot- and underscore-prefixed directories are outside the filesystem walk,
	// but not outside Go's import namespace: an explicit module-path import
	// resolves a package below either. Refuse that route outright so a skipped
	// package cannot hide a boundary dependency or bridge cmd/ben to a package
	// the reachability graph forbids (#243).
	if isLocal && underHiddenPackageDir(local) {
		found = append(found, fmt.Sprintf(
			"%s/%s imports %q — a dot- or underscore-prefixed package directory is outside the "+
				"import walk and may not be imported by module path",
			pkgDir, file, importPath))
	}
	// A module package under testdata is importable by module path — that is the
	// hole ignoredImportDir describes — so the other half of closing it is refusing
	// the import that would make a test helper part of the product. A test file
	// may name one, and so may another package already under testdata (the helper
	// binary internal/agent/claudecode/testdata/fakesrtcmd builds from its own
	// fakesrt); nothing else may (#243).
	if isLocal && underTestdata(local) && !strings.HasSuffix(file, "_test.go") && !underTestdata(pkgDir) {
		found = append(found, fmt.Sprintf(
			"%s/%s imports %q — a package under testdata is test support, importable by module path only "+
				"from a _test.go file or from another testdata package",
			pkgDir, file, importPath))
	}
	if isLocal && hasPathPrefix(local, "internal/scenario") &&
		!hasPathPrefix(pkgDir, "internal/scenario") &&
		!(pkgDir == scenarioConsumer && strings.HasSuffix(file, "_test.go")) {
		found = append(found, fmt.Sprintf(
			"%s/%s imports %q — #220's scenario format is developer-only and may be executed only by %s test files",
			pkgDir, file, importPath, scenarioConsumer))
	}
	if strings.HasSuffix(file, "_test.go") {
		return found
	}
	for pkg, forbidden := range evidenceIsolation {
		if !isLocal || !hasPathPrefix(pkgDir, pkg) {
			continue
		}
		for _, dep := range forbidden {
			if hasPathPrefix(local, dep) {
				found = append(found, fmt.Sprintf(
					"%s/%s imports %q — %s produces publication evidence and must not depend on %s, "+
						"which a run can influence (SPEC §3.5, §9.7)",
					pkgDir, file, importPath, pkg, dep))
			}
		}
	}
	return found
}

// isThisModule reports an import of this module. modulePath is stated once, in
// bench_test.go, for the reason given there.
func isThisModule(importPath string) bool { return strings.HasPrefix(importPath, modulePath) }

// hasPathPrefix reports whether path is prefix itself or below it.
func hasPathPrefix(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// isStdlib uses the canonical heuristic: standard-library import paths have
// no dot in their first element.
func isStdlib(importPath string) bool {
	first, _, _ := strings.Cut(importPath, "/")
	return !strings.Contains(first, ".")
}

// moduleRoot walks up from the test's working directory to the go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above test directory")
		}
		dir = parent
	}
}

// The no-third-party rule is a real trigger, not an inert one.
//
// It has nothing to catch in this repository today — internal/remote imports
// stdlib and internal/core, which is the state the rule exists to preserve — so a
// rule that had silently stopped matching would look exactly like a rule that is
// being obeyed. The control uses a dependency unrestricted at the module level,
// proving this package-specific rule is the reason it is refused; this module's
// own packages must still be allowed.
func TestNoThirdPartyRuleIsARealTrigger(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/outer\n\ngo 1.26\n")

	guarded := noThirdParty[0]
	writeFile(t, filepath.Join(root, filepath.FromSlash(guarded), "wire.go"),
		"package remote\n\nimport (\n\t_ \"net/http\"\n\t_ \""+modulePath+"internal/core\"\n\t_ \"golang.org/x/sync/errgroup\"\n)\n")

	found, err := violations(root)
	if err != nil {
		t.Fatalf("violations: %v", err)
	}
	if len(found) != 1 {
		t.Fatalf("reported %d findings, want exactly the third-party import: %v", len(found), found)
	}
	if !strings.Contains(found[0], "golang.org/x/sync/errgroup") {
		t.Errorf("finding = %q, want it to name the third-party import", found[0])
	}
}
