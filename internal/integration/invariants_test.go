package integration

import (
	"errors"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
)

// SPEC §12.3-3: an agent's claim of success never publishes on evidence short
// of §9.7-complete.
//
// Parameterized over every shape of short evidence rather than over the one the
// acceptance criterion names, because "never `done`" is a claim about all of
// them and the routes differ: two of these contradict the claim and park at
// once, two are merely unfinished and walk the continuation track to max_turns
// first, and one cannot be read at all. A test of the single zero-commit case
// would leave the interesting half — the routes that *do* re-dispatch — asserting
// nothing about publication.
//
// The whole chain is real here: git facts through internal/verify's three legs,
// through the verdict binding, to the loop's route. Only the facts themselves
// are scripted, which is what a worktree on disk would otherwise supply.
func TestAClaimOfSuccessNeverPublishesOnEvidenceShortOfComplete(t *testing.T) {
	cases := []struct {
		name  string
		facts func(core.Workspace) (core.PublishFacts, error)
		// before is the tracker state leg 3 reads.
		before func(*fake.Tracker)
		// wantDetail is the operator-facing line, which comes from the evidence
		// check rather than from the routing decision — so what a human reads
		// names the fact, not the verdict.
		wantDetail string
		// legThree records whether the tracker should have been asked at all.
		// The git legs are local and settle most of these; asking anyway would
		// make a verdict the repository already holds depend on the network.
		legThree bool
	}{
		{
			name: "the run added no commits",
			facts: func(ws core.Workspace) (core.PublishFacts, error) {
				return core.PublishFacts{Head: ws.BaseSHA, DescendsBase: true}, nil
			},
			wantDetail: "the run added no commits",
		},
		{
			name: "the branch was rewritten off its claim-time base",
			facts: func(core.Workspace) (core.PublishFacts, error) {
				// Leg 1 fails, so origin is never probed and RemoteProbed stays
				// false — the shape the worktree provider reports for a rewrite.
				return core.PublishFacts{Head: rewrittenSHA}, nil
			},
			before: func(tr *fake.Tracker) {
				// An open pull request sitting on that very branch: the shape
				// that would otherwise look exactly like a published run.
				tr.SetPR("ben/issue-7", core.PR{Number: 13, URL: "https://example.test/pull/13", State: "open", Branch: "ben/issue-7"})
			},
			wantDetail: "does not descend from its claim-time base",
		},
		{
			name: "commits exist but were never pushed",
			facts: func(core.Workspace) (core.PublishFacts, error) {
				return core.PublishFacts{
					Head: agentCommitSHA, DescendsBase: true,
					RemoteProbed: true, RemoteHasHead: false,
				}, nil
			},
			wantDetail: "max_turns exhausted",
		},
		{
			name:       "pushed with no pull request",
			facts:      pushedAndDescends,
			wantDetail: "max_turns exhausted",
			legThree:   true,
		},
		{
			name:  "pushed with a closed pull request on the branch",
			facts: pushedAndDescends,
			before: func(tr *fake.Tracker) {
				// §9.7 leg 3 names an *open* pull request. A closed one is a
				// rejected earlier attempt, not evidence — and this is where "is
				// there a PR?" and "is there an open PR?" part company.
				tr.SetPR("ben/issue-7", core.PR{Number: 11, URL: "https://example.test/pull/11", State: "closed", Branch: "ben/issue-7"})
			},
			wantDetail: "max_turns exhausted",
			legThree:   true,
		},
		{
			name:  "leg 3 cannot be read",
			facts: pushedAndDescends,
			before: func(tr *fake.Tracker) {
				tr.SetFailFindPR(errors.New("502 from the tracker"))
			},
			// Fail closed: every local fact says published, which is where
			// guessing would be most tempting and most expensive (§9.7).
			wantDetail: "502 from the tracker",
			legThree:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf := defaultWorkflow()
			// One turn, so an unfinished-but-clean run takes exactly one
			// continuation before the budget is out. The route is what varies
			// across this table; the length of it is not the subject.
			wf.MaxTurns = 1
			h := start(t, scenarioConfig{
				workflow: &wf,
				facts:    tc.facts,
				before: func(h *scenario) {
					if tc.before != nil {
						tc.before(h.Tracker)
					}
				},
			})

			h.settle("7", orchestrator.StateNeedsReview)

			if h.reached("7", orchestrator.StateDone) {
				t.Fatalf("path = %v; evidence short of complete must never publish (SPEC §12.3-3)", h.path("7"))
			}
			for _, m := range h.Tracker.Milestones("7") {
				if m == core.MilestonePublished {
					t.Fatalf("milestones = %v; a publish milestone was posted for a run that never verified", h.Tracker.Milestones("7"))
				}
			}

			h.waitMilestone("7", core.MilestoneNeedsReview)
			if got := h.lastComment("7").Detail; !strings.Contains(got, tc.wantDetail) {
				t.Errorf("needs-review detail = %q, want it to name %q", got, tc.wantDetail)
			}
			if got := h.Tracker.FindPRReads() > 0; got != tc.legThree {
				t.Errorf("leg 3 read = %v, want %v — the git legs decide locally where they can", got, tc.legThree)
			}

			// SPEC §9.2/§6.4: parked keeps both the workspace and the claim.
			// Everything a human needs to look at is still there.
			if got := h.Workspaces.Disposals("7"); len(got) != 0 {
				t.Errorf("disposed %+v; needs-review keeps the workspace", got)
			}
			if n := h.Tracker.ReleaseCount("7"); n != 0 {
				t.Errorf("released %d times; needs-review retains the claim", n)
			}
		})
	}
}

