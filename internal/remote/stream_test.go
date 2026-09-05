package remote_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

func TestArbitraryBackendChunksAreFramedAsProviderLines(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks func() [][]byte
		want   []core.EventType
	}{
		{
			name: "one provider line split across chunks",
			chunks: func() [][]byte {
				line := remotetest.Text("split")
				at := len(line) / 2
				return [][]byte{line[:at], line[at:], remotetest.Success()}
			},
			want: []core.EventType{core.EventProgress, core.EventSucceeded},
		},
		{
			name: "several provider lines in one chunk",
			chunks: func() [][]byte {
				return [][]byte{bytes.Join([][]byte{
					remotetest.Init(testSession), remotetest.Text("joined"), remotetest.Success(),
				}, nil)}
			},
			want: []core.EventType{core.EventStarted, core.EventProgress, core.EventSucceeded},
		},
		{
			name: "a final provider line without newline",
			chunks: func() [][]byte {
				return [][]byte{
					remotetest.Init(testSession),
					bytes.TrimSuffix(remotetest.Success(), []byte{'\n'}),
				}
			},
			want: []core.EventType{core.EventStarted, core.EventSucceeded},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t)
			id := rig.acquire(t)
			h, err := rig.runner(t, id).Start(context.Background(), testSpec())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			rig.backend.Emit(rig.run, tc.chunks()...)
			rig.backend.Quiet(rig.run)
			if got := types(collect(t, h)); !sameTypes(got, tc.want...) {
				t.Fatalf("events = %v, want %v", got, tc.want)
			}
			rec, err := rig.store.Load(rig.claim)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if len(rec.DecoderTail) != 0 {
				t.Errorf("decoder tail after stream seal = %q, want empty", rec.DecoderTail)
			}
		})
	}
}

func TestDecoderTailSurvivesAProcessRestart(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	ctx, cancel := context.WithCancel(context.Background())
	h1, err := rig.runner(t, id).Start(ctx, testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	line := remotetest.Text("across restart")
	at := len(line) / 2
	rig.backend.Emit(rig.run, line[:at])
	waitForCursor(t, rig, 1)
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !bytes.Equal(rec.DecoderTail, line[:at]) {
		t.Fatalf("durable decoder tail = %q, want %q", rec.DecoderTail, line[:at])
	}

	cancel()
	for range h1.Events() {
	}
	select {
	case <-h1.Done():
		t.Fatal("consumer restart reaped the remote process")
	default:
	}

	h2, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	rig.backend.Emit(rig.run, line[at:], remotetest.Success())
	rig.backend.Quiet(rig.run)
	if got := types(collect(t, h2)); !sameTypes(got, core.EventProgress, core.EventSucceeded) {
		t.Fatalf("events after restart = %v, want progress then succeeded", got)
	}
}

func TestCursorWaitsForTheDurableConsumerAcknowledgement(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession))
	if got := (<-h.Events()).Type; got != core.EventStarted {
		t.Fatalf("first event = %v, want started", got)
	}
	waitForCursor(t, rig, 1)

	ackErr := errors.New("durable inbox unavailable")
	rig.consumer.SetFault(func(c remote.Consumption) error {
		if c.ID == "event:2" {
			return ackErr
		}
		return nil
	})
	rig.backend.Emit(rig.run, remotetest.Text("must replay"), remotetest.Success())
	rig.backend.Quiet(rig.run)
	for range h.Events() {
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Cursor != 1 {
		t.Fatalf("cursor advanced to %d without a durable acknowledgement, want 1", rec.Cursor)
	}
	if got := h.(*remote.Attempt).CommitErr(); !errors.Is(got, ackErr) {
		t.Fatalf("CommitErr = %v, want %v", got, ackErr)
	}

	rig.consumer.SetFault(nil)
	h2, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	if got := types(collect(t, h2)); !sameTypes(got, core.EventStarted, core.EventProgress, core.EventSucceeded) {
		t.Fatalf("recovered events = %v, want durable started then progress and succeeded", got)
	}
}

