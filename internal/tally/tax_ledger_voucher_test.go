package tally

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

const kssGUID = "54b5dca1-178d-43b8-9aa7-fd652aed0538-000024da"

var (
	sep2026From = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	sep2026To   = time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
)

// kssLedgerSet is the tax ledger set as the tax_ledger lane reads it.
func kssLedgerSet(t *testing.T) map[string]bool {
	t.Helper()
	parsed, err := ParseTaxLedgerV3(readTestdata(t, "tax-ledgers-kss-092026.xml"))
	if err != nil {
		t.Fatalf("ParseTaxLedgerV3: %v", err)
	}
	set := map[string]bool{}
	for _, l := range parsed.Ledgers {
		set[TaxLedgerKey(l.Name)] = true
	}
	if len(set) != 8 {
		t.Fatalf("ledger set has %d ledgers, want 8", len(set))
	}
	return set
}

func selectFixture(t *testing.T, raw []byte) TaxLedgerVoucherSelection {
	t.Helper()
	reply, err := ParseTaxLedgerVoucherReply(raw)
	if err != nil {
		t.Fatalf("ParseTaxLedgerVoucherReply: %v", err)
	}
	return SelectTaxLedgerVouchers(reply, kssLedgerSet(t), sep2026From, sep2026To)
}

func rowByNumber(t *testing.T, sel TaxLedgerVoucherSelection, number string) *TaxLedgerVoucherRow {
	t.Helper()
	for i := range sel.Rows {
		if r := sel.Rows[i]; r.VoucherNumber != nil && *r.VoucherNumber == number {
			return &sel.Rows[i]
		}
	}
	return nil
}

func TestBuildTaxLedgerVoucherXML_ProbeFetchNoFilter(t *testing.T) {
	env, err := BuildTaxLedgerVoucherXML(TaxLedgerVoucherRequest{Company: "KSS INTERIORS INDIA PVT. LTD C", From: sep2026From, To: sep2026To})
	if err != nil {
		t.Fatal(err)
	}
	s := string(env)
	for _, want := range []string{
		"<SVCURRENTCOMPANY>KSS INTERIORS INDIA PVT. LTD C</SVCURRENTCOMPANY>",
		`<SVFROMDATE TYPE="Date">20260901</SVFROMDATE>`,
		`<SVTODATE TYPE="Date">20260930</SVTODATE>`,
		"<TYPE>Voucher</TYPE>",
		"GUID, MASTERID, ALTERID, DATE, VOUCHERTYPENAME, VOUCHERNUMBER, REFERENCE, PARTYLEDGERNAME, PARTYGSTIN, PLACEOFSUPPLY, ISINVOICE, ISCANCELLED, ISOPTIONAL, ISRCMAPPLICABLE, NARRATION",
		"ALLLEDGERENTRIES.LEDGERNAME, ALLLEDGERENTRIES.AMOUNT, ALLLEDGERENTRIES.GSTCLASS, ALLLEDGERENTRIES.BILLALLOCATIONS",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("envelope missing %q", want)
		}
	}
	if strings.Contains(s, "<FILTER>") || strings.Contains(s, "INVENTORYENTRIES") {
		t.Errorf("envelope must read every voucher type without inventory:\n%s", s)
	}
	if _, err := BuildTaxLedgerVoucherXML(TaxLedgerVoucherRequest{Company: "A\x04B", From: sep2026From, To: sep2026To}); err == nil {
		t.Error("control character in company must be refused")
	}
}

