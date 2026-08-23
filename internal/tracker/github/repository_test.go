package github

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	gh "github.com/google/go-github/v90/github"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// deleteToken drops the block credential, leaving the documented
// $GITHUB_TOKEN fallback as the only one (SPEC §5.8).
func deleteToken(c *core.TrackerConfig) { delete(c.Provider, "token") }

// The base clone fetches from the URL GitHub reported for the repository,
// verbatim (SPEC §6.2). A GitHub Enterprise install's API host and clone host
// are chosen independently, so every row here is a clone URL that some rule for
// deriving one from the other would have rewritten — sending the tracker
// credential to a host the operator never configured.
func TestRepositoryUsesTheServersCloneURL(t *testing.T) {
	for _, clone := range []string{
		"https://github.com/acme/widgets.git",
		// A GHES whose own hostname begins with the label a derivation rule
		// would strip as GitHub's API subdomain.
		"https://api.example.com/acme/widgets.git",
		// The same hazard with the casing host comparisons routinely miss.
		"https://API.Example.COM/acme/widgets.git",
		// A GHES served from a path prefix.
		"https://git.example.com/gh/acme/widgets.git",
	} {
		t.Run(clone, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.serveRepoWithCloneURL(clone)
			a := f.adapter(t)
			if err := a.Ready(context.Background()); err != nil {
				t.Fatalf("Ready: %v", err)
			}

			repo, err := a.Repository(context.Background())
			if err != nil {
				t.Fatalf("Repository: %v", err)
			}
			if repo.RemoteURL != clone {
				t.Errorf("RemoteURL = %q, want the server's clone_url %q unchanged", repo.RemoteURL, clone)
			}
		})
	}
}

// The base-clone credential is the tracker credential (SPEC §10.2), served from
// the same **source** the tracker's own requests are — including the documented
// $GITHUB_TOKEN fallback, which is read per fetch (SPEC §5.5, amendment 3).
func TestRepositoryCredentialIsTheTrackerSource(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	t.Setenv("GITHUB_TOKEN", "daemon-env-token")
	a := f.adapter(t, deleteToken)

	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	repo, err := a.Repository(context.Background())
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	if repo.AuthSource == nil {
		t.Fatal("Repository named no credential source")
	}
	auth, err := repo.AuthSource.Auth(context.Background())
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if auth.Password != "daemon-env-token" {
		t.Fatalf("Auth = %+v, want the credential the source resolves", auth)
	}
	// The secret lives strictly in the password half: the workspace redacts
	// that one value, and a username is not a secret.
	if auth.Username != cloneUsername {
		t.Errorf("Auth.Username = %q, want %q", auth.Username, cloneUsername)
	}

	// A source and not a value, so a rotation reaches the *next* fetch. This is
	// the whole reason the lifetime died: a credential captured at readiness is
	// the one every base fetch afterwards would keep presenting.
	t.Setenv("GITHUB_TOKEN", "rotated-daemon-env-token")
	rotated, err := repo.AuthSource.Auth(context.Background())
	if err != nil {
		t.Fatalf("Auth after rotation: %v", err)
	}
	if rotated.Password != "rotated-daemon-env-token" {
		t.Errorf("Auth after rotation = %+v, want the rotated credential", rotated)
	}
}

// A source that answers with nothing is refused **before git could be
// invoked** (SPEC §10.2). Against a public remote an unauthenticated fetch
// would quietly succeed, which is the fallthrough this refusal exists to stop.
func TestRepositoryAuthRefusesAnEmptyCredential(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	a := f.adapter(t)
	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	repo, err := a.Repository(context.Background())
	if err != nil {
		t.Fatalf("Repository: %v", err)
	}
	// Swap in a source that reports success with no credential — a source
	// defect, which no consumer may discover by making an unauthenticated call.
	empty := remoteAuthSource{src: emptySource{}}
	if _, err := empty.Auth(context.Background()); !errors.Is(err, ErrMissingToken) {
		t.Fatalf("Auth = %v, want the empty-credential refusal", err)
	}
	if _, err := empty.Auth(context.Background()); !errors.Is(err, core.ErrCredentialEmpty) {
		t.Fatalf("Auth = %v, want core.ErrCredentialEmpty", err)
	}
	if class, ok := core.CredentialFailure(mustAuthErr(t, empty)); !ok || class != core.CredentialPermanent {
		t.Errorf("class = (%v, %v), want permanent", class, ok)
	}
	_ = repo
}

