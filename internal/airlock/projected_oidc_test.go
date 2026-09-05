package airlock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

func projectedAirlockToken(t testing.TB, tenant, subject, client, nonce string) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"iss": "https://oidc.eks.example/id/cluster", "aud": "airlock-api",
		"tenant": tenant, "sub": subject, "azp": client,
		"exp": time.Now().Add(time.Hour).Unix(), "nonce": nonce,
	})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".fixture-signature"
}

func projectedAirlockSource(t testing.TB, path, tenant, subject string) core.Source {
	t.Helper()
	block := map[string]any{
		"kind":   credential.ProjectedOIDCKindName,
		"issuer": "https://oidc.eks.example/id/cluster", "audience": "airlock-api",
		"tenant_claim": "tenant", "tenant_id": tenant, "subject": subject,
		"token_path": path, "min_ttl_ms": 300000,
	}
	source, err := (credential.ProjectedOIDCKind{}).New(core.SourceDescriptor{}, block)
	if err != nil {
		t.Fatalf("New projected source: %v", err)
	}
	return source
}

func projectedAirlockSubstrate(t testing.TB, srv *airlocktest.Server, store Store, source core.Source) *Substrate {
	t.Helper()
	substrate, err := New(Options{
		BaseURL: "https://airlock.invalid", Auth: source, Profile: airlocktest.DefaultProfile,
		Store: store, Transport: srv.Transport(),
		Timeouts: Timeouts{
			Request: 5 * time.Second, Poll: 5 * time.Second, PollWait: time.Second,
			Settle: 2 * time.Second, Retries: 2,
		},
	})
	if err != nil {
		t.Fatalf("New substrate: %v", err)
	}
	return substrate
}

func writeProjectedAirlockToken(t testing.TB, path, token string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectedOIDCRotationReplaysEveryKeyedAirlockOperation(t *testing.T) {
	t.Parallel()
	const tenant, subject = "ben", "ben-daemon"
	path := filepath.Join(t.TempDir(), "airlock-token")
	srv := airlocktest.New(t)
	store := newMemStore()

	first := projectedAirlockToken(t, tenant, subject, "client-a", "first")
	writeProjectedAirlockToken(t, path, first)
	srv.Token(first)
	sub := projectedAirlockSubstrate(t, srv, store, projectedAirlockSource(t, path, tenant, subject))

	// Commit createSandbox, then lose the durable sandbox ID. Recovery has only
	// the persisted principal/key fence and Airlock's idempotency record.
	crash := errors.New("disk full")
	store.setSaveFault(func(rec SandboxRecord) error {
		if rec.SandboxID != "" {
			return crash
		}
		return nil
	})
	if _, err := sub.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	}); !errors.Is(err, crash) {
		t.Fatalf("Acquire past failed ID persistence: %v", err)
	}
	store.setSaveFault(nil)

	second := projectedAirlockToken(t, tenant, subject, "client-b", "second")
	writeProjectedAirlockToken(t, path, second)
	srv.Token(second)
	srv.Owner(airlocktest.Principal{TenantID: tenant, Subject: subject, ClientID: "client-b"})
	sub = projectedAirlockSubstrate(t, srv, store, projectedAirlockSource(t, path, tenant, subject))
	id, err := sub.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		t.Fatalf("Acquire after same-principal rotation: %v", err)
	}
	if got := srv.SandboxIDs(); len(got) != 1 || got[0] != id.SandboxID {
		t.Fatalf("rotation resolved sandbox %s with backend sandboxes %v", id.SandboxID, got)
	}

	// Do the same at startRun's lost-ID boundary.
	request := spec(id)
	ref := mustRef(t, id, "run-1", request)
	store.setBindFault(func(string, string) error { return crash })
	if _, err := sub.Processes.Start(context.Background(), ref, request); !errors.Is(err, crash) {
		t.Fatalf("Start past failed ID persistence: %v", err)
	}
	store.setBindFault(nil)
	third := projectedAirlockToken(t, tenant, subject, "client-c", "third")
	writeProjectedAirlockToken(t, path, third)
	srv.Token(third)
	srv.Owner(airlocktest.Principal{TenantID: tenant, Subject: subject, ClientID: "client-c"})
	sub = projectedAirlockSubstrate(t, srv, store, projectedAirlockSource(t, path, tenant, subject))
	status, err := sub.Processes.Start(context.Background(), ref, request)
	if err != nil {
		t.Fatalf("Start after same-principal rotation: %v", err)
	}
	if got := srv.RunIDs(); len(got) != 1 || got[0] != status.BackendRunID {
		t.Fatalf("rotation resolved run %s with backend runs %v", status.BackendRunID, got)
	}

	// signalRun reuses one durable key across separate Stop calls. A later
	// credential snapshot under the same owner must replay, not deliver twice.
	if _, err := sub.Processes.Stop(context.Background(), ref, remote.StopRequest{
		Mode: core.StopInterrupt, Grace: time.Second,
	}); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	fourth := projectedAirlockToken(t, tenant, subject, "client-d", "fourth")
	writeProjectedAirlockToken(t, path, fourth)
	srv.Token(fourth)
	srv.Owner(airlocktest.Principal{TenantID: tenant, Subject: subject, ClientID: "client-d"})
	sub = projectedAirlockSubstrate(t, srv, store, projectedAirlockSource(t, path, tenant, subject))
	if _, err := sub.Processes.Stop(context.Background(), ref, remote.StopRequest{
		Mode: core.StopInterrupt, Grace: time.Second,
	}); err != nil {
		t.Fatalf("Stop after same-principal rotation: %v", err)
	}
	if got := srv.SignalCount(status.BackendRunID); got != 1 {
		t.Fatalf("same-principal rotation produced %d signal records, want 1", got)
	}
}

