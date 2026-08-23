package agenttest

import (
	"go/ast"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/partest"
)

// The cohort split (#167) is a table of judgements, and these check it against
// the source that justifies each one rather than against itself — see the
// commentary on partest.Marker for why a list cannot check its own entries.
//
// One of the four rules is enforced a second time, and more brutally, by the
// `testing` package: `t.Setenv` after `t.Parallel` panics. That makes globalEnv
// the rule least likely to rot. It is checked here anyway, because a panic
// mid-suite names a goroutine and this names the row to fix.

// serialMarkers constrain a case's cohort regardless of what the table says.
var serialMarkers = []struct {
	marker  partest.Marker
	allowed []runMode
}{
	{
		marker: partest.Marker{
			Name:  "t.Setenv/t.Chdir",
			Calls: []string{"Setenv", "Chdir"},
			Why: "it mutates state the whole test binary shares, so it must run while nothing " +
				"else does — and the testing package panics on a t.Setenv after t.Parallel, so a " +
				"parallel classification here fails as a panic rather than as a verdict",
		},
		allowed: []runMode{globalEnv},
	},
	{
		marker: partest.Marker{
			Name: "core.RunLimits stall/attempt window",
			Keys: []string{"StallTimeout", "AttemptTimeout"},
			// Qualified by the type it is named for. `AttemptTimeout` is also the
			// publisher TTL gate's operand on Options (SPEC §7.7, #156) —
			// arithmetic evaluated at Ready, over a window nothing waits out — and
			// an unqualified key would read every correct use of it as a §7.4
			// window and force the wrong cohort.
			Types: []string{"RunLimits"},
			Why: "a case that pins a §7.4 window is asserting what happens when that window " +
				"closes on a machine whose load it does not control",
		},
		allowed: []runMode{liveness, discipline},
	},
	{
		marker: partest.Marker{
			Name:  "time.Since",
			Calls: []string{"Since"},
			Why:   "an elapsed-time assertion measures the machine as much as the code",
		},
		allowed: []runMode{liveness, discipline},
	},
	{
		marker: partest.Marker{
			Name:  "process-liveness sample",
			Funcs: []string{"aliveNow"},
			Why: "sampling whether a process is alive at an instant, with no polling, is an " +
				"ordering assertion whose margin may not be spent on neighbours (SPEC §7.5, §9.8)",
		},
		allowed: []runMode{liveness, discipline},
	},
}

// Every row states a cohort. Without this, a row added with no mode would take
// the zero runMode — neither cohort, silently serial — and the classification
// would be a thing you could forget rather than a thing you must decide.
func TestEveryCaseDeclaresItsCohort(t *testing.T) {
	valid := []runMode{parallel, globalEnv, liveness, discipline}
	for _, tc := range conformanceCases {
		if !slices.Contains(valid, tc.run) {
			t.Errorf("case %q declares run mode %q, which is none of the four", tc.name, tc.run)
		}
	}
}

// The table's names are distinct and every row has a function: two rows sharing
// one would run a case twice under two names and silently drop the other.
func TestCaseTableIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range conformanceCases {
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

// The classification matches what the cases actually do.
//
// Both directions. A case that carries a marker and sits in a cohort the marker
// forbids is the regression that costs a flake. A case classified globalEnv
// whose source no longer touches the environment is the one that costs seconds
// forever, because nothing else would ever notice it could rejoin the cohort.
func TestSerialClassificationMatchesTheSource(t *testing.T) {
	src, byCase := suiteSource(t)

	for _, tc := range conformanceCases {
		fn := byCase[tc.name]
		if !src.Declares(fn) {
			t.Errorf("case %q names %s, which this package does not declare", tc.name, fn)
			continue
		}
		for _, rule := range serialMarkers {
			carries := src.Carries(fn, rule.marker)
			switch {
			case carries && !slices.Contains(rule.allowed, tc.run):
				t.Errorf("case %q (%s) uses %s but is classified %q; %s.\nIt must be one of: %s",
					tc.name, fn, rule.marker.Name, tc.run, rule.marker.Why, join(rule.allowed))
			case !carries && tc.run == globalEnv && rule.marker.Name == "t.Setenv/t.Chdir":
				t.Errorf("case %q (%s) is classified %q but no longer touches the process "+
					"environment or working directory; it can rejoin the parallel cohort",
					tc.name, fn, tc.run)
			}
		}
	}
}

// The scan is a real detector rather than one that finds nothing.
//
// A marker that silently stopped matching — a renamed helper, a parser pointed
// at the wrong directory — would report every case clean and pass, which is the
// shape of an assertion that proves nothing. Each one must fire somewhere.
func TestEveryMarkerFiresSomewhere(t *testing.T) {
	src, byCase := suiteSource(t)

	for _, rule := range serialMarkers {
		fired := false
		for _, tc := range conformanceCases {
			if src.Carries(byCase[tc.name], rule.marker) {
				fired = true
				break
			}
		}
		if !fired {
			t.Errorf("marker %q matched no case at all: the scan behind "+
				"TestSerialClassificationMatchesTheSource is inert, and every classification it "+
				"anchors is now unchecked", rule.marker.Name)
		}
	}
}

// Both cohorts are non-empty. A split with nothing in the parallel cohort is
// the pre-#167 suite wearing a table, and one with nothing serial means the
// classification stopped distinguishing anything.
func TestBothCohortsAreOccupied(t *testing.T) {
	counts := map[runMode]int{}
	for _, tc := range conformanceCases {
		counts[tc.run]++
	}
	if counts[parallel] == 0 {
		t.Error("no case is in the parallel cohort; the suite is serial again")
	}
	if len(conformanceCases)-counts[parallel] == 0 {
		t.Error("every case is parallel; nothing that mutates the environment or pins a window can be")
	}
	t.Logf("cohorts: %d parallel (bound %d), %d globalEnv, %d liveness, %d discipline",
		counts[parallel], conformanceParallelism, counts[globalEnv], counts[liveness], counts[discipline])
}

// suiteSource parses this package's shipping files and pairs each row of
// conformanceCases with the function it names.
//
// The pairing is read off the declaration rather than off the runtime values: a
// func value carries no name a test can compare, and reflection reports only
// the closure's.
func suiteSource(t *testing.T) (*partest.Source, map[string]string) {
	t.Helper()
	src, err := partest.ParseSource(".", partest.ImplementationFiles)
	if err != nil {
		t.Fatal(err)
	}

	byCase := map[string]string{}
	src.Inspect(func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != "conformanceCases" || len(spec.Values) != 1 {
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

	if len(byCase) != len(conformanceCases) {
		t.Fatalf("read %d rows from the conformanceCases source but the table holds %d at run "+
			"time: the scan is not looking at the table it is checking", len(byCase), len(conformanceCases))
	}
	return src, byCase
}

func join(modes []runMode) string {
	out := make([]string, len(modes))
	for i, m := range modes {
		out[i] = string(m)
	}
	return strings.Join(out, "; ")
}
