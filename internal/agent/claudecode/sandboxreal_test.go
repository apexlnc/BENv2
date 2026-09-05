package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The posture against the real sandbox runtime.
//
// Everything else in this package models the runtime; this is the only test
// that asks whether the composed posture actually *holds*, and it is the
// evidence behind #81's acceptance criterion 3 for the git half: a real
// process, in a real linked worktree, committing into a real shared git dir.
//
// It is not part of `make check`'s offline guarantee — the runtime is an npm
// install — so it skips loudly rather than silently when `srt` is absent, and
// the PR says so. What it does not cover is the agent half of criterion 3: a
// real `claude` run under this posture needs an environment credential, which
// a host pinning a login method refuses outright (#112).
func TestSandboxCommitsInALinkedWorktree(t *testing.T) {
	srt := requireSandboxRuntime(t)
	git := requireGit(t)

	// A $HOME-rooted fixture, matching SPEC §5.2.4's default workspace root.
	// That is not decoration: with the fixture in /tmp the read policy is never
	// exercised, which is exactly how "allowWrite does not imply read" stayed
	// hidden on #81 until a $HOME-rooted layout found it.
	home := t.TempDir()
	t.Setenv("HOME", home)
	wf := mkdir(t, home, ".local", "share", "ben", "wf")
	shared := filepath.Join(wf, "base.git")
	workspace := filepath.Join(wf, "issues", "issue-7")
	private := mkdir(t, wf, "private", "issue-7")

	seedLinkedWorktree(t, git, shared, workspace)
	worktreeAdmin := filepath.Clean(gitOut(t, git, "-C", workspace, "rev-parse", "--absolute-git-dir"))
	origin := filepath.Join(private, "origin.git")
	gitOut(t, git, "init", "--bare", "--initial-branch=main", origin)
	gitOut(t, git, "--git-dir="+shared, "remote", "add", "origin", origin)
	gitOut(t, git, "--git-dir="+shared, "push", "origin", "refs/heads/main:refs/heads/main")
	sharedConfig := filepath.Join(shared, "config")
	configBefore, err := os.ReadFile(sharedConfig)
	if err != nil {
		t.Fatal(err)
	}

	// The adapter's own composition, not a fixture written for the test: a
	// hand-written settings file would prove the runtime works and say nothing
	// about what this adapter asks it for.
	// A stub `gh` standing in for the credential helper: what is under test is
	// that git reaches it through BEN's config and inside the sandbox, not that
	// GitHub answers. It lives in the private dir so the posture's own gh
	// carve-out is what makes it executable.
	stubGH := filepath.Join(private, "gh")
	if err := os.WriteFile(stubGH,
		[]byte("#!/bin/sh\necho username=x-access-token\necho password=stub-token\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	p := goldenProvider(t)
	// add_dirs and sandbox_domains come from goldenProvider and are irrelevant
	// here; what matters is that the three provider paths are the real ones.
	p.AddDirs = nil
	paths, err := p.sandboxPathsFor(core.RunSpec{Workspace: core.WorkspacePaths{
		Path: workspace, SharedGitDir: shared, PrivateDir: private,
	}}, git, stubGH)
	if err != nil {
		t.Fatalf("sandboxPathsFor: %v", err)
	}
	if err := p.writeSandbox(paths, gitIdentity{Name: "ben-test", Email: "ben@test.invalid"}); err != nil {
		t.Fatalf("writeSandbox: %v", err)
	}

	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+paths.Control.GitConfig,
		"GIT_CONFIG_NOSYSTEM=1",
		envSandboxTmpDir+"="+mkdir(t, private, tmpDirName),
	)
	run := func(t *testing.T, dir string, argv ...string) (string, error) {
		t.Helper()
		cmd := exec.Command(srt, append([]string{"-s", paths.Control.Settings, "--"}, argv...)...)
		cmd.Dir, cmd.Env = dir, env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	// --- the criterion: a commit inside the workspace lands in the shared dir ---

	if err := os.WriteFile(filepath.Join(workspace, "NOTE.md"), []byte("sandboxed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, workspace, git, "status", "--short", "--", "NOTE.md"); err != nil {
		t.Fatalf("git status under the posture: %v: %s", err, out)
	} else if got := strings.TrimSpace(out); got != "?? NOTE.md" {
		t.Fatalf("git status under the posture emitted unexpected output %q; want only the untracked file", got)
	}
	if out, err := run(t, workspace, git, "add", "NOTE.md"); err != nil {
		t.Fatalf("git add under the posture: %v: %s", err, out)
	} else if out != "" {
		t.Fatalf("successful git add under the posture emitted output: %q", out)
	}
	if out, err := run(t, workspace, git, "commit", "-m", "a2 under srt"); err != nil {
		t.Fatalf("git commit under the posture: %v: %s\n\n"+
			"This is the criterion: a linked worktree's .git is a pointer into the shared git "+
			"dir, so the common mutable roots and current admin must be writable while the required "+
			"common metadata remains readable without granting the shared root.", err, out)
	}
	// Read back through the shared git dir, not the worktree: that the object
	// is there is the whole claim.
	head := gitOut(t, git, "--git-dir="+shared, "log", "--format=%s", "-1", "ben/issue-7")
	if head != "a2 under srt" {
		t.Errorf("shared git dir head = %q, want the commit made under the posture", head)
	}
	// Still a linked worktree, not a repository of its own.
	if info, err := os.Stat(filepath.Join(workspace, ".git")); err != nil || info.IsDir() {
		t.Errorf("<workspace>/.git = %v (dir=%v), want the pointer file intact", err, info != nil && info.IsDir())
	}

	// --- and the denials, each of which the commit above must not have needed ---

	// #232's property, beyond the settings fixture: an agent tries the actual
	// redirect against the real runtime, then a daemon-side git resolves the
	// worktree. If commondir changed, Git would leave the reported shared dir
	// before it ever consulted BEN's hook/config neutralization.
	t.Run("commondir tamper cannot redirect daemon git", func(t *testing.T) {
		commondir := filepath.Join(worktreeAdmin, "commondir")
		before, err := os.ReadFile(commondir)
		if err != nil {
			t.Fatal(err)
		}
		rogue := mkdir(t, workspace, "agent-chosen-common.git")
		registry := filepath.Join(shared, "worktrees")
		replaceDirectory := func(target string) string {
			moved := target + ".moved"
			rel, err := filepath.Rel(target, worktreeAdmin)
			if err != nil {
				t.Fatal(err)
			}
			oldAdmin := filepath.Join(moved, rel)
			return strings.Join([]string{
				"mv -- " + shellQuote(target) + " " + shellQuote(moved),
				"mkdir -p -- " + shellQuote(worktreeAdmin),
				"cp -- " + shellQuote(filepath.Join(oldAdmin, "gitdir")) + " " +
					shellQuote(filepath.Join(oldAdmin, "HEAD")) + " " + shellQuote(worktreeAdmin) + "/",
				"printf '%s\\n' " + shellQuote(rogue) + " > " + shellQuote(commondir),
			}, " && ")
		}
		for _, attack := range []struct {
			name, script, hostMoved string
			mustRefuse              bool
		}{
			{"overwrite", "printf '%s\\n' " + shellQuote(rogue) + " > " + shellQuote(commondir), "", true},
			{"remove", "rm -- " + shellQuote(commondir), "", true},
			{"rename", "mv -- " + shellQuote(commondir) + " " + shellQuote(commondir+".moved"), "", true},
			{"rename and recreate admin directory", replaceDirectory(worktreeAdmin), worktreeAdmin + ".moved", true},
			// On Linux the registry's host directory is hidden by denyRead and a
			// tmpfs skeleton may be renamed successfully. That is harmless only if
			// the host directory did not move and no host pointer changed.
			{"rename and recreate worktrees registry", replaceDirectory(registry), registry + ".moved", false},
		} {
			t.Run(attack.name, func(t *testing.T) {
				out, err := run(t, workspace, "/bin/sh", "-c", attack.script)
				if attack.mustRefuse && err == nil {
					t.Errorf("tampering with %s succeeded; Git can now be pointed at agent-owned config and hooks\n%s",
						commondir, out)
				}
				if attack.hostMoved != "" {
					if _, statErr := os.Lstat(attack.hostMoved); !errors.Is(statErr, os.ErrNotExist) {
						t.Errorf("host path %s exists after the sandboxed rename (lstat err=%v)",
							attack.hostMoved, statErr)
					}
				}
				after, readErr := os.ReadFile(commondir)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if string(after) != string(before) {
					t.Errorf("%s changed despite the sandbox refusal", commondir)
				}
			})
		}
		got := gitOut(t, git, "-C", workspace, "rev-parse", "--git-common-dir")
		if !filepath.IsAbs(got) {
			got = filepath.Join(workspace, got)
		}
		if got = filepath.Clean(got); !sameTestPath(t, got, shared) {
			t.Errorf("daemon-side git common dir = %s, want the provider-reported %s", got, shared)
		}
	})

	// A path denial also has to hold when the run asks the kernel for another
	// name for the same inode. Retained aliases are refused before composition;
	// this measures that the runtime does not let a run create one across its
	// read-only and writable roots and then mutate the denied file through it.
	t.Run("denied git files cannot be rewritten through a hard link", func(t *testing.T) {
		for _, target := range []struct{ name, path string }{
			{"commondir", filepath.Join(worktreeAdmin, "commondir")},
			{"shared config", filepath.Join(shared, "config")},
			{"workspace pointer", filepath.Join(workspace, ".git")},
		} {
			t.Run(target.name, func(t *testing.T) {
				before, err := os.ReadFile(target.path)
				if err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(workspace, "hardlink-"+strings.ReplaceAll(target.name, " ", "-"))
				out, err := run(t, workspace, "/bin/sh", "-c", strings.Join([]string{
					"/bin/ln " + shellQuote(target.path) + " " + shellQuote(alias),
					"printf pwned > " + shellQuote(alias),
				}, " && "))
				if err == nil {
					t.Errorf("rewriting denied %s through a new hard link succeeded\n%s", target.path, out)
				}
				after, readErr := os.ReadFile(target.path)
				if readErr != nil || string(after) != string(before) {
					t.Errorf("denied %s changed through a hard link: read err=%v", target.path, readErr)
				}
				if removeErr := os.Remove(alias); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					t.Fatalf("remove hard-link probe: %v", removeErr)
				}
			})
		}
	})

	// The settings were composed before this sibling existed. A snapshot-based
	// deny list misses it; keeping the registry itself outside allowWrite makes
	// its creation time irrelevant.
	t.Run("worktree added after composition stays read only", func(t *testing.T) {
		later := filepath.Join(wf, "issues", "issue-9")
		gitOut(t, git, "--git-dir="+shared, "worktree", "add", "-b", "ben/issue-9", later, "main")
		laterAdmin := filepath.Clean(gitOut(t, git, "-C", later, "rev-parse", "--absolute-git-dir"))
		laterCommon := filepath.Join(laterAdmin, "commondir")
		before, err := os.ReadFile(laterCommon)
		if err != nil {
			t.Fatal(err)
		}
		out, err := run(t, workspace, "/bin/sh", "-c",
			"printf '%s\\n' /agent/chosen > "+shellQuote(laterCommon))
		if err == nil {
			t.Errorf("rewriting a later worktree's commondir succeeded; the older sandbox retained "+
				"write authority over the shared registry\n%s", out)
		}
		if after, readErr := os.ReadFile(laterCommon); readErr != nil || string(after) != string(before) {
			t.Errorf("later commondir changed despite the refusal: read err=%v", readErr)
		}
		pwned := filepath.Join(laterAdmin, "pwned")
		out, err = run(t, workspace, "/bin/sh", "-c", "printf pwned > "+shellQuote(pwned))
		if err == nil {
			t.Errorf("writing a new file in the later admin directory succeeded\n%s", out)
		}
		if _, statErr := os.Lstat(pwned); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("later admin contains pwned after the refusal (lstat err=%v)", statErr)
		}
	})

	for _, tc := range []struct{ name, path, why string }{
		{"a shared hook", filepath.Join(shared, "hooks", "post-checkout"),
			"code the next run executes, in a directory every workspace of this workflow shares"},
		{"the shared config", sharedConfig,
			"the shared repository's own configuration"},
		{"the worktree admin reverse pointer", filepath.Join(worktreeAdmin, "gitdir"),
			"stable administration can be redirected without participating in a commit"},
		{"the per-worktree config", filepath.Join(worktreeAdmin, "config.worktree"),
			"it can steer Git when extensions.worktreeConfig is enabled"},
		{"the worktree .git pointer", filepath.Join(workspace, ".git"),
			"§6.2 reattaches, so a rewrite would choose the next attempt's repository"},
		{"BEN's git config", paths.Control.GitConfig,
			"an agent that rewrites it can restore an insteadOf rewrite and redirect its next push"},
		{"BEN's own settings file", paths.Control.Settings,
			"an agent that rewrites it chooses its own sandbox"},
	} {
		t.Run("denies writing "+tc.name, func(t *testing.T) {
			before, _ := os.ReadFile(tc.path)
			out, err := run(t, workspace, "/bin/sh", "-c",
				"printf '%s\\n' poisoned >> "+shellQuote(tc.path))
			if err == nil {
				t.Errorf("writing %s succeeded — %s\n%s", tc.path, tc.why, out)
			}
			// Refused, not merely reported: a sandbox whose denial the write
			// survives is worse than none, because the posture claims otherwise.
			if after, _ := os.ReadFile(tc.path); string(after) != string(before) {
				t.Errorf("%s changed despite the refusal", tc.path)
			}
		})
	}

	// #150: this is the adapter-composed posture, not a settings fixture made
	// for the test. The canonical command must publish the intended ref without
	// attempting the branch.* write that -u directs at the denied shared config.
	t.Run("canonical publish does not write shared config", func(t *testing.T) {
		out, err := run(t, workspace, git, "push", "origin", "HEAD")
		if err != nil {
			t.Fatalf("canonical publish under the posture: %v: %s", err, out)
		}
		if strings.Contains(strings.ToLower(out), "error:") {
			t.Errorf("successful publish reported an error:\n%s", out)
		}
		want := gitOut(t, git, "-C", workspace, "rev-parse", "HEAD")
		if got := gitOut(t, git, "--git-dir="+origin, "rev-parse", "refs/heads/ben/issue-7"); got != want {
			t.Errorf("origin ben/issue-7 = %s, want %s", got, want)
		}
		configAfter, err := os.ReadFile(sharedConfig)
		if err != nil {
			t.Fatal(err)
		}
		if string(configAfter) != string(configBefore) {
			t.Errorf("shared config changed during canonical publish\n--- before ---\n%s\n--- after ---\n%s",
				configBefore, configAfter)
		}
		branchConfig := exec.Command(git, "--git-dir="+shared, "config", "--local", "--get-regexp", `^branch\.`)
		branchConfig.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
		branchOut, branchErr := branchConfig.CombinedOutput()
		var exitErr *exec.ExitError
		if !errors.As(branchErr, &exitErr) || exitErr.ExitCode() != 1 || len(branchOut) != 0 {
			t.Errorf("branch config after publish = %q, err %v; want no branch.* keys", branchOut, branchErr)
		}
	})

	// §5.6's `git push origin HEAD` needs this route: with
	// GIT_CONFIG_NOSYSTEM set and BEN's config in place git
	// has no credential source at all, because the origin URL is deliberately
	// credential-free (§6.7) and git does not read GH_TOKEN. Measured before the
	// helper existed: `fatal: could not read Username for 'https://github.com'`.
	t.Run("git can authenticate a push", func(t *testing.T) {
		out, err := run(t, workspace, "/bin/sh", "-c",
			"printf 'protocol=https\\nhost=github.com\\n\\n' | "+git+" credential fill")
		if err != nil {
			t.Fatalf("git credential fill under the posture: %v: %s", err, out)
		}
		for _, want := range []string{"username=x-access-token", "password=stub-token"} {
			if !strings.Contains(out, want) {
				t.Errorf("git credential fill = %q, want %q — without it the posture commits "+
					"and cannot publish", out, want)
			}
		}
	})

	t.Run("denies reading the daemon's own $HOME", func(t *testing.T) {
		secret := filepath.Join(mkdir(t, home, ".ssh"), "id_ed25519")
		if err := os.WriteFile(secret, []byte("PRIVATE KEY\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, workspace, "/bin/sh", "-c", "cat "+secret)
		if err == nil || strings.Contains(out, "PRIVATE KEY") {
			t.Errorf("reading %s succeeded; §10.1's protected-mode outcome needs a bounded read "+
				"policy, and an empty denyRead is defense-in-depth only\n%s", secret, out)
		}
	})

	// The runtime's own TMPDIR override, which is why the adapter sets
	// CLAUDE_CODE_TMPDIR at all. Measured against 0.0.73; asserted here so a
	// runtime that stopped honouring it is a failing test rather than an
	// attempt's scratch landing in a path shared by every workspace.
	t.Run("honours the attempt-owned temp dir", func(t *testing.T) {
		out, err := run(t, workspace, "/bin/sh", "-c", "printf %s \"$TMPDIR\"")
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		want := filepath.Join(private, tmpDirName)
		if strings.TrimSpace(out) != want {
			t.Errorf("TMPDIR inside the sandbox = %q, want %q — the runtime replaced #114's pin",
				strings.TrimSpace(out), want)
		}
	})

	// The refusal an unwritten posture produces. srt strips settings keys it does
	// not recognize but will not run without the file at all, so a failure to
	// compose one is a failed run rather than a silently unsandboxed one.
	t.Run("refuses to run without the composed posture", func(t *testing.T) {
		cmd := exec.Command(srt, "-s", filepath.Join(private, "no-such-settings.json"), "--", git, "status")
		cmd.Dir, cmd.Env = workspace, env
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("ran without a settings file: %s", out)
		}
	})
}

// The composed settings must also be what the runtime accepts. srt strips
// unknown keys silently rather than refusing them (measured on 0.0.73), so this
// cannot assert the keys were understood — only that the file parses and the
// runtime starts under it, which is the half a golden cannot check.
func TestRealSandboxAcceptsTheComposedSettings(t *testing.T) {
	srt := requireSandboxRuntime(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	private := mkdir(t, home, "private")
	p := goldenProvider(t)
	p.AddDirs = nil
	paths := p.probeSandboxPaths(private, "/bin/sh", "")
	if err := p.writeSandbox(paths, gitIdentity{Name: "n", Email: "e@x.invalid"}); err != nil {
		t.Fatal(err)
	}
	// Round-trips as the DTO it was written from, so the file the runtime reads
	// and the file this package asserts on are the same shape.
	var settings sandboxSettings
	raw, err := os.ReadFile(paths.Control.Settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("composed settings do not parse: %v", err)
	}

	cmd := exec.Command(srt, "-s", paths.Control.Settings, "--", "/bin/echo", "ok")
	cmd.Dir, cmd.Env = paths.Workspace, os.Environ()
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "ok") {
		t.Errorf("the runtime refused the composed settings: %v: %s", err, out)
	}
}

// requireSandboxRuntime skips loudly: this test needs an npm install, so it is
// not part of `make check`'s offline guarantee, and a skip that did not say so
// would read as coverage.
func requireSandboxRuntime(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the posture is composed for the Seatbelt and bubblewrap backends only")
	}
	path, err := exec.LookPath(DefaultSandboxBinary)
	if err != nil {
		t.Skipf("skipping the only test of real sandbox enforcement: %q is not on PATH "+
			"(npm install -g @anthropic-ai/sandbox-runtime). CI does not run this.",
			DefaultSandboxBinary)
	}
	return path
}

func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	return path
}

