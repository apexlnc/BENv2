package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// SPEC §9.10, the driver. Every restart test here drives the daemon to a state
// with **its own writes**, drops the orchestrator, and builds a new one on the
// same fakes — so what recovery reads back is what a daemon actually left
// behind. A test that hand-authored the change log would be testing the fixture:
// the fake's ordering would have to be adjusted to whatever the implementation
// did, and the two would agree while both were wrong (AGENTS.md; the ordering
// itself is anchored against the real adapter in
// TestSetStateLabelsLogsTheAddBeforeTheRemove).
//
// Three of §9.10's subjects outgrew this file and live beside it (#160):
// marker_test.go for the run marker, sweep_test.go for step 5's workspace
// sweep, and recover_reason_test.go for step 6's failure reason. This file keeps
// the driver and its verdicts — the restart table, the four gates, unknown
// launches, the retry pass, and the startup warnings — and it owns the fixtures
// all four share: `harness.restart`, the switchable prober and failure reader,
// `incompleteEvidence`, and the wait/milestone helpers below.

// restart models a kill and a fresh start on the same host: the process is gone,
// so nothing in memory survives — no records, no held claims, no armed timers —
// while the tracker, the git facts and the run markers are exactly as the dead
// daemon left them.
//
// It returns Recover's error rather than failing on it, because §6.4's soft
// failure is a thing several tests are about.
func (h *harness) restart(opts harnessOpts) error {
	h.t.Helper()
	h.Stop()

	// A fresh clock at the same instant. The wall clock did not reset, but every
	// timer the dead process had armed is gone with it — a clock carried over
	// would leave stale waiters that Advance fires into nobody.
	h.Clock = fake.NewClock(h.Clock.Now())

	var workspaces Workspaces = h.Workspaces
	if opts.workspaces != nil {
		workspaces = opts.workspaces
	}
	if opts.verifier == nil {
		opts.verifier = alwaysPublished
	}
	if opts.concurrency == "" {
		opts.concurrency = "3"
	}
	h.def = definition(h.t, opts.concurrency, opts.extraConfig)
	// A new bundle over the same adapters: a restart rebuilds every adapter from
	// the same configuration against the same world.
	h.Bundle = &Bundle{
		Definition:     h.def,
		Tracker:        h.Tracker,
		Workspaces:     workspaces,
		Runner:         h.Runner,
		Verifier:       opts.verifier,
		ClaimPrincipal: fake.DefaultPrincipal,
	}
	h.Source = newTestSource(h.def, h.Bundle)

	o, err := New(Config{
		Runtime:        h.Source,
		PrepRetryable:  opts.prepRetry,
		RunGone:        opts.runGone,
		FailureReasons: opts.failures,
		Clock:          h.Clock,
		Log:            slog.New(h.Logs),
		DaemonID:       "test-host/test-key",
	})
	if err != nil {
		h.t.Fatalf("New after restart: %v", err)
	}
	h.o = o

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan error, 1)

	recoverErr := o.Recover(ctx)
	go func() { h.done <- o.Run(ctx) }()
	h.settle(0)
	return recoverErr
}

// pinClaimBaseForRecovery installs the provider fact a non-epoch recovery test
// is not itself about. The epoch comes from the fake tracker's ordered history,
// so fixtures cannot accidentally use a milestone occurrence or a convenient
// counter in its place.
func pinClaimBaseForRecovery(t *testing.T, h *harness, identifier string) int64 {
	t.Helper()
	events, err := h.Tracker.ClaimHistory(context.Background(), core.Issue{Identifier: identifier})
	if err != nil {
		t.Fatalf("ClaimHistory(%s): %v", identifier, err)
	}
	claimEpoch := claimCycleAnchor(events, fake.DefaultPrincipal)
	if claimEpoch <= 0 {
		t.Fatalf("ClaimHistory(%s) has no positive current claim epoch: %+v", identifier, events)
	}
	h.Workspaces.SetClaimBase(identifier, core.ClaimBase{
		State: core.ClaimBasePinned, Epoch: claimEpoch, BaseSHA: fake.DefaultBaseSHA,
	})
	return claimEpoch
}

func pendClaimBaseForRecovery(t *testing.T, h *harness, identifier string) int64 {
	t.Helper()
	claimEpoch := pinClaimBaseForRecovery(t, h, identifier)
	h.Workspaces.SetClaimBase(identifier, core.ClaimBase{State: core.ClaimBasePending, Epoch: claimEpoch})
	return claimEpoch
}

// switchableProber and switchableFailures are the two §9.10 seams a test needs to
// *move* while the loop is running — "the group has since died", "the log became
// readable" — and Config is not the place to move them from: the authority
// goroutine reads it, so assigning a field mid-run is a data race the detector
// rightly rejects. The mutable cell belongs to the test, behind a lock, with only
// its method installed in Config.
type switchableProber struct {
	mu sync.Mutex
	fn func(core.RunEvidence) (bool, error)
}

func newProber(fn func(core.RunEvidence) (bool, error)) *switchableProber {
	return &switchableProber{fn: fn}
}

func (p *switchableProber) set(fn func(core.RunEvidence) (bool, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fn = fn
}

func (p *switchableProber) probe(e core.RunEvidence) (bool, error) {
	p.mu.Lock()
	fn := p.fn
	p.mu.Unlock()
	if fn == nil {
		return false, nil
	}
	return fn(e)
}

type switchableFailures struct {
	mu  sync.Mutex
	cur stubFailures
}

func newFailures(cur stubFailures) *switchableFailures {
	return &switchableFailures{cur: cur}
}

func (f *switchableFailures) set(cur stubFailures) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cur = cur
}

func (f *switchableFailures) LastFailure(id string) (core.RunFailure, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cur.LastFailure(id)
}

// groupGone is the prober for a restart whose previous run really did die —
// which is the overwhelmingly common case after a crash, and the one §9.10
// option 1 was chosen to keep converging automatically.
var groupGone = func(core.RunEvidence) (bool, error) { return true, nil }

// groupAlive is the opposite: the process group outlived the daemon, which
// `kill -9` makes reachable because every attempt runs in its own group
// (harness run.go, Setpgid).
var groupAlive = func(core.RunEvidence) (bool, error) { return false, nil }

// Run must refuse a loop that has not recovered. Without this, the mutation
// "drop the gate" makes every duplicate-dispatch test below pass by accident:
// they would each construct an orchestrator, never recover, and observe no
// second dispatch because the first tick had nothing to compare against.
func TestRunRefusesWithoutRecover(t *testing.T) {
	h := start(t, harnessOpts{skipRecover: true})
	select {
	case err := <-h.done:
		if !errors.Is(err, ErrNotRecovered) {
			t.Fatalf("Run returned %v, want ErrNotRecovered", err)
		}
		// Put it back: the harness cleanup waits on this channel, and a refusal is
		// still the loop having returned.
		h.done <- err
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return; it must refuse a loop that has not recovered")
	}
	// Nothing was claimed, which is the consequence the refusal exists for.
	if calls, _, _ := h.Tracker.Snapshot(); len(calls) != 0 {
		t.Errorf("a loop that refused to start still wrote to the tracker: %v", calls)
	}
}

