// Package arch mechanically enforces the repo's structural invariants — the
// ones a reviewer would otherwise have to notice. Here: SPEC §3 invariant 6
// (normalized boundaries) at the import graph, where each third-party boundary
// dependency is owned by exactly one package and internal/core stays
// stdlib-only. In gomod_test.go: the shape of the build's Go version floor.
// When a ticket legitimately needs a new boundary dep, extend the tables below
// in the same PR with a comment citing the ticket (see AGENTS.md).
package arch

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// boundaryOwners maps a third-party import path prefix to the single package
// prefix (module-relative, slash-separated) allowed to import it.
var boundaryOwners = map[string]string{
	"gopkg.in/yaml.v3":             "internal/config",   // WORKFLOW.md front matter (SPEC §5.2)
	"github.com/fsnotify/fsnotify": "internal/config",   // hot reload (SPEC §5.4, B03)
	"github.com/osteele/liquid":    "internal/template", // prompt templates (SPEC §5.6, B02)
	"github.com/google/go-github":  "internal/tracker",  // GitHub adapter (SPEC §8, B04)
	// Boot identity for §9.10's run marker. Darwin has no boot_id equivalent, so
	// the boot instant comes from kern.boottime, which stdlib syscall exposes
	// only as undecoded bytes; Linux reads /proc and needs nothing. Widened
	// deliberately for #8 (B10), per AGENTS.md rules 3 and 4.
	"golang.org/x/sys": "internal/agent/harness",
}

// stdlibOnly lists package prefixes that must import nothing beyond the
// standard library — not even other packages of this module.
var stdlibOnly = []string{
	"internal/core", // shared interfaces; everything depends on it, it depends on nothing (B01)
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
			if ignoredPackageDir(d.Name()) {
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
// `.claude/`, which ignoredPackageDir already skips by its leading dot, and
// removing this check leaves them skipped. It earns its place on a different
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

func ignoredPackageDir(name string) bool {
	return name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")
}

func TestIgnoredPackageDirMatchesGoToolScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{".claude", true},
		{".git", true},
		{"_fixtures", true},
		{"testdata", true},
		{"vendor", true},
		{"internal", false},
		{"cmd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ignoredPackageDir(tc.name); got != tc.want {
				t.Errorf("ignoredPackageDir(%q) = %v, want %v", tc.name, got, tc.want)
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func importViolations(pkgDir, file, importPath string) []string {
	var found []string
	for dep, owner := range boundaryOwners {
		if hasPathPrefix(importPath, dep) && !hasPathPrefix(pkgDir, owner) {
			found = append(found, fmt.Sprintf("%s/%s imports %q — owned by %s/ only (SPEC §3.6; extend the allowlist deliberately if an adapter needs it)",
				pkgDir, file, importPath, owner))
		}
	}
	for _, pkg := range stdlibOnly {
		if hasPathPrefix(pkgDir, pkg) && !isStdlib(importPath) {
			found = append(found, fmt.Sprintf("%s/%s imports %q — %s must stay stdlib-only",
				pkgDir, file, importPath, pkg))
		}
	}
	return found
}

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
