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
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// FileName is the config filename within the config directory. Kept short so
// it fits in Windows' credential-manager diagnostics cleanly.
const FileName = "config.yaml"

// PairedConnection is one server-side GST Reco connection bound to this
// machine. v0.1.20 and earlier stored only the first connection in the
// top-level Config fields; v0.1.21+ stores all connections here while
// mirroring the first entry back into the legacy top-level fields so older
// binaries degrade to "first connection only" instead of failing to load.
type PairedConnection struct {
	Server       string    `yaml:"server,omitempty"`
	ConnectionID string    `yaml:"connection_id,omitempty"`
	CompanyID    string    `yaml:"company_id,omitempty"`
	DeviceName   string    `yaml:"device_name,omitempty"`
	PairedAt     time.Time `yaml:"paired_at,omitempty"`
}

func (c PairedConnection) normalized() PairedConnection {
	c.Server = strings.TrimSpace(c.Server)
	c.ConnectionID = strings.TrimSpace(c.ConnectionID)
	c.CompanyID = strings.TrimSpace(c.CompanyID)
	c.DeviceName = strings.TrimSpace(c.DeviceName)
	return c
}

func (c PairedConnection) IsValid() bool {
	c = c.normalized()
	return c.Server != "" && c.ConnectionID != ""
}

// Config is the on-disk shape of the agent's persistent state. Fields are
// written lowercase (matching the YAML struct tags) so a user editing the
// file by hand doesn't have to guess capitalisation. Zero values indicate an
// unpaired agent.
type Config struct {
	// Legacy primary-connection fields. Kept for back-compat with older
	// binaries that only understand a single paired connection.
	Server       string    `yaml:"server,omitempty"`
	ConnectionID string    `yaml:"connection_id,omitempty"`
	CompanyID    string    `yaml:"company_id,omitempty"`
	DeviceName   string    `yaml:"device_name,omitempty"`
	PairedAt     time.Time `yaml:"paired_at,omitempty"`
	// Connections is the canonical multi-tenant pairing list.
	Connections []PairedConnection `yaml:"connections,omitempty"`
	// TallyEndpoint is the legacy single-endpoint field. Kept for back-
	// compat with pre-A5-2 configs that paired against one Tally instance
	// on the default port. New installs use TallyEndpoints (plural). The
	// resolver below promotes this into TallyEndpoints when the latter is
	// empty so any old config stays valid.
	TallyEndpoint string `yaml:"tally_endpoint,omitempty"`
	// TallyEndpoints is the multi-port list populated by `agentctl
	// discover` (A5-2). Each Tally Prime instance binds a different port,
	// so a single Windows box can host 5–6 endpoints under one agent
	// install while still storing multiple tenant pairings. Empty = fall
	// back to TallyEndpoint, then to
	// tally.DefaultEndpoint.
	TallyEndpoints []string `yaml:"tally_endpoints,omitempty"`
	// LogLevel is "debug" | "info" | "warn" | "error". Blank = info.
	LogLevel string `yaml:"log_level,omitempty"`
	// ScheduleCron is the cron expression the daemon (A5) uses for
	// scheduled syncs. Empty = scheduler.DefaultSpec
	// ("0 11,13,15,17 * * *" — fires at 11am/1pm/3pm/5pm in the
	// agent's local timezone, typically IST for the CA market).
	// Pre-v0.1.12 the default was "0 2 * * *" (overnight); changed
	// after the 2026-04-29 DIPL incident demonstrated that overnight
	// ticks silently waited 24 hours when Tally was closed at the
	// scheduled minute. Server-side `tally_connections.schedule_cron`
	// is the authoritative source; the agent will sync this from
	// heartbeat once B3 ships, but for V1 the local override here is
	// the only knob.
	ScheduleCron string `yaml:"schedule_cron,omitempty"`
}

