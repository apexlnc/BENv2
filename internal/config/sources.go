package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// The `credential_sources` half of the schema (SPEC §5.2, §5.4, §5.5, §5.7,
// §10.2), and the compilation that makes every legacy spelling one of them.
//
// **One runtime treatment.** There is no nil-means-legacy branch anywhere below
// the loader: `tracker.provider.token`, an omitted token resolving from the
// documented fallback, and `publish.value` each compile into an implicit source,
// so an adapter is always handed a source and never a string. What survives of
// the legacy spellings is their *authority*, which is read from loader
// provenance rather than from the config site — because `$FOO` survives in
// provenance, and attributing a `$FOO`-referenced token to its site would
// regress the §10.2 split check that reads it from there today.

// TrackerCredentialSourceKey is the `tracker.provider` key naming a
// `credential_sources` entry (SPEC §5.2, amendment 1).
//
// Three keys of the tracker's otherwise-opaque provider block are core-known,
// and this is the sharpest of them: it names an entry in a core-owned section,
// so the loader has to read it to validate the reference at all. The other two
// are `token`, which the split check has always read through the kind's
// CredentialRefs, and `claim_assignee`, which §8.4 promotes to a field.
const TrackerCredentialSourceKey = "credential_source"

// TrackerTokenKey and TrackerClaimAssigneeKey are the other two.
const (
	TrackerTokenKey         = "token"
	TrackerClaimAssigneeKey = "claim_assignee"
)

// reducedTrackerKeys are the provider keys excluded from both boundaries — the
// reload binding and the adapter's construction options.
//
// Leaving `token` in either would defeat the whole compilation. In the binding
// it makes the digest redundant, because a DeepEqual over a map holding the
// resolved secret already catches a rotation — and worse, it carries the secret
// across a boundary whose purpose is to be non-secret. In the options it hands
// the adapter a second way to reach the credential beside its source, which is
// exactly the two-paths ambiguity the compilation exists to remove.
// `credential_source` and `claim_assignee` are excluded because they have been
// promoted to named fields, and a key that survives in the map is a key somebody
// will read from there.
var reducedTrackerKeys = []string{TrackerTokenKey, TrackerCredentialSourceKey, TrackerClaimAssigneeKey}

// PublishKindSource is the `publish.kind` that names a `credential_sources`
// entry instead of a variable (SPEC §5.2.8, amendment 1).
const PublishKindSource = "source"

// SourceConfig is one declared `credential_sources` entry: its kind and the
// verbatim block beneath it.
//
// The block is **not** `$VAR`-resolved, unlike a provider block. Every field a
// source kind defines is either a non-secret literal it prints in full
// (`octo_sts`, `projected_oidc`) or a variable reference resolved per fetch
// (`static`), so there is nothing here to resolve at load and resolving would
// print a secret (SPEC §5.5, §5.8, amendments 3 and 5).
type SourceConfig struct {
	Kind  string
	Block map[string]any
}

// Credential is one resolved credential: which entry it came from, and the pure
// descriptor that names it.
//
// Name is the entry a workflow file wrote, or "" for an implicit source compiled
// from a legacy spelling. It is carried for refusal messages and for
// `config effective`, and deliberately **not** part of any comparison: neither
// key carries a name, so a rename with an identical definition rebuilds nothing.
type Credential struct {
	Name string
	core.SourceDescriptor
	// literal is the value the loader already resolved, for an implicit source
	// over `tracker.provider.token`. Unexported: it is a secret, and the only
	// thing entitled to read it is the constructor below.
	literal string
	// variable is the daemon-environment variable an implicit or `static`
	// source reads per fetch. Empty for a literal or a workload identity.
	variable string
}

// Configured reports whether a credential was resolved at all. Only the publish
// credential can be absent: the tracker always has one, because an omitted token
// still resolves through its kind's documented fallback.
func (c Credential) Configured() bool { return c.Authority != "" }

