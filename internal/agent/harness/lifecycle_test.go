package harness

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The lifecycle state machine (lifecycle.go) is the decision layer behind
// handle.go's goroutines, and it is tested here rather than through a real
// process on purpose.
//
// What a run *means* depends on five facts arriving from five independent
// goroutines, and the rules relating them are ordering rules: a claimed verdict
// beats a late result, a settled outcome never changes, publication waits for
// cleanup. `-race` finds data races, not ordering bugs, and a subprocess test can
// only stage the orderings a script can aim at — the interesting ones are
// nanoseconds wide. Because the machine is pure, every ordering is reachable by
// calling a function, so these tests do not sample the orderings; they enumerate
// them:
//
//	TestLifecycleTransitions   the decision table, one row per rule
//	TestLifecycleModelCheck    every reachable state × every input, with the
//	                           invariants checked on each edge
//	TestLifecycleOrderings     every permutation of the fact set produces
//	                           exactly one terminal event, and the right one
//
// The conformance suite still owns everything that is only true of a real
// process: that the stream really is drained before it is judged, that the
// signal ladder really reaches a descendant, that the transcript really lands.

// --- inputs, for readable tables ---

func evStarted() input {
	return input{kind: inEvent, event: core.Event{Type: core.EventStarted, SessionID: "s"}}
}
func evProgress() input {
	return input{kind: inEvent, event: core.Event{Type: core.EventProgress, Text: "working"}}
}
func evSucceeded() input {
	return input{kind: inEvent, event: core.Event{Type: core.EventSucceeded}}
}
func evFailed(r core.FailureReason) input {
	return input{kind: inEvent, event: core.Event{Type: core.EventFailed, Reason: r}}
}
func streamEnded() input { return input{kind: inStreamEnded} }
func procExited() input  { return input{kind: inProcessExited} }
func cleanupDone() input { return input{kind: inCleanupFinished} }
func abandoned() input   { return input{kind: inAbandoned} }
func resolveNow() input  { return input{kind: inResolve} }
func verdict(r core.FailureReason) input {
	return input{kind: inVerdict, reason: r}
}

// crashed is SPEC §7.4's unconditional default for a process that exits without
// a terminal event.
var crashed = core.Event{Type: core.EventFailed, Reason: core.FailureCrashed}

func failed(r core.FailureReason) core.Event {
	return core.Event{Type: core.EventFailed, Reason: r}
}

// --- the decision table ---

