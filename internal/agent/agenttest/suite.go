package agenttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/partest"
)

// conformanceParallelism is how many cases of the parallel cohort may run at
// once (#167).
//
// Four rather than "as many as the machine has". Every case here launches at
// least one child process — a re-exec of the race-and-coverage-instrumented
// test binary — and several launch a grandchild too, so the cohort's cost is
// process fan-out rather than CPU. `t.Parallel` alone would widen it to
// `-parallel`, which defaults to GOMAXPROCS and is therefore a property of
// whoever's laptop is running: the same suite would run four wide in CI and
// sixteen wide on a workstation, and only one of those two was ever measured.
// A suite whose subject is liveness under load must not make its own load a
// host variable.
//
// Four is also CI's own ceiling (a GitHub runner reports 4 CPUs), so the widest
// this cohort ever runs is the width CI runs it at.
const conformanceParallelism = 4

// runMode is the cohort a conformance case belongs to, and — when it is not the
// parallel one — why.
//
// The reason is carried rather than implied because "serial" on its own decays:
// the next reader cannot tell a case that must stay serial from one nobody has
// looked at yet, and the whole value of the split is that the distinction was
// made deliberately. There is no zero value: a row that names no mode fails
// TestEveryCaseDeclaresItsCohort rather than defaulting into either cohort.
type runMode string

const (
	// parallel: the case shares nothing with its siblings but the machine.
	parallel runMode = "parallel"

	// globalEnv: the case mutates state the whole test binary shares —
	// t.Setenv, t.Chdir. Anchored in the source rather than in this table
	// (TestSerialClassificationMatchesTheSource), and enforced a second time by
	// the `testing` package itself, which panics on a t.Setenv after
	// t.Parallel.
	globalEnv runMode = "serial: mutates process-global environment or working directory"

	// liveness: the case pins a §7.4 window — a stall or attempt timeout — or
	// asserts how long something took. Both are measurements of a loaded
	// machine, and widening the cohort moves them.
	liveness runMode = "serial: asserts a lifecycle window, or an elapsed bound"

	// discipline: the case samples whether a process is alive at a particular
	// instant, with no polling, to assert that group teardown *precedes*
	// publication (SPEC §7.5, §9.8). That ordering is the assertion, so the
	// margin around it may not be spent on neighbours.
	discipline runMode = "serial: asserts process-group teardown ordering at an instant"
)

// conformanceCase is one row of the suite.
type conformanceCase struct {
	name string
	fn   func(*testing.T, Contract)
	run  runMode
}

// conformanceCases is the suite. Package-level rather than a literal inside Run
// so the classification can be checked against the source that justifies it.
//
// Adding a row means deciding its cohort. The safe answer is a serial one: a
// case that could have been parallel and is not costs seconds, and one that
// could not and is costs a flake nobody can reproduce.
var conformanceCases = []conformanceCase{
	// The event model (SPEC §7.2).
	{"Capabilities", testCapabilities, parallel},
	{"HappyPath", testHappyPath, parallel},
	{"GarbageLinesAreActivity", testGarbageLines, parallel},
	{"BigLineBeyondScannerDefault", testBigLine, parallel},
	{"TranscriptRetainsRawStream", testTranscript, parallel},
	{"PromptIsRetainedBesideTheTranscript", testPromptRetained, parallel},
	{"SlowSinkKeepsTheTerminalLine", testSlowSink, parallel},
	{"TrailingOutputDoesNotStrandTheReader", testTrailingOutput, parallel},

	// Outcome and liveness (SPEC §7.3, §7.4).
	{"ExitWithoutTerminalEventIsCrashed", testCrashed, parallel},
	{"NoStreamIsCrashedWithStderrInTranscript", testNoStream, parallel},
	{"OversizedLineIsOutputOverflow", testOversizedLine, parallel},
	{"StallTimeout", testStallTimeout, liveness},
	{"AttemptTimeout", testAttemptTimeout, liveness},
	{"ActivityResetsStallWindow", testActivityResetsStall, liveness},
	{"TimeoutFiresWhileNobodyDrainsEvents", testTimeoutWithoutConsumer, liveness},
	{"TranscriptIsCompleteWithoutAConsumer", testTranscriptWithoutConsumer, liveness},
	{"InjectedTimingsReachTheRun", testInjectedTimingsReachTheRun, liveness},
	{"ClaimedVerdictWinsOverLateHarnessSuccess", testLateSuccessLoses, liveness},
	{"ClaimedVerdictSurvivesTheCleanupWait", testVerdictSurvivesCleanup, liveness},
	{"ContextCancellationDiscardsRun", testContextCancel, parallel},

	// Harness/domain discipline (SPEC §7.5, §9.8). These use agenttest's
	// process-group domain to exercise real processes and fault injection; the
	// production Linux containment mechanism is proved in localdomain's own real
	// tests. Serial because each case drives teardown under a short grace.
	{"StopEscalatesToSIGKILL", testStopEscalates, discipline},
	{"StopUnconfirmedWhenSignalsDoNotLand", testStopUnconfirmed, discipline},
	{"ProbeObservesWithoutTouchingTheGroup", testProbeObservesOnly, discipline},
	{"ProbeWithoutLookingIsUnconfirmed", testProbeWithoutLookingIsUnconfirmed, discipline},
	{"UnconfirmedInterruptLeavesTheStreamAlone", testUnconfirmedInterruptKeepsWriting, discipline},
	{"StopCleansAGroupThatOutlivedTheProcess", testStopCleansAfterDone, discipline},
	{"StopUnconfirmedWhenProbeIsDenied", testStopProbeDenied, discipline},
	{"StallKillUnconfirmedWhenGroupSurvives", testStallKillUnconfirmed, liveness},
	{"TimeoutKillUnconfirmedWhenGroupSurvives", testTimeoutKillUnconfirmed, liveness},
	{"StopAfterExitIsConfirmed", testStopAfterExit, discipline},
	{"ProcessKilledAfterTerminalEvent", testKilledAfterTerminal, discipline},
	{"DefaultSignalReachesDescendants", testDefaultSignalReachesDescendants, discipline},
	// Serial for the reason the cohort names, and no longer load-sensitive on
	// top of it: #138's race was the fixture's own descendant start-up against
	// the stall window, and it is ordered now rather than margined (see the
	// case).
	{"LivenessKillReachesDescendantsBeforePublishing", testDescendantsDieFirst, discipline},

	// Secrets and I/O (SPEC §7.6).
	{"PromptOnStdinAndCwdIsWorkspace", testPromptAndCwd, parallel},
	{"ArgvAndChildEnvAudit", testArgvAndEnvAudit, globalEnv},
	{"TranscriptRedactsCredentialValues", testTranscriptRedaction, globalEnv},
	{"TranscriptSinkSeesWholeLines", testTranscriptWholeLines, parallel},
	{"UnredactableCredentialIsRefused", testUnredactableCredential, parallel},
	{"UnredactableForwardedValueRefusedAtReady", testUnredactableForwardedAtReady, globalEnv},
	{"UnredactableForwardedValueRefusedAtStart", testUnredactableForwardedAtStart, globalEnv},
	{"StartRejectsRunSpecEnvOutsideNamespace", testRunSpecNamespace, parallel},
	{"StructuralRejectsReservedPrefixInProviderEnv", testProviderEnvNamespace, parallel},

	// The publish credential (SPEC §5.2.8).
	{"PublishCredentialReachesItsVariableAndNotArgv", testPublishReachesChild, globalEnv},
	{"PublishCredentialIsRedactedFromTheTranscript", testPublishRedaction, globalEnv},
	{"StructuralRefusesTwoSitesForOneChildVariable", testPublishReservation, parallel},
	{"UnresolvablePublishCredentialRefusedAtReadyAndStart", testPublishUnresolvable, globalEnv},
	{"AFailedPublishMintNeverLaunchesTheAgent", testPublishNeverLaunches, globalEnv},
	{"PublishCredentialDeadlineCoversTheAttempt", testPublishTTLGate, globalEnv},

	// Structure, readiness, and binding (SPEC §5.7, §7.1).
	{"KindConformsToRunnerKind", testKindConforms, parallel},
	{"ForwardedEnvVarsAreDeclared", testForwardedEnvVars, parallel},
	{"SensitiveFieldsAreDeclared", testSensitiveFields, parallel},
	{"ModelDeclaredIsModelLaunched", testModelDeclared, parallel},
	{"StructuralIsPureAndNeedsNoHarness", testStructuralIsPure, globalEnv},
	{"StructuralRefusesMalformedProvider", testMalformedProvider, parallel},
	{"StructuralRefusalsCarryTheValueAsData", testRefusalCarriesValueAsData, parallel},
	{"StartRefusalsProduceNoHandle", testStartRefusals, parallel},
	{"RunEvidenceAddressesItsOwnWorkspace", testEvidenceAddressesWorkspace, parallel},
	{"RunEvidenceSurvivesInterleavedStarts", testEvidenceSurvivesInterleavedStarts, parallel},
	{"StartMissingBinaryFailsBeforeHandle", testMissingBinary, parallel},
	{"ReadyRefusals", testReadyRefusals, parallel},
	{"ExecutionDomainRefusals", testExecutionDomainRefusals, parallel},
	{"ReadyProbeEnvironmentIsRestricted", testProbeEnvRestricted, globalEnv},
	{"ReadyIsBoundedByItsContext", testReadyBounded, liveness},
	{"ReadyLeavesNoOrphanedProbeChild", testReadyNoOrphans, discipline},
	{"ReadyRefusesAFloodingProbe", testReadyFloodingProbe, parallel},
	{"BinaryInstalledAfterNewIsFoundByReady", testBinaryInstalledLater, globalEnv},
	{"ReadyAndStartCannotDisagree", testReadyAndStartAgree, parallel},
	{"BoundBinarySurvivesPathChange", testBoundBinarySurvivesPath, globalEnv},
	{"RelativeBinaryIsBoundAbsolutely", testRelativeBinaryIsAbsolute, globalEnv},
	{"ContinuationReachesTheHarness", testContinuation, parallel},
	{"HostileContinuationIsRefusedAtBothEnds", testHostileContinuation, parallel},
}

// Run executes the whole conformance suite against one adapter. An adapter's
// package calls it from a single test:
//
//	func TestConformance(t *testing.T) { agenttest.Run(t, contract(t)) }
//
// Cases run in two cohorts (#167): the parallel one, bounded at
// conformanceParallelism, and everything else, serially, exactly as the whole
// suite used to. The `testing` package keeps the two apart for free — a
// parallel subtest is paused until its parent has finished running every
// sequential sibling — so a case that mutates the process environment can never
// overlap one that reads it, and neither the split nor the bound changes which
// cases run or what any of them assert.
func Run(t *testing.T, c Contract) {
	t.Helper()
	gate := partest.New(conformanceParallelism)
	// On the parent, so it runs after the parallel cohort has drained: a
	// cleanup registered here is the last thing this test does, and the peak is
	// not final until then.
	t.Cleanup(func() { gate.Check(t) })

	for _, tc := range conformanceCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.run == parallel {
				gate.Enter(t)
			}
			tc.fn(t, c)
		})
	}
}

// --- the event model (SPEC §7.2) ---

// Capabilities are a promise the orchestrator warns on at load rather than
// discovering mid-run (SPEC §7.1), so each declared one is checked against
// behaviour: resume must carry a token, and usage must actually report.
func testCapabilities(t *testing.T, c Contract) {
	caps := c.runner(t, scriptSuccess, nil, Options{}).Capabilities()

	spec := c.spec(t, core.RunLimits{})
	spec.Continuation = "token-from-a-prior-run"
	h, err := c.runner(t, scriptSuccess, nil, Options{}).Start(context.Background(), spec)
	if caps.Resume {
		if err != nil {
			t.Fatalf("Start with a continuation = %v, but Capabilities claims resume", err)
		}
		t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })
		collect(t, h)
		return
	}
	// An adapter without resume MUST fail loudly rather than silently start a
	// fresh session (SPEC §7.1).
	if err == nil {
		t.Error("Start accepted a continuation token although Capabilities denies resume")
	}
}

func testHappyPath(t *testing.T, c Contract) {
	r := c.runner(t, scriptSuccess, nil, Options{})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))

	evs := collect(t, h)
	want := []core.EventType{
		core.EventStarted,
		core.EventHeartbeat, // the private line: activity, no normalized meaning
		core.EventProgress,
	}
	if r.Capabilities().Usage {
		want = append(want, core.EventUsage)
	}
	want = append(want, core.EventSucceeded)
	if got := types(evs); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	// The session id is identity; the continuation token is what a later
	// RunSpec carries back, minted here and never interpreted by the
	// orchestrator (SPEC §7.1).
	if evs[0].SessionID != c.Fake.SessionID() {
		t.Errorf("started session = %q, want %q", evs[0].SessionID, c.Fake.SessionID())
	}
	if evs[0].Continuation == "" {
		t.Error("started minted no continuation token, but Capabilities claims resume")
	}
	if evs[2].Text != successText {
		t.Errorf("progress text = %q, want %q", evs[2].Text, successText)
	}
	if r.Capabilities().Usage {
		if got, want := *evs[3].Usage, c.Fake.Usage(); got != want {
			t.Errorf("usage = %+v, want %+v", got, want)
		}
	}
	for _, ev := range evs {
		if ev.Time.IsZero() {
			t.Errorf("event %v has no timestamp", ev.Type)
		}
	}
	waitDone(t, h)
}

// Any raw line is activity, and an unparseable or future line kind must not
// end a healthy run (SPEC §7.2).
func testGarbageLines(t *testing.T, c Contract) {
	h := c.start(t, c.runner(t, scriptGarbage, nil, Options{}), c.spec(t, core.RunLimits{}))
	evs := collect(t, h)
	if got := terminal(t, evs); got.Type != core.EventSucceeded {
		t.Errorf("terminal = %+v, want succeeded", got)
	}
	if n := countType(evs, core.EventHeartbeat); n != 2 {
		t.Errorf("heartbeats = %d, want 2 (one per untranslatable line): %v", n, types(evs))
	}
}

// SPEC §7.5: the scanner buffer is raised well past bufio's 64 KiB default, or
// a large tool result would look like a dead stream. The line is *read* whole —
// the run succeeds, and testTranscriptWholeLines has it reach the sink as one
// write — but what the event carries of it is bounded (#235,
// harness.MaxEventText), with the cut stated in the text.
//
// This case used to assert the event carried all 1 MiB. That was the unbounded
// retention the ticket removed: between the adapter and the orchestrator's 16 KiB
// tail sit two queues, and a queue holds whatever it is handed.
func testBigLine(t *testing.T, c Contract) {
	h := c.start(t, c.runner(t, scriptBigLine, nil, Options{}), c.spec(t, core.RunLimits{}))
	evs := collect(t, h)
	if got := terminal(t, evs); got.Type != core.EventSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", got)
	}
	var progress core.Event
	for _, ev := range evs {
		if ev.Type == core.EventProgress {
			progress = ev
		}
	}
	if progress.Type != core.EventProgress {
		t.Fatalf("no progress event for the big line: %v", types(evs))
	}
	if len(progress.Text) > harness.MaxEventText {
		t.Errorf("progress text is %d bytes, want at most %d: the event carries the line unbounded",
			len(progress.Text), harness.MaxEventText)
	}
	if !strings.HasPrefix(progress.Text, strings.Repeat("x", 1024)) {
		t.Errorf("progress text does not begin with the line's own text: %q…",
			progress.Text[:min(len(progress.Text), 64)])
	}
	// Stated, not silent: the next attempt reads this text as the account of
	// what the last one said (SPEC §9.6), and must not take a cut for the whole.
	if !strings.Contains(progress.Text, fmt.Sprintf("of %d bytes", bigLine)) {
		t.Errorf("the cut is not stated in the text; its tail is %q",
			progress.Text[max(0, len(progress.Text)-120):])
	}
}

// The raw stream is retained verbatim (SPEC §7.2, §10.3): the normalized events
// are lossy, the transcript is not.
//
// "Verbatim" is asserted as the harness's own lines, byte for byte and in order
// — including the private one the event stream drops entirely, which is what
// makes the transcript worth keeping. BEN-namespaced lines are excluded rather
// than counted: the record synthesizes one whenever the child writes to stderr
// (see finishTranscript), so any assertion on a total line count breaks on
// stderr traffic the harness contract permits. `go test -cover` is the standing
// example — the instrumented child warns about GOCOVERDIR — and a suite that
// cannot run under coverage cannot report which lifecycle paths it exercises.
func testTranscript(t *testing.T, c Contract) {
	dir := t.TempDir()
	r := c.runner(t, scriptSuccess, nil, Options{Transcripts: harness.DirTranscripts{Dir: filepath.Join(dir, "transcripts")}})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	collect(t, h)
	waitDone(t, h)

	// What the success script wrote to stdout, rebuilt through the very
	// emitters it used: one line each, by the Fake's contract.
	var stdout bytes.Buffer
	c.Fake.Init(&stdout)
	c.Fake.Private(&stdout)
	c.Fake.Text(&stdout, successText)
	c.Fake.Success(&stdout)
	want := splitLines(stdout.String())

	raw := onlyTranscript(t, filepath.Join(dir, "transcripts"))
	got := harnessLines(raw)
	if !slices.Equal(got, want) {
		t.Fatalf("transcript is not the raw stream verbatim.\ngot (%d harness lines):\n%s\nwant (%d):\n%s\nwhole transcript:\n%s",
			len(got), strings.Join(got, "\n"), len(want), strings.Join(want, "\n"), raw)
	}
}

