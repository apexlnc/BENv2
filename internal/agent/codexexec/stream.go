package codexexec

import (
	"encoding/json"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Translation of the harness stream to the closed event enum (SPEC §7.2). The
// orchestrator never sees a line of this; everything below the boundary is
// shaped by what `codex exec --json` actually emits, recorded from 0.147.0 (see
// testdata/).
//
// The line kinds observed, in emission order for a run that used a tool:
//
//	{"type":"thread.started","thread_id":"019fe267-…"}
//	{"type":"turn.started"}
//	{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":…}}
//	{"type":"item.started","item":{"id":"item_1","type":"command_execution",…}}
//	{"type":"item.completed","item":{"id":"item_1","type":"command_execution",…}}
//	{"type":"turn.completed","usage":{"input_tokens":…,"output_tokens":…}}
//
// and on failure, after any number of retry notices:
//
//	{"type":"error","message":"unexpected status 401 Unauthorized: …"}
//	{"type":"turn.failed","error":{"message":"unexpected status 401 …"}}
//
// Unrecognized and future line kinds are not dropped: every raw line is
// activity, so anything that does not translate becomes a heartbeat (SPEC
// §7.2). That keeps a harness upgrade from looking like a stall — and it is
// what an `error` line is, too: those are retry notices the turn continues
// past, so only `turn.failed` ends a run.

// streamLine is the subset of the harness's JSON this adapter reads. Fields it
// does not name are ignored by design — an adapter that decoded strictly would
// break on every harness release.
type streamLine struct {
	Type string `json:"type"`

	// type=thread.started
	ThreadID string `json:"thread_id"`

	// type=item.*
	Item *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`

	// type=turn.completed
	Usage *rawUsage `json:"usage"`

	// type=turn.failed
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// rawUsage mirrors the harness's usage object.
//
// InputTokens is the *total* prompt size: cached_input_tokens and
// cache_write_input_tokens are subsets of it, not additions to it — measured
// across a session and its resume, where input_tokens tracked
// cached + cache_write + the few fresh tokens each turn added. Summing them, as
// the claude-code adapter must for its differently-shaped numbers, would double
// count here. reasoning_output_tokens is likewise a subset of output_tokens.
type rawUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// normalized reports best-effort accounting (SPEC §7.2). Cost is absent: this
// harness reports no price, and core.Usage documents 0 as "the adapter cannot
// report cost" — an invented number would be worse than none, since the
// orchestrator's cost cap acts on it (SPEC §9.9).
func (u rawUsage) normalized() *core.Usage {
	return &core.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens}
}

// translate maps one raw line to zero or more normalized events. A nil slice
// means "no normalized meaning" — the caller still counts the line as activity
// and synthesizes a heartbeat.
//
// It never returns an error: a malformed line is the harness's business, and
// refusing to parse it must not end a run that is otherwise progressing.
func translate(line []byte) []core.Event {
	var l streamLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}

	switch l.Type {
	case "thread.started":
		if l.ThreadID == "" {
			return nil
		}
		// The thread id is both identity and the opaque resume token: it is
		// what `codex exec resume` takes (SPEC §7.1).
		return []core.Event{{
			Type:         core.EventStarted,
			SessionID:    l.ThreadID,
			Continuation: l.ThreadID,
		}}

	case "item.completed":
		// Only assistant prose is progress. Reasoning, command executions, and
		// file changes are not forwarded: the transcript retains them verbatim
		// for forensics, and the event model has one text channel.
		if l.Item == nil || l.Item.Type != "agent_message" || strings.TrimSpace(l.Item.Text) == "" {
			return nil
		}
		return []core.Event{{Type: core.EventProgress, Text: l.Item.Text}}

	case "turn.completed":
		var events []core.Event
		if l.Usage != nil {
			events = append(events, core.Event{Type: core.EventUsage, Usage: l.Usage.normalized()})
		}
		// "The agent claims done" — real success is the orchestrator's git-fact
		// verification (SPEC §7.4, §9.7).
		return append(events, core.Event{Type: core.EventSucceeded})

	case "turn.failed":
		var msg string
		if l.Error != nil {
			msg = l.Error.Message
		}
		return []core.Event{{Type: core.EventFailed, Reason: failureReason(msg)}}

	default:
		return nil
	}
}

// failureReason maps a failed turn to the closed failure taxonomy (SPEC §7.3).
// Unmapped harness failures are crashed: retryable, which is the safe default
// for something we cannot name.
//
// The message text is the only discriminator this stream offers — turn.failed
// carries prose and nothing else — so the needles are recorded phrasings rather
// than guesses. A 0.147.0 run with an unusable key ends with
// {"type":"turn.failed","error":{"message":"unexpected status 401 Unauthorized:
// …"}}. Getting this wrong costs real money: crashed is retryable, and a daemon
// would retry a credential problem until it ran out of attempts.
//
// "incorrect api key" is likewise recorded, not guessed: it is what the API
// returns in the body when CODEX_API_KEY holds an unusable key, as distinct
// from the "missing bearer" phrasing above, which is what a *missing*
// credential produces.
func failureReason(msg string) core.FailureReason {
	switch {
	case mentionsAny(msg, "rate limit", "rate_limited", "429", "usage limit", "quota", "overloaded"):
		return core.FailureRateLimited
	case mentionsAny(msg, "401", "403", "unauthorized", "forbidden", "not logged in",
		"incorrect api key", "invalid api key", "missing bearer", "credential", "authentication"):
		return core.FailureAuth
	default:
		return core.FailureCrashed
	}
}

func mentionsAny(haystack string, needles ...string) bool {
	h := strings.ToLower(haystack)
	for _, n := range needles {
		if strings.Contains(h, n) {
			return true
		}
	}
	return false
}
