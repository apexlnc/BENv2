package partest

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The scanner is driven over a synthetic package, positive and negative rows
// together. Run against this repository alone it could only ever say "pass",
// and a scan that matched nothing would say exactly that.
const fixture = `package sample

import (
	"context"
	"testing"
	"time"
)

type limits struct{ StallTimeout time.Duration }

// opts shares limits' field name and means something else by it — the collision
// Marker.Types exists to resolve. The fixture is parsed, never type-checked, so
// pkg.limits below stands in for the selector spelling a real caller writes.
type opts struct{ StallTimeout time.Duration }

func TestPure(t *testing.T)     { helper(t) }
func TestEnv(t *testing.T)      { t.Setenv("X", "1") }
func TestEnvViaHelper(t *testing.T) { mutates(t) }
func TestInClosure(t *testing.T) {
	t.Run("inner", func(t *testing.T) { t.Chdir("/tmp") })
}
func TestWindow(t *testing.T)   { _ = limits{StallTimeout: time.Second} }
func TestSelectorWindow(t *testing.T) { _ = pkg.limits{StallTimeout: time.Second} }
func TestOtherWindow(t *testing.T)    { _ = opts{StallTimeout: time.Second} }
func TestElapsed(t *testing.T)  { _ = time.Since(time.Now()) }
func TestFanOut(t *testing.T)   { go helper(t) }
func TestSeeded(t *testing.T)   { alive(1) }
func TestDeadline(t *testing.T) { _, cancel := context.WithTimeout(context.Background(), time.Second); cancel() }
func TestMain(m *testing.M)     {}

func helper(t *testing.T)  {}
func mutates(t *testing.T) { t.Setenv("Y", "2") }
func alive(pid int) bool   { return pid > 0 }
`

// A shipping file, so ImplementationFiles and TestFiles select different sets.
const shipped = `package sample

func Exported() {}
`

func fixtureDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sample_test.go", fixture)
	write("sample.go", shipped)
	return dir
}

func TestCarriesFindsMarkersAndOnlyThose(t *testing.T) {
	src, err := ParseSource(fixtureDir(t), TestFiles)
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		marker Marker
		want   []string
	}{
		{
			marker: Marker{Name: "env", Calls: []string{"Setenv", "Chdir"}},
			// Directly, through a helper, and inside a closure — the three
			// spellings a real suite uses.
			want: []string{"TestEnv", "TestEnvViaHelper", "TestInClosure"},
		},
		{
			// Unqualified, a key is the whole hazard: every literal spelling it
			// carries the marker, whatever struct it belongs to.
			marker: Marker{Name: "window", Keys: []string{"StallTimeout"}},
			want:   []string{"TestWindow", "TestSelectorWindow", "TestOtherWindow"},
		},
		{
			// Qualified, it is the named type's field and nothing else's — and
			// the selector spelling matches on the simple name, which is what
			// lets a marker say "RunLimits" for `core.RunLimits{…}`.
			marker: Marker{Name: "window on limits", Keys: []string{"StallTimeout"}, Types: []string{"limits"}},
			want:   []string{"TestWindow", "TestSelectorWindow"},
		},
		{
			// A type nothing declares matches nothing, so the qualifier narrows
			// rather than being ignored when it fails to resolve.
			marker: Marker{Name: "window on an absent type", Keys: []string{"StallTimeout"}, Types: []string{"nosuch"}},
		},
		{
			marker: Marker{Name: "elapsed", Calls: []string{"Since"}},
			want:   []string{"TestElapsed"},
		},
		{
			marker: Marker{Name: "deadline", Calls: []string{"WithTimeout"}},
			want:   []string{"TestDeadline"},
		},
		{
			marker: Marker{Name: "fan-out", Go: true},
			want:   []string{"TestFanOut"},
		},
		{
			marker: Marker{Name: "seeded", Funcs: []string{"alive"}},
			want:   []string{"TestSeeded"},
		},
		{
			// The negative control: a marker nothing carries must match
			// nothing, or every "want" above is satisfied by a scanner that
			// says yes to everything.
			marker: Marker{Name: "absent", Calls: []string{"NoSuchCallAnywhere"}},
		},
	} {
		t.Run(tc.marker.Name, func(t *testing.T) {
			var got []string
			for _, fn := range src.TestFunctions() {
				if src.Carries(fn, tc.marker) {
					got = append(got, fn)
				}
			}
			slices.Sort(got)
			want := slices.Clone(tc.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("carriers = %v, want %v", got, want)
			}
		})
	}
}

// TestMain is the harness, not a case, and a scan that offered it as one would
// have every caller special-case it.
func TestTestFunctionsExcludesTestMain(t *testing.T) {
	src, err := ParseSource(fixtureDir(t), TestFiles)
	if err != nil {
		t.Fatal(err)
	}
	got := src.TestFunctions()
	if slices.Contains(got, "TestMain") {
		t.Errorf("TestFunctions = %v, want TestMain excluded", got)
	}
	if !slices.Contains(got, "TestPure") {
		t.Errorf("TestFunctions = %v, want the ordinary tests included", got)
	}
	if src.Declares("Exported") {
		t.Error("a test-files scan reached a shipping file")
	}
}

func TestImplementationFilesSelectsTheOtherHalf(t *testing.T) {
	src, err := ParseSource(fixtureDir(t), ImplementationFiles)
	if err != nil {
		t.Fatal(err)
	}
	if !src.Declares("Exported") {
		t.Error("the shipping file was not read")
	}
	if src.Declares("TestEnv") {
		t.Error("an implementation scan reached a _test.go file")
	}
}

// A scan that matched no file refuses rather than reporting everything clean —
// the failure mode that would retire this whole mechanism in silence.
func TestParseSourceRefusesAnEmptyScan(t *testing.T) {
	_, err := ParseSource(t.TempDir(), TestFiles)
	if err == nil {
		t.Fatal("a directory with no matching files was accepted")
	}
	if !strings.Contains(err.Error(), "report every candidate clean") {
		t.Errorf("err = %v, want it to say why an empty scan is a failure", err)
	}
}

// Recursion in the call graph terminates. A suite where two helpers call each
// other is not exotic, and a walker that looped would hang `make check` rather
// than fail it.
func TestCarriesTerminatesOnMutualRecursion(t *testing.T) {
	dir := t.TempDir()
	body := `package sample

import "testing"

func TestLoop(t *testing.T) { ping(t) }
func ping(t *testing.T)     { pong(t) }
func pong(t *testing.T)     { ping(t) }
`
	if err := os.WriteFile(filepath.Join(dir, "loop_test.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	src, err := ParseSource(dir, TestFiles)
	if err != nil {
		t.Fatal(err)
	}
	if src.Carries("TestLoop", Marker{Name: "env", Calls: []string{"Setenv"}}) {
		t.Error("reported a marker nothing carries")
	}
}
