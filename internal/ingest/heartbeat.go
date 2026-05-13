package ingest

import "context"

// HeartbeatPath is the daemon's recurring poll endpoint. Mirrors the
// server route at src/app/api/tally/heartbeat/route.ts (S10 / B3).
const HeartbeatPath = "/api/tally/heartbeat"

// HeartbeatAction is the closed set the agent recognises today.
// Forward-compat: agents that see an action they don't know skip it
// silently (we'd rather drop a known-good unknown than refuse to
// heartbeat at all).
type HeartbeatAction string

const (
	HeartbeatActionSyncNow         HeartbeatAction = "sync_now"
	HeartbeatActionPause           HeartbeatAction = "pause"
	HeartbeatActionRevoke          HeartbeatAction = "revoke"
	HeartbeatActionRefetchMappings HeartbeatAction = "refetch_mappings"
)

// HeartbeatRequest is the (small) telemetry the daemon sends with
// every poll. All fields optional; the server only cares about the
// HMAC headers. Sending these helps the operator's
// /settings/tally dashboard show "agent X reported tick Y at time Z".
type HeartbeatRequest struct {
	AgentVersion   string `json:"agent_version,omitempty"`
	LastTickAt     string `json:"last_tick_at,omitempty"`
	LastTickStatus string `json:"last_tick_status,omitempty"`
}

// HeartbeatResponse is what the daemon picks up on each poll. The
// server has already cleared its queue by the time the daemon sees
// this — the daemon must process every action exactly once on the
// agent side (best-effort: failure is logged but not retried, since
// the server only delivered each action once).
type HeartbeatResponse struct {
	PendingActions []HeartbeatAction `json:"pending_actions"`
	// Optional MMYYYY month code attached to a queued sync_now.
	// Empty means "run the daemon's current-month window".
	PendingSyncPeriod string `json:"pending_sync_period,omitempty"`
	// Authoritative cron expression. When the daemon's local
	// schedule disagrees, it should re-schedule to match.
	ScheduleCron string `json:"schedule_cron"`
	// Server time so the daemon can detect clock drift and warn.
	ServerTime string `json:"server_time"`
}

// Heartbeat posts a heartbeat and returns the server's pending
// actions + authoritative schedule. SendError on non-2xx (network +
// 5xx are retryable; auth + payload are give-up).
func (c *Client) Heartbeat(ctx context.Context, body HeartbeatRequest) (*HeartbeatResponse, error) {
	var resp HeartbeatResponse
	if err := c.PostJSONExpect(ctx, HeartbeatPath, body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
