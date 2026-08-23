package github

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// SPEC §8.5: honor both Retry-After and X-RateLimit-Reset. The wait the
// orchestrator gets back is the one the server asked for.
func TestFetchSurfacesRateLimitWait(t *testing.T) {
	tests := []struct {
		name          string
		limit         limitResponse
		wantSecondary bool
		wantAtLeast   time.Duration
		wantAtMost    time.Duration
	}{
		{
			name:        "primary limit with Retry-After",
			limit:       limitResponse{retryAfterSeconds: 45, reset: time.Now().Add(time.Hour)},
			wantAtLeast: 45 * time.Second,
			wantAtMost:  45 * time.Second,
		},
		{
			name:        "primary limit falls back to X-RateLimit-Reset",
			limit:       limitResponse{reset: time.Now().Add(90 * time.Second)},
			wantAtLeast: 80 * time.Second,
			wantAtMost:  90 * time.Second,
		},
		{
			name:          "secondary limit with Retry-After",
			limit:         limitResponse{secondary: true, retryAfterSeconds: 20},
			wantSecondary: true,
			wantAtLeast:   20 * time.Second,
			wantAtMost:    20 * time.Second,
		},
		{
			// A reset already in the past must not become a busy loop.
			name:        "expired reset is clamped",
			limit:       limitResponse{reset: time.Now().Add(-time.Minute)},
			wantAtLeast: minRetryAfter,
			wantAtMost:  minRetryAfter,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.serveIssues(issueFixture(1, "ben-queue"))
			f.rateLimitAll(tt.limit)

			_, err := f.adapter(t).Fetch(context.Background())
			var rl *RateLimitError
			if !errors.As(err, &rl) {
				t.Fatalf("Fetch error = %v, want *RateLimitError", err)
			}
			if rl.Secondary != tt.wantSecondary {
				t.Errorf("Secondary = %v, want %v", rl.Secondary, tt.wantSecondary)
			}
			if rl.RetryAfter < tt.wantAtLeast || rl.RetryAfter > tt.wantAtMost {
				t.Errorf("RetryAfter = %v, want within [%v, %v]", rl.RetryAfter, tt.wantAtLeast, tt.wantAtMost)
			}
		})
	}
}

