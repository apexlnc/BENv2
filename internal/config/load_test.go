package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
	"github.com/srhg-ai-7cef3f93/ben/internal/template"
)

const validMinimal = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: claude-code
deployment:
  mode: attended
---
Do the work described in {{ issue.title }}.
`

// writeWorkflow drops content into a temp dir and returns the file path.
func writeWorkflow(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidMinimalAppliesDefaults(t *testing.T) {
	def, err := Load(writeWorkflow(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg := def.Config

	if cfg.Version != SupportedVersion {
		t.Errorf("version = %d, want %d", cfg.Version, SupportedVersion)
	}
	if cfg.Polling.IntervalMS != DefaultPollingIntervalMS {
		t.Errorf("polling.interval_ms = %d, want %d", cfg.Polling.IntervalMS, DefaultPollingIntervalMS)
	}
	if cfg.Hooks.TimeoutMS != DefaultHooksTimeoutMS {
		t.Errorf("hooks.timeout_ms = %d, want %d", cfg.Hooks.TimeoutMS, DefaultHooksTimeoutMS)
	}
	if cfg.Limits.MaxConcurrentAgents != DefaultMaxConcurrentAgents ||
		cfg.Limits.MaxTurns != DefaultMaxTurns ||
		cfg.Limits.MaxAttempts != DefaultMaxAttempts ||
		cfg.Limits.MaxRetryBackoffMS != DefaultMaxRetryBackoffMS ||
		cfg.Limits.StallTimeoutMS != DefaultStallTimeoutMS ||
		cfg.Limits.AttemptTimeoutMS != DefaultAttemptTimeoutMS ||
		cfg.Limits.MaxPromptBytes != DefaultMaxPromptBytes {
		t.Errorf("limits defaults wrong: %+v", cfg.Limits)
	}
	// The loader's default and the template layer's own must be the same number,
	// or a caller that forgets the config enforces a different bound than an
	// operator who omitted the key (SPEC §5.6).
	if DefaultMaxPromptBytes != template.DefaultMaxPromptBytes {
		t.Errorf("config default %d != template default %d", DefaultMaxPromptBytes, template.DefaultMaxPromptBytes)
	}
	if cfg.Limits.MaxCostUSD != nil {
		t.Error("max_cost_usd should default to disabled (nil)")
	}
	if cfg.Tracker.ActiveStates != nil || cfg.Tracker.TerminalStates != nil {
		t.Error("active/terminal states should stay nil (adapter default)")
	}
	if !strings.HasPrefix(def.PromptTemplate, "Do the work") {
		t.Errorf("prompt template = %q", def.PromptTemplate)
	}
	if def.Provenance["polling.interval_ms"].Source != SourceDefault {
		t.Error("unset field should have default provenance")
	}
	if def.Provenance["tracker.kind"].Source != SourceFile {
		t.Error("set field should have file provenance")
	}
	if def.Provenance["tracker.active_states"].Source != SourceAdapter {
		t.Error("unset states should have adapter-default provenance")
	}
}

func TestLoadStrictness(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string // transforms validMinimal
		wantErr string
	}{
		{
			name:    "unknown top-level key",
			mutate:  func(s string) string { return strings.Replace(s, "agent:", "poling:\n  interval_ms: 5\nagent:", 1) },
			wantErr: "poling",
		},
		{
			name:    "unknown nested key",
			mutate:  func(s string) string { return strings.Replace(s, "  kind: github", "  kind: github\n  klaim: fast", 1) },
			wantErr: "klaim",
		},
		{
			name: "unknown key deep in limits",
			mutate: func(s string) string {
				return strings.Replace(s, "agent:", "limits:\n  max_turnz: 9\nagent:", 1)
			},
			wantErr: "max_turnz",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeWorkflow(t, tc.mutate(validMinimal)))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error naming %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadOpaqueProviderBlocksPassThrough(t *testing.T) {
	content := strings.Replace(validMinimal,
		"    repo: acme/widgets",
		"    repo: acme/widgets\n    totally_novel_key: 42\n    nested:\n      deeper: yes", 1)
	content = strings.Replace(content,
		"  kind: claude-code",
		"  kind: claude-code\n  provider:\n    permission_mode: acceptEdits\n    unknown_flag: true", 1)

	def, err := Load(writeWorkflow(t, content))
	if err != nil {
		t.Fatalf("provider-block keys must pass through core validation: %v", err)
	}
	if def.Config.Tracker.Provider["totally_novel_key"] != 42 {
		t.Error("tracker.provider contents not preserved")
	}
	if def.Config.Agent.Provider["permission_mode"] != "acceptEdits" {
		t.Error("agent.provider contents not preserved")
	}
}

func TestLoadNullProviderBlocks(t *testing.T) {
	// SPEC §5.2.2/§5.2.5 type `provider` as an object. An explicit null decodes
	// to a nil map — the same as an absent key — so without a dedicated check
	// it would bypass the non-map shape refusal that any other scalar hits.
	t.Run("tracker provider bare colon is null", func(t *testing.T) {
		content := strings.Replace(validMinimal, "  provider:\n    repo: acme/widgets\n", "  provider:\n", 1)
		var verr *ValidationError
		_, err := Load(writeWorkflow(t, content))
		if !errors.As(err, &verr) || verr.Field != "tracker.provider" {
			t.Fatalf("want ValidationError on tracker.provider, got: %v", err)
		}
	})
	t.Run("agent provider explicit null", func(t *testing.T) {
		content := strings.Replace(validMinimal, "  kind: claude-code", "  kind: claude-code\n  provider: null", 1)
		var verr *ValidationError
		_, err := Load(writeWorkflow(t, content))
		if !errors.As(err, &verr) || verr.Field != "agent.provider" {
			t.Fatalf("want ValidationError on agent.provider, got: %v", err)
		}
	})
	t.Run("empty map is the deliberate spelling", func(t *testing.T) {
		content := strings.Replace(validMinimal, "  provider:\n    repo: acme/widgets\n", "  provider: {}\n", 1)
		def, err := Load(writeWorkflow(t, content))
		if err != nil {
			t.Fatalf("provider: {} must load: %v", err)
		}
		if def.Config.Tracker.Provider == nil || len(def.Config.Tracker.Provider) != 0 {
			t.Errorf("tracker.provider = %v, want empty map", def.Config.Tracker.Provider)
		}
	})
	t.Run("unsupported version wins over null provider", func(t *testing.T) {
		content := strings.Replace(validMinimal, "  provider:\n    repo: acme/widgets\n", "  provider:\n", 1)
		content = strings.Replace(content, "tracker:", "version: 2\ntracker:", 1)
		var verr *UnsupportedVersionError
		_, err := Load(writeWorkflow(t, content))
		if !errors.As(err, &verr) || verr.Version != 2 {
			t.Fatalf("want UnsupportedVersionError{2}, got: %v", err)
		}
	})
}

func TestLoadNamedErrors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, err := Load(filepath.Join(t.TempDir(), "WORKFLOW.md"))
		if !errors.Is(err, ErrMissingWorkflowFile) {
			t.Fatalf("want ErrMissingWorkflowFile, got %v", err)
		}
	})
	t.Run("no front matter", func(t *testing.T) {
		_, err := Load(writeWorkflow(t, "just a prompt, no front matter\n"))
		if !errors.Is(err, ErrMissingFrontMatter) {
			t.Fatalf("want ErrMissingFrontMatter, got %v", err)
		}
	})
	t.Run("unterminated front matter", func(t *testing.T) {
		_, err := Load(writeWorkflow(t, "---\ntracker:\n  kind: github\n"))
		if !errors.Is(err, ErrMissingFrontMatter) {
			t.Fatalf("want ErrMissingFrontMatter, got %v", err)
		}
	})
	t.Run("front matter not a map", func(t *testing.T) {
		_, err := Load(writeWorkflow(t, "---\n- a\n- list\n---\nbody\n"))
		if !errors.Is(err, ErrFrontMatterNotMap) {
			t.Fatalf("want ErrFrontMatterNotMap, got %v", err)
		}
	})
	t.Run("empty prompt body", func(t *testing.T) {
		content := strings.Replace(validMinimal, "Do the work described in {{ issue.title }}.", "   \n", 1)
		_, err := Load(writeWorkflow(t, content))
		if !errors.Is(err, ErrEmptyPrompt) {
			t.Fatalf("want ErrEmptyPrompt, got %v", err)
		}
	})
	t.Run("unsupported version wins over unknown keys", func(t *testing.T) {
		content := strings.Replace(validMinimal, "tracker:", "version: 2\nfuture_feature: {}\ntracker:", 1)
		var verr *UnsupportedVersionError
		_, err := Load(writeWorkflow(t, content))
		if !errors.As(err, &verr) || verr.Version != 2 {
			t.Fatalf("want UnsupportedVersionError{2}, got %v", err)
		}
	})
}

func TestLoadSecretResolution(t *testing.T) {
	content := strings.Replace(validMinimal,
		"    repo: acme/widgets",
		"    repo: acme/widgets\n    token: $GH_TEST_TOKEN", 1)

	t.Run("set env resolves with env provenance", func(t *testing.T) {
		t.Setenv("GH_TEST_TOKEN", "sekret-value")
		def, err := Load(writeWorkflow(t, content))
		if err != nil {
			t.Fatal(err)
		}
		if def.Config.Tracker.Provider["token"] != "sekret-value" {
			t.Error("token not resolved from env")
		}
		origin := def.Provenance["tracker.provider.token"]
		if origin.Source != SourceEnv || !slices.Equal(origin.EnvVars, []string{"GH_TEST_TOKEN"}) {
			t.Errorf("provenance = %+v, want env $GH_TEST_TOKEN", origin)
		}
	})
	t.Run("unset env is a missing secret", func(t *testing.T) {
		t.Setenv("GH_TEST_TOKEN", "")
		var merr *MissingSecretError
		_, err := Load(writeWorkflow(t, content))
		if !errors.As(err, &merr) || merr.Var != "GH_TEST_TOKEN" {
			t.Fatalf("want MissingSecretError{GH_TEST_TOKEN}, got %v", err)
		}
	})
	t.Run("hook scripts are never env-resolved", func(t *testing.T) {
		withHook := strings.Replace(validMinimal, "agent:",
			"hooks:\n  before_run: |\n    echo $UNDEFINED_RUNTIME_VAR\nagent:", 1)
		def, err := Load(writeWorkflow(t, withHook))
		if err != nil {
			t.Fatalf("hook $VARs belong to the shell, not the loader: %v", err)
		}
		if !strings.Contains(def.Config.Hooks.BeforeRun, "$UNDEFINED_RUNTIME_VAR") {
			t.Error("hook script was rewritten")
		}
	})
}

func TestLoadWorkspaceRoot(t *testing.T) {
	t.Run("default uses XDG_DATA_HOME", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "/tmp/xdg-test")
		def, err := Load(writeWorkflow(t, validMinimal))
		if err != nil {
			t.Fatal(err)
		}
		if def.Config.Workspace.Root != "/tmp/xdg-test/ben" {
			t.Errorf("root = %q", def.Config.Workspace.Root)
		}
		if def.Provenance["workspace.root"].Source != SourceDefault {
			t.Error("default root should have default provenance")
		}
	})
	t.Run("relative XDG default is normalized against workflow dir", func(t *testing.T) {
		t.Setenv("XDG_DATA_HOME", "relative-xdg")
		path := writeWorkflow(t, validMinimal)
		def, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(filepath.Dir(path), "relative-xdg", "ben")
		if def.Config.Workspace.Root != want {
			t.Errorf("root = %q, want %q", def.Config.Workspace.Root, want)
		}
		if !filepath.IsAbs(def.Config.Workspace.Root) {
			t.Errorf("root = %q, want an absolute path", def.Config.Workspace.Root)
		}
	})
	t.Run("relative resolves against workflow dir", func(t *testing.T) {
		content := strings.Replace(validMinimal, "agent:", "workspace:\n  root: ./ws\nagent:", 1)
		path := writeWorkflow(t, content)
		def, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		want := filepath.Join(filepath.Dir(path), "ws")
		if def.Config.Workspace.Root != want {
			t.Errorf("root = %q, want %q", def.Config.Workspace.Root, want)
		}
	})
	t.Run("tilde expands", func(t *testing.T) {
		content := strings.Replace(validMinimal, "agent:", "workspace:\n  root: ~/ben-ws\nagent:", 1)
		def, err := Load(writeWorkflow(t, content))
		if err != nil {
			t.Fatal(err)
		}
		home, _ := os.UserHomeDir()
		if def.Config.Workspace.Root != filepath.Join(home, "ben-ws") {
			t.Errorf("root = %q", def.Config.Workspace.Root)
		}
	})

	// An empty root must refuse, not resolve: the relative-path rule would
	// otherwise turn it into the workflow's own directory and point the
	// workspace sweep at the repo checkout.
	rejected := []struct {
		name string
		root string // YAML fragment after "root:" (may be empty = null)
	}{
		{"empty root", ` ""`},
		{"whitespace-only root", ` "   "`},
		// Explicit null decodes to a nil *string — absent, to the strict pass —
		// and would silently select the default root instead of refusing.
		{"bare-colon null root", ""},
		{"explicit null root", ` null`},
	}
	for _, tc := range rejected {
		t.Run(tc.name+" is rejected", func(t *testing.T) {
			content := strings.Replace(validMinimal, "agent:", "workspace:\n  root:"+tc.root+"\nagent:", 1)
			var verr *ValidationError
			_, err := Load(writeWorkflow(t, content))
			if !errors.As(err, &verr) || verr.Field != "workspace.root" {
				t.Fatalf("want ValidationError on workspace.root, got: %v", err)
			}
		})
	}
	t.Run("env-resolved whitespace root is rejected", func(t *testing.T) {
		// $VAR resolving to empty is already a MissingSecretError (SPEC §5.5);
		// whitespace sneaks past that and must hit the same root refusal.
		t.Setenv("BEN_TEST_WS_ROOT", "   ")
		content := strings.Replace(validMinimal, "agent:", "workspace:\n  root: $BEN_TEST_WS_ROOT\nagent:", 1)
		var verr *ValidationError
		_, err := Load(writeWorkflow(t, content))
		if !errors.As(err, &verr) || verr.Field != "workspace.root" {
			t.Fatalf("want ValidationError on workspace.root, got: %v", err)
		}
		if !strings.Contains(verr.Msg, "BEN_TEST_WS_ROOT") {
			t.Errorf("error %q should name the variable it resolved from", verr.Msg)
		}
	})
}

func TestLoadValidationRules(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(string) string
		field  string
	}{
		{"missing tracker.kind", func(s string) string { return strings.Replace(s, "  kind: github\n", "", 1) }, "tracker.kind"},
		{"unsupported tracker kind", func(s string) string { return strings.Replace(s, "kind: github", "kind: jira", 1) }, "tracker.kind"},
		{"unsupported agent kind", func(s string) string { return strings.Replace(s, "kind: claude-code", "kind: gemini", 1) }, "agent.kind"},
		{"blank required label", func(s string) string { return strings.Replace(s, `["ben"]`, `["ben", "  "]`, 1) }, "tracker.required_labels"},
		{"non-positive polling interval", func(s string) string { return strings.Replace(s, "agent:", "polling:\n  interval_ms: 0\nagent:", 1) }, "polling.interval_ms"},
		{"negative limit", func(s string) string { return strings.Replace(s, "agent:", "limits:\n  max_turns: -1\nagent:", 1) }, "limits.max_turns"},
		// No configured value may disable the prompt ceiling (SPEC §5.6): zero
		// would mean "apply the default" to template.Limits and negative would
		// remove the bound entirely, so both are refused at load.
		{"zero prompt ceiling", func(s string) string {
			return strings.Replace(s, "agent:", "limits:\n  max_prompt_bytes: 0\nagent:", 1)
		}, "limits.max_prompt_bytes"},
		{"negative prompt ceiling", func(s string) string {
			return strings.Replace(s, "agent:", "limits:\n  max_prompt_bytes: -1\nagent:", 1)
		}, "limits.max_prompt_bytes"},
		{"non-positive cost cap", func(s string) string { return strings.Replace(s, "agent:", "limits:\n  max_cost_usd: 0\nagent:", 1) }, "limits.max_cost_usd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var verr *ValidationError
			_, err := Load(writeWorkflow(t, tc.mutate(validMinimal)))
			if !errors.As(err, &verr) || verr.Field != tc.field {
				t.Fatalf("want ValidationError on %s, got: %v", tc.field, err)
			}
		})
	}
}

// The loader keeps no kind list of its own — it asks internal/registry, the same
// table `ben config effective` selects adapters from (#55). So every registered
// kind loads, and the refusal for an unregistered one quotes that table rather
// than a copy that can fall behind it.
func TestLoadAcceptsExactlyTheRegisteredKinds(t *testing.T) {
	for _, kind := range registry.TrackerNames() {
		content := strings.Replace(validMinimal, "kind: github", "kind: "+kind, 1)
		if _, err := Load(writeWorkflow(t, content)); err != nil {
			t.Errorf("registered tracker kind %q does not load: %v", kind, err)
		}
	}
	for _, kind := range registry.RunnerNames() {
		content := strings.Replace(validMinimal, "kind: claude-code", "kind: "+kind, 1)
		if _, err := Load(writeWorkflow(t, content)); err != nil {
			t.Errorf("registered agent kind %q does not load: %v", kind, err)
		}
	}

	var verr *ValidationError
	_, err := Load(writeWorkflow(t, strings.Replace(validMinimal, "kind: claude-code", "kind: gemini", 1)))
	if !errors.As(err, &verr) {
		t.Fatalf("want ValidationError for an unregistered agent kind, got: %v", err)
	}
	for _, kind := range registry.RunnerNames() {
		if !strings.Contains(verr.Msg, kind) {
			t.Errorf("refusal %q does not offer the registered kind %q", verr.Msg, kind)
		}
	}
}

func TestLoadCRLF(t *testing.T) {
	crlf := strings.ReplaceAll(validMinimal, "\n", "\r\n")
	if _, err := Load(writeWorkflow(t, crlf)); err != nil {
		t.Fatalf("CRLF workflow should load: %v", err)
	}
}

func TestWorkflowKey(t *testing.T) {
	dir := t.TempDir()
	weird := filepath.Join(dir, "my repo!")
	if err := os.Mkdir(weird, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(weird, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(validMinimal), 0o644); err != nil {
		t.Fatal(err)
	}

	def1, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	def2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if def1.Key != def2.Key {
		t.Error("workflow key must be deterministic")
	}
	if !strings.HasPrefix(def1.Key, "my_repo_-") {
		t.Errorf("key = %q, want sanitized dir basename prefix", def1.Key)
	}
	if got := len(strings.Split(def1.Key, "-")); got < 2 {
		t.Errorf("key = %q, want <name>-<8 hex>", def1.Key)
	}
	suffix := def1.Key[strings.LastIndex(def1.Key, "-")+1:]
	if len(suffix) != 8 {
		t.Errorf("hash suffix = %q, want 8 hex chars", suffix)
	}
}

func TestLoadRejectsTemplateStrictnessViolations(t *testing.T) {
	// Template strictness is a load concern (SPEC §5.6, §5.7): a typo'd
	// variable or unknown filter refuses the whole definition, pointing at
	// the real WORKFLOW.md line.
	path := writeWorkflow(t, `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: claude-code
