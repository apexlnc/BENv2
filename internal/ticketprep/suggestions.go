package ticketprep

import (
	"fmt"
	"strings"
)

// reportItems is the only assignment site for human-facing IDs. It derives
// them from validated schema order; no advisory input field can choose an ID.
// Most are stable supporting references. Suggestions selects the action-bearing
// subset that asks the human to choose or authorize something.
func reportItems(advice Advice) []Suggestion {
	var out []Suggestion
	add := func(id, section string, binding SectionBinding, text string) {
		out = append(out, Suggestion{ID: id, Section: section, Binding: binding, Text: text})
	}
	add("OUT-01", "restated_outcome", BindingSubject, advice.RestatedOutcome)
	for i, text := range advice.CandidateNonGoals {
		add(numbered("NGO", i), "candidate_non_goals", BindingSubject, text)
	}
	for i, text := range advice.AssumptionsToConfirm {
		add(numbered("ASM", i), "assumptions_to_confirm", BindingSubject, text)
	}
	for i, decision := range advice.DecisionQueue {
		add(numbered("DEC", i), "decision_queue", BindingSubject,
			fmt.Sprintf("%s [kind: %s; changes %s: %s]", decision.Question, decision.Kind, decision.MaterialEffect, decision.Changes))
	}
	for i, text := range advice.ApplicableConstraints {
		add(numbered("CON", i), "applicable_constraints", BindingSubjectRepository, text)
	}
	for i, text := range advice.AcceptanceGaps {
		add(numbered("GAP", i), "acceptance_gaps", BindingSubjectRepository, text)
	}
	for i, text := range advice.ProposedAcceptanceTests {
		add(numbered("TEST", i), "proposed_acceptance_tests", BindingSubjectRepository, text)
	}
	for i, text := range advice.AffectedAreaHypotheses {
		add(numbered("AREA", i), "affected_area_hypotheses", BindingSubjectRepository, text)
	}
	if advice.DecisionDecomposition != nil {
		add("DEST-01", "decision_decomposition", BindingSubjectRepository, advice.DecisionDecomposition.Destination)
		for i, text := range advice.DecisionDecomposition.NotYetSpecifiable {
			add(numbered("FOG", i), "decision_decomposition", BindingSubjectRepository, "not yet specifiable: "+text)
		}
		for i, text := range advice.DecisionDecomposition.OutOfScope {
			add(numbered("OOS", i), "decision_decomposition", BindingSubjectRepository, "out of scope: "+text)
		}
	}
	for i, split := range advice.CandidateDeliverySplits {
		blockers := make([]string, 0, len(split.BlockedBy))
		for _, blockedBy := range split.BlockedBy {
			blockers = append(blockers, numbered("SPLIT", blockedBy-1))
		}
		blocked := "none"
		if len(blockers) > 0 {
			blocked = strings.Join(blockers, ", ")
		}
		add(numbered("SPLIT", i), "candidate_delivery_splits", BindingSubjectRepository,
			fmt.Sprintf("%s [independently verifiable by: %s; blocked by: %s]", split.Outcome, split.IndependentlyVerifiableBy, blocked))
	}
	for i, text := range advice.Reasons {
		add(numbered("WHY", i), "reasons", BindingSubjectRepository, text)
	}
	add("REC-01", "recommendation", BindingSubjectRepository, string(advice.Recommendation))
	return out
}

// Suggestions returns only advice that asks the human to choose or authorize a
// route. Every suggestion requires a disposition. The other wrapper IDs remain
// stable references supporting those choices, rather than suggestions of their
// own. The schema makes decisions and delivery splits mutually exclusive, so
// this set is bounded at five frontier items plus the recommendation.
func Suggestions(advice Advice) []Suggestion {
	var out []Suggestion
	for _, suggestion := range reportItems(advice) {
		switch suggestion.Section {
		case "decision_queue", "candidate_delivery_splits", "recommendation":
			out = append(out, suggestion)
		}
	}
	return out
}

func numbered(prefix string, zeroBased int) string {
	return fmt.Sprintf("%s-%02d", prefix, zeroBased+1)
}

func decisionOptionID(decisionZeroBased, optionZeroBased int) string {
	return fmt.Sprintf("%s-OPT-%02d", numbered("DEC", decisionZeroBased), optionZeroBased+1)
}
