package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/claudecode"
	"github.com/srhg-ai-7cef3f93/ben/internal/airlock"
	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
)

// The v2 substrate leg of assembly. What is provable here is the wiring — that a
// declaration becomes a backend, and that its readiness is proven before
// anything depends on it. The backend's own behaviour is proven against the
// contract fake in internal/airlock, and the dispatch path it feeds in
// remote_test.go.

const airlockWorkflow = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
    token: $TRACKER_PAT
    claim_assignee: ben-bot
  required_labels: ["ben"]
agent:
  kind: claude-code
credential_sources:
  airlock:
    kind: static
    value: $AIRLOCK_TOKEN
substrate:
  kind: airlock
  airlock:
    base_url: https://airlock.invalid
    profile: ` + airlocktest.DefaultProfile + `
    auth_source: airlock
deployment:
  mode: attended
---
Do the work described in {{ issue.title }}.
`

func writeAirlockWorkflow(t *testing.T, content string) *config.WorkflowDefinition {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	def, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return def
}

// staticSource is the constructed credential the assembly hands the backend.
type staticSource struct{ value string }

func (s staticSource) Fetch(context.Context, core.Purpose) (core.Token, error) {
	return core.Token{Value: s.value}, nil
}
func (s staticSource) FetchFresh(context.Context, core.Purpose) (core.Token, error) {
	return core.Token{Value: s.value}, nil
}
func (s staticSource) Descriptor() core.SourceDescriptor {
	return core.SourceDescriptor{
		Kind: "static", Authority: "env:AIRLOCK_TOKEN", BindingKey: "env:AIRLOCK_TOKEN",
	}
}

func substrateBuilder(t *testing.T, srv *airlocktest.Server) *builder {
	t.Helper()
	return substrateBuilderAt(t, srv, state.At(t.TempDir()))
}

func substrateBuilderAt(t *testing.T, srv *airlocktest.Server, dir state.Dir) *builder {
	t.Helper()
	b := newBuilder(slog.New(slog.NewTextHandler(io.Discard, nil)), dir)
	if srv != nil {
		b.substrateTransport = srv.Transport()
	}
	return b
}

// A local substrate is what every existing workflow declares, and this leg must
// do nothing at all for one — no backend, no credential, no request.
func TestReadySubstrateIsANoOpForTheLocalSubstrate(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	def := writeAirlockWorkflow(t, strings.Replace(airlockWorkflow, `substrate:
  kind: airlock
  airlock:
    base_url: https://airlock.invalid
    profile: `+airlocktest.DefaultProfile+`
    auth_source: airlock