// SPEC §12.3-4: a retry after failure preserves the agent's commits.
//
// The `-B` refusal itself is a git fact and belongs to workspace_test.go against
// real repositories. What is only visible from here is the pair of properties
// that make it *matter*: the second attempt is prepared into the same workspace
// rather than a fresh one, and it is measured against the same claim-time base.
// A base that moved per attempt would silently redefine "advanced past its
// claim-time base" on every retry, and a §9.7 check would then be verifying
// something nothing performs.
func TestRetryPreservesTheWorkspaceAndItsClaimTimeBase(t *testing.T) {
	h := start(t, scenarioConfig{
		script: func(_ core.RunSpec, attempt int) []core.Event {
			if attempt == 1 {
				// Retryable per §7.3's static table, so attempts remain.
				return fake.Fail(core.FailureCrashed)
			}
			return fake.Succeed("session-B")
		},
		before: func(h *scenario) {
			h.Tracker.SetPR("ben/issue-7", core.PR{Number: 21, URL: "https://example.test/pull/21", State: "open", Branch: "ben/issue-7"})
		},
	})

	h.settle("7", orchestrator.StateDone)

	prepares := h.Workspaces.Prepares("7")
	if len(prepares) != 2 {
		t.Fatalf("prepares = %+v, want one per attempt", prepares)
	}
	if prepares[0].BaseSHA == "" || prepares[0].BaseSHA != prepares[1].BaseSHA {
		t.Errorf("claim-time base moved between attempts: %q then %q — a retry must be measured against the base its claim pinned (SPEC §6.2)",
			prepares[0].BaseSHA, prepares[1].BaseSHA)
	}
	if prepares[1].Attempt != 2 {
		t.Errorf("second prepare was attempt %d, want 2", prepares[1].Attempt)
	}

	// Nothing was thrown away in between. A disposal before the retry is
	// exactly how the commits of attempt 1 would be lost.
	//
	// Barriered on the disposal itself, because `settle` waits on the *state* and
	// the disposal is queued after it: onVerified's published branch transitions
	// to `done` — publishing the snapshot Status reports — and only then owes the
	// comment and the disposal, which drain on the record's serial effects queue.
	// Asserting straight after the state is the #103 defect exactly, and it
	// reproduces: `disposals = []` twice in 120 runs at GOMAXPROCS=2 under CPU
	// contention, and never in 100 unloaded ones.
	h.waitFor("the workspace disposal `done` owes", func() bool {
		return len(h.Workspaces.Disposals("7")) > 0
	})
	disposals := h.Workspaces.Disposals("7")
	if len(disposals) != 1 || disposals[0].Keep {
		t.Fatalf("disposals = %+v, want exactly the one at `done`, not kept", disposals)
	}

	// SPEC §9.6: the failure track does not resume a session. The continuation
	// token belongs to the continuation track alone — the two tracks are asking
	// for different things, and a retry that resumed the crashed session would
	// be asking the agent to continue from a state it did not reach.
	if got := h.Runner.Continuations(); len(got) != 2 || got[1] != "" {
		t.Errorf("continuations = %q, want the retry to start a fresh session", got)
	}
	// The prompt says so too, which is the half an operator can see.
	if prompt := h.Runner.Prompts()[1]; !strings.Contains(prompt, "attempt 2") ||
		!strings.Contains(prompt, "recover and continue") {
		t.Errorf("retry prompt does not report the previous failure:\n%s", prompt)
	}
}

