package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// #252: a workspace cycle that outlives its claim has to be ended by something,
// and on the remote substrate the only thing that can say it has ended is the
// tracker.
//
// The shape these tests are about: `done` disposes under `on_success` — which
// remote review forces to `suspend`, because the reviewer resumes the same
// sandbox — and the claim is then *retained* while the PR awaits review. The
// sandbox survives that. What ends the cycle is the complete required-label set
// being removed after a clean review, or the issue going terminal on a merge;
// after either, a reapproval mints a different cycle address, so nothing will
// ever attach to this sandbox again.
//
// The route that must *not* end it is the changes-requested one: the controller
// unassigns BEN with every required label standing, and the next claim epoch
// reuses the same workspace cycle.

// heldCycleHarness drives n issues to done through a provider whose workspaces
// outlive their claims, leaving n held-claim records each carrying an opaque
// cycle identity.
func heldCycleHarness(t *testing.T, n int) (*harness, *lifecycleWorkspaces) {
	t.Helper()
	var issues []core.Issue
	for i := 1; i <= n; i++ {
		issues = append(issues, fake.Issue(fmt.Sprint(i), epoch.Add(time.Duration(i)*time.Minute)))
	}
	h, lifecycle := lifecycleHarness(t, &harnessOpts{concurrency: fmt.Sprint(n + 1), issues: issues})
	for i := 1; i <= n; i++ {
		h.WaitState(fmt.Sprint(i), StateDone)
	}
	waitFor(t, "the held set to fill", func() bool { return h.o.HeldCount() == n })
	return h, lifecycle
}

// The provider-local obligation is read before the first dispatch pass. This is
// the exact restart shape #266 closes: the tracker already shows a fresh,
// dispatchable approval, while only durable daemon state still names the cycle
// that approval replaced.
func TestRecoveryAdoptsADurableEndedCycleBeforeDispatch(t *testing.T) {
	old := core.WorkspaceRef{
		Identifier: "1", Key: "issue-1", Path: "remote:acme/widgets#issue-1@100",
	}
	release := make(chan struct{})
	var once sync.Once
	defer func() { once.Do(func() { close(release) }) }()

	var lifecycle *lifecycleWorkspaces
	opts := harnessOpts{issues: []core.Issue{fake.Issue("1", epoch)}, concurrency: "2"}
	opts.wrapWorkspaces = func(workspaces *fake.Workspaces) Workspaces {
		lifecycle = &lifecycleWorkspaces{
			Workspaces:     workspaces,
			durableCycles:  []core.WorkspaceRef{old},
			revocationGate: func() { <-release },
		}
		return lifecycle
	}
	h := start(t, opts)
	waitFor(t, "the recovered disposal to start", func() bool {
		return lifecycle.enteredRevocations() == 1
	})
	if got := h.Runner.StartCount(); got != 0 {
		t.Fatalf("started %d runs while the previous approval cycle was undisposed", got)
	}
	if cycles := lifecycle.endedCycles(); len(cycles) != 1 || cycles[0].Path != old.Path {
		t.Fatalf("disposed cycles = %+v, want recovered address %s", cycles, old.Path)
	}

	once.Do(func() { close(release) })
	h.tickUntil("the issue to dispatch after old-cycle confirmation", func() bool {
		return h.Runner.StartCount() == 1
	})
	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Fatalf("recovered cycle policy calls = %d, want exactly 1", got)
	}
}

// An unreadable obligation directory is not an empty one. Since the refused
// entry cannot reveal which issue it owns, dispatch is blocked globally until a
// later complete read succeeds.
func TestRefusedDurableCycleReadBlocksDispatchUntilRetried(t *testing.T) {
	refused := errors.New("cycle-disposal record is unreadable")
	claimed := fake.Issue("2", epoch.Add(time.Minute))
	claimed.Assignees = []string{fake.DefaultPrincipal}
	claimed.Dispatchable = false
	var lifecycle *lifecycleWorkspaces
	opts := harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch), claimed}, concurrency: "3", recoverErr: true,
		beforeStart: func(tracker *fake.Tracker) {
			tracker.AppendHistory("2", core.ClaimEvent{
				Kind: core.ClaimEventAssigned, Subject: fake.DefaultPrincipal, At: claimed.CreatedAt,
			})
		},
	}
	opts.wrapWorkspaces = func(workspaces *fake.Workspaces) Workspaces {
		lifecycle = &lifecycleWorkspaces{Workspaces: workspaces, durableErr: refused}
		return lifecycle
	}
	h := start(t, opts)
	for i := 0; i < 2; i++ {
		h.Tick()
	}
	if got := h.Runner.StartCount(); got != 0 {
		t.Fatalf("started %d runs while the disposal record could not be read", got)
	}
	if got := h.stateOf("2"); got != "" {
		t.Fatalf("recovered claimed issue as %q while its possible disposal owner was unreadable", got)
	}

	lifecycle.setDurableCycles(nil, nil)
	h.tickUntil("dispatch after the durable-state read recovers", func() bool {
		return h.Runner.StartCount() == 1
	})
	h.tickUntil("the held claim to be classified after the durable-state read recovers", func() bool {
		return h.stateOf("2") != ""
	})
}

// A refused listing also cannot identify which held claim owns the unreadable
// obligation. The global fail-closed gate therefore covers release, not only
// dispatch: otherwise a runtime read failure could give up the only tracker fact
// from which an obligation is recoverable.
func TestRefusedDurableCycleReadBlocksClaimReleaseUntilRetried(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)
	refused := errors.New("cycle-disposal record is unreadable")
	lifecycle.setDurableCycles(nil, refused)

	before := h.applied(sigCycleScan)
	h.Tick()
	if !awaitApplied(h, sigCycleScan, before+1) {
		t.Fatal("the refused durable-cycle read was not applied")
	}

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	h.tickUntil("the live cycle policy to complete", func() bool {
		return len(h.Workspaces.Disposals("1")) == 1
	})
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Fatalf("released the claim %d times while an obligation record was unreadable", got)
	}

	lifecycle.setDurableCycles(nil, nil)
	h.tickUntil("the claim to release after a complete durable-cycle read", func() bool {
		return h.Tracker.ReleaseCount("1") == 1
	})
}

// One issue can owe the durable cycle a downtime reapproval replaced and the
// replacement's own later revocation. Confirming either one alone must not give
// up the tracker claim that keeps the other recoverable.
func TestClaimReleaseWaitsForEveryCycleObligationForTheIssue(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)
	old := core.WorkspaceRef{
		Identifier: "1", Key: "issue-1", Path: "remote:acme/widgets#issue-1@100",
	}
	refused := errors.New("old cycle delete is still in flight")
	lifecycle.refuseRevocation(func(ws core.Workspace) error {
		if ws.Path == old.Path {
			return refused
		}
		return nil
	})
	lifecycle.setDurableCycles([]core.WorkspaceRef{old}, nil)
	h.tickUntil("the durable old cycle to be attempted", func() bool {
		for _, ws := range lifecycle.endedCycles() {
			if ws.Path == old.Path {
				return true
			}
		}
		return false
	})

	// The held replacement now ends too. Its completion succeeds, while the old
	// durable record continues to refuse.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	h.tickUntil("the replacement cycle to be deleted", func() bool {
		return len(h.Workspaces.Disposals("1")) == 1
	})
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Fatalf("released the claim %d times with the old obligation still pending", got)
	}
	refs, err := lifecycle.EndedCycles(context.Background())
	if err != nil || len(refs) != 1 || refs[0].Path != old.Path {
		t.Fatalf("durable obligations = %+v, err %v; want only %s", refs, err, old.Path)
	}

	lifecycle.refuseRevocation(nil)
	h.tickUntil("the claim to release after both obligations", func() bool {
		return h.Tracker.ReleaseCount("1") == 1
	})
}

