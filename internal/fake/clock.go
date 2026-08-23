package fake

import (
	"sync"
	"time"
)

// Clock is a manually advanced clock. Backoff delays are measured in tens of
// seconds and attempt timeouts in hours, so tests that waited them out would
// not be tests. Advance moves time; every timer whose deadline has passed
// fires.
type Clock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
	// waiterAdded is closed and replaced whenever a waiter registers, so
	// BlockUntilWaiters can wake without polling.
	waiterAdded chan struct{}
}

type waiter struct {
	at time.Time
	ch chan time.Time
}

func NewClock(start time.Time) *Clock {
	return &Clock{now: start, waiterAdded: make(chan struct{})}
}

func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *Clock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &waiter{at: c.now.Add(d), ch: make(chan time.Time, 1)}
	if d <= 0 {
		w.ch <- c.now
		return w.ch
	}
	c.waiters = append(c.waiters, w)
	close(c.waiterAdded)
	c.waiterAdded = make(chan struct{})
	return w.ch
}

// Advance moves time forward and fires every timer that has come due.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var due, pending []*waiter
	for _, w := range c.waiters {
		if !w.at.After(now) {
			due = append(due, w)
		} else {
			pending = append(pending, w)
		}
	}
	c.waiters = pending
	c.mu.Unlock()

	for _, w := range due {
		w.ch <- now
	}
}

// Waiters reports how many timers are outstanding.
func (c *Clock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}

// Waits reports how long each outstanding timer still has to run. It answers
// what Advance cannot: *how long* the code under test asked to wait. A test
// that could only advance the clock has to guess an amount, and a wrong guess
// reads as "the delay was honoured" either way — so a delay that must shorten
// when a limit does is asserted here rather than choreographed.
func (c *Clock) Waits() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Duration, 0, len(c.waiters))
	for _, w := range c.waiters {
		out = append(out, w.at.Sub(c.now))
	}
	return out
}

// BlockUntilWaiters waits until at least n timers are outstanding, so a test
// can advance time without racing the code that arms the timer. It gives up
// after a generous deadline rather than hanging a test run.
func (c *Clock) BlockUntilWaiters(n int) bool {
	deadline := time.After(2 * time.Second)
	for {
		c.mu.Lock()
		if len(c.waiters) >= n {
			c.mu.Unlock()
			return true
		}
		added := c.waiterAdded
		c.mu.Unlock()

		select {
		case <-added:
		case <-deadline:
			return false
		}
	}
}
