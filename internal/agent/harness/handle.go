package harness

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// handle is one live attempt (SPEC §7.1). Exactly one goroutine — the pump —
// writes to events, so the terminal event is unambiguous and the channel has a
// single closer. Liveness lives in a separate watchdog goroutine on purpose:
// the timeouts must fire whether or not anyone is draining events.
//
// Everything below is I/O: each goroutine turns something that happened into a
// lifecycle input and carries out the step it gets back. What the run *means* —
// which terminal event, and when it may be published — is decided in
// lifecycle.go, where the orderings can be enumerated instead of argued.
type handle struct {
	launch  Launch
	timings Timings
	cmd     *exec.Cmd
	// pgid is the child's process group; Setpgid makes it the leader, so the
	// group id is its pid (SPEC §7.5).
	pgid   int
	limits core.RunLimits

	// stdout and stderr are read ends this package owns. They are deliberately
	// not cmd.StdoutPipe/StderrPipe: Wait closes those the instant the process
	// exits, discarding whatever is still buffered — which loses the terminal
	// result line and turns a successful run into a phantom crash.
	stdout, stderr *os.File

	// life is every lifecycle decision this run makes (see lifecycle.go).
	life lifecycle

	// redact keeps this run's credential values out of the one event field a
	// consumer may retain (see emit). Nil when there is nothing to replace.
	redact *redactor

	events chan core.Event
	done   chan struct{}

	lines    chan []byte
	activity chan struct{}
	exited   chan struct{}
	// terminated closes once the pump has emitted a terminal event.
	terminated chan struct{}
	// cleaned closes when a promised signal ladder has finished, whatever its
	// outcome. Nothing about the run is published before then (awaitCleanup).
	cleaned     chan struct{}
	cleanedOnce sync.Once
	// flushed closes once the transcript has been written and closed. Done
	// waits for it, so "the run is over" also means "the forensic record is
	// complete" (see reap).
	flushed chan struct{}
	// recorded closes once the run's evidence has been offered to the marker
	// sink and its verdict is known (SPEC §9.10). The pump waits for it before
	// publishing anything, because a verdict claimed after a terminal event has
	// already been published cannot replace it — a fast child would otherwise be
	// reported as `succeeded` while its workspace stayed possibly-live with an
	// un-upgraded marker. The readers, Wait and the watchdog all start ahead of
	// it: reaping in particular must be running, or the ladder a failed sink
	// walks would poll a zombie that answers signal 0 forever.
	recorded chan struct{}
	// abort unblocks a pending emit when the consumer is not coming back.
	abort     chan struct{}
	abortOnce sync.Once
	// stranded releases a reader parked handing a line to a pump that is not
	// collecting. It does not end the reader: the transcript is this package's
	// obligation whether or not anyone is reading events (see readStdout).
	stranded     chan struct{}
	strandedOnce sync.Once
	closeOnce    sync.Once

	mu sync.Mutex
	// tail is the bounded stderr kept for the transcript. It is the only shared
	// state left under this lock: every lifecycle decision moved to life, which
	// carries its own.
	tail []byte
}

func newHandle(l Launch, cmd *exec.Cmd, stdout, stderr *os.File) *handle {
	return &handle{
		launch:     l,
		timings:    l.Timings,
		cmd:        cmd,
		pgid:       cmd.Process.Pid,
		limits:     l.Limits,
		redact:     newRedactor(l.Redact),
		stdout:     stdout,
		stderr:     stderr,
		events:     make(chan core.Event, eventBuffer),
		done:       make(chan struct{}),
		lines:      make(chan []byte),
		activity:   make(chan struct{}, 1),
		exited:     make(chan struct{}),
		terminated: make(chan struct{}),
		cleaned:    make(chan struct{}),
		flushed:    make(chan struct{}),
		stranded:   make(chan struct{}),
		recorded:   make(chan struct{}),
		abort:      make(chan struct{}),
	}
}

func (h *handle) Events() <-chan core.Event { return h.events }

