// Package config persists the agent's non-secret state (server URL,
// connection id, device name, pairing timestamp) to a YAML file under
// %ProgramData%\GST Reco\agent\ on Windows or ~/.gstreco-agent/ elsewhere.
// Secrets (HMAC key, bearer token) live in the OS keyring — see package
// keyring.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the config filename within the config directory. Kept short so
// it fits in Windows' credential-manager diagnostics cleanly.
const FileName = "config.yaml"

// Config is the on-disk shape of the agent's persistent state. Fields are
// written lowercase (matching the YAML struct tags) so a user editing the
// file by hand doesn't have to guess capitalisation. Zero values indicate an
// unpaired agent.
type Config struct {
	// Server is the base URL of GST Reco (e.g. https://gstreco.m2ai.ai). The
	// pair flow writes this from the --server flag; the ingest client reads
	// it to construct request URLs.
	Server string `yaml:"server,omitempty"`
	// ConnectionID is the UUID the server allocated when the agent paired.
	// Used as the x-tally-connection-id header on every signed request and as
	// the keyring key prefix.
	ConnectionID string `yaml:"connection_id,omitempty"`
	// DeviceName is what the agent told the server at pair time. Usually the
	// OS hostname. Surfaced in the server's /settings/tally connection list.
	DeviceName string `yaml:"device_name,omitempty"`
	// PairedAt is when the pair flow completed, in RFC-3339 format. Purely
	// informational — the server has its own authoritative timestamp.
	PairedAt time.Time `yaml:"paired_at,omitempty"`
	// TallyEndpoint overrides http://localhost:9000 if the user changed
	// Tally's TCP port. Blank = default.
	TallyEndpoint string `yaml:"tally_endpoint,omitempty"`
	// LogLevel is "debug" | "info" | "warn" | "error". Blank = info.
	LogLevel string `yaml:"log_level,omitempty"`
}

// ErrNotFound is returned by Load when the config file doesn't exist yet —
// i.e. the agent has never been paired. Callers distinguish this from
// corrupted-YAML errors to surface a clean "run `agentctl pair` first"
// message.
var ErrNotFound = errors.New("config: file not found")

// IsPaired reports whether the config contains enough state for the ingest
// client to sign a request. Missing Server or ConnectionID means the user
// still needs to run `agentctl pair`.
func (c *Config) IsPaired() bool {
	return c != nil && c.Server != "" && c.ConnectionID != ""
}

// DefaultDir returns the platform-appropriate config directory. Windows
// agents install service-wide to %ProgramData% so every user on the machine
// sees the same pairing; non-Windows dev boxes use the invoking user's home
// to avoid needing sudo for `make run`.
func DefaultDir() string {
	if runtime.GOOS == "windows" {
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "GST Reco", "agent")
		}
		return `C:\ProgramData\GST Reco\agent`
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "gstreco-agent")
	}
	return filepath.Join(home, ".gstreco-agent")
}

// DefaultPath returns DefaultDir + FileName.
func DefaultPath() string {
	return filepath.Join(DefaultDir(), FileName)
}

// Load reads the config from path. Returns ErrNotFound (wrapped) if the file
// is missing so callers can distinguish "never paired" from "file corrupted".
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return &c, nil
}

// Save writes the config atomically: writes to a sibling .tmp file then
// renames into place so a power loss mid-write can't leave a half-formatted
// YAML file. Directory is created with 0o755 (world-readable) since the file
// holds no secrets and other users on the same Windows box (service account,
// user account) need to be able to read it.
func Save(path string, c *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}
	body, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return fmt.Errorf("config: write temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: rename %s → %s: %w", tmp, path, err)
	}
	return nil
}
