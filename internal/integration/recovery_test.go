package integration

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
)

// The §9.10 half of §12.3: what a daemon does when it comes back.
//
// The classifier is model-checked in orchestrator/recover_classify_test.go, over
// facts handed to it directly. What only this boundary can assert is the driver
// reaching those verdicts through *real writes* — a daemon that claimed, labelled,
// launched and then died, and a second one that reads back exactly what the first
// one left on the tracker, on disk and in its run markers, with nothing carried
// over in memory.

// resumedPR is the pull request the resumed attempt publishes.
const resumedPR = "https://example.test/pull/41"

// SPEC §12.3-1: kill -9 → restart → converge.
//
// Converge means what it says, and the scenario runs all the way to it: the first
// daemon is killed with an agent live in its worktree, a second one starts on the
// same host, and the issue reaches `done` with a published pull request — no
// human, no second workspace, no lost commits, and the claim never handed back.
//
// The table is over the two answers §9.10's run probe can give, because the
// asymmetry between them *is* the precondition. `true` frees the workspace and the
// orphan resumes; anything else means possibly live, and the daemon must retain
// everything and dispatch nothing until it is confirmed gone — a wrong `false`
// costs a tick, a wrong `true` puts a second agent in a live worktree (SPEC §7.5).
// Both must end in the same place, and the second says so on its own timetable.
func TestAKilledDaemonRecoversAndConverges(t *testing.T) {
	for _, tc := range []struct {
		name string
		// probe is what the restarted daemon's run prober answers about the run
		// the dead one left behind.
		probe func(core.RunEvidence) (bool, error)
		// outlived says the execution domain survived the daemon, so recovery must
		// wait before it may reuse the workspace.
		outlived bool
	}{
		{name: "the agent died with the daemon", probe: domainQuiet},
		{name: "the agent outlived the daemon", probe: domainLive, outlived: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, scenarioConfig{
				runGone: tc.probe,
				before: func(h *scenario) {
					// The first attempt never ends on its own: it is the run that is
					// still going when the daemon dies. The second one does the work
					// and publishes, and the pull request appears *because* that run
					// created it — scripted here rather than seeded up front, since a
					// PR standing before the restart would make recovery's own §9.7
					// read say published and finish an interrupted `done` instead of
					// resuming an orphan, which is a different row of the table.
					h.Runner.SetHangAfterScript(true)
					h.Runner.SetScript(func(_ core.RunSpec, attempt int) []core.Event {
						if attempt == 1 {
							return []core.Event{{Type: core.EventStarted, SessionID: "s1", Continuation: "s1"}}
						}
						h.Tracker.SetPR("ben/issue-7", core.PR{
							Number: 41, URL: resumedPR, State: "open", Branch: "ben/issue-7", BaseBranch: "main",
						})
						return fake.Succeed("s2")
					})
				},
			})

			// The kill point: an agent is live and its marker identifies it, which
			// is the only state from which the probe question can even be asked.
			agent := h.waitRunning("7")
			h.waitFor("the run marker of the live agent", func() bool {
				m, ok := h.Workspaces.RunMarkerFor("7")
				return ok && m.State == core.RunMarkerIdentified
			})
			// waitRunning answers about the record; the tracker projection it owes is
			// asynchronous. The projected label is the kill-point fact this fixture
			// preserves across restart, so wait for that fact rather than sampling the
			// effects goroutine mid-turn.
			h.waitFor("the running projection before the kill", func() bool {
				return h.Tracker.Label("7") == core.StateLabelRunning
			})
			base := h.Workspaces.Prepares("7")[0].BaseSHA

			// The kill, in two steps, because they are two events and their order is
			// the whole subject: the daemon dies first, and the agent's own fate is
			// separate. Nothing here stops the agent — a kill reaches the daemon and
			// nothing else — so where the scenario says the agent died with it, the
			// fixture ends that run itself, with nobody having asked it to.
			//
			// It has to be after the daemon is gone. The old loop is still consuming
			// that run's events until then, and a run ending under it is an attempt
			// that exited without a terminal event: it would route to the failure
			// track and leave a different world behind than the one being recovered.
			h.stop()
			if !tc.outlived {
				endAgent(t, agent)
			}

			if err := h.restart(); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			// The dead daemon's agent hangs by construction; the resumed one has to be
			// able to finish. Safe here and nowhere later: nothing can launch until a
			// backoff timer fires, and on a manual clock only a tick produces one.
			h.Runner.SetHangAfterScript(false)

			// The floor is on the record before anything re-dispatches, which is the
			// only moment it can be read as a *floor* rather than as a count: the
			// resumed dispatch increments it, so an implementation that adopted at 1
			// would still prepare an attempt 2 and be indistinguishable here
			// afterwards. Recover runs on the caller's goroutine, so this is settled
			// by the time restart returns.
			adopted, ok := h.o.StatusFor("7")
			if !ok {
				t.Fatal("recovery adopted no record; a candidate with no record is one dispatch can claim a second time")
			}
			if adopted.Attempt < 2 {
				t.Errorf("adopted at attempt %d, want >= 2: §9.10 reads a recovered claim as work that may already exist",
					adopted.Attempt)
			}

			if tc.outlived {
				// Retained, and the barrier is a positive fact only the wait verdict
				// can produce: the label goes back from `ben:running` to `ben:claimed`,
				// because §9.10 step 4's precondition governs the workspace alone and
				// the tracker repair underneath it still lands.
				//
				// The label rather than the milestone comment, which was the first
				// spelling and proves nothing: the dead daemon posted a `claimed`
				// comment of its own when it claimed the issue, and the comment
				// recovery re-issues is idempotent per occurrence (SPEC §8.4), so it
				// is invisible either way. A barrier satisfied before the restart is
				// no barrier at all — the #100 family, from a new direction.
				h.waitFor("the tracker repair the wait verdict owes", func() bool {
					return h.Tracker.Label("7") == core.StateLabelClaimed
				})
				h.ticks(2)
				if n := h.dispatches("7"); n != 1 {
					t.Errorf("prepared %d attempts while the previous run may still be live; a possibly-live workspace is never reattached", n)
				}
				if n := h.Runner.StartCount(); n != 1 {
					t.Errorf("started %d agents; the whole point is that no second agent enters a live worktree", n)
				}
				if n := h.Tracker.ReleaseCount("7"); n != 0 {
					t.Errorf("released %d times; a possibly-live workspace releases nothing", n)
				}
				if got := h.Workspaces.Disposals("7"); len(got) != 0 {
					t.Errorf("disposed %+v; a possibly-live workspace is not disposed", got)
				}
				if m, ok := h.Workspaces.RunMarkerFor("7"); !ok || m.State != core.RunMarkerIdentified {
					t.Errorf("marker = %+v (present=%v); only proof of absence removes one (SPEC §9.10)", m, ok)
				}

				// And it converges once the domain is confirmed quiet, with no human —
				// which is the reason §9.10 waits rather than parking.
				//
				// The run ends first and the probe answers afterwards, in that order:
				// a prober saying "gone" about a run this fixture still has going
				// would describe two worlds at once, and a scenario built on it would
				// be resuming into a workspace something is still writing.
				endAgent(t, agent)
				h.Prober.set(domainQuiet)
			} else if m, ok := h.Workspaces.RunMarkerFor("7"); ok {
				// The other side of the same rule: a marker comes off exactly when
				// its run is proved gone, and recovery proved it just now. Leaving it
				// standing is a lie with a delay in it — this daemon carries on as
				// though the workspace were free, while the *next* start reads a
				// marker for a run nobody will ever probe again, calls the launch
				// outcome unknown, and parks an issue this one had already resumed.
				// Invisible within a single restart, which is why it is asserted
				// rather than left to the convergence below.
				t.Errorf("marker = %+v still stands after the run was confirmed gone (SPEC §9.10 step 4)", m)
			}

			h.settle("7", orchestrator.StateDone)
			h.waitMilestone("7", core.MilestonePublished)
			if got := h.lastComment("7").PRURL; got != resumedPR {
				t.Errorf("publish comment PRURL = %q, want the URL leg 3 found", got)
			}

			// One workspace, one base, one agent per attempt. The resumed attempt
			// reattached rather than force-creating: a re-minted claim-time pin is how
			// "descends from its claim-time base" would quietly start being measured
			// from somewhere else (SPEC §6.2, §9.7, never -B).
			prepares := h.Workspaces.Prepares("7")
			if len(prepares) != 2 {
				t.Fatalf("prepares = %+v, want one before the kill and one after it", prepares)
			}
			if base == "" || prepares[1].BaseSHA != base {
				t.Errorf("the resumed attempt pinned base %q, want the claim-time base %q the dead daemon minted",
					prepares[1].BaseSHA, base)
			}
			if prepares[1].Attempt < 2 {
				t.Errorf("the resumed prepare is attempt %d, want >= 2: §9.10 reads a recovered orphan as work that may already exist",
					prepares[1].Attempt)
			}
			if n := h.Runner.StartCount(); n != 2 {
				t.Errorf("started %d agents for one issue across the restart, want one per attempt", n)
			}
			// The floor reaches the agent, through the real Liquid layer — which is
			// what "work may already exist" means to the thing that has to act on it.
			//
			// Bound to the number the prepare carried rather than to a literal: §9.6
			// makes the recovered attempt a *floor* and not a reconstructed count, so
			// what §9.10 promises is that the two agree and that they read as more
			// than a first attempt, which is asserted above.
			want := fmt.Sprintf("attempt %d", prepares[1].Attempt)
			if prompt := h.Runner.Prompts()[1]; !strings.Contains(prompt, want) {
				t.Errorf("the resumed attempt's prompt does not say it is %s:\n%s", want, prompt)
			}

			// Nothing was handed back and nothing was left holding the workspace.
			if n := h.Tracker.ReleaseCount("7"); n != 0 {
				t.Errorf("released %d times across the restart; the claim is retained until the pull request is merged (SPEC §9.2)", n)
			}
			h.waitFor("the workspace disposal `done` owes", func() bool {
				return len(h.Workspaces.Disposals("7")) > 0
			})
			if got := h.Workspaces.Disposals("7"); len(got) != 1 || got[0].Keep {
				t.Errorf("disposals = %+v, want exactly the one at `done`, not kept", got)
			}
			h.waitFor("the run marker of the finished run to be cleared", func() bool {
				_, ok := h.Workspaces.RunMarkerFor("7")
				return !ok
			})
		})
	}
}