// A full-directory read can snapshot A immediately before A's confirmation
// removes its durable record, then report afterwards. The confirmation advances
// the mutation generation, so that older successful answer cannot recreate A in
// the loop and retry an address the provider no longer owns.
func TestStaleCycleScanCannotRestoreAConfirmedObligation(t *testing.T) {
	ref := core.WorkspaceRef{
		Identifier: "1", Key: "issue-1", Path: "remote:acme/widgets#issue-1@100",
	}
	workspaces := &lifecycleWorkspaces{Workspaces: fake.NewWorkspaces()}
	o := &Orchestrator{
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		cycleMutationSeq: 1,
		endedCycles: map[string]*endedCycle{
			ref.Path: {
				issue: ref.Identifier,
				workspace: core.Workspace{
					Key: ref.Key, WorkspacePaths: core.WorkspacePaths{Path: ref.Path},
				},
				workspaces: workspaces,
			},
		},
	}
	o.onCycleDisposed(context.Background(), signal{issue: ref.Path})
	if o.cycleMutationSeq != 2 || len(o.endedCycles) != 0 {
		t.Fatalf("confirmation left generation %d and obligations %+v", o.cycleMutationSeq, o.endedCycles)
	}

	o.draining = true // onCycleScan's dispatch tail is irrelevant to this reducer check.
	o.cycleScanInFlight = true
	o.onCycleScan(context.Background(), signal{
		cycleRefs: []core.WorkspaceRef{ref}, cycleWorkspaces: workspaces, cycleScanSeq: 1,
	})
	if len(o.endedCycles) != 0 {
		t.Fatalf("stale scan restored confirmed obligations: %+v", o.endedCycles)
	}
}

// BeginClaimBase follows its mutation with a full disposal-directory read. That
// answer has the same generation hazard as the periodic scan: confirmation may
// remove an obligation while the read is out, so its older snapshot must not
// put the completed address back into the authority loop.
func TestStaleClaimBaseCycleScanCannotRestoreAConfirmedObligation(t *testing.T) {
	ref := core.WorkspaceRef{
		Identifier: "1", Key: "issue-1", Path: "remote:acme/widgets#issue-1@100",
	}
	workspaces := &lifecycleWorkspaces{Workspaces: fake.NewWorkspaces()}
	o := &Orchestrator{
		log:                    slog.New(slog.NewTextHandler(io.Discard, nil)),
		cycleMutationSeq:       1,
		cycleMutationsInFlight: 1,
		endedCycles: map[string]*endedCycle{
			ref.Path: {
				issue: ref.Identifier,
				workspace: core.Workspace{
					Key: ref.Key, WorkspacePaths: core.WorkspacePaths{Path: ref.Path},
				},
				workspaces: workspaces,
			},
		},
	}
	o.onCycleDisposed(context.Background(), signal{issue: ref.Path})
	if o.cycleMutationSeq != 2 || len(o.endedCycles) != 0 {
		t.Fatalf("confirmation left generation %d and obligations %+v", o.cycleMutationSeq, o.endedCycles)
	}

	o.handle(context.Background(), signal{
		kind: sigClaimBaseBegun, issue: ref.Identifier, cycleRead: true,
		cycleRefs: []core.WorkspaceRef{ref}, cycleWorkspaces: workspaces, cycleScanSeq: 1,
	})
	if o.cycleMutationsInFlight != 0 {
		t.Fatalf("mutation accounting = %d, want zero", o.cycleMutationsInFlight)
	}
	if len(o.endedCycles) != 0 {
		t.Fatalf("stale claim-base scan restored confirmed obligations: %+v", o.endedCycles)
	}
}

// A clean review removes the required-label set, which is the end of the
// workspace cycle. The disposal is applied exactly once and the tracker claim is
// released only after it, because the claim is what makes the obligation
// findable again after a restart.
func TestEndedWorkspaceCycleIsDisposedWhenTheApprovalLabelGoes(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	// The gate is what turns "before" from a hope into an assertion: the release
	// is observed *not* to have happened while the disposal is still out.
	release := make(chan struct{})
	lifecycle.gateRevocation(func() { <-release })

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	h.Tick()

	waitFor(t, "the ended-cycle disposal to start", func() bool { return lifecycle.enteredRevocations() == 1 })
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Fatalf("released %d times while the ended cycle's disposal was still out", got)
	}
	// And a further tick does not start a second one behind the first: the record
	// has one operation slot.
	h.Tick()
	if got := lifecycle.enteredRevocations(); got != 1 {
		t.Fatalf("%d disposals in flight for one held claim, want 1", got)
	}

	lifecycle.gateRevocation(nil)
	close(release)

	waitFor(t, "the claim to be released", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })

	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Errorf("ended-cycle disposals = %d, want exactly 1", got)
	}
	cycles := lifecycle.endedCycles()
	if len(cycles) == 0 || cycles[0].Key != "issue-1" {
		t.Errorf("disposed cycle %+v, want the claim's workspace key %q", cycles, "issue-1")
	}
	// A repeated sweep must not order a second one. Nothing owns the issue now, so
	// this is the tick after the record is gone.
	h.Tick()
	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Errorf("ended-cycle disposals = %d after a further sweep, want 1", got)
	}
}

// Merging a PR carrying `Fixes #<n>` closes the issue, which is the fallback
// terminal signal when the label removal did not settle the cycle. Same
// disposal, same ordering, off the sweep's list response alone.
func TestEndedWorkspaceCycleIsDisposedWhenTheIssueCloses(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	waitFor(t, "the claim to be released", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Fatalf("ended-cycle disposals = %d for a closed issue, want 1", got)
	}
}

// The changes-requested route: the controller posts its review and unassigns
// BEN, leaving every required label standing. The cycle is *not* over — the next
// claim epoch reuses the same sandbox — so nothing may be disposed.
func TestUnassignedHeldClaimWithLabelsStandingDisposesNothing(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	// Unassigned only. `ben-queue` is untouched, which is what the controller's
	// revocation-free route looks like: it may remove the label, and this is the
	// case where it did not.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = nil })
	h.Tick()

	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	if got := lifecycle.countOutcome("revocation"); got != 0 {
		t.Errorf("ended-cycle disposals = %d for a claim handed back inside its approval, want 0", got)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times an assignment somebody else had already taken", got)
	}
	// The label is still standing, so the issue is dispatchable again and the next
	// epoch reattaches the cycle rather than acquiring a second one. What this
	// asserts is only that BEN did not dispose it; the reattach itself is
	// remotews's (BeginClaimBase).
	if got := lifecycle.countOutcome("revocation"); got != 0 {
		t.Errorf("ended-cycle disposals = %d after the record was dropped, want 0", got)
	}
}

