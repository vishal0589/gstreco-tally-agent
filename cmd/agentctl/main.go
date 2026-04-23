// Command agentctl is the operator CLI. In the shipped agent it performs the
// one-shot pair flow and prints runtime status over a local IPC socket. A1
// wires the subcommand surface so `agentctl pair --code ABC12-34567 --server
// https://gstreco.m2ai.ai` and `agentctl status` parse cleanly and return the
// right exit codes. Real behaviour is filled in by A3 (pair) and A5 (IPC).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/vishal0589/gstreco-tally-agent/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "pair":
		pairCmd(os.Args[2:])
	case "status":
		statusCmd(os.Args[2:])
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

func pairCmd(args []string) {
	fs := flag.NewFlagSet("pair", flag.ExitOnError)
	code := fs.String("code", "", "pair code printed by the web UI")
	server := fs.String("server", "https://gstreco.m2ai.ai", "GST Reco server URL")
	_ = fs.Parse(args)

	if *code == "" {
		fmt.Fprintln(os.Stderr, "agentctl pair: --code is required")
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "agentctl pair: stub — will POST %s to %s/api/tally/pair/claim in A3\n", *code, *server)
	os.Exit(0)
}

func statusCmd(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	_ = fs.Parse(args)
	fmt.Fprintln(os.Stderr, "agentctl status: stub — will query local IPC socket in A5")
	os.Exit(0)
}

func usage() {
	fmt.Fprintln(os.Stderr, `agentctl — GST Reco Tally agent operator CLI

Usage:
  agentctl pair --code <CODE> [--server <URL>]   pair this agent with a GST Reco connection
  agentctl status                                 print local agent status
  agentctl version                                print version and exit
  agentctl help                                   show this help`)
}
