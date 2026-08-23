package credential_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
)

// octoFixture is a stand-in issuer plus the projected OIDC token beside it.
type octoFixture struct {
	url      string
	oidc     string
	requests atomic.Int64
	// last is what the most recent exchange carried, read off the wire.
	lastQuery  atomic.Value // url.Values
	lastBearer atomic.Value // string
	// status and body are what the next exchange answers with.
	status    atomic.Int64
	body      atomic.Value // string
	shortBody atomic.Bool
	// started and release make a cache fill observable without putting a
	// timing assumption inside the issuer handler. Tests set them before use.
	started chan<- struct{}
	release <-chan struct{}
}

func newOctoFixture(t *testing.T) *octoFixture {
	t.Helper()
	f := &octoFixture{}
	f.status.Store(int64(http.StatusOK))
	f.body.Store(`{"token":"ghs-exchanged"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.started != nil {
			f.started <- struct{}{}
		}
		if f.release != nil {
			<-f.release
		}
		f.lastQuery.Store(r.URL.Query())
		f.lastBearer.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		if f.shortBody.Load() {
			w.Header().Set("Content-Length", "100")
		}
		w.WriteHeader(int(f.status.Load()))
		fmt.Fprint(w, f.body.Load().(string))
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL

	f.oidc = filepath.Join(t.TempDir(), "oidc-token")
	f.writeOIDC(t, "eyJ-projected")
	return f
}

func (f *octoFixture) writeOIDC(t *testing.T, value string) {
	t.Helper()
	if err := os.WriteFile(f.oidc, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f *octoFixture) block(mutate ...func(map[string]any)) map[string]any {
	b := map[string]any{
		"kind":            "octo_sts",
		"url":             f.url,
		"scope":           "srhg-ai-7cef3f93",
		"identity":        "ben-tracker",
		"oidc_token_path": f.oidc,
	}
	for _, m := range mutate {
		m(b)
	}
	return b
}

func (f *octoFixture) source(t *testing.T, mutate ...func(map[string]any)) core.CredentialSource {
	t.Helper()
	block := f.block(mutate...)
	d, err := credential.OctoSTSKind{}.Describe(block)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	src, err := credential.OctoSTSKind{}.New(d, block)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return src
}

// The exchange carries the **configured** scope and identity and nothing
// derived: no repository, no clone URL, no API URL (SPEC §11, mutation 25).
//
// Read off the wire rather than off the descriptor, because the descriptor is
// what a mistake here would agree with: an implementation that put the
// configured literals in Authority and sent something else would pass every
// key-shaped assertion in the suite.
func TestTheExchangeCarriesTheConfiguredScopeAndIdentity(t *testing.T) {
	f := newOctoFixture(t)
	src := f.source(t)
	if _, err := src.FetchFresh(context.Background(), core.PurposeTracker); err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	q := f.lastQuery.Load().(interface{ Get(string) string })
	if got := q.Get("scope"); got != "srhg-ai-7cef3f93" {
		t.Errorf("scope on the wire = %q, want the configured literal", got)
	}
	if got := q.Get("identity"); got != "ben-tracker" {
		t.Errorf("identity on the wire = %q, want the configured literal", got)
	}
	// The OIDC token authorizes the exchange, and nothing else does.
	if got := f.lastBearer.Load().(string); got != "Bearer eyJ-projected" {
		t.Errorf("Authorization = %q, want the projected OIDC token", got)
	}
	// A purpose partitions a cache; it never selects an identity.
	f.requests.Store(0)
	if _, err := src.FetchFresh(context.Background(), core.PurposePublish); err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	if got := f.lastQuery.Load().(interface{ Get(string) string }).Get("identity"); got != "ben-tracker" {
		t.Errorf("identity under a different purpose = %q, want the configured literal unchanged", got)
	}
}

// The OIDC token is re-read on every exchange: the projection rotates under a
// running process, and a path read once would pin whatever was there at
// startup.
func TestTheOIDCTokenIsReReadOnEveryExchange(t *testing.T) {
	f := newOctoFixture(t)
	src := f.source(t)
	ctx := context.Background()
	if _, err := src.FetchFresh(ctx, core.PurposeTracker); err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	f.writeOIDC(t, "eyJ-rotated")
	if _, err := src.FetchFresh(ctx, core.PurposeTracker); err != nil {
		t.Fatalf("FetchFresh after rotation: %v", err)
	}
	if got := f.lastBearer.Load().(string); got != "Bearer eyJ-rotated" {
		t.Errorf("Authorization = %q, want the rotated projection", got)
	}
}

// An unreadable projection is **permanent**: a deployment that did not mount
// what it said it would fails identically on every retry.
func TestAnUnreadableOIDCPathIsPermanent(t *testing.T) {
	f := newOctoFixture(t)
	src := f.source(t, func(b map[string]any) {
		b["oidc_token_path"] = filepath.Join(t.TempDir(), "never-mounted")
	})
	_, err := src.FetchFresh(context.Background(), core.PurposeTracker)
	if class, ok := core.CredentialFailure(err); !ok || class != core.CredentialPermanent {
		t.Fatalf("FetchFresh = %v (class %v, credential %v), want a permanent credential failure", err, class, ok)
	}
	if f.requests.Load() != 0 {
		t.Error("the issuer was contacted with no OIDC token to present")
	}
}

// The issuer's status decides the class, and the default is **unknown** rather
// than transient: an unclassified status parks, which is the same posture the
// zero value takes and for the same reason (mutations 14, 15).
func TestExchangeStatusClassification(t *testing.T) {
	for _, tt := range []struct {
		status int
		want   core.CredentialErrorClass
		why    string
	}{
		{http.StatusUnauthorized, core.CredentialPermanent, "the trust policy does not admit this identity"},
		{http.StatusForbidden, core.CredentialPermanent, "the trust policy does not admit this identity"},
		{http.StatusNotFound, core.CredentialPermanent, "the scope and identity are unknown to the issuer"},
		{http.StatusRequestTimeout, core.CredentialTransient, "the issuer timed out the request"},
		{http.StatusTooManyRequests, core.CredentialTransient, "the issuer asked us to slow down"},
		{http.StatusInternalServerError, core.CredentialTransient, "the issuer is having a moment"},
		{http.StatusBadGateway, core.CredentialTransient, "so is whatever is in front of it"},
		{http.StatusTeapot, core.CredentialUnknown, "neither obviously configuration nor obviously weather"},
	} {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			f := newOctoFixture(t)
			f.status.Store(int64(tt.status))
			f.body.Store(`{}`)
			_, err := f.source(t).FetchFresh(context.Background(), core.PurposeTracker)
			class, ok := core.CredentialFailure(err)
			if !ok {
				t.Fatalf("FetchFresh = %v, want a classified credential failure", err)
			}
			if class != tt.want {
				t.Errorf("class = %v, want %v — %s", class, tt.want, tt.why)
			}
			// The authority is named so an operator reads a wrong trust policy
			// off the log instead of inferring it from a silent stall.
			if !strings.Contains(err.Error(), "octo:") {
				t.Errorf("error = %v, want it to name the authority", err)
			}
		})
	}
}

// A known HTTP verdict wins even when the issuer truncates its irrelevant
// error body. Otherwise a permanent trust-policy refusal is relabelled as a
// transient transport failure merely because the response body is broken.
func TestExchangeStatusClassificationPrecedesBodyRead(t *testing.T) {
	f := newOctoFixture(t)
	f.status.Store(http.StatusUnauthorized)
	f.body.Store(`{}`)
	f.shortBody.Store(true)

	_, err := f.source(t).FetchFresh(context.Background(), core.PurposeTracker)
	if class, ok := core.CredentialFailure(err); !ok || class != core.CredentialPermanent {
		t.Fatalf("FetchFresh = %v (class %v, credential %v), want the permanent 401 verdict", err, class, ok)
	}
}

// A 200 carrying no credential is a **source defect**, refused permanently
// before anything downstream could use it (mutation 17).
func TestAnEmptyExchangeResponseIsPermanent(t *testing.T) {
	for _, body := range []string{`{}`, `{"token":""}`, `{"token":"   "}`, `{"access_token":""}`} {
		t.Run(body, func(t *testing.T) {
			f := newOctoFixture(t)
			f.body.Store(body)
			_, err := f.source(t).FetchFresh(context.Background(), core.PurposeTracker)
			if !errors.Is(err, core.ErrCredentialEmpty) {
				t.Fatalf("FetchFresh = %v, want core.ErrCredentialEmpty", err)
			}
			if class, _ := core.CredentialFailure(err); class != core.CredentialPermanent {
				t.Errorf("class = %v, want permanent", class)
			}
		})
	}
}

// Octo deployments have returned both spellings. The exchange contract is the
// credential, not one JSON field name, and a valid access_token must not become
// a permanent readiness refusal.
func TestExchangeAcceptsBothTokenSpellings(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want string
	}{
		{"token", `{"token":"ghs-token"}`, "ghs-token"},
		{"access_token", `{"access_token":"ghs-access"}`, "ghs-access"},
		{"token takes precedence", `{"token":"ghs-token","access_token":"ghs-access"}`, "ghs-token"},
		{"blank token falls back", `{"token":"   ","access_token":" ghs-access "}`, "ghs-access"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newOctoFixture(t)
			f.body.Store(tt.body)
			got, err := f.source(t).FetchFresh(context.Background(), core.PurposeTracker)
			if err != nil {
				t.Fatalf("FetchFresh: %v", err)
			}
			if got.Value != tt.want {
				t.Errorf("token = %q, want %q", got.Value, tt.want)
			}
		})
	}
}

// A context that has already ended is the caller's deadline, not the issuer's
// verdict — transient, so §9.8 may retry it.
func TestACancelledExchangeIsTransient(t *testing.T) {
	f := newOctoFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.source(t).FetchFresh(ctx, core.PurposeTracker)
	if class, ok := core.CredentialFailure(err); !ok || class != core.CredentialTransient {
		t.Fatalf("FetchFresh = %v (class %v, credential %v), want transient", err, class, ok)
	}
}

// Fetch is cached and FetchFresh is not: the tracker polls every tick, and an
// exchange per request would multiply the issuer's traffic by the daemon's —
// while a token handed to an agent must cover the whole attempt (mutation 1).
func TestFetchCachesAndFetchFreshDoesNot(t *testing.T) {
	f := newOctoFixture(t)
	src := f.source(t)
	ctx := context.Background()

	if _, err := src.Fetch(ctx, core.PurposeTracker); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := src.Fetch(ctx, core.PurposeTracker); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := f.requests.Load(); got != 1 {
		t.Errorf("two cached fetches cost %d exchanges, want 1", got)
	}

	// Partitioned by purpose, which is the whole of what a purpose does here.
	if _, err := src.Fetch(ctx, core.PurposeWorkspace); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got := f.requests.Load(); got != 2 {
		t.Errorf("a second purpose cost %d exchanges in total, want 2", got)
	}

	before := f.requests.Load()
	if _, err := src.FetchFresh(ctx, core.PurposeTracker); err != nil {
		t.Fatalf("FetchFresh: %v", err)
	}
	if f.requests.Load() != before+1 {
		t.Error("FetchFresh was served from the cache")
	}
}

// A source instance is shared by every GitHub request. At rollover all of those
// callers can miss together, but one purpose still owes the issuer one exchange,
// not one exchange per request.
func TestConcurrentFetchesShareOneExchange(t *testing.T) {
	const callers = 8
	f := newOctoFixture(t)
	started := make(chan struct{}, callers)
	release := make(chan struct{})
	f.started, f.release = started, release
	src := f.source(t)

	begin := make(chan struct{})
	ready := make(chan struct{}, callers)
	done := make(chan error, callers)
	for range callers {
		go func() {
			ready <- struct{}{}
			<-begin
			_, err := src.Fetch(context.Background(), core.PurposeTracker)
			done <- err
		}()
	}
	for range callers {
		<-ready
	}
	close(begin)

	<-started
	duplicate := false
	select {
	case <-started:
		duplicate = true
	case <-time.After(time.Second):
	}
	close(release)
	for range callers {
		if err := <-done; err != nil {
			t.Errorf("Fetch: %v", err)
		}
	}
	if duplicate || f.requests.Load() != 1 {
		t.Errorf("%d concurrent cache misses cost %d exchanges, want 1", callers, f.requests.Load())
	}
}

// The declared TTL is the arithmetic every gate is computed against, so it is
// pinned from the load gate's own operands rather than from the table that
// declares it: fifty minutes is what makes the attempt maximum forty-five.
func TestTheDeclaredTTLLeavesFortyFiveMinutesOfAttempt(t *testing.T) {
	f := newOctoFixture(t)
	d, err := credential.OctoSTSKind{}.Describe(f.block())
	if err != nil {
		t.Fatal(err)
	}
	if got := d.MinFreshTTL - core.CredentialTTLMargin; got != 45*time.Minute {
		t.Errorf("MinFreshTTL %s minus the fixed %s margin = %s, want a 45m attempt maximum",
			d.MinFreshTTL, core.CredentialTTLMargin, got)
	}
}

// The load gate admits the exact 45-minute maximum, so a real fresh exchange
// must leave enough observable life for the matching runtime `>=` gate after
// FetchFresh has returned.
func TestTheAdvertisedAttemptMaximumSurvivesTheFreshExchangeHandoff(t *testing.T) {
	f := newOctoFixture(t)
	src := f.source(t)
	attempt := src.Descriptor().MinFreshTTL - core.CredentialTTLMargin
	_, err := harness.MintPublish(context.Background(), core.PublishBinding{
		Env: "GH_TOKEN", Source: src,
	}, attempt, errors.New("publish credential"))
	if err != nil {
		t.Fatalf("MintPublish at the advertised %s maximum: %v", attempt, err)
	}
}

// Descriptor tuples are an identity boundary, not a display string. Escaping
// must make the encoding injective even though each source field is otherwise
// preserved verbatim (SPEC §5.4, §10.2).
func TestDescriptorFieldsCannotCrossTheirSeparators(t *testing.T) {
	f := newOctoFixture(t)
	describe := func(scope, identity, path string) core.SourceDescriptor {
		t.Helper()
		d, err := credential.OctoSTSKind{}.Describe(f.block(func(b map[string]any) {
			b["scope"] = scope
			b["identity"] = identity
			b["oidc_token_path"] = path
		}))
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		return d
	}

	a := describe("org#ben", "tracker", `/run/oidc`)
	b := describe("org", "ben#tracker", `/run/oidc`)
	if a.Authority == b.Authority {
		t.Errorf("distinct scope/identity tuples share authority %q", a.Authority)
	}
	if a.BindingKey == b.BindingKey {
		t.Errorf("distinct scope/identity tuples share binding key %q", a.BindingKey)
	}

	// Escape characters are data too: a literal backslash followed by `#` must
	// not be another spelling of an escaped separator.
	c := describe(`org\#ben`, "tracker", `/run/oidc`)
	if a.Authority == c.Authority || a.BindingKey == c.BindingKey {
		t.Errorf("literal escape character collapsed descriptor tuples: %+v and %+v", a, c)
	}
}

// URL canonicalization: two spellings of one endpoint are one identity, and a
// component that addresses nothing is refused rather than normalized away
// (mutation 20).
func TestIssuerURLCanonicalization(t *testing.T) {
	describe := func(t *testing.T, raw string) (core.SourceDescriptor, error) {
		t.Helper()
		return credential.OctoSTSKind{}.Describe(map[string]any{
			"kind": "octo_sts", "url": raw, "scope": "org", "identity": "ben",
			"oidc_token_path": "/var/run/secrets/octo/oidc-token",
		})
	}

	t.Run("equal spellings reduce to one authority", func(t *testing.T) {
		for _, pair := range [][2]string{
			{"https://octo.example.com", "https://OCTO.Example.COM"},
			{"https://octo.example.com", "https://octo.example.com:443"},
			{"https://octo.example.com", "https://octo.example.com/"},
			{"http://octo.example.com/sts", "http://OCTO.example.com:80/sts/"},
		} {
			a, err := describe(t, pair[0])
			if err != nil {
				t.Fatalf("Describe(%q): %v", pair[0], err)
			}
			b, err := describe(t, pair[1])
			if err != nil {
				t.Fatalf("Describe(%q): %v", pair[1], err)
			}
			if a.Authority != b.Authority {
				t.Errorf("%q and %q gave %q and %q, want one identity", pair[0], pair[1], a.Authority, b.Authority)
			}
		}
	})

	t.Run("a path is preserved", func(t *testing.T) {
		withPath, err := describe(t, "https://octo.example.com/sts")
		if err != nil {
			t.Fatal(err)
		}
		bare, err := describe(t, "https://octo.example.com")
		if err != nil {
			t.Fatal(err)
		}
		if withPath.Authority == bare.Authority {
			t.Error("a path-mounted issuer shares an identity with the bare host")
		}
	})

	for _, tt := range []struct {
		name, url string
		want      error
	}{
		{"a scheme that is not http(s)", "ftp://octo.example.com", credential.ErrSourceURLScheme},
		{"no scheme at all", "octo.example.com", credential.ErrSourceURLScheme},
		{"userinfo", "https://user:pass@octo.example.com", credential.ErrSourceURL},
		{"a query", "https://octo.example.com?scope=org", credential.ErrSourceURL},
		{"a fragment", "https://octo.example.com#frag", credential.ErrSourceURL},
		{"no host", "https:///sts", credential.ErrSourceURL},
		{"unparseable", "://octo", credential.ErrSourceURL},
	} {
		t.Run("refuses "+tt.name, func(t *testing.T) {
			_, err := describe(t, tt.url)
			if !errors.Is(err, tt.want) {
				t.Errorf("Describe(%q) = %v, want %v", tt.url, err, tt.want)
			}
		})
	}

	// Userinfo is refused rather than stripped, and the refusal never echoes
	// the URL: this is the one field that can carry a credential in a section
	// printed in full (SPEC §5.8, amendment 5).
	t.Run("the userinfo refusal echoes nothing", func(t *testing.T) {
		_, err := describe(t, "https://user:hunter2@octo.example.com")
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("refusal = %v, want one that quotes no part of the URL", err)
		}
	})
}

