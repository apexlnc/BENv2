package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// manualClock is the time seam the budget's window and rolling allowance use.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 8, 6, 9, 0, 0, 0, time.UTC)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func spendBilled(t *testing.T, b *requestBudget, n int) {
	t.Helper()
	for i := range n {
		reservation, err := b.acquire(context.Background(), false, false)
		if err != nil {
			t.Fatalf("reserving billed request %d of %d: %v", i+1, n, err)
		}
		reservation.settle(true)
	}
}

// resetBudget puts a test adapter at a stated clock with a full rolling
// allowance. Production never does this; BeginTick deliberately resets only
// the burst window. Tests use the seam to remove fixture setup requests from
// the scenario they are measuring.
func resetBudget(b *requestBudget, now func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if now != nil {
		b.now = now
	}
	current := b.now()
	b.opened = current
	b.lastRefill = current
	b.generation++
	b.used = 0
	b.unattempted = 0
	b.credits = int64(b.limit) * requestCreditUnit
	b.heldCredits = 0
	b.borrowed = false
	b.secondaryCredits = int64(secondaryBurstPoints) * secondaryCreditUnit
	b.secondaryHeldCredits = 0
	b.secondaryLastRefill = current
	b.contentMinuteCredits = int64(contentMinuteBurstRequests) * contentMinuteCreditUnit
	b.contentMinuteHeldCredits = 0
	b.contentMinuteLastRefill = current
	b.contentHourCredits = int64(contentHourBurstRequests) * contentHourCreditUnit
	b.contentHourHeldCredits = 0
	b.contentHourLastRefill = current
	b.ceiling = defaultBudgetWindow
	b.report = core.RequestReport{}
	b.pending = 0
	b.reportEpoch++
	b.notifyLocked()
}

func leaveOrdinaryBudget(t *testing.T, a *Adapter, remaining int) {
	t.Helper()
	if remaining < 0 || remaining > ordinaryPerTick {
		t.Fatalf("invalid remaining ordinary budget %d", remaining)
	}
	resetBudget(a.budget, nil)
	spendBilled(t, a.budget, ordinaryPerTick-remaining)
}

// Admission happens where every HTTP request passes. This interface-driven
// list independently anchors that every public operation reaches the transport
// rather than bypassing the budget.
func TestSpentBudgetRefusesEveryCallWithoutSpendingRequests(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	f.serveIssues(issueFixture(1, "ben-queue"))
	newFakeIssue(1, "ben-queue").serve(f)
	adapter := f.adapter(t)

	if err := adapter.Ready(context.Background()); err != nil {
		t.Fatalf("establishing repository identity: %v", err)
	}
	leaveOrdinaryBudget(t, adapter, 0)
	f.reset()

	calls := []struct {
		name string
		run  func() error
	}{
		// RemotePR must run before the deliberately refused Ready below clears
		// the previously established repository identity.
		{"RemotePR", func() error {
			_, err := adapter.RemotePR(context.Background(), core.RemotePRQuery{
				Issue: core.Issue{Identifier: "1"}, Repository: testRepositoryIdentity, Branch: "ben/x",
			})
			return err
		}},
		{"Ready", func() error { return adapter.Ready(context.Background()) }},
		{"Fetch", func() error { _, err := adapter.Fetch(context.Background()); return err }},
		{"Get", func() error { _, err := adapter.Get(context.Background(), "1"); return err }},
		{"ClaimedByPrincipal", func() error { _, err := adapter.ClaimedByPrincipal(context.Background()); return err }},
		{"HeldClaims", func() error { _, err := adapter.HeldClaims(context.Background()); return err }},
		{"ClaimHistory", func() error {
			_, err := adapter.ClaimHistory(context.Background(), core.Issue{Identifier: "1"})
			return err
		}},
		{"FindPR", func() error {
			_, err := adapter.FindPR(context.Background(), core.Issue{Identifier: "1"}, "ben/x")
			return err
		}},
		{"ContentApproval", func() error {
			_, err := adapter.ContentApproval(context.Background(), core.Issue{Identifier: "1"})
			return err
		}},
		{"Claim", func() error { _, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"}); return err }},
		{"Release", func() error { return adapter.Release(context.Background(), core.Issue{Identifier: "1"}) }},
		{"SetStateLabels", func() error {
			return adapter.SetStateLabels(context.Background(), core.Issue{Identifier: "1"}, core.StateLabelRunning)
		}},
		{"Comment", func() error {
			return adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, core.MilestoneComment{Milestone: core.MilestoneClaimed})
		}},
	}
	for _, call := range calls {
		if err := call.run(); !errors.Is(err, ErrRequestBudget) {
			t.Errorf("%s error = %v, want ErrRequestBudget once ordinary slots are spent", call.name, err)
		}
	}
	if reqs, _ := f.snapshot(); len(reqs) != 0 {
		t.Errorf("a spent budget still made %d requests: %v", len(reqs), reqs)
	}
	if spent := adapter.BeginTick(defaultBudgetWindow); spent.Refused != len(calls) {
		t.Errorf("report = %+v, want %d refusals — one per refused call", spent, len(calls))
	}
}

// Claim reserves assignment, read-back, and possible unwind before writing.
// With fewer than three slots it changes nothing; with exactly three, even a
// failed read-back can still remove the unverifiable assignment (SPEC §8.4).
func TestClaimReservesItsMustFinishSequenceBeforeWriting(t *testing.T) {
	t.Run("insufficient", func(t *testing.T) {
		f := newFakeGitHub(t)
		issue := newFakeIssue(1, "ben-queue")
		issue.serve(f)
		adapter := f.adapter(t)
		if _, err := adapter.claimPrincipal(context.Background()); err != nil {
			t.Fatal(err)
		}
		leaveOrdinaryBudget(t, adapter, 2)
		f.reset()

		claimed, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"})
		if claimed || !errors.Is(err, ErrRequestBudget) {
			t.Fatalf("Claim = (%v, %v), want false, ErrRequestBudget", claimed, err)
		}
		if reqs, _ := f.snapshot(); len(reqs) != 0 {
			t.Errorf("Claim made %d requests before refusing: %v", len(reqs), reqs)
		}
		if got := issue.currentAssignees(); len(got) != 0 {
			t.Errorf("assignees = %v, want no write", got)
		}
	})

	t.Run("exactly enough to unwind", func(t *testing.T) {
		f := newFakeGitHub(t)
		issue := newFakeIssue(1, "ben-queue")
		issue.serve(f)
		adapter := f.adapter(t)
		if _, err := adapter.claimPrincipal(context.Background()); err != nil {
			t.Fatal(err)
		}
		issue.failReadBack = true
		leaveOrdinaryBudget(t, adapter, 3)
		f.reset()

		claimed, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"})
		if claimed || err == nil {
			t.Fatalf("Claim = (%v, %v), want a reported verification failure", claimed, err)
		}
		if errors.Is(err, ErrRequestBudget) {
			t.Fatalf("reserved claim was refused inside its must-finish sequence: %v", err)
		}
		if got := issue.currentAssignees(); len(got) != 0 {
			t.Errorf("assignees = %v, want the unverifiable claim unwound", got)
		}
		if reqs, _ := f.snapshot(); len(reqs) != 3 {
			t.Errorf("claim sequence made %d requests, want assign, read-back, release: %v", len(reqs), reqs)
		}
	})
}

// Claim reserves later stages before its first request. If that first request
// is slow enough to cross a tick, the unstarted read-back and unwind still
// occupy the new window; resetting used to zero would admit a second full burst
// beside them.
func TestClaimLeaseCarriesItsReservationsAcrossTickBoundary(t *testing.T) {
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	lease, err := budget.reserveLease(readRequestCost, readRequestCost, readRequestCost)
	if err != nil {
		t.Fatal(err)
	}

	clock.advance(defaultBudgetWindow)
	if spent := budget.beginTick(defaultBudgetWindow); spent != (core.RequestReport{}) {
		t.Fatalf("closed report = %+v, want no exchange before the lease starts", spent)
	}

	// All three old reservations reach the network in the new window. The 36
	// fresh ordinary calls and one conditional probe fill, but cannot exceed, the
	// 40-request burst beside them.
	for stage := range 3 {
		reservation, err := lease.take(claimStage(stage))
		if err != nil {
			t.Fatal(err)
		}
		reservation.markAttempted()
		reservation.settle(true)
	}
	spendBilled(t, budget, ordinaryPerTick-3)
	probe, err := budget.acquire(context.Background(), true, false)
	if err != nil {
		t.Fatalf("conditional probe: %v", err)
	}
	probe.settle(true)
	if _, err := budget.acquire(context.Background(), false, false); !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("request beyond carried burst = %v, want ErrRequestBudget", err)
	}

	spent := budget.beginTick(defaultBudgetWindow)
	if spent.Billed != billedPerTick || spent.Refused != 1 {
		t.Fatalf("new-window report = %+v, want %d billed and one refusal", spent, billedPerTick)
	}
}

