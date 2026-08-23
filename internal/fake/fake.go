// Package fake provides the in-memory tracker, workspace provider, and agent
// runner the orchestrator is tested against, and that B12's invariant suite
// drives end to end (BUILD.md B08). They are deliberately not test files: two
// packages need them.
//
// All three are safe for concurrent use — the orchestrator calls them from
// worker goroutines — and all three record what they were asked to do, since
// most of what the orchestrator guarantees is about side effects rather than
// return values.
package fake

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// TrackerEpoch is the time a fresh tracker stamps on the events it records.
//
// Fixed rather than wall-clock, so a log's ordering is stated by the fixture rather
// than by when the test ran.
var TrackerEpoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// DefaultPrincipal is the login NewTracker claims as.
const DefaultPrincipal = "ben-bot"

// Tracker is an in-memory GitHub stand-in: issues, labels, assignees,
// comments, and a scripted-failure seam.
type Tracker struct {
	mu     sync.Mutex
	issues map[string]*core.Issue

	// Principal is the login a verified claim assigns.
	Principal string

	// FailClaim, FailFetch, FailGet override the next call's outcome. Nil
	// means succeed.
	FailFetch error
	FailGet   error
	// FailGetFor is FailGet per identifier, for the fixtures where one issue's read
	// fails and the others do not — §9.8's absence confirmation rotates over a set,
	// and a rotation can only be observed by starving one member of it.
	FailGetFor func(identifier string) error
	// ClaimVerified controls whether a claim sticks; nil means it does.
	ClaimVerified func(identifier string) bool
	// FailLabel, when set, decides each SetStateLabels call's outcome. Nil means
	// every projection lands. A predicate rather than a single error because the
	// interesting fixtures fail one projection and not the ones before it — a
	// record cannot reach `running` at all if its ben:claimed write never lands.
	FailLabel func(identifier string, label core.StateLabel) error
	// FailComment is FailLabel for milestone comments, and it exists because the
	// two writes fail independently on the real adapter: a comment is a
	// content-generating request under its own §8.5 allowance, on a resource a
	// human can lock. A fixture that could only wedge the *label* could not
	// express a record whose projection has landed while a comment behind it keeps
	// failing — which is the state §9.8's parked label rule has to keep working in.
	FailComment func(identifier string, m core.Milestone) error
	// GetResult overrides a Get for one identifier.
	GetResult map[string]*core.Issue

	// Calls records every write, in order.
	Calls []string
	// Labels is the last state label projected per issue.
	Labels map[string]core.StateLabel
	// Comments is every milestone posted per issue, in order.
	Comments map[string][]core.MilestoneComment
	// Released records the issues whose claim was successfully dropped.
	Released map[string]int
	// releaseAttempts counts every Release call, successful or not.
	releaseAttempts map[string]int
	failRelease     error
	labelGate       func()
	getGate         func()
	historyGate     func()
	fetchGate       func()
	claimGate       func()
	claimErr        error
	heldReads       int
	historyReads    int
	getReads        int
	// getPerIssue counts reads per issue. The real adapter spends one request per
	// Get, so *which* issues a pass asked about is the only way to see that a bounded
	// pass rotates rather than re-reading one prefix. See GetReadsFor.
	getPerIssue map[string]int
	fetchReads  int
	// failHeldClaims makes the sweep read fail; set via SetFailHeldClaims.
	failHeldClaims error
	failHistory    error
	// failClaimed makes the §9.10 step 1 recovery read fail; claimedReads counts
	// it separately from heldReads, since the two differ in cache posture.
	failClaimed  error
	claimedReads int
	claimedGate  func()
	// RequiredLabels is the workflow's label partition (SPEC §8.3). The
	// dispatchable verdict is computed against it, so an issue that has left
	// the partition does not come back through Fetch — which is what stops a
	// claim released *because* the labels were stripped from being handed
	// straight back as new work.
	RequiredLabels []string
	// History is the change log per issue: what Claim and Release record, plus
	// whatever a fixture scripts.
	History map[string][]core.ClaimEvent
	// labelLog is the `labeled` events an issue's labels imply, held apart from
	// History because they are not scripted — they are a consequence of the
	// issue carrying the labels, the way the assignment event is a consequence
	// of Claim. On GitHub a label cannot stand without one, and §9.5's approving
	// instant is read off exactly those, so SetHistory replacing the scripted
	// log must not take them with it: an issue with no `labeled` event for its
	// required label parks, and every fixture would.
	labelLog map[string][]core.ClaimEvent
	// prs are the pull requests FindPR answers from, keyed by branch.
	prs         map[string]*core.PR
	failFindPR  error
	findPRReads int
	// nextEventID is the tracker's monotonic change-log id (core.ClaimEvent.ID).
	nextEventID int64
	// now is the timestamp stamped on events this tracker records. See appendEvent.
	now time.Time
	// postedMilestones is the adapter's comment marker: one entry per (issue,
	// milestone, occurrence), which is what makes re-issuing a comment free.
	postedMilestones map[string]bool
	// edits is the §9.5 content-edit fact per issue: when the title or body was
	// last edited, as a fact the tracker states. An issue installed here has
	// never been edited — the positive answer, not the zero value, because the
	// zero core.ContentEdit is `unknown` and refuses.
	edits map[string]core.ContentEdit
	// contentResult overrides the whole §9.5 content read for one identifier,
	// which is what lets a test make the list read and the content read disagree.
	//
	// They genuinely can on GitHub, and the divergence is structural rather than
	// a race: Fetch is ETag-conditional (SPEC §8.5) while the content read
	// attests to the world and is not, so a 304-served candidate can carry
	// content older than the read the approval check was made against. That is
	// exactly why §9.5 pins "the content read at claim" and not whatever the
	// candidate happened to carry.
	contentResult map[string]core.ContentApproval
	// failContentApproval makes the §9.5 content read fail until cleared.
	failContentApproval error
	contentReads        int
}

