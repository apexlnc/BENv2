package review

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The controller's whole durable vocabulary. HTML comments: legible to us,
// invisible in rendered markdown, and not prose — the same choice SPEC §3.3
// and §8.4 make for BEN's own milestone markers, for the same reason. The
// controller reads its own state back out of these and nothing else.
const (
	markerReview        = "ben:review"
	markerRoute         = "ben:route"
	markerRouteIntent   = "ben:route-intent"
	markerReviewBlocked = "ben:review-blocked"
	markerMilestone     = "ben:milestone"
)

var (
	// ErrNoMarker is "this body is not one of ours" — the ordinary answer for
	// every human comment on the issue, and not a fault.
	ErrNoMarker = errors.New("review: no marker")
	// ErrAmbiguousMarker is a body carrying two markers of one kind. Only one
	// can be the artifact's identity, and guessing which is how a model that
	// wrote a marker into its findings gets to name its own head. Refuse.
	ErrAmbiguousMarker = errors.New("review: more than one marker of the same kind")
	// ErrMalformedMarker is a marker of ours whose fields do not parse. It is
	// distinct from ErrNoMarker because it means something wrote a marker
	// wrongly, which is worth saying out loud.
	ErrMalformedMarker = errors.New("review: malformed marker")
)

// ReviewMarker is the machine identity of one published controller review:
// which delivery it answers, which approval selected its workspace cycle,
// which claim it was taken under, both endpoints of the exact diff it judged,
// and what it judged.
type ReviewMarker struct {
	Occurrence int64
	Claim      int64
	// Approval is zero only when parsing the legacy marker shape that predates
	// this field. The reducer must bind that shape to the ordered event history
	// before treating it as a durable review.
	Approval int64
	Head     string
	Base     string
	Verdict  Verdict
	// ReviewerProfile is absent only on legacy markers written by the
	// one-argv controller. Named-profile deployments always record it.
	ReviewerProfile string
}

func (m ReviewMarker) String() string {
	profile := ""
	if m.ReviewerProfile != "" {
		profile = " profile=" + m.ReviewerProfile
	}
	return fmt.Sprintf("<!-- %s occurrence=%d claim=%d approval=%d head=%s base=%s verdict=%s%s -->",
		markerReview, m.Occurrence, m.Claim, m.Approval, m.Head, m.Base, m.Verdict, profile)
}

// RouteMarker is the machine identity of one completed route. Its presence for
// an occurrence is what makes redelivery a no-op, so it is posted last, after
// the mutation it records has landed.
type RouteMarker struct {
	Occurrence int64
	Claim      int64
	Head       string
	Outcome    Outcome
}

func (m RouteMarker) String() string {
	return fmt.Sprintf("<!-- %s occurrence=%d claim=%d head=%s outcome=%s -->",
		markerRoute, m.Occurrence, m.Claim, m.Head, m.Outcome)
}

// RouteIntentMarker is the durable decision for a terminal route that has no
// occurrence-bound review of its own, such as no-progress or a pre-review
// round cap. It is posted before label removal so recovery retains the exact
// occurrence, head and outcome even if the subject moves afterwards. Approval
// is the queue-label application standing at the occurrence; a later event is
// a fresh human approval this intent must not revoke.
type RouteIntentMarker struct {
	Occurrence int64
	Claim      int64
	Approval   int64
	Head       string
	Outcome    Outcome
}

func (m RouteIntentMarker) String() string {
	return fmt.Sprintf("<!-- %s occurrence=%d claim=%d approval=%d head=%s outcome=%s -->",
		markerRouteIntent, m.Occurrence, m.Claim, m.Approval, m.Head, m.Outcome)
}

// ParseReviewMarker reads a controller review's marker out of its body.
func ParseReviewMarker(body string) (ReviewMarker, error) {
	f, err := parseMarker(body, markerReview)
	if err != nil {
		return ReviewMarker{}, err
	}
	var m ReviewMarker
	if m.Occurrence, err = f.id("occurrence"); err != nil {
		return ReviewMarker{}, err
	}
	if m.Claim, err = f.id("claim"); err != nil {
		return ReviewMarker{}, err
	}
	if _, ok := f["approval"]; !ok {
		m.Approval = 0
	} else if m.Approval, err = f.id("approval"); err != nil {
		return ReviewMarker{}, err
	}
	if m.Head, err = f.sha("head"); err != nil {
		return ReviewMarker{}, err
	}
	if m.Base, err = f.sha("base"); err != nil {
		return ReviewMarker{}, err
	}
	if profile, ok := f["profile"]; ok {
		if !ValidReviewerProfile(profile) {
			return ReviewMarker{}, fmt.Errorf("%w: profile %q is not a valid reviewer profile", ErrMalformedMarker, profile)
		}
		m.ReviewerProfile = profile
	}
	switch v := Verdict(f["verdict"]); v {
	case VerdictClean, VerdictChangesRequested:
		m.Verdict = v
	default:
		return ReviewMarker{}, fmt.Errorf("%w: verdict %q is outside the closed set", ErrMalformedMarker, f["verdict"])
	}
	return m, nil
}