// A reservation held for a later Claim stage is not refillable allowance. If it
// were, the future stage and a newly full token bucket could spend the same
// credits twice when the stage eventually reached the network.
func TestUnattemptedLeaseCreditsDoNotRefill(t *testing.T) {
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	lease, err := budget.reserveLease(writeRequestCost, readRequestCost, writeRequestCost)
	if err != nil {
		t.Fatal(err)
	}

	clock.advance(defaultBudgetWindow)
	budget.beginTick(defaultBudgetWindow)
	budget.mu.Lock()
	credits, held := budget.credits, budget.heldCredits
	secondary, secondaryHeld := budget.secondaryCredits, budget.secondaryHeldCredits
	contentMinute, contentMinuteHeld := budget.contentMinuteCredits, budget.contentMinuteHeldCredits
	contentHour, contentHourHeld := budget.contentHourCredits, budget.contentHourHeldCredits
	budget.mu.Unlock()
	if credits != int64(billedPerTick-3)*requestCreditUnit || held != 3*requestCreditUnit {
		t.Errorf("primary available/held = %d/%d, want %d/%d",
			credits, held, int64(billedPerTick-3)*requestCreditUnit, 3*requestCreditUnit)
	}
	if secondary != int64(secondaryBurstPoints-11)*secondaryCreditUnit ||
		secondaryHeld != 11*secondaryCreditUnit {
		t.Errorf("secondary available/held = %d/%d, want %d/%d",
			secondary, secondaryHeld,
			int64(secondaryBurstPoints-11)*secondaryCreditUnit, 11*secondaryCreditUnit)
	}
	if contentMinute != int64(contentMinuteBurstRequests-2)*contentMinuteCreditUnit ||
		contentMinuteHeld != 2*contentMinuteCreditUnit ||
		contentHour != int64(contentHourBurstRequests-2)*contentHourCreditUnit ||
		contentHourHeld != 2*contentHourCreditUnit {
		t.Errorf("content available/held minute=%d/%d hour=%d/%d, want minute=%d/%d hour=%d/%d",
			contentMinute, contentMinuteHeld, contentHour, contentHourHeld,
			int64(contentMinuteBurstRequests-2)*contentMinuteCreditUnit, 2*contentMinuteCreditUnit,
			int64(contentHourBurstRequests-2)*contentHourCreditUnit, 2*contentHourCreditUnit)
	}

	lease.close()
	budget.mu.Lock()
	used, unattempted := budget.used, budget.unattempted
	credits, held = budget.credits, budget.heldCredits
	secondary, secondaryHeld = budget.secondaryCredits, budget.secondaryHeldCredits
	contentMinute, contentMinuteHeld = budget.contentMinuteCredits, budget.contentMinuteHeldCredits
	contentHour, contentHourHeld = budget.contentHourCredits, budget.contentHourHeldCredits
	budget.mu.Unlock()
	if used != 0 || unattempted != 0 || held != 0 || secondaryHeld != 0 || contentMinuteHeld != 0 || contentHourHeld != 0 {
		t.Errorf("returned lease left used/unattempted/held secondary=%d/%d/%d/%d content=%d/%d",
			used, unattempted, held, secondaryHeld, contentMinuteHeld, contentHourHeld)
	}
	if credits != int64(billedPerTick)*requestCreditUnit ||
		secondary != int64(secondaryBurstPoints)*secondaryCreditUnit ||
		contentMinute != int64(contentMinuteBurstRequests)*contentMinuteCreditUnit ||
		contentHour != int64(contentHourBurstRequests)*contentHourCreditUnit {
		t.Errorf("returned lease restored available credits primary=%d secondary=%d content=%d/%d, want full allowance",
			credits, secondary, contentMinute, contentHour)
	}
}

