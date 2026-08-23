package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// billedPerTick bounds the burst one request window may spend. The rolling
// allowance below is the hourly bound: 40 requests per 30 seconds is 4,800 an
// hour, leaving headroom below GitHub's 5,000-request primary limit (SPEC
// §8.5). Keeping the burst and rate limits separate means a legal shorter
// polling interval cannot multiply the credential's hourly spend.
const (
	billedPerTick       = 40
	ordinaryPerTick     = billedPerTick - 1
	defaultBudgetWindow = 30 * time.Second
	requestCreditUnit   = int64(defaultBudgetWindow)

	// GitHub's secondary REST limit is 900 points per minute: most reads cost
	// one point and writes cost five. A 100-point bucket refilling at 800 points
	// per minute permits at most 900 points in any minute, including the initial
	// burst. Unlike the primary allowance, a 304 still spends these points.
	secondaryPointsPerMinute = 900
	secondaryBurstPoints     = 100
	secondaryRefillPoints    = secondaryPointsPerMinute - secondaryBurstPoints
	secondaryCreditUnit      = int64(time.Minute)
	secondaryReadPoints      = 1
	secondaryWritePoints     = 5
	// A Claim must be able to assign, verify, and possibly release after the
	// poll that selected its issue. Ordinary traffic cannot spend this floor.
	claimSecondaryReservePoints = 2*secondaryWritePoints + secondaryReadPoints

	// GitHub also applies secondary limits to content-generating requests. Two
	// independent token buckets enforce both published windows. Their bursts
	// plus refill rates equal the corresponding limit, so no rolling minute or
	// hour can exceed it; all non-read methods are classified conservatively.
	contentRequestsPerMinute    = 80
	contentRequestsPerHour      = 500
	contentMinuteBurstRequests  = 20
	contentHourBurstRequests    = 80
	contentMinuteRefillRequests = contentRequestsPerMinute - contentMinuteBurstRequests
	contentHourRefillRequests   = contentRequestsPerHour - contentHourBurstRequests
	contentMinuteCreditUnit     = int64(time.Minute)
	contentHourCreditUnit       = int64(time.Hour)
	claimContentReserveRequests = 2
)

type requestCost struct {
	secondaryPoints int
	contentRequests int
}

var (
	readRequestCost  = requestCost{secondaryPoints: secondaryReadPoints}
	writeRequestCost = requestCost{secondaryPoints: secondaryWritePoints, contentRequests: 1}
)

// claimStage names one reservation in the sequence reserveLease admits for a
// Claim. The stages are *addressed*, never consumed in order, because Claim can
// skip one: an assignment that fails after reaching GitHub unwinds without ever
// verifying. Taking "the next" reservation there would meter the unwinding
// DELETE — a content-generating five-point write — at the read-back's one point
// and no content request, while refunding the write capacity it actually spends
// (SPEC §8.5).
type claimStage int

const (
	claimStageAssign claimStage = iota
	claimStageVerify
	claimStageRelease
)

// claimLeaseCosts prices each stage, in stage order — and is the argument
// reserveLease is called with, so the index that addresses a reservation is the
// index that priced it.
var claimLeaseCosts = [...]requestCost{
	claimStageAssign:  writeRequestCost,
	claimStageVerify:  readRequestCost,
	claimStageRelease: writeRequestCost,
}

