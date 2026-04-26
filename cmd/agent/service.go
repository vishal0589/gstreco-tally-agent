package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/kardianos/service"
	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/log"
)

// Service config — the same on Windows (SCM), macOS (launchd), and
// Linux (systemd). Display fields lifted from the master plan's
// marketing copy; Description shows in `services.msc` and `systemctl
// status`.
//
// Values are lowercase Latin-only because Windows SCM rejects
// non-ASCII service names and Linux/macOS happily accept any string.
// One config = three platforms.
const (
	serviceName        = "gstreco-tally-agent"
	serviceDisplayName = "GST Reco Tally Agent"
	serviceDescription = "Syncs TallyPrime vouchers to GST Reco for GSTR-2B reconciliation."
)

// serviceConfig builds the kardianos config from runtime state. Args
// passed here are forwarded by the SCM/launchd/systemd to the
// service binary on each start, so we propagate the operator's
// `--config` / `--log-level` choices through to the daemon path.
func serviceConfig(args []string) *service.Config {
	return &service.Config{
		Name:        serviceName,
		DisplayName: serviceDisplayName,
		Description: serviceDescription,
		Arguments:   args,
	}
}

// daemonProgram implements service.Interface. The same struct backs
// "run as service" (SCM-driven Start/Stop) and "run interactively"
// (Ctrl+C). kardianos auto-detects which mode we're in.
type daemonProgram struct {
	opts   daemonOptions
	logger zerolog.Logger
	cancel context.CancelFunc
	done   chan struct{}
}

// Start is called by kardianos when the SCM/launchd/systemd asks the
// service to start. Must NOT block — we kick off the work in a
// goroutine and return immediately.
func (p *daemonProgram) Start(_ service.Service) error {
	p.logger.Info().Msg("service start")
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	go func() {
		defer close(p.done)
		// run() reads opts + does the real daemon loop. We wrap it
		// with the service-driven ctx so Stop() can signal shutdown.
		exit := runWithCtx(ctx, p.opts, p.logger)
		p.logger.Info().Int("exit_code", exit).Msg("daemon goroutine returned")
	}()
	return nil
}

// Stop is called by the service manager. Must complete promptly
// (Windows SCM gives ~30s before forcefully terminating). We signal
// the daemon ctx, wait for the run goroutine to drain, and return.
func (p *daemonProgram) Stop(_ service.Service) error {
	p.logger.Info().Msg("service stop")
	if p.cancel != nil {
		p.cancel()
	}
	if p.done == nil {
		return nil
	}
	select {
	case <-p.done:
		p.logger.Info().Msg("daemon stopped cleanly")
	case <-time.After(25 * time.Second):
		p.logger.Warn().Msg("daemon did not stop within 25s; service manager may force-terminate")
	}
	return nil
}

// runDaemonViaService wraps the run() function in a kardianos
// service.Run loop. Called from main() when no service-control
// subcommand was passed. kardianos detects whether we're under an
// SCM and either:
//   - runs Start/Stop via the platform's service manager (production)
//   - runs interactively (Ctrl+C calls Stop, useful for `make run`)
func runDaemonViaService(opts daemonOptions, logger zerolog.Logger) int {
	prg := &daemonProgram{opts: opts, logger: logger}
	cfg := serviceConfig(nil) // Arguments only matter for Install/Update.
	svc, err := service.New(prg, cfg)
	if err != nil {
		logger.Error().Err(err).Msg("service.New failed")
		return 1
	}
	if err := svc.Run(); err != nil {
		logger.Error().Err(err).Msg("service.Run failed")
		return 1
	}
	return 0
}

// serviceControlMain handles the `service` subcommand: install,
// uninstall, start, stop, restart, status. Returns the process exit
// code so main() can os.Exit cleanly.
//
// Operator usage on Windows (Administrator-elevated cmd.exe):
//
//	gstreco-tally-agent.exe service install
//	gstreco-tally-agent.exe service start
//	gstreco-tally-agent.exe service status
//
// On macOS/Linux (with sudo where service installation requires
// privileged access — typically /Library/LaunchDaemons or
// /etc/systemd/system).
func serviceControlMain(args []string) int {
	return runServiceControl(os.Stdout, os.Stderr, args)
}