// A redirect is another HTTP exchange inside the same logical assignment.
// It must be metered without consuming the reservation that guarantees a
// failed read-back can unwind the assignment.
func TestClaimRedirectCannotConsumeItsUnwindReservation(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.redirectAssign = true
	issue.failReadBack = true
	issue.serve(f)
	adapter := f.adapter(t)
	if _, err := adapter.claimPrincipal(context.Background()); err != nil {
		t.Fatal(err)
	}
	resetBudget(adapter.budget, nil)
	f.reset()

	claimed, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"})
	if claimed || err == nil {
		t.Fatalf("Claim = (%v, %v), want a reported verification failure", claimed, err)
	}
	if errors.Is(err, ErrRequestBudget) {
		t.Fatalf("redirect consumed the reserved claim unwind: %v", err)
	}
	if got := issue.currentAssignees(); len(got) != 0 {
		t.Errorf("assignees = %v, want the redirected assignment unwound", got)
	}
	requests, _ := f.snapshot()
	if len(requests) != 4 {
		t.Fatalf("claim sequence made %d exchanges, want redirect, assignment, read-back, release: %v", len(requests), requests)
	}
	if requests[0].Status != http.StatusTemporaryRedirect || requests[0].Method != http.MethodPost || requests[1].Method != http.MethodPost {
		t.Errorf("assignment did not traverse the expected 307 chain: %v", requests[:2])
	}
}

// An assignment that fails after reaching GitHub unwinds without ever verifying,
// so the unwinding DELETE follows the assignment directly. It is still a
// content-generating five-point write, and must be charged as one: metering it at
// the skipped read-back's price would put four secondary points and a content
// request outside the allowance that exists to bound them (SPEC §8.5).
func TestUnwindingAnUncertainAssignmentIsChargedAsAWrite(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.failAssign = true
	issue.serve(f)
	adapter := f.adapter(t)
	if _, err := adapter.claimPrincipal(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A clock that does not move: a refill mid-claim would blur the two prices
	// this test exists to tell apart.
	resetBudget(adapter.budget, newManualClock().Now)
	f.reset()

	claimed, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"})
	if claimed || err == nil {
		t.Fatalf("Claim = (%v, %v), want the failed assignment reported", claimed, err)
	}
	if got := issue.currentAssignees(); len(got) != 0 {
		t.Fatalf("assignees = %v, want the uncertain assignment unwound", got)
	}
	requests, _ := f.snapshot()
	if len(requests) != 2 || requests[0].Method != http.MethodPost || requests[1].Method != http.MethodDelete {
		t.Fatalf("claim made %v, want an assignment and the DELETE that unwinds it", requests)
	}

	adapter.budget.mu.Lock()
	secondary, secondaryHeld := adapter.budget.secondaryCredits, adapter.budget.secondaryHeldCredits
	contentMinute, contentHour := adapter.budget.contentMinuteCredits, adapter.budget.contentHourCredits
	adapter.budget.mu.Unlock()

	wantSecondary := int64(secondaryBurstPoints-2*secondaryWritePoints) * secondaryCreditUnit
	if secondary != wantSecondary || secondaryHeld != 0 {
		t.Errorf("secondary credits/held = %d/%d, want %d/0 — two writes at %d points, not one write and a %d-point read",
			secondary, secondaryHeld, wantSecondary, secondaryWritePoints, secondaryReadPoints)
	}
	wantMinute := int64(contentMinuteBurstRequests-2) * contentMinuteCreditUnit
	wantHour := int64(contentHourBurstRequests-2) * contentHourCreditUnit
	if contentMinute != wantMinute || contentHour != wantHour {
		t.Errorf("content credits minute/hour = %d/%d, want %d/%d — the assignment and its DELETE both generate content",
			contentMinute, contentHour, wantMinute, wantHour)
	}
}

// Claim checks the rate gate once at method entry, then resolves local facts
// before assignment. A refusal learned in that interval must still stop the
// first leased exchange; only post-assignment verification and unwind bypass
// the gate.
func TestClaimAssignmentHonorsRateLimitLearnedAfterMethodEntry(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	adapter := f.adapter(t)
	if _, err := adapter.claimPrincipal(context.Background()); err != nil {
		t.Fatal(err)
	}
	resetBudget(adapter.budget, nil)
	f.reset()

	next := adapter.auth.next
	wrappedCalls := 0
	adapter.auth.next = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		wrappedCalls++
		if wrappedCalls == 1 {
			resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
			adapter.gate.observe(&gh.AbuseRateLimitError{Response: resp, RetryAfter: gh.Ptr(time.Hour)})
		}
		return next.RoundTrip(req)
	})

	claimed, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"})
	var limit *RateLimitError
	if claimed || !errors.As(err, &limit) {
		t.Fatalf("Claim = (%v, %v), want false and the newly learned rate limit", claimed, err)
	}
	if wrappedCalls != 1 {
		t.Errorf("leased calls entering the budget = %d, want only the refused assignment", wrappedCalls)
	}
	if requests, _ := f.snapshot(); len(requests) != 0 {
		t.Errorf("rate-limited assignment still made %d GitHub requests: %v", len(requests), requests)
	}
	if got := issue.currentAssignees(); len(got) != 0 {
		t.Errorf("assignees = %v, want no assignment after the gate closed", got)
	}
}

