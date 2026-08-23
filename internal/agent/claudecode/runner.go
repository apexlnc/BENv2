// Package claudecode is the claude-code agent runner adapter (SPEC §7.7):
// `claude -p --output-format stream-json`, one process per attempt, resume via
// `--resume`. It translates the harness stream into the closed event enum at
// the boundary so the orchestrator never sees a raw agent event (SPEC §3.6).
//
// What this package owns is the claude-specific half: which argv to build,
// which environment variables carry the credential, what one line of the stream
// means, and what readiness asks of the binary. The process lifecycle, liveness
// windows, signal ladder, and transcript retention are the same obligation for
// every process-per-attempt harness and live in internal/agent/harness.
//
// Two consequences of the contract that the interface does not make obvious:
//
//   - Configuration binds at New, not per run (SPEC §7.1). Structural is a pure
//     property of the kind; Ready checks the bound config against the world; and
//     because Start cannot be handed a different block, the two cannot disagree.
//   - Readiness probes run with the same restricted environment as a real
//     attempt (SPEC §7.6), so a harness the daemon merely validates never sees
//     the daemon's secrets — and an environment that authenticates only because
//     the daemon happened to have more in it cannot pass validation and then
//     fail every run.
//
// # The agent.provider block
//
// SPEC §7.7 requires each adapter to document its own keys. The set is closed —
// an unknown key is refused at load, because a typo in a permission mode is not
// something to discover at run time.
//
//	permission_mode  REQUIRED. acceptEdits | auto | bypassPermissions | dontAsk.
//	                 No default: the operator states the posture. `manual` and
//	                 `plan` are refused — the first prompts (a headless run
//	                 stalls), the second cannot write, so it can never publish.
//	binary           Harness executable; a bare name resolves on PATH ("claude").
//	model            Alias ("opus") or full name.
//	api_key          → ANTHROPIC_API_KEY in the child env.
//	auth_token       → ANTHROPIC_AUTH_TOKEN in the child env.
//	config_dir       isolated (default) | inherit. Whose harness state a run
//	                 mutates — see "The isolated config directory" below.
//	sandbox_mode     none (default) | srt. The §2.2 process-level posture — see
//	                 "The process-level sandbox" below.
//	sandbox_binary   Sandbox runtime executable; a bare name resolves on PATH
//	                 ("srt").
//	sandbox_domains  Egress domains added to the allowlist the posture pins.
//	                 Additive: the floor is what the workflow cannot function
//	                 without, so a shorter list is not expressible.
//	settings         → --settings. A file *path*; inline JSON is refused, since
//	                 provider strings are $VAR-resolved and argv is public.
//	allowed_tools    → --allowed-tools, one flag per entry.
//	disallowed_tools → --disallowed-tools, one flag per entry.
//	add_dirs         → --add-dir. Empty is the norm; the workspace is the boundary.
//	env              Extra child environment entries.
//	env_passthrough  Daemon environment variable names to forward. Names only,
//	                 so a secret does not become a load-time requirement.
//
// Neither environment surface may define a `BEN_`-prefixed key: that namespace
// is the orchestrator's, exclusively, and this half of the reservation is a
// property of the file, so it refuses at load (SPEC §7.6). Nor may either one
// spell the variable `publish.env` names, for the same reason pointed at a
// different owner.
//
// Publish-credential surface (SPEC §5.2.8, §6.7, §7.7): the agent runs `gh`
// itself, and what `gh` authenticates from arrives through the top-level
// `publish` block rather than through this one — the identity BEN publishes as is
// the operator's choice, not an adapter parameter. This adapter resolves that
// reference once per attempt and injects it under the variable the block names,
// never into argv.
//
// # The isolated config directory
//
// By default (`config_dir: isolated`) this adapter points CLAUDE_CONFIG_DIR and
// TMPDIR at directories inside the private dir the workspace provider placed
// (SPEC §6.1, §6.2), so the operator's ~/.claude — their settings, their hooks,
// their credential, their caches — stops being the agent's working state. The
// two have different lifetimes on purpose: the config dir is the workspace's,
// because §7.1 resume reads session state out of it and a per-attempt one fails
// with "No conversation found with session ID"; the temp dir is the attempt's,
// and is replaced at each Start (#114 N1, N2).
//
// Three consequences an operator will notice, all of them intended:
//
//   - **Operator hooks stop running.** settings.json is read from
//     CLAUDE_CONFIG_DIR, so user-level hooks and permissions do not apply to
//     BEN's runs. Correct for an unattended daemon — §5.6's prompt is the
//     contract, not the operator's local tooling — but stated here rather than
//     discovered. Workspace-level hooks (<workspace>/.claude/settings.json)
//     still run.
//   - **The config dir starts unauthenticated**, so a run needs a credential
//     this adapter injects. On a host that pins a login method it therefore
//     cannot authenticate at all (#112), which is what `inherit` is for.
//   - **The session path is derived from cwd.** Measured on 2.1.221, session
//     state lands under $CLAUDE_CONFIG_DIR/projects/<encoded-absolute-cwd>/,
//     and cwd is the workspace path. §6.2 reattaches the same path across a
//     chain so this holds — but a workspace that *moved* would break resume as
//     thoroughly as a fresh config dir would.
//
// What it is not: a boundary. `<workspace>/.claude/settings.json` is honoured
// and lives in the one tree the agent writes, so an agent can still give itself
// hooks; closing that needs the write posture below, not an environment
// variable. And the harness's own session temp dir goes to a fixed
// /tmp/claude-<uid> that TMPDIR does not redirect — shared across every
// workspace of one uid, which #51's OS-account boundary already accounts for.
//
// # The process-level sandbox
//
// `sandbox_mode: srt` wraps the whole harness process in
// @anthropic-ai/sandbox-runtime under a settings file this adapter composes
// whole (sandbox.go). `none` is the default, because #51 already decided the OS
// account is the boundary and this rung is RECOMMENDED — and because a default
// that required an npm install would refuse readiness on every host that has
// not done one.
//
// Deliberately not claude-code's own sandbox: that one isolates Bash commands
// and their children only, while Read, Edit, WebFetch, MCP servers and hooks
// run in-process on the host — and `bypassPermissions`, the mode a headless
// daemon wants, is exactly the one that removes the rules gating those.
//
// Five refusals rather than five ways to get it subtly wrong, the first three
// at load:
//
//   - **`config_dir: inherit` is refused with it** (ErrSandboxConfigDir). Under
//     `inherit` the harness resolves its configuration from the `$HOME` this
//     posture denies, so the operator's hooks apply and cannot start; #81's own
//     real-agent run measured the result — every Bash call refused, the agent
//     reporting tool failures and stopping without committing.
//   - **An environment credential is required** (ErrSandboxCredential). An
//     OAuth session reads its credential from `~/Library/Keychains`, inside the
//     denied `$HOME`; carving that out would hand the agent every keychain item
//     on the host to buy back one credential.
//   - **A `publish` block is required** (ErrSandboxPublish). Omitting it is
//     permitted in general and means the agent authenticates from what §7.6's
//     allowlist carries — "HOME, and whatever the forge CLI stores under it"
//     (SPEC §5.2.8) — which is precisely the arrangement this posture denies.
//     Accepting it would produce a run that works, commits, and has no
//     credential to publish with.
//   - **The daemon needs a git identity** (ErrSandboxIdentity), because the
//     posture replaces the global git configuration with one BEN writes, and an
//     identity absent from it fails every `git commit` an attempt makes.
//   - **Ready probes the posture behaviourally** (ErrSandboxPosture), in two
//     parts. First that the runtime enforces *anything*: a wrapper that loaded
//     the settings and executed the child would pass every other check while
//     leaving `$HOME` open, and srt strips settings keys it does not recognize
//     rather than refusing them, so a build whose schema has moved is exactly
//     that shape. Then that a Go client can verify TLS through it. Neither the
//     file nor the version can answer either question — srt reports version
//     1.0.0 whatever the package version is.
//
// What the posture states, each measured against srt 0.0.73 rather than assumed
// (#81 F1–F6 and the gaps after them):
//
//   - The **shared git dir** is in `allowRead` *and* `allowWrite`. srt does not
//     special-case linked worktrees, so `git commit` fails on `index.lock`
//     without the write, and reports "not a git repository" without the read —
//     allowWrite does not imply read, and SPEC §5.2.4's default root under
//     `$HOME` is what makes that reachable. It arrives on the RunSpec because
//     the file that would answer `git rev-parse --git-common-dir` is one srt
//     leaves writable by design, and §6.2 reattaches.
//   - `denyWrite` covers `base.git/hooks`, `base.git/config`,
//     `<workspace>/.git`, and the directory holding BEN's own settings file and
//     git config — the agent's routes to configuring its next attempt.
//   - **`enableWeakerNetworkIsolation` is pinned on darwin.** Seatbelt breaks
//     Go's macOS platform verifier, so `gh` fails with `x509: OSStatus -26276`
//     and a run commits, pushes, and cannot open a PR. srt documents the flag as
//     "a potential data exfiltration vector through the trustd service"; that is
//     the stated cost of §5.6's publish snippet needing a Go CLI. On linux Go
//     reads file roots and the flag is a no-op.
//   - **BEN owns the tool configuration the posture denies**: GIT_CONFIG_GLOBAL
//     plus GIT_CONFIG_NOSYSTEM (git treats an unreadable `~/.gitconfig` as
//     fatal, and a host `insteadOf` rewrite silently redirects a push to SSH),
//     GH_CONFIG_DIR (`gh` refuses to start without its config), and
//     CLAUDE_CODE_TMPDIR (srt replaces the child's TMPDIR with a shared
//     `/tmp/claude` that does not exist on a fresh host, unless this names where
//     to point it).
//   - **The git config carries a credential helper**, or §5.6's `git push
//     origin HEAD` cannot authenticate: with the system config disabled and
//     BEN's global config in place, git has no credential source, the origin URL
//     is deliberately credential-free (§6.7), and git does not read GH_TOKEN.
//     `gh auth git-credential` is the mechanism `gh auth setup-git` installs and
//     reads the token this adapter already injects, so no secret enters the
//     file. Unscoped, because BEN's tracker supports GitHub Enterprise.
//   - **The runtime's own security defaults are pinned, not inherited**:
//     `filesystem.disabled` (true emits no read or write rules at all, which
//     makes every path above decoration), `strictAllowlist` (without it the
//     allowlist is, in srt's words, "a prompt-suppression hint" referred to an
//     ask callback an unattended daemon cannot answer), `allowAppleEvents` (srt
//     documents it as removing code-execution isolation), `allowLocalBinding`,
//     the unix-socket keys and `allowMachLookup` (ways out that are neither a
//     domain nor a path, so the egress check never sees them), `allowGitConfig`,
//     `allowPty`, and `enableWeakerNestedSandbox`.
//   - **The sandbox runtime and `gh` are resolved once and reused**, like the
//     harness binary and for a sharper reason: both default to bare names, Ready
//     is where the runtime is behaviourally verified, and re-resolving at Start
//     would let a new PATH entry hand an attempt the thing doing the bounding.
//
// And what it does not claim, because a posture oversold is worse than one
// absent:
//
//   - **`$HOME` is not sealed.** srt adds `~/.claude/debug` and `~/.npm/_logs`
//     to its own default write paths whatever this file says.
//   - **`git push -u` prints two errors even when it succeeds.** The `-u`
//     bookkeeping writes branch tracking into the *shared* repository config,
//     which this posture denies; measured, the ref lands and git exits 0 with
//     `error: could not write config file …` twice on stderr. The publication is
//     real, and §9.7 verifies it from git facts rather than from that output —
//     but an agent reading its own tool output may not believe it. #150 removes
//     that write from the canonical step: `git push origin HEAD` names the
//     destination without installing local upstream bookkeeping, which is
//     neither BEN state nor publish evidence. Nothing changes here: the shared
//     config remains denied, and authentication, transport and remote-refusal
//     diagnostics remain visible.
//   - **Egress allowlisting is a setting, not a boundary** (SPEC §10.1): the
//     proxy matches the client-supplied hostname without terminating TLS. srt
//     0.0.73 does offer `tlsTerminate`, which would make it more, but every Go
//     client would then have to trust a private CA it cannot be told about —
//     the same failure the darwin pin exists to avoid.
//   - **Acceptance criterion 3's agent half is not evidenced.** A real `git
//     commit` in a linked worktree under the composed posture is
//     (TestSandboxCommitsInALinkedWorktree); a real `claude` run under it needs
//     an environment credential, which a host pinning a login method refuses
//     outright (#112).
//
// # Hosts that pin a login method
//
// Managed settings can require a first-party login, and `claude auth status`
// reports that as a `forcedLoginMethod`. Where one is pinned, every credential
// this adapter can inject — and an `apiKeyHelper` besides — is refused at
// dispatch, so Ready refuses the combination up front with ErrCredentialPinned
// (#112). A pinned host is not itself refused: with no credential in the block
// the harness authenticates from the daemon account's own login and runs
// normally, which on such a host is the only configuration that works.
//
// One route Ready cannot see, stated because a probe's blind spot is not
// something to leave in a comment: `claude auth status` rejects --settings
// (2.1.221), so an `apiKeyHelper` reaching a run through the `settings` key is
// invisible to it and will fail at dispatch on a pinned host. The settings file
// is a path this adapter deliberately never reads — that is what keeps a secret
// out of argv (see checkSettingsPath) — and reading it to find a helper would
// trade a documented blind spot for an undocumented credential surface.
//
// Verified against claude 2.1.221; testdata holds a recorded stream from it.
package claudecode

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// readinessTimeout bounds Ready's two subprocess probes together. Startup
// refuses on it (SPEC §11), so it must not hang a daemon boot.
const readinessTimeout = 10 * time.Second

