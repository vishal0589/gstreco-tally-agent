package syncrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// QA 05-Nov D12: a lane served for a sync request that finds nothing in
// Tally sends one empty final batch, so the server can say "zero rows"
// instead of "no batch received". Accounting batches carry the run kind,
// the window and is_final on their last batch, so the server closes the run
// when the lane is done.

const emptyDayBook = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA></DATA></BODY></ENVELOPE>`

func requestReq(kindName string) Request {
	req := makeReq()
	req.SyncRequestID = fakeSyncRequestID
	switch kindName {
	case "journal":
		req.KindName, req.TallyKind, req.IngestKind = "journal", tally.VoucherJournal, ""
	case "payment":
		req.KindName, req.TallyKind, req.IngestKind = "payment", tally.VoucherPayment, ""
	case "tax_ledger":
		req.KindName, req.IngestKind = "tax_ledger", ""
	}
	return req
}

func TestZeroLane_InvoiceSendsOneEmptyFinalBatchForARequest(t *testing.T) {
	snd := &fakeSender{}
	res, err := RunOne(context.Background(), &fakeTally{resp: []byte(emptyDayBook)}, snd, requestReq("purchase"), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(snd.sent) != 1 || res.BatchesSent != 1 {
		t.Fatalf("sends=%d batchesSent=%d, want 1", len(snd.sent), res.BatchesSent)
	}
	body := snd.sent[0]
	if !body.IsFinal || len(body.Batch) != 0 || body.SyncRequestID != fakeSyncRequestID {
		t.Fatalf("batch: is_final=%t rows=%d sync_request_id=%q", body.IsFinal, len(body.Batch), body.SyncRequestID)
	}
	raw, _ := json.Marshal(body)
	if !strings.Contains(string(raw), `"batch":[]`) {
		t.Fatalf("empty batch must marshal as [], got %s", raw)
	}
}

func TestZeroLane_ScheduledRunSendsNothing(t *testing.T) {
	snd := &fakeSender{}
	if _, err := RunOne(context.Background(), &fakeTally{resp: []byte(emptyDayBook)}, snd, makeReq(), nil); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(snd.sent) != 0 {
		t.Fatalf("scheduled empty lane sent %d batches, want 0", len(snd.sent))
	}
}

func TestZeroLane_UnreadableVouchersAreNotZeroRows(t *testing.T) {
	// A voucher Tally returned that cannot be normalised is held by a sync
	// warning, never reported as an empty lane.
	const unreadable = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
    <VOUCHER><GUID>bad-1</GUID><VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME></VOUCHER>
  </COLLECTION></DATA></BODY></ENVELOPE>`
	snd := &fakeSender{}
	res, err := RunOne(context.Background(), &fakeTally{resp: []byte(unreadable)}, snd, requestReq("purchase"), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.RowCount != 0 {
		t.Skipf("stub voucher normalised into %d rows; case not reachable with this stub", res.RowCount)
	}
	if len(snd.sent) != 0 {
		t.Fatalf("unreadable lane sent %d batches, want 0", len(snd.sent))
	}
}

func TestZeroLane_AccountingLanesSendEmptyFinalBatch(t *testing.T) {
	for _, kind := range []string{"journal", "payment", "tax_ledger"} {
		t.Run(kind, func(t *testing.T) {
			snd := &fakeSender{}
			resp := emptyDayBook
			if kind == "tax_ledger" {
				resp = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION></COLLECTION></DATA></BODY></ENVELOPE>`
			}
			if _, err := RunOne(context.Background(), &fakeTally{resp: []byte(resp)}, snd, requestReq(kind), nil); err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			var raw []byte
			switch kind {
			case "journal":
				if len(snd.journalSent) != 1 {
					t.Fatalf("journal sends=%d, want 1", len(snd.journalSent))
				}
				raw, _ = json.Marshal(snd.journalSent[0])
			case "payment":
				if len(snd.paymentSent) != 1 {
					t.Fatalf("payment sends=%d, want 1", len(snd.paymentSent))
				}
				raw, _ = json.Marshal(snd.paymentSent[0])
			case "tax_ledger":
				if len(snd.taxLedgerSent) != 1 {
					t.Fatalf("tax ledger sends=%d, want 1", len(snd.taxLedgerSent))
				}
				raw, _ = json.Marshal(snd.taxLedgerSent[0])
			}
			for _, want := range []string{`"batch":[]`, `"is_final":true`, `"run_kind":"manual"`, `"period_from":"2026-04-01"`, `"period_to":"2026-04-30"`} {
				if !strings.Contains(string(raw), want) {
					t.Errorf("%s batch missing %s: %s", kind, want, raw)
				}
			}
		})
	}
}

func TestAccountingBatches_OnlyTheLastIsFinal(t *testing.T) {
	rows := make([]tally.JournalVoucherRow, 5)
	batches := buildJournalBatches("run", "run", "KSS", nil, rows, 2, false)
	if len(batches) != 3 {
		t.Fatalf("batches=%d, want 3", len(batches))
	}
	// The run function sets is_final; the builder alone leaves it false.
	for _, b := range batches {
		if b.IsFinal {
			t.Fatalf("builder must not set is_final")
		}
	}
	if got := splitOrEmptyFinal([]int{}, 2, false); got != nil {
		t.Fatalf("no rows without emptyFinal must give no batch, got %v", got)
	}
	if got := splitOrEmptyFinal([]int{}, 2, true); len(got) != 1 || got[0] == nil || len(got[0]) != 0 {
		t.Fatalf("emptyFinal must give one empty non-nil batch, got %#v", got)
	}
}
