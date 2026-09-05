package fake

import (
	"context"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// RunEvidenceScheme names this fake substrate's opaque execution-domain
// evidence. It intentionally resembles neither local nor remote production
// encoding; consumers must not interpret any of them.
const RunEvidenceScheme = "fake-domain-v1"

// Runner is a scriptable agent runner. A test says what the next run should
// do — succeed, fail with a §7.3 reason, report usage, exit without a
// terminal event — and the orchestrator sees exactly that.
type Runner struct {
	mu sync.Mutex

	// Everything scriptable is behind setters: the orchestrator calls Start
	// and Stop from worker goroutines, so a test that assigned a field
	// directly would be racing the code under test.
	script    func(spec core.RunSpec, attempt int) []core.Event
	failStart error
	// onRun is the §9.10 evidence sink, and evidence is what this runner reports
	// about each launch. Nil sink means nothing records the run, which is what a
	// caller with no marker to upgrade looks like (a readiness probe, a test that
	// is not about recovery).
	onRun     func(core.RunSpec, core.RunEvidence)
	evidence  func(core.RunSpec) core.RunEvidence
	stopTerm  core.Termination
	stopGate  func()
	probeGate func()
	eventGate func(int)
	hang      bool
	linger    bool
	holdDone  bool
	startGate func()
	// domainMembers models an execution domain that outlives direct execution:
	// Done closes, and the domain remains live until Stop cleans it. Probe
	// reports it honestly, making the post-Done path testable (#79, #234).
	domainMembers bool

	// Specs records every Start, in order.
	Specs []core.RunSpec
	// Handles are the runs started, in order.
	Handles []*Handle
	// attempts counts starts per workspace path, so Script can branch.
	attempts map[string]int
}

func NewRunner() *Runner {
	return &Runner{
		attempts: map[string]int{},
		// Stated, not defaulted. core.Termination's zero value is unconfirmed
		// (SPEC §9.8), so a fake that left this unset would quietly put every
		// test — including the ones that are not about claim retention at all —
		// on the retention path, and each of them would fail as a timeout rather
		// than as the thing it was checking.
		stopTerm: core.TerminationConfirmed,
	}
}

// SetScript installs the event script for subsequent runs. The attempt number
// is 1-based per workspace.
func (r *Runner) SetScript(fn func(spec core.RunSpec, attempt int) []core.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.script = fn
}

// SetFailStart makes the next Start return err.
func (r *Runner) SetFailStart(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failStart = err
}

// SetProbeGate blocks inside Probe until the test lets it out. It exists for the
// same reason SetStopGate does: an observation that is *in flight* while another
// decision lands is a window nothing else can stage, and after #79 that window
// belongs to the probe on every ordinary run.
func (r *Runner) SetProbeGate(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.probeGate = fn
}

func (r *Runner) pGate() func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.probeGate
}

// SetHoldDone keeps `done` open after the domain has gone quiet, until the test
// releases it or a Stop lands. It is how a fixture expresses the state the real
// harness is in for as long as its transcript takes to finish writing: the domain
// is quiet, and `Done` — which means the process *and* the record — is not closed
// yet (#79). Nothing else can produce a confirmed observation while the phase
// edge is still open.
func (r *Runner) SetHoldDone(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.holdDone = on
}

func (r *Runner) holdsDone() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.holdDone
}

// SetDomainMembers makes every run's execution domain outlive direct
// execution: Probe answers unconfirmed even after Done, until a Stop reports
// confirmed.
func (r *Runner) SetDomainMembers(on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.domainMembers = on
}

func (r *Runner) hasDomainMembers() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.domainMembers
}

// SetStopTermination controls what Stop reports, for runs already started as
// well as future ones — §9.8's retry path needs a stop that only confirms on
// a later attempt.
func (r *Runner) SetStopTermination(t core.Termination) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopTerm = t
}

// SetStartGate installs a hook that runs inside Start, before it returns. The
// real Start execs a process and can take as long as that takes, so "a launch
// still in flight" is an ordinary state — and the only way to put a decision
// (an exit, a shutdown) inside that window deterministically.
func (r *Runner) SetStartGate(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startGate = fn
}

func (r *Runner) sGate() func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.startGate
}

// SetStopGate installs a hook that runs inside Stop, after the call is
// recorded and before it answers. It holds bounded teardown open, which is the
// only way to interleave a stop with a decision that overtakes it.
func (r *Runner) SetStopGate(fn func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopGate = fn
}

