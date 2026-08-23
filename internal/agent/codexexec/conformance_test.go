package codexexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/agenttest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// TestMain lets the test binary re-exec itself as the fake harness: process
// discipline (SPEC §7.5) and liveness (§7.4) are only real if they are tested
// against a real process.
func TestMain(m *testing.M) { agenttest.Main(m, fake{}, cohort) }

// The B06 conformance suite, run unmodified — the ticket's acceptance criterion
// and the whole claim behind "agent-agnostic" (BUILD B07). What is
// codex-specific lives below it.
func TestConformance(t *testing.T) { agenttest.Run(t, contract()) }

func contract() agenttest.Contract {
	return agenttest.Contract{
		Name: KindName,
		Kind: Kind{},
		Block: func(binary string, env map[string]string) map[string]any {
			block := map[string]any{
				"binary": binary,
				// A headless daemon states its posture; there is no default
				// (see ErrSandboxMode).
				"sandbox_mode": "workspace-write",
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
			{Key: "api_key", Env: "CODEX_API_KEY"},
		},
		Posture: agenttest.Posture{Key: "sandbox_mode", Err: ErrSandboxMode},
		Fake:    fake{},
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

// --- the fake harness's codex-shaped half ---

// threadID is what the fake's thread.started line announces; the adapter mints
// the continuation token from it (SPEC §7.1). A UUID because that is what the
// real CLI emits and takes back.
const threadID = "019fe267-3027-73b2-95fc-09a5467477db"

// fake writes `codex exec --json` lines and answers the two readiness probes.
// The shapes are recorded from 0.147.0 (see testdata), not invented.
type fake struct{}

// IsInvocation distinguishes "run as the fake codex" from "run the tests".
// `go test` passes none of these; the adapter passes `exec` for every run, and
// --version / `login status` for the readiness probes.
func (fake) IsInvocation(args []string) bool {
	if len(args) > 0 && (args[0] == "exec" || args[0] == "login") {
		return true
	}
	return slices.Contains(args, "--version")
}

func (fake) Probe(args []string) bool {
	if len(args) >= 2 && args[0] == "login" && args[1] == "status" {
		// Every answer goes to **stderr** and stdout stays empty, logged in or
		// out, exactly as 0.147.0 behaves — the measurement that makes the exit
		// status the only signal. A fixture that printed to stdout would let a
		// body-reading probe pass here and accept everything in production.
		switch os.Getenv(agenttest.AuthEnv) {
		case "logged-out":
			fmt.Fprintln(os.Stderr, "Not logged in")
			os.Exit(1)
		case "bad-home":
			// An unusable CODEX_HOME: `--version` warns and exits 0, so this is
			// the only local probe that catches it.
			fmt.Fprintln(os.Stderr, `Error loading configuration: CODEX_HOME points to "/nope", but that path does not exist`)
			os.Exit(1)
		case "unrecognized":
			// A future release rewording its refusal. Failing closed is the
			// deliberate direction (see checkAuth).
			fmt.Fprintln(os.Stderr, "Session: some future phrasing nobody has parsed")
			os.Exit(1)
		default:
			fmt.Fprintln(os.Stderr, "Logged in using an API key - sk-proj-***AAAA")
			os.Exit(0)
		}
	}
	if slices.Contains(args, "--version") {
		fmt.Println("codex-cli 9.9.9")
		// Leaves a child holding stdout when the suite asks for it: the probe's
		// pipes then stay open long after its process is gone.
		agenttest.LeakPipeHolder()
		os.Exit(0)
	}
	return false
}

func (fake) SessionID() string { return threadID }

// Usage is what the turn.completed numbers normalize to: input_tokens is the
// whole prompt (cached and cache-write are subsets), and no cost is reported.
func (fake) Usage() core.Usage {
	return core.Usage{InputTokens: 15, OutputTokens: 7}
}

func (fake) Init(w io.Writer) {
	fmt.Fprintf(w, `{"type":"thread.started","thread_id":%q}`+"\n", threadID)
}

// Private is the turn boundary: activity with no normalized meaning, which the
// transcript keeps and the event stream drops.
func (fake) Private(w io.Writer) {
	fmt.Fprintln(w, `{"type":"turn.started"}`)
}

func (fake) Text(w io.Writer, text string) {
	b, _ := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"id": "item_0", "type": "agent_message", "text": text},
	})
	fmt.Fprintln(w, string(b))
}

func (fake) Success(w io.Writer) {
	fmt.Fprintln(w, `{"type":"turn.completed","usage":{"input_tokens":15,"cached_input_tokens":5,`+
		`"cache_write_input_tokens":10,"output_tokens":7,"reasoning_output_tokens":0}}`)
}

