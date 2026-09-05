package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The §2.2 process-level sandbox posture (#81).
//
// SPEC §2.2 assigns process-level sandboxing to the adapter-owned provider
// block, and until this key existed only codex-exec took the assignment. What
// makes it non-trivial here is that claude-code's *own* sandbox is not the
// thing to reach for: it isolates Bash commands and their children only, while
// Read, Edit, WebFetch, MCP servers and hooks run in-process on the host — and
// `permission_mode: bypassPermissions`, the mode a headless daemon wants, is
// exactly the one that removes the permission rules gating those. A key that
// flipped it on would read as a boundary and not be one.
//
// So the posture wraps the whole process in @anthropic-ai/sandbox-runtime
// (`srt`), which applies Seatbelt on darwin and bubblewrap on linux to the
// command and everything it spawns. BEN already owns argv, the child
// environment and cwd, so wrapping is a prefix.
//
// Per #51 the OS account is the boundary and stronger sandboxing is
// RECOMMENDED; this is that rung, and `none` is therefore the default. A
// default of `srt` would make every daemon that starts today refuse readiness
// after an upgrade, on a host that has never installed the runtime.
const (
	// SandboxNone runs the harness unwrapped: the behaviour that predates this
	// key, bounded by #51's OS account and nothing else.
	SandboxNone = "none"
	// SandboxSRT wraps the harness in `srt` under a settings file this adapter
	// composes whole — see sandboxSettings for what the posture states and
	// sandboxControl for the files it owns.
	SandboxSRT = "srt"
)

var sandboxModes = []string{SandboxNone, SandboxSRT}

// DefaultSandboxBinary is the sandbox runtime executable when the provider
// block omits one. Its own key exists for the same reason `binary` does: which
// path a per-host npm install lands on is not something a workflow file shared
// across hosts can assume.
const DefaultSandboxBinary = "srt"

// egressFloor is the allowlist the posture pins whatever the workflow says.
//
// These three are what the §5.6 workflow cannot function without, measured
// against srt 0.0.73: with `api.anthropic.com` alone the harness runs and
// reports alive, and `git push` plus `gh` need the other two (statsig and
// sentry are *not* required). An empty operator list therefore means "the
// floor", never "no network" — `allowedDomains: []` produces a harness that
// cannot reach the API at all, which reads as a working configuration and is
// not one. sandbox_domains may add to this; it may not remove from it.
var egressFloor = []string{"api.anthropic.com", "api.github.com", "github.com"}

// The child variables this posture owns, beyond #114's two.
//
// Every one of them exists because the sandbox denies `$HOME`, so a tool that
// resolves its configuration from there stops working — and the remedy that
// keeps the posture *stated* is for BEN to own that configuration rather than
// to carve a hole back into `$HOME`. Measured against srt 0.0.73:
//
//   - CLAUDE_CODE_TMPDIR: srt overrides the child's TMPDIR to `/tmp/claude`
//     whenever a filesystem policy is set, and honours this variable from its
//     own parent environment instead when present. Without it #114's
//     attempt-owned temp dir is silently replaced by a path shared across every
//     workspace — and `/tmp/claude` does not exist on a fresh host, so the
//     first temp write fails outright.
//   - GIT_CONFIG_GLOBAL / GIT_CONFIG_NOSYSTEM: git treats an unreadable
//     `~/.gitconfig` as fatal, and a host `url.<base>.insteadOf` rewrite
//     silently redirects an agent's HTTPS push to SSH. With both pinned the
//     only configuration the agent's git sees is BEN's file and the
//     repository's own.
//   - GH_CONFIG_DIR: `gh` refuses to start at all when it cannot read
//     `~/.config/gh/config.yml` ("failed to create root command"), which lands
//     on §6.7's publish step.
const (
	envSandboxTmpDir = "CLAUDE_CODE_TMPDIR"
	envGitConfig     = "GIT_CONFIG_GLOBAL"
	envGitNoSystem   = "GIT_CONFIG_NOSYSTEM"
	envGHConfigDir   = "GH_CONFIG_DIR"

	// ghConfigDirName is a sibling of #114's two directories rather than a child
	// of the control dir: `gh` writes its own config, and the control dir is
	// denied for writes.
	ghConfigDirName = "gh-config"
	// controlDirName holds what BEN writes *for* the agent and the agent must
	// not rewrite. A directory rather than a list of files, so a file added here
	// later is denied by construction instead of by remembering to extend a set
	// (the enumeration mistake #47 and #52 each paid for once).
	controlDirName = "sandbox"

	settingsFileName  = "settings.json"
	gitConfigFileName = "gitconfig"
	// gitIgnoreFileName is an empty file `core.excludesFile` points at. Without
	// it git warns on every invocation that it cannot read
	// `~/.config/git/ignore`, which is harmless and lands in every transcript.
	gitIgnoreFileName = "gitignore"
)

// sandboxControl is where the posture's own files live for one attempt. All of
// them are under the workspace's private dir, which the provider placed
// (SPEC §6.1) — this adapter may not derive it (SPEC §7.1).
type sandboxControl struct {
	// Dir is the denied-for-writes directory holding the three files below.
	Dir string
	// Settings is the composed srt settings file, passed as `srt -s`.
	Settings string
	// GitConfig is what GIT_CONFIG_GLOBAL points at.
	GitConfig string
	// GitIgnore is what core.excludesFile points at.
	GitIgnore string
	// GHConfigDir is what GH_CONFIG_DIR points at. Writable, unlike the rest.
	GHConfigDir string
}

func sandboxControlFor(private string) sandboxControl {
	dir := filepath.Join(private, controlDirName)
	return sandboxControl{
		Dir:         dir,
		Settings:    filepath.Join(dir, settingsFileName),
		GitConfig:   filepath.Join(dir, gitConfigFileName),
		GitIgnore:   filepath.Join(dir, gitIgnoreFileName),
		GHConfigDir: filepath.Join(private, ghConfigDirName),
	}
}

// sandboxSettings is the srt settings file, as a DTO rather than as whatever
// shape happens to be convenient: the file is a wire format read by another
// program, and the golden test asserts it whole.
//
// Every field is emitted, zero values included. An omitted key is not a neutral
// default — srt fills it from its own recommendations — which is the same
// reasoning codexexec.sandboxOverrides is built on: a setting is only stated if
// it is written down.
type sandboxSettings struct {
	Filesystem sandboxFilesystem `json:"filesystem"`
	Network    sandboxNetwork    `json:"network"`
	// WeakerNetworkIsolation is darwin's price for a working publish step, and
	// is pinned rather than exposed. Seatbelt otherwise breaks Go's macOS
	// platform verifier: measured on srt 0.0.73, `gh api rate_limit` fails with
	// `tls: failed to verify certificate: x509: OSStatus -26276` without it and
	// returns 5000 with it, while `curl` succeeds either way. It is not free —
	// srt's own documentation calls it "a potential data exfiltration vector
	// through the trustd service" — and that is the stated cost of running a Go
	// CLI at all, since §5.6's publish snippet requires `gh`. On linux Go reads
	// file roots and the flag is a no-op, so it is pinned false there.
	WeakerNetworkIsolation bool `json:"enableWeakerNetworkIsolation"`
	// AppleEvents is pinned false and is the sharpest of these: the runtime
	// documents it as removing "code-execution isolation", since a sandboxed
	// command can launch other applications *unsandboxed* with no prompt. A
	// posture that left it to a default would be one an `osascript` call walks
	// out of.
	AppleEvents bool `json:"allowAppleEvents"`
	// Pty is pinned false because nothing here needs one: the harness runs with
	// -p and piped stdio (SPEC §7.2), and a pty is an escape route for terminal
	// injection into whatever is reading the stream.
	Pty bool `json:"allowPty"`
	// WeakerNestedSandbox is the Docker-environment relaxation. BEN runs the
	// harness directly under a systemd unit (deploy/ben.service), so nothing
	// here needs it; it is named because "weaker" is not something to arrive at
	// by inheriting a default.
	WeakerNestedSandbox bool `json:"enableWeakerNestedSandbox"`
}

type sandboxFilesystem struct {
	AllowRead  []string `json:"allowRead"`
	DenyRead   []string `json:"denyRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite"`
	// AllowGitConfig is the runtime's own knob for `.git/config` writes, pinned
	// false. denyWrite already names the shared config, and this is the same
	// refusal said in the runtime's vocabulary: a posture that stated one and
	// left the other to a default would depend on which of the two the runtime
	// consults first.
	AllowGitConfig bool `json:"allowGitConfig"`
	// Disabled turns the whole filesystem policy off — in the runtime's words,
	// "no read or write rules are emitted". Every path above becomes decoration
	// if it is true, which makes it the one key in this file whose default
	// matters most and the one an omission is least visible in.
	Disabled bool `json:"disabled"`
}

// sandboxNetwork pins the whole network policy, not only the allowlist.
//
// The keys below are the ones whose defaults are security decisions, and
// leaving them out would make this file's own claim — that an omitted key is
// not a neutral default — false about itself.
type sandboxNetwork struct {
	AllowedDomains []string `json:"allowedDomains"`
	DeniedDomains  []string `json:"deniedDomains"`
	// StrictAllowlist makes allowedDomains policy rather than, in the runtime's
	// own words, "a prompt-suppression hint": without it a host outside the list
	// is referred to an ask callback. An unattended daemon has nobody to ask
	// (SPEC §10.1), so the question has to be already answered.
	StrictAllowlist bool `json:"strictAllowlist"`
	// AllowLocalBinding and the two socket keys close the paths that are not
	// domains at all. Egress allowlisting is a setting rather than a boundary
	// (SPEC §10.1) precisely because it matches a client-supplied hostname; a
	// local listener or a unix socket does not pass through that check.
	AllowLocalBinding   bool     `json:"allowLocalBinding"`
	AllowAllUnixSockets bool     `json:"allowAllUnixSockets"`
	AllowUnixSockets    []string `json:"allowUnixSockets"`
	// AllowMachLookup is the darwin IPC surface: a mach service is a way out of
	// the sandbox that is neither a domain nor a path, so an inherited list is
	// an inherited hole. Empty is additive-from-nothing, not a reset of the
	// profile's own rules — verified against the runtime, since an empty list
	// that *replaced* them would break the sandbox rather than tighten it.
	AllowMachLookup []string `json:"allowMachLookup"`
}

// sandboxPaths is everything the composed posture binds, gathered so the
// composition below stays a pure function of it.
type sandboxPaths struct {
	// Workspace, SharedGitDir and PrivateDir are the provider's (SPEC §6.1),
	// carried on the RunSpec. The shared git dir in particular is *reported*
	// rather than discovered: `git rev-parse --git-common-dir` reads it out of
	// `<workspace>/.git`, which srt leaves writable by design ("Excludes .git
	// since we need it writable for git operations"), so an adapter that
	// discovered it could be handed a repository the agent chose — and §6.2
	// reattaches, so a rewritten pointer survives into the next attempt.
	Workspace    string
	SharedGitDir string
	PrivateDir   string
	// WorktreeAdmin is this workspace's one linked-worktree administrative
	// directory. Git's pointer names the candidate, but it is accepted only as a
	// real direct child of the provider-reported SharedGitDir and only when its
	// two pointers resolve back to those reported paths. It therefore selects a
	// narrower capability inside a trusted root; it never selects the root.
	WorktreeAdmin string
	// SharedGitWriteDirs is the closed set of common subdirectories a normal
	// linked-worktree commit mutates. Each is checked as a real directory before
	// it reaches the settings file. Keeping the shared root and its `worktrees`
	// child out of this set is what makes later worktree registrations read-only
	// to a sandbox that was composed before they existed.
	SharedGitWriteDirs []string
	// SharedGitReadFiles is the closed set of shared-repository files Git needs in
	// addition to the mutable subdirectories above. The shared root itself must
	// not enter allowRead: Linux srt emits that parent read-only bind after the
	// child write binds, which would mask every one of them.
	SharedGitReadFiles []string
	// Binary is the absolute path Start launches, and Canonical is that path
	// with symlinks resolved. Both are named: srt normalizes settings entries,
	// so on this repo's own install (`~/.local/bin/claude` →
	// `~/.local/share/claude/versions/<v>`) naming either one alone is enough —
	// but naming the canonical path *and* its directory is what covers an
	// install whose entry point is a script beside its package.
	Binary    string
	Canonical string
	// GH is the resolved `gh`, "" when no publish credential is configured and
	// therefore no credential helper is written. It is both an executable the
	// posture must permit and the value the helper line names.
	GH      string
	Control sandboxControl
}

// sandboxSettings composes the whole settings file. Pure — goos and home are
// parameters rather than reads — so the golden test asserts the same function
// the run uses, on every platform.
//
// The read policy is the half that is easy to get wrong, and the posted
// fixtures on #81 got it wrong first: with `denyRead: []` the posture is
// defense-in-depth only, and §10.1's protected-mode outcome is not reached at
// all. Denying `$HOME` reaches it — `~/.ssh/id_ed25519` and `~/.gitconfig` both
// answer "Operation not permitted" — at the cost of the carve-outs below, each
// of which was measured to be load-bearing rather than assumed.
//
// Two things the posture does NOT claim, stated here because a posture oversold
// is worse than one absent:
//
//   - The built-in write set is not replaced. srt adds `/tmp/claude`,
//     `/private/tmp/claude`, `~/.claude/debug` and `~/.npm/_logs` whatever this
//     file says. CLAUDE_CODE_TMPDIR changes the child's TMPDIR, but does not
//     remove either fixed temp grant. Kind.LocalWrites reports all four
//     directory trees so daemon scratch cannot be placed beneath them.
//   - Egress allowlisting is a setting, not a boundary: the proxy matches the
//     client-supplied hostname without terminating TLS (SPEC §10.1's design
//     note). srt 0.0.73 does offer `tlsTerminate`, which would make it more
//     than that, but enabling it would break every Go client that cannot be
//     told to trust a private CA — the same failure the darwin pin exists to
//     avoid.
func (p Provider) sandboxSettings(paths sandboxPaths, goos, home string) sandboxSettings {
	// The common repository is deliberately not a writable root. On Linux every
	// deny is a read-only bind mount; if its writable parent can be renamed, the
	// mount moves with the old directory and the denied name can be recreated.
	// Exact mutable roots leave `base.git/worktrees` read-only while preserving
	// objects, refs, reflogs and this attempt's index/HEAD state.
	write := []string{paths.Workspace, paths.PrivateDir}
	write = append(write, paths.SharedGitWriteDirs...)
	if paths.WorktreeAdmin != "" {
		write = append(write, paths.WorktreeAdmin)
	}
	// allowWrite does not imply read, and a `/tmp` fixture hides it: under
	// `denyRead: [$HOME]` with Git's mutable roots writable but its common
	// metadata not readable, git reports
	// "not a git repository: …/worktrees/<key>". SPEC §5.2.4's default workspace
	// root is under `$HOME`, so this is load-bearing rather than academic.
	read := append(slices.Clone(write), paths.SharedGitReadFiles...)
	read = append(read, paths.Binary, paths.Canonical, filepath.Dir(paths.Canonical))
	if paths.GH != "" {
		// The credential helper runs it, and an install under the denied $HOME
		// would otherwise be unexecutable — the same trap the harness binary's
		// carve-out exists for.
		read = append(read, paths.GH, filepath.Dir(paths.GH))
	}
	// add_dirs grants the harness tool access outside the workspace (--add-dir);
	// a posture that denied what that key granted would be two configurations
	// disagreeing about the same boundary.
	read = append(read, p.AddDirs...)
	write = append(write, p.AddDirs...)
	denyWrite := []string{
		// Hooks and config are code and configuration the *next* run
		// executes, in a directory shared by every workspace of this
		// workflow.
		filepath.Join(paths.SharedGitDir, "hooks"),
		filepath.Join(paths.SharedGitDir, "config"),
		// The gitdir pointer: writable under srt by design, and §6.2
		// reattaches, so an agent that rewrote it would choose the
		// repository the *next* attempt puts in allowWrite.
		filepath.Join(paths.Workspace, ".git"),
		// BEN's own control files — an agent that can rewrite its git
		// config can restore an `insteadOf` rewrite and redirect its next
		// push, and one that can rewrite this settings file chooses its
		// own sandbox.
		paths.Control.Dir,
	}
	if paths.WorktreeAdmin != "" {
		// These stable pointer/config files are not part of a commit's mutable
		// state. commondir relocates the config, hooks, refs and objects Git
		// uses; gitdir points back at the worktree and is consumed by Git's
		// worktree administration; config.worktree can steer Git when the common
		// config enables worktreeConfig. The rest stays writable: index.lock,
		// index and logs/HEAD are genuinely rewritten by a commit.
		denyWrite = append(denyWrite,
			filepath.Join(paths.WorktreeAdmin, "commondir"),
			filepath.Join(paths.WorktreeAdmin, "config.worktree"),
			filepath.Join(paths.WorktreeAdmin, "gitdir"),
		)
	}

	return sandboxSettings{
		Filesystem: sandboxFilesystem{
			AllowRead: dedupe(read),
			DenyRead:  []string{home},
			// denyWrite beats allowWrite, which is what lets the private dir be
			// writable while the control dir inside it is not.
			AllowWrite: dedupe(write),
			Disabled:   false,
			DenyWrite:  dedupe(denyWrite),
		},
		Network: sandboxNetwork{
			AllowedDomains:      dedupe(append(slices.Clone(egressFloor), p.SandboxDomains...)),
			DeniedDomains:       []string{},
			StrictAllowlist:     true,
			AllowLocalBinding:   false,
			AllowAllUnixSockets: false,
			AllowUnixSockets:    []string{},
			AllowMachLookup:     []string{},
		},
		WeakerNetworkIsolation: goos == "darwin",
		AppleEvents:            false,
		Pty:                    false,
		WeakerNestedSandbox:    false,
	}
}

// dedupe sorts and removes duplicates, so the composed file is a deterministic
// function of its inputs — a golden the review can read, and a settings file
// that does not churn between attempts.
func dedupe(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// sandboxEnv is the posture's contribution to the child environment. Empty
// under `none`, which is what keeps that posture the absence of this one.
func (p Provider) sandboxEnv(private string) map[string]string {
	if p.SandboxMode != SandboxSRT || private == "" {
		return nil
	}
	c := sandboxControlFor(private)
	return map[string]string{
		// The same directory #114 pins TMPDIR to. Both are set: TMPDIR for the
		// srt process itself, this one because srt overwrites the child's TMPDIR
		// unless it is told where to point it.
		envSandboxTmpDir: filepath.Join(private, tmpDirName),
		envGitConfig:     c.GitConfig,
		envGitNoSystem:   "1",
		envGHConfigDir:   c.GHConfigDir,
	}
}

// command wraps the harness argv in the sandbox runtime. A prefix, because BEN
// already owns argv, the child environment and cwd.
//
// `srt <argv...>` rather than `srt -c '<string>'`: the argv form shell-quotes
// each element before the `bash -c` it runs internally (measured — `$HOME`, `*`
// and `;rm` all arrive literal), while `-c` is documented as "no escaping
// applied". The prompt never travels here in either case; it is stdin.
func (p Provider) sandboxCommand(sandboxBinary string, control sandboxControl, argv []string) []string {
	wrapped := []string{sandboxBinary, "-s", control.Settings, "--"}
	return append(wrapped, argv...)
}

// probeFunc runs one readiness probe against the harness, wrapped or not
// according to the posture. Ready composes it once so the two probes cannot
// disagree about which of the two they are checking.
type probeFunc func(ctx context.Context, t harness.Timings, env []string, args ...string) ([]byte, error)

// probeCommand builds that function. Under `none` it is harness.Probe
// unchanged; under `srt` it is the same probe behind the same wrapper a run
// gets, over the throwaway private dir Ready supplies.
func (p Provider) probeCommand(sandboxBinary, private, path string) probeFunc {
	if p.SandboxMode != SandboxSRT {
		return func(ctx context.Context, t harness.Timings, env []string, args ...string) ([]byte, error) {
			return harness.Probe(ctx, t, path, env, args...)
		}
	}
	control := sandboxControlFor(private)
	return func(ctx context.Context, t harness.Timings, env []string, args ...string) ([]byte, error) {
		wrapped := append(p.sandboxCommand(sandboxBinary, control, []string{path}), args...)
		return harness.Probe(ctx, t, wrapped[0], env, wrapped[1:]...)
	}
}

// probeEnforcement is the half of readiness that asks whether the runtime
// enforces anything at all.
//
// The egress probe below cannot answer it: a wrapper that loaded the settings
// and simply executed the child would pass `--version`, `auth status` and
// `gh api` alike, while leaving `$HOME` fully readable — a posture reported as
// delivered and doing nothing. That is not a hypothetical shape. The runtime
// strips settings keys it does not recognize rather than refusing them, so a
// build whose filesystem schema has moved produces exactly that, and
// `sandbox_binary` is an operator-supplied path besides.
//
// Two negative probes, one per policy, because they fail independently: a
// runtime honouring denyWrite and ignoring denyRead is a posture that keeps the
// agent out of its own hooks and hands it the operator's ssh key. Both use
// paths the posture already names, so neither invents a rule the settings file
// does not state — and both must *fail* for readiness to pass.
func (p Provider) probeEnforcement(ctx context.Context, t harness.Timings, sandboxBinary, private string, env []string) error {
	if p.SandboxMode != SandboxSRT {
		return nil
	}
	control := sandboxControlFor(private)
	// Inside the control dir rather than at one of its real files: a runtime
	// that does not enforce would otherwise corrupt the settings it is being
	// asked about.
	denied := filepath.Join(control.Dir, "enforcement-probe")
	// Under the denied $HOME, so the read probe tests denyRead and not merely a
	// missing file — an absent path fails for the wrong reason and would pass
	// this check on a runtime enforcing nothing.
	home, err := daemonHome()
	if err != nil {
		return err
	}
	sentinel, err := os.CreateTemp(home, ".ben-sandbox-readiness-*")
	if err != nil {
		return fmt.Errorf("%w: cannot place a file to prove the read policy with: %v", ErrSandbox, err)
	}
	defer os.Remove(sentinel.Name())
	if _, err := sentinel.WriteString("ben-readiness-sentinel\n"); err != nil {
		sentinel.Close()
		return fmt.Errorf("%w: %v", ErrSandbox, err)
	}
	sentinel.Close()

	for _, probe := range []struct{ what, script string }{
		{"write to a denied path", "echo probe > " + shellQuote(denied)},
		// A shell redirection rather than `cat`: the question is whether *open*
		// is refused, and `cat` answers it only incidentally. A `cat` that is
		// missing, or that fails for a reason of its own, exits non-zero on a
		// runtime enforcing nothing and would read here as a policy that holds.
		{"read a file under the denied home directory", "read -r _ < " + shellQuote(sentinel.Name())},
	} {
		argv := append(p.sandboxCommand(sandboxBinary, control, []string{"/bin/sh"}), "-c", probe.script)
		if _, err := harness.ProbeCombined(ctx, t, argv[0], env, argv[1:]...); err == nil {
			return fmt.Errorf("%w: the runtime let a sandboxed command %s, so the posture is "+
				"composed and not enforced — every denial this adapter states is decoration "+
				"(check that %s is the real sandbox runtime and understands this settings schema)",
				ErrSandboxPosture, probe.what, sandboxBinary)
		}
	}
	// A denial that also refuses the *allowed* case proves nothing, so one
	// positive control: the same shell, writing where the posture permits.
	allowed := filepath.Join(private, "enforcement-control")
	argv := append(p.sandboxCommand(sandboxBinary, control, []string{"/bin/sh"}),
		"-c", "echo probe > "+shellQuote(allowed))
	if out, err := harness.ProbeCombined(ctx, t, argv[0], env, argv[1:]...); err != nil {
		return fmt.Errorf("%w: the runtime refused a write the posture allows, so the two "+
			"refusals above say nothing about the policy: %v: %s",
			ErrSandboxPosture, err, harness.Excerpt(strings.TrimSpace(string(out)), harness.ProbeExcerpt))
	}
	return os.Remove(allowed)
}

// probeSandboxPaths shapes a posture for readiness, when no workspace exists.
//
// The three provider paths collapse onto the throwaway private dir. That is not
// the posture a run gets — it cannot be, since the workspace is created at
// dispatch — but it is the same *shape*, and the capability under test here is
// the platform's, not the workspace's.
func (p Provider) probeSandboxPaths(private, binary, gh string) sandboxPaths {
	return p.baseSandboxPaths(core.RunSpec{Workspace: core.WorkspacePaths{
		Path: private, SharedGitDir: private, PrivateDir: private,
	}}, binary, gh)
}

// probeEgress is the behavioural half of readiness (#81 F3′): compose the real
// posture and make one Go-client TLS request through it.
//
// Neither the settings file nor the runtime's version can answer this. srt
// strips unknown settings keys silently instead of refusing them, so a file
// carrying `enableWeakerNetworkIsolation` on a runtime that predates it
// produces a sandbox that looks configured; and `srt --version` reports 1.0.0
// while the package is 0.0.73, so the version is not a gate either. What is
// left is running it, which costs about 0.6s.
//
// `gh` is the probe binary because §5.6's publish snippet already requires it,
// and because it is the client that fails: Seatbelt breaks Go's macOS platform
// verifier, so without the darwin pin the run works, commits, pushes — and
// cannot open a PR. Refusing at startup is the whole point.
//
// Both binaries arrive resolved rather than being looked up here, and `gh` is
// the one that matters: the credential helper this same Ready wrote into the
// settings file names the memoized path, so a fresh lookup could certify one
// `gh` while every attempt runs another — a Ready/Start divergence reached
// inside a single Ready, which no amount of changing PATH afterwards would
// show.
//
// Unreachable with no publish credential under this posture (ErrSandboxPublish
// refuses that at load); the guard stays because the value is resolved per
// attempt and a variable named with nothing behind it leaves `gh`
// unauthenticated, which this probe cannot tell from a broken posture.
func (p Provider) probeEgress(ctx context.Context, t harness.Timings, sandboxBinary, gh, private string, env []string, publish harness.PublishValue) error {
	if p.SandboxMode != SandboxSRT || gh == "" || publish.Env == "" || publish.Value == "" {
		return nil
	}
	argv := append(p.sandboxCommand(sandboxBinary, sandboxControlFor(private), []string{gh}),
		"api", "rate_limit", "--jq", ".rate.limit")
	out, err := harness.ProbeCombined(ctx, t, argv[0], env, argv[1:]...)
	if err == nil {
		return nil
	}
	// The output, not the exit code, because the exit code is 1 for every
	// failure this can have and an operator needs to know which one. It carries
	// no credential: `gh api` reports the URL and the transport error.
	return fmt.Errorf("%w: a Go client cannot reach api.github.com under this posture, so a run "+
		"would commit and push and then fail to open a PR (§6.7): %v: %s",
		ErrSandboxPosture, err, harness.Excerpt(strings.TrimSpace(string(out)), harness.ProbeExcerpt))
}

// gitConfigFile is the global git configuration BEN owns under this posture.
//
// The identity is carried from the daemon's own git configuration rather than
// invented: §6.7 has BEN publishing as whoever runs the daemon, and a synthetic
// author would change who the commits are from. It is *stated* here rather than
// inherited — the file says what the run will use — which is the difference
// between reading the host's configuration once and letting the agent's git
// resolve whatever `$HOME` happens to hold.
//
// The credential helper is what makes §5.6's `git push origin HEAD` possible
// at all. Measured: with GIT_CONFIG_NOSYSTEM set and this file in place, git
// has no credential source — `git credential fill` for github.com answers
// "could not read Username" — because the origin URL is deliberately
// credential-free (§6.7, workspace.go) and git does not read GH_TOKEN. So the
// posture would commit, and fail to publish. `gh auth git-credential` is the
// mechanism `gh auth setup-git` itself installs, it reads the same token this
// adapter already injects for `gh pr create`, and it puts no secret in this
// file.
//
// Unscoped rather than keyed to https://github.com: BEN's tracker supports
// GitHub Enterprise (WithEnterpriseURLs), so a github.com-only helper would
// leave every Enterprise deployment unable to push. `gh` answers nothing for a
// host it holds no token for, which is what makes the wider scope safe.
//
// Automatic maintenance is disabled here because the agent's git does not pass
// through gitcmd.Argv. The deliberately narrow shared-repository write set has
// no common-root lock files, so either modern `git maintenance --auto` or the
// legacy auto-gc path would eventually turn a successful commit into a warning
// or failure as the repository grows.
func gitConfigFile(identity gitIdentity, excludes, ghBinary string) string {
	var b strings.Builder
	b.WriteString("# Written by BEN for one workspace (SPEC §6.2, #81). GIT_CONFIG_GLOBAL points\n")
	b.WriteString("# here and GIT_CONFIG_NOSYSTEM is set, so this and the repository's own config\n")
	b.WriteString("# are the whole of what the agent's git reads.\n")
	b.WriteString("[user]\n")
	fmt.Fprintf(&b, "\tname = %s\n", gitConfigValue(identity.Name))
	fmt.Fprintf(&b, "\temail = %s\n", gitConfigValue(identity.Email))
	b.WriteString("[core]\n")
	fmt.Fprintf(&b, "\texcludesFile = %s\n", gitConfigValue(excludes))
	b.WriteString("[gc]\n")
	b.WriteString("\tauto = 0\n")
	b.WriteString("[maintenance]\n")
	b.WriteString("\tauto = false\n")
	if ghBinary != "" {
		b.WriteString("[credential]\n")
		// The empty value resets any helper list inherited from an earlier file,
		// exactly as `gh auth setup-git` writes it.
		b.WriteString("\thelper =\n")
		fmt.Fprintf(&b, "\thelper = %s\n", gitConfigValue("!"+shellQuote(ghBinary)+" auth git-credential"))
	}
	return b.String()
}

// gitConfigValue encodes one value for a git config file.
//
// Raw interpolation is not safe here and the values are not hypothetical: a
// `#` in a path truncates the line as a comment, a `"` silently changes where
// the value starts and ends, a `\` escapes the next character, and a newline
// ends the entry — which for a value that reaches this file from `git config
// --get` on the daemon's host means the host decides what BEN's config
// contains. Quoting and escaping is what git's own parser undoes exactly
// (git-config(1) "Syntax"), so the value round-trips.
func gitConfigValue(v string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\t", `\t`,
	)
	return `"` + r.Replace(v) + `"`
}

// shellQuote wraps a path for the `!<command>` form of credential.helper, which
// git hands to a shell. Single quotes, because inside them a shell interprets
// nothing at all — and a literal single quote is spliced out and back in, the
// only escape that form has.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// writeSandbox lays down the control directory for one attempt: the settings
// file srt reads and the git configuration the agent's git reads.
//
// Replaced at every Start rather than created once. The settings file names
// paths that are only correct for this attempt's workspace, and a stale one
// would be a posture nobody composed.
func (p Provider) writeSandbox(paths sandboxPaths, identity gitIdentity) error {
	if err := os.MkdirAll(paths.Control.Dir, 0o700); err != nil {
		return fmt.Errorf("%w: creating the sandbox control dir %s: %v", ErrSandbox, paths.Control.Dir, err)
	}
	if err := os.MkdirAll(paths.Control.GHConfigDir, 0o700); err != nil {
		return fmt.Errorf("%w: creating %s at %s: %v", ErrSandbox, envGHConfigDir, paths.Control.GHConfigDir, err)
	}
	home, err := daemonHome()
	if err != nil {
		return err
	}
	settings, err := json.MarshalIndent(p.sandboxSettings(paths, runtime.GOOS, home), "", "  ")
	if err != nil {
		return fmt.Errorf("%w: composing the sandbox settings: %v", ErrSandbox, err)
	}
	for _, f := range []struct{ path, content string }{
		{paths.Control.Settings, string(settings) + "\n"},
		{paths.Control.GitConfig, gitConfigFile(identity, paths.Control.GitIgnore, paths.GH)},
		{paths.Control.GitIgnore, ""},
	} {
		if err := os.WriteFile(f.path, []byte(f.content), 0o600); err != nil {
			return fmt.Errorf("%w: writing %s: %v", ErrSandbox, f.path, err)
		}
	}
	return nil
}

// sandboxPathsFor gathers what the posture binds for one attempt. The shared
// repository comes only from the provider (SPEC §7.1); Git's pointer can select
// one direct child within it only after the reciprocal pointer pair is proved.
func (p Provider) sandboxPathsFor(spec core.RunSpec, binary, gh string) (sandboxPaths, error) {
	paths := p.baseSandboxPaths(spec, binary, gh)
	admin, err := linkedWorktreeAdmin(spec.Workspace)
	if err != nil {
		return sandboxPaths{}, err
	}
	if err := ensureEmptyWorktreeConfig(admin); err != nil {
		return sandboxPaths{}, err
	}
	writeDirs, err := sharedGitWriteDirs(spec.Workspace.SharedGitDir)
	if err != nil {
		return sandboxPaths{}, err
	}
	readFiles, err := sharedGitReadFiles(spec.Workspace.SharedGitDir)
	if err != nil {
		return sandboxPaths{}, err
	}
	paths.WorktreeAdmin = admin
	paths.SharedGitWriteDirs = writeDirs
	paths.SharedGitReadFiles = readFiles
	return paths, nil
}

func (p Provider) baseSandboxPaths(spec core.RunSpec, binary, gh string) sandboxPaths {
	// EvalSymlinks failing is not a refusal: the path came from ResolveBinary,
	// which established it exists, and naming it twice costs nothing.
	canonical, err := filepath.EvalSymlinks(binary)
	if err != nil {
		canonical = binary
	}
	return sandboxPaths{
		Workspace:    spec.Workspace.Path,
		SharedGitDir: spec.Workspace.SharedGitDir,
		PrivateDir:   spec.Workspace.PrivateDir,
		Binary:       binary,
		Canonical:    canonical,
		GH:           gh,
		Control:      sandboxControlFor(spec.Workspace.PrivateDir),
	}
}

const maxGitPointerBytes = 16 << 10

// linkedWorktreeAdmin accepts the workspace's pointer only as a selector inside
// the provider-reported shared repository. That distinction is the trust
// boundary: a rewritten pointer can make the attempt fail, but cannot add an
// arbitrary path to allowWrite. Every accepted pointer is canonical too. A
// path through a writable symlink that merely resolves correctly now could be
// retargeted after the posture starts and would move Git without changing the
// denied pointer file itself.
func linkedWorktreeAdmin(paths core.WorkspacePaths) (string, error) {
	worktrees := filepath.Join(paths.SharedGitDir, "worktrees")
	if err := requireRealDir(worktrees, "linked-worktree registry"); err != nil {
		return "", err
	}

	dotGit := filepath.Join(paths.Path, ".git")
	admin, err := readGitPath(dotGit, "gitdir: ")
	if err != nil {
		return "", err
	}
	if err := requireRealDir(admin, "linked-worktree admin dir"); err != nil {
		return "", err
	}
	resolvedAdmin, err := resolvedPath(admin)
	if err != nil {
		return "", fmt.Errorf("%w: resolving the linked-worktree admin dir: %v", ErrSandbox, err)
	}
	registry, err := resolvedPath(worktrees)
	if err != nil {
		return "", fmt.Errorf("%w: resolving the linked-worktree registry: %v", ErrSandbox, err)
	}
	if admin != resolvedAdmin {
		return "", fmt.Errorf("%w: the workspace gitdir uses a non-canonical path", ErrSandbox)
	}
	if filepath.Dir(resolvedAdmin) != registry {
		return "", fmt.Errorf("%w: the workspace gitdir is not a direct child of the reported shared git dir", ErrSandbox)
	}

	common, err := readGitPath(filepath.Join(resolvedAdmin, "commondir"), "")
	if err != nil {
		return "", err
	}
	back, err := readGitPath(filepath.Join(resolvedAdmin, "gitdir"), "")
	if err != nil {
		return "", err
	}
	shared, err := resolvedPath(paths.SharedGitDir)
	if err != nil {
		return "", fmt.Errorf("%w: resolving the reported shared git dir: %v", ErrSandbox, err)
	}
	resolvedDotGit, err := resolvedPath(dotGit)
	if err != nil {
		return "", fmt.Errorf("%w: resolving the workspace gitdir file: %v", ErrSandbox, err)
	}
	if common != shared {
		return "", fmt.Errorf("%w: worktree commondir is not the canonical reported shared git dir", ErrSandbox)
	}
	if back != resolvedDotGit {
		return "", fmt.Errorf("%w: worktree gitdir is not the canonical reported workspace", ErrSandbox)
	}
	return resolvedAdmin, nil
}

// ensureEmptyWorktreeConfig gives Linux srt an existing file to mount
// read-only. Its bubblewrap backend cannot create a missing deny target after
// this patch makes the admin directory's parent read-only. A retained config is
// not repaired: non-empty content is executable/configuration steering state
// from an earlier attempt, so the next run refuses it.
func ensureEmptyWorktreeConfig(admin string) error {
	path := filepath.Join(admin, "config.worktree")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err == nil {
		if err := f.Close(); err != nil {
			return fmt.Errorf("%w: creating the empty per-worktree Git config: %v", ErrSandbox, err)
		}
		return nil
	}
	if !os.IsExist(err) {
		return fmt.Errorf("%w: creating the empty per-worktree Git config: %v", ErrSandbox, err)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: inspecting the per-worktree Git config: %v", ErrSandbox, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != 0 {
		return fmt.Errorf("%w: per-worktree Git config is not an empty regular file", ErrSandbox)
	}
	if !fileHasOneLink(info) {
		return fmt.Errorf("%w: per-worktree Git config has another hard link", ErrSandbox)
	}
	return nil
}

// sharedGitWriteDirs returns only fixed Git stores, never the shared root or
// its worktree registry. `logs` is created before srt starts because the first
// commit creates the branch reflog, while Linux srt skips an allowWrite path
// that does not exist when its mount namespace is assembled.
func sharedGitWriteDirs(shared string) ([]string, error) {
	logs := filepath.Join(shared, "logs")
	if err := os.Mkdir(logs, 0o755); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("%w: creating the shared Git reflog dir: %v", ErrSandbox, err)
	}

	var dirs []string
	for _, name := range []string{"objects", "refs", "logs", "reftable"} {
		dir := filepath.Join(shared, name)
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) && name == "reftable" {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspecting shared Git %s dir: %v", ErrSandbox, name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("%w: shared Git %s path is not a real directory", ErrSandbox, name)
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

// sharedGitReadFiles exposes Git's required shared metadata without a parent
// bind over the mutable children. `info/exclude` is read by ordinary status and
// add operations even when empty. Optional packed-refs is included only when
// present; a later daemon-side pack cannot turn an older sandbox's fixed
// settings into a new read grant.
func sharedGitReadFiles(shared string) ([]string, error) {
	var files []string
	for _, entry := range []struct {
		name     string
		required bool
	}{
		{"config", true},
		{"HEAD", true},
		{filepath.Join("info", "exclude"), true},
		{"packed-refs", false},
	} {
		path := filepath.Join(shared, entry.name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && !entry.required {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("%w: inspecting shared Git %s: %v", ErrSandbox, entry.name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("%w: shared Git %s is not a regular file", ErrSandbox, entry.name)
		}
		if !fileHasOneLink(info) {
			return nil, fmt.Errorf("%w: shared Git %s has another hard link", ErrSandbox, entry.name)
		}
		files = append(files, path)
	}
	return files, nil
}

func requireRealDir(path, what string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: inspecting %s: %v", ErrSandbox, what, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%w: %s is not a real directory", ErrSandbox, what)
	}
	return nil
}

// readGitPath reads one of Git's one-line path files without letting corrupt
// retained state allocate an unbounded buffer. The contents are never included
// in an error because a prior attempt could have authored them.
func readGitPath(path, prefix string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("%w: inspecting Git path file %s: %v", ErrSandbox, filepath.Base(path), err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: Git path file %s is not a regular file", ErrSandbox, filepath.Base(path))
	}
	// A read-only mount protects one name, not every name for the same inode.
	// Refuse a retained hard link before an agent can rewrite this pointer
	// through an alias in its writable workspace.
	if !fileHasOneLink(info) {
		return "", fmt.Errorf("%w: Git path file %s has another hard link", ErrSandbox, filepath.Base(path))
	}
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%w: opening Git path file %s: %v", ErrSandbox, filepath.Base(path), err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxGitPointerBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: reading Git path file %s: %v", ErrSandbox, filepath.Base(path), err)
	}
	if len(raw) > maxGitPointerBytes {
		return "", fmt.Errorf("%w: Git path file %s is too large", ErrSandbox, filepath.Base(path))
	}
	value := strings.TrimSuffix(string(raw), "\n")
	value = strings.TrimSuffix(value, "\r")
	if prefix != "" {
		if !strings.HasPrefix(value, prefix) {
			return "", fmt.Errorf("%w: Git path file %s has an invalid prefix", ErrSandbox, filepath.Base(path))
		}
		value = strings.TrimPrefix(value, prefix)
	}
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", fmt.Errorf("%w: Git path file %s is not one path", ErrSandbox, filepath.Base(path))
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(filepath.Dir(path), value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("%w: resolving Git path file %s: %v", ErrSandbox, filepath.Base(path), err)
	}
	return filepath.Clean(abs), nil
}

