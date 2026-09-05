package credential

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential/credtest"
)

// All registered kinds run the shared suite unmodified (SPEC §5.7). What each
// case supplies is only what genuinely differs: a minimal block, a second valid
// value per field, and a way to rotate the credential the world holds.
//
// The cases are a table rather than three separate tests so that "every
// registered kind runs the suite" is itself assertable — see
// ConformanceKinds and TestTheConformanceSuiteCoversTheClosedSourceSet, which
// pins this table to the registry's closed set.
var conformanceCases = map[string]func(t *testing.T) credtest.Case{
	StaticKindName:        staticConformanceCase,
	OctoSTSKindName:       octoSTSConformanceCase,
	ProjectedOIDCKindName: projectedOIDCConformanceCase,
}

// ConformanceKinds names the kinds this package runs credtest.Contract over.
//
// Exported from a _test.go file because the assertion pinning it to the closed
// registered set must live in credential_test: `internal/registry` imports this
// package, so package credential cannot import the registry, and the set that
// must not shrink is the registry's.
func ConformanceKinds() []string { return slices.Sorted(maps.Keys(conformanceCases)) }

func TestEveryRegisteredKindConformsToTheSourceContract(t *testing.T) {
	// Contract names its own subtests after the kind, so no wrapping t.Run.
	for _, name := range ConformanceKinds() {
		credtest.Contract(t, conformanceCases[name](t))
	}
}

func staticConformanceCase(t *testing.T) credtest.Case {
	const variable = "BEN_TEST_STATIC_CRED"
	rotation := 0
	t.Setenv(variable, "value-0")
	return credtest.Case{
		Name:  StaticKindName,
		Kind:  StaticKind{},
		Block: func() map[string]any { return map[string]any{"kind": "static", "value": "$" + variable} },
		Alternatives: map[string]any{
			"value": "$BEN_TEST_STATIC_CRED_OTHER",
		},
		Required:          []string{"value"},
		NoRemoteEndpoints: true,
		Secrets:           []string{"value-0", "value-1"},
		Rotate: func(t *testing.T) {
			rotation++
			t.Setenv(variable, "value-"+strconv.Itoa(rotation))
		},
		// A variable nothing ever set: describing it must not read the
		// environment either, and an unset one is only ever a *fetch* failure.
		UnreachableBlock: func() map[string]any {
			return map[string]any{"kind": "static", "value": "$BEN_TEST_STATIC_NEVER_SET"}
		},
	}
}

