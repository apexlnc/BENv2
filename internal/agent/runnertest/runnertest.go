// Package runnertest is the **universal** slice of the AgentRunner contract:
// the outcomes any substrate owes, asserted through core.RunHandle and nothing
// else (#192, SPEC §7.1–§7.5).
//
// # Why it is a separate suite
//
// internal/agent/agenttest is the local suite, and most of it is local by
// subject rather than by accident. It re-execs the test binary as a harness,
// reads the child's argv out of `ps`-visible state, asserts the prompt arrives
// on stdin and the process runs with cwd set to the worktree, and drives a
// signal ladder against a real process *group* until the kernel answers ESRCH.
// Every one of those is a statement about a POSIX process on the daemon's own
// host. A remote substrate has no pid to report and no cwd to be launched in,
// and a fake of one would be exactly the "invents a guarantee the real component
// does not make" that AGENTS.md warns about: it would let code depending on the
// invention pass.
//
// What a remote substrate *does* owe is the outcomes — a stream that starts with
// `started` and ends with a terminal event and then closes; a terminal event
// that is ground truth; an observation that does not act; a stop that reports
// honestly and an unconfirmed one that stays unconfirmed. Those are here, and
// they are asserted through the interface alone, so a runner passes them by
// behaving rather than by exposing anything.
//
// # What is deliberately absent, and how that is held
//
// No assertion here may name a local operating-system fact: no pid, process
// group, cwd, argv, stdin, environment variable, signal number or file
// descriptor. That is not a convention — TestSuiteAssertsNoLocalOSFacts reads
// this package's own source and fails on the identifiers, so a case added later
// that reaches for `os.Getpid` or `t.Setenv` breaks the separation loudly
// instead of quietly making the "universal" suite local again.
//
// Two v1 behaviours are also absent, and their absence is the interesting part
// of the boundary rather than an omission:
//
//   - **Liveness windows.** A stall or attempt timeout is the runner's
//     obligation (SPEC §7.4), but the *mechanism* differs: locally the adapter
//     watches a pipe and kills a group, remotely it must ask a backend that may
//     not answer. Both are asserted where they are implemented.
//   - **Context cancellation.** Locally a cancelled context discards the run and
//     reports killed, and the process really is gone. Remotely a consumer
//     disconnect is *not* a remote cancel — the backend was not asked for
//     anything and the run may still be executing — so the same input yields the
//     same event and a deliberately different fact about the world. A universal
//     assertion over it would have to pick one, and picking either would make the
//     other substrate lie.
package runnertest

import (
	"context"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Scenario is the closed set of runs the universal contract needs a substrate to
// be able to produce.
//
// Closed and small on purpose. Every entry is a shape both a local process and a
// foreign one can genuinely be in, and a substrate that cannot script one of them
// is one this contract cannot speak about — which is a fact worth a compile
// error rather than a skipped subtest.
type Scenario string

const (
	// ScenarioSuccess: started (with a session id), one progress event, one
	// usage event, then succeeded. The run then ends and the substrate goes
	// quiet.
	ScenarioSuccess Scenario = "success"
	// ScenarioFailure: started, then failed with a §7.3 reason. The run ends and
	// the substrate goes quiet.
	ScenarioFailure Scenario = "failure"
	// ScenarioLive: started, then nothing. The run stays live — and the
	// substrate reports it live — until something stops it.
	ScenarioLive Scenario = "live"
	// ScenarioUnstoppable: ScenarioLive, except that a stop cannot confirm the
	// run is gone. It is the shape that makes the claim retention of SPEC §9.8
	// reachable, and the one a substrate is most tempted to be optimistic about.
	ScenarioUnstoppable Scenario = "unstoppable"
)

// Scenarios is the closed set, in the order the suite runs them.
func Scenarios() []Scenario {
	return []Scenario{ScenarioSuccess, ScenarioFailure, ScenarioLive, ScenarioUnstoppable}
}

// Contract is what a substrate supplies. One function: everything else the suite
// needs it reads off the handle.
type Contract struct {
	// Name identifies the substrate in test output.
	Name string
	// Start launches a run scripted for one scenario and returns its handle. It
	// is called once per case; a substrate that needs per-run cleanup registers
	// it with t.Cleanup.
	//
	// It returns a handle rather than a runner because core.RunHandle is the
	// whole of what is universal. core.AgentRunner carries Ready and
	// Capabilities, which are statements about a configuration and a harness —
	// and the local fake, which has neither, would have to grow them to be
	// speakable here.
	Start func(t *testing.T, s Scenario) core.RunHandle
	// SessionID is the identity ScenarioSuccess and ScenarioFailure announce on
	// their started event.
	SessionID string
	// FailureReason is the §7.3 reason ScenarioFailure ends with.
	FailureReason core.FailureReason
}

// Run executes the universal contract against one substrate. A substrate's
// package calls it from a single test:
//
//	func TestUniversalContract(t *testing.T) { runnertest.Run(t, contract()) }
func Run(t *testing.T, c Contract) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.fn(t, c) })
	}
}

