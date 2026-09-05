package codexexec

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Provider is the parsed `agent.provider` block. The key set is closed: the
// core passes the block through opaquely (SPEC §5.2.5), so this adapter is the
// only thing standing between a typo and a run with the wrong sandbox.
//
// Values arrive with `$VAR` indirection already resolved by the config loader
// (SPEC §5.5), so a secret-bearing key here holds the secret itself — which is
// why none of them reach argv (SPEC §7.6, see command).
type Provider struct {
	// Binary is the harness executable; a bare name is resolved on PATH.
	Binary string
	// Model is an alias or full name; empty leaves the CLI default.
	Model string
	// SandboxMode is REQUIRED — see ErrSandboxMode.
	SandboxMode string
	// NetworkAccess grants the workspace-write sandbox network egress. Off by
	// default in the harness, and the agent publishes its own PR (SPEC §6.7),
	// so a workflow that expects `gh push` to work must turn it on.
	NetworkAccess bool
	// APIKey is injected as CODEX_API_KEY, this adapter's documented auth
	// surface (SPEC §7.6).
	APIKey string
	// CodexHome is injected as CODEX_HOME: the directory holding config.toml
	// and a stored login. An *absolute* path (checkCodexHome), never inline
	// content — the second route this harness has to a credential, and a
	// reference cannot carry one into argv.
	CodexHome string
	// AddDirs are the sandbox's writable roots beyond the workspace. Absolute
	// paths only (checkAddDirs). Empty is the norm — the workspace is the
	// boundary — and empty is still stated explicitly, so the harness's own
	// config file cannot add roots of its own (see sandboxOverrides).
	AddDirs []string
	// Env holds extra child environment entries. Not the publish credential:
	// that has a top-level block of its own, and this surface may not spell the
	// variable it names (SPEC §5.2.8, §7.6).
	Env map[string]string
	// EnvPassthrough names daemon environment variables to forward beyond the
	// default allowlist (proxies, CA bundles). Names only — an operator opting
	// in explicitly, never a wholesale inherit (SPEC §7.6).
	EnvPassthrough []string
}

// DefaultBinary is the harness executable when the provider block omits one.
const DefaultBinary = "codex"

// sandboxModes are `codex exec --sandbox`'s choices, split by whether a
// headless daemon that must publish can actually use them. Verified against
// codex-cli 0.147.0.
const (
	// sandboxWorkspaceWrite is the mode with an actual boundary to state; see
	// sandboxOverrides.
	sandboxWorkspaceWrite = "workspace-write"
	sandboxFullAccess     = "danger-full-access"
)

var (
	usableSandboxModes = []string{sandboxWorkspaceWrite, sandboxFullAccess}
	// read-only cannot modify files, so the agent can never reach the publish
	// step the prompt contract requires (SPEC §5.6) — the same reason the
	// claude-code adapter refuses `plan`.
	unusableSandboxModes = map[string]string{
		"read-only": "cannot modify files, so it can never publish",
	}
)

// providerKeys is the closed key set, for the unknown-key refusal and its
// error message.
//
// Two absences are deliberate:
//
//   - There is no generic `-c key=value` passthrough. The flag is how one sets
//     anything in config.toml, and it is exactly the wrong shape for BEN:
//     provider strings are $VAR-resolved (SPEC §5.5), so a generic escape hatch
//     is a route for a resolved secret into argv, which `ps` shows the world
//     (SPEC §7.6). The knobs a workflow needs are named below; anything else
//     belongs in the config.toml under `codex_home`, which is a file — a
//     reference, not content.
//   - There is no key carrying prompt text. The prompt body is BEN's posture
//     surface (SPEC §5.6), so behavioral instructions belong in the template,
//     where they are version-controlled and reviewed.
var providerKeys = []string{
	"add_dirs", "api_key", "binary", "codex_home", "env", "env_passthrough",
	"model", "network_access", "sandbox_mode",
}

var credentialKeys = []harness.CredentialKey{
	{ProviderKey: "api_key", Env: "CODEX_API_KEY"},
}

// ownedEnv is this adapter's environment surface: the variables it injects from
// named provider keys, and therefore the ones the generic env surfaces may not
// spell (see harness.CheckOwnedEnv). Derived from credentialKeys so the
// reservation and the display redaction cannot name different sets.
// CODEX_HOME is appended rather than tabled: it is an owned variable but not a
// credential — it is the directory a credential is resolved *from*, so the
// reservation covers it while display redaction has nothing to hide.
var ownedEnv = append(harness.OwnedEnv(credentialKeys),
	harness.ReservedEnv{Name: "CODEX_HOME", Owner: "agent.provider.codex_home"})

