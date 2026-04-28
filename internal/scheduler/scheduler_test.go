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