// Credentials is the resolved credential per consumer (SPEC §10.2's three).
//
// The base-fetch credential is deliberately not a third entry: §10.2 gives the
// daemon's git reads to the *tracker* credential, and assembly hands the
// workspace the same source instance rather than a second one of its own.
type Credentials struct {
	Tracker Credential
	// Publish is the zero value when the workflow configures no publish
	// credential (SPEC §5.2.8), which is not an error.
	Publish Credential
	// Substrate authenticates BEN to a v2 execution backend (#194). The zero
	// value under the local substrate, which is every v1 workflow.
	Substrate Credential
	// Review authenticates the #204 review controller. The zero value when the
	// controller is not enabled, which is every workflow that predates it.
	//
	// A fifth identity rather than a reuse of the tracker's, and #11's whole
	// safety argument rests on it: the controller may unassign the claim
	// principal and revoke a human's required label, and a controller holding
	// the credential that *takes* claims could grant itself the work it just
	// stopped.
	Review Credential
}

// TrackerBinding is the name-free reload identity of the tracker adapter
// (SPEC §5.4, amendment 2).
//
// It is what `adapterChange` compares and what assembly builds the adapter
// from, which is the same rule AgentBinding follows and for the same reason: a
// field that can reach the adapter without also being a reason to rebuild it is
// the hot-reload bug this repo has now hit three times. `WorkflowKey` is
// excluded because it derives from the watched path and cannot move under a
// running daemon.
type TrackerBinding struct {
	// Provider is the reduced block: see reducedTrackerKeys.
	Provider       map[string]any
	RequiredLabels []string
	ActiveStates   []string
	TerminalStates []string
	ClaimAssignee  string
	// Credential is the source's kind and canonical binding key. Not its
	// authority: authority is narrower than the definition, so comparing it
	// would miss an `oidc_token_path` edit beneath an unchanged name.
	Credential core.SourceBinding
}

// TrackerBinding projects the config onto that identity.
func (c Config) TrackerBinding() TrackerBinding {
	return TrackerBinding{
		Provider:       ReducedTrackerProvider(c.Tracker.Provider),
		RequiredLabels: c.Tracker.RequiredLabels,
		ActiveStates:   c.Tracker.ActiveStates,
		TerminalStates: c.Tracker.TerminalStates,
		ClaimAssignee:  c.Tracker.ClaimAssignee,
		Credential:     c.Credentials.Tracker.Binding(),
	}
}

// ReducedTrackerProvider is the provider block with the three promoted keys
// removed — the one projection both boundaries use, so they cannot disagree
// about what a reduced block is.
//
// A copy, always: the caller may hold it for the life of an adapter generation,
// and deleting from the definition's own map would mutate what `config
// effective` renders and what the next comparison reads.
func ReducedTrackerProvider(provider map[string]any) map[string]any {
	out := make(map[string]any, len(provider))
	for k, v := range provider {
		if slices.Contains(reducedTrackerKeys, k) {
			continue
		}
		out[k] = v
	}
	return out
}

// resolveSources decodes the `credential_sources` section and records its
// provenance. Nothing here is `$VAR`-resolved; see SourceConfig.
func resolveSources(raw map[string]map[string]any, cfg *Config, prov Provenance) error {
	prov["credential_sources"] = originOr(raw != nil, SourceDefault)
	if len(raw) == 0 {
		return nil
	}
	cfg.CredentialSources = make(map[string]SourceConfig, len(raw))
	for _, name := range slices.Sorted(maps.Keys(raw)) {
		block := raw[name]
		if block == nil {
			return &ValidationError{Field: sourcePath(name), Msg: "must be a map naming a kind and that kind's fields"}
		}
		kind, _ := block["kind"].(string)
		kind = strings.TrimSpace(kind)
		if kind == "" {
			return &ValidationError{Field: sourcePath(name) + ".kind", Msg: fmt.Sprintf(
				"required (one of %s)", strings.Join(registry.SourceNames(), ", "))}
		}
		cfg.CredentialSources[name] = SourceConfig{Kind: kind, Block: block}
		for _, key := range slices.Sorted(maps.Keys(block)) {
			prov[appendProvenanceMapKey(sourcePath(name), key)] = FieldOrigin{Source: SourceFile}
		}
	}
	return nil
}