// ParseProvider validates the agent configuration and returns the block typed.
// Pure: no filesystem, no process, no network — `ben config effective` must be
// able to reject a malformed block without a working harness (SPEC §5.8).
//
// It takes the whole core.AgentConfig, not just the block, because the §7.6
// reservation spans both: the variable `publish.env` names may not be respelled
// in the block's generic environment surfaces, and may not itself name a variable
// this adapter owns. Neither half is answerable from one side alone (SPEC §5.7).
//
// This is the one parsing path. Structural and New both come through here, or New
// could construct a runner whose configuration Structural would have refused —
// which is precisely the publish checks, since they are the only ones the block
// alone cannot see.
func ParseProvider(cfg core.AgentConfig) (Provider, error) {
	block := cfg.Provider
	b := harness.NewBlock(block, ErrProviderValue)
	if err := b.Unknown(providerKeys, ErrProviderKey); err != nil {
		return Provider{}, err
	}

	var p Provider
	var err error
	if p.Binary, err = b.String("binary"); err != nil {
		return Provider{}, err
	}
	if p.Binary == "" {
		p.Binary = DefaultBinary
	}
	if p.Model, err = b.String("model"); err != nil {
		return Provider{}, err
	}
	if p.APIKey, err = b.String("api_key"); err != nil {
		return Provider{}, err
	}
	if p.CodexHome, err = b.String("codex_home"); err != nil {
		return Provider{}, err
	}
	if err := checkCodexHome(p.CodexHome); err != nil {
		return Provider{}, err
	}
	if p.NetworkAccess, err = b.Bool("network_access"); err != nil {
		return Provider{}, err
	}
	if p.AddDirs, err = b.Strings("add_dirs"); err != nil {
		return Provider{}, err
	}
	if err := checkAddDirs(p.AddDirs); err != nil {
		return Provider{}, err
	}
	if p.EnvPassthrough, err = b.Strings("env_passthrough"); err != nil {
		return Provider{}, err
	}
	if p.Env, err = b.StringMap("env"); err != nil {
		return Provider{}, err
	}
	if err := checkTempRoot(p.Env); err != nil {
		return Provider{}, err
	}

	// The config half of the BEN_ reservation (SPEC §7.6): a collision authored
	// here is written once and hits every run, so it refuses at load.
	if err := harness.CheckProviderEnv(p.Env, p.EnvPassthrough, ErrEnvNamespace); err != nil {
		return Provider{}, err
	}
	// One child variable, one owning config site (SPEC §7.6), asked in both
	// directions off one table: this adapter's own two have named keys precisely
	// so they can be validated (checkCodexHome, and the api_key branch in
	// checkAuth), and a second spelling reaches the child with neither check
	// applied. The publish credential joins them — the core names its variable
	// and this adapter injects it.
	reserved := append(slices.Clone(ownedEnv), harness.PublishReserved(cfg.Publish)...)
	if err := harness.CheckOwnedEnv(p.Env, p.EnvPassthrough, reserved, ErrEnvReserved); err != nil {
		return Provider{}, err
	}
	// The other direction, and the sharper one: `publish.env: CODEX_HOME` would
	// overwrite the directory this harness resolves its stored credential *from*
	// with a token.
	if err := harness.CheckPublishEnv(cfg.Publish, ownedEnv, ErrEnvReserved); err != nil {
		return Provider{}, err
	}
	// A credential the transcript writer could not keep out of the record refuses
	// here rather than leaking once per run (SPEC §10.3).
	if err := harness.CheckRedactable(block, credentialKeys, ErrProviderValue); err != nil {
		return Provider{}, err
	}

	mode, err := b.String("sandbox_mode")
	if err != nil {
		return Provider{}, err
	}
	if err := checkSandboxMode(mode); err != nil {
		return Provider{}, err
	}
	p.SandboxMode = mode

	return p, nil
}

// checkTempRoot makes the provider's alternate temp root an unambiguous path
// assembly can compare with daemon-only state. An empty or relative TMPDIR has
// process- and cwd-dependent fallback semantics, so it cannot participate in a
// fail-closed containment check.
func checkTempRoot(env map[string]string) error {
	tmp, ok := env["TMPDIR"]
	if !ok {
		return nil
	}
	if tmp != "" && filepath.IsAbs(tmp) {
		return nil
	}
	return harness.ValueRefusal("env.TMPDIR", tmp,
		fmt.Errorf("%w: env.TMPDIR must be a non-empty absolute path so daemon-only state can be kept outside it", ErrProviderValue))
}

