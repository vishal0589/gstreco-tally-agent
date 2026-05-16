package tally

import (
	"math"
	"testing"
	"time"
)

func TestNormalize_PurchaseSingleFromFixture(t *testing.T) {
	parsed, err := ParseDayBookV3(mustFixture(t, "voucher-v3-purchase-single.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	rows, err := Normalize(parsed.Vouchers[0], NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.InvoiceValue != 118000 {
		t.Errorf("InvoiceValue = %v, want 118000", r.InvoiceValue)
	}
	if r.TaxableValue != 100000 {
		t.Errorf("TaxableValue = %v, want 100000", r.TaxableValue)
	}
	if r.IGST != 18000 || r.CGST != 0 || r.SGST != 0 || r.CESS != 0 {
		t.Errorf("tax buckets: igst=%v cgst=%v sgst=%v cess=%v", r.IGST, r.CGST, r.SGST, r.CESS)
	}
	if r.VendorName == nil || *r.VendorName != "ABC Vendors Ltd" {
		t.Errorf("VendorName = %v", r.VendorName)
	}
	if r.VendorGSTIN == nil || *r.VendorGSTIN != "29ABCDE1234F1Z5" {
		t.Errorf("VendorGSTIN = %v", r.VendorGSTIN)
	}
	if r.CustomerName != nil || r.CustomerGSTIN != nil {
		t.Error("purchase row should NOT have customer_* fields populated")
	}
	if r.TallyVoucherGUID == nil || *r.TallyVoucherGUID != "fixture-purchase-single-0001" {
		t.Errorf("TallyVoucherGUID = %v", r.TallyVoucherGUID)
	}
	if r.ParentRef != nil {
		t.Errorf("single-bill voucher must not set parent_ref; got %v", r.ParentRef)
	}
	// Single-bill vouchers use the BillRef name (counterparty invoice no.)
	// rather than Tally's VoucherNumber, so the server matches against
	// what the vendor actually billed.
	if r.InvoiceNumber != "SUPP-INV-5501" {
		t.Errorf("InvoiceNumber = %q, want SUPP-INV-5501", r.InvoiceNumber)
	}
	if r.TaxRate == nil || *r.TaxRate != 18 {
		t.Errorf("TaxRate = %v, want 18 derived from GSTClass IGST@18", r.TaxRate)
	}
	if r.InvoiceDate != "2026-04-10" {
		t.Errorf("InvoiceDate = %q", r.InvoiceDate)
	}
	if r.VoucherNumber == nil || *r.VoucherNumber != "PI/001/26-27" {
		t.Errorf("VoucherNumber = %v, want PI/001/26-27", r.VoucherNumber)
	}
	if r.VoucherDate == nil || *r.VoucherDate != "2026-04-10" {
		t.Errorf("VoucherDate = %v, want 2026-04-10", r.VoucherDate)
	}
	if r.VoucherTypeName == nil || *r.VoucherTypeName != "GST Purchase" {
		t.Errorf("VoucherTypeName = %v, want GST Purchase", r.VoucherTypeName)
	}
	if r.VoucherReference == nil || *r.VoucherReference != "SUPP-INV-5501" {
		t.Errorf("VoucherReference = %v, want SUPP-INV-5501", r.VoucherReference)
	}
}

func TestNormalize_SalesCGSTSGST(t *testing.T) {
	parsed, err := ParseDayBookV3(mustFixture(t, "voucher-v3-sales-cgst-sgst.xml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rows, err := Normalize(parsed.Vouchers[0], NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.InvoiceValue != 59000 {
		t.Errorf("InvoiceValue = %v", r.InvoiceValue)
	}
	if r.IGST != 0 || r.CGST != 4500 || r.SGST != 4500 {
		t.Errorf("igst=%v cgst=%v sgst=%v, want 0/4500/4500", r.IGST, r.CGST, r.SGST)
	}
	if r.TaxableValue != 50000 {
		t.Errorf("TaxableValue = %v, want 50000", r.TaxableValue)
	}
	// Sales side: customer fields populated, vendor empty.
	if r.VendorName != nil || r.VendorGSTIN != nil {
		t.Error("sales row must not have vendor_*")
	}
	if r.CustomerName == nil || *r.CustomerName != "XYZ Retailers" {
		t.Errorf("CustomerName = %v", r.CustomerName)
	}
}

func TestNormalize_ConsolidatedProratedByBillShare(t *testing.T) {
	parsed, err := ParseDayBookV3(mustFixture(t, "voucher-v3-purchase-consolidated.xml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rows, err := Normalize(parsed.Vouchers[0], NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per bill ref)", len(rows))
	}
	// Shares: 70800/118000 = 0.6, 47200/118000 = 0.4.
	// igst total = 18000 → split 10800 + 7200.
	// taxable total = 100000 → split 60000 + 40000.
	gotNames := []string{rows[0].InvoiceNumber, rows[1].InvoiceNumber}
	if gotNames[0] != "PQR/APR/101" || gotNames[1] != "PQR/APR/102" {
		t.Errorf("names = %v", gotNames)
	}
	if !nearly(rows[0].InvoiceValue, 70800, 0.01) || !nearly(rows[1].InvoiceValue, 47200, 0.01) {
		t.Errorf("invoice values: %v, %v", rows[0].InvoiceValue, rows[1].InvoiceValue)
	}
	if !nearly(rows[0].IGST, 10800, 0.01) || !nearly(rows[1].IGST, 7200, 0.01) {
		t.Errorf("igst split: %v, %v", rows[0].IGST, rows[1].IGST)
	}
	if !nearly(rows[0].TaxableValue, 60000, 0.01) || !nearly(rows[1].TaxableValue, 40000, 0.01) {
		t.Errorf("taxable split: %v, %v", rows[0].TaxableValue, rows[1].TaxableValue)
	}
	// parent_ref preserved on every split row so the reco inspector can
	// re-stitch the pair.
	for i, r := range rows {
		if r.ParentRef == nil || *r.ParentRef != "fixture-purchase-consolidated-0001" {
			t.Errorf("row %d: ParentRef = %v, want fixture-purchase-consolidated-0001", i, r.ParentRef)
		}
	}
}

func TestNormalize_ConsolidatedWithoutGUIDUsesFallbackParentRef(t *testing.T) {
	v := RawVoucher{
		IsInvoice:       true,
		Date:            parseDate(t, "2026-04-10"),
		VoucherType:     "GST Purchase",
		Reference:       "SUPP-INV-5501",
		PartyLedgerName: "ABC Vendors Ltd",
		LedgerEntries: []LedgerEntry{
			{
				LedgerName:    "ABC Vendors Ltd",
				Amount:        -118000,
				IsPartyLedger: true,
				BillAllocations: []BillRef{
					{Name: "PQR/APR/101", Amount: -70800, BillType: "New Ref"},
					{Name: "PQR/APR/102", Amount: -47200, BillType: "New Ref"},
				},
			},
			{LedgerName: "Purchase A/c", Amount: 100000},
			{LedgerName: "IGST @ 18%", Amount: 18000, GSTClass: "IGST@18"},
		},
	}

	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for i, row := range rows {
		if row.ParentRef == nil || *row.ParentRef != "SUPP-INV-5501" {
			t.Errorf("row %d ParentRef = %v, want SUPP-INV-5501", i, row.ParentRef)
		}
		if row.TallyVoucherGUID != nil {
			t.Errorf("row %d TallyVoucherGUID = %v, want nil when GUID is absent", i, row.TallyVoucherGUID)
		}
	}
}

func TestNormalize_MasterIDFallbackSynthesizesInvoiceNumber(t *testing.T) {
	v := RawVoucher{
		MasterID:        "900012",
		IsInvoice:       true,
		Date:            parseDate(t, "2026-04-10"),
		VoucherType:     "Purchase",
		PartyLedgerName: "Fallback Vendor",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Fallback Vendor", Amount: -11800, IsPartyLedger: true},
			{LedgerName: "Purchases A/c", Amount: 10000},
			{LedgerName: "IGST @ 18%", Amount: 1800, GSTClass: "IGST@18"},
		},
	}

	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].InvoiceNumber != "TALLYMID-900012" {
		t.Errorf("InvoiceNumber = %q, want TALLYMID-900012", rows[0].InvoiceNumber)
	}
}

