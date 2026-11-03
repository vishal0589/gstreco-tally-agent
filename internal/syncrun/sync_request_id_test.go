package syncrun

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// QA v29 P3: a run started for a server sync request echoes the request's
// id on every batch of every kind, so the server links the run to the
// request instead of inferring it. The ids below are obviously fake.
const fakeSyncRequestID = "00000000-0000-4000-8000-0000000000aa"

func walkAllKinds(t *testing.T, syncRequestID string) *fakeSender {
	t.Helper()
	tly := &fakeTally{
		respond: func(body []byte) []byte {
			if bytes.Contains(body, []byte("GstrecoTaxLedgerCollection")) {
				return []byte(stubTaxLedgerResponse)
			}
			if isDutyLedgerRequest(body) {
				return []byte(stubDutyLedgers)
			}
			return []byte(stubResponse)
		},
	}
	snd := &fakeSender{}
	res := WalkAll(context.Background(), WalkOptions{
		Mappings:        makeMappings(2),
		Kinds:           DefaultKinds,
		From:            time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:              time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		RunKind:         "manual",
		SyncRequestID:   syncRequestID,
		ContinueOnError: true,
		NewTallyClient:  func(string) TallyPoster { return tly },
		Sender:          snd,
	})
	if res.FatalErrors != 0 {
		t.Fatalf("FatalErrors=%d, want 0", res.FatalErrors)
	}
	// 2 mappings × 4 invoice kinds, and 2 each of journal / payment / tax ledger.
	if len(snd.sent) != 8 || len(snd.journalSent) != 2 || len(snd.paymentSent) != 2 || len(snd.taxLedgerSent) != 2 {
		t.Fatalf("sends invoice=%d journal=%d payment=%d tax_ledger=%d, want 8/2/2/2",
			len(snd.sent), len(snd.journalSent), len(snd.paymentSent), len(snd.taxLedgerSent))
	}
	return snd
}

// allBatchJSON marshals every batch the fake sender saw, labelled by kind.
func allBatchJSON(t *testing.T, snd *fakeSender) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	add := func(label string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal %s: %v", label, err)
		}
		out[label] = raw
	}
	for i, b := range snd.sent {
		add(string(b.Kind)+"#"+intToStr(i), b)
	}
	for i, b := range snd.journalSent {
		add("journal#"+intToStr(i), b)
	}
	for i, b := range snd.paymentSent {
		add("payment#"+intToStr(i), b)
	}
	for i, b := range snd.taxLedgerSent {
		add("tax_ledger#"+intToStr(i), b)
	}
	return out
}

func TestWalkAll_SyncRequestIDOnEveryBatchOfEveryKind(t *testing.T) {
	snd := walkAllKinds(t, fakeSyncRequestID)

	kindsSeen := map[tally.IngestKind]bool{}
	for _, b := range snd.sent {
		kindsSeen[b.Kind] = true
		if b.SyncRequestID != fakeSyncRequestID {
			t.Errorf("%s batch %s: SyncRequestID=%q, want %q", b.Kind, b.RequestID, b.SyncRequestID, fakeSyncRequestID)
		}
		// One batch per run here: it is both the run-creating batch and the is_final one.
		if !b.IsFinal {
			t.Errorf("%s batch %s: want is_final on the single batch", b.Kind, b.RequestID)
		}
	}
	for _, k := range []tally.IngestKind{tally.IngestKindPurchase, tally.IngestKindSales, tally.IngestKindCreditNote, tally.IngestKindDebitNote} {
		if !kindsSeen[k] {
			t.Errorf("no %s batch sent", k)
		}
	}
	for _, b := range snd.journalSent {
		if b.SyncRequestID != fakeSyncRequestID {
			t.Errorf("journal batch %s: SyncRequestID=%q", b.RequestID, b.SyncRequestID)
		}
	}
	for _, b := range snd.paymentSent {
		if b.SyncRequestID != fakeSyncRequestID {
			t.Errorf("payment batch %s: SyncRequestID=%q", b.RequestID, b.SyncRequestID)
		}
	}
	for _, b := range snd.taxLedgerSent {
		if b.SyncRequestID != fakeSyncRequestID {
			t.Errorf("tax_ledger batch %s: SyncRequestID=%q", b.RequestID, b.SyncRequestID)
		}
	}
	for label, raw := range allBatchJSON(t, snd) {
		if !strings.Contains(string(raw), `"sync_request_id":"`+fakeSyncRequestID+`"`) {
			t.Errorf("%s: wire body lacks sync_request_id: %s", label, raw)
		}
	}
}

