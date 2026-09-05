package remotetest

import (
	"fmt"
	"sync"

	"github.com/srhg-ai-7cef3f93/ben/internal/remote"
)

type MemHookStore struct {
	mu      sync.Mutex
	records map[string][]byte
	fault   func(remote.HookRecord) error
}

func NewMemHookStore() *MemHookStore {
	return &MemHookStore{records: map[string][]byte{}}
}

func (s *MemHookStore) LoadHook(key remote.HookKey) (remote.HookRecord, error) {
	s.mu.Lock()
	body, ok := s.records[key.String()]
	s.mu.Unlock()
	if !ok {
		return remote.HookRecord{}, fmt.Errorf("%w: %s", remote.ErrNoRecord, key)
	}
	return remote.DecodeHookRecord(body)
}

func (s *MemHookStore) SaveHook(r remote.HookRecord) error {
	s.mu.Lock()
	fault := s.fault
	s.mu.Unlock()
	if fault != nil {
		if err := fault(r); err != nil {
			return err
		}
	}
	body, err := remote.EncodeHookRecord(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[r.Ref.Key().String()] = body
	return nil
}

func (s *MemHookStore) DeleteHook(key remote.HookKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, key.String())
	return nil
}

func (s *MemHookStore) SetFault(fn func(remote.HookRecord) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fault = fn
}

func (s *MemHookStore) Has(key remote.HookKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.records[key.String()]
	return ok
}
