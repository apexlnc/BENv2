package airlock

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The two durable record kinds Airlock owns and #192's journal does not.
//
// remote.Record already carries BEN's run identity, the sandbox id it was bound
// to, and the event cursor — but it is written by remote.Journal at *dispatch*
// time, and both of the facts below have to exist before that. A sandbox record
// must exist before the claim's first acquire, because remote.WorkspaceBackend
// .Attach is handed a Claim alone and Airlock has no list-by-label route: a
// daemon with no record could only re-derive a sandbox by creating one. A run
// start record must exist before Start is attempted, because the timestamp is
// what fences replay of an unanswered request; Airlock's idempotency key has a
// bounded 24-hour life while the run id added to that record is permanent.
//
// Both write-ahead records also carry the canonical endpoint, non-secret
// credential-source binding and runtime principal binding. They land before
// the request whose response would otherwise be their only evidence. That is
// the same ordering remote.Journal states — identity before the act — applied
// to the half of the identity Airlock owns.

// SandboxRecordVersion is the current SandboxRecord.Version.
//
// 5 added Limits (#284). The bump is the rollback guard: a daemon that
// understands 4 refuses a record it would otherwise load while silently
// ignoring the envelope, and so plan a prompt inline that its sandbox refuses.
// A version-4 record is re-stamped 5 only when it gains the field
// (Workspaces.learnLimits); one that never does stays readable by either.
const SandboxRecordVersion = 5

// SubstrateBinding is the non-secret identity of the Airlock scope durable
// state was written against. BaseURL is the canonical endpoint; the credential
// fields are core.SourceDescriptor.Binding projected into JSON-friendly data.
// The dynamic PrincipalBinding beside it covers what an opaque source
// definition cannot. A bearer token never crosses either boundary.
type SubstrateBinding struct {
	BaseURL              string `json:"base_url"`
	CredentialKind       string `json:"credential_kind"`
	CredentialBindingKey string `json:"credential_binding_key"`
}

func (b SubstrateBinding) complete() bool {
	return b.BaseURL != "" && b.CredentialKind != "" && b.CredentialBindingKey != ""
}

// SandboxRecord is what a later process needs in order to address the same
// sandbox without creating a second one.
//
// Nothing in it is a secret and nothing in it is authority. The tracker and git
// remain the source of truth (SPEC §9.10); this is an address, and the reason a
// wrong one is worse than a missing one is that a missing record parks a claim
// while a wrong one dispatches into somebody else's sandbox.
type SandboxRecord struct {
	Version int          `json:"version"`
	Claim   remote.Claim `json:"claim"`
	// Substrate binds this address and its idempotency key to the endpoint and
	// credential identity that created them. Airlock scopes a key by tenant and
	// subject, so replay under a different binding would create a second object.
	Substrate SubstrateBinding `json:"substrate"`
	// PrincipalBinding is the non-secret runtime fence for the credential that
	// was actually used to create the sandbox. A source with a declared stable
	// principal binds that identity; an opaque source binds the full digest of
	// its concrete token. It is required only while SandboxID is unknown and a
	// keyed create could still be replayed.
	PrincipalBinding string `json:"principal_binding,omitempty"`
	// Branch and BaseSHA are BEN's own facts about the claim, carried so that a
	// reattach can prove the recorded sandbox belongs to *this* claim cycle's
	// publication target and verification base rather than to a previous one.
	Branch  string `json:"branch"`
	BaseSHA string `json:"base_sha"`
	// Profile is the approved profile the sandbox was requested from.
	Profile string `json:"profile"`
	// Key is the createSandbox idempotency key. Recomputable from the four
	// fields above (sandboxKey) and stored anyway: it is what an operator greps
	// for in an Airlock audit trail, and a stored copy is what makes a key
	// derivation change visible as a mismatch instead of as a second sandbox.
	Key string `json:"idempotency_key"`
	// CreateAttemptedAt starts the only interval in which an unanswered create
	// may be replayed safely. It lands in the same write-ahead record as Key,
	// before the first request; a zero value is an old ambiguous record and is
	// therefore never replayed.
	CreateAttemptedAt time.Time `json:"create_attempted_at,omitempty"`
	// SandboxID and ProfileRevision are Airlock's two opaque answers, written
	// before the workspace is used for anything.
	SandboxID       string `json:"sandbox_id,omitempty"`
	ProfileRevision string `json:"profile_revision,omitempty"`
	// Owner is the principal Airlock recorded at creation. Compared on every
	// reattach: a record whose sandbox is now owned by somebody else is a
	// cross-tenant or cross-subject collision, and acting on it is the one
	// outcome the ownership check exists to prevent.
	Owner Principal `json:"owner"`
	// Limits is the stdin envelope of the revision this sandbox is pinned to,
	// recorded the first time a profile read reports that exact revision
	// (Workspaces.learnLimits). It lives on the sandbox record and not on the
	// substrate because a profile rolls forward while every existing sandbox
	// stays on its pin, and a run in a sandbox is judged by the pin's limits:
	// planning a prompt against the current revision's envelope is how a
	// rollout strands the reviews already in flight (#284). Nil is unknown —
	// the current revision is not the pinned one, or the read has not
	// happened yet — and a prompt then travels the way it always did, inline,
	// with the backend as the judge.
	Limits *ProfileLimits `json:"limits,omitempty"`
}

