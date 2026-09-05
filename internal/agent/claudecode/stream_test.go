package claudecode

import (
	"bufio"
	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The session id in testdata/stream-success.jsonl, and the shape 2.1.221 mints
// every time: a UUID, which is what makes validSessionID exact rather than a
// guess at a character class.
const fixtureSession = "11111111-2222-3333-4444-555555555555"

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

	if got[0].SessionID != fixtureSession || got[0].Continuation != fixtureSession {
		t.Errorf("started = {session %q, continuation %q}, want both %q",
			got[0].SessionID, got[0].Continuation, fixtureSession)
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
			line: `{"type":"system","subtype":"init","session_id":"` + fixtureSession + `"}`,
			want: []core.EventType{core.EventStarted},
			check: func(t *testing.T, evs []core.Event) {
				if evs[0].Continuation != fixtureSession {
					t.Errorf("continuation = %q, want %q", evs[0].Continuation, fixtureSession)
				}
			},
		},
		{
			name: "init without a session id is not a start",
			line: `{"type":"system","subtype":"init"}`,
		},
		{
			name: "other system subtypes carry no normalized meaning",
			line: `{"type":"system","subtype":"thinking_tokens","session_id":"` + fixtureSession + `","estimated_tokens":3}`,
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
			// Bounded where it is minted (#235): the transcript has the whole
			// message, the event carries at most harness.MaxEventText of it,
			// with the cut stated.
			name: "oversized prose is bounded at the boundary",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"` +
				strings.Repeat("x", harness.MaxEventText+1) + `"}]}}`,
			want: []core.EventType{core.EventProgress},
			check: func(t *testing.T, evs []core.Event) {
				if n := len(evs[0].Text); n > harness.MaxEventText {
					t.Errorf("text is %d bytes, want at most %d", n, harness.MaxEventText)
				}
				if !strings.HasPrefix(evs[0].Text, "xxxx") || !strings.Contains(evs[0].Text, "truncated") {
					t.Errorf("text does not carry the message's own prefix and a truncation notice: %q…", evs[0].Text[:16])
				}
			},
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

// The resume token is minted here, from the child's own JSON stream, and two
// dispatches later it is an argv element the harness reads back (`--resume
// <token>`, see command). This is the first of the two independent anchors on
// it, and the exact one: 2.1.221 mints UUIDs, so the check is that shape rather
// than a guess at what an opaque token may contain.
//
// A refused id mints no started event at all — the same outcome as an init line
// carrying none, which the table above already covers — so the line is still
// activity, the attempt runs, and the orchestrator is left with no token to
// resume from (SPEC §7.1, §9.6).
func TestInitRefusesASessionIDArgvCannotCarry(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name    string
		id      string
		started bool
	}{
		{name: "the shape 2.1.221 mints", id: fixtureSession, started: true},
		{
			// Same id, and hex has two spellings: refusing one of them would
			// cost a resumable chain for nothing.
			name:    "uppercase hex is the same uuid",
			id:      "AABBCCDD-1122-3344-5566-778899AABBCC",
			started: true,
		},
		{
			// The one this ticket is about: `--resume` takes the next element
			// whatever it is, so this is a flag the agent chose for its own
			// next invocation (#233).
			name: "a bare flag",
			id:   "-p",
		},
		{name: "a long flag carrying its own value", id: "--settings=/tmp/evil.json"},
		{name: "a dash where the first hex digit belongs", id: "-1111111-2222-3333-4444-555555555555"},
		{name: "an interior equals", id: "11111111-2222-3333-4444-55555555=555"},
		{name: "an interior space", id: "11111111-2222-3333-4444-5555555 5555"},
		{name: "a newline, which would be a second line to anything re-parsing it", id: "11111111-2222-3333-4444-5555555\n5555"},
		{name: "empty", id: ""},
		{name: "one byte too long", id: fixtureSession + "5"},
		{name: "one byte too short", id: fixtureSession[:len(fixtureSession)-1]},
		{name: "far too long", id: fixtureSession + strings.Repeat("0", 4096)},
		{
			// 36 bytes and every character legal, but the groups are not where
			// a UUID's are: length alone is not the check.
			name: "hyphens in the wrong places",
			id:   "111111112-222-3333-4444-555555555555",
		},
		{name: "a non-hex letter", id: "g1111111-2222-3333-4444-555555555555"},
		{
			// The indices in validSessionID are byte offsets, so a multibyte
			// rune fails one of the two tests — here, the length.
			name: "a multibyte rune",
			id:   fixtureSession[:len(fixtureSession)-1] + "é",
		},
		{
			// The token the pre-#233 tests used. Opaque, harmless as an argv
			// element, and still refused: this adapter knows the shape, so it
			// holds the stream to it rather than to what argv happens to survive.
			name: "an opaque token that is not a session id",
			id:   "sess-123",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translate([]byte(`{"type":"system","subtype":"init","session_id":` + strconv.Quote(tc.id) + `}`))
			switch {
			case tc.started && (len(got) != 1 || got[0].Type != core.EventStarted):
				t.Fatalf("translate = %v, want one started event", types(got))
			case tc.started && (got[0].Continuation != tc.id || got[0].SessionID != tc.id):
				t.Errorf("started = {session %q, continuation %q}, want both %q",
					got[0].SessionID, got[0].Continuation, tc.id)
			case !tc.started && len(got) != 0:
				t.Errorf("translate = %v, want no events: a refused id is not a start", types(got))
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
		// The harness's verdict on a line past the scanner ceiling (#235).
		core.FailureOutputOverflow,
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
