package airlock

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

func TestStartIsIdempotentForOneDispatchAddress(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))

	first, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	second, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("replayed Start: %v", err)
	}
	if first.BackendRunID != second.BackendRunID {
		t.Fatalf("replay returned %s, first returned %s", second.BackendRunID, first.BackendRunID)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("a replayed start produced %d runs, want 1: %v", len(got), got)
	}
	posts := 0
	for _, request := range f.srv.Requests() {
		if request.Method == "POST" && request.Path == "/v2/sandboxes/"+id.SandboxID+"/runs" {
			posts++
		}
	}
	if posts != 1 {
		t.Fatalf("a known run id caused %d startRun requests, want 1 total", posts)
	}
}

// The invocation environment has already crossed the adapter boundary when it
// reaches Processes. It includes the provider's explicit env and API-key
// surface as well as BEN's coordinates; all three must reach the real Airlock
// request unchanged.
func TestStartCarriesTheComposedProviderEnvironment(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	request := spec(id)
	request.Env = map[string]string{
		"AGENT_FLAG":        "on",
		"ANTHROPIC_API_KEY": "agent-key",
		"BEN_ISSUE":         "194",
	}
	ref := mustRef(t, id, "run-1", request)

	if _, err := f.sub.Processes.Start(ctx, ref, request); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var body startRunRequest
	for _, sent := range f.srv.Requests() {
		if sent.Method != "POST" || sent.Path != "/v2/sandboxes/"+id.SandboxID+"/runs" {
			continue
		}
		if err := json.Unmarshal(sent.Body, &body); err != nil {
			t.Fatalf("decoding startRun request: %v", err)
		}
	}
	if len(body.Env) != len(request.Env) {
		t.Fatalf("request env = %v, want %v", body.Env, request.Env)
	}
	for name, want := range request.Env {
		if got := body.Env[name]; got != want {
			t.Fatalf("request env[%s] = %q, want %q", name, got, want)
		}
	}
}

func TestStartCarriesTypedCodingScopeAsRunLabels(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.acquire(context.Background())
	request := spec(id)
	request.Git = remote.GitScope{
		Phase: remote.GitPhaseCoding, Repository: testClaim.Repository,
		Branch: testBranch, BaseCommit: testBaseSHA, BaseBranch: "main",
	}
	ref := mustRef(t, id, "coding-run", request)
	if _, err := f.sub.Processes.Start(context.Background(), ref, request); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var body startRunRequest
	for _, sent := range f.srv.Requests() {
		if sent.Method == "POST" && sent.Path == "/v2/sandboxes/"+id.SandboxID+"/runs" {
			if err := json.Unmarshal(sent.Body, &body); err != nil {
				t.Fatal(err)
			}
		}
	}
	for key, want := range map[string]string{
		"ben.process":             runKey(ref),
		"airlock.git.phase":       "coding",
		"airlock.git.repository":  testClaim.Repository,
		"airlock.git.branch":      testBranch,
		"airlock.git.base_commit": testBaseSHA,
		"airlock.git.base_branch": "main",
	} {
		if got := body.Labels[key]; got != want {
			t.Errorf("label %s = %q, want %q", key, got, want)
		}
	}
	if body.Labels["airlock.git.operation"] != "" || body.Labels["airlock.git.checkout_commit"] != "" {
		t.Fatalf("coding run received prepare or publish authority: %v", body.Labels)
	}
}

// A lost startRun response: the run exists and the client never learned its id.
// remote.Runner's recovery is exactly this call made again, and the contract's
// stored idempotent result is what makes it resolve rather than duplicate.
func TestStartRecoversALostResponse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))

	f.srv.DropNextResponse("POST", "/v2/sandboxes/"+id.SandboxID+"/runs")
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start past a lost response: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("a lost start response produced %d runs, want 1: %v", len(got), got)
	}
	if st.BackendRunID != f.srv.RunIDs()[0] {
		t.Fatalf("resolved %s, backend holds %v", st.BackendRunID, f.srv.RunIDs())
	}
}

func TestStatusLeavesAnUnansweredStartWithoutAnActiveRunUnresolved(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)
	request := spec(id)
	ref := mustRef(t, id, "run-before-request", request)

	// The Airlock client's write-ahead binding lands, but the partition keeps
	// every startRun attempt from reaching the server. A null active slot is not
	// proof that one of those attempts cannot still commit.
	f.srv.Partition(true)
	if _, err := f.sub.Processes.Start(ctx, ref, request); err == nil {
		t.Fatal("partitioned Start unexpectedly answered")
	}
	f.srv.Partition(false)
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("partitioned Start created runs: %v", got)
	}
	if _, err := f.sub.Processes.Status(ctx, ref); !errors.Is(err, remote.ErrProcessUnresolved) {
		t.Fatalf("Status = %v, want %v", err, remote.ErrProcessUnresolved)
	}
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("read-only Status replayed the unanswered Start: %v", got)
	}
}

func TestStatusDoesNotAdoptAnUnrelatedActiveRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)
	pendingSpec := spec(id)
	pending := mustRef(t, id, "pending-run", pendingSpec)

	// Reserve the pending address without letting its request reach Airlock.
	f.srv.Partition(true)
	if _, err := f.sub.Processes.Start(ctx, pending, pendingSpec); err == nil {
		t.Fatal("partitioned pending Start unexpectedly answered")
	}
	f.srv.Partition(false)

	otherSpec := spec(id, "other-agent")
	other := mustRef(t, id, "other-run", otherSpec)
	otherStatus, err := f.sub.Processes.Start(ctx, other, otherSpec)
	if err != nil {
		t.Fatalf("starting unrelated run: %v", err)
	}
	if otherStatus.BackendRunID == "" {
		t.Fatal("unrelated run has no permanent id")
	}

	if _, err := f.sub.Processes.Status(ctx, pending); !errors.Is(err, remote.ErrProcessUnresolved) {
		t.Fatalf("Status adopted an unrelated active run: %v", err)
	}
	binding, err := f.store.LoadBinding(pending.String())
	if err != nil {
		t.Fatal(err)
	}
	if binding.RunID != "" {
		t.Fatalf("pending address was rebound to unrelated run %s", binding.RunID)
	}
}

func TestStartPersistsTheReplayFenceBeforeDispatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	id := f.acquire(context.Background())
	request := spec(id)
	ref := mustRef(t, id, "run-1", request)

	crash := errors.New("disk full")
	f.store.(*memStore).setReserveFault(func(string, time.Time) error { return crash })
	if _, err := f.sub.Processes.Start(context.Background(), ref, request); !errors.Is(err, crash) {
		t.Fatalf("Start past failed replay-fence persistence: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("Start dispatched before its replay fence landed: %v", got)
	}
}

