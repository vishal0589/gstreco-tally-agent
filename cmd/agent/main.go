// Command agent is the long-running service process. On Windows it registers
// with the SCM via kardianos/service (wired in A6). In A1 it's a stub that
// logs its version, sleeps until SIGINT, and exits cleanly so we can smoke
// cross-compilation end-to-end.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/vishal0589/gstreco-tally-agent/internal/log"
	"github.com/vishal0589/gstreco-tally-agent/internal/version"
)

func main() {
	var (
		logLevel = flag.String("log-level", "info", "log level: debug|info|warn|error")
		console  = flag.Bool("console", true, "also log to stderr")
		showVer  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVer {
		os.Stdout.WriteString("gstreco-tally-agent " + version.String() + "\n")
		return
	}

	logger := log.New(log.Options{Level: *logLevel, Console: *console})
	logger.Info().Str("version", version.String()).Msg("agent starting")

	ctx, cancel := signalContext()
	defer cancel()

	<-ctx.Done()
	logger.Info().Msg("agent stopped")
}

func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
