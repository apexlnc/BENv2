package remotews_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
	"github.com/srhg-ai-7cef3f93/ben/internal/orchestrator"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote/remotetest"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
)

// The strategy under test is driven against two fakes whose fidelity is read off
// the components they stand in for: remotetest.Backend for the three #192 seams,
// and fake.Mirror for the daemon-side fact source (#193, both run their owners'
// conformance suites unmodified). Nothing here stubs the strategy's own rules.
//
// The one thing every test in this package shares is `local`: a directory that
// stands for the daemon's own disk. No test ever writes to it, and several
// assert it is still empty afterwards — which is the acceptance criterion that a
// remote claim creates no local worktree, checked where a mistake would actually
// land rather than by reading the code.

// The strategy is a drop-in for the loop's workspace seam. A compile-time
// assertion rather than a runtime one: the two packages must not import each
// other outside a test, and this is the only place the claim can be *proved*
// instead of asserted in a comment.
var _ orchestrator.Workspaces = (*remotews.Provider)(nil)

const (
	profileRev    = "profile-rev-1"
	issueID       = "7"
	gitRepository = "acme/widgets"
	hookWindow    = 5 * time.Second
)

type rig struct {
	t        *testing.T
	backend  *remotetest.Backend
	mirror   *fake.Mirror
	cycles   *stubCycles
	disposer *stubDisposer
	journals *remote.DirStore
	store    *remotews.DirStore
	provider *remotews.Provider
	issue    core.Issue
	// local stands for the daemon's own disk. The remote path must never touch it.
	local string
}

type rigOption func(*remotews.Options)

// withHooks installs the workflow's four lifecycle scripts.
func withHooks(h remote.Hooks) rigOption {
	return func(o *remotews.Options) {
		h.Timeout = hookWindow
		o.Hooks = h
	}
}

func newRig(t *testing.T, opts ...rigOption) *rig {
	t.Helper()
	dir := t.TempDir()
	r := &rig{
		t:        t,
		backend:  remotetest.New(profileRev),
		mirror:   fake.NewMirror(),
		cycles:   &stubCycles{},
		disposer: &stubDisposer{disposition: remotews.DispositionDeleted},
		journals: remote.NewDirStore(filepath.Join(dir, "journal")),
		store:    remotews.NewDirStore(filepath.Join(dir, "cycles")),
		issue:    fake.Issue(issueID, time.Date(2026, 8, 25, 9, 0, 0, 0, time.UTC)),
		local:    filepath.Join(dir, "local"),
	}
	r.cycles.approval = 100

	options := remotews.Options{
		Repository:    r.mirror.Repository(),
		GitRepository: gitRepository,
		Workspaces:    r.backend.Workspaces(),
		Processes:     r.backend,
		Journals:      r.journals,
		Consumptions:  remote.NewDirConsumer(filepath.Join(dir, "journal")),
		HookExec:      r.backend,
		HookStore:     remote.NewHookDirStore(filepath.Join(dir, "journal")),
		Hooks:         remote.Hooks{Timeout: hookWindow},
		Base:          r.mirror,
		Cycles:        r.cycles,
		Store:         r.store,
		Disposer:      r.disposer,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(&options)
	}
	provider, err := remotews.New(options)
	if err != nil {
		t.Fatalf("remotews.New: %v", err)
	}
	r.provider = provider
	return r
}

// begin is BeginClaimBase for one assignment epoch.
func (r *rig) begin(epoch int64) error {
	r.t.Helper()
	return r.provider.BeginClaimBase(context.Background(), r.issue, epoch)
}

// prepare is PrepareClaim, and every test that calls it is exercising the whole
// ordered sequence: quiet gate, pin, acquire, after_create, restore, before_run.
func (r *rig) prepare(attempt int, epoch int64) (core.Workspace, error) {
	r.t.Helper()
	ws, _, err := r.provider.PrepareClaim(context.Background(), r.issue, attempt, epoch)
	return ws, err
}

// mustPrepare is prepare for the many tests whose subject is what happens after
// a successful one.
func (r *rig) mustPrepare(attempt int, epoch int64) core.Workspace {
	r.t.Helper()
	ws, err := r.prepare(attempt, epoch)
	if err != nil {
		r.t.Fatalf("PrepareClaim(attempt %d, epoch %d): %v", attempt, epoch, err)
	}
	return ws
}