// requestBudget reserves capacity before a request reaches the network. That
// is the enforcement point: counting only completed responses lets concurrent
// requests all observe the same spare slot and overspend it.
//
// Ordinary traffic is capped one below the window limit. The last slot is for
// an If-None-Match probe whose cost is unknowable until GitHub answers: a 304
// refunds its reservation, while a changed resource consumes the slot. Without
// that reserve, spending the budget would black out the free revalidations that
// make steady-state polling affordable at all (SPEC §8.5).
type requestBudget struct {
	limit         int
	ordinaryLimit int
	ceiling       time.Duration
	now           func() time.Time

	mu sync.Mutex
	// opened/generation/used are the current burst window. Used includes
	// in-flight and not-yet-attempted reservations, so the bound holds under
	// concurrency and across a tick boundary.
	opened     time.Time
	generation uint64
	used       int
	// unattempted is the subset of used whose exchange has not reached the base
	// transport. Claim reserves its whole must-finish sequence up front, so these
	// reservations survive a window boundary and must occupy the next window
	// until they are attempted or returned.
	unattempted int

	// credits is a fixed-point rolling allowance. One request costs
	// requestCreditUnit; every elapsed nanosecond restores billedPerTick units,
	// capped at one full burst. A conditional probe may borrow one request while
	// credits is non-negative, but only one such probe may be outstanding.
	//
	// heldCredits belongs to reservations not attempted yet. Keeping it outside
	// the refillable balance prevents a slow first step of Claim from reserving
	// later requests, letting their credits refill, and then spending both sets
	// together after the boundary.
	credits     int64
	heldCredits int64
	lastRefill  time.Time
	borrowed    bool

	// secondaryCredits is a second fixed-point allowance for GitHub's
	// request-point limit. It is intentionally independent of the primary
	// allowance: an authenticated 304 refunds the latter but not the former.
	secondaryCredits     int64
	secondaryHeldCredits int64
	secondaryLastRefill  time.Time

	// Content creation has both a minute and an hour limit. A reservation must
	// fit both; held credits keep a multi-request Claim from refilling capacity
	// it has already promised to a later stage.
	contentMinuteCredits     int64
	contentMinuteHeldCredits int64
	contentMinuteLastRefill  time.Time
	contentHourCredits       int64
	contentHourHeldCredits   int64
	contentHourLastRefill    time.Time

	// wake changes whenever admission may have changed before its timer fires: a
	// window opens, a 304 refunds a slot, or a speculative probe settles.
	wake chan struct{}

	// reportEpoch changes only when BeginTick publishes a report. A request
	// records the epoch in which it actually reaches the network, so a response
	// arriving after the boundary is reported as a late resolution rather than
	// charged to the next tick.
	reportEpoch uint64
	pending     int
	report      core.RequestReport
}

func newRequestBudget(now func() time.Time) *requestBudget {
	if now == nil {
		now = time.Now
	}
	started := now()
	return &requestBudget{
		limit:                   billedPerTick,
		ordinaryLimit:           ordinaryPerTick,
		ceiling:                 defaultBudgetWindow,
		now:                     now,
		opened:                  started,
		lastRefill:              started,
		credits:                 int64(billedPerTick) * requestCreditUnit,
		secondaryCredits:        int64(secondaryBurstPoints) * secondaryCreditUnit,
		secondaryLastRefill:     started,
		contentMinuteCredits:    int64(contentMinuteBurstRequests) * contentMinuteCreditUnit,
		contentMinuteLastRefill: started,
		contentHourCredits:      int64(contentHourBurstRequests) * contentHourCreditUnit,
		contentHourLastRefill:   started,
		wake:                    make(chan struct{}),
	}
}

// budgetWaitKey marks a request that is continuing an in-memory paginated
// walk. Refusing it would throw away the pages already read and make a history
// longer than one window impossible to complete, so it waits for the next
// window instead. The first page remains immediately refusable: no progress is
// then lost and the orchestrator can retry the whole operation later.
type budgetWaitKey struct{}

func waitForRequestBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, budgetWaitKey{}, true)
}

// budgetRefuseConditionalKey marks a conditional request whose caller can
// preserve progress outside this call. Normal conditional requests wait when
// their speculative slot is occupied; refusing those would deadlock a cache at
// the cap. A page-cached claim arbitration instead returns so it can release
// its standing assignment, then revalidates the saved prefix on the next try.
type budgetRefuseConditionalKey struct{}

func refuseConditionalRequestBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, budgetRefuseConditionalKey{}, true)
}

// acquire reserves one request. Conditional requests normally wait when their
// probe slot is occupied: refusing before learning whether a request is free is
// the cache-at-cap deadlock this layer exists to prevent. The explicit
// refusable context above is limited to callers that persist their own progress.
func (b *requestBudget) acquire(ctx context.Context, conditional, wait bool) (*requestReservation, error) {
	return b.acquireCost(ctx, conditional, wait, readRequestCost, true)
}