// Claim arbitration cannot wait while this daemon stands assigned, but an
// uncached restart from page one can never cross the same request boundary.
// Cached pages are revalidated at the origin and let the next attempt advance.
func TestContestedClaimHistoryProgressesAcrossRequestWindows(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.assign("other")
	for range 4000 {
		issue.arbitraryEvent("mentioned")
	}
	issue.serve(f)
	adapter := f.adapter(t)
	if _, err := adapter.claimPrincipal(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock := newManualClock()
	resetBudget(adapter.budget, clock.Now)
	f.reset()

	claimed, err := adapter.Claim(context.Background(), core.Issue{Identifier: "1"})
	if claimed || err != nil {
		t.Fatalf("first Claim = (%v, %v), want a prompt yield at the request boundary", claimed, err)
	}
	if pages := len(f.calls("GET", "/events")); pages != ordinaryPerTick-3 {
		t.Fatalf("first arbitration read %d pages, want %d after reserving Claim's three requests", pages, ordinaryPerTick-3)
	}
	if got := issue.currentAssignees(); len(got) != 1 || got[0] != "other" {
		t.Fatalf("assignees after first yield = %v, want only the earlier claimant", got)
	}

	clock.advance(defaultBudgetWindow)
	closed := adapter.BeginTick(defaultBudgetWindow)
	if closed.Billed != ordinaryPerTick || closed.Refused != 1 || closed.Deferred != 0 {
		t.Errorf("closed report = %+v, want 39 billed requests, one prompt refusal, and no wait", closed)
	}
	f.reset()

	claimed, err = adapter.Claim(context.Background(), core.Issue{Identifier: "1"})
	if claimed || err != nil {
		t.Fatalf("second Claim = (%v, %v), want the earlier claimant to win", claimed, err)
	}
	events := f.calls("GET", "/events")
	if len(events) != 41 {
		t.Fatalf("second arbitration made %d event requests, want 36 revalidations plus pages 37-41", len(events))
	}
	for i, request := range events[:ordinaryPerTick-3] {
		if request.IfNoneMatch == "" || request.Status != http.StatusNotModified || request.Billed {
			t.Errorf("saved event page %d was not revalidated for free: %+v", i+1, request)
		}
	}
	if got := issue.currentAssignees(); len(got) != 1 || got[0] != "other" {
		t.Errorf("assignees after completed arbitration = %v, want only the winner", got)
	}
}

// A recovery history can outgrow one request window. Continuations wait with
// their accumulated pages intact; restarting the call every tick would make a
// history beyond the same boundary permanently unreadable.
func TestLongHistoryContinuesAcrossRequestWindows(t *testing.T) {
	f := newFakeGitHub(t)
	issue := newFakeIssue(1, "ben-queue")
	issue.serve(f)
	for range 4001 {
		issue.arbitraryEvent("mentioned")
	}
	adapter := f.adapter(t)
	if _, err := adapter.claimPrincipal(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock := newManualClock()
	resetBudget(adapter.budget, clock.Now)
	f.reset()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := adapter.ClaimHistory(ctx, core.Issue{Identifier: "1"})
		done <- err
	}()

	deadline := time.Now().Add(2 * time.Second)
	for (len(f.calls("GET", "/events")) < ordinaryPerTick || deferredRequests(adapter.budget) == 0) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if pages := len(f.calls("GET", "/events")); pages != ordinaryPerTick {
		t.Fatalf("history reached %d pages before waiting, want %d", pages, ordinaryPerTick)
	}
	select {
	case err := <-done:
		t.Fatalf("history returned instead of preserving its first %d pages: %v", ordinaryPerTick, err)
	default:
	}

	clock.advance(defaultBudgetWindow)
	closed := adapter.BeginTick(defaultBudgetWindow)
	if closed.Deferred != 1 || closed.Refused != 0 {
		t.Errorf("closed report = %+v, want one preserved continuation", closed)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("history after next window: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("history did not resume when the next request window opened")
	}
	if pages := len(f.calls("GET", "/events")); pages != 41 {
		t.Errorf("history used %d pages, want all 41", pages)
	}
}

// Milestone event history is cached page by page against GitHub's ETags. A cold
// walk that fills the request window returns promptly, releasing the serial
// effects worker; the next tick revalidates those pages for free and continues
// instead of waiting in place or billing the same prefix again.
func TestCommentEventHistoryCachesAcrossRequestWindows(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	// The assignment and label fixtures are the other two events. One event on
	// page 40 proves the first attempt stops at the request boundary.
	for range ordinaryPerTick*perPage - 1 {
		issue.arbitraryEvent("mentioned")
	}
	adapter := f.adapter(t)
	if _, err := adapter.claimPrincipal(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock := newManualClock()
	resetBudget(adapter.budget, clock.Now)
	f.reset()

	comment := core.MilestoneComment{Milestone: core.MilestoneClaimed}
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, comment); !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("first Comment error = %v, want a visible request-boundary refusal", err)
	}
	if pages := len(f.calls("GET", "/events")); pages != ordinaryPerTick {
		t.Fatalf("cold event walk reached %d pages, want the %d-page ordinary bound", pages, ordinaryPerTick)
	}
	if pages := len(f.calls("GET", "/comments")); pages != 0 {
		t.Errorf("marker walk started before event history completed: %d pages", pages)
	}
	clock.advance(defaultBudgetWindow)
	closed := adapter.BeginTick(defaultBudgetWindow)
	if closed.Refused != 1 || closed.Deferred != 0 {
		t.Errorf("closed report = %+v, want one refusal and no blocked continuation", closed)
	}
	f.reset()

	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, comment); err != nil {
		t.Fatalf("Comment after next window: %v", err)
	}
	events := f.calls("GET", "/events")
	if len(events) != ordinaryPerTick+1 {
		t.Fatalf("resumed event walk made %d requests, want %d cached pages plus page 40", len(events), ordinaryPerTick+1)
	}
	for i, request := range events[:ordinaryPerTick] {
		if request.IfNoneMatch == "" || request.Status != http.StatusNotModified || request.Billed {
			t.Errorf("cached event page %d was not a free ETag revalidation: %+v", i+1, request)
		}
	}
	if !events[ordinaryPerTick].Billed {
		t.Errorf("previously unread page 40 was not billed: %+v", events[ordinaryPerTick])
	}
	if posts := len(f.calls("POST", "/comments")); posts != 1 {
		t.Errorf("posted %d milestone comments, want 1", posts)
	}
}

// A full cached terminal page needs one extra probe. Appending event 101 leaves
// page 1's body (and therefore a body-derived ETag) unchanged while moving the
// new milestone transition onto page 2; the stale cached no-Link header must not
// hide that page.
func TestMilestoneEventCacheFindsPageAppendedAfterFullBoundary(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	for range perPage - 2 {
		issue.arbitraryEvent("mentioned")
	}
	adapter := f.adapter(t)
	claimed := core.MilestoneComment{Milestone: core.MilestoneClaimed}
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
		t.Fatalf("warming event page: %v", err)
	}

	issue.label("ben:needs-review") // event 101, on a newly-created second page
	f.reset()
	parked := core.MilestoneComment{Milestone: core.MilestoneNeedsReview}
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, parked); err != nil {
		t.Fatalf("Comment after the event page grew: %v", err)
	}

	events := f.calls("GET", "/events")
	if len(events) != 2 {
		t.Fatalf("event lookup made %d requests, want cached page 1 plus page 2", len(events))
	}
	if events[0].Status != http.StatusNotModified || events[0].IfNoneMatch == "" {
		t.Errorf("page 1 was not served from the revalidated cache: %+v", events[0])
	}
	if !strings.Contains(events[1].Query, "page=2") {
		t.Errorf("event lookup did not probe the appended page: %+v", events[1])
	}
	if posts := len(f.calls("POST", "/comments")); posts != 1 {
		t.Errorf("posted %d needs-review comments, want 1", posts)
	}
}