// SPEC §9.5: "what was this agent told" is answerable after the fact. The
// canonical rendered prompt is retained per attempt beside the transcript, at
// 0600.
//
// Asserted here rather than only against DirTranscripts because the claim spans
// the adapter: the bytes on disk must be the bytes the *child* received, and
// only a real run establishes that. The prompt is deliberately given a marker
// the harness echoes back, so a file written from anything but `spec.Prompt`
// fails rather than coincidentally matching.
func testPromptRetained(t *testing.T, c Contract) {
	dir := t.TempDir()
	transcripts := filepath.Join(dir, "transcripts")
	dumpPath := filepath.Join(dir, "dump.json")
	r := c.runner(t, scriptEchoPrompt, map[string]string{DumpEnv: dumpPath},
		Options{Transcripts: harness.DirTranscripts{Dir: transcripts}})
	spec := c.spec(t, core.RunLimits{})
	spec.Prompt = "PROMPT-BODY do the thing\nwith a second line\n"
	h := c.start(t, r, spec)
	collect(t, h)
	waitDone(t, h)

	if got := onlyPrompt(t, transcripts); got != spec.Prompt {
		t.Errorf("retained prompt = %q, want the bytes the run was given, %q", got, spec.Prompt)
	}
	// The same bytes the child actually read on stdin — the two must not be
	// separately-derived answers to one question (SPEC §9.5's "the bytes sent",
	// not a second render).
	if d := readDump(t, dumpPath); d.Stdin != spec.Prompt {
		t.Errorf("harness stdin = %q, want %q", d.Stdin, spec.Prompt)
	}
	// It holds the untrusted issue body verbatim (SPEC §5.6), so it is no more
	// readable than the transcript beside it (SPEC §10.3).
	entries, err := os.ReadDir(transcripts)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), harness.PromptSuffix) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("retained prompt %s has mode %04o, want 0600", e.Name(), got)
		}
	}
}

// Regression: cmd.StdoutPipe hands the read end to Wait, which closes it the
// moment the process exits — so a reader that is even slightly behind loses
// whatever is still buffered, including the line that decides the run. A slow
// transcript sink is the cheapest way to widen that window.
func testSlowSink(t *testing.T, c Contract) {
	r := c.runner(t, scriptSuccess, nil, Options{
		Transcripts: slowTranscripts{delay: 200 * time.Millisecond},
		// The post-exit drain is a *ceiling* on how long the pipes stay open, and
		// this sink deliberately keeps the reader busy well past the process's
		// exit. So the window has to cover the whole stream: stranding the reader
		// when it elapses is the right answer for a run nobody is draining
		// (harness.boundStream), but here it would drop the terminal line for a
		// reason that has nothing to do with the regression under test. Nothing
		// waits this out — the window ends the moment the transcript is flushed.
		Timings: harness.Timings{PostExitDrain: 10 * time.Second},
	})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	if got := terminal(t, collect(t, h)); got.Type != core.EventSucceeded {
		t.Fatalf("terminal = %+v, want succeeded — the terminal line was dropped", got)
	}
	waitDone(t, h)
}

// Regression: anything the harness writes after its terminal line used to
// strand the stdout reader mid-send on an unbuffered channel. That goroutine
// owns the transcript, so the forensic record was never closed — and its file
// descriptors never released.
func testTrailingOutput(t *testing.T, c Contract) {
	rec := &recordingTranscript{}
	h := c.start(t, c.runner(t, scriptTrailing, nil, Options{Transcripts: rec}), c.spec(t, core.RunLimits{}))

	if got := terminal(t, collect(t, h)); got.Type != core.EventSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", got)
	}
	waitDone(t, h)

	// Asserted the instant Done closes, with no polling: Done means the process
	// is reaped *and* the forensic record is complete (SPEC §10.3), so a
	// transcript that is merely closed "shortly afterwards" is the race, not the
	// contract.
	if !rec.isClosed() {
		t.Fatal("transcript not closed when Done closed: the stdout reader is still stranded")
	}
	if got := rec.text(); !strings.Contains(got, "after the result 4") {
		t.Errorf("transcript is missing post-terminal output:\n%s", got)
	}
}

// --- outcome and liveness (SPEC §7.3, §7.4) ---

// Process exit without a terminal event → failed(crashed), and exit codes never
// decide the outcome.
func testCrashed(t *testing.T, c Contract) {
	for _, script := range []string{scriptNoTerminal, scriptNoTerminalZero} {
		t.Run(script, func(t *testing.T) {
			h := c.start(t, c.runner(t, script, nil, Options{}), c.spec(t, core.RunLimits{}))
			got := terminal(t, collect(t, h))
			if got.Type != core.EventFailed || got.Reason != core.FailureCrashed {
				t.Errorf("terminal = %+v, want failed(crashed)", got)
			}
			waitDone(t, h)
		})
	}
}

// SPEC §7.4 is unconditional: exit without a terminal event is crashed, even
// when the harness produced no stream at all and even when stderr says why. The
// explanation is preserved in the transcript instead of bending the taxonomy.
func testNoStream(t *testing.T, c Contract) {
	const stderr = NoStreamStderr
	dir := t.TempDir()
	r := c.runner(t, scriptNoStream, nil,
		Options{Transcripts: harness.DirTranscripts{Dir: dir}})

	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	got := terminal(t, collect(t, h))
	if got.Type != core.EventFailed || got.Reason != core.FailureCrashed {
		t.Errorf("terminal = %+v, want failed(crashed)", got)
	}
	waitDone(t, h)

	raw := onlyTranscript(t, dir)
	if !strings.Contains(raw, stderr) {
		t.Errorf("transcript lost the stderr explanation:\n%s", raw)
	}
	if !strings.Contains(raw, `"type":"ben:stderr"`) {
		t.Errorf("stderr should be a BEN-namespaced line, got:\n%s", raw)
	}
}

// A line past the scanner ceiling → failed(output_overflow), claimed by the
// adapter and non-retryable (SPEC §7.3, §7.5; #235).
//
// Before this case the suite drove a 1 MiB line — an order of magnitude *under*
// the ceiling — and the classification past it was never exercised. What it
// would have found: the scanner stops, nothing drains the pipe, the child blocks
// on a full pipe with no activity, and the run sits until the stall window reads
// it as `stalled` — retryable, so the orchestrator re-dispatches an agent that
// reproduces the line deterministically and burns max_attempts on it.
//
// No liveness window is set here on purpose. The script never terminates on its
// own, so the adapter's own verdict is the *only* thing that can end this run:
// a harness that merely let the stream end would hang this case, not fail it
// softly.
func testOversizedLine(t *testing.T, c Contract) {
	rec := &recordingTranscript{}
	h := c.start(t, c.runner(t, scriptOversizedLine, nil, Options{Transcripts: rec}), c.spec(t, core.RunLimits{}))
	evs := collect(t, h)
	got := terminal(t, evs)
	if got.Type != core.EventFailed || got.Reason != core.FailureOutputOverflow {
		t.Fatalf("terminal = %+v, want failed(output_overflow)", got)
	}
	// The run was healthy up to the line — the started event is delivered — and
	// nothing of the line itself was minted into an event.
	if n := countType(evs, core.EventStarted); n != 1 {
		t.Errorf("started events = %d, want 1: %v", n, types(evs))
	}
	if n := countType(evs, core.EventProgress); n != 0 {
		t.Errorf("progress events = %d, want 0: a fragment of the oversized line was minted", n)
	}
	waitDone(t, h)
	if !rec.isClosed() {
		t.Fatal("transcript not closed when Done closed")
	}

	// The record is verbatim up to the cut and honest about the cut: the
	// harness's lines are exactly the ones before the oversized one, a
	// BEN-namespaced marker says where the stream was cut and why, and no
	// fragment of the line is retained — a 10 MiB prefix ending wherever the
	// buffer ran out is the one shape a credential can straddle unredacted.
	var before bytes.Buffer
	c.Fake.Init(&before)
	raw := rec.text()
	if got, want := harnessLines(raw), splitLines(before.String()); !slices.Equal(got, want) {
		t.Errorf("harness lines in the transcript = %d lines, want exactly the ones before the cut (%d):\n%s",
			len(got), len(want), excerptLines(got))
	}
	if !strings.Contains(raw, `"type":"ben:truncated"`) {
		t.Errorf("transcript carries no truncation marker:\n%s", excerptLines(splitLines(raw)))
	}
	if strings.Contains(raw, strings.Repeat("x", 256)) {
		t.Error("transcript retains a fragment of the oversized line")
	}
}

// excerptLines renders transcript lines for a failure message without the
// message becoming the transcript.
func excerptLines(lines []string) string {
	var b strings.Builder
	for _, line := range lines {
		if len(line) > 160 {
			line = line[:160] + "…"
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// Silence past the stall window → failed(stalled), and the adapter — which owns
// liveness — kills what it declared dead.
func testStallTimeout(t *testing.T, c Contract) {
	h := c.start(t, c.runner(t, scriptSilent, nil, Options{}), c.spec(t, core.RunLimits{
		StallTimeout:   150 * time.Millisecond,
		AttemptTimeout: 30 * time.Second,
	}))
	got := terminal(t, collect(t, h))
	if got.Type != core.EventFailed || got.Reason != core.FailureStalled {
		t.Errorf("terminal = %+v, want failed(stalled)", got)
	}
	waitDone(t, h)
}

func testAttemptTimeout(t *testing.T, c Contract) {
	h := c.start(t, c.runner(t, scriptSilent, nil, Options{}),
		c.spec(t, core.RunLimits{AttemptTimeout: 150 * time.Millisecond}))
	got := terminal(t, collect(t, h))
	if got.Type != core.EventFailed || got.Reason != core.FailureTimeout {
		t.Errorf("terminal = %+v, want failed(timeout)", got)
	}
	waitDone(t, h)
}

// A run that keeps talking must not trip the stall window: every raw line is
// activity that resets it (SPEC §7.2).
func testActivityResetsStall(t *testing.T, c Contract) {
	// The success script emits its lines with no delay; a stall window this
	// short would fire between them if lines did not reset it.
	h := c.start(t, c.runner(t, scriptSuccess, nil, Options{}),
		c.spec(t, core.RunLimits{StallTimeout: 2 * time.Second}))
	if got := terminal(t, collect(t, h)); got.Type != core.EventSucceeded {
		t.Errorf("terminal = %+v, want succeeded", got)
	}
}

// Regression: the timeouts used to live in the same goroutine that delivers
// events, so a consumer that stopped draining could hold a run alive past its
// hard limit. Liveness is the adapter's obligation regardless of who is reading.
func testTimeoutWithoutConsumer(t *testing.T, c Contract) {
	// The harness emits well past the event buffer and never terminates, so the
	// pump is certain to be blocked on a full channel when the timer fires.
	rec := &recordingTranscript{}
	r := c.runner(t, scriptChatty, nil, Options{Transcripts: rec})
	h, err := r.Start(context.Background(), c.spec(t, core.RunLimits{AttemptTimeout: 150 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })

	// Deliberately never read h.Events().
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("attempt timeout never killed the process while events went undrained")
	}

	// An undrained consumer parks the pump, and everything the pump owns stops
	// happening with it — which used to include the post-exit bound on the
	// pipes, so the reader goroutines stayed parked behind it and the transcript
	// was never finished. Done must not paper over that with a timeout: it means
	// the record is complete (SPEC §10.3), and here it is asserted in the one
	// situation where nothing else would force it.
	if !rec.isClosed() {
		t.Error("transcript not closed when Done closed, with a consumer that never drained")
	}

	// The verdict is still delivered once a consumer returns.
	if got := terminal(t, collect(t, h)); got.Reason != core.FailureTimeout {
		t.Errorf("terminal = %+v, want failed(timeout)", got)
	}
}

// The lifecycle windows the suite injects are actually the ones in force.
//
// Nothing else here would notice if they were not. Every other test asserts what
// happens once a window closes, so all of them pass under the production
// defaults too — the only symptom is `make check` growing by ~20s, which is not a
// failure anyone is shown. So a `Timings` that stopped being forwarded, by an
// adapter, by harness.Launch, or by the suite's own defaults, would restore the
// seconds-wide windows silently. That is the same shape as an unasserted fix: the
// value is right and nothing fails when it changes back.
//
// One window is enough to prove the chain, because Options.Timings travels as a
// whole struct — adapter Options → harness.Launch → the handle — so a field that
// arrives means all of them did. It is deliberately the suite's *default* rather
// than a value pinned here, so dropping that default fails this test too.
//
// The setup is testTranscriptWithoutConsumer's, for the same reason it works
// there: a run nobody drains parks the pump, and only the post-exit drain
// elapsing releases the readers and finishes the record. That makes the window
// the dominant term in the time to Done, and the production default 14× the
// injected one.
func testInjectedTimingsReachTheRun(t *testing.T, c Contract) {
	r := c.runner(t, scriptManyLines, nil, Options{})
	spec := c.spec(t, core.RunLimits{AttemptTimeout: 150 * time.Millisecond})

	start := time.Now()
	h, err := r.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })

	// Deliberately never read h.Events().
	waitDone(t, h)
	elapsed := time.Since(start)

	production := harness.DefaultTimings().PostExitDrain
	if elapsed >= production {
		t.Errorf("an undrained run reached Done in %v, at or past the production post-exit "+
			"drain of %v: the suite injects %v (suiteTimings), so its Timings are not reaching "+
			"the run — check the adapter's Options.Timings → harness.Launch.Timings wiring",
			elapsed, production, suiteTimings(harness.Timings{}).PostExitDrain)
	}
}

// The transcript is complete even when nobody reads a single event.
//
// The two obligations are separate: events go to a consumer, the raw stream goes
// to the record (SPEC §7.2, §10.3). A consumer that stops draining parks the
// event side, and everything downstream of that used to stop with it — first the
// record was never closed, then it was closed but cut off at whatever line the
// reader was holding. Neither is a forensic record: an operator reading it after
// a failed run cannot tell a truncated file from a harness that stopped talking.
//
// So the assertion is on *content*, not on the file existing: every line the
// harness wrote before it was killed must be there, including the last.
func testTranscriptWithoutConsumer(t *testing.T, c Contract) {
	rec := &recordingTranscript{}
	r := c.runner(t, scriptManyLines, nil, Options{Transcripts: rec})
	h, err := r.Start(context.Background(), c.spec(t, core.RunLimits{AttemptTimeout: 150 * time.Millisecond}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })

	// Deliberately never read h.Events(): the harness outruns any event buffer,
	// so the reader is parked mid-hand-off long before the run ends.
	waitDone(t, h)

	if !rec.isClosed() {
		t.Fatal("transcript not closed when Done closed")
	}
	got := rec.text()
	for _, want := range []string{"line 0", fmt.Sprintf("line %d", manyLines-1)} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript is missing %q: %d lines, %d bytes",
				want, strings.Count(got, "\n"), len(got))
		}
	}
}

// Regression: liveness is runner-owned (SPEC §7.4), so a terminal line already
// in flight must not overturn a timeout the adapter has declared and acted on.
// Otherwise the hard limit is decided by a race — it reported `succeeded`
// whenever the harness finished inside the kill window.
func testLateSuccessLoses(t *testing.T, c Contract) {
	h := c.start(t, c.runner(t, scriptLateSuccess, nil, Options{}),
		c.spec(t, core.RunLimits{AttemptTimeout: 100 * time.Millisecond}))
	got := terminal(t, collect(t, h))
	if got.Type != core.EventFailed || got.Reason != core.FailureTimeout {
		t.Errorf("terminal = %+v, want failed(timeout) — a late result overturned the timeout", got)
	}
	waitDone(t, h)
}

// The same precedence when the verdict is claimed long before publication: the
// harness declares success ~250ms in, the attempt window closes at 100ms, so
// the claim is already standing when the terminal line arrives.
//
// The narrower version of this — a verdict claimed in the very instant the
// outcome is being chosen — is not here, because against a real process it is a
// window nanoseconds wide that no script can aim at. It is asserted where it is
// decided instead: harness/lifecycle_test.go drives that ordering, and every
// other ordering of the same facts, as a pure transition.
func testVerdictSurvivesCleanup(t *testing.T, c Contract) {
	h := c.start(t, c.runner(t, scriptLateSuccess, nil,
		Options{Timings: harness.Timings{StopGrace: 300 * time.Millisecond}}),
		c.spec(t, core.RunLimits{AttemptTimeout: 100 * time.Millisecond}))
	got := terminal(t, collect(t, h))
	if got.Type != core.EventFailed || got.Reason != core.FailureTimeout {
		t.Errorf("terminal = %+v, want failed(timeout)", got)
	}
	waitDone(t, h)
}

