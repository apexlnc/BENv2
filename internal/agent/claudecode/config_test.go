package claudecode

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

func TestParseProviderAcceptsDocumentedKeys(t *testing.T) {
	parallel(t)
	block := map[string]any{
		"binary":           "/opt/claude",
		"model":            "opus",
		"permission_mode":  "bypassPermissions",
		"api_key":          "sk-secret",
		"auth_token":       "tok-secret",
		"settings":         "/etc/ben/settings.json",
		"allowed_tools":    []any{"Bash(git *)", "Edit"},
		"disallowed_tools": []any{"WebFetch"},
		"add_dirs":         []any{"/srv/shared"},
		"env":              map[string]any{"GH_TOKEN": "gh-secret"},
		"config_dir":       "inherit",
		"env_passthrough":  []any{"HTTPS_PROXY"},
		// `none` rather than `srt`, because this block states `config_dir:
		// inherit` and the two are refused together (ErrSandboxConfigDir). The
		// posture's own keys are exercised in sandbox_test.go.
		"sandbox_mode":    "none",
		"sandbox_binary":  "/opt/srt",
		"sandbox_domains": []any{"proxy.internal"},
	}
	got, err := parseProvider(block)
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	want := Provider{
		Binary:          "/opt/claude",
		Model:           "opus",
		PermissionMode:  "bypassPermissions",
		ConfigDir:       "inherit",
		APIKey:          "sk-secret",
		AuthToken:       "tok-secret",
		Settings:        "/etc/ben/settings.json",
		AllowedTools:    []string{"Bash(git *)", "Edit"},
		DisallowedTools: []string{"WebFetch"},
		AddDirs:         []string{"/srv/shared"},
		Env:             map[string]string{"GH_TOKEN": "gh-secret"},
		EnvPassthrough:  []string{"HTTPS_PROXY"},
		SandboxMode:     SandboxNone,
		SandboxBinary:   "/opt/srt",
		SandboxDomains:  []string{"proxy.internal"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseProvider = %+v, want %+v", got, want)
	}
}

func TestParseProviderBinaryDefault(t *testing.T) {
	parallel(t)
	p, err := parseProvider(map[string]any{"permission_mode": "acceptEdits"})
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	if p.Binary != DefaultBinary {
		t.Errorf("Binary = %q, want %q", p.Binary, DefaultBinary)
	}
}

func TestParseProviderRefusals(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name  string
		block map[string]any
		want  error
	}{
		{
			name:  "unknown key",
			block: map[string]any{"permission_mode": "auto", "permision_mode": "auto"},
			want:  ErrProviderKey,
		},
		{
			name:  "unknown key that looks plausible",
			block: map[string]any{"permission_mode": "auto", "max_turns": 4},
			want:  ErrProviderKey,
		},
		{
			name:  "permission_mode missing",
			block: map[string]any{"model": "opus"},
			want:  ErrPermissionMode,
		},
		{
			name:  "permission_mode empty",
			block: map[string]any{"permission_mode": ""},
			want:  ErrPermissionMode,
		},
		{
			// Prompts for every tool use: a headless run would sit there until
			// the stall window closed.
			name:  "permission_mode manual",
			block: map[string]any{"permission_mode": "manual"},
			want:  ErrPermissionMode,
		},
		{
			// Cannot write, so it can never reach the publish step.
			name:  "permission_mode plan",
			block: map[string]any{"permission_mode": "plan"},
			want:  ErrPermissionMode,
		},
		{
			name:  "permission_mode unknown",
			block: map[string]any{"permission_mode": "YOLO"},
			want:  ErrPermissionMode,
		},
		{
			name:  "binary wrong type",
			block: map[string]any{"permission_mode": "auto", "binary": 7},
			want:  ErrProviderValue,
		},
		{
			name:  "allowed_tools not a list",
			block: map[string]any{"permission_mode": "auto", "allowed_tools": "Bash"},
			want:  ErrProviderValue,
		},
		{
			name:  "allowed_tools entry not a string",
			block: map[string]any{"permission_mode": "auto", "allowed_tools": []any{"Bash", 3}},
			want:  ErrProviderValue,
		},
		{
			name:  "allowed_tools entry empty",
			block: map[string]any{"permission_mode": "auto", "allowed_tools": []any{""}},
			want:  ErrProviderValue,
		},
		{
			name:  "env not a map",
			block: map[string]any{"permission_mode": "auto", "env": []any{"GH_TOKEN=x"}},
			want:  ErrProviderValue,
		},
		{
			name:  "env value not a string",
			block: map[string]any{"permission_mode": "auto", "env": map[string]any{"GH_TOKEN": 1}},
			want:  ErrProviderValue,
		},
		{
			// Inline content is a route for a $VAR-resolved secret into argv.
			name:  "settings as inline JSON",
			block: map[string]any{"permission_mode": "auto", "settings": `{"apiKeyHelper":"echo sk-secret"}`},
			want:  ErrProviderValue,
		},
		{
			// Prompt text belongs in the template (SPEC §5.6), never in argv.
			name:  "append_system_prompt is not a provider key",
			block: map[string]any{"permission_mode": "auto", "append_system_prompt": "be nice"},
			want:  ErrProviderKey,
		},
		{
			name:  "env_passthrough not a list",
			block: map[string]any{"permission_mode": "auto", "env_passthrough": "HTTPS_PROXY"},
			want:  ErrProviderValue,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProvider(tc.block)
			if !errors.Is(err, tc.want) {
				t.Errorf("parseProvider(%v) = %v, want %v", tc.block, err, tc.want)
			}
		})
	}
}