// The run id is the only permanent handle to a live run: the key that produced
// it expires after 24 hours. A binding that could not be persisted must fail the
// call rather than hand back a status for a run nothing can name.
func TestStartFailsWhenTheRunBindingCannotBePersisted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))

	crash := errors.New("disk full")
	f.store.(*memStore).setBindFault(func(string, string) error { return crash })
	if _, err := f.sub.Processes.Start(ctx, ref, spec(id)); !errors.Is(err, crash) {
		t.Fatalf("Start past a failed binding: %v", err)
	}

	// The run exists at the backend, and the ambiguity is honest: a replay
	// resolves the same one rather than creating a second.
	f.store.(*memStore).setBindFault(nil)
	if _, err := f.sub.Processes.Start(ctx, ref, spec(id)); err != nil {
		t.Fatalf("replayed Start: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("recovery produced %d runs, want 1: %v", len(got), got)
	}
}

// A start reservation has the same principal-scoped idempotency domain as a
// sandbox create. A static source's descriptor stays fixed when its $VAR moves,
// so an unanswered start must carry the runtime fence as well.
func TestStartRefusesToReplayAnAmbiguousRunAfterAStaticTokenChangesPrincipal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	request := spec(id)
	ref := mustRef(t, id, "run-1", request)
	store := f.store.(*memStore)

	crash := errors.New("disk full")
	store.setBindFault(func(string, string) error { return crash })
	if _, err := f.sub.Processes.Start(ctx, ref, request); !errors.Is(err, crash) {
		t.Fatalf("Start past failed run-id persistence: %v", err)
	}
	store.setBindFault(nil)
	before := len(f.srv.Requests())

	const rotatedToken = "airlocktest-other-run-principal-token"
	f.auth.setValue(rotatedToken)
	f.srv.Token(rotatedToken)
	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "other-daemon", ClientID: "other-client"})
	_, err := f.sub.Processes.Start(ctx, ref, request)
	mustBe(t, err, ErrSubstrateBinding)
	if got := len(f.srv.Requests()); got != before {
		t.Fatalf("runtime principal refusal sent %d new requests, want 0", got-before)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("static token rotation left %d runs, want the ambiguous original: %v", len(got), got)
	}
}

func TestStartReplaysAnAmbiguousRunAfterAStablePrincipalRotatesToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	f.auth.principalKey = "octo:https://issuer.invalid#airlock#ben"
	request := spec(id)
	ref := mustRef(t, id, "run-1", request)
	store := f.store.(*memStore)

	crash := errors.New("disk full")
	store.setBindFault(func(string, string) error { return crash })
	if _, err := f.sub.Processes.Start(ctx, ref, request); !errors.Is(err, crash) {
		t.Fatalf("Start past failed run-id persistence: %v", err)
	}
	store.setBindFault(nil)
	const rotatedToken = "airlocktest-rotated-stable-run-token"
	f.auth.setValue(rotatedToken)
	f.srv.Token(rotatedToken)

	status, err := f.sub.Processes.Start(ctx, ref, request)
	if err != nil {
		t.Fatalf("Start after stable-principal rotation: %v", err)
	}
	if got := f.srv.RunIDs(); len(got) != 1 || got[0] != status.BackendRunID {
		t.Fatalf("stable-principal replay returned %s with backend runs %v", status.BackendRunID, got)
	}
}

// If the original run committed but its response and durable id were lost, the
// keyed request is the only recovery path. Once that result expires, even a
// terminal original must not be dispatched a second time.
func TestStartFencesAnAmbiguousRunAfterIdempotencyExpiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	request := spec(id)
	ref := mustRef(t, id, "run-1", request)
	store := f.store.(*memStore)

	crash := errors.New("disk full")
	store.setBindFault(func(string, string) error { return crash })
	if _, err := f.sub.Processes.Start(ctx, ref, request); !errors.Is(err, crash) {
		t.Fatalf("Start past failed run-id persistence: %v", err)
	}
	runID := f.srv.RunIDs()[0]
	f.srv.Terminate(runID, airlocktest.Exited(0))
	store.setBindFault(nil)
	store.setBindingAttemptedAt(ref.String(), time.Now().UTC().Add(-idempotencyReplayWindow-time.Minute))
	path := "/v2/sandboxes/" + id.SandboxID + "/runs"
	f.srv.ForgetIdempotency("POST", path, runKey(ref))

	_, err := f.sub.Processes.Start(ctx, ref, request)
	mustBe(t, err, ErrStartReplayExpired)
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("expired recovery dispatched %d runs, want the original only: %v", len(got), got)
	}
}

func TestStartRefusesAMismatchedRequest(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	other := id
	other.SandboxID = "sbx_" + "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		ref  remote.ProcessRef
		spec remote.ProcessSpec
	}{
		{"incomplete reference", remote.ProcessRef{Identity: id}, spec(id)},
		{"spec names another workspace", mustRef(t, id, "run-1", spec(id)), spec(other)},
		{"no argv", mustRef(t, id, "run-1", spec(id)), remote.ProcessSpec{Identity: id}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.sub.Processes.Start(ctx, tc.ref, tc.spec)
			mustBe(t, err, remote.ErrProcessMismatch)
		})
	}
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("a refused start created a run: %v", got)
	}
}

// The same key with a different payload is the sharpest refusal in the
// contract: the same address naming a different request. It must never be
// retried, because a retry either conflicts again or, past the replay window,
// creates a second run.
func TestStartMapsAnIdempotencyConflictToProcessMismatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))

	f.srv.FailNext("POST", "/v2/sandboxes/{sandbox_id}/runs", 409, "idempotency_key_conflict",
		"this key was first used with a different payload", nil)
	_, err := f.sub.Processes.Start(ctx, ref, spec(id))
	mustBe(t, err, remote.ErrProcessMismatch)
	if got := f.srv.RunIDs(); len(got) != 0 {
		t.Fatalf("a conflicting start created a run: %v", got)
	}
}

