package remotews

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// CycleVersion is the current Cycle.Version. An older reader refuses a newer
// record loudly rather than acting on half of it, for remote.Record's reason:
// this file is an address, and a misread address dispatches into the wrong
// sandbox.
const (
	CycleVersion       = 2
	legacyCycleVersion = 1
)

const (
	cyclePending = "pending"
	cyclePinned  = "pinned"
)

const (
	cycleDisposalVersion   = 1
	dispositionUnconfirmed = ""
	dispositionRetained    = "retained"
	dispositionDeleted     = "deleted"
)

// Cycle is one issue's durable workspace-cycle record: which sandbox address the
// standing approval selects, and which assignment epoch the verification base
// currently belongs to.
//
// One record rather than two, because the two facts are read together at every
// decision and a reader that saw one half updated would attach a new cycle's
// sandbox to an old cycle's base. It is replaced whole by rename for the reason
// internal/state replaces its files that way: a torn record is worse than none,
// since none is a verdict.
type Cycle struct {
	Version int `json:"version"`
	// Issue is the tracker identifier; Key is the sanitized workspace key that
	// names the branch and this record (SPEC §6.3).
	Issue string `json:"issue"`
	Key   string `json:"key"`
	// Repository is the credential-free identity of the repository the claim
	// lives in. Compared on every read: a state directory pointed at a second
	// repository must refuse rather than order one repository's work against
	// another's sandbox.
	Repository string `json:"repository"`
	Branch     string `json:"branch"`

	// Approval is the workspace-cycle anchor: the tracker-native id of the
	// standing approval-label event (SPEC §6.7). It selects the sandbox.
	Approval int64 `json:"approval"`
	// Superseded is the pre-#266 operator breadcrumb. New writes leave it empty:
	// the replaced cycle's full identity and disposal state now live in their own
	// CycleDisposal record, whose lifetime is independent of this cycle.
	Superseded int64 `json:"superseded,omitempty"`
	// CycleBaseSHA is the trusted base the sandbox was *acquired* against, and it
	// does not move for the life of the cycle.
	//
	// It is not the verification base, and the difference is the whole of the
	// two-clock design. remote.Identity carries this one, so a reassignment
	// inside one approval reattaches the same sandbox (a backend refuses an
	// acquire whose recorded branch, base or profile moved). BaseSHA below is
	// what §9.7 measures a publication against, and that one moves with the
	// assignment.
	CycleBaseSHA string `json:"cycle_base_sha,omitempty"`

	// The §6.2 claim-base state, per assignment epoch. Identical in meaning to
	// internal/workspace's record; only the base's *source* differs, since here
	// it is pinned daemon-side from the canonical remote (#193).
	State                string `json:"state"`
	Epoch                int64  `json:"epoch"`
	BaseSHA              string `json:"base_sha,omitempty"`
	TargetBranch         string `json:"target_branch,omitempty"`
	OutgoingEpoch        int64  `json:"outgoing_epoch,omitempty"`
	OutgoingBaseSHA      string `json:"outgoing_base_sha,omitempty"`
	OutgoingTargetBranch string `json:"outgoing_target_branch,omitempty"`

	// Attempt is the attempt number of the most recent prepare. It names the run
	// a verification speaks for (core.RemoteRunRef.Run), which is why it is
	// durable rather than held in memory: a verdict earned by attempt 2 must not
	// settle attempt 3, across a restart as well as within one.
	Attempt int `json:"attempt,omitempty"`
}

// CycleDisposal is one durable obligation to apply the revoked policy to a
// workspace cycle that a reapproval replaced while BEN was not observing it.
//
// It is deliberately a record of its own, keyed by the replaced cycle's opaque
// address. The replacement may be deleted, revised, or replaced again without
// changing this address or this record's lifetime. Cycle contains the full old
// backend and pin identities; ReplacementApproval is the latest new cycle BEN
// intended to publish before this one may be touched. It may advance while the
// old cycle is still live, because an interrupted replacement can itself be
// overtaken by another approval.
type CycleDisposal struct {
	Version             int    `json:"version"`
	Cycle               Cycle  `json:"cycle"`
	ReplacementApproval int64  `json:"replacement_approval"`
	Disposition         string `json:"disposition,omitempty"`
}

