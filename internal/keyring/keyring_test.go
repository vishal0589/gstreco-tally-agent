package keyring

import (
	"errors"
	"sync"
	"testing"
)

func TestMemoryStore_SetGetDelete(t *testing.T) {
	s := NewMemoryStore()
	const svc = "test-svc"

	if _, err := s.Get(svc, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: err=%v, want ErrNotFound", err)
	}

	if err := s.Set(svc, "k", "v1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Get(svc, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "v1" {
		t.Errorf("Get = %q, want v1", got)
	}

	// Overwrite
	if err := s.Set(svc, "k", "v2"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if got, _ := s.Get(svc, "k"); got != "v2" {
		t.Errorf("after overwrite Get = %q, want v2", got)
	}

	if err := s.Delete(svc, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(svc, "k"); !errors.Is(err, ErrNotFound) {
		t.Errorf("after Delete Get: err=%v, want ErrNotFound", err)
	}

	// Idempotent delete
	if err := s.Delete(svc, "k"); err != nil {
		t.Errorf("Delete missing: %v, want nil (idempotent)", err)
	}
}

func TestMemoryStore_ServiceNamespacing(t *testing.T) {
	s := NewMemoryStore()
	_ = s.Set("svc1", "shared-key", "v1")
	_ = s.Set("svc2", "shared-key", "v2")

	v1, _ := s.Get("svc1", "shared-key")
	v2, _ := s.Get("svc2", "shared-key")
	if v1 != "v1" || v2 != "v2" {
		t.Errorf("service isolation broken: svc1=%q svc2=%q", v1, v2)
	}
}

func TestMemoryStore_ConcurrentAccess(t *testing.T) {
	// Race detector catches mutex misuse.
	s := NewMemoryStore()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = s.Set("svc", "k", "v")
			_, _ = s.Get("svc", "k")
			_ = s.Delete("svc", "k")
			_ = i
		}(i)
	}
	wg.Wait()
}

func TestConnectionKeys(t *testing.T) {
	hmac, bearer := ConnectionKeys("abc-123")
	if hmac == bearer {
		t.Error("hmac and bearer key names must differ")
	}
	if hmac != "abc-123.hmac_secret" || bearer != "abc-123.bearer_token" {
		t.Errorf("ConnectionKeys unexpected: hmac=%q bearer=%q", hmac, bearer)
	}
}
