package ticketprep

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	objectIDPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

func (i IssueInput) Validate() error {
	if err := version("issue.schema_version", i.SchemaVersion); err != nil {
		return err
	}
	if i.Number <= 0 {
		return fmt.Errorf("%w: issue.number must be positive", ErrInvalidValue)
	}
	if err := boundedString("issue.url", i.URL, maxURLBytes, false); err != nil {
		return err
	}
	if err := boundedString("issue.title", i.Title, maxTitleBytes, false); err != nil {
		return err
	}
	if err := boundedString("issue.body", i.Body, maxBodyBytes, true); err != nil {
		return err
	}
	parsed, err := url.Parse(i.URL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return fmt.Errorf("%w: issue.url must be an absolute HTTPS issue URL", ErrInvalidValue)
	}
	parts := splitPath(parsed.Path)
	if len(parts) != 4 || parts[2] != "issues" || parts[3] != strconv.Itoa(i.Number) || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%w: issue.url does not identify issue %d", ErrInvalidValue, i.Number)
	}
	return nil
}

func (c Capture) Validate() error {
	if err := version("capture.schema_version", c.SchemaVersion); err != nil {
		return err
	}
	if c.KernelVersion != KernelVersion {
		return unsupported("capture.kernel_version", c.KernelVersion)
	}
	if err := boundedString("capture.subject.repository_identity", c.Subject.RepositoryIdentity, maxURLBytes, false); err != nil {
		return err
	}
	if c.Subject.RepositoryIdentity != c.Repository.Identity {
		return fmt.Errorf("%w: subject repository %q != observed repository %q", ErrBindingMismatch, c.Subject.RepositoryIdentity, c.Repository.Identity)
	}
	issueRepository, err := repositoryFromIssueURL(c.Subject.IssueURL)
	if err != nil {
		return err
	}
	if issueRepository != c.Subject.RepositoryIdentity {
		return fmt.Errorf("%w: issue URL repository %q != captured repository %q", ErrBindingMismatch, issueRepository, c.Subject.RepositoryIdentity)
	}
	issue := IssueInput{
		SchemaVersion: SchemaVersion,
		Number:        c.Subject.IssueNumber,
		URL:           c.Subject.IssueURL,
		Title:         c.Subject.Title,
		Body:          c.Subject.Body,
	}
	if err := issue.Validate(); err != nil {
		return fmt.Errorf("capture.subject: %w", err)
	}
	wantDigest, err := IssueDigest(c.Subject.Title, c.Subject.Body)
	if err != nil {
		return err
	}
	if c.Subject.ContentDigest != wantDigest {
		return fmt.Errorf("%w: content digest got %q, want %q", ErrBindingMismatch, c.Subject.ContentDigest, wantDigest)
	}
	if c.Repository.Remote != "origin" {
		return unsupported("capture.repository.remote", c.Repository.Remote)
	}
	if err := boundedString("capture.repository.identity", c.Repository.Identity, maxURLBytes, false); err != nil {
		return err
	}
	if !digestPattern.MatchString(c.Repository.RemoteFingerprint) {
		return fmt.Errorf("%w: capture.repository.remote_fingerprint", ErrInvalidValue)
	}
	if !objectIDPattern.MatchString(c.Repository.Commit) {
		return fmt.Errorf("%w: capture.repository.commit is not a full object ID", ErrInvalidValue)
	}
	if !objectIDPattern.MatchString(c.Repository.Tree) {
		return fmt.Errorf("%w: capture.repository.tree is not a full object ID", ErrInvalidValue)
	}
	if c.Sources != (FactSources{Issue: "declared_issue_snapshot", Repository: "git_object_database", Remote: "git_config:remote.origin.url"}) {
		return fmt.Errorf("%w: capture.sources contains an unsupported fact source", ErrUnsupported)
	}
	return c.Facts.validate(c.Repository)
}