// Address is the durable record's identity and the backend cycle it disposes.
func (d CycleDisposal) Address() string { return d.Cycle.Address() }

// Ref is the orchestrator's opaque view of an outstanding obligation.
func (d CycleDisposal) Ref() core.WorkspaceRef {
	return core.WorkspaceRef{Key: d.Cycle.Key, Path: d.Address(), Identifier: d.Cycle.Issue}
}

func (d CycleDisposal) validate() error {
	if err := d.Cycle.validate(); err != nil {
		return err
	}
	switch {
	case d.Version != cycleDisposalVersion:
		return fmt.Errorf("%w: unsupported cycle-disposal version %d", ErrCycleState, d.Version)
	case d.Cycle.State != cyclePinned:
		return fmt.Errorf("%w: cycle disposal carries non-authorizing state %q", ErrCycleState, d.Cycle.State)
	case d.Cycle.Superseded != 0:
		return fmt.Errorf("%w: cycle disposal carries a nested superseded breadcrumb", ErrCycleState)
	case d.ReplacementApproval <= 0 || d.ReplacementApproval == d.Cycle.Approval:
		return fmt.Errorf("%w: cycle disposal does not name a distinct positive replacement approval", ErrCycleState)
	case d.Disposition != dispositionUnconfirmed && d.Disposition != dispositionRetained && d.Disposition != dispositionDeleted:
		return fmt.Errorf("%w: unknown cycle-disposal disposition %q", ErrCycleState, d.Disposition)
	}
	return nil
}

// Claim is the backend address this cycle selects. The epoch is the *approval*
// anchor, never the assignment — see CycleBaseSHA.
func (c Cycle) Claim() remote.Claim {
	return remote.Claim{Repository: c.Repository, Issue: c.Issue, Epoch: c.Approval}
}

// ClaimRef is the daemon-side pin's address, keyed by the assignment epoch.
func (c Cycle) ClaimRef() core.RemoteClaimRef {
	return core.RemoteClaimRef{Issue: c.Issue, Key: c.Key, Epoch: c.Epoch}
}

// ClaimBase projects the record onto the closed provider state the orchestrator
// reads (core.ClaimBase).
func (c Cycle) ClaimBase() core.ClaimBase {
	switch c.State {
	case cyclePending:
		return core.ClaimBase{
			State: core.ClaimBasePending, Epoch: c.Epoch,
			TargetBranch: c.TargetBranch, OutgoingEpoch: c.OutgoingEpoch,
			OutgoingBaseSHA: c.OutgoingBaseSHA, OutgoingTargetBranch: c.OutgoingTargetBranch,
		}
	case cyclePinned:
		return core.ClaimBase{State: core.ClaimBasePinned, Epoch: c.Epoch, BaseSHA: c.BaseSHA, TargetBranch: c.TargetBranch}
	}
	return core.ClaimBase{}
}

// Address is the opaque name of this workspace cycle, and it is what fills
// core.Workspace.Path.
//
// **It is not a filesystem path, and no consumer may treat it as one.** A remote
// workspace identity carries no path by construction (remote.Identity): the
// sandbox's working directory belongs to the worker profile, and BEN does not
// know it. What core.Workspace.Path is actually used for here is what it can
// honestly be — a stable, non-empty, per-cycle name that says a workspace exists
// (Record.hasWorkspace), distinguishes one cycle's from another's, and reads
// unmistakably in a log line. It is deliberately kept out of the child
// environment for the same reason (harness.RemoteEnvOmitted).
func (c Cycle) Address() string {
	return fmt.Sprintf("remote:%s#%s@%d", c.Repository, c.Key, c.Approval)
}

