package mirror

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

// #154's rule, held over the second component that drives git.
//
// The store has no worktrees, so the original failure — a detached maintenance
// child taking locks in a repository a live attempt is using — is not the one
// that lands here. What lands here is the same class one step further out: the
// daemon must account for every process it starts (SPEC §9.10), and a fetch that
// forks a child outliving the command BEN waited for is a process BEN cannot
// account for, writing into the object store that holds its evidence. A pack
// landing after the fetch returned is also a pack landing after `rev-parse` read
// the ref it was supposed to prove.
//
// Two assertions, because neither sees the whole of it: the argv every mirror
// git actually started, and git's own child_start instrumentation with a control
// phase proving this git forks maintenance when nothing stops it. Both mutate
// the test binary's environment, which is why nothing in this package runs in
// parallel.

// The overrides, written out rather than read from gitcmd: a test driven by the
// declaration it checks agrees with any declaration, including one an edit
// emptied (AGENTS.md, Conventions).
//
// #228's three keys are anchored here too, though the store is a repository no
// agent can reach: what this asserts is the shared invocation shape, and a
// consumer that carried only part of it would be the drift the anchoring exists
// to catch. The store fetches refs it names one at a time, so no refs/replace/
// and no hook arrives here by fetching — the guarantee is that it could not
// matter if one did.
var wantInvocationArgv = []string{
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "core.hooksPath=",
	"-c", "core.fsmonitor=",
	"-c", "core.useReplaceRefs=false",
}

// Written independently of fetchInto for the same completeness reason as the
// maintenance argv above. `all` includes loose and packed objects, pack indexes
// and references; `fsync` overrides macOS's non-durable writeout-only default.
var wantDurableFetchArgv = []string{
	"-c", "core.fsync=all",
	"-c", "core.fsyncMethod=fsync",
	"-c", "fetch.unpackLimit=1",
}

func TestEveryMirrorGitCarriesTheInvocationOverrides(t *testing.T) {
	o := newOrigin(t)
	m := newMirror(t, o)
	// Everything this test drives with `git` directly happens before the recorder
	// is installed: those invocations are the test's, not BEN's, and they would
	// show up as violations of a rule they were never held to.
	o.commit(t, Branch(lifecycleRef.Key))

	recorded := installGitRecorder(t)
	driveMirrorLifecycle(t, m)
	invocations := recorded()

	if len(invocations) == 0 {
		t.Fatal("no git invocation was recorded; the shim did not take, so this test asserts nothing")
	}
	for _, argv := range invocations {
		if len(argv) < len(wantInvocationArgv) || !slices.Equal(argv[:len(wantInvocationArgv)], wantInvocationArgv) {
			// Before the subcommand, not merely present: git refuses `-c` after
			// it, so an override in the wrong place is not an override.
			t.Errorf("git %s\n  does not begin with %s — this invocation may fork maintenance BEN never waits "+
				"for, or be steered by config inside the repository it is reading",
				strings.Join(argv, " "), strings.Join(wantInvocationArgv, " "))
		}
		if fetch := slices.Index(argv, "fetch"); fetch >= 0 &&
			!containsArgv(argv[:fetch], wantDurableFetchArgv) {
			t.Errorf("git %s\n  does not harden the objects and reference before fetch returns",
				strings.Join(argv, " "))
		}
	}

	// Each way this package composes an argv, named by a subcommand only it
	// issues, so a lifecycle that stops reaching one fails here rather than
	// reporting a coverage it no longer has.
	for _, reach := range []struct{ subcommand, seam string }{
		{"init", "the bootstrap, which runs outside the store it is creating"},
		{"fetch", "remoteGit, which composes a credentialed argv of its own"},
		{"ls-remote", "the probe, the only remote read that is not a fetch"},
		{"rev-parse", "the local reads that prove a ref"},
	} {
		if !slices.ContainsFunc(invocations, func(argv []string) bool { return slices.Contains(argv, reach.subcommand) }) {
			t.Errorf("no `git %s` was recorded: %s is not covered by what this test drives",
				reach.subcommand, reach.seam)
		}
	}
}