// The kill points of the acceptance criteria, each driven by the daemon's own
// writes and then restarted.
//
// The table is keyed by *what landed*, not by the dying daemon's in-memory state,
// because nothing in memory survives a kill: recovery reads the tracker, the git
// facts and the run marker, and nothing else. That is also why `claimed` and
// `preparing` are one row — they leave an identical world behind (ben:claimed
// standing, no marker, since the marker is written at the launch and neither state
// has reached one), so a table that listed them separately would be asserting the
// same input twice and calling it coverage.
func TestRestartAtEachKillPoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts harnessOpts
		// waitFor is where the first daemon is held before the kill.
		waitFor State
		// wantLabel is the projection the dead daemon left standing.
		wantLabel core.StateLabel
		// wantMarker says whether a run marker survives it.
		wantMarker bool
		wantState  State
		wantFloor  int
	}{
		{
			name: "claimed/preparing: ben:claimed landed, no launch was reached",
			opts: harnessOpts{
				// Prepare never returns, so no attempt is launched and no marker is
				// written. This is the whole window between the label and the launch.
				prepareGate: func() { time.Sleep(time.Hour) },
			},
			waitFor:   StatePreparing,
			wantLabel: core.StateLabelClaimed,
			wantState: StatePreparing,
			wantFloor: 1,
		},
		{
			name:       "running: an agent is live and its marker identifies it",
			opts:       harnessOpts{script: startedOnly, hang: true},
			waitFor:    StateRunning,
			wantLabel:  core.StateLabelRunning,
			wantMarker: true,
			wantState:  StateBackoff,
			wantFloor:  2,
		},
		{
			name: "backoff: an attempt failed and a retry is armed",
			opts: harnessOpts{
				script: func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureStalled) },
			},
			waitFor:   StateBackoff,
			wantLabel: core.StateLabelClaimed,
			wantState: StateBackoff,
			wantFloor: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.issues = []core.Issue{fake.Issue("1", epoch)}
			opts.runGone = groupGone
			opts.verifier = incompleteEvidence
			h := start(t, opts)
			h.WaitState("1", tc.waitFor)

			// The evidence a real orphan leaves: commits on the branch that nobody
			// pushed, so §9.7 is incomplete and §9.10 calls it an orphan.
			h.Workspaces.SetFacts(func(ws core.Workspace) (core.PublishFacts, error) {
				return core.PublishFacts{Head: "aaaa", DescendsBase: true, RemoteProbed: true}, nil
			})

			if got := h.Tracker.Label("1"); got != tc.wantLabel {
				t.Fatalf("before the restart the label is %q, want %q — the fixture is not at the kill point",
					got, tc.wantLabel)
			}
			if _, ok := h.Workspaces.RunMarkerFor("1"); ok != tc.wantMarker {
				t.Fatalf("marker present = %v, want %v at this kill point", ok, tc.wantMarker)
			}
			startsBefore := h.Runner.StartCount()

			if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			// The claim is still ours. A restart that released would hand the issue
			// back to the queue with its work half done.
			if got := h.Tracker.ReleaseCount("1"); got != 0 {
				t.Errorf("the claim was released %d times across the restart; recovery retains it", got)
			}
			if !containsFold(h.issueAssignees("1"), fake.DefaultPrincipal) {
				t.Errorf("the principal is no longer assigned after recovery: %v", h.issueAssignees("1"))
			}

			// A record owns the issue before any tick could claim it again. A clean
			// pending epoch is still the first prepare and resumes at attempt 1;
			// only a pinned epoch with prior run evidence is an orphan at floor 2.
			h.WaitState("1", tc.wantState)
			if snap := h.statusFor("1"); snap.Attempt < tc.wantFloor {
				t.Errorf("attempt = %d, want >= %d for recovered %s",
					snap.Attempt, tc.wantFloor, tc.wantState)
			}

			// No second agent. The orphan waits for its backoff timer; recovery
			// itself launches nothing.
			if got := h.Runner.StartCount(); got != startsBefore {
				t.Errorf("recovery started %d new runs; §9.10 dispatches no agent", got-startsBefore)
			}
			// The projection is completed and the claimed milestone re-issued.
			if got := h.Tracker.Label("1"); got != core.StateLabelClaimed {
				t.Errorf("label = %q, want ben:claimed — the orphan's projection is completed", got)
			}
			if !hasMilestone(h.Tracker.Milestones("1"), core.MilestoneClaimed) {
				t.Error("no claimed milestone; every recovery verdict re-issues the comment for the state it lands in")
			}
		})
	}
}

// The orphan resumes on the same branch, at attempt >= 2, and Prepare reattaches
// rather than force-creating (SPEC §6.2, never -B).
func TestARecoveredOrphanResumesOnTheSameBranch(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)
	branchBefore := h.Workspaces.Prepares("1")[0]

	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateBackoff)

	// Attempt >= 2 is the whole of §9.10's "work may already exist": the prompt
	// reads it that way (§5.6) and max_attempts is measured from the floor.
	snap := h.statusFor("1")
	if snap.Attempt < 2 {
		t.Errorf("recovered orphan is at attempt %d, want >= 2 — §9.10 says work may already exist", snap.Attempt)
	}

	// Fire the backoff and check the prepare.
	h.Tick()
	prepares := h.Workspaces.Prepares("1")
	if len(prepares) < 2 {
		t.Fatalf("the recovered orphan never prepared again: %+v", prepares)
	}
	resumed := prepares[len(prepares)-1]
	if resumed.Attempt < 2 {
		t.Errorf("the resumed prepare is attempt %d, want >= 2", resumed.Attempt)
	}
	// The same claim-time base, which is what makes it the same branch rather than
	// a new one: a re-minted pin is how "branch advanced past its base" would
	// silently start measuring from the wrong commit (§6.2, #11).
	if resumed.BaseSHA != branchBefore.BaseSHA {
		t.Errorf("the resumed prepare pinned base %q, want the claim-time base %q — recovery must reattach, never -B",
			resumed.BaseSHA, branchBefore.BaseSHA)
	}
}

// Killed after assignment and before the pending epoch is durable. The branch
// visible now cannot reconstruct the historical pinning instant, so recovery
// parks rather than manufacturing a base and dispatching attempt one.
func TestAnUnprojectedClaimWithoutPendingEpochParks(t *testing.T) {
	issue := fake.Issue("1", epoch)
	h := start(t, harnessOpts{runGone: groupGone})

	// The tracker state a daemon killed in that window leaves: assigned, no ben:*
	// label, and an assignment event to anchor the cycle. Written through Claim so
	// the event is the adapter's own, not a hand-authored one.
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Fatalf("the fixture already carries %q; #15 is the window before any label", got)
	}

	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	h.WaitState("1", StateNeedsReview)
	if prepares := h.Workspaces.Prepares("1"); len(prepares) != 0 {
		t.Errorf("missing epoch dispatched prepares: %+v", prepares)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("the claim was released %d times; an epoch fault retains it for remediation", got)
	}
	if !hasMilestone(h.Tracker.Milestones("1"), core.MilestoneNeedsReview) {
		t.Errorf("no needs-review milestone for the missing epoch")
	}
}

// Fresh workspace storage under a standing assignment has lost the historical
// epoch/base authority. Run-marker absence proves only that the workspace is
// free now; it cannot recreate that authority, so the claim parks sticky.
func TestFreshWorkspaceStorageParksEpochFaulted(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	// Fresh storage: the workspace and its pin are gone, the tracker is not. The
	// marker goes with them — a lost workspace root loses the marker store too,
	// since it lives beside issues/ (workspace marker.go).
	h.Workspaces = fake.NewWorkspaces()

	if err := h.restart(harnessOpts{runGone: groupGone, verifier: nil}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateNeedsReview)
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("the claim was released %d times; epoch remediation requires deliberate unassignment", got)
	}
	if got := h.Runner.StartCount(); got != 1 {
		t.Errorf("started %d runs total, want only the pre-restart run", got)
	}
}

