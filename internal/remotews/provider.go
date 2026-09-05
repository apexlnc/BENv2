package remotews

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// EvidenceScheme names this substrate in a §9.10 run marker (core.RunEvidence).
//
// A scheme rather than an assumption, exactly as the local kernel-domain one is: the
// question "is the run this evidence identifies still going" is answered by a
// different mechanism here — a domain-quiet observation from a reachable backend
// rather than local kernel evidence — and a daemon handed the other
// substrate's evidence must refuse rather than interpret it.
//
// No Boot component, and its absence is a fact rather than an omission. Boot
// exists because a pid is unique only within one boot of one host; a workspace
// cycle is globally unique by construction (repository, issue, approval).
const EvidenceScheme = "remote-workspace"

// TrustedBase is the daemon-side git fact source behind the claim's pin and the
// remote-first restore — *mirror.Mirror in the assembly (#193).
//
// A seam declared by the consumer rather than a type taken from core, for the
// reason verify.RemoteFactSource is: this package needs three of the mirror's
// methods and owes nothing to a future fact source that has others.
//
// Everything here is read with the daemon's own credential from the canonical
// remote. Nothing in it passes through the sandbox, which is the whole reason a
// remote publication can be verified at all (SPEC §3.5).
type TrustedBase interface {
	// RecordClaim pins the claim-time base for one assignment epoch, before any
	// run for it can start. Idempotent within the epoch.
	RecordClaim(ctx context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error)
	// RecordClaimRetaining is RecordClaim with the old claim refs whose pins a
	// separately durable cycle-disposal record still owns. A new epoch may prune
	// anything else, but these survive until deletion is confirmed.
	RecordClaimRetaining(ctx context.Context, ref core.RemoteClaimRef, retained []core.RemoteClaimRef) (core.RemoteClaim, error)
	// Claim reads a recorded pin back, proving the store still holds it.
	Claim(ctx context.Context, ref core.RemoteClaimRef) (core.RemoteClaim, error)
	// RemoteFacts observes the canonical remote's head for the claim's branch,
	// ordered against the pin. It is what the restore targets and what §9.7 is
	// verified from.
	RemoteFacts(ctx context.Context, run core.RemoteRunRef) (core.RemotePublishFacts, error)
}

// ConsumptionStore retires a finished run's durable event inbox
// (remote.DirConsumer). See Options.Consumptions.
type ConsumptionStore interface {
	Discard(ctx context.Context, ref remote.ProcessRef) error
}

// AttemptSource is the optional daemon-side account of what an attempt left on
// the canonical branch, for the next attempt's prompt (SPEC §5.6, §9.6).
//
// Optional, and the only optional seam here, because it feeds a *prompt* rather
// than a verdict: a fact source that cannot answer costs a retry some context and
// changes no decision. A source that is absent reports nothing rather than
// guessing — an invented commit list in a continuation prompt is worse than a
// silent one, since the agent would act on it.
type AttemptSource interface {
	AttemptFacts(ctx context.Context, ref core.RemoteClaimRef) (core.AttemptFacts, error)
}

// CycleSource names the standing human approval a workspace cycle is anchored
// to (SPEC §6.7's approval act).
//
// It is a seam rather than a tracker because this package must not hold a
// tracker: §8.2's contract is the read kernel plus the closed write set, and a
// workspace strategy holding one would be a workspace strategy able to write the
// queue that dispatched it (SPEC §10.2). The assembly implements it over the
// tracker's change log, which is the same read §9.5 already makes.
type CycleSource interface {
	// WorkspaceCycle returns the tracker-native id of the standing approval-label
	// event. A non-positive answer is ErrApprovalUnknown at the caller: approval
	// nobody can date is approval this package will not anchor a sandbox to.
	WorkspaceCycle(ctx context.Context, issue core.Issue) (int64, error)
}

// Outcome is why a claim cycle is finished with its remote workspace. The
// ordinary SPEC §6.1 Dispose call can name the first two; the orchestrator's
// optional remote-lifecycle seam names revocation and shutdown without changing
// the locked local workspace contract.
type Outcome uint8

const (
	// OutcomePublished is a claim whose publication the daemon-side verifier
	// confirmed (#193). Never a backend success response.
	OutcomePublished Outcome = iota
	// OutcomeFailed is a failed or attempt-exhausted claim: §6.4's "kept for
	// forensics".
	OutcomeFailed
	// OutcomeRevoked is a claim taken away by tracker state: its approval label
	// was removed, its assignment moved, or its issue became terminal.
	OutcomeRevoked
	// OutcomeShutdown is a daemon stopping while the tracker claim stays held.
	OutcomeShutdown
)