func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(abs)
}

// checkSandboxMode refuses an unknown posture. The refused value travels as
// data (harness.ValueRefusal) like every other provider string, because
// provider values are $VAR-resolvable (SPEC §5.5) and `ben config effective`
// prints these refusals in CI (SPEC §5.8).
func checkSandboxMode(mode string) error {
	if slices.Contains(sandboxModes, mode) {
		return nil
	}
	return harness.ValueRefusal("sandbox_mode", mode,
		fmt.Errorf("%w: unknown posture (one of %s)", ErrSandboxMode, strings.Join(sandboxModes, ", ")))
}

// checkSandboxPostures refuses the three configurations in which `srt` is
// stated and cannot hold: an inherited config dir, no environment credential,
// and no publish block.
//
// All three are load-time facts about the file, so none waits for Ready.
func checkSandboxPostures(p Provider, publish core.PublishCredential) error {
	if p.SandboxMode != SandboxSRT {
		return nil
	}
	// The posture denies `$HOME`, so the operator's ~/.claude is unreadable and
	// their hooks would not run even if it were — but under `inherit` the
	// harness still resolves its config from there, and #81's own real-agent run
	// measured what that produces: every Bash call refused by a PreToolUse hook
	// the sandbox will not let start, an agent that reports tool failures and
	// stops. The writable set such a hook needs is a property of the operator's
	// tooling, not of this adapter, so it cannot be stated here — which is
	// exactly why #114's isolated config dir is the only coherent partner for
	// this posture.
	if p.ConfigDir != ConfigDirIsolated {
		// Anchored at config_dir because that is the key an operator edits — the
		// posture is the thing they asked for — but *both* full paths are named
		// in the message: a refusal reporting one field of a two-field
		// incompatibility reads as that field being wrong on its own.
		return harness.ValueRefusal("config_dir", p.ConfigDir, fmt.Errorf(
			"%w: agent.provider.sandbox_mode %s needs agent.provider.config_dir %s; under %s "+
				"the harness resolves its configuration from $HOME, which this posture denies, "+
				"and the operator's hooks become part of a writable set this adapter cannot "+
				"state (#81, #114)",
			ErrSandboxConfigDir, SandboxSRT, ConfigDirIsolated, p.ConfigDir))
	}
	// Denying `$HOME` denies `~/Library/Keychains`, and that is where an OAuth
	// session's credential lives — measured on claude 2.1.221: with `$HOME`
	// denied and no keychain carve-out the run reports "Not logged in", and
	// adding the keychain restores it. Granting it would hand the agent every
	// keychain item on the host to buy back one, which is a poor trade for a
	// sandbox; an environment credential needs no carve-out at all. So the
	// posture requires one, and says so here rather than at the first dispatch.
	if p.APIKey == "" && p.AuthToken == "" {
		return fmt.Errorf("%w: agent.provider.sandbox_mode %s denies $HOME, and an OAuth session "+
			"reads its credential from ~/Library/Keychains inside it; set "+
			"agent.provider.api_key or agent.provider.auth_token, which need no such carve-out",
			ErrSandboxCredential, SandboxSRT)
	}
	// SPEC §5.2.8 permits omitting the publish block, and says what that means:
	// "the agent authenticates from what §7.6's allowlist already carries —
	// HOME, and whatever the forge CLI stores under it". This posture denies
	// HOME and points the forge CLI at a directory BEN created, so it is
	// precisely the arrangement that omission relies on. Under `none` it still
	// works and is still permitted; here it is a run that commits and then has
	// nothing to publish with, which §6.7 discovers at the end of the attempt.
	if !publish.Configured() {
		return fmt.Errorf("%w: agent.provider.sandbox_mode %s denies $HOME and gives the forge "+
			"CLI a config directory of BEN's, so the credential an omitted `publish` block "+
			"relies on (SPEC §5.2.8) is unreachable; configure `publish`, or use "+
			"sandbox_mode %s", ErrSandboxPublish, SandboxSRT, SandboxNone)
	}
	return nil
}

