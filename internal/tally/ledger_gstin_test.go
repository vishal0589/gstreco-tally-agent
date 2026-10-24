package tally

import (
	"strings"
	"testing"
	"time"
)

const ledgerGSTINResponse = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <LEDGER NAME="Delhi Cargo Service Center Pvt.Ltd-ADVANCE">
      <NAME>Delhi Cargo Service Center Pvt.Ltd-ADVANCE</NAME>
      <PARTYGSTIN>07zzyab1234c1z5</PARTYGSTIN>
    </LEDGER>
    <LEDGER NAME="Carico Systems Pvt.Ltd-Advance">
      <NAME>Carico Systems Pvt.Ltd-Advance</NAME>
      <PARTYGSTIN></PARTYGSTIN>
      <LEDGSTREGDETAILS.LIST>
        <APPLICABLEFROM>20170701</APPLICABLEFROM>
        <GSTIN>27ZZYCD5678E1Z1</GSTIN>
      </LEDGSTREGDETAILS.LIST>
      <LEDGSTREGDETAILS.LIST>
        <APPLICABLEFROM>20240401</APPLICABLEFROM>
        <GSTIN>27ZZYCD5678E2Z0</GSTIN>
      </LEDGSTREGDETAILS.LIST>
    </LEDGER>
    <LEDGER NAME="Attribute Only Ledger">
      <GSTIN>09ZZYEF9012G1Z3</GSTIN>
    </LEDGER>
    <LEDGER NAME="Unregistered Supplier">
      <NAME>Unregistered Supplier</NAME>
      <PARTYGSTIN>NA</PARTYGSTIN>
    </LEDGER>
    <LEDGER NAME="Freight Inward">
      <NAME>Freight Inward</NAME>
    </LEDGER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

func TestParseLedgerGSTINs_ReadsEveryGSTINField(t *testing.T) {
	ledgers, warnings, err := ParseLedgerGSTINs([]byte(ledgerGSTINResponse))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings: %v", warnings)
	}
	cases := map[string]string{
		// Flat field, upper-cased.
		"Delhi Cargo Service Center Pvt.Ltd-ADVANCE": "07ZZYAB1234C1Z5",
		// Dated registrations only: the latest applies.
		"Carico Systems Pvt.Ltd-Advance": "27ZZYCD5678E2Z0",
		// Name from the NAME attribute when the element is absent.
		"Attribute Only Ledger": "09ZZYEF9012G1Z3",
		// Placeholders and ledgers without a GSTIN are skipped.
		"Unregistered Supplier": "",
		"Freight Inward":        "",
	}
	for name, want := range cases {
		if got := ledgers.Lookup(name); got != want {
			t.Errorf("Lookup(%q) = %q, want %q", name, got, want)
		}
	}
	if ledgers.Len() != 3 {
		t.Errorf("Len = %d, want 3", ledgers.Len())
	}
	// Tally ledger names are case-insensitive.
	if got := ledgers.Lookup("delhi cargo service center pvt.ltd-advance"); got != "07ZZYAB1234C1Z5" {
		t.Errorf("case-folded lookup = %q", got)
	}
	var nilLedgers *LedgerGSTINs
	if nilLedgers.Lookup("anything") != "" || nilLedgers.Len() != 0 {
		t.Error("nil map must look up empty")
	}
}

func TestBuildLedgerGSTINXML_AsksForEveryLedgerAndEveryGSTINField(t *testing.T) {
	body, err := BuildLedgerGSTINXML("Aam & Co")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(body)
	for _, want := range []string{"<TYPE>Ledger</TYPE>", "PARTYGSTIN", "PARTYGSTNUMBER", "LEDGSTREGDETAILS.LIST", "Aam &amp; Co"} {
		if !strings.Contains(s, want) {
			t.Errorf("envelope missing %q", want)
		}
	}
	// No group filter: parties live under every kind of group.
	if strings.Contains(s, "<FILTER>") {
		t.Error("ledger GSTIN request must not filter by group")
	}
	if _, err := BuildLedgerGSTINXML(""); err == nil {
		t.Error("empty company must fail")
	}
}

