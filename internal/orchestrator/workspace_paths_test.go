package orchestrator

import (
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/fake"
)

// SPEC §6.1 → §7.1: a dispatch hands the adapter *every* path the provider
// reported, not the worktree alone.
//
// Asserted at the loop's own seam because nothing else can. An adapter needing
// a path it was not given refuses at Start — claude-code's isolated posture does
// exactly that (#114) — but the orchestrator's own suite is built on a fake
// runner that reads none of them, so a pipeline forwarding `{Path: …}` and
// dropping the rest leaves every test in this package green and every real
// `isolated` dispatch failing with ErrPrivateDir.
//
// The comparison is whole-struct against what Prepare reported, deliberately not
// field-by-field: a fourth path added to WorkspacePaths is then carried by this
// assertion the moment it exists, which is the property the grouped type buys
// and a per-field check would give back.
func TestDispatchForwardsEveryReportedWorkspacePath(t *testing.T) {
	h := start(t, harnessOpts{
		issues: []core.Issue{fake.Issue("1", epoch)},
		script: startedOnly,
		hang:   true,
	})
	h.WaitState("1", StateRunning)

	spec, ok := h.Runner.LastSpec()
	if !ok {
		t.Fatal("no run was started")
	}
	// What the provider reported for this issue, read back from the fake rather
	// than restated here: the claim under test is that the loop forwards what it
	// was given, and a literal in this file would only prove it forwards what
	// this file expects.
	ws, err := prepareWorkspaceForTest(h.Workspaces, t.Context(), fake.Issue("1", epoch), 1)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Workspace != ws.WorkspacePaths {
		t.Errorf("dispatch carried %+v, want every path the provider reported: %+v",
			spec.Workspace, ws.WorkspacePaths)
	}
	// Each of the three named once, so a failure says which is missing rather
	// than printing two structs and leaving the reader to diff them.
	for _, f := range []struct{ what, got string }{
		{"worktree", spec.Workspace.Path},
		{"shared git dir", spec.Workspace.SharedGitDir},
		{"private dir", spec.Workspace.PrivateDir},
	} {
		if f.got == "" {
			t.Errorf("the %s never reached the adapter; an adapter may not derive one (SPEC §7.1)", f.what)
		}
	}
}
