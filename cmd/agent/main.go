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
//     config; default "0 11,13,15,17 * * *" = 11am/1pm/3pm/5pm
//     local daily, with 1h+2h retry-with-backoff per tick on
//     failure — see internal/scheduler.DefaultRetryDelays)
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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/autodiscover"
	"github.com/vishal0589/gstreco-tally-agent/internal/autoupdate"
	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/heartbeat"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/log"
	"github.com/vishal0589/gstreco-tally-agent/internal/period"
	"github.com/vishal0589/gstreco-tally-agent/internal/scheduler"
	"github.com/vishal0589/gstreco-tally-agent/internal/secretstore"
	"github.com/vishal0589/gstreco-tally-agent/internal/selfupdate"
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

type daemonConnectionRuntime struct {
	info     config.PairedConnection
	client   *ingest.Client
	disabled atomic.Bool
	// status is this connection's own last result, reported as
	// connection_status on this connection's heartbeat only.
	status *connStatusTracker
	// identityEnforce is config company_identity_enforce: the company
	// GUID probe gates this connection's sync (QA 04-Nov N1). Off =
	// observe only.
	identityEnforce bool
}

func newDaemonConnectionRuntime(info config.PairedConnection, client *ingest.Client) *daemonConnectionRuntime {
	return &daemonConnectionRuntime{info: info, client: client, status: newConnStatusTracker()}
}

func (r *daemonConnectionRuntime) Enabled() bool {
	return r != nil && !r.disabled.Load()
}

func (r *daemonConnectionRuntime) Disable() {
	if r != nil {
		r.disabled.Store(true)
	}
}

type multiCatalogSender struct {
	runtimes []*daemonConnectionRuntime
}

