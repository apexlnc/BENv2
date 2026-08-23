package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// A hook commit belongs to the attempt being prepared, not to a lost prior
// attempt. The fresh-claim snapshot must therefore be taken before both hooks,
// even though Prepare does not return until after they finish.
func TestPrepareWithLocalFactsCapturesFreshBranchBeforeCommitHooks(t *testing.T) {
	parallel(t)
	hookCommit := "touch hook.txt && git add hook.txt && " +
		"git -c user.name=hook -c user.email=hook@test.invalid commit --quiet -m hook"
	tests := []struct {
		name  string
		hooks Hooks
	}{
		{name: "after_create", hooks: Hooks{AfterCreate: hookCommit}},
		{name: "before_run", hooks: Hooks{BeforeRun: hookCommit}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			first := newProvider(t, f, Hooks{})
			if _, err := prepareForTest(t, first, ctx, issue("bootstrap"), 1); err != nil {
				t.Fatalf("bootstrap PrepareClaim: %v", err)
			}
			outgoing := f.head(t)
			runGit(t, first.baseDir, "update-ref", "refs/ben/base/7", outgoing)
			p, err := New(Options{
				Root: first.root, WorkflowKey: "wf", Repository: repo(f.origin),
				Hooks: tt.hooks, Locks: first.LockDomain(), Logger: quietLogger(),
			})
			if err != nil {
				t.Fatal(err)
			}
			const nextEpoch = 202
			if err := p.BeginClaimBase(ctx, issue("7"), nextEpoch); err != nil {
				t.Fatalf("BeginClaimBase: %v", err)
			}

			ws, facts, err := p.PrepareClaim(ctx, issue("7"), 1, nextEpoch)
			if err != nil {
				t.Fatalf("PrepareClaim: %v", err)
			}
			head := runGit(t, ws.Path, "rev-parse", "HEAD")
			if head == ws.BaseSHA {
				t.Fatal("the commit-producing hook did not advance the fresh branch")
			}
			if ws.ClaimEpoch != nextEpoch || ws.BaseSHA != outgoing {
				t.Errorf("new claim base = %d/%s, want %d/%s", ws.ClaimEpoch, ws.BaseSHA, nextEpoch, outgoing)
			}
			if facts.Head != outgoing || !facts.DescendsBase || facts.AdvancedPastBase(outgoing) {
				t.Errorf("pre-hook facts = %+v, want the branch at outgoing base %s", facts, outgoing)
			}
		})
	}
}

