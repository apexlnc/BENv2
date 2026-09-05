package airlock

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

func TestAcquireIsIdempotentPerClaimCycle(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	first := f.acquire(ctx)
	second := f.acquire(ctx)

	if first != second {
		t.Fatalf("a second acquire returned a different identity:\n first %+v\nsecond %+v", first, second)
	}
	if !first.Complete() {
		t.Fatalf("acquired identity is incomplete: %+v", first)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("acquire created %d sandboxes, want 1: %v", len(got), got)
	}
	if first.ProfileRevision != airlocktest.Revision("v1") {
		t.Fatalf("pinned revision %q, want the profile's current one", first.ProfileRevision)
	}
}

func TestSandboxRecordPersistsANonSecretSubstrateBinding(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.acquire(context.Background())

	rec, err := f.store.LoadSandbox(testClaim)
	if err != nil {
		t.Fatalf("LoadSandbox: %v", err)
	}
	if rec.Substrate != f.sub.binding || !rec.Substrate.complete() {
		t.Fatalf("recorded binding = %+v, want %+v", rec.Substrate, f.sub.binding)
	}
	if !strings.HasPrefix(rec.PrincipalBinding, "opaque-token-sha256:") {
		t.Fatalf("runtime principal binding = %q, want an opaque-token digest", rec.PrincipalBinding)
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(body, []byte(f.auth.value)) {
		t.Fatal("durable substrate binding contains the bearer credential")
	}
}

// Once SandboxID is durable it, not the bounded create key, is the resource
// handle. This remains true after the replay window: a POST here could allocate
// a second sandbox before bind noticed the changed id.
func TestAcquireUsesThePersistedSandboxIDAfterTheReplayWindow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	first := f.acquire(ctx)

	rec, err := f.store.LoadSandbox(testClaim)
	if err != nil {
		t.Fatalf("LoadSandbox: %v", err)
	}
	rec.CreateAttemptedAt = time.Now().UTC().Add(-idempotencyReplayWindow - time.Hour)
	if err := f.store.SaveSandbox(rec); err != nil {
		t.Fatalf("SaveSandbox: %v", err)
	}

	second := f.acquire(ctx)
	if second != first {
		t.Fatalf("reattached %+v, want %+v", second, first)
	}
	creates, gets := 0, 0
	for _, request := range f.srv.Requests() {
		switch {
		case request.Method == "POST" && request.Path == "/v2/sandboxes":
			creates++
		case request.Method == "GET" && request.Path == "/v2/sandboxes/"+first.SandboxID:
			gets++
		}
	}
	if creates != 1 || gets == 0 {
		t.Fatalf("requests included %d creates and %d gets, want one create followed by a GET", creates, gets)
	}
}

// A reservation without SandboxID is ambiguous: the create may have committed
// and lost its response. Once its keyed result may have expired, replay is no
// longer safe; an old record with no fence is equally unknowable.
func TestAcquireFencesAnAmbiguousCreatePastTheReplayWindow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		version   int
		attempted time.Time
		bound     bool
		want      error
	}{
		{"expired fenced record", SandboxRecordVersion, time.Now().UTC().Add(-idempotencyReplayWindow - time.Minute), true, ErrCreateReplayExpired},
		{"legacy record with no substrate fence", 1, time.Time{}, false, ErrSubstrateBinding},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			rec := SandboxRecord{
				Version: tc.version, Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
				Profile:           airlocktest.DefaultProfile,
				Key:               sandboxKey(testClaim, testBranch, testBaseSHA, airlocktest.DefaultProfile),
				CreateAttemptedAt: tc.attempted,
			}
			if tc.bound {
				rec.Substrate = f.sub.binding
				auth, err := f.sub.client.keyedAuth(context.Background())
				if err != nil {
					t.Fatalf("keyedAuth: %v", err)
				}
				rec.PrincipalBinding = auth.principalBinding
			}
			if err := f.store.SaveSandbox(rec); err != nil {
				t.Fatalf("SaveSandbox: %v", err)
			}

			_, err := f.sub.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
				Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
			})
			mustBe(t, err, tc.want)
			if got := f.srv.SandboxIDs(); len(got) != 0 {
				t.Fatalf("an ambiguous old reservation created sandboxes: %v", got)
			}
		})
	}
}