// Workspace projects the record onto the orchestrator's workspace value.
// createdNow is this prepare's answer, never the record's: a record cannot know
// whether the acquire that just ran allocated a sandbox or resumed one.
func (c Cycle) Workspace(createdNow bool) core.Workspace {
	return core.Workspace{
		// Deliberately no SharedGitDir and no PrivateDir. Both are directories on
		// the daemon's disk in v1 (SPEC §6.2), and stating one here would hand an
		// adapter a path that does not exist wherever the run happens.
		WorkspacePaths: core.WorkspacePaths{Path: c.Address()},
		Key:            c.Key,
		Branch:         c.Branch,
		ClaimEpoch:     c.Epoch,
		BaseSHA:        c.BaseSHA,
		TargetBranch:   c.TargetBranch,
		PriorWork:      c.CycleBaseSHA != "" && c.BaseSHA != "" && c.CycleBaseSHA != c.BaseSHA,
		CreatedNow:     createdNow,
	}
}

// RunRef names one verification of this cycle's most recent attempt (#193).
//
// Run is derived rather than taken from the backend, and from facts that are
// durable: the key, the assignment epoch and the attempt. A backend run id would
// be the more obvious choice and is the wrong one — it is deleted with the
// journal once the run's termination is confirmed, which happens *before* the
// §9.7 check runs.
func (c Cycle) RunRef(verification string) core.RemoteRunRef {
	return core.RemoteRunRef{
		Claim:        c.ClaimRef(),
		Run:          fmt.Sprintf("%s/%d/%d", c.Key, c.Epoch, c.Attempt),
		Verification: verification,
	}
}

// Ref is the §9.10 step 5 sweep's view of a cycle.
func (c Cycle) Ref() core.WorkspaceRef {
	return core.WorkspaceRef{Key: c.Key, Path: c.Address(), Identifier: c.Issue}
}

// validate refuses a record this package could not have written. Every clause is
// a shape that would otherwise be acted on: a version nobody understands, a
// pinned record with no base, a pending one carrying one.
func (c Cycle) validate() error {
	legacy := c.Version == legacyCycleVersion
	switch {
	case c.Version != CycleVersion && !legacy:
		return fmt.Errorf("%w: unsupported version %d", ErrCycleState, c.Version)
	case legacy && (c.TargetBranch != "" || c.OutgoingTargetBranch != ""):
		return fmt.Errorf("%w: legacy record carries target-branch fields", ErrCycleState)
	case c.Issue == "" || c.Key == "" || c.Repository == "" || c.Branch == "":
		return fmt.Errorf("%w: record is not fully named (%+v)", ErrCycleState, c)
	case c.Approval <= 0:
		return fmt.Errorf("%w: workspace cycle has no positive approval anchor", ErrCycleState)
	case c.Epoch <= 0:
		return fmt.Errorf("%w: record carries no positive assignment epoch", ErrCycleState)
	case c.State == cyclePending && c.BaseSHA != "":
		return fmt.Errorf("%w: pending record carries a pinned base", ErrCycleState)
	case c.State == cyclePending && c.OutgoingEpoch != 0 && c.OutgoingBaseSHA == "":
		return fmt.Errorf("%w: pending record names an outgoing epoch with no base", ErrCycleState)
	case c.State == cyclePending && c.OutgoingTargetBranch != "" && c.OutgoingBaseSHA == "":
		return fmt.Errorf("%w: pending record names an outgoing target with no base", ErrCycleState)
	case c.State == cyclePinned && c.BaseSHA == "":
		return fmt.Errorf("%w: pinned record carries no base", ErrCycleState)
	case c.State == cyclePinned && !legacy && c.TargetBranch == "":
		return fmt.Errorf("%w: pinned record carries no target branch", ErrCycleState)
	case c.State == cyclePinned && (c.OutgoingEpoch != 0 || c.OutgoingBaseSHA != "" || c.OutgoingTargetBranch != ""):
		return fmt.Errorf("%w: pinned record carries transitional outgoing state", ErrCycleState)
	case c.State != cyclePending && c.State != cyclePinned:
		return fmt.Errorf("%w: unknown state %q", ErrCycleState, c.State)
	}
	return nil
}

