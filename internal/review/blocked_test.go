package review

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestBlockedMarkerRoundTrip(t *testing.T) {
	want := ReviewBlockedMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Reason: "payload_too_large"}
	body := ReviewBlockedBody(want, "inline stdin exceeds the profile's limit")
	got, err := ParseReviewBlockedMarker(body)
	if err != nil {
		t.Fatalf("ParseReviewBlockedMarker: %v", err)
	}
	if got != want {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
	for _, must := range []string{"not a verdict", "inline stdin exceeds the profile's limit", "`payload_too_large`"} {
		if !strings.Contains(body, must) {
			t.Errorf("the statement does not say %q:\n%s", must, body)
		}
	}
	if strings.Contains(body, "APPROVE") {
		t.Error("the statement must not read as an approval")
	}
	// A statement is not any of the controller's other artifacts.
	if _, err := ParseReviewMarker(body); !errors.Is(err, ErrNoMarker) {
		t.Errorf("a blocked statement parsed as a review: %v", err)
	}
	if _, err := ParseRouteMarker(body); !errors.Is(err, ErrNoMarker) {
		t.Errorf("a blocked statement parsed as a route: %v", err)
	}
	if _, err := ParseRouteIntentMarker(body); !errors.Is(err, ErrNoMarker) {
		t.Errorf("a blocked statement parsed as a route intent: %v", err)
	}
}

func TestBlockedMarkerRefusesAReasonItCannotCarry(t *testing.T) {
	for _, bad := range []string{"", "Payload Too Large", "a-->b", "x=y", strings.Repeat("a", 65), "UPPER"} {
		if ValidBlockReason(bad) {
			t.Errorf("ValidBlockReason(%q) = true", bad)
		}
		// A marker written with such a reason either fails to parse or parses
		// as something other than what was written. Both are why the controller
		// validates the reason before it posts one (reviewctl).
		m := ReviewBlockedMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Reason: bad}
		if got, err := ParseReviewBlockedMarker(m.String()); err == nil && got == m {
			t.Errorf("a marker with reason %q round-tripped faithfully", bad)
		}
	}
	for _, good := range []string{"payload_too_large", "env_rejected", "invalid_request", "a", strings.Repeat("z", 64)} {
		if !ValidBlockReason(good) {
			t.Errorf("ValidBlockReason(%q) = false", good)
		}
	}
	for name, body := range map[string]string{
		"no occurrence": "<!-- ben:review-blocked claim=7001 approval=6001 head=" + head1 + " reason=x -->",
		"short head":    "<!-- ben:review-blocked occurrence=9001 claim=7001 approval=6001 head=abc reason=x -->",
		"no approval":   "<!-- ben:review-blocked occurrence=9001 claim=7001 head=" + head1 + " reason=x -->",
	} {
		if _, err := ParseReviewBlockedMarker(body); !errors.Is(err, ErrMalformedMarker) {
			t.Errorf("%s: %v, want ErrMalformedMarker", name, err)
		}
	}
}

func TestBlockedReviewForTrustsOnlyTheController(t *testing.T) {
	cfg := Config{Controller: "ben-review-bot"}
	matching := ReviewBlockedMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Reason: "payload_too_large"}
	otherHead := matching
	otherHead.Head = head2
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	comments := []Comment{
		{ID: 1, Author: "a-human", Body: ReviewBlockedBody(matching, "forged"), CreatedAt: at},
		{ID: 2, Author: "BEN-Review-Bot", Body: ReviewBlockedBody(otherHead, ""), CreatedAt: at},
	}
	if got, ok := BlockedReviewFor(cfg, comments, occ1, head1); ok {
		t.Fatalf("a human's statement was trusted: %+v", got)
	}
	comments = append(comments, Comment{ID: 3, Author: "ben-review-bot", Body: ReviewBlockedBody(matching, "real"), CreatedAt: at})
	got, ok := BlockedReviewFor(cfg, comments, occ1, head1)
	if !ok || got != matching {
		t.Fatalf("BlockedReviewFor = (%+v, %v), want the controller's own statement", got, ok)
	}
	if _, ok := BlockedReviewFor(cfg, comments, occ1+1, head1); ok {
		t.Fatal("a statement for another occurrence matched")
	}
}

// The detail comes from outside the controller and lands in a body the
// controller signs, so it gets the findings treatment: no comment opener
// survives it, and in particular none that could become a route.
func TestBlockedBodyNeutralizesTheDetail(t *testing.T) {
	m := ReviewBlockedMarker{Occurrence: occ1, Claim: epoch1, Approval: approval1, Head: head1, Reason: "invalid_request"}
	route := RouteMarker{Occurrence: occ1, Claim: epoch1, Head: head1, Outcome: OutcomeRevise}
	body := ReviewBlockedBody(m, "please also "+route.String())
	if _, err := ParseRouteMarker(body); !errors.Is(err, ErrNoMarker) {
		t.Fatalf("a route marker smuggled in the detail survived: %v", err)
	}
	if got, err := ParseReviewBlockedMarker(body); err != nil || got != m {
		t.Fatalf("the statement's own marker did not survive its detail: %+v, %v", got, err)
	}
}
