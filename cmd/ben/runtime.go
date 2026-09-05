package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock"
	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/registry"
	"github.com/srhg-ai-7cef3f93/ben/internal/state"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// The runtime builder: the other half of assembly decision 13's wiring, and the
// half `ben config effective` deliberately does not do. It turns one workflow
// definition into the adapter set that definition governs — `Structural` →
// `New` → `Ready` → `RepositoryFrom` → `workspace.New` → `verify.New` — and
// hands it back for the config watcher to publish beside the definition it was
// built from (SPEC §5.4, §5.7; #56).
//
// It lives in cmd/ben because it is the only place entitled to import the kind
// registry *and* the orchestrator, which is what internal/arch keeps true. The
// two packages must not know each other: the registry reaches every adapter, and
// the loop is defined by the narrow seams it declares for itself.

// Named so `ben run` can say which stage refused, which is what the operator
// needs: a malformed block is an edit, an unconstructible adapter is usually a
// value the block accepted and the adapter did not, and an unready one is the
// world — a missing credential, an unreachable host, an uninstalled harness.
// Collapsing them would send every one of those to the same first guess.
var (
	// ErrUnknownKind is a tracker.kind or agent.kind with no registered
	// adapter. Distinct from ErrStructural: nothing was asked to validate it.
	ErrUnknownKind = errors.New("no adapter is registered for this kind")
	// ErrStructural is a provider block the kind refuses. Pure, and reached
	// with no credentials and no network (SPEC §5.7).
	ErrStructural = errors.New("the workflow configuration is malformed")
	// ErrConstruct is an adapter that could not be built from a configuration
	// that passed Structural.
	ErrConstruct = errors.New("an adapter could not be constructed")
	// ErrNotReady is an adapter that cannot operate here and now: the
	// credential, the network, the installed harness, the base cache.
	ErrNotReady = errors.New("an adapter is not ready to run here")
	// ErrNoClaimPrincipalSource refuses assembly against a tracker that cannot
	// name the account its claims are assigned to (core.ClaimPrincipalSource).
	// The mirror of workspace.ErrNoRepositorySource, and named for the same
	// reason: it is a refusal tests assert on, and §9.8 cannot tell this
	// daemon's claim from a human's without an answer.
	ErrNoClaimPrincipalSource = errors.New("tracker cannot name its claim principal")
)

