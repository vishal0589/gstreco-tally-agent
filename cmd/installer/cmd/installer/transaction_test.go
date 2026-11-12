package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTransactionalInstall_ExistingService(t *testing.T) {
	for _, failure := range []string{"none", "daemon-download", "new-start", "new-health", "restore-start", "stop", "stopped-success", "stopped-failure", "stop-flake", "sha-mismatch", "ctx-cancel", "restore-first-failure"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			ctl := filepath.Join(dir, "gstreco-tally-agentctl.exe")
			daemon := filepath.Join(dir, "gstreco-tally-agent.exe")
			cfg := filepath.Join(dir, "config.yaml")
			for path, data := range map[string]string{ctl: "old-ctl", daemon: "old-daemon", cfg: "original+second synthetic connection"} {
				if err := os.WriteFile(path, []byte(data), 0700); err != nil {
					t.Fatal(err)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if failure == "daemon-download" && r.URL.Path == "/daemon" {
					w.WriteHeader(500)
					return
				}
				fmt.Fprint(w, "new-"+strings.TrimPrefix(r.URL.Path, "/"))
			}))
			defer srv.Close()
			asset := func(name string) installerAsset {
				a := installerAsset{URL: srv.URL + "/" + name, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("new-"+name)))}
				if failure == "sha-mismatch" && name == "daemon" {
					a.SHA256 = strings.Repeat("0", 64)
				}
				return a
			}
			deps := testInstallerDeps()
			deps.httpClient = srv.Client()
			running := !strings.HasPrefix(failure, "stopped-")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stopCalled, flaked := false, false
			commands := []string{}
			newStarted := false
			deps.runCommand = func(commandCtx context.Context, _ string, args ...string) (commandResult, error) {
				if len(args) == 1 && args[0] == "--version" {
					return commandResult{Output: "synthetic version"}, nil
				}
				commands = append(commands, strings.Join(args, " "))
				if commandCtx.Err() != nil {
					return commandResult{}, commandCtx.Err()
				}
				if failure == "ctx-cancel" && ctx.Err() != nil {
					deadline, ok := commandCtx.Deadline()
					if !ok || time.Until(deadline) < 2*time.Minute {
						t.Fatal("recovery context too short or inherited cancellation")
					}
				}
				if len(args) != 2 || args[0] != "service" {
					t.Fatalf("unexpected command %v", args)
				}
				data, _ := os.ReadFile(daemon)
				isNew := string(data) == "new-daemon"
				// Every mutation must have both durable backups, even after replacement.
				if args[1] != "status" {
					backups, _ := filepath.Glob(filepath.Join(dir, ".gstreco-backup-*"))
					if len(backups) != 1 {
						t.Fatalf("no backup before %v", args)
					}
					for name, want := range map[string]string{filepath.Base(ctl): "old-ctl", filepath.Base(daemon): "old-daemon"} {
						b, e := os.ReadFile(filepath.Join(backups[0], name))
						if e != nil || string(b) != want {
							t.Fatalf("backup missing/changed %s", name)
						}
					}
				}
				switch args[1] {
				case "status":
					if failure == "stop-flake" && stopCalled && !flaked {
						flaked = true
						return commandResult{Output: "service status: running"}, nil
					}
					if failure == "new-health" && newStarted && isNew {
						running = false
					}
					state := "stopped"
					if running {
						state = "running"
					}
					return commandResult{Output: "service status: " + state}, nil
				case "stop":
					stopCalled = true
					if failure == "stop" {
						return commandResult{ExitCode: 1}, fmt.Errorf("injected stop failure")
					}
					running = false
				case "start":
					if isNew && failure == "restore-first-failure" {
						backups, _ := filepath.Glob(filepath.Join(dir, ".gstreco-backup-*"))
						if e := os.Remove(filepath.Join(backups[0], filepath.Base(ctl))); e != nil {
							t.Fatal(e)
						}
						return commandResult{}, fmt.Errorf("injected restore staging failure")
					}
					if isNew && failure == "ctx-cancel" {
						cancel()
						return commandResult{}, context.Canceled
					}
					if isNew && (failure == "new-start" || failure == "restore-start" || failure == "stopped-failure") {
						return commandResult{ExitCode: 1}, fmt.Errorf("injected new-start failure")
					}
					if !isNew && failure == "restore-start" {
						return commandResult{ExitCode: 1}, fmt.Errorf("injected restore-start failure")
					}
					newStarted = isNew
					running = true
				default:
					t.Fatalf("service registration mutated: %v", args)
				}
				return commandResult{}, nil
			}
			err := installBinariesTransactional(ctx, &fakeUI{}, deps, asset("ctl"), asset("daemon"), ctl, daemon)
			if (err != nil) != (failure != "none" && failure != "stopped-success" && failure != "stop-flake") {
				t.Fatalf("failure=%s err=%v", failure, err)
			}
			wantCtl, wantDaemon := "old-ctl", "old-daemon"
			if failure == "none" || failure == "stopped-success" || failure == "stop-flake" {
				wantCtl = "new-ctl"
				wantDaemon = "new-daemon"
			}
			if failure == "restore-first-failure" {
				wantCtl = "new-ctl"
			}
			for path, want := range map[string]string{ctl: wantCtl, daemon: wantDaemon, cfg: "original+second synthetic connection"} {
				b, e := os.ReadFile(path)
				if e != nil || string(b) != want {
					t.Fatalf("file not restored/preserved %s: %s %v", path, b, e)
				}
			}
			if (failure == "daemon-download" || failure == "sha-mismatch") && len(commands) != 0 {
				t.Fatalf("service touched on failed staging: %v", commands)
			}
			if (failure == "none" || failure == "stopped-success" || failure == "stop-flake") && !running {
				t.Fatal("successful service not running")
			}
			if (failure == "new-start" || failure == "new-health") && !running {
				t.Fatal("original service not restored running")
			}
			if (failure == "stopped-failure" || failure == "restore-first-failure") && running {
				t.Fatal("must not restart stopped-origin or inconsistent rollback")
			}
			if failure == "ctx-cancel" && (!errors.Is(err, context.Canceled) || !running) {
				t.Fatalf("cancellation not recovered/wrapped: %v running=%v", err, running)
			}
			if failure == "stop-flake" && !running {
				t.Fatal("stop confirmation flake did not recover original service")
			}
			if failure == "restore-start" && (err == nil || !strings.Contains(err.Error(), "previous binaries restored but restart failed")) {
				t.Fatalf("rollback restart failure hidden: %v", err)
			}
		})
	}
}

