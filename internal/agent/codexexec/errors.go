package codexexec

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
	// silent `sandox_mode:` typo would run with the wrong posture.
	ErrProviderKey = errors.New("codex-exec: unknown agent.provider key")

	// ErrProviderValue refuses a key whose value is the wrong shape or an
	// unusable member of a closed set.
	ErrProviderValue = errors.New("codex-exec: invalid agent.provider value")

	// ErrSandboxMode refuses a missing or unusable sandbox_mode. There is
	// deliberately no default: `codex exec`'s own default has changed between
	// releases, and a daemon that inherits it would silently change posture on a
	// harness upgrade.
	ErrSandboxMode = errors.New("codex-exec: agent.provider.sandbox_mode unusable")

	// ErrBinary refuses a harness binary that is absent, unresolvable, does not
	// answer --version as the Codex CLI, or reports no usable credential.
	// Readiness, not structure: this is what `Ready` catches so a dispatch never
	// discovers it (SPEC §7.3).
	ErrBinary = errors.New("codex-exec: harness binary unusable")

	// ErrPromptEmpty refuses a run with no prompt. The template layer already
	// refuses an empty body (SPEC §5.3); this is the boundary restating it, so
	// a bug upstream cannot spend an agent turn on nothing.
	ErrPromptEmpty = errors.New("codex-exec: empty prompt")

	// ErrWorkspacePath refuses a RunSpec without an absolute workspace path.
	// cwd is the only thing keeping the agent inside its worktree — and, for
	// this harness, it is also the root of the sandbox's writable area.
	ErrWorkspacePath = errors.New("codex-exec: workspace path must be absolute")

	// ErrEnvReserved refuses a configuration in which two sites write one child
	// environment variable (SPEC §7.6): a provider block setting an
	// adapter-owned variable through the generic env surfaces instead of its
	// named key, a block respelling the variable `publish.env` names, or a
	// `publish.env` naming a variable this adapter owns. CODEX_HOME must be an
	// absolute path and CODEX_API_KEY changes what readiness asks; neither check
	// can see a value that arrives as freeform environment, and neither survives
	// a publish credential written over it.
	ErrEnvReserved = errors.New("codex-exec: reserved child environment variable set outside its owning config site")

	// ErrPublishCredential refuses a publish credential that cannot be resolved
	// for this attempt (SPEC §5.2.8). Readiness, not structure: the workflow
	// named a variable and the host does not hold it, which is deliberately not a
	// load error — the file has to load in a CI that holds no publish secret
	// (SPEC §5.5).
	ErrPublishCredential = errors.New("codex-exec: publish credential unresolvable")

	// ErrEnvNamespace refuses a violation of the BEN_ reservation from either
	// side (SPEC §7.6): a provider block defining the prefix (structural, at
	// load) or a RunSpec carrying anything outside it (at Start, which the
	// orchestrator records as launch_error — non-retryable, since a rerun with
	// the same inputs fails identically).
	ErrEnvNamespace = errors.New("codex-exec: BEN_ is reserved to the orchestrator")
)
