package tally

import (
	"encoding/json"
	"strings"
	"testing"
)

// qaPurchase is a single-bill purchase shaped like QA v27's KSS vouchers:
// header REFERENCE (Supplier Invoice No.) and VOUCHERNUMBER (Purchase No.),
// one New Ref bill allocation on the party.
func qaPurchase(t *testing.T, guid, date, vchNo, ref, billRef, party, gstin string, igst float64) RawVoucher {
	t.Helper()
	taxable := igst / 0.18
	gross := taxable + igst
	return RawVoucher{
		GUID:            guid,
		IsInvoice:       true,
		Date:            parseDate(t, date),
		VoucherType:     "Purchase",
		VoucherNumber:   vchNo,
		Reference:       ref,
		PartyLedgerName: party,
		PartyGSTIN:      gstin,
		LedgerEntries: []LedgerEntry{
			{
				LedgerName: party, Amount: gross, IsPartyLedger: true,
				BillAllocations: []BillRef{{Name: billRef, Amount: gross, BillType: "New Ref"}},
			},
			{LedgerName: "Purchase IGST 18%", Amount: -taxable},
			{LedgerName: "IGST Input", Amount: -igst, GSTClass: "IGST@18"},
		},
	}
}

func TestNormalize_QAv27B19_HeaderSupplierInvoiceWinsOverBillRef(t *testing.T) {
	cases := []struct {
		name, date, vchNo, ref, billRef, party, gstin string
	}{
		{"FF 2915 keyed 2982", "2026-06-12", "2915", "2915", "2982", "FF Enterprises", "06AAFFF1234F1Z5"},
		{"FF 2930 keyed 2939", "2026-06-16", "2930", "2930", "2939", "FF Enterprises", "06AAFFF1234F1Z5"},
		{"FF 3146 keyed 3171", "2026-09-08", "3146", "3146", "3171", "FF Enterprises", "06AAFFF1234F1Z5"},
		{"Sampark SP/846 keyed SP/849", "2026-09-18", "117", "SP/846/HR/26-27", "SP/849/HR/26-27", "Sampark", "06AAJCS1234K1Z2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := qaPurchase(t, "g-"+tc.vchNo, tc.date, tc.vchNo, tc.ref, tc.billRef, tc.party, tc.gstin, 1136.70)
			row := normalizeOne(t, v)
			if row.InvoiceNumber != tc.ref {
				t.Errorf("invoice_number = %q, want header %q", row.InvoiceNumber, tc.ref)
			}
			if row.BillReference == nil || *row.BillReference != tc.billRef {
				t.Errorf("bill_reference = %v, want %q", row.BillReference, tc.billRef)
			}
			if !hasFlag(row, FlagReferenceConflict) {
				t.Errorf("flags = %v, want %s", row.NormalizationFlags, FlagReferenceConflict)
			}
		})
	}
}

func TestNormalize_QAv27B19_NoHeaderReferenceFallsBackToVoucherNumber(t *testing.T) {
	v := qaPurchase(t, "g-2915", "2026-06-12", "2915", "", "2982", "FF Enterprises", "06AAFFF1234F1Z5", 1136.70)
	row := normalizeOne(t, v)
	if row.InvoiceNumber != "2915" {
		t.Errorf("invoice_number = %q, want voucher number 2915", row.InvoiceNumber)
	}
	if !hasFlag(row, FlagReferenceConflict) {
		t.Errorf("flags = %v, want %s", row.NormalizationFlags, FlagReferenceConflict)
	}
}