func (b *requestBudget) acquireCost(ctx context.Context, conditional, wait bool, cost requestCost, preserveClaim bool) (*requestReservation, error) {
	deferred := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.mu.Lock()
		now := b.now()
		b.rollIfStaleLocked(now)
		b.refillLocked(now)
		if reservation := b.reserveOneWithCostLocked(conditional, cost, preserveClaim); reservation != nil {
			// Immutable once admission returns. The transport uses this to fetch
			// its credential again after a wait, so neither a rotated static token
			// nor a bounded token's deadline can go stale while this request sleeps.
			reservation.waited = deferred
			b.mu.Unlock()
			return reservation, nil
		}

		refusableConditional := ctx.Value(budgetRefuseConditionalKey{}) != nil
		if (!conditional || refusableConditional) && !wait {
			b.report.Refused++
			used, limit := b.used, b.ordinaryLimit
			kind := "ordinary"
			if conditional {
				limit = b.limit
				kind = "conditional"
			}
			secondaryReserve := 0
			if preserveClaim {
				secondaryReserve = claimSecondaryReservePoints
			}
			if b.secondaryCredits < int64(cost.secondaryPoints+secondaryReserve)*secondaryCreditUnit {
				available := b.secondaryCredits/secondaryCreditUnit - int64(secondaryReserve)
				if available < 0 {
					available = 0
				}
				b.mu.Unlock()
				return nil, fmt.Errorf("%w: GitHub secondary allowance has %d points available after reserving %d for Claim; the request needs %d and is deferred to a later tick",
					ErrRequestBudget, available, secondaryReserve, cost.secondaryPoints)
			}
			contentReserve := 0
			if preserveClaim && cost.contentRequests > 0 {
				contentReserve = claimContentReserveRequests
			}
			if b.contentMinuteCredits < int64(cost.contentRequests+contentReserve)*contentMinuteCreditUnit {
				available := b.contentMinuteCredits/contentMinuteCreditUnit - int64(contentReserve)
				if available < 0 {
					available = 0
				}
				b.mu.Unlock()
				return nil, fmt.Errorf("%w: GitHub content allowance has %d requests available in the minute bucket after reserving %d for Claim; the request needs %d and is deferred to a later tick",
					ErrRequestBudget, available, contentReserve, cost.contentRequests)
			}
			if b.contentHourCredits < int64(cost.contentRequests+contentReserve)*contentHourCreditUnit {
				available := b.contentHourCredits/contentHourCreditUnit - int64(contentReserve)
				if available < 0 {
					available = 0
				}
				b.mu.Unlock()
				return nil, fmt.Errorf("%w: GitHub content allowance has %d requests available in the hour bucket after reserving %d for Claim; the request needs %d and is deferred to a later tick",
					ErrRequestBudget, available, contentReserve, cost.contentRequests)
			}
			b.mu.Unlock()
			return nil, fmt.Errorf("%w: request window has %d of %d %s slots reserved; the call is deferred to a later tick",
				ErrRequestBudget, used, limit, kind)
		}
		if !deferred {
			b.report.Deferred++
			deferred = true
		}
		wake := b.wake
		delay := b.nextAdmissionLocked(now, conditional, cost, preserveClaim)
		b.mu.Unlock()

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			stopTimer(timer)
			return nil, ctx.Err()
		case <-wake:
			stopTimer(timer)
		case <-timer.C:
		}
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (b *requestBudget) reserveOneLocked(conditional bool) *requestReservation {
	return b.reserveOneWithCostLocked(conditional, readRequestCost, true)
}

func (b *requestBudget) reserveOneWithCostLocked(conditional bool, cost requestCost, preserveClaim bool) *requestReservation {
	windowLimit := b.ordinaryLimit
	if conditional {
		windowLimit = b.limit
	}
	secondaryReserve := 0
	contentReserve := 0
	if preserveClaim {
		secondaryReserve = claimSecondaryReservePoints
		if cost.contentRequests > 0 {
			contentReserve = claimContentReserveRequests
		}
	}
	secondaryCost := int64(cost.secondaryPoints) * secondaryCreditUnit
	contentMinuteCost := int64(cost.contentRequests) * contentMinuteCreditUnit
	contentHourCost := int64(cost.contentRequests) * contentHourCreditUnit
	if cost.secondaryPoints <= 0 || cost.contentRequests < 0 || b.used >= windowLimit ||
		b.secondaryCredits < int64(cost.secondaryPoints+secondaryReserve)*secondaryCreditUnit ||
		b.contentMinuteCredits < int64(cost.contentRequests+contentReserve)*contentMinuteCreditUnit ||
		b.contentHourCredits < int64(cost.contentRequests+contentReserve)*contentHourCreditUnit {
		return nil
	}

	borrowed := false
	if b.credits >= requestCreditUnit {
		b.credits -= requestCreditUnit
	} else if conditional && b.credits >= 0 && !b.borrowed {
		// The answer may be a free 304. At most one unknown-cost request is
		// allowed below zero, so concurrency can exceed neither the burst by
		// more than zero nor the rolling rate by more than one bounded probe.
		b.credits -= requestCreditUnit
		b.borrowed = true
		borrowed = true
	} else {
		return nil
	}

	b.used++
	b.secondaryCredits -= secondaryCost
	b.contentMinuteCredits -= contentMinuteCost
	b.contentHourCredits -= contentHourCost
	b.unattempted++
	b.heldCredits += requestCreditUnit
	b.secondaryHeldCredits += secondaryCost
	b.contentMinuteHeldCredits += contentMinuteCost
	b.contentHourHeldCredits += contentHourCost
	return &requestReservation{
		budget:     b,
		generation: b.generation,
		borrowed:   borrowed,
		cost:       cost,
	}
}

