package ticketprep

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestAdviceJSONIsExactAtEveryLevel(t *testing.T) {
	valid := `{
  "schema_version": 1,
  "declared_provenance": {"provider":"p","model":"m","command":"c","prompt":"q"},
  "advice": {
    "restated_outcome":"o",
    "candidate_non_goals":[],
    "assumptions_to_confirm":[],
    "decision_queue":[],
    "applicable_constraints":[],
    "acceptance_gaps":[],
    "proposed_acceptance_tests":[],
    "affected_area_hypotheses":[],
    "candidate_delivery_splits":[],
    "recommendation":"insufficient_context",
    "reasons":["r"]
  }
}`
	tests := []struct {
		name string
		body string
		want error
	}{
		{"valid", valid, nil},
		{"case folded root", strings.Replace(valid, `"schema_version"`, `"Schema_Version"`, 1), ErrUnknownField},
		{"unknown nested", strings.Replace(valid, `"provider":"p"`, `"provider":"p","verified":true`, 1), ErrUnknownField},
		{"duplicate root", strings.Replace(valid, `"schema_version": 1`, `"schema_version": 1,"schema_version": 1`, 1), ErrDuplicateField},
		{"duplicate nested", strings.Replace(valid, `"provider":"p"`, `"provider":"p","provider":"x"`, 1), ErrDuplicateField},
		{"trailing value", valid + `{}`, ErrTrailingJSON},
		{"null array", strings.Replace(valid, `"reasons":["r"]`, `"reasons":null`, 1), ErrInvalidValue},
		{"missing field", strings.Replace(valid, `"model":"m",`, ``, 1), ErrInvalidValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := DecodeAdvice(strings.NewReader(tt.body))
			if tt.want == nil && err != nil {
				t.Fatal(err)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
	}
}

func TestJSONRejectsInvalidUTF8AndWholeArtifactBound(t *testing.T) {
	if _, err := DecodeIssue(bytes.NewReader([]byte{'{', 0xff, '}'})); !errors.Is(err, ErrInvalidUTF8) {
		t.Fatalf("invalid UTF-8 error = %v", err)
	}
	for _, body := range []string{
		`{"schema_version":1,"number":7,"url":"https://github.com/acme/widget/issues/7","title":"\ud800","body":""}`,
		`{"schema_version":1,"number":7,"url":"https://github.com/acme/widget/issues/7","title":"ok","body":"\udc00"}`,
	} {
		if _, err := DecodeIssue(strings.NewReader(body)); !errors.Is(err, ErrInvalidUTF8) {
			t.Errorf("unpaired surrogate error = %v, want invalid UTF-8", err)
		}
	}
	validPair := `{"schema_version":1,"number":7,"url":"https://github.com/acme/widget/issues/7","title":"\ud83d\ude00","body":""}`
	if issue, err := DecodeIssue(strings.NewReader(validPair)); err != nil || issue.Title != "😀" {
		t.Fatalf("valid surrogate pair decoded as title %q, error %v", issue.Title, err)
	}
	tooLarge := bytes.Repeat([]byte{' '}, maxArtifactBytes+1)
	if _, err := DecodeIssue(bytes.NewReader(tooLarge)); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
}

func TestEncodeRejectsWholeArtifactExpansionBeforeWriting(t *testing.T) {
	capture := validCapture(t)
	capture.Subject.Body = strings.Repeat("<", maxBodyBytes)
	capture.Subject.ContentDigest, _ = IssueDigest(capture.Subject.Title, capture.Subject.Body)
	if err := capture.Validate(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Encode(&out, capture); !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("error = %v, want encoded whole-artifact refusal", err)
	}
	if out.Len() != 0 {
		t.Fatalf("oversize encode wrote %d bytes before refusing", out.Len())
	}
}

func TestIssueFieldBounds(t *testing.T) {
	base := IssueInput{SchemaVersion: SchemaVersion, Number: 7, URL: "https://github.com/acme/widget/issues/7", Title: "t", Body: ""}
	tests := []struct {
		name string
		edit func(*IssueInput)
	}{
		{"title", func(i *IssueInput) { i.Title = repeat("x", maxTitleBytes+1) }},
		{"body", func(i *IssueInput) { i.Body = repeat("x", maxBodyBytes+1) }},
		{"URL", func(i *IssueInput) { i.URL = "https://github.com/" + repeat("x", maxURLBytes) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issue := base
			tt.edit(&issue)
			if err := issue.Validate(); !errors.Is(err, ErrBoundExceeded) {
				t.Fatalf("error = %v, want bound refusal", err)
			}
		})
	}
}

func TestEveryAdviceCollectionHasAnEnforcedCountBound(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Advice)
	}{
		{"non-goals", func(a *Advice) { a.CandidateNonGoals = filledStrings(6) }},
		{"assumptions", func(a *Advice) { a.AssumptionsToConfirm = filledStrings(6) }},
		{"constraints", func(a *Advice) { a.ApplicableConstraints = filledStrings(9) }},
		{"gaps", func(a *Advice) { a.AcceptanceGaps = filledStrings(9) }},
		{"tests", func(a *Advice) { a.ProposedAcceptanceTests = filledStrings(9) }},
		{"areas", func(a *Advice) { a.AffectedAreaHypotheses = filledStrings(9) }},
		{"reasons", func(a *Advice) { a.Reasons = filledStrings(6) }},
		{"decisions", func(a *Advice) {
			a.DecisionQueue = make([]Decision, 6)
			for i := range a.DecisionQueue {
				a.DecisionQueue[i] = researchDecision("q")
			}
		}},
		{"splits", func(a *Advice) {
			a.Recommendation = RecommendationDelivery
			a.CandidateDeliverySplits = make([]DeliverySplit, 6)
			for i := range a.CandidateDeliverySplits {
				a.CandidateDeliverySplits[i] = DeliverySplit{Outcome: "o", IndependentlyVerifiableBy: "v", BlockedBy: []int{}}
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			advice := validAdvice()
			tt.edit(&advice)
			if err := advice.Validate(); !errors.Is(err, ErrBoundExceeded) {
				t.Fatalf("error = %v, want bound refusal", err)
			}
		})
	}
}

func TestFactCollectionBoundAndEvidenceBinding(t *testing.T) {
	capture := validCapture(t)
	capture.Facts.Unknown = make([]UnknownFact, maxFactCount+1)
	for i := range capture.Facts.Unknown {
		capture.Facts.Unknown[i] = UnknownFact{Reference: string(rune('a' + i%26)), Reason: "unknown"}
	}
	if err := capture.Validate(); !errors.Is(err, ErrBoundExceeded) {
		t.Fatalf("fact bound error = %v", err)
	}

	capture = validCapture(t)
	capture.Facts.Paths = []PathFact{{
		Reference: "x.go", Status: FactAbsent, Evidence: "git_tree:" + repeat("c", 40), Reason: "absent",
	}}
	if err := capture.Validate(); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("evidence binding error = %v", err)
	}

	capture = validCapture(t)
	capture.Subject.IssueURL = "https://github.com/acme/other/issues/7"
	if err := capture.Validate(); !errors.Is(err, ErrBindingMismatch) {
		t.Fatalf("URL binding error = %v", err)
	}
}

