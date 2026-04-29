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
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

// DefaultSpec is the cron expression used when config.ScheduleCron is
// empty. Fires at the top of 11am, 1pm, 3pm, 5pm in the configured
// timezone (typically IST for the CA market). Pre-v0.1.12 the default
// was "0 2 * * *" — daily 2am — which silently failed for any
// customer whose Tally instance was closed at that minute (DIPL hit
// this 2026-04-29: scheduler ran at 2am IST, Tally was closed,
// connection refused on every kind, scheduler waited a full 24 hours
// before its next attempt). Working-hours ticks plus per-tick
// retry-with-backoff (DefaultRetryDelays below) keep customers fresh
// without depending on the operator running Tally overnight.
const DefaultSpec = "0 11,13,15,17 * * *"

// DefaultRetryDelays is the per-attempt retry schedule applied to
// cron-driven ticks that return a non-nil error from TickFunc. The
// first failed tick re-fires after 1 hour, the second after 2 more
// hours, and then the scheduler waits for the next cron tick. With
// the working-hours DefaultSpec this gives roughly hourly recovery
// during the customer's business day without retry storms. RunNow
// (operator-triggered "Sync now") does NOT retry — the operator is
// already in front of the dashboard and can re-click if needed.
//
// Pass RetryDelays: []time.Duration{} via Options to disable retries
// (e.g. tests that assert "single tick").
var DefaultRetryDelays = []time.Duration{1 * time.Hour, 2 * time.Hour}

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

	retryDelays []time.Duration

	// Track running ticks so Stop() can wait for them. cron's own
	// Stop() returns a context that completes when running jobs
	// finish, but we want explicit visibility for tests.
	mu      sync.Mutex
	running int
	// retryTimers holds outstanding time.AfterFunc timers from
	// scheduleRetry. Stop() walks this slice to cancel pending
	// retries so a wedged customer doesn't keep firing ticks
	// after their daemon was supposed to shut down.
	retryTimers []*time.Timer

	// stopped is set true at the start of Stop(). Pending retries
	// (whose timer may already have started its countdown when the
	// timer is cancelled but the goroutine could still race in)
	// observe this and short-circuit before calling tickFn.
	stopped atomic.Bool
}

// Options configures a new Scheduler.
type Options struct {
	// Spec is a cron expression. Empty = DefaultSpec. The cron
	// flavour is robfig/cron/v3 with seconds disabled (5-field POSIX
	// crontab). "0 11,13,15,17 * * *" = 11:00, 13:00, 15:00, 17:00
	// every day.
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
	// who configures "0 11,13,15,17 * * *" gets local IST, not UTC.
	Location *time.Location
	// RetryDelays is the per-attempt retry schedule applied to
	// cron-driven ticks that return a non-nil error from TickFunc.
	// nil = DefaultRetryDelays. Pass an empty slice ([]time.Duration{})
	// to disable retries entirely (mostly useful in tests).
	RetryDelays []time.Duration
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

	delays := opts.RetryDelays
	if delays == nil {
		delays = DefaultRetryDelays
	}

	s := &Scheduler{
		logger:      opts.Logger,
		spec:        spec,
		tickFn:      opts.TickFunc,
		deadline:    opts.PerTickDeadline,
		retryDelays: delays,
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
// returns, and Stop returns. Also cancels any pending retry timers
// scheduled by scheduleRetry so a stuck-customer's daemon doesn't
// keep firing ticks after shutdown.
func (s *Scheduler) Stop() {
	s.stopped.Store(true)

	s.mu.Lock()
	for _, t := range s.retryTimers {
		t.Stop()
	}
	s.retryTimers = nil
	s.mu.Unlock()

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
// cancelled. Per-tick deadline is applied internally. Failures
// schedule a retry per retryDelays — see scheduleRetry. RunNow does
// NOT route through here, so operator-triggered ticks are not
// retried.
func (s *Scheduler) runTick() {
	if err := s.runTickWithCtx(context.Background()); err != nil {
		s.scheduleRetry(0)
	}
}

// scheduleRetry queues a delayed retry of the tick after a
// cron-driven failure. attempt is the 0-indexed offset into
// retryDelays — 0 = first retry, 1 = second, and so on. After the
// last retry attempt the scheduler waits for the next cron tick.
//
// If the retry itself fails, scheduleRetry is called recursively
// with attempt+1; the recursion bottoms out when attempt is past
// the end of retryDelays.
//
// Stop() flips s.stopped before cancelling timers; the inner
// goroutine re-checks s.stopped after the timer fires to avoid a
// race where the timer.Stop() in Stop() lost (Go 1.22+: timer.Stop
// returns false even when the func has already started running).
func (s *Scheduler) scheduleRetry(attempt int) {
	if s.stopped.Load() {
		return
	}
	if attempt >= len(s.retryDelays) {
		return
	}
	delay := s.retryDelays[attempt]
	s.logger.Info().
		Int("attempt", attempt+1).
		Int("max_attempts", len(s.retryDelays)).
		Dur("delay", delay).
		Msg("scheduler tick failed; queued retry")
	timer := time.AfterFunc(delay, func() {
		if s.stopped.Load() {
			return
		}
		if err := s.runTickWithCtx(context.Background()); err != nil {
			s.scheduleRetry(attempt + 1)
		}
	})
	s.mu.Lock()
	s.retryTimers = append(s.retryTimers, timer)
	s.mu.Unlock()
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
