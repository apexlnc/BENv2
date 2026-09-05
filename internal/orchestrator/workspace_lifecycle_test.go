package orchestrator

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// lifecycleWorkspaces is the optional remote retention surface over the
// ordinary fake. Keeping it out of fake.Workspaces matters: local providers do
// not implement this seam, and making the shared fake do so would invent a
// guarantee the v1 strategy does not make.
type lifecycleWorkspaces struct {
	*fake.Workspaces
	mu    sync.Mutex
	calls []string
	// cycles are the workspaces CompleteRevocation was asked about, in order, so
	// a test can assert *which* cycle was disposed rather than only that one was.
	cycles []core.Workspace
	// revocationErr decides what CompleteRevocation returns, per workspace. A
	// refused disposal is the case the held route's ordering exists for — the
	// release must not follow it — and per-workspace rather than global because
	// "one refusal does not block unrelated held claims" is only assertable if two
	// claims can get different answers.
	revocationErr func(core.Workspace) error
	// revocationGate blocks inside CompleteRevocation, for the test that has to
	// observe the release *not* happening while the disposal is still out.
	revocationGate func()
	// entered counts calls at entry, before the gate, so a barrier can wait for
	// the disposal to be in flight rather than for it to have finished.
	entered int
	// cycleApproval is what CycleApproval answers — the approval the provider's
	// record is anchored to — and cycleApprovalErr a record it cannot read.
	// cycleApprovalGate blocks inside it, for the test that has to land a
	// reconciliation while the conversion's read is out.
	cycleApproval     int64
	cycleApprovalErr  error
	cycleApprovalGate func()
	cycleApprovals    int
	// durableCycles is the provider-local #266 obligation listing. It is kept
	// separate from cycles, which records completions, so a fixture can prove a
	// restart adopted an obligation before anything tried to complete it.
	durableCycles []core.WorkspaceRef
	durableErr    error
}

// Dispose is the published claim's disposal, and on this substrate it *retains*.
//
// That is the fidelity that matters most about this fake, and it is the premise of
// #252. The local provider removes the worktree here and stops listing it; a
// remote one keeps the sandbox and its cycle record, because `on_success: suspend`
// is what lets a reviewer resume the same tree — so §9.10 step 5 still lists the
// workspace until a delete is confirmed. Without that, every interaction between
// the step 5 sweep and a retained cycle is unreachable from a test: the inverse of
// the failure AGENTS.md names, a fake *missing* a guarantee the real component
// makes and hiding the code that depends on it.
//
// Modelled by simply not delegating — the embedded fake's `dirs` stays the one
// authoritative inventory, and the workspace is listed because it is still there.
// A second inventory here is what the first version of this had, and the two
// diverged: it hid the fake's active entries while anything was retained, and
// resurrected a deleted one when the last retained entry went.
func (w *lifecycleWorkspaces) Dispose(context.Context, core.Workspace, bool) error {
	return nil
}

func (w *lifecycleWorkspaces) CompleteFailure(context.Context, core.Workspace) error {
	w.record("failure")
	return nil
}

func (w *lifecycleWorkspaces) CompleteEndedCycle(ctx context.Context, ws core.Workspace) error {
	w.mu.Lock()
	decide, gate := w.revocationErr, w.revocationGate
	w.calls = append(w.calls, "revocation")
	w.cycles = append(w.cycles, ws)
	w.entered++
	w.mu.Unlock()
	if gate != nil {
		gate()
	}
	if decide != nil {
		if err := decide(ws); err != nil {
			return err
		}
	}
	// A confirmed delete is what finally removes it, exactly as retiring the cycle
	// record does on the real provider — and it goes through the one inventory,
	// so nothing can list a workspace this has disposed.
	if err := w.Workspaces.Dispose(ctx, ws, false); err != nil {
		return err
	}
	w.mu.Lock()
	for i, ref := range w.durableCycles {
		if ref.Path == ws.Path {
			w.durableCycles = append(w.durableCycles[:i], w.durableCycles[i+1:]...)
			break
		}
	}
	w.mu.Unlock()
	return nil
}

