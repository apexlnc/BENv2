package remotetest

import (
	"fmt"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

// MemStore is an in-memory remote.Store whose writes a test can fail on demand.
//
// It keeps records **encoded**, not as structs, so every Save/Load in every test
// goes through the real durable encoding — a field that stopped surviving the
// round trip fails wherever it is used rather than only in the one test that
// encodes on purpose.
//
// The fault hook is what the crash tests are written against. A durable ordering
// is only a claim until the write in the middle of it fails: "identity before the
// act" and "the act before the position" are both invisible on a disk that never
// refuses, because every ordering passes when every write succeeds.
type MemStore struct {
	mu      sync.Mutex
	records map[string][]byte
	fault   func(r remote.Record) error
	saves   int
}

// NewMemStore builds an empty store.
func NewMemStore() *MemStore { return &MemStore{records: map[string][]byte{}} }

// SetSaveFault installs a hook consulted before every Save. A non-nil return
// fails the write and leaves the stored record untouched — which is what a disk
// that refused the write leaves behind, and the state a restart then reads. nil
// clears it.
func (s *MemStore) SetSaveFault(fn func(r remote.Record) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = fn
}

// Saves counts successful writes.
func (s *MemStore) Saves() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// Has reports whether a record exists, without decoding it.
func (s *MemStore) Has(claim remote.Claim) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.records[claim.String()]
	return ok
}

func (s *MemStore) Load(claim remote.Claim) (remote.Record, error) {
	s.mu.Lock()
	raw, ok := s.records[claim.String()]
	s.mu.Unlock()
	if !ok {
		return remote.Record{}, fmt.Errorf("%w: %s", remote.ErrNoRecord, claim)
	}
	return remote.DecodeRecord(raw)
}

func (s *MemStore) Save(r remote.Record) error {
	s.mu.Lock()
	fault := s.fault
	s.mu.Unlock()
	if fault != nil {
		if err := fault(r); err != nil {
			return err
		}
	}
	body, err := remote.EncodeRecord(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.Identity.Claim.String()] = body
	s.saves++
	return nil
}

func (s *MemStore) Delete(claim remote.Claim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, claim.String())
	return nil
}