// A cancelled context is the daemon shutting down (SPEC §11): the run is
// discarded and reported killed, which is not retryable.
func testContextCancel(t *testing.T, c Contract) {
	ctx, cancel := context.WithCancel(context.Background())
	h, err := c.runner(t, scriptSilent, nil, Options{}).Start(ctx, c.spec(t, core.RunLimits{}))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	got := terminal(t, collect(t, h))
	if got.Type != core.EventFailed || got.Reason != core.FailureKilled {
		t.Errorf("terminal = %+v, want failed(killed)", got)
	}
	if got.Reason.Retryable() {
		t.Error("killed must not be retryable")
	}
	waitDone(t, h)
}

// --- process discipline (SPEC §7.5, §9.8) ---

// A stubborn child that ignores SIGTERM is escalated to SIGKILL and reported
// confirmed.
func testStopEscalates(t *testing.T, c Contract) {
	var mu sync.Mutex
	var sent []syscall.Signal
	r := c.runner(t, scriptStubborn, nil, Options{
		Signal: func(pgid int, sig syscall.Signal) error {
			if sig != 0 { // signal 0 is the existence probe, not part of the ladder
				mu.Lock()
				sent = append(sent, sig)
				mu.Unlock()
			}
			return syscall.Kill(-pgid, sig)
		},
	})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))

	// Wait for the harness to be up before stopping it.
	if ev := <-h.Events(); ev.Type != core.EventStarted {
		t.Fatalf("first event = %+v, want started", ev)
	}
	// Keep draining so Stop is not measuring a blocked consumer. No assertions
	// here: this is not the test goroutine.
	go func() {
		for range h.Events() {
		}
	}()

	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationConfirmed {
		t.Errorf("Stop = %v, want confirmed", got)
	}
	mu.Lock()
	ladder := slices.Clone(sent)
	mu.Unlock()
	if len(ladder) != 2 || ladder[0] != syscall.SIGTERM || ladder[1] != syscall.SIGKILL {
		t.Errorf("signal ladder = %v, want [SIGTERM SIGKILL]", ladder)
	}
	waitDone(t, h)
}

// Probe answers the group question without acting on it (SPEC §7.5, #79): the
// only signal it may send is the existence check, and whatever the process writes
// afterwards must still reach the transcript.
//
// Both halves matter. The orchestrator asks this the instant a run's event stream
// closes, when the process may still be flushing — so a Probe that walked the
// ladder, or aborted the stream, would truncate §7.2's record over a process
// about to exit on its own.
func testProbeObservesOnly(t *testing.T, c Contract) {
	dir := t.TempDir()
	var mu sync.Mutex
	var sent []syscall.Signal
	r := c.runner(t, scriptStopThenWrite, nil, Options{
		Transcripts: harness.DirTranscripts{Dir: filepath.Join(dir, "transcripts")},
		Signal: func(_ int, sig syscall.Signal) error {
			mu.Lock()
			sent = append(sent, sig)
			mu.Unlock()
			return nil // the group never dies, so the answer must be unconfirmed
		},
	})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	if ev := <-h.Events(); ev.Type != core.EventStarted {
		t.Fatalf("first event = %+v, want started", ev)
	}

	if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
		t.Errorf("Probe = %v, want unconfirmed while the group is alive", got)
	}
	mu.Lock()
	observed := slices.Clone(sent)
	mu.Unlock()
	for _, sig := range observed {
		if sig != 0 {
			t.Errorf("Probe sent %v; it may send nothing but the existence check", observed)
			break
		}
	}

	collect(t, h)
	waitDone(t, h)
	raw := onlyTranscript(t, filepath.Join(dir, "transcripts"))
	if !strings.Contains(raw, stopSurvivorMarker) {
		t.Errorf("the line written after the probe is missing from the transcript:\n%s", raw)
	}

}

// Not having looked is not evidence of anything (SPEC §7.5, §9.8). A probe whose
// context is already cancelled reports unconfirmed even where the honest answer
// would have been confirmed — the same fail-closed direction that makes EPERM
// unconfirmed.
//
// No signal stub here on purpose: the group really is gone, so the confirmed
// baseline is the kernel's answer rather than a fixture's.
func testProbeWithoutLookingIsUnconfirmed(t *testing.T, c Contract) {
	r := c.runner(t, scriptSuccess, nil, Options{})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	collect(t, h)
	waitDone(t, h)

	if got := h.Probe(context.Background()); got != core.TerminationConfirmed {
		t.Fatalf("Probe after the process exited = %v, want confirmed", got)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := h.Probe(cancelled); got != core.TerminationUnconfirmed {
		t.Errorf("Probe with a cancelled context = %v, want unconfirmed", got)
	}
}

// An unconfirmed StopInterrupt must leave the stream alone too (#79, A1). Abort
// is discard's, and only discard's: aborting here closes the output pipes, which
// throws away whatever a surviving process writes from then on — trading §7.2's
// verbatim record for an answer the caller already has.
func testUnconfirmedInterruptKeepsWriting(t *testing.T, c Contract) {
	dir := t.TempDir()
	r := c.runner(t, scriptStopThenWrite, nil, Options{
		Transcripts: harness.DirTranscripts{Dir: filepath.Join(dir, "transcripts")},
		Timings:     harness.Timings{StopGrace: 100 * time.Millisecond},
		Signal:      func(int, syscall.Signal) error { return nil },
	})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	if ev := <-h.Events(); ev.Type != core.EventStarted {
		t.Fatalf("first event = %+v, want started", ev)
	}
	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationUnconfirmed {
		t.Fatalf("Stop = %v, want unconfirmed", got)
	}

	// The process is still there and still writing; the record must be whole
	// when it finally ends.
	collect(t, h)
	waitDone(t, h)
	raw := onlyTranscript(t, filepath.Join(dir, "transcripts"))
	if !strings.Contains(raw, stopSurvivorMarker) {
		t.Errorf("the line written after the unconfirmed stop is missing from the transcript:\n%s", raw)
	}
}

// After Done, a group that is still there has outlived the process that owned
// it, and only a signal will clear it — which is why the orchestrator's question
// becomes Stop at that edge rather than staying a Probe (#79).
func testStopCleansAfterDone(t *testing.T, c Contract) {
	r := c.runner(t, scriptGrandchild, nil, Options{Timings: harness.Timings{StopGrace: 200 * time.Millisecond}})
	h := c.start(t, r, c.spec(t, core.RunLimits{StallTimeout: 300 * time.Millisecond}))
	collect(t, h)
	waitDone(t, h)

	// The grandchild ignores SIGTERM but not SIGKILL, so the ladder that a Probe
	// deliberately does not walk is what ends it.
	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationConfirmed {
		t.Errorf("Stop after Done = %v, want confirmed: the ladder is what cleans a surviving group", got)
	}
	if got := h.Probe(context.Background()); got != core.TerminationConfirmed {
		t.Errorf("Probe after the cleanup = %v, want confirmed", got)
	}
}

// An unkillable (simulated) child reports unconfirmed, which is what makes the
// orchestrator retain the claim (SPEC §9.8). The kernel's cooperation is what
// the stub withholds; the timing, ladder, and verdict are the adapter's real
// code.
func testStopUnconfirmed(t *testing.T, c Contract) {
	r := c.runner(t, scriptSurvivor, nil, Options{
		// Every signal is swallowed, including the existence probe, which then
		// reports the group as alive: the kernel refuses to help.
		Timings: harness.Timings{StopGrace: 100 * time.Millisecond},
		Signal:  func(int, syscall.Signal) error { return nil },
	})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	if ev := <-h.Events(); ev.Type != core.EventStarted {
		t.Fatalf("first event = %+v, want started", ev)
	}
	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationUnconfirmed {
		t.Errorf("Stop = %v, want unconfirmed", got)
	}
	// The survivor exits on its own, so the test leaves nothing behind.
	waitDone(t, h)
}

// In the process-backed test domain, only ESRCH proves its group gone. EPERM
// says the opposite — it exists and we may not signal it — and reading any
// error as "gone" reports confirmed termination over a live process.
func testStopProbeDenied(t *testing.T, c Contract) {
	r := c.runner(t, scriptSurvivor, nil, Options{
		Timings: harness.Timings{StopGrace: 100 * time.Millisecond},
		Signal: func(_ int, sig syscall.Signal) error {
			if sig == 0 {
				return syscall.EPERM // exists, but not ours to signal
			}
			return nil // signals go nowhere
		},
	})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	if ev := <-h.Events(); ev.Type != core.EventStarted {
		t.Fatalf("first event = %+v, want started", ev)
	}
	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationUnconfirmed {
		t.Errorf("Stop = %v, want unconfirmed: EPERM means the group is still there", got)
	}
	waitDone(t, h)
}

// A liveness kill whose process group survives SIGKILL: the §7.4 outcome is
// still published, and the group is still reported honestly when asked.
//
// Two owners, and separating them is the whole case. §7.4 owns the *outcome*:
// silence past the stall window is `failed(stalled)` whatever the kernel did
// about the process, and a run that withheld its terminal event would leave the
// orchestrator with nothing to act on — which is why the outcome is published
// here regardless of what the ladder concluded. §7.5 owns the *group*, and
// answers only through `Stop`. That answer is what the orchestrator asks for
// before it lets anything touch the workspace, so an unconfirmed one here is
// what retains the claim (SPEC §9.8) instead of retrying a replacement into a
// worktree that still has an agent process in it.
//
// The kernel's cooperation is what the stub withholds, as in
// testStopUnconfirmed; the windows, the ladder and the verdict are the adapter's
// real code.
func testLivenessKillUnconfirmed(t *testing.T, c Contract, limits core.RunLimits, want core.FailureReason) {
	t.Helper()
	var mu sync.Mutex
	var sent []syscall.Signal
	r := c.runner(t, scriptSurvivor, nil, Options{
		Timings: harness.Timings{StopGrace: 100 * time.Millisecond},
		Signal: func(_ int, sig syscall.Signal) error {
			if sig == 0 {
				// The existence probe: answering means the group is alive.
				return nil
			}
			mu.Lock()
			sent = append(sent, sig)
			mu.Unlock()
			return nil // and the signals themselves go nowhere
		},
	})
	h := c.start(t, r, c.spec(t, limits))

	if got := terminal(t, collect(t, h)); got.Type != core.EventFailed || got.Reason != want {
		t.Errorf("terminal = %+v, want failed(%s): the outcome is §7.4's, not the kernel's", got, want)
	}
	// Snapshot before the Stop below, so this is the expiry's ladder rather than
	// that call's. Liveness must walk it for the *group*: an expiry that only
	// published a verdict would leave the agent running in the workspace, and
	// every assertion after this one would pass anyway.
	mu.Lock()
	ladder := slices.Clone(sent)
	mu.Unlock()
	if !slices.Equal(ladder, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
		t.Errorf("liveness ladder = %v, want [SIGTERM SIGKILL]", ladder)
	}
	// Done closes even though the group never did — which is exactly why the
	// orchestrator does not wait for it to ask about the group: it observes as
	// soon as the event stream closes and only escalates to a signal after this
	// edge (SPEC §7.5, §9.8; #79). An undecided run holds a claim and a §9.5
	// concurrency slot until that question comes back confirmed.
	waitDone(t, h)

	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationUnconfirmed {
		t.Errorf("Stop after an unconfirmed liveness kill = %v, want unconfirmed: this answer "+
			"alone is what retains the claim (SPEC §7.5, §9.8)", got)
	}
}

func testStallKillUnconfirmed(t *testing.T, c Contract) {
	testLivenessKillUnconfirmed(t, c,
		core.RunLimits{StallTimeout: 150 * time.Millisecond}, core.FailureStalled)
}

func testTimeoutKillUnconfirmed(t *testing.T, c Contract) {
	testLivenessKillUnconfirmed(t, c,
		core.RunLimits{AttemptTimeout: 150 * time.Millisecond}, core.FailureTimeout)
}

func testStopAfterExit(t *testing.T, c Contract) {
	h := c.start(t, c.runner(t, scriptSuccess, nil, Options{}), c.spec(t, core.RunLimits{}))
	collect(t, h)
	waitDone(t, h)
	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationConfirmed {
		t.Errorf("Stop after exit = %v, want confirmed", got)
	}
}

// The terminal event is ground truth: a harness that declares success and then
// lingers must not hold the workspace open (SPEC §7.4).
func testKilledAfterTerminal(t *testing.T, c Contract) {
	h := c.start(t, c.runner(t, scriptLinger, nil, Options{}), c.spec(t, core.RunLimits{}))
	if got := terminal(t, collect(t, h)); got.Type != core.EventSucceeded {
		t.Errorf("terminal = %+v, want succeeded", got)
	}
	waitDone(t, h)
}

// The process-backed contract domain's default must address the process group,
// not only its leader. This is deliberately independent of
// testDescendantsDieFirst: that case wraps SignalGroup to order a liveness
// ladder behind the fixture's registration, so it proves the wrapped sender
// rather than the nil Signal path Start binds in production.
//
// Here no signal hook is supplied. Stop is ordered after the same explicit
// registration instead, and its confirmed answer is not enough on its own: a
// default mutated to probe and signal only pgid reports the dead leader as a
// confirmed cleanup while leaving this SIGTERM-immune descendant alive. The
// pid assertion is the independent boundary that catches that false answer.
func testDefaultSignalReachesDescendants(t *testing.T, c Contract) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	r := c.runner(t, scriptGrandchild, map[string]string{PIDEnv: pidFile}, Options{})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))

	pid := readPID(t, pidFile)
	defer syscall.Kill(pid, syscall.SIGKILL) // never leak it, whatever the outcome
	if !aliveNow(pid) {
		t.Fatalf("grandchild %d was already gone before Stop: the default sender has nothing to prove", pid)
	}

	if got := h.Stop(context.Background(), core.StopInterrupt); got != core.TerminationConfirmed {
		t.Errorf("Stop = %v, want confirmed", got)
	}
	if aliveNow(pid) {
		t.Errorf("grandchild %d survived Stop: the test domain signalled only the leader", pid)
	}
	if got := terminal(t, collect(t, h)); got.Type != core.EventFailed || got.Reason != core.FailureKilled {
		t.Errorf("terminal = %+v, want failed(killed)", got)
	}
	waitDone(t, h)
}

// descendantStall is testDescendantsDieFirst's liveness window: short, because
// what the case is waiting for is the kill, and the descendant's registration is
// ordered rather than fitted inside it (see below).
const descendantStall = 150 * time.Millisecond