func TestEpochFaultedRecoveryParkCannotBeUnparkedIntoALaunch(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// No provider record: this is the loss-of-storage fault, not a clean pending
	// crash. Recovery must mark the park sticky under this assignment.
	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateNeedsReview)
	h.waitLabel("1", core.StateLabelNeedsReview)
	starts, prepares := h.Runner.StartCount(), h.Workspaces.PrepareCount("1")

	// The ordinary human re-queue gesture. For an epoch fault it restores
	// budgets but cannot manufacture the missing pair, so BEN re-projects the
	// park without traversing backoff.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Labels = []string{"ben-queue"}
	})
	h.Tick()
	h.waitLabel("1", core.StateLabelNeedsReview)
	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Fatalf("state after epoch-fault label removal = %s, want needs-review", got)
	}
	if got := h.Runner.StartCount(); got != starts {
		t.Errorf("started %d new runs after sticky unpark", got-starts)
	}
	if got := h.Workspaces.PrepareCount("1"); got != prepares {
		t.Errorf("prepared %d workspaces after sticky unpark", got-prepares)
	}
	if path := h.o.Transitions.Path("1"); containsState(path, StateBackoff) {
		t.Errorf("path = %v; epoch-faulted park traversed the dispatching backoff edge", path)
	}
}

func TestEpochFaultedPendingIsAbandonedBeforeANewAssignment(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// The tracker names the current assignment, while the provider retains an
	// abandoned earlier transition. Recovery must park this assignment, then
	// roll the stale pending state back only after the human removes the claim.
	h.Workspaces.SetClaimBase("1", core.ClaimBase{State: core.ClaimBasePending, Epoch: 999})
	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateNeedsReview)
	h.waitLabel("1", core.StateLabelNeedsReview)

	h.Tracker.UnassignBy("1", fake.DefaultPrincipal)
	h.tickUntil("the lost claim to abandon its pending epoch", func() bool {
		return h.stateOf("1") == ""
	})
	if got, err := h.Workspaces.ClaimBase(t.Context(), issue); err != nil || got.State != core.ClaimBaseAbsent {
		t.Fatalf("claim base after unassignment = %+v, %v; want abandoned pending to be absent", got, err)
	}

	// Clearing the old park makes the still-queued issue dispatchable. The next
	// BEN assignment must now be able to establish and pin its own epoch.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Labels = []string{"ben-queue"}
		i.Dispatchable = true
	})
	h.tickUntil("the new assignment to prepare", func() bool {
		return h.Workspaces.PrepareCount("1") > 0
	})
	got, err := h.Workspaces.ClaimBase(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != core.ClaimBasePinned || got.Epoch <= 0 || got.Epoch == 999 {
		t.Errorf("new assignment claim base = %+v, want a newly pinned epoch", got)
	}
}

func TestPendingClaimEpochMarkerReadFailureRetriesRecovery(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	pendClaimBaseForRecovery(t, h, "1")
	markerErr := errors.New("run-marker store unavailable")
	h.Workspaces.SetFailMarkerRead(markerErr)
	var verified int
	verifier := verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
		verified++
		return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/1"}, nil
	})

	if err := h.restart(harnessOpts{runGone: groupGone, verifier: verifier}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := h.stateOf("1"); got != StateQueued {
		t.Fatalf("state with unreadable pending marker = %s, want inert queued retention", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Errorf("label with unreadable pending marker = %q, want no inferred park", got)
	}
	if verified != 0 || h.Workspaces.PrepareCount("1") != 0 {
		t.Fatalf("unreadable marker called verifier %d times and prepared %d workspaces", verified, h.Workspaces.PrepareCount("1"))
	}

	h.Workspaces.SetFailMarkerRead(nil)
	h.PollNow()
	h.WaitState("1", StateDone)
	if verified != 1 {
		t.Errorf("verifier calls after marker recovery = %d, want 1", verified)
	}
}

func TestPendingClaimEpochWithRunEvidenceParksBeforeProbeOrVerifier(t *testing.T) {
	for _, tc := range []struct {
		name       string
		runMarker  core.RunMarker
		runningLog bool
	}{
		{
			name:      "any marker entry",
			runMarker: core.RunMarker{State: core.RunMarkerIdentified, Evidence: core.RunEvidence{Scheme: "test", ID: "old-run"}},
		},
		{name: "a current-cycle running event", runningLog: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, harnessOpts{runGone: groupGone})
			issue := fake.Issue("1", epoch)
			h.Tracker.Set(issue)
			if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			pendClaimBaseForRecovery(t, h, "1")
			if tc.runningLog {
				if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelRunning); err != nil {
					t.Fatalf("SetStateLabels: %v", err)
				}
			}
			if tc.runMarker.State != core.RunMarkerUnreadable {
				h.Workspaces.SetRunMarker("1", tc.runMarker)
			}

			var verified int
			verifier := verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
				verified++
				return VerifyResult{Verdict: VerdictPublished, PRURL: "https://example.test/pull/old"}, nil
			})
			if err := h.restart(harnessOpts{runGone: groupGone, verifier: verifier}); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			h.WaitState("1", StateNeedsReview)
			if verified != 0 {
				t.Errorf("verifier called %d times for contradictory pending state", verified)
			}
			if got := h.Workspaces.PrepareCount("1"); got != 0 {
				t.Errorf("prepared %d times from contradictory pending state", got)
			}
			if tc.runMarker.State == core.RunMarkerIdentified {
				if _, ok := h.Workspaces.RunMarkerFor("1"); !ok {
					t.Error("identified marker was probed and cleared before the pending contradiction was classified")
				}
			}
		})
	}
}

// A marker present without evidence has no answer coming: the launch outcome is
// unknown, and waiting cannot end a question nobody will answer. Parks, with the
// claim and workspace retained.
func TestAnUnknownLaunchParksRatherThanWaiting(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelRunning); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	pinClaimBaseForRecovery(t, h, "1")
	// BeginRun landed; the evidence upgrade never did. Indistinguishable from a
	// crash before the launch and from an interrupted cleanup of one that failed —
	// and one of the three has a live run in it.
	h.Workspaces.SetRunMarker("1", core.RunMarker{State: core.RunMarkerUnknownLaunch})

	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateNeedsReview)
	if got := h.Tracker.Label("1"); got != core.StateLabelNeedsReview {
		t.Errorf("label = %q, want ben:needs-review", got)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times; an unknown launch retains the claim", got)
	}
	if !hasMilestone(h.Tracker.Milestones("1"), core.MilestoneNeedsReview) {
		t.Error("no needs-review milestone; gate and table verdicts alike owe their comment")
	}
}

// A `done` issue awaiting merge must not be resurrected: the labels are already
// clear, the evidence is complete, and the claim is held for the sweep to
// release on the close.
func TestADoneIssueAwaitingMergeIsNotResurrected(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		runGone: groupGone,
	})
	h.WaitState("1", StateDone)
	// The claim converts to a held record and waits for the merge.
	deadline := time.Now().Add(2 * time.Second)
	for h.o.HeldCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.o.HeldCount() == 0 {
		t.Fatal("the finished run never converted to a held claim; the fixture is not at the kill point")
	}
	startsBefore := h.Runner.StartCount()

	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Held again, not re-run. The claim is retained and §9.8's sweep releases it
	// on the close, within one poll interval and without a restart.
	deadline = time.Now().Add(2 * time.Second)
	for h.o.HeldCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.o.HeldCount() != 1 {
		t.Errorf("held count = %d, want 1: a published issue is adopted as a held claim", h.o.HeldCount())
	}
	if got := h.Runner.StartCount(); got != startsBefore {
		t.Errorf("started %d new runs; a done issue awaiting merge is not resurrected", got-startsBefore)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times; the claim is retained until the close", got)
	}
	// Idempotent per occurrence, which is what makes §9.10 step 4's "every recovery
	// verdict re-issues its milestone comment" a repair rather than spam. Counted,
	// not merely present: presence passes whether the tracker collapsed the re-issue
	// or appended a second copy, and only one of those is the acceptance criterion.
	if got := milestoneCount(h.Tracker.CommentsFor("1"), core.MilestonePublished); got != 1 {
		t.Errorf("published milestone posted %d times across the restart, want exactly 1 "+
			"(comments: %+v)", got, h.Tracker.CommentsFor("1"))
	}
}

