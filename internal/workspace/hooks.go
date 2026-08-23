package workspace

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Hooks are the four optional lifecycle scripts plus their shared timeout
// (SPEC §5.2.6). Scripts are opaque shell text; $VAR indirection deliberately
// does not apply — expansion is the shell's job.
type Hooks struct {
	AfterCreate  string
	BeforeRun    string
	AfterRun     string
	BeforeRemove string
	// Timeout bounds each hook run; ≤0 falls back to the config default (60s).
	Timeout time.Duration
}

// runHook executes one hook script via `sh -c` with cwd = dir and a composed
// environment, bounded by the configured timeout (SPEC §5.2.6, §6.5). nil
// means the hook is absent or exited zero; callers apply the per-hook failure
// semantics.
func (p *Provider) runHook(ctx context.Context, name, script, dir string) error {
	if strings.TrimSpace(script) == "" {
		return nil
	}
	hctx, cancel := context.WithTimeout(ctx, p.hookTimeout)
	defer cancel()
	// `-c`, not `-lc`: a login shell sources /etc/profile and ~/.profile, so a
	// hook's behavior becomes a function of the operator's dotfiles — which is
	// how a hook passes on a laptop and fails under systemd with nothing in the
	// logs. §5.2.6 specifies "`sh -lc` (or stricter)"; this is the stricter.
	// What a dotfile would have contributed — PATH above all — now arrives
	// explicitly through hookEnv.
	cmd := exec.CommandContext(hctx, "sh", "-c", script)
	cmd.Dir = dir
	cmd.Env = hookEnv()
	// The hook gets its own process group and the timeout kills the whole
	// group: an orphaned child must not keep mutating the workspace after
	// the hook is reported dead.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// Backstop: unblock Wait if a grandchild dodged the group kill while
	// holding the output pipe.
	cmd.WaitDelay = 2 * time.Second
	// Output is bounded while the hook runs, not just in the final error —
	// a hook can spew gigabytes (#16). One writer for both streams: os/exec
	// serializes Writes when Stdout == Stderr.
	tail := &tailWriter{limit: outputLimit}
	cmd.Stdout = tail
	cmd.Stderr = tail
	if err := cmd.Run(); err != nil {
		// No credential to scrub, and that is a property rather than an
		// omission: a hook's environment is core.EnvAllowlist and nothing else
		// (hookEnv), so the remote credential never enters this child.
		// Redaction now covers the credential an invocation *used*, and a hook
		// uses none.
		detail := strings.TrimSpace(tail.String())
		if hctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("hook %s timed out after %s: %s", name, p.hookTimeout, detail)
		}
		return fmt.Errorf("hook %s: %v: %s", name, err, detail)
	}
	return nil
}

// hookEnv composes a hook's complete environment: the allowlisted daemon
// passthrough (core.EnvAllowlist) and nothing else — the same discipline
// harness.Environ applies to an agent run, because it is the same trust
// boundary (SPEC §6.5, §7.6).
//
// Inheriting os.Environ() instead would hand every hook the tracker PAT, the
// agent API keys, and whatever else the supervisor exported, while the agent
// those hooks bootstrap for is held to eleven names. Hooks are the weaker side
// of that comparison, not the stronger one: they are repo-authored shell that
// runs on the daemon's host, outside the worktree isolation §6.7 leans on, and
// under BEN's own credentials rather than the agent's.
//
// There is deliberately no per-hook opt-in surface to widen this: `hooks` is a
// closed schema (§5.2.6), so adding one is a spec amendment, not an
// implementation detail. A hook that needs more than the allowlist should be
// reading it out of the workspace, not out of the daemon.
//
// Composed per run rather than at New: the daemon's environment is read at the
// moment the hook fires, the same as for an agent launch.
func hookEnv() []string {
	// Non-nil even when empty — a nil Env means "inherit" to os/exec, which is
	// the exact behavior this function exists to prevent.
	env := make([]string, 0, len(core.EnvAllowlist))
	for _, name := range core.EnvAllowlist {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// tailWriter keeps only the last limit bytes written; failures are
// diagnosed from the tail, where shells and git put the cause.
type tailWriter struct {
	limit     int
	buf       []byte
	truncated bool
}

func (w *tailWriter) Write(p []byte) (int, error) {
	if len(w.buf)+len(p) > w.limit {
		w.truncated = true
	}
	w.buf = append(w.buf, p...)
	if over := len(w.buf) - w.limit; over > 0 {
		copy(w.buf, w.buf[over:])
		w.buf = w.buf[:w.limit]
	}
	return len(p), nil
}

func (w *tailWriter) String() string {
	if w.truncated {
		return "…" + string(w.buf)
	}
	return string(w.buf)
}
