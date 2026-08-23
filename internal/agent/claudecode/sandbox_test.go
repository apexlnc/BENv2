package claudecode

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/agenttest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The golden posture's inputs. Fixed strings rather than t.TempDir paths: the
// settings file is a wire format another program reads, so the test that
// asserts it whole has to be able to assert it whole — including that the
// workspace root sits under the denied $HOME, which is SPEC §5.2.4's default
// and the shape that made "allowWrite does not imply read" load-bearing.
const (
	goldenHome      = "/home/ben"
	goldenWorkspace = "/home/ben/.local/share/ben/wf/issues/issue-7"
	goldenSharedGit = "/home/ben/.local/share/ben/wf/base.git"
	goldenPrivate   = "/home/ben/.local/share/ben/wf/private/issue-7"
	goldenBinary    = "/home/ben/.local/bin/claude"
	goldenCanonical = "/home/ben/.local/share/claude/versions/2.1.221"
)

func goldenPaths() sandboxPaths {
	return sandboxPaths{
		Workspace:    goldenWorkspace,
		SharedGitDir: goldenSharedGit,
		PrivateDir:   goldenPrivate,
		Binary:       goldenBinary,
		Canonical:    goldenCanonical,
		Control:      sandboxControlFor(goldenPrivate),
	}
}

// testPublish is the publish credential every `srt` block needs: the posture
// denies $HOME and gives the forge CLI a config dir of BEN's, so the credential
// an omitted `publish` block relies on (SPEC §5.2.8) is unreachable under it.
var testPublish = core.PublishCredential{Env: "GH_TOKEN", Var: "BEN_TEST_PUBLISH"}

// testPublishBinding is what that reference compiles into: the same child
// variable, and the implicit `static` source over the same daemon variable
// (SPEC §8, amendment 9).
func testPublishBinding() core.PublishBinding {
	return core.PublishBinding{Env: testPublish.Env, Source: agenttest.EnvSource(testPublish.Var)}
}

// parseSandboxProvider parses a block that states the `srt` posture.
func parseSandboxProvider(block map[string]any) (Provider, error) {
	return ParseProvider(core.AgentConfig{Provider: block, Publish: testPublish})
}

func goldenProvider(t *testing.T) Provider {
	t.Helper()
	p, err := parseSandboxProvider(map[string]any{
		"permission_mode": "bypassPermissions",
		"api_key":         "sk-secret",
		"sandbox_mode":    SandboxSRT,
		"add_dirs":        []any{"/srv/shared"},
		"sandbox_domains": []any{"proxy.internal"},
	})
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	return p
}

// The composed settings file, asserted whole. A golden rather than a set of
// field checks because the file is what srt reads: a key this adapter stops
// emitting is a setting srt fills from its own recommendations, and only a
// whole-file comparison sees a key disappear.
//
// Both platforms from one run, since the composition takes goos as a parameter
// — otherwise the darwin pin, which is the difference between a working publish
// step and a broken one, would only ever be checked on darwin.
func TestSandboxSettingsGolden(t *testing.T) {
	parallel(t)
	p := goldenProvider(t)
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			got, err := json.MarshalIndent(p.sandboxSettings(goldenPaths(), goos, goldenHome), "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", "sandbox-settings-"+goos+".json")
			if os.Getenv("UPDATE_GOLDEN") != "" {
				if err := os.WriteFile(golden, append(got, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
				t.Errorf("composed settings differ from %s:\n--- want ---\n%s\n--- got ---\n%s",
					golden, want, got)
			}
		})
	}
}

