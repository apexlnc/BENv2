package review

import (
	"fmt"
	"sort"
	"time"
)

// StepKind is the closed set of things the controller may do. There is no
// approve, no merge, no close, no label *addition* other than the optional
// informational one, and no `ben:*` write: the enum is the permission model
// written down where the code has to obey it.
type StepKind string

const (
	// StepNothing is the common answer and always carries a reason.
	StepNothing StepKind = "nothing"
	// StepReview reviews Head and publishes a COMMENT review carrying the
	// marker this step describes.
	StepReview StepKind = "review"
	// StepUnassign hands the claim back to BEN by removing exactly one
	// assignee. The only route that continues automation.
	StepUnassign StepKind = "unassign"
	// StepRevoke removes the human's required label — revocation, not
	// approval — and optionally adds an informational one.
	StepRevoke StepKind = "revoke"
	// StepRecordIntent posts the durable terminal decision required before a
	// no-review stop may mutate the issue.
	StepRecordIntent StepKind = "record-intent"
	// StepRecordRoute posts the route marker. Always last, so its presence
	// means the mutation before it landed.
	StepRecordRoute StepKind = "record-route"
)

// ReviewStateCommented is the only review state the controller publishes or
// trusts as its own record. `APPROVE` is forbidden outright (a human code
// owner's approval is what branch protection requires), and a
// `CHANGES_REQUESTED` review would block merge from a machine — a gate the
// controller has no authority to hold.
const ReviewStateCommented = "COMMENTED"

// Step is one executable move plus the whole reason it was chosen. Why is not
// decoration: the controller's normal answer is "nothing", and a scheduled
// reconciliation that says nothing without saying why is indistinguishable
// from one that is broken.
type Step struct {
	Kind StepKind
	Why  string

	// PR is the subject; the zero value on StepNothing.
	PR PullRequest

	Occurrence int64
	Claim      int64
	Head       string
	// Approval is the queue-label event standing at the occurrence. It is set on
	// StepRecordIntent, where it is the durable pre-mutation fact recovery needs,
	// and on StepReview, where it is the **workspace-cycle anchor** a remote
	// review runs in (#204, remotews): the sandbox a review judges is selected by
	// the standing human approval, so a reviewer dispatched without it would have
	// to guess which tree the milestone is about.
	Approval int64
	// ReviewerProfile is the operator-defined invocation selected for this
	// review. Empty is the backward-compatible one-argv mode.
	ReviewerProfile string

	// Outcome is set on terminal route and route-artifact steps.
	Outcome Outcome
	// Principal is set on StepUnassign — the one login that may be removed.
	Principal string
	// RemoveLabel and AddLabel are set on StepRevoke. AddLabel may be empty.
	RemoveLabel string
	AddLabel    string
}