// This is the fake-fidelity audit for ProcessBackend's absence vocabulary. The
// expected sentinel is stated independently for each real-world state, then the
// same method is driven against Airlock and remotetest. Adding a fake-only
// interpretation therefore fails here instead of making a higher-level test
// pass on a world production cannot produce.
func TestProcessBackendAbsenceVocabularyMatchesTheContractFake(t *testing.T) {
	methods := []string{"attach", "status", "events", "stdin", "stop", "wait"}
	for _, state := range []struct {
		name    string
		want    error
		methods []string
		setup   func(*testing.T) (processSubject, processSubject)
	}{
		{
			name: "never accepted", want: remote.ErrNoProcess,
			methods: []string{"status", "events", "stdin", "stop", "wait"},
			setup:   neverAcceptedSubjects,
		},
		{
			name: "unanswered start", want: remote.ErrProcessUnresolved,
			// Status is the resolving operation: it adopts the exact active
			// run from the sandbox's durable slot. The other methods have no
			// authority to do that and retain the unresolved result.
			methods: []string{"attach", "events", "stdin", "stop", "wait"}, setup: unresolvedSubjects,
		},
		{
			name: "accepted resource unavailable", want: remote.ErrProcessUnavailable,
			methods: append([]string{"start"}, methods...), setup: unavailableSubjects,
		},
		{
			name: "attach names an unknown permanent id", want: remote.ErrProcessUnavailable,
			methods: []string{"attach"}, setup: neverAcceptedSubjects,
		},
		{
			// The fourth absence (#284): the backend refused the body before
			// creating anything. Definite on every read, and on a re-offered
			// Start — which both implementations answer from their own record
			// rather than by asking again.
			name: "admission refused", want: remote.ErrProcessRefused,
			methods: []string{"start", "status", "events", "stdin", "stop", "wait"}, setup: refusedSubjects,
		},
		{
			// A refusal is also a confirmed absence, so every consumer that
			// retires a dispatch on ErrNoProcess does the right thing with it.
			name: "admission refused reads as never accepted", want: remote.ErrNoProcess,
			methods: []string{"start", "status", "events", "stdin", "stop", "wait"}, setup: refusedSubjects,
		},
	} {
		state := state
		t.Run(state.name, func(t *testing.T) {
			for _, method := range state.methods {
				method := method
				t.Run(method, func(t *testing.T) {
					real, fake := state.setup(t)
					for name, subject := range map[string]processSubject{"airlock": real, "remotetest": fake} {
						err := processMethodError(t, method, subject)
						if !errors.Is(err, state.want) {
							t.Errorf("%s %s error = %v, want %v", name, method, err, state.want)
						}
					}
				})
			}
		})
	}

	t.Run("status resolves the exact active run", func(t *testing.T) {
		real, fake := unresolvedSubjects(t)
		for name, subject := range map[string]processSubject{"airlock": real, "remotetest": fake} {
			st, err := subject.backend.Status(context.Background(), subject.ref)
			if err != nil || st.BackendRunID == "" || !st.Reachable {
				t.Errorf("%s Status = %+v, %v; want the adopted active run", name, st, err)
			}
		}
	})

	// Attach persists a supplied permanent id before reading it. A 404 on that
	// read must therefore remain unavailable on every later operation; the fake
	// may not fall back to the never-accepted answer after the first call.
	t.Run("failed attach retains the unavailable binding", func(t *testing.T) {
		real, fake := neverAcceptedSubjects(t)
		for name, subject := range map[string]processSubject{"airlock": real, "remotetest": fake} {
			if err := processMethodError(t, "attach", subject); !errors.Is(err, remote.ErrProcessUnavailable) {
				t.Fatalf("%s Attach = %v, want %v", name, err, remote.ErrProcessUnavailable)
			}
			if _, err := subject.backend.Status(context.Background(), subject.ref); !errors.Is(err, remote.ErrProcessUnavailable) {
				t.Errorf("%s Status after failed Attach = %v, want %v", name, err, remote.ErrProcessUnavailable)
			}
		}
	})

	// A refused address must never read as an unanswered start: that reading is
	// what replayed one impossible request 129 times (#284).
	t.Run("a refusal is not unresolved", func(t *testing.T) {
		real, fake := refusedSubjects(t)
		for name, subject := range map[string]processSubject{"airlock": real, "remotetest": fake} {
			for _, method := range []string{"start", "status", "events", "stdin", "stop", "wait"} {
				if err := processMethodError(t, method, subject); errors.Is(err, remote.ErrProcessUnresolved) {
					t.Errorf("%s %s = %v; a definite refusal read as an unanswered start", name, method, err)
				}
			}
		}
	})

	// Start itself has no read-side absence outcome. Its independently anchored
	// refusal is an exact-address mismatch in both implementations.
	real, fake := neverAcceptedSubjects(t)
	for name, subject := range map[string]processSubject{"airlock": real, "remotetest": fake} {
		changed := subject.spec
		changed.Argv = []string{"different-agent"}
		_, err := subject.backend.Start(context.Background(), subject.ref, changed)
		if !errors.Is(err, remote.ErrProcessMismatch) {
			t.Errorf("%s Start mismatch = %v, want %v", name, err, remote.ErrProcessMismatch)
		}
	}
}

type processSubject struct {
	backend   remote.ProcessBackend
	ref       remote.ProcessRef
	spec      remote.ProcessSpec
	backendID string
}

func neverAcceptedSubjects(t *testing.T) (processSubject, processSubject) {
	t.Helper()
	f := newFixture(t)
	realID := f.acquire(context.Background())
	realSpec := spec(realID)
	realRef := mustRef(t, realID, "audit-never", realSpec)

	fakeBackend := remotetest.New(realID.ProfileRevision)
	fakeID, err := fakeBackend.Workspaces().Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("fake Acquire: %v", err)
	}
	fakeSpec := spec(fakeID)
	fakeRef := mustRef(t, fakeID, "audit-never", fakeSpec)
	return processSubject{backend: f.sub.Processes, ref: realRef, spec: realSpec, backendID: "run_missing"},
		processSubject{backend: fakeBackend, ref: fakeRef, spec: fakeSpec, backendID: "run_missing"}
}