// --- codex-specific behaviour ---

// Both capabilities are real for this harness: `resume` takes the thread id the
// started line minted, and turn.completed carries token counts. Cost is the one
// thing usage cannot report, which core.Usage models as 0 rather than as a
// missing capability.
func TestCapabilities(t *testing.T) {
	parallel(t)
	r, err := New(Options{Provider: map[string]any{"sandbox_mode": "workspace-write"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Capabilities(); !got.Resume || !got.Usage {
		t.Errorf("Capabilities = %+v, want resume and usage", got)
	}
}

// SPEC §7.1's "auth plausible" — plus "the harness can load its configuration",
// which for this binary is the same probe.
//
// Two measured facts drive the table. `codex login status` answers on stderr
// and writes nothing to stdout, so the **exit status** is the signal (the
// opposite of the claude-code adapter, whose harness prints a full body
// alongside a non-zero exit). And `codex --version` exits 0 with an unusable
// CODEX_HOME, so this is the only local check that catches one — which is why
// it runs even when an api_key makes the stored login irrelevant.
func TestReadyClassifiesTheLoginProbe(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name    string
		apiKey  string
		auth    string
		wantErr bool
	}{
		{name: "stored login present", auth: "", wantErr: false},
		{name: "no credential anywhere", auth: "logged-out", wantErr: true},
		{
			// The measured asymmetry: the key authenticates runs, the probe
			// denies a stored login, and readiness must believe the key.
			name:   "api_key configured, no stored login",
			apiKey: "sk-provider-key",
			auth:   "logged-out",
		},
		{
			// An api_key excuses exactly one failure. A home the harness cannot
			// load fails every run, key or no key.
			name:    "api_key configured, unusable codex home",
			apiKey:  "sk-provider-key",
			auth:    "bad-home",
			wantErr: true,
		},
		{name: "unusable codex home, no key", auth: "bad-home", wantErr: true},
		{
			// Fail closed on a refusal this adapter cannot read: an unrecognized
			// non-zero exit is not evidence of health, and the costs are
			// asymmetric (one loud startup refusal vs. a burned workspace per
			// dispatch).
			name:    "refusal nobody has parsed",
			apiKey:  "sk-provider-key",
			auth:    "unrecognized",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := contract().Block(selfPath(t), map[string]string{agenttest.AuthEnv: tc.auth})
			if tc.apiKey != "" {
				block["api_key"] = tc.apiKey
			}
			r, err := New(Options{Provider: block})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			err = r.Ready(context.Background())
			if tc.wantErr && !errors.Is(err, ErrBinary) {
				t.Errorf("Ready = %v, want ErrBinary", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Ready = %v, want ok", err)
			}
		})
	}
}

// Readiness is exactly two probes — identity, then configuration-and-credential
// — and the second runs unconditionally. An api_key changes how its failure is
// *read*, never whether it is asked (see checkAuth).
func TestReadyProbeInvocations(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name   string
		apiKey string
	}{
		{name: "no key"},
		{name: "api key configured", apiKey: "sk-provider-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dumpPath := filepath.Join(t.TempDir(), "probe.json")
			block := contract().Block(selfPath(t), map[string]string{agenttest.DumpEnv: dumpPath})
			if tc.apiKey != "" {
				block["api_key"] = tc.apiKey
			}
			r, err := New(Options{Provider: block})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if err := r.Ready(context.Background()); err != nil {
				t.Fatalf("Ready: %v", err)
			}
			want := [][]string{{"--version"}, {"login", "status"}}
			if got := probeArgs(t, dumpPath); !equalArgs(got, want) {
				t.Errorf("probe invocations = %v, want %v", got, want)
			}
		})
	}
}

// A logged-in `codex login status` prints account identity, which must never be
// copied into an error or a log line. The failure text is a configuration
// diagnostic and is quoted on purpose.
func TestReadyRefusalQuotesTheDiagnosticNotTheIdentity(t *testing.T) {
	parallel(t)
	block := contract().Block(selfPath(t), map[string]string{agenttest.AuthEnv: "bad-home"})
	r, err := New(Options{Provider: block})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	err = r.Ready(context.Background())
	if !errors.Is(err, ErrBinary) {
		t.Fatalf("Ready = %v, want ErrBinary", err)
	}
	if !strings.Contains(err.Error(), "Error loading configuration") {
		t.Errorf("Ready = %q, want the harness's diagnostic", err)
	}
	if strings.Contains(err.Error(), "sk-proj") {
		t.Errorf("Ready leaked account identity: %q", err)
	}
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

func equalArgs(got, want [][]string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if !slices.Equal(got[i], want[i]) {
			return false
		}
	}
	return true
}
