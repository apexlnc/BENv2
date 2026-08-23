package workspace

import "errors"

// Named refusals (see AGENTS.md conventions); tests assert on these.
var (
	// ErrBaseRepoState refuses an unexpected base.git: fail closed, no
	// auto-repair (SPEC §6.2).
	ErrBaseRepoState = errors.New("workspace: base repository in unexpected state")

	// ErrWorkspaceState refuses a workspace whose directory and git
	// registration disagree in a way prune-and-retry-once could not fix, or
	// any ambiguous git failure — no plain-directory fallback guessing
	// (SPEC §6.6). The workspace is kept for forensics.
	ErrWorkspaceState = errors.New("workspace: workspace in unexpected state")

	// ErrClaimEpoch refuses a missing or non-positive tracker-native assignment
	// event ID. Epoch zero is the non-authorizing value (SPEC §6.1, §8.2).
	ErrClaimEpoch = errors.New("workspace: invalid claim epoch")

	// ErrClaimBaseState refuses malformed, contradictory, unreadable, or
	// mismatched provider-owned claim-base safety evidence (SPEC §6.2).
	ErrClaimBaseState = errors.New("workspace: claim base in unexpected state")

	// ErrBranchDiverged refuses an issue branch whose local and origin
	// histories have each moved since they last agreed: fast-forwarding
	// either way would discard someone's commits, so nothing is touched
	// (SPEC §6.2 remote-first reattach; #16).
	ErrBranchDiverged = errors.New("workspace: issue branch diverged from origin")

	// ErrHookFailed marks an aborting hook failure: after_create aborts
	// workspace creation, before_run aborts the attempt (SPEC §5.2.6).
	ErrHookFailed = errors.New("workspace: hook failed")

	// ErrPathEscape enforces safety invariant 2 (SPEC §6.3): a workspace
	// path outside the workspace root is never touched.
	ErrPathEscape = errors.New("workspace: path escapes workspace root")

	// ErrNoRepositorySource refuses assembly against a tracker that cannot
	// name a repository (core.RepositorySource). The v1 strategy exists only
	// as a clone of one (SPEC §6.2) and the tracker is the only component
	// allowed to know which, so a silent tracker kind is a refusal at wiring
	// rather than a remote guessed somewhere else.
	ErrNoRepositorySource = errors.New("workspace: tracker cannot supply a repository remote")

	// ErrTransportHelperRemote refuses Git's explicit <helper>::<address>
	// syntax. The address grammar belongs to the helper, so workspace cannot
	// reliably distinguish a credential from an ordinary opaque key. Refusing
	// the whole form is the fail-closed decision recorded on #98; legitimate
	// helper-only remotes are the accepted cost.
	//
	// Like ErrRemoteCredentials, the message is constant because the refused
	// value may itself contain a secret.
	ErrTransportHelperRemote = errors.New("workspace: Git transport-helper remotes are not supported (SPEC §10.2)")

	// ErrRemoteCredentials refuses a RemoteURL carrying a credential in its
	// userinfo, for every scheme (SPEC §10.2; #52). The credential belongs in
	// Repository.AuthSource, which reaches git through the child environment.
	//
	// The message is constant by design: the value being refused is the
	// secret. Unlike config's RenderRefusal — where provenance proves the
	// reader can already see the value (SPEC §5.8) — no part of the URL is
	// echoed, not even the scheme. Which workflow refused is in the caller's
	// wrapping context.
	ErrRemoteCredentials = errors.New("workspace: remote URL must not embed credentials — pass them via Repository.AuthSource so they stay out of argv and git config (SPEC §10.2)")
)
