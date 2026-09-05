package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/mirror"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
	"github.com/srhg-ai-7cef3f93/ben/internal/verify"
)

// The v2 dispatch leg of assembly (#205, completing #194).
//
// It binds five things that are strangers by design and meet only here: the
// Airlock backend (internal/airlock), the substrate-agnostic seams it implements
// (internal/remote), the SPEC §6.1 workspace strategy over them
// (internal/remotews), the daemon-side evidence store publication is proved from
// (internal/mirror, #193), and the provider adapter that owns the argv and the
// stream translator. None of those may import the orchestrator and the
// orchestrator may import none of them, which is exactly why the binding is the
// assembly's job (SPEC §11, internal/arch).
//
// The local leg is untouched. `substrate.kind: local` — or an omitted section —
// takes buildWorkspace and buildRunner exactly as it did before this file
// existed, and nothing here is reachable from it.

var (
	// ErrNoRemoteRunner refuses a remote substrate under an agent kind that has
	// not stated how its command is composed for a machine BEN does not own
	// (core.RemoteRunnerKind).
	//
	// A refusal rather than a fallback to the local argv, because the local argv
	// may name this host's paths, a wrapper binary and a generated policy file —
	// and submitting one of those to a sandbox is the "double wrapper" this
	// ticket exists to make unexpressible.
	ErrNoRemoteRunner = errors.New("this agent kind cannot be dispatched onto a v2 execution substrate")

	// ErrNoRemotePRSource refuses a remote substrate under a tracker that cannot
	// answer §9.7 leg 3 for a run BEN did not host (core.RemotePRSource). The
	// mirror of buildWorkspace's verifyTracker refusal, and for the same reason:
	// a verifier quietly missing a leg is worse than one that will not be built.
	ErrNoRemotePRSource = errors.New("tracker cannot answer for a remote pull request (SPEC §9.7 leg 3)")

	// ErrNoCycleSource refuses a remote substrate under a tracker whose change
	// log cannot be read. The workspace cycle is anchored to the standing
	// approval event (SPEC §6.7), and a strategy that could not read it would
	// have to guess which sandbox an approval selects.
	ErrNoCycleSource = errors.New("tracker cannot supply the change log a workspace cycle is anchored to")
)

// buildRemoteWorkspace constructs the v2 workspace strategy and the v2
// publish-evidence checker.
//
// The order mirrors buildWorkspace's, and the one addition is the reason this
// path exists at all: the checker is built over the *mirror* rather than over the
// workspace provider, because on this substrate the provider has nothing a run
// did not author. The two are still built together and from one tracker, so a
// checker can never be paired with a strategy from another generation.
func (b *builder) buildRemoteWorkspace(
	ctx context.Context, def *config.WorkflowDefinition, substrate *airlock.Substrate, bundle *orchestrator.Bundle,
) error {
	store, err := mirror.New(mirror.Options{
		Root:       b.mirrorDir,
		Repository: bundle.Repository,
		BaseBranch: def.Config.Workspace.BaseBranch,
		Logger:     b.log,
	})
	if err != nil {
		return fmt.Errorf("%w: daemon-side evidence store: %w", ErrConstruct, err)
	}
	// Prove the configured selector (or the repository default selected by
	// omission) exists before the daemon can dispatch. Each claim still resolves
	// and records its own target atomically with its base, so default movement
	// after startup cannot rewrite an existing claim's authority.
	if err := store.Ready(ctx); err != nil {
		return fmt.Errorf("%w: daemon-side evidence store: %w", ErrNotReady, err)
	}
	owner, repoName, err := configuredRepository(def)
	if err != nil {
		return err
	}

	cycles, ok := bundle.Tracker.(cycleTracker)
	if !ok {
		return fmt.Errorf("%w: %T", ErrNoCycleSource, bundle.Tracker)
	}
	provider, err := remotews.New(remotews.Options{
		Repository:    store.Repository(),
		GitRepository: owner + "/" + repoName,
		Workspaces:    substrate.Workspaces,
		Processes:     substrate.Processes,
		Journals:      remote.NewDirStore(b.journalDir),
		Consumptions:  remote.NewDirConsumer(b.journalDir),
		HookExec:      substrate.Hooks,
		HookStore:     remote.NewHookDirStore(b.journalDir),
		Hooks:         remoteHooksFrom(def.Config.Hooks),
		Base:          store,
		Cycles:        &trackerCycles{tracker: cycles, required: def.Config.Tracker.RequiredLabels},
		Store:         remotews.NewDirStore(b.cycleDir),
		Disposer:      substrateDisposer{substrate: substrate},
		Logger:        b.log,
	})
	if err != nil {
		return fmt.Errorf("%w: remote workspace strategy: %w", ErrConstruct, err)
	}

	prs, ok := bundle.Tracker.(core.RemotePRSource)
	if !ok {
		return fmt.Errorf("%w: %T", ErrNoRemotePRSource, bundle.Tracker)
	}
	checker, err := verify.NewRemote(store, store, prs, verify.RemoteExpectation{
		Repository: store.Repository(),
	})
	if err != nil {
		return fmt.Errorf("%w: remote publish-evidence checker: %w", ErrConstruct, err)
	}

	bundle.Workspaces = provider
	bundle.Verifier = &remoteVerifier{checker: checker, runs: provider, publish: provider}
	return nil
}

