package heartbeat

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
)

func quietLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

// fakeHeartbeatClient lets tests script a sequence of responses and
// observe the requests the poller sent.
type fakeHeartbeatClient struct {
	mu        sync.Mutex
	responses []*ingest.HeartbeatResponse
	errs      []error
	requests  []ingest.HeartbeatRequest
}

func (f *fakeHeartbeatClient) Heartbeat(_ context.Context, body ingest.HeartbeatRequest) (*ingest.HeartbeatResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, body)
	if len(f.responses) == 0 {
		return &ingest.HeartbeatResponse{ScheduleCron: "0 2 * * *"}, nil
	}
	r := f.responses[0]
	var err error
	if len(f.errs) > 0 {
		err = f.errs[0]
		f.errs = f.errs[1:]
	}
	f.responses = f.responses[1:]
	return r, err
}

// recordingHandler captures action names so tests can assert order.
type recordingHandler struct {
	mu          sync.Mutex
	actions     []string
	syncPeriods []string
	syncErr     error
}

func (h *recordingHandler) record(name string) {
	h.mu.Lock()
	h.actions = append(h.actions, name)
	h.mu.Unlock()
}

func (h *recordingHandler) OnSyncNow(_ context.Context, period string) error {
	h.mu.Lock()
	h.syncPeriods = append(h.syncPeriods, period)
	h.mu.Unlock()
	h.record("sync_now")
	return h.syncErr
}
func (h *recordingHandler) OnPause(_ context.Context) error {
	h.record("pause")
	return nil
}
func (h *recordingHandler) OnRevoke(_ context.Context) error {
	h.record("revoke")
	return nil
}
func (h *recordingHandler) OnRefetchMappings(_ context.Context) error {
	h.record("refetch_mappings")
	return nil
}
func (h *recordingHandler) OnScheduleChanged(_ context.Context, _ string) error {
	h.record("schedule_changed")
	return nil
}

func TestNew_RejectsMissingDeps(t *testing.T) {
	if _, err := New(Options{Handler: &recordingHandler{}}); err == nil {
		t.Error("missing Client should error")
	}
	if _, err := New(Options{Client: &fakeHeartbeatClient{}}); err == nil {
		t.Error("missing Handler should error")
	}
}