// Regression: the signal ladder must be driven by the process *group*, not the
// leader. A harness that spawned a tool which ignores SIGTERM used to survive a
// stall kill, because the leader's exit ended the ladder before SIGKILL.
//
// The assertions are deliberately made the instant the terminal event and Done
// arrive, with no polling: publication must *follow* group cleanup, so a grace
// period of "the grandchild dies shortly afterwards" is not good enough. An
// orchestrator that disposes a workspace on this signal would be racing a live
// process (SPEC §9.8).
//
// # The fixture's own precondition (#138)
//
// The case needs a descendant that is *already* SIGTERM-immune when the ladder
// starts, and that is a race the fixture has to win rather than assume. The
// descendant is a re-exec of this instrumented test binary, so it cannot protect
// itself before its own runtime has started, and it cannot inherit the
// protection either: a SIG_IGN carried across exec — how a C tool would arrive
// already immune — does not survive Go's start-up, which installs a handler for
// SIGTERM regardless (measured: a Go child of a parent holding SIG_IGN still
// ends as `signal: terminated`). Left to a wall clock, the stall window closed
// on that start-up under full-suite load, the group SIGTERM killed the
// descendant before its first instruction, and the case failed as a two-second
// wait for a pid file nothing would ever write.
//
// So the ladder is ordered behind the registration instead of raced against it:
// the Signal hook delays the real sender — it does not replace it — until the
// descendant has published its pid, which scriptGrandchildWork does only after
// its SIGTERM disposition is installed. Nothing about the ladder, the group, the
// escalation or the publication ordering is stubbed; only the moment the first
// signal leaves is. And the descendant's start-up is *deliberately* slower than
// the stall window (SlowStartEnv), so an ordering that regressed to a race fails
// this case every time instead of once a fortnight on a loaded machine.
func testDescendantsDieFirst(t *testing.T, c Contract) {
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")

	// Closed once the descendant is registered; the ladder waits on it. A
	// cleanup releases it too, so a failed assertion before the release cannot
	// strand a process group in a blocked ladder.
	registered := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(registered) }) }
	t.Cleanup(release)

	// A grace long enough that the window would be obvious if publication did
	// not wait for the ladder: the grandchild only dies at SIGKILL, one grace in.
	r := c.runner(t, scriptGrandchild, map[string]string{
		PIDEnv: pidFile,
		// Twice the window below, stated as a multiple of it: the point is that
		// registration finishes *after* the stall kill is due, and two constants
		// that had to be compared by hand would drift apart.
		SlowStartEnv: (2 * descendantStall).String(),
	}, Options{
		Timings: harness.Timings{StopGrace: 2 * time.Second},
		Signal: func(pgid int, sig syscall.Signal) error {
			<-registered
			return SignalGroup(pgid, sig)
		},
	})
	h := c.start(t, r, c.spec(t, core.RunLimits{StallTimeout: descendantStall}))

	pid := readPID(t, pidFile)
	defer syscall.Kill(pid, syscall.SIGKILL) // never leak it, whatever the outcome

	// The precondition, asserted rather than assumed: the ladder is about to run
	// against a live, SIGTERM-immune descendant. Nothing has signalled the group
	// yet — that is what the gate is holding — so a dead one here means the
	// fixture lost its own race again, and every assertion below would hold over
	// a group that had nothing left in it (#138).
	if !aliveNow(pid) {
		t.Fatalf("grandchild %d was already gone before the ladder started: "+
			"this case would pass without testing what it exists for", pid)
	}
	release()

	// Liveness is sampled at the two moments the run becomes visible to the
	// orchestrator, not afterwards: Done and the terminal event are published
	// independently, so each gets its own snapshot.
	var terminalEvent core.Event
	aliveAtTerminal := false
	for ev := range h.Events() {
		if harness.IsTerminal(ev.Type) {
			aliveAtTerminal = aliveNow(pid)
			terminalEvent = ev
		}
	}
	waitDone(t, h)
	aliveAtDone := aliveNow(pid)

	if terminalEvent.Reason != core.FailureStalled {
		t.Fatalf("terminal = %+v, want failed(stalled)", terminalEvent)
	}
	if aliveAtTerminal {
		t.Errorf("grandchild %d was still alive when failed(stalled) was published", pid)
	}
	if aliveAtDone {
		t.Errorf("grandchild %d was still alive when Done closed", pid)
	}
}

// --- secrets and I/O (SPEC §7.6) ---

// The prompt goes in on stdin, and the process runs in the workspace.
func testPromptAndCwd(t *testing.T, c Contract) {
	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	r := c.runner(t, scriptEchoPrompt, map[string]string{DumpEnv: dumpPath}, Options{})
	spec := c.spec(t, core.RunLimits{})
	spec.Prompt = "PROMPT-BODY do the thing"
	h := c.start(t, r, spec)
	evs := collect(t, h)

	d := readDump(t, dumpPath)
	if d.Stdin != spec.Prompt {
		t.Errorf("harness stdin = %q, want %q", d.Stdin, spec.Prompt)
	}
	// Symlinked temp dirs (macOS /var → /private/var) make an exact compare
	// unreliable; resolve both.
	if got, want := resolve(d.Cwd), resolve(spec.Workspace.Path); got != want {
		t.Errorf("harness cwd = %q, want %q", got, want)
	}
	// The prompt made it all the way through: the harness echoed it back.
	if evs[1].Text != spec.Prompt {
		t.Errorf("echoed prompt = %q, want %q", evs[1].Text, spec.Prompt)
	}
}

// The acceptance criterion: `ps`-visible argv and the child env audited — no
// secret material, only the allowlist plus injected vars (SPEC §7.6).
func testArgvAndEnvAudit(t *testing.T, c Contract) {
	t.Setenv("PATH", os.Getenv("PATH")) // allowlisted, needed by the child
	t.Setenv("AWS_SECRET_ACCESS_KEY", "daemon-aws-MUST-NOT-LEAK")
	for _, cred := range c.Credentials {
		t.Setenv(cred.Env, "daemon-credential-MUST-NOT-LEAK")
	}
	t.Setenv("BEN_UNRELATED", "daemon-unrelated")
	// The §10.2 *tracker* credential, which the daemon holds and an agent must
	// never see (#47). Named on its own line rather than left to the exhaustive
	// sweep below, because of the four secrets here this is the one whose leak
	// lets a subverted run rewrite the queue that dispatched it — strip `ben:*`,
	// take the assignment, close the issue, claim more work. The literal is the
	// GitHub adapter's documented fallback variable; naming it through that
	// package would point an agent-side suite at a tracker.
	t.Setenv("GITHUB_TOKEN", "tracker-credential-MUST-NOT-LEAK")

	const (
		credential = "provider-credential-SECRET"
		publish    = "gh-PROVIDER-SECRET"
		prompt     = "PROMPT-BODY-SECRETISH"
	)
	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	block := c.block(t, scriptSuccess, map[string]string{DumpEnv: dumpPath, "GH_TOKEN": publish})
	// Every credential key the adapter declares, each with a value of its own:
	// one shared literal could not tell which variable carried which.
	credentials := map[string]string{} // child env var -> the value it must carry
	for i, cred := range c.Credentials {
		v := fmt.Sprintf("%s-%d", credential, i)
		block[cred.Key] = v
		credentials[cred.Env] = v
	}

	spec := c.spec(t, core.RunLimits{})
	spec.Env = map[string]string{"BEN_RUN_ID": "run-7"}
	spec.Prompt = prompt

	h := c.start(t, c.newRunner(t, block, Options{}), spec)
	collect(t, h)

	d := readDump(t, dumpPath)
	argv := strings.Join(d.Argv, "\x00")
	secrets := append([]string{publish, prompt, "run-7"}, slices.Collect(maps.Values(credentials))...)
	for _, secret := range secrets {
		if strings.Contains(argv, secret) {
			t.Errorf("argv leaks %q: %v", secret, d.Argv)
		}
	}

	env := envMap(d.Env)
	// The adapter's documented auth surface carries the *provider's* value, not
	// the daemon's (SPEC §7.6, §7.7).
	for name, want := range credentials {
		if env[name] != want {
			t.Errorf("%s = %q, want the provider value %q", name, env[name], want)
		}
	}
	// The orchestrator's own variables arrive, and they are the only thing it
	// may contribute.
	if env["BEN_RUN_ID"] != "run-7" {
		t.Errorf("BEN_RUN_ID = %q, want the RunSpec value", env["BEN_RUN_ID"])
	}
	// Everything in the child env is either allowlisted, opted into, or
	// injected — nothing arrives just because the daemon had it.
	injected := map[string]bool{
		"GH_TOKEN": true, "BEN_RUN_ID": true,
		ScriptEnv: true, DumpEnv: true,
	}
	for name := range credentials {
		injected[name] = true
	}
	// The adapter's non-credential injections (Contract.OwnedDirs). They are
	// exempt from "neither allowlisted nor injected" because the adapter put
	// them there, not the daemon — but only from that rule: the secret-value
	// sweep below still reads every value in this environment, so a directory
	// variable carrying a credential is caught by the check that does not
	// consult any declaration.
	for _, name := range c.OwnedDirs {
		injected[name] = true
	}
	for name := range env {
		if injected[name] || slices.Contains(core.EnvAllowlist, name) {
			continue
		}
		t.Errorf("child env carries %q, which is neither allowlisted nor injected", name)
	}
	for _, leaked := range []string{"AWS_SECRET_ACCESS_KEY", "BEN_UNRELATED", "GITHUB_TOKEN"} {
		if _, ok := env[leaked]; ok {
			t.Errorf("child inherited %q from the daemon", leaked)
		}
	}
	// And the *values*, not just the names. A variable's absence from the child
	// environment says nothing about whether its secret arrived under some other
	// name, or in argv: the leaks §10.2 is about are renames — `--model
	// <tracker-pat>`, or `GH_TOKEN` carrying the tracker's secret — and a check on
	// key presence alone passes straight through every one of them.
	for _, secret := range []string{"tracker-credential-MUST-NOT-LEAK", "daemon-aws-MUST-NOT-LEAK"} {
		if strings.Contains(argv, secret) {
			t.Errorf("argv carries the daemon secret %q: %v", secret, d.Argv)
		}
		for name, value := range env {
			if strings.Contains(value, secret) {
				t.Errorf("child env %s carries the daemon secret %q", name, secret)
			}
		}
	}
}

// --- the publish credential (SPEC §5.2.8) ---

// publishVar is the daemon-environment variable the publish tests read the
// credential from. Deliberately not `GH_TOKEN`: that is the *child* variable
// `publish.env` names, and using one name for both would let a test pass on an
// adapter that forwarded the daemon's variable by name instead of injecting the
// resolved value under the configured one — which is exactly the rename §10.2
// says the check must be able to see.
const publishVar = "BEN_TEST_PUBLISH_TOKEN"

// publishCredential is the reference an operator's `publish` block resolves to.
const publishChildVar = "GH_TOKEN"

func publishCredential() core.PublishCredential {
	return core.PublishCredential{Env: publishChildVar, Var: publishVar}
}

// publishBinding is what the reference above compiles into: the same child
// variable, and an implicit `static` source over the same daemon variable
// (SPEC §8, amendment 9). Every adapter sees one runtime treatment, so the
// suite binds one too.
func publishBinding() core.PublishBinding {
	return core.PublishBinding{Env: publishChildVar, Source: EnvSource(publishVar)}
}

// The publish credential reaches the child under the variable `publish.env`
// names, carries the value of the variable `publish.value` names, and never
// touches argv (SPEC §5.2.8, §7.6).
//
// The two variables differ on purpose, so this asserts an *injection* rather than
// a passthrough: an adapter that forwarded publishVar by name would put the secret
// in the child under the wrong key, and `gh` would not read it.
func testPublishReachesChild(t *testing.T, c Contract) {
	t.Setenv("PATH", os.Getenv("PATH"))
	const secret = "ghp-publish-canary-0123456789"
	t.Setenv(publishVar, secret)

	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	block := c.block(t, scriptSuccess, map[string]string{DumpEnv: dumpPath})

	h := c.start(t, c.newRunner(t, block, Options{Publish: publishBinding()}), c.spec(t, core.RunLimits{}))
	collect(t, h)

	d := readDump(t, dumpPath)
	if got := envMap(d.Env)[publishChildVar]; got != secret {
		t.Errorf("child %s = %q, want the resolved publish credential", publishChildVar, got)
	}
	if _, leaked := envMap(d.Env)[publishVar]; leaked {
		t.Errorf("the daemon's %s reached the child by name; the credential is injected under %s",
			publishVar, publishChildVar)
	}
	if argv := strings.Join(d.Argv, "\x00"); strings.Contains(argv, secret) {
		t.Errorf("argv leaks the publish credential: %v", d.Argv)
	}
}

// The publish credential is redacted from a retained transcript (SPEC §10.3).
//
// No declaration holds this value — it is not in the provider block, so
// SensitiveFields cannot report it and CredentialValues cannot find it. It reaches
// the redaction set only because Environ returns what it resolved, which is the
// property this asserts through the real sink rather than through needles().
func testPublishRedaction(t *testing.T, c Contract) {
	const secret = "ghp-publish-transcript-canary-0123456789"
	t.Setenv(publishVar, secret)

	dir := t.TempDir()
	block := c.block(t, scriptEchoEnv, nil)
	const survivor = "run-canary-must-survive"
	spec := c.spec(t, core.RunLimits{})
	spec.Env = map[string]string{"BEN_RUN_ID": survivor}

	// Into the prompt as well: the retained prompt (SPEC §9.5) is the other
	// §10.3 artifact this run leaves on disk, and the publish credential reaches
	// the redaction set from Environ rather than from any declaration — so the
	// two files have to be covered by the same resolution, not by two.
	spec.Prompt = "do the thing\n" + survivor + "\n" + secret + "\n"

	h := c.start(t, c.newRunner(t, block, Options{
		Transcripts: harness.DirTranscripts{Dir: dir},
		Publish:     publishBinding(),
	}), spec)
	collect(t, h)
	waitDone(t, h)
	raw := onlyTranscript(t, dir)
	retainedPrompt := onlyPrompt(t, dir)
	if !strings.Contains(retainedPrompt, survivor) {
		t.Fatalf("the retained prompt is missing the run coordinate, so its absences prove nothing:\n%s", retainedPrompt)
	}
	if strings.Contains(retainedPrompt, secret) {
		t.Errorf("the retained prompt leaks the publish credential:\n%s", retainedPrompt)
	}

	// Non-vacuity first: the agent echoed its environment, so an absent canary
	// means redacted rather than never written.
	for _, want := range []string{"PATH=", survivor, publishChildVar + "="} {
		if !strings.Contains(raw, want) {
			t.Fatalf("transcript is missing %q, so its absences prove nothing:\n%s", want, raw)
		}
	}
	if strings.Contains(raw, secret) {
		t.Errorf("transcript carries the publish credential:\n%s", raw)
	}
}

// One child variable, one owning config site — refused in both directions
// (SPEC §5.2.8, §7.6).
//
// Every row is a configuration where two sites write one variable, so whichever
// wins, the other is silently doing nothing and which one loses is decided by
// composition order. The reverse direction is driven off c.Credentials, so an
// adapter that adds a credential variable is covered here without editing this
// test.
func testPublishReservation(t *testing.T, c Contract) {
	cred := publishCredential()

	t.Run("env respells the publish variable", func(t *testing.T) {
		block := c.Block("harness", map[string]string{publishChildVar: "second-source"})
		assertStructuralAndNew(t, c, core.AgentConfig{Provider: block, Publish: cred}, c.Errors.EnvReserved)
	})
	t.Run("env_passthrough respells the publish variable", func(t *testing.T) {
		block := c.Block("harness", nil)
		block["env_passthrough"] = []any{publishChildVar}
		assertStructuralAndNew(t, c, core.AgentConfig{Provider: block, Publish: cred}, c.Errors.EnvReserved)
	})
	for _, adapterOwned := range c.Credentials {
		t.Run("publish.env names "+adapterOwned.Env, func(t *testing.T) {
			block := c.Block("harness", nil)
			owned := core.PublishCredential{Env: adapterOwned.Env, Var: publishVar}
			assertStructuralAndNew(t, c, core.AgentConfig{Provider: block, Publish: owned}, c.Errors.EnvReserved)
		})
	}
	// The control: a publish credential naming a variable nobody owns is the
	// ordinary case, and must not be refused.
	t.Run("an unowned child variable is fine", func(t *testing.T) {
		block := c.Block("harness", nil)
		if err := c.Kind.Structural(core.AgentConfig{Provider: block, Publish: cred}); err != nil {
			t.Errorf("Structural = %v, want ok", err)
		}
	})
}

// assertStructuralAndNew asserts one configuration is refused by both entry
// points with the same named error.
//
// Both, because New must parse through the path Structural does (SPEC §5.7): a
// New that read only the provider block would construct a runner from exactly the
// configurations these rows describe, and the publish checks are the only ones the
// block alone cannot see.
func assertStructuralAndNew(t *testing.T, c Contract, cfg core.AgentConfig, want error) {
	t.Helper()
	if err := c.Kind.Structural(cfg); !errors.Is(err, want) {
		t.Errorf("Structural = %v, want %v", err, want)
	}
	r, err := c.Kind.New(core.RunnerOptions{
		Provider: cfg.Provider,
		Publish:  core.PublishBinding{Env: cfg.Publish.Env, Source: EnvSource(cfg.Publish.Var)},
	})
	if !errors.Is(err, want) {
		t.Errorf("New = %v, want %v", err, want)
	}
	if r != nil {
		t.Error("New returned a runner alongside an error")
	}
}

// A publish variable the host does not hold is a readiness refusal and a Start
// refusal, never a load one (SPEC §5.2.8, §5.5).
//
// Both, and for different reasons. Ready is where an operator is told once, at
// startup, instead of one dispatched issue at a time. Start is where a run that
// could not publish is refused before it costs a ticket's work — the failure mode
// an absent `env_passthrough` name used to buy silently, discovered at `git push`
// after everything else had succeeded.
func testPublishUnresolvable(t *testing.T, c Contract) {
	t.Setenv("PATH", os.Getenv("PATH"))
	t.Setenv(publishVar, "") // set-but-empty is missing (SPEC §5.5)

	r := c.runner(t, scriptSuccess, nil, Options{Publish: publishBinding()})
	if err := r.Ready(context.Background()); !errors.Is(err, c.Errors.PublishCredential) {
		t.Errorf("Ready = %v, want %v", err, c.Errors.PublishCredential)
	}
	h, err := r.Start(context.Background(), c.spec(t, core.RunLimits{}))
	if !errors.Is(err, c.Errors.PublishCredential) {
		t.Errorf("Start = %v, want %v", err, c.Errors.PublishCredential)
	}
	if h != nil {
		t.Error("Start returned a handle alongside an error")
	}
}