// Done is closed when the harness process has been reaped *and* the transcript
// has been written and closed. It reports the process, not the whole group: a
// descendant that survives SIGKILL is reported through Stop's unconfirmed
// verdict instead (SPEC §9.8), because that is the answer the orchestrator acts
// on when deciding whether to retain a claim.
//
// Including the transcript is what makes Done usable as the signal to archive
// or dispose: the record is written by the reader goroutines, which nothing else
// synchronizes with, so a Done that meant only "the process is gone" would let a
// reader see a truncated forensic record (SPEC §10.3).
func (h *handle) Done() <-chan struct{} { return h.done }

func (h *handle) run(ctx context.Context, transcript io.WriteCloser) {
	// Bounds the stdin-copying goroutine: a harness that exits without reading
	// the whole prompt would otherwise keep Wait from returning.
	h.cmd.WaitDelay = h.timings.PostExitDrain

	var readers sync.WaitGroup
	readers.Add(2)
	go func() { defer readers.Done(); h.readStdout(transcript) }()
	go func() { defer readers.Done(); h.readStderr() }()
	go func() {
		// Exit codes are advisory only (SPEC §7.4), so the Wait error is
		// deliberately discarded: what the stream did decides the outcome.
		// Wait is safe to run alongside the readers because the pipes are ours,
		// not cmd's — it has no descriptors of theirs to close.
		_ = h.cmd.Wait()
		close(h.exited)
	}()
	go func() {
		defer close(h.flushed)
		readers.Wait()
		// Safe only here: the readers are done with the descriptors. Closing
		// them any earlier — on process exit, say — discards whatever the OS
		// pipe still holds, which is the very loss this ownership avoids.
		h.closePipes()
		h.finishTranscript(transcript)
	}()
	go h.watchdog(ctx)
	go h.pump()
	go h.reap()
	go h.boundStream()
}

// boundStream owns the post-exit life of the output pipes, and owns it on a
// goroutine of its own for the same reason the watchdog does: a consumer that
// stops draining events parks the pump, and anything the pump owns stops
// happening.
//
// That is not hypothetical. The bound used to live in the pump's select, so a
// parked pump left the reader goroutines parked behind it, the transcript
// unfinished, and the pipes open — and the only way for Done to stay honest was
// to give up on the transcript after a timeout, which is the same truncated
// record one layer along. Here the bound is unconditional, so the readers always
// finish and the record is always complete: Done waits for it rather than racing
// it (see reap).
//
// The window itself is the same one it always was: only a descendant still
// holding the write end reaches it, and when it elapses the read ends are closed
// so EOF arrives regardless.
func (h *handle) boundStream() {
	select {
	case <-h.exited:
	case <-h.abort:
		// Nobody will read this run's events again, so there is nothing left to
		// deliver and no reason to wait out the window.
		h.closePipes()
		return
	}
	// The bound may not start while publication is gated (see recorded). The
	// pump is waiting, not absent, so stranding here would release the readers
	// from a collector that is about to arrive — and for a child that exits
	// faster than the marker upgrade takes, the line stranded is the terminal
	// one, turning a successful run into failed/crashed. The window itself is
	// unchanged; it now begins when collection can, which is what it always
	// assumed.
	select {
	case <-h.recorded:
	case <-h.abort:
		h.closePipes()
		return
	}
	if h.sleep(h.timings.PostExitDrain) {
		return // the stream ended on its own; the pipes are closed already
	}
	// The window elapsed with the stream still open, which means one of two
	// things, and they want opposite treatment. If the pump is parked because
	// nobody is draining events, the data is all still there to be read: strand
	// the readers so they stop waiting on it and drain the rest into the
	// transcript, and the record ends up complete. If instead a descendant is
	// holding the write end, no amount of waiting produces EOF, and only closing
	// the read ends ends the stream — at the cost of whatever the pipe still
	// holds, which is the one case where the record is knowably short.
	//
	// Trying the cheap one first costs a second window and is the difference
	// between a complete transcript and a truncated one for every undrained run.
	h.strand()
	if h.sleep(h.timings.PostExitDrain) {
		return
	}
	h.closePipes()
}