// checkSandboxSpec refuses an `srt` run the provider reported no shared git dir
// for. Deriving one is forbidden (SPEC §7.1) and guessing one would put a
// repository nobody named into the sandbox grants, so a refusal is the only
// answer left — and without its mutable stores and fixed metadata `git commit`
// fails inside the workspace anyway, one attempt at a time.
func (p Provider) checkSandboxSpec(spec core.RunSpec) error {
	if p.SandboxMode != SandboxSRT || spec.Workspace.SharedGitDir != "" {
		return nil
	}
	return fmt.Errorf("%w: sandbox_mode %s must grant the required paths inside the shared git "+
		"dir — a linked worktree's .git is a pointer into it, so `git commit` fails without them — and this "+
		"adapter may not discover it from inside the worktree (SPEC §6.1, §7.1)",
		ErrSharedGitDir, SandboxSRT)
}

// daemonHome is the directory the read policy denies.
//
// Refused rather than defaulted, because every other answer is worse: an empty
// denyRead entry is a read policy that bounds nothing, and this posture's whole
// claim over #51's OS-account boundary is that it bounds reads. A daemon
// started without HOME is a real shape — a systemd unit without it — and one
// that composed an unbounded posture would report itself sandboxed and not be.
func daemonHome() (string, error) {
	if home := os.Getenv("HOME"); home != "" {
		return home, nil
	}
	return "", fmt.Errorf("%w: sandbox_mode %s bounds reads by denying the daemon account's "+
		"home directory, and HOME is unset, so there is nothing to deny; set HOME in the "+
		"daemon's environment, or use sandbox_mode %s", ErrSandbox, SandboxSRT, SandboxNone)
}