// A reattached local branch can prove prior work without a second origin
// observation. Move origin away in before_run: preparation's normal fetches
// have already completed, and any post-hook publication probe would now fail.
func TestPrepareWithLocalFactsNeedsNoPostHookOriginProbe(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	old, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	wantHead := agentCommit(t, old.Path, "prior-work.txt")
	offline := f.origin + ".offline"

	p2, err := New(Options{
		Root: p.root, WorkflowKey: "wf", Repository: repo(f.origin),
		Hooks:  Hooks{BeforeRun: fmt.Sprintf("mv %q %q", f.origin, offline)},
		Locks:  p.LockDomain(),
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	const nextEpoch = 202
	if err := p2.BeginClaimBase(ctx, issue("7"), nextEpoch); err != nil {
		t.Fatalf("BeginClaimBase: %v", err)
	}
	ws, facts, err := p2.PrepareClaim(ctx, issue("7"), 1, nextEpoch)
	if err != nil {
		t.Fatalf("PrepareClaim after origin went offline in before_run: %v", err)
	}
	if ws.BaseSHA != wantHead {
		t.Errorf("new base = %s, want pre-hook head %s", ws.BaseSHA, wantHead)
	}
	if facts.Head != wantHead || !facts.AdvancedPastBase(old.BaseSHA) {
		t.Errorf("local facts = %+v, want reattached head %s past outgoing base %s", facts, wantHead, old.BaseSHA)
	}
	if _, err := os.Stat(offline); err != nil {
		t.Fatalf("before_run did not move origin offline: %v", err)
	}
}

// A daemon starting against a workspace root nothing has written yet must be able
// to ask §9.10's question and be told "no pin", not "the base repository is in an
// unexpected state" — the answer that stranded a claim in the canary, because
// recovery retains what it cannot classify and no later tick creates a base
// repository on its behalf (#183).
//
// The provider is the real one and the root is genuinely fresh, which is the only
// place this is observable: every fake and every other test in this file reaches
// ResolveWorkspace past a Prepare that built base.git first.
func TestResolveWorkspaceOnFreshStorageReportsNoPin(t *testing.T) {
	parallel(t)
	// Any hook firing leaves this behind, outside the root so that the emptiness
	// check below cannot be what notices it.
	hookRan := filepath.Join(t.TempDir(), "hook-ran")
	touch := fmt.Sprintf("touch %q", hookRan)
	p := newProvider(t, newFixture(t), Hooks{
		AfterCreate: touch, BeforeRun: touch, AfterRun: touch, BeforeRemove: touch,
	})

	ws, ok, err := p.ResolveWorkspace(context.Background(), issue("7"))
	if err != nil {
		t.Fatalf("ResolveWorkspace on fresh storage: %v", err)
	}
	if ok || ws != (core.Workspace{}) {
		t.Errorf("ResolveWorkspace = (%+v, %v), want the zero workspace and no pin", ws, ok)
	}

	// Read-only, asserted at the root rather than path by path: a clone, a fetch
	// into a bootstrapped base.git, a worktree and a private dir all land under it,
	// so an empty root is the whole of "prepared nothing" in one assertion.
	entries, err := os.ReadDir(p.root)
	if err != nil {
		t.Fatalf("reading the workspace root: %v", err)
	}
	if len(entries) != 0 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the workspace root holds %v; a §9.10 resolution prepares nothing", names)
	}
	if _, err := os.Lstat(hookRan); err == nil {
		t.Error("a hook fired: resolution runs none (SPEC §5.2.6 hooks belong to Prepare and Dispose)")
	}
}

// Fresh storage is the *absence* of a base repository, and nothing else. One that
// exists and cannot be used stays the named refusal it was: §6.2 repairs nothing,
// and a read that answered "no pin" for a damaged repository would let recovery
// treat this daemon's own prepared work as work it never did (#183 must not
// weaken #16).
func TestResolveWorkspaceFailsClosedOnAnUnusableBase(t *testing.T) {
	parallel(t)
	tests := []struct {
		name  string
		setup func(t *testing.T, baseDir string)
	}{
		{
			name: "regular file",
			setup: func(t *testing.T, baseDir string) {
				mkdirAll(t, filepath.Dir(baseDir))
				writeFile(t, baseDir, "not a repo\n")
			},
		},
		{
			name:  "directory that is not a repository",
			setup: func(t *testing.T, baseDir string) { mkdirAll(t, baseDir) },
		},
		{
			// The entry is there and its target could reappear, so this is "cannot
			// know" rather than storage nobody has written — the distinction Lstat
			// draws here and in refStoreReadable.
			name: "dangling symlink",
			setup: func(t *testing.T, baseDir string) {
				mkdirAll(t, filepath.Dir(baseDir))
				if err := os.Symlink(filepath.Join(filepath.Dir(baseDir), "gone.git"), baseDir); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(t, newFixture(t), Hooks{})
			tt.setup(t, p.baseDir)

			_, ok, err := p.ResolveWorkspace(context.Background(), issue("7"))
			if !errors.Is(err, ErrBaseRepoState) {
				t.Fatalf("err = %v, want ErrBaseRepoState", err)
			}
			if ok {
				t.Error("a refusal must not also report a pin")
			}
			if _, err := os.Lstat(p.baseDir); err != nil {
				t.Errorf("no auto-repair: %s must be untouched (%v)", p.baseDir, err)
			}
		})
	}
}

// The read itself, once a base repository stands: this daemon's own pin comes
// back, and an issue it never prepared reports absent off the same repository.
// The pair is what keeps the fresh-storage answer above from being reached by a
// short circuit that swallows the real one.
func TestResolveWorkspaceReadsThePinOfAPreparedIssue(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	prepared, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	ws, ok, err := p.ResolveWorkspace(ctx, issue("7"))
	if err != nil || !ok {
		t.Fatalf("ResolveWorkspace of a prepared issue = (%v, %v)", ok, err)
	}
	if ws.BaseSHA != prepared.BaseSHA || ws.Key != prepared.Key ||
		ws.Branch != prepared.Branch || ws.WorkspacePaths != prepared.WorkspacePaths {
		t.Errorf("resolved %+v, want the workspace Prepare reported: %+v", ws, prepared)
	}

	other, ok, err := p.ResolveWorkspace(ctx, issue("8"))
	if err != nil {
		t.Fatalf("ResolveWorkspace of an unprepared issue: %v", err)
	}
	if ok || other != (core.Workspace{}) {
		t.Errorf("unprepared issue resolved to (%+v, %v), want no pin", other, ok)
	}
}

// The §9.7 git legs, read off real repositories: each case moves the branch,
// origin, or both into one shape and asserts the facts that shape produces.
// The verdicts they feed are internal/verify's; these are only the facts.
func TestPublishFacts(t *testing.T) {
	parallel(t)
	tests := []struct {
		name string
		// arrange mutates the prepared workspace and/or origin, then returns
		// the exact two SHAs it expects to be reported ("" for absent). Both
		// are named rather than copied from the result: a test that accepts
		// whatever the remote leg reports cannot catch it reading the wrong ref.
		arrange func(t *testing.T, f *fixture, p *Provider, ws core.Workspace) (head, remote string)
		want    core.PublishFacts // Head and RemoteHead come from arrange
	}{
		{
			// Leg 1 fails, so origin is never asked: RemoteProbed stays false.
			name: "prepared but untouched: branch sits at its base",
			arrange: func(_ *testing.T, _ *fixture, _ *Provider, ws core.Workspace) (string, string) {
				return ws.BaseSHA, ""
			},
			want: core.PublishFacts{DescendsBase: true},
		},
		{
			name: "committed but not pushed",
			arrange: func(t *testing.T, _ *fixture, _ *Provider, ws core.Workspace) (string, string) {
				return agentCommit(t, ws.Path, "work.txt"), ""
			},
			want: core.PublishFacts{DescendsBase: true, RemoteProbed: true},
		},
		{
			name: "committed and pushed: both git legs hold",
			arrange: func(t *testing.T, _ *fixture, _ *Provider, ws core.Workspace) (string, string) {
				head := agentCommit(t, ws.Path, "work.txt")
				agentPush(t, ws.Path)
				return head, head
			},
			want: core.PublishFacts{DescendsBase: true, RemoteProbed: true, RemoteHasHead: true},
		},
		{
			name: "pushed, then committed again: origin is missing the tail",
			arrange: func(t *testing.T, _ *fixture, _ *Provider, ws core.Workspace) (string, string) {
				first := agentCommit(t, ws.Path, "first.txt")
				agentPush(t, ws.Path)
				// The second commit never reaches origin: some of the work is
				// published, which is not the same as the work being published.
				return agentCommit(t, ws.Path, "second.txt"), first
			},
			want: core.PublishFacts{DescendsBase: true, RemoteProbed: true},
		},
		{
			name: "origin ahead of local: another daemon pushed on top",
			arrange: func(t *testing.T, f *fixture, _ *Provider, ws core.Workspace) (string, string) {
				head := agentCommit(t, ws.Path, "coder.txt")
				agentPush(t, ws.Path)
				// A reviser daemon (SPEC §5.1, #11) adds commits to the same
				// issue branch. Our commits are still published; an equality
				// check would call this unpublished and re-dispatch finished work.
				runGit(t, f.seed, "fetch", "--quiet", f.origin, ws.Branch+":reviser")
				runGit(t, f.seed, "checkout", "--quiet", "reviser")
				appendFile(t, filepath.Join(f.seed, "coder.txt"), "reviewed\n")
				runGit(t, f.seed, "commit", "--quiet", "-am", "reviser: address comments")
				runGit(t, f.seed, "push", "--quiet", f.origin, "reviser:"+ws.Branch)
				return head, runGit(t, f.seed, "rev-parse", "HEAD")
			},
			want: core.PublishFacts{DescendsBase: true, RemoteProbed: true, RemoteHasHead: true},
		},
		{
			name: "history rewritten off the base",
			arrange: func(t *testing.T, _ *fixture, _ *Provider, ws core.Workspace) (string, string) {
				// The base pin is origin's main at prepare; resetting behind it
				// leaves a head the claim-time base is not an ancestor of —
				// the force-push shape (BUILD.md B09 acceptance 3).
				runGit(t, ws.Path, "reset", "--hard", "--quiet", ws.BaseSHA+"~1")
				return runGit(t, ws.Path, "rev-parse", "HEAD"), ""
			},
			want: core.PublishFacts{},
		},
		{
			name: "no branch at all",
			arrange: func(t *testing.T, _ *fixture, p *Provider, ws core.Workspace) (string, string) {
				// Dispose removes the worktree; deleting the branch behind it
				// gives the "the run left nothing" fact a path that produces it.
				if err := p.Dispose(context.Background(), ws, false); err != nil {
					t.Fatalf("Dispose: %v", err)
				}
				runGit(t, p.baseDir, "branch", "-D", ws.Branch)
				return "", ""
			},
			want: core.PublishFacts{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			// Two commits on main so a rewrite has somewhere behind the base
			// to land; the base pin is the second.
			f.pushCommit(t, "second")
			p := newProvider(t, f, Hooks{})

			ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			want := tt.want
			want.Head, want.RemoteHead = tt.arrange(t, f, p, ws)

			got, err := p.PublishFacts(ctx, ws)
			if err != nil {
				t.Fatalf("PublishFacts: %v", err)
			}
			if got != want {
				t.Errorf("PublishFacts() = %+v, want %+v", got, want)
			}
		})
	}
}

// A contradiction the local repository has already settled must not become a
// verification error just because origin is unreachable: leg 1 gates the probe,
// so these verdicts survive a dead remote. The published shape, by contrast,
// genuinely needs origin and must fail closed without it.
func TestPublishFactsSkipsOriginWhenLegOneFails(t *testing.T) {
	parallel(t)
	tests := []struct {
		name    string
		arrange func(t *testing.T, ws core.Workspace)
		wantErr bool
	}{
		{
			name:    "no commits",
			arrange: func(*testing.T, core.Workspace) {},
		},
		{
			name: "history rewritten off the base",
			arrange: func(t *testing.T, ws core.Workspace) {
				runGit(t, ws.Path, "reset", "--hard", "--quiet", ws.BaseSHA+"~1")
			},
		},
		{
			name: "commits that do descend: leg 2 needs origin and cannot guess",
			arrange: func(t *testing.T, ws core.Workspace) {
				agentCommit(t, ws.Path, "work.txt")
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			f := newFixture(t)
			f.pushCommit(t, "second")
			p := newProvider(t, f, Hooks{})
			ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			tt.arrange(t, ws)

			// Origin goes away *after* Prepare: any probe from here on fails.
			if err := os.RemoveAll(f.origin); err != nil {
				t.Fatal(err)
			}

			facts, err := p.PublishFacts(ctx, ws)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("PublishFacts() = %+v, nil; leg 2 cannot be answered without origin", facts)
				}
				return
			}
			if err != nil {
				t.Fatalf("PublishFacts: %v — a local contradiction must not depend on the network", err)
			}
			if facts.RemoteProbed {
				t.Errorf("PublishFacts() = %+v; origin was probed for a branch that failed leg 1", facts)
			}
		})
	}
}

