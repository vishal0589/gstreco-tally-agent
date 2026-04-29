package scheduler

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func quietLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

func TestNew_RejectsBadSpec(t *testing.T) {
	_, err := New(Options{
		Spec:     "not a cron",
		TickFunc: func(context.Context) error { return nil },
		Logger:   quietLogger(),
	})
	if err == nil {
		t.Fatal("expected error for bad spec")
	}
}

func TestNew_RequiresTickFunc(t *testing.T) {
	_, err := New(Options{Spec: "0 2 * * *", Logger: quietLogger()})
	if err == nil {
		t.Fatal("expected error for missing TickFunc")
	}
}

func TestNew_DefaultsToDefaultSpec(t *testing.T) {
	s, err := New(Options{
		TickFunc: func(context.Context) error { return nil },
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Spec() != DefaultSpec {
		t.Errorf("Spec=%q, want %q", s.Spec(), DefaultSpec)
	}
}

func TestRunNow_FiresTickFuncSynchronously(t *testing.T) {
	var calls int32
	s, err := New(Options{
		Spec: "0 2 * * *",
		TickFunc: func(_ context.Context) error {
			atomic.AddInt32(&calls, 1)
			return nil
		},
		Logger: quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RunNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls=%d, want 1", got)
	}
}

func TestRunNow_PropagatesCallerError(t *testing.T) {
	want := errors.New("boom")
	s, _ := New(Options{
		Spec:     "0 2 * * *",
		TickFunc: func(context.Context) error { return want },
		Logger:   quietLogger(),
	})
	if err := s.RunNow(context.Background()); !errors.Is(err, want) {
		t.Errorf("err=%v, want %v", err, want)
	}
}

func TestRunNow_AppliesPerTickDeadline(t *testing.T) {
	// TickFunc that blocks on ctx.Done. Per-tick deadline is short
	// so it fires before any real work would complete.
	tickDone := make(chan struct{})
	s, _ := New(Options{
		Spec: "0 2 * * *",
		TickFunc: func(ctx context.Context) error {
			<-ctx.Done()
			close(tickDone)
			return ctx.Err()
		},
		PerTickDeadline: 50 * time.Millisecond,
		Logger:          quietLogger(),
	})
	start := time.Now()
	err := s.RunNow(context.Background())
	dur := time.Since(start)
	<-tickDone

	if dur > 500*time.Millisecond {
		t.Errorf("RunNow took %v; per-tick deadline didn't bound it", dur)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err=%v, want DeadlineExceeded", err)
	}
}

func TestStartStop_DoesNotPanic(t *testing.T) {
	s, _ := New(Options{
		Spec:     "0 2 * * *",
		TickFunc: func(context.Context) error { return nil },
		Logger:   quietLogger(),
	})
	s.Start()
	s.Stop()
}

func TestDefaultSpec_FiresAtWorkingHoursMinutesPastTopOfHour(t *testing.T) {
	// DefaultSpec changed from "0 2 * * *" (overnight) to working
	// hours after the 2026-04-29 DIPL incident: agent ran at 2am
	// IST when Tally was closed, hit connection refused on every
	// kind, and waited a full 24 hours before retrying. Working
	// hours + retry-with-backoff prevent this class of stall.
	if DefaultSpec != "0 11,13,15,17 * * *" {
		t.Errorf("DefaultSpec=%q, want %q (working hours)", DefaultSpec, "0 11,13,15,17 * * *")
	}
	// Pin: the spec is parseable by robfig/cron/v3.
	s, err := New(Options{
		Spec:     DefaultSpec,
		TickFunc: func(context.Context) error { return nil },
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatalf("DefaultSpec %q failed to parse: %v", DefaultSpec, err)
	}
	if s.Spec() != DefaultSpec {
		t.Errorf("Spec()=%q, want %q", s.Spec(), DefaultSpec)
	}
}

func TestDefaultRetryDelays_AreOneAndTwoHours(t *testing.T) {
	// Pin the documented schedule: 1h then 2h. If product changes
	// the cadence, this test should be updated alongside the
	// DefaultRetryDelays var so the docs and tests don't drift.
	want := []time.Duration{1 * time.Hour, 2 * time.Hour}
	if len(DefaultRetryDelays) != len(want) {
		t.Fatalf("len(DefaultRetryDelays)=%d, want %d", len(DefaultRetryDelays), len(want))
	}
	for i, d := range want {
		if DefaultRetryDelays[i] != d {
			t.Errorf("DefaultRetryDelays[%d]=%v, want %v", i, DefaultRetryDelays[i], d)
		}
	}
}

func TestRetry_FiresAfterDelayOnCronDrivenFailure(t *testing.T) {
	// Cron-driven tick that fails once, succeeds on retry. With
	// short test delays we should observe two calls within the
	// timeout. Uses runTick (the cron-invoked path) directly to
	// simulate the cron firing it; retries inherit from this entry
	// point.
	var calls int32
	s, err := New(Options{
		Spec: "0 2 * * *",
		TickFunc: func(_ context.Context) error {
			n := atomic.AddInt32(&calls, 1)
			if n == 1 {
				return errors.New("first attempt fails")
			}
			return nil
		},
		RetryDelays: []time.Duration{20 * time.Millisecond},
		Logger:      quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	s.runTick()
	// Wait up to 500ms for the retry to fire + complete.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&calls) >= 2 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("calls=%d, want 2 (initial fail + 1 successful retry)", got)
	}
}

func TestRetry_AdvancesThroughAllDelaysOnRepeatedFailure(t *testing.T) {
	// Always-failing tickFn. With 2 retry delays we expect 3 total
	// invocations (1 initial + 2 retries) and then the scheduler
	// gives up until the next cron tick.
	var calls int32
	s, _ := New(Options{
		Spec:     "0 2 * * *",
		TickFunc: func(_ context.Context) error {
			atomic.AddInt32(&calls, 1)
			return errors.New("perpetual failure")
		},
		RetryDelays: []time.Duration{20 * time.Millisecond, 20 * time.Millisecond},
		Logger:      quietLogger(),
	})
	defer s.Stop()
	s.runTick()
	// Wait up to 500ms for both retries to complete.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&calls) >= 3 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("calls=%d, want 3 (1 initial + 2 retries)", got)
	}
	// Wait an extra 60ms — no further retries should fire.
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("after backoff exhausted: calls=%d, want stable at 3", got)
	}
}

func TestRetry_DisabledByEmptyRetryDelays(t *testing.T) {
	// RetryDelays: []time.Duration{} disables retries entirely. A
	// failing tickFn should run once and never re-fire.
	var calls int32
	s, _ := New(Options{
		Spec:     "0 2 * * *",
		TickFunc: func(_ context.Context) error {
			atomic.AddInt32(&calls, 1)
			return errors.New("permanent failure")
		},
		RetryDelays: []time.Duration{},
		Logger:      quietLogger(),
	})
	defer s.Stop()
	s.runTick()
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls=%d, want 1 (no retries when RetryDelays is empty)", got)
	}
}

func TestRetry_CancelledByStop(t *testing.T) {
	// Failing tickFn schedules a retry with a long delay. Stop()
	// before the delay elapses must cancel the pending retry.
	var calls int32
	s, _ := New(Options{
		Spec:     "0 2 * * *",
		TickFunc: func(_ context.Context) error {
			atomic.AddInt32(&calls, 1)
			return errors.New("first call fails")
		},
		RetryDelays: []time.Duration{2 * time.Second}, // long enough to outlast Stop()
		Logger:      quietLogger(),
	})
	s.runTick()
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("calls after first runTick=%d, want 1", got)
	}
	s.Stop()
	// Wait past when the retry timer would have fired.
	time.Sleep(100 * time.Millisecond)
	// Cancelled retry must not have invoked tickFn again.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls=%d after Stop(), want stable at 1 (retry cancelled)", got)
	}
}

