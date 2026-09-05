package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// #154, asserted three ways, because one of them alone would go quiet.
//
// The failure this closes is a `git fetch` whose *detached* maintenance child
// wrote into `objects/pack` after the fetch — and the test that ran it — had
// finished (CI run 32158116390). What must hold is that no git BEN starts can
// fork that child at all, and none of these three sees the whole of it:
//
//   - the argv seam, over a driven lifecycle: every git the provider actually
//     started carried the overrides, including the ones composed by remoteGit
//     and by the bounded reader, which build their argv separately;
//   - git's own child_start instrumentation: nothing was forked, measured where
//     the fork would be rather than by watching a directory for a late write;
//   - `git config --get` through the provider: git resolves the keys we passed
//     to the values we meant, over a repository that says the opposite.
//
// The first and third also carry #228's neutralization of the config surfaces a
// run can write inside base.git, which is the same shape of claim about the same
// argv: present before the subcommand, and resolved by git to the value BEN
// meant over a repository configured the other way. What planting the real
// adversarial state proves is next door, in steering_test.go — this pair cannot
// see whether the set is *sufficient*, only whether it is intact.
//
// The first two mutate the test binary's PATH, so they stay out of the #167
// cohort; the marker audit sees the t.Setenv through the helpers.

// The overrides, written out rather than read from gitcmd.Argv: a test driven by
// the declaration it checks agrees with any declaration, including one an edit
// emptied (AGENTS.md, Conventions).
var wantInvocationArgv = []string{
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "core.hooksPath=",
	"-c", "core.fsmonitor=",
	"-c", "core.useReplaceRefs=false",
}

// TestEveryProviderGitCarriesTheInvocationOverrides records the argv of every
// git a lifecycle's worth of provider calls starts.
//
// Driven through the provider rather than asserted about gitArgv, because the
// hazard is a call site that composes its own argv: remoteGit prepends the
// credential helper and gitLines builds a second exec.Command entirely, and both
// would keep passing a unit test of the composer they no longer use. The
// subcommand coverage below is what holds the driving to reaching all three.
func TestEveryProviderGitCarriesTheInvocationOverrides(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	// Everything this test drives with `git` directly happens before the
	// recorder is installed: those invocations are the test's, not BEN's, and
	// they would show up as violations of a rule they were never held to.
	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	agentCommit(t, ws.Path, "work.txt")
	agentPush(t, ws.Path)

	recorded := installGitRecorder(t)
	driveProviderLifecycle(t, ctx, p, ws)
	invocations := recorded()

	if len(invocations) == 0 {
		t.Fatal("no git invocation was recorded; the shim did not take, so this test asserts nothing")
	}
	for _, argv := range invocations {
		if len(argv) < len(wantInvocationArgv) ||
			!slices.Equal(argv[:len(wantInvocationArgv)], wantInvocationArgv) {
			// Before the subcommand, not merely present: git refuses `-c` after
			// it, so an override in the wrong place is not an override.
			t.Errorf("git %s\n  does not begin with %s — this invocation may fork maintenance BEN never waits "+
				"for, or be steered by config the run wrote into base.git",
				strings.Join(argv, " "), strings.Join(wantInvocationArgv, " "))
		}
	}

	// Each of the three constructors, named by a subcommand only it issues, so
	// that a lifecycle which stops reaching one fails here rather than reporting
	// a coverage it no longer has.
	for _, reach := range []struct{ subcommand, seam string }{
		{"fetch", "remoteGit, which composes a credentialed argv of its own"},
		{"worktree", "baseGit, the serialized base-repository path"},
		{"log", "gitLines, which builds a second exec.Command"},
	} {
		if !slices.ContainsFunc(invocations, func(argv []string) bool {
			return slices.Contains(argv, reach.subcommand)
		}) {
			t.Errorf("no `git %s` was recorded: %s is not covered by what this test drives",
				reach.subcommand, reach.seam)
		}
	}
}

