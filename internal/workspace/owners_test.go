package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// SPEC §9.10 step 5 sweeps "workspaces of terminal-state issues", which needs the
// issue behind a directory — and Key() cannot be inverted, because it appends an
// FNV-1a suffix exactly when sanitization has destroyed the original. So the
// mapping is recorded at Prepare and cleared at Dispose, and these are the three
// things a caller depends on.

func TestPrepareRecordsTheIssueAWorkspaceBelongsTo(t *testing.T) {
	parallel(t)
	p, ctx := newTestProvider(t)

	// An identifier Key() cannot round-trip: the slash is sanitized and the hash
	// suffix makes the original unrecoverable from the name alone.
	const identifier = "org/repo#42"
	ws, err := prepareForTest(t, p, ctx, core.Issue{Identifier: identifier}, 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if ws.Key == identifier {
		t.Fatalf("key %q is the identifier; this fixture needs one Key() had to rewrite", ws.Key)
	}

	refs, err := p.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want one", refs)
	}
	if refs[0].Key != ws.Key || refs[0].Identifier != identifier {
		t.Errorf("ref = %+v, want key %q and identifier %q — without this the sweep cannot ask the tracker "+
			"whether the issue is terminal", refs[0], ws.Key, identifier)
	}
}

func TestDisposeClearsTheOwnerRecordButKeepingDoesNot(t *testing.T) {
	parallel(t)
	for _, tc := range []struct {
		name string
		keep bool
		// wantRefs is how many workspaces remain listed afterwards.
		wantRefs int
	}{
		{
			name:     "a disposed workspace is forgotten",
			wantRefs: 0,
		},
		{
			// §6.4 keeps a failure's workspace for debugging, and a later sweep still
			// has to be able to ask whose it is — otherwise the directory becomes
			// permanently unaccountable the moment it is retained.
			name:     "a kept workspace keeps its owner record",
			keep:     true,
			wantRefs: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, ctx := newTestProvider(t)
			ws, err := prepareForTest(t, p, ctx, core.Issue{Identifier: "7"}, 1)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if err := p.Dispose(ctx, ws, tc.keep); err != nil {
				t.Fatalf("Dispose: %v", err)
			}
			refs, err := p.ListWorkspaces(ctx)
			if err != nil {
				t.Fatalf("ListWorkspaces: %v", err)
			}
			if len(refs) != tc.wantRefs {
				t.Errorf("refs = %+v, want %d", refs, tc.wantRefs)
			}
			if tc.wantRefs == 1 && refs[0].Identifier != "7" {
				t.Errorf("kept workspace lost its owner: %+v", refs[0])
			}
		})
	}
}

// A directory with no owner record comes back named but unattributed, rather than
// omitted. That is what a workspace from an older BEN looks like, and what an
// interrupted disposal leaves — and the sweep has to be told about it, because
// §6.4 keeps a failure's workspace and "unowned" therefore cannot mean
// "disposable".
func TestAWorkspaceWithNoOwnerRecordIsListedWithoutAnIssue(t *testing.T) {
	parallel(t)
	p, ctx := newTestProvider(t)
	ws, err := prepareForTest(t, p, ctx, core.Issue{Identifier: "7"}, 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := os.Remove(p.ownerPath(ws.Key)); err != nil {
		t.Fatalf("removing the owner record: %v", err)
	}

	refs, err := p.ListWorkspaces(ctx)
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("refs = %+v, want the directory still reported", refs)
	}
	if refs[0].Key != ws.Key || refs[0].Identifier != "" {
		t.Errorf("ref = %+v, want the key with no identifier", refs[0])
	}
}

// The owner record lives outside the worktree, for the reason the run marker does:
// a workspace *is* a git worktree the agent commits from, so anything stored
// inside one eventually gets committed.
func TestTheOwnerRecordIsNotInsideTheWorktree(t *testing.T) {
	parallel(t)
	p, ctx := newTestProvider(t)
	ws, err := prepareForTest(t, p, ctx, core.Issue{Identifier: "7"}, 1)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	rel, err := filepath.Rel(ws.Path, p.ownerPath(ws.Key))
	if err == nil && !filepath.IsAbs(rel) && rel != ".." && len(rel) > 2 && rel[:3] != ".."+string(filepath.Separator) {
		t.Errorf("the owner record %s is inside the worktree %s; an agent told to commit its work "+
			"would eventually commit it", p.ownerPath(ws.Key), ws.Path)
	}
}

func newTestProvider(t *testing.T) (*Provider, context.Context) {
	t.Helper()
	return newProvider(t, newFixture(t), Hooks{}), context.Background()
}
