package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
)

// RenderRefusal formats a structural refusal for operator output. A refusal
// carrying its offending value (core.ConfigValueError) shows it only when the
// value provably came from the workflow file *and* nothing else says it is a
// secret. Anything else — env-resolved, provenance unknown, a path its adapter
// declared sensitive, or a value the producer marked — is redacted, named by its
// variable when there is one.
//
// Three independent things earn redaction, and no two of them subsume each
// other, which is why this asks `hides` rather than reading provenance alone:
// where the value came from, whether its *path* is always a secret
// (Kind.SensitiveFields), and whether this particular value carries a credential
// in its shape (core.ConfigValueError.Sensitive) — the last being the only one
// answerable by the adapter that parsed it, and the one an `api_url` with
// userinfo in it needs.
//
// The value travels as data and the decision happens here, where provenance
// lives, because scrubbing rendered text after the fact cannot be made safe
// (SPEC §5.8): %q-escaping makes the emitted form diverge from the raw
// value, and short values collide with innocent substrings of the message.
func RenderRefusal(def *WorkflowDefinition, err error) string {
	var verr *core.ConfigValueError
	if !errors.As(err, &verr) {
		return err.Error()
	}
	origin, known := def.Provenance[verr.Field]
	if known && !verr.Sensitive && !newRedactor(def).hides(verr.Field) {
		return fmt.Sprintf("%s: got %q", verr, verr.Value)
	}
	marker := Redacted
	if label := origin.EnvVarLabel(); known && label != "" {
		marker = "[redacted " + label + "]"
	}
	return fmt.Sprintf("%s: got %s", verr, marker)
}

// Redacted replaces every hidden value in all `config effective` output
// (SPEC §5.8). Two things earn it, and neither subsumes the other (see
// redactor): a value a provider block pulled from the environment, since
// secrets live in provider blocks by design (SPEC §10.2); and a value at a path
// its adapter declared sensitive, whatever its provenance.
//
// The second half used to be absent, and a literal credential written into the
// file printed in the clear — on the reasoning that it was already public to
// anyone who could read the repo. That holds for the repo and not for this
// output, which is pasted into pull requests, issues and CI logs, and
// WORKFLOW.md need not be committed at all (#52).
const Redacted = "[redacted]"

