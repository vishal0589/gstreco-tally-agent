package syncrun

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func unicodeWire(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := []byte{0xff, 0xfe}
	for _, v := range u {
		b = append(b, byte(v), byte(v>>8))
	}
	return b
}
func fromUnicodeWire(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 2 || b[0] != 0xff || b[1] != 0xfe || len(b)%2 != 0 {
		t.Fatalf("request is not BOM-marked UTF-16LE")
	}
	u := make([]uint16, (len(b)-2)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2+i*2:])
	}
	return string(utf16.Decode(u))
}

// Real transport, no mocked PostXML: the audit lane must request Unicode,
// while the ledger-list request keeps its existing transport.
func TestTaxLedgerVoucher_HTTPUnicodeExactCharactersAndStrictReply(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "still-refuses-corrupt"}[bad], func(t *testing.T) {
			requests := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				b, _ := io.ReadAll(r.Body)
				if strings.Contains(string(b), "GstrecoTaxLedgerCollection") {
					if r.Header.Get("Content-Type") != "text/xml" {
						t.Error("ledger-list transport changed")
					}
					w.Write(tallyFixture(t, "tax-ledgers-kss-092026.xml"))
					return
				}
				if r.Header.Get("Content-Type") != "UTF-16" {
					t.Errorf("audit voucher Content-Type=%q, want UTF-16", r.Header.Get("Content-Type"))
					w.Write([]byte("bad"))
					return
				}
				xml := fromUnicodeWire(t, b)
				if !strings.Contains(xml, "GstrecoTaxLedgerVoucherCollection") || !strings.Contains(xml, "Synthetic ₹ हिन्दी 😀") {
					t.Error("request lost Unicode scope or collection")
				}
				if bad {
					w.Write([]byte{0xff, 0xfe, 0x3c})
					return
				}
				w.Write(unicodeWire(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DESC><CMPINFO><VOUCHER>1</VOUCHER></CMPINFO></DESC><DATA><COLLECTION><VOUCHER><GUID>synthetic-unicode</GUID><DATE>20260920</DATE><PARTYLEDGERNAME>₹ हिन्दी 😀 ‘quote’</PARTYLEDGERNAME><ALLLEDGERENTRIES.LIST><LEDGERNAME>IGST INPUT</LEDGERNAME><AMOUNT>-18.00</AMOUNT></ALLLEDGERENTRIES.LIST><ALLLEDGERENTRIES.LIST><LEDGERNAME>Offset</LEDGERNAME><AMOUNT>18.00</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER></COLLECTION></DATA></BODY></ENVELOPE>`))
			}))
			defer srv.Close()
			req := kssReq()
			req.TallyCompany = "Synthetic ₹ हिन्दी 😀"
			req.TallyEndpoint = srv.URL
			snd := &fakeSender{}
			_, err := RunOne(context.Background(), tally.NewClient(srv.URL), snd, req, nil)
			if requests != 2 {
				t.Fatalf("requests=%d, want ledger+voucher only", requests)
			}
			if bad {
				if err == nil {
					t.Fatal("corrupt Unicode reply accepted")
				}
				if len(snd.taxVoucherSent) != 1 || snd.taxVoucherSent[0].Snapshot.Complete {
					t.Fatal("corrupt reply did not mark snapshot incomplete")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(snd.taxVoucherSent) != 1 || len(snd.taxVoucherSent[0].Batch) != 1 {
				t.Fatalf("unexpected batch: %+v", snd.taxVoucherSent)
			}
			b := snd.taxVoucherSent[0]
			if !b.Snapshot.Complete || b.Snapshot.VouchersReturned != 1 || b.Batch[0].PartyLedgerName == nil || *b.Batch[0].PartyLedgerName != "₹ हिन्दी 😀 ‘quote’" || b.Batch[0].Legs[0].Amount == nil || *b.Batch[0].Legs[0].Amount != "-18.00" {
				t.Fatalf("source changed: %+v", b)
			}
		})
	}
}