// sleep waits out d, reporting whether the transcript finished first.
func (h *handle) sleep(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-h.flushed:
		return true
	case <-timer.C:
		return false
	}
}

// readStdout is the harness stream reader: every line is retained verbatim in
// the transcript (SPEC §7.2) and handed to the pump for translation.
func (h *handle) readStdout(transcript io.Writer) {
	defer close(h.lines)

	sc := bufio.NewScanner(h.stdout)
	sc.Buffer(make([]byte, 0, 64<<10), maxScanLine)
	// handOff goes false once the event stream has no consumer; the transcript
	// keeps filling either way.
	handOff := true
	for sc.Scan() {
		// One copy serves both consumers: the scanner reuses its buffer, and
		// appending to its token slice would write into buffered bytes it has
		// not handed out yet.
		tok := sc.Bytes()
		buf := make([]byte, len(tok)+1)
		copy(buf, tok)
		buf[len(tok)] = '\n'
		// Forensics are best-effort: losing the transcript must not lose the
		// run, so a write error is not fatal.
		_, _ = transcript.Write(buf)
		h.noteActivity()
		if !handOff {
			continue
		}
		select {
		case h.lines <- buf[:len(tok)]:
		case <-h.abort:
			handOff = false
		case <-h.stranded:
			// Nobody is collecting events any more, but the transcript is a
			// separate obligation (SPEC §7.2, §10.3) and every line above this
			// point already reached it. So the reader stops delivering and keeps
			// *reading*: what is left in the scanner and in the pipe still
			// belongs in the record, and returning here would close a file that
			// is missing the end of the run.
			handOff = false
		}
	}
	// A scanner error — a line past maxScanLine, or the read end closed by the
	// post-exit bound — ends the stream. The lifecycle then classifies from
	// whether a terminal event arrived (inStreamEnded).
}

// readStderr drains stderr, which the child would otherwise block on once the
// pipe filled, and keeps a bounded tail for the transcript. Its traffic counts
// as liveness: a run writing diagnostics is not stalled even while stdout is
// quiet, and the attempt timeout still bounds it.
func (h *handle) readStderr() {
	buf := make([]byte, 4096)
	for {
		n, err := h.stderr.Read(buf)
		if n > 0 {
			h.mu.Lock()
			h.tail = append(h.tail, buf[:n]...)
			if len(h.tail) > stderrTail {
				h.tail = h.tail[len(h.tail)-stderrTail:]
			}
			h.mu.Unlock()
			h.noteActivity()
		}
		if err != nil {
			return
		}
	}
}

// finishTranscript appends the retained stderr as a BEN-namespaced line before
// closing the transcript. The event model has nowhere to put this text, and a
// launch failure whose only explanation was on stderr would otherwise leave an
// operator with a bare `crashed` and no cause.
func (h *handle) finishTranscript(transcript io.WriteCloser) {
	defer transcript.Close()
	h.mu.Lock()
	tail := string(h.tail)
	h.mu.Unlock()
	if tail == "" {
		return
	}
	if line, err := json.Marshal(map[string]string{"type": "ben:stderr", "text": tail}); err == nil {
		_, _ = transcript.Write(append(line, '\n'))
	}
}

func (h *handle) noteActivity() {
	select {
	case h.activity <- struct{}{}:
	default:
	}
}

// watchdog owns the two runner-enforced windows (SPEC §7.4). It runs on its own
// goroutine so a consumer that stops draining events cannot postpone a
// timeout: the process is killed on schedule regardless, and the terminal event
// is delivered whenever the consumer returns.
func (h *handle) watchdog(ctx context.Context) {
	stall := newTimer(h.limits.StallTimeout)
	defer stall.stop()
	attempt := newTimer(h.limits.AttemptTimeout)
	defer attempt.stop()

	for {
		select {
		case <-h.activity:
			stall.reset(h.limits.StallTimeout)
		case <-stall.c():
			h.expire(core.FailureStalled)
			return
		case <-attempt.c():
			h.expire(core.FailureTimeout)
			return
		case <-ctx.Done():
			// A cancelled context is the daemon shutting down: discard the run.
			h.expire(core.FailureKilled)
			return
		case <-h.exited:
			return
		case <-h.terminated:
			return
		}
	}
}