// Verification reads origin fresh. The prepare-time cache is stale by
// construction — the agent pushes after it — so a fact derived from it would
// report a published branch as unpublished on every first check.
func TestPublishFactsRefetchesOrigin(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})

	ws, err := prepareForTest(t, p, ctx, issue("7"), 1) // caches refs/ben/remote/7 as absent
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	head := agentCommit(t, ws.Path, "work.txt")
	agentPush(t, ws.Path)

	got, err := p.PublishFacts(ctx, ws)
	if err != nil {
		t.Fatalf("PublishFacts: %v", err)
	}
	if got.RemoteHead != head || !got.RemoteHasHead {
		t.Errorf("PublishFacts() = %+v; the push after Prepare must be visible", got)
	}
}

func TestPublishFactsRefusals(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	tests := []struct {
		name    string
		ws      core.Workspace
		wantErr error // nil: any error, just not success
	}{
		{"no key", core.Workspace{Branch: ws.Branch, BaseSHA: ws.BaseSHA}, nil},
		{"no branch", core.Workspace{Key: ws.Key, BaseSHA: ws.BaseSHA}, nil},
		{
			// An unpinned workspace is ambiguous state, not an empty base:
			// treating "" as the base would make every branch descend from it.
			name:    "no claim-time base",
			ws:      core.Workspace{Key: ws.Key, Branch: ws.Branch},
			wantErr: ErrWorkspaceState,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			facts, err := p.PublishFacts(ctx, tt.ws)
			if err == nil {
				t.Fatalf("PublishFacts(%+v) = %+v, nil; want a refusal", tt.ws, facts)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// Damage is never a verdict (SPEC §6.6): an unreadable base repository must
// error rather than report an absent branch, which verification would read as
// "the run added no commits" and park a run that may well have published.
func TestPublishFactsFailsClosedOnBaseDamage(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	f := newFixture(t)
	p := newProvider(t, f, Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	agentCommit(t, ws.Path, "work.txt")

	// Point the branch ref at an object that is not there.
	writeFile(t, p.baseDir+"/refs/heads/"+ws.Branch, "0000000000000000000000000000000000000000\n")

	if facts, err := p.PublishFacts(ctx, ws); err == nil {
		t.Fatalf("PublishFacts() = %+v, nil; a broken ref must fail closed", facts)
	}
}

// --- the prior-attempt account (SPEC §9.6, #61) ---

// commitSubject makes one commit with a chosen subject and file, which is what a
// prior-attempt account reports back — and the two strings the agent authors.
func commitSubject(t *testing.T, wsPath, file, subject string) {
	t.Helper()
	writeFile(t, filepath.Join(wsPath, file), file+"\n")
	runGit(t, wsPath, "add", ".")
	runGit(t, wsPath, "commit", "--quiet", "-m", subject)
}

// What the attempt left on its branch: the commits past the claim-time base,
// newest first, and the files they changed.
func TestAttemptFactsReportsCommitsAndFilesPastTheBase(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	commitSubject(t, ws.Path, "first.go", "parse the header")
	commitSubject(t, ws.Path, "second.go", "drop the retry counter")

	facts, err := p.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts: %v", err)
	}
	if len(facts.Commits) != 2 {
		t.Fatalf("commits = %v, want two", facts.Commits)
	}
	// Newest first: what the attempt did last is what says where it got to.
	if !strings.HasSuffix(facts.Commits[0], " drop the retry counter") {
		t.Errorf("commits[0] = %q, want the newest commit's subject", facts.Commits[0])
	}
	if !strings.HasSuffix(facts.Commits[1], " parse the header") {
		t.Errorf("commits[1] = %q, want the older commit's subject", facts.Commits[1])
	}
	if got := facts.Files; len(got) != 2 || got[0] != "first.go" || got[1] != "second.go" {
		t.Errorf("files = %v, want both files the commits added", got)
	}
	if facts.CommitsTruncated || facts.FilesTruncated {
		t.Errorf("facts = %+v, want no truncation for two commits", facts)
	}
}

// An attempt that committed nothing has an empty account, and it is a *successful*
// read: "committed nothing" is a true thing to tell the next attempt, where
// "could not look" is not.
func TestAttemptFactsIsEmptyWithoutCommits(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	facts, err := p.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts on a branch that never moved: %v", err)
	}
	if len(facts.Commits) != 0 || len(facts.Files) != 0 {
		t.Errorf("facts = %+v, want an empty account", facts)
	}
}

// A rewritten branch cannot be said to have "added" anything past the pin, so
// nothing is claimed about it — the same line PublishFacts draws for §9.7 leg 1.
func TestAttemptFactsClaimsNothingAboutARewrittenBranch(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	commitSubject(t, ws.Path, "work.go", "some work")
	// An orphan commit: a branch that no longer descends from the base.
	runGit(t, ws.Path, "checkout", "--quiet", "--orphan", "rewritten")
	runGit(t, ws.Path, "commit", "--quiet", "--allow-empty", "-m", "rewritten")
	rewritten := runGit(t, ws.Path, "rev-parse", "HEAD")
	runGit(t, ws.Path, "update-ref", "refs/heads/"+ws.Branch, rewritten)

	facts, err := p.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts on a rewritten branch: %v", err)
	}
	if len(facts.Commits) != 0 || len(facts.Files) != 0 {
		t.Errorf("facts = %+v, want nothing claimed about a branch that does not descend from the pin", facts)
	}
}

// The provider's own bound, and it reports what it dropped rather than leaving a
// suspiciously round count to be inferred.
func TestAttemptFactsBoundsWhatItReads(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for i := range maxAttemptCommits + 3 {
		commitSubject(t, ws.Path, fmt.Sprintf("file%03d.go", i), fmt.Sprintf("commit %d", i))
	}

	facts, err := p.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts: %v", err)
	}
	if len(facts.Commits) != maxAttemptCommits || !facts.CommitsTruncated {
		t.Errorf("commits = %d truncated=%v, want %d and a stated truncation",
			len(facts.Commits), facts.CommitsTruncated, maxAttemptCommits)
	}
}

// The claim-time pin is what "past the base" is measured against, so an account
// cannot be read without one — the same refusal PublishFacts makes, and for the
// same reason: without the pin every branch trivially descends from nothing.
func TestAttemptFactsRefusesAWorkspaceWithNoPin(t *testing.T) {
	parallel(t)
	_, err := newProvider(t, newFixture(t), Hooks{}).AttemptFacts(context.Background(),
		core.Workspace{Key: "7", Branch: "ben/7"})
	if !errors.Is(err, ErrWorkspaceState) {
		t.Errorf("AttemptFacts with no BaseSHA = %v, want ErrWorkspaceState", err)
	}
}

// The bound is at the source, not after the fact (#61 review, finding 1). A
// commit subject is one line of a message the agent wrote and a path is as deep as
// the filesystem allows, so a repository an agent controls could otherwise make a
// read whose result is discarded a moment later cost gigabytes.
//
// Asserted on the kept line rather than on memory, because the length of what is
// kept is the observable half of "it was never buffered": a helper that read
// everything and then cut would pass a memory assertion nobody can write and fail
// this one only if it also forgot to cut.
func TestAttemptFactsBoundsEachLineItKeeps(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	huge := strings.Repeat("s", 64<<10)
	commitSubject(t, ws.Path, "some.go", huge)

	facts, err := p.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts: %v", err)
	}
	if len(facts.Commits) != 1 {
		t.Fatalf("commits = %d, want the one commit", len(facts.Commits))
	}
	if got := len(facts.Commits[0]); got > maxCommitLineBytes {
		t.Errorf("commit line is %d bytes, over the %d bound", got, maxCommitLineBytes)
	}
	if !facts.CommitsTruncated {
		t.Error("a commit subject was cut without the account reporting it")
	}
	if !strings.HasSuffix(facts.Commits[0], "s") {
		t.Errorf("the kept prefix is not the subject: %q", facts.Commits[0])
	}
}

