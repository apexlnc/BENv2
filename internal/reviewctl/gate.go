package reviewctl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// This file is the forge client's rate discipline (#239). The reference for
// its shape is internal/tracker/github's request budget; it is a
// reimplementation of that discipline rather than a shared package because
// what the tracker meters — conditional probes, claim-stage reservations,
// content windows — is coupled to machinery this client deliberately does not
// have (see the client comment in forge.go). What is mirrored is the rule:
// reserve before the network, honour a declared backoff, and bound what one
// sweep may spend.

// ErrRateLimited refuses the network while a backoff the forge declared is
// still standing. It marks both the response that declared the backoff and
// every refusal made under it, so a sweep can stop at the first one rather
// than iterate through candidates that cannot succeed (SPEC §8.5's posture:
// after exhaustion, continued requests are what GitHub warns can get an
// integration banned).
var ErrRateLimited = errors.New("reviewctl: the forge declared a rate-limit backoff")

// ErrSweepBudget refuses requests past the per-sweep bound. A sweep that
// reaches it stops loudly and carries its interrupted candidate into the next
// pass. A bounded set can still be large; resetting the allowance without that
// cursor would spend every tick on the same prefix and starve the tail (#239).
var ErrSweepBudget = errors.New("reviewctl: the sweep's request budget is spent")

const (
	// paceRequests per paceWindow caps sustained spend at 4,800 requests an
	// hour — under GitHub's 5,000 primary allowance, the same ceiling
	// internal/tracker/github's billedPerTick keeps. The window is fixed
	// rather than rolling, so one boundary may pass a 2× burst; the hourly
	// ceiling holds either way, and fixed windows keep the arithmetic
	// checkable by eye.
	paceRequests = 40
	paceWindow   = 30 * time.Second

	// sweepRequestBudget bounds one sweep outright. Controller.Sweep resumes
	// an interrupted pass at the refused candidate, so this cap bounds one
	// interval's spend without making the candidate order a starvation order.
	sweepRequestBudget = 500

	// A secondary-limit response is allowed to carry neither Retry-After nor
	// a spent primary window. GitHub's documented answer is then at least one
	// minute before another request, rather than treating the 403/429 as an
	// ordinary failure and walking the rest of the candidate set.
	minimumBackoff = time.Minute
)

// gate is the reservation point every request passes before the network.
// Counting completed responses instead would let concurrent requests all
// observe the same spare slot and overspend it.
type gate struct {
	now   func() time.Time
	sleep func(context.Context, time.Duration) error

	mu           sync.Mutex
	windowStart  time.Time
	usedInWindow int
	blockedUntil time.Time
	sweepUsed    int
	sweepBudget  int

	// secondaryFailures is consecutive per forge operation across allowed
	// attempts, not sweeps. Only that operation's successful final response
	// clears it; unrelated successes cannot turn a continuing limit back into
	// attempt one.
	secondaryFailures map[requestClass]uint64
}

