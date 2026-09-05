package airlock

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/claudecode"
	"github.com/srhg-ai-7cef3f93/ben/internal/agent/codexexec"
	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// The whole composition, end to end: a provider adapter's argv and translator,
// #192's remote.Runner, and this package's Airlock backend over the contract
// fake.
//
// The fixtures are the adapters' own recorded runs, read across the package
// boundary rather than copied. A copy would drift, and the point of this test is
// that the *same* bytes produce the *same* normalized events on either
// substrate — which is only a claim about the recorded run if it is the recorded
// run (SPEC §12.2).

type providerCase struct {
	name      string
	fixture   string
	translate remote.Translator
	argv      []string
}

func providerCases() []providerCase {
	return []providerCase{
		{
			name:      "claude-code",
			fixture:   "../agent/claudecode/testdata/stream-success.jsonl",
			translate: claudecode.Translate,
			argv:      []string{"/usr/bin/claude", "--print", "--output-format", "stream-json"},
		},
		{
			name:      "codex-exec",
			fixture:   "../agent/codexexec/testdata/stream-success.jsonl",
			translate: codexexec.Translate,
			argv:      []string{"/usr/bin/codex", "exec", "--json"},
		},
	}
}

// A run over the remote fake produces exactly the events the adapter's own
// translator produces over the same bytes — including across chunk boundaries
// the backend chose, which is the framing guarantee #192 owns.
func TestProviderTranslationIsIdenticalOverTheRemoteFake(t *testing.T) {
	t.Parallel()
	for _, tc := range providerCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatalf("reading the recorded stream: %v", err)
			}
			want := directTranslation(t, tc.translate, raw)

			f := newFixture(t)
			ctx := context.Background()
			id := f.acquire(ctx)
			consumer := remotetest.NewConsumer()
			runner := mustRunner(t, f.sub, id, tc.translate, tc.argv, remotetest.NewMemStore(), consumer)

			handle, err := runner.Start(ctx, core.RunSpec{
				Limits: core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: time.Hour},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			runID := f.srv.RunIDs()[0]
			// Deliberately not one chunk per line: a chunk may split a line and a
			// chunk may carry several, and the contract says chunk boundaries carry
			// no meaning at all.
			for _, chunk := range chunks(raw, 7) {
				f.srv.Emit(runID, "stdout", chunk)
			}
			f.srv.Terminate(runID, airlocktest.Exited(0))

			got := drain(t, handle)
			assertSameEvents(t, got, want)
			if len(consumer.Entries()) == 0 {
				t.Fatal("nothing reached the durable consumer")
			}
		})
	}
}

// The core sees normalized events and nothing else. Raw provider bytes stop at
// the durable consumer, which is where the transcript lives — SPEC §3 invariant
// 6, asserted from the other side: the payloads are present in the consumer's
// retained envelopes and absent from every event the handle published.
func TestProviderPayloadsDoNotReachTheCore(t *testing.T) {
	t.Parallel()
	for _, tc := range providerCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			raw, err := os.ReadFile(tc.fixture)
			if err != nil {
				t.Fatalf("reading the recorded stream: %v", err)
			}
			f := newFixture(t)
			ctx := context.Background()
			id := f.acquire(ctx)
			consumer := remotetest.NewConsumer()
			runner := mustRunner(t, f.sub, id, tc.translate, tc.argv, remotetest.NewMemStore(), consumer)

			handle, err := runner.Start(ctx, core.RunSpec{
				Limits: core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: time.Hour},
			})
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			runID := f.srv.RunIDs()[0]
			f.srv.Emit(runID, "stdout", raw)
			f.srv.Terminate(runID, airlocktest.Exited(0))

			events := drain(t, handle)

			// A JSON envelope key from the provider's own vocabulary. If one of
			// these ever appears in a normalized event's text, an adapter has
			// started forwarding raw records rather than translating them.
			for _, ev := range events {
				for _, marker := range []string{`"type":`, `"session_id"`, `"thread_id"`, `"usage":`} {
					if bytes.Contains([]byte(ev.Text), []byte(marker)) {
						t.Fatalf("a %s event carried a raw provider record: %q", ev.Type, ev.Text)
					}
				}
			}

			var retained bool
			for _, item := range consumer.Entries() {
				if item.Envelope != nil && bytes.Contains(item.Envelope.Payload, []byte(`"type"`)) {
					retained = true
				}
			}
			if !retained {
				t.Fatal("the durable consumer retained no raw provider bytes")
			}
		})
	}
}