// builder holds what the kind lookups resolve through, so a test can supply
// kinds of its own.
//
// Injected rather than reached for, because the registry's tables are
// unexported by design and its kinds do real I/O at `Ready` — a network call
// and, for the runners, a subprocess. Nothing about the assembly this file
// performs is provable in CI otherwise, and the assembly is the part B11 owns:
// the real tables are covered where they can be, by `config effective` and
// `make workflow-check`, which reach the real kinds' real `Structural`.
type builder struct {
	tracker func(name string) (core.TrackerKind, bool)
	runner  func(name string) (core.RunnerKind, bool)
	// source resolves a `credential_sources` kind. Injected for the same reason
	// the other two are, and with a second one of its own: an `octo_sts`
	// instance reaches an issuer over the network, so a test that could not
	// substitute the kind could not build an assembly at all.
	source func(name string) (core.SourceKind, bool)
	log    *slog.Logger
	// transcriptDir is where the runner retains raw harness streams
	// (SPEC §7.2, §10.3). Empty disables retention.
	transcriptDir string
	// workspaceScratchRoot is the daemon-owned state tree used for ephemeral
	// remote Git repositories. It is never placed in a RunSpec, unlike every
	// workspace path and the agent's TMPDIR (#231).
	workspaceScratchRoot string
	// agentTempRoot is the actual default TMPDIR inherited through the core
	// allowlist. The provider proves its scratch root does not overlap it.
	agentTempRoot string
	// agentHomeRoot is the exact HOME inherited through the same allowlist.
	// Runner kinds receive it as an explicit input when a runtime adds implicit
	// home-relative write grants of its own.
	agentHomeRoot string
	// workspaceReady substitutes only the local provider's network readiness
	// probe. Production always installs Provider.Ready; tests inject the same
	// ordered boundary because their fake tracker deliberately names no network
	// endpoint that exists.
	workspaceReady func(context.Context, *workspace.Provider) error
	// substrateDir is where a v2 backend keeps the durable addresses a restart
	// needs (#194, state.Dir.Substrate). Unused under the local substrate, as are
	// the three beside it: the run journals and their event inbox, the
	// workspace-cycle records, and the daemon-side evidence stores (#205).
	substrateDir string
	journalDir   string
	cycleDir     string
	mirrorDir    string
	// reviewDir is where the #204 review controller keeps its durable execution
	// records. A sibling of the substrate tree rather than a subtree, because a
	// review runs on either substrate (state.Dir.Reviews).
	reviewDir string
	// reconciled records that startup reconciliation has run for this process, so
	// a reload does not repeat a survey of every retained claim. It is a
	// *startup* pass by definition (docs/AIRLOCK.md): what it establishes — what
	// this daemon holds on the backend before ordinary dispatch resumes — cannot
	// change by editing a file that is forbidden to move the substrate.
	reconciled bool
	// review is the #204 review controller, built once for the same reason
	// reconciled exists: its declaration is process-lifetime, so a reload cannot
	// have moved it and rebuilding would mint a second durable session over one
	// state directory.
	review      *reviewLeg
	reviewBuilt bool
	// substrateTransport substitutes the backend's HTTP round tripper, for the
	// reason the three kind lookups above are injected: the real one reaches a
	// cluster-internal endpoint over TLS, and nothing about this leg of the
	// assembly is provable in CI otherwise. Nil in every production path.
	substrateTransport http.RoundTripper

	// mu guards the retained candidates. config.Watcher serializes reloads under
	// one mutex, but the watch goroutine and a Revalidate caller both arrive
	// through it.
	mu sync.Mutex
	// candidates is the last tracker constructed for each request-control domain
	// (core.RequestControlDomain), kept whether or not it was published, and
	// offered to later candidates as a predecessor
	// (core.RequestControlSuccessor). A reload that names a new API endpoint has
	// no published predecessor there, and revalidation retries a failing config
	// every tick — so without these, each retry would probe that endpoint with a
	// fresh budget and no memory of its last refusal.
	//
	// Per domain rather than one slot for the daemon, because a config can move
	// between endpoints: a single slot would let each failure evict the endpoint
	// the one before it had just spent requests at, which is the same defect
	// again for a daemon flapping between two.
	candidates map[string]core.TrackerAdapter
	// domains orders those keys by construction, oldest first — the eviction
	// order once retainCandidateLimit is reached.
	domains []string
}

// retainCandidateLimit bounds what the map above may hold. Each entry is a
// constructed adapter with its own HTTP client, so the set cannot be allowed to
// grow with every endpoint a long-lived daemon's config has ever named; the
// endpoints one config actually moves between are a handful.
const retainCandidateLimit = 8

func newBuilder(log *slog.Logger, dir state.Dir) *builder {
	workspaceScratchRoot := dir.Root()
	if absolute, err := filepath.Abs(workspaceScratchRoot); err == nil {
		workspaceScratchRoot = absolute
	}
	return &builder{
		tracker:              registry.Tracker,
		runner:               registry.Runner,
		source:               registry.Source,
		log:                  log,
		transcriptDir:        dir.Transcripts(),
		workspaceScratchRoot: workspaceScratchRoot,
		agentTempRoot:        filepath.Clean(os.TempDir()),
		agentHomeRoot:        os.Getenv("HOME"),
		workspaceReady: func(ctx context.Context, provider *workspace.Provider) error {
			return provider.Ready(ctx)
		},
		substrateDir: dir.Substrate(),
		journalDir:   dir.SubstrateJournals(),
		cycleDir:     dir.SubstrateCycles(),
		mirrorDir:    dir.SubstrateMirror(),
		reviewDir:    dir.Reviews(),
	}
}

