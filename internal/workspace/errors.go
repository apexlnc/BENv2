package workspace

import (
	"errors"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Named refusals (see AGENTS.md conventions); tests assert on these.
var (
	// ErrBaseRepoState refuses an unexpected base.git: fail closed, no
	// auto-repair (SPEC §6.2).
	ErrBaseRepoState = errors.New("workspace: base repository in unexpected state")

	// ErrBaseConfigSteering refuses repository-local Git configuration that can
	// redirect or re-authenticate a remote operation. base.git is writable by a
	// run, so its config is evidence to validate, never daemon policy (#231).
	ErrBaseConfigSteering = errors.New("workspace: base repository config can steer remote Git operations")

	// ErrScratchRoot refuses a missing or overlapping root for the temporary Git
	// repositories that carry credentialed remote commands. The root is supplied
	// from daemon-owned state and must never be one an agent receives (#231).
	ErrScratchRoot = errors.New("workspace: daemon scratch root is not isolated from agent-writable workspace state")

	// ErrBaseBranchNotFound is a structurally valid configured branch, or the
	// branch named by the remote HEAD symref, that the canonical remote does not
	// advertise. Existence is a credentialed readiness concern.
	ErrBaseBranchNotFound = errors.New("workspace: base branch does not exist on the remote")

	// ErrBaseBranchReserved refuses a configured or repository-default target
	// inside the namespace BEN uses for issue publication branches. Allowing the
	// repository default to be ben/<workspace_key> would make the target and
	// candidate the same ref.
	ErrBaseBranchReserved = errors.New("workspace: base branch uses BEN's reserved branch namespace")

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

	// ErrClaimTargetUnrecorded marks a pre-#152 claim-base record. Its base is
	// readable only as outgoing state for a later assignment epoch; it cannot
	// authorize same-epoch prepare, prompt rendering, or verification.
	ErrClaimTargetUnrecorded = core.ErrClaimTargetUnrecorded

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

	// ErrCleartextCredentialRemote refuses the pairing of a credential source
	// with a remote git would authenticate to over an unencrypted transport
	// (#230). The credential helper's host scoping cannot help here: the
	// configured host is the one reading the token off the wire.
	//
	// The pairing and not the scheme alone — a public `http://` remote with no
	// AuthSource exposes nothing, and BEN's own suites drive real git against
	// credential-free local remotes. At construction rather than at the first
	// remote invocation, in this repo's usual style: the remote arrives from the
	// tracker, so assembly is the earliest point that can see it, and a bundle
	// that refuses to build says so once instead of once per dispatched claim.
	ErrCleartextCredentialRemote = errors.New("workspace: a credential source cannot be used with a cleartext remote — http:// and ftp:// put the credential on the wire (SPEC §10.2)")
)
