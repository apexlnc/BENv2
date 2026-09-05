package claudecode

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// What a run request submitted to a v2 execution substrate may and may not
// carry. Every case here is about a *host* fact that must not cross: the
// sandbox launcher, its generated policy, the harness directories under the
// workspace's private dir, the resolved binary path, and this daemon's own
// environment.

func remoteSpec() core.RunSpec {
	return core.RunSpec{
		// A remote workspace reports no paths at all; the address the strategy
		// composes is not a directory (remotews.Cycle.Address).
		Workspace: core.WorkspacePaths{Path: "remote:github.com/acme/widgets#7@100"},
		Prompt:    "do the thing",
		Env: map[string]string{
			"BEN_ISSUE": "7", "BEN_ATTEMPT": "1", "BEN_RUN_ID": "7-1",
			"BEN_BRANCH": "ben/7", "BEN_WORKSPACE": "remote:github.com/acme/widgets#7@100",
		},
	}
}

func remoteConfig(t *testing.T, provider map[string]any) core.AgentConfig {
	t.Helper()
	return core.AgentConfig{
		Provider: provider,
		Publish:  core.PublishCredential{Env: "GH_TOKEN", Var: "PUBLISH_PAT"},
	}
}

// The argv is the provider's own command, and nothing wraps it. `srt` is this
// host's launcher over this host's generated policy; Airlock owns the one
// mandatory outer envelope, and a wrapper here would be a second one inside it.
func TestRemoteInvocationCarriesNoLocalSandboxWrapper(t *testing.T) {
	parallel(t)
	inv, err := Kind{}.RemoteInvocation(remoteConfig(t, map[string]any{
		"permission_mode": "bypassPermissions",
		"model":           "claude-sonnet-5",
		"api_key":         "agent-key",
		"sandbox_mode":    SandboxSRT,
	}), remoteSpec())
	if err != nil {
		t.Fatalf("RemoteInvocation: %v", err)
	}
	if inv.Argv[0] != DefaultBinary {
		t.Fatalf("argv[0] is %q, want the configured binary name %q", inv.Argv[0], DefaultBinary)
	}
	joined := strings.Join(inv.Argv, " ")
	for _, forbidden := range []string{DefaultSandboxBinary, "settings.json", "/tmp/", ".ben-sandbox"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("argv %v carries the host-local %q", inv.Argv, forbidden)
		}
	}
	// The command itself is unchanged: it is the command, not a wrapper.
	for _, want := range [][]string{
		{"-p"}, {"--output-format", "stream-json"}, {"--verbose"},
		{"--permission-mode", "bypassPermissions"}, {"--model", "claude-sonnet-5"},
	} {
		if !hasRun(inv.Argv, want) {
			t.Fatalf("argv %v is missing %v", inv.Argv, want)
		}
	}
	// The prompt is on stdin, never in argv (SPEC §7.6).
	if string(inv.Stdin) != "do the thing" {
		t.Fatalf("stdin is %q", inv.Stdin)
	}
	if strings.Contains(joined, "do the thing") {
		t.Fatalf("the prompt reached argv: %v", inv.Argv)
	}
}

