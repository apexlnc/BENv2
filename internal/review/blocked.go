package review

import (
	"fmt"
	"strings"
)

// The blocked-review artifact (#284).
//
// A reviewer that never started is a different thing from a reviewer that
// stated nothing. Silence leaves the occurrence unrouted and the next sweep
// looks again, which is right for a run still in flight and for a verdict the
// parser refused. But an execution substrate that *definitively declined to
// admit the run* — a prompt over its stdin bound, a rejected environment key —
// will decline the same request on every sweep, and an issue that sits at
// `ben:needs-review` with no review, no route and no word from the controller
// looks like a review loop that silently skipped it. The deployed canary did
// exactly that for five and a half hours.
//
// So the controller posts one comment carrying this marker, once per refused
// (occurrence, head, reason). It is a *statement*, not a route: the marker
// changes nothing the reducer decides, revokes nothing, and is never read as a
// verdict. What it does is make the failure visible where the human whose label
// is still standing will see it, and make the controller's own repeat
// observation idempotent — a second sweep finds the comment and posts no
// second one.

// ReviewBlockedMarker is the machine identity of one such statement: which
// delivery, claim and approval it is about, which head could not be reviewed,
// and the executor's stable reason.
type ReviewBlockedMarker struct {
	Occurrence int64
	Claim      int64
	Approval   int64
	Head       string
	// Reason is the executor's stable code — lowercase letters, digits, `_`
	// and `-`, at most 64 of them (ValidBlockReason). A field value with
	// whitespace in it would split the marker; one with `-->` in it would end
	// it. Both are refused rather than escaped.
	Reason string
}

func (m ReviewBlockedMarker) String() string {
	return fmt.Sprintf("<!-- %s occurrence=%d claim=%d approval=%d head=%s reason=%s -->",
		markerReviewBlocked, m.Occurrence, m.Claim, m.Approval, m.Head, m.Reason)
}

// ValidBlockReason holds a reason to the token shape a marker field can carry.
func ValidBlockReason(reason string) bool {
	if reason == "" || len(reason) > 64 {
		return false
	}
	for _, r := range reason {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ParseReviewBlockedMarker reads a blocked-review statement's marker out of a
// comment body.
func ParseReviewBlockedMarker(body string) (ReviewBlockedMarker, error) {
	f, err := parseMarker(body, markerReviewBlocked)
	if err != nil {
		return ReviewBlockedMarker{}, err
	}
	var m ReviewBlockedMarker
	if m.Occurrence, err = f.id("occurrence"); err != nil {
		return ReviewBlockedMarker{}, err
	}
	if m.Claim, err = f.id("claim"); err != nil {
		return ReviewBlockedMarker{}, err
	}
	if m.Approval, err = f.id("approval"); err != nil {
		return ReviewBlockedMarker{}, err
	}
	if m.Head, err = f.sha("head"); err != nil {
		return ReviewBlockedMarker{}, err
	}
	if !ValidBlockReason(f["reason"]) {
		return ReviewBlockedMarker{}, fmt.Errorf("%w: reason %q is not a reason token", ErrMalformedMarker, f["reason"])
	}
	m.Reason = f["reason"]
	return m, nil
}

// ReviewBlockedBody is the issue comment the controller posts when the
// reviewer could not be started. The detail is the executor's sanitized
// statement; it is neutralized like findings are, because it came from outside
// the controller and lands in a body the controller signs.
func ReviewBlockedBody(m ReviewBlockedMarker, detail string) string {
	var b strings.Builder
	b.WriteString("**Review controller** could not start the independent review of head `" + m.Head + "`.\n\n")
	b.WriteString("The execution substrate refused to admit the reviewer run (`" + m.Reason + "`), so no review was produced and none will be until the request or the substrate's limits change. ")
	b.WriteString("This is not a verdict on the change: the pull request still needs a human code owner's review, and BEN's assignment and the approval label are untouched.\n\n")
	if d := strings.TrimSpace(SanitizeFindings(detail)); d != "" {
		b.WriteString("- detail: " + d + "\n")
	}
	b.WriteString("- occurrence: `" + fmt.Sprint(m.Occurrence) + "`\n\n")
	b.WriteString(m.String())
	b.WriteString("\n")
	return b.String()
}

// BlockedReviewFor finds the controller's own blocked-review statement for an
// occurrence and head, if one exists. Only the controller's comments count,
// for the reason routeFor gives: an artifact anybody else could post is not the
// durable record.
func BlockedReviewFor(cfg Config, comments []Comment, occurrence int64, head string) (ReviewBlockedMarker, bool) {
	for _, c := range comments {
		if !eqFold(c.Author, cfg.Controller) {
			continue
		}
		m, err := ParseReviewBlockedMarker(c.Body)
		if err != nil || m.Occurrence != occurrence || m.Head != head {
			continue
		}
		return m, true
	}
	return ReviewBlockedMarker{}, false
}
