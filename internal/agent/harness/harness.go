// Package harness is the machinery every process-per-attempt agent adapter
// needs and none of them should own privately (SPEC §7.1–§7.6): the child
// process lifecycle, liveness windows, the signal ladder, transcript retention,
// child-environment composition, and the provider-block parsing helpers.
//
// SPEC §7.7 fixes v1 at two harnesses — claude-code and codex-exec — and their
// differences are narrow: which argv to build, which environment variables
// carry the credential, and how to read one line of the harness's stream. That
// is what an adapter implements. Everything below the boundary is the same
// obligation for both, and a second private copy of it would be a second place
// for a stall window to be enforced slightly differently.
//
// So the split is: an adapter decides *what to run and what a line means*; this
// package decides *how a run behaves*. Four obligations live here, none of them
// obvious from the AgentRunner interface:
//
//   - Liveness is runner-owned (SPEC §7.4). Both the stall window and the hard
//     attempt timeout are enforced on a goroutine of their own, so a consumer
//     that stops draining events cannot postpone them — and the terminal event,
//     not the exit code, decides the outcome.
//   - The stream must be drained before it is judged (SPEC §7.5). The output
//     pipes are this package's own rather than cmd.StdoutPipe's, because Wait
//     closes those the instant the process exits and would discard the result
//     line that the outcome depends on.
//   - The adapter owns the whole child environment (SPEC §7.6), composed here
//     from an allowlist plus what the provider block names. The daemon's
//     environment is never inherited wholesale — not even by the readiness
//     probes — and the orchestrator contributes only `BEN_`-prefixed variables.
//   - Stop must be honest (SPEC §7.5, §9.8). SIGTERM to the process group,
//     grace, SIGKILL, driven by the *group's* disappearance rather than the
//     leader's; if anything survives, the termination is reported unconfirmed
//     so the orchestrator keeps the claim.
package harness

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Sizes, as opposed to windows: the lifecycle timings live in Timings.
const (
	// maxScanLine is the line-scanner ceiling (SPEC §7.5 recommends 10 MiB):
	// one assistant message with a large tool result is far past the 64 KiB
	// bufio default, and a truncated line would look like a dead stream.
	maxScanLine = 10 << 20
	// stderrTail bounds the retained stderr used to explain a launch failure.
	stderrTail = 8 << 10
	// eventBuffer keeps a slow consumer from backpressuring the harness read
	// loop for the common burst of progress lines.
	eventBuffer = 64
)

