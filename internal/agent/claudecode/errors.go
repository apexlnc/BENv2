package claudecode

import "errors"

// Named refusals (see AGENTS.md conventions); tests assert on these.
//
// The split mirrors SPEC §7.1's two stages: Structural asks whether the provider
// block is well-formed (pure, no I/O, safe in `ben config effective`), and Ready
// asks whether the harness is usable here (it probes the binary and its
// credentials).
var (
	// ErrProviderKey refuses an unknown key in the agent.provider block. The
	// block is opaque to the core (SPEC §5.2.5) but closed to this adapter: a
	// silent `permision_mode:` typo would run with the wrong permissions.
	ErrProviderKey = errors.New("claude-code: unknown agent.provider key")

	// ErrProviderValue refuses a key whose value is the wrong shape or an
	// unusable member of a closed set.
	ErrProviderValue = errors.New("claude-code: invalid agent.provider value")

	// ErrPermissionMode refuses a missing or headless-unusable
	// permission_mode. There is deliberately no default: choosing one for the
	// operator would either grant unprompted tool access or hand the daemon a
	// mode that stalls on its first prompt (SPEC §7.4's stall window).
	ErrPermissionMode = errors.New("claude-code: agent.provider.permission_mode unusable")

	// ErrBinary refuses a harness binary that is absent, unresolvable, or does
	// not answer --version as Claude Code. Readiness, not structure: this is
	// what `Ready` catches so a dispatch never discovers it (SPEC §7.3).
	ErrBinary = errors.New("claude-code: harness binary unusable")

	// ErrExecutionDomain refuses local startup when the Linux containment and
	// quiet-evidence provider cannot prove its complete capability matrix.
	ErrExecutionDomain = errors.New("claude-code: local execution domain unavailable")

	// ErrCredentialPinned refuses a credential the host's managed settings will
	// not accept. Readiness, like ErrBinary, but a different fact: the harness
	// *has* a credential and reports itself logged in, and will refuse every
	// dispatch with it anyway (#112). Separate from ErrBinary because the two
	// need opposite advice — "set agent.provider.api_key" is the fix for one and
	// the cause of the other.
	ErrCredentialPinned = errors.New("claude-code: credential refused by the host's login policy")

	// ErrPromptEmpty refuses a run with no prompt. The template layer already
	// refuses an empty body (SPEC §5.3); this is the boundary restating it, so
	// a bug upstream cannot spend an agent turn on nothing.
	ErrPromptEmpty = errors.New("claude-code: empty prompt")

	// ErrWorkspacePath refuses a RunSpec without an absolute workspace path.
	// cwd is the only thing keeping the agent inside its worktree.
	ErrWorkspacePath = errors.New("claude-code: workspace path must be absolute")

	// ErrConfigDir refuses an unknown agent.provider.config_dir posture. The set
	// is closed for the same reason permission_mode's is: the two values differ
	// in which config directory — and therefore which credential and which hooks
	// — every run of this workflow uses, so a typo must not silently pick one.
	ErrConfigDir = errors.New("claude-code: agent.provider.config_dir unusable")

	// ErrPrivateDir refuses an isolated run whose RunSpec reports no private dir.
	// The provider places it (SPEC §6.1) and §7.1 forbids this adapter deriving
	// one, so there is nowhere to put the config dir — and the fallback the
	// harness would take, the operator's ~/.claude, is precisely what the
	// isolated posture exists to stop it reaching. A refusal costs one attempt;
	// inheriting by omission costs the posture.
	ErrPrivateDir = errors.New("claude-code: isolated config_dir needs the workspace's private dir")

	// ErrEnvReserved refuses a configuration in which two sites write one child
	// environment variable (SPEC §7.6): a provider block setting an
	// adapter-owned variable through the generic env surfaces instead of its
	// named key, a block respelling the variable `publish.env` names, or a
	// `publish.env` naming a variable this adapter owns. The credential a run
	// authenticates with is what Ready probes, and a value arriving as freeform
	// environment is one Ready never saw.
	ErrEnvReserved = errors.New("claude-code: reserved child environment variable set outside its owning config site")

	// ErrPublishCredential refuses a publish credential that cannot be resolved
	// for this attempt (SPEC §5.2.8). Readiness, not structure: the workflow
	// named a variable and the host does not hold it, which is deliberately not a
	// load error — the file has to load in a CI that holds no publish secret
	// (SPEC §5.5).
	ErrPublishCredential = errors.New("claude-code: publish credential unresolvable")

	// ErrSandboxMode refuses an unknown agent.provider.sandbox_mode posture. The
	// set is closed for the same reason permission_mode's is, and with more at
	// stake: a typo must not silently choose the unsandboxed member.
	ErrSandboxMode = errors.New("claude-code: agent.provider.sandbox_mode unusable")

	// ErrSandboxConfigDir refuses `sandbox_mode: srt` beside `config_dir:
	// inherit`. The two are individually valid and jointly incoherent — the
	// posture denies $HOME while `inherit` sends the harness there for its
	// configuration and its hooks — and #81 measured the result: an agent whose
	// every Bash call is refused by a hook the sandbox will not let start.
	// Structural, not readiness: it is a property of the file.
	ErrSandboxConfigDir = errors.New("claude-code: sandbox_mode needs the isolated config_dir")

	// ErrSandboxCredential refuses `sandbox_mode: srt` with no environment
	// credential. The posture denies $HOME, an OAuth session's credential lives
	// in ~/Library/Keychains inside it, and carving that out would hand the agent
	// every keychain item on the host. Named separately from ErrBinary because
	// the operator acts on it differently: this one is fixed in the workflow
	// file, not on the host.
	ErrSandboxCredential = errors.New("claude-code: sandbox_mode needs an environment credential")

	// ErrSandboxPublish refuses `sandbox_mode: srt` with no `publish` block.
	// Omitting it is permitted in general (SPEC §5.2.8) and means the agent
	// authenticates from what §7.6's allowlist carries — HOME, and whatever the
	// forge CLI stores under it. This posture denies HOME and points the CLI at
	// a directory BEN created, so that arrangement is exactly the one it breaks:
	// the run would work, commit, and have no credential to publish with.
	ErrSandboxPublish = errors.New("claude-code: sandbox_mode needs a publish credential")

	// ErrSandboxIdentity refuses `sandbox_mode: srt` on a host whose git has no
	// configured identity. The posture replaces the global git configuration, so
	// what the daemon account commits as is the only identity a run has, and an
	// absent one fails every `git commit` an attempt makes.
	ErrSandboxIdentity = errors.New("claude-code: sandbox_mode needs the daemon's git identity")

	// ErrSandbox refuses a sandbox runtime that is absent or unusable, and
	// covers the posture's own filesystem work. Readiness and dispatch, not
	// structure — like ErrBinary, and for the same reason.
	ErrSandbox = errors.New("claude-code: sandbox runtime unusable")

	// ErrSandboxPosture refuses a posture the host does not actually deliver.
	// Distinct from ErrSandbox because the runtime is present and runs: what
	// fails is a capability the posture depends on. srt strips unknown settings
	// keys silently rather than refusing them, and reports version 1.0.0 whatever
	// the package version is, so neither the file nor the version can answer
	// this — only running the posture can (#81 F3′).
	ErrSandboxPosture = errors.New("claude-code: sandbox posture not delivered by this host")

	// ErrSharedGitDir refuses a sandboxed run whose RunSpec reports no shared git
	// dir. A linked worktree's `.git` is a pointer into it, so a posture without
	// it cannot commit; the provider reports it (SPEC §6.1) and §7.1 forbids this
	// adapter discovering it from inside the agent's own writable tree.
	ErrSharedGitDir = errors.New("claude-code: sandbox_mode needs the workspace's shared git dir")

	// ErrEnvNamespace refuses a violation of the BEN_ reservation from either
	// side (SPEC §7.6): a provider block defining the prefix (structural, at
	// load) or a RunSpec carrying anything outside it (at Start, which the
	// orchestrator records as launch_error — non-retryable, since a rerun with
	// the same inputs fails identically).
	ErrEnvNamespace = errors.New("claude-code: BEN_ is reserved to the orchestrator")

	// ErrContinuationToken refuses a resume token that argv cannot carry safely
	// (SPEC §7.1, §9.6): the token is minted from the child's own JSON stream, and
	// one beginning with `-` is a flag the agent selected for its own next
	// invocation rather than a session id. The stream layer refuses such a token
	// where it mints it (validSessionID); this is the second, independent anchor,
	// so the argv construction is safe whatever reached it — a state file written
	// by an older binary included.
	ErrContinuationToken = errors.New("claude-code: continuation token unusable as an argv element")

	// ErrRemoteGitHubCredential refuses a standard reusable GitHub credential
	// variable in a remote invocation. Such a variable is ordinary provider
	// environment for a local run, but #194 forbids serializing its authority to
	// an execution substrate; remote publication identity belongs to the worker
	// profile.
	ErrRemoteGitHubCredential = errors.New("claude-code: reusable GitHub credential forbidden on a remote substrate")
)
