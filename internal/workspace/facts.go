package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// ResolveWorkspace names the workspace an issue's work lives in and reads back
// its pinned claim epoch/base, preparing nothing (SPEC §9.10).
//
// Recovery needs a `core.Workspace` to ask §9.7's question, and it needs one
// *before* any verdict says an attempt is owed — so calling Prepare would run
// this attempt's hooks, create a worktree and mint a pin on the strength of a
// decision nobody has made yet. This reads and nothing else: no fetch, no
// update-ref, no worktree, no hooks.
//
// `false` is non-authorizing: the record is absent or pending, so there is no
// pinned epoch/base pair against which §9.7 may be asked. The caller reads the
// closed ClaimBase state to distinguish resume from epoch-faulted park (§9.10).
//
// Fresh storage includes an absent base repository. Claim-record absence is
// established before any reachability ref is read, so it returns false without
// asking Git. Once a record names a pinned or outgoing base, however, its ref
// store is part of that atomic safety fact and missing storage fails closed.
//
// The pair is deliberately the only safety state read: minting one here would
// redefine the historical question recovery is asking.
func (p *Provider) ResolveWorkspace(ctx context.Context, issue core.Issue) (core.Workspace, bool, error) {
	if issue.Identifier == "" {
		return core.Workspace{}, false, fmt.Errorf("%w: issue identifier is empty", ErrPathEscape)
	}
	key := Key(issue.Identifier)

	// Serialized with Prepare and Dispose for this issue, as PublishFacts is: a
	// pin read out from under a concurrent prepare would pair a resolved base with
	// a branch that has since moved.
	unlock := p.lock(key)
	defer unlock()

	claimBase, err := p.readClaimBaseLocked(ctx, key)
	if err != nil {
		return core.Workspace{}, false, err
	}
	if claimBase.State != core.ClaimBasePinned {
		// A genuinely absent base repository is fresh storage. Any entry at
		// that path, however, must be a readable ref store before "no pinned
		// workspace" can be asserted; otherwise damage would masquerade as
		// absence (#183 preserves #16's fail-closed boundary).
		present, err := p.baseRepoPresent()
		if err != nil {
			return core.Workspace{}, false, err
		}
		if present {
			if _, _, err := p.revParse(ctx, baseRefPrefix+key); err != nil {
				return core.Workspace{}, false, err
			}
		}
		return core.Workspace{}, false, nil
	}
	return core.Workspace{
		// The provider's own path set, not a path this function spells: #114 made the
		// worktree one of three the provider owns, and a second definition here would
		// be a second answer to where a workspace is.
		WorkspacePaths: p.pathsFor(key),
		Key:            key,
		Branch:         branchPrefix + key,
		ClaimEpoch:     claimBase.Epoch,
		BaseSHA:        claimBase.BaseSHA,
	}, true, nil
}

// baseRepoPresent reports whether <workflow>/base.git is there to be read.
// Lstat makes a dangling symlink an existing but unusable entry rather than
// fresh storage; revParse supplies the subsequent readability verdict.
func (p *Provider) baseRepoPresent() (bool, error) {
	if _, err := os.Lstat(p.baseDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: cannot tell whether base repository %s exists: %v",
			ErrBaseRepoState, p.baseDir, err)
	}
	return true, nil
}

// PublishFacts reads the two git legs of the §9.7 evidence check: whether the
// issue branch advanced past its claim-time base and descends from it, and
// whether origin carries those commits. The third leg (an open PR) is the
// tracker's, and the verdict is the caller's — the same facts read differently
// depending on how the run ended (SPEC §9.6, §9.7).
//
// Every failure is an error rather than a missing fact. Verification that
// cannot be completed must never read as success (SPEC §9.7 fail closed), and
// a fact this returns as absent will be *acted on* as absent, so the
// distinction is exactly the one revParse and isAncestor already draw.
//
// Origin is re-probed rather than trusted from the prepare-time cache: the
// agent pushed after that fetch, so refs/ben/remote/<key> is stale here by
// construction. It is probed only when leg 1 holds
// (core.PublishFacts.AdvancedPastBase), so a run whose contradiction is
// already visible locally never depends on the network to establish it;
// RemoteProbed reports which happened.
func (p *Provider) PublishFacts(ctx context.Context, ws core.Workspace) (core.PublishFacts, error) {
	if err := validateFactsWorkspace(ws); err != nil {
		return core.PublishFacts{}, err
	}

	// Serialized with Prepare and Dispose for this issue (SPEC §6.4): reading
	// evidence out from under a concurrent dispose would see a half-removed
	// branch and report it as "no commits".
	unlock := p.lock(ws.Key)
	defer unlock()
	if err := p.validateWorkspaceClaimBaseLocked(ctx, ws); err != nil {
		return core.PublishFacts{}, err
	}

	local, err := p.localBranchFactsLocked(ctx, ws)
	if err != nil {
		return core.PublishFacts{}, err
	}
	facts := core.PublishFacts{Head: local.Head, DescendsBase: local.DescendsBase}
	if !facts.AdvancedPastBase(ws.BaseSHA) {
		// Leg 1 already failed, and no remote fact can rescue it: there are no
		// commits, or none that descend from the pin. Contacting origin here
		// would put a verdict the local repository has already settled at the
		// mercy of the network — an unreachable origin would downgrade a
		// contradiction to a verification error.
		return facts, nil
	}

	remote, err := p.fetchRemoteIssueBranch(ctx, ws.Key, ws.Branch)
	if err != nil {
		return core.PublishFacts{}, err
	}
	facts.RemoteProbed = true
	facts.RemoteHead = remote

	if facts.RemoteHead != "" {
		// Reachability, not equality. Origin can legitimately carry commits
		// this daemon has never seen — a second daemon's revise run pushes to
		// the same issue branch (SPEC §5.1) — and those do not unpublish the
		// work this one pushed. The fetch above is what makes RemoteHead an
		// object this repository can order.
		if facts.RemoteHasHead, err = p.isAncestor(ctx, facts.Head, facts.RemoteHead); err != nil {
			return core.PublishFacts{}, err
		}
	}
	return facts, nil
}

