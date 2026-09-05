package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/gitcmd"
)

// #228, measured where BEN acts rather than at the argv.
//
// base.git must stay writable by the run — an agent's `git commit` in a linked
// worktree writes objects and refs there — so everything in it is authored by
// the thing being judged (SPEC §3.5) and read afterwards by a daemon-side git
// that decides whether the run may be published. Those surfaces answer back: a
// hook runs code as the daemon's user, while refs/replace/ and the legacy
// info/grafts file change what an object read says.
//
// The argv seam is anchored next door, in maintenance_test.go, and the
// environment seam in gitcmd_test.go. These cases are the other half: a recorded
// guard can still configure nothing, so each case plants the real adversarial
// state and asks for the answer BEN would act on. Each opens with a control phase
// proving the plant does steer a git that is not BEN's, in that same repository
// — without it, a green case is indistinguishable from one whose plant never
// took.

// TestPlantedReplaceRefCannotForgeDescentFromTheClaimBase plants the §9.7 leg-1
// forgery: history with nothing in common with the claim-time pin, plus one ref
// write under refs/replace/ that makes git answer as though it descends.
//
// The evidence around it does not notice — `rev-parse` keeps reporting the
// original commit — so a run that force-pushed unrelated or rewritten work would
// satisfy "descends from the claim-time base" and internal/verify's
// `!facts.DescendsBase` contradiction would never fire.
func TestPlantedReplaceRefCannotForgeDescentFromTheClaimBase(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// A parentless commit is the sharpest form of "does not descend": no walk
	// from the pin reaches it, whatever the history in between looks like.
	rogue := runGit(t, ws.SharedGitDir, "commit-tree", ws.BaseSHA+"^{tree}", "-m", "rogue")
	runGit(t, ws.SharedGitDir, "update-ref", "refs/heads/"+ws.Branch, rogue)
	if _, err := steerableGit(ws.SharedGitDir, "merge-base", "--is-ancestor", ws.BaseSHA, rogue); err == nil {
		t.Fatalf("the rogue commit already descends from %s before anything was planted; the case asserts nothing",
			ws.BaseSHA)
	}

	// The plant, as a run with write access to base.git would leave it: a commit
	// carrying the rogue's own tree on top of the pin, installed under
	// refs/replace/ as the rogue's stand-in.
	graft := runGit(t, ws.SharedGitDir, "commit-tree", rogue+"^{tree}", "-p", ws.BaseSHA, "-m", "graft")
	if _, err := replaceableGit(ws.SharedGitDir, "replace", rogue, graft); err != nil {
		t.Fatalf("plant replacement: %v", err)
	}
	if _, err := replaceableGit(ws.SharedGitDir, "merge-base", "--is-ancestor", ws.BaseSHA, rogue); err != nil {
		t.Fatalf("a git without the neutralization still says no after the replacement was planted (%v), so "+
			"the subject phase below cannot fail. Either this git no longer honors refs/replace/ (retire this "+
			"case) or the plant did not take (fix it).", err)
	}

	// Compatibility subject: steerableGit deliberately omits
	// core.useReplaceRefs=false, so this answer is protected only by the
	// GIT_NO_REPLACE_OBJECTS value in gitcmd.Env. That spelling predates the
	// config key and keeps this invariant true on Git versions before 2.19.
	out, err := steerableGit(ws.SharedGitDir, "merge-base", "--is-ancestor", ws.BaseSHA, rogue)
	if err == nil {
		t.Fatal("gitcmd.Env honored a planted replacement when the config override was absent: Git before " +
			"2.19 would let the run forge SPEC §9.7 leg 1")
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("environment-guarded merge-base failed for a reason other than 'not an ancestor': %v: %s", err, out)
	}

	// Leg 1 itself, through the provider: the same question, over the same
	// repository, answered by a git that does not consult the plant.
	facts, err := p.PublishFacts(ctx, ws)
	if err != nil {
		t.Fatalf("PublishFacts: %v", err)
	}
	if facts.Head != rogue {
		t.Fatalf("PublishFacts read head %s, want the planted %s: it is not reading the branch this case moved",
			facts.Head, rogue)
	}
	if facts.DescendsBase {
		t.Error("PublishFacts reports DescendsBase for a branch that descends only through a ref the run " +
			"planted in base.git: §9.7 leg 1 is then satisfied by a write the agent controls")
	}
	if facts.AdvancedPastBase(ws.BaseSHA) {
		t.Error("PublishFacts reports the branch advanced past its claim-time base through a planted replacement")
	}

	// The bounded reader composes its own invocation (gitLines) and asks the same
	// question for the prompt's account, so it is a second answer the plant could
	// have flipped: commits reported as work past a base they do not descend from.
	account, err := p.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts: %v", err)
	}
	if len(account.Commits) != 0 || len(account.Files) != 0 {
		t.Errorf("AttemptFacts reports %d commits and %d files past the base for history that does not "+
			"descend from it: %v", len(account.Commits), len(account.Files), account.Commits)
	}
}