// The pinning half of "what it does not name, it pins", anchored at properties
// rather than at the golden. A golden proves the file has not changed; it
// cannot say which parts of it are load-bearing, and a future edit that
// regenerated it would carry a deleted denial along with everything else.
func TestSandboxDeniesTheAgentsOwnControlSurface(t *testing.T) {
	parallel(t)
	fs := goldenProvider(t).sandboxSettings(goldenPaths(), "linux", goldenHome).Filesystem
	control := sandboxControlFor(goldenPrivate)

	for _, tc := range []struct{ path, why string }{
		{filepath.Join(goldenSharedGit, "hooks"),
			"hooks are code the next run executes, in a directory every workspace of this workflow shares"},
		{filepath.Join(goldenSharedGit, "config"),
			"the shared repository's own configuration"},
		{filepath.Join(goldenWorkspace, ".git"),
			"the gitdir pointer srt leaves writable by design; §6.2 reattaches, so a rewrite survives"},
		{control.Dir,
			"BEN's settings file and git config: an agent that rewrites them chooses its own sandbox"},
	} {
		if !slices.Contains(fs.DenyWrite, tc.path) {
			t.Errorf("denyWrite is missing %s — %s", tc.path, tc.why)
		}
	}

	// The shared git dir needs *both* lists. Measured: with it in allowWrite
	// only, under denyRead: [$HOME], git reports "not a git repository:
	// …/worktrees/<key>" — allowWrite does not imply read, and SPEC §5.2.4's
	// default root being under $HOME is what makes that reachable.
	for _, list := range []struct {
		name string
		in   []string
	}{{"allowRead", fs.AllowRead}, {"allowWrite", fs.AllowWrite}} {
		if !slices.Contains(list.in, goldenSharedGit) {
			t.Errorf("%s is missing the shared git dir %s; a linked worktree's .git points into "+
				"it, so `git commit` fails without it in both lists", list.name, goldenSharedGit)
		}
	}
	// Read policy bounded at all: with denyRead empty the posture is
	// defense-in-depth only and §10.1's protected-mode outcome is not reached.
	if !slices.Contains(fs.DenyRead, goldenHome) {
		t.Errorf("denyRead = %v, want the daemon's $HOME; an unbounded read policy leaves "+
			"~/.ssh and ~/.gitconfig readable", fs.DenyRead)
	}
	// add_dirs grants the harness tool access outside the workspace, so a
	// posture that denied it would be two configurations disagreeing.
	for _, list := range [][]string{fs.AllowRead, fs.AllowWrite} {
		if !slices.Contains(list, "/srv/shared") {
			t.Errorf("add_dirs entry /srv/shared is missing from %v", list)
		}
	}
}

// The shared git dir is bound from what the provider reported, never derived
// from the workspace path. SPEC §7.1 forbids the derivation because
// `git rev-parse --git-common-dir` reads it out of `<workspace>/.git`, which
// srt leaves writable by design — so a reattached workspace could name a
// repository the agent chose.
//
// Driven with a shared git dir that shares no prefix with the workspace, so any
// layout arithmetic produces a different answer than the reported one.
func TestSandboxBindsTheReportedSharedGitDirNotOneDerivedFromTheWorkspace(t *testing.T) {
	parallel(t)
	const elsewhere = "/var/lib/ben/somewhere-else.git"
	paths := goldenPaths()
	paths.SharedGitDir = elsewhere

	fs := goldenProvider(t).sandboxSettings(paths, "linux", goldenHome).Filesystem
	if !slices.Contains(fs.AllowWrite, elsewhere) {
		t.Errorf("allowWrite = %v, want the reported shared git dir %s", fs.AllowWrite, elsewhere)
	}
	for _, entry := range slices.Concat(fs.AllowRead, fs.AllowWrite) {
		if strings.HasPrefix(entry, filepath.Dir(filepath.Dir(goldenWorkspace))) &&
			strings.HasSuffix(entry, "base.git") {
			t.Errorf("allow lists contain %s, which is the workspace layout's base.git rather "+
				"than the reported one — the path was derived, not taken from the RunSpec", entry)
		}
	}
	if !slices.Contains(fs.DenyWrite, filepath.Join(elsewhere, "hooks")) {
		t.Errorf("denyWrite = %v, want hooks under the *reported* shared git dir", fs.DenyWrite)
	}
}

// The egress floor is what the §5.6 workflow cannot function without, so the
// provider key adds to it and cannot shrink it. An empty operator list means
// the floor, never "no network": `allowedDomains: []` is a harness that cannot
// reach the API at all, which reads as a working configuration and is not one.
func TestEgressFloorIsAdditiveNotReplaceable(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name    string
		domains []any
		want    []string
	}{
		{name: "unstated", want: egressFloor},
		{name: "added to", domains: []any{"proxy.internal"},
			want: append(slices.Clone(egressFloor), "proxy.internal")},
		{
			// The shrinking attempt: naming one member of the floor must not
			// make it the whole list.
			name: "one floor member restated", domains: []any{"api.anthropic.com"}, want: egressFloor,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := map[string]any{
				"permission_mode": "bypassPermissions",
				"api_key":         "sk-secret",
				"sandbox_mode":    SandboxSRT,
			}
			if tc.domains != nil {
				block["sandbox_domains"] = tc.domains
			}
			p, err := parseSandboxProvider(block)
			if err != nil {
				t.Fatalf("ParseProvider: %v", err)
			}
			got := p.sandboxSettings(goldenPaths(), "linux", goldenHome).Network.AllowedDomains
			if want := dedupe(tc.want); !slices.Equal(got, want) {
				t.Errorf("allowedDomains = %v, want %v", got, want)
			}
			for _, floor := range egressFloor {
				if !slices.Contains(got, floor) {
					t.Errorf("allowedDomains = %v, want it to still carry the floor entry %s", got, floor)
				}
			}
		})
	}
}