// nextAdmissionLocked returns when time alone may make admission possible.
// A wake channel handles earlier changes such as a 304 refund or BeginTick.
func (b *requestBudget) nextAdmissionLocked(now time.Time, conditional bool, cost requestCost, preserveClaim bool) time.Duration {
	delay := time.Duration(0)
	windowLimit := b.ordinaryLimit
	if conditional {
		windowLimit = b.limit
	}
	if b.used >= windowLimit {
		delay = b.opened.Add(b.ceiling).Sub(now)
	}

	needed := requestCreditUnit
	if conditional && !b.borrowed {
		// Reaching zero is enough to make one speculative conditional probe.
		needed = 0
	}
	if b.credits < needed {
		missing := needed - b.credits
		// ceil(missing / billedPerTick), expressed in nanoseconds because
		// each elapsed nanosecond restores billedPerTick fixed-point units.
		creditDelay := time.Duration((missing + int64(billedPerTick) - 1) / int64(billedPerTick))
		if creditDelay > delay {
			delay = creditDelay
		}
	}
	secondaryReserve := 0
	contentReserve := 0
	if preserveClaim {
		secondaryReserve = claimSecondaryReservePoints
		if cost.contentRequests > 0 {
			contentReserve = claimContentReserveRequests
		}
	}
	secondaryNeeded := int64(cost.secondaryPoints+secondaryReserve) * secondaryCreditUnit
	if b.secondaryCredits < secondaryNeeded {
		missing := secondaryNeeded - b.secondaryCredits
		secondaryDelay := time.Duration((missing + int64(secondaryRefillPoints) - 1) / int64(secondaryRefillPoints))
		if secondaryDelay > delay {
			delay = secondaryDelay
		}
	}
	contentMinuteNeeded := int64(cost.contentRequests+contentReserve) * contentMinuteCreditUnit
	if b.contentMinuteCredits < contentMinuteNeeded {
		missing := contentMinuteNeeded - b.contentMinuteCredits
		contentDelay := time.Duration((missing + int64(contentMinuteRefillRequests) - 1) / int64(contentMinuteRefillRequests))
		if contentDelay > delay {
			delay = contentDelay
		}
	}
	contentHourNeeded := int64(cost.contentRequests+contentReserve) * contentHourCreditUnit
	if b.contentHourCredits < contentHourNeeded {
		missing := contentHourNeeded - b.contentHourCredits
		contentDelay := time.Duration((missing + int64(contentHourRefillRequests) - 1) / int64(contentHourRefillRequests))
		if contentDelay > delay {
			delay = contentDelay
		}
	}
	if delay <= 0 {
		// Admission can also be waiting solely on an outstanding speculative
		// request. Its settlement closes wake; the small timer is a safety net
		// against a lost notification and keeps custom clocks testable.
		delay = time.Millisecond
	}
	return delay
}

// reserveLease admits a mutation only if its whole must-finish sequence fits.
// Claim uses three slots: assignment, uncached read-back, and the possible
// release that unwinds an unverifiable or lost claim (claimLeaseCosts). Each is
// taken by stage and priced by its own position; unused slots are returned when
// the lease closes.
func (b *requestBudget) reserveLease(costs ...requestCost) (*requestLease, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.rollIfStaleLocked(now)
	b.refillLocked(now)

	n := len(costs)
	primaryCost := int64(n) * requestCreditUnit
	secondaryPoints := 0
	contentRequests := 0
	for _, cost := range costs {
		if cost.secondaryPoints <= 0 || cost.contentRequests < 0 {
			b.report.Refused++
			return nil, fmt.Errorf("%w: claim request costs must be positive", ErrRequestBudget)
		}
		secondaryPoints += cost.secondaryPoints
		contentRequests += cost.contentRequests
	}
	secondaryCost := int64(secondaryPoints) * secondaryCreditUnit
	contentMinuteCost := int64(contentRequests) * contentMinuteCreditUnit
	contentHourCost := int64(contentRequests) * contentHourCreditUnit
	if n == 0 || b.used+n > b.ordinaryLimit || b.credits < primaryCost ||
		b.secondaryCredits < secondaryCost || b.contentMinuteCredits < contentMinuteCost || b.contentHourCredits < contentHourCost {
		b.report.Refused++
		return nil, fmt.Errorf("%w: claim needs %d reserved requests, %d secondary points, and %d content requests before it can write (ordinary window %d/%d)",
			ErrRequestBudget, n, secondaryPoints, contentRequests, b.used, b.ordinaryLimit)
	}
	b.used += n
	b.credits -= primaryCost
	b.secondaryCredits -= secondaryCost
	b.contentMinuteCredits -= contentMinuteCost
	b.contentHourCredits -= contentHourCost
	b.unattempted += n
	b.heldCredits += primaryCost
	b.secondaryHeldCredits += secondaryCost
	b.contentMinuteHeldCredits += contentMinuteCost
	b.contentHourHeldCredits += contentHourCost
	lease := &requestLease{reservations: make([]*requestReservation, n), taken: make([]bool, n)}
	for i, cost := range costs {
		lease.reservations[i] = &requestReservation{
			budget:     b,
			generation: b.generation,
			cost:       cost,
		}
	}
	return lease, nil
}

