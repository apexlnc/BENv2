package reviewrun

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Store is where review-run records live between processes.
//
// An interface for remote.Store's reason: the daemon writes files under the
// §10.3 state directory, and a test needs a store that can fail on a chosen
// write so the ordering rules are proven against one that fails where a disk
// would.
type Store interface {
	// Load returns one run's record, or ErrNoRecord when there is none.
	Load(run string) (Record, error)
	// Save replaces it, atomically against a reader in another process.
	Save(r Record) error
	// Delete removes it. Absent is not an error.
	Delete(run string) error
	// Records lists every retained record, for the quiet gate and the startup
	// survey.
	Records() ([]Record, error)
}

// DirStore keeps one file per run under a directory, replaced by rename after
// an fsync (remotews.DirStore's ordering, for its reason).
type DirStore struct{ root string }

// NewDirStore names a directory. It is created on first write, so a daemon
// whose state directory does not exist yet is not a construction failure.
func NewDirStore(root string) *DirStore { return &DirStore{root: root} }

// Root is the directory itself.
func (s *DirStore) Root() string { return s.root }

// Path is where one record lives. The run identity is a derived hex string
// (Subject.RunID), which is checked rather than assumed: the check is what
// keeps a future second speller from writing outside the store.
func (s *DirStore) Path(run string) string { return filepath.Join(s.root, run+".json") }

func (s *DirStore) Load(run string) (Record, error) {
	if err := checkRunID(run); err != nil {
		return Record{}, err
	}
	raw, err := os.ReadFile(s.Path(run))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Record{}, fmt.Errorf("%w: %s", ErrNoRecord, run)
	case err != nil:
		return Record{}, fmt.Errorf("%w: reading %s: %v", ErrRecordState, s.Path(run), err)
	}
	return decodeRecord(raw, run)
}

func (s *DirStore) Save(r Record) error {
	if err := checkRunID(r.Run); err != nil {
		return err
	}
	if err := r.validate(); err != nil {
		return err
	}
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encoding the record for %s: %v", ErrRecordState, r.Run, err)
	}
	return replaceFile(s.Path(r.Run), append(body, '\n'))
}

func (s *DirStore) Delete(run string) error {
	if err := checkRunID(run); err != nil {
		return err
	}
	if err := os.Remove(s.Path(run)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: removing %s: %v", ErrRecordState, s.Path(run), err)
	}
	return nil
}

// Records lists what the directory holds.
//
// An unreadable entry is an error rather than a skip, for remotews.Cycles'
// reason: what this feeds is the gate deciding whether another run may start,
// and a listing that quietly dropped a record would report a live run as
// absent.
func (s *DirStore) Records() ([]Record, error) {
	entries, err := os.ReadDir(s.root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: listing %s: %v", ErrRecordState, s.root, err)
	}
	out := make([]Record, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		r, err := s.Load(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Run < out[j].Run })
	return out, nil
}

// decodeRecord refuses unknown fields and a record naming a different run.
func decodeRecord(raw []byte, run string) (Record, error) {
	var r Record
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Record{}, fmt.Errorf("%w: decoding the record for %s: %v", ErrRecordState, run, err)
	}
	// Version 1 predates named reviewer profiles. Its empty profile and run-id
	// derivation are still exactly the legacy one-argv mode, so upgrading the
	// in-memory version is lossless and lets an interrupted review resume.
	if r.Version == 1 {
		r.Version = RecordVersion
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Record{}, fmt.Errorf("%w: the record for %s carries trailing data", ErrRecordState, run)
	}
	if err := r.validate(); err != nil {
		return Record{}, err
	}
	if r.Run != run {
		return Record{}, fmt.Errorf("%w: %s names run %q", ErrRecordState, run, r.Run)
	}
	return r, nil
}

// checkRunID refuses an identity that is not one safe path component. Derived
// identities are hex with a fixed prefix; checking rather than trusting is what
// keeps a caller that composed one by hand from writing outside the store.
func checkRunID(run string) error {
	if run == "" || run == "." || run == ".." || strings.ContainsAny(run, `/\`) {
		return fmt.Errorf("%w: %q is not a usable review-run identity", ErrRecordState, run)
	}
	return nil
}

func replaceFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // no-op once the rename succeeds
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
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // read-only handle; Sync reported what mattered
	return d.Sync()
}