// SandboxClaim is one sandbox-record directory entry discovered for startup
// reconciliation. Err belongs to this entry alone: a corrupt record must be
// reported without hiding every healthy retained claim beside it. Record is
// the non-secret basename an operator can use to locate an entry whose Claim
// could not be decoded.
type SandboxClaim struct {
	Claim  remote.Claim
	Record string
	Err    error
}

// Identity projects the record onto the boundary type. Incomplete until Airlock
// has answered, which remote.Identity.Complete is what reports.
func (r SandboxRecord) Identity() remote.Identity {
	return remote.Identity{
		Claim:           r.Claim,
		Branch:          r.Branch,
		BaseSHA:         r.BaseSHA,
		SandboxID:       r.SandboxID,
		ProfileRevision: r.ProfileRevision,
	}
}

// Store is where this package's two records live between processes.
//
// An interface for remote.Store's reason: the daemon writes files under the
// §10.3 state directory, and the crash tests need a store that can fail on a
// chosen write, so the ordering rules are proven against a store that fails
// where a disk would.
type Store interface {
	// LoadSandbox returns the record for a claim cycle, or ErrNoSandboxRecord.
	LoadSandbox(claim remote.Claim) (SandboxRecord, error)
	// SaveSandbox replaces it, atomically against a reader in another process.
	SaveSandbox(rec SandboxRecord) error
	// DeleteSandbox removes it. Absent is not an error.
	DeleteSandbox(claim remote.Claim) error
	// Claims lists every sandbox-record entry for startup reconciliation.
	// Per-entry read and decode failures are returned on that SandboxClaim;
	// only failure to enumerate the directory itself is global. Sorted, so a
	// reconciliation report is diffable.
	Claims() ([]SandboxClaim, error)

	// LoadBinding returns the write-ahead start record for an address, or
	// ErrNoRunBinding. RunID is empty while a start response is ambiguous.
	LoadBinding(address string) (StartBinding, error)
	// ReserveBinding durably records the first possible start time before a
	// request is sent. Repeating it returns the original record and never moves
	// the replay fence.
	ReserveBinding(address string, substrate SubstrateBinding, principalBinding string, attemptedAt time.Time) (StartBinding, error)
	// SaveBinding records it. A second call with a *different* id for the same
	// address must refuse: an address resolves to one run for its whole life,
	// and a rebinding is how one dispatch becomes two. Binding a run clears
	// any recorded refusal: the address now names something that exists.
	SaveBinding(address string, substrate SubstrateBinding, runID string) error
	// RecordRefusal retains the backend's definite pre-claim refusal of the
	// exact body a start sent, so that body is never sent again while a
	// different one under the same address still may be (#284). Refused while
	// a RunID is bound: a run that exists was not refused.
	RecordRefusal(address string, substrate SubstrateBinding, refusal StartRefusal) error
	// RenewStart re-arms the write-ahead fence at a refused address for a
	// different body: the refusal is cleared and the replay window restarts at
	// attemptedAt, so a lost response to the new send reads as an unanswered
	// start rather than as the old refusal. Refused unless a refusal is recorded
	// and no run is bound.
	RenewStart(address string, substrate SubstrateBinding, principalBinding string, attemptedAt time.Time) (StartBinding, error)
	// SetStdinPending records whether a streaming run's stdin delivery is
	// still owed at this address. Set before the start that will create the
	// run and cleared after the close receipt, so a process that dies between
	// the two leaves a binding that says the prompt has not all arrived.
	SetStdinPending(address string, substrate SubstrateBinding, pending bool) error
}