// The lost-response case the whole idempotency contract exists for: the sandbox
// was created and the client never learned its identifier. The only correct
// resolution is replaying the same key.
func TestAcquireRecoversALostCreateResponse(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	f.srv.DropNextResponse("POST", "/v2/sandboxes")
	// The client's own retry replays the same key, so a dropped response is
	// resolved inside one Acquire. What must not happen is a second sandbox.
	id := f.acquire(ctx)

	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("a lost create response produced %d sandboxes, want 1: %v", len(got), got)
	}
	if id.SandboxID != f.srv.SandboxIDs()[0] {
		t.Fatalf("acquired %s, and the backend holds %v", id.SandboxID, f.srv.SandboxIDs())
	}
}

// Airlock's idempotency domain is the endpoint plus the authenticated tenant
// and subject. A record written before createSandbox answered must not carry
// its key into a different control plane, where the same text is a fresh key.
func TestAcquireRefusesToReplayAnAmbiguousCreateOnAnotherEndpoint(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	leaveAmbiguousCreate(t, f)

	other := airlocktest.New(t)
	restarted := f.newSubstrate("https://other-airlock.invalid", f.auth, other)
	_, err := restarted.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	mustBe(t, err, ErrSubstrateBinding)
	if got := other.Requests(); len(got) != 0 {
		t.Fatalf("binding refusal contacted the new endpoint: %+v", got)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("the original endpoint holds %d sandboxes, want the ambiguous original: %v", len(got), got)
	}
}

func TestAcquireReplaysAnAmbiguousCreateOnTheSameSubstrateAfterRestart(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	leaveAmbiguousCreate(t, f)

	restarted := f.rebuild()
	id, err := restarted.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("Acquire after restart: %v", err)
	}
	if id.SandboxID == "" {
		t.Fatal("Acquire after restart returned no sandbox id")
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 || got[0] != id.SandboxID {
		t.Fatalf("same-substrate replay returned %s with backend sandboxes %v", id.SandboxID, got)
	}
}

// A static source identifies the variable, not the Airlock principal held in
// its current value. This is the supported rotation the pure source binding
// cannot see: the URL, kind, authority and BindingKey all remain unchanged.
func TestAcquireRefusesToReplayAnAmbiguousCreateAfterAStaticTokenChangesPrincipal(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	leaveAmbiguousCreate(t, f)
	before := len(f.srv.Requests())

	const rotatedToken = "airlocktest-other-principal-token"
	f.auth.setValue(rotatedToken)
	f.srv.Token(rotatedToken)
	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "other-daemon", ClientID: "other-client"})
	restarted := f.rebuild()
	_, err := restarted.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	mustBe(t, err, ErrSubstrateBinding)
	if got := len(f.srv.Requests()); got != before {
		t.Fatalf("runtime principal refusal sent %d new requests, want 0", got-before)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("static token rotation left %d sandboxes, want the ambiguous original: %v", len(got), got)
	}
}

// A source that can state its stable service principal is allowed to rotate
// the short-lived credential representing it. The principal key, not the token
// bytes, is the replay scope for that source.
func TestAcquireReplaysAnAmbiguousCreateAfterAStablePrincipalRotatesToken(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.auth.principalKey = "octo:https://issuer.invalid#airlock#ben"
	leaveAmbiguousCreate(t, f)

	const rotatedToken = "airlocktest-rotated-stable-principal-token"
	f.auth.setValue(rotatedToken)
	f.srv.Token(rotatedToken)
	restarted := f.rebuild()
	id, err := restarted.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("Acquire after stable-principal rotation: %v", err)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 || got[0] != id.SandboxID {
		t.Fatalf("stable-principal replay returned %s with backend sandboxes %v", id.SandboxID, got)
	}
}

// Changing credential-source identity can change Airlock's tenant or subject
// even when the URL and textual idempotency key stay fixed. The write-ahead
// record closes that restart boundary before another authenticated request.
func TestAcquireRefusesToReplayAnAmbiguousCreateWithAnotherCredentialIdentity(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	leaveAmbiguousCreate(t, f)
	before := len(f.srv.Requests())

	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "other-daemon", ClientID: "other-client"})
	auth := &tokenSource{value: airlocktest.DefaultToken, bindingKey: "static:other-airlock-identity"}
	restarted := f.newSubstrate("https://airlock.invalid", auth, f.srv)
	_, err := restarted.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	mustBe(t, err, ErrSubstrateBinding)
	if got := len(f.srv.Requests()); got != before {
		t.Fatalf("binding refusal sent %d new requests, want 0", got-before)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("credential change left %d sandboxes, want the ambiguous original: %v", len(got), got)
	}
}

