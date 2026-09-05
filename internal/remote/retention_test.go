package remote_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
)

// A backend's event log expiring under BEN's cursor, as the one discontinuity
// BEN advances over.
//
// The advance is safe for exactly one reason, and every test here is about that
// reason rather than about draining: the range is written down and the attempt is
// failed in the same durable act. Provider output BEN never read cannot be
// re-read, so an incomplete stream may never become success — but a claim parked
// forever on a retention policy is not a better answer, and it is not one a
// daemon can recover from on its own.
//
// What separates this from remote.ErrEventGap is measurement, not severity. A
// backend that names the cursor and the retention floor has told BEN exactly
// which sequences are gone; a hole with no such statement leaves BEN unable to
// say what it would be skipping, and those stay unrepairable refusals below.

// midStreamExpiry drives the fake to the state the ticket describes: two
// sequences consumed with the second an *unterminated* provider line, sequences
// 3 and 4 expired under that cursor, and sequence 5 — a complete `success` line
// — still retained.
//
// Staged rather than scripted in one call, because a floor set before anything
// was consumed is a different scenario (see the fully expired case): the gap has
// to open under a cursor that has already moved, and over a decoder tail.
func midStreamExpiry(t *testing.T, rig *rig) {
	t.Helper()
	line := remotetest.Text("cut in half by the expiry")
	at := len(line) / 2
	rig.backend.Emit(rig.run, remotetest.Init(testSession))
	waitForCursor(t, rig, 1)
	rig.backend.Emit(rig.run, line[:at])
	waitForCursor(t, rig, 2)
	rig.backend.Append(rig.run, line[at:], remotetest.Text("lost"), remotetest.Success())
	rig.backend.ExpireEvents(rig.run, 5)
	rig.backend.Publish(rig.run, 5)
	rig.backend.Quiet(rig.run)
}

// partialLine is the bytes midStreamExpiry leaves in the decoder tail.
func partialLine() []byte {
	line := remotetest.Text("cut in half by the expiry")
	return line[:len(line)/2]
}