// TestNoProviderGitForksBackgroundMaintenance asserts the invariant itself at
// git's own instrumentation: over a driven lifecycle, no git BEN started forked
// a maintenance or gc child.
//
// Trace2 records child_start unconditionally, so this is a fact about what
// happened and not a race anybody has to wait out. The control phase is what
// keeps it honest: it proves this git *does* fork maintenance when nothing stops
// it, so a green subject phase means the overrides worked rather than that the
// detector, the trace, or the git in front of it stopped reporting.
func TestNoProviderGitForksBackgroundMaintenance(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	agentCommit(t, ws.Path, "work.txt")
	agentPush(t, ws.Path)

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("no git on PATH: %v", err)
	}
	control := traceInto(t, realGit)
	scratch := filepath.Join(t.TempDir(), "control.git")
	runGit(t, filepath.Dir(scratch), "clone", "--quiet", "--bare", f.origin, scratch)
	runGit(t, scratch, "fetch", "--quiet", f.origin, "+refs/heads/main:refs/heads/main")
	if forks := control().maintenanceForks; len(forks) == 0 {
		// Loud rather than skipped, and deliberately: silence here is
		// indistinguishable between "this git no longer forks maintenance" and
		// "the trace, or this reading of it, stopped reporting one". The first
		// is a reason to retire this case; the second is the case going quiet
		// while the other two carry an invariant they only half cover.
		t.Fatalf("an unguarded `git fetch` forked no maintenance child, so the subject phase below "+
			"cannot fail. Either this git does not fork one (retire this case) or the trace is not "+
			"being read (fix it). Trace: %+v", control())
	}

	subject := traceInto(t, realGit)
	driveProviderLifecycle(t, ctx, p, ws)
	trace := subject()

	// A phase that ran no fetch would report no forks for the wrong reason —
	// fetch is the command the control phase proved forks one.
	if !slices.ContainsFunc(trace.commands, func(argv []string) bool { return slices.Contains(argv, "fetch") }) {
		t.Fatalf("the driven lifecycle ran no `git fetch`; commands seen: %v", trace.commands)
	}
	for _, forked := range trace.maintenanceForks {
		t.Errorf("a provider git forked `%s`: git detaches it, so it outlives the command BEN waited for "+
			"and takes locks in base.git and in live worktrees BEN believes it owns (#154)",
			strings.Join(forked, " "))
	}
}

// TestInvocationOverridesOutrankTheRepositoryConfig asks git what it resolved,
// which is the half neither of the above can see: an argv carrying a misspelled
// key, or a value git parses as something other than "off", is recorded and
// forked-free and configures nothing.
//
// Over a base repository configured the other way, because that is both the
// stronger claim and the realistic one — BEN can be pointed at a base.git an
// operator or an older BEN created, and for #228's three keys the repository is
// written by the run itself, so "the repository says the opposite" is the
// expected state rather than a contrived one.
func TestInvocationOverridesOutrankTheRepositoryConfig(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// The type is part of the assertion where there is one: git reads gc.auto as
	// an int and the two booleans as bools, and `--type` fails rather than
	// reporting a value git would not honor. The two hook keys are read
	// untyped — `--type=path` refuses an empty value, which is the value that
	// disables them.
	tests := []struct {
		key, typ, want, repo string
	}{
		{key: "gc.auto", typ: "int", want: "0", repo: "1"},
		{key: "maintenance.auto", typ: "bool", want: "false", repo: "true"},
		{key: "core.hooksPath", want: "", repo: "/tmp/ben-test-hooks"},
		{key: "core.fsmonitor", want: "", repo: "/tmp/ben-test-fsmonitor"},
		{key: "core.useReplaceRefs", typ: "bool", want: "false", repo: "true"},
	}
	for _, tt := range tests {
		runGit(t, ws.SharedGitDir, "config", tt.key, tt.repo)
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := runGit(t, ws.SharedGitDir, "config", "--get", tt.key); got != tt.repo {
				t.Fatalf("the repository reads %s=%q, want the opposing %q: the case asserts nothing", tt.key, got, tt.repo)
			}
			args := []string{"config", "--get", tt.key}
			if tt.typ != "" {
				args = []string{"config", "--get", "--type=" + tt.typ, tt.key}
			}
			for _, where := range []struct{ what, dir string }{
				{"base repository", ws.SharedGitDir},
				{"linked worktree", ws.Path},
			} {
				got, err := p.git(ctx, where.dir, args...)
				if err != nil {
					t.Fatalf("git config --get %s in the %s: %v", tt.key, where.what, err)
				}
				if got != tt.want {
					t.Errorf("in the %s a provider git resolves %s to %q, want %q — the repository's own value won",
						where.what, tt.key, got, tt.want)
				}
			}
		})
	}
}