// This assertion is independent of BEN's restart fence: the fake itself must
// expose the contract's principal-scoped idempotency domain, or the regression
// above would stay green after that fence was removed.
func TestContractFakeScopesIdempotencyByTenantAndSubject(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	body := createSandboxRequest{ProfileID: airlocktest.DefaultProfile, Labels: map[string]string{"test": "scope"}}
	key := "ben.fake.scope.123456"
	create := func() Sandbox {
		var sandbox Sandbox
		if err := f.sub.client.do(ctx, request{
			method: "POST", path: "/v2/sandboxes", idem: key, body: body, out: &sandbox,
		}); err != nil {
			t.Fatalf("createSandbox: %v", err)
		}
		return sandbox
	}

	first := create()
	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "other-daemon", ClientID: "client-a"})
	second := create()
	if second.SandboxID == first.SandboxID {
		t.Fatalf("another principal replayed sandbox %s", first.SandboxID)
	}

	// OAuth client id is audit metadata, not ownership or idempotency scope.
	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "other-daemon", ClientID: "client-b"})
	third := create()
	if third.SandboxID != second.SandboxID {
		t.Fatalf("client rotation created %s, want replay of %s", third.SandboxID, second.SandboxID)
	}
	if got := f.srv.SandboxIDs(); len(got) != 2 {
		t.Fatalf("principal-scoped key produced %d sandboxes, want 2: %v", len(got), got)
	}
}

// The crash boundary: the durable record is written *before* createSandbox is
// attempted, so a process that dies in the launch window leaves a record naming
// the exact key. A restart replays it and finds the same sandbox.
func TestAcquireReservesTheRecordBeforeCreating(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	store := f.store.(*memStore)

	crash := errors.New("disk full")
	store.setSaveFault(func(SandboxRecord) error { return crash })
	if _, err := f.sub.Workspaces.Acquire(ctx, remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	}); !errors.Is(err, crash) {
		t.Fatalf("Acquire past a failed reservation: %v", err)
	}
	if got := f.srv.SandboxIDs(); len(got) != 0 {
		t.Fatalf("a sandbox was created before its record landed: %v", got)
	}

	store.setSaveFault(nil)
	id := f.acquire(ctx)
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("after the record landed there are %d sandboxes, want 1", len(got))
	}
	if id.SandboxID == "" {
		t.Fatal("acquire returned no sandbox id")
	}
}

// The token and its durable digest are one snapshot. If a static variable
// rotates after the reservation lands, the already-bound request still uses
// the credential whose digest was persisted; a second fetch here would put the
// request in a scope the record never named.
func TestAcquireUsesTheCredentialSnapshotThatItsRecordBinds(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	store := f.store.(*memStore)
	const rotatedToken = "airlocktest-rotated-after-reservation"
	rotated := false
	store.setSaveFault(func(rec SandboxRecord) error {
		if rec.SandboxID == "" && !rotated {
			rotated = true
			f.auth.setValue(rotatedToken)
		}
		return nil
	})

	id, err := f.sub.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("Acquire across post-reservation rotation: %v", err)
	}
	if id.SandboxID == "" {
		t.Fatal("Acquire returned no sandbox id")
	}
	for _, req := range f.srv.Requests() {
		if req.Method == "POST" && req.Path == "/v2/sandboxes" {
			if req.Auth != "Bearer "+airlocktest.DefaultToken {
				t.Fatalf("create used %q, want the credential snapshotted before reservation", req.Auth)
			}
			return
		}
	}
	t.Fatal("Acquire sent no create request")
}

func leaveAmbiguousCreate(t *testing.T, f *fixture) {
	t.Helper()
	store := f.store.(*memStore)
	crash := errors.New("disk full")
	store.setSaveFault(func(rec SandboxRecord) error {
		if rec.SandboxID != "" {
			return crash
		}
		return nil
	})
	_, err := f.sub.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if !errors.Is(err, crash) {
		t.Fatalf("Acquire past failed sandbox-id persistence: %v", err)
	}
	store.setSaveFault(nil)
	rec, err := store.LoadSandbox(testClaim)
	if err != nil {
		t.Fatalf("LoadSandbox: %v", err)
	}
	if rec.SandboxID != "" || rec.Substrate != f.sub.binding || rec.PrincipalBinding == "" {
		t.Fatalf("ambiguous record = %+v, want no id and complete replay bindings", rec)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("ambiguous create committed %d sandboxes, want 1: %v", len(got), got)
	}
}

