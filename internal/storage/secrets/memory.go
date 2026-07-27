package secrets

import (
	"context"
	"maps"
	"slices"
	"sync"
)

// MemoryStore is an in-memory Store for tests and for the conformance suite.
//
// It is deliberately not wired into any production code path: the control plane requires a
// persistent store, and losing every credential on restart would not be a "development
// convenience".
type MemoryStore struct {
	mu      sync.RWMutex
	entries map[string]memoryEntry
	// FailPing makes HealthCheck fail, so callers can exercise degraded-readiness paths.
	FailPing error
}

type memoryEntry struct {
	ciphertext []byte
	keyVersion int32
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: make(map[string]memoryEntry)}
}

// PutSecret implements Store.
func (m *MemoryStore) PutSecret(_ context.Context, ref Ref, ciphertext []byte, keyVersion int32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Copy so a caller reusing its buffer cannot mutate stored state.
	stored := make([]byte, len(ciphertext))
	copy(stored, ciphertext)
	m.entries[ref.String()] = memoryEntry{ciphertext: stored, keyVersion: keyVersion}
	return nil
}

// GetSecret implements Store.
func (m *MemoryStore) GetSecret(_ context.Context, ref Ref) ([]byte, int32, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	entry, ok := m.entries[ref.String()]
	if !ok {
		return nil, 0, ErrNotFound
	}
	out := make([]byte, len(entry.ciphertext))
	copy(out, entry.ciphertext)
	return out, entry.keyVersion, nil
}

// DeleteSecret implements Store.
func (m *MemoryStore) DeleteSecret(_ context.Context, ref Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, ref.String())
	return nil
}

// PingSecrets implements Store.
func (m *MemoryStore) PingSecrets(context.Context) error { return m.FailPing }

// Corrupt overwrites the stored ciphertext at ref, so tests can assert that tampering is detected
// rather than silently decrypted.
func (m *MemoryStore) Corrupt(ref Ref, ciphertext []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if entry, ok := m.entries[ref.String()]; ok {
		entry.ciphertext = ciphertext
		m.entries[ref.String()] = entry
	}
}

// Keys returns the reference strings currently stored, for assertions.
func (m *MemoryStore) Keys() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return slices.Sorted(maps.Keys(m.entries))
}
