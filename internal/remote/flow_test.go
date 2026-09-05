package remote_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// The whole v2 lifecycle in one case, because the acceptance criterion is the
// *sequence*: acquire → start → disconnect → restart/attach → replay/dedupe →
// terminal, with no duplicate dispatch (#192).
//
// Every step of it is a place a plausible implementation goes wrong in a way the
// steps on either side would hide. A cancel that reached the backend would pass a
// test of the start and a test of the terminal. A restart that dispatched again
// would produce a perfectly correct-looking stream. A replay accepted twice would
// double every event in a transcript nobody reads during the run. So they are
// asserted together, in order, against one backend that counts what it was asked
// for.
func TestLifecycleSurvivesADisconnectAndRestart(t *testing.T) {
	rig := newRig(t)

	// --- acquire, twice: idempotent per claim cycle ---
	first := rig.acquire(t)
	second := rig.acquire(t)
	if first != second {
		t.Fatalf("a second Acquire for one claim cycle returned a different workspace:\n %+v\n %+v", first, second)
	}
	if rig.backend.Acquires() != 2 {
		t.Fatalf("Acquires = %d, want 2 — the test did not exercise idempotence", rig.backend.Acquires())
	}
	if !first.Complete() {
		t.Fatalf("acquired identity is incomplete: %+v", first)
	}

	// --- start ---
	ctx, cancel := context.WithCancel(context.Background())
	h1, err := rig.runner(t, first).Start(ctx, testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := rig.backend.StartCalls(); got != 1 {
		t.Fatalf("backend saw %d dispatches, want 1", got)
	}

	rig.backend.Emit(rig.run, remotetest.Init(testSession), remotetest.Text("first half"))
	if got := (<-h1.Events()).Type; got != core.EventStarted {
		t.Fatalf("first event = %v, want %v", got, core.EventStarted)
	}
	if got := (<-h1.Events()).Type; got != core.EventProgress {
		t.Fatalf("second event = %v, want %v", got, core.EventProgress)
	}
	waitForCursor(t, rig, 2)

	// --- disconnect: the daemon goes away, and the run does not ---
	cancel()
	drain(t, h1)
	select {
	case <-h1.Done():
		t.Error("Done closed when only the event consumer disconnected; the remote process is still live")
	default:
	}

	if !rig.backend.Live(rig.run) {
		t.Error("the backend run ended when the consumer's context did; a disconnect is not a remote cancel")
	}
	if n := rig.backend.StopCalls(rig.run); n != 0 {
		t.Errorf("Stop was called %d times by a consumer that merely went away", n)
	}

	// --- restart: a new daemon over the same durable store ---
	restarted := rig.runner(t, first)

	h2, err := restarted.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start recovered the existing dispatch: %v", err)
	}
	if got := rig.backend.StartCalls(); got != 1 {
		t.Fatalf("backend saw %d keyed Start requests after known-id recovery, want 1", got)
	}
	if got := rig.backend.AttachCalls(); got != 1 {
		t.Fatalf("backend saw %d known-id attachments after recovery, want 1", got)
	}
	if got := rig.backend.RunCreations(); got != 1 {
		t.Fatalf("backend created %d runs after exact replay, want 1", got)
	}

	// --- durable recovery plus backend replay/dedupe ---
	//
	// Appended, then rewound, then published in one step, so the pump reads one
	// batch holding both the two events it already consumed and the two it has
	// not. That is the batch a reconnecting backend serves, and the one a dedupe
	// rule has to be exact about.
	rig.backend.Append(rig.run, remotetest.Usage(10, 20, 0.25), remotetest.Success())
	rig.backend.ReplayFrom(rig.run, 1)
	rig.backend.Publish(rig.run, 4)
	rig.backend.Quiet(rig.run)

	evs := collect(t, h2)
	if got := types(evs); !sameTypes(got,
		core.EventStarted, core.EventProgress, core.EventUsage, core.EventSucceeded) {
		t.Errorf("the recovered stream carried %v, want the durable history followed by "+
			"the two events past the cursor", got)
	}

	// --- terminal ---
	if got := rig.backend.StartCalls(); got != 1 {
		t.Errorf("backend saw %d keyed Start requests across the lifecycle, want 1", got)
	}
	if got := rig.backend.RunCreations(); got != 1 {
		t.Errorf("backend created %d runs across the whole lifecycle, want 1", got)
	}
	if got := h2.Probe(context.Background()); got != core.TerminationConfirmed {
		t.Errorf("Probe of the finished run = %v, want %v", got, core.TerminationConfirmed)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("loading the durable record: %v", err)
	}
	if rec.Cursor != 4 {
		t.Errorf("durable cursor = %d, want 4 — every event was consumed", rec.Cursor)
	}
	if rec.BackendRunID == "" {
		t.Error("the durable record carries no backend run id; restart attachment is impossible")
	}
}

