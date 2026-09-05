// Package state is SPEC §10.3's white-box state directory: the files `ben run`
// writes and `ben status` renders, plus the §9.11 transition log that outlives
// the process which wrote it.
//
// Everything here is shaped by one fact — the reader is a *different process*,
// running while the daemon runs (SPEC §11) — and by the run marker's lesson
// (internal/workspace/marker.go): a partial write must never be readable as
// truth, and absence is the dangerous verdict. So whole-file state is replaced
// by rename after an fsync and never edited in place; the log is appended one
// complete record per write; and a read that could not ask reports that rather
// than reporting nothing.
//
// None of this is a durable orchestrator database — SPEC §9.10 says there is
// none, by design. The tracker and git remain the source of truth. Every file
// here is either forensics or one of §9.10 step 6's two documented local-only
// facts, and recovery is written to work when the whole directory is missing.
//
// # Layout
//
//	$XDG_STATE_HOME/ben/<workflow key>/
//	    runs.json           the daemon's heartbeat and its current run records
//	    transitions.jsonl   the §9.11 append-only transition log
//	    attempts.jsonl      one outcome record per finished attempt (#60)
//	    transcripts/        per-run raw agent streams (harness.DirTranscripts)
//
// §10.3's fourth item, continuation tokens, is a *field of* a run record rather
// than a file: it belongs to exactly one run, and giving it a store of its own
// would be a second place for the same fact to go stale.
package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Dir is one workflow's state directory.
type Dir struct{ root string }

// At names a state directory by path, for tests and for an operator pointing at
// one explicitly. For is what a daemon resolving its own uses.
func At(root string) Dir { return Dir{root: root} }

// For resolves SPEC §10.3's location for a workflow key.
func For(workflowKey string) Dir {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return Dir{root: filepath.Join(xdg, "ben", workflowKey)}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// No home and no XDG_STATE_HOME: put it under the working directory
		// rather than refusing. Everything in here is forensics or a documented
		// degradation, so refusing to start over the *location* of the forensics
		// would trade a working daemon for a tidy one.
		return Dir{root: filepath.Join(".ben-state", workflowKey)}
	}
	return Dir{root: filepath.Join(home, ".local", "state", "ben", workflowKey)}
}

// Root is the directory itself.
func (d Dir) Root() string { return d.root }

// Transcripts is where the harness writes per-run raw agent streams. The
// assembly points harness.DirTranscripts here; this package never writes it.
func (d Dir) Transcripts() string { return filepath.Join(d.root, "transcripts") }

// RunsPath is the whole-file run-record store.
func (d Dir) RunsPath() string { return filepath.Join(d.root, "runs.json") }

// TransitionsPath is the §9.11 append-only log.
func (d Dir) TransitionsPath() string { return filepath.Join(d.root, "transitions.jsonl") }

// AttemptsPath is the append-only attempt-outcome log (#60).
func (d Dir) AttemptsPath() string { return filepath.Join(d.root, "attempts.jsonl") }

// Substrate is where a v2 execution backend keeps the durable addresses a
// restart needs — which sandbox a claim acquired, and which backend run a
// dispatch resolved to (#194, #46). This package never writes it either; the
// assembly points internal/airlock's store here.
//
// Beside the transcripts rather than inside them, because the two have opposite
// lifetimes: a transcript is forensics that outlives the claim, and these are
// addresses that must be removed the moment the workspace they name is
// confirmed gone.
func (d Dir) Substrate() string { return filepath.Join(d.root, "substrate") }

// The four subtrees the v2 dispatch path keeps under Substrate (#205). Named
// here rather than composed at the assembly for the reason every other path on
// this type is: the §10.3 layout is this package's, and a second speller of
// `<state>/substrate/journal` is how a rolling upgrade comes to read one
// directory and write another.
//
// They are separate directories rather than one because they are retired at
// different moments and by different rules. A run journal dies when its run's
// termination is confirmed; a workspace-cycle record outlives every run in the
// cycle; the sandbox addresses are the backend's own and are removed only on a
// confirmed deletion; and the mirror is not a cache at all — it holds the one
// fact a restart cannot reconstruct from the outside world (internal/mirror).

// SubstrateJournals is where a remote run's durable dispatch record lives
// (remote.Store), with its lifecycle-hook records beneath it.
func (d Dir) SubstrateJournals() string { return filepath.Join(d.Substrate(), "journal") }

// SubstrateCycles is where live workspace-cycle records and their independently
// addressed ended-cycle disposal obligations live (remotews.Store).
func (d Dir) SubstrateCycles() string { return filepath.Join(d.Substrate(), "cycles") }

// SubstrateMirror is the root of the daemon-side bare stores publication
// evidence is read from (#193, internal/mirror). Under the substrate tree
// because it exists only for a claim that ran somewhere else — and never
// reachable from a run, which is what makes its contents evidence.
func (d Dir) SubstrateMirror() string { return filepath.Join(d.Substrate(), "mirror") }

// Reviews is where the #204 review controller's durable execution records live
// (reviewrun.Store): the derived run identity, the committed event cursor, and
// the validated verdict a resumed publication is completed from.
//
// A sibling of Substrate rather than a subtree of it, deliberately. A review
// runs on whichever substrate the workflow declares — including the local one,
// where there is no `substrate/` tree at all — and filing its record under a
// directory that exists only for the remote case would put the local path's
// durable state somewhere its own name says it is not.
func (d Dir) Reviews() string { return filepath.Join(d.root, "reviews") }

// Prepare creates the directory and its transcript subdirectory.
//
// 0700 throughout: a run record names branches and issues, a transcript is the
// agent's whole raw stream, and neither is a thing to leave world-readable on a
// shared host.
func (d Dir) Prepare() error {
	if err := os.MkdirAll(d.Transcripts(), 0o700); err != nil {
		return fmt.Errorf("state: preparing %s: %w", d.root, err)
	}
	return nil
}

// ErrNoState reports that a state file is not there at all: no daemon has
// written this directory, or it belongs to a different workflow key.
//
// It is a distinct error rather than a zero value for the reason §9.10 states
// about every other absence in BEN — "the daemon is running nothing" and "there
// is no daemon here" are different answers, and a reader that cannot tell them
// apart reports the first when it means the second.
var ErrNoState = errors.New("state: no state file")

// atomicWrite replaces a whole file so that a concurrent reader sees either the
// previous contents or the new ones, never a splice of both.
//
// The ordering is the run marker's, for the run marker's reason
// (internal/workspace/marker.go): rename publishes a *directory entry*, not the
// bytes behind it, so a rename before the sync can name a file whose contents
// have not landed. After a crash that reads as a truncated or empty state file —
// which, for `ben status`, is a daemon reported as doing nothing.
func atomicWrite(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	if _, err := tmp.Write(body); err != nil {
		tmp.Close() //nolint:errcheck // the write error is the one that matters
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck // as above
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp is 0600; the file is replaced in place, so the mode has to be
	// set on the temporary rather than inherited from what it replaces.
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir makes a directory entry's creation durable. Syncing the file is not
// enough: the entry that names it lives in the directory, and a crash can lose
// the name while keeping the data.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer d.Close() //nolint:errcheck // read-only handle; Sync reported what mattered
	return d.Sync()
}