// TestLifecycleTransitions is the transition table as rules: a sequence of
// inputs, and what the machine must do about the last one. Every row is a claim
// SPEC §7.2–§7.5 makes, or a claim about an ordering that used to be argued in a
// comment.
func TestLifecycleTransitions(t *testing.T) {
	for _, tc := range []struct {
		name string
		// before are inputs fed first, whose steps are not asserted.
		before []input
		// in is the input under test.
		in   input
		want step
	}{
		// --- the ordinary stream (SPEC §7.2) ---
		{
			name: "a non-terminal event is delivered",
			in:   evProgress(),
			want: step{kind: stepEmit, event: evProgress().event},
		},
		{
			name:   "the harness's own terminal event ends the run",
			before: []input{evStarted(), evProgress()},
			in:     evSucceeded(),
			want:   step{kind: stepTerminal, event: core.Event{Type: core.EventSucceeded}},
		},
		{
			name: "a harness failure is published as the harness reported it",
			in:   evFailed(core.FailureRateLimited),
			want: step{kind: stepTerminal, event: failed(core.FailureRateLimited)},
		},
		{
			name:   "nothing is delivered after the run is settled",
			before: []input{evSucceeded()},
			in:     evProgress(),
			want:   step{},
		},
		{
			name:   "a second terminal event cannot publish either",
			before: []input{evSucceeded()},
			in:     evFailed(core.FailureRateLimited),
			want:   step{},
		},
		{
			name:   "nothing is delivered once the consumer is gone",
			before: []input{abandoned()},
			in:     evProgress(),
			want:   step{},
		},

		// --- the crash default (SPEC §7.4) ---
		{
			name: "the stream ending alone decides nothing",
			in:   streamEnded(),
			want: step{},
		},
		{
			name: "the process exiting alone decides nothing: a result line may still be in the pipe",
			in:   procExited(),
			want: step{},
		},
		{
			name:   "exit with the stream drained and no terminal event is crashed",
			before: []input{evStarted(), streamEnded()},
			in:     procExited(),
			want:   step{kind: stepTerminal, event: crashed},
		},
		{
			name:   "the same in the other order",
			before: []input{evStarted(), procExited()},
			in:     streamEnded(),
			want:   step{kind: stepTerminal, event: crashed},
		},
		{
			// Not launch_error: that belongs to a Start that never produced a
			// handle, and is returned as an error rather than an event.
			name:   "no stream at all is crashed too",
			before: []input{procExited()},
			in:     streamEnded(),
			want:   step{kind: stepTerminal, event: crashed},
		},
		{
			name:   "a published outcome is not overwritten by the crash default",
			before: []input{evSucceeded(), streamEnded()},
			in:     procExited(),
			want:   step{},
		},

		// --- verdict precedence (SPEC §7.4: liveness is runner-owned) ---
		{
			name: "claiming a verdict publishes nothing by itself: the kill ends the stream",
			in:   verdict(core.FailureTimeout),
			want: step{},
		},
		{
			name:   "a verdict whose cleanup finished outranks a late harness success",
			before: []input{verdict(core.FailureTimeout), cleanupDone()},
			in:     evSucceeded(),
			want:   step{kind: stepTerminal, event: failed(core.FailureTimeout)},
		},
		{
			name:   "and outranks the crash default",
			before: []input{verdict(core.FailureStalled), cleanupDone(), procExited()},
			in:     streamEnded(),
			want:   step{kind: stepTerminal, event: failed(core.FailureStalled)},
		},
		{
			name:   "publication waits for the ladder the verdict promised (SPEC §9.8)",
			before: []input{verdict(core.FailureTimeout)},
			in:     evSucceeded(),
			want:   step{kind: stepAwaitCleanup},
		},
		{
			name:   "and publishes the verdict once it finishes",
			before: []input{verdict(core.FailureTimeout), evSucceeded(), cleanupDone()},
			in:     resolveNow(),
			want:   step{kind: stepTerminal, event: failed(core.FailureTimeout)},
		},
		{
			name:   "first verdict wins; a second cannot relabel the failure",
			before: []input{verdict(core.FailureStalled), verdict(core.FailureKilled), cleanupDone()},
			in:     evSucceeded(),
			want:   step{kind: stepTerminal, event: failed(core.FailureStalled)},
		},
		{
			// The window the removed BeforePublish hook existed to force does not
			// exist here: choosing the event and closing the door on later
			// verdicts are one transition, so a verdict is either already
			// standing when the outcome is chosen (the rows above) or it arrives
			// after the event was published, and then it is bookkeeping.
			name:   "a verdict claimed after publication changes nothing",
			before: []input{evSucceeded(), verdict(core.FailureTimeout), cleanupDone()},
			in:     resolveNow(),
			want:   step{},
		},

		// --- abandonment ---
		{
			name:   "an abandoned run with a verdict does not wait for a ladder that may never report",
			before: []input{verdict(core.FailureKilled), abandoned()},
			in:     resolveNow(),
			want:   step{},
		},
		{
			// The wait a verdict parks publication in is released by the consumer
			// leaving, too, and it resolves rather than staying undecided: a run
			// stuck here would never close its handle's Done.
			name:   "abandonment releases the cleanup wait and resolves",
			before: []input{verdict(core.FailureKilled), evSucceeded(), abandoned()},
			in:     resolveNow(),
			want:   step{kind: stepTerminal, event: failed(core.FailureKilled)},
		},

		// --- resolve is inert until there is something to decide ---
		{
			name: "resolve before the run has ended decides nothing",
			in:   resolveNow(),
			want: step{},
		},
		{
			name:   "resolve with a verdict but no end of run still decides nothing",
			before: []input{verdict(core.FailureTimeout), cleanupDone()},
			in:     resolveNow(),
			want:   step{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var s lifecycleState
			for _, in := range tc.before {
				s, _ = s.on(in)
			}
			_, got := s.on(tc.in)
			if got != tc.want {
				t.Errorf("on(%s) = %s, want %s", describe(tc.in), describeStep(got), describeStep(tc.want))
			}
		})
	}
}

// --- the model check ---

// every input the driver can deliver, with a second verdict and a second
// terminal event so precedence is exercised rather than assumed.
func allInputs() []input {
	return []input{
		evProgress(),
		evSucceeded(),
		evFailed(core.FailureRateLimited),
		streamEnded(),
		procExited(),
		verdict(core.FailureTimeout),
		verdict(core.FailureStalled),
		cleanupDone(),
		abandoned(),
		resolveNow(),
	}
}

