package config

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
)

// The v2 execution-substrate declaration (#194, #46): where an attempt runs.
//
// **This section is closed, not opaque.** `tracker.provider` and
// `agent.provider` are exempt from strict key validation because an adapter owns
// their schema and the loader cannot know it (SPEC §5.2.2, §5.2.5). Nothing here
// is like that: the substrate is BEN's own choice of where to execute, every key
// below is one this package validates, and a typo in an endpoint or a retention
// policy is not something to discover at the first dispatched claim. So a `kind`
// is a closed keyword, `airlock` is a closed key set, and an unknown key refuses.
//
// The kind set is closed the way `publish.kind` is and not the way `agent.kind`
// is, for `publish.kind`'s reason: these are not pluggable implementations an
// operator can extend, they select between mechanisms this repository
// implements. There is no registry entry to add, and no third value to write.
//
// Nothing here is `$VAR`-resolved. A base URL is not a secret — it is refused
// outright if it embeds one — and a profile, a retention policy and a timeout are
// not places a secret belongs. Resolving them would print an environment value
// in `config effective` and, worse, would make an endpoint carrying userinfo
// arrive from a variable that the load-time refusal below could no longer see in
// the file. The one credential this section needs is named indirectly, through
// `credential_sources`, which is the whole point of that section.

// SubstrateKindLocal runs an attempt as a subprocess in a git worktree on the
// daemon's own host: the locked v1 behaviour (SPEC §6, §7), and what an omitted
// section resolves to. Every workflow that predates this section keeps working
// unchanged, which is deliberate — unlike `deployment.mode`, this declaration is
// not a safety posture §10.1 forbids arriving at by omission.
const SubstrateKindLocal = "local"

// SubstrateKindAirlock runs an attempt in an Airlock v2 sandbox through the
// durable process API (docs/AIRLOCK.md).
const SubstrateKindAirlock = "airlock"

// substrateKinds is the closed set, in the order refusals list them. An array
// rather than a slice, and unexported, for deploymentModes' reason: an exported
// slice is a mutable global, and this one decides where a run executes.
var substrateKinds = [2]string{SubstrateKindLocal, SubstrateKindAirlock}

// SubstrateKinds returns the closed set. A copy — see DeploymentModes.
func SubstrateKinds() []string {
	out := substrateKinds
	return out[:]
}

// Disposal keywords: what BEN does with a claim's remote workspace when the
// claim ends. The set is closed, and `delete` is deliberately never a default —
// it is the only one that destroys work, and a policy nobody wrote must not.
const (
	DisposalRetain  = "retain"
	DisposalSuspend = "suspend"
	DisposalDelete  = "delete"
)

var disposals = [3]string{DisposalRetain, DisposalSuspend, DisposalDelete}

// Disposals returns the closed set. A copy, as above.
func Disposals() []string {
	out := disposals
	return out[:]
}

// Airlock defaults. Each is the value an omitted key resolves to, and each is
// printed by `config effective` with provenance `default` so an operator can see
// what they did not write.
const (
	DefaultAirlockRequestTimeoutMS = 30000
	DefaultAirlockPollTimeoutMS    = 70000
	DefaultAirlockPollWaitMS       = 30000
	DefaultAirlockSettleTimeoutMS  = 300000
	DefaultAirlockMaxRetries       = 4
	// The end-of-claim defaults. Suspend releases compute and keeps the volume,
	// so a later attempt resumes rather than rebuilds; failure retains the warm
	// sandbox, mirroring §6.4's local rule that a failed attempt's worktree is
	// kept for forensics.
	DefaultAirlockOnSuccess  = DisposalSuspend
	DefaultAirlockOnFailure  = DisposalRetain
	DefaultAirlockOnRevoked  = DisposalSuspend
	DefaultAirlockOnShutdown = DisposalSuspend
)

// SubstrateConfig is the resolved declaration.
type SubstrateConfig struct {
	// Kind is one of SubstrateKinds. Never empty after Load.
	Kind string
	// Airlock is populated only under SubstrateKindAirlock; the zero value
	// under `local`, which validation guarantees rather than hopes.
	Airlock AirlockConfig
}

// Remote reports whether attempts run somewhere other than this host.
func (s SubstrateConfig) Remote() bool { return s.Kind == SubstrateKindAirlock }

