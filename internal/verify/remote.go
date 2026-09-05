package verify

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// The v2 publication check: SPEC §9.7's three legs, proved for a run BEN did not
// host and did not watch.
//
// v1 (Checker, above) reads its git legs off the daemon's own disk, which is
// sound precisely because BEN created the worktree, BEN owns the base
// repository, and nothing else wrote either. A run that happens somewhere else
// removes that anchor and leaves nothing in its place: the sandbox's response,
// its transcript, its filesystem, and any git command executed through it are
// all authored by the thing being judged. So the legs keep their meaning and
// change their sources — a daemon-side pin recorded before the run could start,
// the canonical remote head read with the daemon's own credential, and the forge
// read with the daemon's own tracker credential (core.RemotePublishFacts,
// core.RemotePRSource).
//
// **Leg 2 is not missing; it has collapsed into leg 1's source.** v1 asks
// separately whether origin carries the local commits, because the local branch
// and origin's are two different objects and a partial push is the ordinary way
// they differ. Here there is no local branch: the candidate *is* the canonical
// remote head, read by the daemon, so "the branch is on the remote with those
// commits" is a property of how the fact was obtained rather than a comparison
// this code performs. What is left to check is that the head descends from the
// pin, and that the forge agrees about which commit is published.
//
// Everything else is unchanged, including what a verdict means and who decides
// what to do about it (§9.6, §9.7).

// Named refusals, all fail-closed: none of them is a verdict, because a
// verification that could not be completed must never be read as success
// (SPEC §9.7).
var (
	// ErrRemoteFactsUnbound refuses facts that answer a different question:
	// another claim, another attempt, or another verification.
	//
	// It is the check that makes the whole binding worth carrying. A remote
	// branch and its pull request outlive the claim that pushed them, so last
	// cycle's publication is complete evidence for this cycle unless something
	// compares the epochs — and an attempt's verdict must not settle the attempt
	// after it. A fact source that returns an answer bound to something else has
	// not answered, and reading it anyway is how a run that did nothing inherits
	// a `done`.
	ErrRemoteFactsUnbound = errors.New("verify: remote facts are bound to a different claim, run or verification")

	// ErrRemoteMirrorStale refuses facts the fact source did not observe during
	// this verification (core.RemotePublishFacts.Fetched).
	//
	// A head read at an earlier tick is not this tick's fact: the branch may have
	// been force-pushed away since, and a publication proved against a memory is
	// proved against a state of the world nobody is claiming still holds.
	ErrRemoteMirrorStale = errors.New("verify: remote facts were not observed during this verification")

	// ErrRemoteRepositoryMismatch refuses facts read from a repository other than
	// the one this daemon verifies. Ordering a candidate against a pin from
	// another repository is not a check that can fail — the objects are not
	// comparable — so it must not be a check that can pass.
	ErrRemoteRepositoryMismatch = errors.New("verify: remote facts were read from a different repository")

	// ErrRemoteBranchUnexpected refuses facts about a branch other than the one
	// this claim's key derives. The branch is the binding between an issue and
	// the commits verified for it; a fact source that chose a different one has
	// verified somebody else's work.
	ErrRemoteBranchUnexpected = errors.New("verify: remote facts are about a different branch")

	// ErrRemoteClaimUnpinned refuses facts carrying no claim-time base. Without a
	// pin every candidate descends from nothing, which turns a run that pushed
	// somebody else's commits into a publication.
	ErrRemoteClaimUnpinned = errors.New("verify: remote facts carry no claim-time base")

	// ErrRemotePRContract refuses a pull request that breaks the query's
	// contract: an incomplete result, a state other than open, a branch or
	// repository other than the one asked for, or no positive enumeration of
	// its closing-issue linkage.
	//
	// Its v1 counterparts (ErrPRNotOpen, ErrPRBranchMismatch) exist for the same
	// reason and it is worth restating: these are not weak evidence, they are an
	// adapter that answered a question nobody asked, and there is no reading of
	// the answer to pick.
	ErrRemotePRContract = errors.New("verify: the forge returned a pull request that does not answer the query")

	// ErrNoRemoteVerifier refuses to verify a remote attempt with the local
	// checker. See SelectPublication: the fallback is not a degradation, it is
	// the sandbox verifying itself.
	ErrNoRemoteVerifier = errors.New("verify: a remote attempt has no remote verifier configured")
)

// RemoteFactSource is the daemon-side git seam, satisfied structurally by
// *mirror.Mirror. Declared here rather than taken from core for the reason the
// v1 Workspaces seam gives: the consumer owns the shape of what it needs, and
// §6.1's provider interface is not the place to grow it.
type RemoteFactSource interface {
	RemoteFacts(ctx context.Context, run core.RemoteRunRef) (core.RemotePublishFacts, error)
}

