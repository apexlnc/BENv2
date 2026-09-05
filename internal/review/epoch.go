package review

import (
	"sort"
	"time"
)

// SortEvents puts a change log into the one order every reader must agree on:
// (created_at, event id).
//
// GitHub documents the endpoint as ascending, but a verdict two independent
// processes have to reach the same way cannot rest on documentation — the
// tracker adapter sorts for exactly this reason (SPEC §8.2), and the
// controller replays the same log to derive the same claim epoch. Sorting in
// one exported place is what makes "the same log" true rather than hoped for.
func SortEvents(events []Event) []Event {
	out := append([]Event(nil), events...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ClaimEpoch is the id of the `assigned` event that began the principal's
// current standing assignment, or 0 when the principal is not assigned.
//
// This is SPEC §9.5's claim epoch, derived the same way the tracker adapter
// derives it: only the *start of the current streak* counts, so a login
// assigned, unassigned and assigned again reports the later id. It is why a
// controller unassignment followed by BEN reclaiming produces a new epoch, and
// therefore a new §9.7 verification base — the prerequisite this loop rests
// on. It is an assignment id and is never comparable with a milestone
// occurrence, which is a label-transition id from the same log.
func ClaimEpoch(events []Event, principal string) int64 {
	var epoch int64
	for _, ev := range events {
		if !eqFold(ev.Assignee, principal) {
			continue
		}
		switch ev.Type {
		case EventAssigned:
			if epoch == 0 {
				epoch = ev.ID
			}
		case EventUnassigned:
			epoch = 0
		}
	}
	return epoch
}

// cycleStartAtOccurrence is when the approval cycle that produced occurrence
// became complete: the event anchoring the full standing required-label set at
// that occurrence.
//
// Every count for a delivery is scoped to this instant, not to the newest
// approval visible when reconciliation happens. A human may reapprove after a
// terminal mutation landed but before its route marker did; moving the old
// occurrence into that new cycle would rewrite round-cap into revise, or make
// a completed no-progress stop review the same head again. The next published
// occurrence sees the later approval and therefore gets the fresh budget.
//
// A log with no such event yields the zero time, which scopes to everything —
// fail-closed in the direction that counts, since it can only make the
// controller stop sooner.
func cycleStartAtOccurrence(cfg Config, events []Event, occurrence int64) time.Time {
	anchor, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, occurrence)
	if !ok {
		return time.Time{}
	}
	for _, ev := range events {
		if ev.ID == anchor {
			return ev.CreatedAt
		}
	}
	return time.Time{}
}

// claimEpochAtOccurrence returns the principal's standing assignment when the
// published transition occurred. That is the source claim the milestone came
// from; the current assignment may already be a newer claim by the time a
// delayed event or scheduled reconciliation observes it.
func claimEpochAtOccurrence(events []Event, principal string, occurrence int64) (int64, bool) {
	var epoch, source int64
	var found bool
	for _, ev := range events {
		if eqFold(ev.Assignee, principal) {
			switch ev.Type {
			case EventAssigned:
				if epoch == 0 {
					epoch = ev.ID
				}
			case EventUnassigned:
				epoch = 0
			}
		}
		if ev.ID == occurrence {
			if found {
				return 0, false
			}
			source, found = epoch, true
		}
	}
	return source, found && source > 0
}

// publishedOccurrenceOnLog binds a milestone marker back to the transition
// SPEC §8.4 says it names. Author and positive syntax are not enough: a copied
// marker from another issue must not become this issue's delivery key.
func publishedOccurrenceOnLog(events []Event, occurrence int64, tracker string) bool {
	for _, ev := range events {
		if ev.ID != occurrence {
			continue
		}
		return ev.Type == EventUnlabeled &&
			eqFold(ev.Actor, tracker) &&
			(eqFold(ev.Label, "ben:claimed") || eqFold(ev.Label, "ben:running"))
	}
	return false
}

// unassignedAfterClaim reports whether the principal was unassigned after the
// assignment event that established claim. Both facts live in one ordered
// event log, so their order is exact even when GitHub gives them the same
// second-granularity timestamp. The actor is deliberately not constrained: a
// human handing the claim back has the same effect the route needs.
func unassignedAfterClaim(events []Event, principal string, claim int64) bool {
	afterClaim := false
	for _, ev := range events {
		if !afterClaim {
			if ev.ID == claim && ev.Type == EventAssigned && eqFold(ev.Assignee, principal) {
				afterClaim = true
			}
			continue
		}
		if ev.Type == EventUnassigned && eqFold(ev.Assignee, principal) {
			return true
		}
	}
	return false
}

// approvalEpochAtOccurrence returns the workspace-cycle anchor that was
// standing when occurrence happened. The anchor is the greatest event id among
// the current applications of every required label — the exact identity
// trackerCycles gives the remote workspace strategy. Binding reviews and
// routes to this event prevents a delayed artifact from acting on a fresh
// approval made by reapplying *any* member of the set.
func approvalEpochAtOccurrence(events []Event, required []string, occurrence int64) (int64, bool) {
	epochs := make([]int64, len(required))
	var source int64
	var sourceStanding, found bool
	for _, ev := range events {
		if i := requiredLabel(required, ev.Label); i >= 0 {
			switch ev.Type {
			case EventLabeled:
				epochs[i] = ev.ID
			case EventUnlabeled:
				epochs[i] = 0
			}
		}
		if ev.ID == occurrence {
			if found {
				return 0, false
			}
			source, sourceStanding = completeApprovalEpoch(epochs)
			found = true
		}
	}
	return source, found && sourceStanding
}

// labelEpochAtOccurrence returns one label's standing application at an
// occurrence. New artifacts never use this narrower identity; it exists only
// to recognize route intents written before the workspace-cycle anchor was
// widened from QueueLabel to the complete required-label set.
func labelEpochAtOccurrence(events []Event, label string, occurrence int64) (int64, bool) {
	var epoch, source int64
	var standing, sourceStanding, found bool
	for _, ev := range events {
		if eqFold(ev.Label, label) {
			switch ev.Type {
			case EventLabeled:
				epoch, standing = ev.ID, true
			case EventUnlabeled:
				epoch, standing = 0, false
			}
		}
		if ev.ID == occurrence {
			if found {
				return 0, false
			}
			source, sourceStanding = epoch, standing
			found = true
		}
	}
	return source, found && sourceStanding && source > 0
}

// approvalChangesAfterEpoch validates epoch as the full required-label anchor
// at that event and reports whether any member was later removed and whether a
// later complete replacement set was formed. Events are already in the
// controller's canonical order, so this remains exact when GitHub timestamps
// every change to the same second.
func approvalChangesAfterEpoch(events []Event, required []string, epoch int64) (removed bool, newer int64, valid bool) {
	if source, ok := approvalEpochAtOccurrence(events, required, epoch); !ok || source != epoch {
		return false, 0, false
	}
	epochs := make([]int64, len(required))
	after := false
	for _, ev := range events {
		i := requiredLabel(required, ev.Label)
		if i >= 0 {
			switch ev.Type {
			case EventLabeled:
				epochs[i] = ev.ID
			case EventUnlabeled:
				epochs[i] = 0
			}
		}
		if ev.ID == epoch {
			after = true
			continue
		}
		if !after || i < 0 {
			continue
		}
		if ev.Type == EventUnlabeled {
			removed = true
		}
		if anchor, standing := completeApprovalEpoch(epochs); standing && anchor != epoch {
			return removed, anchor, true
		}
	}
	return removed, 0, true
}

func completeApprovalEpoch(epochs []int64) (int64, bool) {
	if len(epochs) == 0 {
		return 0, false
	}
	var anchor int64
	for _, epoch := range epochs {
		if epoch <= 0 {
			return 0, false
		}
		if epoch > anchor {
			anchor = epoch
		}
	}
	return anchor, true
}

func requiredLabel(required []string, label string) int {
	for i, want := range required {
		if eqFold(want, label) {
			return i
		}
	}
	return -1
}