// An adapter's own variables have named keys so they can be checked — the
// credential is what Ready probes. A second spelling through the generic env
// surfaces reaches the child with none of that, and does so exactly when the
// named key is omitted, i.e. when nothing was validated (SPEC §7.6).
func TestParseProviderRefusesRespelledAuthSurface(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name  string
		block map[string]any
	}{
		{"api key through env", map[string]any{"permission_mode": "auto", "env": map[string]any{"ANTHROPIC_API_KEY": "sk-sneaked"}}},
		{"auth token through env", map[string]any{"permission_mode": "auto", "env": map[string]any{"ANTHROPIC_AUTH_TOKEN": "tok-sneaked"}}},
		{"api key through env_passthrough", map[string]any{"permission_mode": "auto", "env_passthrough": []any{"ANTHROPIC_API_KEY"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseProvider(tc.block); !errors.Is(err, ErrEnvReserved) {
				t.Errorf("ParseProvider = %v, want %v", err, ErrEnvReserved)
			}
		})
	}
	// An unrelated variable is exactly what `env` is for.
	if _, err := parseProvider(map[string]any{"permission_mode": "auto", "env": map[string]any{"GH_TOKEN": "t"}}); err != nil {
		t.Errorf("ParseProvider = %v, want ok", err)
	}
}

func TestParseProviderAcceptsEveryUsableMode(t *testing.T) {
	parallel(t)
	for _, mode := range usablePermissionModes {
		if _, err := parseProvider(map[string]any{"permission_mode": mode}); err != nil {
			t.Errorf("parseProvider(permission_mode: %q) = %v, want ok", mode, err)
		}
	}
}

// A YAML `key:` with no value decodes to nil; that is an omitted key, not a
// type error (SPEC §5.3's null handling is the loader's, but the adapter must
// not crash on it).
func TestParseProviderNilValuesAreOmissions(t *testing.T) {
	parallel(t)
	p, err := parseProvider(map[string]any{
		"permission_mode":  "auto",
		"model":            nil,
		"allowed_tools":    nil,
		"env":              nil,
		"env_passthrough":  nil,
		"api_key":          nil,
		"add_dirs":         nil,
		"settings":         nil,
		"auth_token":       nil,
		"disallowed_tools": nil,
	})
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	if p.Model != "" || p.AllowedTools != nil || p.Env != nil {
		t.Errorf("nil values should read as omitted, got %+v", p)
	}
}

// Every key the adapter parses must be in the closed set, or a documented key
// would be refused as unknown.
func TestProviderKeysCoverParsedKeys(t *testing.T) {
	parallel(t)
	for _, key := range providerKeys {
		block := map[string]any{"permission_mode": "auto"}
		if key != "permission_mode" {
			block[key] = nil
		}
		if _, err := parseProvider(block); err != nil {
			t.Errorf("ParseProvider with key %q = %v, want ok", key, err)
		}
	}
}