func TestMidStreamRetentionGapIsAcceptedAsOneFailedAttempt(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	midStreamExpiry(t, rig)

	evs := collect(t, h)
	if got := types(evs); !sameTypes(got, core.EventStarted, core.EventFailed) {
		t.Fatalf("events = %v, want the consumed history then one failed outcome", got)
	}
	if got := evs[len(evs)-1].Reason; got != core.FailureCrashed {
		t.Errorf("terminal reason = %q, want %q", got, core.FailureCrashed)
	}
	if got := h.(*remote.Attempt).CommitErr(); got != nil {
		t.Errorf("CommitErr = %v, want nil — a measured expiry is accepted, not refused", got)
	}

	entries := rig.consumer.Entries()
	// One exact range, and it is the range the backend's two numbers describe:
	// requested_after + 1 through oldest_available_seq - 1.
	gaps := gapRecords(entries)
	if len(gaps) != 1 || gaps[0] != (remote.EventGap{From: 3, To: 4}) {
		t.Fatalf("durable gap records = %v, want exactly [3, 4]", gaps)
	}
	if outcomes := terminalOutcomes(entries); len(outcomes) != 1 ||
		outcomes[0].Type != core.EventFailed || outcomes[0].Reason != core.FailureCrashed {
		t.Fatalf("durable terminal outcomes = %v, want exactly one failed/crashed", outcomes)
	}

	// The pre-gap partial line goes with the range. Bytes on the far side of a
	// hole cannot complete a provider record from this side, and gluing them
	// together would fabricate a line neither the provider nor the backend ever
	// emitted.
	if tail := entryByID(t, entries, "event:2").Checkpoint.Tail; !bytes.Equal(tail, partialLine()) {
		t.Fatalf("checkpoint tail before the expiry = %q, want the partial provider line %q", tail, partialLine())
	}
	if tail := entryByID(t, entries, "event-gap:3-4").Checkpoint.Tail; len(tail) != 0 {
		t.Errorf("checkpoint tail at the accepted range = %q, want empty", tail)
	}

	// Draining resumes at the retention floor: what the backend still holds is
	// retained, and the expired sequences are never invented.
	if got := envelopeSeqs(entries); !sameSeqs(got, 1, 2, 5) {
		t.Errorf("retained envelope sequences = %v, want 1, 2 and the retention floor 5", got)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Cursor != 5 {
		t.Errorf("durable cursor = %d, want 5 — the stream kept draining past the accepted range", rec.Cursor)
	}
	if !rec.Terminal {
		t.Error("durable record is not terminal after an accepted retention gap")
	}
	if len(rec.DecoderTail) != 0 {
		t.Errorf("durable decoder tail = %q, want empty", rec.DecoderTail)
	}
}

// A log that expired entirely is the same rule with nothing left over: the range
// runs through the known end, and the cursor advances to it.
//
// The hot-loop is what this is really watching for. A cursor that did not reach
// the floor would be answered `cursor_too_old` again by the very next poll, and
// a daemon that re-accepted the same gap on every answer would spin — durably,
// once per pass, for as long as the run existed.
func TestFullyExpiredEventLogFailsOnceWithoutHotLooping(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Append(rig.run, remotetest.Init(testSession), remotetest.Text("gone"), remotetest.Success())
	rig.backend.ExpireEvents(rig.run, 4) // oldest_available_seq == latest_seq + 1
	rig.backend.Publish(rig.run, 3)
	rig.backend.Quiet(rig.run)

	if got := types(collect(t, h)); !sameTypes(got, core.EventFailed) {
		t.Fatalf("events = %v, want one failed outcome and nothing translated", got)
	}
	entries := rig.consumer.Entries()
	if gaps := gapRecords(entries); len(gaps) != 1 || gaps[0] != (remote.EventGap{From: 1, To: 3}) {
		t.Fatalf("durable gap records = %v, want exactly [1, 3] — the whole known log", gaps)
	}
	if outcomes := terminalOutcomes(entries); len(outcomes) != 1 {
		t.Fatalf("durable terminal outcomes = %v, want exactly one", outcomes)
	}
	// The gap record and the stream seal, and nothing else: no envelope could be
	// retained, and no second acceptance was committed.
	if len(entries) != 2 {
		t.Fatalf("durable consumptions = %d, want 2 (the accepted range and the stream seal): %+v", len(entries), entries)
	}
	if got := envelopeSeqs(entries); len(got) != 0 {
		t.Errorf("retained envelope sequences = %v, want none", got)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if rec.Cursor != 3 {
		t.Errorf("durable cursor = %d, want 3 — the advance goes through the known end", rec.Cursor)
	}
}

// Every boundary a daemon can stop at while accepting a gap, and the same two
// durable facts on the far side of each: one range, one failed outcome.
//
// Three boundaries because the act has three parts that become durable at
// different instants — the consumer's record, the local checkpoint that mirrors
// it, and the drain that follows. A restart in the first window must redo the
// acceptance; a restart in the second must *recognise* it rather than redo it,
// which is the whole reason the range is retained; and a restart in the third
// must not let the retained sequences it re-reads produce a second outcome.
func TestRestartAtEveryRetentionGapBoundaryYieldsOneAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fault refuses one durable commit: a daemon that never reached the
		// consumer's record at all.
		fault func(remote.Consumption) bool
		// crash ends the daemon's context at a commit that did land.
		crash func(remote.Consumption) bool
	}{
		{
			name:  "before the range is durable",
			fault: func(c remote.Consumption) bool { return c.Gap != nil },
		},
		{
			name:  "after the range is durable and before the redial",
			crash: func(c remote.Consumption) bool { return c.Gap != nil },
		},
		{
			name:  "while draining what the backend still holds",
			crash: func(c remote.Consumption) bool { return c.ID == "event:5" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t)
			id := rig.acquire(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			refused := errors.New("durable inbox unavailable")
			if tc.fault != nil {
				rig.consumer.SetFault(func(c remote.Consumption) error {
					if tc.fault(c) {
						return refused
					}
					return nil
				})
			}
			if tc.crash != nil {
				rig.consumer.SetAfterCommit(func(c remote.Consumption) {
					if tc.crash(c) {
						cancel()
					}
				})
			}

			h1, err := rig.runner(t, id).Start(ctx, testSpec())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			midStreamExpiry(t, rig)
			drain(t, h1)

			rig.consumer.SetFault(nil)
			rig.consumer.SetAfterCommit(nil)
			h2, err := rig.runner(t, id).Start(context.Background(), testSpec())
			if err != nil {
				t.Fatalf("restart Start: %v", err)
			}
			if got := types(collect(t, h2)); !sameTypes(got, core.EventStarted, core.EventFailed) {
				t.Fatalf("events after the restart = %v, want the recovered history then one failed outcome", got)
			}

			entries := rig.consumer.Entries()
			if gaps := gapRecords(entries); len(gaps) != 1 || gaps[0] != (remote.EventGap{From: 3, To: 4}) {
				t.Fatalf("durable gap records across the restart = %v, want exactly [3, 4]", gaps)
			}
			if outcomes := terminalOutcomes(entries); len(outcomes) != 1 ||
				outcomes[0].Type != core.EventFailed {
				t.Fatalf("durable terminal outcomes across the restart = %v, want exactly one failed", outcomes)
			}
			rec, err := rig.store.Load(rig.claim)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if rec.Cursor != 5 || !rec.Terminal {
				t.Errorf("durable record after the restart = cursor %d terminal %v, want cursor 5 and terminal", rec.Cursor, rec.Terminal)
			}
		})
	}
}