// gitIdentity is the author a sandboxed run commits as.
type gitIdentity struct{ Name, Email string }

// daemonGitIdentity reads the identity the daemon's own git is configured with.
//
// Read rather than invented, and refused rather than defaulted: with
// GIT_CONFIG_NOSYSTEM set and GIT_CONFIG_GLOBAL pointed at BEN's file, an
// identity absent from that file is absent altogether, and every `git commit`
// the agent runs fails with "unable to auto-detect email address" — a whole
// attempt's work, discovered at the commit. Ready asks this too, so a host with
// no identity is one loud refusal at startup instead.
func daemonGitIdentity(ctx context.Context, t harness.Timings) (gitIdentity, error) {
	git, err := harness.ResolveBinary("git", ErrSandbox)
	if err != nil {
		return gitIdentity{}, err
	}
	var id gitIdentity
	for _, f := range []struct {
		key   string
		field *string
	}{{"user.name", &id.Name}, {"user.email", &id.Email}} {
		// The daemon's own environment, deliberately: the question is what the
		// account BEN runs as is configured to commit as, which is a fact about
		// the host and not about the child's restricted environment.
		out, err := harness.Probe(ctx, t, git, os.Environ(), "config", "--get", f.key)
		if v := strings.TrimSpace(string(out)); err == nil && v != "" {
			*f.field = v
			continue
		}
		return gitIdentity{}, fmt.Errorf("%w: sandbox_mode %s replaces the global git "+
			"configuration, so the daemon account's own %s is what a run commits as, and this "+
			"host has none; set it (`git config --global %s ...`)",
			ErrSandboxIdentity, SandboxSRT, f.key, f.key)
	}
	return id, nil
}