func (b *requestBudget) beginTick(interval time.Duration) core.RequestReport {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.refillLocked(now)
	spent := b.report
	spent.Pending = b.pending
	b.report = core.RequestReport{}
	b.pending = 0
	b.reportEpoch++
	if interval <= 0 {
		interval = defaultBudgetWindow
	}
	// The burst window is the caller's tick, not the rolling allowance's
	// 30-second refill period. Keeping those clocks separate is what prevents a
	// slow polling cadence from opening several 40-request bursts inside one
	// tick. Before the first BeginTick, newRequestBudget's default ceiling still
	// lets an adapter used without an orchestrator make progress.
	b.ceiling = interval
	b.openLocked(now)
	return spent
}

func (b *requestBudget) rollIfStaleLocked(now time.Time) {
	if b.opened.IsZero() || now.Sub(b.opened) >= b.ceiling {
		b.openLocked(now)
	}
}

func (b *requestBudget) openLocked(now time.Time) {
	b.opened = now
	b.generation++
	// Completed and already-attempted requests belong to the window in which
	// they reached the base transport. A lease stage that has not started yet is
	// different: it can still reach the network in this window, so carry it.
	b.used = b.unattempted
	b.notifyLocked()
}

func (b *requestBudget) refillLocked(now time.Time) {
	refillAllowance(now, &b.lastRefill, &b.credits,
		int64(b.limit)*requestCreditUnit-b.heldCredits, billedPerTick)
	refillAllowance(now, &b.secondaryLastRefill, &b.secondaryCredits,
		int64(secondaryBurstPoints)*secondaryCreditUnit-b.secondaryHeldCredits, secondaryRefillPoints)
	refillAllowance(now, &b.contentMinuteLastRefill, &b.contentMinuteCredits,
		int64(contentMinuteBurstRequests)*contentMinuteCreditUnit-b.contentMinuteHeldCredits, contentMinuteRefillRequests)
	refillAllowance(now, &b.contentHourLastRefill, &b.contentHourCredits,
		int64(contentHourBurstRequests)*contentHourCreditUnit-b.contentHourHeldCredits, contentHourRefillRequests)
}

func refillAllowance(now time.Time, last *time.Time, credits *int64, capacity int64, rate int) {
	if last.IsZero() {
		*last = now
		return
	}
	elapsed := now.Sub(*last)
	if elapsed <= 0 {
		return
	}
	missing := capacity - *credits
	if missing <= 0 {
		*credits = capacity
		*last = now
		return
	}
	// Compare before multiplying so an extreme clock jump cannot overflow.
	if int64(elapsed) >= (missing+int64(rate)-1)/int64(rate) {
		*credits = capacity
	} else {
		*credits += int64(elapsed) * int64(rate)
	}
	*last = now
}

func (b *requestBudget) notifyLocked() {
	close(b.wake)
	b.wake = make(chan struct{})
}

type requestReservation struct {
	budget      *requestBudget
	generation  uint64
	borrowed    bool
	cost        requestCost
	reportEpoch uint64
	attempted   bool
	waited      bool
	once        sync.Once
}

// markAttempted fixes the request to the reporting window in which it reached
// the network. Admission alone is not an attempt: a standing Retry-After can
// still return a reservation before the transport invokes its base.
func (r *requestReservation) markAttempted() {
	b := r.budget
	b.mu.Lock()
	defer b.mu.Unlock()
	// Refill while this reservation is still held. Removing the hold first would
	// let the elapsed time refill the credit that is only now being spent.
	b.refillLocked(b.now())
	r.markAttemptedLocked(b)
}