func (w *lifecycleWorkspaces) CompleteShutdown(context.Context, core.Workspace) error {
	w.record("shutdown")
	return nil
}

func (w *lifecycleWorkspaces) EndedCycles(context.Context) ([]core.WorkspaceRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]core.WorkspaceRef(nil), w.durableCycles...), w.durableErr
}

func (w *lifecycleWorkspaces) setDurableCycles(refs []core.WorkspaceRef, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.durableCycles = append([]core.WorkspaceRef(nil), refs...)
	w.durableErr = err
}

// CycleApproval reports which approval the provider's record is anchored to.
// Zero is the default and supersedes nothing, so every fixture that is not about
// a withdrawn approval is unaffected.
func (w *lifecycleWorkspaces) CycleApproval(context.Context, core.Issue) (int64, error) {
	w.mu.Lock()
	approval, err, gate := w.cycleApproval, w.cycleApprovalErr, w.cycleApprovalGate
	w.cycleApprovals++
	w.mu.Unlock()
	if gate != nil {
		gate()
	}
	return approval, err
}

// enteredCycleApprovals is how many reads have started, gate or no gate.
func (w *lifecycleWorkspaces) enteredCycleApprovals() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cycleApprovals
}

// setCycleApproval installs that answer, or the refusal of a record this provider
// cannot read.
func (w *lifecycleWorkspaces) setCycleApproval(approval int64, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cycleApproval, w.cycleApprovalErr = approval, err
}

// refuseRevocation installs the per-workspace verdict. Nil clears it, which is
// what a control plane coming back looks like.
func (w *lifecycleWorkspaces) refuseRevocation(decide func(core.Workspace) error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.revocationErr = decide
}

func (w *lifecycleWorkspaces) gateRevocation(gate func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.revocationGate = gate
}

// enteredRevocations is how many disposals have started, gate or no gate.
func (w *lifecycleWorkspaces) enteredRevocations() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.entered
}

func (w *lifecycleWorkspaces) record(outcome string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, outcome)
}

func (w *lifecycleWorkspaces) outcomes() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.calls...)
}

func (w *lifecycleWorkspaces) endedCycles() []core.Workspace {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]core.Workspace(nil), w.cycles...)
}

// countOutcome is how many times one outcome was applied. "Exactly once" is the
// assertion most of #252's rows are about, and a repeated sweep must not move it.
func (w *lifecycleWorkspaces) countOutcome(outcome string) int {
	n := 0
	for _, got := range w.outcomes() {
		if got == outcome {
			n++
		}
	}
	return n
}

// hookedLifecycleWorkspaces adds the §6.5 after-run hook to the retention
// surface, recorded into the same ordered list — which is what makes "the hook
// ran before the disposal" an assertion rather than a hope.
//
// A separate type rather than a method on lifecycleWorkspaces: `afterRunner` is a
// *discovered* seam, so a provider that has one is a different fixture from one
// that does not, and the tests above are about providers that do not.
type hookedLifecycleWorkspaces struct {
	*lifecycleWorkspaces
	gate func()
}

func (w *hookedLifecycleWorkspaces) AfterRun(context.Context, core.Workspace) {
	w.record("after_run")
	if w.gate != nil {
		w.gate()
	}
}

func hookedLifecycleHarness(t *testing.T, opts *harnessOpts, gate func()) (*harness, *hookedLifecycleWorkspaces) {
	t.Helper()
	var hooked *hookedLifecycleWorkspaces
	opts.wrapWorkspaces = func(workspaces *fake.Workspaces) Workspaces {
		hooked = &hookedLifecycleWorkspaces{
			lifecycleWorkspaces: &lifecycleWorkspaces{Workspaces: workspaces},
			gate:                gate,
		}
		return hooked
	}
	return start(t, *opts), hooked
}