func NewTracker(issues ...core.Issue) *Tracker {
	t := &Tracker{
		issues:          map[string]*core.Issue{},
		Principal:       DefaultPrincipal,
		Labels:          map[string]core.StateLabel{},
		Comments:        map[string][]core.MilestoneComment{},
		Released:        map[string]int{},
		releaseAttempts: map[string]int{},
		History:         map[string][]core.ClaimEvent{},
		labelLog:        map[string][]core.ClaimEvent{},
		GetResult:       map[string]*core.Issue{},
		prs:             map[string]*core.PR{},
		edits:           map[string]core.ContentEdit{},
		contentResult:   map[string]core.ContentApproval{},
		// Matches the label Issue puts on a fixture and the workflow the
		// orchestrator tests load.
		RequiredLabels: []string{"ben-queue"},
		// Well before the creation times fixtures use, so an event this tracker
		// records is dated *before* anything a test dates by hand — which is the
		// direction that makes "a failure from a previous cycle" expressible.
		now: TrackerEpoch,
	}
	for i := range issues {
		t.install(issues[i])
	}
	return t
}

// install records an issue and the `labeled` events GitHub would already have
// for it. Callers hold the lock, or hold nothing yet (NewTracker).
//
// The labeled events are the point. On GitHub every label an issue carries has a
// `labeled` event dating its application, and §9.5's approving instant is read
// off exactly those. A fake that installed labels without them would answer "no
// approving instant" for every fixture — which parks everything, so no test
// could tell a correct implementation from one that never checks — and, worse,
// would make a test that *did* script one pass for a world GitHub never
// produces.
//
// Stamped at CreatedAt, which is when a filed-and-labelled issue acquires them,
// and rebuilt rather than appended so replacing an issue does not leave the log
// claiming a label twice. A fixture that needs a label applied — or removed and
// re-applied — later in the run says so with AppendHistory, which lands after
// these and therefore wins the replay.
func (t *Tracker) install(issue core.Issue) {
	t.issues[issue.Identifier] = &issue
	if _, ok := t.edits[issue.Identifier]; !ok {
		// Never edited: the positive fact, not the zero value. An adapter that
		// cannot answer reports `unknown` and the issue parks, so a fake whose
		// default was the zero value would park every fixture (BUILD.md
		// decision 15).
		t.edits[issue.Identifier] = core.ContentEdit{Status: core.ContentEditNever}
	}
	t.labelLog[issue.Identifier] = nil
	for _, l := range issue.Labels {
		t.nextEventID++
		t.labelLog[issue.Identifier] = append(t.labelLog[issue.Identifier], core.ClaimEvent{
			Kind: core.ClaimEventLabeled, Actor: "labeler", Subject: l,
			At: issue.CreatedAt, ID: t.nextEventID,
		})
	}
}

// Issue builds a dispatchable issue for a fixture.
func Issue(identifier string, created time.Time) core.Issue {
	return core.Issue{
		Identifier:   identifier,
		Title:        "Ticket " + identifier,
		Body:         "do the thing",
		State:        "open",
		Labels:       []string{"ben-queue"},
		CreatedAt:    created,
		UpdatedAt:    created,
		Dispatchable: true,
	}
}

func (t *Tracker) record(format string, args ...any) {
	t.Calls = append(t.Calls, fmt.Sprintf(format, args...))
}

// Set installs or replaces an issue as the tracker reports it, with the
// `labeled` events its labels imply.
func (t *Tracker) Set(issue core.Issue) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.install(issue)
}

// Mutate edits an issue in place, for simulating a human.
//
// A close or a reopen also lands in the change log, because that is where the
// fact lives: SPEC §9.10 gate 1 and the §9.8 sweep both classify from the
// *event*, and a close a later reopen has undone survives nowhere else. A
// fixture that moved State without recording one would let code that reads only
// current state pass — which is the specific defect gate 1 exists to prevent.
//
// It does not touch the §9.5 content-edit fact, so it models everything a human
// does to an issue *except* rewriting its title or body: labels, assignees,
// state. Edit is that one, and it is separate precisely because on GitHub a
// content change and its timestamp are one act — a fake that let a body move
// without the fact moving would model a world the tracker cannot produce, and
// would let a §9.5 check that never runs pass.
func (t *Tracker) Mutate(identifier string, fn func(*core.Issue)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	iss, ok := t.issues[identifier]
	if !ok {
		return
	}
	before := iss.State
	fn(iss)
	switch {
	case before != "closed" && iss.State == "closed":
		t.appendEvent(identifier, core.ClaimEvent{Kind: core.ClaimEventClosed, Actor: "a-human"})
	case before == "closed" && iss.State != "closed":
		t.appendEvent(identifier, core.ClaimEvent{Kind: core.ClaimEventReopened, Actor: "a-human"})
	}
}

// Edit rewrites an issue's author-controlled content and dates the edit, which
// is the two halves of what a human editing an issue does (SPEC §9.5).
func (t *Tracker) Edit(identifier string, at time.Time, fn func(*core.IssueContent)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	iss, ok := t.issues[identifier]
	if !ok {
		return
	}
	content := core.IssueContent{Title: iss.Title, Body: iss.Body}
	fn(&content)
	iss.Title, iss.Body = content.Title, content.Body
	t.edits[identifier] = core.ContentEdit{Status: core.ContentEditAt, At: at}
}

// SetContentEdit states the §9.5 edit fact directly, for the shapes Edit cannot
// produce: an adapter that cannot answer (`unknown`, which refuses), or an edit
// recorded without the content having to differ.
func (t *Tracker) SetContentEdit(identifier string, edit core.ContentEdit) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.edits[identifier] = edit
}

// SetLabelLog replaces the `labeled` events an issue's labels imply.
//
// Only two fixtures want this and both are refusals: a change log that cannot
// date a required label (no events at all), and one that dates it somewhere
// other than the issue's creation. It exists rather than being spelled through
// SetHistory because the two logs are kept apart on purpose — see labelLog.
func (t *Tracker) SetLabelLog(identifier string, events ...core.ClaimEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.labelLog[identifier] = events
}