// Provider success on the far side of an accepted gap cannot overwrite or
// supplement the failure, in this process or in the next one.
//
// This is the criterion the whole design exists for. The retained sequence 5 is
// a perfectly well-formed `success` line, and the only thing standing between it
// and a §9.7 verdict is that BEN has already recorded an outcome for this
// attempt — so it is retained as evidence and never translated. A restart is the
// harder half: the second daemon reads that success line back with no memory of
// the first, and the durable record is what has to stop it.
func TestPostGapSuccessCannotDisplaceTheAcceptedFailure(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h1, err := rig.runner(t, id).Start(ctx, testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	midStreamExpiry(t, rig)
	if got := types(collect(t, h1)); !sameTypes(got, core.EventStarted, core.EventFailed) {
		t.Fatalf("events = %v, want the consumed history then one failed outcome", got)
	}

	// The backend replays the whole retained tail to a second daemon, exactly as
	// a reconnect would.
	rig.backend.ReplayFrom(rig.run, 5)
	h2, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("restart Start: %v", err)
	}
	for _, ev := range collect(t, h2) {
		if ev.Type == core.EventSucceeded {
			t.Fatalf("the retained success line was translated after an accepted retention gap: %+v", ev)
		}
	}
	entries := rig.consumer.Entries()
	if outcomes := terminalOutcomes(entries); len(outcomes) != 1 || outcomes[0].Type != core.EventFailed {
		t.Fatalf("durable terminal outcomes = %v, want exactly one failed", outcomes)
	}
	// Retained, though: the transcript keeps what the run produced after BEN's
	// outcome, which is the difference between a failed attempt and a lost one.
	if got := envelopeSeqs(entries); !sameSeqs(got, 1, 2, 5) {
		t.Errorf("retained envelope sequences = %v, want 1, 2 and 5", got)
	}
}

// A terminal provider event is ground truth (SPEC §7.4). If raw backend events
// expire after BEN has already accepted succeeded, the gap cannot be rewritten
// into a second terminal outcome; it remains an unaccepted ErrEventGap and the
// cursor stays at the last sequence BEN actually consumed.
func TestRetentionGapAfterSucceededOutcomeIsRefusedWithoutAdvance(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Success())
	if got := (<-h.Events()).Type; got != core.EventSucceeded {
		t.Fatalf("first event = %v, want succeeded", got)
	}
	waitForCursor(t, rig, 1)

	rig.backend.Append(rig.run, remotetest.Text("expired"), remotetest.Text("retained"))
	rig.backend.ExpireEvents(rig.run, 3)
	rig.backend.Publish(rig.run, 3)
	rig.backend.Quiet(rig.run)
	if got := types(collect(t, h)); len(got) != 0 {
		t.Fatalf("events after succeeded = %v, want no second terminal outcome", got)
	}
	if got := h.(*remote.Attempt).CommitErr(); !errors.Is(got, remote.ErrEventGap) {
		t.Fatalf("CommitErr = %v, want ErrEventGap", got)
	}
	if gaps := gapRecords(rig.consumer.Entries()); len(gaps) != 0 {
		t.Fatalf("accepted gaps = %v, want none after succeeded", gaps)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1", rec.Cursor)
	}
}