func lifecycleHarness(t *testing.T, opts *harnessOpts) (*harness, *lifecycleWorkspaces) {
	t.Helper()
	var lifecycle *lifecycleWorkspaces
	opts.wrapWorkspaces = func(workspaces *fake.Workspaces) Workspaces {
		lifecycle = &lifecycleWorkspaces{Workspaces: workspaces}
		return lifecycle
	}
	return start(t, *opts), lifecycle
}

func TestFailedClaimUsesTheFailureWorkspacePolicy(t *testing.T) {
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureAuth) },
	})
	h.WaitGone("1")

	if got := lifecycle.outcomes(); !slices.Equal(got, []string{"failure"}) {
		t.Fatalf("workspace outcomes = %v, want [failure]", got)
	}
}

func TestTrackerRevocationUsesTheRevokedWorkspacePolicy(t *testing.T) {
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: startedOnly,
		hang:   true,
	})
	h.WaitState("1", StateRunning)
	h.Tracker.Mutate("1", func(issue *core.Issue) { issue.State = "closed" })
	// Driven with ticks rather than one: this claim has a live run, so its exit is
	// ordered after a stop that lands between ticks, and the revocation it settles
	// takes the *next* tick's turn at the bounded ended-cycle driver (#252,
	// cycle.go). One poll interval on the exit of a claim a human has already
	// closed, and the price of not starting K backend deletions at once.
	h.tickUntil("the revoked claim to be released", func() bool {
		return h.Tracker.ReleaseCount("1") == 1
	})
	h.WaitGone("1")

	if got := lifecycle.outcomes(); !slices.Equal(got, []string{"revocation"}) {
		t.Fatalf("workspace outcomes = %v, want [revocation]", got)
	}
}

// The §6.5 after-run hook runs in the workspace the disposal is about to delete,
// so the disposal has to wait for it — and once the disposal moved off the
// record's owed queue, head-of-line ordering stopped being what made it wait
// (#252, owesBeforeExit).
//
// The gate is the assertion. It holds the hook open, which holds the whole effect
// queue open, and the ended-cycle driver runs on the loop rather than that queue
// — so a tick while the hook is still executing is exactly the moment a disposal
// with no ordering rule of its own would start. On the remote substrate that
// disposal can *delete*, retiring the cycle record; remotews.AfterRun then reads
// ErrNoCycle and skips the hook without a word.
func TestEndedCycleDisposalWaitsForTheAfterRunHook(t *testing.T) {
	var once sync.Once
	running, finish := make(chan struct{}), make(chan struct{})
	h, lifecycle := hookedLifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: startedOnly,
		hang:   true,
	}, func() {
		once.Do(func() { close(running) })
		<-finish
	})
	h.WaitState("1", StateRunning)

	// A human closed the issue: the claim is revoked, and with it the workspace
	// cycle. The exit queues after_run first.
	h.Tracker.Mutate("1", func(issue *core.Issue) { issue.State = "closed" })
	h.Tick()
	select {
	case <-running:
	case <-time.After(barrierBudget):
		t.Fatal("the after_run hook never started")
	}

	// A tick with the hook still executing. Nothing may dispose here.
	h.Tick()
	if got := lifecycle.countOutcome("revocation"); got != 0 {
		t.Fatalf("%d disposals started while the after_run hook was still running", got)
	}

	close(finish)
	h.tickUntil("the revoked claim to be released", func() bool {
		return h.Tracker.ReleaseCount("1") == 1
	})
	h.WaitGone("1")
	if got := lifecycle.outcomes(); !slices.Equal(got, []string{"after_run", "revocation"}) {
		t.Fatalf("calls = %v, want the §6.5 hook and then the ended cycle's disposal", got)
	}
}

func TestShutdownUsesTheShutdownWorkspacePolicy(t *testing.T) {
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: startedOnly,
		hang:   true,
	})
	h.WaitState("1", StateRunning)
	h.shutdown()

	if got := lifecycle.outcomes(); !slices.Equal(got, []string{"shutdown"}) {
		t.Fatalf("workspace outcomes = %v, want [shutdown]", got)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Fatalf("shutdown released the tracker claim %d times", got)
	}
}