// ResolveTallyEndpoints returns the effective list of Tally HTTP
// endpoints the agent should poll. Order of preference:
//
//  1. TallyEndpoints (A5-2 multi-port config)
//  2. TallyEndpoint (legacy single-port config)
//  3. fallback (caller-supplied default, typically tally.DefaultEndpoint)
//
// Whitespace-only entries are dropped. Returned slice is a fresh copy
// so callers that mutate it (e.g. dedup) don't poison the config.
func (c *Config) ResolveTallyEndpoints(fallback string) []string {
	if c == nil {
		if fallback == "" {
			return nil
		}
		return []string{fallback}
	}
	if len(c.TallyEndpoints) > 0 {
		out := make([]string, 0, len(c.TallyEndpoints))
		for _, e := range c.TallyEndpoints {
			if e = strings.TrimSpace(e); e != "" {
				out = append(out, e)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if e := strings.TrimSpace(c.TallyEndpoint); e != "" {
		return []string{e}
	}
	if fallback == "" {
		return nil
	}
	return []string{fallback}
}

// PairedConnections returns the effective list of paired connections.
// Preference order:
//
//  1. canonical Connections list
//  2. legacy single-connection top-level fields
func (c *Config) PairedConnections() []PairedConnection {
	if c == nil {
		return nil
	}
	if len(c.Connections) > 0 {
		out := sanitizePairedConnections(c.Connections)
		if len(out) > 0 {
			return out
		}
	}
	legacy := PairedConnection{
		Server:       c.Server,
		ConnectionID: c.ConnectionID,
		CompanyID:    c.CompanyID,
		DeviceName:   c.DeviceName,
		PairedAt:     c.PairedAt,
	}.normalized()
	if legacy.IsValid() {
		return []PairedConnection{legacy}
	}
	return nil
}

func (c *Config) PrimaryConnection() (PairedConnection, bool) {
	conns := c.PairedConnections()
	if len(conns) == 0 {
		return PairedConnection{}, false
	}
	return conns[0], true
}

func (c *Config) FindPairedConnection(connectionID string) (PairedConnection, bool) {
	connectionID = strings.TrimSpace(connectionID)
	if connectionID == "" {
		return PairedConnection{}, false
	}
	for _, conn := range c.PairedConnections() {
		if conn.ConnectionID == connectionID {
			return conn, true
		}
	}
	return PairedConnection{}, false
}

func (c *Config) SetPairedConnections(conns []PairedConnection) {
	sanitized := sanitizePairedConnections(conns)
	c.Connections = sanitized
	if len(sanitized) == 0 {
		c.Server = ""
		c.ConnectionID = ""
		c.CompanyID = ""
		c.DeviceName = ""
		c.PairedAt = time.Time{}
		return
	}
	first := sanitized[0]
	c.Server = first.Server
	c.ConnectionID = first.ConnectionID
	c.CompanyID = first.CompanyID
	c.DeviceName = first.DeviceName
	c.PairedAt = first.PairedAt
}

func (c *Config) UpsertPairedConnection(conn PairedConnection) {
	conn = conn.normalized()
	if !conn.IsValid() {
		return
	}
	conns := c.PairedConnections()
	for i := range conns {
		if conns[i].ConnectionID == conn.ConnectionID {
			conns[i] = conn
			c.SetPairedConnections(conns)
			return
		}
	}
	conns = append(conns, conn)
	c.SetPairedConnections(conns)
}

func sanitizePairedConnections(in []PairedConnection) []PairedConnection {
	out := make([]PairedConnection, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, raw := range in {
		conn := raw.normalized()
		if !conn.IsValid() {
			continue
		}
		if _, dup := seen[conn.ConnectionID]; dup {
			continue
		}
		seen[conn.ConnectionID] = struct{}{}
		out = append(out, conn)
	}
	return out
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
	return c != nil && len(c.PairedConnections()) > 0
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
// YAML file.
//
// File mode is 0o600 — owner-only read/write. The config doesn't hold the
// HMAC secret (that's in the keyring), but it does hold connection_id +
// server URL, which together with a leaked bearer token would let an
// attacker forge ingest requests. Narrower-by-default keeps the same-box
// threat model smaller on Unix. Windows ignores the mode argument and
// inherits ACLs from %ProgramData% — Windows-specific hardening lands in
// the MSI installer (A9) where we can set explicit DACLs.
func Save(path string, c *Config) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}
	if err := applyConfigDirACL(dir); err != nil {
		return err
	}
	if c == nil {
		return fmt.Errorf("config: nil config")
	}
	normalized := *c
	normalized.SetPairedConnections(c.PairedConnections())
	body, err := yaml.Marshal(&normalized)
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("config: write temp %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: rename %s → %s: %w", tmp, path, err)
	}
	if err := applyConfigFileACL(path); err != nil {
		return err
	}
	if _, err := os.ReadFile(path); err != nil {
		return fmt.Errorf("config: write-verify %s: %w", path, err)
	}
	return nil
}