// The reader stops at the bound rather than draining a listing it will not use, and
// the exit status of the git it killed for that must not be mistaken for a failure.
func TestBoundedLinesStopsAtTheBoundAndReportsIt(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name          string
		in            string
		maxLines      int
		maxLineBytes  int
		wantLines     []string
		wantTruncated bool
		wantShort     bool
	}{
		{"exactly at the bound is complete", "a\nb\nc\n", 3, 8, []string{"a", "b", "c"}, false, false},
		{"one past the bound is cut", "a\nb\nc\nd\n", 3, 8, []string{"a", "b", "c"}, true, true},
		{"an over-long line is cut, and reading continues", "aaaaaaaaaaaa\nb\n", 3, 4,
			[]string{"aaaa", "b"}, true, false},
		{"no trailing newline", "a\nb", 3, 8, []string{"a", "b"}, false, false},
		{"empty", "", 3, 8, nil, false, false},
		// A rune split by the byte cap is dropped rather than kept broken: it ends
		// up inside a fence (SPEC §5.6).
		{"a split rune is dropped", "x日本\n", 3, 3, []string{"x"}, true, false},
		// And a *complete* trailing rune is kept. The first version of the cut
		// walked back over every trailing continuation byte, so it turned "café"
		// into "caf" on a line it had not cut at all — a silent loss, in the one
		// function whose job is to make cuts reportable (#61 re-review, finding 2).
		{"a complete trailing rune survives", "café\n", 3, 8, []string{"café"}, false, false},
		{"a complete multi-byte line survives", "x日本\n", 3, 16, []string{"x日本"}, false, false},
		{"cut exactly between runes", "x日本\n", 3, 4, []string{"x日"}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines, truncated, short, err := boundedLines(strings.NewReader(tc.in), tc.maxLines, tc.maxLineBytes)
			if err != nil {
				t.Fatalf("boundedLines: %v", err)
			}
			if !slices.Equal(lines, tc.wantLines) {
				t.Errorf("lines = %q, want %q", lines, tc.wantLines)
			}
			if truncated != tc.wantTruncated || short != tc.wantShort {
				t.Errorf("truncated=%v short=%v, want %v/%v", truncated, short, tc.wantTruncated, tc.wantShort)
			}
			for _, l := range lines {
				if !utf8.ValidString(l) {
					t.Errorf("line %q is not valid UTF-8", l)
				}
			}
		})
	}
}

