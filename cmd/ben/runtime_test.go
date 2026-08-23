package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/config"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/workspace"
)

// The builder is the assembly B11 owns, and these tests are about *order* and
// *identity* rather than about what any adapter does: which stage refused, what
// was asked before what, what a rebuild constructs again and what it carries
// forward. The adapters' own behavior is covered where it lives, and the real
// kind table is covered by `config effective` and `make workflow-check`, which
// reach the real kinds' real Structural.
//
// The kinds here are fakes because the real ones cannot be driven in CI: New
// binds a credential and Ready makes a network call and, for the runners, runs a
// subprocess. That is precisely why the lookups are injectable — the assembly
// would otherwise be the one part of B11 with no test at all.

// step names one thing the builder did, recorded in the order it did it.
type step string

const (
	stepTrackerStructural step = "tracker.Structural"
	stepRunnerStructural  step = "runner.Structural"
	stepTrackerNew        step = "tracker.New"
	stepRequestContinuity step = "tracker.ContinueRequestControl"
	stepTrackerReady      step = "tracker.Ready"
	stepClaimPrincipal    step = "tracker.ClaimPrincipal"
	stepRepository        step = "tracker.Repository"
	stepRunnerNew         step = "runner.New"
	stepRunnerReady       step = "runner.Ready"
)

// journal records the assembly sequence across both kinds.
type journal struct {
	mu    sync.Mutex
	steps []step
}

func (j *journal) note(s step) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.steps = append(j.steps, s)
}

func (j *journal) seen() []step {
	j.mu.Lock()
	defer j.mu.Unlock()
	return slices.Clone(j.steps)
}

func (j *journal) has(s step) bool { return slices.Contains(j.seen(), s) }

// fakeTrackerKind is a core.TrackerKind whose every stage can be made to fail,
// so a test can name the stage it means to exercise.
type fakeTrackerKind struct {
	j *journal
	// failures, by stage. Each is checked at the point the real kind would
	// reach it.
	structural, construct, ready, principal, repository error
	// noPrincipalSource and noRepositorySource drop the optional contract
	// entirely, which is a different refusal from one that fails.
	noPrincipalSource, noRepositorySource bool
	remoteURL                             string

	// gotStructural and gotNew are what the builder passed. Captured rather
	// than ignored: a constructor that discards its argument cannot fail when
	// the caller passes the wrong one, so a fake that ignores it makes every
	// wiring assertion vacuous.
	gotStructural core.TrackerConfig
	gotNew        core.TrackerOptions

	// built is every tracker this kind has constructed, in order — including
	// the candidates a failed Ready discarded, which the bundle cannot name.
	built []*fakeTracker

	// domain is the request-control domain the next construction reports, which
	// is how a test moves a config between endpoints. Captured per tracker at
	// New, because the real key is fixed when the client is built.
	domain string
}

func (k *fakeTrackerKind) Structural(cfg core.TrackerConfig) error {
	k.j.note(stepTrackerStructural)
	k.gotStructural = cfg
	return k.structural
}

func (k *fakeTrackerKind) CredentialRefs(map[string]any) core.CredentialRefs {
	return core.CredentialRefs{}
}

// The builder never asks for these — they are the loader's and the effective
// renderer's (SPEC §5.8, §10.2) — so nothing here should ever be called.
func (k *fakeTrackerKind) SensitiveFields(map[string]any) [][]string {
	panic("the builder asked a kind for its display-redaction surface")
}

func (k *fakeTrackerKind) New(opts core.TrackerOptions) (core.TrackerAdapter, error) {
	k.j.note(stepTrackerNew)
	k.gotNew = opts
	if k.construct != nil {
		return nil, k.construct
	}
	a := &fakeTracker{kind: k, domain: k.domain}
	k.built = append(k.built, a)
	switch {
	case k.noPrincipalSource && k.noRepositorySource:
		return struct{ core.TrackerAdapter }{a}, nil
	case k.noPrincipalSource:
		return struct {
			core.TrackerAdapter
			core.RepositorySource
		}{a, a}, nil
	case k.noRepositorySource:
		return struct {
			core.TrackerAdapter
			core.ClaimPrincipalSource
		}{a, a}, nil
	}
	return a, nil
}

// fakeTracker answers the three questions the builder asks and refuses the rest:
// nothing here is dispatched against, and a method the builder never calls
// returning a plausible zero value would only make a miswiring look like it
// worked.
type fakeTracker struct {
	kind   *fakeTrackerKind
	domain string
	// predecessors is the whole offered list, in order. A fake that kept only
	// the first could not see the builder stop offering the rest, which is the
	// half of the contract that carries a discarded candidate's request control
	// forward (core.RequestControlSuccessor).
	predecessors []core.TrackerAdapter
}

var (
	_ core.TrackerAdapter          = (*fakeTracker)(nil)
	_ core.RepositorySource        = (*fakeTracker)(nil)
	_ core.ClaimPrincipalSource    = (*fakeTracker)(nil)
	_ core.RequestControlSuccessor = (*fakeTracker)(nil)
	_ core.RequestControlDomain    = (*fakeTracker)(nil)
)

func (t *fakeTracker) ContinueRequestControl(previous ...core.TrackerAdapter) {
	t.kind.j.note(stepRequestContinuity)
	t.predecessors = slices.Clone(previous)
}

func (t *fakeTracker) RequestControlKey() string { return t.domain }

func (t *fakeTracker) Ready(context.Context) error {
	t.kind.j.note(stepTrackerReady)
	return t.kind.ready
}

func (t *fakeTracker) ClaimPrincipal(context.Context) (string, error) {
	t.kind.j.note(stepClaimPrincipal)
	if t.kind.principal != nil {
		return "", t.kind.principal
	}
	return "ben-bot", nil
}

func (t *fakeTracker) Repository(context.Context) (core.Repository, error) {
	t.kind.j.note(stepRepository)
	if t.kind.repository != nil {
		return core.Repository{}, t.kind.repository
	}
	url := t.kind.remoteURL
	if url == "" {
		url = "https://example.test/acme/widgets.git"
	}
	return core.Repository{
		RemoteURL:  url,
		AuthSource: fake.NewRemoteAuth("x-access-token", "s3cret"),
	}, nil
}

