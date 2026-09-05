package remote_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

type terminalBeforeQuietExec struct {
	starts   int
	statuses int
}

func (e *terminalBeforeQuietExec) StartScript(context.Context, remote.HookRef, remote.HookSpec) (remote.HookStatus, error) {
	e.starts++
	return remote.HookStatus{
		State: remote.HookStateFinished, Domain: remote.DomainStateActive,
		Result: remote.HookResult{Output: "done"},
	}, nil
}

func (e *terminalBeforeQuietExec) StatusScript(context.Context, remote.HookRef) (remote.HookStatus, error) {
	e.statuses++
	return remote.HookStatus{
		State: remote.HookStateFinished, Domain: remote.DomainStateQuiet,
		Result: remote.HookResult{Output: "done"},
	}, nil
}

func (*terminalBeforeQuietExec) WaitScript(context.Context, remote.HookRef) (remote.HookStatus, error) {
	return remote.HookStatus{}, errors.New("unexpected wait for an already-terminal hook")
}

func beforeRunInvocation(id remote.Identity) remote.HookInvocation {
	return remote.HookInvocation{
		Identity: id, ID: "before-run-attempt-1", Phase: remote.HookBeforeRun,
		Attempt: 1, Hooks: testHooks(),
	}
}

func TestHookDispatchRecoversWithoutDuplicateExecution(t *testing.T) {
	t.Run("lost successful response replays the exact hook request", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		store := remotetest.NewMemHookStore()
		in := beforeRunInvocation(testIdentity())
		backend.SetHookStartFault(errors.New("response lost"), true)

		if err := remote.RunHook(context.Background(), backend, store, in); err != nil {
			t.Fatalf("RunHook: %v", err)
		}
		if got := len(backend.Hooks()); got != 1 {
			t.Fatalf("hook executions = %d, want 1", got)
		}
		if err := remote.RunHook(context.Background(), backend, store, in); err != nil {
			t.Fatalf("replayed RunHook: %v", err)
		}
		if got := len(backend.Hooks()); got != 1 {
			t.Fatalf("hook executions after replay = %d, want 1", got)
		}
		rec, err := store.LoadHook(remote.HookKey{Claim: in.Identity.Claim, ID: in.ID})
		if err != nil {
			t.Fatalf("LoadHook: %v", err)
		}
		if !rec.Dispatched || rec.Result == nil {
			t.Fatalf("durable hook record = %+v, want dispatched result", rec)
		}
	})

	t.Run("lost response before creation replays the same hook request", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		store := remotetest.NewMemHookStore()
		in := beforeRunInvocation(testIdentity())
		boom := errors.New("request rejected")
		backend.SetHookStartFault(boom, false)

		if err := remote.RunHook(context.Background(), backend, store, in); err != nil {
			t.Fatalf("RunHook exact replay: %v", err)
		}
		rec, err := store.LoadHook(remote.HookKey{Claim: in.Identity.Claim, ID: in.ID})
		if err != nil {
			t.Fatalf("LoadHook: %v", err)
		}
		if !rec.Dispatched || rec.Result == nil {
			t.Fatalf("durable hook record = %+v, want dispatched result", rec)
		}
		if got := len(backend.Hooks()); got != 1 {
			t.Fatalf("hook executions = %d, want one effect across exact replay", got)
		}
	})

	t.Run("ambiguous replay aborts before-run and later resumes it", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		store := remotetest.NewMemHookStore()
		in := beforeRunInvocation(testIdentity())
		backend.SetHookStartFault(errors.New("response lost"), true)
		backend.SetHookStartFault(errors.New("replay unavailable"), false)

		err := remote.RunHook(context.Background(), backend, store, in)
		if !errors.Is(err, remote.ErrHookFailed) || !remote.Aborts(in.Phase, err) {
			t.Fatalf("ambiguous before_run = %v, want an aborting hook failure", err)
		}
		if got := len(backend.Hooks()); got != 1 {
			t.Fatalf("hook executions = %d, want the one possibly-live hook", got)
		}
		if err := remote.RunHook(context.Background(), backend, store, in); err != nil {
			t.Fatalf("resumed RunHook: %v", err)
		}
		if got := len(backend.Hooks()); got != 1 {
			t.Fatalf("hook executions after recovery = %d, want no duplicate", got)
		}
	})

	t.Run("lost result write recovers without rerunning", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		store := remotetest.NewMemHookStore()
		in := beforeRunInvocation(testIdentity())
		diskFull := errors.New("disk full")
		store.SetFault(func(r remote.HookRecord) error {
			if r.Result != nil {
				return diskFull
			}
			return nil
		})
		if err := remote.RunHook(context.Background(), backend, store, in); !errors.Is(err, remote.ErrHookFailed) {
			t.Fatalf("RunHook = %v, want hook failure for result persistence", err)
		}
		store.SetFault(nil)
		if err := remote.RunHook(context.Background(), backend, store, in); err != nil {
			t.Fatalf("recovered RunHook: %v", err)
		}
		if got := len(backend.Hooks()); got != 1 {
			t.Fatalf("hook executions = %d, want 1 across result-write recovery", got)
		}
	})
}