// A git command that fails on its own must still fail. It is the same code path as
// the one that kills git deliberately, and conflating the two would turn
// `merge-base --is-ancestor`'s exit 1 — a verdict — into a silent empty success.
func TestGitLinesStillReportsAGenuineFailure(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	// Prepared first, so base.git exists: the failure under test is git's verdict
	// on a revision, not a missing directory.
	if _, err := prepareForTest(t, p, ctx, issue("7"), 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	_, _, err := p.gitLines(ctx, 10, 100, "rev-list", "no-such-revision")
	if err == nil {
		t.Fatal("gitLines on a bad revision = nil, want the failure")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("gitLines = %v, want git's own exit status to survive", err)
	}
}

// The lock wait is bounded, because the mutexes here are held across `git fetch`
// and this is the one read for which not answering is a legitimate result. Without
// it a finished attempt stalls for as long as a hung fetch on another issue lasts,
// holding a §9.5 slot and blocking the §11 drain (#61 review, finding 2).
func TestAttemptFactsGivesUpOnAHeldLock(t *testing.T) {
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, context.Background(), issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Held by somebody else and never released, which is what a fetch into a
	// black hole looks like from here.
	held := p.locks.forIssue(ws.Key)
	held.Lock()
	defer held.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := p.AttemptFacts(ctx, ws); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AttemptFacts under a held lock = %v, want the deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("gave up after %v; the wait is not bounded by the context", elapsed)
	}
}

