// Package keyring defines the Store interface used by the pair flow and
// daemon to read/write agent secrets, plus the connection-id-keyed key
// names and the in-memory test backend.
//
// Production secret storage lives in internal/secretstore, which is a
// file-based AES-GCM store with cross-user access on Windows. The
// per-user OS keyring (Windows Credential Manager / macOS Keychain /
// Linux Secret Service) was tried in v0.1.0 and removed because it
// doesn't share between the pair-running user and the LocalSystem-
// running service — see the v0.1.2 PR for the full post-mortem.
//
// This package is now a thin definitions-only module retained so test
// code can import keyring.MemoryStore and so the Store interface is
// importable without taking a dep on secretstore's crypto.
package keyring

import (
	"errors"
	"sync"
)

// ServiceName is the label the agent presents to the OS keyring. Using a
// stable constant rather than a per-binary value means a user who reinstalls
// the agent can re-read their existing secrets instead of losing them to a
// new service label.
const ServiceName = "gstreco-tally-agent"

// ErrNotFound is returned when a Get call finds no entry for the given key.
// Distinguishable from infrastructure errors (keyring daemon unreachable,
// permission denied) so callers can surface a clean "not paired yet" message
// to the user.
var ErrNotFound = errors.New("keyring: secret not found")

// Store is the narrow surface the rest of the agent uses. Production
// implementations live in internal/secretstore (file-based AES-GCM);
// tests use MemoryStore below.
type Store interface {
	Set(service, key, value string) error
	Get(service, key string) (string, error)
	Delete(service, key string) error
}

// MemoryStore is an in-process keyring used by tests and any build that can't
// reach an OS keyring (headless CI, containers without dbus). Do not use in
// production — secrets are lost when the process exits.
type MemoryStore struct {
	mu      sync.Mutex
	entries map[string]string // keyed by service+"\x00"+key
}

// NewMemoryStore returns an empty MemoryStore. Safe for concurrent use.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{entries: map[string]string{}}
}

func memKey(service, key string) string { return service + "\x00" + key }

func (m *MemoryStore) Set(service, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[memKey(service, key)] = value
	return nil
}

func (m *MemoryStore) Get(service, key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.entries[memKey(service, key)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (m *MemoryStore) Delete(service, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, memKey(service, key))
	return nil
}

// ConnectionKeys returns the keyring key names for a given connection. Keeps
// the naming convention in one place so Get/Set/Delete sites don't drift.
// Callers must never log these values — they contain the connection id which
// is sensitive when combined with the token.
func ConnectionKeys(connectionID string) (hmacKey, bearerKey string) {
	return connectionID + ".hmac_secret", connectionID + ".bearer_token"
}
