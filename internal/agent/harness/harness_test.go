package harness

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The process runtime in this package — liveness windows, the signal ladder,
// the pump, transcript retention — is deliberately not tested here: it is only
// real against a real harness process, and both adapters exercise it through
// the shared conformance suite (internal/agent/agenttest). What is tested here
// is the part that is pure, where a table says more than a subprocess can.

var (
	errNamespace = errors.New("test: namespace")
	errPrompt    = errors.New("test: prompt")
	errWorkspace = errors.New("test: workspace")
	errValue     = errors.New("test: value")
	errKey       = errors.New("test: key")
	errBinary    = errors.New("test: binary")
)

var testErrors = SpecErrors{EnvNamespace: errNamespace, PromptEmpty: errPrompt, WorkspacePath: errWorkspace}

func TestCheckSpec(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	valid := core.RunSpec{Prompt: "do the thing", Workspace: core.WorkspacePaths{Path: dir}}

	for _, tc := range []struct {
		name string
		spec core.RunSpec
		want error
	}{
		{"valid", valid, nil},
		{"empty prompt", core.RunSpec{Workspace: core.WorkspacePaths{Path: dir}}, errPrompt},
		{"relative workspace", core.RunSpec{Prompt: "x", Workspace: core.WorkspacePaths{Path: "rel/path"}}, errWorkspace},
		{"missing workspace", core.RunSpec{Prompt: "x", Workspace: core.WorkspacePaths{Path: filepath.Join(dir, "nope")}}, errWorkspace},
		{"workspace is a file", core.RunSpec{Prompt: "x", Workspace: core.WorkspacePaths{Path: file}}, errWorkspace},
		{
			// SPEC §7.6: the reservation is exclusive, and the refusal comes
			// before the prompt check because it is about who owns the child.
			name: "env outside the reserved namespace",
			spec: core.RunSpec{Prompt: "x", Workspace: core.WorkspacePaths{Path: dir}, Env: map[string]string{"HOME": "/elsewhere"}},
			want: errNamespace,
		},
		{
			name: "the orchestrator's own namespace",
			spec: core.RunSpec{Prompt: "x", Workspace: core.WorkspacePaths{Path: dir}, Env: map[string]string{"BEN_RUN_ID": "7"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckSpec(tc.spec, testErrors)
			if tc.want == nil {
				if err != nil {
					t.Errorf("CheckSpec = %v, want ok", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("CheckSpec = %v, want %v", err, tc.want)
			}
		})
	}
}

// The refusal names the offending key, and it names the same one every time:
// several bad entries in one map must not produce a message that changes
// between runs.
func TestCheckSpecNamesOneKeyDeterministically(t *testing.T) {
	spec := core.RunSpec{
		Prompt:    "x",
		Workspace: core.WorkspacePaths{Path: t.TempDir()},
		Env:       map[string]string{"ZZZ": "z", "AAA": "a", "BEN_RUN_ID": "7"},
	}
	for range 20 {
		err := CheckSpec(spec, testErrors)
		if !strings.Contains(err.Error(), `"AAA"`) {
			t.Fatalf("CheckSpec = %v, want the first key in sorted order", err)
		}
	}
}

func TestCheckProviderEnv(t *testing.T) {
	for _, tc := range []struct {
		name        string
		env         map[string]string
		passthrough []string
		wantErr     bool
	}{
		{name: "nothing reserved", env: map[string]string{"GH_TOKEN": "t"}, passthrough: []string{"HTTPS_PROXY"}},
		{name: "env defines the prefix", env: map[string]string{"BEN_RUN_ID": "spoofed"}, wantErr: true},
		{name: "passthrough names the prefix", passthrough: []string{"BEN_RUN_ID"}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckProviderEnv(tc.env, tc.passthrough, errNamespace)
			if got := errors.Is(err, errNamespace); got != tc.wantErr {
				t.Errorf("CheckProviderEnv = %v, want refusal=%v", err, tc.wantErr)
			}
		})
	}
}

func TestCheckOwnedEnv(t *testing.T) {
	owned := []ReservedEnv{
		{Name: "ADAPTER_KEY", Owner: "agent.provider.api_key"},
		{Name: "ADAPTER_HOME", Owner: "agent.provider.home"},
	}
	for _, tc := range []struct {
		name        string
		env         map[string]string
		passthrough []string
		wantErr     bool
	}{
		{name: "unrelated names are what env is for", env: map[string]string{"GH_TOKEN": "t"}, passthrough: []string{"HTTPS_PROXY"}},
		{name: "owned name in env", env: map[string]string{"ADAPTER_KEY": "sneaked"}, wantErr: true},
		{name: "owned name in passthrough", passthrough: []string{"ADAPTER_HOME"}, wantErr: true},
		{
			// Case matters: environment variables are case-sensitive, and a
			// lookalike is a different variable the harness will not read.
			name: "lookalike is not the owned name",
			env:  map[string]string{"adapter_key": "different variable"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckOwnedEnv(tc.env, tc.passthrough, owned, errNamespace)
			if got := errors.Is(err, errNamespace); got != tc.wantErr {
				t.Errorf("CheckOwnedEnv = %v, want refusal=%v", err, tc.wantErr)
			}
		})
	}
}

func TestEnviron(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/ben")
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "daemon-aws-MUST-NOT-LEAK")

	composed, forwarded := Environ(
		[]string{"HTTPS_PROXY"},
		map[string]string{"GH_TOKEN": "provider-gh"},
		map[string]string{"API_KEY": "provider-key", "UNSET_SURFACE": ""},
		PublishValue{},
		core.RunSpec{Env: map[string]string{"BEN_RUN_ID": "run-7"}},
	)
	env := envMap(composed)

	// The redaction set's third layer, and only that layer: the allowlist's values
	// are the daemon's own PATH and HOME, and `injected` mixes a credential with a
	// filesystem path by design, so neither belongs in it (SPEC §10.3).
	if want := map[string]string{"env_passthrough.HTTPS_PROXY": "http://proxy:3128"}; !maps.Equal(forwarded, want) {
		t.Errorf("forwarded = %v, want %v", forwarded, want)
	}

	for k, want := range map[string]string{
		"PATH":        "/usr/bin",
		"HOME":        "/home/ben",
		"HTTPS_PROXY": "http://proxy:3128",
		"GH_TOKEN":    "provider-gh",
		"API_KEY":     "provider-key",
		"BEN_RUN_ID":  "run-7",
	} {
		if env[k] != want {
			t.Errorf("env[%q] = %q, want %q", k, env[k], want)
		}
	}
	// An adapter's auth surface that the block did not configure injects
	// nothing: an empty value is an omission, not an empty credential.
	if _, ok := env["UNSET_SURFACE"]; ok {
		t.Error("an empty injected value reached the child")
	}
	if _, ok := env["AWS_SECRET_ACCESS_KEY"]; ok {
		t.Error("the child inherited a daemon variable outside the allowlist")
	}
}

// Deterministic for tests and for the audit log.
func TestEnvironIsSorted(t *testing.T) {
	got, _ := Environ(nil, map[string]string{"ZED": "1", "ALPHA": "2", "MID": "3"}, nil, PublishValue{}, core.RunSpec{})
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("Environ is unsorted at %d: %v", i, got)
		}
	}
}

