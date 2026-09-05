package gitcmd

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// This assertion crosses the process boundary rather than inspecting childEnv:
// a Git alias is a child selected and launched by Git itself, which is where an
// inherited daemon credential becomes observable. The canary is the value, not
// the variable name Git was asked to read, so a renamed or synthesized entry
// carrying the same daemon-only bytes still fails the test.
func TestGitChildCannotObserveArbitraryDaemonEnvironment(t *testing.T) {
	const sentinel = "daemon-only-git-child-canary-7f6d1c3a"
	t.Setenv("BEN_GITCMD_TEST_SECRET", sentinel)

	for _, scope := range []struct {
		name string
		env  func() []string
	}{
		{name: "local", env: Env},
		{name: "remote", env: RemoteEnv},
	} {
		t.Run(scope.name, func(t *testing.T) {
			cmd := exec.Command("git", Argv([]string{
				"-c", `alias.ben-env=!env`,
				"ben-env",
			})...)
			cmd.Env = scope.env()
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("BEN-invoked git child: %v", err)
			}
			if !bytes.Contains(out, []byte("GIT_TERMINAL_PROMPT=0")) {
				t.Fatal("BEN-invoked git child did not dump the composed environment")
			}
			if bytes.Contains(out, []byte(sentinel)) {
				t.Fatalf("Git child observed arbitrary daemon environment value %q", sentinel)
			}
		})
	}
}

func TestChildEnvUsesTheReviewedAllowlists(t *testing.T) {
	tests := []struct {
		name        string
		baseline    bool
		remote      bool
		remoteValue string
	}{
		{name: "PATH", baseline: true, remote: true},
		{name: "HOME", baseline: true, remote: true},
		{name: "XDG_CONFIG_HOME", baseline: true, remote: true},
		{name: "GIT_CONFIG_GLOBAL", baseline: true, remote: true, remoteValue: os.DevNull},
		{name: "GIT_CONFIG_NOSYSTEM", baseline: true, remote: true, remoteValue: "1"},
		{name: "GIT_CONFIG_SYSTEM", baseline: true, remote: true, remoteValue: os.DevNull},
		{name: "TMPDIR", baseline: true, remote: true},
		{name: "TZ", baseline: true, remote: true},
		{name: "LANG", baseline: true, remote: true},
		{name: "LANGUAGE", baseline: true, remote: true},
		{name: "LC_ALL", baseline: true, remote: true},
		{name: "LC_COLLATE", baseline: true, remote: true},
		{name: "LC_CTYPE", baseline: true, remote: true},
		{name: "LC_MESSAGES", baseline: true, remote: true},
		{name: "LC_MONETARY", baseline: true, remote: true},
		{name: "LC_NUMERIC", baseline: true, remote: true},
		{name: "LC_TIME", baseline: true, remote: true},

		{name: "http_proxy", remote: true},
		{name: "https_proxy", remote: true},
		{name: "all_proxy", remote: true},
		{name: "no_proxy", remote: true},
		{name: "HTTP_PROXY", remote: true},
		{name: "HTTPS_PROXY", remote: true},
		{name: "ALL_PROXY", remote: true},
		{name: "NO_PROXY", remote: true},
		{name: "GIT_SSL_CAINFO", remote: true},
		{name: "GIT_SSL_CAPATH", remote: true},
		{name: "GIT_PROXY_SSL_CAINFO", remote: true},
		{name: "SSL_CERT_FILE", remote: true},
		{name: "SSL_CERT_DIR", remote: true},
		{name: "SSH_AUTH_SOCK", remote: true},
		{name: "GIT_SSH", remote: true},
		{name: "GIT_SSH_COMMAND", remote: true},
		{name: "GIT_SSH_VARIANT", remote: true},

		{name: "GITHUB_TOKEN"},
		{name: "AWS_SECRET_ACCESS_KEY"},
		{name: "BEN_REMOTE_PROTOCOL"},
		{name: "BEN_REMOTE_HOST"},
		{name: "BEN_REMOTE_USERNAME"},
		{name: "BEN_REMOTE_PASSWORD"},
		{name: "GIT_CONFIG"},
		{name: "GIT_ASKPASS"},
		{name: "SSH_ASKPASS"},
		{name: "GIT_SSL_NO_VERIFY"},
		{name: "GIT_TRACE2_EVENT"},
		{name: "LC_DAEMON_SECRET"},
	}

	wantAllowlist := make(map[string]bool)
	wantRemoteAllowlist := make(map[string]bool)
	for _, tt := range tests {
		if tt.baseline {
			wantAllowlist[tt.name] = true
		}
		if tt.remote && !tt.baseline {
			wantRemoteAllowlist[tt.name] = true
		}
		value := "value-for-" + tt.name
		for _, scope := range []struct {
			name    string
			remote  bool
			allowed bool
		}{
			{name: "local", allowed: tt.baseline},
			{name: "remote", remote: true, allowed: tt.remote},
		} {
			t.Run(tt.name+"/"+scope.name, func(t *testing.T) {
				got, ok := environment(childEnv([]string{tt.name + "=" + value}, scope.remote))[tt.name]
				want := value
				if scope.remote && tt.remoteValue != "" {
					want = tt.remoteValue
				}
				if scope.allowed && (!ok || got != want) {
					t.Fatalf("child environment %s = %q, present %t; want %q", tt.name, got, ok, want)
				}
				if !scope.allowed && ok {
					t.Fatalf("child environment retained unallowlisted %s=%q", tt.name, got)
				}
			})
		}
	}

	// The fixed table is independent of the production map. This count and the
	// reverse lookup make an added production entry fail until its compatibility
	// and security cost is reviewed and named above.
	assertReviewedAllowlist(t, "baseline", environmentAllowlist, wantAllowlist)
	assertReviewedAllowlist(t, "remote", remoteEnvironmentAllowlist, wantRemoteAllowlist)
}