func sourcePath(name string) string { return appendProvenanceMapKey("credential_sources", name) }

// describeSources runs each declared entry's kind through its pure Describe
// (SPEC §5.7, amendment 4), which is where the per-kind schema is enforced.
func describeSources(cfg *Config) (map[string]core.SourceDescriptor, error) {
	if len(cfg.CredentialSources) == 0 {
		return nil, nil
	}
	out := make(map[string]core.SourceDescriptor, len(cfg.CredentialSources))
	for _, name := range slices.Sorted(maps.Keys(cfg.CredentialSources)) {
		src := cfg.CredentialSources[name]
		kind, ok := registry.Source(src.Kind)
		if !ok {
			return nil, &ValidationError{Field: sourcePath(name) + ".kind", Msg: fmt.Sprintf(
				"%q is not a supported credential source kind (supported: %s)",
				src.Kind, strings.Join(registry.SourceNames(), ", "))}
		}
		d, err := kind.Describe(src.Block)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", sourcePath(name), err)
		}
		out[name] = d
	}
	return out, nil
}

// resolveCredentials compiles the tracker and publish credentials — declared or
// legacy — into descriptors (SPEC §5.2, §8, amendment 9).
//
// Asked after validate, so both kinds are known to exist, and against the
// provenance resolve recorded, because a legacy credential's identity is a
// property of *where its value came from* and not of the site that spelled it.
func resolveCredentials(cfg *Config, prov Provenance) error {
	described, err := describeSources(cfg)
	if err != nil {
		return err
	}
	tracker, err := trackerCredential(cfg, prov, described)
	if err != nil {
		return err
	}
	publish, err := publishCredential(cfg, described)
	if err != nil {
		return err
	}
	substrate, err := substrateCredential(cfg, described)
	if err != nil {
		return err
	}
	reviewCred, err := reviewCredential(cfg, described)
	if err != nil {
		return err
	}
	cfg.Credentials = Credentials{
		Tracker: tracker, Publish: publish, Substrate: substrate, Review: reviewCred,
	}
	return nil
}

// reviewCredential resolves the #204 review controller's credential.
//
// One spelling only, for substrateCredential's reason: a consumer introduced
// after `credential_sources` inherits no legacy, and a literal or a variable
// read at this site would be another place credentials are spelled.
func reviewCredential(cfg *Config, described map[string]core.SourceDescriptor) (Credential, error) {
	name := cfg.Review.AuthSource
	if !cfg.Review.Enabled || name == "" {
		// validateReview refuses an enabled controller with no auth source.
		return Credential{}, nil
	}
	d, ok := described[name]
	if !ok {
		return Credential{}, unknownSourceRef("review.auth_source", name, cfg)
	}
	return namedCredential(name, d), nil
}

// substrateCredential resolves the v2 backend's credential (#194).
//
// One spelling only — a `credential_sources` entry — where the tracker and the
// publisher each have three. That is not an omission: the legacy spellings exist
// because §8.4 and §5.2.8 predate the section, and a new consumer inherits no
// legacy. A literal here would be a secret in a block `config effective` prints
// in full, and a variable read at this site would be a fourth place credentials
// are spelled.
//
// The reference is resolved rather than merely validated so the §10.2 authority
// comparison below sees it: `auth_source` naming the same entry as the tracker
// is the failure this whole chain exists to refuse.
func substrateCredential(cfg *Config, described map[string]core.SourceDescriptor) (Credential, error) {
	name := cfg.Substrate.Airlock.AuthSource
	if !cfg.Substrate.Remote() || name == "" {
		// validateSubstrate refuses a remote substrate with no auth source; a
		// local one has no backend to authenticate to.
		return Credential{}, nil
	}
	d, ok := described[name]
	if !ok {
		return Credential{}, unknownSourceRef("substrate.airlock.auth_source", name, cfg)
	}
	return namedCredential(name, d), nil
}