// The publisher's half of the no-fallback rule, asserted as an **absence at the
// runner seam**: after a mint failure — or an empty value — **the agent is never
// launched** (SPEC §10.2).
//
// Barriered on the refusal being routed rather than on a duration: Start returns
// the refusal, and the evidence sink is what a launch would have called. A run
// that started and then failed to publish is the failure mode this exists to
// prevent, and it is invisible to an assertion that only reads the error.
//
// Three shapes, because a source can fail in three ways and only one of them
// carries an error at all.
func testPublishNeverLaunches(t *testing.T, c Contract) {
	authority := "octo:https://octo.example#org#ben-publish"
	for _, tt := range []struct {
		name  string
		token core.Token
		err   error
		// wantClass is what must survive to the call site, since §9.8 routes on
		// it and a re-labelled verdict routes differently.
		wantClass core.CredentialErrorClass
	}{
		{
			name:      "a transient exchange failure",
			err:       &core.CredentialError{Class: core.CredentialTransient, Authority: authority, Err: errors.New("the issuer answered 503")},
			wantClass: core.CredentialTransient,
		},
		{
			name:      "a permanent exchange failure",
			err:       &core.CredentialError{Class: core.CredentialPermanent, Authority: authority, Err: errors.New("the trust policy does not admit this identity")},
			wantClass: core.CredentialPermanent,
		},
		{
			// A source that reported success with no credential. The refusal is
			// the boundary's, and it is permanent: a defective source is
			// defective again a moment later.
			name:      "a source that answered with nothing",
			wantClass: core.CredentialPermanent,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PATH", os.Getenv("PATH"))
			var launched atomic.Int64
			src := &ScriptedSource{
				Descriptor_: core.SourceDescriptor{Kind: "octo_sts", Authority: authority, BindingKey: authority + "#/run/oidc"},
				Fetch_:      func() (core.Token, error) { return tt.token, tt.err },
			}
			r := c.runner(t, scriptSuccess, nil, Options{
				Publish: core.PublishBinding{Env: publishChildVar, Source: src},
				OnRun: func(core.RunSpec, core.RunEvidence) error {
					launched.Add(1)
					return nil
				},
			})

			h, err := r.Start(context.Background(), c.spec(t, core.RunLimits{}))
			if !errors.Is(err, c.Errors.PublishCredential) {
				t.Fatalf("Start = %v, want %v", err, c.Errors.PublishCredential)
			}
			if h != nil {
				t.Error("Start returned a handle alongside an error")
			}
			if got := launched.Load(); got != 0 {
				t.Errorf("the run-evidence sink fired %d times; the agent was launched with no credential "+
					"to publish with, which is the whole ticket's work discovered at `git push`", got)
			}
			if got, ok := core.CredentialFailure(err); !ok || got != tt.wantClass {
				t.Errorf("class = (%v, %v), want %v", got, ok, tt.wantClass)
			}
			// Minted, never served from a cache: a token handed to an agent
			// must cover the whole attempt (SPEC §7.7).
			if src.Fresh == 0 {
				t.Error("the publisher never reached the source's fresh surface")
			}
		})
	}
}

// The attempt-lifetime gate is applied at Start as well as at Ready, and a
// deadline too short to cover the attempt is **permanent**: it is arithmetic,
// not weather (SPEC §7.7).
func testPublishTTLGate(t *testing.T, c Contract) {
	t.Setenv("PATH", os.Getenv("PATH"))
	authority := "octo:https://octo.example#org#ben-publish"
	src := &ScriptedSource{
		Descriptor_: core.SourceDescriptor{
			Kind: "octo_sts", Authority: authority, BindingKey: authority + "#/run/oidc",
			MinFreshTTL: 50 * time.Minute,
		},
		Fetch_: func() (core.Token, error) {
			return core.Token{Value: "ghs-too-short", UsableUntil: time.Now().Add(10 * time.Minute)}, nil
		},
	}
	r := c.runner(t, scriptSuccess, nil, Options{
		Publish:        core.PublishBinding{Env: publishChildVar, Source: src},
		AttemptTimeout: 45 * time.Minute,
	})

	if err := r.Ready(context.Background()); !errors.Is(err, core.ErrCredentialTTL) {
		t.Errorf("Ready = %v, want the TTL refusal", err)
	}
	h, err := r.Start(context.Background(), c.spec(t, core.RunLimits{}))
	if !errors.Is(err, core.ErrCredentialTTL) {
		t.Fatalf("Start = %v, want the TTL refusal", err)
	}
	if h != nil {
		t.Error("Start returned a handle alongside an error")
	}
	if class, _ := core.CredentialFailure(err); class != core.CredentialPermanent {
		t.Errorf("class = %v, want permanent", class)
	}

	// An unbounded source is not gated, however long the attempt is — which is
	// what keeps `static` and every legacy spelling valid.
	t.Setenv(publishVar, "ghp-unbounded")
	unbounded := c.runner(t, scriptSuccess, nil, Options{
		Publish:        publishBinding(),
		AttemptTimeout: 24 * time.Hour,
	})
	if err := unbounded.Ready(context.Background()); err != nil {
		t.Errorf("Ready with an unbounded credential = %v, want no gate", err)
	}
}

// A retained transcript must not carry a credential value (SPEC §10.3, §10.2).
//
// Driven off SensitiveFields, so a credential key added to an adapter is covered
// here without editing this test — and with Contract.Credentials a list, both of
// claude-code's keys are actually in the block for it to report.
//
// The agent echoes its whole environment on both streams, which is the leak this
// exists for and also the non-vacuity check: PATH proves the echo happened, so an
// absent canary means redacted rather than never written.
//
// Run against both sink kinds. The on-disk file is what §10.3 retains, and an
// injected store is what proves the wrap is in Start rather than in
// DirTranscripts. No injected store can be the thing trusted to redact.
func testTranscriptRedaction(t *testing.T, c Contract) {
	t.Run("retained file", func(t *testing.T) {
		dir := t.TempDir()
		transcriptRedaction(t, c, harness.DirTranscripts{Dir: dir},
			func() string { return onlyTranscript(t, dir) },
			func() string { return onlyPrompt(t, dir) })
	})
	t.Run("injected store", func(t *testing.T) {
		rec := &recordingTranscript{}
		transcriptRedaction(t, c, rec, rec.text, rec.prompt.String)
	})
}

func transcriptRedaction(t *testing.T, c Contract, store harness.TranscriptStore, read, readPrompt func() string) {
	// The canary no declaration can supply. SensitiveFields reports env_passthrough
	// *names* and must keep doing so — an operator debugging a bad passthrough has
	// to read them — so a set derived from it alone cannot see this value, and §7.6
	// calls env_passthrough the opt-in by which "a tracker PAT or an agent API key"
	// reaches a child. Asserted by name below rather than through the declaration
	// loop, which is the only way a test can cover what the declaration omits.
	const forwarded = "ghp-canary-forwarded-by-name-0123456789"
	t.Setenv("FORWARDED_PAT", forwarded)

	block := c.block(t, scriptEchoEnv, map[string]string{
		"GH_TOKEN": "ghp-canary-publish-0123456789",
		// A value carrying bytes JSON escapes. Both paths into the transcript
		// encode what they write — the Fake's emitters for stdout, the harness's
		// own marshalling for the ben:stderr tail — so this one reaches the file
		// escaped, and a redaction that only matches literals walks past it.
		"QUOTED_TOKEN": `ghp-canary-"quoted\\backslash"-0123456789`,
		// A credential with a newline in it: permitted by StringMap, and split
		// across two writes by readStdout, so neither write contains the value.
		"MULTILINE_TOKEN": "ghp-canary-first-half\nghp-canary-second-half",
	})
	for i, cred := range c.Credentials {
		block[cred.Key] = fmt.Sprintf("canary-credential-%s-%d", cred.Key, i)
	}
	block["env_passthrough"] = []any{"FORWARDED_PAT"}

	// A run coordinate, above the length floor, that must survive: it is in the
	// child environment but is not a credential. Widening the redaction set to
	// the whole environment fails here.
	const survivor = "run-canary-must-survive"
	spec := c.spec(t, core.RunLimits{})
	spec.Env = map[string]string{"BEN_RUN_ID": survivor}

	// The retained prompt (SPEC §9.5) is the other §10.3 artifact this run leaves
	// on disk, and it holds whatever the render put in it. A workflow whose
	// template quoted a credential — or an issue body that did — would otherwise
	// park a secret beside the transcript that carefully scrubbed it. Every canary
	// goes into the prompt so the same absences are asserted of both files.
	var prompt strings.Builder
	prompt.WriteString("do the thing\n")
	prompt.WriteString(survivor + "\n")
	for _, path := range c.Kind.SensitiveFields(block) {
		if value, ok := blockValue(block, path); ok {
			prompt.WriteString(value + "\n")
		}
	}
	prompt.WriteString(forwarded + "\n")
	spec.Prompt = prompt.String()

	h := c.start(t, c.newRunner(t, block, Options{Transcripts: store}), spec)
	evs := collect(t, h)
	waitDone(t, h)
	raw := read()
	retainedPrompt := readPrompt()

	// The retention happened at all, so its absences prove something.
	if !strings.Contains(retainedPrompt, survivor) {
		t.Fatalf("the retained prompt is missing the run coordinate, so its absences prove nothing:\n%s", retainedPrompt)
	}

	// The echo reached the transcript at all, and the stderr half did too.
	for _, want := range []string{"PATH=", survivor, `"type":"ben:stderr"`} {
		if !strings.Contains(raw, want) {
			t.Fatalf("transcript is missing %q, so its absences prove nothing:\n%s", want, raw)
		}
	}

	// Every event text the run produced. core.Event.Text is the third §10.3
	// artifact this run leaves behind: the orchestrator retains a bounded tail of
	// it and renders it into the next attempt's prompt (SPEC §9.6, #61), so it is
	// held to the same standard as the two files.
	//
	// The absences asserted of it below prove something because the echo reached
	// the events at all — established here, on a value that is in the child
	// environment and is not a credential.
	var texts strings.Builder
	for _, ev := range evs {
		texts.WriteString(ev.Text)
		texts.WriteByte('\n')
	}
	for _, want := range []string{"PATH=", survivor} {
		if !strings.Contains(texts.String(), want) {
			t.Fatalf("no event text carries %q, so their absences prove nothing:\n%s", want, texts.String())
		}
	}

	for _, path := range c.Kind.SensitiveFields(block) {
		value, ok := blockValue(block, path)
		if !ok {
			continue // a declared key this block does not set
		}
		name := strings.Join(path, ".")
		if strings.Contains(raw, value) {
			t.Errorf("transcript leaks %s = %q:\n%s", name, value, raw)
		}
		// The prompt is one Write, so even a multiline value is matched whole
		// here — a stronger guarantee than the line-oriented stream can give.
		if strings.Contains(retainedPrompt, value) {
			t.Errorf("the retained prompt leaks %s = %q:\n%s", name, value, retainedPrompt)
		}
		// The spelling the file actually carries, when the encoders changed it.
		if esc := jsonInner(t, value); esc != value && strings.Contains(raw, esc) {
			t.Errorf("transcript leaks %s in its escaped spelling %q:\n%s", name, esc, raw)
		}
		// A multiline value never appears whole in a line-oriented record, so the
		// absence above proves nothing about it. Its lines are what leaked.
		if strings.Contains(value, "\n") {
			for _, line := range strings.Split(value, "\n") {
				if strings.Contains(raw, line) {
					t.Errorf("transcript leaks the %q line of multiline %s:\n%s", line, name, raw)
				}
			}
		}
		// And out of the event field the loop retains (#61).
		if strings.Contains(texts.String(), value) {
			t.Errorf("event text leaks %s = %q:\n%s", name, value, texts.String())
		}
		if strings.Contains(value, "\n") {
			// Same reasoning as the transcript's: a line-oriented stream never
			// carries a multiline value whole, so its lines are what leaked.
			for _, line := range strings.Split(value, "\n") {
				if strings.Contains(texts.String(), line) {
					t.Errorf("event text leaks the %q line of multiline %s:\n%s", line, name, texts.String())
				}
			}
		}
	}
	// The passthrough value: in the child environment, in no declaration.
	if strings.Contains(raw, forwarded) {
		t.Errorf("transcript leaks the env_passthrough value %q:\n%s", forwarded, raw)
	}
	if strings.Contains(retainedPrompt, forwarded) {
		t.Errorf("the retained prompt leaks the env_passthrough value %q:\n%s", forwarded, retainedPrompt)
	}
	if strings.Contains(texts.String(), forwarded) {
		t.Errorf("event text leaks the env_passthrough value %q:\n%s", forwarded, texts.String())
	}
	// The name stays readable — that is what SensitiveFields keeps visible, and it
	// is what an operator needs when a passthrough is misconfigured. In the event
	// text too: what the loop retains has to stay a usable account of the attempt,
	// not a row of markers.
	if !strings.Contains(raw, "FORWARDED_PAT=") {
		t.Errorf("transcript lost the env_passthrough name:\n%s", raw)
	}
	if !strings.Contains(texts.String(), "FORWARDED_PAT=") {
		t.Errorf("event text lost the env_passthrough name:\n%s", texts.String())
	}
	if !strings.Contains(raw, "***") {
		t.Errorf("nothing was redacted, so the canaries were never written:\n%s", raw)
	}
}

// A credential the transcript writer cannot cover is refused at load, not leaked
// once per run: a value at or above the redaction floor whose every line is under
// it straddles writes, so no needle matches (SPEC §10.3).
//
// Asked of every credential surface the adapter has — each named key and `env` —
// because the check reads the same declaration the redaction does, and a surface
// it forgot is a surface that leaks.
func testUnredactableCredential(t *testing.T, c Contract) {
	// Eight bytes, so the invariant covers it; lines of three and four, so nothing
	// the harness writes contains an eligible needle.
	const unredactable = "abc\n1234"

	blocks := map[string]map[string]any{
		"env": c.block(t, "", map[string]string{"SHORT_LINES": unredactable}),
	}
	for _, cred := range c.Credentials {
		b := c.block(t, "", nil)
		b[cred.Key] = unredactable
		blocks[cred.Key] = b
	}

	for where, block := range blocks {
		t.Run(where, func(t *testing.T) {
			err := c.Kind.Structural(core.AgentConfig{Provider: block})
			if !errors.Is(err, c.Errors.ProviderValue) {
				t.Errorf("Structural = %v, want %v", err, c.Errors.ProviderValue)
			}
			// And through the constructor, since that is the path a run takes.
			if _, err := c.Kind.New(core.RunnerOptions{Provider: block}); !errors.Is(err, c.Errors.ProviderValue) {
				t.Errorf("kind.New = %v, want %v", err, c.Errors.ProviderValue)
			}
			// The refusal names the field, never the value (boundary 2's call).
			if err != nil && strings.Contains(err.Error(), unredactable) {
				t.Errorf("refusal echoes the credential: %v", err)
			}
		})
	}
}

// A forwarded value the transcript writer could not cover refuses at readiness,
// which is where a configuration-versus-world mismatch belongs (SPEC §7.1): one
// loud refusal at startup rather than a leak per attempt.
//
// Its shape is the host's, not the block's, so this cannot be asked purely —
// Structural must stay pure (see testStructuralIsPure), and Ready is the step that
// reads the world.
func testUnredactableForwardedAtReady(t *testing.T, c Contract) {
	// Eight bytes, so the invariant covers it; no scanner-visible line eligible.
	t.Setenv("FORWARDED_PAT", "abc\n1234")
	block := c.block(t, scriptSuccess, nil)
	block["env_passthrough"] = []any{"FORWARDED_PAT"}

	err := c.newRunner(t, block, Options{}).Ready(context.Background())
	if !errors.Is(err, c.Errors.ProviderValue) {
		t.Errorf("Ready = %v, want %v", err, c.Errors.ProviderValue)
	}
}

// And again at Start, which is not redundant: Ready reads the daemon environment
// once, and the value behind a passthrough name can change afterwards — or Ready
// can be skipped altogether, which the adapter already tolerates for the binary.
// The set is per-attempt, so the refusal has to be too.
//
// Ready runs here with a *redactable* value and must pass, so a Start that did not
// check would leave this test green.
func testUnredactableForwardedAtStart(t *testing.T, c Contract) {
	t.Setenv("FORWARDED_PAT", "ghp-redactable-0123456789")
	block := c.block(t, scriptSuccess, nil)
	block["env_passthrough"] = []any{"FORWARDED_PAT"}
	r := c.newRunner(t, block, Options{})

	if err := r.Ready(context.Background()); err != nil {
		t.Fatalf("Ready = %v, want ok: the value was redactable when Ready read it", err)
	}

	// The daemon's environment changes under the bound runner.
	t.Setenv("FORWARDED_PAT", "abc\n1234")
	h, err := r.Start(context.Background(), c.spec(t, core.RunLimits{}))
	if !errors.Is(err, c.Errors.ProviderValue) {
		t.Errorf("Start = %v, want %v", err, c.Errors.ProviderValue)
	}
	if h != nil {
		t.Error("Start returned a handle for a refusal (SPEC §7.4)")
		h.Stop(context.Background(), core.StopDiscard)
	}
}