func TestSelectTaxLedgerVouchers_KSSFixture(t *testing.T) {
	sel := selectFixture(t, readTestdata(t, "tax-ledger-vouchers-kss-092026.xml"))

	snap := sel.Snapshot
	want := TaxLedgerVoucherSnapshot{Complete: true, VouchersReturned: 7, CancelledTouching: 1}
	if snap != want {
		t.Fatalf("snapshot=%+v, want %+v", snap, want)
	}
	// Journal 327, Receipt, Contra, the optional journal and the purchase;
	// not the Payment (touches no tax ledger) nor the cancelled journal.
	if len(sel.Rows) != 5 {
		t.Fatalf("rows=%d, want 5", len(sel.Rows))
	}
	if rowByNumber(t, sel, "KSS/P/40") != nil {
		t.Error("a voucher touching no tax ledger must not be selected")
	}
	if rowByNumber(t, sel, "KSS/J/300/26-27") != nil {
		t.Error("a cancelled voucher must not be sent")
	}

	j := rowByNumber(t, sel, "KSS/J/327/26-27")
	if j == nil {
		t.Fatal("KSS/J/327/26-27 not selected")
	}
	if *j.TallyVoucherGUID != kssGUID || *j.VoucherDate != "2026-09-30" || j.VoucherDateRaw != "20260930" {
		t.Errorf("journal identity: guid=%v date=%v raw=%q", *j.TallyVoucherGUID, *j.VoucherDate, j.VoucherDateRaw)
	}
	if *j.MasterID != "9434" || *j.AlterID != "31877" || *j.VoucherTypeName != "Journal" || j.Reference != nil {
		t.Errorf("journal fields: master=%v alter=%v type=%v ref=%v", *j.MasterID, *j.AlterID, *j.VoucherTypeName, j.Reference)
	}
	wantLegs := []struct{ name, amount string }{
		{"IGST OUTPUT", "-162000.00"}, {"CGST Output", "-28243.31"}, {"SGST Output", "-28243.31"},
		{"IGST INPUT", "186490.00"}, {"CGST INPUT", "3753.00"}, {"SGST INPUT", "28243.00"},
		{"Rounded Off", "0.62"},
	}
	if len(j.Legs) != len(wantLegs) {
		t.Fatalf("journal legs=%d, want %d (every leg, not only tax legs)", len(j.Legs), len(wantLegs))
	}
	for i, w := range wantLegs {
		if j.Legs[i].LedgerName != w.name || j.Legs[i].Amount == nil || *j.Legs[i].Amount != w.amount {
			t.Errorf("leg %d = %q %v, want %q %s (Tally sign, exactly as returned)", i, j.Legs[i].LedgerName, j.Legs[i].Amount, w.name, w.amount)
		}
	}
	if j.Legs[0].GSTClass == nil || *j.Legs[0].GSTClass != "\x04 Not Applicable" {
		t.Errorf("&#4; must parse as U+0004 (not repaired), got %v", j.Legs[0].GSTClass)
	}
	if len(j.Problems) != 0 {
		t.Errorf("journal problems=%v, want none", j.Problems)
	}

	// Receipt (legs only under LEDGERENTRIES.LIST) and Contra (leg name in
	// another case with spaces) touching CGST INPUT are selected.
	r := rowByNumber(t, sel, "KSS/R/88")
	if r == nil || len(r.Legs) != 2 {
		t.Fatalf("receipt touching CGST INPUT not selected with its 2 legs: %+v", r)
	}
	c := rowByNumber(t, sel, "KSS/C/12")
	if c == nil || c.Legs[0].LedgerName != "  cgst input " {
		t.Fatalf("contra touching CGST INPUT not selected with its raw ledger name: %+v", c)
	}

	o := rowByNumber(t, sel, "KSS/J/310/26-27")
	if o == nil || !o.IsOptional {
		t.Fatalf("optional journal must be sent with is_optional: %+v", o)
	}

	p := rowByNumber(t, sel, "KSS/PUR/77")
	if p == nil || !p.IsInvoice || !p.IsRCMApplicable || *p.PartyGSTIN != "29AAACA1234A1Z5" || *p.Reference != "INV-9981" {
		t.Fatalf("purchase fields: %+v", p)
	}
	// The purchase's legs sum to 1.00: sent as is; the server flags it.
	if sel.Unbalanced != 1 || len(p.Problems) != 0 {
		t.Errorf("unbalanced=%d purchase problems=%v, want 1 and none", sel.Unbalanced, p.Problems)
	}
}

