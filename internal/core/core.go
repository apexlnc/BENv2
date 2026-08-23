// Package core holds the shared domain types and component interfaces that
// parameterize the orchestrator: the normalized issue model, the agent runner
// contract with its closed event and failure enums, the tracker adapter
// contract, and the workspace provider contract (SPEC §6.1, §7, §8).
//
// Adapters translate the outside world into these types at the boundary; the
// core never sees a raw agent event or a raw provider payload (SPEC §3.6).
package core

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// ErrIssueNotFound says the issue no longer exists (deleted, transferred), as
// against "could not reach the tracker" — the distinction reconciliation turns
// on (SPEC §9.8).
//
// Exactly three calls carry it, and all three are reads: TrackerAdapter.Get,
// TrackerAdapter.ClaimHistory, and ContentApprovalSource.ContentApproval. Each
// asks about one issue named by identifier, so a provider's not-found answer to
// one of them is about that issue and nothing else.
//
// **The write set never carries it, and the asymmetry is the decision rather
// than an omission** (#134). Two reasons, and the second is the one that would
// be expensive to get wrong.
//
// A write's not-found answer is not about the issue alone. Its request names a
// sub-resource — a label, an assignee — whose own absence produces the same
// answer, and this adapter set already depends on that reading: removing a
// `ben:*` label a competing actor removed first is a 404 the GitHub adapter
// treats as success, on the same call a deleted issue would 404. A status that
// already means two things at one call site cannot be promoted into a fact about
// the issue. Trackers also answer not-found for a resource the credential cannot
// see, which is not the same as one that does not exist.
//
// And the two directions of a wrong verdict do not cost the same. Absence is
// what lets the loop *forget* a record: the claim died with the issue, so there
// is nothing left to release (SPEC §9.8). Concluding that from a write's refusal
// is the fail-open direction — it drops a claim that may still be standing, and
// an issue left assigned with no state label is read as
// published-awaiting-review and never revisited (SPEC §9.10 step 3). A read
// wrongly retried costs a tick. So a caller that needs the answer after a failed
// write asks Get, which is the call whose not-found means one thing.
var ErrIssueNotFound = errors.New("issue not found on the tracker")

// ErrClaimNotAttempted marks a TrackerAdapter.Claim refusal reached before any
// assignment could have been written: a spent request budget, a standing
// rate-limit refusal, an identifier that would not parse. It is a statement
// about the *tracker* — nothing the adapter did could have assigned the issue —
// so an adapter may carry it only where that holds.
//
// The default reading of a claim error is the opposite one, deliberately: an
// error that cannot rule out a standing assignment obliges the caller to release
// it, because assigned-with-no-state-label is read as published-awaiting-review
// and never revisited (SPEC §9.10 step 3). That unwinding release is itself a
// write, so paying it for a refusal that never wrote spends exactly the capacity
// the refusal was reporting gone.
var ErrClaimNotAttempted = errors.New("claim was refused before any assignment was attempted")

// ConfigValueError is a structural refusal (SPEC §5.7) anchored to one
// config field, carrying the offending value as data — Error() never prints
// it. Whether showing the value would leak a secret depends on its
// provenance, which only the config layer knows, and mandatory redaction
// (SPEC §5.8) cannot be retrofitted onto freeform text: %q-escaping and
// substring collisions both defeat scrubbing after the fact. Renderers that
// hold provenance decide (config.RenderRefusal).
type ConfigValueError struct {
	// Field is the dotted config path, e.g. "tracker.provider.repo",
	// matching the loader's provenance keys.
	Field string
	// Value is the offending value as the adapter saw it — possibly a
	// resolved secret, deliberately absent from Error().
	Value string
	// Sensitive is the producer's assertion that this value carries a
	// credential whatever its provenance — a URL whose authority may hold
	// userinfo, say. Provenance cannot answer that: the secret is in the value's
	// *shape*, and a literal written into the file is exactly as exposed as an
	// env-resolved one once the refusal is pasted into a pull request or a CI log
	// (#52). Renderers MUST redact a value marked here.
	//
	// It is not a substitute for Kind.SensitiveFields, which names paths that are
	// always secret. This names the one value in hand, at a path that usually is
	// not.
	Sensitive bool
	// Err is the named refusal (ErrX) tests assert on.
	Err error
}

func (e *ConfigValueError) Error() string { return e.Err.Error() }

func (e *ConfigValueError) Unwrap() error { return e.Err }

// Issue is the normalized, tracker-agnostic work item (SPEC §8.3).
type Issue struct {
	// Identifier is stable and unique within tracker scope; it names
	// workspaces and branches. For GitHub it is the issue number as a string.
	Identifier string
	Title      string
	Body       string
	Labels     []string
	// State is the provider-native state name, compared case-insensitively.
	State     string
	Assignees []string
	Blockers  []Blocker
	URL       string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Revision is an opaque per-issue change token: compare it for equality,
	// never interpret it. It answers one question — might this issue have gone
	// terminal since we last looked? — and is defined over the SPEC §8.3
	// **revision projection**, not over the whole polled issue: state, the
	// tracker's reason for the most recent state change (GitHub state_reason),
	// and UpdatedAt.
	//
	// The projection is exhaustive: adapters MUST derive the token from exactly
	// those elements — no fewer, no more — and move it whenever any of them
	// changes. No fewer, because UpdatedAt alone is second-granularity
	// (SPEC §8.4) and holds still across a close-and-reopen sharing one second,
	// the reopen the held-claim sweep exists to catch (SPEC §9.8). No more,
	// because a title, body, label, assignee, or comment count cannot mean the
	// issue went terminal, and each would buy a change-log read per edit in the
	// one place per-issue reads were ruled out.
	//
	// Narrow is sufficient because this token gates one rule only. Every other
	// sweep rule reads its own fact from the same response — terminal state from
	// State, partition membership from Labels, ownership from presence — so a
	// same-second label change is caught by the rule that reads labels.
	//
	// Equality means only "the tracker attests nothing projected changed". It is
	// a trigger for looking closer, never a verdict on its own — absence of a
	// fact is never evidence (SPEC §9.10).
	Revision string
	// Dispatchable is the adapter-computed eligibility verdict: required
	// labels present, state active, unclaimed by any party, zero open
	// blockers, no ben:* state label.
	Dispatchable bool
}

// Blocker is a normalized blocked-by relation entry.
type Blocker struct {
	Identifier string
	State      string
	Open       bool
}

// IssueContent is the author-controlled span of an issue (SPEC §5.6): the
// title and body a labeler approves and a prompt renders.
//
// Named apart from Issue because §9.5 treats exactly these two fields
// differently from every other one. The rest of an issue is the tracker's own
// answer and keeps tracking it; these two are pinned to the moment a trusted
// principal approved them.
type IssueContent struct {
	Title string
	Body  string
}

// ContentEditStatus is the closed set of answers a tracker can give to "when
// was this issue's author-controlled content last edited" (SPEC §9.5).
type ContentEditStatus int

const (
	// ContentEditUnknown is the zero value, and that is the point. A field
	// nobody filled, a read that failed, a tracker with no such capability at
	// all — none of them may read as "never edited". Absence of edit evidence
	// is not evidence of no edit (SPEC §9.10), and the permissive reading
	// dispatches bytes no human approved, which is the whole of what §9.5
	// exists to stop (BUILD.md decision 15). The §9.5 check refuses it.
	ContentEditUnknown ContentEditStatus = iota
	// ContentEditNever is the tracker positively stating the content has not
	// been edited since the issue was filed. A fact, not an absence.
	ContentEditNever
	// ContentEditAt is the tracker stating when the content was last edited;
	// At carries the instant.
	ContentEditAt
)

func (s ContentEditStatus) String() string {
	switch s {
	case ContentEditUnknown:
		return "unknown"
	case ContentEditNever:
		return "never"
	case ContentEditAt:
		return "at"
	default:
		return "ContentEditStatus(" + strconv.Itoa(int(s)) + ")"
	}
}