// trackerCredential resolves the tracker's credential, in the order §8.4 states
// its spellings: a named source, then a written token, then the kind's
// documented environment fallback.
//
// No combination degrades. Naming both a legacy token and a `credential_source`
// on one block is a refusal, because two config sites feeding one credential
// means one of them is silently doing nothing.
func trackerCredential(cfg *Config, prov Provenance, described map[string]core.SourceDescriptor) (Credential, error) {
	provider := cfg.Tracker.Provider
	name, _ := providerText(provider, TrackerCredentialSourceKey)
	token, _ := providerText(provider, TrackerTokenKey)

	if name != "" {
		if token != "" {
			return Credential{}, &ValidationError{
				Field: "tracker.provider." + TrackerCredentialSourceKey,
				Msg: fmt.Sprintf("names credential source %q while tracker.provider.%s is also set: "+
					"two config sites feeding one credential means one of them is silently doing nothing",
					name, TrackerTokenKey),
			}
		}
		d, ok := described[name]
		if !ok {
			return Credential{}, unknownSourceRef("tracker.provider."+TrackerCredentialSourceKey, name, cfg)
		}
		return namedCredential(name, d), nil
	}

	if token != "" {
		// The value is already resolved; its *identity* is not the value but
		// where it came from (SPEC §5.5, amendment 3).
		authority := credentialAuthorityFor(prov, "tracker.provider."+TrackerTokenKey)
		return Credential{
			SourceDescriptor: credential.LiteralDescriptor(authority, token),
			literal:          token,
		}, nil
	}

	// The kind's documented fallback, declared rather than assumed: an
	// undeclared fallback is what made #47's collision invisible.
	kind, ok := registry.Tracker(cfg.Tracker.Kind)
	if !ok {
		return Credential{}, nil // validate already refused this
	}
	for _, v := range kind.CredentialRefs(provider).Vars {
		if v = strings.TrimSpace(v); v != "" {
			return Credential{
				SourceDescriptor: credential.EnvDescriptor(v),
				variable:         v,
			}, nil
		}
	}
	return Credential{}, &ValidationError{
		Field: "tracker.provider." + TrackerTokenKey,
		Msg: fmt.Sprintf("required: tracker.kind %q declares no environment fallback, so the credential "+
			"must be written here or named through %s", cfg.Tracker.Kind, TrackerCredentialSourceKey),
	}
}

// publishCredential resolves the agent's publish credential (SPEC §5.2.8).
func publishCredential(cfg *Config, described map[string]core.SourceDescriptor) (Credential, error) {
	switch cfg.Publish.Kind {
	case "":
		return Credential{}, nil
	case PublishKindToken:
		return Credential{
			SourceDescriptor: credential.EnvDescriptor(cfg.Publish.ValueVar),
			variable:         cfg.Publish.ValueVar,
		}, nil
	case PublishKindSource:
		d, ok := described[cfg.Publish.Source]
		if !ok {
			return Credential{}, unknownSourceRef("publish.source", cfg.Publish.Source, cfg)
		}
		return namedCredential(cfg.Publish.Source, d), nil
	}
	// Unreachable: validatePublish refuses an unregistered kind first.
	return Credential{}, &ValidationError{Field: "publish.kind", Msg: fmt.Sprintf("%q is not a supported publish credential kind", cfg.Publish.Kind)}
}