// ContentApproval is the §9.5 read: the author-controlled content as the tracker
// now reports it, together with when it was last edited (SPEC §9.5,
// core.ContentApprovalSource).
//
// One response carrying both, because that is what the adapter's single GraphQL
// query returns and what the orchestrator's pin depends on: content from one
// read and an edit time from another would leave a window between them.
func (t *Tracker) ContentApproval(_ context.Context, issue core.Issue) (core.ContentApproval, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.contentReads++
	if t.failContentApproval != nil {
		return core.ContentApproval{}, t.failContentApproval
	}
	if override, ok := t.contentResult[issue.Identifier]; ok {
		return override, nil
	}
	iss, ok := t.issues[issue.Identifier]
	if !ok {
		return core.ContentApproval{}, errIssueGone(issue.Identifier)
	}
	return core.ContentApproval{
		Content: core.IssueContent{Title: iss.Title, Body: iss.Body},
		Edit:    t.edits[issue.Identifier],
	}, nil
}

// SetContentApprovalResult overrides the whole §9.5 content read for one
// identifier, so a test can make it disagree with the candidate the list read
// handed over — the ETag-conditional-Fetch-versus-uncached-content-read
// divergence contentResult documents.
func (t *Tracker) SetContentApprovalResult(identifier string, approval core.ContentApproval) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.contentResult[identifier] = approval
}

// SetFailContentApproval makes the §9.5 content read fail until cleared — the
// "could not ask" case, which retains the claim and retries rather than parking.
func (t *Tracker) SetFailContentApproval(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failContentApproval = err
}

// ContentReads counts §9.5 content reads, so a test can assert the check runs
// at a dispatch decision and not on every reconciliation tick.
func (t *Tracker) ContentReads() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.contentReads
}