// Recovering the same world twice adds nothing the first pass did not.
//
// The stronger half of the exactly-once criterion: not "a comment survived" but
// "recovery is safe to repeat", which is what a daemon that restarts twice in a row
// — a crash loop, a supervisor retry — actually does. Driven over the three verdicts
// that owe a comment.
func TestRecoveringTwiceIssuesEachMilestoneOnce(t *testing.T) {
	for _, tc := range []struct {
		name  string
		label core.StateLabel
		want  core.Milestone
	}{
		{name: "an orphan re-issues claimed", label: core.StateLabelRunning, want: core.MilestoneClaimed},
		{name: "a park re-issues needs-review", label: core.StateLabelNeedsReview, want: core.MilestoneNeedsReview},
		{name: "a failure re-issues failed", label: core.StateLabelFailed, want: core.MilestoneFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, harnessOpts{runGone: groupGone})
			issue := fake.Issue("1", epoch)
			h.Tracker.Set(issue)
			if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
				t.Fatalf("Claim: %v", err)
			}
			if err := h.Tracker.SetStateLabels(context.Background(), issue, tc.label); err != nil {
				t.Fatalf("SetStateLabels: %v", err)
			}
			pinClaimBaseForRecovery(t, h, "1")

			opts := harnessOpts{
				runGone:  groupGone,
				verifier: incompleteEvidence,
				failures: stubFailures{reason: core.FailureStalled, ok: true},
			}
			for pass := 1; pass <= 2; pass++ {
				if err := h.restart(opts); err != nil {
					t.Fatalf("Recover (pass %d): %v", pass, err)
				}
				h.waitComment("1", tc.want)
			}

			if got := milestoneCount(h.Tracker.CommentsFor("1"), tc.want); got != 1 {
				t.Errorf("%q posted %d times over two recovery passes, want exactly 1 (comments: %+v)",
					tc.want, got, h.Tracker.CommentsFor("1"))
			}
		})
	}
}

// Gate 1 resolves on the *event*, not on publish evidence. A close a reopen has
// undone is invisible to current state, and reading it through the projection
// table would park an issue whose reopen is the evidence against it.
func TestGate1ReleasesOnACloseInsideTheCycle(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	// A human merged the PR and reopened the issue while the daemon was down. Both
	// events land in the log because the fake records them there (fake Mutate).
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "open" })

	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	h.WaitGone("1")
	if got := h.Tracker.ReleaseCount("1"); got != 1 {
		t.Errorf("released %d times, want 1: gate 1 releases on the close event", got)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want one disposal that does not keep the workspace", got)
	}
}

// A read that failed is not an absence. The candidate is retained, held out of
// dispatch, and reclassified on a later tick — never parked on a 502 and never
// dropped, because a candidate with no record is dispatchable.
func TestAFailedHistoryReadRetainsRatherThanClassifies(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	boom := errors.New("tracker unavailable")
	h.Tracker.SetFailHistory(boom)
	startsBefore := h.Runner.StartCount()
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Not parked: the log did not speak, so there is nothing to park on.
	if got := h.Tracker.Label("1"); got != core.StateLabelRunning {
		t.Errorf("label = %q, want ben:running left exactly as it was — a failed read projects nothing", got)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times; a claim we could not read is still ours", got)
	}
	// Not dropped: a record holds the issue, so dispatch cannot claim it again.
	if _, tracked := h.o.records["1"]; !tracked {
		t.Fatal("the candidate has no record; one with no record is dispatchable, which is a second agent")
	}
	if got := h.Runner.StartCount(); got != startsBefore {
		t.Errorf("started %d runs after the restart; an unclassified candidate dispatches nothing",
			got-startsBefore)
	}

	// And it is retried, rather than held unworked for the life of the process.
	h.Tracker.SetFailHistory(nil)
	h.Tick()
	h.WaitState("1", StateBackoff)
}

// §6.4: a candidate read that fails at startup is a warning, not a fatal error —
// and it must not silently look like "this principal holds nothing".
func TestAFailedCandidateReadWarnsAndContinues(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	boom := errors.New("tracker unavailable")
	h.Tracker.SetFailClaimedByPrincipal(boom)
	err := h.restart(harnessOpts{runGone: groupGone, recoverErr: true})
	if !errors.Is(err, boom) {
		t.Fatalf("Recover returned %v, want the candidate read's error — §6.4's soft failure has to be visible", err)
	}

	// Nothing was classified, nothing was guessed, and the claim is untouched.
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times on an unread candidate set", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Errorf("label = %q; an unread candidate set projects nothing", got)
	}
}

// Test helpers.

// incompleteEvidence is a run that committed but never published — the orphan's
// evidence, and what makes §9.10's "evidence incomplete" rows reachable.
var incompleteEvidence = verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
	return VerifyResult{Verdict: VerdictIncomplete, Detail: "commits exist; nothing was pushed"}, nil
})

// stubFailures is the §9.10 step 6 reader, stubbed so a test can state which of
// the two indistinguishable cases it is in.
type stubFailures struct {
	reason core.FailureReason
	detail string
	ok     bool
	err    error
}

func (s stubFailures) LastFailure(string) (core.RunFailure, bool, error) {
	if !s.ok {
		return core.RunFailure{}, false, s.err
	}
	// Dated strictly *after* the claim-establishing assignment, which the fake dates
	// at the issue's creation at the earliest (Tracker.install seeds the filing
	// labels at CreatedAt, and appendEvent will not stamp earlier than the last
	// event). §9.10 step 2 wants evidence dated after that event, and equality does
	// not order anything — so a fixture that shared the instant would be asserting
	// the degraded path by accident.
	return core.RunFailure{At: epoch.Add(time.Hour), Reason: s.reason, Detail: s.detail}, true, s.err
}

func (h *harness) statusFor(identifier string) Snapshot {
	h.t.Helper()
	for _, s := range h.o.Status() {
		if s.Identifier == identifier {
			return s
		}
	}
	h.t.Fatalf("issue %s is not tracked", identifier)
	return Snapshot{}
}

// waitLabel blocks until an issue carries a state label, which is the
// acknowledgement that the projection write landed — as opposed to WaitEffects,
// which counts writes of any kind and is satisfied by whichever happens to be
// first.
func (h *harness) waitLabel(identifier string, want core.StateLabel) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got core.StateLabel
	for time.Now().Before(deadline) {
		if got = h.Tracker.Label(identifier); got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("issue %s carries label %q, want %q (path: %v)",
		identifier, got, want, h.o.Transitions.Path(identifier))
}

// waitReleased blocks until an issue's claim has been dropped n times.
func (h *harness) waitReleased(identifier string, n int) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.Tracker.ReleaseCount(identifier) >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("issue %s was released %d times, want %d (path: %v)",
		identifier, h.Tracker.ReleaseCount(identifier), n, h.o.Transitions.Path(identifier))
}

