package tally

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// QA 07-Nov D21: Tally's ISOPTIONAL travels on every voucher lane as
// is_optional: true / false as Tally returned it, and absent (never false)
// when the reply carried no flag.

// withOptional puts <ISOPTIONAL>value</ISOPTIONAL> next to the fixture's
// ISCANCELLED, the way Tally returns it once the FETCH asks for it.
func withOptional(t *testing.T, fixture []byte, value string) []byte {
	t.Helper()
	s := string(fixture)
	if strings.Count(s, "<ISCANCELLED>No</ISCANCELLED>") != 1 {
		t.Fatalf("fixture must carry exactly one <ISCANCELLED>No</ISCANCELLED>")
	}
	return []byte(strings.Replace(s, "<ISCANCELLED>No</ISCANCELLED>",
		"<ISCANCELLED>No</ISCANCELLED><ISOPTIONAL>"+value+"</ISOPTIONAL>", 1))
}

func optionalKey(t *testing.T, row any) (present bool, value bool) {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := m["is_optional"]
	if !ok {
		return false, false
	}
	flag, isBool := v.(bool)
	if !isBool {
		t.Fatalf("is_optional = %#v, want a JSON boolean", v)
	}
	return true, flag
}

func TestBuildDayBookXML_FetchesISOPTIONALOnEveryLane(t *testing.T) {
	for _, kind := range []VoucherKind{VoucherPurchase, VoucherSales, VoucherCreditNote, VoucherDebitNote, VoucherJournal, VoucherPayment} {
		body, err := BuildDayBookXML(DayBookRequest{
			Company: "KSS",
			From:    time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
			To:      time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC),
			Kind:    kind,
		})
		if err != nil {
			t.Fatalf("%s: BuildDayBookXML: %v", kind, err)
		}
		if !strings.Contains(string(body), "ISCANCELLED, ISOPTIONAL, ISRCMAPPLICABLE") {
			t.Errorf("%s: FETCH does not ask for ISOPTIONAL; body was:\n%s", kind, body)
		}
	}
}

func TestParseTallyOptionalBool(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want *bool
	}{
		{"Yes", boolPtr(true)},
		{" yes ", boolPtr(true)},
		{"No", boolPtr(false)},
		{"false", boolPtr(false)},
		{"", nil},
		{"   ", nil},
		{"Maybe", nil},
	} {
		got := parseTallyOptionalBool(tc.in)
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("%q = %v, want nil (not reported)", tc.in, *got)
		case tc.want != nil && got == nil:
			t.Errorf("%q = nil, want %v", tc.in, *tc.want)
		case tc.want != nil && *got != *tc.want:
			t.Errorf("%q = %v, want %v", tc.in, *got, *tc.want)
		}
	}
}

func TestNormalize_InvoiceLanesSendIsOptional(t *testing.T) {
	for _, fixture := range []string{
		"voucher-v3-purchase-single.xml",
		"voucher-v3-sales-cgst-sgst.xml",
		"voucher-v3-credit-note-sales.xml",
		"voucher-v3-debit-note-purchase.xml",
	} {
		for _, tc := range []struct {
			tally       string
			wantPresent bool
			wantValue   bool
		}{
			{"Yes", true, true},
			{"No", true, false},
			{"", false, false},
		} {
			raw := mustFixture(t, fixture)
			if tc.tally != "" {
				raw = withOptional(t, raw, tc.tally)
			}
			parsed, err := ParseDayBookV3(raw)
			if err != nil {
				t.Fatalf("%s: ParseDayBookV3: %v", fixture, err)
			}
			rows, err := Normalize(parsed.Vouchers[0], NormalizeOptions{})
			if err != nil {
				t.Fatalf("%s: Normalize: %v", fixture, err)
			}
			if len(rows) == 0 {
				t.Fatalf("%s: no rows", fixture)
			}
			for i, row := range rows {
				present, value := optionalKey(t, row)
				if present != tc.wantPresent || value != tc.wantValue {
					t.Errorf("%s ISOPTIONAL=%q row %d: is_optional present=%v value=%v, want present=%v value=%v",
						fixture, tc.tally, i, present, value, tc.wantPresent, tc.wantValue)
				}
			}
		}
	}
}