func newGate(now func() time.Time) *gate {
	return &gate{
		now: now, sleep: sleepContext, sweepBudget: sweepRequestBudget,
		secondaryFailures: map[requestClass]uint64{},
	}
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// beginSweep re-opens the per-sweep budget. Candidates calls it for ordinary
// passes; Controller calls through the client for a discovery-free dedicated
// retry. A process that only ever reconciles one issue (the operator command)
// spends from one process-lifetime budget instead, which one issue cannot
// approach.
func (g *gate) beginSweep() {
	g.mu.Lock()
	g.sweepUsed = 0
	g.mu.Unlock()
}

func (g *gate) sweepUsage() (used, budget int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sweepUsed, g.sweepBudget
}

// acquire reserves one request or says why it must not be sent. A standing
// backoff and a spent budget refuse immediately; an exhausted pace window
// waits it out, because that wait is bounded by the window itself.
func (g *gate) acquire(ctx context.Context) error {
	for {
		g.mu.Lock()
		now := g.now()
		if now.Before(g.blockedUntil) {
			until := g.blockedUntil
			g.mu.Unlock()
			return fmt.Errorf("%w (until %s)", ErrRateLimited, until.UTC().Format(time.RFC3339))
		}
		if g.sweepUsed >= g.sweepBudget {
			budget := g.sweepBudget
			g.mu.Unlock()
			return fmt.Errorf("%w (%d requests)", ErrSweepBudget, budget)
		}
		if now.Sub(g.windowStart) >= paceWindow {
			g.windowStart, g.usedInWindow = now, 0
		}
		if g.usedInWindow < paceRequests {
			g.usedInWindow++
			g.sweepUsed++
			g.mu.Unlock()
			return nil
		}
		wait := g.windowStart.Add(paceWindow).Sub(now)
		g.mu.Unlock()
		if err := g.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// observe reads every documented rate-limit signal and closes the gate when
// one stands. Its return value is narrower: whether this response itself is a
// rate-limit refusal. A successful (or ordinary-error) response may consume
// the final primary slot and must close the gate for the *next* request without
// changing the answer the caller already received.
//
// A plain 403 remains a permission answer. Retry-After or GitHub's error
// message/documentation URL distinguishes a secondary 403; a 429 names the
// condition by status alone, and a spent primary window is tracked separately.
func (g *gate) observe(class requestClass, status int, h http.Header, body []byte) bool {
	now := g.now()
	retryAfter := strings.TrimSpace(h.Get("Retry-After")) != ""
	exhausted := rateRemainingExhausted(h)
	secondaryNamed := responseNamesSecondaryLimit(body)
	secondaryLimited := status == http.StatusTooManyRequests ||
		(status == http.StatusForbidden && (retryAfter || secondaryNamed))
	responseLimited := status == http.StatusTooManyRequests ||
		(status == http.StatusForbidden && (retryAfter || exhausted || secondaryNamed))

	until, ok := retryAfterUntil(h, now)
	if !ok && exhausted {
		until, ok = rateResetUntil(h)
	}
	g.mu.Lock()
	if !responseLimited && !exhausted {
		g.mu.Unlock()
		return false
	}
	if secondaryLimited {
		if g.secondaryFailures == nil {
			g.secondaryFailures = map[requestClass]uint64{}
		}
		failures := g.secondaryFailures[class]
		if failures < ^uint64(0) {
			failures++
			g.secondaryFailures[class] = failures
		}
	}
	if !ok || !until.After(now) {
		if secondaryLimited {
			until = secondaryBackoffUntil(now, g.secondaryFailures[class])
		} else {
			until = now.Add(minimumBackoff)
		}
	}
	if until.After(g.blockedUntil) {
		g.blockedUntil = until
	}
	g.mu.Unlock()
	return responseLimited
}

func (g *gate) succeed(class requestClass) {
	g.mu.Lock()
	delete(g.secondaryFailures, class)
	g.mu.Unlock()
}

// The client does not retry internally: every secondary-limit response is
// returned as ErrRateLimited. This is the delay before the scheduled sweep may
// make the next attempt, doubled for each failure GitHub continues to return.
func secondaryBackoffUntil(now time.Time, failures uint64) time.Time {
	seconds := uint64(minimumBackoff / time.Second)
	if failures <= 1 {
		return addRetryAfter(now, seconds)
	}
	shift := failures - 1
	if shift >= 64 || seconds > ^uint64(0)>>shift {
		return latestBackoff
	}
	return addRetryAfter(now, seconds<<shift)
}

func rateRemainingExhausted(h http.Header) bool {
	v := strings.TrimSpace(h.Get("X-Ratelimit-Remaining"))
	remaining, err := strconv.Atoi(v)
	return v != "" && err == nil && remaining == 0
}

func retryAfterUntil(h http.Header, now time.Time) (time.Time, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return time.Time{}, false
	}
	if secs, ok := decimalSeconds(v); ok {
		return addRetryAfter(now, secs), true
	}
	at, err := http.ParseTime(v)
	return at, err == nil
}

// decimalSeconds parses an HTTP seconds field without letting an oversized
// value wrap an integer or duration. Saturation is fail-closed: a decimal too
// large for this process to represent remains a far-future refusal rather than
// collapsing to the one-minute fallback.
func decimalSeconds(v string) (uint64, bool) {
	if v == "" {
		return 0, false
	}
	var out uint64
	for i := range len(v) {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
		digit := uint64(v[i] - '0')
		if out > (^uint64(0)-digit)/10 {
			return ^uint64(0), true
		}
		out = out*10 + digit
	}
	return out, true
}

// latestBackoff is the last instant the RFC3339 diagnostic in acquire can
// represent. Saturating here still refuses for the process's useful lifetime.
var latestBackoff = time.Date(9999, 12, 31, 23, 59, 59, int(time.Second-1), time.UTC)

func addRetryAfter(now time.Time, seconds uint64) time.Time {
	const maxDurationSeconds = uint64((1<<63 - 1) / int64(time.Second))
	if seconds <= maxDurationSeconds {
		until := now.Add(time.Duration(seconds) * time.Second)
		if until.After(latestBackoff) {
			return latestBackoff
		}
		return until
	}
	unix := now.Unix()
	latest := latestBackoff.Unix()
	if seconds > uint64(latest) || unix >= latest || unix >= 0 && seconds > uint64(latest-unix) {
		return latestBackoff
	}
	return time.Unix(unix+int64(seconds), int64(now.Nanosecond()))
}

func rateResetUntil(h http.Header) (time.Time, bool) {
	epoch, ok := decimalSeconds(strings.TrimSpace(h.Get("X-Ratelimit-Reset")))
	if !ok || epoch == 0 {
		return time.Time{}, false
	}
	if epoch >= uint64(latestBackoff.Unix()) {
		return latestBackoff, true
	}
	return time.Unix(int64(epoch), 0), true
}

// responseNamesSecondaryLimit recognizes the two response fields GitHub uses
// without importing its client into reviewctl. Searching the bounded raw JSON
// also covers a proxy that returns the same message as plain text.
func responseNamesSecondaryLimit(body []byte) bool {
	said := strings.ToLower(string(body))
	for _, phrase := range []string{
		"secondary rate limit", "secondary-rate-limits", "abuse-rate-limits", "abuse detection",
	} {
		if strings.Contains(said, phrase) {
			return true
		}
	}
	return false
}
