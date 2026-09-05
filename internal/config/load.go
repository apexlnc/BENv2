package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
	"github.com/srhg-ai-7cef3f93/ben/internal/template"
)

// Load reads, parses, resolves, and validates the WORKFLOW.md at path
// (SPEC §5). It is strict at load: any error here means the definition is
// unusable — startup refuses, reload keeps last-known-good.
//
// Every refusal it returns names the file. That is done here, once, rather than
// at each return inside load: there are a dozen of them, several return errors
// built elsewhere, and a review already found one that had been missed
// (`rejectExplicitNulls`). Wrapping per site is the enumeration mistake this
// repo has paid for twice (#47, #52) — the fix is to read the whole surface at
// one boundary, and this is the boundary, because it is the only layer that
// knows the path at all.
func Load(path string) (*WorkflowDefinition, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolving workflow path: %w", err)
	}
	def, err := load(absPath)
	if err != nil {
		return nil, withPath(err, absPath)
	}
	return def, nil
}

// load is Load's body, split out so the wrapper above can name the file on every
// refusal without a dozen call sites having to remember to.
func load(absPath string) (*WorkflowDefinition, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrMissingWorkflowFile, absPath)
		}
		return nil, fmt.Errorf("reading workflow file: %w", err)
	}

	frontMatter, body, bodyLine, err := splitFrontMatter(string(data))
	if err != nil {
		return nil, err
	}
	if body == "" {
		return nil, ErrEmptyPrompt
	}

	// Pass 1: lenient decode — is it a map, and which version does it claim?
	// The version check runs before strict key validation so a future-version
	// file fails on version, not on its unknown keys (SPEC §5.2.1).
	var anyDoc any
	if err := yaml.Unmarshal([]byte(frontMatter), &anyDoc); err != nil {
		return nil, fmt.Errorf("WORKFLOW.md front matter: %w", err)
	}
	if _, ok := anyDoc.(map[string]any); anyDoc != nil && !ok {
		return nil, ErrFrontMatterNotMap
	}
	var versionProbe struct {
		Version *yamlInt `yaml:"version"`
	}
	// A failed probe is discarded rather than reported, and the version check is
	// then skipped rather than made on what the failure left behind. The probe
	// has one field and ignores unknown keys, so its only possible failure is
	// `version` itself — and yaml.v3 allocates the pointer *before* handing the
	// node to the decoder, so a refused value leaves Version non-nil at 0. Trusting
	// that is how `version: "1"` and `version: one` both refused with "declares
	// config version 0 … upgrade ben to use this file": a fabricated version, and
	// an instruction to replace the binary over a quoted or mistyped value. Falling
	// through hands the same key to the strict pass, which decodes it into the same
	// type and states the actual defect.
	if err := yaml.Unmarshal([]byte(frontMatter), &versionProbe); err == nil {
		if versionProbe.Version != nil && versionProbe.Version.Int() != SupportedVersion {
			return nil, &UnsupportedVersionError{Version: versionProbe.Version.Int()}
		}
	}
	// The null-value shape checks also run on the lenient document: the strict
	// pass decodes an explicit null to the field's zero value, indistinguishable
	// from an absent key, so it would silently bypass the refusals below.
	if doc, ok := anyDoc.(map[string]any); ok {
		if err := rejectExplicitNulls(doc); err != nil {
			return nil, err
		}
	}

	// Pass 2: strict decode. Unknown keys fail everywhere except the two
	// opaque provider blocks, which are typed as open maps (SPEC §5.3).
	// An empty front matter block yields io.EOF from Decode; treat it as an
	// empty config and let validation report what's missing.
	var raw rawConfig
	dec := yaml.NewDecoder(strings.NewReader(frontMatter))
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil && !errors.Is(err, io.EOF) {
		// A numeric-spelling refusal already reads as a validation error and
		// carries its own line; it is only missing the dotted path, which nothing
		// below a scalar's decoder can see (number.go). Everything else is a YAML
		// error about the front matter and is named as one.
		if num, ok := locateNumeric(err, frontMatter); ok {
			return nil, num
		}
		return nil, fmt.Errorf("WORKFLOW.md front matter: %w", err)
	}

	// Through KeyFor, not workflowKey: `ben status` derives the same key from a
	// path alone (SPEC §10.3), and two call sites deriving it separately is a
	// daemon writing to one state directory while the reader looks in another.
	key, err := KeyFor(absPath)
	if err != nil {
		return nil, err
	}
	def := &WorkflowDefinition{
		Path:           absPath,
		Key:            key,
		PromptTemplate: body,
		Provenance:     Provenance{},
	}
	if err := resolve(&raw, def); err != nil {
		return nil, err
	}
	if err := validate(&def.Config); err != nil {
		return nil, err
	}
	// After validate, so both kinds are known to exist, and against the
	// provenance resolve recorded: a legacy credential's identity is where its
	// value came from, not the site that spelled it (SPEC §5.5, §10.2).
	if err := resolveCredentials(&def.Config, def.Provenance); err != nil {
		return nil, err
	}
	// The §10.2 split is checked over variable identities, never over the
	// secrets themselves (SPEC §10.2, §6.7). First, because it reads **every
	// route into an agent process** — the provider block's leaves, the §7.6
	// allowlist, `env_passthrough` — and can therefore name the one an operator
	// has to edit. The authority rule beside it is the general statement and
	// catches what a variable cannot express: two `octo_sts` sources sharing a
	// trust-policy identity, or a named source colliding with a legacy spelling.
	if err := checkCredentialSplit(&def.Config, def.Provenance); err != nil {
		return nil, err
	}
	if err := checkCredentialRules(&def.Config); err != nil {
		return nil, err
	}

	// Template strictness is a load concern like everything above: unknown
	// variables and filters refuse here, not at first render (SPEC §5.6,
	// §5.7). The compiled prompt rides along for dispatch.
	prompt, err := template.Load(body, absPath, bodyLine)
	if err != nil {
		return nil, err
	}
	def.prompt = prompt
	return def, nil
}