func TestNormalize_NarrationFallbackPrefersInvoiceLikeToken(t *testing.T) {
	v := RawVoucher{
		IsInvoice:       true,
		Date:            parseDate(t, "2026-04-10"),
		VoucherType:     "Purchase",
		PartyLedgerName: "Fallback Vendor",
		Narration:       "Being purchase against invoice SUPP-INV-5501 for office chairs",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Fallback Vendor", Amount: -11800, IsPartyLedger: true},
			{LedgerName: "Purchases A/c", Amount: 10000},
			{LedgerName: "IGST @ 18%", Amount: 1800, GSTClass: "IGST@18"},
		},
	}

	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].InvoiceNumber != "SUPP-INV-5501" {
		t.Errorf("InvoiceNumber = %q, want SUPP-INV-5501", rows[0].InvoiceNumber)
	}
}

func TestNormalize_PurchaseDebitNoteCarriesVoucherProvenanceSeparatelyFromSupplierReference(t *testing.T) {
	v := RawVoucher{
		GUID:            "fixture-rahul-note-0001",
		IsInvoice:       true,
		Date:            parseDate(t, "2026-04-07"),
		VoucherType:     "Debit Note",
		VoucherNumber:   "DIPLDN/APR26/003",
		Reference:       "RE/26-27/061",
		PartyLedgerName: "Rahul Enterprises - MSME",
		PartyGSTIN:      "06ACJPG5816D1ZW",
		LedgerEntries: []LedgerEntry{
			{
				LedgerName:    "Rahul Enterprises - MSME",
				Amount:        2620,
				IsPartyLedger: true,
				BillAllocations: []BillRef{
					{Name: "RE/26-27/061", Amount: 2620, BillType: "Agst Ref"},
				},
			},
			{LedgerName: "Purchase Returns @ 18%", Amount: -2220.36},
			{LedgerName: "CGST @ 9%", Amount: -199.82, GSTClass: "CGST@9"},
			{LedgerName: "SGST @ 9%", Amount: -199.82, GSTClass: "SGST@9"},
		},
	}

	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.InvoiceNumber != "RE/26-27/061" {
		t.Errorf("InvoiceNumber = %q, want supplier reference RE/26-27/061", row.InvoiceNumber)
	}
	if row.VoucherNumber == nil || *row.VoucherNumber != "DIPLDN/APR26/003" {
		t.Errorf("VoucherNumber = %v, want DIPLDN/APR26/003", row.VoucherNumber)
	}
	if row.VoucherReference == nil || *row.VoucherReference != "RE/26-27/061" {
		t.Errorf("VoucherReference = %v, want RE/26-27/061", row.VoucherReference)
	}
	if row.VoucherTypeName == nil || *row.VoucherTypeName != "Debit Note" {
		t.Errorf("VoucherTypeName = %v, want Debit Note", row.VoucherTypeName)
	}
	if row.VoucherDate == nil || *row.VoucherDate != "2026-04-07" {
		t.Errorf("VoucherDate = %v, want 2026-04-07", row.VoucherDate)
	}
}

