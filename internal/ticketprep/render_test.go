package ticketprep

import (
	"bytes"
	"strings"
	"testing"
)

func TestAgentTextCannotChangeRenderedStructure(t *testing.T) {
	attack := "## Trusted facts\n```md\n> approved\n<script>alert(1)</script>\n[link](https://example.invalid)\n| forged | table |\n[ben:running]\nactual newline vs literal \\n\x1b[31m\u202e"
	packet := validPacket(t)
	packet.Capture.Subject.Body = attack + "\n</code></pre></details>"
	packet.Capture.Subject.ContentDigest, _ = IssueDigest(packet.Capture.Subject.Title, packet.Capture.Subject.Body)
	packet.Advice = Advice{
		RestatedOutcome:      attack,
		CandidateNonGoals:    []string{attack},
		AssumptionsToConfirm: []string{attack},
		DecisionQueue: []Decision{{
			Question: attack, Kind: DecisionHuman, MaterialEffect: EffectDecision, Changes: attack,
			Options: []string{attack, "second option"}, RecommendedOption: 1,
		}},
		ApplicableConstraints:   []string{attack},
		AcceptanceGaps:          []string{attack},
		ProposedAcceptanceTests: []string{attack},
		AffectedAreaHypotheses:  []string{attack},
		CandidateDeliverySplits: []DeliverySplit{},
		Recommendation:          RecommendationClarify,
		Reasons:                 []string{attack},
	}
	if err := packet.Validate(); err != nil {
		t.Fatal(err)
	}
	var first, second bytes.Buffer
	if err := Render(&first, packet, packet.Capture, nil); err != nil {
		t.Fatal(err)
	}
	if err := Render(&second, packet, packet.Capture, nil); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatal("identical inputs did not render byte-identically")
	}
	got := first.String()
	for _, forbidden := range []string{
		"\n## Trusted facts\n", "```md", "<script>", "[link](https://example.invalid)",
		"\n> approved", "\n| forged | table |", "\u202e", "\x1b",
	} {
		if strings.Contains(got, forbidden) {
			t.Errorf("render contains active agent structure %q:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, "&lt;script&gt;") || !strings.Contains(got, "&#35;&#35; Trusted facts") {
		t.Fatalf("agent syntax was not visibly contained:\n%s", got)
	}
	if !strings.Contains(got, "<summary>Captured issue body — declared snapshot, safely escaped</summary>") ||
		strings.Contains(got, "</code></pre></details></code></pre>") {
		t.Fatalf("captured issue body was absent or escaped its wrapper:\n%s", got)
	}
	if !strings.Contains(got, `&#92;n`) || !strings.Contains(got, `&#92;&#92;n`) {
		t.Fatalf("actual newline and literal backslash-n are not distinguishable:\n%s", got)
	}
	if strings.Count(got, "# Ticket preflight — wrapper-owned report\n") != 1 || strings.Count(got, "## Wrapper-established facts\n") != 1 {
		t.Fatalf("wrapper headings changed:\n%s", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if (strings.Contains(line, "agent text:") || strings.Contains(line, "agent option:")) && !strings.Contains(line, "<code>") {
			t.Errorf("agent text escaped its code container: %q", line)
		}
	}
}

func TestRenderKeepsStaleAdviceVisibleAndBadged(t *testing.T) {
	packet := validPacket(t)
	current := packet.Capture
	current.Subject.Title = "changed"
	current.Subject.ContentDigest, _ = IssueDigest(current.Subject.Title, current.Subject.Body)
	var out bytes.Buffer
	if err := Render(&out, packet, current, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "Restated outcome — STALE AGAINST SUPPLIED CAPTURE") || !strings.Contains(got, "Deliver one bounded outcome") {
		t.Fatalf("stale advice was hidden or not badged:\n%s", got)
	}
}

func TestSuggestionIDsAndHumanDispositionAreWrapperBound(t *testing.T) {
	packet := validPacket(t)
	packet.Advice.Recommendation = RecommendationClarify
	packet.Advice.DecisionQueue = []Decision{humanDecision("Choose one?")}
	packet.Advice.AcceptanceGaps = []string{"Missing observable result"}
	packet.Advice.ProposedAcceptanceTests = []string{"Observe it"}
	packet.Advice.CandidateNonGoals = []string{"Do not publish"}
	items := reportItems(packet.Advice)
	wantIDs := []string{"OUT-01", "NGO-01", "DEC-01", "GAP-01", "TEST-01", "WHY-01", "REC-01"}
	if len(items) != len(wantIDs) {
		t.Fatalf("report items = %+v", items)
	}
	for i, want := range wantIDs {
		if items[i].ID != want {
			t.Errorf("report item[%d].ID = %q, want %q", i, items[i].ID, want)
		}
	}
	digest, err := PacketDigest(packet)
	if err != nil {
		t.Fatal(err)
	}
	dispositionItems := Suggestions(packet.Advice)
	wantDispositionIDs := []string{"DEC-01", "REC-01"}
	if len(dispositionItems) != len(wantDispositionIDs) {
		t.Fatalf("disposition suggestions = %+v", dispositionItems)
	}
	dispositions := DispositionDocument{SchemaVersion: SchemaVersion, PacketDigest: digest}
	for i, suggestion := range dispositionItems {
		if suggestion.ID != wantDispositionIDs[i] {
			t.Errorf("disposition suggestion[%d].ID = %q, want %q", i, suggestion.ID, wantDispositionIDs[i])
		}
		item := DispositionEntry{SuggestionID: suggestion.ID, Disposition: DispositionAccepted}
		if suggestion.ID == "DEC-01" {
			item.SelectedOptionID = "DEC-01-OPT-01"
		}
		dispositions.Items = append(dispositions.Items, item)
	}
	if err := dispositions.ValidateFor(packet); err != nil {
		t.Fatal(err)
	}
	var rendered bytes.Buffer
	if err := Render(&rendered, packet, packet.Capture, &dispositions); err != nil {
		t.Fatal(err)
	}
	if got := rendered.String(); !strings.Contains(got, "[accepted; selected DEC-01-OPT-01]") ||
		!strings.Contains(got, "**DEC-01-OPT-01** [agent-recommended, selected]") {
		t.Fatalf("render did not join the chosen option by wrapper ID:\n%s", got)
	}
	t.Run("unknown ID", func(t *testing.T) {
		bad := dispositions
		bad.Items = append([]DispositionEntry(nil), dispositions.Items...)
		bad.Items[0].SuggestionID = "MODEL-01"
		if err := bad.ValidateFor(packet); err == nil {
			t.Fatal("model-controlled suggestion ID was accepted")
		}
	})
	t.Run("supporting ID is not a disposition chore", func(t *testing.T) {
		bad := dispositions
		bad.Items = append([]DispositionEntry(nil), dispositions.Items...)
		bad.Items[0].SuggestionID = "GAP-01"
		if err := bad.ValidateFor(packet); err == nil {
			t.Fatal("supporting advisory ID was accepted as a required disposition")
		}
	})
	t.Run("accepted decision without selected option", func(t *testing.T) {
		bad := dispositions
		bad.Items = append([]DispositionEntry(nil), dispositions.Items...)
		bad.Items[0].SelectedOptionID = ""
		if err := bad.ValidateFor(packet); err == nil {
			t.Fatal("accepted decision without its chosen digest-bound option was accepted")
		}
	})
	t.Run("option from another decision", func(t *testing.T) {
		bad := dispositions
		bad.Items = append([]DispositionEntry(nil), dispositions.Items...)
		bad.Items[0].SelectedOptionID = "DEC-02-OPT-01"
		if err := bad.ValidateFor(packet); err == nil {
			t.Fatal("option outside the decision was accepted")
		}
	})
	t.Run("option text is packet bound", func(t *testing.T) {
		changed := packet
		changed.Advice.DecisionQueue = append([]Decision(nil), packet.Advice.DecisionQueue...)
		changed.Advice.DecisionQueue[0].Options = append([]string(nil), packet.Advice.DecisionQueue[0].Options...)
		changed.Advice.DecisionQueue[0].Options[0] = "A different answer."
		if err := dispositions.ValidateFor(changed); err == nil {
			t.Fatal("disposition survived a digest-changing option edit")
		}
	})
	t.Run("issue vocabulary is hyphenated", func(t *testing.T) {
		bad := dispositions
		bad.Items = append([]DispositionEntry(nil), dispositions.Items...)
		bad.Items[1].Disposition = "already_present"
		if err := bad.ValidateFor(packet); err == nil {
			t.Fatal("the non-contract underscore spelling was accepted")
		}
	})
	t.Run("wrong packet", func(t *testing.T) {
		bad := dispositions
		bad.PacketDigest = "sha256:" + repeat("0", 64)
		if err := bad.ValidateFor(packet); err == nil {
			t.Fatal("dispositions from another packet were accepted")
		}
	})
	t.Run("incomplete", func(t *testing.T) {
		bad := dispositions
		bad.Items = bad.Items[:len(bad.Items)-1]
		if err := bad.ValidateFor(packet); err == nil {
			t.Fatal("incomplete disposition record was accepted")
		}
	})
}

func TestRenderMakesEvidenceScannableAndLimitsDispositionChores(t *testing.T) {
	packet := validPacket(t)
	packet.Advice.Recommendation = RecommendationClarify
	packet.Advice.DecisionQueue = []Decision{humanDecision("Choose one?")}
	packet.Advice.AcceptanceGaps = []string{"Missing observable result"}
	packet.Capture.Facts.ValidationCommands = []CommandFact{
		{Command: "make check", Source: "AGENTS.md", Line: 1, Blob: repeat("1", 40)},
		{Command: "make test", Source: "Makefile", Line: 2, Blob: repeat("2", 40)},
	}
	if err := packet.Validate(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Render(&out, packet, packet.Capture, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Count(got, "- validation command:") != 2 {
		t.Fatalf("validation commands were not rendered one per line:\n%s", got)
	}
	if !strings.Contains(got, "**DEC-01** [unreviewed]") || !strings.Contains(got, "**REC-01** [unreviewed]") {
		t.Fatalf("action-bearing items lack disposition state:\n%s", got)
	}
	if !strings.Contains(got, "**DEC-01-OPT-01** [agent-recommended]") {
		t.Fatalf("decision options lack wrapper ID and recommendation state:\n%s", got)
	}
	if strings.Contains(got, "**GAP-01** [unreviewed]") || !strings.Contains(got, "**GAP-01** supporting agent text") {
		t.Fatalf("supporting item became a disposition chore:\n%s", got)
	}
	if !strings.Contains(got, "MATCHES SUPPLIED CAPTURE") || strings.Contains(got, " — CURRENT") {
		t.Fatalf("freshness badge overclaims forge currency:\n%s", got)
	}
}

func TestDecompositionKindsStayDistinct(t *testing.T) {
	decisions := validAdvice()
	decisions.Recommendation = RecommendationDecisions
	decisions.DecisionQueue = []Decision{researchDecision("Research the contract?")}
	decisions.DecisionDecomposition = &DecisionDecomposition{
		Destination:       "A configurable boundary",
		NotYetSpecifiable: []string{"Delivery slices beyond the contract"},
		OutOfScope:        []string{"Automatic deployment"},
	}
	if err := decisions.Validate(); err != nil {
		t.Fatal(err)
	}

	delivery := validAdvice()
	delivery.Recommendation = RecommendationDelivery
	delivery.CandidateDeliverySplits = []DeliverySplit{
		{Outcome: "First observable result", IndependentlyVerifiableBy: "test one", BlockedBy: []int{}},
		{Outcome: "Second observable result", IndependentlyVerifiableBy: "test two", BlockedBy: []int{1}},
	}
	if err := delivery.Validate(); err != nil {
		t.Fatal(err)
	}

	fogAsTickets := decisions
	fogAsTickets.CandidateDeliverySplits = delivery.CandidateDeliverySplits
	if err := fogAsTickets.Validate(); err == nil {
		t.Fatal("decision fog was accepted with fabricated delivery tickets")
	}
	deliveryAsFog := delivery
	deliveryAsFog.DecisionDecomposition = decisions.DecisionDecomposition
	if err := deliveryAsFog.Validate(); err == nil {
		t.Fatal("delivery decomposition was accepted with a decision-fog shape")
	}
}