// Reduce decides the next move for one issue from one observation. It is pure:
// same snapshot, same step, no clock and no I/O.
//
// The driver executes the step, re-observes, and calls Reduce again until it
// gets StepNothing. That loop is the whole recovery story — every "crash
// between X and Y" row of #11's table is just a snapshot in which X is visible
// and Y is not, and there is only one place that decides what to do about it.
func Reduce(cfg Config, s Snapshot) Step {
	if err := cfg.Validate(); err != nil {
		return nothing("%v", err)
	}
	// The current profile label is part of the authorization for every
	// controller write, including crash recovery. Validate it before repairing a
	// terminal intent or completed route; otherwise an unknown or ambiguous
	// selection could still revoke a label or publish a route marker.
	selectedProfile, err := cfg.SelectReviewerProfile(s.Issue.Labels)
	if err != nil {
		return nothing("issue #%d cannot select a reviewer invocation: %v", cfg.Issue, err)
	}

	triggers := publishedMilestones(cfg, s.Comments)
	events := SortEvents(s.Events)
	all := controllerReviews(cfg, events, s.Reviews, time.Time{})

	// Intent recovery precedes every current-state gate, including closure. The
	// intent was validated before mutation and carries the exact terminal
	// subject; current PR state can no longer reverse it.
	if step, ok := resumeTerminalIntent(cfg, s, events); ok {
		return step
	}

	// A newer publication supersedes unfinished work, but not a completed
	// mutation whose route marker was interrupted. Repair the oldest such route
	// before advancing to the newest occurrence. In particular, terminal
	// recovery is decided from the review that preceded the controller's
	// unlabel event, not from a head or base that may have moved afterwards.
	if done, ok := completedUnroutedRoute(cfg, s.Comments, triggers, events, all); ok {
		return record(observedPR(s), done.trigger, done.claim, done.head, done.outcome, done.why)
	}

	if s.Issue.Closed {
		return nothing("issue #%d is closed", cfg.Issue)
	}
	if len(triggers) == 0 {
		return nothing("no published milestone by %s to act on", cfg.TrackerAuthor)
	}
	trigger := triggers[len(triggers)-1]

	if !publishedOccurrenceOnLog(events, trigger.Occurrence, cfg.TrackerAuthor) {
		return nothing("occurrence %d is not a tracker-authored published transition on issue #%d", trigger.Occurrence, cfg.Issue)
	}
	if routed, ok := routeFor(cfg, s.Comments, trigger.Occurrence); ok {
		return nothing("occurrence %d already routed as %s", trigger.Occurrence, routed.Outcome)
	}
	sourceClaim, ok := claimEpochAtOccurrence(events, cfg.Principal, trigger.Occurrence)
	if !ok {
		return nothing("occurrence %d has no ordered standing claim by %s", trigger.Occurrence, cfg.Principal)
	}
	if err := validateSubject(cfg, trigger, s.PR); err != nil {
		return nothing("occurrence %d does not validate: %v", trigger.Occurrence, err)
	}
	sourceApproval, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, trigger.Occurrence)
	if !ok {
		return nothing("occurrence %d has no ordered standing complete required-label approval", trigger.Occurrence)
	}
	pr := *s.PR

	reviews := forApproval(
		since(all, cycleStartAtOccurrence(cfg, events, trigger.Occurrence)),
		sourceApproval,
	)

	// Resume before deciding, and resume from *every* review rather than this
	// cycle's. Occurrence and head both have to match: the review is the durable
	// record of that exact head/base subject, while an earlier review for the
	// same occurrence remains round evidence but cannot answer for a head push or
	// base movement after publication. A matching artifact is reconciled even if
	// a human has reapproved in the meantime, but route's approval fence permits
	// only recording the old occurrence then — never mutating the new cycle.
	if cur, ok := reviewFor(all, trigger.Occurrence, sourceClaim, sourceApproval, pr.Head, pr.BaseSHA); ok {
		// A profile-bearing marker is the committed identity of the invocation
		// that produced this verdict. A later selection cannot reinterpret it as
		// another profile's result. Empty remains the legacy marker shape and is
		// recoverable under a named deployment, as promised by the migration.
		if cur.marker.ReviewerProfile != "" && cur.marker.ReviewerProfile != selectedProfile {
			return nothing("occurrence %d was reviewed with profile %q, but the issue now selects %q; the durable verdict cannot route under a different profile",
				trigger.Occurrence, cur.marker.ReviewerProfile, selectedProfile)
		}
		return route(cfg, s, events, pr, trigger, cur, outcomeFor(cfg, reviews, cur.marker))
	}
	if cur, ok := mismatchedReviewForOccurrence(all, trigger.Occurrence, sourceClaim, sourceApproval); ok {
		return stop(cfg, s, events, pr, trigger, unreviewed(sourceClaim), decision{
			OutcomeHumanReview,
			fmt.Sprintf("occurrence %d belongs to claim %d and approval %d, but a controller review names claim %d and approval %d; the durable record is contested",
				trigger.Occurrence, sourceClaim, sourceApproval, cur.marker.Claim, cur.marker.Approval),
		})
	}

	// Counting, by contrast, is this cycle's business only: applying the
	// required label is SPEC §9.5's approval act, so reapplying it buys a
	// fresh round budget rather than an issue permanently at its cap.
	heads := distinctHeads(reviews)
	if prior, ok := reviewForHead(all, trigger.Occurrence, sourceClaim, sourceApproval, pr.Head); ok {
		// The same delivery and head were reviewed against a different base.
		// The old verdict is not a verdict on the current diff, but this is not
		// head progress and therefore consumes no additional round.
		return beginReview(cfg, s, events, pr, trigger, sourceClaim, fmt.Sprintf(
			"base moved from %s to %s while occurrence %d still names head %s; reviewing the current diff without consuming another head round",
			short(prior.marker.Base), short(pr.BaseSHA), trigger.Occurrence, short(pr.Head)))
	}
	switch {
	case containsHead(heads, pr.Head):
		// A new delivery for a head this cycle has already reviewed. BEN was
		// asked to revise and produced no new commit, so another round would
		// review the same diff forever. This consumes no round and stops.
		return route(cfg, s, events, pr, trigger, unreviewed(sourceClaim),
			decision{OutcomeNoProgress, fmt.Sprintf("head %s was already reviewed this cycle and has not moved", short(pr.Head))})

	case len(heads) >= cfg.RoundCap:
		return route(cfg, s, events, pr, trigger, unreviewed(sourceClaim),
			decision{OutcomeRoundCap, fmt.Sprintf("%d distinct heads have been reviewed this cycle, which is the cap", len(heads))})

	default:
		return beginReview(cfg, s, events, pr, trigger, sourceClaim, fmt.Sprintf(
			"occurrence %d delivered head %s, which round %d of %d has not reviewed",
			trigger.Occurrence, short(pr.Head), len(heads)+1, cfg.RoundCap))
	}
}