const unreached = "the builder called a tracker method it has no business calling"

func (t *fakeTracker) Fetch(context.Context) ([]core.Issue, error) { panic(unreached) }
func (t *fakeTracker) Get(context.Context, string) (*core.Issue, error) {
	panic(unreached)
}
func (t *fakeTracker) ClaimedByPrincipal(context.Context) ([]core.Issue, error) { panic(unreached) }
func (t *fakeTracker) HeldClaims(context.Context) ([]core.Issue, error)         { panic(unreached) }
func (t *fakeTracker) ClaimHistory(context.Context, core.Issue) ([]core.ClaimEvent, error) {
	panic(unreached)
}
func (t *fakeTracker) FindPR(context.Context, core.Issue, string) (*core.PR, error) {
	panic(unreached)
}
func (t *fakeTracker) Claim(context.Context, core.Issue) (bool, error) { panic(unreached) }
func (t *fakeTracker) Release(context.Context, core.Issue) error       { panic(unreached) }
func (t *fakeTracker) SetStateLabels(context.Context, core.Issue, core.StateLabel) error {
	panic(unreached)
}
func (t *fakeTracker) Comment(context.Context, core.Issue, core.MilestoneComment) error {
	panic(unreached)
}

type fakeRunnerKind struct {
	j                            *journal
	structural, construct, ready error

	// gotStructural is the whole agent configuration, gotNew the whole
	// RunnerOptions.
	gotStructural core.AgentConfig
	gotNew        core.RunnerOptions
}

func (k *fakeRunnerKind) Structural(cfg core.AgentConfig) error {
	k.j.note(stepRunnerStructural)
	k.gotStructural = cfg
	return k.structural
}

func (k *fakeRunnerKind) ForwardedEnvVars(map[string]any) []string { return nil }

func (k *fakeRunnerKind) SensitiveFields(map[string]any) [][]string {
	panic("the builder asked a kind for its display-redaction surface")
}

// Model is the attempt record's, and the builder has no business asking for it:
// the descriptor is resolved beside the loader, where provenance can decide
// whether the value may be published (config.AgentDescriptor). Same assertion,
// same reason, as SensitiveFields above.
func (k *fakeRunnerKind) Model(map[string]any) (string, []string) {
	panic("the builder asked a kind which model it would run")
}

func (k *fakeRunnerKind) New(opts core.RunnerOptions) (core.AgentRunner, error) {
	k.j.note(stepRunnerNew)
	k.gotNew = opts
	if k.construct != nil {
		return nil, k.construct
	}
	return &fakeRunner{kind: k}, nil
}

type fakeRunner struct{ kind *fakeRunnerKind }

var _ core.AgentRunner = (*fakeRunner)(nil)

func (r *fakeRunner) Ready(context.Context) error {
	r.kind.j.note(stepRunnerReady)
	return r.kind.ready
}
func (r *fakeRunner) Capabilities() core.Capabilities { return core.Capabilities{} }
func (r *fakeRunner) Start(context.Context, core.RunSpec) (core.RunHandle, error) {
	panic("the builder started a run")
}

// harness is one builder wired to one pair of fake kinds.
type harness struct {
	t       *testing.T
	j       *journal
	tracker *fakeTrackerKind
	runner  *fakeRunnerKind
	b       *builder
	root    string
	// path is one WORKFLOW.md, rewritten in place for each generation. A reload
	// is the same file changing, and the workflow key derives from that file's
	// path (SPEC §5.1) — so a fixture that wrote each generation to a fresh temp
	// dir would give two generations two keys, and every same-tree property
	// would read as a different tree.
	path string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	j := &journal{}
	h := &harness{
		t:       t,
		j:       j,
		tracker: &fakeTrackerKind{j: j},
		runner:  &fakeRunnerKind{j: j},
		root:    t.TempDir(),
		path:    filepath.Join(t.TempDir(), "WORKFLOW.md"),
	}
	h.b = &builder{
		tracker: func(name string) (core.TrackerKind, bool) {
			if name != "github" {
				return nil, false
			}
			return h.tracker, true
		},
		runner: func(name string) (core.RunnerKind, bool) {
			if name != "claude-code" {
				return nil, false
			}
			return h.runner, true
		},
		log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		transcriptDir: filepath.Join(t.TempDir(), "transcripts"),
	}
	return h
}

// def loads a definition whose only variable parts are the ones a test moves.
// Going through config.Load rather than assembling a literal is deliberate: the
// key, the normalized root and the compiled prompt all come from the loader, and
// a hand-built definition would let the builder pass against a shape Load never
// produces.
func (h *harness) def(opts ...func(*workflowSpec)) *config.WorkflowDefinition {
	h.t.Helper()
	spec := workflowSpec{
		root: h.root, permissionMode: "bypassPermissions", model: "opus",
		deployment: "deployment:\n  mode: attended\n",
	}
	for _, fn := range opts {
		fn(&spec)
	}
	content := fmt.Sprintf(`---
tracker:
  kind: github
  provider:
    repo: acme/widgets
%s  required_labels: ["ben-queue"]
workspace:
  root: %s
hooks:
  after_create: %q
agent:
  kind: claude-code
  provider:
    permission_mode: %s
    model: %s
publish:
  kind: token
  env: GH_TOKEN
  value: $BEN_TEST_PUBLISH_TOKEN
%s---
Work issue {{ issue.identifier }}.
`, spec.trackerProvider, spec.root, spec.afterCreate, spec.permissionMode, spec.model, spec.deployment)

	if err := os.WriteFile(h.path, []byte(content), 0o644); err != nil {
		h.t.Fatal(err)
	}
	def, err := config.Load(h.path)
	if err != nil {
		h.t.Fatalf("loading the fixture workflow: %v", err)
	}
	return def
}