// ContentEdit is when an issue's title or body was last edited.
//
// Both halves fold into one instant deliberately. On GitHub they are separate
// facts — a title-only rename moves neither `lastEditedAt` nor
// `userContentEdits` (measured against issue #39), so the rename event is the
// *only* evidence of it — and the question §9.5 asks is about the pair. An
// adapter that can answer for one half and not the other has not answered.
type ContentEdit struct {
	Status ContentEditStatus
	// At is meaningful only when Status is ContentEditAt. Adapters report it at
	// whatever granularity the tracker gives; SPEC §8.4's second granularity is
	// what makes an edit sharing a second with the approving label unorderable
	// against it, and therefore a refusal.
	At time.Time
}

// ContentApproval is one read of the two facts §9.5's check needs: the
// author-controlled content as it now reads, and when it was last edited.
//
// One value from one read, because the check admits the *content* on the
// strength of the *edit fact*. Content from an earlier read paired with an edit
// time from a later one would leave a window between them in which an edit is
// invisible to both, and the pin taken from it would be bytes nobody approved.
type ContentApproval struct {
	Content IssueContent
	Edit    ContentEdit
}

// ContentApprovalSource is the tracker seam behind §9.5's content-bound
// approval: the read that says what an issue's content is *and* whether it has
// moved since a labeler approved it.
//
// Deliberately a separate interface rather than another TrackerAdapter method,
// for the reason RepositorySource is separate: §8.2's contract is the read
// kernel plus the closed write set, and widening it would oblige every future
// tracker to answer a question its provider may have no API for. GitHub needs
// GraphQL to answer it at all.
//
// A tracker that does not implement it has not said "never edited" — it has
// said nothing, and §9.5 refuses to dispatch on nothing. The absent capability
// therefore reads as ContentEditUnknown and parks, rather than passing.
type ContentApprovalSource interface {
	// ContentApproval reads the issue's author-controlled content together with
	// when it was last edited. An implementation that can produce the content
	// but not the edit time MUST report ContentEditUnknown rather than omit the
	// fact: the caller cannot tell an unset field from an unedited issue.
	//
	// Returns ErrIssueNotFound when the issue is gone. It is one of the three
	// calls that classify absence, because the check it serves has to tell a
	// deleted issue from a tracker that would not answer: the first parks a
	// claim nothing will ever release, the second is retried (SPEC §9.5).
	ContentApproval(ctx context.Context, issue Issue) (ContentApproval, error)
}

// PR is the publish evidence returned by TrackerAdapter.FindPR (SPEC §9.7).
type PR struct {
	Number int
	URL    string
	State  string // "open", "closed", "merged"
	Branch string
}

// StateLabel is the tracker-visible projection of orchestrator state
// (SPEC §9.3). StateLabelNone covers both queued (never labeled) and done
// (labels removed).
type StateLabel string

const (
	StateLabelNone        StateLabel = ""
	StateLabelClaimed     StateLabel = "claimed" // covers claimed/preparing/verifying/backoff
	StateLabelRunning     StateLabel = "running"
	StateLabelNeedsReview StateLabel = "needs-review"
	StateLabelFailed      StateLabel = "failed"
)

// ClaimEventKind is the closed set of tracker change-log entries recovery and
// the held-claim sweep read (SPEC §8.2). Per invariant 6 the core never sees
// the provider's raw event payload; adapters project onto these six.
type ClaimEventKind string

const (
	ClaimEventAssigned   ClaimEventKind = "assigned"
	ClaimEventUnassigned ClaimEventKind = "unassigned"
	ClaimEventLabeled    ClaimEventKind = "labeled"
	ClaimEventUnlabeled  ClaimEventKind = "unlabeled"
	// ClaimEventClosed and ClaimEventReopened carry what current tracker state
	// cannot: a close a later reopen has undone. The held-claim sweep releases
	// a retained done claim on the closed *event*, so a close-and-reopen inside
	// one poll interval is still classified from evidence rather than silently
	// left assigned (SPEC §9.2, §9.8).
	ClaimEventClosed   ClaimEventKind = "closed"
	ClaimEventReopened ClaimEventKind = "reopened"
)

// ClaimEvent is one normalized entry of the tracker's ordered change log —
// the positive evidence recovery classifies from, since the absence of a fact
// is never evidence (SPEC §9.10).
type ClaimEvent struct {
	Kind ClaimEventKind
	// Actor is the login that made the change.
	Actor string
	// Subject is the assignee login for assigned/unassigned, and the label
	// name for labeled/unlabeled. Load-bearing: recovery tells a done
	// projection from a human re-queue by *which* ben:* label a transition
	// removed (SPEC §8.2). Empty on closed/reopened, which name neither.
	Subject string
	At      time.Time
	// ID is the tracker's own monotonic change-log id. Tracker timestamps
	// can be coarser than the events they order, so ordering is by
	// (At, ID) — and ClaimHistory returns the slice already in that order.
	// It also names the claim cycle in milestone comment markers (§8.4).
	ID int64
}

// Milestone is the closed set of transitions that earn a tracker comment
// (SPEC §8.4). Making it an enum rather than free prose is what enforces "no
// per-tick spam": there is no way to say anything else (B04).
type Milestone string

const (
	MilestoneClaimed     Milestone = "claimed"
	MilestonePublished   Milestone = "published"
	MilestoneFailed      Milestone = "failed"
	MilestoneNeedsReview Milestone = "needs-review"
)

// MilestoneComment is the structured payload the adapter renders. Only the
// fields relevant to Milestone are set; daemon identity is adapter-side
// knowledge (SPEC §8.4) and is never passed in.
type MilestoneComment struct {
	Milestone Milestone
	// PRURL is the published pull request (MilestonePublished).
	PRURL string
	// Reason is the §7.3 failure reason (MilestoneFailed).
	Reason FailureReason
	// ReasonUnavailable says the §7.3 reason did not survive a restart, and
	// is the only honest alternative to Reason on MilestoneFailed. The reason
	// lives in the local run record, which a fresh host does not have
	// (SPEC §9.10 step 6): recovery must then say so rather than invent a
	// reason or skip the comment, because a ben:failed label with no
	// explanation is worse than an honest one. Setting both, or neither, is
	// a refusal.
	ReasonUnavailable bool
	// Detail is one line of operator-facing context, optional on every
	// milestone.
	Detail string
}

// TrackerConfig is the tracker's whole slice of the workflow definition: the
// opaque provider block *and* the core-owned tracker fields alongside it.
// Structural validation takes them together because a rule like "required
// labels must be non-empty" spans both, and validating a new provider block
// against a previous reload's core fields would be a silent hot-reload bug
// (SPEC §5.7).
type TrackerConfig struct {
	// Provider is the verbatim tracker.provider block, $VAR-resolved by the
	// config loader and validated by the adapter — the core never inspects
	// it (SPEC §5.2.5).
	Provider map[string]any
	// RequiredLabels is the opt-in label set; adapters refuse an empty one
	// (BUILD.md decision 9).
	RequiredLabels []string
	// ActiveStates/TerminalStates nil means the adapter default applies.
	ActiveStates   []string
	TerminalStates []string
	// WorkflowKey identifies this daemon in tracker-visible writes
	// (SPEC §5.1, §8.4).
	WorkflowKey string
}

// TrackerOptions is what assembly supplies when *constructing* a tracker
// (SPEC §8, amendment 9) — deliberately not TrackerConfig, which is what
// Structural validates.
//
// The difference is the credential. Structural is asked about the file as
// written, including the legacy in-provider spellings; New is handed the
// compiled result, in which there is exactly one credential path. Every legacy
// spelling has already become an implicit source by the time this value exists,
// so Credential is never nil and no adapter needs a nil-means-legacy branch.
type TrackerOptions struct {
	// Provider is the tracker.provider block **reduced**: `token`,
	// `credential_source` and `claim_assignee` are excluded, because each has
	// been promoted to a field of its own here or to Credential.
	//
	// Leaving any of them in would hand the adapter a second way to reach the
	// credential beside Credential — the two-paths ambiguity the implicit-source
	// compilation exists to remove — and a key that survives in the map is a key
	// somebody will read from there.
	Provider       map[string]any
	RequiredLabels []string
	// ActiveStates/TerminalStates nil means the adapter default applies.
	ActiveStates   []string
	TerminalStates []string
	// WorkflowKey identifies this daemon in tracker-visible writes
	// (SPEC §5.1, §8.4).
	WorkflowKey string
	// ClaimAssignee is the machine-user account claims name (SPEC §8.4). Empty
	// preserves the credential-authenticated login fallback — which a
	// workload-identity credential is statically known not to yield, so that
	// combination is refused at load rather than here.
	ClaimAssignee string
	// Credential is the tracker credential's source. Never nil.
	Credential Source
}