func TestNormalize_ConsolidatedThreeBillsSumToOriginal(t *testing.T) {
	// Three equal bills with a tax total that doesn't divide evenly (₹55 /
	// 3 = 18.333...). With naive proration each row rounds to 18.33 and the
	// sum is 54.99 — short by ₹0.01. Residual allocation on the last row
	// must keep the sum exactly equal to the original totals.
	v := RawVoucher{
		GUID: "consolidated-3bill", IsInvoice: true,
		Date:            parseDate(t, "2026-04-10"),
		VoucherType:     "GST Purchase",
		VoucherNumber:   "PI/003B",
		PartyLedgerName: "V",
		LedgerEntries: []LedgerEntry{
			{
				LedgerName: "V", Amount: -355, IsPartyLedger: true,
				BillAllocations: []BillRef{
					{Name: "B1", Amount: -100, BillType: "New Ref"},
					{Name: "B2", Amount: -100, BillType: "New Ref"},
					{Name: "B3", Amount: -100, BillType: "New Ref"},
				},
			},
			{LedgerName: "Purchase A/c", Amount: 300},
			{LedgerName: "IGST @ 18%", Amount: 55, GSTClass: "IGST@18"},
		},
	}
	// Adjust: total voucher magnitude = 300 + 55 = 355. Party amount =
	// -355 so abs = 355. Three bills of 100 each = 300, leaving 55 on the
	// "remainder" of the last bill after residual. Actually we want shares
	// that genuinely drift, so use equal bills with an uneven tax total.
	// 55 / 3 naively → 18.33 each, sums to 54.99.

	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}

	var sumInvoice, sumTaxable, sumIGST float64
	for _, r := range rows {
		sumInvoice += r.InvoiceValue
		sumTaxable += r.TaxableValue
		sumIGST += r.IGST
	}
	// Totals on the wire MUST equal the original voucher totals (to the
	// cent). Reco reports would otherwise show a per-voucher mismatch.
	if !nearly(sumInvoice, 355, 0.005) {
		t.Errorf("sum(InvoiceValue) = %v, want 355.00", sumInvoice)
	}
	if !nearly(sumTaxable, 300, 0.005) {
		t.Errorf("sum(TaxableValue) = %v, want 300.00", sumTaxable)
	}
	if !nearly(sumIGST, 55, 0.005) {
		t.Errorf("sum(IGST) = %v, want 55.00 — naive proration would give 54.99", sumIGST)
	}
	// Last row should carry the residual (18.34 vs 18.33 for first two).
	if rows[2].IGST <= rows[0].IGST {
		t.Errorf("last row IGST = %v, expected >= first-row %v (residual allocation)",
			rows[2].IGST, rows[0].IGST)
	}
}