// Environ composes the complete child environment (SPEC §7.6). The adapter owns
// all of it: allowlisted daemon passthrough, the operator's explicit
// env_passthrough names, provider.env, and the adapter's own auth surface
// (injected). The orchestrator contributes only its BEN_-prefixed variables,
// which by construction cannot collide with any of the above.
//
// There is no precedence rule here on purpose. Layering the per-run map last —
// "the orchestrator's word is final" — is the natural implementation and the
// wrong one: it undoes the binding that lets Ready and Start agree, since Ready
// would verify one credential and Start would run with another. A RunSpec that
// reaches for a non-BEN_ key is refused outright (CheckSpec), never merged.
//
// The daemon's environment is never inherited wholesale, so a stray API key or
// GH_TOKEN in the daemon's own environment cannot leak into an agent run that
// did not ask for it. The allowlist that decides which names do cross lives in
// core (core.EnvAllowlist): workspace hooks (SPEC §6.5) run against the same
// boundary and must not drift from it.
// It also returns the resolved env_passthrough pairs, which are the part of the
// redaction set no declaration can supply: SPEC §7.6 describes env_passthrough as
// the opt-in by which "a tracker PAT or an agent API key" reaches a child, so it
// is where a real credential is most likely to enter, and SensitiveFields
// deliberately reports only the *names* (an operator debugging a bad passthrough
// has to be able to read those). Returned from here rather than looked up again,
// so the set describes the environment the child actually got: a second
// os.LookupEnv would be a second snapshot, and a value that changed between them
// would be in the child and not in the set.
//
// Only the passthrough pairs. Not the allowlist, whose values are the daemon's own
// PATH and HOME, and not `injected`, which mixes a credential with a filesystem
// path by design (codex-exec's CODEX_HOME) — redacting either would damage the
// record retention exists for rather than protect anything.
// The publish credential rides along as a resolved pair rather than being read
// here, so the mechanism that produces it can change — #117 boundary 2 mints an
// installation token instead of reading a variable — without moving the injection
// or the redaction. What must not change is that one value reaches both: the
// environment the child gets and the set kept out of its transcript are built
// from the same string, in one place.
func Environ(passthrough []string, providerEnv, injected map[string]string, publish PublishValue, spec core.RunSpec) (out []string, undeclared map[string]string) {
	env := map[string]string{}
	for _, name := range core.EnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
		}
	}
	resolved := map[string]string{}
	for _, name := range passthrough {
		if v, ok := os.LookupEnv(name); ok {
			env[name] = v
			resolved[name] = v
		}
	}
	maps.Copy(env, providerEnv)
	for k, v := range injected {
		if v != "" {
			env[k] = v
		}
	}
	// Last of the config-derived layers, so no provider surface can override the
	// publish credential. §7.6's reservation already makes that collision
	// unexpressible — it is refused at load — and this is the same
	// refusal-not-precedence argument pointed at what remains: if the two ever
	// disagreed about the variable, an ordering that let the block win would be
	// the wrong answer, because the block's value is the one nothing validated.
	if publish.set() {
		env[publish.Env] = publish.Value
	}
	maps.Copy(env, spec.Env)

	// After composition, not during it: a name in both `env_passthrough` and `env`
	// takes the block's value, and the host value never reaches the child. Reporting
	// it would put a value the child never got into the redaction set — and, since
	// CheckRedactableEnv holds these to a shape requirement, would refuse a run over
	// a host value the block had already overridden. The block's own value is
	// covered as a layer-2 value regardless, so nothing is lost by dropping it here.
	//
	// Keyed by the **site** that put each value there, not by the variable name:
	// two sites report here now, and CheckRedactableEnv names its refusal from
	// this key — an operator told to fix `env_passthrough.GH_TOKEN` when the value
	// came from `publish.value` would be looking at the wrong line.
	undeclared = map[string]string{}
	for name, v := range resolved {
		if env[name] == v {
			undeclared["env_passthrough."+name] = v
		}
	}
	if publish.set() {
		undeclared["publish.value"] = publish.Value
	}

	out = make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	// Deterministic for tests and for the audit log.
	sort.Strings(out)
	return out, undeclared
}

// PublishValue is the publish credential resolved for one attempt: the child
// variable and the value to inject under it (SPEC §5.2.8). The zero value means
// no publish credential is configured, which is not an error — the block is
// OPTIONAL, and an agent may authenticate from what §7.6's allowlist carries.
type PublishValue struct {
	Env   string
	Value string
}

func (p PublishValue) set() bool { return p.Env != "" && p.Value != "" }

