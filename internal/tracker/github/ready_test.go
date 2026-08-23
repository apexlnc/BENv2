package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
)

func withClaimAssignee(login string) func(*core.TrackerConfig) {
	return func(c *core.TrackerConfig) { c.Provider["claim_assignee"] = login }
}

func serveClaimAssignee(f *fakeGitHub, login string, status int) {
	f.handle("GET /api/v3/repos/{owner}/{repo}/assignees/{assignee}", func(w http.ResponseWriter, r *http.Request) {
		if got := r.PathValue("owner") + "/" + r.PathValue("repo"); got != testOwner+"/"+testRepo {
			f.t.Errorf("assignability probe repository = %q, want %s/%s", got, testOwner, testRepo)
		}
		if got := r.PathValue("assignee"); got != login {
			f.t.Errorf("assignability probe login = %q, want %q", got, login)
		}
		w.WriteHeader(status)
	})
}

type readyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f readyRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The retrofit arc (#32, SPEC §5.7): an omitted token passes Structural,
// constructs through the kind, and fails at Ready when the environment holds
// no fallback either — with no request ever leaving the process.
func TestOmittedTokenPassesStructuralAndFailsAtReady(t *testing.T) {
	f := newFakeGitHub(t)
	t.Setenv("GITHUB_TOKEN", "")
	cfg := core.TrackerConfig{
		Provider:       map[string]any{"repo": testOwner + "/" + testRepo, "api_url": f.srv.URL},
		RequiredLabels: []string{"ben-queue"},
		WorkflowKey:    "ben-1a2b3c4d",
	}

	kind := Kind{}
	if err := kind.Structural(cfg); err != nil {
		t.Fatalf("Structural with no credentials anywhere: %v", err)
	}
	adapter, err := kind.New(compileOptions(cfg))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The implicit source over the documented fallback refuses, permanently: an
	// unset variable stays unset until somebody acts, so a retry budget spent
	// waiting for it is spent on nothing (SPEC §8.4).
	err = adapter.Ready(context.Background())
	if class, ok := core.CredentialFailure(err); !ok || class != core.CredentialPermanent {
		t.Fatalf("Ready error = %v (class %v, credential %v), want a permanent credential failure", err, class, ok)
	}
	if !strings.Contains(err.Error(), FallbackTokenEnv) {
		t.Errorf("Ready error = %v, want it to name the documented fallback variable", err)
	}
	if reqs, _ := f.snapshot(); len(reqs) != 0 {
		t.Errorf("Ready dialed the tracker without a credential to present: %+v", reqs)
	}
}

// Ready owns the documented fallback (SPEC §5.8, §8.4): a token omitted from
// the block resolves from $GITHUB_TOKEN at readiness, and the resolved
// credential is the one requests then ride on — the fake reflects a daemon-*
// bearer token back as the login.
func TestReadyResolvesTokenFromEnv(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	t.Setenv("GITHUB_TOKEN", "daemon-env-token")
	adapter := f.adapter(t, func(c *core.TrackerConfig) { delete(c.Provider, "token") })

	if err := adapter.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	principal, err := adapter.claimPrincipal(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if principal != "daemon-env-token" {
		t.Errorf("principal = %q; Ready did not put the env token on the wire", principal)
	}
}

// Ready's two probes attest to the world being set up (SPEC §5.7): the
// credential has an assignable identity, and the configured repository
// answers for it. Both must hit the origin — a cached answer attests to
// nothing.
func TestReadyVerifiesCredentialAndRepo(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	adapter := f.adapter(t)

	if err := adapter.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if got := f.callsExact("GET", "/api/v3/user"); len(got) != 1 || !got[0].Billed {
		t.Errorf("principal probe = %+v, want one billed origin read", got)
	}
	if got := f.callsExact("GET", "/api/v3/repos/"+testOwner+"/"+testRepo); len(got) != 1 || !got[0].Billed {
		t.Errorf("repo probe = %+v, want one billed origin read", got)
	}
}

// A configured assignee replaces `/user`, not the repository probe. Ready
// first proves the repository visible, then asks GitHub whether that account can
// be assigned there. The public identity is normalized at the publication point
// where configured and credential-derived principals converge (SPEC §8.4).
func TestReadyUsesTheConfiguredClaimAssignee(t *testing.T) {
	const configured = "Ben-Claims"
	f := newFakeGitHub(t)
	f.serveRepo()
	serveClaimAssignee(f, configured, http.StatusNoContent)
	a := f.adapter(t, withClaimAssignee(configured))

	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	principal, err := a.ClaimPrincipal(context.Background())
	if err != nil {
		t.Fatalf("ClaimPrincipal: %v", err)
	}
	if principal != "ben-claims" {
		t.Errorf("ClaimPrincipal = %q, want the normalized configured account", principal)
	}
	if got := f.callsExact("GET", "/api/v3/user"); len(got) != 0 {
		t.Errorf("configured claim_assignee still issued /user requests: %+v", got)
	}

	requests, _ := f.snapshot()
	if len(requests) != 2 {
		t.Fatalf("Ready made %d requests, want repository and assignability probes: %+v", len(requests), requests)
	}
	wantPaths := []string{
		"/api/v3/repos/" + testOwner + "/" + testRepo,
		"/api/v3/repos/" + testOwner + "/" + testRepo + "/assignees/" + configured,
	}
	for i, want := range wantPaths {
		if requests[i].Path != want {
			t.Errorf("request %d path = %q, want %q", i, requests[i].Path, want)
		}
	}
}

// 404 is GitHub's complete negative verdict for this endpoint: it covers an
// unknown account and every other reason the account is not assignable. Every
// other failure means the probe did not answer and therefore has a distinct
// named error and remedy.
func TestConfiguredClaimAssigneeProbeClassifiesItsAnswer(t *testing.T) {
	const configured = "Ben-Claims"
	for _, tt := range []struct {
		name      string
		status    int
		transport bool
		want      error
	}{
		{name: "not assignable", status: http.StatusNotFound, want: ErrClaimPrincipalNotAssignable},
		{name: "forbidden", status: http.StatusForbidden, want: ErrClaimPrincipalProbe},
		{name: "server failure", status: http.StatusInternalServerError, want: ErrClaimPrincipalProbe},
		{name: "transport failure", transport: true, want: ErrClaimPrincipalProbe},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.serveRepo()
			a := f.adapter(t, withClaimAssignee(configured))
			if tt.transport {
				a.transport.next = readyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if strings.Contains(req.URL.Path, "/assignees/") {
						return nil, errors.New("assignability transport failed")
					}
					return http.DefaultTransport.RoundTrip(req)
				})
			} else {
				serveClaimAssignee(f, configured, tt.status)
			}

			err := a.Ready(context.Background())
			if !errors.Is(err, tt.want) {
				t.Fatalf("Ready error = %v, want %v", err, tt.want)
			}
			if tt.want == ErrClaimPrincipalProbe && errors.Is(err, ErrClaimPrincipalNotAssignable) {
				t.Errorf("indeterminate probe reported the configured account as not assignable: %v", err)
			}
			if !strings.Contains(err.Error(), configured) || !strings.Contains(err.Error(), testOwner+"/"+testRepo) {
				t.Errorf("error = %q, want the configured account and repository", err)
			}
			if _, err := a.ClaimPrincipal(context.Background()); !errors.Is(err, ErrNotReady) {
				t.Errorf("ClaimPrincipal after failed Ready = %v, want ErrNotReady", err)
			}
			if got := f.callsExact("GET", "/api/v3/user"); len(got) != 0 {
				t.Errorf("configured claim_assignee still issued /user requests: %+v", got)
			}
		})
	}
}