func (t *Tracker) Fetch(context.Context) ([]core.Issue, error) {
	t.mu.Lock()
	t.fetchReads++
	gate := t.fetchGate
	t.mu.Unlock()
	if gate != nil {
		gate()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.FailFetch != nil {
		return nil, t.FailFetch
	}
	out := make([]core.Issue, 0, len(t.issues))
	for _, iss := range t.issues {
		out = append(out, *iss)
	}
	return out, nil
}

func (t *Tracker) Get(_ context.Context, identifier string) (*core.Issue, error) {
	t.mu.Lock()
	t.getReads++
	if t.getPerIssue == nil {
		t.getPerIssue = map[string]int{}
	}
	t.getPerIssue[identifier]++
	gate := t.getGate
	t.mu.Unlock()
	if gate != nil {
		gate()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.FailGet != nil {
		return nil, t.FailGet
	}
	if t.FailGetFor != nil {
		if err := t.FailGetFor(identifier); err != nil {
			return nil, err
		}
	}
	if override, ok := t.GetResult[identifier]; ok {
		if override == nil {
			return nil, errIssueGone(identifier)
		}
		return override, nil
	}
	iss, ok := t.issues[identifier]
	if !ok {
		// The real adapter names absence rather than returning a nil issue,
		// and the orchestrator has to distinguish it from a failed read
		// (SPEC §9.8) — so the fake states it the same way.
		return nil, errIssueGone(identifier)
	}
	copied := *iss
	return &copied, nil
}

func (t *Tracker) Claim(_ context.Context, issue core.Issue) (bool, error) {
	t.mu.Lock()
	gate := t.claimGate
	t.mu.Unlock()
	if gate != nil {
		gate()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.record("claim %s", issue.Identifier)
	// The three answers below are in the adapter's own order, and the order is
	// the contract: §8.4's claim is a *sequence* — refuse, write, read back — and
	// each stage can only report what the stages before it reached (github
	// Claim). A fake that answers out of order invents outcomes the adapter
	// cannot produce, and a caller that routes on them passes here and meets
	// something else in production.
	if errors.Is(t.claimErr, core.ErrClaimNotAttempted) {
		// Before the write. The other shape of a claim error, and the one that
		// must not assign: the adapter refused before writing, so there is no
		// assignee and no event for the caller to reconcile with
		// (core.ErrClaimNotAttempted).
		return false, t.claimErr
	}
	iss, ok := t.issues[issue.Identifier]
	if !ok {
		// The write. Its 404 is where an issue deleted between the candidate read
		// and the claim surfaces — the one deletion race a claim can lose, since
		// Fetch answers from a list this tracker may already have moved past.
		//
		// A refusal, and **not** `false, nil`: that pair is the adapter stating it
		// released whatever it wrote (SPEC §8.4), and answering it for an issue
		// that does not exist would let a caller treat a lost race and a vanished
		// issue as one outcome.
		//
		// Unclassified, like every other write, and without
		// core.ErrClaimNotAttempted: the adapter's 404 arrives from the assignment
		// request itself, so a write *was* attempted and it cannot promise nothing
		// landed. The caller therefore owes an unwinding release, which is the
		// fail-closed direction (github Claim).
		return false, errAbsentIssue(issue.Identifier)
	}
	if t.ClaimVerified != nil && !t.ClaimVerified(issue.Identifier) {
		// The read-back, and therefore *after* the write: this models GitHub
		// accepting an assignment with a 201 and silently discarding it, which is
		// the whole reason §8.4 verifies by reading back. It cannot answer for an
		// issue whose assignment never got a 201 to begin with — an absent issue
		// reaches the refusal above and never gets here.
		//
		// No assignment is recorded, because the point of the fixture is that the
		// write did not stick.
		return false, nil
	}
	iss.Assignees = append(iss.Assignees, t.Principal)
	iss.Dispatchable = false
	// A claim *is* an assignment event on the real tracker, and §9.10 step 2
	// anchors the claim cycle on it. Recording it here rather than leaving
	// every fixture to script one is what stops a test from silently agreeing
	// with an implementation that never establishes an anchor.
	t.appendEvent(issue.Identifier, core.ClaimEvent{
		Kind: core.ClaimEventAssigned, Actor: t.Principal, Subject: t.Principal,
	})
	if t.claimErr != nil {
		// Assigned first, deliberately. §8.4's unwind paths return an error
		// only after the write landed and the release that should have undone
		// it also failed, so an error that had skipped the assignment would be
		// a shape the adapter never produces — and would let a caller that
		// strands the claim pass.
		return false, t.claimErr
	}
	return true, nil
}

// ClaimBy records *another party's* standing assignment: the login joins the
// assignees, and the change log gets the `assigned` event GitHub writes with it.
//
// It exists because Claim can only ever assign this tracker's own principal,
// while SPEC §9.10 gate 2 is entirely about a second daemon holding the same
// issue — and a fixture for it has to be able to say which assignment came first.
// Ordering is by (At, ID) like every other change-log question, so the party
// whose ClaimBy or Claim lands first is the one arbitration calls the winner.
//
// Both halves together, deliberately. On GitHub an assignment and its timeline
// entry are one act, so a fixture that moved the assignee list alone would
// describe a world the tracker cannot produce by that act — an assignee the log
// has no event for. That state is reachable by a different route entirely (event
// retention, a transfer), it is a fact of its own, and it is precisely the one
// arbitration refuses to order (orchestrator arbitrateRecovery). Keeping the two
// spellings apart is what lets a fixture mean one of them.
//
// Idempotent on a login already assigned, and silent on an issue that does not
// exist: both are acts that change nothing.
func (t *Tracker) ClaimBy(identifier, login string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	iss, ok := t.issues[identifier]
	if !ok || containsFold(iss.Assignees, login) {
		return
	}
	iss.Assignees = append(iss.Assignees, login)
	iss.Dispatchable = t.dispatchableNow(iss)
	t.appendEvent(identifier, core.ClaimEvent{
		Kind: core.ClaimEventAssigned, Actor: login, Subject: login,
	})
}

// UnassignBy models an external controller removing one assignment. Assignment
// state and its change-log event are one act, just as ClaimBy's are; using
// Mutate for this would deliberately create the unorderable retained-log shape
// recovery gate 2 reserves for event loss rather than a normal re-claim.
func (t *Tracker) UnassignBy(identifier, login string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	iss, ok := t.issues[identifier]
	if !ok || !containsFold(iss.Assignees, login) {
		return
	}
	kept := iss.Assignees[:0]
	for _, assignee := range iss.Assignees {
		if !strings.EqualFold(assignee, login) {
			kept = append(kept, assignee)
		}
	}
	iss.Assignees = kept
	iss.Dispatchable = t.dispatchableNow(iss)
	t.appendEvent(identifier, core.ClaimEvent{
		Kind: core.ClaimEventUnassigned, Actor: "external-controller", Subject: login,
	})
}

// SetClaimError makes Claim fail, in whichever of the adapter's two shapes err
// names. By default it assigns and then fails — what the adapter reports when it
// cannot verify or unwind (github Claim's joined errors). An err carrying
// core.ErrClaimNotAttempted instead refuses before writing anything, because
// that error is precisely the promise that nothing was written; a fake that
// assigned anyway would let a caller that skips the release pass while the real
// adapter left an assignment standing.
func (t *Tracker) SetClaimError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.claimErr = err
}

// SetFetchGate blocks inside Fetch, for a candidate read that hangs while the
// rest of the tick has work to do.
func (t *Tracker) SetFetchGate(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fetchGate = fn
}

// FetchReads counts candidate reads, which is how a test observes that a tick
// happened at all.
func (t *Tracker) FetchReads() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.fetchReads
}

// Release drops this daemon's claim. Idempotent on an issue that exists — the
// adapter's RemoveAssignees is a no-op for an assignee that is not there — and a
// **refusal** on one that does not.
//
// The second half is the fidelity that matters. GitHub answers 404 for an issue
// that was deleted or transferred, so the adapter returns an error, and an owed
// release that errors is retried every tick forever (owed.go): the record is
// never forgotten and its §9.5 concurrency slot is never freed. A fake that
// answered "released" for an issue it does not have models a tracker that cannot
// exist, and it let exactly that defect through review — the vanished-issue path
// was fixed to call finishNow, which releases, and passed because this returned
// nil. Absence is a fact the caller must handle by forgetting, not by asking a
// tracker to unassign something that is gone.
func (t *Tracker) Release(_ context.Context, issue core.Issue) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.record("release %s", issue.Identifier)
	t.releaseAttempts[issue.Identifier]++
	if t.failRelease != nil {
		return t.failRelease
	}
	iss, ok := t.issues[issue.Identifier]
	if !ok {
		return errAbsentIssue(issue.Identifier)
	}
	t.Released[issue.Identifier]++
	{
		var kept []string
		for _, a := range iss.Assignees {
			if a != t.Principal {
				kept = append(kept, a)
			}
		}
		iss.Assignees = kept
		iss.Dispatchable = t.dispatchableNow(iss)
	}
	t.appendEvent(issue.Identifier, core.ClaimEvent{
		Kind: core.ClaimEventUnassigned, Actor: t.Principal, Subject: t.Principal,
	})
	return nil
}