// completedRoute is a route whose externally visible mutation landed but whose
// final marker did not. The mutation is evidence of what already happened; it
// never authorizes repeating or changing that route.
type completedRoute struct {
	trigger Trigger
	claim   int64
	head    string
	outcome Outcome
	why     string
}

type terminalIntent struct {
	marker RouteIntentMarker
}

// resumeTerminalIntent completes the oldest terminal decision that has no
// route marker. An intent is authoritative only for a stop: if its label
// removal landed, record it; if the label is absent for any other reason, the
// intended terminal effect already holds; otherwise finish the revocation.
// None of those answers needs the PR to remain open or on the same head.
func resumeTerminalIntent(cfg Config, s Snapshot, events []Event) (Step, bool) {
	seen := map[int64]terminalIntent{}
	conflicted := map[int64]bool{}
	var ordered []terminalIntent
	for _, c := range s.Comments {
		if !eqFold(c.Author, cfg.Controller) {
			continue
		}
		m, err := ParseRouteIntentMarker(c.Body)
		if err != nil || routeExists(cfg, s.Comments, m.Occurrence) {
			continue
		}
		if prior, ok := seen[m.Occurrence]; ok {
			if prior.marker != m {
				conflicted[m.Occurrence] = true
			}
			continue
		}
		intent := terminalIntent{marker: m}
		seen[m.Occurrence] = intent
		ordered = append(ordered, intent)
	}

	if len(ordered) == 0 {
		return Step{}, false
	}
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].marker.Occurrence < ordered[j].marker.Occurrence
	})
	intent := ordered[0]
	m := intent.marker
	if conflicted[m.Occurrence] {
		return nothing("occurrence %d has conflicting terminal route intents", m.Occurrence), true
	}
	if !publishedOccurrenceOnLog(events, m.Occurrence, cfg.TrackerAuthor) {
		return nothing("terminal route intent occurrence %d is not a tracker-authored published transition", m.Occurrence), true
	}
	claim, ok := claimEpochAtOccurrence(events, cfg.Principal, m.Occurrence)
	if !ok || claim != m.Claim {
		return nothing("terminal route intent occurrence %d names claim %d, but its source claim is %d", m.Occurrence, m.Claim, claim), true
	}
	sourceApproval, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, m.Occurrence)
	if !ok {
		return nothing("terminal route intent occurrence %d names approval epoch %d, but its source approval is %d", m.Occurrence, m.Approval, sourceApproval), true
	}
	intentApproval := m.Approval
	if intentApproval != sourceApproval {
		// Before the complete required-label set selected the workspace cycle,
		// route intents recorded QueueLabel's event. Accept exactly that legacy
		// identity, then fence recovery with the independently derived full anchor.
		queueApproval, legacy := labelEpochAtOccurrence(events, cfg.QueueLabel, m.Occurrence)
		if !legacy || intentApproval != queueApproval {
			return nothing("terminal route intent occurrence %d names approval epoch %d, but its source approval is %d", m.Occurrence, m.Approval, sourceApproval), true
		}
		intentApproval = sourceApproval
	}
	approvalRemoved, newerApproval, validApproval := approvalChangesAfterEpoch(events, cfg.RequiredLabels, intentApproval)
	if !validApproval {
		return nothing("terminal route intent occurrence %d resolves to approval epoch %d, which is not a complete required-label anchor", m.Occurrence, intentApproval), true
	}

	trigger := Trigger{Occurrence: m.Occurrence}
	why := fmt.Sprintf("resuming durable %s intent for head %s", m.Outcome, short(m.Head))
	if !hasLabel(s.Issue.Labels, cfg.QueueLabel) {
		return record(observedPR(s), trigger, m.Claim, m.Head, m.Outcome,
			why+"; the required label removal is complete"), true
	}
	if newerApproval != 0 {
		return record(observedPR(s), trigger, m.Claim, m.Head, m.Outcome,
			fmt.Sprintf("%s; preserving newer human approval epoch %d", why, newerApproval)), true
	}
	if approvalRemoved {
		return record(observedPR(s), trigger, m.Claim, m.Head, m.Outcome,
			why+"; the observed approval epoch was withdrawn"), true
	}

	addLabel := ""
	if cfg.AddHumanReviewLabel {
		addLabel = HumanReviewLabel
	}
	return Step{
		Kind:        StepRevoke,
		Why:         why,
		PR:          observedPR(s),
		Occurrence:  m.Occurrence,
		Claim:       m.Claim,
		Head:        m.Head,
		Outcome:     m.Outcome,
		RemoveLabel: cfg.QueueLabel,
		AddLabel:    addLabel,
	}, true
}

