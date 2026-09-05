package agenttest

import (
	"encoding/json"
	"fmt"
	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/partest"
)

// The scripted fake harness. Process discipline (SPEC §7.5) and liveness (§7.4)
// are only real if they are tested against a real process, so an adapter's test
// binary re-execs itself as the harness: when its argv looks like a harness
// invocation rather than a `go test` one, it behaves as the scripted harness
// and never reaches the suite.
//
// The scripts are shared because the situations they stage are properties of a
// child process, not of a harness — a run that dies mid-stream, one that never
// speaks, one that ignores SIGTERM, one that spawns a tool of its own. Only the
// bytes on stdout differ between harnesses, and those come from the Fake.
const (
	// Deliberately not BEN_-prefixed: that namespace is reserved to the
	// orchestrator and a provider block may not define it (SPEC §7.6). These
	// travel through provider.env, so the rule applies to them.
	ScriptEnv = "FAKE_HARNESS_SCRIPT"
	DumpEnv   = "FAKE_HARNESS_DUMP"
	PIDEnv    = "FAKE_HARNESS_PIDFILE"
	// SlowStartEnv is a duration the spawned grandchild waits before it protects
	// itself from SIGTERM — the start-up latency #138 measured happening by
	// itself, staged deliberately so a caller's ordering guarantee is asserted
	// rather than hoped for. Unset means no delay.
	SlowStartEnv = "FAKE_HARNESS_SLOW_START"
	// AuthEnv and ProbeEnv drive the adapter-supplied Probe: what its
	// credential check should answer, and what its version check should do
	// besides answer — "leak" a child holding stdout (LeakPipeHolder), or
	// "flood" stdout past the bound the probe retains (FloodProbe).
	AuthEnv  = "FAKE_HARNESS_AUTH"
	ProbeEnv = "FAKE_HARNESS_PROBE"
)

// Scripts the suite drives. An adapter never names these; they are listed for
// the reader of a failing test.
const (
	scriptSuccess        = "success"
	scriptEchoPrompt     = "echo-prompt"
	scriptNoTerminal     = "no-terminal"
	scriptNoTerminalZero = "no-terminal-zero-exit"
	scriptNoStream       = "no-stream"
	scriptSilent         = "silent"
	scriptStubborn       = "stubborn"
	scriptSurvivor       = "survivor"
	scriptStopThenWrite  = "stop-then-write"
	scriptBigLine        = "big-line"
	scriptOversizedLine  = "oversized-line"
	scriptLinger         = "linger"
	scriptChatty         = "chatty"
	scriptManyLines      = "many-lines"
	scriptLateSuccess    = "late-success"
	scriptTrailing       = "trailing-output"
	scriptGrandchild     = "grandchild-silent"
	scriptGrandchildWork = "grandchild-worker"
	scriptPipeHolder     = "pipe-holder"
	scriptGarbage        = "garbage"
	scriptEchoEnv        = "echo-env"
	scriptUntrustedID    = "untrusted-session-id"
	scriptUniversalOK    = "universal-success"
	scriptUniversalFail  = "universal-failure"
	scriptUniversalLive  = "universal-live"
)

// HostileSessionID is the identity scriptUntrustedID announces: a token shaped
// like a flag rather than like a session id (#233).
//
// One constant serves both harnesses because the hazard is not harness-shaped.
// The session or thread id an adapter reads off the child's stream is the one
// argv element the *agent* chose, and every argv reads a leading `-` the same
// way — so a token like this is what the harness would be handed as an option
// on the next attempt if the adapter minted a continuation from it.
const HostileSessionID = "--config=ben.hostile=1"

// NoStreamStderr is what scriptNoStream writes to stderr: the shape of
// explanation a real harness gives when it refuses before streaming anything.
//
// A constant rather than a value travelling through provider.env, which is how
// the suite used to set it. Every `env` value is a credential value as far as
// SensitiveFields is concerned (deliberately — see harness.SensitiveFields), so
// a value passed that way is now redacted from the transcript, and this test
// asserts the transcript *keeps* it. Production stderr is not an env value, so
// the channel was the artifact, not the rule.
const NoStreamStderr = "Invalid API key \u00b7 Please run /login"

