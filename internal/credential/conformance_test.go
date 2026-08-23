package credential

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential/credtest"
)

// Both registered kinds run the shared suite unmodified (SPEC §5.7). What each
// case supplies is only what genuinely differs: a minimal block, a second valid
// value per field, and a way to rotate the credential the world holds.

func TestStaticConformsToTheSourceContract(t *testing.T) {
	const variable = "BEN_TEST_STATIC_CRED"
	rotation := 0
	t.Setenv(variable, "value-0")
	credtest.Contract(t, credtest.Case{
		Name:  StaticKindName,
		Kind:  StaticKind{},
		Block: func() map[string]any { return map[string]any{"kind": "static", "value": "$" + variable} },
		Alternatives: map[string]any{
			"value": "$BEN_TEST_STATIC_CRED_OTHER",
		},
		Required: []string{"value"},
		Secrets:  []string{"value-0", "value-1"},
		Rotate: func(t *testing.T) {
			rotation++
			t.Setenv(variable, "value-"+strconv.Itoa(rotation))
		},
		// A variable nothing ever set: describing it must not read the
		// environment either, and an unset one is only ever a *fetch* failure.
		UnreachableBlock: func() map[string]any {
			return map[string]any{"kind": "static", "value": "$BEN_TEST_STATIC_NEVER_SET"}
		},
	})
}

func TestOctoSTSConformsToTheSourceContract(t *testing.T) {
	var minted atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"token":"ghs-%d"}`, minted.Add(1))
	}))
	t.Cleanup(srv.Close)

	oidc := filepath.Join(t.TempDir(), "oidc-token")
	if err := os.WriteFile(oidc, []byte("eyJ-projected-0"), 0o600); err != nil {
		t.Fatal(err)
	}
	credtest.Contract(t, credtest.Case{
		Name: OctoSTSKindName,
		Kind: OctoSTSKind{},
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
		Secrets:  []string{"ghs-1", "eyJ-projected-0"},
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
	})
}
