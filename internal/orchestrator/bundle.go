package orchestrator

import (
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Bundle is one immutable adapter set and the definition it was built from
// (SPEC §5.4, §5.7). The config watcher publishes a definition and a bundle
// together or not at all, so no adapter here is ever asked to operate under a
// definition it was not constructed and checked Ready against.
//
// Nothing in it is replaced after publication. A decision that spans several
// items therefore takes one bundle at its linearization point and carries it,
// rather than re-reading per question: re-reading would judge the first issue
// under one configuration and the next under another, and the pass would
// describe a configuration that never existed.
type Bundle struct {
	// Definition is the definition this set was built from. It is what makes the
	// binding checkable rather than asserted.
	Definition *config.WorkflowDefinition

	Tracker    Tracker
	Workspaces Workspaces
	Runner     Runner
	// Agent names the runner in the two terms an attempt-outcome record has to
	// carry (#60, #62): the `agent.kind` and the model its block selects.
	//
	// Here rather than resolved by the loop, because the loop has no route to it:
	// the model lives in the opaque provider block, which only the adapter may
	// read (SPEC §3.6), and this bundle is where the definition and the adapters
	// built from it are already bound together. A record pins it from the same
	// snapshot it pins Definition and takes its Runner from, so a reload that
	// changes the model changes the *next* attempt's record and never a live
	// one's (SPEC §5.4, Record.attemptAgent).
	//
	// Descriptive only. Nothing decides anything from it; Runner is what runs.
	Agent core.AgentDescriptor
	// Verifier is derived from Workspaces and Tracker (verify.New), so it cannot
	// be carried forward independently of either: a checker paired with a
	// provider from one bundle and a tracker from another would read one
	// repository's worktree and another's pull requests.
	Verifier Verifier

	// ClaimPrincipal is the identity this bundle's claims are assigned to
	// (SPEC §8.4). Without it §9.8 cannot tell our own claim from a human who
	// replaced it, and "exactly one assignee" is not the same question as "still
	// ours". Resolved from the built tracker, which is why a rebuilt tracker can
	// change it — see Identity.
	ClaimPrincipal string
	// Repository is the git remote the tracker names and the credential that
	// fetches it (SPEC §6.2, §10.2). The loop never reads it: it is already
	// bound into Workspaces. It is here because it is half of this bundle's
	// identity.
	Repository core.Repository
}

// Identity is what outstanding work is bound to, as opposed to configuration
// that may move under it freely.
//
// A claim belongs to the principal that made it, and a worktree to the root it
// was created under, so a rebuild that changes either cannot simply be adopted
// while such work exists — the daemon would sweep for claims under a principal
// that holds none, and address worktrees under a root that has none. §9.8's one
// conditional list read per tick and §10.1's unique claim principal per daemon
// instance both assume exactly one of these is live, and this is what keeps that
// true across a reload.
//
// The workflow key is deliberately absent: it is derived from the WORKFLOW.md
// path (config.workflowKey) and a watcher watches one path, so it cannot drift.
//
// A rotated credential for the same account does not change the principal, so
// the ordinary rotation is not an identity change and is never refused.
type Identity struct {
	Principal string
	// RemoteURL is the repository the claims live in. Two workflows on one root
	// with different repositories are different identities, and the base cache
	// refuses the mismatch anyway (workspace.validateBase).
	RemoteURL string
	// Root is workspace.root: where the worktrees and the base cache live.
	Root string
}

// Identity reports what this bundle's work is bound to.
func (b *Bundle) Identity() Identity {
	return Identity{
		Principal: b.ClaimPrincipal,
		RemoteURL: b.Repository.RemoteURL,
		Root:      b.Definition.Config.Workspace.Root,
	}
}

// Snapshot is the configuration in force for this orchestrator: one definition,
// one bundle, one verdict, one revision.
type snapshot = config.Snapshot[*Bundle]