// Every refusal that quotes a provider value carries it as data, anchored at the
// loader's provenance path for that key, so `config effective` can redact it
// (SPEC §5.8). The conformance suite asserts the invariant for the posture key;
// this pins the field paths key by key, including the bracket form for list
// entries.
func TestStructuralRefusalsNeverPrintTheValue(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name  string
		block map[string]any
		field string
		value string
		want  error
	}{
		{
			name:  "permission_mode unknown",
			block: map[string]any{"permission_mode": "secret-looking-mode"},
			field: "agent.provider.permission_mode", value: "secret-looking-mode", want: ErrPermissionMode,
		},
		{
			// A mode the CLI knows and a headless daemon cannot use: the reason
			// is the operator's, the value is still the renderer's to show.
			name:  "permission_mode unusable",
			block: map[string]any{"permission_mode": "plan"},
			field: "agent.provider.permission_mode", value: "plan", want: ErrPermissionMode,
		},
		{
			name:  "env_passthrough entry in the reserved namespace",
			block: map[string]any{"permission_mode": "auto", "env_passthrough": []any{"BEN_SECRET"}},
			field: "agent.provider.env_passthrough[0]", value: "BEN_SECRET", want: ErrEnvNamespace,
		},
		{
			name:  "env_passthrough entry respelling an owned variable",
			block: map[string]any{"permission_mode": "auto", "env_passthrough": []any{"HTTPS_PROXY", "ANTHROPIC_API_KEY"}},
			field: "agent.provider.env_passthrough[1]", value: "ANTHROPIC_API_KEY", want: ErrEnvReserved,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Kind{}.Structural(core.AgentConfig{Provider: tc.block})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Structural = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), tc.value) {
				t.Errorf("refusal text %q carries the offending value", err.Error())
			}
			var verr *core.ConfigValueError
			if !errors.As(err, &verr) || verr.Field != tc.field || verr.Value != tc.value {
				t.Errorf("refusal = %#v, want ConfigValueError{Field: %q, Value: %q}", err, tc.field, tc.value)
			}
		})
	}
}

// Every entry in credentialKeys names a provider key this adapter really parses
// and really injects as the paired variable (SPEC §7.6, §7.7, §5.8).
//
// Independent of the table on purpose. ownedEnv derives from it, so the §7.6
// reservation pins the Env half — but nothing pinned the ProviderKey half, and
// the two halves fail differently. Mistyping a ProviderKey leaves the
// reservation intact and the parser unchanged (it reads its keys by name), while
// SensitiveFields starts declaring a key nobody writes: the credential keeps
// arriving in the child and stops being redacted, which is a literal token
// printed by `config effective`.
//
// So the assertion goes end to end — block → ParseProvider → environ — and an
// entry naming a key ParseProvider does not accept fails as an unknown key.
func TestEveryCredentialKeyIsParsedInjectedAndRedacted(t *testing.T) {
	parallel(t)
	for _, k := range credentialKeys {
		t.Run(k.ProviderKey, func(t *testing.T) {
			canary := "canary-for-" + k.ProviderKey
			p, err := parseProvider(map[string]any{
				"permission_mode": "auto",
				k.ProviderKey:     canary,
			})
			if err != nil {
				t.Fatalf("parseProvider(%s) = %v: the table names a key this adapter does not parse",
					k.ProviderKey, err)
			}
			if got := envValue(first(p.environ(harness.PublishValue{}, core.RunSpec{})), k.Env); got != canary {
				t.Errorf("%s = %q, want the value written at provider.%s: the table pairs them, "+
					"and SensitiveFields redacts on that pairing", k.Env, got, k.ProviderKey)
			}
			// The third leg. The two above prove the entry names a real
			// credential — parsed, and injected as the paired variable — which is
			// what makes this an assertion rather than a tautology: a
			// SensitiveFields that skips an entry now contradicts a key proven to
			// carry a credential, instead of merely disagreeing with a list that
			// describes only itself.
			if !declaresPath(Kind{}.SensitiveFields(map[string]any{"permission_mode": "auto"}), k.ProviderKey) {
				t.Errorf("SensitiveFields omits %q, which the table says is a credential: "+
					"a literal written there prints in the clear in `config effective` (SPEC §5.8)",
					k.ProviderKey)
			}
		})
	}
}

// envValue reads one variable out of a composed child environment.
func envValue(env []string, name string) string {
	for _, entry := range env {
		if after, ok := strings.CutPrefix(entry, name+"="); ok {
			return after
		}
	}
	return ""
}

// declaresPath reports whether a SensitiveFields answer names a top-level key.
func declaresPath(fields [][]string, key string) bool {
	for _, segs := range fields {
		if len(segs) == 1 && segs[0] == key {
			return true
		}
	}
	return false
}
