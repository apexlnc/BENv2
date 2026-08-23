package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// #114 acceptance: a Prepare reports every path this provider owns (SPEC §6.1),
// and each is where §6.2's layout says it is.
//
// The layout is asserted against literal path arithmetic rather than against
// pathsFor, because pathsFor is the declaration under test: a test driven by it
// would agree with any layout it happened to state, including one that put the
// private dir inside the worktree.
func TestPrepareReportsEveryPathItOwns(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	wf := filepath.Join(p.root, "wf") // newProvider's WorkflowKey
	want := core.WorkspacePaths{
		Path:         filepath.Join(wf, "issues", "42"),
		SharedGitDir: filepath.Join(wf, "base.git"),
		PrivateDir:   filepath.Join(wf, "private", "42"),
	}
	if ws.WorkspacePaths != want {
		t.Errorf("reported paths = %+v, want %+v (SPEC §6.2 layout)", ws.WorkspacePaths, want)
	}
	if !dirExists(ws.PrivateDir) {
		t.Errorf("private dir %s does not exist; an adapter is handed the path unconditionally", ws.PrivateDir)
	}
	// §6.3's second rule, read off the reported values rather than off the
	// layout that produced them: inside the worktree is the one place it
	// must not be, because that is what would put harness state in a commit.
	if strictlyUnder(normalizePath(ws.PrivateDir), normalizePath(ws.Path)) {
		t.Errorf("private dir %s is inside the worktree %s (SPEC §6.3)", ws.PrivateDir, ws.Path)
	}
	// git must not see it. `status` from inside the worktree is the check that
	// matters, since that is the process that would commit it.
	if err := os.WriteFile(filepath.Join(ws.PrivateDir, "harness-state"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := runGit(t, ws.Path, "status", "--porcelain"); out != "" {
		t.Errorf("git status in the worktree = %q, want clean — private state reached the repository", out)
	}
}

// #114 acceptance, and the reason the config dir's lifetime is the workspace's
// rather than the attempt's (N1 on the issue): state written into the private
// dir survives into the next attempt, because §7.1 resume is stored there and a
// fresh directory breaks it outright.
//
// Driven through Prepare across attempts rather than asserted about pathsFor: a
// stable *path* is not a surviving *directory*, and only one of those is what
// resume needs.
func TestPrivateDirSurvivesReattach(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws1, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare 1: %v", err)
	}
	session := filepath.Join(ws1.PrivateDir, "session.jsonl")
	if err := os.WriteFile(session, []byte("attempt 1"), 0o600); err != nil {
		t.Fatal(err)
	}

	ws2, err := prepareForTest(t, p, ctx, issue("7"), 2)
	if err != nil {
		t.Fatalf("Prepare 2: %v", err)
	}
	if !samePath(ws2.PrivateDir, ws1.PrivateDir) {
		t.Fatalf("attempt 2 private dir = %s, want %s", ws2.PrivateDir, ws1.PrivateDir)
	}
	got, err := os.ReadFile(session)
	if err != nil || string(got) != "attempt 1" {
		t.Errorf("attempt 1's state read back as %q, %v; a continuation chain would have lost its session", got, err)
	}
}

// A private dir an operator or a tmp-sweep removed is repaired by the next
// Prepare rather than left absent: the workspace has lost harness state, not
// its worktree, and an adapter is still handed the path.
func TestPrepareRecreatesARemovedPrivateDir(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("3"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := os.RemoveAll(ws.PrivateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareForTest(t, p, ctx, issue("3"), 2); err != nil {
		t.Fatalf("Prepare after removal: %v", err)
	}
	if !dirExists(ws.PrivateDir) {
		t.Errorf("private dir %s was not re-created", ws.PrivateDir)
	}
}

// SPEC §6.4: the private dir shares the worktree's lifetime exactly — kept when
// it is kept, removed when it is removed — while the shared git dir outlives
// both, being per-workflow rather than per-workspace.
func TestPrivateDirSharesTheWorktreeLifetime(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("5"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	if err := p.Dispose(ctx, ws, true); err != nil {
		t.Fatalf("Dispose(keep): %v", err)
	}
	if !dirExists(ws.PrivateDir) {
		t.Error("Dispose(keep=true) removed the private dir; forensics keep the harness state too")
	}

	if err := p.Dispose(ctx, ws, false); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if dirExists(ws.PrivateDir) {
		t.Error("Dispose(keep=false) left the private dir behind")
	}
	if !dirExists(ws.SharedGitDir) {
		t.Error("Dispose removed the shared git dir, which is per-workflow (SPEC §6.4)")
	}
}

// The startup sweep reconstructs a Workspace from a directory listing, so its
// disposals go through the same derivation. Without it the private dir of every
// swept workspace would leak — invisibly, since Sweep enumerates issues/.
func TestSweepRemovesThePrivateDir(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})

	ws1, err := prepareForTest(t, p, ctx, issue("1"), 1)
	if err != nil {
		t.Fatal(err)
	}
	ws2, err := prepareForTest(t, p, ctx, issue("2"), 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Sweep(ctx, func(key string) bool { return key == "1" }); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if dirExists(ws1.PrivateDir) {
		t.Error("the swept workspace's private dir survived")
	}
	if !dirExists(ws2.PrivateDir) {
		t.Error("a live workspace's private dir was swept")
	}
}

// A caller carrying a private dir this provider's layout does not place is
// refused, not overruled: it means the record came from another provider, and
// removing this one's directory on its behalf would delete a path nobody asked
// about. The check is what keeps derivation from silently ignoring the field.
func TestDisposeRefusesAForeignPrivateDir(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("9"), 1)
	if err != nil {
		t.Fatal(err)
	}
	foreign := ws
	foreign.PrivateDir = filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(foreign.PrivateDir, 0o700); err != nil {
		t.Fatal(err)
	}

	err = p.Dispose(ctx, foreign, false)
	if !errors.Is(err, ErrWorkspaceState) {
		t.Errorf("Dispose with a foreign private dir = %v, want ErrWorkspaceState", err)
	}
	if !dirExists(foreign.PrivateDir) {
		t.Error("the refused dispose removed the foreign directory anyway")
	}
	if !dirExists(ws.Path) {
		t.Error("the refused dispose removed the worktree")
	}
}

// SPEC §6.3 invariant 2, over every reported path rather than the worktree
// alone. Each case names the path a consumer would bind into a sandbox posture
// (§10.1) if the check did not exist.
func TestCheckContainedCoversEveryReportedPath(t *testing.T) {
	parallel(t)
	p := newProvider(t, newFixture(t), Hooks{})
	good := p.pathsFor("42")
	outside := t.TempDir()

	mutate := func(fn func(*core.WorkspacePaths)) core.WorkspacePaths {
		out := good
		fn(&out)
		return out
	}
	// Each case names the phrase of the rule that must answer for it, not just
	// the sentinel. Every guard in checkContained returns ErrPathEscape, so a
	// test asserting only that passes when the wrong rule refuses — which is
	// how the outside-the-worktree rule and the unreported-path rule can both
	// be deleted with nothing going red (both survived as mutants until this
	// column existed).
	tests := []struct {
		name  string
		paths core.WorkspacePaths
		want  string
	}{
		{"worktree outside the root", mutate(func(w *core.WorkspacePaths) { w.Path = outside }),
			"workspace path " + outside + " is not under"},
		{"shared git dir outside the root", mutate(func(w *core.WorkspacePaths) { w.SharedGitDir = outside }),
			"shared git dir " + outside + " is not under"},
		{"private dir outside the root", mutate(func(w *core.WorkspacePaths) { w.PrivateDir = outside }),
			"private dir " + outside + " is not under"},
		{"private dir inside the worktree", mutate(func(w *core.WorkspacePaths) {
			w.PrivateDir = filepath.Join(good.Path, ".ben-private")
		}), "is inside the worktree"},
		{"private dir is the worktree", mutate(func(w *core.WorkspacePaths) { w.PrivateDir = good.Path }),
			"is inside the worktree"},
		{"worktree unreported", mutate(func(w *core.WorkspacePaths) { w.Path = "" }),
			"workspace path is empty"},
		{"shared git dir unreported", mutate(func(w *core.WorkspacePaths) { w.SharedGitDir = "" }),
			"shared git dir is empty"},
		{"private dir unreported", mutate(func(w *core.WorkspacePaths) { w.PrivateDir = "" }),
			"private dir is empty"},
		{"private dir traversal", mutate(func(w *core.WorkspacePaths) {
			w.PrivateDir = filepath.Join(p.privateDir, "..", "..", "escape")
		}), "is not under"},
	}
	if err := p.checkContained(good); err != nil {
		t.Fatalf("checkContained(%+v) = %v, want nil", good, err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := p.checkContained(tt.paths)
			if !errors.Is(err, ErrPathEscape) {
				t.Fatalf("checkContained(%+v) = %v, want ErrPathEscape", tt.paths, err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("refused by the wrong rule: %v, want a refusal saying %q", err, tt.want)
			}
		})
	}
}

// The containers are checked as well as the paths, because a symlinked
// private/ redirects every workspace's private dir at once while each
// individual path still looks like a child of it. The sibling case for issues/
// is TestIssuesDirSymlinkEscapeFailsClosed.
func TestPrivateDirSymlinkEscapeFailsClosed(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	outside := t.TempDir()

	if err := os.MkdirAll(filepath.Dir(p.privateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, p.privateDir); err != nil {
		t.Fatal(err)
	}
	_, err := prepareForTest(t, p, ctx, issue("42"), 1)
	if !errors.Is(err, ErrPathEscape) {
		t.Fatalf("Prepare with a symlinked private/ = %v, want ErrPathEscape", err)
	}
	if !strings.Contains(err.Error(), p.privateDir) {
		t.Errorf("error %q does not name %s", err, p.privateDir)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("the escape target was written to: %v", entries)
	}
}