// After a gap has made the outcome failed, retained stdout is transcript only.
// It must not re-enter provider framing: a long unterminated record is invalid
// provider input before an outcome, but it is ordinary opaque evidence after
// translation has stopped.
func TestPostGapTranscriptBypassesProviderFraming(t *testing.T) {
	rig := newRig(t)
	id := rig.acquire(t)
	h, err := rig.runner(t, id).Start(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	rig.backend.Emit(rig.run, remotetest.Init(testSession))
	waitForCursor(t, rig, 1)

	payloads := [][]byte{remotetest.Text("expired")}
	for range 11 {
		payloads = append(payloads, bytes.Repeat([]byte{'x'}, 1<<20))
	}
	rig.backend.Append(rig.run, payloads...)
	rig.backend.ExpireEvents(rig.run, 3)
	rig.backend.Publish(rig.run, len(payloads)+1)
	rig.backend.Quiet(rig.run)
	if got := types(collect(t, h)); !sameTypes(got, core.EventStarted, core.EventFailed) {
		t.Fatalf("events = %v, want started then failed", got)
	}
	if got := h.(*remote.Attempt).CommitErr(); got != nil {
		t.Fatalf("CommitErr = %v, want nil", got)
	}
	rec, err := rig.store.Load(rig.claim)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Cursor != 13 {
		t.Fatalf("cursor = %d, want 13", rec.Cursor)
	}
	if len(rec.DecoderTail) != 0 {
		t.Fatalf("post-terminal decoder tail = %q, want empty", rec.DecoderTail)
	}
	if item := entryByID(t, rig.consumer.Entries(), "event:13"); item.Envelope == nil {
		t.Fatal("the last post-gap envelope was not retained")
	}
}

// An expiry BEN cannot measure is the refusal it always was.
//
// Each of these is a way the backend's answer fails to be evidence about *this*
// request, and the consequence of accepting one is identical in every case: a
// cursor advanced over sequences nobody can name. A range that describes no
// missing sequence is a contradiction; one about another cursor is an answer to
// somebody else's question, which is exactly what a client polling one log with
// another run's cursor would get.
func TestUnmeasurableExpiryStaysAnUnrepairableRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "a floor that describes no missing sequence",
			err:  &remote.RetentionGap{RequestedAfter: 1, OldestAvailable: 2},
		},
		{
			name: "a floor behind the cursor",
			err:  &remote.RetentionGap{RequestedAfter: 1, OldestAvailable: 1},
		},
		{
			name: "an answer about a cursor this stream never asked from",
			err:  &remote.RetentionGap{RequestedAfter: 9, OldestAvailable: 20},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newRig(t)
			id := rig.acquire(t)
			h, err := rig.runner(t, id).Start(context.Background(), testSpec())
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			rig.backend.Emit(rig.run, remotetest.Init(testSession))
			waitForCursor(t, rig, 1)
			rig.backend.Append(rig.run, remotetest.Success())
			// Disconnect is checked before anything else the fake could answer
			// with, so the refusal is what this cursor sees however the two
			// notifications interleave. Quiet only lets the handle finish.
			rig.backend.Disconnect(rig.run, tc.err)
			rig.backend.Quiet(rig.run)

			evs := collect(t, h)
			last := evs[len(evs)-1]
			if last.Type != core.EventFailed || last.Reason != core.FailureCrashed {
				t.Fatalf("terminal event = %v/%v, want failed/crashed", last.Type, last.Reason)
			}
			if got := h.(*remote.Attempt).CommitErr(); !errors.Is(got, remote.ErrEventGap) {
				t.Fatalf("CommitErr = %v, want %v", got, remote.ErrEventGap)
			}
			if gaps := gapRecords(rig.consumer.Entries()); len(gaps) != 0 {
				t.Fatalf("durable gap records = %v, want none — nothing measurable was stated", gaps)
			}
			rec, err := rig.store.Load(rig.claim)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if rec.Cursor != 1 {
				t.Errorf("durable cursor = %d, want 1 — an unmeasurable hole advances nothing", rec.Cursor)
			}
		})
	}
}

