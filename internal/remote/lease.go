package remote

import "strconv"

// What `limits.max_concurrent_agents` counts, respecified for a remote
// substrate.
//
// v1's count is unchanged and stays where it is (orchestrator.runningCount): a
// local agent process, plus the records on the way to one, minus a verifying
// record whose execution domain is confirmed quiet. That number is about the
// daemon's own host — the scarce thing is a subprocess and the CPU under it —
// and the exclusion at the end follows from it, since the §9.7 check reads git
// and the tracker with nothing executing.
//
// On a remote substrate the scarce thing is somewhere else, and the same cap has
// to be counted against it: a backend sandbox costs while it is allocated, not
// while BEN is talking to it. Two consequences, and they pull in opposite
// directions from v1's rule.
//
// A held lease costs even with no run in it. A suspended workspace is still a
// reservation against the backend's capacity, and a daemon that counted only
// live runs could hold every sandbox its quota allows while reporting itself
// idle.
//
// A run costs until its termination is *confirmed*, not until its stream ends.
// This is v1's exclusion read honestly on the other substrate: locally the
// verifying record stops counting once its execution domain is proven quiet, and
// remotely the equivalent proof is domain quiet (Status.Termination). An
// unconfirmed run may still be executing, and the workspace it may still be
// executing in is exactly the resource being capped.

// LeaseState is what one claim cycle costs against the concurrency cap.
type LeaseState uint8

const (
	// LeaseNone — nothing is held. The zero value, and the only one that costs
	// nothing, which is the right way round: a state nobody computed must not
	// free capacity.
	LeaseNone LeaseState = iota
	// LeaseHeld — a workspace/sandbox is reserved with no unquiet run in it.
	// Includes a suspended workspace: suspension releases the warm sandbox, not
	// the reservation.
	LeaseHeld
	// LeaseRunning — a dispatched run whose termination is unconfirmed.
	LeaseRunning
)

func (s LeaseState) String() string {
	switch s {
	case LeaseNone:
		return "none"
	case LeaseHeld:
		return "held"
	case LeaseRunning:
		return "running"
	default:
		return "LeaseState(" + strconv.Itoa(int(s)) + ")"
	}
}

// Cost is what this state counts for against `limits.max_concurrent_agents`.
func (s LeaseState) Cost() int {
	if s == LeaseNone {
		return 0
	}
	return 1
}

// LeaseStateOf reads one claim cycle's cost off its durable record and the last
// status observed for it.
//
// The status is passed in rather than fetched, for core.Termination's reason:
// this is arithmetic over an observation the caller already made, and a function
// that went and asked would be turning a count into a fan-out of backend reads
// once per tick per record.
//
// A record with no sandbox holds nothing — Reserve refuses an incomplete
// identity, so that shape only exists for a claim nothing has acquired for. A
// record that was reserved but never dispatched holds the workspace and no run.
func LeaseStateOf(rec Record, st Status) LeaseState {
	if rec.Identity.SandboxID == "" {
		return LeaseNone
	}
	if rec.Dispatched && !MayReuse(st) {
		return LeaseRunning
	}
	return LeaseHeld
}

// Active is the v2 concurrency count over a set of claim cycles: the number of
// backend runs and leases this daemon is holding.
func Active(states []LeaseState) int {
	n := 0
	for _, s := range states {
		n += s.Cost()
	}
	return n
}
