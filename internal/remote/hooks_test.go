package remote_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// The hooks run through the backend and keep the workflow's rules: the same four
// phases, in the same order, under the same shared timeout, with the same
// containment (SPEC §5.2.6).
//
// Anchored against internal/workspace's behaviour rather than against this
// package's own table, as far as a test can be: the ordering and the containment
// are read off what the local provider does — after_create and before_run abort
// what they gate, after_run and before_remove are logged and ignored — and this
// asserts the remote path agrees. A substrate that quietly made a failing
// before_run advisory would turn a bootstrap failure into an attempt against a
// half-built workspace.

func testHooks() remote.Hooks {
	return remote.Hooks{
		AfterCreate:  "echo create",
		BeforeRun:    "echo run",
		AfterRun:     "echo after",
		BeforeRemove: "echo remove",
		Timeout:      30 * time.Second,
	}
}

func runHook(ctx context.Context, exec remote.HookExec, id remote.Identity, hooks remote.Hooks, phase remote.HookPhase) error {
	return remote.RunHook(ctx, exec, remotetest.NewMemHookStore(), remote.HookInvocation{
		Identity: id, ID: remote.HookID(phase + "-attempt-1"),
		Phase: phase, Attempt: 1, Hooks: hooks,
	})
}

func TestHookPhasesFireInTheWorkflowsOrder(t *testing.T) {
	want := []remote.HookPhase{
		remote.HookAfterCreate, remote.HookBeforeRun, remote.HookAfterRun, remote.HookBeforeRemove,
	}
	got := remote.HookPhases()
	if len(got) != len(want) {
		t.Fatalf("HookPhases = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("HookPhases = %v, want %v", got, want)
		}
	}

	// The order is the order the backend is asked in, not just the order of a
	// slice: a caller iterating HookPhases must reach StartScript in it.
	backend := remotetest.New(testProfile)
	id := testIdentity()
	for _, phase := range remote.HookPhases() {
		if err := runHook(context.Background(), backend, id, testHooks(), phase); err != nil {
			t.Fatalf("RunHook(%s): %v", phase, err)
		}
	}
	calls := backend.Hooks()
	if len(calls) != 4 {
		t.Fatalf("the backend ran %d scripts, want 4", len(calls))
	}
	wantScripts := []string{"echo create", "echo run", "echo after", "echo remove"}
	for i, call := range calls {
		if call.Script != wantScripts[i] {
			t.Errorf("script %d = %q, want %q", i, call.Script, wantScripts[i])
		}
		if call.Timeout != testHooks().Timeout {
			t.Errorf("script %d ran under timeout %s, want %s", i, call.Timeout, testHooks().Timeout)
		}
		if call.Claim != id.Claim {
			t.Errorf("script %d ran against %s, want %s", i, call.Claim, id.Claim)
		}
	}
}

// Containment per phase, stated as data and checked against both halves: which
// failures abort, and which do not.
func TestHookContainmentMatchesTheWorkflowContract(t *testing.T) {
	for _, tc := range []struct {
		phase remote.HookPhase
		want  remote.Containment
	}{
		{remote.HookAfterCreate, remote.ContainmentAbort},
		{remote.HookBeforeRun, remote.ContainmentAbort},
		{remote.HookAfterRun, remote.ContainmentIgnore},
		{remote.HookBeforeRemove, remote.ContainmentIgnore},
	} {
		t.Run(string(tc.phase), func(t *testing.T) {
			if got := remote.ContainmentOf(tc.phase); got != tc.want {
				t.Fatalf("ContainmentOf(%s) = %v, want %v", tc.phase, got, tc.want)
			}
			backend := remotetest.New(testProfile)
			backend.SetHookResult(func(string) (remote.HookResult, error) {
				return remote.HookResult{ExitCode: 3, Output: "boom"}, nil
			})
			err := runHook(context.Background(), backend, testIdentity(), testHooks(), tc.phase)
			if !errors.Is(err, remote.ErrHookFailed) {
				t.Fatalf("RunHook = %v, want it to wrap %v", err, remote.ErrHookFailed)
			}
			if got := remote.Aborts(tc.phase, err); got != (tc.want == remote.ContainmentAbort) {
				t.Errorf("Aborts(%s) = %v, want %v", tc.phase, got, tc.want == remote.ContainmentAbort)
			}
		})
	}
}

