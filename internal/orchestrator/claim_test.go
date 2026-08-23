package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// Claiming an issue and giving it back (SPEC §8.4, §9.2, §9.8). Claim has more
// than one answer and they are not interchangeable: verified, refused, errored,
// and refused-before-writing-anything. Only the refusal carries the unwind
// guarantee, so only an error may have left an assignment standing — and
// assigned-with-no-state-label is the one shape §9.10 never touches again.
//
// The rest of the file is the projection: `ben:claimed` is what §8.3 excludes an
// issue on, so it may not be written before the claim verifies, and no attempt
// may start before it lands. The claim *anchor* — dating a close against the
// current claim cycle — and the retained `done` claim are held_test.go.

// A lost claim must leave the issue exactly as it was found. §9.2's trigger
// for queued → claimed is "Claim() verified by read-back", and that
// transition projects ben:claimed — so projecting before the claim verifies
// would leave a state label on an issue we do not own, and §8.3 excludes any
// issue carrying one. The loser of a race would block it for everyone.
func TestLostClaimProjectsNothing(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.ClaimVerified = func(string) bool { return false }
		},
	})
	h.WaitGone("1")

	calls, labels, comments := h.Tracker.Snapshot()
	if got, ok := labels["1"]; ok {
		t.Errorf("projected %q for a claim that never verified; §8.3 would then block the issue for everyone", got)
	}
	if got := comments["1"]; len(got) != 0 {
		t.Errorf("commented %v on an issue we do not own", got)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "label ") || strings.HasPrefix(c, "comment ") {
			t.Errorf("wrote %q for an unverified claim; calls = %v", c, calls)
		}
	}
	if got := h.o.Transitions.For("1"); len(got) != 0 {
		t.Errorf("logged %v for a claim that never landed", got)
	}
}

