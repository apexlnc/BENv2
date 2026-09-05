// Package workspace implements the v1 workspace strategy (SPEC §6): a bare
// base clone per workflow plus one git worktree and one ben/<workspace_key>
// branch per issue, laid out as <root>/<workflow_key>/base.git,
// <root>/<workflow_key>/issues/<workspace_key>, and
// <root>/<workflow_key>/private/<workspace_key>.
//
// Strategy invariants owned here (SPEC §6.2–§6.7):
//
//   - base.git is bootstrapped atomically (temp dir + rename) and fetched
//     before every attempt; unexpected base state fails closed, no
//     auto-repair. The claim's retained target branch is fetched into
//     refs/heads/; origin's
//     ben/* issue branch is probed every prepare and fetched into
//     refs/ben/remote/<workspace_key> — fetch cannot move a checked-out
//     branch — as the remote-first reattach source (SPEC §6.2; #16).
//   - base.git carries a credential-free `origin` remote so agents can run
//     the canonical `git push origin HEAD` (SPEC §5.6, §6.7) with their own
//     publish credentials. The daemon's tracker credential (SPEC §10.2)
//     reaches fetches through a credential helper reading the child
//     environment — never argv, never on-disk config.
//   - The per-issue branch is created once and reattached on every retry —
//     never -B (force-recreating it has discarded agent commits in
//     production). Reattach is remote-first (SPEC §6.2; #16): a branch that
//     exists on origin but not locally is created at the remote head, never
//     derived from the configured/default target branch; when both exist, strictly-behind
//     fast-forwards, strictly-ahead (unpushed work) attaches as-is, and true
//     divergence fails closed.
//   - A durable provider-owned claim-base record binds the positive tracker
//     assignment event ID to the branch head observed after remote-first
//     reattachment and before any current-attempt hook. Pending and pinned are
//     whole-file atomic states; refs/ben/claim-base/* keeps their commits
//     reachable. Every prepare validates the expected epoch and never remints a
//     same-epoch base (SPEC §6.2, §9.7).
//   - The workspace root is a disposable cache (SPEC §3.1, §6.2): branch
//     identity and pushed commits reconstruct from origin's ben/* branch, and
//     refs/ben/ready/* is deliberately per-host — a fresh host re-runs its
//     bootstrap. A standing claim's historical pin does not reconstruct: loss
//     of the claim-base record parks for a deliberate re-claim (SPEC §3.1).
//   - private/<workspace_key> is the per-workspace directory a harness may
//     write but the repository must not carry (SPEC §6.2). It is placed and
//     disposed here — outside the worktree, which is what keeps its contents
//     out of a commit — and it shares the worktree's lifetime exactly, so a
//     continuation chain's harness state survives every attempt in the chain
//     (SPEC §6.4). Not unwritable: the harness owns everything inside it.
//   - after_create completion is recorded in refs/ben/ready/<workspace_key>
//     (cleared on removal): a workspace whose bootstrap never succeeded
//     re-runs it on the next Prepare instead of being silently reused.
//   - Worktree failures follow the normative taxonomy (SPEC §6.6):
//     registration is verified via `git worktree list --porcelain`,
//     recoverable debris is prune-and-retried exactly once, and anything
//     ambiguous fails closed with the workspace kept.
//   - Per-issue locks serialize all workspace operations for an issue, so
//     one issue never has two live workspaces (SPEC §6.4).
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/gitremote"
)

const (
	branchPrefix   = "ben/"
	baseRefPrefix  = "refs/ben/base/"
	readyRefPrefix = "refs/ben/ready/"
	// remoteRefPrefix caches origin's view of the issue branch per prepare
	// (SPEC §6.2 remote-first reattach): a branch ref cannot receive the
	// fetch while checked out in a worktree, a non-branch ref always can.
	remoteRefPrefix = "refs/ben/remote/"
	// defaultHookTimeout mirrors the hooks.timeout_ms config default
	// (SPEC §5.2.6); a zero Options value must not mean "kill instantly".
	defaultHookTimeout = 60 * time.Second
)

