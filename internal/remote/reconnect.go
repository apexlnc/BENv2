package remote

import "time"

// The default reconnect window.
//
// Sized against Airlock's `follow` semantics — the event log is cursor-addressed
// and retained, so a failed read costs nothing but the wait — and against how
// long the read path is actually gone in practice: the #195 restart drill lost
// it for 63 seconds while the run itself never noticed.
const (
	DefaultReconnectInitial = 500 * time.Millisecond
	DefaultReconnectMax     = 15 * time.Second
	DefaultReconnectBudget  = 5 * time.Minute
)

// ReconnectPolicy bounds how an Attempt waits out a backend whose event reads
// are failing.
//
// It bounds the *waiting*, never the attempt. A read that failed is not evidence
// about the process (SPEC §3.5, §9.8), so there is no number of failures after
// which BEN may conclude anything about the run — which makes Budget the point
// where a reconnect stops being a blip and becomes an outage worth an operator's
// attention, not a deadline at which an outcome is synthesized. Past it the
// attempt is *held*: the stream stays open, nothing is committed, and the
// orchestrator goes on retaining the claim until the run's own termination can
// be observed.
type ReconnectPolicy struct {
	// Initial is the wait after the first failed read.
	Initial time.Duration
	// Max caps one wait, so a long outage is polled at a steady rate rather
	// than at an exponentially receding one — the backend coming back has to be
	// noticed in bounded time.
	Max time.Duration
	// Budget is how much accumulated waiting is still an ordinary reconnect.
	Budget time.Duration
}

func DefaultReconnectPolicy() ReconnectPolicy {
	return ReconnectPolicy{
		Initial: DefaultReconnectInitial,
		Max:     DefaultReconnectMax,
		Budget:  DefaultReconnectBudget,
	}
}

// withDefaults fills the unset fields. Non-positive is unset: a zero wait would
// spin a failing read as fast as the backend can refuse it, and a zero budget
// would report every dropped connection as a sustained outage.
func (p ReconnectPolicy) withDefaults() ReconnectPolicy {
	d := DefaultReconnectPolicy()
	if p.Initial <= 0 {
		p.Initial = d.Initial
	}
	if p.Max <= 0 {
		p.Max = d.Max
	}
	if p.Max < p.Initial {
		p.Max = p.Initial
	}
	if p.Budget <= 0 {
		p.Budget = d.Budget
	}
	return p
}

// delay is the wait before retry n, counting from 1: doubling from Initial,
// capped at Max.
func (p ReconnectPolicy) delay(n int) time.Duration {
	d := p.Initial
	for i := 1; i < n && d < p.Max; i++ {
		d *= 2
	}
	if d > p.Max {
		return p.Max
	}
	return d
}

// reconnectState is one outage's progress through the policy. Accumulated
// backoff rather than wall-clock elapsed time, so the budget is a property of
// the policy's own arithmetic and needs no clock seam to be deterministic.
type reconnectState struct {
	attempts int
	spent    time.Duration
}

func (s *reconnectState) reset() { *s = reconnectState{} }
