package syncrun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// stubResponse — minimal Tally Prime 3.x day-book with one purchase
// voucher. Identical shape to what cmd/agentctl/sync_test.go uses so
// the two test suites stay in sync on what "valid Tally XML" means.
const stubResponse = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260410</DATE>
      <GUID>g-001</GUID>
      <ALTERID>42</ALTERID>
      <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
      <VOUCHERNUMBER>PI/001/26-27</VOUCHERNUMBER>
      <PARTYLEDGERNAME>SMOKE Vendors Ltd</PARTYLEDGERNAME>
      <PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN>
      <ISINVOICE>Yes</ISINVOICE>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>SMOKE Vendors Ltd</LEDGERNAME>
        <AMOUNT>11800.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>Purchases A/c</LEDGERNAME>
        <AMOUNT>-10000.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>IGST @ 18%</LEDGERNAME>
        <GSTCLASS>IGST@18</GSTCLASS>
        <AMOUNT>-1800.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
    </VOUCHER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

type fakeTally struct {
	resp    []byte
	err     error
	got     []byte
	respond func(body []byte) []byte
}

func (f *fakeTally) PostXML(_ context.Context, body []byte) ([]byte, error) {
	f.got = body
	if f.respond != nil {
		return f.respond(body), f.err
	}
	return f.resp, f.err
}

type fakeSender struct {
	sent           []tally.IngestRequestBody
	journalSent    []tally.JournalIngestRequestBody
	paymentSent    []tally.PaymentIngestRequestBody
	taxLedgerSent  []tally.TaxLedgerIngestRequestBody
	err            error
	resp           ingest.AcceptedResponse
	accountingResp tally.AccountingAcceptedResponse
}

func (f *fakeSender) Send(_ context.Context, body tally.IngestRequestBody) (ingest.AcceptedResponse, error) {
	f.sent = append(f.sent, body)
	if f.err != nil {
		return ingest.AcceptedResponse{}, f.err
	}
	resp := f.resp
	if resp.Counters == (ingest.AcceptedCounters{}) {
		resp.Counters.Inserted = len(body.Batch)
	}
	return resp, nil
}

func (f *fakeSender) SendJournal(_ context.Context, body tally.JournalIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	f.journalSent = append(f.journalSent, body)
	if f.err != nil {
		return tally.AccountingAcceptedResponse{}, f.err
	}
	resp := f.accountingResp
	if resp.Processed == 0 {
		resp.Processed = len(body.Batch)
	}
	return resp, nil
}

func (f *fakeSender) SendPayment(_ context.Context, body tally.PaymentIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	f.paymentSent = append(f.paymentSent, body)
	if f.err != nil {
		return tally.AccountingAcceptedResponse{}, f.err
	}
	resp := f.accountingResp
	if resp.Processed == 0 {
		resp.Processed = len(body.Batch)
	}
	return resp, nil
}

func (f *fakeSender) SendTaxLedgers(_ context.Context, body tally.TaxLedgerIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	f.taxLedgerSent = append(f.taxLedgerSent, body)
	if f.err != nil {
		return tally.AccountingAcceptedResponse{}, f.err
	}
	resp := f.accountingResp
	if resp.Processed == 0 {
		resp.Processed = len(body.Batch)
	}
	return resp, nil
}

func makeReq() Request {
	return Request{
		TallyCompany:  "PLLUM CASA",
		TallyEndpoint: "http://127.0.0.1:9000",
		TallyKind:     tally.VoucherPurchase,
		IngestKind:    tally.IngestKindPurchase,
		From:          time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:            time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		RunID:         "test-run-1",
		RunKind:       "manual",
	}
}

