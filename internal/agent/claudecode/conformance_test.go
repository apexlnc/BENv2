package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/agenttest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// TestMain lets the test binary re-exec itself as the fake harness: process
// discipline (SPEC §7.5) and liveness (§7.4) are only real if they are tested
// against a real process.
func TestMain(m *testing.M) {
	agenttest.Main(m, fake{}, cohort, cleanupFakeSandboxBinary)
}

// The AgentRunner contract, asserted by the suite every v1 adapter runs
// (BUILD B06, B07). What is claude-specific lives below it.
func TestConformance(t *testing.T) { agenttest.Run(t, contract()) }

func contract() agenttest.Contract {
	return agenttest.Contract{
		Name: KindName,
		Kind: Kind{},
		Block: func(binary string, env map[string]string) map[string]any {
			block := map[string]any{
				"binary": binary,
				// A headless daemon states its posture; there is no default
				// (see ErrPermissionMode).
				"permission_mode": "bypassPermissions",
			}
			if len(env) > 0 {
				vals := make(map[string]any, len(env))
				for k, v := range env {
					vals[k] = v
				}
				block["env"] = vals
			}
			return block
		},
		New: func(t *testing.T, block map[string]any, o agenttest.Options) core.AgentRunner {
			t.Helper()
			r, err := New(Options{
				Provider:       block,
				Publish:        o.Publish,
				AttemptTimeout: o.AttemptTimeout,
				Transcripts:    o.Transcripts,
				Timings:        o.Timings,
				OnRun:          o.OnRun,
				signal:         o.Signal,
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			return r
		},
		Credentials: []agenttest.Credential{
			{Key: "api_key", Env: "ANTHROPIC_API_KEY"},
			{Key: "auth_token", Env: "ANTHROPIC_AUTH_TOKEN"},
		},
		// The isolated posture is the default, so every conformance run injects
		// both (#114). What they must *contain* is asserted in
		// TestIsolatedConfigDirIsUnderThePrivateDir, off the environment the fake
		// harness received — this list only tells the audit they are not the
		// daemon's.
		OwnedDirs: []string{"CLAUDE_CONFIG_DIR", "TMPDIR"},
		Posture:   agenttest.Posture{Key: "permission_mode", Err: ErrPermissionMode},
		Fake:      fake{},
		Errors: agenttest.Errors{
			ProviderKey:   ErrProviderKey,
			Binary:        ErrBinary,
			PromptEmpty:   ErrPromptEmpty,
			WorkspacePath: ErrWorkspacePath,
			EnvNamespace:  ErrEnvNamespace,
			ProviderValue: ErrProviderValue,

			EnvReserved:       ErrEnvReserved,
			PublishCredential: ErrPublishCredential,
		},
	}
}

// --- the fake harness's claude-shaped half ---

// sessionID is what the fake's init line announces; the adapter mints the
// continuation token from it (SPEC §7.1).
const sessionID = "fake-session-1"

// fake writes `claude -p --output-format stream-json` lines and answers the two
// readiness probes. The shapes are recorded from 2.1.221 (see testdata), not
// invented: the auth answer in particular is a complete JSON body alongside a
// non-zero exit, which is the case checkAuth exists to read correctly.
type fake struct{}

// isHarnessInvocation distinguishes "run as the fake claude" from "run the
// tests". `go test` passes none of these; the adapter passes -p for every run,
// and --version / `auth status` for the readiness probes.
func (fake) IsInvocation(args []string) bool {
	if len(args) > 0 && args[0] == "auth" {
		return true
	}
	if isSandboxInvocation(args) {
		return true
	}
	for _, a := range args {
		if a == "-p" || a == "--version" {
			return true
		}
	}
	return false
}

// The pinned answers are transcribed from 2.1.221 on a host with managed
// settings (#112), not composed to suit the check — a fake that invents a
// guarantee the harness does not make is worse than a missing test (AGENTS.md).
// Every one reports loggedIn: true, which is the whole problem: a pinned host
// announces a usable credential and then refuses every dispatch with it.
//
// Two of them are what keep the check from being written the easy way. The api
// key answer reads authMethod "claude.ai", identical to a working unpinned
// login, so a method comparison alone passes it; the auth token answer reads
// "oauth_token" with **no** apiKeySource at all, so a source comparison alone
// misses it. Reusing one fixture for both — as the first draft of this file did
// — hides exactly that.
const (
	authPinnedNoCredential = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty",` +
		`"forcedLoginMethod":"claudeai"}`
	authPinnedAPIKey = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty",` +
		`"forcedLoginMethod":"claudeai","apiKeySource":"ANTHROPIC_API_KEY"}`
	// Also, byte for byte, what a CLAUDE_CODE_OAUTH_TOKEN session reports, and
	// what an Anthropic profile reports — neither of which the pin blocks. That
	// this one string has to serve both a refusal and a pass is the point: the
	// answer does not name a source, so only the block can say which it is.
	authPinnedAuthToken = `{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty",` +
		`"forcedLoginMethod":"claudeai"}`
	// A bearer-token session with an API key also in play. Two configurations
	// produce this exact answer on 2.1.221 — ANTHROPIC_AUTH_TOKEN with
	// ANTHROPIC_API_KEY, and CLAUDE_CODE_OAUTH_TOKEN with ANTHROPIC_API_KEY — and
	// both are refused at dispatch, but they are refused *for different fields*.
	// That is what makes the two readings of it a real test rather than a
	// hypothetical: `authMethod` reports only that a bearer token is present, so
	// which field to name is decided by what the block supplies.
	authPinnedOAuthWithAPIKey = `{"loggedIn":true,"authMethod":"oauth_token","apiProvider":"firstParty",` +
		`"forcedLoginMethod":"claudeai","apiKeySource":"ANTHROPIC_API_KEY"}`
	authPinnedHelper = `{"loggedIn":true,"authMethod":"api_key_helper","apiProvider":"firstParty",` +
		`"forcedLoginMethod":"claudeai","apiKeySource":"apiKeyHelper"}`
	authPinnedLoggedOut = `{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty",` +
		`"forcedLoginMethod":"claudeai"}`

	// A Console login under a console pin: a login credential that reports an
	// apiKeySource, and which the pin is satisfied by rather than blocking. Both
	// values are #126's review's measurements — this host carries a claudeai pin
	// and cannot produce a Console session — and the authMethod is "claude.ai",
	// not "console", which is the kind of guess the fake-fidelity rule exists to
	// keep out of this file.
	authConsoleLogin = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty",` +
		`"forcedLoginMethod":"console","apiKeySource":"/login managed key"}`
	// A third-party provider session. Cloud provider and gateway credentials
	// outrank both environment variables and are documented as not blocked by a
	// pin. authMethod "third_party" is #126's review's direct measurement,
	// correcting the "api_key" this fixture invented; the rest of the row matched.
	authThirdPartyProvider = `{"loggedIn":true,"authMethod":"third_party","apiProvider":"bedrock",` +
		`"forcedLoginMethod":"claudeai","apiKeySource":"ANTHROPIC_API_KEY"}`
)

func (fake) Probe(args []string) bool {
	// Before the harness shapes: an `srt` invocation carries the harness argv
	// inside it, so a check that looked for `--version` first would answer as the
	// harness for a wrapper invocation.
	if isSandboxInvocation(args) {
		runFakeSandbox(args)
	}
	if len(args) >= 2 && args[0] == "auth" && args[1] == "status" {
		switch os.Getenv(agenttest.AuthEnv) {
		case "logged-out":
			// Mirrors 2.1.221: a complete JSON answer on stdout *and* a non-zero
			// exit. An adapter that read the exit code instead of the body would
			// pass validation here.
			fmt.Println(`{"loggedIn":false,"authMethod":"none","apiProvider":"firstParty"}`)
			os.Exit(1)
		case "no-subcommand":
			// An older build: usage on stderr, nothing parseable on stdout.
			fmt.Fprintln(os.Stderr, "error: unknown command 'auth'")
			os.Exit(1)
		case "pinned":
			// What the host reports when the block supplies no credential and the
			// daemon account's own login satisfies the pin: a working run.
			fmt.Println(authPinnedNoCredential)
			os.Exit(0)
		case "pinned-api-key":
			fmt.Println(authPinnedAPIKey)
			os.Exit(0)
		case "pinned-auth-token":
			fmt.Println(authPinnedAuthToken)
			os.Exit(0)
		case "pinned-oauth-with-api-key":
			fmt.Println(authPinnedOAuthWithAPIKey)
			os.Exit(0)
		case "console-login":
			fmt.Println(authConsoleLogin)
			os.Exit(0)
		case "third-party-provider":
			fmt.Println(authThirdPartyProvider)
			os.Exit(0)
		case "pinned-helper":
			fmt.Println(authPinnedHelper)
			os.Exit(0)
		case "pinned-logged-out":
			fmt.Println(authPinnedLoggedOut)
			os.Exit(1)
		default:
			fmt.Println(`{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty"}`)
			os.Exit(0)
		}
	}
	for _, a := range args {
		if a == "--version" {
			fmt.Println("9.9.9 (Claude Code)")
			// Leaves a child holding stdout when the suite asks for it: the
			// probe's pipes then stay open long after its process is gone.
			agenttest.LeakPipeHolder()
			os.Exit(0)
		}
	}
	return false
}

func (fake) SessionID() string { return sessionID }

// Usage is what the result line's numbers normalize to: cache reads and writes
// are real input tokens the run was billed for, so they count (SPEC §7.2).
func (fake) Usage() core.Usage {
	return core.Usage{InputTokens: 15, OutputTokens: 7, CostUSD: 0.25}
}

func (fake) Init(w io.Writer) {
	fmt.Fprintf(w, `{"type":"system","subtype":"init","session_id":%q,"model":"fake"}`+"\n", sessionID)
}

// Private is a thinking-only assistant line: activity with no normalized
// meaning, which the transcript keeps and the event stream drops.
func (fake) Private(w io.Writer) {
	fmt.Fprintf(w, `{"type":"assistant","session_id":%q,"message":{"content":[{"type":"thinking","thinking":"private"}]}}`+"\n", sessionID)
}

func (fake) Text(w io.Writer, text string) {
	b, _ := json.Marshal(map[string]any{
		"type":       "assistant",
		"session_id": sessionID,
		"message":    map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}},
	})
	fmt.Fprintln(w, string(b))
}