func (f Facts) validate(repo Repository) error {
	if f.Paths == nil || f.Symbols == nil || f.InstructionFiles == nil || f.ValidationCommands == nil || f.Unknown == nil {
		return fmt.Errorf("%w: capture fact collections must be JSON arrays, not null", ErrInvalidValue)
	}
	for _, collection := range []struct {
		name  string
		count int
	}{
		{"paths", len(f.Paths)}, {"symbols", len(f.Symbols)}, {"instruction_files", len(f.InstructionFiles)},
		{"validation_commands", len(f.ValidationCommands)}, {"unknown", len(f.Unknown)},
	} {
		if collection.count > maxFactCount {
			return fmt.Errorf("%w: capture.facts.%s has %d values, max %d", ErrBoundExceeded, collection.name, collection.count, maxFactCount)
		}
	}
	pathKeys := make([]string, 0, len(f.Paths))
	for n, fact := range f.Paths {
		path := fmt.Sprintf("capture.facts.paths[%d]", n)
		if err := boundedString(path+".reference", fact.Reference, maxTextBytes, false); err != nil {
			return err
		}
		if !isOneOf(fact.Status, FactExists, FactAbsent, FactUnknown) {
			return unsupported(path+".status", fact.Status)
		}
		if err := boundedString(path+".evidence", fact.Evidence, maxTextBytes, false); err != nil {
			return err
		}
		if fact.Evidence != "git_tree:"+repo.Tree {
			return fmt.Errorf("%w: %s.evidence does not name the captured tree", ErrBindingMismatch, path)
		}
		if fact.Status == FactExists {
			if err := boundedString(path+".resolved_path", fact.ResolvedPath, maxTextBytes, false); err != nil {
				return err
			}
			if !objectIDPattern.MatchString(fact.Blob) || fact.Reason != "" {
				return fmt.Errorf("%w: %s: exists requires path/blob and no reason", ErrInvalidValue, path)
			}
		} else if fact.ResolvedPath != "" || fact.Blob != "" || fact.Reason == "" {
			return fmt.Errorf("%w: %s: non-existence requires only a reason", ErrInvalidValue, path)
		} else if err := boundedString(path+".reason", fact.Reason, maxTextBytes, false); err != nil {
			return err
		}
		pathKeys = append(pathKeys, fact.Reference)
	}
	if err := sortedUnique("capture.facts.paths", pathKeys); err != nil {
		return err
	}

	symbolKeys := make([]string, 0, len(f.Symbols))
	for n, fact := range f.Symbols {
		path := fmt.Sprintf("capture.facts.symbols[%d]", n)
		if err := boundedString(path+".reference", fact.Reference, maxTextBytes, false); err != nil {
			return err
		}
		if err := boundedString(path+".name", fact.Name, maxTextBytes, false); err != nil {
			return err
		}
		if !isOneOf(fact.Status, FactExists, FactAbsent, FactUnknown) {
			return unsupported(path+".status", fact.Status)
		}
		if fact.Evidence != "go_syntax_at_commit:"+repo.Commit {
			return fmt.Errorf("%w: %s.evidence does not name the captured commit", ErrBindingMismatch, path)
		}
		if fact.Status == FactExists {
			if err := boundedString(path+".path", fact.Path, maxTextBytes, false); err != nil {
				return err
			}
			if fact.Line <= 0 || !objectIDPattern.MatchString(fact.Blob) || fact.Reason != "" {
				return fmt.Errorf("%w: %s: exists requires path/line/blob and no reason", ErrInvalidValue, path)
			}
		} else if fact.Path != "" || fact.Line != 0 || fact.Blob != "" || fact.Reason == "" {
			return fmt.Errorf("%w: %s: unresolved symbol requires only a reason", ErrInvalidValue, path)
		} else if err := boundedString(path+".reason", fact.Reason, maxTextBytes, false); err != nil {
			return err
		}
		symbolKeys = append(symbolKeys, fact.Reference)
	}
	if err := sortedUnique("capture.facts.symbols", symbolKeys); err != nil {
		return err
	}

	instructionKeys := make([]string, 0, len(f.InstructionFiles))
	for n, fact := range f.InstructionFiles {
		path := fmt.Sprintf("capture.facts.instruction_files[%d]", n)
		if err := boundedString(path+".path", fact.Path, maxTextBytes, false); err != nil {
			return err
		}
		if !objectIDPattern.MatchString(fact.Blob) {
			return fmt.Errorf("%w: %s.blob", ErrInvalidValue, path)
		}
		instructionKeys = append(instructionKeys, fact.Path)
	}
	if err := sortedUnique("capture.facts.instruction_files", instructionKeys); err != nil {
		return err
	}

	commandKeys := make([]string, 0, len(f.ValidationCommands))
	for n, fact := range f.ValidationCommands {
		path := fmt.Sprintf("capture.facts.validation_commands[%d]", n)
		if err := boundedString(path+".command", fact.Command, maxTextBytes, false); err != nil {
			return err
		}
		if err := boundedString(path+".source", fact.Source, maxTextBytes, false); err != nil {
			return err
		}
		if fact.Line <= 0 || !objectIDPattern.MatchString(fact.Blob) {
			return fmt.Errorf("%w: %s requires a positive line and full blob ID", ErrInvalidValue, path)
		}
		commandKeys = append(commandKeys, fmt.Sprintf("%s:%09d:%s", fact.Source, fact.Line, fact.Command))
	}
	if err := sortedUnique("capture.facts.validation_commands", commandKeys); err != nil {
		return err
	}

	unknownKeys := make([]string, 0, len(f.Unknown))
	for n, fact := range f.Unknown {
		path := fmt.Sprintf("capture.facts.unknown[%d]", n)
		if err := boundedString(path+".reference", fact.Reference, maxTextBytes, false); err != nil {
			return err
		}
		if err := boundedString(path+".reason", fact.Reason, maxTextBytes, false); err != nil {
			return err
		}
		unknownKeys = append(unknownKeys, fact.Reference)
	}
	if err := sortedUnique("capture.facts.unknown", unknownKeys); err != nil {
		return err
	}
	return nil
}

