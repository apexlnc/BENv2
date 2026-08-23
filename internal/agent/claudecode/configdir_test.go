package claudecode

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/agenttest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// #114 acceptance criterion 4, anchored where the ticket's own wording is not
// enough. "Neither points inside $HOME/.claude" passes trivially for an adapter
// that pins nothing at all — which is the declaration-driven shape AGENTS.md
// names — so this asserts presence first, and reads every value off the
// environment a real child process received rather than off the adapter's
// composition function.
func TestIsolatedConfigDirIsUnderThePrivateDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	env, spec := contract().ChildEnv(t, nil)

	for _, name := range []string{envConfigDir, envTmpDir} {
		got, ok := env[name]
		if !ok {
			t.Errorf("%s is absent from the child environment; the isolated posture is the default", name)
			continue
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s = %q, want an absolute path", name, got)
		}
		// Under the private dir the *provider* reported, which is the property
		// that makes the lifetime the workspace's (SPEC §6.1, §6.4). Being
		// outside ~/.claude follows from it here, and is asserted separately
		// below so a layout that satisfied only one of the two is visible.
		if !strings.HasPrefix(got, spec.Workspace.PrivateDir+string(filepath.Separator)) {
			t.Errorf("%s = %q, want a path under the reported private dir %q", name, got, spec.Workspace.PrivateDir)
		}
		if operator := filepath.Join(home, ".claude"); got == operator ||
			strings.HasPrefix(got, operator+string(filepath.Separator)) {
			t.Errorf("%s = %q, which is inside the operator's %s", name, got, operator)
		}
	}
	if env[envConfigDir] == env[envTmpDir] {
		t.Errorf("%s and %s are the same directory %q; their lifetimes differ (#114 N1, N2)",
			envConfigDir, envTmpDir, env[envConfigDir])
	}
	for _, name := range []string{envConfigDir, envTmpDir} {
		if info, err := os.Stat(env[name]); err != nil || !info.IsDir() {
			t.Errorf("%s = %q, which is not a directory the run could use: %v", name, env[name], err)
		}
	}
}

// The lifetime split, driven through the adapter rather than asserted about the
// path helper: a stable path is not a surviving directory, and §7.1 resume needs
// the second. #114 N1 measured that --resume against a fresh config dir fails
// outright ("No conversation found with session ID"), so this is the property
// that keeps a continuation chain from silently restarting.
func TestConfigDirSurvivesAContinuationAndTheTempDirDoesNot(t *testing.T) {
	parallel(t)
	c := contract()
	first, spec := c.ChildEnv(t, nil)

	config, tmp := first[envConfigDir], first[envTmpDir]
	sessionFile := filepath.Join(config, "session.jsonl")
	scratchFile := filepath.Join(tmp, "scratch")
	for _, path := range []string{sessionFile, scratchFile} {
		if err := os.WriteFile(path, []byte("attempt 1"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// The second attempt of the same chain: same workspace, continuation set —
	// exactly what the orchestrator builds on a retry (SPEC §9.6).
	r := c.FakeRunner(t, c.FakeBlock(t, nil, nil))
	second := spec
	second.Continuation = "sess-1"
	h, err := r.Start(t.Context(), second)
	if err != nil {
		t.Fatalf("Start (continuation): %v", err)
	}
	t.Cleanup(func() { h.Stop(t.Context(), core.StopDiscard) })
	for range h.Events() {
	}

	if got, err := os.ReadFile(sessionFile); err != nil || string(got) != "attempt 1" {
		t.Errorf("the config dir did not survive the continuation (%v); resume reads session state "+
			"from it, so a fresh one restarts the chain instead of resuming it", err)
	}
	if _, err := os.Stat(scratchFile); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("attempt 1's scratch survived into attempt 2 (%v); the temp dir is attempt-owned", err)
	}
	if !dirExists(t, tmp) {
		t.Errorf("the temp dir %s was not re-created for the second attempt", tmp)
	}
}

// TMPDIR is the one adapter-owned name that core.EnvAllowlist also carries, so
// without the pin the daemon's own temp directory is what the child gets. The
// override is the whole point of injecting it, and this is what proves the
// injection lands after the allowlist rather than before it.
func TestDaemonTMPDIRDoesNotReachAnIsolatedChild(t *testing.T) {
	daemonTmp := t.TempDir()
	t.Setenv("TMPDIR", daemonTmp)

	env, spec := contract().ChildEnv(t, nil)

	if env[envTmpDir] == daemonTmp {
		t.Errorf("%s = the daemon's own %q; the allowlist value was not overridden", envTmpDir, daemonTmp)
	}
	if !strings.HasPrefix(env[envTmpDir], spec.Workspace.PrivateDir+string(filepath.Separator)) {
		t.Errorf("%s = %q, want a path under the workspace's private dir", envTmpDir, env[envTmpDir])
	}
}

// `inherit` is today's behaviour, kept reachable and named for a host whose
// login pin makes an isolated config dir unauthenticable (#112 M4). It must
// therefore be the *absence* of the pin, not a different one: a posture that
// still redirected something would not be the escape hatch it exists to be.
func TestInheritLeavesTheHarnessOnTheOperatorsConfigDir(t *testing.T) {
	daemonTmp := t.TempDir()
	t.Setenv("TMPDIR", daemonTmp)

	env, _ := contract().ChildEnv(t, map[string]any{"config_dir": ConfigDirInherit})

	if got, ok := env[envConfigDir]; ok {
		t.Errorf("%s = %q under `inherit`; the harness must resolve its config from $HOME", envConfigDir, got)
	}
	if env[envTmpDir] != daemonTmp {
		t.Errorf("%s = %q, want the daemon's %q — under `inherit` TMPDIR is an ordinary allowlist variable",
			envTmpDir, env[envTmpDir], daemonTmp)
	}
}

// The posture decides which credential and which hooks every run uses, so an
// unrecognized value must not fall back to either member of the set.
func TestConfigDirRefusals(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name, value string
	}{
		{"unknown posture", "sandboxed"},
		{"a path, which this key is not", "/var/lib/ben/claude"},
		{"case variant", "Isolated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProvider(map[string]any{"permission_mode": "auto", "config_dir": tc.value})
			if !errors.Is(err, ErrConfigDir) {
				t.Fatalf("ParseProvider = %v, want ErrConfigDir", err)
			}
			// The value travels as data, never in the message: provider strings
			// are $VAR-resolved and `config effective` prints these (SPEC §5.5).
			if strings.Contains(err.Error(), tc.value) {
				t.Errorf("refusal text %q carries the offending value", err)
			}
			var verr *core.ConfigValueError
			if !errors.As(err, &verr) || verr.Field != "agent.provider.config_dir" || verr.Value != tc.value {
				t.Errorf("refusal = %#v, want ConfigValueError{Field: agent.provider.config_dir, Value: %q}",
					err, tc.value)
			}
		})
	}
	// The default is the isolated one: an operator who never read #114 gets the
	// posture that leaves their own ~/.claude alone, and inheriting is what has
	// to be written down.
	p, err := parseProvider(map[string]any{"permission_mode": "auto"})
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	if p.ConfigDir != ConfigDirIsolated {
		t.Errorf("ConfigDir default = %q, want %q", p.ConfigDir, ConfigDirIsolated)
	}
}