func (fake) Success(w io.Writer) {
	fmt.Fprintf(w, `{"type":"result","subtype":"success","is_error":false,"session_id":%q,`+
		`"total_cost_usd":0.25,"usage":{"input_tokens":10,"cache_read_input_tokens":5,"output_tokens":7}}`+"\n", sessionID)
}

// --- claude-specific behaviour ---

// Both capabilities are real for this harness: `--resume` takes the session id
// the init line minted, and the result line carries token counts and a cost.
func TestCapabilities(t *testing.T) {
	parallel(t)
	r, err := New(Options{Provider: map[string]any{"permission_mode": "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Capabilities(); !got.Resume || !got.Usage {
		t.Errorf("Capabilities = %+v, want resume and usage", got)
	}
}

// SPEC §7.1's "auth plausible". The body is the signal, not the exit status:
// 2.1.221 exits 1 when logged out while still printing a complete answer, so an
// adapter that read the exit code would let a logged-out harness through — and
// then fail every dispatch.
func TestReadyReadsAuthAnswerNotExitCode(t *testing.T) {
	parallelGroup(t)
	for _, tc := range []struct {
		name    string
		auth    string
		wantErr bool
	}{
		{name: "logged in", auth: "", wantErr: false},
		{name: "logged out, non-zero exit, JSON body", auth: "logged-out", wantErr: true},
		// An older build without the subcommand must stay usable: --version has
		// already established identity, so an unparseable answer is not a refusal.
		{name: "subcommand absent", auth: "no-subcommand", wantErr: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parallel(t)
			r := testRunner(t, map[string]string{agenttest.AuthEnv: tc.auth})
			err := r.Ready(context.Background())
			if tc.wantErr && !errors.Is(err, ErrBinary) {
				t.Errorf("Ready = %v, want ErrBinary", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Ready = %v, want ok", err)
			}
		})
	}
}

// SPEC §7.1's readiness has three answers on a host that pins a login method,
// not two, and the middle one is the whole of #112: the harness reports itself
// logged in and refuses every dispatch anyway, so before this check Ready was
// green and each attempt burned a workspace rediscovering the refusal.
//
// The row that keeps the refusal honest is the first: a pinned host whose block
// supplies no credential must stay usable, because on such a host that is the
// only configuration that runs. A check written against the pin alone passes
// every other row here and fails that one.
func TestReadySeparatesRefusedCredentialFromAbsentOne(t *testing.T) {
	parallelGroup(t)
	const credential = "sk-ant-test-credential-value"
	for _, tc := range []struct {
		name  string
		auth  string
		block map[string]any
		want  error
	}{
		{name: "pinned, harness authenticates itself", auth: "pinned"},
		{
			name:  "pinned, block supplies api_key",
			auth:  "pinned-api-key",
			block: map[string]any{"api_key": credential},
			want:  ErrCredentialPinned,
		},
		{
			// Its own recorded answer, which reports no apiKeySource at all: a
			// check reading that field alone lets this one through.
			name:  "pinned, block supplies auth_token",
			auth:  "pinned-auth-token",
			block: map[string]any{"auth_token": credential},
			want:  ErrCredentialPinned,
		},
		{
			// The same answer as the row above, and this block supplies no
			// auth_token — so the bearer token in use is a CLAUDE_CODE_OAUTH_TOKEN,
			// an Anthropic profile, or a federation credential, none of which the
			// pin blocks. Reading `oauth_token` as a source refuses all three.
			name: "oauth_token session this block did not supply", auth: "pinned-auth-token",
		},
		{
			// A login the pin selects rather than blocks. It reports an
			// apiKeySource, so a non-emptiness test refuses a working host.
			name: "console login under a console pin", auth: "console-login",
		},
		{
			// A cloud-provider session outranks both environment variables and is
			// not subject to the pin, so the configured api_key is not what the
			// run authenticates with — refusing on the block's contents would
			// refuse a working host on the strength of an unused field.
			name:  "third-party provider, api_key configured",
			auth:  "third-party-provider",
			block: map[string]any{"api_key": credential},
		},
		{
			// No provider key to anchor to: an apiKeyHelper in the config dir's
			// own settings.json is still a credential the pin will refuse.
			name: "pinned, credential from outside the block",
			auth: "pinned-helper",
			want: ErrCredentialPinned,
		},
		{
			// Logged out is logged out, pinned or not — ErrBinary, and never the
			// pin's error: the operator has no credential to remove.
			name: "pinned and logged out", auth: "pinned-logged-out", want: ErrBinary,
		},
		{name: "unpinned and logged out", auth: "logged-out", want: ErrBinary},
		{name: "unpinned and logged in", auth: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parallel(t)
			err := testRunnerWith(t, map[string]string{agenttest.AuthEnv: tc.auth}, tc.block).
				Ready(context.Background())
			switch {
			case tc.want == nil && err != nil:
				t.Fatalf("Ready = %v, want ok", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Fatalf("Ready = %v, want %v", err, tc.want)
			}
			// The two refusals must stay distinguishable in both directions: an
			// operator acts on which one they got.
			if tc.want == ErrBinary && errors.Is(err, ErrCredentialPinned) {
				t.Errorf("Ready = %v, want an absent-credential refusal, not the pin's", err)
			}
			if tc.want == ErrCredentialPinned && errors.Is(err, ErrBinary) {
				t.Errorf("Ready = %v, want the pin's refusal, not an absent-credential one", err)
			}
		})
	}
}

// Both logged-out refusals are ErrBinary, so the sentinel cannot carry the
// difference and the message has to: on a pinned host "set agent.provider.api_key"
// is not incomplete advice but the cause of the next failure, and the operator
// needs the pin named to know that.
func TestLoggedOutAdviceFollowsThePin(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name, auth string
		mentions   []string
		omits      string
	}{
		{
			name: "unpinned", auth: "logged-out",
			mentions: []string{"agent.provider.api_key"}, omits: "claudeai",
		},
		{
			name: "pinned", auth: "pinned-logged-out",
			mentions: []string{"claudeai", "claude auth login"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := testRunner(t, map[string]string{agenttest.AuthEnv: tc.auth}).Ready(context.Background())
			if !errors.Is(err, ErrBinary) {
				t.Fatalf("Ready = %v, want ErrBinary", err)
			}
			for _, want := range tc.mentions {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Ready = %v, want it to mention %q", err, want)
				}
			}
			if tc.omits != "" && strings.Contains(err.Error(), tc.omits) {
				t.Errorf("Ready = %v, want no mention of %q", err, tc.omits)
			}
		})
	}
}