func TestRunOne_HappyPath_PostsOneBatch(t *testing.T) {
	tly := &fakeTally{resp: []byte(stubResponse)}
	snd := &fakeSender{}

	res, err := RunOne(context.Background(), tly, snd, makeReq(), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(snd.sent) != 1 || res.BatchesSent != 1 {
		t.Errorf("sent=%d res.BatchesSent=%d, want 1", len(snd.sent), res.BatchesSent)
	}
	if snd.sent[0].TallyEndpoint == nil || *snd.sent[0].TallyEndpoint != "http://127.0.0.1:9000" {
		t.Errorf("TallyEndpoint=%v, want http://127.0.0.1:9000", snd.sent[0].TallyEndpoint)
	}
	if snd.sent[0].PeriodFrom == nil || *snd.sent[0].PeriodFrom != "2026-04-01" {
		t.Errorf("PeriodFrom=%v, want 2026-04-01", snd.sent[0].PeriodFrom)
	}
	if snd.sent[0].PeriodTo == nil || *snd.sent[0].PeriodTo != "2026-04-30" {
		t.Errorf("PeriodTo=%v, want 2026-04-30", snd.sent[0].PeriodTo)
	}
	if res.RowCount != 1 {
		t.Errorf("RowCount=%d, want 1", res.RowCount)
	}
	if !snd.sent[0].IsFinal {
		t.Error("single-batch run should have IsFinal=true")
	}
	if !strings.Contains(string(tly.got), "PLLUM CASA") {
		t.Errorf("envelope missing company; body=%s", tly.got)
	}
	if res.ParseStatus != 1 {
		t.Errorf("ParseStatus=%d, want 1", res.ParseStatus)
	}
	if res.ServerInserted != 1 || res.ServerSkipped != 0 || res.ServerErrors != 0 {
		t.Errorf("server counters = inserted:%d skipped:%d errors:%d, want 1/0/0",
			res.ServerInserted, res.ServerSkipped, res.ServerErrors)
	}
}

func TestRunOne_ReferenceOnlyVoucherStillPostsBatch(t *testing.T) {
	const referenceOnlyVoucher = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260410</DATE>
      <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
      <REFERENCE>SUPP-INV-5501</REFERENCE>
      <PARTYLEDGERNAME>SMOKE Vendors Ltd</PARTYLEDGERNAME>
      <PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN>
      <ISINVOICE>Yes</ISINVOICE>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>SMOKE Vendors Ltd</LEDGERNAME>
        <AMOUNT>-11800.00</AMOUNT>
        <BILLALLOCATIONS.LIST>
          <NAME>SUPP-INV-5501</NAME>
          <BILLTYPE>New Ref</BILLTYPE>
          <AMOUNT>-11800.00</AMOUNT>
        </BILLALLOCATIONS.LIST>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>Purchases A/c</LEDGERNAME>
        <AMOUNT>10000.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>IGST @ 18%</LEDGERNAME>
        <GSTCLASS>IGST@18</GSTCLASS>
        <AMOUNT>1800.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
    </VOUCHER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

	tly := &fakeTally{resp: []byte(referenceOnlyVoucher)}
	snd := &fakeSender{}

	res, err := RunOne(context.Background(), tly, snd, makeReq(), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(snd.sent) != 1 || res.BatchesSent != 1 {
		t.Fatalf("sent=%d batchesSent=%d, want 1", len(snd.sent), res.BatchesSent)
	}
	if res.RowCount != 1 {
		t.Fatalf("RowCount=%d, want 1", res.RowCount)
	}
	row := snd.sent[0].Batch[0]
	if row.InvoiceNumber != "SUPP-INV-5501" {
		t.Errorf("InvoiceNumber=%q, want SUPP-INV-5501", row.InvoiceNumber)
	}
	if row.TallyVoucherGUID != nil {
		t.Errorf("TallyVoucherGUID=%v, want nil when GUID is absent", row.TallyVoucherGUID)
	}
	if res.ParseWarnings != nil && len(res.ParseWarnings) != 0 {
		t.Errorf("unexpected parse warnings: %v", res.ParseWarnings)
	}
}

func TestRunOne_NormalizeFailureCountsButContinues(t *testing.T) {
	// Voucher with PartyLedgerName but zero ledger entries — parses
	// fine, but the normaliser can't find a party ledger and returns
	// an error. Exercises the DroppedOnNormalize counter.
	const noLedgerVoucher = `<ENVELOPE>
      <HEADER><STATUS>1</STATUS></HEADER>
      <BODY><DATA><COLLECTION>
        <VOUCHER>
          <DATE>20260410</DATE>
          <GUID>g-bad</GUID>
          <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
          <VOUCHERNUMBER>BAD-01</VOUCHERNUMBER>
          <PARTYLEDGERNAME>Some Vendor</PARTYLEDGERNAME>
          <PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN>
          <ISINVOICE>Yes</ISINVOICE>
          <ISCANCELLED>No</ISCANCELLED>
        </VOUCHER>
      </COLLECTION></DATA></BODY>
    </ENVELOPE>`
	tly := &fakeTally{resp: []byte(noLedgerVoucher)}
	snd := &fakeSender{}

	res, err := RunOne(context.Background(), tly, snd, makeReq(), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.DroppedOnNormalize != 1 {
		t.Errorf("DroppedOnNormalize=%d, want 1", res.DroppedOnNormalize)
	}
	if res.RowCount != 0 || len(snd.sent) != 0 {
		t.Errorf("expected nothing sent; rows=%d sent=%d", res.RowCount, len(snd.sent))
	}
}

func TestRunOne_BatchFailureCapturedNotFatal(t *testing.T) {
	tly := &fakeTally{resp: []byte(stubResponse)}
	snd := &fakeSender{err: errors.New("server boom")}

	res, err := RunOne(context.Background(), tly, snd, makeReq(), nil)
	if err != nil {
		t.Fatalf("RunOne returned err on per-batch failure: %v", err)
	}
	if res.BatchesFailed != 1 {
		t.Errorf("BatchesFailed=%d, want 1", res.BatchesFailed)
	}
	if len(res.BatchErrors) != 1 || res.BatchErrors[0].Err == nil {
		t.Errorf("BatchErrors=%+v", res.BatchErrors)
	}
}

func TestRunOne_JournalPathPostsOneBatch(t *testing.T) {
	const journalResponse = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260415</DATE>
      <GUID>j-001</GUID>
      <ALTERID>84</ALTERID>
      <VOUCHERTYPENAME>Journal</VOUCHERTYPENAME>
      <VOUCHERNUMBER>JV-001</VOUCHERNUMBER>
      <PARTYLEDGERNAME>ABC Vendors Pvt Ltd</PARTYLEDGERNAME>
      <PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>ABC Vendors Pvt Ltd</LEDGERNAME>
        <AMOUNT>11800.00</AMOUNT>
        <BILLALLOCATIONS.LIST>
          <NAME>PI-5501</NAME>
          <BILLTYPE>Agst Ref</BILLTYPE>
          <AMOUNT>11800.00</AMOUNT>
        </BILLALLOCATIONS.LIST>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>Purchases A/c</LEDGERNAME>
        <AMOUNT>-10000.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>IGST @ 18%</LEDGERNAME>
        <GSTCLASS>IGST@18</GSTCLASS>
        <AMOUNT>-1800.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
    </VOUCHER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

	tly := &fakeTally{resp: []byte(journalResponse)}
	snd := &fakeSender{}
	req := makeReq()
	req.KindName = "journal"
	req.TallyKind = tally.VoucherJournal
	req.IngestKind = ""

	res, err := RunOne(context.Background(), tly, snd, req, nil)
	if err != nil {
		t.Fatalf("RunOne journal: %v", err)
	}
	if len(snd.journalSent) != 1 || res.BatchesSent != 1 {
		t.Fatalf("journal sends=%d batchesSent=%d, want 1", len(snd.journalSent), res.BatchesSent)
	}
	if len(snd.sent) != 0 {
		t.Fatalf("invoice sends=%d, want 0", len(snd.sent))
	}
	if got := snd.journalSent[0].Batch[0].VoucherNumber; got != "JV-001" {
		t.Errorf("VoucherNumber=%q, want JV-001", got)
	}
	if got := snd.journalSent[0].Batch[0].AgainstInvoiceRefs[0].Reference; got != "PI-5501" {
		t.Errorf("AgainstInvoiceRefs[0]=%q, want PI-5501", got)
	}
	if !snd.journalSent[0].IsFinal {
		t.Error("journal single-batch run should mark IsFinal=true")
	}
	if snd.journalSent[0].PeriodFrom == nil || *snd.journalSent[0].PeriodFrom != "2026-04-01" {
		t.Errorf("journal PeriodFrom=%v, want 2026-04-01", snd.journalSent[0].PeriodFrom)
	}
	if snd.journalSent[0].PeriodTo == nil || *snd.journalSent[0].PeriodTo != "2026-04-30" {
		t.Errorf("journal PeriodTo=%v, want 2026-04-30", snd.journalSent[0].PeriodTo)
	}
	if res.RowCount != 1 {
		t.Errorf("RowCount=%d, want 1", res.RowCount)
	}
}

func TestRunOne_PaymentPathPostsOneBatch(t *testing.T) {
	const paymentResponse = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260418</DATE>
      <GUID>p-001</GUID>
      <ALTERID>85</ALTERID>
      <VOUCHERTYPENAME>Bank Payment</VOUCHERTYPENAME>
      <VOUCHERNUMBER>BP-001</VOUCHERNUMBER>
      <PARTYLEDGERNAME>ABC Vendors Pvt Ltd</PARTYLEDGERNAME>
      <PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>ABC Vendors Pvt Ltd</LEDGERNAME>
        <AMOUNT>-11800.00</AMOUNT>
        <BILLALLOCATIONS.LIST>
          <NAME>PI-5501</NAME>
          <BILLTYPE>Agst Ref</BILLTYPE>
          <AMOUNT>-11800.00</AMOUNT>
        </BILLALLOCATIONS.LIST>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>HDFC Bank</LEDGERNAME>
        <AMOUNT>11800.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
    </VOUCHER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

	tly := &fakeTally{resp: []byte(paymentResponse)}
	snd := &fakeSender{}
	req := makeReq()
	req.KindName = "payment"
	req.TallyKind = tally.VoucherPayment
	req.IngestKind = ""

	res, err := RunOne(context.Background(), tly, snd, req, nil)
	if err != nil {
		t.Fatalf("RunOne payment: %v", err)
	}
	if len(snd.paymentSent) != 1 || res.BatchesSent != 1 {
		t.Fatalf("payment sends=%d batchesSent=%d, want 1", len(snd.paymentSent), res.BatchesSent)
	}
	if len(snd.sent) != 0 {
		t.Fatalf("invoice sends=%d, want 0", len(snd.sent))
	}
	row := snd.paymentSent[0].Batch[0]
	if row.VoucherNumber != "BP-001" {
		t.Errorf("VoucherNumber=%q, want BP-001", row.VoucherNumber)
	}
	if row.InstrumentType == nil || *row.InstrumentType != "bank" {
		t.Errorf("InstrumentType=%v, want bank", row.InstrumentType)
	}
	if !snd.paymentSent[0].IsFinal {
		t.Error("payment single-batch run should mark IsFinal=true")
	}
	if snd.paymentSent[0].PeriodFrom == nil || *snd.paymentSent[0].PeriodFrom != "2026-04-01" {
		t.Errorf("payment PeriodFrom=%v, want 2026-04-01", snd.paymentSent[0].PeriodFrom)
	}
	if snd.paymentSent[0].PeriodTo == nil || *snd.paymentSent[0].PeriodTo != "2026-04-30" {
		t.Errorf("payment PeriodTo=%v, want 2026-04-30", snd.paymentSent[0].PeriodTo)
	}
	if res.RowCount != 1 {
		t.Errorf("RowCount=%d, want 1", res.RowCount)
	}
}

func TestRunOne_TaxLedgerPathPostsOneBatch(t *testing.T) {
	const taxLedgerResponse = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <LEDGER>
      <NAME>Input IGST</NAME>
      <PARENT>Duties &amp; Taxes</PARENT>
      <PERIODOPENINGBALANCE>100.00</PERIODOPENINGBALANCE>
      <PERIODDEBITTOTALS>1200.00</PERIODDEBITTOTALS>
      <PERIODCREDITTOTALS>300.00</PERIODCREDITTOTALS>
      <PERIODCLOSINGBALANCE>1000.00</PERIODCLOSINGBALANCE>
    </LEDGER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

	tly := &fakeTally{resp: []byte(taxLedgerResponse)}
	snd := &fakeSender{}
	req := makeReq()
	req.KindName = "tax_ledger"
	req.TallyKind = ""
	req.IngestKind = ""

	res, err := RunOne(context.Background(), tly, snd, req, nil)
	if err != nil {
		t.Fatalf("RunOne tax_ledger: %v", err)
	}
	if len(snd.taxLedgerSent) != 1 || res.BatchesSent != 1 {
		t.Fatalf("tax-ledger sends=%d batchesSent=%d, want 1", len(snd.taxLedgerSent), res.BatchesSent)
	}
	if len(snd.sent) != 0 {
		t.Fatalf("invoice sends=%d, want 0", len(snd.sent))
	}
	row := snd.taxLedgerSent[0].Batch[0]
	if row.Period != "042026" {
		t.Errorf("Period=%q, want 042026", row.Period)
	}
	if row.LedgerName != "Input IGST" {
		t.Errorf("LedgerName=%q, want Input IGST", row.LedgerName)
	}
	if row.DebitAmount != 1200 {
		t.Errorf("DebitAmount=%v, want 1200", row.DebitAmount)
	}
	if !snd.taxLedgerSent[0].IsFinal {
		t.Error("tax-ledger single-batch run should mark IsFinal=true")
	}
	if snd.taxLedgerSent[0].PeriodFrom == nil || *snd.taxLedgerSent[0].PeriodFrom != "2026-04-01" {
		t.Errorf("tax-ledger PeriodFrom=%v, want 2026-04-01", snd.taxLedgerSent[0].PeriodFrom)
	}
	if snd.taxLedgerSent[0].PeriodTo == nil || *snd.taxLedgerSent[0].PeriodTo != "2026-04-30" {
		t.Errorf("tax-ledger PeriodTo=%v, want 2026-04-30", snd.taxLedgerSent[0].PeriodTo)
	}
}

func TestRunOne_CollectsServerCountersAndFindings(t *testing.T) {
	tly := &fakeTally{resp: []byte(stubResponse)}
	snd := &fakeSender{
		resp: ingest.AcceptedResponse{
			Counters: ingest.AcceptedCounters{
				Inserted: 0,
				Skipped:  1,
			},
			Findings: ingest.AcceptedFindings{
				MissingGSTIN: []ingest.ValidationFinding{
					{
						RowIndex:    0,
						ErrorType:   "invalid_gstin",
						ErrorDetail: "Vendor GSTIN is missing.",
						Severity:    "error",
					},
				},
			},
		},
	}

	res, err := RunOne(context.Background(), tly, snd, makeReq(), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.ServerSkipped != 1 || res.ServerInserted != 0 {
		t.Fatalf("server counters = %+v", res)
	}
	if len(res.ServerFindings) != 1 {
		t.Fatalf("ServerFindings=%v, want 1 finding", res.ServerFindings)
	}
	if got := res.ServerFindings[0].Scope; got != "missing_gstin" {
		t.Fatalf("finding scope=%q, want missing_gstin", got)
	}
}

func TestRunOne_TallyFetchFailureIsFatal(t *testing.T) {
	tly := &fakeTally{err: errors.New("tally down")}
	snd := &fakeSender{}

	_, err := RunOne(context.Background(), tly, snd, makeReq(), nil)
	if err == nil {
		t.Fatal("expected fetch error to be returned")
	}
	if len(snd.sent) != 0 {
		t.Errorf("nothing should be sent when fetch fails; got %d", len(snd.sent))
	}
}

func TestRunOne_RequiresRunID(t *testing.T) {
	req := makeReq()
	req.RunID = ""
	_, err := RunOne(context.Background(), &fakeTally{}, &fakeSender{}, req, nil)
	if err == nil {
		t.Fatal("expected error for empty RunID")
	}
}

func TestRunOne_ProgressCallbackEmitsExpectedStages(t *testing.T) {
	tly := &fakeTally{resp: []byte(stubResponse)}
	snd := &fakeSender{}

	var stages []string
	progress := func(e Event) { stages = append(stages, e.Stage) }

	if _, err := RunOne(context.Background(), tly, snd, makeReq(), progress); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	want := []string{"fetching", "parsed", "normalized", "batch_sending", "batch_sent", "complete"}
	if !equalSlice(stages, want) {
		t.Errorf("stages=%v, want %v", stages, want)
	}
}

func TestRunOne_AbortsCleanlyOnCtxCancellation(t *testing.T) {
	// Stub response with 3 vouchers so multiple batches fire (with
	// BatchSize=1). cancellingSender succeeds the first send then
	// cancels the parent ctx so the next loop iteration sees
	// ctx.Done() and breaks cleanly — no false "network error" for
	// the remaining batches.
	const threeVouchers = `<ENVELOPE>
      <HEADER><STATUS>1</STATUS></HEADER>
      <BODY><DATA><COLLECTION>
        <VOUCHER><DATE>20260410</DATE><GUID>g-1</GUID><VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME><VOUCHERNUMBER>P1</VOUCHERNUMBER><PARTYLEDGERNAME>V Ltd</PARTYLEDGERNAME><PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN><ISINVOICE>Yes</ISINVOICE><ISCANCELLED>No</ISCANCELLED>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>V Ltd</LEDGERNAME><AMOUNT>11800</AMOUNT></ALLLEDGERENTRIES.LIST>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>P A/c</LEDGERNAME><AMOUNT>-10000</AMOUNT></ALLLEDGERENTRIES.LIST>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>IGST 18</LEDGERNAME><GSTCLASS>IGST@18</GSTCLASS><AMOUNT>-1800</AMOUNT></ALLLEDGERENTRIES.LIST>
        </VOUCHER>
        <VOUCHER><DATE>20260411</DATE><GUID>g-2</GUID><VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME><VOUCHERNUMBER>P2</VOUCHERNUMBER><PARTYLEDGERNAME>V Ltd</PARTYLEDGERNAME><PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN><ISINVOICE>Yes</ISINVOICE><ISCANCELLED>No</ISCANCELLED>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>V Ltd</LEDGERNAME><AMOUNT>11800</AMOUNT></ALLLEDGERENTRIES.LIST>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>P A/c</LEDGERNAME><AMOUNT>-10000</AMOUNT></ALLLEDGERENTRIES.LIST>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>IGST 18</LEDGERNAME><GSTCLASS>IGST@18</GSTCLASS><AMOUNT>-1800</AMOUNT></ALLLEDGERENTRIES.LIST>
        </VOUCHER>
        <VOUCHER><DATE>20260412</DATE><GUID>g-3</GUID><VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME><VOUCHERNUMBER>P3</VOUCHERNUMBER><PARTYLEDGERNAME>V Ltd</PARTYLEDGERNAME><PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN><ISINVOICE>Yes</ISINVOICE><ISCANCELLED>No</ISCANCELLED>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>V Ltd</LEDGERNAME><AMOUNT>11800</AMOUNT></ALLLEDGERENTRIES.LIST>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>P A/c</LEDGERNAME><AMOUNT>-10000</AMOUNT></ALLLEDGERENTRIES.LIST>
          <ALLLEDGERENTRIES.LIST><LEDGERNAME>IGST 18</LEDGERNAME><GSTCLASS>IGST@18</GSTCLASS><AMOUNT>-1800</AMOUNT></ALLLEDGERENTRIES.LIST>
        </VOUCHER>
      </COLLECTION></DATA></BODY>
    </ENVELOPE>`
	tly := &fakeTally{resp: []byte(threeVouchers)}
	ctx, cancel := context.WithCancel(context.Background())
	snd := &cancellingSender{cancel: cancel}

	req := makeReq()
	req.BatchSize = 1
	res, err := RunOne(ctx, tly, snd, req, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if res.BatchesSent != 1 {
		t.Errorf("BatchesSent = %d, want 1", res.BatchesSent)
	}
	if res.BatchesFailed != 0 {
		t.Errorf("BatchesFailed = %d, want 0 (clean abort, no false network errors)", res.BatchesFailed)
	}
	if len(res.BatchErrors) != 0 {
		t.Errorf("BatchErrors = %d, want 0", len(res.BatchErrors))
	}
}

// cancellingSender succeeds the first Send, then cancels the parent
// ctx so the next loop iteration sees ctx.Done() and breaks cleanly.
// Used to simulate a user Ctrl+C between batches.
type cancellingSender struct {
	cancel context.CancelFunc
	calls  int
}

func (c *cancellingSender) Send(_ context.Context, _ tally.IngestRequestBody) (ingest.AcceptedResponse, error) {
	c.calls++
	if c.calls == 1 {
		c.cancel()
		return ingest.AcceptedResponse{Counters: ingest.AcceptedCounters{Inserted: 1}}, nil
	}
	return ingest.AcceptedResponse{}, errors.New("cancellingSender: should not be called after ctx cancel")
}

func (c *cancellingSender) SendJournal(_ context.Context, _ tally.JournalIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	return tally.AccountingAcceptedResponse{}, errors.New("cancellingSender: unexpected SendJournal call")
}

func (c *cancellingSender) SendPayment(_ context.Context, _ tally.PaymentIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	return tally.AccountingAcceptedResponse{}, errors.New("cancellingSender: unexpected SendPayment call")
}

func (c *cancellingSender) SendTaxLedgers(_ context.Context, _ tally.TaxLedgerIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	return tally.AccountingAcceptedResponse{}, errors.New("cancellingSender: unexpected SendTaxLedgers call")
}

func TestRunOne_NoVouchersStillEmitsCompleteEvent(t *testing.T) {
	tly := &fakeTally{resp: []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA></DATA></BODY></ENVELOPE>`)}
	snd := &fakeSender{}

	var stages []string
	res, err := RunOne(context.Background(), tly, snd, makeReq(), func(e Event) {
		stages = append(stages, e.Stage)
	})
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.RowCount != 0 {
		t.Errorf("RowCount=%d", res.RowCount)
	}
	hasComplete := false
	for _, s := range stages {
		if s == "complete" {
			hasComplete = true
			break
		}
	}
	if !hasComplete {
		t.Errorf("expected 'complete' event even on empty result; stages=%v", stages)
	}
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