// The posture decides whether a run is bounded at all, so an unrecognized value
// must not fall back to either member of the set.
func TestSandboxModeRefusals(t *testing.T) {
	parallel(t)
	for _, tc := range []struct{ name, value string }{
		{"unknown posture", "seatbelt"},
		{"the runtime's package name", "sandbox-runtime"},
		{"case variant", "SRT"},
		{"a boolean, which this key is not", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProvider(map[string]any{
				"permission_mode": "auto", "sandbox_mode": tc.value,
			})
			if !errors.Is(err, ErrSandboxMode) {
				t.Fatalf("ParseProvider = %v, want ErrSandboxMode", err)
			}
			// The value travels as data, never in the message: provider strings
			// are $VAR-resolved and `config effective` prints these (SPEC §5.5).
			if strings.Contains(err.Error(), tc.value) {
				t.Errorf("refusal text %q carries the offending value", err)
			}
			var verr *core.ConfigValueError
			if !errors.As(err, &verr) || verr.Field != "agent.provider.sandbox_mode" || verr.Value != tc.value {
				t.Errorf("refusal = %#v, want ConfigValueError{Field: agent.provider.sandbox_mode, Value: %q}",
					err, tc.value)
			}
		})
	}

	// #51 made the OS account the boundary and stronger sandboxing RECOMMENDED,
	// so the default is the behaviour that predates the key. A default of `srt`
	// would make every daemon that starts today refuse readiness after an
	// upgrade, on a host that has never installed the runtime.
	p, err := parseProvider(map[string]any{"permission_mode": "auto"})
	if err != nil {
		t.Fatalf("ParseProvider: %v", err)
	}
	if p.SandboxMode != SandboxNone {
		t.Errorf("SandboxMode default = %q, want %q", p.SandboxMode, SandboxNone)
	}
	if p.SandboxBinary != DefaultSandboxBinary {
		t.Errorf("SandboxBinary default = %q, want %q", p.SandboxBinary, DefaultSandboxBinary)
	}
}

// `srt` and `config_dir: inherit` are individually valid and jointly
// incoherent. #81's own real-agent run measured the result: under `inherit` the
// harness still resolves its configuration from $HOME, the operator's
// PreToolUse hook applies, the sandbox will not let it start, and every Bash
// call is refused — an agent that reports tool failures and stops without
// committing. The writable set such a hook needs belongs to the operator's
// tooling, so this adapter cannot state it, which is what makes the refusal the
// only honest answer.
func TestSandboxRefusesTheInheritedConfigDir(t *testing.T) {
	parallel(t)
	_, err := parseProvider(map[string]any{
		"permission_mode": "auto",
		"api_key":         "sk-secret",
		"sandbox_mode":    SandboxSRT,
		"config_dir":      ConfigDirInherit,
	})
	if !errors.Is(err, ErrSandboxConfigDir) {
		t.Fatalf("ParseProvider = %v, want ErrSandboxConfigDir", err)
	}
	var verr *core.ConfigValueError
	if !errors.As(err, &verr) || verr.Field != "agent.provider.config_dir" {
		t.Errorf("refusal = %#v, want it anchored at agent.provider.config_dir — that is the "+
			"key the operator changes", err)
	}
	// The isolated default is the working pair, and it must not need stating.
	if _, err := parseSandboxProvider(map[string]any{
		"permission_mode": "auto", "api_key": "sk-secret", "sandbox_mode": SandboxSRT,
	}); err != nil {
		t.Errorf("ParseProvider with the default config_dir = %v, want ok", err)
	}
}

// The posture denies $HOME, and an OAuth session reads its credential from
// ~/Library/Keychains inside it — measured on claude 2.1.221: with $HOME denied
// and no keychain carve-out the run reports "Not logged in", and adding the
// keychain restores it. Carving it out would hand the agent every keychain item
// on the host to buy back one credential, so the posture requires the
// credential surface that needs no carve-out and says so at load.
func TestSandboxRefusesWithoutAnEnvironmentCredential(t *testing.T) {
	parallel(t)
	_, err := parseProvider(map[string]any{
		"permission_mode": "auto", "sandbox_mode": SandboxSRT,
	})
	if !errors.Is(err, ErrSandboxCredential) {
		t.Fatalf("ParseProvider = %v, want ErrSandboxCredential", err)
	}
	if !strings.Contains(err.Error(), "Keychains") {
		t.Errorf("refusal = %v, want it to name the reason; an operator who is told only "+
			"'needs a credential' will reach for the OAuth login this posture cannot use", err)
	}

	// Either named credential satisfies it, driven off the adapter's own table
	// so a credential added there is covered by the edit that adds it.
	for _, k := range credentialKeys {
		t.Run(k.ProviderKey, func(t *testing.T) {
			if _, err := parseSandboxProvider(map[string]any{
				"permission_mode": "auto", "sandbox_mode": SandboxSRT, k.ProviderKey: "secret",
			}); err != nil {
				t.Errorf("ParseProvider with %s = %v, want ok", k.ProviderKey, err)
			}
		})
	}
	// And none of it applies to the unsandboxed posture, which is the one every
	// existing workflow is on.
	if _, err := parseProvider(map[string]any{"permission_mode": "auto"}); err != nil {
		t.Errorf("ParseProvider under %s = %v, want ok", SandboxNone, err)
	}
}