// The remove-and-reapply between two sweeps, which current labels cannot show.
//
// The required set is complete again by the time the sweep looks, so the issue
// reads exactly as it did — and the sandbox this claim published from has become
// unreachable, because the new application is a new approval and selects a
// different cycle address. Only the moved id in the change log says so. Cycle A
// ended at the removal and cycle B begins at the reapplication, which is the
// acceptance criterion stated directly.
func TestAWithdrawnAndReappliedApprovalEndsTheWorkspaceCycle(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	// Removed and restored between sweeps: the labels the sweep sees are complete
	// again, and only the standing approval having moved says it happened. The
	// comparison itself is the provider's — its durable record is the authority on
	// which cycle the retained sandbox belongs to — so here it simply answers, and
	// remotews has the test for the comparison.
	// The provider's record still names the cycle this claim published from, while
	// the approval standing now is a different one — which is all a withdrawal and
	// a reapplication leaves behind. The comparison is the loop's; the record is
	// the provider's, and remotews has the test for what it reports.
	lifecycle.setCycleApproval(11, nil)
	// The revision is what buys the history read the question rides on.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "requeued" })

	h.tickUntil("the ended cycle to be disposed", func() bool {
		return lifecycle.countOutcome("revocation") == 1
	})
	if cycles := lifecycle.endedCycles(); len(cycles) != 1 || cycles[0].Key != "issue-1" {
		t.Fatalf("disposed %+v, want exactly the cycle the withdrawn approval anchored", cycles)
	}
	// The claim goes with it: the reapproved issue is new work under cycle B, not a
	// continuation of the one whose sandbox has just been released.
	waitFor(t, "the claim to be released", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
}

// The window between `done` and the held record, which nothing else covers.
//
// Reconciliation adopts the issue's *current* revision on the way to `done`, and
// the conversion baselines the held record to it — so a withdrawal and
// re-application that happened before this instant leaves no later revision
// mismatch, the sweep never buys a history read, and nothing ever asks. The
// conversion has to ask for itself, from the change log it already reads.
//
// Started from `running` rather than from a held claim, deliberately: a fixture
// that begins already held cannot reach this window at all, which is why the
// earlier test could not see it.
func TestAnApprovalWithdrawnBeforeConversionEndsTheWorkspaceCycle(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			// `done`'s third label write, held open so the withdrawal is in place
			// before the conversion's history read goes out.
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	// The provider's record still names the cycle this attempt ran in, while the
	// approval standing now is a different one.
	lifecycle.setCycleApproval(11, nil)
	// The disposal is held open too, which is what gives the withdrawal below a
	// place to stand: after the conversion has asked its question, before the
	// claim it is holding back is released.
	resume := make(chan struct{})
	lifecycle.gateRevocation(func() { <-resume })
	close(gate)

	// Ticked: the release is held behind the disposal, and the disposal takes the
	// tick's turn at the bounded driver.
	h.tickUntil("the ended cycle to be disposed", func() bool {
		return lifecycle.enteredRevocations() == 1
	})

	// Withdrawing the label here is not part of the subject — the conversion has
	// already asked and answered from the change log, which is the whole of what
	// this test is about. It is what makes `not tracked` *terminal*, and #276 is
	// why that has to be arranged rather than assumed.
	//
	// The issue still carries `ben-queue` and, once the claim is released, is
	// unassigned and unlabelled — dispatchable again. The CI failure was exactly
	// that: WaitGone reported the path `queued claimed preparing running verifying
	// done claimed preparing running verifying done`, a second complete cycle that
	// had legitimately been dispatched. No barrier can fix it, because cycle B is
	// superseded by the same stale provider record and would be disposed and
	// released in its turn — so `endedCycles`, `HeldCount` and `ReleaseCount`
	// below are not stable quantities at all while the door is open. Closing it is
	// the fix; the assertions then describe one cycle because there is one.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })

	lifecycle.gateRevocation(nil)
	close(resume)

	h.WaitGone("1")
	if cycles := lifecycle.endedCycles(); len(cycles) != 1 || cycles[0].Key != "issue-1" {
		t.Fatalf("disposed %+v, want the cycle the withdrawn approval anchored", cycles)
	}
	// It never became a held claim: the cycle it would have been retained for is
	// the one that just ended.
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; a claim was retained for a cycle that had ended", got)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 1 {
		t.Errorf("released %d times, want 1", got)
	}
	// And the door stays shut: a sweep after the record is gone finds nothing to
	// claim, which is what "terminal" means here.
	h.Tick()
	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Errorf("ended-cycle disposals = %d after a further sweep, want 1", got)
	}
	if got := h.Runner.StartCount(); got != 1 {
		t.Errorf("started %d run(s); the released issue was dispatched again", got)
	}
}

// A cycle question that cannot be answered is not an answer, in either of its two
// shapes — and the consequence is the same: retain, do not re-baseline, ask again.
//
// The second shape is the one that reads as harmless and is not. A standing
// approval of zero means the change log does not show the required set applied,
// for a claim whose *list* read says it is: the two disagree and the log is
// behind. Treating that as "unchanged" settles it forever, because the caller
// spends the revision bump that would have bought the next read.
func TestAnUnansweredCycleQuestionRetainsTheClaim(t *testing.T) {
	for _, tc := range []struct {
		name  string
		arm   func(*harness, *lifecycleWorkspaces)
		match string
	}{
		{
			name: "the provider cannot read its record",
			arm: func(h *harness, lifecycle *lifecycleWorkspaces) {
				lifecycle.setCycleApproval(0, errors.New("state dir unreadable"))
			},
			match: "state dir unreadable",
		},
		{
			name: "the change log does not show the approval",
			arm: func(h *harness, lifecycle *lifecycleWorkspaces) {
				// The label is applied as far as the list is concerned, and absent from
				// the log — which is what a lagging change log looks like from here.
				lifecycle.setCycleApproval(11, nil)
				h.Tracker.AppendHistory("1", core.ClaimEvent{
					Kind: core.ClaimEventUnlabeled, Subject: "ben-queue", At: epoch,
				})
			},
			match: "does not show the required-label set applied",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, lifecycle := heldCycleHarness(t, 1)
			tc.arm(h, lifecycle)
			h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "moved" })

			h.tickUntil("the question to be asked and refused", func() bool {
				return len(h.Logs.find("could not read the workspace cycle's approval")) > 0
			})
			if got := lifecycle.countOutcome("revocation"); got != 0 {
				t.Fatalf("disposed %d cycles on a question nobody answered", got)
			}
			if got := h.o.HeldCount(); got != 1 {
				t.Fatalf("held count = %d; the claim was dropped on an unanswered question", got)
			}

			// And the revision was not re-baselined, so the next sweep asks again
			// rather than recording an answer nobody gave.
			before := h.Tracker.HistoryReads()
			h.Tick()
			if got := h.Tracker.HistoryReads(); got <= before {
				t.Errorf("the revision was re-baselined on an unanswered question; nothing will ask again")
			}
		})
	}
}

// A label genuinely removed before the conversion, which is the *other* thing a
// zero standing approval can mean.
//
// On the sweep path a list response has just said the labels are complete, so a
// log naming no approval is behind. At conversion nothing has read them, and the
// likelier cause is the opposite — the set really was withdrawn. Reading both as
// "ask again" retried an issue that had left the workflow for the life of the
// process, which is a hang rather than a leak.
//
// So the conversion settles it against current state, by the same `Get` a missing
// claim-cycle anchor is settled by.
func TestALabelRemovedBeforeConversionIsSettledAgainstCurrentState(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	// The withdrawal, in the log *and* in current state — which is the fixture the
	// earlier test omitted, and the reason it could not see this.
	h.Tracker.AppendHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventUnlabeled, Subject: "ben-queue", At: epoch,
	})
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	close(gate)

	h.tickUntil("the ended cycle to be disposed", func() bool {
		return lifecycle.countOutcome("revocation") == 1
	})
	h.WaitGone("1")
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; a claim was retained for a cycle whose approval is gone", got)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 1 {
		t.Errorf("released %d times, want 1", got)
	}
}

