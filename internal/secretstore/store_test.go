package secretstore

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// tempStore returns a fileStore backed by a temp dir and a random AES
// key. Tests bypass the machine-id derivation so we don't depend on the
// host platform's machine-id machinery.
func tempStore(t *testing.T) *fileStore {
	t.Helper()
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return newFileStoreWithKey(dir, key)
}

func TestSetGet_RoundTrip(t *testing.T) {
	s := tempStore(t)

	if err := s.Set("svc", "k", "secret-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, err := s.Get("svc", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "secret-value" {
		t.Errorf("got %q, want %q", v, "secret-value")
	}
}

func TestGet_Missing_ReturnsErrNotFound(t *testing.T) {
	s := tempStore(t)

	_, err := s.Get("svc", "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestDelete_Missing_NoError(t *testing.T) {
	s := tempStore(t)

	if err := s.Delete("svc", "never-existed"); err != nil {
		t.Errorf("Delete on missing entry should be no-op, got %v", err)
	}
}

func TestDelete_Existing_RemovesFile(t *testing.T) {
	s := tempStore(t)

	if err := s.Set("svc", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Delete("svc", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err := s.Get("svc", "k")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete, Get should be ErrNotFound; got %v", err)
	}
}

func TestSet_Overwrites(t *testing.T) {
	s := tempStore(t)

	if err := s.Set("svc", "k", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := s.Set("svc", "k", "v2"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	v, err := s.Get("svc", "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if v != "v2" {
		t.Errorf("got %q, want %q (overwrite failed)", v, "v2")
	}
}

func TestServiceKeyNamespacing(t *testing.T) {
	s := tempStore(t)

	if err := s.Set("svc-a", "k", "v-a"); err != nil {
		t.Fatalf("Set svc-a: %v", err)
	}
	if err := s.Set("svc-b", "k", "v-b"); err != nil {
		t.Fatalf("Set svc-b: %v", err)
	}
	if err := s.Set("svc-a", "other", "v-other"); err != nil {
		t.Fatalf("Set svc-a/other: %v", err)
	}

	if v, _ := s.Get("svc-a", "k"); v != "v-a" {
		t.Errorf("svc-a/k = %q, want v-a", v)
	}
	if v, _ := s.Get("svc-b", "k"); v != "v-b" {
		t.Errorf("svc-b/k = %q, want v-b", v)
	}
	if v, _ := s.Get("svc-a", "other"); v != "v-other" {
		t.Errorf("svc-a/other = %q, want v-other", v)
	}
}

func TestEntryFile_TamperDetection(t *testing.T) {
	s := tempStore(t)

	if err := s.Set("svc", "k", "original"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	// Find the file and flip a byte in the ciphertext region (after
	// the 12-byte nonce). AES-GCM auth tag should reject the read.
	path := s.entryPath("svc", "k")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(body) <= nonceLen {
		t.Fatalf("body too short to tamper")
	}
	body[nonceLen] ^= 0x01
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("WriteFile tampered: %v", err)
	}

	_, err = s.Get("svc", "k")
	if err == nil {
		t.Fatal("expected decrypt error after tamper, got nil")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("expected decrypt-flavoured error, got %v", err)
	}
}

func TestEntryFile_AADBindsServiceKey(t *testing.T) {
	s := tempStore(t)

	if err := s.Set("svc-a", "k1", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	src := s.entryPath("svc-a", "k1")
	dst := s.entryPath("svc-b", "k1") // different service, same key
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := os.WriteFile(dst, body, 0o600); err != nil {
		t.Fatalf("WriteFile dst: %v", err)
	}

	// Rename attack: ciphertext from svc-a/k1 placed at svc-b/k1's
	// path. AAD (service+key) doesn't match — must fail.
	_, err = s.Get("svc-b", "k1")
	if err == nil {
		t.Fatal("expected decrypt error after file-swap, got nil")
	}
}

func TestEntryFile_RejectsTooShort(t *testing.T) {
	s := tempStore(t)

	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := s.entryPath("svc", "k")
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := s.Get("svc", "k")
	if err == nil {
		t.Fatal("expected error on too-short body")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("expected 'too short' in error, got %v", err)
	}
}

func TestSet_EmptyServiceOrKey_Rejected(t *testing.T) {
	s := tempStore(t)

	if err := s.Set("", "k", "v"); err == nil {
		t.Error("Set with empty service should error")
	}
	if err := s.Set("svc", "", "v"); err == nil {
		t.Error("Set with empty key should error")
	}
}

func TestConcurrent_SetGet(t *testing.T) {
	s := tempStore(t)

	const N = 50
	var wg sync.WaitGroup
	wg.Add(N * 2)

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			if err := s.Set("svc", "k", "value"); err != nil {
				t.Errorf("concurrent Set %d: %v", i, err)
			}
		}()
		go func() {
			defer wg.Done()
			// May see ErrNotFound if read happens before any write
			// has landed; tolerate that.
			if _, err := s.Get("svc", "k"); err != nil && !errors.Is(err, ErrNotFound) {
				t.Errorf("concurrent Get %d: %v", i, err)
			}
		}()
	}
	wg.Wait()

	// Final state must be readable.
	v, err := s.Get("svc", "k")
	if err != nil {
		t.Fatalf("final Get: %v", err)
	}
	if v != "value" {
		t.Errorf("final v = %q, want value", v)
	}
}

func TestNewFileStore_HKDFDerivation(t *testing.T) {
	// Sanity-check that the production constructor's keyDeriver
	// produces a 32-byte key deterministic in the machine ID.
	dir := t.TempDir()
	calls := 0
	machineID := func() (string, error) {
		calls++
		return "test-machine-id-deadbeef", nil
	}
	store := NewFileStore(dir, machineID)
	fs, ok := store.(*fileStore)
	if !ok {
		t.Fatalf("expected *fileStore, got %T", store)
	}

	k1, err := fs.keySrc()
	if err != nil {
		t.Fatalf("keySrc: %v", err)
	}
	k2, err := fs.keySrc()
	if err != nil {
		t.Fatalf("keySrc 2: %v", err)
	}
	if len(k1) != 32 {
		t.Errorf("expected 32-byte key, got %d", len(k1))
	}
	if string(k1) != string(k2) {
		t.Error("key derivation is not deterministic")
	}
	if calls != 2 {
		t.Errorf("expected machineID called twice (once per keySrc), got %d", calls)
	}
}

func TestNewFileStore_EmptyMachineID_Errors(t *testing.T) {
	dir := t.TempDir()
	store := NewFileStore(dir, func() (string, error) { return "", nil })
	if err := store.Set("svc", "k", "v"); err == nil {
		t.Error("expected error when machine ID is empty")
	}
}

func TestDefaultDir_SubdirOfConfigDir(t *testing.T) {
	got := DefaultDir("/tmp/agent")
	want := filepath.Join("/tmp/agent", "secrets")
	if got != want {
		t.Errorf("DefaultDir = %q, want %q", got, want)
	}
}

func TestSetAndGet_AcrossStoreInstances(t *testing.T) {
	// Critical for the "agentctl pair writes, daemon reads" pattern:
	// two separate fileStore instances pointed at the same dir and
	// derived from the same key MUST round-trip secrets between them.
	// Without this, the v0.1.2 cross-process-cross-user secret share
	// is just an unverified claim.
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand: %v", err)
	}

	writer := newFileStoreWithKey(dir, key)
	if err := writer.Set("svc", "k", "shared-secret"); err != nil {
		t.Fatalf("writer.Set: %v", err)
	}

	// Brand new store instance, same dir, same key (simulating
	// agentctl process exit + daemon process start).
	reader := newFileStoreWithKey(dir, key)
	v, err := reader.Get("svc", "k")
	if err != nil {
		t.Fatalf("reader.Get: %v", err)
	}
	if v != "shared-secret" {
		t.Errorf("got %q, want shared-secret", v)
	}
}

func TestSetAndGet_DifferentKey_FailsDecrypt(t *testing.T) {
	// If two boxes have different machine IDs, the same secrets
	// directory restored from a backup MUST NOT decrypt — that's the
	// "stolen disk" threat the machine-id binding protects against.
	dir := t.TempDir()

	keyA := make([]byte, 32)
	keyB := make([]byte, 32)
	if _, err := rand.Read(keyA); err != nil {
		t.Fatalf("rand A: %v", err)
	}
	if _, err := rand.Read(keyB); err != nil {
		t.Fatalf("rand B: %v", err)
	}

	writer := newFileStoreWithKey(dir, keyA)
	if err := writer.Set("svc", "k", "secret"); err != nil {
		t.Fatalf("writer.Set: %v", err)
	}

	// Different key = different box. Read attempt MUST fail.
	reader := newFileStoreWithKey(dir, keyB)
	_, err := reader.Get("svc", "k")
	if err == nil {
		t.Fatal("expected decrypt failure with different key, got nil")
	}
	if !strings.Contains(err.Error(), "decrypt") {
		t.Errorf("expected decrypt-flavoured error, got %v", err)
	}
}

func TestReadMachineID_ReturnsNonEmpty(t *testing.T) {
	// Smoke test for the platform-specific reader. On the dev machine
	// (Mac) this hits ioreg; on Linux runners /etc/machine-id; on
	// Windows the registry. Just confirm we get something back — the
	// value itself is per-machine and not assertable.
	id, err := ReadMachineID()
	if err != nil {
		t.Skipf("readPlatformMachineID unavailable in this env: %v", err)
	}
	if id == "" {
		t.Errorf("ReadMachineID returned empty string with nil error")
	}
}

// TestSet_FatalWhenApplyDirACLFails confirms the v0.1.10 behavior
// change: applyDirACL failure must propagate up from Set rather than
// log-and-continue. Pre-v0.1.10 behavior (fmt.Fprintf to os.Stderr +
// proceed with write) caused the DIPL Delhi pilot incident — the
// secret file landed in a directory whose DACL the LocalSystem
// service couldn't traverse, and the agent silently 401-looped on
// every heartbeat.
func TestSet_FatalWhenApplyDirACLFails(t *testing.T) {
	s := tempStore(t)

	// Inject a failing applyDirACLFn for the duration of this test.
	// Mirrors what would happen if icacls was blocked by AV / GPO /
	// container minus icacls.exe.
	prev := applyDirACLFn
	applyDirACLFn = func(string) error {
		return errors.New("simulated icacls failure: GPO blocks SeRestorePrivilege")
	}
	t.Cleanup(func() { applyDirACLFn = prev })

	err := s.Set("svc", "k", "v")
	if err == nil {
		t.Fatal("Set with failing applyDirACL must return error; got nil")
	}
	if !strings.Contains(err.Error(), "applyDirACL") {
		t.Errorf("error should reference applyDirACL; got %v", err)
	}
	if !strings.Contains(err.Error(), "simulated icacls failure") {
		t.Errorf("error should wrap underlying applyDirACL error; got %v", err)
	}

	// And the secret must NOT have been written — verify Get returns
	// ErrNotFound. Pre-v0.1.10 the file would have been written
	// despite the ACL warning, leaving the keyring half-broken.
	if _, getErr := s.Get("svc", "k"); !errors.Is(getErr, ErrNotFound) {
		t.Errorf("after failed Set, Get should be ErrNotFound; got %v", getErr)
	}
}

// TestSet_WriteVerifyDetectsCorruption confirms the v0.1.10
// write-verify roundtrip catches the case where Set wrote bytes to
// disk but the host environment (EFS, AV, GPO) blocks the immediate
// read-back. Without write-verify, pair returns success but the
// service hits the read-failure on its first heartbeat 60s later
// with no actionable error trail back to the operator.
//
// We simulate read-failure by deleting the file between Set's
// rename step and Set's verify step. We can't intercept Set
// mid-flight cleanly, so instead we drive the path via a custom
// keyDeriver that returns one key on the Set encrypt path and a
// different key on the verify decrypt path — same effect: verify
// fails because decrypt fails.
func TestSet_WriteVerifyDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	encKey := make([]byte, 32)
	if _, err := rand.Read(encKey); err != nil {
		t.Fatalf("rand: %v", err)
	}
	verifyKey := make([]byte, 32)
	if _, err := rand.Read(verifyKey); err != nil {
		t.Fatalf("rand: %v", err)
	}

	// keyDeriver alternates encKey (first call: encrypt) and verifyKey
	// (second call: read-back decrypt) — the second call is in the
	// write-verify path and should fail decryption.
	calls := 0
	deriver := func() ([]byte, error) {
		calls++
		fresh := make([]byte, 32)
		switch calls {
		case 1:
			copy(fresh, encKey)
		default:
			copy(fresh, verifyKey)
		}
		return fresh, nil
	}
	s := &fileStore{dir: dir, keySrc: deriver}

	err := s.Set("svc", "k", "v")
	if err == nil {
		t.Fatal("Set with mismatched encrypt/verify keys must return error; got nil")
	}
	if !strings.Contains(err.Error(), "write-verify") {
		t.Errorf("error should reference write-verify; got %v", err)
	}
}

// TestSet_WriteVerifyRoundtrip confirms the happy path still works
// — write-verify shouldn't break Set on a normal store, and a Get
// after Set returns the original value.
func TestSet_WriteVerifyRoundtrip(t *testing.T) {
	s := tempStore(t)

	if err := s.Set("svc", "k", "secret-value-123"); err != nil {
		t.Fatalf("Set on healthy store must succeed: %v", err)
	}
	v, err := s.Get("svc", "k")
	if err != nil {
		t.Fatalf("Get after Set: %v", err)
	}
	if v != "secret-value-123" {
		t.Errorf("Get returned %q, want %q", v, "secret-value-123")
	}
}

// TestApplyDirACLFnSeam confirms that applyDirACLFn can be swapped
// at the package level — protects the seam from accidental removal
// in future refactors. The same pattern is used by the test above
// for the fatal-fail path.
func TestApplyDirACLFnSeam(t *testing.T) {
	prev := applyDirACLFn
	t.Cleanup(func() { applyDirACLFn = prev })

	called := 0
	applyDirACLFn = func(dir string) error {
		called++
		return nil
	}
	s := tempStore(t)
	if err := s.Set("svc", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if called == 0 {
		t.Error("expected applyDirACLFn to be invoked from Set; got 0 calls")
	}
}

func TestSet_ReappliesACLAfterRename(t *testing.T) {
	prev := applyDirACLFn
	t.Cleanup(func() { applyDirACLFn = prev })

	var calls []string
	applyDirACLFn = func(dir string) error {
		calls = append(calls, dir)
		return nil
	}
	s := tempStore(t)
	if err := s.Set("svc", "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("applyDirACLFn calls = %d, want 2", len(calls))
	}
	if calls[0] != calls[1] {
		t.Fatalf("applyDirACLFn dirs differ: %q vs %q", calls[0], calls[1])
	}
}