// TrackerKind is one tracker.kind, registered at package level: the two
// entry points that exist before any instance does (SPEC §5.7, §8.2).
// Validation cannot be a method on the constructed adapter — a malformed
// config fails during construction, leaving no instance to ask — so the
// structural check is a property of the kind, which is what lets
// `ben config effective` report a bad block for an adapter it could never
// have built (SPEC §5.8).
type TrackerKind interface {
	// Structural reports whether the configuration is well-formed. PURE: it
	// must not touch the network, the filesystem, or a subprocess, and must
	// be answerable with no credentials present (SPEC §5.7).
	Structural(cfg TrackerConfig) error
	// New constructs the adapter, binding opts to the instance. Structural
	// failures surface here too; nothing else does.
	New(opts TrackerOptions) (TrackerAdapter, error)
	// CredentialRefs names where this kind's *tracker credential* comes from
	// (SPEC §10.2), so the loader can prove it never reaches an agent process.
	// PURE, and about references only: see CredentialRefs.
	CredentialRefs(provider map[string]any) CredentialRefs
	// SensitiveFields names the provider paths whose value is a secret, for
	// display redaction (SPEC §5.8). See the shared documentation below.
	SensitiveFields(provider map[string]any) [][]string
}

// SensitiveFields — the contract both kinds implement (SPEC §5.8, §10.2).
//
// It answers "is this a credential", which is a different question from the one
// provenance answers, and the two were conflated. Redaction used to hide a
// provider value only when it was `$VAR`-resolved, on the reasoning that a
// literal in the file is already public to anyone who can read the repo. That is
// true of the *repo* and not of the *output*: `config effective` is pasted into
// pull requests, issues and CI logs, and WORKFLOW.md need not be committed at
// all. So provenance answers "can this reader already see it" — which is the
// right question for a refusal that quotes an offending value — and sensitivity
// answers "is this a credential", which is the right question for display.
//
// A path is given as its key segments from the block root — {"env", "GH_TOKEN"}
// for provider.env.GH_TOKEN — because the renderer owns how a path is spelled
// (non-identifier keys are quoted) and two spellings of one path would silently
// fail to match. A path naming a map or a list is redacted whole rather than
// descended into.
//
// PURE: no filesystem, no network, and answerable with nothing installed, since
// `config effective` must redact for an adapter it could never have built.
// Tolerant of a malformed block, for the same reason as #47's methods: Load does
// not run Structural, so this is asked of blocks nobody has validated yet.

// CredentialRefs names where a kind's secrets come from, as **references**
// rather than values (SPEC §10.2, §5.5). It is what lets the loader refuse a
// workflow that hands the tracker credential to an agent (§6.7): the tracker
// credential authorizes issue writes, so an agent holding it can rewrite the
// queue that dispatched it — strip `ben:*`, take the assignment, close the
// issue, claim more work.
//
// A kind answers about its own block, which is opaque to everyone else, and
// answers in references because by the time anyone asks, the block holds
// resolved secrets: §5.5's `$VAR` indirection is applied at load, so the
// variable name survives only in the loader's provenance. Comparing resolved
// bytes instead would mean holding two secrets side by side to compare them,
// would risk echoing one in the refusal, and would false-positive two genuinely
// distinct credentials that happen to be equal mid-rotation.
type CredentialRefs struct {
	// Fields are paths into the provider block whose *value* is a secret, each
	// given as its map keys from the block root: {"env", "GH_TOKEN"} for
	// provider.env.GH_TOKEN. The loader maps each to the variable the value was
	// resolved from.
	//
	// Segments rather than a joined path because the loader owns how a path is
	// spelled — non-identifier keys are quoted — and two spellings of one path
	// would silently fail to match, which fails in the permissive direction.
	//
	// A path that was not `$VAR`-resolved contributes nothing: a literal secret
	// in the file has no variable identity to collide on.
	Fields [][]string
	// Vars are variables this kind reads or forwards *by name*, whatever the
	// block's values are: a documented fallback the file never mentions, or an
	// `env_passthrough` entry, which is a name by construction.
	Vars []string
}

// TrackerAdapter is the normalized read kernel plus the closed write set
// (SPEC §8.2). The orchestrator decides when to call these; the adapter
// decides how they render on the tracker. It is deliberately not a CRUD
// state enum.
type TrackerAdapter interface {
	// Ready reports whether the bound configuration can operate now:
	// credentials resolve, the tracker answers (SPEC §5.7). It owns
	// everything that can fail because the world is not set up rather than
	// because the config is wrong — including resolving a credential the
	// config omitted in favor of a documented environment fallback
	// (SPEC §5.8).
	Ready(ctx context.Context) error
	// Fetch returns normalized candidate issues (ETag-aware): the queue as
	// the workflow defines it. Dispatchable is meaningful only here.
	Fetch(ctx context.Context) ([]Issue, error)
	// Get refreshes one issue regardless of labels or state — the
	// reconciliation read (SPEC §9.8), which must still see an issue whose
	// queue label a human removed or that someone closed mid-run. Returns
	// ErrIssueNotFound if the issue is gone. Dispatchable is not computed.
	Get(ctx context.Context, identifier string) (*Issue, error)
	// ClaimedByPrincipal returns the recovery candidates: every issue this
	// daemon's claim principal holds, in any state, with any labels or none
	// (SPEC §8.2, §9.10 step 1). Deliberately unfiltered — the claims most
	// in need of cleanup are exactly the ones that have left the queue
	// partition. Cache-bypassing; Dispatchable is not computed.
	ClaimedByPrincipal(ctx context.Context) ([]Issue, error)
	// HeldClaims asks ClaimedByPrincipal's question on the steady-state path:
	// the held-claim sweep that releases retained done claims (SPEC §9.8). It
	// is ETag-conditional, so a review backlog costs one request per tick and
	// no core budget — a Get per held claim would cost one *per claim* per
	// tick, and the held set grows with human review latency.
	//
	// A second method rather than a flag on ClaimedByPrincipal: the cache
	// posture is part of the contract. Recovery must read origin, and one
	// method serving both postures could hand it a cached answer.
	HeldClaims(ctx context.Context) ([]Issue, error)
	// ClaimHistory returns the issue's ordered claim, label, and state
	// evidence (SPEC §8.2, §9.10 step 2). Also cache-bypassing. Returns
	// ErrIssueNotFound if the issue is gone — the same fact Get states, and it
	// is stated here because this is the read a caller reaches first for an
	// issue it already holds (see ErrIssueNotFound for why the write set does
	// not).
	ClaimHistory(ctx context.Context, issue Issue) ([]ClaimEvent, error)
	// FindPR returns the *open* PR published on branch, or nil — the third
	// leg of the §9.7 evidence check, which names an open PR specifically.
	// Workspace providers own branch naming (SPEC §6.3), so verification
	// passes the resolved branch explicitly.
	FindPR(ctx context.Context, issue Issue, branch string) (*PR, error)
	// Claim writes the claim and verifies it landed by read-back
	// (SPEC §8.4). false means the claim did not stick (e.g. lost race);
	// the adapter has already released a partial claim in that case.
	//
	// An error means the adapter could not finish, and the caller must assume
	// an assignment may be standing — unless the error carries
	// ErrClaimNotAttempted, which promises no write was made.
	Claim(ctx context.Context, issue Issue) (bool, error)
	Release(ctx context.Context, issue Issue) error
	SetStateLabels(ctx context.Context, issue Issue, label StateLabel) error
	// Comment posts a structured milestone comment — the only prose the
	// orchestrator ever writes (SPEC §3.3, §8.4). Idempotent per milestone
	// kind and claim cycle: re-issuing one is a no-op, which is what lets
	// recovery complete an interrupted terminal projection instead of
	// choosing between skipping the comment and double-posting it.
	Comment(ctx context.Context, issue Issue, comment MilestoneComment) error
}

