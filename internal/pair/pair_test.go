package pair

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
)

func TestCanonicalizePairCode(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"ABCD23", "ABCD23", false},
		{"abcd23", "ABCD23", false},
		{"ABC-D23", "ABCD23", false},
		{"  ABC-D23  ", "ABCD23", false},
		{"ABC D23", "ABCD23", false},
		{"ABCDEO", "", true},  // O not in Crockford
		{"ABCDEI", "", true},  // I not in Crockford
		{"ABCDEL", "", true},  // L not in Crockford
		{"ABCDEU", "", true},  // U not in Crockford
		{"ABC", "", true},     // too short
		{"ABCDEFG", "", true}, // too long
		{"", "", true},
	}
	for _, tc := range cases {
		got, err := canonicalizePairCode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("canonicalizePairCode(%q): want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("canonicalizePairCode(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("canonicalizePairCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCanonicalizePairCode_AcceptsFullCrockfordAlphabet(t *testing.T) {
	// "0Z-9H4" is 0, Z, 9, H, 4 + one more — let's build a 6-char sample.
	for _, in := range []string{"0123AB", "VWXYZ0", "HJKMNP", "QRSTVW"} {
		if _, err := canonicalizePairCode(in); err != nil {
			t.Errorf("canonicalizePairCode(%q) rejected a valid code: %v", in, err)
		}
	}
}

func TestClaim_Success(t *testing.T) {
	// Fake server returns a valid ClaimResponse.
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tally/pair/claim" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_ = json.NewEncoder(w).Encode(ClaimResponse{
			ConnectionID: "conn-123",
			CompanyID:    "company-456",
			Token:        "bearer-token-raw",
			HmacSecret:   "aGVsbG8gd29ybGQ=",
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	ks := keyring.NewMemoryStore()

	resp, err := Claim(context.Background(), Options{
		Server:       srv.URL,
		Code:         "abc-d23",
		ConfigPath:   cfgPath,
		Keyring:      ks,
		DeviceName:   "test-laptop",
		AgentVersion: "v0.1.0",
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if resp.ConnectionID != "conn-123" {
		t.Errorf("ConnectionID = %q", resp.ConnectionID)
	}

	// Request normalised the code to canonical form.
	if code, _ := gotBody["code"].(string); code != "ABCD23" {
		t.Errorf("server received code = %q, want ABCD23", code)
	}
	if dn, _ := gotBody["device_name"].(string); dn != "test-laptop" {
		t.Errorf("server received device_name = %q", dn)
	}

	// Config persisted.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if !cfg.IsPaired() {
		t.Error("config not marked paired")
	}
	if cfg.ConnectionID != "conn-123" {
		t.Errorf("cfg.ConnectionID = %q", cfg.ConnectionID)
	}
	if cfg.Server != srv.URL {
		t.Errorf("cfg.Server = %q, want %q", cfg.Server, srv.URL)
	}
	if len(cfg.PairedConnections()) != 1 {
		t.Fatalf("len(PairedConnections) = %d, want 1", len(cfg.PairedConnections()))
	}
	if cfg.PairedConnections()[0].CompanyID != "company-456" {
		t.Errorf("company_id = %q, want company-456", cfg.PairedConnections()[0].CompanyID)
	}

	// Keyring persisted both secrets.
	hmacKey, bearerKey := keyring.ConnectionKeys("conn-123")
	if v, _ := ks.Get(keyring.ServiceName, hmacKey); v != "aGVsbG8gd29ybGQ=" {
		t.Errorf("hmac_secret in keyring = %q", v)
	}
	if v, _ := ks.Get(keyring.ServiceName, bearerKey); v != "bearer-token-raw" {
		t.Errorf("bearer token in keyring = %q", v)
	}
}

func TestClaim_AppendsAdditionalConnectionToExistingConfig(t *testing.T) {
	var claimCalls int
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := config.Save(cfgPath, &config.Config{
		Server:       "https://x",
		ConnectionID: "existing",
		CompanyID:    "company-existing",
		Connections: []config.PairedConnection{
			{Server: "https://x", ConnectionID: "existing", CompanyID: "company-existing"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		claimCalls++
		_ = json.NewEncoder(w).Encode(ClaimResponse{
			ConnectionID: "conn-new",
			CompanyID:    "company-new",
			Token:        "token-new",
			HmacSecret:   "c2VjcmV0LW5ldw==",
		})
	}))
	defer srv.Close()

	if _, err := Claim(context.Background(), Options{
		Server:     srv.URL,
		Code:       "ABCDEF",
		ConfigPath: cfgPath,
		Keyring:    keyring.NewMemoryStore(),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimCalls != 1 {
		t.Fatalf("claim server called %d times, want 1", claimCalls)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	got := cfg.PairedConnections()
	if len(got) != 2 {
		t.Fatalf("len(PairedConnections) = %d, want 2", len(got))
	}
	if got[0].ConnectionID != "existing" || got[1].ConnectionID != "conn-new" {
		t.Fatalf("unexpected connections after append: %+v", got)
	}
}

func TestClaim_ReplacesExistingConnectionByConnectionID(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := config.Save(cfgPath, &config.Config{
		Server:       "https://x",
		ConnectionID: "conn-123",
		CompanyID:    "company-old",
		Connections: []config.PairedConnection{
			{Server: "https://x", ConnectionID: "conn-123", CompanyID: "company-old", DeviceName: "box-old"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ClaimResponse{
			ConnectionID: "conn-123",
			CompanyID:    "company-new",
			Token:        "token-new",
			HmacSecret:   "c2VjcmV0LW5ldw==",
		})
	}))
	defer srv.Close()

	if _, err := Claim(context.Background(), Options{
		Server:       srv.URL,
		Code:         "ABCDEF",
		ConfigPath:   cfgPath,
		Keyring:      keyring.NewMemoryStore(),
		DeviceName:   "box-new",
		AgentVersion: "v0.1.21",
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	got := cfg.PairedConnections()
	if len(got) != 1 {
		t.Fatalf("len(PairedConnections) = %d, want 1", len(got))
	}
	if got[0].Server != srv.URL || got[0].CompanyID != "company-new" || got[0].DeviceName != "box-new" {
		t.Fatalf("connection not replaced: %+v", got[0])
	}
}

func TestClaim_LoadConfigErrorStillFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(cfgPath, []byte("not: valid: yaml: : :"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Claim(context.Background(), Options{
		Server:     "https://irrelevant",
		Code:       "ABCDEF",
		ConfigPath: cfgPath,
		Keyring:    keyring.NewMemoryStore(),
	})
	if err == nil {
		t.Fatal("expected config load error")
	}
}

func TestClaim_410Gone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "code_gone"})
	}))
	defer srv.Close()

	_, err := Claim(context.Background(), Options{
		Server:     srv.URL,
		Code:       "ABCDEF",
		ConfigPath: filepath.Join(t.TempDir(), "c.yaml"),
		Keyring:    keyring.NewMemoryStore(),
	})
	if !errors.Is(err, ErrCodeGone) {
		t.Errorf("err = %v, want ErrCodeGone", err)
	}
}

func TestClaim_400InvalidCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := Claim(context.Background(), Options{
		Server:     srv.URL,
		Code:       "ABCDEF",
		ConfigPath: filepath.Join(t.TempDir(), "c.yaml"),
		Keyring:    keyring.NewMemoryStore(),
	})
	if !errors.Is(err, ErrInvalidCode) {
		t.Errorf("err = %v, want ErrInvalidCode", err)
	}
}

func TestClaim_BadCodeFormatFailsLocally(t *testing.T) {
	// "ABCDEL" contains L which isn't in Crockford — we should fail before
	// the network round-trip.
	handlerCalled := false
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		handlerCalled = true
	}))
	defer srv.Close()

	_, err := Claim(context.Background(), Options{
		Server:     srv.URL,
		Code:       "ABCDEL",
		ConfigPath: filepath.Join(t.TempDir(), "c.yaml"),
		Keyring:    keyring.NewMemoryStore(),
	})
	if !errors.Is(err, ErrInvalidCode) {
		t.Errorf("err = %v, want ErrInvalidCode", err)
	}
	if handlerCalled {
		t.Error("server handler was called; canonicalize should short-circuit locally")
	}
}

func TestClaim_ValidatesOptions(t *testing.T) {
	cases := []struct {
		name string
		o    Options
	}{
		{"missing server", Options{Code: "ABCDEF", Keyring: keyring.NewMemoryStore()}},
		{"missing keyring", Options{Server: "https://x", Code: "ABCDEF"}},
	}
	for _, tc := range cases {
		if _, err := Claim(context.Background(), tc.o); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

func TestClaim_KeyringFailureLeavesConfigUntouched(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ClaimResponse{
			ConnectionID: "conn-abc",
			CompanyID:    "co-abc",
			Token:        "t",
			HmacSecret:   "s",
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// failingStore returns an error on Set so we can verify rollback.
	ks := &failingStore{err: errors.New("keychain locked")}

	_, err := Claim(context.Background(), Options{
		Server:     srv.URL,
		Code:       "ABCDEF",
		ConfigPath: cfgPath,
		Keyring:    ks,
	})
	if err == nil {
		t.Fatal("expected keyring error")
	}
	if !strings.Contains(err.Error(), "hmac") {
		t.Errorf("err = %v, want mention of hmac secret storage", err)
	}
	if _, err := config.Load(cfgPath); !errors.Is(err, config.ErrNotFound) {
		t.Error("config was written despite keyring failure — must stay unpaired")
	}
}

type failingStore struct{ err error }

func (f *failingStore) Set(_, _, _ string) error        { return f.err }
func (f *failingStore) Get(_, _ string) (string, error) { return "", keyring.ErrNotFound }
func (f *failingStore) Delete(_, _ string) error        { return nil }
