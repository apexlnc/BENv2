package remote_test

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// What a read failure may and may not conclude (#275).
//
// The three cases here are the same backend seen through three different
// failures, and the difference between them is the whole point: a read that
// could not be completed says nothing about the process, while a backend that
// answered "your stream is over" says everything. The first two hold or resume;
// only the third is a verdict. Getting that boundary wrong in the safe-looking
// direction is what the incident was — a durable `crashed` committed over a
// transport error, an attempt failed, and a second coding run dispatched into a
// workspace whose first run had already exited 0.

// A read path that stays down for many reads resumes from the admitted cursor,
// admits everything the backend held for it, and reports the run's own outcome.
func TestASustainedReadOutageResumesFromTheAdmittedCursor(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	logs := &logRecorder{}
	h, err := rig.runnerWith(t, id, withReconnect(fastReconnect()), withLogger(logs)).
		Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession), remotetest.Text("first half"))
	if got := (<-h.Events()).Type; got != core.EventStarted {
		t.Fatalf("first event = %v, want %v", got, core.EventStarted)
	}
	if got := (<-h.Events()).Type; got != core.EventProgress {
		t.Fatalf("second event = %v, want %v", got, core.EventProgress)
	}
	waitForCursor(t, rig, 2)

	// The API pod is deleted mid-run. The runner never learns about it: it
	// produces the rest of its output and exits while nothing can read the log.
	rig.backend.SetEventsFault(errors.New("connection reset by peer"))
	waitForReconnects(t, rig, 3)
	rig.backend.Emit(rig.run, remotetest.Usage(10, 20, 0.25), remotetest.Success())
	rig.backend.Quiet(rig.run)

	select {
	case ev, ok := <-h.Events():
		if !ok {
			t.Fatal("the event stream was sealed while the read path was down")
		}
		t.Fatalf("an unreadable stream published %v/%v", ev.Type, ev.Reason)
	default:
	}

	// The pod comes back. The cursor is still valid and the events are still
	// retained, so the same attempt reads the rest of its own stream.
	rig.backend.SetEventsFault(nil)

	if got := types(collect(t, h)); !sameTypes(got, core.EventUsage, core.EventSucceeded) {
		t.Fatalf("events after the outage = %v, want the run's remaining output", got)
	}

	entries := rig.consumer.Entries()
	if got := envelopeSeqs(entries); !sameSeqs(got, 1, 2, 3, 4) {
		t.Errorf("durable envelopes = %v, want 1..4 — a reconnect skips nothing", got)
	}
	if gaps := gapRecords(entries); len(gaps) != 0 {
		t.Errorf("durable gap records = %v, want none", gaps)
	}
	if got := terminalOutcomes(entries); len(got) != 1 || got[0].Type != core.EventSucceeded {
		t.Errorf("durable outcomes = %v, want exactly the run's success", got)
	}
	for _, item := range entries {
		if strings.HasPrefix(item.ID, "stream-refused:") || strings.HasPrefix(item.ID, "stream-error:") {
			t.Errorf("a read failure committed %q — a durable verdict no later evidence can revisit", item.ID)
		}
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Cursor != 4 {
		t.Errorf("durable cursor = %d, want 4", rec.Cursor)
	}
	if got := rig.backend.StartCalls(); got != 1 {
		t.Errorf("backend saw %d dispatches, want 1 — an outage is not a reason to run the work again", got)
	}
	if logs.total() == 0 {
		t.Error("the outage was never logged; nothing else in the state directory records it")
	}
}

// A read path that stays down past the reconnect budget *holds* the attempt: no
// verdict, nothing durable, nothing closed — and it goes on holding, so the
// attempt is still the one that finishes when the backend returns.
//
// Held rather than failed because there is no evidence to fail on, and because
// the orchestrator already knows how to hold: it retains the claim until the
// run's own termination can be observed (SPEC §9.8). The log is the only thing
// a held attempt emits, which is why it is asserted here as strictly as the
// silence around it.
func TestAReadOutagePastTheBudgetHoldsTheAttempt(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	logs := &logRecorder{}
	h, err := rig.runnerWith(t, id, withReconnect(fastReconnect()), withLogger(logs)).
		Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession))
	if got := (<-h.Events()).Type; got != core.EventStarted {
		t.Fatalf("first event = %v, want %v", got, core.EventStarted)
	}
	waitForCursor(t, rig, 1)

	// Well past the budget by construction: fastReconnect caps one wait at 2ms
	// and calls 10ms of accumulated waiting an outage.
	rig.backend.SetEventsFault(errors.New("no route to host"))
	waitForReconnects(t, rig, 20)
	// Once per read, not once per outage: twenty lines can only come from the
	// former, and an operator watching a held claim has nothing else to look at
	// — a line written only when the outage began scrolls away while the claim
	// stays held.
	waitForLogs(t, logs, 20)
	waitForLevel(t, logs, slog.LevelError)

	select {
	case ev, ok := <-h.Events():
		if !ok {
			t.Fatal("the attempt sealed its stream over a backend it could not read")
		}
		t.Fatalf("the attempt published %v/%v over an unreadable stream", ev.Type, ev.Reason)
	default:
	}
	select {
	case <-h.Done():
		t.Error("Done closed without process-reaped evidence")
	default:
	}
	for _, item := range rig.consumer.Entries() {
		if item.Checkpoint.Terminal {
			t.Errorf("consumption %q is terminal; an unreadable stream concluded nothing", item.ID)
		}
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Cursor != 1 {
		t.Errorf("durable cursor = %d, want 1 — a held attempt advances over nothing", rec.Cursor)
	}
	if got := h.(*remote.Attempt).CommitErr(); got != nil {
		t.Errorf("CommitErr = %v, want nil — a read that failed is not a commit that failed", got)
	}
	if got := h.Probe(context.Background()); got != core.TerminationUnconfirmed {
		t.Errorf("Probe = %v, want %v — the run may still be executing", got, core.TerminationUnconfirmed)
	}

	warn, errored := logs.counts()
	if warn == 0 {
		t.Error("no reconnect was logged below the budget")
	}
	if errored == 0 {
		t.Error("the budget was spent without the log escalating")
	}
	if faults := rig.backend.EventsFaults(); warn+errored > faults {
		t.Errorf("%d log lines for %d refused reads — a read is logged at most once", warn+errored, faults)
	}
	if got := logs.attr("error"); !strings.Contains(got, "no route to host") {
		t.Errorf("logged error = %q, want the backend's own message", got)
	}

	// Still holding, which is what makes holding worth anything: the backend
	// returns and this attempt — not a replacement — reads its own outcome.
	rig.backend.SetEventsFault(nil)
	rig.backend.Emit(rig.run, remotetest.Success())
	rig.backend.Quiet(rig.run)
	if got := types(collect(t, h)); !sameTypes(got, core.EventSucceeded) {
		t.Fatalf("events after the backend returned = %v, want succeeded", got)
	}
	if got := rig.backend.StartCalls(); got != 1 {
		t.Errorf("backend saw %d dispatches, want 1 — a held attempt is not a re-dispatched one", got)
	}
}