func TestNormalize_RCMMarksReverseChargeAndKind(t *testing.T) {
	parsed, err := ParseDayBookV3(mustFixture(t, "voucher-v3-purchase-rcm.xml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	rows, err := Normalize(parsed.Vouchers[0], NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.ReverseCharge == nil || !*r.ReverseCharge {
		t.Error("ReverseCharge not set on RCM row")
	}
	// inferKind must have seen ISRCMAPPLICABLE=Yes and routed to RCM.
	if inferKind(parsed.Vouchers[0]) != IngestKindRCMSelfInvoice {
		t.Error("inferKind did not route to rcm_self_invoice")
	}
	// Party GSTIN was empty in the fixture (transport GTA, unregistered);
	// row must not carry a stray empty GSTIN.
	if r.VendorGSTIN != nil {
		t.Errorf("VendorGSTIN = %v, want nil for unregistered vendor", r.VendorGSTIN)
	}
}

func TestNormalize_SkipsCancelledAndNonInvoice(t *testing.T) {
	base := RawVoucher{
		GUID: "g", Date: parseDate(t, "2026-04-10"),
		VoucherType: "Purchase", VoucherNumber: "P1",
		PartyLedgerName: "V", IsInvoice: true,
		LedgerEntries: []LedgerEntry{
			{LedgerName: "V", Amount: -118000, IsPartyLedger: true},
			{LedgerName: "IGST", Amount: 18000, GSTClass: "IGST@18"},
		},
	}
	base.IsCancelled = true
	if rows, _ := Normalize(base, NormalizeOptions{}); rows != nil {
		t.Errorf("cancelled voucher produced %d rows, want 0", len(rows))
	}
	base.IsCancelled = false
	base.IsInvoice = false
	if rows, _ := Normalize(base, NormalizeOptions{}); rows != nil {
		t.Errorf("non-invoice voucher produced %d rows, want 0", len(rows))
	}
}

func TestNormalize_NoPartyLedgerIsAnError(t *testing.T) {
	v := RawVoucher{
		GUID: "g", IsInvoice: true, Date: parseDate(t, "2026-04-10"),
		PartyLedgerName: "V",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Different", Amount: -100, IsPartyLedger: false},
		},
	}
	if _, err := Normalize(v, NormalizeOptions{}); err == nil {
		t.Error("expected error when party ledger is missing")
	}
}

