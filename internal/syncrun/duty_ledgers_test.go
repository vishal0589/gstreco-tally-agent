package syncrun

import (
	"context"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// SYNTHETIC test data, not client data: a company's Duties & Taxes masters
// and a purchase whose tax ledger has no GST class, so only the masters can
// make it GST (QA v18 R3).
const r3DutyLedgers = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <LEDGER NAME="IGST ITC @ 18%"><NAME>IGST ITC @ 18%</NAME><PARENT>GST Input</PARENT><TAXTYPE>GST</TAXTYPE><GSTDUTYHEAD>Integrated Tax</GSTDUTYHEAD></LEDGER>
  <LEDGER NAME="TDS on Contract 194C"><NAME>TDS on Contract 194C</NAME><PARENT>TDS Payable</PARENT><TAXTYPE>TDS</TAXTYPE><GSTDUTYHEAD></GSTDUTYHEAD></LEDGER>
</COLLECTION></DATA></BODY></ENVELOPE>`

const r3PurchaseWithTDS = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260420</DATE>
      <GUID>syn-r3-sync-1</GUID>
      <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
      <VOUCHERNUMBER>SYN-R3-S1</VOUCHERNUMBER>
      <PARTYLEDGERNAME>Synthetic Stalls (SYNTHETIC)</PARTYLEDGERNAME>
      <PARTYGSTIN>27AAAAA0000A1Z5</PARTYGSTIN>
      <ISINVOICE>No</ISINVOICE>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>Synthetic Stalls (SYNTHETIC)</LEDGERNAME>
        <AMOUNT>479700.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>Event Expense IGST @18%</LEDGERNAME>
        <AMOUNT>-410000.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>IGST ITC @ 18%</LEDGERNAME>
        <AMOUNT>-73800.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>TDS on Contract 194C</LEDGERNAME>
        <AMOUNT>4100.00</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
    </VOUCHER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

func onlyRow(t *testing.T, snd *fakeSender, run int) tally.IngestVoucherRow {
	t.Helper()
	if len(snd.sent) <= run || len(snd.sent[run].Batch) != 1 {
		t.Fatalf("run %d: sent %d batches", run, len(snd.sent))
	}
	return snd.sent[run].Batch[0]
}

func TestRunOne_DutyLedgerMastersReachTheNormalizer(t *testing.T) {
	resetDutyLedgerCache()
	defer resetDutyLedgerCache()

	dutyRequests := 0
	tly := &fakeTally{respond: func(body []byte) []byte {
		if isDutyLedgerRequest(body) {
			dutyRequests++
			if !strings.Contains(string(body), "<SVCURRENTCOMPANY>PLLUM CASA</SVCURRENTCOMPANY>") {
				t.Errorf("duty-ledger request for the wrong company: %s", body)
			}
			return []byte(r3DutyLedgers)
		}
		return []byte(r3PurchaseWithTDS)
	}}
	snd := &fakeSender{}
	for i := 0; i < 2; i++ {
		res, err := RunOne(context.Background(), tly, snd, makeReq(), nil)
		if err != nil {
			t.Fatalf("RunOne %d: %v", i, err)
		}
		for _, w := range res.ParseWarnings {
			if strings.Contains(w, "duty ledger masters") {
				t.Errorf("run %d warned: %s", i, w)
			}
		}
	}
	// One master read serves every invoice lane of the company in the TTL.
	if dutyRequests != 1 {
		t.Errorf("duty-ledger requests = %d, want 1", dutyRequests)
	}

	row := onlyRow(t, snd, 1)
	if row.IGST != 73800 || row.TaxableValue != 410000 || row.InvoiceValue != 483800 {
		t.Errorf("row = invoice %v taxable %v igst %v, want 483800 / 410000 / 73800", row.InvoiceValue, row.TaxableValue, row.IGST)
	}
	if row.TDSAmount == nil || *row.TDSAmount != 4100 || row.NetPayable == nil || *row.NetPayable != 479700 {
		t.Errorf("tds %v / net %v, want 4100 / 479700", row.TDSAmount, row.NetPayable)
	}
	var taxLeg *tally.IngestVoucherLeg
	for i := range row.VoucherLegs {
		if row.VoucherLegs[i].LedgerName == "IGST ITC @ 18%" {
			taxLeg = &row.VoucherLegs[i]
		}
	}
	if taxLeg == nil || taxLeg.Classification.Evidence != tally.EvidenceDutyMaster || taxLeg.ParentGroup == nil || *taxLeg.ParentGroup != "GST Input" {
		t.Fatalf("tax leg = %+v, want its Duties & Taxes master as evidence", taxLeg)
	}
}

func TestRunOne_DutyLedgerFetchFailureLeavesNamedTaxUncertain(t *testing.T) {
	resetDutyLedgerCache()
	defer resetDutyLedgerCache()

	healthy := false
	tly := &fakeTally{respond: func(body []byte) []byte {
		if isDutyLedgerRequest(body) {
			if !healthy {
				return []byte(`<ENVELOPE><HEADER><STATUS>0</STATUS></HEADER><BODY><DATA>Company not open</DATA></BODY></ENVELOPE>`)
			}
			return []byte(r3DutyLedgers)
		}
		return []byte(r3PurchaseWithTDS)
	}}

	first := &fakeSender{}
	res, err := RunOne(context.Background(), tly, first, makeReq(), nil)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(res.ParseWarnings) == 0 || !strings.Contains(res.ParseWarnings[len(res.ParseWarnings)-1], "duty ledger masters unavailable") {
		t.Errorf("warnings = %v, want the failed master read recorded", res.ParseWarnings)
	}
	row := onlyRow(t, first, 0)
	// No evidence: the GST-named legs are uncertain, out of GST and taxable
	// value, still in the gross; the sync is not dropped.
	if row.IGST != 0 || row.TaxableValue != 0 || row.InvoiceValue != 483800 {
		t.Errorf("row = invoice %v taxable %v igst %v, want 483800 / 0 / 0", row.InvoiceValue, row.TaxableValue, row.IGST)
	}
	flagged := false
	for _, f := range row.NormalizationFlags {
		flagged = flagged || strings.Contains(f, "no Duties & Taxes ledger masters")
	}
	if !flagged {
		t.Errorf("flags = %v", row.NormalizationFlags)
	}

	// The failed reply was not cached.
	healthy = true
	second := &fakeSender{}
	if _, err := RunOne(context.Background(), tly, second, makeReq(), nil); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if row := onlyRow(t, second, 0); row.IGST != 73800 {
		t.Errorf("second run IGST = %v, want 73800 from the masters", row.IGST)
	}
}

// SYNTHETIC ledger and group masters for r3PurchaseWithTDS. Tally exports a
// top-level group's parent as "&#4; Primary".
const r3LedgerMasters = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <LEDGER NAME="Synthetic Stalls (SYNTHETIC)"><NAME>Synthetic Stalls (SYNTHETIC)</NAME><PARENT>Sundry Creditors</PARENT><PARTYGSTIN>27AAAAA0000A1Z5</PARTYGSTIN></LEDGER>
  <LEDGER NAME="Event Expense IGST @18%"><NAME>Event Expense IGST @18%</NAME><PARENT>Event Expenses</PARENT></LEDGER>
</COLLECTION></DATA></BODY></ENVELOPE>`

const r3GroupTree = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <GROUP NAME="Event Expenses"><NAME>Event Expenses</NAME><PARENT>Indirect Expenses</PARENT></GROUP>
  <GROUP NAME="Indirect Expenses"><NAME>Indirect Expenses</NAME><PARENT>&#4; Primary</PARENT></GROUP>
  <GROUP NAME="Sundry Creditors"><NAME>Sundry Creditors</NAME><PARENT>Current Liabilities</PARENT></GROUP>
  <GROUP NAME="Current Liabilities"><NAME>Current Liabilities</NAME><PARENT>&#4; Primary</PARENT></GROUP>
</COLLECTION></DATA></BODY></ENVELOPE>`

// The voucher carries its party GSTIN, so the ledger directory is not needed
// for the GSTIN. It is still read: every leg goes to GST Reco with its Tally
// group, so the server classes it by group instead of logging it as
// "classified by name - needs confirmation" for the filing gate.
func TestRunOne_InvoiceLegsCarryTheirTallyGroups(t *testing.T) {
	resetDutyLedgerCache()
	resetLedgerGSTINCache()
	defer resetDutyLedgerCache()
	defer resetLedgerGSTINCache()

	requests := map[string]int{}
	tly := &fakeTally{respond: func(body []byte) []byte {
		switch b := string(body); {
		case isDutyLedgerRequest(body):
			requests["duty"]++
			return []byte(r3DutyLedgers)
		case strings.Contains(b, "GstrecoLedgerGstinCollection"):
			requests["ledgers"]++
			return []byte(r3LedgerMasters)
		case strings.Contains(b, "GstrecoGroupTreeCollection"):
			requests["groups"]++
			return []byte(r3GroupTree)
		}
		requests["vouchers"]++
		return []byte(r3PurchaseWithTDS)
	}}
	snd := &fakeSender{}
	if _, err := RunOne(context.Background(), tly, snd, makeReq(), nil); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	for _, kind := range []string{"vouchers", "duty", "ledgers", "groups"} {
		if requests[kind] != 1 {
			t.Errorf("%s requests = %d, want 1 (%v)", kind, requests[kind], requests)
		}
	}

	row := onlyRow(t, snd, 0)
	groups := map[string][2]string{}
	for _, leg := range row.VoucherLegs {
		var parent, primary string
		if leg.ParentGroup != nil {
			parent = *leg.ParentGroup
		}
		if leg.PrimaryGroup != nil {
			primary = *leg.PrimaryGroup
		}
		groups[leg.LedgerName] = [2]string{parent, primary}
	}
	for name, want := range map[string][2]string{
		"Synthetic Stalls (SYNTHETIC)": {"Sundry Creditors", "Current Liabilities"},
		"Event Expense IGST @18%":      {"Event Expenses", "Indirect Expenses"},
		"IGST ITC @ 18%":               {"GST Input", "Duties & Taxes"},
		"TDS on Contract 194C":         {"TDS Payable", "Duties & Taxes"},
	} {
		if groups[name] != want {
			t.Errorf("%s groups = %v, want %v", name, groups[name], want)
		}
	}
	if row.VendorGSTIN == nil || *row.VendorGSTIN != "27AAAAA0000A1Z5" {
		t.Errorf("vendor GSTIN = %v, want the voucher's own", row.VendorGSTIN)
	}
}