func TestNormalize_BillRefAgreeingAfterNormalisationIsNoConflict(t *testing.T) {
	for _, billRef := range []string{"sp/846/hr/26-27", "SP-846-HR-26-27", "SP/0846/HR/26-27", "SP846HR2627"} {
		v := qaPurchase(t, "g-sp", "2026-09-18", "117", "SP/846/HR/26-27", billRef, "Sampark", "06AAJCS1234K1Z2", 180)
		row := normalizeOne(t, v)
		if row.InvoiceNumber != "SP/846/HR/26-27" {
			t.Errorf("%s: invoice_number = %q", billRef, row.InvoiceNumber)
		}
		if hasFlag(row, FlagReferenceConflict) {
			t.Errorf("%s: flagged reference_conflict for the same number", billRef)
		}
		if row.BillReference == nil || *row.BillReference != billRef {
			t.Errorf("%s: bill_reference = %v", billRef, row.BillReference)
		}
	}
}

func TestNormalize_QAv27B24_PlaceholderNeverKeysARow(t *testing.T) {
	// 05-May Rs 540 and 31-Jul Rs 567: the bill ref said "No".
	for _, ph := range []string{"No", "NA", "N/A", "Nil", "-", ".", "0", "  "} {
		v := qaPurchase(t, "g-540", "2026-05-05", "88", "", ph, "Local Vendor", "", 82.37)
		row := normalizeOne(t, v)
		if row.InvoiceNumber != "88" {
			t.Errorf("bill ref %q: invoice_number = %q, want voucher number 88", ph, row.InvoiceNumber)
		}
		if row.BillReference != nil {
			t.Errorf("bill ref %q: bill_reference = %q, want none", ph, *row.BillReference)
		}
		if hasFlag(row, FlagReferenceConflict) || hasFlag(row, FlagPlaceholderReference) {
			t.Errorf("bill ref %q: flags = %v, want none (the voucher number is a real key)", ph, row.NormalizationFlags)
		}
	}

	// Header reference is the placeholder: the voucher number keys it.
	v := qaPurchase(t, "g-567", "2026-07-31", "131", "No", "No", "Local Vendor", "", 86.49)
	if row := normalizeOne(t, v); row.InvoiceNumber != "131" {
		t.Errorf("header \"No\": invoice_number = %q, want 131", row.InvoiceNumber)
	}

	// Nothing better than placeholders: fall back to the internal id, flagged.
	v = qaPurchase(t, "", "2026-07-31", "", "No", "No", "Local Vendor", "", 86.49)
	v.MasterID = "4412"
	row := normalizeOne(t, v)
	if row.InvoiceNumber != "TALLYMID-4412" {
		t.Errorf("all placeholders: invoice_number = %q, want TALLYMID-4412", row.InvoiceNumber)
	}
	if !hasFlag(row, FlagPlaceholderReference) {
		t.Errorf("all placeholders: flags = %v, want %s", row.NormalizationFlags, FlagPlaceholderReference)
	}
}

func TestNormalize_BillRefKeysOnlyWhenHeaderHasNoNumber(t *testing.T) {
	v := qaPurchase(t, "g-x", "2026-06-12", "", "", "INV-77", "FF Enterprises", "06AAFFF1234F1Z5", 18)
	row := normalizeOne(t, v)
	if row.InvoiceNumber != "INV-77" || hasFlag(row, FlagReferenceConflict) {
		t.Errorf("invoice_number = %q flags = %v, want INV-77 unflagged", row.InvoiceNumber, row.NormalizationFlags)
	}
}

func TestNormalize_SalesKeysOnTheCompanysOwnVoucherNumber(t *testing.T) {
	v := qaPurchase(t, "g-s", "2026-06-12", "S/26-27/101", "PO-5531", "S/26-27/100", "Customer", "29ABCDE1234F1Z5", 18)
	v.VoucherType = "Sales"
	v.LedgerEntries[0].Amount = -v.LedgerEntries[0].Amount
	v.LedgerEntries[1].Amount = -v.LedgerEntries[1].Amount
	v.LedgerEntries[2].Amount = -v.LedgerEntries[2].Amount
	rows, err := Normalize(v, NormalizeOptions{Kind: IngestKindSales})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if rows[0].InvoiceNumber != "S/26-27/101" {
		t.Errorf("invoice_number = %q, want voucher number S/26-27/101", rows[0].InvoiceNumber)
	}
	if !hasFlag(rows[0], FlagReferenceConflict) {
		t.Errorf("flags = %v, want %s", rows[0].NormalizationFlags, FlagReferenceConflict)
	}
}

