package tally

import "testing"

// tdsPurchase is a purchase voucher with a TDS leg, as an exhibitions client
// books it (QA v16 F4): the expense ledger is named after the GST that goes
// with it, the tax sits in an "IGST ITC" ledger and the TDS is credited next
// to the supplier. Names and GSTINs are made up; amounts are the real ones.
func tdsPurchase(t *testing.T, number, expenseName, taxName, tdsName string, party, expense, tax, tds float64) RawVoucher {
	t.Helper()
	return RawVoucher{
		GUID:            "f4-" + number,
		IsInvoice:       true,
		Date:            parseDate(t, "2026-06-20"),
		VoucherType:     "Purchase",
		VoucherNumber:   number,
		Reference:       number,
		PartyLedgerName: "Vendor One",
		PartyGSTIN:      "09AAAAA0000A1Z5",
		LedgerEntries: []LedgerEntry{
			{
				LedgerName:    "Vendor One",
				Amount:        party,
				IsPartyLedger: true,
				BillAllocations: []BillRef{
					{Name: number, Amount: party, BillType: "New Ref"},
				},
			},
			{LedgerName: expenseName, Amount: -expense, GSTClass: "Not Applicable"},
			{LedgerName: taxName, Amount: -tax, GSTClass: "Not Applicable"},
			{LedgerName: tdsName, Amount: tds},
		},
	}
}

func normalizeOne(t *testing.T, v RawVoucher) IngestVoucherRow {
	t.Helper()
	rows, err := Normalize(v, NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	return rows[0]
}

func assertValues(t *testing.T, row IngestVoucherRow, invoice, taxable, igst, cgst, sgst float64) {
	t.Helper()
	if !nearly(row.InvoiceValue, invoice, 0.001) || !nearly(row.TaxableValue, taxable, 0.001) {
		t.Errorf("invoice/taxable = %v/%v, want %v/%v", row.InvoiceValue, row.TaxableValue, invoice, taxable)
	}
	if !nearly(row.IGST, igst, 0.001) || !nearly(row.CGST, cgst, 0.001) || !nearly(row.SGST, sgst, 0.001) || row.CESS != 0 {
		t.Errorf("igst/cgst/sgst/cess = %v/%v/%v/%v, want %v/%v/%v/0", row.IGST, row.CGST, row.SGST, row.CESS, igst, cgst, sgst)
	}
}

// QA v16 F4: v0.1.43 read the GST-named expense ledger as IGST, so the tax
// came to more than the invoice and the TDS leg became the taxable value
// (taxable 4,100 / IGST 4,83,800 and taxable 1,400 / IGST 1,65,200). Under
// QA v18 R3 the "IGST ITC" ledger is tax because its Duties & Taxes master
// says so, and the expense is the base because it is not a duty ledger.
func TestNormalize_TDSPurchaseWithGSTNamedExpenseLedger(t *testing.T) {
	book := syntheticDutyBook(t)
	cases := []struct {
		name                                  string
		number, expenseName, taxName, tdsName string
		party, expense, tax, tds              float64
	}{
		{
			name:   "June voucher, 1% TDS on 4,10,000",
			number: "INV/26-27/001", expenseName: "Event Expenses IGST-18%",
			taxName: "IGST ITC @ 18%", tdsName: "Tds on Contractor - 1%",
			party: 479700, expense: 410000, tax: 73800, tds: 4100,
		},
		{
			name:   "September voucher, 1% TDS on 1,40,000",
			number: "VE-26/27-076", expenseName: "Event Expenses IGST-18%",
			taxName: "IGST ITC -18%", tdsName: "TDS on Contractor - 1%",
			party: 163800, expense: 140000, tax: 25200, tds: 1400,
		},
		{
			// "Expenses GST" squeezed to EXPENSESGST used to read as SGST.
			name:   "expense ledger whose words only run into a head",
			number: "INV/26-27/002", expenseName: "Event Expenses GST-18%",
			taxName: "IGST ITC @ 18%", tdsName: "Tds on Contractor - 1%",
			party: 479700, expense: 410000, tax: 73800, tds: 4100,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := normalizeOneWith(t, tdsPurchase(t, tc.number, tc.expenseName, tc.taxName, tc.tdsName, tc.party, tc.expense, tc.tax, tc.tds), book)
			// The invoice is the supplier's: the party amount plus the TDS.
			assertValues(t, row, tc.expense+tc.tax, tc.expense, tc.tax, 0, 0)
			assertSource(t, row, tc.expense+tc.tax, tc.tds, tc.party)
		})
	}
}