// KindName is the agent.kind this package answers to.
const KindName = "claude-code"

// Kind is the package-level agent.kind registration (SPEC §5.7, §7.1; BUILD
// assembly decision 13): the entry points that exist before any runner does.
type Kind struct{}

var _ core.RunnerKind = Kind{}

// Structural reports whether an agent.provider block is well-formed. It is pure
// — no filesystem, no subprocess, no network — so `ben config effective` works
// on a machine that has never installed the harness (SPEC §5.8, §7.1).
// Everything that can fail because the world is not set up belongs to Ready.
func (Kind) Structural(cfg core.AgentConfig) error {
	_, err := ParseProvider(cfg)
	return err
}

// ForwardedEnvVars names the variables this adapter copies into a child by
// name (SPEC §10.2). Everything the block carries in as a resolved value is the
// loader's to see from its own provenance, which is what keeps a route from
// being forgotten here.
func (Kind) ForwardedEnvVars(provider map[string]any) []string {
	return harness.ForwardedEnvVars(provider)
}

// SensitiveFields names the provider values that are secrets, so every
// `config effective` rendering hides them whatever their provenance
// (SPEC §5.8). Delegated, because the two adapters place secrets in the same
// two shapes and a private copy each is a second place to forget one.
func (Kind) SensitiveFields(provider map[string]any) [][]string {
	return harness.SensitiveFields(provider, credentialKeys)
}

