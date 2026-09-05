package codexexec

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

func TestParseProviderAcceptsDocumentedKeys(t *testing.T) {
	parallel(t)
	block := map[string]any{
		"binary":          "/opt/bin/codex",
		"sandbox_mode":    "danger-full-access",
		"network_access":  true,
		"model":           "gpt-5.1-codex-max",
		"api_key":         "sk-secret",
		"codex_home":      "/var/lib/ben/codex",
		"add_dirs":        []any{"/srv/shared"},
		"env":             map[string]any{"GH_TOKEN": "gh-secret"},
		"env_passthrough": []any{"HTTPS_PROXY"},
	}
	got, err := parseProvider(block)
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	want := Provider{
		Binary:         "/opt/bin/codex",
		SandboxMode:    "danger-full-access",
		NetworkAccess:  true,
		Model:          "gpt-5.1-codex-max",
		APIKey:         "sk-secret",
		CodexHome:      "/var/lib/ben/codex",
		AddDirs:        []string{"/srv/shared"},
		Env:            map[string]string{"GH_TOKEN": "gh-secret"},
		EnvPassthrough: []string{"HTTPS_PROXY"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseProvider = %+v\nwant %+v", got, want)
	}
}

func TestParseProviderBinaryDefault(t *testing.T) {
	parallel(t)
	p, err := parseProvider(map[string]any{"sandbox_mode": "workspace-write"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Binary != DefaultBinary {
		t.Errorf("Binary = %q, want %q", p.Binary, DefaultBinary)
	}
}

func TestLocalWritesCoversEveryProviderGrant(t *testing.T) {
	parallel(t)
	for _, tt := range []struct {
		name     string
		provider map[string]any
		want     core.LocalWriteScope
	}{
		{
			name: "workspace-write names configured grants",
			provider: map[string]any{
				"sandbox_mode": "workspace-write",
				"add_dirs":     []any{"/srv/shared"},
				"env":          map[string]any{"TMPDIR": "/var/tmp/agent", "OTHER": "value"},
			},
			want: core.LocalWriteScope{Roots: []string{"/srv/shared", "/var/tmp/agent"}},
		},
		{
			name:     "danger-full-access is explicitly unbounded",
			provider: map[string]any{"sandbox_mode": "danger-full-access"},
			want:     core.LocalWriteScope{Unbounded: true},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scope, err := (Kind{}).LocalWrites(core.AgentConfig{Provider: tt.provider}, core.LocalRuntimePaths{})
			if err != nil {
				t.Fatalf("LocalWrites: %v", err)
			}
			if scope.Unbounded != tt.want.Unbounded || !slices.Equal(scope.Roots, tt.want.Roots) {
				t.Errorf("LocalWrites = %+v, want %+v", scope, tt.want)
			}
		})
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
			block: map[string]any{"sandbox_mode": "workspace-write", "sandox_mode": "workspace-write"},
			want:  ErrProviderKey,
		},
		{
			// The generic -c passthrough is deliberately absent: it is a route
			// for a $VAR-resolved secret into argv (SPEC §7.6).
			name:  "config passthrough is not a key",
			block: map[string]any{"sandbox_mode": "workspace-write", "config": map[string]any{"model": "o3"}},
			want:  ErrProviderKey,
		},
		{
			name:  "missing sandbox mode",
			block: map[string]any{"model": "gpt-5.1-codex-max"},
			want:  ErrSandboxMode,
		},
		{
			// read-only cannot write, so the agent can never publish
			// (SPEC §5.6, §6.7).
			name:  "read-only cannot publish",
			block: map[string]any{"sandbox_mode": "read-only"},
			want:  ErrSandboxMode,
		},
		{
			name:  "unknown sandbox mode",
			block: map[string]any{"sandbox_mode": "workspace-read"},
			want:  ErrSandboxMode,
		},
		{
			name:  "sandbox mode is not a string",
			block: map[string]any{"sandbox_mode": 7},
			want:  ErrProviderValue,
		},
		{
			name:  "network access is not a bool",
			block: map[string]any{"sandbox_mode": "workspace-write", "network_access": "yes"},
			want:  ErrProviderValue,
		},
		{
			name:  "add_dirs is not a list",
			block: map[string]any{"sandbox_mode": "workspace-write", "add_dirs": "/srv"},
			want:  ErrProviderValue,
		},
		{
			name:  "add_dirs holds an empty entry",
			block: map[string]any{"sandbox_mode": "workspace-write", "add_dirs": []any{""}},
			want:  ErrProviderValue,
		},
		{
			// Readiness runs from the daemon's directory, an attempt from the
			// workspace: a relative home names two different directories, which
			// is the divergence binding at New exists to remove (SPEC §7.1).
			name:  "relative codex home",
			block: map[string]any{"sandbox_mode": "workspace-write", "codex_home": "codex-home"},
			want:  ErrProviderValue,
		},
		{
			name:  "dot-relative codex home",
			block: map[string]any{"sandbox_mode": "workspace-write", "codex_home": "./codex"},
			want:  ErrProviderValue,
		},
		{
			// A relative writable root would resolve against the agent's own
			// workspace at exec time.
			name:  "relative add_dirs entry",
			block: map[string]any{"sandbox_mode": "workspace-write", "add_dirs": []any{"srv/a"}},
			want:  ErrProviderValue,
		},
		{
			// Assembly cannot prove daemon scratch lies outside a cwd-relative
			// temp root selected by the provider.
			name:  "relative TMPDIR override",
			block: map[string]any{"sandbox_mode": "workspace-write", "env": map[string]any{"TMPDIR": "tmp/agent"}},
			want:  ErrProviderValue,
		},
		{
			name:  "empty TMPDIR override",
			block: map[string]any{"sandbox_mode": "workspace-write", "env": map[string]any{"TMPDIR": ""}},
			want:  ErrProviderValue,
		},
		{
			// The roots travel as a TOML array in a -c override; a value needing
			// escaping is refused rather than guessed at.
			name:  "add_dirs entry with a quote",
			block: map[string]any{"sandbox_mode": "workspace-write", "add_dirs": []any{`/srv/a"b`}},
			want:  ErrProviderValue,
		},
		{
			name:  "add_dirs entry with a backslash",
			block: map[string]any{"sandbox_mode": "workspace-write", "add_dirs": []any{`/srv/a\b`}},
			want:  ErrProviderValue,
		},
		{
			name:  "add_dirs entry with a newline",
			block: map[string]any{"sandbox_mode": "workspace-write", "add_dirs": []any{"/srv/a\nb"}},
			want:  ErrProviderValue,
		},
		{
			name:  "env value is not a string",
			block: map[string]any{"sandbox_mode": "workspace-write", "env": map[string]any{"GH_TOKEN": 1}},
			want:  ErrProviderValue,
		},
		{
			// The named key is where CODEX_HOME is checked for absoluteness; a
			// second spelling through the generic map would reach the child with
			// no check at all — and does so precisely when codex_home is
			// omitted, i.e. when nothing was validated.
			name:  "codex home respelled through env",
			block: map[string]any{"sandbox_mode": "workspace-write", "env": map[string]any{"CODEX_HOME": "relative/home"}},
			want:  ErrEnvReserved,
		},
		{
			// Likewise the credential: readiness branches on the *named* key, so
			// this one would authenticate runs that readiness refused.
			name:  "credential respelled through env",
			block: map[string]any{"sandbox_mode": "workspace-write", "env": map[string]any{"CODEX_API_KEY": "sk-sneaked"}},
			want:  ErrEnvReserved,
		},
		{
			name:  "credential forwarded through env_passthrough",
			block: map[string]any{"sandbox_mode": "workspace-write", "env_passthrough": []any{"CODEX_API_KEY"}},
			want:  ErrEnvReserved,
		},
		{
			// SPEC §7.6, config side — the half authored once that hits every run.
			name:  "env defines the reserved prefix",
			block: map[string]any{"sandbox_mode": "workspace-write", "env": map[string]any{"BEN_RUN_ID": "spoofed"}},
			want:  ErrEnvNamespace,
		},
		{
			name:  "env_passthrough names the reserved prefix",
			block: map[string]any{"sandbox_mode": "workspace-write", "env_passthrough": []any{"BEN_RUN_ID"}},
			want:  ErrEnvNamespace,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseProvider(tc.block); !errors.Is(err, tc.want) {
				t.Errorf("ParseProvider = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseProviderAcceptsEveryUsableMode(t *testing.T) {
	parallel(t)
	for _, mode := range usableSandboxModes {
		t.Run(mode, func(t *testing.T) {
			if _, err := parseProvider(map[string]any{"sandbox_mode": mode}); err != nil {
				t.Errorf("parseProvider(%q) = %v, want ok", mode, err)
			}
		})
	}
}

// A key present but null is an omission, not a type error: YAML writes it that
// way when a value is commented out.
func TestParseProviderNilValuesAreOmissions(t *testing.T) {
	parallel(t)
	p, err := parseProvider(map[string]any{
		"sandbox_mode":    "workspace-write",
		"binary":          nil,
		"model":           nil,
		"api_key":         nil,
		"codex_home":      nil,
		"network_access":  nil,
		"add_dirs":        nil,
		"env":             nil,
		"env_passthrough": nil,
	})
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	if p.Binary != DefaultBinary || p.Model != "" || p.APIKey != "" || p.CodexHome != "" ||
		p.NetworkAccess || p.AddDirs != nil || p.Env != nil || p.EnvPassthrough != nil {
		t.Errorf("nil values were not treated as omissions: %+v", p)
	}
}

// The closed key set and the parser must not drift apart: a key the parser
// reads but the set omits would be refused as unknown, and one the set names
// but the parser ignores would be silently accepted and dropped.
func TestProviderKeysCoverParsedKeys(t *testing.T) {
	parallel(t)
	parsed := []string{
		"add_dirs", "api_key", "binary", "codex_home", "env", "env_passthrough",
		"model", "network_access", "sandbox_mode",
	}
	if !slices.IsSorted(providerKeys) {
		t.Error("providerKeys is not sorted; the refusal message lists it verbatim")
	}
	if !slices.Equal(slices.Sorted(slices.Values(providerKeys)), parsed) {
		t.Errorf("providerKeys = %v, want %v", providerKeys, parsed)
	}
}

// Every refusal that quotes a provider value carries it as data, anchored at the
// loader's provenance path for that key, so `config effective` can redact it
// (SPEC §5.8; the shared invariant is asserted for the posture key by the
// conformance suite, this pins the field paths key by key — including the
// bracket form for list entries, which the renderer needs to find the entry's own
// origin rather than the list's).
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
			name:  "sandbox_mode",
			block: map[string]any{"sandbox_mode": "secret-looking-mode"},
			field: "agent.provider.sandbox_mode", value: "secret-looking-mode", want: ErrSandboxMode,
		},
		{
			name:  "codex_home",
			block: map[string]any{"sandbox_mode": "workspace-write", "codex_home": "relative/secret"},
			field: "agent.provider.codex_home", value: "relative/secret", want: ErrProviderValue,
		},
		{
			name:  "add_dirs entry",
			block: map[string]any{"sandbox_mode": "workspace-write", "add_dirs": []any{"/srv/ok", "relative/secret"}},
			field: "agent.provider.add_dirs[1]", value: "relative/secret", want: ErrProviderValue,
		},
		{
			name:  "add_dirs entry with an unquotable character",
			block: map[string]any{"sandbox_mode": "workspace-write", "add_dirs": []any{`/srv/secret"quote`}},
			field: "agent.provider.add_dirs[0]", value: `/srv/secret"quote`, want: ErrProviderValue,
		},
		{
			name:  "relative TMPDIR override",
			block: map[string]any{"sandbox_mode": "workspace-write", "env": map[string]any{"TMPDIR": "relative/secret"}},
			field: "agent.provider.env.TMPDIR", value: "relative/secret", want: ErrProviderValue,
		},
		{
			name:  "env_passthrough entry in the reserved namespace",
			block: map[string]any{"sandbox_mode": "workspace-write", "env_passthrough": []any{"BEN_SECRET"}},
			field: "agent.provider.env_passthrough[0]", value: "BEN_SECRET", want: ErrEnvNamespace,
		},
		{
			name:  "env_passthrough entry respelling an owned variable",
			block: map[string]any{"sandbox_mode": "workspace-write", "env_passthrough": []any{"HTTPS_PROXY", "CODEX_API_KEY"}},
			field: "agent.provider.env_passthrough[1]", value: "CODEX_API_KEY", want: ErrEnvReserved,
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
// and really injects as the paired variable (SPEC §7.6, §7.7, §5.8). See
// claudecode's copy for why the ProviderKey half needs its own assertion: the
// §7.6 reservation pins only the Env half, and a mistyped ProviderKey moves
// display redaction off the key the parser accepts while the credential keeps
// arriving — a literal token printed by `config effective`.
func TestEveryCredentialKeyIsParsedInjectedAndRedacted(t *testing.T) {
	parallel(t)
	for _, k := range credentialKeys {
		t.Run(k.ProviderKey, func(t *testing.T) {
			canary := "canary-for-" + k.ProviderKey
			p, err := parseProvider(map[string]any{
				"sandbox_mode": "workspace-write",
				k.ProviderKey:  canary,
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
			if !declaresPath(Kind{}.SensitiveFields(map[string]any{"sandbox_mode": "workspace-write"}), k.ProviderKey) {
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