// The same bound on the base repository lock, which is the one a fetch for a
// *different* issue holds — so it is reachable without this issue's own lock being
// contended at all.
func TestGitLinesGivesUpOnAHeldBaseLock(t *testing.T) {
	p := newProvider(t, newFixture(t), Hooks{})
	p.locks.baseMu.Lock()
	defer p.locks.baseMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := p.gitLines(ctx, 10, 100, "rev-parse", "HEAD"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("gitLines under a held base lock = %v, want the deadline", err)
	}
}

// The cut keeps whole runes and drops only broken ones. Driven directly because the
// line-reader cases above cannot reach every shape: a lone continuation byte, a lead
// byte with nothing after it, and a real U+FFFD that must not be mistaken for the
// decoder's error rune.
func TestTrimPartialRune(t *testing.T) {
	parallel(t)
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"plain", "plain"},
		{"café", "café"},     // complete: untouched
		{"caf\xc3", "caf"},   // lead byte, continuation cut off
		{"x日本", "x日本"},       // complete
		{"x日\xe6\x9c", "x日"}, // two-thirds of a three-byte rune
		{"x\xe6", "x"},       // lead byte alone
		{"ok\xff", "ok"},     // not a rune in any encoding
		{"\ufffd", "\ufffd"}, // a real replacement character, encoded whole
		{"ok\ufffd", "ok\ufffd"},
	} {
		if got := trimPartialRune(tc.in); got != tc.want {
			t.Errorf("trimPartialRune(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if !utf8.ValidString(trimPartialRune(tc.in)) && utf8.ValidString(tc.in) {
			t.Errorf("trimPartialRune(%q) made valid input invalid", tc.in)
		}
	}
}