func TestAttachWithoutARecordIsNoWorkspace(t *testing.T) {
	t.Parallel()
	f := newFixture(t)

	_, err := f.sub.Workspaces.Attach(context.Background(), testClaim)
	mustBe(t, err, remote.ErrNoWorkspace)
	if got := f.srv.SandboxIDs(); len(got) != 0 {
		t.Fatalf("Attach created a sandbox: %v", got)
	}
}

// A restart reattaches from the durable record alone. This is the reason the
// record exists: Attach is handed a claim and Airlock has no list-by-label
// route, so a daemon with no record could only re-derive a sandbox by creating
// one.
func TestAttachAfterRestartResolvesTheSameSandbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	acquired := f.acquire(ctx)

	restarted := f.rebuild()
	attached, err := restarted.Workspaces.Attach(ctx, testClaim)
	if err != nil {
		t.Fatalf("Attach after restart: %v", err)
	}
	if attached != acquired {
		t.Fatalf("attached %+v, acquired %+v", attached, acquired)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("restart produced %d sandboxes, want 1: %v", len(got), got)
	}
}

// Cross-tenant and cross-subject collisions are one refusal from BEN's side:
// the record names something this principal may not act on.
func TestAttachRefusesASandboxThisPrincipalDoesNotOwn(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		owner airlocktest.Principal
	}{
		{"another tenant", airlocktest.Principal{TenantID: "other", Subject: "them", ClientID: "cli"}},
		{"another subject in the owning tenant", airlocktest.Principal{TenantID: "ben", Subject: "them", ClientID: "cli"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			ctx := context.Background()
			f.acquire(ctx)

			f.srv.Owner(tc.owner)
			_, err := f.sub.Workspaces.Attach(ctx, testClaim)
			mustBe(t, err, ErrNotOwned)
		})
	}
}

// The mutable-profile hazard ProfileRevision exists to close: the same sandbox
// id naming a different world.
func TestAcquireRefusesAMovedProfileRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	f.acquire(ctx)

	f.srv.SetProfile(airlocktest.DefaultProfile, "approved", airlocktest.Revision("v2"))
	_, err := f.sub.Workspaces.Acquire(ctx, remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
		ProfileRevision: airlocktest.Revision("v2"),
	})
	mustBe(t, err, ErrProfileRevision)
}

// A withdrawn revision fails resume with a non-retryable refusal, and BEN must
// park rather than silently resume on a newer world.
func TestAcquireRefusesAWithdrawnPinnedRevision(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	if err := f.sub.Workspaces.Suspend(ctx, testClaim); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	f.srv.SetProfile(airlocktest.DefaultProfile, "withdrawn", airlocktest.Revision("v1"))

	_, err := f.sub.Workspaces.Acquire(ctx, remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA, ProfileRevision: id.ProfileRevision,
	})
	mustBe(t, err, ErrProfileRevision)
}

func TestSuspendThenAcquireResumesTheSameSandbox(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	first := f.acquire(ctx)

	if err := f.sub.Workspaces.Suspend(ctx, testClaim); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if got := f.srv.SandboxState(first.SandboxID); got != "suspended" {
		t.Fatalf("sandbox is %q after suspend, want suspended", got)
	}

	second := f.acquire(ctx)
	if second != first {
		t.Fatalf("resume returned a different identity: %+v vs %+v", second, first)
	}
	if got := f.srv.SandboxState(first.SandboxID); got != "ready" {
		t.Fatalf("sandbox is %q after resume, want ready", got)
	}
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("suspend/resume produced %d sandboxes, want 1", len(got))
	}
}

func TestSuspendRefusesACrossTenant404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "them", ClientID: "cli"})
	err := f.sub.Workspaces.Suspend(ctx, testClaim)
	mustBe(t, err, ErrNotOwned)
	if got := f.srv.SandboxState(id.SandboxID); got != "ready" {
		t.Fatalf("cross-tenant suspend left the sandbox %q, want ready", got)
	}
}

