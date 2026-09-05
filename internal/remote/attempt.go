package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// MaxProviderLine matches the local harness's bounded scanner (SPEC §7.5).
const MaxProviderLine = 10 << 20

// Attempt is one durable remote run exposed through core.RunHandle. Its event
// stream, direct-process Done signal, and domain-quiet Probe are intentionally
// independent.
type Attempt struct {
	backend   ProcessBackend
	journal   *Journal
	consumer  DurableConsumer
	ref       ProcessRef
	translate Translator
	seq       *Sequencer
	stopGrace time.Duration
	reconnect ReconnectPolicy
	log       *slog.Logger
	recovered []Consumption

	events chan core.Event
	done   chan struct{}

	pumpCancel context.CancelFunc
	discarding atomic.Bool
	discarded  chan struct{}

	streamOnce sync.Once
	doneOnce   sync.Once
	stopOnce   sync.Once

	mu        sync.Mutex
	commitErr error
	stops     []core.StopMode
	tail      []byte
}

type AttemptConfig struct {
	Backend   ProcessBackend
	Journal   *Journal
	Consumer  DurableConsumer
	Translate Translator
	StopGrace time.Duration
	// Reconnect bounds the wait between event reads that fail at the transport.
	// The zero value is the default policy.
	Reconnect ReconnectPolicy
	// Logger receives the read failures nothing else records: a held attempt
	// publishes no event and commits nothing, so without this the outage is
	// invisible. Defaults to slog.Default().
	Logger *slog.Logger
}

func Attach(ctx context.Context, cfg AttemptConfig) (*Attempt, error) {
	if cfg.Backend == nil || cfg.Journal == nil || cfg.Consumer == nil || cfg.Translate == nil {
		return nil, errors.New("remote: Attach needs a backend, journal, durable consumer and translator")
	}
	ref, dispatched, err := cfg.Journal.Resume()
	if err != nil {
		return nil, err
	}
	if !dispatched {
		return nil, fmt.Errorf("%w: %s has a reserved process reference but no dispatch to attach to", ErrNoRunID, cfg.Journal.Claim())
	}
	grace := cfg.StopGrace
	if grace <= 0 {
		grace = defaultStopGrace
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	cp := cfg.Journal.Checkpoint()
	a := &Attempt{
		backend: cfg.Backend, journal: cfg.Journal, consumer: cfg.Consumer,
		ref: ref, translate: cfg.Translate, seq: NewSequencer(cp.Cursor),
		stopGrace: grace, reconnect: cfg.Reconnect.withDefaults(), log: logger,
		events: make(chan core.Event, 16), done: make(chan struct{}),
		discarded: make(chan struct{}), tail: append([]byte(nil), cp.Tail...),
	}
	degraded := func(err error) (*Attempt, error) {
		// Once dispatch is durable, an attachment failure cannot be represented
		// by a nil handle: the backend process may be live, and the orchestrator
		// treats nil as evidence that it may clear the run marker. Keep the
		// process controls and liveness probe while exposing the recovery failure
		// through CommitErr and a sealed event stream.
		a.setCommitErr(err)
		a.closeStream()
		go a.watchReap(ctx)
		return a, nil
	}
	recovered, err := cfg.Consumer.Recover(ctx, ref)
	if err != nil {
		return degraded(fmt.Errorf("remote: recovering durable consumptions for %s: %w", ref, err))
	}
	cp, history, gaps, err := reconcileRecovery(cp, recovered)
	if err != nil {
		return degraded(err)
	}
	if len(recovered) > 0 {
		if err := cfg.Journal.CommitCheckpoint(ctx, cp); err != nil {
			return degraded(fmt.Errorf("remote: reconciling the attach checkpoint for %s: %w", ref, err))
		}
	}
	seq := NewSequencer(cp.Cursor)
	if err := seq.Restore(history, gaps); err != nil {
		return degraded(fmt.Errorf("remote: restoring the replay proof for %s: %w", ref, err))
	}
	a.seq = seq
	a.tail = append([]byte(nil), cp.Tail...)
	a.recovered = recovered
	pumpCtx, cancel := context.WithCancel(ctx)
	a.pumpCancel = cancel
	go a.pump(pumpCtx, cp.Terminal)
	go a.watchReap(ctx)
	return a, nil
}

func (a *Attempt) Events() <-chan core.Event { return a.events }
func (a *Attempt) Done() <-chan struct{}     { return a.done }
func (a *Attempt) Ref() ProcessRef           { return a.ref }

func (a *Attempt) Probe(ctx context.Context) core.Termination {
	if ctx.Err() != nil {
		return core.TerminationUnconfirmed
	}
	st, err := a.backend.Status(ctx, a.ref)
	if err != nil {
		return core.TerminationUnconfirmed
	}
	a.observe(st)
	return st.Termination()
}

// Stop preserves core.StopMode all the way to the backend. Interrupt leaves the
// stream attached while the backend performs its TERM/grace/KILL ladder,
// including when that ladder is unconfirmed. Discard stops live delivery.
func (a *Attempt) Stop(ctx context.Context, mode core.StopMode) core.Termination {
	a.mu.Lock()
	a.stops = append(a.stops, mode)
	a.mu.Unlock()

	grace := a.stopGrace
	if mode == core.StopDiscard {
		grace = 0
		a.discarding.Store(true)
		a.stopOnce.Do(func() { close(a.discarded) })
		if a.pumpCancel != nil {
			a.pumpCancel()
		}
	}
	st, err := a.backend.Stop(ctx, a.ref, StopRequest{Mode: mode, Grace: grace})
	if err != nil {
		return core.TerminationUnconfirmed
	}
	a.observe(st)
	return st.Termination()
}

func (a *Attempt) Stops() []core.StopMode {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]core.StopMode(nil), a.stops...)
}