// resolve applies defaults, $VAR indirection, and path normalization, and
// records per-field provenance.
func resolve(raw *rawConfig, def *WorkflowDefinition) error {
	cfg := &def.Config
	prov := def.Provenance

	setInt := func(path string, dst *int, src *yamlInt, def int) {
		if src != nil {
			*dst, prov[path] = src.Int(), FieldOrigin{Source: SourceFile}
		} else {
			*dst, prov[path] = def, FieldOrigin{Source: SourceDefault}
		}
	}
	setStr := func(path string, dst *string, src *string) {
		if src != nil {
			*dst, prov[path] = *src, FieldOrigin{Source: SourceFile}
		} else {
			prov[path] = FieldOrigin{Source: SourceDefault}
		}
	}

	setInt("version", &cfg.Version, raw.Version, SupportedVersion)

	// tracker
	tr := raw.Tracker
	if tr == nil {
		tr = &rawTracker{}
	}
	setStr("tracker.kind", &cfg.Tracker.Kind, tr.Kind)
	cfg.Tracker.Provider = tr.Provider
	if cfg.Tracker.Provider == nil {
		cfg.Tracker.Provider = map[string]any{}
	}
	if err := resolveProviderEnv(cfg.Tracker.Provider, "tracker.provider", prov); err != nil {
		return err
	}
	cfg.Tracker.RequiredLabels = trimAll(tr.RequiredLabels)
	if tr.RequiredLabels != nil {
		prov["tracker.required_labels"] = FieldOrigin{Source: SourceFile}
	} else {
		prov["tracker.required_labels"] = FieldOrigin{Source: SourceDefault}
	}
	cfg.Tracker.ActiveStates = trimAll(tr.ActiveStates)
	cfg.Tracker.TerminalStates = trimAll(tr.TerminalStates)
	prov["tracker.active_states"] = originOr(tr.ActiveStates != nil, SourceAdapter)
	prov["tracker.terminal_states"] = originOr(tr.TerminalStates != nil, SourceAdapter)
	// Promoted out of the opaque block (SPEC §8.4, amendment 10). Read
	// tolerantly: a non-string value here is the adapter's Structural to refuse,
	// and reading it as absent would only mean the reduced block still carries
	// it — which the reduction test would catch.
	cfg.Tracker.ClaimAssignee, _ = providerText(cfg.Tracker.Provider, TrackerClaimAssigneeKey)

	// credential_sources: names are the operator's, keys beneath them belong to
	// each entry's kind. Deliberately not $VAR-resolved (see SourceConfig).
	if err := resolveSources(raw.CredentialSources, cfg, prov); err != nil {
		return err
	}

	// polling
	pl := raw.Polling
	if pl == nil {
		pl = &rawPolling{}
	}
	setInt("polling.interval_ms", &cfg.Polling.IntervalMS, pl.IntervalMS, DefaultPollingIntervalMS)

	// workspace: default from XDG, then ~ expansion, $VAR indirection,
	// relative-to-workflow-dir resolution, absolute normalization (§5.2.4).
	ws := raw.Workspace
	if ws == nil {
		ws = &rawWorkspace{}
	}
	if ws.Root != nil {
		root, envVars, err := resolveEnvString(*ws.Root, "workspace.root")
		if err != nil {
			return err
		}
		// An empty root is not a path (SPEC §5.2.4): left alone it would fall
		// into the relative-path rule and aim the workspace tree — including
		// the startup sweep — at the directory holding WORKFLOW.md itself.
		if strings.TrimSpace(root) == "" {
			msg := "must not be empty or whitespace-only"
			if len(envVars) > 0 {
				msg = fmt.Sprintf("%s resolved to an empty or whitespace-only path",
					FieldOrigin{EnvVars: envVars}.EnvVarLabel())
			}
			return &ValidationError{Field: "workspace.root", Msg: msg}
		}
		if len(envVars) > 0 {
			prov["workspace.root"] = FieldOrigin{Source: SourceEnv, EnvVars: envVars}
		} else {
			prov["workspace.root"] = FieldOrigin{Source: SourceFile}
		}
		root, err = expandHome(root)
		if err != nil {
			return err
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(filepath.Dir(def.Path), root)
		}
		cfg.Workspace.Root = filepath.Clean(root)
	} else {
		root, err := defaultWorkspaceRoot()
		if err != nil {
			return err
		}
		if !filepath.IsAbs(root) {
			root = filepath.Join(filepath.Dir(def.Path), root)
		}
		cfg.Workspace.Root = filepath.Clean(root)
		prov["workspace.root"] = FieldOrigin{Source: SourceDefault}
	}
	if ws.BaseBranch != nil {
		if err := validateBaseBranch(*ws.BaseBranch); err != nil {
			return &ValidationError{Field: "workspace.base_branch", Msg: err.Error()}
		}
	}
	setStr("workspace.base_branch", &cfg.Workspace.BaseBranch, ws.BaseBranch)

	// hooks: scripts are shell text — $VAR indirection deliberately does not
	// apply; runtime expansion belongs to the shell (§5.5).
	hk := raw.Hooks
	if hk == nil {
		hk = &rawHooks{}
	}
	setStr("hooks.after_create", &cfg.Hooks.AfterCreate, hk.AfterCreate)
	setStr("hooks.before_run", &cfg.Hooks.BeforeRun, hk.BeforeRun)
	setStr("hooks.after_run", &cfg.Hooks.AfterRun, hk.AfterRun)
	setStr("hooks.before_remove", &cfg.Hooks.BeforeRemove, hk.BeforeRemove)
	setInt("hooks.timeout_ms", &cfg.Hooks.TimeoutMS, hk.TimeoutMS, DefaultHooksTimeoutMS)

	// agent
	ag := raw.Agent
	if ag == nil {
		ag = &rawAgent{}
	}
	setStr("agent.kind", &cfg.Agent.Kind, ag.Kind)
	cfg.Agent.Provider = ag.Provider
	if cfg.Agent.Provider == nil {
		cfg.Agent.Provider = map[string]any{}
	}
	if err := resolveProviderEnv(cfg.Agent.Provider, "agent.provider", prov); err != nil {
		return err
	}
	cfg.Agent.providerEnvSources = providerEnvSources(prov, "agent.provider")

	// publish: three plain strings, and `value` is deliberately not resolved.
	// Reading the variable here would refuse the file on every host that does not
	// hold the publish secret — including the CI that load-validates this repo's
	// own WORKFLOW.md — so absence is a readiness failure, never a load one
	// (SPEC §5.2.8, §5.5). Provenance therefore says "file" for a field holding a
	// variable *name*: nothing was read from the environment, and the §10.2 check
	// reads the parsed name from the config rather than from provenance.
	pb := raw.Publish
	if pb == nil {
		pb = &rawPublish{}
	}
	setStr("publish.kind", &cfg.Publish.Kind, pb.Kind)
	setStr("publish.env", &cfg.Publish.Env, pb.Env)
	setStr("publish.source", &cfg.Publish.Source, pb.Source)
	cfg.Publish.Source = strings.TrimSpace(cfg.Publish.Source)
	if pb.Value != nil {
		name, err := publishValueVar(*pb.Value)
		if err != nil {
			return err
		}
		cfg.Publish.ValueVar, prov["publish.value"] = name, FieldOrigin{Source: SourceFile}
	} else {
		prov["publish.value"] = FieldOrigin{Source: SourceDefault}
	}
	// A written key must state a kind, and this is the only place that can say
	// so: `publish` is OPTIONAL, so validation infers absence from an empty
	// PublishConfig — and every all-zero *written* block is indistinguishable
	// from an omitted one by then. `{}`, `{kind: ""}`, `{env: ""}`, and any
	// combination of them would all load and silently inject no credential, which
	// is the ambient-`HOME` fallback the section exists to replace (SPEC §5.2.8).
	//
	// Stated as presence ⇒ kind rather than as a list of empty shapes, because
	// enumerating shapes is what let `{kind: ""}` through: `kind` is the
	// discriminator every other field hangs off, so requiring it is the whole rule.
	// A resolve-time refusal for the same reason workspace.root's is — the fact it
	// rests on (was the key written?) exists here and nowhere later.
	if raw.Publish != nil && cfg.Publish.Kind == "" {
		return publishKindRequired()
	}

	// deployment: two plain strings, neither $VAR-resolved. A mode is a closed
	// keyword and a reason is prose; neither is a place a secret belongs, and
	// resolving them would make `config effective` print one.
	dp := raw.Deployment
	if dp == nil {
		dp = &rawDeployment{}
	}
	var mode string
	setStr("deployment.mode", &mode, dp.Mode)
	cfg.Deployment.Mode = DeploymentMode(mode)
	setStr("deployment.accepted_because", &cfg.Deployment.AcceptedBecause, dp.AcceptedBecause)

	// limits
	lm := raw.Limits
	if lm == nil {
		lm = &rawLimits{}
	}
	setInt("limits.max_concurrent_agents", &cfg.Limits.MaxConcurrentAgents, lm.MaxConcurrentAgents, DefaultMaxConcurrentAgents)
	setInt("limits.max_turns", &cfg.Limits.MaxTurns, lm.MaxTurns, DefaultMaxTurns)
	setInt("limits.max_attempts", &cfg.Limits.MaxAttempts, lm.MaxAttempts, DefaultMaxAttempts)
	setInt("limits.max_retry_backoff_ms", &cfg.Limits.MaxRetryBackoffMS, lm.MaxRetryBackoffMS, DefaultMaxRetryBackoffMS)
	setInt("limits.stall_timeout_ms", &cfg.Limits.StallTimeoutMS, lm.StallTimeoutMS, DefaultStallTimeoutMS)
	setInt("limits.attempt_timeout_ms", &cfg.Limits.AttemptTimeoutMS, lm.AttemptTimeoutMS, DefaultAttemptTimeoutMS)
	setInt("limits.max_prompt_bytes", &cfg.Limits.MaxPromptBytes, lm.MaxPromptBytes, DefaultMaxPromptBytes)
	cfg.Limits.MaxCostUSD = lm.MaxCostUSD.Float()
	prov["limits.max_cost_usd"] = originOr(lm.MaxCostUSD != nil, SourceDefault)

	// substrate: a closed key set this package owns end to end, and the one
	// section whose *written-ness* decides a refusal — see resolveSubstrate.
	if err := resolveSubstrate(raw.Substrate, cfg, prov); err != nil {
		return err
	}
	// review: the #204 controller. After the tracker, because its required label
	// defaults to the one that already dispatches this daemon.
	return resolveReview(raw.Review, cfg, prov)
}

