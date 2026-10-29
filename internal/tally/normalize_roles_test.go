package tally

import (
	"encoding/json"
	"strings"
	"testing"
)

// QA v18 R3: every invoice voucher leg gets one role from the evidence
// (duty-ledger master, then GST class, then the name), and gross, taxable,
// GST and TDS come from the signed legs. The ledger masters and vouchers
// here are synthetic (testdata/synthetic-r3-*.xml); none is client data.

func syntheticDutyBook(t *testing.T) *DutyLedgerBook {
	t.Helper()
	book, warnings, err := ParseDutyLedgers(readTestdata(t, "synthetic-r3-duty-ledgers.xml"))
	if err != nil {
		t.Fatalf("ParseDutyLedgers: %v", err)
	}
	if len(warnings) > 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if book == nil {
		t.Fatal("synthetic duty ledgers parsed to a nil book")
	}
	return book
}

func normalizeOneWith(t *testing.T, v RawVoucher, book *DutyLedgerBook) IngestVoucherRow {
	t.Helper()
	rows, err := Normalize(v, NormalizeOptions{DutyLedgers: book})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	return rows[0]
}

func hasFlag(row IngestVoucherRow, substr string) bool {
	for _, f := range row.NormalizationFlags {
		if strings.Contains(f, substr) {
			return true
		}
	}
	return false
}

func legNamed(t *testing.T, row IngestVoucherRow, name string) IngestVoucherLeg {
	t.Helper()
	for _, l := range row.VoucherLegs {
		if l.LedgerName == name {
			return l
		}
	}
	t.Fatalf("no leg %q in %+v", name, row.VoucherLegs)
	return IngestVoucherLeg{}
}

func assertSource(t *testing.T, row IngestVoucherRow, gross, tds, net float64) {
	t.Helper()
	if row.GrossAmount == nil || !nearly(*row.GrossAmount, gross, 0.001) || !nearly(row.InvoiceValue, gross, 0.001) {
		t.Errorf("gross = %v / invoice %v, want %v", row.GrossAmount, row.InvoiceValue, gross)
	}
	gotTDS := 0.0
	if row.TDSAmount != nil {
		gotTDS = *row.TDSAmount
	}
	if !nearly(gotTDS, tds, 0.001) {
		t.Errorf("tds = %v, want %v", gotTDS, tds)
	}
	if row.NetPayable == nil || !nearly(*row.NetPayable, net, 0.001) {
		t.Errorf("net payable = %v, want %v", row.NetPayable, net)
	}
}

func TestParseDutyLedgers_SyntheticMasters(t *testing.T) {
	book := syntheticDutyBook(t)
	if book.Len() != 32 {
		t.Errorf("Len = %d, want 32", book.Len())
	}
	m, ok := book.Lookup("  igst ITC   @ 18% ")
	if !ok {
		t.Fatal("lookup must fold case and whitespace")
	}
	if m.Parent != "GST Input" || m.TaxType != "GST" || m.DutyHead != "Integrated Tax" {
		t.Errorf("IGST ITC master = %+v", m)
	}
	if m, _ := book.Lookup("ECL - IGST"); m.Parent != "Duties & Taxes" {
		t.Errorf("parent = %q, want the decoded Duties & Taxes", m.Parent)
	}
	if _, ok := book.Lookup("Event Expenses IGST-18%"); ok {
		t.Error("an expense ledger is not a duty ledger")
	}
}

func TestParseDutyLedgers_EmptyExportIsNoEvidence(t *testing.T) {
	empty := `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION></COLLECTION></DATA></BODY></ENVELOPE>`
	book, _, err := ParseDutyLedgers([]byte(empty))
	if err != nil {
		t.Fatalf("ParseDutyLedgers: %v", err)
	}
	if book != nil {
		t.Fatalf("an empty export must be no evidence (nil book), got %d ledgers", book.Len())
	}
	if _, _, err := ParseDutyLedgers([]byte(`<ENVELOPE><HEADER><STATUS>0</STATUS></HEADER></ENVELOPE>`)); err == nil {
		t.Error("STATUS 0 must be an error, not an empty book")
	}
	if _, _, err := ParseDutyLedgers([]byte("  ")); err == nil {
		t.Error("an empty reply must be an error")
	}
}

