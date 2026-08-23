package claudecode

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/agenttest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// readySandboxRunner builds a runner for the readiness path: harness and
// sandbox runtime are both this test binary, and PATH is narrowed to a
// directory this test controls so `git` and `gh` resolve to what it put there.
func readySandboxRunner(t *testing.T, bin string, extra map[string]any) *Runner {
	t.Helper()
	sandbox := fakeSandboxBinary(t)
	identity := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(identity,
		[]byte("[user]\n\tname = ben-test\n\temail = ben@test.invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", identity)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("PATH", bin)
	// Ready's enforcement probe plants a sentinel under the denied home to prove
	// the read policy, so the tests get a home of their own rather than writing
	// into the developer's.
	t.Setenv("HOME", t.TempDir())

	block := contract().Block(selfPath(t), nil)
	block["sandbox_mode"] = SandboxSRT
	block["sandbox_binary"] = sandbox
	block["api_key"] = "sk-secret"
	// The fake runtime forwards without enforcing unless told to, which is what
	// TestReadyRefusesARuntimeThatEnforcesNothing relies on; every other case
	// needs a runtime that behaves like the real one.
	block["env"] = map[string]any{fakeSandboxEnforceEnv: "read,write"}
	for k, v := range extra {
		block[k] = v
	}
	t.Setenv(testPublish.Var, "gh-token-value")
	r, err := New(Options{Provider: block, Publish: testPublishBinding()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// binDir builds a PATH directory holding the real `git` plus whichever stubs
// the case wants. `git` is real because daemonGitIdentity asks it a question
// only git can answer; `gh` is a stub because the whole point is to control
// what the egress probe sees.
func binDir(t *testing.T, stubs map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git is not on PATH: %v", err)
	}
	if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
		t.Fatal(err)
	}
	// A working `gh` unless the case supplies its own: every `srt` block needs a
	// publish credential now, so the credential helper resolves `gh` on every
	// readiness path and a directory without one fails for a reason no case here
	// is about.
	if _, ok := stubs["gh"]; !ok {
		if stubs == nil {
			stubs = map[string]string{}
		}
		stubs["gh"] = "#!/bin/sh\necho 5000\n"
	}
	for name, script := range stubs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// Under `srt` both harness probes run wrapped. A readiness that checked the
// unwrapped binary would answer for a process no attempt ever has — the
// divergence SPEC §7.1's bind-at-New rule exists to remove, reached by omission
// — and it is not hypothetical: on this repo's own install `claude` resolves
// through a symlink in a subtree the posture denies, so an unwrapped probe
// passes while every attempt dies with "command not found".
func TestReadyProbesTheHarnessThroughThePosture(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "probe.json")
	r := readySandboxRunner(t, binDir(t, nil), map[string]any{
		"env": map[string]any{agenttest.DumpEnv: dump, fakeSandboxEnforceEnv: "read,write"},
	})
	if err := r.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	recs := records(t, dump)
	var wrapped, settings []string
	for _, rec := range recs {
		if !rec.isWrapper() {
			continue
		}
		settings = append(settings, rec.args[1])
		// Only the invocations of the harness itself; readiness also wraps the
		// enforcement probes, which run /bin/sh and are asserted elsewhere.
		if inner := fakeSandboxInner(rec.args); len(inner) > 0 && inner[0] == selfPath(t) {
			wrapped = append(wrapped, strings.Join(inner[1:], " "))
		}
	}
	// Two probes, both wrapped: identity then credential.
	if len(wrapped) != 2 {
		t.Fatalf("wrapped harness probes = %v, want both --version and `auth status`", wrapped)
	}
	for i, want := range []string{"--version", "auth status"} {
		if !strings.Contains(wrapped[i], want) {
			t.Errorf("wrapped probe %d = %q, want it to carry %q", i, wrapped[i], want)
		}
	}
	// The probe posture is a real composed file, not a placeholder — and it is
	// gone afterwards. Readiness runs at every startup and every reload, so a
	// probe directory that outlives it accumulates one per boot.
	for _, path := range settings {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("Ready left its probe posture %s behind (%v)", path, err)
		}
	}
}

// A sandbox runtime that is not installed is a startup refusal, not a dispatch
// one. Named apart from ErrBinary because the operator installs a different
// thing.
func TestReadyRefusesAnAbsentSandboxRuntime(t *testing.T) {
	r := readySandboxRunner(t, binDir(t, nil), map[string]any{
		"sandbox_binary": "srt-that-is-not-installed",
	})
	err := r.Ready(t.Context())
	if !errors.Is(err, ErrSandbox) {
		t.Fatalf("Ready = %v, want ErrSandbox", err)
	}
	if errors.Is(err, ErrBinary) {
		t.Errorf("Ready = %v, want the sandbox runtime's refusal, not the harness binary's — "+
			"they name different things to install", err)
	}
}

// The posture replaces the global git configuration, so the daemon account's
// own identity is the only one a run has. Without it every `git commit` an
// agent makes fails with "unable to auto-detect email address" — a whole
// attempt's work, discovered at the commit.
func TestReadyRefusesWithoutTheDaemonsGitIdentity(t *testing.T) {
	empty := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(empty, []byte("[core]\n\tbare = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := readySandboxRunner(t, binDir(t, nil), nil)
	// After the helper, so this is the configuration git actually reads.
	t.Setenv("GIT_CONFIG_GLOBAL", empty)

	err := r.Ready(t.Context())
	if !errors.Is(err, ErrSandboxIdentity) {
		t.Fatalf("Ready = %v, want ErrSandboxIdentity", err)
	}
	if !strings.Contains(err.Error(), "user.name") {
		t.Errorf("Ready = %v, want it to name the setting the operator adds", err)
	}
}

// The behavioural half (#81 F3′). Neither the settings file nor the runtime's
// version can answer this: srt strips unknown settings keys silently instead of
// refusing them, and reports version 1.0.0 whatever the package version is. So
// a posture missing the darwin pin produces a sandbox that looks configured and
// a run that commits, pushes, and cannot open a PR.
func TestReadyProbesEgressBehaviourally(t *testing.T) {
	// The failure the probe exists to catch, in the shape `gh` reports it.
	const ghFails = "#!/bin/sh\n" +
		`echo 'Get "https://api.github.com/rate_limit": tls: failed to verify certificate: ` +
		`x509: OSStatus -26276' >&2` + "\nexit 1\n"
	const ghWorks = "#!/bin/sh\necho 5000\n"

	for _, tc := range []struct {
		name    string
		gh      string
		publish core.PublishBinding
		want    error
	}{
		{name: "a Go client can verify TLS", gh: ghWorks, publish: testPublishBinding()},
		{name: "it cannot", gh: ghFails, want: ErrSandboxPosture, publish: testPublishBinding()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox := fakeSandboxBinary(t)
			t.Setenv("BEN_TEST_PUBLISH", "gh-token-value")
			bin := binDir(t, map[string]string{"gh": tc.gh})
			identity := filepath.Join(t.TempDir(), "gitconfig")
			if err := os.WriteFile(identity,
				[]byte("[user]\n\tname = ben-test\n\temail = ben@test.invalid\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_CONFIG_GLOBAL", identity)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			t.Setenv("PATH", bin)
			t.Setenv("HOME", t.TempDir())

			block := contract().Block(selfPath(t), nil)
			block["sandbox_mode"] = SandboxSRT
			block["sandbox_binary"] = sandbox
			block["api_key"] = "sk-secret"
			// The enforcement probes run first and are not what this case is
			// about, so the runtime has to behave like the real one for the
			// egress verdict to be the one under test.
			block["env"] = map[string]any{fakeSandboxEnforceEnv: "read,write"}
			r, err := New(Options{Provider: block, Publish: tc.publish})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			err = r.Ready(t.Context())
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Ready = %v, want ok", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Ready = %v, want %v", err, tc.want)
			}
			// The refusal has to say what a run would have done, or an operator
			// reads a TLS error at startup as a network blip and retries.
			if !strings.Contains(err.Error(), "PR") {
				t.Errorf("Ready = %v, want it to name the publish step it protects", err)
			}
			// And it must not carry the credential it probed with.
			if strings.Contains(err.Error(), "gh-token-value") {
				t.Errorf("Ready = %v names the publish credential", err)
			}
		})
	}
}

// `none` leaves readiness exactly as it was: two unwrapped probes, no runtime
// resolution, no git identity requirement. Every existing workflow is on it.
func TestReadyUnderTheUnsandboxedPostureIsUnchanged(t *testing.T) {
	dump := filepath.Join(t.TempDir(), "probe.json")
	// No sandbox runtime and no git on PATH: neither is this posture's business.
	t.Setenv("PATH", t.TempDir())
	if err := testRunner(t, map[string]string{agenttest.DumpEnv: dump}).Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, rec := range records(t, dump) {
		if rec.isWrapper() {
			t.Errorf("an unsandboxed readiness probe was wrapped: %v", rec.args)
		}
	}
}

// A daemon started without HOME — a systemd unit that does not set it — must
// not compose a posture whose denyRead entry is the empty string. That is a
// read policy bounding nothing, reported as a sandbox.
func TestReadyRefusesWithoutAHomeToDeny(t *testing.T) {
	r := readySandboxRunner(t, binDir(t, nil), nil)
	t.Setenv("HOME", "")

	err := r.Ready(t.Context())
	if !errors.Is(err, ErrSandbox) {
		t.Fatalf("Ready = %v, want ErrSandbox", err)
	}
	if !strings.Contains(err.Error(), "HOME") {
		t.Errorf("Ready = %v, want it to name the variable the operator sets", err)
	}
}

// The refusal that closes the gap a purely-behavioural egress check leaves: a
// wrapper that loads the settings and simply executes the child passes
// `--version`, `auth status` and `gh api rate_limit` alike while enforcing
// nothing, and would be reported as a delivered posture.
//
// Not a hypothetical shape — the runtime strips settings keys it does not
// recognize rather than refusing them, so a build whose filesystem schema has
// moved is exactly this, and `sandbox_binary` is an operator-supplied path
// besides. The fake is that shim by default, which is what makes this testable
// at all.
func TestReadyRefusesARuntimeThatEnforcesNothing(t *testing.T) {
	r := readySandboxRunner(t, binDir(t, nil), map[string]any{
		"env": map[string]any{fakeSandboxEnforceEnv: ""},
	})

	err := r.Ready(t.Context())
	if !errors.Is(err, ErrSandboxPosture) {
		t.Fatalf("Ready = %v, want ErrSandboxPosture — a wrapper that enforces nothing must "+
			"not pass readiness", err)
	}
	// The message has to say which half failed, because the two are fixed
	// differently: an unenforced write policy is a broken runtime, an unenforced
	// read policy on an enforcing one is a schema mismatch.
	if !strings.Contains(err.Error(), "write") && !strings.Contains(err.Error(), "read") {
		t.Errorf("Ready = %v, want it to name the probe that was not refused", err)
	}
}

// Both negative probes must fail independently, so all four combinations are
// covered rather than the two that agree. A runtime honouring denyWrite and
// ignoring denyRead keeps the agent out of its own hooks and hands it the
// operator's ssh key; the reverse leaves the agent able to rewrite its own
// sandbox. Testing only neither/both cannot tell a check that covers each from
// one that covers whichever it happens to run first.
func TestReadyProbesReadAndWriteDenialsSeparately(t *testing.T) {
	for _, tc := range []struct {
		name, enforce string
		want          error
	}{
		{name: "neither policy enforced", enforce: "", want: ErrSandboxPosture},
		{name: "writes denied, reads not", enforce: "write", want: ErrSandboxPosture},
		{name: "reads denied, writes not", enforce: "read", want: ErrSandboxPosture},
		{name: "both enforced", enforce: "read,write"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := readySandboxRunner(t, binDir(t, nil), map[string]any{
				"env": map[string]any{fakeSandboxEnforceEnv: tc.enforce},
			})
			err := r.Ready(t.Context())
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Ready = %v, want ok — a runtime enforcing both must pass", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("Ready = %v, want %v — half a policy is not the posture", err, tc.want)
			}
		})
	}
}