// Airlock ownership is tenant plus subject. client_id is retained for audit,
// but rotating the OAuth client must not strand the same workload principal's
// existing sandboxes.
func TestOAuthClientRotationRetainsSandboxOwnership(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	f.srv.Owner(airlocktest.Principal{TenantID: "ben", Subject: "ben-daemon", ClientID: "rotated-client"})
	attached, err := f.sub.Workspaces.Attach(ctx, testClaim)
	if err != nil {
		t.Fatalf("Attach after client rotation: %v", err)
	}
	if attached != id {
		t.Fatalf("attached %+v, want %+v", attached, id)
	}
	if err := f.sub.Workspaces.Suspend(ctx, testClaim); err != nil {
		t.Fatalf("Suspend after client rotation: %v", err)
	}
	if got := f.srv.SandboxState(id.SandboxID); got != "suspended" {
		t.Fatalf("sandbox is %q after client rotation and suspend, want suspended", got)
	}
}

func TestSuspendAndDeleteAreNoOpsWithoutARecord(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	if err := f.sub.Workspaces.Suspend(ctx, testClaim); err != nil {
		t.Fatalf("Suspend with no record: %v", err)
	}
	if err := f.sub.Workspaces.Delete(ctx, testClaim); err != nil {
		t.Fatalf("Delete with no record: %v", err)
	}
}

// Deletion is not complete when the call returns. The record survives until all
// three evidence fields confirm, because a forgotten record is a sandbox
// nothing will ever finish deleting.
func TestDeleteWaitsForConfirmedEvidenceAndClearsTheRecord(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	if err := f.sub.Workspaces.Delete(ctx, testClaim); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := f.srv.SandboxState(id.SandboxID); got != "deleted" {
		t.Fatalf("sandbox is %q after delete, want deleted", got)
	}
	if _, err := f.store.LoadSandbox(testClaim); !errors.Is(err, ErrNoSandboxRecord) {
		t.Fatalf("the durable record survived a confirmed deletion: %v", err)
	}
	// And a repeated disposal after a crash must not fail.
	if err := f.sub.Workspaces.Delete(ctx, testClaim); err != nil {
		t.Fatalf("repeated Delete: %v", err)
	}
}