// SetEventGate installs a hook run immediately before each scripted event is
// emitted, with the 1-based index of the event about to be sent.
//
// It models the one thing a slice of events cannot: a harness emits its stream
// *over time*, so a decision can land between two lines of one run. Every other
// gate here stages a window around the run (a launch in flight, a ladder
// walking); this is the only one that stages a window *inside* it, which is what
// a rule reading accumulated state — §9.9's cost cap against a record whose
// situation changed mid-run — needs to be tested against.
//
// Without it a test can only choose whether all of a run's events land before
// the decision or after it, and the interesting cases are the ones that straddle.
func (r *Runner) SetEventGate(fn func(i int)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.eventGate = fn
}

func (r *Runner) eGate() func(int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.eventGate
}

// SetHangAfterScript leaves the event channel open after the scripted events,
// so a run ends only when it is stopped — or when the test ends it directly
// (Handle.EndRun).
func (r *Runner) SetHangAfterScript(hang bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.hang = hang
}

// SetLingerAfterStream keeps the *process* alive after its event stream has
// closed, which is what the real adapter does while its stop grace runs: the
// pump closes Events as soon as it has emitted the terminal event, and Done
// closes only once the process has been reaped (SPEC §7.4, claudecode
// handle.reap). Nothing else distinguishes the two, so a fake that closed them
// together could never show a run whose process outlives its stream.
//
// The run is released by Handle.ReleaseProcess, or by a stop.
func (r *Runner) SetLingerAfterStream(linger bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.linger = linger
}

func (r *Runner) lingers() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.linger
}

func (r *Runner) stopTermination() core.Termination {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopTerm
}

func (r *Runner) gate() func() {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stopGate
}

func (r *Runner) Start(_ context.Context, spec core.RunSpec) (core.RunHandle, error) {
	// Outside the lock and before the failure branch: the window a caller cares
	// about is "the launch is out and has not answered yet", and a failing launch
	// is in it just as much as a succeeding one — it is the case that hands the
	// caller a nil handle.
	if gate := r.sGate(); gate != nil {
		gate()
	}
	r.mu.Lock()
	if r.failStart != nil {
		err := r.failStart
		r.mu.Unlock()
		return nil, err
	}
	r.Specs = append(r.Specs, spec)
	r.attempts[spec.Workspace.Path]++
	attempt := r.attempts[spec.Workspace.Path]
	script := r.script
	hang := r.hang
	sink, evidence := r.onRun, r.evidence

	h := &Handle{
		events:      make(chan core.Event, 16),
		done:        make(chan struct{}),
		stopped:     make(chan struct{}),
		release:     make(chan struct{}),
		doneRelease: make(chan struct{}),
		discarded:   make(chan struct{}),
		runner:      r,
		// A started run has a live domain until something ends it.
		domainLive: true,
	}
	r.Handles = append(r.Handles, h)
	r.mu.Unlock()

	// The run exists, so its evidence is recorded — after the process and before
	// the handle reaches the caller, which is where both real adapters call the
	// sink and where agenttest.Contract enforces that they do
	// (harness.Start, core.RunEvidenceSink).
	//
	// Modelled here rather than left to fixtures because the ordering is the
	// contract §9.10 rests on. A fake that upgraded the marker at some other
	// moment — or not at all — would make every restart read as unknown_launch,
	// and a test asserting an orphan resumes would then be asserting a park.
	// The sink cannot be scripted to fail, and returns nothing for that reason. A
	// sink failure is the adapter's ladder concern — §7.4 forbids reporting it as
	// "nothing is running", and agenttest.Contract already holds both adapters to
	// that at two entry points. A failure seam here would only let an orchestrator
	// test assert against a path the orchestrator does not own.
	if sink != nil {
		sink(spec, evidence(spec))
	}

	go func() {
		var events []core.Event
		if script != nil {
			events = script(spec, attempt)
		}
		for i, ev := range events {
			// Before the send, so a test holding the gate holds a run that has
			// emitted everything up to here and nothing after it — the state a
			// decision has to be able to land in.
			if gate := r.eGate(); gate != nil {
				gate(i + 1)
			}
			select {
			case h.events <- ev:
			case <-h.stopped:
				h.closeStream()
				h.reap()
				return
			}
		}
		if hang {
			<-h.stopped
		}
		// The stream ends here. Whether the process has ended with it is a
		// separate question, and Done is the only thing that answers it.
		h.closeStream()
		if r.lingers() {
			select {
			case <-h.release:
			case <-h.stopped:
			}
		}
		// Direct execution is over. Its domain is too, unless the test says a
		// member outlived it — and this happens *before* reap, so a domain can be quiet
		// while the transcript is still being written, which is the ordinary
		// harness's shape and a state `done` cannot express.
		if !r.hasDomainMembers() {
			h.domainEnded()
		}
		if r.holdsDone() {
			// The domain is quiet; the record is not written yet.
			//
			// A *discard* ends the wait, an interrupt does not, and the asymmetry
			// is the real handle's: discard closes `abort` and abandons the reader
			// that owns the transcript, so there is nothing left to finish, while
			// an interrupt deliberately leaves the record being written
			// (harness handle.Stop, SPEC §7.2). Releasing on either would make
			// Done effectively simultaneous with a confirmed Stop — a guarantee
			// the real handle does not make, and one a caller can be built to
			// depend on.
			select {
			case <-h.doneRelease:
			case <-h.discarded:
			}
		}
		h.reap()
	}()

	return h, nil
}

