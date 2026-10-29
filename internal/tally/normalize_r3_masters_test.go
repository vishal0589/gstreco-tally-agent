package tally

import "testing"

// QA v18 R3 against the shapes of a real company's Duties & Taxes masters
// (testdata/synthetic-r3-duty-ledgers-real-shapes.xml, synthetic names):
// Tally writes duty heads as "IGST" / "CGST" / "SGST/UTGST", and keeps RCM,
// import, URD and GST TCS ledgers under Duties & Taxes typed "Others".

func realShapesDutyBook(t *testing.T) *DutyLedgerBook {
	t.Helper()
	book, warnings, err := ParseDutyLedgers(readTestdata(t, "synthetic-r3-duty-ledgers-real-shapes.xml"))
	if err != nil || len(warnings) > 0 || book == nil {
		t.Fatalf("ParseDutyLedgers: book=%v warnings=%v err=%v", book, warnings, err)
	}
	return book
}

func TestHeadFromDutyHead_RealTallyValues(t *testing.T) {
	cases := map[string]string{
		"IGST":              "IGST",
		"CGST":              "CGST",
		"SGST/UTGST":        "SGST",
		"Integrated Tax":    "IGST",
		"Central Tax":       "CGST",
		"State Tax":         "SGST",
		"UT Tax":            "SGST",
		"State/UT Tax":      "SGST",
		"Cess":              "CESS",
		"Compensation Cess": "CESS",
		"State Cess":        "",
		"":                  "",
		"Not Applicable":    "",
	}
	for dutyHead, want := range cases {
		if got := headFromDutyHead(dutyHead); got != want {
			t.Errorf("headFromDutyHead(%q) = %q, want %q", dutyHead, got, want)
		}
	}
}

func TestClassifyLeg_RealMasterShapes(t *testing.T) {
	book := realShapesDutyBook(t)
	cases := []struct {
		ledger            string
		role              LegRole
		head, withholding string
		flagged           bool
	}{
		// Typed GST with Tally's own head values: plain tax, no flag.
		{"IGST Input Credit", LegTax, "IGST", "", false},
		{"CGST Input Credit", LegTax, "CGST", "", false},
		{"SGST Input Credit", LegTax, "SGST", "", false},
		{"SGST Output", LegTax, "SGST", "", false},
		// Typed "Others" under Duties & Taxes: GST from the name, flagged.
		{"RCM IGST Input Credit", LegTax, "IGST", "", true},
		{"RCM IGST Payable", LegTax, "IGST", "", true},
		{"IGST Input Credit on Import", LegTax, "IGST", "", true},
		{"URD CGST Input Credit", LegTax, "CGST", "", true},
		// GST TCS (section 52) is collected, never input credit.
		{"TCS - IGST", LegWithholding, "", WithholdingTCS, true},
		// Pre-GST duties stay other duty, whatever their name says.
		{"Service Tax Payable @ 14%", LegOtherDuty, "", "", false},
		{"Krishi Kalyan Cess @ 0.5% Payable", LegOtherDuty, "", "", true},
		{"Vat Input @ 5%", LegOtherDuty, "", "", false},
		// TDS ledgers, including ones typed "Others" with a stray duty head.
		{"TDS Payable-Rent (94I) - Co", LegWithholding, "", WithholdingTDS, false},
		{"TDS Pending Old", LegWithholding, "", WithholdingTDS, true},
		{"TDS Payable (94Q) @ 0.1 %", LegWithholding, "", WithholdingTDS, true},
		// Typed "Others" with no GST head anywhere: other duty.
		{"Advance Tax Paid", LegOtherDuty, "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.ledger, func(t *testing.T) {
			d := classifyLeg(LedgerEntry{LedgerName: tc.ledger, Amount: -100}, book)
			if d.role != tc.role || d.head != tc.head || d.withholding != tc.withholding || d.evidence != EvidenceDutyMaster {
				t.Errorf("classifyLeg(%q) = %s %q %q %s; want %s %q %q duty_master",
					tc.ledger, d.role, d.head, d.withholding, d.evidence, tc.role, tc.head, tc.withholding)
			}
			if got := len(d.flags) > 0; got != tc.flagged {
				t.Errorf("flagged = %v (%v), want %v", got, d.flags, tc.flagged)
			}
		})
	}
}

func TestNormalize_IntraStatePurchaseWithTallyDutyHeads(t *testing.T) {
	v := RawVoucher{
		GUID: "r3-real-intra", Date: parseDate(t, "2026-09-10"), VoucherType: "Purchase", VoucherNumber: "SYN/R3/060",
		PartyLedgerName: "Synthetic Vendor",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Synthetic Vendor", Amount: 11800, IsPartyLedger: true},
			{LedgerName: "Purchase - Local", Amount: -10000},
			{LedgerName: "CGST Input Credit", Amount: -900},
			{LedgerName: "SGST Input Credit", Amount: -900},
		},
	}
	row := normalizeOneWith(t, v, realShapesDutyBook(t))
	assertValues(t, row, 11800, 10000, 0, 900, 900)
	if len(row.NormalizationFlags) > 0 {
		t.Errorf("clean intra-state purchase flagged: %v", row.NormalizationFlags)
	}
	if leg := legNamed(t, row, "SGST Input Credit"); leg.Classification.Head == nil || *leg.Classification.Head != "sgst" ||
		leg.GSTDutyHead == nil || *leg.GSTDutyHead != "SGST/UTGST" {
		t.Errorf("SGST leg = %+v", leg)
	}
}

func TestNormalize_RCMAndImportLedgersTypedOthersKeepTheirGST(t *testing.T) {
	book := realShapesDutyBook(t)
	t.Run("reverse charge", func(t *testing.T) {
		v := RawVoucher{
			GUID: "r3-real-rcm", Date: parseDate(t, "2026-07-10"), VoucherType: "GST Purchase - RCM", VoucherNumber: "SYN/RCM/2",
			PartyLedgerName: "Synthetic Transport", ReverseCharge: true,
			LedgerEntries: []LedgerEntry{
				{LedgerName: "Synthetic Transport", Amount: 10000, IsPartyLedger: true},
				{LedgerName: "Freight Inward", Amount: -10000},
				{LedgerName: "RCM IGST Input Credit", Amount: -1800},
				{LedgerName: "RCM IGST Payable", Amount: 1800},
			},
		}
		row := normalizeOneWith(t, v, book)
		assertValues(t, row, 11800, 10000, 1800, 0, 0)
		if !hasFlag(row, `"RCM IGST Payable" (1800.00) is the reverse-charge liability`) || hasFlag(row, "do not balance") {
			t.Errorf("flags = %v", row.NormalizationFlags)
		}
	})
	t.Run("import", func(t *testing.T) {
		v := RawVoucher{
			GUID: "r3-real-import", Date: parseDate(t, "2026-07-12"), VoucherType: "Purchase", VoucherNumber: "SYN/IMP/1",
			PartyLedgerName: "Synthetic Customs Broker",
			LedgerEntries: []LedgerEntry{
				{LedgerName: "Synthetic Customs Broker", Amount: 118000, IsPartyLedger: true},
				{LedgerName: "Purchase - Import", Amount: -100000},
				{LedgerName: "IGST Input Credit on Import", Amount: -18000},
			},
		}
		row := normalizeOneWith(t, v, book)
		assertValues(t, row, 118000, 100000, 18000, 0, 0)
		if !hasFlag(row, `"IGST Input Credit on Import" is a Duties & Taxes ledger typed "Others", not GST: counted as IGST`) {
			t.Errorf("flags = %v", row.NormalizationFlags)
		}
	})
}