// Store is where cycle records live between processes.
//
// An interface for remote.Store's reason: the daemon writes files under the
// §10.3 state directory, and a test needs a store that can fail on a chosen
// write so the ordering rules are proven against one that fails where a disk
// would.
type Store interface {
	// WithCycle serializes a read-modify-write transaction for one workspace key.
	// Prepare, recovery and disposal all mutate that record; atomic file replace
	// prevents torn reads, while this prevents two complete snapshots from
	// overwriting each other's updates.
	WithCycle(key string, fn func() error) error
	// LoadCycle returns one issue's record, or ErrNoCycle when there is none.
	LoadCycle(key string) (Cycle, error)
	// SaveCycle replaces it, atomically against a reader in another process.
	SaveCycle(c Cycle) error
	// DeleteCycle removes it. Absent is not an error.
	DeleteCycle(key string) error
	// Cycles lists every retained record, for the §9.10 step 5 sweep.
	Cycles() ([]Cycle, error)
	// LoadCycleDisposal returns the independent obligation at an opaque cycle
	// address, or ErrNoCycleDisposal when there is none.
	LoadCycleDisposal(address string) (CycleDisposal, error)
	// SaveCycleDisposal durably creates an obligation. Repeating the identical
	// create is a no-op; while it is unconfirmed, a replay may advance its
	// replacement approval. It never resets a confirmed disposition.
	SaveCycleDisposal(disposal CycleDisposal) error
	// SetCycleDisposalDisposition records the backend's confirmed answer before
	// any pin or obligation record is retired.
	SetCycleDisposalDisposition(address, disposition string) error
	// DeleteCycleDisposal removes one fully discharged obligation. gone reports
	// that the name is absent even when syncing its parent fails; that outcome is
	// safe because a crash either preserves the absence or restores the confirmed
	// record for recovery to enumerate again.
	DeleteCycleDisposal(address string) (gone bool, err error)
	// CycleDisposals lists every independent obligation record, including a
	// deletion whose backend confirmation landed but whose local cleanup did not.
	CycleDisposals() ([]CycleDisposal, error)
}

// DirStore keeps one file per issue under a directory, replaced by rename after
// an fsync (remote.DirStore's ordering, for its reason).
type DirStore struct{ root string }

type keyedLock struct {
	mu    sync.Mutex
	users int
}

type keyedLocks struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

var cycleRecordLocks keyedLocks

func (l *keyedLocks) lock(key string) func() {
	l.mu.Lock()
	if l.locks == nil {
		l.locks = map[string]*keyedLock{}
	}
	entry := l.locks[key]
	if entry == nil {
		entry = &keyedLock{}
		l.locks[key] = entry
	}
	entry.users++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(l.locks, key)
		}
		l.mu.Unlock()
	}
}

// NewDirStore names a directory. It is created on first write, so a daemon whose
// state directory does not exist yet is not a construction failure.
func NewDirStore(root string) *DirStore { return &DirStore{root: root} }

// Root is the directory itself.
func (s *DirStore) Root() string { return s.root }

// Path is where one cycle's record lives. The key is already sanitized to a
// single path component (SPEC §6.3), which is checked rather than assumed.
func (s *DirStore) Path(key string) string { return filepath.Join(s.root, key+".json") }

// DisposalPath is where one independently-lived end-of-cycle obligation lives.
// The full address remains inside the record and is checked on every read; its
// digest is only a bounded, path-safe filename.
func (s *DirStore) DisposalPath(address string) string {
	return filepath.Join(s.root, "ended", fmt.Sprintf("%x.json", sha256.Sum256([]byte(address))))
}

func (s *DirStore) WithCycle(key string, fn func() error) error {
	if err := checkKey(key); err != nil {
		return err
	}
	unlock := cycleRecordLocks.lock(s.Path(key))
	defer unlock()
	return fn()
}

func (s *DirStore) LoadCycle(key string) (Cycle, error) {
	if err := checkKey(key); err != nil {
		return Cycle{}, err
	}
	raw, err := os.ReadFile(s.Path(key))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return Cycle{}, fmt.Errorf("%w: %s", ErrNoCycle, key)
	case err != nil:
		return Cycle{}, fmt.Errorf("%w: reading %s: %v", ErrCycleState, s.Path(key), err)
	}
	return decodeCycle(raw, key)
}