// The restart case, end to end. A daemon that stops mid-stream and comes back
// re-projects what it durably accepted and then resumes the backend stream
// after its cursor: no event is skipped and no outcome is invented.
func TestARestartResumesAfterTheDurableCursor(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../agent/claudecode/testdata/stream-success.jsonl")
	if err != nil {
		t.Fatalf("reading the recorded stream: %v", err)
	}
	want := directTranslation(t, claudecode.Translate, raw)
	lines := bytes.SplitAfter(raw, []byte("\n"))

	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	journals := remotetest.NewMemStore()
	consumer := remotetest.NewConsumer()
	argv := []string{"/usr/bin/claude", "--print"}

	first := mustRunner(t, f.sub, id, claudecode.Translate, argv, journals, consumer)
	firstCtx, stop := context.WithCancel(ctx)
	handle, err := first.Start(firstCtx, core.RunSpec{
		Limits: core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: time.Hour},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runID := f.srv.RunIDs()[0]

	// The first process consumes the opening lines and then dies.
	f.srv.Emit(runID, "stdout", bytes.Join(lines[:2], nil))
	// Wait for the cursor to be durable rather than for the event to be
	// delivered: the commit is what a restart resumes from, and a test that
	// raced it would be asserting about the live channel instead.
	waitForCursor(t, journals, 2)
	stop()
	<-handle.Events()

	// The replacement process attaches to the same durable identity. Nothing it
	// knows came from the previous process's memory.
	second := mustRunner(t, f.sub, id, claudecode.Translate, argv, journals, consumer)
	resumed, err := second.Start(ctx, core.RunSpec{
		Limits: core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: time.Hour},
	})
	if err != nil {
		t.Fatalf("Start after restart: %v", err)
	}
	f.srv.Emit(runID, "stdout", bytes.Join(lines[2:], nil))
	f.srv.Terminate(runID, airlocktest.Exited(0))

	got := drain(t, resumed)
	if got[len(got)-1].Type != core.EventSucceeded {
		t.Fatalf("the resumed stream ended with %s, want succeeded", got[len(got)-1].Type)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Fatalf("the restart dispatched %d runs, want 1: %v", len(got), got)
	}
	// Every normalized event survives the restart. At-least-once: the first
	// process's events are re-projected by Recover, so the resumed stream is a
	// superset of the direct translation and never a subset.
	assertContainsInOrder(t, got, want)
}

// initLine is one well-formed claude-code announcement, for the cases below
// that need the stream to say something before the interesting thing happens.
//
// Its session id is the recorded fixture's, and spelling it out is the point:
// the adapter validates the shape it mints (#233, claudecode.validSessionID), so
// an id shortened for brevity would translate to no started event and these
// cases would stage a quieter stream than they read as staging.
const initLine = `{"type":"system","subtype":"init","session_id":"11111111-2222-3333-4444-555555555555"}` + "\n"

// resultLine is the provider's own success verdict, the only thing that may
// become a succeeded event (SPEC §7.4). Airlock's `exit 0` is not it, and the
// case below is careful to make the run produce both.
const resultLine = `{"type":"result","subtype":"success","is_error":false,` +
	`"session_id":"11111111-2222-3333-4444-555555555555"}` + "\n"

