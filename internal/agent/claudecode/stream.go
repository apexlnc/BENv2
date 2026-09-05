package claudecode

import (
	"encoding/json"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/agent/harness"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Translation of the harness stream to the closed event enum (SPEC §7.2). The
// orchestrator never sees a line of this; everything below the boundary is
// shaped by what `claude -p --output-format stream-json` actually emits,
// recorded from 2.1.221 (see testdata/).
//
// The line kinds observed, in emission order for a trivial run:
//
//	{"type":"system","subtype":"init","session_id":…,"model":…,"tools":[…]}
//	{"type":"system","subtype":"thinking_tokens",…}
//	{"type":"assistant","message":{"content":[{"type":"thinking"|"text",…}],"usage":{…}}}
//	{"type":"rate_limit_event","rate_limit_info":{"status":"allowed",…}}
//	{"type":"result","subtype":"success","is_error":false,"total_cost_usd":…,"usage":{…}}
//
// Unrecognized and future line kinds are not dropped: every raw line is
// activity, so anything that does not translate becomes a heartbeat (SPEC
// §7.2). That keeps a harness upgrade from looking like a stall.

// streamLine is the subset of the harness's JSON this adapter reads. Fields it
// does not name are ignored by design — an adapter that decodes strictly would
// break on every harness release.
type streamLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`

	// type=assistant
	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`

	// type=result
	IsError        bool      `json:"is_error"`
	Result         string    `json:"result"`
	TotalCostUSD   float64   `json:"total_cost_usd"`
	APIErrorStatus *int      `json:"api_error_status"`
	Usage          *rawUsage `json:"usage"`
}

// rawUsage mirrors the harness's usage object. Cache reads and writes are real
// input tokens the run was billed for, so they count (SPEC §7.2's best-effort
// normalization).
type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u rawUsage) normalized(cost float64) *core.Usage {
	return &core.Usage{
		InputTokens:  u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
		OutputTokens: u.OutputTokens,
		CostUSD:      cost,
	}
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
	case "system":
		if l.Subtype != "init" || !validSessionID(l.SessionID) {
			return nil
		}
		// The session id is both identity and the opaque resume token: it is
		// what `--resume` takes (SPEC §7.1).
		return []core.Event{{
			Type:         core.EventStarted,
			SessionID:    l.SessionID,
			Continuation: l.SessionID,
		}}

	case "assistant":
		if l.Message == nil {
			return nil
		}
		var texts []string
		for _, c := range l.Message.Content {
			// Only assistant prose is progress. Thinking blocks are not
			// forwarded: they are the model's private reasoning, and the
			// transcript retains them verbatim for forensics either way.
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				texts = append(texts, c.Text)
			}
		}
		if len(texts) == 0 {
			return nil
		}
		// Bounded here, where the raw payload becomes the one event field a
		// consumer retains (#235): everything between this line and the
		// orchestrator's 16 KiB tail is a queue, and a queue holds whatever it
		// is handed. The transcript already has the whole message.
		return []core.Event{{Type: core.EventProgress, Text: harness.BoundText(strings.Join(texts, "\n"))}}

	case "result":
		var events []core.Event
		if l.Usage != nil {
			events = append(events, core.Event{Type: core.EventUsage, Usage: l.Usage.normalized(l.TotalCostUSD)})
		}
		if l.IsError || l.Subtype != "success" {
			return append(events, core.Event{Type: core.EventFailed, Reason: resultReason(l)})
		}
		// "The agent claims done" — real success is the orchestrator's git-fact
		// verification (SPEC §7.4, §9.7).
		return append(events, core.Event{Type: core.EventSucceeded})

	default:
		return nil
	}
}