// endAgent ends a run and refuses to go on until its domain is quiet.
//
// The check is a precondition, not a test of the fake: every caller's next act is
// to let a prober report that group as gone, and a probe answering "unconfirmed"
// at this line would put the two facts in the wrong order — a scenario resuming
// into a workspace its own fixture still has something running in. EndRun promises
// this; the assertion is what would notice if it stopped.
func endAgent(t *testing.T, agent *fake.Handle) {
	t.Helper()
	agent.EndRun()
	if got := agent.Probe(t.Context()); got != core.TerminationConfirmed {
		t.Fatalf("the run's group reads %v after it ended; a prober told it was gone here would be ahead of the world", got)
	}
}

// SPEC §12.3-1's other half: the windows *between* the writes of a multi-write
// projection.
//
// "Any point" is the clause that matters, and §12.3-1 names the points it means:
// after the assignment and before the label projection; mid-`done`, with the labels
// cleared and the publish comment not yet posted; and mid-`failed`, with the label
// set and the claim not yet released. BUILD.md's acceptance adds the boundaries
// *inside* one projection, since add-before-remove makes them reachable and each has
// its own verdict — the fourth case below is that one. Every window here is a state
// a tracker can really be left in, because each write is its own request and a
// process can stop between any two of them.
//
// Every one of them is *driven* here rather than written down: the loop claims,
// projects, runs, verifies and fails through its own writes, and the fixture only
// makes the one write that defines the window fail. So the world recovery reads
// back is the world a daemon actually leaves, including the change log — which is
// what §9.10 classifies from, and the thing a hand-authored fixture would quietly
// get to agree with whatever the implementation did.
//
// The window's fault heals with the process: a restart is a new client, and the
// tracker that refused one write is not refusing them for ever. What is left is
// the *state*, which is the whole of what recovery has to work from.
func TestARestartFinishesTheProjectionItDiedInsideOf(t *testing.T) {
	// Case-local fixture state, declared out here so an arrange and its restore can
	// share it. Subtests run in order, and none of them is parallel.
	var commentLost atomic.Bool
	lostWrite := errors.New("502 from the tracker")

	for _, tc := range []struct {
		name string
		// workflow is the configuration this window needs. Nil takes the default.
		workflow *workflow
		// arrange installs the seam that makes one write fail, before the loop runs.
		arrange func(h *scenario)
		// window blocks until the daemon is inside the window, and refuses to go on
		// if the fixture is somewhere else. Where the window is *inside* a single
		// write — add-before-remove, which returns to nobody and so cannot be reached
		// by failing anything — it is also where the fixture performs the half of it
		// that landed.
		window func(t *testing.T, h *scenario)
		// restore is the fault going with the process.
		restore func(h *scenario)
		// assert is what the restarted daemon finishes, and how it converges.
		assert func(t *testing.T, h *scenario)
	}{
		{
			name: "after the assignment landed and before pending is durable",
			arrange: func(h *scenario) {
				h.Tracker.SetPR("ben/issue-7", core.PR{
					Number: 51, URL: resumedPR, State: "open", Branch: "ben/issue-7", BaseBranch: "main",
				})
				// §9.5's content read is what stands between a verified claim and the
				// first projection, and a read that failed is retried with the claim
				// retained (§9.10's recoveryActionApprove says so in as many words). So
				// this is not a simulation of the window — it is the window, held open.
				h.Tracker.SetFailContentApproval(lostWrite)
			},
			window: func(t *testing.T, h *scenario) {
				h.waitFor("the §9.5 content check that stands between the claim and its projection",
					func() bool { return h.Tracker.ContentReads() > 0 })
				if got := h.assignees("7"); !slices.Contains(got, fake.DefaultPrincipal) {
					t.Fatalf("assignees = %v; the assignment has not landed, so this is not the window", got)
				}
				if got := h.Tracker.Label("7"); got != core.StateLabelNone {
					t.Fatalf("label = %q; the window is before any ben:* projection", got)
				}
				if n := h.dispatches("7"); n != 0 {
					t.Fatalf("prepared %d workspaces; §9.2 puts the projection before `preparing`, so no attempt can have run here", n)
				}
			},
			restore: func(h *scenario) { h.Tracker.SetFailContentApproval(nil) },
			assert: func(t *testing.T, h *scenario) {
				// The historical pinning instant was never made durable. Recovery
				// cannot reconstruct it from the branch now present, so this is the
				// deliberate loss-of-storage degradation rather than an attempt-one
				// dispatch.
				h.waitPosted("7", core.MilestoneNeedsReview)
				h.waitState("7", orchestrator.StateNeedsReview)
				if prepares := h.Workspaces.Prepares("7"); len(prepares) != 0 {
					t.Errorf("prepares = %+v, want none without a durable pending epoch", prepares)
				}
				if n := h.Tracker.ReleaseCount("7"); n != 0 {
					t.Errorf("released %d times; the epoch-faulted claim is retained for remediation", n)
				}
			},
		},
		{
			name: "after pending is durable and before ben:claimed",
			arrange: func(h *scenario) {
				h.Tracker.SetPR("ben/issue-7", core.PR{
					Number: 51, URL: resumedPR, State: "open", Branch: "ben/issue-7", BaseBranch: "main",
				})
				h.Tracker.FailLabel = func(_ string, label core.StateLabel) error {
					if label == core.StateLabelClaimed {
						return lostWrite
					}
					return nil
				}
			},
			window: func(t *testing.T, h *scenario) {
				h.waitFor("the durable pending epoch before claim projection", func() bool {
					state, err := h.Workspaces.ClaimBase(context.Background(), core.Issue{Identifier: "7"})
					return err == nil && state.State == core.ClaimBasePending && state.Epoch > 0
				})
				if got := h.Tracker.Label("7"); got != core.StateLabelNone {
					t.Fatalf("label = %q; the fixture is before ben:claimed lands", got)
				}
				if n := h.dispatches("7"); n != 0 {
					t.Fatalf("prepared %d workspaces before the claim projection", n)
				}
			},
			restore: func(h *scenario) { h.Tracker.FailLabel = nil },
			assert: func(t *testing.T, h *scenario) {
				h.waitPosted("7", core.MilestoneClaimed)
				h.settle("7", orchestrator.StateDone)
				prepares := h.Workspaces.Prepares("7")
				if len(prepares) != 1 || prepares[0].Attempt != 1 {
					t.Errorf("prepares = %+v, want the pending first prepare at attempt 1", prepares)
				}
			},
		},
		{
			name: "mid-done: the labels are cleared and the publish comment is not posted",
			arrange: func(h *scenario) {
				h.Tracker.SetPR("ben/issue-7", core.PR{
					Number: 52, URL: resumedPR, State: "open", Branch: "ben/issue-7", BaseBranch: "main",
				})
				commentLost.Store(true)
				h.Tracker.FailComment = func(_ string, m core.Milestone) error {
					if m == core.MilestonePublished && commentLost.Load() {
						return lostWrite
					}
					return nil
				}
			},
			window: func(t *testing.T, h *scenario) {
				// The *attempt* is the barrier, not the label set: the fake records a
				// comment call whether or not it lands, while `ben:*` cleared and
				// `ben:*` never projected are the same reading of an empty label — a
				// barrier already true before the run began is no barrier at all.
				h.waitFor("the publish comment the `done` verdict owes to have been attempted and lost", func() bool {
					calls, _, _ := h.Tracker.Snapshot()
					return slices.Contains(calls, "comment 7=published")
				})
				if h.posted("7", core.MilestonePublished) {
					t.Fatal("the publish comment landed; the fixture is past the window, not inside it")
				}
				if got := h.Tracker.Label("7"); got != core.StateLabelNone {
					t.Fatalf("label = %q; `done` clears the projection before it comments", got)
				}
			},
			restore: func(h *scenario) { commentLost.Store(false) },
			assert: func(t *testing.T, h *scenario) {
				// The interrupted `done` is finished rather than re-run: the comment the
				// dead daemon owed lands, carrying the pull request the adapter refuses a
				// published comment without.
				h.waitPosted("7", core.MilestonePublished)
				if got := h.lastComment("7").PRURL; got != resumedPR {
					t.Errorf("publish comment PRURL = %q, want the URL leg 3 found", got)
				}
				if n := h.milestones("7", core.MilestonePublished); n != 1 {
					t.Errorf("posted %d publish comments; re-issuing one is idempotent per occurrence (SPEC §8.4)", n)
				}
				// Held, not resurrected: the claim stays while the pull request awaits
				// review, and §9.8 releases it on the close with no further restart.
				h.waitFor("the published claim to become a held claim", func() bool { return h.o.HeldCount() == 1 })
				if n := h.Runner.StartCount(); n != 1 {
					t.Errorf("started %d agents; a published issue awaiting review is not re-run", n)
				}
				if n := h.Tracker.ReleaseCount("7"); n != 0 {
					t.Errorf("released %d times while the pull request was open", n)
				}

				h.Tracker.Mutate("7", func(iss *core.Issue) { iss.State = "closed" })
				h.tick()
				h.waitFor("the retained claim to be released once the issue closes", func() bool {
					return h.Tracker.ReleaseCount("7") == 1
				})
			},
		},
		{
			name: "mid-failed: ben:failed is set and the claim is not released",
			workflow: func() *workflow {
				// One attempt, so the first failure is terminal and the run reaches
				// `failed` rather than the retry track.
				wf := defaultWorkflow()
				wf.MaxAttempts = 1
				return &wf
			}(),
			arrange: func(h *scenario) {
				h.Runner.SetScript(func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureCrashed) })
				h.Tracker.SetFailRelease(lostWrite)
			},
			window: func(t *testing.T, h *scenario) {
				h.waitFor("the release the `failed` verdict owes to have been attempted and lost", func() bool {
					return h.Tracker.ReleaseAttempts("7") > 0
				})
				if got := h.Tracker.Label("7"); got != core.StateLabelFailed {
					t.Fatalf("label = %q; the window is after the projection and before the release", got)
				}
				if n := h.Tracker.ReleaseCount("7"); n != 0 {
					t.Fatalf("released %d times; the fixture is past the window", n)
				}
			},
			restore: func(h *scenario) { h.Tracker.SetFailRelease(nil) },
			assert: func(t *testing.T, h *scenario) {
				// The release is the barrier and the convergence at once: only this
				// daemon can produce it, because the previous one's never landed.
				h.waitFor("the claim of the failed run to be released", func() bool {
					return h.Tracker.ReleaseCount("7") == 1
				})
				// §6.4 keeps a failure's workspace for the human who will read it.
				if got := h.Workspaces.Disposals("7"); len(got) != 0 {
					t.Errorf("disposed %+v; a failure's workspace is kept", got)
				}
				// The comment is not duplicated, and that matters more here than
				// anywhere else: recovery re-issues a `failed` comment saying the reason
				// did not survive (this daemon wires no §9.11 reader), and a second
				// comment would replace an honest reason with that sentence.
				if n := h.milestones("7", core.MilestoneFailed); n != 1 {
					t.Errorf("posted %d failed comments; re-issuing one is idempotent per occurrence (SPEC §8.4)", n)
				}
				// Released back to the queue and left there: the issue still carries
				// ben:failed, so §8.3 does not call it dispatchable again.
				h.ticks(2)
				if n := h.dispatches("7"); n != 1 {
					t.Errorf("prepared %d workspaces; a released failure is not new work while it is labelled", n)
				}
			},
		},
		{
			// The boundary *inside* one projection, which BUILD.md's acceptance calls
			// out and which the three above cannot reach: §9.3 projects a state label
			// by adding the new one and then removing the old (github SetStateLabels),
			// so a process that stops between those two requests leaves **two** `ben:*`
			// labels standing.
			//
			// It is the one window no failing write can produce, and that is a fact
			// about the world rather than a limitation of the fixture: a crash returns
			// to nobody, while a write that *fails* leaves the projection owed and
			// retried — a different state, which §9.10 step 3 classifies differently.
			// So the fixture performs the half that landed (InterruptStateLabels,
			// which shares the adapter's own add-then-remove path so the two cannot
			// drift), on top of a `ben:claimed` the loop projected itself.
			//
			// The verdict is what makes it worth a case of its own. Classified from
			// the *ordered events*, the most recently labelled one is effective and the
			// issue parks; read as a label **set**, it matches two table rows at once —
			// and the other one resumes the run, putting an agent back on an issue a
			// human was asked to look at.
			name: "inside one projection: ben:needs-review was added and ben:claimed not yet removed",
			arrange: func(h *scenario) {
				h.Runner.SetScript(func(core.RunSpec, int) []core.Event { return fake.Fail(core.FailureCrashed) })
			},
			window: func(t *testing.T, h *scenario) {
				// A retryable failure leaves the loop's own ben:claimed standing with
				// no run in flight, which is the state a park decision would be taken
				// from — a §9.6 re-fetch that found content drift, say. Wait passively
				// on the append-only transition: settle advances the manual poll clock,
				// which can fire the retry before this crash window has been built.
				h.waitFor("the first attempt's backoff transition", func() bool {
					return h.reached("7", orchestrator.StateBackoff)
				})
				// The transition answers about the *record*, and both facts this fixture is
				// built on are behind it: the projection is a serial effect the
				// record's arrival owed, and the marker clear runs in a goroutine of
				// its own (orchestrator/marker.go). Reading either the instant the
				// state lands samples the daemon mid-turn — which is #276, and is
				// what put `label = "running", want "claimed"` in CI. Waiting on the
				// label *is* the acknowledgement here: the effect has landed on the
				// tracker, which is the only thing InterruptStateLabels below can
				// build on.
				h.waitFor("ben:claimed on issue 7 — the fixture needs a projection to interrupt", func() bool {
					return h.Tracker.Label("7") == core.StateLabelClaimed
				})
				h.waitFor("the attempt's run marker to be cleared, which its run being confirmed gone owes", func() bool {
					_, ok := h.Workspaces.RunMarkerFor("7")
					return !ok
				})
				if n := h.dispatches("7"); n != 1 {
					t.Fatalf("prepared %d workspaces before the crash window, want the first attempt only", n)
				}

				// The park's first write lands and its second does not.
				h.Tracker.InterruptStateLabels("7", core.StateLabelNeedsReview)
				if got := h.labels("7"); len(stateLabels(got)) != 2 {
					t.Fatalf("labels = %v; add-before-remove leaves both standing, and that is the window", got)
				}
			},
			restore: func(*scenario) {},
			assert: func(t *testing.T, h *scenario) {
				// Parked, because ben:needs-review was labelled last. The milestone is
				// a fact only this daemon produced: the previous one died before
				// commenting.
				h.waitPosted("7", core.MilestoneNeedsReview)
				h.waitState("7", orchestrator.StateNeedsReview)

				// And the projection is *completed*, which is the repair §9.10 owes:
				// the stale label goes, and exactly one `ben:*` is left standing. A
				// daemon that only re-asserted the effective label would leave the
				// issue matching two rows for ever, for the next restart to
				// misclassify.
				h.waitFor("the interrupted projection to be completed", func() bool {
					return len(stateLabels(h.labels("7"))) == 1
				})
				if got := stateLabels(h.labels("7")); got[0] != "ben:needs-review" {
					t.Errorf("standing label = %q, want ben:needs-review — the most recently labelled one is effective", got[0])
				}

				// A park retains everything and dispatches nothing, and the negatives
				// are read after a further whole cycle.
				h.ticks(2)
				if n := h.Tracker.ReleaseCount("7"); n != 0 {
					t.Errorf("released %d times; a park retains the claim for the human it is for", n)
				}
				if got := h.Workspaces.Disposals("7"); len(got) != 0 {
					t.Errorf("disposed %+v; needs-review keeps the workspace", got)
				}
				if n := h.dispatches("7"); n != 1 {
					t.Errorf("prepared %d workspaces; a parked issue is not resumed (SPEC §9.2)", n)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := start(t, scenarioConfig{
				workflow: tc.workflow,
				// Every window here is reached before or after a run rather than during
				// one, so the probe is never consulted — and stating it anyway would be
				// a capability this daemon is not being asked about.
				before: tc.arrange,
			})

			tc.window(t, h)
			h.stop()
			tc.restore(h)
			if err := h.restart(); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			tc.assert(t, h)
		})
	}
}

