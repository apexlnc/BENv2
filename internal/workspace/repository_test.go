package workspace

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
	"github.com/srhg-ai-7cef3f93/ben/internal/tracker/github"
)

// sourceTracker is a tracker that can name its repository; the embedded
// interface is nil because RepositoryFrom asks for exactly one method and
// calling any other would be the bug this stands in to catch.
type sourceTracker struct {
	core.TrackerAdapter
	repo core.Repository
	err  error
}

func (t sourceTracker) Repository(context.Context) (core.Repository, error) {
	return t.repo, t.err
}

// plainTracker implements the §8.2 contract and nothing more — a tracker whose
// issues have no git repository behind them.
type plainTracker struct{ core.TrackerAdapter }

func TestRepositoryFrom(t *testing.T) {
	parallel(t)
	ctx := context.Background()
	auth := fake.NewRemoteAuth("x-access-token", "t0ken")
	want := core.Repository{
		RemoteURL:  "https://github.com/acme/widgets.git",
		AuthSource: auth,
	}

	t.Run("passes the tracker's answer through", func(t *testing.T) {
		got, err := RepositoryFrom(ctx, sourceTracker{repo: want})
		if err != nil {
			t.Fatalf("RepositoryFrom: %v", err)
		}
		if got.RemoteURL != want.RemoteURL || got.AuthSource != want.AuthSource {
			t.Errorf("Repository = %+v, want %+v", got, want)
		}
		// The *source* crosses the seam, never a value: nothing here has asked
		// for a credential yet, and the first thing to do so will be a git
		// invocation that needs one (SPEC §6.2, amendment 6).
		if auth.Calls() != 0 {
			t.Errorf("the credential was obtained %d times crossing the seam; want 0", auth.Calls())
		}
	})

	t.Run("a tracker that cannot name a repository is refused", func(t *testing.T) {
		_, err := RepositoryFrom(ctx, plainTracker{})
		if !errors.Is(err, ErrNoRepositorySource) {
			t.Fatalf("error = %v, want ErrNoRepositorySource", err)
		}
	})

	t.Run("the tracker's own refusal survives", func(t *testing.T) {
		sentinel := errors.New("no credential")
		_, err := RepositoryFrom(ctx, sourceTracker{err: sentinel})
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want the adapter's refusal wrapped", err)
		}
	})
}

// fakeTracker serves the two reads github's Ready makes — the credential's
// identity and the repository's visibility — and nothing else. The clone URL
// it reports has a host of its own: the daemon fetches from what the server
// names, not from anything derived off the API URL.
func fakeTracker(t *testing.T, cloneURL string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v3/user":
			fmt.Fprint(w, `{"login":"ben-bot"}`)
		case "/api/v3/repos/acme/widgets":
			fmt.Fprintf(w, `{"name":"widgets","full_name":"acme/widgets","clone_url":%q}`, cloneURL)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// The seam end to end (SPEC §5.2.5, §6.2, §10.2): a WORKFLOW.md the loader
// accepted produces a real GitHub adapter, readiness establishes the repository
// and the credential, and the git-worktree provider is built from that answer
// alone. Nothing outside the adapter parses `tracker.provider`.
func TestProviderFromLoadedWorkflow(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("BEN_TEST_TRACKER_TOKEN", "t0ken-from-env")
	const cloneURL = "https://api.example.com/acme/widgets.git"
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`---
tracker:
  kind: github
  provider:
    repo: acme/widgets
    token: $BEN_TEST_TRACKER_TOKEN
    api_url: %s
  required_labels: ["ben-queue"]
workspace:
  root: %s
hooks:
  after_create: "echo bootstrap"
agent:
  kind: claude-code
  provider:
    permission_mode: bypassPermissions
deployment:
  mode: attended
---
Work on {{ issue.title }}.
`, fakeTracker(t, cloneURL), root)), 0o644); err != nil {
		t.Fatal(err)
	}

	def, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	// The legacy `token: $VAR` spelling compiles into an implicit source, and
	// the adapter is constructed from that source alone (SPEC §8, amendment 9).
	sources, err := def.Config.NewSources(registry.Source)
	if err != nil {
		t.Fatalf("NewSources: %v", err)
	}
	binding := def.Config.TrackerBinding()
	tracker, err := github.Kind{}.New(core.TrackerOptions{
		Provider:       binding.Provider,
		RequiredLabels: binding.RequiredLabels,
		ActiveStates:   binding.ActiveStates,
		TerminalStates: binding.TerminalStates,
		WorkflowKey:    def.Key,
		ClaimAssignee:  binding.ClaimAssignee,
		Credential:     sources.Tracker,
	})
	if err != nil {
		t.Fatalf("github.Kind.New: %v", err)
	}
	// Assembly order (SPEC §11): Structural → New → Ready, and only then is
	// there a repository to build a workspace from.
	if err := tracker.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	repo, err := RepositoryFrom(context.Background(), tracker)
	if err != nil {
		t.Fatalf("RepositoryFrom: %v", err)
	}
	p, err := New(Options{
		Root:        def.Config.Workspace.Root,
		WorkflowKey: def.Key,
		Repository:  repo,
		Hooks: Hooks{
			AfterCreate: def.Config.Hooks.AfterCreate,
			Timeout:     time.Duration(def.Config.Hooks.TimeoutMS) * time.Millisecond,
		},
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}

	// Verbatim: the API host here is the fake's, and the clone host is one no
	// derivation from it could produce.
	if p.remoteURL != cloneURL {
		t.Errorf("remote = %q, want the server's clone_url %q", p.remoteURL, cloneURL)
	}
	if p.authSource == nil {
		t.Fatal("the provider holds no credential source (SPEC §5.5, §10.2)")
	}
	// The credential is obtained *through* the source, at the moment it is
	// needed — and the provider holds no copy of it, which is what lets a
	// rotation reach the next fetch and redaction cover the value in use.
	got, err := p.authSource.Auth(context.Background())
	if err != nil {
		t.Fatalf("Auth: %v", err)
	}
	if got.Password != "t0ken-from-env" || got.Username != "x-access-token" {
		t.Fatalf("Auth = %+v, want the $VAR-resolved tracker credential (SPEC §5.5, §10.2)", got)
	}
	if got := filepath.Join(root, def.Key, "base.git"); p.baseDir != got {
		t.Errorf("baseDir = %q, want %q", p.baseDir, got)
	}
}