// The conversion's answers are scoped to the revision they were computed against.
//
// The window is narrow and nothing reaches it by accident, which is why an
// earlier version of this test asserted nothing. Reconciliation excludes `done`
// from new passes, so a tick *after* the conversion begins never refreshes the
// record — the refresh has to be one that started while the record was still
// `verifying` and lands after it is not. That stale pass adopts a newer issue onto
// the record while the conversion worker is blocked on the provider, and
// baselining the held record to *that* revision would claim the cycle question had
// been asked about a change nobody looked at. The trigger that buys the next
// history read is spent exactly once.
func TestTheConversionBaselinesToTheRevisionItAsked(t *testing.T) {
	verifyGate, getGate := make(chan struct{}), make(chan struct{})
	approvalGate := make(chan struct{})
	var lifecycle *lifecycleWorkspaces
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			<-verifyGate
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
		beforeStart: func(tr *fake.Tracker) { tr.SetGetGate(func() { <-getGate }) },
		wrapWorkspaces: func(workspaces *fake.Workspaces) Workspaces {
			lifecycle = &lifecycleWorkspaces{
				Workspaces:        workspaces,
				cycleApprovalGate: func() { <-approvalGate },
			}
			return lifecycle
		},
	})
	h.WaitState("1", StateVerifying)

	// A reconciliation snapshots the issue while it is `verifying` — the last state
	// it is eligible in — and its read stalls.
	h.Tick()
	waitFor(t, "the tick's refresh read", func() bool { return h.Tracker.GetReads() > 0 })
	reconciled := h.applied(sigReconciled)

	// The run finishes and the conversion begins, then blocks on the provider.
	close(verifyGate)
	h.WaitState("1", StateDone)
	waitFor(t, "the conversion to reach the provider", func() bool {
		return lifecycle.enteredCycleApprovals() >= 1
	})

	// The revision moves, and the stalled pass lands: it adopts the new issue onto
	// a record whose conversion asked about the old one.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "moved-while-converting" })
	close(getGate)
	waitFor(t, "the stale reconciliation to land", func() bool {
		return h.applied(sigReconciled) > reconciled
	})

	// Only now does the conversion complete.
	close(approvalGate)
	waitFor(t, "the held record", func() bool { return h.o.HeldCount() == 1 })

	// The moved revision must still buy its history read: nothing has asked the
	// cycle question about it.
	before := h.Tracker.HistoryReads()
	h.Tick()
	waitFor(t, "the moved revision to buy a history read", func() bool {
		return h.Tracker.HistoryReads() > before
	})
}

// The discriminator: a revision bump with the approval standing still buys one
// history read and changes nothing. Without it the rule above would end a cycle on
// every comment.
func TestARevisionBumpWithTheApprovalStandingEndsNothing(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Revision = "someone-commented" })

	for range 3 {
		h.Tick()
	}
	if got := lifecycle.countOutcome("revocation"); got != 0 {
		t.Fatalf("ended-cycle disposals = %d for a comment, want 0", got)
	}
	if got := h.o.HeldCount(); got != 1 {
		t.Fatalf("held count = %d; the claim was released on a comment", got)
	}
}

// The confirming read answers two questions, and they are not the same one.
//
// A held claim absent from the assignee-filtered sweep is confirmed with one Get.
// That Get's job is "is there still a claim of ours here" — and an issue that is
// *both* unassigned and closed answers that first, dropping the record on a
// disappearance while the facts sitting in the same response say the workspace
// cycle is over. The cycle question has to be asked first (#252, cycleEndedBy).
func TestHeldClaimThatDisappearsWithItsCycleEndedStillDisposes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		end     func(*core.Issue)
		dispose bool
	}{
		{
			name: "unassigned and closed",
			end: func(i *core.Issue) {
				i.Assignees = nil
				i.State = "closed"
			},
			dispose: true,
		},
		{
			name: "unassigned and stripped of its labels",
			end: func(i *core.Issue) {
				i.Assignees = nil
				i.Labels = nil
			},
			dispose: true,
		},
		{
			// The changes-requested route, and the discriminator for the two above:
			// the approval still stands, so the next claim epoch resumes this exact
			// sandbox and nothing may be disposed.
			name:    "unassigned with every required label standing",
			end:     func(i *core.Issue) { i.Assignees = nil },
			dispose: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, lifecycle := heldCycleHarness(t, 1)
			h.Tracker.Mutate("1", tc.end)

			h.tickUntil("the held record to drop", func() bool { return h.o.HeldCount() == 0 })
			if tc.dispose {
				h.tickUntil("the ended cycle to be disposed", func() bool {
					return lifecycle.countOutcome("revocation") == 1
				})
				if cycles := lifecycle.endedCycles(); len(cycles) != 1 || cycles[0].Key != "issue-1" {
					t.Fatalf("disposed %+v, want the held claim's own workspace cycle", cycles)
				}
			} else {
				h.Tick()
				h.Tick()
				if got := lifecycle.countOutcome("revocation"); got != 0 {
					t.Fatalf("ended-cycle disposals = %d for a claim handed back inside its approval, want 0", got)
				}
			}
			// Either way the claim itself is somebody else's now: there was nothing
			// of ours left to release.
			if got := h.Tracker.ReleaseCount("1"); got != 0 {
				t.Errorf("released %d times an assignment somebody else had already taken", got)
			}
		})
	}
}

// An issue deleted or transferred while its claim was held. Nothing on this
// tracker will address that cycle again, and the held record is the last thing
// that knows which sandbox it was — so the obligation is registered before the
// record goes, and then outlives every owner. That is the case `drained` keeps
// its own clause for.
func TestHeldClaimGoneFromTheTrackerStillDisposesItsEndedCycle(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	wedged := make(chan struct{})
	var once sync.Once
	unwedge := func() { once.Do(func() { close(wedged) }) }
	defer unwedge()
	lifecycle.gateRevocation(func() { <-wedged })

	h.Tracker.Delete("1")
	h.tickUntil("the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	h.tickUntil("the ended cycle's disposal to start", func() bool {
		return lifecycle.enteredRevocations() == 1
	})

	// No record, no held claim: the obligation is the only thing left, and the
	// drain has to be what waits for it.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.o.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	select {
	case <-stopped:
		t.Fatal("the drain completed with an ownerless ended-cycle disposal still out")
	case <-time.After(100 * time.Millisecond):
	}

	unwedge()
	select {
	case <-stopped:
	case <-time.After(barrierBudget):
		t.Fatal("the drain did not complete after the disposal landed")
	}
	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Errorf("ended-cycle disposals = %d, want 1", got)
	}
}

// The same disappearance one step earlier. The claim-cycle anchor read names the
// issue and nothing else, so its not-found *is* the issue — the run record is
// dropped straight from `done` without ever becoming a held claim, and it is the
// last thing that knows which sandbox the published attempt ran in.
func TestPublishedClaimWhoseIssueVanishesStillDisposesItsEndedCycle(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			// `done`'s third label write, held open so the change log is refusing
			// before the anchor read that follows it goes out.
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	h.Tracker.SetFailHistory(core.ErrIssueNotFound)
	close(gate)

	h.WaitGone("1")
	h.tickUntil("the ended cycle to be disposed", func() bool {
		return lifecycle.countOutcome("revocation") == 1
	})
	if cycles := lifecycle.endedCycles(); len(cycles) != 1 || cycles[0].Key != "issue-1" {
		t.Fatalf("disposed %+v, want the published claim's own workspace cycle", cycles)
	}
	if got := h.o.HeldCount(); got != 0 {
		t.Errorf("held count = %d; a claim was retained for an issue that is gone", got)
	}
}