func TestBuildDutyLedgerXML(t *testing.T) {
	body, err := BuildDutyLedgerXML("Synthetic Co")
	if err != nil {
		t.Fatalf("BuildDutyLedgerXML: %v", err)
	}
	for _, want := range []string{
		"<SVCURRENTCOMPANY>Synthetic Co</SVCURRENTCOMPANY>",
		"<TYPE>Ledger</TYPE>",
		"<CHILDOF>Duties &amp; Taxes</CHILDOF>",
		"<BELONGSTO>Yes</BELONGSTO>",
		"<FETCH>NAME, PARENT, TAXTYPE, GSTDUTYHEAD</FETCH>",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("envelope missing %q", want)
		}
	}
	if _, err := BuildDutyLedgerXML(""); err == nil {
		t.Error("an empty company must be refused")
	}
}

func TestClassifyLeg_EvidenceOrder(t *testing.T) {
	book := syntheticDutyBook(t)
	cases := []struct {
		name, ledger, gstClass string
		book                   *DutyLedgerBook
		role                   LegRole
		head, withholding      string
		evidence               LegEvidence
		flagged                bool
	}{
		// Contract R3 counterexamples.
		{"expense named IGST, no class", "Event Expense IGST @18%", "", nil, LegUncertain, "", "", EvidenceName, true},
		{"expense named IGST, unmatched class", "Event Expense IGST @18%", "Not Applicable", nil, LegUncertain, "", "", EvidenceName, true},
		{"expense named IGST, class without a head", "Event Expense IGST @18%", "GST@18", nil, LegUncertain, "", "", EvidenceName, true},
		{"expense named IGST, outside Duties & Taxes", "Event Expense IGST @18%", "", book, LegBase, "", "", EvidenceNotDutyLedger, true},
		{"processing charges are not CESS", "Processing Charges", "", nil, LegBase, "", "", EvidenceNone, false},
		{"processing charges with masters", "Processing Charges", "", book, LegBase, "", "", EvidenceNotDutyLedger, false},
		{"GST TDS by name", "TDS Payable Under CGST @1%", "", nil, LegWithholding, "", WithholdingTDS, EvidenceName, false},
		{"GST TDS with a CGST class", "TDS Payable Under CGST @1%", "CGST@1", nil, LegWithholding, "", WithholdingTDS, EvidenceName, true},
		{"GST TDS typed GST in its master", "TDS Payable Under CGST @ 1%", "", book, LegWithholding, "", WithholdingTDS, EvidenceDutyMaster, true},
		{"GST TDS typed TDS in its master", "TDS Payable Under SGST @ 1%", "", book, LegWithholding, "", WithholdingTDS, EvidenceDutyMaster, false},
		{"taxable purchase account", "Purchase (IGST)@18%", "", nil, LegBase, "", "", EvidenceName, false},
		{"taxable purchase account with masters", "Purchase (IGST)@18%", "", book, LegBase, "", "", EvidenceNotDutyLedger, false},
		// A name alone never makes a leg GST; the master or the class does.
		{"tax ledger named only", "IGST ITC @ 18%", "", nil, LegUncertain, "", "", EvidenceName, true},
		{"tax ledger in the master", "IGST ITC @ 18%", "", book, LegTax, "IGST", "", EvidenceDutyMaster, false},
		{"GST class prefix", "IGST @ 18%", "IGST@18", nil, LegTax, "IGST", "", EvidenceGSTClass, false},
		{"UTGST class is SGST", "UTGST Output", "UTGST@9", nil, LegTax, "SGST", "", EvidenceGSTClass, false},
		{"master outranks the class", "Event Expenses", "IGST@18", book, LegBase, "", "", EvidenceNotDutyLedger, true},
		{"GST master with no duty head", "GST Suspense", "", book, LegUncertain, "", "", EvidenceDutyMaster, true},
		{"other duty", "Mandi Tax Payable", "", book, LegOtherDuty, "", "", EvidenceDutyMaster, false},
		{"cess master", "Compensation Cess Input", "", book, LegTax, "CESS", "", EvidenceDutyMaster, false},
		{"TCS master", "TCS on Purchase 206C(1H)", "", book, LegWithholding, "", WithholdingTCS, EvidenceDutyMaster, false},
		{"TCS by name", "TCS on Purchase", "", nil, LegWithholding, "", WithholdingTCS, EvidenceName, false},
		{"rounding", "Round Off", "", nil, LegRounding, "", "", EvidenceName, false},
		{"discount", "Discount Received", "", book, LegDiscount, "", "", EvidenceName, false},
		{"freight", "Freight Inward", "", nil, LegFreight, "", "", EvidenceName, false},
		{"freight named IGST, no evidence", "Freight IGST", "", nil, LegUncertain, "", "", EvidenceName, true},
		{"freight named IGST, outside Duties & Taxes", "Freight IGST", "", book, LegFreight, "", "", EvidenceName, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := classifyLeg(LedgerEntry{LedgerName: tc.ledger, GSTClass: tc.gstClass, Amount: -100}, tc.book)
			if d.role != tc.role || d.head != tc.head || d.withholding != tc.withholding || d.evidence != tc.evidence {
				t.Errorf("classifyLeg(%q, %q) = role %s head %q withholding %q evidence %s; want %s %q %q %s",
					tc.ledger, tc.gstClass, d.role, d.head, d.withholding, d.evidence, tc.role, tc.head, tc.withholding, tc.evidence)
			}
			if got := len(d.flags) > 0; got != tc.flagged {
				t.Errorf("flagged = %v (%v), want %v", got, d.flags, tc.flagged)
			}
		})
	}
}

