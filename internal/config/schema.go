// Package config loads and validates the repo-owned WORKFLOW.md contract
// (SPEC §5): strict front-matter parsing with two opaque carve-outs, $VAR
// secret indirection, defaults, per-field provenance, and the workflow key.
//
// The load result is a WorkflowDefinition — the single value that
// parameterizes the orchestrator core (SPEC §3.7). Nothing in the core reads
// process-global config.
package config

import (
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/template"
)

// SupportedVersion is the config-format version this daemon understands
// (SPEC §5.2.1).
const SupportedVersion = 1

// Defaults (SPEC §5.2). workspace.root's default is computed at load time
// from XDG_DATA_HOME.
const (
	DefaultPollingIntervalMS   = 30000
	DefaultHooksTimeoutMS      = 60000
	DefaultMaxConcurrentAgents = 3
	DefaultMaxTurns            = 4
	DefaultMaxAttempts         = 3
	DefaultMaxRetryBackoffMS   = 300000
	DefaultStallTimeoutMS      = 300000
	DefaultAttemptTimeoutMS    = 3600000
	// DefaultMaxPromptBytes is the template layer's own ceiling, not a second
	// copy of it: the number a zero template.Limits applies and the number an
	// omitted `limits.max_prompt_bytes` resolves to have to be the same, or a
	// caller that forgets the config would enforce a different bound than the
	// operator configured (SPEC §5.6).
	DefaultMaxPromptBytes = template.DefaultMaxPromptBytes
)

// The closed v1 adapter kind sets live in internal/registry — the one table
// both the loader and `ben config effective` ask (SPEC §2.2, §7.7, §8.4).

// Config is the fully-resolved, validated front matter. The top-level key set
// is closed (SPEC §5.2).
type Config struct {
	Version int
	Tracker TrackerConfig
	// CredentialSources are the declared `credential_sources` entries by name
	// (SPEC §5.2, amendment 1). Nil when the OPTIONAL section is absent, which
	// is every configuration that predates it.
	CredentialSources map[string]SourceConfig
	Polling           PollingConfig
	Workspace         WorkspaceConfig
	Hooks             HooksConfig
	Agent             AgentConfig
	Publish           PublishConfig
	Limits            LimitsConfig
	Deployment        DeploymentConfig
	// Credentials is what the declared entries and the legacy spellings
	// **compile to**: one descriptor per consumer. Derived, never written by an
	// operator, and the only thing downstream of the loader reads.
	Credentials Credentials
}

// DeploymentMode is SPEC §10.1's declared posture, as a closed set.
//
// It declares what the *deployment* has arranged and changes nothing about how
// a run behaves — unlike agent.provider.permission_mode, which is a behavioural
// knob. BEN verifies none of it: §10.1 is explicit that requirement 2 is "a
// property, not a mechanism", endorses none, and owes verification to the
// deployment. What BEN can do is refuse to proceed without the declaration,
// which is what "MUST NOT arrive in this mode by default or by omission" needs
// from a program.
type DeploymentMode string

const (
	// DeploymentProtected asserts all three of §10.1's requirements.
	DeploymentProtected DeploymentMode = "protected"
	// DeploymentRiskAccepted asserts requirement 2 relaxed and only 2, with the
	// reason recorded. The agent is trusted with the daemon's tracker authority
	// and the dispatch label is routing rather than a boundary.
	DeploymentRiskAccepted DeploymentMode = "risk-accepted"
	// DeploymentAttended is §10.1's on-ramp exemption: a human is present for
	// the whole lifetime of this process. BEN cannot detect them leaving, which
	// is why this is declared rather than sensed and why changing it needs a
	// restart.
	DeploymentAttended DeploymentMode = "attended"
)

// deploymentModes is the closed set, in the order refusals list them.
//
// An array rather than a slice, and unexported: a package-level slice is a
// mutable global, and this one is what validation compares against — so an
// exported one lets any importer widen or empty the closed set of SPEC §10.1 at
// run time, from anywhere, with no compile error.
var deploymentModes = [3]DeploymentMode{DeploymentProtected, DeploymentRiskAccepted, DeploymentAttended}

// DeploymentModes returns the closed set (SPEC §5.2.9).
//
// The copy is the whole point and it is easy to get wrong: `deploymentModes[:]`
// slices the package-level array itself, so a caller writing to the result
// rewrites the set validation compares against. Assigning to a local first
// copies the array — that is what an array buys over a slice — and the return
// then aliases the copy.
func DeploymentModes() []DeploymentMode {
	out := deploymentModes
	return out[:]
}