// build is config.WatchOptions.BuildRuntime: construct what `changed` names,
// carry the rest forward from prev, and return the whole set or nothing.
//
// The cascade is owned here rather than by AdapterChange, because it follows
// from what each adapter is constructed *from* rather than from what the file
// says: the workspace strategy is built from the repository the tracker names
// (§6.2), and the checker from the provider and the tracker together (§9.7). So
// a rebuilt tracker obliges a rebuilt provider and checker whatever the flags
// say — carrying either forward would leave it bound to the previous
// repository, or to a credential that has just been rotated away from.
func (b *builder) build(ctx context.Context, def *config.WorkflowDefinition, prev *orchestrator.Bundle, changed config.AdapterChange) (*orchestrator.Bundle, error) {
	// Both families, every time, even when only one moved. Structural is pure
	// and costs nothing, and running both keeps `ben run`'s first refusal
	// identical to the one `config effective` reports for the same file — an
	// operator who validated a config and then started the daemon should not
	// meet a second, different opinion of it.
	trackerKind, runnerKind, err := structuralKinds(def, b.tracker, b.runner)
	if err != nil {
		return nil, err
	}

	// One instance per named credential source (SPEC §11). Built here, before
	// anything is constructed from them, and narrowed at each consumer: the
	// tracker and the workspace share the tracker instance — the workspace
	// reaches it through core.Repository.AuthSource, which the tracker itself
	// supplies — and the publisher gets its own, narrowed to the fresh-only
	// surface so "serve the publisher from the shared cache" is not expressible
	// downstream.
	//
	// An invalid split never reaches here: two credentials of equal authority
	// are a load refusal, so the two instances are always distinct.
	sources, err := def.Config.NewSources(b.source)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConstruct, err)
	}

	// Before any local adapter is built. A workflow that declares a remote
	// substrate is not one this daemon may serve from a local worktree, and
	// constructing the local set first would spend a base fetch and a tracker
	// probe on a configuration that will not use either.
	substrate, err := b.readySubstrate(ctx, def, sources.Substrate)
	if err != nil {
		return nil, err
	}

	// Tracker ⇒ workspace ⇒ verifier. A local agent edit also rebuilds the
	// workspace so its daemon scratch placement is revalidated against the new
	// provider's complete writable-root declaration. Remote runs cannot address
	// this host's scratch tree, so their workspace cycle is unchanged by an
	// agent-only reload.
	rebuildTracker := changed.Tracker || prev == nil
	rebuildWorkspace := rebuildTracker || changed.Workspace || (changed.Agent && substrate == nil)
	// Workspace ⇒ runner, as well as agent ⇒ runner. The runner is constructed with
	// a §9.10 evidence sink closed over *this* workspace provider (buildRunner), so
	// a runner carried forward across a workspace rebuild would keep recording every
	// launch against the previous provider. After a `workspace.root` edit that means
	// RecordRun is handed a path outside the new provider's tree, which it rightly
	// refuses (safety invariant 2, §6.3) — and a refused sink fails the attempt, so
	// every launch under the new configuration dies at the marker upgrade.
	//
	// Stated as one flag rather than three conditions, like the tracker cascade
	// above, so it cannot be half-applied.
	rebuildRunner := changed.Agent || rebuildWorkspace || prev == nil

	// Resolved from the definition on every build, rebuild or not: it describes
	// the configuration rather than an adapter instance, so an edit that moves
	// the model has to reach the next attempt's outcome record (#60) whether or
	// not it also obliged a new runner.
	bundle := &orchestrator.Bundle{Definition: def, Agent: config.AgentDescriptor(def)}
	if !rebuildTracker {
		bundle.Tracker, bundle.ClaimPrincipal, bundle.Repository = prev.Tracker, prev.ClaimPrincipal, prev.Repository
	}
	if !rebuildWorkspace {
		bundle.Workspaces, bundle.Verifier = prev.Workspaces, prev.Verifier
	}
	if !rebuildRunner {
		bundle.Runner, bundle.ResolveRun = prev.Runner, prev.ResolveRun
	}

	if rebuildTracker {
		if err := b.buildTracker(ctx, def, trackerKind, sources.Tracker, prev, bundle); err != nil {
			return nil, err
		}
	}
	// The one fork in this function, and it is total: a workflow runs entirely on
	// one substrate or entirely on the other. There is no fallback edge between
	// them — a remote configuration that cannot be assembled refuses rather than
	// preparing a local worktree, which would be two trees, one claim, and a §9.7
	// verdict read from the wrong one.
	if rebuildWorkspace {
		if substrate != nil {
			if err := b.buildRemoteWorkspace(ctx, def, substrate, bundle); err != nil {
				return nil, err
			}
		} else {
			writeScope, err := runnerKind.LocalWrites(def.Config.AgentBinding(), core.LocalRuntimePaths{
				DaemonHomeDir: b.agentHomeRoot,
			})
			if err != nil {
				// Structural just accepted the same binding through the same parse
				// path. Keep a context-dependent scope refusal or a disagreement
				// fail-closed rather than silently dropping write roots.
				return nil, fmt.Errorf("%w: agent.kind %q local write scope: %w",
					ErrConstruct, def.Config.Agent.Kind, err)
			}
			if err := b.buildWorkspace(ctx, def, prev, bundle, writeScope); err != nil {
				return nil, err
			}
		}
	}
	if rebuildRunner {
		if substrate != nil {
			if err := b.buildRemoteRunner(ctx, def, runnerKind, substrate, bundle); err != nil {
				return nil, err
			}
		} else if err := b.buildRunner(ctx, def, runnerKind, sources.Publish, bundle); err != nil {
			return nil, err
		}
	}
	if substrate != nil {
		// After the strategy exists and before this bundle can be published, which
		// is what "before ordinary dispatch" means here: the config watcher stores
		// revision 1 before Watch returns, and the loop cannot tick until it has.
		if err := b.reconcile(ctx, substrate); err != nil {
			return nil, err
		}
	}

	// The review controller last, and once per process (#204). Last because it
	// is built over the workspace strategy this function has just selected, and
	// once because — like the substrate — its declaration is process-lifetime:
	// outstanding review runs address the reviewer they were dispatched to, and
	// a reload that moved the controller's identities under an in-flight round
	// could route on artifacts a different login wrote. config refuses that
	// reload (ErrReviewChanged), so this is the enforcement's other half rather
	// than a second opinion of it.
	if err := b.buildReviewOnce(ctx, def, sources.Review, substrate, bundle); err != nil {
		return nil, err
	}
	return bundle, nil
}