// A standing rate window is a timing verdict, not evidence about whether the
// configured account is assignable. Preserve it bare so the operator is told
// when to retry rather than being pointed at the account.
func TestConfiguredClaimAssigneeProbePreservesStandingRateLimit(t *testing.T) {
	now := time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC)
	cause := errors.New("server asked this credential to wait")
	f := newFakeGitHub(t)
	a := f.adapter(t, withClaimAssignee("Ben-Claims"))
	a.gate = &rateGate{
		now:   func() time.Time { return now },
		until: now.Add(time.Minute),
		last:  &RateLimitError{Secondary: true, RetryAfter: time.Minute, Err: cause},
	}

	err := a.checkClaimPrincipal(context.Background(), "Ben-Claims")
	var limit *RateLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("checkClaimPrincipal error = %v, want the standing RateLimitError", err)
	}
	if errors.Is(err, ErrClaimPrincipalProbe) {
		t.Errorf("standing rate limit was misreported as an identity probe failure: %v", err)
	}
	if got := f.calls("GET", "/assignees/"); len(got) != 0 {
		t.Errorf("standing rate limit still issued an assignability request: %+v", got)
	}
}

// The assignability question belongs after repository visibility. If the
// credential cannot reach the repository, no answer about one of its assignees
// can make the adapter ready and spending that request would misreport the
// earlier failure.
func TestConfiguredClaimAssigneeIsNotProbedBeforeTheRepository(t *testing.T) {
	const configured = "Ben-Claims"
	f := newFakeGitHub(t) // no serveRepo: repository probe returns 404
	serveClaimAssignee(f, configured, http.StatusNoContent)
	a := f.adapter(t, withClaimAssignee(configured))

	if err := a.Ready(context.Background()); err == nil {
		t.Fatal("Ready = nil for an invisible repository")
	}
	if got := f.calls("GET", "/assignees/"); len(got) != 0 {
		t.Errorf("assignability was probed after repository visibility failed: %+v", got)
	}
}

