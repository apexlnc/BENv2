package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
)

// Run markers: the workspace precondition of SPEC §9.10.
//
// A workspace whose previous run is not confirmed gone may not be reused,
// disposed, or released. Within one process the run handle answers that; across
// a restart there is no handle, so the answer has to have been written down
// *before* the crash — which is the whole design constraint here. A marker
// written at shutdown would be worthless: `kill -9` writes nothing.
//
// The marker therefore has two lifetimes and three readings. It is created
// before the launch, meaning only "a run may be live in this workspace"; it is
// upgraded once the run exists to carry evidence identifying it; and it is
// removed only when that run is confirmed gone — the same fact, and the same
// linearization point, that frees a workspace at run time (§9.8).
//
// It lives beside `issues/` rather than inside a workspace, because a workspace
// *is* a git worktree the agent commits from: a marker in there would show up in
// `git status`, and an agent told to commit its work would eventually commit it.

const markerDirName = "runs"

// MarkerState is recovery's reading of one workspace's marker (SPEC §9.10).
type MarkerState string

const (
	// MarkerAbsent — the workspace is free.
	MarkerAbsent MarkerState = "absent"
	// MarkerIdentified — present, carrying evidence. The run can be probed, and
	// only proof of its absence frees the workspace.
	MarkerIdentified MarkerState = "identified"
	// MarkerUnknownLaunch — present without usable evidence, so the launch
	// outcome is unknown. It covers a crash before the launch, a crash after it
	// and before the upgrade, and an interrupted cleanup of a failed launch.
	// The three are indistinguishable and one of them has a live run in it, so
	// §9.10 parks for a human rather than guessing in either direction.
	MarkerUnknownLaunch MarkerState = "unknown_launch"
)

// Marker is one workspace's run marker as read back.
type Marker struct {
	State MarkerState
	// Evidence is set only when State is MarkerIdentified.
	Evidence core.RunEvidence
}

// markerFile is the on-disk form. Evidence is omitted entirely until the
// upgrade, so "no evidence" is a shape the reader cannot confuse with a zero
// value it happened to write.
type markerFile struct {
	Evidence *core.RunEvidence `json:"evidence,omitempty"`
}

// BeginRun records that a run may be live in this workspace, before the attempt
// is launched (SPEC §9.10).
//
// It returns only once the marker is durable. That ordering is the point: a
// marker still in the page cache when the machine loses power is a workspace
// that reads as free while an agent was starting in it, which is the one
// mistake this precondition exists to prevent. The cost is one fsync per
// attempt, against a launch that is about to spend seconds.
func (p *Provider) BeginRun(key string) error {
	if err := p.writeMarker(key, markerFile{}); err != nil {
		return fmt.Errorf("workspace: begin run marker: %w", err)
	}
	return nil
}

// RecordRun upgrades the marker to carry the run's evidence (SPEC §9.10),
// turning "something may be live here" into a question a later daemon can
// actually ask.
//
// Keyed by the workspace path because that is what a runner has: the sink is
// handed a core.RunSpec, and the adapter never learns the workspace key. A path
// outside this provider's tree is refused rather than resolved — it is not our
// workspace, and creating a marker for it would be writing into someone else's
// state (safety invariant 2, SPEC §6.3).
func (p *Provider) RecordRun(workspacePath string, evidence core.RunEvidence) error {
	key, err := p.keyForPath(workspacePath)
	if err != nil {
		return err
	}
	if err := p.writeMarker(key, markerFile{Evidence: &evidence}); err != nil {
		return fmt.Errorf("workspace: record run marker: %w", err)
	}
	return nil
}