// The provider's own bounds on an attempt account (SPEC §9.6). They exist so
// that a branch with a hundred thousand commits is not read into memory to be
// discarded a moment later; the caller applies a byte budget of its own, and
// both bounds are stated in what the prompt renders.
const (
	maxAttemptCommits = 50
	maxAttemptFiles   = 200
	// And a bound per line, because the entries themselves are agent-authored and
	// unbounded: a commit subject is one line of a message the agent wrote, and a
	// path can be as deep as the filesystem allows. Generous against anything
	// legible and far short of what a repository could be made to produce.
	maxCommitLineBytes = 200
	maxFileLineBytes   = 1024
)

// AttemptFacts reads what an attempt left on its branch: the commits past the
// claim-time base, newest first, and the files they changed (SPEC §9.6).
//
// An empty account is a legitimate answer, and it is a different one from a
// failure. A branch that never moved off its base carries no commits, which is
// the true account of an attempt that committed nothing; a read that could not be
// completed is an error, so a caller cannot report "committed nothing" about a
// branch nobody managed to look at.
//
// Serialized with Prepare and Dispose for this issue (SPEC §6.4), like every
// other read here: an account taken out from under a concurrent dispose would see
// a half-removed branch and report an attempt's work as absent.
//
// Nothing about this is a verdict, and it is deliberately not part of the §9.7
// evidence check. The three legs behind `done` are read by internal/verify from
// PublishFacts; this is prompt material, and a bug in it must never be able to
// publish or contradict a run.
func (p *Provider) AttemptFacts(ctx context.Context, ws core.Workspace) (core.AttemptFacts, error) {
	if err := validateFactsWorkspace(ws); err != nil {
		return core.AttemptFacts{}, err
	}

	// Bounded, unlike every other read here, because giving up is a legitimate
	// result for this one: the caller reports an unread branch as unread and routes
	// the attempt regardless. The ordinary `p.lock` cannot be bounded — these
	// mutexes are held across fetches — so a caller that must be able to stop
	// waiting would otherwise stall a finished attempt for as long as a hung
	// `git fetch` on another issue lasts, holding a §9.5 slot and blocking the
	// §11 drain (#61 review, finding 2).
	unlock, err := lockUntil(ctx, p.locks.forIssue(ws.Key))
	if err != nil {
		return core.AttemptFacts{}, fmt.Errorf("workspace %s: waiting for the issue lock: %w", ws.Key, err)
	}
	defer unlock()

	branch := "refs/heads/" + ws.Branch
	// "Added past the base", asked directly rather than through revParse and
	// LocalBranchFacts. Those separate a genuinely absent ref from a ref store
	// nobody could read, because §9.7 turns that distinction into a verdict (#16) —
	// and they reach it through unbounded git calls, on a path that must stay
	// bounded.
	//
	// What is *not* collapsed is failure. A branch still at its pin and one
	// rewritten so that "added" cannot be asserted are the same answer here — this
	// daemon has no work of its own to report — but a branch whose ref does not
	// resolve, or a repository that cannot be read, is an error and the caller
	// reports it as unread.
	descends, err := p.isAncestorBounded(ctx, ws.BaseSHA, branch)
	if err != nil {
		return core.AttemptFacts{}, err
	}
	if !descends {
		return core.AttemptFacts{}, nil
	}

	// %h and %s: the abbreviated SHA and the subject. The subject is git's first
	// line, so a commit message with a body cannot smuggle extra lines into what
	// looks like one entry — the fence covers the text either way, but a list
	// whose entries are lines should have one entry per line.
	//
	// --max-count is one over the bound so git itself stops early on a long
	// history, and the reader's own bound is what reports the truncation.
	var facts core.AttemptFacts
	facts.Commits, facts.CommitsTruncated, err = p.gitLines(ctx, maxAttemptCommits, maxCommitLineBytes,
		"log", "--no-color", "--format=%h %s",
		fmt.Sprintf("--max-count=%d", maxAttemptCommits+1), ws.BaseSHA+".."+branch)
	if err != nil {
		return core.AttemptFacts{}, err
	}
	facts.Files, facts.FilesTruncated, err = p.gitLines(ctx, maxAttemptFiles, maxFileLineBytes,
		"diff", "--name-only", ws.BaseSHA, branch)
	if err != nil {
		return core.AttemptFacts{}, err
	}
	return facts, nil
}