// buildRemoteRunner composes the backend into a core.AgentRunner.
//
// There is no §9.10 evidence sink here, and its absence is the design rather
// than an omission. Locally the sink upgrades a marker file with the execution
// domain's opaque evidence, because nothing else records it; the remote journal
// writes the run's identity *before* dispatch is attempted, so the marker
// question is already answered off that record (remotews's marker.go).
func (b *builder) buildRemoteRunner(
	ctx context.Context, def *config.WorkflowDefinition, kind core.RunnerKind,
	substrate *airlock.Substrate, bundle *orchestrator.Bundle,
) error {
	remoteKind, ok := kind.(core.RemoteRunnerKind)
	if !ok {
		return fmt.Errorf("%w: agent.kind %q", ErrNoRemoteRunner, def.Config.Agent.Kind)
	}
	provider, ok := bundle.Workspaces.(*remotews.Provider)
	if !ok {
		return fmt.Errorf("%w: remote runner: %T is not a remote workspace strategy", ErrConstruct, bundle.Workspaces)
	}
	binding := def.Config.AgentBinding()
	agent := config.AgentDescriptor(def)

	runner, err := substrate.Runner(airlock.RunnerConfig{
		Journals: remote.NewDirStore(b.journalDir),
		Consumer: remote.NewDirConsumer(b.journalDir),
		Bind: func(spec core.RunSpec) (remote.Binding, error) {
			// The context is deliberately the strategy's own bounded one rather
			// than the caller's: remote.Binder takes no context, and a Start that
			// hung on an unreachable control plane would hold the attempt open with
			// nothing to interrupt it.
			id, gitScope, err := provider.Bind(context.WithoutCancel(ctx), spec)
			if err != nil {
				return remote.Binding{}, err
			}
			return remote.Binding{
				Identity: id,
				Run:      remote.RunID(spec.Env["BEN_RUN_ID"]),
				Git:      gitScope,
				Meta: remote.Meta{
					TemplateRevision: templateRevision(def),
					PromptDigest:     promptDigest(spec.Prompt),
					Provider:         agent.Kind,
					Model:            agent.Model,
				},
			}, nil
		},
		Invoke: func(spec core.RunSpec) (remote.Invocation, error) {
			inv, err := remoteKind.RemoteInvocation(binding, spec)
			if err != nil {
				return remote.Invocation{}, err
			}
			return remote.Invocation{Argv: inv.Argv, Env: inv.Env, Stdin: inv.Stdin}, nil
		},
		Translate:    remoteKind.RemoteTranslate,
		Capabilities: remoteKind.RemoteCapabilities(),
		Logger:       b.log,
	})
	if err != nil {
		return fmt.Errorf("%w: agent.kind %q on substrate.kind %q: %w",
			ErrConstruct, def.Config.Agent.Kind, def.Config.Substrate.Kind, err)
	}
	if err := runner.Ready(ctx); err != nil {
		return fmt.Errorf("%w: agent.kind %q on substrate.kind %q: %w",
			ErrNotReady, def.Config.Agent.Kind, def.Config.Substrate.Kind, err)
	}
	// The resolver belongs only to the workspace strategy's authority-gated
	// recovery path. The fresh substrate used by the pre-tracker startup survey
	// deliberately never receives it: that pass is observational and cannot
	// replay a request for work whose approval may have been revoked while BEN
	// was down.
	bundle.Runner = runner
	bundle.ResolveRun = func(issue core.Issue, evidence core.RunEvidence, approval int64) (bool, error) {
		return provider.ResolveRun(issue, evidence, approval, runner.ResolveStart)
	}
	return nil
}

