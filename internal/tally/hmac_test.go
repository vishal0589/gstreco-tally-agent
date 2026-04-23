package tally

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestCanonicalStringShape(t *testing.T) {
	got := CanonicalString("POST", "/api/tally/ingest", "1700000000", "deadbeef", []byte(`{"x":1}`))
	lines := strings.Split(got, "\n")
	if len(lines) != 5 {
		t.Fatalf("canonical has %d lines, want 5:\n%s", len(lines), got)
	}
	if lines[0] != "POST" {
		t.Errorf("method line = %q", lines[0])
	}
	if lines[1] != "/api/tally/ingest" {
		t.Errorf("path line = %q", lines[1])
	}
	if lines[2] != "1700000000" {
		t.Errorf("timestamp line = %q", lines[2])
	}
	if lines[3] != "deadbeef" {
		t.Errorf("nonce line = %q", lines[3])
	}
	wantBody := sha256.Sum256([]byte(`{"x":1}`))
	if lines[4] != hex.EncodeToString(wantBody[:]) {
		t.Errorf("body hash mismatch: got %q", lines[4])
	}
}

func TestSignMatchesRawHMAC(t *testing.T) {
	secret := []byte("super-secret-key-bytes")
	body := []byte(`{"run_id":"r1"}`)
	got := Sign(secret, "POST", "/x", "100", "n", body)

	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte("POST\n/x\n100\nn\n" + hex.EncodeToString(sha256.New().Sum(nil)))) // wrong: sha(body)
	// Compute the correct reference:
	bodyH := sha256.Sum256(body)
	ref := hmac.New(sha256.New, secret)
	ref.Write([]byte("POST\n/x\n100\nn\n" + hex.EncodeToString(bodyH[:])))
	want := hex.EncodeToString(ref.Sum(nil))

	if got != want {
		t.Errorf("Sign:\n got  %s\n want %s", got, want)
	}
}

func TestSignIsDeterministic(t *testing.T) {
	secret := []byte("k")
	body := []byte(`{}`)
	a := Sign(secret, "POST", "/p", "1", "n", body)
	b := Sign(secret, "POST", "/p", "1", "n", body)
	if a != b {
		t.Errorf("non-deterministic Sign: %s != %s", a, b)
	}
}

func TestNewNonceShape(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 16; i++ {
		n, err := NewNonce()
		if err != nil {
			t.Fatalf("NewNonce: %v", err)
		}
		if len(n) != NonceBytes*2 {
			t.Errorf("nonce length = %d, want %d hex chars", len(n), NonceBytes*2)
		}
		if _, err := hex.DecodeString(n); err != nil {
			t.Errorf("nonce not hex: %q", n)
		}
		if seen[n] {
			t.Errorf("duplicate nonce %q — rand read is broken", n)
		}
		seen[n] = true
	}
}

func TestNowTimestampSeconds(t *testing.T) {
	now := time.Date(2026, 4, 23, 10, 30, 5, 999_000_000, time.UTC)
	// Unix seconds — nanoseconds must not leak in. Value verified against
	// `date -u -d '2026-04-23 10:30:05' +%s` = 1776940205.
	if got := NowTimestamp(now); got != "1776940205" {
		t.Errorf("NowTimestamp = %s, want 1776940205", got)
	}
}