// DeploymentConfig is SPEC §5.2.9.
type DeploymentConfig struct {
	Mode DeploymentMode
	// AcceptedBecause is the recorded half of "an explicit, recorded choice"
	// (§10.1), required and non-blank for risk-accepted and optional otherwise.
	// Operator prose, never a secret: it is logged at startup and printed by
	// `config effective`, because a record nobody can read is not a record.
	AcceptedBecause string
}

type TrackerConfig struct {
	Kind string
	// Provider is the adapter-owned opaque block (SPEC §5.2.2): exempt from
	// strict key validation, $VAR-resolved string leaves, adapter-validated.
	Provider       map[string]any
	RequiredLabels []string
	// ActiveStates/TerminalStates nil means the adapter default applies
	// (GitHub: open/closed).
	ActiveStates   []string
	TerminalStates []string
	// ClaimAssignee is `tracker.provider.claim_assignee`, promoted out of the
	// opaque block because §8.4 makes it a field the adapter is constructed
	// from rather than a value it reads (amendment 10). Empty preserves the
	// credential-authenticated login fallback, which a bounded credential
	// source is refused for at load.
	ClaimAssignee string
}

type PollingConfig struct {
	IntervalMS int
}

type WorkspaceConfig struct {
	// Root is normalized to an absolute path at load (SPEC §5.2.4).
	Root string
}

type HooksConfig struct {
	// Hook scripts are executed by a shell; $VAR indirection deliberately
	// does not apply to them — shell expansion is the shell's job.
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	TimeoutMS    int
}

type AgentConfig struct {
	Kind     string
	Provider map[string]any
}

// AgentBinding is the slice of configuration the runner kind is bound to at New,
// and asked about at Structural (SPEC §5.7, §7.1): the opaque block plus the
// core-owned publish credential beside it.
//
// One expression, two callers, deliberately. The assembly builds a runner from it,
// and adapterChange decides whether a reload must rebuild that runner by comparing
// it — so a field added here cannot reach an adapter without also being a reason to
// rebuild. Two spellings is the hot-reload bug this repo has now hit three times:
// the unchanged-file check compares the whole config while the rebuild predicate
// compared a subset, so an edit published a new definition, rebuilt nothing, and
// logged as adopted.
func (c Config) AgentBinding() core.AgentConfig {
	return core.AgentConfig{
		Provider: c.Agent.Provider,
		Publish:  c.Publish.Credential(),
		// The publisher's source, name-free (SPEC §5.4, amendment 2).
		PublishSource: c.Credentials.Publish.Binding(),
		// And the TTL gate's other operand. It lives in `limits` and not here
		// today; this contract puts it here, because Ready applies that gate and
		// a runner is therefore *bound to* the timeout (SPEC §7.7).
		AttemptTimeout: time.Duration(c.Limits.AttemptTimeoutMS) * time.Millisecond,
	}
}

// PublishKindToken names a variable holding the credential; PublishKindSource
// names a `credential_sources` entry (SPEC §5.2.8, amendment 1). The set is
// closed for the same reason `tracker.kind` and `agent.kind` are: a typo in the
// credential's kind is not something to discover at the publish step.
const PublishKindToken = "token"

// publishKinds is the closed set, for the refusal message. A list rather than a
// registry: unlike an adapter kind, a publish kind is not a pluggable
// implementation an operator can extend — it selects between mechanisms this
// package implements.
var publishKinds = []string{PublishKindToken, PublishKindSource}

// PublishConfig is the publish credential as the file states it (SPEC §5.2.8):
// the credential the *agent* authenticates its push and pull request with.
//
// It holds a **reference**, never a secret. `publish.value` is exactly one `$VAR`
// reference and is deliberately not resolved at load, so this struct can be
// built — and `config effective` can print it — on a host that holds no publish
// credential at all (SPEC §5.5). The zero value means the OPTIONAL block was
// absent: BEN then injects no publish credential, and the agent authenticates
// from what §7.6's allowlist already carries.
type PublishConfig struct {
	// Kind selects how the credential is obtained; empty when the block is
	// absent.
	Kind string
	// Env is the child environment variable the resolved credential is injected
	// as. It is owned exclusively by the publish credential (SPEC §7.6).
	Env string
	// ValueVar is the variable name behind `value`'s single `$VAR` reference —
	// "GH_TOKEN" for `value: $GH_TOKEN`. Never the secret: nothing in this
	// package reads it. Empty under kind `source`.
	ValueVar string
	// Source names the `credential_sources` entry, under kind `source`
	// (SPEC §5.2.8, amendment 1). Empty under kind `token`.
	Source string
}

