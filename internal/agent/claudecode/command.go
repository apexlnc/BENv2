package claudecode

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The invocation surface (SPEC §7.7): `claude -p --output-format stream-json`.
// Verified against claude 2.1.221 — `--verbose` is mandatory with that pair
// ("When using --print, --output-format=stream-json requires --verbose"), and
// the prompt arrives on stdin when no prompt argument is given.
//
// Two rules about what does not go here:
//   - RunLimits.MaxTurns is deliberately not mapped. The CLI does accept
//     --max-turns (undocumented in 2.1.221's --help, but its parser takes it);
//     the reason to leave it alone is semantic, not availability. BEN's
//     max_turns counts *continuation sessions per issue* (SPEC §5.2.7, §9.6) —
//     a chain the orchestrator schedules — whereas the flag caps assistant
//     turns inside one session. Passing one as the other would silently cut
//     sessions short.
//   - Nothing derived from a secret reaches argv, ever (SPEC §7.6): argv is
//     world-readable through ps. That is why no provider key carries inline
//     content, only paths and references.

// command builds the argv for one attempt. The prompt is never part of it.
//
// It returns an error rather than a best-effort argv because one element is not
// this adapter's to choose: the resume token comes from the child's own stream
// (see the continuation branch). A caller that could ignore the refusal and use
// the argv anyway would leave the check advisory, and there are exactly two
// callers — Start and RemoteInvocation — each of which owes the orchestrator a
// refusal with no process behind it (SPEC §7.3).
func (p Provider) command(spec core.RunSpec) ([]string, error) {
	argv := []string{p.Binary, "-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", p.PermissionMode}

	// Continuation is this adapter's session id, minted in the started event
	// and carried back opaquely by the orchestrator (SPEC §7.1). A resumed run
	// reports the same session id it was handed (verified against 2.1.221;
	// --fork-session is what changes it), so a continuation chain keeps one
	// stable token.
	//
	// Opaque to the orchestrator, checked here: the token is the one argv element
	// the *agent* chose, so it is validated where it is minted (validSessionID)
	// and again here, independently, against the one property that matters to an
	// argv (harness.CheckContinuationArgv). `--resume` takes its value as the next
	// element with no positional terminator available, so the value check is the
	// only control there is.
	if spec.Continuation != "" {
		if err := harness.CheckContinuationArgv(spec.Continuation, ErrContinuationToken); err != nil {
			return nil, err
		}
		argv = append(argv, "--resume", spec.Continuation)
	}
	if p.Model != "" {
		argv = append(argv, "--model", p.Model)
	}
	if p.Settings != "" {
		argv = append(argv, "--settings", p.Settings)
	}
	for _, t := range p.AllowedTools {
		argv = append(argv, "--allowed-tools", t)
	}
	for _, t := range p.DisallowedTools {
		argv = append(argv, "--disallowed-tools", t)
	}
	for _, d := range p.AddDirs {
		argv = append(argv, "--add-dir", d)
	}
	// A harness-side backstop only: the orchestrator owns the cost cap and the
	// budget_exceeded verdict (SPEC §9.9). Belt and braces, because a runaway
	// session can outspend a polling interval.
	if spec.Limits.MaxCostUSD > 0 {
		argv = append(argv, "--max-budget-usd", strconv.FormatFloat(spec.Limits.MaxCostUSD, 'f', -1, 64))
	}
	return argv, nil
}

// injected is this adapter's own contribution to the child environment: its
// documented auth surface (SPEC §7.6, §7.7), keyed by the variable each
// credential is injected as, so the keys line up with credentialKeys.Env.
//
// Split out of environ because readiness needs the same answer without composing
// a whole environment: checkPin has to know which credential a run would
// authenticate with, and reading it here keeps that question and the injection
// over one map (see credentialFor).
func (p Provider) injected() map[string]string {
	return map[string]string{
		sourceAPIKey:    p.APIKey,
		sourceAuthToken: p.AuthToken,
	}
}

