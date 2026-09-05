package remote

import (
	"bytes"
	"crypto/sha256"
	"fmt"
)

// Sequencer is the replay filter between a backend's event log and BEN's
// translation of it.
//
// A reconnect replays. That is the point of an events-after-cursor API and it is
// not an error: BEN commits a cursor only after it has durably consumed an
// event, so the window between consuming and committing is replayed by design
// (Journal). What the filter has to do is tell the three cases apart, because
// they are three and only one of them is ordinary:
//
//   - a sequence already handed over, same payload — a replay. Dropped.
//   - a sequence already handed over, *different* payload — the backend's log is
//     not the log BEN read. ErrEventConflict; no dedupe rule can repair it, and
//     continuing would translate one history while having acted on another.
//   - a sequence beyond the next expected one — a gap. ErrEventGap, because
//     advancing across missing evidence commits BEN to events it never saw, and
//     the events it never saw are exactly the ones a §9.7 verdict might have
//     turned on.
//
// A backend that states which sequences it no longer retains is the one
// exception, and it is not a fourth case in Admit: AcceptGap moves the filter
// across a measured range only after its caller has written that range and the
// attempt's failure down (RetentionGap).
//
// Two high-water marks rather than one, and the difference between them is the
// crash window. `admitted` is what this process has handed to the translator;
// `committed` is what the durable record says. They coincide in steady state and
// separate for exactly as long as a translation is in flight — which is the
// interval a crash lands in, and the interval a restart replays.
//
// The conflict check remembers a digest per sequence rather than the payload: a
// run's stream is unbounded and its lines can be megabytes (SPEC §7.5's scanner
// bound exists for that), so retaining payloads to compare against would grow
// with the run. A digest is 32 bytes and supports the only comparison this needs.
type Sequencer struct {
	admitted  int64
	committed int64
	digests   map[int64][32]byte
	// gaps are the ranges an explicit, measured retention expiry moved the
	// filter across (AcceptGap). They are kept in ascending order and never
	// overlap, and they are what lets the filter answer "that sequence is gone"
	// rather than "that sequence has no digest" — two situations that would
	// otherwise be indistinguishable from a rewritten log.
	gaps []EventGap
}

// NewSequencer starts a filter at a cursor — zero for a fresh run, the durable
// record's cursor for recovery after a restart. A non-zero cursor is not ready
// to admit replay until Restore has rebuilt its digest history from the durable
// consumer; a cursor without those digests cannot distinguish replay from a
// rewritten backend log.
func NewSequencer(at Cursor) *Sequencer {
	return &Sequencer{
		admitted:  int64(at),
		committed: int64(at),
		digests:   map[int64][32]byte{},
	}
}

// Restore rebuilds the conflict proof for a committed cursor from the raw
// envelopes retained by DurableConsumer, plus the retention ranges that consumer
// recorded as accepted. History is complete and in sequence order: omitting an
// already-consumed sequence would make a later changed replay indistinguishable
// from an ordinary duplicate, so absence fails closed.
//
// A gap is the *only* thing that may account for a sequence the history does not
// carry, and it has to line up exactly — each range beginning where the
// envelopes stop and ending where they resume. That is what turns "these
// sequences were never retained" into a checkable statement instead of a hole a
// lost record could also have produced.
func (s *Sequencer) Restore(history []Envelope, gaps []EventGap) error {
	for i, gap := range gaps {
		switch {
		case !gap.Valid():
			return fmt.Errorf("%w: accepted retention range %s is unusable", ErrEventGap, gap)
		case i > 0 && gap.From <= gaps[i-1].To:
			return fmt.Errorf("%w: accepted retention ranges %s and %s overlap or are out of order", ErrEventGap, gaps[i-1], gap)
		case gap.To > s.committed:
			return fmt.Errorf("%w: accepted retention range %s reaches past cursor %d", ErrEventGap, gap, s.committed)
		}
	}
	restored := make(map[int64][32]byte, len(history))
	expected := int64(1)
	next := 0
	// skipAccepted walks past every range that starts exactly where the
	// envelopes stopped. Consecutive ranges are possible: a second expiry can
	// land while a stream is still being drained past the first.
	skipAccepted := func() {
		for next < len(gaps) && gaps[next].From == expected {
			expected = gaps[next].To + 1
			next++
		}
	}
	skipAccepted()
	for _, env := range history {
		if env.Seq != expected {
			return fmt.Errorf("%w: durable replay history expected sequence %d, got %d", ErrEventGap, expected, env.Seq)
		}
		if env.Seq > s.committed {
			return fmt.Errorf("%w: durable replay history reaches sequence %d past cursor %d", ErrEventGap, env.Seq, s.committed)
		}
		restored[env.Seq] = envelopeDigest(env)
		expected++
		skipAccepted()
	}
	if next != len(gaps) {
		return fmt.Errorf("%w: accepted retention range %s does not begin at sequence %d", ErrEventGap, gaps[next], expected)
	}
	if expected-1 != s.committed {
		return fmt.Errorf("%w: durable cursor is %d but replay history ends at %d", ErrEventGap, s.committed, expected-1)
	}
	s.digests = restored
	s.gaps = append([]EventGap(nil), gaps...)
	return nil
}