// Handle is one scripted run.
type Handle struct {
	events  chan core.Event
	done    chan struct{}
	stopped chan struct{}
	probes  int
	// domainLive is tracked apart from direct execution and transcript completion
	// because the three are independent facts. It changes only on natural domain
	// quiet or a confirmed Stop, never merely because Done closes.
	domainLive  bool
	release     chan struct{}
	doneRelease chan struct{}
	// discarded closes on a StopDiscard. It is what ends a held transcript early,
	// because discard is the mode that abandons the reader; see the holdsDone
	// wait in Start.
	discarded chan struct{}
	runner    *Runner

	once            sync.Once
	doneOnce        sync.Once
	releaseOnce     sync.Once
	doneReleaseOnce sync.Once
	stopOnce        sync.Once
	discardOnce     sync.Once

	mu    sync.Mutex
	stops []core.StopMode
}

// closeStream ends the event stream: the adapter has emitted its terminal
// event and has nothing more to say.
func (h *Handle) closeStream() { h.once.Do(func() { close(h.events) }) }

// reap ends the process. Done means this and only this (SPEC §7.4).
func (h *Handle) reap() { h.doneOnce.Do(func() { close(h.done) }) }

// ReleaseDone lets a run whose domain is already quiet finish its record. See
// Runner.SetHoldDone.
func (h *Handle) ReleaseDone() { h.doneReleaseOnce.Do(func() { close(h.doneRelease) }) }

// ReleaseProcess lets a lingering run finally exit. See
// Runner.SetLingerAfterStream.
func (h *Handle) ReleaseProcess() { h.releaseOnce.Do(func() { close(h.release) }) }

// EndRun models direct execution exiting with nobody having asked it to: the
// stream closes, the domain goes quiet, and the run is reaped.
//
// It is the one thing SetHangAfterScript cannot express on its own, and SPEC
// §9.10 is what needs it. A hung run otherwise ends only through Stop — the
// daemon acting — while the question recovery asks is about a run *no daemon
// stopped*: this one ended, and whether its whole domain went with it is a
// property of the world rather than of anything BEN did. Without this, a fixture
// whose prober answers "quiet" could leave a live domain, which is two worlds at
// once and pins neither.
//
// It records no stop, deliberately. Stops() stays empty, so a test can still tell
// a run that was asked to stop from one that simply ended — the distinction §7.4
// and §9.8 both turn on.
//
// It returns with the **domain already quiet**. Closing `stopped` only wakes the
// run goroutine, which records that fact a hop later; doing it synchronously keeps
// Probe and the world the fixture describes from disagreeing.
//
// Deliberately not a wait on Done(): that is the process *and* its record (#79),
// it can be held open on purpose (SetHoldDone), and it is not the fact a run
// prober reports.
func (h *Handle) EndRun() {
	h.stopOnce.Do(func() { close(h.stopped) })
	h.domainEnded()
}

func (h *Handle) Events() <-chan core.Event { return h.events }
func (h *Handle) Done() <-chan struct{}     { return h.done }

// Probe observes without acting, as the real handle's does (SPEC §7.5, #79): no
// teardown, and — unlike Stop — it does not end a lingering run.
//
// The answer is derived rather than configured: direct execution still running,
// or a retained domain member after it ends, means the domain is not quiet.
func (h *Handle) Probe(ctx context.Context) core.Termination {
	h.mu.Lock()
	h.probes++
	h.mu.Unlock()
	// Recorded before the gate, so a test that waits for ProbeCount knows the
	// observation is standing in it.
	if gate := h.runner.pGate(); gate != nil {
		gate()
	}
	if ctx.Err() != nil {
		return core.TerminationUnconfirmed
	}
	// Done is deliberately not consulted: transcript completion and domain quiet
	// are independent facts.
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.domainLive {
		return core.TerminationUnconfirmed
	}
	return core.TerminationConfirmed
}