// claim is the backend address the current workspace cycle selects.
func (r *rig) claim() remote.Claim {
	r.t.Helper()
	return remote.Claim{
		Repository: r.mirror.Repository(), Issue: issueID, Epoch: r.cycles.approval,
	}
}

// sandbox is the backend's identity for the current cycle.
func (r *rig) sandbox() remote.Identity {
	r.t.Helper()
	id, err := r.backend.Workspaces().Attach(context.Background(), r.claim())
	if err != nil {
		r.t.Fatalf("Attach: %v", err)
	}
	return id
}

// dispatch writes the durable run record a launched attempt would leave, without
// going through remote.Runner: the tests here are about the *strategy's* view of
// a run, and the runner's own recovery is internal/remote's subject.
func (r *rig) dispatch(runID remote.RunID) remote.ProcessRef {
	r.t.Helper()
	identity := r.sandbox()
	spec := remote.ProcessSpec{Identity: identity, Argv: []string{"agent"}}
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		r.t.Fatal(err)
	}
	ref := remote.ProcessRef{Identity: identity, RunID: runID, RequestDigest: digest}
	journal, err := remote.Reserve(context.Background(), r.journals, ref, remote.Meta{})
	if err != nil {
		r.t.Fatalf("Reserve: %v", err)
	}
	if _, err := journal.Dispatch(context.Background(),
		func(ctx context.Context, ref remote.ProcessRef) (remote.Status, error) {
			return r.backend.Start(ctx, ref, spec)
		}); err != nil {
		r.t.Fatalf("Dispatch: %v", err)
	}
	return ref
}

// hookScripts is every script the backend was asked to run, in order, as
// `<phase>/<attempt>`.
func (r *rig) hookScripts() []string {
	r.t.Helper()
	var out []string
	for _, call := range r.backend.Hooks() {
		out = append(out, string(call.Phase))
	}
	return out
}

// prepareCall is the last typed Airlock Git prepare the backend ran.
func (r *rig) prepareCall() remotetest.HookCall {
	r.t.Helper()
	for i := len(r.backend.Hooks()) - 1; i >= 0; i-- {
		if call := r.backend.Hooks()[i]; call.Phase == remote.HookGitPrepare {
			return call
		}
	}
	r.t.Fatal("no typed Git prepare ran")
	return remotetest.HookCall{}
}

// localIsUntouched is the acceptance criterion that a remote claim creates no
// local worktree, asserted against a directory nothing in this package is
// configured with.
func (r *rig) localIsUntouched() {
	r.t.Helper()
	if _, err := filepath.Glob(filepath.Join(r.local, "*")); err != nil {
		r.t.Fatal(err)
	}
	entries, err := filepath.Glob(filepath.Join(r.local, "**"))
	if err != nil {
		r.t.Fatal(err)
	}
	if len(entries) != 0 {
		r.t.Fatalf("the remote path created %d local entries under %s", len(entries), r.local)
	}
}

// stubCycles is the standing approval anchor. A field rather than a function so
// a test can move the approval — which is the whole revocation-and-reapproval
// case — without rewiring the provider.
type stubCycles struct {
	mu       sync.Mutex
	approval int64
	err      error
}

func (s *stubCycles) WorkspaceCycle(context.Context, core.Issue) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.approval, s.err
}

func (s *stubCycles) set(approval int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.approval = approval
}

type disposeCall struct {
	Claim   remote.Claim
	Outcome remotews.Outcome
	Quiet   bool
}

type stubDisposer struct {
	mu          sync.Mutex
	calls       []disposeCall
	disposition remotews.Disposition
	err         error
}

func (d *stubDisposer) Complete(
	_ context.Context, claim remote.Claim, outcome remotews.Outcome, prev remote.Status,
) (remotews.Disposition, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, disposeCall{Claim: claim, Outcome: outcome, Quiet: remote.MayReuse(prev)})
	return d.disposition, d.err
}

func (d *stubDisposer) setDisposition(disposition remotews.Disposition) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disposition = disposition
}

func (d *stubDisposer) Calls() []disposeCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]disposeCall(nil), d.calls...)
}

func contains(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("expected %q to contain %q", haystack, needle)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got %v, want %v", err, target)
	}
}
