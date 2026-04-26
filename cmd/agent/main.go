// Command agent is the long-running daemon. Loads the agent config,
// authenticates with the GST Reco server via the keyring-stored HMAC
// secret, schedules sync runs per `cfg.ScheduleCron`, and walks every
// active mapping × kind on each tick. This is the "daemon" piece of
// A5 — the cron + tray + IPC trio in the master plan.
//
// What this version does (B1):
//   - Load config + keyring secret
//   - Build ingest client (one, reused across all ticks)
//   - Schedule syncs via internal/scheduler (cron expression from
//     config; default "0 2 * * *" = 02:00 local daily)
//   - Each tick: fetch active mappings via S15 GET /mappings/active,
//     walk them via syncrun.WalkAll, log per-mapping results
//   - Graceful shutdown on SIGINT/SIGTERM (cron drains in-flight tick)
//
// What's deferred to later B-numbered PRs:
//   - Heartbeat / "sync now" action queue → B3
//   - Tray UI / IPC → A5 follow-up
//   - Self-update notification → B5
//   - Server-driven cron expression (heartbeat sync) → B3
//
// Service vs. console: this binary runs in two modes. Direct
// invocation (`./gstreco-tally-agent`) prints to stderr and exits on
// SIGINT — useful for `make run` during dev. Service-mode integrates
// with kardianos/service in B2.
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

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/log"
	"github.com/vishal0589/gstreco-tally-agent/internal/scheduler"
	"github.com/vishal0589/gstreco-tally-agent/internal/syncrun"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
	"github.com/vishal0589/gstreco-tally-agent/internal/version"
)

// daemonOptions hold the parsed CLI flags. Surfaced as a struct so
// integration tests can construct one directly without re-parsing
// argv.
type daemonOptions struct {
	logLevel         string
	console          bool
	configPath       string
	scheduleOverride string
	runOnceAndExit   bool
	showVersion      bool
}

func main() {
	// Service control subcommand. Lives before flag.Parse() because
	// "service" is a positional argument, not a flag — flag.Parse
	// would otherwise reject it and exit 2.
	if len(os.Args) > 1 && os.Args[1] == "service" {
		os.Exit(serviceControlMain(os.Args[2:]))
	}

	opts := parseFlags()

	if opts.showVersion {
		fmt.Fprintln(os.Stdout, "gstreco-tally-agent "+version.String())
		return
	}

	logger := log.New(log.Options{Level: opts.logLevel, Console: opts.console})
	logger.Info().Str("version", version.String()).Msg("agent starting")

	// kardianos/service auto-detects whether the process was started
	// by an SCM/launchd/systemd or interactively. In service mode it
	// drives Start/Stop on our daemonProgram; in console mode it
	// effectively passes through to runDaemonViaService → which calls
	// Start (kicks off the daemon goroutine), waits for SIGINT, and
	// then calls Stop.
	//
	// run-once is a separate path that doesn't go through the service
	// machinery at all — it's just a one-shot tick + exit, useful for
	// smoke testing without registering the service.
	if opts.runOnceAndExit {
		os.Exit(run(opts, logger))
	}

	os.Exit(runDaemonViaService(opts, logger))
}

func parseFlags() daemonOptions {
	o := daemonOptions{}
	flag.StringVar(&o.logLevel, "log-level", "info", "log level: debug|info|warn|error")
	flag.BoolVar(&o.console, "console", true, "also log to stderr")
	flag.StringVar(&o.configPath, "config", "", "override config file path")
	flag.StringVar(&o.scheduleOverride, "schedule", "", "override cron expression from config (e.g. \"0 */6 * * *\")")
	flag.BoolVar(&o.runOnceAndExit, "run-once", false, "fire one tick immediately, then exit (for smoke testing)")
	flag.BoolVar(&o.showVersion, "version", false, "print version and exit")
	flag.Parse()
	return o
}

// run is main() minus os.Exit so tests can drive it without exiting
// the test binary. Returns the process exit code.
//
// run uses signalContext for graceful shutdown (SIGINT/SIGTERM). The
// service-driven path uses runWithCtx instead — Stop() cancels a
// service-level ctx and the daemon drains via that.
func run(opts daemonOptions, logger zerolog.Logger) int {
	ctx, cancel := signalContext()
	defer cancel()
	return runWithCtx(ctx, opts, logger)
}