// expire is the liveness paths' claim. The ladder's answer is discarded on
// purpose, and the reason is the one in core.Termination: it is a probe, not a
// memory.
//
// A liveness window has no caller to answer, so the only thing it could do with
// the boolean is *remember* it — and the fact anyone acts on is whether the
// group is gone now, not whether it was gone when the watchdog fired. The
// orchestrator asks for that itself, with Stop, before it lets anything touch
// the workspace: the terminal event this expiry produces is held until that
// probe confirms, and an unconfirmed one retains the claim (SPEC §7.5, §9.8;
// orchestrator confirmQuiet, Record.groupGone). A remembered unconfirmed would
// be the worse answer twice over — staler than the probe, and sticky, so a claim
// would be retained forever over a group that has since died.
func (h *handle) expire(r core.FailureReason) {
	h.claim(context.Background(), r, h.timings.StopGrace)
}

// claim records the run's outcome and walks the signal ladder for its process
// group, reporting whether the group is gone.
//
// The two halves are one function because they are one promise: claiming a
// verdict is what parks publication in awaitCleanup, and only a ladder that runs
// to a conclusion releases it. There are two callers — a liveness window and
// Stop — and neither can reach the first half without the second, so a claim
// that nothing ever cleans up cannot be made.
//
// It deliberately does not touch the event channel. Killing the process ends the
// stream, and the terminal event is published from there with the verdict
// applied (lifecycleState.resolve), so there is exactly one publication path
// however a run ends.
func (h *handle) claim(ctx context.Context, r core.FailureReason, grace time.Duration) bool {
	h.life.on(input{kind: inVerdict, reason: r})
	return h.signalLadder(ctx, grace)
}

// awaitCleanup blocks until the signal ladder that a claimed verdict promised
// has finished.
//
// Nothing about the run may be published before then. A liveness failure means
// the runner has decided the attempt is over and is killing its process group;
// announcing that while a descendant is still running would hand the
// orchestrator a workspace it believes is idle and is not (SPEC §9.8). The wait
// is bounded by the ladder itself — two graces at most — and short-circuits when
// the consumer has already been abandoned.
//
// The ladder that *gives up* releases this wait too, so the paragraph above is a
// statement about the ordinary case and not a guarantee about the group. It
// cannot be one: a run that refused to publish would never close Done either, so
// the orchestrator would be left with no outcome and nothing to ask about. What
// gates workspace reuse is not this event but the confirmed/unconfirmed answer
// the orchestrator asks Stop for once the run has ended, and an unconfirmed one
// retains the claim (SPEC §7.5, §9.8). So publication is ordered behind cleanup
// wherever cleanup can conclude, and honest about the group wherever it cannot.
//
// Both ways out record themselves in the lifecycle *before* releasing this wait
// (see signalLadder and abortEmit), which is what makes the resolve that follows
// settle rather than ask to wait again.
func (h *handle) awaitCleanup() {
	if !h.life.claimed() {
		// The harness ended on its own terms; no ladder was promised, and
		// whatever it may have left behind is the workspace's problem to
		// dispose, not a live process the runner is racing.
		return
	}
	select {
	case <-h.cleaned:
	case <-h.abort:
	}
}

// pump owns the event channel: it translates lines, synthesizes heartbeats, and
// emits exactly one terminal event (SPEC §7.2) — while deciding none of that
// itself. Every branch below hands an input to the lifecycle and carries out the
// step it returns.
//
// After the terminal event it keeps consuming the stream, discarding what it
// reads. A harness that writes anything after its result line would otherwise
// strand the reader mid-send on an unbuffered channel, and that goroutine owns
// the transcript — so the forensic record would never be closed.
func (h *handle) pump() {
	// Publication waits for the marker verdict (see recorded). An abort releases
	// it too: nobody will read this run's events again, so there is no ordering
	// left to protect and holding here would strand the drain below.
	select {
	case <-h.recorded:
	case <-h.abort:
	}
	lines := h.dispatch()
	close(h.events)
	if lines != nil {
		for range lines {
		}
	}
}

