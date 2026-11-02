package syncrun

import (
	"context"
	"fmt"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func gkGlassVoucher(guid, date, vchNo, ref, billRef, amount string) string {
	return fmt.Sprintf(`
    <VOUCHER>
      <DATE>%s</DATE>
      <GUID>%s</GUID>
      <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
      <VOUCHERNUMBER>%s</VOUCHERNUMBER>
      <REFERENCE>%s</REFERENCE>
      <PARTYLEDGERNAME>GK Glass</PARTYLEDGERNAME>
      <PARTYGSTIN>06AAGFG1234Q1Z1</PARTYGSTIN>
      <ISINVOICE>Yes</ISINVOICE>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>GK Glass</LEDGERNAME>
        <AMOUNT>%s</AMOUNT>
        <BILLALLOCATIONS.LIST>
          <NAME>%s</NAME>
          <BILLTYPE>New Ref</BILLTYPE>
          <AMOUNT>%s</AMOUNT>
        </BILLALLOCATIONS.LIST>
      </ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST>
        <LEDGERNAME>Purchase 18%%</LEDGERNAME>
        <AMOUNT>-%s</AMOUNT>
      </ALLLEDGERENTRIES.LIST>
    </VOUCHER>`, date, guid, vchNo, ref, amount, billRef, amount, amount)
}

func rowCarriesFlag(r tally.IngestVoucherRow, flag string) bool {
	for _, f := range r.NormalizationFlags {
		if f == flag {
			return true
		}
	}
	return false
}

// QA v27 B25: two GK Glass vouchers of one run keyed to the same number are
// both sent with duplicate_number_in_period, even across batch chunks.
func TestRunOne_QAv27B25_DuplicateNumbersInOneRunAreFlagged(t *testing.T) {
	resp := `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>` +
		gkGlassVoucher("gk-62", "20260402", "", "", "62", "4220.00") +
		gkGlassVoucher("gk-63", "20260406", "", "", "62", "8540.00") +
		gkGlassVoucher("gk-64", "20260407", "64", "64", "64", "100.00") +
		`</COLLECTION></DATA></BODY></ENVELOPE>`
	tly := &fakeTally{resp: []byte(resp)}
	snd := &fakeSender{}
	req := makeReq()
	req.BatchSize = 1

	if _, err := RunOne(context.Background(), tly, snd, req, nil); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	var rows []tally.IngestVoucherRow
	for _, b := range snd.sent {
		rows = append(rows, b.Batch...)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		dup := rowCarriesFlag(r, tally.FlagDuplicateNumberInPeriod)
		guid := *r.TallyVoucherGUID
		want := guid == "gk-62" || guid == "gk-63"
		if dup != want {
			t.Errorf("%s (%s): duplicate flag = %t, want %t (%v)", guid, r.InvoiceNumber, dup, want, r.NormalizationFlags)
		}
	}
}
