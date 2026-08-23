package github

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v90/github"
)

// rateGate turns a rate-limit response into a wait the caller can act on, and
// holds the door shut until that wait elapses (SPEC §8.5). Discovering the
// same limit once per call would spend the budget we are being told we no
// longer have.
type rateGate struct {
	now func() time.Time

	mu    sync.Mutex
	until time.Time
	last  *RateLimitError
}

func newRateGate(now func() time.Time) *rateGate {
	if now == nil {
		now = time.Now
	}
	return &rateGate{now: now}
}

// check reports the standing refusal, if the window has not yet passed.
func (g *rateGate) check() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if g.until.IsZero() || !now.Before(g.until) {
		return nil
	}
	return &RateLimitError{
		Secondary:  g.last.Secondary,
		RetryAfter: g.until.Sub(now),
		Err:        g.last.Err,
	}
}

// observe classifies err. A rate-limit error is recorded (closing the gate)
// and returned as a *RateLimitError; anything else passes through unchanged.
func (g *rateGate) observe(err error) error {
	if err == nil {
		return nil
	}
	rl := classify(err, g.now())
	if rl == nil {
		return err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if until := g.now().Add(rl.RetryAfter); until.After(g.until) {
		g.until, g.last = until, rl
	}
	return rl
}

// minRetryAfter keeps a limit already-reset-by-the-clock (skew, or a reset
// header in the past) from collapsing into a busy loop.
const minRetryAfter = time.Second

// classify maps go-github's rate-limit errors, plus the bare 403/429 it does
// not classify, onto the wait the server asked for. Retry-After wins over
// X-RateLimit-Reset where both are present, per SPEC §8.5.
func classify(err error, now time.Time) *RateLimitError {
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) {
		// go-github resolves Retry-After, falling back to X-RateLimit-Reset.
		// A synthetic GraphQL error did not pass through its CheckResponse, so
		// read the same headers here when that resolved wait is absent.
		wait := time.Duration(0)
		if abuse.RetryAfter != nil {
			wait = *abuse.RetryAfter
		} else if retryAfter, ok := retryAfterHeader(abuse.Response); ok {
			wait = retryAfter
		} else if reset, ok := resetHeader(abuse.Response, now); ok {
			wait = reset
		}
		return &RateLimitError{Secondary: true, RetryAfter: clampWait(wait), Err: err}
	}

	var primary *gh.RateLimitError
	if errors.As(err, &primary) {
		if wait, ok := retryAfterHeader(primary.Response); ok {
			return &RateLimitError{RetryAfter: clampWait(wait), Err: err}
		}
		return &RateLimitError{RetryAfter: clampWait(primary.Rate.Reset.Sub(now)), Err: err}
	}

	// A 403/429 go-github did not classify. It reaches here having matched
	// neither of the two signals GitHub documents, so what it is has to be read
	// off the response.
	var errResp *gh.ErrorResponse
	if errors.As(err, &errResp) && errResp.Response != nil {
		return classifyResponse(errResp, err, now)
	}
	return nil
}

// classifyResponse decides whether an unclassified 403/429 is a rate limit.
//
// **A reset timestamp is not evidence** (#198). Every GitHub response carries
// X-RateLimit-Reset, including `403 Resource not accessible by integration` —
// the permanent, actionable refusal of a permission the credential does not
// have. Reading a future reset as proof of a limit reported a missing scope as a
// wait and shut the gate on every unrelated tracker request until it elapsed.
//
// So a 403 must say so: the Retry-After GitHub sends when it wants a wait, prose
// naming the secondary limit, or a primary budget the headers show is spent.
// A 429 needs no such evidence — its status names the condition — and stays
// fail-closed on whatever wait the server did provide.
func classifyResponse(errResp *gh.ErrorResponse, err error, now time.Time) *RateLimitError {
	resp := errResp.Response
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusTooManyRequests:
	default:
		return nil
	}

	retryAfter, hasRetryAfter := retryAfterHeader(resp)
	secondary := hasRetryAfter || namesSecondaryLimit(errResp)
	exhausted := budgetExhausted(resp)
	if resp.StatusCode == http.StatusForbidden && !secondary && !exhausted {
		return nil
	}

	// Secondary unless the primary budget is what the headers say is gone: an
	// unnamed cause on a budget with room left is the conservative reading.
	limit := &RateLimitError{Secondary: secondary || !exhausted, Err: err}
	if hasRetryAfter {
		limit.RetryAfter = clampWait(retryAfter)
		return limit
	}
	// A reset the response did not state leaves the wait at zero, which clamps
	// to the floor rather than to none.
	wait, _ := resetHeader(resp, now)
	limit.RetryAfter = clampWait(wait)
	return limit
}

// namesSecondaryLimit reports whether the response says a secondary limit in
// either place GitHub says it. go-github matches the documentation_url by exact
// suffix and does not read the message at all, so both are re-read here: the
// message is the only signal on a response whose documentation_url is absent or
// carries a fragment go-github's suffix test misses.
func namesSecondaryLimit(errResp *gh.ErrorResponse) bool {
	said := strings.ToLower(errResp.DocumentationURL + " " + errResp.Message)
	for _, phrase := range []string{"secondary rate limit", "secondary-rate-limits", "abuse-rate-limits", "abuse detection"} {
		if strings.Contains(said, phrase) {
			return true
		}
	}
	return false
}

// budgetExhausted reports a primary allowance the server says is spent. An
// absent or unparsable header is not a claim that it is: only a number at or
// below zero is.
func budgetExhausted(resp *http.Response) bool {
	v := resp.Header.Get(gh.HeaderRateRemaining)
	if v == "" {
		return false
	}
	remaining, err := strconv.Atoi(v)
	return err == nil && remaining <= 0
}

func clampWait(d time.Duration) time.Duration {
	if d < minRetryAfter {
		return minRetryAfter
	}
	return d
}

func retryAfterHeader(resp *http.Response) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0, false
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(secs) * time.Second, true
}

func resetHeader(resp *http.Response, now time.Time) (time.Duration, bool) {
	if resp == nil {
		return 0, false
	}
	v := resp.Header.Get(gh.HeaderRateReset)
	if v == "" {
		return 0, false
	}
	epoch, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return time.Unix(epoch, 0).Sub(now), true
}
