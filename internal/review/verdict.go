package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	// ErrNoVerdict is a report that states nothing: an absent file, an empty
	// one, or one whose verdict field is blank. It authorizes no routing —
	// silence from a reviewer is not a clean bill.
	ErrNoVerdict = errors.New("review: the reviewer stated no verdict")
	// ErrUnknownVerdict is a report outside the closed set. A third word is a
	// reviewer that has misunderstood its contract, and guessing which side of
	// the loop it meant is how a stop becomes a revise.
	ErrUnknownVerdict = errors.New("review: verdict is outside the closed set")
)

// Report is what the reviewer model writes and the controller validates. It is
// a file rather than a request because the reviewer holds no forge credential:
// the model reads a diff and writes JSON, and trusted code decides what — if
// anything — that becomes on the pull request.
type Report struct {
	Verdict  Verdict `json:"verdict"`
	Findings string  `json:"findings,omitempty"`
}

// ParseReport reads the reviewer's verdict file.
//
// Strict on purpose, and strict in the direction that fails safe: unknown
// fields are refused so a `verdct` typo cannot read as an absent verdict with
// a decorative extra key, and both refusals below leave the occurrence
// unrouted rather than routing it either way. An unrouted occurrence is
// retried by the scheduled sweep and, if it keeps failing, is simply an issue
// waiting for the human who already has it in their queue.
func ParseReport(data []byte) (Report, error) {
	body := strings.TrimSpace(string(data))
	if body == "" {
		return Report{}, ErrNoVerdict
	}
	if !strings.HasPrefix(body, "{") {
		// A model asked for JSON often returns it inside a fenced block, so
		// one fenced object is accepted. *One*: a reply carrying an example
		// alongside the answer is ambiguous, and picking either would make the
		// verdict depend on which the model wrote first.
		fenced, err := soleFencedObject(body)
		if err != nil {
			return Report{}, err
		}
		body = fenced
	}

	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()

	var r Report
	if err := dec.Decode(&r); err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrUnknownVerdict, err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("more than one JSON value")
		}
		return Report{}, fmt.Errorf("%w: trailing content: %v", ErrUnknownVerdict, err)
	}
	switch r.Verdict {
	case "":
		return Report{}, ErrNoVerdict
	case VerdictClean, VerdictChangesRequested:
		return r, nil
	default:
		return Report{}, fmt.Errorf("%w: %q", ErrUnknownVerdict, r.Verdict)
	}
}

// soleFencedObject returns the one fenced JSON object in a reply, or a
// refusal. It is deliberately dumb: find every ``` fence, keep the blocks
// whose content begins with `{`, and insist there is exactly one.
func soleFencedObject(body string) (string, error) {
	parts := strings.Split(body, "```")
	var found []string
	// Fenced content is at the odd indices of a split on the fence marker.
	for i := 1; i < len(parts); i += 2 {
		block := parts[i]
		// Drop an info string such as `json` on the opening fence.
		if nl := strings.IndexByte(block, '\n'); nl >= 0 && !strings.HasPrefix(strings.TrimSpace(block), "{") {
			block = block[nl+1:]
		}
		if block = strings.TrimSpace(block); strings.HasPrefix(block, "{") {
			found = append(found, block)
		}
	}
	switch len(found) {
	case 0:
		return "", ErrNoVerdict
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("%w: the reply carries %d JSON objects, so none of them is the verdict", ErrUnknownVerdict, len(found))
	}
}

// maxFindings bounds the prose the controller will republish. GitHub refuses a
// review body over 65536 characters, and a refusal here is a round that cannot
// complete, so the model's output is truncated rather than allowed to fail the
// publication it belongs to.
const maxFindings = 60000

// ReviewBody assembles what the controller publishes: a fixed header that says
// what this is and what it is not, the model's findings with every comment
// opener neutralized, and the marker that makes the review machine-readable.
//
// The header is not decoration. A `COMMENT` review from a bot is easy to
// mistake for an approval at a glance, and #11's whole safety argument is that
// it never is one.
func ReviewBody(m ReviewMarker, findings string) string {
	findings = strings.TrimSpace(SanitizeFindings(findings))
	if len(findings) > maxFindings {
		findings = findings[:maxFindings] + "\n\n_(truncated)_"
	}
	if findings == "" {
		findings = "_The reviewer returned no findings._"
	}

	var b strings.Builder
	b.WriteString("**Automated review** — advisory only. This is a `COMMENT` review by BEN's review controller; it is not an approval, and a human code owner's review is still required.\n\n")
	b.WriteString("- verdict: `" + string(m.Verdict) + "`\n")
	b.WriteString("- head: `" + m.Head + "`\n")
	if m.ReviewerProfile != "" {
		b.WriteString("- reviewer profile: `" + m.ReviewerProfile + "`\n")
	}
	b.WriteString("\n")
	b.WriteString(findings)
	b.WriteString("\n\n")
	b.WriteString(m.String())
	b.WriteString("\n")
	return b.String()
}

// RouteBody is the issue comment recording a completed route. Its marker is
// the idempotency key for the occurrence, so this is the last write of every
// cycle and never the first.
func RouteBody(m RouteMarker, why string) string {
	var b strings.Builder
	b.WriteString("**Review controller** routed occurrence `" + fmt.Sprint(m.Occurrence) + "` to `" + string(m.Outcome) + "`.\n\n")
	b.WriteString("- head: `" + m.Head + "`\n")
	b.WriteString("- reason: " + strings.TrimSpace(SanitizeFindings(why)) + "\n\n")
	b.WriteString(m.String())
	b.WriteString("\n")
	return b.String()
}

// RouteIntentBody records a terminal decision before its label mutation. It is
// not the route's idempotency key — RouteBody remains the completed record —
// but it preserves the subject and outcome needed to repair that record after
// a crash, subject movement, or pull-request closure.
func RouteIntentBody(m RouteIntentMarker, why string) string {
	var b strings.Builder
	b.WriteString("**Review controller** prepared terminal route `" + string(m.Outcome) + "` for occurrence `" + fmt.Sprint(m.Occurrence) + "`.\n\n")
	b.WriteString("- head: `" + m.Head + "`\n")
	b.WriteString("- approval event: `" + fmt.Sprint(m.Approval) + "`\n")
	b.WriteString("- reason: " + strings.TrimSpace(SanitizeFindings(why)) + "\n\n")
	b.WriteString(m.String())
	b.WriteString("\n")
	return b.String()
}
