package github

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// KindName is the tracker.kind this package answers to.
const KindName = "github"

// Kind is the package-level tracker.kind registration (SPEC §5.7, §8.2): the
// two entry points that exist before any instance does. Validation cannot be
// a method on the constructed adapter — a malformed config fails in New,
// leaving no instance to ask — so the structural check is a property of the
// kind. internal/registry maps KindName onto this value.
type Kind struct{}

var _ core.TrackerKind = Kind{}

// Structural reports whether the tracker config — the opaque provider block
// and the core-owned fields together — is well-formed. Pure: no network,
// filesystem, subprocess, or environment, so `ben config effective` and
// credential-free CI can ask it with nothing installed (SPEC §5.7, §5.8).
func (Kind) Structural(cfg core.TrackerConfig) error {
	_, err := parse(cfg)
	return err
}

// New constructs the adapter, binding opts to the instance (SPEC §5.7, §8
// amendment 9).
func (Kind) New(opts core.TrackerOptions) (core.TrackerAdapter, error) {
	a, err := New(opts)
	if err != nil {
		return nil, err
	}
	return a, nil
}

// providerKeys is the closed key set of the GitHub provider block. The block
// is opaque to the loader but not to us: strict-at-load applies here too, so a
// typo is a refusal rather than a silently ignored setting.
var providerKeys = []string{"repo", "token", "api_url", "claim_assignee", "credential_source"}

// FallbackTokenEnv is the variable an omitted `token` resolves from at Ready
// (SPEC §8.4). Named here because two places must agree about it and neither
// can see the other's copy: Ready reads it, and CredentialRefs declares it so
// the loader can tell whether the agent is being handed the same secret.
//
// An undeclared fallback is what made #47's collision invisible. The tracker
// block named no variable at all, the agent forwarded GITHUB_TOKEN by name, and
// nothing in the file said they were one secret.
const FallbackTokenEnv = "GITHUB_TOKEN"

// CredentialRefs names where the tracker credential comes from (SPEC §10.2).
//
// The fallback is declared only when the block does not supply a token, which
// mirrors Ready exactly: it reads the environment only when the bound config
// carried nothing. Declaring it unconditionally would refuse a workflow whose
// agent legitimately forwards GITHUB_TOKEN for publishing while the tracker
// authenticates as somebody else entirely — a split that is the point of §10.2,
// not a violation of it.
// A named `credential_source` suppresses the fallback as surely as a written
// token does, and for the same reason: the fallback is what Ready reads *when
// nothing else supplies the credential*, and a block naming a source has
// supplied it. Declaring it anyway would attribute $GITHUB_TOKEN to a tracker
// that never reads it — a refusal over a secret this daemon does not hold.
func (k Kind) CredentialRefs(provider map[string]any) core.CredentialRefs {
	// The same list SensitiveFields reports, not a second copy of it: the two
	// answer different questions about the same key, and a key added to one and
	// forgotten in the other is either an unenforced split (§10.2) or a printed
	// secret (§5.8).
	refs := core.CredentialRefs{Fields: k.SensitiveFields(provider)}
	tok, _ := providerString(provider, "token")
	src, _ := providerString(provider, "credential_source")
	if tok == "" && src == "" {
		refs.Vars = []string{FallbackTokenEnv}
	}
	return refs
}

// SensitiveFields names the provider values that are secrets, so every
// `config effective` rendering hides them whatever their provenance
// (SPEC §5.8). `repo` and `api_url` are addresses, not credentials, and stay
// readable — redacting them would cost an operator the two facts they most need
// from that output while protecting nothing.
func (Kind) SensitiveFields(map[string]any) [][]string {
	return [][]string{{"token"}}
}

// settings is the validated result of a core.TrackerConfig.
type settings struct {
	owner, repo string
	// token is the block-supplied credential, empty when omitted — the
	// documented $GITHUB_TOKEN fallback resolves at Ready, not here
	// (SPEC §5.8, §8.4).
	token  string
	apiURL string // empty = github.com
	// claimAssignee is the machine-user account claims name. Empty preserves
	// the credential-authenticated login fallback. Deployments running several
	// daemons must configure a distinct account for each: the assignee is also
	// the ownership partition §9.8 and §9.10 read (SPEC §8.4).
	claimAssignee  string
	requiredLabels []string
	activeStates   []string
	workflowKey    string
}

