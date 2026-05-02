// Package heartbeat polls the GST Reco server every 60s for pending
// actions queued by the operator's UI ("Sync now", "Pause", etc.)
// and dispatches them via a Handler. The polling interval is fixed
// for V1 — the server's rate limit is 60 req/min per connection, so
// 60s gives us comfortable headroom plus alignment with the
// operator's expectation ("clicked Sync now ~30s ago").
package heartbeat

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
)

// DefaultInterval is the recurring poll cadence. Operators expect
// "Sync now" to land within ~60s; faster polling would chew through
// the 60 req/min rate limit and add noise to tally_ingest_log.
const DefaultInterval = 60 * time.Second

// Handler is the agent-side dispatch table. Each method corresponds
// to one HeartbeatAction. Methods are called serially per poll; if a
// poll returns three actions, OnSyncNow → OnPause → OnRevoke fire in
// order. Errors are logged but never returned to the server (the
// queue is at-most-once delivery; failed actions stay failed).
type Handler interface {
	// OnSyncNow fires when the operator clicked "Sync now". The
	// daemon's scheduler.RunNow is the obvious implementation — kick
	// off an immediate sync without waiting for the next cron tick.
	OnSyncNow(ctx context.Context, period string) error
	// OnPause fires when the operator paused the agent. Daemon
	// stops the scheduler. (Server-side `tally_connections.status`
	// also flips to 'paused'; the agent could rely on the next
	// heartbeat returning 403 instead, but explicit local pause is
	// cleaner.)
	OnPause(ctx context.Context) error
	// OnRevoke fires when the operator revoked the agent. Daemon
	// should stop everything and exit; the next start would hit a
	// 403 anyway. Implementation typically signals the daemon's
	// main ctx to cancel.
	OnRevoke(ctx context.Context) error
	// OnRefetchMappings fires when the operator wants the daemon to
	// re-read the active mapping list before the next tick (e.g.
	// after they changed a mapping in the UI and don't want to wait
	// for the natural 60s + tick boundary). Implementation typically
	// flushes any cached mappings + triggers a fresh fetch on next
	// scheduler.RunNow.
	OnRefetchMappings(ctx context.Context) error
	// OnScheduleChanged fires when the server returns a different
	// schedule_cron than the daemon's currently-configured one. The
	// daemon should re-create its scheduler with the new spec.
	// Skipped in V1 (daemon reads cron from config.yaml only;
	// server-driven re-scheduling lands when this returns nil-error).
	OnScheduleChanged(ctx context.Context, newCron string) error
}

// HeartbeatClient is the narrow surface Poller uses. *ingest.Client
// implements it; tests pass fakes.
type HeartbeatClient interface {
	Heartbeat(ctx context.Context, body ingest.HeartbeatRequest) (*ingest.HeartbeatResponse, error)
}

// Options configures a Poller. Zero values are sane: 60s interval,
// no-op for missing handler methods (caller must supply the Handler;
// the *struct* zero value is rejected at New time).
type Options struct {
	Client   HeartbeatClient
	Handler  Handler
	Interval time.Duration
	Logger   zerolog.Logger
	// AgentVersion is sent in every heartbeat body. Optional.
	AgentVersion string
	// LastTickReporter, when non-nil, is called once per poll to
	// gather the most-recent tick timestamp + status to include in
	// the heartbeat body. The daemon wires this up to its scheduler
	// state.
	LastTickReporter func() (time.Time, string)
}

// Poller runs the heartbeat loop. Start() launches the goroutine;
// Stop() signals it via ctx and waits for it to drain.
type Poller struct {
	opts   Options
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	// scheduleCron tracks the most recently observed server-side
	// schedule. OnScheduleChanged fires only on transitions.
	scheduleCron string
}

// New builds a Poller. Interval defaults to DefaultInterval. Handler
// + Client are required.
func New(opts Options) (*Poller, error) {
	if opts.Client == nil {
		return nil, errors.New("heartbeat: Client is required")
	}
	if opts.Handler == nil {
		return nil, errors.New("heartbeat: Handler is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = DefaultInterval
	}
	return &Poller{opts: opts}, nil
}

// Start launches the poll loop in a goroutine. parent ctx drives
// shutdown — when parent is cancelled, the loop exits at the next
// tick boundary or interrupted in-flight poll. Idempotent: calling
// Start twice is a no-op (only the first call wins).
func (p *Poller) Start(parent context.Context) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done != nil {
		return // already started
	}
	ctx, cancel := context.WithCancel(parent)
	p.cancel = cancel
	done := make(chan struct{})
	p.done = done
	// Capture done into the closure so loop() doesn't have to read
	// p.done after Start returns. Stop() may set p.done=nil under the
	// mutex while loop() is mid-flight; the captured local ref keeps
	// the close target stable. (Race detector flagged this on CI
	// before — see TestStartStopIdempotent.)
	go p.loop(ctx, done)
}