func (s *DirStore) SaveCycle(c Cycle) error {
	if err := checkKey(c.Key); err != nil {
		return err
	}
	if err := c.validate(); err != nil {
		return err
	}
	body, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encoding the record for %s: %v", ErrCycleState, c.Key, err)
	}
	return replaceFile(s.Path(c.Key), append(body, '\n'))
}

func (s *DirStore) DeleteCycle(key string) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if err := os.Remove(s.Path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: removing %s: %v", ErrCycleState, s.Path(key), err)
	}
	return nil
}

// Cycles lists what the directory holds.
//
// An unreadable entry is an error rather than a skip. The sweep this feeds
// decides what to *dispose*, and a listing that quietly dropped a record would
// report a live workspace cycle as absent.
func (s *DirStore) Cycles() ([]Cycle, error) {
	entries, err := os.ReadDir(s.root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: listing %s: %v", ErrCycleState, s.root, err)
	}
	out := make([]Cycle, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		c, err := s.LoadCycle(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

func (s *DirStore) LoadCycleDisposal(address string) (CycleDisposal, error) {
	if address == "" {
		return CycleDisposal{}, fmt.Errorf("%w: the address is empty", ErrCycleState)
	}
	raw, err := os.ReadFile(s.DisposalPath(address))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return CycleDisposal{}, fmt.Errorf("%w: %s", ErrNoCycleDisposal, address)
	case err != nil:
		return CycleDisposal{}, fmt.Errorf("%w: reading disposal %s: %v", ErrCycleState, address, err)
	}
	d, err := decodeCycleDisposal(raw)
	if err != nil {
		return CycleDisposal{}, err
	}
	if d.Address() != address {
		return CycleDisposal{}, fmt.Errorf("%w: disposal %s names address %q", ErrCycleState, address, d.Address())
	}
	return d, nil
}

func (s *DirStore) SaveCycleDisposal(d CycleDisposal) error {
	if err := d.validate(); err != nil {
		return err
	}
	if current, err := s.LoadCycleDisposal(d.Address()); err == nil {
		// A replay of the create must never turn a confirmed record back into
		// pending. The old cycle is the obligation's identity; its replacement
		// approval may advance only while that obligation is still unconfirmed.
		// This is the interrupted A -> B, then reapproved C case: A remains live,
		// so C must be able to supersede the recovery intent for B before C's live
		// record is published.
		d.Disposition = current.Disposition
		switch {
		case current == d:
			// Re-sync the directory before acknowledging an identical replay: the
			// previous rename may be visible even when that final sync returned an
			// error, and visibility alone is not a durable obligation.
			return syncDir(filepath.Dir(s.DisposalPath(d.Address())))
		case current.Cycle != d.Cycle || current.Disposition != dispositionUnconfirmed:
			return fmt.Errorf("%w: cycle-disposal address %s already names another obligation", ErrCycleState, d.Address())
		}
		body, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return fmt.Errorf("%w: encoding disposal %s: %v", ErrCycleState, d.Address(), err)
		}
		return replaceFile(s.DisposalPath(d.Address()), append(body, '\n'))
	} else if !errors.Is(err, ErrNoCycleDisposal) {
		return err
	}
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encoding disposal %s: %v", ErrCycleState, d.Address(), err)
	}
	return replaceFile(s.DisposalPath(d.Address()), append(body, '\n'))
}

func (s *DirStore) SetCycleDisposalDisposition(address, disposition string) error {
	if disposition != dispositionRetained && disposition != dispositionDeleted {
		return fmt.Errorf("%w: cannot confirm cycle-disposal disposition %q", ErrCycleState, disposition)
	}
	d, err := s.LoadCycleDisposal(address)
	if err != nil {
		return err
	}
	if d.Disposition == disposition {
		// Finish an ambiguous prior replace: the new value can be visible even
		// when the directory sync that makes it durable returned an error.
		return syncDir(filepath.Dir(s.DisposalPath(address)))
	}
	if d.Disposition != dispositionUnconfirmed {
		return fmt.Errorf("%w: disposal %s is already confirmed %q, not %q",
			ErrCycleState, address, d.Disposition, disposition)
	}
	d.Disposition = disposition
	body, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: encoding disposal %s: %v", ErrCycleState, address, err)
	}
	return replaceFile(s.DisposalPath(address), append(body, '\n'))
}