func (a *Attempt) CommitErr() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.commitErr
}

func (a *Attempt) watchReap(ctx context.Context) {
	st, err := a.backend.Wait(ctx, a.ref)
	if err == nil {
		a.observe(st)
	}
}

func (a *Attempt) observe(st Status) {
	if st.Reaped() {
		a.doneOnce.Do(func() { close(a.done) })
	}
}

func (a *Attempt) pump(ctx context.Context, terminal bool) {
	defer a.closeStream()

	// Commit precedes live publication, so a prior daemon may have stopped in
	// that window. Re-project every durable consequence before reading after the
	// recovered cursor. The orchestrator's state transition is the dedupe point;
	// losing an outcome is not an admissible substitute for at-least-once replay.
	durableTerminal := terminal
	terminal = false
	succeeded := false
	for _, item := range a.recovered {
		if !a.publish(ctx, item.Events, &terminal) {
			return
		}
		if terminalType(item.Events) == core.EventSucceeded {
			succeeded = true
		}
	}
	a.recovered = nil
	if durableTerminal && !terminal {
		a.setCommitErr(fmt.Errorf("%w: durable terminal checkpoint has no recoverable terminal event", ErrEventConflict))
		return
	}

	// A failed read is not a fact about the run, so the loop distinguishes the
	// two things a failure can be. A backend that *answered* — about a hole it
	// cannot measure, a durable envelope it cannot decode, or a cursor belonging
	// to another run's log — has given a verdict on BEN's read position that
	// re-reading the same cursor would only repeat, so those fail closed.
	// Everything else, transport failures above all, is BEN failing to reach an
	// untouched run: the cursor is still valid and the events are still retained,
	// so the only correct move is to ask again (#275).
	var outage reconnectState
	for {
		batch, err := a.backend.Events(ctx, a.ref, Cursor(a.seq.Admitted()))
		if err != nil {
			var expiry *RetentionGap
			switch {
			case ctx.Err() != nil:
				return
			case errors.As(err, &expiry):
				if !a.acceptRetentionGap(ctx, expiry, &terminal, succeeded) {
					return
				}
				outage.reset()
				continue
			case errors.Is(err, ErrEventGap), errors.Is(err, ErrProcessMismatch):
				a.setCommitErr(err)
				a.refuseStream(ctx, terminal)
				return
			default:
				if !a.awaitReconnect(ctx, &outage, err) {
					return
				}
				continue
			}
		}
		outage.reset()
		if len(batch) == 0 {
			a.endOnSeal(ctx, terminal)
			if st, err := a.backend.Status(ctx, a.ref); err == nil {
				a.observe(st)
			}
			return
		}
		fresh, err := a.seq.Admit(batch)
		if err != nil {
			a.setCommitErr(err)
			events := []core.Event{{Type: core.EventFailed, Reason: core.FailureCrashed}}
			first, _ := a.accept(ctx, Consumption{
				ID: "sequence-error:" + strconv.FormatInt(a.seq.Admitted()+1, 10),
				Checkpoint: Checkpoint{
					Cursor: a.seq.Cursor(), Tail: append([]byte(nil), a.tail...), Terminal: true,
				},
				Events: events,
			})
			if first {
				_ = a.publish(ctx, events, &terminal)
			}
			return
		}
		for _, env := range fresh {
			nextTail := append([]byte(nil), a.tail...)
			var events []core.Event
			if env.Stream == StreamStdout && !terminal {
				lines, tail, frameErr := frameProvider(nextTail, env.Payload, false)
				if frameErr != nil {
					a.setCommitErr(frameErr)
					events := []core.Event{{Type: core.EventFailed, Reason: core.FailureCrashed}}
					copyEnv := cloneEnvelope(env)
					first, _ := a.accept(ctx, Consumption{
						ID: "event:" + strconv.FormatInt(env.Seq, 10),
						Checkpoint: Checkpoint{
							Cursor: Cursor(env.Seq), Terminal: true,
						},
						Envelope: &copyEnv, Events: events,
					})
					if first {
						_ = a.publish(ctx, events, &terminal)
					}
					return
				}
				nextTail = tail
				if !terminal {
					events = a.translateLines(lines)
				}
			}
			nextTerminal := terminal || hasTerminal(events)
			if nextTerminal {
				// Once an outcome is durable, later stdout is transcript only. A
				// decoder tail can no longer complete a provider record and must not
				// make raw post-terminal bytes subject to the framing ceiling.
				nextTail = nil
			}
			cp := Checkpoint{Cursor: Cursor(env.Seq), Tail: nextTail, Terminal: nextTerminal}
			copyEnv := cloneEnvelope(env)
			first, checkpointed := a.accept(ctx, Consumption{
				ID: "event:" + strconv.FormatInt(env.Seq, 10), Checkpoint: cp,
				Envelope: &copyEnv, Events: events,
			})
			if first {
				_ = a.publish(ctx, events, &terminal)
			}
			if !checkpointed {
				return
			}
			if terminalType(events) == core.EventSucceeded {
				succeeded = true
			}
			a.tail = nextTail
			terminal = nextTerminal
		}
	}
}