// EffectiveText renders the fully-resolved configuration with per-field
// provenance annotations, for `ben config effective`.
func EffectiveText(def *WorkflowDefinition) string {
	red := newRedactor(def)
	var b strings.Builder
	w := func(indent int, field, value, path string) {
		line := strings.Repeat("  ", indent) + field + ": " + value
		fmt.Fprintf(&b, "%-52s (%s)\n", line, originLabel(def.Provenance, path))
	}

	fmt.Fprintf(&b, "workflow: %s\n", def.Path)
	fmt.Fprintf(&b, "workflow_key: %s\n\n", def.Key)

	cfg := def.Config
	w(0, "version", fmt.Sprint(cfg.Version), "version")

	b.WriteString("tracker:\n")
	w(1, "kind", cfg.Tracker.Kind, "tracker.kind")
	writeProvider(&b, def, red, 1, "tracker.provider", cfg.Tracker.Provider)
	w(1, "required_labels", renderList(cfg.Tracker.RequiredLabels), "tracker.required_labels")
	w(1, "active_states", renderList(cfg.Tracker.ActiveStates), "tracker.active_states")
	w(1, "terminal_states", renderList(cfg.Tracker.TerminalStates), "tracker.terminal_states")

	// Rendered in full and deliberately not routed through the redactor: a
	// source block holds no secret (SPEC §5.8, amendment 5). Every `octo_sts`
	// field is a non-secret literal — an issuer URL, a policy scope, an
	// identity, a path — and a `static` source's `value` is a variable
	// *reference*, printed as written for the reason `publish.value` is. Hiding
	// them would conceal exactly what an operator needs when a trust policy is
	// wrong.
	writeSources(&b, def, cfg.CredentialSources)

	b.WriteString("polling:\n")
	w(1, "interval_ms", fmt.Sprint(cfg.Polling.IntervalMS), "polling.interval_ms")

	b.WriteString("workspace:\n")
	w(1, "root", fmt.Sprint(red.value("workspace.root", cfg.Workspace.Root)), "workspace.root")
	baseBranch := cfg.Workspace.BaseBranch
	if baseBranch == "" {
		baseBranch = "<repository-default>"
	}
	w(1, "base_branch", baseBranch, "workspace.base_branch")

	b.WriteString("hooks:\n")
	w(1, "after_create", renderScript(cfg.Hooks.AfterCreate), "hooks.after_create")
	w(1, "before_run", renderScript(cfg.Hooks.BeforeRun), "hooks.before_run")
	w(1, "after_run", renderScript(cfg.Hooks.AfterRun), "hooks.after_run")
	w(1, "before_remove", renderScript(cfg.Hooks.BeforeRemove), "hooks.before_remove")
	w(1, "timeout_ms", fmt.Sprint(cfg.Hooks.TimeoutMS), "hooks.timeout_ms")

	b.WriteString("agent:\n")
	w(1, "kind", cfg.Agent.Kind, "agent.kind")
	writeProvider(&b, def, red, 1, "agent.provider", cfg.Agent.Provider)

	// Printed in the clear, and deliberately not routed through the redactor:
	// every field here is a name — a kind, a child variable, and the variable
	// holding the credential — and the schema is what guarantees it, since
	// `publish.value` accepts exactly one `$VAR` reference and refuses a literal
	// (SPEC §5.2.8). Hiding the reference would conceal exactly what an operator
	// needs to read when the publish credential is misconfigured, which is the
	// call §10.2 already makes for `env_passthrough` names.
	b.WriteString("publish:\n")
	w(1, "kind", orUnset(cfg.Publish.Kind), "publish.kind")
	w(1, "env", orUnset(cfg.Publish.Env), "publish.env")
	w(1, "value", orUnset(cfg.Publish.ValueReference()), "publish.value")
	w(1, "source", orUnset(cfg.Publish.Source), "publish.source")

	b.WriteString("limits:\n")
	w(1, "max_concurrent_agents", fmt.Sprint(cfg.Limits.MaxConcurrentAgents), "limits.max_concurrent_agents")
	w(1, "max_turns", fmt.Sprint(cfg.Limits.MaxTurns), "limits.max_turns")
	w(1, "max_attempts", fmt.Sprint(cfg.Limits.MaxAttempts), "limits.max_attempts")
	w(1, "max_retry_backoff_ms", fmt.Sprint(cfg.Limits.MaxRetryBackoffMS), "limits.max_retry_backoff_ms")
	cost := "(disabled)"
	if cfg.Limits.MaxCostUSD != nil {
		cost = fmt.Sprint(*cfg.Limits.MaxCostUSD)
	}
	w(1, "max_cost_usd", cost, "limits.max_cost_usd")
	w(1, "stall_timeout_ms", fmt.Sprint(cfg.Limits.StallTimeoutMS), "limits.stall_timeout_ms")
	w(1, "attempt_timeout_ms", fmt.Sprint(cfg.Limits.AttemptTimeoutMS), "limits.attempt_timeout_ms")
	w(1, "max_prompt_bytes", fmt.Sprint(cfg.Limits.MaxPromptBytes), "limits.max_prompt_bytes")

	// deployment: never redacted. `accepted_because` is operator prose and it is
	// the *record* §10.1 asks for — a record nobody can read is not a record.
	b.WriteString("deployment:\n")
	w(1, "mode", string(cfg.Deployment.Mode), "deployment.mode")
	w(1, "accepted_because", orUnset(cfg.Deployment.AcceptedBecause), "deployment.accepted_because")

	// substrate: never redacted either, and for a stronger reason than
	// deployment's. Every field is a name, a keyword or a number, the one
	// credential is named indirectly through `credential_sources`, and the base
	// URL is refused at load if it carries so much as userinfo — so there is
	// nothing here that could be a secret, and hiding an endpoint would conceal
	// exactly what an operator needs when the backend is unreachable.
	b.WriteString("substrate:\n")
	w(1, "kind", cfg.Substrate.Kind, "substrate.kind")
	if cfg.Substrate.Remote() {
		a := cfg.Substrate.Airlock
		b.WriteString("  airlock:\n")
		w(2, "base_url", a.BaseURL, "substrate.airlock.base_url")
		w(2, "profile", a.Profile, "substrate.airlock.profile")
		w(2, "auth_source", a.AuthSource, "substrate.airlock.auth_source")
		w(2, "tls_ca_file", orUnset(a.TLSCAFile), "substrate.airlock.tls_ca_file")
		w(2, "request_timeout_ms", fmt.Sprint(a.RequestTimeoutMS), "substrate.airlock.request_timeout_ms")
		w(2, "poll_timeout_ms", fmt.Sprint(a.PollTimeoutMS), "substrate.airlock.poll_timeout_ms")
		w(2, "poll_wait_ms", fmt.Sprint(a.PollWaitMS), "substrate.airlock.poll_wait_ms")
		w(2, "settle_timeout_ms", fmt.Sprint(a.SettleTimeoutMS), "substrate.airlock.settle_timeout_ms")
		w(2, "max_retries", fmt.Sprint(a.MaxRetries), "substrate.airlock.max_retries")
		w(2, "idle_suspend_ms", renderWindow(a.IdleSuspendMS), "substrate.airlock.idle_suspend_ms")
		w(2, "delete_after_idle_ms", renderWindow(a.DeleteAfterIdleMS), "substrate.airlock.delete_after_idle_ms")
		w(2, "on_success", a.OnSuccess, "substrate.airlock.on_success")
		w(2, "on_failure", a.OnFailure, "substrate.airlock.on_failure")
		w(2, "on_revoked", a.OnRevoked, "substrate.airlock.on_revoked")
		w(2, "on_shutdown", a.OnShutdown, "substrate.airlock.on_shutdown")
	}

	// review: never redacted, for substrate's reason — three logins, a label, an
	// argv, some numbers, and a credential named indirectly. The logins in
	// particular are the thing an operator most needs to read back: a value that
	// is close but not the API login makes every author check fail silently, and
	// this rendering is where that is caught (docs/REVIEW.md).
	b.WriteString("review:\n")
	w(1, "enabled", fmt.Sprint(cfg.Review.Enabled), "review.enabled")
	if cfg.Review.Enabled {
		r := cfg.Review
		w(1, "principal", r.Principal, "review.principal")
		w(1, "tracker_author", r.TrackerAuthor, "review.tracker_author")
		w(1, "controller", r.Controller, "review.controller")
		w(1, "allow_shared_tracker_controller", fmt.Sprint(r.AllowSharedTrackerController), "review.allow_shared_tracker_controller")
		w(1, "auth_source", r.AuthSource, "review.auth_source")
		w(1, "api_base_url", orUnset(r.APIBaseURL), "review.api_base_url")
		w(1, "queue_label", r.QueueLabel, "review.queue_label")
		w(1, "add_human_review_label", fmt.Sprint(r.AddHumanReviewLabel), "review.add_human_review_label")
		w(1, "round_cap", fmt.Sprint(r.RoundCap), "review.round_cap")
		if len(r.ReviewerProfiles) == 0 {
			w(1, "reviewer_argv", renderList(r.ReviewerArgv), "review.reviewer_argv")
		} else {
			w(1, "reviewer_default_profile", r.ReviewerDefaultProfile, "review.reviewer_default_profile")
			b.WriteString("  reviewer_profiles:\n")
			names := make([]string, 0, len(r.ReviewerProfiles))
			for name := range r.ReviewerProfiles {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				w(2, name, renderList(r.ReviewerProfiles[name]), "review.reviewer_profiles."+name)
			}
		}
		w(1, "reviewer_env", renderList(r.ReviewerEnv), "review.reviewer_env")
		w(1, "guidance_file", orUnset(r.GuidanceFile), "review.guidance_file")
		w(1, "interval_ms", fmt.Sprint(r.IntervalMS), "review.interval_ms")
		w(1, "timeout_ms", fmt.Sprint(r.TimeoutMS), "review.timeout_ms")
		w(1, "request_timeout_ms", fmt.Sprint(r.RequestTimeoutMS), "review.request_timeout_ms")
		w(1, "max_diff_bytes", fmt.Sprint(r.MaxDiffBytes), "review.max_diff_bytes")
	}

	return b.String()
}