// Exit 1 is git's "no"; every other failure is a repository nobody could read, and
// the account must report that as unread rather than as "committed nothing" — the
// fabrication SPEC §5.6 forbids, and one an agent acts on (#61 re-review, finding 3).
func TestAttemptFactsReportsAnUnresolvableBranchAsUnread(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	p := newProvider(t, newFixture(t), Hooks{})
	ws, err := prepareForTest(t, p, ctx, issue("7"), 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	// The pin stands and the branch does not: `merge-base --is-ancestor` exits 128
	// here, not 1, so it is trouble rather than a verdict.
	runGit(t, p.baseDir, "update-ref", "-d", "refs/heads/"+ws.Branch)

	if _, err := p.AttemptFacts(ctx, ws); !errors.Is(err, ErrBaseRepoState) {
		t.Errorf("AttemptFacts on an unresolvable branch = %v, want ErrBaseRepoState", err)
	}

	// The verdict half still works: a branch that exists and does not descend from
	// the pin is exit 1, and that is an empty account rather than an error.
	runGit(t, ws.Path, "checkout", "--quiet", "--orphan", "rewritten")
	runGit(t, ws.Path, "commit", "--quiet", "--allow-empty", "-m", "rewritten")
	runGit(t, p.baseDir, "update-ref", "refs/heads/"+ws.Branch,
		runGit(t, ws.Path, "rev-parse", "HEAD"))

	facts, err := p.AttemptFacts(ctx, ws)
	if err != nil {
		t.Fatalf("AttemptFacts on a rewritten branch = %v, want the empty account", err)
	}
	if len(facts.Commits) != 0 {
		t.Errorf("facts = %+v, want nothing claimed", facts)
	}
}
