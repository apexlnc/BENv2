// Package verify implements the SPEC §9.7 publish-evidence check: the three
// legs of git and tracker fact that separate a run which published its work
// from one that merely claims to have.
//
// The check answers from evidence alone and never from the agent's own
// account, which is the §3.5 invariant this package exists to enforce. It
// reports a verdict, not an action: an unpublished-but-clean run and an
// unpublished run out of turns produce the same evidence and route
// differently (§9.6), and only the orchestrator knows which it is holding.
package verify

import (
	"context"
	"errors"
	"fmt"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// ErrPRNotOpen refuses a pull request the tracker returned in any state but
// open. FindPR's contract is open PRs only (SPEC §8.2, §9.7) precisely so a
// rejected PR from an earlier attempt cannot satisfy leg 3 — so a closed one
// arriving here is a broken adapter, not weak evidence, and reading it either
// way would be guessing.
var ErrPRNotOpen = errors.New("verify: tracker returned a pull request that is not open")

// ErrPRBranchMismatch refuses a pull request published on some branch other
// than the workspace's. FindPR is asked for one branch; a PR on another is a
// different piece of work, and accepting it would let an unrelated open PR
// supply leg 3 for a branch that never got one. Same reasoning as
// ErrPRNotOpen: the tracker broke its contract, so there is no reading to pick.
var ErrPRBranchMismatch = errors.New("verify: tracker returned a pull request for a different branch")

// ErrRemoteUnprobed refuses git facts whose remote legs were never read on a
// branch that did advance past its base. The provider skips the probe only
// when leg 1 has already failed (core.PublishFacts.AdvancedPastBase), so this
// is a provider contract violation — and the alternative is reading an
// unprobed RemoteHead as "not pushed", which is absence standing in for
// evidence (SPEC §9.10).
var ErrRemoteUnprobed = errors.New("verify: git facts carry no remote observation")

// Verdict is what the evidence says. It is deliberately not what to do about
// it: routing needs the run's outcome too (SPEC §9.6, §9.7).
type Verdict int

const (
	// VerdictUnknown is the zero value and never an answer. It exists so that
	// the Result returned alongside an error cannot be read as success: §9.7
	// says verification errors never count as success, and a zero value
	// meaning "published" would make that true only by the caller's diligence.
	VerdictUnknown Verdict = iota
	// VerdictPublished — all three legs of the §9.7 evidence check hold.
	VerdictPublished
	// VerdictIncomplete — commits exist and descend from the claim-time base,
	// but publishing did not finish. A clean exit here has somewhere to go:
	// the continuation track (§9.6).
	VerdictIncomplete
	// VerdictContradicted — the evidence contradicts any claim of success, and
	// no continuation helps because there is nothing to finish publishing.
	VerdictContradicted
)

func (v Verdict) String() string {
	switch v {
	case VerdictUnknown:
		return "unknown"
	case VerdictPublished:
		return "published"
	case VerdictIncomplete:
		return "incomplete"
	case VerdictContradicted:
		return "contradicted"
	default:
		return fmt.Sprintf("Verdict(%d)", int(v))
	}
}

// Result is the verdict plus what the caller needs to say about it: the PR
// link for the publish milestone comment, and one operator-facing line for a
// needs-review comment. Detail is set for every verdict except
// VerdictPublished, where the URL is the whole story.
type Result struct {
	Verdict Verdict
	PRURL   string
	Detail  string
}

// Workspaces is the git-facts seam, satisfied structurally by
// *workspace.Provider. Declared here rather than taken from core so that
// §6.1's closed provider interface does not grow a fourth method every
// strategy would owe (see core.PublishFacts).
type Workspaces interface {
	PublishFacts(ctx context.Context, ws core.Workspace) (core.PublishFacts, error)
}

// Tracker is the publish-evidence seam, satisfied by core.TrackerAdapter. One
// method, because leg 3 is the only tracker fact verification reads — and a
// verifier that could write would be one §8.1 boundary away from posting its
// own evidence.
type Tracker interface {
	FindPR(ctx context.Context, issue core.Issue, branch string) (*core.PR, error)
}

// Checker performs the §9.7 check.
type Checker struct {
	workspaces Workspaces
	tracker    Tracker
}

// New refuses a Checker missing either seam. A verifier that silently skipped
// a leg would report published on less evidence than the spec names, which is
// the one failure mode this package must not have.
func New(workspaces Workspaces, tracker Tracker) (*Checker, error) {
	if workspaces == nil || tracker == nil {
		return nil, errors.New("verify: both the workspace and tracker seams are required")
	}
	return &Checker{workspaces: workspaces, tracker: tracker}, nil
}

// Verify reads the three legs in order and returns the first verdict the
// evidence settles. Errors are returned unwrapped into a verdict: verification
// that cannot be completed is never success (SPEC §9.7 fail closed), and the
// caller parks rather than guessing.
//
// Leg order is cost as well as logic — the git legs are local and answer most
// runs, and the tracker read only happens for a run whose commits are already
// on origin.
func (c *Checker) Verify(ctx context.Context, issue core.Issue, ws core.Workspace) (Result, error) {
	facts, err := c.workspaces.PublishFacts(ctx, ws)
	if err != nil {
		return Result{}, fmt.Errorf("verify: reading git evidence for %s: %w", issue.Identifier, err)
	}

	// Leg 1 — branch advanced, and descends from the claim-time base. Each of
	// these failures is a contradiction rather than unfinished work: there are
	// no commits to publish, so no continuation can finish publishing them.
	switch {
	case facts.Head == "":
		return contradicted("branch %s does not exist: the run left no commits", ws.Branch), nil
	case facts.Head == ws.BaseSHA:
		return contradicted("branch %s is still at its claim-time base %s: the run added no commits",
			ws.Branch, short(ws.BaseSHA)), nil
	case !facts.DescendsBase:
		// A rewritten or force-pushed branch. "Advanced" is a claim about
		// *this daemon's* runs (§9.7), and a head the base is not an ancestor
		// of cannot support it — the commits there may be anyone's.
		return contradicted("branch %s at %s does not descend from its claim-time base %s: history was rewritten",
			ws.Branch, short(facts.Head), short(ws.BaseSHA)), nil
	}

	// Leg 1 held, so core.PublishFacts.AdvancedPastBase held, so the provider
	// owed a probe. Without one, RemoteHead "" would read as "not pushed" —
	// absence of a fact standing in for a fact (§9.10). Refuse instead.
	if !facts.RemoteProbed {
		return Result{}, fmt.Errorf("%w: branch %s advanced past its base but origin was never asked",
			ErrRemoteUnprobed, ws.Branch)
	}

	// Leg 2 — the branch is on origin, carrying those commits. Work exists but
	// is not published: a clean exit routes to the continuation track, which
	// re-dispatches with a prompt to finish publishing (§9.6, B09 acceptance 1).
	switch {
	case facts.RemoteHead == "":
		return incomplete("branch %s has commits at %s but is not on origin",
			ws.Branch, short(facts.Head)), nil
	case !facts.RemoteHasHead:
		return incomplete("origin's %s is at %s and does not carry the local head %s: the push was partial",
			ws.Branch, short(facts.RemoteHead), short(facts.Head)), nil
	}

	// Leg 3 — an open PR on the branch.
	pr, err := c.tracker.FindPR(ctx, issue, ws.Branch)
	if err != nil {
		return Result{}, fmt.Errorf("verify: finding the pull request for %s: %w", ws.Branch, err)
	}
	if pr == nil {
		return incomplete("branch %s is pushed to origin but has no open pull request", ws.Branch), nil
	}
	if pr.State != "open" {
		return Result{}, fmt.Errorf("%w: #%d on %s is %q", ErrPRNotOpen, pr.Number, ws.Branch, pr.State)
	}
	if pr.Branch != ws.Branch {
		return Result{}, fmt.Errorf("%w: #%d is on %q, asked for %q",
			ErrPRBranchMismatch, pr.Number, pr.Branch, ws.Branch)
	}
	return Result{Verdict: VerdictPublished, PRURL: pr.URL}, nil
}

func contradicted(format string, args ...any) Result {
	return Result{Verdict: VerdictContradicted, Detail: fmt.Sprintf(format, args...)}
}

func incomplete(format string, args ...any) Result {
	return Result{Verdict: VerdictIncomplete, Detail: fmt.Sprintf(format, args...)}
}

// short abbreviates a SHA for operator-facing detail lines, leaving anything
// that is not one alone.
func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