// SPEC §12.3-5: an unconfirmed stop retains the claim, and no workspace is ever
// reused while a run may still hold it.
//
// The scenario is the one §9.8 is written for: the issue goes terminal under a
// live run, so the loop orders a stop and a release — and the stop cannot be
// confirmed. Nothing may be let go on that evidence. The claim stays, the
// workspace stays, and the release lands only once a later stop confirms, which
// is asserted here rather than assumed: a test that stopped at "it did not
// release" would pass equally against a daemon that never releases at all.
func TestAnUnconfirmedStopRetainsTheClaimAndTheWorkspace(t *testing.T) {
	h := start(t, scenarioConfig{
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s", Continuation: "s"}}
		},
		before: func(h *scenario) {
			// The run is held open for the whole scenario, and that is load-bearing
			// alongside the unconfirmed stop rather than incidental. Stop's answer
			// is the knob below; Probe derives its own from the process group, so a
			// run that *ends* here would have a group Probe rightly calls gone and
			// a Stop claiming otherwise — a world the harness cannot produce, and
			// the family of fixture defect #100 and #103 came from. A live process
			// makes both answers agree (orchestrator harness_test, stopUnconfirmed).
			h.Runner.SetHangAfterScript(true)
			h.Runner.SetStopTermination(core.TerminationUnconfirmed)
		},
	})
	handle := h.waitRunning("7")

	// The issue closes under the running agent: §9.8's terminal reconciliation.
	h.Tracker.Mutate("7", func(iss *core.Issue) { iss.State = "closed" })
	h.tick()
	h.waitFor("the stop the terminal issue ordered", func() bool { return handle.StopCount() > 0 })

	// The **second** stop is the barrier, and the first one cannot be.
	//
	// StopCount increments at Stop's entry — deliberately, so a test can catch a
	// ladder standing in a gate — so it proves the question was asked and says
	// nothing about whether the unconfirmed answer has reached the loop. Ticking
	// from there only synchronizes the dispatch cycle, which is a different
	// goroutine's business entirely: every negative below could then be read
	// before the retention decision had been made at all, and would pass without
	// exercising the invariant they are here for.
	//
	// A retry is the acknowledgement, because of what has to be true for one to
	// happen: beginStop holds a single in-flight ladder slot that is released
	// only when the loop applies sigStopped, and retryPendingExits re-drives the
	// stop on a later tick only while the record is *still* stopping. So a second
	// stop means the loop received "unconfirmed", kept the record, and came back
	// for another go — which is precisely the decision under test.
	h.tick()
	h.waitFor("the stop to be retried, which is the loop having applied the unconfirmed result", func() bool {
		return handle.StopCount() >= 2
	})

	// A further tick on top, so the negatives are read after a whole cycle that
	// had the applied verdict in hand and still let nothing go.
	h.ticks(2)
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times while the process group was unconfirmed; §9.8 retains the claim until a stop confirms", n)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; a workspace a live process may still hold is not free", got)
	}
	if n := h.dispatches("7"); n != 1 {
		t.Errorf("prepared %d attempts; no workspace may be re-entered while its run is unconfirmed (SPEC §12.3-5)", n)
	}
	if !h.hasRecord("7") {
		t.Error("the record was forgotten with the claim still standing")
	}

	// The ladder gets there in the end, and the exit completes without a
	// restart. This is the half that proves the retention above was a wait and
	// not a wedge.
	h.Runner.SetStopTermination(core.TerminationConfirmed)
	h.tick()
	h.waitFor("the release of the terminal issue's claim", func() bool { return h.Tracker.ReleaseCount("7") == 1 })
	h.waitFor("the disposal of its workspace", func() bool { return len(h.Workspaces.Disposals("7")) == 1 })
}