// DELETE is the one workspace verb Airlock does not refuse over a live run: it
// moves the sandbox to `deleting` and marks whatever was executing in it `lost`.
// So the gate is BEN's, and it is read off the sandbox's own single active slot
// (#252).
//
// The run here stands for the one BEN's claim journal cannot see. A review
// executes in the same workspace-cycle sandbox under its own run id
// (internal/reviewrun), so the claim-level quiet gate remote.Dispose applies
// would pass while the reviewer still held the domain — and the volume it is
// writing to would go.
func TestDeleteRefusesWhileARunHoldsTheSandboxDomain(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	review := mustRef(t, id, "run-review", spec(id, "/usr/bin/codex", "exec"))
	started, err := f.sub.Processes.Start(ctx, review, spec(id, "/usr/bin/codex", "exec"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	mustBe(t, f.sub.Workspaces.Delete(ctx, testClaim), remote.ErrNotQuiet)
	if _, err := f.store.LoadSandbox(testClaim); err != nil {
		t.Fatalf("a refused delete removed the durable record: %v", err)
	}
	if got := f.srv.SandboxState(id.SandboxID); got != "ready" {
		t.Fatalf("the sandbox is %q after a refused delete, want ready", got)
	}
	if got := f.srv.RunState(started.BackendRunID); got != "running" && got != "queued" {
		t.Fatalf("the run holding the domain is %q; a refused delete must not have touched it", got)
	}

	// The reviewer ends and releases the slot: the same delete now lands, which is
	// what makes the refusal a retry rather than a dead end.
	f.srv.Terminate(started.BackendRunID, airlocktest.Exited(0))
	if err := f.sub.Workspaces.Delete(ctx, testClaim); err != nil {
		t.Fatalf("Delete after the domain went quiet: %v", err)
	}
	if _, err := f.store.LoadSandbox(testClaim); !errors.Is(err, ErrNoSandboxRecord) {
		t.Fatalf("the durable record survived a confirmed deletion: %v", err)
	}
}

// The empty slot is not the whole gate, and this is the case that separates the
// two facts.
//
// Airlock releases the active slot when a run reaches *any* terminal state, and
// moves the sandbox `ready -> failed` in the same step when that run's domain
// quiet was not confirmed. A gate reading only the slot would see it empty, call
// the domain quiet and destroy a volume something may still be writing to — which
// is remote.MayReuse's fail-open mistake reached from the sandbox instead of from
// the run.
func TestDeleteRefusesASandboxWhoseDomainQuietIsUnconfirmed(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	review := mustRef(t, id, "run-review", spec(id, "/usr/bin/codex", "exec"))
	started, err := f.sub.Processes.Start(ctx, review, spec(id, "/usr/bin/codex", "exec"))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Terminal, reaped, streams sealed — and the one fact that matters withheld.
	f.srv.Terminate(started.BackendRunID, airlocktest.Terminal{
		State: "failed", Reason: "runner_lost",
		Sealed: "confirmed", Reaped: "confirmed", Quiet: "not_confirmed",
	})
	if got := f.srv.ActiveRun(id.SandboxID); got != "" {
		t.Fatalf("the active slot holds %q; this test needs the slot released", got)
	}
	if got := f.srv.SandboxState(id.SandboxID); got != "failed" {
		t.Fatalf("the sandbox is %q; this test needs the unconfirmed-quiet state", got)
	}

	mustBe(t, f.sub.Workspaces.Delete(ctx, testClaim), remote.ErrNotQuiet)
	if _, err := f.store.LoadSandbox(testClaim); err != nil {
		t.Fatalf("a refused delete removed the durable record: %v", err)
	}
	if got := f.srv.SandboxState(id.SandboxID); got != "failed" {
		t.Fatalf("the sandbox is %q after a refused delete; its volume was touched", got)
	}
}

// A 202 is not a deletion. Until compute release, volume destruction and record
// tombstoning are each `confirmed`, BEN keeps the record and keeps owing the
// delete — including across the restart in the middle of this test, which is the
// case where the only thing the new process knows it read off disk.
//
// The partial confirmation is the point: two of three fields reading `confirmed`
// is exactly the shape that satisfies code checking the wrong conjunction.
func TestDeleteWaitsThroughAPartialConfirmationAndAcrossARestart(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) {
		// A settle window this test does not spend: the poll delay is stubbed out
		// below, so the deadline is reached after one extra read rather than after
		// the fixture's whole budget.
		o.Timeouts.Settle = time.Millisecond
	})
	f.sub.Workspaces.sleep = func(context.Context, time.Duration) error { return nil }
	ctx := context.Background()
	id := f.acquire(ctx)

	f.srv.PendingDeletion("confirmed", "confirmed", "not_confirmed")
	err := f.sub.Workspaces.Delete(ctx, testClaim)
	mustBe(t, err, ErrDeletionUnconfirmed)
	if got := f.srv.SandboxState(id.SandboxID); got != "deleting" {
		t.Fatalf("the sandbox is %q, want deleting", got)
	}
	if _, err := f.store.LoadSandbox(testClaim); err != nil {
		t.Fatalf("an unconfirmed deletion removed the durable record: %v", err)
	}

	// The daemon restarts mid-deletion. The obligation is the record, so the new
	// process finds it and replays the same delete.
	sub := f.rebuild()
	sub.Workspaces.sleep = func(context.Context, time.Duration) error { return nil }
	mustBe(t, sub.Workspaces.Delete(ctx, testClaim), ErrDeletionUnconfirmed)
	if _, err := f.store.LoadSandbox(testClaim); err != nil {
		t.Fatalf("a replayed unconfirmed deletion removed the durable record: %v", err)
	}

	f.srv.ConfirmDeletion(id.SandboxID)
	if err := sub.Workspaces.Delete(ctx, testClaim); err != nil {
		t.Fatalf("Delete after the evidence completed: %v", err)
	}
	if _, err := f.store.LoadSandbox(testClaim); !errors.Is(err, ErrNoSandboxRecord) {
		t.Fatalf("the durable record survived a confirmed deletion: %v", err)
	}
	// And the replay after the record is gone is a no-op, not a second DELETE.
	if err := sub.Workspaces.Delete(ctx, testClaim); err != nil {
		t.Fatalf("a repeated Delete: %v", err)
	}
}