// reviewJSON is the same view for the machine-readable output. Rendered whether
// or not the controller is enabled, so a consumer reads one shape either way.
func reviewJSON(cfg Config) map[string]any {
	out := map[string]any{"enabled": cfg.Review.Enabled, "controller": nil}
	if !cfg.Review.Enabled {
		return out
	}
	r := cfg.Review
	out["controller"] = map[string]any{
		"principal":                       r.Principal,
		"tracker_author":                  r.TrackerAuthor,
		"controller":                      r.Controller,
		"allow_shared_tracker_controller": r.AllowSharedTrackerController,
		"auth_source":                     r.AuthSource,
		"api_base_url":                    r.APIBaseURL,
		"queue_label":                     r.QueueLabel,
		"add_human_review_label":          r.AddHumanReviewLabel,
		"round_cap":                       r.RoundCap,
		"reviewer_argv":                   r.ReviewerArgv,
		"reviewer_profiles":               r.ReviewerProfiles,
		"reviewer_default_profile":        r.ReviewerDefaultProfile,
		"reviewer_env":                    r.ReviewerEnv,
		"guidance_file":                   r.GuidanceFile,
		"interval_ms":                     r.IntervalMS,
		"timeout_ms":                      r.TimeoutMS,
		"request_timeout_ms":              r.RequestTimeoutMS,
		"max_diff_bytes":                  r.MaxDiffBytes,
	}
	return out
}