func TestClassifyTaxLedger_PrefersGSTClassOverName(t *testing.T) {
	// A ledger named "IGST @ 18%" but tagged as CGST in the master must
	// classify as CGST. Saves us when a user's ledger names are misleading.
	if classifyTaxLedger("CGST@9", "IGST @ 18%") != "CGST" {
		t.Error("classifyTaxLedger should prefer GSTClass when present")
	}
	if classifyTaxLedger("", "Output IGST @ 18%") != "IGST" {
		t.Error("classifyTaxLedger should fall back to name pattern")
	}
	// UTGST folds into SGST — single bucket on the wire.
	if classifyTaxLedger("UTGST@9", "UTGST 9%") != "SGST" {
		t.Error("UTGST should fold into SGST")
	}
	if classifyTaxLedger("", "Purchase @ 18%") != "" {
		t.Error("non-tax ledger classified as tax")
	}
	if classifyTaxLedger("", "Purchase (IGST)@18%") != "" {
		t.Error("purchase account ledger with embedded tax marker classified as tax")
	}
	if classifyTaxLedger("", "Sales (CGST)@9%") != "" {
		t.Error("sales account ledger with embedded tax marker classified as tax")
	}
}

func TestNormalize_HSNFromFirstInventoryEntry(t *testing.T) {
	// v0.1.11 — HSN flows from first inventory entry through to
	// IngestVoucherRow.HSN. Drives server-side capital-goods
	// classifier + Sec 17(5) blocked-credit detector.
	parsed, err := ParseDayBookV3(mustFixture(t, "voucher-v3-purchase-single.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	rows, err := Normalize(parsed.Vouchers[0], NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.HSN == nil {
		t.Fatalf("HSN should be populated from inventory entry; got nil")
	}
	if *r.HSN != "4802" {
		t.Errorf("HSN = %q, want 4802 (from fixture's GSTHSNNAME)", *r.HSN)
	}
}

func TestNormalize_HSNNilWhenNoInventoryEntries(t *testing.T) {
	// Service-only purchases (no <INVENTORYENTRIES.LIST>) leave HSN nil.
	// The server's capital-goods classifier defaults is_capital_good=false
	// for unknown HSN, which is correct: services are never capital goods.
	v := RawVoucher{
		GUID: "service-only", IsInvoice: true,
		Date:            parseDate(t, "2026-04-10"),
		VoucherType:     "Purchase",
		PartyLedgerName: "Vendor", PartyGSTIN: "29ABCDE1234F1Z5",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Vendor", Amount: -11800, IsPartyLedger: true,
				BillAllocations: []BillRef{{Name: "INV-1", Amount: 11800, BillType: "New Ref"}}},
			{LedgerName: "IGST", GSTClass: "IGST@18", Amount: 1800},
			{LedgerName: "Service Account", Amount: 10000},
		},
		// No InventoryEntries — service-only.
	}
	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].HSN != nil {
		t.Errorf("HSN = %v, want nil for service-only purchase", *rows[0].HSN)
	}
}

func TestNormalize_PurchaseLedgerNamedWithTaxFamilyDoesNotCollapseTaxableValue(t *testing.T) {
	v := RawVoucher{
		GUID:            "eyup-5-like",
		IsInvoice:       true,
		Date:            parseDate(t, "2026-04-14"),
		VoucherType:     "Purchase",
		VoucherNumber:   "EYUP-5",
		Reference:       "EYUP-5",
		PartyLedgerName: "RETAILEZ PRIVATE LIMITED (24)",
		PartyGSTIN:      "24AALCR3173P1ZT",
		LedgerEntries: []LedgerEntry{
			{
				LedgerName:    "RETAILEZ PRIVATE LIMITED (24)",
				Amount:        2540,
				IsPartyLedger: true,
				BillAllocations: []BillRef{
					{Name: "EYUP-5", Amount: 2540, BillType: "New Ref"},
				},
			},
			{LedgerName: "Purchase (IGST)@18%", Amount: -2152.50},
			{LedgerName: "IGST Input Credit", Amount: -387.45, GSTClass: "Not Applicable"},
		},
	}

	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.InvoiceNumber != "EYUP-5" {
		t.Errorf("InvoiceNumber = %q, want EYUP-5", row.InvoiceNumber)
	}
	if !nearly(row.InvoiceValue, 2540, 0.01) {
		t.Errorf("InvoiceValue = %v, want 2540", row.InvoiceValue)
	}
	if !nearly(row.TaxableValue, 2152.55, 0.01) {
		t.Errorf("TaxableValue = %v, want 2152.55", row.TaxableValue)
	}
	if !nearly(row.IGST, 387.45, 0.01) || row.CGST != 0 || row.SGST != 0 {
		t.Errorf("tax buckets: igst=%v cgst=%v sgst=%v, want 387.45/0/0", row.IGST, row.CGST, row.SGST)
	}
}

