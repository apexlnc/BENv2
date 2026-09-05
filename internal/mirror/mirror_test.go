package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
	"github.com/srhg-ai-7cef3f93/ben/internal/mirror/mirrortest"
)

// TestMain pins git to a hermetic configuration: no user or system config, a
// fixed identity for the commits the fixtures create.
func TestMain(m *testing.M) {
	os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	os.Setenv("GIT_AUTHOR_NAME", "ben-test")
	os.Setenv("GIT_AUTHOR_EMAIL", "ben@test.invalid")
	os.Setenv("GIT_COMMITTER_NAME", "ben-test")
	os.Setenv("GIT_COMMITTER_EMAIL", "ben@test.invalid")
	os.Exit(m.Run())
}

// Nothing here runs in parallel. These tests drive real git against real
// repositories, and three of them replace `git` on PATH or read a process-wide
// trace; the suite is small enough that sequential is cheaper than the
// bookkeeping that would make it safe to widen (compare internal/partest, which
// exists for suites where it is not).

var commitSeq atomic.Uint64

// origin is a canonical remote: a bare repository with a default branch, which
// the fixtures then move around the way a run and a human would.
type origin struct{ path string }

func newOrigin(t *testing.T) *origin {
	t.Helper()
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed")
	runGit(t, dir, "init", "--quiet", "-b", "main", seed)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", ".")
	runGit(t, seed, "commit", "--quiet", "-m", "seed")
	path := filepath.Join(dir, "origin.git")
	runGit(t, dir, "clone", "--quiet", "--bare", seed, path)
	return &origin{path: path}
}

// commit appends a commit to a branch, creating it from the default branch when
// it does not exist. Plumbing rather than a worktree: nothing here needs a
// checkout, and `commit-tree` keeps a case that moves a branch five times to
// five git invocations.
func (o *origin) commit(t *testing.T, branch string) string {
	t.Helper()
	parent := o.resolve(t, "refs/heads/"+branch)
	if parent == "" {
		parent = o.resolve(t, "refs/heads/main")
	}
	sha := runGit(t, o.path, "commit-tree", parent+"^{tree}", "-p", parent,
		"-m", fmt.Sprintf("work %d", commitSeq.Add(1)))
	runGit(t, o.path, "update-ref", "refs/heads/"+branch, sha)
	return sha
}

// rewrite replaces a branch with a root commit: unrelated history, which is
// what a force push of somebody else's work looks like from the outside.
func (o *origin) rewrite(t *testing.T, branch string) string {
	t.Helper()
	main := o.resolve(t, "refs/heads/main")
	sha := runGit(t, o.path, "commit-tree", main+"^{tree}",
		"-m", fmt.Sprintf("rewritten %d", commitSeq.Add(1)))
	runGit(t, o.path, "update-ref", "refs/heads/"+branch, sha)
	return sha
}

func (o *origin) delete(t *testing.T, branch string) {
	t.Helper()
	runGit(t, o.path, "update-ref", "-d", "refs/heads/"+branch)
}