func (o Outcome) String() string {
	switch o {
	case OutcomePublished:
		return "published"
	case OutcomeFailed:
		return "failed"
	case OutcomeRevoked:
		return "revoked"
	case OutcomeShutdown:
		return "shutdown"
	default:
		return fmt.Sprintf("Outcome(%d)", uint8(o))
	}
}

// Disposition reports whether Complete left the workspace addressable. Retain
// and suspend both do; only delete permits the strategy to retire the cycle
// identity that a later assignment under the same approval must reuse.
type Disposition uint8

const (
	DispositionRetained Disposition = iota
	DispositionDeleted
)

func (d Disposition) String() string {
	switch d {
	case DispositionRetained:
		return "retained"
	case DispositionDeleted:
		return "deleted"
	default:
		return fmt.Sprintf("Disposition(%d)", uint8(d))
	}
}

// Disposer applies the backend's configured end-of-claim retention policy under
// the one gate that is not configurable: nothing is suspended or deleted while a
// run's termination is unconfirmed (remote.MayReuse, SPEC §9.8).
type Disposer interface {
	Complete(ctx context.Context, claim remote.Claim, outcome Outcome, prev remote.Status) (Disposition, error)
}

// Options are what one strategy is constructed from. Every seam is required
// except Disposer's siblings noted below; a strategy missing one would fail at
// the first claim instead of at startup.
type Options struct {
	// Repository is the credential-free identity of the repository the claims
	// live in — mirror.Mirror.Repository(). It is part of the sandbox address, so
	// two repositories cannot collide on one workspace cycle.
	Repository string
	// GitRepository is the forge's operator-configured owner/name identifier.
	// It is distinct from Repository: the latter is a fingerprinted, durable
	// mirror identity and must never be sent as an Airlock Git scope. Airlock's
	// broker authorizes this value against its own repository policy.
	GitRepository string
	// Workspaces and Processes are the two backend seams: the workspace
	// lifecycle, and the run status the quiet gate is read from.
	Workspaces remote.WorkspaceBackend
	Processes  remote.ProcessBackend
	// Journals is the run journal store the runner writes. This package reads it
	// to answer §9.10's marker question and closes it once a run is confirmed
	// gone; it never dispatches through it.
	Journals remote.Store
	// Consumptions retires the durable event inbox of a run whose journal this
	// package has just closed (remote.DirConsumer.Discard). Optional: a consumer
	// with no cleanup keeps its logs, which costs disk and answers a later
	// Recover for a reference nothing will address again.
	//
	// Never called before the journal is closed. The inbox is what a restart
	// re-projects a terminal outcome from, so removing it over a run that may
	// still be live would discard the one copy of an outcome BEN has accepted and
	// not yet acted on (remote.DurableConsumer).
	Consumptions ConsumptionStore
	// HookExec and HookStore run the §5.2.6 lifecycle hooks and BEN's own restore
	// script in the sandbox, durably.
	HookExec  remote.HookExec
	HookStore remote.HookStore
	// Hooks are the workflow's four scripts and their shared timeout. The restore
	// script runs inside the same timeout domain, deliberately: it is a script in
	// the same sandbox with the same "a script BEN cannot signal holds the claim
	// forever" hazard, and a second bound would be a second thing to configure.
	Hooks remote.Hooks
	// Base is the daemon-side pin and canonical-remote observation.
	Base TrustedBase
	// Cycles names the standing approval.
	Cycles CycleSource
	// Store holds the workspace-cycle records.
	Store Store
	// Disposer applies the retention policy at the end of a claim.
	Disposer Disposer
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// StatusTimeout bounds the backend read RunGone makes, which has no context
	// of its own: the §9.10 seam is `func(core.RunEvidence) (bool, error)`, and a
	// daemon whose control plane hangs must not block recovery forever.
	StatusTimeout time.Duration
}

// DefaultStatusTimeout bounds RunGone's backend read. Long enough for a control
// plane under load, short enough that a recovery pass over many claims finishes.
const DefaultStatusTimeout = 30 * time.Second