func TestSelectTaxLedgerVouchers_KSSLegsSumToZero(t *testing.T) {
	var s decimalSum
	for _, a := range []string{"-162000.00", "-28243.31", "-28243.31", "186490.00", "3753.00", "28243.00", "0.62"} {
		if !s.add(a) {
			t.Fatalf("add %s", a)
		}
	}
	if !s.zero() {
		t.Fatalf("KSS journal legs sum to %d micros, want 0", s.micros)
	}
}

// oneVoucher wraps voucher XML in a minimal collection reply.
func oneVoucher(body string) []byte {
	return []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>` + body + `</COLLECTION></DATA></BODY></ENVELOPE>`)
}

func TestSelectTaxLedgerVouchers_Problems(t *testing.T) {
	cases := []struct {
		name, xml string
		want      []string
	}{
		{"no GUID", `<VOUCHER><DATE>20260910</DATE><VOUCHERNUMBER>X</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`,
			[]string{TLVProblemMissingGUID}},
		{"duplicate GUID", `<VOUCHER><GUID>g-1</GUID><DATE>20260910</DATE><VOUCHERNUMBER>X</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>
			<VOUCHER><GUID>G-1</GUID><DATE>20260911</DATE><VOUCHERNUMBER>Y</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>Cash</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`,
			[]string{TLVProblemDuplicateGUID}},
		{"unreadable amount", `<VOUCHER><GUID>g-2</GUID><DATE>20260910</DATE><VOUCHERNUMBER>X</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1,200.00 Dr</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`,
			[]string{TLVProblemAmountUnreadable}},
		{"empty amount", `<VOUCHER><GUID>g-3</GUID><DATE>20260910</DATE><VOUCHERNUMBER>X</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME></ALLLEDGERENTRIES.LIST></VOUCHER>`,
			[]string{TLVProblemAmountUnreadable}},
		{"leg without ledger name", `<VOUCHER><GUID>g-4</GUID><DATE>20260910</DATE><VOUCHERNUMBER>X</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME> </LEDGERNAME><AMOUNT>-1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`,
			[]string{TLVProblemLegWithoutLedgerName}},
		{"control character in ledger name", `<VOUCHER><GUID>g-5</GUID><DATE>20260910</DATE><VOUCHERNUMBER>X</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT&#4;</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`,
			[]string{TLVProblemLedgerNameControl}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sel := selectFixture(t, oneVoucher(tc.xml))
			x := rowByNumber(t, sel, "X")
			if x == nil {
				t.Fatalf("voucher with a problem must still be sent; rows=%+v", sel.Rows)
			}
			if strings.Join(x.Problems, ",") != strings.Join(tc.want, ",") {
				t.Errorf("problems=%v, want %v", x.Problems, tc.want)
			}
			if !sel.Snapshot.Complete {
				t.Errorf("a problem is per voucher; snapshot stays complete: %+v", sel.Snapshot)
			}
		})
	}
}

func TestSelectTaxLedgerVouchers_UnreadableAmountIsNull(t *testing.T) {
	sel := selectFixture(t, oneVoucher(`<VOUCHER><GUID>g</GUID><DATE>20260910</DATE><VOUCHERNUMBER>X</VOUCHERNUMBER>
		<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>abc</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`))
	if a := sel.Rows[0].Legs[0].Amount; a != nil {
		t.Errorf("unreadable amount = %q, want null", *a)
	}
}

