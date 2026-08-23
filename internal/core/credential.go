package core

import (
	"context"
	"errors"
	"strconv"
	"time"
)

// Credential sources (SPEC §5.2, §5.7, §10.2): the shapes behind "every
// tracker, base-fetch and publish credential is obtained from a credential
// source at the moment it is needed".
//
// Scoped to those three. The harness's own API keys stay in `agent.provider`
// (§5.2.5, §7.6): they are the adapter's parameters, not BEN's credentials.

// CredentialErrorClass says what kind of failure an exchange met, and therefore
// whether waiting could help (SPEC §9.7, §9.8).
type CredentialErrorClass int

const (
	// CredentialUnknown is the zero value and is treated as non-retryable
	// wherever the class is consulted (§9.8's attempt backoff, and §9.7's
	// verification routing). A class that defaulted to transient would make
	// every unclassified error retryable by omission — and an omission is
	// exactly what a new kind or a new error path produces.
	CredentialUnknown CredentialErrorClass = iota
	// CredentialTransient is weather: network, timeout, 5xx, a context
	// deadline. The same request may succeed a moment later.
	CredentialTransient
	// CredentialPermanent is configuration: 401/403 (trust policy), 404
	// (identity unknown), an unreadable OIDC path, an empty token value, a
	// deadline shorter than the attempt it must cover. Retrying spends the
	// budget to fail identically.
	CredentialPermanent
)

func (c CredentialErrorClass) String() string {
	switch c {
	case CredentialUnknown:
		return "unknown"
	case CredentialTransient:
		return "transient"
	case CredentialPermanent:
		return "permanent"
	default:
		return "CredentialErrorClass(" + strconv.Itoa(int(c)) + ")"
	}
}

// CredentialError is a credential-source failure carrying the one thing routing
// needs (the class) and the one thing an operator needs (the authority).
//
// Authority names the *source*, never the token. It is the same non-secret
// identity SourceDescriptor.Authority carries, so an error read off a log names
// something an operator can find in the workflow file.
type CredentialError struct {
	Class     CredentialErrorClass
	Authority string
	Err       error
}

func (e *CredentialError) Error() string {
	if e.Err == nil {
		return "credential source " + e.Authority + ": " + e.Class.String() + " failure"
	}
	return "credential source " + e.Authority + " (" + e.Class.String() + "): " + e.Err.Error()
}

func (e *CredentialError) Unwrap() error { return e.Err }

// ErrCredentialEmpty is the boundary's refusal of a source that reported
// success with no credential (SPEC §10.2).
//
// A source that does this is defective, and no consumer may discover it by
// making an unauthenticated call: the refusal happens before any downstream
// GitHub request, git invocation or agent launch. Permanent, because a source
// that answers this way answers this way again.
var ErrCredentialEmpty = errors.New("credential source returned an empty token")

// ErrCredentialTTL is the boundary's refusal of a deadline too short to cover
// the attempt it would authenticate (SPEC §7.1, §7.7).
//
// Permanent: it is arithmetic, not weather.
var ErrCredentialTTL = errors.New("credential deadline is shorter than the configured attempt")

// CredentialTTLMargin is the fixed headroom every TTL gate adds to the attempt
// timeout — issuer clock skew plus the publish step's own duration.
//
// A constant and not an operator key. A workflow author is positioned to tune
// neither of the two things it absorbs, and a knob here would let one be tuned
// to zero.
const CredentialTTLMargin = 5 * time.Minute

// CredentialFailure reports the class of a credential-source failure, and
// whether err is one at all.
//
// The one reader of the class, so that "unclassified is not transient" is a
// property of this function rather than of each of the five call sites
// (SPEC §9.7, §9.8). An error that is not a credential failure reports
// CredentialUnknown and false — never a class a caller could route on by
// accident.
func CredentialFailure(err error) (CredentialErrorClass, bool) {
	var credErr *CredentialError
	if !errors.As(err, &credErr) {
		return CredentialUnknown, false
	}
	return credErr.Class, true
}

// CredentialAuthority names the source behind a credential failure, or "" when
// err is not one. For log severity and operator messages only: nothing routes on
// it.
func CredentialAuthority(err error) string {
	var credErr *CredentialError
	if !errors.As(err, &credErr) {
		return ""
	}
	return credErr.Authority
}

// SourceDescriptor is a credential source reduced to pure data: what a kind's
// Describe produces, and the whole of what load-time validation, the §10.2 split
// refusal and §5.4's reload comparison read.
//
// No kind knowledge and no name. The name is how a workflow file refers to an
// entry; it is not part of what the entry *is*, and a descriptor carrying one
// would make a rename a rebuild.
type SourceDescriptor struct {
	// Kind is the registered source kind, e.g. "octo_sts" or "static".
	Kind string
	// Authority is CREDENTIAL IDENTITY — deliberately narrow. Two sources with
	// equal Authority are one credential, so this is what the split refusal
	// compares. For octo_sts it includes canonical URL, configured scope and
	// identity: scope selects the policy namespace. A shared OIDC token path is
	// NOT part of it: one projected service-account token federating two
	// trust-policy identities is the intended deployment (SPEC §10.2).
	//
	// Namespaced, so a variable cannot collide with a config site: `env:FOO`,
	// `site:tracker.provider.token`, `octo:<url>#<scope>#<identity>`.
	Authority string
	// BindingKey is the COMPLETE canonical definition, name-free — every field
	// that changes behaviour. For octo_sts that includes URL, scope, identity
	// and oidc_token_path. This is what reload compares. Narrower than the
	// definition and a rebuild is missed; wider and a rename rebuilds.
	//
	// It carries no secret. Where the definition *is* a secret — a credential
	// the loader resolved to a literal — the key carries the full SHA-256 of
	// that value instead, which is comparable, non-secret, and sensitive to the
	// one field that matters (SPEC §5.4).
	BindingKey string
	// MinFreshTTL is the shortest remaining lifetime this kind promises a
	// freshly minted token. Zero means the kind is explicitly UNBOUNDED —
	// `static` and every legacy spelling — and every TTL gate is skipped.
	MinFreshTTL time.Duration
}

