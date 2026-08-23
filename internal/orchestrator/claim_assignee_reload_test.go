package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
	githubtracker "github.com/srhg-ai-7cef3f93/ben/internal/tracker/github"
)

type claimReloadWorkflow struct {
	apiURL        string
	tokenVariable string
	claimAssignee string
}

func (w claimReloadWorkflow) render() string {
	claim := ""
	if w.claimAssignee != "" {
		claim = "    claim_assignee: " + w.claimAssignee + "\n"
	}
	return fmt.Sprintf(`---
tracker:
  kind: github
  provider:
    repo: acme/widgets
    token: $%s
    api_url: %s
%s  required_labels: ["ben-queue"]
agent:
  kind: claude-code
limits:
  max_concurrent_agents: 1
  max_turns: 4
  max_attempts: 3
deployment:
  mode: attended
---
Work issue {{ issue.identifier }}: {{ issue.title }}.
`, w.tokenVariable, w.apiURL, claim)
}

// claimReloadGitHub is the real adapter's production seam. The two credentials
// authenticate as differently cased spellings of one account; configured
// assignees are all assignable. That is enough to distinguish an identity move
// from a credential rotation, a re-spelling, and either direction of the
// unset/configured migration.
func claimReloadGitHub(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/user", func(w http.ResponseWriter, r *http.Request) {
		login := ""
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case "token-a":
			login = "Ben-Bot"
		case "token-b":
			login = "BEN-BOT"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"login":%q}`, login)
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("owner") != "acme" || r.PathValue("repo") != "widgets" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"widgets","full_name":"acme/widgets","clone_url":"https://git.example.test/acme/widgets.git"}`)
	})
	mux.HandleFunc("GET /api/v3/repos/{owner}/{repo}/assignees/{assignee}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return httptest.NewServer(mux)
}

