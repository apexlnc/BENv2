package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
	"github.com/srhg-ai-7cef3f93/ben/internal/review"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewctl"
	"github.com/srhg-ai-7cef3f93/ben/internal/reviewrun"
)

// The review leg of assembly (#204, superseding #11's GitHub Actions
// deployment).
//
// It binds four things that are strangers by design and meet only here: the
// policy reducer and its forge client (internal/reviewctl), the
// substrate-neutral reviewer execution boundary (internal/reviewrun), whichever
// executor the declared substrate selects, and the workspace-cycle strategy that
// says which sandbox an issue's review runs in (internal/remotews). None of them
// may import the orchestrator and the orchestrator may import none of them —
// the two halves still meet only at SPEC §8.4's published milestone in and a
// `COMMENT` review plus one unassignment-or-revocation out, which is exactly the
// coupling #11 established and #204 preserves.
//
// **There is no repository workflow, no `workflow_dispatch`, and no model
// process on a GitHub runner.** The controller is a component of this daemon,
// running on its ordinary poll/sweep lifecycle; an operator command
// (cmd/benreview) exists for a dry run and a manual reconcile and is explicitly
// not the availability mechanism.

var (
	// ErrNoReviewCredential refuses an enabled controller whose credential could
	// not be constructed. The controller's identity is what every author check on
	// the forge is decided against, so one it cannot authenticate as is one that
	// would do nothing at all — quietly, and forever.
	ErrNoReviewCredential = errors.New("the review controller's credential could not be constructed")

	// ErrNoConfiguredRepository refuses a tracker that cannot name the forge
	// repository used by Airlock Git scopes and, when enabled, review actions.
	ErrNoConfiguredRepository = errors.New("tracker cannot name its configured repository")
)

// reviewLeg is what `ben run` holds between building the controller and running
// its sweep. Nil when `review.enabled` is false, which is every workflow that
// predates this section.
type reviewLeg struct {
	controller *reviewctl.Controller
	session    *reviewrun.Session
	interval   time.Duration
	log        *slog.Logger
}