func TestNew_DefaultsInterval(t *testing.T) {
	p, err := New(Options{
		Client:  &fakeHeartbeatClient{},
		Handler: &recordingHandler{},
		Logger:  quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.opts.Interval != DefaultInterval {
		t.Errorf("Interval=%v, want %v", p.opts.Interval, DefaultInterval)
	}
}

func TestPoll_DispatchesActionsInOrder(t *testing.T) {
	client := &fakeHeartbeatClient{
		responses: []*ingest.HeartbeatResponse{{
			PendingActions: []ingest.HeartbeatAction{
				ingest.HeartbeatActionSyncNow,
				ingest.HeartbeatActionPause,
				ingest.HeartbeatActionRefetchMappings,
			},
			PendingSyncPeriod: "032026",
			ScheduleCron:      "0 2 * * *",
			ServerTime:        "2026-04-26T10:00:00Z",
		}},
	}
	handler := &recordingHandler{}
	p, _ := New(Options{
		Client: client, Handler: handler,
		Interval: time.Hour, // we drive via Start+immediate poll
		Logger:   quietLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	// Poller fires immediately on Start; wait for the actions to
	// dispatch.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		handler.mu.Lock()
		n := len(handler.actions)
		handler.mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	p.Stop()

	want := []string{"sync_now", "pause", "refetch_mappings"}
	handler.mu.Lock()
	got := append([]string{}, handler.actions...)
	handler.mu.Unlock()
	if len(got) < len(want) {
		t.Fatalf("got %v, want at least %v", got, want)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("action[%d]=%q, want %q (got=%v)", i, got[i], w, got)
		}
	}
	handler.mu.Lock()
	gotPeriods := append([]string{}, handler.syncPeriods...)
	handler.mu.Unlock()
	if len(gotPeriods) != 1 || gotPeriods[0] != "032026" {
		t.Errorf("syncPeriods=%v, want [032026]", gotPeriods)
	}
}

func TestPoll_UnknownActionLoggedAndSkipped(t *testing.T) {
	client := &fakeHeartbeatClient{
		responses: []*ingest.HeartbeatResponse{{
			PendingActions: []ingest.HeartbeatAction{
				"some_future_action",
				ingest.HeartbeatActionSyncNow,
			},
			ScheduleCron: "0 2 * * *",
		}},
	}
	handler := &recordingHandler{}
	p, _ := New(Options{
		Client: client, Handler: handler,
		Interval: time.Hour,
		Logger:   quietLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		handler.mu.Lock()
		n := len(handler.actions)
		handler.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	p.Stop()

	handler.mu.Lock()
	defer handler.mu.Unlock()
	// Only sync_now should land — the unknown action skips.
	if len(handler.actions) != 1 || handler.actions[0] != "sync_now" {
		t.Errorf("actions=%v, want [sync_now]", handler.actions)
	}
}

func TestPoll_NetworkErrorDoesNotPanicAndDoesNotDispatch(t *testing.T) {
	client := &fakeHeartbeatClient{
		responses: []*ingest.HeartbeatResponse{nil},
		errs:      []error{errors.New("network down")},
	}
	handler := &recordingHandler{}
	p, _ := New(Options{
		Client: client, Handler: handler,
		Interval: time.Hour,
		Logger:   quietLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()
	p.Stop()

	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.actions) != 0 {
		t.Errorf("actions=%v, want none", handler.actions)
	}
}

func TestPoll_ScheduleChangeFiresOnTransition(t *testing.T) {
	client := &fakeHeartbeatClient{
		responses: []*ingest.HeartbeatResponse{
			{ScheduleCron: "0 2 * * *"},
			{ScheduleCron: "0 4 * * *"}, // changed
		},
	}
	handler := &recordingHandler{}
	p, _ := New(Options{
		Client: client, Handler: handler,
		Interval: 30 * time.Millisecond,
		Logger:   quietLogger(),
	})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		handler.mu.Lock()
		n := len(handler.actions)
		handler.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	p.Stop()

	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.actions) != 1 || handler.actions[0] != "schedule_changed" {
		t.Errorf("actions=%v, want [schedule_changed]", handler.actions)
	}
}

func TestPoll_LastTickReporterFillsBody(t *testing.T) {
	client := &fakeHeartbeatClient{}
	handler := &recordingHandler{}
	tickAt := time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC)
	p, _ := New(Options{
		Client: client, Handler: handler,
		Interval:     time.Hour,
		Logger:       quietLogger(),
		AgentVersion: "0.5.0",
		LastTickReporter: func() (time.Time, string) {
			return tickAt, "ok"
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	cancel()
	p.Stop()

	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.requests) < 1 {
		t.Fatal("no heartbeat fired")
	}
	r := client.requests[0]
	if r.AgentVersion != "0.5.0" {
		t.Errorf("AgentVersion=%q", r.AgentVersion)
	}
	if r.LastTickAt != tickAt.Format(time.RFC3339) {
		t.Errorf("LastTickAt=%q", r.LastTickAt)
	}
	if r.LastTickStatus != "ok" {
		t.Errorf("LastTickStatus=%q", r.LastTickStatus)
	}
}

func TestStartStopIdempotent(t *testing.T) {
	p, _ := New(Options{
		Client:   &fakeHeartbeatClient{},
		Handler:  &recordingHandler{},
		Interval: time.Hour,
		Logger:   quietLogger(),
	})
	ctx := context.Background()
	p.Start(ctx)
	p.Start(ctx) // second Start should no-op
	var pollCount int32
	go func() {
		// Just observe RunningCount stays bounded.
		for i := 0; i < 5; i++ {
			atomic.AddInt32(&pollCount, 1)
			time.Sleep(10 * time.Millisecond)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	p.Stop()
	p.Stop() // second Stop should no-op
}