// RemoteClaimSource supplies the immutable target/base pair selected before a
// remote run could start. It is separate from RemoteFactSource so verification
// cannot substitute workflow-scoped configuration for claim-scoped authority.
type RemoteClaimSource interface {
	Claim(ctx context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error)
}

// RemotePRs is leg 3's seam, satisfied by core.RemotePRSource. One method, and a
// read: a verifier one interface away from a write could publish the evidence it
// is judging (SPEC §8.1).
type RemotePRs interface {
	RemotePR(ctx context.Context, q core.RemotePRQuery) (*core.RemotePR, error)
}

// RemoteExpectation is what the daemon — not the run, and not the forge —
// asserts a publication must join.
//
// It is constructor state rather than a per-call argument on purpose. The
// target branch is deliberately absent: it belongs to the immutable claim
// record and may differ across concurrent claims after default movement.
type RemoteExpectation struct {
	// Repository is the credential-free identity of the repository BEN fetches
	// and reads — mirror.Mirror.Repository().
	Repository string
}

// RemoteResult is the v2 verdict plus the facts it was reached from.
//
// The facts travel with the verdict because a v2 verdict is not self-explanatory
// the way a v1 one is: the operator reading a needs-review comment cannot look at
// the workspace, because there is no workspace. What they get instead is the
// bounded audit record — repository identity, branch, base, observed head, the
// claim and attempt it was all bound to — and nothing else. No URLs, no
// credentials, no transcript, no sandbox output.
type RemoteResult struct {
	Verdict Verdict
	PRURL   string
	Detail  string
	Facts   core.RemotePublishFacts
}

// Result projects onto v1's shape, which is what the orchestrator routes on.
// Routing is deliberately identical: the evidence has moved, the state machine
// has not (SPEC §9.2, §9.6).
func (r RemoteResult) Result() Result {
	return Result{Verdict: r.Verdict, PRURL: r.PRURL, Detail: r.Detail}
}

// RemoteChecker performs the v2 check.
type RemoteChecker struct {
	claims RemoteClaimSource
	facts  RemoteFactSource
	prs    RemotePRs
	expect RemoteExpectation
}

// NewRemote refuses a checker missing any of its four inputs. A verifier that
// silently skipped a leg — or that compared a publication against an empty
// expectation, which every publication satisfies — would report published on
// less evidence than the spec names.
func NewRemote(claims RemoteClaimSource, facts RemoteFactSource, prs RemotePRs, expect RemoteExpectation) (*RemoteChecker, error) {
	switch {
	case claims == nil || facts == nil || prs == nil:
		return nil, errors.New("verify: the remote claim, fact, and forge seams are required")
	case expect.Repository == "":
		return nil, errors.New("verify: a remote verifier must know which repository it verifies")
	}
	return &RemoteChecker{claims: claims, facts: facts, prs: prs, expect: expect}, nil
}