// AcceptGap moves the filter across a range the backend has stated it no longer
// retains. It is the one place the filter advances over sequences nothing
// translated, and it is safe only because its caller has durably recorded the
// range and the attempt's conservative failed outcome in the same act
// (Attempt.acceptRetentionGap).
//
// The range must begin at the very next sequence. A range starting anywhere else
// would leave sequences that are neither consumed nor recorded as lost, which is
// the position this whole filter exists to make unreachable.
func (s *Sequencer) AcceptGap(gap EventGap) error {
	if !gap.Valid() {
		return fmt.Errorf("%w: retention range %s is unusable", ErrEventGap, gap)
	}
	if gap.From != s.admitted+1 {
		return fmt.Errorf("%w: retention range %s does not begin at sequence %d", ErrEventGap, gap, s.admitted+1)
	}
	s.gaps = append(s.gaps, gap)
	s.admitted = gap.To
	return nil
}

// expired reports whether a sequence is inside a range already accepted as lost.
func (s *Sequencer) expired(seq int64) bool {
	for _, gap := range s.gaps {
		if gap.Contains(seq) {
			return true
		}
	}
	return false
}

// Cursor is the durably committed position — what a restart would resume from.
func (s *Sequencer) Cursor() Cursor { return Cursor(s.committed) }

// Admitted is the highest sequence handed to the translator by this process.
// It is ahead of Cursor exactly while a translation is uncommitted.
func (s *Sequencer) Admitted() int64 { return s.admitted }

// Admit filters one batch, in backend order, returning the envelopes BEN has not
// consumed yet.
//
// The whole batch is refused on the first gap or conflict rather than the prefix
// being kept. A caller that took the prefix would have to decide what its
// position now means, and the honest answer — "everything up to the break" — is
// what a retry of the same call produces anyway.
func (s *Sequencer) Admit(batch []Envelope) ([]Envelope, error) {
	admitted := s.admitted
	pending := make(map[int64][32]byte, len(batch))
	fresh := make([]Envelope, 0, len(batch))
	for _, env := range batch {
		if env.Seq <= 0 {
			return nil, fmt.Errorf("%w: sequence %d is not positive (a backend log starts at 1)", ErrEventGap, env.Seq)
		}
		switch {
		case env.Seq <= admitted:
			seen, ok := pending[env.Seq]
			if !ok {
				seen, ok = s.digests[env.Seq]
			}
			if !ok {
				if s.expired(env.Seq) {
					// The backend served a sequence it told BEN it no longer
					// held. BEN has already acted on that statement, so this is
					// the log disagreeing with itself, not a replay.
					return nil, fmt.Errorf("%w: sequence %d was accepted as expired and cannot be served again", ErrEventConflict, env.Seq)
				}
				return nil, fmt.Errorf("%w: sequence %d has no durable replay digest", ErrEventConflict, env.Seq)
			}
			got := envelopeDigest(env)
			if !bytes.Equal(seen[:], got[:]) {
				return nil, fmt.Errorf("%w: sequence %d", ErrEventConflict, env.Seq)
			}
			// A replay of something already translated. Dropped.
		case env.Seq == admitted+1:
			pending[env.Seq] = envelopeDigest(env)
			admitted = env.Seq
			fresh = append(fresh, env)
		default:
			return nil, fmt.Errorf("%w: expected sequence %d, got %d", ErrEventGap, admitted+1, env.Seq)
		}
	}
	for seq, digest := range pending {
		s.digests[seq] = digest
	}
	s.admitted = admitted
	return fresh, nil
}

// Commit advances the durable cursor after the caller has persisted everything
// up to seq.
//
// It never moves backwards, and never past what was admitted. Backwards would
// replay events BEN has already acted on; past admitted would record a position
// over events nothing translated, which is the one thing the commit-after rule
// exists to prevent.
func (s *Sequencer) Commit(seq int64) {
	if seq > s.admitted {
		seq = s.admitted
	}
	if seq > s.committed {
		s.committed = seq
	}
}

func envelopeDigest(env Envelope) [32]byte {
	h := sha256.New()
	h.Write([]byte{byte(env.Stream)})
	if env.Truncated {
		h.Write([]byte{1})
	} else {
		h.Write([]byte{0})
	}
	h.Write(env.Payload)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