func TestHeadFromNameTokens_WholeWordsOnly(t *testing.T) {
	cases := map[string]string{
		"Processing Charges":          "",
		"Exhibition Expenses GST-18%": "",
		"Big Stall Charges":           "",
		"Tds on Contractor - 1%":      "",
		"OutputIGST 18%":              "IGST",
		"IGSTPAYABLE":                 "IGST",
		"IGST ITC @ 18%":              "IGST",
		"Event Expenses IGST-18%":     "IGST",
		"UTGST Output":                "SGST",
		"Compensation Cess":           "CESS",
		"GST Cess Input":              "CESS",
	}
	for name, want := range cases {
		if got := headFromNameTokens(name); got != want {
			t.Errorf("headFromNameTokens(%q) = %q, want %q", name, got, want)
		}
	}
	if got := classifyTaxLedger("", "IGST ITC @ 18%"); got != "" {
		t.Errorf("classifyTaxLedger reads the GST class only; got %q from a name", got)
	}
}

// netPartyTDSVoucher is the contract's counterexample: net party 479,700,
// true IGST 73,800, TDS 4,100 and nothing else. sign is the party leg's sign.
func netPartyTDSVoucher(t *testing.T, sign float64) RawVoucher {
	t.Helper()
	return RawVoucher{
		GUID: "r3-net-party", Date: parseDate(t, "2026-06-20"),
		VoucherType: "Purchase", VoucherNumber: "SYN/R3/010",
		PartyLedgerName: "Synthetic Stalls", PartyGSTIN: "27AAAAA0000A1Z5",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Synthetic Stalls", Amount: sign * 479700, IsPartyLedger: true,
				BillAllocations: []BillRef{{Name: "SYN/R3/010", Amount: sign * 479700, BillType: "New Ref"}}},
			{LedgerName: "Event Expense IGST @18%", Amount: -sign * 410000, GSTClass: "Not Applicable"},
			{LedgerName: "IGST ITC @ 18%", Amount: -sign * 73800},
			{LedgerName: "TDS on Contract 194C", Amount: sign * 4100},
		},
	}
}