// The fallback source reaches the same publication point as a configured
// assignee. Normalizing only the configured value at parse would leave this
// mixed-case login distinct across an unset → configured reload.
func TestReadyNormalizesTheCredentialLoginAtPublication(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	a := f.adapter(t, func(c *core.TrackerConfig) { c.Provider["token"] = "daemon-Ben-Claims" })

	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	principal, err := a.ClaimPrincipal(context.Background())
	if err != nil {
		t.Fatalf("ClaimPrincipal: %v", err)
	}
	if principal != "daemon-ben-claims" {
		t.Errorf("ClaimPrincipal = %q, want the credential login normalized at publication", principal)
	}
}

// A failed reload candidate is discarded, but the requests it made are not.
// Every candidate rebuilt from the published generation must spend the same
// endpoint/account allowance or a bad config retried each tick can mint a fresh
// hourly budget each time.
func TestReadyContinuesRequestControlAcrossDiscardedGenerations(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	predecessor := f.adapter(t)
	if err := predecessor.Ready(context.Background()); err != nil {
		t.Fatalf("predecessor Ready: %v", err)
	}
	leaveOrdinaryBudget(t, predecessor, 2)
	f.reset()

	badRepo := func(c *core.TrackerConfig) {
		c.Provider["repo"] = testOwner + "/missing"
		// go-github canonicalizes both spellings to the same enterprise API
		// endpoint. Comparing the raw config strings would mint a fresh control
		// for this failed candidate on every reload attempt.
		c.Provider["api_url"] = f.srv.URL + "/"
	}
	discarded := f.adapter(t, badRepo)
	if discarded.client.BaseURL() != predecessor.client.BaseURL() {
		t.Fatalf("test endpoints differ after canonicalization: %q != %q", discarded.client.BaseURL(), predecessor.client.BaseURL())
	}
	discarded.ContinueRequestControl(predecessor)
	if err := discarded.Ready(context.Background()); err == nil {
		t.Fatal("discarded candidate Ready = nil for a missing repository")
	}
	if discarded.budget != predecessor.budget {
		t.Error("discarded candidate received a fresh request budget")
	}
	if discarded.gate != predecessor.gate {
		t.Error("same-token candidate received a fresh rate-limit gate")
	}
	if got, _ := f.snapshot(); len(got) != 2 {
		t.Fatalf("first failed candidate made %d requests, want identity and repository probes", len(got))
	}

	f.reset()
	next := f.adapter(t, badRepo)
	next.ContinueRequestControl(predecessor)
	if err := next.Ready(context.Background()); !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("next Ready error = %v, want ErrRequestBudget from shared allowance", err)
	}
	if got, _ := f.snapshot(); len(got) != 0 {
		t.Errorf("next failed candidate reached GitHub after shared budget was spent: %+v", got)
	}
}

