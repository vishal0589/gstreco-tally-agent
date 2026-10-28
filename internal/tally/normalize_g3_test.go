package tally

import "testing"

// QA v17 G3: the purchase lane stored the TDS leg as taxable value and the
// gross as tax for vouchers with TDS legs or GST-named expense ledgers
// (Vinayak VE-26/27-76 taxable 1,400 / tax 1,65,200; Europa 03/04 tax
// 8,33,498 / 13,89,167 on taxable 7,064 / 11,774). The raw Tally XML goes
// through the same parse + Normalize the purchase sync runs.
func TestNormalize_G3PurchaseLegMappingFromFixture(t *testing.T) {
	parsed, err := ParseDayBookV3(readTestdata(t, "voucher-v3-purchase-tds-legs.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	byGUID := map[string]RawVoucher{}
	for _, v := range parsed.Vouchers {
		byGUID[v.GUID] = v
	}
	cases := []struct {
		guid, invoiceNumber                string
		invoice, taxable, igst, cgst, sgst float64
	}{
		// Party 1,63,800 + TDS 1,400 = 1,65,200; 2B: 1,40,000 / 25,200.
		{"g3-vinayak-ve-76", "VE-26/27-76", 165200, 140000, 25200, 0, 0},
		// Party 4,79,700 + TDS 4,100 = 4,83,800; 2B: 4,10,000 / 73,800.
		{"g3-dd-001", "DD/26-27/001", 483800, 410000, 73800, 0, 0},
		// "Material Purchase IGST 18%" is the expense next to "IGST Input".
		{"g3-europa-03", "03", 833498.90, 706355, 127143.90, 0, 0},
		{"g3-europa-04", "04", 1389166.80, 1177260, 211906.80, 0, 0},
		// No TDS, real tax ledgers: unchanged.
		{"g3-plain", "SUP/090", 59000, 50000, 0, 4500, 4500},
		// Only a GST-named expense ledger: it cannot be the whole invoice's
		// tax, so it is the taxable value and the bill carries no GST.
		{"g3-named-only", "SUP/091", 100000, 100000, 0, 0, 0},
	}
	if len(parsed.Vouchers) != len(cases) {
		t.Fatalf("vouchers = %d, want %d", len(parsed.Vouchers), len(cases))
	}
	for _, tc := range cases {
		t.Run(tc.guid, func(t *testing.T) {
			v, ok := byGUID[tc.guid]
			if !ok {
				t.Fatalf("voucher %s not in fixture", tc.guid)
			}
			row := normalizeOne(t, v)
			if row.InvoiceNumber != tc.invoiceNumber {
				t.Errorf("invoice number = %q, want %q", row.InvoiceNumber, tc.invoiceNumber)
			}
			if row.VendorGSTIN == nil || *row.VendorGSTIN != v.PartyGSTIN {
				t.Errorf("vendor GSTIN = %v, want %s", row.VendorGSTIN, v.PartyGSTIN)
			}
			assertValues(t, row, tc.invoice, tc.taxable, tc.igst, tc.cgst, tc.sgst)
			if tax := row.IGST + row.CGST + row.SGST + row.CESS; tax > row.TaxableValue {
				t.Errorf("GST %v is larger than taxable %v", tax, row.TaxableValue)
			}
		})
	}
}

// A leg whose GST head leads its name is a tax ledger, as on the server's
// journal lane (GST_Reco 9406fac): blocked credit ("IGST Exp 17(5)"), GST
// written off to expense ("CGST Exp"), "IGST on X" and the cash ledger ("ECL
// - IGST"). Next to a real "IGST Input" ledger it is still tax, not the
// taxable value; only a head after other words names the thing taxed.
func TestNormalize_G3HeadFirstTaxLedgerNextToATaxLedger(t *testing.T) {
	cases := []struct {
		name, blocked string
	}{
		{"blocked credit", "IGST Exp 17(5)"},
		{"blocked credit spelled out", "IGST Blocked Credit"},
		{"GST written off to expense", "IGST Exp - Revenue Expenditure"},
		{"tax on a supply", "IGST on Hotel Stay"},
		{"ineligible credit", "Ineligible IGST"},
		{"electronic cash ledger", "ECL - IGST"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := RawVoucher{
				GUID:            "g3-mixed-" + tc.name,
				Date:            parseDate(t, "2026-09-20"),
				VoucherType:     "Purchase",
				VoucherNumber:   "VE-26/27-80",
				PartyLedgerName: "Vendor One",
				PartyGSTIN:      "07AAAAA0000A1Z5",
				LedgerEntries: []LedgerEntry{
					{LedgerName: "Vendor One", Amount: -128500, IsPartyLedger: true},
					{LedgerName: "Exhibition Expenses IGST-18%", Amount: 100000},
					{LedgerName: "IGST Input @ 18%", Amount: 18000},
					{LedgerName: "Hotel Stay", Amount: 10000},
					{LedgerName: tc.blocked, Amount: 1800},
					{LedgerName: "TDS on Contract 194C", Amount: -1300},
				},
			}
			row := normalizeOne(t, v)
			assertValues(t, row, 129800, 110000, 19800, 0, 0)
		})
	}
}

func TestClassifyTaxLedger_G3HeadFirstNamesAreTaxLedgers(t *testing.T) {
	cases := map[string]taxLedgerGrade{
		"IGST Exp 17(5)":                 taxLedger,
		"CGST Exp - Revenue Expenditure": taxLedger,
		"Igst Expense":                   taxLedger,
		"IGST on Freight":                taxLedger,
		"ECL - IGST":                     taxLedger,
		"Disallowed CGST":                taxLedger,
		"Integrated Tax IGST":            taxLedger,
		"Exhibition Expenses IGST-18%":   namedTaxLedger,
		"Material Purchase IGST 18%":     namedTaxLedger,
		"Int on Late Payment of IGST":    namedTaxLedger,
		"TDS on Contract 194C":           notTaxLedger,
	}
	for name, want := range cases {
		if _, got := gradeTaxLedger("", name); got != want {
			t.Errorf("gradeTaxLedger(%q) grade = %v, want %v", name, got, want)
		}
	}
}