func TestSelectTaxLedgerVouchers_OutsideWindowCountedNotSent(t *testing.T) {
	sel := selectFixture(t, oneVoucher(`
		<VOUCHER><GUID>in</GUID><DATE>20260930</DATE><VOUCHERNUMBER>IN</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>
		<VOUCHER><GUID>out</GUID><DATE>20261001</DATE><VOUCHERNUMBER>OUT</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>
		<VOUCHER><GUID>out2</GUID><DATE>31-Aug-26</DATE><VOUCHERNUMBER>OUT2</VOUCHERNUMBER>
			<ALLLEDGERENTRIES.LIST><LEDGERNAME>Cash</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`))
	if sel.Snapshot.OutsideWindow != 2 || sel.Snapshot.Complete {
		t.Fatalf("snapshot=%+v, want outside_window=2 complete=false", sel.Snapshot)
	}
	if len(sel.Rows) != 1 || rowByNumber(t, sel, "IN") == nil {
		t.Fatalf("rows=%+v, want only IN", sel.Rows)
	}
	if strings.Join(sel.OutsideWindowGUIDs, ",") != "out,out2" {
		t.Errorf("outside GUIDs=%v", sel.OutsideWindowGUIDs)
	}
}

func TestSelectTaxLedgerVouchers_UnreadableDate(t *testing.T) {
	sel := selectFixture(t, oneVoucher(`<VOUCHER><GUID>g</GUID><DATE>Sept 31st</DATE><VOUCHERNUMBER>X</VOUCHERNUMBER>
		<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`))
	if sel.Snapshot.DateUnreadable != 1 || sel.Snapshot.Complete {
		t.Fatalf("snapshot=%+v, want date_unreadable=1 complete=false", sel.Snapshot)
	}
	x := rowByNumber(t, sel, "X")
	if x == nil || x.VoucherDate != nil || x.VoucherDateRaw != "Sept 31st" {
		t.Fatalf("unreadable-date voucher: %+v", x)
	}
}

func TestParseTaxLedgerVoucherReply_UndeclaredUDFParsesOtherRefused(t *testing.T) {
	ok := oneVoucher(`<VOUCHER><GUID>g</GUID><DATE>20260910</DATE><UDF:FOO.LIST><UDF:BAR>1</UDF:BAR></UDF:FOO.LIST>
		<ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT><UDF:X>y</UDF:X></ALLLEDGERENTRIES.LIST></VOUCHER>`)
	reply, err := ParseTaxLedgerVoucherReply(ok)
	if err != nil {
		t.Fatalf("UDF: prefix must parse: %v", err)
	}
	if len(reply.Vouchers) != 1 || len(reply.Vouchers[0].Legs) != 1 {
		t.Fatalf("reply=%+v", reply)
	}
	declared := []byte(`<ENVELOPE xmlns:ABC="urn:x"><BODY><VOUCHER><GUID>g</GUID><ABC:Z>1</ABC:Z></VOUCHER></BODY></ENVELOPE>`)
	if _, err := ParseTaxLedgerVoucherReply(declared); err != nil {
		t.Fatalf("a declared prefix is fine: %v", err)
	}
	for name, raw := range map[string][]byte{
		"other undeclared prefix":        oneVoucher(`<VOUCHER><GUID>g</GUID><FOO:X>1</FOO:X></VOUCHER>`),
		"undeclared attribute prefix":    oneVoucher(`<VOUCHER FOO:A="1"><GUID>g</GUID></VOUCHER>`),
		"namespaced VOUCHER":             oneVoucher(`<VOUCHER xmlns="urn:v"><GUID>g</GUID></VOUCHER>`),
		"namespaced AMOUNT":              oneVoucher(`<VOUCHER><GUID>g</GUID><ALLLEDGERENTRIES.LIST><UDF:AMOUNT>1</UDF:AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>`),
		"LINEERROR":                      []byte(`<ENVELOPE><LINEERROR>Could not set 'SVCurrentCompany'</LINEERROR></ENVELOPE>`),
		"STATUS 0":                       []byte(`<ENVELOPE><HEADER><STATUS>0</STATUS></HEADER></ENVELOPE>`),
		"literal control byte":           oneVoucher("<VOUCHER><GUID>g\x04</GUID></VOUCHER>"),
		"invalid UTF-8 is not repaired":  oneVoucher("<VOUCHER><GUID>g\x92</GUID></VOUCHER>"),
		"truncated":                      []byte(`<ENVELOPE><BODY><VOUCHER><GUID>g</GUID>`),
		"empty":                          []byte("  "),
		"undeclared prefix inside a leg": oneVoucher(`<VOUCHER><ALLLEDGERENTRIES.LIST><BAD:X/></ALLLEDGERENTRIES.LIST></VOUCHER>`),
	} {
		if _, err := ParseTaxLedgerVoucherReply(raw); err == nil || !errors.Is(err, ErrTaxLedgerVoucherReply) {
			t.Errorf("%s: err=%v, want ErrTaxLedgerVoucherReply", name, err)
		}
	}
}