func observedPR(s Snapshot) PullRequest {
	if s.PR == nil {
		return PullRequest{}
	}
	return *s.PR
}

func routeExists(cfg Config, comments []Comment, occurrence int64) bool {
	_, ok := routeFor(cfg, comments, occurrence)
	return ok
}

// completedUnroutedRoute finds the oldest publication with a completed route
// and no marker. Only routes with a review-bound subject are recovered here;
// no-review stops are recovered from their terminal intent before this legacy
// review-and-mutation pairing is considered.
func completedUnroutedRoute(cfg Config, comments []Comment, triggers []Trigger, events []Event, reviews []published) (completedRoute, bool) {
	positions := publishedPositions(cfg, triggers, events)
	for _, trigger := range triggers {
		if _, ok := routeFor(cfg, comments, trigger.Occurrence); ok {
			continue
		}
		if _, ok := positions[trigger.Occurrence]; !ok {
			continue
		}
		claim, ok := claimEpochAtOccurrence(events, cfg.Principal, trigger.Occurrence)
		if !ok {
			continue
		}
		approval, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, trigger.Occurrence)
		if !ok {
			continue
		}

		for i, ev := range events {
			isStop := ev.Type == EventUnlabeled && eqFold(ev.Actor, cfg.Controller) && eqFold(ev.Label, cfg.QueueLabel)
			isHandoff := ev.Type == EventUnassigned && eqFold(ev.Assignee, cfg.Principal)
			if !isStop && !isHandoff {
				continue
			}
			owner, ok := publicationBeforeMutation(triggers, positions, events, i)
			if !ok || owner.Occurrence != trigger.Occurrence {
				continue
			}
			prior, ok := latestReviewForOccurrenceThrough(reviews, trigger.Occurrence, claim, approval, ev.CreatedAt)
			if !ok {
				continue
			}
			cycle := through(forApproval(
				since(reviews, cycleStartAtOccurrence(cfg, events, trigger.Occurrence)),
				approval,
			), ev.CreatedAt)

			switch {
			case isStop:
				d := outcomeFor(cfg, cycle, prior.marker)
				if d.outcome == OutcomeRevise {
					// A controller-authored queue-label removal is a terminal
					// mutation. With an under-cap changes-requested review it can
					// only have been an escalation, never a delayed revise route.
					d = decision{OutcomeHumanReview, "the controller already removed the required label after the review"}
				}
				return completedRoute{
					trigger: trigger,
					claim:   claim,
					head:    prior.marker.Head,
					outcome: d.outcome,
					why:     d.why + "; the required label is no longer standing because the terminal mutation already landed, recording its original route",
				}, true

			case isHandoff && prior.marker.Verdict == VerdictChangesRequested:
				d := outcomeFor(cfg, cycle, prior.marker)
				if d.outcome != OutcomeRevise {
					continue
				}
				return completedRoute{
					trigger: trigger,
					claim:   claim,
					head:    prior.marker.Head,
					outcome: OutcomeRevise,
					why:     d.why + "; the source claim has already been handed back, recording the route",
				}, true
			}
		}
	}
	return completedRoute{}, false
}

// publishedPositions binds each trusted milestone to its ordered transition.
// A copied marker with no matching tracker-authored event gets no position and
// therefore cannot claim a later mutation.
func publishedPositions(cfg Config, triggers []Trigger, events []Event) map[int64]int {
	wanted := make(map[int64]bool, len(triggers))
	for _, trigger := range triggers {
		wanted[trigger.Occurrence] = true
	}
	out := make(map[int64]int, len(triggers))
	for i, ev := range events {
		if !wanted[ev.ID] || ev.Type != EventUnlabeled || !eqFold(ev.Actor, cfg.TrackerAuthor) ||
			!(eqFold(ev.Label, "ben:claimed") || eqFold(ev.Label, "ben:running")) {
			continue
		}
		out[ev.ID] = i
	}
	return out
}