func unresolvedSubjects(t *testing.T) (processSubject, processSubject) {
	t.Helper()
	f := newFixture(t)
	realID := f.acquire(context.Background())
	realSpec := spec(realID)
	realRef := mustRef(t, realID, "audit-unresolved", realSpec)
	responseLost := errors.New("response lost before the permanent id was recorded")
	store := f.store.(*memStore)
	store.setBindFault(func(string, string) error { return responseLost })
	if _, err := f.sub.Processes.Start(context.Background(), realRef, realSpec); !errors.Is(err, responseLost) {
		t.Fatalf("real unresolved Start = %v, want %v", err, responseLost)
	}
	store.setBindFault(nil)

	fakeBackend := remotetest.New(realID.ProfileRevision)
	fakeID, err := fakeBackend.Workspaces().Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("fake Acquire: %v", err)
	}
	fakeSpec := spec(fakeID)
	fakeRef := mustRef(t, fakeID, "audit-unresolved", fakeSpec)
	fakeBackend.SetStartFault(responseLost, true)
	if _, err := fakeBackend.Start(context.Background(), fakeRef, fakeSpec); !errors.Is(err, responseLost) {
		t.Fatalf("fake unresolved Start = %v, want %v", err, responseLost)
	}
	return processSubject{backend: f.sub.Processes, ref: realRef, spec: realSpec},
		processSubject{backend: fakeBackend, ref: fakeRef, spec: fakeSpec}
}

func unavailableSubjects(t *testing.T) (processSubject, processSubject) {
	t.Helper()
	f := newFixture(t)
	realID := f.acquire(context.Background())
	realSpec := spec(realID)
	realRef := mustRef(t, realID, "audit-unavailable", realSpec)
	realStatus, err := f.sub.Processes.Start(context.Background(), realRef, realSpec)
	if err != nil {
		t.Fatalf("real Start: %v", err)
	}
	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "other-daemon", ClientID: "other-client"})

	fakeBackend := remotetest.New(realID.ProfileRevision)
	fakeID, err := fakeBackend.Workspaces().Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("fake Acquire: %v", err)
	}
	fakeSpec := spec(fakeID)
	fakeRef := mustRef(t, fakeID, "audit-unavailable", fakeSpec)
	fakeStatus, err := fakeBackend.Start(context.Background(), fakeRef, fakeSpec)
	if err != nil {
		t.Fatalf("fake Start: %v", err)
	}
	fakeBackend.SetProcessUnavailable(fakeRef.RunID, true)
	return processSubject{backend: f.sub.Processes, ref: realRef, spec: realSpec, backendID: realStatus.BackendRunID},
		processSubject{backend: fakeBackend, ref: fakeRef, spec: fakeSpec, backendID: fakeStatus.BackendRunID}
}

// refusedSubjects are addresses whose one start the backend refused before
// creating anything. The real backend meets the contract fake's own 413 — its
// sandbox's envelope is deliberately unreadable (a legacy record on a
// rolled-forward profile), so the prompt travels inline and the server, not
// the client, is what refuses it.
func refusedSubjects(t *testing.T) (processSubject, processSubject) {
	t.Helper()
	f := newFixture(t)
	realID := f.acquire(context.Background())
	unreadableEnvelope(f)
	realSpec := spec(realID)
	realSpec.Stdin = []byte(strings.Repeat("p", int(airlocktest.DefaultLimits.Inline)+1))
	realRef := mustRef(t, realID, "audit-refused", realSpec)
	if _, err := f.sub.Processes.Start(context.Background(), realRef, realSpec); !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("real refused Start = %v, want %v", err, remote.ErrProcessRefused)
	}

	fakeBackend := remotetest.New(realID.ProfileRevision)
	fakeID, err := fakeBackend.Workspaces().Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("fake Acquire: %v", err)
	}
	fakeSpec := spec(fakeID)
	fakeSpec.Stdin = realSpec.Stdin
	fakeRef := mustRef(t, fakeID, "audit-refused", fakeSpec)
	fakeBackend.SetStartRefusal("payload_too_large", "inline stdin exceeds the profile's limit", airlocktest.DefaultLimits.Inline)
	if _, err := fakeBackend.Start(context.Background(), fakeRef, fakeSpec); !errors.Is(err, remote.ErrProcessRefused) {
		t.Fatalf("fake refused Start = %v, want %v", err, remote.ErrProcessRefused)
	}
	return processSubject{backend: f.sub.Processes, ref: realRef, spec: realSpec},
		processSubject{backend: fakeBackend, ref: fakeRef, spec: fakeSpec}
}

func processMethodError(t *testing.T, method string, subject processSubject) error {
	t.Helper()
	ctx := context.Background()
	switch method {
	case "start":
		_, err := subject.backend.Start(ctx, subject.ref, subject.spec)
		return err
	case "attach":
		_, err := subject.backend.Attach(ctx, subject.ref, subject.backendID)
		return err
	case "status":
		_, err := subject.backend.Status(ctx, subject.ref)
		return err
	case "events":
		_, err := subject.backend.Events(ctx, subject.ref, 0)
		return err
	case "stdin":
		return subject.backend.Stdin(ctx, subject.ref, []byte("x"))
	case "stop":
		_, err := subject.backend.Stop(ctx, subject.ref, remote.StopRequest{Mode: core.StopDiscard})
		return err
	case "wait":
		_, err := subject.backend.Wait(ctx, subject.ref)
		return err
	default:
		t.Fatalf("unknown ProcessBackend method %q", method)
		return nil
	}
}

