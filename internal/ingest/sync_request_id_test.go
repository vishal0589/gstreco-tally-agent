package ingest

import (
	"context"
	"encoding/json"
	"testing"
)

// QA v29 P3: the ids here are obviously fake.

func TestHeartbeatResponse_DecodesSyncRequest(t *testing.T) {
	raw := `{"pending_actions":["sync_now"],"pending_sync_period":"032026",
		"sync_request_id":"00000000-0000-4000-8000-0000000000aa",
		"sync_request_period":"042026",
		"sync_request_kinds":["purchase","journal"],
		"schedule_cron":"0 2 * * *","server_time":"2026-11-03T10:00:00Z"}`
	var resp HeartbeatResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SyncRequestID != "00000000-0000-4000-8000-0000000000aa" {
		t.Errorf("SyncRequestID=%q", resp.SyncRequestID)
	}
	if got := resp.SyncNowPeriod(); got != "042026" {
		t.Errorf("SyncNowPeriod=%q, want 042026 (sync_request_period wins)", got)
	}
	if len(resp.SyncRequestKinds) != 2 || resp.SyncRequestKinds[0] != "purchase" || resp.SyncRequestKinds[1] != "journal" {
		t.Errorf("SyncRequestKinds=%v", resp.SyncRequestKinds)
	}
}

func TestHeartbeatResponse_NullRequestPeriodFallsBack(t *testing.T) {
	raw := `{"pending_actions":["sync_now"],"pending_sync_period":"032026",
		"sync_request_id":"00000000-0000-4000-8000-0000000000ab","sync_request_period":null,
		"sync_request_kinds":[],"schedule_cron":"0 2 * * *"}`
	var resp HeartbeatResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SyncRequestPeriod != nil {
		t.Errorf("SyncRequestPeriod=%v, want nil", *resp.SyncRequestPeriod)
	}
	if got := resp.SyncNowPeriod(); got != "032026" {
		t.Errorf("SyncNowPeriod=%q, want 032026", got)
	}
}

// An older server sends none of the new keys: nothing changes.
func TestHeartbeatResponse_OldServerHasNoSyncRequest(t *testing.T) {
	raw := `{"pending_actions":["sync_now"],"pending_sync_period":"032026","schedule_cron":"0 2 * * *","server_time":"2026-11-03T10:00:00Z"}`
	var resp HeartbeatResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SyncRequestID != "" || resp.SyncRequestPeriod != nil || resp.SyncRequestKinds != nil {
		t.Errorf("old server: got id=%q period=%v kinds=%v, want all empty",
			resp.SyncRequestID, resp.SyncRequestPeriod, resp.SyncRequestKinds)
	}
	if got := resp.SyncNowPeriod(); got != "032026" {
		t.Errorf("SyncNowPeriod=%q, want 032026", got)
	}
}

// A batch queued for retry keeps the request id it was built with.
func TestOutbox_RetriedBatchKeepsSyncRequestID(t *testing.T) {
	o := openTestOutbox(t)
	ctx := context.Background()
	stamped := sampleBody("run-1-1", true)
	stamped.SyncRequestID = "00000000-0000-4000-8000-0000000000aa"
	plain := sampleBody("run-2-1", true)
	plain.RunID = "run-2"
	if _, err := o.Enqueue(ctx, stamped); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Enqueue(ctx, plain); err != nil {
		t.Fatal(err)
	}
	pending, err := o.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Fatalf("pending=%d, want 2", len(pending))
	}
	if err := o.MarkFailed(ctx, pending[0].ID, "fake 503"); err != nil {
		t.Fatal(err)
	}
	pending, err = o.Pending(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := pending[0].Body.SyncRequestID; got != stamped.SyncRequestID {
		t.Errorf("retried batch SyncRequestID=%q, want %q", got, stamped.SyncRequestID)
	}
	if !pending[0].Body.IsFinal || pending[0].Attempts != 1 {
		t.Errorf("retried batch IsFinal=%t Attempts=%d, want true/1", pending[0].Body.IsFinal, pending[0].Attempts)
	}
	if got := pending[1].Body.SyncRequestID; got != "" {
		t.Errorf("unstamped batch picked up SyncRequestID=%q", got)
	}
	raw, err := json.Marshal(pending[1].Body)
	if err != nil {
		t.Fatal(err)
	}
	if containsKey(raw, "sync_request_id") {
		t.Errorf("unstamped replay body sends sync_request_id: %s", raw)
	}
}

func containsKey(raw []byte, key string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}