// Stop signals the loop to exit and waits for it to drain. Bounded
// by the per-poll timeout (10s default) — caller doesn't have to
// worry about waiting forever.
func (p *Poller) Stop() {
	p.mu.Lock()
	cancel := p.cancel
	done := p.done
	p.cancel = nil
	p.done = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

// loop is the goroutine body. Fires immediately on Start (so a
// freshly-started daemon picks up any backlog without a 60s wait),
// then on every tick. Takes its done channel as an argument (rather
// than reading p.done) so concurrent Stop() can safely null out
// p.done without racing the loop's defer.
func (p *Poller) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	p.poll(ctx)
	ticker := time.NewTicker(p.opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

// poll fires one heartbeat. Network errors are logged and don't
// trigger retries — the next tick handles them. Action dispatch
// errors are logged per-action but don't stop the rest of the
// actions in the response from running.
func (p *Poller) poll(parent context.Context) {
	pollCtx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()

	body := ingest.HeartbeatRequest{
		AgentVersion: p.opts.AgentVersion,
	}
	if p.opts.LastTickReporter != nil {
		t, status := p.opts.LastTickReporter()
		if !t.IsZero() {
			body.LastTickAt = t.UTC().Format(time.RFC3339)
		}
		if status != "" {
			body.LastTickStatus = status
		}
	}

	resp, err := p.opts.Client.Heartbeat(pollCtx, body)
	if err != nil {
		p.opts.Logger.Warn().Err(err).Msg("heartbeat failed")
		return
	}

	for _, action := range resp.PendingActions {
		p.dispatch(parent, action, resp.PendingSyncPeriod)
	}

	// Schedule change detection. Skipped on the first poll (we don't
	// know what the local schedule was before the server told us).
	if p.scheduleCron != "" && resp.ScheduleCron != "" && resp.ScheduleCron != p.scheduleCron {
		dispatchCtx, dispatchCancel := context.WithTimeout(parent, 5*time.Second)
		if err := p.opts.Handler.OnScheduleChanged(dispatchCtx, resp.ScheduleCron); err != nil {
			p.opts.Logger.Error().Err(err).Str("cron", resp.ScheduleCron).Msg("OnScheduleChanged failed")
		}
		dispatchCancel()
	}
	p.scheduleCron = resp.ScheduleCron
}

// dispatch routes one action to its Handler method. Each call gets a
// fresh ctx so a slow OnSyncNow doesn't block subsequent actions.
func (p *Poller) dispatch(parent context.Context, action ingest.HeartbeatAction, syncPeriod string) {
	dispatchCtx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	logger := p.opts.Logger.With().Str("action", string(action)).Logger()
	logger.Info().Msg("heartbeat action dispatch")

	var err error
	switch action {
	case ingest.HeartbeatActionSyncNow:
		err = p.opts.Handler.OnSyncNow(dispatchCtx, syncPeriod)
	case ingest.HeartbeatActionPause:
		err = p.opts.Handler.OnPause(dispatchCtx)
	case ingest.HeartbeatActionRevoke:
		err = p.opts.Handler.OnRevoke(dispatchCtx)
	case ingest.HeartbeatActionRefetchMappings:
		err = p.opts.Handler.OnRefetchMappings(dispatchCtx)
	default:
		// Forward-compat: server returned an action this agent
		// doesn't know. Log + skip rather than failing the whole
		// poll loop. Operator's dashboard will show the queued
		// action stayed cleared (server already cleared on read),
		// and the next operator who looks will see "the agent
		// version doesn't recognise that action — upgrade the agent".
		logger.Warn().Msg("unknown heartbeat action — skipping (consider upgrading the agent)")
		return
	}
	if err != nil {
		logger.Error().Err(err).Msg("heartbeat action failed")
	}
}