// ClearRun removes the marker, freeing the workspace.
//
// The caller must have confirmed the run is gone. §9.10 admits exactly one route
// to removal — positive evidence of absence — because every other reading
// ("probably finished", "the read failed", "it has been a while") is a guess, and
// the guess that frees a live workspace puts a second agent in a worktree.
// Removing an absent marker is not an error: recovery and the run-time path can
// both reach the same conclusion about the same workspace.
func (p *Provider) ClearRun(key string) error {
	if err := os.Remove(p.markerPath(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("workspace: clear run marker: %w", err)
	}
	// The removal has to outlive a crash for the same reason the creation does:
	// a marker that returns after a power loss re-parks a workspace nothing is
	// running in.
	return syncDir(p.markerDir())
}

// ReadRun reports this workspace's marker (SPEC §9.10).
//
// A marker that exists but cannot be parsed reads as MarkerUnknownLaunch, not as
// an error and never as absent. Its evidence is unusable either way, and the
// difference between the two failing readings is that one parks a human and the
// other hands a possibly-live worktree to a second agent.
func (p *Provider) ReadRun(key string) (Marker, error) {
	path := p.markerPath(key)
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// ENOENT from ReadFile is not proof of absence: a marker that is a
		// symlink to a missing target reads exactly the same, and the entry is
		// still there. Lstat asks about the entry rather than what it resolves
		// to, which is the question being answered — "is this workspace free".
		// Freeing one on a dangling marker is the failure this whole precondition
		// exists to prevent, so anything that is present-but-unreadable is
		// unknown_launch.
		if _, lerr := os.Lstat(path); lerr == nil {
			return Marker{State: MarkerUnknownLaunch}, nil
		} else if !errors.Is(lerr, os.ErrNotExist) {
			return Marker{}, fmt.Errorf("workspace: read run marker: %w", lerr)
		}
		// Lstat only declines to follow the *last* component: an ancestor that
		// does not resolve yields the same ENOENT as a free workspace. So the
		// absence is only trustworthy once the directory it would live in is
		// shown to be a real directory.
		//
		// This one is not a per-workspace verdict. A runs/ that does not resolve
		// makes every workspace in the workflow read as free at once, and it is
		// an environment fault rather than a state a run left behind — so it is
		// an error, "we could not look", and never unknown_launch, which would
		// park each issue with a reason that misdescribes what is wrong.
		return p.absenceOrError()
	case err != nil:
		return Marker{}, fmt.Errorf("workspace: read run marker: %w", err)
	}
	var m markerFile
	if err := json.Unmarshal(raw, &m); err != nil || m.Evidence == nil {
		return Marker{State: MarkerUnknownLaunch}, nil
	}
	return Marker{State: MarkerIdentified, Evidence: *m.Evidence}, nil
}

// The three below are the orchestrator's view of the marker store, keyed by
// issue instead of by workspace key or path.
//
// Recovery has no workspace to read a key off — that is the whole point of it —
// and providers own key and branch naming (SPEC §6.3), so the derivation belongs
// here rather than in a caller that would have to reimplement Key.

// BeginRunMarkerFor is BeginRun for an issue.
func (p *Provider) BeginRunMarkerFor(issue core.Issue) error {
	key, err := markerKey(issue)
	if err != nil {
		return err
	}
	return p.BeginRun(key)
}

// ReadRunMarkerFor is ReadRun for an issue, projected onto the closed core enum
// the orchestrator decides from.
//
// The switch is exhaustive and its default fails closed. A numeric cast between
// two enums with the same member names would compile and mistranslate the moment
// either grew a case — the shim #7 exists for — and here a mistranslation of
// exactly one value hands a possibly-live worktree to a second agent.
func (p *Provider) ReadRunMarkerFor(issue core.Issue) (core.RunMarker, error) {
	key, err := markerKey(issue)
	if err != nil {
		return core.RunMarker{}, err
	}
	m, err := p.ReadRun(key)
	if err != nil {
		return core.RunMarker{}, err
	}
	switch m.State {
	case MarkerAbsent:
		return core.RunMarker{State: core.RunMarkerAbsent}, nil
	case MarkerIdentified:
		return core.RunMarker{State: core.RunMarkerIdentified, Evidence: m.Evidence}, nil
	case MarkerUnknownLaunch:
		return core.RunMarker{State: core.RunMarkerUnknownLaunch}, nil
	default:
		return core.RunMarker{}, fmt.Errorf("%w: run marker state %q has no core projection",
			ErrWorkspaceState, m.State)
	}
}

// ClearRunMarkerFor is ClearRun for an issue.
func (p *Provider) ClearRunMarkerFor(issue core.Issue) error {
	key, err := markerKey(issue)
	if err != nil {
		return err
	}
	return p.ClearRun(key)
}

