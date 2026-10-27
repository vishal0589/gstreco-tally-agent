// Package autoupdate installs a newer agent release on this machine without
// anyone at it.
//
// When the version check finds a newer release and the server allows it
// (/api/tally/agent/version auto_update.enabled, GST Reco's kill switch), the
// daemon downloads the GST Reco installer the server publishes, checks it
// against the published SHA-256 and starts it detached. On a paired machine
// the installer runs its silent update: it downloads the new agent binaries
// (each checked against the manifest), swaps them in, restarts the Windows
// service and keeps every company. The new daemon then reports its version in
// its heartbeat.
//
// Guards: Windows only; never a downgrade or a dev build; never while a sync
// is running; one attempt per release per hour and at most three per
// release, so a release that cannot install does not loop.
package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

const (
	// MaxAttempts is how many times one release is tried before the
	// daemon leaves it to the operator.
	MaxAttempts = 3
	// RetryAfter is the least time between two attempts at one release.
	RetryAfter = time.Hour
	// stateFile records the attempts, next to config.yaml.
	stateFile = "autoupdate-state.json"
	// maxInstallerBytes bounds the download; the installer is ~7 MB.
	maxInstallerBytes = 100 << 20
)

// Installer is the installer asset from the version manifest.
type Installer struct {
	URL     string
	SHA256  string
	Version string
}

// Request is one version check's result.
type Request struct {
	CurrentVersion string
	LatestVersion  string
	// Enabled is the manifest's auto_update.enabled.
	Enabled   bool
	Installer *Installer
}

// Outcome is what Consider did.
type Outcome string

const (
	OutcomeLaunched    Outcome = "launched"
	OutcomeDisabled    Outcome = "disabled_by_server"
	OutcomeUnsupported Outcome = "unsupported_platform"
	OutcomeNotNewer    Outcome = "not_newer"
	OutcomeNoInstaller Outcome = "no_installer"
	OutcomeBusy        Outcome = "sync_running"
	OutcomeBackoff     Outcome = "recently_attempted"
	OutcomeGaveUp      Outcome = "gave_up"
)

// Options configures an Updater.
type Options struct {
	HTTPClient *http.Client
	// StateDir holds the attempt record and the downloaded installer:
	// the directory of config.yaml.
	StateDir string
	// Server, ConfigPath and InstallDir are passed to the installer so it
	// updates this agent, with this config, in place.
	Server     string
	ConfigPath string
	InstallDir string
	// GOOS is runtime.GOOS; injected for tests.
	GOOS string
	// Busy reports whether a sync tick is running.
	Busy func() bool
	// Launch starts the installer detached from the daemon, which the
	// installer restarts.
	Launch func(path string, args []string) error
	Now    func() time.Time
	Logger zerolog.Logger
}

// Updater applies newer releases. Safe for concurrent use.
type Updater struct {
	opts Options
	mu   sync.Mutex
}

// New builds an Updater, filling defaults for the injected functions.
func New(opts Options) *Updater {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 5 * time.Minute}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Busy == nil {
		opts.Busy = func() bool { return false }
	}
	if opts.Launch == nil {
		opts.Launch = launchDetached
	}
	return &Updater{opts: opts}
}

type attemptState struct {
	Version     string    `json:"version"`
	Attempts    int       `json:"attempts"`
	LastAttempt time.Time `json:"last_attempt"`
}