func TestNormalize_NetPartyAndTDSEstablishGross(t *testing.T) {
	book := syntheticDutyBook(t)
	for _, tc := range []struct {
		name string
		sign float64
	}{{"party credited (Tally export signs)", 1}, {"party debited (G3 fixture signs)", -1}} {
		t.Run(tc.name, func(t *testing.T) {
			row := normalizeOneWith(t, netPartyTDSVoucher(t, tc.sign), book)
			// Not gross 479,700 / taxable 405,900: gross is never the net payable.
			assertValues(t, row, 483800, 410000, 73800, 0, 0)
			assertSource(t, row, 483800, 4100, 479700)
			if hasFlag(row, "do not balance") {
				t.Errorf("balanced voucher flagged: %v", row.NormalizationFlags)
			}
			if !hasFlag(row, `"Event Expense IGST @18%" is named after IGST but is not a Duties & Taxes ledger`) {
				t.Errorf("the GST-named expense must be flagged: %v", row.NormalizationFlags)
			}
		})
	}
	t.Run("without masters a name is never GST", func(t *testing.T) {
		row := normalizeOneWith(t, netPartyTDSVoucher(t, 1), nil)
		// Both GST-named legs are uncertain: out of GST and taxable value,
		// still in the gross, and flagged.
		assertValues(t, row, 483800, 0, 0, 0, 0)
		assertSource(t, row, 483800, 4100, 479700)
		for _, want := range []string{"no Duties & Taxes ledger masters", `"IGST ITC @ 18%" is named after IGST with no duty-ledger`, `"Event Expense IGST @18%" is named after IGST with no duty-ledger`} {
			if !hasFlag(row, want) {
				t.Errorf("missing flag %q in %v", want, row.NormalizationFlags)
			}
		}
	})
}