// buildReviewOnce constructs the review leg for the first published revision and
// retains it. Later revisions cannot have moved it (see build).
func (b *builder) buildReviewOnce(
	ctx context.Context, def *config.WorkflowDefinition, cred core.CredentialSource,
	substrate *airlock.Substrate, bundle *orchestrator.Bundle,
) error {
	b.mu.Lock()
	built := b.reviewBuilt
	b.reviewBuilt = true
	b.mu.Unlock()
	if built {
		return nil
	}
	leg, err := b.buildReview(ctx, def, cred, substrate, bundle)
	if err != nil {
		b.mu.Lock()
		b.reviewBuilt = false
		b.mu.Unlock()
		return err
	}
	b.mu.Lock()
	b.review = leg
	b.mu.Unlock()
	return nil
}

// Review returns the constructed controller, or nil when the workflow declares
// none. Read by `ben run` after the first revision is published.
func (b *builder) Review() *reviewLeg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.review
}

// structuralKinds resolves both adapter families and runs every applicable pure
// structural check — every family, not just the tracker, plus the runner's
// remote-only check when the workflow selects that substrate. Both provider
// blocks are opaque to the loader (SPEC §5.2.2, §5.2.5), so a check that asked
// only the tracker would green-light a typo'd `agent.provider` (#55).
//
// The one implementation, called from two places on purpose. `ben run` needs it
// as the first stage of the build; `ben config effective` needs it and nothing
// after it, which is what lets inspecting a config work with no credentials, no
// network, and no installed harness (SPEC §5.8). Two spellings of "resolve the
// kinds and validate both blocks" is the drift the registry was introduced to
// remove.
//
// Errors already carry their stage. `config effective` renders them through
// config.RenderRefusal, which reads the offending value's provenance and decides
// whether showing it would leak a secret; that path finds its *core.ConfigValueError
// through the wrap, so the redaction is unaffected by it.
func structuralKinds(
	def *config.WorkflowDefinition,
	trackerFor func(string) (core.TrackerKind, bool),
	runnerFor func(string) (core.RunnerKind, bool),
) (core.TrackerKind, core.RunnerKind, error) {
	trackerKind, ok := trackerFor(def.Config.Tracker.Kind)
	if !ok {
		// Unreachable through Load, which validated both names against this same
		// registry (SPEC §5.7). Refusing beats silently skipping validation
		// should that ever stop being true.
		return nil, nil, fmt.Errorf("%w: tracker.kind %q", ErrUnknownKind, def.Config.Tracker.Kind)
	}
	runnerKind, ok := runnerFor(def.Config.Agent.Kind)
	if !ok {
		return nil, nil, fmt.Errorf("%w: agent.kind %q", ErrUnknownKind, def.Config.Agent.Kind)
	}
	if err := trackerKind.Structural(trackerConfig(def)); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrStructural, err)
	}
	binding := def.Config.AgentBinding()
	if err := runnerKind.Structural(binding); err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrStructural, err)
	}
	if def.Config.Substrate.Remote() {
		remoteKind, ok := runnerKind.(core.RemoteRunnerKind)
		if !ok {
			return nil, nil, fmt.Errorf("%w: %w: agent.kind %q",
				ErrStructural, ErrNoRemoteRunner, def.Config.Agent.Kind)
		}
		if err := remoteKind.RemoteStructural(binding); err != nil {
			return nil, nil, fmt.Errorf("%w: %w", ErrStructural, err)
		}
	}
	return trackerKind, runnerKind, nil
}