// The one route the in-memory obligation cannot survive a crash on, and what
// re-derives it.
//
// Every other route leaves the tracker claim standing, so §9.10 step 1 enumerates
// it at the next start and the verdict is reached again. This one does not: the
// same read that ended the cycle also said the assignment is gone, so the issue is
// not a candidate for any recovery read — and it is still *open*, which is exactly
// what step 5's sweep used to treat as "leave it alone". A crash there stranded the
// sandbox permanently.
//
// So the sweep re-derives it from the tracker: on a provider whose workspaces
// outlive their claims, an open issue that has left the label partition is an
// ended cycle. Nothing new is written down, which is §9.10's own posture.
func TestARestartRediscoversAnUnassignedRevokedCycle(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	// Refused rather than gated: the pre-restart attempts have to fail fast, so
	// nothing is parked in the dead process's goroutine when it goes.
	lifecycle.refuseRevocation(func(core.Workspace) error {
		return errors.New("airlock: control plane unreachable")
	})

	// The clean review's own gesture plus the controller's unassignment, arriving
	// in one read: the cycle ends and its last owner goes at the same instant.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Labels = nil
		i.Assignees = nil
	})
	h.tickUntil("the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	h.tickUntil("the disposal to be attempted and refused", func() bool {
		return lifecycle.enteredRevocations() >= 1
	})

	// The crash. Nothing in memory survives, and the issue is assigned to nobody:
	// there is no claim for recovery to enumerate.
	before := lifecycle.enteredRevocations()
	lifecycle.refuseRevocation(nil)
	if err := h.restart(harnessOpts{
		concurrency: "2", workspaces: lifecycle,
		runGone: func(core.RunEvidence) (bool, error) { return true, nil },
	}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := h.o.HeldCount(); got != 0 {
		t.Fatalf("held count = %d after a restart; the issue is assigned to nobody", got)
	}

	h.tickUntil("the new process to dispose the rediscovered cycle", func() bool {
		return lifecycle.enteredRevocations() > before
	})
	cycles := lifecycle.endedCycles()
	if last := cycles[len(cycles)-1]; last.Key != "issue-1" {
		t.Fatalf("disposed %+v, want the stranded workspace cycle", last)
	}
	// And it is gone from the provider's inventory, so a further pass finds nothing.
	waitFor(t, "the workspace to leave the inventory", func() bool {
		refs, err := lifecycle.ListWorkspaces(context.Background())
		return err == nil && len(refs) == 0
	})
}

// The same predicate must leave a *local* provider's workspace alone, and the
// case where that is reachable is §6.4's kept failure.
//
// A failed attempt's worktree is retained on purpose, so somebody can look at it —
// and §9.10 gate 4 *keeps* a local workspace when its issue leaves the label
// partition rather than disposing it. So the sweep examining a kept worktree whose
// open issue has been unlabelled must decide "leave it", which is the opposite of
// what it decides for a provider whose cycles outlive their claims. Without the
// provider half of the predicate, this is a human's forensic evidence deleted.
func TestTheSweepKeepsAKeptLocalWorkspaceWhoseIssueLeftThePartition(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureAuth) },
	})
	h.WaitGone("1")
	// §6.4 keeps it by not disposing it at all, so the inventory is where the
	// retention shows.
	if refs, err := h.Workspaces.ListWorkspaces(context.Background()); err != nil || len(refs) != 1 {
		t.Fatalf("the failed attempt's worktree = %+v (err %v), want the one §6.4 retains", refs, err)
	}

	// The issue leaves the workflow while still open — the same tracker fact that
	// ends a workspace cycle on the other substrate.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Labels = nil
		i.Assignees = nil
	})

	// A restart is what runs a full §9.10 step 5 pass with the kept worktree
	// already on disk, so the pass actually examines it.
	if err := h.restart(harnessOpts{concurrency: "2"}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	for range 3 {
		h.Tick()
	}
	refs, err := h.Workspaces.ListWorkspaces(context.Background())
	if err != nil || len(refs) != 1 {
		t.Fatalf("the kept worktree is %+v (err %v); the sweep removed a human's forensic evidence for an "+
			"open issue out of the label partition", refs, err)
	}
}

// A recovery candidate the obligation holds back must not be *forgotten* — and
// the thing that comes back for it must not be the scan itself.
//
// Two halves, and the second is why this is not simply `scanOwed = true`. Nothing
// else ever looks at a held-back candidate: §8.3 excludes an assigned issue from
// the ordinary Fetch, so an assigned claim a pass declined is invisible until a
// restart. But `scanOwed` means "the pass never spoke", which gates §9.10 step 5's
// cleanup for every workspace — and the scan handler re-enters the scan directly
// on its own tail, so owing it there spins `ClaimedByPrincipal` without even
// waiting for a tick. So the obligation is marked, and the disposal's confirmation
// is what owes the read.
//
// Driven through the handler directly, as the adoption tests are: this is the
// authority goroutine's step, and running it here is what running it there means.
// The alternative fixture — a candidate scan that fails at startup and keeps
// failing until a held claim's cycle has ended ownerlessly — stages five unrelated
// things to observe one.
func TestARecoveryCandidateHeldBackByAnObligationReowesTheScanOnConfirmation(t *testing.T) {
	tracker := fake.NewTracker(fake.Issue("1", epoch))
	o, src := idleWithSource(t, tracker)
	cur, _ := src.Load()
	ctx := context.Background()

	o.endedCycles = map[string]*endedCycle{
		"remote:acme#issue-1@7": {
			issue:     "1",
			workspace: core.Workspace{Key: "issue-1", WorkspacePaths: core.WorkspacePaths{Path: "remote:acme#issue-1@7"}},
		},
	}
	o.scanOwed = true
	o.scanInFlight = true

	scan := func() {
		o.scanInFlight = true
		o.onRecoveryScan(ctx, signal{
			kind: sigRecoveryScan, revision: cur.Revision, candidates: []core.Issue{fake.Issue("1", epoch)},
		})
	}
	o.onRecoveryScan(ctx, signal{
		kind: sigRecoveryScan, revision: cur.Revision, candidates: []core.Issue{fake.Issue("1", epoch)},
	})

	if _, adopted := o.records["1"]; adopted {
		t.Error("a candidate whose previous cycle's disposal is still owed was adopted")
	}
	if o.scanOwed {
		t.Fatal("the deferring scan owed itself again; the handler's own tail re-enters it, so this spins")
	}
	if !o.endedCycles["remote:acme#issue-1@7"].deferredCandidate {
		t.Fatal("the obligation does not record that it held a candidate back; nothing will ask again")
	}
	// A further scan changes nothing and still does not owe itself.
	scan()
	if o.scanOwed {
		t.Fatal("a repeated deferring scan owed itself")
	}

	// The disposal confirms. *That* is what owes the read, once.
	o.clearEndedCycle(ctx, "remote:acme#issue-1@7")
	if !o.scanOwed {
		t.Fatal("the confirmed disposal did not owe the read its deferral made necessary")
	}

	scan()
	if _, adopted := o.records["1"]; !adopted {
		t.Fatal("the candidate was not adopted once its previous cycle was disposed")
	}
	if o.scanOwed {
		t.Error("the scan is still owed with nothing left to defer")
	}
}