// Provider is the SPEC §6.1 workspace strategy over a v2 execution substrate.
type Provider struct {
	repository    string
	gitRepository string
	backend       remote.WorkspaceBackend
	processes     remote.ProcessBackend
	journals      remote.Store
	consumptions  ConsumptionStore
	hookExec      remote.HookExec
	hookStore     remote.HookStore
	hooks         remote.Hooks
	base          TrustedBase
	cycles        CycleSource
	store         Store
	disposer      Disposer
	log           *slog.Logger
	statusWait    time.Duration
	// completionLocks serialize retries of one exact cycle address without
	// blocking a replacement cycle for the same issue. Two different cycles are
	// required to make progress concurrently (#266).
	completionLocks keyedLocks
}

// Repository is the canonical, credential-free identity this strategy uses in
// every backend claim. Consumers that address a process in one of its
// sandboxes must use this exact spelling rather than deriving owner/repo again.
func (p *Provider) Repository() string { return p.repository }

// GitRepository is the forge owner/name used only in Airlock's typed Git
// scopes. It is not a sandbox or workspace-cycle address.
func (p *Provider) GitRepository() string { return p.gitRepository }

// New validates the seams and builds the strategy. It touches nothing: a daemon
// whose remote path is never reached leaves no state behind.
func New(opts Options) (*Provider, error) {
	switch {
	case opts.Repository == "":
		return nil, errors.New("remotews: a strategy must know which repository its claims live in")
	case opts.GitRepository == "":
		return nil, errors.New("remotews: a strategy must know which forge repository its Git scopes target")
	case opts.Workspaces == nil || opts.Processes == nil:
		return nil, errors.New("remotews: a strategy needs both the workspace and process backends")
	case opts.Journals == nil:
		return nil, errors.New("remotews: a strategy needs the run journal store")
	case opts.HookExec == nil || opts.HookStore == nil:
		return nil, errors.New("remotews: a strategy needs a hook executor and its durable store")
	case opts.Base == nil:
		return nil, errors.New("remotews: a strategy needs a daemon-side trusted base (#193)")
	case opts.Cycles == nil:
		return nil, errors.New("remotews: a strategy needs a source for the standing approval anchor")
	case opts.Store == nil:
		return nil, errors.New("remotews: a strategy needs a workspace-cycle store")
	case opts.Hooks.Timeout <= 0:
		// Refused at construction rather than at the first hook, because the
		// alternative is a script on a backend BEN cannot signal holding the claim
		// forever (remote.RunScript).
		return nil, errors.New("remotews: a strategy needs a positive hook timeout")
	}
	p := &Provider{
		repository: opts.Repository, gitRepository: opts.GitRepository,
		backend: opts.Workspaces, processes: opts.Processes,
		journals: opts.Journals, consumptions: opts.Consumptions, hookExec: opts.HookExec, hookStore: opts.HookStore,
		hooks: opts.Hooks, base: opts.Base, cycles: opts.Cycles, store: opts.Store,
		disposer: opts.Disposer, log: opts.Logger, statusWait: opts.StatusTimeout,
	}
	if p.log == nil {
		p.log = slog.Default()
	}
	if p.statusWait <= 0 {
		p.statusWait = DefaultStatusTimeout
	}
	return p, nil
}

// Key is the sanitized workspace key for an issue, and it is internal/workspace's
// definition rather than a second one.
//
// The key names the branch, and the branch is the whole binding between an issue
// and the commits verified for it — the daemon-side fact source derives
// `ben/<key>` from it (mirror.Branch) and so does the v2 verifier. Two spellings
// of it would be two answers to "which branch is this claim's", and the one that
// disagreed would verify somebody else's work. So there is one definition and
// this package imports it, rather than restating a sanitizer that has to stay
// identical forever.
func Key(identifier string) string { return workspace.Key(identifier) }

// Branch is the canonical issue branch for a key (SPEC §6.3).
func Branch(key string) string { return "ben/" + key }

