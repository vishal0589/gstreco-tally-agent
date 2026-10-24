package syncrun

import (
	"context"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

const journalWithoutPartyGSTIN = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260401</DATE>
      <GUID>j-145</GUID>
      <VOUCHERTYPENAME>Journal</VOUCHERTYPENAME>
      <VOUCHERNUMBER>CI26270000000145</VOUCHERNUMBER>
      <PARTYLEDGERNAME>Delhi Cargo Service Center Pvt.Ltd-ADVANCE</PARTYLEDGERNAME>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>Delhi Cargo Service Center Pvt.Ltd-ADVANCE</LEDGERNAME>
        <AMOUNT>2509.80</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>Terminal Handling Charges</LEDGERNAME>
        <AMOUNT>-2127.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>CGST Input Credit</LEDGERNAME>
        <AMOUNT>-191.40</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>SGST Input Credit</LEDGERNAME>
        <AMOUNT>-191.40</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
    </VOUCHER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

const ledgerGSTINs = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <LEDGER NAME="Delhi Cargo Service Center Pvt.Ltd-ADVANCE">
    <NAME>Delhi Cargo Service Center Pvt.Ltd-ADVANCE</NAME>
    <PARTYGSTIN>07ZZYAB1234C1Z5</PARTYGSTIN>
  </LEDGER>
</COLLECTION></DATA></BODY></ENVELOPE>`

func TestRunOne_JournalWithoutVoucherGSTINGetsTheLedgerGSTIN(t *testing.T) {
	resetLedgerGSTINCache()
	defer resetLedgerGSTINCache()

	ledgerRequests := 0
	tly := &fakeTally{respond: func(body []byte) []byte {
		if strings.Contains(string(body), "GstrecoLedgerGstinCollection") {
			ledgerRequests++
			return []byte(ledgerGSTINs)
		}
		return []byte(journalWithoutPartyGSTIN)
	}}
	snd := &fakeSender{}

	for _, kind := range []struct {
		name  string
		tally tally.VoucherKind
	}{{"journal", tally.VoucherJournal}, {"payment", tally.VoucherPayment}} {
		req := makeReq()
		req.KindName = kind.name
		req.TallyKind = kind.tally
		req.IngestKind = ""
		if _, err := RunOne(context.Background(), tly, snd, req, nil); err != nil {
			t.Fatalf("RunOne %s: %v", kind.name, err)
		}
	}

	if len(snd.journalSent) != 1 {
		t.Fatalf("journal sends = %d, want 1", len(snd.journalSent))
	}
	got := snd.journalSent[0].Batch[0].PartyGSTIN
	if got == nil || *got != "07ZZYAB1234C1Z5" {
		t.Fatalf("journal PartyGSTIN = %v, want the ledger's GSTIN", got)
	}
	if len(snd.paymentSent) != 1 || snd.paymentSent[0].Batch[0].PartyGSTIN == nil {
		t.Fatalf("payment row missing the ledger GSTIN")
	}
	// One ledger request serves every lane of the company within the TTL.
	if ledgerRequests != 1 {
		t.Errorf("ledger requests = %d, want 1", ledgerRequests)
	}
}

func TestRunOne_LedgerLookupFailureDoesNotFailTheRun(t *testing.T) {
	resetLedgerGSTINCache()
	defer resetLedgerGSTINCache()

	tly := &fakeTally{respond: func(body []byte) []byte {
		if strings.Contains(string(body), "GstrecoLedgerGstinCollection") {
			return []byte("   ")
		}
		return []byte(journalWithoutPartyGSTIN)
	}}
	snd := &fakeSender{}
	req := makeReq()
	req.KindName = "journal"
	req.TallyKind = tally.VoucherJournal
	req.IngestKind = ""

	res, err := RunOne(context.Background(), tly, snd, req, nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if res.BatchesSent != 1 || snd.journalSent[0].Batch[0].PartyGSTIN != nil {
		t.Fatalf("want the journal sent without a GSTIN; sent=%d", res.BatchesSent)
	}
	if len(res.ParseWarnings) == 0 || !strings.Contains(res.ParseWarnings[len(res.ParseWarnings)-1], "ledger GSTIN lookup failed") {
		t.Errorf("warnings = %v, want the lookup failure recorded", res.ParseWarnings)
	}
}

func TestRunOne_FailedLedgerReplyIsNotCached(t *testing.T) {
	resetLedgerGSTINCache()
	defer resetLedgerGSTINCache()

	healthy := false
	tly := &fakeTally{respond: func(body []byte) []byte {
		if strings.Contains(string(body), "GstrecoLedgerGstinCollection") {
			if !healthy {
				return []byte(`<ENVELOPE><HEADER><STATUS>0</STATUS></HEADER><BODY><DATA>Company not open</DATA></BODY></ENVELOPE>`)
			}
			return []byte(ledgerGSTINs)
		}
		return []byte(journalWithoutPartyGSTIN)
	}}
	req := makeReq()
	req.KindName = "journal"
	req.TallyKind = tally.VoucherJournal
	req.IngestKind = ""

	first := &fakeSender{}
	if _, err := RunOne(context.Background(), tly, first, req, nil); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if first.journalSent[0].Batch[0].PartyGSTIN != nil {
		t.Fatal("first run: Tally replied STATUS 0, so no GSTIN expected")
	}

	healthy = true
	second := &fakeSender{}
	if _, err := RunOne(context.Background(), tly, second, req, nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if got := second.journalSent[0].Batch[0].PartyGSTIN; got == nil || *got != "07ZZYAB1234C1Z5" {
		t.Fatalf("second run PartyGSTIN = %v: the failed reply must not have been cached", got)
	}
}