func writeClaimReloadWorkflow(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func replaceClaimReloadWorkflow(t *testing.T, path, body string) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func trackerConfigForReload(def *config.WorkflowDefinition) core.TrackerConfig {
	return core.TrackerConfig{
		Provider:       def.Config.Tracker.Provider,
		RequiredLabels: def.Config.Tracker.RequiredLabels,
		ActiveStates:   def.Config.Tracker.ActiveStates,
		TerminalStates: def.Config.Tracker.TerminalStates,
		WorkflowKey:    def.Key,
	}
}

// This is #155's reload matrix at the public seams: a real WORKFLOW.md is
// loaded by config.Watch, a real GitHub adapter resolves and publishes the
// principal, and Orchestrator.AdoptIdentity decides whether a live record lets
// the watcher commit. A struct-only test at any one layer would miss the joins
// this contract exists to protect.
func TestClaimAssigneeReloadUsesTheIdentityBarrier(t *testing.T) {
	server := claimReloadGitHub(t)
	defer server.Close()
	t.Setenv("CLAIM_TOKEN_A", "token-a")
	t.Setenv("CLAIM_TOKEN_B", "token-b")

	for _, tt := range []struct {
		name          string
		busy          bool
		from, to      claimReloadWorkflow
		wantPrincipal string
		wantBlocked   bool
	}{
		{
			name: "configured login re-spelling adopts while busy", busy: true,
			from:          claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A", claimAssignee: "Ben-Bot"},
			to:            claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A", claimAssignee: "ben-bot"},
			wantPrincipal: "ben-bot",
		},
		{
			name: "unset to configured for the same account adopts while busy", busy: true,
			from:          claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A"},
			to:            claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A", claimAssignee: "BEN-BOT"},
			wantPrincipal: "ben-bot",
		},
		{
			name: "configured to unset for the same account adopts while busy", busy: true,
			from:          claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A", claimAssignee: "Ben-Bot"},
			to:            claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A"},
			wantPrincipal: "ben-bot",
		},
		{
			name: "credential rotation for the same account adopts while busy", busy: true,
			from:          claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A"},
			to:            claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_B"},
			wantPrincipal: "ben-bot",
		},
		{
			name: "different assignee defers while busy", busy: true,
			from:          claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A", claimAssignee: "Ben-Bot"},
			to:            claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A", claimAssignee: "Other-Bot"},
			wantPrincipal: "ben-bot", wantBlocked: true,
		},
		{
			name:          "different assignee adopts when quiescent",
			from:          claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A", claimAssignee: "Ben-Bot"},
			to:            claimReloadWorkflow{tokenVariable: "CLAIM_TOKEN_A", claimAssignee: "Other-Bot"},
			wantPrincipal: "other-bot",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			from, to := tt.from, tt.to
			from.apiURL, to.apiURL = server.URL, server.URL

			var issues []core.Issue
			if tt.busy {
				issues = []core.Issue{fake.Issue("1", epoch)}
			}
			h := start(t, harnessOpts{issues: issues, script: startedOnly, hang: tt.busy})
			if tt.busy {
				waitFor(t, "the record that makes identity work outstanding", func() bool {
					return h.Runner.StartCount() == 1
				})
			}

			path := writeClaimReloadWorkflow(t, from.render())
			buildRuntime := func(ctx context.Context, def *config.WorkflowDefinition, prev *Bundle, _ config.AdapterChange) (*Bundle, error) {
				cfg := trackerConfigForReload(def)
				if err := (githubtracker.Kind{}).Structural(cfg); err != nil {
					return nil, err
				}
				// Structural sees the block as written; New sees the compiled
				// options, in which the credential is a source and the claim
				// assignee is a field (SPEC §8, amendment 9).
				sources, err := def.Config.NewSources(registry.Source)
				if err != nil {
					return nil, err
				}
				binding := def.Config.TrackerBinding()
				adapter, err := githubtracker.New(core.TrackerOptions{
					Provider:       binding.Provider,
					RequiredLabels: binding.RequiredLabels,
					ActiveStates:   binding.ActiveStates,
					TerminalStates: binding.TerminalStates,
					WorkflowKey:    def.Key,
					ClaimAssignee:  binding.ClaimAssignee,
					Credential:     sources.Tracker,
				})
				if err != nil {
					return nil, err
				}
				if prev != nil {
					if prior, ok := prev.Tracker.(*githubtracker.Adapter); ok {
						adapter.ContinueRequestControl(prior)
					}
				}
				if err := adapter.Ready(ctx); err != nil {
					return nil, err
				}
				principal, err := adapter.ClaimPrincipal(ctx)
				if err != nil {
					return nil, err
				}
				repository, err := adapter.Repository(ctx)
				if err != nil {
					return nil, err
				}
				next := *h.Bundle
				next.Definition = def
				next.Tracker = adapter
				next.ClaimPrincipal = principal
				next.Repository = repository
				return &next, nil
			}

			w, err := config.Watch(context.Background(), path, config.WatchOptions[*Bundle]{
				Debounce:     time.Hour,
				Logger:       discardLogger(),
				BuildRuntime: buildRuntime,
				Barrier:      h.o.AdoptIdentity,
				Quiescent:    h.o.IdentityQuiescent,
			})
			if err != nil {
				t.Fatalf("Watch: %v", err)
			}
			t.Cleanup(func() { _ = w.Close() })
			before := w.Snapshot()
			if before.Runtime.ClaimPrincipal != "ben-bot" {
				t.Errorf("startup ClaimPrincipal = %q, want normalized ben-bot before identity comparison", before.Runtime.ClaimPrincipal)
			}

			replaceClaimReloadWorkflow(t, path, to.render())
			after := w.Revalidate(context.Background())
			if tt.wantBlocked {
				if !errors.Is(after.Blocked, config.ErrWorkOutstanding) {
					t.Fatalf("blocked = %v, want ErrWorkOutstanding", after.Blocked)
				}
				if after.Runtime != before.Runtime || after.Definition != before.Definition {
					t.Error("a refused principal change replaced the runtime or definition")
				}
			} else {
				if after.Blocked != nil {
					t.Fatalf("adoptable reload was blocked: %v", after.Blocked)
				}
				if after.Runtime == before.Runtime || after.Definition == before.Definition {
					t.Error("the accepted reload did not publish its rebuilt tracker and definition")
				}
			}
			if got := after.Runtime.ClaimPrincipal; got != tt.wantPrincipal {
				t.Errorf("ClaimPrincipal = %q, want %q", got, tt.wantPrincipal)
			}
		})
	}
}