func TestNormalize_ConsolidatedPlaceholderBillNeverKeys(t *testing.T) {
	v := qaPurchase(t, "g-c", "2026-06-12", "41", "GK/41", "x", "GK Glass", "06AAGFG1234Q1Z1", 180)
	party := v.LedgerEntries[0].Amount
	v.LedgerEntries[0].BillAllocations = []BillRef{
		{Name: "GK/41", Amount: party / 2, BillType: "New Ref"},
		{Name: "No", Amount: party / 2, BillType: "New Ref"},
	}
	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if rows[0].InvoiceNumber != "GK/41" || hasFlag(rows[0], FlagPlaceholderReference) {
		t.Errorf("row 0 = %q %v", rows[0].InvoiceNumber, rows[0].NormalizationFlags)
	}
	if rows[1].InvoiceNumber == "No" || !hasFlag(rows[1], FlagPlaceholderReference) {
		t.Errorf("row 1 = %q %v, want a non-placeholder key, flagged", rows[1].InvoiceNumber, rows[1].NormalizationFlags)
	}
	if rows[1].BillReference != nil {
		t.Errorf("row 1 bill_reference = %q, want none", *rows[1].BillReference)
	}
}

func TestFlagDuplicateInvoiceNumbers_QAv27B25(t *testing.T) {
	// GK Glass 62 (Rs 4,220) and 63 (Rs 8,540) both carried "62" as their
	// only number (no header reference, bill ref 62 on each).
	gk62 := qaPurchase(t, "gk-62", "2026-06-02", "", "", "62", "GK Glass", "06AAGFG1234Q1Z1", 643.73)
	gk63 := qaPurchase(t, "gk-63", "2026-06-06", "", "", "62", "GK Glass", "06AAGFG1234Q1Z1", 1302.71)
	other := qaPurchase(t, "ot-62", "2026-06-06", "", "62", "62", "Other Vendor", "06AAAAA1234Q1Z1", 18)
	rows := []IngestVoucherRow{normalizeOne(t, gk62), normalizeOne(t, gk63), normalizeOne(t, other)}
	FlagDuplicateInvoiceNumbers(rows)
	if !hasFlag(rows[0], FlagDuplicateNumberInPeriod) || !hasFlag(rows[1], FlagDuplicateNumberInPeriod) {
		t.Errorf("GK Glass rows not both flagged: %v / %v", rows[0].NormalizationFlags, rows[1].NormalizationFlags)
	}
	if hasFlag(rows[2], FlagDuplicateNumberInPeriod) {
		t.Errorf("another party's 62 was flagged: %v", rows[2].NormalizationFlags)
	}

	// With the header numbers present (62 and 63) the header keys them
	// apart and the copied bill ref is a conflict, not a duplicate.
	gk62 = qaPurchase(t, "gk-62", "2026-06-02", "62", "62", "62", "GK Glass", "06AAGFG1234Q1Z1", 643.73)
	gk63 = qaPurchase(t, "gk-63", "2026-06-06", "63", "63", "62", "GK Glass", "06AAGFG1234Q1Z1", 1302.71)
	rows = []IngestVoucherRow{normalizeOne(t, gk62), normalizeOne(t, gk63)}
	FlagDuplicateInvoiceNumbers(rows)
	if rows[0].InvoiceNumber != "62" || rows[1].InvoiceNumber != "63" {
		t.Errorf("keys = %q, %q, want 62, 63", rows[0].InvoiceNumber, rows[1].InvoiceNumber)
	}
	if hasFlag(rows[0], FlagDuplicateNumberInPeriod) || hasFlag(rows[1], FlagDuplicateNumberInPeriod) {
		t.Error("distinct header numbers flagged as duplicates")
	}
	if !hasFlag(rows[1], FlagReferenceConflict) {
		t.Errorf("voucher 63 flags = %v, want %s", rows[1].NormalizationFlags, FlagReferenceConflict)
	}
}

