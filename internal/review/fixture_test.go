package review

import (
	"fmt"
	"strings"
	"time"
)

// The fixture world: one issue, one branch, one pull request, one BEN daemon
// and claim principal — #11's fixed architecture, in the smallest form that
// can express every row of its table.
const (
	fxOwner      = "acme"
	fxRepo       = "ben"
	fxIssue      = 11
	fxPrincipal  = "ben-claim-bot"
	fxTracker    = "ben-tracker-bot"
	fxController = "ben-review-bot"
	fxQueue      = "ben-queue"
	fxHumanLabel = "human-review"
	fxPRNumber   = 42
	fxPRURL      = "https://github.com/acme/ben/pull/42"
)

// Three identities, deliberately three logins: #155 decoupled the claim
// assignee from the tracker credential, and #11 adds a controller that must be
// neither. A fixture that reused one account would make every author check
// pass for the wrong reason.
var fxCycleStart = time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return fxCycleStart.Add(time.Duration(minutes) * time.Minute) }

// sha expands a hex seed into the 40-character form the API returns.
func sha(seed string) string {
	return strings.Repeat(seed, 40/len(seed)+1)[:40]
}

var (
	head1 = sha("a1")
	head2 = sha("b2")
	head3 = sha("c3")
	head4 = sha("d4")
	base1 = sha("e5")
	base2 = sha("f6")
)

const (
	// The required-label event whose approval produced the first cycle.
	approval1 = int64(6001)
	// The label transition ids BEN's published milestones are anchored to.
	occ1 = int64(9001)
	occ2 = int64(9002)
	occ3 = int64(9003)
	occ4 = int64(9004)
	// The assignment ids that are claim epochs.
	epoch1 = int64(7001)
	epoch2 = int64(7002)
)

func fxConfig() Config {
	return Config{
		Owner:               fxOwner,
		Repo:                fxRepo,
		Issue:               fxIssue,
		Principal:           fxPrincipal,
		TrackerAuthor:       fxTracker,
		Controller:          fxController,
		RequiredLabels:      []string{fxQueue},
		QueueLabel:          fxQueue,
		AddHumanReviewLabel: true,
		RoundCap:            3,
	}
}

// milestone renders a published milestone exactly as
// internal/tracker/github/comment.go does, marker and all. The shape is the
// coupling #11 rests on, so the fixture reproduces it rather than approximating
// it; the tracker package pins the same two lines from its own side.
func milestone(id, occurrence int64, prURL string, when time.Time) Comment {
	return Comment{
		ID:     id,
		Author: fxTracker,
		Body: "**BEN published a pull request.**\n\n" +
			"- pull request: " + prURL + "\n" +
			"- daemon: `ben@canary`\n" +
			fmt.Sprintf("<!-- ben:milestone kind=published occurrence=%d -->\n", occurrence),
		CreatedAt: when,
	}
}

func routeComment(id int64, m RouteMarker, when time.Time) Comment {
	return Comment{
		ID:        id,
		Author:    fxController,
		Body:      "Routed to " + string(m.Outcome) + ".\n\n" + m.String() + "\n",
		CreatedAt: when,
	}
}

func intentComment(id int64, m RouteIntentMarker, when time.Time) Comment {
	return Comment{
		ID:        id,
		Author:    fxController,
		Body:      RouteIntentBody(m, "prepared by the fixture"),
		CreatedAt: when,
	}
}

func routeIntentMarker(occurrence, claim int64, head string, outcome Outcome) RouteIntentMarker {
	return RouteIntentMarker{
		Occurrence: occurrence,
		Claim:      claim,
		Approval:   approval1,
		Head:       head,
		Outcome:    outcome,
	}
}

func controllerReview(id int64, m ReviewMarker, when time.Time) Review {
	return Review{
		ID:          id,
		Author:      fxController,
		Body:        "Automated review findings.\n\n" + m.String() + "\n",
		CommitID:    m.Head,
		State:       ReviewStateCommented,
		SubmittedAt: when,
	}
}

