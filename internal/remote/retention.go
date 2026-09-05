package remote

import (
	"fmt"
	"math"
)

// EventGap is a closed range of backend sequences BEN has accepted it will
// never see.
//
// It exists so that an accepted discontinuity is *evidence* rather than an
// absence. A durable consumer history that jumps from sequence 4 to sequence 9
// is two entirely different situations: a retention expiry BEN measured, wrote
// down and failed the attempt over, or local history that is missing or
// corrupt. Recovery has to tell those apart before it resumes a stream, and
// only a recorded range can.
type EventGap struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

func (g EventGap) String() string { return fmt.Sprintf("[%d, %d]", g.From, g.To) }

// Valid reports whether the range names at least one sequence a backend log can
// actually have (they start at 1).
func (g EventGap) Valid() bool { return g.From >= 1 && g.To >= g.From }

// Contains reports whether seq is inside the accepted range.
func (g EventGap) Contains(seq int64) bool { return seq >= g.From && seq <= g.To }

// RetentionGap is a backend's explicit, self-describing statement that its
// event log no longer holds a range BEN has not consumed: it names the cursor
// the answer is about and the oldest sequence still retained.
//
// Measurable is the whole distinction. Provider output BEN never saw is gone
// either way, and an incomplete attempt may not translate what remains into
// success. But a backend that says exactly which sequences are gone lets BEN
// write that range down, fail the attempt once, and go on retaining what is
// left; an unexplained hole leaves BEN unable to say what it would be skipping,
// and a cursor advanced over sequences nobody can name is exactly what
// ErrEventGap exists to refuse. A succeeded outcome already made durable cannot
// be replaced by that failure, so its caller refuses a later gap without
// advancing instead.
//
// It unwraps to ErrRetentionGap — the measured fact — and to ErrEventGap, so a
// reader that has not learned to accept one keeps refusing it. Advancing across
// a gap is reserved for a reader that routes on the type and durably records
// the range (Attempt).
type RetentionGap struct {
	// RequestedAfter is the cursor the backend is answering about. A gap is
	// admissible only against the cursor BEN actually asked from: an answer
	// about some other position is not evidence about this stream's hole.
	RequestedAfter int64
	// OldestAvailable is the lowest sequence the backend still holds — one past
	// the end of the missing range, and the log's latest sequence plus one when
	// nothing at all is left.
	OldestAvailable int64
	// Cause is the backend's own refusal, retained so the error still names the
	// request that produced it.
	Cause error
}

// Range is the closed range of sequences the expiry removed, and whether the
// backend's two numbers describe a range at all. A floor at or below the next
// sequence BEN would have read describes no loss whatsoever, which makes the
// answer a contradiction rather than a gap.
func (g *RetentionGap) Range() (EventGap, bool) {
	if g == nil || g.RequestedAfter < 0 || g.RequestedAfter == math.MaxInt64 ||
		g.OldestAvailable <= 0 {
		return EventGap{}, false
	}
	from := g.RequestedAfter + 1
	if g.OldestAvailable <= from {
		return EventGap{}, false
	}
	gap := EventGap{From: from, To: g.OldestAvailable - 1}
	if !gap.Valid() {
		return EventGap{}, false
	}
	return gap, true
}

func (g *RetentionGap) Error() string {
	msg := fmt.Sprintf("%s: requested events after %d, oldest retained is %d",
		ErrRetentionGap.Error(), g.RequestedAfter, g.OldestAvailable)
	if g.Cause != nil {
		msg += ": " + g.Cause.Error()
	}
	return msg
}

func (g *RetentionGap) Unwrap() []error {
	out := []error{ErrRetentionGap, ErrEventGap}
	if g.Cause != nil {
		out = append(out, g.Cause)
	}
	return out
}
