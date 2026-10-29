package tally

import "testing"

// QA v17 G3: the purchase lane stored the TDS leg as taxable value and the
// gross as tax for vouchers with TDS legs or GST-named expense ledgers
// (Vinayak VE-26/27-76 taxable 1,400 / tax 1,65,200; Europa 03/04 tax
// 8,33,498 / 13,89,167 on taxable 7,064 / 11,774). The raw Tally XML goes
// through the same parse + Normalize the purchase sync runs, with the
// company's Duties & Taxes masters (QA v18 R3: only those make a leg GST).
func TestNormalize_G3PurchaseLegMappingFromFixture(t *testing.T) {
	parsed, err := ParseDayBookV3(readTestdata(t, "voucher-v3-purchase-tds-legs.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	byGUID := map[string]RawVoucher{}
	for _, v := range parsed.Vouchers {
		byGUID[v.GUID] = v
	}
	book := syntheticDutyBook(t)
	cases := []struct {
		guid, invoiceNumber                string
		invoice, taxable, igst, cgst, sgst float64
		tds                                float64
		namedExpense                       string
	}{
		// Party 1,63,800 + TDS 1,400 = 1,65,200; 2B: 1,40,000 / 25,200.
		{"g3-vinayak-ve-76", "VE-26/27-76", 165200, 140000, 25200, 0, 0, 1400, "Exhibition Expenses IGST-18%"},
		// Party 4,79,700 + TDS 4,100 = 4,83,800; 2B: 4,10,000 / 73,800.
		{"g3-dd-001", "DD/26-27/001", 483800, 410000, 73800, 0, 0, 4100, "Event Expenses IGST-18%"},
		// "Material Purchase IGST 18%" is the expense next to "IGST Input".
		{"g3-europa-03", "03", 833498.90, 706355, 127143.90, 0, 0, 7064, "Material Purchase IGST 18%"},
		{"g3-europa-04", "04", 1389166.80, 1177260, 211906.80, 0, 0, 11774, "Material Purchase IGST 18%"},
		// No TDS, real tax ledgers: unchanged.
		{"g3-plain", "SUP/090", 59000, 50000, 0, 4500, 4500, 0, ""},
		// Only a GST-named expense ledger, outside Duties & Taxes: it is the
		// taxable value and the bill carries no GST.
		{"g3-named-only", "SUP/091", 100000, 100000, 0, 0, 0, 1000, "Exhibition Expenses IGST-18%"},
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
			row := normalizeOneWith(t, v, book)
			if row.InvoiceNumber != tc.invoiceNumber {
				t.Errorf("invoice number = %q, want %q", row.InvoiceNumber, tc.invoiceNumber)
			}
			if row.VendorGSTIN == nil || *row.VendorGSTIN != v.PartyGSTIN {
				t.Errorf("vendor GSTIN = %v, want %s", row.VendorGSTIN, v.PartyGSTIN)
			}
			assertValues(t, row, tc.invoice, tc.taxable, tc.igst, tc.cgst, tc.sgst)
			assertSource(t, row, tc.invoice, tc.tds, tc.invoice-tc.tds)
			if tax := row.IGST + row.CGST + row.SGST + row.CESS; tax > row.TaxableValue {
				t.Errorf("GST %v is larger than taxable %v", tax, row.TaxableValue)
			}
			if tc.namedExpense != "" && !hasFlag(row, `"`+tc.namedExpense+`" is named after IGST but is not a Duties & Taxes ledger`) {
				t.Errorf("GST-named expense not flagged: %v", row.NormalizationFlags)
			}
			if hasFlag(row, "do not balance") {
				t.Errorf("balanced voucher flagged: %v", row.NormalizationFlags)
			}
		})
	}
}

// A ledger whose GST head leads its name (blocked credit "IGST Exp 17(5)",
// GST written off to expense, "IGST on X", the cash ledger "ECL - IGST") is
// tax when Tally keeps it under Duties & Taxes as a GST ledger, next to a
// real "IGST Input" ledger. Outside Duties & Taxes it is never tax, whatever
// its name (QA v18 R3).
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
	withMasters := syntheticDutyBook(t)
	withoutBlocked := NewDutyLedgerBook([]DutyLedger{
		{Name: "IGST Input @ 18%", Parent: "GST Input", TaxType: "GST", DutyHead: "Integrated Tax"},
		{Name: "TDS on Contract 194C", Parent: "TDS Payable", TaxType: "TDS"},
	})
	for _, tc := range cases {
		voucher := RawVoucher{
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
		t.Run(tc.name+" under Duties & Taxes", func(t *testing.T) {
			row := normalizeOneWith(t, voucher, withMasters)
			assertValues(t, row, 129800, 110000, 19800, 0, 0)
			assertSource(t, row, 129800, 1300, 128500)
		})
		t.Run(tc.name+" outside Duties & Taxes", func(t *testing.T) {
			row := normalizeOneWith(t, voucher, withoutBlocked)
			assertValues(t, row, 129800, 111800, 18000, 0, 0)
			if !hasFlag(row, `"`+tc.blocked+`" is named after IGST but is not a Duties & Taxes ledger`) {
				t.Errorf("flags = %v", row.NormalizationFlags)
			}
		})
	}
}

// Head-first names were tax ledgers by name in v0.1.46. Under R3 no name
// makes a leg GST: without masters each is uncertain (kept out of GST and
// taxable value, flagged); with its Duties & Taxes master it is tax.
func TestClassifyLeg_G3HeadFirstNamesNeedMasterEvidence(t *testing.T) {
	cases := map[string]string{
		"IGST Exp 17(5)":                 "IGST",
		"CGST Exp - Revenue Expenditure": "CGST",
		"Igst Expense":                   "IGST",
		"IGST on Freight":                "IGST",
		"ECL - IGST":                     "IGST",
		"Disallowed CGST":                "CGST",
		"Integrated Tax IGST":            "IGST",
		"Exhibition Expenses IGST-18%":   "IGST",
		"Material Purchase IGST 18%":     "IGST",
		"Int on Late Payment of IGST":    "IGST",
	}
	dutyHead := map[string]string{"IGST": "Integrated Tax", "CGST": "Central Tax"}
	for name, head := range cases {
		if d := classifyLeg(LedgerEntry{LedgerName: name}, nil); d.role != LegUncertain || len(d.flags) == 0 {
			t.Errorf("no masters: classifyLeg(%q) = %s %v, want uncertain and flagged", name, d.role, d.flags)
		}
		book := NewDutyLedgerBook([]DutyLedger{{Name: name, TaxType: "GST", DutyHead: dutyHead[head]}})
		if d := classifyLeg(LedgerEntry{LedgerName: name}, book); d.role != LegTax || d.head != head {
			t.Errorf("GST master: classifyLeg(%q) = %s %q, want tax %s", name, d.role, d.head, head)
		}
	}
	if d := classifyLeg(LedgerEntry{LedgerName: "TDS on Contract 194C"}, nil); d.role != LegWithholding {
		t.Errorf("TDS on Contract 194C = %s, want withholding", d.role)
	}
}
