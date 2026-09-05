package claudecode

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// parseProvider is ParseProvider for the block alone, which is what most of these
// tests are about. The publish credential has its own tests (publish_test.go);
// naming it here would put `core.AgentConfig{Provider: ...}` on every call site
// and say nothing.
func parseProvider(block map[string]any) (Provider, error) {
	return ParseProvider(core.AgentConfig{Provider: block})
}

func testProvider(t *testing.T, extra map[string]any) Provider {
	t.Helper()
	block := map[string]any{"permission_mode": "bypassPermissions"}
	for k, v := range extra {
		block[k] = v
	}
	p, err := parseProvider(block)
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	return p
}

func TestCommandArgv(t *testing.T) {
	parallel(t)
	// The mandatory head, verified against claude 2.1.221: stream-json with
	// --print requires --verbose.
	head := []string{"claude", "-p", "--output-format", "stream-json", "--verbose",
		"--permission-mode", "bypassPermissions"}

	for _, tc := range []struct {
		name  string
		extra map[string]any
		spec  core.RunSpec
		want  []string
	}{
		{
			name: "fresh session",
			want: head,
		},
		{
			name: "resume carries the opaque continuation token",
			spec: core.RunSpec{Continuation: fixtureSession},
			want: append(slices.Clone(head), "--resume", fixtureSession),
		},
		{
			name:  "model and settings",
			extra: map[string]any{"model": "opus", "settings": "/etc/s.json"},
			want:  append(slices.Clone(head), "--model", "opus", "--settings", "/etc/s.json"),
		},
		{
			name:  "tool policy and extra dirs",
			extra: map[string]any{"allowed_tools": []any{"Bash(git *)", "Edit"}, "disallowed_tools": []any{"WebFetch"}, "add_dirs": []any{"/srv/a"}},
			want: append(slices.Clone(head),
				"--allowed-tools", "Bash(git *)", "--allowed-tools", "Edit",
				"--disallowed-tools", "WebFetch", "--add-dir", "/srv/a"),
		},
		{
			name: "cost cap is passed as a harness-side backstop",
			spec: core.RunSpec{Limits: core.RunLimits{MaxCostUSD: 2.5}},
			want: append(slices.Clone(head), "--max-budget-usd", "2.5"),
		},
		{
			name: "no cost cap flag when disabled",
			spec: core.RunSpec{Limits: core.RunLimits{MaxCostUSD: 0}},
			want: head,
		},
		{
			// MaxTurns is the orchestrator's continuation-chain cap over
			// *sessions* (SPEC §5.2.7); the CLI's --max-turns caps turns within
			// one session. The flag exists — it is simply not this knob.
			name: "max turns is not mapped to the harness flag",
			spec: core.RunSpec{Limits: core.RunLimits{MaxTurns: 4}},
			want: head,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := testProvider(t, tc.extra).command(tc.spec)
			if err != nil {
				t.Fatalf("command(): %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("command() = %v\nwant %v", got, tc.want)
			}
		})
	}
}

// The second of the two anchors on the resume token (#233, SPEC §7.1, §9.6).
// It holds whatever reached the RunSpec, not only what this adapter's stream
// layer minted — a state file written by a build without validSessionID is the
// case it exists for — and it is deliberately narrower than that check, because
// what an argv element means is decided by its first character alone
// (harness.CheckContinuationArgv).
func TestCommandRefusesAContinuationTokenArgvCannotCarry(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name    string
		token   string
		refused bool
	}{
		{name: "a session id", token: fixtureSession},
		{
			// Independent anchors: this one is not validSessionID, and a token
			// argv can carry safely is not this layer's to second-guess.
			name:  "an opaque token the stream layer would have refused",
			token: "sess-123",
		},
		{name: "a bare flag", token: "-p", refused: true},
		{name: "a long flag carrying its own value", token: "--settings=/tmp/evil.json", refused: true},
		{
			// The flag that would undo the posture the operator configured.
			name:    "the permission mode the block already stated",
			token:   "--permission-mode=bypassPermissions",
			refused: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			argv, err := testProvider(t, nil).command(core.RunSpec{Continuation: tc.token})
			if got := errors.Is(err, ErrContinuationToken); got != tc.refused {
				t.Fatalf("command(%q) error = %v, want refusal=%v", tc.token, err, tc.refused)
			}
			if tc.refused {
				if argv != nil {
					t.Errorf("command(%q) returned an argv alongside its refusal: %v", tc.token, argv)
				}
				return
			}
			if !slices.Contains(argv, tc.token) {
				t.Errorf("command(%q) = %v, want the token carried", tc.token, argv)
			}
		})
	}
}

