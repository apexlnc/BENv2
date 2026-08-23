package partest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The audit is driven over a synthetic package holding one instance of every
// verdict it can reach. Run against a compliant package it could only ever say
// "pass", which is the same output it would give if it had stopped looking.
const audited = `package sample

import (
	"context"
	"testing"
	"time"
)

func parallel(t *testing.T) {}

func TestInTheCohort(t *testing.T)   { parallel(t) }
func TestBlockedByEnv(t *testing.T)  { t.Setenv("X", "1") }
func TestBothAtOnce(t *testing.T)    { parallel(t); t.Setenv("X", "1") }
func TestSilentlySerial(t *testing.T) {}
func TestExempt(t *testing.T)         {}
func TestExemptButJoined(t *testing.T) { parallel(t) }
func TestExemptButBlocked(t *testing.T) { t.Chdir("/tmp") }
func TestDeadline(t *testing.T) { _, cancel := context.WithTimeout(context.Background(), time.Second); cancel() }
func TestDirectParallelDeadline(t *testing.T) { t.Parallel(); _, cancel := context.WithTimeout(context.Background(), time.Second); cancel() }
`

func auditFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample_test.go"), []byte(audited), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCohortAuditReportsEveryDeparture(t *testing.T) {
	a := CohortAudit{
		Dir:  auditFixture(t),
		Join: "parallel",
		Markers: []Marker{
			{Name: "env", Calls: []string{"Setenv", "Chdir"}, Why: "process-global"},
			{Name: "deadline", Calls: []string{"WithTimeout"}, Why: "load-sensitive"},
		},
		Exempt: map[string]string{
			"TestExempt":            "a reason no marker can see",
			"TestExemptButJoined":   "contradicted by the source",
			"TestExemptButBlocked":  "already kept out by a marker",
			"TestNoSuchTestAnyMore": "stale",
		},
	}
	problems, err := a.Problems()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(problems, "\n")

	for _, want := range []struct{ name, fragment string }{
		{"a member that carries a marker", "TestBothAtOnce joins the parallel cohort but uses env"},
		{"a test in neither category", "TestSilentlySerial is neither in the parallel cohort"},
		{"an exemption the source contradicts", "TestExemptButJoined is exempt"},
		{"an exemption a marker already covers", "TestExemptButBlocked is exempt"},
		{"a stale exemption", `exempts "TestNoSuchTestAnyMore"`},
		{"a direct t.Parallel behind a serial marker", "TestDirectParallelDeadline calls t.Parallel directly"},
	} {
		if !strings.Contains(got, want.fragment) {
			t.Errorf("%s was not reported.\ngot:\n%s", want.name, got)
		}
	}

	// And the compliant ones are not reported, or the audit is just noise.
	for _, quiet := range []string{"TestInTheCohort", "TestBlockedByEnv", "TestExempt ", "TestDeadline"} {
		if strings.Contains(got, quiet) {
			t.Errorf("%s was reported, but it complies.\ngot:\n%s", quiet, got)
		}
	}
}

// A scan that finds nothing at all is reported, because that is exactly what it
// would report if it had stopped looking — and a compliant package and a broken
// audit would then be the same output.
//
// One marker that fires is enough, and the second row is why the rule is not
// "every marker fires": the set is shared, so a package with no deadline test
// is not a package whose audit has broken.
func TestCohortAuditReportsAScanThatMatchesNothing(t *testing.T) {
	quiet := map[string]string{
		"TestSilentlySerial":   "not the subject here",
		"TestExempt":           "not the subject here",
		"TestExemptButJoined":  "not the subject here",
		"TestExemptButBlocked": "not the subject here",
		"TestDeadline":         "not the subject here",
		"TestBlockedByEnv":     "not the subject here",
		"TestBothAtOnce":       "not the subject here",
	}
	for _, tc := range []struct {
		name    string
		markers []Marker
		want    bool
	}{
		{
			name:    "nothing matches",
			markers: []Marker{{Name: "never fires", Calls: []string{"NoSuchCallAnywhere"}, Why: "unused"}},
			want:    true,
		},
		{
			name: "one of two matches",
			markers: []Marker{
				{Name: "env", Calls: []string{"Setenv", "Chdir"}, Why: "process-global"},
				{Name: "never fires", Calls: []string{"NoSuchCallAnywhere"}, Why: "unused"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := CohortAudit{Dir: auditFixture(t), Join: "parallel", Markers: tc.markers, Exempt: quiet}
			problems, err := a.Problems()
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Join(problems, "\n")
			if reported := strings.Contains(got, "the scan has found nothing"); reported != tc.want {
				t.Errorf("scan-found-nothing reported = %v, want %v.\ngot:\n%s", reported, tc.want, got)
			}
		})
	}
}

// A package with no join helper is a misconfigured audit, not a clean one.
func TestCohortAuditRefusesAnUnknownJoinHelper(t *testing.T) {
	a := CohortAudit{Dir: auditFixture(t), Join: "noSuchHelper", Markers: DefaultMarkers()}
	if _, err := a.Problems(); err == nil {
		t.Fatal("an audit naming a helper the package does not declare was accepted")
	}
}

// The default marker set is not empty and every entry says why, since the
// reason is what a failing audit hands the next reader.
func TestDefaultMarkersAreUsable(t *testing.T) {
	markers := DefaultMarkers()
	if len(markers) == 0 {
		t.Fatal("DefaultMarkers is empty")
	}
	for _, m := range markers {
		if m.Name == "" {
			t.Error("a default marker has no name")
		}
		if m.Why == "" {
			t.Errorf("default marker %q gives no reason", m.Name)
		}
		if len(m.Calls) == 0 && len(m.Keys) == 0 && len(m.Funcs) == 0 && !m.Go {
			t.Errorf("default marker %q matches nothing at all", m.Name)
		}
	}
}
