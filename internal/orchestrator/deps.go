package orchestrator

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The orchestrator's dependencies are declared here as the narrow slices it
// actually uses, not as the full adapter contracts. core.TrackerAdapter
// satisfies Tracker structurally, and so does a fake with seven methods instead
// of eleven — which is the point: the loop cannot come to depend on readiness
// checks or kind registration, and changes to those cannot reach it.

// Tracker is the queue mechanics the loop needs (SPEC §8.2), plus the one
// startup read Recover adds.
type Tracker interface {
	Fetch(ctx context.Context) ([]core.Issue, error)
	Get(ctx context.Context, identifier string) (*core.Issue, error)
	Claim(ctx context.Context, issue core.Issue) (bool, error)
	Release(ctx context.Context, issue core.Issue) error
	SetStateLabels(ctx context.Context, issue core.Issue, label core.StateLabel) error
	Comment(ctx context.Context, issue core.Issue, comment core.MilestoneComment) error
	// HeldClaims is the §9.8 held-claim sweep read: one ETag-conditional
	// request per tick, whatever the review backlog.
	HeldClaims(ctx context.Context) ([]core.Issue, error)
	// ClaimedByPrincipal is §9.10 step 1: every issue the claim principal holds,
	// in any tracker state, carrying any labels or none — the recovery candidate
	// set (SPEC §8.2).
	//
	// A second method beside HeldClaims rather than a flag on it, because the
	// cache posture is part of the contract. HeldClaims is ETag-conditional, and
	// a 304 there means "nothing changed since *this daemon* last looked" — a
	// sentence with no meaning across a restart. Recovery must read origin.
	ClaimedByPrincipal(ctx context.Context) ([]core.Issue, error)
	// ClaimHistory answers the two questions current state cannot: which
	// assignment established this claim cycle, and whether a close inside it
	// has since been undone by a reopen (SPEC §9.8, §9.10 step 2). Also
	// cache-bypassing, for the same reason.
	ClaimHistory(ctx context.Context, issue core.Issue) ([]core.ClaimEvent, error)
}

// Workspaces is the workspace lifecycle the loop drives, plus its claim-scoped
// safety/evidence operations (SPEC §6.1, §6.2, §9.6). They remain off
// core.WorkspaceProvider: the orchestrator is the consumer that knows the
// tracker assignment epoch and must compare it before hooks or verification.
type Workspaces interface {
	// BeginClaimBase durably records pending intent before ben:claimed can be
	// projected. A successful return is the queued→claimed precondition.
	BeginClaimBase(ctx context.Context, issue core.Issue, epoch int64) error
	// PrepareClaim validates the expected epoch on every prepare. Its local fact
	// is non-zero only for pending→pinned and is measured against the outgoing
	// pin before the current base is installed.
	PrepareClaim(ctx context.Context, issue core.Issue, attempt int, epoch int64) (core.Workspace, core.LocalBranchFacts, error)
	// ClaimBase reads the closed provider state without preparing or running
	// hooks; recovery gates §9.7 on it.
	ClaimBase(ctx context.Context, issue core.Issue) (core.ClaimBase, error)
	// AbandonPendingClaimBase rolls an unfinished transition back to its
	// outgoing pin, or to absence when it had none. The loop calls it only after
	// the workspace is quiet and before ending the tracker claim, so a later
	// assignment can establish a new epoch without overwriting a live intent.
	AbandonPendingClaimBase(ctx context.Context, issue core.Issue) error
	Dispose(ctx context.Context, ws core.Workspace, keep bool) error

	// The three below are §9.10's workspace precondition: a workspace whose
	// previous run is not confirmed gone may not be reused, disposed, or
	// released. Within one process the run handle answers that; across a restart
	// only what was written down before the crash can.
	//
	// Required rather than discovered like afterRunner, deliberately. A provider
	// that cannot be asked has told us nothing, and §9.10 reads "nothing" as
	// possibly-live — so a discovered seam's absent case is a daemon that
	// recovers no issue at all. A compile error is the better failure.
	//
	// Keyed by issue, not by workspace key: providers own key and branch naming
	// (SPEC §6.3), and recovery has no workspace yet to read one off.

	// BeginRunMarkerFor records that a run may be live in this issue's workspace.
	// Called before the launch and durable on return — a marker still in the page
	// cache when the machine dies is a workspace that reads as free with an agent
	// starting in it.
	BeginRunMarkerFor(issue core.Issue) error
	// ReadRunMarkerFor reports what the previous tenure left behind.
	ReadRunMarkerFor(issue core.Issue) (core.RunMarker, error)
	// ClearRunMarkerFor frees the workspace. The caller must have confirmed the
	// run is gone: §9.10 admits exactly one route to removal, positive evidence of
	// absence, because every other reading is a guess and the guess that frees a
	// live workspace puts a second agent in a worktree.
	ClearRunMarkerFor(issue core.Issue) error

	// ListWorkspaces reports every workspace on disk and the issue it belongs to,
	// for SPEC §9.10 step 5's sweep. An empty Identifier means nothing records whose
	// the directory is, which is a fact the caller must not paper over: §6.4 keeps a
	// failure's workspace, so "unowned" is not "disposable".
	ListWorkspaces(ctx context.Context) ([]core.WorkspaceRef, error)

	// ResolveWorkspace names the workspace an issue's work lives in and reports
	// its pinned claim epoch/base pair, preparing nothing.
	//
	// Recovery needs it because §9.7's evidence question has to be asked *before*
	// a verdict says an attempt is owed, and Prepare would run this attempt's
	// hooks and mint a pin on a decision nobody has made. `false` authorizes no
	// evidence read; ClaimBase tells recovery whether to resume or park.
	ResolveWorkspace(ctx context.Context, issue core.Issue) (core.Workspace, bool, error)
	// AttemptFacts is the git account of what a finished attempt left on its
	// branch, for the next attempt's prompt (SPEC §9.6, §5.6). Required rather
	// than discovered like the after-run hook, because a provider that quietly
	// lacked it would render an emptier prompt with nothing to notice: a missing
	// method should be a compile error, not a silently worse handoff.
	AttemptFacts(ctx context.Context, ws core.Workspace) (core.AttemptFacts, error)
}