// Airlock deliberately answers 404 when the current principal is in another
// tenant. It is not deletion evidence: forgetting the record would leak the
// still-live sandbox and make it unaddressable by BEN.
func TestDeleteRetainsTheRecordOnACrossTenant404(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	f.srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "them", ClientID: "cli"})
	err := f.sub.Workspaces.Delete(ctx, testClaim)
	mustBe(t, err, ErrNotOwned)
	if _, err := f.store.LoadSandbox(testClaim); err != nil {
		t.Fatalf("Delete removed the durable record after a cross-tenant 404: %v", err)
	}
	if got := f.srv.SandboxState(id.SandboxID); got != "ready" {
		t.Fatalf("the still-live sandbox is %q, want ready", got)
	}
}

func TestDeleteRetainsAnAmbiguousCreateReservation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	rec := SandboxRecord{
		Version: SandboxRecordVersion, Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
		Substrate:         f.sub.binding,
		Profile:           airlocktest.DefaultProfile,
		Key:               sandboxKey(testClaim, testBranch, testBaseSHA, airlocktest.DefaultProfile),
		CreateAttemptedAt: time.Now().UTC(),
	}
	if err := f.store.SaveSandbox(rec); err != nil {
		t.Fatalf("SaveSandbox: %v", err)
	}

	err := f.sub.Workspaces.Delete(context.Background(), testClaim)
	mustBe(t, err, ErrDeletionUnconfirmed)
	if _, err := f.store.LoadSandbox(testClaim); err != nil {
		t.Fatalf("Delete removed the ambiguous reservation: %v", err)
	}
}

// The same claim cycle asking for a different branch, base or profile is not a
// retry of this one: refusing beats leaking the first sandbox.
func TestAcquireRefusesADifferentPublicationTargetForTheSameClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	f.acquire(ctx)

	_, err := f.sub.Workspaces.Acquire(ctx, remote.AcquireRequest{
		Claim: testClaim, Branch: "ben/other", BaseSHA: testBaseSHA,
	})
	mustBe(t, err, remote.ErrClaimMismatch)
	if got := f.srv.SandboxIDs(); len(got) != 1 {
		t.Fatalf("the refusal created a second sandbox: %v", got)
	}
}

func TestAcquireRefusesAnIncompleteClaim(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		req  remote.AcquireRequest
		want error
	}{
		{"pre-epoch claim", remote.AcquireRequest{
			Claim:  remote.Claim{Repository: "o/r", Issue: "1"},
			Branch: testBranch, BaseSHA: testBaseSHA,
		}, remote.ErrClaimMismatch},
		{"no branch", remote.AcquireRequest{Claim: testClaim, BaseSHA: testBaseSHA}, remote.ErrIdentityMissing},
		{"no trusted base", remote.AcquireRequest{Claim: testClaim, Branch: testBranch}, remote.ErrIdentityMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.sub.Workspaces.Acquire(ctx, tc.req)
			mustBe(t, err, tc.want)
		})
	}
	if got := f.srv.SandboxIDs(); len(got) != 0 {
		t.Fatalf("a refused acquire created a sandbox: %v", got)
	}
}

// Ready is the readiness gate an operator's profile withdrawal has to surface
// at, rather than at the first dispatched claim.
func TestReadyRefusesAProfileThatCannotBeProvisioned(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status string
		ok     bool
	}{
		{"approved", "approved", true},
		{"deprecated", "deprecated", true},
		{"withdrawn", "withdrawn", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFixture(t)
			f.srv.SetProfile(airlocktest.DefaultProfile, tc.status, airlocktest.Revision("v1"))
			err := f.sub.Ready(context.Background())
			if tc.ok && err != nil {
				t.Fatalf("Ready: %v", err)
			}
			if !tc.ok {
				mustBe(t, err, ErrUnready)
			}
		})
	}
}

func TestReadyRefusesAnUnknownProfile(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Profile = "no-such-profile" })
	mustBe(t, f.sub.Ready(context.Background()), ErrUnready)
}

// The whole auth story is a bearer token from a credential source. A base URL
// carrying one, or a plain-http endpoint, is refused before any request.
func TestNewRefusesAnUnusableEndpoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"plain http", "http://airlock.internal"},
		{"credentials in userinfo", "https://user:secret@airlock.internal"},
		{"bare userinfo over https", "https://token@airlock.internal"},
		{"credential in query", "https://airlock.internal?token=secret"},
		{"credential in fragment", "https://airlock.internal#secret"},
		{"unsupported scheme", "ssh://airlock.internal"},
		{"no host", "https:///v2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := New(Options{
				BaseURL: tc.url, Auth: &tokenSource{value: "t"},
				Profile: "p", Store: newMemStore(),
			})
			mustBe(t, err, ErrConfig)
			if strings.Contains(err.Error(), "secret") {
				t.Fatalf("the refusal echoed the rejected URL: %v", err)
			}
		})
	}
}

