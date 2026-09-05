package harness

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"
)

// The three bounds on what this package holds of a child's output (#235). The
// scanner ceiling's classification is only real against a process and is
// asserted by the conformance suite (agenttest testOversizedLine); what is
// pinned here is the pure part — the cut, the notice, and the copy — and the
// probe's capture, which needs no harness to overflow.

func TestBoundTextKeepsTheBoundNoticeIncluded(t *testing.T) {
	// A limit small enough to drive by hand, and large enough that the notice's
	// longest form fits with room to spare for text.
	const limit = 120
	// The room left for text once the notice is measured, for a text whose
	// length has three digits — which every row below has, so the cut lands at
	// the same byte in each.
	room := limit - len(fmt.Sprintf(textNotice, 100, 100))
	for _, tc := range []struct {
		name string
		text string
	}{
		{"one byte over", strings.Repeat("a", limit+1)},
		{"far over", strings.Repeat("a", 8*limit)},
		{"multi-byte runes throughout", strings.Repeat("é", limit)},
		// The first rune of 日本語 begins one byte before the cut and spans it,
		// so the cut has to back up to keep the string valid.
		{"a rune straddling the cut", strings.Repeat("a", room-1) + "日本語" + strings.Repeat("a", limit)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := boundText(tc.text, limit)
			if len(got) > limit {
				t.Fatalf("len = %d, want <= %d: the notice is inside the bound, not on top of it", len(got), limit)
			}
			if !utf8.ValidString(got) {
				t.Errorf("cut split a rune: %q", got)
			}
			at := strings.LastIndex(got, "\n[")
			if at < 0 {
				t.Fatalf("no notice in %q", got)
			}
			kept := got[:at]
			if !strings.HasPrefix(tc.text, kept) {
				t.Errorf("what was kept is not a prefix of the text: %q", kept)
			}
			want := fmt.Sprintf(textNotice, len(kept), len(tc.text))
			if !strings.HasSuffix(got, want) {
				t.Errorf("notice = %q, want %q", got[len(kept):], want)
			}
		})
	}
}

func TestBoundTextLeavesTextInsideTheBoundAlone(t *testing.T) {
	for _, text := range []string{"", "short", strings.Repeat("x", MaxEventText)} {
		if got := BoundText(text); got != text {
			t.Errorf("BoundText(%d bytes) changed text that was inside the bound", len(text))
		}
	}
	// And the production bound is the one the exported form applies.
	over := strings.Repeat("x", MaxEventText+1)
	got := BoundText(over)
	if len(got) > MaxEventText || len(got) >= len(over) {
		t.Errorf("BoundText(%d bytes) = %d bytes, want <= %d", len(over), len(got), MaxEventText)
	}
	if !strings.Contains(got, fmt.Sprintf("of %d bytes", len(over))) {
		t.Errorf("the notice does not state the size that was cut: %q", got[len(got)-80:])
	}
}

// The bounded text is a copy. A substring shares its backing array, so a 10 MiB
// message bounded to 64 KiB would hold the 10 MiB for as long as the event sat
// in a queue — the bound would be on what is addressed, not on what is held.
// Pinned as a property of the result rather than of a particular call, because
// the copy is made by the concatenation with the notice (boundText), and a
// rewrite that returned the substring would pass every other test here.
func TestBoundTextDoesNotShareTheInputsMemory(t *testing.T) {
	big := strings.Repeat("y", 4*MaxEventText)
	got := BoundText(big)
	base := uintptr(unsafe.Pointer(unsafe.StringData(big)))
	at := uintptr(unsafe.Pointer(unsafe.StringData(got)))
	if at >= base && at < base+uintptr(len(big)) {
		t.Error("the bounded text points into the input's backing array")
	}
}

func TestExcerpt(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"", 5, ""},
		{"short", 5, "short"},
		{"longer than that", 6, "longer…"},
		{"日本語です", 7, "日本…"}, // 7 bytes is two runes and a third of one
	} {
		if got := Excerpt(tc.in, tc.max); got != tc.want {
			t.Errorf("Excerpt(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// A probe that floods stdout is refused with what was retained, and it is
// refused *promptly*: the capture keeps draining past the bound, so the child is
// never left blocked on a full pipe waiting for a reader that has stopped.
func TestProbeOutputIsBounded(t *testing.T) {
	flood := 4 * MaxProbeOutput
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	out, err := Probe(ctx, Timings{}, "/bin/sh", []string{"PATH=/usr/bin:/bin"},
		"-c", fmt.Sprintf("head -c %d /dev/zero", flood))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrProbeOutput) {
		t.Fatalf("err = %v, want ErrProbeOutput", err)
	}
	if len(out) != MaxProbeOutput {
		t.Errorf("retained %d bytes, want exactly the bound %d", len(out), MaxProbeOutput)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("%d bytes written", flood)) {
		t.Errorf("the refusal does not state what was written: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("the probe ran into its context: the child was not drained past the bound")
	}
	// Generous, and only a sanity bound: a child left blocked on the pipe would
	// have sat until the context above expired and been reported by it.
	if elapsed > 10*time.Second {
		t.Errorf("probe took %v for %d bytes", elapsed, flood)
	}
}

// Exactly the bound is not over it: a refusal on the boundary would make the
// bound one byte smaller than it says.
func TestProbeOutputAtTheBoundIsNotRefused(t *testing.T) {
	out, err := Probe(context.Background(), Timings{}, "/bin/sh", []string{"PATH=/usr/bin:/bin"},
		"-c", fmt.Sprintf("head -c %d /dev/zero", MaxProbeOutput))
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(out) != MaxProbeOutput {
		t.Errorf("retained %d bytes, want %d", len(out), MaxProbeOutput)
	}
}

// The combined form shares one bound across both streams — the point of the
// capture being one writer — and a probe that both fails and floods reports the
// flood with the exit folded in, never the exit alone.
func TestProbeCombinedOutputIsBoundedAcrossBothStreams(t *testing.T) {
	half := MaxProbeOutput/2 + 1
	out, err := ProbeCombined(context.Background(), Timings{}, "/bin/sh", []string{"PATH=/usr/bin:/bin"},
		"-c", fmt.Sprintf("head -c %d /dev/zero; head -c %d /dev/zero >&2; exit 3", half, half))
	if !errors.Is(err, ErrProbeOutput) {
		t.Fatalf("err = %v, want ErrProbeOutput", err)
	}
	if !strings.Contains(err.Error(), "exit status 3") {
		t.Errorf("the refusal dropped the exit status: %v", err)
	}
	if len(out) != MaxProbeOutput {
		t.Errorf("retained %d bytes, want %d", len(out), MaxProbeOutput)
	}
}