func TestNormalize_SyntheticPurchaseTDSFixture(t *testing.T) {
	parsed, err := ParseDayBookV3(readTestdata(t, "synthetic-r3-purchase-tds.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(parsed.Vouchers) != 2 {
		t.Fatalf("vouchers = %d, want 2", len(parsed.Vouchers))
	}
	book := syntheticDutyBook(t)

	first := normalizeOneWith(t, parsed.Vouchers[0], book)
	if first.InvoiceNumber != "SYN/26-27/001" {
		t.Errorf("invoice number = %q", first.InvoiceNumber)
	}
	assertValues(t, first, 483800, 410000, 73800, 0, 0)
	assertSource(t, first, 483800, 4100, 479700)

	second := normalizeOneWith(t, parsed.Vouchers[1], book)
	// Processing Charges is the taxable value, never CESS; the two GST TDS
	// legs and the income-tax TDS are withheld, never CGST / SGST.
	assertValues(t, second, 11800, 10000, 0, 900, 900)
	assertSource(t, second, 11800, 300, 11500)
	if leg := legNamed(t, second, "Processing Charges"); leg.Classification.AgentRole != LegBase {
		t.Errorf("Processing Charges role = %s, want base", leg.Classification.AgentRole)
	}
	if leg := legNamed(t, second, "TDS Payable Under CGST @ 1%"); leg.Classification.Role != "tds" || leg.Classification.Head != nil {
		t.Errorf("GST TDS leg = %+v, want tds with no head", leg.Classification)
	}
}

func TestNormalize_LegsCarryRoleAndEvidence(t *testing.T) {
	row := normalizeOneWith(t, netPartyTDSVoucher(t, 1), syntheticDutyBook(t))
	if len(row.VoucherLegs) != 4 {
		t.Fatalf("legs = %d, want 4", len(row.VoucherLegs))
	}
	party := legNamed(t, row, "Synthetic Stalls")
	if !party.IsPartyLedger || party.Classification.Role != "party" || party.Amount != 479700 {
		t.Errorf("party leg = %+v", party)
	}
	tax := legNamed(t, row, "IGST ITC @ 18%")
	if tax.Classification.Role != "tax" || tax.Classification.Head == nil || *tax.Classification.Head != "igst" ||
		tax.Classification.Method != "duty_field" || tax.Classification.Evidence != EvidenceDutyMaster ||
		tax.Amount != -73800 {
		t.Errorf("tax leg = %+v", tax)
	}
	if tax.ParentGroup == nil || *tax.ParentGroup != "GST Input" || tax.PrimaryGroup == nil || *tax.PrimaryGroup != "Duties & Taxes" ||
		tax.TaxType == nil || *tax.TaxType != "GST" || tax.GSTDutyHead == nil || *tax.GSTDutyHead != "Integrated Tax" {
		t.Errorf("tax leg master fields = %+v", tax)
	}
	expense := legNamed(t, row, "Event Expense IGST @18%")
	if expense.Classification.Role != "base" || expense.Classification.Evidence != EvidenceNotDutyLedger ||
		expense.GSTClass == nil || *expense.GSTClass != "Not Applicable" {
		t.Errorf("expense leg = %+v", expense)
	}
	tds := legNamed(t, row, "TDS on Contract 194C")
	if tds.Classification.Role != "tds" || tds.Classification.AgentRole != LegWithholding {
		t.Errorf("TDS leg = %+v", tds)
	}

	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{`"gross_amount":483800`, `"tds_amount":4100`, `"net_payable":479700`, `"voucher_legs":[`, `"normalization_flags":[`, `"agent_role":"withholding"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("payload missing %s: %s", key, raw)
		}
	}
	if strings.Contains(string(raw), "tcs_amount") {
		t.Error("tcs_amount must be omitted when there is no TCS")
	}
}

func TestNormalize_NegativeTaxableNeverCountsTDSDiscountOrAdjustmentAsBase(t *testing.T) {
	book := syntheticDutyBook(t)
	t.Run("discount and TDS", func(t *testing.T) {
		// v0.1.44 fell back to |discount| = 500 as the taxable value.
		v := RawVoucher{
			GUID: "r3-negative", Date: parseDate(t, "2026-07-01"), VoucherType: "Purchase", VoucherNumber: "SYN/R3/020",
			PartyLedgerName: "Synthetic Vendor",
			LedgerEntries: []LedgerEntry{
				{LedgerName: "Synthetic Vendor", Amount: 1000, IsPartyLedger: true},
				{LedgerName: "IGST Input", Amount: -1800},
				{LedgerName: "Discount Received", Amount: 500},
				{LedgerName: "TDS on Contract 194C", Amount: 300},
			},
		}
		row := normalizeOneWith(t, v, book)
		assertValues(t, row, 1300, 0, 1800, 0, 0)
		assertSource(t, row, 1300, 300, 1000)
		if !hasFlag(row, "taxable value from the legs is negative (-500.00): sent as 0") {
			t.Errorf("flags = %v", row.NormalizationFlags)
		}
	})
	t.Run("base leg on the party side", func(t *testing.T) {
		v := RawVoucher{
			GUID: "r3-adjustment", Date: parseDate(t, "2026-07-01"), VoucherType: "Purchase", VoucherNumber: "SYN/R3/021",
			PartyLedgerName: "Synthetic Vendor",
			LedgerEntries: []LedgerEntry{
				{LedgerName: "Synthetic Vendor", Amount: 1000, IsPartyLedger: true},
				{LedgerName: "Purchase Returns", Amount: 200},
				{LedgerName: "IGST Input", Amount: -1200},
			},
		}
		row := normalizeOneWith(t, v, book)
		assertValues(t, row, 1000, 0, 1200, 0, 0)
		if leg := legNamed(t, row, "Purchase Returns"); leg.Classification.AgentRole != LegAdjustment || leg.Classification.Role != "base" {
			t.Errorf("party-side base leg = %+v, want an adjustment", leg.Classification)
		}
		if !hasFlag(row, `"Purchase Returns" (200.00) is on the party's side of the voucher: netted as an adjustment`) {
			t.Errorf("flags = %v", row.NormalizationFlags)
		}
	})
}