func TestProjectedOIDCPrincipalChangeRefusesBeforeAirlock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, tenant, subject string
	}{
		{"tenant", "other", "ben-daemon"},
		{"subject", "ben", "other-daemon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "airlock-token")
			srv := airlocktest.New(t)
			store := newMemStore()
			first := projectedAirlockToken(t, "ben", "ben-daemon", "client-a", "first")
			writeProjectedAirlockToken(t, path, first)
			srv.Token(first)
			sub := projectedAirlockSubstrate(t, srv, store,
				projectedAirlockSource(t, path, "ben", "ben-daemon"))
			if _, err := sub.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
				Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
			}); err != nil {
				t.Fatalf("Acquire: %v", err)
			}
			before := len(srv.Requests())

			rotated := projectedAirlockToken(t, tc.tenant, tc.subject, "client-z", "rotated")
			writeProjectedAirlockToken(t, path, rotated)
			srv.Token(rotated)
			srv.Owner(airlocktest.Principal{
				TenantID: tc.tenant, Subject: tc.subject, ClientID: "client-z",
			})
			restarted := projectedAirlockSubstrate(t, srv, store,
				projectedAirlockSource(t, path, tc.tenant, tc.subject))
			_, err := restarted.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
				Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
			})
			mustBe(t, err, ErrSubstrateBinding)
			if got := len(srv.Requests()); got != before {
				t.Fatalf("principal change sent %d Airlock requests, want 0", got-before)
			}
		})
	}
}

func TestProjectedOIDCTokenPrincipalDriftRefusesBeforeAirlock(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "airlock-token")
	srv := airlocktest.New(t)
	store := newMemStore()
	first := projectedAirlockToken(t, "ben", "ben-daemon", "client-a", "first")
	writeProjectedAirlockToken(t, path, first)
	srv.Token(first)
	sub := projectedAirlockSubstrate(t, srv, store, projectedAirlockSource(t, path, "ben", "ben-daemon"))
	if _, err := sub.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	}); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	before := len(srv.Requests())

	// The source definition still promises the original owner, but the current
	// projection addresses somebody else. Refuse the credential itself before
	// the backend can scope a reused idempotency key under that new principal.
	rotated := projectedAirlockToken(t, "other", "other-daemon", "client-z", "rotated")
	writeProjectedAirlockToken(t, path, rotated)
	srv.Token(rotated)
	srv.Owner(airlocktest.Principal{TenantID: "other", Subject: "other-daemon", ClientID: "client-z"})
	restarted := projectedAirlockSubstrate(t, srv, store,
		projectedAirlockSource(t, path, "ben", "ben-daemon"))
	_, err := restarted.Workspaces.Acquire(context.Background(), remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if !errors.Is(err, credential.ErrProjectedOIDCToken) {
		t.Fatalf("Acquire = %v, want the projected-token identity refusal", err)
	}
	if got := len(srv.Requests()); got != before {
		t.Fatalf("token principal drift sent %d Airlock requests, want 0", got-before)
	}
}