// Every credential this adapter can inject is refused by a pin, driven from the
// conformance contract's list rather than from credentialKeys — the declaration
// under test cannot also be the thing that decides what to test (AGENTS.md).
// Dropping an entry from credentialKeys leaves it injected but no longer
// recognised as supplied, and this is what notices.
func TestPinRefusesEveryInjectedCredential(t *testing.T) {
	parallelGroup(t)
	const credential = "sk-ant-test-credential-value"
	// Each credential is probed against the answer the harness gives when *that*
	// route is the one in use, since the two answers differ in which field even
	// reports the source. Keyed by the contract's env var, so a credential added
	// there without a recorded answer fails here rather than reusing another's.
	answers := map[string]string{
		"ANTHROPIC_API_KEY":    "pinned-api-key",
		"ANTHROPIC_AUTH_TOKEN": "pinned-auth-token",
	}
	for _, c := range contract().Credentials {
		t.Run(c.Key, func(t *testing.T) {
			parallel(t)
			auth, ok := answers[c.Env]
			if !ok {
				t.Fatalf("no recorded `auth status` answer for %s; record one from the real "+
					"harness rather than reusing another credential's", c.Env)
			}
			r := testRunnerWith(t, map[string]string{agenttest.AuthEnv: auth},
				map[string]any{c.Key: credential})
			err := r.Ready(context.Background())
			if !errors.Is(err, ErrCredentialPinned) {
				t.Fatalf("Ready = %v, want ErrCredentialPinned", err)
			}
			// Anchored at the key that supplied it, with the value as data so
			// `config effective` redacts by provenance (SPEC §5.8, #55) — and
			// absent from the message, which is #52 boundary 2's rule.
			var refusal *core.ConfigValueError
			if !errors.As(err, &refusal) {
				t.Fatalf("Ready = %v, want a core.ConfigValueError", err)
			}
			if want := "agent.provider." + c.Key; refusal.Field != want {
				t.Errorf("Field = %q, want %q", refusal.Field, want)
			}
			if refusal.Value != credential {
				t.Errorf("Value = %q, want the credential carried as data", refusal.Value)
			}
			if strings.Contains(err.Error(), credential) {
				t.Errorf("Error() names the credential: %v", err)
			}
		})
	}
}

