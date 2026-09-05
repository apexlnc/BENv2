package orchestrator

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

const advancedHeadSHA = "2222222222222222222222222222222222222222"

// A remote provider has no local branch interval: the earlier publication is
// already the new epoch's trusted base. Its typed provider fact still renders a
// revision, while the baseline shift preserves this claim's full retry budget.
func TestRemotePriorWorkRaisesTheAttemptFloorWithoutLocalFacts(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		hang:   true,
		script: startedOnly,
		configureWorkspaces: func(w *fake.Workspaces) {
			w.SetPriorWork("1", true)
		},
	})
	h.WaitState("1", StateRunning)

	spec, ok := h.Runner.LastSpec()
	if !ok {
		t.Fatal("the provider-owned prior-work fact produced no RunSpec")
	}
	if got := spec.Env["BEN_ATTEMPT"]; got != "2" {
		t.Errorf("BEN_ATTEMPT = %q, want 2", got)
	}
	if !containsAll(spec.Prompt, "Attempt 2", "previous outcome unknown") {
		t.Errorf("remote revision prompt does not report attempt 2 with an unknown prior outcome:\n%s", spec.Prompt)
	}
}

// SPEC §9.6, §9.10 gate 4 (#94): removing the required label releases the
// claim and keeps the workspace. If the label later returns, the new record's
// first Prepare reattaches that workspace and derives only what git proves:
// work beyond the standing base raises the attempt floor, while a branch still
// at the base remains a numbered first attempt.
func TestRelabelledKeptBranchDerivesAttemptFloorFromGit(t *testing.T) {
	for _, tc := range []struct {
		name        string
		advanced    bool
		wantAttempt string
	}{
		{name: "advanced branch starts at two", advanced: true, wantAttempt: "2"},
		{name: "branch at base stays at one", wantAttempt: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reclaimed atomic.Bool
			h := start(t, harnessOpts{
				issues: []core.Issue{fake.Issue("1", epoch)},
				hang:   true,
				script: startedOnly,
				prepareFacts: func(ws core.Workspace) (core.LocalBranchFacts, error) {
					if reclaimed.Load() && tc.advanced {
						return core.LocalBranchFacts{Head: advancedHeadSHA, DescendsBase: true}, nil
					}
					return core.LocalBranchFacts{Head: ws.BaseSHA, DescendsBase: true}, nil
				},
			})
			h.WaitState("1", StateRunning)

			// The steady-state equivalent of recovery gate 4: the issue leaves
			// the label partition, so BEN stops the run, keeps the workspace and
			// releases its claim.
			h.Tracker.Mutate("1", func(i *core.Issue) {
				i.Labels = nil
				i.Dispatchable = false
			})
			h.PollNow()
			h.WaitGone("1")
			waitFor(t, "the gate-4-equivalent release", func() bool {
				return h.Tracker.ReleaseCount("1") == 1
			})
			if got := h.Workspaces.Disposals("1"); len(got) != 1 || !got[0].Keep {
				t.Fatalf("disposals = %+v, want the workspace kept across the claim boundary", got)
			}

			reclaimed.Store(true)
			h.Tracker.Mutate("1", func(i *core.Issue) {
				i.Labels = []string{"ben-queue"}
				i.Dispatchable = true
			})
			h.PollNow()
			waitFor(t, "the relabelled issue to launch", func() bool {
				return h.Runner.StartCount() == 2
			})

			spec, ok := h.Runner.LastSpec()
			if !ok {
				t.Fatal("the relabelled issue produced no RunSpec")
			}
			if got := spec.Env["BEN_ATTEMPT"]; got != tc.wantAttempt {
				t.Errorf("BEN_ATTEMPT = %q, want %q", got, tc.wantAttempt)
			}
			if tc.advanced {
				if !containsAll(spec.Prompt, "Attempt 2", "previous outcome unknown") {
					t.Errorf("evidence-floored prompt does not report attempt 2 with an unknown prior outcome:\n%s", spec.Prompt)
				}
			} else if containsAll(spec.Prompt, "Attempt", "previous outcome") {
				t.Errorf("a branch with no work rendered as a later attempt:\n%s", spec.Prompt)
			}
		})
	}
}

// The evidence floor changes presentation, not accounting. max_attempts=3
// still permits three failure-track dispatches in the fresh claim, numbered
// 2, 3 and 4 because the baseline moves with the floor.
func TestEvidenceFloorPreservesTheFreshFailureBudget(t *testing.T) {
	var evidenceReads atomic.Int32
	h := start(t, harnessOpts{
		issues:     []core.Issue{fake.Issue("1", epoch)},
		legacyBase: fake.DefaultBaseSHA,
		prepareFacts: func(core.Workspace) (core.LocalBranchFacts, error) {
			evidenceReads.Add(1)
			return core.LocalBranchFacts{Head: advancedHeadSHA, DescendsBase: true}, nil
		},
		script: func(core.RunSpec, int) []core.Event {
			return fake.Fail(core.FailureCrashed)
		},
	})
	waitFor(t, "the evidence-floored first dispatch", func() bool {
		return h.Runner.StartCount() == 1
	})

	for _, wantStarts := range []int{2, 3} {
		if !h.Clock.BlockUntilWaiters(2) {
			t.Fatal("the failure retry timer was never armed")
		}
		h.Clock.Advance(10 * time.Minute)
		waitFor(t, "the next failure-budget dispatch", func() bool {
			return h.Runner.StartCount() == wantStarts
		})
	}
	h.WaitGone("1")

	if got := h.Runner.StartCount(); got != 3 {
		t.Fatalf("started %d runs, want all 3 failure-budget dispatches", got)
	}
	if got := evidenceReads.Load(); got != 1 {
		t.Errorf("read first-Prepare evidence %d times, want once for the fresh claim", got)
	}
	prompts := h.Runner.Prompts()
	if len(prompts) != 3 || !containsAll(prompts[0], "Attempt 2") ||
		!containsAll(prompts[1], "Attempt 3") || !containsAll(prompts[2], "Attempt 4") {
		t.Errorf("dispatch prompts = %q, want numbered attempts 2, 3 and 4", prompts)
	}
}

// A fact-read error is not negative evidence. It follows the existing
// fail-closed preparation path and starts no agent, rather than silently
// presenting the reattached branch as attempt 1.
func TestAttemptFloorEvidenceErrorLaunchesNothing(t *testing.T) {
	h := start(t, harnessOpts{
		issues:     []core.Issue{fake.Issue("1", epoch)},
		legacyBase: fake.DefaultBaseSHA,
		prepareFacts: func(core.Workspace) (core.LocalBranchFacts, error) {
			return core.LocalBranchFacts{}, errors.New("git evidence unavailable")
		},
	})
	h.WaitGone("1")

	if got := h.Runner.StartCount(); got != 0 {
		t.Errorf("started %d agents after the evidence read failed, want 0", got)
	}
	if got := h.Workspaces.PrepareCount("1"); got != 1 {
		t.Errorf("Prepare called %d times, want the one call before the evidence refusal", got)
	}
	if got := h.o.Transitions.Path("1"); !containsState(got, StateFailed) {
		t.Errorf("path = %v, want the fail-closed preparation path", got)
	}
}