func (t *Tracker) SetStateLabels(_ context.Context, issue core.Issue, label core.StateLabel) error {
	t.mu.Lock()
	gate := t.labelGate
	t.mu.Unlock()
	if gate != nil {
		gate()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	t.record("label %s=%s", issue.Identifier, label)
	if _, ok := t.issues[issue.Identifier]; !ok {
		// 404, like the adapter: there is no issue to label. Recorded above
		// first, because the real one has tried by the time it reports this.
		return errAbsentIssue(issue.Identifier)
	}
	if t.FailLabel != nil {
		if err := t.FailLabel(issue.Identifier, label); err != nil {
			return err
		}
	}
	// The projection itself, which also appends the change-log entries the adapter
	// appends — add before remove, and none for an idempotent re-add.
	t.projectStateLabel(issue.Identifier, label, true)
	return nil
}

// InterruptStateLabels performs only the *first* write of a projection: the new
// label goes on and the old ones stay. It is what a daemon killed between the
// two writes leaves behind (github SetStateLabels: add before remove, so a
// crash leaves two `ben:*` labels standing rather than none).
//
// A fixture verb rather than an error injected into SetStateLabels, because a
// crash returns to nobody — an error would leave the projection owed and
// retried, which is a different state from the one §9.10 step 3 classifies.
// It shares projectStateLabel with the ordinary path so the two cannot drift.
func (t *Tracker) InterruptStateLabels(identifier string, label core.StateLabel) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.record("label %s=%s (interrupted)", identifier, label)
	t.projectStateLabel(identifier, label, false)
}

// projectStateLabel mirrors the adapter's control flow (github SetStateLabels):
// the wanted label is added first, then every *other* `ben:*` label is removed.
// Each write that changes something appends its own change-log entry, and an
// add of a label the issue already carries appends none — GitHub's add is
// idempotent and writes no timeline entry.
//
// removeOld=false stops after the add. Callers hold the lock.
func (t *Tracker) projectStateLabel(identifier string, label core.StateLabel, removeOld bool) {
	t.Labels[identifier] = label
	iss, ok := t.issues[identifier]
	if !ok {
		return
	}

	want := ""
	if label != core.StateLabelNone {
		want = "ben:" + string(label)
	}
	if want != "" && !containsFold(iss.Labels, want) {
		iss.Labels = append(iss.Labels, want)
		t.appendEvent(identifier, core.ClaimEvent{
			Kind: core.ClaimEventLabeled, Actor: t.Principal, Subject: want,
		})
	}
	if !removeOld {
		return
	}
	var kept []string
	for _, l := range iss.Labels {
		if !hasStateLabel([]string{l}) || strings.EqualFold(l, want) {
			kept = append(kept, l)
			continue
		}
		t.appendEvent(identifier, core.ClaimEvent{
			Kind: core.ClaimEventUnlabeled, Actor: t.Principal, Subject: l,
		})
	}
	iss.Labels = kept
}

// ErrMilestonePayload and ErrNoMilestoneOccurrence mirror the adapter's two
// refusals (github renderMilestone, ErrNoMilestoneOccurrence). A fake that
// accepted what the adapter rejects would let a caller build a comment the tracker
// can never take — and an owed write that can never land is retried forever, with
// everything queued behind it.
var (
	ErrMilestonePayload      = errors.New("fake: milestone comment payload is incomplete")
	ErrNoMilestoneOccurrence = errors.New("fake: no label transition anchors this milestone")
)

// Comment posts one of the four milestones, validated and idempotent per
// occurrence — both because the adapter is (SPEC §8.4), and because §9.10 leans on
// the second: "every recovery verdict re-issues the milestone comment for the state
// it lands in" is only a repair rather than spam because re-issuing is free.
func (t *Tracker) Comment(_ context.Context, issue core.Issue, c core.MilestoneComment) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.record("comment %s=%s", issue.Identifier, c.Milestone)
	if _, ok := t.issues[issue.Identifier]; !ok {
		return errAbsentIssue(issue.Identifier)
	}
	if err := checkMilestonePayload(c); err != nil {
		return err
	}
	if t.FailComment != nil {
		// After the record and the absence check, like SetStateLabels: the real
		// adapter has tried by the time it reports a failure. Before the occurrence
		// bookkeeping below, because a comment that failed did not post — marking it
		// as posted would make the retry a silent no-op.
		if err := t.FailComment(issue.Identifier, c.Milestone); err != nil {
			return err
		}
	}
	occurrence, err := t.milestoneOccurrence(issue.Identifier, c.Milestone)
	if err != nil {
		return err
	}
	if t.postedMilestones == nil {
		t.postedMilestones = map[string]bool{}
	}
	key := fmt.Sprintf("%s/%s/%d", issue.Identifier, c.Milestone, occurrence)
	if t.postedMilestones[key] {
		// Already on the issue for this occurrence: a no-op, exactly as the adapter's
		// marker check makes it. Counted in Calls above, because the request *was*
		// made — what is idempotent is the effect, not the round trip.
		return nil
	}
	t.postedMilestones[key] = true
	t.Comments[issue.Identifier] = append(t.Comments[issue.Identifier], c)
	return nil
}

// checkMilestonePayload restates the adapter's rendering refusals. Both are about
// a comment asserting something it does not carry.
func checkMilestonePayload(c core.MilestoneComment) error {
	switch c.Milestone {
	case core.MilestonePublished:
		if c.PRURL == "" {
			return fmt.Errorf("%w: %q requires a pull request URL", ErrMilestonePayload, c.Milestone)
		}
	case core.MilestoneFailed:
		// Exactly one of the two. A reason and a disclaimer together is incoherent;
		// neither is a comment that explains nothing.
		if (c.Reason == "") == !c.ReasonUnavailable {
			return fmt.Errorf("%w: %q needs either a §7.3 reason or ReasonUnavailable, not both and not neither",
				ErrMilestonePayload, c.Milestone)
		}
	case core.MilestoneClaimed, core.MilestoneNeedsReview:
	default:
		return fmt.Errorf("%w: unknown milestone %q", ErrMilestonePayload, c.Milestone)
	}
	return nil
}