// dispatch feeds the lifecycle until the run has nothing left to deliver, and
// returns its view of the line channel so the caller can finish draining it. A
// nil return means the stream is already closed.
func (h *handle) dispatch() <-chan []byte {
	// Local copies: the pump stops selecting on a channel by nilling its own
	// view of it, never the shared field.
	lines, exited := h.lines, h.exited

	for {
		select {
		case line, ok := <-lines:
			if !ok {
				lines = nil
				if h.feed(input{kind: inStreamEnded}) {
					return lines
				}
				continue
			}
			for _, ev := range h.translate(line) {
				if h.feed(input{kind: inEvent, event: ev}) {
					return lines
				}
			}

		case <-exited:
			exited = nil
			// Whether this ends the run depends on the stream, which is the
			// lifecycle's call. If it has not ended yet the wait is bounded, but
			// not here: boundStream closes the read ends when the window
			// elapses, which ends the stream and decides the run.
			if h.feed(input{kind: inProcessExited}) {
				return lines
			}

		case <-h.abort:
			// abortEmit recorded this before releasing us, and there is nothing
			// left to deliver either way.
			return lines
		}
	}
}

// translate maps one raw line to the events it means. A line with no normalized
// meaning is still activity (SPEC §7.2), so it becomes a heartbeat rather than
// nothing: an unrecognized or future line kind must not make a healthy run look
// dead.
func (h *handle) translate(line []byte) []core.Event {
	if evs := h.launch.Translate(line); len(evs) > 0 {
		return evs
	}
	return []core.Event{{Type: core.EventHeartbeat}}
}

// feed hands one input to the lifecycle and carries out the step it returns. It
// reports whether the pump is finished delivering events — because the terminal
// one has been published, or because the consumer is gone.
//
// The loop runs at most twice. stepAwaitCleanup is returned only for a verdict
// whose cleanup has not finished with a consumer still reading, and awaitCleanup
// returns only once one of those two facts has changed, so the resolve that
// follows settles (see lifecycleState.resolve).
func (h *handle) feed(in input) bool {
	for {
		switch st := h.life.on(in); st.kind {
		case stepIdle:
			return false
		case stepEmit:
			return !h.emit(st.event)
		case stepTerminal:
			h.emit(st.event)
			close(h.terminated)
			return true
		case stepAwaitCleanup:
			h.awaitCleanup()
			in = input{kind: inResolve}
		}
	}
}

// emit delivers one event, stamping its time here so every event carries one
// regardless of which path produced it. It reports false when the consumer is
// not coming back.
//
// Text is redacted here, and only Text (SPEC §10.3, #61). The orchestrator
// retains a bounded tail of it to put in the next attempt's prompt, so a
// credential the agent echoed would otherwise land on the run record, in the
// state dir, and in front of the next agent — three places the transcript
// writer's own redaction does not reach.
//
// This is the single funnel every event passes through, so no path can be added
// that forgets. And it is deliberately *after* translation: the raw line reaches
// Translate unmodified, and the run's verdict is read from Type, Reason and
// Usage, never from Text. Redaction still cannot change how a run is judged —
// what changed is that one field is now retained rather than counted and
// discarded.
func (h *handle) emit(ev core.Event) bool {
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	ev.Text = h.redact.apply(ev.Text)
	select {
	case h.events <- ev:
		return true
	case <-h.abort:
		return false
	}
}

// Probe observes the process group and reports whether it is gone, without
// touching it (SPEC §7.5; #79).
//
// One existence check, nothing else: no verdict claimed, no ladder, no abort, no
// lifecycle input. That is the whole point of it existing next to Stop — the
// caller that needs this answer earliest is the orchestrator the instant a run's
// event stream closes, and at that moment the process may still be flushing its
// transcript. Asking with Stop then would SIGTERM a group about to exit on its
// own and truncate §7.2's record; asking with Probe costs one syscall and
// changes nothing.
//
// Only ESRCH is confirmation (see groupAlive). A cancelled context is
// unconfirmed rather than an error, because "we did not get to look" and "it is
// still there" call for the same caution (SPEC §9.8).
func (h *handle) Probe(ctx context.Context) core.Termination {
	if ctx.Err() != nil {
		return core.TerminationUnconfirmed
	}
	if h.groupAlive() {
		return core.TerminationUnconfirmed
	}
	return core.TerminationConfirmed
}