// BeginClaimBase records the pending intent for one assignment epoch, and — the
// part with no v1 counterpart — resolves which workspace cycle that assignment
// belongs to.
//
// Three shapes, and the third is the one this ticket exists for:
//
//   - No record: a first claim. The cycle is anchored to the standing approval.
//   - A record anchored to the *same* approval: an ordinary revision round, or a
//     controller-driven reassignment inside it. The sandbox address does not
//     move; only the verification epoch does, exactly as internal/workspace moves
//     it, outgoing pin and all.
//   - A record anchored to a *different* approval: the label was revoked and a
//     human approved again. That is a new workspace cycle. Before installing it,
//     BEN records the old cycle's own durable disposal obligation; the two then
//     have independent addresses and lifetimes.
func (p *Provider) BeginClaimBase(ctx context.Context, issue core.Issue, epoch int64) error {
	if epoch <= 0 {
		return fmt.Errorf("%w: claim epoch must be positive", ErrClaimEpoch)
	}
	key, err := p.key(issue)
	if err != nil {
		return err
	}
	approval, err := p.cycles.WorkspaceCycle(ctx, issue)
	if err != nil {
		return fmt.Errorf("remotews: resolving the workspace cycle for issue %s: %w", issue.Identifier, err)
	}
	if approval <= 0 {
		return fmt.Errorf("%w: issue %s", ErrApprovalUnknown, issue.Identifier)
	}

	return p.store.WithCycle(key, func() error {
		current, err := p.loadForTransition(key, issue.Identifier)
		switch {
		case errors.Is(err, ErrNoCycle):
			return p.store.SaveCycle(Cycle{
				Version: CycleVersion, Issue: issue.Identifier, Key: key,
				Repository: p.repository, Branch: Branch(key),
				Approval: approval, State: cyclePending, Epoch: epoch,
			})
		case err != nil:
			return err
		}

		if current.Approval != approval {
			p.log.Info("the approval that anchors this workspace cycle has moved; recording the previous cycle's "+
				"disposal before a new sandbox can be acquired",
				"issue", issue.Identifier, "previous_approval", current.Approval, "approval", approval)
			if previous, ok := cycleForDisposal(current); ok {
				obligation := CycleDisposal{
					Version: cycleDisposalVersion, Cycle: previous, ReplacementApproval: approval,
				}
				// Obligation first. A crash between these two writes leaves the old
				// cycle current, and completion refuses it until this retry publishes
				// the replacement; the reverse order loses the old address forever.
				if err := p.store.SaveCycleDisposal(obligation); err != nil {
					return err
				}
			}
			return p.store.SaveCycle(Cycle{
				Version: CycleVersion, Issue: issue.Identifier, Key: key,
				Repository: p.repository, Branch: Branch(key),
				Approval: approval, State: cyclePending, Epoch: epoch,
			})
		}

		// The same workspace cycle. From here the transition is
		// internal/workspace's, value for value, because it is the same §6.2
		// rule about the same fact.
		switch current.State {
		case cyclePending:
			if current.Epoch != epoch {
				return fmt.Errorf("%w: issue %s is pending for epoch %d, not %d",
					ErrCycleState, issue.Identifier, current.Epoch, epoch)
			}
			if current.Version == legacyCycleVersion {
				return fmt.Errorf("%w: issue %s epoch %d", ErrClaimTargetUnrecorded, issue.Identifier, epoch)
			}
			return nil
		case cyclePinned:
			if current.Epoch == epoch {
				if current.Version == legacyCycleVersion {
					return fmt.Errorf("%w: issue %s epoch %d", ErrClaimTargetUnrecorded, issue.Identifier, epoch)
				}
				return nil
			}
			next := current
			next.Version = CycleVersion
			next.State, next.Epoch, next.BaseSHA = cyclePending, epoch, ""
			next.OutgoingEpoch, next.OutgoingBaseSHA = current.Epoch, current.BaseSHA
			next.TargetBranch = ""
			next.OutgoingTargetBranch = current.TargetBranch
			return p.store.SaveCycle(next)
		}
		return fmt.Errorf("%w: issue %s has non-authorizing state %q", ErrCycleState, issue.Identifier, current.State)
	})
}

// cycleForDisposal projects a cycle's last authorizing pin into the immutable
// identity an end-of-cycle obligation owns. A pending first claim acquired no
// sandbox and owes nothing; a pending reassignment carries the previous pin in
// its outgoing fields and therefore still owns the cycle it was revising.
func cycleForDisposal(c Cycle) (Cycle, bool) {
	switch c.State {
	case cyclePinned:
	case cyclePending:
		if c.OutgoingEpoch <= 0 {
			return Cycle{}, false
		}
		c.State, c.Epoch, c.BaseSHA = cyclePinned, c.OutgoingEpoch, c.OutgoingBaseSHA
		c.TargetBranch = c.OutgoingTargetBranch
		if c.TargetBranch == "" {
			// A v1 pin has no target branch. BeginClaimBase upgrades the
			// transitional envelope to v2, but projecting its outgoing pin must
			// restore the version whose shape that pin actually satisfies.
			c.Version = legacyCycleVersion
		}
	default:
		return Cycle{}, false
	}
	c.OutgoingEpoch, c.OutgoingBaseSHA, c.OutgoingTargetBranch = 0, "", ""
	c.Superseded = 0
	return c, true
}

