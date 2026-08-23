package orchestrator

import (
	"testing"
	"time"
)

// WaitLaunch's decision layer, tested apart from the I/O that drives it and
// from the one fixture that calls it (#139).
//
// None of it can fail loudly on its own. A wrong abort set, a wrong reading of
// the path, or a budget that does not move with `-timeout` all still let the
// barrier pass whenever the launch happens; they show up only as a failure
// message that sends a reader after the wrong bug, which is exactly what the
// fixed 10s budget this replaced did.

// TestNotLaunchedNamesEveryExitFromPreparing anchors WaitLaunch's abort set
// against SPEC §9.2 read from the other side.
//
// notLaunched is derived from `legalTransitions`, so a test that walked the
// same map would agree with it by construction and could not see the set go
// wrong. The wanted set is therefore written out from §9.2's prose — `preparing`
// leaves for running, backoff, needs-review, or failed — and the states it is
// checked against are enumerated here rather than taken from any table the
// derivation reads.
func TestNotLaunchedNamesEveryExitFromPreparing(t *testing.T) {
	// The nine states of §9.2, listed independently of state.go's own tables.
	all := []State{
		StateQueued, StateClaimed, StatePreparing, StateRunning,
		StateVerifying, StateBackoff, StateDone, StateNeedsReview, StateFailed,
	}
	want := map[State]bool{StateBackoff: true, StateNeedsReview: true, StateFailed: true}

	for _, s := range all {
		if got := notLaunched[s]; got != want[s] {
			t.Errorf("notLaunched[%q] = %v, want %v", s, got, want[s])
		}
	}
	if len(notLaunched) != len(want) {
		t.Errorf("notLaunched has %d entries, want %d: %v", len(notLaunched), len(want), notLaunched)
	}

	// The two properties the barrier actually rests on, stated as properties
	// rather than as membership: an abort state must be somewhere `preparing`
	// can legally go — otherwise the barrier aborts on a state the loop cannot
	// be in — and the launch itself must never be one, or a successful launch
	// would be read as a refusal.
	for s := range notLaunched {
		if !Allowed(StatePreparing, s) {
			t.Errorf("notLaunched names %q, which §9.2 does not let preparing reach", s)
		}
	}
	if notLaunched[StateRunning] {
		t.Error("notLaunched names the launch itself")
	}

	// And the completeness half: every §9.2 exit from preparing other than the
	// launch is in the set. A missing one is the silent failure — the barrier
	// waits out its whole budget on a record that has already settled, and
	// reports a slow machine.
	for _, s := range all {
		if s == StateRunning || !Allowed(StatePreparing, s) {
			continue
		}
		if !notLaunched[s] {
			t.Errorf("§9.2 lets preparing reach %q, which is not a launch and not in notLaunched", s)
		}
	}
}

