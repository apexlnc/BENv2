package ticketprep

import "fmt"

type sectionDefinition struct {
	name    string
	binding SectionBinding
}

var sectionDefinitions = []sectionDefinition{
	{"restated_outcome", BindingSubject},
	{"candidate_non_goals", BindingSubject},
	{"assumptions_to_confirm", BindingSubject},
	{"decision_queue", BindingSubject},
	{"applicable_constraints", BindingSubjectRepository},
	{"acceptance_gaps", BindingSubjectRepository},
	{"proposed_acceptance_tests", BindingSubjectRepository},
	{"affected_area_hypotheses", BindingSubjectRepository},
	{"decision_decomposition", BindingSubjectRepository},
	{"candidate_delivery_splits", BindingSubjectRepository},
	{"reasons", BindingSubjectRepository},
	{"recommendation", BindingSubjectRepository},
}

// Freshness compares immutable bindings, not prose. Subject movement stales
// every advisory section; commit or tree movement stales only sections whose
// claims depend on repository observations.
func Freshness(packet Packet, current Capture) (FreshnessReport, error) {
	if err := packet.Validate(); err != nil {
		return FreshnessReport{}, err
	}
	if err := current.Validate(); err != nil {
		return FreshnessReport{}, err
	}
	digest, err := PacketDigest(packet)
	if err != nil {
		return FreshnessReport{}, err
	}
	bound := packet.Capture
	subjectMatches := bound.Subject.RepositoryIdentity == current.Subject.RepositoryIdentity &&
		bound.Subject.IssueNumber == current.Subject.IssueNumber &&
		bound.Subject.IssueURL == current.Subject.IssueURL &&
		bound.Subject.ContentDigest == current.Subject.ContentDigest
	repositoryMatches := bound.Repository.Identity == current.Repository.Identity &&
		bound.Repository.Commit == current.Repository.Commit &&
		bound.Repository.Tree == current.Repository.Tree
	report := FreshnessReport{
		SchemaVersion:     SchemaVersion,
		PacketDigest:      digest,
		SubjectMatches:    subjectMatches,
		RepositoryMatches: repositoryMatches,
	}
	for _, definition := range sectionDefinitions {
		status, reason := FreshnessMatchesCapture, "packet binding matches the supplied comparison capture"
		switch {
		case !subjectMatches:
			status, reason = FreshnessStale, "issue identity or exact title/body content changed"
		case definition.binding == BindingSubjectRepository && !repositoryMatches:
			status, reason = FreshnessStale, "repository identity, commit, or tree changed"
		case definition.binding == BindingSubject:
			reason = "packet subject matches the supplied comparison capture"
		}
		report.Sections = append(report.Sections, SectionFreshness{
			Section: definition.name,
			Binding: definition.binding,
			Status:  status,
			Reason:  reason,
		})
	}
	return report, nil
}

func freshnessFor(report FreshnessReport, section string) (SectionFreshness, error) {
	for _, got := range report.Sections {
		if got.Section == section {
			return got, nil
		}
	}
	return SectionFreshness{}, fmt.Errorf("ticketprep: no freshness binding for section %q", section)
}