func (a *Attempt) endOnSeal(ctx context.Context, terminal bool) {
	var lines [][]byte
	if !terminal {
		var err error
		lines, _, err = frameProvider(a.tail, nil, true)
		if err != nil {
			a.setCommitErr(err)
			lines = nil
		}
	}
	var events []core.Event
	if !terminal {
		events = a.translateLines(lines)
		if !hasTerminal(events) {
			events = append(events, core.Event{Type: core.EventFailed, Reason: core.FailureCrashed})
		}
	}
	cp := Checkpoint{Cursor: a.seq.Cursor(), Terminal: true}
	first, checkpointed := a.accept(ctx, Consumption{
		ID:         "stream-sealed:" + strconv.FormatInt(int64(cp.Cursor), 10),
		Checkpoint: cp,
		Events:     events,
	})
	if checkpointed {
		a.tail = nil
	}
	if first {
		_ = a.publish(ctx, events, &terminal)
	}
}

// acceptRetentionGap converts a backend's measured retention expiry into the one
// discontinuity BEN advances over, and reports whether the stream may keep
// draining.
//
// Three things move in a single durable act, and separating any of them would
// leave a state BEN could not read back: the range, so a later recovery can tell
// an accepted loss from local history that lost records; the cursor, at the last
// expired sequence, so what the backend still holds can be drained; and one
// conservative failed outcome, because provider output BEN never saw cannot be
// re-read and an incomplete stream may never become success. A succeeded
// outcome already made durable is the exception to the advance: SPEC §7.4 makes
// that provider verdict ground truth, so the gap is refused without moving the
// cursor rather than accepted without its required failure. The decoder tail is
// dropped with an accepted gap — a partial provider line before a hole cannot be
// completed by the bytes after it, and completing it with them would fabricate
// a record neither side ever emitted.
//
// The backend's two numbers are re-checked here rather than trusted from the
// adapter that decoded them. An expiry is evidence about one exact cursor, so an
// answer about any other position, or one that describes no missing sequence at
// all, is an unexplained hole again — ErrEventGap, and no advance.
func (a *Attempt) acceptRetentionGap(
	ctx context.Context,
	expiry *RetentionGap,
	terminal *bool,
	succeeded bool,
) bool {
	gap, ok := expiry.Range()
	if !ok || expiry.RequestedAfter != a.seq.Admitted() {
		a.setCommitErr(fmt.Errorf("%w: %s", ErrEventGap, expiry))
		a.refuseStream(ctx, *terminal)
		return false
	}
	if succeeded {
		// SPEC §7.4 makes a terminal provider event ground truth. It cannot be
		// rewritten into a second outcome, so a later raw-stream expiry is not
		// the measured pre-outcome loss this path is allowed to accept.
		a.setCommitErr(fmt.Errorf("%w: retention expired after a succeeded outcome: %v", ErrEventGap, expiry))
		return false
	}
	if err := a.seq.AcceptGap(gap); err != nil {
		a.setCommitErr(err)
		a.refuseStream(ctx, *terminal)
		return false
	}
	var events []core.Event
	if !*terminal {
		events = []core.Event{{Type: core.EventFailed, Reason: core.FailureCrashed}}
	}
	record := gap
	first, checkpointed := a.accept(ctx, Consumption{
		ID:         "event-gap:" + strconv.FormatInt(gap.From, 10) + "-" + strconv.FormatInt(gap.To, 10),
		Checkpoint: Checkpoint{Cursor: Cursor(gap.To), Terminal: true},
		Gap:        &record,
		Events:     events,
	})
	if first {
		_ = a.publish(ctx, events, terminal)
	}
	if !checkpointed {
		return false
	}
	// Terminal regardless of whether the live send landed: what suppresses
	// translation from here on is the durable failure, not its publication.
	*terminal = true
	a.tail = nil
	return true
}

