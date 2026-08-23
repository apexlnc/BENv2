// Package agenttest is the AgentRunner conformance suite: one shared,
// table-driven statement of what SPEC §7.1–§7.6 requires of *any* adapter,
// which every v1 harness adapter runs unmodified (BUILD B06, B07).
//
// The point of a shared suite is that the second adapter cannot quietly ask for
// a weaker contract than the first. "Agent-agnostic" is a claim about behaviour
// — a stall window that closes, a Stop that reports honestly, an environment
// that carries no daemon secret — and a claim only two private test suites make
// separately is a claim about two implementations that happen to agree today.
// So the assertions live here once, and an adapter supplies only what genuinely
// differs between harnesses:
//
//   - a Fake that writes its harness's native stream lines and answers its
//     readiness probes, so the suite's scripts drive a real process;
//   - a Block that builds a minimal valid agent.provider block;
//   - a New that constructs the runner, which lives in the adapter's own
//     package and is therefore free to reach the test-only hooks;
//   - the adapter's named refusals, since AGENTS.md wants tests asserting on
//     ErrX values rather than on message text.
//
// What deliberately stays in an adapter's own tests: provider-key parsing, argv
// shape, credential-variable naming, and the quirks of its readiness probes —
// everything whose correctness is a statement about one harness rather than
// about the contract.
package agenttest

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/credential"
)

// EnvSource is the publish credential source the suite binds by default: one
// daemon environment variable, read per attempt, which is exactly what
// `publish.value: $VAR` compiles into (SPEC §5.5, amendment 3).
//
// Nil for an empty name, so "no publish credential configured" stays the zero
// binding rather than a source that refuses.
func EnvSource(variable string) core.FreshSource {
	if variable == "" {
		return nil
	}
	return credential.NewEnv(variable)
}

// ScriptedSource is a credential source under a test's control: it answers with
// whatever the function returns, under a descriptor the test names.
//
// Here rather than in each adapter's package because both adapters need the
// same three shapes — a source that fails, one that answers empty, one whose
// deadline is too short — and three private copies would be three chances to
// build a fake that promises something no real source does.
type ScriptedSource struct {
	Descriptor_ core.SourceDescriptor
	Fetch_      func() (core.Token, error)
	// Fresh counts FetchFresh calls, so a test can prove the publisher was never
	// served from a cache.
	Fresh int
}

func (s *ScriptedSource) Descriptor() core.SourceDescriptor { return s.Descriptor_ }

func (s *ScriptedSource) Fetch(ctx context.Context, p core.Purpose) (core.Token, error) {
	return s.FetchFresh(ctx, p)
}

func (s *ScriptedSource) FetchFresh(context.Context, core.Purpose) (core.Token, error) {
	s.Fresh++
	return s.Fetch_()
}

// Contract is everything the suite needs to exercise one adapter.
type Contract struct {
	// Name is the agent.kind, for test output.
	Name string
	// Kind is the package-level registration under test (SPEC §5.7, §7.1).
	Kind core.RunnerKind

	// Block builds a *minimal valid* provider block: the given binary, whatever
	// keys the adapter requires, and exactly the given child-environment
	// entries under its `env` key. The suite adds keys of its own to the result,
	// so it must be a fresh map every call.
	Block func(binary string, env map[string]string) map[string]any

	// New constructs a runner from a block plus the suite's hooks. It lives in
	// the adapter's package, which is what lets the suite reach test-only
	// options without the adapter exporting them.
	New func(t *testing.T, block map[string]any, opts Options) core.AgentRunner

	// Credentials names every provider key carrying a credential and the child
	// environment variable each lands in (SPEC §7.6). The audit asserts each
	// value reaches its variable and never reaches argv.
	//
	// A list, not one key: claude-code declares two (api_key, auth_token) and a
	// singular field silently exercised the first while reading as coverage of
	// the surface. Adding a key to an adapter's table and not to this list now
	// under-covers visibly — the sensitivity, argv, namespace and transcript
	// tests all iterate it.
	Credentials []Credential

	// OwnedDirs names child environment variables this adapter injects from its
	// own configuration that are *not* credentials — a harness config or scratch
	// directory (claude-code's CLAUDE_CONFIG_DIR and TMPDIR, codex-exec's
	// CODEX_HOME). The child-environment audit needs them, because its rule is
	// "nothing arrives just because the daemon had it" and these arrive because
	// the adapter put them there.
	//
	// Names only, and deliberately not what an adapter does with them: which
	// directory, under which posture, is the adapter's own business (#81) and is
	// asserted in the adapter's tests. What the suite gets from this list is the
	// ability to tell an adapter's own injection from a daemon variable leaking
	// through — which it otherwise cannot, since both are just names in the
	// child's environment.
	//
	// It is a declaration, so it proves nothing about completeness on its own: an
	// adapter that injected a variable and forgot to list it fails the audit
	// loudly, which is the direction that matters, but one that lists a variable
	// it must not inject is only caught where that variable's *value* is asserted
	// — in the adapter's own tests, anchored on where the value came from.
	OwnedDirs []string

	// Posture names the adapter's REQUIRED provider key — the permission or
	// sandbox posture a headless daemon must state rather than inherit — and
	// the refusal for omitting it.
	Posture Posture

	// Fake writes this harness's native stream and answers its probes.
	Fake Fake

	// Errors are the adapter's named refusals.
	Errors Errors
}