// The §9.10 step 5 sweep is the other thing that disposes a workspace, and it
// reaches for the same one from the other side.
//
// Its handback is the route: a pass that skipped this workspace because a record
// owned its issue takes it back the moment nothing does — which is exactly what an
// ownerless obligation looks like to a set built from records and held claims. The
// issue is terminal, so the pass would dispose it, concurrently with the disposal
// the obligation already has in flight. Two completions over one cycle.
func TestTheWorkspaceSweepDoesNotRaceAnOwnerlessObligation(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	// A full §9.10 pass with the workspace already on disk and its issue owned, so
	// the ref lands in the skipped set the handback is driven from. A restart is
	// what runs one.
	if err := h.restart(harnessOpts{
		concurrency: "2", workspaces: lifecycle,
		runGone: func(core.RunEvidence) (bool, error) { return true, nil },
	}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	waitFor(t, "the claim to be retained again", func() bool { return h.o.HeldCount() == 1 })

	wedged := make(chan struct{})
	var once sync.Once
	unwedge := func() { once.Do(func() { close(wedged) }) }
	defer unwedge()
	lifecycle.gateRevocation(func() { <-wedged })

	// Closed *and* unassigned: the cycle ended and the claim disappeared in one
	// read, so the obligation outlives the record — and the issue is terminal,
	// which is the sweep's own trigger.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.State = "closed"
		i.Assignees = nil
	})
	h.tickUntil("the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	h.tickUntil("the ended cycle's disposal to start", func() bool {
		return lifecycle.enteredRevocations() == 1
	})

	// Ticks are what drive the handback and the pass it feeds. None of them may
	// add a second completion over a cycle already being disposed.
	reads := h.Tracker.GetReadsFor("1")
	for range 4 {
		h.Tick()
	}
	if got := lifecycle.enteredRevocations(); got != 1 {
		t.Fatalf("%d completions over one workspace cycle; the sweep raced the obligation", got)
	}
	// And the *cost* gate held, independently of the reservation. A pass examines a
	// ref with one tracker `Get` and only then reserves, so a read here would mean
	// the ref reached the I/O and was stopped at the reservation — which is the
	// other guard doing this one's job, at the price of a request per tick per
	// outstanding obligation (ownedIssues, grantDisposal).
	if got := h.Tracker.GetReadsFor("1"); got != reads {
		t.Errorf("the sweep spent %d tracker reads on a workspace its ownership set should have filtered",
			got-reads)
	}
	unwedge()
}

// The confirming read is spent for two disjoint reasons, and only one of them may
// derive an obligation.
//
// The second reason is a settled release the tracker keeps refusing — and the
// release is only ever *attempted* once the cycle's disposal has been confirmed
// (driveHeldExit). So the facts that read comes back with describe a cycle that is
// already disposed, and deriving from them again would re-call the backend for a
// completion that has landed. Under `on_revoked: suspend` the cycle record
// survives the first disposal, so the second call is real — and it re-gates the
// release that was about to land, which wedges it if the backend has since gone.
func TestAFailedReleasesConfirmationDoesNotRederiveTheDisposal(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	// The release is refused, so the record earns a confirming Get. The issue is
	// closed, which is exactly the fact that would re-derive a cycle end.
	h.Tracker.SetFailRelease(errors.New("503 from the tracker"))
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })

	h.tickUntil("the ended cycle to be disposed", func() bool {
		return lifecycle.countOutcome("revocation") == 1
	})
	h.tickUntil("the refused release to earn its confirmation", func() bool {
		return h.Tracker.GetReads() > 0
	})
	// Several more ticks: each one re-drives the release, each failure earns
	// another confirmation, and none of them may buy a second disposal.
	for range 3 {
		h.Tick()
	}
	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Fatalf("ended-cycle disposals = %d; a failed release's confirmation re-derived one", got)
	}

	// And the release still lands once the tracker takes it: nothing re-gated it.
	h.Tracker.SetFailRelease(nil)
	h.tickUntil("the claim to be released", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Errorf("ended-cycle disposals = %d in total, want exactly 1", got)
	}
}

// An obligation with no owner is still an owner, and this is why.
//
// The sequence: a held claim's cycle ends and its claim disappears in the same
// read, so the obligation is registered and the record dropped. A human then
// reapproves the issue. Nothing owns it any more, so dispatch would take it and
// start another tenure while cleanup still owns the issue. The provider now
// addresses old and replacement cycles independently; the orchestrator's gate is
// the complementary claim that an unresolved disposal still owns this issue.
func TestAnOwnerlessObligationHoldsTheIssueOutOfDispatch(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 1)

	wedged := make(chan struct{})
	var once sync.Once
	unwedge := func() { once.Do(func() { close(wedged) }) }
	defer unwedge()
	lifecycle.gateRevocation(func() { <-wedged })

	// Unassigned *and* unlabelled: the cycle ended and the claim disappeared in
	// one gesture, which is the read that leaves the obligation ownerless.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Assignees = nil
		i.Labels = nil
	})
	h.tickUntil("the held record to drop", func() bool { return h.o.HeldCount() == 0 })
	h.tickUntil("the ended cycle's disposal to start", func() bool {
		return lifecycle.enteredRevocations() == 1
	})

	// Reapproved, and dispatchable as far as the tracker is concerned.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Labels = []string{"ben-queue"}
		i.Dispatchable = true
	})
	h.Tick()
	h.Tick()
	if got := h.Runner.StartCount(); got != 1 {
		t.Fatalf("started %d runs; a new cycle was dispatched while the previous one's disposal was owed", got)
	}
	if got := h.stateOf("1"); got != "" {
		t.Fatalf("the issue is tracked as %q while its previous cycle's disposal is owed", got)
	}

	// The backend confirms, and only then is it dispatchable again.
	lifecycle.gateRevocation(nil)
	unwedge()
	h.tickUntil("the reapproved issue to be dispatched", func() bool { return h.Runner.StartCount() == 2 })
}

// A refused disposal is retried and never assumed: the claim stays retained, its
// release stays unsent, and the obligation is not forgotten. What it must not do
// is hold up an unrelated held claim, which is why the refusal is per workspace.
func TestRefusedEndedCycleDisposalHoldsItsOwnReleaseOnly(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 2)

	stuck := "issue-1"
	refused := errors.New("airlock: control plane unreachable")
	lifecycle.refuseRevocation(func(ws core.Workspace) error {
		if ws.Key == stuck {
			return refused
		}
		return nil
	})

	// Both cycles end in the same gesture — a human clearing the queue label
	// across a review backlog, which is the case that produces several at once.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	h.Tracker.Mutate("2", func(i *core.Issue) { i.Labels = nil })

	// The one whose backend answers finishes. Driven with tickUntil rather than a
	// counted tick because the two share one per-tick turn (endedCyclesInFlight),
	// so which of them goes first is the rotation's business and not this test's.
	h.tickUntil("the reachable cycle is released", func() bool { return h.Tracker.ReleaseCount("2") == 1 })
	waitFor(t, "the reachable claim to drop", func() bool { return h.o.HeldCount() == 1 })
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Fatalf("released %d times a claim whose ended cycle is not disposed", got)
	}

	// Retried, on later ticks, for as long as it keeps failing.
	before := lifecycle.enteredRevocations()
	h.tickUntil("the refused disposal is retried", func() bool {
		return lifecycle.enteredRevocations() > before
	})
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Fatalf("released %d times after a retried disposal also failed", got)
	}
	if got := h.o.HeldCount(); got != 1 {
		t.Fatalf("held count = %d; the claim carrying an unconfirmed obligation was dropped", got)
	}

	// The control plane comes back: the same disposal lands and the release
	// follows it.
	lifecycle.refuseRevocation(nil)
	h.tickUntil("the recovered cycle is released", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the last held record to drop", func() bool { return h.o.HeldCount() == 0 })
}