// The §7.6 reservation extended to the four variables this posture writes,
// under *every* posture. Each exists to replace a tool's ambient configuration
// with one BEN wrote, so a second site setting any of them is not a duplicate
// value but a hole: a GIT_CONFIG_GLOBAL from `env` restores the `insteadOf`
// rewrite the pin exists to remove.
func TestSandboxVariablesAreReservedUnderEveryPosture(t *testing.T) {
	parallel(t)
	for _, posture := range append(slices.Clone(sandboxModes), "") {
		for _, name := range []string{envSandboxTmpDir, envGitConfig, envGitNoSystem, envGHConfigDir} {
			block := func(extra map[string]any) map[string]any {
				b := map[string]any{"permission_mode": "auto", "api_key": "sk-secret"}
				if posture != "" {
					b["sandbox_mode"] = posture
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

// --- what a run does with it ---

// sandboxRunner builds a runner whose harness is this test binary and whose
// sandbox runtime is the shared lightweight fake, with a git identity the
// posture can carry.
func sandboxRunner(t *testing.T, extra map[string]any) (*Runner, core.RunSpec) {
	t.Helper()
	sandbox := fakeSandboxBinary(t)
	// daemonGitIdentity reads the daemon account's own git configuration, so the
	// test pins one rather than depending on the host having any. Neither
	// variable is in core.EnvAllowlist, so neither reaches a child from here.
	identity := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(identity,
		[]byte("[user]\n\tname = ben-test\n\temail = ben@test.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", identity)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	block := contract().Block(selfPath(t), nil)
	block["sandbox_mode"] = SandboxSRT
	block["sandbox_binary"] = sandbox
	block["api_key"] = "sk-secret"
	for k, v := range extra {
		block[k] = v
	}
	t.Setenv(testPublish.Var, "gh-token-value")
	// A stub `gh` ahead of whatever the host has: the credential helper names
	// it, so Start resolves it, and a host without one would fail these tests
	// for a reason none of them is about.
	stubs := t.TempDir()
	if err := os.WriteFile(filepath.Join(stubs, "gh"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", stubs+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, err := New(Options{Provider: block, Publish: testPublishBinding()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	root := t.TempDir()
	spec := core.RunSpec{
		Workspace: core.WorkspacePaths{
			Path:         mkdir(t, root, "issues", "issue-7"),
			SharedGitDir: mkdir(t, root, "base.git"),
			PrivateDir:   mkdir(t, root, "private", "issue-7"),
		},
		Prompt: "do the thing",
	}
	return r, spec
}

func mkdir(t *testing.T, parts ...string) string {
	t.Helper()
	dir := filepath.Join(parts...)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// record is one invocation the fake wrote down: its arguments (argv[0]
// stripped, as probeArgs reports them) beside the environment it ran with.
//
// Paired rather than read from probeArgs and probeEnvs separately, because
// every assertion here is about *which* process saw what — the wrapper's
// environment and the harness's differ by design, and a test that matched them
// up by position would be reading the fake's dump discipline rather than the
// adapter's behaviour. The fake writes two records for a run it carries through
// (once on entry, once after reading the prompt) and one for a wrapper
// invocation, which never reaches the prompt read.
type record struct {
	args []string
	env  map[string]string
}

func (r record) isWrapper() bool { return isSandboxInvocation(r.args) }

func records(t *testing.T, path string) []record {
	t.Helper()
	argvs, envs := probeArgs(t, path), probeEnvs(t, path)
	if len(argvs) != len(envs) {
		t.Fatalf("dump holds %d argv records and %d env records", len(argvs), len(envs))
	}
	out := make([]record, len(argvs))
	for i := range argvs {
		out[i] = record{args: argvs[i], env: envs[i]}
	}
	return out
}

// onlyWrapper returns the single wrapper invocation, failing when the count is
// not one: "the wrapper ran" and "the wrapper ran twice" are different facts,
// and the second would mean the posture was composed more than once per
// attempt.
func onlyWrapper(t *testing.T, in []record) record {
	t.Helper()
	var hits []record
	for _, r := range in {
		if r.isWrapper() {
			hits = append(hits, r)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("recorded %d wrapper invocations, want exactly 1 (records: %v)", len(hits), in)
	}
	return hits[0]
}

// harnessRecords returns the invocations that are the harness itself. The fake
// writes one on entry and one after reading the prompt, so there is more than
// one by design and the count is not the assertion.
func harnessRecords(t *testing.T, in []record) []record {
	t.Helper()
	var hits []record
	for _, r := range in {
		if !r.isWrapper() && slices.Contains(r.args, "--output-format") {
			hits = append(hits, r)
		}
	}
	if len(hits) == 0 {
		t.Fatalf("no harness invocation recorded (records: %v)", in)
	}
	return hits
}

// startSandboxed runs one attempt to completion and returns what the fake
// recorded.
func startSandboxed(t *testing.T, extra map[string]any) ([]record, core.RunSpec) {
	t.Helper()
	dump := filepath.Join(t.TempDir(), "dump.json")
	if extra == nil {
		extra = map[string]any{}
	}
	extra["env"] = map[string]any{agenttest.DumpEnv: dump}

	r, spec := sandboxRunner(t, extra)
	h, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Stop(t.Context(), core.StopDiscard) })
	for range h.Events() {
	}
	return records(t, dump), spec
}

// The wrapping is a prefix and nothing more: the harness argv the adapter
// composed must arrive at the harness unchanged, or the sandbox has quietly
// become a second place that decides what the agent runs.
func TestSandboxWrapsTheHarnessArgvWithoutRewritingIt(t *testing.T) {
	recs, spec := startSandboxed(t, nil)
	wrapper := onlyWrapper(t, recs)
	harnessArgv := harnessRecords(t, recs)[0].args

	if wrapper.args[0] != sandboxSettingsFlag {
		t.Errorf("wrapper argv = %v, want the settings flag first", wrapper.args)
	}
	settings := wrapper.args[1]
	if want := filepath.Join(spec.Workspace.PrivateDir, controlDirName, settingsFileName); settings != want {
		t.Errorf("settings path = %q, want %q — the posture lives in the private dir the "+
			"provider placed, never one this adapter derived", settings, want)
	}
	inner := fakeSandboxInner(wrapper.args)
	if len(inner) == 0 {
		t.Fatalf("wrapper argv = %v, want a `--` separating it from the harness argv", wrapper.args)
	}
	// probeArgs strips argv[0], so the comparable slice of what the wrapper was
	// handed is inner[1:].
	if !slices.Equal(inner[1:], harnessArgv) {
		t.Errorf("the harness ran %v but the wrapper was given %v; wrapping must not rewrite argv",
			harnessArgv, inner[1:])
	}
	if !slices.Contains(harnessArgv, "-p") {
		t.Errorf("harness argv = %v, want the unwrapped invocation intact", harnessArgv)
	}
}

// #114 pins TMPDIR to an attempt-owned directory; the sandbox runtime replaces
// the child's TMPDIR with its own default unless CLAUDE_CODE_TMPDIR tells it
// where to point. Measured on srt 0.0.73 — and `/tmp/claude` does not exist on
// a fresh host, so the default is not merely shared but broken.
//
// Asserted off the environment the *harness* received, downstream of the
// wrapper's override, because that is the only place the two pins can be seen
// to agree.
func TestSandboxTempDirSurvivesTheRuntimesOwnOverride(t *testing.T) {
	recs, spec := startSandboxed(t, nil)
	wrapper := onlyWrapper(t, recs)

	want := filepath.Join(spec.Workspace.PrivateDir, tmpDirName)
	if got := wrapper.env[envSandboxTmpDir]; got != want {
		t.Errorf("%s = %q in the wrapper's environment, want %q; without it the runtime sends "+
			"the child's temp writes to a path no attempt owns", envSandboxTmpDir, got, want)
	}
	// Every harness record, not one of them: the pin has to hold downstream of
	// the wrapper's override for the whole run, not just at entry.
	for _, r := range harnessRecords(t, recs) {
		if got := r.env[envTmpDir]; got != want {
			t.Errorf("TMPDIR = %q at the harness, want %q — the runtime overrode #114's pin",
				got, want)
		}
	}
}

// The tool configurations BEN owns under this posture, each because the sandbox
// denies $HOME and the tool would otherwise resolve its own from there.
func TestSandboxOwnsTheToolConfigurationItDenies(t *testing.T) {
	recs, spec := startSandboxed(t, nil)
	harness := harnessRecords(t, recs)[0].env
	control := sandboxControlFor(spec.Workspace.PrivateDir)

	for _, tc := range []struct{ name, want, why string }{
		{envGitConfig, control.GitConfig,
			"git treats an unreadable ~/.gitconfig as fatal, and a host insteadOf rewrite redirects an agent's push"},
		{envGitNoSystem, "1",
			"this host carries a Homebrew system gitconfig; without the pin it is still in effect"},
		{envGHConfigDir, control.GHConfigDir,
			"gh refuses to start when it cannot read ~/.config/gh/config.yml, which lands on the publish step"},
	} {
		if got := harness[tc.name]; got != tc.want {
			t.Errorf("%s = %q, want %q — %s", tc.name, got, tc.want, tc.why)
		}
	}

	// The git config BEN wrote is the agent's whole global configuration, so it
	// has to carry an identity: with the global config replaced and no identity
	// in it, every `git commit` fails with "unable to auto-detect email address".
	b, err := os.ReadFile(control.GitConfig)
	if err != nil {
		t.Fatalf("reading the composed git config: %v", err)
	}
	for _, want := range []string{"ben-test", "ben@test.invalid", "excludesFile"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("composed git config does not carry %q:\n%s", want, b)
		}
	}
	// And the agent may not rewrite it: one that can restore an `insteadOf`
	// rewrite can redirect its next push.
	var settings sandboxSettings
	raw, err := os.ReadFile(control.Settings)
	if err != nil {
		t.Fatalf("reading the composed settings: %v", err)
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(settings.Filesystem.DenyWrite, control.Dir) {
		t.Errorf("denyWrite = %v, want the control dir %s — it holds the settings file and the "+
			"git config", settings.Filesystem.DenyWrite, control.Dir)
	}
	// The gh config dir is the exception: gh writes it, so it is a sibling of
	// the control dir rather than a child of it.
	if strings.HasPrefix(control.GHConfigDir, control.Dir+string(filepath.Separator)) {
		t.Errorf("%s = %q is inside the denied control dir; gh writes its own config",
			envGHConfigDir, control.GHConfigDir)
	}
	if info, err := os.Stat(control.GHConfigDir); err != nil || !info.IsDir() {
		t.Errorf("%s = %q was not created: %v", envGHConfigDir, control.GHConfigDir, err)
	}
}

// The settings file names this attempt's workspace, so it is composed per
// attempt. A stale one left from the previous attempt would bind a different
// tree — and under §6.2 reattachment the previous attempt's tree is a real
// directory, not a missing one, so nothing would fail loudly.
func TestSandboxSettingsAreComposedPerAttempt(t *testing.T) {
	r, spec := sandboxRunner(t, nil)
	control := sandboxControlFor(spec.Workspace.PrivateDir)
	if err := os.MkdirAll(control.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(control.Settings, []byte(`{"stale":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	h, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Stop(t.Context(), core.StopDiscard) })
	for range h.Events() {
	}

	raw, err := os.ReadFile(control.Settings)
	if err != nil {
		t.Fatal(err)
	}
	var settings sandboxSettings
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatalf("the stale settings file was not replaced: %v", err)
	}
	if !slices.Contains(settings.Filesystem.AllowWrite, spec.Workspace.Path) {
		t.Errorf("allowWrite = %v, want this attempt's workspace %s",
			settings.Filesystem.AllowWrite, spec.Workspace.Path)
	}
}

// A sandboxed run whose provider reported no shared git dir is refused, not run
// without one. Deriving it is forbidden (SPEC §7.1) and guessing would put a
// repository nobody named into allowWrite; without it `git commit` fails inside
// the workspace anyway, one attempt at a time.
func TestStartRefusesSandboxWithoutTheSharedGitDir(t *testing.T) {
	r, spec := sandboxRunner(t, nil)
	spec.Workspace.SharedGitDir = ""

	h, err := r.Start(t.Context(), spec)
	if err == nil {
		h.Stop(t.Context(), core.StopDiscard)
		t.Fatal("Start with no shared git dir succeeded; the run could not have committed")
	}
	if !errors.Is(err, ErrSharedGitDir) {
		t.Errorf("Start = %v, want ErrSharedGitDir", err)
	}
}

// The unsandboxed posture is the absence of this one, not a quieter version of
// it: every existing workflow is on it, and a variable or an argv prefix
// leaking into it would change runs nobody opted in.
func TestUnsandboxedPostureAddsNothing(t *testing.T) {
	parallel(t)
	dump := filepath.Join(t.TempDir(), "dump.json")
	block := contract().Block(selfPath(t), map[string]string{agenttest.DumpEnv: dump})
	r, err := New(Options{Provider: block})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	root := t.TempDir()
	spec := core.RunSpec{
		Workspace: core.WorkspacePaths{
			Path:         mkdir(t, root, "issues", "issue-7"),
			SharedGitDir: mkdir(t, root, "base.git"),
			PrivateDir:   mkdir(t, root, "private", "issue-7"),
		},
		Prompt: "do the thing",
	}
	h, err := r.Start(t.Context(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Stop(t.Context(), core.StopDiscard) })
	for range h.Events() {
	}

	recs := records(t, dump)
	for _, r := range recs {
		if r.isWrapper() {
			t.Errorf("an unsandboxed run was wrapped: %v", r.args)
		}
		if !slices.Contains(r.args, "-p") {
			t.Errorf("harness argv = %v, want the unwrapped invocation", r.args)
		}
		for _, name := range []string{envSandboxTmpDir, envGitConfig, envGitNoSystem, envGHConfigDir} {
			if got, ok := r.env[name]; ok {
				t.Errorf("%s = %q reached an unsandboxed child; `none` is the absence of the posture",
					name, got)
			}
		}
	}
	if len(harnessRecords(t, recs)) == 0 {
		t.Fatal("the harness never ran")
	}
	if _, err := os.Stat(filepath.Join(spec.Workspace.PrivateDir, controlDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("an unsandboxed run wrote a control dir (%v)", err)
	}
}

// The credential route §5.6's `git push origin HEAD` needs.
//
// Measured: with GIT_CONFIG_NOSYSTEM set and BEN's global config in place, git
// has no credential source at all — `git credential fill` for github.com
// answers "could not read Username" — because the origin URL is deliberately
// credential-free (§6.7) and git does not read GH_TOKEN. So the posture would
// commit and fail to publish, which is worse than not sandboxing.
func TestSandboxWritesTheCredentialHelperTheCanonicalPushNeeds(t *testing.T) {
	parallel(t)
	const gh = "/opt/homebrew/bin/gh"
	p := goldenProvider(t)
	paths := goldenPaths()
	paths.GH = gh

	config := gitConfigFile(gitIdentity{Name: "n", Email: "e@x.invalid"}, "/ignore", gh)
	if !strings.Contains(config, "helper") || !strings.Contains(config, "auth git-credential") {
		t.Errorf("composed git config carries no credential helper:\n%s", config)
	}
	// Unscoped, not keyed to https://github.com: BEN's tracker supports GitHub
	// Enterprise, so a github.com-only helper leaves every Enterprise deployment
	// unable to push, and `gh` answers nothing for a host it holds no token for.
	if strings.Contains(config, `[credential "https://github.com"]`) {
		t.Errorf("the helper is scoped to github.com, which cannot serve a GitHub Enterprise "+
			"remote:\n%s", config)
	}

	// The helper runs `gh`, so the posture has to permit it — an install under
	// the denied $HOME is otherwise unexecutable, the same trap the harness
	// binary's carve-out exists for.
	fs := p.sandboxSettings(paths, "linux", goldenHome).Filesystem
	if !slices.Contains(fs.AllowRead, gh) {
		t.Errorf("allowRead = %v, want the gh the credential helper runs", fs.AllowRead)
	}

	// And no helper without a publish credential: one that always answered
	// nothing would turn a missing credential into a git prompt.
	if got := gitConfigFile(gitIdentity{Name: "n", Email: "e@x.invalid"}, "/ignore", ""); strings.Contains(got, "credential") {
		t.Errorf("a block with no publish credential still got a helper:\n%s", got)
	}
	if fs := p.sandboxSettings(goldenPaths(), "linux", goldenHome).Filesystem; slices.Contains(fs.AllowRead, gh) {
		t.Errorf("allowRead = %v, want no gh carve-out when no helper is written", fs.AllowRead)
	}
}

// Values reaching this file are not literals: the identity comes from the
// daemon host's own git configuration and the paths from the workspace
// provider. Raw interpolation changes what git then reads — `#` truncates the
// line as a comment, `"` moves where the value starts and ends, `\` escapes the
// next character, and a newline ends the entry outright, which is a config
// section the host gets to choose.
//
// Round-tripped through real `git config --get` rather than compared to an
// expected encoding: the claim is that git reads back what BEN meant, and only
// git can settle that.
func TestGitConfigValuesRoundTripThroughGit(t *testing.T) {
	parallel(t)
	git := requireGit(t)
	for _, tc := range []struct{ name, value string }{
		{"plain", "ben-test"},
		{"a hash, which starts a comment", "/srv/ben#1/ignore"},
		{"a quote", `Ben "The Daemon"`},
		{"a backslash", `C:\Users\ben\ignore`},
		{"a semicolon, the other comment character", "ben;test"},
		{"leading and trailing space", "  ben  "},
		{"a newline, which would end the entry", "ben\ninjected = yes"},
		{"a tab", "ben\ttest"},
		{"a bracket, which would open a section", "ben[core]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gitconfig")
			body := "[user]\n\tname = " + gitConfigValue(tc.value) + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(git, "config", "--file", path, "--get", "user.name")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("git could not read back the value it was given (%v): %s\nfile:\n%s",
					err, out, body)
			}
			if got := strings.TrimSuffix(string(out), "\n"); got != tc.value {
				t.Errorf("git read %q, want %q\nfile:\n%s", got, tc.value, body)
			}
		})
	}
}

// The shell layer under the git-config layer: the helper value is a `!command`
// that git hands to a shell, so a path with a space or a quote in it has to
// survive both encodings.
func TestCredentialHelperSurvivesAnAwkwardBinaryPath(t *testing.T) {
	parallel(t)
	git := requireGit(t)
	for _, gh := range []string{
		"/opt/gh/gh",
		"/opt/my tools/gh",
		"/opt/ben's tools/gh",
		`/opt/gh"x"/gh`,
	} {
		t.Run(gh, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gitconfig")
			body := gitConfigFile(gitIdentity{Name: "n", Email: "e@x.invalid"}, "/ignore", gh)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(git, "config", "--file", path, "--get-all", "credential.helper").
				CombinedOutput()
			if err != nil {
				t.Fatalf("git could not read the helper back (%v): %s\nfile:\n%s", err, out, body)
			}
			// The last entry is the helper; the first is the reset.
			lines := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
			helper := lines[len(lines)-1]
			if !strings.HasPrefix(helper, "!") {
				t.Fatalf("helper = %q, want the `!command` form", helper)
			}
			// Not a substring check: a path holding a quote is correctly encoded
			// as `'…'\''…'`, which does not contain the path literally. What has
			// to be true is that *a shell* resolves the command back to it —
			// git hands this to `sh`, so `sh` is the thing to ask.
			args := strings.TrimSuffix(strings.TrimPrefix(helper, "!"), " auth git-credential")
			resolved, err := exec.Command("/bin/sh", "-c", "printf %s "+args).CombinedOutput()
			if err != nil {
				t.Fatalf("a shell could not parse the helper command %q: %v: %s", args, err, resolved)
			}
			if string(resolved) != gh {
				t.Errorf("the shell resolved the helper to %q, want %q\nfile:\n%s",
					resolved, gh, body)
			}
		})
	}
}

// The required-key test the golden cannot be: a golden proves the file has not
// changed, and a field deleted from the DTO changes the golden along with
// everything else, so regenerating it makes the loss invisible. This list is
// the independent anchor (AGENTS.md) — it is written in the settings schema's
// own vocabulary rather than derived from the struct, so dropping a field fails
// here whatever happens to testdata.
//
// Every entry is a setting whose default is a security decision, which is why
// omitting it is not neutral: srt fills what the file does not state.
func TestComposedSettingsCarryEverySecurityKey(t *testing.T) {
	parallel(t)
	raw, err := json.Marshal(goldenProvider(t).sandboxSettings(goldenPaths(), "darwin", goldenHome))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []struct{ section, name string }{
		{"filesystem", "allowRead"},
		{"filesystem", "denyRead"},
		{"filesystem", "allowWrite"},
		{"filesystem", "denyWrite"},
		{"filesystem", "allowGitConfig"},
		// Turns the whole filesystem policy off — "no read or write rules are
		// emitted" — so every path above is decoration if it is true.
		{"filesystem", "disabled"},
		{"network", "allowedDomains"},
		{"network", "deniedDomains"},
		// Without it the allowlist is, in srt's words, "a prompt-suppression
		// hint" referred to an ask callback an unattended daemon cannot answer.
		{"network", "strictAllowlist"},
		{"network", "allowLocalBinding"},
		{"network", "allowAllUnixSockets"},
		{"network", "allowUnixSockets"},
		// A mach service is a way out that is neither a domain nor a path.
		{"network", "allowMachLookup"},
		// srt documents this one as removing code-execution isolation.
		{"", "allowAppleEvents"},
		{"", "allowPty"},
		{"", "enableWeakerNetworkIsolation"},
		{"", "enableWeakerNestedSandbox"},
	} {
		where, name := got, key.name
		if key.section != "" {
			section, ok := got[key.section].(map[string]any)
			if !ok {
				t.Fatalf("composed settings have no %q section", key.section)
			}
			where = section
		}
		if _, ok := where[name]; !ok {
			t.Errorf("composed settings omit %s%s%s, so srt fills it from its own defaults",
				key.section, map[bool]string{true: ".", false: ""}[key.section != ""], name)
		}
	}
}