// A measured expiry unwraps to both errors, so a reader that has not learned to
// accept one keeps refusing it.
//
// Anchored here rather than only through the pump, because the compatibility
// claim is about every *other* reader of a backend Events error — internal
// reviewrun propagates them and internal reviewctl turns any of them into "no
// verdict, no mutation" — and none of those know the type exists.
func TestRetentionGapUnwrapsToTheGapRefusal(t *testing.T) {
	gap := &remote.RetentionGap{RequestedAfter: 2, OldestAvailable: 5, Cause: errors.New("cursor_too_old")}
	if !errors.Is(gap, remote.ErrRetentionGap) {
		t.Error("a measured expiry does not unwrap to ErrRetentionGap")
	}
	if !errors.Is(gap, remote.ErrEventGap) {
		t.Error("a measured expiry does not unwrap to ErrEventGap; readers that predate it would stop refusing")
	}
	if got, ok := gap.Range(); !ok || got != (remote.EventGap{From: 3, To: 4}) {
		t.Errorf("Range() = (%v, %v), want ([3, 4], true)", got, ok)
	}
	for _, tc := range []struct {
		name string
		gap  *remote.RetentionGap
	}{
		{name: "a floor at the next sequence", gap: &remote.RetentionGap{RequestedAfter: 2, OldestAvailable: 3}},
		{name: "a floor behind the cursor", gap: &remote.RetentionGap{RequestedAfter: 2, OldestAvailable: 1}},
		{name: "a negative cursor", gap: &remote.RetentionGap{RequestedAfter: -1, OldestAvailable: 5}},
		{name: "an overflowing floor", gap: &remote.RetentionGap{RequestedAfter: 0, OldestAvailable: math.MinInt64}},
		{name: "an overflowing cursor", gap: &remote.RetentionGap{RequestedAfter: math.MaxInt64, OldestAvailable: math.MaxInt64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, ok := tc.gap.Range(); ok {
				t.Errorf("Range() = (%v, true), want no range", got)
			}
		})
	}
}

// gapRecords is every accepted retention range in a durable history.
func gapRecords(entries []remote.Consumption) []remote.EventGap {
	var out []remote.EventGap
	for _, item := range entries {
		if item.Gap != nil {
			out = append(out, *item.Gap)
		}
	}
	return out
}

// terminalOutcomes is every durable normalized outcome. Exactly one is the
// invariant an attempt has to hold; live delivery is at-least-once and may
// repeat what is already durable.
func terminalOutcomes(entries []remote.Consumption) []core.Event {
	var out []core.Event
	for _, item := range entries {
		for _, ev := range item.Events {
			if ev.Type == core.EventSucceeded || ev.Type == core.EventFailed {
				out = append(out, ev)
			}
		}
	}
	return out
}

// envelopeSeqs is the backend sequences a durable history retained bytes for, in
// commit order and deduplicated.
func envelopeSeqs(entries []remote.Consumption) []int64 {
	var out []int64
	seen := map[int64]bool{}
	for _, item := range entries {
		if item.Envelope == nil || seen[item.Envelope.Seq] {
			continue
		}
		seen[item.Envelope.Seq] = true
		out = append(out, item.Envelope.Seq)
	}
	return out
}

func sameSeqs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func entryByID(t *testing.T, entries []remote.Consumption, id string) remote.Consumption {
	t.Helper()
	for _, item := range entries {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("durable history has no consumption %q", id)
	return remote.Consumption{}
}