// RemoteAuth is a git remote credential — for the base repo, the tracker
// credential of SPEC §10.2, which "also authenticates base-clone git fetch".
// It is delivered to git through a credential helper reading the child
// environment, never argv (`ps`-visible) and never on-disk config.
//
// Username is not treated as a secret (for GitHub PATs it is a placeholder
// such as "x-access-token"); Password is additionally redacted from all errors
// and logs, which is why the secret half is a field of its own rather than
// userinfo inside a URL.
type RemoteAuth struct {
	Username string
	Password string
}

// Repository is a git repository and the source of the credential that fetches
// it: the whole of what a repository-backed workspace strategy needs in order to
// exist (SPEC §6.2 base clone, §10.2 credentials).
type Repository struct {
	// RemoteURL is the URL the base repo fetches from and stores as its
	// `origin`. It MUST be credential-free: it reaches git as argv, and agents
	// push through the same remote with their own publish auth (SPEC §6.7,
	// §10.2). The credential belongs behind AuthSource.
	RemoteURL string
	// AuthSource authenticates the daemon's own fetches, resolved immediately
	// before each remote git invocation. nil means unauthenticated — a public
	// repo, a file remote, or ambient credential helpers.
	//
	// A source rather than a value, because a value has a lifetime: built once
	// at construction, it is the string a rotated credential leaves behind, and
	// every fetch after the rotation fails with a credential nothing can
	// refresh. It also owns its complete exchange binding, including any scope,
	// so this struct carries and derives no credential scope of its own
	// (SPEC §11).
	AuthSource RemoteAuthSource
}

// RepositorySource is the seam between a tracker and a repository-backed
// workspace strategy (SPEC §6.2, §10.2): the tracker adapter owns the provider
// block and the credential resolved from it (SPEC §5.2.2, §5.8), so it — never
// the core — is what turns a provider-block repository into a git remote. The
// alternative is the core parsing `tracker.provider.repo`, which §5.2.5 and
// invariant 6 forbid.
//
// Deliberately a separate interface rather than two more TrackerAdapter
// methods: §8.2's contract is the read kernel plus the closed write set, and
// widening it would oblige every future tracker — including one whose issues
// have no git repository at all — to answer a git question. Assembly asks for
// this contract by type assertion and refuses when it is absent (see
// workspace.RepositoryFrom).
type RepositorySource interface {
	// Repository returns the repository this tracker's issues live in.
	//
	// It MUST be called after Ready has succeeded, and MUST report what
	// readiness established rather than establishing anything itself: no
	// network I/O, no credential resolution, no environment fallback. Those
	// belong to Ready alone (SPEC §5.7, §5.8) — a second resolution path would
	// mean a second set of failure modes, reached by whoever happened to ask
	// first. An implementation asked too early MUST refuse; returning an
	// unauthenticated or guessed remote is not an option.
	Repository(ctx context.Context) (Repository, error)
}

// ClaimPrincipalSource is the identity a tracker's claims are assigned to
// (SPEC §8.4, §10.1). Assembly needs it because the orchestrator does: §9.8
// asks whether an issue is still ours, and "exactly one assignee" is not that
// question — a human who unassigned BEN and took the issue themselves satisfies
// it while the claim the run depends on is gone.
//
// Separate from RepositorySource for the same reason RepositorySource is
// separate from TrackerAdapter: they answer about different things, and a
// tracker may legitimately implement one and not the other. It is the same
// shape otherwise, refusal included, because the principal is a property of the
// credential and the credential is resolved in exactly one place.
type ClaimPrincipalSource interface {
	// ClaimPrincipal returns the login this adapter's claims are assigned to.
	//
	// It MUST be called after Ready has succeeded, and MUST report what
	// readiness established rather than establishing anything itself: no
	// network I/O, no credential resolution, no environment fallback. An
	// implementation asked before Ready — or after one that returned an error,
	// however far into its own sequence that error arrived — MUST refuse. A
	// readiness that did not complete has established nothing, and an
	// implementation that caches this value on the way through would answer for
	// a world nobody finished probing.
	ClaimPrincipal(ctx context.Context) (string, error)
}

// RequestBudget is a tracker that bounds what one tick may spend on API
// requests, and says afterwards what the tick cost (SPEC §8.5).
//
// A capability discovered by assertion, like RepositorySource and
// ClaimPrincipalSource, and for the same reason: §8.2's contract is the read
// kernel plus the closed write set, and a tracker whose API has no meterable
// cost — or none worth bounding — should not owe an answer here. Absence is
// therefore not a refusal; it means "this tracker does not meter itself".
//
// The *tick* is the window because the tick is the unit whose cost the daemon
// controls: §9.4 fixes what one costs, and §9.5 caps how many runs it can start.
// Only the component that owns ticks can name their boundaries, which is why
// this is told to the tracker rather than inferred inside it.
type RequestBudget interface {
	// BeginTick closes the window the last tick spent from, opens a new one, and
	// returns what the closed window spent. Interval is the cadence currently in
	// force; implementations use it to keep a faster-than-default poll loop from
	// multiplying the credential's hourly spend.
	//
	// It is the reporting boundary and the per-tick enforcement reset. The
	// sustainable-rate allowance does not reset: otherwise a legal shorter poll
	// interval would spend the same hourly credential many times over. An
	// implementation MUST NOT depend on being called: a tracker whose windows are
	// never opened has to keep working — degraded accounting is a caller's
	// mistake, and refusing every request afterwards would turn it into a stalled
	// daemon.
	BeginTick(interval time.Duration) RequestReport
}

// RequestControlSuccessor is a rebuilt tracker generation that can continue
// request admission state from its predecessor. Budgets and server-directed
// backoff belong to the credential and API endpoint, not to the short-lived
// adapter object assembly creates for a config reload: starting them over would
// let repeated failed reloads multiply the daemon's request rate.
//
// Assembly calls ContinueRequestControl after New and before Ready. The method
// MUST perform no I/O and may ignore a predecessor whose provider, endpoint, or
// credential is not compatible with its own request-control domain.
//
// Predecessors are offered most-authoritative first — the published generation,
// then the candidates earlier reloads constructed and discarded — and the
// implementation adopts the first compatible one. The discarded candidates are
// what makes this a list: a reload that moves the daemon to a *new* endpoint has
// no published predecessor there at all, so without them every revalidation of a
// failing config would meet that endpoint with a fresh allowance and no memory of
// the backoff it was just asked for.
type RequestControlSuccessor interface {
	ContinueRequestControl(previous ...TrackerAdapter)
}

// RequestControlDomain names the scope a tracker's request controls belong to —
// for a REST tracker, the API endpoint. Assembly retains the last candidate
// constructed for each domain, so a daemon whose config moves between endpoints
// does not let the newest failure evict the memory of the endpoint it just left.
// Which retained candidate a successor actually continues from remains the
// successor's decision (RequestControlSuccessor); this key only decides what is
// kept.
//
// The key is an identity held for the life of the process, so it MUST NOT carry
// a credential — an endpoint is the whole of what it is for. A tracker that
// cannot name a domain simply does not implement this, and shares one retained
// slot with every other such tracker.
type RequestControlDomain interface {
	RequestControlKey() string
}

// RequestReport is one window's request accounting (SPEC §8.5, §10.3). Counts
// only: the limits that produced them, and what to do about them, belong to the
// adapter that set them.
type RequestReport struct {
	// Billed counts requests the tracker's rate limit was charged for.
	Billed int
	// Unbilled counts requests it was not — on GitHub, a conditional read the
	// server answered 304. Measured rather than bounded: these are the requests
	// that make polling a large queue affordable at all, so what matters about
	// them is that they are visible.
	Unbilled int
	// Refused counts calls the budget declined because the window was spent.
	// Nonzero is a degradation to report: the work was not done, and whoever
	// asked got an error saying so.
	Refused int
	// Deferred counts continuations that waited for a later request window rather
	// than discarding partial progress. They eventually completed (or their
	// context ended), but the tick still ran slower than it looked.
	Deferred int
	// Pending counts requests that reached the network in this window but had
	// not produced a response when the window closed. Their eventual outcomes
	// appear in LateBilled or LateUnbilled, never in a later window's Billed or
	// Unbilled count.
	Pending int
	// LateBilled and LateUnbilled count outcomes received during this window for
	// requests reported pending by an earlier one. Keeping them separate makes
	// every closed-window count stable while retaining complete accounting.
	LateBilled   int
	LateUnbilled int
}

