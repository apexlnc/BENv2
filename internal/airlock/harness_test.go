package airlock

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/srhg-ai-7cef3f93/ben/internal/airlock/airlocktest"
	"github.com/srhg-ai-7cef3f93/ben/internal/core"
	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// The shared fixture. Every test in this package drives the same three things —
// the contract fake, a durable store, and a Substrate built over both — because
// the properties under test are properties of the *pair*: what BEN persists
// before a request, and what the contract does with that request when the
// response is lost.

// testClaim is one claim cycle. Positive epoch, because zero authorizes nothing.
var testClaim = remote.Claim{Repository: "srhg-ai/ben", Issue: "194", Epoch: 4242}

const (
	testBranch  = "ben/194"
	testBaseSHA = "0f5d3c1b9a7e6d4c2b0a8f6e4d2c0b9a8f6e4d2c"
)

// tokenSource is a core.Source that answers with a scripted bearer token. The
// default value is deliberately distinctive, so a test asserting that no
// credential was serialized into a request body can search for it.
type tokenSource struct {
	value        string
	nextValue    string
	err          error
	bindingKey   string
	principalKey string
	mu           sync.Mutex
	calls        int
}

func (s *tokenSource) Fetch(context.Context, core.Purpose) (core.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return core.Token{}, s.err
	}
	value := s.value
	if s.nextValue != "" {
		s.value, s.nextValue = s.nextValue, ""
	}
	return core.Token{Value: value}, nil
}

func (s *tokenSource) Descriptor() core.SourceDescriptor {
	key := s.bindingKey
	if key == "" {
		key = "static:airlock"
	}
	return core.SourceDescriptor{
		Kind: "static", Authority: "env:AIRLOCK_TOKEN", BindingKey: key, PrincipalKey: s.principalKey,
	}
}

func (s *tokenSource) setValue(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
}

// rotateAfterNextFetch models an environment value changing after one caller
// has captured it but before that caller retries.
func (s *tokenSource) rotateAfterNextFetch(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextValue = value
}

// memStore is the durable store with a settable write fault, so a crash can be
// placed at an exact durable boundary rather than approximated by killing a
// process at an unpredictable moment.
type memStore struct {
	mu        sync.Mutex
	sandboxes map[string]SandboxRecord
	bindings  map[string]StartBinding
	// saveFault fails a SaveSandbox, chosen by the record about to be written.
	saveFault func(SandboxRecord) error
	// reserveFault fails a write-ahead start reservation.
	reserveFault func(address string, attemptedAt time.Time) error
	// bindFault fails a SaveBinding.
	bindFault func(address, runID string) error
	// refusalFault fails a RecordRefusal: the disk giving out exactly when the
	// backend's definite answer has to be made durable.
	refusalFault func(address string) error
	saves        int
	binds        int
}

func newMemStore() *memStore {
	return &memStore{sandboxes: map[string]SandboxRecord{}, bindings: map[string]StartBinding{}}
}

func (s *memStore) LoadSandbox(claim remote.Claim) (SandboxRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.sandboxes[claim.String()]
	if !ok {
		return SandboxRecord{}, fmt.Errorf("%w: %s", ErrNoSandboxRecord, claim)
	}
	return rec, nil
}

func (s *memStore) SaveSandbox(rec SandboxRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saveFault != nil {
		if err := s.saveFault(rec); err != nil {
			return err
		}
	}
	s.saves++
	s.sandboxes[rec.Claim.String()] = rec
	return nil
}

func (s *memStore) DeleteSandbox(claim remote.Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sandboxes, claim.String())
	return nil
}

func (s *memStore) Claims() ([]SandboxClaim, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	claims := make([]SandboxClaim, 0, len(s.sandboxes))
	for _, rec := range s.sandboxes {
		claims = append(claims, SandboxClaim{Claim: rec.Claim})
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Claim.String() < claims[j].Claim.String() })
	return claims, nil
}

func (s *memStore) LoadBinding(address string) (StartBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding, ok := s.bindings[address]
	if !ok {
		return StartBinding{}, fmt.Errorf("%w: %s", ErrNoRunBinding, address)
	}
	return binding, nil
}

