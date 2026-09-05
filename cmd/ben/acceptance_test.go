package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
)

// B09's acceptance criteria are chains, not verdicts: evidence a run left in a
// workspace, read as a verdict, routed to an outcome. Each link is covered
// where it lives — internal/workspace's facts table produces the git legs off
// real repositories, internal/verify's table turns leg shapes into verdicts,
// the orchestrator's policy tests route a verdict it is handed — and until
// this file nothing joined them. The seam between the two verdict enums lives
// here (verifier.go) and is unimportable from either side, so this is the only
// place the whole chain can be assembled, and B11's `ben run` uses that same
// assembly.
//
// The links are asserted rather than re-asserted: these tests state evidence
// and read the tracker, and say nothing about verdicts in between. A criterion
// that stops holding because a verdict was rewired, not because a leg changed,
// fails here and nowhere else.

const acceptanceWorkflow = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben-queue"]
agent:
  kind: claude-code
  provider:
    permission_mode: bypassPermissions
limits:
  max_concurrent_agents: 2
  max_turns: 4
  max_attempts: 3
deployment:
  mode: attended
---
Work issue {{ issue.identifier }}: {{ issue.title }}.
{% if attempt %}This is attempt {{ attempt }}.
{% if run.previous_outcome == "succeeded" %}Your previous session ended cleanly but without a published pull request — finish the remaining work and publish.
{% elsif run.previous_outcome %}Your previous session failed ({{ run.previous_outcome }}) — recover and continue.
{% else %}This branch already carries work, but the previous run outcome did not survive the claim boundary — inspect it and continue.
{% endif %}{% endif %}
`

// The prompt branch that distinguishes a continuation from a retry, worded as
// the dogfood WORKFLOW.md's publishing section words it. B09 acceptance 1 is
// about this sentence reaching the agent, not about a token being carried.
const publishCompletionPrompt = "finish the remaining work and publish"

// epochE2E is the fixed start time; the fake clock begins here so nothing
// depends on when the test runs.
var epochE2E = time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

// The SHAs a scripted run "left behind". Full-length because the §9.7 detail
// lines abbreviate anything longer than 12 characters, and a fixture that was
// already short would hide a truncation bug in the operator-facing text.
const (
	agentCommitSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rewrittenSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// pushedAndDescends is the evidence of a run that committed and pushed
// everything: all three git legs of §9.7 hold, so the verdict turns on
// whether the tracker has an open pull request.
func pushedAndDescends(core.Workspace) (core.PublishFacts, error) {
	return core.PublishFacts{
		Head:          agentCommitSHA,
		DescendsBase:  true,
		RemoteProbed:  true,
		RemoteHead:    agentCommitSHA,
		RemoteHasHead: true,
	}, nil
}

// e2e is the assembly under test: the three fakes, a manual clock, and a real
// *verify.Checker* reading the same fakes through the real shim.
type e2e struct {
	t          *testing.T
	o          *orchestrator.Orchestrator
	Tracker    *fake.Tracker
	Workspaces *fake.Workspaces
	Runner     *fake.Runner
	Clock      *fake.Clock

	done chan error
}

// startE2E runs the daemon loop over an issue whose agent always exits
// cleanly, claiming success. What separates the criteria is only what evidence
// that run left behind, which is what facts states.
//
// before configures the tracker ahead of the first tick. A test that seeded it
// afterwards would be racing its own dispatch — harmlessly today, since
// verification is several hops away, but the assertion would then rest on
// timing rather than on the state it means to describe.
func startE2E(t *testing.T, facts func(core.Workspace) (core.PublishFacts, error), before ...func(*fake.Tracker)) *e2e {
	t.Helper()

	h := &e2e{
		t:          t,
		Tracker:    fake.NewTracker(fake.Issue("7", epochE2E)),
		Workspaces: fake.NewWorkspaces(),
		Runner:     fake.NewRunner(),
		Clock:      fake.NewClock(epochE2E),
		done:       make(chan error, 1),
	}
	// The fresh-claim observation is local and precedes hooks and the run; the
	// supplied publication facts describe what the scripted run leaves behind.
	// Keep those two moments independently scripted, as the real provider does.
	h.Workspaces.SetPrepareFacts(func(ws core.Workspace) (core.LocalBranchFacts, error) {
		return core.LocalBranchFacts{Head: ws.BaseSHA, DescendsBase: true}, nil
	})
	h.Workspaces.SetFacts(facts)
	// A clean exit reporting success — the only run shape §9.7 is asked about.
	// Everything else is routed by the §7.3 taxonomy before verification.
	h.Runner.SetScript(func(core.RunSpec, int) []core.Event { return fake.Succeed("session-A") })
	for _, fn := range before {
		fn(h.Tracker)
	}

	def, err := config.Load(writeWorkflowContent(t, acceptanceWorkflow))
	if err != nil {
		t.Fatalf("loading the acceptance workflow: %v", err)
	}

	// The real chain: verify.Checker over the fakes' §9.7 seams, adapted to
	// the loop by the same routableVerdict the daemon will use.
	verifier, err := newVerifier(h.Workspaces, h.Tracker)
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}

	// The adapters arrive with the definition they were built from, through the
	// source the loop reads (SPEC §5.4). Nothing here reloads, so the source is
	// fixed at this one configuration.
	o, err := orchestrator.New(orchestrator.Config{
		Runtime: config.NewRuntimeSource(def, &orchestrator.Bundle{
			Definition:     def,
			Tracker:        h.Tracker,
			Workspaces:     h.Workspaces,
			Runner:         h.Runner,
			Verifier:       verifier,
			ClaimPrincipal: fake.DefaultPrincipal,
		}),
		Clock:    h.Clock,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		DaemonID: "test-host/acceptance",
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	h.o = o

	ctx, cancel := context.WithCancel(context.Background())
	// The daemon's own startup order (§9.10 then §9.1): Run refuses a loop that
	// has not recovered. The fixture's principal holds nothing, so this classifies
	// no candidates — what it buys is that the end-to-end tests drive the same
	// sequence supervise does.
	if err := o.Recover(ctx); err != nil {
		cancel()
		t.Fatalf("Recover: %v", err)
	}
	go func() { h.done <- o.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(2 * time.Second):
			t.Error("orchestrator did not stop within 2s")
		}
	})
	return h
}

// waitState blocks until the issue reaches a state, reporting the path it took
// instead if it does not.
func (h *e2e) waitState(want orchestrator.State) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got orchestrator.State
	for time.Now().Before(deadline) {
		for _, s := range h.o.Status() {
			if s.Identifier == "7" {
				got = s.State
				if s.State == want {
					return
				}
			}
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("issue 7 is %q, want %q (path: %v)", got, want, h.o.Transitions.Path("7"))
}

// waitFor polls a condition, failing rather than hanging.
func (h *e2e) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s (path: %v)", what, h.o.Transitions.Path("7"))
}

// waitForNeedsReviewComment blocks until the needs-review *milestone* is the
// latest one posted, which is the barrier every assertion on its Detail needs.
//
// Waiting on the state label instead is a barrier on the wrong fact, and it is
// how this suite flaked in CI (#86): enterNeedsReview projects the label and
// *then* posts the comment, both on the orchestrator's serial effects queue, so
// the label is visible first — and lastComment() at that instant returns the
// `claimed` comment, whose Detail is empty. The wait and the assertion now read
// the same list.
func (h *e2e) waitForNeedsReviewComment() {
	h.t.Helper()
	h.waitFor("the needs-review milestone", func() bool {
		posted := h.Tracker.Milestones("7")
		return len(posted) > 0 && posted[len(posted)-1] == core.MilestoneNeedsReview
	})
	// The label precedes the comment on that queue, so this cannot race — and
	// asserting it here keeps the projection covered now that nothing waits on
	// it, while stating the ordering the barrier above relies on.
	if got := h.Tracker.Label("7"); got != core.StateLabelNeedsReview {
		h.t.Errorf("state label = %q, want %q once the needs-review milestone is posted", got, core.StateLabelNeedsReview)
	}
}

// lastComment returns the most recent milestone comment posted for the issue.
func (h *e2e) lastComment() core.MilestoneComment {
	h.t.Helper()
	_, _, comments := h.Tracker.Snapshot()
	got := comments["7"]
	if len(got) == 0 {
		h.t.Fatal("no milestone comment was posted")
	}
	return got[len(got)-1]
}

func (h *e2e) reachedDone() bool {
	for _, s := range h.o.Transitions.Path("7") {
		if s == orchestrator.StateDone {
			return true
		}
	}
	return false
}

// B09 acceptance 1. The agent commits and pushes, then `gh pr create` fails —
// so the git legs hold and leg 3 does not. That is not a contradiction: the
// work exists and only publishing is unfinished, so §9.6 re-dispatches on the
// continuation track with the session token and a prompt that says what is
// left to do. Then the PR appears and the same evidence verifies.
func TestPushedWithoutAPRContinuesAndThenPublishes(t *testing.T) {
	h := startE2E(t, pushedAndDescends)

	// The first attempt verifies incomplete and arms the ~1s continuation
	// timer alongside the poll ticker.
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the continuation timer was never armed")
	}
	if h.reachedDone() {
		t.Fatalf("path = %v; a branch with no pull request must not publish", h.o.Transitions.Path("7"))
	}

	// The retry's `gh pr create` succeeds.
	h.Tracker.SetPR("ben/issue-7", core.PR{Number: 12, URL: "https://example.test/pull/12", State: "open", Branch: "ben/issue-7", BaseBranch: "main"})

	h.Clock.Advance(2 * time.Second)
	h.waitState(orchestrator.StateDone)

	if got := h.Runner.StartCount(); got != 2 {
		t.Fatalf("started %d runs, want a continuation re-dispatch", got)
	}
	// SPEC §9.6: the continuation track re-dispatches *with the token*, so the
	// second session resumes the first rather than starting over.
	if got := h.Runner.Continuations(); len(got) != 2 || got[1] != "session-A" {
		t.Errorf("continuations = %q, want the second run to resume session-A", got)
	}
	// SPEC §6.2: the claim-time base is pinned at the first prepare and read
	// back by later ones, so both turns of this continuation verified against
	// the same base. A base that moved per attempt would redefine "advanced
	// past its claim-time base" on every turn, and this test would then be
	// asserting a §9.7 check that nothing performs.
	prepares := h.Workspaces.Prepares("7")
	if len(prepares) != 2 {
		t.Fatalf("prepares = %+v, want one per attempt", prepares)
	}
	if prepares[0].BaseSHA == "" || prepares[0].BaseSHA != prepares[1].BaseSHA {
		t.Errorf("claim-time base moved between attempts: %q then %q",
			prepares[0].BaseSHA, prepares[1].BaseSHA)
	}
	// The acceptance criterion itself: the prompt tells the agent to finish
	// publishing, which it can only do because the incomplete verdict left
	// run.previous_outcome as "succeeded" (§5.2).
	prompt := h.Runner.Prompts()[1]
	if !strings.Contains(prompt, publishCompletionPrompt) {
		t.Errorf("the continuation prompt does not ask for publish completion:\n%s", prompt)
	}

	// §9.2's `done`: publish milestone with the link, workspace disposed,
	// claim retained so the PR is not re-dispatched while it awaits review.
	//
	// The barrier waits for *both* effects, because both are asserted on.
	// onVerified's published branch owes the comment and then the disposal, and
	// they ride the record's serial effects queue in that order — so waiting on
	// the comment barriers only itself and leaves the disposal assertion
	// unsynchronized, which is how this chain flaked (#103; #86 is the same
	// defect with the operands swapped). Waiting on the later of the two would
	// cover both today and silently stop doing so if the queue order ever
	// changed. Waiting on each fact asserted on depends on no ordering at all.
	// Two waits rather than one compound condition, so a timeout names the fact
	// that never arrived.
	h.waitFor("the publish milestone", func() bool {
		posted := h.Tracker.Milestones("7")
		return len(posted) > 0 && posted[len(posted)-1] == core.MilestonePublished
	})
	h.waitFor("the workspace disposal", func() bool {
		return len(h.Workspaces.Disposals("7")) > 0
	})
	if got := h.Workspaces.Disposals("7"); len(got) != 1 || got[0].Keep {
		t.Errorf("disposals = %+v, want the workspace disposed once and not kept", got)
	}
	if got := h.lastComment().PRURL; got != "https://example.test/pull/12" {
		t.Errorf("publish comment PRURL = %q, want the URL leg 3 found", got)
	}
	// The one negative here, and it covers less than it looks like it does: that
	// the `done` transition queues no release of its own. That much is sound
	// without a barrier, because nothing else could have issued one yet — the
	// §9.8 sweep decides on a tick and the manual clock fires none between the
	// verdict and here. It is not evidence about the sweep, though: a mutant
	// that releases a held claim whose issue is still open survives 30 runs of
	// this test, and internal/orchestrator's held tests are what reject it.
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; `done` retains the claim (SPEC §9.2)", n)
	}
}

// B09 acceptance 2. The agent reports success having committed nothing: the
// branch is still at the base pinned when the workspace was claimed. Evidence
// beats the account (§3.5), and there is nothing a continuation could finish
// publishing, so this parks for a human with everything intact.
func TestASuccessClaimWithZeroCommitsParksAndKeepsTheWorkspace(t *testing.T) {
	h := startE2E(t, func(ws core.Workspace) (core.PublishFacts, error) {
		return core.PublishFacts{Head: ws.BaseSHA, DescendsBase: true}, nil
	})
	h.waitState(orchestrator.StateNeedsReview)

	if h.reachedDone() {
		t.Fatalf("path = %v; a run with no commits must never reach done", h.o.Transitions.Path("7"))
	}
	if got := h.Runner.StartCount(); got != 1 {
		t.Errorf("started %d runs; a contradiction has nowhere to continue to", got)
	}
	// The git legs settle this locally. Asking the tracker would make a
	// verdict the repository already holds depend on the network (§9.7).
	if n := h.Tracker.FindPRReads(); n != 0 {
		t.Errorf("FindPR called %d times for a branch that never advanced", n)
	}

	h.waitForNeedsReviewComment()
	// The operator-facing line comes from the evidence check itself, so what a
	// human reads names the fact rather than the routing decision.
	if got := h.lastComment().Detail; !strings.Contains(got, "the run added no commits") {
		t.Errorf("needs-review detail = %q, want it to name the missing commits", got)
	}
	// SPEC §9.2/§6.4: parked keeps both the workspace and the claim.
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; needs-review keeps the workspace for debugging", got)
	}
	if n := h.Tracker.ReleaseCount("7"); n != 0 {
		t.Errorf("released %d times; needs-review retains the claim", n)
	}
}

// B09 acceptance 3. The branch was rewritten off its claim-time base, so
// "advanced" cannot be asserted about this daemon's commits — the history
// there may be anyone's. It does not verify even with an open pull request
// sitting on that very branch, which is the shape that would otherwise look
// exactly like a published run.
func TestARewrittenBranchDoesNotVerifyEvenWithAnOpenPR(t *testing.T) {
	h := startE2E(t, func(core.Workspace) (core.PublishFacts, error) {
		// What the worktree provider reports for this shape: leg 1 fails, so
		// origin is never probed and RemoteProbed stays false (§9.7's gate,
		// core.PublishFacts.AdvancedPastBase).
		return core.PublishFacts{Head: rewrittenSHA}, nil
	}, func(tr *fake.Tracker) {
		tr.SetPR("ben/issue-7", core.PR{Number: 13, URL: "https://example.test/pull/13", State: "open", Branch: "ben/issue-7", BaseBranch: "main"})
	})

	h.waitState(orchestrator.StateNeedsReview)

	if h.reachedDone() {
		t.Fatalf("path = %v; a rewritten branch must not verify", h.o.Transitions.Path("7"))
	}
	// Leg 1 decides, so the open PR is never even consulted — it cannot supply
	// evidence for commits that are not this daemon's.
	if n := h.Tracker.FindPRReads(); n != 0 {
		t.Errorf("FindPR called %d times; an open PR must not rescue a rewritten branch", n)
	}
	h.waitForNeedsReviewComment()
	if got := h.lastComment().Detail; !strings.Contains(got, "does not descend from its claim-time base") {
		t.Errorf("needs-review detail = %q, want it to name the rewritten history", got)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; the rewritten branch is exactly what a human needs to look at", got)
	}
}

// SPEC §9.7 leg 3 names an *open* pull request. A closed one on the same
// branch is a rejected earlier attempt, not evidence — and this is the shape
// where "is there a PR?" and "is there an open PR?" part company, since every
// other leg holds and a caller checking only for non-nil would publish.
func TestAClosedPRIsNotPublishEvidence(t *testing.T) {
	h := startE2E(t, pushedAndDescends, func(tr *fake.Tracker) {
		tr.SetPR("ben/issue-7", core.PR{Number: 11, URL: "https://example.test/pull/11", State: "closed", Branch: "ben/issue-7", BaseBranch: "main"})
	})

	// Incomplete, not published: the work exists and is pushed, so this takes
	// the continuation track rather than parking.
	if !h.Clock.BlockUntilWaiters(2) {
		t.Fatal("the continuation timer was never armed")
	}
	if h.reachedDone() {
		t.Fatalf("path = %v; a closed pull request must not satisfy leg 3", h.o.Transitions.Path("7"))
	}
	if n := h.Tracker.FindPRReads(); n == 0 {
		t.Error("leg 3 was never read; the git legs all held, so the tracker decides this one")
	}
}

func TestAWrongTargetPullRequestParksAsContradicted(t *testing.T) {
	h := startE2E(t, pushedAndDescends, func(tr *fake.Tracker) {
		tr.SetPR("ben/issue-7", core.PR{
			Number: 14, URL: "https://example.test/pull/14", State: "open",
			Branch: "ben/issue-7", BaseBranch: "unprotected",
		})
	})

	h.waitState(orchestrator.StateNeedsReview)
	h.waitForNeedsReviewComment()
	if h.reachedDone() {
		t.Fatalf("path = %v; a wrong-target pull request must not publish", h.o.Transitions.Path("7"))
	}
	if got := h.lastComment().Detail; !strings.Contains(got, "targets unprotected, not main") {
		t.Fatalf("needs-review detail = %q, want the target contradiction", got)
	}
}

func TestAmbiguousPullRequestsParkWithoutSelectingEither(t *testing.T) {
	h := startE2E(t, pushedAndDescends, func(tr *fake.Tracker) {
		tr.SetFailFindPR(core.ErrPRAmbiguous)
	})

	h.waitState(orchestrator.StateNeedsReview)
	h.waitForNeedsReviewComment()
	if h.reachedDone() {
		t.Fatalf("path = %v; ambiguous pull requests must not publish", h.o.Transitions.Path("7"))
	}
	if got := h.lastComment().Detail; !strings.Contains(got, core.ErrPRAmbiguous.Error()) {
		t.Fatalf("needs-review detail = %q, want ErrPRAmbiguous", got)
	}
}

// SPEC §9.7 fails closed: a verification that could not be completed is never
// success. The git legs hold here and only the tracker read fails, which is
// the case where guessing would be most tempting and most expensive — every
// local fact says published.
func TestAnUnreadableLegParksRatherThanPublishing(t *testing.T) {
	h := startE2E(t, pushedAndDescends, func(tr *fake.Tracker) {
		tr.SetFailFindPR(errors.New("502 from the tracker"))
	})

	h.waitState(orchestrator.StateNeedsReview)

	if h.reachedDone() {
		t.Fatalf("path = %v; an unreadable leg is not a leg that held", h.o.Transitions.Path("7"))
	}
	h.waitForNeedsReviewComment()
	if got := h.lastComment().Detail; !strings.Contains(got, "502 from the tracker") {
		t.Errorf("needs-review detail = %q, want the unreadable leg's error carried to the operator", got)
	}
	if got := h.Workspaces.Disposals("7"); len(got) != 0 {
		t.Errorf("disposed %+v; nothing was proven, so nothing may be thrown away", got)
	}
}
