package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/pair"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseReleaseVersion(t *testing.T) {
	for _, value := range []string{"0.1.56", "v0.1.56", "0.1.56+build.9", "1.0.0"} {
		if _, err := parseReleaseVersion(value); err != nil {
			t.Errorf("%q: %v", value, err)
		}
	}
	for _, value := range []string{"", "dev", "0.1.55-kss.1", "0.1", "-1.2.3", "0.01.56", "0.1.56 garbage", "18446744073709551616.0.0"} {
		if _, err := parseReleaseVersion(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}

func TestCheckInstalledVersions(t *testing.T) {
	cases := []struct {
		name, ctl, daemon, latest string
		failProbe, wantError      bool
	}{
		{name: "fresh", latest: "0.1.56"},
		{name: "equal", ctl: "v0.1.56", daemon: "0.1.56", latest: "0.1.56"},
		{name: "upgrade", ctl: "0.1.54", daemon: "0.1.54", latest: "0.1.56"},
		{name: "ctl newer", ctl: "0.1.56", daemon: "0.1.54", latest: "0.1.54", wantError: true},
		{name: "daemon newer", ctl: "0.1.54", daemon: "0.1.56", latest: "0.1.54", wantError: true},
		{name: "numeric not lexical", ctl: "0.1.9", daemon: "0.1.9", latest: "0.1.10"},
		{name: "major downgrade", daemon: "1.0.0", latest: "0.99.99", wantError: true},
		{name: "custom build", daemon: "0.1.55-kss.1", latest: "0.1.56", wantError: true},
		{name: "dev", ctl: "dev", latest: "0.1.56", wantError: true},
		{name: "bad metadata fresh", latest: "dev", wantError: true},
		{name: "probe failed", ctl: "0.1.54", latest: "0.1.56", failProbe: true, wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			versions := map[string]string{"gstreco-tally-agentctl.exe": tc.ctl, "gstreco-tally-agent.exe": tc.daemon}
			for name, v := range versions {
				if v != "" {
					if err := os.WriteFile(filepath.Join(dir, name), []byte("existing"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			probe := func(_ context.Context, path string, args ...string) (commandResult, error) {
				if strings.Join(args, " ") != "--version" {
					t.Fatalf("unsafe probe: %v", args)
				}
				if tc.failProbe {
					return commandResult{ExitCode: 1}, errors.New("probe failed")
				}
				prefix := "agentctl "
				if filepath.Base(path) == "gstreco-tally-agent.exe" {
					prefix = "gstreco-tally-agent "
				}
				return commandResult{Output: prefix + versions[filepath.Base(path)] + " (29e429f+pr69, 2026-11-12)", ExitCode: 0}, nil
			}
			err := checkInstalledVersions(context.Background(), probe, dir, tc.latest)
			if (err != nil) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
		})
	}
}

// Exercise every installer entry mode, not merely the version helper. A stale
// manifest must stop before ACL repair, config load, approval/claim, credentials,
// downloads, discovery or any service command.
func TestRunInstaller_StaleMetadataStopsBeforePairingAndWrites(t *testing.T) {
	for _, opts := range []installerOptions{{}, {addTenant: true}, {code: "ABC123"}, {addTenant: true, code: "ABC123"}} {
		t.Run(strings.Join([]string{opts.code, func() string {
			if opts.addTenant {
				return "add-tenant"
			}
			return "update"
		}()}, "-"), func(t *testing.T) {
			dir := t.TempDir()
			opts.installDir = dir
			opts.configPath = filepath.Join(dir, "config.yaml")
			opts.server = "https://example.com"
			for _, name := range []string{"gstreco-tally-agentctl.exe", "gstreco-tally-agent.exe", "config.yaml"} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte("unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			deps := testInstallerDeps()
			deps.goos = func() string { return "windows" }
			deps.isAdmin = func() (bool, error) { return true, nil }
			deps.loadLocalPair = func(string) (localPairState, error) {
				t.Fatal("config load/repair happened before guard")
				return localPairState{}, nil
			}
			requests := 0
			probes := 0
			deps.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				requests++
				if req.Method != "GET" || req.URL.Path != "/api/tally/agent/version" {
					t.Fatalf("unexpected HTTP effect: %s %s", req.Method, req.URL)
				}
				return jsonResponse(200, versionMetadataResponse{Latest: "0.1.54", Platforms: map[string]installerAsset{"windows-amd64": {URL: "https://example.com/ctl"}}, Daemon: map[string]installerAsset{"windows-amd64": {URL: "https://example.com/daemon"}}}), nil
			})}
			deps.runCommand = func(_ context.Context, path string, args ...string) (commandResult, error) {
				probes++
				if strings.Join(args, " ") != "--version" {
					t.Fatalf("unsafe command %s %v", path, args)
				}
				return commandResult{Output: "agentctl 0.1.56 (29e429f+pr69, 2026-11-12)"}, nil
			}
			ui := &fakeUI{}
			if exit := runInstaller(context.Background(), ui, opts, deps); exit == 0 {
				t.Fatal("unsafe install succeeded")
			}
			if requests != 1 || probes != 1 || !ui.sawLog("refusing downgrade") {
				t.Fatalf("requests=%d probes=%d logs=%v", requests, probes, ui.logs)
			}
			for _, name := range []string{"gstreco-tally-agentctl.exe", "gstreco-tally-agent.exe", "config.yaml"} {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || !bytes.Equal(data, []byte("unchanged")) {
					t.Fatalf("changed %s", name)
				}
			}
		})
	}
}

func TestCheckInstalledVersions_ProbeTimeout(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gstreco-tally-agentctl.exe"), []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	// A shorter caller deadline exercises the same CommandContext cancellation
	// without making every suite run wait for the full ten-second production cap.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	called := false
	probe := func(ctx context.Context, _ string, _ ...string) (commandResult, error) {
		called = true
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second {
			t.Fatal("probe has no bounded deadline")
		}
		<-ctx.Done()
		return commandResult{ExitCode: -1}, ctx.Err()
	}
	err := checkInstalledVersions(ctx, probe, dir, "0.1.57")
	if !called || err == nil || !strings.Contains(err.Error(), "cannot verify installed version") || !strings.Contains(err.Error(), versionRepairAdvice) {
		t.Fatalf("called=%v err=%v", called, err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "gstreco-tally-agentctl.exe"))
	if err != nil || string(data) != "unchanged" {
		t.Fatal("binary changed")
	}
}

func TestCheckInstalledVersions_UnknownVersionGivesRepairAdvice(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gstreco-tally-agentctl.exe"), []byte("existing"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"agentctl dev (local, today)", "agentctl 0.1.55-kss.1 (local, today)", "agentctl ", "unexpected"} {
		err := checkInstalledVersions(context.Background(), func(context.Context, string, ...string) (commandResult, error) {
			return commandResult{Output: output}, nil
		}, dir, "0.1.57")
		if err == nil || !strings.Contains(err.Error(), versionRepairAdvice) {
			t.Fatalf("%q: %v", output, err)
		}
	}
}

func TestRunInstaller_SecondGateRejectsBinaryChangedDuringPairing(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	ctlPath := filepath.Join(dir, "gstreco-tally-agentctl.exe")
	daemonPath := filepath.Join(dir, "gstreco-tally-agent.exe")
	for _, path := range []string{ctlPath, daemonPath, cfgPath} {
		if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	deps := testInstallerDeps()
	deps.goos = func() string { return "windows" }
	deps.isAdmin = func() (bool, error) { return true, nil }
	paired := false
	probes := 0
	requests := 0
	deps.loadLocalPair = func(string) (localPairState, error) {
		return localPairState{State: "paired", ConfigPath: cfgPath, Config: &config.Config{Server: "https://example.com", ConnectionID: "old-connection"}}, nil
	}
	deps.claimPair = func(context.Context, pair.Options) (*pair.ClaimResponse, error) {
		// Model the claim's credential write and a concurrent daemon upgrade while
		// the operator is pairing. The second gate must leave both as they are.
		paired = true
		if err := os.WriteFile(cfgPath, []byte("credentials already persisted"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(daemonPath, []byte("concurrent newer binary"), 0600); err != nil {
			t.Fatal(err)
		}
		return &pair.ClaimResponse{ConnectionID: "qa-connection"}, nil
	}
	deps.httpClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Method != "GET" || req.URL.Path != "/api/tally/agent/version" {
			t.Fatalf("unexpected HTTP: %s %s", req.Method, req.URL)
		}
		return jsonResponse(200, versionMetadataResponse{Latest: "0.1.57", Platforms: map[string]installerAsset{"windows-amd64": {URL: "https://example.com/ctl"}}, Daemon: map[string]installerAsset{"windows-amd64": {URL: "https://example.com/daemon"}}}), nil
	})}
	deps.runCommand = func(_ context.Context, path string, args ...string) (commandResult, error) {
		if strings.Join(args, " ") == "--version" {
			probes++
			prefix := "agentctl "
			v := "0.1.56"
			if path == daemonPath {
				prefix = "gstreco-tally-agent "
				if paired {
					v = "0.1.58"
				}
			}
			return commandResult{Output: prefix + v + " (29e429f+pr69, 2026-11-12)"}, nil
		}
		if filepath.Base(path) == "icacls.exe" {
			return commandResult{Output: "Successfully processed 1 files; Failed processing 0 files"}, nil
		}
		t.Fatalf("unsafe command %s %v", path, args)
		return commandResult{}, nil
	}
	ui := &fakeUI{}
	exit := runInstaller(context.Background(), ui, installerOptions{server: "https://example.com", installDir: dir, configPath: cfgPath, addTenant: true, code: "ABC123"}, deps)
	if exit == 0 || !paired || probes != 4 || requests != 1 || !ui.sawLog("refusing downgrade") {
		t.Fatalf("exit=%d paired=%v probes=%d requests=%d logs=%v", exit, paired, probes, requests, ui.logs)
	}
	for path, want := range map[string]string{ctlPath: "before", daemonPath: "concurrent newer binary", cfgPath: "credentials already persisted"} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("changed %s: %s %v", path, data, err)
		}
	}
}