// renderWindow spells an idle window, and says what zero means rather than
// printing a number that reads as "immediately".
func renderWindow(ms int) string {
	if ms == 0 {
		return "(profile default)"
	}
	return fmt.Sprint(ms)
}

// substrateJSON is the same view for the machine-readable output. Rendered
// whether or not the substrate is remote, and with an explicit null for an
// unconfigured backend, so a consumer reads one shape either way.
func substrateJSON(cfg Config) map[string]any {
	out := map[string]any{"kind": cfg.Substrate.Kind, "airlock": nil}
	if !cfg.Substrate.Remote() {
		return out
	}
	a := cfg.Substrate.Airlock
	out["airlock"] = map[string]any{
		"base_url":             a.BaseURL,
		"profile":              a.Profile,
		"auth_source":          a.AuthSource,
		"tls_ca_file":          a.TLSCAFile,
		"request_timeout_ms":   a.RequestTimeoutMS,
		"poll_timeout_ms":      a.PollTimeoutMS,
		"poll_wait_ms":         a.PollWaitMS,
		"settle_timeout_ms":    a.SettleTimeoutMS,
		"max_retries":          a.MaxRetries,
		"idle_suspend_ms":      a.IdleSuspendMS,
		"delete_after_idle_ms": a.DeleteAfterIdleMS,
		"on_success":           a.OnSuccess,
		"on_failure":           a.OnFailure,
		"on_revoked":           a.OnRevoked,
		"on_shutdown":          a.OnShutdown,
	}
	return out
}