// driveProviderLifecycle runs the provider calls that reach every git-invoking
// seam: preparation's fetches and worktree work, the bounded attempt account,
// the §9.7 evidence read, and disposal.
func driveProviderLifecycle(t *testing.T, ctx context.Context, p *Provider, ws core.Workspace) {
	t.Helper()
	if _, err := prepareForTest(t, p, ctx, issue(ws.Key), 2); err != nil {
		t.Fatalf("Prepare (reattach): %v", err)
	}
	if _, err := p.AttemptFacts(ctx, ws); err != nil {
		t.Fatalf("AttemptFacts: %v", err)
	}
	if _, err := p.PublishFacts(ctx, ws); err != nil {
		t.Fatalf("PublishFacts: %v", err)
	}
	p.AfterRun(ctx, ws)
	if err := p.Dispose(ctx, ws, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
}

// installGitRecorder puts a `git` in front of the real one on PATH that appends
// its argv to a file and then execs it, and returns a reader of what it caught.
//
// A shim rather than a seam in the provider: what is under test is the argv that
// reaches the operating system, and a recorder the code under test cooperates
// with could not see a call site that bypassed it.
func installGitRecorder(t *testing.T) func() [][]string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("no git on PATH: %v", err)
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "argv")
	// One line per invocation, fields separated by US (\037) so an argument
	// containing a space survives, written by a single printf: provider gits run
	// concurrently, and two half-records would read as two malformed argvs.
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$(printf '%%s\\037' \"$@\")\" >> %s\nexec %s \"$@\"\n",
		shellQuote(record), shellQuote(real))
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() [][]string {
		raw, err := os.ReadFile(record)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatal(err)
		}
		var out [][]string
		for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
			if line == "" {
				continue
			}
			out = append(out, strings.Split(strings.TrimSuffix(line, "\x1f"), "\x1f"))
		}
		return out
	}
}

// gitTrace is what one phase of trace2 output says about processes and forks.
type gitTrace struct {
	// commands are the argvs of the git processes that ran.
	commands [][]string
	// maintenanceForks are the children a git started to maintain a repository
	// in the background — the thing that must never appear.
	maintenanceForks [][]string
}

// traceInto puts a test-only Git shim on PATH that points GIT_TRACE2_EVENT at a
// fresh directory — one file per process, so nothing interleaves — and returns
// a reader of what accumulated there. Trace controls are intentionally absent
// from gitcmd.Env's production allowlist; injecting at the executable boundary
// keeps this instrumentation independent without widening that boundary.
func traceInto(t *testing.T, realGit string) func() gitTrace {
	t.Helper()
	dir := t.TempDir()
	shimDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nGIT_TRACE2_EVENT=%s exec %s \"$@\"\n",
		shellQuote(dir), shellQuote(realGit))
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shimDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() gitTrace {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var trace gitTrace
		for _, e := range entries {
			raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var ev struct {
					Event string   `json:"event"`
					Argv  []string `json:"argv"`
				}
				if err := json.Unmarshal([]byte(line), &ev); err != nil {
					continue // trace2 carries events with shapes this does not read.
				}
				switch ev.Event {
				case "start":
					trace.commands = append(trace.commands, ev.Argv)
				case "child_start":
					// `git gc --auto` and `git maintenance run --auto` are the
					// two routes; a fetch's own children (upload-pack,
					// pack-objects, rev-list) are not maintenance.
					if slices.Contains(ev.Argv, "gc") || slices.Contains(ev.Argv, "maintenance") {
						trace.maintenanceForks = append(trace.maintenanceForks, ev.Argv)
					}
				}
			}
		}
		return trace
	}
}

// shellQuote wraps s for `sh`, so a temp directory with a quote in it produces a
// broken shim loudly rather than a recorder that silently caught nothing.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