// publicationBeforeMutation identifies the publication a later mutation could
// have completed. Both the transition and its milestone comment must precede
// the mutation; this prevents one unlabel event for O2 from completing every
// older unrouted occurrence too.
func publicationBeforeMutation(triggers []Trigger, positions map[int64]int, events []Event, mutation int) (Trigger, bool) {
	if mutation < 0 || mutation >= len(events) {
		return Trigger{}, false
	}
	var latest Trigger
	latestPosition := -1
	for _, trigger := range triggers {
		position, ok := positions[trigger.Occurrence]
		if !ok || position >= mutation || trigger.At.After(events[mutation].CreatedAt) {
			continue
		}
		if position > latestPosition {
			latest, latestPosition = trigger, position
		}
	}
	return latest, latestPosition >= 0
}

func beginReview(cfg Config, s Snapshot, events []Event, pr PullRequest, t Trigger, sourceClaim int64, why string) Step {
	if missing := missingRequiredLabel(s.Issue.Labels, cfg.RequiredLabels); missing != "" {
		// Not an escalation: the human broke the required approval set and
		// nothing is owed. Reapplying the label starts a newly approved cycle.
		return nothing("%q is not standing on issue #%d; automation is revoked", missing, cfg.Issue)
	}
	epoch := ClaimEpoch(events, cfg.Principal)
	if epoch == 0 {
		// Nothing to route a revision back to, and no claim to record in the
		// review's marker. SPEC §9.7 retains the claim at `done`, so this is a
		// claim a human has already taken.
		return nothing("no standing claim by %s to review under", cfg.Principal)
	}
	if epoch != sourceClaim {
		return nothing("occurrence %d belongs to claim %d, but the current claim is %d; a stale delivery never reviews or routes the newer claim",
			t.Occurrence, sourceClaim, epoch)
	}
	approval, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, t.Occurrence)
	if !ok {
		// The reducer already refused an issue whose required label is not
		// standing, so this is the narrower case: the label is present and the
		// change log cannot date its application. A workspace cycle nobody can
		// anchor is a sandbox a remote review would have to guess at (#204).
		return nothing("occurrence %d has no datable required-label approval to anchor a review to", t.Occurrence)
	}
	removed, newer, valid := approvalChangesAfterEpoch(events, cfg.RequiredLabels, approval)
	if !valid {
		return nothing("occurrence %d approval epoch %d is not a complete required-label anchor", t.Occurrence, approval)
	}
	if newer != 0 {
		return nothing("occurrence %d belongs to approval epoch %d, but it was replaced by epoch %d; stale output never reviews or routes the replacement cycle",
			t.Occurrence, approval, newer)
	}
	if removed {
		return nothing("occurrence %d approval epoch %d was withdrawn; stale output never reviews or routes a replacement cycle",
			t.Occurrence, approval)
	}
	profile, err := cfg.SelectReviewerProfile(s.Issue.Labels)
	if err != nil {
		return nothing("issue #%d cannot select a reviewer invocation: %v", cfg.Issue, err)
	}
	if profile != "" {
		why += fmt.Sprintf(" using reviewer profile %q", profile)
	}
	return Step{
		Kind:            StepReview,
		Why:             why,
		PR:              pr,
		Occurrence:      t.Occurrence,
		Claim:           sourceClaim,
		Approval:        approval,
		Head:            pr.Head,
		ReviewerProfile: profile,
	}
}

// decision is an outcome and the sentence explaining it, carried together so
// no route can be recorded without one.
type decision struct {
	outcome Outcome
	why     string
}

// outcomeFor turns a published verdict into a route. The cap is read off the
// reviews rather than a counter: three distinct heads reviewed with the third
// still asking for changes is where automation stops, and that is a fact on
// the pull request rather than a number in a process that may have restarted.
func outcomeFor(cfg Config, reviews []published, cur ReviewMarker) decision {
	if cur.Verdict == VerdictClean {
		return decision{OutcomeHumanReview, "the review of head " + short(cur.Head) + " is clean"}
	}
	if n := len(distinctHeads(reviews)); n >= cfg.RoundCap {
		return decision{OutcomeRoundCap, fmt.Sprintf("changes are still requested after %d distinct heads, which is the cap", n)}
	}
	return decision{OutcomeRevise, "changes are requested on head " + short(cur.Head)}
}