// With both credentials configured the harness authenticates with the bearer
// token, so that is the field the refusal must name. Naming the other one is not
// a cosmetic slip: the operator removes `api_key`, the run still authenticates
// with `auth_token`, and the pin refuses it again — advice that costs a cycle
// and reads as the check being wrong.
func TestPinNamesTheEffectiveCredentialNotTheFirstConfigured(t *testing.T) {
	parallel(t)
	const apiKey, authToken = "sk-ant-test-api-key", "sk-ant-test-auth-token"
	err := testRunnerWith(t,
		map[string]string{agenttest.AuthEnv: "pinned-oauth-with-api-key"},
		map[string]any{"api_key": apiKey, "auth_token": authToken}).
		Ready(context.Background())

	if !errors.Is(err, ErrCredentialPinned) {
		t.Fatalf("Ready = %v, want ErrCredentialPinned", err)
	}
	var refusal *core.ConfigValueError
	if !errors.As(err, &refusal) {
		t.Fatalf("Ready = %v, want a core.ConfigValueError", err)
	}
	if refusal.Field != "agent.provider.auth_token" {
		t.Errorf("Field = %q, want agent.provider.auth_token — the credential the harness "+
			"selects by its own precedence, not the first one this adapter's table lists",
			refusal.Field)
	}
	if refusal.Value != authToken {
		t.Errorf("Value = %q, want the auth token carried as data", refusal.Value)
	}
}

