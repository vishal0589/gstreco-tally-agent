// Package scheduler runs a registered tick function on a cron
// schedule. Thin wrapper over robfig/cron/v3 so the daemon doesn't
// import the cron library directly — keeps dependency surface focused
// and makes testing the daemon easier (tests use a fake scheduler
// that fires on demand).
package scheduler

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

// DefaultSpec is the cron expression used when config.ScheduleCron is
// empty. Matches the master plan's "02:00 daily" default and aligns
// with the typical CA workflow: GSTR-2B becomes available in the
// morning, sync runs overnight before the operator opens the laptop.
const DefaultSpec = "0 2 * * *"

// TickFunc is the callback fired on each scheduled tick. It receives
// a fresh ctx with the run's deadline already applied. Returning an
// error doesn't stop the schedule — it only logs.
type TickFunc func(ctx context.Context) error

// Scheduler wraps a cron schedule + tick function. One Scheduler per
// agent process; safe for concurrent use because robfig/cron is.
type Scheduler struct {
	cron     *cron.Cron
	logger   zerolog.Logger
	spec     string
	tickFn   TickFunc
	deadline time.Duration

	// Track running ticks so Stop() can wait for them. cron's own
	// Stop() returns a context that completes when running jobs
	// finish, but we want explicit visibility for tests.
	mu      sync.Mutex
	running int
}

// Options configures a new Scheduler.
type Options struct {
	// Spec is a cron expression. Empty = DefaultSpec. The cron
	// flavour is robfig/cron/v3 with seconds disabled (5-field POSIX
	// crontab). "0 2 * * *" = 02:00 every day.
	Spec string
	// TickFunc fires on each scheduled tick. Required.
	TickFunc TickFunc
	// PerTickDeadline caps the duration of one tick. Zero = no
	// deadline (the tick runs to completion). Daemon sets this to
	// 30 minutes — long enough for a multi-mapping sync, short
	// enough to fail loud if Tally hangs.
	PerTickDeadline time.Duration
	// Logger is used for tick start/end + error logs. Required.
	Logger zerolog.Logger
	// Location is the timezone the cron expression interprets.
	// Empty = time.Local. Daemon uses time.Local so a CA on IST
	// who configures "0 2 * * *" gets 02:00 IST, not UTC.
	Location *time.Location
}

// New builds a Scheduler. Returns an error for an invalid cron spec
// so the daemon fails fast at startup instead of silently scheduling
// nothing.
func New(opts Options) (*Scheduler, error) {
	if opts.TickFunc == nil {
		return nil, fmt.Errorf("scheduler: TickFunc is required")
	}
	spec := opts.Spec
	if spec == "" {
		spec = DefaultSpec
	}
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}

	s := &Scheduler{
		logger:   opts.Logger,
		spec:     spec,
		tickFn:   opts.TickFunc,
		deadline: opts.PerTickDeadline,
	}

	c := cron.New(cron.WithLocation(loc), cron.WithLogger(cron.DefaultLogger))
	if _, err := c.AddFunc(spec, s.runTick); err != nil {
		return nil, fmt.Errorf("scheduler: bad cron spec %q: %w", spec, err)
	}
	s.cron = c
	return s, nil
}

// Spec returns the resolved cron expression. Useful for the daemon's
// "scheduler started" log line so the operator can confirm the
// expression they configured took effect.
func (s *Scheduler) Spec() string { return s.spec }

// Start begins the schedule. Non-blocking — cron runs in a background
// goroutine. Caller is responsible for arranging Stop() before exit.
func (s *Scheduler) Start() {
	s.cron.Start()
	s.logger.Info().Str("spec", s.spec).Msg("scheduler started")
}

// Stop halts new ticks and waits for any in-flight tick to finish.
// Bounded by the per-tick deadline so a wedged tick doesn't block
// shutdown forever — the deadline cancels the tick's ctx, the tick
// returns, and Stop returns.
func (s *Scheduler) Stop() {
	stopCtx := s.cron.Stop()
	<-stopCtx.Done()
	s.logger.Info().Msg("scheduler stopped")
}

// RunNow fires the tick function immediately, outside the cron
// schedule. Used by the heartbeat path when the server requests
// "sync_now" — and by tests that want deterministic firing without
// waiting for a real cron tick.
func (s *Scheduler) RunNow(ctx context.Context) error {
	return s.runTickWithCtx(ctx)
}

// runTick is the cron-invoked entry point. Uses background ctx so a
// tick keeps running even if some upstream caller's ctx was
// cancelled. Per-tick deadline is applied internally.
func (s *Scheduler) runTick() {
	_ = s.runTickWithCtx(context.Background())
}

func (s *Scheduler) runTickWithCtx(parent context.Context) error {
	s.mu.Lock()
	s.running++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running--
		s.mu.Unlock()
	}()

	ctx := parent
	var cancel context.CancelFunc
	if s.deadline > 0 {
		ctx, cancel = context.WithTimeout(parent, s.deadline)
		defer cancel()
	}

	start := time.Now()
	s.logger.Info().Msg("tick start")
	err := s.tickFn(ctx)
	dur := time.Since(start)
	if err != nil {
		s.logger.Error().Err(err).Dur("duration", dur).Msg("tick failed")
		return err
	}
	s.logger.Info().Dur("duration", dur).Msg("tick complete")
	return nil
}

// RunningCount returns the number of in-flight ticks. Useful for
// tests that want to assert "no overlapping ticks" semantics.
func (s *Scheduler) RunningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}