// `srt` with no `publish` block is refused at load. SPEC §5.2.8 permits the
// omission and says what it means — the agent authenticates from what §7.6's
// allowlist carries, "HOME, and whatever the forge CLI stores under it" — which
// is exactly the arrangement this posture denies. Accepting it would produce a
// run that works, commits, and has no credential to publish with.
func TestSandboxRefusesWithoutAPublishCredential(t *testing.T) {
	parallel(t)
	block := map[string]any{
		"permission_mode": "auto",
		"api_key":         "sk-secret",
		"sandbox_mode":    SandboxSRT,
	}
	_, err := ParseProvider(core.AgentConfig{Provider: block})
	if !errors.Is(err, ErrSandboxPublish) {
		t.Fatalf("ParseProvider = %v, want ErrSandboxPublish", err)
	}
	if !strings.Contains(err.Error(), "publish") {
		t.Errorf("refusal = %v, want it to name the block the operator adds", err)
	}
	// And it is the posture that requires it, not this adapter in general:
	// omission stays permitted where the agent can still reach the forge CLI's
	// own credential under $HOME.
	if _, err := ParseProvider(core.AgentConfig{Provider: map[string]any{
		"permission_mode": "auto",
	}}); err != nil {
		t.Errorf("ParseProvider under %s with no publish block = %v, want ok", SandboxNone, err)
	}
}