// refuseStream ends the attempt over a backend answer BEN cannot resume from:
// a discontinuity it cannot measure, a durable envelope its adapter cannot
// decode, a cursor the backend says belongs to some other run, or a gap it
// declined to accept. The verdict is conservative because the missing provider
// output cannot be re-read — never because a read failed, which is what
// awaitReconnect is for.
func (a *Attempt) refuseStream(ctx context.Context, terminal bool) {
	if a.discarding.Load() || terminal || ctx.Err() != nil {
		return
	}
	a.commitLocal(ctx, "stream-refused:"+strconv.FormatInt(int64(a.seq.Cursor()), 10),
		[]core.Event{{Type: core.EventFailed, Reason: core.FailureCrashed}}, &terminal)
}

// awaitReconnect waits out a backend BEN could not read from, and reports
// whether the pump may retry.
//
// It never gives up on its own. The wait grows to the policy ceiling and then
// stays there, and once the accumulated wait passes the budget the failure is
// reported as an outage rather than converted into an outcome: a stream that
// stopped being readable says nothing about whether the process is running, so
// the attempt is held with its stream open and its cursor unmoved (SPEC §9.8).
// Only the pump's own context — a daemon shutting down, or Stop(StopDiscard) —
// ends the wait, and neither is a verdict about the run either.
//
// The log line is the whole of what a held attempt emits, so it is written on
// every retry rather than once per outage. Nothing else in the state directory
// records the error, and the #275 incident was invisible for exactly that
// reason.
func (a *Attempt) awaitReconnect(ctx context.Context, outage *reconnectState, err error) bool {
	if a.discarding.Load() || ctx.Err() != nil {
		return false
	}
	outage.attempts++
	delay := a.reconnect.delay(outage.attempts)
	level, msg := slog.LevelWarn, "remote: reading the event stream failed; reconnecting from the admitted cursor"
	if outage.spent >= a.reconnect.Budget {
		level, msg = slog.LevelError, "remote: the event stream has been unreadable past the reconnect budget; holding the attempt"
	}
	a.log.LogAttrs(ctx, level, msg,
		slog.String("process", a.ref.String()),
		slog.String("claim", a.ref.Identity.Claim.String()),
		slog.Int64("cursor", a.seq.Admitted()),
		slog.Int("attempt", outage.attempts),
		slog.Duration("backoff", delay),
		slog.String("error", err.Error()),
	)
	outage.spent += delay

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-a.discarded:
		return false
	case <-ctx.Done():
		return false
	}
}

func (a *Attempt) commitLocal(ctx context.Context, id string, events []core.Event, terminal *bool) {
	cp := Checkpoint{Cursor: a.seq.Cursor(), Tail: append([]byte(nil), a.tail...), Terminal: true}
	first, _ := a.accept(ctx, Consumption{ID: id, Checkpoint: cp, Events: events})
	if first {
		_ = a.publish(ctx, events, terminal)
	}
}