// A remote command must not load either user-global spelling or a selected
// system file. The local control proves Git would read all three canaries from
// the exact input environment; RemoteEnv's result is absence at Git's own
// config parser, not merely a scrubbed-looking string slice.
func TestRemoteEnvIgnoresGlobalAndSystemGitConfig(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[ben]\n\thomeCanary = loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "config"), []byte("[ben]\n\txdgCanary = loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	explicit := filepath.Join(dir, "explicit-global.gitconfig")
	system := filepath.Join(dir, "system.gitconfig")
	if err := os.WriteFile(explicit, []byte("[ben]\n\texplicitCanary = loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(system, []byte("[ben]\n\tsystemCanary = loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	standard := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + home,
		"XDG_CONFIG_HOME=" + xdg,
		"GIT_CONFIG_SYSTEM=" + system,
	}
	for _, layer := range []struct {
		name string
		env  []string
		keys []string
	}{
		{name: "standard user locations", env: standard,
			keys: []string{"ben.homeCanary", "ben.xdgCanary", "ben.systemCanary"}},
		{name: "explicit global selector", env: append(standard, "GIT_CONFIG_GLOBAL="+explicit),
			keys: []string{"ben.explicitCanary", "ben.systemCanary"}},
	} {
		t.Run(layer.name, func(t *testing.T) {
			for _, key := range layer.keys {
				read := func(remote bool) ([]byte, error) {
					cmd := exec.Command("git", "config", "--get", key)
					cmd.Env = childEnv(layer.env, remote)
					return cmd.CombinedOutput()
				}
				out, err := read(false)
				if err != nil || strings.TrimSpace(string(out)) != "loaded" {
					t.Fatalf("local control did not read %s: %v: %s", key, err, out)
				}
				out, err = read(true)
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(out) != 0 {
					t.Fatalf("remote Git read %s from ambient config: %v: %s", key, err, out)
				}
			}
		})
	}
}

// GIT_CONFIG_NOSYSTEM is the refusal paired with Git's system-config layer.
// Prove its effect at Git's own config reader: the control must see a canary in
// the selected system file, while the otherwise identical composed child with
// the suppression flag must report that the key is absent.
func TestChildEnvPreservesSystemConfigSuppression(t *testing.T) {
	systemConfig := filepath.Join(t.TempDir(), "system.gitconfig")
	if err := os.WriteFile(systemConfig, []byte("[ben]\n\tsystemCanary = loaded\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"XDG_CONFIG_HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + systemConfig,
	}
	readCanary := func(extra ...string) ([]byte, error) {
		cmd := exec.Command("git", "config", "--get", "ben.systemCanary")
		cmd.Env = childEnv(append(base, extra...), false)
		return cmd.CombinedOutput()
	}

	out, err := readCanary()
	if err != nil || strings.TrimSpace(string(out)) != "loaded" {
		t.Fatalf("control did not read the selected system config: %v: %s", err, out)
	}
	out, err = readCanary("GIT_CONFIG_NOSYSTEM=1")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		t.Fatalf("git config with GIT_CONFIG_NOSYSTEM returned %v, want absent-key exit 1: %s", err, out)
	}
	if len(out) != 0 {
		t.Fatalf("suppressed system config produced output %q", out)
	}
}

func assertReviewedAllowlist(t *testing.T, name string, got, want map[string]bool) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("%s environment allowlist has %d entries, want the %d reviewed entries", name, len(got), len(want))
	}
	for variable := range got {
		if !want[variable] {
			t.Errorf("%s environment allowlist contains unreviewed %s", name, variable)
		}
	}
}