// Stop signals the process group and reports honestly whether it died
// (SPEC §7.5, §9.8). Both modes walk the same ladder — SIGTERM, grace,
// SIGKILL — because that is what the spec fixes; discard is simply less
// patient, since its output is being thrown away.
//
// Only discard aborts the stream, and that is the whole of the difference
// (#79). An abort closes the output pipes (boundStream), so aborting on an
// unconfirmed answer would throw away whatever a surviving process writes from
// then on — truncating §7.2's verbatim record over a process still producing it,
// which is exactly the cost that ruled out the alternatives on that ticket.
// Discard is the mode that *means* nobody will read this run again, so it is the
// one entitled to that.
//
// Nothing is left parked by not aborting, for a different reason at each of the
// two callers:
//
//   - Stopping a live run: its consumer is still draining events, and the run
//     context that would end it is cancelled only once the group is confirmed
//     gone (orchestrator onStopped).
//   - The quiescence probe a finished run owes its workspace: Events is already
//     closed by then, so the pump is past emitting and is draining raw lines
//     (see pump) — there is no send for an abort to release.
//
// The consequence is that Done is *deferred* on an unconfirmed stop, not
// falsified: the pipes stay open, the readers keep writing the transcript, and
// Done closes once the process really does end, with the record complete.
func (h *handle) Stop(ctx context.Context, mode core.StopMode) core.Termination {
	grace := h.timings.StopGrace
	if mode == core.StopDiscard {
		// Nobody will read this run's events again.
		defer h.abortEmit()
		// A floor, not a window of its own: a discard still has to give the
		// group a real chance to die between the two signals, however short the
		// configured grace is.
		grace = max(grace/10, 100*time.Millisecond)
	}

	if h.claim(ctx, core.FailureKilled, grace) {
		return core.TerminationConfirmed
	}
	// Possibly still alive: the claim must be retained rather than handing a
	// live process's workspace to a replacement (SPEC §9.8).
	return core.TerminationUnconfirmed
}

// signalLadder is SPEC §7.5's SIGTERM → grace → SIGKILL, driven by the *group's*
// disappearance rather than the leader's: a harness that spawned tools of its own
// leaves them in the group, and stopping at the leader would leave them running
// in the workspace. It reports whether the group is gone.
//
// Completing the ladder is what releases anything waiting in awaitCleanup, so
// every exit path must close cleaned — including the one that gives up. The
// lifecycle is told before the channel closes, so a waiter released by it always
// sees the finished cleanup.
func (h *handle) signalLadder(ctx context.Context, grace time.Duration) bool {
	defer h.cleanedOnce.Do(func() {
		h.life.on(input{kind: inCleanupFinished})
		close(h.cleaned)
	})

	if !h.groupAlive() {
		return true
	}
	_ = h.launch.Signal(h.pgid, syscall.SIGTERM)
	if h.awaitGroupGone(ctx, grace) {
		return true
	}
	_ = h.launch.Signal(h.pgid, syscall.SIGKILL)
	return h.awaitGroupGone(ctx, grace)
}

