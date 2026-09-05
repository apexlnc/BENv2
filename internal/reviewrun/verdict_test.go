package reviewrun

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

func TestExtractVerdictUnwrapsCodexJSONLAgentMessages(t *testing.T) {
	line := func(v any) string {
		t.Helper()
		body, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	answer := VerdictOpen + "\n" +
		`{"verdict":"changes_requested","findings":"line 12"}` + "\n" + VerdictClose
	output := strings.Join([]string{
		line(map[string]any{"type": "thread.started", "thread_id": "t"}),
		line(map[string]any{"type": "item.completed", "item": map[string]any{
			"id": "i1", "type": "agent_message", "text": "working",
		}}),
		line(map[string]any{"type": "item.completed", "item": map[string]any{
			"id": "i2", "type": "agent_message", "text": answer,
		}}),
		line(map[string]any{"type": "turn.completed", "usage": map[string]any{"output_tokens": 20}}),
	}, "\n") + "\n"

	report, err := ExtractVerdict([]byte(output))
	if err != nil {
		t.Fatalf("ExtractVerdict: %v\n%s", err, output)
	}
	if report.Verdict != review.VerdictChangesRequested || report.Findings != "line 12" {
		t.Fatalf("report = %+v", report)
	}
}

func TestMalformedCodexJSONLNeverFallsBackToARetainedPrefix(t *testing.T) {
	output := `{"type":"item.completed","item":{"type":"agent_message","text":"` +
		VerdictOpen + `\n{\"verdict\":\"clean\"}\n` + VerdictClose + `"}}` + "\n" +
		`{"type":"turn.completed"` // a sealed but malformed final line
	if _, err := ExtractVerdict([]byte(output)); !errors.Is(err, ErrNoVerdictBlock) {
		t.Fatalf("ExtractVerdict = %v, want ErrNoVerdictBlock", err)
	}
}

func TestStderrDiagnosticsDoNotCorruptCodexJSONL(t *testing.T) {
	answer := VerdictOpen + "\n" + `{"verdict":"clean"}` + "\n" + VerdictClose
	line, err := json.Marshal(map[string]any{
		"type": "item.completed",
		"item": map[string]any{"type": "agent_message", "text": answer},
	})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := admit(Record{Run: "codex-stderr"}, []Chunk{
		{Seq: 1, Stream: ChunkStderr, Payload: []byte("warning: retrying transport\n")},
		{Seq: 2, Stream: ChunkStdout, Payload: append(line, '\n')},
	})
	if err != nil {
		t.Fatal(err)
	}
	report, err := ExtractVerdict(rec.Output)
	if err != nil {
		t.Fatalf("ExtractVerdict: %v\n%s", err, rec.Output)
	}
	if report.Verdict != review.VerdictClean {
		t.Fatalf("report = %+v", report)
	}
}
