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

func TestWaitServiceStoppedRequiresFinalState(t *testing.T) {
	for _, state := range []serviceState{servicePending, servicePaused, serviceUnknown, serviceRunning} {
		t.Run(string(state), func(t *testing.T) {
			calls := 0
			observe := func(context.Context, func(context.Context, string, ...string) (commandResult, error), string) (serviceState, error) {
				calls++
				if calls == 1 {
					return state, nil
				}
				return serviceStopped, nil
			}
			if err := waitServiceStopped(context.Background(), observe, nil, "preserved-old-controller.exe"); err != nil || calls != 2 {
				t.Fatalf("accepted nonfinal state: calls=%d err=%v", calls, err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			observe = func(context.Context, func(context.Context, string, ...string) (commandResult, error), string) (serviceState, error) {
				return state, nil
			}
			if err := waitServiceStopped(ctx, observe, nil, "preserved-old-controller.exe"); err == nil {
				t.Fatal("unbounded/nonfinal stop accepted")
			}
		})
	}
}
func TestHealthRejectsEveryNonRunningState(t *testing.T) {
	for _, state := range []serviceState{serviceStopped, servicePending, servicePaused, serviceUnknown, serviceNotInstalled} {
		observe := func(context.Context, func(context.Context, string, ...string) (commandResult, error), string) (serviceState, error) {
			return state, nil
		}
		if err := verifyObservedServiceRunning(context.Background(), observe, nil, "preserved-old-controller.exe"); err == nil {
			t.Fatalf("false health: %s", state)
		}
	}
}

func TestTransactionInitialNonfinalStateFailsBeforeMutation(t *testing.T) {
	for _, initial := range []serviceState{servicePending, servicePaused, serviceUnknown} {
		t.Run(string(initial), func(t *testing.T) {
			dir := t.TempDir()
			ctl := filepath.Join(dir, "ctl.exe")
			daemon := filepath.Join(dir, "daemon.exe")
			for p, b := range map[string]string{ctl: "old-ctl", daemon: "old-daemon"} {
				if e := os.WriteFile(p, []byte(b), 0700); e != nil {
					t.Fatal(e)
				}
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "new") }))
			defer srv.Close()
			asset := installerAsset{URL: srv.URL, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("new")))}
			deps := testInstallerDeps()
			deps.httpClient = srv.Client()
			observations := 0
			commands := 0
			deps.observeService = func(context.Context, func(context.Context, string, ...string) (commandResult, error), string) (serviceState, error) {
				observations++
				if observations == 1 {
					return initial, nil
				}
				return serviceStopped, nil
			}
			deps.runCommand = func(_ context.Context, _ string, args ...string) (commandResult, error) {
				if len(args) == 1 && args[0] == "--version" {
					return commandResult{}, nil
				}
				commands++
				return commandResult{}, nil
			}
			err := installBinariesTransactional(context.Background(), &fakeUI{}, deps, asset, asset, ctl, daemon)
			if err == nil || !strings.Contains(err.Error(), "cannot establish original service state") {
				t.Fatalf("initial %s accepted: %v", initial, err)
			}
			backups, _ := filepath.Glob(filepath.Join(dir, ".gstreco-backup-*"))
			if len(backups) != 0 || commands != 0 || observations != 1 {
				t.Fatalf("nonfinal state crossed gate: backups=%v commands=%d observations=%d", backups, commands, observations)
			}
			for p, want := range map[string]string{ctl: "old-ctl", daemon: "old-daemon"} {
				b, e := os.ReadFile(p)
				if e != nil || string(b) != want {
					t.Fatalf("original changed %s: %q %v", p, b, e)
				}
			}
		})
	}
}