// WorkspacePaths is every path a workspace provider owns for one issue
// (SPEC §6.1). The provider chooses the layout (SPEC §6.2) and is the only
// party that knows these without inspecting the tree, so it reports them
// rather than leaving a consumer to derive them.
//
// One struct rather than three fields on each end, because these travel
// together by contract: a Workspace reports the set and a RunSpec carries the
// set (SPEC §7.1), and copying it whole is what keeps an attempt from being
// handed a private dir belonging to one workspace and a worktree belonging to
// another. A field added here reaches both ends without a seam remembering to
// forward it.
//
// Every one of them is subject to §6.3 invariant 2 — see the provider's
// containment check, which is the only place that can enforce it, since only
// the provider knows the root they must stay under.
type WorkspacePaths struct {
	// Path is the worktree: the agent subprocess cwd (SPEC §6.3 invariant 1),
	// and the one tree of the three the agent may write freely.
	Path string
	// SharedGitDir is the per-workflow bare repository the worktree is cut
	// from (`base.git`, SPEC §6.2). Reported rather than discovered: it is
	// readable from inside the worktree with `git rev-parse --git-common-dir`,
	// and the file that answers is one the agent can rewrite, so a consumer
	// that read it there could be pointed at a repository the agent chose —
	// and §6.2 reattaches, so the rewrite would survive into the next attempt
	// (SPEC §7.1).
	SharedGitDir string
	// PrivateDir is per-workspace state a harness must write but the
	// repository must not carry (SPEC §6.2). "Private" means placed and
	// disposed by BEN, outside the worktree — not unwritable: the harness
	// writes it freely. Being outside the worktree is what keeps that state
	// out of a commit, which is why §6.3 checks it separately from
	// containment.
	//
	// It shares the worktree's lifetime exactly (SPEC §6.4), so state a
	// continuation chain depends on survives every attempt in the chain.
	// Attempt-scoped children within it may be removed independently.
	PrivateDir string
}

// ClaimBaseState is the closed set of durable claim-base readings (SPEC §6.2).
// The zero value authorizes nothing: a caller that forgot to read the provider
// cannot accidentally treat that omission as an absent, pending, or pinned
// claim.
type ClaimBaseState uint8

const (
	ClaimBaseUnknown ClaimBaseState = iota
	ClaimBaseAbsent
	ClaimBasePending
	ClaimBasePinned
)

func (s ClaimBaseState) String() string {
	switch s {
	case ClaimBaseUnknown:
		return "unknown"
	case ClaimBaseAbsent:
		return "absent"
	case ClaimBasePending:
		return "pending"
	case ClaimBasePinned:
		return "pinned"
	default:
		return "ClaimBaseState(" + strconv.Itoa(int(s)) + ")"
	}
}

// ClaimBase is the workspace provider's one atomic safety fact for a claim.
// Only Pinned authorizes verification; Pending retains the outgoing pin long
// enough for the first claim-aware prepare to derive §9.6's prior-work fact.
// An outgoing epoch of zero denotes a legacy pre-epoch pin and is evidence for
// that comparison only — it never authorizes a hook, launch, or verdict.
type ClaimBase struct {
	State           ClaimBaseState
	Epoch           int64
	BaseSHA         string
	OutgoingEpoch   int64
	OutgoingBaseSHA string
}

// Workspace is the prepared per-issue working directory (SPEC §6).
type Workspace struct {
	WorkspacePaths
	Key    string // sanitized issue identifier (SPEC §6.3)
	Branch string // ben/<workspace_key>
	// ClaimEpoch and BaseSHA are the inseparable claim-scoped verification
	// base. ClaimEpoch is the positive tracker-native ID of the assignment
	// event that established the current claim; zero authorizes nothing.
	ClaimEpoch int64
	BaseSHA    string
	CreatedNow bool
}

// LocalBranchFacts is the local-only branch observation shared by §9.6's
// pre-hook fresh-claim snapshot and §9.7's first publication leg. Keeping it
// separate from PublishFacts makes the preparation seam unable to smuggle in
// a remote probe whose availability is irrelevant to the attempt floor.
type LocalBranchFacts struct {
	// BaseSHA is the base this observation was made against. During a pending
	// epoch transition it is the outgoing claim's pin, not the newly installed
	// Workspace.BaseSHA; carrying it prevents §9.6 from accidentally comparing
	// the reattached head to itself.
	BaseSHA string
	// Head is the local issue branch's head, "" when no such branch exists.
	Head string
	// DescendsBase reports that the workspace's claim-time BaseSHA is an
	// ancestor of Head.
	DescendsBase bool
}

// AdvancedPastBase reports a branch exists, has moved off the claim-time base,
// and descends from it.
func (f LocalBranchFacts) AdvancedPastBase(baseSHA string) bool {
	return advancedPastBase(f.Head, f.DescendsBase, baseSHA)
}

// PublishFacts is the git half of the §9.7 evidence check, read back from
// whichever provider owns the repository. It carries facts, never a verdict:
// the same shape routes differently depending on how the run ended (§9.6), so
// the reading belongs to the caller.
//
// Deliberately not a WorkspaceProvider method. §6.1's interface is the
// strategy seam — three methods, and widening it would make every future
// strategy owe a git answer. Consumers that need evidence declare the narrower
// seam themselves, which this provider satisfies structurally.
type PublishFacts struct {
	// Head is the local issue branch's head, "" when no such branch exists.
	Head string
	// RemoteHead is origin's head for the issue branch, "" when origin has no
	// such branch. Meaningful only when RemoteProbed.
	RemoteHead string
	// RemoteProbed reports that origin was asked at all. False means the git
	// legs were settled locally and the remote was never contacted, so
	// RemoteHead and RemoteHasHead carry no fact — distinguishing that from a
	// probe that found nothing, because absence of a fact is never evidence
	// (SPEC §9.10).
	RemoteProbed bool
	// DescendsBase reports that the workspace's claim-time BaseSHA is an
	// ancestor of Head. False on a rewritten or force-pushed branch, where
	// "advanced" cannot be asserted about this daemon's commits at all.
	DescendsBase bool
	// RemoteHasHead reports that Head is reachable from RemoteHead — the
	// "with those commits" half of §9.7 leg 2. A local branch ahead of origin
	// published some of its work, not all of it. Meaningful only when
	// RemoteProbed.
	RemoteHasHead bool
}

// AdvancedPastBase reports §9.7 leg 1: a branch exists, has moved off the
// claim-time base, and descends from it.
//
// It is also exactly the condition under which the remote legs may be read. A
// provider that cannot establish it MUST NOT contact origin and MUST leave
// RemoteProbed false — otherwise an unreachable origin would turn a
// contradiction already settled from local facts into a verification error.
// The implementation is shared with LocalBranchFacts, so preparation, the
// provider's remote-probe gate, and the §9.7 checker cannot drift.
func (f PublishFacts) AdvancedPastBase(baseSHA string) bool {
	return advancedPastBase(f.Head, f.DescendsBase, baseSHA)
}

func advancedPastBase(head string, descendsBase bool, baseSHA string) bool {
	return head != "" && head != baseSHA && descendsBase
}

// AttemptFacts is the git account of what an attempt left on its branch: the
// commits it added past the claim-time base, and the files those commits
// changed (SPEC §9.6). It is the observed half of what a retry's prompt reports
// about the attempt before it (§5.6, `run.previous_attempt`).
//
// **Every string here is agent-authored.** Git is authoritative about a commit
// existing and a path being in a tree; it says nothing about the words the agent
// chose for either, and an agent that has read a fenced issue body can put
// anything in `git commit -m` or in a filename. So none of this is a trusted fact
// about *content*, and the summary it feeds renders fenced.
//
// Bounded at the provider, because the alternative is asking git for a hundred
// thousand lines in order to throw most of them away. What was dropped is
// reported rather than left to be inferred from a suspiciously round count.
//
// Deliberately not a WorkspaceProvider method, for the reason PublishFacts gives:
// §6.1's interface is the strategy seam, and a fourth method would make every
// future strategy owe a git answer. The consumer declares the narrower seam.
type AttemptFacts struct {
	// Commits are `<abbreviated sha> <subject>` lines, newest first.
	Commits []string
	// Files are the paths those commits changed, spelled as git spells them —
	// quoted where git quotes them, since that is the fact and not a rendering
	// choice of ours.
	Files []string
	// CommitsTruncated and FilesTruncated report that the branch carried more
	// than the provider's bound allows.
	CommitsTruncated bool
	FilesTruncated   bool
}