// checkCodexHome refuses a relative CODEX_HOME, because the two things that
// must agree about it run from different directories.
//
// Readiness probes run from wherever the daemon was started; an attempt runs
// with cwd set to the workspace (SPEC §7.6). A relative path therefore names one
// directory when Ready verifies the credential and a different one — inside the
// agent's own writable tree — when Start uses it, which is exactly the
// divergence binding the configuration at New exists to remove (SPEC §7.1). It
// is the same defect as a relative harness binary, one layer over.
//
// Refusing rather than resolving: resolution would have to happen against the
// daemon's cwd, which is invisible in the config file and changes with how the
// daemon was launched. An absolute path means the same thing to everyone.
// The path travels as data for the same reason a mode does: it is a
// $VAR-resolvable provider string, so the renderer decides by provenance whether
// printing it is safe (SPEC §5.8).
func checkCodexHome(home string) error {
	if home == "" || filepath.IsAbs(home) {
		return nil
	}
	return harness.ValueRefusal("codex_home", home,
		fmt.Errorf("%w: codex_home must be an absolute path "+
			"(readiness runs from the daemon's directory and an attempt runs from the workspace, "+
			"so a relative path names two different homes)", ErrProviderValue))
}

// checkAddDirs refuses a writable root this adapter cannot state unambiguously.
//
// Absolute for the same reason as codex_home, and more sharply: these become
// sandbox writable roots, so a relative one would resolve against the workspace
// at exec time — an agent-controlled path granted write access on the agent's
// own say-so.
//
// The character rules are about the transport. The roots are passed as a TOML
// array in a `-c` override (see command), so a value containing a quote,
// a backslash, or a control character would need escaping this adapter would
// have to get exactly right; a writable root that cannot be spelled
// unambiguously is not one to guess at.
// Entries are anchored per index, matching the loader's indexed provenance paths,
// so a literal sibling in the same list cannot decide the redaction of an
// env-resolved one (SPEC §5.8).
func checkAddDirs(dirs []string) error {
	for i, d := range dirs {
		if !filepath.IsAbs(d) {
			return harness.ValueRefusalIndex("add_dirs", i, d,
				fmt.Errorf("%w: add_dirs entry must be an absolute path "+
					"(a relative writable root would resolve against the agent's own workspace)", ErrProviderValue))
		}
		if strings.ContainsAny(d, "\"\\") || strings.ContainsFunc(d, unicode.IsControl) {
			return harness.ValueRefusalIndex("add_dirs", i, d,
				fmt.Errorf("%w: add_dirs entry contains a quote, backslash, or control character, "+
					"which this adapter will not attempt to escape into a config override", ErrProviderValue))
		}
	}
	return nil
}

// checkSandboxMode refuses a missing or unusable sandbox posture. A refused mode
// travels as data (harness.ValueRefusal), never in the message: `sandbox_mode` is
// a $VAR-resolvable provider string like any other, and `ben config effective`
// prints these refusals in CI (SPEC §5.5, §5.8).
func checkSandboxMode(mode string) error {
	switch {
	case mode == "":
		// Nothing to redact, and no value to anchor: an absent key is a fact
		// about the file, not about a value.
		return fmt.Errorf("%w: required (one of %s)", ErrSandboxMode, strings.Join(usableSandboxModes, ", "))
	case slices.Contains(usableSandboxModes, mode):
		return nil
	default:
		if why, known := unusableSandboxModes[mode]; known {
			return harness.ValueRefusal("sandbox_mode", mode,
				fmt.Errorf("%w: that mode %s; use one of %s", ErrSandboxMode, why, strings.Join(usableSandboxModes, ", ")))
		}
		return harness.ValueRefusal("sandbox_mode", mode,
			fmt.Errorf("%w: unknown mode (one of %s)", ErrSandboxMode, strings.Join(usableSandboxModes, ", ")))
	}
}

