package remotews_test

import (
	"slices"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
)

// The trusted remote-first restore, and the hooks around it.

// The order the sandbox sees: after_create only when the sandbox was allocated,
// then BEN's restore, then before_run. The restore sits before before_run
// because an operator's script may legitimately depend on the tree, and the tree
// is not this claim's until the restore has run.
func TestHooksKeepTheirOrderWithTheRestoreBeforeTheRun(t *testing.T) {
	t.Parallel()
	r := newRig(t, withHooks(remote.Hooks{
		AfterCreate:  "echo created",
		BeforeRun:    "echo before",
		AfterRun:     "echo after",
		BeforeRemove: "echo remove",
	}))
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)

	want := []string{
		string(remote.HookAfterCreate), string(remote.HookGitPrepare), string(remote.HookBeforeRun),
	}
	got := r.hookScripts()
	if len(got) != len(want) {
		t.Fatalf("hooks ran %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hooks ran %v, want %v", got, want)
		}
	}

	// A second attempt reattaches: after_create is a workspace-creation hook and
	// must not fire again, while the restore and before_run must.
	r.mustPrepare(2, 11)
	got = r.hookScripts()
	after := got[len(want):]
	want2 := []string{string(remote.HookGitPrepare), string(remote.HookBeforeRun)}
	if len(after) != len(want2) {
		t.Fatalf("the second prepare ran %v, want %v", after, want2)
	}
	for i := range want2 {
		if after[i] != want2[i] {
			t.Fatalf("the second prepare ran %v, want %v", after, want2)
		}
	}
}

// A hook the workflow does not declare does not fire; the restore still does.
// The restore is BEN's, so it is not skippable by writing an empty workflow.
func TestTheRestoreRunsWithNoWorkflowHooksAtAll(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	if got := r.hookScripts(); len(got) != 1 || got[0] != string(remote.HookGitPrepare) {
		t.Fatalf("hooks ran %v, want only the restore", got)
	}
}

// With no branch on the canonical remote, the restore targets the claim's
// trusted base — the state a first attempt starts from.
func TestTheRestoreTargetsTheTrustedBaseWhenTheRemoteHasNoBranch(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)

	call := r.prepareCall()
	if got, want := call.Argv, []string{"/usr/local/bin/airlock-git", "prepare"}; !slices.Equal(got, want) {
		t.Fatalf("prepare argv = %v, want %v", got, want)
	}
	if call.Script != "" {
		t.Fatalf("typed prepare unexpectedly used a shell script: %q", call.Script)
	}
	wantScope := remote.GitScope{
		Phase: remote.GitPhasePrepare, Repository: gitRepository, Branch: "ben/7",
		BaseCommit: ws.BaseSHA, BaseBranch: "main", CheckoutCommit: ws.BaseSHA,
	}
	if call.Git != wantScope {
		t.Fatalf("prepare scope = %+v, want %+v", call.Git, wantScope)
	}
}

// Once the remote carries the branch, the restore targets the head the daemon
// independently observed — the same observation §9.7 verifies against — and
// proves the sandbox produced that exact commit before touching the tree.
func TestTheRestoreTargetsTheObservedRemoteHead(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	published := r.mirror.Commit(remotews.Branch(first.Key))

	r.mustPrepare(2, 11)
	call := r.prepareCall()
	if call.Git.CheckoutCommit != published {
		t.Fatalf("prepare checkout = %s, want observed head %s", call.Git.CheckoutCommit, published)
	}
	if call.Git.BaseCommit != first.BaseSHA {
		t.Fatalf("prepare base = %s, want pinned base %s", call.Git.BaseCommit, first.BaseSHA)
	}
}

// A head that does not descend the pin is a force push or somebody else's
// history. Restoring to it would seed a revision whose publication can never
// satisfy leg 1, so the prepare parks instead of spending an attempt to arrive
// at the same refusal.
func TestTheRestoreRefusesADivergedHead(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	first := r.mustPrepare(1, 11)
	r.mirror.Rewrite(remotews.Branch(first.Key))

	_, err := r.prepare(2, 11)
	wantErr(t, err, remotews.ErrRestoreDiverged)
	r.localIsUntouched()
}

// A failing restore aborts the attempt. It is BEN's script and not a workflow
// hook, so its containment is not a policy question: an attempt started over an
// unrestored tree is the whole hazard.
func TestAFailingRestoreAbortsThePrepare(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.backend.SetHookResult(func(script string) (remote.HookResult, error) {
		return remote.HookResult{ExitCode: 1, Output: "detached head"}, nil
	})
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	_, err := r.prepare(1, 11)
	wantErr(t, err, remote.ErrHookFailed)
	contains(t, err.Error(), string(remote.HookGitPrepare))
}

// The two §5.2.6 containments, unchanged by the substrate: an aborting hook
// stops the attempt and an ignored one does not.
func TestWorkflowHookContainmentIsUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		hooks remote.Hooks
		fails string
		abort bool
	}{
		{name: "after_create aborts", hooks: remote.Hooks{AfterCreate: "boom"}, fails: "boom", abort: true},
		{name: "before_run aborts", hooks: remote.Hooks{BeforeRun: "boom"}, fails: "boom", abort: true},
		{name: "after_run is ignored", hooks: remote.Hooks{AfterRun: "boom"}, fails: "boom", abort: false},
		{name: "before_remove is ignored", hooks: remote.Hooks{BeforeRemove: "boom"}, fails: "boom", abort: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, withHooks(tc.hooks))
			r.backend.SetHookResult(func(script string) (remote.HookResult, error) {
				if script == tc.fails {
					return remote.HookResult{ExitCode: 3, Output: "no"}, nil
				}
				return remote.HookResult{}, nil
			})
			if err := r.begin(11); err != nil {
				t.Fatal(err)
			}
			ws, err := r.prepare(1, 11)
			if tc.abort {
				wantErr(t, err, remote.ErrHookFailed)
				// The workspace is still reported so the claim's exit can dispose it,
				// which is §6.6's rule on the local path too.
				if ws.Path == "" {
					t.Fatal("an aborting hook left no workspace to dispose")
				}
				return
			}
			if err != nil {
				t.Fatalf("an ignored hook failure aborted the prepare: %v", err)
			}
			// The ignored ones fire outside prepare; drive them and prove neither
			// stops what it gates.
			r.provider.AfterRun(t.Context(), ws)
			if err := r.provider.Dispose(t.Context(), ws, false); err != nil {
				t.Fatalf("before_remove prevented a disposal: %v", err)
			}
		})
	}
}

// A typed prepare scope is composed only after the durable cycle record passes
// its own validation.
func TestTheRestoreRefusesAnUnnameableBranchOrCommit(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// A key that is not a single safe component cannot reach here through
	// workspace.Key, so this drives the refusal through the record instead: a
	// store handed a key with a separator refuses before anything is composed.
	if err := r.store.SaveCycle(remotews.Cycle{
		Version: remotews.CycleVersion, Issue: issueID, Key: "a/b",
		Repository: r.mirror.Repository(), Branch: "ben/a/b",
		Approval: 100, State: "pinned", Epoch: 11, BaseSHA: "abc",
	}); err == nil {
		t.Fatal("a key with a path separator was accepted")
	}
}