// Rediscovering a limit costs the budget we were just told we do not have.
func TestRateLimitClosesTheGateWithoutSpendingRequests(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveIssues(issueFixture(1, "ben-queue"))
	f.rateLimitAll(limitResponse{retryAfterSeconds: 60})
	adapter := f.adapter(t)

	if _, err := adapter.Fetch(context.Background()); err == nil {
		t.Fatal("Fetch should have reported the rate limit")
	}
	f.reset()

	for _, call := range []struct {
		name string
		run  func() error
	}{
		{"Fetch", func() error { _, err := adapter.Fetch(context.Background()); return err }},
		{"Claim", func() error { _, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"}); return err }},
		{"Release", func() error { return adapter.Release(context.Background(), core.Issue{Identifier: "1"}) }},
		{"SetStateLabels", func() error {
			return adapter.SetStateLabels(context.Background(), core.Issue{Identifier: "1"}, core.StateLabelClaimed)
		}},
		{"Comment", func() error {
			return adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, core.MilestoneComment{Milestone: core.MilestoneNeedsReview})
		}},
		{"FindPR", func() error {
			_, err := adapter.FindPR(context.Background(), core.Issue{Identifier: "1"}, "ben/x")
			return err
		}},
		{"Get", func() error { _, err := adapter.Get(context.Background(), "1"); return err }},
		// Both recovery reads resolve the claim principal first, which is
		// itself a request: gating only the work that follows would spend one
		// on every retry while the window is open.
		{"ClaimedByPrincipal", func() error { _, err := adapter.ClaimedByPrincipal(context.Background()); return err }},
		{"ClaimHistory", func() error {
			_, err := adapter.ClaimHistory(context.Background(), core.Issue{Identifier: "1"})
			return err
		}},
	} {
		var rl *RateLimitError
		if err := call.run(); !errors.As(err, &rl) {
			t.Errorf("%s error = %v, want *RateLimitError while the window is open", call.name, err)
		}
	}
	if reqs, _ := f.snapshot(); len(reqs) != 0 {
		t.Errorf("the closed gate still spent %d requests: %v", len(reqs), reqs)
	}
}

// An exhausted budget must not black out the polls that cost nothing: a
// conditional request answered 304 is free and still served. go-github's
// client-side preflight would refuse them all, which is why it is off.
func TestExhaustedBudgetStillAllowsFreeConditionalPolls(t *testing.T) {
	f := newFakeGitHub(t)
	var mu sync.Mutex
	page := []*gh.Issue{issueFixture(1, "ben-queue")}
	f.handle("GET /api/v3/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, r, page)
	})
	adapter := f.adapter(t)

	if _, err := adapter.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	// Spend the budget down to zero and serve one genuine 200 at that mark,
	// so the client observes Remaining: 0 the way it would on a busy hour or
	// a token shared with another tool.
	f.exhaustBudget()
	mu.Lock()
	page = append(page, issueFixture(2, "ben-queue")) // changed: revalidates to a real 200
	mu.Unlock()
	if _, err := adapter.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch at Remaining: 0: %v", err)
	}
	f.reset()

	issues, err := adapter.Fetch(context.Background())
	if err != nil {
		t.Fatalf("conditional Fetch at Remaining: 0 was refused locally: %v", err)
	}
	if len(issues) != 2 {
		t.Errorf("got %d issues, want the cached queue replayed", len(issues))
	}
	requests, billed := f.snapshot()
	if len(requests) == 0 {
		t.Fatal("no request left the client; the local preflight refused a free 304")
	}
	if billed != 0 {
		t.Errorf("billed %d requests, want the poll to have been free", billed)
	}
}

func TestRateGateReopensAfterTheWindow(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	advance := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}

	gate := newRateGate(clock)
	if err := gate.check(); err != nil {
		t.Fatalf("a fresh gate is open: %v", err)
	}

	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	resp.Header.Set("Retry-After", "30")
	observed := gate.observe(&gh.AbuseRateLimitError{Response: resp, RetryAfter: gh.Ptr(30 * time.Second)})
	var rl *RateLimitError
	if !errors.As(observed, &rl) {
		t.Fatalf("observe() = %v, want *RateLimitError", observed)
	}

	advance(29 * time.Second)
	if err := gate.check(); err == nil {
		t.Error("gate reopened one second early")
	}
	advance(2 * time.Second)
	if err := gate.check(); err != nil {
		t.Errorf("gate still closed past the window: %v", err)
	}
}

