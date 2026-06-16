package secretstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"golang.org/x/crypto/hkdf"
)

// ErrNotFound is returned when a Get call finds no entry for the given
// (service, key). Distinguishable from infrastructure errors so callers
// can surface "not paired yet" vs "disk full / permission denied".
var ErrNotFound = errors.New("secretstore: secret not found")

// Store is the narrow surface the rest of the agent uses. Mirrors
// internal/keyring.Store so existing call sites refactor with a single
// constructor swap.
type Store interface {
	Set(service, key, value string) error
	Get(service, key string) (string, error)
	Delete(service, key string) error
}

// fileStore is the production backend. Files live under one directory
// per Store; one file per (service, key) entry.
//
// Concurrency: a Store is safe for concurrent use across goroutines. The
// mutex serialises filesystem operations so two writers don't race on
// the rename step. Cross-process safety is provided by the rename being
// atomic on the same filesystem — a reader either sees the old file or
// the new file, never a half-written one.
type fileStore struct {
	dir    string
	keySrc keyDeriver
	mu     sync.Mutex
}

// keyDeriver returns the 32-byte AES-256 key used for this store. Wrapped
// in a closure so tests can inject a deterministic key without mocking
// the entire machine-id / HKDF chain.
type keyDeriver func() ([]byte, error)

// hkdfInfo is the per-purpose label HKDF uses to domain-separate keys
// derived from the same machine ID. Bumping the version suffix forces
// existing secret files to fail decryption — used as a kill switch if we
// ever discover a flaw in the derivation.
const hkdfInfo = "gstreco-tally-agent/v1/secret-key"

// hkdfSalt is the binary-baked salt. Must be the same across all installs
// so a paired agent's secrets remain readable across upgrades. Generated
// once via `openssl rand -hex 16` and committed.
var hkdfSalt = mustHex("9c5b3f1e88a2c4d70f4e6a1b2c3d4e5f")

// nonceLen is the AES-GCM nonce size. 12 is the standard.
const nonceLen = 12

// applyDirACLFn is a package-level seam for tests. Defaults to the
// platform implementation in acl_windows.go / acl_other.go; tests
// swap it to a stub that simulates a Windows DACL adjustment failing
// (e.g., icacls returns non-zero, GPO blocks, antivirus interferes).
//
// The DIPL Delhi pilot 2026-04-27 hit the silent-failure variant of
// this — applyDirACL failed but Set() warned-and-continued, so the
// secret file was written into a directory whose DACL the running
// LocalSystem service couldn't traverse → "Access is denied" loop on
// every heartbeat with no actionable error. v0.1.10 makes the
// failure fatal so pair surfaces it immediately.
var applyDirACLFn = applyDirACL
var applyEntryACLFn = applyEntryACL

// NewFileStore returns a Store backed by encrypted files at dir. The
// directory is created on first write if it does not exist; an
// inaccessible parent directory surfaces as a wrapped os error from
// Set().
//
// machineID returns the local machine identifier — the caller is
// expected to wire ReadMachineID() in production and a stub in tests.
func NewFileStore(dir string, machineID func() (string, error)) Store {
	return &fileStore{
		dir: dir,
		keySrc: func() ([]byte, error) {
			id, err := machineID()
			if err != nil {
				return nil, fmt.Errorf("secretstore: read machine id: %w", err)
			}
			if id == "" {
				return nil, errors.New("secretstore: machine id is empty (host did not provide one)")
			}
			h := hkdf.New(sha256.New, []byte(id), hkdfSalt, []byte(hkdfInfo))
			out := make([]byte, 32)
			if _, err := io.ReadFull(h, out); err != nil {
				return nil, fmt.Errorf("secretstore: hkdf: %w", err)
			}
			return out, nil
		},
	}
}

// newFileStoreWithKey is the test-friendly constructor. Skips machine-id
// resolution entirely so tests don't depend on the host environment.
// Returns a fresh copy of the key on every keySrc() call so the
// `defer zero(aesKey)` in Set/Get can't blank out the master copy.
func newFileStoreWithKey(dir string, key []byte) *fileStore {
	master := append([]byte(nil), key...)
	return &fileStore{
		dir: dir,
		keySrc: func() ([]byte, error) {
			fresh := make([]byte, len(master))
			copy(fresh, master)
			return fresh, nil
		},
	}
}

// entryPath returns the absolute path of the file backing one (service,
// key) entry. Filenames are sha256(service+\x00+key) in hex so they're
// short, filesystem-safe, and don't leak the connection id by name.
func (s *fileStore) entryPath(service, key string) string {
	h := sha256.Sum256([]byte(service + "\x00" + key))
	return filepath.Join(s.dir, hex.EncodeToString(h[:])+".bin")
}