deployment:
  mode: attended
---
Fix the issue titled {{ issue.titel }}.
`)
	_, err := Load(path)
	// A caller outside the template package classifies with the sentinel; the
	// concrete type is for reading the detail out (#74).
	if !errors.Is(err, template.ErrUnknownVariable) {
		t.Errorf("Load = %v, want template.ErrUnknownVariable", err)
	}
	var uv *template.UnknownVariableError
	if !errors.As(err, &uv) {
		t.Fatalf("Load = %v, want template.UnknownVariableError", err)
	}
	if uv.Ref != "issue.titel" {
		t.Errorf("flagged ref %q, want issue.titel", uv.Ref)
	}
	if uv.File != path || uv.Line != 12 {
		t.Errorf("error at %s:%d, want %s:12", uv.File, uv.Line, path)
	}

	path = writeWorkflow(t, `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: claude-code
deployment:
  mode: attended
---
{{ issue.state | shout }}
`)
	_, err = Load(path)
	if !errors.Is(err, template.ErrUnknownFilter) {
		t.Errorf("Load = %v, want template.ErrUnknownFilter", err)
	}
	var uf *template.UnknownFilterError
	if !errors.As(err, &uf) {
		t.Fatalf("Load = %v, want template.UnknownFilterError", err)
	}
	if uf.Name != "shout" {
		t.Errorf("flagged filter %q, want shout", uf.Name)
	}
}

func TestLoadCompilesPrompt(t *testing.T) {
	def, err := Load(writeWorkflow(t, validMinimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if def.prompt == nil {
		t.Fatal("the compiled template should ride along on the definition")
	}
	out, err := def.RenderPrompt(template.Vars{
		Issue:     core.Issue{Identifier: "7", Title: "Sharpen the axe"},
		Attempt:   1,
		Workspace: "/w",
		Run:       template.Run{ID: "r-1"},
	})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	// The title is untrusted (SPEC §5.6), so it arrives fenced. Assert the
	// content and the boundary, not the nonce, which is content-derived.
	for _, want := range []string{"Do the work described in ", "BEN-UNTRUSTED issue.title", "Sharpen the axe"} {
		if !strings.Contains(out, want) {
			t.Errorf("Render = %q, missing %q", out, want)
		}
	}
}

// renderVars is one attempt's bindings, with a body long enough to matter to a
// ceiling.
func renderVars(body string) template.Vars {
	return template.Vars{
		Issue:     core.Issue{Identifier: "7", Title: "Sharpen the axe", Body: body},
		Attempt:   1,
		Workspace: "/w",
		Run:       template.Run{ID: "r-1"},
	}
}

// The configured ceiling is what the daemon renders under. `limits.max_prompt_bytes`
// is worth nothing if the render call assembles its own template.Limits, which is
// how the knob would read as wired while enforcing the 256 KiB default instead
// (SPEC §5.6, #50).
func TestRenderPromptEnforcesTheConfiguredCeiling(t *testing.T) {
	const workflow = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: claude-code
limits:
  max_prompt_bytes: 400
deployment:
  mode: attended
---
Work {{ issue.body }}
`
	def, err := Load(writeWorkflow(t, workflow))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if def.Config.Limits.MaxPromptBytes != 400 {
		t.Fatalf("max_prompt_bytes = %d, want 400", def.Config.Limits.MaxPromptBytes)
	}
	if def.Provenance["limits.max_prompt_bytes"].Source != SourceFile {
		t.Errorf("a configured ceiling should carry file provenance, got %q", def.Provenance["limits.max_prompt_bytes"].Source)
	}

	// Well under the template layer's default, well over this workflow's: a
	// render that reached for template.Limits{} would accept it.
	big := renderVars(strings.Repeat("x", 2000))
	out, err := def.RenderPrompt(big)
	var tooLarge *template.PromptTooLargeError
	if !errors.Is(err, template.ErrPromptTooLarge) || !errors.As(err, &tooLarge) {
		t.Fatalf("RenderPrompt = %v, want %v", err, template.ErrPromptTooLarge)
	}
	if tooLarge.Max != 400 {
		t.Errorf("refusal cites Max = %d, want the configured 400", tooLarge.Max)
	}
	// Refused, not truncated — truncation would cut the closing fence off the
	// untrusted span (SPEC §5.6).
	if out != "" {
		t.Errorf("RenderPrompt returned %d bytes alongside the refusal", len(out))
	}

	// The same definition renders what fits.
	if _, err := def.RenderPrompt(renderVars("short body")); err != nil {
		t.Fatalf("RenderPrompt under the ceiling: %v", err)
	}
}