// remoteHooksFrom projects the workflow's four lifecycle scripts onto the
// substrate's own shape. Same scripts, same shared timeout, same order — only
// the shell's location differs (docs/REMOTE.md).
func remoteHooksFrom(h config.HooksConfig) remote.Hooks {
	return remote.Hooks{
		AfterCreate:  h.AfterCreate,
		BeforeRun:    h.BeforeRun,
		AfterRun:     h.AfterRun,
		BeforeRemove: h.BeforeRemove,
		Timeout:      ms(h.TimeoutMS),
	}
}

// cycleTracker is the change-log read the workspace cycle is anchored to,
// restated as the one method this assembly needs. It is core.TrackerAdapter's
// ClaimHistory, and the whole tracker is deliberately not handed to the strategy
// (remotews.CycleSource).
type cycleTracker interface {
	ClaimHistory(ctx context.Context, issue core.Issue) ([]core.ClaimEvent, error)
}

// trackerCycles resolves the standing approval-label event a workspace cycle is
// anchored to.
//
// It replays the change log for each required label exactly as §9.5's approving
// instant is computed (orchestrator.approvingInstant) — a label applied, removed
// and never re-applied is not a standing approval — and takes the *event id*
// rather than the timestamp. The id, because this is an identity rather than an
// ordering: two approvals a second apart must select two sandboxes, and a
// second-granularity timestamp cannot say they differ (SPEC §8.4).
//
// The **last** required label to be applied, for approvingInstant's reason:
// approval is not complete until the set is.
type trackerCycles struct {
	tracker  cycleTracker
	required []string
}

func (c *trackerCycles) WorkspaceCycle(ctx context.Context, issue core.Issue) (int64, error) {
	history, err := c.tracker.ClaimHistory(ctx, issue)
	if err != nil {
		return 0, err
	}
	return orchestrator.StandingApproval(history, c.required), nil
}

// substrateDisposer maps the strategy's outcomes and the resulting workspace
// reachability onto the backend's retention vocabulary.
//
// Exhaustively and never by conversion: the enums are distinct on purpose, and
// a numeric cast would compile today and start choosing the wrong policy the
// first time either grew a member.
type substrateCompleter interface {
	Complete(context.Context, remote.Claim, airlock.Outcome, remote.Status) (airlock.Disposal, error)
}

type substrateDisposer struct{ substrate substrateCompleter }