// Verify reads the legs in order and returns the first verdict the evidence
// settles, together with the facts it settled from.
//
// Errors are returned without a verdict: verification that cannot be completed
// is never success (SPEC §9.7 fail closed), and the caller parks or retries
// rather than guessing. A transient credential failure arrives here as the
// source's own error with its class intact, which is what lets §9.7's one
// exception — retry in `verifying`, once per poll tick — be a decision the
// caller can still make.
func (c *RemoteChecker) Verify(ctx context.Context, issue core.Issue, run core.RemoteRunRef) (RemoteResult, error) {
	if !run.Complete() {
		return RemoteResult{}, fmt.Errorf(
			"%w: a verification names a claim, a run and a verification attempt (got %+v)", ErrRemoteFactsUnbound, run)
	}
	if issue.Identifier != run.Claim.Issue {
		return RemoteResult{}, fmt.Errorf("%w: asked about issue %s under a claim for issue %s",
			ErrRemoteFactsUnbound, issue.Identifier, run.Claim.Issue)
	}
	branch := remoteBranch(run.Claim.Key)
	claim, err := c.claims.Claim(ctx, run.Claim)
	if err != nil {
		return RemoteResult{}, fmt.Errorf("verify: reading the claim target for %s: %w", issue.Identifier, err)
	}
	if err := c.admitClaim(claim, run.Claim, branch); err != nil {
		return RemoteResult{}, err
	}

	facts, err := c.facts.RemoteFacts(ctx, run)
	if err != nil {
		return RemoteResult{}, fmt.Errorf("verify: reading daemon-side git evidence for %s: %w", issue.Identifier, err)
	}
	if err := c.admit(facts, run, branch, claim); err != nil {
		return RemoteResult{}, err
	}

	// Leg 1 — the canonical remote carries commits this claim's run added, and
	// they descend from the pin taken before it started. Each failure is a
	// contradiction rather than unfinished work: nothing a continuation could
	// finish publishing exists, because whatever the run built is gone with the
	// sandbox that built it.
	switch {
	case facts.RemoteHead == "":
		return contradictedRemote(facts, "branch %s is not on %s: the run published nothing",
			branch, facts.Repository), nil
	case facts.RemoteHead == facts.BaseSHA:
		return contradictedRemote(facts, "branch %s is still at its claim-time base %s: the run added no commits",
			branch, short(facts.BaseSHA)), nil
	case !facts.DescendsBase:
		// A force push, a rewritten history, or a branch of somebody else's
		// commits. "Advanced" is a claim about *this claim's* run, and a head the
		// pin is not an ancestor of cannot support it.
		return contradictedRemote(facts,
			"branch %s on %s is at %s, which does not descend from the claim-time base %s: history was rewritten",
			branch, facts.Repository, short(facts.RemoteHead), short(facts.BaseSHA)), nil
	}

	// Leg 3 — an open pull request that joins this publication. Reached only for
	// a claim whose commits BEN has already seen on the canonical remote, so the
	// forge read is not what establishes that anything was published.
	pr, err := c.prs.RemotePR(ctx, core.RemotePRQuery{
		Issue:      issue,
		Repository: c.expect.Repository,
		Branch:     branch,
	})
	if err != nil {
		// core.ErrRemotePRIncomplete and core.ErrRemotePRAmbiguous arrive here,
		// and both must stay errors. An enumeration that did not finish is not
		// "no pull request", and two candidates are not one.
		return RemoteResult{}, fmt.Errorf("verify: reading the pull request for %s: %w", branch, err)
	}
	if pr == nil {
		return incompleteRemote(facts, "branch %s is on %s at %s but has no open pull request",
			branch, facts.Repository, short(facts.RemoteHead)), nil
	}
	if err := c.admitPR(pr, branch); err != nil {
		return RemoteResult{}, err
	}
	if verdict, ok := c.joins(pr, facts, issue, branch, claim.TargetBranch); !ok {
		return verdict, nil
	}
	return RemoteResult{Verdict: VerdictPublished, PRURL: pr.URL, Facts: facts}, nil
}

// admit refuses facts that do not answer this verification's question.
//
// Every check here is about the *provenance* of the evidence rather than about
// what it says, which is why they all precede leg 1 and all produce errors
// rather than verdicts. A fact bound to another claim is not weak evidence for
// this one; it is evidence about something else.
func (c *RemoteChecker) admitClaim(claim core.RemoteClaim, ref core.RemoteClaimRef, branch string) error {
	switch {
	case claim.Ref != ref:
		return fmt.Errorf("%w: asked for claim %+v, answered for %+v", ErrRemoteFactsUnbound, ref, claim.Ref)
	case claim.Repository != c.expect.Repository:
		return fmt.Errorf("%w: claim reads %q, this daemon verifies %q",
			ErrRemoteRepositoryMismatch, claim.Repository, c.expect.Repository)
	case claim.Branch != branch:
		return fmt.Errorf("%w: claim records %q, expected %q", ErrRemoteBranchUnexpected, claim.Branch, branch)
	case claim.BaseSHA == "":
		return fmt.Errorf("%w: claim %d of issue %s", ErrRemoteClaimUnpinned, ref.Epoch, ref.Issue)
	case claim.TargetBranch == "":
		return fmt.Errorf("verify: claim %d of issue %s carries no target branch", ref.Epoch, ref.Issue)
	}
	return nil
}

func (c *RemoteChecker) admit(facts core.RemotePublishFacts, run core.RemoteRunRef, branch string, claim core.RemoteClaim) error {
	switch {
	case facts.Run != run:
		return fmt.Errorf("%w: asked for %+v, answered for %+v", ErrRemoteFactsUnbound, run, facts.Run)
	case facts.Repository != c.expect.Repository:
		return fmt.Errorf("%w: read %q, this daemon verifies %q",
			ErrRemoteRepositoryMismatch, facts.Repository, c.expect.Repository)
	case facts.Branch != branch:
		return fmt.Errorf("%w: read %q, this claim's branch is %q", ErrRemoteBranchUnexpected, facts.Branch, branch)
	case facts.BaseSHA == "":
		return fmt.Errorf("%w: claim %d of issue %s", ErrRemoteClaimUnpinned, run.Claim.Epoch, run.Claim.Issue)
	case facts.BaseSHA != claim.BaseSHA:
		return fmt.Errorf("%w: facts use base %s, claim records %s",
			ErrRemoteFactsUnbound, facts.BaseSHA, claim.BaseSHA)
	case !facts.Fetched:
		return fmt.Errorf("%w: branch %s of %s", ErrRemoteMirrorStale, branch, facts.Repository)
	}
	return nil
}