// route performs the second half of every cycle: confirm the world still looks
// the way it did when the verdict was formed, then take the one mutation the
// outcome authorizes, then record it.
func route(cfg Config, s Snapshot, events []Event, pr PullRequest, t Trigger, cur published, d decision) Step {
	if d.outcome != OutcomeRevise {
		return stop(cfg, s, events, pr, t, cur, d)
	}

	// Everything below is the revise path — the only route that keeps
	// automation running, and therefore the only one whose preconditions are
	// checked to exhaustion.
	handedBack := unassignedAfterClaim(events, cfg.Principal, cur.marker.Claim)
	if missing := missingRequiredLabel(s.Issue.Labels, cfg.RequiredLabels); missing != "" && !handedBack {
		// The human withdrew approval between the review and its route. The
		// unassignment would be nearly harmless — BEN releases a claim whose
		// required label is gone anyway (SPEC §9.8) — but "nearly harmless" is
		// not the standard for acting after a revocation. The second clause
		// keeps the ordinary repair working when the unassignment landed first
		// and the label went afterwards.
		return stop(cfg, s, events, pr, t, cur, decision{OutcomeHumanReview,
			fmt.Sprintf("required label %q was withdrawn before the revision could be routed", missing)})
	}
	approval, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, t.Occurrence)
	if !ok || approval != cur.marker.Approval {
		return nothing("occurrence %d review names approval %d, but its source approval is %d; a contested review never routes",
			t.Occurrence, cur.marker.Approval, approval)
	}
	approvalRemoved, newerApproval, validApproval := approvalChangesAfterEpoch(events, cfg.RequiredLabels, approval)
	if !validApproval {
		return nothing("occurrence %d approval epoch %d is not a complete required-label anchor", t.Occurrence, approval)
	}
	if newerApproval != 0 {
		return record(pr, t, cur.marker.Claim, cur.marker.Head, d.outcome,
			fmt.Sprintf("%s; preserving newer human approval epoch %d", d.why, newerApproval))
	}
	if approvalRemoved {
		// The issue page still reports the queue label while the ordered event log
		// says the complete approval set no longer stands. That is never authority
		// to hand back a claim under a withdrawn approval.
		return nothing("occurrence %d approval epoch %d was withdrawn; waiting for issue state to converge",
			t.Occurrence, approval)
	}
	if pr.Head != cur.marker.Head {
		// Defensive against a future caller that does not select reviews by both
		// occurrence and head. A stale verdict never authorizes a mutation; the
		// next reduction reviews the current head.
		return nothing("head moved from %s to %s since the review; a stale verdict never routes",
			short(cur.marker.Head), short(pr.Head))
	}
	if pr.BaseSHA != cur.marker.Base {
		return nothing("base moved from %s to %s since the review; a stale verdict never routes",
			short(cur.marker.Base), short(pr.BaseSHA))
	}

	epoch := ClaimEpoch(events, cfg.Principal)
	if epoch != cur.marker.Claim {
		if handedBack {
			// Either this controller's own unassignment landed and the marker
			// did not, or BEN has already reclaimed under a newer epoch. Both
			// are the intended effect; repeating the mutation would remove a
			// claim nobody asked us to touch.
			return record(pr, t, cur.marker.Claim, cur.marker.Head, OutcomeRevise,
				"the claim has been handed back; recording the route")
		}
		return stop(cfg, s, events, pr, t, cur, decision{OutcomeHumanReview,
			fmt.Sprintf("the claim epoch moved from %d to %d with no unassignment of ours; the state is contested", cur.marker.Claim, epoch)})
	}

	switch assignees := s.Issue.Assignees; {
	case !containsLogin(assignees, cfg.Principal):
		// The change log says the claim stands and the assignee list says it
		// does not. That is read lag, not a contest: doing nothing costs one
		// reconciliation tick, and escalating on it would revoke a human's
		// label over a stale page.
		return nothing("the change log holds claim epoch %d for %s but the assignee list does not; retrying",
			epoch, cfg.Principal)
	case len(assignees) != 1:
		return stop(cfg, s, events, pr, t, cur, decision{OutcomeHumanReview,
			fmt.Sprintf("issue #%d has %d assignees; no assignee is removed when a human shares the claim", cfg.Issue, len(assignees))})
	}

	return Step{
		Kind:       StepUnassign,
		Why:        d.why + "; handing the claim back to " + cfg.Principal,
		PR:         pr,
		Occurrence: t.Occurrence,
		Claim:      cur.marker.Claim,
		Head:       cur.marker.Head,
		Principal:  cfg.Principal,
	}
}

