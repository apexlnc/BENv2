package codexexec

import (
	"bufio"
	"os"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

const fixtureThread = "11111111-2222-3333-4444-555555555555"

// The fixtures are real 0.147.0 runs (see testdata/README.md): the translator is
// tested against what the harness emits, not against what we remember it
// emitting (SPEC §12.2).
func TestTranslateRecordedSuccessRun(t *testing.T) {
	parallel(t)
	got := translateFixture(t, "testdata/stream-success.jsonl")

	// thread.started → started; turn.started and the command_execution items
	// translate to nothing (the run loop turns those into heartbeats); each
	// agent_message is progress; turn.completed yields usage then the terminal
	// event.
	want := []core.EventType{
		core.EventStarted, core.EventProgress, core.EventProgress,
		core.EventUsage, core.EventSucceeded,
	}
	assertTypes(t, got, want)

	if got[0].SessionID != fixtureThread || got[0].Continuation != fixtureThread {
		t.Errorf("started = {session %q, continuation %q}, want both %q",
			got[0].SessionID, got[0].Continuation, fixtureThread)
	}
	if got[2].Text != "done" {
		t.Errorf("progress text = %q, want %q", got[2].Text, "done")
	}
	// input_tokens is the whole prompt — cached (12396) and cache-write (12488)
	// are subsets of it, and summing them would double count. No cost: this
	// harness reports none.
	want3 := core.Usage{InputTokens: 24890, OutputTokens: 72}
	if *got[3].Usage != want3 {
		t.Errorf("usage = %+v, want %+v", *got[3].Usage, want3)
	}
}

// The failure fixture's shape is the point: retry notices and an error *item*
// are activity, not outcomes, and only turn.failed ends the run (SPEC §7.2).
func TestTranslateRecordedAuthFailure(t *testing.T) {
	parallel(t)
	got := translateFixture(t, "testdata/stream-auth-failure.jsonl")
	assertTypes(t, got, []core.EventType{core.EventStarted, core.EventFailed})

	if got[1].Reason != core.FailureAuth {
		t.Errorf("reason = %q, want %q", got[1].Reason, core.FailureAuth)
	}
	if got[1].Reason.Retryable() {
		t.Error("an auth failure must not be retryable: a daemon would burn every attempt on it")
	}
}

func TestTranslateLines(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name string
		line string
		want []core.Event
	}{
		{
			name: "thread start mints the continuation token",
			line: `{"type":"thread.started","thread_id":"019f-abc"}`,
			want: []core.Event{{Type: core.EventStarted, SessionID: "019f-abc", Continuation: "019f-abc"}},
		},
		{
			name: "thread start without an id is not a start",
			line: `{"type":"thread.started"}`,
		},
		{
			name: "agent prose is progress",
			line: `{"type":"item.completed","item":{"id":"i0","type":"agent_message","text":"working on it"}}`,
			want: []core.Event{{Type: core.EventProgress, Text: "working on it"}},
		},
		{
			name: "blank prose is not progress",
			line: `{"type":"item.completed","item":{"id":"i0","type":"agent_message","text":"  "}}`,
		},
		{
			// The transcript keeps it; the event model has one text channel and
			// it is for what the agent said.
			name: "a completed tool call is activity, not progress",
			line: `{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"ls","exit_code":0}}`,
		},
		{
			name: "an item that only started is activity",
			line: `{"type":"item.started","item":{"id":"i1","type":"agent_message","text":"partial"}}`,
		},
		{
			// Mid-run retry notices: the turn continues past them.
			name: "an error line is activity, not a verdict",
			line: `{"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 Unauthorized)"}`,
		},
		{
			name: "turn completion carries usage then the terminal event",
			line: `{"type":"turn.completed","usage":{"input_tokens":9,"cached_input_tokens":4,"output_tokens":3,"reasoning_output_tokens":1}}`,
			want: []core.Event{
				{Type: core.EventUsage, Usage: &core.Usage{InputTokens: 9, OutputTokens: 3}},
				{Type: core.EventSucceeded},
			},
		},
		{
			// Usage is best-effort: a turn that reports none still succeeds.
			name: "turn completion without usage",
			line: `{"type":"turn.completed"}`,
			want: []core.Event{{Type: core.EventSucceeded}},
		},
		{
			name: "turn failure carries a taxonomy reason",
			line: `{"type":"turn.failed","error":{"message":"stream disconnected before completion"}}`,
			want: []core.Event{{Type: core.EventFailed, Reason: core.FailureCrashed}},
		},
		{
			name: "turn failure with no error object is still a failure",
			line: `{"type":"turn.failed"}`,
			want: []core.Event{{Type: core.EventFailed, Reason: core.FailureCrashed}},
		},
		{
			// A harness release adding a line kind must not look like a stall:
			// the run loop counts it as activity.
			name: "an unknown line kind translates to nothing",
			line: `{"type":"turn.some_future_kind","detail":1}`,
		},
		{
			name: "malformed JSON translates to nothing",
			line: `not json at all`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := translate([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("events = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i].Type != tc.want[i].Type ||
					got[i].SessionID != tc.want[i].SessionID ||
					got[i].Continuation != tc.want[i].Continuation ||
					got[i].Text != tc.want[i].Text ||
					got[i].Reason != tc.want[i].Reason {
					t.Errorf("event %d = %+v, want %+v", i, got[i], tc.want[i])
				}
				if (got[i].Usage == nil) != (tc.want[i].Usage == nil) {
					t.Fatalf("event %d usage = %v, want %v", i, got[i].Usage, tc.want[i].Usage)
				}
				if got[i].Usage != nil && *got[i].Usage != *tc.want[i].Usage {
					t.Errorf("event %d usage = %+v, want %+v", i, *got[i].Usage, *tc.want[i].Usage)
				}
			}
		})
	}
}

// The message text is the only discriminator turn.failed offers, and the
// taxonomy is closed (SPEC §7.3). Getting `auth` wrong costs real money:
// crashed is retryable, so a daemon would retry a credential problem until it
// ran out of attempts.
func TestFailureReasonTaxonomy(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		msg  string
		want core.FailureReason
	}{
		// Recorded verbatim from a 0.147.0 run with an unusable credential.
		{"unexpected status 401 Unauthorized: Missing bearer or basic authentication in header", core.FailureAuth},
		{"unexpected status 403 Forbidden", core.FailureAuth},
		{"Not logged in · run codex login", core.FailureAuth},
		{"Incorrect API key provided", core.FailureAuth},
		{"unexpected status 429 Too Many Requests", core.FailureRateLimited},
		{"You have hit your usage limit; try again later", core.FailureRateLimited},
		{"rate limit exceeded", core.FailureRateLimited},
		{"stream closed before response.completed", core.FailureCrashed},
		{"", core.FailureCrashed},
	} {
		t.Run(tc.msg, func(t *testing.T) {
			if got := failureReason(tc.msg); got != tc.want {
				t.Errorf("failureReason(%q) = %q, want %q", tc.msg, got, tc.want)
			}
		})
	}
}

// Whatever the harness says, the reason stays inside the closed taxonomy
// (SPEC §7.3): the orchestrator applies retry policy from it without inspecting
// agent internals.
func TestReasonsStayInsideTheTaxonomy(t *testing.T) {
	parallel(t)
	allowed := map[core.FailureReason]bool{
		core.FailureCrashed: true, core.FailureRateLimited: true, core.FailureAuth: true,
	}
	for _, msg := range []string{
		"", "something nobody has seen", "418 I'm a teapot", "unexpected status 500",
		"sandbox denied network access", "no rollout found for thread id",
	} {
		if got := failureReason(msg); !allowed[got] {
			t.Errorf("failureReason(%q) = %q, which is outside what this stream can produce", msg, got)
		}
	}
}

func translateFixture(t *testing.T, path string) []core.Event {
	t.Helper()
	f, err := os.Open(path)
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
	return got
}

func assertTypes(t *testing.T, got []core.Event, want []core.EventType) {
	t.Helper()
	types := make([]core.EventType, len(got))
	for i, ev := range got {
		types[i] = ev.Type
	}
	if len(types) != len(want) {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("event types = %v, want %v", types, want)
		}
	}
}