func TestFlagDuplicateInvoiceNumbers_SameNumberAfterNormalisationAndNoGSTIN(t *testing.T) {
	a := qaPurchase(t, "a", "2026-05-05", "INV/0012", "", "", "Local  Vendor", "", 18)
	b := qaPurchase(t, "b", "2026-05-09", "inv-12", "", "", "local vendor", "", 18)
	rows := []IngestVoucherRow{normalizeOne(t, a), normalizeOne(t, b)}
	FlagDuplicateInvoiceNumbers(rows)
	if !hasFlag(rows[0], FlagDuplicateNumberInPeriod) || !hasFlag(rows[1], FlagDuplicateNumberInPeriod) {
		t.Errorf("flags = %v / %v", rows[0].NormalizationFlags, rows[1].NormalizationFlags)
	}
}

func TestFlagDuplicateInvoiceNumbers_SplitRowsOfOneVoucherAreNotDuplicates(t *testing.T) {
	v := qaPurchase(t, "g-split", "2026-06-12", "41", "", "x", "GK Glass", "06AAGFG1234Q1Z1", 180)
	party := v.LedgerEntries[0].Amount
	v.LedgerEntries[0].BillAllocations = []BillRef{
		{Name: "No", Amount: party / 2, BillType: "New Ref"},
		{Name: "-", Amount: party / 2, BillType: "New Ref"},
	}
	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	FlagDuplicateInvoiceNumbers(rows)
	for _, r := range rows {
		if hasFlag(r, FlagDuplicateNumberInPeriod) {
			t.Errorf("split row flagged duplicate: %v", r.NormalizationFlags)
		}
	}
}

func TestInvoiceNumberKey(t *testing.T) {
	same := [][2]string{
		{"INV/0012", "inv-12"},
		{"INV/0012", "INV12"},
		{"SP/846/HR/26-27", "sp 846 hr 26 27"},
		{"02915", "2915"},
	}
	for _, p := range same {
		if invoiceNumberKey(p[0]) != invoiceNumberKey(p[1]) {
			t.Errorf("%q and %q should be one number", p[0], p[1])
		}
	}
	diff := [][2]string{{"2915", "2982"}, {"SP/846/HR/26-27", "SP/849/HR/26-27"}}
	for _, p := range diff {
		if invoiceNumberKey(p[0]) == invoiceNumberKey(p[1]) {
			t.Errorf("%q and %q should differ", p[0], p[1])
		}
	}
}

func TestIsPlaceholderReference(t *testing.T) {
	for _, s := range []string{"", " ", "No", "NO", "no.", "NA", "N/A", "n.a.", "Nil", "NIL", "-", ".", "0", "000", "None", "--"} {
		if !isPlaceholderReference(s) {
			t.Errorf("%q should be a placeholder", s)
		}
	}
	for _, s := range []string{"62", "No.62", "NA/101", "2915", "SP/846/HR/26-27", "0012", "NOV-12"} {
		if isPlaceholderReference(s) {
			t.Errorf("%q is a real number", s)
		}
	}
}

func TestIngestVoucherRow_BillReferenceOnTheWire(t *testing.T) {
	v := qaPurchase(t, "g-2915", "2026-06-12", "2915", "2915", "2982", "FF Enterprises", "06AAFFF1234F1Z5", 1136.70)
	b, err := json.Marshal(normalizeOne(t, v))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"invoice_number":"2915"`, `"bill_reference":"2982"`, `"voucher_reference":"2915"`, `"reference_conflict"`} {
		if !strings.Contains(s, want) {
			t.Errorf("wire row lacks %s: %s", want, s)
		}
	}
}
