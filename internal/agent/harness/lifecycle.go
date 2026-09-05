package harness

import (
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The lifecycle decision layer (SPEC §7.4).
//
// handle.go is goroutines over pipes, timers and signals. This file is the part
// of it that is a *decision*: which terminal event a run ends on, and when that
// event may be published. Nothing here does I/O, measures time, or blocks, so
// every ordering the goroutines can produce is reachable from a test by calling
// a function — including the ones a scheduler produces once in a thousand runs.
//
// That split is the answer to a specific problem. The outcome depends on five
// facts that arrive from five independent goroutines (the stream ended, the
// process exited, a liveness window claimed a verdict, the bounded teardown
// finished, the consumer left), and the rules relating them — first writer wins,
// a settled outcome never changes, publication waits for cleanup — used to live
// in comments around a mutex. `-race` does not check ordering, so the only way
// to test one of those rules was to force an interleaving through a hook
// threaded into production types, which does not scale to the dozen orderings
// that exist. As a pure transition table they are enumerable instead: see
// lifecycle_test.go, which walks the reachable state space exhaustively.

// inputKind is the closed set of things that can happen to a run. Each one is
// delivered by exactly one handle goroutine.
type inputKind uint8

const (
	// inEvent is one translated harness event (SPEC §7.2). A terminal one is
	// the harness declaring its own outcome.
	inEvent inputKind = iota
	// inStreamEnded is the stdout stream reaching its end — EOF, the post-exit
	// bound closing the read end, or the reader giving up on a line past the
	// scanner ceiling, which it has already claimed a verdict for by then
	// (handle.overflow).
	inStreamEnded
	// inProcessExited is Wait returning. It carries no exit code: they are
	// advisory only (SPEC §7.4), so the stream decides the outcome.
	inProcessExited
	// inVerdict is a runner-owned claim on the run's outcome: a liveness
	// window, a Stop, or the reader refusing a line past the scanner ceiling.
	inVerdict
	// inCleanupFinished is the bounded teardown that a verdict promised reaching a
	// conclusion, whatever its outcome (SPEC §7.5).
	inCleanupFinished
	// inAbandoned is the consumer never coming back for another event.
	inAbandoned
	// inResolve re-asks for a decision, after the driver waited out a cleanup
	// the machine asked for.
	inResolve
)

// input is one thing that happened, with whatever it carries.
type input struct {
	kind inputKind
	// event is the translated harness event, for inEvent.
	event core.Event
	// reason is the claimed liveness or Stop outcome, for inVerdict.
	reason core.FailureReason
}

// stepKind is what the driver must do about one input.
type stepKind uint8

const (
	// stepIdle: nothing to deliver.
	stepIdle stepKind = iota
	// stepEmit: deliver step.event; more may follow.
	stepEmit
	// stepTerminal: deliver step.event as the run's last, then end the stream.
	stepTerminal
	// stepAwaitCleanup: a claimed verdict's bounded teardown is still outstanding.
	// Wait for it — or for the consumer to be abandoned — then ask again with
	// inResolve.
	stepAwaitCleanup
)

// step is the driver's instruction. event is meaningful only for stepEmit and
// stepTerminal.
type step struct {
	kind  stepKind
	event core.Event
}

// lifecycleState is the whole of a run's decision state, as a value: `on`
// returns the next one, which is what makes the transition table enumerable.
type lifecycleState struct {
	// streamEnded and procExited are the two halves of "every byte the harness
	// wrote has been accounted for" (SPEC §7.5). Neither alone decides
	// anything: a result line can still be sitting in the pipe after the
	// process is gone, which is why this package owns the descriptors.
	streamEnded bool
	procExited  bool
	// outcome is the terminal event the run ended on, held until it may be
	// published. Meaningful only when hasOutcome.
	outcome    core.Event
	hasOutcome bool
	// verdict is the liveness or Stop decision, first writer wins. Empty means
	// the harness ended on its own terms.
	verdict core.FailureReason
	// cleanupDone records that promised teardown ran to a conclusion, so
	// a verdict can be told from "a verdict whose cleanup is still in flight".
	cleanupDone bool
	// abandoned records that no consumer will read another event.
	abandoned bool
	// settled closes the outcome: once the terminal event is chosen, a later
	// verdict still kills the process but can no longer change what was
	// published.
	settled bool
}

// on applies one input, returning the next state and what the driver must do.
//
// It is total — every input is legal in every state — because the goroutines
// that deliver them are independent, and a machine with illegal states would
// only move the ordering argument back into the driver where it cannot be
// enumerated.
func (s lifecycleState) on(in input) (lifecycleState, step) {
	switch in.kind {
	case inEvent:
		if s.settled || s.abandoned {
			// The run is over, or nobody is listening. The pump keeps reading
			// the stream either way — the transcript is a separate obligation
			// (SPEC §7.2) — but nothing more is delivered.
			return s, step{}
		}
		if !IsTerminal(in.event.Type) {
			return s, step{kind: stepEmit, event: in.event}
		}
		s.outcome, s.hasOutcome = in.event, true
		return s.resolve()

	case inStreamEnded:
		s.streamEnded = true
		return s.endOfRun()

	case inProcessExited:
		s.procExited = true
		return s.endOfRun()

	case inVerdict:
		// First writer wins, and a claim after publication is bookkeeping only:
		// the process is still killed — a run past its deadline must not keep
		// running — but the event does not change retroactively.
		if s.verdict == "" && !s.settled {
			s.verdict = in.reason
		}
		return s, step{}

	case inCleanupFinished:
		s.cleanupDone = true
		return s, step{}

	case inAbandoned:
		s.abandoned = true
		return s, step{}

	case inResolve:
		return s.resolve()
	}
	// inputKind is closed and every value is handled above; a new one that
	// reaches here is a programming error, and doing nothing quietly would hide
	// it behind a run that never publishes an outcome.
	panic("harness: unknown lifecycle input")
}

// endOfRun applies SPEC §7.4's unconditional rule: a process that exits without
// a terminal event crashed.
//
// It waits for *both* halves. Until the stream has ended as well, a result line
// may still be in the pipe, and judging the run on the exit alone is the loss
// this package owns its descriptors to avoid (SPEC §7.5). A no-stream-at-all
// exit is therefore crashed too, not launch_error — launch_error belongs to a
// Start that never produced a handle, which is returned as an error rather than
// an event. The stderr text that would distinguish the two is preserved in the
// transcript instead (see handle.finishTranscript), so nothing is lost but the
// reason stays inside the locked taxonomy.
func (s lifecycleState) endOfRun() (lifecycleState, step) {
	if !s.streamEnded || !s.procExited || s.hasOutcome || s.settled {
		return s, step{}
	}
	s.outcome = core.Event{Type: core.EventFailed, Reason: core.FailureCrashed}
	s.hasOutcome = true
	return s.resolve()
}

// resolve publishes the run's outcome, or reports that a claimed verdict's
// bounded teardown has to finish first.
//
// Choosing the event and closing the window on later verdicts happen in one
// transition, and that is the whole point of resolving here. Choosing earlier —
// before the cleanup wait — leaves a gap as wide as the teardown in which
// the watchdog can claim a timeout that an already-chosen `succeeded` then
// overwrites; liveness is runner-owned (SPEC §7.4), so a late result must never
// overturn a declared timeout.
//
// The wait is asked for at most once, and that bound is in the table rather than
// in a comment: stepAwaitCleanup requires a verdict whose cleanup has not
// finished *and* a consumer still reading, and the driver's only two ways out of
// that wait are the two facts that make the next resolve settle.
func (s lifecycleState) resolve() (lifecycleState, step) {
	switch {
	case s.settled, !s.hasOutcome:
		// Published already, or the run has not ended: nothing to decide.
		return s, step{}
	case s.verdict == "":
		// The harness ended on its own terms, so its own terminal event stands
		// — and from here a late verdict can no longer change it.
	case s.cleanupDone:
		// A liveness or Stop verdict outranks whatever the stream said. Waiting
		// for teardown is what publication owes the orchestrator: announcing
		// a failure while a descendant is still running would hand back a
		// workspace it believes is idle and is not (SPEC §9.8).
		s.outcome = core.Event{Type: core.EventFailed, Reason: s.verdict}
	case s.abandoned:
		// Nobody is left to read the event, so the wait above protects nothing:
		// what the orchestrator acts on for an abandoned run is Stop's
		// confirmed/unconfirmed answer, not an event nothing will drain
		// (SPEC §9.8, and see handle.Done). Waiting anyway would be the worse
		// trade — it leaves the run permanently undecided if the teardown that was
		// promised never reports, and an undecided run never closes Done.
		s.outcome = core.Event{Type: core.EventFailed, Reason: s.verdict}
	default:
		return s, step{kind: stepAwaitCleanup}
	}
	s.settled = true
	return s, step{kind: stepTerminal, event: s.outcome}
}

// lifecycle is the state machine plus the mutex serializing the goroutines that
// feed it. It is the handle's only shared decision state; what remains under the
// handle's own lock is data (the stderr tail), not policy.
type lifecycle struct {
	mu sync.Mutex
	s  lifecycleState
}

func (l *lifecycle) on(in input) step {
	l.mu.Lock()
	defer l.mu.Unlock()
	next, st := l.s.on(in)
	l.s = next
	return st
}

// claimed reports whether a verdict has been claimed — which is also the
// promise that bounded teardown will run, so it is what decides whether there is
// any cleanup to wait for (see handle.awaitCleanup).
func (l *lifecycle) claimed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.s.verdict != ""
}