// A variable the daemon does not have is simply absent — never an empty value,
// which a harness would read as a deliberate override.
func TestEnvironSkipsUnsetPassthrough(t *testing.T) {
	os.Unsetenv("BEN_TEST_ABSENT_VAR")
	composed, forwarded := Environ([]string{"BEN_TEST_ABSENT_VAR"}, nil, nil, PublishValue{}, core.RunSpec{})
	if _, ok := envMap(composed)["BEN_TEST_ABSENT_VAR"]; ok {
		t.Error("an unset passthrough name became an empty child variable")
	}
	// And it contributes no needle: "" would match every write.
	if len(forwarded) != 0 {
		t.Errorf("forwarded = %v, want nothing for an unset name", forwarded)
	}
}

func TestBlockValues(t *testing.T) {
	b := NewBlock(map[string]any{
		"str":  "s",
		"yes":  true,
		"list": []any{"a", "b"},
		"map":  map[string]any{"K": "v"},
		"null": nil,
	}, errValue)

	if got, err := b.String("str"); got != "s" || err != nil {
		t.Errorf("String = %q, %v", got, err)
	}
	if got, err := b.String("missing"); got != "" || err != nil {
		t.Errorf("String(missing) = %q, %v", got, err)
	}
	if got, err := b.String("null"); got != "" || err != nil {
		t.Errorf("String(null) = %q, %v, want an omission", got, err)
	}
	if got, err := b.Bool("yes"); !got || err != nil {
		t.Errorf("Bool = %v, %v", got, err)
	}
	if got, err := b.Strings("list"); !reflect.DeepEqual(got, []string{"a", "b"}) || err != nil {
		t.Errorf("Strings = %v, %v", got, err)
	}
	if got, err := b.StringMap("map"); !reflect.DeepEqual(got, map[string]string{"K": "v"}) || err != nil {
		t.Errorf("StringMap = %v, %v", got, err)
	}
}

func TestBlockRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(Block) error
	}{
		{"string is not a string", func(b Block) error { _, err := b.String("num"); return err }},
		{"bool is not a bool", func(b Block) error { _, err := b.Bool("num"); return err }},
		{"list is not a list", func(b Block) error { _, err := b.Strings("num"); return err }},
		{"list entry is not a string", func(b Block) error { _, err := b.Strings("mixed"); return err }},
		{"list entry is empty", func(b Block) error { _, err := b.Strings("blank"); return err }},
		{"map is not a map", func(b Block) error { _, err := b.StringMap("num"); return err }},
		{"map value is not a string", func(b Block) error { _, err := b.StringMap("badmap"); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBlock(map[string]any{
				"num":    7,
				"mixed":  []any{"a", 1},
				"blank":  []any{""},
				"badmap": map[string]any{"K": 1},
			}, errValue)
			if err := tc.read(b); !errors.Is(err, errValue) {
				t.Errorf("read = %v, want %v", err, errValue)
			}
		})
	}
}

func TestBlockUnknownKey(t *testing.T) {
	known := []string{"alpha", "beta"}
	if err := NewBlock(map[string]any{"alpha": "a"}, errValue).Unknown(known, errKey); err != nil {
		t.Errorf("Unknown = %v, want ok", err)
	}
	err := NewBlock(map[string]any{"alpha": "a", "gamma": "g"}, errValue).Unknown(known, errKey)
	if !errors.Is(err, errKey) {
		t.Fatalf("Unknown = %v, want %v", err, errKey)
	}
	// The message has to name the key *and* the closed set, since the whole
	// point of the refusal is to catch a typo.
	if !strings.Contains(err.Error(), `"gamma"`) || !strings.Contains(err.Error(), "alpha, beta") {
		t.Errorf("Unknown = %q, want the offending key and the known set", err)
	}
}