func mustAuthErr(t *testing.T, s remoteAuthSource) error {
	t.Helper()
	_, err := s.Auth(context.Background())
	if err == nil {
		t.Fatal("Auth succeeded with an empty credential")
	}
	return err
}

// emptySource reports success and hands back nothing.
type emptySource struct{}

func (emptySource) Fetch(context.Context, core.Purpose) (core.Token, error) {
	return core.Token{}, nil
}

func (emptySource) Descriptor() core.SourceDescriptor {
	return core.SourceDescriptor{Kind: "static", Authority: "env:EMPTY", BindingKey: "env:EMPTY"}
}

// Everything that can fail because the world is not set up belongs to Ready
// (SPEC §5.7). Asking earlier — or after a Ready that failed — is a refusal,
// not a second resolution path: a Repository that read $GITHUB_TOKEN itself
// would answer for a configuration nothing had probed.
func TestRepositoryRefusesUntilReadySucceeds(t *testing.T) {
	t.Run("before Ready", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.serveRepo()
		a := f.adapter(t)

		if _, err := a.Repository(context.Background()); !errors.Is(err, ErrNotReady) {
			t.Fatalf("Repository error = %v, want ErrNotReady", err)
		}
		if reqs, _ := f.snapshot(); len(reqs) != 0 {
			t.Errorf("Repository probed the tracker: %+v", reqs)
		}
	})

	t.Run("after a failed Ready", func(t *testing.T) {
		f := newFakeGitHub(t) // no serveRepo: the reachability probe 404s
		a := f.adapter(t)

		if err := a.Ready(context.Background()); err == nil {
			t.Fatal("Ready = nil for a repository the token cannot reach")
		}
		if _, err := a.Repository(context.Background()); !errors.Is(err, ErrNotReady) {
			t.Fatalf("Repository error = %v, want ErrNotReady", err)
		}
	})

	t.Run("with no credential anywhere", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.serveRepo()
		t.Setenv("GITHUB_TOKEN", "")
		a := f.adapter(t, deleteToken)

		assertPermanentFallbackRefusal(t, a.Ready(context.Background()))
		if _, err := a.Repository(context.Background()); !errors.Is(err, ErrNotReady) {
			t.Fatalf("Repository error = %v, want ErrNotReady", err)
		}
	})
}

// Repository reports what readiness established and spends nothing to do it:
// assembly asks it while wiring the daemon, where a request would turn a
// wiring step into a network dependency and a rate-limit budget line.
func TestRepositoryCostsNoRequest(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	a := f.adapter(t)
	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	before, billedBefore := f.snapshot()

	for range 3 {
		if _, err := a.Repository(context.Background()); err != nil {
			t.Fatalf("Repository: %v", err)
		}
	}

	after, billedAfter := f.snapshot()
	if len(after) != len(before) || billedAfter != billedBefore {
		t.Errorf("Repository issued %d request(s), %d billed; want none",
			len(after)-len(before), billedAfter-billedBefore)
	}
}

// A repository read that names no clone URL is a readiness failure: the base
// clone (SPEC §6.2) would have no remote, and inventing one is exactly the
// guess this contract exists to remove.
func TestReadyRefusesARepoWithNoCloneURL(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepoWithCloneURL("")
	a := f.adapter(t)

	if err := a.Ready(context.Background()); err == nil {
		t.Fatal("Ready = nil for a repository reporting no clone URL")
	}
	if _, err := a.Repository(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Fatalf("Repository error = %v, want ErrNotReady", err)
	}
}