// Stateless redaction is sound on two facts together: the harness writes whole
// lines, and every value in the set has a needle that fits inside one such line.
// A multiline credential's own needle *does* straddle writes — only its lines
// match — which is why harness.CheckRedactable and CheckRedactableEnv refuse the
// values for which no eligible line exists.
//
// This test pins the first fact only, because that is the one that lives in code
// rather than in configuration: it is a property of readStdout and
// finishTranscript, not of the sink, so switch either to a chunked copy and this
// fails instead of a value leaking through a boundary. The second fact is a
// refusal, and its own tests.
func testTranscriptWholeLines(t *testing.T, c Contract) {
	rec := &recordingTranscript{}
	// The big-line script, because it covers what a chunked reader would break
	// first: a 1 MiB line, past bufio's default buffer, still has to arrive as
	// one write rather than sixteen 64 KiB pieces.
	r := c.runner(t, scriptBigLine, nil, Options{Transcripts: rec})
	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	collect(t, h)
	waitDone(t, h)

	writes := rec.writes()
	if len(writes) == 0 {
		t.Fatal("the sink saw no writes at all")
	}
	for i, w := range writes {
		if !strings.HasSuffix(w, "\n") {
			t.Errorf("write %d does not end a line: %q", i, w)
		}
		if strings.Count(w, "\n") != 1 {
			t.Errorf("write %d carries %d lines, want 1: %q", i, strings.Count(w, "\n"), w)
		}
	}
}

// SPEC §7.6, RunSpec side. The two rows a "may not override provider-derived
// keys" rule would have let through are the point of the table: the adapter's
// credential variable when the block omits it (nothing was derived, so nothing
// was protected), and HOME, which arrives via the allowlisted passthrough
// rather than the provider block yet redirects keychain and config-file
// credential lookup.
//
// The assertion is the refusal, never an ordering.
func testRunSpecNamespace(t *testing.T, c Contract) {
	// Deliberately omits the credential key, so nothing is "provider-derived"
	// to protect.
	r := c.runner(t, scriptSuccess, nil, Options{})

	type row struct {
		name string
		env  map[string]string
		want error
	}
	// One row per credential, not the first one: the reservation is per variable,
	// and a table that checks one of an adapter's two reads as coverage of both.
	rows := make([]row, 0, len(c.Credentials))
	for _, cred := range c.Credentials {
		rows = append(rows, row{
			name: "credential " + cred.Env + " injected where the block derived nothing",
			env:  map[string]string{cred.Env: "injected-not-overridden"},
			want: c.Errors.EnvNamespace,
		})
	}
	for _, tc := range append(rows, []row{
		{
			name: "HOME redirected, which reaches the child via the allowlist",
			env:  map[string]string{"HOME": "/tmp/someone-elses-keychain"},
			want: c.Errors.EnvNamespace,
		},
		{
			name: "an ordinary variable is still outside the namespace",
			env:  map[string]string{"PATH": "/evil/bin"},
			want: c.Errors.EnvNamespace,
		},
		{
			name: "one reserved key among many is still a refusal",
			env:  map[string]string{"BEN_RUN_ID": "run-7", "GH_TOKEN": "sneaked"},
			want: c.Errors.EnvNamespace,
		},
		{
			name: "the orchestrator's own namespace is accepted",
			env:  map[string]string{"BEN_RUN_ID": "run-7", "BEN_ISSUE": "123"},
		},
	}...) {
		t.Run(tc.name, func(t *testing.T) {
			spec := c.spec(t, core.RunLimits{})
			spec.Env = tc.env
			h, err := r.Start(context.Background(), spec)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("Start = %v, want ok", err)
				}
				t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })
				collect(t, h)
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("Start = %v, want %v", err, tc.want)
			}
			if h != nil {
				t.Error("Start returned a handle alongside an error")
			}
		})
	}
}

// SPEC §7.6, config side: a provider block may not define the reserved prefix in
// *any* environment surface. This is the half that matters more — a collision
// authored here is written once and hits every run — and a namespace enforced on
// only the RunSpec side must fail these rows.
func testProviderEnvNamespace(t *testing.T, c Contract) {
	for _, tc := range []struct {
		name string
		mut  func(map[string]any)
		want error
	}{
		{
			name: "env defines the prefix",
			mut:  func(b map[string]any) { b["env"] = map[string]any{"BEN_RUN_ID": "spoofed"} },
			want: c.Errors.EnvNamespace,
		},
		{
			name: "env_passthrough names the prefix",
			mut:  func(b map[string]any) { b["env_passthrough"] = []any{"BEN_RUN_ID"} },
			want: c.Errors.EnvNamespace,
		},
		{
			name: "env with an unrelated key is fine",
			mut:  func(b map[string]any) { b["env"] = map[string]any{"GH_TOKEN": "t"} },
		},
		{
			name: "env_passthrough with an unrelated name is fine",
			mut:  func(b map[string]any) { b["env_passthrough"] = []any{"HTTPS_PROXY"} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := c.Block("harness", nil)
			tc.mut(block)
			err := c.Kind.Structural(core.AgentConfig{Provider: block})
			if tc.want == nil {
				if err != nil {
					t.Errorf("Structural = %v, want ok", err)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("Structural = %v, want %v", err, tc.want)
			}
		})
	}
}

// --- structure, readiness, and binding (SPEC §5.7, §7.1) ---

// The package-level kind satisfies core.RunnerKind, and everything reached
// through that interface behaves: a pure Structural, a New that binds, and a
// runner whose Ready and Capabilities survive the indirection. B11's registry
// selects a kind by name (BUILD assembly decision 13); what an adapter owes is
// conformance.
// The `env_passthrough` surface is declared, because it is the one route into a
// child the loader cannot see for itself (SPEC §10.2, #47). Every other secret a
// block carries in arrives as a resolved *value*, and the loader knows which
// variable each value came from; a variable *name* in the block is opaque to it.
// If a kind stops reporting these, the §10.2 split check stops seeing them and a
// shared tracker credential loads.
// The model an adapter *declares* is the model it *launches*, and the path it
// names is where that value lives in the block (#60).
//
// Anchored at argv rather than at the declaration, deliberately. A test that
// asked Kind.Model and then read the block back at the path Kind.Model returned
// would agree with itself for any pair of answers, including a spelling no
// launch uses — which is exactly the drift that puts a model in the
// attempt-outcome record that nothing ran, and makes #62's comparison a
// comparison of the wrong thing. Argv is the independent boundary: it is what
// the harness is actually told.
//
// An adapter that stops passing `--model` at all fails here, and should: the
// record would then be describing a selection the launch does not make.
func testModelDeclared(t *testing.T, c Contract) {
	const model = "a-model-name-nothing-else-uses"
	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	block := c.block(t, scriptSuccess, map[string]string{DumpEnv: dumpPath})
	block["model"] = model

	got, path := c.Kind.Model(block)
	if got != model {
		t.Errorf("Model = %q, want %q", got, model)
	}
	if len(path) == 0 {
		t.Fatal("Model returned no path; the redactor cannot decide whether the value may be printed")
	}
	// The path has to name the value, or `config effective`'s provenance lookup
	// asks about a key nothing wrote (config.AgentDescriptor).
	var at any = map[string]any(block)
	for _, seg := range path {
		m, ok := at.(map[string]any)
		if !ok {
			t.Fatalf("Model path %v does not resolve in the block", path)
		}
		at = m[seg]
	}
	if at != model {
		t.Errorf("Model path %v resolves to %v, want the declared model", path, at)
	}

	h := c.start(t, c.newRunner(t, block, Options{}), c.spec(t, core.RunLimits{}))
	collect(t, h)
	if argv := readDump(t, dumpPath).Argv; !slices.Contains(argv, model) {
		t.Errorf("argv = %v, missing the declared model: the record would name a model the launch did not select", argv)
	}
}

func testForwardedEnvVars(t *testing.T, c Contract) {
	block := c.Block("harness", nil)
	block["env_passthrough"] = []any{"FORWARDED_BY_NAME"}

	got := c.Kind.ForwardedEnvVars(block)
	if !slices.Contains(got, "FORWARDED_BY_NAME") {
		t.Errorf("ForwardedEnvVars = %v, missing the env_passthrough entry: a tracker "+
			"credential forwarded this way would be invisible to the §10.2 split check", got)
	}
}

// Both routes by which a secret sits in an agent provider block are declared
// sensitive, so `config effective` hides them whatever their provenance
// (SPEC §5.8, #52). Here rather than in `internal/config`, whose redaction test
// is driven off these declarations and therefore asserts that what is *declared*
// is hidden — it cannot notice a declaration going missing.
//
// `env_passthrough` is asserted absent on purpose: its entries are variable
// names rather than secrets, and hiding them would conceal exactly what an
// operator needs to read when a passthrough is misconfigured.
func testSensitiveFields(t *testing.T, c Contract) {
	block := c.Block("harness", map[string]string{"GH_TOKEN": "publish-secret"})
	for _, cred := range c.Credentials {
		block[cred.Key] = "credential-secret"
	}
	block["env_passthrough"] = []any{"FORWARDED_BY_NAME"}

	var got []string
	for _, segs := range c.Kind.SensitiveFields(block) {
		got = append(got, strings.Join(segs, "."))
	}
	wantFields := []string{"env.GH_TOKEN"}
	for _, cred := range c.Credentials {
		wantFields = append(wantFields, cred.Key)
	}
	for _, want := range wantFields {
		if !slices.Contains(got, want) {
			t.Errorf("SensitiveFields = %v, missing %q: a credential there would print in "+
				"the clear in `config effective`", got, want)
		}
	}
	for _, unwanted := range []string{"env_passthrough", "env_passthrough.0"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("SensitiveFields = %v, redacting %q: those are variable names, not secrets",
				got, unwanted)
		}
	}
}

func testKindConforms(t *testing.T, c Contract) {
	kind := c.Kind
	if err := kind.Structural(core.AgentConfig{Provider: c.Block("harness", nil)}); err != nil {
		t.Errorf("kind.Structural = %v, want ok", err)
	}
	runner, err := kind.New(core.RunnerOptions{
		Provider:      c.block(t, "", nil),
		TranscriptDir: filepath.Join(t.TempDir(), "transcripts"),
	})
	if err != nil {
		t.Fatalf("kind.New = %v, want ok", err)
	}
	if err := runner.Ready(context.Background()); err != nil {
		t.Errorf("Ready = %v, want ok", err)
	}
	if runner.Capabilities() != c.runner(t, "", nil, Options{}).Capabilities() {
		t.Error("Capabilities changed when reached through the kind")
	}
}

// Structural is a property of the kind, and it is pure (SPEC §5.7, §7.1):
// `ben config effective` has to work on a machine that never installed the
// harness, so a well-formed block must pass with nothing on PATH.
func testStructuralIsPure(t *testing.T, c Contract) {
	t.Setenv("PATH", t.TempDir()) // nothing executable anywhere
	block := c.Block("some-harness-binary", nil)
	if err := c.Kind.Structural(core.AgentConfig{Provider: block}); err != nil {
		t.Errorf("Structural = %v, want ok with no harness on PATH", err)
	}
	// The same configuration is not *ready*, and that is where the refusal
	// belongs.
	r, err := c.Kind.New(core.RunnerOptions{Provider: block})
	if err != nil {
		t.Fatalf("New = %v, want ok", err)
	}
	if err := r.Ready(context.Background()); !errors.Is(err, c.Errors.Binary) {
		t.Errorf("Ready = %v, want %v", err, c.Errors.Binary)
	}
}

// A refusal that quotes a provider value must carry it as data, never in the
// error text (SPEC §5.8). Provider strings are `$VAR`-resolved before an adapter
// sees them (SPEC §5.5), and `ben config effective` prints these refusals in CI,
// so a value in the message is a secret in a log — and redaction cannot be
// retrofitted onto freeform text, which is why core.ConfigValueError exists.
//
// Driven through the posture key because every adapter has one and it is
// required, so every adapter has at least one value-refusing check. Field must
// match the loader's provenance path, or the renderer cannot find the value's
// origin: config.RenderRefusal then redacts unconditionally, losing the variable
// name that tells an operator which secret to fix.
func testRefusalCarriesValueAsData(t *testing.T, c Contract) {
	const value = "not-a-posture-but-plausibly-a-secret"
	block := c.Block("harness", nil)
	block[c.Posture.Key] = value

	err := c.Kind.Structural(core.AgentConfig{Provider: block})
	if !errors.Is(err, c.Posture.Err) {
		t.Fatalf("Structural = %v, want %v", err, c.Posture.Err)
	}
	if strings.Contains(err.Error(), value) {
		t.Errorf("refusal text %q carries the offending value", err.Error())
	}
	var verr *core.ConfigValueError
	if !errors.As(err, &verr) {
		t.Fatalf("refusal = %#v, want a *core.ConfigValueError carrying the value", err)
	}
	if want := "agent.provider." + c.Posture.Key; verr.Field != want || verr.Value != value {
		t.Errorf("refusal = ConfigValueError{Field: %q, Value: %q}, want {%q, %q}",
			verr.Field, verr.Value, want, value)
	}
}

// Structural failures belong to the kind, before any instance exists: a
// malformed block fails construction too, so there is nothing to ask
// (SPEC §5.7).
func testMalformedProvider(t *testing.T, c Contract) {
	for _, tc := range []struct {
		name string
		mut  func(map[string]any)
		want error
	}{
		{"unknown provider key", func(b map[string]any) { b["bogus_key_no_adapter_has"] = "x" }, c.Errors.ProviderKey},
		{"missing required posture", func(b map[string]any) { delete(b, c.Posture.Key) }, c.Posture.Err},
	} {
		t.Run(tc.name, func(t *testing.T) {
			block := c.Block("harness", nil)
			tc.mut(block)
			if err := c.Kind.Structural(core.AgentConfig{Provider: block}); !errors.Is(err, tc.want) {
				t.Errorf("Structural = %v, want %v", err, tc.want)
			}
			r, err := c.Kind.New(core.RunnerOptions{Provider: block})
			if !errors.Is(err, tc.want) {
				t.Errorf("New = %v, want %v", err, tc.want)
			}
			if r != nil {
				t.Error("New returned a runner alongside an error")
			}
		})
	}
}

func testStartRefusals(t *testing.T, c Contract) {
	r := c.runner(t, scriptSuccess, nil, Options{})
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		mut  func(*core.RunSpec)
		want error
	}{
		{"empty prompt", func(s *core.RunSpec) { s.Prompt = "" }, c.Errors.PromptEmpty},
		{"relative workspace", func(s *core.RunSpec) { s.Workspace.Path = "relative" }, c.Errors.WorkspacePath},
		{"missing workspace", func(s *core.RunSpec) { s.Workspace.Path = filepath.Join(dir, "nope") }, c.Errors.WorkspacePath},
		{"workspace is a file", func(s *core.RunSpec) { s.Workspace.Path = file }, c.Errors.WorkspacePath},
		{
			// SPEC §7.6: the orchestrator's namespace is exclusive, and a
			// RunSpec reaching outside it is refused rather than merged.
			name: "env key outside the BEN_ namespace",
			mut:  func(s *core.RunSpec) { s.Env = map[string]string{"SOMETHING_ELSE": "injected"} },
			want: c.Errors.EnvNamespace,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := c.spec(t, core.RunLimits{})
			tc.mut(&spec)
			h, err := r.Start(context.Background(), spec)
			if !errors.Is(err, tc.want) {
				t.Errorf("Start = %v, want %v", err, tc.want)
			}
			if h != nil {
				t.Error("Start returned a handle alongside an error")
			}
		})
	}
}

func testMissingBinary(t *testing.T, c Contract) {
	block := c.block(t, scriptSuccess, nil)
	block["binary"] = filepath.Join(t.TempDir(), "no-such-harness")
	h, err := c.newRunner(t, block, Options{}).Start(context.Background(), c.spec(t, core.RunLimits{}))
	if !errors.Is(err, c.Errors.Binary) {
		t.Fatalf("Start = %v, want %v", err, c.Errors.Binary)
	}
	if h != nil {
		t.Error("Start returned a handle alongside an error")
	}
}

func testReadyRefusals(t *testing.T, c Contract) {
	for _, tc := range []struct {
		name   string
		binary string
	}{
		{"missing binary", filepath.Join(t.TempDir(), "nope")},
		// A real, working binary that is not this harness: identity is checked,
		// not mere existence.
		{"not this harness", "echo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := c.newRunner(t, c.Block(tc.binary, nil), Options{})
			if err := r.Ready(context.Background()); !errors.Is(err, c.Errors.Binary) {
				t.Errorf("Ready = %v, want %v", err, c.Errors.Binary)
			}
		})
	}
}

