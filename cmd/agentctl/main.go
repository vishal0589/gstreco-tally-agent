// Command agentctl is the operator CLI. `agentctl pair` performs the one-shot
// claim flow against the GST Reco server; `agentctl status` reports local
// pairing state; real runtime status via a local IPC socket arrives in A5.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/pair"
	"github.com/vishal0589/gstreco-tally-agent/internal/secretstore"
	"github.com/vishal0589/gstreco-tally-agent/internal/version"
)

// defaultSecretStore returns the production secret backend used by every
// agentctl command. Built once per command invocation, never cached —
// secretstore.fileStore is cheap to construct and stateless across calls
// (the backing files are the only state).
func defaultSecretStore() keyring.Store {
	return secretstore.NewFileStore(
		secretstore.DefaultDir(config.DefaultDir()),
		secretstore.ReadMachineID,
	)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "pair":
		os.Exit(pairCmd(os.Args[2:]))
	case "status":
		os.Exit(statusCmd(os.Args[2:]))
	case "sync":
		os.Exit(syncCmd(os.Args[2:]))
	case "discover":
		os.Exit(discoverCmd(os.Args[2:]))
	case "sync-all":
		os.Exit(syncAllCmd(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("agentctl " + version.String())
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "agentctl: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

// pairCmd drives the one-shot claim flow. Exits 0 on success, 2 on
// user-fixable errors (bad flags, invalid code), 1 on infrastructure failures
// (network, keyring). Different codes let install-time automation distinguish
// "retry" from "tell the user to fix the code".
func pairCmd(args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	code := fs.String("code", "", "pair code printed by the GST Reco settings UI")
	server := fs.String("server", "https://gstreco.m2ai.ai", "GST Reco server URL")
	configPath := fs.String("config", "", "override config file path (default: platform-standard location)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *code == "" {
		fmt.Fprintln(os.Stderr, "agentctl pair: --code is required")
		fs.Usage()
		return 2
	}

	ctx, cancel := signalContext(60 * time.Second)
	defer cancel()

	path := resolveConfigPath(*configPath)
	resp, err := pair.Claim(ctx, pair.Options{
		Server:       *server,
		Code:         *code,
		ConfigPath:   path,
		Keyring:      defaultSecretStore(),
		AgentVersion: version.Version,
	})
	if err != nil {
		switch {
		case errors.Is(err, pair.ErrInvalidCode):
			fmt.Fprintf(os.Stderr, "agentctl pair: %v\n", err)
			return 2
		case errors.Is(err, pair.ErrAlreadyPaired):
			fmt.Fprintf(os.Stderr, "agentctl pair: %v\nUpgrade the agent build or clear the local config, then retry.\n", err)
			return 2
		case errors.Is(err, pair.ErrCodeGone):
			fmt.Fprintln(os.Stderr, "agentctl pair: this code is expired or already used. Generate a new code in the settings page and try again.")
			return 2
		default:
			fmt.Fprintf(os.Stderr, "agentctl pair: %v\n", err)
			return 1
		}
	}

	fmt.Fprintf(os.Stdout, "✓ paired successfully (connection_id=%s company_id=%s)\n", resp.ConnectionID, resp.CompanyID)
	return 0
}

// statusCmd reports local pairing state. A5 will extend this to query a
// running agent process via the local IPC socket for live sync progress; for
// now it reports what the config file and keyring show, which is enough for a
// user to confirm the pair flow actually persisted.
func statusCmd(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	configPath := fs.String("config", "", "override config file path")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path := resolveConfigPath(*configPath)
	cfg, err := config.Load(path)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			fmt.Fprintln(os.Stdout, "agent not paired (run `agentctl pair --code <CODE>`)")
			return 0
		}
		fmt.Fprintf(os.Stderr, "agentctl status: %v\n", err)
		return 1
	}
	if !cfg.IsPaired() {
		fmt.Fprintln(os.Stdout, "agent config exists but is incomplete — re-run `agentctl pair`")
		return 0
	}
	conns := cfg.PairedConnections()
	if len(conns) == 1 {
		conn := conns[0]
		fmt.Fprintf(os.Stdout, "paired\n  server:        %s\n  connection_id: %s\n  company_id:    %s\n  device_name:   %s\n  paired_at:     %s\n",
			conn.Server, conn.ConnectionID, conn.CompanyID, conn.DeviceName, conn.PairedAt.Format(time.RFC3339))
		return 0
	}

	fmt.Fprintf(os.Stdout, "paired (%d connections)\n", len(conns))
	for i, conn := range conns {
		fmt.Fprintf(
			os.Stdout,
			"  [%d]\n    server:        %s\n    connection_id: %s\n    company_id:    %s\n    device_name:   %s\n    paired_at:     %s\n",
			i+1,
			conn.Server,
			conn.ConnectionID,
			conn.CompanyID,
			conn.DeviceName,
			conn.PairedAt.Format(time.RFC3339),
		)
	}
	return 0
}

// signalContext returns a context that cancels on SIGINT/SIGTERM or after
// the given timeout. Long pair runs are rare but if Tally is unreachable we
// don't want the CLI to hang forever.
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

func usage() {
	fmt.Fprintln(os.Stderr, `agentctl — GST Reco Tally agent operator CLI

Usage:
  agentctl pair --code <CODE> [--server <URL>] [--config <PATH>]
      pair this agent with a GST Reco connection; repeat per tenant

  agentctl status [--config <PATH>]
      print local pairing state (all stored connections)

  agentctl sync --tally-company <NAME> --kind <KIND>
                (--period MMYYYY | --from YYYY-MM-DD --to YYYY-MM-DD)
                [--tally-url <URL>] [--server <URL>] [--config <PATH>]
                [--connection-id <UUID>]
                [--batch-size N] [--dry-run]
      one-shot fetch from a Tally endpoint → parse → ingest a single window.
      KIND is one of: purchase, sales, credit_note, debit_note,
      journal, payment, tax_ledger.

  agentctl discover [--ports START-END] [--host HOST] [--endpoint URL]...
                    [--timeout DURATION] [--concurrency N]
                    [--no-push] [--no-save] [--dry-run]
                    [--server <URL>] [--config <PATH>]
      probe localhost (or --host) for Tally instances, persist the
      discovered endpoints to config, and push the company catalog
      to the server's mapping list. Default port range: 9000-9009.

  agentctl sync-all (--period MMYYYY | --from YYYY-MM-DD --to YYYY-MM-DD)
                    [--kinds purchase,sales,credit_note,debit_note,journal,payment,tax_ledger]
                    [--connection-id <UUID>]
                    [--batch-size N] [--dry-run]
                    [--continue-on-error]
                    [--server <URL>] [--config <PATH>]
      walk every active mapping for every stored connection (or one
      selected connection), run the fetch → parse → ingest pipeline
      per (endpoint, company, kind), report a per-mapping + grand-total
      summary. The pre-A5 daemon driver for multi-port pilots.

  agentctl version
      print version and exit

  agentctl help
      show this help`)
}