// ClaimBase reads the closed provider state without acquiring anything, running
// a hook, or asking the backend.
func (p *Provider) ClaimBase(ctx context.Context, issue core.Issue) (core.ClaimBase, error) {
	key, err := p.key(issue)
	if err != nil {
		return core.ClaimBase{}, err
	}
	c, err := p.loadForTransition(key, issue.Identifier)
	switch {
	case errors.Is(err, ErrNoCycle):
		return core.ClaimBase{State: core.ClaimBaseAbsent}, nil
	case err != nil:
		return core.ClaimBase{}, err
	}
	if c.Version == legacyCycleVersion {
		return c.ClaimBase(), fmt.Errorf("%w: issue %s epoch %d", ErrClaimTargetUnrecorded, c.Issue, c.Epoch)
	}
	if c.State != cyclePinned {
		return c.ClaimBase(), nil
	}
	// A pinned record is only authority while the daemon-side store still holds
	// the commit it names. internal/workspace checks the same thing against its
	// reachability ref; here the pin lives in the mirror, and a record whose pin
	// is gone must not authorize a verification whose leg 1 cannot be computed
	// (mirror.ErrClaimPinLost).
	pin, err := p.base.Claim(ctx, c.ClaimRef())
	if err != nil {
		return core.ClaimBase{}, fmt.Errorf("remotews: proving the claim-time base for issue %s: %w",
			issue.Identifier, err)
	}
	if pin.BaseSHA != c.BaseSHA || pin.TargetBranch != c.TargetBranch {
		return core.ClaimBase{}, fmt.Errorf("%w: issue %s records base %s and the trusted store pins %s",
			ErrCycleState, issue.Identifier, c.BaseSHA, pin.BaseSHA)
	}
	return c.ClaimBase(), nil
}

// AbandonPendingClaimBase rolls an unfinished epoch back to its outgoing pin, or
// to absence when it had none. Pinned state is retained unchanged (SPEC §6.2,
// §9.8).
//
// The record is removed rather than kept when there was no outgoing pin, which
// also retires the workspace cycle: a claim that ended before it ever pinned a
// base never acquired a sandbox either, since acquisition happens in the prepare
// that pins.
func (p *Provider) AbandonPendingClaimBase(ctx context.Context, issue core.Issue) error {
	key, err := p.key(issue)
	if err != nil {
		return err
	}
	return p.store.WithCycle(key, func() error {
		c, err := p.load(key, issue.Identifier)
		switch {
		case errors.Is(err, ErrNoCycle):
			return nil
		case err != nil:
			return err
		}
		if c.State != cyclePending {
			return nil
		}
		if c.OutgoingEpoch > 0 {
			next := c
			next.State, next.Epoch, next.BaseSHA = cyclePinned, c.OutgoingEpoch, c.OutgoingBaseSHA
			next.TargetBranch = c.OutgoingTargetBranch
			next.OutgoingEpoch, next.OutgoingBaseSHA = 0, ""
			next.OutgoingTargetBranch = ""
			if next.TargetBranch == "" {
				next.Version = legacyCycleVersion
			}
			return p.store.SaveCycle(next)
		}
		return p.store.DeleteCycle(key)
	})
}

// ResolveWorkspace names the workspace an issue's work lives in and reports its
// pinned epoch/base/target tuple, preparing nothing.
//
// It deliberately does not ask the backend. §9.7's evidence question is asked of
// the canonical remote and the forge, never of the sandbox, so a claim whose
// sandbox has been suspended or deleted since it published still has a workspace
// worth resolving — and a recovery pass that needed the control plane to be
// reachable in order to *name* a workspace would park every claim on an outage.
func (p *Provider) ResolveWorkspace(ctx context.Context, issue core.Issue) (core.Workspace, bool, error) {
	key, err := p.key(issue)
	if err != nil {
		return core.Workspace{}, false, err
	}
	c, err := p.load(key, issue.Identifier)
	switch {
	case errors.Is(err, ErrNoCycle):
		return core.Workspace{}, false, nil
	case err != nil:
		return core.Workspace{}, false, err
	}
	return c.Workspace(false), true, nil
}