// accept is the only cursor-advance path. The durable consumer acknowledges
// first; the local attach checkpoint follows. A crash between them replays the
// same ID into an idempotent consumer and cannot skip the normalized event.
func (a *Attempt) accept(ctx context.Context, c Consumption) (first, checkpointed bool) {
	durableCtx := context.WithoutCancel(ctx)
	first, err := a.consumer.Commit(durableCtx, a.ref, c)
	if err != nil {
		a.setCommitErr(err)
		return false, false
	}
	if err := a.journal.CommitCheckpoint(durableCtx, c.Checkpoint); err != nil {
		a.setCommitErr(err)
		return first, false
	}
	a.seq.Commit(int64(c.Checkpoint.Cursor))
	return first, true
}

func (a *Attempt) translateLines(lines [][]byte) []core.Event {
	var events []core.Event
	for _, line := range lines {
		translated := a.translate(line)
		events = append(events, translated...)
		if hasTerminal(translated) {
			break
		}
	}
	return events
}

func (a *Attempt) publish(ctx context.Context, events []core.Event, terminal *bool) bool {
	for _, ev := range events {
		if *terminal || !a.send(ctx, ev) {
			return false
		}
		if isTerminal(ev) {
			*terminal = true
			return true
		}
	}
	return true
}

func (a *Attempt) send(ctx context.Context, ev core.Event) bool {
	// A detach already observed before publication wins deterministically over a
	// receiver that happens to become ready at the same instant. Otherwise Go's
	// select may choose the channel and leak one last live notification after the
	// daemon has begun shutting down; the durable consumer will recover it.
	select {
	case <-a.discarded:
		return false
	case <-ctx.Done():
		return false
	default:
	}
	select {
	case a.events <- ev:
		return true
	case <-a.discarded:
		return false
	case <-ctx.Done():
		return false
	}
}

func (a *Attempt) setCommitErr(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.commitErr == nil {
		a.commitErr = err
	}
}

func (a *Attempt) closeStream() { a.streamOnce.Do(func() { close(a.events) }) }

func isTerminal(ev core.Event) bool {
	return ev.Type == core.EventSucceeded || ev.Type == core.EventFailed
}

func hasTerminal(events []core.Event) bool {
	return terminalType(events) != ""
}

func terminalType(events []core.Event) core.EventType {
	for _, ev := range events {
		if isTerminal(ev) {
			return ev.Type
		}
	}
	return ""
}

func cloneEnvelope(env Envelope) Envelope {
	env.Payload = append([]byte(nil), env.Payload...)
	return env
}

