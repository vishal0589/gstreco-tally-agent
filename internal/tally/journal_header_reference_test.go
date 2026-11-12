package tally

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Source header and bill allocations can identify different documents. Capture
// them independently: neither the old allocation nor any amount is rewritten.
const journalHeaderReferenceXML = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION><VOUCHER>
<DATE TYPE="Date">20260401</DATE><GUID>fixture-journal-header</GUID>
<VOUCHERTYPENAME>Journal</VOUCHERTYPENAME><VOUCHERNUMBER>JV/001</VOUCHERNUMBER>
<REFERENCE TYPE="String"> CURRENT/26/17 </REFERENCE><ISCANCELLED>No</ISCANCELLED>
<PARTYLEDGERNAME>Example Supplier</PARTYLEDGERNAME>
<ALLLEDGERENTRIES.LIST><LEDGERNAME>Professional Expenses</LEDGERNAME><AMOUNT>-1000</AMOUNT></ALLLEDGERENTRIES.LIST>
<ALLLEDGERENTRIES.LIST><LEDGERNAME>IGST INPUT</LEDGERNAME><AMOUNT>-180</AMOUNT></ALLLEDGERENTRIES.LIST>
<ALLLEDGERENTRIES.LIST><LEDGERNAME>Example Supplier</LEDGERNAME><ISPARTYLEDGER>Yes</ISPARTYLEDGER><AMOUNT>1080</AMOUNT>
<BILLALLOCATIONS.LIST><NAME>OLDER/25/635</NAME><BILLTYPE>Agst Ref</BILLTYPE><AMOUNT>1080</AMOUNT></BILLALLOCATIONS.LIST>
</ALLLEDGERENTRIES.LIST><ALLLEDGERENTRIES.LIST><LEDGERNAME>TDS</LEDGERNAME><AMOUNT>100</AMOUNT></ALLLEDGERENTRIES.LIST>
</VOUCHER></COLLECTION></DATA></BODY></ENVELOPE>`

func headerJournal(t *testing.T, xml string) RawVoucher {
	t.Helper()
	parsed, err := ParseDayBookV3([]byte(xml))
	if err != nil || len(parsed.Vouchers) != 1 {
		t.Fatalf("parse: %v vouchers=%d", err, len(parsed.Vouchers))
	}
	return parsed.Vouchers[0]
}
func journalJSON(t *testing.T, row *JournalVoucherRow) map[string]json.RawMessage {
	t.Helper()
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err = json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func TestJournalHeaderReference_PreservesConflictingBillReference(t *testing.T) {
	v := headerJournal(t, journalHeaderReferenceXML)
	row, err := NormalizeJournal(v)
	if err != nil {
		t.Fatal(err)
	}
	m := journalJSON(t, row)
	if string(m["voucher_reference"]) != `"CURRENT/26/17"` {
		t.Fatalf("header missing or replaced: %s", m["voucher_reference"])
	}
	if len(row.AgainstInvoiceRefs) != 1 || row.AgainstInvoiceRefs[0].Reference != "OLDER/25/635" {
		t.Fatalf("bill refs changed: %#v", row.AgainstInvoiceRefs)
	}
	without := v
	without.Reference = ""
	baseline, err := NormalizeJournal(without)
	if err != nil {
		t.Fatal(err)
	}
	delete(m, "voucher_reference")
	if !reflect.DeepEqual(m, journalJSON(t, baseline)) {
		t.Fatal("capturing header changed other payload fields")
	}
}
func TestJournalHeaderReference_AbsentDoesNotInventFromBillOrVoucher(t *testing.T) {
	for _, ref := range []string{"", "  "} {
		v := headerJournal(t, journalHeaderReferenceXML)
		v.Reference = ref
		row, err := NormalizeJournal(v)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := journalJSON(t, row)["voucher_reference"]; ok {
			t.Fatal("missing header synthesized")
		}
		if row.AgainstInvoiceRefs[0].Reference != "OLDER/25/635" {
			t.Fatal("bill reference lost")
		}
	}
}
func TestJournalHeaderReference_NoBillReferencesStillCapturesHeader(t *testing.T) {
	v := headerJournal(t, journalHeaderReferenceXML)
	for i := range v.LedgerEntries {
		v.LedgerEntries[i].BillAllocations = nil
	}
	row, err := NormalizeJournal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(journalJSON(t, row)["voucher_reference"]) != `"CURRENT/26/17"` {
		t.Fatal("header missing without bills")
	}
	if len(row.AgainstInvoiceRefs) != 0 {
		t.Fatal("bill allocation invented")
	}
}
func TestJournalHeaderReference_CancelledRemainsSkipped(t *testing.T) {
	v := headerJournal(t, strings.Replace(journalHeaderReferenceXML, "<ISCANCELLED>No</ISCANCELLED>", "<ISCANCELLED>Yes</ISCANCELLED>", 1))
	row, err := NormalizeJournal(v)
	if err != nil || row != nil {
		t.Fatalf("cancelled journal: %#v %v", row, err)
	}
}