// The §7.6 reservation extended to the two names the posture writes. Both
// directions, and under *both* postures: a load-time reservation conditioned on
// another key's value would answer differently for the same variable depending
// on how far the file had been read.
func TestConfigDirVariablesAreReservedUnderEveryPosture(t *testing.T) {
	parallel(t)
	for _, posture := range append(slicesClone(configDirModes), "") {
		for _, name := range []string{envConfigDir, envTmpDir} {
			block := func(extra map[string]any) map[string]any {
				b := map[string]any{"permission_mode": "auto"}
				if posture != "" {
					b["config_dir"] = posture
				}
				for k, v := range extra {
					b[k] = v
				}
				return b
			}
			t.Run(posture+"/"+name+"/env", func(t *testing.T) {
				_, err := parseProvider(block(map[string]any{"env": map[string]any{name: "/tmp/sneaked"}}))
				if !errors.Is(err, ErrEnvReserved) {
					t.Errorf("ParseProvider = %v, want ErrEnvReserved", err)
				}
			})
			t.Run(posture+"/"+name+"/env_passthrough", func(t *testing.T) {
				_, err := parseProvider(block(map[string]any{"env_passthrough": []any{name}}))
				if !errors.Is(err, ErrEnvReserved) {
					t.Errorf("ParseProvider = %v, want ErrEnvReserved", err)
				}
			})
			t.Run(posture+"/"+name+"/publish.env", func(t *testing.T) {
				_, err := ParseProvider(core.AgentConfig{
					Provider: block(nil),
					Publish:  core.PublishCredential{Env: name, Var: "SOME_TOKEN"},
				})
				if !errors.Is(err, ErrEnvReserved) {
					t.Errorf("ParseProvider = %v, want ErrEnvReserved", err)
				}
			})
		}
	}
}

// An isolated run whose RunSpec reports no private dir is refused, not quietly
// returned to $HOME. The provider places that directory (SPEC §6.1) and §7.1
// forbids this adapter deriving one, so the only alternatives are a refusal and
// a posture that silently stops holding.
func TestStartRefusesIsolatedWithoutAPrivateDir(t *testing.T) {
	parallel(t)
	c := contract()
	r := c.FakeRunner(t, c.FakeBlock(t, nil, nil))

	spec := core.RunSpec{
		Workspace: core.WorkspacePaths{Path: t.TempDir()},
		Prompt:    "do the thing",
	}
	h, err := r.Start(t.Context(), spec)
	if err == nil {
		h.Stop(t.Context(), core.StopDiscard)
		t.Fatal("Start with no private dir succeeded; the run would have used the operator's ~/.claude")
	}
	if !errors.Is(err, ErrPrivateDir) {
		t.Errorf("Start = %v, want ErrPrivateDir", err)
	}

	// `inherit` is the configuration that legitimately has no private dir, and
	// it must still start — otherwise the escape hatch #112 needs is closed by
	// the check that exists to protect the other posture.
	inherit := c.FakeBlock(t, map[string]any{"config_dir": ConfigDirInherit}, nil)
	h2, err := c.FakeRunner(t, inherit).Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start under `inherit` with no private dir = %v, want ok", err)
	}
	t.Cleanup(func() { h2.Stop(t.Context(), core.StopDiscard) })
	for range h2.Events() {
	}
}

