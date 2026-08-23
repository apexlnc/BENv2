package claudecode

import (
	"bufio"
	"os"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The fixture is a real 2.1.221 run (see testdata/README.md): the translator is
// tested against what the harness emits, not against what we remember it
// emitting (SPEC §12.2).
func TestTranslateRecordedSuccessRun(t *testing.T) {
	parallel(t)
	f, err := os.Open("testdata/stream-success.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var got []core.Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		got = append(got, translate(sc.Bytes())...)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	// init → started; thinking-only assistant and rate_limit_event translate to
	// nothing (the run loop turns those into heartbeats); the text assistant
	// message is progress; result yields usage then the terminal event.
	want := []core.EventType{
		core.EventStarted, core.EventProgress, core.EventUsage, core.EventSucceeded,
	}
	if len(got) != len(want) {
		t.Fatalf("event types = %v, want %v", types(got), want)
	}
	for i, w := range want {
		if got[i].Type != w {
			t.Fatalf("event types = %v, want %v", types(got), want)
		}
	}

	const session = "11111111-2222-3333-4444-555555555555"
	if got[0].SessionID != session || got[0].Continuation != session {
		t.Errorf("started = {session %q, continuation %q}, want both %q",
			got[0].SessionID, got[0].Continuation, session)
	}
	if got[1].Text != "hi" {
		t.Errorf("progress text = %q, want %q", got[1].Text, "hi")
	}
	// input 9 + cache_creation 8788 + cache_read 16347 = 25144 billed input.
	want3 := core.Usage{InputTokens: 25144, OutputTokens: 44, CostUSD: 0.0194397}
	if *got[2].Usage != want3 {
		t.Errorf("usage = %+v, want %+v", *got[2].Usage, want3)
	}
}

func TestTranslateLines(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name  string
		line  string
		want  []core.EventType
		check func(*testing.T, []core.Event)
	}{
		{
			name: "init mints the continuation token",
			line: `{"type":"system","subtype":"init","session_id":"s1"}`,
			want: []core.EventType{core.EventStarted},
			check: func(t *testing.T, evs []core.Event) {
				if evs[0].Continuation != "s1" {
					t.Errorf("continuation = %q, want s1", evs[0].Continuation)
				}
			},
		},
		{
			name: "init without a session id is not a start",
			line: `{"type":"system","subtype":"init"}`,
		},
		{
			name: "other system subtypes carry no normalized meaning",
			line: `{"type":"system","subtype":"thinking_tokens","session_id":"s1","estimated_tokens":3}`,
		},
		{
			name: "thinking blocks are not forwarded as progress",
			line: `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"private"}]}}`,
		},
		{
			name: "assistant text is progress",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`,
			want: []core.EventType{core.EventProgress},
			check: func(t *testing.T, evs []core.Event) {
				if evs[0].Text != "working" {
					t.Errorf("text = %q", evs[0].Text)
				}
			},
		},
		{
			name: "multiple text blocks join",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"a"},{"type":"thinking","thinking":"x"},{"type":"text","text":"b"}]}}`,
			want: []core.EventType{core.EventProgress},
			check: func(t *testing.T, evs []core.Event) {
				if evs[0].Text != "a\nb" {
					t.Errorf("text = %q, want %q", evs[0].Text, "a\nb")
				}
			},
		},
		{
			name: "whitespace-only text is not progress",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"   "}]}}`,
		},
		{
			name: "successful result yields usage then succeeded",
			line: `{"type":"result","subtype":"success","is_error":false,"total_cost_usd":0.5,"usage":{"input_tokens":1,"output_tokens":2}}`,
			want: []core.EventType{core.EventUsage, core.EventSucceeded},
			check: func(t *testing.T, evs []core.Event) {
				if evs[0].Usage.CostUSD != 0.5 || evs[0].Usage.OutputTokens != 2 {
					t.Errorf("usage = %+v", *evs[0].Usage)
				}
			},
		},
		{
			name: "result without usage still terminates",
			line: `{"type":"result","subtype":"success","is_error":false}`,
			want: []core.EventType{core.EventSucceeded},
		},
		{
			name: "unknown line kinds carry no normalized meaning",
			line: `{"type":"future_event_kind","payload":{"x":1}}`,
		},
		{
			name: "malformed json is not an error",
			line: `{"type":"result"`,
		},
		{
			name: "empty line",
			line: ``,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translate([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("translate = %v, want %v", types(got), tc.want)
			}
			for i, w := range tc.want {
				if got[i].Type != w {
					t.Fatalf("translate = %v, want %v", types(got), tc.want)
				}
			}
			if tc.check != nil {
				tc.check(t, got)
			}
		})
	}
}

func TestResultReasonTaxonomy(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name string
		line string
		want core.FailureReason
	}{
		{
			name: "429 is rate limited",
			line: `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429}`,
			want: core.FailureRateLimited,
		},
		{
			name: "401 is auth",
			line: `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":401}`,
			want: core.FailureAuth,
		},
		{
			name: "403 is auth",
			line: `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":403}`,
			want: core.FailureAuth,
		},
		{
			name: "harness budget stop maps to budget_exceeded",
			line: `{"type":"result","subtype":"error_max_budget","is_error":true}`,
			want: core.FailureBudgetExceeded,
		},
		{
			name: "rate limit prose",
			line: `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"API rate limit reached"}`,
			want: core.FailureRateLimited,
		},
		{
			name: "credential prose",
			line: `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Invalid API key"}`,
			want: core.FailureAuth,
		},
		{
			// Recorded verbatim from claude 2.1.221 with no reachable
			// credential: subtype says success, is_error says otherwise, and
			// there is no status code — the prose is the only discriminator.
			name: "recorded not-logged-in result",
			line: `{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","api_error_status":null,"stop_reason":"stop_sequence","terminal_reason":"api_error"}`,
			want: core.FailureAuth,
		},
		{
			// Retryable is the safe default for a failure we cannot name.
			name: "unnamed failure is crashed",
			line: `{"type":"result","subtype":"error_max_turns","is_error":true}`,
			want: core.FailureCrashed,
		},
		{
			// is_error false but a non-success subtype is still a failure.
			name: "non-success subtype without is_error",
			line: `{"type":"result","subtype":"error_during_execution","is_error":false}`,
			want: core.FailureCrashed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translate([]byte(tc.line))
			last := got[len(got)-1]
			if last.Type != core.EventFailed {
				t.Fatalf("translate = %v, want a failed event", types(got))
			}
			if last.Reason != tc.want {
				t.Errorf("reason = %q, want %q", last.Reason, tc.want)
			}
		})
	}
}

// The failure taxonomy is closed (SPEC §7.3) and the reasons this adapter can
// produce are a subset of it. launch_error is absent on purpose: a launch that
// fails produces no handle and so returns an error from Start rather than an
// event (see TestStartRefusalsProduceNoHandle).
func TestReasonsStayInsideTheTaxonomy(t *testing.T) {
	parallel(t)
	produced := []core.FailureReason{
		core.FailureCrashed, core.FailureStalled, core.FailureTimeout,
		core.FailureRateLimited, core.FailureAuth, core.FailureKilled,
		core.FailureBudgetExceeded,
	}
	retryable := map[core.FailureReason]bool{
		core.FailureCrashed: true, core.FailureStalled: true,
		core.FailureTimeout: true, core.FailureRateLimited: true,
	}
	for _, r := range produced {
		if r.Retryable() != retryable[r] {
			t.Errorf("%s.Retryable() = %v, want %v", r, r.Retryable(), retryable[r])
		}
	}
}

func types(evs []core.Event) []core.EventType {
	out := make([]core.EventType, len(evs))
	for i, e := range evs {
		out[i] = e.Type
	}
	return out
}
