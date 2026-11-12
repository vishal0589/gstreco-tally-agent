package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stage both downloads and preserve both originals before changing the shared
// service. Never uninstall an existing service as an update repair fallback.
// Pairing credentials precede this transaction and are deliberately not undone.
func installBinariesTransactional(ctx context.Context, ui installerUI, deps installerDeps, ctlAsset, daemonAsset installerAsset, ctlPath, daemonPath string) error {
	stage, err := os.MkdirTemp(filepath.Dir(daemonPath), ".gstreco-install-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, a := range []struct {
		asset installerAsset
		name  string
	}{{ctlAsset, "agentctl"}, {daemonAsset, "daemon"}} {
		if err := downloadAsset(ctx, deps.httpClient, a.asset.URL, filepath.Join(stage, a.name+".exe"), a.asset.SHA256); err != nil {
			return fmt.Errorf("stage %s: %w", a.name, err)
		}
	}
	// Catch an invalid/wrong-architecture daemon before stopping a healthy
	// service. This probe does not replace an independent recovery controller.
	probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
	probe, probeErr := deps.runCommand(probeCtx, filepath.Join(stage, "daemon.exe"), "--version")
	cancelProbe()
	if probeErr != nil || probe.ExitCode != 0 {
		if probeErr == nil {
			probeErr = fmt.Errorf("exit code %d", probe.ExitCode)
		}
		return fmt.Errorf("staged daemon executable preflight failed; no binaries or service changed: %w", probeErr)
	}

	state, err := deps.observeService(ctx, deps.runCommand, daemonPath)
	if err != nil || (state != serviceRunning && state != serviceStopped && state != serviceNotInstalled) {
		return fmt.Errorf("cannot establish original service state; no binaries changed")
	}
	if state == serviceNotInstalled {
		for _, a := range []struct{ name, path string }{{"agentctl", ctlPath}, {"daemon", daemonPath}} {
			if err := replaceDownloadedFile(filepath.Join(stage, a.name+".exe"), a.path); err != nil {
				return err
			}
		}
		if err := ensureServiceHealthy(ctx, deps.runCommand, daemonPath); err != nil {
			return err
		}
		return verifyObservedServiceRunning(ctx, deps.observeService, deps.runCommand, daemonPath)
	}
	backup, err := os.MkdirTemp(filepath.Dir(daemonPath), ".gstreco-backup-")
	if err != nil {
		return err
	}
	for _, p := range []string{ctlPath, daemonPath} {
		err := copyBinaryExclusive(p, filepath.Join(backup, filepath.Base(p)))
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("existing service has missing binary %s; cannot prepare complete rollback; no binaries changed", p)
		}
		if err != nil {
			return fmt.Errorf("preserve %s: %w; originals unchanged, backup %s", p, err, backup)
		}
	}
	// The saved daemon controls the same fixed SCM service by name; it never
	// installs/re-registers it. Its bytes/path survive replacement of both EXEs.
	controllerPath := filepath.Join(backup, filepath.Base(daemonPath))
	confirmedOriginal, controllerErr := deps.observeService(ctx, deps.runCommand, controllerPath)
	if controllerErr != nil || confirmedOriginal != state {
		return fmt.Errorf("preserved service controller verification failed; no binaries changed")
	}

	ui.Infof("Previous binaries preserved in %s until service health is verified.", backup)
	restore := func(cause error) error {
		// Caller cancellation must not skip recovery of a stopped shared service.
		recovery, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		current, _ := deps.observeService(recovery, deps.runCommand, controllerPath)
		if current != serviceNotInstalled && current != serviceStopped {
			if _, err := deps.runCommand(recovery, controllerPath, "service", "stop"); err != nil {
				return fmt.Errorf("%w; cannot stop service for safe rollback: %w; backups retained at %s", cause, err, backup)
			}
			if err := waitServiceStopped(recovery, deps.observeService, deps.runCommand, controllerPath); err != nil {
				return fmt.Errorf("%w; rollback stop not confirmed; backups retained at %s", cause, backup)
			}
		}
		if err := restoreBinarySet(stage, backup, []string{ctlPath, daemonPath}); err != nil {
			return fmt.Errorf("%w; rollback incomplete, service left stopped: %w; backups retained at %s", cause, err, backup)
		}
		if state == serviceRunning {
			if _, err := deps.runCommand(recovery, controllerPath, "service", "start"); err != nil {
				return fmt.Errorf("%w; previous binaries restored but restart failed: %w; backup %s", cause, err, backup)
			}
			if err := verifyObservedServiceRunning(recovery, deps.observeService, deps.runCommand, controllerPath); err != nil {
				return fmt.Errorf("%w; previous binaries restored but unhealthy: %w; backup %s", cause, err, backup)
			}
		}
		return fmt.Errorf("%w; previous binaries restored (original service state %s); backups retained at %s. Pairing credentials, if already saved, remain", cause, state, backup)
	}
	if state == serviceRunning {
		if _, err := deps.runCommand(ctx, controllerPath, "service", "stop"); err != nil {
			return restore(fmt.Errorf("stop before replacement: %w", err))
		}
		if err := waitServiceStopped(ctx, deps.observeService, deps.runCommand, controllerPath); err != nil {
			return restore(fmt.Errorf("service stop was not confirmed"))
		}
	}
	for _, a := range []struct{ name, path string }{{"agentctl", ctlPath}, {"daemon", daemonPath}} {
		if err := replaceDownloadedFile(filepath.Join(stage, a.name+".exe"), a.path); err != nil {
			return restore(fmt.Errorf("replace %s: %w", a.name, err))
		}
	}
	// An installer success activates the paired agent, even if it was stopped
	// before this run. Recovery alone preserves the original stopped state.
	if _, err := deps.runCommand(ctx, daemonPath, "service", "start"); err != nil {
		return restore(fmt.Errorf("start new service: %w", err))
	}
	if err := verifyObservedServiceRunning(ctx, deps.observeService, deps.runCommand, daemonPath); err != nil {
		return restore(err)
	}
	if err := pruneBinaryBackups(filepath.Dir(daemonPath), backup, 3); err != nil {
		ui.Warnf("Service is healthy, but old backup cleanup needs attention: %v", err)
	}
	ui.Infof("Agent service verified running. Previous binaries retained at %s.", backup)
	return nil
}

func copyBinaryExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	return closeErr
}

type serviceObserver func(context.Context, func(context.Context, string, ...string) (commandResult, error), string) (serviceState, error)

func verifyServiceRunning(ctx context.Context, run func(context.Context, string, ...string) (commandResult, error), path string) error {
	return verifyObservedServiceRunning(ctx, observePlatformService, run, path)
}
func verifyObservedServiceRunning(ctx context.Context, observe serviceObserver, run func(context.Context, string, ...string) (commandResult, error), path string) error {
	for i := 0; i < 3; i++ {
		state, err := observe(ctx, run, path)
		if err != nil || state != serviceRunning {
			return fmt.Errorf("service health check failed: %s (%v)", state, err)
		}
		if i < 2 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}
	return nil
}

// Try every original, even when one cannot be restored. Never restart a mixed
// binary set: the caller only restarts after this function fully succeeds.
func restoreBinarySet(stage, backup string, paths []string) error {
	var failures []error
	for i, p := range paths {
		staged := filepath.Join(stage, fmt.Sprintf("restore-%d", i))
		if err := copyBinaryExclusive(filepath.Join(backup, filepath.Base(p)), staged); err != nil {
			failures = append(failures, fmt.Errorf("restore staging %s: %w", p, err))
			continue
		}
		if err := replaceDownloadedFile(staged, p); err != nil {
			failures = append(failures, fmt.Errorf("restore replacement %s: %w", p, err))
		}
	}
	return errors.Join(failures...)
}

// Only called after successful health verification. Keep this transaction's
// backup plus the two newest older backups. Failures never trigger cleanup.
func pruneBinaryBackups(dir, current string, keep int) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	type backupInfo struct {
		path     string
		modified time.Time
	}
	var older []backupInfo
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if !entry.IsDir() || path == current || !strings.HasPrefix(entry.Name(), ".gstreco-backup-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		older = append(older, backupInfo{path, info.ModTime()})
	}
	sort.Slice(older, func(i, j int) bool {
		if older[i].modified.Equal(older[j].modified) {
			return older[i].path > older[j].path
		}
		return older[i].modified.After(older[j].modified)
	})
	var failures []error
	for i := keep - 1; i < len(older); i++ {
		if err := os.RemoveAll(older[i].path); err != nil {
			failures = append(failures, fmt.Errorf("prune %s: %w", older[i].path, err))
		}
	}
	return errors.Join(failures...)
}

// Only exact final Stopped permits binary replacement. Pending/paused/unknown
// states never count as stopped, including old controller output collapsed by
// kardianos. Windows observations bypass that output entirely.
func waitServiceStopped(ctx context.Context, observe serviceObserver, run func(context.Context, string, ...string) (commandResult, error), path string) error {
	wait, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for {
		state, err := observe(wait, run, path)
		if err != nil {
			return err
		}
		if state == serviceStopped {
			return nil
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("waiting for final Stopped: %w", wait.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}
