package harness

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// The bounds on what this package holds of an untrusted child's output (#235),
// as functions. The constants they enforce are with the other sizes in
// harness.go; what lives here is the one discipline all three share, which
// retainOutput in the orchestrator set the pattern for: **count the total,
// keep a bounded part, and state the cut in what is kept.** A bound that is
// silent about itself hands the next reader a record that looks complete and is
// not — for Event.Text that reader is the next attempt's prompt (SPEC §9.6), and
// for a probe it is the operator reading a startup refusal.

// ErrProbeOutput is the refusal for a readiness probe whose child wrote more
// than MaxProbeOutput bytes. The bytes that were retained accompany it, so a
// caller can still quote what it saw (see Excerpt).
var ErrProbeOutput = errors.New("harness: readiness probe output exceeded its bound")

// textNotice is what BoundText appends in place of what it cut. The numbers
// are bytes retained of bytes minted, in that order, matching the account the
// orchestrator renders of the same text (orchestrator attemptAccount.said) so
// an agent reading its previous attempt meets one spelling of "truncated".
//
// It names BEN because the text it lands in is agent-authored and travels into
// the next prompt inside a fence that says so (SPEC §5.6): without the name the
// notice reads as something the agent said. It is not authentication — an agent
// can write these words itself — it is attribution, which is all a notice
// inside untrusted text can be.
const textNotice = "\n[BEN: progress text truncated, %d of %d bytes retained]"

// BoundText bounds one progress event's text at MaxEventText, the notice
// included, cutting on a UTF-8 boundary and stating how much was cut. Text at
// or under the bound is returned as it was.
//
// Both adapters call it where the field is minted (claudecode.translate,
// codexexec.translate), and handle.emit calls it again after redaction, where
// it is a no-op on text already bounded. The two are anchors on the same rule:
// the boundary covers a substrate that reaches Translate without this package
// in the path (internal/remote), the funnel covers an adapter that forgets.
func BoundText(text string) string { return boundText(text, MaxEventText) }

// boundText is BoundText with the bound as a parameter, so a test can drive the
// cut without minting 64 KiB of text per row. The notice must fit inside limit;
// the production bound is thousands of times its length, and a limit under it
// keeps only the notice.
func boundText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	// Room is measured against the notice's *longest* form — the one whose
	// first number is as wide as its second — because the number of bytes kept
	// is what the room decides, and measuring with a guess at it would let the
	// fit depend on the answer. The form actually appended can only be shorter.
	room := limit - len(fmt.Sprintf(textNotice, len(text), len(text)))
	if room < 0 {
		room = 0
	}
	// kept is a substring and shares text's backing array; the concatenation
	// below is what makes the result a **copy**. That is the difference between
	// bounding what is addressed and bounding what is held: a 10 MiB message
	// cut to 64 KiB would otherwise hold the 10 MiB for as long as the event
	// sat in a queue. `+` allocates whenever both operands are non-empty, and
	// the notice never is — TestBoundTextDoesNotShareTheInputsMemory pins the
	// property rather than the mechanism, so a rewrite that returned the
	// substring fails there instead of silently undoing the bound
	// (orchestrator.truncateRunes documents the same trap).
	kept := truncateRunes(text, room)
	return kept + fmt.Sprintf(textNotice, len(kept), len(text))
}

// Excerpt bounds harness output copied into a refusal: the first max bytes, cut
// on a UTF-8 boundary, with an ellipsis where the rest was. It is what lets a
// readiness refusal quote the body it classified from (SPEC §7.1) without a
// binary that answers with a wall of text turning a startup error into one.
// The ellipsis is non-empty, so the concatenation copies here too (boundText).
func Excerpt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return truncateRunes(s, max) + "…"
}

// truncateRunes is the first max bytes of s, cut back to a rune boundary. A
// substring, sharing s's memory: both callers concatenate a non-empty suffix
// onto it, and that is where the copy happens (see boundText).
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// boundedBuffer keeps the first limit bytes written to it and counts the rest,
// accepting every write in full so the copier feeding it never stalls. It is
// the probe's capture (see probe): exec.Cmd drains the child's pipes into
// whatever Stdout and Stderr are, and a writer that refused bytes would leave
// the child blocked on a full pipe rather than bounded.
type boundedBuffer struct {
	limit int
	buf   []byte
	// total is every byte the child wrote, retained or not. Past limit it is
	// the evidence the probe overflowed and the number the refusal states.
	total int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	if room := b.limit - len(b.buf); room > 0 {
		b.buf = append(b.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}