func octoSTSConformanceCase(t *testing.T) credtest.Case {
	var minted atomic.Int64
	// TLS, not plaintext: the kind refuses an `http://` issuer (#245), because
	// the exchange presents the projected OIDC token as a bearer credential.
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":"ghs-%d"}`, minted.Add(1))
	}))
	t.Cleanup(srv.Close)

	oidc := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(oidc, []byte("eyJ-projected-0"), 0o600); err != nil {
		t.Fatal(err)
	}
	return credtest.Case{
		Name: OctoSTSKindName,
		// The registered kind's own schema and exchange; only httptest's
		// self-signed certificate is trusted, which nothing else would.
		Kind: OctoKindTrustingForTest(srv.Client().Transport),
		Block: func() map[string]any {
			return map[string]any{
				"kind":            "octo_sts",
				"url":             srv.URL,
				"scope":           "srhg-ai-7cef3f93",
				"identity":        "ben-tracker",
				"oidc_token_path": oidc,
			}
		},
		Alternatives: map[string]any{
			"url":      "https://octo.elsewhere.invalid",
			"scope":    "another-org",
			"identity": "ben-publish",
			// The field Authority deliberately ignores and BindingKey must not:
			// this is mutation 9 in test form.
			"oidc_token_path": "/var/run/secrets/octo/other-token",
		},
		Required: []string{"url", "scope", "identity", "oidc_token_path"},
		Bounded:  true,
		// Unlike an arbitrary static bearer, the issuer/scope/identity tuple is
		// the trust-policy identity every minted token represents.
		StablePrincipal: true,
		Secrets:         []string{"ghs-1", "eyJ-projected-0"},
		// The exchange mints a new credential every time it is asked, which is
		// what a real STS does; the projected OIDC token rotates beside it, so
		// the re-read is exercised too.
		Rotate: func(t *testing.T) {
			if err := os.WriteFile(oidc, []byte("eyJ-projected-rotated"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		AdvancePastDeadline: func(t *testing.T, source core.CredentialSource, deadline time.Time) {
			t.Helper()
			octo, ok := source.(*octoSource)
			if !ok {
				t.Fatalf("source = %T, want *octoSource", source)
			}
			now := deadline.Add(time.Nanosecond)
			octo.mu.Lock()
			octo.now = func() time.Time { return now }
			octo.mu.Unlock()
		},
		// A path that does not exist and a host nothing listens on: Describe
		// must still answer, immediately.
		UnreachableBlock: func() map[string]any {
			return map[string]any{
				"kind":            "octo_sts",
				"url":             "https://127.0.0.1:1",
				"scope":           "org",
				"identity":        "ben",
				"oidc_token_path": filepath.Join(t.TempDir(), "does-not-exist"),
			}
		},
	}
}

func projectedOIDCConformanceCase(t *testing.T) credtest.Case {
	now := time.Now().UTC().Truncate(time.Second)
	tokenPath := filepath.Join(t.TempDir(), "airlock-token")
	rotation := 0
	write := func(t *testing.T) string {
		t.Helper()
		rotation++
		token := projectedOIDCTestToken(t, map[string]any{
			"iss": "https://oidc.eks.example/id/cluster", "aud": []string{"airlock-api", "other"},
			"sub": "system:serviceaccount:ben:ben", "exp": now.Add(2 * time.Hour).Unix(),
			"nonce": rotation,
		})
		if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
			t.Fatal(err)
		}
		return token
	}
	initial := write(t)
	return credtest.Case{
		Name: ProjectedOIDCKindName,
		Kind: ProjectedOIDCKind{},
		Block: func() map[string]any {
			return map[string]any{
				"kind": ProjectedOIDCKindName, "issuer": "https://oidc.eks.example/id/cluster",
				"audience": "airlock-api", "tenant_claim": "sub",
				"tenant_id": "system:serviceaccount:ben:ben",
				"subject":   "system:serviceaccount:ben:ben", "token_path": tokenPath,
				"min_ttl_ms": 300000,
			}
		},
		Alternatives: map[string]any{
			"issuer": "https://issuer.elsewhere.example", "audience": "another-api",
			"tenant_claim": "tenant_id", "tenant_id": "another-tenant",
			"subject":    "system:serviceaccount:ben:other",
			"token_path": filepath.Join(t.TempDir(), "other-token"), "min_ttl_ms": 600000,
		},
		Required: []string{
			"issuer", "audience", "tenant_claim", "tenant_id", "subject", "token_path", "min_ttl_ms",
		},
		Bounded:         true,
		StablePrincipal: true,
		Secrets:         []string{initial},
		Rotate: func(t *testing.T) {
			write(t)
		},
		AdvancePastDeadline: func(t *testing.T, source core.CredentialSource, deadline time.Time) {
			t.Helper()
			projected, ok := source.(*projectedOIDCSource)
			if !ok {
				t.Fatalf("source = %T, want *projectedOIDCSource", source)
			}
			projected.now = func() time.Time { return deadline.Add(time.Nanosecond) }
		},
		UnreachableBlock: func() map[string]any {
			return map[string]any{
				"kind": ProjectedOIDCKindName, "issuer": "https://issuer.invalid",
				"audience": "airlock-api", "tenant_claim": "sub", "tenant_id": "tenant",
				"subject": "subject", "token_path": filepath.Join(t.TempDir(), "does-not-exist"),
				"min_ttl_ms": 300000,
			}
		},
	}
}