// Consider installs req.LatestVersion when every guard allows it.
func (u *Updater) Consider(ctx context.Context, req Request) (Outcome, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if !req.Enabled {
		return OutcomeDisabled, nil
	}
	if u.opts.GOOS != "windows" {
		return OutcomeUnsupported, nil
	}
	if !IsNewer(req.LatestVersion, req.CurrentVersion) {
		return OutcomeNotNewer, nil
	}
	if req.Installer == nil || strings.TrimSpace(req.Installer.URL) == "" || strings.TrimSpace(req.Installer.SHA256) == "" {
		return OutcomeNoInstaller, nil
	}
	if u.opts.Busy() {
		return OutcomeBusy, nil
	}

	latest := normalizeVersion(req.LatestVersion)
	state := u.loadState()
	if state.Version != latest {
		state = attemptState{Version: latest}
	}
	now := u.opts.Now()
	// The last attempt's installer may still be at work.
	if state.Attempts > 0 && now.Sub(state.LastAttempt) < RetryAfter {
		return OutcomeBackoff, nil
	}
	if state.Attempts >= MaxAttempts {
		return OutcomeGaveUp, nil
	}
	// Counted before the download so a release that cannot even be
	// fetched or verified still runs out of attempts.
	state.Attempts++
	state.LastAttempt = now
	if err := u.saveState(state); err != nil {
		return "", fmt.Errorf("autoupdate: record attempt: %w", err)
	}

	path, err := u.download(ctx, latest, *req.Installer)
	if err != nil {
		return "", err
	}
	args := []string{"--no-browser"}
	if u.opts.Server != "" {
		args = append(args, "--server", u.opts.Server)
	}
	if u.opts.ConfigPath != "" {
		args = append(args, "--config", u.opts.ConfigPath)
	}
	if u.opts.InstallDir != "" {
		args = append(args, "--install-dir", u.opts.InstallDir)
	}
	if err := u.opts.Launch(path, args); err != nil {
		return "", fmt.Errorf("autoupdate: start installer: %w", err)
	}
	return OutcomeLaunched, nil
}

// download fetches the installer into StateDir/updates and checks its
// SHA-256. Its name never carries a setup code, so the installer takes its
// update path on a paired machine.
func (u *Updater) download(ctx context.Context, version string, asset Installer) (string, error) {
	dir := filepath.Join(u.opts.StateDir, "updates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("autoupdate: create %s: %w", dir, err)
	}
	dest := filepath.Join(dir, "gstreco-tally-installer-"+version+".exe")

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", fmt.Errorf("autoupdate: %w", err)
	}
	resp, err := u.opts.HTTPClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("autoupdate: download installer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("autoupdate: download installer: HTTP %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(dir, "installer-*.download")
	if err != nil {
		return "", fmt.Errorf("autoupdate: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(resp.Body, maxInstallerBytes+1))
	closeErr := tmp.Close()
	if copyErr != nil {
		return "", fmt.Errorf("autoupdate: download installer: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("autoupdate: %w", closeErr)
	}
	if written > maxInstallerBytes {
		return "", errors.New("autoupdate: installer is larger than expected")
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); !strings.EqualFold(got, strings.TrimSpace(asset.SHA256)) {
		return "", fmt.Errorf("autoupdate: installer sha256 %s does not match the published %s", got, asset.SHA256)
	}
	_ = os.Remove(dest)
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", fmt.Errorf("autoupdate: %w", err)
	}
	return dest, nil
}

func (u *Updater) statePath() string { return filepath.Join(u.opts.StateDir, stateFile) }

func (u *Updater) loadState() attemptState {
	var state attemptState
	raw, err := os.ReadFile(u.statePath())
	if err != nil {
		return state
	}
	if json.Unmarshal(raw, &state) != nil {
		return attemptState{}
	}
	return state
}

func (u *Updater) saveState(state attemptState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(u.statePath(), raw, 0o600)
}

// IsNewer reports whether latest is a higher release than current. Both
// must be plain releases ("0.1.45" or "v0.1.45"); a dev build or anything
// unparseable is never updated.
func IsNewer(latest, current string) bool {
	l, okL := parseVersion(latest)
	c, okC := parseVersion(current)
	if !okL || !okC {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

func normalizeVersion(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}

func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	// "v0.1.44 (4369843, 2026-…)" is how the daemon prints itself.
	v = normalizeVersion(v)
	if i := strings.IndexAny(v, " +"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
