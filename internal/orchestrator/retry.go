package orchestrator

import (
	"fmt"
	"hash/fnv"
	"time"
)

// baseBackoff is the first retry delay; each further attempt doubles it
// (SPEC §9.6).
const baseBackoff = 10 * time.Second

// jitterFraction is the width of the spread applied to each delay, as a
// fraction of the delay itself. Enough to de-synchronize a fleet, small
// enough that the sequence is still recognizably exponential.
const jitterFraction = 0.2

// backoffDelay is `min(10s · 2^(attempt−1), max)` plus deterministic jitter
// (SPEC §9.6).
//
// The jitter is FNV-1a over the issue identifier and attempt, not a random
// draw: the sequence has to be exactly reproducible in tests, and a daemon
// that restarts mid-backoff should compute the same delay it would have
// before. Two daemons on the same issue still spread, because they spread by
// *issue*, which is what the collision is about.
func backoffDelay(identifier string, attempt int, max time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := baseBackoff
	for i := 1; i < attempt; i++ {
		delay *= 2
		if delay >= max {
			delay = max
			break
		}
	}
	if delay > max {
		delay = max
	}

	// Symmetric around the delay: ±jitterFraction, so the mean is unchanged.
	h := fnv.New64a()
	fmt.Fprintf(h, "%s#%d", identifier, attempt)
	// [0, 2^53) → [0, 1) without floating-point surprises at the top end.
	unit := float64(h.Sum64()>>11) / float64(uint64(1)<<53)
	spread := float64(delay) * jitterFraction
	jittered := float64(delay) - spread + 2*spread*unit

	out := time.Duration(jittered)
	if out < 0 {
		out = 0
	}
	return out
}

// continuationDelay is the pause before re-dispatching a clean exit that
// produced no publish evidence (SPEC §9.6, "~1 s"). Short because nothing is
// wrong — the agent simply has more to do.
const continuationDelay = time.Second

// continuable reports whether a clean exit with no publish evidence may be
// re-dispatched on the continuation track (SPEC §9.6).
func (o *Orchestrator) continuable(r *Record) bool {
	return r.Turns < o.limits().MaxTurns
}

// attemptsRemain measures max_attempts from the last re-queue, not from the
// beginning of time (SPEC §9.8, 2026-08-08 amendment): two of the three ways
// into needs-review are exhausted bounds, so a re-queue that did not restore
// them would re-park immediately and the human gesture would mean nothing.
func (o *Orchestrator) attemptsRemain(r *Record) bool {
	return r.Attempt-r.attemptBase < o.limits().MaxAttempts
}

// restoreBudgets is what a human re-queue grants (SPEC §9.8).
func (r *Record) restoreBudgets() {
	r.Turns = 0
	r.costUSD = 0
	r.attemptBase = r.Attempt
	// lastOutcome is deliberately kept: it is what the next attempt's prompt
	// reports as run.previous_outcome (SPEC §5.6), and a
	// budget-exceeded retry told "previous outcome succeeded" would be
	// working from a false account of why it is here. FailureReason remains the
	// record's most recent §7.3 failure for status and milestone reporting.
}