type workflowSpec struct {
	root           string
	afterCreate    string
	permissionMode string
	// model is the agent.provider key the attempt-outcome record names (#60).
	// Stated by the fixture rather than omitted, so the descriptor a daemon test
	// exercises is the non-empty one an operator's workflow has.
	model string
	// trackerProvider is spliced into the tracker's provider block, indented to
	// its level. It exists so a test can put a real credential reference in the
	// configuration the daemon runs under.
	trackerProvider string
	// deployment overrides the §5.2.9 block. Empty keeps the fixture's own.
	deployment string
}

func withRoot(root string) func(*workflowSpec) { return func(s *workflowSpec) { s.root = root } }
func withHook(script string) func(*workflowSpec) {
	return func(s *workflowSpec) { s.afterCreate = script }
}

// withModel sets `agent.provider.model`. The empty string is the case worth
// reaching for: a block that names no model is the ordinary configuration, and
// the harness picks its own default (#60).
func withModel(model string) func(*workflowSpec) {
	return func(s *workflowSpec) { s.model = model }
}

// withTrackerProvider adds entries to the tracker's provider block, e.g.
// "    token: $BEN_TEST_TOKEN".
func withTrackerProvider(lines string) func(*workflowSpec) {
	return func(s *workflowSpec) { s.trackerProvider = lines }
}

// withDeployment replaces the §5.2.9 block, e.g. "deployment:\n  mode: protected\n".
func withDeployment(block string) func(*workflowSpec) {
	return func(s *workflowSpec) { s.deployment = block }
}

// everything is the change set a first build sees.
var everything = config.AdapterChange{Tracker: true, Agent: true, Workspace: true}

// SPEC §5.7 and BUILD.md's B11 "Repository seam": Structural before New, New
// before Ready, and the two things only a ready tracker can answer strictly
// after it. The order is the contract — a builder that asked for the repository
// first would be asking an adapter that has resolved no credential — so it is
// asserted as a sequence rather than as a set of "was called" flags.
func TestBuildOrdersTheAssembly(t *testing.T) {
	h := newHarness(t)

	bundle, err := h.b.build(context.Background(), h.def(), nil, everything)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	want := []step{
		stepTrackerStructural, stepRunnerStructural,
		stepTrackerNew, stepTrackerReady, stepClaimPrincipal, stepRepository,
		stepRunnerNew, stepRunnerReady,
	}
	if got := h.j.seen(); !slices.Equal(got, want) {
		t.Errorf("assembly order:\n got %v\nwant %v", got, want)
	}

	// Both structural checks run before either construction: `ben run`'s first
	// refusal for a file is then the same one `config effective` reports for it,
	// rather than depending on which adapter happens to be built first.
	if i, k := slices.Index(want, stepRunnerStructural), slices.Index(want, stepTrackerNew); i > k {
		t.Error("a construction ran before both blocks had been validated")
	}

	if bundle.ClaimPrincipal != "ben-bot" {
		t.Errorf("ClaimPrincipal = %q, want the tracker's answer", bundle.ClaimPrincipal)
	}
	if bundle.Repository.RemoteURL != "https://example.test/acme/widgets.git" {
		t.Errorf("Repository.RemoteURL = %q, want the tracker's answer", bundle.Repository.RemoteURL)
	}

	// The whole point of the bundle: the loop can be constructed from it, and
	// the loop is what refuses a set with a hole in it (orchestrator.checkBundle).
	if _, err := orchestrator.New(orchestrator.Config{
		Runtime: config.NewRuntimeSource(bundle.Definition, bundle),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}); err != nil {
		t.Fatalf("orchestrator.New over the built bundle: %v", err)
	}
}

// A tracker candidate spends requests in Ready before a config reload can
// decide whether to publish it. Request-control state therefore crosses the
// generation boundary before readiness, including for candidates that Ready
// ultimately rejects.
func TestRebuiltTrackerContinuesRequestControlBeforeReady(t *testing.T) {
	h := newHarness(t)
	first, err := h.b.build(context.Background(), h.def(), nil, everything)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	second, err := h.b.build(context.Background(), h.def(), first, config.AdapterChange{Tracker: true})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	successor, ok := second.Tracker.(*fakeTracker)
	if !ok {
		t.Fatalf("rebuilt tracker = %T, want *fakeTracker", second.Tracker)
	}
	if len(successor.predecessors) == 0 || successor.predecessors[0] != first.Tracker {
		t.Errorf("request-control predecessors = %v, want the published tracker %T first", successor.predecessors, first.Tracker)
	}

	steps := h.j.seen()
	lastIndex := func(want step) int {
		for i := len(steps) - 1; i >= 0; i-- {
			if steps[i] == want {
				return i
			}
		}
		return -1
	}
	newAt := lastIndex(stepTrackerNew)
	continuedAt := lastIndex(stepRequestContinuity)
	readyAt := lastIndex(stepTrackerReady)
	if !(newAt < continuedAt && continuedAt < readyAt) {
		t.Errorf("rebuild order = %v; want New < ContinueRequestControl < Ready", steps)
	}
}

// The published generation is not the only predecessor there can be. A reload
// that moves the tracker to a new API endpoint has none there at all — and
// because revalidation retries a failing config every tick, each retry would
// otherwise construct a candidate with a fresh budget and no memory of the
// backoff the endpoint just asked for. So the builder retains the candidate it
// constructed even when Ready rejects it, and offers it behind the published one.
func TestFailedTrackerCandidateIsOfferedToTheNextOne(t *testing.T) {
	h := newHarness(t)
	published, err := h.b.build(context.Background(), h.def(), nil, everything)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	h.tracker.ready = errors.New("the endpoint this config names is not answering")
	if _, err := h.b.build(context.Background(), h.def(), published, config.AdapterChange{Tracker: true}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("failed reload error = %v, want ErrNotReady", err)
	}
	discarded := h.tracker.built[len(h.tracker.built)-1]

	// The same failing config, revalidated: the reload the daemon retries every
	// tick until the file is fixed.
	if _, err := h.b.build(context.Background(), h.def(), published, config.AdapterChange{Tracker: true}); !errors.Is(err, ErrNotReady) {
		t.Fatalf("second failed reload error = %v, want ErrNotReady", err)
	}
	retried := h.tracker.built[len(h.tracker.built)-1]
	if retried == discarded {
		t.Fatal("the retry reused the discarded candidate instead of constructing one")
	}

	want := []core.TrackerAdapter{published.Tracker.(core.TrackerAdapter), discarded}
	if !slices.Equal(retried.predecessors, want) {
		t.Errorf("predecessors = %v, want the published tracker then the discarded candidate %v",
			retried.predecessors, want)
	}
}