// The acceptance criterion: nothing a ps listing can show may carry secret
// material, and the prompt is not an argument either (SPEC §7.6).
func TestCommandArgvCarriesNoSecretsOrPrompt(t *testing.T) {
	parallel(t)
	p := testProvider(t, map[string]any{
		"api_key":    "sk-ant-SECRET",
		"auth_token": "tok-SECRET",
		"model":      "opus",
		"settings":   "/etc/ben/settings.json",
		"env":        map[string]any{"GH_TOKEN": "gh-SECRET"},
	})
	spec := core.RunSpec{
		Workspace:    core.WorkspacePaths{Path: "/w"},
		Prompt:       "PROMPT-BODY do the thing",
		Env:          map[string]string{"EXTRA_TOKEN": "extra-SECRET"},
		Continuation: fixtureSession,
	}

	argv, err := p.command(spec)
	if err != nil {
		t.Fatalf("command(): %v", err)
	}
	joined := strings.Join(argv, "\x00")
	for _, secret := range secretValues(p, spec) {
		if strings.Contains(joined, secret) {
			t.Errorf("argv leaks %q: %v", secret, argv)
		}
	}
	if len(secretValues(p, spec)) != 5 {
		t.Fatalf("secretValues did not collect every secret: %v", secretValues(p, spec))
	}
}

func TestEnvironComposition(t *testing.T) {
	// A daemon environment with a stray credential in it: the child must not
	// inherit it (SPEC §7.6).
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/ben")
	t.Setenv("LANG", "en_US.UTF-8")
	t.Setenv("ANTHROPIC_API_KEY", "daemon-key-MUST-NOT-LEAK")
	t.Setenv("GH_TOKEN", "daemon-gh-MUST-NOT-LEAK")
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "daemon-aws-MUST-NOT-LEAK")

	p := testProvider(t, map[string]any{
		"api_key":         "provider-key",
		"env":             map[string]any{"GH_TOKEN": "provider-gh"},
		"env_passthrough": []any{"HTTPS_PROXY"},
	})
	env := envMap(first(p.environ(harness.PublishValue{}, core.RunSpec{Env: map[string]string{"BEN_RUN_ID": "run-7"}})))

	for k, want := range map[string]string{
		"PATH":              "/usr/bin",
		"HOME":              "/home/ben",
		"LANG":              "en_US.UTF-8",
		"HTTPS_PROXY":       "http://proxy:3128", // explicit opt-in
		"ANTHROPIC_API_KEY": "provider-key",      // adapter auth surface, not the daemon's
		"GH_TOKEN":          "provider-gh",
		"BEN_RUN_ID":        "run-7",
	} {
		if env[k] != want {
			t.Errorf("env[%q] = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("child inherited a daemon variable outside the allowlist")
	}
}

// The contract is a refusal, never an ordering (SPEC §7.6). What used to be
// here — a test asserting RunSpec.Env wins over the provider block — encoded the
// precedence rule #17 rejected; the refusal it was replaced by lives in
// TestStartRejectsRunSpecEnvOutsideNamespace, and the only keys that reach this
// function from a RunSpec are BEN_-prefixed ones that cannot collide.
func TestEnvironCarriesTheOrchestratorNamespace(t *testing.T) {
	parallel(t)
	p := testProvider(t, map[string]any{
		"api_key": "provider-key",
		"env":     map[string]any{"GH_TOKEN": "provider-gh"},
	})
	env := envMap(first(p.environ(harness.PublishValue{}, core.RunSpec{Env: map[string]string{"BEN_RUN_ID": "run-7"}})))
	if env["BEN_RUN_ID"] != "run-7" {
		t.Errorf("BEN_RUN_ID = %q, want run-7", env["BEN_RUN_ID"])
	}
	if env["ANTHROPIC_API_KEY"] != "provider-key" || env["GH_TOKEN"] != "provider-gh" {
		t.Errorf("adapter-owned keys changed under a RunSpec: %v", env)
	}
}

func TestEnvironAuthTokenSurface(t *testing.T) {
	parallel(t)
	p := testProvider(t, map[string]any{"auth_token": "tok"})
	env := envMap(first(p.environ(harness.PublishValue{}, core.RunSpec{})))
	if env["ANTHROPIC_AUTH_TOKEN"] != "tok" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN = %q, want %q", env["ANTHROPIC_AUTH_TOKEN"], "tok")
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Error("api_key was not set, so ANTHROPIC_API_KEY must be absent")
	}
}

// secretValues is everything a run carries that a ps listing must never show:
// the adapter's auth surface, any injected environment value, and the prompt.
func secretValues(p Provider, spec core.RunSpec) []string {
	var vals []string
	add := func(v string) {
		if v != "" && !slices.Contains(vals, v) {
			vals = append(vals, v)
		}
	}
	add(p.APIKey)
	add(p.AuthToken)
	for _, v := range p.Env {
		add(v)
	}
	for _, v := range spec.Env {
		add(v)
	}
	add(spec.Prompt)
	return vals
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

// first keeps environ's composed environment and drops its second result, the
// resolved env_passthrough pairs, where a test asserts only on the environment.
func first(env []string, _ map[string]string) []string { return env }