// One gesture ends every cycle in a review backlog at once, which is the case the
// bound exists for: K ended cycles must not become K concurrent backend calls on
// the tick that notices them, nor K again on every tick after that.
//
// The bound is on the **rate**, and the two halves of that are asserted together
// because getting one right at the other's expense is the easy mistake. One start
// per tick — and a disposal that never returns does not hold the turn, so the
// cycles behind it are not blocked by a claim they have nothing to do with.
func TestEndedCyclesAreDisposedOnePerTickInRotation(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 3)

	// Every disposal blocks, so nothing completes and nothing leaves the
	// obligation set: what the ticks below measure is purely how many are started.
	held := make(chan struct{})
	lifecycle.gateRevocation(func() { <-held })

	for i := 1; i <= 3; i++ {
		h.Tracker.Mutate(fmt.Sprint(i), func(iss *core.Issue) { iss.Labels = nil })
	}

	// One per tick, and a *different* one each time: the wedged disposal is not a
	// candidate for a new one, so the turn passes to somebody else.
	for want := 1; want <= 3; want++ {
		h.Tick()
		waitFor(t, fmt.Sprintf("disposal %d to start", want), func() bool {
			return lifecycle.enteredRevocations() == want
		})
	}
	seen := map[string]bool{}
	for _, ws := range lifecycle.endedCycles() {
		seen[ws.Key] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three turns went to %d distinct cycles: %v", len(seen), seen)
	}
	if got := h.Tracker.ReleaseCount("1") + h.Tracker.ReleaseCount("2") + h.Tracker.ReleaseCount("3"); got != 0 {
		t.Errorf("released %d claims whose ended cycles are not disposed", got)
	}
	if got := h.o.HeldCount(); got != 3 {
		t.Errorf("held count = %d; a claim left while its cycle was undisposed", got)
	}

	lifecycle.gateRevocation(nil)
	close(held)
	for i := 1; i <= 3; i++ {
		id := fmt.Sprint(i)
		waitFor(t, "claim "+id+" to be released", func() bool { return h.Tracker.ReleaseCount(id) == 1 })
	}
	waitFor(t, "the held set to empty", func() bool { return h.o.HeldCount() == 0 })
}

// The same fairness question with the *other* failure shape, and the one that
// actually needs the cursor. A disposal that blocks leaves the candidate set as
// soon as it starts, so the next turn goes to somebody else however the offer is
// picked; a disposal that fails *fast* is back in the set before the next tick,
// so a rotation that did not advance would re-offer the same cycle forever and
// the rest would never be attempted at all.
func TestRefusedEndedCycleDisposalsTakeTurns(t *testing.T) {
	h, lifecycle := heldCycleHarness(t, 3)

	lifecycle.refuseRevocation(func(core.Workspace) error {
		return errors.New("airlock: control plane unreachable")
	})
	for i := 1; i <= 3; i++ {
		h.Tracker.Mutate(fmt.Sprint(i), func(iss *core.Issue) { iss.Labels = nil })
	}

	// Three turns, however many ticks that takes: the count is the rotation's
	// business, and what this asserts is who got them.
	h.tickUntil("three disposals have been attempted", func() bool {
		return lifecycle.enteredRevocations() >= 3
	})
	seen := map[string]bool{}
	for _, ws := range lifecycle.endedCycles() {
		seen[ws.Key] = true
	}
	if len(seen) != 3 {
		t.Fatalf("three turns went to %d distinct cycles: %v", len(seen), seen)
	}

	// And the retries are paced by the tick. A refusal that returns immediately
	// frees the slot immediately, so a driver that re-admitted on the way out
	// would spin against a backend that is already failing — which shows up here
	// as attempts accumulating while no tick happens.
	before := lifecycle.enteredRevocations()
	time.Sleep(50 * time.Millisecond)
	if got := lifecycle.enteredRevocations(); got != before {
		t.Errorf("%d further disposals started with no tick between them; retries must be paced by the tick",
			got-before)
	}

	// None of them was released or dropped on the strength of a refusal.
	for i := 1; i <= 3; i++ {
		if got := h.Tracker.ReleaseCount(fmt.Sprint(i)); got != 0 {
			t.Errorf("issue %d released %d times with its disposal unconfirmed", i, got)
		}
	}
	if got := h.o.HeldCount(); got != 3 {
		t.Errorf("held count = %d, want the 3 claims still owing a disposal", got)
	}
}

// The route that never becomes a held claim at all.
//
// A published run needs its claim-cycle anchor before it can convert, and a
// change log that has not caught up cannot supply one. onDoneOwnership settles
// that against current state instead — and when current state says the issue has
// left the workflow, the record releases *directly*, without ever reaching
// releaseHeld. `on_success` disposed the claim at `done` and left the sandbox
// suspended, so this is the exit through which a whole workspace cycle used to
// leave with nothing releasing it.
func TestAnchorlessPublishedClaimStillDisposesItsEndedCycle(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			// `done`'s third label write, held open so the two mutations below are
			// in place before the anchor read goes out.
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	// The log no longer shows the assignment that established this claim, so the
	// anchor read comes back empty and the confirming Get decides. The issue is
	// still ours and still open — only its required labels are gone, which is a
	// clean review's own gesture.
	h.Tracker.AppendHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventUnassigned, Subject: fake.DefaultPrincipal, At: epoch,
	})
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	close(gate)

	// Ticked rather than merely waited on: the anchor read and its confirming Get
	// resolve off the tick, so the obligation they settle takes the next tick's
	// turn at the bounded driver.
	h.tickUntil("the ended cycle is disposed", func() bool {
		return lifecycle.countOutcome("revocation") == 1
	})
	h.WaitGone("1")
	if cycles := lifecycle.endedCycles(); len(cycles) != 1 || cycles[0].Key != "issue-1" {
		t.Fatalf("disposed %+v, want the published claim's own workspace cycle", cycles)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 1 {
		t.Errorf("released %d times, want 1", got)
	}
}

// The disposal is ordered ahead of the release on this route too, and the
// ordering is what makes the obligation survive a crash: while the claim stands,
// §9.10 step 1 enumerates it and recovery re-derives the same verdict.
func TestAnchorlessPublishedClaimHoldsItsReleaseBehindTheDisposal(t *testing.T) {
	var labelWrites atomic.Int32
	gate := make(chan struct{})
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() {
				if labelWrites.Add(1) == 3 {
					<-gate
				}
			})
		},
	})
	h.WaitState("1", StateDone)

	held := make(chan struct{})
	lifecycle.gateRevocation(func() { <-held })

	h.Tracker.AppendHistory("1", core.ClaimEvent{
		Kind: core.ClaimEventUnassigned, Subject: fake.DefaultPrincipal, At: epoch,
	})
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	close(gate)

	h.tickUntil("the ended-cycle disposal to start", func() bool {
		return lifecycle.enteredRevocations() == 1
	})
	h.Tick()
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Fatalf("released %d times while the ended cycle's disposal was still out", got)
	}

	lifecycle.gateRevocation(nil)
	close(held)
	waitFor(t, "the claim to be released", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	h.WaitGone("1")
}