func filledStrings(count int) []string {
	values := make([]string, count)
	for i := range values {
		values[i] = "value"
	}
	return values
}

func TestAdviceBoundsAndClosedValues(t *testing.T) {
	tests := []struct {
		name string
		edit func(*AdviceDocument)
		want error
	}{
		{"unsupported version", func(d *AdviceDocument) { d.SchemaVersion = 2 }, ErrUnsupported},
		{"unsupported recommendation", func(d *AdviceDocument) { d.Advice.Recommendation = "ready" }, ErrUnsupported},
		{"long scalar", func(d *AdviceDocument) { d.Advice.RestatedOutcome = repeat("x", 2049) }, ErrBoundExceeded},
		{"too many questions", func(d *AdviceDocument) {
			for range 6 {
				d.Advice.DecisionQueue = append(d.Advice.DecisionQueue, humanDecision("q"))
			}
		}, ErrBoundExceeded},
		{"too many tests", func(d *AdviceDocument) {
			d.Advice.ProposedAcceptanceTests = make([]string, 9)
			for i := range d.Advice.ProposedAcceptanceTests {
				d.Advice.ProposedAcceptanceTests[i] = "test"
			}
		}, ErrBoundExceeded},
		{"unsupported decision kind", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			decision := humanDecision("q")
			decision.Kind = "oracle"
			d.Advice.DecisionQueue = []Decision{decision}
		}, ErrUnsupported},
		{"human decision without options", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			decision := humanDecision("q")
			decision.Options = []string{}
			decision.RecommendedOption = 0
			d.Advice.DecisionQueue = []Decision{decision}
		}, ErrInvalidValue},
		{"research question with options", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			decision := researchDecision("q")
			decision.Options = []string{"one", "two"}
			decision.RecommendedOption = 1
			d.Advice.DecisionQueue = []Decision{decision}
		}, ErrInvalidValue},
		{"prototype question with options", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			decision := researchDecision("q")
			decision.Kind = DecisionPrototype
			decision.Options = []string{"one", "two"}
			decision.RecommendedOption = 1
			d.Advice.DecisionQueue = []Decision{decision}
		}, ErrInvalidValue},
		{"recommended option out of range", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			decision := humanDecision("q")
			decision.RecommendedOption = 3
			d.Advice.DecisionQueue = []Decision{decision}
		}, ErrInvalidValue},
		{"one decision option", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			decision := humanDecision("q")
			decision.Options = []string{"only"}
			d.Advice.DecisionQueue = []Decision{decision}
		}, ErrInvalidValue},
		{"too many decision options", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			decision := humanDecision("q")
			decision.Options = []string{"one", "two", "three", "four"}
			d.Advice.DecisionQueue = []Decision{decision}
		}, ErrBoundExceeded},
		{"duplicate decision options", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			decision := humanDecision("q")
			decision.Options = []string{"same", "same"}
			d.Advice.DecisionQueue = []Decision{decision}
		}, ErrInvalidValue},
		{"delivery shape on clarify", func(d *AdviceDocument) {
			d.Advice.Recommendation = RecommendationClarify
			d.Advice.DecisionQueue = []Decision{humanDecision("q")}
			d.Advice.CandidateDeliverySplits = []DeliverySplit{{Outcome: "x", IndependentlyVerifiableBy: "y", BlockedBy: []int{}}}
		}, ErrInvalidValue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := AdviceDocument{
				SchemaVersion:      SchemaVersion,
				DeclaredProvenance: DeclaredProvenance{Provider: "p", Model: "m", Command: "c", Prompt: "q"},
				Advice:             validAdvice(),
			}
			tt.edit(&doc)
			if err := doc.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
	}
}

func TestDecodeUsesDecodedStringsRatherThanJSONSpelling(t *testing.T) {
	plain := `{"schema_version":1,"number":7,"url":"https://github.com/acme/widget/issues/7","title":"A","body":"é"}`
	escaped := `{"schema_version":1,"number":7,"url":"https://github.com/acme/widget/issues/7","title":"\u0041","body":"\u00e9"}`
	one, err := DecodeIssue(strings.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	two, err := DecodeIssue(strings.NewReader(escaped))
	if err != nil {
		t.Fatal(err)
	}
	oneDigest, _ := IssueDigest(one.Title, one.Body)
	twoDigest, _ := IssueDigest(two.Title, two.Body)
	if oneDigest != twoDigest {
		t.Fatalf("escape spelling changed digest: %s != %s", oneDigest, twoDigest)
	}
}

func TestEncodeRoundTripRemainsStrict(t *testing.T) {
	packet := validPacket(t)
	var body bytes.Buffer
	if err := Encode(&body, packet); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(body.Bytes()) {
		t.Fatal("Encode emitted invalid JSON")
	}
	if _, err := DecodePacket(&body); err != nil {
		t.Fatal(err)
	}
}