// ParseRouteMarker reads a completed route's marker out of a comment body.
func ParseRouteMarker(body string) (RouteMarker, error) {
	f, err := parseMarker(body, markerRoute)
	if err != nil {
		return RouteMarker{}, err
	}
	var m RouteMarker
	if m.Occurrence, err = f.id("occurrence"); err != nil {
		return RouteMarker{}, err
	}
	if m.Claim, err = f.id("claim"); err != nil {
		return RouteMarker{}, err
	}
	if m.Head, err = f.sha("head"); err != nil {
		return RouteMarker{}, err
	}
	switch o := Outcome(f["outcome"]); o {
	case OutcomeRevise, OutcomeHumanReview, OutcomeRoundCap, OutcomeNoProgress:
		m.Outcome = o
	default:
		return RouteMarker{}, fmt.Errorf("%w: outcome %q is outside the closed set", ErrMalformedMarker, f["outcome"])
	}
	return m, nil
}

// ParseRouteIntentMarker reads a terminal route decision recorded before its
// mutation. Revise is deliberately excluded: its occurrence-bound review is
// already the durable decision, and a stale positive route must never become
// authoritative merely because an intent comment exists.
func ParseRouteIntentMarker(body string) (RouteIntentMarker, error) {
	f, err := parseMarker(body, markerRouteIntent)
	if err != nil {
		return RouteIntentMarker{}, err
	}
	var m RouteIntentMarker
	if m.Occurrence, err = f.id("occurrence"); err != nil {
		return RouteIntentMarker{}, err
	}
	if m.Claim, err = f.id("claim"); err != nil {
		return RouteIntentMarker{}, err
	}
	if m.Approval, err = f.id("approval"); err != nil {
		return RouteIntentMarker{}, err
	}
	if m.Head, err = f.sha("head"); err != nil {
		return RouteIntentMarker{}, err
	}
	switch o := Outcome(f["outcome"]); o {
	case OutcomeHumanReview, OutcomeRoundCap, OutcomeNoProgress:
		m.Outcome = o
	default:
		return RouteIntentMarker{}, fmt.Errorf("%w: terminal outcome %q is outside the closed set", ErrMalformedMarker, f["outcome"])
	}
	return m, nil
}

// PublishedOccurrence reads the occurrence out of BEN's `published` milestone
// marker, which is the only kind of milestone that drives this controller.
//
// The marker is the trigger, never the headline: SPEC §8.4's prose is written
// for a human and may be reworded without anybody thinking of this file, while
// the marker is the contract. A `claimed`, `failed` or `needs-review`
// milestone parses here as ErrNoMarker, which is the correct answer — those
// occurrences are not deliveries of a pull request.
func PublishedOccurrence(body string) (int64, error) {
	f, err := parseMarker(body, markerMilestone)
	if err != nil {
		return 0, err
	}
	if f["kind"] != "published" {
		return 0, ErrNoMarker
	}
	return f.id("occurrence")
}

type fields map[string]string

func (f fields) id(key string) (int64, error) {
	raw, ok := f[key]
	if !ok {
		return 0, fmt.Errorf("%w: no %s field", ErrMalformedMarker, key)
	}
	// Strictly positive: GitHub event ids start well above zero, and a zero
	// here would compare equal to "no claim epoch" everywhere downstream.
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%w: %s=%q is not a tracker event id", ErrMalformedMarker, key, raw)
	}
	return v, nil
}

func (f fields) sha(key string) (string, error) {
	raw := f[key]
	if !isFullSHA(raw) {
		return "", fmt.Errorf("%w: %s=%q is not a full commit sha", ErrMalformedMarker, key, raw)
	}
	return raw, nil
}

// isFullSHA holds the marker to the 40-hex form the API returns. An
// abbreviation would make two heads compare unequal that are the same commit,
// and the controller only ever writes back what GitHub gave it.
func isFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// parseMarker finds the single `<!-- <name> k=v ... -->` in body and splits
// its fields.
//
// "Single" is the security-relevant word. Findings text is model-authored and
// reaches a body the controller signs with its own identity, so a body is
// trusted to carry one marker or none; two is a refusal rather than a
// first-wins or last-wins race between the machinery and whatever the model
// decided to write. SanitizeFindings removes the opportunity, and this closes
// the case where something else creates one.
func parseMarker(body, name string) (fields, error) {
	open := "<!-- " + name + " "
	idx := strings.Index(body, open)
	if idx < 0 {
		return nil, ErrNoMarker
	}
	if strings.Contains(body[idx+len(open):], open) {
		return nil, fmt.Errorf("%w: %s", ErrAmbiguousMarker, name)
	}
	rest := body[idx+len(open):]
	end := strings.Index(rest, "-->")
	if end < 0 {
		return nil, fmt.Errorf("%w: %s is unterminated", ErrMalformedMarker, name)
	}

	f := fields{}
	for _, tok := range strings.Fields(rest[:end]) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			return nil, fmt.Errorf("%w: %s carries %q, which is not key=value", ErrMalformedMarker, name, tok)
		}
		if _, dup := f[k]; dup {
			return nil, fmt.Errorf("%w: %s repeats %q", ErrMalformedMarker, name, k)
		}
		f[k] = v
	}
	return f, nil
}

// SanitizeFindings neutralizes every HTML comment opener in model-authored
// text before it is published under the controller's identity.
//
// The reviewer model reads a pull request diff, which is attacker-controlled
// in exactly the sense SPEC §6.7 means: whoever can open a PR can write text
// into it asking the reviewer to emit `<!-- ben:route ... outcome=revise -->`.
// The verdict itself is already closed and validated, but the findings prose
// is not, and it lands in a body the controller signs. Breaking the opener
// with a space keeps the text readable and makes it impossible for it to
// become a marker — including a marker of a kind this package does not know
// about yet.
func SanitizeFindings(s string) string {
	return strings.ReplaceAll(s, "<!--", "< !--")
}
