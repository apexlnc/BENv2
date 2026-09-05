package remotews_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
	"github.com/srhg-ai-7cef3f93/ben/internal/remotews"
)

// What survives a restart, and what a restart must not do twice.
//
// Every test here builds a *second* provider over the same durable stores and
// the same backend, which is as close to a new process as one package can get:
// nothing in-memory carries over, so anything that still holds was written down.

// restart returns a fresh provider over this rig's stores. Nothing is shared
// but the disk and the backend, which is the whole point.
func (r *rig) restart() *rig {
	return r.restartWithStore(r.store)
}

func (r *rig) restartWithStore(store remotews.Store) *rig {
	r.t.Helper()
	next := *r
	provider, err := remotews.New(remotews.Options{
		Repository:    r.mirror.Repository(),
		GitRepository: gitRepository,
		Workspaces:    r.backend.Workspaces(), Processes: r.backend,
		Journals:  r.journals,
		HookExec:  r.backend,
		HookStore: remote.NewHookDirStore(r.journals.Root()),
		Hooks:     remote.Hooks{Timeout: hookWindow},
		Base:      r.mirror, Cycles: r.cycles, Store: store,
		Disposer: r.disposer,
	})
	if err != nil {
		r.t.Fatalf("restarting the strategy: %v", err)
	}
	next.provider = provider
	return &next
}

// The workspace cycle, its verification base/target and its attempt are durable: a
// restarted daemon reattaches the same sandbox rather than acquiring a second
// one, and the claim keeps the base its run was dispatched against.
func TestARestartReattachesRatherThanAcquiringAgain(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	before := r.mustPrepare(1, 11)
	sandbox := r.sandbox()
	acquires := r.backend.Acquires()

	next := r.restart()
	ws, found, err := next.provider.ResolveWorkspace(context.Background(), next.issue)
	if err != nil || !found {
		t.Fatalf("ResolveWorkspace after a restart = %+v, %v, %v", ws, found, err)
	}
	if ws.Path != before.Path || ws.BaseSHA != before.BaseSHA ||
		ws.TargetBranch != before.TargetBranch || ws.ClaimEpoch != before.ClaimEpoch {
		t.Fatalf("the restart resolved %+v, want %+v", ws, before)
	}
	if r.backend.Acquires() != acquires {
		t.Fatal("resolving a workspace after a restart acquired one")
	}

	// And the next attempt reattaches the same sandbox at the same pinned
	// revision, without allocating a second.
	retried := next.mustPrepare(2, 11)
	if retried.TargetBranch != before.TargetBranch {
		t.Fatalf("the restart moved the target from %q to %q", before.TargetBranch, retried.TargetBranch)
	}
	after := next.sandbox()
	if after.SandboxID != sandbox.SandboxID || after.ProfileRevision != sandbox.ProfileRevision {
		t.Fatalf("the restart moved to %+v, want %+v", after, sandbox)
	}
}

// A restart that finds a dispatched run does not start a replacement: the marker
// is the journal, and only a positive domain-quiet observation frees it.
func TestARestartDoesNotDispatchOverALiveRun(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	r.dispatch("run-1")

	next := r.restart()
	marker, err := next.provider.ReadRunMarkerFor(next.issue)
	if err != nil {
		t.Fatal(err)
	}
	if marker.State != core.RunMarkerIdentified {
		t.Fatalf("a restart reads the marker as %v, want identified", marker.State)
	}
	gone, err := next.provider.RunGone(marker.Evidence)
	if err != nil || gone {
		t.Fatalf("RunGone across a restart = %v, %v; want false over a live run", gone, err)
	}
	_, err = next.prepare(2, 11)
	wantErr(t, err, remote.ErrNotQuiet)

	// Once the backend attests quiet, the same restarted daemon proceeds — and
	// the run it retires is the one it found, not a replacement it minted.
	r.backend.Quiet("run-1")
	if gone, err := next.provider.RunGone(marker.Evidence); err != nil || !gone {
		t.Fatalf("RunGone after quiet = %v, %v; want true", gone, err)
	}
	if _, err := next.prepare(2, 11); err != nil {
		t.Fatalf("PrepareClaim after quiet: %v", err)
	}
	if got := r.backend.RunCreations(); got != 1 {
		t.Fatalf("the backend ran %d processes across the restart, want 1", got)
	}
}

// The cycle record is a whole-record replacement, so a reader sees one state or
// the previous one — never a splice of an approval from one and a base from
// another.
func TestTheCycleRecordIsReplacedWhole(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)

	path := r.store.Path(ws.Key)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"approval": 100`, `"epoch": 11`, `"state": "pinned"`} {
		contains(t, string(body), want)
	}
	// The cycle base is recorded separately from the verification base, which is
	// the two-clock design made durable.
	contains(t, string(body), `"cycle_base_sha"`)

	// A record this package did not write is refused rather than repaired.
	if err := os.WriteFile(path, []byte(`{"version":1,"issue":"7","key":"issue-7","surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = r.restart().provider.ClaimBase(context.Background(), r.issue)
	wantErr(t, err, remotews.ErrCycleState)
	if !strings.Contains(err.Error(), "surprise") {
		t.Fatalf("the refusal does not name the unknown field: %v", err)
	}
}

// A record from a newer version is refused rather than half-understood: it is
// an address, and a misread address dispatches into the wrong sandbox.
func TestAFutureRecordVersionIsRefused(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	ws := r.mustPrepare(1, 11)
	body, err := os.ReadFile(r.store.Path(ws.Key))
	if err != nil {
		t.Fatal(err)
	}
	bumped := strings.Replace(string(body), `"version": 2`, `"version": 99`, 1)
	if bumped == string(body) {
		t.Fatal("the fixture did not change the version")
	}
	if err := os.WriteFile(r.store.Path(ws.Key), []byte(bumped), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = r.restart().provider.ClaimBase(context.Background(), r.issue)
	wantErr(t, err, remotews.ErrCycleState)
}

// A malformed obligation cannot disappear from the listing that owns dispatch
// safety. Both recovery enumeration and replacement preparation refuse the
// complete directory rather than skipping the entry and pruning its pin.
func TestAnUnreadableCycleDisposalIsNeverTreatedAsAbsent(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if err := r.begin(11); err != nil {
		t.Fatal(err)
	}
	r.mustPrepare(1, 11)
	r.cycles.set(200)
	if err := r.begin(21); err != nil {
		t.Fatal(err)
	}
	refs := endedCycles(t, r.provider)
	if len(refs) != 1 {
		t.Fatalf("ended cycles = %+v, want one", refs)
	}
	path := r.store.DisposalPath(refs[0].Path)
	if err := os.WriteFile(path, []byte(`{"version":1,"surprise":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	next := r.restart()
	if _, err := next.provider.EndedCycles(context.Background()); !errors.Is(err, remotews.ErrCycleState) {
		t.Fatalf("EndedCycles = %v, want ErrCycleState", err)
	}
	acquires := r.backend.Acquires()
	if _, err := next.prepare(1, 21); !errors.Is(err, remotews.ErrCycleState) {
		t.Fatalf("PrepareClaim = %v, want ErrCycleState", err)
	}
	if got := r.backend.Acquires(); got != acquires {
		t.Fatalf("unreadable obligation allowed an acquire: %d -> %d", acquires, got)
	}
}
