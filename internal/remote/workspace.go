package remote

import (
	"context"
	"fmt"
	"strconv"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Claim names one claim cycle, and it is the idempotency key of every workspace
// operation on this boundary.
//
// The claim cycle rather than the issue, because the issue outlives the claim
// and the workspace must not. §6.2's claim-scoped verification base is pinned to
// the tracker-native id of the assignment event that established the current
// claim (core.Workspace.ClaimEpoch), and a workspace acquired under the previous
// claim carries the previous base — so an Acquire keyed by issue alone would
// hand a fresh claim a tree pinned to work nobody re-approved.
//
// Epoch zero authorizes nothing, exactly as core.ClaimBase says of a pre-epoch
// pin: it is evidence for a comparison, never a key.
type Claim struct {
	// Repository is the backend-visible name of the repository the issue lives
	// in. Opaque here: this package never parses it.
	Repository string
	// Issue is core.Issue.Identifier — the tracker-stable identifier, not a
	// sanitized workspace key. The key is a workspace provider's spelling
	// (SPEC §6.3) and a backend has its own.
	Issue string
	// Epoch is the positive tracker-native id of the assignment event behind
	// the current claim (core.Workspace.ClaimEpoch).
	Epoch int64
}

// Valid reports whether a claim can key anything at all.
func (c Claim) Valid() bool {
	return c.Repository != "" && c.Issue != "" && c.Epoch > 0
}

// String is the stable durable spelling of a claim cycle: it names files in a
// store and appears in refusals. Deliberately not %v of the struct — a format
// that changes with a field rename would rename every record on disk.
func (c Claim) String() string {
	return c.Repository + "#" + c.Issue + "@" + strconv.FormatInt(c.Epoch, 10)
}

// Identity is the opaque remote workspace identity: everything a later process
// needs to address the same remote tree, and nothing it needs to interpret.
//
// "Opaque" is about SandboxID and ProfileRevision specifically. BEN never parses
// either; it stores them, hands them back, and compares them for equality. What
// it does own is the rest — the claim cycle, the branch, and the trusted base —
// because those are facts about *BEN's* work that a backend must not be the
// authority on.
//
// One struct rather than a reference plus a side table, for core.WorkspacePaths'
// reason: these travel together by contract, and a field added here reaches the
// durable record and the attach path without a seam remembering to forward it.
type Identity struct {
	Claim Claim
	// Branch is the issue branch this workspace publishes on — the provider's
	// `ben/<key>` (SPEC §6.3), resolved by BEN and told to the backend.
	Branch string
	// BaseSHA is the trusted base the claim pinned (SPEC §6.2). It is BEN's
	// fact: a backend that reported a different one would be moving the
	// verification base out from under §9.7.
	BaseSHA string
	// SandboxID is the backend's own name for the sandbox behind this
	// workspace. Opaque, and the reason a restart can attach at all.
	SandboxID string
	// ProfileRevision is the immutable identity of the image/profile the
	// sandbox was created from.
	//
	// Immutable is the load-bearing word. A profile that can be edited under a
	// running claim makes "attach to the same world" unprovable — the sandbox
	// id would still match while the world it names had changed — so a backend
	// publishes a revision and BEN pins it. A revision that moves between
	// acquire and attach is a different workspace wearing the same id.
	ProfileRevision string
}

// Complete reports whether an identity can be attached to later. Branch and
// BaseSHA are part of the address too: omitting either allows the same sandbox
// token to be reused for a different publication target or verification base.
func (i Identity) Complete() bool {
	return i.Claim.Valid() && i.Branch != "" && i.BaseSHA != "" &&
		i.SandboxID != "" && i.ProfileRevision != ""
}

// AcquireRequest is what BEN asks a backend for. It carries the facts BEN owns;
// the backend answers with those plus the two it owns (Identity).
type AcquireRequest struct {
	Claim   Claim
	Branch  string
	BaseSHA string
	// ProfileRevision pins the profile when the caller has one to pin — a
	// reattach after a restart, where the revision came off the durable record.
	// Empty asks the backend for its current revision, which is the fresh-claim
	// case.
	//
	// A backend MUST refuse a pinned revision it can no longer serve rather
	// than substituting its current one: substituting is the silent form of the
	// mutable-profile hazard ProfileRevision exists to close.
	ProfileRevision string
}

// WorkspaceBackend is the remote half of SPEC §6's workspace lifecycle:
// acquire, attach, suspend, delete, each idempotent over one claim cycle.
//
// Four verbs rather than §6.1's three, because a remote workspace has a state
// v1's has not: suspended. A local worktree costs a directory whether or not an
// attempt is running, so §6.1 needs only Prepare and Dispose; a backend sandbox
// costs money while it is warm, and the choice between releasing it between
// attempts and paying to keep it is the backend's to offer and BEN's to make.
// Suspend is therefore not Delete: the branch, the base pin and the sandbox id
// survive it, and a later Acquire on the same claim resumes rather than rebuilds.
//
// Idempotence is per claim cycle and is a requirement on implementations, not a
// hope: BEN retries these across restarts with no memory of whether the previous
// call landed, which is precisely the situation §9.10 is written for.
type WorkspaceBackend interface {
	// Acquire creates or resumes the workspace for a claim cycle and returns its
	// identity. Called twice with the same request it MUST return the same
	// identity rather than a second sandbox.
	Acquire(ctx context.Context, req AcquireRequest) (Identity, error)
	// Attach returns the identity of an existing workspace without creating one.
	// ErrNoWorkspace when the backend holds none — a fact. Any other error is
	// "could not ask", which is not (SPEC §9.10).
	Attach(ctx context.Context, claim Claim) (Identity, error)
	// Suspend releases the warm sandbox while retaining the workspace identity,
	// so a later Acquire resumes it. A no-op on an already-suspended claim.
	Suspend(ctx context.Context, claim Claim) error
	// Delete removes the workspace. A no-op when there is none, so a repeated
	// disposal after a crash is not an error.
	Delete(ctx context.Context, claim Claim) error
}

// MayReuse is the one gate between a run's status and touching its workspace.
//
// It is a function rather than a rule each call site applies, because it is the
// rule that gets applied *nearly*: "the phase is not running" and "the context
// did not error" are both one step from correct and both fail open. The only
// safe reading is a positive one — the backend attests domain quiet — and every
// other input, including a status nobody filled, must answer false
// (SPEC §9.8, core.Termination).
func MayReuse(s Status) bool {
	return s.Termination() == core.TerminationConfirmed
}

// Reacquire resumes a claim's workspace for a further attempt, and refuses while
// the previous run's termination is unconfirmed.
//
// Two operations rather than a rule each caller applies, because the rule is the
// one that gets applied *nearly*: a retry after an unconfirmed stop is exactly
// the moment the orchestrator wants the workspace back, and it is exactly the
// moment a possibly-live foreign process may still be writing to it (SPEC §9.8).
// Refusing here is what turns that into a retained claim and another tick.
func Reacquire(ctx context.Context, ws WorkspaceBackend, req AcquireRequest, prev Status) (Identity, error) {
	if !MayReuse(prev) {
		return Identity{}, fmt.Errorf("%w: %s is %s", ErrNotQuiet, req.Claim, prev.Phase)
	}
	return ws.Acquire(ctx, req)
}

// Dispose deletes a claim's workspace under the same gate. §6.4's local
// counterpart keeps a failed attempt's worktree for forensics; what is
// non-negotiable on either substrate is that nothing is removed out from under a
// run that may still be executing in it.
func Dispose(ctx context.Context, ws WorkspaceBackend, claim Claim, prev Status) error {
	if !MayReuse(prev) {
		return fmt.Errorf("%w: %s is %s", ErrNotQuiet, claim, prev.Phase)
	}
	return ws.Delete(ctx, claim)
}