func namedCredential(name string, d core.SourceDescriptor) Credential {
	c := Credential{Name: name, SourceDescriptor: d}
	// A declared `static` source reads a variable per fetch, and the split check
	// compares variables as well as authorities; recovering the name here keeps
	// that comparison anchored on one rule rather than on the kind's spelling.
	if v, ok := strings.CutPrefix(d.Authority, credential.EnvAuthorityPrefix); ok && d.Kind == credential.StaticKindName {
		c.variable = v
	}
	return c
}

func unknownSourceRef(field, name string, cfg *Config) error {
	known := slices.Sorted(maps.Keys(cfg.CredentialSources))
	msg := fmt.Sprintf("names credential source %q, which `credential_sources` does not declare", name)
	if len(known) > 0 {
		msg += " (declared: " + strings.Join(known, ", ") + ")"
	}
	return &ValidationError{Field: field, Msg: msg}
}

// credentialAuthorityFor reads a resolved value's credential identity off the
// loader's provenance (SPEC §10.2, amendment 15).
//
// A variable wherever one was referenced, and the config site only where none
// ever was. Attributing a `$FOO`-referenced token to its site would regress the
// existing split check, which resolves the tracker's credential to a variable
// name today and would stop seeing the collision.
//
// A value that interpolated *several* variables has no single variable
// identity — it is a composite, and none of its parts is the credential — so its
// identity is the site it was composed at. Nothing is lost: the variable-level
// split check below still sees every one of them.
func credentialAuthorityFor(prov Provenance, field string) string {
	if o := prov[field]; o.Source == SourceEnv && len(o.EnvVars) == 1 {
		return credential.EnvAuthority(o.EnvVars[0])
	}
	return credential.SiteAuthority(field)
}

