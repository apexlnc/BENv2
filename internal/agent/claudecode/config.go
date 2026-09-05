package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Provider is the parsed `agent.provider` block. The key set is closed: the
// core passes the block through opaquely (SPEC §5.2.5), so this adapter is the
// only thing standing between a typo and a run with the wrong permissions.
//
// Values arrive with `$VAR` indirection already resolved by the config loader
// (SPEC §5.5), so a secret-bearing key here holds the secret itself — which is
// why none of them reach argv (SPEC §7.6, see command).
type Provider struct {
	// Binary is the harness executable; a bare name is resolved on PATH.
	Binary string
	// Model is an alias ("opus") or full name; empty leaves the CLI default.
	Model string
	// PermissionMode is REQUIRED — see ErrPermissionMode.
	PermissionMode string
	// ConfigDir is the closed posture deciding where the harness keeps its own
	// state: isolated (the default) points it at a BEN-owned directory inside
	// the workspace's private dir, inherit leaves it on the operator's
	// ~/.claude. A posture rather than a path — see configDirModes.
	ConfigDir string
	// SandboxMode is the closed §2.2 process-level posture: none (the default)
	// or srt. See sandbox.go.
	SandboxMode string
	// SandboxBinary is the sandbox runtime executable; a bare name is resolved
	// on PATH.
	SandboxBinary string
	// SandboxDomains adds to the egress allowlist the posture pins. Additive
	// only: the floor is what the workflow cannot function without, so a shorter
	// list is not expressible (see egressFloor).
	SandboxDomains []string
	// APIKey and AuthToken are injected into the child environment as
	// ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN. This adapter's documented
	// auth surface (SPEC §7.6).
	APIKey    string
	AuthToken string
	// Settings is a settings *file path* for --settings. Inline JSON, which the
	// flag also accepts, is refused: provider strings are $VAR-resolved, so
	// inline content is a route for a secret into argv (SPEC §7.6).
	Settings        string
	AllowedTools    []string
	DisallowedTools []string
	// AddDirs grants tool access outside the workspace (--add-dir). Absolute
	// paths only (checkAddDirs); empty is the norm: the workspace is the boundary.
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
const DefaultBinary = "claude"

// The config_dir postures (#114).
//
// A closed set of two rather than an operator-named path, which is where this
// differs from codex-exec's `codex_home`. The two values are not two locations;
// they are two answers to "whose state does a run mutate", and the operator
// picks one the way permission_mode makes them pick a posture instead of
// inheriting one. A path key would additionally have to be per-workspace to be
// correct at all — see harnessDirs — which is not something a workflow file can
// spell.
const (
	// ConfigDirIsolated points CLAUDE_CONFIG_DIR and TMPDIR at directories BEN
	// created inside the workspace's private dir (SPEC §6.2). The operator's
	// ~/.claude — their settings, their hooks, their credential, their caches —
	// then stops being the agent's working state.
	//
	// What that does *not* buy, stated here because a posture oversold is worse
	// than one absent (#114 N3, N4):
	//
	//   - It is not a sandbox. The agent's own self-configuration route stays
	//     open through <workspace>/.claude/settings.json, which the harness
	//     honours and which lives in the one tree the agent writes. Closing that
	//     needs a write posture (#81), not an environment variable.
	//   - It does not move the harness's session temp dir. Measured on
	//     claude 2.1.221, that goes to a fixed, uid-scoped /tmp/claude-<uid>
	//     that TMPDIR does not redirect; what honours TMPDIR is the child's own
	//     subprocesses. Per #51 the OS account is the boundary, so a path shared
	//     across every workspace of one uid is consistent with the security
	//     model — but this pin is not what bounds it.
	ConfigDirIsolated = "isolated"
	// ConfigDirInherit is the behaviour that predates #114: the harness resolves
	// its config from $HOME like any interactive invocation. Kept reachable and
	// named, because on a host whose managed settings pin a login method an
	// isolated config dir cannot be authenticated at all (#112) — and a posture
	// with no escape hatch would make this adapter unusable there rather than
	// merely un-isolated.
	ConfigDirInherit = "inherit"
)

var configDirModes = []string{ConfigDirIsolated, ConfigDirInherit}

// The two child variables the isolated posture writes, and this adapter's
// children of the private dir they point at.
const (
	envConfigDir = "CLAUDE_CONFIG_DIR"
	envTmpDir    = "TMPDIR"

	// configDirName holds session state, so its lifetime is the workspace's:
	// measured on 2.1.221, --resume reads
	// $CLAUDE_CONFIG_DIR/projects/<encoded-cwd>/<session>.jsonl, and a resume
	// against a fresh config dir fails outright with "No conversation found
	// with session ID" (#114 N1). A per-attempt config dir would therefore
	// break §7.1 resume silently — the run starts, it just starts over.
	configDirName = "claude-config"
	// tmpDirName is per attempt, because it is not resume state: with the
	// config dir held fixed, changing TMPDIR between attempts left recall
	// working (#114 N2). Replaced at each Start rather than tracked and cleaned
	// up afterwards, so a crashed daemon leaves at most one attempt's scratch
	// behind and the next Start collects it — durable completion state over
	// best-effort cleanup.
	tmpDirName = "claude-tmp"
)

// permissionModes are the CLI's --permission-mode choices, split by whether a
// headless daemon can actually use them. Verified against claude 2.1.221.
var (
	usablePermissionModes = []string{"acceptEdits", "auto", "bypassPermissions", "dontAsk"}
	// manual prompts for every tool use — a headless run would sit at the
	// prompt until the stall window closes. plan cannot write, so it can never
	// reach the publish step the prompt contract requires (SPEC §5.6).
	unusablePermissionModes = map[string]string{
		"manual": "prompts for every tool use; a headless run stalls",
		"plan":   "cannot modify files, so it can never publish",
	}
)

// providerKeys is the closed key set, for the unknown-key refusal and its
// error message.
//
// There is deliberately no key carrying prompt text (the CLI's
// --append-system-prompt): the prompt body is BEN's posture surface
// (SPEC §5.6), so behavioral instructions belong in the template, where they
// are version-controlled and reviewed — and where they cannot reach argv.
var providerKeys = []string{
	"add_dirs", "allowed_tools", "api_key", "auth_token",
	"binary", "config_dir", "disallowed_tools", "env", "env_passthrough", "model",
	"permission_mode", "sandbox_binary", "sandbox_domains", "sandbox_mode", "settings",
}

var credentialKeys = []harness.CredentialKey{
	{ProviderKey: "api_key", Env: sourceAPIKey},
	{ProviderKey: "auth_token", Env: sourceAuthToken},
}

// ownedEnv is this adapter's environment surface: the variables it injects from
// named provider keys, and therefore the ones the generic env surfaces may not
// spell (see harness.CheckOwnedEnv). Derived from credentialKeys so the
// reservation and the display redaction cannot name different sets.
//
// CLAUDE_CONFIG_DIR and TMPDIR are appended rather than tabled — they are owned
// variables but not credentials, the same shape as codex-exec's CODEX_HOME. Both
// are reserved under *every* posture, not only the one that writes them:
// conditioning a load-time reservation on another key's value would make
// `config effective` answer differently for the same variable depending on how
// far the reader had read, and the reservation exists so that one variable has
// one owning config site (SPEC §7.6).
//
// TMPDIR needs this more sharply than the rest of the table. It is the only
// adapter-owned name that is also in core.EnvAllowlist, so without the
// reservation the daemon's own value crosses into the child by default and an
// `env: {TMPDIR: …}` entry silently overrides the pin — a second site writing
// the variable, which is exactly what CheckOwnedEnv exists to refuse.
//
// The sandbox posture's four are reserved on the same terms and for a sharper
// reason: each of them exists to replace a tool's ambient configuration with
// one BEN wrote (see sandbox.go), so a second site setting any of them is not a
// duplicate value but a hole in the posture — a GIT_CONFIG_GLOBAL from `env`
// would restore the `insteadOf` rewrite the pin exists to remove.
var ownedEnv = append(harness.OwnedEnv(credentialKeys),
	harness.ReservedEnv{Name: envConfigDir, Owner: "agent.provider.config_dir"},
	harness.ReservedEnv{Name: envTmpDir, Owner: "agent.provider.config_dir"},
	harness.ReservedEnv{Name: envSandboxTmpDir, Owner: "agent.provider.sandbox_mode"},
	harness.ReservedEnv{Name: envGitConfig, Owner: "agent.provider.sandbox_mode"},
	harness.ReservedEnv{Name: envGitNoSystem, Owner: "agent.provider.sandbox_mode"},
	harness.ReservedEnv{Name: envGHConfigDir, Owner: "agent.provider.sandbox_mode"})

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
	if p.AuthToken, err = b.String("auth_token"); err != nil {
		return Provider{}, err
	}
	if p.Settings, err = b.String("settings"); err != nil {
		return Provider{}, err
	}
	if err := checkSettingsPath(p.Settings); err != nil {
		return Provider{}, err
	}
	if p.AllowedTools, err = b.Strings("allowed_tools"); err != nil {
		return Provider{}, err
	}
	if p.DisallowedTools, err = b.Strings("disallowed_tools"); err != nil {
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

	// The config half of the BEN_ reservation (SPEC §7.6): a collision authored
	// here is written once and hits every run, so it refuses at load.
	if err := harness.CheckProviderEnv(p.Env, p.EnvPassthrough, ErrEnvNamespace); err != nil {
		return Provider{}, err
	}
	// One child variable, one owning config site (SPEC §7.6), asked in both
	// directions off one table: this adapter's own auth surface, which has named
	// keys so that Ready checks the credential a run will actually use, plus the
	// publish credential, which the core names and this adapter injects.
	reserved := append(slices.Clone(ownedEnv), harness.PublishReserved(cfg.Publish)...)
	if err := harness.CheckOwnedEnv(p.Env, p.EnvPassthrough, reserved, ErrEnvReserved); err != nil {
		return Provider{}, err
	}
	// The other direction, and the sharper one: `publish.env: ANTHROPIC_API_KEY`
	// aims the publish credential at this harness's own auth.
	if err := harness.CheckPublishEnv(cfg.Publish, ownedEnv, ErrEnvReserved); err != nil {
		return Provider{}, err
	}
	// A credential the transcript writer could not keep out of the record refuses
	// here rather than leaking once per run (SPEC §10.3).
	if err := harness.CheckRedactable(block, credentialKeys, ErrProviderValue); err != nil {
		return Provider{}, err
	}

	mode, err := b.String("permission_mode")
	if err != nil {
		return Provider{}, err
	}
	if err := checkPermissionMode(mode); err != nil {
		return Provider{}, err
	}
	p.PermissionMode = mode

	if p.ConfigDir, err = b.String("config_dir"); err != nil {
		return Provider{}, err
	}
	if p.ConfigDir == "" {
		p.ConfigDir = ConfigDirIsolated
	}
	if err := checkConfigDir(p.ConfigDir); err != nil {
		return Provider{}, err
	}

	if p.SandboxMode, err = b.String("sandbox_mode"); err != nil {
		return Provider{}, err
	}
	if p.SandboxMode == "" {
		p.SandboxMode = SandboxNone
	}
	if err := checkSandboxMode(p.SandboxMode); err != nil {
		return Provider{}, err
	}
	if p.SandboxBinary, err = b.String("sandbox_binary"); err != nil {
		return Provider{}, err
	}
	if p.SandboxBinary == "" {
		p.SandboxBinary = DefaultSandboxBinary
	}
	if p.SandboxDomains, err = b.Strings("sandbox_domains"); err != nil {
		return Provider{}, err
	}
	// Last, because both refusals are about the posture's *relationship* to keys
	// parsed above — the config dir it needs and the credential it needs — and
	// neither can be asked before those have their values.
	if err := checkSandboxPostures(p, cfg.Publish); err != nil {
		return Provider{}, err
	}

	return p, nil
}

// checkAddDirs refuses a write grant whose meaning depends on the harness cwd.
// Assembly compares these paths with daemon-only state before any run starts,
// so the adapter must state each one absolutely.
func checkAddDirs(dirs []string) error {
	for i, dir := range dirs {
		if !filepath.IsAbs(dir) {
			return harness.ValueRefusalIndex("add_dirs", i, dir,
				fmt.Errorf("%w: add_dirs entry must be an absolute path so daemon-only state can be kept outside it", ErrProviderValue))
		}
	}
	return nil
}

// checkSettingsPath keeps inline JSON out of `settings`. The flag accepts
// either, but a provider string is $VAR-resolved (SPEC §5.5), so inline content
// can carry a secret — and argv is world-readable through ps (SPEC §7.6). A
// path is a reference, not content.
func checkSettingsPath(settings string) error {
	if strings.HasPrefix(strings.TrimSpace(settings), "{") {
		return fmt.Errorf("%w: settings must be a file path, not inline JSON "+
			"(inline values can carry secrets into argv)", ErrProviderValue)
	}
	return nil
}

// checkPermissionMode refuses a missing or unusable posture. A refused mode
// travels as data (harness.ValueRefusal), never in the message: `permission_mode`
// is a $VAR-resolvable provider string like any other, and `ben config effective`
// prints these refusals in CI (SPEC §5.5, §5.8).
func checkPermissionMode(mode string) error {
	switch {
	case mode == "":
		// Nothing to redact, and no value to anchor: an absent key is a fact
		// about the file, not about a value.
		return fmt.Errorf("%w: required (one of %s)", ErrPermissionMode, strings.Join(usablePermissionModes, ", "))
	case slices.Contains(usablePermissionModes, mode):
		return nil
	default:
		if why, known := unusablePermissionModes[mode]; known {
			return harness.ValueRefusal("permission_mode", mode,
				fmt.Errorf("%w: that mode %s; use one of %s", ErrPermissionMode, why, strings.Join(usablePermissionModes, ", ")))
		}
		return harness.ValueRefusal("permission_mode", mode,
			fmt.Errorf("%w: unknown mode (one of %s)", ErrPermissionMode, strings.Join(usablePermissionModes, ", ")))
	}
}

// checkConfigDir refuses an unknown posture. Unlike permission_mode this key
// has a default, and the default is the isolated one: an operator who never
// heard of #114 gets the posture that leaves their own ~/.claude alone, and
// inheriting it is the choice that has to be written down.
//
// The refused value travels as data (harness.ValueRefusal) like every other
// provider string, because provider values are $VAR-resolvable (SPEC §5.5) and
// `ben config effective` prints these refusals in CI (SPEC §5.8).
func checkConfigDir(mode string) error {
	if slices.Contains(configDirModes, mode) {
		return nil
	}
	return harness.ValueRefusal("config_dir", mode,
		fmt.Errorf("%w: unknown posture (one of %s)", ErrConfigDir, strings.Join(configDirModes, ", ")))
}

// checkRuntime is what Ready asks of the world (SPEC §7.1): the binary exists,
// identifies as Claude Code, and has plausible credentials. Structure was
// already settled — purely — by Structural.
//
// Both probes run with the same restricted environment as a real attempt
// (SPEC §7.6). That is not only hygiene — inheriting the daemon's environment
// would hand every daemon secret to a operator-configured binary — it is also
// what makes the credential probe meaningful: an environment that authenticates
// only because the daemon happened to have more in it would pass validation and
// then fail every run.
// Under `srt` both probes run *wrapped*, for the same reason they run with the
// restricted environment: a posture that makes the harness unreachable is a
// posture every dispatch discovers. It is not hypothetical — measured on this
// repo's own install, `claude` resolves through a symlink in a denied subtree,
// and an unwrapped probe passes while every attempt dies with "command not
// found".
func (p Provider) checkRuntime(ctx context.Context, path string, publish harness.PublishValue, t harness.Timings, private, sandboxBinary string) error {
	spec := core.RunSpec{Workspace: core.WorkspacePaths{PrivateDir: private}}
	env, undeclared := p.environ(publish, spec)
	// A value the transcript writer could not cover refuses here, at startup,
	// rather than once per attempt (SPEC §7.1, §10.3).
	if err := harness.CheckRedactableEnv(undeclared, ErrProviderValue); err != nil {
		return err
	}

	probe := p.probeCommand(sandboxBinary, private, path)
	out, err := probe(ctx, t, env, "--version")
	if err != nil {
		// Wrapped, not formatted: a probe refused for flooding its output
		// (harness.ErrProbeOutput) is a different fact about the binary from one
		// that exited non-zero, and the caller should be able to tell them apart.
		return fmt.Errorf("%w: %s --version failed: %w", ErrBinary, path, err)
	}
	// `2.1.221 (Claude Code)` — the marker, not the number: pinning a minimum
	// version would refuse harness upgrades we have no evidence to refuse.
	//
	// Quoted as an excerpt (#235): the capture is bounded, and a refusal is not
	// the place for even that much of it.
	if !strings.Contains(string(out), "Claude Code") {
		return fmt.Errorf("%w: %s --version does not identify as Claude Code: %q",
			ErrBinary, path, harness.Excerpt(strings.TrimSpace(string(out)), harness.ProbeExcerpt))
	}
	return p.checkAuth(ctx, path, env, t, probe)
}

// authStatus is the subset of `claude auth status` this adapter reads. The rest
// of that output is account identity (email, org name) and is deliberately never
// copied into an error message or a log line.
//
// LoggedIn is a pointer so "the field was absent" stays distinguishable from
// "the field said false": only the second is a refusal.
//
// The three policy fields are safe to name in a refusal where the identity
// fields are not: measured against 2.1.221, they hold method, provider and route
// *names* ("claudeai", "firstParty", "ANTHROPIC_API_KEY") — never key material.
type authStatus struct {
	LoggedIn   *bool  `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	// ForcedLoginMethod is the login method the host's managed settings pin —
	// claudeai, console, or gateway. It is machine scope, not user
	// configuration: it survives a fresh CLAUDE_CONFIG_DIR, so the adapter
	// cannot be handed a copy without it.
	ForcedLoginMethod string `json:"forcedLoginMethod"`
	// APIProvider distinguishes a first-party Anthropic credential from a cloud
	// provider or gateway session. Only the first is subject to the pin: a
	// Bedrock, Vertex or Foundry session authenticates against that cloud and is
	// documented as *not* blocked, and a gateway session outranks every
	// environment credential, so a block that sets api_key alongside one has not
	// supplied the credential the run will use.
	APIProvider string `json:"apiProvider"`
	// APIKeySource names the route an API key arrived by. Absent for
	// a plain subscription login, and — importantly — *present* for some login
	// credentials that the pin does not block, which is why blockedSource matches
	// it against the documented set rather than testing it for emptiness.
	APIKeySource string `json:"apiKeySource"`
}

// The credential routes a login pin blocks, named exactly as `auth status`
// reports them, and closed because the harness documents it closed: an
// environment credential is blocked "since organization membership can't be
// verified" for it, while cloud-provider sessions, profile and federation
// credentials, and login credentials are not.
//
// Matching this set is the difference between refusing a credential and refusing
// a *source*. `apiKeySource` is non-empty for login-derived keys too — a Console
// login reports one — so a non-emptiness test refuses a session that satisfies a
// `console` pin perfectly well. Naming the three costs an under-refusal if the
// harness ever renames one, which is loud at dispatch; the other direction is a
// daemon that will not start against a working host.
var pinBlockedSources = []string{sourceAPIKey, sourceAuthToken, sourceKeyHelper}

// The three routes, spelled once. sourceAPIKey and sourceAuthToken are also this
// adapter's injected variable names (credentialKeys), which is not a coincidence
// to be maintained in two places: the route a pin blocks and the variable this
// adapter injects are the same string, and blockedSource maps between them.
const (
	sourceAPIKey    = "ANTHROPIC_API_KEY"
	sourceAuthToken = "ANTHROPIC_AUTH_TOKEN"
	// Not an environment variable: a settings key naming a script to run.
	sourceKeyHelper = "apiKeyHelper"
	// oauthTokenMethod is `authMethod` for *any* bearer-token session, which is
	// why it is a method and not a source — see blockedSource.
	oauthTokenMethod = "oauth_token"
)

// blockedSource returns the credential route this session will actually
// authenticate with, when the host's pin blocks it, and "" when nothing is
// blocked.
//
// It is a Provider method rather than a property of the answer because one
// answer is genuinely ambiguous and only the block can resolve it — see the
// oauth_token case below.
func (p Provider) blockedSource(s authStatus) string {
	if s.ForcedLoginMethod == "" {
		return ""
	}
	// A cloud provider or gateway session is not first-party, and the pin does
	// not reach it. Read as a positive test so an unrecognized provider is
	// treated as not-first-party: that direction costs a refusal we did not
	// make, which the dispatch reports, rather than one we should not have made.
	if s.APIProvider != "firstParty" {
		return ""
	}
	// `oauth_token` names a *shape* of credential, not where it came from, and
	// several sources produce it. Measured on 2.1.221 in a fresh config dir, a
	// bogus ANTHROPIC_AUTH_TOKEN and a bogus CLAUDE_CODE_OAUTH_TOKEN produce
	// byte-identical answers; an Anthropic profile reports the same pair. Of
	// those, only ANTHROPIC_AUTH_TOKEN is blocked — `claude setup-token` output
	// and profile/federation credentials are documented as not blocked — so
	// treating the method as a source refuses two working configurations.
	//
	// The block resolves it, and soundly: ANTHROPIC_AUTH_TOKEN is adapter-owned
	// (ownedEnv), so CheckOwnedEnv bars every other environment surface from
	// spelling it and the named key is the *only* route it can reach a child by.
	// If this block does not carry one, the child does not have one, and the
	// bearer token in use came from somewhere the pin allows.
	if s.AuthMethod == oauthTokenMethod {
		if key, _ := p.credentialFor(sourceAuthToken); key != "" {
			// Checked before APIKeySource because it outranks it: with both
			// environment credentials set the harness selects the bearer token
			// (measured), while APIKeySource still names the key that is merely
			// present.
			return sourceAuthToken
		}
		// Not ours — fall through rather than returning, because an API key can be
		// in play behind an oauth-shaped session and the pin blocks it. Measured
		// on 2.1.221, CLAUDE_CODE_OAUTH_TOKEN and ANTHROPIC_API_KEY together:
		//
		//	{"authMethod":"oauth_token", …, "apiKeySource":"ANTHROPIC_API_KEY"}
		//	→ dispatch exits with the managed-pin refusal
		//
		// So `authMethod` does not report which credential won — it reports that a
		// bearer token is present — and the two fields have to be read together.
		// An early return here would pass that configuration at startup and fail
		// every attempt, which is the exact bug this whole check exists for.
	}
	// A method that is not itself a route leaves APIKeySource to say what is in
	// use: an ambient subscription login with ANTHROPIC_API_KEY set reports
	// authMethod "claude.ai" and is refused anyway.
	if slices.Contains(pinBlockedSources, s.APIKeySource) {
		return s.APIKeySource
	}
	return ""
}

// checkAuth is SPEC §7.1's "auth plausible". `claude auth status` answers
// locally — no API call.
//
// The exit code is deliberately ignored: 2.1.221 exits 1 when logged out while
// still printing a complete JSON answer on stdout, so treating a non-zero exit
// as "no answer" would swallow the one case this probe exists to catch. The body
// is the signal.
//
// An answer that is not parseable is not a refusal: a harness build without the
// subcommand prints usage instead, and --version has already established
// identity. Refusing there would make this probe a compatibility trap.
//
// "Logged in" is not on its own the question readiness needs answered, which is
// what #112 cost a green startup and a failing run to establish. The three
// states below are distinct facts and each needs its own verdict; conflating the
// middle one with either neighbour is the bug.
func (p Provider) checkAuth(ctx context.Context, path string, env []string, t harness.Timings, probe probeFunc) error {
	out, _ := probe(ctx, t, env, "auth", "status")
	if ctx.Err() != nil {
		// The binary took the whole validation window and said nothing, which
		// is its own kind of unusable.
		return fmt.Errorf("%w: %s auth status did not answer: %v", ErrBinary, path, ctx.Err())
	}
	var status authStatus
	if err := json.Unmarshal(bytes.TrimSpace(out), &status); err != nil || status.LoggedIn == nil {
		return nil
	}
	if !*status.LoggedIn {
		return noCredential(path, status)
	}
	return p.checkPin(path, status)
}

// noCredential refuses a logged-out harness. Its advice depends on the pin,
// because the unpinned advice — set agent.provider.api_key — is exactly what
// produces the pinned refusal one dispatch later (#112).
func noCredential(path string, status authStatus) error {
	if status.ForcedLoginMethod != "" {
		return fmt.Errorf("%w: %s reports no usable credential, and this host pins login method %q; "+
			"this adapter's named credentials (agent.provider.api_key, agent.provider.auth_token) "+
			"cannot satisfy that pin, so authenticate the account the "+
			"daemon runs as (`claude auth login`) rather than setting agent.provider.api_key",
			ErrBinary, path, status.ForcedLoginMethod)
	}
	return fmt.Errorf("%w: %s reports no usable credential (auth method %q); "+
		"set agent.provider.api_key, or name the variables the harness needs in "+
		"agent.provider.env_passthrough", ErrBinary, path, status.AuthMethod)
}

// checkPin refuses a credential the host's managed settings will not accept.
//
// Measured on 2.1.221 (#112): where a login method is pinned, every route this
// adapter has — ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, and an apiKeyHelper —
// is refused at dispatch, while this same probe reports loggedIn: true. So
// without this check readiness is green and every attempt burns a workspace
// rediscovering the refusal, which is the failure mode SPEC §7.1's Ready exists
// to prevent.
//
// It refuses the *combination*, not the host. A pinned host whose account holds
// a first-party login and whose block supplies no credential runs normally, and
// that is the only working configuration on such a machine — refusing it would
// make the pin fatal rather than restrictive.
//
// What it refuses is the *effective* source, never the configured one
// (blockedSource). Two ways that distinction bites, both of which turn a correct
// refusal into a false one:
//
//   - A pin is not always claudeai, and not every credential reporting an
//     apiKeySource is an environment credential — a Console login supplies one
//     and satisfies a `console` pin.
//   - An environment credential in the block is not necessarily what the run
//     uses. A cloud-provider or gateway session outranks both of this adapter's
//     variables, and the pin does not reach it, so a block that sets api_key
//     beside one has supplied a credential the run never reads.
//
// One route stays invisible here and is documented rather than guessed at:
// `auth status` rejects --settings, so an apiKeyHelper reaching a run through
// agent.provider.settings cannot be probed (see the package doc).
func (p Provider) checkPin(path string, status authStatus) error {
	source := p.blockedSource(status)
	if source == "" {
		return nil
	}
	// Anchored at the key that supplies *this* source, so the field the operator
	// is told to remove is the one the run authenticates with. A $VAR-resolved
	// provider string may be a secret, so the value travels as data and the
	// renderer decides what to show (SPEC §5.5, §5.8).
	if key, value := p.credentialFor(source); key != "" {
		return harness.ValueRefusal(key, value, fmt.Errorf(
			"%w: this host pins login method %q, which this adapter's named credentials cannot "+
				"satisfy; "+
				"remove agent.provider.%s and authenticate the account the daemon runs as "+
				"(`claude auth login`), or run this workflow on a host without the pin",
			ErrCredentialPinned, status.ForcedLoginMethod, key))
	}
	// A credential from somewhere this block cannot name — no field to anchor to,
	// and the source is a route name rather than key material (see authStatus).
	return fmt.Errorf("%w: this host pins login method %q, and %s is authenticating with an "+
		"credential from %q that the pin blocks and this workflow did not supply; remove it, or "+
		"authenticate the account the daemon runs as (`claude auth login`)",
		ErrCredentialPinned, status.ForcedLoginMethod, path, source)
}

// credentialFor reports the provider key that supplies one environment
// credential route, and its value; "" when this block does not supply that route
// and the credential reached the harness some other way.
//
// Keyed by the route rather than scanned in table order, because the harness
// picks by its own documented precedence and the caller has already read which
// route won. Scanning would name whichever key the table happens to list first —
// api_key today — and tell an operator running on ANTHROPIC_AUTH_TOKEN to remove
// a field that is not the one in use.
//
// Read off credentialKeys through the injected environment rather than off the
// Provider fields, so a credential added to that table is covered by the edit
// that adds it — the same one-declaration rule OwnedEnv and SensitiveFields
// already derive from it (SPEC §7.7).
func (p Provider) credentialFor(source string) (key, value string) {
	injected := p.injected()
	for _, k := range credentialKeys {
		if k.Env == source && injected[k.Env] != "" {
			return k.ProviderKey, injected[k.Env]
		}
	}
	return "", ""
}