`, "", 1))
	if def.Config.Substrate.Remote() {
		t.Fatal("the fixture is not local")
	}
	substrate, err := substrateBuilder(t, nil).readySubstrate(context.Background(), def, nil)
	if err != nil {
		t.Fatalf("readySubstrate on a local workflow: %v", err)
	}
	if substrate != nil {
		t.Fatal("a local workflow constructed a remote backend")
	}
}

// A reachable, approved configuration now yields a backend rather than a scope
// refusal: this is the assertion that #205 removed the dispatch block, and the
// one that would fail if it came back.
func TestReadySubstrateBuildsAReachableBackend(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	srv := airlocktest.New(t)
	def := writeAirlockWorkflow(t, airlockWorkflow)

	substrate, err := substrateBuilder(t, srv).readySubstrate(context.Background(), def,
		staticSource{value: airlocktest.DefaultToken})
	if err != nil {
		t.Fatalf("readySubstrate: %v", err)
	}
	if substrate == nil {
		t.Fatal("a reachable substrate produced no backend")
	}
	if substrate.Workspaces == nil || substrate.Processes == nil || substrate.Hooks == nil {
		t.Fatal("the backend is missing one of the three #192 seams")
	}
}

// A startRun may commit while every response is lost. The Airlock binding then
// carries a write-ahead fence but no permanent run id. Startup recovers the
// exact active run from the sandbox's durable slot and must not turn that claim
// into ErrNotReady for the whole daemon.
func TestStartupReconciliationRecoversAnUnansweredStartAndStartsTheDaemon(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	srv := airlocktest.New(t)
	workflow := strings.Replace(airlockWorkflow,
		"    profile: "+airlocktest.DefaultProfile,
		"    profile: "+airlocktest.DefaultProfile+"\n    max_retries: 1", 1)
	def := writeAirlockWorkflow(t, workflow)
	dir := state.At(t.TempDir())
	b := substrateBuilderAt(t, srv, dir)
	substrate, err := b.readySubstrate(context.Background(), def,
		staticSource{value: airlocktest.DefaultToken})
	if err != nil {
		t.Fatalf("readySubstrate: %v", err)
	}

	claim := remote.Claim{Repository: "acme/widgets", Issue: "238", Epoch: 4242}
	id, err := substrate.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: claim, Branch: "ben/238",
		BaseSHA: "0f5d3c1b9a7e6d4c2b0a8f6e4d2c0b9a8f6e4d2c",
	})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	request := remote.ProcessSpec{Identity: id, Argv: []string{"agent"}}
	digest, err := remote.ProcessRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	ref := remote.ProcessRef{Identity: id, RunID: "run-238", RequestDigest: digest}
	journal, err := remote.Reserve(context.Background(), remote.NewDirStore(b.journalDir), ref, remote.Meta{})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	path := "/v2/sandboxes/" + id.SandboxID + "/runs"
	// Keep a surplus over the client's two attempts: net/http may itself retry
	// an idempotency-keyed request when an already-used connection aborts.
	for range 8 {
		srv.DropNextResponse("POST", path)
	}
	if _, err := journal.Dispatch(context.Background(), func(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
		return substrate.Processes.Start(ctx, ref, request)
	}); err == nil {
		t.Fatal("every lost response unexpectedly produced a resolved Start")
	}
	if got := srv.RunIDs(); len(got) != 1 {
		t.Fatalf("lost Start responses created %d runs, want exactly one: %v", len(got), got)
	}

	// Reconstruct every client and store handle from the same state directory.
	// Nothing from the process that lost the response may be needed to classify
	// the durable fence, or this test would not cover daemon restart.
	restartedBuilder := substrateBuilderAt(t, srv, dir)
	restarted, err := restartedBuilder.readySubstrate(context.Background(), def,
		staticSource{value: airlocktest.DefaultToken})
	if err != nil {
		t.Fatalf("readySubstrate after restart: %v", err)
	}
	states, err := restarted.Reconcile(context.Background(), remote.NewDirStore(restartedBuilder.journalDir))
	if err != nil || len(states) != 1 {
		t.Fatalf("Reconcile = %+v, %v; want one retained claim", states, err)
	}
	runID := srv.RunIDs()[0]
	if states[0].Err != nil || states[0].StartUnresolved || states[0].Lease != remote.LeaseRunning ||
		states[0].Status.BackendRunID != runID {
		t.Fatalf("unanswered Start reconciled to %+v, want adopted active run %s", states[0], runID)
	}
	if err := restartedBuilder.reconcile(context.Background(), restarted); err != nil {
		t.Fatalf("daemon startup refused an unanswered Start: %v", err)
	}

	// The adopted id is permanent: once the run becomes quiet, the same
	// restarted backend observes ordinary termination evidence and releases the
	// running lease without replaying a request body it did not retain.
	srv.Terminate(runID, airlocktest.Exited(0))
	states, err = restarted.Reconcile(context.Background(), remote.NewDirStore(restartedBuilder.journalDir))
	if err != nil || len(states) != 1 || !remote.MayReuse(states[0].Status) || states[0].Lease != remote.LeaseHeld {
		t.Fatalf("recovered run after quiet = %+v, %v; want a reusable held claim", states, err)
	}
}

// Remote-only provider refusals are structural: build must return them before
// credential construction or backend readiness, leaving no bundle a daemon
// could dispatch through and making no Airlock request at all.
func TestRemoteCredentialRefusesBeforeAirlockReadiness(t *testing.T) {
	const secret = "ghp-reusable-github-credential-MUST-NOT-CROSS"
	t.Setenv("TRACKER_PAT", "tracker-secret")
	t.Setenv("GH_TOKEN", secret)
	provider := `agent:
  kind: claude-code
  provider:
    permission_mode: bypassPermissions
    model: $GH_TOKEN
