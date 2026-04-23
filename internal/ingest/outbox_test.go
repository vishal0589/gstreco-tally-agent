package ingest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func openTestOutbox(t *testing.T) *Outbox {
	t.Helper()
	path := filepath.Join(t.TempDir(), "outbox.db")
	o, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() })
	return o
}

func sampleBody(reqID string, final bool) tally.IngestRequestBody {
	return tally.IngestRequestBody{
		RunID:        "run-1",
		Kind:         tally.IngestKindPurchase,
		TallyCompany: "Acme Ltd",
		Batch: []tally.IngestVoucherRow{
			{InvoiceNumber: "PI-" + reqID, InvoiceDate: "2026-04-10", InvoiceValue: 100, TaxableValue: 100},
		},
		RequestID: reqID,
		IsFinal:   final,
	}
}

func TestOutbox_EnqueuePendingAck(t *testing.T) {
	o := openTestOutbox(t)
	ctx := context.Background()

	id1, err := o.Enqueue(ctx, sampleBody("req-1", false))
	if err != nil || id1 == 0 {
		t.Fatalf("Enqueue: id=%d err=%v", id1, err)
	}
	id2, _ := o.Enqueue(ctx, sampleBody("req-2", true))
	if id2 <= id1 {
		t.Errorf("second enqueue id %d not after first %d", id2, id1)
	}

	got, err := o.Pending(ctx, 10)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pending len = %d, want 2", len(got))
	}
	// FIFO order (oldest first).
	if got[0].RequestID != "req-1" || got[1].RequestID != "req-2" {
		t.Errorf("order wrong: %s, %s", got[0].RequestID, got[1].RequestID)
	}
	if !got[1].IsFinal {
		t.Error("is_final flag not round-tripping")
	}
	// Body round-trip.
	if got[0].Body.RunID != "run-1" || got[0].Body.Kind != tally.IngestKindPurchase {
		t.Errorf("body round-trip broken: %+v", got[0].Body)
	}

	if err := o.Ack(ctx, id1); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	after, _ := o.Pending(ctx, 10)
	if len(after) != 1 || after[0].RequestID != "req-2" {
		t.Errorf("after ack: %+v", after)
	}
}

func TestOutbox_EnqueueDeduplicatesByRequestID(t *testing.T) {
	o := openTestOutbox(t)
	ctx := context.Background()

	id1, _ := o.Enqueue(ctx, sampleBody("dup", false))
	id2, _ := o.Enqueue(ctx, sampleBody("dup", false))
	if id1 == 0 {
		t.Fatalf("first enqueue failed")
	}
	// Second enqueue of same request_id: SQLite INSERT OR IGNORE returns
	// LastInsertId = 0 (no row inserted).
	if id2 != 0 {
		t.Errorf("duplicate enqueue returned id=%d, want 0", id2)
	}
	count, _ := o.Count(ctx)
	if count != 1 {
		t.Errorf("count = %d, want 1", count)
	}
}

func TestOutbox_MarkFailedBumpsAttempts(t *testing.T) {
	o := openTestOutbox(t)
	ctx := context.Background()
	id, _ := o.Enqueue(ctx, sampleBody("req-1", false))

	if err := o.MarkFailed(ctx, id, "connection refused"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if err := o.MarkFailed(ctx, id, "timeout"); err != nil {
		t.Fatalf("MarkFailed 2: %v", err)
	}
	got, _ := o.Pending(ctx, 10)
	if len(got) != 1 {
		t.Fatalf("Pending len = %d", len(got))
	}
	if got[0].Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", got[0].Attempts)
	}
	if got[0].LastError != "timeout" {
		t.Errorf("LastError = %q", got[0].LastError)
	}
	if got[0].LastAttempt.IsZero() {
		t.Error("LastAttempt not set")
	}
}

func TestOutbox_DropRemoves(t *testing.T) {
	o := openTestOutbox(t)
	ctx := context.Background()
	id, _ := o.Enqueue(ctx, sampleBody("req-1", false))

	if err := o.Drop(ctx, id); err != nil {
		t.Fatalf("Drop: %v", err)
	}
	got, _ := o.Pending(ctx, 10)
	if len(got) != 0 {
		t.Errorf("after Drop: %+v", got)
	}
}

func TestOutbox_Count(t *testing.T) {
	o := openTestOutbox(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, _ = o.Enqueue(ctx, sampleBody("req-"+intToStr(i+1), i == 2))
	}
	n, err := o.Count(ctx)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 3 {
		t.Errorf("Count = %d, want 3", n)
	}
}

func TestOutbox_ReopenPersistsEntries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "outbox.db")

	o1, err := Open(path)
	if err != nil {
		t.Fatalf("Open 1: %v", err)
	}
	_, _ = o1.Enqueue(ctx, sampleBody("req-persist", false))
	_ = o1.Close()

	o2, err := Open(path)
	if err != nil {
		t.Fatalf("Open 2: %v", err)
	}
	defer o2.Close()
	got, _ := o2.Pending(ctx, 10)
	if len(got) != 1 || got[0].RequestID != "req-persist" {
		t.Errorf("entry did not survive reopen: %+v", got)
	}
}
