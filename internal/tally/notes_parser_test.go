package tally

import (
	"testing"
	"time"
)

func TestParseDayBookV3_CreditNoteSales(t *testing.T) {
	parsed, err := ParseDayBookV3(mustFixture(t, "voucher-v3-credit-note-sales.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(parsed.Vouchers) != 1 {
		t.Fatalf("vouchers = %d, want 1", len(parsed.Vouchers))
	}
	v := parsed.Vouchers[0]

	if v.VoucherType != "Credit Note" {
		t.Errorf("VoucherType = %q", v.VoucherType)
	}
	if v.VoucherNumber != "CN/007/26-27" {
		t.Errorf("VoucherNumber = %q", v.VoucherNumber)
	}
	if v.Reference != "INV/0042/26-27" {
		t.Errorf("Reference (original invoice) = %q", v.Reference)
	}
	// Sales-side credit note: party (customer) amount is negative per Tally
	// double-entry. The reference points at the ORIGINAL sales invoice —
	// the server uses this to link the note back to the invoice in reco.
	wantDate := time.Date(2026, 4, 22, 0, 0, 0, 0, time.UTC)
	if !v.Date.Equal(wantDate) {
		t.Errorf("Date = %s, want %s", v.Date, wantDate)
	}
	if v.PartyGSTIN != "29XYZAB5678K2L1" {
		t.Errorf("PartyGSTIN = %q", v.PartyGSTIN)
	}
	if len(v.LedgerEntries) != 4 {
		t.Fatalf("LedgerEntries = %d, want 4", len(v.LedgerEntries))
	}

	// Party ledger is the first entry, signed negative (sales-side credit
	// note: customer is being refunded so their receivable decreases →
	// credit to Sundry Debtors ledger → negative in Tally's sign model).
	party := v.LedgerEntries[0]
	if !party.IsPartyLedger {
		t.Error("first entry should be party ledger")
	}
	if party.Amount != -11800 {
		t.Errorf("party Amount = %v, want -11800", party.Amount)
	}
	if len(party.BillAllocations) != 1 {
		t.Fatalf("BillAllocations = %d", len(party.BillAllocations))
	}
	if party.BillAllocations[0].BillType != "Agst Ref" {
		// Credit notes typically use Agst Ref to link to the original
		// invoice. The normaliser treats New Ref + Agst Ref as
		// invoice-producing allocations (unlike Advance / On Account).
		t.Errorf("BillType = %q, want Agst Ref", party.BillAllocations[0].BillType)
	}

	// CGST + SGST (intrastate) both classified via GSTClass attribute.
	var cgstFound, sgstFound bool
	for _, le := range v.LedgerEntries[1:] {
		if le.GSTClass == "CGST@9" && le.Amount == 900 {
			cgstFound = true
		}
		if le.GSTClass == "SGST@9" && le.Amount == 900 {
			sgstFound = true
		}
	}
	if !cgstFound || !sgstFound {
		t.Error("CGST/SGST entries not found or amounts wrong")
	}
}

func TestParseDayBookV3_DebitNotePurchase(t *testing.T) {
	parsed, err := ParseDayBookV3(mustFixture(t, "voucher-v3-debit-note-purchase.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(parsed.Vouchers) != 1 {
		t.Fatalf("vouchers = %d", len(parsed.Vouchers))
	}
	v := parsed.Vouchers[0]

	if v.VoucherType != "Debit Note" {
		t.Errorf("VoucherType = %q", v.VoucherType)
	}
	if v.Reference != "SUPP-INV-5501" {
		t.Errorf("Reference = %q, want vendor invoice no.", v.Reference)
	}
	// Purchase-side debit note: party (vendor) has positive amount (their
	// payable decreases → debit to Sundry Creditors → positive in Tally).
	party := v.LedgerEntries[0]
	if !party.IsPartyLedger || party.Amount != 23600 {
		t.Errorf("party ledger wrong: %+v", party)
	}

	// IGST (interstate) classified via GSTClass. Note: signed negative on
	// purchase-side debit note because the tax is being reversed out.
	var igstFound bool
	for _, le := range v.LedgerEntries[1:] {
		if le.GSTClass == "IGST@18" && le.Amount == -3600 {
			igstFound = true
		}
	}
	if !igstFound {
		t.Error("IGST entry not found or amount wrong")
	}
}