func TestHookTerminalStateKeepsReconcilingUntilTheDomainIsQuiet(t *testing.T) {
	exec := &terminalBeforeQuietExec{}
	store := remotetest.NewMemHookStore()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := remote.RunCommand(ctx, exec, store, remote.CommandInvocation{
		Identity: testIdentity(), ID: "publish-round-0", Phase: remote.HookGitPublish,
		Attempt: 1, Argv: []string{"airlock-git", "publish"}, Timeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("RunCommand: %v", err)
	}
	if result.Output != "done" || exec.starts != 1 || exec.statuses != 1 {
		t.Fatalf("result=%+v starts=%d statuses=%d, want one start and quiet reconciliation",
			result, exec.starts, exec.statuses)
	}
	record, err := store.LoadHook(remote.HookKey{Claim: testIdentity().Claim, ID: "publish-round-0"})
	if err != nil || record.Result == nil {
		t.Fatalf("quiet result was not durably completed: record=%+v err=%v", record, err)
	}
}

func TestHookIDCannotBeReusedForADifferentRequest(t *testing.T) {
	backend := remotetest.New(testProfile)
	store := remotetest.NewMemHookStore()
	in := beforeRunInvocation(testIdentity())
	if err := remote.RunHook(context.Background(), backend, store, in); err != nil {
		t.Fatalf("RunHook: %v", err)
	}
	changed := in
	changed.Hooks.BeforeRun = "echo a different script"
	if err := remote.RunHook(context.Background(), backend, store, changed); !errors.Is(err, remote.ErrHookMismatch) {
		t.Fatalf("RunHook with reused id = %v, want %v", err, remote.ErrHookMismatch)
	}
	rec, err := store.LoadHook(remote.HookKey{Claim: in.Identity.Claim, ID: in.ID})
	if err != nil {
		t.Fatalf("LoadHook: %v", err)
	}
	changedSpec := remote.HookSpec{
		Identity: in.Identity, Phase: in.Phase, Attempt: in.Attempt,
		Script: changed.Hooks.BeforeRun, Timeout: in.Hooks.Timeout,
	}
	if _, err := backend.StartScript(context.Background(), rec.Ref, changedSpec); !errors.Is(err, remote.ErrHookMismatch) {
		t.Fatalf("StartScript with same ref/different spec = %v, want %v", err, remote.ErrHookMismatch)
	}
	if got := len(backend.Hooks()); got != 1 {
		t.Fatalf("hook executions = %d, want no mismatched replay", got)
	}
}

func TestHookDirStoreRoundTripsTheDurableReferenceAndResult(t *testing.T) {
	store := remote.NewHookDirStore(t.TempDir())
	in := beforeRunInvocation(testIdentity())
	spec := remote.HookSpec{
		Identity: in.Identity, Phase: in.Phase, Attempt: in.Attempt,
		Script: in.Hooks.BeforeRun, Timeout: in.Hooks.Timeout,
	}
	digest, err := remote.HookRequestDigest(spec)
	if err != nil {
		t.Fatalf("HookRequestDigest: %v", err)
	}
	ref := remote.HookRef{
		Identity: in.Identity, ID: in.ID, Phase: in.Phase,
		Attempt: in.Attempt, RequestDigest: digest,
	}
	journal, err := remote.ReserveHook(context.Background(), store, ref)
	if err != nil {
		t.Fatalf("ReserveHook: %v", err)
	}
	if _, err := journal.Dispatch(context.Background(), func(context.Context, remote.HookRef) (remote.HookStatus, error) {
		return remote.HookStatus{State: remote.HookStateFinished}, nil
	}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	want := remote.HookResult{ExitCode: 7, Output: "tail"}
	if err := journal.Complete(context.Background(), want); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	reopened, err := remote.OpenHookJournal(store, ref.Key())
	if err != nil {
		t.Fatalf("OpenHookJournal: %v", err)
	}
	got := reopened.Record()
	if got.Ref != ref || !got.Dispatched || got.Result == nil || *got.Result != want {
		t.Fatalf("round trip = %+v, want ref, dispatch mark and %+v", got, want)
	}
}