// domainEnded records that the run's execution domain is positively quiet.
func (h *Handle) domainEnded() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.domainLive = false
}

// ProbeCount reports how many times this run's domain was observed.
func (h *Handle) ProbeCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.probes
}

func (h *Handle) Stop(_ context.Context, mode core.StopMode) core.Termination {
	h.mu.Lock()
	h.stops = append(h.stops, mode)
	h.mu.Unlock()

	// Recorded before the gate, so a test that waits for StopCount knows the
	// ladder is standing in it.
	if gate := h.runner.gate(); gate != nil {
		gate()
	}

	// The event stream closes either way, as claude-code's does: an
	// unconfirmed stop means the *process* may still be alive, not that the
	// adapter keeps streaming. The orchestrator must still retain the claim
	// and be able to retry the stop, which is what makes this the faithful
	// shape to test against.
	h.stopOnce.Do(func() { close(h.stopped) })
	if mode == core.StopDiscard {
		h.discardOnce.Do(func() { close(h.discarded) })
	}
	term := h.runner.stopTermination()
	if term == core.TerminationConfirmed {
		// Teardown cleaned it, which is the whole difference between Stop and
		// Probe: a retained domain member is gone after this and a later
		// observation must agree.
		h.domainEnded()
	}
	return term
}

// StopCount reports how many times this run was asked to stop.
func (h *Handle) StopCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.stops)
}

// Stops reports the modes this run was asked to stop with, in order.
//
// The mode is part of the contract rather than an implementation detail:
// StopDiscard abandons the transcript reader mid-stream, and SPEC §7.2 keeps
// the raw stream verbatim. A caller that only counts stops cannot tell a probe
// that preserved the forensic record from one that truncated it.
func (h *Handle) Stops() []core.StopMode {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]core.StopMode(nil), h.stops...)
}

// LastHandle returns the most recently started run, or nil.
func (r *Runner) LastHandle() *Handle {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Handles) == 0 {
		return nil
	}
	return r.Handles[len(r.Handles)-1]
}

// LastSpec returns the RunSpec the most recent run was launched with, and
// whether there was one. It is how a test reads the limits a launch actually
// carried, as opposed to the ones the record was dispatched under.
func (r *Runner) LastSpec() (core.RunSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Specs) == 0 {
		return core.RunSpec{}, false
	}
	return r.Specs[len(r.Specs)-1], true
}

// StartCount reports how many runs were started.
// SetEvidenceSink installs the §9.10 run-evidence sink the assembly wires
// between a runner and its workspace provider (core.RunnerOptions.OnRun).
//
// evidence may be nil, in which case every launch reports an opaque fake-domain
// identity keyed by workspace path.
func (r *Runner) SetEvidenceSink(sink func(core.RunSpec, core.RunEvidence), evidence func(core.RunSpec) core.RunEvidence) {
	if evidence == nil {
		evidence = func(spec core.RunSpec) core.RunEvidence {
			return core.RunEvidence{
				Scheme: RunEvidenceScheme,
				ID:     "run-" + spec.Workspace.Path,
				Boot:   "fake-boot-1",
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRun, r.evidence = sink, evidence
}

func (r *Runner) StartCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Specs)
}

// Prompts returns the prompt of each started run, in order.
func (r *Runner) Prompts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.Specs))
	for _, s := range r.Specs {
		out = append(out, s.Prompt)
	}
	return out
}

// Continuations returns the continuation token each run was started with.
func (r *Runner) Continuations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.Specs))
	for _, s := range r.Specs {
		out = append(out, s.Continuation)
	}
	return out
}

// Succeed is the common script: one started event, then success.
func Succeed(session string) []core.Event {
	return []core.Event{
		{Type: core.EventStarted, SessionID: session, Continuation: session},
		{Type: core.EventSucceeded},
	}
}

// Fail is the common failure script.
func Fail(reason core.FailureReason) []core.Event {
	return []core.Event{
		{Type: core.EventStarted, SessionID: "s", Continuation: "s"},
		{Type: core.EventFailed, Reason: reason},
	}
}