func testExecutionDomainRefusals(t *testing.T, c Contract) {
	cause := errors.New("test execution domain unavailable")
	r := c.runner(t, scriptSuccess, nil, Options{Domain: refusingDomain{err: cause}})

	if err := r.Ready(context.Background()); !errors.Is(err, c.Errors.ExecutionDomain) || !errors.Is(err, cause) {
		t.Errorf("Ready = %v, want %v wrapping %v", err, c.Errors.ExecutionDomain, cause)
	}
	h, err := r.Start(context.Background(), c.spec(t, core.RunLimits{}))
	if !errors.Is(err, c.Errors.ExecutionDomain) || !errors.Is(err, cause) {
		t.Errorf("Start = %v, want %v wrapping %v", err, c.Errors.ExecutionDomain, cause)
	}
	if h != nil {
		t.Error("Start returned a handle alongside an execution-domain refusal")
	}
}

// A harness the daemon merely *validates* must not be handed the daemon's
// secrets: every readiness probe runs with the same restricted environment as a
// real attempt (SPEC §7.6).
func testProbeEnvRestricted(t *testing.T, c Contract) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "daemon-aws-MUST-NOT-LEAK")
	for _, cred := range c.Credentials {
		t.Setenv(cred.Env, "daemon-credential-MUST-NOT-LEAK")
	}

	dumpPath := filepath.Join(t.TempDir(), "probe.json")
	block := c.block(t, "", map[string]string{DumpEnv: dumpPath})
	for _, cred := range c.Credentials {
		block[cred.Key] = "provider-credential"
	}

	if err := c.newRunner(t, block, Options{}).Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Every probe, not just the last one: each invocation is a place a secret
	// could leak, and an adapter may make more than one.
	dumps := readDumps(t, dumpPath)
	if len(dumps) == 0 {
		t.Fatal("Ready made no probe invocation at all")
	}
	for _, d := range dumps {
		env := envMap(d.Env)
		if _, ok := env["AWS_SECRET_ACCESS_KEY"]; ok {
			t.Errorf("probe %v inherited a daemon secret", d.Argv[1:])
		}
		for _, cred := range c.Credentials {
			if env[cred.Env] != "provider-credential" {
				t.Errorf("probe %v %s = %q, want the provider value", d.Argv[1:], cred.Env, env[cred.Env])
			}
		}
	}
}

// Ready must be bounded even when a probe leaves a descendant holding stdout:
// killing the process is not enough, because the wait on its pipes is unbounded
// without a WaitDelay. Bounded, not instant, is the claim — a daemon refuses to
// start on a failed readiness check (SPEC §11), which requires the check to
// return at all.
func testReadyBounded(t *testing.T, c Contract) {
	r := c.runner(t, "", map[string]string{ProbeEnv: "leak"}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_ = r.Ready(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Ready took %v: it is waiting on the leaked pipe rather than on its own bound", elapsed)
	}
}

// A probe that leaks a child must not leak it past readiness. Returning on time
// is only half the job: a daemon probes on every reload, and orphans accumulate.
func testReadyNoOrphans(t *testing.T, c Contract) {
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	r := c.runner(t, "", map[string]string{ProbeEnv: "leak", PIDEnv: pidFile}, Options{})

	// A window wide enough that the probe finishes and actually leaks its child:
	// what is under test is that readiness cleans up after itself, not that it
	// returns on time (testReadyBounded owns the bound, and cutting the probe
	// short here would leave nothing to clean up and nothing to assert).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = r.Ready(ctx)

	pid := readPID(t, pidFile)
	defer syscall.Kill(pid, syscall.SIGKILL) // never leak it, whatever the outcome
	if aliveNow(pid) {
		t.Errorf("probe child %d outlived Ready", pid)
	}
}

// A probe that floods its output is refused, not read (#235). The fake prints
// the answer the adapter looks for *first*, so a refusal here is the bound
// speaking and not a missing marker: the head of the output is exactly what
// would have passed. And the refusal quotes an excerpt, never the flood — a
// startup error is read by an operator.
func testReadyFloodingProbe(t *testing.T, c Contract) {
	r := c.runner(t, "", map[string]string{ProbeEnv: "flood"}, Options{})
	err := r.Ready(context.Background())
	if !errors.Is(err, c.Errors.Binary) {
		t.Fatalf("Ready = %v, want %v", err, c.Errors.Binary)
	}
	if !errors.Is(err, harness.ErrProbeOutput) {
		t.Errorf("Ready = %v, want it to carry harness.ErrProbeOutput: the flood is the reason", err)
	}
	if n := len(err.Error()); n > 1024 {
		t.Errorf("the refusal is %d bytes: it embeds the probe's output rather than an excerpt", n)
	}
}

// The binary is resolved during Ready, not New (BUILD B06). A harness installed
// between construction and readiness must be found: caching the failure at
// construction would make readiness permanently wrong about a machine that has
// since been set up.
func testBinaryInstalledLater(t *testing.T, c Contract) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	// One runner throughout: the point is that construction may not decide this.
	r := c.newRunner(t, c.Block("harness-installed-later", nil), Options{})
	if err := r.Ready(context.Background()); !errors.Is(err, c.Errors.Binary) {
		t.Fatalf("Ready before install = %v, want %v", err, c.Errors.Binary)
	}

	// Install it, exactly as an operator would between daemon start and reload.
	installFakeHarness(t, filepath.Join(dir, "harness-installed-later"))

	if err := r.Ready(context.Background()); err != nil {
		t.Errorf("Ready after install = %v, want ok: resolution is not construction's to cache", err)
	}
}

// The binary Ready verified is the binary Start executes. A runner that took its
// provider block per-run could pass readiness against one configuration and
// launch another; binding at New removes the divergence rather than documenting
// it (SPEC §7.1). Mutating the caller's map afterwards must change nothing.
func testReadyAndStartAgree(t *testing.T, c Contract) {
	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	block := c.block(t, scriptSuccess, map[string]string{DumpEnv: dumpPath})
	bound := block["binary"].(string)

	r := c.newRunner(t, block, Options{})
	if err := r.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Whatever the caller does to the block now, the runner is already bound —
	// including edits that would make the block refuse to parse at all.
	block["binary"] = filepath.Join(t.TempDir(), "some-other-harness")
	delete(block, c.Posture.Key)

	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	if got := terminal(t, collect(t, h)); got.Type != core.EventSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", got)
	}
	if got := readDump(t, dumpPath).Argv[0]; got != bound {
		t.Errorf("Start executed %q, but Ready verified %q", got, bound)
	}
}

// The binary Ready verified is the binary Start executes, even if PATH changes
// underneath. Resolving a bare name again at Start would undo the binding.
func testBoundBinarySurvivesPath(t *testing.T, c Contract) {
	// A directory containing the harness — a copy of this test binary — on PATH.
	dir := t.TempDir()
	bound := filepath.Join(dir, "bound-harness")
	installFakeHarness(t, bound)
	t.Setenv("PATH", dir)

	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	block := c.Block("bound-harness", map[string]string{ // a bare name: resolution is the point
		ScriptEnv: scriptSuccess,
		DumpEnv:   dumpPath,
	})
	r := c.newRunner(t, block, Options{})
	if err := r.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// Now put a *different* harness of the same name earlier on PATH, as a
	// reload or an operator's install would.
	other := t.TempDir()
	installFakeHarness(t, filepath.Join(other, "bound-harness"))
	t.Setenv("PATH", other)

	h := c.start(t, r, c.spec(t, core.RunLimits{}))
	if got := terminal(t, collect(t, h)); got.Type != core.EventSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", got)
	}
	if got := readDump(t, dumpPath).Argv[0]; got != bound {
		t.Errorf("Start executed %q, but Ready verified %q", got, bound)
	}
}

// A relative binding is an execution path the *agent* controls. Start runs with
// cwd set to the workspace — the agent's own writable tree — so a path like
// "./harness" would resolve there at exec time and run whatever the agent put
// in it, not what Ready verified.
func testRelativeBinaryIsAbsolute(t *testing.T, c Contract) {
	harnessDir := t.TempDir()
	installFakeHarness(t, filepath.Join(harnessDir, "rel-harness"))
	t.Chdir(harnessDir)

	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	r := c.newRunner(t, c.Block("./rel-harness", map[string]string{ // relative on purpose
		ScriptEnv: scriptSuccess,
		DumpEnv:   dumpPath,
	}), Options{})
	if err := r.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}

	// The workspace contains a *different* ./rel-harness, as a hostile or
	// careless agent could arrange. Running it would produce a stream-less crash.
	spec := c.spec(t, core.RunLimits{})
	decoy := filepath.Join(spec.Workspace.Path, "rel-harness")
	if err := os.WriteFile(decoy, []byte("#!/bin/sh\nexit 9\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	h := c.start(t, r, spec)
	if got := terminal(t, collect(t, h)); got.Type != core.EventSucceeded {
		t.Fatalf("terminal = %+v, want succeeded — the workspace copy ran", got)
	}
	got := readDump(t, dumpPath).Argv[0]
	if !filepath.IsAbs(got) {
		t.Errorf("Start executed %q: a relative argv[0] resolves against the workspace at exec time", got)
	}
	// macOS temp dirs are symlinked (/var → /private/var), so compare resolved.
	if resolve(got) != resolve(filepath.Join(harnessDir, "rel-harness")) {
		t.Errorf("Start executed %q, want the harness Ready verified", got)
	}
}

// Continuation is adapter-opaque and never interpreted by the orchestrator
// (SPEC §7.1) — but it must reach the harness, or a resumed attempt silently
// starts a fresh session and repeats work the previous one did.
func testContinuation(t *testing.T, c Contract) {
	if !c.runner(t, "", nil, Options{}).Capabilities().Resume {
		t.Skip("adapter declares no resume; the refusal is covered by Capabilities")
	}
	const token = "continuation-token-0123456789"
	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	r := c.runner(t, scriptSuccess, map[string]string{DumpEnv: dumpPath}, Options{})

	spec := c.spec(t, core.RunLimits{})
	spec.Continuation = token
	h := c.start(t, r, spec)
	if got := terminal(t, collect(t, h)); got.Type != core.EventSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", got)
	}
	if argv := readDump(t, dumpPath).Argv; !slices.Contains(argv, token) {
		t.Errorf("argv = %v, want the continuation token %q", argv, token)
	}

	// And a fresh session carries no token: a stale resume is worse than none.
	fresh := filepath.Join(t.TempDir(), "fresh.json")
	h = c.start(t, c.runner(t, scriptSuccess, map[string]string{DumpEnv: fresh}, Options{}), c.spec(t, core.RunLimits{}))
	collect(t, h)
	if argv := readDump(t, fresh).Argv; slices.Contains(argv, token) {
		t.Errorf("a fresh run carried a continuation token: %v", argv)
	}
}

// The resume token is the one argv element the *agent* chooses (#233). It is
// minted from the child's own JSON stream and appended to the next attempt's
// argv, and the two ends are far enough apart — a translator, a state file, a
// dispatch — that an adapter checking only the convenient one still hands the
// harness a flag it selected for itself.
//
// So both ends are asserted here, of every adapter, against a real process. The
// halves are independent by construction: the second token never passes through
// the translator at all, which is exactly the case a state file written by an
// older build presents.
func testHostileContinuation(t *testing.T, c Contract) {
	r := c.runner(t, scriptUntrustedID, nil, Options{})
	if !r.Capabilities().Resume {
		t.Skip("adapter declares no resume; there is no token to mint (see Capabilities)")
	}

	// The minting end. The harness announces an identity that is a flag rather
	// than a session id, and the attempt is otherwise healthy — and must stay
	// healthy: a line the adapter will not read is activity like any other
	// (SPEC §7.2), so refusing it costs the resume and nothing else.
	evs := collect(t, c.start(t, r, c.spec(t, core.RunLimits{})))
	want := []core.EventType{core.EventHeartbeat, core.EventProgress}
	if r.Capabilities().Usage {
		want = append(want, core.EventUsage)
	}
	want = append(want, core.EventSucceeded)
	if got := types(evs); !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v: the announcement is activity, not a start", got, want)
	}
	for _, e := range evs {
		if e.Continuation != "" || e.SessionID != "" {
			t.Errorf("event %+v carries an identity minted from %q", e, HostileSessionID)
		}
	}

	// The argv end. A refusal, so no process exists to reason about
	// (SPEC §7.3): the adapter owes this wherever it builds an argv, not only
	// where it happens to have translated a line.
	spec := c.spec(t, core.RunLimits{})
	spec.Continuation = HostileSessionID
	h, err := c.runner(t, scriptSuccess, nil, Options{}).Start(context.Background(), spec)
	if !errors.Is(err, c.Errors.Continuation) {
		t.Fatalf("Start with a flag-shaped continuation = %v, want %v", err, c.Errors.Continuation)
	}
	if h != nil {
		t.Error("Start returned a handle alongside its refusal")
	}
}

// --- helpers ---

// block builds the provider block for a scripted run: this test binary as the
// harness, with the script and any fake controls travelling in provider.env —
// which is also the only way to reach the child environment, since RunSpec.Env
// is reserved to BEN_ keys (SPEC §7.6).
func (c Contract) block(t *testing.T, script string, extra map[string]string) map[string]any {
	t.Helper()
	env := map[string]string{}
	if script != "" {
		env[ScriptEnv] = script
	}
	maps.Copy(env, extra)
	return c.Block(selfPath(t), env)
}

func (c Contract) newRunner(t *testing.T, block map[string]any, opts Options) core.AgentRunner {
	t.Helper()
	opts.Timings = suiteTimings(opts.Timings)
	if opts.Domain == nil {
		opts.Domain = processDomain{signal: opts.Signal}
	}
	return c.New(t, block, opts)
}

// suiteTimings fills the lifecycle windows a test did not pin.
//
// The production ones are seconds wide, and rightly so — SPEC §7.5 argues them
// against a real harness that may hold a pipe or ignore a signal. But nothing in
// this suite asserts a window's *duration*; every test asserts what happens once
// one closes. Sleeping through the real ones therefore buys no coverage, and it
// is not free: two of these tests spent a full post-exit drain each, per adapter,
// on a `make check` that BEN itself runs in every worktree.
func suiteTimings(t harness.Timings) harness.Timings {
	if t.StopGrace == 0 {
		// Short enough to keep the suite quick, long enough that a loaded
		// machine still reaps within the grace and the verdict stays stable.
		t.StopGrace = 500 * time.Millisecond
	}
	if t.PostExitDrain == 0 {
		// Two of these elapse back to back for a run nobody drains (see
		// harness.boundStream), and the scripts here hold no pipe open, so the
		// window is only ever paid in full when the test means to.
		t.PostExitDrain = 200 * time.Millisecond
	}
	if t.ProbeWait == 0 {
		// testReadyBounded asserts a probe's leaked pipe is bounded, not that it
		// is instant; the bound just does not have to be the production one.
		t.ProbeWait = 250 * time.Millisecond
	}
	return t
}

func (c Contract) runner(t *testing.T, script string, extra map[string]string, opts Options) core.AgentRunner {
	t.Helper()
	return c.newRunner(t, c.block(t, script, extra), opts)
}