// The #275 incident, against the contract fake that can reproduce it: the API
// pod is deleted mid-run, and the run does not notice.
//
// This is the composition the incident actually ran through — provider argv and
// translator, remote.Attempt's pump, this package's client with its own
// per-request retries — so it is the level at which "a transport error is not a
// termination fact" either holds or does not. The partition is total: the read
// path, the status read and the wait are all gone, exactly as they were when the
// pod went away, while the runner goes on to write its result line and exit 0.
// BEN's job is to report *that*, and the previous behaviour was to commit a
// durable `crashed` within milliseconds of the first failed read and let a
// second coding run redo the work.
func TestAPartitionedControlPlaneResumesRatherThanCrashingTheAttempt(t *testing.T) {
	t.Parallel()
	// One client retry, so the pump reaches its own reconnect promptly: the
	// client's retries are the short end of the same problem and this test is
	// about the long end.
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)
	journals := remotetest.NewMemStore()
	consumer := remotetest.NewConsumer()
	reads := &readFailures{}
	runner := mustRunner(t, f.sub, id, claudecode.Translate, []string{"/usr/bin/claude"},
		journals, consumer, func(c *RunnerConfig) {
			c.Reconnect = remote.ReconnectPolicy{
				Initial: time.Millisecond, Max: 5 * time.Millisecond, Budget: time.Minute,
			}
			c.Logger = slog.New(reads)
		})

	handle, err := runner.Start(ctx, core.RunSpec{
		Limits: core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: time.Hour},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runID := f.srv.RunIDs()[0]
	f.srv.Emit(runID, "stdout", []byte(initLine))
	// run.started and the output chunk: the cursor BEN has admitted, and the one
	// the reconnect has to come back to.
	waitForCursor(t, journals, 2)

	f.srv.Partition(true)
	waitFor(t, func() bool { return reads.count() > 0 })
	f.srv.Emit(runID, "stdout", []byte(resultLine))
	f.srv.Terminate(runID, airlocktest.Exited(0))
	f.srv.Partition(false)

	events := drain(t, handle)
	if len(events) == 0 {
		t.Fatal("the attempt published nothing")
	}
	if last := events[len(events)-1]; last.Type != core.EventSucceeded {
		t.Fatalf("the attempt ended with %s/%s, want succeeded — the run exited 0", last.Type, last.Reason)
	}
	if got := handle.Probe(ctx); got != core.TerminationConfirmed {
		t.Fatalf("probe is %s, want confirmed — the run was reaped and its domain quiet", got)
	}
	for _, item := range consumer.Entries() {
		if strings.HasPrefix(item.ID, "stream-refused:") || strings.HasPrefix(item.ID, "stream-error:") {
			t.Errorf("a failed read committed %q, a durable verdict no later evidence can revisit", item.ID)
		}
		if item.Gap != nil {
			t.Errorf("consumption %q recorded a gap %s; nothing was ever unreadable", item.ID, item.Gap)
		}
	}
	rec, err := journals.Load(testClaim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// run.started, the two output chunks, run.terminal — the whole log, read
	// across an outage that interrupted the middle of it.
	if rec.Cursor != 4 {
		t.Errorf("durable cursor = %d, want 4 — the reconnect skipped part of the log", rec.Cursor)
	}
	if got := f.srv.RunIDs(); len(got) != 1 {
		t.Errorf("the outage produced %d runs, want 1: %v", len(got), got)
	}
	if got := f.srv.SignalCount(runID); got != 0 {
		t.Errorf("%d signals were sent over a control plane BEN could not read", got)
	}
}

// A fully received malformed envelope is not a transport outage. Airlock's log
// is append-only, so asking for the same admitted cursor again can only return
// the same unusable event and hold a finished attempt forever. It is the
// unresumable-hole side of #275's boundary and must fail closed once.
func TestAMalformedRetainedEventIsRefusedRatherThanReconnected(t *testing.T) {
	t.Parallel()
	f := newFixture(t, func(o *Options) { o.Timeouts.Retries = 1 })
	ctx := context.Background()
	id := f.acquire(ctx)
	journals := remotetest.NewMemStore()
	consumer := remotetest.NewConsumer()
	reads := &readFailures{}
	runner := mustRunner(t, f.sub, id, claudecode.Translate, []string{"/usr/bin/claude"},
		journals, consumer, func(c *RunnerConfig) {
			c.Reconnect = remote.ReconnectPolicy{
				Initial: time.Millisecond, Max: time.Millisecond, Budget: 2 * time.Millisecond,
			}
			c.Logger = slog.New(reads)
		})

	handle, err := runner.Start(ctx, core.RunSpec{
		Limits: core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: time.Hour},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runID := f.srv.RunIDs()[0]
	// The server assigns a durable sequence before the adapter rejects this
	// contract-invalid stream name. Retrying cannot change that sequence.
	f.srv.Emit(runID, "sideways", []byte(initLine))
	f.srv.Terminate(runID, airlocktest.Exited(0))

	events := drain(t, handle)
	if len(events) != 1 || events[0].Type != core.EventFailed || events[0].Reason != core.FailureCrashed {
		t.Fatalf("events = %v, want one failed/crashed refusal", eventTypes(events))
	}
	if got := handle.(*remote.Attempt).CommitErr(); !errors.Is(got, remote.ErrEventGap) {
		t.Fatalf("CommitErr = %v, want ErrEventGap", got)
	}
	if got := reads.count(); got != 0 {
		t.Errorf("malformed durable envelope was logged as %d reconnects, want none", got)
	}
	entries := consumer.Entries()
	if len(entries) != 1 || !strings.HasPrefix(entries[0].ID, "stream-refused:") || !entries[0].Checkpoint.Terminal {
		t.Fatalf("durable consumptions = %+v, want one terminal stream refusal", entries)
	}
	rec, err := journals.Load(testClaim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Cursor != 0 || !rec.Terminal {
		t.Errorf("checkpoint = cursor %d terminal %v, want cursor 0 terminal", rec.Cursor, rec.Terminal)
	}
	if got := handle.Probe(ctx); got != core.TerminationConfirmed {
		t.Errorf("Probe = %s, want confirmed for the finished run", got)
	}
}

// readFailures is what a held attempt emits, and all of it: no event, no
// consumption, one log line per read it could not complete.
type readFailures struct {
	mu sync.Mutex
	n  int
}

func (h *readFailures) Enabled(context.Context, slog.Level) bool { return true }

func (h *readFailures) Handle(context.Context, slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.n++
	return nil
}

func (h *readFailures) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *readFailures) WithGroup(string) slog.Handler      { return h }

func (h *readFailures) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.n
}

// A backend that ends the stream with no provider outcome is a crash, and a
// crash is *not* a claim about the workspace: the domain-quiet evidence is what
// decides that, independently.
func TestASealedStreamWithoutAProviderOutcomeIsACrash(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	runner := mustRunner(t, f.sub, id, claudecode.Translate, []string{"/usr/bin/claude"},
		remotetest.NewMemStore(), remotetest.NewConsumer())

	handle, err := runner.Start(ctx, core.RunSpec{
		Limits: core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: time.Hour},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runID := f.srv.RunIDs()[0]
	f.srv.Emit(runID, "stdout", []byte(initLine))
	// Reaped and sealed, domain quiet never observed: the agent stopped without
	// reporting, and Airlock cannot say the sandbox is safe to reuse.
	f.srv.Terminate(runID, airlocktest.Terminal{
		State: "exited", Reason: "process_exit", Sealed: "confirmed", Reaped: "confirmed", ExitCode: ptr(1),
	})

	events := drain(t, handle)
	last := events[len(events)-1]
	if last.Type != core.EventFailed || last.Reason != core.FailureCrashed {
		t.Fatalf("the sealed stream produced %s/%s, want failed/crashed", last.Type, last.Reason)
	}
	if got := handle.Probe(ctx); got != core.TerminationUnconfirmed {
		t.Fatalf("probe is %s, want unconfirmed — domain quiet was never observed", got)
	}
}

// A reader that goes away says nothing about the run. It neither signals it nor
// invents a terminal outcome, and the backend keeps the run exactly as it was.
func TestAConsumerDisconnectIsNotARemoteCancel(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	ctx := context.Background()
	id := f.acquire(ctx)
	runner := mustRunner(t, f.sub, id, claudecode.Translate, []string{"/usr/bin/claude"},
		remotetest.NewMemStore(), remotetest.NewConsumer())

	runCtx, disconnect := context.WithCancel(ctx)
	handle, err := runner.Start(runCtx, core.RunSpec{
		Limits: core.RunLimits{StallTimeout: time.Minute, AttemptTimeout: time.Hour},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	runID := f.srv.RunIDs()[0]
	f.srv.Emit(runID, "stdout", []byte(initLine))

	disconnect()
	for range handle.Events() { //nolint:revive // draining until the reader's stream closes
	}

	if got := f.srv.RunState(runID); got == "exited" || got == "failed" || got == "lost" {
		t.Fatalf("the run became %s when its reader disconnected", got)
	}
	if got := f.srv.SignalCount(runID); got != 0 {
		t.Fatalf("%d signals were sent when the reader disconnected", got)
	}
}

// mustRunner builds the runner assembly binds. The mutators are for the seams a
// case has to set itself — the reconnect window above all, whose real magnitudes
// are sized in wall-clock seconds for an API rollout.
func mustRunner(
	t *testing.T, sub *Substrate, id remote.Identity, translate remote.Translator,
	argv []string, journals remote.Store, consumer remote.DurableConsumer,
	mutate ...func(*RunnerConfig),
) *remote.Runner {
	t.Helper()
	cfg := RunnerConfig{
		Journals:  journals,
		Consumer:  consumer,
		Bind:      func(core.RunSpec) (remote.Binding, error) { return remote.Binding{Identity: id, Run: "attempt-1"}, nil },
		Invoke:    func(core.RunSpec) (remote.Invocation, error) { return remote.Invocation{Argv: argv}, nil },
		Translate: translate,
		StopGrace: time.Second,
	}
	for _, fn := range mutate {
		fn(&cfg)
	}
	runner, err := sub.Runner(cfg)
	if err != nil {
		t.Fatalf("Runner: %v", err)
	}
	return runner
}

func directTranslation(t *testing.T, translate remote.Translator, raw []byte) []core.Event {
	t.Helper()
	var out []core.Event
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64<<10), remote.MaxProviderLine)
	for sc.Scan() {
		out = append(out, translate(sc.Bytes())...)
		if len(out) > 0 && isTerminal(out[len(out)-1]) {
			break
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scanning the recorded stream: %v", err)
	}
	return out
}

func chunks(raw []byte, size int) [][]byte {
	var out [][]byte
	for len(raw) > size {
		out = append(out, raw[:size])
		raw = raw[size:]
	}
	if len(raw) > 0 {
		out = append(out, raw)
	}
	return out
}

func drain(t *testing.T, h core.RunHandle) []core.Event {
	t.Helper()
	var got []core.Event
	deadline := time.After(20 * time.Second)
	for {
		select {
		case ev, ok := <-h.Events():
			if !ok {
				return got
			}
			got = append(got, ev)
		case <-deadline:
			t.Fatalf("the event stream did not close; got %d events", len(got))
		}
	}
}

func assertSameEvents(t *testing.T, got, want []core.Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", eventTypes(got), eventTypes(want))
	}
	for i := range want {
		if got[i].Type != want[i].Type || got[i].Text != want[i].Text || got[i].Reason != want[i].Reason {
			t.Fatalf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// assertContainsInOrder allows the at-least-once replay a restart produces: the
// durable consumer re-projects what it accepted, so an event may arrive twice.
// Losing one is what must not happen.
func assertContainsInOrder(t *testing.T, got, want []core.Event) {
	t.Helper()
	i := 0
	for _, ev := range got {
		if i < len(want) && ev.Type == want[i].Type && ev.Text == want[i].Text {
			i++
		}
	}
	if i != len(want) {
		t.Fatalf("the resumed stream %v is missing events from %v", eventTypes(got), eventTypes(want))
	}
}

func eventTypes(events []core.Event) []core.EventType {
	out := make([]core.EventType, len(events))
	for i, ev := range events {
		out[i] = ev.Type
	}
	return out
}

func isTerminal(ev core.Event) bool {
	return ev.Type == core.EventSucceeded || ev.Type == core.EventFailed
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForCursor(t *testing.T, journals *remotetest.MemStore, want int64) {
	t.Helper()
	waitFor(t, func() bool {
		rec, err := journals.Load(testClaim)
		return err == nil && rec.Cursor >= want
	})
}