// groupAlive probes for any surviving group member. Signal 0 delivers nothing;
// an error (ESRCH) is the confirmation that the group is gone. A leader that
// has exited but not been reaped still answers, which is why this is polled
// alongside Wait rather than checked once.
//
// Group and member probes are not interchangeable: a just-killed member keeps
// answering a signal-0 probe on its own pid for a few milliseconds after the
// group as a whole has stopped answering. The group is the question that matters
// here — "is anything left that could still touch the workspace" — and erring
// toward alive only costs an unconfirmed verdict, which is the safe direction
// (SPEC §9.8).
// Only ESRCH proves disappearance. Every other error — EPERM above all, which
// says the group exists and we may not signal it — means something is still
// there, and reading it as "gone" would report a confirmed termination over a
// live process. Unknown errors fail closed the same way: an unconfirmed stop
// costs a retained claim, a wrong confirmation costs a shared workspace
// (SPEC §9.8).
func (h *handle) groupAlive() bool {
	err := h.launch.Signal(h.pgid, syscall.Signal(0))
	if err == nil {
		return true
	}
	return !errors.Is(err, syscall.ESRCH)
}

func (h *handle) awaitGroupGone(ctx context.Context, d time.Duration) bool {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(h.timings.GroupPoll)
	defer tick.Stop()
	for {
		if !h.groupAlive() {
			return true
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			return !h.groupAlive()
		case <-ctx.Done():
			return !h.groupAlive()
		}
	}
}

// reap closes Done once the process has been reaped, any promised cleanup has
// finished, and the transcript is on disk; and it refuses to let a process
// linger past a terminal event.
//
// Its kill claims no verdict: the outcome has been published, and a verdict now
// could not change it (lifecycleState.resolve). What it owes is the ladder, and
// that is the same obligation either way.
func (h *handle) reap() {
	defer close(h.done)
	defer h.awaitFlush()
	defer h.awaitCleanup()
	select {
	case <-h.exited:
		return
	case <-h.terminated:
	}
	select {
	case <-h.exited:
	case <-time.After(h.timings.StopGrace):
		h.signalLadder(context.Background(), h.timings.StopGrace)
		<-h.exited
	}
}

// awaitFlush waits for the transcript to be written and closed. There is no
// bound and no fallback: a Done that gave up after a timeout would report a
// complete forensic record when it has one *usually*, which is the race it
// exists to remove rather than a smaller version of it.
//
// It terminates because the readers always do. Their only blocking waits are on
// the pipes and on the hand-off to the pump, and boundStream closes the pipes —
// which releases both — within one post-exit window of the process ending, or
// immediately once the run is abandoned.
func (h *handle) awaitFlush() { <-h.flushed }

// closePipes releases the read ends this package owns. Closing them is what
// unblocks a reader whose write end is held by a surviving descendant.
func (h *handle) closePipes() {
	// Closing the read ends discards whatever the OS pipe still holds, so
	// stranding first is not an ordering detail: it is what lets the reader
	// drain that data into the transcript before it is thrown away.
	h.strand()
	h.closeOnce.Do(func() {
		h.stdout.Close()
		h.stderr.Close()
	})
}

// strand tells the readers to stop waiting on a pump that is not collecting.
func (h *handle) strand() {
	h.strandedOnce.Do(func() { close(h.stranded) })
}

// abortEmit records that no consumer will read another event, then releases
// everything parked waiting for one. The order is load-bearing: a pump released
// by the closed channel must already see the abandonment, or it would go back to
// waiting for a cleanup nothing is going to report.
func (h *handle) abortEmit() {
	h.abortOnce.Do(func() {
		h.life.on(input{kind: inAbandoned})
		close(h.abort)
	})
}

// timer wraps an optional time.Timer: a non-positive duration disables the
// window (a nil channel never fires), which is how an unset limit behaves.
type timer struct{ t *time.Timer }

func newTimer(d time.Duration) timer {
	if d <= 0 {
		return timer{}
	}
	return timer{t: time.NewTimer(d)}
}

func (x timer) c() <-chan time.Time {
	if x.t == nil {
		return nil
	}
	return x.t.C
}

// reset restarts the window. It relies on Go 1.23+ timer semantics, where
// Reset discards a value already sent on the channel — otherwise a timer that
// fired while the watchdog was busy elsewhere would report a stall on the next
// select despite the activity that just reset it.
func (x timer) reset(d time.Duration) {
	if x.t == nil || d <= 0 {
		return
	}
	x.t.Reset(d)
}

func (x timer) stop() {
	if x.t != nil {
		x.t.Stop()
	}
}