func TestNormalize_SignedCreditAndDebitNotes(t *testing.T) {
	book := syntheticDutyBook(t)
	cases := []struct {
		name                               string
		kind                               IngestKind
		v                                  RawVoucher
		invoice, taxable, igst, cgst, sgst float64
		flag                               string
	}{
		{
			name: "sales credit note", kind: IngestKindCreditNote,
			v: RawVoucher{GUID: "r3-cn", Date: parseDate(t, "2026-05-02"), VoucherType: "Credit Note", VoucherNumber: "SYN/CN/1",
				PartyLedgerName: "Synthetic Customer", LedgerEntries: []LedgerEntry{
					{LedgerName: "Synthetic Customer", Amount: 11800, IsPartyLedger: true},
					{LedgerName: "Sales Returns @ 18%", Amount: -10000},
					{LedgerName: "Output CGST @ 9%", Amount: -900},
					{LedgerName: "Output SGST @ 9%", Amount: -900},
				}},
			invoice: 11800, taxable: 10000, cgst: 900, sgst: 900,
		},
		{
			name: "purchase debit note", kind: IngestKindDebitNote,
			v: RawVoucher{GUID: "r3-dn", Date: parseDate(t, "2026-05-03"), VoucherType: "Debit Note", VoucherNumber: "SYN/DN/1",
				PartyLedgerName: "Synthetic Vendor", LedgerEntries: []LedgerEntry{
					{LedgerName: "Synthetic Vendor", Amount: -23600, IsPartyLedger: true},
					{LedgerName: "Purchase Returns @ 18%", Amount: 20000},
					{LedgerName: "IGST Input @ 18%", Amount: 3600},
				}},
			invoice: 23600, taxable: 20000, igst: 3600,
		},
		{
			name: "credit note with a reversed tax leg", kind: IngestKindCreditNote,
			v: RawVoucher{GUID: "r3-cn-reversal", Date: parseDate(t, "2026-05-04"), VoucherType: "Credit Note", VoucherNumber: "SYN/CN/2",
				PartyLedgerName: "Synthetic Customer", LedgerEntries: []LedgerEntry{
					{LedgerName: "Synthetic Customer", Amount: 11700, IsPartyLedger: true},
					{LedgerName: "Sales Returns @ 18%", Amount: -10000},
					{LedgerName: "Output CGST @ 9%", Amount: -900},
					{LedgerName: "Output SGST @ 9%", Amount: -900},
					{LedgerName: "CGST @ 9%", Amount: 100},
				}},
			invoice: 11700, taxable: 10000, cgst: 800, sgst: 900,
			flag: `"CGST @ 9%" (100.00) is a tax leg on the party's side: netted as a reversal`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := inferKind(tc.v); got != tc.kind {
				t.Fatalf("kind = %s, want %s", got, tc.kind)
			}
			row := normalizeOneWith(t, tc.v, book)
			assertValues(t, row, tc.invoice, tc.taxable, tc.igst, tc.cgst, tc.sgst)
			if hasFlag(row, "do not balance") {
				t.Errorf("balanced note flagged: %v", row.NormalizationFlags)
			}
			if tc.flag != "" && !hasFlag(row, tc.flag) {
				t.Errorf("missing flag %q in %v", tc.flag, row.NormalizationFlags)
			}
		})
	}
}

