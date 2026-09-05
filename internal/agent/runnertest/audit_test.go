package runnertest

import (
	"go/ast"
	"go/token"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/partest"
)

// "Universal" is a claim about what this suite does *not* do, and that is the
// one kind of claim a suite cannot make about itself by passing. What follows
// reads this package's own source, so a case added later that reaches for a pid,
// a working directory, an environment variable or a subprocess fails here — at a
// boundary that does not move when somebody edits a list — rather than quietly
// turning the universal suite back into a local one.
//
// Coarse on purpose, in partest.Marker's idiom: it matches selector names, so
// two functions called `Kill` are one function to it. The error points towards
// reporting a fact that is not there, which fails loudly, rather than missing one
// that is.

// localFacts are the operating-system facts a remote substrate has none of. Each
// one is something agenttest legitimately asserts and this package must not.
var localFacts = []partest.Marker{
	{
		Name:  "process identity",
		Calls: []string{"Getpid", "Getppid", "Getpgid", "Setpgid", "FindProcess", "Kill"},
		Why: "a pid, a process group and a signal are facts about a POSIX process on the daemon's " +
			"own host; a remote substrate has none of them, and a fake of one would be a guarantee " +
			"the real backend does not make",
	},
	{
		Name:  "working directory",
		Calls: []string{"Getwd", "Chdir"},
		Why: "cwd is where a local child was launched (SPEC §7.6); a backend run has a workspace " +
			"identity instead, and nothing universal can be said about a path on this host",
	},
	{
		Name:  "process environment",
		Calls: []string{"Setenv", "Getenv", "LookupEnv", "Environ", "Unsetenv"},
		Why: "the child-environment audit is a statement about one harness's own composition " +
			"(SPEC §7.6) and belongs in the adapter's suite",
	},
	{
		Name:  "subprocess",
		Calls: []string{"Command", "CommandContext", "StartProcess"},
		Why: "a universal case that launched a process would be asserting the local substrate's " +
			"shape while claiming to assert every substrate's",
	},
}

// forbiddenImports are packages whose presence in a shipping file is by itself
// the breach: nothing universal needs to spell a signal, exec a process, or
// reach the local harness.
var forbiddenImports = []string{
	"os",
	"os/exec",
	"os/signal",
	"syscall",
	"golang.org/x/sys",
	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness",
	"github.com/srhg-ai-7cef3f93/ben/internal/agent/agenttest",
}

// No case in this package asserts a local operating-system fact.
func TestSuiteAssertsNoLocalOSFacts(t *testing.T) {
	src, byCase := suiteSource(t)

	for _, tc := range cases {
		fn := byCase[tc.name]
		if !src.Declares(fn) {
			t.Errorf("case %q names %s, which this package does not declare", tc.name, fn)
			continue
		}
		for _, m := range localFacts {
			if src.Carries(fn, m) {
				t.Errorf("case %q (%s) reaches a %s; %s.\nIt belongs in internal/agent/agenttest, "+
					"which is the local suite", tc.name, fn, m.Name, m.Why)
			}
		}
	}
}

// The shipping suite imports nothing that could only be about a local process.
//
// A second boundary rather than a restatement of the one above, and each is
// blind where the other looks: the marker scan reads call names and cannot see a
// fact reached through a variable or a type, while an import list cannot see a
// fact reached through a package already allowed.
//
// Scoped to the shipping files deliberately. This very file imports `os` and
// `os/exec` — markerBait is what proves the marker scan is not inert — and a
// detector's positive control is not part of the contract it guards.
func TestSuiteImportsNothingLocal(t *testing.T) {
	src, err := partest.ParseSource(".", partest.ImplementationFiles)
	if err != nil {
		t.Fatal(err)
	}
	src.Inspect(func(n ast.Node) bool {
		spec, ok := n.(*ast.ImportSpec)
		if !ok {
			return true
		}
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return true
		}
		for _, bad := range forbiddenImports {
			if path == bad || strings.HasPrefix(path, bad+"/") {
				t.Errorf("this package imports %q in a shipping file, which is a local-substrate "+
					"dependency; the universal contract is asserted through core.RunHandle alone", path)
			}
		}
		return true
	})
}

