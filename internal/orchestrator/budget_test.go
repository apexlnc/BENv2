package orchestrator

import (
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// SPEC §8.5's budget is per tick, and only the loop knows where a tick begins.
// One window per tick, opened before the reads it pays for.
func TestEachTickOpensTheTrackerRequestWindow(t *testing.T) {
	budget := &budgetTracker{}
	h := start(t, harnessOpts{budget: budget})

	// The startup tick has already run by the time start returns (§9.4).
	if got := budget.windows(); got != 1 {
		t.Fatalf("opened %d windows over the startup tick, want 1", got)
	}
	for i := 2; i <= 4; i++ {
		h.Tick()
		if got := budget.windows(); got != i {
			t.Fatalf("opened %d windows over %d ticks, want one each", got, i)
		}
	}
}

// The cadence is part of the capability contract: the tracker needs it to keep
// a shorter legal poll interval from multiplying an hourly API allowance.
func TestTickPassesPollingCadenceToTrackerBudget(t *testing.T) {
	budget := &budgetTracker{}
	start(t, harnessOpts{
		budget:      budget,
		extraConfig: "polling:\n  interval_ms: 1000\n",
	})
	if got := budget.cadence(0); got != time.Second {
		t.Errorf("budget cadence = %s, want 1s from the active definition", got)
	}
}

// A spent budget degrades visibly: the tick that could not afford its reads says
// so in the operator log, with the numbers behind it (SPEC §8.5, §10.3).
func TestARefusedTickIsReported(t *testing.T) {
	logs := &syncBuffer{}
	budget := &budgetTracker{report: core.RequestReport{Billed: 40, Unbilled: 12, Refused: 3}}
	h := start(t, harnessOpts{budget: budget, logs: logs})
	h.Tick()

	out := logs.String()
	if !strings.Contains(out, "per-tick API request budget") {
		t.Errorf("the log never mentions the budget:\n%s", out)
	}
	for _, want := range []string{"refused=3", "billed=40", "unbilled=12"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "level=WARN") == false {
		t.Errorf("a refused tick was reported quietly:\n%s", out)
	}
}

// A continuation that had to cross a window is completed rather than refused,
// but it still means the daemon ran slower than one tick and deserves the same
// operator-visible warning.
func TestADeferredTickIsReported(t *testing.T) {
	logs := &syncBuffer{}
	budget := &budgetTracker{report: core.RequestReport{Billed: 40, Deferred: 2}}
	h := start(t, harnessOpts{budget: budget, logs: logs})
	h.Tick()

	out := logs.String()
	for _, want := range []string{"per-tick API request budget", "deferred=2", "billed=40", "level=WARN"} {
		if !strings.Contains(out, want) {
			t.Errorf("deferred report is missing %q:\n%s", want, out)
		}
	}
}

// And a tick that stayed inside its budget reports without raising an alarm: the
// numbers are worth having every tick, a warning every tick is not.
func TestAnAffordableTickIsReportedQuietly(t *testing.T) {
	logs := &syncBuffer{}
	budget := &budgetTracker{report: core.RequestReport{
		Billed: 4, Unbilled: 9, Pending: 2, LateBilled: 3, LateUnbilled: 1,
	}}
	h := start(t, harnessOpts{budget: budget, logs: logs})
	h.Tick()

	out := logs.String()
	for _, want := range []string{
		"billed=4", "unbilled=9", "pending=2", "late_billed=3", "late_unbilled=1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the tick's cost is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "per-tick API request budget") {
		t.Errorf("a tick with budget to spare warned about it:\n%s", out)
	}
}

// The capability is discovered, not required. fake.Tracker deliberately does not
// implement it — an in-memory tracker has no API cost to meter, and a fake that
// claimed a bound it does not have would let the loop come to depend on one — so
// a tick over one has to be entirely unaffected.
func TestATrackerWithoutABudgetTicksNormally(t *testing.T) {
	if _, ok := any(fake.NewTracker()).(core.RequestBudget); ok {
		t.Fatal("fake.Tracker answers core.RequestBudget without modelling one")
	}

	h := start(t, harnessOpts{issues: []core.Issue{fake.Issue("1", epoch)}})
	h.WaitState("1", StateDone)
	h.Tick()
	if got := h.o.Transitions.Path("1"); len(got) == 0 {
		t.Error("the tick did no work at all")
	}
}