func TestNormalize_ThreeBillResidualSumsExactly(t *testing.T) {
	book := syntheticDutyBook(t)
	v := RawVoucher{
		GUID: "r3-three-bills", Date: parseDate(t, "2026-08-05"), VoucherType: "Purchase", VoucherNumber: "SYN/R3/030",
		PartyLedgerName: "Synthetic Vendor",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Synthetic Vendor", Amount: -1170, IsPartyLedger: true, BillAllocations: []BillRef{
				{Name: "B1", Amount: -390.01, BillType: "New Ref"},
				{Name: "B2", Amount: -389.99, BillType: "New Ref"},
				{Name: "B3", Amount: -390, BillType: "Agst Ref"},
			}},
			{LedgerName: "Purchase A/c", Amount: 1000},
			{LedgerName: "IGST Input", Amount: 180},
			{LedgerName: "TDS on Contract 194C", Amount: -10},
		},
	}
	rows, err := Normalize(v, NormalizeOptions{DutyLedgers: book})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	var gross, taxable, igst, tds, net float64
	for _, r := range rows {
		gross += r.InvoiceValue
		taxable += r.TaxableValue
		igst += r.IGST
		tds += *r.TDSAmount
		net += *r.NetPayable
		if *r.GrossAmount != r.InvoiceValue {
			t.Errorf("row %s gross_amount %v != invoice_value %v", r.InvoiceNumber, *r.GrossAmount, r.InvoiceValue)
		}
		if len(r.VoucherLegs) != 4 {
			t.Errorf("row %s carries %d legs, want the voucher's 4", r.InvoiceNumber, len(r.VoucherLegs))
		}
		if hasFlag(r, "bill allocations total") {
			t.Errorf("tying bills flagged: %v", r.NormalizationFlags)
		}
	}
	for _, c := range []struct {
		label     string
		got, want float64
	}{{"gross", gross, 1180}, {"taxable", taxable, 1000}, {"igst", igst, 180}, {"tds", tds, 10}, {"net", net, 1170}} {
		if !nearly(c.got, c.want, 0.0001) {
			t.Errorf("sum(%s) = %.4f, want exactly %.2f", c.label, c.got, c.want)
		}
	}

	t.Run("bills that don't tie to the party are flagged", func(t *testing.T) {
		v := v
		v.LedgerEntries = append([]LedgerEntry(nil), v.LedgerEntries...)
		v.LedgerEntries[0].BillAllocations = []BillRef{
			{Name: "B1", Amount: -390, BillType: "New Ref"},
			{Name: "B2", Amount: -390, BillType: "New Ref"},
			{Name: "", Amount: -390, BillType: "On Account"},
		}
		rows, err := Normalize(v, NormalizeOptions{DutyLedgers: book})
		if err != nil {
			t.Fatalf("Normalize: %v", err)
		}
		if len(rows) != 2 || !hasFlag(rows[0], "bill allocations total -780.00 but the party amount is -1170.00") {
			t.Fatalf("rows = %d, flags = %v", len(rows), rows[0].NormalizationFlags)
		}
		if !nearly(rows[0].InvoiceValue+rows[1].InvoiceValue, 1180, 0.0001) {
			t.Errorf("rows must still sum to the voucher gross: %v + %v", rows[0].InvoiceValue, rows[1].InvoiceValue)
		}
	})
}

func TestNormalize_ReverseChargeLiabilityStaysOutOfGST(t *testing.T) {
	v := RawVoucher{
		GUID: "r3-rcm", Date: parseDate(t, "2026-07-10"), VoucherType: "GST Purchase - RCM", VoucherNumber: "SYN/RCM/1",
		PartyLedgerName: "Synthetic Transport", ReverseCharge: true,
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Synthetic Transport", Amount: -10000, IsPartyLedger: true},
			{LedgerName: "Freight Inward", Amount: 10000},
			{LedgerName: "IGST Input", Amount: 1800},
			{LedgerName: "IGST", Amount: -1800},
		},
	}
	row := normalizeOneWith(t, v, syntheticDutyBook(t))
	assertValues(t, row, 11800, 10000, 1800, 0, 0)
	assertSource(t, row, 11800, 0, 10000)
	if !hasFlag(row, `"IGST" (-1800.00) is the reverse-charge liability`) || hasFlag(row, "do not balance") {
		t.Errorf("flags = %v", row.NormalizationFlags)
	}
}

func TestNormalize_InventoryBaseOnlyWhenLedgerLegsDoNotBalance(t *testing.T) {
	v := RawVoucher{
		GUID: "r3-items", Date: parseDate(t, "2026-04-10"), VoucherType: "GST Purchase", VoucherNumber: "SYN/R3/040",
		PartyLedgerName: "Synthetic Vendor",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Synthetic Vendor", Amount: -118000, IsPartyLedger: true},
			{LedgerName: "IGST @ 18%", Amount: 18000, GSTClass: "IGST@18"},
		},
		InventoryEntries: []InventoryEntry{
			{StockItem: "Synthetic Paper", Amount: 60000},
			{StockItem: "Synthetic Ink", Amount: 40000},
		},
	}
	row := normalizeOneWith(t, v, nil)
	assertValues(t, row, 118000, 100000, 18000, 0, 0)
	if !hasFlag(row, "taxable base 100000.00 taken from 2 inventory entries") || hasFlag(row, "do not balance") {
		t.Errorf("flags = %v", row.NormalizationFlags)
	}
}