// resolve returns the commit a ref names, "" when there is no such ref.
func (o *origin) resolve(t *testing.T, ref string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	cmd.Dir = o.path
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newMirror builds a mirror over a fresh root for one origin.
func newMirror(t *testing.T, o *origin) *Mirror {
	t.Helper()
	return newMirrorAt(t, o, t.TempDir(), nil)
}

func newMirrorAt(t *testing.T, o *origin, root string, auth core.RemoteAuthSource) *Mirror {
	t.Helper()
	m, err := New(Options{
		Root:       root,
		Repository: core.Repository{RemoteURL: o.path, AuthSource: auth},
		Now:        func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) },
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func ref(key string, epoch int64) core.RemoteClaimRef {
	return core.RemoteClaimRef{Issue: key, Key: key, Epoch: epoch}
}

func run(r core.RemoteClaimRef, id, verification string) core.RemoteRunRef {
	return core.RemoteRunRef{Claim: r, Run: id, Verification: verification}
}

func TestReadyAuthenticatesAndRequiresTheSelectedBranch(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	o.commit(t, "release/v2")
	auth := fake.NewRemoteAuth("x-access-token", "ready-token")
	m, err := New(Options{
		Root: t.TempDir(), Repository: core.Repository{RemoteURL: o.path, AuthSource: auth},
		BaseBranch: "release/v2", Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Ready(ctx); err != nil {
		t.Fatalf("Ready(configured branch): %v", err)
	}
	if auth.Calls() == 0 {
		t.Fatal("Ready did not obtain the mirror credential")
	}

	missing, err := New(Options{
		Root: t.TempDir(), Repository: core.Repository{RemoteURL: o.path},
		BaseBranch: "valid-but-absent", Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := missing.Ready(ctx); !errors.Is(err, ErrBaseBranchNotFound) {
		t.Fatalf("Ready(absent branch) = %v, want ErrBaseBranchNotFound", err)
	}

	runGit(t, o.path, "symbolic-ref", "HEAD", "refs/heads/absent-default")
	omitted, err := New(Options{
		Root: t.TempDir(), Repository: core.Repository{RemoteURL: o.path}, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := omitted.Ready(ctx); !errors.Is(err, ErrBaseBranchNotFound) {
		t.Fatalf("Ready(absent repository default) = %v, want ErrBaseBranchNotFound", err)
	}
	runGit(t, o.path, "symbolic-ref", "HEAD", "refs/heads/main")

	o.commit(t, "ben/7")
	runGit(t, o.path, "symbolic-ref", "HEAD", "refs/heads/ben/7")
	reserved, err := New(Options{
		Root: t.TempDir(), Repository: core.Repository{RemoteURL: o.path}, Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reserved.Ready(ctx); !errors.Is(err, ErrBaseBranchReserved) {
		t.Fatalf("Ready(reserved repository default) = %v, want ErrBaseBranchReserved", err)
	}
	runGit(t, o.path, "symbolic-ref", "HEAD", "refs/heads/main")

	credentialErr := errors.New("credential unavailable")
	blockedAuth := fake.NewRemoteAuth("x-access-token", "unused")
	blockedAuth.Err = credentialErr
	blocked, err := New(Options{
		Root: t.TempDir(), Repository: core.Repository{RemoteURL: o.path, AuthSource: blockedAuth},
		BaseBranch: "release/v2", Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := blocked.Ready(ctx); !errors.Is(err, credentialErr) {
		t.Fatalf("Ready credential failure = %v, want original error preserved", err)
	}
}

func TestClaimsRetainTargetsAcrossRepositoryDefaultMovement(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	firstRef := ref("41", 1)
	first, err := m.RecordClaim(ctx, firstRef)
	if err != nil {
		t.Fatal(err)
	}
	if first.TargetBranch != "main" {
		t.Fatalf("first target = %q, want main", first.TargetBranch)
	}

	o.commit(t, "trunk")
	runGit(t, o.path, "symbolic-ref", "HEAD", "refs/heads/trunk")
	second, err := m.RecordClaim(ctx, ref("42", 1))
	if err != nil {
		t.Fatal(err)
	}
	if second.TargetBranch != "trunk" {
		t.Fatalf("second target = %q, want trunk", second.TargetBranch)
	}
	retained, err := m.Claim(ctx, firstRef)
	if err != nil || retained.TargetBranch != "main" {
		t.Fatalf("first claim after movement = %+v, %v; want retained main", retained, err)
	}
}

func TestLegacyTargetlessClaimIsNonAuthorizingUntilALaterEpoch(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	firstRef := ref("42", 1)
	first, err := m.RecordClaim(ctx, firstRef)
	if err != nil {
		t.Fatal(err)
	}
	record, err := m.readRecord(firstRef.Key)
	if err != nil {
		t.Fatal(err)
	}
	record.TargetBranch = ""
	if err := m.writeRecord(record); err != nil {
		t.Fatal(err)
	}

	if _, err := m.Claim(ctx, firstRef); !errors.Is(err, ErrClaimTargetUnrecorded) {
		t.Fatalf("Claim(legacy) = %v, want ErrClaimTargetUnrecorded", err)
	}
	if _, err := m.RecordClaim(ctx, firstRef); !errors.Is(err, ErrClaimTargetUnrecorded) {
		t.Fatalf("RecordClaim(same legacy epoch) = %v, want ErrClaimTargetUnrecorded", err)
	}

	runGit(t, o.path, "update-ref", "refs/heads/"+Branch(firstRef.Key), first.BaseSHA)
	o.commit(t, "trunk")
	runGit(t, o.path, "symbolic-ref", "HEAD", "refs/heads/trunk")
	next, err := m.RecordClaim(ctx, ref("42", 2))
	if err != nil {
		t.Fatalf("RecordClaim(later epoch): %v", err)
	}
	if next.TargetBranch != "trunk" || next.BaseSHA != first.BaseSHA {
		t.Fatalf("later claim = %+v, want retained branch base %s and new target trunk", next, first.BaseSHA)
	}
}

// The store is held to the shared contract against a real repository, which is
// the half internal/fake's run of the same suite cannot cover: here the answers
// come from git's own ancestry, ref resolution and absence verdicts.
func TestMirrorMeetsTheFactSourceContract(t *testing.T) {
	mirrortest.Contract(t, func(t *testing.T) mirrortest.Harness {
		o := newOrigin(t)
		return mirrortest.Harness{
			Store:   newMirror(t, o),
			Commit:  o.commit,
			Rewrite: o.rewrite,
			Delete:  o.delete,
		}
	})
}

// Assembly asks for the verifier's target branch before the first claim is
// recorded. That read is therefore the mirror's first use on a fresh daemon and
// must bootstrap the store it runs Git from.
func TestDefaultBranchBootstrapsAFreshStore(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)

	if _, err := os.Stat(m.gitDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fresh mirror Git directory stat error = %v, want not exist", err)
	}
	got, err := m.DefaultBranch(ctx)
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}
	if got != "main" {
		t.Fatalf("DefaultBranch = %q, want main", got)
	}
	if bare := runGit(t, m.gitDir, "rev-parse", "--is-bare-repository"); bare != "true" {
		t.Fatalf("bootstrapped repository is bare = %q, want true", bare)
	}
}

// A pin outlives the process that took it. The store is the one place in BEN
// that holds a fact the outside world cannot restate: after the run has pushed,
// nothing left anywhere says where its branch started.
func TestPinSurvivesARestart(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	root := t.TempDir()
	r := ref("42", 1)

	first := newMirrorAt(t, o, root, nil)
	claim, err := first.RecordClaim(ctx, r)
	if err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	head := o.commit(t, Branch(r.Key))

	// A different Mirror value over the same root: the daemon restarted, and
	// every in-memory state it had is gone.
	second := newMirrorAt(t, o, root, nil)
	read, err := second.Claim(ctx, r)
	if err != nil {
		t.Fatalf("Claim after restart: %v", err)
	}
	if read.BaseSHA != claim.BaseSHA {
		t.Errorf("base after restart = %s, pinned %s", read.BaseSHA, claim.BaseSHA)
	}
	facts, err := second.RemoteFacts(ctx, run(r, "run-1", "v1"))
	if err != nil {
		t.Fatalf("RemoteFacts after restart: %v", err)
	}
	if facts.RemoteHead != head || !facts.DescendsBase {
		t.Errorf("facts after restart = %+v, want head %s descending from %s", facts, head, claim.BaseSHA)
	}
}

// Git's repository-local environment belongs to whichever repository started
// the daemon, not to BEN. In particular GIT_OBJECT_DIRECTORY redirects fetched
// objects while leaving the mirror's ref behind: RecordClaim would then appear
// to succeed, but a restart without that ambient directory could not resolve its
// supposedly durable pin.
func TestPinIgnoresAnAmbientGitObjectDirectory(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t) // Build the world before introducing the hostile ambient value.
	root := t.TempDir()
	outside := t.TempDir()
	t.Setenv("GIT_OBJECT_DIRECTORY", outside)
	r := ref("42", 1)

	first := newMirrorAt(t, o, root, nil)
	want, err := first.RecordClaim(ctx, r)
	if err != nil {
		t.Fatalf("RecordClaim with ambient object directory: %v", err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("ambient object directory received %d entries; mirror objects escaped %s", len(entries), first.gitDir)
	}

	// A restarted daemon need not inherit the shell repository that happened to
	// launch the first one. The pin is durable only if the mirror resolves it with
	// that ambient redirect gone.
	if err := os.Unsetenv("GIT_OBJECT_DIRECTORY"); err != nil {
		t.Fatal(err)
	}
	second := newMirrorAt(t, o, root, nil)
	got, err := second.Claim(ctx, r)
	if err != nil {
		t.Fatalf("Claim after restart without ambient object directory: %v", err)
	}
	if got != want {
		t.Fatalf("Claim after restart = %+v, want %+v", got, want)
	}
}

// Rename alone does not make the store's destination entry durable. A remote
// run may start only after RecordClaim returns, so a failed parent-directory
// sync must keep RecordClaim from authorizing that start, and a retry must be
// able to finish publication of the already-complete store.
func TestRecordClaimWaitsForDurableStorePublication(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	root := t.TempDir()
	m := newMirrorAt(t, o, root, nil)
	realSync := m.syncDirectory

	treeErr := errors.New("sync initialized tree")
	m.syncDirectory = func(dir string) error {
		if filepath.Base(dir) == claimsDirName {
			return treeErr
		}
		return realSync(dir)
	}
	if _, err := m.RecordClaim(ctx, ref("42", 1)); !errors.Is(err, treeErr) {
		t.Fatalf("RecordClaim with failed initialized-tree sync = %v, want %v", err, treeErr)
	}
	if present, err := m.present(); err != nil || present {
		t.Fatalf("store after failed initialized-tree sync = present %t, error %v; want absent", present, err)
	}

	parentErr := errors.New("sync destination parent")
	parentSyncs := 0
	m.syncDirectory = func(dir string) error {
		if filepath.Clean(dir) == filepath.Clean(root) {
			parentSyncs++
			return parentErr
		}
		return realSync(dir)
	}

	if _, err := m.RecordClaim(ctx, ref("42", 1)); !errors.Is(err, parentErr) {
		t.Fatalf("RecordClaim with failed store publication sync = %v, want %v", err, parentErr)
	}
	if parentSyncs != 1 {
		t.Fatalf("destination parent synced %d times, want once", parentSyncs)
	}
	if _, err := os.Stat(m.recordPath("42")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim record after failed store publication sync: %v, want absent", err)
	}

	m.syncDirectory = realSync
	if _, err := m.RecordClaim(ctx, ref("42", 1)); err != nil {
		t.Fatalf("RecordClaim retry: %v", err)
	}
}

// Rename makes a record visible before the directory sync makes it durable. A
// retry that finds that record must finish the failed sync and the pruning that
// follows it before it can authorize the run.
func TestRecordClaimRetryFinishesAVisibleRecord(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	first, second := ref("42", 1), ref("42", 2)
	if _, err := m.RecordClaim(ctx, first); err != nil {
		t.Fatalf("RecordClaim (first epoch): %v", err)
	}

	realSync := m.syncDirectory
	syncErr := errors.New("sync claim record")
	failed := false
	m.syncDirectory = func(dir string) error {
		if !failed && filepath.Clean(dir) == filepath.Clean(m.claimsDir) {
			failed = true
			return syncErr
		}
		return realSync(dir)
	}
	if _, err := m.RecordClaim(ctx, second); !errors.Is(err, syncErr) {
		t.Fatalf("RecordClaim with failed record sync = %v, want %v", err, syncErr)
	}
	if !failed {
		t.Fatal("the record-directory sync was not reached; the case asserts nothing")
	}
	if record, err := m.readRecord(second.Key); err != nil || record.Epoch != second.Epoch {
		t.Fatalf("visible record after failed sync = %+v, %v; want epoch %d", record, err, second.Epoch)
	}
	if _, ok, err := m.revParse(ctx, m.pinRef(first)); err != nil || !ok {
		t.Fatalf("old pin after interrupted pruning = present %t, error %v; want present", ok, err)
	}

	retrySyncs := 0
	m.syncDirectory = func(dir string) error {
		if filepath.Clean(dir) == filepath.Clean(m.claimsDir) {
			retrySyncs++
		}
		return realSync(dir)
	}
	if _, err := m.RecordClaim(ctx, second); err != nil {
		t.Fatalf("RecordClaim retry: %v", err)
	}
	if retrySyncs == 0 {
		t.Fatal("RecordClaim retry did not repeat the failed record-directory sync")
	}
	if _, ok, err := m.revParse(ctx, m.pinRef(first)); err != nil || ok {
		t.Fatalf("old pin after retry = present %t, error %v; want pruned", ok, err)
	}
}

// Git fsyncs the files written by fetch; the mirror owns the directory entries
// that make those files and the pin ref durable as one fact. Neither failed sync
// may be followed by a claim record that authorizes a run.
func TestRecordClaimWaitsForDurableGitDirectories(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target func(m *Mirror, r core.RemoteClaimRef) string
	}{
		{
			name: "objects",
			target: func(m *Mirror, _ core.RemoteClaimRef) string {
				return filepath.Join(m.gitDir, "objects", "pack")
			},
		},
		{
			name: "pin ref",
			target: func(m *Mirror, r core.RemoteClaimRef) string {
				return filepath.Dir(filepath.Join(m.gitDir, filepath.FromSlash(m.pinRef(r))))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			o := newOrigin(t)
			m := newMirror(t, o)
			if err := m.ensure(ctx); err != nil {
				t.Fatalf("ensure: %v", err)
			}
			r := ref("42", 1)
			target := filepath.Clean(tc.target(m, r))
			realSync := m.syncDirectory
			syncErr := errors.New("sync Git directory")
			failed := false
			m.syncDirectory = func(dir string) error {
				if !failed && filepath.Clean(dir) == target {
					failed = true
					return syncErr
				}
				return realSync(dir)
			}

			if _, err := m.RecordClaim(ctx, r); !errors.Is(err, syncErr) {
				t.Fatalf("RecordClaim with failed Git-directory sync = %v, want %v", err, syncErr)
			}
			if !failed {
				t.Fatal("target Git-directory sync was not reached; the case asserts nothing")
			}
			if _, err := os.Stat(m.recordPath(r.Key)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("claim record after failed Git-directory sync: %v, want absent", err)
			}

			m.syncDirectory = realSync
			if _, err := m.RecordClaim(ctx, r); err != nil {
				t.Fatalf("RecordClaim retry: %v", err)
			}
		})
	}
}

// A pin the store can no longer resolve is a park, not a re-pin. Re-taking it
// here would take it from a branch the run has already moved, and leg 1 would
// then be measuring the run against itself.
func TestALostPinRefuses(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	r := ref("42", 1)
	if _, err := m.RecordClaim(ctx, r); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	o.commit(t, Branch(r.Key))
	runGit(t, m.gitDir, "update-ref", "-d", m.pinRef(r))

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Claim", func() error { _, err := m.Claim(ctx, r); return err }},
		{"RemoteFacts", func() error { _, err := m.RemoteFacts(ctx, run(r, "run-1", "v1")); return err }},
		{"RecordClaim", func() error { _, err := m.RecordClaim(ctx, r); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrClaimPinLost) {
				t.Fatalf("error = %v, want ErrClaimPinLost", err)
			}
		})
	}
}

// Concurrent claims over one store: distinct issues, distinct epochs, one
// object store and one ref namespace. Under -race this also covers the locking.
func TestConcurrentClaimsDoNotMix(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)

	const issues = 6
	heads := make([]string, issues)
	refs := make([]core.RemoteClaimRef, issues)
	for i := range issues {
		refs[i] = ref(fmt.Sprintf("issue-%d", i), int64(i+1))
		if _, err := m.RecordClaim(ctx, refs[i]); err != nil {
			t.Fatalf("RecordClaim %d: %v", i, err)
		}
		heads[i] = o.commit(t, Branch(refs[i].Key))
	}

	var wg sync.WaitGroup
	facts := make([]core.RemotePublishFacts, issues)
	errs := make([]error, issues)
	for i := range issues {
		wg.Add(1)
		go func() {
			defer wg.Done()
			facts[i], errs[i] = m.RemoteFacts(ctx, run(refs[i], fmt.Sprintf("run-%d", i), fmt.Sprintf("v-%d", i)))
		}()
	}
	wg.Wait()

	for i := range issues {
		if errs[i] != nil {
			t.Fatalf("RemoteFacts %d: %v", i, errs[i])
		}
		if facts[i].Branch != Branch(refs[i].Key) {
			t.Errorf("issue %d observed branch %s", i, facts[i].Branch)
		}
		if facts[i].RemoteHead != heads[i] {
			t.Errorf("issue %d observed head %s, its branch is at %s", i, facts[i].RemoteHead, heads[i])
		}
		if !facts[i].DescendsBase {
			t.Errorf("issue %d does not descend from its own base", i)
		}
	}
}

// One root, two repositories: the store must be provably the one this mirror
// reads. The identity file is what turns a copied tree, a re-pointed root or a
// digest collision into a refusal instead of one repository's commits being
// ordered against another's pin.
func TestAStoreForAnotherRepositoryRefuses(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	r := ref("42", 1)
	if _, err := m.RecordClaim(ctx, r); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}

	if err := os.WriteFile(filepath.Join(m.Dir(), repositoryFileName), []byte("github.test/someone/else\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Claim(ctx, r); !errors.Is(err, ErrRepositoryMismatch) {
		t.Fatalf("Claim against a foreign store: %v, want ErrRepositoryMismatch", err)
	}
}

// Two repositories under one root get two stores, and neither can see the
// other's pins — the "concurrent repositories" half of the durability rule.
func TestTwoRepositoriesShareARootWithoutSharingPins(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	first, second := newOrigin(t), newOrigin(t)
	a := newMirrorAt(t, first, root, nil)
	b := newMirrorAt(t, second, root, nil)

	if a.Dir() == b.Dir() {
		t.Fatal("two repositories resolved to one store directory")
	}
	r := ref("42", 1)
	if _, err := a.RecordClaim(ctx, r); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	if _, err := b.Claim(ctx, r); !errors.Is(err, ErrClaimUnrecorded) {
		t.Fatalf("the second repository read the first's pin: %v, want ErrClaimUnrecorded", err)
	}
}

func TestNewRefusesACredentialBearingRemote(t *testing.T) {
	tests := []struct {
		name, url, wantSuffix string
		wantErr               error
	}{
		{name: "https with userinfo", url: "https://user:token@github.test/acme/repo.git", wantErr: ErrRemoteCredentials},
		{name: "https with a bare username", url: "https://user@github.test/acme/repo.git", wantErr: ErrRemoteCredentials},
		{name: "https", url: "https://github.test/acme/repo.git", wantSuffix: "/github.test/acme/repo"},
		{name: "ssh scp-like", url: "git@github.test:acme/repo.git", wantSuffix: "/github.test:acme/repo"},
		{name: "a local path", url: "/srv/git/acme/repo.git", wantSuffix: "//srv/git/acme/repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := identify(tt.url)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("identify(%q) = %q, %v; want %v", tt.url, got, err, tt.wantErr)
				}
				if strings.Contains(err.Error(), "user") || strings.Contains(err.Error(), "token") {
					t.Errorf("the refusal echoes the value it refused: %v", err)
				}
				return
			}
			if err != nil || !strings.HasPrefix(got, "sha256:") || !strings.HasSuffix(got, tt.wantSuffix) {
				t.Fatalf("identify(%q) = %q, %v; want a fingerprinted identity ending in %q", tt.url, got, err, tt.wantSuffix)
			}
		})
	}
}

func TestNewRefusesOpaqueOrMalformedCredentialRemotesWithoutLeaking(t *testing.T) {
	const secret = "mirror-canary-secret"
	tests := []struct {
		name    string
		remote  string
		wantErr error
	}{
		{"unparseable https userinfo", "https://" + secret + "@github.test:notaport/acme/repo.git", ErrRemoteCredentials},
		{"transport helper", "corp::token=" + secret, ErrTransportHelperRemote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := New(Options{
				Root:       t.TempDir(),
				Repository: core.Repository{RemoteURL: tt.remote},
			})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("New() error = %v, want %v", err, tt.wantErr)
			}
			if m != nil {
				t.Fatal("New returned a mirror alongside a refusal")
			}
			for _, leak := range []string{tt.remote, secret, "github.test", "corp", "@"} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("refusal leaked %q into its message: %v", leak, err)
				}
			}
		})
	}
}