`
	workflow := strings.Replace(airlockWorkflow, "agent:\n  kind: claude-code\n", provider, 1)
	def := writeAirlockWorkflow(t, workflow)
	srv := airlocktest.New(t)

	bundle, err := substrateBuilder(t, srv).build(context.Background(), def, nil, everything)
	if !errors.Is(err, ErrStructural) || !errors.Is(err, claudecode.ErrRemoteGitHubCredential) {
		t.Fatalf("build = (%v, %v), want structural remote-credential refusal", bundle, err)
	}
	if bundle != nil {
		t.Fatalf("refused build returned bundle %v", bundle)
	}
	if requests := srv.Requests(); len(requests) != 0 {
		t.Fatalf("structurally refused build made %d Airlock requests: %+v", len(requests), requests)
	}
}

// A provider.env value is already resolved by the time the adapter sees it, so
// this regression plants a real secret sentinel in the workflow and drives the
// real adapter/Runner/Airlock HTTP boundary. The adapter must refuse before a
// startRun request exists, and the contract server's raw request capture proves
// the sentinel did not cross under another field or spelling either.
func TestReusableGitHubCredentialNeverReachesAnAirlockRequest(t *testing.T) {
	const secret = "ghp-reusable-github-credential-MUST-NOT-CROSS"
	t.Setenv("TRACKER_PAT", "tracker-secret")
	t.Setenv("BEN_TEST_REMOTE_GITHUB_PAT", secret)
	t.Setenv("GH_TOKEN", secret)
	for _, tc := range []struct {
		name          string
		providerEntry string
	}{
		{
			name:          "credential destination",
			providerEntry: "    env:\n      GH_TOKEN: $BEN_TEST_REMOTE_GITHUB_PAT\n",
		},
		{
			name:          "credential source renamed in environment",
			providerEntry: "    env:\n      AGENT_FLAG: $GH_TOKEN\n",
		},
		{
			name:          "credential source bound to argv",
			providerEntry: "    model: $GH_TOKEN\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := `agent:
  kind: claude-code
  provider:
    permission_mode: bypassPermissions
    api_key: agent-key
