package remotews

import (
	"errors"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The named refusals (AGENTS.md conventions); tests assert on these rather than
// on message text. Every one of them fails closed: the claim is retained and
// nothing is dispatched, disposed or verified.
var (
	// ErrNoCycle says the durable store holds no workspace cycle for an issue.
	// A fact — nothing was ever begun — as against a store that would not answer.
	ErrNoCycle = errors.New("remotews: no workspace cycle is recorded for this issue")

	// ErrNoCycleDisposal says the durable store holds no end-of-cycle disposal
	// record at the address the caller supplied. It is an absence fact, unlike an
	// unreadable record, and is used only to select the current-cycle completion
	// path.
	ErrNoCycleDisposal = errors.New("remotews: no ended workspace-cycle disposal is recorded at this address")

	// ErrCycleState refuses a cycle record this package did not leave behind: an
	// unknown version, a torn epoch/base pair, a record naming another issue or
	// another repository. Fail closed with no auto-repair, for SPEC §6.6's
	// reason — a record nobody understands is an address, and repairing an
	// address is how a claim dispatches into somebody else's workspace.
	ErrCycleState = errors.New("remotews: workspace-cycle record is in an unexpected state")

	// ErrClaimTargetUnrecorded marks a pre-#152 cycle record. It may be read to
	// carry a valid base/cycle into a later assignment, but it cannot authorize
	// same-epoch prepare, restore, prompt rendering, or verification.
	ErrClaimTargetUnrecorded = core.ErrClaimTargetUnrecorded

	// ErrClaimEpoch refuses a non-positive assignment epoch. Zero authorizes
	// nothing (core.ClaimBase), and it is the value a caller that forgot to
	// establish one has.
	ErrClaimEpoch = errors.New("remotews: claim epoch must be positive")

	// ErrApprovalUnknown refuses a claim whose standing approval event cannot be
	// named. The approval is what selects the sandbox, so a cycle BEN cannot
	// anchor is a sandbox BEN would have to guess at — and the guess that
	// succeeds is the one that attaches the previous cycle's tree.
	ErrApprovalUnknown = errors.New("remotews: the tracker's change log does not name a standing approval to anchor the workspace cycle to (SPEC §6.7, §9.5)")

	// ErrRestoreDiverged refuses to restore a canonical branch whose head does
	// not descend the claim's trusted base.
	//
	// It is the fail-closed half of the remote-first restore. A head the pin is
	// not an ancestor of is a force push or somebody else's history, and handing
	// it to the next attempt would seed a revision on a tree whose publication
	// can never verify (verify.RemoteChecker leg 1). Parking costs a human's
	// attention; restoring costs an attempt and ends in the same park.
	ErrRestoreDiverged = errors.New("remotews: the canonical branch head does not descend the claim's trusted base")

	// ErrNoDisposer refuses a disposal on a strategy assembled without a
	// retention policy. Named rather than silently skipped: a claim whose
	// workspace is never released holds a backend lease against
	// `limits.max_concurrent_agents` forever (docs/AIRLOCK.md).
	ErrNoDisposer = errors.New("remotews: this strategy has no end-of-claim disposal policy")

	// ErrEvidenceScheme refuses run evidence from another substrate. §9.10's
	// marker names its mechanism precisely so that a scheme this daemon cannot
	// interpret is a refusal rather than a guess (core.RunEvidence).
	ErrEvidenceScheme = errors.New("remotews: run evidence does not name this substrate")

	// ErrCycleMoved refuses a completion for a workspace cycle the record no
	// longer names. Cycle records are keyed by workspace key, which is per issue
	// and outlives every cycle under it, so a disposal owed for one approval can
	// arrive after a revocation and a reapproval have installed another — and
	// applying it would delete the replacement's sandbox and retire its pin
	// (#252). The address carries the approval anchor, so the caller's own
	// core.Workspace is enough to tell the two apart.
	ErrCycleMoved = errors.New("remotews: this completion is owed for a workspace cycle the record no longer names")

	// ErrCycleReplacementPending refuses to dispose a superseded cycle until the
	// replacement cycle is durable. The disposal record is written first, so a
	// crash can expose this state; waiting is what prevents a retry from recording
	// and applying the same obligation a second time.
	ErrCycleReplacementPending = errors.New("remotews: the replacement workspace cycle is not durable yet")
)