func (s *DirStore) DeleteCycleDisposal(address string) (bool, error) {
	if address == "" {
		return false, fmt.Errorf("%w: the address is empty", ErrCycleState)
	}
	if err := os.Remove(s.DisposalPath(address)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("%w: removing disposal %s: %v", ErrCycleState, address, err)
	}
	err := syncDir(filepath.Dir(s.DisposalPath(address)))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	return true, err
}

func (s *DirStore) CycleDisposals() ([]CycleDisposal, error) {
	root := filepath.Join(s.root, "ended")
	entries, err := os.ReadDir(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("%w: listing %s: %v", ErrCycleState, root, err)
	}
	out := make([]CycleDisposal, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			return nil, fmt.Errorf("%w: reading %s: %v", ErrCycleState, filepath.Join(root, name), err)
		}
		d, err := decodeCycleDisposal(raw)
		if err != nil {
			return nil, err
		}
		if filepath.Base(s.DisposalPath(d.Address())) != name {
			return nil, fmt.Errorf("%w: disposal file %s does not match address %s", ErrCycleState, name, d.Address())
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address() < out[j].Address() })
	return out, nil
}

// decodeCycle refuses unknown fields and a record naming a different key.
//
// Strict, unlike remote.Record's rolling-upgrade decode, because the two are
// read by different things: that one is read by a possibly-older binary and this
// one only by the strategy that wrote it, in the same state directory.
func decodeCycle(raw []byte, key string) (Cycle, error) {
	var c Cycle
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Cycle{}, fmt.Errorf("%w: decoding the record for %s: %v", ErrCycleState, key, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return Cycle{}, fmt.Errorf("%w: the record for %s carries trailing data", ErrCycleState, key)
	}
	if err := c.validate(); err != nil {
		return Cycle{}, err
	}
	if c.Key != key {
		return Cycle{}, fmt.Errorf("%w: %s names key %q", ErrCycleState, key, c.Key)
	}
	return c, nil
}

func decodeCycleDisposal(raw []byte) (CycleDisposal, error) {
	var d CycleDisposal
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return CycleDisposal{}, fmt.Errorf("%w: decoding a cycle-disposal record: %v", ErrCycleState, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return CycleDisposal{}, fmt.Errorf("%w: a cycle-disposal record carries trailing data", ErrCycleState)
	}
	if err := d.validate(); err != nil {
		return CycleDisposal{}, err
	}
	return d, nil
}

// checkKey refuses a key that is not one safe path component. The key comes from
// workspace.Key, which sanitizes towards exactly this; checking rather than
// trusting is what keeps a future second speller from writing outside the store.
func checkKey(key string) error {
	if key == "" || key == "." || key == ".." || strings.ContainsAny(key, `/\`) {
		return fmt.Errorf("%w: %q is not a usable workspace key", ErrCycleState, key)
	}
	return nil
}

func replaceFile(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := mkdirAllSynced(dir); err != nil {
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
	return syncDir(dir)
}

// mkdirAllSynced makes newly created path components durable before a file
// inside the deepest one can become the only copy of an obligation. Syncing the
// child after its file rename is not enough: without the parent sync, a crash may
// lose the child directory entry and the complete file with it.
func mkdirAllSynced(dir string) error {
	dir = filepath.Clean(dir)
	var missing []string

walk:
	for cursor := dir; ; cursor = filepath.Dir(cursor) {
		info, err := os.Stat(cursor)
		switch {
		case err == nil:
			if !info.IsDir() {
				return fmt.Errorf("%s is not a directory", cursor)
			}
			break walk
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		missing = append(missing, cursor)
		if parent := filepath.Dir(cursor); parent == cursor {
			return fmt.Errorf("no existing ancestor for %s", dir)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for i := len(missing) - 1; i >= 0; i-- {
		info, err := os.Stat(missing[i])
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", missing[i])
		}
		if err := syncDir(filepath.Dir(missing[i])); err != nil {
			return err
		}
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // read-only handle; Sync reported what mattered
	return d.Sync()
}