// One retained candidate per request-control domain, not one for the daemon. A
// config can move between endpoints, and a single slot would let each failure
// evict the endpoint the failure before it had just spent requests at — leaving a
// daemon flapping between two bad endpoints arriving fresh at both, forever.
func TestRetainedCandidatesAreKeptPerRequestControlDomain(t *testing.T) {
	h := newHarness(t)
	h.tracker.domain = "https://a.example/api/v3/"
	published, err := h.b.build(context.Background(), h.def(), nil, everything)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	h.tracker.ready = errors.New("the endpoint this config names is not answering")
	failAt := func(domain string) *fakeTracker {
		t.Helper()
		h.tracker.domain = domain
		if _, err := h.b.build(context.Background(), h.def(), published, config.AdapterChange{Tracker: true}); !errors.Is(err, ErrNotReady) {
			t.Fatalf("reload against %s: error = %v, want ErrNotReady", domain, err)
		}
		return h.tracker.built[len(h.tracker.built)-1]
	}

	movedToB := failAt("https://b.example/api/v3/")
	movedToC := failAt("https://c.example/api/v3/")
	// Back to the endpoint two reloads ago. Its own discarded candidate is the
	// only generation that has ever reached it.
	backToB := failAt("https://b.example/api/v3/")

	want := []core.TrackerAdapter{published.Tracker.(core.TrackerAdapter), movedToC, movedToB}
	if !slices.Equal(backToB.predecessors, want) {
		t.Errorf("predecessors = %v, want the published tracker then both discarded candidates, newest first %v",
			backToB.predecessors, want)
	}
}

// The map is bounded, so the memory it holds is too — and the bound evicts the
// domain nothing has been built for in longest, never the one being retried.
func TestRetainedCandidatesEvictTheOldestDomain(t *testing.T) {
	h := newHarness(t)
	h.tracker.ready = errors.New("no endpoint in this test answers")

	// Two past the cap: a build is offered the map as its predecessor *left* it,
	// so the domain evicted by build N+1 is first absent from what build N+2 sees.
	built := make([]*fakeTracker, 0, retainCandidateLimit+2)
	for i := range retainCandidateLimit + 2 {
		h.tracker.domain = fmt.Sprintf("https://%d.example/api/v3/", i)
		if _, err := h.b.build(context.Background(), h.def(), nil, everything); !errors.Is(err, ErrNotReady) {
			t.Fatalf("reload %d: error = %v, want ErrNotReady", i, err)
		}
		built = append(built, h.tracker.built[len(h.tracker.built)-1])
	}

	last := built[len(built)-1]
	if got := len(last.predecessors); got != retainCandidateLimit {
		t.Fatalf("offered %d predecessors, want the %d retained domains", got, retainCandidateLimit)
	}
	if slices.Contains(last.predecessors, core.TrackerAdapter(built[0])) {
		t.Error("the oldest domain survived past the cap")
	}
	if !slices.Contains(last.predecessors, core.TrackerAdapter(built[1])) {
		t.Error("the cap evicted more than the oldest domain")
	}
}

// BUILD.md B11 acceptance: the workspace provider is built from the tracker's
// core.RepositorySource answer, and no component outside the adapter reads
// `tracker.provider` (#54).
//
// Asserted by moving the remote to something no rule could derive from
// `tracker.provider.repo`. A builder that parsed the block would produce a
// github.com URL for `acme/widgets`; one that asks the tracker produces this.
func TestTheWorkspaceRemoteComesFromTheTrackerNotTheProviderBlock(t *testing.T) {
	h := newHarness(t)
	h.tracker.remoteURL = "https://ghe.internal.example/other-org/other-name.git"

	bundle, err := h.b.build(context.Background(), h.def(), nil, everything)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	remote := bundle.Repository.RemoteURL
	if remote != h.tracker.remoteURL {
		t.Fatalf("Repository.RemoteURL = %q, want the tracker's %q", remote, h.tracker.remoteURL)
	}
	if strings.Contains(remote, "acme/widgets") {
		t.Errorf("the provider block's repo reached the workspace remote: %q", remote)
	}
	// And it reached workspace.New, not just the bundle — the provider is what
	// clones, so a remote recorded beside it rather than passed to it would fail
	// at the first Prepare instead of here. Proven by handing back a URL only
	// workspace.New objects to: its refusal is evidence that it saw the value.
	h2 := newHarness(t)
	h2.tracker.remoteURL = "https://user:token@example.test/acme/widgets.git"
	if _, err := h2.b.build(context.Background(), h2.def(), nil, everything); !errors.Is(err, ErrConstruct) {
		t.Errorf("error = %v; the tracker's remote never reached workspace.New", err)
	}
}

