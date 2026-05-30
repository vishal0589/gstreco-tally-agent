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
	code        string
	port        string
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
	fs.StringVar(&opts.code, "code", "", "pair/setup code (default: read from the installer filename gstreco-tally-setup-<CODE>.exe)")
	fs.StringVar(&opts.port, "port", "", "Tally port to confirm at the prompt (default 9000; comma-separate several; host:port for a remote Tally)")

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
  gstreco-tally-setup-<CODE>.exe                  (one-click: run it, confirm the Tally port)
  gstreco-tally-installer [--code <CODE>] [--port 9000] [--server <URL>]
                          [--config <PATH>] [--install-dir <PATH>]
                          [--custom-ports 2026,9000] [--add-tenant] [--no-browser]

Behavior:
  - Setup-bundle (default for clients): reads the pair code from the installer's
    own filename (or --code), asks for the Tally port, claims the code, and
    installs the Windows service. No login, no browser.
  - With no code (the generic installer): starts a browser-authorized approval
    session for whoever is signed in on this machine.
  - Downloads or refreshes the agent binaries and installs/repairs the service.
  - Checks the confirmed Tally port first, then the common ports.
  - On an already paired machine, rerun with --add-tenant to add another tenant.

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