// WorkspaceProvider is the pluggable workspace strategy seam (SPEC §6.1).
// v1 ships exactly one implementation: git worktree + per-issue branch.
type WorkspaceProvider interface {
	// IsApplicable reports whether this strategy can serve the workflow;
	// providers are consulted in priority order.
	IsApplicable(ctx context.Context) bool
	// Prepare creates or reattaches under an already established claim base —
	// never force-recreating the branch (SPEC §6.2). The returned Workspace
	// reports every path this provider owns (WorkspacePaths).
	Prepare(ctx context.Context, issue Issue, attempt int) (Workspace, error)
	// Dispose removes the workspace; keep=true preserves it for forensics.
	// The private dir goes with the worktree on both paths (SPEC §6.4).
	Dispose(ctx context.Context, ws Workspace, keep bool) error
}

// EventType enumerates the closed normalized event set (SPEC §7.2).
type EventType string

const (
	EventStarted   EventType = "started"
	EventProgress  EventType = "progress"
	EventUsage     EventType = "usage"
	EventHeartbeat EventType = "heartbeat"
	EventSucceeded EventType = "succeeded"
	EventFailed    EventType = "failed"
)

// Event is one normalized runner event. Only the fields for its Type are set.
type Event struct {
	Type EventType
	Time time.Time

	// EventStarted
	SessionID string
	// Continuation is the adapter-opaque resume token, minted here and later
	// carried back in RunSpec.Continuation. Empty if the adapter has none.
	Continuation string

	// EventProgress
	Text string

	// EventUsage
	Usage *Usage

	// EventFailed
	Reason FailureReason
}

// Usage is best-effort normalized token/cost accounting (SPEC §7.2).
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	CostUSD      float64 // 0 when the adapter cannot report cost
}

// FailureReason is the closed failure taxonomy (SPEC §7.3).
type FailureReason string

const (
	FailureCrashed        FailureReason = "crashed" // exit without a terminal event
	FailureStalled        FailureReason = "stalled" // no events past the stall window
	FailureTimeout        FailureReason = "timeout" // attempt timeout exceeded
	FailureRateLimited    FailureReason = "rate_limited"
	FailureAuth           FailureReason = "auth"
	FailureLaunchError    FailureReason = "launch_error"
	FailureKilled         FailureReason = "killed"
	FailureBudgetExceeded FailureReason = "budget_exceeded" // orchestrator-initiated (SPEC §9.9)
	// FailureCredential is a **transient** failure to obtain a credential
	// (SPEC §7.3, amendment 8). Retryable, because that is what transient
	// means. An unknown or permanent credential failure is not a run failure at
	// all: it parks (SPEC §9.2), so it never reaches this taxonomy.
	FailureCredential FailureReason = "credential"
)

// Retryable is the static verdict per reason (SPEC §7.3). The orchestrator
// applies retry policy from this alone, never from agent internals.
func (r FailureReason) Retryable() bool {
	switch r {
	case FailureCrashed, FailureStalled, FailureTimeout, FailureRateLimited, FailureCredential:
		return true
	default:
		return false
	}
}

// RunFailure is one run's §7.3 verdict as the §9.11 transition log recorded
// it: the whole of what SPEC §9.10 step 6 may take from that log, and nothing
// more.
//
// It is stated here rather than in either package that uses it because the two
// sides of that seam must not import each other. The state dir (§10.3) reads it
// off disk; recovery (§9.10) turns it into a MilestoneComment's Reason and
// Detail. A failure this type cannot account for is one recovery must report as
// not having survived — see MilestoneComment.ReasonUnavailable.
type RunFailure struct {
	// At is when the transition that recorded it happened. Carried because the
	// log has no notion of a claim cycle (§9.10 step 2), so a consumer that
	// needs one has to compare timestamps to get it.
	At     time.Time
	Reason FailureReason
	// Detail is the operator-facing line that accompanied the failure. Empty is
	// ordinary: not every failure edge has one.
	Detail string
}

// RunLimits are the orchestrator-owned per-run bounds handed to adapters via
// RunSpec (SPEC §5.2.7, §7.1).
type RunLimits struct {
	StallTimeout   time.Duration
	AttemptTimeout time.Duration
	MaxTurns       int
	MaxCostUSD     float64 // 0 = disabled
}

// RunSpec is everything an adapter needs to start one attempt (SPEC §7.1).
// The orchestrator builds it; the adapter maps it to a harness invocation.
type RunSpec struct {
	// Workspace is the path set reported by the provider that owns them
	// (SPEC §6.1). An adapter MUST NOT derive any of them — not from the
	// worktree path, not from the worktree's contents (SPEC §7.1); an adapter
	// that needs none of them beyond cwd ignores the rest.
	//
	// These are invocation inputs, not adapter configuration, which is the
	// line §7.1's bind-at-New rule draws: they say which workspace this
	// attempt runs in, never what the adapter does with it.
	Workspace WorkspacePaths
	Prompt    string
	// Continuation is the opaque token from a prior started event; empty
	// means a fresh session.
	Continuation string
	// Env carries the orchestrator's own per-run variables and nothing else:
	// every key MUST be BEN_-prefixed (SPEC §7.6). The adapter owns the whole
	// child environment, so any other key is a refusal — launch_error, not a
	// precedence decision. There is deliberately no provider block here: it
	// binds at New, so Ready cannot verify one configuration while Start
	// launches another (SPEC §7.1).
	Env    map[string]string
	Limits RunLimits
}

// EnvPrefix is the namespace reserved to the orchestrator in RunSpec.Env
// (SPEC §7.6). It is exclusive from both sides: adapters refuse a RunSpec
// carrying anything else, and refuse a provider block that defines the prefix
// in any environment surface. A one-sided namespace is not a namespace.
const EnvPrefix = "BEN_"

// EnvAllowlist is the closed set of daemon-environment variables that may
// cross into a child process: enough for one to find its tools, home,
// identity, and locale, and nothing else. Everything past it is opt-in per
// workflow (an adapter's provider.env_passthrough), so a tracker PAT or an
// agent API key sitting in the daemon's environment cannot reach a child that
// never asked for it (SPEC §7.6 — never inherited wholesale).
//
// Agent runs (§7.6) and lifecycle hooks (§6.5) read the same list because they
// are the same trust boundary: both are repo-authored instructions the daemon
// executes on its own host, and the daemon's secrets are no more theirs than
// the agent's. Two lists would be two places to widen it, and the one that got
// widened would be whichever had the weaker justification attached.
//
// USER earns its place by measurement, not by convention: with HOME and PATH
// alone, claude 2.1.221 cannot reach a keychain-stored OAuth credential and
// every run fails with "Not logged in · Please run /login". LOGNAME alone does
// not substitute for it (verified both ways against the real binary).
//
// HOME is what both harnesses resolve their stored credential and config from
// (`~/.claude`, `$CODEX_HOME` defaulting to `~/.codex`), which is why a RunSpec
// may not redirect it — see harness.CheckSpec.
var EnvAllowlist = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR",
	"LANG", "LC_ALL", "LC_CTYPE", "TERM", "TZ",
}

// StopMode selects between letting the run finish signaling (interrupt) and
// discarding it (SPEC §9.8).
type StopMode int

const (
	StopInterrupt StopMode = iota
	StopDiscard
)

// Termination reports whether a run's process *group* is gone. An unconfirmed
// termination retains the claim: a possibly-alive process must never share a
// workspace with a replacement (SPEC §9.8).
//
// It is a probe's answer, never a memory. The question a caller asks is whether
// the group is gone *now* — a verdict remembered from a ladder that ran a moment
// ago would be a second, staler answer to it, and a sticky unconfirmed would
// retain a claim forever over a group that has since died.
//
// Two operations answer it and they are not interchangeable (#79): Probe
// *observes*, Stop *acts and then observes*. Both own the same evidence — only
// ESRCH on the group proves disappearance — but only one of them signals, so a
// caller that merely wants to know must not reach for the one that kills.
type Termination int