// Each stage refuses with its own sentinel, and refuses *before* the next stage
// runs. Collapsing these into one error would send a missing credential, a
// typo'd block and an uninstalled harness to the same first guess.
func TestEachStageRefusesWithItsOwnErrorAndStopsThere(t *testing.T) {
	boom := errors.New("boom")

	cases := []struct {
		name     string
		arrange  func(*harness)
		want     error
		notAfter step // must not have run
	}{
		{
			name:     "unknown tracker kind",
			arrange:  func(h *harness) { h.b.tracker = func(string) (core.TrackerKind, bool) { return nil, false } },
			want:     ErrUnknownKind,
			notAfter: stepTrackerStructural,
		},
		{
			name:     "unknown agent kind",
			arrange:  func(h *harness) { h.b.runner = func(string) (core.RunnerKind, bool) { return nil, false } },
			want:     ErrUnknownKind,
			notAfter: stepTrackerStructural,
		},
		{
			name:     "malformed tracker block",
			arrange:  func(h *harness) { h.tracker.structural = boom },
			want:     ErrStructural,
			notAfter: stepTrackerNew,
		},
		{
			name:     "malformed agent block",
			arrange:  func(h *harness) { h.runner.structural = boom },
			want:     ErrStructural,
			notAfter: stepTrackerNew,
		},
		{
			name:     "tracker cannot be constructed",
			arrange:  func(h *harness) { h.tracker.construct = boom },
			want:     ErrConstruct,
			notAfter: stepTrackerReady,
		},
		{
			name:     "tracker is not ready",
			arrange:  func(h *harness) { h.tracker.ready = boom },
			want:     ErrNotReady,
			notAfter: stepClaimPrincipal,
		},
		{
			name:     "runner cannot be constructed",
			arrange:  func(h *harness) { h.runner.construct = boom },
			want:     ErrConstruct,
			notAfter: stepRunnerReady,
		},
		{
			name:    "runner is not ready",
			arrange: func(h *harness) { h.runner.ready = boom },
			want:    ErrNotReady,
		},
		{
			name:     "the tracker cannot name its claim principal",
			arrange:  func(h *harness) { h.tracker.principal = boom },
			want:     ErrNotReady,
			notAfter: stepRepository,
		},
		{
			name:     "the tracker cannot name a repository",
			arrange:  func(h *harness) { h.tracker.repository = boom },
			want:     ErrNotReady,
			notAfter: stepRunnerNew,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)

			bundle, err := h.b.build(context.Background(), h.def(), nil, everything)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want one wrapping %v", err, tc.want)
			}
			// Nothing half-built escapes: the watcher publishes only on a nil
			// error, and a builder that returned a usable set beside one would
			// make that rule depend on the watcher's diligence.
			if bundle != nil {
				t.Errorf("a bundle was returned alongside %v", err)
			}
			if tc.notAfter != "" && h.j.has(tc.notAfter) {
				t.Errorf("%s ran after the refusal; the stages must stop at the first failure (%v)", tc.notAfter, h.j.seen())
			}
		})
	}
}

// The optional contracts are asked for by assertion, and their absence is a
// refusal rather than a default. Guessing either would be inventing the fact the
// whole assembly rests on: §9.8 cannot tell this daemon's claim from a human's
// without a principal, and the v1 workspace strategy exists only as a clone of
// some repository.
func TestATrackerMissingAnOptionalContractIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*harness)
		named   error
	}{
		{"no ClaimPrincipalSource", func(h *harness) { h.tracker.noPrincipalSource = true }, ErrNoClaimPrincipalSource},
		{"no RepositorySource", func(h *harness) { h.tracker.noRepositorySource = true }, workspace.ErrNoRepositorySource},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			tc.arrange(h)

			_, err := h.b.build(context.Background(), h.def(), nil, everything)
			// The stage, so `ben run` says which one refused...
			if !errors.Is(err, ErrNotReady) {
				t.Fatalf("error = %v, want one wrapping ErrNotReady", err)
			}
			// ...and the named refusal, so a test can assert on the reason
			// rather than on its wording.
			if !errors.Is(err, tc.named) {
				t.Errorf("error = %v, want one wrapping %v", err, tc.named)
			}
		})
	}
}

// SPEC §5.7 through #56's cascade: what a reload rebuilds follows from what each
// adapter was *constructed from*, not only from what the file says moved. The
// workspace strategy is built from the repository the tracker names (§6.2) and
// the checker from the provider and the tracker together (§9.7), so a rebuilt
// tracker obliges both — carrying either forward would leave it bound to the
// previous repository, or to a credential just rotated away from.
func TestReloadRebuildsTheCascadeAndCarriesTheRestForward(t *testing.T) {
	otherRoot := t.TempDir()

	cases := []struct {
		name    string
		changed config.AdapterChange
		next    func(*harness) *config.WorkflowDefinition
		// same lists the bundle members that must be pointer-identical to the
		// previous generation's.
		same []string
	}{
		{
			name:    "agent only",
			changed: config.AdapterChange{Agent: true},
			next:    func(h *harness) *config.WorkflowDefinition { return h.def() },
			same:    []string{"tracker", "workspaces", "verifier"},
		},
		{
			// Tracker ⇒ workspace ⇒ verifier, and workspace ⇒ runner, so a tracker
			// edit rebuilds everything.
			name:    "tracker cascades to workspace, verifier and runner",
			changed: config.AdapterChange{Tracker: true},
			next:    func(h *harness) *config.WorkflowDefinition { return h.def() },
			same:    nil,
		},
		{
			// The runner goes with the provider even when the agent block did not
			// move: it is constructed with a §9.10 evidence sink closed over *this*
			// provider (buildRunner), so one carried forward would record every launch
			// against the previous one. After a workspace.root edit that is a path
			// outside the new provider's tree, which RecordRun refuses — failing every
			// attempt at the marker upgrade.
			name:    "a hooks-only edit rebuilds the provider, the verifier and the runner",
			changed: config.AdapterChange{Workspace: true},
			next:    func(h *harness) *config.WorkflowDefinition { return h.def(withHook("echo hi")) },
			same:    []string{"tracker"},
		},
		{
			name:    "a workspace.root edit rebuilds the provider, the verifier and the runner",
			changed: config.AdapterChange{Workspace: true},
			next:    func(h *harness) *config.WorkflowDefinition { return h.def(withRoot(otherRoot)) },
			same:    []string{"tracker"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			first, err := h.b.build(context.Background(), h.def(), nil, everything)
			if err != nil {
				t.Fatalf("first build: %v", err)
			}

			second, err := h.b.build(context.Background(), tc.next(h), first, tc.changed)
			if err != nil {
				t.Fatalf("rebuild: %v", err)
			}

			carried := map[string]bool{
				"tracker":    second.Tracker == first.Tracker,
				"workspaces": second.Workspaces == first.Workspaces,
				"verifier":   second.Verifier == first.Verifier,
				"runner":     second.Runner == first.Runner,
			}
			for member, isSame := range carried {
				want := slices.Contains(tc.same, member)
				if isSame != want {
					verb := map[bool]string{true: "carried forward", false: "rebuilt"}
					t.Errorf("%s was %s; want it %s", member, verb[isSame], verb[want])
				}
			}
			// A carried-forward tracker keeps its principal and repository with
			// it: they are what readiness established for *that* instance.
			if second.Tracker == first.Tracker {
				if second.ClaimPrincipal != first.ClaimPrincipal || second.Repository != first.Repository {
					t.Error("the tracker was carried forward but its principal or repository was not")
				}
			}
			if second.Definition == first.Definition {
				t.Error("the rebuild published the previous definition")
			}
		})
	}
}