func (s *memStore) ReserveBinding(
	address string, substrate SubstrateBinding, principalBinding string, attemptedAt time.Time,
) (StartBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !substrate.complete() {
		return StartBinding{}, fmt.Errorf("%w: %s has an incomplete current binding", ErrSubstrateBinding, address)
	}
	if existing, ok := s.bindings[address]; ok {
		if err := requireSubstrateBinding(existing.Substrate, substrate, address); err != nil {
			return StartBinding{}, err
		}
		if existing.RunID == "" {
			if err := requirePrincipalBinding(existing.PrincipalBinding, principalBinding, address); err != nil {
				return StartBinding{}, err
			}
		}
		return existing, nil
	}
	if principalBinding == "" {
		return StartBinding{}, fmt.Errorf("%w: %s has no current runtime principal binding", ErrSubstrateBinding, address)
	}
	if attemptedAt.IsZero() {
		return StartBinding{}, fmt.Errorf("%w: %s has no start time", ErrUnexpectedRun, address)
	}
	if s.reserveFault != nil {
		if err := s.reserveFault(address, attemptedAt); err != nil {
			return StartBinding{}, err
		}
	}
	binding := StartBinding{
		Version: StartBindingVersion, Address: address, Substrate: substrate,
		PrincipalBinding: principalBinding, StartAttemptedAt: attemptedAt.UTC(),
	}
	s.bindings[address] = binding
	return binding, nil
}

func (s *memStore) SaveBinding(address string, substrate SubstrateBinding, runID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !substrate.complete() {
		return fmt.Errorf("%w: %s has an incomplete current binding", ErrSubstrateBinding, address)
	}
	if runID == "" {
		return fmt.Errorf("%w: %s answered with no run id", ErrUnexpectedRun, address)
	}
	if existing, ok := s.bindings[address]; ok {
		if err := requireSubstrateBinding(existing.Substrate, substrate, address); err != nil {
			return err
		}
		if existing.RunID == runID {
			return nil
		}
		if existing.RunID != "" {
			return fmt.Errorf("%w: %s is already bound", ErrUnexpectedRun, address)
		}
	}
	if s.bindFault != nil {
		if err := s.bindFault(address, runID); err != nil {
			return err
		}
	}
	s.binds++
	binding := s.bindings[address]
	if binding.Address == "" {
		binding = StartBinding{Version: StartBindingVersion, Address: address, Substrate: substrate}
	}
	binding.RunID = runID
	binding.Refusal = nil
	s.bindings[address] = binding
	return nil
}

func (s *memStore) RecordRefusal(address string, substrate SubstrateBinding, refusal StartRefusal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[address]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoRunBinding, address)
	}
	if err := requireSubstrateBinding(b.Substrate, substrate, address); err != nil {
		return err
	}
	if err := checkRefusal(b, refusal); err != nil {
		return err
	}
	if s.refusalFault != nil {
		if err := s.refusalFault(address); err != nil {
			return err
		}
	}
	s.bindings[address] = refuseBinding(b, refusal)
	return nil
}

func (s *memStore) setRefusalFault(fn func(string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refusalFault = fn
}

// forgetLimits turns a record into one written before the envelope was
// recorded — the shape every sandbox record has on the first daemon that
// carries #284.
func (s *memStore) forgetLimits(claim remote.Claim) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.sandboxes[claim.String()]
	rec.Limits = nil
	s.sandboxes[claim.String()] = rec
}

func (s *memStore) sandbox(claim remote.Claim) SandboxRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sandboxes[claim.String()]
}

func (s *memStore) RenewStart(
	address string, substrate SubstrateBinding, principalBinding string, attemptedAt time.Time,
) (StartBinding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[address]
	if !ok {
		return StartBinding{}, fmt.Errorf("%w: %s", ErrNoRunBinding, address)
	}
	if err := requireSubstrateBinding(b.Substrate, substrate, address); err != nil {
		return StartBinding{}, err
	}
	renewed, err := renewBinding(b, principalBinding, attemptedAt)
	if err != nil {
		return StartBinding{}, err
	}
	s.bindings[address] = renewed
	return renewed, nil
}

func (s *memStore) SetStdinPending(address string, substrate SubstrateBinding, pending bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bindings[address]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNoRunBinding, address)
	}
	if err := requireSubstrateBinding(b.Substrate, substrate, address); err != nil {
		return err
	}
	s.bindings[address] = stdinPendingBinding(b, pending)
	return nil
}