// SourceBinding is the half of a descriptor a reload comparison carries: the
// kind that selects the implementation and the key that names the definition
// (SPEC §5.4).
//
// Separate from SourceDescriptor because a binding must not carry Authority:
// authority is deliberately narrower than the definition, and comparing it
// instead would miss an `oidc_token_path` edit under an unchanged name.
type SourceBinding struct {
	Kind       string
	BindingKey string
}

// Binding projects a descriptor onto its reload identity.
func (d SourceDescriptor) Binding() SourceBinding {
	return SourceBinding{Kind: d.Kind, BindingKey: d.BindingKey}
}

// Bounded reports whether this source states a deadline. A bounded source's
// tokens are subject to every TTL gate; an unbounded one's are subject to none.
func (d SourceDescriptor) Bounded() bool { return d.MinFreshTTL > 0 }

// Purpose partitions a source's cache and labels its audit line (SPEC §10.2).
//
// It MUST NOT select an identity. An identity is a property of the source
// definition — for octo_sts, the configured `identity` — and a consumer that
// could choose one by asking for a different purpose would be choosing a trust
// policy from the call site, which is exactly what the two-instance deployment
// exists to prevent.
type Purpose string

const (
	PurposeTracker   Purpose = "tracker"
	PurposeWorkspace Purpose = "workspace"
	PurposePublish   Purpose = "publish"
)

// Token is one credential and the deadline its source stands behind.
type Token struct {
	Value string
	// UsableUntil is the source's conservative CONTRACT, not an observed
	// expiry: an STS exchange does not return one. Zero means the source is
	// explicitly unbounded (static, and every legacy spelling), never "expired"
	// and never "unknown".
	UsableUntil time.Time
}

// CredentialSource is the full surface a kind implements. The exchange scope is
// captured by the instance's validated source definition and is never supplied
// by a caller (SPEC §11).
type CredentialSource interface {
	// Fetch returns a usable credential, possibly from this source's own cache.
	Fetch(ctx context.Context, p Purpose) (Token, error)
	// FetchFresh performs the exchange every time. The publisher's view is
	// narrowed to this method alone: a token handed to an agent must cover the
	// whole attempt, and a cached one has already spent part of its life.
	FetchFresh(ctx context.Context, p Purpose) (Token, error)
	Descriptor() SourceDescriptor
}

// Source is the cached view a consumer holds when a shared credential is right:
// the tracker's polls and the daemon's own git fetches.
type Source interface {
	Fetch(ctx context.Context, p Purpose) (Token, error)
	Descriptor() SourceDescriptor
}

// FreshSource is the publisher's view. It deliberately has no cached path, so
// "serve the publisher from the shared cache" is not expressible at the call
// site — narrowing is assembly's decision (SPEC §11), and this type is what
// makes it stick.
type FreshSource interface {
	FetchFresh(ctx context.Context, p Purpose) (Token, error)
	Descriptor() SourceDescriptor
}

// RemoteAuthSource is the shape a git remote credential is obtained through,
// immediately before each invocation that needs one (SPEC §6.2, §6.4).
//
// The username half is the adapter's to choose — for GitHub PATs it is the
// documented `x-access-token` placeholder — which is why this is a seam the
// tracker satisfies rather than a projection the workspace performs.
type RemoteAuthSource interface {
	Auth(ctx context.Context) (RemoteAuth, error)
}

// SourceKind is one `credential_sources` kind, registered at package level —
// the two entry points that exist before any instance does (SPEC §5.7).
//
// It mirrors TrackerKind and RunnerKind, and for the same reason: a malformed
// block fails during construction and leaves no instance to ask.
type SourceKind interface {
	// Describe validates, canonicalizes and reduces a block to its descriptor.
	// PURE: no network, no instance, no filesystem — this is what load-time
	// validation and reload comparison call, so a workload-identity
	// configuration load-validates on a host holding no credential at all.
	Describe(block map[string]any) (SourceDescriptor, error)
	// New builds the runtime instance. It returns the FULL surface; consumers
	// receive narrowed views, so narrowing is assembly's decision and not a
	// kind's to get wrong.
	New(d SourceDescriptor, block map[string]any) (CredentialSource, error)
}

// PublishBinding is what a runner publishes with (SPEC §5.2.8, §6.7): the child
// variable it is injected as, and the source it is minted from.
//
// Source is never nil once publish is configured — every legacy spelling
// compiles into an implicit source, so there is exactly one runtime treatment
// and no nil-means-legacy branch anywhere.
type PublishBinding struct {
	// Env is the child environment variable, exclusively owned (SPEC §5.2.8).
	Env string
	// Source mints the credential per attempt. Fresh-only by type.
	Source FreshSource
}

// Configured reports whether a publish credential was configured at all.
func (b PublishBinding) Configured() bool { return b.Env != "" && b.Source != nil }