// The marker lookup is deliberately uncached correctness evidence. Once that
// bounded suffix has been read, its write waits for the next window rather than
// throwing the completed lookup away and repeating it forever.
func TestCommentMarkerWalkContinuesAcrossRequestWindows(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	// One event page plus 38 comment pages fills the ordinary window.
	for i := range (ordinaryPerTick - 1) * perPage {
		issue.addComment(fmt.Sprintf("human comment %d", i), eventTime.Time)
	}
	adapter := f.adapter(t)
	if _, err := adapter.claimPrincipal(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock := newManualClock()
	resetBudget(adapter.budget, clock.Now)
	f.reset()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- adapter.Comment(ctx, core.Issue{Identifier: "1"}, core.MilestoneComment{Milestone: core.MilestoneClaimed})
	}()

	deadline := time.Now().Add(2 * time.Second)
	for deferredRequests(adapter.budget) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if deferredRequests(adapter.budget) == 0 {
		t.Fatal("comment never waited after completing the marker walk")
	}
	if pages := len(f.calls("GET", "/events")); pages != 1 {
		t.Errorf("event walk used %d pages, want 1", pages)
	}
	if pages := len(f.calls("GET", "/comments")); pages != ordinaryPerTick-1 {
		t.Errorf("marker walk used %d pages, want %d", pages, ordinaryPerTick-1)
	}
	select {
	case err := <-done:
		t.Fatalf("comment returned instead of retaining its completed marker walk: %v", err)
	default:
	}

	clock.advance(defaultBudgetWindow)
	closed := adapter.BeginTick(defaultBudgetWindow)
	if closed.Deferred != 1 || closed.Refused != 0 {
		t.Errorf("closed report = %+v, want one preserved continuation", closed)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("comment after next window: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("comment did not resume when the next request window opened")
	}
	if posts := len(f.calls("POST", "/comments")); posts != 1 {
		t.Errorf("posted %d milestone comments, want 1", posts)
	}
}

func deferredRequests(b *requestBudget) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.report.Deferred
}

// Slot 40 is kept for a conditional probe. Spending all 39 ordinary slots must
// still let a cached 304 through, and the free answer returns the probe slot so
// repeated idle polls remain free.
func TestRevalidationStillRunsAfterOrdinaryBudgetIsSpent(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveIssues(issueFixture(1, "ben-queue"))
	adapter := f.adapter(t)
	if _, err := adapter.Fetch(context.Background()); err != nil {
		t.Fatalf("cold Fetch: %v", err)
	}
	resetBudget(adapter.budget, nil)
	spendBilled(t, adapter.budget, ordinaryPerTick)
	f.reset()

	const polls = 3
	for i := range polls {
		if _, err := adapter.Fetch(context.Background()); err != nil {
			t.Fatalf("cached Fetch %d: %v", i+1, err)
		}
	}
	requests, billed := f.snapshot()
	if len(requests) != polls || billed != 0 {
		t.Fatalf("cached polls = %d requests, %d billed; want %d free revalidations", len(requests), billed, polls)
	}
	for _, request := range requests {
		if request.IfNoneMatch == "" || request.Status != http.StatusNotModified {
			t.Errorf("poll was not a 304 revalidation: %+v", request)
		}
	}
	spent := adapter.BeginTick(defaultBudgetWindow)
	if spent.Billed != ordinaryPerTick || spent.Unbilled != polls || spent.Refused != 0 {
		t.Errorf("report = %+v, want %d billed setup requests and %d free probes", spent, ordinaryPerTick, polls)
	}
}

// GitHub exempts an authenticated 304 from the primary limit, not its
// secondary request-point limit. Revalidation therefore returns the primary
// reservation while the independent rolling allowance still paces the walk.
// The last 11 points stay available for the Claim selected by that walk.
func TestUnbilledRevalidationsPreserveClaimSecondaryCapacity(t *testing.T) {
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	ordinaryBurst := secondaryBurstPoints - claimSecondaryReservePoints
	for i := range ordinaryBurst {
		reservation, err := budget.acquire(context.Background(), true, false)
		if err != nil {
			t.Fatalf("revalidation %d of %d: %v", i+1, ordinaryBurst, err)
		}
		reservation.settle(false)
	}

	refusable := refuseConditionalRequestBudget(context.Background())
	if _, err := budget.acquire(refusable, true, false); !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("request at Claim reserve = %v, want ErrRequestBudget", err)
	}
	lease, err := budget.reserveLease(writeRequestCost, readRequestCost, writeRequestCost)
	if err != nil {
		t.Fatalf("Claim could not use its protected secondary capacity: %v", err)
	}
	lease.close()
	closed := budget.beginTick(defaultBudgetWindow)
	if closed.Unbilled != ordinaryBurst || closed.Refused != 1 {
		t.Errorf("closed report = %+v, want %d unbilled requests and one refusal", closed, ordinaryBurst)
	}
	if _, err := budget.acquire(refusable, true, false); !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("BeginTick replenished the secondary allowance: %v", err)
	}

	clock.advance(time.Duration((secondaryCreditUnit + int64(secondaryRefillPoints) - 1) / int64(secondaryRefillPoints)))
	reservation, err := budget.acquire(context.Background(), true, false)
	if err != nil {
		t.Fatalf("one secondary point did not refill: %v", err)
	}
	reservation.settle(false)
}

func TestRequestCostDependsOnHTTPMethod(t *testing.T) {
	for _, test := range []struct {
		method      string
		wantPoints  int
		wantContent int
	}{
		{http.MethodGet, secondaryReadPoints, 0},
		{http.MethodHead, secondaryReadPoints, 0},
		{http.MethodOptions, secondaryReadPoints, 0},
		{http.MethodPost, secondaryWritePoints, 1},
		{http.MethodPut, secondaryWritePoints, 1},
		{http.MethodPatch, secondaryWritePoints, 1},
		{http.MethodDelete, secondaryWritePoints, 1},
	} {
		got := requestCostForMethod(test.method)
		if got.secondaryPoints != test.wantPoints || got.contentRequests != test.wantContent {
			t.Errorf("requestCostForMethod(%q) = %+v, want %d secondary points and %d content requests",
				test.method, got, test.wantPoints, test.wantContent)
		}
	}
}