func TestDurableTerminalIsRedeliveredAfterACrashBeforePublish(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	ctx, cancel := context.WithCancel(context.Background())
	rig.consumer.SetAfterCommit(func(c remote.Consumption) {
		if c.Checkpoint.Terminal {
			cancel() // the daemon dies after durable acceptance and before publish
		}
	})
	h, err := rig.runner(t, id).Start(ctx, testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Success())
	rig.backend.Quiet(rig.run)
	var firstProcess []core.Event
	for ev := range h.Events() {
		firstProcess = append(firstProcess, ev)
	}
	if got := types(firstProcess); len(got) != 0 {
		t.Fatalf("events before the simulated crash = %v, want none", got)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !rec.Terminal {
		t.Fatal("terminal outcome was not durable before the simulated crash")
	}

	rig.consumer.SetAfterCommit(nil)
	h2, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	if got := types(collect(t, h2)); !sameTypes(got, core.EventSucceeded) {
		t.Fatalf("recovered events = %v, want the durable succeeded outcome", got)
	}
}

func TestJournalCannotCloseUntilPostTerminalDrainIsCheckpointed(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	rig.consumer.SetAfterCommit(func(c remote.Consumption) {
		if c.ID == "event:2" {
			close(entered)
			<-release
		}
	})
	runner := rig.runner(t, id)
	h, err := runner.Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Both envelopes are one backend batch. The first publishes the terminal
	// outcome; the second is raw post-terminal evidence whose durable commit is
	// held open at the consumer/journal crash boundary.
	rig.backend.Emit(rig.run, remotetest.Success(), remotetest.Private())
	rig.backend.Quiet(rig.run)
	if got := (<-h.Events()).Type; got != core.EventSucceeded {
		t.Fatalf("event = %v, want succeeded", got)
	}
	<-entered
	prematureClose := false
	select {
	case _, ok := <-h.Events():
		if !ok {
			prematureClose = true
		} else {
			t.Error("unexpected normalized event after terminal outcome")
		}
	default:
	}

	journal, err := runner.Journal(rig.claim)
	if err != nil {
		t.Fatalf("Journal: %v", err)
	}
	closed := make(chan error, 1)
	go func() {
		for range h.Events() {
		}
		closed <- journal.Close()
	}()
	close(release)
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if prematureClose {
		t.Error("event stream closed while a post-terminal checkpoint was still in flight")
	}
	if rig.store.Has(rig.claim) {
		t.Fatal("post-terminal checkpoint recreated the journal after Close")
	}
}

func TestDurableTerminalReconcilesAConsumerAheadOfTheJournal(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	ctx, cancel := context.WithCancel(context.Background())
	rig.consumer.SetAfterCommit(func(c remote.Consumption) {
		if c.Checkpoint.Terminal {
			cancel()
		}
	})
	diskFull := errors.New("disk full")
	rig.store.SetSaveFault(func(r remote.Record) error {
		if r.Terminal {
			return diskFull
		}
		return nil
	})
	h, err := rig.runner(t, id).Start(ctx, testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Success())
	rig.backend.Quiet(rig.run)
	if got := types(collect(t, h)); len(got) != 0 {
		t.Fatalf("events before the simulated crash = %v, want none", got)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Terminal {
		t.Fatal("test did not leave the durable consumer ahead of the attach journal")
	}

	rig.consumer.SetAfterCommit(nil)
	rig.store.SetSaveFault(nil)
	h2, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	if got := types(collect(t, h2)); !sameTypes(got, core.EventSucceeded) {
		t.Fatalf("recovered events = %v, want the durable succeeded outcome", got)
	}
	rec, err = rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load after recovery: %v", err)
	}
	if !rec.Terminal {
		t.Fatal("recovery did not reconcile the terminal checkpoint into the attach journal")
	}
}

func TestChangedReplayAfterRestartFailsClosed(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	ctx, cancel := context.WithCancel(context.Background())
	h1, err := rig.runner(t, id).Start(ctx, testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession), remotetest.Text("accepted"))
	if got := (<-h1.Events()).Type; got != core.EventStarted {
		t.Fatalf("first event = %v, want started", got)
	}
	if got := (<-h1.Events()).Type; got != core.EventProgress {
		t.Fatalf("second event = %v, want progress", got)
	}
	waitForCursor(t, rig, 2)
	cancel()
	drain(t, h1)

	rig.backend.Rewrite(rig.run, 1, remotetest.Text("rewritten"))
	rig.backend.ReplayFrom(rig.run, 1)
	rig.backend.Quiet(rig.run)
	h2, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	if got := types(collect(t, h2)); !sameTypes(got, core.EventStarted, core.EventProgress, core.EventFailed) {
		t.Fatalf("events = %v, want recovered history then fail-closed outcome", got)
	}
	if got := h2.(*remote.Attempt).CommitErr(); !errors.Is(got, remote.ErrEventConflict) {
		t.Fatalf("CommitErr = %v, want %v", got, remote.ErrEventConflict)
	}
}

func TestGapRefusesTheWholeBackendBatch(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.EmitEnvelopes(rig.run,
		remote.Envelope{Seq: 1, Stream: remote.StreamStdout, Payload: remotetest.Init(testSession)},
		remote.Envelope{Seq: 3, Stream: remote.StreamStdout, Payload: remotetest.Success()},
	)
	rig.backend.Quiet(rig.run)
	if got := types(collect(t, h)); !sameTypes(got, core.EventFailed) {
		t.Fatalf("events = %v, want only the fail-closed outcome and no admitted prefix", got)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Cursor != 0 {
		t.Fatalf("cursor after refused [1,3] batch = %d, want 0", rec.Cursor)
	}
}
