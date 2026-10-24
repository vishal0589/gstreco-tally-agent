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
      <PARENT>Advance to Suppliers</PARENT>
      <PARTYGSTIN>07zzyab1234c1z5</PARTYGSTIN>
    </LEDGER>
    <LEDGER NAME="Carico Systems Pvt.Ltd-Advance">
      <NAME>Carico Systems Pvt.Ltd-Advance</NAME>
      <PARENT>Advance to Suppliers</PARENT>
      <PARTYGSTIN></PARTYGSTIN>
      <LEDGSTREGDETAILS.LIST>
        <APPLICABLEFROM>20240401</APPLICABLEFROM>
        <GSTIN>27ZZYCD5678E1Z1</GSTIN>
      </LEDGSTREGDETAILS.LIST>
      <LEDGSTREGDETAILS.LIST>
        <APPLICABLEFROM>20261001</APPLICABLEFROM>
        <GSTIN>27ZZYCD5678E2Z0</GSTIN>
      </LEDGSTREGDETAILS.LIST>
    </LEDGER>
    <LEDGER NAME="Closed Vendor">
      <NAME>Closed Vendor</NAME>
      <PARTYGSTIN>09ZZYCV0000C1Z9</PARTYGSTIN>
      <LEDGSTREGDETAILS.LIST>
        <APPLICABLEFROM>20200401</APPLICABLEFROM>
        <GSTIN>09ZZYCV0000C1Z9</GSTIN>
      </LEDGSTREGDETAILS.LIST>
      <LEDGSTREGDETAILS.LIST>
        <APPLICABLEFROM>20260701</APPLICABLEFROM>
        <GSTIN></GSTIN>
      </LEDGSTREGDETAILS.LIST>
    </LEDGER>
    <LEDGER NAME="Attribute Only Ledger">
      <GSTIN>09ZZYEF9012G1Z3</GSTIN>
    </LEDGER>
    <LEDGER NAME="Unregistered Supplier">
      <NAME>Unregistered Supplier</NAME>
      <PARTYGSTIN>NA</PARTYGSTIN>
    </LEDGER>
    <LEDGER NAME="HDFC A/c 1234">
      <NAME>HDFC A/c 1234</NAME>
      <PARENT>Current Bank Accounts</PARENT>
      <PARTYGSTIN>27ZZYHD0000B1Z7</PARTYGSTIN>
    </LEDGER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

const groupTreeResponse = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <GROUP NAME="Current Bank Accounts"><NAME>Current Bank Accounts</NAME><PARENT>Bank Accounts</PARENT></GROUP>
  <GROUP NAME="Bank Accounts"><NAME>Bank Accounts</NAME><PARENT>Current Assets</PARENT></GROUP>
  <GROUP NAME="Advance to Suppliers"><NAME>Advance to Suppliers</NAME><PARENT>Loans &amp; Advances (Asset)</PARENT></GROUP>