// GitHub's content-generation limits are independent of its 900-point REST
// limit. Ordinary mutations retain the two content slots Claim may need for
// assignment and unwind, and the combined spend fits both published windows.
func TestContentRequestsHonorMinuteAndHourLimits(t *testing.T) {
	tests := []struct {
		name       string
		window     time.Duration
		step       time.Duration
		limit      int
		minimumUse int
	}{
		{"minute", time.Minute, 100 * time.Millisecond, contentRequestsPerMinute, 75},
		{"hour", time.Hour, time.Second, contentRequestsPerHour, 490},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clock := newManualClock()
			budget := newRequestBudget(clock.Now)
			admitted := 0
			for elapsed := time.Duration(0); elapsed < test.window; elapsed += test.step {
				reservation, err := budget.acquireCost(context.Background(), false, false, writeRequestCost, true)
				if err == nil {
					reservation.settle(true)
					admitted++
				} else if !errors.Is(err, ErrRequestBudget) {
					t.Fatalf("mutation at %s: %v", elapsed, err)
				}
				clock.advance(test.step)
				budget.beginTick(test.step)
			}

			lease, err := budget.reserveLease(claimLeaseCosts[:]...)
			if err != nil {
				t.Fatalf("Claim could not use its protected content capacity: %v", err)
			}
			for stage := range len(claimLeaseCosts) {
				reservation, err := lease.take(claimStage(stage))
				if err != nil {
					t.Fatal(err)
				}
				reservation.settle(true)
			}
			lease.close()
			totalContent := admitted + claimContentReserveRequests
			if totalContent > test.limit {
				t.Errorf("admitted %d content requests in one %s window, limit %d", totalContent, test.name, test.limit)
			}
			if totalContent < test.minimumUse {
				t.Errorf("admitted only %d content requests in one %s window, want at least %d", totalContent, test.name, test.minimumUse)
			}
		})
	}
}

// If slot 40 turns out to be billed, the next conditional request waits for a
// new window instead of being refused before its cost is known.
func TestChangedConditionalDefersTheNextProbeInsteadOfRefusingIt(t *testing.T) {
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	spendBilled(t, budget, ordinaryPerTick)

	var mu sync.Mutex
	calls := 0
	transport := &budgetTransport{budget: budget, next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		status := http.StatusNotModified
		if call == 1 {
			status = http.StatusOK
		}
		return response(req, status), nil
	})}
	conditional := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, "https://api.github.test/x", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("If-None-Match", `"v1"`)
		return transport.RoundTrip(req)
	}

	first, err := conditional()
	if err != nil {
		t.Fatalf("changed conditional request: %v", err)
	}
	first.Body.Close()
	done := make(chan error, 1)
	go func() {
		resp, err := conditional()
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("second probe returned before another window opened: %v", err)
	default:
	}

	clock.advance(defaultBudgetWindow)
	closed := budget.beginTick(defaultBudgetWindow)
	if closed.Billed != billedPerTick || closed.Deferred != 1 || closed.Refused != 0 {
		t.Errorf("closed report = %+v, want 40 billed and one deferred probe", closed)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("deferred probe: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("deferred probe did not resume")
	}
	if spent := budget.beginTick(defaultBudgetWindow); spent.Unbilled != 1 {
		t.Errorf("resumed probe report = %+v, want one free 304", spent)
	}
}

// Claim arbitration persists event pages in the conditional cache, but it
// cannot wait while the issue stands assigned. Its explicit mode refuses an
// occupied conditional slot so Claim can unwind and retry the saved prefix.
func TestClaimRevalidationCanRefuseInsteadOfWaitingAssigned(t *testing.T) {
	budget := newRequestBudget(newManualClock().Now)
	spendBilled(t, budget, ordinaryPerTick)
	probe, err := budget.acquire(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	probe.settle(true)

	networkCalls := 0
	transport := &budgetTransport{budget: budget, next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		networkCalls++
		return response(req, http.StatusNotModified), nil
	})}
	ctx := refuseConditionalRequestBudget(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("If-None-Match", `"events"`)

	resp, err := transport.RoundTrip(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("refusable revalidation error = %v, want ErrRequestBudget", err)
	}
	if networkCalls != 0 {
		t.Errorf("refused revalidation made %d network calls, want 0", networkCalls)
	}
	if spent := budget.beginTick(defaultBudgetWindow); spent.Billed != billedPerTick || spent.Refused != 1 || spent.Deferred != 0 {
		t.Errorf("report = %+v, want 40 billed, one refusal, and no deferred request", spent)
	}
}