func (d AdviceDocument) Validate() error {
	if err := version("advice_document.schema_version", d.SchemaVersion); err != nil {
		return err
	}
	if err := d.DeclaredProvenance.validate("advice_document.declared_provenance"); err != nil {
		return err
	}
	return d.Advice.Validate()
}

func (p Packet) Validate() error {
	if err := version("packet.schema_version", p.SchemaVersion); err != nil {
		return err
	}
	if p.KernelVersion != KernelVersion {
		return unsupported("packet.kernel_version", p.KernelVersion)
	}
	if err := p.Capture.Validate(); err != nil {
		return err
	}
	if err := p.DeclaredProvenance.validate("packet.declared_provenance"); err != nil {
		return err
	}
	return p.Advice.Validate()
}

func (p DeclaredProvenance) validate(path string) error {
	return joinErrors(
		boundedString(path+".provider", p.Provider, 256, false),
		boundedString(path+".model", p.Model, 256, false),
		boundedString(path+".command", p.Command, maxTextBytes, false),
		boundedString(path+".prompt", p.Prompt, 2048, false),
	)
}

func (a Advice) Validate() error {
	if a.CandidateNonGoals == nil || a.AssumptionsToConfirm == nil || a.DecisionQueue == nil ||
		a.ApplicableConstraints == nil || a.AcceptanceGaps == nil || a.ProposedAcceptanceTests == nil ||
		a.AffectedAreaHypotheses == nil || a.CandidateDeliverySplits == nil || a.Reasons == nil {
		return fmt.Errorf("%w: advisory collections must be JSON arrays, not null", ErrInvalidValue)
	}
	if err := boundedString("advice.restated_outcome", a.RestatedOutcome, 2048, false); err != nil {
		return err
	}
	for _, check := range []struct {
		path   string
		values []string
		max    int
	}{
		{"advice.candidate_non_goals", a.CandidateNonGoals, 5},
		{"advice.assumptions_to_confirm", a.AssumptionsToConfirm, 5},
		{"advice.applicable_constraints", a.ApplicableConstraints, 8},
		{"advice.acceptance_gaps", a.AcceptanceGaps, 8},
		{"advice.proposed_acceptance_tests", a.ProposedAcceptanceTests, 8},
		{"advice.affected_area_hypotheses", a.AffectedAreaHypotheses, 8},
		{"advice.reasons", a.Reasons, 5},
	} {
		if err := boundedList(check.path, check.values, check.max); err != nil {
			return err
		}
	}
	if len(a.DecisionQueue) > 5 {
		return fmt.Errorf("%w: advice.decision_queue has %d values, max 5", ErrBoundExceeded, len(a.DecisionQueue))
	}
	for i, decision := range a.DecisionQueue {
		path := fmt.Sprintf("advice.decision_queue[%d]", i)
		if decision.Options == nil {
			return fmt.Errorf("%w: %s.options must be a JSON array, not null", ErrInvalidValue, path)
		}
		if err := joinErrors(
			boundedString(path+".question", decision.Question, maxTextBytes, false),
			boundedString(path+".changes", decision.Changes, maxTextBytes, false),
			boundedList(path+".options", decision.Options, 3),
		); err != nil {
			return err
		}
		if !isOneOf(decision.Kind, DecisionResearch, DecisionHuman, DecisionPrototype) {
			return unsupported(path+".kind", decision.Kind)
		}
		if !isOneOf(decision.MaterialEffect, EffectDecision, EffectAcceptanceGap, EffectSplitBoundary) {
			return unsupported(path+".material_effect", decision.MaterialEffect)
		}
		if decision.Kind == DecisionHuman && len(decision.Options) < 2 {
			return fmt.Errorf("%w: %s human decision requires two or three concrete options", ErrInvalidValue, path)
		}
		if decision.Kind != DecisionHuman && len(decision.Options) != 0 {
			return fmt.Errorf("%w: %s research and prototype questions cannot carry decision options", ErrInvalidValue, path)
		}
		if len(decision.Options) == 1 {
			return fmt.Errorf("%w: %s.options must be empty or contain two or three values", ErrInvalidValue, path)
		}
		if len(decision.Options) == 0 {
			if decision.RecommendedOption != 0 {
				return fmt.Errorf("%w: %s.recommended_option must be zero without options", ErrInvalidValue, path)
			}
			continue
		}
		if decision.RecommendedOption < 1 || decision.RecommendedOption > len(decision.Options) {
			return fmt.Errorf("%w: %s.recommended_option must name a one-based option", ErrInvalidValue, path)
		}
		seenOptions := map[string]bool{}
		for _, option := range decision.Options {
			if seenOptions[option] {
				return fmt.Errorf("%w: %s.options contains a duplicate", ErrInvalidValue, path)
			}
			seenOptions[option] = true
		}
	}
	if len(a.CandidateDeliverySplits) > 5 {
		return fmt.Errorf("%w: advice.candidate_delivery_splits has %d values, max 5", ErrBoundExceeded, len(a.CandidateDeliverySplits))
	}
	for i, split := range a.CandidateDeliverySplits {
		path := fmt.Sprintf("advice.candidate_delivery_splits[%d]", i)
		if split.BlockedBy == nil {
			return fmt.Errorf("%w: %s.blocked_by must be a JSON array, not null", ErrInvalidValue, path)
		}
		if err := joinErrors(
			boundedString(path+".outcome", split.Outcome, maxTextBytes, false),
			boundedString(path+".independently_verifiable_by", split.IndependentlyVerifiableBy, maxTextBytes, false),
		); err != nil {
			return err
		}
		if len(split.BlockedBy) > 4 {
			return fmt.Errorf("%w: %s.blocked_by has %d values, max 4", ErrBoundExceeded, path, len(split.BlockedBy))
		}
		seen := map[int]bool{}
		for _, dependency := range split.BlockedBy {
			if dependency < 1 || dependency >= i+1 || seen[dependency] {
				return fmt.Errorf("%w: %s.blocked_by value %d must uniquely name an earlier split", ErrInvalidValue, path, dependency)
			}
			seen[dependency] = true
		}
	}
	if !isOneOf(a.Recommendation,
		RecommendationNoGap, RecommendationClarify, RecommendationDecisions,
		RecommendationDelivery, RecommendationContractDecision, RecommendationInsufficient) {
		return unsupported("advice.recommendation", a.Recommendation)
	}
	if len(a.Reasons) == 0 {
		return fmt.Errorf("%w: advice.reasons must explain the recommendation", ErrInvalidValue)
	}

	switch a.Recommendation {
	case RecommendationDecisions:
		if a.DecisionDecomposition == nil || len(a.DecisionQueue) == 0 || len(a.CandidateDeliverySplits) != 0 {
			return fmt.Errorf("%w: decompose_decisions requires a decision decomposition and frontier, and forbids delivery splits", ErrInvalidValue)
		}
	case RecommendationDelivery:
		if a.DecisionDecomposition != nil || len(a.CandidateDeliverySplits) < 2 || len(a.DecisionQueue) != 0 {
			return fmt.Errorf("%w: decompose_delivery requires at least two delivery splits and forbids decision decomposition or unresolved decisions", ErrInvalidValue)
		}
	default:
		if a.DecisionDecomposition != nil || len(a.CandidateDeliverySplits) != 0 {
			return fmt.Errorf("%w: decomposition shapes require their matching recommendation", ErrInvalidValue)
		}
	}
	if a.Recommendation == RecommendationNoGap && (len(a.DecisionQueue) != 0 || len(a.AcceptanceGaps) != 0) {
		return fmt.Errorf("%w: no_material_gap_identified cannot carry decisions or acceptance gaps", ErrInvalidValue)
	}
	if isOneOf(a.Recommendation, RecommendationClarify, RecommendationContractDecision) && len(a.DecisionQueue) == 0 {
		return fmt.Errorf("%w: %s requires at least one decision", ErrInvalidValue, a.Recommendation)
	}
	if a.DecisionDecomposition != nil {
		if a.DecisionDecomposition.NotYetSpecifiable == nil || a.DecisionDecomposition.OutOfScope == nil {
			return fmt.Errorf("%w: decision decomposition collections must be JSON arrays, not null", ErrInvalidValue)
		}
		if err := boundedString("advice.decision_decomposition.destination", a.DecisionDecomposition.Destination, maxTextBytes, false); err != nil {
			return err
		}
		if err := boundedList("advice.decision_decomposition.not_yet_specifiable", a.DecisionDecomposition.NotYetSpecifiable, 5); err != nil {
			return err
		}
		if err := boundedList("advice.decision_decomposition.out_of_scope", a.DecisionDecomposition.OutOfScope, 5); err != nil {
			return err
		}
	}
	return nil
}