// A reload that moves the daemon to a *new* API endpoint has no published
// predecessor there: the standing generation's controls belong to the old host,
// and adopting them would meter this endpoint against another one's allowance.
// The candidate an earlier attempt discarded is the only generation that has
// spent requests at the new endpoint — and revalidation retries a failing config
// every tick, so it is what stops each retry from arriving with a fresh burst.
func TestReadyContinuesRequestControlFromADiscardedCandidateAtANewEndpoint(t *testing.T) {
	published := newFakeGitHub(t)
	published.serveRepo()
	standing := published.adapter(t)
	if err := standing.Ready(context.Background()); err != nil {
		t.Fatalf("published Ready: %v", err)
	}

	// No serveRepo: the endpoint the config moves to does not answer for the
	// repository, so every candidate built against it fails Ready.
	moved := newFakeGitHub(t)
	first := moved.adapter(t)
	first.ContinueRequestControl(standing)
	if first.budget == standing.budget {
		t.Fatal("a candidate at a new endpoint adopted the published endpoint's allowance")
	}
	if err := first.Ready(context.Background()); err == nil {
		t.Fatal("first candidate Ready = nil for a repository the new endpoint does not serve")
	}
	if got, _ := moved.snapshot(); len(got) != 2 {
		t.Fatalf("first candidate made %d requests, want identity and repository probes", len(got))
	}
	leaveOrdinaryBudget(t, first, 0)
	moved.reset()

	next := moved.adapter(t)
	next.ContinueRequestControl(standing, first)
	if next.budget != first.budget {
		t.Error("the next candidate at the new endpoint received a fresh request budget")
	}
	if err := next.Ready(context.Background()); !errors.Is(err, ErrRequestBudget) {
		t.Fatalf("next Ready error = %v, want ErrRequestBudget from the endpoint's spent allowance", err)
	}
	if next.gate != first.gate {
		t.Error("the next candidate at the new endpoint received a fresh rate-limit gate")
	}
	if got, _ := moved.snapshot(); len(got) != 0 {
		t.Errorf("the retried candidate reached the new endpoint again: %+v", got)
	}
}

// The key is what two generations are judged the same endpoint by, so a spelling
// that changes it without changing the server mints a second allowance for one
// endpoint — and rotating spellings would mint one per reload. Config refuses the
// components that address nothing (ErrAPIURLNotAnEndpoint); these are the rest,
// plus those same components again, because a key this load-bearing should not
// depend on a check in another file to be right.
func TestRequestControlKeyIsTheEndpointAndNothingElse(t *testing.T) {
	const canonical = "https://ghe.example.com/api/v3/"
	same := []struct{ name, base string }{
		{"as written", canonical},
		{"uppercase host", "https://GHE.Example.COM/api/v3/"},
		{"uppercase scheme", "HTTPS://ghe.example.com/api/v3/"},
		{"explicit default port", "https://ghe.example.com:443/api/v3/"},
		{"fragment", canonical + "#a"},
		{"query", canonical + "?tenant=a"},
		{"userinfo", "https://ben:hunter2@ghe.example.com/api/v3/"},
	}
	for _, tt := range same {
		t.Run(tt.name, func(t *testing.T) {
			if got := requestControlKey(tt.base); got != canonical {
				t.Errorf("requestControlKey(%q) = %q, want %q", tt.base, got, canonical)
			}
			if strings.Contains(requestControlKey(tt.base), "hunter2") {
				t.Error("the key carries a credential (core.RequestControlDomain)")
			}
		})
	}

	// The other boundary: merging is safe only where the endpoint really is the
	// same. A key that collapsed these would let one host's backoff silence
	// another's traffic.
	distinct := []struct{ name, base string }{
		{"another host", "https://other.example.com/api/v3/"},
		{"another path", "https://ghe.example.com/gh/api/v3/"},
		{"another scheme", "http://ghe.example.com/api/v3/"},
		{"a non-default port", "https://ghe.example.com:8443/api/v3/"},
		{"an IPv6 literal", "https://[2001:db8::1]/api/v3/"},
		{"an unreadable URL", "://ghe"},
	}
	seen := map[string]string{canonical: "canonical"}
	for _, tt := range distinct {
		got := requestControlKey(tt.base)
		if prior, dup := seen[got]; dup {
			t.Errorf("requestControlKey(%q) = %q, already held by %s", tt.base, got, prior)
		}
		seen[got] = tt.name
	}
	if got := requestControlKey("https://[2001:DB8::1]:443/api/v3/"); got != "https://[2001:db8::1]/api/v3/" {
		t.Errorf("IPv6 key = %q, want the bracketed canonical form", got)
	}
}

// The same fact through the adapters that use it: two generations spelled
// differently, one endpoint, one allowance. No network — continuity is decided
// before Ready.
func TestContinuityFollowsTheCanonicalEndpoint(t *testing.T) {
	f := newFakeGitHub(t)
	at := func(apiURL string) *Adapter {
		return f.adapter(t, func(c *core.TrackerConfig) { c.Provider["api_url"] = apiURL })
	}
	predecessor := at("https://ghe.example.com/")

	spelledDifferently := at("https://GHE.example.com:443/")
	spelledDifferently.ContinueRequestControl(predecessor)
	if spelledDifferently.budget != predecessor.budget {
		t.Error("a second spelling of one endpoint received a second allowance")
	}

	elsewhere := at("https://ghe.example.com/gh/")
	elsewhere.ContinueRequestControl(predecessor)
	if elsewhere.budget == predecessor.budget {
		t.Error("a genuinely different endpoint adopted another's allowance")
	}
}

