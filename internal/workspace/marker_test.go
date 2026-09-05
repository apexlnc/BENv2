package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

func markerProvider(t *testing.T) *Provider {
	t.Helper()
	p, err := providerFromOptions(t, Options{
		Root:        t.TempDir(),
		WorkflowKey: "wf",
		Repository:  repo("https://github.com/o/r.git"),
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The three readings of §9.10, in the order one attempt produces them.
func TestRunMarkerLifecycle(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	const key = "issue-1"
	wsPath := filepath.Join(p.issuesDir, key)

	if m, err := p.ReadRun(key); err != nil || m.State != MarkerAbsent {
		t.Fatalf("before Begin: %v, %v; want absent", m.State, err)
	}

	// Before the launch: something may be live here, but the launch outcome is
	// not yet known. §9.10 parks this state rather than guessing.
	if err := p.BeginRun(key); err != nil {
		t.Fatal(err)
	}
	if m, err := p.ReadRun(key); err != nil || m.State != MarkerUnknownLaunch {
		t.Fatalf("after Begin: %v, %v; want unknown_launch", m.State, err)
	}

	want := core.RunEvidence{Scheme: core.RunEvidenceLocal, ID: "4242", Boot: "boot-abc"}
	if err := p.RecordRun(wsPath, want); err != nil {
		t.Fatal(err)
	}
	m, err := p.ReadRun(key)
	if err != nil {
		t.Fatal(err)
	}
	if m.State != MarkerIdentified {
		t.Fatalf("after Record: %v; want identified", m.State)
	}
	if m.Evidence != want {
		t.Errorf("evidence = %+v, want %+v", m.Evidence, want)
	}

	if err := p.ClearRun(key); err != nil {
		t.Fatal(err)
	}
	if m, err := p.ReadRun(key); err != nil || m.State != MarkerAbsent {
		t.Fatalf("after Clear: %v, %v; want absent", m.State, err)
	}
}

// A marker must never live inside the workspace: a workspace is a git worktree
// the agent commits from, and an agent told to commit its work would eventually
// commit the marker.
func TestRunMarkerLivesOutsideTheWorkspace(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	const key = "issue-1"
	if err := p.BeginRun(key); err != nil {
		t.Fatal(err)
	}
	path := p.markerPath(key)
	if inWorkspace, err := filepath.Rel(p.issuesDir, path); err == nil &&
		!filepath.IsAbs(inWorkspace) && inWorkspace != "" && inWorkspace[0] != '.' {
		t.Errorf("marker at %s is inside the issues tree (%s): an agent would commit it",
			path, p.issuesDir)
	}
}

// Clearing is the only route out, so it must be safe to reach twice — recovery
// and the run-time path can both conclude the same run is gone.
func TestClearRunIsIdempotent(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	if err := p.ClearRun("never-began"); err != nil {
		t.Errorf("clearing an absent marker = %v, want nil", err)
	}
}

// A marker present but unreadable is unknown_launch, never absent. Its evidence
// is unusable either way; the difference is that one parks a human and the other
// hands a possibly-live worktree to a second agent.
func TestUnparseableMarkerReadsAsUnknownLaunch(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	const key = "issue-1"
	if err := p.BeginRun(key); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.markerPath(key), []byte("{ truncated"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := p.ReadRun(key)
	if err != nil {
		t.Fatalf("ReadRun = %v, want a verdict rather than an error", err)
	}
	if m.State != MarkerUnknownLaunch {
		t.Errorf("state = %v, want unknown_launch: a corrupt marker must not read as free", m.State)
	}
}

// RecordRun is addressed by workspace path because that is what a runner has.
// A path outside this provider's tree is someone else's state.
func TestRecordRunRefusesForeignPaths(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	for _, tt := range []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"outside the root", "/tmp/elsewhere/issue-1"},
		{"the issues dir itself", p.issuesDir},
		{"nested deeper than a workspace", filepath.Join(p.issuesDir, "issue-1", "sub")},
		{"traversal", filepath.Join(p.issuesDir, "issue-1", "..", "..", "escape")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := p.RecordRun(tt.path, core.RunEvidence{Scheme: "pgid", ID: "1"})
			if !errors.Is(err, ErrPathEscape) {
				t.Errorf("RecordRun(%q) = %v, want ErrPathEscape", tt.path, err)
			}
		})
	}
}

// The upgrade replaces the marker whole, so a reader sees the old state or the
// new one. A partially written file would parse as unknown_launch and park a
// workspace whose run was perfectly identifiable.
func TestRecordRunIsAtomic(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	const key = "issue-1"
	wsPath := filepath.Join(p.issuesDir, key)
	if err := p.BeginRun(key); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordRun(wsPath, core.RunEvidence{Scheme: "pgid", ID: "7", Boot: "b"}); err != nil {
		t.Fatal(err)
	}
	// No temp files left behind: a stray .marker-* would be read by nothing, but
	// it accumulates once per attempt forever.
	entries, err := os.ReadDir(p.markerDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("runs dir holds %v, want only the marker", names)
	}
	raw, err := os.ReadFile(p.markerPath(key))
	if err != nil {
		t.Fatal(err)
	}
	var m markerFile
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("marker on disk is not valid JSON: %v", err)
	}
}