// Ready and Start must run the same sandbox runtime. Both `sandbox_binary` and
// the credential helper's `gh` default to bare names resolved on PATH, and
// Ready is where the runtime is *behaviourally* verified — so re-resolving at
// Start would let a PATH entry appearing in between hand an attempt a sandbox
// nothing checked. Sharper here than for the harness binary: the thing being
// swapped is the thing doing the bounding.
//
// Asserted by removing them from PATH entirely after Ready: a runner that
// resolved again would refuse, and one that kept the answer starts.
func TestStartRunsTheSandboxReadyVerified(t *testing.T) {
	bin := binDir(t, nil)
	if err := os.Symlink(fakeSandboxBinary(t), filepath.Join(bin, "srt")); err != nil {
		t.Fatal(err)
	}
	r := readySandboxRunner(t, bin, map[string]any{
		// A bare name, which is the default and the only shape PATH can move.
		"sandbox_binary": "srt",
	})
	if err := r.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// PATH loses both the runtime and gh; git stays, since daemonGitIdentity
	// asks it a fresh question every attempt by design.
	after := t.TempDir()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not on PATH")
	}
	if err := os.Symlink(git, filepath.Join(after, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", after)

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
		t.Fatalf("Start after PATH changed = %v; the attempt must run the runtime Ready "+
			"verified, not whatever PATH answers now", err)
	}
	t.Cleanup(func() { h.Stop(t.Context(), core.StopDiscard) })
	for range h.Events() {
	}
}