func TestNormalize_UnbalancedLegsAreFlagged(t *testing.T) {
	v := RawVoucher{
		GUID: "r3-unbalanced", Date: parseDate(t, "2026-04-11"), VoucherType: "Purchase", VoucherNumber: "SYN/R3/041",
		PartyLedgerName: "Synthetic Vendor",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Synthetic Vendor", Amount: -11800, IsPartyLedger: true},
			{LedgerName: "Purchase A/c", Amount: 10000},
		},
	}
	row := normalizeOneWith(t, v, syntheticDutyBook(t))
	assertValues(t, row, 10000, 10000, 0, 0, 0)
	if !hasFlag(row, "legs do not balance: they establish 10000.00 net of TDS/TCS against a party amount of 11800.00") {
		t.Errorf("flags = %v", row.NormalizationFlags)
	}
}

func TestNormalize_LegsCarryTallyGroupsFromTheLedgerDirectory(t *testing.T) {
	// SYNTHETIC ledger masters: the purchase account sits in a sub-group of
	// Purchase Accounts; the party under Sundry Creditors.
	ledgers, _, err := ParseLedgerGSTINs([]byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <LEDGER NAME="Purchase A/c"><NAME>Purchase A/c</NAME><PARENT>Purchase - Local</PARENT></LEDGER>
  <LEDGER NAME="Synthetic Vendor"><NAME>Synthetic Vendor</NAME><PARENT>Sundry Creditors</PARENT><PARTYGSTIN>27AAAAA0000A1Z5</PARTYGSTIN></LEDGER>
</COLLECTION></DATA></BODY></ENVELOPE>`))
	if err != nil {
		t.Fatalf("ParseLedgerGSTINs: %v", err)
	}
	ledgers.ApplyGroupTree(map[string]string{
		"Purchase - Local":    "Purchase Accounts",
		"Purchase Accounts":   "\x04 Primary",
		"Sundry Creditors":    "Current Liabilities",
		"Current Liabilities": "",
	})
	v := RawVoucher{
		GUID: "r3-groups", Date: parseDate(t, "2026-04-12"), VoucherType: "Purchase", VoucherNumber: "SYN/R3/050",
		PartyLedgerName: "Synthetic Vendor",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Synthetic Vendor", Amount: 11800, IsPartyLedger: true},
			{LedgerName: "Purchase A/c", Amount: -10000},
			{LedgerName: "IGST Input", Amount: -1800},
		},
	}
	rows, err := Normalize(v, NormalizeOptions{Ledgers: ledgers, DutyLedgers: syntheticDutyBook(t)})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	row := rows[0]
	purchase := legNamed(t, row, "Purchase A/c")
	if purchase.ParentGroup == nil || *purchase.ParentGroup != "Purchase - Local" || purchase.PrimaryGroup == nil || *purchase.PrimaryGroup != "Purchase Accounts" {
		t.Errorf("purchase leg groups = %v / %v", purchase.ParentGroup, purchase.PrimaryGroup)
	}
	party := legNamed(t, row, "Synthetic Vendor")
	if party.PrimaryGroup == nil || *party.PrimaryGroup != "Current Liabilities" {
		t.Errorf("party leg primary group = %v", party.PrimaryGroup)
	}
	// A duty ledger's groups come from its own master.
	if tax := legNamed(t, row, "IGST Input"); tax.ParentGroup == nil || *tax.ParentGroup != "GST Input" || *tax.PrimaryGroup != "Duties & Taxes" {
		t.Errorf("tax leg groups = %v / %v", tax.ParentGroup, tax.PrimaryGroup)
	}
	if row.VendorGSTIN != nil && *row.VendorGSTIN != "27AAAAA0000A1Z5" {
		t.Errorf("vendor GSTIN = %v", *row.VendorGSTIN)
	}
}
