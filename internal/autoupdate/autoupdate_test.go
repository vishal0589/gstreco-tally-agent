package autoupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

var installerBytes = []byte("MZ fake installer")

func installerSum() string {
	sum := sha256.Sum256(installerBytes)
	return hex.EncodeToString(sum[:])
}

type harness struct {
	updater  *Updater
	stateDir string
	launches []launch
	now      time.Time
	busy     bool
	fetches  *atomic.Int32
	srv      *httptest.Server
}

type launch struct {
	path string
	args []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		stateDir: t.TempDir(),
		now:      time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
		fetches:  &atomic.Int32{},
	}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.fetches.Add(1)
		_, _ = w.Write(installerBytes)
	}))
	t.Cleanup(h.srv.Close)
	h.updater = New(Options{
		HTTPClient: h.srv.Client(),
		StateDir:   h.stateDir,
		Server:     "https://gstreco.example",
		ConfigPath: filepath.Join(h.stateDir, "config.yaml"),
		InstallDir: `C:\Program Files\GST Reco Agent`,
		GOOS:       "windows",
		Busy:       func() bool { return h.busy },
		Launch: func(path string, args []string) error {
			h.launches = append(h.launches, launch{path, args})
			return nil
		},
		Now:    func() time.Time { return h.now },
		Logger: zerolog.New(io.Discard),
	})
	return h
}

func (h *harness) request() Request {
	return Request{
		CurrentVersion: "v0.1.44 (4369843, 2026-10-27T16:26:51Z)",
		LatestVersion:  "0.1.45",
		Enabled:        true,
		Installer:      &Installer{URL: h.srv.URL + "/installer.exe", SHA256: installerSum(), Version: "0.1.41"},
	}
}

func TestConsider_LaunchesTheVerifiedInstallerInUpdateMode(t *testing.T) {
	h := newHarness(t)
	outcome, err := h.updater.Consider(context.Background(), h.request())
	if err != nil || outcome != OutcomeLaunched {
		t.Fatalf("Consider = %q, %v; want launched", outcome, err)
	}
	if len(h.launches) != 1 {
		t.Fatalf("launches = %d, want 1", len(h.launches))
	}
	got := h.launches[0]
	if want := filepath.Join(h.stateDir, "updates", "gstreco-tally-installer-0.1.45.exe"); got.path != want {
		t.Errorf("path = %s, want %s", got.path, want)
	}
	// No pair code in the name or the args: the installer takes its
	// silent update path on a paired machine.
	if strings.Contains(filepath.Base(got.path), "setup") {
		t.Errorf("installer name %s could be read as a setup code", got.path)
	}
	wantArgs := []string{
		"--no-browser",
		"--server", "https://gstreco.example",
		"--config", filepath.Join(h.stateDir, "config.yaml"),
		"--install-dir", `C:\Program Files\GST Reco Agent`,
	}
	if strings.Join(got.args, "|") != strings.Join(wantArgs, "|") {
		t.Errorf("args = %q, want %q", got.args, wantArgs)
	}
	if raw, err := os.ReadFile(got.path); err != nil || string(raw) != string(installerBytes) {
		t.Errorf("installer on disk = %q, %v", raw, err)
	}
}

func TestConsider_GuardsSkipWithoutDownloading(t *testing.T) {
	cases := []struct {
		name   string
		modify func(h *harness, r *Request)
		want   Outcome
	}{
		{"server switched it off", func(_ *harness, r *Request) { r.Enabled = false }, OutcomeDisabled},
		{"not Windows", func(h *harness, _ *Request) { h.updater.opts.GOOS = "linux" }, OutcomeUnsupported},
		{"same release", func(_ *harness, r *Request) { r.LatestVersion = "0.1.44" }, OutcomeNotNewer},
		{"older release", func(_ *harness, r *Request) { r.LatestVersion = "0.1.43" }, OutcomeNotNewer},
		{"dev build", func(_ *harness, r *Request) { r.CurrentVersion = "a27a610-dirty" }, OutcomeNotNewer},
		{"no installer", func(_ *harness, r *Request) { r.Installer = nil }, OutcomeNoInstaller},
		{"installer without checksum", func(_ *harness, r *Request) { r.Installer.SHA256 = "" }, OutcomeNoInstaller},
		{"sync running", func(h *harness, _ *Request) { h.busy = true }, OutcomeBusy},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			req := h.request()
			c.modify(h, &req)
			outcome, err := h.updater.Consider(context.Background(), req)
			if err != nil || outcome != c.want {
				t.Fatalf("Consider = %q, %v; want %q", outcome, err, c.want)
			}
			if len(h.launches) != 0 || h.fetches.Load() != 0 {
				t.Errorf("launches = %d, fetches = %d; want none", len(h.launches), h.fetches.Load())
			}
		})
	}
}

func TestConsider_RefusesAnInstallerThatFailsItsChecksum(t *testing.T) {
	h := newHarness(t)
	req := h.request()
	req.Installer.SHA256 = strings.Repeat("0", 64)
	if _, err := h.updater.Consider(context.Background(), req); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("err = %v, want a sha256 mismatch", err)
	}
	if len(h.launches) != 0 {
		t.Fatal("a mismatched installer must never start")
	}
	if _, err := os.Stat(filepath.Join(h.stateDir, "updates", "gstreco-tally-installer-0.1.45.exe")); !os.IsNotExist(err) {
		t.Errorf("mismatched installer left on disk: %v", err)
	}
}

func TestConsider_BacksOffAndGivesUpPerRelease(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for attempt := 1; attempt <= MaxAttempts; attempt++ {
		if outcome, err := h.updater.Consider(ctx, h.request()); err != nil || outcome != OutcomeLaunched {
			t.Fatalf("attempt %d = %q, %v", attempt, outcome, err)
		}
		// The next check within the hour waits.
		h.now = h.now.Add(30 * time.Minute)
		if outcome, _ := h.updater.Consider(ctx, h.request()); outcome != OutcomeBackoff {
			t.Fatalf("after attempt %d: %q, want recently_attempted", attempt, outcome)
		}
		h.now = h.now.Add(RetryAfter)
	}
	if outcome, _ := h.updater.Consider(ctx, h.request()); outcome != OutcomeGaveUp {
		t.Fatalf("after %d attempts: %q, want gave_up", MaxAttempts, outcome)
	}
	// A newer release starts over.
	req := h.request()
	req.LatestVersion = "0.1.46"
	if outcome, err := h.updater.Consider(ctx, req); err != nil || outcome != OutcomeLaunched {
		t.Fatalf("next release = %q, %v; want launched", outcome, err)
	}
	if len(h.launches) != MaxAttempts+1 {
		t.Errorf("launches = %d, want %d", len(h.launches), MaxAttempts+1)
	}
}

func TestIsNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.1.45", "v0.1.44", true},
		{"0.1.45", "v0.1.44 (4369843, 2026-10-27T16:26:51Z)", true},
		{"0.2.0", "0.1.99", true},
		{"0.1.44", "v0.1.44", false},
		{"0.1.43", "v0.1.44", false},
		{"0.1.45", "v0.1.44-3-gabc1234", false},
		{"0.1.45", "dev", false},
		{"", "v0.1.44", false},
	}
	for _, c := range cases {
		if got := IsNewer(c.latest, c.current); got != c.want {
			t.Errorf("IsNewer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}