// checkRuntime is what Ready asks of the world (SPEC §7.1): the binary exists,
// identifies as the Codex CLI, and has plausible credentials. Structure was
// already settled — purely — by Structural.
//
// The probes run with the same restricted environment as a real attempt
// (SPEC §7.6). That is not only hygiene — inheriting the daemon's environment
// would hand every daemon secret to an operator-configured binary — it is also
// what makes the credential probe meaningful: an environment that authenticates
// only because the daemon happened to have more in it would pass validation and
// then fail every run.
func (p Provider) checkRuntime(ctx context.Context, path string, publish harness.PublishValue, t harness.Timings) error {
	env, undeclared := p.environ(publish, core.RunSpec{})
	// A value the transcript writer could not cover refuses here, at
	// startup, rather than once per attempt (SPEC §7.1, §10.3).
	if err := harness.CheckRedactableEnv(undeclared, ErrProviderValue); err != nil {
		return err
	}

	out, err := harness.Probe(ctx, t, path, env, "--version")
	if err != nil {
		// Wrapped, not formatted: a probe refused for flooding its output
		// (harness.ErrProbeOutput) is a different fact about the binary from one
		// that exited non-zero, and the caller should be able to tell them apart.
		return fmt.Errorf("%w: %s --version failed: %w", ErrBinary, path, err)
	}
	// `codex-cli 0.147.0` — the marker, not the number: pinning a minimum
	// version would refuse harness upgrades we have no evidence to refuse.
	if !strings.Contains(string(out), versionMarker) {
		return fmt.Errorf("%w: %s --version does not identify as the Codex CLI: %q",
			ErrBinary, path, harness.Excerpt(strings.TrimSpace(string(out)), harness.ProbeExcerpt))
	}
	return p.checkAuth(ctx, path, env, t)
}

// versionMarker is what `codex --version` prints ahead of the number.
const versionMarker = "codex-cli"

// checkAuth is SPEC §7.1's "auth plausible", and for this harness it is also
// "the configuration loads at all". Both are measured facts about 0.147.0, and
// each is a trap for the obvious implementation:
//
//   - `codex login status` answers on **stderr** and writes nothing to stdout,
//     logged in or out. A stdout-only read of it is always empty, so a probe
//     that classifies from the body silently accepts every configuration.
//     Here the **exit status** is the answer — 0 stored login, non-zero not —
//     the opposite of the claude-code adapter's rule, because that harness
//     prints a complete body alongside a non-zero exit and this one prints no
//     body at all.
//   - `codex --version` succeeds with an unusable CODEX_HOME, warning and
//     exiting 0, so identity cannot catch a home that does not exist or is not
//     a directory. `login status` exits non-zero on it ("Error loading
//     configuration"), which is why this probe runs even when api_key makes the
//     stored login irrelevant: it is the only local check that the harness can
//     load its configuration, and a home it cannot load fails every run.
//
// The asymmetry is therefore narrow: an api_key excuses exactly one failure —
// the absent stored login — and nothing else. Everything else refuses, and so
// does a non-zero exit whose text this adapter does not recognize. That is the
// deliberate direction: a readiness check that fails closed costs one loud
// refusal at startup, and one that fails open costs a burned workspace per
// dispatch (SPEC §7.1). A future release rewording "Not logged in" would refuse
// an api_key-only configuration until this needle is updated; the reverse
// mistake is silent.
func (p Provider) checkAuth(ctx context.Context, path string, env []string, t harness.Timings) error {
	out, err := harness.ProbeCombined(ctx, t, path, env, "login", "status")
	if ctx.Err() != nil {
		// The binary took the whole validation window and said nothing, which
		// is its own kind of unusable.
		return fmt.Errorf("%w: %s login status did not answer: %v", ErrBinary, path, ctx.Err())
	}
	if errors.Is(err, harness.ErrProbeOutput) {
		// Before the excuse below, and not folded into it: that excuse reads the
		// body, and a body past the probe's bound is not the one-line "Not
		// logged in" it exists for, whatever the flood happens to contain
		// (#235). Failing closed here is the same direction as everything else
		// in this function.
		return fmt.Errorf("%w: %s login status: %w", ErrBinary, path, err)
	}
	if err == nil {
		return nil
	}
	answer := strings.TrimSpace(string(out))
	if p.APIKey != "" && strings.Contains(strings.ToLower(answer), "not logged in") {
		// The one excusable failure: no stored login, but a key that
		// authenticates runs (`login status` does not consult it — measured).
		return nil
	}
	// Quoting the probe's output is safe on this path and not on the other:
	// the failure text is a configuration diagnostic, while the *success* text
	// is account identity, which never reaches an error or a log line.
	return fmt.Errorf("%w: %s login status failed: %s; set agent.provider.api_key, "+
		"point agent.provider.codex_home at a usable home, or run `codex login` as the daemon user",
		ErrBinary, path, harness.Excerpt(answer, harness.ProbeExcerpt))
}