// TestBudgetUntilTracksTheBinaryDeadline pins the property that makes the
// budget worth deriving at all: it moves with `-timeout`. A budget that
// answered the same duration whatever the deadline would be the constant
// #139 removed, wearing a function's clothes.
func TestBudgetUntilTracksTheBinaryDeadline(t *testing.T) {
	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)

	for _, tc := range []struct {
		name        string
		deadline    time.Time
		set         bool
		want        time.Duration
		wantBounded bool
	}{
		{
			name:        "the default -timeout, less the margin",
			deadline:    now.Add(10 * time.Minute),
			set:         true,
			want:        10*time.Minute - deadlineMargin,
			wantBounded: true,
		},
		{
			// The property, not the arithmetic: a slower machine is given a
			// longer -timeout and the barrier widens with it.
			name:        "a longer -timeout buys a longer barrier",
			deadline:    now.Add(30 * time.Minute),
			set:         true,
			want:        30*time.Minute - deadlineMargin,
			wantBounded: true,
		},
		{
			name:        "inside the margin, split the remaining time",
			deadline:    now.Add(20 * time.Second),
			set:         true,
			want:        10 * time.Second,
			wantBounded: true,
		},
		{
			// The review reproduction used `-timeout=25s`: a two-second cap
			// failed real Git while roughly 22 seconds remained (#172).
			name:        "the review reproduction keeps a proportional real Git budget",
			deadline:    now.Add(25 * time.Second),
			set:         true,
			want:        25 * time.Second / 2,
			wantBounded: true,
		},
		{
			name:        "less than the fallback remains",
			deadline:    now.Add(time.Second),
			set:         true,
			want:        500 * time.Millisecond,
			wantBounded: true,
		},
		{
			name:        "at the deadline, fail immediately",
			deadline:    now,
			set:         true,
			wantBounded: true,
		},
		{
			name:        "past the deadline entirely",
			deadline:    now.Add(-time.Minute),
			set:         true,
			wantBounded: true,
		},
		{
			// `-timeout 0`. There is nothing to derive from, and the separate
			// bounded verdict keeps this zero from meaning "fail now".
			name: "-timeout 0 is no bound",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, bounded := budgetUntil(tc.deadline, tc.set, now)
			if got != tc.want {
				t.Errorf("budgetUntil = %v, want %v", got, tc.want)
			}
			if bounded != tc.wantBounded {
				t.Errorf("bounded = %v, want %v", bounded, tc.wantBounded)
			}
			remaining := tc.deadline.Sub(now)
			if tc.set && remaining > 0 && got >= remaining {
				t.Errorf("budgetUntil = %v with only %v remaining; the binary deadline can fire first",
					got, remaining)
			}
		})
	}
}

// TestReadLaunchTellsRefusedFromNotYet is the distinction the whole barrier
// exists for (#139): a path that has settled somewhere other than the launch is
// a regression and must be reported as one, and a path that is simply not there
// yet is a slow machine and must not.
//
// The two are told apart by position, not by membership, which is the case a
// set-of-states check would get wrong: `failed` after `running` is a run that
// ended, and reporting it as a launch that never came would send a reader after
// a bug that is not there.
func TestReadLaunchTellsRefusedFromNotYet(t *testing.T) {
	for _, tc := range []struct {
		name string
		path []State
		want launchVerdict
	}{
		{
			name: "nothing recorded yet",
			path: nil,
		},
		{
			// The exact path #139's flake reported. It is not a refusal: the
			// loop was doing the right thing, slowly.
			name: "still preparing",
			path: []State{StateQueued, StateClaimed, StatePreparing},
		},
		{
			name: "launched",
			path: []State{StateQueued, StateClaimed, StatePreparing, StateRunning},
			want: launchVerdict{launched: true},
		},
		{
			name: "prep failed retryably",
			path: []State{StateQueued, StateClaimed, StatePreparing, StateBackoff},
			want: launchVerdict{refused: StateBackoff},
		},
		{
			name: "prep failed for good",
			path: []State{StateQueued, StateClaimed, StatePreparing, StateFailed},
			want: launchVerdict{refused: StateFailed},
		},
		{
			name: "prep parked for a credential repair",
			path: []State{StateQueued, StateClaimed, StatePreparing, StateNeedsReview},
			want: launchVerdict{refused: StateNeedsReview},
		},
		{
			// Position, not membership: this one launched.
			name: "failed after running",
			path: []State{StateQueued, StateClaimed, StatePreparing, StateRunning, StateFailed},
			want: launchVerdict{launched: true},
		},
		{
			// Backoff first, so backoff is the answer even though a later
			// launch is on the path. The barrier's caller drives a manual
			// clock: nothing there fires a backoff, so a record that reaches
			// one has stopped, and the earliest decisive state is the honest
			// report of why.
			name: "backoff before a launch",
			path: []State{StateQueued, StateClaimed, StatePreparing, StateBackoff, StatePreparing, StateRunning},
			want: launchVerdict{refused: StateBackoff},
		},
		{
			name: "ran to completion",
			path: []State{StateQueued, StateClaimed, StatePreparing, StateRunning, StateVerifying, StateDone},
			want: launchVerdict{launched: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readLaunch(tc.path); got != tc.want {
				t.Errorf("readLaunch(%v) = %+v, want %+v", tc.path, got, tc.want)
			}
		})
	}
}