const (
	// TerminationUnconfirmed is the zero value, and that is the point. A field
	// nobody filled, or a caller that forgot to state an answer, must not read
	// as "the workspace is free": the safe direction costs a retained claim and
	// another tick, the other one costs two agent processes in one worktree
	// (SPEC §9.8). Same reasoning as verify.VerdictUnknown.
	TerminationUnconfirmed Termination = iota
	// TerminationConfirmed means a probe of the process group answered that it
	// is gone. It is only ever stated, never arrived at by omission.
	TerminationConfirmed
)

func (t Termination) String() string {
	switch t {
	case TerminationUnconfirmed:
		return "unconfirmed"
	case TerminationConfirmed:
		return "confirmed"
	default:
		return "Termination(" + strconv.Itoa(int(t)) + ")"
	}
}

// RunHandle is a live attempt (SPEC §7.1).
type RunHandle interface {
	// Events yields the normalized stream; the adapter closes it after the
	// terminal event (succeeded/failed), which is ground truth (SPEC §7.4).
	Events() <-chan Event
	// Done is closed when the underlying process has fully ended.
	Done() <-chan struct{}
	// Probe reports whether the run's process group is gone, without touching
	// it: one fresh observation, no signal beyond the existence check, no
	// verdict claimed and no lifecycle effect (SPEC §7.1, §7.5).
	//
	// It exists because the caller that needs this answer soonest is the one
	// that must not act (#79). A run whose event stream has closed may still
	// have a process finishing its transcript, and asking with Stop would send
	// SIGTERM to a group that was about to exit on its own — trading a
	// truncated forensic record for an answer Probe gives for free.
	//
	// Only ESRCH is confirmation. A cancelled context, or any other answer,
	// is unconfirmed: the safe direction costs a retained claim and another
	// tick (SPEC §9.8).
	Probe(ctx context.Context) Termination
	// Stop walks the signal ladder and then reports the same fact Probe does.
	// It is what cleans a group that will not leave on its own; Done having
	// closed is what makes it safe to use on an ordinary run, since by then
	// anything left in the group has outlived the process that owned it.
	Stop(ctx context.Context, mode StopMode) Termination
}

// Capabilities declares what an adapter supports so the orchestrator can warn
// at config load, not mid-run (SPEC §7.1).
type Capabilities struct {
	Resume bool
	Usage  bool
}

// AgentDescriptor names which agent ran, in the two terms a later comparison
// between adapters has to group by (#60, #62): the `agent.kind` an operator
// wrote and the model that kind says the block selects.
//
// It is *not* a second copy of the configuration. Nothing decides anything from
// it — it exists so an attempt-outcome record can say which agent produced it,
// and it is resolved where the configuration and the kind registry already meet
// rather than by the loop, which never sees a provider block (SPEC §3.6).
//
// An empty Model is an answer rather than a gap: the block names none, so the
// harness's own default applied. That default is not knowable here — it is
// whatever the installed binary decides at launch — and inventing a name for it
// would put a model in the record that nothing ran.
//
// It is an answer that has to be *carried*, though, not dropped: it is the
// ordinary configuration, so a consumer that cannot see it loses the largest
// cohort in the comparison it is running. The record writes the field empty
// rather than omitting it and every renderer names it (state.Attempt.Model).
//
// The authoritative per-run answer exists and is not reachable from here:
// claude-code announces the model it resolved on its `system/init` line, and
// codex-exec announces none. Carrying it would mean a field on the §7.2 event
// model, which is closed and needs sign-off — #62's territory, not this
// descriptor's.
type AgentDescriptor struct {
	Kind  string
	Model string
}

// AgentRunner is a *constructed* harness adapter with its provider config
// already bound (SPEC §7.1). v1 ships exactly claude-code and codex-exec.
type AgentRunner interface {
	// Ready reports whether the bound configuration can actually run here:
	// binary present and identifying itself, credentials plausible —
	// everything that can fail because the world is not set up. Structure was
	// already settled by RunnerKind.Structural, before this instance existed.
	Ready(ctx context.Context) error
	Capabilities() Capabilities
	Start(ctx context.Context, spec RunSpec) (RunHandle, error)
}

// AgentConfig is the whole agent configuration a kind is asked about: the
// opaque provider block and the core-owned fields beside it (SPEC §5.2.5,
// §5.2.8).
//
// Both, not just the block, for the reason SPEC §5.7 already gives on the
// tracker side: a rule can span the two. The §7.6 reservation is one — the
// variable `publish.env` names may not be respelled in the block's generic
// environment surfaces, and may not itself name a variable the adapter owns —
// and neither half is answerable from one side alone. It is passed as one value
// so a kind cannot be asked about a new block against another generation's
// publish credential, which would be a silent hot-reload bug (SPEC §5.4).
type AgentConfig struct {
	// Provider is the verbatim agent.provider block, $VAR-resolved by the
	// loader and validated by the adapter — the core never inspects it.
	Provider map[string]any
	// Publish is the publish credential, or the zero value when the workflow
	// configures none (SPEC §5.2.8). The zero value is not an error: an agent
	// may authenticate its push from what §7.6's allowlist already carries.
	Publish PublishCredential
	// PublishSource is the reload identity of the source behind Publish
	// (SPEC §5.4, amendment 2), zero when no publish credential is configured.
	// It carries no secret and no source name: an edit beneath an unchanged name
	// rebuilds, a rename with an identical definition does not.
	PublishSource SourceBinding
	// AttemptTimeout is `limits.attempt_timeout_ms`. It lives here because the
	// publisher's readiness gate is computed against it (SPEC §7.7): the
	// timeout is something the runner is *bound to*, so an edit to it must
	// rebuild the runner and re-run Ready.
	AttemptTimeout time.Duration
}

// PublishCredential is the credential the *agent* publishes with, as a
// reference rather than a value (SPEC §5.2.8, §6.7, §10.2).
//
// A reference, because the value must not exist at load: resolving it there
// would make a workflow file unloadable on every machine that does not hold the
// secret, including the CI that load-validates the repo's own WORKFLOW.md
// (SPEC §5.5, §5.8). So the daemon holds a variable name and reads it once per
// attempt — which is also why an absent value is a Ready refusal at startup and
// a contained per-attempt failure after it, never a load error.
//
// The core names the variable and supplies the value; the adapter still writes
// the child environment (SPEC §7.6). No BEN component authenticates a git write
// with it (SPEC §6.7).
type PublishCredential struct {
	// Env is the child environment variable the resolved credential is injected
	// as, e.g. "GH_TOKEN". Empty means no publish credential is configured.
	Env string
	// Var is the daemon-environment variable holding the credential — the name
	// behind `publish.value`'s single `$VAR` reference, never the secret. Empty
	// where the credential comes from a named `credential_sources` entry, which
	// references no variable at all (SPEC §5.5).
	Var string
}

// Configured reports whether a publish credential was configured at all.
//
// Env alone, because Var is no longer the only spelling: `publish.kind: source`
// names an entry rather than a variable, and both spellings compile to the same
// implicit-or-named source. The child variable is what every remaining consumer
// of this type asks about — the §7.6 reservation, and the one-owner rule — and
// it is required under every kind.
func (p PublishCredential) Configured() bool { return p.Env != "" }