// A relative binding is an execution path the agent controls: Start runs with
// cwd set to the workspace, so a relative argv[0] would resolve there at exec
// time (SPEC §7.1).
func TestResolveBinaryIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "harness")
	if err := os.WriteFile(name, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	got, err := ResolveBinary("./harness", errBinary)
	if err != nil {
		t.Fatalf("ResolveBinary: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("ResolveBinary = %q, want an absolute path", got)
	}

	if _, err := ResolveBinary("no-such-harness-anywhere", errBinary); !errors.Is(err, errBinary) {
		t.Errorf("ResolveBinary(missing) = %v, want %v", err, errBinary)
	}
}

// Timings.ProbeWait is the readiness half of the seam, and it is asserted here
// because the conformance suite structurally cannot: testReadyBounded asserts
// that a probe leaking a pipe returns *bounded*, not which bound, so it passes
// under the production window and the injected one alike.
//
// The window is only paid when a child outlives the probe holding its stdout, so
// that is what this stages: a shell that backgrounds a sleep and exits. Its
// process is gone immediately, the write end is not, and Wait therefore has
// nothing to wait for but WaitDelay.
func TestProbeWaitIsTheInjectedWindow(t *testing.T) {
	const injected = 100 * time.Millisecond

	start := time.Now()
	// An explicit PATH rather than the daemon's environment, which is the rule a
	// real probe is held to (SPEC §7.6) and all this one needs.
	out, _ := Probe(context.Background(), Timings{ProbeWait: injected},
		"/bin/sh", []string{"PATH=/usr/bin:/bin"}, "-c", "sleep 30 & echo held")
	elapsed := time.Since(start)

	if production := DefaultTimings().ProbeWait; elapsed >= production {
		t.Errorf("a probe with a leaked pipe returned in %v, at or past the production "+
			"ProbeWait of %v: the injected %v is not reaching exec.Cmd.WaitDelay",
			elapsed, production, injected)
	}
	// The other direction: the window is honored rather than skipped. A probe that
	// returned instantly would not be bounding the leaked pipe at all.
	if elapsed < injected {
		t.Errorf("a probe with a leaked pipe returned in %v, inside its own %v window",
			elapsed, injected)
	}
	// And the bound is not achieved by throwing the answer away — readiness
	// classifies from the body (SPEC §7.1), so a window that cost the output would
	// refuse every configuration behind a leaky harness.
	if got := strings.TrimSpace(string(out)); got != "held" {
		t.Errorf("probe output = %q, want %q: the bound discarded what the child wrote", got, "held")
	}
}

func TestIsTerminal(t *testing.T) {
	for _, ty := range []core.EventType{core.EventSucceeded, core.EventFailed} {
		if !IsTerminal(ty) {
			t.Errorf("IsTerminal(%v) = false", ty)
		}
	}
	for _, ty := range []core.EventType{core.EventStarted, core.EventProgress, core.EventUsage, core.EventHeartbeat} {
		if IsTerminal(ty) {
			t.Errorf("IsTerminal(%v) = true", ty)
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

// A name in both env_passthrough and env takes the block's value: the host value
// never reaches the child, so it is not a forwarded value and must not be in the
// redaction set. Recording it during composition instead of after would put a
// value the child never got into the set — and, because CheckRedactableEnv holds
// forwarded values to a shape requirement, would refuse a run over a host value
// the block had already overridden.
func TestEnvironDropsPassthroughOverriddenByTheBlock(t *testing.T) {
	// Unredactable if it were forwarded: eight bytes, no eligible line.
	t.Setenv("FORWARDED_PAT", "abc\n1234")

	composed, forwarded := Environ(
		[]string{"FORWARDED_PAT"},
		map[string]string{"FORWARDED_PAT": "block-value-0123456789"},
		nil, PublishValue{}, core.RunSpec{},
	)
	if got := envMap(composed)["FORWARDED_PAT"]; got != "block-value-0123456789" {
		t.Errorf("child FORWARDED_PAT = %q, want the block's value", got)
	}
	if len(forwarded) != 0 {
		t.Errorf("forwarded = %v, want nothing: the host value did not reach the child", forwarded)
	}
	// And therefore no refusal over a value nothing will write.
	if err := CheckRedactableEnv(forwarded, errors.New("test")); err != nil {
		t.Errorf("CheckRedactableEnv = %v, want ok: refusing an overridden host value", err)
	}
}

// The same name with the same value on both sides stays reported — it is in the
// child either way, and dropping it would depend on which layer "won" a tie.
func TestEnvironKeepsPassthroughMatchingTheBlock(t *testing.T) {
	t.Setenv("FORWARDED_PAT", "ghp-same-value-0123456789")
	_, forwarded := Environ(
		[]string{"FORWARDED_PAT"},
		map[string]string{"FORWARDED_PAT": "ghp-same-value-0123456789"},
		nil, PublishValue{}, core.RunSpec{},
	)
	if forwarded["env_passthrough.FORWARDED_PAT"] != "ghp-same-value-0123456789" {
		t.Errorf("forwarded = %v, want the value that reached the child", forwarded)
	}
}

// The migration pin, environment half (#117 boundary 1): forwarding a variable by
// name and naming it as the publish credential compose the *same* child
// environment, so moving BEN's own workflow from `env_passthrough: [GH_TOKEN]` to
// a `publish` block changed nothing about what the agent gets.
//
// The other half is TestPublishBlockDenotesTheEnvPassthroughItReplaces in
// internal/config, which asserts the two spellings denote these two calls. Split
// because neither package can hold both: composing a child environment is not the
// loader's job, and this package must not import the loader.
//
// The redaction maps are compared by *value* and not by key: the keys differ by
// design, because each names the site the value came from and that is what a
// refusal has to say.
func TestEnvironPublishMatchesForwardingTheSameVariable(t *testing.T) {
	const token = "ghp-dogfood-token-0123456789"
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("HOME", "/home/ben")
	t.Setenv("GH_TOKEN", token)

	spec := core.RunSpec{Env: map[string]string{"BEN_RUN_ID": "run-7"}}
	forwarded, forwardedRedact := Environ([]string{"GH_TOKEN"}, nil, nil, PublishValue{}, spec)
	published, publishedRedact := Environ(nil, nil, nil, PublishValue{Env: "GH_TOKEN", Value: token}, spec)

	if !slices.Equal(forwarded, published) {
		t.Errorf("the two spellings compose different child environments:\n passthrough %v\n publish     %v",
			forwarded, published)
	}
	if got := envMap(published)["GH_TOKEN"]; got != token {
		t.Errorf("child GH_TOKEN = %q, want the publish credential", got)
	}
	if want := []string{token}; !slices.Equal(slices.Sorted(maps.Values(publishedRedact)), want) {
		t.Errorf("publish redaction values = %v, want %v", publishedRedact, want)
	}
	if !slices.Equal(slices.Sorted(maps.Values(forwardedRedact)), slices.Sorted(maps.Values(publishedRedact))) {
		t.Errorf("the two spellings redact different values:\n passthrough %v\n publish     %v",
			forwardedRedact, publishedRedact)
	}
	// The keys differ, and that is the point: the site is what a refusal names.
	if _, ok := publishedRedact["publish.value"]; !ok {
		t.Errorf("publish redaction keys = %v, want it keyed by publish.value", publishedRedact)
	}
}

// The publish credential is injected last among the config-derived layers, so no
// provider surface can override it.
//
// The §7.6 reservation makes this collision unexpressible in a workflow file — it
// is refused at load, and a constructed runner therefore cannot present it — so
// this asserts at the one level that can still reach the combination. It is
// defence in depth for the same reason §7.6 gives everywhere else: if two sites
// ever disagree about a variable, an ordering that let the unvalidated one win
// would be the wrong answer.
func TestEnvironPublishIsNotOverridableByTheBlock(t *testing.T) {
	composed, _ := Environ(
		nil,
		map[string]string{"GH_TOKEN": "block-value"},
		map[string]string{"GH_TOKEN": "injected-value"},
		PublishValue{Env: "GH_TOKEN", Value: "publish-value"},
		core.RunSpec{},
	)
	if got := envMap(composed)["GH_TOKEN"]; got != "publish-value" {
		t.Errorf("child GH_TOKEN = %q, want the publish credential to win", got)
	}
}

// One child variable, one owning config site — the direction that refuses a
// `publish.env` naming a variable somebody else owns (SPEC §5.2.8, §7.6).
func TestCheckPublishEnv(t *testing.T) {
	sentinel := errors.New("test: reserved")
	reserved := []ReservedEnv{
		{Name: "ADAPTER_KEY", Owner: "agent.provider.api_key"},
		{Name: "ADAPTER_HOME", Owner: "agent.provider.home"},
	}
	for _, tc := range []struct {
		name    string
		cred    core.PublishCredential
		wantErr bool
		// wantOwner is the site the refusal must name, so an operator is sent to
		// the line that already sets the variable.
		wantOwner string
	}{
		{name: "no publish credential", cred: core.PublishCredential{}},
		{name: "an unowned variable is the ordinary case", cred: core.PublishCredential{Env: "GH_TOKEN", Var: "PAT"}},
		{
			name:      "naming the adapter's credential variable",
			cred:      core.PublishCredential{Env: "ADAPTER_KEY", Var: "PAT"},
			wantErr:   true,
			wantOwner: "agent.provider.api_key",
		},
		{
			// Owned but not a credential: the directory a credential is resolved
			// *from* (codex-exec's CODEX_HOME) is the same defect.
			name:      "naming an adapter-owned non-credential",
			cred:      core.PublishCredential{Env: "ADAPTER_HOME", Var: "PAT"},
			wantErr:   true,
			wantOwner: "agent.provider.home",
		},
		{
			name: "case matters: a lookalike is a different variable",
			cred: core.PublishCredential{Env: "adapter_key", Var: "PAT"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckPublishEnv(tc.cred, reserved, sentinel)
			if got := errors.Is(err, sentinel); got != tc.wantErr {
				t.Fatalf("CheckPublishEnv = %v, want refusal=%v", err, tc.wantErr)
			}
			if tc.wantOwner != "" && !strings.Contains(err.Error(), tc.wantOwner) {
				t.Errorf("refusal does not name the owning site %q: %v", tc.wantOwner, err)
			}
		})
	}
}

// CheckOwnedEnv's refusal names the *owning* site, not a fixed one.
//
// It used to say "set it through its named agent.provider key, which is where it
// is validated", which became false advice the moment a second site owned a
// variable: an operator who followed it would move the publish credential into the
// surface the rule had just refused.
func TestCheckOwnedEnvNamesTheOwningSite(t *testing.T) {
	sentinel := errors.New("test: reserved")
	reserved := []ReservedEnv{{Name: "GH_TOKEN", Owner: "publish.env"}}

	err := CheckOwnedEnv(map[string]string{"GH_TOKEN": "second-source"}, nil, reserved, sentinel)
	if !errors.Is(err, sentinel) {
		t.Fatalf("CheckOwnedEnv = %v, want a refusal", err)
	}
	if !strings.Contains(err.Error(), "publish.env") {
		t.Errorf("refusal does not name publish.env: %v", err)
	}
	if strings.Contains(err.Error(), "agent.provider key") {
		t.Errorf("refusal sends the operator to the wrong site: %v", err)
	}
}

// scriptedSource answers with whatever the test says, under a descriptor the
// test names. The fields it can vary are the three MintPublish is responsible
// for reading: the value, the deadline, and the failure.
type scriptedSource struct {
	descriptor core.SourceDescriptor
	token      core.Token
	err        error
	// fresh counts FetchFresh calls; cached counts Fetch calls, so a test can
	// prove which surface the publisher reached for.
	fresh, cached int
}

func (s *scriptedSource) Descriptor() core.SourceDescriptor { return s.descriptor }

func (s *scriptedSource) Fetch(context.Context, core.Purpose) (core.Token, error) {
	s.cached++
	return s.token, s.err
}

func (s *scriptedSource) FetchFresh(context.Context, core.Purpose) (core.Token, error) {
	s.fresh++
	return s.token, s.err
}

func boundedDescriptor() core.SourceDescriptor {
	return core.SourceDescriptor{
		Kind: "octo_sts", Authority: "octo:https://octo.example#org#ben-publish",
		BindingKey: "octo:https://octo.example#org#ben-publish#/run/oidc", MinFreshTTL: 50 * time.Minute,
	}
}

func unboundedDescriptor() core.SourceDescriptor {
	return core.SourceDescriptor{Kind: "static", Authority: "env:GH_TOKEN", BindingKey: "env:GH_TOKEN"}
}

// MintPublish obtains the credential per attempt, and classifies the two things
// only the credential boundary can see (SPEC §5.2.8, §7.7).
func TestMintPublish(t *testing.T) {
	sentinel := errors.New("test: publish unresolvable")
	ctx := context.Background()

	t.Run("unconfigured is not an error", func(t *testing.T) {
		got, err := MintPublish(ctx, core.PublishBinding{}, 0, sentinel)
		if err != nil || got != (PublishValue{}) {
			t.Errorf("MintPublish = (%+v, %v), want the zero value and no error", got, err)
		}
	})
	t.Run("minted from the source, never from its cache", func(t *testing.T) {
		src := &scriptedSource{descriptor: unboundedDescriptor(), token: core.Token{Value: "ghp-token"}}
		got, err := MintPublish(ctx, core.PublishBinding{Env: "GH_TOKEN", Source: src}, 0, sentinel)
		if err != nil {
			t.Fatalf("MintPublish: %v", err)
		}
		if want := (PublishValue{Env: "GH_TOKEN", Value: "ghp-token"}); got != want {
			t.Errorf("MintPublish = %+v, want %+v", got, want)
		}
		if src.fresh != 1 || src.cached != 0 {
			t.Errorf("fetches = (fresh %d, cached %d), want exactly one fresh exchange", src.fresh, src.cached)
		}
	})
	// An empty value is a source defect, and the refusal is the boundary's: the
	// class is permanent, so the run parks rather than spending its budget.
	t.Run("an empty value is a permanent refusal", func(t *testing.T) {
		src := &scriptedSource{descriptor: unboundedDescriptor()}
		_, err := MintPublish(ctx, core.PublishBinding{Env: "GH_TOKEN", Source: src}, 0, sentinel)
		if !errors.Is(err, sentinel) || !errors.Is(err, core.ErrCredentialEmpty) {
			t.Fatalf("MintPublish = %v, want a refusal naming the empty credential", err)
		}
		if class, ok := core.CredentialFailure(err); !ok || class != core.CredentialPermanent {
			t.Errorf("class = (%v, %v), want permanent", class, ok)
		}
	})
	// TTL insufficiency is arithmetic, not weather — and it is classified here
	// because this is the only thing holding both the token and the timeout.
	t.Run("a deadline shorter than the attempt is permanent", func(t *testing.T) {
		src := &scriptedSource{
			descriptor: boundedDescriptor(),
			token:      core.Token{Value: "ghp-token", UsableUntil: time.Now().Add(20 * time.Minute)},
		}
		_, err := MintPublish(ctx, core.PublishBinding{Env: "GH_TOKEN", Source: src}, 30*time.Minute, sentinel)
		if !errors.Is(err, core.ErrCredentialTTL) {
			t.Fatalf("MintPublish = %v, want the TTL refusal", err)
		}
		if class, _ := core.CredentialFailure(err); class != core.CredentialPermanent {
			t.Errorf("class = %v, want permanent", class)
		}
	})
	t.Run("the deadline gate cannot be bypassed by duration overflow", func(t *testing.T) {
		src := &scriptedSource{
			descriptor: boundedDescriptor(),
			token:      core.Token{Value: "ghp-token", UsableUntil: time.Now().Add(50 * time.Minute)},
		}
		attempt := time.Duration(9223372036854) * time.Millisecond
		_, err := MintPublish(ctx, core.PublishBinding{Env: "GH_TOKEN", Source: src}, attempt, sentinel)
		if !errors.Is(err, core.ErrCredentialTTL) {
			t.Fatalf("MintPublish = %v, want the TTL refusal", err)
		}
	})
	t.Run("the gate passes with the margin exactly covered", func(t *testing.T) {
		src := &scriptedSource{
			descriptor: boundedDescriptor(),
			// A whisker past the boundary: the gate is `>=`, and the clock moves
			// between the token being built and the comparison.
			token: core.Token{Value: "ghp-token", UsableUntil: time.Now().Add(35*time.Minute + time.Second)},
		}
		if _, err := MintPublish(ctx, core.PublishBinding{Env: "GH_TOKEN", Source: src}, 30*time.Minute, sentinel); err != nil {
			t.Errorf("MintPublish = %v, want the gate satisfied by 30m + the %s margin", err, core.CredentialTTLMargin)
		}
	})
	// The gate is skipped for an unbounded source, which is what keeps `static`
	// and every legacy spelling valid however long the attempt is.
	t.Run("an unbounded source is not gated", func(t *testing.T) {
		src := &scriptedSource{descriptor: unboundedDescriptor(), token: core.Token{Value: "ghp-token"}}
		if _, err := MintPublish(ctx, core.PublishBinding{Env: "GH_TOKEN", Source: src}, time.Hour, sentinel); err != nil {
			t.Errorf("MintPublish = %v, want no gate for a source stating no deadline", err)
		}
	})
	// A source's own verdict rides through unchanged: re-labelling it here is
	// how a transient blip would become a park.
	t.Run("a source's class survives the boundary", func(t *testing.T) {
		src := &scriptedSource{
			descriptor: boundedDescriptor(),
			err: &core.CredentialError{
				Class: core.CredentialTransient, Authority: boundedDescriptor().Authority,
				Err: errors.New("the issuer answered 503"),
			},
		}
		_, err := MintPublish(ctx, core.PublishBinding{Env: "GH_TOKEN", Source: src}, 0, sentinel)
		if !errors.Is(err, sentinel) {
			t.Fatalf("MintPublish = %v, want the adapter's sentinel", err)
		}
		if class, ok := core.CredentialFailure(err); !ok || class != core.CredentialTransient {
			t.Errorf("class = (%v, %v), want transient", class, ok)
		}
	})
	t.Run("half-configured is a refusal", func(t *testing.T) {
		src := &scriptedSource{descriptor: unboundedDescriptor(), token: core.Token{Value: "x"}}
		for _, b := range []core.PublishBinding{{Env: "GH_TOKEN"}, {Source: src}} {
			if _, err := MintPublish(ctx, b, 0, sentinel); !errors.Is(err, sentinel) {
				t.Errorf("MintPublish(%+v) = %v, want a refusal", b, err)
			}
		}
	})
	// Every refusal names the authority, which is non-secret by construction,
	// and never the value.
	t.Run("no refusal echoes a value", func(t *testing.T) {
		src := &scriptedSource{
			descriptor: boundedDescriptor(),
			token:      core.Token{Value: "ghp-secret-value", UsableUntil: time.Now().Add(time.Minute)},
		}
		_, err := MintPublish(ctx, core.PublishBinding{Env: "GH_TOKEN", Source: src}, time.Hour, sentinel)
		if err == nil || strings.Contains(err.Error(), "ghp-secret-value") {
			t.Fatalf("refusal = %v, want one that names no value", err)
		}
		if !strings.Contains(err.Error(), boundedDescriptor().Authority) {
			t.Errorf("refusal = %v, want it to name the authority", err)
		}
	})
}

// PublishReference keeps only the child variable, which is the whole of what the
// §7.6 one-owner rules read.
func TestPublishReferenceCarriesTheNameAndNothingElse(t *testing.T) {
	src := &scriptedSource{descriptor: unboundedDescriptor(), token: core.Token{Value: "ghp-token"}}
	got := PublishReference(core.PublishBinding{Env: "GH_TOKEN", Source: src})
	if got.Env != "GH_TOKEN" || got.Var != "" {
		t.Errorf("PublishReference = %+v, want the child variable alone", got)
	}
	if src.fresh != 0 || src.cached != 0 {
		t.Error("PublishReference reached the source; §5.5 defers that to an attempt")
	}
}