// TestLifecycleModelCheck walks the whole reachable state space — every state
// the driver can put the machine in, crossed with every input it can deliver —
// and checks the lifecycle's invariants on each edge.
//
// This is what "exhaustively tested" has to mean for an ordering contract. A
// table asserts the rules somebody thought to write down; this asserts that no
// reachable combination of the same facts, in any order, can violate them. The
// state is a handful of booleans plus a verdict, so the closure is small enough
// to walk completely.
func TestLifecycleModelCheck(t *testing.T) {
	start := lifecycleState{}
	seen := map[lifecycleState]bool{start: true}
	queue := []lifecycleState{start}
	edges := 0

	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		for _, in := range allInputs() {
			next, st := s.on(in)
			edges++
			checkInvariants(t, s, next, st)
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}

	// A canary, not a contract. It is pinned because the number moving means the
	// state shape changed, and then the invariants below — which are about
	// *this* set of facts — deserve a fresh read rather than a green tick.
	t.Logf("reachable states = %d, edges checked = %d", len(seen), edges)
	const wantStates = 168
	if len(seen) != wantStates {
		t.Errorf("reachable states = %d, want %d: the machine's state shape changed, so re-read "+
			"the invariants in checkInvariants before repinning this", len(seen), wantStates)
	}

	// Every reachable state must be able to finish. A state that can never
	// publish is a run the orchestrator waits on forever.
	for s := range seen {
		if !canSettle(s) {
			t.Errorf("state %s can never publish a terminal event", describeState(s))
		}
		// A settled run has something it published; the two are set together.
		if s.settled && !s.hasOutcome {
			t.Errorf("state %s is settled with no outcome", describeState(s))
		}
	}
}

// checkInvariants asserts the lifecycle's contract on one transition.
func checkInvariants(t *testing.T, from, to lifecycleState, st step) {
	t.Helper()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf("%s: %s", fmt.Sprintf(format, args...), describeEdge(from, to, st))
	}

	// SPEC §7.2: exactly one terminal event per attempt, and it is the last.
	// Settling is what records that it has been published, so it is one-way, and
	// nothing may be emitted after it.
	if from.settled && !to.settled {
		fail("a settled outcome was reopened")
	}
	if from.settled && st.kind != stepIdle {
		fail("a settled run produced a step")
	}
	if st.kind == stepTerminal && !to.settled {
		fail("a terminal event was published without settling")
	}
	if st.kind == stepTerminal && from.settled {
		fail("a second terminal event")
	}
	if to.settled && !from.settled && st.kind != stepTerminal {
		fail("the run settled without publishing anything")
	}

	// SPEC §7.2: only a terminal event type may be published as terminal, and a
	// terminal type may never be published as a mid-run event.
	if st.kind == stepTerminal && !IsTerminal(st.event.Type) {
		fail("terminal step carries the non-terminal event %v", st.event.Type)
	}
	if st.kind == stepEmit && IsTerminal(st.event.Type) {
		fail("a terminal event %v was emitted mid-run", st.event.Type)
	}

	// SPEC §7.4: liveness is runner-owned. A verdict standing when the outcome is
	// chosen decides it; the harness's own event cannot overturn it.
	if st.kind == stepTerminal && from.verdict != "" {
		if st.event.Type != core.EventFailed || st.event.Reason != from.verdict {
			fail("published %v(%v) over the standing verdict %v",
				st.event.Type, st.event.Reason, from.verdict)
		}
	}

	// First writer wins: a verdict is never relabelled or cleared.
	if from.verdict != "" && to.verdict != from.verdict {
		fail("the verdict changed from %v to %v", from.verdict, to.verdict)
	}

	// SPEC §9.8: a verdict is published only once the ladder it promised has
	// finished — unless nobody is left to read the event, in which case the wait
	// protects nothing and would leave the run undecided.
	if st.kind == stepTerminal && from.verdict != "" && !from.cleanupDone && !from.abandoned {
		fail("a verdict was published before its cleanup finished")
	}

	// The cleanup wait is bounded by the table rather than by a comment, and
	// this is the bound: it is only ever asked for while a verdict is standing
	// and both facts that end the wait are still outstanding, and once either
	// arrives the next resolve settles. handle.feed therefore goes round at most
	// twice — awaitCleanup returns only after one of those facts has been fed
	// (handle.signalLadder, handle.abortEmit).
	if st.kind == stepAwaitCleanup {
		if from.verdict == "" || from.cleanupDone || from.abandoned {
			fail("asked to wait for a cleanup that is not outstanding")
		}
		for _, ending := range []input{cleanupDone(), abandoned()} {
			released, _ := to.on(ending)
			if _, again := released.on(resolveNow()); again.kind != stepTerminal {
				fail("after %s the resolve produced %s instead of settling",
					describe(ending), describeStep(again))
			}
		}
	}

	// Facts are monotone: nothing un-ends the stream, un-exits the process,
	// un-finishes a cleanup, or brings a consumer back.
	for _, m := range []struct {
		name     string
		was, now bool
	}{
		{"streamEnded", from.streamEnded, to.streamEnded},
		{"procExited", from.procExited, to.procExited},
		{"cleanupDone", from.cleanupDone, to.cleanupDone},
		{"abandoned", from.abandoned, to.abandoned},
		{"hasOutcome", from.hasOutcome, to.hasOutcome},
	} {
		if m.was && !m.now {
			fail("%s went back to false", m.name)
		}
	}
}