// RunnerOptions is what the daemon supplies when constructing a runner
// (SPEC §7.1). Adapter-specific knobs live in the adapter's own options type;
// this is the core-owned subset a kind can carry (B11 owns kind selection).
type RunnerOptions struct {
	// Provider is the adapter-owned opaque block (SPEC §5.2.5), already
	// $VAR-resolved by the loader and already accepted by Structural.
	Provider map[string]any
	// Publish is the publish credential's binding (SPEC §5.2.8): the child
	// variable and the source that mints it. It binds here, with the provider
	// block, so Ready and Start cannot disagree about which credential a run
	// publishes with (SPEC §7.1); the *value* behind it is minted per attempt.
	//
	// No credential scope reaches a runner. The source instance owns its
	// complete exchange binding, and assembly passes nothing repository-derived
	// (SPEC §11).
	Publish PublishBinding
	// AttemptTimeout is the configured per-attempt bound, and the other operand
	// of the TTL gate the publisher applies at Ready and at Start (SPEC §7.7).
	AttemptTimeout time.Duration
	// TranscriptDir is where the adapter retains raw per-run harness streams
	// (SPEC §10.3). Empty disables retention.
	TranscriptDir string
	// StopGrace overrides the adapter's default SIGTERM→SIGKILL grace.
	StopGrace time.Duration
	// OnRun receives the run's evidence once the run exists (SPEC §9.10).
	// Nil disables the upgrade, which is right for a caller with no workspace
	// to mark — a readiness probe, a test — and wrong for the daemon.
	OnRun RunEvidenceSink
}

// RunEvidence identifies a launched run well enough for a *different process*
// to ask, later, whether that run is still going (SPEC §9.10's run marker).
//
// Deliberately not a pid. §9.10 says "evidence" throughout precisely so a remote
// substrate (#46) can answer the same question with a session id and no wording
// change, so the mechanism is named rather than assumed.
//
// Boot is what makes the answer trustworthy across a restart: a process id is
// unique only within one boot of one host, and a reboot reuses it freely. Asking
// "is pid 4242 alive" after a reboot is not a stale answer but a wrong one — it
// can name an unrelated process and report a dead run as live forever, which
// under §9.10 retains a claim nothing will ever release.
type RunEvidence struct {
	// Scheme names the mechanism, e.g. "pgid" for the local process substrate.
	Scheme string
	// ID is the identifier within that scheme.
	ID string
	// Boot identifies the host boot the ID belongs to. Empty when the scheme
	// does not need one — a remote session id is already globally unique.
	Boot string
}

// RunEvidenceSink durably records a run's evidence against the workspace it
// belongs to. It is called once, after the run exists and before its handle
// reaches the caller.
//
// The RunSpec is not context — it is the address. One sink is installed on a
// runner that serves every issue, and §9.10's marker is *per workspace*, so a
// sink handed only the evidence could not tell which of several concurrent
// launches to upgrade. It would have to guess, and a marker upgraded against the
// wrong workspace is worse than one never upgraded: it reports a live run's
// workspace as free while parking an idle one.
//
// An error means the run is real and its evidence could not be recorded — the
// worst of the three states §9.10 reads, because the marker is then present
// without evidence and recovery must park for a human. The runner therefore
// fails the attempt through its ordinary ladder rather than returning an error:
// once a process exists, "error returned" must never imply "nothing is running"
// (SPEC §7.4).
type RunEvidenceSink func(RunSpec, RunEvidence) error

// RunEvidenceLocal is the scheme for the local process substrate: ID is the
// process group id, Boot the host boot it belongs to.
const RunEvidenceLocal = "pgid"

// WorkspaceRef names one workspace on disk and the issue it belongs to
// (SPEC §9.10 step 5, §6.4).
//
// Identifier is empty when nothing records whose the directory is — a workspace
// from an older BEN, or a partial disposal. That is a fact rather than a gap:
// §6.4 keeps a failure's workspace, so "unowned" cannot be read as "disposable",
// and a caller that cannot name the issue cannot ask whether it is terminal.
type WorkspaceRef struct {
	// Key is the provider's own name for the workspace (SPEC §6.3).
	Key string
	// Path is where it is on disk. Carried because a caller that has to dispose one
	// cannot always resolve it the ordinary way: ResolveWorkspace answers "is there
	// a claim-time base pin", which is a different question from "is there a
	// directory", and a workspace whose pin is missing is exactly the residue a
	// sweep exists to remove.
	Path string
	// Identifier is the issue it was prepared for, or empty.
	Identifier string
}

// RunMarkerState is what a workspace's run marker says about the tenure before
// this one (SPEC §9.10's workspace precondition).
//
// Here rather than in either package that uses it, because it is the one shape
// both sides of the precondition speak: the provider owns the on-disk form, the
// orchestrator owns what to do about it, and a type per package would put a
// translation in the assembly — which is where a mistranslation would be
// invisible. Closed, and ordered so that the zero value authorizes nothing.
type RunMarkerState uint8

const (
	// RunMarkerUnreadable is the zero value and never an answer: a provider that
	// returned a marker without setting a state has not said a workspace is free.
	RunMarkerUnreadable RunMarkerState = iota
	// RunMarkerAbsent — no marker. The workspace is free.
	RunMarkerAbsent
	// RunMarkerIdentified — present, carrying evidence. The run can be probed,
	// and only proof of its absence frees the workspace.
	RunMarkerIdentified
	// RunMarkerUnknownLaunch — present without usable evidence, so the launch
	// outcome is unknown. It covers a crash before the launch, a crash after it
	// and before the evidence upgrade, and an interrupted cleanup of a launch that
	// failed. The three are indistinguishable and one of them has a live run in
	// it, so §9.10 parks for a human rather than guessing in either direction.
	RunMarkerUnknownLaunch
)

// RunMarker is one workspace's run marker as a later daemon reads it.
type RunMarker struct {
	State RunMarkerState
	// Evidence is meaningful only when State is RunMarkerIdentified.
	Evidence RunEvidence
}

// RunnerKind is one agent.kind, registered at package level: the two entry
// points that exist before any instance does (SPEC §5.7, §7.1). It mirrors
// TrackerKind — validation cannot be a method on the constructed runner,
// because a malformed provider block fails during construction and leaves no
// instance to ask.
type RunnerKind interface {
	// Structural reports whether the agent configuration is well-formed — the
	// provider block, and the core-owned fields beside it whose rules span both
	// (AgentConfig). PURE: no filesystem, no subprocess, no network, and
	// answerable with no harness installed, since `ben config effective` must
	// report a bad block for an adapter it could never have built (SPEC §5.8).
	Structural(cfg AgentConfig) error
	// New constructs the runner, binding the configuration to the instance.
	// Structural failures surface here too; nothing else does — which requires
	// New to parse through the same path Structural does, or it could build a
	// runner whose configuration Structural would have refused.
	New(opts RunnerOptions) (AgentRunner, error)
	// ForwardedEnvVars names the daemon-environment variables this kind copies
	// into an agent process **by name** — its `env_passthrough` surface. PURE:
	// names only, no filesystem, no network.
	//
	// Deliberately narrow. Everything a block's *values* carry into a child is
	// the loader's to see, because it resolved those values and recorded which
	// variable each came from; asking a kind to enumerate its "credential keys"
	// instead would miss every other key that reaches the child, and `model:
	// $TRACKER_PAT` reaches it as `--model <secret>` just as surely as an
	// environment variable does. What the loader genuinely cannot see is a value
	// that is a variable *name* rather than a secret, and that is this method.
	ForwardedEnvVars(provider map[string]any) []string
	// SensitiveFields names the provider paths whose value is a secret, for
	// display redaction (SPEC §5.8). See the shared documentation above.
	SensitiveFields(provider map[string]any) [][]string
	// Model reports the model this block selects, and the path within the block
	// it was read from. PURE: names and values already in hand, no filesystem,
	// no network.
	//
	// It is here rather than on the constructed runner because the answer is a
	// function of the block alone, and because the caller that wants it —
	// AgentDescriptor, resolved beside the loader — has a block and no instance.
	//
	// **The path is returned with the value, and both are needed.** Where the
	// value came from is what decides whether it may be printed (SPEC §5.8's
	// redaction is over provenance, never over the bytes), and only the adapter
	// knows which key in its own block the model lives under. A value alone would
	// force its consumer either to publish an env-resolved secret or to guess a
	// key name — and guessing an adapter's spelling is how `config effective` and
	// the loader drifted apart before the kind registry (#55). The path is
	// returned even when the value is empty, since "the block names none" is a
	// statement about that path.
	//
	// Tolerant of a malformed block, for ForwardedEnvVars' reason: it is asked of
	// blocks nothing has validated yet, and a parse that gave up on an unrelated
	// type error would report no model for a block that plainly names one.
	Model(provider map[string]any) (model string, path []string)
}
