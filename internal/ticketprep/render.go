package ticketprep

import (
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Render emits the wrapper-owned report structure. Dynamic values are
// control-escaped and HTML-escaped inside wrapper-owned code containers;
// advisory text cannot create a heading, link, marker, table, HTML element,
// fence, or additional physical line. The captured issue body alone preserves
// line boundaries inside an escaped wrapper-owned preformatted container.
func Render(w io.Writer, packet Packet, current Capture, dispositions *DispositionDocument) error {
	report, err := Freshness(packet, current)
	if err != nil {
		return err
	}
	dispositionByID := map[string]DispositionEntry{}
	if dispositions != nil {
		if err := dispositions.ValidateFor(packet); err != nil {
			return err
		}
		for _, item := range dispositions.Items {
			dispositionByID[item.SuggestionID] = item
		}
	}
	digest := report.PacketDigest
	dispositionIDs := make(map[string]bool)
	for _, suggestion := range Suggestions(packet.Advice) {
		dispositionIDs[suggestion.ID] = true
	}
	b := &strings.Builder{}
	b.WriteString("# Ticket preflight — wrapper-owned report\n\n")
	b.WriteString("> **ADVISORY ONLY:** this packet grants no approval, does not authorize `ben-queue`, and does not establish implementation readiness.\n\n")
	b.WriteString("> **FRESHNESS:** badges compare this packet only with the supplied comparison capture; the offline kernel did not query the forge.\n\n")
	b.WriteString("## Wrapper-established facts\n\n")
	writeFact(b, "packet digest", digest)
	writeFact(b, "repository", packet.Capture.Repository.Identity)
	writeFact(b, "commit", packet.Capture.Repository.Commit)
	writeFact(b, "tree", packet.Capture.Repository.Tree)
	writeFact(b, "issue", fmt.Sprintf("#%d %s", packet.Capture.Subject.IssueNumber, packet.Capture.Subject.IssueURL))
	writeFact(b, "title", packet.Capture.Subject.Title)
	writeFact(b, "content digest", packet.Capture.Subject.ContentDigest)
	writeSubjectBody(b, packet.Capture.Subject.Body)
	b.WriteString("\n### Literal repository observations\n\n")
	if len(packet.Capture.Facts.Paths) == 0 && len(packet.Capture.Facts.Symbols) == 0 && len(packet.Capture.Facts.Unknown) == 0 {
		b.WriteString("- none\n")
	}
	for _, fact := range packet.Capture.Facts.Paths {
		value := fmt.Sprintf("%s => %s", fact.Reference, fact.Status)
		if fact.ResolvedPath != "" {
			value += " at " + fact.ResolvedPath + " (blob " + fact.Blob + ")"
		}
		if fact.Reason != "" {
			value += ": " + fact.Reason
		}
		writeFact(b, "path", value)
	}
	for _, fact := range packet.Capture.Facts.Symbols {
		value := fmt.Sprintf("%s => %s", fact.Reference, fact.Status)
		if fact.Path != "" {
			value += fmt.Sprintf(" at %s:%d (blob %s)", fact.Path, fact.Line, fact.Blob)
		}
		if fact.Reason != "" {
			value += ": " + fact.Reason
		}
		writeFact(b, "Go symbol", value)
	}
	for _, fact := range packet.Capture.Facts.Unknown {
		writeFact(b, "unknown", fact.Reference+": "+fact.Reason)
	}
	b.WriteString("\n### Applicable instructions and declared validation\n\n")
	if len(packet.Capture.Facts.InstructionFiles) == 0 {
		b.WriteString("- instruction files: none\n")
	} else {
		values := make([]string, 0, len(packet.Capture.Facts.InstructionFiles))
		for _, fact := range packet.Capture.Facts.InstructionFiles {
			values = append(values, fact.Path+" (blob "+fact.Blob+")")
		}
		writeFact(b, "instructions", strings.Join(values, "; "))
	}
	if len(packet.Capture.Facts.ValidationCommands) == 0 {
		b.WriteString("- validation commands: none\n")
	} else {
		for _, fact := range packet.Capture.Facts.ValidationCommands {
			writeFact(b, "validation command", fmt.Sprintf("%s [%s:%d; blob %s]", fact.Command, fact.Source, fact.Line, fact.Blob))
		}
	}

	b.WriteString("\n## Declared invocation provenance — not verified\n\n")
	writeFact(b, "provider", packet.DeclaredProvenance.Provider)
	writeFact(b, "model", packet.DeclaredProvenance.Model)
	writeFact(b, "command", packet.DeclaredProvenance.Command)
	writeFact(b, "prompt", packet.DeclaredProvenance.Prompt)

	b.WriteString("\n## Agent-authored advisory\n\n")
	b.WriteString("> **REVIEW ITEMS:** only `DEC-*`, `SPLIT-*`, and `REC-01` require a disposition. An accepted or already-present human decision must select one wrapper-owned `DEC-*-OPT-*` ID. Other IDs are stable supporting references, not separate approval chores.\n")
	suggestions := reportItems(packet.Advice)
	decisionByID := make(map[string]struct {
		index    int
		decision Decision
	}, len(packet.Advice.DecisionQueue))
	for index, decision := range packet.Advice.DecisionQueue {
		decisionByID[numbered("DEC", index)] = struct {
			index    int
			decision Decision
		}{index: index, decision: decision}
	}
	for _, definition := range sectionDefinitions {
		var sectionSuggestions []Suggestion
		for _, suggestion := range suggestions {
			if suggestion.Section == definition.name {
				sectionSuggestions = append(sectionSuggestions, suggestion)
			}
		}
		if len(sectionSuggestions) == 0 {
			continue
		}
		freshness, err := freshnessFor(report, definition.name)
		if err != nil {
			return err
		}
		fmt.Fprintf(b, "\n### %s — %s (%s)\n\n", heading(definition.name), freshnessLabel(freshness.Status), definition.binding)
		if freshness.Status == FreshnessStale {
			fmt.Fprintf(b, "Stale because: %s.\n\n", code(freshness.Reason))
		}
		for _, suggestion := range sectionSuggestions {
			if dispositionIDs[suggestion.ID] {
				disposition := "unreviewed"
				if got, ok := dispositionByID[suggestion.ID]; ok {
					disposition = string(got.Disposition)
					if got.SelectedOptionID != "" {
						disposition += "; selected " + got.SelectedOptionID
					}
				}
				fmt.Fprintf(b, "- **%s** [%s] agent text: %s\n", suggestion.ID, disposition, code(suggestion.Text))
				if entry, ok := decisionByID[suggestion.ID]; ok {
					selected := ""
					if got, exists := dispositionByID[suggestion.ID]; exists {
						selected = got.SelectedOptionID
					}
					for optionIndex, option := range entry.decision.Options {
						id := decisionOptionID(entry.index, optionIndex)
						var markers []string
						if optionIndex+1 == entry.decision.RecommendedOption {
							markers = append(markers, "agent-recommended")
						}
						if id == selected {
							markers = append(markers, "selected")
						}
						marker := ""
						if len(markers) > 0 {
							marker = " [" + strings.Join(markers, ", ") + "]"
						}
						fmt.Fprintf(b, "  - **%s**%s agent option: %s\n", id, marker, code(option))
					}
				}
				continue
			}
			fmt.Fprintf(b, "- **%s** supporting agent text: %s\n", suggestion.ID, code(suggestion.Text))
		}
	}
	b.WriteString("\n---\nThis is a read-only decision aid. Every downstream clarification, documentation, split, tracker edit, or queue action requires an explicit human choice.\n")
	_, err = io.WriteString(w, b.String())
	return err
}

func writeFact(b *strings.Builder, label, value string) {
	fmt.Fprintf(b, "- %s: %s\n", label, code(value))
}

func writeSubjectBody(b *strings.Builder, body string) {
	b.WriteString("\n<details>\n<summary>Captured issue body — declared snapshot, safely escaped</summary>\n\n<pre><code>")
	if body == "" {
		b.WriteString("(empty)")
	} else {
		b.WriteString(safeBlock(body))
	}
	b.WriteString("</code></pre>\n\n</details>\n")
}

func freshnessLabel(status FreshnessStatus) string {
	switch status {
	case FreshnessMatchesCapture:
		return "MATCHES SUPPLIED CAPTURE"
	case FreshnessStale:
		return "STALE AGAINST SUPPLIED CAPTURE"
	default:
		return "UNKNOWN FRESHNESS"
	}
}

func heading(section string) string {
	words := strings.ReplaceAll(section, "_", " ")
	if words == "" {
		return words
	}
	return strings.ToUpper(words[:1]) + words[1:]
}

func code(value string) string { return "<code>" + safeText(value) + "</code>" }

func safeText(value string) string { return safe(value, false) }

func safeBlock(value string) string { return safe(value, true) }

func safe(value string, preserveNewlines bool) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`&#92;&#92;`)
		case '\n':
			if preserveNewlines {
				b.WriteByte('\n')
			} else {
				b.WriteString(`&#92;n`)
			}
		case '\r':
			b.WriteString(`&#92;r`)
		case '\t':
			b.WriteString(`&#92;t`)
		case '&':
			b.WriteString(`&amp;`)
		case '<':
			b.WriteString(`&lt;`)
		case '>':
			b.WriteString(`&gt;`)
		case '"':
			b.WriteString(`&quot;`)
		case '\'':
			b.WriteString(`&#39;`)
		case '#', '[', ']', '(', ')', '|', '`', '*', '_', '!':
			fmt.Fprintf(&b, "&#%d;", r)
		default:
			if unicode.IsControl(r) || unicode.In(r, unicode.Cf) || r == '\u2028' || r == '\u2029' {
				if r <= 0xffff {
					fmt.Fprintf(&b, `\u%04x`, r)
				} else {
					fmt.Fprintf(&b, `\U%08x`, r)
				}
			} else {
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}
