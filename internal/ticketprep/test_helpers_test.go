package ticketprep

import "testing"

func validCapture(t *testing.T) Capture {
	t.Helper()
	digest, err := IssueDigest("A ticket", "A body")
	if err != nil {
		t.Fatal(err)
	}
	return Capture{
		SchemaVersion: SchemaVersion,
		KernelVersion: KernelVersion,
		Subject: Subject{
			RepositoryIdentity: "github.com/acme/widget",
			IssueNumber:        7,
			IssueURL:           "https://github.com/acme/widget/issues/7",
			Title:              "A ticket",
			Body:               "A body",
			ContentDigest:      digest,
		},
		Repository: Repository{
			Remote:            "origin",
			Identity:          "github.com/acme/widget",
			RemoteFingerprint: "sha256:" + repeat("1", 64),
			Commit:            repeat("a", 40),
			Tree:              repeat("b", 40),
		},
		Facts: Facts{
			Paths:              []PathFact{},
			Symbols:            []SymbolFact{},
			InstructionFiles:   []InstructionFact{},
			ValidationCommands: []CommandFact{},
			Unknown:            []UnknownFact{},
		},
		Sources: FactSources{
			Issue:      "declared_issue_snapshot",
			Repository: "git_object_database",
			Remote:     "git_config:remote.origin.url",
		},
	}
}

func validAdvice() Advice {
	return Advice{
		RestatedOutcome:         "Deliver one bounded outcome.",
		CandidateNonGoals:       []string{},
		AssumptionsToConfirm:    []string{},
		DecisionQueue:           []Decision{},
		ApplicableConstraints:   []string{},
		AcceptanceGaps:          []string{},
		ProposedAcceptanceTests: []string{},
		AffectedAreaHypotheses:  []string{},
		CandidateDeliverySplits: []DeliverySplit{},
		Recommendation:          RecommendationInsufficient,
		Reasons:                 []string{"The ticket omits its observable completion fact."},
	}
}

func humanDecision(question string) Decision {
	return Decision{
		Question:          question,
		Kind:              DecisionHuman,
		MaterialEffect:    EffectDecision,
		Changes:           "the contract",
		Options:           []string{"Choose option one.", "Choose option two."},
		RecommendedOption: 1,
	}
}

func researchDecision(question string) Decision {
	return Decision{
		Question:       question,
		Kind:           DecisionResearch,
		MaterialEffect: EffectDecision,
		Changes:        "the next specifiable work",
		Options:        []string{},
	}
}

func validPacket(t *testing.T) Packet {
	t.Helper()
	return Packet{
		SchemaVersion: SchemaVersion,
		KernelVersion: KernelVersion,
		Capture:       validCapture(t),
		DeclaredProvenance: DeclaredProvenance{
			Provider: "openai",
			Model:    "declared-model",
			Command:  "$prep-ticket",
			Prompt:   "one-shot review",
		},
		Advice: validAdvice(),
	}
}

func repeat(value string, n int) string {
	out := ""
	for range n {
		out += value
	}
	return out
}