type universalCase struct {
	name string
	fn   func(*testing.T, Contract)
}

// cases is the suite. Package-level so its shape can be checked by the audit
// beside it, in agenttest's idiom.
var cases = []universalCase{
	{"SuccessStreamShape", testSuccessShape},
	{"FailureCarriesAClosedReason", testFailureReason},
	{"NothingIsPublishedAfterTheTerminalEvent", testNothingAfterTerminal},
	{"DoneFollowsTheClosedStream", testDoneFollowsStream},
	{"EveryEventTypeIsInTheClosedSet", testClosedEnums},
	{"ProbeObservesWithoutEndingTheRun", testProbeObservesOnly},
	{"ProbeWithoutLookingIsUnconfirmed", testProbeUnconfirmedWithoutLooking},
	{"ProbeConfirmsAFinishedRun", testProbeConfirmsFinished},
	{"StopEndsTheStreamAndConfirms", testStopConfirms},
	{"StopIsHonestWhenItCannotConfirm", testStopUnconfirmed},
	{"StopIsRepeatable", testStopRepeatable},
}

// The happy path: the normalized sequence, in order, ending in a terminal event
// after which the stream closes (SPEC §7.2, §7.4).
func testSuccessShape(t *testing.T, c Contract) {
	h := c.Start(t, ScenarioSuccess)
	evs := collect(t, h)

	want := []core.EventType{core.EventStarted, core.EventProgress, core.EventUsage, core.EventSucceeded}
	got := types(evs)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
	if evs[0].SessionID != c.SessionID {
		t.Errorf("started event session = %q, want %q", evs[0].SessionID, c.SessionID)
	}
	if evs[2].Usage == nil {
		t.Error("usage event carries no Usage")
	}
}

// A failure ends with a reason from the closed §7.3 taxonomy, and the substrate
// does not get to invent one.
func testFailureReason(t *testing.T, c Contract) {
	h := c.Start(t, ScenarioFailure)
	evs := collect(t, h)

	last := evs[len(evs)-1]
	if last.Type != core.EventFailed {
		t.Fatalf("last event = %v, want %v", last.Type, core.EventFailed)
	}
	if last.Reason != c.FailureReason {
		t.Errorf("failure reason = %q, want %q", last.Reason, c.FailureReason)
	}
	if !knownReason(last.Reason) {
		t.Errorf("failure reason %q is outside the closed §7.3 taxonomy", last.Reason)
	}
}

// The terminal event is ground truth: nothing follows it on the stream
// (SPEC §7.4). Asserted as the last element rather than as a channel that closed
// promptly, because a substrate that published one more event *and* closed would
// pass the second reading.
func testNothingAfterTerminal(t *testing.T, c Contract) {
	for _, s := range []Scenario{ScenarioSuccess, ScenarioFailure} {
		t.Run(string(s), func(t *testing.T) {
			evs := collect(t, c.Start(t, s))
			for i, ev := range evs {
				terminal := ev.Type == core.EventSucceeded || ev.Type == core.EventFailed
				if terminal && i != len(evs)-1 {
					t.Fatalf("event %d is terminal (%v) but %d events follow it: %v",
						i, ev.Type, len(evs)-i-1, types(evs))
				}
			}
		})
	}
}

// Done closes at the run's terminal phase edge, and never before the stream
// has. Domain quiet remains a separate Probe/Stop fact.
func testDoneFollowsStream(t *testing.T, c Contract) {
	h := c.Start(t, ScenarioSuccess)
	collect(t, h)
	select {
	case <-h.Done():
	case <-time.After(waitFor):
		t.Fatal("Done did not close after the event stream did")
	}
}

// Only the closed enums cross the boundary (SPEC §3.6, §7.2).
func testClosedEnums(t *testing.T, c Contract) {
	for _, s := range []Scenario{ScenarioSuccess, ScenarioFailure} {
		for _, ev := range collect(t, c.Start(t, s)) {
			if !knownType(ev.Type) {
				t.Errorf("%s: event type %q is outside the closed §7.2 set", s, ev.Type)
			}
			if ev.Type == core.EventFailed && !knownReason(ev.Reason) {
				t.Errorf("%s: failure reason %q is outside the closed §7.3 taxonomy", s, ev.Reason)
			}
		}
	}
}

// Probe observes and does not act (SPEC §7.5, #79): a live run is reported live,
// and it is still live afterwards.
//
// The second half is what makes this more than a spelling check. A Probe
// implemented as "stop it and see" would report unconfirmed for a live run too,
// and only the run surviving distinguishes them.
func testProbeObservesOnly(t *testing.T, c Contract) {
	h := c.Start(t, ScenarioLive)
	t.Cleanup(func() { h.Stop(context.Background(), core.StopDiscard) })

	if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
		t.Fatalf("Probe of a live run = %v, want %v", got, core.TerminationUnconfirmed)
	}
	select {
	case <-h.Done():
		t.Fatal("the run ended under a Probe; Probe must not act on it")
	default:
	}
	if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
		t.Errorf("second Probe = %v, want %v — the first one ended the run", got, core.TerminationUnconfirmed)
	}
}

