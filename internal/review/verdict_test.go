package review

import (
	"errors"
	"testing"
)

// The reviewer holds no forge credential, so its whole influence is this file.
// Every refusal below leaves the occurrence unrouted, which is the safe
// direction: a human already has the issue in their queue.
func TestParseReport(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want Verdict
		err  error
	}{
		{name: "clean", in: `{"verdict":"clean"}`, want: VerdictClean},
		{name: "changes requested", in: `{"verdict":"changes_requested","findings":"line 3"}`, want: VerdictChangesRequested},
		{name: "whitespace around it", in: "\n  {\"verdict\":\"clean\"}\n", want: VerdictClean},
		{name: "an empty file", in: "", err: ErrNoVerdict},
		{name: "whitespace only", in: "   \n", err: ErrNoVerdict},
		{name: "an empty object", in: `{}`, err: ErrNoVerdict},
		{name: "an empty verdict", in: `{"verdict":""}`, err: ErrNoVerdict},
		{name: "a third word", in: `{"verdict":"approve"}`, err: ErrUnknownVerdict},
		{name: "the wrong case", in: `{"verdict":"CLEAN"}`, err: ErrUnknownVerdict},
		{name: "a misspelled key", in: `{"verdct":"clean"}`, err: ErrUnknownVerdict},
		{name: "an extra key", in: `{"verdict":"clean","route":"revise"}`, err: ErrUnknownVerdict},
		{name: "two raw objects are ambiguous", in: `{"verdict":"clean"}{"verdict":"changes_requested"}`, err: ErrUnknownVerdict},
		{name: "trailing prose is ambiguous", in: `{"verdict":"clean"} but reconsider`, err: ErrUnknownVerdict},
		{name: "not json", in: `clean`, err: ErrNoVerdict},
		{name: "an array", in: `[{"verdict":"clean"}]`, err: ErrNoVerdict},
		{name: "one fenced object", in: "Here it is:\n```json\n{\"verdict\":\"clean\"}\n```\n", want: VerdictClean},
		{name: "one fence with no info string", in: "```\n{\"verdict\":\"changes_requested\"}\n```", want: VerdictChangesRequested},
		{name: "two fenced objects are ambiguous", in: "```json\n{\"verdict\":\"clean\"}\n```\nor maybe\n```json\n{\"verdict\":\"changes_requested\"}\n```", err: ErrUnknownVerdict},
		{name: "a fence with no object", in: "```\nlooks clean to me\n```", err: ErrNoVerdict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReport([]byte(tc.in))
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("error = %v, want %v", err, tc.err)
				}
				if got.Verdict != "" {
					t.Errorf("a refused report returned verdict %q", got.Verdict)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseReport: %v", err)
			}
			if got.Verdict != tc.want {
				t.Errorf("verdict = %q, want %q", got.Verdict, tc.want)
			}
		})
	}
}