// Without the duty-ledger masters the "IGST ITC" ledger is only named after
// IGST: it is never counted as GST, but kept in the gross and flagged.
func TestNormalize_TDSPurchaseWithoutMastersKeepsNamedTaxOutOfGST(t *testing.T) {
	v := tdsPurchase(t, "INV/26-27/002", "Event Expenses GST-18%", "IGST ITC @ 18%", "Tds on Contractor - 1%", 479700, 410000, 73800, 4100)
	row := normalizeOne(t, v)
	assertValues(t, row, 483800, 410000, 0, 0, 0)
	assertSource(t, row, 483800, 4100, 479700)
	if !hasFlag(row, `"IGST ITC @ 18%" is named after IGST with no duty-ledger or GST-class evidence`) {
		t.Errorf("flags = %v", row.NormalizationFlags)
	}
}

func TestNormalize_TDSIsNeitherTaxNorTaxable(t *testing.T) {
	v := RawVoucher{
		GUID:            "f4-gst-tds",
		IsInvoice:       true,
		Date:            parseDate(t, "2026-04-22"),
		VoucherType:     "GST - Purchase Invoices (Registered)",
		VoucherNumber:   "RIC/26-27/007",
		PartyLedgerName: "Vendor Two",
		PartyGSTIN:      "06AAAAA0000A1Z5",
		LedgerEntries: []LedgerEntry{
			{
				LedgerName: "Vendor Two",
				// Net of all three TDS legs: 57,997 - 492 - 246 - 246.
				Amount:        57013,
				IsPartyLedger: true,
				BillAllocations: []BillRef{
					{Name: "RIC/26-27/007", Amount: 57013, BillType: "New Ref"},
				},
			},
			{LedgerName: "Purchase Raw Material Local - GST Registered", Amount: -46750},
			{LedgerName: "Purchase Raw Material Local - GST Registered", Amount: -2400},
			{LedgerName: "CGST Input Credit", Amount: -4423.5, GSTClass: "CGST@9"},
			{LedgerName: "SGST Input Credit", Amount: -4423.5, GSTClass: "SGST@9"},
			{LedgerName: "Tds Payable (94C) @ 1% (Non-Company)", Amount: 492},
			// GST TDS under section 51 names a head; it is still TDS.
			{LedgerName: "TDS Payable Under CGST @ 1%", Amount: 246},
			{LedgerName: "TDS Payable Under SGST @ 1%", Amount: 246},
		},
	}
	// The GST class alone decides without masters; the masters decide with
	// them. The answer is the same.
	for _, book := range []*DutyLedgerBook{nil, syntheticDutyBook(t)} {
		row := normalizeOneWith(t, v, book)
		assertValues(t, row, 57997, 49150, 0, 4423.5, 4423.5)
		assertSource(t, row, 57997, 984, 57013)
	}
}

func TestNormalize_BlockedCreditLedgerIsTaxOnlyWithItsMaster(t *testing.T) {
	// Blocked credit booked straight to an expense-style tax ledger, the only
	// GST leg on the voucher: tax when its Duties & Taxes master is GST.
	v := tdsPurchase(t, "INV/26-27/003", "Event Expenses", "IGST Exp 17(5)", "Round Off", 118000, 100000, 18000, 0)
	row := normalizeOneWith(t, v, syntheticDutyBook(t))
	assertValues(t, row, 118000, 100000, 18000, 0, 0)
	// Its name alone is no evidence: uncertain, out of GST, still in the gross.
	row = normalizeOne(t, v)
	assertValues(t, row, 118000, 100000, 0, 0, 0)
	if !hasFlag(row, `"IGST Exp 17(5)" is named after IGST with no duty-ledger`) {
		t.Errorf("flags = %v", row.NormalizationFlags)
	}
}