// EffectiveJSON renders the same view as JSON: redacted config values plus a
// provenance map keyed by dotted field path.
func EffectiveJSON(def *WorkflowDefinition) ([]byte, error) {
	red := newRedactor(def)
	cfg := def.Config
	provOut := map[string]map[string]any{}
	for path, origin := range def.Provenance {
		entry := map[string]any{"source": string(origin.Source)}
		if len(origin.EnvVars) > 0 {
			// A list, because a value may interpolate several variables and
			// each contributed a secret to it (FieldOrigin.EnvVars).
			entry["env_vars"] = origin.EnvVars
		}
		provOut[path] = entry
	}
	doc := map[string]any{
		"workflow":     def.Path,
		"workflow_key": def.Key,
		"config": map[string]any{
			"version": cfg.Version,
			"tracker": map[string]any{
				"kind":            cfg.Tracker.Kind,
				"provider":        red.block("tracker.provider", cfg.Tracker.Provider),
				"required_labels": cfg.Tracker.RequiredLabels,
				"active_states":   cfg.Tracker.ActiveStates,
				"terminal_states": cfg.Tracker.TerminalStates,
			},
			"polling": map[string]any{"interval_ms": cfg.Polling.IntervalMS},
			"workspace": map[string]any{
				"root":        red.value("workspace.root", cfg.Workspace.Root),
				"base_branch": nullableString(cfg.Workspace.BaseBranch),
			},
			"hooks": map[string]any{
				"after_create":  cfg.Hooks.AfterCreate,
				"before_run":    cfg.Hooks.BeforeRun,
				"after_run":     cfg.Hooks.AfterRun,
				"before_remove": cfg.Hooks.BeforeRemove,
				"timeout_ms":    cfg.Hooks.TimeoutMS,
			},
			"agent": map[string]any{
				"kind":     cfg.Agent.Kind,
				"provider": red.block("agent.provider", cfg.Agent.Provider),
			},
			// Names, not secrets — see EffectiveText. Empty strings rather than
			// null for an absent block, so a consumer reads three fields of one
			// shape whether or not a publish credential is configured.
			"publish": map[string]any{
				"kind":   cfg.Publish.Kind,
				"env":    cfg.Publish.Env,
				"value":  cfg.Publish.ValueReference(),
				"source": cfg.Publish.Source,
			},
			// In full, unredacted: a source block holds no secret (SPEC §5.8,
			// amendment 5). An empty object rather than null for an absent
			// section, so a consumer reads one shape either way.
			"credential_sources": sourcesJSON(cfg.CredentialSources),
			"deployment": map[string]any{
				"mode":             string(cfg.Deployment.Mode),
				"accepted_because": cfg.Deployment.AcceptedBecause,
			},
			// Names, keywords and numbers only — see EffectiveText.
			"substrate": substrateJSON(cfg),
			"review":    reviewJSON(cfg),
			"limits": map[string]any{
				"max_concurrent_agents": cfg.Limits.MaxConcurrentAgents,
				"max_turns":             cfg.Limits.MaxTurns,
				"max_attempts":          cfg.Limits.MaxAttempts,
				"max_retry_backoff_ms":  cfg.Limits.MaxRetryBackoffMS,
				"max_cost_usd":          cfg.Limits.MaxCostUSD,
				"stall_timeout_ms":      cfg.Limits.StallTimeoutMS,
				"attempt_timeout_ms":    cfg.Limits.AttemptTimeoutMS,
				"max_prompt_bytes":      cfg.Limits.MaxPromptBytes,
			},
		},
		"provenance": provOut,
	}
	return json.MarshalIndent(doc, "", "  ")
}

