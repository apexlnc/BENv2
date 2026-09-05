package review

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Verdict is the closed set a reviewer may return. Anything else — missing,
// empty, misspelled, or a third word — authorizes no routing at all, which is
// why the zero value is not a member.
type Verdict string

const (
	VerdictClean            Verdict = "clean"
	VerdictChangesRequested Verdict = "changes_requested"
)

// Outcome is the closed set of routes the controller records. `revise` is the
// only one that continues automation; the other three stop it.
type Outcome string

const (
	OutcomeRevise      Outcome = "revise"
	OutcomeHumanReview Outcome = "human-review"
	OutcomeRoundCap    Outcome = "round-cap"
	OutcomeNoProgress  Outcome = "no-progress"

	// HumanReviewLabel is the controller's one permitted label addition. The
	// value is deliberately not configuration: accepting an arbitrary label
	// would let a deployment turn a terminal route into a workflow approval or
	// a write to BEN's state projection.
	HumanReviewLabel = "human-review"

	// ReviewerProfileLabelPrefix is the fixed issue-label namespace a human
	// uses to select one operator-defined reviewer profile. It is deliberately
	// not configurable: a deployment may choose the profiles, but every ticket
	// should spell the selection the same way and the controller must never be
	// able to turn one of its own label writes into a profile choice.
	ReviewerProfileLabelPrefix = "review-profile:"
	// MaxReviewerProfiles keeps one workflow declaration and the label-choice
	// policy bounded. More tiers are operationally indistinguishable from raw
	// model selection and should be consolidated by the operator.
	MaxReviewerProfiles = 8
)

// ErrInvalidConfig is the load-time refusal: a controller that does not know
// which identities to trust cannot decide anything safely.
var ErrInvalidConfig = errors.New("review: invalid controller configuration")

// Config is what the controller must be told, because none of it is derivable
// from the artifacts it reads.
//
// The three identities are deliberately separate fields even where a
// deployment happens to use one account for two of them. #155 decoupled the
// claim assignee from the tracker credential, so Principal and TrackerAuthor
// are different logins in the topology docs/DEPLOY.md describes, and
// Controller is a third that must never be Principal. Tracker/controller
// overlap is refused by default; the one explicit exception exists only for a
// supervised canary that accepts GitHub App attribution cannot distinguish
// their otherwise separate credentials.
type Config struct {
	Owner string // repository owner of the issue and its pull request
	Repo  string // repository name
	Issue int    // the issue number this reduction is about

	// Principal is BEN's claim assignee (SPEC §9.1): the one login the
	// controller may ever unassign, and only when it is the sole assignee.
	Principal string
	// TrackerAuthor is the login BEN's tracker credential posts milestone
	// comments as. Only comments by this author can trigger a round.
	TrackerAuthor string
	// Controller is the login the controller's own reviews and route artifacts
	// are published as. Only artifacts by this author are trusted as the
	// durable record.
	Controller string
	// AllowSharedTrackerController is an attended-canary escape hatch for a
	// deployment whose tracker and controller tokens are minted by the same
	// GitHub App. GitHub attributes both artifacts to one login, so enabling
	// this forfeits independent author provenance while leaving the controller's
	// closed write set and credential isolation intact.
	AllowSharedTrackerController bool

	// RequiredLabels is the complete human-applied approval set (SPEC §9.5).
	// Its last-applied standing event is the workspace-cycle anchor, exactly as
	// the remote workspace strategy derives it.
	RequiredLabels []string
	// QueueLabel is the one required label the controller may remove. Removing
	// it revokes the whole approval set; the controller never adds any member.
	QueueLabel string
	// AddHumanReviewLabel controls whether terminal routes add the one fixed,
	// non-required informational label named by HumanReviewLabel.
	AddHumanReviewLabel bool

	// RoundCap bounds automation at this many distinct controller-reviewed
	// head SHAs within one approval cycle.
	RoundCap int

	// ReviewerProfiles is the closed set of operator-defined profile names a
	// ticket may select. Empty preserves the legacy one-argv mode and disables
	// issue-label selection. DefaultReviewerProfile must name one member when
	// the set is non-empty.
	ReviewerProfiles       []string
	DefaultReviewerProfile string
}

// Branch is the canonical head branch for this issue — `ben/<n>`, the one
// SPEC §6.2 gives the workspace. A pull request on any other branch is not
// this issue's work no matter what its body says.
func (c Config) Branch() string {
	return fmt.Sprintf("ben/%d", c.Issue)
}