// ListWorkspaces reports every retained workspace cycle, for §9.10 step 5.
func (p *Provider) ListWorkspaces(ctx context.Context) ([]core.WorkspaceRef, error) {
	cycles, err := p.store.Cycles()
	if err != nil {
		return nil, err
	}
	refs := make([]core.WorkspaceRef, 0, len(cycles))
	for _, c := range cycles {
		refs = append(refs, c.Ref())
	}
	return refs, nil
}

// EndedCycles reports every independently durable disposal still requiring
// work. A retained disposition is complete policy, so its record remains only
// as the address and pin of the allocation the operator chose to retain.
func (p *Provider) EndedCycles(ctx context.Context) ([]core.WorkspaceRef, error) {
	disposals, err := p.disposalRecords()
	if err != nil {
		return nil, err
	}
	refs := make([]core.WorkspaceRef, 0, len(disposals))
	for _, disposal := range disposals {
		if disposal.Disposition == dispositionRetained {
			continue
		}
		refs = append(refs, disposal.Ref())
	}
	return refs, nil
}

// disposalRecords holds every self-addressed obligation to this provider's
// repository before any caller can retain its pin or hand its embedded claim to
// the backend. Unlike a live cycle, completion resolves this record directly,
// so loadForTransition cannot provide this ownership check on its behalf.
func (p *Provider) disposalRecords() ([]CycleDisposal, error) {
	disposals, err := p.store.CycleDisposals()
	if err != nil {
		return nil, err
	}
	for _, disposal := range disposals {
		if err := p.validateDisposalRepository(disposal); err != nil {
			return nil, err
		}
	}
	return disposals, nil
}

func (p *Provider) validateDisposalRepository(disposal CycleDisposal) error {
	if disposal.Cycle.Repository != p.repository {
		return fmt.Errorf("%w: cycle disposal %s is recorded against repository %q, this strategy serves %q",
			ErrCycleState, disposal.Address(), disposal.Cycle.Repository, p.repository)
	}
	return nil
}

// AttemptFacts is the account of what the finished attempt left on the canonical
// branch (SPEC §9.6).
//
// Read daemon-side or not at all. There is a tempting second source — ask the
// sandbox for `git log` — and it is exactly the source SPEC §3.5 rules out: the
// commits it would report are the agent's own report of its own work, rendered
// into the next attempt's prompt as though BEN had observed them.
func (p *Provider) AttemptFacts(ctx context.Context, ws core.Workspace) (core.AttemptFacts, error) {
	source, ok := p.base.(AttemptSource)
	if !ok {
		return core.AttemptFacts{}, nil
	}
	c, err := p.load(ws.Key, "")
	switch {
	case errors.Is(err, ErrNoCycle):
		return core.AttemptFacts{}, nil
	case err != nil:
		return core.AttemptFacts{}, err
	}
	return source.AttemptFacts(ctx, c.ClaimRef())
}

func (p *Provider) key(issue core.Issue) (string, error) {
	if issue.Identifier == "" {
		return "", fmt.Errorf("%w: issue identifier is empty", ErrCycleState)
	}
	return Key(issue.Identifier), nil
}

// load reads a record and holds it to this strategy's repository and, when the
// caller knows it, to the issue it asked about.
//
// Both comparisons are refusals rather than repairs. A record for another
// repository under this key is a state directory serving two workflows, and a
// record for another issue under this key is a key collision — and in either case
// the recorded sandbox belongs to work this claim has nothing to do with.
func (p *Provider) load(key, identifier string) (Cycle, error) {
	c, err := p.loadForTransition(key, identifier)
	if err != nil {
		return Cycle{}, err
	}
	if c.Version == legacyCycleVersion {
		return Cycle{}, fmt.Errorf("%w: issue %s epoch %d", ErrClaimTargetUnrecorded, c.Issue, c.Epoch)
	}
	return c, nil
}

// loadForTransition is the sole tolerant read of a pre-#152 record. The next
// assignment may carry its base and workspace-cycle identity forward; all
// ordinary consumers go through load and refuse it.
func (p *Provider) loadForTransition(key, identifier string) (Cycle, error) {
	c, err := p.store.LoadCycle(key)
	if err != nil {
		return Cycle{}, err
	}
	if c.Repository != p.repository {
		return Cycle{}, fmt.Errorf("%w: %s is recorded against repository %q, this strategy serves %q",
			ErrCycleState, key, c.Repository, p.repository)
	}
	if identifier != "" && c.Issue != identifier {
		return Cycle{}, fmt.Errorf("%w: %s is recorded for issue %s, asked for %s",
			ErrCycleState, key, c.Issue, identifier)
	}
	return c, nil
}
