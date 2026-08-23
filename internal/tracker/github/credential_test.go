package github

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The no-fallback property, asserted as an **absence at the boundary** rather
// than as an error value (SPEC §10.2).
//
// "No source failure falls through to a different credential" is a claim about
// what does *not* happen, and the thing that must not happen is a GitHub request
// with no credential — not a GitHub request that GitHub then rejects. An
// unauthenticated call is unreachable here by construction: the auth transport
// is outermost, so a source failure returns before the conditional cache, the
// request budget or the network are reached at all.
//
// The source's own exchange is a different matter and is not what this asserts:
// a real `octo_sts` source contacts an issuer, and that request is expected.

// failingSource answers with one classified failure, forever, and counts what it
// was asked.
type failingSource struct {
	err   error
	calls int
}

func (s *failingSource) Fetch(context.Context, core.Purpose) (core.Token, error) {
	s.calls++
	return core.Token{}, s.err
}

func (s *failingSource) Descriptor() core.SourceDescriptor {
	return core.SourceDescriptor{
		Kind: "octo_sts", Authority: "octo:https://octo.example#org#ben-tracker",
		BindingKey: "octo:https://octo.example#org#ben-tracker#/run/oidc",
	}
}

// silentSource reports success and hands back nothing — a source defect.
type silentSource struct{ calls int }

func (s *silentSource) Fetch(context.Context, core.Purpose) (core.Token, error) {
	s.calls++
	return core.Token{}, nil
}

func (s *silentSource) Descriptor() core.SourceDescriptor {
	return core.SourceDescriptor{Kind: "static", Authority: "env:GH_TOKEN", BindingKey: "env:GH_TOKEN"}
}

// expiringTrackerSource models octo_sts's five-minute cache margin without
// involving an issuer. The first token is barely cacheable; once the manual
// clock crosses its margin, the next Fetch mints a distinguishable token.
type expiringTrackerSource struct {
	mu    sync.Mutex
	now   func() time.Time
	token core.Token
	calls int
}

func (s *expiringTrackerSource) Fetch(context.Context, core.Purpose) (core.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if !s.now().Before(s.token.UsableUntil.Add(-core.CredentialTTLMargin)) {
		s.token = core.Token{Value: "fresh-tracker-token", UsableUntil: s.now().Add(50 * time.Minute)}
	}
	return s.token, nil
}

func (s *expiringTrackerSource) Descriptor() core.SourceDescriptor {
	return core.SourceDescriptor{
		Kind:        "octo_sts",
		Authority:   "octo:https://octo.example#org#ben-tracker",
		BindingKey:  "octo:https://octo.example#org#ben-tracker#/run/oidc",
		MinFreshTTL: 50 * time.Minute,
	}
}

func (s *expiringTrackerSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestNoGitHubRequestIsIssuedWithoutACredential(t *testing.T) {
	// A credential in the environment throughout, so a fallthrough has somewhere
	// to fall *to*: without this the test would pass on a host that simply has
	// no second credential to reach for.
	t.Setenv(FallbackTokenEnv, "a-credential-a-fallthrough-would-find")

	for _, tt := range []struct {
		name   string
		source core.Source
		want   error
	}{
		{
			name:   "a mint failure",
			source: &failingSource{err: &core.CredentialError{Class: core.CredentialTransient, Authority: "octo:x", Err: errors.New("the issuer answered 503")}},
		},
		{
			name:   "a source that reported success with no credential",
			source: &silentSource{},
			want:   ErrMissingToken,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.serveRepo()
			opts := compileOptions(baseConfig())
			opts.Provider["api_url"] = f.srv.URL
			opts.Credential = tt.source
			a, err := New(opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			if err := a.Ready(context.Background()); err == nil {
				t.Fatal("Ready = nil with no credential to present")
			} else if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Ready = %v, want %v", err, tt.want)
			}
			if reqs, _ := f.snapshot(); len(reqs) != 0 {
				t.Errorf("Ready reached GitHub with no credential: %+v", reqs)
			}

			// And not only at Ready. Every ordinary read goes through the same
			// transport, so the absence holds for the whole adapter rather than
			// for the one call somebody remembered to guard.
			f.reset()
			if _, err := a.Fetch(context.Background()); err == nil {
				t.Fatal("Fetch = nil with no credential to present")
			}
			if reqs, _ := f.snapshot(); len(reqs) != 0 {
				t.Errorf("Fetch reached GitHub with no credential: %+v", reqs)
			}
			// Nothing was charged to the §8.5 budget either: the credential
			// boundary sits above it.
			if spent := a.BeginTick(0); spent.Billed != 0 || spent.Unbilled != 0 {
				t.Errorf("request accounting = %+v, want nothing spent", spent)
			}
		})
	}
}