// MintPublish obtains the publish credential for one attempt (SPEC §5.2.8,
// §7.7).
//
// Per attempt, not at construction, because §5.5 defers the read on purpose: a
// workflow file has to load on a host that holds no publish secret, since that
// is the host CI validates on. So the daemon learns whether the credential
// exists at Ready — one loud startup refusal — and again at Start, where the
// cost is one attempt rather than a whole ticket's work discovering it at
// `git push`. Both adapters call this from both places; **the difference in
// consequence lives at the call sites, not in this function.**
//
// It is deliberately the fresh-only surface (core.FreshSource): a token handed
// to an agent must cover the whole attempt, and a cached one has already spent
// part of its life against a gate computed from `now`.
//
// Two classifications belong here rather than to a source, because a source
// reports what the exchange did and cannot know what the caller needs:
//
//   - **An empty value** is a permanent refusal, raised **before the agent is
//     launched**. A source that returns success with no credential is a source
//     defect, and no consumer may discover that by running an agent that then
//     cannot push.
//   - **TTL insufficiency** — a deadline shorter than the configured attempt
//     plus the fixed margin — is permanent too. It is arithmetic, not weather,
//     and this is the only thing holding both the token and the attempt timeout.
//
// Skipped when the source states no deadline (core.SourceDescriptor.Bounded),
// which is what keeps `static` and every legacy spelling valid.
func MintPublish(ctx context.Context, b core.PublishBinding, attemptTimeout time.Duration, sentinel error) (PublishValue, error) {
	if b.Env == "" && b.Source == nil {
		return PublishValue{}, nil
	}
	if !b.Configured() {
		return PublishValue{}, fmt.Errorf("%w: half-configured publish credential (env %q, source present: %t); "+
			"both are required together (SPEC §5.2.8)", sentinel, b.Env, b.Source != nil)
	}
	d := b.Source.Descriptor()
	tok, err := b.Source.FetchFresh(ctx, core.PurposePublish)
	if err != nil {
		// The class rides through untouched: this function classifies the two
		// things only it can see, and re-labelling a source's own verdict is how
		// a transient blip becomes a park.
		return PublishValue{}, fmt.Errorf("%w: %w", sentinel, err)
	}
	if tok.Value == "" {
		return PublishValue{}, fmt.Errorf("%w: %w", sentinel, &core.CredentialError{
			Class:     core.CredentialPermanent,
			Authority: d.Authority,
			Err:       core.ErrCredentialEmpty,
		})
	}
	if d.Bounded() {
		left := time.Until(tok.UsableUntil)
		// Subtract the fixed margin from the finite token lifetime instead of
		// adding it to an operator-controlled timeout: a representable timeout
		// near MaxDuration would otherwise wrap negative and pass this gate.
		if left < core.CredentialTTLMargin || attemptTimeout > left-core.CredentialTTLMargin {
			return PublishValue{}, fmt.Errorf("%w: %w", sentinel, &core.CredentialError{
				Class:     core.CredentialPermanent,
				Authority: d.Authority,
				Err: fmt.Errorf("%w: %s of credential life remains; limits.attempt_timeout_ms %s "+
					"plus the fixed %s margin does not fit",
					core.ErrCredentialTTL, left.Round(time.Second), attemptTimeout, core.CredentialTTLMargin),
			})
		}
	}
	return PublishValue{Env: b.Env, Value: tok.Value}, nil
}

