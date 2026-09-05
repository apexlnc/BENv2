package scenario

import (
	"strings"
	"testing"
)

const validDocument = `{
  "schema_version": 1,
  "name": "retry-to-success",
  "issue": {"identifier": "7"},
  "attempts": [
    {"outcome": "crashed", "session": "session-1"},
    {"outcome": "succeeded", "session": "session-2"}
  ],
  "publication": "complete",
  "steps": [
    {"action": "start", "until": "backoff"},
    {"action": "advance", "until": "done"}
  ]
}`

func TestDecodeIsStrictAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want string
	}{
		{name: "valid", doc: validDocument},
		{
			name: "unknown field",
			doc:  strings.Replace(validDocument, `"name":`, `"surprise": true, "name":`, 1),
			want: "unknown field",
		},
		{
			name: "case-folded root field",
			doc:  strings.Replace(validDocument, `"name":`, `"Name":`, 1),
			want: `unknown field "Name"`,
		},
		{
			name: "case-folded nested field",
			doc:  strings.Replace(validDocument, `"session":`, `"Session":`, 1),
			want: `unknown field "Session"`,
		},
		{
			name: "duplicate root field",
			doc: strings.Replace(validDocument,
				`"name": "retry-to-success",`,
				`"name": "shadow", "name": "retry-to-success",`, 1),
			want: `duplicate field "name"`,
		},
		{
			name: "duplicate nested field",
			doc: strings.Replace(validDocument,
				`"session": "session-1"`,
				`"session": "shadow", "session": "session-1"`, 1),
			want: `duplicate field "session"`,
		},
		{
			name: "unsupported version",
			doc:  strings.Replace(validDocument, `"schema_version": 1`, `"schema_version": 2`, 1),
			want: "schema_version",
		},
		{
			name: "invalid action",
			doc:  strings.Replace(validDocument, `"action": "advance"`, `"action": "teleport"`, 1),
			want: "unsupported value \"teleport\"",
		},
		{
			name: "invalid outcome",
			doc:  strings.Replace(validDocument, `"outcome": "crashed"`, `"outcome": "mystery"`, 1),
			want: "unsupported value \"mystery\"",
		},
		{
			name: "invalid publication",
			doc:  strings.Replace(validDocument, `"publication": "complete"`, `"publication": "asserted"`, 1),
			want: "unsupported value \"asserted\"",
		},
		{
			name: "invalid state",
			doc:  strings.Replace(validDocument, `"until": "backoff"`, `"until": "sleeping"`, 1),
			want: "unsupported value \"sleeping\"",
		},
		{
			name: "missing required value",
			doc:  strings.Replace(validDocument, `"name": "retry-to-success"`, `"name": ""`, 1),
			want: "scenario.name: required",
		},
		{
			name: "control character in a trace identity",
			doc:  strings.Replace(validDocument, `"name": "retry-to-success"`, `"name": "retry\u0000success"`, 1),
			want: "control characters are not allowed",
		},
		{
			name: "too many attempts for the bounded driver",
			doc: strings.Replace(validDocument,
				`{"outcome": "succeeded", "session": "session-2"}`,
				`{"outcome": "crashed", "session": "extra"}, {"outcome": "succeeded", "session": "session-2"}`, 1),
			want: "want 1..2",
		},
		{
			name: "structurally deadlocking sequence",
			doc:  strings.Replace(validDocument, `"until": "backoff"`, `"until": "done"`, 1),
			want: "crashed then succeeded requires",
		},
		{
			name: "trailing document",
			doc:  validDocument + "\n{}",
			want: "trailing JSON value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Decode(strings.NewReader(tc.doc))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Decode: %v", err)
				}
				if got.Name != "retry-to-success" || got.SchemaVersion != SchemaVersion {
					t.Errorf("document = %+v", got)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Decode error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestDecodeRequiresAPositiveRunAbsenceFactForRestart(t *testing.T) {
	doc := strings.Replace(validDocument,
		`{"action": "advance", "until": "done"}`,
		`{"action": "restart", "until": "done"}`, 1)
	doc = strings.Replace(doc, `"outcome": "crashed"`, `"outcome": "running"`, 1)

	_, err := Decode(strings.NewReader(doc))
	if err == nil || !strings.Contains(err.Error(), `prior_run: got "", want "gone"`) {
		t.Fatalf("Decode error = %v, want the missing positive absence fact", err)
	}
}