// validSessionID reports whether an announced session id is the shape this
// harness actually mints. It is the first of two independent anchors on a token
// that the child's own JSON stream chooses and the *next* attempt's argv carries
// (SPEC §7.1, §9.6); the second is in command.go, where the same string becomes
// `--resume <token>`.
//
// This is the anchor that belongs here. SPEC §3.6 puts translation of a raw
// provider payload at the adapter boundary, and this line is that payload:
// everything past it — the started event, the run record, the state directory,
// the next RunSpec — treats the token as opaque and is right to, because "opaque
// to the orchestrator" (SPEC §7.1) is a statement about who may *interpret* it,
// not a licence for nobody to validate it. The adapter that mints it from an
// attacker-controlled stream is the only party that knows what shape it has.
//
// claude-code session ids are UUIDs — verified against 2.1.221, see
// testdata/stream-success.jsonl — so the exact check is available and is the one
// used, rather than a character class that would still accept a token nothing
// here ever emits. Hex digits are accepted in either case: a UUID's case carries
// no meaning, and refusing an uppercase spelling would be this adapter inventing
// a rule about a future release rather than reading the one it measured.
//
// A failing id mints no started event at all, which is the same answer an absent
// one already got: the line stays activity, so the run continues (SPEC §7.2),
// and the attempt ends with no resume token — costing one fresh session, which
// is the conservative direction. Reporting it as `SessionID` while withholding
// it as `Continuation` was the alternative and is worse: it would write an
// identity into the §10.3 record that this function has already concluded the
// harness did not produce.
func validSessionID(id string) bool {
	// 8-4-4-12 hex, hyphens at the fixed offsets, and nothing else. Indices are
	// byte offsets, so a multibyte rune anywhere fails on one test or the other.
	const uuidLen = 36
	if len(id) != uuidLen {
		return false
	}
	for i := range len(id) {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if id[i] != '-' {
				return false
			}
			continue
		}
		if !isHexDigit(id[i]) {
			return false
		}
	}
	return true
}

func isHexDigit(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

// resultReason maps a failed result to the closed failure taxonomy
// (SPEC §7.3). Unmapped harness failures are crashed: retryable, which is the
// safe default for something we cannot name.
func resultReason(l streamLine) core.FailureReason {
	if l.APIErrorStatus != nil {
		switch *l.APIErrorStatus {
		case 429:
			return core.FailureRateLimited
		case 401, 403:
			return core.FailureAuth
		}
	}
	switch l.Subtype {
	case "error_max_budget", "error_budget_exceeded":
		// The harness enforced the cost cap we passed as --max-budget-usd; the
		// orchestrator still owns the verdict and the parking (SPEC §9.9).
		return core.FailureBudgetExceeded
	}
	// The subtype vocabulary is the harness's, not ours, and it grows between
	// releases; classify from the message text only where the taxonomy has a
	// non-retryable answer that matters.
	//
	// The not-logged-in phrasings are recorded, not guessed: a 2.1.221 run
	// without a reachable credential emits
	// {"subtype":"success","is_error":true,"api_error_status":null,
	//  "result":"Not logged in · Please run /login"} — no status code and a
	// "success" subtype, so the prose is the only discriminator. Getting this
	// wrong costs real money: crashed is retryable, and a daemon would retry a
	// login problem until it ran out of attempts.
	switch {
	case mentionsAny(l.Result, "rate limit", "rate_limit", "429", "overloaded"):
		return core.FailureRateLimited
	case mentionsAny(l.Result, "not logged in", "/login", "authentication",
		"unauthorized", "invalid api key", "credential", "oauth"):
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

// Translate is this adapter's raw-line boundary, exported for the substrate
// that cannot reach it through harness.Launch (#194, #46).
//
// The v2 backend runs the same argv somewhere else and hands BEN opaque process
// bytes; internal/remote frames those bytes into complete lines and then calls a
// remote.Translator. That translator has to be *this* function, because parsing
// a provider record is the provider adapter's business and nothing else's
// (SPEC §3.6, §7.7) — a second implementation behind the substrate would be a
// second opinion about what a claude-code line means.
//
// A function value rather than a method: it is stateless by construction, and
// making that visible is what keeps the remote path from acquiring per-attempt
// state the local path does not have.
func Translate(line []byte) []core.Event { return translate(line) }
