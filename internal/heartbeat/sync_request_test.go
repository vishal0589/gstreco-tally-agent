package heartbeat

import (
	"context"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
)

// QA v29 P3: ids below are obviously fake.

func strPtr(s string) *string { return &s }

// pollSyncNows runs the poller until it has dispatched want sync_now
// actions, then returns what the handler saw.
func pollSyncNows(t *testing.T, responses []*ingest.HeartbeatResponse, want int) []SyncNow {
	t.Helper()
	client := &fakeHeartbeatClient{responses: responses}
	handler := &recordingHandler{}
	p, err := New(Options{Client: client, Handler: handler, Interval: 20 * time.Millisecond, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.Start(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		handler.mu.Lock()
		n := len(handler.syncNows)
		handler.mu.Unlock()
		if n >= want {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	p.Stop()
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if len(handler.syncNows) < want {
		t.Fatalf("dispatched %d sync_now, want %d", len(handler.syncNows), want)
	}
	return append([]SyncNow{}, handler.syncNows...)
}

func TestPoll_SyncNowCarriesServerRequest(t *testing.T) {
	got := pollSyncNows(t, []*ingest.HeartbeatResponse{{
		PendingActions:    []ingest.HeartbeatAction{ingest.HeartbeatActionSyncNow},
		PendingSyncPeriod: "032026",
		SyncRequestID:     "00000000-0000-4000-8000-0000000000aa",
		SyncRequestPeriod: strPtr("042026"),
		SyncRequestKinds:  []string{"purchase", " ", "journal"},
		ScheduleCron:      "0 2 * * *",
	}}, 1)
	req := got[0]
	if req.RequestID != "00000000-0000-4000-8000-0000000000aa" {
		t.Errorf("RequestID=%q", req.RequestID)
	}
	if req.Period != "042026" {
		t.Errorf("Period=%q, want 042026", req.Period)
	}
	if len(req.Kinds) != 2 || req.Kinds[0] != "purchase" || req.Kinds[1] != "journal" {
		t.Errorf("Kinds=%v, want [purchase journal]", req.Kinds)
	}
}

// An older server sends none of the new keys: the handler sees the same
// period as before and no request id or kinds.
func TestPoll_OldServerSyncNowHasNoRequestID(t *testing.T) {
	got := pollSyncNows(t, []*ingest.HeartbeatResponse{{
		PendingActions:    []ingest.HeartbeatAction{ingest.HeartbeatActionSyncNow},
		PendingSyncPeriod: "032026",
		ScheduleCron:      "0 2 * * *",
	}}, 1)
	if got[0].RequestID != "" || got[0].Kinds != nil || got[0].Period != "032026" {
		t.Errorf("old server: got %+v, want period 032026 only", got[0])
	}
}

// Two sync_now deliveries on successive polls reach the handler as two
// separate values, each with its own id.
func TestPoll_TwoSyncNowsKeepTheirOwnIDs(t *testing.T) {
	got := pollSyncNows(t, []*ingest.HeartbeatResponse{
		{
			PendingActions: []ingest.HeartbeatAction{ingest.HeartbeatActionSyncNow},
			SyncRequestID:  "00000000-0000-4000-8000-0000000000a1",
			ScheduleCron:   "0 2 * * *",
		},
		{
			PendingActions: []ingest.HeartbeatAction{ingest.HeartbeatActionSyncNow},
			SyncRequestID:  "00000000-0000-4000-8000-0000000000a2",
			ScheduleCron:   "0 2 * * *",
		},
	}, 2)
	if got[0].RequestID != "00000000-0000-4000-8000-0000000000a1" || got[1].RequestID != "00000000-0000-4000-8000-0000000000a2" {
		t.Errorf("ids = %q, %q", got[0].RequestID, got[1].RequestID)
	}
}