// waitComment blocks until a milestone has been posted for an issue, and returns
// it. The acknowledgement each comment assertion below actually wants: a write
// count cannot say *which* write landed, and every one of these fixtures makes
// several.
func (h *harness) waitComment(identifier string, want core.Milestone) core.MilestoneComment {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got, ok := lastComment(h.Tracker.CommentsFor(identifier), want); ok {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("issue %s never got a %q milestone (comments: %+v, path: %v)",
		identifier, want, h.Tracker.CommentsFor(identifier), h.o.Transitions.Path(identifier))
	return core.MilestoneComment{}
}

// waitDispatched blocks until recovery has prepared an attempt for the issue.
//
// The barrier the dispatch verdicts want, rather than a transient state: the
// default script succeeds, so a record can pass through `running` and reach `done`
// before an assertion looks. The prepare is the fact — an attempt exists — and it
// does not move once it has happened.
func (h *harness) waitDispatched(identifier string) []fake.PrepareCall {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := h.Workspaces.Prepares(identifier); len(got) > 0 {
			return got
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("recovery never dispatched issue %s (path: %v)", identifier, h.o.Transitions.Path(identifier))
	return nil
}

// milestoneCount reports how many times a milestone was actually posted, which is
// the assertion an idempotency claim needs — presence cannot tell a collapsed
// re-issue from a duplicate.
func milestoneCount(comments []core.MilestoneComment, want core.Milestone) int {
	n := 0
	for _, c := range comments {
		if c.Milestone == want {
			n++
		}
	}
	return n
}

func hasMilestone(got []core.Milestone, want core.Milestone) bool {
	for _, m := range got {
		if m == want {
			return true
		}
	}
	return false
}

func lastComment(comments []core.MilestoneComment, want core.Milestone) (core.MilestoneComment, bool) {
	for i := len(comments) - 1; i >= 0; i-- {
		if comments[i].Milestone == want {
			return comments[i], true
		}
	}
	return core.MilestoneComment{}, false
}

// Gate 2, and the mutual-release bug it exists to prevent. Two daemons recovering
// the same published issue — reachable when one crashed after assigning but
// before its read-back released it — must not both let go: an issue left
// unassigned and unlabelled is dispatchable, and the published work would be
// redone. Arbitration is what makes exactly one of them release.
func TestGate2ReleasesOnlyTheLoser(t *testing.T) {
	for _, tc := range []struct {
		name string
		// otherFirst says the co-assignee's standing assignment predates ours.
		otherFirst bool
		// wantProjected is the label recovery leaves. Gate 2's loser projects
		// nothing — it releases and stops — while the winner carries on into the
		// table and completes the projection its label events describe.
		wantProjected core.StateLabel
	}{
		{name: "another party assigned first: we release, and only our own", otherFirst: true},
		{
			name:          "we assigned first: we retain and keep classifying",
			wantProjected: core.StateLabelClaimed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, harnessOpts{runGone: groupGone})
			issue := fake.Issue("1", epoch)
			h.Tracker.Set(issue)

			// Both assignments go through the tracker, so the log that arbitration
			// replays is the one the writes produced.
			other := "other-daemon"
			assign := func(login string) {
				h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = append(i.Assignees, login) })
				h.Tracker.AppendHistory("1", core.ClaimEvent{
					Kind: core.ClaimEventAssigned, Actor: login, Subject: login,
				})
			}
			if tc.otherFirst {
				assign(other)
				assign(fake.DefaultPrincipal)
			} else {
				assign(fake.DefaultPrincipal)
				assign(other)
			}
			pinClaimBaseForRecovery(t, h, "1")

			if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
				t.Fatalf("Recover: %v", err)
			}

			// A barrier for each fact, not a count of writes: WaitEffects(1) is
			// satisfied by *any* one tracker call, and both branches here make several.
			//
			// What gate 2 itself did is read off the projection rather than off the
			// release count. A winner that classifies as active work *is* released
			// eventually — but by §9.8's unroutable rule on a later tick, once the
			// co-assignee is reconciled away, which is a different rule with a
			// different reason and would make a release assertion here ambiguous.
			if tc.wantProjected != "" {
				// The milestone rather than the label: recovery projects ben:claimed and
				// then *dispatches*, so the label moves to ben:running within the same
				// pass. A comment is a record and does not move.
				h.waitComment("1", core.MilestoneClaimed)
			} else {
				// The loser's terminal act is the release, and a record's writes are
				// FIFO — so once it has landed, a projection this verdict owed would
				// have landed before it. That is what makes the negative assertion
				// below barriered rather than timed.
				h.waitReleased("1", 1)
				if got := milestoneCount(h.Tracker.CommentsFor("1"), core.MilestoneClaimed); got != 0 {
					t.Errorf("gate 2's loser posted %d claimed milestones; it releases and stops", got)
				}
				if got := h.Tracker.Label("1"); got != core.StateLabelNone {
					t.Errorf("gate 2's loser projected %q; it releases and stops", got)
				}
			}
			// Our own assignment only, at every verdict and everywhere else. The
			// co-assignee's is never ours to remove.
			if !containsFold(h.issueAssignees("1"), other) {
				t.Errorf("the co-assignee's assignment was removed: %v", h.issueAssignees("1"))
			}
		})
	}
}

// Gate 3: the issue is assigned to the principal, but the returned log carries no
// standing `assigned` event for it — event retention, or a transfer. The log
// *spoke*, and it could not account for our own claim, so recovery refuses to
// guess: retain the claim, park, and raise a loud operator error.
//
// The mutation this rejects is reading a failed ClaimHistory read the same way. A
// 502 read as "no assignment event" parks a healthy published claim; that is
// TestAFailedHistoryReadRetainsRatherThanClassifies, and the two together are the
// absence-vs-failure-to-ask rule.
func TestGate3ParksAClaimTheLogCannotAccountFor(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	issue.Assignees = []string{fake.DefaultPrincipal}
	h.Tracker.Set(issue)
	// Assigned, and the log shows nothing that established it.
	h.Tracker.SetHistory("1")

	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateNeedsReview)

	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times; gate 3 retains the claim and refuses to guess", got)
	}
	if got := h.Tracker.Label("1"); got != core.StateLabelNeedsReview {
		t.Errorf("label = %q, want ben:needs-review", got)
	}
	if !hasMilestone(h.Tracker.Milestones("1"), core.MilestoneNeedsReview) {
		t.Error("no needs-review milestone; gate 3 owes its comment like every other verdict")
	}
}

// Classification reads the *ordered label events*, never the current label set.
// An interrupted add-before-remove leaves two `ben:*` labels standing, and a
// set-based reading matches two table rows at once — returning contradictory
// verdicts for one issue. The most recently labeled one is effective.
func TestAnInterruptedProjectionClassifiesFromTheOrderedEvents(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelClaimed); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	// Killed between the add and the remove: ben:needs-review is on, ben:claimed
	// is still standing beside it.
	h.Tracker.InterruptStateLabels("1", core.StateLabelNeedsReview)
	pinClaimBaseForRecovery(t, h, "1")

	got, err := h.Tracker.Get(context.Background(), "1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !containsFold(got.Labels, "ben:claimed") || !containsFold(got.Labels, "ben:needs-review") {
		t.Fatalf("labels = %v; the fixture must leave both standing, which is what add-before-remove produces",
			got.Labels)
	}

	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// needs-review, because it was labeled last — not `claimed`, and not both.
	h.WaitState("1", StateNeedsReview)
	h.waitLabel("1", core.StateLabelNeedsReview)
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times; a parked issue retains its claim", got)
	}
}