// DirStore keeps one file per record under a directory, replaced by rename
// after an fsync — internal/state's ordering, for its reason: rename publishes
// a directory entry, not the bytes behind it.
type DirStore struct{ root string }

// NewDirStore names a directory. Created on first write, so a daemon whose
// state directory does not exist yet is not a construction failure.
func NewDirStore(root string) *DirStore { return &DirStore{root: root} }

// Root is the directory itself.
func (s *DirStore) Root() string { return s.root }

func (s *DirStore) sandboxDir() string { return filepath.Join(s.root, "sandboxes") }
func (s *DirStore) bindingDir() string { return filepath.Join(s.root, "runs") }
func (s *DirStore) sandboxPath(claim remote.Claim) string {
	return filepath.Join(s.sandboxDir(), safeName(claim.String())+".json")
}
func (s *DirStore) bindingPath(address string) string {
	return filepath.Join(s.bindingDir(), safeName(address)+".json")
}

func (s *DirStore) LoadSandbox(claim remote.Claim) (SandboxRecord, error) {
	body, err := os.ReadFile(s.sandboxPath(claim))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return SandboxRecord{}, fmt.Errorf("%w: %s", ErrNoSandboxRecord, claim)
	case err != nil:
		return SandboxRecord{}, fmt.Errorf("airlock: reading the sandbox record for %s: %w", claim, err)
	}
	var rec SandboxRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return SandboxRecord{}, fmt.Errorf("airlock: decoding the sandbox record for %s: %w", claim, err)
	}
	if rec.Version > SandboxRecordVersion {
		// remote.DecodeRecord's call, for its reason: this file is an address,
		// and attaching to a half-understood one acquires twice.
		return SandboxRecord{}, fmt.Errorf("airlock: sandbox record is version %d and this binary understands %d: "+
			"refusing to address a sandbox it may name wrongly", rec.Version, SandboxRecordVersion)
	}
	if rec.Claim != claim {
		return SandboxRecord{}, fmt.Errorf("airlock: %s names claim %s", s.sandboxPath(claim), rec.Claim)
	}
	return rec, nil
}

func (s *DirStore) SaveSandbox(rec SandboxRecord) error {
	if rec.Version == 0 {
		rec.Version = SandboxRecordVersion
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("airlock: encoding the sandbox record for %s: %w", rec.Claim, err)
	}
	return replaceFile(s.sandboxPath(rec.Claim), append(body, '\n'))
}

func (s *DirStore) DeleteSandbox(claim remote.Claim) error {
	err := os.Remove(s.sandboxPath(claim))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("airlock: removing the sandbox record for %s: %w", claim, err)
	}
	return nil
}