// canSettle reports whether some sequence of inputs from s reaches a settled
// state — a property of the machine, not of one path into it.
func canSettle(s lifecycleState) bool {
	seen := map[lifecycleState]bool{s: true}
	queue := []lifecycleState{s}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.settled {
			return true
		}
		for _, in := range allInputs() {
			next, _ := cur.on(in)
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return false
}

// --- orderings ---

// TestLifecycleOrderings drives every ordering of the facts that decide a run
// through the machine the way handle.feed does, and checks each against a rule
// stated over the *input sequence* rather than over the machine's state.
//
// This is what the removed BeforePublish hook was an approximation of. The hook
// forced one interleaving — the one somebody noticed was wrong — and these
// enumerate the orderings nobody thought of. The rule is the one SPEC §7.4
// fixes: whatever was standing when the outcome was chosen decides it, a
// liveness verdict outranks the harness's own word, and a process that exits
// with nothing declared crashed.
//
// Only causally reachable orderings are driven (see causal): a line cannot arrive
// after the stream ended, and a cleanup cannot finish before the verdict that
// promised it, so an ordering that violates those is not a scheduling the driver
// can produce and asserting anything about it says nothing.
func TestLifecycleOrderings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		facts []input
	}{
		{
			name:  "harness success",
			facts: []input{evStarted(), evProgress(), evSucceeded(), streamEnded(), procExited()},
		},
		{
			// The case the conformance suite stages with scriptLateSuccess, in
			// every order rather than the one the scheduler happened to produce.
			name: "timeout claimed against a harness success",
			facts: []input{
				evStarted(), evSucceeded(), streamEnded(), procExited(),
				verdict(core.FailureTimeout), cleanupDone(),
			},
		},
		{
			name: "stall claimed against a silent harness",
			facts: []input{
				streamEnded(), procExited(), verdict(core.FailureStalled), cleanupDone(),
			},
		},
		{
			name:  "no terminal event and no verdict",
			facts: []input{evStarted(), streamEnded(), procExited()},
		},
		// Abandonment is deliberately not a fact set here. The driver stops
		// feeding the moment the consumer is gone (handle.dispatch returns on
		// abort), so orderings that continue past it are not schedulings — the
		// one thing it decides, releasing the cleanup wait, is asserted in the
		// table above and from every reachable state by the model check.
	} {
		t.Run(tc.name, func(t *testing.T) {
			checked := 0
			permute(tc.facts, func(order []input) {
				if !causal(order) {
					return
				}
				checked++
				res := drive(order)

				// SPEC §7.2: exactly one terminal event per attempt.
				if res.published != 1 {
					t.Fatalf("%s published %d terminal events, want exactly 1",
						describeAll(order), res.published)
				}
				// SPEC §7.2: and it is the last thing delivered.
				if res.emittedAfterTerminal {
					t.Fatalf("%s emitted an event after the terminal one", describeAll(order))
				}
				if got, want := res.terminal, expected(res); got != want {
					t.Fatalf("%s published %s, want %s",
						describeAll(order), describeStep(step{kind: stepTerminal, event: got}),
						describeStep(step{kind: stepTerminal, event: want}))
				}
			})
			if checked == 0 {
				t.Fatal("no causally reachable ordering: the filter rejects everything")
			}
			t.Logf("%d causally reachable orderings", checked)
		})
	}
}

