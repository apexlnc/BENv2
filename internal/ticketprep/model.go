// Package ticketprep implements #222's offline, read-only ticket preflight
// artifact. It establishes repository facts from committed Git objects and
// keeps them structurally separate from bounded, model-authored advice.
package ticketprep

import "errors"

const (
	SchemaVersion = 1
	KernelVersion = "ticketprep.v0"
)

const (
	maxArtifactBytes = 1 << 20
	maxTitleBytes    = 1024
	maxBodyBytes     = 256 << 10
	maxURLBytes      = 4096
	maxAdviceBytes   = 64 << 10
	maxTextBytes     = 1024
	maxFactCount     = 128
	maxTreeEntries   = 100_000
	maxBlobBytes     = 2 << 20
)

var (
	ErrInvalidUTF8      = errors.New("ticketprep: input is not valid UTF-8")
	ErrArtifactTooLarge = errors.New("ticketprep: artifact exceeds its byte limit")
	ErrInvalidJSON      = errors.New("ticketprep: invalid JSON")
	ErrDuplicateField   = errors.New("ticketprep: duplicate JSON field")
	ErrUnknownField     = errors.New("ticketprep: unknown JSON field")
	ErrTrailingJSON     = errors.New("ticketprep: trailing JSON value")
	ErrUnsupported      = errors.New("ticketprep: unsupported version or value")
	ErrBoundExceeded    = errors.New("ticketprep: schema bound exceeded")
	ErrInvalidValue     = errors.New("ticketprep: invalid value")
	ErrBindingMismatch  = errors.New("ticketprep: artifact binding mismatch")
	ErrRepository       = errors.New("ticketprep: repository observation failed")
	ErrDisposition      = errors.New("ticketprep: invalid human disposition")
)