// stopSurvivorMarker is what scriptStopThenWrite writes after the caller has its
// answer. A transcript missing it is a transcript something truncated.
const stopSurvivorMarker = "written after the stop returned"

// bigLine is the assistant text used to prove the scanner buffer is raised well
// past bufio's 64 KiB default (SPEC §7.5).
const bigLine = 1 << 20

// successText is the single assistant line the success script writes. It is
// named because the suite asserts on it from two directions — as a progress
// event, and as a transcript line rebuilt through the Fake's emitters — and a
// literal repeated across the script and both assertions is a divergence
// waiting to happen.
const successText = "working on it"

// manyLines is how many lines the many-lines script writes before going quiet:
// far past any internal event buffer, and small enough that the whole burst fits
// in a pipe without the writer blocking, so the run is decided by the liveness
// window rather than by back-pressure.
const manyLines = 200

// Main is an adapter test binary's entry point: it becomes the fake harness
// when invoked as one, and runs the tests otherwise.
//
//	func TestMain(m *testing.M) { agenttest.Main(m, fake{}, cohort) }
//
// cohort is the adapter package's own bounded cohort (#167), reported on after
// the run. Its members are top-level tests, so there is no parent to hang a
// cleanup on and no earlier point at which its peak is final. Nil is legal, for
// a package that has no cohort of its own. Optional cleanups run after that
// verdict and before the parent test process exits; fake-harness re-execs return
// before reaching them.
func Main(m *testing.M, f Fake, cohort *partest.Gate, cleanups ...func()) {
	args := os.Args[1:]
	// The suite's own re-execs (a spawned tool, a pipe holder) carry a marker
	// rather than a harness-shaped argv. Neither adapter has to recognize them,
	// and a marker cannot be mistaken for a `go test` flag.
	if len(args) > 0 && args[0] == harnessArg {
		runFake(f, args)
		return
	}
	if f.IsInvocation(args) {
		runFake(f, args)
		return
	}
	code := m.Run()
	if cohort != nil {
		for _, problem := range cohort.Problems() {
			fmt.Fprintf(os.Stderr, "package cohort: %s\n", problem)
			code = 1
		}
	}
	for _, cleanup := range cleanups {
		cleanup()
	}
	os.Exit(code)
}

// dump is what the fake records about its own invocation, for the tests that
// assert on argv, env, cwd, and stdin.
type dump struct {
	Argv   []string `json:"argv"`
	Env    []string `json:"env"`
	Cwd    string   `json:"cwd"`
	Stdin  string   `json:"stdin"`
	Script string   `json:"script"`
}