func (s *fileStore) Set(service, key, value string) error {
	if service == "" || key == "" {
		return errors.New("secretstore: service and key must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("secretstore: mkdir %s: %w", s.dir, err)
	}
	// applyDirACLFn failure is FATAL as of v0.1.10. On Windows it
	// invokes icacls to grant SYSTEM + BUILTIN\Administrators full
	// control on the secrets dir; if that call fails, any file we
	// write next will be inaccessible to the LocalSystem service at
	// runtime and the agent silently 401-loops. Surfacing the error
	// here makes pair fail loudly so the operator can fix it (run
	// install.ps1 again with the icacls reset baked in, or check
	// AV / GPO interference) before pair returns success.
	if err := applyDirACLFn(s.dir); err != nil {
		return fmt.Errorf("secretstore: applyDirACL %s: %w", s.dir, err)
	}

	aesKey, err := s.keySrc()
	if err != nil {
		return err
	}
	defer zero(aesKey)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return fmt.Errorf("secretstore: aes new: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("secretstore: gcm new: %w", err)
	}
	nonce := make([]byte, nonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("secretstore: rand nonce: %w", err)
	}

	// AAD ties the ciphertext to its (service, key) pair so a renamed
	// file fails to decrypt — defence in depth against a "swap two
	// secret files and see which lock opens" attack.
	aad := []byte(service + "\x00" + key)
	ciphertext := gcm.Seal(nil, nonce, []byte(value), aad)

	body := append([]byte(nil), nonce...)
	body = append(body, ciphertext...)

	path := s.entryPath(service, key)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("secretstore: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("secretstore: rename %s: %w", tmp, err)
	}

	// Apply the directory ACL again after the final entry exists. On
	// Windows, the temp-file creation path can leave the renamed file
	// readable by the pairing user while still unreadable by the
	// LocalSystem service. The pre-write ACL makes the directory
	// traversable; the post-rename pass makes the concrete entry
	// inherit the service-readable DACL before write-verify succeeds.
	if err := applyDirACLFn(s.dir); err != nil {
		return fmt.Errorf("secretstore: apply entry ACL %s: %w", path, err)
	}
	if err := applyEntryACLFn(path); err != nil {
		return fmt.Errorf("secretstore: apply file ACL %s: %w", path, err)
	}

	// Write-verify roundtrip. We just successfully wrote ciphertext to
	// disk and applied the entry ACL; now confirm the resulting
	// file is actually readable + decryptable from THIS process. If
	// the host environment (EFS bound to a different user, restrictive
	// inherited ACL, AV intercepting reads) blocks read access, this
	// catches it now so pair returns a clear error rather than letting
	// the service hit the failure on its first heartbeat 60 seconds
	// later. Cost: one extra ReadFile + GCM Open per pair. Pair is
	// one-shot so the cost is negligible.
	got, verifyErr := s.getLocked(service, key)
	if verifyErr != nil {
		return fmt.Errorf("secretstore: write-verify %s: %w", path, verifyErr)
	}
	if got != value {
		return fmt.Errorf("secretstore: write-verify %s: roundtrip mismatch (got %d-byte plaintext, want %d-byte)", path, len(got), len(value))
	}
	return nil
}

func (s *fileStore) Get(service, key string) (string, error) {
	if service == "" || key == "" {
		return "", errors.New("secretstore: service and key must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(service, key)
}

// getLocked is the lock-free body of Get. Callers must hold s.mu.
// Set's write-verify roundtrip uses this helper to read back the
// just-written entry without re-acquiring the lock.
func (s *fileStore) getLocked(service, key string) (string, error) {
	path := s.entryPath(service, key)
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("secretstore: read %s: %w", path, err)
	}
	if len(body) < nonceLen+16 { // 16 = AES-GCM auth tag
		return "", fmt.Errorf("secretstore: %s is too short to be a valid entry", path)
	}

	aesKey, err := s.keySrc()
	if err != nil {
		return "", err
	}
	defer zero(aesKey)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return "", fmt.Errorf("secretstore: aes new: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("secretstore: gcm new: %w", err)
	}
	nonce := body[:nonceLen]
	ciphertext := body[nonceLen:]
	aad := []byte(service + "\x00" + key)

	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		// Decryption failure is either tampering or a different
		// machine-id (machine restored from backup, etc.). Same error
		// — caller should surface "secret unreadable, repair".
		return "", fmt.Errorf("secretstore: decrypt %s: %w", path, err)
	}
	return string(plaintext), nil
}

func (s *fileStore) Delete(service, key string) error {
	if service == "" || key == "" {
		return errors.New("secretstore: service and key must be non-empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.entryPath(service, key)
	if err := os.Remove(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Idempotent — deleting a non-existent entry is fine.
			return nil
		}
		return fmt.Errorf("secretstore: remove %s: %w", path, err)
	}
	return nil
}

// DefaultDir returns the platform-appropriate secrets directory.
// Windows: %ProgramData%\GST Reco\agent\secrets — same root as the
// config file so the install is self-contained.
// Unix: ~/.gstreco-agent/secrets — same root as the config file in the
// dev-on-Mac case.
func DefaultDir(configDir string) string {
	return filepath.Join(configDir, "secrets")
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(fmt.Sprintf("secretstore: bad salt constant: %v", err))
	}
	return b
}

// zero overwrites the AES key in memory after we're done with it. Best
// effort — Go's garbage collector may have already moved the slice.
func zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
