package github

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

func baseConfig() core.TrackerConfig {
	return core.TrackerConfig{
		Provider:       map[string]any{"repo": "acme/widgets", "token": "t0ken"},
		RequiredLabels: []string{"ben-queue"},
		WorkflowKey:    "ben-1a2b3c4d",
	}
}

// Structural is the adapter's half of strict-at-load (SPEC §5.7): the
// provider block is opaque to the loader, so every refusal it earns is named
// here. It takes the core-owned fields alongside the block — the
// required_labels rule spans both — and every row runs with no credentials
// in the environment, because `ben config effective` must work on a laptop
// without secrets and in CI (SPEC §5.8).
func TestStructural(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*core.TrackerConfig)
		wantErr error
	}{
		{"valid", func(*core.TrackerConfig) {}, nil},
		// An omitted token is structurally valid: the documented
		// $GITHUB_TOKEN fallback is a readiness concern (SPEC §5.8).
		{"omitted token", func(c *core.TrackerConfig) { delete(c.Provider, "token") }, nil},
		{"configured claim assignee", func(c *core.TrackerConfig) { c.Provider["claim_assignee"] = "ben-claims" }, nil},
		{"blank claim assignee", func(c *core.TrackerConfig) { c.Provider["claim_assignee"] = "" }, ErrEmptyClaimAssignee},
		{"whitespace claim assignee", func(c *core.TrackerConfig) { c.Provider["claim_assignee"] = "  " }, ErrEmptyClaimAssignee},
		{"null claim assignee", func(c *core.TrackerConfig) { c.Provider["claim_assignee"] = nil }, ErrEmptyClaimAssignee},
		{"non-string claim assignee", func(c *core.TrackerConfig) { c.Provider["claim_assignee"] = 7 }, ErrProviderKeyType},
		{"missing repo", func(c *core.TrackerConfig) { delete(c.Provider, "repo") }, ErrMissingRepo},
		{"blank repo", func(c *core.TrackerConfig) { c.Provider["repo"] = "  " }, ErrMissingRepo},
		{"repo without owner", func(c *core.TrackerConfig) { c.Provider["repo"] = "widgets" }, ErrInvalidRepo},
		{"repo with empty owner", func(c *core.TrackerConfig) { c.Provider["repo"] = "/widgets" }, ErrInvalidRepo},
		{"repo with url", func(c *core.TrackerConfig) { c.Provider["repo"] = "github.com/acme/widgets" }, ErrInvalidRepo},
		{"unknown provider key", func(c *core.TrackerConfig) { c.Provider["organisation"] = "acme" }, ErrUnknownProviderKey},
		{"non-string provider value", func(c *core.TrackerConfig) { c.Provider["repo"] = 7 }, ErrProviderKeyType},
		{"invalid api_url", func(c *core.TrackerConfig) { c.Provider["api_url"] = "not a url" }, ErrInvalidAPIURL},
		{"api_url accepted", func(c *core.TrackerConfig) { c.Provider["api_url"] = "https://ghe.example.com/" }, nil},
		// Each of these reaches the identical endpoint — go-github drops all
		// three resolving a request against the base — while leaving a different
		// BaseURL behind, which is what request control is retained under
		// (Adapter.RequestControlKey). A path is not among them: `/gh/api/v3/`
		// is where a GHE instance genuinely lives.
		{"api_url with userinfo", func(c *core.TrackerConfig) {
			c.Provider["api_url"] = "https://ben:hunter2@ghe.example.com/"
		}, ErrAPIURLNotAnEndpoint},
		{"api_url with query", func(c *core.TrackerConfig) {
			c.Provider["api_url"] = "https://ghe.example.com/?tenant=a"
		}, ErrAPIURLNotAnEndpoint},
		{"api_url with fragment", func(c *core.TrackerConfig) {
			c.Provider["api_url"] = "https://ghe.example.com/#a"
		}, ErrAPIURLNotAnEndpoint},
		{"api_url with a path", func(c *core.TrackerConfig) {
			c.Provider["api_url"] = "https://ghe.example.com/gh/api/v3/"
		}, nil},
		// BUILD.md decision 9: an empty set would make every issue in the
		// repository BEN's.
		{"no required labels", func(c *core.TrackerConfig) { c.RequiredLabels = nil }, ErrEmptyRequiredLabels},
		{"only blank required labels", func(c *core.TrackerConfig) { c.RequiredLabels = []string{"", "  "} }, ErrEmptyRequiredLabels},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_TOKEN", "")
			cfg := baseConfig()
			tt.mutate(&cfg)

			err := Kind{}.Structural(cfg)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Structural() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// Refusals that quote an offending value carry it as data
// (core.ConfigValueError), never in the error text: the adapter cannot know
// whether a value is an env-resolved secret, so printing it is the
// provenance-holding renderer's decision (SPEC §5.8).
func TestStructuralRefusalsNeverPrintTheValue(t *testing.T) {
	for _, tt := range []struct {
		field, key, value string
		want              error
		// sensitive is the stronger promise: not merely absent from the text, but
		// marked so no renderer prints it whatever its provenance. An api_url can
		// hold userinfo, at a path this kind deliberately leaves readable.
		sensitive bool
	}{
		{field: "tracker.provider.repo", key: "repo", value: "secret-with-no-slash", want: ErrInvalidRepo},
		{field: "tracker.provider.api_url", key: "api_url", value: "secret not a url", want: ErrInvalidAPIURL, sensitive: true},
		{field: "tracker.provider.api_url", key: "api_url", value: "ben:hunter2@ghe.example.com", want: ErrInvalidAPIURL, sensitive: true},
		{field: "tracker.provider.api_url", key: "api_url", value: "https://ben:hunter2@ghe.example.com/", want: ErrAPIURLNotAnEndpoint, sensitive: true},
	} {
		cfg := baseConfig()
		cfg.Provider[tt.key] = tt.value

		err := Kind{}.Structural(cfg)
		if !errors.Is(err, tt.want) {
			t.Fatalf("Structural(%s=%q) error = %v, want %v", tt.key, tt.value, err, tt.want)
		}
		if strings.Contains(err.Error(), tt.value) {
			t.Errorf("refusal text %q carries the offending value", err.Error())
		}
		var verr *core.ConfigValueError
		if !errors.As(err, &verr) || verr.Field != tt.field || verr.Value != tt.value {
			t.Errorf("refusal = %#v, want ConfigValueError{Field: %q, Value: %q}", err, tt.field, tt.value)
			continue
		}
		if verr.Sensitive != tt.sensitive {
			t.Errorf("refusal on %s=%q: Sensitive = %v, want %v (config.RenderRefusal prints a file-sourced value that is neither)",
				tt.key, tt.value, verr.Sensitive, tt.sensitive)
		}
	}
}

func TestParseDefaultsAndSplit(t *testing.T) {
	cfg := baseConfig()
	cfg.Provider["claim_assignee"] = "  Ben-Claims  "

	set, err := parse(cfg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if set.owner != "acme" || set.repo != "widgets" {
		t.Errorf("repo split = %q/%q, want acme/widgets", set.owner, set.repo)
	}
	if len(set.activeStates) != 1 || set.activeStates[0] != "open" {
		t.Errorf("activeStates = %v, want the adapter default [open]", set.activeStates)
	}
	if set.claimAssignee != "Ben-Claims" {
		t.Errorf("claimAssignee = %q, want the trimmed configured spelling", set.claimAssignee)
	}
}

// Structural is pure and New performs no network I/O (SPEC §5.7): startup
// has to be able to refuse a bad config with no GitHub at the other end, and
// `ben config effective` must answer with no credentials present.
func TestStructuralAndNewDoNoNetworkIO(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	cfg := baseConfig()
	delete(cfg.Provider, "token")
	cfg.Provider["api_url"] = "https://127.0.0.1:1/" // connection-refused if touched

	if err := (Kind{}).Structural(cfg); err != nil {
		t.Fatalf("Structural: %v", err)
	}
	if _, err := New(compileOptions(cfg)); err != nil {
		t.Fatalf("New: %v", err)
	}
}

// SPEC §10.2's tracker credential, declared so the loader can prove it never
// reaches an agent (#47). The fallback is the part that matters: an omitted
// `token` reads $GITHUB_TOKEN at Ready, and while that went undeclared the
// collision in BEN's own WORKFLOW.md was invisible — the tracker block named no
// variable at all, the agent forwarded GITHUB_TOKEN by name, and nothing in the
// file said they were one secret.
func TestCredentialRefs(t *testing.T) {
	t.Run("an omitted token declares the fallback", func(t *testing.T) {
		refs := Kind{}.CredentialRefs(map[string]any{"repo": "acme/widgets"})
		if !slices.Contains(refs.Vars, FallbackTokenEnv) {
			t.Errorf("Vars = %v, want the $%s fallback", refs.Vars, FallbackTokenEnv)
		}
	})
	t.Run("a stated token does not", func(t *testing.T) {
		// Declaring it anyway would refuse a workflow whose agent legitimately
		// publishes with $GITHUB_TOKEN while the tracker authenticates as
		// somebody else — the split §10.2 is asking for, not a violation of it.
		refs := Kind{}.CredentialRefs(map[string]any{"repo": "acme/widgets", "token": "ghp_stated"})
		if slices.Contains(refs.Vars, FallbackTokenEnv) {
			t.Errorf("Vars = %v; Ready never reads the fallback when the block supplies a token", refs.Vars)
		}
	})
	t.Run("the token field is always referenced", func(t *testing.T) {
		refs := Kind{}.CredentialRefs(map[string]any{"repo": "acme/widgets"})
		found := false
		for _, segs := range refs.Fields {
			if slices.Equal(segs, []string{"token"}) {
				found = true
			}
		}
		if !found {
			t.Errorf("Fields = %v, want the token path: a $VAR there is the stated credential", refs.Fields)
		}
	})
	t.Run("claim assignee is not a credential reference", func(t *testing.T) {
		refs := Kind{}.CredentialRefs(map[string]any{
			"repo": "acme/widgets", "token": "ghp_stated", "claim_assignee": "ben-bot",
		})
		if containsPath(refs.Fields, "claim_assignee") {
			t.Errorf("Fields = %v; a public login was classified as a credential", refs.Fields)
		}
	})
}

// The tracker credential is declared sensitive, and the two questions asked
// about that key are answered from one list (SPEC §5.8, §10.2). Named
// explicitly, because the registry-driven redaction test asserts that whatever
// is *declared* is redacted — it cannot notice a declaration going missing.
func TestSensitiveFieldsDeclaresTheCredential(t *testing.T) {
	fields := Kind{}.SensitiveFields(map[string]any{"repo": "acme/widgets"})
	if !containsPath(fields, "token") {
		t.Errorf("SensitiveFields = %v, want the credential key declared", fields)
	}
	// One list, two consumers: #47's split check reads the same paths. Asserted
	// as *equality* over several blocks rather than as membership, because
	// membership passes for two lists that happen to agree today. This is the
	// strongest behavioural statement available — an identical second literal is
	// still undetectable from outside, and is also not the failure mode: drift is
	// one list changing, and any divergence fails here.
	for _, block := range []map[string]any{
		{"repo": "acme/widgets"},
		{"repo": "acme/widgets", "token": "ghp_stated"},
		{},
	} {
		got := Kind{}.CredentialRefs(block).Fields
		want := Kind{}.SensitiveFields(block)
		if !pathsEqual(got, want) {
			t.Errorf("CredentialRefs.Fields = %v, SensitiveFields = %v: the two answer different "+
				"questions about the same keys and must read one list", got, want)
		}
	}
	// An address and a login are not credentials.
	if containsPath(fields, "repo") || containsPath(fields, "api_url") || containsPath(fields, "claim_assignee") {
		t.Errorf("SensitiveFields = %v; redacting an address or claim login protects nothing and costs the operator", fields)
	}
}

func containsPath(fields [][]string, want ...string) bool {
	for _, segs := range fields {
		if slices.Equal(segs, want) {
			return true
		}
	}
	return false
}

func pathsEqual(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !slices.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
