package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIndependentRestoreSetContinuesOnReplacementFailure(t *testing.T) {
	d := t.TempDir()
	stage := filepath.Join(d, "stage")
	backup := filepath.Join(d, "backup")
	os.Mkdir(stage, 0700)
	os.Mkdir(backup, 0700)
	ctl := filepath.Join(d, "ctl")
	daemon := filepath.Join(d, "daemon")
	for _, p := range []string{ctl, daemon} {
		os.WriteFile(p, []byte("new"), 0700)
		os.WriteFile(filepath.Join(backup, filepath.Base(p)), []byte("old"), 0700)
	}
	os.Mkdir(ctl+".bak", 0700)
	os.WriteFile(filepath.Join(ctl+".bak", "obstacle"), []byte("keep"), 0700)
	err := restoreBinarySet(stage, backup, []string{ctl, daemon})
	if err == nil || !strings.Contains(err.Error(), "restore replacement") {
		t.Fatalf("missing error: %v", err)
	}
	for p, want := range map[string]string{ctl: "new", daemon: "old"} {
		b, _ := os.ReadFile(p)
		if string(b) != want {
			t.Fatalf("%s = %q want %q", p, b, want)
		}
	}
	for _, p := range []string{ctl, daemon} {
		b, _ := os.ReadFile(filepath.Join(backup, filepath.Base(p)))
		if string(b) != "old" {
			t.Fatal("backup changed")
		}
	}
}

func TestIndependentRestoreSetAggregatesFailures(t *testing.T) {
	d := t.TempDir()
	os.Mkdir(filepath.Join(d, "stage"), 0700)
	os.Mkdir(filepath.Join(d, "backup"), 0700)
	err := restoreBinarySet(filepath.Join(d, "stage"), filepath.Join(d, "backup"), []string{filepath.Join(d, "ctl"), filepath.Join(d, "daemon")})
	if err == nil || !strings.Contains(err.Error(), "ctl") || !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("not aggregated: %v", err)
	}
}