// buildTracker constructs the tracker and resolves the two things only a ready
// one can answer: the account its claims are assigned to, and the repository
// its issues live in (SPEC §6.2, §8.4, §10.2).
//
// Both are asked strictly after Ready, and both refuse if asked earlier — which
// is why the order here is a property to test rather than a convention to
// follow. Request-control continuity is offered before Ready because readiness
// itself makes network requests, including on candidates a failed reload will
// discard. A kind that answers neither fact is refused rather than defaulted:
// the v1 workspace strategy exists only as a clone of some repository, and
// §9.8 cannot tell our claim from a human's without a principal, so guessing
// either would be inventing the fact both rest on.
func (b *builder) buildTracker(ctx context.Context, def *config.WorkflowDefinition, kind core.TrackerKind, cred core.CredentialSource, prev *orchestrator.Bundle, bundle *orchestrator.Bundle) error {
	tracker, err := kind.New(trackerOptions(def, cred))
	if err != nil {
		return fmt.Errorf("%w: tracker.kind %q: %w", ErrConstruct, def.Config.Tracker.Kind, err)
	}
	predecessors := b.adopt(prev, tracker)
	if successor, ok := tracker.(core.RequestControlSuccessor); ok && len(predecessors) > 0 {
		successor.ContinueRequestControl(predecessors...)
	}
	if err := tracker.Ready(ctx); err != nil {
		return fmt.Errorf("%w: tracker.kind %q: %w", ErrNotReady, def.Config.Tracker.Kind, err)
	}

	principal, err := claimPrincipalFrom(ctx, tracker)
	if err != nil {
		return fmt.Errorf("%w: tracker.kind %q: %w", ErrNotReady, def.Config.Tracker.Kind, err)
	}
	repo, err := workspace.RepositoryFrom(ctx, tracker)
	if err != nil {
		return fmt.Errorf("%w: tracker.kind %q: %w", ErrNotReady, def.Config.Tracker.Kind, err)
	}

	bundle.Tracker, bundle.ClaimPrincipal, bundle.Repository = tracker, principal, repo
	return nil
}

// adopt retains tracker for its request-control domain and returns the
// predecessors it may continue from, most authoritative first: the published
// generation, then the candidates earlier reloads constructed and discarded,
// newest first.
//
// Order is the whole contract. The published tracker is the generation actually
// metering traffic, so it wins wherever it is compatible; a discarded candidate
// answers only for an endpoint the published one does not serve. The list is
// offered whole rather than filtered to the matching key, because compatibility
// is the successor's own question (core.RequestControlSuccessor) — the key here
// decides only what is *kept*, and a builder that pre-selected on its own reading
// of it could hand back nothing where the adapter would have found a match.
//
// Bundle deliberately exposes only the loop's narrow Tracker surface; builders
// always put a core adapter into it, so the assertion recovers that construction
// boundary without widening the orchestrator's dependency.
func (b *builder) adopt(prev *orchestrator.Bundle, tracker core.TrackerAdapter) []core.TrackerAdapter {
	b.mu.Lock()
	defer b.mu.Unlock()
	var predecessors []core.TrackerAdapter
	if prev != nil && prev.Tracker != nil {
		if published, ok := prev.Tracker.(core.TrackerAdapter); ok {
			predecessors = append(predecessors, published)
		}
	}
	// Newest first: where two retained candidates could serve, the one that spent
	// requests most recently holds the newest backoff.
	for i := len(b.domains) - 1; i >= 0; i-- {
		candidate := b.candidates[b.domains[i]]
		if candidate == nil || candidate == tracker || slices.Contains(predecessors, candidate) {
			continue
		}
		predecessors = append(predecessors, candidate)
	}
	b.retainLocked(tracker)
	return predecessors
}