// seedLinkedWorktree builds SPEC §6.2's shape with real git: a shared git dir
// and a worktree whose `.git` is a *file* pointing into it. That the pointer is
// a file rather than a directory is the whole reason the posture needs paths
// inside the shared dir; a plain clone would pass a posture that cannot commit.
func seedLinkedWorktree(t *testing.T, git, shared, workspace string) {
	t.Helper()
	seed := t.TempDir()
	for _, argv := range [][]string{
		{"init", "--bare", "--initial-branch=main", shared},
		{"-C", seed, "init", "--initial-branch=main"},
	} {
		gitOut(t, git, argv...)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, git, "-C", seed, "add", "-A")
	gitOut(t, git, "-C", seed, "-c", "user.name=seed", "-c", "user.email=seed@test.invalid",
		"commit", "-m", "seed")
	gitOut(t, git, "-C", seed, "push", shared, "HEAD:refs/heads/main")
	gitOut(t, git, "--git-dir="+shared, "worktree", "add", "-b", "ben/issue-7", workspace, "main")

	if info, err := os.Stat(filepath.Join(workspace, ".git")); err != nil || info.IsDir() {
		t.Fatalf("fixture is not a linked worktree: <workspace>/.git err=%v isDir=%v",
			err, info != nil && info.IsDir())
	}
}

func gitOut(t *testing.T, git string, argv ...string) string {
	t.Helper()
	cmd := exec.Command(git, argv...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(argv, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}