// The divergence a Ready→Start test cannot reach: within *one* Ready, the
// credential helper written into the posture names the memoized `gh`, while a
// fresh lookup in the egress probe could certify a different one. PATH changed
// after Ready never shows it, because by then both have already run.
//
// Reached by warming the memo against one directory and then moving PATH: a
// probe that re-resolved would run the second `gh` and certify a binary no
// attempt will ever use.
func TestReadyProbesTheSameGHItsPostureNames(t *testing.T) {
	first := binDir(t, map[string]string{"gh": "#!/bin/sh\necho 5000\n"})
	dump := filepath.Join(t.TempDir(), "probe.json")
	r := readySandboxRunner(t, first, map[string]any{
		"env": map[string]any{agenttest.DumpEnv: dump, fakeSandboxEnforceEnv: "read,write"},
	})

	// Warm the memo against `first`, exactly as an earlier Ready would have.
	warmed, err := r.resolveGH()
	if err != nil {
		t.Fatalf("resolveGH: %v", err)
	}
	if want := filepath.Join(first, "gh"); warmed != want {
		t.Fatalf("resolveGH = %q, want %q", warmed, want)
	}

	// A second, equally valid `gh` earlier on PATH. Both work, so only the path
	// distinguishes them.
	second := binDir(t, map[string]string{"gh": "#!/bin/sh\necho 5000\n"})
	t.Setenv("PATH", second)
	if err := r.Ready(t.Context()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	var probed []string
	for _, rec := range records(t, dump) {
		if !rec.isWrapper() {
			continue
		}
		if inner := fakeSandboxInner(rec.args); len(inner) > 0 && filepath.Base(inner[0]) == "gh" {
			probed = append(probed, inner[0])
		}
	}
	if len(probed) != 1 {
		t.Fatalf("gh probe invocations = %v, want exactly 1", probed)
	}
	if probed[0] != warmed {
		t.Errorf("the egress probe ran %q but the posture's credential helper names %q; "+
			"Ready certified a binary no attempt will use", probed[0], warmed)
	}
}

// `none` adds nothing, and that includes needing the tools the posture's own
// machinery needs. `gh` is resolved only because the credential helper names it,
// and that helper exists only under `srt` — so an unsandboxed workflow with a
// publish block on a host without `gh` must go on starting exactly as it did
// before this key existed.
//
// The publish block is the half that makes this reachable: without one there is
// no credential to write a helper for, so a test lacking it passes whatever the
// resolution is gated on.
func TestUnsandboxedReadyDoesNotRequireGH(t *testing.T) {
	// PATH holds neither `gh` nor a sandbox runtime — nothing `none` has any use
	// for. The harness binary is absolute, so it is still found.
	t.Setenv("PATH", t.TempDir())
	t.Setenv(testPublish.Var, "gh-token-value")

	r, err := New(Options{
		Provider: contract().Block(selfPath(t), nil),
		Publish:  testPublishBinding(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := r.Ready(t.Context()); err != nil {
		t.Fatalf("Ready under %s = %v, want ok — `gh` is the sandbox posture's dependency, "+
			"not this adapter's", SandboxNone, err)
	}
}
