package remotews

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The trusted remote-first restore: before every attempt, the sandbox's
// canonical branch is put back to the state the *daemon* independently observed
// on the canonical remote.
//
// It is BEN's own script rather than a workflow hook (remote.HookRestore) for
// the reason the four §5.2.6 hooks are the workflow's: this one is not
// configurable, cannot be skipped, and its failure is not a policy question.
//
// # Why it has to exist
//
// A retained sandbox is a persistent volume, and between two attempts anything
// may have touched it — the previous attempt's uncommitted work, a reviewer who
// opened a shell in it, a hook that wrote a file. None of that is a publication
// fact: §9.7's evidence is read from the canonical remote and the forge, and a
// commit that exists only inside the sandbox is invisible to every leg of it.
// Handing the next attempt a tree carrying such changes would let it build a
// revision on a base BEN cannot verify against, and — worse — let a reviewer's
// local edit silently become part of what the agent publishes.
//
// So the rule is stated the other way round: the only tree a revision may start
// from is the one the daemon read. Whatever else is in the sandbox is discarded,
// ignored files included, because "ignored" is the sandbox's own opinion and the
// sandbox is the thing being reset.
//
// # What it targets
//
//   - The canonical remote's head for the issue branch, when there is one. That
//     is the same observation §9.7 verifies against (mirror.RemoteFacts) — read
//     with BEN's own credential, materialized in BEN's own object store, and
//     ordered against the pin.
//   - The claim's trusted base otherwise, which is the state a first attempt
//     starts from.
//
// A head that does not descend the pin is neither: it is ErrRestoreDiverged, a
// park. Restoring to it would seed a revision whose publication can never satisfy
// leg 1, so the attempt would be spent to arrive at the same refusal.

// restore puts the sandbox's canonical branch back to the independently observed
// remote head.
func (p *Provider) restore(ctx context.Context, c Cycle, attempt int) error {
	target, _, err := p.restoreTarget(ctx, c, attempt)
	if err != nil {
		return err
	}
	identity, err := p.identity(ctx, c)
	if err != nil {
		return err
	}
	scope := p.gitScope(c, remote.GitPhasePrepare)
	scope.CheckoutCommit = target
	result, err := remote.RunCommand(ctx, p.hookExec, p.hookStore, remote.CommandInvocation{
		Identity: identity,
		ID:       hookID(c, remote.HookGitPrepare, attempt),
		Phase:    remote.HookGitPrepare,
		Attempt:  attempt,
		Argv:     []string{"/usr/local/bin/airlock-git", "prepare"},
		Git:      scope,
		Timeout:  p.hooks.Timeout,
	})
	if err != nil {
		// ContainmentOf(HookRestore) is abort, and this is where that is spent: an
		// attempt started over an unrestored tree is the whole hazard above.
		return err
	}
	var prepared struct {
		Repository string `json:"repository"`
		Branch     string `json:"branch"`
		HeadSHA    string `json:"head_sha"`
	}
	decoder := json.NewDecoder(strings.NewReader(result.Output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&prepared); err != nil {
		return fmt.Errorf("%w: decoding airlock-git prepare result: %v", remote.ErrHookFailed, err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("%w: airlock-git prepare returned trailing output", remote.ErrHookFailed)
	}
	if prepared.Repository != p.gitRepository || prepared.Branch != c.Branch || prepared.HeadSHA != target {
		return fmt.Errorf("%w: airlock-git prepare returned repository %q branch %q head %q",
			remote.ErrHookMismatch, prepared.Repository, prepared.Branch, prepared.HeadSHA)
	}
	p.log.Info("restored the canonical branch from the trusted remote-first source",
		"issue", c.Issue, "branch", c.Branch, "target", target, "attempt", attempt)
	return nil
}

// restoreTarget is the commit the branch is restored to, and whether the
// canonical remote carries it on the branch ref.
//
// The observation is made now, per attempt. A head read at an earlier tick is
// not this attempt's fact — the branch may have been force-pushed away since —
// which is the same rule verify.ErrRemoteMirrorStale states about a verdict.
func (p *Provider) restoreTarget(ctx context.Context, c Cycle, attempt int) (string, bool, error) {
	facts, err := p.base.RemoteFacts(ctx, c.RunRef(restoreVerification(c, attempt)))
	if err != nil {
		return "", false, fmt.Errorf("remotews: observing the canonical branch for issue %s: %w", c.Issue, err)
	}
	switch {
	case !facts.Fetched:
		// The fact source did not reach the remote during this observation. It has
		// therefore stated nothing, and restoring to a memory is restoring to a
		// state nobody is claiming still holds.
		return "", false, fmt.Errorf("remotews: the canonical branch for issue %s was not observed", c.Issue)
	case facts.BaseSHA != c.BaseSHA:
		return "", false, fmt.Errorf("%w: issue %s is pinned at %s and the fact source answered for %s",
			ErrCycleState, c.Issue, c.BaseSHA, facts.BaseSHA)
	case facts.RemoteHead == "":
		return c.BaseSHA, false, nil
	case !facts.DescendsBase:
		return "", false, fmt.Errorf("%w: issue %s: branch %s is at %s, which does not descend %s",
			ErrRestoreDiverged, c.Issue, c.Branch, facts.RemoteHead, c.BaseSHA)
	}
	return facts.RemoteHead, true, nil
}

// restoreVerification names this observation. Deterministic from the claim and
// the attempt: the fact source binds an answer to the question it was asked, and
// a stable name is what lets a retried prepare ask the same question rather than
// accumulate identities for observations nobody kept.
func restoreVerification(c Cycle, attempt int) string {
	return fmt.Sprintf("restore/%d/%d", c.Epoch, attempt)
}

// RunRef names one verification of a workspace's most recent attempt, for the
// v2 publish-evidence checker (#193, verify.SelectPublication).
//
// The verification identity is the caller's, minted per observation: it is what
// makes "these facts were observed for this question" checkable rather than
// assumed (core.RemoteRunRef).
func (p *Provider) RunRef(ws core.Workspace, verification string) (core.RemoteRunRef, error) {
	c, err := p.load(ws.Key, "")
	if err != nil {
		return core.RemoteRunRef{}, err
	}
	if c.Epoch != ws.ClaimEpoch {
		return core.RemoteRunRef{}, fmt.Errorf("%w: workspace %s carries epoch %d and the record holds %d",
			ErrCycleState, ws.Key, ws.ClaimEpoch, c.Epoch)
	}
	ref := c.RunRef(verification)
	if !ref.Complete() {
		return core.RemoteRunRef{}, fmt.Errorf("%w: %s has no complete run reference (%+v)",
			ErrCycleState, ws.Key, ref)
	}
	return ref, nil
}