</COLLECTION></DATA></BODY></ENVELOPE>`

func day(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func parseFixture(t *testing.T) *LedgerGSTINs {
	t.Helper()
	ledgers, warnings, err := ParseLedgerGSTINs([]byte(ledgerGSTINResponse))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("parse: err=%v warnings=%v", err, warnings)
	}
	groups, err := ParseGroupTree([]byte(groupTreeResponse))
	if err != nil {
		t.Fatalf("groups: %v", err)
	}
	ledgers.ApplyGroupTree(groups)
	return ledgers
}

func TestLedgerGSTINs_ReadsEveryGSTINField(t *testing.T) {
	ledgers := parseFixture(t)
	on := day("2026-09-01")
	cases := map[string]string{
		"Delhi Cargo Service Center Pvt.Ltd-ADVANCE": "07ZZYAB1234C1Z5", // flat, upper-cased
		"Attribute Only Ledger":                      "09ZZYEF9012G1Z3", // NAME attribute only
		"Unregistered Supplier":                      "",                // placeholder skipped
		"Freight Inward":                             "",                // unknown ledger
	}
	for name, want := range cases {
		if got := ledgers.GSTIN(name, on); got != want {
			t.Errorf("GSTIN(%q) = %q, want %q", name, got, want)
		}
	}
	if got := ledgers.GSTIN("delhi cargo service center pvt.ltd-advance", on); got != "07ZZYAB1234C1Z5" {
		t.Errorf("case-folded lookup = %q", got)
	}
	var nilLedgers *LedgerGSTINs
	if nilLedgers.GSTIN("anything", on) != "" || nilLedgers.Len() != 0 || nilLedgers.IsNonParty("x") {
		t.Error("a nil directory must answer empty")
	}
}

func TestLedgerGSTINs_UsesTheRegistrationInForceOnTheVoucherDate(t *testing.T) {
	ledgers := parseFixture(t)
	// Changed registration on 1 Oct 2026: April vouchers keep the old GSTIN.
	if got := ledgers.GSTIN("Carico Systems Pvt.Ltd-Advance", day("2026-04-15")); got != "27ZZYCD5678E1Z1" {
		t.Errorf("April = %q, want the registration then in force", got)
	}
	if got := ledgers.GSTIN("Carico Systems Pvt.Ltd-Advance", day("2026-10-15")); got != "27ZZYCD5678E2Z0" {
		t.Errorf("October = %q, want the new registration", got)
	}
	if got := ledgers.GSTIN("Carico Systems Pvt.Ltd-Advance", day("2023-04-15")); got != "" {
		t.Errorf("before any registration = %q, want none", got)
	}
	// Deregistered from 1 Jul 2026: later vouchers get no GSTIN, even though
	// the flat field still holds the old one.
	if got := ledgers.GSTIN("Closed Vendor", day("2026-05-01")); got != "09ZZYCV0000C1Z9" {
		t.Errorf("before deregistration = %q", got)
	}
	if got := ledgers.GSTIN("Closed Vendor", day("2026-08-01")); got != "" {
		t.Errorf("after deregistration = %q, want none", got)
	}
}

func TestLedgerGSTINs_GroupTreeMarksBanksWhateverTheirName(t *testing.T) {
	ledgers := parseFixture(t)
	if !ledgers.IsNonParty("HDFC A/c 1234") {
		t.Error("a ledger under Bank Accounts (via a sub-group) must be a non-party")
	}
	if ledgers.IsNonParty("Delhi Cargo Service Center Pvt.Ltd-ADVANCE") {
		t.Error("an advance-to-supplier ledger is a party")
	}
}

func TestParseLedgerGSTINs_RejectsTallyErrorReplies(t *testing.T) {
	for name, body := range map[string]string{
		"status 0":  `<ENVELOPE><HEADER><VERSION>1</VERSION><STATUS>0</STATUS></HEADER><BODY><DATA>Could not find company</DATA></BODY></ENVELOPE>`,
		"lineerror": `<RESPONSE><LINEERROR>Could not set 'SVCurrentCompany' to 'Aam Exim'</LINEERROR></RESPONSE>`,
		"empty":     "   ",
	} {
		if _, _, err := ParseLedgerGSTINs([]byte(body)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestBuildLedgerGSTINXML_AsksForEveryLedgerAndEveryGSTINField(t *testing.T) {
	body, err := BuildLedgerGSTINXML("Aam & Co")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	s := string(body)
	for _, want := range []string{"<TYPE>Ledger</TYPE>", "PARENT", "PARTYGSTIN", "PARTYGSTNUMBER", "LEDGSTREGDETAILS.LIST", "Aam &amp; Co"} {
		if !strings.Contains(s, want) {
			t.Errorf("envelope missing %q", want)
		}
	}
	if strings.Contains(s, "<FILTER>") {
		t.Error("ledger GSTIN request must not filter by group")
	}
	if _, err := BuildLedgerGSTINXML(""); err == nil {
		t.Error("empty company must fail")
	}
	if g, err := BuildGroupTreeXML("Aam & Co"); err != nil || !strings.Contains(string(g), "<TYPE>Group</TYPE>") {
		t.Errorf("group tree envelope: %v", err)
	}
}

// fakeDirectory is a LedgerDirectory over fixed maps.
type fakeDirectory struct {
	gstins   map[string]string
	nonParty map[string]bool
}

func (f fakeDirectory) GSTIN(name string, _ time.Time) string { return f.gstins[name] }
func (f fakeDirectory) IsNonParty(name string) bool           { return f.nonParty[name] }

func journalVoucher(party string, entries ...LedgerEntry) RawVoucher {
	return RawVoucher{
		GUID:            "jv-1",
		Date:            day("2026-04-01"),
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
	row, err := NormalizeJournalWith(v, AccountingNormalizeOptions{Ledgers: parseFixture(t)})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if row.PartyGSTIN == nil || *row.PartyGSTIN != "07ZZYAB1234C1Z5" {
		t.Fatalf("PartyGSTIN = %v, want 07ZZYAB1234C1Z5", row.PartyGSTIN)
	}
	plain, _ := NormalizeJournal(v)
	if plain.PartyGSTIN != nil {
		t.Errorf("without the directory PartyGSTIN = %q, want nil", *plain.PartyGSTIN)
	}
}

func TestNormalizeJournalWith_VoucherGSTINWins(t *testing.T) {
	v := journalVoucher("Vendor", LedgerEntry{LedgerName: "Vendor", Amount: 118}, LedgerEntry{LedgerName: "Expense", Amount: -118})
	v.PartyGSTIN = "29ZZYAB1234C1Z5"
	row, _ := NormalizeJournalWith(v, AccountingNormalizeOptions{
		Ledgers: fakeDirectory{gstins: map[string]string{"Vendor": "29ZZYXX0000X1Z0"}},
	})
	if row.PartyGSTIN == nil || *row.PartyGSTIN != "29ZZYAB1234C1Z5" {
		t.Fatalf("PartyGSTIN = %v, want the voucher's own", row.PartyGSTIN)
	}
}

func TestNormalizeJournalWith_FindsTheOneLedgerThatCarriesAGSTIN(t *testing.T) {
	// No voucher party, no bill refs: the party is inferred, so the one
	// GSTIN-bearing ledger is the party, wherever it sits in the voucher.
	entries := []LedgerEntry{
		{LedgerName: "Shipment Exp Domestic", Amount: -11000},
		{LedgerName: "IGST Input Credit", Amount: -1980},
		{LedgerName: "Demo Logistics Pvt Ltd", Amount: 12980},
	}
	dir := fakeDirectory{gstins: map[string]string{"Demo Logistics Pvt Ltd": "27ZZYAB1234C1Z5"}}
	for _, order := range [][]LedgerEntry{entries, {entries[2], entries[1], entries[0]}} {
		row, _ := NormalizeJournalWith(journalVoucher("", order...), AccountingNormalizeOptions{Ledgers: dir})
		if row.PartyGSTIN == nil || *row.PartyGSTIN != "27ZZYAB1234C1Z5" {
			t.Fatalf("PartyGSTIN = %v", row.PartyGSTIN)
		}
		if row.PartyName == nil || *row.PartyName != "Demo Logistics Pvt Ltd" {
			t.Fatalf("PartyName = %v, want the GSTIN-bearing ledger", row.PartyName)
		}
	}
}

func TestNormalizeJournalWith_DoesNotGuessBetweenTwoGSTINsInAnyOrder(t *testing.T) {
	a := LedgerEntry{LedgerName: "Carrier A", Amount: 600}
	b := LedgerEntry{LedgerName: "Carrier B", Amount: 400}
	freight := LedgerEntry{LedgerName: "Freight", Amount: -1000}
	dir := fakeDirectory{gstins: map[string]string{"Carrier A": "27ZZYAB1234C1Z5", "Carrier B": "07ZZYCD5678E1Z1"}}
	for _, order := range [][]LedgerEntry{{freight, a, b}, {a, b, freight}, {b, a, freight}} {
		row, _ := NormalizeJournalWith(journalVoucher("", order...), AccountingNormalizeOptions{Ledgers: dir})
		if row.PartyGSTIN != nil {
			t.Fatalf("order %v: PartyGSTIN = %q, want none when two parties carry GSTINs", order, *row.PartyGSTIN)
		}
	}
}

func TestNormalizePaymentWith_NeverSwapsANamedPartyForItsBank(t *testing.T) {
	// The contractor is named but unregistered; the bank ledger (no "BANK"
	// in its name) carries the bank's GSTIN. The payment stays the
	// contractor's, without a GSTIN.
	v := journalVoucher("Unregistered Contractor",
		LedgerEntry{LedgerName: "Unregistered Contractor", Amount: -50000},
		LedgerEntry{LedgerName: "HDFC A/c 1234", Amount: 50000},
	)
	v.VoucherType = "Payment"
	row, err := NormalizePaymentWith(v, AccountingNormalizeOptions{Ledgers: parseFixture(t)})
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if row.PartyName == nil || *row.PartyName != "Unregistered Contractor" || row.PartyGSTIN != nil {
		t.Fatalf("party = %v / %v, want the contractor without a GSTIN", row.PartyName, row.PartyGSTIN)
	}
}

func TestNormalizeJournalWith_InferenceSkipsBankLedgersByGroup(t *testing.T) {
	// Inferred party; the only GSTIN-bearing line is a bank under Bank
	// Accounts. No party GSTIN rather than the bank's.
	v := journalVoucher("",
		LedgerEntry{LedgerName: "Bank Charges", Amount: -590},
		LedgerEntry{LedgerName: "HDFC A/c 1234", Amount: 590},
	)
	row, _ := NormalizeJournalWith(v, AccountingNormalizeOptions{Ledgers: parseFixture(t)})
	if row.PartyGSTIN != nil {
		t.Fatalf("PartyGSTIN = %q, want none (bank ledger)", *row.PartyGSTIN)
	}
}

func TestNormalizePaymentWith_FillsGSTINFromThePartyLedger(t *testing.T) {
	v := journalVoucher("Vendor",
		LedgerEntry{LedgerName: "Vendor", Amount: -11800},
		LedgerEntry{LedgerName: "HDFC Bank", Amount: 11800},
	)
	v.VoucherType = "Bank Payment"
	row, err := NormalizePaymentWith(v, AccountingNormalizeOptions{
		Ledgers: fakeDirectory{gstins: map[string]string{"Vendor": "29ZZYAB1234C1Z5"}},
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
		Date:            day("2026-09-02"),
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
		Kind:    IngestKindPurchase,
		Ledgers: fakeDirectory{gstins: map[string]string{"Mds Marine Services.": "27ZZYMD1234M1Z2"}},
	})
	if err != nil || len(rows) != 1 {
		t.Fatalf("normalize: rows=%d err=%v", len(rows), err)
	}
	if rows[0].VendorGSTIN == nil || *rows[0].VendorGSTIN != "27ZZYMD1234M1Z2" {
		t.Fatalf("VendorGSTIN = %v", rows[0].VendorGSTIN)
	}
	plain, _ := Normalize(v, NormalizeOptions{Kind: IngestKindPurchase})
	if plain[0].VendorGSTIN != nil {
		t.Errorf("without the directory VendorGSTIN = %q, want nil", *plain[0].VendorGSTIN)
	}
}