// Git owns the closed list of variables whose meaning is local to a repository.
// Ask Git for that list rather than driving this test from repositoryLocalEnv:
// if Git learns another redirector, this independent boundary goes red until BEN
// deliberately contains it too (AGENTS.md, Conventions).
func TestChildEnvNeutralizesEveryGitRepositoryLocal(t *testing.T) {
	out, err := exec.Command("git", "rev-parse", "--local-env-vars").Output()
	if err != nil {
		t.Fatalf("git rev-parse --local-env-vars: %v", err)
	}
	local := strings.Fields(string(out))
	if len(local) == 0 {
		t.Fatal("Git reported no repository-local environment variables; the test asserts nothing")
	}

	input := []string{
		"PATH=/usr/bin",
		"BEN_SENTINEL=must-not-cross",
		"GIT_CONFIG_GLOBAL=/etc/ben-gitconfig",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/etc/ben-system-gitconfig",
		"GIT_SSH_COMMAND=ssh -F /etc/ben-ssh-config",
		"GIT_TERMINAL_PROMPT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: secret",
	}
	for _, key := range local {
		input = append(input, key+"=/outside/ben")
	}

	for _, scope := range []struct {
		name   string
		remote bool
	}{
		{name: "local"},
		{name: "remote", remote: true},
	} {
		t.Run(scope.name, func(t *testing.T) {
			got := environment(childEnv(input, scope.remote))
			for _, key := range local {
				if key == "GIT_GRAFT_FILE" || key == "GIT_NO_REPLACE_OBJECTS" {
					continue
				}
				if _, ok := got[key]; ok {
					t.Errorf("child environment retained Git repository-local %s", key)
				}
			}
			for _, key := range []string{"GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0"} {
				if _, ok := got[key]; ok {
					t.Errorf("child environment retained indexed config payload %s", key)
				}
			}
			if _, ok := got["BEN_SENTINEL"]; ok {
				t.Error("child environment retained unallowlisted BEN_SENTINEL")
			}
			if _, ok := got["GIT_SSH_COMMAND"]; ok != scope.remote {
				t.Errorf("GIT_SSH_COMMAND present = %t, want %t", ok, scope.remote)
			}
			want := map[string]string{
				"GIT_CONFIG_GLOBAL":      "/etc/ben-gitconfig",
				"GIT_CONFIG_NOSYSTEM":    "1",
				"GIT_CONFIG_SYSTEM":      "/etc/ben-system-gitconfig",
				"GIT_GRAFT_FILE":         "",
				"GIT_NO_REPLACE_OBJECTS": "1",
				"GIT_TERMINAL_PROMPT":    "0",
			}
			if scope.remote {
				want["GIT_SSH_COMMAND"] = "ssh -F /etc/ben-ssh-config"
				want["GIT_CONFIG_GLOBAL"] = os.DevNull
				want["GIT_CONFIG_SYSTEM"] = os.DevNull
			}
			for key, value := range want {
				gotValue, ok := got[key]
				if !ok || gotValue != value {
					t.Errorf("child environment %s = %q, present %t; want %q", key, gotValue, ok, value)
				}
			}
		})
	}
}

func environment(entries []string) map[string]string {
	out := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}