// redactor is the single decision point for what `config effective` hides, and
// the only one: EffectiveText, EffectiveJSON and workspace.root all route
// through it, so a renderer cannot acquire a redaction rule of its own — which
// is what makes `ben status` (SPEC §10.3) correct when it arrives rather than
// correct if somebody remembers.
//
// Two signals, because there are two questions (see core.SensitiveFields), and
// they are a union rather than a replacement: sensitivity hides what provenance
// cannot see is a credential, and provenance still hides an env-resolved value
// under a key no adapter declared. Dropping either widens what is printed.
type redactor struct {
	prov Provenance
	// sensitive holds joined provenance paths an adapter called secret. Joined
	// here, once, with the same helper the renderers use, so a key needing
	// quotes matches in the set exactly as it does in the output.
	sensitive map[string]bool
}

// newRedactor resolves both signals for one rendering. Kinds are asked through
// internal/registry, the one table the loader and `config effective` share
// (#55); an unknown kind contributes nothing, since validate has already refused
// that file.
func newRedactor(def *WorkflowDefinition) redactor {
	r := redactor{prov: def.Provenance, sensitive: map[string]bool{}}
	if kind, ok := registry.Tracker(def.Config.Tracker.Kind); ok {
		r.mark("tracker.provider", kind.SensitiveFields(def.Config.Tracker.Provider))
	}
	if kind, ok := registry.Runner(def.Config.Agent.Kind); ok {
		r.mark("agent.provider", kind.SensitiveFields(def.Config.Agent.Provider))
	}
	return r
}

func (r redactor) mark(prefix string, fields [][]string) {
	for _, segments := range fields {
		path := prefix
		for _, seg := range segments {
			path = appendProvenanceMapKey(path, seg)
		}
		r.sensitive[path] = true
	}
}

// AgentDescriptor names which agent this workflow runs, for the attempt-outcome
// record (#60): the `agent.kind` an operator wrote, and the model its adapter
// says the provider block selects.
//
// It lives here, and not in the orchestrator or the assembly, because this is
// the one place the three things it needs already meet: the resolved
// configuration, the kind registry (#55's single table), and the provenance that
// decides what may be printed. The loop never sees a provider block (SPEC §3.6),
// and the assembly has no business learning an adapter's key spellings.
//
// **The model goes through the same redactor as `config effective`.** A workflow
// may legitimately write `model: $MODEL`, and once it does, provenance is all
// that separates a model name from a secret — so the rule that governs there
// governs here, or `ben status` acquires a redaction policy of its own and is
// correct only while somebody remembers it (see redactor). The cost is a
// `[redacted]` in the aggregate for an env-parameterized model; the other
// direction publishes to whatever an operator pastes into an issue.
//
// An unknown kind yields the kind name and no model. That is unreachable for a
// definition validate has accepted, and is stated rather than assumed because
// definitions are also built by hand in tests.
func AgentDescriptor(def *WorkflowDefinition) core.AgentDescriptor {
	d := core.AgentDescriptor{Kind: def.Config.Agent.Kind}
	kind, ok := registry.Runner(d.Kind)
	if !ok {
		return d
	}
	model, field := kind.Model(def.Config.Agent.Provider)
	if model == "" {
		// No model named, so there is no value to decide about. Returning early
		// also keeps an empty model from rendering as `[redacted]` under a
		// sensitive-path declaration, which would report a secret that is not
		// there.
		return d
	}
	path := "agent.provider"
	for _, seg := range field {
		path = appendProvenanceMapKey(path, seg)
	}
	if newRedactor(def).hides(path) {
		d.Model = Redacted
		return d
	}
	d.Model = model
	return d
}

// hides reports whether the value at path must not be printed.
func (r redactor) hides(path string) bool {
	if r.sensitive[path] {
		return true
	}
	origin, ok := r.prov[path]
	return ok && origin.Source == SourceEnv
}

// block deep-copies a provider block, replacing what must not be printed.
func (r redactor) block(pathPrefix string, block map[string]any) map[string]any {
	out := make(map[string]any, len(block))
	for k, v := range block {
		out[k] = r.value(appendProvenanceMapKey(pathPrefix, k), v)
	}
	return out
}