// Model names the model this block selects, for the attempt-outcome record
// (#60). Delegated for the same reason the two above are: both adapters read
// the same key, and the key this package turns into `--model` (command.go) is
// the one the record has to name.
func (Kind) Model(provider map[string]any) (string, []string) {
	return harness.Model(provider)
}

// New binds the configuration to a runner instance.
//
// The nil is returned explicitly rather than as a typed *Runner: a refusal must
// leave the caller with a nil interface, or `runner, err := kind.New(...)`
// hands back something non-nil to call methods on after it has already failed.
func (Kind) New(opts core.RunnerOptions) (core.AgentRunner, error) {
	r, err := New(Options{
		Provider:       opts.Provider,
		Publish:        opts.Publish,
		AttemptTimeout: opts.AttemptTimeout,
		TranscriptDir:  opts.TranscriptDir,
		OnRun:          opts.OnRun,
		Timings:        harness.Timings{StopGrace: opts.StopGrace},
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Runner is a constructed claude-code runner: its provider configuration is
// bound and will not change under it (SPEC §7.1).
type Runner struct {
	provider Provider
	// publish is the publish credential *reference* (SPEC §5.2.8), bound here
	// with the provider block so Ready and Start cannot disagree about which
	// credential a run publishes with (SPEC §7.1). The value behind it is read
	// per attempt, never held: a workflow file must load on a host that has none,
	// so this is the one piece of configuration whose resolution is deferred.
	publish core.PublishBinding
	// attemptTimeout is the bound the publish credential's deadline is checked
	// against, bound here with the credential for the same reason: Ready and
	// Start must not disagree about the arithmetic (SPEC §7.7).
	attemptTimeout time.Duration

	// binary is the harness path, resolved on the first successful Ready or
	// Start and reused from then on. Resolution belongs to Ready rather than New
	// (BUILD B06): a binary installed between construction and readiness must be
	// found. Only success is remembered — memoizing a failure would make a
	// runner permanently wrong about a machine that has since been set up, and
	// the binding exists to prevent divergence, not to cache absence. Resolving
	// again after that would undo it in the other direction: a PATH change
	// between Ready and Start would run a binary nothing verified (SPEC §7.1).
	binary resolvedBinary
	// sandbox and gh are bound on the same terms and for the same reason. Both
	// default to bare names resolved on PATH, and both are verified at Ready —
	// the sandbox behaviourally, `gh` by being the client the egress probe runs
	// — so resolving them again at Start would let a PATH entry appearing in
	// between hand an attempt a sandbox nothing checked. That is the sharper
	// direction here than for the harness: the thing being swapped is the thing
	// doing the bounding.
	sandbox resolvedBinary
	gh      resolvedBinary

	transcripts harness.TranscriptStore
	onRun       core.RunEvidenceSink
	// redact are the bound block's credential values, kept out of retained
	// transcripts (SPEC §10.3). Read off credentialKeys through
	// harness.CredentialValues, so this adapter states each credential once.
	redact  []string
	timings harness.Timings
	// signal sends sig to a process group; injectable so a test can simulate a
	// process the kernel will not kill for us.
	signal harness.SignalFunc
}

// Options configures a Runner.
type Options struct {
	// Provider is the agent.provider block. It is parsed once, here.
	Provider map[string]any
	// Publish is the publish credential's binding (SPEC §5.2.8): the child
	// variable and the source that mints it. The zero value configures none.
	Publish core.PublishBinding
	// AttemptTimeout is `limits.attempt_timeout_ms`, the other operand of the
	// TTL gate Ready and Start apply to a bounded credential (SPEC §7.7).
	AttemptTimeout time.Duration
	// TranscriptDir retains raw per-run harness streams (SPEC §10.3); empty
	// disables retention. Transcripts overrides it when set.
	TranscriptDir string
	// Transcripts is an explicit sink, for a caller that owns run identity.
	Transcripts harness.TranscriptStore
	// Timings overrides the harness's lifecycle windows (harness.Timings); each
	// unset field keeps its harness.DefaultTimings value.
	Timings harness.Timings
	// OnRun records each run's evidence against the workspace it belongs to
	// (SPEC §9.10). Bound per attempt in Start, never hoisted: one runner serves
	// every issue, so a sink that could not name the workspace would upgrade the
	// wrong marker.
	OnRun core.RunEvidenceSink

	// signal is test-only (see Runner.signal).
	signal harness.SignalFunc
}

// New binds the provider configuration to a runner. Binding at construction is
// what makes Ready meaningful: the configuration Ready checks is the one Start
// will use, and a reload constructs a fresh runner rather than mutating this
// one underneath an in-flight attempt (SPEC §7.1).
func New(opts Options) (*Runner, error) {
	// The same call Structural makes, with the same argument, so a runner cannot
	// be constructed from a configuration Structural would have refused.
	p, err := ParseProvider(core.AgentConfig{Provider: opts.Provider, Publish: harness.PublishReference(opts.Publish)})
	if err != nil {
		return nil, err
	}
	r := &Runner{
		provider:       p,
		publish:        opts.Publish,
		attemptTimeout: opts.AttemptTimeout,
		redact:         harness.CredentialValues(opts.Provider, credentialKeys),
		transcripts:    opts.Transcripts,
		onRun:          opts.OnRun,
		timings:        opts.Timings,
		signal:         opts.signal,
	}
	if r.transcripts == nil {
		if opts.TranscriptDir != "" {
			r.transcripts = harness.DirTranscripts{Dir: opts.TranscriptDir}
		} else {
			r.transcripts = harness.NopTranscripts{}
		}
	}
	return r, nil
}

// Capabilities reports what this harness supports (SPEC §7.1). Resume is the
// session id carried in RunSpec.Continuation; usage arrives on the result line.
func (r *Runner) Capabilities() core.Capabilities {
	return core.Capabilities{Resume: true, Usage: true}
}

// Ready checks the bound configuration against the world: the binary exists,
// identifies itself as Claude Code, and holds a usable credential (SPEC §7.1).
// Catching any of it here is the difference between one loud refusal at startup
// and every dispatch burning a workspace to rediscover it.
func (r *Runner) Ready(ctx context.Context) error {
	path, err := r.resolveBinary()
	if err != nil {
		return err
	}
	// The publish credential is read here as well as at Start, and this is the
	// reading that matters: §5.5 defers it so the file loads without the secret,
	// which leaves startup as the one place a host missing it can be told once
	// rather than one dispatched issue at a time (SPEC §5.2.8).
	publish, err := harness.MintPublish(ctx, r.publish, r.attemptTimeout, ErrPublishCredential)
	if err != nil {
		return err
	}
	// Under `isolated` the probes must run inside a config dir of the kind an
	// attempt will use, not the operator's. #112's M4 measured why: a config
	// dir BEN created is unauthenticated ("Not logged in · Please run /login")
	// until this adapter injects a credential into it, so probing ~/.claude
	// would answer for a session no attempt ever has — a Ready that verifies
	// one configuration while Start launches another, which is the divergence
	// §7.1's bind-at-New rule exists to remove, reached here by omission
	// instead of by a reload.
	//
	// A throwaway directory rather than a workspace's: there is no workspace at
	// startup, and every workspace's config dir begins in exactly this state,
	// so the first attempt of every chain authenticates under what is probed
	// here. It is not the whole answer — attempt 2 runs against whatever
	// attempt 1 left — but a first attempt that cannot authenticate is a chain
	// that never has a second.
	probe, cleanup, err := r.provider.probePrivateDir()
	if err != nil {
		return err
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(ctx, readinessTimeout)
	defer cancel()

	// The sandbox runtime is resolved and its posture laid down before either
	// harness probe, because under `srt` both of them run through it.
	//
	// Both binaries come back from the one call, and that is the whole reason it
	// returns a pair: `gh` is named by the credential helper this posture just
	// wrote, so the probe below has to run the same path — and resolving it
	// anywhere else would either certify a different binary or, under `none`,
	// require a `gh` that posture has no use for.
	sandbox, gh, err := r.readySandbox(ctx, probe, path)
	if err != nil {
		return err
	}
	env, _ := r.provider.environ(publish, core.RunSpec{
		Workspace: core.WorkspacePaths{PrivateDir: probe},
	})
	// Before the harness probes: if the runtime enforces nothing, what the two
	// of them report is true of a process that is not sandboxed, and the cheaper
	// question is the one that decides whether the rest mean anything.
	if err := r.provider.probeEnforcement(ctx, r.timings, sandbox, probe, env); err != nil {
		return err
	}
	if err := r.provider.checkRuntime(ctx, path, publish, r.timings, probe, sandbox); err != nil {
		return err
	}
	// Last, because it is the one probe that leaves the host.
	return r.provider.probeEgress(ctx, r.timings, sandbox, gh, probe, env, publish)
}

// readySandbox resolves the sandbox runtime and writes a posture over the
// throwaway probe dir, returning the runtime's path ("" under `none`).
//
// The git identity is read here as well as at Start, and this is the reading
// that matters: with the global git configuration replaced, a host that has
// none fails every `git commit` an agent makes, and Ready is where that costs
// one refusal instead of one workspace per attempt.
func (r *Runner) readySandbox(ctx context.Context, probe, binary string) (sandbox, gh string, err error) {
	// The one place the posture's binaries are resolved. `none` resolves
	// neither: it adds nothing, so a host without a sandbox runtime — or without
	// `gh`, which only the credential helper needs — must go on starting.
	if r.provider.SandboxMode != SandboxSRT {
		return "", "", nil
	}
	if sandbox, err = r.resolveSandbox(); err != nil {
		return "", "", err
	}
	identity, err := daemonGitIdentity(ctx, r.timings)
	if err != nil {
		return "", "", err
	}
	// The bound reference, not the resolved value: whether a credential helper
	// is written is a property of the configuration, and Ready must lay down the
	// posture a run gets rather than one that differs when a secret happens to
	// be absent from this process.
	if gh, err = r.resolveGH(); err != nil {
		return "", "", err
	}
	if err := r.provider.writeSandbox(r.provider.probeSandboxPaths(probe, binary, gh), identity); err != nil {
		return "", "", err
	}
	return sandbox, gh, nil
}

// probePrivateDir stands in for a workspace's private dir during readiness.
// Under `inherit` there is nothing to stand in for, and the probes run against
// the operator's config dir because that is what an attempt will use too.
func (p Provider) probePrivateDir() (private string, cleanup func(), err error) {
	if p.ConfigDir != ConfigDirIsolated {
		return "", func() {}, nil
	}
	dir, err := os.MkdirTemp("", "ben-claude-ready-")
	if err != nil {
		return "", func() {}, fmt.Errorf("%w: cannot create a config dir to probe with: %v", ErrBinary, err)
	}
	if err := ensureHarnessDirs(p.harnessDirs(dir)); err != nil {
		os.RemoveAll(dir)
		return "", func() {}, err
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}

// resolvedBinary memoizes one executable's resolved path.
//
// Only success is remembered — memoizing a failure would make a runner
// permanently wrong about a machine that has since been set up, and the binding
// exists to prevent divergence, not to cache absence. Resolving again after
// success would undo it in the other direction.
type resolvedBinary struct {
	mu   sync.Mutex
	path string
}

func (b *resolvedBinary) resolve(name string, sentinel error) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.path != "" {
		return b.path, nil
	}
	path, err := harness.ResolveBinary(name, sentinel)
	if err != nil {
		return "", err
	}
	b.path = path
	return b.path, nil
}

// resolveBinary finds the harness once and keeps the answer.
func (r *Runner) resolveBinary() (string, error) {
	return r.binary.resolve(r.provider.Binary, ErrBinary)
}

// resolveSandbox finds the sandbox runtime once and keeps the answer.
func (r *Runner) resolveSandbox() (string, error) {
	return r.sandbox.resolve(r.provider.SandboxBinary, ErrSandbox)
}

// resolveGH finds the `gh` the credential helper runs, or "" when no publish
// credential is configured and therefore no helper is written.
//
// Tied to the publish credential because that is the token the helper answers
// with: without one there is nothing for it to supply, and a helper that always
// answered nothing would turn a missing credential into a git prompt. Under
// `srt` the configuration cannot arise — ErrSandboxPublish refuses it at load —
// so this is the unsandboxed posture's answer and a second guard on that one.
func (r *Runner) resolveGH() (string, error) {
	if !r.publish.Configured() {
		return "", nil
	}
	gh, err := r.gh.resolve("gh", ErrSandboxPosture)
	if err != nil {
		return "", fmt.Errorf("%w: it is both §5.6's publish command and, under this posture, "+
			"the only credential source `git push` has", err)
	}
	return gh, nil
}

// Start launches one attempt. An error here means no process and no handle:
// the orchestrator never has to reason about a half-started run. Once Start
// returns a handle, every outcome arrives as a terminal event instead
// (SPEC §7.4).
//
// Cancelling ctx after a successful Start discards the run — the daemon is
// shutting down — equivalent to Stop(StopDiscard).
func (r *Runner) Start(ctx context.Context, spec core.RunSpec) (core.RunHandle, error) {
	// The provider config is the one bound at New. Nothing about this attempt
	// can change it (SPEC §7.1).
	p := r.provider
	if err := harness.CheckSpec(spec, specErrors); err != nil {
		return nil, err
	}
	if err := p.checkPrivateDir(spec); err != nil {
		return nil, err
	}
	if err := p.checkSandboxSpec(spec); err != nil {
		return nil, err
	}
	if spec.Continuation != "" && !r.Capabilities().Resume {
		// Unreachable today; the contract says an adapter without resume MUST
		// fail loudly rather than silently start a fresh session (SPEC §7.1).
		return nil, fmt.Errorf("claude-code: resume not supported but continuation token supplied")
	}

	// The path Ready resolved and verified, not the configured name. A caller
	// that skipped Ready resolves here instead — once, and identically.
	binary, err := r.resolveBinary()
	if err != nil {
		return nil, err
	}
	argv := p.command(spec)
	argv[0] = binary

	// Per attempt, before the environment is composed. Refusing here costs one
	// attempt; launching without it costs the whole ticket's work, discovered at
	// `git push`, which is what an absent env_passthrough name silently bought
	// before the credential was a named thing (SPEC §5.2.8).
	publish, err := harness.MintPublish(ctx, r.publish, r.attemptTimeout, ErrPublishCredential)
	if err != nil {
		return nil, err
	}

	// Before the environment names them, so a run never starts pointing at a
	// directory that does not exist. Under `inherit` this does nothing.
	if err := ensureHarnessDirs(p.harnessDirs(spec.Workspace.PrivateDir)); err != nil {
		return nil, err
	}

	// The posture is composed per attempt, because it names this attempt's
	// workspace: a settings file left from the previous one would bind a
	// different tree. Under `none` this does nothing and argv is unchanged.
	if p.SandboxMode == SandboxSRT {
		// Both resolved once and remembered, so the attempt runs the sandbox
		// Ready verified rather than whatever PATH answers now.
		sandbox, err := r.resolveSandbox()
		if err != nil {
			return nil, err
		}
		identity, err := daemonGitIdentity(ctx, r.timings)
		if err != nil {
			return nil, err
		}
		gh, err := r.resolveGH()
		if err != nil {
			return nil, err
		}
		paths := p.sandboxPathsFor(spec, binary, gh)
		if err := p.writeSandbox(paths, identity); err != nil {
			return nil, err
		}
		argv = p.sandboxCommand(sandbox, paths.Control, argv)
	}

	// One snapshot behind both: the environment the child gets and the values
	// kept out of its transcript cannot disagree (SPEC §10.3).
	env, undeclared := p.environ(publish, spec)
	if err := harness.CheckRedactableEnv(undeclared, ErrProviderValue); err != nil {
		return nil, err
	}
	redact := append(slices.Clone(r.redact), slices.Sorted(maps.Values(undeclared))...)

	transcript, promptSink, err := r.transcripts.Open(spec)
	if err != nil {
		return nil, fmt.Errorf("claude-code: %w", err)
	}
	return harness.Start(ctx, harness.Launch{
		Name:       KindName,
		Argv:       argv,
		Env:        env,
		Dir:        spec.Workspace.Path,
		Prompt:     spec.Prompt,
		Limits:     spec.Limits,
		Translate:  translate,
		Transcript: transcript,
		PromptSink: promptSink,
		Redact:     redact,
		OnRun:      harness.BindEvidence(r.onRun, spec),
		Timings:    r.timings,
		Signal:     r.signal,
	})
}

// specErrors names this adapter's refusals for the shared RunSpec checks
// (SPEC §7.6): each adapter keeps its own sentinels, only the reasoning is
// shared.
var specErrors = harness.SpecErrors{
	EnvNamespace:  ErrEnvNamespace,
	PromptEmpty:   ErrPromptEmpty,
	WorkspacePath: ErrWorkspacePath,
}
