// Package keyring stores agent secrets (HMAC key, bearer token) in the OS
// keyring rather than on disk. On Windows this is the Credential Manager, on
// macOS the Keychain, on Linux the Secret Service API (gnome-keyring / KDE
// Wallet). The Store interface lets tests inject an in-memory backend so CI
// runners without a configured keyring still exercise the calling code.
package keyring

import (
	"errors"
	"fmt"
	"sync"

	zk "github.com/zalando/go-keyring"
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

// Store is the narrow surface the rest of the agent uses. Two implementations
// ship: OSKeyring (production) and MemoryStore (tests + CI).
type Store interface {
	Set(service, key, value string) error
	Get(service, key string) (string, error)
	Delete(service, key string) error
}

// OSKeyring is the production backend. Safe for concurrent use — the
// underlying zalando/go-keyring is thread-safe on all three supported OSes.
type OSKeyring struct{}

// NewOSKeyring returns the default production Store. Callers don't need a
// constructor; OSKeyring{} is equivalent. The constructor exists so future
// platform-specific configuration (e.g. keychain group ACLs) has somewhere
// to live without breaking call sites.
func NewOSKeyring() *OSKeyring { return &OSKeyring{} }

// Set writes (or overwrites) a secret under the given service+key. Limits
// vary by OS — Windows Credential Manager caps at 2560 bytes per entry, macOS
// Keychain allows much more. The agent's largest secret is a 32-byte HMAC
// key base64-encoded (~44 bytes), so the limit never bites us in practice.
func (OSKeyring) Set(service, key, value string) error {
	if err := zk.Set(service, key, value); err != nil {
		return fmt.Errorf("keyring set %s/%s: %w", service, key, err)
	}
	return nil
}

// Get reads a secret. Returns ErrNotFound if the entry doesn't exist, or a
// wrapped error on infrastructure failure.
func (OSKeyring) Get(service, key string) (string, error) {
	v, err := zk.Get(service, key)
	if err != nil {
		if errors.Is(err, zk.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("keyring get %s/%s: %w", service, key, err)
	}
	return v, nil
}

// Delete removes a secret. Idempotent: deleting a non-existent entry is not
// an error, which simplifies the unpair flow (revoke can call Delete
// unconditionally without checking existence first).
func (OSKeyring) Delete(service, key string) error {
	if err := zk.Delete(service, key); err != nil {
		if errors.Is(err, zk.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("keyring delete %s/%s: %w", service, key, err)
	}
	return nil
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
