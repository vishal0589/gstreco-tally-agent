package tally

import (
	"encoding/xml"
	"strings"
	"testing"
	"time"
)

func TestSourceCaptureHasOnlyDateFilterAndEscapesCompany(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	raw, err := BuildSourceCaptureXML("A & B <Company>", from, to)
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, forbidden := range []string{"$$IsSales", "$$IsPurchase", "ISCANCELLED =", "AMOUNT >", "Import"} {
		if strings.Contains(s, forbidden) {
			t.Fatal(forbidden)
		}
	}
	for _, required := range []string{"A &amp; B &lt;Company&gt;", "20260901", "20260930", "<TALLYREQUEST>Export</TALLYREQUEST>", "($Date &gt;= ##SVFROMDATE) AND ($Date &lt;= ##SVTODATE)", "<FETCH>*</FETCH>"} {
		if !strings.Contains(s, required) {
			t.Fatal(required)
		}
	}
	var v any
	if err = xml.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
}
func TestSourceCountIncludesMalformedVoucherDroppedByTolerantParser(t *testing.T) {
	raw := []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><VOUCHER></VOUCHER><VOUCHER><GUID>g1</GUID><DATE>20260901</DATE></VOUCHER></ENVELOPE>`)
	count, err := CountSourceVoucherElements(raw)
	if err != nil || count != 2 {
		t.Fatalf("count %d, error %v", count, err)
	}
	parsed, err := ParseDayBookV3(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Vouchers) != 1 {
		t.Fatalf("unexpected tolerant parser count %d", len(parsed.Vouchers))
	}
}
func TestSourceCountRejectsTruncatedXML(t *testing.T) {
	if _, err := CountSourceVoucherElements([]byte(`<ENVELOPE><VOUCHER><GUID>g1</GUID>`)); err == nil {
		t.Fatal("accepted truncated XML")
	}
}
