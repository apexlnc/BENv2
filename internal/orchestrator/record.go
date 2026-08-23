package orchestrator

import (
	"fmt"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Record is the loop's per-issue state. SPEC §9.1 keeps attempt and failure
// reason as *fields* rather than states: the closed §7.3 taxonomy already
// distinguishes what a wider state enum would exist to encode.
//
// Only the authority goroutine touches these. Nothing here is safe to read
// from another goroutine, which is why Snapshot exists.
type Record struct {
	// Issue is the record's view of the tracker's issue, and its title and body
	// are the **pin**: the content a trusted principal approved, held against
	// approvedAt and rendered into every attempt's prompt (SPEC §9.5).
	//
	// Pinning the issue itself rather than keeping an `Approved` field beside it
	// is what makes rendering from the pin the default and drift the exception.
	// Every prompt, every template variable, every log line reads this one value,
	// so there is no second spelling of "the issue" for a refresh to overwrite —
	// which is exactly how the defect arrived: three sites assigning a fresh
	// issue wholesale, and a prompt re-rendered from whichever assignment landed
	// last. The refresh path now has to say what it carries forward
	// (adoptRouting), and title and body are not on that list.
	Issue core.Issue
	State State
	// approvedAt is the approving instant the pin was taken against: the
	// standing `labeled` event of the last required label applied (SPEC §9.5).
	//
	// It is what tells an unchanged approval from a fresh one at the next
	// dispatch decision. A labeler re-applying a required label moves the
	// instant, and only that moves the pin — the §9.2 human re-queue resumes a
	// parked run and approves nothing (§6.7: the label is the only approval act).
	approvedAt time.Time
	// claimVerified records that Claim came back verified, so the §9.5 check is
	// this record's remaining gate before it may leave `queued`. It is what
	// retryPendingExits re-drives: a change-log or content read that failed is a
	// question BEN could not ask, not an answer, and the claim is retained while
	// it asks again.
	claimVerified bool
	// approvalInFlight is the check's single in-flight slot, so a tick arriving
	// while the reads are out does not start a second pair.
	approvalInFlight bool
	// claimEpoch is the positive tracker-native ID of the assignment event that
	// established this claim. claimBaseInFlight is the provider's durable
	// pending write; until it lands the record remains queued, projects nothing,
	// and cannot prepare (SPEC §6.2, §9.5).
	claimEpoch        int64
	claimBaseInFlight bool
	// epochFaulted is a sticky park under the current assignment. Removing the
	// needs-review label cannot manufacture a missing or contradictory local
	// safety fact, so applyParked restores budgets and re-projects the park until
	// the principal is unassigned and a later assignment creates a new epoch.
	epochFaulted      bool
	epochFaultDetail  string
	claimBaseDispatch bool

	// Attempt counts dispatches of this issue, 1-based, and increments on
	// both retry tracks (SPEC §9.6).
	Attempt int
	// Turns counts continuation sessions consumed; max_turns bounds it. Reset
	// by a human re-queue, which restores the run budgets (SPEC §9.8).
	Turns int
	// attemptBase is the attempt count at the last re-queue. max_attempts is
	// measured from there, while Attempt keeps counting: §9.10's "attempt ≥ 2"
	// and the template's {% if attempt %} both read it as *work may already
	// exist*, which a re-queue does not undo.
	attemptBase int
	// FailureReason is the §7.3 verdict of the most recent failure, if any.
	FailureReason core.FailureReason
	// lastOutcome is the prior attempt's prompt-facing outcome. Empty means
	// this record has no prior outcome — including when git evidence raised a
	// fresh claim's attempt floor from 1 to 2 (SPEC §5.6, §9.6).
	lastOutcome string
	// previousAttempt is the account of the attempt before this one, and what
	// `run.previous_attempt` renders fenced (SPEC §5.6, §9.6 — see summary.go).
	// Empty on lastOutcome's occasions plus the continuation track, and it binds
	// the empty string rather than null — an untrusted variable cannot be guarded,
	// so a null would be unrenderable (template.untrustedOptional).
	//
	// Composed when an outcome is routed and read at prompt render, never
	// re-derived there: a render-time derivation would read a workspace the next
	// attempt may already be preparing. In memory only — a fresh host after a
	// restart has no account to give, which §9.10 step 6's existing degradation
	// language already covers.
	previousAttempt string
	// outputTail is the bounded tail of what the running attempt has said, and
	// outputTotal how much it said altogether. Both accumulate from
	// core.EventProgress as events arrive, and both reset when the next attempt
	// is prepared.
	//
	// From the event stream rather than the retained transcript, and not because
	// the file would be worse: TranscriptStore exposes only Open, so there is
	// nothing to read one back with, and the file is not addressable per attempt
	// anyway (harness.DirTranscripts names by workspace and start instant). The
	// text arrives already redacted — handle.emit covers core.Event.Text precisely
	// because this retains it (SPEC §10.3).
	outputTail  string
	outputTotal int
	// attemptFacts is the git account of what the finished attempt left on the
	// branch; attemptFactsRead reports that the branch was read at all, which is a
	// different fact from its being empty. Read once the workspace has fallen
	// quiet and before the outcome routes.
	attemptFacts     core.AttemptFacts
	attemptFactsRead bool
	// summarizing is the account's single in-flight slot and summarized its
	// completion. applyOutcome will not route until the read has reported, so no
	// retry can be dispatched against a half-composed account (see beginSummary).
	summarizing bool
	summarized  bool
	// parkOnBudgetPending is §9.9's park waiting on that read. It exists because a
	// budget breach stops the run rather than waiting for one to end, so it is the
	// one attempt end that does not route through applyOutcome and needs its own
	// resume point (see resumeAfterAccount).
	parkOnBudgetPending bool

	// Workspace is set once Prepare succeeds.
	Workspace core.Workspace
	// Continuation is the resume token from the last started event, carried
	// into the next attempt on the continuation track.
	Continuation string
	// SessionID is the same started event's session identity (SPEC §7.4). Held
	// apart from Continuation because core.Event holds them apart: both current
	// adapters mint one string that serves as both, and the event model does not
	// require that. §10.3 asks for the *session id* as a correlation attribute,
	// so it is read from the field that means it.
	SessionID string
	// PRURL is the published pull request, set at the `done` verdict. The
	// held-claim record the run converts into carries it (SPEC §9.8).
	PRURL string

	// Definition is the workflow snapshot this attempt started under. Held
	// per-record so a reload cannot change the ground under a live run
	// (SPEC §5.4).
	Definition *config.WorkflowDefinition

	// handle is the live run, if any.
	handle core.RunHandle
	// cancelRun stops the goroutine pumping this run's events.
	cancelRun func()
	// costUSD accumulates reported usage for budget enforcement (SPEC §9.9).
	// Cumulative over the issue, which is the unit max_cost_usd is a cap on,
	// and therefore never reset by a retry.
	costUSD float64
	// attemptStartedAt, attemptUsage, attemptAgent and attemptRecorded belong to
	// the attempt rather than to the record, and all four reset in beginPrepare
	// (#60).
	//
	// attemptUsage is deliberately *not* the same accumulation as costUSD, for
	// the reason above: the cap asks what this issue has spent so far, and the
	// outcome record asks what this attempt spent. One field serving both would
	// have to reset for the second and must not for the first.
	//
	// attemptAgent is pinned beside Definition and for its reason: an attempt
	// runs under the configuration in force when it launched, and a reload that
	// lands while it runs must not change what its record says ran. Reading the
	// bundle at the *outcome* would do exactly that, and the resulting row would
	// attribute a run to a model that was configured after it finished — which is
	// the one thing #62's comparison cannot survive.
	attemptStartedAt time.Time
	attemptUsage     core.Usage
	attemptAgent     core.AgentDescriptor
	attemptRecorded  bool
	// stopping records that a stop has been asked for but not confirmed; the
	// claim is retained until it is (SPEC §9.8).
	stopping bool
	// keepOnStop and stopReason carry the reconciliation verdict across the
	// stop: §9.8 keeps the workspace when an issue went unroutable and
	// disposes it when the issue went terminal.
	keepOnStop bool
	stopReason string
	// gone records that the tracker positively said this issue is absent —
	// deleted or transferred, stated with core.ErrIssueNotFound rather than
	// inferred from a failed read (SPEC §9.8).
	//
	// It exists because the exit differs: there is no claim to release, and
	// asking for one is a 404 the owed queue retries forever. Set at each site
	// that reads the fact and carried across a stop, since reconciliation can
	// learn it while a run is still being wound down.
	gone bool
	// claimLost records that the tracker positively said the principal is not
	// assigned — the assignment *is* the claim (SPEC §8.3), so the claim is
	// already gone rather than waiting to be dropped.
	//
	// Beside `gone` because it selects the same exit for the same reason: there is
	// no claim to release, and a Release that changes nothing is still a write on
	// the serial queue that the owed machinery would retry. It differs from `gone`
	// in what it says — the issue exists, it is simply not ours — which is what the
	// operator log needs and what keeps the disposal's `keep` argument honest.
	claimLost bool
	// absenceInFlight is the one confirming Get this record may have out — the
	// parked record absent from the sweep read (SPEC §9.8, parked.go), or the
	// record whose owed tracker write keeps failing (absence.go). One per record
	// whichever asked: starting a second every tick is the O(records) cost both
	// budgets exist to avoid, and the two ask the same question of the same issue.
	absenceInFlight bool
	// owedWriteFailed says the head of the owed queue is a *tracker* write whose
	// last attempt failed — which is what makes this record a candidate for that
	// confirmation. Set only for effectTracker, and cleared whenever the head is
	// retired, so it holds exactly while a write that may be failing because the
	// issue is gone is standing at the head (see onEffectDone).
	//
	// A write's own refusal never classifies (#134): its 404 also means a missing
	// sub-resource. So the flag records that the question is worth asking, and the
	// `Get` answers it.
	owedWriteFailed bool
	// pending counts workers in flight for this record — a Prepare, a Start,
	// a Verify. A record with pending work may not be forgotten: its result
	// would arrive with nobody to own the workspace or the process it
	// produced.
	pending int
	// finish is a stop-and-exit deferred until pending work reports back.
	finish *finishRequest
	// releasing tracks an exit whose claim removal has not been confirmed yet.
	// stopInFlight is the handle's single signal-ladder slot: without it the
	// tick that begins a stop would immediately retry it, and the exit and the
	// quiescence probe — which ask the same handle the same question — would
	// race two answers to it (see beginStop).
	// suspended marks a record shutdown has taken over (SPEC §9.8, §11). It is
	// not an exit: nothing is released, disposed, projected or routed, and the
	// claim and state label are left standing for §9.10 to resume from. What it
	// does is stop the record moving — see exiting — while the drain waits for
	// the one thing shutdown does insist on, a confirmed termination.
	suspended bool
	// verifyRetry marks a §9.7 check whose publish credential failed transiently,
	// or whose mandatory tracker epoch read failed, so the next poll tick
	// re-issues it (SPEC §8.5, §9.7).
	//
	// A flag rather than a timer, because either retry rides the poll tick and not
	// §9.8's attempt backoff: no attempt is being retried. The record stays in
	// `verifying` with its claim and its `ben:claimed` label standing, the
	// attempt is neither ended nor recorded, and no verdict is routed — so an
	// abrupt restart mid-wait leaves exactly the state §9.10 already knows how to
	// resume.
	verifyRetry  bool
	releasing    bool
	stopInFlight bool
	// probeInFlight is the observation's own slot, separate from stopInFlight
	// because the two are different operations on the same fact: a probe that is
	// still out must not stop a Done from starting the cleanup it enables, and a
	// probe answered after that point cannot stand in for the stop's result
	// (#79, see onProbed).
	probeInFlight bool
	// owed are the tracker writes this record still has to land, in order;
	// owedInFlight is the head being attempted. See owed.go.
	owed         []owedEffect
	owedInFlight bool
	// disposalOwed says the workspace has already been queued for return, so a
	// second exit does not queue another. The exit *can* be reached twice: a
	// record that disposed on its way out and then had its release fail is finished
	// again when the Get confirms its issue is gone (absence.go), and `done`
	// disposes before its writes land. A second Dispose of a directory the first
	// one removed fails, and a failing local effect is retried from the head
	// forever — so the record would never be forgotten at all.
	//
	// First call wins, including its `keep` argument: the disposal was decided and
	// queued under the facts known then, and reaching into a queued effect to
	// change it is the sweep owed.go refuses to do for the same reason.
	disposalOwed bool
	// claimBaseAbandonOwed makes the pending-epoch rollback exactly once on an
	// ordered exit. It precedes claim release, closing the crash window in which
	// the tracker claim could disappear while its abandoned pending record stayed
	// behind to block every later assignment.
	claimBaseAbandonOwed bool
	// ranThisAttempt and afterRunFired make the §6.5 after-run hook exactly
	// once per attempt that actually started a process. Both reset when the
	// next attempt is prepared.
	ranThisAttempt bool
	afterRunFired  bool
	// eventsClosed records that this attempt's event stream is closed — the
	// adapter has nothing further to say (SPEC §7.4). groupGone records that its
	// whole process group is confirmed gone, which is the fact that means the
	// workspace is free (SPEC §7.5, §9.8).
	//
	// Three facts, tracked separately because they permit different things: the
	// stream closing permits a non-signalling observation, Done permits an active
	// cleanup, and only a confirmed termination permits reuse or release. Making
	// progress depend on Done alone is what left a run with a surviving leader
	// stuck in `running`, unprobed and unlogged (#79).
	eventsClosed bool
	// handleDone records RunHandle.Done: the process is reaped and its transcript
	// is complete. It is a *phase edge*, not permission — it selects whether the
	// group may be observed only (Probe) or actively cleaned (Stop), and
	// authorizes neither reuse nor release (#79).
	handleDone bool
	groupGone  bool
	// outcome is what this attempt reported, held until the workspace is
	// quiet. See holdOutcome.
	outcome *runOutcome
	// convertInFlight is the conversion pipeline's single in-flight slot: the
	// history read that resolves the claim-cycle anchor, and the Get that
	// confirms ownership when the log cannot (see held.go).
	convertInFlight bool
	// generation invalidates timers from superseded attempts: a backoff or
	// continuation timer that fires after its attempt was overtaken must not
	// act. It counts attempts within this record and starts at zero for the
	// next record on the same issue, so it cannot separate one record's work
	// from its successor's — token is what does that.
	generation int
	// instance is the daemon instance this record belongs to — the component of
	// runID that makes it unique in a log that outlives the process. See runID.
	instance string
	// token is this record's process-unique identity (see newToken). Results
	// that can outlive the record they were started for are keyed to it.
	token int
	// recovered marks a record §9.10 reconstructed rather than dispatch created.
	// Its state was restored to match what the tracker already says, so it did not
	// arrive by a §9.2 edge.
	recovered bool
	// unclassified is a candidate recovery holds but could not account for: a read
	// failed, or the classifier reached no verdict. It is *not* an exit and not a
	// verdict — it is the absence of one, and the record exists only so dispatch
	// cannot claim the issue a second time while we work out what it is
	// (SPEC §9.10: a read that failed is not an absence either).
	//
	// The loop deliberately does nothing with such a record: StateQueued projects
	// no label, is outside the reconciliation read's state set, and owes no writes.
	// retryRecovery is the only thing that moves it.
	unclassified bool
	// recoverInFlight is retryRecovery's single slot, so a tick every poll interval
	// cannot stack candidate reads on a tracker that is already failing them.
	recoverInFlight bool

	// nextTimerAt and nextTimerKind describe the §9.6 wake-up this record is
	// waiting on, for `ben status` — §10.3 names "next backoff timers" among
	// what it shows, and it is the field that separates *stuck* from *waiting*.
	//
	// Advisory, and deliberately not authority: the timer is the goroutine
	// armTimer starts, and nothing reads these to decide anything. A stale one
	// misreports a status line; it cannot fire, suppress, or double a wake-up.
	nextTimerAt   time.Time
	nextTimerKind timerKind

	UpdatedAt time.Time
}

// runOutcome is a finished run's verdict, waiting for its workspace to fall
// quiet before anything acts on it.
type runOutcome struct {
	event core.Event
	// detail is the operator-facing line the failure track reports.
	detail string
}

// finishRequest is a stop-and-exit waiting on in-flight work.
type finishRequest struct {
	keepWorkspace bool
	why           string
}

// runID is this attempt's correlation handle: the value the daemon's log lines
// carry, the child sees as BEN_RUN_ID, and the §9.11 log and run record join on
// (SPEC §10.3, §7.6).
//
// Derived rather than stored, so it cannot disagree with the attempt it names.
// The daemon instance is in it because the log it is written to outlives the
// process: token restarts at 1 on every start, so without an instance component
// two different attempts across a restart both read as `7-1.0` in one durable
// file — which is the one thing a correlation handle may not do. token is
// process-unique per record and generation counts attempts within one, so the
// three together identify an attempt exactly, and all three are deterministic
// under the manual clock, which is what makes them assertable.
//
// Attempt is deliberately not in it: §9.6 lets a fresh claim's floor move from 1
// to 2 after Prepare reads git evidence, so an identity built from Attempt would
// change halfway through the attempt it identifies.
func (r *Record) runID() string {
	if r.instance == "" {
		return fmt.Sprintf("%s-%d.%d", sanitizeRunID(r.Issue.Identifier), r.token, r.generation)
	}
	return fmt.Sprintf("%s-%s-%d.%d", sanitizeRunID(r.Issue.Identifier), r.instance, r.token, r.generation)
}

// sanitizeRunID keeps a run id to characters that cannot surprise a shell, a
// filename, or a log reader — it reaches all three. Same rule, and the same
// reason, as harness.sanitizeName.
func sanitizeRunID(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

// Snapshot is the copyable view of a record, for `ben status` and tests.
type Snapshot struct {
	Identifier    string
	State         State
	Attempt       int
	Turns         int
	FailureReason core.FailureReason
	Branch        string
	UpdatedAt     time.Time
	RunID         string
	// NextTimerAt is zero when no §9.6 wake-up is armed.
	NextTimerAt time.Time
	NextTimer   timerKind
	// Continuation is the resume token the next attempt would carry, and
	// SessionID the session it belongs to (§10.3).
	Continuation string
	SessionID    string
}

func (r *Record) snapshot() Snapshot {
	return Snapshot{
		Identifier:    r.Issue.Identifier,
		State:         r.State,
		Attempt:       r.Attempt,
		Turns:         r.Turns,
		FailureReason: r.FailureReason,
		Branch:        r.Workspace.Branch,
		UpdatedAt:     r.UpdatedAt,
		RunID:         r.runID(),
		NextTimerAt:   r.nextTimerAt,
		NextTimer:     r.nextTimerKind,
		Continuation:  r.Continuation,
		SessionID:     r.SessionID,
	}
}

// exiting reports whether the record is already on its way out of the
// machine, or out of this *process*. Reconciliation must not re-decide for one:
// the exit is in progress, and retryPendingExits is what re-drives its
// unfinished half.
//
// suspended is here rather than beside it because every one of this predicate's
// readers wants the same answer for it. They all ask some form of "may I start
// the next thing for this record?" — prepare it, route its outcome, convert it
// to a held claim, re-decide it from a refresh — and during a drain the answer
// to all of them is no. The one reader that must tell the two apart is
// onStopped, which checks suspended first (SPEC §9.8, §11).
func (r *Record) exiting() bool {
	return r.orderedExit() || r.suspended
}

// orderedExit reports an exit the loop decided on: reconciliation found the
// issue terminal or unroutable, a budget breached, an attempt failed. It is
// what shutdown *completes* rather than replaces (SPEC §9.8 as amended): the
// release was already ordered when the signal arrived, so suspending the record
// instead would strand a claim the daemon had already decided to let go.
func (r *Record) orderedExit() bool {
	return r.releasing || r.stopping || r.finish != nil || r.gone || r.claimLost
}

// markGone records that the tracker positively said this issue is absent.
//
// A setter for one field, because the field is easy to set and easy to forget
// what it is for: it makes the record an ordered exit. Without that,
// reconciliation re-decides it on every tick — `stopping` clears when the stop
// confirms, and nothing else was holding it — and each tick appends another
// disposal and another forget to a queue that only grows. None of the extras
// ever run, so nothing fails and nothing looks wrong; the queue just grows on a
// record that can sit for hours behind a failing disposal.
func (r *Record) markGone() { r.gone = true }

// markClaimLost records that the tracker positively said the claim is not ours.
// An ordered exit for the same reason markGone is one: without it every tick
// re-decides the record and appends another disposal to a queue that only grows.
func (r *Record) markClaimLost() { r.claimLost = true }

// hasWorkspace reports whether Prepare has produced one to dispose of.
func (r *Record) hasWorkspace() bool { return r.Workspace.Path != "" }

// adoptRouting takes the tracker's fresh answer for everything the §9.8 sweep
// rules read, and leaves the approved content standing (SPEC §9.5).
//
// The three sites that used to assign `r.Issue = fresh` wholesale are what put
// unapproved bytes in front of an agent: the §9.6 re-fetch, the steady-state
// reconciliation of a live record, and the refresh of a parked one. All three
// still need the routing facts — terminal state, the label partition, ownership,
// blockers — because that is how a run learns it must stop.
//
// An **allowlist**, and the direction matters. Copying `fresh` and restoring two
// fields would be a blocklist: a field added to core.Issue tomorrow would flow
// into the prompt silently, which is this ticket's defect returning through a
// different door. Naming what is carried means a new field is simply not carried
// until someone classifies it, and TestIssueFieldsAreClassified fails until they
// do.
//
// Identifier and CreatedAt are absent because they do not change for an issue;
// Title and Body are absent because they are the pin.
func (r *Record) adoptRouting(fresh core.Issue) {
	r.Issue.State = fresh.State
	r.Issue.Labels = fresh.Labels
	r.Issue.Assignees = fresh.Assignees
	r.Issue.Blockers = fresh.Blockers
	r.Issue.URL = fresh.URL
	r.Issue.UpdatedAt = fresh.UpdatedAt
	r.Issue.Revision = fresh.Revision
	r.Issue.Dispatchable = fresh.Dispatchable
}

// pin binds approved content to the instant it was approved at (SPEC §9.5).
//
// Called at exactly two points, and both are approvals rather than refreshes:
// the claim read-back that admits a fresh dispatch, and a later dispatch
// decision that finds the approving instant has *moved* — a labeler having
// re-applied a required label over content they have now seen. Nothing else
// writes it. A reconciliation tick, an unpark, or a re-dispatch under the same
// approving instant all leave it exactly as it was.
//
// The argument is what enforces that rather than the comment: only checkApproval
// can produce an approvedContent, so a caller that has not run §9.5's check has
// nothing to pass.
func (r *Record) pin(a approvedContent) {
	r.Issue.Title, r.Issue.Body = a.content.Title, a.content.Body
	r.approvedAt = a.at
}