// afterRunner is the optional §6.5 after-run hook. B05 deliberately kept it
// off the three-method core interface, so it is discovered rather than
// required: a provider that has one gets it called after every attempt ends,
// and one that does not is unaffected.
type afterRunner interface {
	AfterRun(ctx context.Context, ws core.Workspace)
}

// contentApprovalSource is §9.5's content read, discovered on the tracker the
// same way the after-run hook is discovered on the workspace provider. It is
// core.ContentApprovalSource restated as the slice the loop uses — the reason
// every interface in this file is restated (see the note at the top).
//
// Discovered rather than required, and the two differ in what absence means. A
// provider with no after-run hook owes nothing; a tracker that cannot date a
// content edit has *not answered* §9.5's question, and the loop refuses to
// dispatch on an unanswered question. readApproval is where that is spelled out.
type contentApprovalSource interface {
	ContentApproval(ctx context.Context, issue core.Issue) (core.ContentApproval, error)
}

// Runner starts one attempt (SPEC §7.1).
type Runner interface {
	Start(ctx context.Context, spec core.RunSpec) (core.RunHandle, error)
}

// Verdict is what verification concluded about a finished run (SPEC §9.7).
// B08 owns the routing; B09 owns the evidence that produces the verdict.
type Verdict int

const (
	// VerdictUnknown is the zero value and never an answer. `done` is the one
	// irreversible verdict — it clears the state projection, posts the publish
	// milestone and disposes the workspace — so it must require explicit
	// construction. A zero value meaning "published" would make §9.7's "errors
	// never count as success" true only by each caller's diligence, and the
	// first careless `VerifyResult{}` would publish a run nobody verified.
	//
	// It mirrors verify.VerdictUnknown deliberately. The two enums stay
	// distinct — this one is what the loop routes on, that one is what the
	// evidence says — so the shim between them (#7) has to be an exhaustive
	// switch: a numeric cast between distinct types with identical member
	// names compiles, and mistranslates the moment either one grows.
	VerdictUnknown Verdict = iota
	// VerdictPublished — all three legs of the §9.7 evidence check hold.
	VerdictPublished
	// VerdictIncomplete — a clean exit that simply has not published yet.
	// Routes to the continuation track.
	VerdictIncomplete
	// VerdictContradicted — evidence contradicts the agent's claim. Parks.
	VerdictContradicted
)

