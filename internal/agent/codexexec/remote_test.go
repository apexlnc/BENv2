package codexexec

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The opposite call from claude-code's, and the reason it is opposite: this
// harness enforces its sandbox inside its own process, so the posture is part of
// the command rather than a launcher around it and travels unchanged inside
// Airlock's profile-owned envelope.

func remoteSpec() core.RunSpec {
	return core.RunSpec{
		Workspace: core.WorkspacePaths{Path: "remote:github.com/acme/widgets#7@100"},
		Prompt:    "do the thing",
		Env: map[string]string{
			"BEN_ISSUE": "7", "BEN_ATTEMPT": "1", "BEN_RUN_ID": "7-1",
			"BEN_BRANCH": "ben/7", "BEN_WORKSPACE": "remote:github.com/acme/widgets#7@100",
		},
	}
}

// The pinned native sandbox survives: the mode flag and both `-c` overrides,
// which are not neutral defaults — an omitted one hands the decision to whatever
// `config.toml` the profile happens to carry.
func TestRemoteInvocationKeepsThePinnedNativeSandbox(t *testing.T) {
	parallel(t)
	inv, err := Kind{}.RemoteInvocation(core.AgentConfig{
		Provider: map[string]any{
			"sandbox_mode":   sandboxWorkspaceWrite,
			"network_access": true,
			"add_dirs":       []any{"/srv/cache"},
			"model":          "gpt-5-codex",
			"api_key":        "agent-key",
		},
		Publish: core.PublishCredential{Env: "GH_TOKEN", Var: "PUBLISH_PAT"},
	}, remoteSpec())
	if err != nil {
		t.Fatalf("RemoteInvocation: %v", err)
	}
	for _, want := range [][]string{
		{"exec", "--json"},
		{"--sandbox", sandboxWorkspaceWrite},
		{"-c", "sandbox_workspace_write.network_access=true"},
		{"-c", `sandbox_workspace_write.writable_roots=["/srv/cache"]`},
		{"--model", "gpt-5-codex"},
	} {
		if !hasRun(inv.Argv, want) {
			t.Fatalf("argv %v is missing %v", inv.Argv, want)
		}
	}
	// The prompt still arrives on stdin, which is what the trailing `-` means.
	if inv.Argv[len(inv.Argv)-1] != "-" {
		t.Fatalf("argv %v does not end in the stdin marker", inv.Argv)
	}
	if string(inv.Stdin) != "do the thing" {
		t.Fatalf("stdin is %q", inv.Stdin)
	}
	// And nothing wraps it: no launcher, no policy file, no resolved host path.
	if inv.Argv[0] != DefaultBinary {
		t.Fatalf("argv[0] is %q, want the configured binary name %q", inv.Argv[0], DefaultBinary)
	}
	if strings.Contains(strings.Join(inv.Argv, " "), "/") &&
		!strings.Contains(strings.Join(inv.Argv, " "), "/srv/cache") {
		t.Fatalf("argv %v names a host path", inv.Argv)
	}
}

// CODEX_HOME is the one host fact in this adapter's environment surface, and it
// is dropped: it names a directory on the daemon's disk.
func TestRemoteInvocationEnvironmentDropsTheHarnessHome(t *testing.T) {
	t.Setenv("HOME", "/home/daemon")
	t.Setenv("PUBLISH_PAT", "publish-secret")
	t.Setenv("OPERATOR_VAR", "operator-secret")

	inv, err := Kind{}.RemoteInvocation(core.AgentConfig{
		Provider: map[string]any{
			"sandbox_mode":    sandboxWorkspaceWrite,
			"network_access":  false,
			"api_key":         "agent-key",
			"codex_home":      "/var/lib/ben/codex",
			"env":             map[string]any{"AGENT_FLAG": "on"},
			"env_passthrough": []any{"OPERATOR_VAR"},
		},
		Publish: core.PublishCredential{Env: "GH_TOKEN", Var: "PUBLISH_PAT"},
	}, remoteSpec())
	if err != nil {
		t.Fatalf("RemoteInvocation: %v", err)
	}
	if inv.Env["CODEX_API_KEY"] != "agent-key" || inv.Env["AGENT_FLAG"] != "on" {
		t.Fatalf("the operator's configuration did not travel: %v", inv.Env)
	}
	for _, name := range []string{"CODEX_HOME", "GH_TOKEN", "OPERATOR_VAR", "HOME", "BEN_WORKSPACE"} {
		if got, ok := inv.Env[name]; ok {
			t.Fatalf("env carries %s = %q, which is a fact about the daemon's host", name, got)
		}
	}
}

func TestRemoteInvocationRefusesAReusableGitHubCredential(t *testing.T) {
	parallel(t)
	const secret = "ghp-reusable-MUST-NOT-CROSS"
	for _, tc := range []struct {
		name     string
		provider map[string]any
		sources  []core.ProviderEnvSource
	}{
		{
			name: "credential destination",
			provider: map[string]any{
				"sandbox_mode": sandboxWorkspaceWrite, "api_key": "agent-key",
				"env": map[string]any{"GH_TOKEN": secret},
			},
		},
		{
			name: "renamed environment source",
			provider: map[string]any{
				"sandbox_mode": sandboxWorkspaceWrite, "api_key": "agent-key",
				"env": map[string]any{"AGENT_FLAG": secret},
			},
			sources: []core.ProviderEnvSource{{Variable: "GH_TOKEN", Field: "agent.provider.env.AGENT_FLAG"}},
		},
		{
			name: "argv-bound source",
			provider: map[string]any{
				"sandbox_mode": sandboxWorkspaceWrite, "api_key": "agent-key", "model": secret,
			},
			sources: []core.ProviderEnvSource{{Variable: "GH_TOKEN", Field: "agent.provider.model"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := core.AgentConfig{
				Provider: tc.provider, ProviderEnvSources: tc.sources,
			}
			for _, check := range []struct {
				name string
				run  func() error
			}{
				{"RemoteStructural", func() error { return Kind{}.RemoteStructural(cfg) }},
				{"RemoteInvocation", func() error {
					_, err := Kind{}.RemoteInvocation(cfg, remoteSpec())
					return err
				}},
			} {
				t.Run(check.name, func(t *testing.T) {
					err := check.run()
					if !errors.Is(err, ErrRemoteGitHubCredential) {
						t.Fatalf("%s = %v, want %v", check.name, err, ErrRemoteGitHubCredential)
					}
					if strings.Contains(err.Error(), secret) {
						t.Fatalf("refusal printed the credential value: %v", err)
					}
				})
			}
		})
	}
}

// One parser, both substrates.
func TestRemoteTranslateIsTheAdaptersOwnParser(t *testing.T) {
	parallel(t)
	line := []byte(`{"type":"thread.started","thread_id":"t-1"}`)
	got, want := (Kind{}).RemoteTranslate(line), Translate(line)
	if len(want) == 0 {
		// Two parsers that both produce nothing agree about nothing.
		t.Fatal("the fixture line translates to no events")
	}
	if !slices.Equal(got, want) {
		t.Fatalf("RemoteTranslate produced %v, Translate %v", got, want)
	}
	if caps := (Kind{}).RemoteCapabilities(); caps != capabilities() {
		t.Fatalf("RemoteCapabilities %+v differs from the local ones %+v", caps, capabilities())
	}
}

func hasRun(argv, want []string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(want)], want) {
			return true
		}
	}
	return false
}
