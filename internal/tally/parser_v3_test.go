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

func TestSanitizeXMLBytes_StripsControlChars(t *testing.T) {
	input := []byte("<NARR>hello\x04world\x00bye</NARR>")
	got := sanitizeXMLBytes(input)
	want := "<NARR>helloworldbye</NARR>"
	if string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSanitizeXMLBytes_PreservesValidWhitespace(t *testing.T) {
	input := []byte("<X>line1\nline2\tcol\rend</X>")
	got := sanitizeXMLBytes(input)
	if string(got) != string(input) {
		t.Errorf("expected tab/LF/CR preserved, got %q", got)
	}
}

func TestSanitizeXMLBytes_NoAllocOnClean(t *testing.T) {
	input := []byte("<X>clean</X>")
	got := sanitizeXMLBytes(input)
	if &got[0] != &input[0] {
		t.Errorf("clean input should not allocate a new slice")
	}
}

func TestParseDayBookV3_StripsTallyControlBytes(t *testing.T) {
	xml := []byte(`<ENVELOPE>
<HEADER><STATUS>1</STATUS></HEADER>
<BODY><DATA><TALLYMESSAGE>
<VOUCHER>
<DATE>20260415</DATE>
<GUID>abc-123</GUID>
<VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
<VOUCHERNUMBER>P/0001</VOUCHERNUMBER>
<PARTYLEDGERNAME>Supplier` + "\x04" + `Co</PARTYLEDGERNAME>
<NARRATION>copied` + "\x00" + `from word</NARRATION>
</VOUCHER>
</TALLYMESSAGE></DATA></BODY>
</ENVELOPE>`)
	got, err := ParseDayBookV3(xml)
	if err != nil {
		t.Fatalf("ParseDayBookV3 err = %v", err)
	}
	if len(got.Vouchers) != 1 {
		t.Fatalf("expected 1 voucher, got %d", len(got.Vouchers))
	}
	if got.Vouchers[0].PartyLedgerName != "SupplierCo" {
		t.Errorf("PartyLedgerName = %q, want SupplierCo (control char stripped)", got.Vouchers[0].PartyLedgerName)
	}
	if got.Vouchers[0].Narration != "copiedfrom word" {
		t.Errorf("Narration = %q (control char stripped)", got.Vouchers[0].Narration)
	}
}

func TestDecodeTallyResponse_UTF16LEWithBOM(t *testing.T) {
	// Same XML, encoded as UTF-16LE with BOM.
	utf8 := []byte("<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER></ENVELOPE>")
	utf16le := []byte{0xff, 0xfe} // BOM
	for _, b := range utf8 {
		utf16le = append(utf16le, b, 0x00)
	}
	got := decodeTallyResponse(utf16le)
	if string(got) != string(utf8) {
		t.Errorf("got %q, want %q", got, utf8)
	}
}

func TestDecodeTallyResponse_UTF16LEWithoutBOM(t *testing.T) {
	// BOM-less UTF-16LE — detected via null-byte heuristic.
	utf8 := []byte("<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER></ENVELOPE>")
	utf16le := []byte{}
	for _, b := range utf8 {
		utf16le = append(utf16le, b, 0x00)
	}
	got := decodeTallyResponse(utf16le)
	if string(got) != string(utf8) {
		t.Errorf("got %q, want %q", got, utf8)
	}
}

func TestDecodeTallyResponse_UTF16BEWithBOM(t *testing.T) {
	utf8 := []byte("<ENVELOPE></ENVELOPE>")
	utf16be := []byte{0xfe, 0xff}
	for _, b := range utf8 {
		utf16be = append(utf16be, 0x00, b)
	}
	got := decodeTallyResponse(utf16be)
	if string(got) != string(utf8) {
		t.Errorf("got %q, want %q", got, utf8)
	}
}

func TestDecodeTallyResponse_UTF8BOMStripped(t *testing.T) {
	utf8WithBOM := append([]byte{0xef, 0xbb, 0xbf}, []byte("<X/>")...)
	got := decodeTallyResponse(utf8WithBOM)
	if string(got) != "<X/>" {
		t.Errorf("got %q, want <X/>", got)
	}
}

func TestDecodeTallyResponse_PlainUTF8Unchanged(t *testing.T) {
	utf8 := []byte("<ENVELOPE><X/></ENVELOPE>")
	got := decodeTallyResponse(utf8)
	if string(got) != string(utf8) {
		t.Errorf("plain UTF-8 should be unchanged, got %q", got)
	}
}

func TestParseDayBookV3_HandlesUTF16LEWithControlChars(t *testing.T) {
	// Realistic worst case: UTF-16LE response (BOM-less) with embedded
	// U+0004 in a narration field.
	utf8 := `<ENVELOPE>
<HEADER><STATUS>1</STATUS></HEADER>
<BODY><DATA><TALLYMESSAGE>
<VOUCHER>
<DATE>20260415</DATE>
<GUID>g-1</GUID>
<VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
<VOUCHERNUMBER>P/0001</VOUCHERNUMBER>
<NARRATION>copied` + "\x04" + `from word</NARRATION>
</VOUCHER>
</TALLYMESSAGE></DATA></BODY>
</ENVELOPE>`
	utf16le := []byte{}
	for _, r := range utf8 {
		utf16le = append(utf16le, byte(r&0xff), byte((r>>8)&0xff))
	}
	got, err := ParseDayBookV3(utf16le)
	if err != nil {
		t.Fatalf("ParseDayBookV3 err = %v", err)
	}
	if len(got.Vouchers) != 1 {
		t.Fatalf("expected 1 voucher, got %d", len(got.Vouchers))
	}
	if got.Vouchers[0].Narration != "copiedfrom word" {
		t.Errorf("Narration = %q, want %q", got.Vouchers[0].Narration, "copiedfrom word")
	}
}

func TestStripIllegalCharRefs_DropsControlChars(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`<X>&#4; Not Applicable</X>`, `<X> Not Applicable</X>`},
		{`<X>&#x04;hi</X>`, `<X>hi</X>`},
		{`<X>&#0;&#1;&#2;hello</X>`, `<X>hello</X>`},
		{`<X>&#9;tab</X>`, `<X>&#9;tab</X>`},      // tab is valid, keep
		{`<X>&#65;</X>`, `<X>&#65;</X>`},          // 'A' is valid, keep
		{`<X>&#xff;hi</X>`, `<X>&#xff;hi</X>`},    // 0xFF (255) is valid, keep
		{`<X>&amp;</X>`, `<X>&amp;</X>`},          // entity ref, not numeric — leave alone
		{`<X>plain text</X>`, `<X>plain text</X>`}, // no refs at all
	}
	for _, c := range cases {
		got := string(stripIllegalCharRefs([]byte(c.in)))
		if got != c.want {
			t.Errorf("input=%q: got=%q want=%q", c.in, got, c.want)
		}
	}
}