func runFake(f Fake, args []string) {
	// Dumping before the probe handling lets the readiness invocations be
	// audited the same way a run is.
	DumpInvocation("")
	if f.Probe(args) {
		// A probe that returns rather than exiting is a bug in the Fake: the
		// suite counts probe invocations and expects the real harness's exit
		// status, not this one.
		fmt.Fprintln(os.Stderr, "fake harness: Probe handled an invocation without exiting")
		os.Exit(70)
	}

	// A real harness reads its prompt from stdin; reading it here is what makes
	// the "prompt never in argv" assertion meaningful (SPEC §7.6).
	prompt, _ := io.ReadAll(os.Stdin)
	script := os.Getenv(ScriptEnv)

	if path := os.Getenv(DumpEnv); path != "" {
		writeDump(path, string(prompt))
	}

	out := os.Stdout
	switch script {
	case scriptUniversalOK:
		f.Init(out)
		f.Text(out, successText)
		f.Success(out)
		os.Exit(0)

	case scriptUniversalFail:
		f.Init(out)
		os.Exit(3)

	case scriptUniversalLive:
		f.Init(out)
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptSuccess:
		f.Init(out)
		f.Private(out)
		f.Text(out, successText)
		f.Success(out)
		os.Exit(0)

	case scriptUntrustedID:
		// A healthy run that announces an identity no adapter may resume from
		// (#233). Healthy on purpose: the announcement is a line like any other,
		// so refusing it costs the resume and must not cost the attempt
		// (SPEC §7.2).
		f.InitUntrusted(out)
		f.Text(out, successText)
		f.Success(out)
		os.Exit(0)

	case scriptEchoPrompt:
		f.Init(out)
		f.Text(out, strings.TrimSpace(string(prompt)))
		f.Success(out)
		os.Exit(0)

	case scriptNoTerminal:
		// Started, said something, then died: SPEC §7.4's crash case. The
		// non-zero exit is deliberately not the signal being tested.
		f.Init(out)
		f.Text(out, "about to die")
		os.Exit(3)

	case scriptNoTerminalZero:
		// Same case with a zero exit: exit codes never decide the outcome.
		f.Init(out)
		os.Exit(0)

	case scriptNoStream:
		fmt.Fprintln(os.Stderr, NoStreamStderr)
		os.Exit(1)

	case scriptEchoEnv:
		// The leak transcript redaction exists for: an agent that prints its own
		// environment. On both streams, because each reaches the transcript by a
		// different path — stdout line by line through the Fake's emitters,
		// stderr as the ben:stderr tail the harness marshals itself.
		//
		// The child environment is the restricted one (SPEC §7.6), so all of it
		// fits well inside the retained stderr tail.
		f.Init(out)
		for _, kv := range os.Environ() {
			f.Text(out, kv)
		}
		fmt.Fprintln(os.Stderr, strings.Join(os.Environ(), "\n"))
		// The same dump again, raw rather than through the emitters: an agent
		// printing a traceback writes text the harness has not escaped, so a
		// multiline value arrives as several lines — several writes — which is the
		// case a needle spelled with its newline cannot match. Non-JSON lines are
		// activity with no normalized meaning (SPEC §7.2), so this stays legal.
		fmt.Fprintln(out, strings.Join(os.Environ(), "\n"))
		f.Success(out)
		os.Exit(0)

	case scriptSilent:
		// Never writes a byte; the stall window is the only thing that ends it.
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptStubborn:
		// Ignores SIGTERM, so only SIGKILL ends it (SPEC §7.5's escalation).
		signal.Ignore(syscall.SIGTERM)
		f.Init(out)
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptSurvivor:
		// Outlives a Stop whose signals do not land (the suite stubs the signal
		// sender), then exits on its own so the test leaks nothing. The sleep
		// only has to outlast that test's signal ladder.
		signal.Ignore(syscall.SIGTERM)
		f.Init(out)
		time.Sleep(1500 * time.Millisecond)
		os.Exit(0)

	case scriptStopThenWrite:
		// Survives a signal ladder the suite stubs out, keeps writing *after*
		// the caller has been told the termination was unconfirmed, then exits.
		// That trailing line is the evidence for #79: an unconfirmed
		// StopInterrupt, or a Probe, must leave the stream alone, so the line
		// has to reach the transcript.
		signal.Ignore(syscall.SIGTERM)
		f.Init(out)
		time.Sleep(600 * time.Millisecond)
		f.Text(out, stopSurvivorMarker)
		os.Exit(0)

	case scriptBigLine:
		f.Init(out)
		f.Text(out, strings.Repeat("x", bigLine))
		f.Success(out)
		os.Exit(0)

	case scriptOversizedLine:
		// One assistant line past the scanner ceiling, then silence (#235). The
		// scanner cannot continue past it, so nothing but the adapter's own
		// verdict can end this run: the script never terminates on its own, and
		// the case that drives it sets no liveness window.
		f.Init(out)
		f.Text(out, strings.Repeat("x", harness.MaxScanLine+1))
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptLinger:
		// Declares success, then hangs around: the terminal event is ground
		// truth and the adapter must not let the process outlive it.
		f.Success(out)
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptChatty:
		// Emits far past the event buffer and never terminates: whoever is (not)
		// draining events must not be able to postpone the attempt timeout.
		f.Init(out)
		for i := 0; ; i++ {
			f.Text(out, fmt.Sprintf("line %d", i))
			if i > 4*64 {
				time.Sleep(50 * time.Millisecond)
			}
		}

	case scriptManyLines:
		// Writes a known number of lines and then goes quiet, without ever
		// terminating. A consumer that never drains parks the event stream after
		// the first few, so what ends up in the transcript is the question: the
		// record is the adapter's obligation whether or not anyone is reading
		// (SPEC §7.2, §10.3).
		f.Init(out)
		for i := range manyLines {
			f.Text(out, fmt.Sprintf("line %d", i))
		}
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptLateSuccess:
		// Ignores SIGTERM and declares success *after* a short liveness window
		// would have closed: the harness's own verdict arriving inside the kill
		// window must not undo a timeout the adapter already declared.
		signal.Ignore(syscall.SIGTERM)
		time.Sleep(250 * time.Millisecond)
		f.Success(out)
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptTrailing:
		// Writes after its terminal line. The pump has published a terminal
		// event by then, so it must keep consuming or the reader — which owns
		// the transcript — strands mid-send.
		f.Init(out)
		f.Success(out)
		for i := range 5 {
			f.Text(out, fmt.Sprintf("after the result %d", i))
		}
		os.Exit(0)

	case scriptGrandchild:
		// A tool the harness spawned, in the same process group, that ignores
		// SIGTERM — the case where killing only the leader leaves something
		// alive in the workspace. It records its pid so the test can check.
		spawnGrandchild()
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptGrandchildWork:
		// Any staged start-up latency happens here, *before* the process is
		// SIGTERM-proof: exec to signal.Ignore is the window in which a group
		// SIGTERM kills a descendant that has not registered yet. #138 measured
		// that window closing on its own — a re-exec of this
		// race-and-coverage-instrumented binary reached its first instruction
		// 15-70 ms after its parent started it under full-suite load, and in the
		// failing run never reached it at all.
		if raw := os.Getenv(SlowStartEnv); raw != "" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				// Loud, not ignored: a delay that silently became zero would
				// leave the case that asked for it passing by luck again.
				recordFixtureError(fmt.Sprintf("%s=%q is not a duration: %v", SlowStartEnv, raw, err))
				os.Exit(64)
			}
			time.Sleep(d)
		}
		signal.Ignore(syscall.SIGTERM)
		// The pid lands after the ignore, never before. This file is the
		// registration marker a caller orders the signal ladder behind
		// (testDescendantsDieFirst), so a pid published first would release the
		// SIGTERM into the very window the ordering exists to close.
		os.WriteFile(os.Getenv(PIDEnv), []byte(strconv.Itoa(os.Getpid())), 0o600)
		time.Sleep(60 * time.Second)
		os.Exit(0)

	case scriptPipeHolder:
		// Holds the inherited stdout for longer than any readiness context, so
		// the probe's pipes stay open after its process is gone. Its pid was
		// recorded by whoever spawned it (LeakPipeHolder).
		time.Sleep(30 * time.Second)
		os.Exit(0)

	case scriptGarbage:
		// Any raw line is activity, and neither malformed JSON nor a line kind
		// from a future harness release may end a healthy run (SPEC §7.2).
		f.Init(out)
		fmt.Fprintln(out, "not json at all")
		fmt.Fprintln(out, `{"type":"unknown_future_kind"}`)
		f.Success(out)
		os.Exit(0)

	default:
		fmt.Fprintf(os.Stderr, "fake harness: unknown script %q\n", script)
		os.Exit(64)
	}
}