func utf16Bytes(s string, bom, bigEndian bool) []byte {
	var out []byte
	put := func(u uint16) {
		var b [2]byte
		if bigEndian {
			binary.BigEndian.PutUint16(b[:], u)
		} else {
			binary.LittleEndian.PutUint16(b[:], u)
		}
		out = append(out, b[:]...)
	}
	if bom {
		put(0xfeff)
	}
	for _, u := range utf16.Encode([]rune(s)) {
		put(u)
	}
	return out
}

func TestParseTaxLedgerVoucherReply_UTF16(t *testing.T) {
	src := string(readTestdata(t, "tax-ledger-vouchers-kss-092026.xml"))
	src = `<?xml version="1.0" encoding="UTF-16"?>` + "\n" + strings.Replace(src, "GST set-off", "GST set-off ₹ 𝟙", 1)
	for name, raw := range map[string][]byte{
		"LE with BOM": utf16Bytes(src, true, false),
		"BE with BOM": utf16Bytes(src, true, true),
		"LE no BOM":   utf16Bytes(src, false, false),
		"BE no BOM":   utf16Bytes(src, false, true),
	} {
		sel := selectFixture(t, raw)
		if len(sel.Rows) != 5 || sel.Snapshot.VouchersReturned != 7 {
			t.Errorf("%s: rows=%d returned=%d, want 5/7", name, len(sel.Rows), sel.Snapshot.VouchersReturned)
		}
	}
	if _, err := ParseTaxLedgerVoucherReply(utf16Bytes(src, true, false)[:101]); err == nil {
		t.Error("odd-length UTF-16 must be refused")
	}
}

func TestTaxLedgerKey(t *testing.T) {
	if TaxLedgerKey("  CGST Input\t") != TaxLedgerKey("cgst INPUT") {
		t.Error("ledger names compare trimmed and case-insensitively")
	}
	if TaxLedgerKey("CGST\x04 Input") != "cgst input" {
		t.Errorf("control characters are dropped from the key: %q", TaxLedgerKey("CGST\x04 Input"))
	}
}

func TestParseTaxLedgerVoucherReply_ControlRefsExactAndCDATAUntouched(t *testing.T) {
	reply, err := ParseTaxLedgerVoucherReply(oneVoucher(`<VOUCHER><GUID>g&#x1F;&#9;</GUID><VOUCHERNUMBER><![CDATA[&#4;]]></VOUCHERNUMBER><!-- &#4; --></VOUCHER>`))
	if err != nil {
		t.Fatal(err)
	}
	v := reply.Vouchers[0]
	if v.GUID != "g\x1f\t" || v.VoucherNumber != "&#4;" {
		t.Errorf("guid=%q number=%q", v.GUID, v.VoucherNumber)
	}
	if _, err := ParseTaxLedgerVoucherReply(oneVoucher("<VOUCHER><GUID>\U000F0004</GUID></VOUCHER>")); err == nil {
		t.Error("a reply holding a sentinel rune must be refused")
	}
	if _, err := ParseTaxLedgerVoucherReply(oneVoucher(`<VOUCHER><GUID>&#xFFFE;</GUID></VOUCHER>`)); err == nil {
		t.Error("a reference to a non-character is still refused")
	}
}