// rejectExplicitNulls refuses explicitly-null values whose strict decoding
// collapses into "absent" and would silently select a default:
//
//   - credential_sources — SPEC §5.2.10 types it as a map; `{}` (or omitting
//     the key) is the deliberate spelling for no named sources, while null
//     would silently compile the legacy credential spellings instead.
//   - provider blocks — SPEC §5.2.2 and §5.2.5 type `provider` as an object;
//     `{}` (or omitting the key) is the deliberate spelling for an empty
//     block, null is a shape error like any other non-map value.
//   - workspace.root — SPEC §5.2.4 types it as a path; a written key with no
//     value is not a request for the default root.
//   - workspace.base_branch — omission selects the repository default, while
//     null is not a branch name and must not silently select that fallback.
//   - publish — SPEC §5.2.8 types it as an object, and "absent" here means
//     something: no publish credential is injected, so the agent publishes with
//     whatever it can find under HOME. A written key with no value would select
//     that silently, which is a capability loss that looks like a passing load.
//     Only the null shape is caught here, because it is the one that leaves no
//     trace by the strict pass — `publish:` decodes to a nil *rawPublish, exactly
//     as an omitted key does. Every other written-but-empty spelling (`{}`,
//     `{kind: ""}`) yields a non-nil pointer, so resolve sees the key was written
//     and requires a kind.
func rejectExplicitNulls(doc map[string]any) error {
	if v, present := doc["credential_sources"]; present && v == nil {
		return &ValidationError{Field: "credential_sources", Msg: "must be a map; write {} for an empty source set or omit the key"}
	}
	for _, section := range []string{"tracker", "agent"} {
		block, ok := doc[section].(map[string]any)
		if !ok {
			continue // non-map sections are the strict pass's problem
		}
		if v, present := block["provider"]; present && v == nil {
			return &ValidationError{Field: section + ".provider", Msg: "must be a map; write {} for an empty provider block or omit the key"}
		}
	}
	if ws, ok := doc["workspace"].(map[string]any); ok {
		if v, present := ws["root"]; present && v == nil {
			return &ValidationError{Field: "workspace.root", Msg: "must be a path; omit the key to use the default root"}
		}
		if v, present := ws["base_branch"]; present && v == nil {
			return &ValidationError{Field: "workspace.base_branch", Msg: "must be a branch name; omit the key to use the repository default"}
		}
	}
	if v, present := doc["publish"]; present {
		if v == nil {
			return &ValidationError{Field: "publish", Msg: "must be a map; omit the key entirely to inject no publish credential"}
		}
	}
	// substrate — the same reasoning as `publish`, one step sharper: absence
	// here means "run attempts on this host", and a written-but-null key would
	// select that silently. `substrate.airlock` is caught too, because a null
	// there decodes to a nil pointer and is indistinguishable from an omitted
	// key by the strict pass — which is exactly the distinction the
	// mixed-configuration refusal rests on.
	if sb, present := doc["substrate"]; present {
		if sb == nil {
			return &ValidationError{Field: "substrate", Msg: "must be a map; omit the key entirely to run attempts on this host"}
		}
		if block, ok := sb.(map[string]any); ok {
			if v, written := block["airlock"]; written && v == nil {
				return &ValidationError{Field: "substrate.airlock", Msg: "must be a map; omit the key entirely under substrate.kind local"}
			}
		}
	}
	return nil
}

