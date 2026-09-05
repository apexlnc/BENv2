package reviewrun

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/review"
)

// Role names what a run in a workspace cycle's sandbox is *for*.
//
// It is part of the derived run identity so that the coding attempt, the review
// of what it produced, and the revision that follows are three different
// durable runs in one physical sandbox — which is the whole of #204's
// "distinct durable run IDs in the same physical sandbox". A run id that
// omitted the role would let a review resolve a coding dispatch's idempotency
// address and inherit its output.
const Role = "codex-review"

// Subject is the exact review subject, captured by trusted BEN code.
//
// Every field is read from the forge and revalidated by the controller before
// and after publication (docs/REVIEW.md); nothing here is discovered by the
// model. In particular Diff is bytes BEN fetched pinned to both endpoint SHAs,
// not a repository the reviewer is invited to explore — the reviewer holds no
// forge credential and is never asked which pull request is current.
type Subject struct {
	// Repository is the credential-free identity of the repository, spelled the
	// way the workspace cycle spells it (mirror.Mirror.Repository()).
	Repository string
	// Issue is the tracker-stable identifier the workspace cycle is keyed by.
	Issue string
	// Cycle is the workspace-cycle anchor: the tracker-native id of the standing
	// human approval-label event (SPEC §6.7). It selects the sandbox, and a
	// revocation followed by reapproval gives a different value — which is what
	// makes attaching the previous cycle's sandbox unexpressible rather than
	// discouraged (remotews).
	Cycle int64
	// Occurrence is the published milestone's state-transition id: the delivery
	// and idempotency key of the review round (internal/review).
	Occurrence int64
	// Claim is the SPEC §9.5 claim epoch standing at the occurrence. It is
	// recorded but deliberately *not* part of the run identity: a reassignment
	// inside one approval revises the same tree, and the occurrence, base and
	// head already move when there is new work to judge.
	Claim int64

	// PR, TargetBranch, Base and Head name the exact comparison. Base and Head
	// are full commit SHAs; a moved endpoint or retarget is a different subject
	// and therefore a different run.
	PR           int
	TargetBranch string
	Base         string
	Head         string

	// Diff is the captured comparison, as bytes.
	Diff string

	// ReviewerProfile names the operator-defined invocation selected for this
	// ticket. Empty preserves the legacy one-argv identity exactly.
	ReviewerProfile string
}

// Complete reports whether a subject can key a durable run. Every clause is a
// field whose absence would let two different reviews share one identity.
func (s Subject) Complete() bool {
	return s.Repository != "" && s.Issue != "" && s.Cycle > 0 && s.Occurrence > 0 &&
		s.Claim > 0 && s.PR > 0 && s.TargetBranch != "" && isFullSHA(s.Base) && isFullSHA(s.Head)
}

// Validate is Complete with a sentence saying which clause failed.
func (s Subject) Validate() error {
	var missing []string
	if s.Repository == "" {
		missing = append(missing, "repository")
	}
	if s.Issue == "" {
		missing = append(missing, "issue")
	}
	if s.Cycle <= 0 {
		missing = append(missing, "workspace cycle")
	}
	if s.Occurrence <= 0 {
		missing = append(missing, "occurrence")
	}
	if s.Claim <= 0 {
		missing = append(missing, "claim epoch")
	}
	if s.PR <= 0 {
		missing = append(missing, "pull request")
	}
	if s.TargetBranch == "" {
		missing = append(missing, "target branch")
	}
	if !isFullSHA(s.Base) {
		missing = append(missing, "base sha")
	}
	if !isFullSHA(s.Head) {
		missing = append(missing, "head sha")
	}
	if s.ReviewerProfile != "" && !review.ValidReviewerProfile(s.ReviewerProfile) {
		missing = append(missing, "reviewer profile")
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: %s", ErrSubject, strings.Join(missing, ", "))
	}
	return nil
}

// CycleAddress is the opaque name of the workspace cycle this subject belongs
// to. Records are grouped by it, which is what lets the quiet gate ask "is any
// other run in *this sandbox* still going" without holding a backend handle.
func (s Subject) CycleAddress() string {
	return s.Repository + "#" + s.Issue + "@" + strconv.FormatInt(s.Cycle, 10)
}

// RunID derives the idempotent review-run identity.
//
// From the workspace cycle, the occurrence, both diff endpoints and the
// reviewer role — #204's list exactly — and from nothing else. Two properties
// follow, and both are load-bearing:
//
//   - **Deterministic.** A restart, a retry, or a second daemon reduces the same
//     subject to the same address, so a lost start response is *resolved* rather
//     than replaced. Nothing here reads a clock or a counter.
//   - **Total over the subject.** A moved head or base, a new occurrence, or a
//     new approval cycle produces a different address, so a stale run can never
//     be reattached and mistaken for a verdict on the current diff.
//
// The digest is over length-prefixed fields rather than a joined string: a
// repository named `a#b` and an issue `c` must not collide with `a` and `b#c`.
func (s Subject) RunID() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	h := sha256.New()
	parts := []string{
		Role,
		s.Repository,
		s.Issue,
		strconv.FormatInt(s.Cycle, 10),
		strconv.FormatInt(s.Occurrence, 10),
		s.TargetBranch,
		s.Base,
		s.Head,
	}
	if s.ReviewerProfile != "" {
		parts = append(parts, s.ReviewerProfile)
	}
	for _, part := range parts {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return "review-" + hex.EncodeToString(h.Sum(nil))[:32], nil
}

// SubjectDigest identifies the whole subject including the bytes handed to the
// model. It is compared on every resume: an identity that matched while the
// diff behind it had changed would be a verdict on one comparison recorded
// against another.
func (s Subject) SubjectDigest() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	h := sha256.New()
	parts := []string{
		Role,
		s.Repository,
		s.Issue,
		strconv.FormatInt(s.Cycle, 10),
		strconv.FormatInt(s.Occurrence, 10),
		strconv.FormatInt(s.Claim, 10),
		strconv.Itoa(s.PR),
		s.TargetBranch,
		s.Base,
		s.Head,
		s.Diff,
	}
	if s.ReviewerProfile != "" {
		parts = append(parts, s.ReviewerProfile)
	}
	for _, part := range parts {
		fmt.Fprintf(h, "%d:%s\n", len(part), part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// isFullSHA holds an endpoint to the 40-hex form the API returns, exactly as
// internal/review's marker parser does. An abbreviation would make two heads
// compare unequal that are the same commit, and every identity here is an
// equality comparison.
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