// SubstrateBinding is the complete process-lifetime identity of the execution
// backend: its resolved declaration and the canonical definition of the
// credential source it holds. AuthSource's name alone is insufficient because
// a reload may edit the referenced source beneath an unchanged name.
type SubstrateBinding struct {
	Config     SubstrateConfig
	Credential core.SourceBinding
}

// SubstrateBinding projects Config onto the identity a running substrate is
// bound to. The zero credential is part of the local substrate's identity.
func (c Config) SubstrateBinding() SubstrateBinding {
	return SubstrateBinding{Config: c.Substrate, Credential: c.Credentials.Substrate.Binding()}
}

// AirlockConfig is the closed `substrate.airlock` key set.
type AirlockConfig struct {
	// BaseURL is the cluster-internal endpoint. HTTPS, and refused if it
	// embeds credentials in its userinfo (gitremote.EmbedsCredential — the same
	// check both Git drivers apply, not a second implementation of it).
	BaseURL string
	// Profile is the operator-approved profile a sandbox is provisioned from.
	// BEN never submits a pod or sandbox spec; it names this and the backend
	// pins an immutable revision.
	Profile string
	// AuthSource names a `credential_sources` entry that mints the backend
	// bearer token. Indirect, and required: the alternative spellings are a
	// literal in the file or a variable read here, and both put a credential in
	// a section `config effective` prints in full.
	AuthSource string
	// TLSCAFile is a PEM bundle the endpoint's certificate is verified against.
	// Empty uses the host's roots, which is the ordinary case; a private CA is
	// the reason this exists.
	TLSCAFile string

	RequestTimeoutMS int
	PollTimeoutMS    int
	PollWaitMS       int
	SettleTimeoutMS  int
	MaxRetries       int

	// IdleSuspendMS and DeleteAfterIdleMS are the idle windows BEN asks the
	// backend to enforce on its own. Zero leaves the profile's defaults. They
	// exist because a daemon that crashes between a claim's attempts would
	// otherwise leave a warm sandbox and a volume allocated with nothing left to
	// release them.
	IdleSuspendMS     int
	DeleteAfterIdleMS int

	// The end-of-claim disposals, one per outcome. A retry is deliberately not
	// among them: the claim's workspace is always reused, because §6.2
	// reattaches and the tree carries the previous attempt's work.
	OnSuccess  string
	OnFailure  string
	OnRevoked  string
	OnShutdown string
}

// rawSubstrate mirrors the YAML. Airlock is a pointer so "written but empty" is
// distinguishable from absent, which is what the mixed-configuration refusal
// below rests on.
type rawSubstrate struct {
	Kind    *string     `yaml:"kind"`
	Airlock *rawAirlock `yaml:"airlock"`
}

type rawAirlock struct {
	BaseURL           *string  `yaml:"base_url"`
	Profile           *string  `yaml:"profile"`
	AuthSource        *string  `yaml:"auth_source"`
	TLSCAFile         *string  `yaml:"tls_ca_file"`
	RequestTimeoutMS  *yamlInt `yaml:"request_timeout_ms"`
	PollTimeoutMS     *yamlInt `yaml:"poll_timeout_ms"`
	PollWaitMS        *yamlInt `yaml:"poll_wait_ms"`
	SettleTimeoutMS   *yamlInt `yaml:"settle_timeout_ms"`
	MaxRetries        *yamlInt `yaml:"max_retries"`
	IdleSuspendMS     *yamlInt `yaml:"idle_suspend_ms"`
	DeleteAfterIdleMS *yamlInt `yaml:"delete_after_idle_ms"`
	OnSuccess         *string  `yaml:"on_success"`
	OnFailure         *string  `yaml:"on_failure"`
	OnRevoked         *string  `yaml:"on_revoked"`
	OnShutdown        *string  `yaml:"on_shutdown"`
}