func TestRunNow_DoesNotRetryOnFailure(t *testing.T) {
	// RunNow is operator-driven (Sync now button). A failure should
	// propagate to the caller and NOT enqueue a retry.
	var calls int32
	s, _ := New(Options{
		Spec: "0 2 * * *",
		TickFunc: func(_ context.Context) error {
			atomic.AddInt32(&calls, 1)
			return errors.New("operator-triggered fail")
		},
		RetryDelays: []time.Duration{20 * time.Millisecond},
		Logger:      quietLogger(),
	})
	defer s.Stop()
	if err := s.RunNow(context.Background()); err == nil {
		t.Fatal("expected RunNow to propagate the tickFn error")
	}
	// Wait past the would-be retry window. RunNow doesn't retry, so
	// calls should stay at 1.
	time.Sleep(60 * time.Millisecond)
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("calls=%d, want 1 (RunNow does not retry)", got)
	}
}

func TestRunningCount_TracksInFlight(t *testing.T) {
	gate := make(chan struct{})
	s, _ := New(Options{
		Spec: "0 2 * * *",
		TickFunc: func(_ context.Context) error {
			<-gate
			return nil
		},
		Logger: quietLogger(),
	})

	// Fire RunNow in a goroutine and observe RunningCount=1 mid-flight.
	done := make(chan error, 1)
	go func() {
		done <- s.RunNow(context.Background())
	}()

	// Wait until the tick is running.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if s.RunningCount() == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if got := s.RunningCount(); got != 1 {
		t.Errorf("RunningCount mid-tick = %d, want 1", got)
	}
	close(gate) // let it finish
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := s.RunningCount(); got != 0 {
		t.Errorf("RunningCount post-tick = %d, want 0", got)
	}
}