// An API key behind a bearer-token session the block did not supply. Measured on
// 2.1.221 with CLAUDE_CODE_OAUTH_TOKEN and ANTHROPIC_API_KEY set together:
//
//	{"authMethod":"oauth_token", …, "apiKeySource":"ANTHROPIC_API_KEY"}
//	→ dispatch exits with the managed-pin refusal
//
// `CLAUDE_CODE_OAUTH_TOKEN` is not adapter-owned, so an operator reaches it
// through the generic `env` surface — which is how the two arrive together here.
// The verdict must be the pin's, anchored at `api_key`: the bearer token is not
// this block's to remove, and the key is.
//
// This is the row that makes blockedSource's oauth_token branch fall through
// rather than return. Without it that fall-through survives mutation (#126
// ledger), and an early return there passes this configuration at startup and
// fails every attempt — the exact bug the whole check exists for.
func TestPinCatchesAnAPIKeyBehindAnOAuthTokenSession(t *testing.T) {
	parallel(t)
	const apiKey = "sk-ant-test-api-key"
	err := testRunnerWith(t, map[string]string{
		agenttest.AuthEnv:         "pinned-oauth-with-api-key",
		"CLAUDE_CODE_OAUTH_TOKEN": "bogus-oauth-token",
	}, map[string]any{"api_key": apiKey}).Ready(context.Background())

	if !errors.Is(err, ErrCredentialPinned) {
		t.Fatalf("Ready = %v, want ErrCredentialPinned", err)
	}
	var refusal *core.ConfigValueError
	if !errors.As(err, &refusal) {
		t.Fatalf("Ready = %v, want a core.ConfigValueError", err)
	}
	if refusal.Field != "agent.provider.api_key" {
		t.Errorf("Field = %q, want agent.provider.api_key — the bearer token came from "+
			"outside this block, so the key is the credential the operator can act on",
			refusal.Field)
	}
	if refusal.Value != apiKey {
		t.Errorf("Value = %q, want the api key carried as data", refusal.Value)
	}
}