// retainLocked makes tracker the candidate its domain answers for, and drops the
// least recently constructed domain once the map is full.
func (b *builder) retainLocked(tracker core.TrackerAdapter) {
	key := ""
	if domain, ok := tracker.(core.RequestControlDomain); ok {
		key = domain.RequestControlKey()
	}
	if b.candidates == nil {
		b.candidates = make(map[string]core.TrackerAdapter)
	}
	b.candidates[key] = tracker
	b.domains = append(slices.DeleteFunc(b.domains, func(k string) bool { return k == key }), key)
	for len(b.domains) > retainCandidateLimit {
		delete(b.candidates, b.domains[0])
		b.domains = b.domains[1:]
	}
}

// buildWorkspace constructs the provider and the checker derived from it.
//
// The lock domain is carried from the previous generation whenever this one
// addresses the same tree — same `workspace.root`, same workflow key — because
// the tree outlives any single provider (workspace.LockDomain). It is scoped to
// the tree and not to the identity deliberately: a repository or principal change
// still addresses the same base.git, and CheckBaseCache below takes that very
// base mutex before any refusal about the change can happen.
func (b *builder) buildWorkspace(
	ctx context.Context,
	def *config.WorkflowDefinition,
	prev *orchestrator.Bundle,
	bundle *orchestrator.Bundle,
	agentWrites core.LocalWriteScope,
) error {
	provider, err := workspace.New(workspace.Options{
		Root:          def.Config.Workspace.Root,
		WorkflowKey:   def.Key,
		ScratchRoot:   b.workspaceScratchRoot,
		AgentTempRoot: b.agentTempRoot,
		AgentWrites:   agentWrites,
		Repository:    bundle.Repository,
		BaseBranch:    def.Config.Workspace.BaseBranch,
		Hooks:         hooksFrom(def.Config.Hooks),
		Locks:         carriedLocks(def, prev),
		Logger:        b.log,
	})
	if err != nil {
		return fmt.Errorf("%w: workspace provider: %w", ErrConstruct, err)
	}
	// Checked at build rather than left to the first Prepare. validateBase
	// refuses an existing base.git whose origin no longer matches, fail-closed
	// and with no auto-repair — but it runs inside Prepare, so a repository
	// change adopted against a populated cache does not fail the reload: it
	// fails every subsequent attempt as a launch error, one dispatched issue at
	// a time. One local git read here turns that into one loud refusal.
	if err := provider.CheckBaseCache(ctx); err != nil {
		return fmt.Errorf("%w: workspace provider: %w", ErrNotReady, err)
	}
	ready := b.workspaceReady
	if ready == nil {
		ready = func(ctx context.Context, provider *workspace.Provider) error { return provider.Ready(ctx) }
	}
	if err := ready(ctx, provider); err != nil {
		return fmt.Errorf("%w: workspace provider: %w", ErrNotReady, err)
	}

	// The checker reads one tracker fact — the §9.7 open PR — and the bundle
	// holds the tracker through the *loop's* narrow seam, which deliberately
	// does not include it: §8.2's read kernel is wider than what the loop
	// programs against, and both are narrower than the adapter. So the seam the
	// checker needs is asked for by assertion, as RepositorySource and
	// ClaimPrincipalSource are, and its absence is a refusal rather than a
	// verifier quietly missing a leg.
	evidence, ok := bundle.Tracker.(verifyTracker)
	if !ok {
		return fmt.Errorf("%w: publish-evidence checker: %T cannot answer for an open pull request (SPEC §9.7 leg 3)", ErrConstruct, bundle.Tracker)
	}
	verifier, err := newVerifier(provider, evidence)
	if err != nil {
		return fmt.Errorf("%w: publish-evidence checker: %w", ErrConstruct, err)
	}
	bundle.Workspaces, bundle.Verifier = provider, verifier
	return nil
}