func (s *memStore) binding(address string) StartBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bindings[address]
}

func (s *memStore) setBindingAttemptedAt(address string, attemptedAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.bindings[address]
	binding.StartAttemptedAt = attemptedAt.UTC()
	s.bindings[address] = binding
}

func (s *memStore) setBindingPrincipal(address, principal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	binding := s.bindings[address]
	binding.PrincipalBinding = principal
	s.bindings[address] = binding
}

func (s *memStore) setReserveFault(fn func(string, time.Time) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reserveFault = fn
}

func (s *memStore) setSaveFault(fn func(SandboxRecord) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveFault = fn
}

func (s *memStore) setBindFault(fn func(string, string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bindFault = fn
}

// fixture is a fake control plane, a durable store, and the Substrate over both.
type fixture struct {
	t     *testing.T
	srv   *airlocktest.Server
	store Store
	sub   *Substrate
	auth  *tokenSource
}

// newFixture builds the trio. The base URL is https and unreachable by DNS on
// purpose: the fake is reached through the injected transport, which keeps the
// client's plain-http refusal under test rather than making the fake a reason
// to weaken it.
func newFixture(t *testing.T, mutate ...func(*Options)) *fixture {
	t.Helper()
	srv := airlocktest.New(t)
	store := newMemStore()
	auth := &tokenSource{value: airlocktest.DefaultToken}
	opts := Options{
		BaseURL:   "https://airlock.invalid",
		Auth:      auth,
		Profile:   airlocktest.DefaultProfile,
		Store:     store,
		Transport: srv.Transport(),
		Timeouts: Timeouts{
			Request: 5 * time.Second, Poll: 5 * time.Second, PollWait: time.Second,
			Settle: 2 * time.Second, Retries: 2,
		},
	}
	for _, fn := range mutate {
		fn(&opts)
	}
	sub, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &fixture{t: t, srv: srv, store: opts.Store, sub: sub, auth: auth}
}

// rebuild is a daemon restart: a fresh Substrate over the *same* durable store
// and the same fake. Everything the new process knows it read off disk.
func (f *fixture) rebuild() *Substrate {
	f.t.Helper()
	sub := f.newSubstrate("https://airlock.invalid", f.auth, f.srv)
	f.sub = sub
	return sub
}

func (f *fixture) newSubstrate(baseURL string, auth core.Source, srv *airlocktest.Server) *Substrate {
	f.t.Helper()
	sub, err := New(Options{
		BaseURL: baseURL, Auth: auth, Profile: airlocktest.DefaultProfile,
		Store: f.store, Transport: srv.Transport(),
		Timeouts: Timeouts{
			Request: 5 * time.Second, Poll: 5 * time.Second, PollWait: time.Second,
			Settle: 2 * time.Second, Retries: 2,
		},
	})
	if err != nil {
		f.t.Fatalf("new substrate: %v", err)
	}
	return sub
}

func (f *fixture) acquire(ctx context.Context) remote.Identity {
	f.t.Helper()
	id, err := f.sub.Workspaces.Acquire(ctx, remote.AcquireRequest{
		Claim: testClaim, Branch: testBranch, BaseSHA: testBaseSHA,
	})
	if err != nil {
		f.t.Fatalf("Acquire: %v", err)
	}
	return id
}

// ref builds the durable dispatch address for a spec, exactly as remote.Runner
// would: the digest is over the immutable ProcessSpec, so a test that changed
// argv would be addressing a different dispatch.
func mustRef(t *testing.T, id remote.Identity, run remote.RunID, spec remote.ProcessSpec) remote.ProcessRef {
	t.Helper()
	digest, err := remote.ProcessRequestDigest(spec)
	if err != nil {
		t.Fatalf("ProcessRequestDigest: %v", err)
	}
	return remote.ProcessRef{Identity: id, RunID: run, RequestDigest: digest}
}

func spec(id remote.Identity, argv ...string) remote.ProcessSpec {
	if len(argv) == 0 {
		argv = []string{"/usr/bin/claude", "--print"}
	}
	return remote.ProcessSpec{
		Identity: id, Argv: argv,
		Limits: core.RunLimits{StallTimeout: 5 * time.Minute, AttemptTimeout: time.Hour},
	}
}

func mustBe(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got %v, want %v", err, target)
	}
}