// harnessDirs is the isolated posture's contribution to the child environment:
// the two directories this adapter points the harness at instead of letting it
// resolve them from $HOME (#114).
//
// Both are children of the private dir the *provider* placed and reports
// (SPEC §6.1). This adapter must not derive them from the workspace path —
// §7.1 forbids it, because §6.2's layout belongs to the provider and a second
// definition here would be wrong the first time that layout changes.
//
// They have deliberately different lifetimes, and the split is the whole
// finding behind this key. The config dir is the *workspace's*: session state
// lives in it, so a per-attempt one breaks §7.1 resume outright (#114 N1). The
// temp dir is the *attempt's*: it holds no resume state (#114 N2), and it is
// the one of the two that costs nothing to discard.
//
// Returns nil under `inherit`, and nil for an absent private dir — which is not
// a fallback but an unreachable state on both entry points, since Start refuses
// it (checkPrivateDir) and Ready supplies one of its own.
func (p Provider) harnessDirs(private string) map[string]string {
	if p.ConfigDir != ConfigDirIsolated || private == "" {
		return nil
	}
	return map[string]string{
		envConfigDir: filepath.Join(private, configDirName),
		envTmpDir:    filepath.Join(private, tmpDirName),
	}
}

// ensureHarnessDirs creates what harnessDirs names, and is the only I/O in this
// file. Called from Start and from Ready's probe, never from environ: composing
// an environment must stay a pure function of configuration, or the two callers
// that compose one without launching anything would have filesystem effects.
//
// The config dir is created if absent and otherwise left alone — it is the
// workspace's, and attempt 2 needs what attempt 1 wrote. The temp dir is
// *replaced*, which is what makes it attempt-owned: no bookkeeping, no cleanup
// hook to miss, and a daemon killed mid-attempt leaves one attempt's scratch
// for the next Start to collect rather than an unbounded pile.
func ensureHarnessDirs(dirs map[string]string) error {
	if dir := dirs[envTmpDir]; dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("claude-code: replacing the attempt temp dir %s: %w", dir, err)
		}
	}
	for _, name := range []string{envConfigDir, envTmpDir} {
		if dir := dirs[name]; dir != "" {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return fmt.Errorf("claude-code: creating %s at %s: %w", name, dir, err)
			}
		}
	}
	return nil
}

// checkPrivateDir refuses an isolated run the provider reported no private dir
// for. The alternative is the one thing the posture exists to prevent: with
// CLAUDE_CONFIG_DIR unset the harness resolves its config from $HOME, so an
// omission upstream would silently return every run to the operator's ~/.claude
// while the workflow file still says `isolated`.
func (p Provider) checkPrivateDir(spec core.RunSpec) error {
	if p.ConfigDir != ConfigDirIsolated || spec.Workspace.PrivateDir != "" {
		return nil
	}
	return fmt.Errorf("%w: the workspace reported none, and this adapter may not derive one "+
		"from the workspace path (SPEC §6.1, §7.1); set agent.provider.config_dir: %s to run "+
		"against the operator's ~/.claude deliberately", ErrPrivateDir, ConfigDirInherit)
}

// environ builds the complete child environment (SPEC §7.6). The adapter owns
// all of it; this adapter's contribution is its documented auth surface plus
// the isolated posture's directories, and the composition rules — allowlist,
// operator passthrough, provider env, the publish credential, then the
// orchestrator's BEN_ variables, with no precedence rule anywhere — are
// harness.Environ's.
//
// The two directories go in `injected`, which harness.Environ applies after the
// allowlist. That layer matters for TMPDIR specifically: it is an allowlist
// variable, so without this the daemon's own temp directory is what the child
// gets.
//
// The publish credential arrives already resolved (harness.ResolvePublish): the
// core names its variable and this adapter injects it (SPEC §5.2.8), so the value
// is read once per attempt by the caller and the same string reaches both the
// child and the transcript redaction set.
func (p Provider) environ(publish harness.PublishValue, spec core.RunSpec) ([]string, map[string]string) {
	injected := p.injected()
	maps.Copy(injected, p.harnessDirs(spec.Workspace.PrivateDir))
	// After the config-dir pins and reading the same private dir, because the
	// sandbox posture's CLAUDE_CODE_TMPDIR must name exactly what #114 pinned
	// TMPDIR to: srt replaces the child's TMPDIR with its own default unless
	// this variable tells it where to point, so the two disagreeing would leave
	// the attempt's scratch somewhere no attempt owns.
	maps.Copy(injected, p.sandboxEnv(spec.Workspace.PrivateDir))
	return harness.Environ(p.EnvPassthrough, p.Env, injected, publish, spec)
}
