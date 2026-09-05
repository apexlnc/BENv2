package credential

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// StaticKindName is the `kind` this file answers to.
const StaticKindName = "static"

// StaticKind is the explicitly-unbounded credential source: one daemon
// environment variable, read per fetch (SPEC §5.5, amendment 3).
//
// It ships for compatibility and development, and it is also what every legacy
// spelling compiles into — which is why the invariant distinguishes **bounded**
// from **explicitly unbounded** rather than asserting that every credential has
// a deadline. A `static` source states none, and every TTL gate is skipped for
// it, so every configuration that loads today still loads.
type StaticKind struct{}

var _ core.SourceKind = StaticKind{}

// Describe validates the block and reduces it to its descriptor.
//
// The schema is one required field, and the rule on it is exact: `value` must be
// **exactly one `$VAR` reference** — not a literal, not an interpolation. A
// literal would put a credential in a repo-owned file and in every
// `config effective` rendering of it; an interpolation would make one token the
// concatenation of several secrets, none of which has an identity the §10.2
// split check could compare.
func (StaticKind) Describe(block map[string]any) (core.SourceDescriptor, error) {
	if err := checkKeys(block, StaticKindName, "value"); err != nil {
		return core.SourceDescriptor{}, err
	}
	v, present := block["value"]
	if !present || v == nil {
		return core.SourceDescriptor{}, fmt.Errorf("%w: %s requires %q", ErrMissingSourceField, StaticKindName, "value")
	}
	raw, ok := v.(string)
	if !ok {
		return core.SourceDescriptor{}, fmt.Errorf("%w: %s.value must be a string, got %T", ErrSourceFieldType, StaticKindName, v)
	}
	// Not trimmed first. "Exactly one reference and nothing else" makes
	// surrounding whitespace something else, and a quoted `" $FOO "` is a
	// deliberate spelling: rewriting it here would make the loader lenient about
	// the one field the rule exists to keep exact (the same reasoning as
	// config.publishValueVar).
	m := oneReferenceRe.FindStringSubmatch(raw)
	if m == nil {
		return core.SourceDescriptor{}, fmt.Errorf("%w: %s.value must be exactly one $VAR reference "+
			"(e.g. $GH_TOKEN); the value is not shown", ErrSourceValueNotReference, StaticKindName)
	}
	return EnvDescriptor(m[1]), nil
}

// New builds the runtime instance.
func (k StaticKind) New(d core.SourceDescriptor, block map[string]any) (core.CredentialSource, error) {
	// Re-derived rather than parsed out of the descriptor: New must build only
	// what Describe would accept, which is the same rule TrackerKind.New and
	// RunnerKind.New follow (SPEC §5.7).
	got, err := k.Describe(block)
	if err != nil {
		return nil, err
	}
	name, ok := envVarOf(got.Authority)
	if !ok {
		return nil, fmt.Errorf("%w: %s built a descriptor naming no variable", ErrSourceValueNotReference, StaticKindName)
	}
	return &envSource{descriptor: got, variable: name}, nil
}

// EnvAuthorityPrefix namespaces a credential identity that is a daemon
// environment variable, so a variable can never collide with a config site
// (SPEC §10.2).
const EnvAuthorityPrefix = "env:"

// SiteAuthorityPrefix namespaces a credential identity that is a config site —
// used only where no variable was ever referenced, which is a `token:` written
// as a true file literal.
const SiteAuthorityPrefix = "site:"

// EnvAuthority is the credential identity of a daemon environment variable.
func EnvAuthority(name string) string { return EnvAuthorityPrefix + name }

// SiteAuthority is the credential identity of a config site.
func SiteAuthority(field string) string { return SiteAuthorityPrefix + field }

// EnvDescriptor is the descriptor of a source that reads one variable per fetch.
//
// Its BindingKey carries no digest, and that is not an omission: the value is
// read at every fetch, so a rotation is picked up without rebuilding anything
// and there is nothing about it for a reload to compare. PrincipalKey remains
// empty for a different reason: an opaque replacement token may name a
// different downstream principal, so a principal-scoped replay must bind the
// concrete token. The literal case below is the one where the value *is* part
// of the definition.
func EnvDescriptor(variable string) core.SourceDescriptor {
	return core.SourceDescriptor{
		Kind:      StaticKindName,
		Authority: EnvAuthority(variable),
		// Name-free by construction: a `credential_sources` entry called
		// anything at all over `$FOO` reduces to this.
		BindingKey: EnvAuthority(variable),
	}
}