// withPath names the file a refusal came from, once, at Load's boundary.
//
// Load is the only layer that knows the path — validate takes a Config,
// deliberately, so it can be called on one nobody read off disk — and a per-site
// wrapper is one somebody forgets: two review rounds found two sites I had
// missed, which is the signal that the sites are the wrong unit.
//
// Two kinds of refusal already name the file themselves and are left alone
// rather than double-named: a missing file, whose message *is* the path, and a
// **located** template error, which carries file *and line* and is therefore
// strictly better than what this would add.
//
// The template side is asked through template.Located rather than by listing
// error types. Listing them is what this function got wrong twice — first by
// annotating only *ValidationError, then by skipping only UnknownVariableError
// while four of its siblings carry a location too. An interface makes a new
// located error opt in where it is defined, next to its File and Line fields,
// instead of in a list over here that nobody edits.
//
// Both are recognised by type, never by looking for the path in the text: a
// message that happens to contain the path is not the same fact as an error that
func withPath(err error, path string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrMissingWorkflowFile) || hasUsableLocation(err) {
		return err
	}
	return &WorkflowError{Path: path, Err: err}
}

// hasUsableLocation reports whether an error already says where it is.
//
// It asks for the *values*, not only the interface. An error that implements
// Located and reports an empty file — a type that grew the method before it grew
// the fields, or one that builds the location conditionally — would otherwise be
// skipped here and end up the only refusal naming no file at all, which is worse
// than the double-naming the skip exists to prevent. Failing that way round is
// the point: a doubtful location gets wrapped.
func hasUsableLocation(err error) bool {
	var located template.Located
	if !errors.As(err, &located) {
		return false
	}
	file, line := located.Location()
	return file != "" && line > 0
}