// milestoneOccurrence ports the adapter's anchor rules (github
// milestoneOccurrence): the id of the label transition that defines *this*
// occurrence of this kind.
//
// The anchor differs per kind because the four recur differently, and the
// differences are load-bearing rather than incidental — keying everything on the
// claim cycle would suppress a legitimate second needs-review, and keying on every
// transition would spam claimed, since §9.3 maps preparing, verifying and backoff
// onto ben:claimed too. Callers hold the lock.
func (t *Tracker) milestoneOccurrence(identifier string, m core.Milestone) (int64, error) {
	events := t.History[identifier]
	switch m {
	case core.MilestoneClaimed:
		// The *first* ben:claimed of this claim cycle — later re-entries are the same
		// claim, not a new one.
		var start int64
		for _, ev := range events {
			if !strings.EqualFold(ev.Subject, t.Principal) {
				continue
			}
			switch ev.Kind {
			case core.ClaimEventAssigned:
				start = ev.ID
			case core.ClaimEventUnassigned:
				start = 0
			}
		}
		if start == 0 {
			return 0, fmt.Errorf("%w: %s has no standing assignment to %s to scope its claim milestone",
				ErrNoMilestoneOccurrence, identifier, t.Principal)
		}
		for _, ev := range events {
			if ev.ID >= start && ev.Kind == core.ClaimEventLabeled && labelIs(ev.Subject, core.StateLabelClaimed) {
				return ev.ID, nil
			}
		}
	case core.MilestoneNeedsReview:
		if id, ok := lastLabelTransition(events, core.ClaimEventLabeled, core.StateLabelNeedsReview); ok {
			return id, nil
		}
	case core.MilestoneFailed:
		if id, ok := lastLabelTransition(events, core.ClaimEventLabeled, core.StateLabelFailed); ok {
			return id, nil
		}
	case core.MilestonePublished:
		// done clears the projection; the transition that removed it is what the
		// publish comment belongs to.
		if id, ok := lastLabelTransition(events, core.ClaimEventUnlabeled,
			core.StateLabelClaimed, core.StateLabelRunning); ok {
			return id, nil
		}
	}
	return 0, fmt.Errorf("%w: %s carries no label transition anchoring the %q milestone",
		ErrNoMilestoneOccurrence, identifier, m)
}

func lastLabelTransition(events []core.ClaimEvent, kind core.ClaimEventKind, labels ...core.StateLabel) (int64, bool) {
	var id int64
	var found bool
	for _, ev := range events {
		if ev.Kind != kind {
			continue
		}
		for _, l := range labels {
			if labelIs(ev.Subject, l) {
				id, found = ev.ID, true
			}
		}
	}
	return id, found
}

func labelIs(subject string, label core.StateLabel) bool {
	return strings.EqualFold(subject, "ben:"+string(label))
}

// FindPR is the §9.7 leg-3 read (SPEC §8.2): the *open* pull request published
// on a branch, or nil.
//
// The open filter is applied here and not left to SetPR, because that is where
// the real adapter applies it — it asks GitHub for open PRs and then re-checks
// state and head ref anyway, since a server-side filter is "not a guarantee we
// let decide evidence" (github FindPR). A fake that handed back a closed PR
// would model an adapter that does not exist, and would exercise verifier
// paths (verify.ErrPRNotOpen) only a broken adapter can reach.
func (t *Tracker) FindPR(_ context.Context, _ core.Issue, branch string) (*core.PR, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.findPRReads++
	if t.failFindPR != nil {
		return nil, t.failFindPR
	}
	pr, ok := t.prs[branch]
	if !ok || pr.State != "open" || pr.Branch != branch {
		// Absence is the answer, not an error: a pushed branch with no open PR
		// is what "the agent pushed but PR creation failed" looks like.
		return nil, nil
	}
	copied := *pr
	return &copied, nil
}

// SetPR publishes a pull request on a branch, which is what the agent doing
// step 3 of the workflow's publishing instructions looks like to the tracker.
// A PR that is not open, or is on another branch, is stored and then filtered
// by FindPR exactly as the adapter filters it.
func (t *Tracker) SetPR(branch string, pr core.PR) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.prs[branch] = &pr
}

// SetFailFindPR makes leg 3 unreadable, for asserting that verification which
// cannot be completed is never success (SPEC §9.7).
func (t *Tracker) SetFailFindPR(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failFindPR = err
}

// FindPRReads counts leg-3 reads, which is how a test observes that the git
// legs settled a case without asking the tracker at all.
func (t *Tracker) FindPRReads() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.findPRReads
}

// Snapshot returns the recorded writes so far.
func (t *Tracker) Snapshot() (calls []string, labels map[string]core.StateLabel, comments map[string][]core.MilestoneComment) {
	t.mu.Lock()
	defer t.mu.Unlock()
	calls = append([]string(nil), t.Calls...)
	labels = map[string]core.StateLabel{}
	for k, v := range t.Labels {
		labels[k] = v
	}
	comments = map[string][]core.MilestoneComment{}
	for k, v := range t.Comments {
		comments[k] = append([]core.MilestoneComment(nil), v...)
	}
	return calls, labels, comments
}

// CommentsFor returns the milestone comments posted for an issue, in order.
//
// Under the lock, and returning a copy, because the orchestrator posts from its
// effect goroutine: a test ranging over the Comments map directly is racing the
// code under test, and the race detector says so — eventually, which is the worst
// way to find out.
func (t *Tracker) CommentsFor(identifier string) []core.MilestoneComment {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]core.MilestoneComment(nil), t.Comments[identifier]...)
}

// Milestones returns the milestone kinds posted for an issue, in order.
func (t *Tracker) Milestones(identifier string) []core.Milestone {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []core.Milestone
	for _, c := range t.Comments[identifier] {
		out = append(out, c.Milestone)
	}
	return out
}

// ReleaseCount reports how many times a claim was dropped.
// dispatchableNow recomputes §8.3's verdict, all five conditions in one place.
//
// One place because Release used to restate a subset inline, and a subset is
// how it came to omit first the label partition and then open blockers. Each
// omission had the same consequence: an issue the daemon had just decided was
// not its work — released precisely for that reason — was handed straight back
// as eligible, re-dispatched, and re-run. The adapter computes the verdict per
// read from all five (github eligibleIgnoringBlockers + hasOpenBlocker), so a
// fake that answers from fewer is not a fake of it.
func (t *Tracker) dispatchableNow(iss *core.Issue) bool {
	if iss.State != "open" || len(iss.Assignees) > 0 || hasStateLabel(iss.Labels) {
		return false
	}
	for _, want := range t.RequiredLabels {
		if !containsFold(iss.Labels, want) {
			return false
		}
	}
	for _, b := range iss.Blockers {
		if b.Open {
			return false
		}
	}
	return true
}