func (r *requestReservation) markAttemptedLocked(b *requestBudget) {
	if r.attempted {
		return
	}
	r.attempted = true
	if b.unattempted > 0 {
		b.unattempted--
	}
	b.heldCredits -= requestCreditUnit
	b.secondaryHeldCredits -= int64(r.cost.secondaryPoints) * secondaryCreditUnit
	b.contentMinuteHeldCredits -= int64(r.cost.contentRequests) * contentMinuteCreditUnit
	b.contentHourHeldCredits -= int64(r.cost.contentRequests) * contentHourCreditUnit
	if b.heldCredits < 0 || b.secondaryHeldCredits < 0 || b.contentMinuteHeldCredits < 0 || b.contentHourHeldCredits < 0 {
		panic("github request budget: negative held request credits")
	}
	// openLocked carried every unattempted reservation into the current used
	// count. Rebase an older reservation so a 304 settlement can return that
	// current-window slot.
	if r.generation != b.generation {
		r.generation = b.generation
	}
	r.reportEpoch = b.reportEpoch
	b.pending++
}

func (r *requestReservation) settle(billed bool) {
	r.once.Do(func() {
		b := r.budget
		b.mu.Lock()
		defer b.mu.Unlock()
		b.refillLocked(b.now())
		r.markAttemptedLocked(b)
		late := r.reportEpoch != b.reportEpoch
		if !late && b.pending > 0 {
			b.pending--
		}
		if billed && late {
			b.report.LateBilled++
		} else if billed {
			b.report.Billed++
		} else if late {
			b.report.LateUnbilled++
			b.refundPrimaryLocked(r.generation)
		} else {
			b.report.Unbilled++
			b.refundPrimaryLocked(r.generation)
		}
		if r.borrowed {
			b.borrowed = false
		}
		b.notifyLocked()
	})
}

func (r *requestReservation) releaseUnused() {
	r.once.Do(func() {
		b := r.budget
		b.mu.Lock()
		defer b.mu.Unlock()
		b.refillLocked(b.now())
		if !r.attempted {
			b.unattempted--
			b.heldCredits -= requestCreditUnit
			b.secondaryHeldCredits -= int64(r.cost.secondaryPoints) * secondaryCreditUnit
			b.contentMinuteHeldCredits -= int64(r.cost.contentRequests) * contentMinuteCreditUnit
			b.contentHourHeldCredits -= int64(r.cost.contentRequests) * contentHourCreditUnit
			if r.generation != b.generation {
				r.generation = b.generation
			}
		}
		b.refundPrimaryLocked(r.generation)
		b.refundSecondaryLocked(r.cost.secondaryPoints)
		b.refundContentLocked(r.cost.contentRequests)
		if r.attempted && r.reportEpoch == b.reportEpoch && b.pending > 0 {
			b.pending--
		}
		if r.borrowed {
			b.borrowed = false
		}
		b.notifyLocked()
	})
}

func (b *requestBudget) refundPrimaryLocked(generation uint64) {
	capacity := int64(b.limit)*requestCreditUnit - b.heldCredits
	b.credits += requestCreditUnit
	if b.credits > capacity {
		b.credits = capacity
	}
	if generation == b.generation && b.used > 0 {
		b.used--
	}
}

func (b *requestBudget) refundSecondaryLocked(points int) {
	capacity := int64(secondaryBurstPoints)*secondaryCreditUnit - b.secondaryHeldCredits
	b.secondaryCredits += int64(points) * secondaryCreditUnit
	if b.secondaryCredits > capacity {
		b.secondaryCredits = capacity
	}
}

func (b *requestBudget) refundContentLocked(requests int) {
	minuteCapacity := int64(contentMinuteBurstRequests)*contentMinuteCreditUnit - b.contentMinuteHeldCredits
	b.contentMinuteCredits += int64(requests) * contentMinuteCreditUnit
	if b.contentMinuteCredits > minuteCapacity {
		b.contentMinuteCredits = minuteCapacity
	}
	hourCapacity := int64(contentHourBurstRequests)*contentHourCreditUnit - b.contentHourHeldCredits
	b.contentHourCredits += int64(requests) * contentHourCreditUnit
	if b.contentHourCredits > hourCapacity {
		b.contentHourCredits = hourCapacity
	}
}

type requestLease struct {
	mu           sync.Mutex
	reservations []*requestReservation
	taken        []bool
	closed       bool
}

// take hands out the reservation reserved for one stage, once. Addressing by
// stage rather than in sequence is what keeps each request charged at the price
// it was admitted under when Claim skips a stage (see claimStage).
func (l *requestLease) take(stage claimStage) (*requestReservation, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := int(stage)
	if l.closed || i < 0 || i >= len(l.reservations) {
		return nil, fmt.Errorf("%w: reserved claim request sequence has no stage %d", ErrRequestBudget, i)
	}
	if l.taken[i] {
		return nil, fmt.Errorf("%w: reserved claim stage %d was already taken", ErrRequestBudget, i)
	}
	l.taken[i] = true
	return l.reservations[i], nil
}