func fxPR(head string) *PullRequest {
	return &PullRequest{
		Number:  fxPRNumber,
		URL:     fxPRURL,
		Branch:  "ben/11",
		Head:    head,
		Base:    "main",
		BaseSHA: base1,
		Body:    "What changed.\n\nFixes #11\n",
	}
}

// fxSnapshot is the state right after BEN's first publication: the human's
// label stands, BEN holds the claim, one published milestone links an open
// pull request at head1, and the controller has done nothing yet.
func fxSnapshot() Snapshot {
	return Snapshot{
		Issue: Issue{
			Number:    fxIssue,
			Labels:    []string{fxQueue},
			Assignees: []string{fxPrincipal},
		},
		Events: []Event{
			{ID: approval1, Type: EventLabeled, Actor: "a-human", Label: fxQueue, CreatedAt: at(0)},
			{ID: epoch1, Type: EventAssigned, Actor: fxTracker, Assignee: fxPrincipal, CreatedAt: at(1)},
			{ID: 6002, Type: EventLabeled, Actor: fxTracker, Label: "ben:claimed", CreatedAt: at(2)},
			{ID: occ1, Type: EventUnlabeled, Actor: fxTracker, Label: "ben:claimed", CreatedAt: at(9)},
		},
		Comments: []Comment{
			{ID: 1, Author: "a-human", Body: "Please do this.", CreatedAt: at(-10)},
			milestone(2, occ1, fxPRURL, at(10)),
		},
		PR: fxPR(head1),
	}
}

// reviewed advances the fixture by one completed round: the controller's
// review of head at the given verdict, published at `when`.
func (s *Snapshot) reviewed(id int64, occurrence, claim int64, head string, v Verdict, when time.Time) {
	approval, _ := approvalEpochAtOccurrence(SortEvents(s.Events), fxConfig().RequiredLabels, occurrence)
	s.Reviews = append(s.Reviews, controllerReview(id, ReviewMarker{
		Occurrence: occurrence, Claim: claim, Approval: approval,
		Head: head, Base: s.PR.BaseSHA, Verdict: v,
	}, when))
}

// delivered advances the fixture by one further publication: a new occurrence
// for a (possibly new) head.
func (s *Snapshot) delivered(id, occurrence int64, head string, when time.Time) {
	s.Events = append(s.Events, Event{
		ID: occurrence, Type: EventUnlabeled, Actor: fxTracker,
		Label: "ben:running", CreatedAt: when.Add(-time.Second),
	})
	s.Comments = append(s.Comments, milestone(id, occurrence, fxPRURL, when))
	s.PR = fxPR(head)
}

func (s *Snapshot) unassign(id int64, when time.Time) {
	s.Events = append(s.Events, Event{ID: id, Type: EventUnassigned, Actor: fxController, Assignee: fxPrincipal, CreatedAt: when})
	s.Issue.Assignees = nil
}

func (s *Snapshot) assign(id int64, when time.Time) {
	s.Events = append(s.Events, Event{ID: id, Type: EventAssigned, Actor: fxTracker, Assignee: fxPrincipal, CreatedAt: when})
	s.Issue.Assignees = []string{fxPrincipal}
}

func (s *Snapshot) unlabelQueue(id int64, actor string, when time.Time) {
	s.Events = append(s.Events, Event{ID: id, Type: EventUnlabeled, Actor: actor, Label: fxQueue, CreatedAt: when})
	var kept []string
	for _, l := range s.Issue.Labels {
		if l != fxQueue {
			kept = append(kept, l)
		}
	}
	s.Issue.Labels = kept
}

func (s *Snapshot) labelQueue(id int64, actor string, when time.Time) {
	s.Events = append(s.Events, Event{ID: id, Type: EventLabeled, Actor: actor, Label: fxQueue, CreatedAt: when})
	s.Issue.Labels = append(s.Issue.Labels, fxQueue)
}
