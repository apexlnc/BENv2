package remote_test

import (
	"errors"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The replay filter, as a table over the three answers it has to keep apart: a
// duplicate is dropped, a gap is refused, and a duplicate that is not actually a
// duplicate is refused too.
//
// The third is the one worth the machinery. Dropping a replay is what makes
// reconnection work at all, and a filter that dropped *anything* whose sequence
// it had seen would also drop a backend rewriting history — which is not a
// replay, and which no amount of deduplication can make safe.
func TestSequencerAdmitsRepliesAndRefusesGaps(t *testing.T) {
	env := func(seq int64, payload string) remote.Envelope {
		return remote.Envelope{Seq: seq, Payload: []byte(payload)}
	}

	for _, tc := range []struct {
		name string
		// batches are fed in order; each is committed in full when it is admitted.
		batches [][]remote.Envelope
		// want is the sequences admitted across every batch, in order.
		want []int64
		err  error
	}{
		{
			name:    "a contiguous stream is admitted whole",
			batches: [][]remote.Envelope{{env(1, "a"), env(2, "b"), env(3, "c")}},
			want:    []int64{1, 2, 3},
		},
		{
			name: "an overlapping reconnect drops what was already consumed",
			batches: [][]remote.Envelope{
				{env(1, "a"), env(2, "b")},
				{env(1, "a"), env(2, "b"), env(3, "c")},
			},
			want: []int64{1, 2, 3},
		},
		{
			name: "a whole batch of replay admits nothing and is not an error",
			batches: [][]remote.Envelope{
				{env(1, "a"), env(2, "b")},
				{env(1, "a"), env(2, "b")},
			},
			want: []int64{1, 2},
		},
		{
			name:    "a gap in the first batch is refused",
			batches: [][]remote.Envelope{{env(1, "a"), env(3, "c")}},
			err:     remote.ErrEventGap,
		},
		{
			name: "a gap across batches is refused",
			batches: [][]remote.Envelope{
				{env(1, "a")},
				{env(3, "c")},
			},
			want: []int64{1},
			err:  remote.ErrEventGap,
		},
		{
			name:    "a sequence that is not positive is refused",
			batches: [][]remote.Envelope{{env(0, "a")}},
			err:     remote.ErrEventGap,
		},
		{
			name: "a replayed sequence carrying different bytes is refused",
			batches: [][]remote.Envelope{
				{env(1, "a"), env(2, "b")},
				{env(2, "rewritten"), env(3, "c")},
			},
			want: []int64{1, 2},
			err:  remote.ErrEventConflict,
		},
		{
			name: "a fresh sequence that was admitted and not committed is still a replay",
			batches: [][]remote.Envelope{
				{env(1, "a"), env(2, "b")},
				// The same two, from a backend that rewound further than the
				// committed cursor. Both were handed over, so both are dropped.
				{env(1, "a"), env(2, "b"), env(3, "c")},
			},
			want: []int64{1, 2, 3},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := remote.NewSequencer(0)
			var got []int64
			var err error
			for _, batch := range tc.batches {
				var fresh []remote.Envelope
				fresh, err = s.Admit(batch)
				if err != nil {
					break
				}
				for _, e := range fresh {
					got = append(got, e.Seq)
					s.Commit(e.Seq)
				}
			}
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("Admit error = %v, want %v", err, tc.err)
				}
			} else if err != nil {
				t.Fatalf("Admit: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("admitted %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("admitted %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// A restart restores the digests retained by the durable consumer as well as
// the cursor. The cursor says which sequences are replays; the digests prove
// they are replays of the same log.
func TestSequencerResumesAtADurableCursor(t *testing.T) {
	s := remote.NewSequencer(2)
	if err := s.Restore([]remote.Envelope{
		{Seq: 1, Payload: []byte("consumed before the crash")},
		{Seq: 2, Payload: []byte("also consumed")},
	}, nil); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if s.Cursor() != 2 {
		t.Fatalf("Cursor = %d, want 2", s.Cursor())
	}
	fresh, err := s.Admit([]remote.Envelope{
		{Seq: 1, Payload: []byte("consumed before the crash")},
		{Seq: 2, Payload: []byte("also consumed")},
		{Seq: 3, Payload: []byte("new")},
	})
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if len(fresh) != 1 || fresh[0].Seq != 3 {
		t.Fatalf("admitted %v, want only sequence 3", fresh)
	}

	restarted := remote.NewSequencer(2)
	if err := restarted.Restore([]remote.Envelope{
		{Seq: 1, Payload: []byte("consumed before the crash")},
		{Seq: 2, Payload: []byte("also consumed")},
	}, nil); err != nil {
		t.Fatalf("Restore for conflict case: %v", err)
	}
	if _, err := restarted.Admit([]remote.Envelope{
		{Seq: 2, Payload: []byte("rewritten after the crash")},
	}); !errors.Is(err, remote.ErrEventConflict) {
		t.Fatalf("changed replay after restart = %v, want %v", err, remote.ErrEventConflict)
	}
}

// An accepted retention range is the only thing that may account for a sequence
// the durable history does not carry, and it has to line up with the envelopes
// on both sides of it.
//
// The refusals are the point. A range floating free of the history would let a
// *lost* record present itself as a retention expiry, which is the substitution
// this whole restore path exists to prevent — so a hole with no range, a range
// that does not begin where the envelopes stop, and a range reaching past the
// durable cursor all fail closed.
func TestSequencerRestoreAccountsForAcceptedRetentionRanges(t *testing.T) {
	env := func(seq int64) remote.Envelope {
		return remote.Envelope{Seq: seq, Payload: []byte("consumed")}
	}

	for _, tc := range []struct {
		name    string
		cursor  remote.Cursor
		history []remote.Envelope
		gaps    []remote.EventGap
		err     error
	}{
		{
			name:    "a mid-stream range bridges the envelopes around it",
			cursor:  6,
			history: []remote.Envelope{env(1), env(2), env(6)},
			gaps:    []remote.EventGap{{From: 3, To: 5}},
		},
		{
			name:   "a range reaching the cursor needs no envelope after it",
			cursor: 5,
			gaps:   []remote.EventGap{{From: 1, To: 5}},
		},
		{
			name:    "two ranges are accepted when each begins where the last left off",
			cursor:  7,
			history: []remote.Envelope{env(1), env(4)},
			gaps:    []remote.EventGap{{From: 2, To: 3}, {From: 5, To: 7}},
		},
		{
			name:    "a hole with no accepted range is refused",
			cursor:  6,
			history: []remote.Envelope{env(1), env(2), env(6)},
			err:     remote.ErrEventGap,
		},
		{
			name:    "a range that does not begin where the envelopes stop is refused",
			cursor:  6,
			history: []remote.Envelope{env(1), env(2), env(6)},
			gaps:    []remote.EventGap{{From: 4, To: 5}},
			err:     remote.ErrEventGap,
		},
		{
			name:    "a range reaching past the durable cursor is refused",
			cursor:  2,
			history: []remote.Envelope{env(1), env(2)},
			gaps:    []remote.EventGap{{From: 3, To: 4}},
			err:     remote.ErrEventGap,
		},
		{
			name:   "an unusable range is refused",
			cursor: 3,
			gaps:   []remote.EventGap{{From: 0, To: 3}},
			err:    remote.ErrEventGap,
		},
		{
			name:    "overlapping ranges are refused",
			cursor:  6,
			history: []remote.Envelope{env(1)},
			gaps:    []remote.EventGap{{From: 2, To: 5}, {From: 4, To: 6}},
			err:     remote.ErrEventGap,
		},
		{
			name:    "an accepted range does not excuse a short history",
			cursor:  8,
			history: []remote.Envelope{env(1), env(5)},
			gaps:    []remote.EventGap{{From: 2, To: 4}},
			err:     remote.ErrEventGap,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := remote.NewSequencer(tc.cursor)
			err := s.Restore(tc.history, tc.gaps)
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("Restore error = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if s.Cursor() != tc.cursor {
				t.Fatalf("Cursor = %d, want %d", s.Cursor(), tc.cursor)
			}
		})
	}
}

// AcceptGap is the one advance over sequences nothing translated, so it accepts
// only a range that starts at the very next one — and a sequence inside a range
// already accepted may never be served again, because BEN has acted on the
// statement that it was gone.
func TestSequencerAcceptGap(t *testing.T) {
	s := remote.NewSequencer(0)
	if _, err := s.Admit([]remote.Envelope{{Seq: 1, Payload: []byte("a")}}); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	s.Commit(1)

	if err := s.AcceptGap(remote.EventGap{From: 3, To: 4}); !errors.Is(err, remote.ErrEventGap) {
		t.Fatalf("AcceptGap([3, 4]) after sequence 1 = %v, want %v", err, remote.ErrEventGap)
	}
	if err := s.AcceptGap(remote.EventGap{From: 2, To: 1}); !errors.Is(err, remote.ErrEventGap) {
		t.Fatalf("AcceptGap of an inverted range = %v, want %v", err, remote.ErrEventGap)
	}
	if got := s.Admitted(); got != 1 {
		t.Fatalf("Admitted after refused ranges = %d, want 1", got)
	}

	if err := s.AcceptGap(remote.EventGap{From: 2, To: 4}); err != nil {
		t.Fatalf("AcceptGap([2, 4]): %v", err)
	}
	if got := s.Admitted(); got != 4 {
		t.Fatalf("Admitted after accepting [2, 4] = %d, want 4", got)
	}
	s.Commit(4)
	if s.Cursor() != 4 {
		t.Fatalf("Cursor after committing an accepted range = %d, want 4", s.Cursor())
	}

	// The filter resumes at the retention floor.
	fresh, err := s.Admit([]remote.Envelope{{Seq: 5, Payload: []byte("after the gap")}})
	if err != nil {
		t.Fatalf("Admit after a gap: %v", err)
	}
	if len(fresh) != 1 || fresh[0].Seq != 5 {
		t.Fatalf("admitted %v, want only sequence 5", fresh)
	}

	// A sequence the backend said it no longer held, served anyway. That is the
	// log disagreeing with itself, not a replay — and it has no digest, so
	// without the recorded range it would be indistinguishable from a lost one.
	if _, err := s.Admit([]remote.Envelope{{Seq: 3, Payload: []byte("resurrected")}}); !errors.Is(err, remote.ErrEventConflict) {
		t.Fatalf("Admit of an expired sequence = %v, want %v", err, remote.ErrEventConflict)
	}
}

// Refusing a batch refuses all of it. In particular, seeing sequence 1 before
// discovering a gap at 3 must not make a retry silently omit sequence 1.
func TestFailedBatchAdmissionDoesNotMutateSequencerState(t *testing.T) {
	s := remote.NewSequencer(0)
	if fresh, err := s.Admit([]remote.Envelope{
		{Seq: 1, Payload: []byte("one")},
		{Seq: 3, Payload: []byte("three")},
	}); !errors.Is(err, remote.ErrEventGap) || len(fresh) != 0 {
		t.Fatalf("Admit([1,3]) = (%v, %v), want no prefix and %v", fresh, err, remote.ErrEventGap)
	}
	if got := s.Admitted(); got != 0 {
		t.Fatalf("Admitted after refused batch = %d, want 0", got)
	}
	fresh, err := s.Admit([]remote.Envelope{
		{Seq: 1, Payload: []byte("one after retry")},
		{Seq: 2, Payload: []byte("two")},
		{Seq: 3, Payload: []byte("three")},
	})
	if err != nil {
		t.Fatalf("retry Admit: %v", err)
	}
	if len(fresh) != 3 || fresh[0].Seq != 1 || fresh[1].Seq != 2 || fresh[2].Seq != 3 {
		t.Fatalf("retry admitted %v, want [1 2 3]", fresh)
	}
}

// The commit cursor moves forward only, and never past what was handed over.
//
// Both directions are a way to lose evidence. Backwards replays events BEN has
// already acted on — a second `succeeded` into a loop that has moved on. Past
// admitted records a position over events nothing translated, which is exactly
// the skip the commit-after rule exists to prevent.
func TestSequencerCommitIsMonotonicAndBoundedByAdmission(t *testing.T) {
	s := remote.NewSequencer(0)
	if _, err := s.Admit([]remote.Envelope{{Seq: 1, Payload: []byte("a")}, {Seq: 2, Payload: []byte("b")}}); err != nil {
		t.Fatalf("Admit: %v", err)
	}
	if s.Admitted() != 2 {
		t.Fatalf("Admitted = %d, want 2", s.Admitted())
	}

	s.Commit(2)
	s.Commit(1)
	if s.Cursor() != 2 {
		t.Errorf("Cursor after committing backwards = %d, want 2", s.Cursor())
	}
	s.Commit(99)
	if s.Cursor() != 2 {
		t.Errorf("Cursor after committing past admission = %d, want 2 — nothing translated sequence 99", s.Cursor())
	}
}