// The regression from #193, at the boundary it failed on: BEN published PR #196
// and then parked for 21 minutes because the tracker policy lacked
// `pull_requests: read` and the 403 saying so carried the ordinary rate headers.
// A permission failure is permanent and actionable — it must be stated as
// itself, and it must not shut the door on everything else.
func TestPermissionForbiddenIsNotARateLimit(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveIssues(issueFixture(1, "ben-queue"))
	f.handle("GET /api/v3/repos/{owner}/{repo}/pulls", func(w http.ResponseWriter, r *http.Request) {
		writeForbidden(w)
	})
	adapter := f.adapter(t)

	_, err := adapter.FindPR(context.Background(), core.Issue{Identifier: "1"}, "ben/issue-1-9f2a")
	var rl *RateLimitError
	if errors.As(err, &rl) {
		t.Fatalf("FindPR error = %v, want the permission failure stated as itself", err)
	}
	// go-github's typed error, not a status code this package re-read: the
	// message is the actionable half of the report.
	var errResp *gh.ErrorResponse
	if !errors.As(err, &errResp) || errResp.Response == nil {
		t.Fatalf("FindPR error = %v, want go-github's *ErrorResponse preserved", err)
	}
	if errResp.Response.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", errResp.Response.StatusCode)
	}
	if !strings.Contains(err.Error(), "Resource not accessible by integration") {
		t.Errorf("FindPR error = %v, want GitHub's own message in it", err)
	}

	// The fixture's point: this is a refusal carrying every rate header a
	// served response carries. Reading the reset alone as evidence is what
	// misclassified it.
	remaining, convErr := strconv.Atoi(errResp.Response.Header.Get(gh.HeaderRateRemaining))
	if convErr != nil || remaining <= 0 {
		t.Fatalf("X-RateLimit-Remaining = %q, want a positive budget on the fixture",
			errResp.Response.Header.Get(gh.HeaderRateRemaining))
	}
	epoch, convErr := strconv.ParseInt(errResp.Response.Header.Get(gh.HeaderRateReset), 10, 64)
	if convErr != nil || !time.Unix(epoch, 0).After(time.Now()) {
		t.Fatalf("X-RateLimit-Reset = %q, want a future reset on the fixture",
			errResp.Response.Header.Get(gh.HeaderRateReset))
	}

	// And the gate stayed open: the next unrelated read reaches the server
	// rather than being suppressed until that reset.
	f.reset()
	if _, err := adapter.Fetch(context.Background()); err != nil {
		t.Fatalf("Fetch after a permission failure: %v", err)
	}
	if reqs, _ := f.snapshot(); len(reqs) == 0 {
		t.Error("a permission failure closed the rate gate: no request left the client")
	}
}