func TestParseDayBookV3_HandlesGstClassWithIllegalCharRef(t *testing.T) {
	// The actual PLLUM bug: Tally emits `<GSTCLASS>&#4; Not Applicable</GSTCLASS>`
	// inside ledger entries. Confirms parser handles it now.
	xml := `<ENVELOPE>
<HEADER><STATUS>1</STATUS></HEADER>
<BODY><DATA><TALLYMESSAGE>
<VOUCHER>
<DATE>20260415</DATE>
<GUID>g-1</GUID>
<VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
<VOUCHERNUMBER>P/0001</VOUCHERNUMBER>
<PARTYLEDGERNAME>Acme Co</PARTYLEDGERNAME>
<ALLLEDGERENTRIES.LIST>
<LEDGERNAME>Acme Co</LEDGERNAME>
<AMOUNT>1180.00</AMOUNT>
<GSTCLASS TYPE="String">&#4; Not Applicable</GSTCLASS>
</ALLLEDGERENTRIES.LIST>
</VOUCHER>
</TALLYMESSAGE></DATA></BODY>
</ENVELOPE>`
	got, err := ParseDayBookV3([]byte(xml))
	if err != nil {
		t.Fatalf("ParseDayBookV3 err = %v", err)
	}
	if len(got.Vouchers) != 1 {
		t.Fatalf("expected 1 voucher, got %d (warnings: %v)", len(got.Vouchers), got.Warnings)
	}
	if got.Vouchers[0].PartyLedgerName != "Acme Co" {
		t.Errorf("PartyLedgerName = %q", got.Vouchers[0].PartyLedgerName)
	}
}

func TestSanitizeInvalidUTF8_DropsBareBytes(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{
			"valid UTF-8 unchanged",
			[]byte("hello world"),
			"hello world",
		},
		{
			"Windows-1252 right single quote (0x92) dropped",
			[]byte{'h', 'i', 0x92, 's', ' ', 'o', 'k'},
			"his ok",
		},
		{
			"valid multi-byte UTF-8 preserved",
			[]byte("café"), // é = 0xC3 0xA9 in UTF-8
			"café",
		},
		{
			"₹ rupee sign preserved",
			[]byte("₹100"), // ₹ = 0xE2 0x82 0xB9
			"₹100",
		},
		{
			"truncated UTF-8 sequence dropped",
			append([]byte("ok"), 0xC3),
			"ok",
		},
	}
	for _, c := range cases {
		got := string(sanitizeInvalidUTF8(c.in))
		if got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestParseDayBookV3_HandlesWindows1252Bytes(t *testing.T) {
	// Realistic case: Tally narration field contains a Windows-1252
	// right single quote (0x92) — invalid UTF-8 — embedded in
	// otherwise-clean XML. The PLLUM LEGNO sales response on
	// 2026-04-27 hit this.
	xml := []byte(`<ENVELOPE>
<HEADER><STATUS>1</STATUS></HEADER>
<BODY><DATA><TALLYMESSAGE>
<VOUCHER>
<DATE>20260415</DATE>
<GUID>g-1</GUID>
<VOUCHERTYPENAME>Sales</VOUCHERTYPENAME>
<VOUCHERNUMBER>S/0001</VOUCHERNUMBER>
<PARTYLEDGERNAME>Acme` + string([]byte{0x92}) + `s Ltd</PARTYLEDGERNAME>
<NARRATION>March` + string([]byte{0x92}) + `s order</NARRATION>
</VOUCHER>
</TALLYMESSAGE></DATA></BODY>
</ENVELOPE>`)
	got, err := ParseDayBookV3(xml)
	if err != nil {
		t.Fatalf("ParseDayBookV3 err = %v", err)
	}
	if len(got.Vouchers) != 1 {
		t.Fatalf("expected 1 voucher, got %d", len(got.Vouchers))
	}
	if got.Vouchers[0].PartyLedgerName != "Acmes Ltd" {
		t.Errorf("PartyLedgerName = %q (0x92 should be dropped)", got.Vouchers[0].PartyLedgerName)
	}
	if got.Vouchers[0].Narration != "Marchs order" {
		t.Errorf("Narration = %q", got.Vouchers[0].Narration)
	}
}
