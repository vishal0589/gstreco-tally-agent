package syncrun

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	resp []byte
	err  error
	got  []byte
}

func (f *fakeTally) PostXML(_ context.Context, body []byte) ([]byte, error) {
	f.got = body
	return f.resp, f.err
}

type fakeSender struct {
	sent []tally.IngestRequestBody
	err  error
}

func (f *fakeSender) Send(_ context.Context, body tally.IngestRequestBody) error {
	f.sent = append(f.sent, body)
	return f.err
}

func makeReq() Request {
	return Request{
		TallyCompany: "PLLUM CASA",
		TallyKind:    tally.VoucherPurchase,
		IngestKind:   tally.IngestKindPurchase,
		From:         time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:           time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		RunID:        "test-run-1",
		RunKind:      "manual",
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