// The local substrate is untouched: `done` already removed the worktree, so a
// held claim there owes no disposal and the end-of-cycle route is never entered.
//
// Asserted on the log because that is the whole observable difference. A held
// record that wrongly owed a disposal would still release — driveHeldDisposal
// finds no policy, says so, and hands over — so the only thing separating
// "correct" from "one spurious step per released claim" is that this line is
// absent. On a remote substrate the same wrong obligation over an unresolvable
// workspace is not cosmetic at all: it would retry a disposal for a cycle that
// cannot be named and never release the claim.
func TestLocalSubstrateHeldClaimOwesNoCycleDisposal(t *testing.T) {
	h := doneHarness(t, 1)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	h.Tick()

	waitFor(t, "the sweep to release", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
	waitFor(t, "the held record to drop", func() bool { return h.o.HeldCount() == 0 })

	for _, msg := range []string{
		"ended workspace cycle",
		"has no end-of-cycle policy",
	} {
		if got := h.Logs.find(msg); len(got) != 0 {
			t.Errorf("the local path reported %q: %+v", msg, got)
		}
	}
}

// A claim revoked while the daemon was down reaches the same obligation, and
// reaching it is what keeps the effect queue free.
//
// Recovery used to owe this disposal like any other effect, and a record's owed
// effects run on the one serial queue every issue's label writes and milestone
// comments share. A remote revocation does not return until compute release,
// volume destruction and record tombstoning each confirm — minutes, on a control
// plane that is merely slow — so one of them at the head of that queue stalls the
// projection for every other issue. That is what this asserts by *not* observing:
// the second issue's recovery writes land while the first's disposal is wedged.
func TestRecoveredRevocationDoesNotBlockUnrelatedTrackerWrites(t *testing.T) {
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		concurrency: "5",
		issues:      []core.Issue{fake.Issue("1", epoch)},
		script:      startedOnly,
		hang:        true,
	})
	h.WaitState("1", StateRunning)

	// A clean review landed for issue 1 while the daemon was down: its required
	// labels are gone, so §9.10 gate 4 releases the claim — and ends its cycle.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = nil })
	wedged := make(chan struct{})
	var once sync.Once
	unwedge := func() { once.Do(func() { close(wedged) }) }
	// Deferred as well as called below, so a failing assertion cannot leave the
	// disposal goroutine parked on a channel the harness's cleanup then waits for.
	defer unwedge()
	lifecycle.gateRevocation(func() { <-wedged })

	// The previous tenure's runs are confirmed gone, so §9.10's workspace
	// precondition is satisfied and the gates decide. Without it every candidate
	// is possibly-live and recovery retains rather than releasing — a different
	// row of the table, and not the one this test is about.
	if err := h.restart(harnessOpts{
		concurrency: "5", workspaces: lifecycle,
		runGone: func(core.RunEvidence) (bool, error) { return true, nil },
	}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Ticked, and it takes more than one: recovery's own projection writes and the
	// claim-base abandon are queued ahead of this record's exit, and all of them
	// touch the workspace the disposal is about to delete (owesBeforeExit).
	h.tickUntil("the recovered cycle's disposal to start", func() bool {
		return lifecycle.enteredRevocations() == 1
	})
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Fatalf("released %d times while the ended cycle's disposal was still out", got)
	}

	// A *new* issue, queued strictly after the disposal is known to be out — which
	// is the whole fixture. Recovery's own writes are enqueued before the loop
	// starts draining them, so asserting on one of those would pass whichever side
	// of the queue the disposal is on: it would simply have gone in first.
	// Dispatching this one needs `ben:claimed` to land, and `ben:claimed` is an
	// owed projection on the queue the disposal must not be holding.
	h.Tracker.Set(fake.Issue("3", epoch.Add(2*time.Minute)))
	h.Tick()
	h.WaitState("3", StateRunning)

	// And the control plane coming back finishes it.
	lifecycle.gateRevocation(nil)
	unwedge()
	waitFor(t, "the recovered claim to be released", func() bool { return h.Tracker.ReleaseCount("1") == 1 })
}

// The claim with nothing to release, which is where the obligation is least
// recoverable and was least protected.
//
// An issue deleted from the tracker leaves a record whose exit is the *forget*
// rather than the release — there is no assignment to give up. That also means
// there is no standing claim for §9.10 to re-derive the obligation from, so if
// the record vanishes and the daemon then stops, the sandbox and its volume are
// left with nothing anywhere that can name them. Two rules cover it: the forget
// is held behind the disposal exactly as a release is, and the drain waits for
// every outstanding obligation.
func TestGoneClaimHoldsItsForgetAndItsDrainUntilTheCycleIsDisposed(t *testing.T) {
	h, lifecycle := lifecycleHarness(t, &harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: startedOnly,
		hang:   true,
	})
	h.WaitState("1", StateRunning)

	wedged := make(chan struct{})
	var once sync.Once
	unwedge := func() { once.Do(func() { close(wedged) }) }
	defer unwedge()
	lifecycle.gateRevocation(func() { <-wedged })

	h.Tracker.Delete("1")
	h.tickUntil("the gone claim's cycle disposal to start", func() bool {
		return lifecycle.enteredRevocations() == 1
	})

	// The record is the last thing that knows about this sandbox, so it stays.
	h.Tick()
	if got := h.stateOf("1"); got == "" {
		t.Fatal("the record was forgotten while its ended cycle's disposal was still out")
	}

	// And so does the drain. A daemon that left here would take the only account
	// of the allocation with it.
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.o.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	}()
	select {
	case <-stopped:
		t.Fatal("the drain completed with an ended workspace cycle's disposal still out")
	case <-time.After(100 * time.Millisecond):
	}

	unwedge()
	select {
	case <-stopped:
	case <-time.After(barrierBudget):
		t.Fatal("the drain did not complete after the disposal landed")
	}
	if got := lifecycle.countOutcome("revocation"); got != 1 {
		t.Errorf("ended-cycle disposals = %d, want 1", got)
	}
}

// AC1 as a property of the value rather than of a code path: a held claim
// retains the opaque cycle identity and nothing else, and on the local substrate
// it retains nothing at all — `done` has already removed the worktree, so a
// retained path would name a directory that is not there.
func TestHeldWorkspaceRetainsOnlyTheOpaqueCycleIdentity(t *testing.T) {
	full := core.Workspace{
		WorkspacePaths: core.WorkspacePaths{
			Path:         "remote:acme/widgets#issue-1@42",
			SharedGitDir: "/var/lib/ben/git",
			PrivateDir:   "/var/lib/ben/private/issue-1",
		},
		Key:        "issue-1",
		Branch:     "ben/issue-1",
		ClaimEpoch: 7,
		BaseSHA:    "0f1e2d3c",
		CreatedNow: true,
	}

	t.Run("a local provider retains nothing", func(t *testing.T) {
		if got := cycleWorkspace(&fake.Workspaces{}, full); got != (core.Workspace{}) {
			t.Fatalf("cycleWorkspace = %+v for a provider with no end-of-cycle policy, want the zero value", got)
		}
	})

	t.Run("a remote provider retains the address and nothing else", func(t *testing.T) {
		got := cycleWorkspace(&lifecycleWorkspaces{}, full)
		want := core.Workspace{
			WorkspacePaths: core.WorkspacePaths{Path: full.Path},
			Key:            full.Key,
			Branch:         full.Branch,
			ClaimEpoch:     full.ClaimEpoch,
		}
		if got != want {
			t.Fatalf("cycleWorkspace = %+v, want %+v", got, want)
		}
		if got.SharedGitDir != "" || got.PrivateDir != "" {
			t.Error("a held claim carries host filesystem paths")
		}
		if got.BaseSHA != "" {
			t.Error("a held claim carries a verification base only the checker may read")
		}
	})

	t.Run("a workspace that was never authorized is not an address", func(t *testing.T) {
		if got := cycleWorkspace(&lifecycleWorkspaces{}, core.Workspace{}); got != (core.Workspace{}) {
			t.Fatalf("cycleWorkspace = %+v for an unauthorized claim, want the zero value", got)
		}
	})
}