// Configured reports whether the workflow states a publish credential.
func (p PublishConfig) Configured() bool { return p.Kind != "" }

// Credential projects the block onto the reference the adapters are handed
// (SPEC §5.2.8, §7.1). The projection is the whole of what crosses that
// boundary: a kind is this package's business, and a value does not exist yet.
func (p PublishConfig) Credential() core.PublishCredential {
	if !p.Configured() {
		return core.PublishCredential{}
	}
	return core.PublishCredential{Env: p.Env, Var: p.ValueVar}
}

// ValueReference renders `value` as the file wrote it. Reconstructed rather than
// stored, which is lossless because validation accepts exactly one `$VAR` and
// nothing else (SPEC §5.2.8) — and keeps one spelling of the field rather than a
// raw copy that could drift from the parsed name.
func (p PublishConfig) ValueReference() string {
	if p.ValueVar == "" {
		return ""
	}
	return "$" + p.ValueVar
}

// LimitsConfig is the closed orchestrator knob set (SPEC §5.2.7).
type LimitsConfig struct {
	MaxConcurrentAgents int
	MaxTurns            int
	MaxAttempts         int
	MaxRetryBackoffMS   int
	// MaxCostUSD nil means the budget cap is disabled.
	MaxCostUSD       *float64
	StallTimeoutMS   int
	AttemptTimeoutMS int
	// MaxPromptBytes caps the rendered prompt (SPEC §5.6). Unlike MaxCostUSD,
	// there is no spelling that disables it: §5.6 makes refusing an oversized
	// prompt a MUST, because an issue body is attacker-controlled token spend
	// and truncating it would cut the closing fence off the untrusted span.
	MaxPromptBytes int
}

// Source records where an effective field value came from (SPEC §5.8).
type Source string

const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	// SourceAdapter marks fields whose default the adapter owns
	// (active_states/terminal_states left unset).
	SourceAdapter Source = "adapter default"
)

// FieldOrigin is the provenance entry for one dotted field path.
type FieldOrigin struct {
	Source Source
	// EnvVars names **every** variable a SourceEnv value interpolated, in
	// order. Values inside provider blocks resolved from env are treated as
	// secrets and redacted in all `config effective` output (SPEC §5.8).
	//
	// A list rather than one name because a value may interpolate several
	// variables, and each contributes its secret to the result. The §10.2
	// credential check reads this to decide whether one secret is doing two
	// jobs, so recording only one of them is a hole, not an imprecision:
	// `$TRACKER_PAT-$SUFFIX` remembered as "SUFFIX" is a tracker credential
	// nothing can see.
	EnvVars []string
}

// EnvVarLabel renders the variables behind an env-resolved value for operator
// output — "$A", or "$A, $B" where a value interpolated several. Empty for a
// value that referenced none.
func (o FieldOrigin) EnvVarLabel() string {
	if len(o.EnvVars) == 0 {
		return ""
	}
	return "$" + strings.Join(o.EnvVars, ", $")
}

// Provenance maps structural field paths to their origin. Ordinary map keys
// use dotted segments (for example, "tracker.provider.token"); provider keys
// containing path syntax use quoted bracket segments to prevent collisions.
type Provenance map[string]FieldOrigin

// WorkflowDefinition is the load result: config plus the prompt template,
// both raw (for introspection) and compiled with load-time strictness
// already enforced (SPEC §5.6).
type WorkflowDefinition struct {
	Config         Config
	PromptTemplate string
	// prompt is unexported so RenderPrompt is the only way to render one.
	// Handing out the compiled template means handing out its limits argument,
	// and the literal a call site reaches for is template.Limits{} — which
	// enforces the package default in place of the operator's
	// `limits.max_prompt_bytes`. Nothing outside this package needs the
	// compiled form: PromptTemplate carries the source for introspection
	// (SPEC §5.6, §5.8).
	prompt *template.Prompt
	// Path is the absolute path of the loaded WORKFLOW.md.
	Path string
	// Key is the stable workflow identity (SPEC §5.1): sanitized parent
	// directory basename + 8 hex chars of FNV-1a of Path.
	Key        string
	Provenance Provenance
}