// A phase nobody classified aborts. The permissive default is the one that costs
// an attempt against a workspace whose bootstrap silently failed.
func TestAnUnclassifiedHookPhaseAborts(t *testing.T) {
	if got := remote.ContainmentOf(remote.HookPhase("after_everything")); got != remote.ContainmentAbort {
		t.Errorf("ContainmentOf(an unknown phase) = %v, want %v", got, remote.ContainmentAbort)
	}
}

// The refusals a hook run can produce, each as a named error rather than a
// message.
func TestHookRefusals(t *testing.T) {
	id := testIdentity()

	t.Run("an absent script is not a run", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		hooks := remote.Hooks{Timeout: time.Second}
		if err := runHook(context.Background(), backend, id, hooks, remote.HookBeforeRun); err != nil {
			t.Fatalf("RunHook with no script = %v, want nil", err)
		}
		if len(backend.Hooks()) != 0 {
			t.Error("an absent script still reached the backend")
		}
	})

	t.Run("a whitespace-only script is absent", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		hooks := remote.Hooks{BeforeRun: "   \n\t ", Timeout: time.Second}
		if err := runHook(context.Background(), backend, id, hooks, remote.HookBeforeRun); err != nil {
			t.Fatalf("RunHook = %v, want nil", err)
		}
		if len(backend.Hooks()) != 0 {
			t.Error("a whitespace-only script still reached the backend")
		}
	})

	t.Run("an unbounded hook is refused before it runs", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		hooks := remote.Hooks{BeforeRun: "sleep forever"}
		err := runHook(context.Background(), backend, id, hooks, remote.HookBeforeRun)
		if !errors.Is(err, remote.ErrHookFailed) {
			t.Fatalf("RunHook with no timeout = %v, want it to wrap %v", err, remote.ErrHookFailed)
		}
		if len(backend.Hooks()) != 0 {
			t.Error("an unbounded hook was dispatched anyway")
		}
	})

	t.Run("a timeout is reported as one", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		backend.SetHookResult(func(string) (remote.HookResult, error) {
			return remote.HookResult{TimedOut: true, Output: "still going"}, nil
		})
		err := runHook(context.Background(), backend, id, testHooks(), remote.HookBeforeRun)
		if !errors.Is(err, remote.ErrHookFailed) {
			t.Fatalf("RunHook = %v, want it to wrap %v", err, remote.ErrHookFailed)
		}
	})

	t.Run("a backend that could not run the script is not a script that failed", func(t *testing.T) {
		backend := remotetest.New(testProfile)
		unreachable := errors.New("backend unreachable")
		backend.SetHookResult(func(string) (remote.HookResult, error) {
			return remote.HookResult{}, unreachable
		})
		err := runHook(context.Background(), backend, id, testHooks(), remote.HookBeforeRun)
		if !errors.Is(err, remote.ErrHookFailed) {
			t.Fatalf("RunHook = %v, want it to wrap %v", err, remote.ErrHookFailed)
		}
		if !errors.Is(err, unreachable) {
			t.Errorf("RunHook = %v, want it to carry the backend's own error too: "+
				"'could not ask' and 'the script failed' are different operator problems", err)
		}
	})

	t.Run("a substrate with no hook executor refuses rather than skipping", func(t *testing.T) {
		err := runHook(context.Background(), nil, id, testHooks(), remote.HookAfterCreate)
		if !errors.Is(err, remote.ErrHookFailed) {
			t.Fatalf("RunHook with no executor = %v, want it to wrap %v", err, remote.ErrHookFailed)
		}
	})
}

// Aborts reads the phase, not the error: a nil error never aborts, and an error
// that is not a hook failure is not this rule's business.
func TestAbortsIsAboutThePhase(t *testing.T) {
	if remote.Aborts(remote.HookBeforeRun, nil) {
		t.Error("Aborts(nil) is true")
	}
	if remote.Aborts(remote.HookBeforeRun, errors.New("something else")) {
		t.Error("Aborts reported an abort for an error that is not a hook failure")
	}
}