// Existing flow tests used a status-always-running fake; transaction tests need
// a fake that actually models stop/start and old files for a complete backup.
func prepareExistingInstallFixture(t *testing.T, dir string, deps *installerDeps) {
	t.Helper()
	for _, name := range []string{"gstreco-tally-agentctl.exe", "gstreco-tally-agent.exe"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old-binary"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	original := deps.runCommand
	stopped := false
	deps.runCommand = func(ctx context.Context, name string, args ...string) (commandResult, error) {
		if len(args) == 1 && args[0] == "--version" {
			prefix := "agentctl "
			if filepath.Base(name) == "gstreco-tally-agent.exe" {
				prefix = "gstreco-tally-agent "
			}
			return commandResult{Output: prefix + "0.1.0 (fixture, today)"}, nil
		}
		result, err := original(ctx, name, args...)
		if len(args) >= 2 && args[0] == "service" {
			if args[1] == "stop" && err == nil {
				stopped = true
			}
			if args[1] == "start" && err == nil {
				stopped = false
			}
			if args[1] == "status" && stopped {
				return commandResult{Output: "service status: stopped"}, nil
			}
		}
		return result, err
	}
}

func TestPruneBinaryBackupsKeepsNewestAndCurrent(t *testing.T) {
	dir := t.TempDir()
	current := filepath.Join(dir, ".gstreco-backup-current")
	for i, name := range []string{"oldest", "older", "newest", "current"} {
		path := filepath.Join(dir, ".gstreco-backup-"+name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(i), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "other-directory")
	if err := os.Mkdir(unrelated, 0700); err != nil {
		t.Fatal(err)
	}
	if err := pruneBinaryBackups(dir, current, 3); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"older", "newest", "current"} {
		if _, err := os.Stat(filepath.Join(dir, ".gstreco-backup-"+name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".gstreco-backup-oldest")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest retained: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatal(err)
	}
}

func TestReplaceDownloadedFileFailsClosedOnStaleBackupRemoval(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "binary")
	staged := filepath.Join(dir, "staged")
	if err := os.WriteFile(dest, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, []byte("new"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dest+".bak", 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest+".bak", "locked"), []byte("retain"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := replaceDownloadedFile(staged, dest); err == nil || !strings.Contains(err.Error(), "remove stale backup") {
		t.Fatal("ignored stale backup removal failure")
	}
	b, _ := os.ReadFile(dest)
	if string(b) != "old" {
		t.Fatal("original changed")
	}
}

func testInstallerDeps() installerDeps {
	d := defaultInstallerDeps()
	d.observeService = queryServiceState
	return d
}