func TestWalkAll_ScheduledRunSendsNoSyncRequestID(t *testing.T) {
	snd := walkAllKinds(t, "")
	for label, raw := range allBatchJSON(t, snd) {
		if strings.Contains(string(raw), "sync_request_id") {
			t.Errorf("%s: scheduled run must not send sync_request_id: %s", label, raw)
		}
	}
}

// A run split into several batches carries the id on the first (the one
// that creates the run on the server), the middle and the is_final batch.
func TestRunOne_SyncRequestIDOnFirstAndFinalBatch(t *testing.T) {
	second := strings.Replace(
		strings.Replace(stubResponse, "<GUID>g-001</GUID>", "<GUID>g-002</GUID>", 1),
		"PI/001/26-27", "PI/002/26-27", 1)
	third := strings.Replace(
		strings.Replace(stubResponse, "<GUID>g-001</GUID>", "<GUID>g-003</GUID>", 1),
		"PI/001/26-27", "PI/003/26-27", 1)
	voucher := func(env string) string {
		start := strings.Index(env, "<VOUCHER>")
		end := strings.Index(env, "</VOUCHER>") + len("</VOUCHER>")
		return env[start:end]
	}
	multi := strings.Replace(stubResponse, "</VOUCHER>", "</VOUCHER>"+voucher(second)+voucher(third), 1)

	tly := &fakeTally{resp: []byte(multi)}
	snd := &fakeSender{}
	req := makeReq()
	req.BatchSize = 1
	req.SyncRequestID = fakeSyncRequestID
	if _, err := RunOne(context.Background(), tly, snd, req, nil); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(snd.sent) != 3 {
		t.Fatalf("sent %d batches, want 3", len(snd.sent))
	}
	for i, b := range snd.sent {
		if b.SyncRequestID != fakeSyncRequestID {
			t.Errorf("batch %d: SyncRequestID=%q, want %q", i+1, b.SyncRequestID, fakeSyncRequestID)
		}
		if b.RunID != req.RunID {
			t.Errorf("batch %d: RunID=%q, want %q", i+1, b.RunID, req.RunID)
		}
		if b.IsFinal != (i == 2) {
			t.Errorf("batch %d: IsFinal=%t", i+1, b.IsFinal)
		}
	}
}

// Two requests served back to back each keep their own id: nothing in the
// pipeline holds an id between runs.
func TestRunOne_TwoRequestsDoNotShareAnID(t *testing.T) {
	tly := &fakeTally{resp: []byte(stubResponse)}
	snd := &fakeSender{}
	ids := []string{
		"00000000-0000-4000-8000-0000000000a1",
		"00000000-0000-4000-8000-0000000000a2",
	}
	for i, id := range ids {
		req := makeReq()
		req.RunID = "test-run-" + intToStr(i+1)
		req.SyncRequestID = id
		if _, err := RunOne(context.Background(), tly, snd, req, nil); err != nil {
			t.Fatalf("RunOne %d: %v", i, err)
		}
	}
	// A third run with no request (a scheduled one) must not inherit either id.
	if _, err := RunOne(context.Background(), tly, snd, makeReq(), nil); err != nil {
		t.Fatalf("RunOne scheduled: %v", err)
	}
	if len(snd.sent) != 3 {
		t.Fatalf("sent %d, want 3", len(snd.sent))
	}
	if snd.sent[0].SyncRequestID != ids[0] || snd.sent[1].SyncRequestID != ids[1] || snd.sent[2].SyncRequestID != "" {
		t.Errorf("ids = %q, %q, %q; want %q, %q, \"\"",
			snd.sent[0].SyncRequestID, snd.sent[1].SyncRequestID, snd.sent[2].SyncRequestID, ids[0], ids[1])
	}
}