// Validate refuses a configuration that could route wrongly rather than
// discovering it mid-cycle. The distinctness checks are the load-bearing ones:
// see the type comment.
func (c Config) Validate() error {
	var missing []string
	for _, f := range []struct {
		name  string
		value string
	}{
		{"owner", c.Owner},
		{"repo", c.Repo},
		{"principal", c.Principal},
		{"tracker author", c.TrackerAuthor},
		{"controller", c.Controller},
		{"queue label", c.QueueLabel},
	} {
		if strings.TrimSpace(f.value) == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing %s", ErrInvalidConfig, strings.Join(missing, ", "))
	}
	if len(c.RequiredLabels) == 0 {
		return fmt.Errorf("%w: missing required labels", ErrInvalidConfig)
	}
	seenLabels := make([]string, 0, len(c.RequiredLabels))
	for i, label := range c.RequiredLabels {
		label = strings.TrimSpace(label)
		if label == "" {
			return fmt.Errorf("%w: required label %d is blank", ErrInvalidConfig, i)
		}
		if hasLabel(seenLabels, label) {
			return fmt.Errorf("%w: required label %q is repeated", ErrInvalidConfig, label)
		}
		seenLabels = append(seenLabels, label)
	}
	if c.Issue <= 0 {
		return fmt.Errorf("%w: issue number %d is not positive", ErrInvalidConfig, c.Issue)
	}
	if c.RoundCap < 1 {
		return fmt.Errorf("%w: round cap %d leaves no round to run", ErrInvalidConfig, c.RoundCap)
	}
	if err := c.validateReviewerProfiles(); err != nil {
		return err
	}
	queueLabel := strings.TrimSpace(c.QueueLabel)
	if len(queueLabel) >= len("ben:") && strings.EqualFold(queueLabel[:len("ben:")], "ben:") {
		return fmt.Errorf("%w: queue label %q is in BEN's reserved state-label namespace", ErrInvalidConfig, c.QueueLabel)
	}
	if eqFold(c.Controller, c.Principal) {
		return fmt.Errorf("%w: the controller identity %q is also the claim principal — it would review and unassign itself", ErrInvalidConfig, c.Controller)
	}
	if eqFold(c.Controller, c.TrackerAuthor) && !c.AllowSharedTrackerController {
		return fmt.Errorf("%w: the controller identity %q is also the milestone author — it could trigger its own rounds", ErrInvalidConfig, c.Controller)
	}
	if c.AddHumanReviewLabel && eqFold(HumanReviewLabel, c.QueueLabel) {
		return fmt.Errorf("%w: the fixed informational label %q is the required label — adding it would be an approval", ErrInvalidConfig, HumanReviewLabel)
	}
	if !hasLabel(c.RequiredLabels, c.QueueLabel) {
		return fmt.Errorf("%w: queue label %q is not in the complete required-label set", ErrInvalidConfig, c.QueueLabel)
	}
	if c.AddHumanReviewLabel && hasLabel(c.RequiredLabels, HumanReviewLabel) {
		return fmt.Errorf("%w: the fixed informational label %q is in the required-label set — adding it would be an approval", ErrInvalidConfig, HumanReviewLabel)
	}
	for _, label := range c.RequiredLabels {
		if hasReviewerProfilePrefix(label) {
			return fmt.Errorf("%w: required label %q is in the reviewer-profile namespace; profile choice is operational policy, not workflow approval", ErrInvalidConfig, label)
		}
	}
	return nil
}

func (c Config) validateReviewerProfiles() error {
	if len(c.ReviewerProfiles) == 0 {
		if c.DefaultReviewerProfile != "" {
			return fmt.Errorf("%w: default reviewer profile %q is set without named reviewer profiles", ErrInvalidConfig, c.DefaultReviewerProfile)
		}
		return nil
	}
	if !ValidReviewerProfile(c.DefaultReviewerProfile) {
		return fmt.Errorf("%w: default reviewer profile %q is not a lowercase name of 1-32 letters, digits, or hyphens", ErrInvalidConfig, c.DefaultReviewerProfile)
	}
	if len(c.ReviewerProfiles) > MaxReviewerProfiles {
		return fmt.Errorf("%w: %d reviewer profiles exceed the limit of %d", ErrInvalidConfig, len(c.ReviewerProfiles), MaxReviewerProfiles)
	}
	seen := make([]string, 0, len(c.ReviewerProfiles))
	foundDefault := false
	for _, name := range c.ReviewerProfiles {
		if !ValidReviewerProfile(name) {
			return fmt.Errorf("%w: reviewer profile %q is not a lowercase name of 1-32 letters, digits, or hyphens", ErrInvalidConfig, name)
		}
		if hasLabel(seen, name) {
			return fmt.Errorf("%w: reviewer profile %q is repeated", ErrInvalidConfig, name)
		}
		seen = append(seen, name)
		foundDefault = foundDefault || name == c.DefaultReviewerProfile
	}
	if !foundDefault {
		return fmt.Errorf("%w: default reviewer profile %q is not in the configured set", ErrInvalidConfig, c.DefaultReviewerProfile)
	}
	return nil
}