func (r redactor) value(path string, v any) any {
	// Checked before descending: a sensitive path naming a map or a list is
	// redacted whole. Descending first would print its leaves one by one, each
	// of them individually undeclared.
	if r.hides(path) {
		return Redacted
	}
	switch val := v.(type) {
	case map[string]any:
		return r.block(path, val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = r.value(appendProvenanceIndex(path, i), item)
		}
		return out
	default:
		return v
	}
}

func writeProvider(b *strings.Builder, def *WorkflowDefinition, red redactor, indent int, pathPrefix string, block map[string]any) {
	pad := strings.Repeat("  ", indent)
	if len(block) == 0 {
		fmt.Fprintf(b, "%sprovider: {}\n", pad)
		return
	}
	fmt.Fprintf(b, "%sprovider:\n", pad)
	writeProviderEntries(b, def, red, indent+1, pathPrefix, block)
}

func writeProviderEntries(b *strings.Builder, def *WorkflowDefinition, red redactor, indent int, pathPrefix string, block map[string]any) {
	keys := make([]string, 0, len(block))
	for k := range block {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pad := strings.Repeat("  ", indent)
	for _, k := range keys {
		path := appendProvenanceMapKey(pathPrefix, k)
		// The redaction test precedes the descent, or a sensitive nested map
		// would be walked and printed key by key — each leaf undeclared on its
		// own, and the subtree its parent named never hidden.
		if nested, ok := block[k].(map[string]any); ok && !red.hides(path) {
			fmt.Fprintf(b, "%s%s:\n", pad, k)
			writeProviderEntries(b, def, red, indent+1, path, nested)
			continue
		}
		value := fmt.Sprint(red.value(path, block[k]))
		line := fmt.Sprintf("%s%s: %s", pad, k, value)
		fmt.Fprintf(b, "%-52s (%s)\n", line, originLabel(def.Provenance, path))
	}
}

// writeSources renders the `credential_sources` section, entries and keys each
// in sorted order so two runs of `config effective` over one file are diffable.
func writeSources(b *strings.Builder, def *WorkflowDefinition, sources map[string]SourceConfig) {
	if len(sources) == 0 {
		fmt.Fprintf(b, "%-52s (%s)\n", "credential_sources: {}", originLabel(def.Provenance, "credential_sources"))
		return
	}
	b.WriteString("credential_sources:\n")
	names := make([]string, 0, len(sources))
	for name := range sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(b, "  %s:\n", name)
		block := sources[name].Block
		keys := make([]string, 0, len(block))
		for k := range block {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			line := fmt.Sprintf("    %s: %v", k, block[k])
			fmt.Fprintf(b, "%-52s (%s)\n", line, originLabel(def.Provenance, appendProvenanceMapKey(sourcePath(name), k)))
		}
	}
}

func sourcesJSON(sources map[string]SourceConfig) map[string]any {
	out := make(map[string]any, len(sources))
	for name, src := range sources {
		out[name] = src.Block
	}
	return out
}

func originLabel(prov Provenance, path string) string {
	origin, ok := prov[path]
	if !ok {
		return string(SourceFile)
	}
	if origin.Source == SourceEnv {
		return "env " + origin.EnvVarLabel()
	}
	return string(origin.Source)
}

func renderList(items []string) string {
	if items == nil {
		return "(unset)"
	}
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

// orUnset renders an omitted string field the way renderList and renderScript
// render theirs, so an absent `publish` block reads as three unset fields rather
// than three blank lines.
func orUnset(s string) string {
	if s == "" {
		return "(unset)"
	}
	return s
}

func renderScript(script string) string {
	if script == "" {
		return "(unset)"
	}
	lines := strings.Count(strings.TrimRight(script, "\n"), "\n") + 1
	if lines == 1 {
		return fmt.Sprintf("%q", strings.TrimSpace(script))
	}
	return fmt.Sprintf("(%d-line script)", lines)
}