func (d substrateDisposer) Complete(
	ctx context.Context, claim remote.Claim, outcome remotews.Outcome, prev remote.Status,
) (remotews.Disposition, error) {
	var mapped airlock.Outcome
	switch outcome {
	case remotews.OutcomePublished:
		mapped = airlock.OutcomePublished
	case remotews.OutcomeFailed:
		mapped = airlock.OutcomeFailed
	case remotews.OutcomeRevoked:
		mapped = airlock.OutcomeRevoked
	case remotews.OutcomeShutdown:
		mapped = airlock.OutcomeShutdown
	default:
		return remotews.DispositionRetained,
			fmt.Errorf("ben: %s is not an outcome this substrate can dispose on", outcome)
	}
	disposal, err := d.substrate.Complete(ctx, claim, mapped, prev)
	if err != nil {
		return remotews.DispositionRetained, err
	}
	switch disposal {
	case airlock.DisposalRetain, airlock.DisposalSuspend:
		return remotews.DispositionRetained, nil
	case airlock.DisposalDelete:
		return remotews.DispositionDeleted, nil
	default:
		return remotews.DispositionRetained,
			fmt.Errorf("ben: %s is not a completed disposal this strategy recognizes", disposal)
	}
}

// runRefSource is the strategy's half of a verification's identity.
type runRefSource interface {
	RunRef(ws core.Workspace, verification string) (core.RemoteRunRef, error)
}

type remotePublisher interface {
	Publish(ctx context.Context, issue core.Issue, ws core.Workspace) error
}

// remoteVerifier adapts the v2 publish-evidence checker to the loop's seam.
//
// The translation is deliberately explicit about the one thing that must not
// happen: the attempt is built with a *nil* local workspace, so
// verify.SelectPublication refuses if anything ever tried to hand a remote
// attempt to the local checker (verify.ErrAmbiguousSubstrate,
// verify.ErrNoRemoteVerifier). The fallback is not a degradation — it is the
// sandbox verifying itself.
type remoteVerifier struct {
	checker *verify.RemoteChecker
	runs    runRefSource
	publish remotePublisher
	// seq mints a fresh verification identity per call. It is what makes "these
	// facts were observed for this question" checkable rather than assumed
	// (core.RemoteRunRef.Verification): a fact source replaying an earlier
	// answer fails the binding check instead of settling a later attempt.
	seq atomic.Uint64
}

func (v *remoteVerifier) Verify(
	ctx context.Context, issue core.Issue, ws core.Workspace,
) (orchestrator.VerifyResult, error) {
	if v.publish == nil {
		return orchestrator.VerifyResult{}, errors.New("remote publication has no trusted publisher")
	}
	if err := v.publish.Publish(ctx, issue, ws); err != nil {
		return orchestrator.VerifyResult{}, fmt.Errorf("trusted publication: %w", err)
	}
	run, err := v.runs.RunRef(ws, "v"+strconv.FormatUint(v.seq.Add(1), 36))
	if err != nil {
		return orchestrator.VerifyResult{}, err
	}
	publication, err := verify.SelectPublication(nil, v.checker, verify.Attempt{Issue: issue, Remote: &run})
	if err != nil {
		return orchestrator.VerifyResult{}, err
	}
	res, err := publication.Verify(ctx)
	if err != nil {
		// The zero VerifyResult is VerdictUnknown, so a caller that ignored this
		// error still could not read the result as success (SPEC §9.7).
		return orchestrator.VerifyResult{}, err
	}
	verdict, err := routableVerdict(res.Verdict)
	if err != nil {
		return orchestrator.VerifyResult{}, err
	}
	return orchestrator.VerifyResult{Verdict: verdict, PRURL: res.PRURL, Detail: res.Detail}, nil
}

// templateRevision identifies the prompt template an attempt rendered from
// (remote.Meta.TemplateRevision).
//
// A digest of the template source rather than a counter: the field's question is
// "which template was installed when this claim was taken", and a digest answers
// it across restarts, across daemons, and after a reload that put the previous
// template back — which a monotonic revision number cannot.
func templateRevision(def *config.WorkflowDefinition) string {
	return digestOf(def.PromptTemplate)
}

// promptDigest identifies the bytes an attempt actually sent (§9.5 has to be
// answerable after the fact). The remote journal also retains those bytes as
// operational replay input, but this is the compact identity diagnostics use.
func promptDigest(prompt string) string { return digestOf(prompt) }

func digestOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}
