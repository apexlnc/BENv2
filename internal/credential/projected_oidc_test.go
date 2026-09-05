package credential

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

const (
	projectedTestIssuer  = "https://oidc.eks.example/id/cluster"
	projectedTestSubject = "system:serviceaccount:ben:ben"
)

func projectedOIDCTestToken(t testing.TB, claims map[string]any) string {
	t.Helper()
	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".fixture-signature"
}

type projectedOIDCFixture struct {
	t         *testing.T
	path      string
	now       time.Time
	block     map[string]any
	lastToken string
}

func newProjectedOIDCFixture(t *testing.T) *projectedOIDCFixture {
	t.Helper()
	f := &projectedOIDCFixture{
		t: t, path: filepath.Join(t.TempDir(), "airlock-token"),
		now: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC),
	}
	f.block = map[string]any{
		"kind": ProjectedOIDCKindName, "issuer": projectedTestIssuer,
		"audience": "airlock-api", "tenant_claim": "sub", "tenant_id": projectedTestSubject,
		"subject": projectedTestSubject, "token_path": f.path, "min_ttl_ms": 300000,
	}
	f.write(map[string]any{"aud": "airlock-api", "nonce": "initial"})
	return f
}

func (f *projectedOIDCFixture) claims(overrides map[string]any) map[string]any {
	claims := map[string]any{
		"iss": projectedTestIssuer, "aud": "airlock-api", "sub": projectedTestSubject,
		"exp": f.now.Add(time.Hour).Unix(),
	}
	for key, value := range overrides {
		if value == nil {
			delete(claims, key)
			continue
		}
		claims[key] = value
	}
	return claims
}