// The single active slot. A second dispatch into a sandbox already running one
// is a conflict, not an invitation to signal the other run.
func TestStartRefusesASecondActiveRun(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)

	first := mustRef(t, id, "run-1", spec(id))
	if _, err := f.sub.Processes.Start(ctx, first, spec(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	second := mustRef(t, id, "run-2", spec(id, "/usr/bin/codex", "exec"))
	_, err := f.sub.Processes.Start(ctx, second, spec(id, "/usr/bin/codex", "exec"))
	if err == nil {
		t.Fatal("a second run was dispatched into an occupied sandbox")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != CodeRunConflict {
		t.Fatalf("got %v, want run_conflict", err)
	}
	if apiErr.ActiveRunID == "" {
		t.Fatal("run_conflict did not name the active run")
	}
}

// Framing is BEN's, not Airlock's: a chunk may split a line and a chunk may
// carry several. The envelopes handed to the boundary are raw bytes with a
// stream label, and nothing here parses a provider record.
func TestEventsCarryOpaqueChunksWithTheirStream(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	f.srv.Emit(st.BackendRunID, "stdout", []byte(`{"type":"a"}`+"\n"+`{"type":`))
	f.srv.Emit(st.BackendRunID, "stderr", []byte("warning\n"))
	f.srv.Truncate(st.BackendRunID, 4096)

	envelopes, err := f.sub.Processes.Events(ctx, ref, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	// run.started is sequence 1 and is a control envelope: retained, never
	// handed to a provider translator.
	if len(envelopes) != 4 {
		t.Fatalf("got %d envelopes, want 4: %+v", len(envelopes), envelopes)
	}
	want := []remote.Stream{remote.StreamControl, remote.StreamStdout, remote.StreamStderr, remote.StreamControl}
	for i, env := range envelopes {
		if env.Seq != int64(i+1) {
			t.Fatalf("envelope %d has sequence %d", i, env.Seq)
		}
		if env.Stream != want[i] {
			t.Fatalf("envelope %d is %s, want %s", i, env.Stream, want[i])
		}
	}
	if got := string(envelopes[1].Payload); got != `{"type":"a"}`+"\n"+`{"type":` {
		t.Fatalf("stdout payload was re-framed: %q", got)
	}
	if !envelopes[3].Truncated {
		t.Fatal("output.truncated was flattened into ordinary control data")
	}
}

// An empty result means the stream is sealed, and remote.Attempt synthesizes an
// outcome from it. A long poll that merely elapsed must therefore never be
// empty — this is the property that keeps a quiet running agent from being
// reported as a crash.
func TestEventsIsEmptyOnlyWhenTheStreamIsSealed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.srv.Emit(st.BackendRunID, "stdout", []byte("hello\n"))

	drained, err := f.sub.Processes.Events(ctx, ref, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	cursor := remote.Cursor(drained[len(drained)-1].Seq)

	// Running and quiet: the call must block rather than report a seal. A
	// bounded context is how the test observes "did not return empty".
	quiet, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err := f.sub.Processes.Events(quiet, ref, cursor); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a quiet running run returned %v, want the reader's own deadline", err)
	}

	f.srv.Terminate(st.BackendRunID, airlocktest.Exited(0))
	tail, err := f.sub.Processes.Events(ctx, ref, cursor)
	if err != nil {
		t.Fatalf("Events after terminate: %v", err)
	}
	if len(tail) == 0 {
		t.Fatal("the run.terminal event was not delivered")
	}
	sealed, err := f.sub.Processes.Events(ctx, ref, remote.Cursor(tail[len(tail)-1].Seq))
	if err != nil {
		t.Fatalf("Events at the seal: %v", err)
	}
	if len(sealed) != 0 {
		t.Fatalf("a drained terminal run returned %d envelopes", len(sealed))
	}
}

// Retention expiring under BEN's cursor is missing evidence either way, and the
// adapter's job is to say whether BEN can name what it lost. An expiry carrying
// both the cursor it is about and the retention floor is a measured range the
// daemon may durably accept; it stays an ErrEventGap as well, so a reader that
// has not learned to accept one keeps refusing it.
func TestEventsMapsAnExplainedExpiryToAMeasuredRetentionGap(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.srv.Emit(st.BackendRunID, "stdout", []byte("a\n"))
	f.srv.Emit(st.BackendRunID, "stdout", []byte("b\n"))
	f.srv.ExpireEvents(st.BackendRunID, 3)

	_, err = f.sub.Processes.Events(ctx, ref, 0)
	mustBe(t, err, remote.ErrEventGap)
	mustBe(t, err, remote.ErrRetentionGap)
	var gap *remote.RetentionGap
	if !errors.As(err, &gap) {
		t.Fatalf("Events after an expiry = %v, want a *remote.RetentionGap", err)
	}
	if gap.RequestedAfter != 0 || gap.OldestAvailable != 3 {
		t.Fatalf("gap = %+v, want the cursor 0 and the floor 3", gap)
	}
	if got, ok := gap.Range(); !ok || got != (remote.EventGap{From: 1, To: 2}) {
		t.Fatalf("Range() = (%v, %v), want ([1, 2], true)", got, ok)
	}
}

// An expiry BEN cannot read as a range is the refusal it always was.
//
// Each case is a way the backend's answer stops being evidence about *this*
// request, and the consequence of accepting any of them is identical: a cursor
// advanced over sequences nobody can name. Driven through the stored-refusal
// seam rather than the retention floor, because a conformant fake cannot produce
// these bodies — the point is what BEN does with a backend that does.
func TestEventsRefusesAnExpiryItCannotMeasure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		status  int
		details map[string]any
	}{
		{name: "no details at all"},
		{name: "a floor with no cursor", details: map[string]any{"oldest_available_seq": 5}},
		{name: "a cursor with no floor", details: map[string]any{"requested_after": 0}},
		{
			name:    "an answer about a cursor this call never sent",
			details: map[string]any{"requested_after": 7, "oldest_available_seq": 20},
		},
		{
			name:    "a floor that describes no missing sequence",
			details: map[string]any{"requested_after": 0, "oldest_available_seq": 1},
		},
		{
			name:    "a floor that is not a sequence",
			details: map[string]any{"requested_after": 0, "oldest_available_seq": 3.5},
		},
		{
			name:    "a negative floor",
			details: map[string]any{"requested_after": 0, "oldest_available_seq": -3},
		},
		{
			name:    "a floor outside the client's sequence range",
			details: map[string]any{"requested_after": 0, "oldest_available_seq": uint64(1) << 63},
		},
		{
			name:    "the right code on the wrong HTTP status",
			status:  http.StatusInternalServerError,
			details: map[string]any{"requested_after": 0, "oldest_available_seq": 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			id := f.acquire(ctx)
			ref := mustRef(t, id, "run-1", spec(id))
			if _, err := f.sub.Processes.Start(ctx, ref, spec(id)); err != nil {
				t.Fatalf("Start: %v", err)
			}
			status := tc.status
			if status == 0 {
				status = http.StatusConflict
			}
			f.srv.FailNext("GET", "/v2/sandboxes/{sandbox_id}/runs/{run_id}/events",
				status, "cursor_too_old", "the requested cursor has expired", tc.details)

			_, err := f.sub.Processes.Events(ctx, ref, 0)
			mustBe(t, err, remote.ErrEventGap)
			if errors.Is(err, remote.ErrRetentionGap) {
				t.Fatalf("Events = %v, want an unmeasurable refusal rather than an acceptable range", err)
			}
			var gap *remote.RetentionGap
			if errors.As(err, &gap) {
				t.Fatalf("an unreadable expiry produced the range %+v", gap)
			}
		})
	}
}

