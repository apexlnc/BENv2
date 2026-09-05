package scenario

import "testing"

func TestOneLineDistinguishesEscapedTextFromControlCharacters(t *testing.T) {
	if got, want := oneLine("line\nbreak"), `line\nbreak`; got != want {
		t.Errorf("oneLine(actual newline) = %q, want %q", got, want)
	}
	if got, want := oneLine(`line\nbreak`), `line\\nbreak`; got != want {
		t.Errorf("oneLine(literal backslash-n) = %q, want %q", got, want)
	}
}

func TestTraceTextPreservesSourceOrderAndEmptySections(t *testing.T) {
	trace := Trace{
		Scenario:      "retry-to-success",
		SchemaVersion: 1,
		Steps: []StepTrace{
			{
				Number:    1,
				Action:    "start until=backoff",
				Observed:  []string{"issue=7 state=backoff", "attempts=1\nforged: effect", `literal\nsequence`},
				Decisions: []string{"running -> backoff: retryable failure"},
				Effects:   []string{"tracker: label 7=ben:claimed"},
				Next:      "advance the manual clock",
			},
			{Number: 2, Action: "advance until=done"},
		},
	}

	want := `scenario: retry-to-success
schema_version: 1

step 1: start until=backoff
observed:
  - issue=7 state=backoff
  - attempts=1\nforged: effect
  - literal\\nsequence
decisions:
  - running -> backoff: retryable failure
effects:
  - tracker: label 7=ben:claimed
next: advance the manual clock

step 2: advance until=done
observed: none
decisions: none
effects: none
next: none
`
	if got := trace.Text(); got != want {
		t.Fatalf("Trace.Text() =\n%s\nwant:\n%s", got, want)
	}
}