// The published milestone carries its pull request link, because the adapter
// refuses one without it (github renderMilestone) — and a refused write is owed
// forever, with the disposal and the held-claim conversion queued behind it. A
// verdict alone cannot supply it, which is why the whole VerifyResult travels.
func TestARecoveredPublishCommentCarriesItsPullRequest(t *testing.T) {
	const prURL = "https://example.test/pull/99"
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// The interrupted `done`: ben:running standing, evidence complete. Recovery
	// finishes it, which means clearing the label and posting the publish comment.
	if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelRunning); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	// A base pin has to stand for the evidence read to be reached at all.
	pinClaimBaseForRecovery(t, h, "1")

	published := verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
		return VerifyResult{Verdict: VerdictPublished, PRURL: prURL}, nil
	})
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: published}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := h.waitComment("1", core.MilestonePublished)
	if got.PRURL != prURL {
		t.Errorf("PRURL = %q, want %q — the adapter refuses a published comment with no link, forever", got.PRURL, prURL)
	}

	// And the held-claim record carries it, which is what §9.8 releases against.
	deadline := time.Now().Add(2 * time.Second)
	for h.o.HeldCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.o.HeldCount() != 1 {
		t.Errorf("held count = %d, want 1: a published issue is adopted as a held claim", h.o.HeldCount())
	}
}

// A checker that answers published with no link has broken its own contract, and
// recovery refuses it rather than converting a detectable bug into a comment the
// tracker rejects forever.
func TestPublishedWithNoLinkIsRefusedRatherThanPosted(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelRunning); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	pinClaimBaseForRecovery(t, h, "1")

	linkless := verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
		return VerifyResult{Verdict: VerdictPublished}, nil
	})
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: linkless}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if _, posted := lastComment(h.Tracker.CommentsFor("1"), core.MilestonePublished); posted {
		t.Error("a published comment was posted with no pull request link")
	}
	// Retained unclassified, like every other read that could not be trusted.
	if _, tracked := h.o.records["1"]; !tracked {
		t.Fatal("the candidate was dropped; one with no record is dispatchable")
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times on evidence nobody could use", got)
	}
}

// §6.4's warn-and-continue must not become "and never look again". §8.3 excludes
// assigned issues from the ordinary Fetch, so if the startup scan fails and is not
// redone, every claim this principal holds sits unaccounted for until somebody
// restarts the daemon.
func TestAFailedCandidateScanIsRetriedOnLaterTicks(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	pendClaimBaseForRecovery(t, h, "1")

	boom := errors.New("tracker unavailable")
	h.Tracker.SetFailClaimedByPrincipal(boom)
	if err := h.restart(harnessOpts{runGone: groupGone, recoverErr: true}); !errors.Is(err, boom) {
		t.Fatalf("Recover returned %v, want the candidate read's error", err)
	}
	if _, tracked := h.o.records["1"]; tracked {
		t.Fatal("a scan that failed produced a record; an unread candidate set is not a candidate")
	}

	// Still failing: the scan is retried and still reaches no answer.
	h.Tick()
	if got := h.Tracker.ClaimedReads(); got < 2 {
		t.Errorf("claimed reads = %d, want the scan retried on the tick", got)
	}

	// The tracker comes back, and the claim is picked up without a restart.
	h.Tracker.SetFailClaimedByPrincipal(nil)
	h.Tick()
	h.waitDispatched("1")
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times; the recovered claim is worked, not dropped", got)
	}
}

// A retry re-reads the issue rather than re-deciding from the copy the failed pass
// saw. Hours can pass while a fault persists, and every gate classifies from
// current state.
func TestARetriedClassificationRefetchesTheIssue(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.SetFailHistory(errors.New("tracker unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, tracked := h.o.records["1"]; !tracked {
		t.Fatal("the unclassified candidate has no record")
	}

	// While the fault stands, a human closes the issue. Gate 1 must see that on the
	// retry — a re-decision from the stale copy would read it as still open.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })
	h.Tracker.SetFailHistory(nil)
	h.Tick()

	h.WaitGone("1")
	if got := h.Tracker.ReleaseCount("1"); got != 1 {
		t.Errorf("released %d times, want 1: the retry must classify from the issue as it is now", got)
	}
}

// An issue deleted or transferred while the fault stood has no claim of ours left
// to release. The record goes rather than being retried forever.
func TestARetriedCandidateThatVanishedIsDropped(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.SetFailHistory(errors.New("tracker unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.Tracker.Delete("1")
	h.Tracker.SetFailHistory(nil)
	h.Tick()

	h.WaitGone("1")
	if got := h.Tracker.ReleaseAttempts("1"); got != 0 {
		t.Errorf("attempted %d releases against an issue that is gone", got)
	}
}

// An unknown launch is an unconditional park, so the reads that cannot change it
// are not spent — and, more importantly, a transient failure in one of them must
// not defer the needs-review projection §9.10 *requires* for the one state that
// has no answer coming.
func TestAnUnknownLaunchParksEvenWhenEvidenceIsUnreadable(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelRunning); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	pinClaimBaseForRecovery(t, h, "1")
	h.Workspaces.SetRunMarker("1", core.RunMarker{State: core.RunMarkerUnknownLaunch})

	unreadable := verifierFunc(func(context.Context, core.Issue, core.Workspace) (VerifyResult, error) {
		return VerifyResult{}, errors.New("origin unreachable")
	})
	// And the failure-reason read is unavailable too, for the same reason: neither
	// is consulted by a verdict that is already settled.
	if err := h.restart(harnessOpts{
		runGone:  groupGone,
		verifier: unreadable,
		failures: stubFailures{err: errors.New("state file is corrupt")},
	}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	h.WaitState("1", StateNeedsReview)
	if got := h.Tracker.Label("1"); got != core.StateLabelNeedsReview {
		t.Errorf("label = %q, want ben:needs-review — the park must not wait on reads it does not need", got)
	}
	if got := h.Tracker.ReleaseCount("1"); got != 0 {
		t.Errorf("released %d times; an unknown launch retains the claim and the workspace", got)
	}
}

// SPEC §9.10 step 5's disposal is gate 1's, and it must fire exactly once.
//
// It replaces a sweep keyed on "no record owns this workspace", which was wrong in
// the direction that loses work: §6.4 keeps a failure's workspace, and a `failed`
// or gate-4 verdict releases the claim, so unowned describes a kept failure exactly
// as well as a merged issue's residue. See the note above recoverCandidate.
func TestGate1DisposesTheWorkspaceExactlyOnce(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)
	// A human merged the PR and closed the issue while the daemon was down.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.State = "closed" })

	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitGone("1")

	got := h.Workspaces.Disposals("1")
	if len(got) != 1 {
		t.Fatalf("disposals = %+v, want exactly one", got)
	}
	if got[0].Keep {
		t.Error("gate 1 kept the workspace; a terminal issue's worktree is disposed (SPEC §6.4)")
	}
}

// A parked record must not be unparked by *its own* un-landed label write.
//
// Reconciliation reads "no ben:* label stands" on a needs-review record as a human
// re-queue (SPEC §9.2, §9.8). That inference is only sound once the tracker
// carries what this daemon last decided, and recovery's park is where it is not:
// gate 3 projects ben:needs-review onto an issue that carries no state label at
// all, so until the write lands the tracker looks exactly like a re-queue.
//
// Found as a 1-in-8 flake in TestGate3ParksAClaimTheLogCannotAccountFor. Driven
// deterministically here by holding the label write open across the tick that
// reconciles, because a timing-dependent version of this test is how the bug got
// in: the window is one tracker round trip wide and closes on its own.
func TestARecoveredParkIsNotUnparkedByItsOwnPendingProjection(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	issue.Assignees = []string{fake.DefaultPrincipal}
	h.Tracker.Set(issue)
	// Gate 3: assigned, and the log shows nothing that established it. The park it
	// earns has to reach a human, so it must survive this daemon's own lag.
	h.Tracker.SetHistory("1")

	// The label write blocks until the test lets it through, so the tick below
	// reconciles while the projection is provably still owed.
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	h.Tracker.SetLabelGate(func() { <-release })

	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateNeedsReview)
	if got := h.Tracker.Label("1"); got != core.StateLabelNone {
		t.Fatalf("the label write already landed (%q); this fixture needs it held open", got)
	}

	// Reconcile with the projection still in flight. The record is parked, the
	// tracker shows no ben:* label, and the two facts must not be read together.
	h.Tick()
	if got := h.stateOf("1"); got != StateNeedsReview {
		t.Errorf("state = %q, want needs-review: the daemon unparked an issue on its own un-landed write "+
			"(path: %v)", got, h.o.Transitions.Path("1"))
	}

	// Once it lands, the park stands on the tracker too — and a *real* re-queue is
	// still honoured afterwards, so the guard has not disabled the mechanism.
	unblock()
	h.waitLabel("1", core.StateLabelNeedsReview)
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Labels = []string{"ben-queue"} })
	h.Tick()
	h.WaitState("1", StateBackoff)
}