// isAncestorBounded is isAncestor over a bounded read: one line at most, since
// `--is-ancestor` answers with an exit status and says nothing on stdout.
//
// The classification is isAncestor's own (ancestorAnswer), deliberately shared
// rather than restated. This function used to have a looser one — every ExitError
// read as "not an ancestor" — on the reasoning that a prompt is not a verdict. That
// was wrong in the direction that matters: exit 1 is git's "no", and everything
// else is a repository nobody could read, which the account must report as unread.
// Reporting it as "commits: none" is the fabrication §5.6 forbids, and it is worse
// here than in a verdict, because an agent acts on it (#61 re-review, finding 3).
//
// Reflexive: a branch still at its pin answers true. Deliberately not corrected
// for — the commit range is empty for it, which is the same answer arrived at from
// the fact rather than from comparing a SHA against a ref name.
func (p *Provider) isAncestorBounded(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, _, err := p.gitLines(ctx, 1, 256, "merge-base", "--is-ancestor", ancestor, descendant)
	return p.ancestorAnswer(ctx, ancestor, descendant, err)
}

func validateFactsWorkspace(ws core.Workspace) error {
	if ws.Key == "" || ws.Branch == "" {
		return errors.New("workspace: branch facts require a prepared workspace's key and branch")
	}
	if ws.BaseSHA == "" {
		// Without the pin there is no "advanced" to assert: every branch
		// trivially descends from nothing, which would turn a run that added
		// no commits into published evidence (SPEC §9.7).
		return fmt.Errorf("%w: workspace %s carries no claim-time base SHA", ErrWorkspaceState, ws.Key)
	}
	if ws.ClaimEpoch <= 0 {
		return fmt.Errorf("%w: workspace %s carries no positive claim epoch", ErrClaimEpoch, ws.Key)
	}
	return nil
}

func (p *Provider) validateWorkspaceClaimBaseLocked(ctx context.Context, ws core.Workspace) error {
	state, err := p.readClaimBaseLocked(ctx, ws.Key)
	if err != nil {
		return err
	}
	if state.State != core.ClaimBasePinned || state.Epoch != ws.ClaimEpoch || state.BaseSHA != ws.BaseSHA {
		return fmt.Errorf("%w: workspace %s carries epoch/base %d/%s, provider has %s %d/%s",
			ErrClaimBaseState, ws.Key, ws.ClaimEpoch, ws.BaseSHA,
			state.State, state.Epoch, state.BaseSHA)
	}
	return nil
}

// localBranchFactsLocked reads the local observation shared by the pre-hook
// attach snapshot and post-run publication evidence. The caller owns the issue
// lock, so the branch cannot be disposed while its facts are read.
func (p *Provider) localBranchFactsLocked(ctx context.Context, ws core.Workspace) (core.LocalBranchFacts, error) {
	if err := validateFactsWorkspace(ws); err != nil {
		return core.LocalBranchFacts{}, err
	}
	return p.localBranchFactsAgainstLocked(ctx, ws.Branch, ws.BaseSHA)
}

// localBranchFactsAgainstLocked is the raw local observation used while a new
// epoch is still pending. It deliberately takes the outgoing base separately:
// constructing a Workspace would falsely present that old base as belonging to
// the new epoch, while zero epoch is correctly non-authorizing everywhere else.
func (p *Provider) localBranchFactsAgainstLocked(ctx context.Context, branch, baseSHA string) (core.LocalBranchFacts, error) {
	if branch == "" || baseSHA == "" {
		return core.LocalBranchFacts{}, fmt.Errorf("%w: local branch facts require a branch and base", ErrWorkspaceState)
	}

	facts := core.LocalBranchFacts{BaseSHA: baseSHA}
	head, ok, err := p.revParse(ctx, "refs/heads/"+branch)
	if err != nil {
		return core.LocalBranchFacts{}, err
	}
	if ok {
		facts.Head = head
		// isAncestor is reflexive, so a branch still at its base reports
		// DescendsBase — leg 1 asks "advanced" separately, and the caller
		// must not read this field as commits having been added.
		if facts.DescendsBase, err = p.isAncestor(ctx, baseSHA, head); err != nil {
			return core.LocalBranchFacts{}, err
		}
	}
	return facts, nil
}