// The environment carries the operator's configuration for this agent and the
// orchestrator's coordinates — and none of this host's state.
func TestRemoteInvocationEnvironmentCarriesNoHostState(t *testing.T) {
	t.Setenv("HOME", "/home/daemon")
	t.Setenv("PATH", "/daemon/bin")
	t.Setenv("PUBLISH_PAT", "publish-secret")
	t.Setenv("OPERATOR_VAR", "operator-secret")

	inv, err := Kind{}.RemoteInvocation(remoteConfig(t, map[string]any{
		"permission_mode":  "bypassPermissions",
		"api_key":          "agent-key",
		"env":              map[string]any{"AGENT_FLAG": "on"},
		"env_passthrough":  []any{"OPERATOR_VAR"},
		"config_dir":       ConfigDirIsolated,
		"sandbox_mode":     SandboxSRT,
		"disallowed_tools": []any{"WebFetch"},
	}), remoteSpec())
	if err != nil {
		t.Fatalf("RemoteInvocation: %v", err)
	}

	// Kept: the block's own environment, its documented auth surface, and the
	// BEN_ coordinates.
	for name, want := range map[string]string{
		"AGENT_FLAG":  "on",
		sourceAPIKey:  "agent-key",
		"BEN_ISSUE":   "7",
		"BEN_ATTEMPT": "1",
		"BEN_RUN_ID":  "7-1",
		"BEN_BRANCH":  "ben/7",
	} {
		if got := inv.Env[name]; got != want {
			t.Fatalf("env[%s] = %q, want %q", name, got, want)
		}
	}

	// Dropped, and each for a different reason:
	//   - the publish credential never leaves the daemon;
	//   - `env_passthrough` and the §7.6 allowlist are *this host's* environment;
	//   - the harness directories and the sandbox posture name paths on this disk;
	//   - BEN_WORKSPACE would state a working directory BEN does not know.
	for _, name := range []string{
		"GH_TOKEN", "OPERATOR_VAR", "HOME", "PATH", "TMPDIR",
		envConfigDir, envTmpDir, "BEN_WORKSPACE",
	} {
		if got, ok := inv.Env[name]; ok {
			t.Fatalf("env carries %s = %q, which is a fact about the daemon's host", name, got)
		}
	}
	for _, v := range inv.Env {
		if v == "publish-secret" || v == "operator-secret" {
			t.Fatalf("a host secret reached the request environment: %v", inv.Env)
		}
	}
}

// Local provider.env may still carry GH_TOKEN for the v1 publish path. Remote
// mode has no such exception: #194 forbids serializing reusable GitHub authority
// into a substrate request, so the remote adapter refuses before one exists.
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
				"permission_mode": "bypassPermissions", "api_key": "agent-key",
				"env": map[string]any{"GH_TOKEN": secret},
			},
		},
		{
			name: "renamed environment source",
			provider: map[string]any{
				"permission_mode": "bypassPermissions", "api_key": "agent-key",
				"env": map[string]any{"AGENT_FLAG": secret},
			},
			sources: []core.ProviderEnvSource{{Variable: "GH_TOKEN", Field: "agent.provider.env.AGENT_FLAG"}},
		},
		{
			name: "argv-bound source",
			provider: map[string]any{
				"permission_mode": "bypassPermissions", "api_key": "agent-key", "model": secret,
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

// A malformed block refuses here exactly as it does at construction: this is a
// kind method, so Structural's rules are the ones that apply.
func TestRemoteInvocationRefusesAMalformedBlock(t *testing.T) {
	parallel(t)
	if _, err := (Kind{}).RemoteInvocation(remoteConfig(t, map[string]any{
		"permission_mode": "nonsense",
	}), remoteSpec()); err == nil {
		t.Fatal("a malformed provider block produced an invocation")
	}
	// And a RunSpec no adapter may honor: the BEN_ namespace is exclusive from
	// both sides wherever the run happens.
	spec := remoteSpec()
	spec.Env["NOT_BEN"] = "x"
	if _, err := (Kind{}).RemoteInvocation(remoteConfig(t, map[string]any{}), spec); err == nil {
		t.Fatal("a RunSpec carrying a non-BEN_ variable produced an invocation")
	}
}

// The remote path reads a provider record with the same parser the local one
// does. A second implementation behind the substrate would be a second opinion
// about what a claude-code line means.
func TestRemoteTranslateIsTheAdaptersOwnParser(t *testing.T) {
	parallel(t)
	line := []byte(`{"type":"system","subtype":"init","session_id":"` + fixtureSession + `"}`)
	got := Kind{}.RemoteTranslate(line)
	want := Translate(line)
	if len(want) == 0 {
		// Two parsers that both produce nothing agree about nothing.
		t.Fatal("the fixture line translates to no events")
	}
	if len(got) != len(want) {
		t.Fatalf("RemoteTranslate produced %v, Translate %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("RemoteTranslate produced %v, Translate %v", got, want)
		}
	}
	if caps := (Kind{}).RemoteCapabilities(); caps != capabilities() {
		t.Fatalf("RemoteCapabilities %+v differs from the local ones %+v", caps, capabilities())
	}
}

// hasRun reports whether argv contains want as a contiguous run.
func hasRun(argv, want []string) bool {
	for i := 0; i+len(want) <= len(argv); i++ {
		if slices.Equal(argv[i:i+len(want)], want) {
			return true
		}
	}
	return false
}