// Credential is the adapter's documented auth surface (SPEC §7.6, §7.7).
type Credential struct {
	// Key is the agent.provider key, e.g. "api_key".
	Key string
	// Env is the child environment variable it is injected as, e.g.
	// "ANTHROPIC_API_KEY".
	Env string
}

// Posture is the adapter's required permission/sandbox key.
type Posture struct {
	Key string
	Err error
}

// Errors are the named refusals the suite asserts on (AGENTS.md conventions).
type Errors struct {
	// ProviderKey refuses an unknown agent.provider key.
	ProviderKey error
	// Binary refuses a harness that is absent or does not identify itself.
	Binary error
	// PromptEmpty refuses a run with no prompt.
	PromptEmpty error
	// WorkspacePath refuses a RunSpec without a usable absolute workspace.
	WorkspacePath error
	// EnvNamespace refuses a BEN_ reservation violation from either side.
	EnvNamespace error
	// ProviderValue refuses a key whose value is the wrong shape or unusable.
	ProviderValue error
	// EnvReserved refuses a configuration in which two sites write one child
	// environment variable (SPEC §7.6): a generic surface respelling an
	// adapter-owned variable or the one `publish.env` names, or a `publish.env`
	// naming a variable the adapter owns.
	EnvReserved error
	// PublishCredential refuses a publish credential that cannot be resolved for
	// an attempt (SPEC §5.2.8) — readiness, not structure.
	PublishCredential error
}

// Options are what an adapter forwards to harness.Launch on the suite's behalf:
// the lifecycle windows, the transcript sink, and the signal sender.
//
// Signal is the only test-only hook left, and it serves two needs. One of the
// contract's guarantees — an unkillable process reporting unconfirmed
// (SPEC §9.8) — requires the kernel to withhold its cooperation, and nothing but
// a stub can arrange that. The other is *ordering* rather than withholding: a
// case whose subject is what the ladder reaches has to know the descendant is
// there before the first signal leaves, and delaying harness.SignalGroup is how
// that precondition is met without weakening what the ladder then does (#138,
// testDescendantsDieFirst). A hook that delays the real sender is not a fake of
// it; one that respells `-pgid` would be.
//
// There used to be a second: a hook fired inside the terminal-event publication,
// so a test could claim a liveness verdict in the instant the outcome was being
// chosen. That interleaving is now one transition in the harness's lifecycle
// state machine, which is pure, so it is asserted by calling a function instead
// of by threading a hook through production types (harness/lifecycle_test.go).
// A hook can only ever force the one ordering somebody thought of.
type Options struct {
	// Timings are the harness's lifecycle windows. The suite drives them short
	// rather than sleeping through the production ones (see suiteTimings); a
	// test that depends on a particular window pins that field itself.
	Timings     harness.Timings
	Transcripts harness.TranscriptStore
	Signal      harness.SignalFunc
	// Publish is the publish credential's binding (SPEC §5.2.8): the child
	// variable and the source it is minted from. An adapter's New must forward
	// it, and the tests that set it assert on the child environment rather than
	// on the forwarding, so an adapter that drops it fails rather than silently
	// passing.
	Publish core.PublishBinding
	// AttemptTimeout is the TTL gate's other operand (SPEC §7.7). Zero for the
	// unbounded sources most cases bind, where no gate applies.
	AttemptTimeout time.Duration
	// OnRun is the per-workspace run-evidence sink (SPEC §9.10). The suite
	// injects one to assert the adapter addresses the right workspace.
	OnRun core.RunEvidenceSink
}

// Fake is the adapter's half of the scripted harness that the suite drives as a
// real process (SPEC §7.4, §7.5 are only meaningfully tested against one).
//
// The emitters have an exact contract, because the suite asserts on the event
// sequence and on the transcript line count:
//
//	Init     one line → exactly one started event carrying SessionID
//	Private  one line → no normalized meaning, i.e. exactly one heartbeat
//	Text     one line → exactly one progress event whose Text is the argument
//	Success  one line → a usage event equal to Usage, then succeeded
//
// One line each: the transcript is the raw stream verbatim (SPEC §7.2), and the
// suite counts its lines.
type Fake interface {
	// IsInvocation reports whether argv is a harness call rather than a `go
	// test` one. The test binary re-execs itself as the harness, so missing a
	// form here does not fail loudly — it would re-run the suite as a child —
	// and the set must cover every way the adapter invokes the binary.
	IsInvocation(args []string) bool
	// Probe answers a readiness invocation (version, credential status) and
	// reports whether it recognized one. It never returns for a probe it
	// handled: it exits the process with the code the real harness would use,
	// which is what lets a test assert an adapter reads the *answer* rather
	// than the exit status.
	Probe(args []string) (handled bool)

	// SessionID is the identity Init announces, and therefore the continuation
	// token the adapter must mint from it (SPEC §7.1).
	SessionID() string
	// Usage is the normalized accounting Success implies (SPEC §7.2).
	Usage() core.Usage

	Init(w io.Writer)
	Private(w io.Writer)
	Text(w io.Writer, text string)
	Success(w io.Writer)
}