func (t *Tracker) ReleaseCount(identifier string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Released[identifier]
}

// Label reports the last state label projected for an issue.
func (t *Tracker) Label(identifier string) core.StateLabel {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Labels[identifier]
}

// Delete removes an issue entirely, so Get reports core.ErrIssueNotFound —
// the deleted-or-transferred case of SPEC §9.8.
func (t *Tracker) Delete(identifier string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.issues, identifier)
}

// SetFailRelease makes Release return err until cleared.
func (t *Tracker) SetFailRelease(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failRelease = err
}

// SetGetResult overrides what Get reports for one identifier — a nil issue
// means "not found" — so a test can make Get and the assignee-filtered list
// reads disagree, which is the consistency lag §9.8 refuses to treat as
// evidence. Safe to call while the loop is running.
func (t *Tracker) SetGetResult(identifier string, issue *core.Issue) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.GetResult[identifier] = issue
}

// SetFailGet makes Get return err until cleared.
func (t *Tracker) SetFailGet(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.FailGet = err
}

// ReleaseAttempts counts every Release call for an issue, successful or not.
func (t *Tracker) ReleaseAttempts(identifier string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.releaseAttempts[identifier]
}

// SetLabelGate installs a function called at the top of every
// SetStateLabels, so a test can hold a projection open and observe what the
// orchestrator does — or refuses to do — while it is pending.
func (t *Tracker) SetLabelGate(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.labelGate = fn
}

// HeldClaims is the §9.8 sweep read: the principal's assignments in any
// state, with any labels. Counted separately from Fetch so a test can assert
// the sweep costs one request per tick however many claims are held.
func (t *Tracker) HeldClaims(context.Context) ([]core.Issue, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.heldReads++
	if t.failHeldClaims != nil {
		return nil, t.failHeldClaims
	}
	var out []core.Issue
	for _, iss := range t.issues {
		if containsFold(iss.Assignees, t.Principal) {
			out = append(out, *iss)
		}
	}
	return out, nil
}

// ClaimedByPrincipal is §9.10 step 1's recovery read: every issue the principal
// holds, in any tracker state, carrying any labels or none.
//
// A separate method from HeldClaims rather than a flag, because that is how the
// adapter states it (core.TrackerAdapter): the cache posture is part of the
// contract, and recovery must read origin. Counted separately for the same
// reason — a test asserting the sweep's one-request-per-tick cost must not be
// able to satisfy it with a recovery read.
//
// Deliberately unfiltered and deliberately unordered, like the map-ranging
// HeldClaims beside it: GitHub promises no order here, so a driver that depended
// on one would pass against a fake and fail against the adapter.
func (t *Tracker) ClaimedByPrincipal(context.Context) ([]core.Issue, error) {
	// Counted before the gate runs, so a test can wait for a read to be *in
	// progress* and then hold it there — the window a reload has to land in.
	t.mu.Lock()
	t.claimedReads++
	gate := t.claimedGate
	t.mu.Unlock()
	if gate != nil {
		gate()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failClaimed != nil {
		return nil, t.failClaimed
	}
	var out []core.Issue
	for _, iss := range t.issues {
		if containsFold(iss.Assignees, t.Principal) {
			out = append(out, *iss)
		}
	}
	return out, nil
}

// SetClaimedGate holds the §9.10 step 1 candidate read open, so a test can put a
// decision — a reload, a shutdown — inside the window while it is out.
func (t *Tracker) SetClaimedGate(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.claimedGate = fn
}

// SetFailClaimedByPrincipal makes the recovery candidate read fail until
// cleared — §6.4's warn-and-continue at startup.
func (t *Tracker) SetFailClaimedByPrincipal(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failClaimed = err
}

// ClaimedReads counts recovery candidate reads.
func (t *Tracker) ClaimedReads() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.claimedReads
}

