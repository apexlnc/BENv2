package remote

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Store is where a Record lives between processes.
//
// An interface because the two callers want different things from it and only
// one of them is a filesystem: the daemon writes one file per claim under the
// §10.3 state directory, and the crash tests need a store that can fail on a
// chosen write. Both are the same three methods, so the ordering rules Journal
// enforces are proven against a store that fails where a disk would.
type Store interface {
	// Load returns the record for a claim cycle. ErrNoRecord when there is
	// none — a fact, and the one that says nothing was ever dispatched.
	Load(claim Claim) (Record, error)
	// Save replaces the record. It MUST be atomic against a concurrent reader
	// in another process: a torn record is worse than none, because none is a
	// verdict and a splice of two is an address.
	Save(r Record) error
	// Delete removes it. Absent is not an error — a repeated disposal after a
	// crash must not fail.
	Delete(claim Claim) error
}

// DirStore keeps one file per claim cycle under a directory, replaced by rename
// after an fsync.
//
// The ordering is internal/state's and internal/workspace/marker.go's, for their
// reason: rename publishes a *directory entry*, not the bytes behind it, so a
// rename before the sync can name a file whose contents have not landed. After a
// crash that reads as an empty or truncated record — which here is a run BEN
// cannot attach to and must park.
type DirStore struct{ root string }

// NewDirStore names a directory. It is created on first write, so a daemon whose
// state directory does not exist yet is not a construction failure.
func NewDirStore(root string) *DirStore { return &DirStore{root: root} }

// Root is the directory itself.
func (s *DirStore) Root() string { return s.root }

// Path is where one claim cycle's record lives.
//
// The filename is derived from Claim.String with every character outside a safe
// set replaced, because a repository name carries a slash and an issue
// identifier is a tracker's to choose. Sanitizing alone would collide — `a/b` and
// `a_b` land on one name — so the readable part is followed by a digest of the
// exact claim, which is what actually distinguishes them.
func (s *DirStore) Path(claim Claim) string {
	return filepath.Join(s.root, claimFilename(claim)+".json")
}

func (s *DirStore) Load(claim Claim) (Record, error) {
	raw, err := os.ReadFile(s.Path(claim))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Record{}, fmt.Errorf("%w: %s", ErrNoRecord, claim)
	case err != nil:
		return Record{}, fmt.Errorf("remote: reading record for %s: %w", claim, err)
	}
	rec, err := DecodeRecord(raw)
	if err != nil {
		return Record{}, err
	}
	// The file's own claim is authoritative over the one asked for. They can
	// only differ through a filename collision the digest was meant to prevent,
	// and answering with somebody else's address is the failure mode this whole
	// package is arranged around.
	if rec.Identity.Claim != claim {
		return Record{}, fmt.Errorf("%w: %s names %s", ErrClaimMismatch, s.Path(claim), rec.Identity.Claim)
	}
	return rec, nil
}

func (s *DirStore) Save(r Record) error {
	body, err := EncodeRecord(r)
	if err != nil {
		return err
	}
	return replaceFile(s.Path(r.Identity.Claim), body)
}

func (s *DirStore) Delete(claim Claim) error {
	err := os.Remove(s.Path(claim))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remote: removing record for %s: %w", claim, err)
	}
	return nil
}

// claimFilename is a readable prefix plus a digest of the exact claim. See Path.
func claimFilename(claim Claim) string {
	var b strings.Builder
	for _, r := range claim.String() {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	readable := b.String()
	if len(readable) > 64 {
		readable = readable[:64]
	}
	return readable + "-" + shortDigest(claim.String())
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
