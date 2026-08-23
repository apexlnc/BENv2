package orchestrator

import (
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Recovery classification is the pure decision half of SPEC §9.10. The I/O
// driver reads the tracker, verifier, and run marker; this file decides what
// those facts mean. Keeping the two apart makes the four gates and the
// projection table enumerable without a tracker, git repository, or process.

// recoveryCandidate is the current tracker fact plus the two workflow-policy
// questions the driver answers from one configuration snapshot. InPartition is
// independent of Active because gate 1 precedes gate 4: a terminal issue is
// disposed and released even if its required labels were also removed.
type recoveryCandidate struct {
	issue       core.Issue
	active      bool
	inPartition bool
}

// recoveryMarkerState is the workspace precondition after the marker read and,
// for an identified run, its probe (SPEC §9.10). It is deliberately not the
// workspace package's on-disk enum: MarkerIdentified still needs a probe before
// it answers this decision.
type recoveryMarkerState uint8

const (
	// The zero value authorizes nothing. A driver that forgets to resolve the
	// marker gets a blocked verdict, never a workspace treated as free.
	recoveryMarkerUnresolved recoveryMarkerState = iota
	// Absent, or identified and positively confirmed gone.
	recoveryMarkerFree
	// Identified, but absence was not confirmed (including a probe error).
	recoveryMarkerPossiblyLive
	// Present without evidence that can answer whether launch succeeded.
	recoveryMarkerUnknownLaunch
)

// recoveryAction is the local action the recovery driver adopts. Tracker
// projection repair is carried separately on recoveryVerdict because a
// possibly-live workspace blocks reuse/disposal/release, not tracker-only
// effects.
type recoveryAction uint8

const (
	// Like every safety-asymmetric enum in BEN, zero is non-authorizing.
	recoveryActionUnknown recoveryAction = iota
	// A required read produced no answer. Adopt an inert record and retry the
	// reads; do not project anything from an error or VerdictUnknown.
	recoveryActionBlocked
	// A previous run may still be live. Retain everything and probe again.
	recoveryActionWait
	// The assignment landed before its first projection. The claim is ours and
	// nothing has been announced about it, so the record is adopted as an
	// **unapproved claim**: `queued` with a verified claim, which is the shape
	// beginApproval drives (SPEC §9.5). Attempt floor 1.
	//
	// Not "dispatch", and the rename is the whole point. The window this
	// classifies — assignment landed, first projection did not — is exactly the
	// window §9.5's content check occupies, so a recovery that dispatched from
	// here would run content nobody established was approved: approve, edit,
	// crash, and the edit reaches the agent past the control that exists to stop
	// it. It is also why this verdict projects nothing and posts no milestone.
	// Announcing `ben:claimed` for a claim that may be about to park for
	// reapproval is the same mistake onClaimed stopped making, and the ordinary
	// path already owes both writes once the check passes.
	recoveryActionApprove
	// Work may already exist: re-enter backoff at attempt >= 2.
	//
	// §9.5 is covered here by the route rather than by the verdict: a backoff
	// record reaches an agent only through the §9.6 re-fetch, and that is a
	// dispatch decision point which runs the content check itself
	// (onTimerFetched). Load-bearing, so it is written down — a driver that
	// dispatched a recovered backoff record directly would bypass the check.
	recoveryActionBackoff
	// Retain the claim and workspace for a human.
	recoveryActionPark
	// Published: retain the claim as a held-claim record and dispose workspace.
	recoveryActionHold
	// Release the claim while keeping the workspace.
	recoveryActionReleaseKeep
	// Dispose the workspace, then release the claim.
	recoveryActionReleaseDispose
	// The issue is gone from the tracker entirely — deleted or transferred. Not a
	// classification of the issue but the absence of one to classify: there is no
	// claim of ours left to release. Never produced by classifyRecovery, which is
	// handed facts about an issue that exists; only the driver's refetch can
	// discover it.
	recoveryActionGone
)

// recoveryVerdict is declarative so the I/O driver cannot have to replay the
// classifier's reasoning. project distinguishes "do not touch labels" from a
// projection to StateLabelNone. attemptFloor is meaningful only for dispatch
// and backoff: 1 for an unprojected claim and 2 for a recovered path on which
// work may already exist. A possibly-live marker preserves it as part of the
// underlying verdict while changing the action to wait; wait itself dispatches
// nothing.
type recoveryVerdict struct {
	action       recoveryAction
	project      bool
	stateLabel   core.StateLabel
	milestone    core.Milestone
	attemptFloor int
	cycleAnchor  int64
	operatorErr  bool
	epochFault   bool
	detail       string
}

// classifyRecovery applies SPEC §9.10's four gates and exhaustive projection
// table, then applies the workspace precondition. It assumes the reads
// themselves returned; a driver-side read error never becomes an empty event
// log or an evidence verdict.
func classifyRecovery(
	candidate recoveryCandidate,
	events []core.ClaimEvent,
	evidence Verdict,
	marker recoveryMarkerState,
	principal string,
) recoveryVerdict {
	if marker == recoveryMarkerUnresolved {
		return recoveryVerdict{action: recoveryActionBlocked}
	}

	base := classifyRecoveryFacts(candidate, events, evidence, principal)

	switch marker {
	case recoveryMarkerFree:
		return base
	case recoveryMarkerPossiblyLive:
		if base.action == recoveryActionBlocked {
			return base
		}
		// Preserve project/stateLabel/milestone: tracker-only repair is not
		// suppressed by a possibly-live workspace (SPEC §9.10 precondition).
		base.action = recoveryActionWait
		return base
	case recoveryMarkerUnknownLaunch:
		// There is no later probe that can resolve this state. Park instead of
		// waiting forever or guessing that the workspace is free.
		return recoveryVerdict{
			action:      recoveryActionPark,
			project:     true,
			stateLabel:  core.StateLabelNeedsReview,
			milestone:   core.MilestoneNeedsReview,
			cycleAnchor: base.cycleAnchor,
		}
	default:
		return recoveryVerdict{action: recoveryActionBlocked}
	}
}

func classifyRecoveryFacts(
	candidate recoveryCandidate,
	events []core.ClaimEvent,
	evidence Verdict,
	principal string,
) recoveryVerdict {
	anchor := claimCycleAnchor(events, principal)

	// Gate 1: terminal now, or closed and reopened inside this claim cycle.
	if !candidate.active || closedInCycle(events, anchor) {
		return recoveryVerdict{action: recoveryActionReleaseDispose, cycleAnchor: anchor}
	}

	// Gate 2: the set comparison is case-insensitive, as every tracker identity
	// comparison is. A winner other than us releases only our own assignment;
	// an unorderable race fails closed like gate 3.
	if !solePrincipal(candidate.issue.Assignees, principal) {
		switch arbitrateRecovery(events, candidate.issue.Assignees, principal) {
		case recoveryArbitrationOurs:
			// Retain and continue classifying.
		case recoveryArbitrationOther:
			return recoveryVerdict{action: recoveryActionReleaseKeep, cycleAnchor: anchor}
		case recoveryArbitrationUnorderable:
			return parkRecovery(anchor, true)
		default:
			return recoveryVerdict{action: recoveryActionBlocked, cycleAnchor: anchor}
		}
	}

	// Gate 3: current state says the claim is ours, but the returned log cannot
	// account for it. The log spoke; its silence is not evidence to release.
	if anchor == 0 {
		return parkRecovery(0, true)
	}

	// Gate 4: active but no longer in this workflow's label partition.
	if !candidate.inPartition {
		return recoveryVerdict{action: recoveryActionReleaseKeep, cycleAnchor: anchor}
	}

	projection := replayRecoveryProjection(events, anchor)
	if projection.standing {
		if !projection.effectiveKnown {
			return parkRecovery(anchor, true)
		}
		switch projection.effective {
		case core.StateLabelFailed:
			return recoveryVerdict{
				action:      recoveryActionReleaseKeep,
				project:     true,
				stateLabel:  core.StateLabelFailed,
				milestone:   core.MilestoneFailed,
				cycleAnchor: anchor,
			}
		case core.StateLabelNeedsReview:
			return parkRecovery(anchor, false)
		case core.StateLabelClaimed, core.StateLabelRunning:
			return classifyActiveRecoveryProjection(anchor, evidence)
		default:
			return parkRecovery(anchor, true)
		}
	}

	// No ben:* label was ever projected in this claim cycle: the daemon died
	// after assignment and before ben:claimed. Evidence from an earlier cycle
	// is deliberately irrelevant here.
	//
	// The claim is ours and unannounced, which is precisely the state a live
	// claim occupies while §9.5's content check is out — and that check can span
	// several ticks, because a change-log or content read that failed is retried
	// with the claim retained. So this is not a dispatch: it is the same
	// unapproved claim the loop already knows how to drive, handed back to it.
	// Projection and milestone are deliberately absent for the same reason
	// (see recoveryActionApprove).
	if !projection.sawLabeled {
		return recoveryVerdict{
			action:       recoveryActionApprove,
			attemptFloor: 1,
			cycleAnchor:  anchor,
		}
	}

	// With no standing projection, the identity of the last removed label is
	// the discriminator. An unknown/malformed transition parks rather than
	// inventing a tenth table row in a permissive direction.
	if !projection.lastWasRemoval || !projection.lastRemovedKnown {
		return parkRecovery(anchor, true)
	}
	switch projection.lastRemoved {
	case core.StateLabelNeedsReview:
		return backoffRecovery(anchor)
	case core.StateLabelFailed:
		return recoveryVerdict{action: recoveryActionReleaseKeep, cycleAnchor: anchor}
	case core.StateLabelClaimed, core.StateLabelRunning:
		switch evidence {
		case VerdictPublished:
			return holdRecovery(anchor)
		case VerdictIncomplete, VerdictContradicted:
			return parkRecovery(anchor, false)
		default:
			return recoveryVerdict{action: recoveryActionBlocked, cycleAnchor: anchor}
		}
	default:
		return parkRecovery(anchor, true)
	}
}

// classifyRecoveryTrackerGates resolves only gates 1–4. Recovery calls it
// before reading provider-owned claim-base or run-marker state, so terminal,
// contested, unaccountable, and out-of-partition claims retain §9.10's
// tracker-first ordering. The projection classifier above deliberately repeats
// these gates; the cross-check tests keep the two answers identical.
func classifyRecoveryTrackerGates(
	candidate recoveryCandidate,
	events []core.ClaimEvent,
	principal string,
) (recoveryVerdict, bool, int64) {
	anchor := claimCycleAnchor(events, principal)
	if !candidate.active || closedInCycle(events, anchor) {
		return recoveryVerdict{action: recoveryActionReleaseDispose, cycleAnchor: anchor}, true, anchor
	}
	if !solePrincipal(candidate.issue.Assignees, principal) {
		switch arbitrateRecovery(events, candidate.issue.Assignees, principal) {
		case recoveryArbitrationOurs:
		case recoveryArbitrationOther:
			return recoveryVerdict{action: recoveryActionReleaseKeep, cycleAnchor: anchor}, true, anchor
		case recoveryArbitrationUnorderable:
			return parkRecovery(anchor, true), true, anchor
		default:
			return recoveryVerdict{action: recoveryActionBlocked, cycleAnchor: anchor}, true, anchor
		}
	}
	if anchor == 0 {
		return parkRecovery(0, true), true, 0
	}
	if !candidate.inPartition {
		return recoveryVerdict{action: recoveryActionReleaseKeep, cycleAnchor: anchor}, true, anchor
	}
	return recoveryVerdict{}, false, anchor
}

// classifyRecoveryClaimBase is the safety precondition between tracker gates
// 1–4 and the existing projection/evidence table. resolved=false is the sole
// authorizing answer: a matching positive pinned pair may continue. Every other
// state resolves without asking §9.7.
func classifyRecoveryClaimBase(
	anchor int64,
	state core.ClaimBase,
	readErr error,
	marker core.RunMarker,
	markerErr error,
	runningEvidence bool,
) (recoveryVerdict, bool) {
	if readErr != nil {
		return epochFaultRecovery(anchor, "claim-base state is unreadable"), true
	}
	if state.State == core.ClaimBasePinned && state.Epoch == anchor && state.BaseSHA != "" {
		return recoveryVerdict{}, false
	}
	if state.State == core.ClaimBasePending && state.Epoch == anchor {
		if runningEvidence {
			return epochFaultRecovery(anchor, "pending claim epoch has current-cycle running evidence"), true
		}
		if markerErr != nil {
			return recoveryVerdict{action: recoveryActionBlocked, cycleAnchor: anchor}, true
		}
		switch marker.State {
		case core.RunMarkerAbsent:
			return recoveryVerdict{action: recoveryActionApprove, attemptFloor: 1, cycleAnchor: anchor}, true
		case core.RunMarkerIdentified, core.RunMarkerUnknownLaunch:
			return epochFaultRecovery(anchor, "pending claim epoch has a run-marker entry"), true
		default:
			// An unreadable or future state is not a successfully read marker
			// entry. It authorizes neither prepare nor a sticky contradiction.
			return recoveryVerdict{action: recoveryActionBlocked, cycleAnchor: anchor}, true
		}
	}
	return epochFaultRecovery(anchor, "claim-base state is absent, malformed, or belongs to another epoch"), true
}

// currentCycleRunningEvidence is intentionally historical, not a replay of the
// standing projection. Once ben:running was ever labeled in this assignment
// cycle, a pending epoch is impossible under the required ordering even if a
// later transition removed the label.
func currentCycleRunningEvidence(events []core.ClaimEvent, anchor int64) bool {
	if anchor <= 0 {
		return false
	}
	anchored := false
	for _, event := range events {
		if !anchored {
			// ClaimHistory is ordered by (At, ID); IDs break timestamp ties but
			// are not a substitute for that order across timestamps. The epoch
			// identifies the exact assignment entry after which evidence counts.
			anchored = event.Kind == core.ClaimEventAssigned && event.ID == anchor
			continue
		}
		if event.Kind != core.ClaimEventLabeled {
			continue
		}
		label, known := recoveryStateLabel(event.Subject)
		if known && label == core.StateLabelRunning {
			return true
		}
	}
	return false
}

func epochFaultRecovery(anchor int64, detail string) recoveryVerdict {
	v := parkRecovery(anchor, true)
	v.epochFault = true
	v.detail = detail
	return v
}

func classifyActiveRecoveryProjection(anchor int64, evidence Verdict) recoveryVerdict {
	switch evidence {
	case VerdictPublished:
		return holdRecovery(anchor)
	case VerdictIncomplete, VerdictContradicted:
		return backoffRecovery(anchor)
	default:
		return recoveryVerdict{action: recoveryActionBlocked, cycleAnchor: anchor}
	}
}

func parkRecovery(anchor int64, operatorErr bool) recoveryVerdict {
	return recoveryVerdict{
		action:      recoveryActionPark,
		project:     true,
		stateLabel:  core.StateLabelNeedsReview,
		milestone:   core.MilestoneNeedsReview,
		cycleAnchor: anchor,
		operatorErr: operatorErr,
	}
}

func backoffRecovery(anchor int64) recoveryVerdict {
	return recoveryVerdict{
		action:       recoveryActionBackoff,
		project:      true,
		stateLabel:   core.StateLabelClaimed,
		milestone:    core.MilestoneClaimed,
		attemptFloor: 2,
		cycleAnchor:  anchor,
	}
}

func holdRecovery(anchor int64) recoveryVerdict {
	return recoveryVerdict{
		action:      recoveryActionHold,
		project:     true,
		stateLabel:  core.StateLabelNone,
		milestone:   core.MilestonePublished,
		cycleAnchor: anchor,
	}
}

func solePrincipal(assignees []string, principal string) bool {
	set := make(map[string]struct{}, len(assignees))
	for _, login := range assignees {
		set[strings.ToLower(login)] = struct{}{}
	}
	if len(set) != 1 {
		return false
	}
	_, ok := set[strings.ToLower(principal)]
	return ok
}

type recoveryArbitration uint8

const (
	recoveryArbitrationUnknown recoveryArbitration = iota
	recoveryArbitrationOurs
	recoveryArbitrationOther
	recoveryArbitrationUnorderable
)

type recoveryAssignment struct {
	at time.Time
	id int64
}

type recoveryContender struct {
	login      string
	assignment recoveryAssignment
}

func (a recoveryAssignment) before(b recoveryAssignment) bool {
	if !a.at.Equal(b.at) {
		return a.at.Before(b.at)
	}
	return a.id < b.id
}

func (a recoveryAssignment) sameOrder(b recoveryAssignment) bool {
	return a.at.Equal(b.at) && a.id == b.id
}

// arbitrateRecovery replays the normalized assignment log using §8.4's
// first-standing-assignment rule. Unlike claimCycleAnchor, a duplicate assign
// while already standing does not restart a party's tenure: the former asks
// when a race began, while the latter deliberately names the most recent event
// that establishes our claim cycle.
func arbitrateRecovery(events []core.ClaimEvent, assignees []string, principal string) recoveryArbitration {
	held := map[string]recoveryAssignment{}
	seen := map[string]bool{}
	for _, event := range events {
		login := strings.ToLower(event.Subject)
		if login == "" {
			continue
		}
		switch event.Kind {
		case core.ClaimEventAssigned:
			seen[login] = true
			if _, standing := held[login]; !standing {
				held[login] = recoveryAssignment{at: event.At, id: event.ID}
			}
		case core.ClaimEventUnassigned:
			seen[login] = true
			delete(held, login)
		}
	}

	var contenders []recoveryContender
	current := map[string]bool{}
	for _, assignee := range assignees {
		login := strings.ToLower(assignee)
		if current[login] {
			continue
		}
		current[login] = true

		assigned, standing := held[login]
		if !standing {
			if seen[login] {
				// The log is newer than the candidate: this party withdrew.
				continue
			}
			return recoveryArbitrationUnorderable
		}
		contenders = append(contenders, recoveryContender{login: login, assignment: assigned})
	}

	if len(contenders) == 0 {
		return recoveryArbitrationUnorderable
	}
	best := contenders[0].assignment
	for _, contender := range contenders[1:] {
		if contender.assignment.before(best) {
			best = contender.assignment
		}
	}
	// Decide ambiguity only at the minimum, after seeing every contender. A
	// duplicate order among later assignments cannot obscure an earlier unique
	// winner, and assignee slice order therefore supplies no evidence.
	var winner string
	for _, contender := range contenders {
		if !contender.assignment.sameOrder(best) {
			continue
		}
		if winner != "" && contender.login != winner {
			return recoveryArbitrationUnorderable
		}
		winner = contender.login
	}
	switch {
	case equalFold(winner, principal):
		return recoveryArbitrationOurs
	default:
		return recoveryArbitrationOther
	}
}

type recoveryProjection struct {
	standing         bool
	effective        core.StateLabel
	effectiveKnown   bool
	sawLabeled       bool
	lastWasRemoval   bool
	lastRemoved      core.StateLabel
	lastRemovedKnown bool
}

type standingRecoveryLabel struct {
	label core.StateLabel
	known bool
	order int
}

// replayRecoveryProjection implements SPEC §9.10 step 3 from ordered events,
// never from Issue.Labels. An interrupted add-before-remove projection can
// leave several labels standing; the most recently labeled one is effective.
func replayRecoveryProjection(events []core.ClaimEvent, anchor int64) recoveryProjection {
	standing := map[string]standingRecoveryLabel{}
	var out recoveryProjection

	for order, event := range events {
		if event.ID < anchor || !isRecoveryStateLabel(event.Subject) {
			continue
		}
		key := strings.ToLower(event.Subject)
		label, known := recoveryStateLabel(event.Subject)
		switch event.Kind {
		case core.ClaimEventLabeled:
			out.sawLabeled = true
			out.lastWasRemoval = false
			standing[key] = standingRecoveryLabel{label: label, known: known, order: order}
		case core.ClaimEventUnlabeled:
			delete(standing, key)
			out.lastWasRemoval = true
			out.lastRemoved = label
			out.lastRemovedKnown = known
		}
	}

	latest := -1
	for _, label := range standing {
		if label.order <= latest {
			continue
		}
		latest = label.order
		out.standing = true
		out.effective = label.label
		out.effectiveKnown = label.known
	}
	return out
}

func isRecoveryStateLabel(subject string) bool {
	return strings.HasPrefix(strings.ToLower(subject), "ben:")
}

func recoveryStateLabel(subject string) (core.StateLabel, bool) {
	for _, label := range [...]core.StateLabel{
		core.StateLabelClaimed,
		core.StateLabelRunning,
		core.StateLabelNeedsReview,
		core.StateLabelFailed,
	} {
		if equalFold(subject, "ben:"+string(label)) {
			return label, true
		}
	}
	return core.StateLabelNone, false
}
