package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	os.Exit(runInstallerMain(os.Stdout, os.Stderr, os.Stdin, os.Args[1:], defaultInstallerDeps()))
}

func runInstallerMain(stdout, stderr, stdin *os.File, args []string, deps installerDeps) int {
	opts, exitCode, action, err := parseInstallerArgs(stdout, stderr, args)
	if err != nil {
		fmt.Fprintf(stderr, "installer: %v\n", err)
		return exitCode
	}
	switch action {
	case installerActionHelp:
		return 0
	case installerActionVersion:
		fmt.Fprintln(stdout, deps.version())
		return 0
	}
	if deps.goos() != "windows" {
		fmt.Fprintln(stderr, "installer: Windows is the only supported platform for this flow.")
		return 2
	}

	ctx, cancel := signalContext(25 * time.Minute)
	defer cancel()

	ui := newConsoleUI(stdout, stderr, stdin)
	return runInstaller(ctx, ui, opts, deps)
}

type installerOptions struct {
	server      string
	configPath  string
	installDir  string
	customPorts string
	addTenant   bool
	noBrowser   bool
}

type installerAction string

const (
	installerActionRun     installerAction = "run"
	installerActionHelp    installerAction = "help"
	installerActionVersion installerAction = "version"
)

func parseInstallerArgs(stdout, stderr *os.File, args []string) (installerOptions, int, installerAction, error) {
	fs := flag.NewFlagSet("gstreco-tally-installer", flag.ContinueOnError)
	fs.SetOutput(stderr)

	opts := installerOptions{}
	fs.StringVar(&opts.server, "server", "https://gstreco.m2ai.ai", "GST Reco server URL")
	fs.StringVar(&opts.configPath, "config", "", "override config file path")
	fs.StringVar(&opts.installDir, "install-dir", defaultInstallDir(), "directory for agent binaries")
	fs.StringVar(&opts.customPorts, "custom-ports", "", "comma-separated custom Tally ports to try after default discovery fails")
	fs.BoolVar(&opts.addTenant, "add-tenant", false, "start a fresh browser approval to add another GST Reco tenant on this machine")
	fs.BoolVar(&opts.noBrowser, "no-browser", false, "print the approval URL instead of opening it automatically")

	versionFlag := fs.Bool("version", false, "print version and exit")
	helpFlag := fs.Bool("help", false, "show help")

	if err := fs.Parse(args); err != nil {
		return installerOptions{}, 2, installerActionRun, err
	}
	if *helpFlag {
		printInstallerUsage(stdout)
		return installerOptions{}, 0, installerActionHelp, nil
	}
	if *versionFlag {
		return installerOptions{}, 0, installerActionVersion, nil
	}
	return opts, 0, installerActionRun, nil
}

func printInstallerUsage(stdout *os.File) {
	fmt.Fprintln(stdout, `gstreco-tally-installer — Windows-first onboarding for GST Reco

Usage:
  gstreco-tally-installer [--server <URL>] [--config <PATH>] [--install-dir <PATH>]
                          [--custom-ports 2026,9000] [--add-tenant] [--no-browser]

Behavior:
  - Starts a browser-authorized installer session
  - Opens GST Reco for approval
  - Claims credentials after approval
  - Downloads or refreshes the agent binaries
  - Installs or repairs the Windows service
  - Runs discovery-first and only falls back to custom ports if needed
  - On an already paired machine, rerun with --add-tenant to pair another GST Reco tenant

Fallbacks:
  - agentctl pair --code <CODE> stays available for support scenarios
  - install.ps1 stays available as the advanced recovery path`)
}

func signalContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