// Options parameterize the provider. The wirer (orchestrator assembly, B08)
// builds them from the loaded WorkflowDefinition and the tracker adapter.
type Options struct {
	// Root is the absolute workspace root (workspace.root, SPEC §5.2.4);
	// the config loader normalizes it before it gets here.
	Root string
	// WorkflowKey names this workflow's subtree under Root (SPEC §5.1, §6.2).
	WorkflowKey string
	// ScratchRoot is a daemon-only directory for ephemeral repositories used by
	// credentialed remote Git commands. Assembly supplies the workflow's state
	// directory: unlike workspace paths and TMPDIR, it is never handed to an
	// agent. It must be absolute and must not overlap any agent-writable root.
	ScratchRoot string
	// AgentTempRoot is the TMPDIR root the local agent receives. It is supplied
	// separately so New can prove ScratchRoot does not overlap it; using the
	// process environment here would make tests and assembly disagree about the
	// boundary they constructed.
	AgentTempRoot string
	// AgentWrites is the complete additional write scope reported by the
	// selected local agent kind. Concrete roots must not overlap ScratchRoot.
	// Unbounded is accepted deliberately: it records that this in-process path
	// check cannot establish isolation, while the deployment may supply an
	// external principal/filesystem boundary (SPEC §10.1). Treating it as "/"
	// would make every possible scratch path a startup refusal.
	AgentWrites core.LocalWriteScope
	// Repository is what the base repo fetches from and what it stores as its
	// `origin` remote, plus the credential for those fetches — as the tracker
	// named them (RepositoryFrom). The value passes from the adapter to here
	// whole: the wirer never re-derives a URL of its own, which is what keeps
	// the provider block out of the core (SPEC §5.2.5, §6.2, §10.2).
	// RemoteURL MUST be credential-free — userinfo carrying a password is
	// rejected whatever the scheme, and http(s) userinfo is rejected whole
	// (ErrRemoteCredentials); the credential belongs in Repository.AuthSource.
	// When an AuthSource is present the remote must also not be one git would
	// authenticate to in the clear (ErrCleartextCredentialRemote).
	Repository core.Repository
	// BaseBranch is workspace.base_branch. Empty selects the repository default
	// when a new claim epoch is first prepared.
	BaseBranch string
	Hooks      Hooks
	// Locks is the serialization for this workspace tree (see LockDomain). Nil
	// allocates a fresh one, which is right for the first provider on a tree and
	// wrong for a rebuild: assembly carries the previous generation's domain
	// forward whenever Root and WorkflowKey are unchanged, so both generations
	// address one base.git under one set of mutexes.
	Locks *LockDomain
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// Provider is the git-worktree WorkspaceProvider (SPEC §6.1). Beyond the
// core interface it exposes the claim-aware prepare/store operations,
// PublishFacts, AfterRun and Sweep to the consumers that own those questions.
type Provider struct {
	root       string
	remoteURL  string
	baseBranch string
	// authSource is where the remote credential comes from, resolved immediately
	// before each remote invocation (SPEC §6.2, amendment 6). Nil means
	// unauthenticated — a public repo, a file remote, ambient helpers.
	//
	// A source and not a value. The value it yields has a lifetime; a provider
	// outlives it, and a provider holding one would fetch with a credential
	// nothing can refresh and redact a token nobody is using any more.
	authSource  core.RemoteAuthSource
	hooks       Hooks
	hookTimeout time.Duration
	logger      *slog.Logger

	wfDir       string // <root>/<workflow_key>
	baseDir     string // <root>/<workflow_key>/base.git
	issuesDir   string // <root>/<workflow_key>/issues
	privateDir  string // <root>/<workflow_key>/private
	scratchRoot string // daemon-owned; never reported in core.WorkspacePaths

	// locks is the serialization for this workspace tree — shared, not owned.
	// See LockDomain.
	locks *LockDomain
}

// LockDomain serializes access to one workspace tree: git against the base
// repository, and each issue's worktree (SPEC §6.4, §6.6).
//
// It is a value a provider is constructed *with*, not one it creates, because the
// tree outlives any single provider. Two generations addressing one
// `<root>/<workflow_key>` would otherwise hold different mutexes over the same
// base.git — serializing nothing, exactly while an operation from the previous
// generation is still completing through the adapters it captured.
//
// **Scoped to the tree, not to the identity.** A caller must carry the domain
// forward whenever `Root` and `WorkflowKey` are unchanged, including across a
// repository or claim-principal change: those rebuild the provider and may be
// refused as identity changes later, but the new provider still addresses the same
// directory, and CheckBaseCache takes this very base mutex *before* any such
// refusal can happen. Keying the domain on identity instead would leave that
// window unserialized.
type LockDomain struct {
	baseMu sync.Mutex

	mu    sync.Mutex
	issue map[string]*sync.Mutex
}

// NewLockDomain returns a domain for one workspace tree.
func NewLockDomain() *LockDomain {
	return &LockDomain{issue: map[string]*sync.Mutex{}}
}

// forIssue returns this issue's mutex, creating it on first use.
func (d *LockDomain) forIssue(key string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	m, ok := d.issue[key]
	if !ok {
		m = &sync.Mutex{}
		d.issue[key] = m
	}
	return m
}

// LockDomain is the serialization this provider was built with, so a rebuild
// can carry it forward (see LockDomain, Options.Locks).
//
// Options.Locks is how a domain goes in; without this there is no way to get one
// back out, and assembly rebuilding a provider on the same tree would have
// nothing to pass but a fresh domain — two generations holding different mutexes
// over one base.git, which is the state the type exists to prevent. It is a
// getter for a field rather than a registry of trees on purpose: the value is
// assembly's to own, and a package-level table would be process-global state in
// a package that has none.
func (p *Provider) LockDomain() *LockDomain { return p.locks }

var _ core.WorkspaceProvider = (*Provider)(nil)

func New(opts Options) (*Provider, error) {
	switch {
	case opts.Root == "":
		return nil, errors.New("workspace: Root is required")
	case !filepath.IsAbs(opts.Root):
		return nil, fmt.Errorf("workspace: Root must be absolute (the config loader normalizes it — SPEC §5.2.4): %q", opts.Root)
	case opts.WorkflowKey == "":
		return nil, errors.New("workspace: WorkflowKey is required")
	case opts.ScratchRoot == "":
		return nil, fmt.Errorf("%w: ScratchRoot is required", ErrScratchRoot)
	case !filepath.IsAbs(opts.ScratchRoot):
		return nil, fmt.Errorf("%w: ScratchRoot must be absolute: %q", ErrScratchRoot, opts.ScratchRoot)
	case opts.AgentTempRoot == "":
		return nil, fmt.Errorf("%w: AgentTempRoot is required", ErrScratchRoot)
	case !filepath.IsAbs(opts.AgentTempRoot):
		return nil, fmt.Errorf("%w: AgentTempRoot must be absolute: %q", ErrScratchRoot, opts.AgentTempRoot)
	case opts.AgentWrites.Unbounded && len(opts.AgentWrites.Roots) != 0:
		return nil, fmt.Errorf("%w: an unbounded AgentWrites scope must not also name concrete roots", ErrScratchRoot)
	case opts.Repository.RemoteURL == "":
		return nil, errors.New("workspace: Repository.RemoteURL is required")
	}
	for i, root := range opts.AgentWrites.Roots {
		if root == "" || !filepath.IsAbs(root) {
			// Provider values may be env-resolved, so identify the field without
			// copying its value into an unredacted runtime error.
			return nil, fmt.Errorf("%w: AgentWrites.Roots[%d] must be a non-empty absolute path", ErrScratchRoot, i)
		}
	}
	if gitremote.IsTransportHelper(opts.Repository.RemoteURL) {
		return nil, ErrTransportHelperRemote
	}
	if gitremote.EmbedsCredential(opts.Repository.RemoteURL) {
		return nil, ErrRemoteCredentials
	}
	if opts.Repository.AuthSource != nil && gitremote.IsCleartextTransport(opts.Repository.RemoteURL) {
		return nil, ErrCleartextCredentialRemote
	}
	p := &Provider{
		root:        filepath.Clean(opts.Root),
		remoteURL:   opts.Repository.RemoteURL,
		baseBranch:  opts.BaseBranch,
		authSource:  opts.Repository.AuthSource,
		hooks:       opts.Hooks,
		hookTimeout: opts.Hooks.Timeout,
		logger:      opts.Logger,
		locks:       opts.Locks,
	}
	if p.locks == nil {
		p.locks = NewLockDomain()
	}
	if p.hookTimeout <= 0 {
		p.hookTimeout = defaultHookTimeout
	}
	if p.logger == nil {
		p.logger = slog.Default()
	}
	p.wfDir = filepath.Join(p.root, opts.WorkflowKey)
	p.baseDir = filepath.Join(p.wfDir, "base.git")
	p.issuesDir = filepath.Join(p.wfDir, "issues")
	p.privateDir = filepath.Join(p.wfDir, "private")
	// Keep the resolved spelling, not the caller's symlink. Otherwise a link
	// beneath an agent-writable parent can point somewhere safe during this
	// check and be substituted before temporaryRemoteRepository follows it.
	p.scratchRoot = normalizePath(opts.ScratchRoot)
	root := normalizePath(p.root)
	scratch := p.scratchRoot
	untrustedRoots := []struct{ name, path string }{
		{name: "workspace root", path: root},
		{name: "agent TMPDIR", path: normalizePath(opts.AgentTempRoot)},
	}
	for i, path := range opts.AgentWrites.Roots {
		untrustedRoots = append(untrustedRoots, struct{ name, path string }{
			name: fmt.Sprintf("agent writable root %d", i), path: normalizePath(path),
		})
	}
	for _, untrusted := range untrustedRoots {
		if untrusted.path == scratch || strictlyUnder(untrusted.path, scratch) || strictlyUnder(scratch, untrusted.path) {
			return nil, fmt.Errorf("%w: ScratchRoot %s overlaps %s",
				ErrScratchRoot, p.scratchRoot, untrusted.name)
		}
	}
	return p, nil
}

// IsApplicable reports whether this strategy can serve the workflow
// (SPEC §6.1). v1 ships exactly one strategy; it always applies.
func (p *Provider) IsApplicable(ctx context.Context) bool {
	return true
}

// Prepare is the closed core strategy operation: it creates or reattaches only
// under an already pinned positive claim base. The orchestrator uses
// PrepareClaim instead, because it can supply the tracker epoch it expects.
func (p *Provider) Prepare(ctx context.Context, issue core.Issue, attempt int) (core.Workspace, error) {
	ws, _, err := p.prepare(ctx, issue, attempt, 0)
	return ws, err
}

// PrepareClaim supplies the expected tracker-native claim epoch on every
// prepare. When that epoch is pending, it returns the local prior-work fact
// computed against the outgoing pin after remote-first reattachment and before
// atomically installing the current pin. Same-epoch calls retain the installed
// base and return no prior-work observation (SPEC §6.2, §9.6, §9.7).
func (p *Provider) PrepareClaim(ctx context.Context, issue core.Issue, attempt int, epoch int64) (core.Workspace, core.LocalBranchFacts, error) {
	if epoch <= 0 {
		return core.Workspace{}, core.LocalBranchFacts{}, fmt.Errorf("%w: claim epoch must be positive", ErrClaimEpoch)
	}
	return p.prepare(ctx, issue, attempt, epoch)
}

// PrepareWithLocalFacts is retained for the closed strategy's existing
// consumers. It can operate only on an already pinned base and cannot initialize
// pending state; production orchestration uses PrepareClaim so the tracker epoch
// is compared before hooks.
func (p *Provider) PrepareWithLocalFacts(ctx context.Context, issue core.Issue, attempt int) (core.Workspace, core.LocalBranchFacts, error) {
	return p.prepare(ctx, issue, attempt, 0)
}

func (p *Provider) prepare(ctx context.Context, issue core.Issue, attempt int, expectedEpoch int64) (core.Workspace, core.LocalBranchFacts, error) {
	if issue.Identifier == "" {
		return core.Workspace{}, core.LocalBranchFacts{}, errors.New("workspace: issue identifier is empty")
	}
	key := Key(issue.Identifier)
	ws := core.Workspace{
		WorkspacePaths: p.pathsFor(key),
		Key:            key,
		Branch:         branchPrefix + key,
	}
	unlock := p.lock(key)
	defer unlock()

	// Safety invariant 2 (SPEC §6.3) — the sanitized key cannot traverse,
	// but the check is cheap and guards against future path plumbing bugs.
	if err := p.checkContained(ws.WorkspacePaths); err != nil {
		return ws, core.LocalBranchFacts{}, err
	}
	claimBase, err := p.readClaimBaseLocked(ctx, key)
	if err != nil {
		return ws, core.LocalBranchFacts{}, err
	}
	if expectedEpoch == 0 {
		if claimBase.State != core.ClaimBasePinned || claimBase.Epoch <= 0 {
			return ws, core.LocalBranchFacts{}, fmt.Errorf("%w: workspace %s has no established positive claim base",
				ErrClaimBaseState, key)
		}
		expectedEpoch = claimBase.Epoch
	}
	if claimBase.Epoch != expectedEpoch ||
		(claimBase.State != core.ClaimBasePending && claimBase.State != core.ClaimBasePinned) {
		return ws, core.LocalBranchFacts{}, fmt.Errorf("%w: workspace %s is %s for epoch %d, expected epoch %d",
			ErrClaimBaseState, key, claimBase.State, claimBase.Epoch, expectedEpoch)
	}
	if claimBase.State == core.ClaimBasePinned {
		ws.ClaimEpoch, ws.BaseSHA = claimBase.Epoch, claimBase.BaseSHA
		ws.TargetBranch = claimBase.TargetBranch
	}
	target := claimBase.TargetBranch
	if claimBase.State == core.ClaimBasePending {
		target, err = p.resolveTargetBranch(ctx)
		if err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
	}
	if err := p.ensureBase(ctx, target); err != nil {
		return ws, core.LocalBranchFacts{}, err
	}
	// Whose directory this is, recorded before anything creates one. §9.10 step 5
	// sweeps the workspaces of *terminal issues*, and Key() cannot be inverted to
	// answer which issue a directory belongs to (see owners.go). Written early and
	// unconditionally: a prepare that fails past this point can still have left a
	// worktree behind, and an unrecorded one is a directory the sweep can only
	// report rather than remove.
	if err := p.recordOwner(key, issue.Identifier); err != nil {
		return ws, core.LocalBranchFacts{}, fmt.Errorf("workspace: recording the owner of %s: %w", key, err)
	}
	if err := p.fetchBase(ctx, target); err != nil { // fetch-before-attempt (SPEC §6.2)
		return ws, core.LocalBranchFacts{}, err
	}
	remoteSHA, err := p.fetchRemoteIssueBranch(ctx, key, ws.Branch)
	if err != nil {
		return ws, core.LocalBranchFacts{}, err
	}
	localSHA, localExists, err := p.revParse(ctx, "refs/heads/"+ws.Branch)
	if err != nil {
		return ws, core.LocalBranchFacts{}, err
	}
	startSHA := localSHA
	if !localExists {
		switch {
		case remoteSHA != "":
			startSHA = remoteSHA
		case claimBase.State == core.ClaimBasePinned:
			startSHA = claimBase.BaseSHA
		default:
			startSHA, localExists, err = p.revParse(ctx, "refs/heads/"+target)
			if err != nil {
				return ws, core.LocalBranchFacts{}, err
			}
			if !localExists {
				return ws, core.LocalBranchFacts{}, fmt.Errorf("%w: target branch %q missing from base repository", ErrBaseRepoState, target)
			}
		}
	}

	entries, err := p.listWorktrees(ctx)
	if err != nil {
		return ws, core.LocalBranchFacts{}, err
	}
	var atPath, ofBranch *worktreeEntry
	for i := range entries {
		e := &entries[i]
		if e.bare {
			continue
		}
		if samePath(e.path, ws.Path) {
			atPath = e
		}
		if e.branch == "refs/heads/"+ws.Branch {
			ofBranch = e
		}
	}
	wantRef := "refs/heads/" + ws.Branch

	switch {
	case atPath != nil && dirExists(ws.Path):
		if atPath.branch != wantRef {
			return ws, core.LocalBranchFacts{}, fmt.Errorf("%w: %s has %q checked out, want %q — refusing to touch it (SPEC §6.6)",
				ErrWorkspaceState, ws.Path, atPath.branch, wantRef)
		}
		if err := p.reconcileBranch(ctx, ws.Branch, remoteSHA, ws.Path); err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
		// Reattach: the existing worktree is reused as-is (SPEC §6.4).
	case atPath != nil:
		// Stale registration / crashed-run debris (SPEC §6.6): prune the
		// dead registration, then recreate on the surviving branch.
		if _, err := p.baseGit(ctx, "worktree", "prune"); err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
		if err := p.reconcileBranch(ctx, ws.Branch, remoteSHA, ""); err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
		if err := p.addWorktree(ctx, &ws, startSHA); err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
	case dirExists(ws.Path):
		return ws, core.LocalBranchFacts{}, fmt.Errorf("%w: %s exists but is not a registered worktree of %s — refusing to guess (SPEC §6.6)",
			ErrWorkspaceState, ws.Path, p.baseDir)
	case ofBranch != nil:
		return ws, core.LocalBranchFacts{}, fmt.Errorf("%w: branch %s is already checked out at %s — one issue must never have two live workspaces (SPEC §6.4)",
			ErrWorkspaceState, ws.Branch, ofBranch.path)
	default:
		if err := p.reconcileBranch(ctx, ws.Branch, remoteSHA, ""); err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
		if err := p.addWorktree(ctx, &ws, startSHA); err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
	}

	// Origin has now won every remote-first reconciliation decision, and neither
	// current-attempt hook has run. Observe the outgoing pin before publishing
	// the current epoch/base, or #94 would compare the head to itself.
	var prior core.LocalBranchFacts
	if claimBase.State == core.ClaimBasePending {
		if claimBase.OutgoingBaseSHA != "" {
			prior, err = p.localBranchFactsAgainstLocked(ctx, ws.Branch, claimBase.OutgoingBaseSHA)
			if err != nil {
				return ws, core.LocalBranchFacts{}, fmt.Errorf("workspace: reading prior-work facts before repinning: %w", err)
			}
		}
		head, ok, err := p.revParse(ctx, "refs/heads/"+ws.Branch)
		if err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
		if !ok {
			return ws, core.LocalBranchFacts{}, fmt.Errorf("%w: attached branch %s has no head", ErrWorkspaceState, ws.Branch)
		}
		if err := p.pinClaimBaseLocked(ctx, key, claimBase, head, target); err != nil {
			return ws, core.LocalBranchFacts{}, err
		}
		ws.ClaimEpoch, ws.BaseSHA, ws.TargetBranch = expectedEpoch, head, target
	}

	// After the worktree is established and on every path through the switch,
	// including reattach: the private dir shares the worktree's lifetime
	// (SPEC §6.4), and an adapter is handed the path unconditionally, so it
	// must exist whenever a Prepare returns one. MkdirAll rather than
	// create-once, because a workspace whose private dir an operator or a
	// tmp-sweep removed is a workspace that has lost harness state — not one
	// that has lost its worktree — and re-creating it empty is the recovery.
	if err := os.MkdirAll(ws.PrivateDir, 0o700); err != nil {
		return ws, prior, fmt.Errorf("workspace: creating private dir %s: %w", ws.PrivateDir, err)
	}

	readyRef := readyRefPrefix + key
	needBootstrap := ws.CreatedNow
	if !needBootstrap {
		_, ready, err := p.revParse(ctx, readyRef)
		if err != nil {
			return ws, prior, err
		}
		needBootstrap = !ready
	}
	if needBootstrap {
		if ws.CreatedNow {
			p.logger.Info("workspace created",
				"key", key, "path", ws.Path, "branch", ws.Branch,
				"epoch", ws.ClaimEpoch, "base", ws.BaseSHA, "attempt", attempt)
		} else {
			p.logger.Info("workspace bootstrap incomplete; re-running after_create", "key", key)
		}
		if err := p.runHook(ctx, "after_create", p.hooks.AfterCreate, ws.Path); err != nil {
			// Aborts workspace creation (SPEC §5.2.6). The worktree is kept
			// and deliberately not marked ready: the next Prepare re-runs
			// the bootstrap instead of silently reusing a half-built
			// workspace (durable completion state over best-effort cleanup).
			return ws, prior, fmt.Errorf("%w: after_create aborted workspace creation: %v", ErrHookFailed, err)
		}
		if _, err := p.baseGit(ctx, "update-ref", readyRef, ws.BaseSHA); err != nil {
			return ws, prior, err
		}
		ws.CreatedNow = true
	}
	if err := p.runHook(ctx, "before_run", p.hooks.BeforeRun, ws.Path); err != nil {
		// Aborts the attempt only; the workspace is kept (SPEC §5.2.6).
		return ws, prior, fmt.Errorf("%w: before_run aborted the attempt: %v", ErrHookFailed, err)
	}
	return ws, prior, nil
}

// Dispose removes the workspace worktree and its private dir, which share a
// lifetime (SPEC §6.4). keep=true preserves everything on disk for forensics —
// nothing is removed. The before_remove hook fires before any dispose,
// keep=true and the startup sweep included (SPEC §5.2.6), with
// logged-and-ignored semantics. The ben/* branch is never deleted on either
// path: the pushed branch is the archive, and recovery reattaches it
// (SPEC §6.4, §9.10). The shared git dir outlives both — it is per-workflow.
func (p *Provider) Dispose(ctx context.Context, ws core.Workspace, keep bool) error {
	if ws.Key == "" || ws.Path == "" {
		return errors.New("workspace: dispose needs a workspace with key and path")
	}
	unlock := p.lock(ws.Key)
	defer unlock()

	// The private dir to remove is derived from this provider's own layout
	// rather than read off the argument. §9.10 reconstructs a Workspace from
	// git and tracker evidence and Sweep reconstructs one from a directory
	// listing, so a Dispose that removed only what its caller happened to
	// remember would leak the private dir of every workspace no live record
	// survives for — silently, and for as long as the root does. Deriving is
	// the provider's prerogative and only the provider's: §7.1 refuses it to
	// adapters precisely because the layout is this package's (SPEC §6.2).
	//
	// A caller that carries a *different* private dir is refused rather than
	// overruled. It means the workspace came from another provider or another
	// layout, and removing this one's directory on its behalf would delete a
	// path nobody asked about.
	paths := p.pathsFor(ws.Key)
	if ws.PrivateDir != "" && !samePath(ws.PrivateDir, paths.PrivateDir) {
		return fmt.Errorf("%w: workspace %s reports private dir %s, but this provider's layout places it at %s",
			ErrWorkspaceState, ws.Key, ws.PrivateDir, paths.PrivateDir)
	}
	paths.Path = ws.Path

	// Safety invariant 2 (SPEC §6.3): never remove — or run hooks — outside
	// our own subtree.
	if err := p.checkContained(paths); err != nil {
		return err
	}
	if dirExists(ws.Path) {
		if err := p.runHook(ctx, "before_remove", p.hooks.BeforeRemove, ws.Path); err != nil {
			p.logger.Warn("before_remove hook failed (ignored)", "key", ws.Key, "error", err)
		}
	}
	if keep {
		p.logger.Info("workspace kept for forensics", "key", ws.Key, "path", ws.Path)
		return nil
	}
	// Clear the bootstrap marker before removing: if removal is interrupted,
	// the leftover worktree re-runs its bootstrap rather than skipping it.
	// A marker that cannot be cleared blocks removal entirely — a stale
	// ready ref would let a future worktree skip its bootstrap (#16).
	readyRef := readyRefPrefix + ws.Key
	_, ok, err := p.revParse(ctx, readyRef)
	if err != nil {
		return err
	}
	if ok {
		if _, err := p.baseGit(ctx, "update-ref", "-d", readyRef); err != nil {
			return fmt.Errorf("%w: cannot clear bootstrap marker %s; refusing to remove: %v",
				ErrWorkspaceState, readyRef, err)
		}
	}
	// Before the worktree, not after, and the order is the whole reason there
	// is no orphan to sweep later. Sweep enumerates issues/, so a private dir
	// left behind by an interrupted dispose would be invisible to it and
	// survive as long as the root. Removing it first makes an interruption
	// leave the opposite state — a worktree whose private dir is gone — which
	// the next Prepare repairs by re-creating it (see the MkdirAll there). A
	// failure to remove it returns before the worktree is touched, so nothing
	// is half-disposed on the path that fails closed.
	if err := os.RemoveAll(paths.PrivateDir); err != nil {
		return fmt.Errorf("%w: could not remove private dir %s: %v", ErrWorkspaceState, paths.PrivateDir, err)
	}
	if _, err := p.baseGit(ctx, "worktree", "remove", "--force", "--force", ws.Path); err != nil {
		// Prune-and-retry once (SPEC §6.6): an already-gone worktree
		// disposes idempotently; anything else fails closed, workspace kept.
		if _, pruneErr := p.baseGit(ctx, "worktree", "prune"); pruneErr != nil {
			return fmt.Errorf("%w: worktree remove failed (%v) and prune failed (%v)",
				ErrWorkspaceState, err, pruneErr)
		}
		if dirExists(ws.Path) {
			return fmt.Errorf("%w: could not remove %s: %v", ErrWorkspaceState, ws.Path, err)
		}
	}
	// The directory is gone, so the record naming its issue has nothing left to
	// describe. Cleared after removal rather than before: a record outliving its
	// directory costs a sweep one tracker read that finds nothing to do, while a
	// directory outliving its record cannot be swept at all.
	if err := p.clearOwner(ws.Key); err != nil {
		p.logger.Warn("workspace removed but its owner record was not", "key", ws.Key, "error", err)
	}
	p.logger.Info("workspace disposed", "key", ws.Key, "path", ws.Path)
	return nil
}

// AfterRun fires the after_run hook: after each attempt, any outcome;
// failures are logged and ignored (SPEC §5.2.6). Not part of
// core.WorkspaceProvider — the orchestrator calls it on the concrete type.
func (p *Provider) AfterRun(ctx context.Context, ws core.Workspace) {
	if ws.Key == "" || !dirExists(ws.Path) {
		return
	}
	unlock := p.lock(ws.Key)
	defer unlock()
	if err := p.runHook(ctx, "after_run", p.hooks.AfterRun, ws.Path); err != nil {
		p.logger.Warn("after_run hook failed (ignored)", "key", ws.Key, "error", err)
	}
}

// Sweep disposes the workspaces of terminal-state issues at startup
// (SPEC §6.4). terminal maps a workspace key to the verdict; the caller
// resolves keys via Key. The base repository is validated before any
// mutation, and a directory is only disposed once its worktree registration
// on the matching ben/* branch is proven — an unproven directory is left in
// place and reported, never guessed at (SPEC §6.6; #16). Per-workspace
// failures are logged and joined, never fatal to the sweep — startup
// hygiene must not block the daemon.
func (p *Provider) Sweep(ctx context.Context, terminal func(workspaceKey string) bool) error {
	var keys []string
	dirs, err := os.ReadDir(p.issuesDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No workspaces on disk; stale registrations may still need pruning.
	case err != nil:
		return fmt.Errorf("workspace: sweep: %w", err)
	default:
		for _, d := range dirs {
			if d.IsDir() {
				keys = append(keys, d.Name())
			}
		}
	}
	if !dirExists(p.baseDir) {
		if len(keys) > 0 {
			return fmt.Errorf("%w: issue workspaces exist under %s but %s is missing",
				ErrBaseRepoState, p.issuesDir, p.baseDir)
		}
		return nil
	}
	p.locks.baseMu.Lock()
	err = p.validateBase(ctx)
	p.locks.baseMu.Unlock()
	if err != nil {
		return err
	}
	// Prune against the validated base even when issues/ is empty: a stale
	// registration whose directory is gone would otherwise survive startup
	// and block the next Prepare of its branch (SPEC §6.6).
	if _, err := p.baseGit(ctx, "worktree", "prune"); err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}
	entries, err := p.listWorktrees(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, key := range keys {
		if !terminal(key) {
			continue
		}
		ws := core.Workspace{
			WorkspacePaths: p.pathsFor(key),
			Key:            key,
			Branch:         branchPrefix + key,
		}
		wantRef := "refs/heads/" + branchPrefix + key
		registered := false
		for i := range entries {
			if !entries[i].bare && samePath(entries[i].path, ws.Path) && entries[i].branch == wantRef {
				registered = true
				break
			}
		}
		if !registered {
			err := fmt.Errorf("%w: %s is not a registered worktree on %s — left in place",
				ErrWorkspaceState, ws.Path, wantRef)
			p.logger.Warn("sweep refusing unproven directory", "key", key, "error", err)
			errs = append(errs, err)
			continue
		}
		if err := p.Dispose(ctx, ws, false); err != nil {
			p.logger.Warn("sweep could not dispose workspace", "key", key, "error", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ensureBase validates or bootstraps <workflow>/base.git (SPEC §6.2). The
// bootstrap is atomic — init, HEAD discovery, and the first fetch happen in
// a temp dir renamed into place — so an existing base.git is always fully
// formed and anything unexpected fails closed with no auto-repair.
func (p *Provider) ensureBase(ctx context.Context, target string) error {
	p.locks.baseMu.Lock()
	defer p.locks.baseMu.Unlock()

	st, err := os.Stat(p.baseDir)
	switch {
	case err == nil && !st.IsDir():
		return fmt.Errorf("%w: %s is not a directory (fail closed, no auto-repair — SPEC §6.2)",
			ErrBaseRepoState, p.baseDir)
	case err == nil:
		return p.validateBase(ctx)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("workspace: stat base repository: %w", err)
	}

	wfDir := filepath.Dir(p.baseDir)
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	tmp, err := os.MkdirTemp(wfDir, ".base-bootstrap-")
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	defer os.RemoveAll(tmp)
	if _, err := p.git(ctx, wfDir, "init", "--quiet", "--bare", tmp); err != nil {
		return fmt.Errorf("workspace: bootstrap base: %w", err)
	}
	// origin lets agents run the canonical `git push origin HEAD`
	// (SPEC §5.6, §6.7). The URL is credential-free (enforced by New):
	// agents publish with their own credentials, and the daemon's fetches
	// inject theirs per call.
	if _, err := p.git(ctx, tmp, "remote", "add", "origin", p.remoteURL); err != nil {
		return fmt.Errorf("workspace: bootstrap base: %w", err)
	}
	if target == "" {
		return fmt.Errorf("%w: no claim-scoped target branch", ErrClaimTargetUnrecorded)
	}
	if _, err := p.git(ctx, tmp, "symbolic-ref", "HEAD", "refs/heads/"+target); err != nil {
		return fmt.Errorf("workspace: bootstrap base: %w", err)
	}
	if _, err := p.remoteGit(ctx, tmp, "fetch", "--quiet", p.remoteURL, "+refs/heads/"+target+":refs/heads/"+target); err != nil {
		return fmt.Errorf("workspace: bootstrap fetch: %w", err)
	}
	if err := os.Rename(tmp, p.baseDir); err != nil {
		return fmt.Errorf("workspace: bootstrap base: %w", err)
	}
	p.logger.Info("base repository bootstrapped", "path", p.baseDir, "branch", target)
	return nil
}

// CheckBaseCache reports whether the base cache already on disk is one this
// provider may use: absent, or present with an origin that matches the repository
// this workflow tracks.
//
// It exists so that an incompatible repository is a refused *reload* rather than a
// burned queue. validateBase runs from ensureBase, i.e. inside Prepare, so
// adopting a configuration whose repository has moved while a populated cache
// stands does not fail the reload — it fails every subsequent attempt, one
// dispatched issue at a time, as a launch error. Assembly calls this while
// building, alongside Ready.
//
// It answers from the filesystem and a local git config read: no network, no
// credentials. It repairs nothing (SPEC §6.2 fails closed).
func (p *Provider) CheckBaseCache(ctx context.Context) error {
	p.locks.baseMu.Lock()
	defer p.locks.baseMu.Unlock()

	st, err := os.Stat(p.baseDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// Nothing cached, so nothing to be incompatible with: the first Prepare
		// bootstraps it against the repository in force then.
		return nil
	case err != nil:
		return fmt.Errorf("workspace: stat base repository: %w", err)
	case !st.IsDir():
		return fmt.Errorf("%w: %s is not a directory (fail closed, no auto-repair — SPEC §6.2)",
			ErrBaseRepoState, p.baseDir)
	}
	// The one comparison, in the one place that owns it. Restating "origin must
	// match" here would be a second implementation to drift from.
	return p.validateBase(ctx)
}

// Ready proves the branch a future claim would select exists on the canonical
// remote, using the same credential source as Prepare. It intentionally runs
// after CheckBaseCache in assembly: a local identity mismatch is cheaper and
// more definitive than a network lookup.
func (p *Provider) Ready(ctx context.Context) error {
	_, err := p.resolveTargetBranch(ctx)
	return err
}

// baseConfigSteeringPattern is the closed repository-local refusal policy for
// validateBase (#231). The run can write base.git/config, so transport and
// authentication policy there is untrusted state (SPEC §3.5, §9.7):
//
//   - url.*.{insteadOf,pushInsteadOf} aliases an explicit URL, so passing
//     p.remoteURL in argv does not pin the endpoint;
//   - every http.* key is refused as one namespace rather than enumerating
//     Git's evolving transport settings: proxy and curloptResolve steer the
//     endpoint, sslVerify changes its authenticity, and URL-scoped variants
//     carry the same authority;
//   - remote.*.pushurl supplies a second publication endpoint;
//   - extensions.partialClone and remote.*.{promisor,partialCloneFilter} can
//     turn an otherwise local missing-object read into an implicit fetch;
//   - include.path, includeIf.*.path and extensions.worktreeConfig can hide any
//     of these keys in another run-authored file, including behind a condition
//     validation does not take or in base.git/config.worktree;
//   - credential[.<url>].helper supplies executable authentication policy in
//     the same untrusted file.
//
// This refusal is cache-state defense in depth, not the command boundary: all
// post-bootstrap network commands run in a fresh daemon-created repository, so
// even an unclassified key or a write after validation is not read by them.
// Other remote.* URLs cannot affect those commands because each receives
// p.remoteURL rather than a remote name. Hooks, fsmonitor, replacement refs and
// grafts are the separate invocation-wide neutralization owned by internal/gitcmd
// (#228). The hard-coded cases in workspace_test.go anchor this policy outside
// the declaration, so deleting one alternative cannot make its own test disappear.
const baseConfigSteeringPattern = `^(url\..*\.(insteadof|pushinsteadof)|http\..*|remote\..*\.(pushurl|promisor|partialclonefilter)|include\.path|includeif\..*\.path|extensions\.(worktreeconfig|partialclone)|credential(\..*)?\.helper)$`

// validateBase checks an existing base.git without repairing anything
// (SPEC §6.2, fail closed). Callers hold baseMu.
func (p *Provider) validateBase(ctx context.Context) error {
	// The HEAD check pins validation to this directory: rev-parse alone
	// would walk up and could report on an enclosing repository.
	if _, statErr := os.Stat(filepath.Join(p.baseDir, "HEAD")); statErr != nil {
		return fmt.Errorf("%w: %s has no HEAD (fail closed, no auto-repair — SPEC §6.2)",
			ErrBaseRepoState, p.baseDir)
	}
	out, gitErr := p.git(ctx, p.baseDir, "rev-parse", "--is-bare-repository")
	if gitErr != nil {
		return fmt.Errorf("%w: %s: %v", ErrBaseRepoState, p.baseDir, gitErr)
	}
	if out != "true" {
		return fmt.Errorf("%w: %s is not a bare repository (fail closed, no auto-repair — SPEC §6.2)",
			ErrBaseRepoState, p.baseDir)
	}
	// --no-includes asks only what base.git/config itself declares. Includes are
	// refused by name rather than followed: their target and activation are both
	// run-authored, and a conditional include could answer differently later.
	hasSteering, gitErr := p.gitConfigHasMatch(ctx, p.baseDir, baseConfigSteeringPattern)
	if gitErr != nil {
		return fmt.Errorf("%w: inspecting repository-local config in %s: %v",
			ErrBaseRepoState, p.baseDir, gitErr)
	}
	if hasSteering {
		// Do not echo the matched key: URL and credential subsections can carry
		// sensitive text, and the named refusal is enough to identify the class.
		return fmt.Errorf("%w: %w (fail closed, no auto-repair — SPEC §6.2)",
			ErrBaseRepoState, ErrBaseConfigSteering)
	}

	originURL, gitErr := p.git(ctx, p.baseDir, "config", "--local", "--no-includes", "--get", "remote.origin.url")
	if gitErr != nil {
		return fmt.Errorf("%w: %s has no origin remote (fail closed, no auto-repair — SPEC §6.2): %v",
			ErrBaseRepoState, p.baseDir, gitErr)
	}
	if originURL != p.remoteURL {
		return fmt.Errorf("%w: %s origin is %q but the workflow tracks %q (fail closed, no auto-repair — SPEC §6.2)",
			ErrBaseRepoState, p.baseDir, originURL, p.remoteURL)
	}
	return nil
}

// temporaryRemoteRepository creates the configuration boundary for a remote
// operation. Its empty template and fresh config are daemon-authored; base.git
// is deliberately not a repository discovery parent. ScratchRoot comes from
// daemon state and is neither reported as a workspace path nor included in any
// writable root declared by the selected agent kind, so a concurrent run cannot
// substitute config between initialization and exec (#231).
func (p *Provider) temporaryRemoteRepository(ctx context.Context, objectFormat string) (repo string, cleanup func(), err error) {
	if err := os.MkdirAll(p.scratchRoot, 0o700); err != nil {
		return "", nil, fmt.Errorf("workspace: create daemon scratch root: %w", err)
	}
	root, err := os.MkdirTemp(p.scratchRoot, "ben-remote-")
	if err != nil {
		return "", nil, fmt.Errorf("workspace: create remote scratch directory: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(root) }
	template := filepath.Join(root, "template")
	if err := os.Mkdir(template, 0o700); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("workspace: create empty Git template: %w", err)
	}
	repo = filepath.Join(root, "repo.git")
	initArgs := []string{"init", "--quiet", "--bare", "--template=" + template}
	if objectFormat != "" {
		initArgs = append(initArgs, "--object-format="+objectFormat)
	}
	initArgs = append(initArgs, repo)
	if _, err := p.git(ctx, root, initArgs...); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("workspace: initialize remote scratch repository: %w", err)
	}
	return repo, cleanup, nil
}

// isolatedRemoteGit runs a remote command where no repository-local config a
// run can author is discoverable. It is for remote reads that do not retain
// objects; fetchRemoteRef is the corresponding object-import path.
func (p *Provider) isolatedRemoteGit(ctx context.Context, args ...string) (string, error) {
	repo, cleanup, err := p.temporaryRemoteRepository(ctx, "")
	if err != nil {
		return "", err
	}
	defer cleanup()
	return p.remoteGit(ctx, repo, args...)
}

// fetchRemoteRef fetches through a fresh repository while sharing only
// base.git's content-addressed object store, then advances the requested cache
// ref explicitly. Holding baseMu preserves the existing serialization of all
// cache object/ref writes. Crucially, the credentialed process reads the fresh
// repository's config, never base.git/config; validation timing is therefore
// irrelevant to where it connects.
func (p *Provider) fetchRemoteRef(ctx context.Context, remoteRef, cacheRef string) (string, error) {
	negotiationTip, tipExists, err := p.revParse(ctx, cacheRef)
	if err != nil {
		return "", err
	}

	p.locks.baseMu.Lock()
	defer p.locks.baseMu.Unlock()
	objectFormat, err := p.git(ctx, p.baseDir, "rev-parse", "--show-object-format")
	if err != nil || objectFormat == "" || strings.ContainsAny(objectFormat, " \t\r\n") {
		return "", fmt.Errorf("%w: cannot determine base object format %q: %v",
			ErrBaseRepoState, objectFormat, err)
	}

	repo, cleanup, err := p.temporaryRemoteRepository(ctx, objectFormat)
	if err != nil {
		return "", err
	}
	defer cleanup()

	objectDir := filepath.Join(p.baseDir, "objects")
	const (
		scratchRef     = "refs/ben/fetch"
		negotiationRef = "refs/ben/have"
	)
	if tipExists {
		// The scratch repository has no run-authored refs. Give negotiation only
		// the previous value of the cache ref being advanced: if origin knows it,
		// upload-pack excludes its reachable history; if not, protocol negotiation
		// falls back to the complete pack without trusting the tip as evidence.
		if _, err := p.gitObjectDir(ctx, repo, objectDir,
			"update-ref", negotiationRef, negotiationTip); err != nil {
			return "", fmt.Errorf("workspace: seed remote negotiation: %w", err)
		}
	}
	if _, err := p.remoteGitObjectDir(ctx, repo, objectDir,
		"fetch", "--quiet", "--no-tags", p.remoteURL, "+"+remoteRef+":"+scratchRef); err != nil {
		return "", err
	}
	sha, err := p.gitObjectDir(ctx, repo, objectDir,
		"rev-parse", "--verify", scratchRef+"^{commit}")
	if err != nil || sha == "" {
		return "", fmt.Errorf("%w: fetched %s did not materialize: %v", ErrBaseRepoState, remoteRef, err)
	}
	if _, err := p.git(ctx, p.baseDir, "update-ref", cacheRef, sha); err != nil {
		return "", err
	}
	return sha, nil
}

// remoteDefaultBranch reads the remote HEAD symref. The credential-bearing URL
// is passed per-invocation and the command cannot discover base.git's config.
func (p *Provider) remoteDefaultBranch(ctx context.Context) (string, error) {
	out, err := p.isolatedRemoteGit(ctx, "ls-remote", "--symref", p.remoteURL, "HEAD")
	if err != nil {
		return "", fmt.Errorf("workspace: resolve remote default branch: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if name, _, found := strings.Cut(rest, "\t"); found && name != "" {
				return name, nil
			}
		}
	}
	// The remote URL is credential-free by construction: New refuses one that
	// embeds a credential (ErrRemoteCredentials), which is why the secret lives
	// behind AuthSource in the first place.
	return "", fmt.Errorf("%w: remote %s did not advertise a default branch (empty repository?)",
		ErrBaseBranchNotFound, p.remoteURL)
}

func (p *Provider) resolveTargetBranch(ctx context.Context) (string, error) {
	target := p.baseBranch
	if target == "" {
		var err error
		target, err = p.remoteDefaultBranch(ctx)
		if err != nil {
			return "", err
		}
	}
	if target == "ben" || strings.HasPrefix(target, branchPrefix) {
		return "", fmt.Errorf("%w: %q", ErrBaseBranchReserved, target)
	}
	ref := "refs/heads/" + target
	out, err := p.isolatedRemoteGit(ctx, "ls-remote", "--", p.remoteURL, ref)
	if err != nil {
		return "", fmt.Errorf("workspace: resolve base branch %q: %w", target, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if _, name, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok && name == ref {
			return target, nil
		}
	}
	return "", fmt.Errorf("%w: %q", ErrBaseBranchNotFound, target)
}

// fetchBase updates only the claim's retained target branch before an attempt
// (SPEC §6.2).
// The ben/* branch head is deliberately not fetched here: it may be checked
// out in a worktree, and fetch refuses to move checked-out refs — origin's
// view arrives via fetchRemoteIssueBranch instead.
func (p *Provider) fetchBase(ctx context.Context, def string) error {
	_, err := p.fetchRemoteRef(ctx, "refs/heads/"+def, "refs/heads/"+def)
	if err != nil {
		return fmt.Errorf("workspace: fetch before attempt: %w", err)
	}
	return nil
}

// fetchRemoteIssueBranch probes origin for the issue branch (SPEC §6.2
// remote-first reattach) and returns its head, "" when origin has no such
// branch. ls-remote's empty answer is the protocol's absence verdict — a
// failed probe is an error, never absence (#16 fail-closed). A present
// branch is fetched into refs/ben/remote/<key>, and the returned head is
// that ref's, so it always names objects this repository has.
func (p *Provider) fetchRemoteIssueBranch(ctx context.Context, key, branch string) (string, error) {
	ref := "refs/heads/" + branch
	out, err := p.isolatedRemoteGit(ctx, "ls-remote", "--", p.remoteURL, ref)
	if err != nil {
		return "", fmt.Errorf("workspace: probe origin for %s: %w", branch, err)
	}
	found := false
	for _, line := range strings.Split(out, "\n") {
		if _, r, ok := strings.Cut(line, "\t"); ok && r == ref {
			found = true
			break
		}
	}
	cacheRef := remoteRefPrefix + key
	if !found {
		// Keep the cache truthful: a leftover refs/ben/remote/* for a branch
		// origin no longer has would read as a remote fact that isn't.
		if _, ok, err := p.revParse(ctx, cacheRef); err != nil {
			return "", err
		} else if ok {
			if _, err := p.baseGit(ctx, "update-ref", "-d", cacheRef); err != nil {
				return "", err
			}
		}
		return "", nil
	}
	sha, err := p.fetchRemoteRef(ctx, ref, cacheRef)
	if err != nil {
		return "", fmt.Errorf("workspace: fetch origin %s: %w", branch, err)
	}
	return sha, nil
}

// reconcileBranch aligns the local issue branch with origin's view of it
// before the worktree attaches (SPEC §6.2 remote-first; #16). remoteSHA ""
// (origin has no branch) and an absent local branch are both no-ops. Equal
// heads and a local branch strictly ahead (unpushed work) attach as-is; a
// local branch strictly behind fast-forwards — through the worktree at
// checkoutPath when the branch is checked out there, so index and files
// follow the ref, or by compare-and-swap update-ref when it is free. True
// divergence means fast-forwarding either way would discard someone's
// commits: refuse, touch nothing.
func (p *Provider) reconcileBranch(ctx context.Context, branch, remoteSHA, checkoutPath string) error {
	if remoteSHA == "" {
		return nil
	}
	localSHA, ok, err := p.revParse(ctx, "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if !ok || localSHA == remoteSHA {
		return nil
	}
	behind, err := p.isAncestor(ctx, localSHA, remoteSHA)
	if err != nil {
		return err
	}
	if behind {
		if checkoutPath == "" {
			// The old value makes this compare-and-swap: if anything moved
			// the branch since it was read, refuse rather than overwrite.
			if _, err := p.baseGit(ctx, "update-ref", "refs/heads/"+branch, remoteSHA, localSHA); err != nil {
				return fmt.Errorf("%w: cannot fast-forward %s to origin's %s: %v",
					ErrWorkspaceState, branch, remoteSHA, err)
			}
			return nil
		}
		p.locks.baseMu.Lock()
		_, ffErr := p.git(ctx, checkoutPath, "merge", "--ff-only", remoteSHA)
		p.locks.baseMu.Unlock()
		if ffErr != nil {
			// Typically leftover uncommitted changes colliding with the
			// incoming commits — ambiguous, keep everything (SPEC §6.6).
			return fmt.Errorf("%w: cannot fast-forward %s to origin's %s in %s: %v",
				ErrWorkspaceState, branch, remoteSHA, checkoutPath, ffErr)
		}
		return nil
	}
	ahead, err := p.isAncestor(ctx, remoteSHA, localSHA)
	if err != nil {
		return err
	}
	if ahead {
		return nil // strictly ahead: unpushed local work, origin will catch up
	}
	return fmt.Errorf("%w: %s is %s locally but %s on origin, and neither descends from the other — refusing to choose (#16)",
		ErrBranchDiverged, branch, localSHA, remoteSHA)
}

// addWorktree creates the worktree, reattaching the branch when it already
// exists and creating it at the pinned base otherwise — never -B:
// force-recreating the branch has discarded agent commits in production
// (SPEC §6.2). On failure it prunes stale registrations and retries exactly
// once (SPEC §6.6), then fails closed keeping whatever exists on disk.
func (p *Provider) addWorktree(ctx context.Context, ws *core.Workspace, baseSHA string) error {
	if err := os.MkdirAll(p.issuesDir, 0o755); err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	// infra records ref-inspection failures, which mean repository trouble —
	// they must propagate as-is, not enter the prune-and-retry path.
	var infra error
	add := func() error {
		// Recheck the branch on each try: a failed `add -b` may still have
		// created the branch before dying.
		_, branchExists, err := p.revParse(ctx, "refs/heads/"+ws.Branch)
		if err != nil {
			infra = err
			return err
		}
		var args []string
		if branchExists {
			args = []string{"worktree", "add", ws.Path, ws.Branch}
		} else {
			args = []string{"worktree", "add", "-b", ws.Branch, ws.Path, baseSHA}
		}
		_, err = p.baseGit(ctx, args...)
		return err
	}
	if firstErr := add(); firstErr != nil {
		if infra != nil {
			return infra
		}
		if _, pruneErr := p.baseGit(ctx, "worktree", "prune"); pruneErr != nil {
			return fmt.Errorf("%w: worktree add failed (%v) and prune failed (%v)",
				ErrWorkspaceState, firstErr, pruneErr)
		}
		if retryErr := add(); retryErr != nil {
			if infra != nil {
				return infra
			}
			return fmt.Errorf("%w: worktree add failed after prune-and-retry, keeping state for forensics: %v (first attempt: %v)",
				ErrWorkspaceState, retryErr, firstErr)
		}
	}
	// Normative post-check (SPEC §6.6): the registration must be visible.
	entries, err := p.listWorktrees(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.bare && samePath(e.path, ws.Path) && e.branch == "refs/heads/"+ws.Branch {
			ws.CreatedNow = true
			return nil
		}
	}
	return fmt.Errorf("%w: worktree add reported success but %s is not registered on %s",
		ErrWorkspaceState, ws.Path, ws.Branch)
}

// lock serializes all workspace operations for one issue (SPEC §6.4, §6.6).
func (p *Provider) lock(key string) (unlock func()) {
	m := p.locks.forIssue(key)
	m.Lock()
	return m.Unlock
}

// lockPoll is how often a bounded acquisition re-offers for a mutex. sync.Mutex
// has TryLock and no channel, so "acquire or give up when ctx expires" is a poll;
// the interval only bounds the latency of noticing, and every caller of it is
// already prepared to wait seconds.
const lockPoll = 20 * time.Millisecond

// lockUntil acquires mu, or gives up when ctx does.
//
// It exists because these mutexes are held across `git fetch` — an operation whose
// only bound is the caller's own context — so a lock wait is not a short wait, and
// a caller that must be able to stop waiting cannot use Lock. Every ordinary
// caller still does: giving up is only correct where *not answering* is a
// legitimate result, which for the §9.6 prior-attempt account it is (AttemptFacts)
// and for a §9.7 verdict it is not.
func lockUntil(ctx context.Context, mu *sync.Mutex) (unlock func(), err error) {
	for {
		if mu.TryLock() {
			return mu.Unlock, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}

// pathsFor states this provider's layout for one issue (SPEC §6.2), in one
// place. Prepare reports it, Dispose removes what it names, and Sweep
// reconstructs it from a directory listing — three callers that must agree
// about where a workspace's directories are, which is the same reason §7.1
// forbids an adapter a fourth spelling of it.
func (p *Provider) pathsFor(key string) core.WorkspacePaths {
	return core.WorkspacePaths{
		Path:         filepath.Join(p.issuesDir, key),
		SharedGitDir: p.baseDir,
		PrivateDir:   filepath.Join(p.privateDir, key),
	}
}

// checkContained enforces safety invariant 2 (SPEC §6.3) over **every path a
// Workspace reports**, not just the worktree. The other two are handed to
// adapters and bound into sandbox postures (SPEC §10.1), so checking only the
// worktree would verify the one path that is already the agent's writable tree
// and skip the two that decide what else it may reach.
//
// Each path is checked against the directory this provider places it in, and
// each of those against the root, so both links have to hold. That matters
// twice over: macOS aliases /var to /private/var, which breaks naive prefix
// comparison, and issues/ or private/ could themselves have been replaced by a
// symlink pointing outside the root — a substitution the per-path check alone
// would follow without noticing.
//
// The private dir then gets a second check in the opposite direction: it MUST
// lie outside the worktree. Containment cannot express that — a directory
// inside the worktree is under the root too — and it is the property the
// private dir exists for, since state the repository must not carry is only
// kept out of a commit by being somewhere git is not looking (SPEC §6.2).
// Today's layout makes it siblings, so this holds structurally; it is checked
// anyway, because a layout change is exactly when it would stop holding and
// nothing else in this file would notice.
func (p *Provider) checkContained(ws core.WorkspacePaths) error {
	targets := []struct{ what, path, under string }{
		{"workspace path", ws.Path, p.issuesDir},
		{"shared git dir", ws.SharedGitDir, p.wfDir},
		{"private dir", ws.PrivateDir, p.privateDir},
	}
	for _, t := range targets {
		if t.path == "" {
			// Never a pass. An unreported path is a caller that did not go
			// through pathsFor, and treating the empty string as "nothing to
			// check" would let it through the one gate that exists to stop it.
			return fmt.Errorf("%w: %s is empty; every path a Workspace reports must be stated (SPEC §6.1)",
				ErrPathEscape, t.what)
		}
	}
	// Before containment, not after, so that this rule is the one that answers
	// for it. Under today's layout a private dir inside the worktree is also
	// outside private/, so a later check would refuse the same input for the
	// wrong reason — and a test could not tell the two apart, which is how a
	// rule stops being enforced without anything going red.
	if samePath(ws.PrivateDir, ws.Path) || strictlyUnder(normalizePath(ws.PrivateDir), normalizePath(ws.Path)) {
		return fmt.Errorf("%w: private dir %s is inside the worktree %s, which is the one thing it must not be (SPEC §6.3)",
			ErrPathEscape, ws.PrivateDir, ws.Path)
	}
	root := normalizePath(p.root)
	for _, t := range targets {
		container := normalizePath(t.under)
		if !strictlyUnder(container, root) {
			return fmt.Errorf("%w: %s resolves outside workspace root %s",
				ErrPathEscape, t.under, p.root)
		}
		if !strictlyUnder(normalizePath(t.path), container) {
			return fmt.Errorf("%w: %s %s is not under %s", ErrPathEscape, t.what, t.path, t.under)
		}
	}
	return nil
}

func strictlyUnder(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != "." && rel != ".." && !filepath.IsAbs(rel) &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// normalizePath resolves symlinks on the deepest existing ancestor so that
// containment checks compare real paths even when the target is gone.
func normalizePath(path string) string {
	cur := filepath.Clean(path)
	if abs, err := filepath.Abs(cur); err == nil {
		cur = abs
	}
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, suffix)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return filepath.Join(cur, suffix)
		}
		suffix = filepath.Join(filepath.Base(cur), suffix)
		cur = parent
	}
}

func samePath(a, b string) bool {
	return normalizePath(a) == normalizePath(b)
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