func TestRepositoryIdentityKeepsThePort(t *testing.T) {
	root := t.TempDir()
	first, err := New(Options{Root: root, Repository: core.Repository{RemoteURL: "https://github.test:8443/acme/repo.git"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := New(Options{Root: root, Repository: core.Repository{RemoteURL: "https://github.test:9443/acme/repo.git"}})
	if err != nil {
		t.Fatal(err)
	}
	if first.Repository() == second.Repository() {
		t.Fatalf("repository identities collide at %q", first.Repository())
	}
	if first.Dir() == second.Dir() {
		t.Fatalf("repository stores collide at %q", first.Dir())
	}
}

// Nothing the store writes carries the remote URL or the credential. The store
// configures no remote at all, which is what makes this a property of the design
// rather than of every error path remembering to redact.
func TestTheStoreWritesNoRemoteURLOrCredential(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	const secret = "s3cr3t-fetch-token" //nolint:gosec // a fixture value, not a credential
	m := newMirrorAt(t, o, t.TempDir(), staticAuth{password: secret})

	r := ref("42", 1)
	if _, err := m.RecordClaim(ctx, r); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	o.commit(t, Branch(r.Key))
	if _, err := m.RemoteFacts(ctx, run(r, "run-1", "v1")); err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(m.gitDir, "FETCH_HEAD")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("FETCH_HEAD exists after an explicit-URL fetch: %v", err)
	}

	err := filepath.WalkDir(m.Dir(), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(m.Dir(), path)
		if strings.Contains(string(body), secret) {
			t.Errorf("%s contains the fetch credential", rel)
		}
		if strings.Contains(string(body), o.path) {
			t.Errorf("%s contains the remote URL", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A credential failure reaches the caller with its class intact. §9.7's one
// exception to fail-closed — a transient credential failure retried in
// `verifying` — is a decision the caller can only make from the class, and a
// wrapper that flattened it would turn the retry into a parked claim.
func TestCredentialFailuresKeepTheirClass(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	for _, tc := range []struct {
		name  string
		class core.CredentialErrorClass
	}{
		{"transient", core.CredentialTransient},
		{"permanent", core.CredentialPermanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMirrorAt(t, o, t.TempDir(), staticAuth{
				err: &core.CredentialError{Class: tc.class, Authority: "octo:test", Err: errors.New("no token")},
			})
			_, err := m.RecordClaim(ctx, ref("42", 1))
			class, ok := core.CredentialFailure(err)
			if !ok || class != tc.class {
				t.Fatalf("RecordClaim error = %v, classified (%v, %v); want %v", err, class, ok, tc.class)
			}
		})
	}
}

// Credential resolution belongs inside the serialized invocation slot. If it
// happens before the lock, a short-lived credential can spend its useful life
// waiting behind another repository fetch.
func TestCredentialIsResolvedWithTheGitLockHeld(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	if err := m.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	auth := &lockInspectingAuth{lock: &m.locks.git}
	m.authSource = auth
	if _, _, err := m.lsRemote(ctx, "refs/heads/main"); err != nil {
		t.Fatalf("lsRemote: %v", err)
	}
	if !auth.held {
		t.Fatal("credential source ran before the Git mutex was acquired")
	}
}

// TestRemoteGitScopesTheCredentialToTheConfiguredRemote is #230's anchor at this
// package's boundary: what the store hands the operating system.
//
// internal/gitremote proves the shared helper refuses a foreign host. It cannot
// prove that *this* store tells the helper which host is its own, and this store
// is the copy where a silent regression would last longest: it configures no
// `origin` and names the URL per invocation, so nothing on disk would hint that
// the scope had gone missing.
func TestRemoteGitScopesTheCredentialToTheConfiguredRemote(t *testing.T) {
	ctx := context.Background()
	const (
		remote   = "https://forge.test:8443/acme/widgets.git"
		password = "mirror-scope-token" //nolint:gosec // a fixture value, not a credential
	)
	m, err := New(Options{
		Root:       t.TempDir(),
		Repository: core.Repository{RemoteURL: remote, AuthSource: staticAuth{password: password}},
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The store exists before the probe replaces git: creating it is not the
	// invocation under test, and the probe keeps only one.
	if err := m.ensure(ctx); err != nil {
		t.Fatalf("ensure: %v", err)
	}

	invocation := installGitProbe(t)
	if _, err := m.remoteGit(ctx, "ls-remote", "--", remote, "HEAD"); err != nil {
		t.Fatalf("remoteGit: %v", err)
	}
	argv, env := invocation()

	if !slices.Contains(argv, "credential.helper="+gitremote.CredentialHelper) {
		t.Errorf("git %s\n  does not install the scoped credential helper", strings.Join(argv, " "))
	}
	if !slices.Contains(argv, "credential.helper=") {
		t.Errorf("git %s\n  does not clear the inherited helper list first, so a credential helper "+
			"configured on the daemon host could answer instead", strings.Join(argv, " "))
	}
	// Derived from the configured remote, and spelled as git spells the request:
	// the port is part of the host.
	for _, want := range []struct{ key, value string }{
		{gitremote.EnvProtocol, "https"},
		{gitremote.EnvHost, "forge.test:8443"},
		{gitremote.EnvUsername, "x-access-token"},
		{gitremote.EnvPassword, password},
	} {
		if got := env[want.key]; got != want.value {
			t.Errorf("child %s = %q, want %q", want.key, got, want.value)
		}
	}
	// The credential is delivered by environment alone (SPEC §10.2).
	for _, arg := range argv {
		if strings.Contains(arg, password) {
			t.Errorf("the credential reached argv, where `ps` can read it: %q", arg)
		}
	}
}

// installGitProbe puts a `git` in front of the real one on PATH that records one
// invocation's argv and credential environment and exits without running git.
//
// Separate from installGitRecorder, which execs the real git and keeps argv
// only: this one must read the child's *environment*, and it must not run a
// remote git at all — the invocation it stands in front of names a remote that
// does not exist.
func installGitProbe(t *testing.T) func() (argv []string, env map[string]string) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "invocation")
	// One record, fields separated by US (\037), written under a single
	// redirection so a partial file cannot read as a complete invocation. The
	// variable names are the exported constants: a rename that CredentialEnv and
	// the shell agreed on but this did not leaves the reads empty and fails here.
	var script strings.Builder
	script.WriteString("#!/bin/sh\n{\nprintf 'arg=%s\\037' \"$@\"\n")
	for _, name := range []string{
		gitremote.EnvProtocol, gitremote.EnvHost, gitremote.EnvUsername, gitremote.EnvPassword,
	} {
		fmt.Fprintf(&script, "printf 'env=%s=%%s\\037' \"$%s\"\n", name, name)
	}
	fmt.Fprintf(&script, "} > %s\n", shellQuote(record))
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script.String()), 0o755); err != nil { //nolint:gosec // an executable shim is the point
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	return func() ([]string, map[string]string) {
		raw, err := os.ReadFile(record)
		if err != nil {
			t.Fatalf("the probe recorded nothing, so this test asserts nothing: %v", err)
		}
		var argv []string
		env := map[string]string{}
		for _, field := range strings.Split(strings.TrimSuffix(string(raw), "\x1f"), "\x1f") {
			switch key, value, _ := strings.Cut(field, "="); key {
			case "arg":
				argv = append(argv, value)
			case "env":
				name, v, _ := strings.Cut(value, "=")
				env[name] = v
			}
		}
		return argv, env
	}
}

// A credential source and a remote git would authenticate to in the clear is a
// combination no scoping can make safe: the configured host is the one reading
// the token off the wire (#230).
func TestNewRefusesACredentialOverACleartextRemote(t *testing.T) {
	const secret = "cleartext-canary-token" //nolint:gosec // a fixture value, not a credential
	for _, remote := range []string{
		"http://github.test/acme/repo.git",
		"HTTP://github.test/acme/repo.git",
		"ftp://github.test/acme/repo.git",
	} {
		t.Run(remote, func(t *testing.T) {
			opts := Options{
				Root:       t.TempDir(),
				Repository: core.Repository{RemoteURL: remote, AuthSource: staticAuth{password: secret}},
				Logger:     quietLogger(),
			}
			m, err := New(opts)
			if !errors.Is(err, ErrCleartextCredentialRemote) {
				t.Fatalf("New(%q) error = %v, want ErrCleartextCredentialRemote", remote, err)
			}
			if m != nil {
				t.Error("New returned a mirror alongside a refusal")
			}
			for _, leak := range []string{secret, remote} {
				if strings.Contains(err.Error(), leak) {
					t.Errorf("the refusal echoes %q: %v", leak, err)
				}
			}

			// The pairing is the refusal, not the scheme: the same remote with no
			// credential source exposes nothing, and BEN's own suites read from
			// credential-free remotes over transports git never authenticates.
			opts.Repository.AuthSource = nil
			if _, err := New(opts); err != nil {
				t.Errorf("New(%q) without a credential source = %v, want it accepted", remote, err)
			}
		})
	}
}

// A remote that cannot be reached is an error, never absence.
//
// The two are one line apart in the code and worlds apart in consequence: a
// probe that failed reads as "the branch is not there", which is a contradiction
// — and a daemon whose network is down would route every claim it holds to a
// human on the strength of not having looked (SPEC §9.10).
func TestAnUnreachableRemoteIsNotAbsence(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	r := ref("42", 1)
	if _, err := m.RecordClaim(ctx, r); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	o.commit(t, Branch(r.Key))
	if err := os.RemoveAll(o.path); err != nil {
		t.Fatal(err)
	}

	facts, err := m.RemoteFacts(ctx, run(r, "run-1", "v1"))
	if err == nil {
		t.Fatalf("RemoteFacts against an unreachable remote = %+v, want a refusal", facts)
	}
	if facts != (core.RemotePublishFacts{}) {
		t.Errorf("a refused observation returned facts %+v, want the zero value", facts)
	}
}

// The credential is scrubbed from what git says before it reaches an error or a
// log line — defense in depth behind the environment-only delivery, and asserted
// through the real git rather than over the substitution alone: what matters is
// that the value this invocation used is the value that gets scrubbed.
func TestErrorsRedactTheCredential(t *testing.T) {
	const secret = "s3cr3t-fetch-token" //nolint:gosec // a fixture value, not a credential
	o := newOrigin(t)
	m := newMirrorAt(t, o, t.TempDir(), staticAuth{password: secret})

	// git echoes the argument back in its failure, which is exactly how a
	// credential reaches an error message in production: through git's stderr.
	_, err := m.gitIn(context.Background(), o.path, secret, nil, "cat-file", "-e", secret)
	if err == nil {
		t.Fatal("the invocation succeeded; this case asserts nothing")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the error carries the credential: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Errorf("the error does not show a redaction, so git may not have echoed the value "+
			"and this case may be passing for the wrong reason: %v", err)
	}
}

func TestErrorsRedactTheRemoteURL(t *testing.T) {
	const remote = "https://github.test/acme/private-canary.git"
	o := newOrigin(t)
	m, err := New(Options{
		Root:       t.TempDir(),
		Repository: core.Repository{RemoteURL: remote},
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// cat-file echoes its argument without contacting the network. Exercising
	// both spellings covers the command argv and Git's own .git normalization.
	for _, echoed := range []string{remote, strings.TrimSuffix(remote, ".git")} {
		_, err := m.gitIn(context.Background(), o.path, "", nil, "cat-file", "-e", echoed)
		if err == nil {
			t.Fatal("the invocation succeeded; this case asserts nothing")
		}
		if strings.Contains(err.Error(), remote) || strings.Contains(err.Error(), strings.TrimSuffix(remote, ".git")) {
			t.Errorf("the error carries the remote URL: %v", err)
		}
		if !strings.Contains(err.Error(), "<remote>") {
			t.Errorf("the error does not show a remote redaction: %v", err)
		}
	}
}

func TestDiscardForgetsAClaim(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	r := ref("42", 1)
	if _, err := m.RecordClaim(ctx, r); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	if err := m.Discard(ctx, r); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if _, err := m.Claim(ctx, r); !errors.Is(err, ErrClaimUnrecorded) {
		t.Fatalf("Claim after Discard: %v, want ErrClaimUnrecorded", err)
	}
	if err := m.Discard(ctx, r); err != nil {
		t.Errorf("Discard of a forgotten claim: %v, want tolerance", err)
	}
	// A mirror whose store was never created has nothing to discard and must not
	// create one to say so.
	fresh := newMirrorAt(t, o, t.TempDir(), nil)
	if err := fresh.Discard(ctx, r); err != nil {
		t.Errorf("Discard against an absent store: %v", err)
	}
	if _, err := os.Lstat(fresh.Dir()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Discard created the store at %s", fresh.Dir())
	}
}

// A failed directory sync can leave a removed record visibly absent without
// making that absence durable. Discard retries must repeat the sync before they
// remove the pin, or a crash can restore an authoritative record without it.
func TestDiscardRetryMakesRecordRemovalDurableBeforeDeletingRefs(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	r := ref("42", 1)
	if _, err := m.RecordClaim(ctx, r); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}

	realSync := m.syncDirectory
	failClaimsSync := func(syncErr error) {
		m.syncDirectory = func(dir string) error {
			if filepath.Clean(dir) == filepath.Clean(m.claimsDir) {
				return syncErr
			}
			return realSync(dir)
		}
	}
	assertPinPresent := func(stage string) {
		t.Helper()
		if _, ok, err := m.revParse(ctx, m.pinRef(r)); err != nil || !ok {
			t.Fatalf("pin %s = present %t, error %v; want present", stage, ok, err)
		}
	}

	firstErr := errors.New("first claim-record directory sync")
	failClaimsSync(firstErr)
	if err := m.Discard(ctx, r); !errors.Is(err, firstErr) {
		t.Fatalf("Discard with failed removal sync = %v, want %v", err, firstErr)
	}
	if _, err := os.Stat(m.recordPath(r.Key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("claim record after failed removal sync: %v, want absent", err)
	}
	assertPinPresent("after failed removal sync")

	retryErr := errors.New("retry claim-record directory sync")
	failClaimsSync(retryErr)
	if err := m.Discard(ctx, r); !errors.Is(err, retryErr) {
		t.Fatalf("Discard retry with an absent record = %v, want %v", err, retryErr)
	}
	assertPinPresent("after failed retry sync")

	m.syncDirectory = realSync
	if err := m.Discard(ctx, r); err != nil {
		t.Fatalf("Discard retry: %v", err)
	}
	if _, ok, err := m.revParse(ctx, m.pinRef(r)); err != nil || ok {
		t.Fatalf("pin after durable retry = present %t, error %v; want absent", ok, err)
	}
}

// Record identity cannot stand in for store identity: absent records and stale
// cleanup epochs still delete refs, so both must refuse a tree whose repository
// file says it belongs to somebody else before they mutate it.
func TestDiscardValidatesStoreIdentityBeforeMutation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, m *Mirror, claim core.RemoteClaim) core.RemoteClaimRef
	}{
		{
			name: "absent record",
			setup: func(t *testing.T, m *Mirror, claim core.RemoteClaim) core.RemoteClaimRef {
				t.Helper()
				if err := os.Remove(m.recordPath(claim.Ref.Key)); err != nil {
					t.Fatal(err)
				}
				return claim.Ref
			},
		},
		{
			name: "mismatched epoch",
			setup: func(t *testing.T, m *Mirror, claim core.RemoteClaim) core.RemoteClaimRef {
				t.Helper()
				stale := claim.Ref
				stale.Epoch++
				runGit(t, m.gitDir, "update-ref", m.pinRef(stale), claim.BaseSHA)
				return stale
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			o := newOrigin(t)
			m := newMirror(t, o)
			claim, err := m.RecordClaim(ctx, ref("42", 1))
			if err != nil {
				t.Fatalf("RecordClaim: %v", err)
			}
			target := tc.setup(t, m, claim)
			if err := os.WriteFile(filepath.Join(m.Dir(), repositoryFileName), []byte("github.test/someone/else\n"), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := m.Discard(ctx, target); !errors.Is(err, ErrRepositoryMismatch) {
				t.Fatalf("Discard against a foreign store = %v, want ErrRepositoryMismatch", err)
			}
			if _, ok, err := m.revParse(ctx, m.pinRef(target)); err != nil || !ok {
				t.Fatalf("target pin after refused Discard = present %t, error %v; want untouched", ok, err)
			}
		})
	}
}

// A new claim cycle leaves no refs from the old one behind: the ref namespace is
// bounded by the issues in flight rather than by the repository's history.
func TestANewEpochPrunesTheOldOne(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	first, second := ref("42", 1), ref("42", 2)
	if _, err := m.RecordClaim(ctx, first); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	o.commit(t, Branch(first.Key))
	if _, err := m.RemoteFacts(ctx, run(first, "run-1", "v1")); err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if _, err := m.RecordClaim(ctx, second); err != nil {
		t.Fatalf("RecordClaim (new epoch): %v", err)
	}

	refs := runGit(t, m.gitDir, "for-each-ref", "--format=%(refname)", "refs/ben/")
	for _, name := range strings.Split(refs, "\n") {
		if strings.Contains(name, fmt.Sprintf("/%d/", first.Epoch)) {
			t.Errorf("epoch %d still has %s after epoch %d was recorded", first.Epoch, name, second.Epoch)
		}
	}
	if !strings.Contains(refs, fmt.Sprintf("/%d/", second.Epoch)) {
		t.Errorf("the current epoch's pin is missing; refs are %q", refs)
	}
}

// A replaced workspace cycle owns its verification refs until its backend
// deletion is confirmed. The later epoch is still the only authoritative claim
// record; retention preserves only the old cycle's cleanup evidence.
func TestANewEpochRetainsRefsOwnedByAnEndedCycle(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	m := newMirror(t, o)
	first, second := ref("42", 1), ref("42", 2)
	if _, err := m.RecordClaim(ctx, first); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	o.commit(t, Branch(first.Key))
	if _, err := m.RemoteFacts(ctx, run(first, "run-1", "v1")); err != nil {
		t.Fatalf("RemoteFacts: %v", err)
	}
	if _, err := m.RecordClaimRetaining(ctx, second, []core.RemoteClaimRef{first}); err != nil {
		t.Fatalf("RecordClaimRetaining: %v", err)
	}

	for _, name := range []string{m.pinRef(first), m.headRef(first), m.pinRef(second)} {
		if _, ok, err := m.revParse(ctx, name); err != nil || !ok {
			t.Fatalf("retained ref %s = present %t, err %v", name, ok, err)
		}
	}
	claim, err := m.Claim(ctx, second)
	if err != nil || claim.Ref != second {
		t.Fatalf("authoritative claim = %+v, err %v; want %+v", claim.Ref, err, second)
	}

	if err := m.Discard(ctx, first); err != nil {
		t.Fatalf("Discard(old): %v", err)
	}
	for _, name := range []string{m.pinRef(first), m.headRef(first)} {
		if _, ok, err := m.revParse(ctx, name); err != nil || ok {
			t.Fatalf("old ref %s after confirmation = present %t, err %v", name, ok, err)
		}
	}
	if _, ok, err := m.revParse(ctx, m.pinRef(second)); err != nil || !ok {
		t.Fatalf("current pin after old cleanup = present %t, err %v", ok, err)
	}
}

// An ls-remote answer naming one path twice is refused rather than resolved.
// The peeled entry beside an annotated tag is the control: a prefix match would
// read it as a second answer for the branch, which it is not.
func TestAmbiguousRefsAreRefusedAndPeeledOnesAreNot(t *testing.T) {
	const ref = "refs/heads/ben/42"
	tests := []struct {
		name string
		out  string
		want int
	}{
		{name: "one", out: "aaa\t" + ref, want: 1},
		{name: "none", out: "aaa\trefs/heads/other", want: 0},
		{name: "two for one path", out: "aaa\t" + ref + "\nbbb\t" + ref, want: 2},
		{name: "a peeled neighbour", out: "aaa\t" + ref + "\nbbb\t" + ref + "^{}", want: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refMatches(tt.out, ref); len(got) != tt.want {
				t.Fatalf("refMatches = %v, want %d matches", got, tt.want)
			}
		})
	}
}

// A branch that moves between the probe and the fetch is refused, not averaged.
// The shim moves it exactly once, in the window that exists in production and
// nowhere else, so the case is the real race rather than an approximation of it.
func TestAMovingBranchRefuses(t *testing.T) {
	ctx := context.Background()
	o := newOrigin(t)
	root := t.TempDir()
	r := ref("42", 1)

	m := newMirrorAt(t, o, root, nil)
	if _, err := m.RecordClaim(ctx, r); err != nil {
		t.Fatalf("RecordClaim: %v", err)
	}
	first := o.commit(t, Branch(r.Key))
	moved := runGit(t, o.path, "commit-tree", first+"^{tree}", "-p", first, "-m", "moved")
	installMovingFetch(t, o, Branch(r.Key), moved)

	_, err := m.RemoteFacts(ctx, run(r, "run-1", "v1"))
	if !errors.Is(err, ErrRemoteRaced) {
		t.Fatalf("RemoteFacts over a moving branch = %v, want ErrRemoteRaced", err)
	}
}

// installMovingFetch puts a `git` in front of the real one that advances a
// branch on the origin the first time a fetch runs, then execs the real git.
func installMovingFetch(t *testing.T, o *origin, branch, to string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatalf("no git on PATH: %v", err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "fired")
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" fetch "*)
    if [ ! -e %s ]; then
      : > %s
      %s --git-dir=%s update-ref refs/heads/%s %s
    fi
    ;;
esac
exec %s "$@"
`, shellQuote(marker), shellQuote(marker), shellQuote(real), shellQuote(o.path),
		branch, to, shellQuote(real))
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil { //nolint:gosec // an executable shim is the point
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// staticAuth is a fixed credential, or a fixed failure to obtain one.
type staticAuth struct {
	password string
	err      error
}

func (a staticAuth) Auth(context.Context) (core.RemoteAuth, error) {
	if a.err != nil {
		return core.RemoteAuth{}, a.err
	}
	return core.RemoteAuth{Username: "x-access-token", Password: a.password}, nil
}

type lockInspectingAuth struct {
	lock *sync.Mutex
	held bool
}

func (a *lockInspectingAuth) Auth(context.Context) (core.RemoteAuth, error) {
	if a.lock.TryLock() {
		a.lock.Unlock()
	} else {
		a.held = true
	}
	return core.RemoteAuth{Username: "x-access-token", Password: "test-token"}, nil
}