func (f *projectedOIDCFixture) write(overrides map[string]any) string {
	f.t.Helper()
	token := projectedOIDCTestToken(f.t, f.claims(overrides))
	if err := os.WriteFile(f.path, []byte(token+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.lastToken = token
	return token
}

func (f *projectedOIDCFixture) source(mutate ...func(map[string]any)) *projectedOIDCSource {
	f.t.Helper()
	block := make(map[string]any, len(f.block))
	for key, value := range f.block {
		block[key] = value
	}
	for _, fn := range mutate {
		fn(block)
	}
	source, err := (ProjectedOIDCKind{}).New(core.SourceDescriptor{}, block)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	projected := source.(*projectedOIDCSource)
	projected.now = func() time.Time { return f.now }
	return projected
}

func TestProjectedOIDCDescriptorMatchesAirlockOwnership(t *testing.T) {
	f := newProjectedOIDCFixture(t)
	d, err := (ProjectedOIDCKind{}).Describe(f.block)
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != ProjectedOIDCKindName || d.MinFreshTTL != 5*time.Minute {
		t.Fatalf("descriptor = %+v", d)
	}
	if d.PrincipalKey != "airlock-owner:"+projectedTestSubject+"#"+projectedTestSubject {
		t.Errorf("PrincipalKey = %q, want Airlock's (tenant_id, subject) tuple", d.PrincipalKey)
	}
	if strings.Contains(d.PrincipalKey, "client") {
		t.Errorf("PrincipalKey = %q, but Airlock excludes client_id from owner equality", d.PrincipalKey)
	}
	if strings.Contains(d.Authority, f.path) {
		t.Errorf("Authority = %q, want the shared projection path excluded", d.Authority)
	}
	for _, want := range []string{"tenant_claim=sub", "token_path=" + f.path, "min_ttl_ms=300000"} {
		if !strings.Contains(d.BindingKey, want) {
			t.Errorf("BindingKey = %q, want %q", d.BindingKey, want)
		}
	}
	if strings.Contains(d.Authority, f.lastToken) || strings.Contains(d.BindingKey, f.lastToken) ||
		strings.Contains(d.PrincipalKey, f.lastToken) {
		t.Fatal("a descriptor key contains the bearer token")
	}
}

func TestProjectedOIDCReadsAndValidatesEveryRotation(t *testing.T) {
	f := newProjectedOIDCFixture(t)
	src := f.source()
	ctx := context.Background()
	first, err := src.Fetch(ctx, core.PurposeSubstrate)
	if err != nil {
		t.Fatal(err)
	}
	if first.Value != f.lastToken || !first.UsableUntil.Equal(f.now.Add(5*time.Minute)) {
		t.Fatalf("first token = %+v, want the current projection and conservative deadline", first)
	}

	rotated := f.write(map[string]any{
		"aud": []string{"unrelated", "airlock-api"}, "azp": "rotated-client", "nonce": "rotated",
	})
	second, err := src.FetchFresh(ctx, core.PurposeSubstrate)
	if err != nil {
		t.Fatal(err)
	}
	if second.Value != rotated || second.Value == first.Value {
		t.Errorf("rotated value = %q, want the new projection", second.Value)
	}
	if src.Descriptor().PrincipalKey == "" {
		t.Fatal("rotation-safe source lost its stable principal key")
	}
}

func TestProjectedOIDCRefusesWrongOrMalformedClaims(t *testing.T) {
	for _, tc := range []struct {
		name      string
		write     func(*projectedOIDCFixture)
		transient bool
	}{
		{"issuer", func(f *projectedOIDCFixture) { f.write(map[string]any{"iss": "https://other.invalid"}) }, false},
		{"audience", func(f *projectedOIDCFixture) { f.write(map[string]any{"aud": "other-api"}) }, false},
		{"tenant", func(f *projectedOIDCFixture) { f.write(map[string]any{"sub": "raw-claim-sentinel"}) }, false},
		{"subject", func(f *projectedOIDCFixture) {
			f.block["tenant_claim"] = "tenant"
			f.block["tenant_id"] = "ben"
			f.write(map[string]any{"tenant": "ben", "sub": "other-subject"})
		}, false},
		{"missing expiry", func(f *projectedOIDCFixture) { f.write(map[string]any{"exp": nil}) }, false},
		{"string expiry", func(f *projectedOIDCFixture) { f.write(map[string]any{"exp": "9999999999"}) }, false},
		{"fractional expiry", func(f *projectedOIDCFixture) { f.write(map[string]any{"exp": 9999999999.5}) }, false},
		{"near expiry", func(f *projectedOIDCFixture) {
			f.write(map[string]any{"exp": f.now.Add(5*time.Minute - time.Second).Unix()})
		}, true},
		{"not a JWT", func(f *projectedOIDCFixture) {
			f.lastToken = "credential-sentinel-not-a-jwt"
			if err := os.WriteFile(f.path, []byte(f.lastToken), 0o600); err != nil {
				f.t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newProjectedOIDCFixture(t)
			tc.write(f)
			_, err := f.source().FetchFresh(context.Background(), core.PurposeSubstrate)
			if !errors.Is(err, ErrProjectedOIDCToken) {
				t.Fatalf("FetchFresh = %v, want ErrProjectedOIDCToken", err)
			}
			class, ok := core.CredentialFailure(err)
			want := core.CredentialPermanent
			if tc.transient {
				want = core.CredentialTransient
			}
			if !ok || class != want {
				t.Errorf("class = %v, credential = %v; want %v", class, ok, want)
			}
			if strings.Contains(err.Error(), f.lastToken) {
				t.Errorf("error leaks the bearer token: %v", err)
			}
			if strings.Contains(err.Error(), "raw-claim-sentinel") {
				t.Errorf("error leaks a raw claim value: %v", err)
			}
		})
	}
}

func TestProjectedOIDCFileRefusalsAreBoundedAndPermanent(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		f := newProjectedOIDCFixture(t)
		if err := os.Remove(f.path); err != nil {
			t.Fatal(err)
		}
		_, err := f.source().Fetch(context.Background(), core.PurposeSubstrate)
		if !errors.Is(err, ErrProjectedOIDCToken) {
			t.Fatalf("Fetch = %v, want ErrProjectedOIDCToken", err)
		}
		if class, _ := core.CredentialFailure(err); class != core.CredentialPermanent {
			t.Errorf("class = %v, want permanent", class)
		}
	})

	t.Run("oversized", func(t *testing.T) {
		f := newProjectedOIDCFixture(t)
		if err := os.WriteFile(f.path, []byte(strings.Repeat("x", projectedOIDCMaxTokenBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := f.source().Fetch(context.Background(), core.PurposeSubstrate)
		if !errors.Is(err, ErrProjectedOIDCToken) {
			t.Fatalf("Fetch = %v, want ErrProjectedOIDCToken", err)
		}
	})
}

func TestProjectedOIDCSchemaIsStrict(t *testing.T) {
	valid := newProjectedOIDCFixture(t).block
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		want   error
	}{
		{"http issuer", func(b map[string]any) { b["issuer"] = "http://issuer.invalid" }, ErrProjectedOIDCIssuer},
		{"issuer without hostname", func(b map[string]any) { b["issuer"] = "https://:443" }, ErrProjectedOIDCIssuer},
		{"issuer query", func(b map[string]any) { b["issuer"] = projectedTestIssuer + "?token=x" }, ErrProjectedOIDCIssuer},
		{"empty issuer query", func(b map[string]any) { b["issuer"] = projectedTestIssuer + "?" }, ErrProjectedOIDCIssuer},
		{"empty issuer fragment", func(b map[string]any) { b["issuer"] = projectedTestIssuer + "#" }, ErrProjectedOIDCIssuer},
		{"claim path", func(b map[string]any) { b["tenant_claim"] = "nested claim" }, ErrProjectedOIDCClaim},
		{"relative path", func(b map[string]any) { b["token_path"] = "token" }, ErrProjectedOIDCPath},
		{"unclean path", func(b map[string]any) { b["token_path"] = "/var/run/../token" }, ErrProjectedOIDCPath},
		{"zero ttl", func(b map[string]any) { b["min_ttl_ms"] = 0 }, ErrProjectedOIDCTTL},
		{"string ttl", func(b map[string]any) { b["min_ttl_ms"] = "300000" }, ErrProjectedOIDCTTL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := make(map[string]any, len(valid))
			for key, value := range valid {
				block[key] = value
			}
			tc.mutate(block)
			if _, err := (ProjectedOIDCKind{}).Describe(block); !errors.Is(err, tc.want) {
				t.Errorf("Describe = %v, want %v", err, tc.want)
			}
		})
	}
}