// TestPlantedGraftCannotForgeDescentFromTheClaimBase is the legacy spelling of
// the same §9.7 forgery. core.useReplaceRefs=false and --no-replace-objects
// disable refs/replace/, but neither disables $GIT_COMMON_DIR/info/grafts. The
// latter is inside the same run-writable base.git and can attach a fake parent
// directly to the rogue commit.
func TestPlantedGraftCannotForgeDescentFromTheClaimBase(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	rogue := runGit(t, ws.SharedGitDir, "commit-tree", ws.BaseSHA+"^{tree}", "-m", "rogue")
	runGit(t, ws.SharedGitDir, "update-ref", "refs/heads/"+ws.Branch, rogue)
	// Git's deprecation advice is stderr, not a safety boundary. The same
	// attacker-authored config can turn it off, leaving clean command output and
	// the forged ancestry as the only observable effect.
	runGit(t, ws.SharedGitDir, "config", "advice.graftFileDeprecated", "false")
	if _, err := graftableGit(ws.SharedGitDir, "merge-base", "--is-ancestor", ws.BaseSHA, rogue); err == nil {
		t.Fatalf("the rogue commit already descends from %s before anything was planted; the case asserts nothing",
			ws.BaseSHA)
	}

	info := filepath.Join(ws.SharedGitDir, "info")
	mkdirAll(t, info)
	if err := os.WriteFile(filepath.Join(info, "grafts"), []byte(rogue+" "+ws.BaseSHA+"\n"), 0o644); err != nil {
		t.Fatalf("plant info/grafts: %v", err)
	}
	if _, err := graftableGit(ws.SharedGitDir, "merge-base", "--is-ancestor", ws.BaseSHA, rogue); err != nil {
		t.Fatalf("a git without the graft-file neutralization still says no after info/grafts was planted "+
			"(%v), so the subject phase below cannot fail. Either this git no longer honors legacy grafts "+
			"(retire this case) or the plant did not take (fix it).", err)
	}

	facts, err := p.PublishFacts(ctx, ws)
	if err != nil {
		t.Fatalf("PublishFacts: %v", err)
	}
	if facts.Head != rogue {
		t.Fatalf("PublishFacts read head %s, want the planted %s: it is not reading the branch this case moved",
			facts.Head, rogue)
	}
	if facts.DescendsBase {
		t.Error("PublishFacts reports DescendsBase for a branch that descends only through info/grafts " +
			"the run planted in base.git: §9.7 leg 1 is then satisfied by a write the agent controls")
	}
	if facts.AdvancedPastBase(ws.BaseSHA) {
		t.Error("PublishFacts reports the branch advanced past its claim-time base through a planted graft")
	}

	account, err := p.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts: %v", err)
	}
	if len(account.Commits) != 0 || len(account.Files) != 0 {
		t.Errorf("AttemptFacts reports %d commits and %d files past the base for history that descends only "+
			"through a planted graft: %v", len(account.Commits), len(account.Files), account.Commits)
	}
}

// plantedHooks are the hooks git may reach for while BEN prepares a workspace: a
// fetch runs reference-transaction on every ref write, `git worktree add` runs
// post-checkout, and the index paths run the rest. Planted together because
// which of them a given git version reaches is not the invariant — that none of
// them can run at all is.
var plantedHooks = []string{
	"reference-transaction",
	"post-checkout",
	"post-index-change",
	"pre-auto-gc",
	"fsmonitor-watchman",
}