// admitPR refuses a pull request that does not answer the query — the forge's
// contract rather than the publication's merits.
func (c *RemoteChecker) admitPR(pr *core.RemotePR, branch string) error {
	switch {
	case pr.Number <= 0 || pr.URL == "" || pr.HeadRepository == "" || pr.HeadSHA == "" || pr.BaseBranch == "":
		return fmt.Errorf("%w: the forge returned incomplete facts for pull request #%d", ErrRemotePRContract, pr.Number)
	case pr.State != "open":
		return fmt.Errorf("%w: #%d on %s is %q, and the query is for open pull requests",
			ErrRemotePRContract, pr.Number, branch, pr.State)
	case pr.HeadBranch != branch:
		return fmt.Errorf("%w: #%d is on %q, asked for %q", ErrRemotePRContract, pr.Number, pr.HeadBranch, branch)
	case pr.Repository != c.expect.Repository:
		return fmt.Errorf("%w: #%d belongs to %q, asked about %q",
			ErrRemotePRContract, pr.Number, pr.Repository, c.expect.Repository)
	case pr.LinkStatus != core.RemoteIssueLinkStated:
		return fmt.Errorf("%w: #%d did not positively enumerate its closing issues", ErrRemotePRContract, pr.Number)
	}
	for _, issue := range pr.LinkedIssues {
		if issue == "" {
			return fmt.Errorf("%w: #%d returned an empty closing-issue identifier", ErrRemotePRContract, pr.Number)
		}
	}
	return nil
}

// joins checks that the open pull request is *this* publication's, returning a
// contradiction verdict and false when it is not.
//
// Verdicts rather than errors, because each of these is a real observation of a
// real pull request that says the publication is not what was claimed —
// unlike admitPR's cases, which are the forge answering a different question.
func (c *RemoteChecker) joins(pr *core.RemotePR, facts core.RemotePublishFacts, issue core.Issue, branch, targetBranch string) (RemoteResult, bool) {
	switch {
	case pr.HeadRepository != c.expect.Repository:
		// A fork. The commits BEN verified are on the canonical remote; this pull
		// request proposes somebody else's copy of the branch, which BEN has not
		// read and cannot vouch for.
		return contradictedRemote(facts, "pull request #%d publishes %s from %s, not from %s",
			pr.Number, branch, pr.HeadRepository, c.expect.Repository), false

	case pr.BaseBranch != targetBranch:
		// The review gate is configured per target branch (SPEC §3.4, §10.1): a
		// pull request against an unprotected branch is a merge nobody has to
		// approve, so this is a contradiction and not a detail.
		return contradictedRemote(facts, "pull request #%d targets %s, not %s",
			pr.Number, pr.BaseBranch, targetBranch), false

	case pr.HeadSHA != facts.RemoteHead:
		// The forge and the canonical remote disagree about what is published.
		// This is the shape of a stale pull request left by an earlier claim, and
		// of a branch that moved while the two were being read; neither may pass,
		// and neither is distinguishable from the other from here.
		return contradictedRemote(facts, "pull request #%d proposes %s and %s is at %s: the forge and %s disagree",
			pr.Number, short(pr.HeadSHA), branch, short(facts.RemoteHead), facts.Repository), false

	case !slices.Contains(pr.LinkedIssues, issue.Identifier):
		// The forge enumerated what this pull request closes and this issue is
		// not in it. Silence was refused in admitPR; this is a positive fact whose
		// contents contradict the claimed issue binding.
		return contradictedRemote(facts, "pull request #%d closes %v, not issue %s",
			pr.Number, pr.LinkedIssues, issue.Identifier), false
	}
	return RemoteResult{}, true
}

// remoteBranch derives the canonical issue branch from a claim's workspace key.
//
// Derived here as well as in the fact source, deliberately: the two must agree,
// and the way to make disagreement visible is for each to compute it rather than
// for one to accept the other's answer.
func remoteBranch(key string) string { return "ben/" + key }

func contradictedRemote(facts core.RemotePublishFacts, format string, args ...any) RemoteResult {
	return RemoteResult{Verdict: VerdictContradicted, Detail: fmt.Sprintf(format, args...), Facts: facts}
}

func incompleteRemote(facts core.RemotePublishFacts, format string, args ...any) RemoteResult {
	return RemoteResult{Verdict: VerdictIncomplete, Detail: fmt.Sprintf(format, args...), Facts: facts}
}