// ClaimPrincipal answers for a readiness that *completed*, which is a stronger
// claim than "a login was resolved at some point" — and the difference is
// reachable, not theoretical. Ready fills the request-saving `principal` cache
// with `/user`'s answer and can still fail afterwards, on the rate gate or on
// the repository probe. An accessor reading that cache would hand assembly a
// principal for a tracker nothing proved usable, and the first claim written
// under it would be what discovered that (SPEC §5.7, §8.4).
func TestClaimPrincipalRefusesUntilReadySucceeds(t *testing.T) {
	t.Run("before Ready", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.serveRepo()
		a := f.adapter(t)

		if _, err := a.ClaimPrincipal(context.Background()); !errors.Is(err, ErrNotReady) {
			t.Fatalf("ClaimPrincipal error = %v, want ErrNotReady", err)
		}
		if reqs, _ := f.snapshot(); len(reqs) != 0 {
			t.Errorf("ClaimPrincipal probed the tracker: %+v", reqs)
		}
	})

	// The case the split field exists for: `/user` answered, so the login is
	// cached, and the readiness that would have published it then failed.
	t.Run("after a Ready that resolved the login and then failed", func(t *testing.T) {
		f := newFakeGitHub(t) // no serveRepo: the reachability probe 404s
		a := f.adapter(t)

		if err := a.Ready(context.Background()); err == nil {
			t.Fatal("Ready = nil for a repository the token cannot reach")
		}
		a.mu.Lock()
		cached := a.principal
		a.mu.Unlock()
		if cached == "" {
			t.Fatal("the login was never resolved, so this proves nothing about reading the cache")
		}

		if _, err := a.ClaimPrincipal(context.Background()); !errors.Is(err, ErrNotReady) {
			t.Fatalf("ClaimPrincipal error = %v, want ErrNotReady; it answered from the request-saving cache", err)
		}
	})

	t.Run("with no credential anywhere", func(t *testing.T) {
		f := newFakeGitHub(t)
		f.serveRepo()
		t.Setenv("GITHUB_TOKEN", "")
		a := f.adapter(t, deleteToken)

		assertPermanentFallbackRefusal(t, a.Ready(context.Background()))
		if _, err := a.ClaimPrincipal(context.Background()); !errors.Is(err, ErrNotReady) {
			t.Fatalf("ClaimPrincipal error = %v, want ErrNotReady", err)
		}
	})
}

// assertPermanentFallbackRefusal pins what an unset $GITHUB_TOKEN now is: the
// implicit source's own **permanent** refusal, naming the variable.
//
// Permanent rather than unknown because an unset variable stays unset until
// somebody acts, so the retry budget would be spent waiting for a human either
// way — and naming the variable is what §5.5 makes the file's own vocabulary.
// It is distinct from ErrMissingToken, which is the boundary's refusal of a
// source that reported *success* with no credential.
func assertPermanentFallbackRefusal(t *testing.T, err error) {
	t.Helper()
	class, ok := core.CredentialFailure(err)
	if !ok || class != core.CredentialPermanent {
		t.Fatalf("Ready error = %v (class %v, credential %v), want a permanent credential failure", err, class, ok)
	}
	if !strings.Contains(err.Error(), FallbackTokenEnv) {
		t.Fatalf("Ready error = %v, want it to name %s", err, FallbackTokenEnv)
	}
}