// A cursor past the run's log means the client holds one from a different run.
// That is an address error rather than a gap, and it must park rather than
// resume from anywhere.
func TestEventsMapsACursorAheadToAProcessMismatch(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	if _, err := f.sub.Processes.Start(ctx, ref, spec(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err := f.sub.Processes.Events(ctx, ref, 99)
	mustBe(t, err, remote.ErrProcessMismatch)
}

// The two stop modes stay different all the way to the backend, and neither is
// termination: a 202 acknowledges the request and the evidence is read back.
func TestStopSendsTheModeSpecificSignalAndReturnsFreshEvidence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		mode core.StopMode
		want string
		// The grace as it must appear on the wire. BEN's §7.5 window crosses the
		// boundary unrounded so the backend ladder and the local one agree; a
		// floor applied here would make the remote ladder the more patient of the
		// two. Grace is TERM-only, so Discard must carry none at all.
		wantBody string
	}{
		{"interrupt is the patient ladder", core.StopInterrupt, "TERM", `"grace_seconds":10`},
		{"discard asks for immediate termination", core.StopDiscard, "KILL", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			id := f.acquire(ctx)
			ref := mustRef(t, id, "run-1", spec(id))
			st, err := f.sub.Processes.Start(ctx, ref, spec(id))
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			f.srv.Emit(st.BackendRunID, "stdout", []byte("working\n"))

			if _, err := f.sub.Processes.Stop(ctx, ref, remote.StopRequest{Mode: tc.mode, Grace: 10 * time.Second}); err != nil {
				t.Fatalf("Stop: %v", err)
			}

			var sent bool
			for _, req := range f.srv.Requests() {
				if req.Method != "POST" || !contains(req.Body, `"signal":"`+tc.want+`"`) {
					continue
				}
				sent = true
				if tc.wantBody != "" && !contains(req.Body, tc.wantBody) {
					t.Fatalf("%s body %s does not carry %s", tc.want, req.Body, tc.wantBody)
				}
				if tc.wantBody == "" && contains(req.Body, "grace_seconds") {
					t.Fatalf("%s body %s carries a grace", tc.want, req.Body)
				}
			}
			if !sent {
				t.Fatalf("no %s signal was sent", tc.want)
			}
		})
	}
}

// A signal acceptance is not termination. The evidence fields are untouched by
// signalling, so MayReuse stays false until the domain is observed quiet.
func TestStopDoesNotAuthorizeTouchingTheWorkspace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.srv.Emit(st.BackendRunID, "stdout", []byte("winding up\n"))

	after, _ := f.sub.Processes.Stop(ctx, ref, remote.StopRequest{Mode: core.StopInterrupt, Grace: time.Second})
	if remote.MayReuse(after) {
		t.Fatalf("a delivered signal authorized workspace reuse: %+v", after)
	}
	if after.Termination() != core.TerminationUnconfirmed {
		t.Fatalf("termination is %s after a signal, want unconfirmed", after.Termination())
	}
}

// A stop ladder that asks repeatedly must not append a durable signal record
// every time: the per-run client quota is the one a client can spend against
// itself. The key is derived from the run and the signal, so a repeat replays.
func TestRepeatedStopsReplayRatherThanAppend(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.srv.Emit(st.BackendRunID, "stdout", []byte("working\n"))

	for range 5 {
		f.sub.Processes.Stop(ctx, ref, remote.StopRequest{Mode: core.StopInterrupt, Grace: time.Second})
	}
	if got := f.srv.SignalCount(st.BackendRunID); got != 1 {
		t.Fatalf("five interrupts appended %d durable signal records, want 1", got)
	}
}

// signalRun is keyed just like createSandbox and startRun. Its fence is only
// needed for one call's retry lifetime, but it must still be the exact token
// captured before the first attempt: a static $VAR may rotate while an
// unanswered signal is backing off.
func TestASignalRetryReusesItsCredentialSnapshot(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	path := "/v2/sandboxes/" + id.SandboxID + "/runs/" + st.BackendRunID + "/signal"
	const rotatedToken = "airlocktest-rotated-during-signal-retry"
	f.auth.rotateAfterNextFetch(rotatedToken)
	f.sub.client.sleep = func(context.Context, time.Duration) error { return nil }
	f.srv.FailNext("POST", "/v2/sandboxes/{sandbox_id}/runs/{run_id}/signal", 503,
		"dependency_unavailable", "retry the signal", nil)

	// keyedAuth consumes the original token and then the test source rotates.
	// The first request receives a retryable refusal; the second must still use
	// the snapshot, even though a fresh Fetch would now return the rotated value.
	_, err = f.sub.Processes.Stop(ctx, ref, remote.StopRequest{Mode: core.StopInterrupt, Grace: time.Second})
	if !hasCode(err, CodeUnauthenticated) {
		// signalRun succeeds under the snapshot; the following status GET is the
		// first operation allowed to fetch again and is rejected by the fake.
		t.Fatalf("Stop after credential rotation = %v, want the later status fetch to be rejected", err)
	}
	var attempts []airlocktest.Request
	for _, req := range f.srv.Requests() {
		if req.Method == "POST" && req.Path == path {
			attempts = append(attempts, req)
		}
	}
	if len(attempts) != 2 {
		t.Fatalf("signal attempts = %d, want the retryable refusal and one retry", len(attempts))
	}
	for i, req := range attempts {
		if req.Auth != "Bearer "+airlocktest.DefaultToken {
			t.Errorf("signal attempt %d used %q, want the original credential snapshot", i+1, req.Auth)
		}
	}
	if got := f.srv.SignalCount(st.BackendRunID); got != 1 {
		t.Fatalf("credential rotation appended %d signal records, want 1", got)
	}
}

func TestALostSignalResponseReplaysOneSignalRecord(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	path := "/v2/sandboxes/" + id.SandboxID + "/runs/" + st.BackendRunID + "/signal"
	f.srv.DropNextResponse("POST", path)
	if _, err := f.sub.Processes.Stop(ctx, ref, remote.StopRequest{Mode: core.StopInterrupt, Grace: time.Second}); err != nil {
		t.Fatalf("Stop after a lost signal response: %v", err)
	}
	if got := f.srv.SignalCount(st.BackendRunID); got != 1 {
		t.Fatalf("lost response appended %d signal records, want 1", got)
	}
	attempts := 0
	for _, req := range f.srv.Requests() {
		if req.Method == "POST" && req.Path == path {
			attempts++
			if req.Auth != "Bearer "+airlocktest.DefaultToken {
				t.Errorf("signal retry used %q, want the original credential snapshot", req.Auth)
			}
		}
	}
	if attempts != 2 {
		t.Fatalf("signal attempts = %d, want the lost response and one replay", attempts)
	}
}

