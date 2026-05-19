package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	want := &Config{
		Server:        "https://gstreco.m2ai.ai",
		ConnectionID:  "11111111-2222-3333-4444-555555555555",
		CompanyID:     "company-1",
		DeviceName:    "test-laptop",
		PairedAt:      time.Date(2026, 4, 23, 10, 30, 0, 0, time.UTC),
		TallyEndpoint: "http://localhost:9000",
		LogLevel:      "info",
	}
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Server != want.Server || got.ConnectionID != want.ConnectionID ||
		got.CompanyID != want.CompanyID ||
		got.DeviceName != want.DeviceName || got.TallyEndpoint != want.TallyEndpoint ||
		got.LogLevel != want.LogLevel {
		t.Errorf("round-trip mismatch: got=%+v want=%+v", got, want)
	}
	if !got.PairedAt.Equal(want.PairedAt) {
		t.Errorf("PairedAt: got=%s want=%s", got.PairedAt, want.PairedAt)
	}
	if !got.IsPaired() {
		t.Error("IsPaired() = false on valid config")
	}
}

func TestLoadMissingFileReturnsErrNotFound(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "never-written.yaml"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want wraps ErrNotFound", err)
	}
}

func TestLoadCorruptedFileReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("not: valid: yaml: : :"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	if errors.Is(err, ErrNotFound) {
		t.Error("corrupted file must NOT be reported as ErrNotFound — caller behaviour differs")
	}
}

func TestIsPaired(t *testing.T) {
	cases := []struct {
		name string
		c    *Config
		want bool
	}{
		{"nil", nil, false},
		{"zero", &Config{}, false},
		{"only server", &Config{Server: "https://x"}, false},
		{"only id", &Config{ConnectionID: "abc"}, false},
		{"both", &Config{Server: "https://x", ConnectionID: "abc"}, true},
		{"connections", &Config{Connections: []PairedConnection{{Server: "https://x", ConnectionID: "abc"}}}, true},
	}
	for _, tc := range cases {
		if got := tc.c.IsPaired(); got != tc.want {
			t.Errorf("%s: IsPaired=%v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSaveAtomicCreatesDir(t *testing.T) {
	// Save must mkdir -p the parent; agents install under %ProgramData%\GST
	// Reco\agent which the first pair flow creates.
	nested := filepath.Join(t.TempDir(), "a", "b", "c", "config.yaml")
	if err := Save(nested, &Config{Server: "https://x", ConnectionID: "y"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(nested)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() == 0 {
		t.Error("saved file is empty")
	}
}

func TestSaveDoesNotLeaveTempOnSuccess(t *testing.T) {
	// Successful Save renames .tmp → path atomically; the .tmp file must not
	// be left behind for a user or backup tool to pick up.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := Save(path, &Config{Server: "https://x", ConnectionID: "y"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf(".tmp leaked after successful Save: %v", err)
	}
}

func TestDefaultDirReturnsNonEmpty(t *testing.T) {
	dir := DefaultDir()
	if dir == "" {
		t.Error("DefaultDir() returned empty")
	}
	if !strings.Contains(dir, "gstreco") && !strings.Contains(dir, "GST Reco") {
		t.Errorf("DefaultDir() = %q, expected to include gstreco/GST Reco", dir)
	}
}

func TestPairedConnections_FallsBackToLegacyFields(t *testing.T) {
	cfg := &Config{
		Server:       "https://x",
		ConnectionID: "conn-1",
		CompanyID:    "company-1",
		DeviceName:   "box-1",
		PairedAt:     time.Date(2026, 5, 15, 10, 0, 0, 0, time.UTC),
	}
	got := cfg.PairedConnections()
	if len(got) != 1 {
		t.Fatalf("len(PairedConnections) = %d, want 1", len(got))
	}
	if got[0].ConnectionID != "conn-1" || got[0].Server != "https://x" || got[0].CompanyID != "company-1" {
		t.Fatalf("unexpected legacy connection: %+v", got[0])
	}
}

func TestPairedConnections_PrefersCanonicalConnectionsList(t *testing.T) {
	cfg := &Config{
		Server:       "https://legacy",
		ConnectionID: "legacy-conn",
		Connections: []PairedConnection{
			{Server: "https://a", ConnectionID: "conn-a", CompanyID: "company-a"},
			{Server: "https://b", ConnectionID: "conn-b", CompanyID: "company-b"},
		},
	}
	got := cfg.PairedConnections()
	if len(got) != 2 {
		t.Fatalf("len(PairedConnections) = %d, want 2", len(got))
	}
	if got[0].ConnectionID != "conn-a" || got[1].ConnectionID != "conn-b" {
		t.Fatalf("unexpected canonical order: %+v", got)
	}
}

func TestSetPairedConnections_MirrorsFirstConnectionToLegacyFields(t *testing.T) {
	cfg := &Config{}
	wantFirst := PairedConnection{
		Server:       "https://a",
		ConnectionID: "conn-a",
		CompanyID:    "company-a",
		DeviceName:   "box-a",
		PairedAt:     time.Date(2026, 5, 15, 11, 0, 0, 0, time.UTC),
	}
	cfg.SetPairedConnections([]PairedConnection{
		wantFirst,
		{Server: "https://b", ConnectionID: "conn-b", CompanyID: "company-b"},
	})
	if cfg.Server != wantFirst.Server || cfg.ConnectionID != wantFirst.ConnectionID || cfg.CompanyID != wantFirst.CompanyID {
		t.Fatalf("legacy mirror mismatch: %+v", cfg)
	}
	if len(cfg.Connections) != 2 {
		t.Fatalf("len(Connections) = %d, want 2", len(cfg.Connections))
	}
}

func TestUpsertPairedConnection_AppendsAndReplacesByConnectionID(t *testing.T) {
	cfg := &Config{}
	cfg.UpsertPairedConnection(PairedConnection{Server: "https://a", ConnectionID: "conn-a", CompanyID: "company-a"})
	cfg.UpsertPairedConnection(PairedConnection{Server: "https://b", ConnectionID: "conn-b", CompanyID: "company-b"})
	cfg.UpsertPairedConnection(PairedConnection{Server: "https://a2", ConnectionID: "conn-a", CompanyID: "company-a2"})

	got := cfg.PairedConnections()
	if len(got) != 2 {
		t.Fatalf("len(PairedConnections) = %d, want 2", len(got))
	}
	if got[0].Server != "https://a2" || got[0].CompanyID != "company-a2" {
		t.Fatalf("first connection not replaced: %+v", got[0])
	}
	if got[1].ConnectionID != "conn-b" {
		t.Fatalf("second connection changed unexpectedly: %+v", got[1])
	}
}