func containsArgv(argv, sequence []string) bool {
	for i := 0; i+len(sequence) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(sequence)], sequence) {
			return true
		}
	}
	return false
}

func TestNoMirrorGitForksBackgroundMaintenance(t *testing.T) {
	o := newOrigin(t)
	m := newMirror(t, o)
	o.commit(t, Branch(lifecycleRef.Key))

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("no git on PATH: %v", err)
	}
	control := traceInto(t, realGit)
	scratch := filepath.Join(t.TempDir(), "control.git")
	runGit(t, filepath.Dir(scratch), "clone", "--quiet", "--bare", o.path, scratch)
	runGit(t, scratch, "fetch", "--quiet", o.path, "+refs/heads/main:refs/heads/main")
	if forks := control().maintenanceForks; len(forks) == 0 {
		// Loud rather than skipped: silence here is indistinguishable between
		// "this git no longer forks maintenance" and "the trace, or this reading
		// of it, stopped reporting one".
		t.Fatalf("an unguarded `git fetch` forked no maintenance child, so the subject phase below cannot "+
			"fail. Either this git does not fork one (retire this case) or the trace is not being read. Trace: %+v",
			control())
	}

	subject := traceInto(t, realGit)
	driveMirrorLifecycle(t, m)
	trace := subject()

	if !slices.ContainsFunc(trace.commands, func(argv []string) bool { return slices.Contains(argv, "fetch") }) {
		t.Fatalf("the driven lifecycle ran no `git fetch`; commands seen: %v", trace.commands)
	}
	for _, forked := range trace.maintenanceForks {
		t.Errorf("a mirror git forked `%s`: git detaches it, so it outlives the command BEN waited for and "+
			"writes into the object store BEN reads its evidence from (#154)", strings.Join(forked, " "))
	}
}

// lifecycleRef is the claim both phases drive, named here so each can arrange
// the branch with its own git before BEN's is being watched.
var lifecycleRef = core.RemoteClaimRef{Issue: "42", Key: "42", Epoch: 1}

// driveMirrorLifecycle runs the calls that reach every git-invoking seam: the
// bootstrap, the claim pin, an observation of a published branch, and disposal.
// It starts no git of its own beyond the mirror's.
func driveMirrorLifecycle(t *testing.T, m *Mirror) {
	t.Helper()
	ctx := context.Background()
	if _, err := m.RecordClaim(ctx, lifecycleRef); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	if _, err := m.RemoteFacts(ctx, run(lifecycleRef, "run-1", "v1")); err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if err := m.Discard(ctx, lifecycleRef); err != nil {
		t.Fatalf("Discard: %v", err)
	}
}

// installGitRecorder puts a `git` in front of the real one on PATH that appends
// its argv to a file and then execs it, and returns a reader of what it caught.
//
// A shim rather than a seam in the mirror: what is under test is the argv that
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
	// containing a space survives, written by a single printf: two half-records
	// would read as two malformed argvs.
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$(printf '%%s\\037' \"$@\")\" >> %s\nexec %s \"$@\"\n",
		shellQuote(record), shellQuote(real))
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil { //nolint:gosec // an executable shim is the point
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
	commands         [][]string
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
	if err := os.WriteFile(filepath.Join(shimDir, "git"), []byte(script), 0o755); err != nil { //nolint:gosec // an executable shim is the point
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
					// `git gc --auto` and `git maintenance run --auto` are the two
					// routes; a fetch's own children are not maintenance.
					if slices.Contains(ev.Argv, "gc") || slices.Contains(ev.Argv, "maintenance") {
						trace.maintenanceForks = append(trace.maintenanceForks, ev.Argv)
					}
				}
			}
		}
		return trace
	}
}
