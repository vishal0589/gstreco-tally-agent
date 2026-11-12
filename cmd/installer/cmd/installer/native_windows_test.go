//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeWindows_RealBinaryGateAndReplacement(t *testing.T) {
	dir := t.TempDir()
	deps := defaultInstallerDeps()
	deps.observeService = func(ctx context.Context, _ func(context.Context, string, ...string) (commandResult, error), _ string) (serviceState, error) {
		return observeNamedWindowsService(ctx, "gstreco-native-qa-only")
	}
	module := "github.com/vishal0589/gstreco-tally-agent/internal/version"
	for _, b := range []struct{ name, pkg string }{{"gstreco-tally-agentctl.exe", "agentctl"}, {"gstreco-tally-agent.exe", "agent"}} {
		cmd := exec.Command("go", "build", "-ldflags", "-X "+module+".Version=0.1.56 -X "+module+".Commit=29e429f+pr69 -X "+module+".Date=2026-11-12", "-o", filepath.Join(dir, b.name), "../"+b.pkg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v %s", err, out)
		}
		result, err := deps.runCommand(context.Background(), filepath.Join(dir, b.name), "--version")
		if err != nil || !strings.Contains(result.Output, "0.1.56 (29e429f+pr69, 2026-11-12)") {
			t.Fatalf("version: %v %+v", err, result)
		}
		t.Log(result.Output)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, []byte("synthetic config preserved"), 0600); err != nil {
		t.Fatal(err)
	}
	before := map[string][32]byte{}
	for _, name := range []string{"gstreco-tally-agentctl.exe", "gstreco-tally-agent.exe", "config.yaml"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		before[name] = sha256.Sum256(data)
	}
	if err := checkInstalledVersions(context.Background(), deps.runCommand, dir, "0.1.54"); err == nil {
		t.Fatal("downgrade accepted")
	}
	for name, hash := range before {
		data, _ := os.ReadFile(filepath.Join(dir, name))
		if sha256.Sum256(data) != hash {
			t.Fatal("downgrade modified " + name)
		}
	}
	for _, v := range []string{"0.1.56", "0.1.57"} {
		if err := checkInstalledVersions(context.Background(), deps.runCommand, dir, v); err != nil {
			t.Fatalf("same/newer gate %s: %v", v, err)
		}
	}
	// Real Windows replacement of an executable using the installer downloader.
	for _, v := range []string{"0.1.56", "0.1.57"} {
		staged := filepath.Join(t.TempDir(), "agentctl.exe")
		cmd := exec.Command("go", "build", "-ldflags", "-X "+module+".Version="+v, "-o", staged, "../agentctl")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build: %v %s", err, out)
		}
		payload, err := os.ReadFile(staged)
		if err != nil {
			t.Fatal(err)
		}
		hash := fmt.Sprintf("%x", sha256.Sum256(payload))
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload) }))
		err = downloadAsset(context.Background(), srv.Client(), srv.URL, filepath.Join(dir, "gstreco-tally-agentctl.exe"), hash)
		srv.Close()
		if err != nil {
			t.Fatalf("same/newer native replacement %s: %v", v, err)
		}
		out, err := exec.Command(filepath.Join(dir, "gstreco-tally-agentctl.exe"), "--version").CombinedOutput()
		if err != nil || !strings.Contains(string(out), v) {
			t.Fatalf("replacement version: %v %s", err, out)
		}
	}
}

func TestNativeWindows_IdleServiceRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qa-service.exe")
	cmd := exec.Command("go", "build", "-o", path, "./testdata/native_service.go")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build service: %v %s", err, out)
	}
	run := defaultInstallerDeps().runCommand
	for _, action := range []string{"install", "start"} {
		if result, err := run(context.Background(), path, "service", action); err != nil {
			t.Fatalf("%s: %v %s", action, err, result.Output)
		}
	}
	defer run(context.Background(), path, "service", "uninstall")
	if err := ensureServiceHealthy(context.Background(), run, path); err != nil {
		t.Fatalf("native idle service restart: %v", err)
	}
	result, err := run(context.Background(), path, "service", "status")
	if err != nil || !strings.Contains(result.Output, "running") {
		t.Fatalf("status: %v %s", err, result.Output)
	}
}