// expected is the rule, restated over what the driver had fed by the time the
// outcome was chosen. SPEC §7.4: a liveness verdict outranks the harness's own
// terminal event, and an exit with nothing declared is crashed.
func expected(res driveResult) core.Event {
	switch {
	case res.verdictAtPublication != "":
		return failed(res.verdictAtPublication)
	case res.harnessDeclared:
		return res.harnessTerminal
	default:
		return crashed
	}
}

// driveResult is what one ordering produced, alongside the facts that were in
// hand when it produced it.
type driveResult struct {
	terminal             core.Event
	published            int
	emittedAfterTerminal bool
	// verdictAtPublication is the first verdict fed before the outcome was
	// chosen; harnessTerminal is the last terminal event the harness declared
	// before then.
	verdictAtPublication core.FailureReason
	harnessTerminal      core.Event
	harnessDeclared      bool
}

// drive feeds one ordering exactly as handle.feed does, including going round
// again whenever the machine asks for a cleanup wait.
//
// The wait is answered the way awaitCleanup's two exits are: the fact that ends
// it is recorded before the resolve, because both channels that release that wait
// are closed after the lifecycle has been told (handle.signalLadder,
// handle.abortEmit). An ordering that never supplies a cleanup still terminates,
// which is the driver-side half of the bound.
func drive(order []input) driveResult {
	var s lifecycleState
	var res driveResult
	// The facts as the driver has fed them, tracked independently of the state
	// so the oracle above is not the machine's own bookkeeping read back.
	var verdictSoFar core.FailureReason
	var harnessSoFar core.Event
	harnessSeen := false

	feed := func(in input) {
		switch {
		case in.kind == inVerdict && verdictSoFar == "":
			verdictSoFar = in.reason // first writer wins
		case in.kind == inEvent && IsTerminal(in.event.Type):
			harnessSoFar, harnessSeen = in.event, true
		}
		for {
			var st step
			s, st = s.on(in)
			switch st.kind {
			case stepEmit:
				if res.published > 0 {
					res.emittedAfterTerminal = true
				}
				return
			case stepTerminal:
				if res.published == 0 {
					res.terminal = st.event
					res.verdictAtPublication = verdictSoFar
					res.harnessTerminal, res.harnessDeclared = harnessSoFar, harnessSeen
				}
				res.published++
				return
			case stepAwaitCleanup:
				// The driver blocks here, and the fact that releases it is fed
				// first. These orderings all have a live consumer, so it is the
				// ladder finishing; abandonment is the other exit and is covered
				// by the table and the model check.
				s, _ = s.on(cleanupDone())
				in = resolveNow()
			default:
				return
			}
		}
	}
	for _, in := range order {
		feed(in)
	}
	// The run must be decided by the end of its facts, with nothing left owing.
	feed(resolveNow())
	return res
}

// causal rejects an ordering the driver could not produce. Two constraints are
// enough for the fact sets above:
//
//   - every line is handed up before the reader closes the channel, so no event
//     can follow the stream's end (handle.readStdout);
//   - only a claimed verdict promises a signal ladder, so no cleanup can finish
//     before one (handle.claim). reap walks the ladder without claiming, but that
//     is after publication and is not among these facts.
func causal(order []input) bool {
	streamOver, claimed := false, false
	for _, in := range order {
		switch in.kind {
		case inStreamEnded:
			streamOver = true
		case inEvent:
			if streamOver {
				return false
			}
		case inVerdict:
			claimed = true
		case inCleanupFinished:
			if !claimed {
				return false
			}
		}
	}
	return true
}

// permute calls fn with every ordering of in. The fact sets here are ≤6 long, so
// this is 720 orderings at most.
func permute(in []input, fn func([]input)) {
	perm := slices.Clone(in)
	var rec func(int)
	rec = func(k int) {
		if k == len(perm) {
			fn(slices.Clone(perm))
			return
		}
		for i := k; i < len(perm); i++ {
			perm[k], perm[i] = perm[i], perm[k]
			rec(k + 1)
			perm[k], perm[i] = perm[i], perm[k]
		}
	}
	rec(0)
}

// --- the two facts that pair the machine to its driver ---