// A restart that finds a reserved identity with no dispatch may start, and that
// is only safe because the mark is written *before* the call.
//
// The negative control for the case above. Both worlds — dispatched and not —
// leave a record behind, and a restart that could not tell them apart would
// either dispatch over a live run or strand one nobody will ever start.
func TestARestartWithNoDispatchMayStillStart(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)

	if _, err := remote.Reserve(context.Background(), rig.store, testProcessRef(id, rig.run), remote.Meta{}); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start over a reservation with no dispatch: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession), remotetest.Success())
	rig.backend.Quiet(rig.run)
	if got := types(collect(t, h)); !sameTypes(got, core.EventStarted, core.EventSucceeded) {
		t.Errorf("events = %v, want started then succeeded", got)
	}
	if got := rig.backend.StartCalls(); got != 1 {
		t.Errorf("backend saw %d dispatches, want 1", got)
	}
}

// A dropped connection mid-run is a failed *read*, and nothing else: BEN
// reconnects from the cursor it has admitted and the run is untouched.
//
// The distinction the whole boundary rests on, at its sharpest point. The read
// side is BEN's and the run is the backend's, so a connection that broke is
// evidence about the former only — and Airlock's log is cursor-addressed and
// retained, which makes the read that failed the same read that succeeds when
// the connection comes back. Concluding anything else is durable and can never
// be revisited: #275 was an API pod restart ending an attempt whose run went on
// to exit 0, after which a second coding run redid the work.
func TestADroppedConnectionReconnectsAndLeavesTheRunAlone(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runnerWith(t, id, withReconnect(fastReconnect())).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession))
	if got := (<-h.Events()).Type; got != core.EventStarted {
		t.Fatalf("first event = %v, want %v", got, core.EventStarted)
	}
	rig.backend.Disconnect(rig.run, errors.New("connection reset"))
	if !rig.backend.Live(rig.run) {
		t.Error("the backend run ended on a dropped connection")
	}
	if n := rig.backend.StopCalls(rig.run); n != 0 {
		t.Errorf("Stop was called %d times over a dropped connection", n)
	}

	// The run finishes the way it was always going to.
	rig.backend.Emit(rig.run, remotetest.Success())
	rig.backend.Quiet(rig.run)

	if got := types(collect(t, h)); !sameTypes(got, core.EventSucceeded) {
		t.Fatalf("events after the reconnect = %v, want the run's own outcome", got)
	}
	if got := h.(*remote.Attempt).CommitErr(); got != nil {
		t.Errorf("CommitErr = %v, want nil — nothing about the stream was inconsistent", got)
	}
	if got := h.Probe(context.Background()); got != core.TerminationConfirmed {
		t.Errorf("Probe of the finished run = %v, want %v", got, core.TerminationConfirmed)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Cursor != 2 {
		t.Errorf("durable cursor = %d, want 2 — the reconnect resumed rather than restarted", rec.Cursor)
	}
	if got := terminalOutcomes(rig.consumer.Entries()); len(got) != 1 || got[0].Type != core.EventSucceeded {
		t.Errorf("durable outcomes = %v, want exactly the run's success", got)
	}
}

// waitForCursor waits for the durable record to reach a sequence.
//
// A poll rather than a barrier because the durable consumer and attach journal
// are committed before the live channel projection. The event and the record
// become visible on different goroutines, so polling the durable state is what
// a restart would do anyway.
func waitForCursor(t *testing.T, rig *rig, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := rig.store.Load(rig.claim)
		if err == nil && rec.Cursor >= want {
			if rec.Cursor != want {
				t.Fatalf("durable cursor = %d, want %d", rec.Cursor, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("durable cursor did not reach %d (last read: %+v, %v)", want, rec, err)
		}
		time.Sleep(time.Millisecond)
	}
}

func drain(t *testing.T, h core.RunHandle) {
	t.Helper()
	for range h.Events() {
	}
}