// A request may wait after its public call passed the rate gate. If another
// concurrent call learns a Retry-After in that interval, admission must return
// its reservation and surface the standing refusal instead of reaching GitHub.
func TestDeferredRequestHonorsRateLimitLearnedWhileWaiting(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveIssues(issueFixture(1, "ben-queue"))
	adapter := f.adapter(t)
	if _, err := adapter.Fetch(context.Background()); err != nil {
		t.Fatalf("warming conditional cache: %v", err)
	}
	clock := newManualClock()
	resetBudget(adapter.budget, clock.Now)
	spendBilled(t, adapter.budget, ordinaryPerTick)
	probe, err := adapter.budget.acquire(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	probe.settle(true)
	f.reset()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := adapter.Fetch(ctx)
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for deferredRequests(adapter.budget) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if deferredRequests(adapter.budget) == 0 {
		t.Fatal("conditional request never waited on the spent window")
	}

	resp := &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{}}
	observed := adapter.gate.observe(&gh.AbuseRateLimitError{Response: resp, RetryAfter: gh.Ptr(time.Hour)})
	var observedLimit *RateLimitError
	if !errors.As(observed, &observedLimit) {
		t.Fatalf("closing rate gate: %v", observed)
	}
	closed := adapter.BeginTick(defaultBudgetWindow)
	if closed.Billed != billedPerTick || closed.Deferred != 1 {
		t.Errorf("closed report = %+v, want %d billed and one deferred request", closed, billedPerTick)
	}

	select {
	case err := <-done:
		var limit *RateLimitError
		if !errors.As(err, &limit) {
			t.Fatalf("deferred Fetch error = %v, want the standing rate limit", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("deferred request did not wake when the request window opened")
	}
	if reqs, _ := f.snapshot(); len(reqs) != 0 {
		t.Errorf("deferred request ignored Retry-After and made %d requests: %v", len(reqs), reqs)
	}

	adapter.budget.mu.Lock()
	used, credits, borrowed := adapter.budget.used, adapter.budget.credits, adapter.budget.borrowed
	adapter.budget.mu.Unlock()
	if used != 0 || credits != 0 || borrowed {
		t.Errorf("returned reservation left used/credits/borrowed = %d/%d/%v, want 0/0/false", used, credits, borrowed)
	}
}

// Reservations, not completed responses, enforce the bound. More callers can
// race than fit, but at most 39 ordinary requests plus one conditional probe
// may reach the network.
func TestConcurrentRequestsCannotOverspendTheWindow(t *testing.T) {
	budget := newRequestBudget(newManualClock().Now)
	started := make(chan struct{}, 100)
	release := make(chan struct{})
	transport := &budgetTransport{budget: budget, next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-release
		return response(req, http.StatusOK), nil
	})}

	type outcome struct{ err error }
	results := make(chan outcome, 64)
	request := func(ctx context.Context, conditional bool) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/x", nil)
		if err == nil && conditional {
			req.Header.Set("If-None-Match", `"v1"`)
		}
		if err == nil {
			var resp *http.Response
			resp, err = transport.RoundTrip(req)
			if resp != nil {
				resp.Body.Close()
			}
		}
		results <- outcome{err: err}
	}

	const ordinaryCallers = 60
	for range ordinaryCallers {
		go request(context.Background(), false)
	}
	for range ordinaryPerTick {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("ordinary requests did not fill their reservations")
		}
	}
	go request(context.Background(), true)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("reserved conditional probe did not reach the network")
	}

	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	go request(blockedCtx, true)
	select {
	case <-started:
		t.Fatal("more than 40 concurrent requests reached the network")
	case <-time.After(20 * time.Millisecond):
	}
	cancelBlocked()
	close(release)

	var succeeded, refused, canceled int
	for range ordinaryCallers + 2 {
		select {
		case result := <-results:
			switch {
			case result.err == nil:
				succeeded++
			case errors.Is(result.err, ErrRequestBudget):
				refused++
			case errors.Is(result.err, context.Canceled):
				canceled++
			default:
				t.Errorf("unexpected request error: %v", result.err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent requests did not settle")
		}
	}
	if succeeded != billedPerTick || refused != ordinaryCallers-ordinaryPerTick || canceled != 1 {
		t.Errorf("outcomes success/refused/canceled = %d/%d/%d, want %d/%d/1",
			succeeded, refused, canceled, billedPerTick, ordinaryCallers-ordinaryPerTick)
	}
	if spent := budget.beginTick(defaultBudgetWindow); spent.Billed != billedPerTick || spent.Refused != refused {
		t.Errorf("report = %+v, want %d billed and %d refused", spent, billedPerTick, refused)
	}
}

// BeginTick reports and resets burst accounting, but it does not mint rolling
// credits. A faster legal polling interval therefore cannot spend a fresh 40
// requests on every tick.
func TestBeginTickPreservesTheRollingAllowance(t *testing.T) {
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	spendBilled(t, budget, 3)
	free, err := budget.acquire(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	free.settle(false)

	if spent := budget.beginTick(time.Second); spent != (core.RequestReport{Billed: 3, Unbilled: 1}) {
		t.Errorf("report = %+v, want three billed and one free", spent)
	}
	if again := budget.beginTick(time.Second); again != (core.RequestReport{}) {
		t.Errorf("second report = %+v, want cleared counters", again)
	}
	spendBilled(t, budget, billedPerTick-3)
	if _, err := budget.acquire(context.Background(), false, false); !errors.Is(err, ErrRequestBudget) {
		t.Errorf("BeginTick minted rolling credits: %v", err)
	}
}

// A slow polling cadence is still one tick and therefore one burst window. The
// rolling allowance refills every 30 seconds, but replenished credits must not
// open extra 40-request bursts before the next tick begins.
func TestSlowTickDoesNotOpenIntermediateBurstWindows(t *testing.T) {
	const interval = 5 * time.Minute
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	budget.beginTick(interval)
	spendBilled(t, budget, ordinaryPerTick)
	probe, err := budget.acquire(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	probe.settle(true)

	clock.advance(defaultBudgetWindow)
	if reservation := tryReserveAtCurrentTime(budget, false); reservation != nil {
		reservation.releaseUnused()
		t.Fatal("the 30-second rolling refill opened a second burst inside a five-minute tick")
	}

	clock.advance(interval - defaultBudgetWindow)
	reservation := tryReserveAtCurrentTime(budget, false)
	if reservation == nil {
		t.Fatal("the burst window did not self-roll when the supplied tick interval elapsed")
	}
	reservation.settle(true)
}

// An adapter nobody ticks self-rolls its burst window, while the elapsed-time
// credit refill still keeps it at the sustainable rate.
func TestWindowRollsItselfWhenNobodyOpensOne(t *testing.T) {
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	spendBilled(t, budget, ordinaryPerTick)
	probe, err := budget.acquire(context.Background(), true, false)
	if err != nil {
		t.Fatal(err)
	}
	probe.settle(true)

	if _, err := budget.acquire(context.Background(), false, false); !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("full window error = %v, want ErrRequestBudget", err)
	}
	clock.advance(defaultBudgetWindow - time.Millisecond)
	if _, err := budget.acquire(context.Background(), false, false); !errors.Is(err, ErrRequestBudget) {
		t.Errorf("window rolled before its ceiling: %v", err)
	}
	clock.advance(2 * time.Millisecond)
	reservation, err := budget.acquire(context.Background(), false, false)
	if err != nil {
		t.Fatalf("window never rolled itself: %v", err)
	}
	reservation.settle(true)
	if spent := budget.beginTick(defaultBudgetWindow); spent.Billed != billedPerTick+1 {
		t.Errorf("report = %+v, want all %d billed requests across both windows", spent, billedPerTick+1)
	}
}

// A one-second polling cadence opens 3,600 burst windows an hour. The rolling
// allowance, including the one bounded speculative conditional probe, still
// remains below the credential's 5,000-request primary limit.
func TestRollingAllowanceFitsHourlyLimitAtShortCadence(t *testing.T) {
	const hourlyLimit = 5000
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	total := 0
	for range int(time.Hour / time.Second) {
		budget.beginTick(time.Second)
		for {
			reservation := tryReserveAtCurrentTime(budget, false)
			if reservation == nil {
				break
			}
			reservation.settle(true)
			total++
		}
		if reservation := tryReserveAtCurrentTime(budget, true); reservation != nil {
			reservation.settle(true)
			total++
		}
		clock.advance(time.Second)
	}
	if total > hourlyLimit {
		t.Errorf("one-second ticks spent %d billed requests in an hour, over %d", total, hourlyLimit)
	}
	if total < 4000 {
		t.Errorf("simulation spent only %d requests; it did not exercise the rolling allowance", total)
	}
}

func tryReserveAtCurrentTime(b *requestBudget, conditional bool) *requestReservation {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.rollIfStaleLocked(now)
	b.refillLocked(now)
	return b.reserveOneLocked(conditional)
}

// The report is what makes a spent tick visible, so its numbers must match the
// exchanges the server saw.
func TestReportCountsWhatTheServerWasAsked(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	adapter := f.adapter(t)
	if err := adapter.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	requests, billed := f.snapshot()
	spent := adapter.BeginTick(defaultBudgetWindow)
	if spent.Billed != billed || spent.Billed+spent.Unbilled != len(requests) {
		t.Errorf("report = %+v, want %d billed of %d requests", spent, billed, len(requests))
	}
}

// A response racing BeginTick belongs to exactly one side of the boundary.
// Once the request is reported pending, its later result is a late resolution
// and cannot inflate the next tick's own Billed or Unbilled count.
func TestResponseAfterTickBoundaryDoesNotMoveIntoTheNextWindow(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		want   core.RequestReport
	}{
		{"billed", http.StatusOK, core.RequestReport{LateBilled: 1}},
		{"unbilled", http.StatusNotModified, core.RequestReport{LateUnbilled: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := newRequestBudget(nil)
			started := make(chan struct{})
			release := make(chan struct{})
			transport := &budgetTransport{budget: budget, next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-release
				return response(req, test.status), nil
			})}
			done := make(chan error, 1)
			go func() {
				req, err := http.NewRequest(http.MethodGet, "https://api.github.test/x", nil)
				if err == nil && test.status == http.StatusNotModified {
					req.Header.Set("If-None-Match", `"v1"`)
				}
				if err == nil {
					var resp *http.Response
					resp, err = transport.RoundTrip(req)
					if resp != nil {
						resp.Body.Close()
					}
				}
				done <- err
			}()

			<-started
			if closed := budget.beginTick(defaultBudgetWindow); closed != (core.RequestReport{Pending: 1}) {
				t.Errorf("closed report = %+v, want one pending request", closed)
			}
			close(release)
			if err := <-done; err != nil {
				t.Fatalf("request: %v", err)
			}
			if next := budget.beginTick(defaultBudgetWindow); next != test.want {
				t.Errorf("next report = %+v, want %+v", next, test.want)
			}
		})
	}
}