func TestNormalize_ConsolidatedRowsAllCarryIsOptional(t *testing.T) {
	parsed, err := ParseDayBookV3(withOptional(t, mustFixture(t, "voucher-v3-purchase-consolidated.xml"), "Yes"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	rows, err := Normalize(parsed.Vouchers[0], NormalizeOptions{})
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for i, row := range rows {
		if present, value := optionalKey(t, row); !present || !value {
			t.Errorf("row %d: is_optional present=%v value=%v, want true", i, present, value)
		}
	}
}

// Journal 741 (KSS, March 2026): an optional journal moving CGST/SGST INPUT.
const optionalJournalXML = `<?xml version="1.0" encoding="UTF-8"?>
<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <VOUCHER>
    <DATE>20260326</DATE>
    <GUID>jv-741</GUID>
    <VOUCHERTYPENAME>Journal</VOUCHERTYPENAME>
    <VOUCHERNUMBER>741</VOUCHERNUMBER>
    <ISCANCELLED>No</ISCANCELLED>%s
    <ALLLEDGERENTRIES.LIST>
      <LEDGERNAME>CGST INPUT</LEDGERNAME>
      <AMOUNT>-10.60</AMOUNT>
    </ALLLEDGERENTRIES.LIST>
    <ALLLEDGERENTRIES.LIST>
      <LEDGERNAME>SGST INPUT</LEDGERNAME>
      <AMOUNT>-10.60</AMOUNT>
    </ALLLEDGERENTRIES.LIST>
    <ALLLEDGERENTRIES.LIST>
      <LEDGERNAME>Office Expenses</LEDGERNAME>
      <AMOUNT>21.20</AMOUNT>
    </ALLLEDGERENTRIES.LIST>
  </VOUCHER>
</COLLECTION></DATA></BODY></ENVELOPE>`

func parsedJournal(t *testing.T, flag string) RawVoucher {
	t.Helper()
	extra := ""
	if flag != "" {
		extra = "\n    <ISOPTIONAL>" + flag + "</ISOPTIONAL>"
	}
	parsed, err := ParseDayBookV3([]byte(strings.Replace(optionalJournalXML, "%s", extra, 1)))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(parsed.Vouchers) != 1 {
		t.Fatalf("Vouchers = %d, want 1", len(parsed.Vouchers))
	}
	return parsed.Vouchers[0]
}

func TestNormalizeJournalAndPayment_SendIsOptional(t *testing.T) {
	for _, tc := range []struct {
		tally       string
		wantPresent bool
		wantValue   bool
	}{
		{"Yes", true, true},
		{"No", true, false},
		{"", false, false},
	} {
		v := parsedJournal(t, tc.tally)
		journal, err := NormalizeJournal(v)
		if err != nil || journal == nil {
			t.Fatalf("NormalizeJournal: row=%v err=%v", journal, err)
		}
		if present, value := optionalKey(t, journal); present != tc.wantPresent || value != tc.wantValue {
			t.Errorf("journal ISOPTIONAL=%q: is_optional present=%v value=%v, want present=%v value=%v",
				tc.tally, present, value, tc.wantPresent, tc.wantValue)
		}

		v.VoucherType = "Payment"
		payment, err := NormalizePayment(v)
		if err != nil || payment == nil {
			t.Fatalf("NormalizePayment: row=%v err=%v", payment, err)
		}
		if present, value := optionalKey(t, payment); present != tc.wantPresent || value != tc.wantValue {
			t.Errorf("payment ISOPTIONAL=%q: is_optional present=%v value=%v, want present=%v value=%v",
				tc.tally, present, value, tc.wantPresent, tc.wantValue)
		}
	}
}

// An optional voucher is labelled, never dropped: it normalizes exactly like
// the same voucher without the flag.
func TestNormalize_OptionalVoucherIsNotDroppedOrChanged(t *testing.T) {
	plain, err := NormalizeJournal(parsedJournal(t, ""))
	if err != nil {
		t.Fatalf("NormalizeJournal: %v", err)
	}
	optional, err := NormalizeJournal(parsedJournal(t, "Yes"))
	if err != nil {
		t.Fatalf("NormalizeJournal: %v", err)
	}
	optional.IsOptional = nil
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(optional)
	if string(a) != string(b) {
		t.Errorf("optional journal differs beyond is_optional:\n plain    %s\n optional %s", a, b)
	}
}
