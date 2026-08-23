package workspace

import (
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/partest"
)

// This package's tests are the second half of #167. Almost every one of them
// builds a git repository of its own under t.TempDir and drives real `git`
// subprocesses against it, so they share nothing but the machine — which is
// exactly the shape a bounded cohort is for, and exactly why the bound matters:
// unbounded, `t.Parallel` would put as many concurrent `git` processes on the
// host as it has cores, and the load-sensitive cases here are the ones that
// would pay for it.
//
// The cohort is a *complete* policy rather than an opt-in, enforced by
// TestCohortPolicyIsComplete below: a test added to this package is in the
// cohort, or carries a marker that keeps it out, or is exempt for a reason
// written down here. That is what stops the split decaying back into a serial
// suite one new test at a time, with nothing failing while it does.
var cohort = partest.New(workspaceParallelism)

// workspaceParallelism is how many of this package's tests may run at once.
//
// Four, for the reason given on agenttest's own bound: it is what a GitHub
// runner's `-parallel` default allows, so the widest this cohort ever runs is
// the width CI runs it at, rather than a number that varies with whose
// workstation is running `make check`.
const workspaceParallelism = 4

// parallel joins this test to the package's bounded cohort.
func parallel(t *testing.T) {
	t.Helper()
	cohort.Enter(t)
}

// Every test is accounted for: in the cohort, kept out by something a machine
// can see, or exempt with a reason.
//
// The markers are the shared set (partest.DefaultMarkers) with nothing added:
// what keeps a workspace test serial is the same list of facts that keeps an
// agent test serial — a `t.Setenv`, a deadline it waits out, a signal it sends
// to another process, a fan-out of its own.
func TestCohortPolicyIsComplete(t *testing.T) {
	parallel(t)
	audit := partest.CohortAudit{
		Dir:     ".",
		Join:    "parallel",
		Markers: partest.DefaultMarkers(),
	}
	problems, err := audit.Problems()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}