// buildReview constructs the controller for one workflow definition, or returns
// nil for a workflow that does not run one.
//
// The order is deliberate and mirrors buildRemoteWorkspace's: prove the
// credential, then resolve the identity the reducer needs, then select the
// executor, then wire the session. Each stage's refusal is one an operator can
// act on without reading the next.
func (b *builder) buildReview(
	ctx context.Context, def *config.WorkflowDefinition, cred core.CredentialSource,
	substrate *airlock.Substrate, bundle *orchestrator.Bundle,
) (*reviewLeg, error) {
	rc := def.Config.Review
	if !rc.Enabled {
		return nil, nil
	}
	if cred == nil {
		return nil, fmt.Errorf("%w: review.auth_source resolved to no credential source", ErrNoReviewCredential)
	}
	// Prove the source at assembly, then discard this value. The forge client
	// resolves through the source for every request, whose own cache understands
	// the issuer's safe lifetime; no token is pinned for the daemon's lifetime.
	if _, err := cred.FetchFresh(ctx, core.PurposeReview); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotReady, err)
	}

	owner, repoName, err := configuredRepository(def)
	if err != nil {
		return nil, err
	}
	repository, err := reviewRepositoryIdentity(substrate, bundle, owner+"/"+repoName)
	if err != nil {
		return nil, err
	}
	guidance, err := reviewGuidance(rc.GuidanceFile)
	if err != nil {
		return nil, err
	}

	session, err := b.buildReviewSession(def, substrate, bundle, cred, repository)
	if err != nil {
		return nil, err
	}

	controller, err := reviewctl.New(reviewctl.Options{
		Policy: review.Config{
			Owner:                        owner,
			Repo:                         repoName,
			Principal:                    rc.Principal,
			TrackerAuthor:                rc.TrackerAuthor,
			Controller:                   rc.Controller,
			AllowSharedTrackerController: rc.AllowSharedTrackerController,
			RequiredLabels:               append([]string(nil), def.Config.Tracker.RequiredLabels...),
			QueueLabel:                   rc.QueueLabel,
			AddHumanReviewLabel:          rc.AddHumanReviewLabel,
			RoundCap:                     rc.RoundCap,
			ReviewerProfiles:             sortedReviewerProfileNames(rc.ReviewerProfiles),
			DefaultReviewerProfile:       rc.ReviewerDefaultProfile,
		},
		Forge: reviewctl.NewCredentialClient(
			reviewAPI(rc.APIBaseURL), func(ctx context.Context) (string, error) {
				token, err := cred.Fetch(ctx, core.PurposeReview)
				if err != nil {
					return "", err
				}
				return token.Value, nil
			}, owner, repoName, ms(rc.RequestTimeoutMS)).
			WithLog(func(format string, args ...any) {
				b.log.Info("review forge: " + fmt.Sprintf(format, args...))
			}),
		Reviewer:     reviewctl.Over(session),
		Repository:   repository,
		MaxDiffBytes: rc.MaxDiffBytes,
		Log: func(format string, args ...any) {
			b.log.Info("review controller: " + fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		return nil, fmt.Errorf("%w: review controller: %w", ErrConstruct, err)
	}
	// Composed once here so the argv, the prompt contract and the guidance are
	// proven to compose *before* a claim depends on them, rather than at the
	// first published milestone.
	if err := validateReviewCompositions(rc, guidance, owner, repoName); err != nil {
		return nil, fmt.Errorf("%w: reviewer invocation: %w", ErrConstruct, err)
	}
	b.log.Info("review controller assembled",
		"controller", rc.Controller, "queue_label", rc.QueueLabel, "round_cap", rc.RoundCap,
		"substrate", def.Config.Substrate.Kind, "interval_ms", rc.IntervalMS,
		"reviewer_default_profile", rc.ReviewerDefaultProfile,
		"reviewer_profiles", sortedReviewerProfileNames(rc.ReviewerProfiles))
	if rc.AllowSharedTrackerController {
		b.log.Warn("review controller is using shared GitHub App attribution; tracker/controller provenance is not independent",
			"controller", rc.Controller, "deployment_mode", def.Config.Deployment.Mode)
	}
	return &reviewLeg{
		controller: controller,
		session:    session,
		interval:   ms(rc.IntervalMS),
		log:        b.log,
	}, nil
}

func reviewRepositoryIdentity(
	substrate *airlock.Substrate, bundle *orchestrator.Bundle, local string,
) (string, error) {
	if substrate == nil {
		return local, nil
	}
	provider, ok := bundle.Workspaces.(*remotews.Provider)
	if !ok {
		return "", fmt.Errorf("%w: review controller: %T is not a remote workspace strategy",
			ErrConstruct, bundle.Workspaces)
	}
	// The backend claim is keyed by the mirror's canonical repository identity,
	// not the forge's owner/repo display name. A process ref using the latter
	// cannot resolve the sandbox acquired under the former.
	return provider.Repository(), nil
}

// buildReviewSession selects the executor the declared substrate implies.
//
// The fork is total, exactly as the workspace fork is: a review runs entirely
// as a local child or entirely as one durable backend process. There is no
// fallback edge between them — a remote configuration whose sandbox cannot be
// resolved parks the occurrence rather than starting a local reviewer, because
// a local reviewer is not a degraded remote one, it is a different trust
// boundary.
func (b *builder) buildReviewSession(
	def *config.WorkflowDefinition, substrate *airlock.Substrate,
	bundle *orchestrator.Bundle, cred core.CredentialSource, repository string,
) (*reviewrun.Session, error) {
	rc := def.Config.Review
	guidance, err := reviewGuidance(rc.GuidanceFile)
	if err != nil {
		return nil, err
	}

	opts := reviewrun.Options{
		Store:    reviewrun.NewDirStore(b.reviewDir),
		Compose:  reviewctl.ProfiledInvocations(rc.ReviewerArgv, rc.ReviewerProfiles, guidance),
		Deadline: ms(rc.TimeoutMS),
		Logger:   b.log,
		// What the trusted process holds and the reviewer must not. Compared by
		// value against the composed request before it can reach an executor,
		// which is the check #204's security acceptance asserts against the wire.
		Secrets: reviewSecrets(cred),
	}

	if substrate == nil {
		local, err := reviewrun.NewLocal(reviewrun.LocalOptions{
			Timeout:     ms(rc.TimeoutMS),
			Passthrough: rc.ReviewerEnv,
			Logger:      b.log,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: local reviewer: %w", ErrConstruct, err)
		}
		opts.Executor = local
	} else {
		if len(rc.ReviewerEnv) > 0 {
			// A sandbox has no host environment to pass through, and a request
			// carrying one would be BEN exporting a credential into a machine it
			// does not own. Refused before anything else here, because it is a
			// statement in the file rather than a property of the assembly.
			return nil, fmt.Errorf("%w: review.reviewer_env is set under substrate.kind %q: a sandbox "+
				"authenticates its own model calls from its profile, and nothing from this host crosses to it",
				ErrConstruct, def.Config.Substrate.Kind)
		}
		invocations := reviewerInvocations(rc)
		fields := make([]string, 0, len(invocations))
		for field := range invocations {
			fields = append(fields, field)
		}
		slices.Sort(fields)
		for _, field := range fields {
			argv := invocations[field]
			if err := requireReadOnlyRemoteCodex(argv); err != nil {
				return nil, fmt.Errorf("%w: %s: %v", ErrConstruct, field, err)
			}
		}
		provider, ok := bundle.Workspaces.(*remotews.Provider)
		if !ok {
			return nil, fmt.Errorf("%w: review controller: %T is not a remote workspace strategy",
				ErrConstruct, bundle.Workspaces)
		}
		remoteExec, err := reviewrun.NewRemote(reviewrun.RemoteOptions{
			Backend:       substrate.Processes,
			GitRepository: provider.GitRepository(),
			// The reviewer's clocks are the backend's to enforce while BEN is
			// disconnected, exactly as an attempt's are (docs/REMOTE.md). Both are
			// the review timeout: a review has no separate stall window, because a
			// model reading a bounded diff either answers or does not.
			Limits: core.RunLimits{StallTimeout: ms(rc.TimeoutMS), AttemptTimeout: ms(rc.TimeoutMS)},
			Logger: b.log,
		})
		if err != nil {
			return nil, fmt.Errorf("%w: remote reviewer: %w", ErrConstruct, err)
		}
		if err := checkReviewPromptDelivery(b.log, def, substrate.StdinLimits(), repository, guidance); err != nil {
			return nil, err
		}
		opts.Executor = remoteExec
		opts.Sandbox = cyclePlacement(provider)
	}

	session, err := reviewrun.New(opts)
	if err != nil {
		return nil, fmt.Errorf("%w: review session: %w", ErrConstruct, err)
	}
	return session, nil
}

// checkReviewPromptDelivery refuses, at assembly, a review bound the substrate
// could never deliver (#284).
//
// The operator's knob is review.max_diff_bytes, which bounds the diff; the
// profile's bounds are on the whole prompt — inline in the start request up to
// one limit, streamed after it up to another. The first is handled by
// delivery: a prompt over the inline bound streams, and this only says so in
// the log. The second is not: a prompt over the profile's total stdin bound has
// no path at all, and a deployment whose largest possible prompt exceeds it
// would discover that at its first large pull request, as a refusal on the
// issue. It is a configuration fact knowable here, so it is refused here.
//
// Unknown limits — a substrate whose readiness probe did not run — check
// nothing, which is the pre-#284 posture: the backend judges each request and
// a refusal is a definite, surfaced answer rather than an unresolved start.
func checkReviewPromptDelivery(
	log *slog.Logger, def *config.WorkflowDefinition, limits airlock.StdinLimits, repository, guidance string,
) error {
	if !limits.Known() {
		return nil
	}
	rc := def.Config.Review
	ceiling := reviewctl.PromptCeiling(repository, guidance, rc.MaxDiffBytes)
	if !limits.Admits(int64(ceiling)) {
		return fmt.Errorf("%w: review.max_diff_bytes %d composes a reviewer prompt of up to %d bytes, and profile %q "+
			"admits at most %d bytes of stdin per run (max_stdin_total_bytes); lower review.max_diff_bytes or raise the profile's limit",
			ErrConstruct, rc.MaxDiffBytes, ceiling, def.Config.Substrate.Airlock.Profile, limits.Total)
	}
	log.Info("remote reviewer prompt delivery",
		"profile", def.Config.Substrate.Airlock.Profile,
		"max_diff_bytes", rc.MaxDiffBytes, "prompt_ceiling_bytes", ceiling,
		"inline_limit_bytes", limits.Inline, "total_limit_bytes", limits.Total,
		"streams_above_inline", int64(ceiling) > limits.Inline)
	return nil
}

// requireReadOnlyRemoteCodex keeps a reviewer from modifying the retained
// workspace that a later coding revision resumes. Codex has accumulated aliases
// whose effect takes precedence over --sandbox (for example --yolo) and options
// that add writable roots, so this is a closed command grammar rather than a
// blacklist. A new Codex flag is refused until its effect is understood here.
// Other reviewer programs are model-neutral and remain governed by the
// substrate profile.
func requireReadOnlyRemoteCodex(argv []string) error {
	if len(argv) == 0 || filepath.Base(argv[0]) != "codex" {
		return nil
	}
	if len(argv) < 2 || argv[1] != "exec" {
		return errors.New("remote Codex review must use the closed `codex exec` command shape")
	}

	var sandbox, jsonOutput, stdinPrompt, model, effort bool
	for i := 2; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case stdinPrompt:
			return fmt.Errorf("remote Codex review has an argument after the stdin prompt marker: %q", arg)
		case arg == "--json":
			if jsonOutput {
				return errors.New("remote Codex review repeats --json")
			}
			jsonOutput = true
		case arg == "--skip-git-repo-check":
			// The local reviewer needs this because it runs in a fresh non-git
			// directory. It changes no sandbox or filesystem authority.
		case arg == "--sandbox" || arg == "-s":
			if sandbox || i+1 >= len(argv) || argv[i+1] != "read-only" {
				return errors.New("remote Codex review must use --sandbox read-only")
			}
			sandbox = true
			i++
		case strings.HasPrefix(arg, "--sandbox="):
			if sandbox || arg != "--sandbox=read-only" {
				return errors.New("remote Codex review must use --sandbox read-only")
			}
			sandbox = true
		case arg == "--model" || arg == "-m":
			if model || i+1 >= len(argv) || !codexReviewAtom(argv[i+1]) {
				return errors.New("remote Codex review must name one non-empty model value")
			}
			model = true
			i++
		case strings.HasPrefix(arg, "--model="):
			if model || !codexReviewAtom(strings.TrimPrefix(arg, "--model=")) {
				return errors.New("remote Codex review must name one non-empty model value")
			}
			model = true
		case arg == "--config" || arg == "-c":
			if effort || i+1 >= len(argv) || !codexReviewEffortOverride(argv[i+1]) {
				return errors.New("remote Codex review permits only one model_reasoning_effort=<token> config override")
			}
			effort = true
			i++
		case strings.HasPrefix(arg, "--config="):
			if effort || !codexReviewEffortOverride(strings.TrimPrefix(arg, "--config=")) {
				return errors.New("remote Codex review permits only one model_reasoning_effort=<token> config override")
			}
			effort = true
		case arg == "-":
			stdinPrompt = true
		default:
			return fmt.Errorf("remote Codex review option or argument %q is outside the closed read-only command shape", arg)
		}
	}
	if !sandbox {
		return errors.New("remote Codex review must declare --sandbox read-only")
	}
	if !jsonOutput {
		return errors.New("remote Codex review must declare --json")
	}
	if !stdinPrompt {
		return errors.New("remote Codex review must read its prompt from the final `-` argument")
	}
	return nil
}

func codexReviewAtom(value string) bool {
	if value == "" || strings.HasPrefix(value, "-") {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool { return r <= ' ' || r == 0x7f })
}

func codexReviewEffortOverride(override string) bool {
	key, value, ok := strings.Cut(override, "=")
	if !ok || key != "model_reasoning_effort" {
		return false
	}
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') ||
		(value[0] == '\'' && value[len(value)-1] == '\'')) {
		value = value[1 : len(value)-1]
	}
	if value == "" {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// cyclePlacement is the strategy's answer to "which sandbox does this issue's
// review run in".
//
// An attach to the workspace-cycle record followed by a resume of that exact
// existing identity: a review judges the tree a coding attempt produced, and an
// issue whose cycle holds no sandbox is a review that parks rather than
// allocating one. It is also the point at which a revocation-and-reapproval
// becomes unexpressible rather than discouraged — the strategy answers for the
// cycle the *standing* approval selects, so the retained sandbox of a withdrawn
// one has no address here.
func cyclePlacement(p *remotews.Provider) reviewrun.SandboxSource {
	return func(ctx context.Context, sub reviewrun.Subject) (reviewrun.Placement, error) {
		id, target, err := p.CycleIdentity(ctx, core.Issue{Identifier: sub.Issue}, sub.Cycle)
		if err != nil {
			return reviewrun.Placement{}, err
		}
		if id.Claim.Epoch != sub.Cycle {
			// The strategy's cycle and the controller's approval anchor are derived
			// independently — one from the tracker's change log at assembly, one
			// from the reducer's replay at the occurrence. Disagreement means the
			// approval moved between them, and reviewing anyway would judge one
			// cycle's tree under another cycle's authority.
			return reviewrun.Placement{}, fmt.Errorf(
				"%w: issue %s holds workspace cycle %d and this occurrence is anchored to %d",
				remotews.ErrCycleState, sub.Issue, id.Claim.Epoch, sub.Cycle)
		}
		if id.Claim.Repository != sub.Repository {
			return reviewrun.Placement{}, fmt.Errorf(
				"%w: issue %s holds repository identity %q and this review names %q",
				remotews.ErrCycleState, sub.Issue, id.Claim.Repository, sub.Repository)
		}
		if target != sub.TargetBranch {
			return reviewrun.Placement{}, fmt.Errorf(
				"%w: issue %s review targets %q and its durable workspace cycle targets %q",
				remotews.ErrCycleState, sub.Issue, sub.TargetBranch, target)
		}
		return reviewrun.Placement{
			Branch:       id.Branch,
			BaseSHA:      id.BaseSHA,
			TargetBranch: target,
			Sandbox:      id.SandboxID,
			Profile:      id.ProfileRevision,
		}, nil
	}
}

// reviewSecrets is what the trusted process holds, read fresh on every check.
//
// A function rather than a slice because a minted credential has a lifetime,
// and comparing a composed request against a stale copy would pass one carrying
// the current token. Errors are swallowed deliberately: this feeds a *refusal*,
// and an issuer that will not answer must not turn a leak check into a
// dispatch failure that hides it — the request is still checked against every
// value that could be read.
func reviewSecrets(cred core.CredentialSource) func() []string {
	return func() []string {
		var out []string
		if cred != nil {
			if token, err := cred.FetchFresh(context.Background(), core.PurposeReview); err == nil {
				out = append(out, token.Value)
			}
		}
		for _, name := range append(reviewrun.ForbiddenEnv(), reviewrun.ProviderEnv()...) {
			if v, ok := os.LookupEnv(name); ok && v != "" {
				out = append(out, v)
			}
		}
		return out
	}
}

// configuredRepository resolves `owner/repo` from the tracker's declaration.
//
// From the same key the tracker adapter reads rather than from a second
// spelling: the reducer validates a pull request's canonical URL against these
// two names, and a controller pointed at a repository the daemon does not work
// would refuse every subject it was handed while looking configured.
func configuredRepository(def *config.WorkflowDefinition) (owner, repo string, err error) {
	raw, _ := def.Config.Tracker.Provider["repo"].(string)
	owner, repo, ok := strings.Cut(strings.TrimSpace(raw), "/")
	if !ok || owner == "" || repo == "" {
		return "", "", fmt.Errorf("%w: tracker.provider.repo is %q", ErrNoConfiguredRepository, raw)
	}
	return owner, repo, nil
}

// reviewGuidance reads the deployment's own standard for what counts as a
// finding. Read at assembly rather than per review: a file that has moved under
// a running daemon would change what every in-flight round was judged against,
// and a missing one is a startup refusal an operator can act on.
func reviewGuidance(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: review.guidance_file: %w", ErrNotReady, err)
	}
	return string(body), nil
}

func reviewAPI(base string) string {
	if strings.TrimSpace(base) == "" {
		return "https://api.github.com"
	}
	return base
}

// reviewProbe is a syntactically complete subject used only to prove the
// invocation composes at startup. Nothing is dispatched for it.
func reviewProbe(owner, repo string) reviewrun.Subject {
	const zero = "0000000000000000000000000000000000000000"
	return reviewrun.Subject{
		Repository: owner + "/" + repo, Issue: "1",
		Cycle: 1, Occurrence: 1, Claim: 1, PR: 1, TargetBranch: "main", Base: zero, Head: zero,
	}
}

func reviewerInvocations(rc config.ReviewConfig) map[string][]string {
	if len(rc.ReviewerProfiles) > 0 {
		out := make(map[string][]string, len(rc.ReviewerProfiles))
		for name, argv := range rc.ReviewerProfiles {
			out["review.reviewer_profiles."+name] = argv
		}
		return out
	}
	return map[string][]string{"review.reviewer_argv": rc.ReviewerArgv}
}

func sortedReviewerProfileNames(profiles map[string][]string) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func validateReviewCompositions(rc config.ReviewConfig, guidance, owner, repo string) error {
	compose := reviewctl.ProfiledInvocations(rc.ReviewerArgv, rc.ReviewerProfiles, guidance)
	if len(rc.ReviewerProfiles) == 0 {
		_, err := compose(reviewProbe(owner, repo))
		return err
	}
	for _, name := range sortedReviewerProfileNames(rc.ReviewerProfiles) {
		probe := reviewProbe(owner, repo)
		probe.ReviewerProfile = name
		if _, err := compose(probe); err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
	}
	return nil
}

// Reconcile completes the startup survey of every retained review run before
// the sweep may dispatch new work.
//
// The same posture as the substrate's (docs/AIRLOCK.md), and for the same
// reason: what a restart cannot reconstruct is whether a run it dispatched is
// still executing, and starting a replacement into a live execution domain is
// the one mistake that cannot be undone by looking again.
func (r *reviewLeg) Reconcile(ctx context.Context) error {
	states, err := r.session.Reconcile(ctx)
	if err != nil {
		return err
	}
	for _, st := range states {
		r.log.Info("retained review run surveyed",
			"run", st.Run, "issue", st.Issue, "cycle", st.Cycle, "head", st.Head,
			"reviewer_profile", st.ReviewerProfile,
			"dispatched", st.Dispatched, "cursor", st.Cursor, "sealed", st.Sealed,
			"verdict", st.Verdict, "quiet", st.Quiet, "refused", st.Refused())
	}
	r.log.Info("review reconciliation complete", "runs", len(states))
	return nil
}

// Run is the controller's whole lifecycle: reconcile, then sweep on the
// interval until the context ends.
//
// A sweep rather than an event subscription, and the choice is #204's:
// delivery is a wake-up that can be missed and the durable state is on the
// forge either way, so a controller whose availability depended on a webhook
// would be one that silently stops. A failed sweep is warn-and-continue for
// SPEC §6.4's reason — a daemon that exited because the forge was briefly down
// would be less available than one that retries on the next tick.
func (r *reviewLeg) Run(ctx context.Context) {
	if err := r.Reconcile(ctx); err != nil {
		r.log.Warn("review reconciliation did not complete; sweeping anyway and retrying on later ticks", "error", err)
	}
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		if err := r.controller.Sweep(ctx); err != nil && ctx.Err() == nil {
			r.log.Warn("review sweep did not settle every issue; retrying on the next tick", "error", err)
		}
		select {
		case <-ctx.Done():
			r.log.Info("review controller stopped")
			return
		case <-t.C:
		}
	}
}