// ForwardedEnvVars is the `env_passthrough` half of core.RunnerKind's
// ForwardedEnvVars (SPEC §10.2, §7.6), shared because both adapters spell that
// surface the same way.
//
// Only this route. Every other secret a block carries into a child does so as a
// resolved *value* — argv, the child environment, a settings file — and the
// loader already knows which variable each value came from, so it reads its own
// provenance rather than trusting an adapter to enumerate keys. That distinction
// is the fix for a real hole: a list of "credential keys" missed
// `model: $TRACKER_PAT`, which reaches the child as `--model <secret>`.
//
// Deliberately tolerant of a malformed block, and deliberately not routed
// through an adapter's ParseProvider. Load does not run Structural — the assembly
// does (SPEC §5.7, §5.8) — so this is asked of blocks nobody has validated yet,
// and a parse that gave up on an unrelated type error would report *no* forwarded
// variables for a block that plainly forwards some.
func ForwardedEnvVars(block map[string]any) []string {
	var out []string
	for _, name := range blockStrings(block["env_passthrough"]) {
		// An env_passthrough entry forwards the daemon's variable of that name
		// into the child, so the entry *is* the source variable — no provenance
		// lookup, and none possible: §5.5 resolves the entry's own $VAR to a
		// name, which is what lands here.
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// CredentialKey pairs a named credential provider key with the child
// environment variable an adapter injects it as (SPEC §7.6, §7.7).
//
// It exists so an adapter states each credential once. Two lists derive from the
// table — OwnedEnv, which bars the generic environment surfaces from respelling
// the variable (§7.6), and SensitiveFields, which keeps the value out of
// `config effective` (§5.8) — and a credential added to one and forgotten in the
// other is either a bypassable reservation or a printed secret. Dropping an
// entry now drops both at once, which is what makes the loss visible: the
// reservation has tests of its own.
type CredentialKey struct {
	// ProviderKey is the agent.provider key, e.g. "api_key".
	ProviderKey string
	// Env is the child environment variable it is injected as.
	Env string
}

// ReservedEnv pairs a child environment variable with the config site that owns
// it — the site where its value is validated, and therefore the only site an
// operator may set it through (SPEC §7.6).
//
// The owner travels with the name because there is more than one owner. The
// refusal used to say "set it through its named `agent.provider` key, which is
// where it is validated", which is false advice for the variable `publish.env`
// names: that one belongs to the publish block, and an operator who followed the
// message would move the credential into the surface the rule just refused.
type ReservedEnv struct {
	// Name is the child environment variable.
	Name string
	// Owner is the owning config site as an operator writes it —
	// "agent.provider.api_key", "publish.env".
	Owner string
}

// OwnedEnv is the environment half of a credential table, for CheckOwnedEnv.
func OwnedEnv(keys []CredentialKey) []ReservedEnv {
	out := make([]ReservedEnv, 0, len(keys))
	for _, k := range keys {
		out = append(out, ReservedEnv{Name: k.Env, Owner: "agent.provider." + k.ProviderKey})
	}
	return out
}

// PublishReference projects a runtime binding back onto the reference shape the
// §7.6 rules are written over (SPEC §5.2.8).
//
// The child variable is the whole of what those rules need — CheckPublishEnv and
// CheckOwnedEnv both ask "who owns this name" — and it is the only half a
// constructed runner still has. The source behind it is deliberately dropped:
// re-checking a *value* at construction is what §5.5 defers, and a rule stated
// over a credential nobody has yet would be a rule that cannot run in CI.
func PublishReference(b core.PublishBinding) core.PublishCredential {
	return core.PublishCredential{Env: b.Env}
}

// PublishReserved is the publish credential's entry in the same table, or none
// when the workflow configures no publish credential (SPEC §5.2.8).
//
// One table, asked in two directions: CheckOwnedEnv refuses a provider surface
// that respells a reserved variable, and CheckPublishEnv refuses a `publish.env`
// that names one somebody else owns. Both are the same defect — two config sites
// writing one child variable, so that whichever wins, the other is silently doing
// nothing — and deriving both from one list is what keeps them from disagreeing
// about which variables are reserved.
func PublishReserved(cred core.PublishCredential) []ReservedEnv {
	if cred.Env == "" {
		return nil
	}
	return []ReservedEnv{{Name: cred.Env, Owner: "publish.env"}}
}

// reservedOwner reports the owning site of name, and whether it is reserved.
func reservedOwner(reserved []ReservedEnv, name string) (string, bool) {
	for _, r := range reserved {
		if r.Name == name {
			return r.Owner, true
		}
	}
	return "", false
}

// ModelKey is the provider key both v1 adapters select a model with. Stated
// once, because both spell it the same way and a private copy each is a second
// place for `--model` and the attempt record to disagree about which key was
// read.
const ModelKey = "model"

// Model is the shared core.RunnerKind implementation (#60): the model this
// block names, and the path it was read from.
//
// Read straight off the map rather than through ParseProvider, for the reason
// ForwardedEnvVars is: this is asked of blocks nothing has validated, and an
// unrelated type error elsewhere in the block must not turn a stated model into
// a missing one. A non-string value at the key is the block's own problem —
// Structural refuses it at load — and reports as no model here rather than as a
// rendering of whatever type it is.
func Model(block map[string]any) (string, []string) {
	path := []string{ModelKey}
	s, _ := block[ModelKey].(string)
	return s, path
}

// SensitiveFields is the shared core.RunnerKind implementation (SPEC §5.8):
// the adapter's own named credential keys, plus every `env` entry.
//
// All of `env`, not a chosen subset of it, because `env` is where an operator
// puts a credential the adapter has no named key for — and an allowlist inside it
// would be the enumeration mistake #47 already paid for once. The cost is an
// unredacted `env: {SOME_FLAG: true}` rendering as `[redacted]`, which loses an
// operator a boolean; the other direction loses them a credential. The publish
// credential is no longer among them (SPEC §5.2.8) and is not covered here: it is
// not in the block, so its display and its redaction are the loader's and
// Environ's respectively.
//
// `env_passthrough` is deliberately absent. Its entries are variable *names*,
// not secrets, and hiding them would conceal exactly what an operator needs to
// read when a passthrough is misconfigured.
func SensitiveFields(block map[string]any, credentials []CredentialKey) [][]string {
	fields := make([][]string, 0, len(credentials)+len(block))
	for _, k := range credentials {
		fields = append(fields, []string{k.ProviderKey})
	}
	if env, ok := block["env"].(map[string]any); ok {
		for _, name := range slices.Sorted(maps.Keys(env)) {
			fields = append(fields, []string{"env", name})
		}
	}
	return fields
}

// CredentialValues returns the secret *values* in a provider block: the string
// leaves at every path SensitiveFields names.
//
// A projection of that one declaration rather than a list of its own, so a key
// added to a credential table is hidden from `config effective` (SPEC §5.8) and
// from retained transcripts (SPEC §10.3) in the same edit. The alternative —
// reading values from the adapter's injected environment map — cannot work: that
// map mixes credentials with non-credentials by design, and scrubbing
// CODEX_HOME's path out of every transcript is not a redaction, it is damage.
//
// An absent path contributes nothing: a credential key with no value has no
// secret. A path naming a subtree contributes each string inside it, since any
// of them could be the secret. Malformed blocks are tolerated, as in #47: Load
// never runs Structural, so this is asked of blocks nobody has validated.
func CredentialValues(block map[string]any, credentials []CredentialKey) []string {
	var out []string
	eachCredentialValue(block, credentials, func(_ []string, v string) {
		out = append(out, v)
	})
	return out
}

// eachCredentialValue visits every secret value in a block with the path it came
// from, in SensitiveFields order. CheckRedactable needs the path to name a field
// in its refusal, and reading both from one walk is what keeps the check and the
// redaction set over the same values.
func eachCredentialValue(block map[string]any, credentials []CredentialKey, fn func(path []string, value string)) {
	for _, path := range SensitiveFields(block, credentials) {
		for _, v := range stringLeaves(valueAt(block, path)) {
			fn(path, v)
		}
	}
}

// valueAt walks a path of map keys, returning nil if any segment is missing or
// is not a map.
func valueAt(block map[string]any, path []string) any {
	var cur any = block
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[seg]; !ok {
			return nil
		}
	}
	return cur
}

// stringLeaves collects every string inside an unvalidated block leaf, in a
// deterministic order.
func stringLeaves(v any) []string {
	switch t := v.(type) {
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	case map[string]any:
		var out []string
		for _, k := range slices.Sorted(maps.Keys(t)) {
			out = append(out, stringLeaves(t[k])...)
		}
		return out
	case []any:
		var out []string
		for _, item := range t {
			out = append(out, stringLeaves(item)...)
		}
		return out
	default:
		// Numbers and booleans are not credentials, and their rendered spelling
		// is short enough that replacing it would corrupt the record.
		return nil
	}
}

// blockStrings reads a string list out of an unvalidated block leaf.
func blockStrings(v any) []string {
	switch list := v.(type) {
	case []string:
		return list
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// SpecErrors names an adapter's refusals for the checks CheckSpec makes. Each
// adapter keeps its own sentinels (AGENTS.md conventions: tests assert on
// them); only the reasoning is shared.
type SpecErrors struct {
	EnvNamespace  error
	PromptEmpty   error
	WorkspacePath error
}

// CheckSpec refuses a RunSpec no adapter can honor, before spending a process
// on it. Start returning an error means no handle at all, which the
// orchestrator records as launch_error — non-retryable, since a rerun with the
// same inputs fails identically (SPEC §7.3).
func CheckSpec(spec core.RunSpec, errs SpecErrors) error {
	// The orchestrator's half of the reserved namespace (SPEC §7.6). This is a
	// refusal, not a precedence question: if the two sides ever disagree about
	// a variable, one of them is confused about who owns it, and no ordering
	// makes that safe. Note what a narrower "may not override provider-derived
	// keys" rule would let through — the adapter's credential variable when the
	// block omits it (nothing was derived, so nothing was protected), and HOME,
	// which arrives via the allowlist yet redirects keychain lookup.
	for _, name := range slices.Sorted(maps.Keys(spec.Env)) {
		if !strings.HasPrefix(name, core.EnvPrefix) {
			return fmt.Errorf("%w: RunSpec.Env may carry only %s-prefixed keys, got %q",
				errs.EnvNamespace, core.EnvPrefix, name)
		}
	}
	if spec.Prompt == "" {
		return errs.PromptEmpty
	}
	if !filepath.IsAbs(spec.Workspace.Path) {
		return fmt.Errorf("%w, got %q", errs.WorkspacePath, spec.Workspace.Path)
	}
	info, err := os.Stat(spec.Workspace.Path)
	if err != nil {
		return fmt.Errorf("%w: %v", errs.WorkspacePath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %q is not a directory", errs.WorkspacePath, spec.Workspace.Path)
	}
	return nil
}

// CheckProviderEnv is the config half of the BEN_ reservation (SPEC §7.6).
//
// It is the half that matters more: a collision authored here is written once
// and hits every run, whereas the RunSpec half is per attempt. Both environment
// surfaces are covered, because a namespace enforced on one of them is not a
// namespace — and this is a property of the file, so it refuses at load rather
// than at dispatch.
func CheckProviderEnv(providerEnv map[string]string, passthrough []string, sentinel error) error {
	for _, name := range slices.Sorted(maps.Keys(providerEnv)) {
		if strings.HasPrefix(name, core.EnvPrefix) {
			return fmt.Errorf("%w: env.%s uses the %s prefix, which is reserved to the orchestrator",
				sentinel, name, core.EnvPrefix)
		}
	}
	// A passthrough entry is a $VAR-resolvable string leaf, unlike an `env` key,
	// so its value goes out as data and the renderer decides (ValueRefusal).
	for i, name := range passthrough {
		if strings.HasPrefix(name, core.EnvPrefix) {
			return ValueRefusalIndex("env_passthrough", i, name,
				fmt.Errorf("%w: env_passthrough entry uses the %s prefix, which is reserved to the orchestrator",
					sentinel, core.EnvPrefix))
		}
	}
	return nil
}

// CheckOwnedEnv refuses a provider block that respells a reserved child
// environment variable through the generic `env`/`env_passthrough` surfaces.
//
// It is the BEN_ reservation's argument pointed the other way (SPEC §7.6). A
// reserved variable has a *named* config site, which is where it is validated — a
// path checked for absoluteness, a credential the readiness probe knows about, a
// publish credential checked against every other owner. The generic map is a
// second spelling for the same variable with none of that: it reaches the child,
// and nothing sees it as configuration. So `env.<CREDENTIAL>` sails past the
// readiness rule that exists for it, and `env.<HOME>` past the path rule.
//
// Refusal rather than precedence, again. Layering the named field over the map
// would leave the map's value in force whenever the named field is *omitted* —
// exactly the case where nothing was validated — which is the same shape as the
// "may not override what the adapter derived" rule §7.6 rejects: if a field is
// omitted, the adapter derived nothing, so nothing was protected.
func CheckOwnedEnv(providerEnv map[string]string, passthrough []string, reserved []ReservedEnv, sentinel error) error {
	for _, name := range slices.Sorted(maps.Keys(providerEnv)) {
		if owner, ok := reservedOwner(reserved, name); ok {
			return fmt.Errorf("%w: env.%s is owned by %s, which is where it is validated; "+
				"a second spelling here reaches the child with none of that validation", sentinel, name, owner)
		}
	}
	for i, name := range passthrough {
		if owner, ok := reservedOwner(reserved, name); ok {
			return ValueRefusalIndex("env_passthrough", i, name,
				fmt.Errorf("%w: that entry names a variable owned by %s, which is where it is validated; "+
					"forwarding it here reaches the child with none of that validation", sentinel, owner))
		}
	}
	return nil
}

// CheckPublishEnv refuses a publish credential whose child variable another site
// owns (SPEC §5.2.8, §7.6) — the second direction of the one-variable-one-owner
// rule, and the sharper one.
//
// `publish.env: ANTHROPIC_API_KEY` points the publish credential at the harness's
// own auth; the same spelling aimed at `CODEX_HOME` overwrites the directory a
// credential is resolved *from* with a token. Both are configurations in which
// one of two sites silently does nothing, and which one loses is decided by
// composition order rather than by anything an operator wrote.
//
// The core-owned half of the rule — the `BEN_` prefix, and §7.6's
// daemon-environment allowlist — is a load refusal in internal/config, because
// both of those sets are core's own. This is the half that needs the adapter's
// table, which is opaque to the loader.
//
// The refusal names the variable, which is a name and not a secret; the
// credential's *value* is not in this function's reach at all.
func CheckPublishEnv(cred core.PublishCredential, reserved []ReservedEnv, sentinel error) error {
	if cred.Env == "" {
		return nil
	}
	if owner, ok := reservedOwner(reserved, cred.Env); ok {
		return fmt.Errorf("%w: publish.env names %s, which is owned by %s — the publish credential "+
			"cannot be injected under a variable another site sets, because one of the two would "+
			"silently do nothing", sentinel, cred.Env, owner)
	}
	return nil
}

// ResolveBinary finds the harness executable and makes the answer absolute.
//
// Absolute is not cosmetic: Start runs with cwd set to the workspace, so a
// relative path — "./claude", or anything LookPath returns relative when PATH
// holds "." — would resolve against the *workspace* at exec time. That
// workspace is the agent's own writable tree, so a relative binding is an
// execution path the agent could supply itself, and it would not be the binary
// Ready verified.
func ResolveBinary(name string, sentinel error) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%w: %q not found on PATH: %v", sentinel, name, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("%w: %q could not be resolved to an absolute path: %v", sentinel, path, err)
	}
	return abs, nil
}

// Probe runs one readiness probe under ctx, honors that ctx, and leaves nothing
// behind. Its environment is the caller's to compose — the same restricted one
// a real attempt gets, so that readiness cannot pass by way of a daemon secret
// the run will not have (SPEC §7.6).
//
// Two separate hazards, both from a probe that spawns a child:
//
//   - CommandContext kills the probe when ctx is done, but the wait for its
//     output pipes is unbounded, and a child holding stdout keeps them open. So
//     WaitDelay bounds that wait — not to zero: the probe can return up to
//     Timings.ProbeWait after the context expires. Bounded, not instant, is the
//     claim.
//   - Returning is not the same as cleaning up. The child that held the pipe is
//     still running, and a daemon that probes on every reload would accumulate
//     them. The probe therefore gets its own process group and the group is
//     killed on the way out, so readiness costs no orphans.
func Probe(ctx context.Context, t Timings, path string, env []string, args ...string) ([]byte, error) {
	return probe(ctx, t, path, env, false, args)
}

// ProbeCombined is Probe with stderr interleaved into the returned bytes, for a
// harness that answers on stderr. Which stream a probe answers on is a measured
// property of the binary, not a convention: `codex login status` prints its
// whole answer — logged in or not — to stderr and leaves stdout empty, so a
// stdout-only read of it is indistinguishable from silence.
func ProbeCombined(ctx context.Context, t Timings, path string, env []string, args ...string) ([]byte, error) {
	return probe(ctx, t, path, env, true, args)
}

func probe(ctx context.Context, t Timings, path string, env []string, combined bool, args []string) ([]byte, error) {
	probe := exec.CommandContext(ctx, path, args...)
	probe.Env = env
	probe.WaitDelay = t.withDefaults().ProbeWait
	probe.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var out bytes.Buffer
	probe.Stdout = &out
	if combined {
		probe.Stderr = &out
	}
	if err := probe.Start(); err != nil {
		return nil, err
	}
	// Setpgid makes the child its own group leader, so its pid is the group id.
	pgid := probe.Process.Pid
	defer syscall.Kill(-pgid, syscall.SIGKILL)

	err := probe.Wait()
	return out.Bytes(), err
}
