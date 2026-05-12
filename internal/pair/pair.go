// Package pair implements the one-shot claim flow. The user generates a
// code in the web UI, types it into `agentctl pair`, and the agent POSTs it
// to /api/tally/pair/claim. On success the server returns a connection_id +
// bearer token + HMAC secret; pair persists the non-secret state via
// config.Save and the secrets via keyring.Store.
package pair

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
)

// PairCodeLen is the canonical pair-code length (Crockford base32, 6 chars).
// Must stay in sync with `src/lib/tally/pair-code.ts` in the GST_Reco repo.
const PairCodeLen = 6

// pairCodeRE matches the canonical (no-hyphen, uppercase) form. The
// Crockford base32 alphabet intentionally excludes I, L, O, U so the
// display font can't confuse letters and numbers.
var pairCodeRE = regexp.MustCompile(`^[0-9A-HJ-KMNP-TV-Z]{6}$`)

// ClaimResponse mirrors the server's /api/tally/pair/claim 200 body. Must
// stay in sync with GST_Reco's src/app/api/tally/pair/claim/route.ts.
type ClaimResponse struct {
	ConnectionID string `json:"connection_id"`
	CompanyID    string `json:"company_id"`
	Token        string `json:"token"`
	HmacSecret   string `json:"hmac_secret"`
}

// ClaimRequest is what the agent sends. Device name is surfaced in the
// server's connection list UI so the user can tell agents apart.
type ClaimRequest struct {
	Code         string `json:"code"`
	DeviceName   string `json:"device_name"`
	AgentVersion string `json:"agent_version"`
	OS           string `json:"os"`
}

// ErrAlreadyPaired is returned by Claim when the local config shows an
// existing connection. Pair flow refuses to clobber — caller must revoke
// the old connection first via agentctl unpair (A5) or edit/delete the
// config file manually.
var ErrAlreadyPaired = errors.New("pair: this agent is already paired; revoke first")

// ErrInvalidCode wraps 400 responses for visibly malformed codes — caller
// shows a clear "check the code" message instead of a generic error.
var ErrInvalidCode = errors.New("pair: invalid code format")

// ErrCodeGone wraps 410 responses — the code expired or was already claimed
// by a different agent. Caller tells the user to generate a new code.
var ErrCodeGone = errors.New("pair: pair code is invalid, expired, or already claimed")

// Options customises Claim. ConfigPath, DeviceName, and AgentVersion have
// sensible defaults derived at call time; HTTPClient falls back to a
// 30-second timeout client if nil.
type Options struct {
	// Server is the GST Reco base URL (e.g. https://gstreco.m2ai.ai). Required.
	Server string
	// Code is what the user typed. Whitespace, hyphens, and case are
	// normalised before hitting the wire.
	Code string
	// ConfigPath overrides config.DefaultPath(). Used by tests.
	ConfigPath string
	// Keyring is where the HMAC secret + bearer token go. Required.
	Keyring keyring.Store
	// DeviceName overrides os.Hostname().
	DeviceName string
	// AgentVersion is the agent binary version (from internal/version.Version).
	AgentVersion string
	// HTTPClient overrides the default 30s-timeout client.
	HTTPClient *http.Client
}