// spec is the per-attempt half: no provider block, because a runner binds its
// configuration at New (SPEC §7.1).
//
// The workspace half is *complete* — every path §6.1 says a Workspace reports,
// laid out the way §6.2 lays them out, with the private dir outside the
// worktree. Not because any adapter is known to read them: because that is the
// shape the orchestrator builds, and a suite that handed over the degenerate
// one would only ever prove adapters work when two thirds of the contract is
// missing. An adapter needing none of them beyond cwd ignores the rest.
//
// What the suite still does not learn is what an adapter *does* with them. That
// is a posture, and postures are the adapter's own business (#81) — which is
// why the assertions about a particular variable live in each adapter's tests
// and not here.
func (c Contract) spec(t *testing.T, limits core.RunLimits) core.RunSpec {
	t.Helper()
	root := t.TempDir()
	paths := core.WorkspacePaths{
		Path:         filepath.Join(root, "issues", "conformance"),
		SharedGitDir: filepath.Join(root, "base.git"),
		PrivateDir:   filepath.Join(root, "private", "conformance"),
	}
	for _, dir := range []string{paths.Path, paths.SharedGitDir, paths.PrivateDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return core.RunSpec{
		Workspace: paths,
		Prompt:    "do the thing",
		Limits:    limits,
	}
}

// ChildEnv runs one attempt through this contract's adapter and returns the
// environment the fake harness actually received, with the RunSpec that
// produced it. providerKeys are merged over the minimal valid block, so a caller
// can select a posture.
//
// Exported for the assertions an adapter must make about its own environment
// surface and the suite must not: which variable a posture writes, and what it
// points at, belong to the adapter (#81). What belongs here is the machinery for
// observing a real child's environment — a second copy of it in each adapter's
// tests would be a second fake harness to keep faithful to the real one, which
// is the failure the shared suite exists to prevent.
//
// The environment is read off the child rather than off the adapter's own
// composition function, because those are two different claims: one is what the
// adapter built, the other is what a process got.
func (c Contract) ChildEnv(t *testing.T, providerKeys map[string]any) (map[string]string, core.RunSpec) {
	t.Helper()
	dumpPath := filepath.Join(t.TempDir(), "dump.json")
	block := c.FakeBlock(t, providerKeys, map[string]string{DumpEnv: dumpPath})
	spec := c.spec(t, core.RunLimits{})
	h := c.start(t, c.FakeRunner(t, block), spec)
	collect(t, h)
	return envMap(readDump(t, dumpPath).Env), spec
}

// FakeBlock builds a minimal valid provider block wired to the fake harness — a
// run that succeeds — with providerKeys merged over it so an adapter test can
// select a posture, and childEnv added to the block's `env`.
//
// Exported alongside FakeRunner because the binary is the one part an adapter
// test must not get wrong and cannot get right on its own: the fake harness is
// the *test binary itself*, re-exec'd at a path only this package knows. A block
// naming anything else resolves a real harness on PATH — which fails on a
// machine without one and, far worse, silently launches the real thing on a
// machine with one. Two tests in this suite's first #114 draft passed locally
// for exactly that reason and failed only on CI.
func (c Contract) FakeBlock(t *testing.T, providerKeys map[string]any, childEnv map[string]string) map[string]any {
	t.Helper()
	block := c.block(t, scriptSuccess, childEnv)
	maps.Copy(block, providerKeys)
	return block
}

// FakeRunner constructs a runner from a FakeBlock under the suite's timings.
func (c Contract) FakeRunner(t *testing.T, block map[string]any) core.AgentRunner {
	t.Helper()
	return c.newRunner(t, block, Options{})
}

// start launches a run and guarantees the process cannot outlive the test.
func (c Contract) start(t *testing.T, r core.AgentRunner, spec core.RunSpec) core.RunHandle {
	t.Helper()
	h, err := r.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })
	return h
}

func selfPath(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// installFakeHarness copies this test binary, which behaves as the fake harness
// when invoked like one.
func installFakeHarness(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(selfPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o700); err != nil {
		t.Fatal(err)
	}
}

// collect drains the event stream to its close, which the adapter does after
// the terminal event (SPEC §7.2).
func collect(t *testing.T, h core.RunHandle) []core.Event {
	t.Helper()
	var got []core.Event
	timeout := time.After(30 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-timeout:
			t.Fatalf("event stream did not close; got %v", types(got))
		}
	}
}

func waitDone(t *testing.T, h core.RunHandle) {
	t.Helper()
	select {
	case <-h.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("Done never closed")
	}
}

// terminal returns the last event, which must be the only terminal one.
func terminal(t *testing.T, evs []core.Event) core.Event {
	t.Helper()
	if len(evs) == 0 {
		t.Fatal("no events at all")
	}
	for i, ev := range evs[:len(evs)-1] {
		if harness.IsTerminal(ev.Type) {
			t.Fatalf("terminal event at index %d of %v; exactly one, last, is the contract", i, types(evs))
		}
	}
	last := evs[len(evs)-1]
	if !harness.IsTerminal(last.Type) {
		t.Fatalf("stream ended without a terminal event: %v", types(evs))
	}
	return last
}

func types(evs []core.Event) []core.EventType {
	out := make([]core.EventType, len(evs))
	for i, ev := range evs {
		out[i] = ev.Type
	}
	return out
}

func countType(evs []core.Event, want core.EventType) int {
	n := 0
	for _, ev := range evs {
		if ev.Type == want {
			n++
		}
	}
	return n
}

// readDumps returns every invocation the fake recorded, in order.
func readDumps(t *testing.T, path string) []dump {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("harness never wrote its dump: %v", err)
	}
	var dumps []dump
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		var d dump
		if err := json.Unmarshal([]byte(line), &d); err != nil {
			t.Fatal(err)
		}
		dumps = append(dumps, d)
	}
	return dumps
}

// readDump returns the last recorded invocation — for a run, the one made after
// the prompt was read.
func readDump(t *testing.T, path string) dump {
	t.Helper()
	dumps := readDumps(t, path)
	return dumps[len(dumps)-1]
}

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		m[k] = v
	}
	return m
}

// blockValue reads the string at a SensitiveFields path, reporting whether the
// block sets one. Only strings: a number or a boolean there is not a credential.
func blockValue(block map[string]any, path []string) (string, bool) {
	var cur any = block
	for _, seg := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		if cur, ok = m[seg]; !ok {
			return "", false
		}
	}
	v, ok := cur.(string)
	return v, ok && v != ""
}

// jsonInner returns a value as it appears inside a JSON string — the spelling
// the transcript carries once an encoder has been over it.
func jsonInner(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %q: %v", v, err)
	}
	return string(b[1 : len(b)-1])
}

// onlyTranscript returns the single `.jsonl` an attempt wrote.
//
// It counts by suffix rather than counting the directory, because an attempt now
// leaves two files: the raw stream, and the canonical rendered prompt retained
// beside it (SPEC §9.5, harness.PromptSuffix). Both are per-attempt and neither
// is the other, so "exactly one transcript" is a claim about the `.jsonl` alone;
// counting entries would read the pair as a duplicate transcript.
func onlyTranscript(t *testing.T, dir string) string {
	t.Helper()
	return onlyFileWithSuffix(t, dir, ".jsonl", "transcript")
}

// onlyPrompt returns the single retained prompt an attempt wrote (SPEC §9.5):
// the bytes the agent was given, answerable after the fact.
func onlyPrompt(t *testing.T, dir string) string {
	t.Helper()
	return onlyFileWithSuffix(t, dir, harness.PromptSuffix, "retained prompt")
}

func onlyFileWithSuffix(t *testing.T, dir, suffix, what string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			found = append(found, e.Name())
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s files = %d (the directory holds %d entries), want 1", what, len(found), len(entries))
	}
	b, err := os.ReadFile(filepath.Join(dir, found[0]))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// harnessLines returns the transcript lines the harness itself wrote, dropping
// the BEN-namespaced ones the record synthesizes around them (SPEC §7.2). The
// two are different kinds of fact — one is the child's output, the other is
// BEN's account of the run — and only the first is what "verbatim" is a claim
// about.
func harnessLines(raw string) []string {
	var out []string
	for _, line := range splitLines(raw) {
		if benNamespaced(line) {
			continue
		}
		out = append(out, line)
	}
	return out
}

// benNamespaced reports whether a transcript line is one BEN synthesized. The
// `ben:` type prefix is reserved for exactly that, so a harness line can never
// be mistaken for one — and a line that is not JSON at all is the harness's
// (the garbage script writes such lines on purpose).
func benNamespaced(line string) bool {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil {
		return false
	}
	return strings.HasPrefix(probe.Type, "ben:")
}

// splitLines splits a newline-terminated stream into its lines, with no empty
// tail element for the final newline and none for an empty stream.
func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// pidBudget bounds the wait for a fixture process to record its pid.
//
// Generous, and it may be: this is not the synchronization. Neither caller races
// this wait against anything — testDescendantsDieFirst holds the signal ladder
// until the pid arrives, and LeakPipeHolder's parent records the pid itself
// before the probe exits — so what remains for the bound to detect is "nothing
// was ever spawned", and a spawn that failed says so in its own file (#138). A
// tighter budget would only turn a slow machine into a red suite.
//
// Not unbounded, and not larger than this: a wait this long is what a *broken*
// ordering now costs the suite, and 10 s is already twenty-odd times the slowest
// registration measured under full-suite load.
const pidBudget = 10 * time.Second

func readPID(t *testing.T, path string) int {
	t.Helper()
	// The child writes its pid as it starts; give it a moment to appear.
	deadline := time.Now().Add(pidBudget)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if why, err := os.ReadFile(path + fixtureErrorSuffix); err == nil {
		t.Fatalf("no process ever recorded its pid at %s — the fixture failed first: %s", path, why)
	}
	t.Fatalf("no process ever recorded its pid at %s within %v, and the fixture reported no failure "+
		"of its own: the descendant was started and then lost, or never scheduled", path, pidBudget)
	return 0
}

// aliveNow reports whether pid is a live process at this instant. No polling on
// purpose: the tests that use it assert on ordering, and a retry loop would paper
// over exactly the window they exist to catch.
//
// It asks ps rather than signal 0, which is not the same question. A signal-0
// probe keeps succeeding for a few milliseconds after a process dies, until it is
// reaped — measured at ~8 ms for a SIGKILLed grandchild whose parent was already
// gone, while ps had stopped listing it. Using kill(pid, 0) made the ordering
// assertion fail under -race even though the process group really had been torn
// down first.
func aliveNow(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false // ps cannot find it
	}
	state := strings.TrimSpace(string(out))
	// A zombie is dead: it holds no resources and cannot touch the workspace.
	return state != "" && !strings.HasPrefix(state, "Z")
}

func resolve(path string) string {
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		return path
	}
	return p
}

// slowTranscripts widens the window between the harness exiting and the reader
// finishing, which is where a Wait-owned pipe silently discards output.
type slowTranscripts struct{ delay time.Duration }

func (s slowTranscripts) Open(core.RunSpec) (io.WriteCloser, io.WriteCloser, error) {
	// The delay is the transcript's; the retained prompt is written and closed
	// before the process starts and is not what this widens.
	return s, &promptRecorder{}, nil
}

func (s slowTranscripts) Write(p []byte) (int, error) {
	time.Sleep(s.delay)
	return len(p), nil
}
func (slowTranscripts) Close() error { return nil }

// recordingTranscript is a transcript sink that remembers whether it was closed,
// which is how a stranded reader goroutine becomes observable.
type recordingTranscript struct {
	mu       sync.Mutex
	buf      []byte
	perWrite []string
	closed   bool
	// prompt is the §9.5 retention sink, recorded separately: it is a different
	// artifact from the stream, and folding the two into one buffer would let a
	// prompt assertion pass on transcript bytes.
	prompt promptRecorder
}

// promptRecorder captures what Start wrote to the retained-prompt sink.
type promptRecorder struct {
	mu  sync.Mutex
	buf []byte
}

func (p *promptRecorder) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	return len(b), nil
}

func (p *promptRecorder) Close() error { return nil }

func (p *promptRecorder) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.buf)
}

func (rec *recordingTranscript) Open(core.RunSpec) (io.WriteCloser, io.WriteCloser, error) {
	return rec, &rec.prompt, nil
}

func (rec *recordingTranscript) Write(p []byte) (int, error) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.buf = append(rec.buf, p...)
	// Boundaries, not just bytes: testTranscriptWholeLines asserts on them.
	rec.perWrite = append(rec.perWrite, string(p))
	return len(p), nil
}

func (rec *recordingTranscript) writes() []string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return slices.Clone(rec.perWrite)
}

func (rec *recordingTranscript) Close() error {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	rec.closed = true
	return nil
}

func (rec *recordingTranscript) isClosed() bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.closed
}

func (rec *recordingTranscript) text() string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return string(rec.buf)
}

// testEvidenceAddressesWorkspace pins the binding SPEC §9.10's marker depends on:
// a run's evidence must arrive addressed to the workspace that run is in.
//
// Driven through core.RunnerKind.New, not the adapter's own constructor, because
// RunnerOptions.OnRun is the seam the daemon actually wires — a kind that accepts
// the option and drops it on the floor is the regression most worth catching, and
// only this entry point can see it.
//
// The two attempts overlap deliberately. One runner serves every issue, so an
// adapter that stashed "the current spec" on itself would satisfy a sequential
// test perfectly and mis-address every concurrent pair in production; with both
// Starts in flight, the second overwrites the first's field before either sink
// runs. Overlap is what makes the field visible as a field.
func testEvidenceAddressesWorkspace(t *testing.T, c Contract) {
	type record struct {
		workspace string
		evidence  core.RunEvidence
	}
	var mu sync.Mutex
	got := map[string]record{}

	runner, err := c.Kind.New(core.RunnerOptions{
		Provider:  c.block(t, scriptSuccess, nil),
		StopGrace: suiteTimings(harness.Timings{}).StopGrace,
		OnRun: func(spec core.RunSpec, e core.RunEvidence) error {
			mu.Lock()
			defer mu.Unlock()
			got[spec.Workspace.Path] = record{workspace: spec.Workspace.Path, evidence: e}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Kind.New: %v", err)
	}

	specs := []core.RunSpec{c.spec(t, core.RunLimits{}), c.spec(t, core.RunLimits{})}
	handles := make([]core.RunHandle, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := runner.Start(context.Background(), spec)
			if err != nil {
				t.Errorf("Start(%s): %v", spec.Workspace.Path, err)
				return
			}
			handles[i] = h
		}()
	}
	wg.Wait()
	for _, h := range handles {
		if h != nil {
			collect(t, h)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != len(specs) {
		t.Fatalf("sink recorded %d workspaces, want %d — one per attempt, each its own",
			len(got), len(specs))
	}
	seen := map[string]string{}
	for _, spec := range specs {
		rec, ok := got[spec.Workspace.Path]
		if !ok {
			t.Errorf("no evidence recorded for %s: the sink is not bound to the "+
				"attempt's own RunSpec", spec.Workspace.Path)
			continue
		}
		if rec.evidence.ID == "" || rec.evidence.Scheme == "" {
			t.Errorf("%s evidence = %+v, want a scheme and an id", spec.Workspace.Path, rec.evidence)
		}
		if prev, dup := seen[rec.evidence.ID]; dup {
			t.Errorf("%s and %s recorded run id %q: two workspaces cannot share one run",
				prev, spec.Workspace.Path, rec.evidence.ID)
		}
		seen[rec.evidence.ID] = spec.Workspace.Path
	}
}

// testEvidenceSurvivesInterleavedStarts is the deterministic half of the
// addressing requirement.
//
// Running two attempts concurrently is not enough on its own: an adapter that
// stashed "the current spec" on itself still wins that race almost every time,
// because the window between storing the spec and reading it back is a few
// microseconds wide. Verified — that mutation passes the concurrent test.
//
// So this holds both attempts open *inside* that window. The transcript store is
// the injectable seam that sits between a runner receiving a RunSpec and building
// the Launch from it, so a store that blocks until both attempts have reached it
// forces the interleaving instead of hoping for it: with a shared field, the
// second attempt overwrites the first's before either Launch is built.
func testEvidenceSurvivesInterleavedStarts(t *testing.T, c Contract) {
	const attempts = 2
	var mu sync.Mutex
	got := map[string]core.RunEvidence{}

	r := c.newRunner(t, c.block(t, scriptSuccess, nil), Options{
		Transcripts: newBarrierTranscripts(attempts),
		OnRun: func(spec core.RunSpec, e core.RunEvidence) error {
			mu.Lock()
			defer mu.Unlock()
			got[spec.Workspace.Path] = e
			return nil
		},
	})

	specs := make([]core.RunSpec, attempts)
	handles := make([]core.RunHandle, attempts)
	var wg sync.WaitGroup
	for i := range specs {
		specs[i] = c.spec(t, core.RunLimits{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := r.Start(context.Background(), specs[i])
			if err != nil {
				t.Errorf("Start(%s): %v", specs[i].Workspace.Path, err)
				return
			}
			handles[i] = h
		}()
	}
	wg.Wait()
	for _, h := range handles {
		if h != nil {
			collect(t, h)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	for _, spec := range specs {
		if _, ok := got[spec.Workspace.Path]; !ok {
			t.Errorf("no evidence recorded for %s while another attempt was in flight: "+
				"the runner is carrying one attempt's RunSpec where every attempt's "+
				"should be bound into its own sink", spec.Workspace.Path)
		}
	}
}

// barrierTranscripts releases Open only once every concurrent attempt has
// reached it, pinning them all inside the window between receiving a RunSpec and
// building a Launch from it.
type barrierTranscripts struct {
	mu      sync.Mutex
	arrived int
	want    int
	gate    chan struct{}
}

func newBarrierTranscripts(want int) *barrierTranscripts {
	return &barrierTranscripts{want: want, gate: make(chan struct{})}
}

func (b *barrierTranscripts) Open(spec core.RunSpec) (io.WriteCloser, io.WriteCloser, error) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.want {
		close(b.gate)
	}
	b.mu.Unlock()
	select {
	case <-b.gate:
	case <-time.After(30 * time.Second):
		return nil, nil, fmt.Errorf("barrier: only %d of %d attempts arrived", b.arrived, b.want)
	}
	return harness.NopTranscripts{}.Open(spec)
}