// runServiceControl is the testable form of serviceControlMain.
// stdout/stderr injected so tests can assert on output.
func runServiceControl(stdout, stderr io.Writer, args []string) int {
	if len(args) < 1 {
		printServiceUsage(stderr)
		return 2
	}
	action := strings.ToLower(strings.TrimSpace(args[0]))
	if action == "help" || action == "--help" || action == "-h" {
		printServiceUsage(stdout)
		return 0
	}

	// Validate the action up-front so a typo doesn't cascade into a
	// confusing kardianos error.
	if _, ok := allowedServiceActions[action]; !ok {
		fmt.Fprintf(stderr, "service: unknown action %q (try install, uninstall, start, stop, restart, status, help)\n", action)
		return 2
	}

	// Default install args forwarded to the service every time it
	// starts. Operators can override at install time:
	//   gstreco-tally-agent service install --config "C:\custom\config.yaml"
	//
	// Empty by default — daemon falls back to config.DefaultPath()
	// (%ProgramData%\GST Reco\agent\config.yaml on Windows).
	installArgs := args[1:]

	logger := log.New(log.Options{Level: "info", Console: true})
	prg := &daemonProgram{logger: logger}

	cfg := serviceConfig(installArgs)
	svc, err := service.New(prg, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "service: %v\n", err)
		return 1
	}

	switch action {
	case "install":
		if err := svc.Install(); err != nil {
			fmt.Fprintf(stderr, "service install: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "✓ service installed. Start with: gstreco-tally-agent service start")
		return 0
	case "uninstall":
		// Stop first so we leave behind a clean state. Ignore the
		// error from Stop — service may not be running.
		_ = svc.Stop()
		if err := svc.Uninstall(); err != nil {
			fmt.Fprintf(stderr, "service uninstall: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "✓ service uninstalled")
		return 0
	case "start":
		if err := svc.Start(); err != nil {
			fmt.Fprintf(stderr, "service start: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "✓ service started")
		return 0
	case "stop":
		if err := svc.Stop(); err != nil {
			fmt.Fprintf(stderr, "service stop: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "✓ service stopped")
		return 0
	case "restart":
		if err := svc.Restart(); err != nil {
			fmt.Fprintf(stderr, "service restart: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "✓ service restarted")
		return 0
	case "status":
		st, err := svc.Status()
		if err != nil {
			if errors.Is(err, service.ErrNotInstalled) {
				fmt.Fprintln(stdout, "service not installed")
				return 0
			}
			fmt.Fprintf(stderr, "service status: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "service status: %s\n", statusName(st))
		return 0
	}
	return 0
}

// allowedServiceActions is the validation set. Centralised so the
// help text and the switch statement can't drift.
var allowedServiceActions = map[string]struct{}{
	"install":   {},
	"uninstall": {},
	"start":     {},
	"stop":      {},
	"restart":   {},
	"status":    {},
}

// statusName converts kardianos's enum to a human label. Stable
// across platforms because we don't expose the int value to the
// operator.
func statusName(st service.Status) string {
	switch st {
	case service.StatusRunning:
		return "running"
	case service.StatusStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

func printServiceUsage(w io.Writer) {
	fmt.Fprintln(w, `gstreco-tally-agent service — manage the daemon as a system service

Usage:
  gstreco-tally-agent service <action> [install-args...]

Actions:
  install     Register the daemon with the platform service manager
              (Windows SCM, macOS launchd, or Linux systemd). Add
              install-args to bake them into every service start
              (e.g. --config "C:\Custom\config.yaml").
  uninstall   Stop the service if running, then remove the registration.
  start       Tell the service manager to start the daemon. Auto-starts
              on next boot once installed.
  stop        Tell the service manager to stop the daemon.
  restart     Stop + start.
  status      Print "running" / "stopped" / "not installed".

Operator notes:
  - Run from an Administrator command prompt on Windows. Without
    elevation, install / uninstall / start / stop will fail with a
    permissions error from the SCM.
  - On macOS/Linux, run with sudo for install / uninstall (writes to
    /Library/LaunchDaemons or /etc/systemd/system). start / stop /
    status work without sudo once the service is installed.
  - The daemon reads its config from %ProgramData%\GST Reco\agent\
    config.yaml on Windows or ~/.gstreco-agent/config.yaml elsewhere
    by default. Pass --config at install time to override.`)
}
