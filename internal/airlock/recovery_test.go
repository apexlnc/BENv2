package airlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// Startup reconciliation and end-of-claim disposal: the two places where BEN's
// durable records and Airlock's state have to be compared before ordinary
// dispatch resumes or a workspace is touched.

func quietStatus() remote.Status {
	return remote.Status{
		Phase: remote.PhaseQuiet, Stream: remote.StreamStateSealed,
		Process: remote.ProcessStateReaped, Domain: remote.DomainStateQuiet, Reachable: true,
	}
}

func TestCompleteRefusesEveryDisposalWhileTerminationIsUnconfirmed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		prev remote.Status
	}{
		{"unreachable backend", remote.Status{}},
		{"signal delivered", remote.Status{Phase: remote.PhaseSignaled, Reachable: true}},
		{"reaped but domain unobserved", remote.Status{
			Phase: remote.PhaseUnknown, Process: remote.ProcessStateReaped, Reachable: true,
		}},
		{"survivors observed", remote.Status{
			Phase: remote.PhaseUnknown, Process: remote.ProcessStateReaped,
			Domain: remote.DomainStateActive, Reachable: true,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, disposal := range []Disposal{DisposalSuspend, DisposalDelete} {
				f := newFixture(t, func(o *Options) {
					o.Retention = Retention{OnSuccess: disposal, OnFailure: disposal}
				})
				ctx := context.Background()
				id := f.acquire(ctx)

				_, err := f.sub.Complete(ctx, testClaim, OutcomePublished, tc.prev)
				mustBe(t, err, remote.ErrNotQuiet)
				if got := f.srv.SandboxState(id.SandboxID); got != "ready" {
					t.Fatalf("the sandbox is %s after a refused %s, want untouched", got, disposal)
				}
			}
		})
	}
}

func TestCompleteAppliesTheConfiguredDisposalOnceQuiet(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		retain  Retention
		outcome Outcome
		want    string
		record  bool
	}{
		{"success suspends", Retention{OnSuccess: DisposalSuspend}, OutcomePublished, "suspended", true},
		{"failure retains for forensics", Retention{OnFailure: DisposalRetain}, OutcomeFailed, "ready", true},
		{"revocation deletes", Retention{OnRevoked: DisposalDelete}, OutcomeRevoked, "deleted", false},
		{"shutdown suspends", Retention{OnShutdown: DisposalSuspend}, OutcomeShutdown, "suspended", true},
		{"a retry always reuses", Retention{OnSuccess: DisposalDelete}, OutcomeRetry, "ready", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t, func(o *Options) { o.Retention = tc.retain })
			ctx := context.Background()
			id := f.acquire(ctx)

			disposal, err := f.sub.Complete(ctx, testClaim, tc.outcome, quietStatus())
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if want := tc.retain.Disposal(tc.outcome); disposal != want {
				t.Fatalf("Complete reported %s, want %s", disposal, want)
			}
			if got := f.srv.SandboxState(id.SandboxID); got != tc.want {
				t.Fatalf("the sandbox is %s, want %s", got, tc.want)
			}
			_, err = f.store.LoadSandbox(testClaim)
			if tc.record && err != nil {
				t.Fatalf("the durable record was dropped: %v", err)
			}
			if !tc.record && err == nil {
				t.Fatal("the durable record survived a confirmed deletion")
			}
		})
	}
}

// A retry reuses the claim's workspace whatever the retention policy says. The
// tree carries the previous attempt's work, which §9.6's continuation prompt
// reports on, and §6.2 reattaches rather than recreating.
func TestARetryReusesTheSameSandbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) {
		o.Retention = Retention{OnSuccess: DisposalDelete, OnFailure: DisposalDelete}
	})
	ctx := context.Background()
	first := f.acquire(ctx)

	if _, err := f.sub.Complete(ctx, testClaim, OutcomeRetry, quietStatus()); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	second := f.acquire(ctx)
	if second != first {
		t.Fatalf("the retry acquired %+v, want %+v", second, first)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("the retry produced %d sandboxes, want 1", len(got))
	}
}