// verifyTracker is verify.Tracker, restated here only so the assertion above
// reads as one type rather than an inline interface literal. It is the same
// contract; the compile-time proof is in the call to newVerifier.
type verifyTracker interface {
	FindPR(ctx context.Context, issue core.Issue, branch string) (*core.PR, error)
}

// runEvidenceRecorder is the workspace provider's half of SPEC §9.10's run
// marker: the upgrade that turns "something may be live in this workspace" into a
// question a later daemon can actually ask.
//
// Asked for by assertion, as verifyTracker and core.ClaimPrincipalSource are, and
// for the same reason: the loop's Workspaces seam is narrower than the provider,
// and this is a consumer-specific need of the *assembly* rather than of the loop.
// Keyed by workspace path because that is what a runner has — the sink is handed
// a core.RunSpec and the adapter never learns the workspace key.
type runEvidenceRecorder interface {
	RecordRun(workspacePath string, evidence core.RunEvidence) error
}

func (b *builder) buildRunner(ctx context.Context, def *config.WorkflowDefinition, kind core.RunnerKind, cred core.CredentialSource, bundle *orchestrator.Bundle) error {
	// The same projection adapterChange compares, so a reload that changes it
	// rebuilds this runner and re-checks Ready (SPEC §5.4, §7.1,
	// config.AgentBinding).
	binding := def.Config.AgentBinding()

	// The binding that makes crash recovery converge instead of parking. The
	// orchestrator writes a marker before each launch; this is what upgrades it
	// with evidence identifying the run, and without it every marker a crash leaves
	// behind reads as unknown_launch — fail-closed, but it parks for a human every
	// issue §9.10 was meant to resume by itself.
	//
	// buildWorkspace runs before this, and the not-rebuilt path carries the
	// provider forward from prev, so bundle.Workspaces is populated either way.
	recorder, ok := bundle.Workspaces.(runEvidenceRecorder)
	if !ok {
		return fmt.Errorf("%w: run evidence sink: %T cannot record a launched run against its workspace (SPEC §9.10)",
			ErrConstruct, bundle.Workspaces)
	}
	runner, err := kind.New(core.RunnerOptions{
		Provider: binding.Provider,
		// Narrowed to the fresh-only surface here, at the one place entitled to
		// decide it (SPEC §11): a token handed to an agent must cover the whole
		// attempt, and the type is what makes serving it from a cache
		// unspeakable rather than merely discouraged.
		Publish:        core.PublishBinding{Env: binding.Publish.Env, Source: publishSource(cred)},
		AttemptTimeout: binding.AttemptTimeout,
		TranscriptDir:  b.transcriptDir,
		// Called once per attempt after the trusted domain exists and before
		// untrusted provider execution is released (core.RunEvidenceSink). On an
		// error the adapter tears down that unreleased domain before returning a
		// launch refusal, leaving no unowned process behind.
		OnRun: func(spec core.RunSpec, evidence core.RunEvidence) error {
			return recorder.RecordRun(spec.Workspace.Path, evidence)
		},
	})
	if err != nil {
		return fmt.Errorf("%w: agent.kind %q: %w", ErrConstruct, def.Config.Agent.Kind, err)
	}
	if err := runner.Ready(ctx); err != nil {
		return fmt.Errorf("%w: agent.kind %q: %w", ErrNotReady, def.Config.Agent.Kind, err)
	}
	bundle.Runner = runner
	return nil
}

// claimPrincipalFrom asks a tracker which account its claims are assigned to,
// and refuses a kind that cannot say (SPEC §8.4).
//
// The mirror of workspace.RepositoryFrom, and here rather than beside it
// because it is not the workspace's question. Assembly asks for the contract by
// type assertion so §8.2's read-kernel-plus-closed-write-set stays as
// specified — a tracker for which "claims" mean something else simply does not
// implement it, and is refused at startup instead of failing at the first claim.
func claimPrincipalFrom(ctx context.Context, tracker core.TrackerAdapter) (string, error) {
	src, ok := tracker.(core.ClaimPrincipalSource)
	if !ok {
		return "", fmt.Errorf("%w: %T does not implement core.ClaimPrincipalSource", ErrNoClaimPrincipalSource, tracker)
	}
	principal, err := src.ClaimPrincipal(ctx)
	if err != nil {
		return "", fmt.Errorf("resolving the tracker's claim principal: %w", err)
	}
	if principal == "" {
		// A source that reports success and no principal has broken its own
		// contract. The same refusal as one that does not implement it at all:
		// both leave assembly with no account to claim as, and the orchestrator
		// would refuse this bundle a moment later (checkBundle) with less to say
		// about which component is responsible.
		return "", fmt.Errorf("%w: %T reported an empty claim principal", ErrNoClaimPrincipalSource, tracker)
	}
	return principal, nil
}