// The lock domain is scoped to the *tree*, not to the identity: two generations
// addressing one `<root>/<workflow_key>` must hold the same mutexes, or they
// serialize nothing over one base.git exactly while an operation from the
// previous generation is still completing (workspace.LockDomain).
//
// The provider's own half of this is TestARebuiltProviderSharesItsPredecessorsLockDomain
// in internal/workspace; what is asserted here is that assembly hands it over.
func TestASameTreeRebuildSharesTheLockDomain(t *testing.T) {
	h := newHarness(t)
	first, err := h.b.build(context.Background(), h.def(), nil, everything)
	if err != nil {
		t.Fatalf("first build: %v", err)
	}
	// A hooks edit: same root, same key, so the same tree.
	second, err := h.b.build(context.Background(), h.def(withHook("echo hi")), first, config.AdapterChange{Workspace: true})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if second.Workspaces == first.Workspaces {
		t.Fatal("the provider was not rebuilt, so this proves nothing about the domain")
	}
	if locksOf(t, second) != locksOf(t, first) {
		t.Error("a same-tree rebuild allocated a fresh lock domain; the two generations serialize nothing over one base.git")
	}

	// A different root is a different tree, and sharing a domain across them
	// would serialize two unrelated trees against each other.
	moved, err := h.b.build(context.Background(), h.def(withRoot(t.TempDir())), second, config.AdapterChange{Workspace: true})
	if err != nil {
		t.Fatalf("rebuild at a new root: %v", err)
	}
	if locksOf(t, moved) == locksOf(t, second) {
		t.Error("a rebuild at a different root carried the previous tree's lock domain")
	}
}

func locksOf(t *testing.T, b *orchestrator.Bundle) *workspace.LockDomain {
	t.Helper()
	p, ok := b.Workspaces.(*workspace.Provider)
	if !ok {
		t.Fatalf("bundle.Workspaces is %T, not *workspace.Provider", b.Workspaces)
	}
	return p.LockDomain()
}

// #56 revision 4 §3: a base cache the repository in force cannot serve fails the
// *reload*, not every subsequent attempt. validateBase runs inside Prepare, so
// without this check an adopted repository change is discovered one dispatched
// issue at a time, as a launch error.
//
// What this asserts is the wiring: CheckBaseCache is called during the build and
// its refusal becomes ErrNotReady. Which cache states it refuses is its own
// contract, covered against real repositories by internal/workspace's
// TestCheckBaseCache — one predicate, one place.
func TestAnUnusableBaseCacheFailsTheBuild(t *testing.T) {
	h := newHarness(t)
	def := h.def()

	// A file where base.git belongs: the shape CheckBaseCache refuses without
	// needing a repository to compare against.
	wfDir := filepath.Join(def.Config.Workspace.Root, def.Key)
	if err := os.MkdirAll(wfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wfDir, "base.git"), []byte("not a repository"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := h.b.build(context.Background(), def, nil, everything)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("error = %v, want one wrapping ErrNotReady", err)
	}
	if !errors.Is(err, workspace.ErrBaseRepoState) {
		t.Errorf("error = %v, want the workspace's own refusal carried through", err)
	}
	// Reached at build time, which is the point: the runner is built after the
	// workspace, so a refusal here must not have gone on to construct one.
	if h.j.has(stepRunnerNew) {
		t.Error("the runner was constructed after the base cache refused")
	}
}

