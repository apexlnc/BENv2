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

// parseProvider is ParseProvider for the block alone, which is what most of these
// tests are about. The publish credential has its own tests (publish_test.go);
// naming it here would put `core.AgentConfig{Provider: ...}` on every call site
// and say nothing.
func parseProvider(block map[string]any) (Provider, error) {
	return ParseProvider(core.AgentConfig{Provider: block})
}

func testProvider(t *testing.T, extra map[string]any) Provider {
	t.Helper()
	block := map[string]any{"sandbox_mode": "workspace-write"}
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
	// The mandatory head, verified against codex-cli 0.147.0. The trailing "-"
	// is what makes the prompt arrive on stdin rather than in argv.
	head := []string{"codex", "exec", "--json", "--sandbox", "workspace-write"}
	// The sandbox is stated on every workspace-write invocation, defaults
	// included, so the harness's config file cannot widen it.
	pins := []string{
		"-c", "sandbox_workspace_write.network_access=false",
		"-c", "sandbox_workspace_write.writable_roots=[]",
		"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
	}
	tail := []string{"-"}

	for _, tc := range []struct {
		name  string
		extra map[string]any
		spec  core.RunSpec
		want  []string
	}{
		{
			name: "fresh session",
			want: slices.Concat(head, pins, tail),
		},
		{
			// `codex exec [OPTIONS] resume <THREAD_ID> [PROMPT]`: the options
			// precede the subcommand, as the CLI's own usage states.
			name: "resume carries the opaque continuation token",
			spec: core.RunSpec{Continuation: "019fe267-3027-73b2-95fc-09a5467477db"},
			want: slices.Concat(head, pins, []string{"resume", "019fe267-3027-73b2-95fc-09a5467477db"}, tail),
		},
		{
			name:  "model",
			extra: map[string]any{"model": "gpt-5.1-codex-max"},
			want:  slices.Concat(head, []string{"--model", "gpt-5.1-codex-max"}, pins, tail),
		},
		{
			// The agent publishes its own branch and PR (SPEC §6.7), and the
			// workspace-write sandbox denies egress by default.
			name:  "network access on",
			extra: map[string]any{"network_access": true},
			want: slices.Concat(head, []string{
				"-c", "sandbox_workspace_write.network_access=true",
				"-c", "sandbox_workspace_write.writable_roots=[]",
				"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
			}, tail),
		},
		{
			// The point of the pin: `false` is *stated*, not left to whatever
			// the harness's config.toml says.
			name:  "network access off is still stated",
			extra: map[string]any{"network_access": false},
			want:  slices.Concat(head, pins, tail),
		},
		{
			name:  "extra writable roots",
			extra: map[string]any{"add_dirs": []any{"/srv/a", "/srv/b"}},
			want: slices.Concat(head, []string{
				"-c", "sandbox_workspace_write.network_access=false",
				"-c", `sandbox_workspace_write.writable_roots=["/srv/a","/srv/b"]`,
				"-c", "sandbox_workspace_write.exclude_slash_tmp=true",
			}, tail),
		},
		{
			// Nothing to pin: danger-full-access sandboxes nothing, so there is
			// no boundary the config file could widen.
			name:  "no sandbox pins under danger-full-access",
			extra: map[string]any{"sandbox_mode": "danger-full-access", "network_access": true},
			want: slices.Concat([]string{"codex", "exec", "--json", "--sandbox", "danger-full-access"},
				tail),
		},
		{
			// MaxTurns is the orchestrator's continuation-chain cap over
			// *sessions* (SPEC §5.2.7); this CLI has no equivalent knob.
			name: "max turns is not mapped",
			spec: core.RunSpec{Limits: core.RunLimits{MaxTurns: 4}},
			want: slices.Concat(head, pins, tail),
		},
		{
			// Unlike claude-code's --max-budget-usd, this harness offers no
			// cost cap; the orchestrator still owns the verdict (SPEC §9.9).
			name: "cost cap has no harness-side flag",
			spec: core.RunSpec{Limits: core.RunLimits{MaxCostUSD: 2.5}},
			want: slices.Concat(head, pins, tail),
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
// layer minted — a state file written by a build without validThreadID is the
// case it exists for — and it is deliberately narrower than that check, because
// what an argv element means is decided by its first character alone
// (harness.CheckContinuationArgv).
//
// This adapter is where it matters most: the `-c sandbox_workspace_write.*`
// pins are argv, so an element the agent chose is an element that can restate
// them (sandboxOverrides).
func TestCommandRefusesAContinuationTokenArgvCannotCarry(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name    string
		token   string
		refused bool
	}{
		{name: "a thread id", token: fixtureThread},
		{
			// Independent anchors: this one is not validThreadID, and a token
			// argv can carry safely is not this layer's to second-guess.
			name:  "an opaque token in some other spelling",
			token: "thread_ABC.123",
		},
		{name: "a bare flag", token: "-c", refused: true},
		{
			// The pin the agent is not allowed to restate: `-c` takes the next
			// element, but so does this, and it needs no second element at all.
			name:    "the sandbox override the pins exist to withhold",
			token:   "--config=sandbox_workspace_write.network_access=true",
			refused: true,
		},
		{name: "a sandbox mode of the agent's choosing", token: "--sandbox=danger-full-access", refused: true},
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
		"api_key":    "sk-SECRET",
		"codex_home": "/var/lib/ben/codex",
		"add_dirs":   []any{"/srv/shared"},
		"model":      "gpt-5.1-codex-max",
		"env":        map[string]any{"GH_TOKEN": "gh-SECRET"},
	})
	spec := core.RunSpec{
		Workspace:    core.WorkspacePaths{Path: "/w"},
		Prompt:       "PROMPT-BODY do the thing",
		Env:          map[string]string{"BEN_RUN_ID": "run-7"},
		Continuation: "thread-1",
	}

	got, err := p.command(spec)
	if err != nil {
		t.Fatalf("command(): %v", err)
	}
	argv := strings.Join(got, "\x00")
	for _, secret := range []string{"sk-SECRET", "gh-SECRET", spec.Prompt, "run-7"} {
		if strings.Contains(argv, secret) {
			t.Errorf("argv leaks %q: %v", secret, got)
		}
	}
}

func TestEnvironComposition(t *testing.T) {
	// A daemon environment with a stray credential in it: the child must not
	// inherit it (SPEC §7.6).
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/ben")
	t.Setenv("CODEX_API_KEY", "daemon-key-MUST-NOT-LEAK")
	t.Setenv("CODEX_HOME", "/home/daemon/.codex")
	t.Setenv("GH_TOKEN", "daemon-gh-MUST-NOT-LEAK")
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "daemon-aws-MUST-NOT-LEAK")

	p := testProvider(t, map[string]any{
		"api_key":         "provider-key",
		"codex_home":      "/var/lib/ben/codex",
		"env":             map[string]any{"GH_TOKEN": "provider-gh", "TMPDIR": "/var/tmp/agent"},
		"env_passthrough": []any{"HTTPS_PROXY"},
	})
	env := envMap(first(p.environ(harness.PublishValue{}, core.RunSpec{Env: map[string]string{"BEN_RUN_ID": "run-7"}})))

	for k, want := range map[string]string{
		"PATH":          "/usr/bin",
		"HOME":          "/home/ben",
		"HTTPS_PROXY":   "http://proxy:3128",  // explicit opt-in
		"CODEX_API_KEY": "provider-key",       // adapter auth surface, not the daemon's
		"CODEX_HOME":    "/var/lib/ben/codex", // likewise: a stored login is a credential
		"GH_TOKEN":      "provider-gh",
		"TMPDIR":        "/var/tmp/agent",
		"BEN_RUN_ID":    "run-7",
	} {
		if env[k] != want {
			t.Errorf("env[%q] = %q, want %q", k, env[k], want)
		}
	}
	if _, ok := env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("child inherited a daemon variable outside the allowlist")
	}
}

// An omitted credential key injects nothing at all: a run then authenticates
// from the stored login under HOME, and readiness asks the matching question
// (see checkAuth).
func TestEnvironOmitsUnsetAuthSurface(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "daemon-key-MUST-NOT-LEAK")
	t.Setenv("CODEX_HOME", "/home/daemon/.codex")

	env := envMap(first(testProvider(t, nil).environ(harness.PublishValue{}, core.RunSpec{})))
	for _, name := range []string{"CODEX_API_KEY", "CODEX_HOME"} {
		if v, ok := env[name]; ok {
			t.Errorf("%s = %q, want absent: the block set neither, and the daemon's is not the agent's", name, v)
		}
	}
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
