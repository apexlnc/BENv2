package orchestrator

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// This composes the real worktree provider with the authority loop. The hook
// genuinely advances a brand-new branch before Prepare returns; attempt 1 is
// the regression boundary, because treating that post-snapshot commit as
// reattached work was the review finding on #94.
func TestFreshHookCommitRemainsAttemptOne(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	origin := newAttemptFloorOrigin(t)

	provider, err := workspace.New(workspace.Options{
		Root:          t.TempDir(),
		WorkflowKey:   "wf",
		ScratchRoot:   t.TempDir(),
		AgentTempRoot: t.TempDir(),
		Repository:    core.Repository{RemoteURL: origin},
		Hooks: workspace.Hooks{AfterCreate: "touch hook.txt && git add hook.txt && " +
			"git -c user.name=hook -c user.email=hook@test.invalid " +
			"-c commit.gpgSign=false commit --quiet -m hook"},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	h := start(t, harnessOpts{
		issues:     []core.Issue{fake.Issue("1", epoch)},
		workspaces: provider,
		script:     startedOnly,
		hang:       true,
	})
	// The barrier is the state, not a stopwatch. Nothing here asserts latency —
	// what is waited for is that the launch happens at all — and a wall-clock
	// budget over this fixture's dozen git subprocesses could only ever encode the
	// load on the machine the number was chosen on, which is what made it flake
	// (#139). WaitLaunch fails the moment the loop takes an exit out of
	// `preparing` that is not a launch, and otherwise bounds itself by `-timeout`.
	h.WaitLaunch("1")
	if got := h.Runner.StartCount(); got != 1 {
		t.Fatalf("StartCount = %d, want exactly one launch (path: %v)", got, h.o.Transitions.Path("1"))
	}

	spec, ok := h.Runner.LastSpec()
	if !ok {
		t.Fatal("the fresh workspace produced no RunSpec")
	}
	if got := spec.Env["BEN_ATTEMPT"]; got != "1" {
		t.Errorf("BEN_ATTEMPT = %q, want 1 for work created by this attempt's hook", got)
	}
	if got := runAttemptFloorGit(t, spec.Workspace.Path, "log", "-1", "--pretty=%s"); got != "hook" {
		t.Errorf("workspace head subject = %q, want the hook commit", got)
	}
}

// A pre-pin provider failure still returns the workspace identity it computed.
// The retained path is not run evidence and must not turn the pending retry into
// an epoch fault; only a retained epoch/base pair would contradict pending.
func TestPrePinFailureRetriesWithTheRealWorkspaceProvider(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	origin := newAttemptFloorOrigin(t)
	offline := origin + ".offline"
	if err := os.Rename(origin, offline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := os.Stat(origin); errors.Is(err, os.ErrNotExist) {
			_ = os.Rename(offline, origin)
		}
	})

	provider, err := workspace.New(workspace.Options{
		Root:          t.TempDir(),
		WorkflowKey:   "wf",
		ScratchRoot:   t.TempDir(),
		AgentTempRoot: t.TempDir(),
		Repository:    core.Repository{RemoteURL: origin},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	h := start(t, harnessOpts{
		issues:     []core.Issue{fake.Issue("1", epoch)},
		workspaces: provider,
		prepRetry:  func(error) bool { return true },
		script:     startedOnly,
		hang:       true,
	})
	waitAttemptFloorState(t, h, StateBackoff)
	state, err := provider.ClaimBase(t.Context(), core.Issue{Identifier: "1"})
	if err != nil || state.State != core.ClaimBasePending {
		t.Fatalf("claim base after offline fetch = %+v, %v; want pending", state, err)
	}

	if err := os.Rename(offline, origin); err != nil {
		t.Fatal(err)
	}
	h.Clock.Advance(11 * time.Second)
	waitAttemptFloorLaunch(t, h)
	if got := h.Runner.StartCount(); got != 1 {
		t.Fatalf("StartCount = %d, want one retry launch (path: %v)", got, h.o.Transitions.Path("1"))
	}
	state, err = provider.ClaimBase(t.Context(), core.Issue{Identifier: "1"})
	if err != nil || state.State != core.ClaimBasePinned || state.Epoch <= 0 || state.BaseSHA == "" {
		t.Errorf("claim base after retry = %+v, %v; want a positive pinned pair", state, err)
	}
}

func newAttemptFloorOrigin(t *testing.T) string {
	t.Helper()
	repoRoot := t.TempDir()
	seed := filepath.Join(repoRoot, "seed")
	runAttemptFloorGit(t, repoRoot, "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runAttemptFloorGit(t, seed, "add", "README.md")
	runAttemptFloorGit(t, seed,
		"-c", "user.name=seed", "-c", "user.email=seed@test.invalid",
		"-c", "commit.gpgSign=false", "commit", "--quiet", "-m", "seed")
	origin := filepath.Join(repoRoot, "origin.git")
	runAttemptFloorGit(t, repoRoot, "clone", "--quiet", "--bare", seed, origin)
	return origin
}

func waitAttemptFloorState(t *testing.T, h *harness, want State) {
	t.Helper()
	budget, bounded := launchBudget(t)
	deadline := time.Now().Add(budget)
	for {
		if got := h.stateOf("1"); got == want {
			return
		} else if got == StateNeedsReview || got == StateFailed {
			t.Fatalf("issue 1 reached %s, want %s (path: %v)", got, want, h.o.Transitions.Path("1"))
		}
		if bounded && !time.Now().Before(deadline) {
			t.Fatalf("issue 1 did not reach %s within %s (path: %v)", want, budget, h.o.Transitions.Path("1"))
		}
		time.Sleep(time.Millisecond)
	}
}

func waitAttemptFloorLaunch(t *testing.T, h *harness) {
	t.Helper()
	budget, bounded := launchBudget(t)
	deadline := time.Now().Add(budget)
	for {
		if h.Runner.StartCount() > 0 {
			return
		}
		if got := h.stateOf("1"); got == StateNeedsReview || got == StateFailed {
			t.Fatalf("issue 1 reached %s instead of retrying the launch (path: %v)", got, h.o.Transitions.Path("1"))
		}
		if bounded && !time.Now().Before(deadline) {
			t.Fatalf("issue 1 did not retry its launch within %s (path: %v)", budget, h.o.Transitions.Path("1"))
		}
		time.Sleep(time.Millisecond)
	}
}

func runAttemptFloorGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}