// LiteralDescriptor is the descriptor of an implicit source over a value the
// loader already resolved (SPEC §5.4, amendment 2).
//
// The digest is the whole of why this is a separate constructor. `Config.Tracker`
// used to catch a rotated `tracker.provider.token` literal because the secret sat
// in the compared map; a name-free binding carrying only `site:…` would stop
// rebuilding on that rotation — a regression introduced by the fix. So the key
// ends in the **full** SHA-256 of the resolved value: non-secret, comparable, and
// sensitive to the one field that matters.
//
// The full digest, not a prefix. A truncated digest makes a collision suppress a
// required rebuild, and there is no length worth defending for a value that is
// compared and never displayed.
//
// PrincipalKey is still empty. Knowing the exact opaque token does not give the
// pure descriptor a contract that all future tokens from this source name one
// downstream principal; a replay consumer binds the concrete token instead.
//
// This is not a reintroduction of the token-keyed rate gate. That gate needed
// **stability across rotation**; this key needs **instability across change**.
// Opposite requirements, so the same primitive is correct in one place and wrong
// in the other.
func LiteralDescriptor(authority, value string) core.SourceDescriptor {
	sum := sha256.Sum256([]byte(value))
	return core.SourceDescriptor{
		Kind:       StaticKindName,
		Authority:  authority,
		BindingKey: authority + "#" + hex.EncodeToString(sum[:]),
	}
}

// NewEnv builds a source that reads one daemon environment variable per fetch —
// the runtime behind a declared `static` source, and behind `publish.value`'s
// `$VAR` reference, which §5.5 resolves per fetch for the same reason.
func NewEnv(variable string) core.CredentialSource {
	return &envSource{descriptor: EnvDescriptor(variable), variable: variable}
}

// NewLiteral builds a source over a value the loader already resolved: the
// implicit source behind `tracker.provider.token`, whether it was written as a
// literal or as a `$VAR` the loader expanded.
//
// The authority is supplied rather than derived, because it comes from
// **provenance** and not from this value: `$FOO` survives in the loader's
// provenance, and attributing a `$FOO`-referenced token to its config site would
// regress the §10.2 split check that reads it from there today.
func NewLiteral(authority, value string) core.CredentialSource {
	return &literalSource{descriptor: LiteralDescriptor(authority, value), value: value}
}

// envVarOf recovers the variable name from an `env:` authority.
func envVarOf(authority string) (string, bool) {
	if len(authority) <= len(EnvAuthorityPrefix) || authority[:len(EnvAuthorityPrefix)] != EnvAuthorityPrefix {
		return "", false
	}
	return authority[len(EnvAuthorityPrefix):], true
}

// envSource reads its variable on every fetch (SPEC §5.5, amendment 3).
//
// No cache, deliberately, and Fetch is FetchFresh. The value is a daemon
// environment variable an operator can change under a running process, and this
// kind states no deadline — so a cache would have no expiry to invalidate it and
// would pin the first value read for the life of the daemon.
type envSource struct {
	descriptor core.SourceDescriptor
	variable   string
}

func (s *envSource) Descriptor() core.SourceDescriptor { return s.descriptor }

func (s *envSource) Fetch(ctx context.Context, p core.Purpose) (core.Token, error) {
	return s.FetchFresh(ctx, p)
}

func (s *envSource) FetchFresh(context.Context, core.Purpose) (core.Token, error) {
	v := os.Getenv(s.variable)
	if v == "" {
		// Permanent: an unset variable stays unset until somebody acts, and a
		// retry budget spent waiting for that is a budget spent on nothing. The
		// message names the variable, which §5.5 makes the thing the file says
		// — never the value, which there is none of.
		return core.Token{}, permanent(s.descriptor.Authority, fmt.Errorf(
			"$%s is unset or empty in the daemon's environment", s.variable))
	}
	// UsableUntil deliberately zero: explicitly unbounded (SPEC §10.2).
	return core.Token{Value: v}, nil
}

// literalSource answers with the value the loader resolved. Unbounded, like
// every other legacy spelling.
type literalSource struct {
	descriptor core.SourceDescriptor
	value      string
}

func (s *literalSource) Descriptor() core.SourceDescriptor { return s.descriptor }

func (s *literalSource) Fetch(ctx context.Context, p core.Purpose) (core.Token, error) {
	return s.FetchFresh(ctx, p)
}

func (s *literalSource) FetchFresh(context.Context, core.Purpose) (core.Token, error) {
	return core.Token{Value: s.value}, nil
}