// A retried candidate whose claim is no longer ours is dropped, not parked.
//
// The gates cannot catch this: an unassigned issue reaches gate 2 with no contender
// the log can order, which is the unorderable fallback — so BEN would project
// `ben:needs-review` onto an issue nobody holds and strand it, having been told
// plainly that it is not ours.
func TestARetriedCandidateThatLostOurClaimIsDropped(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	h.Tracker.SetFailHistory(errors.New("tracker unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if _, tracked := h.o.records["1"]; !tracked {
		t.Fatal("the unclassified candidate has no record")
	}

	// A human takes the issue while the fault stands.
	h.Tracker.Mutate("1", func(i *core.Issue) { i.Assignees = []string{"a-human"} })
	h.Tracker.SetFailHistory(nil)
	h.Tick()

	h.WaitGone("1")
	if got := h.Tracker.Label("1"); got == core.StateLabelNeedsReview {
		t.Error("parked an issue this daemon does not hold; §9.10's verdicts are all about *our* claim")
	}
	if got := h.Tracker.ReleaseAttempts("1"); got != 0 {
		t.Errorf("attempted %d releases against a claim that is not ours", got)
	}
	// And the human's assignment is untouched, at this verdict as at every other.
	if !containsFold(h.issueAssignees("1"), "a-human") {
		t.Errorf("the human's assignment was removed: %v", h.issueAssignees("1"))
	}
}

// A reload adopted while a recovery read is in flight discards the result.
//
// The candidate read is "every issue *this principal* holds", and an identity reload
// rebinds the principal and the repository together (Bundle.Identity). Salvaging the
// answer would classify one repository's issue identifiers against another's — the
// same number naming a different issue — and project labels and releases onto
// whatever happens to share it.
func TestARecoveryScanOvertakenByAReloadIsDiscarded(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	pendClaimBaseForRecovery(t, h, "1")

	// The startup scan fails, so a retry is owed.
	h.Tracker.SetFailClaimedByPrincipal(errors.New("tracker unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone, recoverErr: true}); err == nil {
		t.Fatal("the startup scan was supposed to fail, leaving a retry owed")
	}
	h.Tracker.SetFailClaimedByPrincipal(nil)

	// Hold the retried scan open, publish a reload while it is out, then let it land.
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	h.Tracker.SetClaimedGate(func() { <-release })

	h.Tick()
	waitFor(t, "the retried scan to be in flight", func() bool { return h.Tracker.ClaimedReads() >= 2 })
	// A new revision. The bundle is rebuilt over the same adapters — a fresh pointer
	// is what publish treats as a move, and it is what a reload produces even when
	// the adapters behind it are equivalent.
	reloaded := *h.Bundle
	h.Source.publish(h.def, &reloaded, nil)
	unblock()

	// The result is dropped, so nothing is adopted from it — and the scan stays owed,
	// so a later tick asks the configuration now in force and the claim is worked.
	h.Tracker.SetClaimedGate(nil)
	waitFor(t, "the superseded scan to be discarded", func() bool {
		return len(h.Logs.find("discarding a recovery scan")) > 0
	})
	h.tickUntil("the claim is picked up under the new revision", func() bool {
		return len(h.Workspaces.Prepares("1")) > 0
	})
}

// A possibly-live workspace is reported as a warning, naming both the issue and
// the workspace — on the first pass and on every tick that asks again.
//
// The whole observable effect of this verdict *is* the log line: the claim is
// retained, nothing is dispatched, nothing is projected, and §9.10 converges on its
// own only if the run really does end. A run that never ends waits forever, so an
// operator has to be able to see it — and to act, they need the worktree to inspect
// and the group to check, neither of which is derivable from an issue number.
func TestAPossiblyLiveWorkspaceIsWarnedAboutByName(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)
	if err := h.restart(harnessOpts{runGone: groupAlive, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	assertPossiblyLiveWarning := func(when string) {
		t.Helper()
		got := h.Logs.find("may still be live")
		if len(got) == 0 {
			t.Fatalf("%s: nothing was logged about a possibly-live workspace", when)
		}
		last := got[len(got)-1]
		if last.Level != slog.LevelWarn {
			t.Errorf("%s: logged at %v, want WARN — a retained claim with an agent nobody can account for "+
				"is not an informational fact about a queue", when, last.Level)
		}
		if last.Attrs["issue"] != "1" {
			t.Errorf("%s: issue attr = %q, want \"1\"", when, last.Attrs["issue"])
		}
		if ws := last.Attrs["workspace"]; ws == "" || ws == "(no workspace resolved)" {
			t.Errorf("%s: workspace attr = %q; an operator cannot inspect a worktree they were not told about", when, ws)
		}
		if last.Attrs["run"] == "" {
			t.Errorf("%s: no run attr; the recorded group is what `ps` is pointed at", when)
		}
	}
	assertPossiblyLiveWarning("on the first pass")

	// And again on the retry, because the wait is unbounded: a line that appeared
	// once at startup and never again describes a daemon that looks idle.
	before := len(h.Logs.find("may still be live"))
	h.Tick()
	waitFor(t, "the possibly-live warning to repeat", func() bool {
		return len(h.Logs.find("may still be live")) > before
	})
	assertPossiblyLiveWarning("on the retry")
}

// The two capability absences §9.10 depends on are named at startup, because both
// produce behaviour indistinguishable from a legitimate degradation.
func TestMissingRecoveryCapabilitiesAreNamedAtStartup(t *testing.T) {
	h := start(t, harnessOpts{}) // no runGone, no failures reader

	for _, want := range []struct {
		substr string
		why    string
	}{
		{"no run prober", "an identified marker can never be confirmed free, so such issues are never resumed"},
		{"no transition-log reader", "every recovered failure reports that its reason did not survive"},
	} {
		got := h.Logs.find(want.substr)
		if len(got) == 0 {
			t.Errorf("nothing named the missing capability %q: %s", want.substr, want.why)
			continue
		}
		if got[0].Level != slog.LevelWarn {
			t.Errorf("%q logged at %v, want WARN", want.substr, got[0].Level)
		}
	}
}

// An unknown launch is warned about by name too: it parks for a human, and the
// human's first question is which workspace to look in.
func TestAnUnknownLaunchIsWarnedAboutByName(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := h.Tracker.SetStateLabels(context.Background(), issue, core.StateLabelRunning); err != nil {
		t.Fatalf("SetStateLabels: %v", err)
	}
	pinClaimBaseForRecovery(t, h, "1")
	h.Workspaces.SetRunMarker("1", core.RunMarker{State: core.RunMarkerUnknownLaunch})

	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.WaitState("1", StateNeedsReview)

	got := h.Logs.find("launch outcome is unknown")
	if len(got) == 0 {
		t.Fatal("nothing was logged about an unknown launch outcome")
	}
	if got[0].Level != slog.LevelWarn {
		t.Errorf("logged at %v, want WARN — this state has no answer coming and reaches a human", got[0].Level)
	}
	if got[0].Attrs["issue"] != "1" || got[0].Attrs["workspace"] == "" {
		t.Errorf("attrs = %v, want the issue and the workspace named", got[0].Attrs)
	}
}

// A per-record classification that a reload overtook is discarded, for the reason
// the scan's is: the principal and the repository move together, so a verdict
// reached under the old identity is about a different issue that happens to share
// an identifier.
//
// The two configurations are made to *disagree*, because otherwise nothing
// discriminates: a discarded verdict and an applied one both end at the same state
// when both were computed from the same world, so the test would pass with the
// guard removed. Here the reload takes the issue out of the workflow's label
// partition, so the old configuration says "orphan, project ben:claimed and re-enter
// backoff" and the new one says "gate 4, release and keep the workspace". The
// projection is the tell: only the superseded verdict has one.
func TestARetriedClassificationOvertakenByAReloadIsDiscarded(t *testing.T) {
	h := start(t, harnessOpts{
		issues:  []core.Issue{fake.Issue("1", epoch)},
		script:  startedOnly,
		hang:    true,
		runGone: groupGone,
	})
	h.WaitState("1", StateRunning)

	// Leave the candidate unclassified, so the tick retries it.
	h.Tracker.SetFailHistory(errors.New("tracker unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone, verifier: incompleteEvidence}); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	h.Tracker.SetFailHistory(nil)
	claimedBefore := milestoneCount(h.Tracker.CommentsFor("1"), core.MilestoneClaimed)

	// Hold the retry's history read open, publish a reload while it is out, and let
	// it land. The read succeeds, so a verdict *is* reached — and must be dropped.
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	h.Tracker.SetHistoryGate(func() { <-release })

	reads := h.Tracker.HistoryReads()
	h.Tick()
	waitFor(t, "the retried classification to be in flight", func() bool {
		return h.Tracker.HistoryReads() > reads
	})

	// The reload: same adapters, a partition the issue is no longer in.
	body := strings.ReplaceAll(workflowTemplate, "%CONCURRENCY%", "3")
	body = strings.ReplaceAll(body, "%TURNS%", "4")
	body = strings.ReplaceAll(body, "%EXTRA%", "")
	body = strings.ReplaceAll(body, `required_labels: ["ben-queue"]`, `required_labels: ["moved-elsewhere"]`)
	moved := loadDefinition(t, body)
	reloaded := *h.Bundle
	reloaded.Definition = moved
	h.def = moved
	h.Source.publish(moved, &reloaded, nil)
	unblock()
	h.Tracker.SetHistoryGate(nil)

	waitFor(t, "the superseded classification to be discarded", func() bool {
		return len(h.Logs.find("discarding a recovery classification")) > 0
	})

	// The orphan verdict the old configuration reached is never applied. Its
	// signature is the claimed projection and its milestone — gate 4, which is what
	// the configuration in force says, projects nothing at all.
	h.tickUntil("the claim is released under the new partition", func() bool {
		return h.Tracker.ReleaseCount("1") >= 1
	})
	if got := milestoneCount(h.Tracker.CommentsFor("1"), core.MilestoneClaimed); got != claimedBefore {
		t.Errorf("a claimed milestone was posted (%d, was %d): that is the superseded verdict's, reached "+
			"under a label partition this issue had already left", got, claimedBefore)
	}
	if got := h.Workspaces.Disposals("1"); len(got) != 0 {
		t.Errorf("disposals = %+v; gate 4 releases and *keeps* the workspace (SPEC §9.10)", got)
	}
}

// A retried classification adopts the issue it was *reached from*, not the copy the
// failed pass left on the record.
//
// They differ by however long the fault lasted, and the difference is not cosmetic:
// a §9.10 dispatch renders the prompt from Issue.Title and Issue.Body (SPEC §5.6),
// so adopting the stale one launches an agent against a description a human has
// since rewritten.
func TestARetriedClassificationAdoptsTheFreshIssue(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	issue.Title = "the old title"
	issue.Body = "the old body"
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	pendClaimBaseForRecovery(t, h, "1")

	// Unclassifiable at first: #15's shape, so the eventual verdict is a dispatch and
	// the prompt is rendered from whatever issue the verdict carried.
	h.Tracker.SetFailHistory(errors.New("tracker unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone}); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// A human rewrites the issue while the fault stands.
	h.Tracker.Mutate("1", func(i *core.Issue) {
		i.Title = "the rewritten title"
		i.Body = "the rewritten body"
	})
	h.Tracker.SetFailHistory(nil)
	h.tickUntil("the recovered claim is dispatched", func() bool {
		return h.Runner.StartCount() > 0
	})

	prompts := h.Runner.Prompts()
	if len(prompts) == 0 {
		t.Fatal("nothing was dispatched")
	}
	if strings.Contains(prompts[0], "the old title") {
		t.Errorf("the agent was launched against the stale issue:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[0], "the rewritten title") {
		t.Errorf("the prompt does not carry the issue as it is now:\n%s", prompts[0])
	}
}

// An unresolved candidate scan is outstanding work for identity purposes.
//
// An empty record set there means "nothing discovered", not "nothing held": the
// scan is what discovers claims. Adopting a new identity moves workspace.root out
// from under claims about to be classified, and the next pass reads the *new* root's
// absent marker as a free workspace while the old root's process is still running.
func TestAnIdentityReloadIsRefusedWhileTheRecoveryScanIsUnresolved(t *testing.T) {
	h := start(t, harnessOpts{runGone: groupGone})
	issue := fake.Issue("1", epoch)
	h.Tracker.Set(issue)
	if _, err := h.Tracker.Claim(context.Background(), issue); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	pendClaimBaseForRecovery(t, h, "1")

	h.Tracker.SetFailClaimedByPrincipal(errors.New("tracker unavailable"))
	if err := h.restart(harnessOpts{runGone: groupGone, recoverErr: true}); err == nil {
		t.Fatal("the startup scan was supposed to fail, leaving it owed")
	}
	if len(h.o.Status()) != 0 {
		t.Fatal("a scan that failed produced records; this test needs the set to be empty")
	}

	// An identity change: a different claim principal, which is what a rebuilt
	// tracker can produce (Bundle.Identity).
	moved := *h.Bundle
	moved.ClaimPrincipal = "someone-else"
	err := h.o.AdoptIdentity(context.Background(), h.Bundle, &moved, func() {})
	if !errors.Is(err, config.ErrWorkOutstanding) {
		t.Fatalf("AdoptIdentity = %v, want ErrWorkOutstanding: the daemon does not yet know what it holds", err)
	}

	// Once the scan resolves, the same publication is judged on what it actually
	// found — here a claim, so still refused, but for a reason the daemon can state.
	h.Tracker.SetFailClaimedByPrincipal(nil)
	h.tickUntil("the hidden claim to be picked up", func() bool {
		return len(h.Workspaces.Prepares("1")) > 0
	})
}
