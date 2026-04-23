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