// And an omitted key leaves the template layer's own ceiling in force, so a
// workflow that says nothing about prompt size is still bounded.
func TestRenderPromptDefaultsToTheTemplateCeiling(t *testing.T) {
	// Emits the body, unlike validMinimal: a ceiling is only observable through
	// a template that renders the untrusted span.
	const workflow = `---
tracker:
  kind: github
  provider:
    repo: acme/widgets
  required_labels: ["ben"]
agent:
  kind: claude-code
deployment:
  mode: attended
---
Work {{ issue.body }}
`
	def, err := Load(writeWorkflow(t, workflow))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if def.Provenance["limits.max_prompt_bytes"].Source != SourceDefault {
		t.Errorf("an omitted ceiling should carry default provenance, got %q", def.Provenance["limits.max_prompt_bytes"].Source)
	}
	if got := def.Config.Limits.MaxPromptBytes; got != template.DefaultMaxPromptBytes {
		t.Errorf("limits.max_prompt_bytes = %d, want the template default %d", got, template.DefaultMaxPromptBytes)
	}
	if _, err := def.RenderPrompt(renderVars(strings.Repeat("x", template.DefaultMaxPromptBytes+1))); !errors.Is(err, template.ErrPromptTooLarge) {
		t.Errorf("RenderPrompt = %v, want %v", err, template.ErrPromptTooLarge)
	}
}
