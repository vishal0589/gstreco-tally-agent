package tally

import (
	"strings"
	"testing"
	"time"
)

func TestVoucherCounterIsMetadataOnly(t *testing.T) {
	raw := []byte(`<ENVELOPE><BODY><DESC><CMPINFO><VOUCHER>12</VOUCHER></CMPINFO></DESC><DATA><COLLECTION><VOUCHER><DATE>20260331</DATE><GUID>synthetic</GUID><ALLLEDGERENTRIES.LIST><LEDGERNAME>IGST INPUT</LEDGERNAME><AMOUNT>-18</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER></COLLECTION></DATA></BODY></ENVELOPE>`)
	r, e := ParseTaxLedgerVoucherReply(raw)
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Vouchers) != 1 {
		t.Fatalf("got%d vouchers", len(r.Vouchers))
	}
	s := SelectTaxLedgerVouchers(r, map[string]bool{TaxLedgerKey("IGST INPUT"): true}, time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC))
	if !s.Snapshot.Complete || s.Snapshot.DateUnreadable != 0 || len(s.Rows) != 1 {
		t.Fatalf("%+v", s)
	}
}
func TestRealBlankVoucherStillMakesSnapshotIncomplete(t *testing.T) {
	r, e := ParseTaxLedgerVoucherReply([]byte(`<ENVELOPE><BODY><DATA><COLLECTION><VOUCHER>0</VOUCHER></COLLECTION></DATA></BODY></ENVELOPE>`))
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Vouchers) != 1 {
		t.Fatal("real malformed voucher lost")
	}
	s := SelectTaxLedgerVouchers(r, nil, time.Time{}, time.Time{})
	if s.Snapshot.DateUnreadable != 1 || s.Snapshot.Complete {
		t.Fatal("bad date hidden")
	}
}
func TestVoucherCounterRejectsBadMetadata(t *testing.T) {
	for _, counter := range []string{"-1", "not-a-count", "<DATE>20260331</DATE>"} {
		_, e := ParseTaxLedgerVoucherReply([]byte(strings.Replace(`<ENVELOPE><BODY><DESC><CMPINFO><VOUCHER>COUNTER</VOUCHER></CMPINFO></DESC></BODY></ENVELOPE>`, "COUNTER", counter, 1)))
		if e == nil {
			t.Fatalf("accepted%s", counter)
		}
	}
}