func (d DispositionDocument) ValidateFor(packet Packet) error {
	if err := version("dispositions.schema_version", d.SchemaVersion); err != nil {
		return err
	}
	wantDigest, err := PacketDigest(packet)
	if err != nil {
		return err
	}
	if d.PacketDigest != wantDigest {
		return fmt.Errorf("%w: disposition packet digest got %q, want %q", ErrBindingMismatch, d.PacketDigest, wantDigest)
	}
	suggestions := Suggestions(packet.Advice)
	want := make(map[string]bool, len(suggestions))
	for _, suggestion := range suggestions {
		want[suggestion.ID] = true
	}
	seen := make(map[string]bool, len(d.Items))
	decisionOptions := make(map[string]map[string]bool, len(packet.Advice.DecisionQueue))
	for decisionIndex, decision := range packet.Advice.DecisionQueue {
		options := make(map[string]bool, len(decision.Options))
		for optionIndex := range decision.Options {
			options[decisionOptionID(decisionIndex, optionIndex)] = true
		}
		decisionOptions[numbered("DEC", decisionIndex)] = options
	}
	for i, item := range d.Items {
		if !want[item.SuggestionID] {
			return fmt.Errorf("%w: items[%d] names a non-disposition item %q", ErrDisposition, i, item.SuggestionID)
		}
		if seen[item.SuggestionID] {
			return fmt.Errorf("%w: duplicate suggestion %q", ErrDisposition, item.SuggestionID)
		}
		if !isOneOf(item.Disposition, DispositionAccepted, DispositionRejected, DispositionAlreadyPresent, DispositionUnclear) {
			return unsupported(fmt.Sprintf("dispositions.items[%d].disposition", i), item.Disposition)
		}
		options, isDecision := decisionOptions[item.SuggestionID]
		selectionRequired := isDecision && len(options) > 0 &&
			isOneOf(item.Disposition, DispositionAccepted, DispositionAlreadyPresent)
		switch {
		case selectionRequired && !options[item.SelectedOptionID]:
			return fmt.Errorf("%w: items[%d] must select an option belonging to %s", ErrDisposition, i, item.SuggestionID)
		case !selectionRequired && item.SelectedOptionID != "":
			return fmt.Errorf("%w: items[%d] cannot select an option for disposition %q", ErrDisposition, i, item.Disposition)
		}
		seen[item.SuggestionID] = true
	}
	if len(seen) != len(want) {
		var missing []string
		for id := range want {
			if !seen[id] {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		return fmt.Errorf("%w: missing dispositions for %v", ErrDisposition, missing)
	}
	return nil
}

func sortedUnique(path string, values []string) error {
	for i := 1; i < len(values); i++ {
		if values[i-1] >= values[i] {
			return fmt.Errorf("%w: %s must be sorted with unique values", ErrInvalidValue, path)
		}
	}
	return nil
}

func splitPath(path string) []string {
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return parts
}