func (l *requestLease) close() {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	l.closed = true
	// releaseUnused is guarded by the reservation's sync.Once, so walking the
	// whole lease also returns a slot whose logical request was constructed but
	// failed before reaching the transport.
	reservations := append([]*requestReservation(nil), l.reservations...)
	l.mu.Unlock()
	for _, reservation := range reservations {
		reservation.releaseUnused()
	}
}

type budgetLeaseKey struct{}

// claimLeaseBudgetKey marks work performed after Claim has already reserved
// its must-finish sequence. Supplementary arbitration reads may use the
// otherwise protected secondary floor because the assignment's release is
// already held by the lease.
type claimLeaseBudgetKey struct{}

func useClaimLeaseBudget(ctx context.Context) context.Context {
	return context.WithValue(ctx, claimLeaseBudgetKey{}, true)
}

// leasedRequest binds one reservation to one logical GitHub client call. The
// HTTP client may execute several RoundTrips while following redirects; only
// the first exchange consumes this reservation, so a redirect cannot steal a
// later slot reserved for Claim's read-back or unwind.
type leasedRequest struct {
	mu sync.Mutex

	first         *requestReservation
	firstTaken    bool
	honorRateGate bool
	attempted     bool
}

func (l *requestLease) requestContext(ctx context.Context, stage claimStage, honorRateGate bool) (context.Context, *leasedRequest, error) {
	reservation, err := l.take(stage)
	if err != nil {
		return nil, nil, err
	}
	request := &leasedRequest{first: reservation, honorRateGate: honorRateGate}
	return context.WithValue(ctx, budgetLeaseKey{}, request), request, nil
}

func (r *leasedRequest) acquire(ctx context.Context, budget *requestBudget, conditional bool, cost requestCost) (*requestReservation, error) {
	r.mu.Lock()
	if !r.firstTaken {
		r.firstTaken = true
		reservation := r.first
		r.mu.Unlock()
		return reservation, nil
	}
	r.mu.Unlock()

	// Any later exchange belongs to a redirect chain. It is still metered, but
	// waits for ordinary capacity instead of consuming the next logical Claim
	// reservation. Waiting is necessary for the release call: once assignment
	// may have landed, a local budget edge must not strand it.
	return budget.acquireCost(waitForRequestBudget(ctx), conditional, true, cost, false)
}

func (r *leasedRequest) markAttempted() {
	r.mu.Lock()
	r.attempted = true
	r.mu.Unlock()
}

func (r *leasedRequest) wasAttempted() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempted
}

// networkTimeoutState follows one logical http.Client call through redirects.
// Each transport exchange consumes from remaining, while request-budget waits
// happen before a segment starts and therefore do not consume network time.
type networkTimeoutState struct {
	mu        sync.Mutex
	remaining time.Duration
	now       func() time.Time
}

type networkTimeoutKey struct{}

type networkSegment struct {
	state   *networkTimeoutState
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
	allowed time.Duration
	once    sync.Once
}

func (s *networkTimeoutState) start(parent context.Context) (context.Context, *networkSegment, error) {
	s.mu.Lock()
	remaining := s.remaining
	s.mu.Unlock()
	if remaining <= 0 {
		return nil, nil, context.DeadlineExceeded
	}
	ctx, cancel := context.WithTimeout(parent, remaining)
	segment := &networkSegment{
		state:   s,
		ctx:     ctx,
		cancel:  cancel,
		started: s.now(),
		allowed: remaining,
	}
	return context.WithValue(ctx, networkTimeoutKey{}, s), segment, nil
}

func (s *networkSegment) deadlineExceeded() bool {
	elapsed := s.state.now().Sub(s.started)
	return elapsed >= s.allowed || s.ctx.Err() == context.DeadlineExceeded
}

func (s *networkSegment) finish() {
	s.once.Do(func() {
		elapsed := s.state.now().Sub(s.started)
		if elapsed < 0 {
			elapsed = 0
		}
		s.state.mu.Lock()
		s.state.remaining -= elapsed
		if s.state.remaining < 0 {
			s.state.remaining = 0
		}
		s.state.mu.Unlock()
		s.cancel()
	})
}

// budgetTransport sits below condCache: it must see the outgoing
// If-None-Match and the origin's 304 before the cache turns that exchange into
// a replayed 200. Transport errors are charged because the server may have seen
// the request; over-counting delays work, while under-counting breaks the bound.
type budgetTransport struct {
	next              http.RoundTripper
	budget            *requestBudget
	gate              *rateGate
	refreshCredential func(*http.Request) (*http.Request, error)
	networkTimeout    time.Duration
	networkNow        func() time.Time
}

func requestCostForMethod(method string) requestCost {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return readRequestCost
	default:
		return writeRequestCost
	}
}