// Server-directed backoff is keyed by **(API endpoint, credential source
// authority)** — and therefore **survives a rotation** (SPEC §8.5,
// amendment 11).
//
// This is the inversion the token-keyed gate got wrong. A gate keyed by the
// token abandons its backoff on every refresh, which with a fifty-minute
// credential is every fifty minutes, and precisely when a server has just asked
// the daemon to slow down; it also accumulates one entry per rotation for the
// life of the process. An authority does not move when a token does, so the
// refusal is still standing on the other side.
//
// It still survives a *discarded candidate*, which is what the endpoint-scoped
// control buys: a failed reload's Retry-After is what the next candidate honours
// instead of spending a request to rediscover it.
func TestBackoffSurvivesRotationAndDiscardedGenerations(t *testing.T) {
	f := newFakeGitHub(t)
	// One authority, two token values — an ordinary rotation of the credential
	// behind `env:GITHUB_TOKEN`.
	authority := credential.EnvAuthority(FallbackTokenEnv)
	fromEnv := func(c *core.TrackerConfig) { delete(c.Provider, "token") }

	t.Setenv(FallbackTokenEnv, "before-rotation")
	predecessor := f.adapter(t, fromEnv)
	f.rateLimitAll(limitResponse{retryAfterSeconds: 60, secondary: true})
	if err := predecessor.Ready(context.Background()); err == nil {
		t.Fatal("the first candidate ignored the server rate limit")
	}
	if got, _ := f.snapshot(); len(got) != 1 {
		t.Fatalf("the first candidate made %d requests, want one rate-limited identity probe", len(got))
	}

	f.reset()
	t.Setenv(FallbackTokenEnv, "after-rotation")
	next := f.adapter(t, fromEnv)
	next.ContinueRequestControl(predecessor)
	if next.gate != predecessor.gate {
		t.Error("a rotated credential received a fresh gate; the backoff was abandoned on refresh")
	}
	if err := next.Ready(context.Background()); err == nil {
		t.Fatal("the rotated candidate ignored the retained backoff")
	}
	if got, _ := f.snapshot(); len(got) != 0 {
		t.Errorf("retained backoff still reached GitHub after a rotation: %+v", got)
	}
	// And the key really is the authority, not the value: one entry, however
	// many times the credential rotates.
	if got := len(predecessor.control.gates); got != 1 {
		t.Errorf("the control holds %d gates after a rotation, want 1 per authority", got)
	}
	if _, ok := predecessor.control.gates[authority]; !ok {
		t.Errorf("the gate is not held under the authority %q: %v", authority, predecessor.control.gates)
	}
}

// A different authority is a different gate, and two endpoints under one
// authority still hold separate gates — the pair is the key, not either half
// (SPEC §8.5, amendment 11).
func TestGatesArePerEndpointAndAuthority(t *testing.T) {
	f := newFakeGitHub(t)
	base := f.adapter(t)

	// A genuinely different *identity*, not merely a different value: the block
	// credential is `site:tracker.provider.token`, the fallback is
	// `env:GITHUB_TOKEN`. A second value at one site is a rotation and shares a
	// gate on purpose (see the test above).
	other := f.adapter(t, deleteToken)
	other.ContinueRequestControl(base)
	if other.gate == base.gate {
		t.Error("two credential identities at one endpoint share a backoff gate")
	}

	// Same credential, a genuinely different endpoint: ContinueRequestControl
	// refuses the adoption, so the gate comes from a control of its own.
	elsewhere := f.adapter(t, func(c *core.TrackerConfig) { c.Provider["api_url"] = "https://ghe.example.com/" })
	elsewhere.ContinueRequestControl(base)
	if elsewhere.gate == base.gate {
		t.Error("one authority at two endpoints shares a backoff gate")
	}
}

// A repository the credential cannot see is a readiness refusal at startup,
// not a surprise on the first poll (SPEC §5.7).
func TestReadyRefusesAnInvisibleRepo(t *testing.T) {
	f := newFakeGitHub(t) // no serveRepo: the repo read 404s
	adapter := f.adapter(t)

	if err := adapter.Ready(context.Background()); err == nil {
		t.Fatal("Ready = nil for a repository the token cannot reach")
	}
}