// ValidReviewerProfile reports whether name can safely travel in a GitHub
// label, a marker k=v field, a durable run identity and structured logs without
// another escaping convention.
func ValidReviewerProfile(name string) bool {
	if len(name) < 1 || len(name) > 32 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

func hasReviewerProfilePrefix(label string) bool {
	label = strings.TrimSpace(label)
	return len(label) >= len(ReviewerProfileLabelPrefix) &&
		strings.EqualFold(label[:len(ReviewerProfileLabelPrefix)], ReviewerProfileLabelPrefix)
}

// SelectReviewerProfile resolves the current ticket labels against the closed
// operator-defined set. The empty result is the legacy one-argv mode. Unknown
// or multiple selections are refusals, never a reason to guess at the default.
func (c Config) SelectReviewerProfile(labels []string) (string, error) {
	var selected string
	count := 0
	for _, label := range labels {
		if !hasReviewerProfilePrefix(label) {
			continue
		}
		count++
		if len(c.ReviewerProfiles) == 0 {
			continue
		}
		name := strings.TrimSpace(label)[len(ReviewerProfileLabelPrefix):]
		canonical := ""
		for _, allowed := range c.ReviewerProfiles {
			if strings.EqualFold(name, allowed) {
				canonical = allowed
				break
			}
		}
		if canonical == "" {
			return "", fmt.Errorf("reviewer profile label %q names no configured profile", label)
		}
		selected = canonical
	}
	if len(c.ReviewerProfiles) == 0 {
		if count > 0 {
			return "", fmt.Errorf("reviewer profile labels are not selectable with legacy review.reviewer_argv")
		}
		return "", nil
	}
	if count > 1 {
		return "", fmt.Errorf("issue carries %d reviewer profile labels; exactly zero or one is allowed", count)
	}
	if selected == "" {
		return c.DefaultReviewerProfile, nil
	}
	return selected, nil
}

// Issue is the tracker facts the reducer reads. Labels and assignees are
// compared case-insensitively, as GitHub logins are.
type Issue struct {
	Number    int
	Closed    bool
	Labels    []string
	Assignees []string
}

// Comment is one issue comment. The controller reads BEN's published
// milestones (Author == TrackerAuthor) and its own route intents/completions
// (Author == Controller); shape alone is never authority for either.
type Comment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
}

// Event is one entry of the issue's ordered change log, normalized to the four
// types the controller reasons about: assigned, unassigned, labeled,
// unlabeled.
type Event struct {
	ID        int64
	Type      string
	Actor     string
	Assignee  string
	Label     string
	CreatedAt time.Time
}

// Event types, spelled as GitHub spells them.
const (
	EventAssigned   = "assigned"
	EventUnassigned = "unassigned"
	EventLabeled    = "labeled"
	EventUnlabeled  = "unlabeled"
)

// PullRequest is the reviewed subject, resolved from the milestone's link and
// then checked against the branch and the issue independently.
type PullRequest struct {
	Number  int
	URL     string
	Closed  bool
	Merged  bool
	Branch  string // head ref
	Head    string // head SHA
	Base    string // base ref
	BaseSHA string // base SHA, the exact lower endpoint of the reviewed diff
	Body    string
}

// Review is one published pull request review. CommitID is GitHub's own
// binding of the review to a commit; the controller requires it to agree with
// the head recorded in the review's marker, so a marker alone cannot claim a
// head the forge does not confirm.
type Review struct {
	ID          int64
	Author      string
	Body        string
	CommitID    string
	State       string
	SubmittedAt time.Time
}

// Snapshot is one whole observation of an issue and its pull request. Every
// list is aggregated across pages before it gets here and sorted ascending:
// the reducer counts and filters, and a reducer handed one page of three would
// silently answer a different question than one handed all thirty.
type Snapshot struct {
	Issue    Issue
	Comments []Comment
	Events   []Event
	Reviews  []Review

	// PR is the pull request the latest published milestone links to, or nil
	// when there is none to resolve or it could not be read. Nil is a refusal,
	// never an empty success.
	PR *PullRequest
}

func eqFold(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if eqFold(l, want) {
			return true
		}
	}
	return false
}

func missingRequiredLabel(labels, required []string) string {
	for _, want := range required {
		if !hasLabel(labels, want) {
			return want
		}
	}
	return ""
}