// writeDump appends one line per invocation rather than overwriting, so a test
// can audit *every* way the adapter invoked the binary — the readiness probes
// included. An overwriting dump silently hides all but the last call, which is
// how a probe running with the wrong environment escaped notice.
func writeDump(path, prompt string) {
	cwd, _ := os.Getwd()
	b, _ := json.Marshal(dump{
		Argv: os.Args, Env: os.Environ(), Cwd: cwd,
		Stdin: prompt, Script: os.Getenv(ScriptEnv),
	})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

// DumpInvocation appends this process's argv and environment to the configured
// fake-harness dump. Test helpers that stand in front of the harness use it so
// an audit sees the wrapper and the wrapped process through the same format.
func DumpInvocation(prompt string) {
	if path := os.Getenv(DumpEnv); path != "" {
		writeDump(path, prompt)
	}
}

// LeakPipeHolder leaves a descendant holding this process's stdout, so the
// probe's exit does not close the pipe. A Fake calls it from its version probe
// when ProbeEnv is set; it is what makes an unbounded post-exit wait — and an
// orphaned probe child — observable.
func LeakPipeHolder() {
	if os.Getenv(ProbeEnv) != "leak" {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, harnessArg)
	cmd.Env = append(os.Environ(), ScriptEnv+"="+scriptPipeHolder)
	cmd.Stdout = os.Stdout // the point: inherits the probe's stdout
	if err := cmd.Start(); err != nil {
		return
	}
	// The *parent* records the pid, synchronously, before this probe exits.
	// Letting the child record its own is a race the test cannot win: the probe
	// runs in its own process group and the group is killed on the way out, so
	// under load the child can be killed before it is ever scheduled — and a
	// test that reads "no pid recorded" as a failure then fails for the one
	// reason that is not a defect.
	if path := os.Getenv(PIDEnv); path != "" {
		os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	}
}

// FloodProbe writes past the bound a readiness probe retains
// (harness.MaxProbeOutput) when the suite asks for it (ProbeEnv "flood"). A
// Fake calls it from its version probe *after* printing the answer the adapter
// reads, so a refusal is the bound speaking and not a missing marker — the head
// of the output is exactly what would have passed.
func FloodProbe() {
	if os.Getenv(ProbeEnv) != "flood" {
		return
	}
	Flood(os.Stdout)
}

// Flood writes more than harness.MaxProbeOutput bytes to w, in whole lines of
// low-entropy text. Exported for a Fake that has to flood a stream of its own
// choosing — codex's login probe answers on stderr.
func Flood(w io.Writer) {
	chunk := []byte(strings.Repeat("x", 4095) + "\n")
	for written := 0; written <= harness.MaxProbeOutput; written += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			return
		}
	}
}