// ClaimHistory returns the change log for an issue, in (At, ID) order. The
// read is counted before the gate runs, so a test can wait for one to be in
// progress and then hold it there.
//
// An issue the tracker does not have is core.ErrIssueNotFound, because the
// adapter's change-log read classifies its 404: the endpoint names one issue
// (github ClaimHistory). It is the read a claimed record reaches first, so this
// is the answer that keeps a deleted issue from holding its claim.
func (t *Tracker) ClaimHistory(_ context.Context, issue core.Issue) ([]core.ClaimEvent, error) {
	t.mu.Lock()
	t.historyReads++
	gate := t.historyGate
	t.mu.Unlock()
	if gate != nil {
		gate()
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failHistory != nil {
		return nil, t.failHistory
	}
	if _, ok := t.issues[issue.Identifier]; !ok {
		return nil, errIssueGone(issue.Identifier)
	}
	// The labels the issue was installed with come first: they were applied when
	// it was filed, and the ids say so. Everything scripted or recorded since
	// follows, which is the (At, ID) order the adapter's own read is sorted into.
	out := append([]core.ClaimEvent(nil), t.labelLog[issue.Identifier]...)
	return append(out, t.History[issue.Identifier]...), nil
}

// SetGetGate and SetHistoryGate hold a read open, so a test can order an
// asynchronous read against a decision the loop makes while it is out.
func (t *Tracker) SetGetGate(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.getGate = fn
}

func (t *Tracker) SetHistoryGate(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.historyGate = fn
}

// SetClaimGate holds the claim *write* open, which is the only honest way to
// produce the one deletion race a claim can lose: `Fetch` answers from a listing
// this tracker may already have moved past, so the issue can be deleted between
// the candidate read and the assignment request (see Claim). Deleting it before
// dispatch instead would model a world where Fetch returned an issue that was
// already gone, which is a different fixture and a weaker one.
func (t *Tracker) SetClaimGate(fn func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.claimGate = fn
}

// appendEvent records one change-log entry, stamping the tracker's next id and
// the current time. Callers hold the lock.
//
// Every event is dated, because every event on the real tracker is: GitHub stamps
// created_at on each timeline entry, and §9.10 step 2 dates evidence against the
// claim-establishing assignment — "evidence means evidence dated after that
// event". A fake whose events carried the zero time would leave every claim cycle
// undated, so code that compares against the anchor could never be exercised
// through it, and a previous cycle's failure reason would look exactly like this
// cycle's.
//
// Never goes backwards, which is what keeps ClaimHistory's promised (At, ID) order
// true: `now` only advances (SetNow), and a scripted event's own timestamp is left
// alone.
func (t *Tracker) appendEvent(identifier string, ev core.ClaimEvent) {
	t.nextEventID++
	ev.ID = t.nextEventID
	if ev.At.IsZero() {
		ev.At = t.now
	}
	// Never earlier than the event before it, over the log ClaimHistory actually
	// returns — the install-time label entries *and* everything recorded since. A
	// fixture can script a dated event, and the labels an issue was filed with are
	// dated at its CreatedAt, so an ordinary write stamped `now` can otherwise sort
	// before either. ClaimHistory promises (At, ID) order, and a caller that derives
	// the claim cycle from an unordered log reads a live assignment as superseded.
	if last := t.lastEventAt(identifier); ev.At.Before(last) {
		ev.At = last
	}
	t.History[identifier] = append(t.History[identifier], ev)
}

// lastEventAt is the timestamp of the newest event in the log ClaimHistory would
// return for an issue. Callers hold the lock.
func (t *Tracker) lastEventAt(identifier string) time.Time {
	var at time.Time
	for _, log := range [][]core.ClaimEvent{t.labelLog[identifier], t.History[identifier]} {
		if n := len(log); n > 0 && log[n-1].At.After(at) {
			at = log[n-1].At
		}
	}
	return at
}

// SetNow moves the time the tracker stamps on subsequent events, for a fixture
// that needs one thing to have happened before another — a failure in a previous
// claim cycle, say (SPEC §9.10 step 2).
func (t *Tracker) SetNow(at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.now = at
}

// Now reports the time the tracker is currently stamping.
func (t *Tracker) Now() time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.now
}

// AppendHistory adds events to an issue's change log with ids after every
// event recorded so far — the shape of something that happened *now*, which
// is what a fixture simulating a human close or reopen wants. SetHistory is
// for scripting what happened before the daemon ever looked.
func (t *Tracker) AppendHistory(identifier string, events ...core.ClaimEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ev := range events {
		t.appendEvent(identifier, ev)
	}
}

// SetFailHistory makes ClaimHistory return err until cleared.
func (t *Tracker) SetFailHistory(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failHistory = err
}

// An issue the tracker does not have refuses every call that names it, in one of
// two shapes — and which shape belongs to which call is the adapter's contract,
// not this fake's convenience (core.ErrIssueNotFound).
//
// errIssueGone is the classified answer, for the three reads that ask about one
// issue by identifier: `Get`, `ClaimHistory`, and the §9.5 content read. The loop
// routes on it, forgetting the record without asking the tracker to unassign
// something that is gone (SPEC §9.8).
//
// errAbsentIssue is the unclassified refusal, for the writes. GitHub answers 404
// there too, but the same status on the same call also means a sub-resource is
// missing — a label another actor removed first — so the adapter wraps it raw and
// promises nothing. A fake that promised the sentinel here would let a caller
// classify a *write* failure as absence, pass, and then classify nothing in
// production; that is how #49's half-fix survived review twice. What a caller may
// rely on is that the write failed: an owed write that errors is retried at the
// head of the record's queue, and absence is learned from the reads above.
func errIssueGone(identifier string) error {
	return fmt.Errorf("%w: %s", core.ErrIssueNotFound, identifier)
}

func errAbsentIssue(identifier string) error {
	return fmt.Errorf("fake tracker: issue %s does not exist", identifier)
}

func hasStateLabel(labels []string) bool {
	for _, l := range labels {
		if len(l) >= 4 && strings.EqualFold(l[:4], "ben:") {
			return true
		}
	}
	return false
}

// HeldReads and HistoryReads report the sweep's request cost.
func (t *Tracker) HeldReads() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.heldReads
}

func (t *Tracker) HistoryReads() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.historyReads
}

// GetReads counts per-issue reads. The §9.8 sweep is specified to cost none:
// a Get per held claim is the O(held)-per-tick shape it exists to avoid.
func (t *Tracker) GetReads() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.getReads
}

// GetReadsFor counts the reads spent on one issue. A total cannot say whether a
// bounded pass is rotating or re-reading the same prefix; this can.
func (t *Tracker) GetReadsFor(identifier string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.getPerIssue[identifier]
}

// SetHistory scripts an issue's change log, replacing whatever is there. The
// tracker's id counter moves past the scripted ids, so anything recorded
// afterwards — a claim, a close — orders after them.
func (t *Tracker) SetHistory(identifier string, events ...core.ClaimEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.History[identifier] = events
	for _, ev := range events {
		if ev.ID > t.nextEventID {
			t.nextEventID = ev.ID
		}
		// The clock moves with the script, for the same reason the ids do: what is
		// recorded next has to sort after what was scripted, and `now` is what stamps
		// it. Without this a scripted event dated in the future makes every
		// subsequent write sort before it.
		if ev.At.After(t.now) {
			t.now = ev.At
		}
	}
}

func containsFold(haystack []string, needle string) bool {
	for _, h := range haystack {
		if strings.EqualFold(h, needle) {
			return true
		}
	}
	return false
}

// SetFailHeldClaims makes the §9.8 sweep read fail until cleared.
func (t *Tracker) SetFailHeldClaims(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.failHeldClaims = err
}