// What makes an unclassified 403 or 429 a rate limit, stated at the classifier
// so each signal can be varied on its own. go-github resolves the two cases
// GitHub documents (a spent X-RateLimit-Remaining, the secondary
// documentation_url) into typed errors before this table is reached; everything
// here is what arrives as a bare *gh.ErrorResponse.
func TestClassifyUnnamedRefusals(t *testing.T) {
	now := time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC)
	const (
		positiveRemaining = "4999"
		spentRemaining    = "0"
	)
	reset := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }

	tests := []struct {
		name          string
		status        int
		message       string
		docURL        string
		header        map[string]string
		wantLimit     bool
		wantSecondary bool
		wantWait      time.Duration
	}{
		{
			name:    "a permission 403 carrying ordinary rate headers is not a limit",
			status:  http.StatusForbidden,
			message: "Resource not accessible by integration",
			docURL:  "https://docs.github.com/rest/pulls/pulls#list-pull-requests",
			header:  map[string]string{gh.HeaderRateRemaining: positiveRemaining, gh.HeaderRateReset: reset(time.Hour)},
		},
		{
			name:    "a permission 403 with no rate headers at all is not a limit",
			status:  http.StatusForbidden,
			message: "Resource not accessible by integration",
		},
		{
			name:          "a 403 the server asked us to retry after is a limit",
			status:        http.StatusForbidden,
			message:       "Resource not accessible by integration",
			header:        map[string]string{"Retry-After": "30", gh.HeaderRateRemaining: positiveRemaining, gh.HeaderRateReset: reset(time.Hour)},
			wantLimit:     true,
			wantSecondary: true,
			wantWait:      30 * time.Second,
		},
		{
			// The message is the only signal here: no Retry-After, budget
			// untouched, and a documentation_url go-github's suffix test misses.
			name:          "a 403 naming the secondary limit in its message is a limit",
			status:        http.StatusForbidden,
			message:       "You have exceeded a secondary rate limit. Please wait a few minutes before you try again.",
			header:        map[string]string{gh.HeaderRateRemaining: positiveRemaining, gh.HeaderRateReset: reset(90 * time.Second)},
			wantLimit:     true,
			wantSecondary: true,
			wantWait:      90 * time.Second,
		},
		{
			name:      "a 403 whose primary budget is spent surfaces the server's reset",
			status:    http.StatusForbidden,
			message:   "API rate limit exceeded",
			header:    map[string]string{gh.HeaderRateRemaining: spentRemaining, gh.HeaderRateReset: reset(15 * time.Minute)},
			wantLimit: true,
			wantWait:  15 * time.Minute,
		},
		{
			name:      "a spent budget whose reset has already passed is clamped, not skipped",
			status:    http.StatusForbidden,
			message:   "API rate limit exceeded",
			header:    map[string]string{gh.HeaderRateRemaining: spentRemaining, gh.HeaderRateReset: reset(-time.Minute)},
			wantLimit: true,
			wantWait:  minRetryAfter,
		},
		{
			// Both signals: the kind reported is the one the server named.
			name:          "a spent budget that also names the secondary limit stays secondary",
			status:        http.StatusForbidden,
			message:       "You have exceeded a secondary rate limit",
			header:        map[string]string{gh.HeaderRateRemaining: spentRemaining, gh.HeaderRateReset: reset(time.Minute)},
			wantLimit:     true,
			wantSecondary: true,
			wantWait:      time.Minute,
		},
		{
			// 429 names the condition in its status, so it needs no other
			// evidence and stays fail-closed.
			name:          "a 429 with only a reset is still a limit",
			status:        http.StatusTooManyRequests,
			message:       "Too many requests",
			header:        map[string]string{gh.HeaderRateRemaining: positiveRemaining, gh.HeaderRateReset: reset(2 * time.Minute)},
			wantLimit:     true,
			wantSecondary: true,
			wantWait:      2 * time.Minute,
		},
		{
			name:          "a 429 honors the wait the server provided",
			status:        http.StatusTooManyRequests,
			message:       "Too many requests",
			header:        map[string]string{"Retry-After": "60", gh.HeaderRateReset: reset(time.Hour)},
			wantLimit:     true,
			wantSecondary: true,
			wantWait:      time.Minute,
		},
		{
			name:          "a 429 with no headers falls back to the floor",
			status:        http.StatusTooManyRequests,
			message:       "Too many requests",
			wantLimit:     true,
			wantSecondary: true,
			wantWait:      minRetryAfter,
		},
		{
			name:    "another status is not a limit however its headers read",
			status:  http.StatusNotFound,
			message: "Not Found",
			header:  map[string]string{gh.HeaderRateRemaining: spentRemaining, gh.HeaderRateReset: reset(time.Hour)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tt.status, Header: http.Header{}}
			for k, v := range tt.header {
				resp.Header.Set(k, v)
			}
			err := &gh.ErrorResponse{Response: resp, Message: tt.message, DocumentationURL: tt.docURL}

			got := classify(err, now)
			if !tt.wantLimit {
				if got != nil {
					t.Fatalf("classify() = %v, want the error left alone", got)
				}
				return
			}
			if got == nil {
				t.Fatal("classify() = nil, want a *RateLimitError")
			}
			if got.Secondary != tt.wantSecondary {
				t.Errorf("Secondary = %v, want %v", got.Secondary, tt.wantSecondary)
			}
			if got.RetryAfter != tt.wantWait {
				t.Errorf("RetryAfter = %v, want %v", got.RetryAfter, tt.wantWait)
			}
			if !errors.Is(got, err) {
				t.Errorf("classify() dropped the underlying error: %v", got)
			}
		})
	}
}

// Everything that is not a rate limit must pass through untouched, or the
// orchestrator would back off on ordinary failures.
func TestObserveLeavesOtherErrorsAlone(t *testing.T) {
	gate := newRateGate(nil)
	plain := errors.New("connection reset")
	if got := gate.observe(plain); !errors.Is(got, plain) {
		t.Errorf("observe(%v) = %v, want the error unchanged", plain, got)
	}
	notFound := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}}}
	if got := gate.observe(notFound); !errors.Is(got, notFound) {
		t.Errorf("observe(404) = %v, want the error unchanged", got)
	}
	if err := gate.check(); err != nil {
		t.Errorf("ordinary errors must not close the gate: %v", err)
	}
	if gate.observe(nil) != nil {
		t.Error("observe(nil) must stay nil")
	}
}