func (s *DirStore) Claims() ([]SandboxClaim, error) {
	entries, err := os.ReadDir(s.sandboxDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("airlock: listing sandbox records: %w", err)
	}
	var claims []SandboxClaim
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(s.sandboxDir(), entry.Name()))
		if err != nil {
			claims = append(claims, SandboxClaim{Record: entry.Name(), Err: fmt.Errorf(
				"airlock: reading sandbox record %s: %w", entry.Name(), err)})
			continue
		}
		var rec SandboxRecord
		if err := json.Unmarshal(body, &rec); err != nil {
			// json.Unmarshal may still recover Claim from a valid prefix or from a
			// later field's type error. Carry it when available; Record identifies
			// the entry when the syntax was too damaged to recover even that.
			claims = append(claims, SandboxClaim{Claim: rec.Claim, Record: entry.Name(), Err: fmt.Errorf(
				"airlock: decoding sandbox record %s: %w", entry.Name(), err)})
			continue
		}
		if !rec.Claim.Valid() || entry.Name() != filepath.Base(s.sandboxPath(rec.Claim)) {
			claims = append(claims, SandboxClaim{Claim: rec.Claim, Record: entry.Name(), Err: fmt.Errorf(
				"airlock: sandbox record %s does not name the claim encoded by its filename", entry.Name())})
			continue
		}
		claims = append(claims, SandboxClaim{Claim: rec.Claim, Record: entry.Name()})
	}
	sort.SliceStable(claims, func(i, j int) bool {
		left, right := claims[i].Claim.String(), claims[j].Claim.String()
		if left == right {
			return claims[i].Record < claims[j].Record
		}
		return left < right
	})
	return claims, nil
}

// StartBindingVersion is the current StartBinding.Version.
//
// 4 added StdinPending and Refusal (#284). The bump is the rollback guard: a
// daemon that understands 3 would load either binding and ignore the field —
// attaching to a streaming run whose prompt it never finishes delivering, or
// replaying a refused body as an unanswered start on every sweep, which is the
// loop this ticket closed. A version-3 binding is re-stamped 4 only when it
// gains one of the fields (refuseBinding, renewBinding, SetStdinPending).
const StartBindingVersion = 4

// StartBinding is the write-ahead record for one startRun address. The fence
// lands before the request; RunID is added after Airlock answers and becomes
// the permanent handle used after the bounded replay window.
type StartBinding struct {
	Version   int              `json:"version,omitempty"`
	Address   string           `json:"address"`
	Substrate SubstrateBinding `json:"substrate"`
	// PrincipalBinding has the sandbox record's meaning for startRun. It is
	// needed both to replay an unanswered start and to keep every later keyed
	// signal in the principal scope that owns the permanent RunID.
	PrincipalBinding string    `json:"principal_binding,omitempty"`
	RunID            string    `json:"run_id,omitempty"`
	StartAttemptedAt time.Time `json:"start_attempted_at,omitempty"`
	// StdinPending says the run at this address was started in streaming stdin
	// mode and its prompt has not been confirmed closed. Set before the start
	// request, cleared after the close receipt; a replayed Start completes the
	// delivery it finds owed. Offset receipts make that resend exact rather
	// than duplicated (docs/AIRLOCK.md).
	StdinPending bool `json:"stdin_pending,omitempty"`
	// Refusal is the backend's definite pre-claim refusal of the last body sent
	// under this address, while no run is bound. It is what turns "unanswered"
	// into "answered, and the answer was no" for every later read — and the
	// fingerprint it carries is what lets a changed body go out while an
	// unchanged one is answered locally (#284).
	Refusal *StartRefusal `json:"refusal,omitempty"`
}

// StartRefusal is one recorded admission refusal.
type StartRefusal struct {
	// Code is the backend's stable failure identifier.
	Code string `json:"code"`
	// Message is the backend's sanitized statement, bound by the contract to
	// carry no stdin bytes, environment values or argv beyond argv[0].
	Message string `json:"message,omitempty"`
	// LimitBytes is the exceeded limit when the code names one.
	LimitBytes int64 `json:"limit_bytes,omitempty"`
	// Fingerprint is a digest of the exact encoded request body that was
	// refused. The same address composes the same body only while nothing
	// about the request or its delivery has changed, so an equal fingerprint
	// is proof that resending would be refused identically.
	Fingerprint string `json:"fingerprint"`
	// RefusedAt is when the refusal was recorded.
	RefusedAt time.Time `json:"refused_at"`
}

// Complete reports a refusal that names its code and the body it refused.
func (r StartRefusal) Complete() bool { return r.Code != "" && r.Fingerprint != "" }