// Every `octo_sts` field must be a **literal**: a $VAR there would make a
// non-secret field invisible in `config effective` while still deciding which
// trust policy the daemon exchanges against (SPEC §5.5, amendment 3).
func TestOctoFieldsMustBeLiterals(t *testing.T) {
	f := newOctoFixture(t)
	for _, field := range []string{"url", "scope", "identity", "oidc_token_path"} {
		t.Run(field, func(t *testing.T) {
			block := f.block(func(b map[string]any) { b[field] = "$SOME_VAR" })
			if _, err := (credential.OctoSTSKind{}).Describe(block); !errors.Is(err, credential.ErrSourceFieldNotLiteral) {
				t.Errorf("Describe with %s: $SOME_VAR = %v, want ErrSourceFieldNotLiteral", field, err)
			}
			// An interpolation is refused for the same reason, and by the same
			// rule: the field is not a literal.
			block = f.block(func(b map[string]any) { b[field] = "prefix-$SOME_VAR-suffix" })
			if _, err := (credential.OctoSTSKind{}).Describe(block); !errors.Is(err, credential.ErrSourceFieldNotLiteral) {
				t.Errorf("Describe with an interpolated %s = %v, want ErrSourceFieldNotLiteral", field, err)
			}
		})
	}
	t.Run("a non-string field", func(t *testing.T) {
		block := f.block(func(b map[string]any) { b["scope"] = 42 })
		if _, err := (credential.OctoSTSKind{}).Describe(block); !errors.Is(err, credential.ErrSourceFieldType) {
			t.Errorf("Describe with a numeric scope = %v, want ErrSourceFieldType", err)
		}
	})
}