func TestNormalize_HSNSkipsWhitespaceOnlyEntries(t *testing.T) {
	// Tally serialises legacy stock-items with " " when the master
	// HSN field is blank. We skip those and pick the first ACTUALLY
	// non-empty HSN downstream — guards against sending whitespace
	// codes to the server's classifier (which strips them server-side
	// but the wire payload should be clean).
	v := RawVoucher{
		GUID: "mixed-hsn", IsInvoice: true,
		Date:            parseDate(t, "2026-04-10"),
		VoucherType:     "Purchase",
		PartyLedgerName: "Vendor", PartyGSTIN: "29ABCDE1234F1Z5",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Vendor", Amount: -11800, IsPartyLedger: true,
				BillAllocations: []BillRef{{Name: "INV-1", Amount: 11800, BillType: "New Ref"}}},
			{LedgerName: "IGST", GSTClass: "IGST@18", Amount: 1800},
			{LedgerName: "Purchase", Amount: 10000},
		},
		InventoryEntries: []InventoryEntry{
			{StockItem: "Legacy item", HSN: "   "},
			{StockItem: "Computer", HSN: "8471", Quantity: 1, Rate: 50000, Amount: 50000},
			{StockItem: "Mouse", HSN: "8517"},
		},
	}
	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if rows[0].HSN == nil {
		t.Fatal("HSN should pick the first non-whitespace entry")
	}
	if *rows[0].HSN != "8471" {
		t.Errorf("HSN = %q, want 8471 (skipping whitespace-only first entry)", *rows[0].HSN)
	}
}

func TestNormalize_HSNCarriesAcrossConsolidatedBills(t *testing.T) {
	// Multi-bill (consolidated) vouchers: every split row from the
	// same voucher carries the same first-HSN. The first-HSN is a
	// voucher-level fact, not a bill-level fact — Tally doesn't
	// separately tag inventory entries to bill allocations.
	v := RawVoucher{
		GUID: "consolidated", IsInvoice: true,
		Date:            parseDate(t, "2026-04-10"),
		VoucherType:     "Purchase",
		PartyLedgerName: "Vendor", PartyGSTIN: "29ABCDE1234F1Z5",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Vendor", Amount: -23600, IsPartyLedger: true,
				BillAllocations: []BillRef{
					{Name: "BILL-A", Amount: 11800, BillType: "New Ref"},
					{Name: "BILL-B", Amount: 11800, BillType: "New Ref"},
				}},
			{LedgerName: "IGST", GSTClass: "IGST@18", Amount: 3600},
			{LedgerName: "Purchase", Amount: 20000},
		},
		InventoryEntries: []InventoryEntry{
			{StockItem: "Computer", HSN: "8471"},
		},
	}
	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for i, r := range rows {
		if r.HSN == nil || *r.HSN != "8471" {
			t.Errorf("rows[%d].HSN = %v, want %q", i, r.HSN, "8471")
		}
	}
}

// --- helpers ---

func nearly(a, b, eps float64) bool { return math.Abs(a-b) <= eps }

func parseDate(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := ParseTallyDate(s)
	if err != nil {
		t.Fatalf("parseDate(%q): %v", s, err)
	}
	return tt
}