// TestNoProviderGitRunsAHookPlantedInTheBaseRepository plants those hooks in
// base.git and drives a preparation over them.
//
// A hook here is not a script BEN configured: it is arbitrary code the run left
// in a directory it must be able to write, executed by the daemon's own git as
// the daemon's user, inside a command BEN believes it is waiting for. SPEC §6.5
// hooks are unaffected — those are commands the provider runs itself, never
// found by git.
func TestNoProviderGitRunsAHookPlantedInTheBaseRepository(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ran, forget := plantHooks(t, ws.SharedGitDir)

	// Control: the same repository, the same plant, a git BEN did not compose. A
	// ref write is what reference-transaction answers, and every fetch performs
	// one.
	if _, err := steerableGit(ws.SharedGitDir, "fetch", "--quiet", f.origin,
		"+refs/heads/main:refs/ben-test/control"); err != nil {
		t.Fatalf("control fetch: %v", err)
	}
	if fired := ran(); len(fired) == 0 {
		// Loud rather than skipped: silence here is indistinguishable between
		// "this git runs none of these hooks" and "the plant did not take", and
		// the second leaves the subject phase asserting nothing.
		t.Fatalf("a git without the neutralization ran none of the planted hooks %v, so the subject phase "+
			"below cannot fail. Either this git no longer runs them (retire this case) or the plant did not "+
			"take (fix it).", plantedHooks)
	}
	forget()

	// Subject: preparations BEN drives over the same planted directory. Origin
	// moves first so their fetches certainly write a ref — which is what the
	// control phase proved is enough — and the second issue is what reaches a
	// fresh `git worktree add`, the checkout leg, rather than a reattachment.
	f.pushCommit(t, "upstream")
	if _, err := prepareForTest(t, p, ctx, issue(ws.Key), 2); err != nil {
		t.Fatalf("Prepare (attempt 2): %v", err)
	}
	if _, err := prepareForTest(t, p, ctx, issue("43"), 1); err != nil {
		t.Fatalf("Prepare (second issue): %v", err)
	}
	if _, err := p.PublishFacts(ctx, ws); err != nil {
		t.Fatalf("PublishFacts: %v", err)
	}
	if err := p.Dispose(ctx, ws, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if fired := ran(); len(fired) != 0 {
		t.Errorf("a provider git ran hooks the run had planted in base.git: %v — arbitrary code executing as "+
			"the daemon's user, inside a command BEN is waiting for", fired)
	}
}

// A command that looks local can become a network command when a repository is
// marked as a partial clone: resolving a missing object asks the configured
// promisor remote for it. The control proves that exact fetch occurs. The
// provider read must instead fail closed and leave the object absent, because
// repository-local config must not be able to create an implicit remote
// boundary after #231 isolated every explicit one.
func TestProviderLocalGitCannotLazyFetchFromAPlantedPromisor(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	attacker := newFixture(t)
	attackerObject := attacker.pushCommit(t, "attacker-only promisor object")

	prepare := func(t *testing.T) *Provider {
		p := newProvider(t, newFixture(t), Hooks{})
		if _, err := prepareForTest(t, p, ctx, issue("42"), 1); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if objectAvailableWithoutLazyFetch(p.baseDir, attackerObject) {
			t.Fatalf("base already has attacker object %s; the case cannot observe a fetch", attackerObject)
		}
		plantPromisor(t, p.baseDir, attacker.origin, attackerObject)
		return p
	}

	t.Run("control", func(t *testing.T) {
		p := prepare(t)
		got, err := steerableGit(p.baseDir, "rev-parse", "--verify", "refs/ben-test/lazy^{commit}")
		if err != nil {
			t.Fatalf("unguarded rev-parse did not lazy-fetch the planted object: %v", err)
		}
		lines := strings.Split(got, "\n")
		if lines[len(lines)-1] != attackerObject || !objectAvailableWithoutLazyFetch(p.baseDir, attackerObject) {
			t.Fatalf("unguarded rev-parse = %q, want fetched attacker object %s", got, attackerObject)
		}
	})

	t.Run("provider", func(t *testing.T) {
		p := prepare(t)
		if got, ok, err := p.revParse(ctx, "refs/ben-test/lazy"); err == nil || ok || got != "" {
			t.Fatalf("provider revParse = (%q, %v, %v), want a fail-closed missing-object error", got, ok, err)
		}
		if objectAvailableWithoutLazyFetch(p.baseDir, attackerObject) {
			t.Fatalf("provider local read fetched attacker object %s through base config", attackerObject)
		}
	})
}

func plantPromisor(t *testing.T, baseDir, remote, missingObject string) {
	t.Helper()
	runGit(t, baseDir, "config", "core.repositoryFormatVersion", "1")
	runGit(t, baseDir, "config", "extensions.partialClone", "evil")
	runGit(t, baseDir, "config", "remote.evil.url", remote)
	runGit(t, baseDir, "config", "remote.evil.fetch", "+refs/heads/*:refs/remotes/evil/*")
	runGit(t, baseDir, "config", "remote.evil.promisor", "true")
	runGit(t, baseDir, "config", "remote.evil.partialCloneFilter", "blob:none")
	ref := filepath.Join(baseDir, "refs", "ben-test", "lazy")
	if err := os.MkdirAll(filepath.Dir(ref), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ref, []byte(missingObject+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func objectAvailableWithoutLazyFetch(dir, object string) bool {
	env := append(gitcmd.Env(),
		"GIT_NO_LAZY_FETCH=1",
		"GIT_ALLOW_PROTOCOL=",
	)
	_, err := steerableGitEnv(dir, env, "cat-file", "-e", object+"^{commit}")
	return err == nil
}

// A filter command is repository-authored code reached during the local
// checkout inside `git worktree add`. Transport settings belong only to the
// fetches that need them: giving the same environment to this local command
// lets an agent-authored filter use the daemon's SSH agent or proxy credential.
// The filter dumps its complete environment so the assertion is anchored on
// the secret value, independent of which name might carry it.
func TestLocalWorktreeFilterCannotObserveRemoteTransportEnvironment(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	writeFile(t, filepath.Join(f.seed, ".gitattributes"), "transport-probe filter=ben-env-probe\n")
	writeFile(t, filepath.Join(f.seed, "transport-probe"), "probe\n")
	runGit(t, f.seed, "add", ".gitattributes", "transport-probe")
	runGit(t, f.seed, "commit", "--quiet", "-m", "add transport probe")
	runGit(t, f.seed, "push", "--quiet", f.origin, "main:main")

	p := newProvider(t, f, Hooks{})
	if _, err := prepareForTest(t, p, ctx, issue("42"), 1); err != nil {
		t.Fatalf("initial Prepare: %v", err)
	}

	record := filepath.Join(t.TempDir(), "filter-environment")
	script := filepath.Join(t.TempDir(), "smudge-filter")
	body := "#!/bin/sh\nenv >> " + shellQuote(record) + "\ncat\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // executable filter is the adversarial fixture
		t.Fatal(err)
	}
	runGit(t, p.baseDir, "config", "filter.ben-env-probe.smudge", shellQuote(script))
	runGit(t, p.baseDir, "config", "filter.ben-env-probe.required", "true")

	const sentinel = "daemon-transport-canary-c67a2f91"
	for _, name := range []string{"SSH_AUTH_SOCK", "HTTPS_PROXY", "GIT_SSL_CAINFO"} {
		t.Setenv(name, sentinel)
	}
	if _, err := prepareForTest(t, p, ctx, issue("43"), 1); err != nil {
		t.Fatalf("Prepare with planted smudge filter: %v", err)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read smudge-filter environment: %v", err)
	}
	if !bytes.Contains(raw, []byte("GIT_TERMINAL_PROMPT=0")) {
		t.Fatal("smudge filter did not record the BEN-composed Git environment")
	}
	if bytes.Contains(raw, []byte(sentinel)) {
		t.Fatal("local worktree filter observed a daemon remote-transport setting")
	}
}

// plantHooks writes an executable hook into base.git/hooks for each name in
// plantedHooks, each appending its own name to one record, and returns a reader
// of which have run plus a way to forget what the control phase saw.
//
// Deliberately not a seam the provider cooperates with: what is under test is
// whether the operating system executed the file, so the evidence has to be
// written by the file itself. The record lives outside base.git, which a
// worktree operation may rewrite.
func plantHooks(t *testing.T, baseDir string) (ran func() []string, forget func()) {
	t.Helper()
	dir := filepath.Join(baseDir, "hooks")
	mkdirAll(t, dir)
	record := filepath.Join(t.TempDir(), "hooks-that-ran")
	for _, name := range plantedHooks {
		script := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(name) + " >> " + shellQuote(record) + "\nexit 0\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil { //nolint:gosec // an executable hook is the point
			t.Fatal(err)
		}
	}
	ran = func() []string {
		raw, err := os.ReadFile(record)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			t.Fatal(err)
		}
		var fired []string
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			if line != "" {
				fired = append(fired, line)
			}
		}
		sort.Strings(fired)
		return fired
	}
	forget = func() {
		if err := os.Remove(record); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return ran, forget
}

// TestReplaceableGitUsesTheProviderEnvironment holds the replacement control
// helper to the other half of its claim: it removes only BEN's explicit
// GIT_NO_REPLACE_OBJECTS guard while still stripping repository-local state
// inherited from the process that launched the test. Without that scrub an
// inherited GIT_NO_REPLACE_OBJECTS makes the control report "not an ancestor",
// while GIT_REPLACE_REF_BASE makes the plant and the control consult different
// namespaces before the provider is ever exercised.
func TestReplaceableGitUsesTheProviderEnvironment(t *testing.T) {
	f := newFixture(t)
	base := f.head(t)
	rogue := runGit(t, f.origin, "commit-tree", base+"^{tree}", "-m", "rogue")
	graft := runGit(t, f.origin, "commit-tree", rogue+"^{tree}", "-p", base, "-m", "graft")

	t.Setenv("GIT_NO_REPLACE_OBJECTS", "1")
	t.Setenv("GIT_REPLACE_REF_BASE", "refs/ben-test-replacements")
	if _, err := replaceableGit(f.origin, "replace", rogue, graft); err != nil {
		t.Fatalf("control git inherited repository-local replacement state while planting: %v", err)
	}
	if _, err := replaceableGit(f.origin, "merge-base", "--is-ancestor", base, rogue); err != nil {
		t.Fatalf("control git inherited repository-local replacement state instead of using the provider "+
			"environment: %v", err)
	}
}

// graftableGit is the control for the one steering mechanism Env itself turns
// off. Start from the provider environment so inherited repository-local state
// is still scrubbed, then remove only its explicit empty GIT_GRAFT_FILE: absence
// makes Git consult base.git/info/grafts again.
func graftableGit(dir string, args ...string) (string, error) {
	return gitWithoutEnvironmentGuard(dir, "GIT_GRAFT_FILE", args...)
}

// replaceableGit is the equivalent control for replacement refs: the provider
// environment is intact except for BEN's explicit no-replacement guard.
func replaceableGit(dir string, args ...string) (string, error) {
	return gitWithoutEnvironmentGuard(dir, "GIT_NO_REPLACE_OBJECTS", args...)
}

func gitWithoutEnvironmentGuard(dir, guard string, args ...string) (string, error) {
	base := gitcmd.Env()
	env := make([]string, 0, len(base))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if key != guard {
			env = append(env, kv)
		}
	}
	return steerableGitEnv(dir, env, args...)
}

// steerableGit runs a git this test composed rather than one gitcmd.Argv did: it
// carries #154's maintenance pair, so it cannot leave a detached child behind in
// a repository the case is still using, and none of #228's argv neutralization.
// Env still neutralizes legacy grafts and replacement refs; graftableGit and
// replaceableGit remove one value apiece for their controls. Each differs from a
// provider git only in what is under test, which is what makes it a control
// rather than a second subject.
func steerableGit(dir string, args ...string) (string, error) {
	return steerableGitEnv(dir, gitcmd.Env(), args...)
}

func steerableGitEnv(dir string, env []string, args ...string) (string, error) {
	argv := append([]string{"-c", "gc.auto=0", "-c", "maintenance.auto=false"}, args...)
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