// RenderPrompt renders the attempt's prompt under this workflow's own configured
// ceiling — the only render path, by construction (see prompt).
//
// A failure here is contained: it fails this attempt, not the load (SPEC §5.7).
func (d *WorkflowDefinition) RenderPrompt(v template.Vars) (string, error) {
	if d.prompt == nil {
		// Load always compiles or fails, so this is a definition that did not
		// come from Load — a zero value, or a literal assembled in a test. It
		// refuses rather than dereferencing nil, because a panic here would
		// surface as an engine bug rather than as the miswiring it is.
		return "", ErrNoCompiledPrompt
	}
	return d.prompt.Render(v, d.promptLimits())
}

// promptLimits projects the config onto the template layer's bound. Unexported
// for the same reason as prompt: a caller holding limits without the template,
// or the template without limits, is a caller that can render with the wrong
// ceiling.
func (d *WorkflowDefinition) promptLimits() template.Limits {
	return template.Limits{MaxPromptBytes: d.Config.Limits.MaxPromptBytes}
}

// raw* mirror the YAML schema exactly. Every field is a pointer (or map) so
// set-vs-unset drives defaults and provenance. Strict decoding
// (KnownFields) rejects unknown keys everywhere except the two opaque
// provider blocks (SPEC §5.3).
type rawConfig struct {
	Version *int        `yaml:"version"`
	Tracker *rawTracker `yaml:"tracker"`
	// CredentialSources is open per entry, like a provider block: the keys
	// beneath a name belong to that entry's kind, which validates them
	// (SPEC §5.2, §5.3, amendment 1). The *names* are the operator's.
	CredentialSources map[string]map[string]any `yaml:"credential_sources"`
	Polling           *rawPolling               `yaml:"polling"`
	Workspace         *rawWorkspace             `yaml:"workspace"`
	Hooks             *rawHooks                 `yaml:"hooks"`
	Agent             *rawAgent                 `yaml:"agent"`
	Publish           *rawPublish               `yaml:"publish"`
	Limits            *rawLimits                `yaml:"limits"`
	Deployment        *rawDeployment            `yaml:"deployment"`
}

// rawDeployment is SPEC §5.2.9. Neither field is $VAR-resolved: a mode is a
// closed keyword and a reason is prose, and neither is a place a secret belongs.
type rawDeployment struct {
	Mode            *string `yaml:"mode"`
	AcceptedBecause *string `yaml:"accepted_because"`
}

type rawTracker struct {
	Kind           *string        `yaml:"kind"`
	Provider       map[string]any `yaml:"provider"`
	RequiredLabels []string       `yaml:"required_labels"`
	ActiveStates   []string       `yaml:"active_states"`
	TerminalStates []string       `yaml:"terminal_states"`
}

type rawPolling struct {
	IntervalMS *int `yaml:"interval_ms"`
}

type rawWorkspace struct {
	Root *string `yaml:"root"`
}

type rawHooks struct {
	AfterCreate  *string `yaml:"after_create"`
	BeforeRun    *string `yaml:"before_run"`
	AfterRun     *string `yaml:"after_run"`
	BeforeRemove *string `yaml:"before_remove"`
	TimeoutMS    *int    `yaml:"timeout_ms"`
}

type rawAgent struct {
	Kind     *string        `yaml:"kind"`
	Provider map[string]any `yaml:"provider"`
}

// rawPublish is SPEC §5.2.8. `value` is a string like every other field here and
// is deliberately *not* run through $VAR resolution: it names the variable rather
// than holding what the variable holds.
type rawPublish struct {
	Kind   *string `yaml:"kind"`
	Env    *string `yaml:"env"`
	Value  *string `yaml:"value"`
	Source *string `yaml:"source"`
}

type rawLimits struct {
	MaxConcurrentAgents *int     `yaml:"max_concurrent_agents"`
	MaxTurns            *int     `yaml:"max_turns"`
	MaxAttempts         *int     `yaml:"max_attempts"`
	MaxRetryBackoffMS   *int     `yaml:"max_retry_backoff_ms"`
	MaxCostUSD          *float64 `yaml:"max_cost_usd"`
	StallTimeoutMS      *int     `yaml:"stall_timeout_ms"`
	AttemptTimeoutMS    *int     `yaml:"attempt_timeout_ms"`
	MaxPromptBytes      *int     `yaml:"max_prompt_bytes"`
}
