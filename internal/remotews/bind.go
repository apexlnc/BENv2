package remotews

import (
	"context"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// EnvIssue is the orchestrator variable a run spec carries its issue identifier
// in (SPEC §7.6's BEN_ namespace, pipeline.beginStart).
//
// Read rather than derived, because a RunSpec deliberately does not carry the
// issue any other way: §7.1 gives an adapter the workspace *paths* and nothing
// about the claim, and on this substrate the "path" is an opaque address. The
// two are cross-checked below, which is what turns a mismatched spec into a
// refusal rather than a run dispatched into the wrong cycle's sandbox.
const EnvIssue = "BEN_ISSUE"

// Bind resolves one attempt's run spec to the backend workspace it must run in.
//
// It reads the identity from the backend rather than from a copy of its own. The
// sandbox id and the pinned profile revision are the backend's facts, and a
// second store of them here would be a second address that can go stale — which
// docs/AIRLOCK.md names as worse than having none, because a missing address
// parks a claim while a wrong one dispatches into somebody else's sandbox.
//
// The context is the caller's: this runs inside AgentRunner.Start, which the
// orchestrator already gives a context descending from the loop's.
func (p *Provider) Bind(ctx context.Context, spec core.RunSpec) (remote.Identity, remote.GitScope, error) {
	identifier := spec.Env[EnvIssue]
	if identifier == "" {
		return remote.Identity{}, remote.GitScope{}, fmt.Errorf("%w: the run spec names no issue in %s", ErrCycleState, EnvIssue)
	}
	key := Key(identifier)
	c, err := p.load(key, identifier)
	if err != nil {
		return remote.Identity{}, remote.GitScope{}, err
	}
	if spec.Workspace.Path != c.Address() {
		// The spec was prepared against a different workspace cycle — a revocation
		// and reapproval that landed between the prepare and the launch is the shape
		// that produces it. Launching anyway would run this attempt in the sandbox
		// of a cycle nobody dispatched it for.
		return remote.Identity{}, remote.GitScope{}, fmt.Errorf("%w: the run spec names workspace %q and issue %s is now %q",
			ErrCycleState, spec.Workspace.Path, identifier, c.Address())
	}
	if c.State != cyclePinned {
		return remote.Identity{}, remote.GitScope{}, fmt.Errorf("%w: issue %s is %s and a launch needs a pinned verification base",
			ErrCycleState, identifier, c.State)
	}
	id, err := p.identity(ctx, c)
	if err != nil {
		return remote.Identity{}, remote.GitScope{}, err
	}
	return id, p.gitScope(c, remote.GitPhaseCoding), nil
}

func (p *Provider) gitScope(c Cycle, phase remote.GitPhase) remote.GitScope {
	return remote.GitScope{
		Phase: phase, Repository: p.gitRepository, Branch: c.Branch,
		BaseCommit: c.BaseSHA, BaseBranch: c.TargetBranch,
	}
}

// CycleIdentity names and resumes the sandbox an issue's *standing workspace
// cycle* already holds, without preparing, creating or pinning anything (#204).
// expectedApproval is the controller's independently derived subject anchor;
// the durable record and a fresh tracker read must both still name it before
// the backend is touched.
//
// It is Bind's sibling and deliberately not Bind. Bind answers "where does this
// attempt launch", so it demands a run spec and a pinned verification base;
// this answers "which sandbox does this issue's approved work live in", which
// is what a review of that work has to be told and is a strictly weaker
// question. In particular it does not require a pinned base: a review happens
// *after* an attempt published, and the claim may already have been handed back.
//
// What it does require is unchanged and is the whole safety property: the
// identity comes from the backend rather than from a copy, while the target
// returned beside it comes from the durable cycle, and both belong to the one
// the *standing* approval selects. A cycle that has been superseded by a
// revocation and a reapproval has a different address, so the retained sandbox
// of the previous cycle is not reachable from here at all — which is what makes
// attaching it unexpressible rather than merely discouraged. Attach precedes
// Acquire so a deleted cycle remains absent; Acquire only resumes the existing
// suspended sandbox.
func (p *Provider) CycleIdentity(ctx context.Context, issue core.Issue, expectedApproval int64) (remote.Identity, string, error) {
	if expectedApproval <= 0 {
		return remote.Identity{}, "", fmt.Errorf("%w: review approval must be positive", ErrApprovalUnknown)
	}
	key, err := p.key(issue)
	if err != nil {
		return remote.Identity{}, "", err
	}
	c, err := p.load(key, issue.Identifier)
	if err != nil {
		return remote.Identity{}, "", err
	}
	if c.TargetBranch == "" {
		return remote.Identity{}, "", fmt.Errorf("%w: issue %s", ErrClaimTargetUnrecorded, issue.Identifier)
	}
	approval, err := p.cycles.WorkspaceCycle(ctx, issue)
	if err != nil {
		return remote.Identity{}, "", fmt.Errorf("remotews: resolving the workspace cycle for issue %s: %w", issue.Identifier, err)
	}
	if approval <= 0 {
		return remote.Identity{}, "", fmt.Errorf("%w: issue %s", ErrApprovalUnknown, issue.Identifier)
	}
	if c.Approval != expectedApproval || approval != expectedApproval {
		// BeginClaimBase is the only transition that may replace a cycle record.
		// A reviewer is a reader of the standing cycle, so observing that the
		// tracker has moved first must park it before any backend attach or resume
		// can make the superseded sandbox executable again.
		return remote.Identity{}, "", fmt.Errorf(
			"%w: issue %s review expects approval %d, the record names %d, and the standing approval is %d",
			ErrCycleState, issue.Identifier, expectedApproval, c.Approval, approval)
	}
	// Attach first is the existence fence. A successful publication normally
	// suspends its sandbox, and Acquire is the verb that resumes one; calling it
	// without first proving that this cycle still has a workspace would let a
	// delete-on-success policy manufacture an empty replacement for the tree the
	// review is supposed to judge.
	before, err := p.identity(ctx, c)
	if err != nil {
		return remote.Identity{}, "", err
	}
	resumed, err := p.backend.Acquire(ctx, remote.AcquireRequest{
		Claim:           c.Claim(),
		Branch:          c.Branch,
		BaseSHA:         c.CycleBaseSHA,
		ProfileRevision: before.ProfileRevision,
	})
	if err != nil {
		return remote.Identity{}, "", err
	}
	if !resumed.Complete() {
		return remote.Identity{}, "", fmt.Errorf("%w: %s", remote.ErrIdentityMissing, c.Claim())
	}
	if resumed != before {
		return remote.Identity{}, "", fmt.Errorf("%w: review resume moved %s from sandbox %s to %s",
			remote.ErrClaimMismatch, c.Claim(), before.SandboxID, resumed.SandboxID)
	}
	return resumed, c.TargetBranch, nil
}
