package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// A rebuilt provider on the same tree must share its predecessor's serialization.
//
// The defect: the base mutex and the per-issue map were *instance* fields while
// baseDir is `<root>/<workflow_key>/base.git`, so two generations of one workflow
// held different mutexes over the same base repository — serializing nothing,
// exactly while an operation begun under the previous generation was still
// completing through the adapters it captured. A reload that moves hooks or the
// root rebuilds the provider (config.AdapterChange.Workspace), so this is
// reachable from an ordinary edit.
//
// Asserted as mutual exclusion rather than as pointer equality: sharing the value
// is the implementation, and excluding each other is the property.
func TestARebuiltProviderSharesItsPredecessorsLockDomain(t *testing.T) {
	parallel(t)
	domain := NewLockDomain()
	root := t.TempDir()

	build := func(hook string) *Provider {
		p, err := providerFromOptions(t, Options{
			Root:        root,
			WorkflowKey: "wf",
			Repository:  repo("file:///nonexistent.git"),
			Hooks:       Hooks{AfterCreate: hook},
			Locks:       domain,
			Logger:      quietLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Two generations, as a hooks-only reload produces.
	first, second := build("echo one"), build("echo two")

	// TryLock rather than a timeout: "the second one did not get in" is a negative
	// claim, and a deadline only says it had not got in *yet*. This says it cannot.
	first.locks.baseMu.Lock()
	if second.locks.baseMu.TryLock() {
		second.locks.baseMu.Unlock()
		t.Error("the rebuilt provider acquired base.git while its predecessor held it: two mutexes over one repository serialize nothing")
	}
	first.locks.baseMu.Unlock()
	if !second.locks.baseMu.TryLock() {
		t.Error("base.git stayed locked after its holder released it")
	} else {
		second.locks.baseMu.Unlock()
	}

	// And the per-issue lock, which §6.4/§6.6 use to keep two Prepares off one
	// worktree.
	unlock := first.lock("issue-1")
	if second.locks.forIssue("issue-1").TryLock() {
		t.Error("the rebuilt provider took an issue lock its predecessor held")
	}
	unlock()
	if m := second.locks.forIssue("issue-1"); !m.TryLock() {
		t.Error("the issue lock stayed held after its holder released it")
	} else {
		m.Unlock()
	}
}

// A provider built without a domain gets its own, so a standalone New still
// serializes. The default must not be "no locking".
func TestAProviderWithoutADomainStillSerializes(t *testing.T) {
	parallel(t)
	p, err := providerFromOptions(t, Options{
		Root: t.TempDir(), WorkflowKey: "wf",
		Repository: repo("file:///nonexistent.git"), Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.locks == nil {
		t.Fatal("no lock domain was allocated; every operation would run unserialized")
	}

	unlock := p.lock("issue-1")
	if p.locks.forIssue("issue-1").TryLock() {
		t.Error("a second acquisition of one issue's lock succeeded while the first was held")
	}
	unlock()
	if m := p.locks.forIssue("issue-1"); !m.TryLock() {
		t.Error("the lock stayed held after unlock")
	} else {
		m.Unlock()
	}
}

// CheckBaseCache refuses a repository the cache on disk cannot serve, and accepts
// an absent one.
//
// The point is *when* it refuses. validateBase runs from ensureBase, i.e. inside
// Prepare, so adopting a moved repository against a populated cache does not fail
// the reload — it fails every subsequent attempt as a launch error, one dispatched
// issue at a time. Assembly calls this while building, so the operator gets a
// refused reload instead.
func TestCheckBaseCache(t *testing.T) {
	parallel(t)
	f := newFixture(t)
	root := t.TempDir()

	build := func(remote string) *Provider {
		p, err := providerFromOptions(t, Options{
			Root: root, WorkflowKey: "wf",
			Repository: core.Repository{RemoteURL: remote},
			Logger:     quietLogger(),
		})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}

	// Absent cache: nothing to be incompatible with.
	p := build(f.origin)
	if err := p.CheckBaseCache(context.Background()); err != nil {
		t.Fatalf("an absent base cache was refused: %v", err)
	}

	// Populate it, the way the first Prepare does.
	if _, err := prepareForTest(t, p, context.Background(), issue("1"), 1); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := p.CheckBaseCache(context.Background()); err != nil {
		t.Fatalf("the cache this provider populated was refused: %v", err)
	}

	// A different repository at the same root: refused, and named as a base-repo
	// state fault rather than as a missing file.
	other := newFixture(t)
	moved := build(other.origin)
	err := moved.CheckBaseCache(context.Background())
	if err == nil {
		t.Fatal("a cache whose origin is another repository was accepted; every later Prepare would fail instead")
	}
	if !errors.Is(err, ErrBaseRepoState) {
		t.Errorf("err = %v, want ErrBaseRepoState", err)
	}

	// And it is the same verdict Prepare would reach — one predicate, not two.
	if _, prepErr := prepareForTest(t, moved, context.Background(), issue("2"), 1); prepErr == nil {
		t.Error("Prepare accepted the mismatch CheckBaseCache refused; the two must not disagree")
	} else if !errors.Is(prepErr, ErrBaseRepoState) {
		t.Errorf("Prepare refused with %v, want the same ErrBaseRepoState", prepErr)
	}
}

// A base directory that is not a directory is a fault, not an absence: fail
// closed, never auto-repair (SPEC §6.2).
func TestCheckBaseCacheRefusesANonDirectory(t *testing.T) {
	parallel(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "wf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "wf", "base.git"), []byte("not a repo"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := providerFromOptions(t, Options{
		Root: root, WorkflowKey: "wf",
		Repository: repo("file:///nonexistent.git"), Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.CheckBaseCache(context.Background()); !errors.Is(err, ErrBaseRepoState) {
		t.Errorf("err = %v, want ErrBaseRepoState", err)
	}
}
