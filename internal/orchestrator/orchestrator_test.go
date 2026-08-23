package orchestrator

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// The whole machine, once: a labeled issue goes claim → worktree → run →
// verified → done, and every side effect the spec asks for lands.
func TestHappyPathReachesDone(t *testing.T) {
	h := start(t, harnessOpts{issues: []core.Issue{fake.Issue("1", epoch)}})
	h.WaitState("1", StateDone)

	if got, want := h.o.Transitions.Path("1"),
		[]State{StateQueued, StateClaimed, StatePreparing, StateRunning, StateVerifying, StateDone}; !sameStates(got, want) {
		t.Errorf("path = %v, want %v", got, want)
	}

	// SPEC §9.3: done removes the state labels.
	h.WaitEffects(1)
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Errorf("final label = %q, want none", got)
	}
	// SPEC §8.4: exactly the claimed and published milestones, in order.
	if got := fmt.Sprint(h.Tracker.Milestones("1")); got != "[claimed published]" {
		t.Errorf("milestones = %s, want [claimed published]", got)
	}
	// The claim is retained at done (BUILD.md decision 7).
	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times at done; the claim is retained while the PR awaits review", n)
	}
	// Workspace disposed, not kept.
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want one non-keeping dispose", got)
	}
}

// SPEC §9.3: the projection is exactly the five-value table, and the
// transient states collapse onto ben:claimed rather than churning.
func TestLabelProjectionFollowsTheTable(t *testing.T) {
	tests := []struct {
		state State
		want  core.StateLabel
	}{
		{StateQueued, core.StateLabelNone},
		{StateClaimed, core.StateLabelClaimed},
		{StatePreparing, core.StateLabelClaimed},
		{StateVerifying, core.StateLabelClaimed},
		{StateBackoff, core.StateLabelClaimed},
		{StateRunning, core.StateLabelRunning},
		{StateNeedsReview, core.StateLabelNeedsReview},
		{StateFailed, core.StateLabelFailed},
		{StateDone, core.StateLabelNone},
	}
	for _, tt := range tests {
		if got := stateLabel(tt.state); got != tt.want {
			t.Errorf("stateLabel(%s) = %q, want %q", tt.state, got, tt.want)
		}
	}
}

// SPEC §9.5: oldest created_at first, identifier as tiebreak, and only while
// slots remain.
func TestDispatchIsFIFOAndCapped(t *testing.T) {
	h := start(t, harnessOpts{
		concurrency: "2",
		// Runs hang so the slots stay occupied.
		hang:   true,
		script: startedOnly,
		issues: []core.Issue{
			fake.Issue("30", epoch.Add(3*time.Hour)),
			fake.Issue("10", epoch.Add(time.Hour)),
			fake.Issue("20", epoch.Add(2*time.Hour)),
			// Same age as 10: the identifier breaks the tie, and "10" sorts
			// before "11".
			fake.Issue("11", epoch.Add(time.Hour)),
		},
	})
	h.Tick()
	h.WaitState("10", StateRunning)
	h.WaitState("11", StateRunning)

	if n := h.Runner.StartCount(); n != 2 {
		t.Fatalf("started %d runs with max_concurrent_agents=2, want 2", n)
	}
	started := map[string]bool{}
	for _, s := range h.o.Status() {
		if s.State == StateRunning {
			started[s.Identifier] = true
		}
	}
	if !started["10"] || !started["11"] {
		t.Errorf("running = %v, want the two oldest (10 then 11 by identifier tiebreak)", started)
	}
	if started["20"] || started["30"] {
		t.Error("a younger issue jumped the queue")
	}
}

// SPEC §9.4 step 2: a blocked preflight skips dispatch for the tick and
// nothing else — reconciliation always runs.
func TestBlockedPreflightSkipsDispatchNotReconciliation(t *testing.T) {
	// DispatchBlocked is called from the authority goroutine, so the test's
	// side of it has to be synchronized.
	var mu sync.Mutex
	block := error(errors.New("workflow reload failed"))
	setBlock := func(e error) { mu.Lock(); block = e; mu.Unlock() }

	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		blocked: func() error {
			mu.Lock()
			defer mu.Unlock()
			return block
		},
	})

	h.Tick()
	if n := h.Runner.StartCount(); n != 0 {
		t.Fatalf("dispatched %d runs while config validation was failing", n)
	}
	if len(h.o.Status()) != 0 {
		t.Fatalf("tracked something while blocked: %v", h.o.Status())
	}

	setBlock(nil)
	h.Tick()
	h.WaitState("1", StateDone)
}

// SPEC §9.6 failure track: a retryable verdict with attempts remaining goes
// to backoff and re-dispatches when the timer fires.
func TestRetryableFailureBacksOffThenRetries(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("session-2")
		},
	})

	// One waiter is the ticker; the backoff timer is the second.
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the backoff timer was never armed")
	}
	// Assert the detour through backoff from the log rather than the live
	// state: the state is transient by construction, and polling for it races
	// the timer that ends it.
	if got := h.o.Transitions.Path("1"); !containsState(got, StateBackoff) {
		t.Fatalf("path = %v, want it to pass through backoff", got)
	}

	h.Clock.Advance(11 * time.Second)
	h.WaitState("1", StateDone)

	if got := h.Workspaces.PrepareCount("1"); got != 2 {
		t.Errorf("prepared %d attempts, want 2", got)
	}
}

// SPEC §9.6: a non-retryable verdict fails immediately rather than spending
// the remaining attempts.
func TestNonRetryableFailureSkipsBackoff(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureAuth) },
	})

	h.WaitGone("1")

	path := h.o.Transitions.Path("1")
	if containsState(path, StateBackoff) {
		t.Errorf("path = %v, want no backoff for a non-retryable verdict", path)
	}
	if path[len(path)-1] != StateFailed {
		t.Errorf("path = %v, want it to end at failed", path)
	}
	h.WaitEffects(1)
	// SPEC §9.2: failed releases the claim; the label blocks re-dispatch.
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("released %d times, want 1 — failed releases the claim", n)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelFailed {
		t.Errorf("label = %q, want ben:failed to stand after release", got)
	}
	if got := fmt.Sprint(h.Tracker.Milestones("1")); got != "[claimed failed]" {
		t.Errorf("milestones = %s, want [claimed failed]", got)
	}
}

func containsState(path []State, want State) bool {
	for _, s := range path {
		if s == want {
			return true
		}
	}
	return false
}

func sameStates(a, b []State) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