// Claim performs the pair flow end-to-end. Returns the server's ClaimResponse
// on success. On success the agent is ready to start signing ingest requests
// on the next run.
func Claim(ctx context.Context, opts Options) (*ClaimResponse, error) {
	if opts.Server == "" {
		return nil, fmt.Errorf("pair: Server is required")
	}
	if opts.Keyring == nil {
		return nil, fmt.Errorf("pair: Keyring is required")
	}

	code, err := canonicalizePairCode(opts.Code)
	if err != nil {
		return nil, err
	}

	configPath := opts.ConfigPath
	if configPath == "" {
		configPath = config.DefaultPath()
	}

	if existing, err := config.Load(configPath); err == nil && existing.IsPaired() {
		return nil, fmt.Errorf("%w (connection_id=%s)", ErrAlreadyPaired, existing.ConnectionID)
	} else if err != nil && !errors.Is(err, config.ErrNotFound) {
		return nil, fmt.Errorf("pair: pre-check config: %w", err)
	}

	deviceName := opts.DeviceName
	if deviceName == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			deviceName = h
		} else {
			deviceName = "unknown-device"
		}
	}
	if len(deviceName) > 128 {
		deviceName = deviceName[:128]
	}

	agentVersion := opts.AgentVersion
	if agentVersion == "" {
		agentVersion = "dev"
	}

	reqBody, err := json.Marshal(ClaimRequest{
		Code:         code,
		DeviceName:   deviceName,
		AgentVersion: agentVersion,
		OS:           runtime.GOOS + "-" + runtime.GOARCH,
	})
	if err != nil {
		return nil, fmt.Errorf("pair: encode request: %w", err)
	}

	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	url := strings.TrimRight(opts.Server, "/") + "/api/tally/pair/claim"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("pair: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "gstreco-tally-agent/"+agentVersion)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("pair: POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Fall through.
	case http.StatusBadRequest:
		return nil, fmt.Errorf("%w: server replied 400 (check the code and try again)", ErrInvalidCode)
	case http.StatusGone:
		return nil, ErrCodeGone
	default:
		return nil, fmt.Errorf("pair: unexpected status %d from %s", resp.StatusCode, url)
	}

	var claim ClaimResponse
	if err := json.NewDecoder(resp.Body).Decode(&claim); err != nil {
		return nil, fmt.Errorf("pair: decode response: %w", err)
	}
	if claim.ConnectionID == "" || claim.HmacSecret == "" || claim.Token == "" {
		return nil, fmt.Errorf("pair: server response missing connection_id/hmac_secret/token")
	}

	if err := PersistCredentials(PersistOptions{
		ConfigPath:   configPath,
		Keyring:      opts.Keyring,
		Server:       opts.Server,
		DeviceName:   deviceName,
		ConnectionID: claim.ConnectionID,
		Token:        claim.Token,
		HmacSecret:   claim.HmacSecret,
		PairedAt:     time.Now().UTC(),
	}); err != nil {
		return nil, err
	}

	return &claim, nil
}

type PersistOptions struct {
	ConfigPath   string
	Keyring      keyring.Store
	Server       string
	DeviceName   string
	ConnectionID string
	Token        string
	HmacSecret   string
	PairedAt     time.Time
}

func PersistCredentials(opts PersistOptions) error {
	if opts.ConfigPath == "" {
		opts.ConfigPath = config.DefaultPath()
	}
	if opts.Keyring == nil {
		return fmt.Errorf("pair: Keyring is required")
	}
	if opts.ConnectionID == "" || opts.Token == "" || opts.HmacSecret == "" {
		return fmt.Errorf("pair: connection_id, token, and hmac_secret are required")
	}
	pairedAt := opts.PairedAt
	if pairedAt.IsZero() {
		pairedAt = time.Now().UTC()
	}

	// Persist secrets to keyring FIRST. If the keyring write fails, the
	// config is never touched and the agent remains in the "unpaired" state
	// so the user can retry without clobbering anything.
	hmacKey, bearerKey := keyring.ConnectionKeys(opts.ConnectionID)
	if err := opts.Keyring.Set(keyring.ServiceName, hmacKey, opts.HmacSecret); err != nil {
		return fmt.Errorf("pair: store hmac secret: %w", err)
	}
	if err := opts.Keyring.Set(keyring.ServiceName, bearerKey, opts.Token); err != nil {
		// Roll back the hmac write to keep the two-of-two invariant.
		_ = opts.Keyring.Delete(keyring.ServiceName, hmacKey)
		return fmt.Errorf("pair: store bearer token: %w", err)
	}

	cfg := &Config{
		Server:       strings.TrimRight(opts.Server, "/"),
		ConnectionID: opts.ConnectionID,
		DeviceName:   opts.DeviceName,
		PairedAt:     pairedAt,
	}
	if err := config.Save(opts.ConfigPath, (*config.Config)(cfg)); err != nil {
		// Roll back both keyring writes so the agent stays clean-unpaired.
		_ = opts.Keyring.Delete(keyring.ServiceName, hmacKey)
		_ = opts.Keyring.Delete(keyring.ServiceName, bearerKey)
		return fmt.Errorf("pair: save config: %w", err)
	}
	return nil
}

// Config is a local alias so we can use struct conversion to config.Config
// above while keeping the field set visible in one file.
type Config config.Config

// canonicalizePairCode strips whitespace and hyphens, uppercases, then
// validates the 6-char Crockford shape. Done on the client side so the user
// gets a fast "invalid format" message without a round-trip.
func canonicalizePairCode(raw string) (string, error) {
	c := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(raw), "-", ""), " ", ""))
	if len(c) != PairCodeLen {
		return "", fmt.Errorf("%w: expected %d characters (got %d)", ErrInvalidCode, PairCodeLen, len(c))
	}
	if !pairCodeRE.MatchString(c) {
		return "", fmt.Errorf("%w: only Crockford base32 characters (0-9, A-Z minus I/L/O/U) allowed", ErrInvalidCode)
	}
	return c, nil
}