// Readiness is exactly two probes for this harness — identity then credential —
// and the suite's audit of their environments is only as good as the count.
func TestReadyProbesVersionThenAuth(t *testing.T) {
	parallel(t)
	dumpPath := filepath.Join(t.TempDir(), "probe.json")
	if err := testRunner(t, map[string]string{agenttest.DumpEnv: dumpPath}).Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	argvs := probeArgs(t, dumpPath)
	want := [][]string{{"--version"}, {"auth", "status"}}
	if len(argvs) != len(want) {
		t.Fatalf("probe invocations = %v, want %v", argvs, want)
	}
	for i, w := range want {
		if len(argvs[i]) != len(w) || (len(w) > 0 && argvs[i][0] != w[0]) {
			t.Errorf("probe %d = %v, want %v", i, argvs[i], w)
		}
	}
}

// testRunner builds a runner whose harness is this test binary, with the fake's
// controls travelling in provider.env.
func testRunner(t *testing.T, env map[string]string) *Runner {
	t.Helper()
	return testRunnerWith(t, env, nil)
}

// testRunnerWith is testRunner with extra provider keys layered on, for the
// cases where what Ready decides depends on the block and not only on what the
// harness answers.
func testRunnerWith(t *testing.T, env map[string]string, extra map[string]any) *Runner {
	t.Helper()
	block := contract().Block(selfPath(t), env)
	maps.Copy(block, extra)
	r, err := New(Options{Provider: block})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

func selfPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// probeArgs reads the fake's dump and returns each invocation's arguments.
func probeArgs(t *testing.T, path string) [][]string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("harness never wrote its dump: %v", err)
	}
	var out [][]string
	dec := json.NewDecoder(bytes.NewReader(b))
	for {
		var d struct {
			Argv []string `json:"argv"`
		}
		if err := dec.Decode(&d); err != nil {
			break
		}
		out = append(out, d.Argv[1:])
	}
	return out
}