func TestNewRefusesAnIncompleteConfiguration(t *testing.T) {
	t.Parallel()
	base := func() Options {
		return Options{BaseURL: "https://airlock.internal", Auth: &tokenSource{value: "t"},
			Profile: "p", Store: newMemStore()}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Options)
	}{
		{"no profile", func(o *Options) { o.Profile = "  " }},
		{"no store", func(o *Options) { o.Store = nil }},
		{"no auth source", func(o *Options) { o.Auth = nil }},
		{"long poll outlives its request bound", func(o *Options) {
			o.Timeouts = Timeouts{Request: time.Second, Poll: time.Second, PollWait: time.Second, Settle: time.Second, Retries: 1}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := base()
			tc.mutate(&opts)
			_, err := New(opts)
			mustBe(t, err, ErrConfig)
		})
	}
}

// The substrate credential is transport authority, never process
// configuration: it stays in Authorization while the already-vetted provider
// environment travels in the request body. The adapter boundary separately
// proves tracker and publish credentials never enter that environment.
func TestSubstrateCredentialStaysOutOfTheRequestBody(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)

	request := spec(id)
	request.Env = map[string]string{"AGENT_FLAG": "on"}
	ref := mustRef(t, id, "run-1", request)
	if _, err := f.sub.Processes.Start(ctx, ref, request); err != nil {
		t.Fatalf("Start: %v", err)
	}

	requests := f.srv.Requests()
	if len(requests) == 0 {
		t.Fatal("no requests were recorded")
	}
	for _, req := range requests {
		if req.Auth != "Bearer "+airlocktest.DefaultToken {
			t.Fatalf("%s %s carried %q, want the substrate bearer token", req.Method, req.Path, req.Auth)
		}
		if bytes.Contains(req.Body, []byte(airlocktest.DefaultToken)) {
			t.Fatalf("%s %s serialized the bearer token into its body", req.Method, req.Path)
		}
	}
}

// seconds carries no floor: it renders BEN's own windows — the §7.5 stop grace,
// the attempt and stall limits, a hook's timeout — and each must cross the
// boundary as the domain BEN enforces locally. The idle-window floor belongs to
// idleSeconds alone; applying it here made a 10-second grace a 60-second one.
func TestSecondsRoundsUpAndAppliesNoFloor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   time.Duration
		want *int
	}{
		{"unset leaves the profile default", 0, nil},
		{"negative leaves the profile default", -time.Second, nil},
		{"sub-second still asks for a second", time.Millisecond, ptr(1)},
		{"the default stop grace crosses unchanged", 10 * time.Second, ptr(10)},
		{"an ordinary hook timeout crosses unchanged", 30 * time.Second, ptr(30)},
		{"exact", 900 * time.Second, ptr(900)},
		{"rounds up rather than cutting the window short", 90*time.Second + time.Millisecond, ptr(91)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertSeconds(t, seconds(tc.in), tc.want)
		})
	}
}

// The two sandbox idle windows, and only they, carry the contract's floor.
func TestIdleSecondsHonoursTheContractFloor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   time.Duration
		want *int
	}{
		{"unset leaves the profile's own window", 0, nil},
		{"negative leaves the profile's own window", -time.Second, nil},
		{"sub-second is raised to the floor", time.Millisecond, ptr(60)},
		{"under the floor is raised to it", 30 * time.Second, ptr(60)},
		{"at the floor", 60 * time.Second, ptr(60)},
		{"above the floor is left alone", 900 * time.Second, ptr(900)},
		{"rounds up rather than releasing early", 90*time.Second + time.Millisecond, ptr(91)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertSeconds(t, idleSeconds(tc.in), tc.want)
		})
	}
}

func assertSeconds(t *testing.T, got, want *int) {
	t.Helper()
	switch {
	case want == nil && got != nil:
		t.Fatalf("got %d, want nil", *got)
	case want != nil && got == nil:
		t.Fatalf("got nil, want %d", *want)
	case want != nil && *got != *want:
		t.Fatalf("got %d, want %d", *got, *want)
	}
}

func ptr[T any](v T) *T { return &v }