func lookupFrom(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

func journalVoucher(party string, entries ...LedgerEntry) RawVoucher {
	return RawVoucher{
		GUID:            "jv-1",
		Date:            time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		VoucherType:     "Journal",
		VoucherNumber:   "CI26270000000145",
		PartyLedgerName: party,
		LedgerEntries:   entries,
	}
}

func TestNormalizeJournalWith_FillsGSTINFromThePartyLedger(t *testing.T) {
	v := journalVoucher("Delhi Cargo Service Center Pvt.Ltd-ADVANCE",
		LedgerEntry{LedgerName: "Delhi Cargo Service Center Pvt.Ltd-ADVANCE", Amount: 2509.8, IsPartyLedger: true},
		LedgerEntry{LedgerName: "Terminal Handling Charges", Amount: -2127},
		LedgerEntry{LedgerName: "CGST Input Credit", Amount: -191.4},
		LedgerEntry{LedgerName: "SGST Input Credit", Amount: -191.4},
	)
	row, err := NormalizeJournalWith(v, AccountingNormalizeOptions{
		LedgerGSTIN: lookupFrom(map[string]string{"Delhi Cargo Service Center Pvt.Ltd-ADVANCE": "07ZZYAB1234C1Z5"}),
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if row.PartyGSTIN == nil || *row.PartyGSTIN != "07ZZYAB1234C1Z5" {
		t.Fatalf("PartyGSTIN = %v, want 07ZZYAB1234C1Z5", row.PartyGSTIN)
	}

	// Without the lookup, behaviour is unchanged.
	plain, _ := NormalizeJournal(v)
	if plain.PartyGSTIN != nil {
		t.Errorf("plain PartyGSTIN = %q, want nil", *plain.PartyGSTIN)
	}
}

func TestNormalizeJournalWith_VoucherGSTINWins(t *testing.T) {
	v := journalVoucher("Vendor", LedgerEntry{LedgerName: "Vendor", Amount: 118}, LedgerEntry{LedgerName: "Expense", Amount: -118})
	v.PartyGSTIN = "29ZZYAB1234C1Z5"
	row, _ := NormalizeJournalWith(v, AccountingNormalizeOptions{
		LedgerGSTIN: lookupFrom(map[string]string{"Vendor": "29ZZYXX0000X1Z0"}),
	})
	if row.PartyGSTIN == nil || *row.PartyGSTIN != "29ZZYAB1234C1Z5" {
		t.Fatalf("PartyGSTIN = %v, want the voucher's own", row.PartyGSTIN)
	}
}

func TestNormalizeJournalWith_FindsTheOneLedgerThatCarriesAGSTIN(t *testing.T) {
	// No voucher party and no bill refs: the first non-bank, non-tax line
	// (the expense) is chosen, but the party is the GSTIN-bearing ledger.
	v := journalVoucher("",
		LedgerEntry{LedgerName: "Shipment Exp Domestic", Amount: -11000},
		LedgerEntry{LedgerName: "IGST Input Credit", Amount: -1980},
		LedgerEntry{LedgerName: "Demo Logistics Pvt Ltd", Amount: 12980},
	)
	row, _ := NormalizeJournalWith(v, AccountingNormalizeOptions{
		LedgerGSTIN: lookupFrom(map[string]string{"Demo Logistics Pvt Ltd": "27ZZYAB1234C1Z5"}),
	})
	if row.PartyGSTIN == nil || *row.PartyGSTIN != "27ZZYAB1234C1Z5" {
		t.Fatalf("PartyGSTIN = %v", row.PartyGSTIN)
	}
	if row.PartyName == nil || *row.PartyName != "Demo Logistics Pvt Ltd" {
		t.Fatalf("PartyName = %v, want the GSTIN-bearing ledger", row.PartyName)
	}
}

func TestNormalizeJournalWith_DoesNotGuessBetweenTwoGSTINs(t *testing.T) {
	v := journalVoucher("",
		LedgerEntry{LedgerName: "Freight", Amount: -1000},
		LedgerEntry{LedgerName: "Carrier A", Amount: 600},
		LedgerEntry{LedgerName: "Carrier B", Amount: 400},
	)
	row, _ := NormalizeJournalWith(v, AccountingNormalizeOptions{
		LedgerGSTIN: lookupFrom(map[string]string{"Carrier A": "27ZZYAB1234C1Z5", "Carrier B": "07ZZYCD5678E1Z1"}),
	})
	if row.PartyGSTIN != nil {
		t.Fatalf("PartyGSTIN = %q, want none when two parties carry GSTINs", *row.PartyGSTIN)
	}
}

func TestNormalizePaymentWith_FillsGSTINFromThePartyLedger(t *testing.T) {
	v := journalVoucher("Vendor",
		LedgerEntry{LedgerName: "Vendor", Amount: -11800},
		LedgerEntry{LedgerName: "HDFC Bank", Amount: 11800},
	)
	v.VoucherType = "Bank Payment"
	row, err := NormalizePaymentWith(v, AccountingNormalizeOptions{
		LedgerGSTIN: lookupFrom(map[string]string{"Vendor": "29ZZYAB1234C1Z5"}),
	})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if row.PartyGSTIN == nil || *row.PartyGSTIN != "29ZZYAB1234C1Z5" {
		t.Fatalf("PartyGSTIN = %v", row.PartyGSTIN)
	}
}

func TestNormalize_FillsVendorGSTINFromThePartyLedger(t *testing.T) {
	v := RawVoucher{
		GUID:            "pv-1",
		Date:            time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		VoucherType:     "Purchase",
		VoucherNumber:   "PV-77",
		Reference:       "INV/26-27/0441",
		PartyLedgerName: "Mds Marine Services.",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Mds Marine Services.", Amount: 11800, IsPartyLedger: true},
			{LedgerName: "Ocean Freight", Amount: -10000},
			{LedgerName: "IGST INPUT @18%", GSTClass: "IGST@18", Amount: -1800},
		},
	}
	rows, err := Normalize(v, NormalizeOptions{
		Kind:        IngestKindPurchase,
		LedgerGSTIN: lookupFrom(map[string]string{"Mds Marine Services.": "27ZZYMD1234M1Z2"}),
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("normalize: rows=%d err=%v", len(rows), err)
	}
	if rows[0].VendorGSTIN == nil || *rows[0].VendorGSTIN != "27ZZYMD1234M1Z2" {
		t.Fatalf("VendorGSTIN = %v", rows[0].VendorGSTIN)
	}

	plain, _ := Normalize(v, NormalizeOptions{Kind: IngestKindPurchase})
	if plain[0].VendorGSTIN != nil {
		t.Errorf("without the lookup VendorGSTIN = %q, want nil", *plain[0].VendorGSTIN)
	}
}
