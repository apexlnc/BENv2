package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Workspace ownership records: which issue a directory belongs to.
//
// SPEC §9.10 step 5 sweeps "workspaces of terminal-state issues", and §6.4 says
// the same. Deciding that needs the *issue* behind a directory, and Key() cannot
// be inverted: it appends an FNV-1a suffix whenever sanitization changed anything,
// which is exactly the case where the original is unrecoverable. Nor is the branch
// or the base pin any help — both are named after the key, not the identifier.
//
// So the mapping is written down when the workspace is created and removed when it
// is disposed. It lives beside `issues/` for the same reason the run marker does: a
// workspace *is* a git worktree the agent commits from, so anything stored inside
// one eventually gets committed.
//
// A directory with no owner record is not an error and not a guess. It is what a
// workspace created by an older BEN looks like, and what a partial disposal leaves;
// the sweep reports those and leaves them alone, because §6.4 keeps a failure's
// workspace and there is no way to tell the two apart without this file.

const ownerDirName = "owners"

// recordOwner writes the issue identifier a workspace belongs to.
//
// Not fsynced, deliberately, and this is the one place in this package that says
// so. The run marker's durability is load-bearing — a marker lost to a power cut is
// a workspace that reads as free with an agent starting in it — while a lost owner
// record costs an unswept directory that the sweep then reports by name. Buying the
// second guarantee at an fsync per prepare would be paying the launch path for
// startup hygiene.
func (p *Provider) recordOwner(key, identifier string) error {
	if key == "" || identifier == "" {
		return fmt.Errorf("%w: workspace owner needs both a key and an identifier", ErrPathEscape)
	}
	dir := p.ownerDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".owner-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	if _, err := tmp.WriteString(identifier); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// Atomic replace: a reader sees the previous identifier or the new one, never a
	// half-written name that would address the wrong issue.
	return os.Rename(tmp.Name(), p.ownerPath(key))
}

// clearOwner forgets a workspace's issue. Removing an absent record is not an
// error: Dispose is retried, and the second call must not fail.
func (p *Provider) clearOwner(key string) error {
	if err := os.Remove(p.ownerPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: clear owner record: %w", err)
	}
	return nil
}

// ListWorkspaces reports every workspace on disk and the issue it belongs to
// (SPEC §9.10 step 5).
//
// Driven by the directories rather than by the owner records, because the sweep is
// about what is *taking up space*: a record with no directory is stale bookkeeping
// and interesting to nobody, while a directory with no record is the case a caller
// has to be told about. That one comes back with an empty Identifier, which is a
// statement — "this exists and nothing says whose it is" — rather than an omission.
//
// Sorted, so a caller's log and its actions are in a stable order.
func (p *Provider) ListWorkspaces(_ context.Context) ([]core.WorkspaceRef, error) {
	entries, err := os.ReadDir(p.issuesDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// No workspaces have ever been created under this workflow.
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("workspace: list workspaces: %w", err)
	}

	var out []core.WorkspaceRef
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ref := core.WorkspaceRef{Key: e.Name(), Path: p.pathsFor(e.Name()).Path}
		raw, err := os.ReadFile(p.ownerPath(ref.Key))
		switch {
		case err == nil:
			ref.Identifier = strings.TrimSpace(string(raw))
		case errors.Is(err, fs.ErrNotExist):
			// Left empty: see above.
		default:
			return nil, fmt.Errorf("workspace: read owner record for %s: %w", ref.Key, err)
		}
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (p *Provider) ownerDir() string {
	return filepath.Join(filepath.Dir(p.issuesDir), ownerDirName)
}

func (p *Provider) ownerPath(key string) string {
	return filepath.Join(p.ownerDir(), key)
}