// validate enforces the core value rules (SPEC §5.7). Adapter-owned provider
// blocks are validated by their adapters, not here.
//
// The supported kind sets are asked of internal/registry rather than listed
// here: the loader and `ben config effective` must agree about which names have
// an adapter, or a config accepted at load names a kind the CLI cannot find
// (#55).
func validate(cfg *Config) error {
	if cfg.Tracker.Kind == "" {
		return &ValidationError{Field: "tracker.kind", Msg: "required"}
	}
	if _, ok := registry.Tracker(cfg.Tracker.Kind); !ok {
		return &ValidationError{Field: "tracker.kind", Msg: fmt.Sprintf("%q is not a supported tracker (supported: %s)", cfg.Tracker.Kind, strings.Join(registry.TrackerNames(), ", "))}
	}
	if cfg.Agent.Kind == "" {
		return &ValidationError{Field: "agent.kind", Msg: "required"}
	}
	if _, ok := registry.Runner(cfg.Agent.Kind); !ok {
		return &ValidationError{Field: "agent.kind", Msg: fmt.Sprintf("%q is not a supported agent (supported: %s)", cfg.Agent.Kind, strings.Join(registry.RunnerNames(), ", "))}
	}
	for _, l := range cfg.Tracker.RequiredLabels {
		if l == "" {
			return &ValidationError{Field: "tracker.required_labels", Msg: "blank label entries are not allowed (a blank label matches no issue)"}
		}
	}
	if cfg.Workspace.BaseBranch != "" {
		if err := validateBaseBranch(cfg.Workspace.BaseBranch); err != nil {
			return &ValidationError{Field: "workspace.base_branch", Msg: err.Error()}
		}
	}
	positives := []struct {
		field string
		v     int
	}{
		{"polling.interval_ms", cfg.Polling.IntervalMS},
		{"hooks.timeout_ms", cfg.Hooks.TimeoutMS},
		{"limits.max_concurrent_agents", cfg.Limits.MaxConcurrentAgents},
		{"limits.max_turns", cfg.Limits.MaxTurns},
		{"limits.max_attempts", cfg.Limits.MaxAttempts},
		{"limits.max_retry_backoff_ms", cfg.Limits.MaxRetryBackoffMS},
		{"limits.stall_timeout_ms", cfg.Limits.StallTimeoutMS},
		{"limits.attempt_timeout_ms", cfg.Limits.AttemptTimeoutMS},
		// Positive-only, so no configured value can disable the ceiling
		// (SPEC §5.6): a template.Limits with a negative MaxPromptBytes renders
		// unbounded, and that spelling stays unreachable from a workflow file.
		{"limits.max_prompt_bytes", cfg.Limits.MaxPromptBytes},
	}
	for _, p := range positives {
		if p.v <= 0 {
			return &ValidationError{Field: p.field, Msg: "must be a positive integer"}
		}
	}
	if v := cfg.Limits.MaxCostUSD; v != nil {
		// Finiteness first, as its own refusal rather than folded into the
		// positivity rule below: **every** comparison against NaN is false, so
		// `<= 0` does not refuse it, `> 0` does not accept it, and a cap written
		// as `.nan` was recorded with provenance `file`, printed by
		// `config effective` as though set, and then inert at both consumers'
		// gates — a budget ceiling switched off by a value that looks like one.
		// `.inf` is the mirror image: it passes every gate and reaches the child
		// argv as `+Inf`.
		//
		// Load refuses both spellings a layer earlier (yamlFloat), and this is
		// still the rule rather than a restatement of it: validate takes a Config
		// deliberately, so it can be called on one nobody read off disk, and the
		// value rule has to hold there too.
		if math.IsNaN(*v) || math.IsInf(*v, 0) {
			return &ValidationError{Field: "limits.max_cost_usd", Msg: "must be a finite number"}
		}
		if *v <= 0 {
			return &ValidationError{Field: "limits.max_cost_usd", Msg: "must be positive when set (omit to disable the budget cap)"}
		}
	}
	if err := validatePublish(cfg.Publish); err != nil {
		return err
	}
	if err := validateDeployment(cfg.Deployment); err != nil {
		return err
	}
	// After the credential sources are resolved into cfg.CredentialSources, so
	// `auth_source` can be validated against the entries that actually exist.
	if err := validateSubstrate(cfg); err != nil {
		return err
	}
	return validateReview(cfg)
}

func trimAll(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimSpace(s)
	}
	return out
}

func originOr(fromFile bool, fallback Source) FieldOrigin {
	if fromFile {
		return FieldOrigin{Source: SourceFile}
	}
	return FieldOrigin{Source: fallback}
}

func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expanding ~ in workspace.root: %w", err)
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}

func defaultWorkspaceRoot() (string, error) {
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		return filepath.Join(xdg, "ben"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("computing default workspace.root: %w", err)
	}
	return filepath.Join(home, ".local", "share", "ben"), nil
}
