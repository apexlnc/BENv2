package codexexec

import (
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/partest"
)

// The codex-exec half of #167. Same shape as the claude-code adapter's cohort
// and the same bound, because the cost is the same: a test here that reaches
// Ready or Start re-execs this binary as the harness, and what stays serial is
// what narrows the process environment to do it.
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

// No parallelGroup here, unlike the claude-code adapter: this package's
// readiness tables are a second or two end to end, so splitting their rows
// would buy nothing measurable — and `make check`'s wall clock is its slowest
// package, which this is not.
//
// Every test is accounted for: in the cohort, kept out by something a machine
// can see, or exempt with a reason.
func TestCohortPolicyIsComplete(t *testing.T) {
	parallel(t)
	audit := partest.CohortAudit{
		Dir:     ".",
		Join:    "parallel",
		Markers: partest.DefaultMarkers(),
		Exempt: map[string]string{
			"TestConformance": "its subtests are agenttest's bounded cohort; a test may not " +
				"hold a slot in one gate while its children take slots in another",
			// The audit reads a directory, and this one holds two packages:
			// dogfood_test.go is `codexexec_test`, external so it can import the
			// loader without the #55 cycle, and so cannot reach an unexported
			// helper in this one.
			"TestDogfoodWorkflowProviderBlock": "it is in the external codexexec_test package, " +
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