// The scan is a real detector rather than one that finds nothing.
//
// A marker that silently stopped matching — a renamed helper, a parser pointed at
// the wrong directory — would report every case clean and pass, which is the
// shape of an assertion that proves nothing. Each marker is fired against
// markerBait, which exists for exactly that.
func TestEveryMarkerFires(t *testing.T) {
	src, err := partest.ParseSource(".", partest.TestFiles)
	if err != nil {
		t.Fatal(err)
	}
	// The bait is reached by name through the parser, so nothing else in this
	// package references it. Named here so it is not dead code to a linter that
	// cannot see a string.
	_ = markerBait
	for _, m := range localFacts {
		if !src.Carries("markerBait", m) {
			t.Errorf("marker %q matched nothing even in markerBait: the scan behind "+
				"TestSuiteAssertsNoLocalOSFacts is inert, and the separation it anchors is unchecked", m.Name)
		}
	}
}

// The table is well-formed: distinct names, and every row has a function.
func TestCaseTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range cases {
		switch {
		case tc.name == "":
			t.Error("a case has no name")
		case seen[tc.name]:
			t.Errorf("case %q appears twice", tc.name)
		case tc.fn == nil:
			t.Errorf("case %q has no function", tc.name)
		}
		seen[tc.name] = true
	}
}

// Every scenario the closed set declares is named inside a case.
//
// A scenario nothing exercises is a shape every substrate is obliged to script
// and nothing ever checks — the same dead weight a Contract field with no reader
// would be. Read off the source rather than by running the suite, because a run
// cannot tell "no case uses it" from "the substrate produced it".
//
// The declarations are excluded by looking only inside function bodies, and
// Scenarios itself is excluded by name: it lists all four, so counting it would
// make every scenario trivially exercised.
func TestEveryScenarioIsExercised(t *testing.T) {
	src, err := partest.ParseSource(".", partest.ImplementationFiles)
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	src.Inspect(func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil || fn.Name.Name == "Scenarios" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				used[id.Name] = true
			}
			return true
		})
		return true
	})
	for _, s := range Scenarios() {
		name := "Scenario" + strings.ToUpper(string(s)[:1]) + string(s)[1:]
		if !used[name] {
			t.Errorf("scenario %q is declared but no case names it: substrates are asked to "+
				"script a shape nothing asserts", s)
		}
	}
}

// suiteSource parses this package's shipping files and pairs each row of `cases`
// with the function it names, read off the declaration — a func value carries no
// name a test can compare, and reflection reports only the closure's.
func suiteSource(t *testing.T) (*partest.Source, map[string]string) {
	t.Helper()
	src, err := partest.ParseSource(".", partest.ImplementationFiles)
	if err != nil {
		t.Fatal(err)
	}

	byCase := map[string]string{}
	src.Inspect(func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "cases" || len(spec.Values) != 1 {
			return true
		}
		lit, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		for _, elt := range lit.Elts {
			row, ok := elt.(*ast.CompositeLit)
			if !ok || len(row.Elts) < 2 {
				continue
			}
			name, ok := row.Elts[0].(*ast.BasicLit)
			if !ok || name.Kind != token.STRING {
				continue
			}
			fn, ok := row.Elts[1].(*ast.Ident)
			if !ok {
				continue
			}
			if unquoted, err := strconv.Unquote(name.Value); err == nil {
				byCase[unquoted] = fn.Name
			}
		}
		return false
	})

	if len(byCase) != len(cases) {
		t.Fatalf("read %d rows from the `cases` source but the table holds %d at run time: "+
			"the scan is not looking at the table it is checking", len(byCase), len(cases))
	}
	return src, byCase
}

// markerBait is the detector's positive control: one line per marker, never
// called. Deleting a line makes that marker's own inertness undetectable, which
// is what TestEveryMarkerFires refuses.
func markerBait() {
	_ = os.Getpid()
	_, _ = os.Getwd()
	_ = os.Getenv("BEN_NOT_READ")
	_ = exec.Command("true")
}