// SPEC §12.3-5, the other half: one issue is never dispatched twice.
//
// The tracker is made to keep reporting a claimed issue as dispatchable, which
// is not a contrived fixture — it is §9.8's consistency lag, and the shape a
// second daemon's stale list read has too. The loop's defence is its own record,
// not the tracker's verdict, and that is what this asserts: however many ticks
// see the issue as eligible, exactly one workspace is ever prepared for it.
func TestALaggingQueueReadNeverDispatchesAnIssueTwice(t *testing.T) {
	h := start(t, scenarioConfig{
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s", Continuation: "s"}}
		},
		before: func(h *scenario) { h.Runner.SetHangAfterScript(true) },
	})
	h.waitRunning("7")

	// The queue read has not caught up with the claim. The assignment is left
	// standing deliberately: clearing it would make the issue *unroutable* and
	// §9.8 would tear the live run down, which is a different invariant and
	// would hide this one.
	h.Tracker.Mutate("7", func(iss *core.Issue) { iss.Dispatchable = true })
	h.ticks(3)

	if n := h.dispatches("7"); n != 1 {
		t.Errorf("prepared %d workspaces for one issue; a stale queue read must not produce a second dispatch", n)
	}
	if n := h.Runner.StartCount(); n != 1 {
		t.Errorf("started %d agents for one issue (SPEC §12.3-5, §9.5)", n)
	}
}

// SPEC §12.3-7 at the daemon level: a drain is not a kill.
//
// The kill edge itself — *any non-terminal state* → `failed(killed)` — is
// asserted over all 81 state pairs in orchestrator/transitions_test.go against a
// restatement of §9.2 independent of the implementation's map, and v1 exposes no
// command that kills a run from outside, so there is no wider shape to drive
// here. What *is* integration-shaped is the confusion the edge invites: shutdown
// interrupts every in-flight run and waits for a confirmed termination, and none
// of that may be recorded as a failure. §9.8 as amended is explicit that a drain
// initiates no release and no terminal projection — the claim and label are left
// standing precisely so §9.10 can resume the work.
func TestShutdownSuspendsRatherThanFailing(t *testing.T) {
	h := start(t, scenarioConfig{
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{{Type: core.EventStarted, SessionID: "s", Continuation: "s"}}
		},
		before: func(h *scenario) { h.Runner.SetHangAfterScript(true) },
	})
	handle := h.waitRunning("7")

	if err := h.o.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if handle.StopCount() == 0 {
		t.Error("the drain returned without interrupting the run it was draining")
	}
	if h.reached("7", orchestrator.StateFailed) {
		t.Errorf("path = %v; a drain must not record an in-flight run as failed (SPEC §9.8, §11)", h.path("7"))
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; shutdown initiates no release — a released issue still bearing ben:running is one no daemon will pick up and only a human can clear", n)
	}
	if got := h.Tracker.Label("7"); got != core.StateLabelRunning {
		t.Errorf("state label = %q, want it left standing at %q for §9.10 to classify from", got, core.StateLabelRunning)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; the drain leaves the workspace for the next tenure", got)
	}
	if !h.o.Draining() {
		t.Error("Draining() is false after a completed drain; `ben status` is what tells an operator a still queue is a daemon on its way out")
	}
}

// SPEC §12.3-10: a budget breach stops the run and parks it.
func TestABudgetBreachStopsTheRunAndParks(t *testing.T) {
	budget := 1.0
	wf := defaultWorkflow()
	wf.MaxCostUSD = &budget

	h := start(t, scenarioConfig{
		workflow: &wf,
		script: func(core.RunSpec, int) []core.Event {
			return []core.Event{
				{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 10, OutputTokens: 10, CostUSD: 2.5}},
			}
		},
		before: func(h *scenario) {
			// The agent does not stop on its own — the orchestrator's own
			// enforcement is the subject, so the run has to outlive the breach.
			h.Runner.SetHangAfterScript(true)
		},
	})

	h.waitFor("a handle for the run", func() bool { return h.Runner.LastHandle() != nil })
	handle := h.Runner.LastHandle()
	h.waitState("7", orchestrator.StateNeedsReview)
	// Barriered, not asserted: §9.9 stops the run *and* parks the issue, and the
	// park is what `waitState` sees. Reading StopCount at that instant asks the
	// same question the retry-preserves case got wrong — whether an effect
	// ordered around the transition has landed yet.
	h.waitFor("the budget breach to stop the run", func() bool { return handle.StopCount() > 0 })
	// The limit reaches the adapter too, so a harness that can enforce it does.
	if spec, ok := h.Runner.LastSpec(); !ok || spec.Limits.MaxCostUSD != budget {
		t.Errorf("RunSpec.Limits.MaxCostUSD = %v, want the configured %v", spec.Limits.MaxCostUSD, budget)
	}

	h.waitMilestone("7", core.MilestoneNeedsReview)
	if h.reached("7", orchestrator.StateDone) {
		t.Errorf("path = %v; a breached budget never publishes", h.path("7"))
	}
	// Parked, not failed: the work so far is intact and a human decides.
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; needs-review retains the claim", n)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; needs-review keeps the workspace", got)
	}
}

