package arch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// `make worktree-check` guards the one-branch-one-worktree rule (AGENTS.md,
// "Working in worktrees"): two worktrees sharing a branch share its ref but not
// their indexes, so advancing it in one leaves the other's tree silently short a
// commit.
//
// Tested here rather than demonstrated in a PR body because the target's whole
// value is the case CI never reaches. CI checks out one worktree, so the
// duplicate branch — the only input the detector exists for — is unreachable
// there, and a green pipeline says nothing about whether the check still works.
// A detector whose firing path is never exercised is indistinguishable from one
// that always passes.
//
// The real target is invoked, never a reimplementation of its shell: a copy of
// the pipeline in Go would prove the copy correct and leave the Makefile free to
// drift (AGENTS.md conventions, on fakes that restate what they stand in for).

// runWorktreeCheck invokes the repo's real `worktree-check` target with dir as
// the working directory, so the target sees whatever git topology dir sits in.
func runWorktreeCheck(t *testing.T, dir string) (combined string, exitCode int) {
	t.Helper()
	makefile := filepath.Join(moduleRoot(t), "Makefile")
	cmd := exec.Command("make", "-f", makefile, "worktree-check")
	cmd.Dir = dir
	// Keep git from reading the developer's identity, hooks or includes, and from
	// discovering a repository above dir — the not-a-repository case depends on
	// that search failing.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CEILING_DIRECTORIES="+filepath.Dir(dir),
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exit *exec.ExitError
	if !asExitError(err, &exit) {
		t.Fatalf("running worktree-check in %s: %v\n%s", dir, err, out)
	}
	return string(out), exit.ExitCode()
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// git runs a git command in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=arch", "GIT_AUTHOR_EMAIL=arch@example.invalid",
		"GIT_COMMITTER_NAME=arch", "GIT_COMMITTER_EMAIL=arch@example.invalid",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

// scratchRepo builds a one-commit repository on branch main.
func scratchRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "seed")
	git(t, dir, "commit", "-m", "seed")
	return dir
}

func TestWorktreeCheckAcceptsOneWorktreePerBranch(t *testing.T) {
	repo := scratchRepo(t)
	// A second worktree is present and legitimate: distinct branch, so the rule
	// holds. Without it this case would pass for the trivial reason that there is
	// only ever one worktree to compare.
	git(t, repo, "worktree", "add", filepath.Join(t.TempDir(), "other"), "-b", "other")

	out, code := runWorktreeCheck(t, repo)
	if code != 0 {
		t.Errorf("worktree-check failed on a compliant layout: exit %d\n%s", code, out)
	}
}

func TestWorktreeCheckRejectsOneBranchInTwoWorktrees(t *testing.T) {
	repo := scratchRepo(t)
	// --force is the point: git refuses this on its own, so reaching the state at
	// all means the refusal was overridden, which is exactly what the detector is
	// for.
	git(t, repo, "worktree", "add", "--force", filepath.Join(t.TempDir(), "dupe"), "main")

	out, code := runWorktreeCheck(t, repo)
	if code == 0 {
		t.Fatalf("worktree-check passed with main checked out twice:\n%s", out)
	}
	// The branch must be named. A bare non-zero exit would also satisfy a target
	// that failed for an unrelated reason.
	if !strings.Contains(out, "refs/heads/main") {
		t.Errorf("failure does not name the duplicated branch:\n%s", out)
	}
}

func TestWorktreeCheckFailsWhenItCannotEnumerate(t *testing.T) {
	// Not a repository, and GIT_CEILING_DIRECTORIES stops git finding one above.
	// A pipeline that discarded git's error would read this as zero duplicates and
	// report the all-clear (#118 review, P2).
	dir := t.TempDir()

	out, code := runWorktreeCheck(t, dir)
	if code == 0 {
		t.Fatalf("worktree-check passed where worktrees cannot be enumerated:\n%s", out)
	}
	if !strings.Contains(out, "cannot enumerate worktrees") {
		t.Errorf("failure does not distinguish a failure to look from a clean result:\n%s", out)
	}
}
