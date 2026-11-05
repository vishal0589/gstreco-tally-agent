package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/syncrun"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// An older server without /api/tally/tax-ledger-vouchers answers 404: the
// lane is unsupported for the tick, and the tick (last_tick_status) and the
// connection's connection_status stay ok (QA v29 L19).

type tlvTally struct{}

func (tlvTally) PostXML(_ context.Context, body []byte) ([]byte, error) {
	if bytes.Contains(body, []byte("GstrecoTaxLedgerCollection")) {
		return []byte(`<ENVELOPE><BODY><DATA><COLLECTION><LEDGER><NAME>CGST INPUT</NAME><PARENT>Input Credit</PARENT></LEDGER></COLLECTION></DATA></BODY></ENVELOPE>`), nil
	}
	return []byte(`<ENVELOPE><BODY><DATA><COLLECTION><VOUCHER><GUID>g</GUID><DATE>20260910</DATE>
		<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>-1</AMOUNT></ALLLEDGERENTRIES.LIST>
		<ALLLEDGERENTRIES.LIST><LEDGERNAME>Bank</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER></COLLECTION></DATA></BODY></ENVELOPE>`), nil
}

type tlv404Sender struct{ calls int }

func (s *tlv404Sender) Send(context.Context, tally.IngestRequestBody) (ingest.AcceptedResponse, error) {
	return ingest.AcceptedResponse{}, nil
}
func (s *tlv404Sender) SendJournal(context.Context, tally.JournalIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	return tally.AccountingAcceptedResponse{}, nil
}
func (s *tlv404Sender) SendPayment(context.Context, tally.PaymentIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	return tally.AccountingAcceptedResponse{}, nil
}
func (s *tlv404Sender) SendTaxLedgers(context.Context, tally.TaxLedgerIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	return tally.AccountingAcceptedResponse{}, nil
}
func (s *tlv404Sender) SendTaxLedgerVouchers(context.Context, tally.TaxLedgerVoucherIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	s.calls++
	return tally.AccountingAcceptedResponse{}, &ingest.SendError{Kind: ingest.ErrorKindUnknown, Status: 404, Snippet: "Not Found"}
}

func TestTaxLedgerVoucherLane404KeepsTickOK(t *testing.T) {
	snd := &tlv404Sender{}
	var authErr error
	res := syncrun.WalkAll(context.Background(), syncrun.WalkOptions{
		Mappings:        []ingest.ActiveMapping{{MappingID: "m", TallyEndpoint: "http://127.0.0.1:9553", TallyCompanyName: "KSS"}},
		Kinds:           []syncrun.KindPair{{Name: "tax_ledger_voucher"}},
		From:            time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:              time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		RunKind:         "incremental",
		ContinueOnError: true,
		NewTallyClient:  func(string) syncrun.TallyPoster { return tlvTally{} },
		Sender:          snd,
		PerKindResultHook: func(_ ingest.ActiveMapping, _ syncrun.KindPair, r syncrun.Result, fatalErr error) {
			if err := walkAuthError(r, fatalErr); err != nil {
				authErr = err
			}
			if !r.LaneUnsupported || fatalErr != nil {
				t.Errorf("unsupported=%t fatal=%v", r.LaneUnsupported, fatalErr)
			}
		},
	})
	if snd.calls != 1 {
		t.Fatalf("calls=%d, want 1", snd.calls)
	}
	if err := errorFromWalkResult(res); err != nil {
		t.Errorf("tick must stay ok on a 404 lane: %v", err)
	}
	if authErr != nil {
		t.Errorf("a 404 is not this connection's auth failure: %v", authErr)
	}
}