func TestNativeWindows_TransactionRestoresBothAfterFailedServiceStart(t *testing.T) {
	for _, mode := range []string{"failed-start", "unexecutable-after-preflight"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			daemon := filepath.Join(dir, "gstreco-tally-agent.exe")
			ctl := filepath.Join(dir, "gstreco-tally-agentctl.exe")
			cfg := filepath.Join(dir, "config.yaml")
			build := func(out string, fail bool) {
				args := []string{"build"}
				if fail {
					args = append(args, "-ldflags", "-X main.failure=true")
				}
				args = append(args, "-o", out, "./testdata/native_service.go")
				if b, e := exec.Command("go", args...).CombinedOutput(); e != nil {
					t.Fatalf("service build: %v %s", e, b)
				}
			}
			build(daemon, false)
			build(ctl, false)
			oldDaemon, _ := os.ReadFile(daemon)
			oldCtl, _ := os.ReadFile(ctl)
			staged := filepath.Join(t.TempDir(), "fail.exe")
			build(staged, true)
			newDaemon, _ := os.ReadFile(staged)
			if err := os.WriteFile(cfg, []byte("original+second synthetic connections"), 0600); err != nil {
				t.Fatal(err)
			}
			deps := defaultInstallerDeps()
			deps.observeService = func(ctx context.Context, _ func(context.Context, string, ...string) (commandResult, error), _ string) (serviceState, error) {
				return observeNamedWindowsService(ctx, "gstreco-native-qa-only")
			}
			run := deps.runCommand
			for _, action := range []string{"install", "start"} {
				r, e := run(context.Background(), daemon, "service", action)
				if e != nil {
					t.Fatalf("prepare %s: %v %s", action, e, r.Output)
				}
			}
			defer func() { run(context.Background(), daemon, "service", "uninstall") }()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/daemon" {
					w.Write(newDaemon)
				} else {
					w.Write([]byte("new harmless ctl bytes"))
				}
			}))
			defer srv.Close()
			deps.httpClient = srv.Client()
			if mode == "unexecutable-after-preflight" {
				poisoned := false
				deps.runCommand = func(ctx context.Context, path string, args ...string) (commandResult, error) {
					if path == daemon && len(args) == 2 && args[0] == "service" && args[1] == "start" && !poisoned {
						poisoned = true
						if e := os.WriteFile(daemon, []byte("invalid executable after successful preflight"), 0700); e != nil {
							t.Fatal(e)
						}
					}
					return run(ctx, path, args...)
				}
			}
			asset := func(path string, b []byte) installerAsset {
				return installerAsset{URL: srv.URL + path, SHA256: fmt.Sprintf("%x", sha256.Sum256(b))}
			}
			err := installBinariesTransactional(context.Background(), &fakeUI{}, deps, asset("/ctl", []byte("new harmless ctl bytes")), asset("/daemon", newDaemon), ctl, daemon)
			if err == nil || !strings.Contains(err.Error(), "previous binaries restored") {
				t.Fatalf("expected native rollback: %v", err)
			}
			for path, want := range map[string][]byte{daemon: oldDaemon, ctl: oldCtl, cfg: []byte("original+second synthetic connections")} {
				b, e := os.ReadFile(path)
				if e != nil || sha256.Sum256(b) != sha256.Sum256(want) {
					t.Fatalf("not restored/preserved: %s", path)
				}
			}
			r, e := run(context.Background(), daemon, "service", "status")
			if e != nil || !strings.Contains(r.Output, "running") {
				t.Fatalf("previous service not running: %v %s", e, r.Output)
			}
			backups, _ := filepath.Glob(filepath.Join(dir, ".gstreco-backup-*"))
			if len(backups) != 1 {
				t.Fatal("no durable backup retained")
			}
		})
	}
}
