package codexexec

import (
	"strconv"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The invocation surface (SPEC §7.7): `codex exec --json`. Verified against
// codex-cli 0.147.0.
//
// Three properties of that CLI the argv depends on:
//   - A bare `-` in the prompt position means "read the prompt from stdin",
//     which is how the prompt stays out of argv (SPEC §7.6).
//   - Resume is a subcommand — `codex exec [OPTIONS] resume <THREAD_ID> -` —
//     and the options belong before it, as the CLI's own usage states.
//   - `codex exec` refuses to run outside a trusted git repository unless
//     `--skip-git-repo-check` is passed. That flag is deliberately *not* passed:
//     a BEN workspace is always a git worktree (SPEC §6.2), which the harness
//     accepts (verified against a linked worktree), so keeping the check means a
//     workspace that is somehow not a repository fails loudly instead of running
//     the agent somewhere nobody vetted.
//
// Two limits are deliberately not mapped:
//   - RunLimits.MaxTurns counts *continuation sessions per issue*
//     (SPEC §5.2.7, §9.6), a chain the orchestrator schedules; this CLI has no
//     equivalent, and the nearest flag would cap something else.
//   - RunLimits.MaxCostUSD has no harness-side backstop here: `codex exec`
//     reports tokens but not cost and takes no budget flag. The orchestrator
//     owns the cap and the budget_exceeded verdict either way (SPEC §9.9); what
//     this adapter cannot offer is the belt-and-braces the claude-code adapter
//     gets from --max-budget-usd.

// command builds the argv for one attempt. The prompt is never part of it.
func (p Provider) command(spec core.RunSpec) []string {
	argv := []string{p.Binary, "exec", "--json", "--sandbox", p.SandboxMode}

	if p.Model != "" {
		argv = append(argv, "--model", p.Model)
	}
	argv = append(argv, p.sandboxOverrides()...)
	// Continuation is this adapter's thread id, minted in the started event and
	// carried back opaquely by the orchestrator (SPEC §7.1). A resumed run
	// reports the same thread id it was handed (verified against 0.147.0), so a
	// continuation chain keeps one stable token.
	if spec.Continuation != "" {
		argv = append(argv, "resume", spec.Continuation)
	}
	// The prompt arrives on stdin (SPEC §7.6).
	return append(argv, "-")
}

// sandboxOverrides pins every sandbox setting the workflow states, so the
// harness's own `config.toml` cannot widen it.
//
// An omitted flag is not a neutral default here: `-c` overrides what would
// otherwise be loaded from the config file under CODEX_HOME, so *not* passing
// `sandbox_workspace_write.network_access` hands that decision to whatever that
// file says. A workflow that wrote `network_access: false` would then get
// egress anyway, and empty `add_dirs` would inherit whatever writable roots the
// file names — silently, since neither appears in argv or in `ben config
// effective`. The whole posture of this adapter is that the operator states the
// sandbox rather than inheriting one (see ErrSandboxMode), and a setting is only
// stated if it is passed.
//
// So both keys are always emitted under workspace-write, false and empty
// included. The roots travel as the `writable_roots` array rather than as
// `--add-dir` so there is exactly one mechanism and no question about which
// wins: a flag that merges with the config key would leave the inherited roots
// standing. Values are pre-validated absolute and escape-free (checkAddDirs), so
// wrapping them in TOML basic strings is exact.
//
// Nothing is emitted under danger-full-access: that mode sandboxes nothing, so
// there is no boundary for the config file to widen. Key names and array syntax
// verified against codex-cli 0.147.0 with `--strict-config`, which rejects an
// unrecognized `-c` override.
func (p Provider) sandboxOverrides() []string {
	if p.SandboxMode != sandboxWorkspaceWrite {
		return nil
	}
	roots := make([]string, 0, len(p.AddDirs))
	for _, d := range p.AddDirs {
		roots = append(roots, `"`+d+`"`)
	}
	return []string{
		"-c", "sandbox_workspace_write.network_access=" + strconv.FormatBool(p.NetworkAccess),
		"-c", "sandbox_workspace_write.writable_roots=[" + strings.Join(roots, ",") + "]",
	}
}

// environ builds the complete child environment (SPEC §7.6). The adapter owns
// all of it; this adapter's contribution is its documented auth surface, and
// the composition rules — allowlist, operator passthrough, provider env, then
// the orchestrator's BEN_ variables, with no precedence rule anywhere — are
// harness.Environ's.
//
// CODEX_API_KEY, not OPENAI_API_KEY: measured against 0.147.0 with an empty
// CODEX_HOME, a bogus key in CODEX_API_KEY produces an authenticated request the
// API rejects, while the same key in OPENAI_API_KEY produces a request with no
// bearer at all. Only the first is the credential this harness actually uses.
func (p Provider) environ(publish harness.PublishValue, spec core.RunSpec) ([]string, map[string]string) {
	return harness.Environ(p.EnvPassthrough, p.Env, map[string]string{
		"CODEX_API_KEY": p.APIKey,
		"CODEX_HOME":    p.CodexHome,
	}, publish, spec)
}