// Authentication is initially outside the request budget so a source failure
// costs no request. A continuation may then wait a whole slow polling window;
// the credential must be fetched again after that wait or the old bearer can be
// sent after the deadline its source stated (SPEC §5.2.10).
func TestDeferredTrackerRequestRefreshesCredentialBeforeNetwork(t *testing.T) {
	const interval = 10 * time.Minute
	clock := newManualClock()
	oldDeadline := clock.Now().Add(core.CredentialTTLMargin + time.Second)
	source := &expiringTrackerSource{
		now:   clock.Now,
		token: core.Token{Value: "nearly-expired-tracker-token", UsableUntil: oldDeadline},
	}
	opts := compileOptions(baseConfig())
	opts.Credential = source
	adapter, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	resetBudget(adapter.budget, clock.Now)
	adapter.BeginTick(interval)
	spendBilled(t, adapter.budget, ordinaryPerTick)
	adapter.transport.networkNow = clock.Now
	networkAuth := make(chan string, 1)
	adapter.transport.next = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		networkAuth <- req.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})

	ctx, cancel := context.WithTimeout(waitForRequestBudget(context.Background()), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.test/deferred", nil)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := adapter.http.Do(req)
		done <- result{resp: resp, err: err}
	}()

	deadline := time.Now().Add(time.Second)
	for deferredRequests(adapter.budget) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := deferredRequests(adapter.budget); got != 1 {
		t.Fatalf("deferred requests = %d, want 1", got)
	}
	select {
	case got := <-networkAuth:
		t.Fatalf("request reached the network before the next window with %q", got)
	default:
	}

	clock.advance(interval)
	closed := adapter.BeginTick(interval)
	if closed.Billed != ordinaryPerTick || closed.Deferred != 1 {
		t.Errorf("closed report = %+v, want %d billed and one deferred", closed, ordinaryPerTick)
	}
	select {
	case got := <-done:
		if got.resp != nil {
			got.resp.Body.Close()
		}
		if got.err != nil {
			t.Fatalf("deferred request: %v", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("deferred request did not resume")
	}

	if !clock.Now().After(oldDeadline) {
		t.Fatalf("clock = %s, want after old deadline %s", clock.Now(), oldDeadline)
	}
	if got := <-networkAuth; got != "Bearer fresh-tracker-token" {
		t.Errorf("network authorization = %q, want refreshed bearer", got)
	}
	if got := source.callCount(); got != 2 {
		t.Errorf("credential fetches = %d, want initial fetch plus post-admission refresh", got)
	}
}

// A source failure keeps its class through the tracker's whole call path, so the
// severity an operator sees is the source's own verdict (SPEC §9.8,
// amendment 14).
func TestATrackerCredentialFailureKeepsItsClass(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	for _, class := range []core.CredentialErrorClass{
		core.CredentialTransient, core.CredentialPermanent, core.CredentialUnknown,
	} {
		t.Run(class.String(), func(t *testing.T) {
			opts := compileOptions(baseConfig())
			opts.Provider["api_url"] = f.srv.URL
			opts.Credential = &failingSource{
				err: &core.CredentialError{Class: class, Authority: "octo:x#org#ben", Err: errors.New("nope")},
			}
			a, err := New(opts)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = a.Fetch(context.Background())
			got, ok := core.CredentialFailure(err)
			if !ok || got != class {
				t.Errorf("class = (%v, %v), want %v — a re-labelled verdict routes differently", got, ok, class)
			}
			if core.CredentialAuthority(err) != "octo:x#org#ben" {
				t.Errorf("authority = %q, want the source's own", core.CredentialAuthority(err))
			}
		})
	}
}

// The construction boundary refuses what the compilation exists to remove: a
// missing source, and a block that still carries a promoted key (mutation 22).
func TestConstructionRefusesTheKeysTheCompilationPromoted(t *testing.T) {
	t.Run("no credential source at all", func(t *testing.T) {
		opts := compileOptions(baseConfig())
		opts.Credential = nil
		if _, err := New(opts); !errors.Is(err, ErrNoCredentialSource) {
			t.Errorf("New = %v, want ErrNoCredentialSource", err)
		}
	})
	for _, key := range []string{"token", "credential_source", "claim_assignee"} {
		t.Run("a surviving "+key, func(t *testing.T) {
			opts := compileOptions(baseConfig())
			opts.Provider[key] = "leftover"
			if _, err := New(opts); !errors.Is(err, ErrUnknownProviderKey) {
				t.Errorf("New with %q still in the block = %v, want a refusal: a key that survives "+
					"the projection is a second path somebody will read from", key, err)
			}
		})
	}
}