func TestPendingClaimBaseIsDurableBeforeClaimProjection(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		claimBaseGate: func() {
			close(entered)
			<-release
		},
	})
	<-entered

	if got := h.stateOf("1"); got != StateQueued {
		t.Fatalf("state while pending write is blocked = %s, want queued", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Fatalf("label while pending write is blocked = %q, want none", got)
	}
	if got := h.Workspaces.PrepareCount("1"); got != 0 {
		t.Fatalf("prepared %d workspaces before pending was durable", got)
	}
	if got := h.Runner.StartCount(); got != 0 {
		t.Fatalf("started %d runs before pending was durable", got)
	}

	close(release)
	h.WaitState("1", StateDone)
	state, err := h.Workspaces.ClaimBase(t.Context(), core.Issue{Identifier: "1"})
	if err != nil || state.State != core.ClaimBasePinned || state.Epoch <= 0 {
		t.Fatalf("claim base after dispatch = %+v, %v; want a positive pinned epoch", state, err)
	}
	history, err := h.Tracker.ClaimHistory(t.Context(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if want := claimCycleAnchor(history, fake.DefaultPrincipal); state.Epoch != want {
		t.Errorf("claim epoch = %d, want assignment event ID %d; a label occurrence must not substitute", state.Epoch, want)
	}
}

func TestPendingClaimBaseWriteFailureRetriesWithoutProjection(t *testing.T) {
	boom := errors.New("claim-base store unavailable")
	h := start(t, harnessOpts{
		issues:             []core.Issue{fake.Issue("1", epoch)},
		failBeginClaimBase: boom,
	})
	waitFor(t, "the first pending write attempt", func() bool {
		return h.Workspaces.ClaimBaseBeginCount("1") == 1
	})

	if got := h.stateOf("1"); got != StateQueued {
		t.Fatalf("state after pending write failure = %s, want queued", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Fatalf("label after pending write failure = %q, want none", got)
	}
	if got := h.Workspaces.PrepareCount("1"); got != 0 {
		t.Fatalf("prepared %d workspaces after pending write failure", got)
	}
	if got := h.Runner.StartCount(); got != 0 {
		t.Fatalf("started %d runs after pending write failure", got)
	}

	h.Workspaces.SetFailBeginClaimBase(nil)
	h.PollNow()
	h.WaitState("1", StateDone)
	if got := h.Workspaces.ClaimBaseBeginCount("1"); got != 2 {
		t.Errorf("pending write attempts = %d, want one failure and one retry", got)
	}
}

func TestFailedNewEpochInitializationRetriesFromTheOlderPin(t *testing.T) {
	boom := errors.New("claim-base replace unavailable")
	const oldEpoch = int64(17)
	h := start(t, harnessOpts{
		issues:             []core.Issue{fake.Issue("1", epoch)},
		failBeginClaimBase: boom,
		configureWorkspaces: func(w *fake.Workspaces) {
			w.SetClaimBase("1", core.ClaimBase{
				State: core.ClaimBasePinned, Epoch: oldEpoch, BaseSHA: fake.DefaultBaseSHA,
			})
		},
	})
	waitFor(t, "the failed E2 initialization", func() bool {
		return h.Workspaces.ClaimBaseBeginCount("1") == 1
	})

	if got := h.stateOf("1"); got != StateQueued {
		t.Fatalf("state after failed E2 initialization = %s, want queued retry", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Fatalf("label after failed E2 initialization = %q, want no projection", got)
	}

	h.Workspaces.SetFailBeginClaimBase(nil)
	h.PollNow()
	h.WaitState("1", StateDone)
	state, err := h.Workspaces.ClaimBase(t.Context(), core.Issue{Identifier: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if state.State != core.ClaimBasePinned || state.Epoch <= 0 || state.Epoch == oldEpoch {
		t.Errorf("claim base after retry = %+v, want a newly pinned E2", state)
	}
}

func TestPendingPrepareRetryToleratesAPathAndRetriesAMarkerRead(t *testing.T) {
	prepareErr := errors.New("fetch unavailable before pin")
	markerErr := errors.New("run-marker store unavailable")
	var prepares atomic.Int32
	h := start(t, harnessOpts{
		issues:    []core.Issue{fake.Issue("1", epoch)},
		prepRetry: func(error) bool { return true },
		configureWorkspaces: func(w *fake.Workspaces) {
			w.FailPrepare = func(string, int) error {
				if prepares.Add(1) == 1 {
					return prepareErr
				}
				return nil
			}
		},
	})
	h.WaitState("1", StateBackoff)
	state, err := h.Workspaces.ClaimBase(t.Context(), core.Issue{Identifier: "1"})
	if err != nil || state.State != core.ClaimBasePending {
		t.Fatalf("claim base after pre-pin failure = %+v, %v; want pending", state, err)
	}

	// The first timer can read neither presence nor absence. It must remain an
	// inert retry, not turn the retained path into a sticky epoch fault.
	h.Workspaces.SetFailMarkerRead(markerErr)
	reads := h.Tracker.HistoryReads()
	h.Clock.Advance(11 * time.Second)
	waitFor(t, "the failed live marker read", func() bool {
		return h.Tracker.HistoryReads() > reads
	})
	if got := h.stateOf("1"); got != StateBackoff {
		t.Fatalf("state after marker read failure = %s, want backoff retry", got)
	}
	if got := h.Workspaces.PrepareCount("1"); got != 1 {
		t.Fatalf("prepare count after marker read failure = %d, want no second prepare", got)
	}

	h.Workspaces.SetFailMarkerRead(nil)
	h.Clock.Advance(11 * time.Second)
	h.WaitState("1", StateDone)
	if got := h.Workspaces.PrepareCount("1"); got != 2 {
		t.Errorf("prepare count after recovery = %d, want failed pre-pin call plus retry", got)
	}
}

func TestTerminalPrePinFailureAbandonsPendingEpochBeforeReassignment(t *testing.T) {
	boom := errors.New("permanent pre-pin failure")
	var prepares atomic.Int32
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		configureWorkspaces: func(w *fake.Workspaces) {
			w.FailPrepare = func(string, int) error {
				if prepares.Add(1) == 1 {
					return boom
				}
				return nil
			}
		},
	})
	waitFor(t, "the terminal pre-pin failure", func() bool {
		return h.Workspaces.PrepareCount("1") == 1
	})
	h.WaitGone("1")

	issue := core.Issue{Identifier: "1"}
	if got, err := h.Workspaces.ClaimBase(t.Context(), issue); err != nil || got.State != core.ClaimBaseAbsent {
		t.Fatalf("claim base after terminal release = %+v, %v; want abandoned pending state to be absent", got, err)
	}

	// The human clears the terminal projection and returns the issue to the
	// queue. A new assignment must establish its own epoch instead of being
	// refused by the failed assignment's pending intent.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Labels = []string{"ben-queue"}
		i.Dispatchable = true
	})
	h.PollNow()
	h.WaitState("1", StateDone)
	got, err := h.Workspaces.ClaimBase(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.ClaimBasePinned || got.Epoch <= 0 || got.BaseSHA == "" {
		t.Errorf("claim base after reassignment = %+v, want a newly pinned epoch/base pair", got)
	}
	if begins := h.Workspaces.ClaimBaseBeginCount("1"); begins != 2 {
		t.Errorf("claim-base begins = %d, want one per assignment", begins)
	}
}

func TestVerificationValidatesClaimEpochBeforeCallingVerifier(t *testing.T) {
	beforeSuccess := make(chan struct{})
	releaseSuccess := make(chan struct{})
	var verifierCalls atomic.Int32
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Succeed("s") },
		eventGate: func(index int) {
			if index == 2 {
				close(beforeSuccess)
				<-releaseSuccess
			}
		},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifierCalls.Add(1)
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/old"}, nil
		}),
	})
	<-beforeSuccess
	h.WaitState("1", StateRunning)

	// Simulate loss of the provider-owned authority after the run began but
	// before its success event is routed. The §9.7 checker must remain entirely
	// uncalled; rejecting inside PublishFacts would be one layer too late.
	h.Workspaces.SetClaimBase("1", core.ClaimBase{State: core.ClaimBaseAbsent})
	close(releaseSuccess)
	h.WaitState("1", StateNeedsReview)
	if got := verifierCalls.Load(); got != 0 {
		t.Errorf("verifier calls = %d, want 0 before a valid current epoch/base pair", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNeedsReview {
		t.Errorf("label = %q, want sticky needs-review", got)
	}
	if containsMilestone(h.Tracker.Milestones("1"), core.MilestonePublished) {
		t.Error("published an attempt whose claim epoch disappeared")
	}
}

func TestVerificationRejectsAReassignmentBeforeCallingVerifier(t *testing.T) {
	beforeSuccess := make(chan struct{})
	releaseSuccess := make(chan struct{})
	var verifierCalls atomic.Int32
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Succeed("s") },
		eventGate: func(index int) {
			if index == 2 {
				close(beforeSuccess)
				<-releaseSuccess
			}
		},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifierCalls.Add(1)
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/old"}, nil
		}),
	})
	<-beforeSuccess
	h.WaitState("1", StateRunning)

	// The final assignee set is unchanged, but these two tracker acts create a
	// new claim-establishing event. Provider state still belongs to the old
	// assignment, so §9.7 must remain entirely unreachable.
	h.Tracker.UnassignBy("1", fake.DefaultPrincipal)
	h.Tracker.ClaimBy("1", fake.DefaultPrincipal)
	close(releaseSuccess)

	h.WaitState("1", StateNeedsReview)
	if got := verifierCalls.Load(); got != 0 {
		t.Errorf("verifier calls = %d, want 0 after the tracker reminted the assignment epoch", got)
	}
	if containsMilestone(h.Tracker.Milestones("1"), core.MilestonePublished) {
		t.Error("published work under an epoch superseded by reassignment")
	}
}