func (t *budgetTransport) timeoutState(req *http.Request) *networkTimeoutState {
	if state, ok := req.Context().Value(networkTimeoutKey{}).(*networkTimeoutState); ok {
		return state
	}
	// net/http rebuilds a redirect request from the original request context.
	// The preceding response is the bridge to the clone that actually reached
	// the transport and carries this logical call's remaining network time.
	if req.Response != nil && req.Response.Request != nil {
		if state, ok := req.Response.Request.Context().Value(networkTimeoutKey{}).(*networkTimeoutState); ok {
			return state
		}
	}
	timeout := t.networkTimeout
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	now := t.networkNow
	if now == nil {
		now = time.Now
	}
	return &networkTimeoutState{remaining: timeout, now: now}
}

func (t *budgetTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}

	leased, isLeased := req.Context().Value(budgetLeaseKey{}).(*leasedRequest)
	honorRateGate := !isLeased || leased.honorRateGate
	if honorRateGate && t.gate != nil {
		if err := t.gate.check(); err != nil {
			return nil, err
		}
	}

	conditional := req.Header.Get("If-None-Match") != ""
	cost := requestCostForMethod(req.Method)
	var (
		reservation *requestReservation
		err         error
	)
	if isLeased {
		reservation, err = leased.acquire(req.Context(), t.budget, conditional, cost)
	} else {
		wait := req.Context().Value(budgetWaitKey{}) != nil
		preserveClaim := req.Context().Value(claimLeaseBudgetKey{}) == nil
		reservation, err = t.budget.acquireCost(req.Context(), conditional, wait, cost, preserveClaim)
	}
	if err != nil {
		return nil, err
	}
	if honorRateGate && t.gate != nil {
		// Admission may have waited across request windows. Honor a server
		// Retry-After another concurrent call learned while this one slept. This
		// check also closes Claim's method-entry-to-assignment gap: its first
		// leased request still observes a refusal learned in that interval.
		if gateErr := t.gate.check(); gateErr != nil {
			reservation.releaseUnused()
			return nil, gateErr
		}
	}
	if reservation.waited && t.refreshCredential != nil {
		// The outer auth boundary ran before conditional-cache lookup and budget
		// admission. Admission may deliberately sleep for a whole polling window,
		// which can outlive a bounded token or an operator's static-token rotation.
		// Fetch again at the actual point of network use. A failure still costs no
		// budget and cannot fall through to an unauthenticated request.
		req, err = t.refreshCredential(req)
		if err != nil {
			reservation.releaseUnused()
			return nil, err
		}
	}

	// Budget waits are orchestration time, not a slow GitHub exchange. Start this
	// redirect chain's next network segment only after admission, and keep it
	// alive until the body is consumed just as http.Client.Timeout would.
	networkCtx, segment, err := t.timeoutState(req).start(req.Context())
	if err != nil {
		reservation.releaseUnused()
		return nil, err
	}
	if isLeased {
		leased.markAttempted()
	}
	reservation.markAttempted()
	outbound := req.Clone(networkCtx)
	resp, err := next.RoundTrip(outbound)
	if resp != nil {
		// This is the request that produced the response. Keeping the timeout
		// state here is what lets net/http carry it across a redirect.
		resp.Request = outbound
	}
	reservation.settle(err != nil || resp == nil || resp.StatusCode != http.StatusNotModified)
	expired := segment.deadlineExceeded()
	if err != nil {
		segment.finish()
		if expired {
			return resp, context.DeadlineExceeded
		}
		return resp, err
	}
	if resp == nil {
		segment.finish()
		if expired {
			return nil, context.DeadlineExceeded
		}
		return nil, fmt.Errorf("github transport returned a nil response")
	}
	if expired {
		if resp.Body != nil {
			resp.Body.Close()
		}
		segment.finish()
		return nil, context.DeadlineExceeded
	}
	if resp.Body == nil {
		segment.finish()
	} else {
		resp.Body = &cancelBody{ReadCloser: resp.Body, segment: segment}
	}
	return resp, err
}

type cancelBody struct {
	io.ReadCloser
	segment *networkSegment
}

func (b *cancelBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	expired := b.segment.deadlineExceeded()
	if err != nil || expired {
		b.segment.finish()
	}
	if expired {
		return n, context.DeadlineExceeded
	}
	return n, err
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	b.segment.finish()
	return err
}

// BeginTick implements core.RequestBudget. The interval names both the current
// reporting window and the fallback self-roll cadence; rolling credits remain
// untouched, so faster polling cannot mint a fresh hourly allowance.
func (a *Adapter) BeginTick(interval time.Duration) core.RequestReport {
	return a.budget.beginTick(interval)
}