func (m multiCatalogSender) SendCatalog(ctx context.Context, body ingest.CatalogRequest) error {
	var firstErr error
	for _, runtime := range m.runtimes {
		if runtime == nil || !runtime.Enabled() {
			continue
		}
		if err := runtime.client.SendCatalog(ctx, body); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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

	// Resolve one ingest client per paired connection. Secret store
	// reads happen once at startup, not on every tick — keeps tick
	// latency low and the file-system read count bounded.
	ks := secretstore.NewFileStore(
		secretstore.DefaultDir(filepath.Dir(cfgPath)),
		secretstore.ReadMachineID,
	)
	runtimes := loadConnectionRuntimes(cfg.PairedConnections(), ks, logger)
	if len(runtimes) == 0 {
		logger.Error().Msg("no usable paired connections could be initialized")
		return 1
	}
	for _, rt := range runtimes {
		rt.identityEnforce = cfg.CompanyIdentityEnforce
	}
	logger.Info().Bool("company_identity_enforce", cfg.CompanyIdentityEnforce).
		Msg("company identity gate: " + identityModeName(cfg.CompanyIdentityEnforce))

	// Boot-time auto-discovery. Runs synchronously before the
	// scheduler starts so the first scheduled tick sees mappings.
	// Cheap when there's nothing new (probe sweep is bounded by
	// 5s × concurrency); always-on so a customer who later adds a
	// new Tally instance gets it picked up at the next service
	// restart even if the periodic Loop has been napping.
	//
	// Best-effort: failure here doesn't stop the daemon. The
	// scheduler still starts and FetchActiveMappings will return
	// whatever mappings the server already has. Operator sees the
	// non-fatal warning in Event Viewer / journalctl.
	bootCtx, bootCancel := context.WithTimeout(ctx, 90*time.Second)
	bootRes, bootErr := autodiscover.Run(bootCtx, autodiscover.Options{
		Cfg:     cfg,
		CfgPath: cfgPath,
		Sender:  multiCatalogSender{runtimes: runtimes},
		Logger:  logger,
	})
	bootCancel()
	if bootErr != nil {
		logger.Warn().Err(bootErr).Msg("boot autodiscover errored (non-fatal)")
	} else if bootRes.SkippedReason != "" {
		logger.Info().Str("reason", bootRes.SkippedReason).Msg("boot autodiscover skipped")
	} else {
		logger.Info().
			Strs("endpoints", bootRes.Endpoints).
			Int("companies_pushed", bootRes.CompaniesPushed).
			Bool("catalog_pushed", bootRes.CatalogPushed).
			Bool("config_saved", bootRes.ConfigSaved).
			Msg("boot autodiscover complete")
	}

	scheduleSpec := opts.scheduleOverride
	if scheduleSpec == "" {
		scheduleSpec = cfg.ScheduleCron
	}
	scheduleFrom := scheduleSource(opts.scheduleOverride, cfg.ScheduleCron)

	tickFn := makeTickFunc(runtimes, tally.NewClient, logger)

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

	// Wrap parent ctx with a child we own so the heartbeat handler
	// can cancel the whole daemon on a "revoke" action without
	// reaching into the caller's signal/service context.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// Tick state is shared between the scheduler-driven sync loop
	// (writes) and the heartbeat poller's LastTickReporter (reads).
	// Atomic-style updates via mutex; the body is small enough that
	// there's no measurable contention.
	tickState := newTickState()
	tickFnTracked := wrapTickWithState(tickFn, tickState)

	sched, err := scheduler.New(scheduler.Options{
		Spec:            scheduleSpec,
		TickFunc:        tickFnTracked,
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
	// QA 04-Nov N3: every heartbeat reports the spec the scheduler
	// actually runs (sched.Spec(), DefaultSpec already applied) and its
	// source, so the server stops showing its own schedule_cron as if
	// it were in use.
	effectiveSchedule := &ingest.ScheduleReport{Cron: sched.Spec(), Source: scheduleFrom}
	logger.Info().Str("cron", effectiveSchedule.Cron).Str("source", effectiveSchedule.Source).Msg("effective sync schedule")

	// Periodic re-discovery in a background goroutine. Picks up new
	// Tally instances added after the daemon started — common when
	// a customer opens a second company file on a fresh port. The
	// loop does NOT fire immediately; we already ran one synchronous
	// sweep above before scheduler.Start.
	go autodiscover.Loop(runCtx, autodiscover.DefaultPeriodicInterval, autodiscover.Options{
		Cfg:     cfg,
		CfgPath: cfgPath,
		Sender:  multiCatalogSender{runtimes: runtimes},
		Logger:  logger,
	})

	pollers := make([]*heartbeat.Poller, 0, len(runtimes))
	for _, runtime := range runtimes {
		runtime := runtime
		periodSyncFnTracked := wrapSyncNowWithState(
			makePeriodSyncFunc(runtime, tally.NewClient, logger),
			tickState,
		)
		currentMonthSyncFnTracked := wrapSyncNowWithState(
			makeSingleConnectionSyncNowFunc(runtime, tally.NewClient, logger),
			tickState,
		)
		hbHandler := &daemonHeartbeatHandler{
			runtime:         runtime,
			logger:          logger,
			runCurrentSync:  currentMonthSyncFnTracked,
			runPeriodSync:   periodSyncFnTracked,
			cancelIfLastOne: func() { cancelRunIfAllConnectionsDisabled(runtimes, cancelRun) },
		}
		poller, pollerErr := heartbeat.New(heartbeat.Options{
			Client:           runtime.client,
			Handler:          hbHandler,
			Logger:           logger,
			AgentVersion:     version.Version,
			LastTickReporter: tickState.report,
			// Per-connection: this connection's own result only.
			ConnectionStatusReporter: runtime.status.snapshot,
			OnHeartbeatResult:        runtime.status.recordHeartbeat,
			Schedule:                 effectiveSchedule,
		})
		if pollerErr != nil {
			logger.Error().
				Str("connection_id", runtime.info.ConnectionID).
				Err(pollerErr).
				Msg("heartbeat poller init failed")
			return 1
		}
		poller.Start(runCtx)
		pollers = append(pollers, poller)
	}
	defer func() {
		for _, poller := range pollers {
			poller.Stop()
		}
	}()

	// Self-update (B5). Polls /api/tally/agent/version every 6 hours.
	// When the server allows it (auto_update.enabled), a newer release
	// installs itself through the installer's silent update
	// (internal/autoupdate); otherwise the warning tells the operator.
	installDir := ""
	if exe, exeErr := os.Executable(); exeErr == nil {
		installDir = filepath.Dir(exe)
	}
	updater := autoupdate.New(autoupdate.Options{
		StateDir:   filepath.Dir(cfgPath),
		Server:     runtimes[0].info.Server,
		ConfigPath: cfgPath,
		InstallDir: installDir,
		GOOS:       runtime.GOOS,
		Busy:       func() bool { return sched.RunningCount() > 0 },
		Logger:     logger,
	})
	updateChecker, err := selfupdate.New(selfupdate.Options{
		HTTPClient:     http.DefaultClient,
		VersionURL:     strings.TrimRight(runtimes[0].info.Server, "/") + "/api/tally/agent/version",
		CurrentVersion: version.Version,
		Handler: func(n selfupdate.Notification) {
			req := autoupdate.Request{
				CurrentVersion: n.CurrentVersion,
				LatestVersion:  n.LatestVersion,
				Enabled:        n.AutoUpdate,
			}
			if n.Installer != nil {
				req.Installer = &autoupdate.Installer{URL: n.Installer.URL, SHA256: n.Installer.SHA256, Version: n.Installer.Version}
			}
			outcome, updateErr := updater.Consider(runCtx, req)
			event := logger.Info()
			if updateErr != nil {
				event = logger.Warn().Err(updateErr)
			}
			event.
				Str("current", n.CurrentVersion).
				Str("latest", n.LatestVersion).
				Str("released_at", n.ReleasedAt).
				Str("auto_update", string(outcome)).
				Msg(autoUpdateMessage(outcome, updateErr))
		},
		Logger: logger,
	})
	if err != nil {
		// Self-update is non-essential — log + skip. The daemon
		// keeps running without it.
		logger.Warn().Err(err).Msg("self-update checker init failed; daemon continues without update notifications")
	} else {
		updateChecker.Start(runCtx)
		defer updateChecker.Stop()
	}

	logger.Info().
		Int("connections", len(runtimes)).
		Str("schedule", sched.Spec()).
		Msg("daemon ready")

	<-runCtx.Done()
	logger.Info().Msg("daemon stopping (ctx done)")
	return 0
}

// secretGetter is the read side of the secret store.
type secretGetter interface {
	Get(service, key string) (string, error)
}

// loadConnectionRuntimes builds one runtime (ingest client + status) per
// paired connection, each from that connection's OWN keyring entries
// (keyring.ConnectionKeys(connection_id)). Logs key names, never values.
func loadConnectionRuntimes(conns []config.PairedConnection, ks secretGetter, logger zerolog.Logger, opts ...ingest.Option) []*daemonConnectionRuntime {
	runtimes := make([]*daemonConnectionRuntime, 0, len(conns))
	for _, conn := range conns {
		hmacKey, bearerKey := keyring.ConnectionKeys(conn.ConnectionID)
		secret, secretErr := ks.Get(keyring.ServiceName, hmacKey)
		if secretErr != nil {
			logger.Error().
				Str("connection_id", conn.ConnectionID).
				Err(secretErr).
				Msg("read hmac secret from keyring failed; skipping connection")
			continue
		}
		bearer, bearerErr := ks.Get(keyring.ServiceName, bearerKey)
		if bearerErr != nil {
			logger.Error().
				Str("connection_id", conn.ConnectionID).
				Err(bearerErr).
				Msg("read bearer token from keyring failed; skipping connection")
			continue
		}
		client, clientErr := ingest.NewClient(conn.Server, conn.ConnectionID, bearer, secret, opts...)
		if clientErr != nil {
			logger.Error().
				Str("connection_id", conn.ConnectionID).
				Err(clientErr).
				Msg("build ingest client failed; skipping connection")
			continue
		}
		// Key names (never values) so a mixed-up pairing is visible
		// in the log: each connection must read its own entries.
		logger.Info().
			Str("connection_id", conn.ConnectionID).
			Str("hmac_key", hmacKey).
			Str("bearer_key", bearerKey).
			Msg("connection credentials loaded")
		runtimes = append(runtimes, newDaemonConnectionRuntime(conn, client))
	}
	return runtimes
}

// daemonHeartbeatHandler implements heartbeat.Handler by routing each
// action to the corresponding daemon primitive.
type daemonHeartbeatHandler struct {
	runtime         *daemonConnectionRuntime
	logger          zerolog.Logger
	runCurrentSync  func(ctx context.Context, req heartbeat.SyncNow) error
	runPeriodSync   func(ctx context.Context, req heartbeat.SyncNow) error
	cancelIfLastOne func()
}

// OnSyncNow runs the sync a delivered sync_now asks for. The request id
// travels by value into that one run (QA v29 P3): the poller dispatches
// sync_now actions one at a time, so a second sync_now starts its own run
// with its own id after this one returns and never re-stamps it.
func (h *daemonHeartbeatHandler) OnSyncNow(ctx context.Context, req heartbeat.SyncNow) error {
	if h.runtime == nil || !h.runtime.Enabled() {
		return fmt.Errorf("heartbeat sync_now: connection is disabled")
	}
	req.Period = strings.TrimSpace(req.Period)
	if req.Period == "" {
		h.logger.Info().
			Str("connection_id", h.runtime.info.ConnectionID).
			Str("sync_request_id", req.RequestID).
			Strs("kinds", req.Kinds).
			Msg("heartbeat sync_now: firing current-month sync for this connection")
		if h.runCurrentSync == nil {
			return fmt.Errorf("heartbeat sync_now: current-month sync is unavailable")
		}
		return h.runCurrentSync(ctx, req)
	}
	h.logger.Info().
		Str("connection_id", h.runtime.info.ConnectionID).
		Str("period", req.Period).
		Str("sync_request_id", req.RequestID).
		Strs("kinds", req.Kinds).
		Msg("heartbeat sync_now: running explicit month window")
	if h.runPeriodSync == nil {
		return fmt.Errorf("heartbeat sync_now: explicit month sync is unavailable")
	}
	return h.runPeriodSync(ctx, req)
}

func (h *daemonHeartbeatHandler) OnPause(_ context.Context) error {
	// Server-side `tally_connections.status` flip already rejects
	// future ingest at HTTP 403; agent keeps polling heartbeat to
	// learn when status flips back (resume = OnRevoke's inverse).
	// V1 logs the action; explicit local pause lands when the
	// pilot reveals operators expect the scheduler to actually
	// stop ticking.
	h.logger.Warn().Msg("heartbeat pause: server-side status flip in effect; agent keeps heartbeating")
	return nil
}

func (h *daemonHeartbeatHandler) OnRevoke(_ context.Context) error {
	if h.runtime != nil {
		h.runtime.Disable()
		h.logger.Warn().
			Str("connection_id", h.runtime.info.ConnectionID).
			Msg("heartbeat revoke: connection disabled locally")
	}
	if h.cancelIfLastOne != nil {
		h.cancelIfLastOne()
	}
	return nil
}

func (h *daemonHeartbeatHandler) OnRefetchMappings(_ context.Context) error {
	// No-op for V1: each scheduler tick fetches a fresh mapping list
	// via /mappings/active anyway. The action stays in the protocol
	// for future use (e.g. when we add a local mapping cache for
	// faster ticks).
	h.logger.Info().Msg("heartbeat refetch_mappings: noop (next tick fetches fresh)")
	return nil
}

func (h *daemonHeartbeatHandler) OnScheduleChanged(_ context.Context, newCron string) error {
	h.logger.Warn().
		Str("new_cron", newCron).
		Msg("heartbeat schedule_changed: server cron differs from local; restart daemon to pick up (V1)")
	return nil
}

// tickState shares last-successful-tick state between the scheduler
// (writer) and the heartbeat poller (reader).
type tickState struct {
	mu       sync.Mutex
	lastAt   time.Time
	lastStat string
}

func newTickState() *tickState { return &tickState{} }

func (s *tickState) record(at time.Time, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAt = at
	s.lastStat = status
}

func (s *tickState) report() (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAt, s.lastStat
}

// wrapTickWithState wraps the bare tick function with state-recording
// hooks so heartbeat reports preserve local sync failure detail.
func wrapTickWithState(fn scheduler.TickFunc, state *tickState) scheduler.TickFunc {
	return func(ctx context.Context) error {
		err := fn(ctx)
		state.record(time.Now().UTC(), tickStatusForError(err))
		return err
	}
}

func wrapSyncNowWithState(
	fn func(ctx context.Context, req heartbeat.SyncNow) error,
	state *tickState,
) func(ctx context.Context, req heartbeat.SyncNow) error {
	return func(ctx context.Context, req heartbeat.SyncNow) error {
		err := fn(ctx, req)
		state.record(time.Now().UTC(), tickStatusForError(err))
		return err
	}
}

func tickStatusForError(err error) string {
	if err == nil {
		return "ok"
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		return "error"
	}
	status := "error: " + msg
	const maxStatusLen = 500
	if len(status) > maxStatusLen {
		return status[:maxStatusLen]
	}
	return status
}

// scheduleSource names where the scheduler's spec came from, following
// the selection in runWithCtx exactly: the --schedule override when set,
// else config.yaml schedule_cron when set, else scheduler.DefaultSpec
// (scheduler.New applies it for an empty spec). Reporting only.
func scheduleSource(override, configCron string) string {
	switch {
	case override != "":
		return ingest.ScheduleSourceOverride
	case configCron != "":
		return ingest.ScheduleSourceConfig
	default:
		return ingest.ScheduleSourceDefault
	}
}

// makeTickFunc returns the closure the scheduler fires on each tick.
// One factory call at startup, one closure forever — it walks every enabled
// paired connection.
func makeTickFunc(
	runtimes []*daemonConnectionRuntime,
	newTallyClient func(string, ...tally.ClientOption) *tally.Client,
	logger zerolog.Logger,
) scheduler.TickFunc {
	return func(ctx context.Context) error {
		from, to := currentMonthWindow(time.Now().In(time.Local))
		return runWindowSyncAllConnections(ctx, runtimes, newTallyClient, logger, from, to, "incremental")
	}
}

// syncScope is what one sync run serves: the server request id (empty
// for scheduled ticks) and the voucher kinds to walk.
type syncScope struct {
	requestID string
	kinds     []syncrun.KindPair
}

// scheduledScope is a scheduled tick: no request, every kind.
var scheduledScope = syncScope{kinds: syncrun.DefaultKinds}

// syncScopeFor turns a delivered sync_now into the run's scope. Kinds the
// agent does not know are logged and skipped; when none resolve (or the
// server sent none, as older servers do) every kind runs, as before.
func syncScopeFor(req heartbeat.SyncNow, logger zerolog.Logger) syncScope {
	scope := syncScope{requestID: strings.TrimSpace(req.RequestID), kinds: syncrun.DefaultKinds}
	if len(req.Kinds) == 0 {
		return scope
	}
	asked := make(map[string]bool, len(req.Kinds))
	for _, name := range req.Kinds {
		kp, err := syncrun.MapKindByName(name)
		if err != nil {
			logger.Warn().Str("kind", name).Str("sync_request_id", scope.requestID).
				Msg("sync_now asked for a kind this agent does not know; skipping it")
			continue
		}
		asked[kp.Name] = true
	}
	if len(asked) == 0 {
		return scope
	}
	kinds := make([]syncrun.KindPair, 0, len(asked))
	for _, kp := range syncrun.DefaultKinds {
		if asked[kp.Name] {
			kinds = append(kinds, kp)
		}
	}
	scope.kinds = kinds
	return scope
}

func makePeriodSyncFunc(
	runtime *daemonConnectionRuntime,
	newTallyClient func(string, ...tally.ClientOption) *tally.Client,
	logger zerolog.Logger,
) func(ctx context.Context, req heartbeat.SyncNow) error {
	return func(ctx context.Context, req heartbeat.SyncNow) error {
		periodCode := req.Period
		from, to, err := period.ParseMonthCode(periodCode)
		if err != nil {
			return fmt.Errorf("sync_now period %q: %w", periodCode, err)
		}
		if runtime == nil || !runtime.Enabled() {
			return fmt.Errorf("sync_now period %q: connection is disabled", periodCode)
		}
		return runWindowSync(ctx, runtime, newTallyClient, logger, from, to, "manual", syncScopeFor(req, logger))
	}
}

func makeSingleConnectionSyncNowFunc(
	runtime *daemonConnectionRuntime,
	newTallyClient func(string, ...tally.ClientOption) *tally.Client,
	logger zerolog.Logger,
) func(ctx context.Context, req heartbeat.SyncNow) error {
	return func(ctx context.Context, req heartbeat.SyncNow) error {
		if runtime == nil || !runtime.Enabled() {
			return fmt.Errorf("current-month sync: connection is disabled")
		}
		from, to := currentMonthWindow(time.Now().In(time.Local))
		return runWindowSync(ctx, runtime, newTallyClient, logger, from, to, "incremental", syncScopeFor(req, logger))
	}
}

func runWindowSyncAllConnections(
	ctx context.Context,
	runtimes []*daemonConnectionRuntime,
	newTallyClient func(string, ...tally.ClientOption) *tally.Client,
	logger zerolog.Logger,
	from time.Time,
	to time.Time,
	runKind string,
) error {
	activeCount := 0
	var firstErr error
	for _, runtime := range runtimes {
		if runtime == nil || !runtime.Enabled() {
			continue
		}
		activeCount++
		// Each connection records its own result inside runWindowSync;
		// a failure here never becomes another connection's status.
		if err := runWindowSync(ctx, runtime, newTallyClient, logger, from, to, runKind, scheduledScope); err != nil {
			logger.Error().
				Str("connection_id", runtime.info.ConnectionID).
				Err(err).
				Msg("connection window sync failed")
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if activeCount == 0 {
		logger.Warn().Msg("no enabled paired connections remain")
		return nil
	}
	return firstErr
}

func cancelRunIfAllConnectionsDisabled(
	runtimes []*daemonConnectionRuntime,
	cancelRun context.CancelFunc,
) {
	for _, runtime := range runtimes {
		if runtime != nil && runtime.Enabled() {
			return
		}
	}
	cancelRun()
}

func currentMonthWindow(now time.Time) (time.Time, time.Time) {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return from, from.AddDate(0, 1, -1)
}

// runWindowSync syncs one connection and records that connection's own
// result in runtime.status (connection_status on its heartbeat only).
func runWindowSync(
	ctx context.Context,
	runtime *daemonConnectionRuntime,
	newTallyClient func(string, ...tally.ClientOption) *tally.Client,
	logger zerolog.Logger,
	from time.Time,
	to time.Time,
	runKind string,
	scope syncScope,
) error {
	if runtime == nil || runtime.client == nil {
		return fmt.Errorf("window sync: connection is not initialised")
	}
	client := runtime.client
	logger = logger.With().Str("connection_id", runtime.info.ConnectionID).Logger()
	var outcome syncOutcome
	defer func() {
		status := outcome.status()
		// QA v29 P3 x L19: the connection's status names the request it served.
		if scope.requestID != "" {
			status.SyncRequestID = ingest.OptString(scope.requestID)
		}
		runtime.status.recordSync(status)
	}()

	mappings, err := client.FetchActiveMappings(ctx)
	if err != nil {
		if !outcome.noteAuth(err, "fetch active mappings") {
			outcome.fetchErr = err
		}
		return fmt.Errorf("fetch active mappings: %w", err)
	}
	outcome.mappings = mappings.Mappings
	if len(mappings.Mappings) == 0 {
		outcome.noMapping = true
		logger.Warn().Msg("no active mappings — open /settings/tally and map the auto-discovered companies to GSTINs")
		return nil
	}
	logger.Info().
		Int("mappings", len(mappings.Mappings)).
		Time("from", from).
		Time("to", to).
		Str("run_kind", runKind).
		Str("sync_request_id", scope.requestID).
		Msg("walking mappings")

	// Sync vendor + customer masters BEFORE walking vouchers so
	// the server's ingest path back-fills vendor_id / customer_id
	// on each invoice as it lands. Best-effort: per-(mapping,
	// kind) failures log warn and the voucher walk still runs.
	// Reusing the same Tally client per endpoint keeps connection
	// pooling sensible (Tally HTTP is single-process anyway).
	//
	// Before any master or voucher fetch, each mapped company (name +
	// GUID) must be the one Tally has open on the endpoint we would read.
	// A mismatch is skipped and reported on THIS connection only.
	active, skipped, identity, unreadable := resolveMappingsForTick(ctx, mappings.Mappings, runtime.identityEnforce, logger)
	outcome.active, outcome.skipped = active, skipped
	outcome.identity, outcome.identityEnforce = identity, runtime.identityEnforce
	if len(active) == 0 {
		return unreadable
	}

	if authErr := syncMastersForMappings(ctx, client, newTallyClient, active, logger); authErr != nil {
		outcome.noteAuth(authErr, "masters push")
	}

	runIDPrefix := fmt.Sprintf("%s-%d", runKind, time.Now().Unix())

	opts := walkOptionsFor(active, from, to, runIDPrefix, runKind, scope, client, newTallyClient, logger)
	logKindResult := opts.PerKindResultHook
	opts.PerKindResultHook = func(m ingest.ActiveMapping, k syncrun.KindPair, r syncrun.Result, fatalErr error) {
		// A 401/403 on this connection's ingest is this connection's auth result.
		if authErr := walkAuthError(r, fatalErr); authErr != nil {
			outcome.noteAuth(authErr, "ingest "+k.Name)
		}
		logKindResult(m, k, r, fatalErr)
	}
	res := syncrun.WalkAll(ctx, opts)

	logger.Info().
		Int("mappings_run", res.MappingsRun).
		Int("rows", res.TotalRows).
		Int("batches_sent", res.TotalBatchesSent).
		Int("batches_failed", res.TotalBatchesFailed).
		Int("server_skipped", res.TotalServerSkipped).
		Int("server_errors", res.TotalServerErrors).
		Int("fatal_errors", res.FatalErrors).
		Str("run_kind", runKind).
		Str("sync_request_id", scope.requestID).
		Msg("tick complete")

	walkErr := errorFromWalkResult(res)
	outcome.otherErr = walkErr
	// Both reach the heartbeat: a walk failure must not hide which
	// companies could not be read at all.
	return errors.Join(walkErr, unreadable)
}

// walkOptionsFor builds the walk for one window sync. The scope's request
// id goes on WalkOptions, so every (mapping, kind) run and each of its
// batches carries it; a scheduled tick's scope has none.
func walkOptionsFor(
	active []ingest.ActiveMapping,
	from, to time.Time,
	runIDPrefix, runKind string,
	scope syncScope,
	sender syncrun.IngestSender,
	newTallyClient func(string, ...tally.ClientOption) *tally.Client,
	logger zerolog.Logger,
) syncrun.WalkOptions {
	kinds := scope.kinds
	if len(kinds) == 0 {
		kinds = syncrun.DefaultKinds
	}
	return syncrun.WalkOptions{
		Mappings:        active,
		Kinds:           kinds,
		From:            from,
		To:              to,
		RunIDPrefix:     runIDPrefix,
		RunKind:         runKind,
		SyncRequestID:   scope.requestID,
		ContinueOnError: true,
		NewTallyClient: func(endpoint string) syncrun.TallyPoster {
			return newTallyClient(endpoint)
		},
		Sender: sender,
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
				Int("landed", r.ServerInserted+r.ServerAmended).
				Int("skipped", r.ServerSkipped).
				Int("server_errors", r.ServerErrors).
				Str("sync_request_id", scope.requestID).
				Msg("per-kind result")
		},
	}
}

// followProbeTimeout bounds each port probe before a tick. A Tally that
// can't list its companies in this time is busy or hung; it is skipped this
// tick instead of holding every voucher kind for minutes.
const followProbeTimeout = 8 * time.Second

// identityModeName names the company identity gate mode for logs and
// connection_status detail.
func identityModeName(enforce bool) string {
	if enforce {
		return "enforce"
	}
	return "observe"
}

// resolveMappingsForTick reads each mapped company from the Tally port it is
// open on right now (syncrun.ResolveMappingEndpoints): it probes the mapped
// ports plus every port a tally.exe is listening on. Returns the mappings to
// read, the ones skipped (each with a connection_status result), the
// company identity check per mapping, and, when some company can't be
// read, an error naming each one. If the probe itself can't run nothing is
// read: vouchers are never fetched for a company the agent could not
// confirm is the one open (QA v29 L19).
//
// Every port gets both company probes (List of Companies and the
// Type:Company NAME+GUID collection, QA 04-Nov N1). With enforce off the
// gate is List of Companies exactly as before and the GUID probe is only
// observed; with enforce on it is syncrun.ResolveMappingEndpointsEnforced.
func resolveMappingsForTick(ctx context.Context, mappings []ingest.ActiveMapping, enforce bool, logger zerolog.Logger) ([]ingest.ActiveMapping, []syncrun.SkippedMapping, []syncrun.MappingIdentity, error) {
	ports, err := tally.TallyListeningPorts()
	if err != nil {
		logger.Warn().Err(err).Msg("could not list Tally ports; probing mapped ports only")
	}
	var probes []tally.ProbeResult
	// No candidate endpoint means every mapping lacks one; Discover would
	// then fall back to a port-range sweep, which is not what we want.
	if candidates := syncrun.CandidateEndpoints(mappings, ports); len(candidates) > 0 {
		probes, err = tally.Discover(ctx, tally.DiscoverOptions{
			Endpoints:     candidates,
			Timeout:       followProbeTimeout,
			Concurrency:   8,
			IdentityProbe: true,
		})
		if err != nil {
			logger.Warn().Err(err).Msg("Tally port probe failed; not reading any mapped company this tick")
			skipped := make([]syncrun.SkippedMapping, 0, len(mappings))
			for _, m := range mappings {
				skipped = append(skipped, syncrun.SkippedMapping{
					Mapping:  m,
					Endpoint: m.TallyEndpoint,
					Result:   ingest.ConnStatusEndpointUnreachable,
					Reason:   fmt.Sprintf("could not probe Tally for %q: %v", m.TallyCompanyName, err),
				})
			}
			return nil, skipped, nil, fmt.Errorf("tally port probe failed: %w", err)
		}
	}
	var (
		resolved []ingest.ActiveMapping
		followed []syncrun.FollowedMapping
		skipped  []syncrun.SkippedMapping
		identity []syncrun.MappingIdentity
	)
	if enforce {
		resolved, followed, skipped, identity = syncrun.ResolveMappingEndpointsEnforced(mappings, probes)
	} else {
		resolved, followed, skipped = syncrun.ResolveMappingEndpoints(mappings, probes)
		// Checked against where each mapping was read (or, if skipped,
		// its mapped port). Reported only.
		identity = syncrun.ObserveIdentity(append(append([]ingest.ActiveMapping(nil), resolved...), skippedMappings(skipped)...), probes)
	}
	for _, c := range identity {
		logger.Info().
			Str("mode", identityModeName(enforce)).
			Str("mapping_id", c.MappingID).
			Str("mapping", c.Company).
			Str("endpoint", c.Endpoint).
			Bool("in_list_of_companies", c.InList).
			Strs("guid_probe_guids", c.GUIDs).
			Str("stored_guid", c.StoredGUID).
			Str("verdict", string(c.Verdict)).
			Msg("company identity check")
	}
	for _, f := range followed {
		logger.Warn().
			Str("mapping", f.Mapping.TallyCompanyName).
			Str("mapped_endpoint", f.From).
			Str("read_endpoint", f.To).
			Msg("company is open on another Tally port; reading it there")
	}
	if len(skipped) == 0 {
		return resolved, nil, identity, nil
	}
	reasons := make([]string, 0, len(skipped))
	for _, sk := range skipped {
		logger.Error().
			Str("mapping", sk.Mapping.TallyCompanyName).
			Str("mapping_id", sk.Mapping.MappingID).
			Str("endpoint", sk.Mapping.TallyEndpoint).
			Str("result", sk.Result).
			Msg(sk.Reason)
		reasons = append(reasons, sk.Reason)
	}
	return resolved, skipped, identity, fmt.Errorf("%d of %d mapped companies not readable: %s", len(skipped), len(mappings), strings.Join(reasons, "; "))
}

func skippedMappings(sk []syncrun.SkippedMapping) []ingest.ActiveMapping {
	out := make([]ingest.ActiveMapping, 0, len(sk))
	for _, s := range sk {
		out = append(out, s.Mapping)
	}
	return out
}

func errorFromWalkResult(res syncrun.WalkResult) error {
	if res.FatalErrors == 0 &&
		res.TotalBatchesFailed == 0 &&
		res.TotalServerErrors == 0 {
		return nil
	}

	return fmt.Errorf(
		"sync completed with failures: fatal_errors=%d batches_failed=%d server_errors=%d rows=%d batches_sent=%d mappings_run=%d",
		res.FatalErrors,
		res.TotalBatchesFailed,
		res.TotalServerErrors,
		res.TotalRows,
		res.TotalBatchesSent,
		res.MappingsRun,
	)
}

// syncMastersForMappings fetches vendor + customer master ledgers
// from each Tally company in the active mappings list and POSTs
// each batch to /api/tally/masters. Runs once at the start of every
// scheduled tick so the server's vendor_master / customer_master
// tables stay synchronised with Tally — populating email, phone,
// address, state code, GST registration type that the voucher feed
// alone doesn't carry.
//
// Best-effort: failures are logged at warn level and the daemon
// continues to the voucher walk. Masters are configuration; a stale
// row never blocks reconciliation.
//
// Per-mapping cost: 2 Tally fetches (vendor, customer) + up to 2
// HTTP POSTs. On a 500-vendor book this is < 5 seconds total per
// mapping. Cheaper than walking 4 voucher kinds.
func syncMastersForMappings(
	ctx context.Context,
	client *ingest.Client,
	newTallyClient func(string, ...tally.ClientOption) *tally.Client,
	mappings []ingest.ActiveMapping,
	logger zerolog.Logger,
) (authErr error) {
	for _, m := range mappings {
		tc := newTallyClient(m.ReadEndpoint())
		for _, kind := range []tally.MasterKind{tally.MasterVendor, tally.MasterCustomer} {
			env, err := tally.BuildMasterXML(tally.MasterRequest{
				Company: m.TallyCompanyName,
				Kind:    kind,
			})
			if err != nil {
				logger.Warn().
					Str("mapping", m.TallyCompanyName).
					Str("kind", string(kind)).
					Err(err).
					Msg("masters envelope build failed (skipping)")
				continue
			}
			fetchCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			resp, err := tc.PostXML(fetchCtx, env)
			cancel()
			if err != nil {
				logger.Warn().
					Str("mapping", m.TallyCompanyName).
					Str("kind", string(kind)).
					Err(err).
					Msg("masters fetch failed (skipping)")
				continue
			}
			parsed, err := tally.ParseMastersV3(resp)
			if err != nil {
				logger.Warn().
					Str("mapping", m.TallyCompanyName).
					Str("kind", string(kind)).
					Err(err).
					Msg("masters parse failed (skipping)")
				continue
			}
			items := tally.NormalizeMasters(parsed.Masters, kind)
			if len(items) == 0 {
				logger.Info().
					Str("mapping", m.TallyCompanyName).
					Str("kind", string(kind)).
					Int("raw_ledgers", len(parsed.Masters)).
					Msg("no GSTIN-bearing masters to push")
				continue
			}
			pushCtx, pushCancel := context.WithTimeout(ctx, 30*time.Second)
			err = client.SendMasters(pushCtx, tally.IngestMastersRequest{
				Kind:  kind,
				Items: items,
			})
			pushCancel()
			if err != nil {
				if _, _, isAuth := ingest.AuthFailure(err); isAuth && authErr == nil {
					authErr = err
				}
				logger.Warn().
					Str("mapping", m.TallyCompanyName).
					Str("kind", string(kind)).
					Int("items", len(items)).
					Err(err).
					Msg("masters push failed (continuing)")
				continue
			}
			logger.Info().
				Str("mapping", m.TallyCompanyName).
				Str("kind", string(kind)).
				Int("items", len(items)).
				Msg("masters pushed")
		}
	}
	return authErr
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

// autoUpdateMessage is the log line for one version check that found a
// newer release.
func autoUpdateMessage(outcome autoupdate.Outcome, err error) string {
	switch {
	case err != nil:
		return "self-update failed; will retry at a later check (or rerun the installer on this PC)"
	case outcome == autoupdate.OutcomeLaunched:
		return "self-update: installing the new release; the service restarts when it is done"
	case outcome == autoupdate.OutcomeBusy:
		return "self-update available; waiting for the running sync to finish"
	case outcome == autoupdate.OutcomeGaveUp:
		return "self-update did not install after several attempts — rerun the installer on this PC"
	case outcome == autoupdate.OutcomeNotNewer:
		return "self-update: already on the latest release"
	default:
		return "self-update available — rerun the installer on this PC to upgrade"
	}
}