func TestVerificationRetriesAClaimEpochReadBeforeCallingVerifier(t *testing.T) {
	beforeSuccess := make(chan struct{})
	releaseSuccess := make(chan struct{})
	var verifierCalls atomic.Int32
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Succeed("s") },
		eventGate: func(index int) {
			if index == 2 {
				close(beforeSuccess)
				<-releaseSuccess
			}
		},
		verifier: verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
			verifierCalls.Add(1)
			return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
		}),
	})
	<-beforeSuccess
	h.WaitState("1", StateRunning)

	h.Tracker.SetFailHistory(errors.New("tracker history unavailable"))
	close(releaseSuccess)
	waitFor(t, "the failed verification epoch read", func() bool { return h.Tracker.HistoryReads() >= 2 })
	h.WaitState("1", StateVerifying)
	if got := verifierCalls.Load(); got != 0 {
		t.Fatalf("verifier calls = %d, want 0 while the current assignment epoch is unreadable", got)
	}

	h.Tracker.SetFailHistory(nil)
	h.PollNow()
	h.WaitState("1", StateDone)
	if got := verifierCalls.Load(); got != 1 {
		t.Errorf("verifier calls = %d, want one after the epoch read recovered", got)
	}
}

// SPEC §9.8 asks whether the issue is still *ours*, not whether it has
// exactly one assignee. A human who unassigns BEN and takes the issue leaves
// one assignee and no claim.
func TestRoutableChecksAssigneeIdentity(t *testing.T) {
	tests := []struct {
		name      string
		assignees []string
		want      bool
	}{
		{"our claim alone", []string{fake.DefaultPrincipal}, true},
		{"case-insensitive", []string{strings.ToUpper(fake.DefaultPrincipal)}, true},
		{"a human replaced us", []string{"a-human"}, false},
		{"a human joined us", []string{fake.DefaultPrincipal, "a-human"}, false},
		{"nobody", nil, false},
	}
	o := idleOrchestrator(t, fake.NewTracker())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := core.Issue{Identifier: "1", State: "open", Labels: []string{"ben-queue"}, Assignees: tt.assignees}
			if got := o.routable(o.configNow(), issue); got != tt.want {
				t.Errorf("routable(%v) = %v, want %v", tt.assignees, got, tt.want)
			}
		})
	}
}

