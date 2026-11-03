package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/heartbeat"
	"github.com/vishal0589/gstreco-tally-agent/internal/syncrun"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// QA v29 P3: ids below are obviously fake.

func quiet() zerolog.Logger { return zerolog.New(io.Discard) }

func TestOnSyncNow_RoutesRequestToTheRightRun(t *testing.T) {
	var current, period []heartbeat.SyncNow
	h := &daemonHeartbeatHandler{
		runtime: &daemonConnectionRuntime{info: config.PairedConnection{ConnectionID: "conn-fake"}},
		logger:  quiet(),
		runCurrentSync: func(_ context.Context, req heartbeat.SyncNow) error {
			current = append(current, req)
			return nil
		},
		runPeriodSync: func(_ context.Context, req heartbeat.SyncNow) error {
			period = append(period, req)
			return nil
		},
	}
	reqs := []heartbeat.SyncNow{
		{RequestID: "00000000-0000-4000-8000-0000000000a1"},
		{RequestID: "00000000-0000-4000-8000-0000000000a2", Period: "042026", Kinds: []string{"sales"}},
		{Period: " 052026 "}, // older server: no id
	}
	for _, r := range reqs {
		if err := h.OnSyncNow(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
	if len(current) != 1 || current[0].RequestID != reqs[0].RequestID {
		t.Fatalf("current-month runs = %+v", current)
	}
	if len(period) != 2 {
		t.Fatalf("period runs = %+v", period)
	}
	if period[0].RequestID != reqs[1].RequestID || period[0].Period != "042026" || len(period[0].Kinds) != 1 {
		t.Errorf("period run 1 = %+v", period[0])
	}
	if period[1].RequestID != "" || period[1].Period != "052026" {
		t.Errorf("old-server period run = %+v, want no id and period 052026", period[1])
	}
}

func TestSyncScopeFor(t *testing.T) {
	all := syncScopeFor(heartbeat.SyncNow{RequestID: " 00000000-0000-4000-8000-0000000000aa "}, quiet())
	if all.requestID != "00000000-0000-4000-8000-0000000000aa" {
		t.Errorf("requestID=%q", all.requestID)
	}
	if len(all.kinds) != len(syncrun.DefaultKinds) {
		t.Errorf("no kinds asked: got %d kinds, want every kind", len(all.kinds))
	}

	some := syncScopeFor(heartbeat.SyncNow{RequestID: "x", Kinds: []string{"tax_ledger", "purchase", "credit-note", "purchase"}}, quiet())
	var names []string
	for _, k := range some.kinds {
		names = append(names, k.Name)
	}
	if len(names) != 3 || names[0] != "purchase" || names[1] != "credit_note" || names[2] != "tax_ledger" {
		t.Errorf("kinds=%v, want [purchase credit_note tax_ledger] in walk order", names)
	}

	unknown := syncScopeFor(heartbeat.SyncNow{Kinds: []string{"stock_journal"}}, quiet())
	if len(unknown.kinds) != len(syncrun.DefaultKinds) {
		t.Errorf("only unknown kinds: got %d, want every kind", len(unknown.kinds))
	}

	if scheduledScope.requestID != "" || len(scheduledScope.kinds) != len(syncrun.DefaultKinds) {
		t.Errorf("scheduledScope = %+v, want no id and every kind", scheduledScope)
	}
}

func TestWalkOptionsFor_CarriesRequestID(t *testing.T) {
	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	scope := syncScopeFor(heartbeat.SyncNow{RequestID: "00000000-0000-4000-8000-0000000000aa", Kinds: []string{"journal"}}, quiet())
	opts := walkOptionsFor(nil, from, to, "manual-1", "manual", scope, nil, tally.NewClient, quiet())
	if opts.SyncRequestID != "00000000-0000-4000-8000-0000000000aa" {
		t.Errorf("SyncRequestID=%q", opts.SyncRequestID)
	}
	if len(opts.Kinds) != 1 || opts.Kinds[0].Name != "journal" {
		t.Errorf("Kinds=%v", opts.Kinds)
	}

	sched := walkOptionsFor(nil, from, to, "incremental-1", "incremental", scheduledScope, nil, tally.NewClient, quiet())
	if sched.SyncRequestID != "" {
		t.Errorf("scheduled walk SyncRequestID=%q, want empty", sched.SyncRequestID)
	}
	if len(sched.Kinds) != len(syncrun.DefaultKinds) {
		t.Errorf("scheduled walk kinds=%d, want every kind", len(sched.Kinds))
	}
}