// spawnGrandchild re-execs this binary as a worker in the *same* process group,
// which is what a harness spawning a tool looks like. Its output goes nowhere so
// the test exercises the signal ladder rather than the pipe-drain bound.
func spawnGrandchild() {
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		recordFixtureError("opening " + os.DevNull + ": " + err.Error())
		return
	}
	defer devnull.Close()
	self, err := os.Executable()
	if err != nil {
		recordFixtureError("locating this binary: " + err.Error())
		return
	}
	cmd := exec.Command(self, harnessArg)
	cmd.Env = append(os.Environ(), ScriptEnv+"="+scriptGrandchildWork)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	if err := cmd.Start(); err != nil {
		recordFixtureError("spawning the grandchild: " + err.Error())
	}
}

// recordFixtureError leaves a reason beside the pid file the caller is waiting
// for, which is why every path above that gives up without a descendant says so
// on the way out.
//
// Every stream these scripts own goes to devnull by design, so a fixture that
// could not produce a descendant and one whose descendant is merely slow present
// as the same symptom: a pid that never arrives. #138 was two candidate
// mechanisms behind exactly that symptom, and telling them apart took an
// instrumented re-run rather than a reading of the failure.
func recordFixtureError(reason string) {
	if path := os.Getenv(PIDEnv); path != "" {
		os.WriteFile(path+fixtureErrorSuffix, []byte(reason), 0o600)
	}
}

// fixtureErrorSuffix names that file, beside the pid file (see readPID).
const fixtureErrorSuffix = ".fixture-error"

// harnessArg marks a re-exec the suite made itself, so Main can recognize its
// own children without asking the adapter whether an argv looks like a harness
// invocation.
const harnessArg = "--ben-fake-harness"