`
			workflow := strings.Replace(airlockWorkflow, "agent:\n  kind: claude-code\n", provider+tc.providerEntry, 1)
			def := writeAirlockWorkflow(t, workflow)
			srv := airlocktest.New(t)
			substrate, err := substrateBuilder(t, srv).readySubstrate(
				context.Background(), def, staticSource{value: airlocktest.DefaultToken},
			)
			if err != nil {
				t.Fatalf("readySubstrate: %v", err)
			}

			claim := remote.Claim{Repository: "acme/widgets", Issue: "194", Epoch: 4242}
			id, err := substrate.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
				Claim: claim, Branch: "ben/194",
				BaseSHA: "0f5d3c1b9a7e6d4c2b0a8f6e4d2c0b9a8f6e4d2c",
			})
			if err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			dir := t.TempDir()
			runner, err := remote.New(remote.Options{
				Backend:  substrate.Processes,
				Store:    remote.NewDirStore(filepath.Join(dir, "journal")),
				Consumer: remote.NewDirConsumer(filepath.Join(dir, "journal")),
				Bind: func(core.RunSpec) (remote.Binding, error) {
					return remote.Binding{Identity: id, Run: "run-1"}, nil
				},
				Invoke: func(spec core.RunSpec) (remote.Invocation, error) {
					inv, err := claudecode.Kind{}.RemoteInvocation(def.Config.AgentBinding(), spec)
					return remote.Invocation{Argv: inv.Argv, Env: inv.Env, Stdin: inv.Stdin}, err
				},
				Translate: func([]byte) []core.Event { return nil },
			})
			if err != nil {
				t.Fatalf("remote.New: %v", err)
			}
			handle, startErr := runner.Start(context.Background(), core.RunSpec{
				Prompt: "work issue 194",
				Env:    map[string]string{"BEN_ISSUE": "194", "BEN_RUN_ID": "run-1"},
			})
			if handle != nil {
				handle.Stop(context.Background(), core.StopDiscard)
				t.Error("a forbidden remote credential still produced a run handle")
			}
			if !errors.Is(startErr, claudecode.ErrRemoteGitHubCredential) {
				t.Fatalf("Start = %v, want %v", startErr, claudecode.ErrRemoteGitHubCredential)
			}

			runRequests := 0
			for _, request := range srv.Requests() {
				if bytes.Contains(request.Body, []byte(secret)) || strings.Contains(request.Auth, secret) {
					t.Fatalf("%s %s serialized the reusable GitHub credential", request.Method, request.Path)
				}
				if request.Method == "POST" && strings.HasSuffix(request.Path, "/runs") {
					runRequests++
				}
			}
			if runRequests != 0 {
				t.Fatalf("the refused invocation made %d startRun requests, want none", runRequests)
			}
		})
	}
}

// An unusable backend is refused, and the refusal names what the operator can
// act on: their endpoint, their credential, their profile.
func TestReadySubstrateReportsAnUnusableBackend(t *testing.T) {
	t.Setenv("TRACKER_PAT", "tracker-secret")
	srv := airlocktest.New(t)
	def := writeAirlockWorkflow(t, airlockWorkflow)

	for _, tc := range []struct {
		name  string
		setup func()
		cred  core.CredentialSource
		want  error
	}{
		{
			name: "the token is rejected",
			cred: staticSource{value: "not-the-token"},
			want: ErrNotReady,
		},
		{
			name:  "the profile has been withdrawn",
			setup: func() { srv.SetProfile(airlocktest.DefaultProfile, "withdrawn", airlocktest.Revision("v1")) },
			cred:  staticSource{value: airlocktest.DefaultToken},
			want:  ErrNotReady,
		},
		{
			name: "no credential could be constructed",
			cred: nil,
			want: ErrSubstrateCredential,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setup != nil {
				tc.setup()
			}
			_, err := substrateBuilder(t, srv).readySubstrate(context.Background(), def, tc.cred)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSubstrateTLSReadsAPrivateCA(t *testing.T) {
	t.Parallel()
	if cfg, err := substrateTLS(""); err != nil || cfg != nil {
		t.Fatalf("an unset CA file produced %+v, %v — want the host's own roots", cfg, err)
	}
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.pem")
	if _, err := substrateTLS(missing); err == nil {
		t.Fatal("a missing CA bundle was accepted")
	}
	junk := filepath.Join(dir, "junk.pem")
	if err := os.WriteFile(junk, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := substrateTLS(junk); err == nil || !strings.Contains(err.Error(), "PEM") {
		t.Fatalf("a CA bundle with no certificates produced %v", err)
	}
}

func TestDisposalKeywordsMapOntoTheBackendSet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		keyword string
		want    airlock.Disposal
	}{
		{config.DisposalRetain, airlock.DisposalRetain},
		{config.DisposalSuspend, airlock.DisposalSuspend},
		{config.DisposalDelete, airlock.DisposalDelete},
		// Unreachable through a loaded workflow, and stated because retain is
		// the only safe answer for a keyword nobody could name.
		{"archive", airlock.DisposalRetain},
		{"", airlock.DisposalRetain},
	} {
		if got := disposalOf(tc.keyword); got != tc.want {
			t.Fatalf("disposalOf(%q) = %s, want %s", tc.keyword, got, tc.want)
		}
	}
	// Every keyword the loader accepts is mapped: a disposal the config admits
	// and the assembly silently retained would be a policy nobody applied.
	for _, keyword := range config.Disposals() {
		if keyword != config.DisposalRetain && disposalOf(keyword) == airlock.DisposalRetain {
			t.Fatalf("the loader accepts %q and the assembly maps it to retain", keyword)
		}
	}
}