// The signal key outlives one client.do call: the stop ladder may ask again on
// another tick, and startup recovery may ask from another process. Both must
// remain in the principal scope recorded before startRun, not merely snapshot
// whatever credential is current for that one call.
func TestSeparateStopsRefuseAChangedStaticPrincipal(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		restart bool
	}{
		{"another tick", false},
		{"after restart", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			id := f.acquire(ctx)
			ref := mustRef(t, id, "run-1", spec(id))
			st, err := f.sub.Processes.Start(ctx, ref, spec(id))
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if _, err := f.sub.Processes.Stop(ctx, ref, remote.StopRequest{
				Mode: core.StopInterrupt, Grace: time.Second,
			}); err != nil {
				t.Fatalf("first Stop: %v", err)
			}
			before := len(f.srv.Requests())

			const rotatedToken = "airlocktest-admin-token"
			f.auth.setValue(rotatedToken)
			f.srv.Token(rotatedToken)
			f.srv.Owner(airlocktest.Principal{
				TenantID: "admin", Subject: "other-daemon", ClientID: "admin-client",
			})
			if tc.restart {
				f.rebuild()
			}
			_, err = f.sub.Processes.Stop(ctx, ref, remote.StopRequest{
				Mode: core.StopInterrupt, Grace: time.Second,
			})
			mustBe(t, err, ErrSubstrateBinding)
			if got := len(f.srv.Requests()); got != before {
				t.Fatalf("principal refusal sent %d requests, want 0", got-before)
			}
			if got := f.srv.SignalCount(st.BackendRunID); got != 1 {
				t.Fatalf("separate Stops appended %d signal records, want the original only", got)
			}
		})
	}
}

func TestSeparateStopsAllowAStablePrincipalToRotateTokens(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.principalKey = "octo:https://issuer.invalid#airlock#ben"
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := f.sub.Processes.Stop(ctx, ref, remote.StopRequest{
		Mode: core.StopInterrupt, Grace: time.Second,
	}); err != nil {
		t.Fatalf("first Stop: %v", err)
	}

	const rotatedToken = "airlocktest-rotated-stable-signal-token"
	f.auth.setValue(rotatedToken)
	f.srv.Token(rotatedToken)
	if _, err := f.sub.Processes.Stop(ctx, ref, remote.StopRequest{
		Mode: core.StopInterrupt, Grace: time.Second,
	}); err != nil {
		t.Fatalf("Stop after stable-principal rotation: %v", err)
	}
	if got := f.srv.SignalCount(st.BackendRunID); got != 1 {
		t.Fatalf("stable-principal token rotation appended %d signal records, want 1", got)
	}
}

func TestStopRefusesARunBindingWithNoPrincipalFence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	if _, err := f.sub.Processes.Start(ctx, ref, spec(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.store.(*memStore).setBindingPrincipal(ref.String(), "")
	before := len(f.srv.Requests())

	_, err := f.sub.Processes.Stop(ctx, ref, remote.StopRequest{Mode: core.StopDiscard})
	mustBe(t, err, ErrSubstrateBinding)
	if got := len(f.srv.Requests()); got != before {
		t.Fatalf("missing-principal refusal sent %d requests, want 0", got-before)
	}
}

// An unreachable backend is the absence of an answer, not an answer. It must
// map to unconfirmed and never to "the run went away".
func TestAnUnreachableBackendIsUnconfirmedRatherThanTerminal(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	if _, err := f.sub.Processes.Start(ctx, ref, spec(id)); err != nil {
		t.Fatalf("Start: %v", err)
	}

	f.srv.Partition(true)
	st, err := f.sub.Processes.Status(ctx, ref)
	if err == nil {
		t.Fatal("Status answered while the backend was unreachable")
	}
	if remote.MayReuse(st) {
		t.Fatalf("an unreachable backend authorized workspace reuse: %+v", st)
	}
	if st.Termination() != core.TerminationUnconfirmed {
		t.Fatalf("termination is %s, want unconfirmed", st.Termination())
	}
}

// No write-ahead binding means Start never crossed the backend's acceptance
// fence. The seam reports that narrower fact; remotews is the layer that maps
// it to a locally justified quiet status and retires the journal.
func TestStatusWithoutAStartFenceReportsNeverAccepted(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))

	st, err := f.sub.Processes.Status(ctx, ref)
	mustBe(t, err, remote.ErrNoProcess)
	if remote.MayReuse(st) {
		t.Fatalf("an unnameable run authorized workspace reuse: %+v", st)
	}
}

func TestWaitReturnsOnTheTerminalStateAndNotOnDomainQuiet(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.srv.Emit(st.BackendRunID, "stdout", []byte("working\n"))
	// Reaped, streams sealed, domain *not* observed: Wait returns because the
	// direct process is the fact it reports, and the workspace stays untouchable.
	f.srv.Terminate(st.BackendRunID, airlocktest.Terminal{
		State: "exited", Reason: "process_exit", Sealed: "confirmed", Reaped: "confirmed",
		ExitCode: ptr(0),
	})

	waited, err := f.sub.Processes.Wait(ctx, ref)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !waited.Reaped() {
		t.Fatalf("Wait returned a status that is not reaped: %+v", waited)
	}
	if remote.MayReuse(waited) {
		t.Fatal("a reaped process authorized workspace reuse without domain quiet")
	}
}