// SPEC §12.3-11: a published issue's retained claim is released without a
// restart, once the issue closes.
//
// `done` deliberately keeps the claim: the pull request is awaiting review and a
// released issue would be re-dispatched under it (§9.2). What §12.3-11 adds is
// that the retention *ends by itself* — the §9.8 sweep sees the close and lets
// go on the next tick, rather than leaving a claim standing until someone
// restarts the daemon. The `closed` event is the evidence, and it is appended
// here as a change-log entry because that is how the real tracker reports one.
func TestAPublishedClaimIsReleasedWhenTheIssueClosesWithoutARestart(t *testing.T) {
	h := start(t, scenarioConfig{
		before: func(h *scenario) {
			h.Tracker.SetPR("ben/issue-7", core.PR{Number: 31, URL: "https://example.test/pull/31", State: "open", Branch: "ben/issue-7"})
		},
	})

	h.waitState("7", orchestrator.StateDone)
	h.waitMilestone("7", core.MilestonePublished)
	if got := h.lastComment("7").PRURL; got != "https://example.test/pull/31" {
		t.Errorf("publish comment PRURL = %q, want the URL leg 3 found", got)
	}
	// The run record converts into a held-claim record once every write `done`
	// ordered has landed and the claim-cycle anchor has been read back.
	// Everything below is about the held record, so the conversion is the
	// barrier — and the fact that marks it is the anchor read: exactly one
	// history read per issue that reaches `done`, on the done path and never on
	// the sweep, whose cost must not grow with the held set (§9.8).
	//
	// Deliberately not "the issue left `ben status`". It does not: convertToHeld
	// keeps the published snapshot on purpose, so an operator does not lose an
	// issue at the moment it starts awaiting review. A barrier on Status would
	// wait for something this daemon is designed never to do.
	h.tick()
	h.waitFor("the claim-cycle anchor read that converts the run record", func() bool {
		return h.Tracker.HistoryReads() > 0
	})
	if got := h.Tracker.Label("7"); got != core.StateLabelNone {
		t.Errorf("state label = %q, want it cleared at `done`", got)
	}

	// The claim is retained across ticks while the PR is open, and the sweep
	// that keeps looking costs no per-issue read.
	//
	// Measured as a *delta* across the held period rather than as an absolute:
	// reconciliation legitimately re-reads a live record's issue, so a total of
	// zero would be a claim about a different phase, and asserting it would
	// force this test to be written around the loop instead of around §9.8.
	// What §9.8 bounds is the sweep — one conditional list read per tick,
	// however long the review backlog grows — and an O(held) shape shows up
	// here and nowhere else.
	getsBefore := h.Tracker.GetReads()
	heldBefore := h.Tracker.HeldReads()
	h.ticks(2)
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Fatalf("released %d times while the pull request was open; `done` retains the claim (SPEC §9.2)", n)
	}
	if n := h.Tracker.GetReads() - getsBefore; n != 0 {
		t.Errorf("the sweep spent %d per-issue reads over two ticks; §9.8 bounds it at one conditional list read per tick", n)
	}
	if n := h.Tracker.HeldReads() - heldBefore; n != 2 {
		t.Errorf("the sweep made %d list reads over two ticks, want one per tick", n)
	}

	// A human merges and the issue closes.
	h.Tracker.Mutate("7", func(iss *core.Issue) { iss.State = "closed" })
	h.Tracker.AppendHistory("7", core.ClaimEvent{Kind: core.ClaimEventClosed, Actor: "human"})

	h.tick()
	h.waitFor("the retained claim to be released", func() bool { return h.Tracker.ReleaseCount("7") == 1 })

	// Released, not re-dispatched: a closed issue is not this daemon's work.
	h.ticks(2)
	if n := h.dispatches("7"); n != 1 {
		t.Errorf("prepared %d workspaces; releasing a closed issue must not hand it back as new work", n)
	}
}