// resolveSubstrate applies defaults and records provenance.
//
// The mixed-configuration refusal happens here rather than in validate for
// workspace.root's reason: the fact it rests on — was the `airlock` key
// *written*? — exists at this layer and nowhere later. By the time validation
// sees a Config, a written-but-empty block and an omitted one are the same zero
// value, and the whole point is that they are not the same statement.
func resolveSubstrate(raw *rawSubstrate, cfg *Config, prov Provenance) error {
	sb := raw
	if sb == nil {
		sb = &rawSubstrate{}
	}
	kind := SubstrateKindLocal
	if sb.Kind != nil {
		kind = strings.TrimSpace(*sb.Kind)
		prov["substrate.kind"] = FieldOrigin{Source: SourceFile}
	} else {
		prov["substrate.kind"] = FieldOrigin{Source: SourceDefault}
	}
	cfg.Substrate.Kind = kind

	switch {
	case kind != SubstrateKindAirlock && sb.Airlock != nil:
		return &ValidationError{Field: "substrate.airlock", Msg: fmt.Sprintf(
			"written under substrate.kind %q: a local substrate has no backend to configure, and a "+
				"block that is read by nothing is a setting an operator believes is in effect", kind)}
	case kind == SubstrateKindAirlock && sb.Airlock == nil:
		return &ValidationError{Field: "substrate.airlock", Msg: fmt.Sprintf(
			"required under substrate.kind %q: the endpoint, the approved profile and the auth source "+
				"have no defaults", kind)}
	case sb.Airlock == nil:
		return nil
	}

	air := sb.Airlock
	setStr := func(path string, dst *string, src *string) {
		if src != nil {
			*dst, prov[path] = strings.TrimSpace(*src), FieldOrigin{Source: SourceFile}
		} else {
			prov[path] = FieldOrigin{Source: SourceDefault}
		}
	}
	setInt := func(path string, dst *int, src *yamlInt, fallback int) {
		if src != nil {
			*dst, prov[path] = src.Int(), FieldOrigin{Source: SourceFile}
		} else {
			*dst, prov[path] = fallback, FieldOrigin{Source: SourceDefault}
		}
	}
	setDisposal := func(path string, dst *string, src *string, fallback string) {
		if src != nil {
			*dst, prov[path] = strings.TrimSpace(*src), FieldOrigin{Source: SourceFile}
		} else {
			*dst, prov[path] = fallback, FieldOrigin{Source: SourceDefault}
		}
	}

	a := &cfg.Substrate.Airlock
	setStr("substrate.airlock.base_url", &a.BaseURL, air.BaseURL)
	setStr("substrate.airlock.profile", &a.Profile, air.Profile)
	setStr("substrate.airlock.auth_source", &a.AuthSource, air.AuthSource)
	setStr("substrate.airlock.tls_ca_file", &a.TLSCAFile, air.TLSCAFile)
	setInt("substrate.airlock.request_timeout_ms", &a.RequestTimeoutMS, air.RequestTimeoutMS, DefaultAirlockRequestTimeoutMS)
	setInt("substrate.airlock.poll_timeout_ms", &a.PollTimeoutMS, air.PollTimeoutMS, DefaultAirlockPollTimeoutMS)
	setInt("substrate.airlock.poll_wait_ms", &a.PollWaitMS, air.PollWaitMS, DefaultAirlockPollWaitMS)
	setInt("substrate.airlock.settle_timeout_ms", &a.SettleTimeoutMS, air.SettleTimeoutMS, DefaultAirlockSettleTimeoutMS)
	setInt("substrate.airlock.max_retries", &a.MaxRetries, air.MaxRetries, DefaultAirlockMaxRetries)
	setInt("substrate.airlock.idle_suspend_ms", &a.IdleSuspendMS, air.IdleSuspendMS, 0)
	setInt("substrate.airlock.delete_after_idle_ms", &a.DeleteAfterIdleMS, air.DeleteAfterIdleMS, 0)
	setDisposal("substrate.airlock.on_success", &a.OnSuccess, air.OnSuccess, DefaultAirlockOnSuccess)
	setDisposal("substrate.airlock.on_failure", &a.OnFailure, air.OnFailure, DefaultAirlockOnFailure)
	setDisposal("substrate.airlock.on_revoked", &a.OnRevoked, air.OnRevoked, DefaultAirlockOnRevoked)
	setDisposal("substrate.airlock.on_shutdown", &a.OnShutdown, air.OnShutdown, DefaultAirlockOnShutdown)
	return nil
}