// Two workspaces have two markers. One overwriting the other would free a live
// run's workspace while parking an idle one.
func TestMarkersArePerWorkspace(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	if err := p.BeginRun("issue-1"); err != nil {
		t.Fatal(err)
	}
	if err := p.RecordRun(filepath.Join(p.issuesDir, "issue-1"),
		core.RunEvidence{Scheme: "pgid", ID: "111", Boot: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginRun("issue-2"); err != nil {
		t.Fatal(err)
	}

	first, err := p.ReadRun("issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.State != MarkerIdentified || first.Evidence.ID != "111" {
		t.Errorf("issue-1 = %+v, want its own identified marker", first)
	}
	second, err := p.ReadRun("issue-2")
	if err != nil {
		t.Fatal(err)
	}
	if second.State != MarkerUnknownLaunch {
		t.Errorf("issue-2 = %v, want unknown_launch", second.State)
	}
}

// A write that fails part-way must leave nothing behind. The success path
// removes the temp file by renaming it into place, so only a failure exercises
// the cleanup — and a leak there is invisible until a runs/ directory has one
// stray file per failed attempt, forever.
func TestFailedMarkerWriteLeavesNoTemp(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	const key = "issue-1"
	// A directory where the marker file belongs: the rename cannot replace it.
	if err := os.MkdirAll(p.markerPath(key), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.markerPath(key), "occupied"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.BeginRun(key); err == nil {
		t.Fatal("BeginRun succeeded onto a directory, want an error")
	}
	entries, err := os.ReadDir(p.markerDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".marker-") {
			t.Errorf("failed write left %s behind; runs/ would accumulate one per failure", e.Name())
		}
	}
}

// A marker entry that exists but does not resolve is present, not absent.
// os.ReadFile reports ENOENT for a dangling symlink exactly as it does for a
// missing file, and reading the first as "free" hands a possibly-live worktree
// to a second agent — the one outcome §9.10's precondition exists to prevent.
func TestDanglingMarkerIsNotAbsent(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	const key = "issue-1"
	if err := os.MkdirAll(p.markerDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(p.markerDir(), "no-such-target.json"), p.markerPath(key)); err != nil {
		t.Fatal(err)
	}
	m, err := p.ReadRun(key)
	if err != nil {
		t.Fatalf("ReadRun = %v, want a verdict", err)
	}
	if m.State != MarkerUnknownLaunch {
		t.Errorf("state = %v, want unknown_launch: a dangling marker read as %v frees "+
			"a workspace whose run may still be live", m.State, m.State)
	}
}

// A runs/ that does not resolve must never read as absence. Lstat declines to
// follow only the last path component, so an ancestor that dangles yields the
// same ENOENT as a free workspace — and this one frees every workspace in the
// workflow at once, not just the one being asked about.
func TestUnresolvableMarkerStoreIsNotAbsent(t *testing.T) {
	parallel(t)
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, p *Provider)
	}{
		{"dangling symlink", func(t *testing.T, p *Provider) {
			if err := os.Symlink(filepath.Join(p.root, "no-such-dir"), p.markerDir()); err != nil {
				t.Fatal(err)
			}
		}},
		{"a regular file", func(t *testing.T, p *Provider) {
			if err := os.WriteFile(p.markerDir(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := markerProvider(t)
			if err := os.MkdirAll(filepath.Dir(p.markerDir()), 0o700); err != nil {
				t.Fatal(err)
			}
			tt.setup(t, p)
			m, err := p.ReadRun("issue-1")
			if err == nil {
				t.Errorf("ReadRun = %v, nil; want an error: an unreadable store reported "+
					"as %v frees every workspace in the workflow", m.State, m.State)
			}
			if m.State == MarkerAbsent && err == nil {
				t.Error("state = absent")
			}
		})
	}
}

// The ordinary case stays absence: no attempt has ever been launched here.
func TestMissingMarkerStoreIsAbsent(t *testing.T) {
	parallel(t)
	p := markerProvider(t)
	m, err := p.ReadRun("issue-1")
	if err != nil || m.State != MarkerAbsent {
		t.Errorf("ReadRun on a fresh tree = %v, %v; want absent, nil", m.State, err)
	}
}