func TestIndependentTransaction(t *testing.T) {
	for _, mode := range []string{"success", "health-failure", "cancel", "stopped-partial-start", "unexecutable-daemon", "exact-pending-health", "exact-stop-pending"} {
		t.Run(mode, func(t *testing.T) {
			d := t.TempDir()
			ctl := filepath.Join(d, "ctl.exe")
			daemon := filepath.Join(d, "daemon.exe")
			os.WriteFile(ctl, []byte("old-ctl"), 0700)
			os.WriteFile(daemon, []byte("old-daemon"), 0700)
			for i := 0; i < 5; i++ {
				p := filepath.Join(d, fmt.Sprintf(".gstreco-backup-old%d", i))
				os.Mkdir(p, 0700)
				stamp := time.Unix(int64(i), 0)
				os.Chtimes(p, stamp, stamp)
			}
			outside := t.TempDir()
			os.WriteFile(filepath.Join(outside, "sentinel"), []byte("untouched"), 0700)
			os.Symlink(outside, filepath.Join(d, ".gstreco-backup-symlink"))
			os.Mkdir(filepath.Join(d, "unrelated"), 0700)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, "new-"+strings.TrimPrefix(r.URL.Path, "/"))
			}))
			defer srv.Close()
			asset := func(n string) installerAsset {
				return installerAsset{URL: srv.URL + "/" + n, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("new-"+n)))}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			deps := testInstallerDeps()
			deps.httpClient = srv.Client()
			running := mode != "stopped-partial-start"
			started := false
			health := 0
			recoveryCommands := 0
			oldStarts := 0
			deps.runCommand = func(c context.Context, p string, args ...string) (commandResult, error) {
				if len(args) == 1 && args[0] == "--version" {
					return commandResult{}, nil
				}
				b, _ := os.ReadFile(p)
				isNew := string(b) == "new-daemon"
				if ctx.Err() != nil {
					recoveryCommands++
					deadline, ok := c.Deadline()
					if c.Err() != nil || !ok || time.Until(deadline) < 179*time.Second {
						t.Fatal("recovery lacks independent 3-minute budget")
					}
				}
				if mode == "unexecutable-daemon" && isNew {
					return commandResult{ExitCode: -1}, fmt.Errorf("exec format error")
				}
				switch args[1] {
				case "stop":
					running = false
				case "start":
					if !isNew {
						oldStarts++
					}
					if isNew {
						started = true
						if mode == "cancel" {
							cancel()
							return commandResult{}, context.Canceled
						}
						if mode == "stopped-partial-start" {
							running = true
							return commandResult{}, fmt.Errorf("started but command failed")
						}
					}
					running = true
				case "status":
					if isNew && started {
						health++
						dirs, _ := filepath.Glob(filepath.Join(d, ".gstreco-backup-old*"))
						if len(dirs) != 5 {
							t.Fatal("pruned before all health checks")
						}
						if mode == "health-failure" {
							return commandResult{Output: "service status: stopped"}, nil
						}
					}
					state := "stopped"
					if running {
						state = "running"
					}
					return commandResult{Output: "service status: " + state}, nil
				}
				return commandResult{}, nil
			}
			originalObserve := deps.observeService
			pendingStopObserved := false
			deps.observeService = func(c context.Context, run func(context.Context, string, ...string) (commandResult, error), path string) (serviceState, error) {
				state, err := originalObserve(c, run, path)
				if mode == "exact-pending-health" && started && running {
					b, _ := os.ReadFile(daemon)
					if string(b) == "new-daemon" {
						return servicePending, nil
					}
				}
				if mode == "exact-stop-pending" && !running && !pendingStopObserved {
					pendingStopObserved = true
					b, _ := os.ReadFile(daemon)
					if string(b) != "old-daemon" {
						t.Fatal("replaced before final Stopped")
					}
					return servicePending, nil
				}
				return state, err
			}
			err := installBinariesTransactional(ctx, &fakeUI{}, deps, asset("ctl"), asset("daemon"), ctl, daemon)
			if mode == "success" || mode == "exact-stop-pending" {
				if err != nil || health != 3 || !running {
					t.Fatalf("success: err=%v health=%d running=%v", err, health, running)
				}
			} else {
				if err == nil {
					t.Fatal("failure hidden")
				}
				for p, want := range map[string]string{ctl: "old-ctl", daemon: "old-daemon"} {
					b, _ := os.ReadFile(p)
					if string(b) != want {
						t.Errorf("%s not restored: %q; err=%v", p, b, err)
					}
				}
				if mode == "stopped-partial-start" {
					if running || oldStarts != 0 {
						t.Fatal("stopped origin restarted")
					}
				} else if !running {
					t.Errorf("original running state not recovered: %v", err)
				}
			}
			if mode == "cancel" && recoveryCommands == 0 {
				t.Fatal("no recovery commands")
			}
			for i := 0; i < 5; i++ {
				_, e := os.Stat(filepath.Join(d, fmt.Sprintf(".gstreco-backup-old%d", i)))
				want := (mode != "success" && mode != "exact-stop-pending") || i >= 3
				if (e == nil) != want {
					t.Fatalf("wrong backup pruned: old%d err=%v", i, e)
				}
			}
			for _, p := range []string{filepath.Join(outside, "sentinel"), filepath.Join(d, "unrelated")} {
				if _, e := os.Stat(p); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestIndependentPruneCurrentOldestAndLinks(t *testing.T) {
	d := t.TempDir()
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	os.WriteFile(sentinel, []byte("retain"), 0700)
	current := filepath.Join(d, ".gstreco-backup-current")
	os.Mkdir(current, 0700)
	stamp := time.Unix(-100, 0)
	os.Chtimes(current, stamp, stamp)
	for i := 0; i < 5; i++ {
		p := filepath.Join(d, fmt.Sprintf(".gstreco-backup-%d", i))
		os.Mkdir(p, 0700)
		stamp = time.Unix(int64(i), 0)
		os.Chtimes(p, stamp, stamp)
	}
	os.Symlink(outside, filepath.Join(d, ".gstreco-backup-link"))
	os.WriteFile(filepath.Join(d, ".gstreco-backup-file"), []byte("retain"), 0700)
	if err := pruneBinaryBackups(d, current, 3); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{current, filepath.Join(d, ".gstreco-backup-3"), filepath.Join(d, ".gstreco-backup-4"), sentinel, filepath.Join(d, ".gstreco-backup-file")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		if _, err := os.Stat(filepath.Join(d, fmt.Sprintf(".gstreco-backup-%d", i))); !os.IsNotExist(err) {
			t.Fatalf("wrong retention %d: %v", i, err)
		}
	}
}
