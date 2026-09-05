package harness

import (
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Legacy pgid evidence remains readable until a reboot can positively retire
// it, so its boot source must stay stable across processes.
func TestBootIdentityIsResolvable(t *testing.T) {
	if got := bootID(); got == "" {
		t.Fatal("bootIdentity() is empty")
	}
	if fresh := bootIdentity(); fresh != bootID() {
		t.Errorf("bootIdentity() = %q on a re-read, cached %q", fresh, bootID())
	}
}

func TestBindEvidenceKeepsTheWorkspaceAddress(t *testing.T) {
	spec := core.RunSpec{Workspace: core.WorkspacePaths{Path: "/workspace/one"}}
	want := core.RunEvidence{Scheme: "domain", ID: "run-1"}
	var gotSpec core.RunSpec
	var gotEvidence core.RunEvidence
	bound := BindEvidence(func(s core.RunSpec, e core.RunEvidence) error {
		gotSpec, gotEvidence = s, e
		return nil
	}, spec)
	if err := bound(want); err != nil {
		t.Fatal(err)
	}
	if gotSpec.Workspace.Path != spec.Workspace.Path || gotEvidence != want {
		t.Fatalf("bound sink = (%+v, %+v), want (%+v, %+v)", gotSpec, gotEvidence, spec, want)
	}
	if BindEvidence(nil, spec) != nil {
		t.Fatal("nil sink became non-nil")
	}
}
