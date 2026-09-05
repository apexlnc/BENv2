package scenario

import (
	"fmt"
	"strings"
	"unicode"
)

// Trace is the normalized diagnostic account of one replay. Every list is
// ordered by the fact source that supplied it; callers normalize unordered
// sets before adding them.
type Trace struct {
	Scenario      string
	SchemaVersion int
	Steps         []StepTrace
}

type StepTrace struct {
	Number    int
	Action    string
	Observed  []string
	Decisions []string
	Effects   []string
	Next      string
}

// Text renders a byte-stable, line-oriented account suitable for a reviewed
// golden file. It deliberately accepts no maps: map iteration must not be able
// to change diagnostic evidence from one replay to the next.
func (t Trace) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "scenario: %s\nschema_version: %d\n", oneLine(t.Scenario), t.SchemaVersion)
	for _, step := range t.Steps {
		fmt.Fprintf(&b, "\nstep %d: %s\n", step.Number, oneLine(step.Action))
		writeLines(&b, "observed", step.Observed)
		writeLines(&b, "decisions", step.Decisions)
		writeLines(&b, "effects", step.Effects)
		if step.Next == "" {
			b.WriteString("next: none\n")
		} else {
			fmt.Fprintf(&b, "next: %s\n", oneLine(step.Next))
		}
	}
	return b.String()
}

func writeLines(b *strings.Builder, heading string, lines []string) {
	if len(lines) == 0 {
		fmt.Fprintf(b, "%s: none\n", heading)
		return
	}
	fmt.Fprintf(b, "%s:\n", heading)
	for _, line := range lines {
		fmt.Fprintf(b, "  - %s\n", oneLine(line))
	}
}

// oneLine keeps values inside their trace field. Most inputs are BEN-owned
// enums and reasons, but future boundary errors may contain arbitrary text; a
// newline in one must not be able to manufacture a decision or effect row.
func oneLine(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if unicode.IsControl(r) {
				fmt.Fprintf(&b, `\u%04x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