// carriedLocks is the previous generation's lock domain when this one addresses
// the same tree, and nil — a fresh domain — when it does not.
//
// Root and the workflow key are exactly what `<root>/<workflow_key>` is built
// from, which is what the domain serializes. The key cannot drift under a
// running daemon (it derives from the WORKFLOW.md path and the watcher watches
// one path), so in practice this turns on the root; it is compared anyway,
// because "in practice" is not what a lock is for.
func carriedLocks(def *config.WorkflowDefinition, prev *orchestrator.Bundle) *workspace.LockDomain {
	if prev == nil || prev.Definition == nil {
		return nil
	}
	if prev.Definition.Config.Workspace.Root != def.Config.Workspace.Root || prev.Definition.Key != def.Key {
		return nil
	}
	provider, ok := prev.Workspaces.(*workspace.Provider)
	if !ok {
		return nil
	}
	return provider.LockDomain()
}

// trackerConfig projects the definition onto the adapter's whole slice of it —
// the opaque block *as written*, and the core-owned fields together, because
// rules like non-empty required_labels span both (SPEC §5.7).
//
// This is the **validation** projection, and it deliberately carries the legacy
// credential spellings: Structural is asked about the file an operator wrote,
// including `token` and `credential_source`, and a reduced block would leave
// exactly those unvalidated. What assembly *constructs* from is the projection
// below.
func trackerConfig(def *config.WorkflowDefinition) core.TrackerConfig {
	return core.TrackerConfig{
		Provider:       def.Config.Tracker.Provider,
		RequiredLabels: def.Config.Tracker.RequiredLabels,
		ActiveStates:   def.Config.Tracker.ActiveStates,
		TerminalStates: def.Config.Tracker.TerminalStates,
		WorkflowKey:    def.Key,
	}
}

// trackerOptions projects the definition onto what the adapter is *built* from
// (SPEC §8, amendment 9; §11).
//
// It is `Config.TrackerBinding()` plus the two things a binding deliberately
// omits: the workflow key, which derives from the watched path and cannot move
// under a running daemon, and the constructed credential instance, which is not
// a comparable value. Anchoring it on the binding is what keeps "every field the
// adapter is built from is a reason to rebuild it" true by construction rather
// than by a reviewer noticing — the same rule AgentBinding has enforced on the
// agent leg since #117.
func trackerOptions(def *config.WorkflowDefinition, cred core.CredentialSource) core.TrackerOptions {
	binding := def.Config.TrackerBinding()
	return core.TrackerOptions{
		Provider:       binding.Provider,
		RequiredLabels: binding.RequiredLabels,
		ActiveStates:   binding.ActiveStates,
		TerminalStates: binding.TerminalStates,
		WorkflowKey:    def.Key,
		ClaimAssignee:  binding.ClaimAssignee,
		// Narrowed to the cached surface: the tracker polls on every tick, and
		// an exchange per request would multiply the issuer's traffic by the
		// daemon's.
		Credential: cred,
	}
}

// publishSource narrows a constructed instance to the publisher's view, and
// keeps a nil nil.
//
// The explicit nil matters: a typed nil inside a non-nil interface would make
// core.PublishBinding.Configured report a configured credential for a workflow
// that declares none, and the adapter would then mint from it.
func publishSource(cred core.CredentialSource) core.FreshSource {
	if cred == nil {
		return nil
	}
	return cred
}

func hooksFrom(h config.HooksConfig) workspace.Hooks {
	return workspace.Hooks{
		AfterCreate:  h.AfterCreate,
		BeforeRun:    h.BeforeRun,
		AfterRun:     h.AfterRun,
		BeforeRemove: h.BeforeRemove,
		Timeout:      time.Duration(h.TimeoutMS) * time.Millisecond,
	}
}