func TestNormalize_GSTNamedLegsWithoutEvidenceAreNeverTax(t *testing.T) {
	// Two GST-named legs and no tax ledger. v0.1.46 guessed the smaller one
	// was the tax; under QA v18 R3 a name never makes a leg GST. Outside
	// Duties & Taxes both are the base; with no masters both are uncertain.
	v := RawVoucher{
		GUID:            "f4-two-named",
		IsInvoice:       true,
		Date:            parseDate(t, "2026-07-01"),
		VoucherType:     "Purchase",
		VoucherNumber:   "INV/26-27/004",
		PartyLedgerName: "Vendor One",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Vendor One", Amount: 105000, IsPartyLedger: true},
			{LedgerName: "Prchase IGST 5%", Amount: -5000},
			{LedgerName: "Event Expenses IGST 5%", Amount: -100000},
		},
	}
	row := normalizeOneWith(t, v, syntheticDutyBook(t))
	assertValues(t, row, 105000, 105000, 0, 0, 0)
	row = normalizeOne(t, v)
	assertValues(t, row, 105000, 0, 0, 0, 0)
	for _, name := range []string{"Prchase IGST 5%", "Event Expenses IGST 5%"} {
		if !hasFlag(row, `"`+name+`" is named after IGST with no duty-ledger`) {
			t.Errorf("%s not flagged: %v", name, row.NormalizationFlags)
		}
	}
}

func TestNormalize_GSTClassOnAnExpenseLedgerGivesWayToTheDutyMaster(t *testing.T) {
	v := RawVoucher{
		GUID:            "f4-class-tagged",
		IsInvoice:       true,
		Date:            parseDate(t, "2026-07-01"),
		VoucherType:     "Purchase",
		VoucherNumber:   "INV/26-27/005",
		PartyLedgerName: "Vendor One",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Vendor One", Amount: 117000, IsPartyLedger: true},
			{LedgerName: "Event Expenses", Amount: -100000, GSTClass: "IGST@18"},
			{LedgerName: "IGST Input Credit", Amount: -18000},
			{LedgerName: "TDS on Contractor - 1%", Amount: 1000},
		},
	}
	row := normalizeOneWith(t, v, syntheticDutyBook(t))
	assertValues(t, row, 118000, 100000, 18000, 0, 0)
	if !hasFlag(row, `"Event Expenses" has GST class "IGST@18" but is not a Duties & Taxes ledger`) {
		t.Errorf("flags = %v", row.NormalizationFlags)
	}
	// Without masters the GST class is the only evidence and it calls the
	// expense IGST: the row says the tax outweighs the taxable value.
	if row = normalizeOne(t, v); !hasFlag(row, "exceeds the taxable value") {
		t.Errorf("no-masters flags = %v", row.NormalizationFlags)
	}
}

func TestNormalize_TDSConsolidatedBillsShareTheGrossInvoice(t *testing.T) {
	v := RawVoucher{
		GUID:            "f4-consolidated",
		IsInvoice:       true,
		Date:            parseDate(t, "2026-08-05"),
		VoucherType:     "Purchase",
		VoucherNumber:   "PV-24",
		PartyLedgerName: "Vendor Three",
		LedgerEntries: []LedgerEntry{
			{
				LedgerName:    "Vendor Three",
				Amount:        117000,
				IsPartyLedger: true,
				BillAllocations: []BillRef{
					{Name: "INV/26-27/024", Amount: 58500, BillType: "New Ref"},
					{Name: "INV/26-27/025", Amount: 58500, BillType: "New Ref"},
				},
			},
			{LedgerName: "Event Expenses IGST-18%", Amount: -100000},
			{LedgerName: "IGST ITC @ 18%", Amount: -18000},
			{LedgerName: "TDS on Contractor - 1%", Amount: 1000},
		},
	}
	rows, err := Normalize(v, NormalizeOptions{DutyLedgers: syntheticDutyBook(t)})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		assertValues(t, row, 59000, 50000, 9000, 0, 0)
		assertSource(t, row, 59000, 500, 58500)
	}
}

// The journal lane's party guess stays broad: a GST class head, a head read
// word by word, or the word GST keeps a ledger from being guessed as the
// party. Taxable purchase accounts named after their tax are not GST ledgers.
func TestNamesGSTLedger_KeepsTheJournalPartyGuessBroad(t *testing.T) {
	cases := map[string]bool{
		"Exhibition Expenses GST-18%": true,
		"CGST Input Credit":           true,
		"OutputIGST 18%":              true,
		"Compensation Cess":           true,
		"Terminal Handling Charges":   false,
		"Processing Charges":          false,
		"Purchase (IGST)@18%":         false,
	}
	for name, want := range cases {
		if got := namesGSTLedger("", name); got != want {
			t.Errorf("namesGSTLedger(%q) = %v, want %v", name, got, want)
		}
	}
	if !namesGSTLedger("IGST@18", "Event Expenses") {
		t.Error("a GST class head must count")
	}
}