// Not having looked is not evidence of anything (SPEC §7.5, §9.8): a probe whose
// context is already cancelled reports unconfirmed.
func testProbeUnconfirmedWithoutLooking(t *testing.T, c Contract) {
	h := c.Start(t, ScenarioSuccess)
	collect(t, h)
	<-h.Done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := h.Probe(ctx); got != core.TerminationUnconfirmed {
		t.Errorf("Probe with a cancelled context = %v, want %v", got, core.TerminationUnconfirmed)
	}
}

// A run that has finished and whose substrate has gone quiet is confirmed. This
// is the positive control for the case above: without it, a Probe hard-wired to
// unconfirmed would pass every other assertion here.
func testProbeConfirmsFinished(t *testing.T, c Contract) {
	h := c.Start(t, ScenarioSuccess)
	collect(t, h)
	<-h.Done()

	if got := h.Probe(context.Background()); got != core.TerminationConfirmed {
		t.Errorf("Probe of a finished, quiet run = %v, want %v", got, core.TerminationConfirmed)
	}
}

// A stop ends the stream and reports the fact it established. Both modes, because
// the event stream closes either way — an unconfirmed stop means the run may
// still be alive, not that the substrate keeps streaming it.
func testStopConfirms(t *testing.T, c Contract) {
	for _, mode := range []core.StopMode{core.StopInterrupt, core.StopDiscard} {
		t.Run(modeName(mode), func(t *testing.T) {
			h := c.Start(t, ScenarioLive)
			if got := h.Stop(context.Background(), mode); got != core.TerminationConfirmed {
				t.Errorf("Stop = %v, want %v", got, core.TerminationConfirmed)
			}
			drain(t, h)
			select {
			case <-h.Done():
			case <-time.After(waitFor):
				t.Fatal("Done did not close after a stop")
			}
		})
	}
}

// The verdict that retains the claim (SPEC §9.8). A substrate that cannot prove
// the run is gone must say so; reporting confirmed would put a second agent in a
// workspace the first one may still hold.
func testStopUnconfirmed(t *testing.T, c Contract) {
	h := c.Start(t, ScenarioUnstoppable)
	if got := h.Stop(context.Background(), core.StopDiscard); got != core.TerminationUnconfirmed {
		t.Errorf("Stop of an unstoppable run = %v, want %v", got, core.TerminationUnconfirmed)
	}
	drain(t, h)
}

// Stop is asked again on the retry path, so it must be safe to ask again.
func testStopRepeatable(t *testing.T, c Contract) {
	h := c.Start(t, ScenarioLive)
	first := h.Stop(context.Background(), core.StopDiscard)
	drain(t, h)
	if second := h.Stop(context.Background(), core.StopDiscard); second != first {
		t.Errorf("second Stop = %v, first = %v: a repeated stop must report the same settled fact", second, first)
	}
}

// waitFor bounds the suite's waits. Generous, because nothing here is a
// measurement — every case waits for an event a script has already decided to
// send, so the only thing this number can express is how loaded the machine is.
const waitFor = 30 * time.Second

// collect drains the stream and returns every event, failing if it does not end.
func collect(t *testing.T, h core.RunHandle) []core.Event {
	t.Helper()
	var out []core.Event
	deadline := time.After(waitFor)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				if len(out) == 0 {
					t.Fatal("the event stream closed without a single event")
				}
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatalf("the event stream did not close within %s; events so far: %v", waitFor, types(out))
			return nil
		}
	}
}

// drain reads the stream to its close without asserting on what is in it, for
// the cases whose subject is the stop rather than the events.
func drain(t *testing.T, h core.RunHandle) {
	t.Helper()
	deadline := time.After(waitFor)
	for {
		select {
		case _, ok := <-h.Events():
			if !ok {
				return
			}
		case <-deadline:
			t.Fatalf("the event stream did not close within %s of a stop", waitFor)
			return
		}
	}
}

func types(evs []core.Event) []core.EventType {
	out := make([]core.EventType, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Type)
	}
	return out
}

func knownType(t core.EventType) bool {
	switch t {
	case core.EventStarted, core.EventProgress, core.EventUsage,
		core.EventHeartbeat, core.EventSucceeded, core.EventFailed:
		return true
	}
	return false
}

func knownReason(r core.FailureReason) bool {
	switch r {
	case core.FailureCrashed, core.FailureStalled, core.FailureTimeout,
		core.FailureRateLimited, core.FailureAuth, core.FailureLaunchError,
		core.FailureKilled, core.FailureBudgetExceeded, core.FailureCredential,
		core.FailureOutputOverflow:
		return true
	}
	return false
}

func modeName(m core.StopMode) string {
	if m == core.StopDiscard {
		return "discard"
	}
	return "interrupt"
}