// validateSubstrate enforces the value rules (SPEC §5.7's posture, applied to a
// section this package owns end to end).
func validateSubstrate(cfg *Config) error {
	kind := cfg.Substrate.Kind
	if !containsString(SubstrateKinds(), kind) {
		return &ValidationError{Field: "substrate.kind", Msg: fmt.Sprintf(
			"%q is not a supported substrate (supported: %s)", kind, strings.Join(SubstrateKinds(), ", "))}
	}
	if kind != SubstrateKindAirlock {
		return nil
	}

	a := cfg.Substrate.Airlock
	if err := validateAirlockURL(a.BaseURL); err != nil {
		return err
	}
	if a.Profile == "" {
		return &ValidationError{Field: "substrate.airlock.profile", Msg: "required: BEN names an operator-approved profile and never submits a sandbox spec"}
	}
	if a.AuthSource == "" {
		return &ValidationError{Field: "substrate.airlock.auth_source", Msg: fmt.Sprintf(
			"required: name a `credential_sources` entry (declared: %s)", declaredSourceNames(cfg))}
	}
	if _, ok := cfg.CredentialSources[a.AuthSource]; !ok {
		return unknownSourceRef("substrate.airlock.auth_source", a.AuthSource, cfg)
	}

	positives := []struct {
		field string
		v     int
	}{
		{"substrate.airlock.request_timeout_ms", a.RequestTimeoutMS},
		{"substrate.airlock.poll_timeout_ms", a.PollTimeoutMS},
		{"substrate.airlock.poll_wait_ms", a.PollWaitMS},
		{"substrate.airlock.settle_timeout_ms", a.SettleTimeoutMS},
		{"substrate.airlock.max_retries", a.MaxRetries},
	}
	for _, p := range positives {
		if p.v <= 0 {
			return &ValidationError{Field: p.field, Msg: "must be a positive integer"}
		}
	}
	// A server asked to hold a poll open for longer than the client will wait
	// makes every long poll look like a transport failure.
	if a.PollWaitMS >= a.PollTimeoutMS {
		return &ValidationError{Field: "substrate.airlock.poll_wait_ms", Msg: fmt.Sprintf(
			"%dms must be shorter than substrate.airlock.poll_timeout_ms (%dms), or every long poll "+
				"expires client-side while the backend is still holding it", a.PollWaitMS, a.PollTimeoutMS)}
	}
	// The backend's own floor for both idle windows. Refused rather than raised
	// silently: an operator who wrote 30 seconds asked for something this
	// contract cannot express, and quietly giving them 60 is a different policy.
	for _, w := range []struct {
		field string
		v     int
	}{
		{"substrate.airlock.idle_suspend_ms", a.IdleSuspendMS},
		{"substrate.airlock.delete_after_idle_ms", a.DeleteAfterIdleMS},
	} {
		if w.v != 0 && w.v < 60000 {
			return &ValidationError{Field: w.field, Msg: "must be at least 60000ms, or 0 to leave the profile's own window"}
		}
	}

	for _, d := range []struct {
		field string
		v     string
	}{
		{"substrate.airlock.on_success", a.OnSuccess},
		{"substrate.airlock.on_failure", a.OnFailure},
		{"substrate.airlock.on_revoked", a.OnRevoked},
		{"substrate.airlock.on_shutdown", a.OnShutdown},
	} {
		if !containsString(Disposals(), d.v) {
			return &ValidationError{Field: d.field, Msg: fmt.Sprintf(
				"%q is not a supported disposal (supported: %s)", d.v, strings.Join(Disposals(), ", "))}
		}
	}
	return nil
}

// validateAirlockURL refuses an endpoint before any credential is minted for it.
//
// The rejected value never appears in the message. It is the one field in this
// section that could carry a secret, and the refusal for that case is precisely
// the one an operator would paste into an issue.
func validateAirlockURL(raw string) error {
	const field = "substrate.airlock.base_url"
	if raw == "" {
		return &ValidationError{Field: field, Msg: "required"}
	}
	if gitremote.EmbedsCredential(raw) {
		return &ValidationError{Field: field, Msg: "embeds credentials; authenticate through substrate.airlock.auth_source instead"}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return &ValidationError{Field: field, Msg: "must be an absolute https URL with a host"}
	}
	if u.Scheme != "https" {
		return &ValidationError{Field: field, Msg: fmt.Sprintf(
			"must be https, not %q: the bearer token and the run's whole output cross this connection", u.Scheme)}
	}
	if u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") {
		return &ValidationError{Field: field, Msg: "must name only an endpoint: query and fragment components are not allowed"}
	}
	return nil
}

func containsString(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func declaredSourceNames(cfg *Config) string {
	if len(cfg.CredentialSources) == 0 {
		return "none"
	}
	names := make([]string, 0, len(cfg.CredentialSources))
	for name := range cfg.CredentialSources {
		names = append(names, name)
	}
	sortStrings(names)
	return strings.Join(names, ", ")
}

func sortStrings(in []string) {
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && in[j] < in[j-1]; j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
}