// The lifecycle is fed under a mutex and read through claimed(), which is what
// handle.awaitCleanup consults to decide whether any cleanup was promised. A
// verdict that did not show up there would skip the wait SPEC §9.8 requires.
func TestLifecycleClaimedTracksTheVerdict(t *testing.T) {
	var l lifecycle
	if l.claimed() {
		t.Error("claimed() before any verdict")
	}
	l.on(evProgress())
	if l.claimed() {
		t.Error("an ordinary event claimed a verdict")
	}
	l.on(verdict(core.FailureStalled))
	if !l.claimed() {
		t.Error("a claimed verdict is invisible to awaitCleanup, so nothing would wait for its ladder")
	}
	// Settling does not release the promise: the ladder still has to finish
	// before Done reports the run over (handle.reap).
	l.on(cleanupDone())
	l.on(evSucceeded())
	if !l.claimed() {
		t.Error("the verdict was forgotten once the run settled")
	}
}

// A verdict claimed after publication is recorded for nothing but bookkeeping,
// and must not be recorded at all — the process is still killed by the caller,
// but a claim that changed the state could reopen an outcome that has already
// been reported.
func TestLifecycleVerdictAfterSettling(t *testing.T) {
	var s lifecycleState
	s, st := s.on(evSucceeded())
	if st.kind != stepTerminal {
		t.Fatalf("setup: %s", describeStep(st))
	}
	s, st = s.on(verdict(core.FailureTimeout))
	if st.kind != stepIdle {
		t.Errorf("a post-publication verdict produced %s", describeStep(st))
	}
	if s.verdict != "" {
		t.Errorf("verdict recorded as %q after settling; the outcome could be reopened", s.verdict)
	}
	if _, st := s.on(resolveNow()); st.kind != stepIdle {
		t.Errorf("resolve after settling produced %s", describeStep(st))
	}
}

// An input kind the table does not handle must fail loudly. Doing nothing
// quietly would show up as a run that never publishes an outcome, which is the
// hardest failure in this package to trace back.
func TestLifecycleRejectsUnknownInput(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("an unknown input was accepted silently")
		}
	}()
	var s lifecycleState
	s.on(input{kind: inResolve + 1})
}

// --- describers, so a failing table row reads as a sentence ---

func describe(in input) string {
	switch in.kind {
	case inEvent:
		return fmt.Sprintf("event(%v)", in.event.Type)
	case inStreamEnded:
		return "streamEnded"
	case inProcessExited:
		return "procExited"
	case inVerdict:
		return fmt.Sprintf("verdict(%v)", in.reason)
	case inCleanupFinished:
		return "cleanupFinished"
	case inAbandoned:
		return "abandoned"
	case inResolve:
		return "resolve"
	}
	return "?"
}

func describeAll(ins []input) string {
	parts := make([]string, len(ins))
	for i, in := range ins {
		parts[i] = describe(in)
	}
	return "[" + strings.Join(parts, " → ") + "]"
}

func describeStep(st step) string {
	switch st.kind {
	case stepIdle:
		return "idle"
	case stepEmit:
		return fmt.Sprintf("emit(%v)", st.event.Type)
	case stepTerminal:
		if st.event.Reason != "" {
			return fmt.Sprintf("terminal(%v: %v)", st.event.Type, st.event.Reason)
		}
		return fmt.Sprintf("terminal(%v)", st.event.Type)
	case stepAwaitCleanup:
		return "awaitCleanup"
	}
	return "?"
}

func describeState(s lifecycleState) string {
	var on []string
	for _, f := range []struct {
		name string
		set  bool
	}{
		{"streamEnded", s.streamEnded},
		{"procExited", s.procExited},
		{"hasOutcome", s.hasOutcome},
		{"cleanupDone", s.cleanupDone},
		{"abandoned", s.abandoned},
		{"settled", s.settled},
	} {
		if f.set {
			on = append(on, f.name)
		}
	}
	if s.verdict != "" {
		on = append(on, "verdict="+string(s.verdict))
	}
	if s.hasOutcome {
		on = append(on, "outcome="+string(s.outcome.Type))
	}
	if len(on) == 0 {
		return "{}"
	}
	return "{" + strings.Join(on, " ") + "}"
}

func describeEdge(from, to lifecycleState, st step) string {
	return fmt.Sprintf("%s ⇒ %s, step %s", describeState(from), describeState(to), describeStep(st))
}
