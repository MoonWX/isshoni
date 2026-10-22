package credstore

import (
	"maps"
	"slices"
	"sync"
)

// memoryStore is NewMemory's Store: a map guarded by a mutex.
type memoryStore struct {
	mu sync.Mutex
	m  map[string]Credential // origin → credential, normalized
}

// NewMemory returns an empty Store that lives in memory, for tests and the e2e harness. It applies the rules of
// every Store (the package documentation), with the default prefix for the key-length check. Each call returns an
// independent store.
func NewMemory() Store {
	return &memoryStore{m: make(map[string]Credential)}
}

func (s *memoryStore) Get(origin string) (Credential, error) {
	if err := checkOrigin(origin); err != nil {
		return Credential{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.m[origin]
	if !ok {
		return Credential{}, ErrNotFound
	}
	return c, nil
}

func (s *memoryStore) Put(origin string, c Credential) error {
	if _, err := checkPut(defaultPrefix, origin, c); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[origin] = normalize(c)
	return nil
}

func (s *memoryStore) Delete(origin string) error {
	if err := checkOrigin(origin); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.m, origin)
	return nil
}

func (s *memoryStore) List() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.m)), nil
}