// The same, end to end: a human who takes the issue over is noticed and the
// run is stopped, workspace kept.
func TestHumanReplacingTheClaimStopsTheRun(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = []string{"a-human"} })
	h.Tick()
	h.WaitGone("1")

	if got := h.Workspaces.Disposals("1"); len(got) != 1 || !got[0].Keep {
		t.Errorf("disposals = %+v, want the workspace kept for an unroutable issue", got)
	}
}

// A release the tracker refused is retried; the record — and therefore the
// claim — is not forgotten on the strength of one failed write.
func TestFailedReleaseIsRetried(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureAuth) },
		beforeStart: func(tr *fake.Tracker) {
			tr.SetFailRelease(errors.New("503 from the tracker"))
		},
	})
	h.WaitState("1", StateFailed)

	waitFor(t, "the first release attempt", func() bool { return h.Tracker.ReleaseAttempts("1") > 0 })
	if h.stateOf("1") == "" {
		t.Fatal("the record was forgotten before the release succeeded; the claim would be stranded")
	}

	h.Tracker.SetFailRelease(nil)
	h.Tick()
	h.WaitGone("1")
	if n := h.Tracker.ReleaseCount("1"); n != 1 {
		t.Errorf("succeeded releases = %d, want 1", n)
	}
}

// ClaimPrincipal is required: without it §9.8 cannot tell our own claim from
// a human who replaced it, and the check would silently degrade to counting
// assignees.
func TestNewRequiresAClaimPrincipal(t *testing.T) {
	def := definition(t, "3", "")
	_, err := New(Config{Runtime: newTestSource(def, &Bundle{
		Definition: def,
		Tracker:    fake.NewTracker(),
		Workspaces: fake.NewWorkspaces(),
		Runner:     fake.NewRunner(),
		Verifier:   alwaysPublished,
	})})
	if err == nil {
		t.Fatal("New accepted an empty ClaimPrincipal")
	}
	if !strings.Contains(err.Error(), "ClaimPrincipal") {
		t.Errorf("error = %v, want it to name ClaimPrincipal", err)
	}
}

// SPEC §9.10 re-dispatches "assigned, no ben:* label" at attempt 1 on the
// stated grounds that label projection precedes preparing, "so no attempt can
// have run". Starting the agent while the label write is still queued would
// make that false, and recovery would re-run an attempt it believes never
// happened.
func TestNoAttemptStartsBeforeTheClaimLabelLands(t *testing.T) {
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetLabelGate(func() { <-gate })
		},
	})

	waitFor(t, "the claim to verify", func() bool { return h.stateOf("1") == StateClaimed })
	// The label write is blocked. Nothing may have started.
	time.Sleep(20 * time.Millisecond)
	if n := h.Workspaces.PrepareCount("1"); n != 0 {
		t.Fatalf("prepared %d attempts before ben:claimed landed", n)
	}
	if n := h.Runner.StartCount(); n != 0 {
		t.Fatalf("started %d runs before ben:claimed landed — a crash here would look like §9.10's unprojected claim", n)
	}

	close(gate)
	h.WaitState("1", StateDone)
	if n := h.Runner.StartCount(); n != 1 {
		t.Errorf("started %d runs once the label landed, want 1", n)
	}
}

