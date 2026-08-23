package claudecode

import (
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/partest"
)

// This package's own tests are the third part of #167, and the one where the
// bound earns its keep: a readiness check under `srt` launches the sandbox
// runtime, the harness, `git` and `gh`, so a test here is several child
// processes rather than one. What is left serial is most of the elapsed time
// and cannot move — the sandbox and config-dir fixtures narrow PATH, HOME and
// the git identity through t.Setenv, and only a production change that took
// those as parameters instead of reading the process environment would make
// them isolated. The ticket's own non-goal covers that: no test is
// parallelized on isolation that is assumed rather than demonstrated.
var cohort = partest.New(adapterParallelism)

// adapterParallelism is how many of this package's tests may run at once. Four,
// for the reason on agenttest's own bound: it is what a GitHub runner's
// `-parallel` default allows, so the widest this cohort ever runs is the width
// CI runs it at.
const adapterParallelism = 4

// parallel joins this test to the package's bounded cohort.
func parallel(t *testing.T) {
	t.Helper()
	cohort.Enter(t)
}

// parallelGroup is what a table-driven test calls when its *rows* are the
// cohort members. The readiness tables here are the longest members this
// package has — nine rows of a real `Ready`, each launching a harness — so a
// parent that took one slot for the whole table would be the cohort's critical
// path all by itself.
func parallelGroup(t *testing.T) {
	t.Helper()
	cohort.EnterGroup(t)
}

// Every test is accounted for: in the cohort, kept out by something a machine
// can see, or exempt with a reason.
func TestCohortPolicyIsComplete(t *testing.T) {
	parallel(t)
	audit := partest.CohortAudit{
		Dir:     ".",
		Join:    "parallel",
		Group:   "parallelGroup",
		Markers: partest.DefaultMarkers(),
		Exempt: map[string]string{
			// The one exemption a marker cannot see, because what keeps it out
			// is in another package. Its subtests are agenttest's own cohort
			// (agenttest.conformanceParallelism), so joining this one would put
			// a parent in one gate while its children queue in a second — and
			// its serial cases call t.Setenv, which is only safe while nothing
			// else in this binary is running.
			"TestConformance": "its subtests are agenttest's bounded cohort; a test may not " +
				"hold a slot in one gate while its children take slots in another",
			// The audit reads a *directory*, and this directory holds two
			// packages: dogfood_test.go is `claudecode_test`, external so it can
			// import the loader without the #55 cycle, and therefore unable to
			// reach an unexported helper in this one.
			"TestDogfoodWorkflowProviderBlock": "it is in the external claudecode_test package, " +
				"which cannot reach this package's cohort",
		},
	}
	problems, err := audit.Problems()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
