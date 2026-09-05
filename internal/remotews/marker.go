package remotews

import (
	"context"
	"errors"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// SPEC §9.10's workspace precondition on a substrate where the run journal
// already is one.
//
// The local strategy needs a marker file because a pid is not written down
// anywhere else: the orchestrator writes the marker before the launch, the
// runner upgrades it with evidence once the process exists, and a crash between
// those two leaves the "unknown launch" state a human has to resolve.
//
// The remote runner has no such window, and that is the whole reason this
// strategy does not add a second file. remote.Journal's first rule is *identity
// before the act*: the sandbox-scoped run reference is on disk before a start may
// be attempted, and `Dispatched` is set before the call rather than after it. So
// the journal already answers the marker's three questions exactly:
//
//   - no journal → nothing was reserved, so nothing can be running (Absent);
//   - a journal that was never dispatched → the mark precedes the call, so
//     nothing was started (Absent);
//   - a dispatched journal → a run may be live, and it can be *named*, which is
//     the identified state (RunMarkerIdentified).
//
// There is no fourth state. `RunMarkerUnknownLaunch` covers a local crash after
// exec and before the evidence upgrade; here the evidence is written before the
// dispatch, so the shape that produces it does not exist. Writing a marker of our
// own would only add a way for the two to disagree — and the one that disagreed
// would be the file with no run behind it.

// BeginRunMarkerFor records that a run may be live in this issue's workspace.
//
// A no-op with a precondition, deliberately. What the orchestrator needs from
// this call is that the workspace reads as occupied from the moment a launch may
// have happened, and remote.Journal.Reserve — which runs inside Runner.Start,
// before any backend call — already establishes it durably. What this checks is
// the thing that would make the launch meaningless: a cycle record that is not
// pinned describes a claim with no verification base, and an attempt started
// against one could not be verified whatever it did.
func (p *Provider) BeginRunMarkerFor(issue core.Issue) error {
	key, err := p.key(issue)
	if err != nil {
		return err
	}
	c, err := p.load(key, issue.Identifier)
	if err != nil {
		return err
	}
	if c.State != cyclePinned || c.BaseSHA == "" {
		return fmt.Errorf("%w: issue %s is %s and a launch needs a pinned verification base",
			ErrCycleState, issue.Identifier, c.State)
	}
	return nil
}

// ReadRunMarkerFor reports what the previous tenure left behind, read off the
// run journal (see the file comment).
func (p *Provider) ReadRunMarkerFor(issue core.Issue) (core.RunMarker, error) {
	key, err := p.key(issue)
	if err != nil {
		return core.RunMarker{}, err
	}
	c, err := p.load(key, issue.Identifier)
	switch {
	case errors.Is(err, ErrNoCycle):
		return core.RunMarker{State: core.RunMarkerAbsent}, nil
	case err != nil:
		return core.RunMarker{}, err
	}
	journal, err := remote.OpenJournal(p.journals, c.Claim())
	switch {
	case errors.Is(err, remote.ErrNoRecord):
		return core.RunMarker{State: core.RunMarkerAbsent}, nil
	case err != nil:
		// A journal that exists and cannot be read is the one answer that is not a
		// verdict. RunMarkerUnreadable never frees a workspace (core.RunMarkerState),
		// which is the direction that costs a tick rather than a second agent.
		return core.RunMarker{}, err
	}
	if !journal.Record().Dispatched {
		return core.RunMarker{State: core.RunMarkerAbsent}, nil
	}
	return core.RunMarker{
		State:    core.RunMarkerIdentified,
		Evidence: core.RunEvidence{Scheme: EvidenceScheme, ID: c.Key},
	}, nil
}

// ClearRunMarkerFor frees the workspace by retiring the run journal.
//
// It re-asks the backend rather than trusting its caller, and the extra round
// trip is the point. §9.10 admits exactly one route to removal — positive
// evidence of absence — and on this substrate that evidence is an explicit
// domain-quiet observation from a reachable backend, not a stream that ended and
// not a status remembered from a moment ago. An unreachable control plane leaves
// the journal standing and the clear owed, which is the same posture the local
// path takes on an unconfirmed stop.
func (p *Provider) ClearRunMarkerFor(issue core.Issue) error {
	key, err := p.key(issue)
	if err != nil {
		return err
	}
	c, err := p.load(key, issue.Identifier)
	switch {
	case errors.Is(err, ErrNoCycle):
		return nil
	case err != nil:
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.statusWait)
	defer cancel()
	_, _, err = p.settle(ctx, c)
	return err
}

// RunGone answers §9.10's precondition for a run this process never started:
// orchestrator.Config.RunGone, wired to this strategy by the assembly.
//
// `true` only on proof of absence. A scheme this substrate does not own, a
// journal that cannot be read, a control plane that will not answer — every one
// of them is `false`, which recovery reads as possibly live. The asymmetry is
// the contract: `false` costs a retained claim and another tick, `true` over a
// live run puts a second agent in one sandbox.
//
// It composes its own bounded context because the seam has none, and a recovery
// pass must not be able to hang on an unreachable endpoint.
func (p *Provider) RunGone(evidence core.RunEvidence) (bool, error) {
	return p.runGone(core.Issue{}, evidence, 0, nil)
}

// ResolveRun is RunGone's authority-gated sibling. It may replay an unanswered
// Start, so its caller supplies the standing approval event from the same
// tracker history that selected the orphan/backoff route. Before touching the
// process backend, the provider compares that event with both the persisted
// workspace cycle and a fresh CycleSource read. This is the replay equivalent
// of Bind's last-moment reapproval check.
func (p *Provider) ResolveRun(
	issue core.Issue,
	evidence core.RunEvidence,
	expectedApproval int64,
	resolve remote.StartResolver,
) (bool, error) {
	if resolve == nil {
		return false, fmt.Errorf("%w: no exact Start resolver", remote.ErrReplayUnavailable)
	}
	if issue.Identifier == "" {
		return false, fmt.Errorf("%w: replay issue identifier is empty", ErrCycleState)
	}
	if expectedApproval <= 0 {
		return false, fmt.Errorf("%w: issue %s", ErrApprovalUnknown, issue.Identifier)
	}
	return p.runGone(issue, evidence, expectedApproval, resolve)
}

func (p *Provider) runGone(
	issue core.Issue,
	evidence core.RunEvidence,
	expectedApproval int64,
	resolve remote.StartResolver,
) (bool, error) {
	if evidence.Scheme != EvidenceScheme {
		return false, fmt.Errorf("%w: %q", ErrEvidenceScheme, evidence.Scheme)
	}
	if evidence.ID == "" {
		return false, fmt.Errorf("%w: evidence names no workspace cycle", ErrEvidenceScheme)
	}
	identifier := ""
	if resolve != nil {
		identifier = issue.Identifier
	}
	c, err := p.load(evidence.ID, identifier)
	switch {
	case errors.Is(err, ErrNoCycle):
		// No cycle record, so no address, so nothing this daemon can name is
		// running. Absence here is a fact about BEN's own store rather than about
		// the backend, which is why it may be read as gone: a run whose cycle was
		// retired had its journal retired first (ClearRunMarkerFor).
		return true, nil
	case err != nil:
		return false, err
	}
	journal, err := remote.OpenJournal(p.journals, c.Claim())
	switch {
	case errors.Is(err, remote.ErrNoRecord):
		return true, nil
	case err != nil:
		return false, err
	}
	rec := journal.Record()
	if !rec.Dispatched {
		return true, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), p.statusWait)
	defer cancel()
	if resolve != nil {
		standingApproval, err := p.cycles.WorkspaceCycle(ctx, issue)
		if err != nil {
			return false, fmt.Errorf("remotews: resolving the standing approval for issue %s before replay: %w",
				issue.Identifier, err)
		}
		if standingApproval <= 0 {
			return false, fmt.Errorf("%w: issue %s", ErrApprovalUnknown, issue.Identifier)
		}
		if c.Approval != expectedApproval || standingApproval != expectedApproval {
			return false, fmt.Errorf(
				"%w: issue %s replay expects approval %d, the record names %d, and the standing approval is %d",
				ErrCycleState, issue.Identifier, expectedApproval, c.Approval, standingApproval,
			)
		}
	}
	st, err := p.processes.Status(ctx, rec.ProcessRef())
	if resolve != nil && errors.Is(err, remote.ErrProcessUnresolved) {
		st, err = resolve(ctx, rec.ProcessRef())
	}
	switch {
	case errors.Is(err, remote.ErrNoProcess):
		return true, nil
	case err != nil:
		return false, err
	}
	if resolve != nil {
		if st.BackendRunID == "" {
			return false, fmt.Errorf("%w: resolving %s returned no permanent run id",
				remote.ErrProcessUnresolved, rec.ProcessRef())
		}
		// Active-slot recovery learns the backend id without going through Runner;
		// retain it here so a later terminal observation does not need creation-key
		// replay after the backend's bounded idempotency window.
		if err := journal.Observe(context.WithoutCancel(ctx), st); err != nil {
			return false, err
		}
	}
	return remote.MayReuse(st), nil
}