// stop is every route that ends automation: clean, the round cap, no progress,
// and every escalation. It removes the required label and never an assignee —
// BEN observes the revocation and releases its own claim (SPEC §9.8).
func stop(cfg Config, s Snapshot, events []Event, pr PullRequest, t Trigger, cur published, d decision) Step {
	head, claim := cur.head(pr)
	approval, ok := approvalEpochAtOccurrence(events, cfg.RequiredLabels, t.Occurrence)
	if !ok {
		return nothing("occurrence %d has no standing required-label approval epoch", t.Occurrence)
	}
	if !cur.zero && cur.marker.Approval != approval {
		return nothing("occurrence %d review names approval %d, but its source approval is %d; a contested review never routes",
			t.Occurrence, cur.marker.Approval, approval)
	}
	addLabel := ""
	if cfg.AddHumanReviewLabel {
		addLabel = HumanReviewLabel
	}

	if cur.zero {
		return Step{
			Kind:       StepRecordIntent,
			Why:        d.why,
			PR:         pr,
			Occurrence: t.Occurrence,
			Claim:      claim,
			Head:       head,
			Approval:   approval,
			Outcome:    d.outcome,
		}
	}
	approvalRemoved, newerApproval, validApproval := approvalChangesAfterEpoch(events, cfg.RequiredLabels, approval)
	if !validApproval {
		return nothing("occurrence %d approval epoch %d is not a complete required-label anchor", t.Occurrence, approval)
	}
	if newerApproval != 0 {
		return record(pr, t, claim, head, d.outcome,
			fmt.Sprintf("%s; preserving newer human approval epoch %d", d.why, newerApproval))
	}
	if approvalRemoved {
		return record(pr, t, claim, head, d.outcome, d.why+"; the occurrence's approval epoch was withdrawn")
	}
	if hasLabel(s.Issue.Labels, cfg.QueueLabel) {
		return Step{
			Kind:        StepRevoke,
			Why:         d.why,
			PR:          pr,
			Occurrence:  t.Occurrence,
			Claim:       claim,
			Head:        head,
			Outcome:     d.outcome,
			RemoveLabel: cfg.QueueLabel,
			AddLabel:    addLabel,
		}
	}
	return record(pr, t, claim, head, d.outcome, d.why+"; the label is already gone, recording the route")
}

func record(pr PullRequest, t Trigger, claim int64, head string, o Outcome, why string) Step {
	return Step{
		Kind:       StepRecordRoute,
		Why:        why,
		PR:         pr,
		Occurrence: t.Occurrence,
		Claim:      claim,
		Head:       head,
		Outcome:    o,
	}
}

// published is a controller review the forge has confirmed: its marker, and
// the review GitHub bound to a commit.
type published struct {
	review Review
	marker ReviewMarker
	// zero reports the "no review was performed for this occurrence" case,
	// where the outcome came from the round rules rather than from a verdict.
	zero bool
}

// head answers what a route marker should record for both the reviewed and the
// unreviewed case.
func (p published) head(pr PullRequest) (head string, claim int64) {
	if p.zero {
		return pr.Head, p.marker.Claim
	}
	return p.marker.Head, p.marker.Claim
}

// unreviewed carries the source claim into a route decided without a review
// for this occurrence (round-cap before another review, or no progress). A
// prior review's claim belongs to its own occurrence and must not be copied
// into the new route marker.
func unreviewed(sourceClaim int64) published {
	return published{marker: ReviewMarker{Claim: sourceClaim}, zero: true}
}

// controllerReviews is every review of this cycle that the controller can
// treat as its own durable record.
//
// Three conditions, and each removes a different forgery. The author must be
// the controller identity, so nobody else's review counts a round. The body
// must carry exactly one well-formed marker, so a review that merely mentions
// one is not one. And the marker's head must equal GitHub's own commit_id for
// the review, so the record cannot claim to have judged a commit the forge
// does not agree it was attached to — the binding #11 requires, and the reason
// a round is countable evidence rather than an assertion.
func controllerReviews(cfg Config, events []Event, all []Review, from time.Time) []published {
	var out []published
	for _, r := range all {
		if !eqFold(r.Author, cfg.Controller) || r.State != ReviewStateCommented {
			continue
		}
		if r.SubmittedAt.Before(from) {
			continue
		}
		m, err := ParseReviewMarker(r.Body)
		if err != nil || m.Head != r.CommitID {
			continue
		}
		if m.Approval == 0 {
			// Markers written before approval became explicit remain recoverable,
			// but they gain no authority from omission. Bind them to the immutable
			// ordered event history at their own occurrence before using them.
			m.Approval, _ = approvalEpochAtOccurrence(events, cfg.RequiredLabels, m.Occurrence)
			if m.Approval == 0 {
				continue
			}
		}
		out = append(out, published{review: r, marker: m})
	}
	return out
}