// runWithCtx is run() with an injected ctx so the service-driven path
// can wire kardianos's Stop into ctx cancellation without relying on
// signal handlers (Windows SCM doesn't deliver SIGINT). Tests use
// runWithCtx directly so they don't have to send real signals.
func runWithCtx(ctx context.Context, opts daemonOptions, logger zerolog.Logger) int {
	cfgPath := opts.configPath
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			logger.Error().Str("path", cfgPath).Msg("agent not paired (run `agentctl pair --code <CODE>` first)")
			return 2
		}
		logger.Error().Err(err).Msg("load config failed")
		return 1
	}
	if !cfg.IsPaired() {
		logger.Error().Msg("config exists but is incomplete — re-run `agentctl pair`")
		return 2
	}

	// Resolve ingest client once. Keyring read at startup, not on
	// every tick — keeps tick latency low and the keyring API
	// invocation count bounded.
	ks := keyring.NewOSKeyring()
	hmacKey, bearerKey := keyring.ConnectionKeys(cfg.ConnectionID)
	secret, err := ks.Get(keyring.ServiceName, hmacKey)
	if err != nil {
		logger.Error().Err(err).Msg("read hmac secret from keyring failed")
		return 1
	}
	bearer, err := ks.Get(keyring.ServiceName, bearerKey)
	if err != nil {
		logger.Error().Err(err).Msg("read bearer token from keyring failed")
		return 1
	}
	client, err := ingest.NewClient(cfg.Server, cfg.ConnectionID, bearer, secret)
	if err != nil {
		logger.Error().Err(err).Msg("build ingest client failed")
		return 1
	}

	scheduleSpec := opts.scheduleOverride
	if scheduleSpec == "" {
		scheduleSpec = cfg.ScheduleCron
	}

	tickFn := makeTickFunc(client, tally.NewClient, logger)

	if opts.runOnceAndExit {
		// Smoke path — fire once and report. The service path uses
		// the cron loop instead.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		logger.Info().Msg("run-once mode: firing immediate tick")
		if err := tickFn(ctx); err != nil {
			logger.Error().Err(err).Msg("tick failed")
			return 1
		}
		return 0
	}

	sched, err := scheduler.New(scheduler.Options{
		Spec:            scheduleSpec,
		TickFunc:        tickFn,
		PerTickDeadline: 30 * time.Minute,
		Logger:          logger,
		Location:        time.Local,
	})
	if err != nil {
		logger.Error().Err(err).Msg("scheduler init failed")
		return 2
	}
	sched.Start()
	defer sched.Stop()

	logger.Info().
		Str("server", cfg.Server).
		Str("connection_id", cfg.ConnectionID).
		Str("schedule", sched.Spec()).
		Msg("daemon ready")

	<-ctx.Done()
	logger.Info().Msg("daemon stopping (ctx done)")
	return 0
}

// makeTickFunc returns the closure the scheduler fires on each tick.
// One factory call at startup, one closure forever — the server URL,
// connection id, and ingest client are bound once.
func makeTickFunc(
	client *ingest.Client,
	newTallyClient func(string, ...tally.ClientOption) *tally.Client,
	logger zerolog.Logger,
) scheduler.TickFunc {
	return func(ctx context.Context) error {
		// Each tick runs against the current calendar month. CAs
		// reconcile monthly; daily cron means each tick re-syncs the
		// month-to-date state. Idempotent on the server side
		// (ingest's ON CONFLICT DO UPDATE per migration 00033).
		now := time.Now().In(time.Local)
		from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		to := from.AddDate(0, 1, -1)

		mappings, err := client.FetchActiveMappings(ctx)
		if err != nil {
			return fmt.Errorf("fetch active mappings: %w", err)
		}
		if len(mappings.Mappings) == 0 {
			logger.Warn().Msg("no active mappings — run `agentctl discover` then map companies in /settings/tally")
			return nil
		}
		logger.Info().
			Int("mappings", len(mappings.Mappings)).
			Time("from", from).Time("to", to).
			Msg("walking mappings")

		runIDPrefix := fmt.Sprintf("daemon-%d", time.Now().Unix())

		res := syncrun.WalkAll(ctx, syncrun.WalkOptions{
			Mappings:        mappings.Mappings,
			Kinds:           syncrun.DefaultKinds,
			From:            from,
			To:              to,
			RunIDPrefix:     runIDPrefix,
			RunKind:         "incremental",
			ContinueOnError: true,
			NewTallyClient: func(endpoint string) syncrun.TallyPoster {
				return newTallyClient(endpoint)
			},
			Sender: client,
			PerKindResultHook: func(m ingest.ActiveMapping, k syncrun.KindPair, r syncrun.Result, fatalErr error) {
				if fatalErr != nil {
					logger.Error().
						Str("mapping", m.TallyCompanyName).
						Str("endpoint", m.TallyEndpoint).
						Str("kind", k.Name).
						Err(fatalErr).
						Msg("per-kind fatal")
					return
				}
				logger.Info().
					Str("mapping", m.TallyCompanyName).
					Str("endpoint", m.TallyEndpoint).
					Str("kind", k.Name).
					Int("rows", r.RowCount).
					Int("sent", r.BatchesSent).
					Int("failed", r.BatchesFailed).
					Msg("per-kind result")
			},
		})

		logger.Info().
			Int("mappings_run", res.MappingsRun).
			Int("rows", res.TotalRows).
			Int("batches_sent", res.TotalBatchesSent).
			Int("batches_failed", res.TotalBatchesFailed).
			Int("fatal_errors", res.FatalErrors).
			Msg("tick complete")

		// Tick is "successful" even with non-zero failures — the
		// scheduler keeps firing. The operator triages via the
		// sync-status dashboard (B4) once it ships. A non-nil return
		// here would just produce a duplicate scheduler-level log.
		return nil
	}
}

// signalContext returns a context that cancels on SIGINT/SIGTERM.
// Daemon uses this to drive graceful shutdown — the scheduler's Stop
// drains any in-flight tick before this returns.
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
