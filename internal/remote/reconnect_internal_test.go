package remote

import (
	"testing"
	"time"
)

// The reconnect policy's arithmetic, asserted directly because every test that
// drives it waits it out rather than reading it. The shape is what a reconnect
// is — doubling, capped, budgeted — and the magnitudes are what make the
// default survive an API rollout rather than only a lost packet (#275).
func TestReconnectPolicyDefaults(t *testing.T) {
	t.Parallel()
	want := DefaultReconnectPolicy()
	for _, tc := range []struct {
		name string
		in   ReconnectPolicy
		want ReconnectPolicy
	}{
		{name: "the zero policy is the default", in: ReconnectPolicy{}, want: want},
		{
			// Non-positive is unset, not "off": a zero wait would spin a failing
			// read as fast as the backend can refuse it, and a zero budget would
			// report every dropped connection as a sustained outage.
			name: "non-positive fields are unset",
			in:   ReconnectPolicy{Initial: -time.Second, Max: 0, Budget: -1},
			want: want,
		},
		{
			// A ceiling below the first wait would make the backoff shrink with
			// every retry. The first wait wins, so the policy still bounds.
			name: "an inverted ceiling is raised to the first wait",
			in:   ReconnectPolicy{Initial: 4 * time.Second, Max: time.Second, Budget: time.Minute},
			want: ReconnectPolicy{Initial: 4 * time.Second, Max: 4 * time.Second, Budget: time.Minute},
		},
		{
			name: "an explicit policy is preserved",
			in:   ReconnectPolicy{Initial: time.Second, Max: 8 * time.Second, Budget: time.Minute},
			want: ReconnectPolicy{Initial: time.Second, Max: 8 * time.Second, Budget: time.Minute},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.in.withDefaults(); got != tc.want {
				t.Errorf("withDefaults() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestReconnectDelayDoublesToTheCeiling(t *testing.T) {
	t.Parallel()
	p := ReconnectPolicy{Initial: time.Second, Max: 4 * time.Second, Budget: time.Minute}
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{n: 1, want: time.Second},
		{n: 2, want: 2 * time.Second},
		{n: 3, want: 4 * time.Second},
		{n: 4, want: 4 * time.Second},
		// The ceiling holds however long the outage lasts. A backoff that went
		// on doubling would take hours to notice a backend that came back.
		{n: 1000, want: 4 * time.Second},
	} {
		if got := p.delay(tc.n); got != tc.want {
			t.Errorf("delay(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}

	// A ceiling that is not a power-of-two multiple of the first wait is still
	// a ceiling rather than a value the doubling steps over.
	odd := ReconnectPolicy{Initial: 500 * time.Millisecond, Max: 3 * time.Second, Budget: time.Minute}
	if got := odd.delay(4); got != 3*time.Second {
		t.Errorf("delay(4) under an uneven ceiling = %v, want %v", got, 3*time.Second)
	}
}

// The budget is accumulated backoff, so a held attempt escalates after a fixed
// amount of waiting rather than after a fixed number of failures — which is what
// keeps it a property of the policy's own arithmetic and free of a clock seam.
func TestReconnectBudgetIsSpentInBackoff(t *testing.T) {
	t.Parallel()
	p := ReconnectPolicy{Initial: time.Second, Max: 4 * time.Second, Budget: 10 * time.Second}
	var outage reconnectState
	spent := []time.Duration{}
	for outage.spent < p.Budget {
		outage.attempts++
		outage.spent += p.delay(outage.attempts)
		spent = append(spent, outage.spent)
	}
	// 1 + 2 + 4 + 4 = 11s over four retries, so the fifth is the first held one.
	if outage.attempts != 4 {
		t.Errorf("the budget was spent after %d retries (%v), want 4", outage.attempts, spent)
	}
	outage.reset()
	if outage != (reconnectState{}) {
		t.Errorf("reset left %+v; a successful read starts the next outage from zero", outage)
	}
}