// since narrows the trusted reviews to one approval cycle. Inclusive of the
// boundary instant, because GitHub's second granularity (SPEC §8.4) cannot
// order a review against a label applied in the same second, and counting one
// review too many can only make the controller stop sooner.
func since(reviews []published, from time.Time) []published {
	var out []published
	for _, p := range reviews {
		if !p.review.SubmittedAt.Before(from) {
			out = append(out, p)
		}
	}
	return out
}

// forApproval narrows round evidence to the workspace cycle its durable marker
// names. Time remains an ordering check, but an explicit event id is the
// identity: a stale review submitted after reapproval cannot spend the new
// cycle's round budget merely because its timestamp is later.
func forApproval(reviews []published, approval int64) []published {
	var out []published
	for _, p := range reviews {
		if p.marker.Approval == approval {
			out = append(out, p)
		}
	}
	return out
}

// through narrows trusted reviews to facts visible when a route mutation
// landed. Inclusive is deliberate: GitHub timestamps reviews and issue events
// only to the second, so excluding equality could hide the review that caused
// the mutation.
func through(reviews []published, to time.Time) []published {
	var out []published
	for _, p := range reviews {
		if !p.review.SubmittedAt.After(to) {
			out = append(out, p)
		}
	}
	return out
}

func reviewFor(reviews []published, occurrence, claim, approval int64, head, base string) (published, bool) {
	for _, p := range reviews {
		if p.marker.Occurrence == occurrence && p.marker.Claim == claim && p.marker.Approval == approval &&
			p.marker.Head == head && p.marker.Base == base {
			return p, true
		}
	}
	return published{}, false
}

func reviewForHead(reviews []published, occurrence, claim, approval int64, head string) (published, bool) {
	for _, p := range reviews {
		if p.marker.Occurrence == occurrence && p.marker.Claim == claim &&
			p.marker.Approval == approval && p.marker.Head == head {
			return p, true
		}
	}
	return published{}, false
}

func latestReviewForOccurrenceThrough(reviews []published, occurrence, claim, approval int64, to time.Time) (published, bool) {
	var latest published
	var found bool
	for _, p := range reviews {
		if p.marker.Occurrence != occurrence || p.marker.Claim != claim ||
			p.marker.Approval != approval || p.review.SubmittedAt.After(to) {
			continue
		}
		if !found || p.review.SubmittedAt.After(latest.review.SubmittedAt) ||
			(p.review.SubmittedAt.Equal(latest.review.SubmittedAt) && p.review.ID > latest.review.ID) {
			latest, found = p, true
		}
	}
	return latest, found
}

func mismatchedReviewForOccurrence(reviews []published, occurrence, claim, approval int64) (published, bool) {
	for _, p := range reviews {
		if p.marker.Occurrence == occurrence && (p.marker.Claim != claim || p.marker.Approval != approval) {
			return p, true
		}
	}
	return published{}, false
}

// routeFor finds a completed route for an occurrence. Scoped to the whole
// issue rather than the cycle on purpose: an occurrence is globally unique, so
// a route recorded for one before a human reapplied the label must still
// suppress a redelivery of it afterwards.
func routeFor(cfg Config, comments []Comment, occurrence int64) (RouteMarker, bool) {
	for _, c := range comments {
		if !eqFold(c.Author, cfg.Controller) {
			continue
		}
		m, err := ParseRouteMarker(c.Body)
		if err != nil || m.Occurrence != occurrence {
			continue
		}
		return m, true
	}
	return RouteMarker{}, false
}

// distinctHeads is the round counter: how many different commits this cycle
// has actually reviewed. Duplicate delivery adds no entry because it publishes
// no second review, and a repeated head adds none because it is not distinct —
// which is exactly what "#11: duplicate delivery consumes no round; a repeated
// head consumes no round" asks for, without a counter to keep.
func distinctHeads(reviews []published) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range reviews {
		if h := p.marker.Head; !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

func containsHead(heads []string, head string) bool {
	for _, h := range heads {
		if h == head {
			return true
		}
	}
	return false
}

func containsLogin(logins []string, want string) bool {
	for _, l := range logins {
		if eqFold(l, want) {
			return true
		}
	}
	return false
}

func nothing(format string, args ...any) Step {
	return Step{Kind: StepNothing, Why: fmt.Sprintf(format, args...)}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
