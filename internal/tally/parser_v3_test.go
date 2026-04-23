package tally

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mustFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

func TestParseDayBookV3_PurchaseSingle(t *testing.T) {
	got, err := ParseDayBookV3(mustFixture(t, "voucher-v3-purchase-single.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if got.Status != 1 {
		t.Errorf("Status = %d, want 1", got.Status)
	}
	if len(got.Vouchers) != 1 {
		t.Fatalf("Vouchers = %d, want 1", len(got.Vouchers))
	}
	if len(got.Warnings) > 0 {
		t.Errorf("unexpected warnings: %v", got.Warnings)
	}

	v := got.Vouchers[0]
	if v.GUID != "fixture-purchase-single-0001" {
		t.Errorf("GUID = %q", v.GUID)
	}
	if v.AlterID != 12345 {
		t.Errorf("AlterID = %d, want 12345", v.AlterID)
	}
	wantDate := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	if !v.Date.Equal(wantDate) {
		t.Errorf("Date = %s, want %s", v.Date, wantDate)
	}
	if v.VoucherNumber != "PI/001/26-27" {
		t.Errorf("VoucherNumber = %q", v.VoucherNumber)
	}
	if v.Reference != "SUPP-INV-5501" {
		t.Errorf("Reference = %q", v.Reference)
	}
	if v.PartyLedgerName != "ABC Vendors Ltd" {
		t.Errorf("PartyLedgerName = %q", v.PartyLedgerName)
	}
	if v.PartyGSTIN != "29ABCDE1234F1Z5" {
		t.Errorf("PartyGSTIN = %q", v.PartyGSTIN)
	}
	if !v.IsInvoice {
		t.Error("IsInvoice = false, want true")
	}
	if v.ReverseCharge {
		t.Error("ReverseCharge = true, want false")
	}
	if len(v.LedgerEntries) != 3 {
		t.Fatalf("LedgerEntries = %d, want 3", len(v.LedgerEntries))
	}

	party := v.LedgerEntries[0]
	if !party.IsPartyLedger {
		t.Error("party ledger entry IsPartyLedger = false")
	}
	if party.Amount != -118000 {
		t.Errorf("party Amount = %f, want -118000", party.Amount)
	}
	if len(party.BillAllocations) != 1 {
		t.Fatalf("party BillAllocations = %d, want 1", len(party.BillAllocations))
	}
	if party.BillAllocations[0].Name != "SUPP-INV-5501" {
		t.Errorf("bill name = %q", party.BillAllocations[0].Name)
	}

	igst := v.LedgerEntries[2]
	if igst.LedgerName != "IGST @ 18%" {
		t.Errorf("igst LedgerName = %q", igst.LedgerName)
	}
	if igst.Amount != 18000 {
		t.Errorf("igst Amount = %f, want 18000", igst.Amount)
	}
	if igst.GSTClass != "IGST@18" {
		t.Errorf("igst GSTClass = %q", igst.GSTClass)
	}

	if len(v.InventoryEntries) != 1 {
		t.Fatalf("InventoryEntries = %d, want 1", len(v.InventoryEntries))
	}
	inv := v.InventoryEntries[0]
	if inv.StockItem != "A4 Paper Ream" || inv.Quantity != 200 || inv.Unit != "NOS" || inv.Rate != 500 || inv.HSN != "4802" {
		t.Errorf("InventoryEntry mismatch: %+v", inv)
	}
}

func TestParseDayBookV3_SalesCGSTSGST(t *testing.T) {
	got, err := ParseDayBookV3(mustFixture(t, "voucher-v3-sales-cgst-sgst.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(got.Vouchers) != 1 {
		t.Fatalf("Vouchers = %d, want 1", len(got.Vouchers))
	}
	v := got.Vouchers[0]

	var cgst, sgst *LedgerEntry
	for i := range v.LedgerEntries {
		switch v.LedgerEntries[i].GSTClass {
		case "CGST@9":
			cgst = &v.LedgerEntries[i]
		case "SGST@9":
			sgst = &v.LedgerEntries[i]
		}
	}
	if cgst == nil || sgst == nil {
		t.Fatalf("missing cgst/sgst entries: cgst=%v sgst=%v", cgst, sgst)
	}
	if cgst.Amount != -4500 || sgst.Amount != -4500 {
		t.Errorf("cgst=%f sgst=%f, want -4500 each", cgst.Amount, sgst.Amount)
	}
}

func TestParseDayBookV3_ConsolidatedBillRefs(t *testing.T) {
	got, err := ParseDayBookV3(mustFixture(t, "voucher-v3-purchase-consolidated.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(got.Vouchers) != 1 {
		t.Fatalf("Vouchers = %d, want 1", len(got.Vouchers))
	}
	v := got.Vouchers[0]
	// Indian-style "1,18,000.00" must round-trip.
	if v.LedgerEntries[0].Amount != -118000 {
		t.Errorf("party Amount = %f, want -118000", v.LedgerEntries[0].Amount)
	}
	if len(v.LedgerEntries[0].BillAllocations) != 2 {
		t.Fatalf("BillAllocations = %d, want 2", len(v.LedgerEntries[0].BillAllocations))
	}
	names := []string{
		v.LedgerEntries[0].BillAllocations[0].Name,
		v.LedgerEntries[0].BillAllocations[1].Name,
	}
	if names[0] != "PQR/APR/101" || names[1] != "PQR/APR/102" {
		t.Errorf("bill names = %v", names)
	}
	if v.LedgerEntries[1].Amount != 100000 {
		t.Errorf("purchase ledger Amount = %f, want 100000", v.LedgerEntries[1].Amount)
	}
}

func TestParseDayBookV3_RCMSelfInvoice(t *testing.T) {
	got, err := ParseDayBookV3(mustFixture(t, "voucher-v3-purchase-rcm.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	v := got.Vouchers[0]
	if !v.ReverseCharge {
		t.Error("ReverseCharge = false, want true")
	}
	if v.PartyGSTIN != "" {
		t.Errorf("PartyGSTIN = %q, want empty (transport vendor unregistered)", v.PartyGSTIN)
	}
}

func TestParseDayBookV3_EmptyCollection(t *testing.T) {
	got, err := ParseDayBookV3(mustFixture(t, "voucher-v3-empty.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(got.Vouchers) != 0 {
		t.Errorf("Vouchers = %d, want 0", len(got.Vouchers))
	}
	if got.Status != 1 {
		t.Errorf("Status = %d, want 1", got.Status)
	}
}

func TestParseDayBookV3_WrappedInTallyMessage(t *testing.T) {
	got, err := ParseDayBookV3(mustFixture(t, "voucher-v3-wrapped-in-tallymessage.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(got.Vouchers) != 1 {
		t.Fatalf("Vouchers = %d, want 1 (parser should find VOUCHER at any depth)", len(got.Vouchers))
	}
	v := got.Vouchers[0]
	// This fixture uses DD-MMM-YY date form — dateparse.go must handle it.
	wantDate := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	if !v.Date.Equal(wantDate) {
		t.Errorf("Date = %s, want %s", v.Date, wantDate)
	}
}

func TestParseDayBookV3_EmptyInputReturnsError(t *testing.T) {
	if _, err := ParseDayBookV3(nil); err == nil {
		t.Error("nil input: want error, got nil")
	}
	if _, err := ParseDayBookV3([]byte("   \n\t")); err == nil {
		t.Error("whitespace-only: want error, got nil")
	}
}

func TestParseDayBookV3_MalformedXMLReturnsError(t *testing.T) {
	bad := []byte(`<ENVELOPE><HEADER><VERSION>1</VERSION`) // truncated
	if _, err := ParseDayBookV3(bad); err == nil {
		t.Error("malformed xml: want error, got nil")
	}
}

func TestParseDayBookV3_DropsVoucherWithoutIdentifiers(t *testing.T) {
	// A voucher with no GUID and no VOUCHERNUMBER is untrackable — parser
	// drops it with a warning rather than shipping a ghost row to the server.
	minimal := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <VOUCHER><DATE>20260401</DATE><VOUCHERTYPENAME>X</VOUCHERTYPENAME></VOUCHER>
  <VOUCHER><GUID>kept-0001</GUID><DATE>20260402</DATE><VOUCHERTYPENAME>X</VOUCHERTYPENAME></VOUCHER>
</COLLECTION></DATA></BODY></ENVELOPE>`)
	got, err := ParseDayBookV3(minimal)
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(got.Vouchers) != 1 || got.Vouchers[0].GUID != "kept-0001" {
		t.Errorf("Vouchers = %+v", got.Vouchers)
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "dropped") {
		t.Errorf("expected a 'dropped' warning, got %v", got.Warnings)
	}
}