// IssueInput is the exact forge snapshot supplied to capture. It is declared
// input, not a repository observation; the kernel validates its identity and
// computes its content digest without normalizing title or body bytes.
type IssueInput struct {
	SchemaVersion int    `json:"schema_version"`
	Number        int    `json:"number"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	Body          string `json:"body"`
}

type Capture struct {
	SchemaVersion int         `json:"schema_version"`
	KernelVersion string      `json:"kernel_version"`
	Subject       Subject     `json:"subject"`
	Repository    Repository  `json:"repository"`
	Facts         Facts       `json:"facts"`
	Sources       FactSources `json:"sources"`
}

type Subject struct {
	RepositoryIdentity string `json:"repository_identity"`
	IssueNumber        int    `json:"issue_number"`
	IssueURL           string `json:"issue_url"`
	Title              string `json:"title"`
	Body               string `json:"body"`
	ContentDigest      string `json:"content_digest"`
}

type Repository struct {
	Remote            string `json:"remote"`
	Identity          string `json:"identity"`
	RemoteFingerprint string `json:"remote_fingerprint"`
	Commit            string `json:"commit"`
	Tree              string `json:"tree"`
}

type FactSources struct {
	Issue      string `json:"issue"`
	Repository string `json:"repository"`
	Remote     string `json:"remote"`
}

type Facts struct {
	Paths              []PathFact        `json:"paths"`
	Symbols            []SymbolFact      `json:"symbols"`
	InstructionFiles   []InstructionFact `json:"instruction_files"`
	ValidationCommands []CommandFact     `json:"validation_commands"`
	Unknown            []UnknownFact     `json:"unknown"`
}

type FactStatus string

const (
	FactExists  FactStatus = "exists"
	FactAbsent  FactStatus = "absent"
	FactUnknown FactStatus = "unknown"
)

type PathFact struct {
	Reference    string     `json:"reference"`
	Status       FactStatus `json:"status"`
	ResolvedPath string     `json:"resolved_path,omitempty"`
	Blob         string     `json:"blob,omitempty"`
	Evidence     string     `json:"evidence"`
	Reason       string     `json:"reason,omitempty"`
}

type SymbolFact struct {
	Reference string     `json:"reference"`
	Status    FactStatus `json:"status"`
	Name      string     `json:"name"`
	Path      string     `json:"path,omitempty"`
	Line      int        `json:"line,omitempty"`
	Blob      string     `json:"blob,omitempty"`
	Evidence  string     `json:"evidence"`
	Reason    string     `json:"reason,omitempty"`
}

type InstructionFact struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
}

type CommandFact struct {
	Command string `json:"command"`
	Source  string `json:"source"`
	Line    int    `json:"line"`
	Blob    string `json:"blob"`
}

type UnknownFact struct {
	Reference string `json:"reference"`
	Reason    string `json:"reason"`
}

type AdviceDocument struct {
	SchemaVersion      int                `json:"schema_version"`
	DeclaredProvenance DeclaredProvenance `json:"declared_provenance"`
	Advice             Advice             `json:"advice"`
}

// DeclaredProvenance is copied by an operator or skill. The kernel did not
// launch the model, so none of these values are verified invocation facts.
type DeclaredProvenance struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Command  string `json:"command"`
	Prompt   string `json:"prompt"`
}

type Advice struct {
	RestatedOutcome         string                 `json:"restated_outcome"`
	CandidateNonGoals       []string               `json:"candidate_non_goals"`
	AssumptionsToConfirm    []string               `json:"assumptions_to_confirm"`
	DecisionQueue           []Decision             `json:"decision_queue"`
	ApplicableConstraints   []string               `json:"applicable_constraints"`
	AcceptanceGaps          []string               `json:"acceptance_gaps"`
	ProposedAcceptanceTests []string               `json:"proposed_acceptance_tests"`
	AffectedAreaHypotheses  []string               `json:"affected_area_hypotheses"`
	DecisionDecomposition   *DecisionDecomposition `json:"decision_decomposition,omitempty"`
	CandidateDeliverySplits []DeliverySplit        `json:"candidate_delivery_splits"`
	Recommendation          Recommendation         `json:"recommendation"`
	Reasons                 []string               `json:"reasons"`
}

type DecisionKind string

const (
	DecisionResearch  DecisionKind = "research_question"
	DecisionHuman     DecisionKind = "human_decision"
	DecisionPrototype DecisionKind = "prototype_question"
)

type MaterialEffect string

const (
	EffectDecision      MaterialEffect = "decision"
	EffectAcceptanceGap MaterialEffect = "acceptance_gap"
	EffectSplitBoundary MaterialEffect = "split_boundary"
)

type Decision struct {
	Question          string         `json:"question"`
	Kind              DecisionKind   `json:"kind"`
	MaterialEffect    MaterialEffect `json:"material_effect"`
	Changes           string         `json:"changes"`
	Options           []string       `json:"options"`
	RecommendedOption int            `json:"recommended_option"`
}

type DecisionDecomposition struct {
	Destination       string   `json:"destination"`
	NotYetSpecifiable []string `json:"not_yet_specifiable"`
	OutOfScope        []string `json:"out_of_scope"`
}

// BlockedBy contains one-based positions in CandidateDeliverySplits. The
// renderer converts them to its own SPLIT-NN IDs, so model text never assigns
// or impersonates a wrapper-owned suggestion identifier.
type DeliverySplit struct {
	Outcome                   string `json:"outcome"`
	IndependentlyVerifiableBy string `json:"independently_verifiable_by"`
	BlockedBy                 []int  `json:"blocked_by"`
}

type Recommendation string

const (
	RecommendationNoGap            Recommendation = "no_material_gap_identified"
	RecommendationClarify          Recommendation = "clarify"
	RecommendationDecisions        Recommendation = "decompose_decisions"
	RecommendationDelivery         Recommendation = "decompose_delivery"
	RecommendationContractDecision Recommendation = "requires_contract_decision"
	RecommendationInsufficient     Recommendation = "insufficient_context"
)

type Packet struct {
	SchemaVersion      int                `json:"schema_version"`
	KernelVersion      string             `json:"kernel_version"`
	Capture            Capture            `json:"capture"`
	DeclaredProvenance DeclaredProvenance `json:"declared_provenance"`
	Advice             Advice             `json:"advice"`
}

type Disposition string

const (
	DispositionAccepted       Disposition = "accepted"
	DispositionRejected       Disposition = "rejected"
	DispositionAlreadyPresent Disposition = "already-present"
	DispositionUnclear        Disposition = "unclear"
)

type DispositionDocument struct {
	SchemaVersion int                `json:"schema_version"`
	PacketDigest  string             `json:"packet_digest"`
	Items         []DispositionEntry `json:"items"`
}

type DispositionEntry struct {
	SuggestionID     string      `json:"suggestion_id"`
	Disposition      Disposition `json:"disposition"`
	SelectedOptionID string      `json:"selected_option_id,omitempty"`
}

type SectionBinding string

const (
	BindingSubject           SectionBinding = "subject"
	BindingSubjectRepository SectionBinding = "subject_and_repository"
)

type FreshnessStatus string

const (
	FreshnessMatchesCapture FreshnessStatus = "matches_capture"
	FreshnessStale          FreshnessStatus = "stale"
)

type FreshnessReport struct {
	SchemaVersion     int                `json:"schema_version"`
	PacketDigest      string             `json:"packet_digest"`
	SubjectMatches    bool               `json:"subject_matches"`
	RepositoryMatches bool               `json:"repository_matches"`
	Sections          []SectionFreshness `json:"sections"`
}

type SectionFreshness struct {
	Section string          `json:"section"`
	Binding SectionBinding  `json:"binding"`
	Status  FreshnessStatus `json:"status"`
	Reason  string          `json:"reason"`
}

// Suggestion is a renderer-owned view. ID and binding are derived from schema
// position and never accepted from advisory JSON.
type Suggestion struct {
	ID      string
	Section string
	Binding SectionBinding
	Text    string
}