// The HTTP timeout belongs to the logical client call. Redirect exchanges draw
// down the same allowance even though each one enters RoundTrip separately.
func TestRedirectChainSharesOneNetworkTimeout(t *testing.T) {
	clock := newManualClock()
	budget := newRequestBudget(clock.Now)
	calls := 0
	transport := &budgetTransport{
		budget:         budget,
		networkTimeout: 30 * time.Millisecond,
		networkNow:     clock.Now,
		next: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			clock.advance(20 * time.Millisecond)
			resp := response(req, http.StatusOK)
			if calls == 1 {
				resp.StatusCode = http.StatusTemporaryRedirect
				resp.Header.Set("Location", "/redirected")
			}
			return resp, nil
		}),
	}
	client := &http.Client{Transport: transport}
	req, err := http.NewRequest(http.MethodGet, "https://api.github.test/start", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("redirected request error = %v, want context deadline exceeded", err)
	}
	if calls != 2 {
		t.Errorf("network exchanges = %d, want both redirect hops to exercise the shared timeout", calls)
	}
}

// A transport failure is charged: GitHub may have received it, so
// under-counting would violate the bound.
func TestFailedRequestsAreCharged(t *testing.T) {
	budget := newRequestBudget(newManualClock().Now)
	transport := &budgetTransport{next: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	}), budget: budget}
	req, err := http.NewRequest(http.MethodGet, "https://api.github.test/x", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.RoundTrip(req); err == nil {
		t.Fatal("the transport error must surface")
	}
	if spent := budget.beginTick(defaultBudgetWindow); spent.Billed != 1 {
		t.Errorf("report = %+v, want the failed request charged", spent)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(req *http.Request, status int) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}
}

// The marker lookup is read from the anchor forward, so an issue with a long
// discussion costs the same as a quiet one — and stays idempotent (SPEC §8.4).
func TestMarkerLookupReadsOnlyFromTheAnchor(t *testing.T) {
	f := newFakeGitHub(t)
	issue := claimedIssue(t, f)
	for i := range 3 * perPage {
		issue.addComment(fmt.Sprintf("human comment %d", i), eventTime.Add(-time.Hour))
	}
	adapter := f.adapter(t)

	claimed := core.MilestoneComment{Milestone: core.MilestoneClaimed}
	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	reads := f.calls("GET", "/comments")
	if len(reads) != 1 {
		t.Fatalf("read the comment list over %d requests, want 1 — the discussion is not ours to walk", len(reads))
	}
	if !strings.Contains(reads[0].Query, "since=") {
		t.Errorf("comment read was unbounded: %s", reads[0].Query)
	}

	if err := adapter.Comment(context.Background(), core.Issue{Identifier: "1"}, claimed); err != nil {
		t.Fatalf("second Comment: %v", err)
	}
	if got := issue.currentComments(); len(got) != 3*perPage+1 {
		t.Errorf("got %d comments, want the discussion plus exactly one milestone", len(got))
	}
}

// The orchestrator discovers the budget through its declared capability.
func TestAdapterAnswersTheDeclaredBudgetContract(t *testing.T) {
	f := newFakeGitHub(t)
	var tracker core.TrackerAdapter = f.adapter(t)
	budget, ok := tracker.(core.RequestBudget)
	if !ok {
		t.Fatalf("%T does not implement core.RequestBudget", tracker)
	}
	if spent := budget.BeginTick(defaultBudgetWindow); spent != (core.RequestReport{}) {
		t.Errorf("a fresh adapter reports %+v, want zero", spent)
	}
}