// The survey a daemon runs before ordinary dispatch resumes: what BEN's two
// durable records say, what Airlock says, and what the pair costs.
func TestReconcileReportsEveryRetainedClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	journals := remotetest.NewMemStore()

	// A workspace with no dispatch journal: acquired, nothing started in it.
	states, err := f.sub.Reconcile(ctx, journals)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d claim states, want 1", len(states))
	}
	held := states[0]
	if held.Err != nil {
		t.Fatalf("reconciling a fresh workspace: %v", held.Err)
	}
	if held.Sandbox != SandboxReady || held.Dispatched || held.Lease != remote.LeaseHeld {
		t.Fatalf("a workspace with no run reconciled to %+v", held)
	}
	if got := Active(states); got != 1 {
		t.Fatalf("an idle held workspace costs %d, want 1 — a reservation is a reservation", got)
	}

	// Now a dispatched run whose termination is unconfirmed.
	ref := mustRef(t, id, "run-1", spec(id))
	journal, err := remote.Reserve(ctx, journals, ref, remote.Meta{Provider: "claude-code"})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	st, err := journal.Dispatch(ctx, func(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
		return f.sub.Processes.Start(ctx, ref, spec(id))
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if err := journal.Observe(ctx, st); err != nil {
		t.Fatalf("Observe: %v", err)
	}

	states, err = f.sub.Reconcile(ctx, journals)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	running := states[0]
	if !running.Dispatched {
		t.Fatalf("a dispatched run reconciled as undispatched: %+v", running)
	}
	if running.ActiveRunID != st.BackendRunID {
		t.Fatalf("the active slot is %q, want %q", running.ActiveRunID, st.BackendRunID)
	}
	if running.Lease != remote.LeaseRunning {
		t.Fatalf("an unconfirmed run costs %s, want running", running.Lease)
	}

	// And once the domain is observed quiet, the run stops costing a run — but
	// the workspace it left behind is still a reservation.
	f.srv.Terminate(st.BackendRunID, airlocktest.Exited(0))
	states, err = f.sub.Reconcile(ctx, journals)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if states[0].Lease != remote.LeaseHeld {
		t.Fatalf("a confirmed-quiet run costs %s, want held", states[0].Lease)
	}
	if !remote.MayReuse(states[0].Status) {
		t.Fatalf("a confirmed-quiet run reconciled to %+v", states[0].Status)
	}
}

// A per-record decode failure is one parked claim, not a global survey failure.
// Healthy retained claims beside it must still be read from Airlock and
// reported in the same reconciliation result.
func TestReconcileContainsACorruptSandboxRecord(t *testing.T) {
	t.Parallel()
	store := NewDirStore(t.TempDir())
	f := newFixture(t, func(opts *Options) { opts.Store = store })
	healthy := f.acquire(context.Background())

	corruptClaim := remote.Claim{Repository: "srhg-ai/ben", Issue: "corrupt", Epoch: 99}
	corrupt := SandboxRecord{
		Version: SandboxRecordVersion, Claim: corruptClaim, Branch: "ben/corrupt", BaseSHA: testBaseSHA,
		Substrate: f.sub.binding,
		Profile:   airlocktest.DefaultProfile, Key: sandboxKey(corruptClaim, "ben/corrupt", testBaseSHA, airlocktest.DefaultProfile),
		CreateAttemptedAt: time.Now().UTC(),
	}
	if err := store.SaveSandbox(corrupt); err != nil {
		t.Fatalf("SaveSandbox: %v", err)
	}
	body, err := json.Marshal(corrupt)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	body = bytes.Replace(body, []byte(`"branch":"ben/corrupt"`), []byte(`"branch":[]`), 1)
	if err := os.WriteFile(store.sandboxPath(corruptClaim), body, 0o600); err != nil {
		t.Fatalf("corrupting sandbox record: %v", err)
	}

	states, err := f.sub.Reconcile(context.Background(), remotetest.NewMemStore())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(states) != 2 {
		t.Fatalf("got %d states, want the healthy and corrupt records: %+v", len(states), states)
	}
	var sawHealthy, sawCorrupt bool
	for _, state := range states {
		switch state.Claim {
		case healthy.Claim:
			sawHealthy = state.Err == nil && state.Sandbox == SandboxReady
		case corruptClaim:
			sawCorrupt = state.Err != nil && state.Record != "" && state.Lease == remote.LeaseHeld
		}
	}
	if !sawHealthy || !sawCorrupt {
		t.Fatalf("reconciliation did not report both records independently: %+v", states)
	}
	if got := Active(states); got != 2 {
		t.Fatalf("healthy plus unreadable retained records cost %d slots, want 2", got)
	}
}

// One unreadable claim must not hide the rest, and it must be reported rather
// than repaired: deleting the record would silently release a workspace that
// may be somebody else's.
func TestReconcileReportsAnUnownedClaimWithoutActingOnIt(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	f.srv.Owner(airlocktest.Principal{TenantID: "ben", Subject: "somebody-else", ClientID: "cli"})
	states, err := f.sub.Reconcile(ctx, remotetest.NewMemStore())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(states) != 1 {
		t.Fatalf("got %d claim states, want 1", len(states))
	}
	mustBe(t, states[0].Err, ErrNotOwned)
	if _, err := f.store.LoadSandbox(testClaim); err != nil {
		t.Fatalf("reconciliation removed the record: %v", err)
	}
	if got := f.srv.SandboxState(id.SandboxID); got != "ready" {
		t.Fatalf("reconciliation touched the sandbox: %s", got)
	}
}

// An unreachable backend during reconciliation leaves the claim costing a
// lease. A survey that could not reach the control plane must not free capacity.
func TestReconcileCostsALeaseWhenTheBackendIsUnreachable(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)
	journals := remotetest.NewMemStore()
	ref := mustRef(t, id, "run-1", spec(id))
	journal, err := remote.Reserve(ctx, journals, ref, remote.Meta{})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	st, err := journal.Dispatch(ctx, func(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
		return f.sub.Processes.Start(ctx, ref, spec(id))
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_ = journal.Observe(ctx, st)

	f.srv.Partition(true)
	states, err := f.sub.Reconcile(ctx, journals)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if states[0].Err == nil {
		t.Fatal("an unreachable backend reconciled cleanly")
	}
	if got := Active(states); got != 1 {
		t.Fatalf("an unreachable claim costs %d, want 1", got)
	}
}

func TestReconcileOverNoClaimsIsEmpty(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	states, err := f.sub.Reconcile(context.Background(), remotetest.NewMemStore())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(states) != 0 || Active(states) != 0 {
		t.Fatalf("got %d states costing %d, want none", len(states), Active(states))
	}
}

// The two facts this package persists survive a restart with no help from the
// previous process: a fresh Substrate over the same store resolves the same
// sandbox and the same run, and dispatches nothing.
func TestARestartResolvesTheSameSandboxAndRunFromDiskAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	ref := mustRef(t, id, "run-1", spec(id))
	started, err := f.sub.Processes.Start(ctx, ref, spec(id))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	restarted := f.rebuild()
	attached, err := restarted.Workspaces.Attach(ctx, testClaim)
	if err != nil {
		t.Fatalf("Attach after restart: %v", err)
	}
	if attached != id {
		t.Fatalf("attached %+v, want %+v", attached, id)
	}
	status, err := restarted.Processes.Status(ctx, ref)
	if err != nil {
		t.Fatalf("Status after restart: %v", err)
	}
	if status.BackendRunID != started.BackendRunID {
		t.Fatalf("the restart resolved run %s, want %s", status.BackendRunID, started.BackendRunID)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("the restart produced %d sandboxes, want 1", len(got))
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("the restart produced %d runs, want 1", len(got))
	}
}

// The disposal table itself, read as data: a retry is never configurable, and
// an unconfigured outcome retains.
func TestRetentionDisposalTable(t *testing.T) {
	t.Parallel()
	r := Retention{
		OnSuccess: DisposalSuspend, OnFailure: DisposalRetain,
		OnRevoked: DisposalDelete, OnShutdown: DisposalSuspend,
	}
	for _, tc := range []struct {
		outcome Outcome
		want    Disposal
	}{
		{OutcomeRetry, DisposalRetain},
		{OutcomePublished, DisposalSuspend},
		{OutcomeFailed, DisposalRetain},
		{OutcomeRevoked, DisposalDelete},
		{OutcomeShutdown, DisposalSuspend},
	} {
		if got := r.Disposal(tc.outcome); got != tc.want {
			t.Fatalf("Disposal(%s) is %s, want %s", tc.outcome, got, tc.want)
		}
	}
	var unconfigured Retention
	for _, outcome := range []Outcome{OutcomeRetry, OutcomePublished, OutcomeFailed, OutcomeRevoked, OutcomeShutdown} {
		if got := unconfigured.Disposal(outcome); got != DisposalRetain {
			t.Fatalf("an unconfigured %s disposes by %s, want retain", outcome, got)
		}
	}
}

// A run whose termination is never confirmed parks: the claim keeps its lease
// and the workspace is never touched, which is SPEC §9.8's fail-closed rule on
// this substrate.
func TestAnUnconfirmedTerminationParksTheClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Retention = Retention{OnFailure: DisposalDelete} })
	ctx := context.Background()
	id := f.acquire(ctx)
	journals := remotetest.NewMemStore()
	ref := mustRef(t, id, "run-1", spec(id))
	journal, err := remote.Reserve(ctx, journals, ref, remote.Meta{})
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	st, err := journal.Dispatch(ctx, func(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
		return f.sub.Processes.Start(ctx, ref, spec(id))
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	_ = journal.Observe(ctx, st)

	// Lost: Airlock can no longer obtain evidence, so every field stays unknown.
	f.srv.Terminate(st.BackendRunID, airlocktest.Lost())
	states, err := f.sub.Reconcile(ctx, journals)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if states[0].Lease != remote.LeaseRunning {
		t.Fatalf("a lost run costs %s, want running", states[0].Lease)
	}
	if states[0].Status.Termination() != core.TerminationUnconfirmed {
		t.Fatalf("a lost run reports %s", states[0].Status.Termination())
	}
	_, err = f.sub.Complete(ctx, testClaim, OutcomeFailed, states[0].Status)
	mustBe(t, err, remote.ErrNotQuiet)
	if got := f.srv.SandboxState(id.SandboxID); got == "deleted" {
		t.Fatal("a lost run's workspace was deleted")
	}
}

func TestDirStoreRoundTripsBothRecords(t *testing.T) {
	t.Parallel()
	store := NewDirStore(t.TempDir())
	substrate := SubstrateBinding{
		BaseURL: "https://airlock.test", CredentialKind: "static", CredentialBindingKey: "env:AIRLOCK_TOKEN",
	}

	if _, err := store.LoadSandbox(testClaim); err == nil {
		t.Fatal("an empty store answered for a claim")
	}
	rec := SandboxRecord{
		Claim: testClaim, Substrate: substrate, Branch: testBranch, BaseSHA: testBaseSHA, Profile: "p",
		Key:       sandboxKey(testClaim, testBranch, testBaseSHA, "p"),
		SandboxID: "sbx_1", ProfileRevision: "sha256:abc",
		Owner: Principal{TenantID: "ben", Subject: "daemon", ClientID: "cli"},
	}
	if err := store.SaveSandbox(rec); err != nil {
		t.Fatalf("SaveSandbox: %v", err)
	}
	got, err := store.LoadSandbox(testClaim)
	if err != nil {
		t.Fatalf("LoadSandbox: %v", err)
	}
	if got.Version != SandboxRecordVersion {
		t.Fatalf("version %d, want %d", got.Version, SandboxRecordVersion)
	}
	rec.Version = SandboxRecordVersion
	if got != rec {
		t.Fatalf("round trip changed the record:\n got %+v\nwant %+v", got, rec)
	}
	claims, err := store.Claims()
	if err != nil || len(claims) != 1 || claims[0].Claim != testClaim || claims[0].Err != nil {
		t.Fatalf("Claims() = %v, %v", claims, err)
	}

	attemptedAt := time.Now().UTC().Truncate(time.Millisecond)
	const principalBinding = "opaque-token-sha256:principal"
	reserved, err := store.ReserveBinding("addr", substrate, principalBinding, attemptedAt)
	if err != nil {
		t.Fatalf("ReserveBinding: %v", err)
	}
	if reserved.RunID != "" || reserved.Substrate != substrate || reserved.PrincipalBinding != principalBinding ||
		!reserved.StartAttemptedAt.Equal(attemptedAt) {
		t.Fatalf("ReserveBinding = %+v, want an unanswered start at %s", reserved, attemptedAt)
	}
	otherSubstrate := substrate
	otherSubstrate.BaseURL = "https://other-airlock.test"
	if _, err := store.ReserveBinding("addr", otherSubstrate, principalBinding, attemptedAt); !errors.Is(err, ErrSubstrateBinding) {
		t.Fatalf("ReserveBinding on another substrate = %v, want %v", err, ErrSubstrateBinding)
	}
	if err := store.SaveBinding("addr", substrate, "run_1"); err != nil {
		t.Fatalf("SaveBinding: %v", err)
	}
	if binding, err := store.LoadBinding("addr"); err != nil || binding.RunID != "run_1" ||
		binding.Substrate != substrate || !binding.StartAttemptedAt.Equal(attemptedAt) {
		t.Fatalf("LoadBinding = %+v, %v", binding, err)
	}
	mustBe(t, store.SaveBinding("addr", otherSubstrate, "run_1"), ErrSubstrateBinding)
	// An address resolves to one run for its whole life: rebinding is how one
	// dispatch becomes two.
	if err := store.SaveBinding("addr", substrate, "run_1"); err != nil {
		t.Fatalf("an identical rebinding was refused: %v", err)
	}
	mustBe(t, store.SaveBinding("addr", substrate, "run_2"), ErrUnexpectedRun)

	if err := store.DeleteSandbox(testClaim); err != nil {
		t.Fatalf("DeleteSandbox: %v", err)
	}
	if err := store.DeleteSandbox(testClaim); err != nil {
		t.Fatalf("a repeated disposal failed: %v", err)
	}
}

func TestDerivedKeysAreStableAndContractShaped(t *testing.T) {
	t.Parallel()
	// The contract's pattern: ^[A-Za-z0-9_.:-]{16,255}$.
	valid := func(key string) bool {
		if len(key) < 16 || len(key) > 255 {
			return false
		}
		for i := range len(key) {
			c := key[i]
			switch {
			case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			case c == '_' || c == '.' || c == ':' || c == '-':
			default:
				return false
			}
		}
		return true
	}

	id := remote.Identity{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
		SandboxID: "sbx_1", ProfileRevision: "sha256:abc",
	}
	ref := remote.ProcessRef{Identity: id, RunID: "attempt-1", RequestDigest: "sha256:deadbeef"}
	hook := remote.HookRef{
		Identity: id, ID: "h1", Phase: remote.HookBeforeRun, Attempt: 2, RequestDigest: "sha256:cafe",
	}
	keys := map[string]string{
		"sandbox": sandboxKey(testClaim, testBranch, testBaseSHA, "profile"),
		"run":     runKey(ref),
		"hook":    hookKey(hook),
		"signal":  signalKey("run_1", SignalTERM),
	}
	seen := map[string]string{}
	for name, key := range keys {
		if !valid(key) {
			t.Fatalf("the %s key %q does not match the contract's pattern", name, key)
		}
		if other, clash := seen[key]; clash {
			t.Fatalf("the %s and %s keys collide", name, other)
		}
		seen[key] = name
	}

	// Stable: the same inputs recompute the same key after a restart, which is
	// what removes a durable write from the critical path.
	if again := runKey(ref); again != keys["run"] {
		t.Fatalf("the run key is not stable: %q then %q", keys["run"], again)
	}
	// And sensitive to every input the request body carries.
	moved := ref
	moved.RequestDigest = "sha256:other"
	if runKey(moved) == keys["run"] {
		t.Fatal("a changed request digest produced the same run key")
	}
	if sandboxKey(testClaim, "ben/other", testBaseSHA, "profile") == keys["sandbox"] {
		t.Fatal("a changed branch produced the same sandbox key")
	}
	if signalKey("run_1", SignalKILL) == keys["signal"] {
		t.Fatal("TERM and KILL produced the same signal key")
	}
}

func TestTimeoutsResolveTheirDefaults(t *testing.T) {
	t.Parallel()
	got := Timeouts{}.withDefaults()
	if got != DefaultTimeouts {
		t.Fatalf("an empty Timeouts resolved to %+v, want %+v", got, DefaultTimeouts)
	}
	partial := Timeouts{Request: time.Second}.withDefaults()
	if partial.Request != time.Second || partial.Poll != DefaultTimeouts.Poll {
		t.Fatalf("a partial Timeouts resolved to %+v", partial)
	}
}