// Ready must probe a config dir of the kind an attempt will use. #112's M4
// measured that a BEN-created one is unauthenticated until this adapter injects
// a credential, so probing the operator's would answer for a session no attempt
// ever has — green at startup, failing at every dispatch, which is the exact
// divergence SPEC §7.1's Ready exists to remove.
//
// Driven through Ready rather than through probePrivateDir, because the two
// things that can go wrong are only observable there: a probe composed with no
// private dir still *returns* one from that helper, and a probe that never
// cleans up still returns the right path. Both are read off the environment the
// probe children actually received, and off the filesystem after Ready has
// returned.
func TestReadyProbesAnIsolatedConfigDirAndCleansItUp(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	daemonTmp := t.TempDir()
	t.Setenv("TMPDIR", daemonTmp)

	dumpPath := filepath.Join(t.TempDir(), "probe.json")
	if err := testRunner(t, map[string]string{agenttest.DumpEnv: dumpPath}).Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	envs := probeEnvs(t, dumpPath)
	if len(envs) == 0 {
		t.Fatal("no readiness probe recorded its environment")
	}
	operator := filepath.Join(home, ".claude")
	var probed []string
	for i, env := range envs {
		got := env[envConfigDir]
		if got == "" {
			t.Errorf("probe %d ran with no %s, so it answered for the operator's config dir "+
				"and not for the one an attempt gets (#112 M4)", i, envConfigDir)
			continue
		}
		if !filepath.IsAbs(got) {
			t.Errorf("probe %d: %s = %q, want an absolute path", i, envConfigDir, got)
		}
		if got == operator || strings.HasPrefix(got, operator+string(filepath.Separator)) {
			t.Errorf("probe %d: %s = %q, which is the operator's", i, envConfigDir, got)
		}
		if env[envTmpDir] == daemonTmp {
			t.Errorf("probe %d: %s = the daemon's own %q; the probe must run under the pin too",
				i, envTmpDir, daemonTmp)
		}
		probed = append(probed, got)
	}

	// After Ready has returned, not merely after the helper's cleanup func is
	// called: readiness runs at every startup and every reload, so a probe dir
	// that outlives it accumulates one directory per boot.
	for _, dir := range probed {
		if dirExists(t, dir) {
			t.Errorf("Ready left its probe config dir %s behind", dir)
		}
	}
}

// The `inherit` half: its attempts use the operator's config dir, so its probe
// must too. A posture that still redirected something here would be verifying a
// session its own runs never have — the same divergence, pointed the other way.
func TestReadyUnderInheritProbesTheOperatorsConfigDir(t *testing.T) {
	daemonTmp := t.TempDir()
	t.Setenv("TMPDIR", daemonTmp)

	dumpPath := filepath.Join(t.TempDir(), "probe.json")
	r := testRunnerWith(t, map[string]string{agenttest.DumpEnv: dumpPath},
		map[string]any{"config_dir": ConfigDirInherit})
	if err := r.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	envs := probeEnvs(t, dumpPath)
	if len(envs) == 0 {
		t.Fatal("no readiness probe recorded its environment")
	}
	for i, env := range envs {
		if got, ok := env[envConfigDir]; ok {
			t.Errorf("probe %d: %s = %q under `inherit`; the harness must resolve its config from $HOME",
				i, envConfigDir, got)
		}
		if env[envTmpDir] != daemonTmp {
			t.Errorf("probe %d: %s = %q, want the daemon's %q", i, envTmpDir, env[envTmpDir], daemonTmp)
		}
	}
}

// probeEnvs is probeArgs' sibling: the environment each readiness invocation
// received, in order. The fake dumps before it answers a probe, so both
// invocations are recorded.
func probeEnvs(t *testing.T, path string) []map[string]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("harness never wrote its dump: %v", err)
	}
	var out []map[string]string
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var d struct {
			Env []string `json:"env"`
		}
		if err := dec.Decode(&d); err != nil {
			break
		}
		env := map[string]string{}
		for _, e := range d.Env {
			k, v, _ := strings.Cut(e, "=")
			env[k] = v
		}
		out = append(out, env)
	}
	return out
}

func dirExists(t *testing.T, path string) bool {
	t.Helper()
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func slicesClone(in []string) []string { return append([]string(nil), in...) }