// SPEC §12.3-2 at the recovery boundary: two daemons hold one issue, and the one
// that comes back arbitrates from the change log (SPEC §9.10 gate 2).
//
// The *live* claim race — two daemons claiming a free issue at the same moment —
// is asserted against the real GitHub adapter, where §8.4's refuse-write-read-back
// sequence lives: TestClaimRaceHasExactlyOneWinner, TestReleaseNeverRemovesAnother
// Party, TestClaimYieldsWhenTheRaceCannotBeOrdered. What is only reachable from
// here is the other half. A daemon killed *inside* that sequence — the assignment
// written, the read-back that would have yielded never made — comes back to find
// the issue assigned to itself and to somebody else, and current state cannot say
// who won because both assignments stand.
//
// The mutual-release bug is what the gate exists to prevent: an issue both daemons
// let go of is unassigned, unlabelled and therefore dispatchable, so whatever work
// stands on its branch is redone. Exactly one may release, and the third verdict is
// the one that refuses to choose.
//
// So what each case here asserts is *which* daemon lets go, and nothing about what
// a release removes. The other half — that a release takes only our own assignment
// and never a co-assignee's — is the adapter's, asserted against it in
// TestReleaseNeverRemovesAnotherParty, and restating it here would assert the fake:
// nothing the orchestrator can do reaches another party's assignment, so a check on
// it could not fail whatever this loop did.
//
// Each world here is stated rather than driven, because §8.4's claim window is
// where these orders are producible and nothing else has happened in it yet: no
// label, no workspace, no run. Both assignments are still made through the
// tracker's own writes — Claim for ours, ClaimBy for the other party — so the log
// arbitration replays is the one those writes produced, not one written to match.
func TestARestartArbitratesAClaimAnotherPartyAlsoHolds(t *testing.T) {
	const other = "other-daemon"
	for _, tc := range []struct {
		name string
		// world is what the two daemons' writes left on the tracker before this
		// one started.
		world func(h *scenario, issue core.Issue)
		// assert is what the verdict owes, each barriered on a fact of its own.
		assert func(t *testing.T, h *scenario)
	}{
		{
			name: "our assignment is the first standing one, so we retain it",
			world: func(h *scenario, issue core.Issue) {
				h.claim(issue)
				h.Tracker.ClaimBy(issue.Identifier, other)
				h.beginCurrentClaimBase(issue.Identifier)
			},
			assert: func(t *testing.T, h *scenario) {
				// The winner carries on into the projection table, and what it reaches
				// here is #15's unprojected claim: assigned, never labelled, so the
				// claim goes to §9.5's content check and is announced once it passes.
				// That announcement is the fact to wait on.
				//
				// Read off the milestone rather than off the release count, and
				// deliberately: a winner still co-assigned *is* released eventually —
				// by §9.8's routability rule on a later tick, once nothing has
				// reconciled the co-assignee away — which is a different rule with a
				// different reason, and would make a release assertion here ambiguous.
				h.waitPosted("7", core.MilestoneClaimed)
			},
		},
		{
			name: "another party assigned first, so we release and only our own",
			world: func(h *scenario, issue core.Issue) {
				h.Tracker.ClaimBy(issue.Identifier, other)
				h.claim(issue)
			},
			assert: func(t *testing.T, h *scenario) {
				// The loser's terminal act is the release, and a record's writes are
				// FIFO — so once it has landed, any projection or comment this verdict
				// owed would have landed before it. That is what makes the negatives
				// below barriered rather than timed.
				h.waitFor("the loser to release its own assignment", func() bool {
					return h.Tracker.ReleaseCount("7") == 1
				})
				if got := h.Tracker.Label("7"); got != core.StateLabelNone {
					t.Errorf("projected %q; gate 2's loser releases and stops", got)
				}
				if got := h.Tracker.Milestones("7"); len(got) != 0 {
					t.Errorf("posted %v; gate 2's loser announces nothing", got)
				}
				if n := h.dispatches("7"); n != 0 {
					t.Errorf("prepared %d workspaces for an issue another daemon won", n)
				}
				if n := h.Runner.StartCount(); n != 0 {
					t.Errorf("started %d agents for an issue another daemon won", n)
				}
			},
		},
		{
			name: "the log cannot order the race, so it parks and says so",
			world: func(h *scenario, issue core.Issue) {
				h.claim(issue)
				// A co-assignee the change log has no event for. The log *spoke* —
				// this is not a failed read — and it cannot account for a party
				// currently holding the issue, which is the same absence gate 3
				// refuses to guess from: event retention, or a transfer. Nothing
				// orders this race, in either direction.
				h.Tracker.Mutate(issue.Identifier, func(iss *core.Issue) {
					iss.Assignees = append(iss.Assignees, other)
				})
			},
			assert: func(t *testing.T, h *scenario) {
				// The park is half the verdict and the operator error is the other
				// half. A scenario that only checked that nothing was dispatched would
				// pass against a daemon that silently did nothing at all — and this is
				// the one verdict reached *because* BEN refuses to guess, so somebody
				// has to be told.
				h.waitPosted("7", core.MilestoneNeedsReview)
				if got := h.Tracker.Label("7"); got != core.StateLabelNeedsReview {
					t.Errorf("label = %q, want %q", got, core.StateLabelNeedsReview)
				}
				if !h.reached("7", orchestrator.StateNeedsReview) {
					t.Errorf("path = %v, want the record parked", h.path("7"))
				}
				said := h.Logs.find("refuses to guess")
				if len(said) == 0 {
					t.Fatal("nothing was logged about a race recovery would not guess at; the park is the quiet half of this verdict")
				}
				if got := said[0]; got.Level != slog.LevelError || got.Attrs["issue"] != "7" {
					t.Errorf("logged at %v with %v; §9.10 owes a loud operator error naming the issue", got.Level, got.Attrs)
				}

				// And it stays parked: the claim is retained for the human it was
				// raised for, and further ticks neither let it go nor put an agent on
				// an issue two parties hold.
				h.ticks(2)
				if n := h.Tracker.ReleaseCount("7"); n != 0 {
					t.Errorf("released %d times; a park retains the claim", n)
				}
				if n := h.dispatches("7"); n != 0 {
					t.Errorf("prepared %d workspaces for a parked issue", n)
				}
				if !h.hasRecord("7") {
					t.Error("the record was dropped; a candidate with no record is one dispatch can claim")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := fake.Issue("7", epoch)
			h := start(t, scenarioConfig{
				issues: []core.Issue{issue},
				before: func(h *scenario) { tc.world(h, issue) },
			})

			tc.assert(t, h)
		})
	}
}
