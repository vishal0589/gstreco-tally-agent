package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestIndependentRealUnexecutableDaemon(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux process-boundary reproduction")
	}
	d := t.TempDir()
	ctl := filepath.Join(d, "ctl")
	daemon := filepath.Join(d, "daemon")
	state := filepath.Join(d, "running")
	old := fmt.Sprintf("#!/bin/sh\ncase \"$2\" in\nstatus) if [ -f '%s' ]; then echo 'service status: running'; else echo 'service status: stopped'; fi;;\nstop) rm -f '%s';;\nstart) touch '%s';;\nesac\n", state, state, state)
	os.WriteFile(ctl, []byte("old ctl"), 0700)
	os.WriteFile(daemon, []byte(old), 0700)
	os.WriteFile(state, nil, 0600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "not an executable") }))
	defer srv.Close()
	deps := testInstallerDeps()
	deps.httpClient = srv.Client()
	asset := installerAsset{URL: srv.URL, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("not an executable")))}
	err := installBinariesTransactional(context.Background(), &fakeUI{}, deps, asset, asset, ctl, daemon)
	b, _ := os.ReadFile(daemon)
	_, runningErr := os.Stat(state)
	t.Logf("error=%v; daemon=%q; running=%v", err, b, runningErr == nil)
	if string(b) != old || runningErr != nil {
		t.Fatal("expected original executable and running service after failed start")
	}
}

// Unlike the preflight rejection above, this crosses the stop/replace boundary:
// a once-executable download becomes unexecutable at its installed path.
func TestRealRecoveryControllerSurvivesInvalidInstalledDaemon(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux real-process test; native SCM equivalent in Windows suite")
	}
	d := t.TempDir()
	ctl := filepath.Join(d, "ctl")
	daemon := filepath.Join(d, "daemon")
	state := filepath.Join(d, "running")
	old := fmt.Sprintf("#!/bin/sh\ncase \"$2\" in\nstatus) if [ -f '%s' ]; then echo 'service status: running'; else echo 'service status: stopped'; fi;;\nstop) rm -f '%s';;\nstart) touch '%s';;\nesac\n", state, state, state)
	os.WriteFile(ctl, []byte("old ctl"), 0700)
	os.WriteFile(daemon, []byte(old), 0700)
	os.WriteFile(state, nil, 0600)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, old) }))
	defer srv.Close()
	deps := testInstallerDeps()
	deps.httpClient = srv.Client()
	run := deps.runCommand
	poisoned := false
	deps.runCommand = func(ctx context.Context, path string, args ...string) (commandResult, error) {
		if path == daemon && len(args) == 2 && args[1] == "start" && !poisoned {
			poisoned = true
			if e := os.WriteFile(daemon, []byte("invalid"), 0700); e != nil {
				t.Fatal(e)
			}
		}
		return run(ctx, path, args...)
	}
	asset := installerAsset{URL: srv.URL, SHA256: fmt.Sprintf("%x", sha256.Sum256([]byte(old)))}
	err := installBinariesTransactional(context.Background(), &fakeUI{}, deps, asset, asset, ctl, daemon)
	b, _ := os.ReadFile(daemon)
	c, _ := os.ReadFile(ctl)
	_, e := os.Stat(state)
	if err == nil || !poisoned || string(b) != old || string(c) != "old ctl" || e != nil {
		t.Fatalf("recovery failed: %v daemon=%q ctl=%q running=%v", err, b, c, e == nil)
	}
}