func (s *DirStore) LoadBinding(address string) (StartBinding, error) {
	body, err := os.ReadFile(s.bindingPath(address))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return StartBinding{}, fmt.Errorf("%w: %s", ErrNoRunBinding, address)
	case err != nil:
		return StartBinding{}, fmt.Errorf("airlock: reading the run binding for %s: %w", address, err)
	}
	var b StartBinding
	if err := json.Unmarshal(body, &b); err != nil {
		return StartBinding{}, fmt.Errorf("airlock: decoding the run binding for %s: %w", address, err)
	}
	if b.Version > StartBindingVersion {
		return StartBinding{}, fmt.Errorf("airlock: run binding is version %d and this binary understands %d",
			b.Version, StartBindingVersion)
	}
	if b.Address != address || (b.RunID == "" && b.StartAttemptedAt.IsZero()) {
		return StartBinding{}, fmt.Errorf("%w: %s is bound to %q", ErrUnexpectedRun, address, b.Address)
	}
	return b, nil
}

func (s *DirStore) ReserveBinding(
	address string, substrate SubstrateBinding, principalBinding string, attemptedAt time.Time,
) (StartBinding, error) {
	if existing, err := s.LoadBinding(address); err == nil {
		if err := requireSubstrateBinding(existing.Substrate, substrate, address); err != nil {
			return StartBinding{}, err
		}
		if existing.RunID == "" {
			if err := requirePrincipalBinding(existing.PrincipalBinding, principalBinding, address); err != nil {
				return StartBinding{}, err
			}
		}
		return existing, nil
	} else if !errors.Is(err, ErrNoRunBinding) {
		return StartBinding{}, err
	}
	if !substrate.complete() {
		return StartBinding{}, fmt.Errorf("%w: %s has an incomplete current binding", ErrSubstrateBinding, address)
	}
	if principalBinding == "" {
		return StartBinding{}, fmt.Errorf("%w: %s has no current runtime principal binding", ErrSubstrateBinding, address)
	}
	if attemptedAt.IsZero() {
		return StartBinding{}, fmt.Errorf("%w: %s has no start time", ErrUnexpectedRun, address)
	}
	b := StartBinding{
		Version: StartBindingVersion, Address: address, Substrate: substrate,
		PrincipalBinding: principalBinding, StartAttemptedAt: attemptedAt.UTC(),
	}
	if err := s.saveBinding(b); err != nil {
		return StartBinding{}, err
	}
	return b, nil
}

func (s *DirStore) SaveBinding(address string, substrate SubstrateBinding, runID string) error {
	if runID == "" {
		return fmt.Errorf("%w: %s answered with no run id", ErrUnexpectedRun, address)
	}
	b, err := s.LoadBinding(address)
	switch {
	case err == nil:
		if err := requireSubstrateBinding(b.Substrate, substrate, address); err != nil {
			return err
		}
		if b.RunID == runID {
			return nil
		}
		if b.RunID != "" {
			return fmt.Errorf("%w: %s is already bound to another run", ErrUnexpectedRun, address)
		}
	case errors.Is(err, ErrNoRunBinding):
		if !substrate.complete() {
			return fmt.Errorf("%w: %s has an incomplete current binding", ErrSubstrateBinding, address)
		}
		// Attach can supply a permanent run id from remote.Journal without the
		// start reservation that proves its creator principal. Reads by that id
		// remain safe; PrincipalBinding stays empty so a later keyed signal fails
		// closed instead of guessing from whichever credential is current then.
		b = StartBinding{Version: StartBindingVersion, Address: address, Substrate: substrate}
	case err != nil:
		return err
	}
	b.RunID = runID
	b.Refusal = nil
	return s.saveBinding(b)
}

func (s *DirStore) RecordRefusal(address string, substrate SubstrateBinding, refusal StartRefusal) error {
	b, err := s.LoadBinding(address)
	if err != nil {
		return err
	}
	if err := requireSubstrateBinding(b.Substrate, substrate, address); err != nil {
		return err
	}
	if err := checkRefusal(b, refusal); err != nil {
		return err
	}
	return s.saveBinding(refuseBinding(b, refusal))
}