// A claim whose label write is still retrying can sit in claimed for several
// ticks. If the issue goes terminal in that window, the eventual projection
// must not start work on it.
func TestClaimProjectedAfterTheIssueClosedStartsNothing(t *testing.T) {
	gate := make(chan struct{})
	h := start(t, harnessOpts{
		issues:      []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) { tr.SetLabelGate(func() { <-gate }) },
	})
	waitFor(t, "the claim to verify", func() bool { return h.stateOf("1") == StateClaimed })

	// The issue closes while the projection is stuck.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tick()

	close(gate)
	h.WaitGone("1")

	if n := h.Workspaces.PrepareCount("1"); n != 0 {
		t.Errorf("prepared %d attempts for an issue that closed before the claim projected", n)
	}
	if n := h.Runner.StartCount(); n != 0 {
		t.Errorf("started %d runs on a closed issue", n)
	}
}

// SPEC §8.4 + §9.10 step 3: an error from Claim is not the same answer as a
// refusal, and only the refusal carries the unwind guarantee. The adapter's two
// riskiest paths — an unverifiable read-back and an unorderable race — both try
// to release and both return a *joined error* when that release also fails, so
// an error is precisely the case where an assignment may be standing.
//
// Forgetting there leaves assigned-with-no-state-label, which §9.10 step 3
// classifies as published-awaiting-review and never touches again: the issue is
// undispatchable by anyone, forever, with no PR behind it.
func TestAClaimThatErroredIsReleasedRatherThanForgotten(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetClaimError(errors.New("verifying claim: 502; unwinding unverifiable claim: 502"))
		},
	})

	waitFor(t, "the release of a claim that may have landed", func() bool {
		return h.Tracker.ReleaseCount("1") == 1
	})
	h.WaitGone("1")

	if got := h.issueAssignees("1"); len(got) != 0 {
		t.Errorf("assignees = %v; a failed claim stranded its assignment", got)
	}
	// Nothing was ever projected, so the issue is left exactly as it was found
	// — which is the shape a human, or this daemon's next tick, can pick up.
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Errorf("label = %q, want none for a claim that never verified", got)
	}
}

// The third answer, and the one the §8.5 request budget makes necessary: an
// error the adapter can prove it reached before writing anything
// (core.ErrClaimNotAttempted). There is no assignment to unwind, and the release
// is itself a write — so paying it here would spend the write capacity whose
// exhaustion is the usual reason for the refusal, and hold a §9.5 concurrency
// slot for an issue this daemon does not own.
func TestAClaimRefusedBeforeWritingIsNotReleased(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.SetClaimError(fmt.Errorf("%w: per-tick GitHub request budget spent", core.ErrClaimNotAttempted))
		},
	})
	h.WaitGone("1")

	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times; the adapter promised there was nothing to undo", n)
	}
	if got := h.issueAssignees("1"); len(got) != 0 {
		t.Errorf("assignees = %v; the refusal promised no assignment was written", got)
	}
	// Left exactly as it was found, so an ordinary poll can dispatch it again
	// once the budget or the server's Retry-After allows.
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Errorf("label = %q, want none for a claim that never began", got)
	}
}

// The other half, and the reason the two answers are handled separately: a
// claim the adapter *refused* has already been unwound (`false, nil` is it
// saying so), so releasing again would spend a request to undo nothing.
func TestARefusedClaimIsNotReleasedAgain(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		beforeStart: func(tr *fake.Tracker) {
			tr.ClaimVerified = func(string) bool { return false }
		},
	})
	h.WaitGone("1")

	if n := h.Tracker.ReleaseCount("1"); n != 0 {
		t.Errorf("released %d times; the adapter had already unwound this claim (SPEC §8.4)", n)
	}
}
