package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/vishal0589/gstreco-tally-agent/internal/pair"
	"github.com/vishal0589/gstreco-tally-agent/internal/secretstore"
	"github.com/vishal0589/gstreco-tally-agent/internal/version"
)

// setupBundleCodeRE pulls the pair code out of the installer's own filename.
// The web app hands the installer to the client as
// "gstreco-tally-setup-<CODE>.exe", so the person at the Tally PC never types a
// code — they just run the file. Matching the full product prefix (not a bare
// "setup-") keeps unrelated binaries, including go test binaries, from ever
// looking like a coded installer. Tolerant of case and browser "(1)" suffixes.
var setupBundleCodeRE = regexp.MustCompile(`(?i)gstreco-tally-setup-([0-9a-z]{6})`)

// setupBundleCodeFromName extracts a pair code from the installer's filename,
// or "" when the file carries no code (the generic installer, or a renamed
// file). The code itself is validated later by pair.Claim.
func setupBundleCodeFromName(execPath string) string {
	base := filepath.Base(strings.TrimSpace(execPath))
	m := setupBundleCodeRE.FindStringSubmatch(base)
	if m == nil {
		return ""
	}
	return strings.ToUpper(m[1])
}

// resolveSetupBundleCode decides the pair code for this run: an explicit
// --code wins, then the code baked into the filename. If neither is present,
// an already-paired machine returns "" so runInstaller can update silently
// (no prompt), while an unpaired machine offers the manual setup key with a
// press-Enter fallback to browser sign-in.
func resolveSetupBundleCode(opts installerOptions, ui installerUI, alreadyPaired bool) string {
	if c := strings.TrimSpace(opts.code); c != "" {
		return strings.ToUpper(c)
	}
	if c := setupBundleCodeFromName(os.Args[0]); c != "" {
		return c
	}
	if alreadyPaired {
		// Update on an already-paired machine — don't ask for a setup key.
		return ""
	}
	entered := ui.ReadLine("Enter your GST Reco setup key (from your accountant), or press Enter to sign in with a browser: ")
	return strings.ToUpper(strings.TrimSpace(entered))
}

// promptTallyPorts always asks the operator to confirm the Tally port before
// pairing — the port is per-install and the one thing only the client knows.
// Pressing Enter accepts the default (or a preset from --port/--custom-ports).
// Accepts several comma-separated ports, and host:port for a Tally on another
// machine.
func promptTallyPorts(ui installerUI, preset string) string {
	def := strings.TrimSpace(preset)
	if def == "" {
		def = "9000"
	}
	ui.Infof("Tally usually listens on port 9000. If your Tally uses a different port, enter it now.")
	entered := strings.TrimSpace(ui.ReadLine(fmt.Sprintf("Tally port(s), comma-separated [%s]: ", def)))
	if entered == "" {
		return def
	}
	return entered
}

// runSetupBundleClaim is the one-click path: confirm the Tally port, then claim
// the pair code against the live /api/tally/pair/claim endpoint (no browser, no
// login). On an already-paired machine it only re-confirms the port unless
// --add-tenant was passed. Returns (exitCode, err); a non-nil err means stop
// with that exit code.
func runSetupBundleClaim(
	ctx context.Context,
	ui installerUI,
	opts *installerOptions,
	deps installerDeps,
	serverURL string,
	deviceName string,
	pairState localPairState,
	code string,
) (int, error) {
	preset := strings.TrimSpace(opts.port)
	if preset == "" {
		preset = strings.TrimSpace(opts.customPorts)
	}

	if pairState.State == "paired" && !opts.addTenant {
		ui.Infof("This machine is already connected to GST Reco.")
		opts.customPorts = promptTallyPorts(ui, preset)
		return 0, nil
	}

	// Always confirm the Tally port before pairing (customer requirement).
	opts.customPorts = promptTallyPorts(ui, preset)

	ui.Infof("Connecting this machine to GST Reco…")
	store := secretstore.NewFileStore(
		secretstore.DefaultDir(filepath.Dir(opts.configPath)),
		secretstore.ReadMachineID,
	)
	if _, err := deps.claimPair(ctx, pair.Options{
		Server:       serverURL,
		Code:         code,
		ConfigPath:   opts.configPath,
		Keyring:      store,
		DeviceName:   deviceName,
		AgentVersion: version.Version,
		HTTPClient:   deps.httpClient,
	}); err != nil {
		switch {
		case errors.Is(err, pair.ErrCodeGone):
			return 2, fmt.Errorf("this setup link has expired or was already used — ask your accountant to prepare a fresh installer link")
		case errors.Is(err, pair.ErrInvalidCode):
			return 2, fmt.Errorf("the setup key in this installer is not valid (%v) — re-download the link your accountant sent", err)
		default:
			return 1, fmt.Errorf("could not connect to GST Reco: %w", err)
		}
	}
	ui.Infof("Connected. Setting up the background service…")
	return 0, nil
}