// markerKey refuses an identifier that names no workspace, rather than letting
// Key sanitize it into one. An empty identifier would otherwise address the
// marker of a workspace called "" — shared by every issue that reached here the
// same way, which is the one marker collision that frees a live workspace.
func markerKey(issue core.Issue) (string, error) {
	if issue.Identifier == "" {
		return "", fmt.Errorf("%w: issue identifier is empty", ErrPathEscape)
	}
	return Key(issue.Identifier), nil
}

// markerDir is the workflow subtree's runs/ directory — a sibling of issues/,
// derived from it so there is one definition of where the subtree is.
func (p *Provider) markerDir() string {
	return filepath.Join(filepath.Dir(p.issuesDir), markerDirName)
}

func (p *Provider) markerPath(key string) string {
	return filepath.Join(p.markerDir(), key+".json")
}

// keyForPath resolves a workspace path to its key, refusing anything that is not
// a direct child of this provider's issues directory.
func (p *Provider) keyForPath(workspacePath string) (string, error) {
	if workspacePath == "" {
		return "", fmt.Errorf("%w: empty workspace path", ErrPathEscape)
	}
	clean := filepath.Clean(workspacePath)
	if filepath.Dir(clean) != filepath.Clean(p.issuesDir) {
		return "", fmt.Errorf("%w: %s is not a workspace of %s", ErrPathEscape, clean, p.issuesDir)
	}
	return filepath.Base(clean), nil
}

// writeMarker replaces the marker atomically: a reader either sees the previous
// state or the new one, never a half-written file. Rename is what buys that, and
// it is also why the upgrade is a whole-file write rather than an append.
func (p *Provider) writeMarker(key string, m markerFile) error {
	if key == "" {
		return fmt.Errorf("%w: empty workspace key", ErrPathEscape)
	}
	dir := p.markerDir()
	// Syncing the marker's own directory is not enough on the attempt that
	// creates it: `runs/` is itself a directory entry in the workflow subtree,
	// and a crash can lose that entry while keeping everything inside it. The
	// marker would then be gone despite BeginRun having returned success — a
	// workspace reading as free with an agent starting in it. Only the creating
	// attempt pays for this.
	_, statErr := os.Lstat(dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := syncDir(filepath.Dir(dir)); err != nil {
			return err
		}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".marker-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	// Sync before rename, not after: rename orders the *directory entry*, not
	// the file's contents. Renaming first can publish a name whose data has not
	// landed, which after a crash is a marker that parses as empty — read as
	// unknown_launch, which parks a workspace that was in fact never launched
	// into.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p.markerPath(key)); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes a directory entry's creation or removal durable. Syncing the
// file is not enough: the entry that names it lives in the directory, and a
// crash can lose the name while keeping the data.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer d.Close()
	return d.Sync()
}

// absenceOrError confirms that a missing marker means a free workspace rather
// than a directory nobody can read.
//
// A runs/ that is absent outright is the ordinary case — no attempt has ever
// been launched under this workflow — and is genuine absence. A runs/ that
// exists but does not resolve to a directory is not: the marker may be there and
// unreachable, and reporting that as free is how a live worktree gets a second
// agent.
func (p *Provider) absenceOrError() (Marker, error) {
	dir := p.markerDir()
	fi, err := os.Stat(dir) // follows symlinks: this is the resolved truth
	switch {
	case err == nil && fi.IsDir():
		return Marker{State: MarkerAbsent}, nil
	case err == nil:
		return Marker{}, fmt.Errorf("workspace: run marker store %s is not a directory", dir)
	case errors.Is(err, os.ErrNotExist):
		// Nothing resolves. Either it was never created, or the entry is there
		// and dangling — and only the second is a reason to distrust absence.
		if _, lerr := os.Lstat(dir); lerr == nil {
			return Marker{}, fmt.Errorf("workspace: run marker store %s exists but does not resolve", dir)
		}
		return Marker{State: MarkerAbsent}, nil
	default:
		return Marker{}, fmt.Errorf("workspace: read run marker store: %w", err)
	}
}