// The negative control, and the case the reconnect must not swallow: a backend
// that answered — sealed, no more events — with no provider outcome behind it.
// That is a statement about the stream rather than a failure to read it, so it
// is still the conservative crash it always was (SPEC §7.4).
func TestASealedStreamWithoutATerminalEventIsStillACrash(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runnerWith(t, id, withReconnect(fastReconnect())).
		Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession))
	if got := (<-h.Events()).Type; got != core.EventStarted {
		t.Fatalf("first event = %v, want %v", got, core.EventStarted)
	}
	rig.backend.Quiet(rig.run)

	evs := collect(t, h)
	if len(evs) == 0 {
		t.Fatal("a sealed stream with no provider outcome published nothing")
	}
	last := evs[len(evs)-1]
	if last.Type != core.EventFailed || last.Reason != core.FailureCrashed {
		t.Fatalf("terminal event = %v/%v, want failed/crashed", last.Type, last.Reason)
	}
	if !last.Reason.Retryable() {
		t.Error("a run that ended without saying so must be retryable; the attempt failed, not the work")
	}
	entries := rig.consumer.Entries()
	if got := terminalOutcomes(entries); len(got) != 1 || got[0].Type != core.EventFailed {
		t.Errorf("durable outcomes = %v, want exactly one synthesized failure", got)
	}
	sealed := false
	for _, item := range entries {
		sealed = sealed || strings.HasPrefix(item.ID, "stream-sealed:")
	}
	if !sealed {
		t.Error("no stream-sealed consumption recorded the backend's own statement")
	}
}

func withReconnect(p remote.ReconnectPolicy) func(*remote.Options) {
	return func(o *remote.Options) { o.Reconnect = p }
}

func withLogger(h slog.Handler) func(*remote.Options) {
	return func(o *remote.Options) { o.Logger = slog.New(h) }
}

// fastReconnect is the default policy's shape at a scale a test can wait out.
// Doubling, capped, with a budget — the magnitudes are the only thing scaled.
func fastReconnect() remote.ReconnectPolicy {
	return remote.ReconnectPolicy{
		Initial: time.Millisecond,
		Max:     2 * time.Millisecond,
		Budget:  10 * time.Millisecond,
	}
}

// logRecorder is a held attempt's only observable. It publishes no event,
// commits nothing and closes no channel, so a test that could not read its log
// could not tell "holding" from "hung".
type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

func (r *logRecorder) Enabled(context.Context, slog.Level) bool { return true }

func (r *logRecorder) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, rec.Clone())
	return nil
}

func (r *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return r }
func (r *logRecorder) WithGroup(string) slog.Handler      { return r }

func (r *logRecorder) total() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.records)
}

func (r *logRecorder) counts() (warn, errored int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.records {
		switch {
		case rec.Level >= slog.LevelError:
			errored++
		case rec.Level >= slog.LevelWarn:
			warn++
		}
	}
	return warn, errored
}

func (r *logRecorder) has(level slog.Level) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rec := range r.records {
		if rec.Level >= level {
			return true
		}
	}
	return false
}

// attr is the named attribute of the most recent record, or "".
func (r *logRecorder) attr(key string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.records) == 0 {
		return ""
	}
	var out string
	r.records[len(r.records)-1].Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			out = a.Value.String()
			return false
		}
		return true
	})
	return out
}

// waitForReconnects waits until the backend has refused at least n reads, which
// is how a test knows the pump is retrying rather than merely slow.
func waitForReconnects(t *testing.T, rig *rig, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for rig.backend.EventsFaults() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the pump refused %d reads in 10s, want %d", rig.backend.EventsFaults(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForLogs(t *testing.T, logs *logRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for logs.total() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d log lines in 10s, want %d — the failure must be logged per read", logs.total(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForLevel(t *testing.T, logs *logRecorder, level slog.Level) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !logs.has(level) {
		if time.Now().After(deadline) {
			t.Fatalf("no %v log line in 10s", level)
		}
		time.Sleep(time.Millisecond)
	}
}