// DefaultActiveStates is the GitHub default when the workflow leaves
// `tracker.active_states` unset (SPEC §5.2.2).
var DefaultActiveStates = []string{"open"}

// parse is the structural check (SPEC §5.7): everything answerable from the
// config alone, with no credentials present and no network at the other end.
func parse(cfg core.TrackerConfig) (settings, error) {
	var s settings

	for k := range cfg.Provider {
		if !slices.Contains(providerKeys, k) {
			return s, fmt.Errorf("%w: %q (known keys: %s)", ErrUnknownProviderKey, k, strings.Join(providerKeys, ", "))
		}
	}
	repoSpec, err := providerString(cfg.Provider, "repo")
	if err != nil {
		return s, err
	}
	if repoSpec == "" {
		return s, ErrMissingRepo
	}
	owner, name, ok := strings.Cut(repoSpec, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		// The offending value rides as data, never in the text: it may be an
		// env-resolved secret, and only the provenance-holding renderer can
		// tell (SPEC §5.8).
		return s, &core.ConfigValueError{Field: "tracker.provider.repo", Value: repoSpec, Err: ErrInvalidRepo}
	}
	s.owner, s.repo = owner, name

	// Token is optional here: an omitted token is structurally valid,
	// unresolved until Ready (SPEC §5.8). An explicit `token: $VAR` that
	// resolved empty never reaches us — the loader refused it first
	// (SPEC §5.5).
	if s.token, err = providerString(cfg.Provider, "token"); err != nil {
		return s, err
	}

	if s.apiURL, err = providerString(cfg.Provider, "api_url"); err != nil {
		return s, err
	}
	if s.apiURL != "" {
		// Every refusal below carries Sensitive, and none of them says which value
		// it read. A URL's authority is `[userinfo@]host[:port]`, so this field can
		// hold a credential at a path that is otherwise an address worth printing
		// (Kind.SensitiveFields) — and the value that reaches an *invalid* URL
		// refusal is exactly the one whose authority could not be located, so it
		// cannot be shown to be credential-free either. What each refusal says
		// instead is what was wrong, which needs no part of the value.
		u, err := url.Parse(s.apiURL)
		switch {
		case err != nil:
			// url.Parse's own error text embeds the URL, so it is never wrapped.
			return s, apiURLRefusal(s.apiURL, ErrInvalidAPIURL, "it could not be parsed")
		case u.Scheme == "":
			return s, apiURLRefusal(s.apiURL, ErrInvalidAPIURL, "it names no scheme")
		case u.Host == "":
			return s, apiURLRefusal(s.apiURL, ErrInvalidAPIURL, "it names no host")
		}
		// Measured against go-github v90 and this package's fake: all of these
		// reach the identical path, while the client's BaseURL keeps whatever was
		// written — so each is a second identity for one endpoint
		// (Adapter.RequestControlKey). Refused rather than normalized away, so an
		// operator who wrote one learns it meant nothing.
		if carried := nonEndpointComponents(u); len(carried) > 0 {
			return s, apiURLRefusal(s.apiURL, ErrAPIURLNotAnEndpoint, "it carries "+strings.Join(carried, " and "))
		}
	}

	// Presence is significant. A written-but-blank key is an operator mid-edit,
	// not a request to fall back to the credential's login (SPEC §8.4).
	if _, present := cfg.Provider["claim_assignee"]; present {
		if s.claimAssignee, err = providerString(cfg.Provider, "claim_assignee"); err != nil {
			return s, err
		}
		if s.claimAssignee == "" {
			return s, ErrEmptyClaimAssignee
		}
	}

	// Same rule, one key over: a written-but-blank `credential_source` names no
	// entry, and silently falling through to the token or the environment
	// fallback is exactly the degradation §5.7 refuses (amendment 4). The
	// *reference* is resolved by the loader, which owns the section it points
	// into; what this can say is that the reference is well-formed and that the
	// block does not also spell the credential out.
	if _, present := cfg.Provider["credential_source"]; present {
		src, err := providerString(cfg.Provider, "credential_source")
		if err != nil {
			return s, err
		}
		if src == "" {
			return s, ErrEmptyCredentialSource
		}
		if s.token != "" {
			return s, ErrCredentialSourceAndToken
		}
	}

	// An empty required-labels set would make opt-in vacuous: every issue in
	// the repository would be BEN's (BUILD.md decision 9).
	for _, l := range cfg.RequiredLabels {
		if l = strings.TrimSpace(l); l != "" {
			s.requiredLabels = append(s.requiredLabels, l)
		}
	}
	if len(s.requiredLabels) == 0 {
		return s, ErrEmptyRequiredLabels
	}

	s.activeStates = cfg.ActiveStates
	if len(s.activeStates) == 0 {
		s.activeStates = DefaultActiveStates
	}
	s.workflowKey = cfg.WorkflowKey
	return s, nil
}