func (s *DirStore) RenewStart(
	address string, substrate SubstrateBinding, principalBinding string, attemptedAt time.Time,
) (StartBinding, error) {
	b, err := s.LoadBinding(address)
	if err != nil {
		return StartBinding{}, err
	}
	if err := requireSubstrateBinding(b.Substrate, substrate, address); err != nil {
		return StartBinding{}, err
	}
	renewed, err := renewBinding(b, principalBinding, attemptedAt)
	if err != nil {
		return StartBinding{}, err
	}
	if err := s.saveBinding(renewed); err != nil {
		return StartBinding{}, err
	}
	return renewed, nil
}

// renewBinding is the one shape a renewed reservation may take, shared by both
// stores: only a refused, unbound address may be re-armed, and it is re-armed
// under the principal that will send the new body.
func renewBinding(b StartBinding, principalBinding string, attemptedAt time.Time) (StartBinding, error) {
	switch {
	case b.RunID != "":
		return StartBinding{}, fmt.Errorf("%w: %s is bound to run %s and needs no new start", ErrUnexpectedRun, b.Address, b.RunID)
	case b.Refusal == nil:
		return StartBinding{}, fmt.Errorf("%w: %s has an unanswered start and may not be re-armed", ErrUnexpectedRun, b.Address)
	case principalBinding == "":
		return StartBinding{}, fmt.Errorf("%w: %s has no current runtime principal binding", ErrSubstrateBinding, b.Address)
	case attemptedAt.IsZero():
		return StartBinding{}, fmt.Errorf("%w: %s has no start time", ErrUnexpectedRun, b.Address)
	}
	b.Version = StartBindingVersion
	b.Refusal = nil
	b.StdinPending = false
	b.PrincipalBinding = principalBinding
	b.StartAttemptedAt = attemptedAt.UTC()
	return b, nil
}

func (s *DirStore) SetStdinPending(address string, substrate SubstrateBinding, pending bool) error {
	b, err := s.LoadBinding(address)
	if err != nil {
		return err
	}
	if err := requireSubstrateBinding(b.Substrate, substrate, address); err != nil {
		return err
	}
	if b.StdinPending == pending {
		return nil
	}
	return s.saveBinding(stdinPendingBinding(b, pending))
}

// stdinPendingBinding is the one shape an owed or settled delivery may take,
// shared by both stores. Marking a delivery owed is the first use of a field
// an older reader does not have, so it stamps the version that carries it.
func stdinPendingBinding(b StartBinding, pending bool) StartBinding {
	b.StdinPending = pending
	if pending {
		b.Version = StartBindingVersion
	}
	return b
}

// refuseBinding is the one shape a refused binding may take, shared by both
// stores so neither can record a refusal over a run that exists. It stamps the
// version that carries the field, so a reader without it refuses the record
// instead of replaying the refused body as an unanswered start.
func refuseBinding(b StartBinding, refusal StartRefusal) StartBinding {
	b.Version = StartBindingVersion
	b.Refusal = &refusal
	b.StdinPending = false
	return b
}

// checkRefusal is the rule refuseBinding relies on, separated so a store can
// state it before writing.
func checkRefusal(b StartBinding, refusal StartRefusal) error {
	if b.RunID != "" {
		return fmt.Errorf("%w: %s is bound to run %s and cannot be recorded as refused", ErrUnexpectedRun, b.Address, b.RunID)
	}
	if !refusal.Complete() {
		return fmt.Errorf("%w: %s: a refusal must name its code and the body it refused", ErrUnexpectedRun, b.Address)
	}
	return nil
}

func (s *DirStore) saveBinding(b StartBinding) error {
	if b.Version == 0 {
		b.Version = StartBindingVersion
	}
	body, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("airlock: encoding the run binding for %s: %w", b.Address, err)
	}
	return replaceFile(s.bindingPath(b.Address), append(body, '\n'))
}

// safeName is a readable prefix plus a digest of the exact key. Sanitizing
// alone would collide — `a/b` and `a_b` land on one name — so the digest is what
// actually distinguishes them, exactly as remote.DirStore.Path does it.
func safeName(key string) string {
	var b strings.Builder
	for _, r := range key {
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
	sum := sha256.Sum256([]byte(key))
	return readable + "-" + hex.EncodeToString(sum[:6])
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