// The evidence translation, held to every combination that matters. Three facts
// in and three out, with nothing derived across them.
func TestStatusOfTranslatesEachEvidenceFieldIndependently(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		run     Run
		phase   remote.Phase
		stream  remote.StreamState
		process remote.ProcessState
		domain  remote.DomainState
		reuse   bool
		reaped  bool
	}{
		{
			name:  "a fresh run claims nothing",
			run:   Run{RunID: "run_1", State: RunQueued},
			phase: remote.PhaseStarting, stream: remote.StreamStateUnknown,
			process: remote.ProcessStateUnknown, domain: remote.DomainStateUnknown,
		},
		{
			name: "running with an open stream",
			run: Run{RunID: "run_1", State: RunRunning, Termination: RunTermination{
				StreamSealed: EvidenceNotConfirmed, ProcessReaped: EvidenceNotConfirmed, DomainQuiet: EvidenceNotConfirmed,
			}},
			phase: remote.PhaseRunning, stream: remote.StreamStateOpen,
			process: remote.ProcessStateRunning, domain: remote.DomainStateActive,
		},
		{
			name: "a delivered TERM is not termination",
			run: Run{RunID: "run_1", State: RunTerminating, Termination: RunTermination{
				Reason: ReasonClientSignal,
			}},
			phase: remote.PhaseSignaled, stream: remote.StreamStateUnknown,
			process: remote.ProcessStateUnknown, domain: remote.DomainStateUnknown,
		},
		{
			name: "exited and quiet is the only reuse case",
			run: Run{RunID: "run_1", State: RunExited, Termination: RunTermination{
				Reason: ReasonProcessExit, StreamSealed: EvidenceConfirmed,
				ProcessReaped: EvidenceConfirmed, DomainQuiet: EvidenceConfirmed,
			}},
			phase: remote.PhaseQuiet, stream: remote.StreamStateSealed,
			process: remote.ProcessStateReaped, domain: remote.DomainStateQuiet,
			reuse: true, reaped: true,
		},
		{
			name: "exited with survivors is not reusable",
			run: Run{RunID: "run_1", State: RunExited, Termination: RunTermination{
				StreamSealed: EvidenceConfirmed, ProcessReaped: EvidenceConfirmed, DomainQuiet: EvidenceNotConfirmed,
			}},
			phase: remote.PhaseUnknown, stream: remote.StreamStateSealed,
			process: remote.ProcessStateReaped, domain: remote.DomainStateActive,
			reaped: true,
		},
		{
			name:  "lost is never evidence of anything",
			run:   Run{RunID: "run_1", State: RunLost, Termination: RunTermination{Reason: ReasonRunnerLost}},
			phase: remote.PhaseUnknown, stream: remote.StreamStateUnknown,
			process: remote.ProcessStateUnknown, domain: remote.DomainStateUnknown,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := statusOf(tc.run)
			if got.Phase != tc.phase || got.Stream != tc.stream || got.Process != tc.process || got.Domain != tc.domain {
				t.Fatalf("got %+v, want phase=%s stream=%s process=%s domain=%s",
					got, tc.phase, tc.stream, tc.process, tc.domain)
			}
			if !got.Reachable {
				t.Fatal("a status built from a response reports unreachable")
			}
			if remote.MayReuse(got) != tc.reuse {
				t.Fatalf("MayReuse is %v, want %v", remote.MayReuse(got), tc.reuse)
			}
			if got.Reaped() != tc.reaped {
				t.Fatalf("Reaped is %v, want %v", got.Reaped(), tc.reaped)
			}
		})
	}
}

// The zero Status is what an unreachable backend produces, and it must not
// authorize anything.
func TestTheZeroStatusAuthorizesNothing(t *testing.T) {
	t.Parallel()
	var zero remote.Status
	if remote.MayReuse(zero) || zero.Reaped() || zero.Termination() != core.TerminationUnconfirmed {
		t.Fatalf("the zero status authorizes something: %+v", zero)
	}
}

func contains(body []byte, needle string) bool {
	return len(body) > 0 && indexOf(string(body), needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// A backend timeout is an ordinary terminal run with ordinary evidence: the
// ladder ran, the process was reaped, and the domain was observed quiet. What
// makes it a timeout is the *reason*, which is diagnosis — BEN's outcome comes
// from the provider's own stream, and its workspace decision from domain quiet.
func TestABackendTimeoutIsTerminalWithConfirmedEvidence(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.srv.Emit(st.BackendRunID, "stdout", []byte("thinking\n"))
	f.srv.Terminate(st.BackendRunID, airlocktest.TimedOut())

	after, err := f.sub.Processes.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !remote.MayReuse(after) {
		t.Fatalf("a timed-out run with confirmed quiet did not authorize reuse: %+v", after)
	}
	if !after.Reaped() {
		t.Fatalf("a timed-out run was not reported reaped: %+v", after)
	}
	// The attempt and stall bounds crossed the boundary so the backend could
	// enforce them while BEN was disconnected; reconnecting never resets either.
	var carried bool
	for _, req := range f.srv.Requests() {
		if contains(req.Body, `"hard_seconds":3600`) && contains(req.Body, `"output_stall_seconds":300`) {
			carried = true
		}
	}
	if !carried {
		t.Fatal("the run's deadlines were not sent to the backend")
	}
}

// The replay filter over this package's envelope translation. A reconnect
// replays by design — BEN commits a cursor only after durably consuming an
// event — so a duplicate is dropped and a *different* payload under a sequence
// already consumed is the one thing no dedupe rule can repair.
func TestReplayIsDroppedAndARewrittenSequenceConflicts(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	st, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	f.srv.Emit(st.BackendRunID, "stdout", []byte("first\n"))
	f.srv.Emit(st.BackendRunID, "stdout", []byte("second\n"))

	batch, err := f.sub.Processes.Events(ctx, ref, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	seq := remote.NewSequencer(0)
	fresh, err := seq.Admit(batch)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(fresh) != len(batch) {
		t.Fatalf("admitted %d of %d fresh envelopes", len(fresh), len(batch))
	}

	// The same page again, as a reconnect would deliver it: dropped.
	replay, err := f.sub.Processes.Events(ctx, ref, 0)
	if err != nil {
		t.Fatalf("Events replay: %v", err)
	}
	again, err := seq.Admit(replay)
	if err != nil {
		t.Fatalf("Admit replay: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("a replayed page admitted %d envelopes", len(again))
	}

	// And the backend's log ceasing to be the log BEN read.
	f.srv.Rewrite(st.BackendRunID, 2, []byte("rewritten\n"))
	rewritten, err := f.sub.Processes.Events(ctx, ref, 0)
	if err != nil {
		t.Fatalf("Events after rewrite: %v", err)
	}
	_, err = seq.Admit(rewritten)
	mustBe(t, err, remote.ErrEventConflict)
}

// A malformed envelope is a durable hole, not a failed read. The event page was
// decoded and its cursor position cannot change under Airlock's append-only log,
// so retrying it would hold the attempt forever at the same sequence (#275).
func TestMalformedEventEnvelopesAreUnresumableGaps(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		raw  json.RawMessage
	}{
		{name: "event shape", raw: json.RawMessage(`{"seq":[]}`)},
		{name: "output encoding", raw: json.RawMessage(`{"seq":1,"kind":"output","stream":"stdout","data_b64":"%%%"}`)},
		{name: "output stream", raw: json.RawMessage(`{"seq":1,"kind":"output","stream":"sideways","data_b64":""}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := envelopesOf([]json.RawMessage{tc.raw}); !errors.Is(err, remote.ErrEventGap) {
				t.Fatalf("envelopesOf() error = %v, want ErrEventGap", err)
			}
		})
	}
}