// After a successful Ready it is the login `/user` reported, and asking again
// costs nothing: assembly asks it while wiring, where a request would turn a
// wiring step into a network dependency and a rate-limit budget line.
func TestClaimPrincipalIsTheReadyLoginAndCostsNoRequest(t *testing.T) {
	f := newFakeGitHub(t)
	f.serveRepo()
	a := f.adapter(t)
	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	before, billedBefore := f.snapshot()

	for range 3 {
		principal, err := a.ClaimPrincipal(context.Background())
		if err != nil {
			t.Fatalf("ClaimPrincipal: %v", err)
		}
		if principal != testLogin {
			t.Fatalf("ClaimPrincipal = %q, want the login readiness resolved (%q)", principal, testLogin)
		}
	}

	after, billedAfter := f.snapshot()
	if len(after) != len(before) || billedAfter != billedBefore {
		t.Errorf("ClaimPrincipal issued %d request(s), %d billed; want none",
			len(after)-len(before), billedAfter-billedBefore)
	}
}

// Readiness is a point-in-time fact, so a *later* failed Ready must retract what
// an earlier successful one published (SPEC §5.7; core.ClaimPrincipalSource,
// core.RepositorySource: "asked after one that returned an error … MUST refuse").
//
// Publishing only on the success path is not sufficient on its own: it leaves the
// previous success standing, which is the same defect one call later. A daemon
// re-checking a rotated or revoked credential would go on claiming as an account
// this token may no longer be, and fetching with a credential the server has
// stopped honouring.
func TestAFailedRecheckRetractsWhatAnEarlierReadyPublished(t *testing.T) {
	f := newFakeGitHub(t)
	// The repository is visible on the first pass and gone on every one after —
	// a repo made private, an install revoked, a token narrowed.
	visible := true
	f.handle("GET /api/v3/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if !visible {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		visible = false
		writeJSON(w, r, &gh.Repository{
			Name:     gh.Ptr(testRepo),
			FullName: gh.Ptr(testOwner + "/" + testRepo),
			CloneURL: gh.Ptr(testCloneURL),
		})
	})
	a := f.adapter(t)

	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("first Ready: %v", err)
	}
	if _, err := a.ClaimPrincipal(context.Background()); err != nil {
		t.Fatalf("ClaimPrincipal after a successful Ready: %v", err)
	}

	if err := a.Ready(context.Background()); err == nil {
		t.Fatal("second Ready = nil for a repository that is no longer visible")
	}
	if _, err := a.ClaimPrincipal(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Errorf("ClaimPrincipal error = %v, want ErrNotReady; the previous success is still standing", err)
	}
	if _, err := a.Repository(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Errorf("Repository error = %v, want ErrNotReady; the previous success is still standing", err)
	}
}

// And the window itself fails closed: between the moment a re-check begins and
// the moment it succeeds, there is no published answer to give. A reader in that
// window costs a retry; the other direction costs a claim written as an account
// the credential may no longer authenticate.
func TestAReadyInProgressHasNoPublishedAnswer(t *testing.T) {
	f := newFakeGitHub(t)

	// Observed from inside the re-check, at the one point that is deterministic
	// without a second seam: the repository read, which happens after the
	// invalidation and before the publication. One handler for both passes,
	// because a ServeMux pattern cannot be re-registered.
	var a *Adapter
	var observe, ran bool
	var during error
	f.handle("GET /api/v3/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if observe {
			ran = true
			_, during = a.ClaimPrincipal(context.Background())
		}
		writeJSON(w, r, &gh.Repository{
			Name:     gh.Ptr(testRepo),
			FullName: gh.Ptr(testOwner + "/" + testRepo),
			CloneURL: gh.Ptr(testCloneURL),
		})
	})
	a = f.adapter(t)
	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("first Ready: %v", err)
	}

	observe = true
	if err := a.Ready(context.Background()); err != nil {
		t.Fatalf("second Ready: %v", err)
	}
	if !ran {
		t.Fatal("the repository read never happened, so nothing was observed mid-Ready")
	}
	if !errors.Is(during, ErrNotReady) {
		t.Errorf("ClaimPrincipal mid-Ready = %v, want ErrNotReady", during)
	}
	// And it is published again once the re-check completes.
	if _, err := a.ClaimPrincipal(context.Background()); err != nil {
		t.Errorf("ClaimPrincipal after the re-check succeeded: %v", err)
	}
}
