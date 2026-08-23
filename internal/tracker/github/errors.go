package github

import (
	"errors"
	"fmt"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Named refusals (SPEC §5.7 applied to the adapter-owned provider block).
// Configuration errors surface at load, where the daemon can refuse to start;
// call errors surface per request.
var (
	ErrMissingRepo = errors.New("tracker.provider.repo is required (owner/name)")
	ErrInvalidRepo = errors.New("tracker.provider.repo must be owner/name")
	// ErrMissingToken is the credential boundary's refusal of a source that
	// reported success with no credential (SPEC §10.2). It is raised **before
	// any GitHub request is issued**, because a source that hands back nothing
	// is a source defect and no consumer may discover that by making an
	// unauthenticated call.
	//
	// A *readiness and per-request* refusal, not a structural one: an omitted
	// token is a valid config whose credential compiles into an implicit source
	// over the documented $GITHUB_TOKEN fallback (SPEC §5.8, §8.4).
	ErrMissingToken = errors.New("the tracker credential source returned an empty token: set tracker.provider.token to a $VAR, name a credential_source, or export GITHUB_TOKEN")
	// ErrNoCredentialSource refuses construction with no credential source at
	// all. Every legacy spelling compiles into an implicit one (SPEC §8,
	// amendment 9), so a nil here is an assembly defect rather than a
	// configuration, and defaulting to the environment would be the second
	// credential path the compilation exists to remove.
	ErrNoCredentialSource = errors.New("tracker options carry no credential source")
	// ErrEmptyClaimAssignee refuses a key an operator wrote but left blank.
	// Treating it as absent would silently select the credential identity instead
	// of the account the file appears to name (SPEC §8.4).
	ErrEmptyClaimAssignee = errors.New("tracker.provider.claim_assignee must not be blank when set")
	// ErrEmptyCredentialSource refuses a written-but-blank credential_source,
	// for the reason above it: a blank reference names no entry, and falling
	// through to the token or the environment fallback is the silent downgrade
	// SPEC §5.7 refuses (amendment 4).
	ErrEmptyCredentialSource = errors.New("tracker.provider.credential_source must not be blank when set")
	// ErrCredentialSourceAndToken refuses a block that spells the credential
	// twice. Two config sites feeding one credential means one of them is
	// silently doing nothing, and which one loses would be decided by
	// composition order rather than by anything an operator wrote.
	ErrCredentialSourceAndToken = errors.New("tracker.provider names both credential_source and token; exactly one supplies the credential")
	// ErrNotReady refuses a question only readiness can answer — currently the
	// claim principal and base clone's repository and credential (SPEC §6.2,
	// §8.4, §10.2). Readiness owns
	// credential resolution and every probe of the world (SPEC §5.7, §5.8), so
	// asking before it has succeeded gets a refusal rather than a second
	// resolution path with its own failure modes.
	ErrNotReady           = errors.New("tracker is not ready: Ready must succeed before the repository is known")
	ErrUnknownProviderKey = errors.New("unknown key in tracker.provider")
	ErrProviderKeyType    = errors.New("tracker.provider value has the wrong type")
	ErrInvalidAPIURL      = errors.New("tracker.provider.api_url is not a valid URL")
	// ErrAPIURLNotAnEndpoint refuses an api_url carrying anything past the
	// endpoint it names. Scheme, host and path decide where a request goes;
	// userinfo, a query and a fragment are all dropped when go-github resolves a
	// request against this base, so each is a spelling that changes the tracker's
	// *identity* without changing the server it reaches — and that identity is
	// what request control is retained under (Adapter.RequestControlKey).
	//
	// Userinfo is refused twice over: it is a credential in a field
	// `config effective` deliberately prints in full (Kind.SensitiveFields), and
	// it would be held in a process-lifetime key. Redacting api_url instead would
	// cost operators the one address they most need from that output, to protect
	// a credential that has no business there.
	ErrAPIURLNotAnEndpoint = errors.New("tracker.provider.api_url must name only an endpoint: no userinfo, query, or fragment")
	ErrEmptyRequiredLabels = errors.New("tracker.required_labels must list at least one label: an empty list would make every issue in the repository dispatchable")
	ErrUnknownStateLabel   = errors.New("not a SPEC §9.3 state label")
	ErrUnknownMilestone    = errors.New("not a SPEC §8.4 milestone")
	// ErrNoPrincipal means the token's identity could not be resolved when
	// claim_assignee is omitted, so there is nobody to assign (SPEC §8.4).
	ErrNoPrincipal = errors.New("cannot resolve the claim principal for this token")
	// ErrClaimPrincipalNotAssignable is the configured principal's definitive
	// readiness refusal: GitHub reports that the account cannot be assigned in
	// this repository (SPEC §8.4).
	ErrClaimPrincipalNotAssignable = errors.New("configured claim principal is not assignable to the repository")
	// ErrClaimPrincipalProbe is distinct from a negative answer: GitHub did not
	// establish whether the configured principal is assignable, so BEN must not
	// report the account itself as invalid.
	ErrClaimPrincipalProbe = errors.New("cannot determine whether the configured claim principal is assignable to the repository")
	// ErrRequestBudget means the current request window or its rolling allowance
	// cannot admit an ordinary call (SPEC §8.5, budget.go). A refusal, not a
	// failure: the read is deferred to a later tick, and every caller in the
	// orchestrator already retries a tracker read it did not get (§9.8). It is
	// deliberately *not* a RateLimitError — nothing about the server's budget is
	// known here, so a server wait to honor would be invented.
	ErrRequestBudget = errors.New("per-tick GitHub request budget spent")
	// ErrNoMilestoneOccurrence means the label transition that defines this
	// milestone's occurrence is not on the issue's change log (SPEC §8.4).
	// Every milestone is posted after the label write that anchors it
	// (SPEC §9.2), so this is a caller-order error, not a state to paper
	// over: a milestone that cannot name its occurrence cannot be idempotent.
	ErrNoMilestoneOccurrence = errors.New("no label transition anchoring this milestone")
	// ErrGraphQL is the §9.5 content read failing to produce an answer — a
	// transport failure, a non-200, or an `errors` array in an otherwise-200
	// response. Named because it must not be mistaken for "never edited": the
	// orchestrator retries a failed read with the claim retained rather than
	// dispatching content it could not check (SPEC §9.5, §9.10).
	ErrGraphQL = errors.New("the GitHub GraphQL API did not answer")
)

// notAttempted marks a Claim refusal this adapter reached before an assignment
// could have left the process, so the orchestrator can leave the issue as it
// found it instead of owing it an unwinding release
// (core.ErrClaimNotAttempted, SPEC §9.2).
//
// The wrapped refusal keeps its own identity: a budget refusal is still
// ErrRequestBudget, a server refusal still a *RateLimitError. This says only
// what did *not* happen.
func notAttempted(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", core.ErrClaimNotAttempted, err)
}

// RateLimitError reports a primary or secondary rate limit with the wait the
// server asked for (SPEC §8.5: honor both Retry-After and X-RateLimit-Reset).
// The adapter refuses further requests until RetryAfter elapses rather than
// spending the budget discovering the limit again.
type RateLimitError struct {
	// Secondary distinguishes GitHub's abuse/secondary limit from the
	// primary hourly budget.
	Secondary  bool
	RetryAfter time.Duration
	Err        error
}

func (e *RateLimitError) Error() string {
	kind := "primary"
	if e.Secondary {
		kind = "secondary"
	}
	return fmt.Sprintf("github %s rate limit: retry after %s: %v", kind, e.RetryAfter.Round(time.Second), e.Err)
}

func (e *RateLimitError) Unwrap() error { return e.Err }