// reconcileRecovery treats DurableConsumer as the source of what BEN accepted
// and the attach journal as its resumable cache. The journal can lag the
// consumer by one crash window; it can never lead it. Returning the complete raw
// history — and the retention ranges recorded beside it — lets Sequencer rebuild
// conflict proofs for every committed sequence, including the ones no envelope
// can account for because they expired.
func reconcileRecovery(local Checkpoint, items []Consumption) (Checkpoint, []Envelope, []EventGap, error) {
	if len(items) == 0 {
		if local.Cursor != 0 || len(local.Tail) != 0 || local.Terminal {
			return Checkpoint{}, nil, nil, fmt.Errorf("%w: attach checkpoint %d has no durable consumer history", ErrEventGap, local.Cursor)
		}
		return local, nil, nil, nil
	}

	var latest Checkpoint
	var history []Envelope
	var gaps []EventGap
	var terminalEvent core.EventType
	for i, item := range items {
		cp := item.Checkpoint
		if i > 0 && cp.Cursor < latest.Cursor {
			return Checkpoint{}, nil, nil, fmt.Errorf("%w: durable consumer cursor moved from %d back to %d", ErrEventGap, latest.Cursor, cp.Cursor)
		}
		if latest.Terminal && !cp.Terminal {
			return Checkpoint{}, nil, nil, fmt.Errorf("%w: durable consumer cleared a terminal checkpoint", ErrEventConflict)
		}
		previousTerminal := terminalEvent
		itemTerminal := terminalType(item.Events)
		if itemTerminal != "" {
			if terminalEvent != "" || !cp.Terminal {
				return Checkpoint{}, nil, nil, fmt.Errorf("%w: durable consumer has an inconsistent terminal consequence", ErrEventConflict)
			}
			terminalEvent = itemTerminal
		} else if terminalEvent != "" && len(item.Events) != 0 {
			return Checkpoint{}, nil, nil, fmt.Errorf("%w: durable consumer has normalized events after terminal", ErrEventConflict)
		}
		if item.Gap != nil {
			// A recorded range is only readable as an accepted loss if it says
			// exactly which sequences went missing *here*: it has to start where
			// the previous checkpoint stopped and end at this one. A range that
			// floats free of the cursors around it would let a lost or rewritten
			// record present itself as a retention expiry, which is the
			// substitution this record exists to prevent.
			gap := *item.Gap
			switch {
			case !gap.Valid() || Cursor(gap.From) != latest.Cursor+1 || Cursor(gap.To) != cp.Cursor:
				return Checkpoint{}, nil, nil, fmt.Errorf("%w: accepted retention range %s does not span cursor %d to %d", ErrEventGap, gap, latest.Cursor, cp.Cursor)
			case item.Envelope != nil:
				return Checkpoint{}, nil, nil, fmt.Errorf("%w: accepted retention range %s also carries envelope %d", ErrEventConflict, gap, item.Envelope.Seq)
			case len(cp.Tail) != 0:
				return Checkpoint{}, nil, nil, fmt.Errorf("%w: accepted retention range %s retained a partial provider line", ErrEventConflict, gap)
			case previousTerminal == core.EventSucceeded:
				return Checkpoint{}, nil, nil, fmt.Errorf("%w: accepted retention range %s follows a succeeded outcome", ErrEventConflict, gap)
			case previousTerminal == "" &&
				(len(item.Events) != 1 || item.Events[0].Type != core.EventFailed ||
					item.Events[0].Reason != core.FailureCrashed || !cp.Terminal):
				return Checkpoint{}, nil, nil, fmt.Errorf("%w: accepted retention range %s has no failed(crashed) outcome", ErrEventConflict, gap)
			case previousTerminal == core.EventFailed && len(item.Events) != 0:
				return Checkpoint{}, nil, nil, fmt.Errorf("%w: accepted retention range %s adds an outcome after failure", ErrEventConflict, gap)
			}
			gaps = append(gaps, gap)
		}
		if item.Envelope != nil {
			if item.Envelope.Seq <= 0 || Cursor(item.Envelope.Seq) > cp.Cursor {
				return Checkpoint{}, nil, nil, fmt.Errorf("%w: envelope %d is not covered by checkpoint %d", ErrEventGap, item.Envelope.Seq, cp.Cursor)
			}
			history = append(history, cloneEnvelope(*item.Envelope))
		}
		latest = Checkpoint{Cursor: cp.Cursor, Tail: append([]byte(nil), cp.Tail...), Terminal: cp.Terminal}
	}
	if local.Cursor > latest.Cursor {
		return Checkpoint{}, nil, nil, fmt.Errorf("%w: attach cursor %d leads durable consumer cursor %d", ErrEventGap, local.Cursor, latest.Cursor)
	}
	if local.Terminal && !latest.Terminal {
		return Checkpoint{}, nil, nil, fmt.Errorf("%w: attach journal is terminal but durable consumer is not", ErrEventConflict)
	}
	if latest.Terminal && terminalEvent == "" {
		return Checkpoint{}, nil, nil, fmt.Errorf("%w: terminal checkpoint has no durable terminal event", ErrEventConflict)
	}
	return latest, history, gaps, nil
}

// frameProvider implements bufio.ScanLines semantics over arbitrary chunks,
// including CRLF stripping and a final unterminated line when sealed.
func frameProvider(tail, chunk []byte, sealed bool) ([][]byte, []byte, error) {
	buf := make([]byte, 0, len(tail)+len(chunk))
	buf = append(buf, tail...)
	buf = append(buf, chunk...)
	var lines [][]byte
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		if i > MaxProviderLine {
			return nil, nil, ErrFrameTooLarge
		}
		line := buf[:i]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		lines = append(lines, append([]byte(nil), line...))
		buf = buf[i+1:]
	}
	if len(buf) > MaxProviderLine {
		return nil, nil, ErrFrameTooLarge
	}
	if sealed && len(buf) > 0 {
		if buf[len(buf)-1] == '\r' {
			buf = buf[:len(buf)-1]
		}
		lines = append(lines, append([]byte(nil), buf...))
		buf = nil
	}
	return lines, append([]byte(nil), buf...), nil
}