// reducedProviderKeys are the keys a *construction* block may not carry: each
// has been promoted to a field of core.TrackerOptions, or to the credential
// source itself.
//
// Refused rather than ignored, and this is the independent boundary the
// reduction is asserted at. A key that survived the projection would be a second
// path to the credential or to the claim identity, and "ignored" is how a second
// path stays invisible: the adapter would keep working, from the field, while
// the map quietly disagreed with it.
var reducedProviderKeys = []string{"token", "credential_source", "claim_assignee"}

// parseOptions is the structural check over what assembly *constructs* from,
// which is a strictly narrower block than what Structural validates.
//
// Two entry points rather than one, because the two are asked different
// questions. Structural is asked about the file as written, including every
// legacy credential spelling; this is asked about the compiled result, in which
// the credential is a source and the claim identity is a field.
func parseOptions(opts core.TrackerOptions) (settings, error) {
	var s settings
	if opts.Credential == nil {
		return s, ErrNoCredentialSource
	}
	for _, k := range reducedProviderKeys {
		if _, present := opts.Provider[k]; present {
			return s, fmt.Errorf("%w: %q is promoted out of the construction block (SPEC §8.4, §11)",
				ErrUnknownProviderKey, k)
		}
	}
	set, err := parse(core.TrackerConfig{
		Provider:       opts.Provider,
		RequiredLabels: opts.RequiredLabels,
		ActiveStates:   opts.ActiveStates,
		TerminalStates: opts.TerminalStates,
		WorkflowKey:    opts.WorkflowKey,
	})
	if err != nil {
		return s, err
	}
	set.claimAssignee = strings.TrimSpace(opts.ClaimAssignee)
	return set, nil
}

// apiURLRefusal states what was wrong with an api_url without quoting any part
// of it, and marks the value unprintable whatever its provenance
// (core.ConfigValueError.Sensitive). The value still rides as data: a renderer
// that redacts it can still name the variable it came from.
func apiURLRefusal(value string, named error, because string) error {
	return &core.ConfigValueError{
		Field:     "tracker.provider.api_url",
		Value:     value,
		Sensitive: true,
		Err:       fmt.Errorf("%w: %s", named, because),
	}
}

// nonEndpointComponents names what a URL carries beyond the endpoint it
// addresses, in a fixed order so a refusal reads the same way twice. Path is
// absent deliberately: `/gh/api/v3/` is where a GitHub Enterprise instance
// genuinely lives.
func nonEndpointComponents(u *url.URL) []string {
	var carried []string
	if u.User != nil {
		carried = append(carried, "userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery {
		carried = append(carried, "a query")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		carried = append(carried, "a fragment")
	}
	return carried
}

func providerString(provider map[string]any, key string) (string, error) {
	v, ok := provider[key]
	if !ok || v == nil {
		return "", nil
	}
	str, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: tracker.provider.%s must be a string, got %T", ErrProviderKeyType, key, v)
	}
	return strings.TrimSpace(str), nil
}
