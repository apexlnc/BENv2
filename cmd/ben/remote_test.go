package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/claudecode"
	"github.com/srhg-ai-7cef3f93/ben/internal/airlock"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
	"github.com/srhg-ai-7cef3f93/ben/internal/verify"
)

// The v2 dispatch path end to end, over contract fakes and the *real* loop.
//
// It is here for the reason the v1 acceptance suite is here: the pieces are
// strangers by design — the workspace strategy, the durable process boundary,
// the daemon-side evidence store and the two verdict enums — and this assembly
// is the only place they meet. What each of them does on its own is proven in
// its own package; what is provable only here is that a claim goes from the
// queue to `done` through them, and that the two things a remote claim must
// *not* do do not happen.
//
// Nothing is exec'd and nothing is written to a worktree, which is not an
// accident of the fakes: the assertions below name a local directory and a
// process count, so a strategy that quietly prepared one or launched one fails
// here rather than in production.

const remoteWorkflow = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben-queue"]
agent:
  kind: claude-code
  provider:
    permission_mode: bypassPermissions
    api_key: agent-key
limits:
  max_concurrent_agents: 2
  max_turns: 4
  max_attempts: 3
deployment:
  mode: attended
---
Work issue {{ issue.identifier }}: {{ issue.title }}. Target {{ target_branch }}.
`

var remoteEpoch = time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC)

// remoteE2E is the assembly under test.
type remoteE2E struct {
	t             *testing.T
	o             *orchestrator.Orchestrator
	Tracker       *fake.Tracker
	Backend       *remotetest.Backend
	Mirror        *fake.Mirror
	Forge         *fake.Forge
	Provider      *remotews.Provider
	Journals      *remote.DirStore
	Consumer      *remote.DirConsumer
	WorktreeAt    string
	GitRepository string

	done chan error
}

const remoteProfileRev = "profile-rev-1"

// remoteRunID is the run identity the loop composes for the first attempt of the
// first record, under a stated instance. Deterministic, which is what lets this
// test script the backend's stream for a run it did not start.
const remoteRunID = remote.RunID("7-e2e-1.0")

func startRemoteE2E(t *testing.T, publishSucceeds bool) *remoteE2E {
	t.Helper()
	dir := t.TempDir()
	h := &remoteE2E{
		t:        t,
		Tracker:  fake.NewTracker(fake.Issue("7", remoteEpoch)),
		Backend:  remotetest.New(remoteProfileRev),
		Mirror:   fake.NewMirror(),
		Forge:    fake.NewForge(),
		Journals: remote.NewDirStore(filepath.Join(dir, "journal")),
		Consumer: remote.NewDirConsumer(filepath.Join(dir, "journal")),
		// A directory nothing in the remote assembly is configured with. It stands
		// for `workspace.root`, and the point is that it stays empty.
		WorktreeAt: filepath.Join(dir, "worktrees"),
		done:       make(chan error, 1),
	}
	if err := os.MkdirAll(h.WorktreeAt, 0o700); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	def, err := config.Load(writeWorkflowContent(t, remoteWorkflow))
	if err != nil {
		t.Fatalf("loading the remote workflow: %v", err)
	}
	owner, repoName, err := configuredRepository(def)
	if err != nil {
		t.Fatalf("resolving the configured repository: %v", err)
	}
	h.GitRepository = owner + "/" + repoName

	provider, err := remotews.New(remotews.Options{
		Repository:    h.Mirror.Repository(),
		GitRepository: h.GitRepository,
		Workspaces:    h.Backend.Workspaces(),
		Processes:     h.Backend,
		Journals:      h.Journals,
		Consumptions:  h.Consumer,
		HookExec:      h.Backend,
		HookStore:     remote.NewHookDirStore(filepath.Join(dir, "journal")),
		Hooks:         remote.Hooks{Timeout: 30 * time.Second},
		Base:          h.Mirror,
		Cycles:        &trackerCycles{tracker: h.Tracker, required: def.Config.Tracker.RequiredLabels},
		Store:         remotews.NewDirStore(filepath.Join(dir, "cycles")),
		Disposer:      &recordingDisposer{},
		Logger:        log,
	})
	if err != nil {
		t.Fatalf("remotews.New: %v", err)
	}
	h.Provider = provider
	h.Backend.SetHookSpecResult(func(spec remote.HookSpec) (remote.HookResult, error) {
		if spec.Git.Phase != remote.GitPhasePublish {
			return remote.HookResult{}, nil
		}
		if !publishSucceeds {
			return remote.HookResult{ExitCode: 1, Output: "broker unavailable"}, nil
		}
		head := h.publish()
		return remote.HookResult{Output: fmt.Sprintf(
			`{"status":"published","pr_url":"https://example.test/pull/1","branch":%q,"head_sha":%q}`,
			spec.Git.Branch, head,
		)}, nil
	})

	// The real provider adapter composes the argv and parses the stream is the
	// scripted one: the two are independent boundaries, and pairing them this way
	// is what lets the captured request be asserted against claude-code's real
	// command while the run's output stays a fixture.
	runner, err := remote.New(remote.Options{
		Backend:  h.Backend,
		Store:    h.Journals,
		Consumer: h.Consumer,
		Bind: func(spec core.RunSpec) (remote.Binding, error) {
			id, gitScope, err := provider.Bind(context.Background(), spec)
			if err != nil {
				return remote.Binding{}, err
			}
			return remote.Binding{Identity: id, Run: remote.RunID(spec.Env["BEN_RUN_ID"]), Git: gitScope}, nil
		},
		Invoke: func(spec core.RunSpec) (remote.Invocation, error) {
			inv, err := claudecode.Kind{}.RemoteInvocation(def.Config.AgentBinding(), spec)
			if err != nil {
				return remote.Invocation{}, err
			}
			return remote.Invocation{Argv: inv.Argv, Env: inv.Env, Stdin: inv.Stdin}, nil
		},
		Translate:    remotetest.Translate,
		Capabilities: claudecode.Kind{}.RemoteCapabilities(),
	})
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}

	checker, err := verify.NewRemote(h.Mirror, h.Mirror, h.Forge, verify.RemoteExpectation{
		Repository: h.Mirror.Repository(),
	})
	if err != nil {
		t.Fatalf("verify.NewRemote: %v", err)
	}

	o, err := orchestrator.New(orchestrator.Config{
		Runtime: config.NewRuntimeSource(def, &orchestrator.Bundle{
			Definition:     def,
			Tracker:        h.Tracker,
			Workspaces:     provider,
			Runner:         runner,
			Verifier:       &remoteVerifier{checker: checker, runs: provider, publish: provider},
			ClaimPrincipal: fake.DefaultPrincipal,
		}),
		PrepRetryable: prepRetryable,
		Log:           log,
		DaemonID:      "test-host/remote",
		Instance:      "e2e",
	})
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	h.o = o

	ctx, cancel := context.WithCancel(context.Background())
	if err := o.Recover(ctx); err != nil {
		cancel()
		t.Fatalf("Recover: %v", err)
	}
	go func() { h.done <- o.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("orchestrator did not stop within 5s")
		}
	})
	return h
}

// runRemoteAgent waits for the loop to dispatch, lets the caller state what the
// run did to the world, and then scripts the run's whole life: a clean session,
// a stream that ends, and — separately — a positively quiet execution domain.
//
// `during` runs after the dispatch rather than before it, and that ordering is
// the point rather than a convenience. The claim's trusted base is pinned inside
// PrepareClaim, from the canonical remote as it reads at that moment; a
// publication staged earlier would be *in* the base, and leg 1 would rightly
// report that the run added no commits.
func (h *remoteE2E) runRemoteAgent(during func()) {
	h.t.Helper()
	h.waitFor("the remote dispatch", func() bool { return h.Backend.Live(remoteRunID) })
	if during != nil {
		during()
	}
	h.Backend.Emit(remoteRunID, remotetest.Init("session-A"), remotetest.Text("working"), remotetest.Success())
	h.Backend.Complete(remoteRunID)
	h.Backend.Quiet(remoteRunID)
}

func (h *remoteE2E) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("timed out waiting for %s (path: %v)", what, h.o.Transitions.Path("7"))
}

func (h *remoteE2E) waitState(want orchestrator.State) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("issue 7 to reach %v", want), func() bool {
		for _, s := range h.o.Status() {
			if s.Identifier == "7" && s.State == want {
				return true
			}
		}
		return false
	})
}

// waitMilestone uses the write an assertion actually reads as its barrier.
// State transitions precede their asynchronously queued tracker effects, so a
// state wait alone can still observe the previous (usually claimed) comment.
func (h *remoteE2E) waitMilestone(want core.Milestone) core.MilestoneComment {
	h.t.Helper()
	var last core.MilestoneComment
	h.waitFor(fmt.Sprintf("issue 7 to post the %s milestone", want), func() bool {
		comments := h.Tracker.CommentsFor("7")
		if len(comments) == 0 {
			return false
		}
		last = comments[len(comments)-1]
		return last.Milestone == want
	})
	return last
}

// publish moves the canonical remote and opens the pull request that joins it —
// the daemon-side facts, and the only thing that can route a claim to `done`.
func (h *remoteE2E) publish() string {
	h.t.Helper()
	head := h.Mirror.Commit(fake.RemoteBranch(remotews.Key("7")))
	h.Forge.Open(core.RemotePR{
		HeadBranch: fake.RemoteBranch(remotews.Key("7")), HeadSHA: head, BaseBranch: "main",
		LinkStatus: core.RemoteIssueLinkStated, LinkedIssues: []string{"7"},
	})
	return head
}

// noLocalWorkspace is the acceptance criterion, asserted against a directory the
// remote assembly is not configured with: nothing in the path may create one.
func (h *remoteE2E) noLocalWorkspace() {
	h.t.Helper()
	entries, err := os.ReadDir(h.WorktreeAt)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(entries) != 0 {
		h.t.Fatalf("the remote path created %d entries under the local workspace root", len(entries))
	}
}

// A remote claim goes queue → done through the real loop, and the only evidence
// that got it there is daemon-side.
func TestARemoteClaimReachesDoneThroughDaemonSideEvidence(t *testing.T) {
	h := startRemoteE2E(t, true)
	h.runRemoteAgent(nil)
	h.waitState(orchestrator.StateDone)
	last := h.waitMilestone(core.MilestonePublished)
	head := h.Mirror.Head(fake.RemoteBranch(remotews.Key("7")))

	h.noLocalWorkspace()

	// No local agent process. The one dispatch went to the backend, and the
	// request it carried is claude-code's own command with no launcher in front
	// of it and no host path inside it.
	if got := h.Backend.RunCreations(); got != 1 {
		t.Fatalf("the backend ran %d processes, want 1", got)
	}
	spec, ok := h.Backend.Spec(remoteRunID)
	if !ok {
		t.Fatal("the backend captured no request")
	}
	argv := strings.Join(spec.Argv, " ")
	if spec.Argv[0] != claudecode.DefaultBinary {
		t.Fatalf("argv[0] is %q, want the provider binary", spec.Argv[0])
	}
	for _, want := range []string{"-p", "--output-format stream-json", "--verbose", "--permission-mode bypassPermissions"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("argv %q is missing %q", argv, want)
		}
	}
	for _, forbidden := range []string{claudecode.DefaultSandboxBinary, h.WorktreeAt, "settings.json"} {
		if strings.Contains(argv, forbidden) {
			t.Fatalf("argv %q carries the host-local %q", argv, forbidden)
		}
	}
	// The prompt reached the sandbox on stdin, and the run's coordinates reached
	// it as BEN_ variables — without BEN_WORKSPACE, which would state a working
	// directory BEN does not know.
	if !strings.Contains(string(spec.Stdin), "Work issue 7") || !strings.Contains(string(spec.Stdin), "Target main") {
		t.Fatalf("the prompt did not reach the request: %q", spec.Stdin)
	}
	if spec.Env["BEN_ISSUE"] != "7" || spec.Env["BEN_BRANCH"] != remotews.Branch(remotews.Key("7")) {
		t.Fatalf("the run's coordinates are %v", spec.Env)
	}
	if _, ok := spec.Env["BEN_WORKSPACE"]; ok {
		t.Fatalf("the request states a sandbox working directory: %v", spec.Env)
	}
	wantCodingScope := remote.GitScope{
		Phase: remote.GitPhaseCoding, Repository: h.GitRepository,
		Branch: remotews.Branch(remotews.Key("7")), BaseCommit: spec.Identity.BaseSHA, BaseBranch: "main",
	}
	if spec.Git != wantCodingScope {
		t.Fatalf("coding Git scope = %+v, want %+v", spec.Git, wantCodingScope)
	}
	var prepare, publish *remotetest.HookCall
	for _, call := range h.Backend.Hooks() {
		call := call
		switch call.Phase {
		case remote.HookGitPrepare:
			prepare = &call
		case remote.HookGitPublish:
			publish = &call
		}
	}
	if prepare == nil || publish == nil {
		t.Fatalf("typed Git phases = %+v, want prepare and publish", h.Backend.Hooks())
	}
	if prepare.ID == publish.ID || publish.Git.Operation == "" || publish.Git.Phase != remote.GitPhasePublish {
		t.Fatalf("phase identities/scopes are not distinct and complete: prepare=%+v publish=%+v", prepare, publish)
	}

	// The publication milestone names the pull request the daemon read, not
	// anything the run said about itself.
	if got := h.Tracker.Label("7"); got != core.StateLabelNone {
		t.Fatalf("state label = %q, want none once the claim is done", got)
	}
	if last.Milestone != core.MilestonePublished || !strings.Contains(last.PRURL, "/pull/") {
		t.Fatalf("last milestone = %+v, want the published one", last)
	}
	if h.Mirror.Head(fake.RemoteBranch(remotews.Key("7"))) != head {
		t.Fatal("the fixture's canonical head moved")
	}
}

// A backend success is not evidence of a publication. With nothing on the
// canonical remote, the same clean run parks for a human instead of publishing —
// which is the whole of #193 applied to this path.
func TestABackendSuccessAloneCannotReachDoneWhenTrustedPublishFails(t *testing.T) {
	h := startRemoteE2E(t, false)
	h.runRemoteAgent(nil)
	h.waitState(orchestrator.StateNeedsReview)
	last := h.waitMilestone(core.MilestoneNeedsReview)

	h.noLocalWorkspace()
	for _, s := range h.o.Transitions.Path("7") {
		if s == orchestrator.StateDone {
			t.Fatalf("path = %v; a run that published nothing reached done", h.o.Transitions.Path("7"))
		}
	}
	if last.Milestone != core.MilestoneNeedsReview {
		t.Fatalf("last milestone = %+v, want needs-review", last)
	}
	if !strings.Contains(last.Detail, "trusted publication") {
		t.Fatalf("the park does not say why: %q", last.Detail)
	}
}

func TestARemoteWrongTargetPullRequestReachesNeedsReview(t *testing.T) {
	h := startRemoteE2E(t, true)
	h.Backend.SetHookSpecResult(func(spec remote.HookSpec) (remote.HookResult, error) {
		if spec.Git.Phase != remote.GitPhasePublish {
			return remote.HookResult{}, nil
		}
		head := h.Mirror.Commit(fake.RemoteBranch(remotews.Key("7")))
		h.Forge.Open(core.RemotePR{
			HeadBranch: fake.RemoteBranch(remotews.Key("7")), HeadSHA: head,
			BaseBranch: "unprotected", LinkStatus: core.RemoteIssueLinkStated,
			LinkedIssues: []string{"7"},
		})
		return remote.HookResult{Output: fmt.Sprintf(
			`{"status":"published","pr_url":"https://example.test/pull/1","branch":%q,"head_sha":%q}`,
			spec.Git.Branch, head,
		)}, nil
	})
	h.runRemoteAgent(nil)
	h.waitState(orchestrator.StateNeedsReview)
	last := h.waitMilestone(core.MilestoneNeedsReview)
	if !strings.Contains(last.Detail, "targets unprotected, not main") {
		t.Fatalf("needs-review detail = %q, want target contradiction", last.Detail)
	}
	for _, state := range h.o.Transitions.Path("7") {
		if state == orchestrator.StateDone {
			t.Fatalf("path = %v; wrong-target remote PR reached done", h.o.Transitions.Path("7"))
		}
	}
}

// recordingDisposer stands in for the backend's retention policy. The policy
// itself is airlock's; what this proves is that the strategy reaches it.
type recordingDisposer struct {
	calls []remotews.Outcome
}

func (d *recordingDisposer) Complete(
	_ context.Context, _ remote.Claim, outcome remotews.Outcome, prev remote.Status,
) (remotews.Disposition, error) {
	if !remote.MayReuse(prev) {
		return remotews.DispositionRetained, remote.ErrNotQuiet
	}
	d.calls = append(d.calls, outcome)
	return remotews.DispositionRetained, nil
}

type substrateCompleterFunc func(
	context.Context, remote.Claim, airlock.Outcome, remote.Status,
) (airlock.Disposal, error)

func (f substrateCompleterFunc) Complete(
	ctx context.Context, claim remote.Claim, outcome airlock.Outcome, prev remote.Status,
) (airlock.Disposal, error) {
	return f(ctx, claim, outcome, prev)
}

// The assembly owns both translations: why the strategy is finishing and
// whether the configured Airlock action left its workspace addressable. Anchor
// every member here so either closed enum growing cannot silently fall through.
func TestSubstrateDisposerMapsEveryOutcomeAndDisposition(t *testing.T) {
	for _, tc := range []struct {
		name            string
		outcome         remotews.Outcome
		wantOutcome     airlock.Outcome
		disposal        airlock.Disposal
		wantDisposition remotews.Disposition
	}{
		{"published retains", remotews.OutcomePublished, airlock.OutcomePublished,
			airlock.DisposalRetain, remotews.DispositionRetained},
		{"failure suspends", remotews.OutcomeFailed, airlock.OutcomeFailed,
			airlock.DisposalSuspend, remotews.DispositionRetained},
		{"revocation deletes", remotews.OutcomeRevoked, airlock.OutcomeRevoked,
			airlock.DisposalDelete, remotews.DispositionDeleted},
		{"shutdown retains", remotews.OutcomeShutdown, airlock.OutcomeShutdown,
			airlock.DisposalRetain, remotews.DispositionRetained},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			d := substrateDisposer{substrate: substrateCompleterFunc(func(
				_ context.Context, _ remote.Claim, got airlock.Outcome, _ remote.Status,
			) (airlock.Disposal, error) {
				called = true
				if got != tc.wantOutcome {
					t.Fatalf("backend outcome = %s, want %s", got, tc.wantOutcome)
				}
				return tc.disposal, nil
			})}
			got, err := d.Complete(context.Background(), remote.Claim{}, tc.outcome, remote.Status{})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if !called {
				t.Fatal("the backend completion was not called")
			}
			if got != tc.wantDisposition {
				t.Fatalf("disposition = %s, want %s", got, tc.wantDisposition)
			}
		})
	}
}