// providerText reads a provider key as a trimmed string, tolerating a malformed
// block: Load does not run Structural (SPEC §5.7), so this is asked of blocks
// nobody has validated yet, and a type error on an unrelated key must not make
// the credential invisible.
func providerText(provider map[string]any, key string) (string, bool) {
	v, ok := provider[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok {
		return "", true
	}
	return strings.TrimSpace(s), true
}

// checkCredentialRules enforces the rules that span a credential and the rest of
// the configuration (SPEC §8.4 amendment 10, §7.7, §10.2).
func checkCredentialRules(cfg *Config) error {
	if err := checkAuthoritySplit(cfg); err != nil {
		return err
	}
	if err := checkClaimAssignee(cfg); err != nil {
		return err
	}
	return checkPublishTTL(cfg)
}

// ErrCredentialAuthorityShared is the §10.2 refusal for two credentials that are
// one credential however they are spelled.
var ErrCredentialAuthorityShared = errCredentialAuthorityShared

// checkAuthoritySplit is the split refusal, stated once (SPEC §10.2).
//
// > Two credentials with equal Authority are one credential, however they are
// > spelled. If the tracker's and the publisher's authorities are equal, the
// > configuration is a load refusal.
//
// That one sentence subsumes every combination: the same source named twice, two
// `octo_sts` sources sharing (url, scope, identity), two `static` sources naming
// one `$VAR`, and legacy↔named where a `static` source and the tracker's
// `$GITHUB_TOKEN` fallback both reduce to `env:GITHUB_TOKEN`. It is stated over
// authorities rather than over routes because enumerating routes is what let
// three bypasses through (#47).
//
// A shared `oidc_token_path` deliberately **loads**: it is not part of source
// identity, and one projected service-account token federating two trust-policy
// identities is the intended deployment.
// The substrate credential joins the same rule (#194). Two credentials with
// equal authority are one credential whatever they authenticate to, so a v2
// backend token that is also the tracker's or the publisher's is refused for the
// reason above applied to a wider blast radius: it can create and destroy
// execution environments. The sole exception is the explicit attended-canary
// tracker/controller concession: it already accepts one GitHub App actor for
// both roles, and therefore may use the same minting authority as well.
func checkAuthoritySplit(cfg *Config) error {
	tracker, publish := cfg.Credentials.Tracker, cfg.Credentials.Publish
	substrate := cfg.Credentials.Substrate
	if tracker.Configured() && publish.Configured() && tracker.Authority == publish.Authority {
		return &CredentialAuthorityError{
			Authority:   tracker.Authority,
			TrackerName: tracker.Name,
			PublishName: publish.Name,
		}
	}
	// Ordered, so a configuration with several collisions always refuses on the
	// same one: a refusal that varied would be one an operator cannot reproduce.
	//
	// The review controller joins the same rule (#204) and is the sharpest case
	// of it. #11's entire safety argument is that three identities are distinct;
	// a controller credential equal in authority to the tracker's could take the
	// claim it just handed back, and one equal to the substrate's could destroy
	// the sandbox holding the work it is reviewing.
	for _, authority := range []struct {
		cred Credential
		name string
	}{
		{substrate, "substrate"},
		{cfg.Credentials.Review, "review"},
	} {
		if !authority.cred.Configured() {
			continue
		}
		for _, other := range []struct {
			consumer string
			cred     Credential
		}{
			{"tracker", tracker},
			{"publish", publish},
			{"substrate", substrate},
		} {
			if other.consumer == authority.name || !other.cred.Configured() {
				continue
			}
			if other.cred.Authority == authority.cred.Authority {
				if authority.name == "review" && other.consumer == "tracker" &&
					cfg.Review.AllowSharedTrackerController && cfg.Deployment.Mode == DeploymentAttended {
					continue
				}
				return &SubstrateCredentialError{
					Authority:     authority.cred.Authority,
					Holder:        authority.name,
					Consumer:      other.consumer,
					SubstrateName: authority.cred.Name,
					ConsumerName:  other.cred.Name,
				}
			}
		}
	}
	return nil
}

// checkClaimAssignee refuses a bounded credential source with no configured
// claim assignee (SPEC §8.4, amendment 10).
//
// Stated over *boundedness* rather than over the kind name, so a future
// workload-identity kind inherits it: a credential with a deadline is one an
// issuer minted for a policy, and such a credential is statically known not to
// authenticate as a machine user whose login claims can name.
func checkClaimAssignee(cfg *Config) error {
	if !cfg.Credentials.Tracker.Bounded() || cfg.Tracker.ClaimAssignee != "" {
		return nil
	}
	return &ValidationError{
		Field: "tracker.provider." + TrackerClaimAssigneeKey,
		Msg: fmt.Sprintf("required with credential source kind %q: a credential a policy minted does not "+
			"authenticate as a machine user, so there is no login for claims to fall back to (SPEC §8.4)",
			cfg.Credentials.Tracker.Kind),
	}
}

// checkPublishTTL is the load half of the three TTL gates (SPEC §7.7).
//
//	load:    attempt_timeout_ms + margin <= descriptor(publish source).MinFreshTTL
//
// `<=` here matching `>=` at runtime, so a configuration that loads is one whose
// arithmetic Ready and Start can both satisfy. Skipped entirely when
// MinFreshTTL is zero, which is what keeps `static`, every legacy spelling and
// every configuration that loads today valid.
//
// DefaultAttemptTimeoutMS stays at one hour: a bounded source must set a valid
// timeout explicitly, and the refusal shows the arithmetic rather than quietly
// lowering it.
func checkPublishTTL(cfg *Config) error {
	d := cfg.Credentials.Publish
	if !d.Bounded() {
		return nil
	}
	maximum := d.MinFreshTTL - core.CredentialTTLMargin
	// Compare in the configuration's integer-millisecond domain. Converting a
	// near-MaxDuration timeout and then adding the margin would wrap negative
	// and turn the largest invalid timeout into one that passes the gate.
	if int64(cfg.Limits.AttemptTimeoutMS) <= int64(maximum/time.Millisecond) {
		return nil
	}
	return &ValidationError{
		Field: "limits.attempt_timeout_ms",
		Msg: fmt.Sprintf("%dms + the fixed %s margin exceeds the %s a %q publish credential is usable for: "+
			"an attempt could outlive the credential it publishes with. Lower it to at most %s",
			cfg.Limits.AttemptTimeoutMS, core.CredentialTTLMargin, d.MinFreshTTL, d.Kind, maximum),
	}
}

// Sources is the consumer selection from one constructed instance per named
// credential source (SPEC §11).
//
// Constructed here rather than in the assembly because the legacy compilation
// lives here: the assembly would otherwise have to re-derive which spelling
// produced which source, which is the second implementation of a rule that has
// exactly one correct answer. What assembly still owns is the **narrowing** —
// which consumer gets the cached surface and which the fresh-only one.
type Sources struct {
	// Tracker is the tracker credential's instance. The workspace's base fetch
	// is served from this same instance (SPEC §10.2), never a second one.
	Tracker core.CredentialSource
	// Publish is nil when the workflow configures no publish credential.
	Publish core.CredentialSource
	// Substrate is nil under the local substrate (#194). Narrowed by assembly
	// to the cached surface: the backend client makes several calls per tick per
	// claim, and an exchange per request would multiply the issuer's traffic by
	// the daemon's — the same call the tracker's narrowing makes.
	Substrate core.CredentialSource
	// Review is nil when the #204 controller is not enabled.
	Review core.CredentialSource
}

// NewSources constructs every declared named source exactly once, then selects
// those instances for the consumers. Kinds are resolved through the injected
// lookup for the reason cmd/ben injects its own: the registry's tables reach
// every adapter, and a test needs a kind of its own.
func (c Config) NewSources(kindFor func(string) (core.SourceKind, bool)) (Sources, error) {
	named := make(map[string]core.CredentialSource, len(c.CredentialSources))
	for _, name := range slices.Sorted(maps.Keys(c.CredentialSources)) {
		definition := c.CredentialSources[name]
		kind, ok := kindFor(definition.Kind)
		if !ok {
			return Sources{}, fmt.Errorf("%s: no credential source kind is registered for %q", sourcePath(name), definition.Kind)
		}
		descriptor, err := kind.Describe(definition.Block)
		if err != nil {
			return Sources{}, fmt.Errorf("%s: %w", sourcePath(name), err)
		}
		source, err := kind.New(descriptor, definition.Block)
		if err != nil {
			return Sources{}, fmt.Errorf("%s: %w", sourcePath(name), err)
		}
		named[name] = source
	}

	tracker, err := c.Credentials.Tracker.newSource(named)
	if err != nil {
		return Sources{}, fmt.Errorf("tracker credential: %w", err)
	}
	publish, err := c.Credentials.Publish.newSource(named)
	if err != nil {
		return Sources{}, fmt.Errorf("publish credential: %w", err)
	}
	substrate, err := c.Credentials.Substrate.newSource(named)
	if err != nil {
		return Sources{}, fmt.Errorf("substrate credential: %w", err)
	}
	reviewSource, err := c.Credentials.Review.newSource(named)
	if err != nil {
		return Sources{}, fmt.Errorf("review credential: %w", err)
	}
	return Sources{Tracker: tracker, Publish: publish, Substrate: substrate, Review: reviewSource}, nil
}

func (c Credential) newSource(named map[string]core.CredentialSource) (core.CredentialSource, error) {
	switch {
	case !c.Configured():
		return nil, nil
	case c.Name != "":
		source, ok := named[c.Name]
		if !ok {
			return nil, fmt.Errorf("credential source %q was not constructed", c.Name)
		}
		return source, nil
	case c.variable != "":
		return credential.NewEnv(c.variable), nil
	default:
		return credential.NewLiteral(c.Authority, c.literal), nil
	}
}