// The lookups are injectable so the assembly can be driven in CI, which leaves
// one thing the fakes above structurally cannot cover: that the production
// builder is wired to the *real* kind table. A builder pointed at a stub would
// pass every test in this file.
//
// Structural only, so this stays inside the environment §5.8 guarantees — no
// credentials, no network, no installed harness.
func TestTheProductionBuilderResolvesTheRealKinds(t *testing.T) {
	b := newBuilder(slog.New(slog.NewTextHandler(io.Discard, nil)), "")

	for name, want := range map[string]bool{"github": true, "gitlab": false} {
		if _, ok := b.tracker(name); ok != want {
			t.Errorf("tracker(%q) resolved = %v, want %v", name, ok, want)
		}
	}
	for name, want := range map[string]bool{"claude-code": true, "codex-exec": true, "claude": false} {
		if _, ok := b.runner(name); ok != want {
			t.Errorf("runner(%q) resolved = %v, want %v", name, ok, want)
		}
	}

	// And they are the kinds that refuse: a registration pointing at a
	// Structural that accepts everything is an adapter that cannot refuse a typo
	// in its own opaque block (#55), which would read as green above.
	def, err := config.Load(writeWorkflow(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	def.Config.Agent.Provider = map[string]any{"permission_mode": "no-such-mode"}
	if _, _, err := structuralKinds(def, b.tracker, b.runner); !errors.Is(err, ErrStructural) {
		t.Errorf("error = %v, want one wrapping ErrStructural", err)
	}
}

// Each adapter gets its own slice of the definition, exactly (SPEC §5.2.5,
// §5.7). Asserted against the values the kinds actually received, because a
// constructor that discards its argument cannot fail when the caller passes the
// wrong one — an empty WorkflowKey, the tracker's block handed to the runner, a
// transcript directory that never arrives — and every such miswiring shows up
// first as a daemon that runs and misbehaves.
func TestTheBuilderPassesEachAdapterItsOwnSliceOfTheDefinition(t *testing.T) {
	h := newHarness(t)
	def := h.def()

	if _, err := h.b.build(context.Background(), def, nil, everything); err != nil {
		t.Fatalf("build: %v", err)
	}

	// Structural is asked about the block **as written** — including the legacy
	// credential spellings, which a reduced block would leave unvalidated — and
	// takes the core-owned fields with it, because a rule like non-empty
	// required_labels spans both (SPEC §5.7).
	wantStructural := core.TrackerConfig{
		Provider:       def.Config.Tracker.Provider,
		RequiredLabels: def.Config.Tracker.RequiredLabels,
		ActiveStates:   def.Config.Tracker.ActiveStates,
		TerminalStates: def.Config.Tracker.TerminalStates,
		// The workflow key, which nothing else supplies: it names this daemon in
		// tracker-visible writes (SPEC §5.1, §8.4), so an empty one would post
		// milestone comments and claim markers attributed to nobody.
		WorkflowKey: def.Key,
	}
	if !reflect.DeepEqual(wantStructural, h.tracker.gotStructural) {
		t.Errorf("tracker.Structural config:\n got %+v\nwant %+v", h.tracker.gotStructural, wantStructural)
	}

	// New is handed the **compiled** options: the reduced block, the promoted
	// fields, and one credential source (SPEC §8, amendment 9; §11).
	binding := def.Config.TrackerBinding()
	gotTracker := h.tracker.gotNew
	if gotTracker.Credential == nil {
		t.Fatal("tracker.New got no credential source; every legacy spelling compiles into one")
	}
	gotTracker.Credential = nil
	wantTracker := core.TrackerOptions{
		Provider:       binding.Provider,
		RequiredLabels: binding.RequiredLabels,
		ActiveStates:   binding.ActiveStates,
		TerminalStates: binding.TerminalStates,
		WorkflowKey:    def.Key,
		ClaimAssignee:  binding.ClaimAssignee,
	}
	if !reflect.DeepEqual(wantTracker, gotTracker) {
		t.Errorf("tracker.New options:\n got %+v\nwant %+v", gotTracker, wantTracker)
	}

	// The publish credential travels as a binding: a child variable and a
	// **source**, never a value, because nothing mints that credential until an
	// attempt does (SPEC §5.2.8, §5.5). A builder that dropped it would leave
	// the agent with no credential to push with, discovered at `git push`.
	wantPublish := core.PublishCredential{Env: "GH_TOKEN", Var: "BEN_TEST_PUBLISH_TOKEN"}

	gotRunner := h.gotRunnerNew()
	if gotRunner.Publish.Source == nil {
		t.Error("runner.New got no publish credential source")
	}
	if got := gotRunner.Publish.Env; got != wantPublish.Env {
		t.Errorf("runner.New publish env = %q, want %q", got, wantPublish.Env)
	}
	// A **distinct instance** from the tracker's (SPEC §11). The narrowing to the
	// fresh-only surface is a static property of core.PublishBinding.Source and
	// therefore not assertable at run time — this is the half that is: an
	// assembly that handed the publisher the tracker's instance would put the
	// two credentials on one cache, and their authorities are a load refusal
	// apart for a reason.
	if any(gotRunner.Publish.Source) == any(h.tracker.gotNew.Credential) {
		t.Error("the publisher and the tracker share one credential instance")
	}
	gotRunner.Publish = core.PublishBinding{}
	// OnRun is compared by presence rather than by value: it is a closure over the
	// workspace provider, and reflect.DeepEqual reports every non-nil func unequal.
	// That it is *wired at all* is the property worth pinning here — a nil one
	// leaves §9.10's marker unupgraded, so every crash parks instead of resuming —
	// and where it goes is asserted end to end in
	// TestTheRunEvidenceSinkRecordsAgainstTheLaunchedWorkspace.
	if gotRunner.OnRun == nil {
		t.Error("runner.New got no run-evidence sink; §9.10's marker would never carry a run's identity")
	}
	gotRunner.OnRun = nil
	wantRunner := core.RunnerOptions{
		Provider: def.Config.Agent.Provider,
		// The TTL gate's other operand, which lives in `limits` and reaches the
		// runner only through this field (SPEC §7.7).
		AttemptTimeout: time.Duration(def.Config.Limits.AttemptTimeoutMS) * time.Millisecond,
		TranscriptDir:  h.b.transcriptDir,
	}
	if !reflect.DeepEqual(wantRunner, gotRunner) {
		t.Errorf("runner.New options:\n got %+v\nwant %+v", gotRunner, wantRunner)
	}
	// Structural takes the whole agent configuration, and the same one New does:
	// the §7.6 reservation between the block and `publish.env` spans both, so a
	// kind asked about one without the other could not refuse a collision
	// (SPEC §5.7).
	wantAgent := def.Config.AgentBinding()
	if !reflect.DeepEqual(wantAgent, h.runner.gotStructural) {
		t.Errorf("runner.Structural config:\n got %+v\nwant %+v", h.runner.gotStructural, wantAgent)
	}
	if h.runner.gotStructural.Publish != wantPublish {
		t.Errorf("runner.Structural publish = %+v, want the reference %+v", h.runner.gotStructural.Publish, wantPublish)
	}
	// The two blocks are opaque and adapter-owned, so handing one kind the
	// other's block is a miswiring nothing downstream could detect.
	if _, leaked := h.runner.gotStructural.Provider["repo"]; leaked {
		t.Error("the tracker's provider block reached the runner kind")
	}
	if _, leaked := h.tracker.gotNew.Provider["permission_mode"]; leaked {
		t.Error("the agent's provider block reached the tracker kind")
	}
}

func (h *harness) gotRunnerNew() core.RunnerOptions {
	// StopGrace is deliberately not set by the builder: §5.2.7's knob set is
	// closed and has no entry for it, so the adapter's own default stands.
	return h.runner.gotNew
}

// hooksFrom is the whole of the workspace provider's hook configuration, and it
// is a field-by-field copy — the shape that silently drops a field when one is
// added. The provider offers no way to read them back, and the hooks themselves
// only fire inside Prepare and Dispose against a real repository, so the mapping
// is asserted here directly.
func TestHooksFromCopiesEveryField(t *testing.T) {
	got := hooksFrom(config.HooksConfig{
		AfterCreate:  "a",
		BeforeRun:    "b",
		AfterRun:     "c",
		BeforeRemove: "d",
		TimeoutMS:    1500,
	})
	want := workspace.Hooks{
		AfterCreate:  "a",
		BeforeRun:    "b",
		AfterRun:     "c",
		BeforeRemove: "d",
		// Milliseconds in the config (SPEC §5.2.6), a Duration in the provider.
		// A copy that forgot the conversion would read 1500ns and kill every
		// hook instantly.
		Timeout: 1500 * time.Millisecond,
	}
	if !reflect.DeepEqual(want, got) {
		t.Errorf("hooksFrom:\n got %+v\nwant %+v", got, want)
	}
}

// And it copies *every* field, including ones added after this was written. A
// table remembers the fields its author knew about; this fails for the next one
// — the same move BUILD.md decision 14 makes with the revision projection.
func TestHooksFromLeavesNoFieldBehind(t *testing.T) {
	// A config whose every string field is distinct and non-zero, built by
	// reflection so adding a field to HooksConfig cannot skip it.
	var cfg config.HooksConfig
	v := reflect.ValueOf(&cfg).Elem()
	for i := range v.NumField() {
		switch f := v.Field(i); f.Kind() {
		case reflect.String:
			f.SetString("hook-" + v.Type().Field(i).Name)
		case reflect.Int:
			f.SetInt(1500)
		default:
			t.Fatalf("config.HooksConfig.%s is a %s, which hooksFrom's mapping test does not know how to populate — extend it deliberately",
				v.Type().Field(i).Name, f.Kind())
		}
	}

	got := hooksFrom(cfg)
	gv := reflect.ValueOf(got)
	for i := range v.NumField() {
		name := v.Type().Field(i).Name

		// TimeoutMS is the one field whose name *and* type change on the way
		// across, so it cannot be checked by same-name lookup. Excused by name,
		// never by kind: excusing every int would let the next integer field
		// through in silence, which is the failure this whole test exists to
		// prevent one level up.
		if name == "TimeoutMS" {
			if want := 1500 * time.Millisecond; got.Timeout != want {
				t.Errorf("hooksFrom dropped TimeoutMS: Timeout = %v, want %v", got.Timeout, want)
			}
			continue
		}

		out := gv.FieldByName(name)
		if !out.IsValid() {
			t.Errorf("config.HooksConfig.%s has no workspace.Hooks counterpart; hooksFrom cannot be copying it", name)
			continue
		}
		if out.Kind() != v.Field(i).Kind() {
			t.Errorf("config.HooksConfig.%s is a %s and workspace.Hooks.%s is a %s; this test compares like for like, so the conversion needs asserting the way TimeoutMS is",
				name, v.Field(i).Kind(), name, out.Kind())
			continue
		}
		if !out.Equal(v.Field(i)) {
			t.Errorf("hooksFrom dropped %s: got %v, want %v", name, out, v.Field(i))
		}
	}
}

// SPEC §9.10: the run-evidence sink is what upgrades a workspace's run marker
// from "something may be live here" to a question a later daemon can ask. This
// drives the wire the builder installs, end to end into the real provider's
// marker store, because presence alone would not catch a sink pointed at the
// wrong workspace — and a marker upgraded against the wrong workspace is worse
// than one never upgraded: it reports a live run's workspace as free while
// parking an idle one (core.RunEvidenceSink).
func TestTheRunEvidenceSinkRecordsAgainstTheLaunchedWorkspace(t *testing.T) {
	h := newHarness(t)
	bundle, err := h.b.build(context.Background(), h.def(), nil, everything)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	sink := h.gotRunnerNew().OnRun
	if sink == nil {
		t.Fatal("the builder installed no run-evidence sink")
	}

	// The workspace the orchestrator would launch into, named by the provider
	// rather than spelled here: §6.3 gives key and path naming to the provider.
	issue := core.Issue{Identifier: "42"}
	if err := bundle.Workspaces.BeginRunMarkerFor(issue); err != nil {
		t.Fatalf("BeginRunMarkerFor: %v", err)
	}
	before, err := bundle.Workspaces.ReadRunMarkerFor(issue)
	if err != nil {
		t.Fatalf("ReadRunMarkerFor: %v", err)
	}
	if before.State != core.RunMarkerUnknownLaunch {
		t.Fatalf("marker before the upgrade = %v, want unknown_launch: BeginRun records no evidence, "+
			"which is the fork-to-record window §9.10 fails closed on", before.State)
	}

	ws, ok, err := bundle.Workspaces.ResolveWorkspace(context.Background(), issue)
	if err != nil || !ok {
		// No pin stands for an issue nothing prepared, so the path is derived the
		// way the runner's RunSpec would carry it.
		ws = core.Workspace{WorkspacePaths: core.WorkspacePaths{
			Path: filepath.Join(h.root, h.def().Key, "issues", workspace.Key("42")),
		}}
	}

	want := core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "4242", Boot: "boot-abc"}
	if err := sink(core.RunSpec{Workspace: core.WorkspacePaths{Path: ws.Path}}, want); err != nil {
		t.Fatalf("the sink refused a launched run: %v", err)
	}

	got, err := bundle.Workspaces.ReadRunMarkerFor(issue)
	if err != nil {
		t.Fatalf("ReadRunMarkerFor after the upgrade: %v", err)
	}
	if got.State != core.RunMarkerIdentified {
		t.Fatalf("marker = %v, want identified once the run exists", got.State)
	}
	if got.Evidence != want {
		t.Errorf("evidence = %+v, want %+v — the sink must record what the run reported", got.Evidence, want)
	}
}

// A workspace path outside this provider's tree is refused rather than resolved,
// so a miswired sink is a loud attempt failure instead of a marker written into
// someone else's state (safety invariant 2, SPEC §6.3).
func TestTheRunEvidenceSinkRefusesAForeignWorkspace(t *testing.T) {
	h := newHarness(t)
	if _, err := h.b.build(context.Background(), h.def(), nil, everything); err != nil {
		t.Fatalf("build: %v", err)
	}
	sink := h.gotRunnerNew().OnRun
	err := sink(
		core.RunSpec{Workspace: core.WorkspacePaths{Path: filepath.Join(t.TempDir(), "not-ours")}},
		core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "1", Boot: "b"})
	if err == nil {
		t.Error("the sink accepted a workspace outside the provider's tree")
	}
}