func (v Verdict) String() string {
	switch v {
	case VerdictUnknown:
		return "unknown"
	case VerdictPublished:
		return "published"
	case VerdictIncomplete:
		return "incomplete"
	case VerdictContradicted:
		return "contradicted"
	default:
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// Verifier answers the §9.7 question from git and tracker facts. The loop
// treats it as a black box and fails closed on error: a verification that
// cannot be completed is never success.
type Verifier interface {
	Verify(ctx context.Context, issue core.Issue, ws core.Workspace) (VerifyResult, error)
}

// RuntimeSource is the configuration the loop reads: one snapshot, and the
// channel that says it has moved. config.RuntimeSource satisfies it structurally,
// and so does a test's own cell — which is the point, in the same way the four
// adapter interfaces above are narrower than the contracts that satisfy them. The
// loop needs to *read* the configuration; it must not come to depend on being
// able to write it, because exactly one thing may (SPEC §5.4).
type RuntimeSource interface {
	// Load returns the configuration in force and the channel that closes when it
	// next moves. Both under one acquisition: read apart, a reload landing between
	// them pairs an interval from one revision with a wake from the next, and the
	// next has not fired.
	Load() (config.Snapshot[*Bundle], <-chan struct{})
}

// Clock is the time seam. Tests drive backoff and continuation timers without
// sleeping through them.
type Clock interface {
	Now() time.Time
	// After behaves like time.After.
	After(d time.Duration) <-chan time.Time
}

type realClock struct{}

func (realClock) Now() time.Time                         { return time.Now() }
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// Config is what the loop is built with. The adapters are deliberately not
// fields here: they arrive with the definition they were built from, through the
// runtime source, so no decision can pair one with a definition it was never
// checked Ready against (SPEC §5.4, §5.7).
type Config struct {
	// Runtime is the configuration in force — definition, adapters, dispatch
	// verdict and revision as one value, published by the config watcher.
	// Required.
	//
	// Read at each decision's linearization point, and the snapshot then carried
	// through that decision rather than re-read per question. There is
	// deliberately no loop-owned copy beside it: a private mirror refreshed at
	// the next tick is a second answer to "which configuration governs this",
	// and §5.4 hands dispatch, retry scheduling, reconciliation, hooks and
	// launches to the reload *when it lands*, not when the loop next gets round
	// to it. A mirror made every one of those decisions answer under a
	// configuration a human had already replaced, for as long as a poll interval.
	Runtime RuntimeSource

	// Revalidate is §9.4 step 2: defensive config revalidation before each
	// dispatch cycle, reporting what is in force afterwards. A blocked verdict
	// skips dispatch for the tick and nothing else, never reconciliation.
	//
	// It returns the whole snapshot rather than only a verdict because
	// revalidation is what discovers a reload the watch missed (SPEC §5.4): a
	// hook that answered only "blocked?" would leave the loop dispatching the
	// stale configuration it had just proved out of date. B11 wires it to the
	// watcher's Revalidate. Nil means never blocked and never reloaded.
	//
	// It MUST report the same *revision* while the file has not changed. The
	// loop discards work whose revision has been superseded — that is the
	// point — so a hook that re-versioned an identical configuration on every
	// call would invalidate reads nothing had superseded, on every tick.
	Revalidate func(ctx context.Context) config.Snapshot[*Bundle]

	// PrepRetryable classifies a workspace Prepare failure. SPEC §9.2 has a
	// retryable prep edge (→ backoff) and a non-retryable one (→ failed),
	// and §6.6 fails closed on ambiguity — so nil means a prep failure never
	// retries. B11 wires one that recognizes its provider's hook-failure
	// sentinel; keeping it out here is what keeps the loop provider-agnostic
	// (SPEC §6.1).
	PrepRetryable func(error) bool

	// RunGone asks §9.10's precondition of a run this process never started:
	// is the run this evidence identifies confirmed gone?
	//
	// It reports `true` only on proof of absence. Everything else — a probe that
	// errored, a scheme it does not recognize, evidence whose boot identity
	// cannot be matched — is `false`, which recovery reads as possibly live. The
	// asymmetry is the whole contract: `false` costs a retained claim and another
	// tick, `true` on a live group puts a second agent in a worktree (SPEC §7.5:
	// only ESRCH proves disappearance).
	//
	// A seam rather than a direct call into the harness, because the evidence
	// scheme is named rather than assumed (core.RunEvidence): a remote substrate
	// answers the same question with a session id, and the loop must not come to
	// depend on the local process substrate to ask it.
	//
	// Nil means this daemon cannot ask. An identified marker is then possibly
	// live — never free — and recovery says so by name rather than silently
	// waiting forever on a question nobody is asking.
	RunGone func(core.RunEvidence) (bool, error)

	// FailureReasons is §9.10 step 6's read of the §9.11 transition log: the
	// failure reason of an issue's last run, if it survived the restart.
	//
	// B11 owns writing the log to the state dir; this is the whole of what
	// recovery needs back. Nil is a *capability* absence and not a fact: see
	// Recover, which warns at startup naming what this daemon cannot report,
	// because an unimplemented dependency and a genuine cold start otherwise
	// produce the same comment.
	FailureReasons FailureReasonReader

	// Log receives the transition log (SPEC §9.11) and operator errors.
	Log *slog.Logger
	// Clock defaults to the real one.
	Clock Clock
	// DaemonID identifies this daemon in the transition log.
	DaemonID string
	// Instance names this run of the daemon, and defaults to the start instant.
	// It is a component of every run id, because run ids are written to a log
	// that outlives the process (see Record.runID). Tests set it to keep run ids
	// stated rather than derived from a clock.
	Instance string

	// Transitions makes the §9.11 log durable. B11 wires it to the state dir;
	// nil leaves the log in-memory only, which is what every test that does not
	// assert persistence runs with.
	//
	// The consequence of nil is already written down: §9.10 step 6's failure
	// reason does not survive a restart, and the `failed` comment recovery
	// reconstructs says so rather than inventing one.
	Transitions TransitionSink

	// Attempts makes the attempt-outcome log durable (#60), on the same terms.
	// Nil costs telemetry and nothing else: no decision in BEN reads an outcome
	// record back, which is what keeps §9.10's statelessness invariant intact.
	Attempts AttemptSink
}
