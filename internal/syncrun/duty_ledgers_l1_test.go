package syncrun

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// QA v27 L1: the duty-ledger graph must be found on companies that renamed
// Duties & Taxes, fall back to Tally's grouping when the duty masters cannot
// be read, and, with neither, every debit note is still sent, flagged
// tax_legs_uncertain, with the masters warning naming it. SYNTHETIC data
// (../tally/testdata/synthetic-l1-*.xml), not client data.

func l1Fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../tally/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func l1DebitNoteReq() Request {
	req := makeReq()
	req.TallyCompany = "Synthetic KS Steels (SYNTHETIC)"
	req.KindName = "debit_note"
	req.TallyKind = tally.VoucherDebitNote
	req.IngestKind = tally.IngestKindDebitNote
	return req
}

// l1Tally answers each master request; duty answers the duty-ledger request
// given the CHILDOF group it names.
func l1Tally(t *testing.T, duty func(group string) []byte, ledgers, groups []byte) *fakeTally {
	notes := l1Fixture(t, "synthetic-l1-debit-notes.xml")
	return &fakeTally{respond: func(body []byte) []byte {
		b := string(body)
		switch {
		case isDutyLedgerRequest(body):
			i := strings.Index(b, "<CHILDOF>")
			j := strings.Index(b, "</CHILDOF>")
			return duty(b[i+len("<CHILDOF>") : j])
		case strings.Contains(b, "GstrecoLedgerGstinCollection"):
			return ledgers
		case strings.Contains(b, "GstrecoGroupTreeCollection"):
			return groups
		}
		return notes
	}}
}

func l1Rows(t *testing.T, snd *fakeSender) map[string]tally.IngestVoucherRow {
	t.Helper()
	rows := map[string]tally.IngestVoucherRow{}
	for _, body := range snd.sent {
		if body.Kind != tally.IngestKindDebitNote {
			t.Errorf("batch kind = %s", body.Kind)
		}
		for _, r := range body.Batch {
			rows[*r.TallyVoucherGUID] = r
		}
	}
	if len(rows) != 2 {
		t.Fatalf("sent rows %v, want both debit notes", rows)
	}
	return rows
}

func hasRowFlag(r tally.IngestVoucherRow, flag string) bool {
	for _, f := range r.NormalizationFlags {
		if f == flag {
			return true
		}
	}
	return false
}

// The company renamed Duties & Taxes: CHILDOF "Duties & Taxes" finds nothing,
// the group tree names the group, and the masters read under that name make
// the legs GST from their duty masters.
func TestRunOne_L1RenamedDutiesGroupStillYieldsTheGraph(t *testing.T) {
	resetMastersCaches(t)
	var asked []string
	duty := func(group string) []byte {
		asked = append(asked, group)
		if group == "Duties and Taxes" {
			return l1Fixture(t, "synthetic-l1-duty-ledgers-namelist.xml")
		}
		return []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION></COLLECTION></DATA></BODY></ENVELOPE>`)
	}
	tly := l1Tally(t, duty, l1Fixture(t, "synthetic-l1-ledgers.xml"), l1Fixture(t, "synthetic-l1-groups-renamed.xml"))
	snd := &fakeSender{}
	res, err := RunOne(context.Background(), tly, snd, l1DebitNoteReq(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(asked, []string{"Duties and Taxes"}) {
		t.Errorf("duty-ledger requests named %v, want the renamed group", asked)
	}
	if len(res.SyncWarnings) != 0 {
		t.Errorf("sync warnings = %+v, want none", res.SyncWarnings)
	}
	r := l1Rows(t, snd)["syn-l1-dn-1"]
	if r.CGST != 900 || r.SGST != 900 || r.TaxableValue != 10000 || hasRowFlag(r, tally.FlagTaxLegsUncertain) {
		t.Errorf("row = taxable %v cgst %v sgst %v flags %v", r.TaxableValue, r.CGST, r.SGST, r.NormalizationFlags)
	}
	for _, leg := range r.VoucherLegs {
		if leg.LedgerName == "SGST INPUT" && leg.Classification.Evidence != tally.EvidenceDutyMaster {
			t.Errorf("SGST INPUT evidence = %s", leg.Classification.Evidence)
		}
	}
}

// The duty masters cannot be read; the ledger masters and group tree place
// the GST ledgers under Duties & Taxes, so the graph comes from Tally's
// grouping. The warning still reaches sync health.
func TestRunOne_L1MastersUnavailableFallsBackToTheGroupTree(t *testing.T) {
	resetMastersCaches(t)
	fixSyncWarningClock(t)
	tly := l1Tally(t, func(string) []byte { return []byte(tallyNotOpen) },
		l1Fixture(t, "synthetic-l1-ledgers.xml"), l1Fixture(t, "synthetic-l1-groups-renamed.xml"))
	snd := &fakeSender{}
	res, err := RunOne(context.Background(), tly, snd, l1DebitNoteReq(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := l1Rows(t, snd)
	if r := rows["syn-l1-dn-1"]; r.CGST != 900 || r.SGST != 900 || hasRowFlag(r, tally.FlagTaxLegsUncertain) {
		t.Errorf("dn-1 = cgst %v sgst %v flags %v", r.CGST, r.SGST, r.NormalizationFlags)
	}
	if r := rows["syn-l1-dn-2"]; r.IGST != 900 || r.TaxableValue != 5000 {
		t.Errorf("dn-2 = taxable %v igst %v", r.TaxableValue, r.IGST)
	}
	w := warningByCode(res.SyncWarnings, tally.SyncWarningDutyLedgerMastersUnavailable)
	if w == nil || !strings.Contains(w.Message, "taken from the group tree") {
		t.Fatalf("sync warnings = %+v", res.SyncWarnings)
	}
	if !reflect.DeepEqual(w.AffectedVoucherGUIDs, []string{"syn-l1-dn-1", "syn-l1-dn-2"}) {
		t.Errorf("GUIDs = %v", w.AffectedVoucherGUIDs)
	}
}

// No graph at all: every debit note is sent, none with a guessed tax, each
// flagged tax_legs_uncertain, and the warning names them all.
func TestRunOne_L1NoGraphSendsEveryDebitNoteFlagged(t *testing.T) {
	resetMastersCaches(t)
	fixSyncWarningClock(t)
	tly := l1Tally(t, func(string) []byte { return []byte(tallyNotOpen) },
		[]byte(tallyNotOpen), []byte(tallyNotOpen))
	snd := &fakeSender{}
	res, err := RunOne(context.Background(), tly, snd, l1DebitNoteReq(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.DroppedOnNormalize != 0 {
		t.Errorf("dropped %d", res.DroppedOnNormalize)
	}
	for guid, r := range l1Rows(t, snd) {
		if r.IGST+r.CGST+r.SGST != 0 {
			t.Errorf("%s: tax guessed from names", guid)
		}
		if !hasRowFlag(r, tally.FlagTaxLegsUncertain) {
			t.Errorf("%s: flags = %v", guid, r.NormalizationFlags)
		}
	}
	for _, body := range snd.sent {
		w := warningByCode(body.SyncWarnings, tally.SyncWarningDutyLedgerMastersUnavailable)
		if w == nil || !reflect.DeepEqual(w.AffectedVoucherGUIDs, []string{"syn-l1-dn-1", "syn-l1-dn-2"}) {
			t.Errorf("batch sync warnings = %+v", body.SyncWarnings)
		}
	}
}

// A voucher the agent cannot read into a row is named in sync health, never
// dropped silently.
func TestRunOne_L1UnreadableVoucherIsNamedInSyncHealth(t *testing.T) {
	resetMastersCaches(t)
	fixSyncWarningClock(t)
	notes := `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260417</DATE>
      <GUID>syn-l1-dn-noparty</GUID>
      <VOUCHERTYPENAME>Debit Note - Material Rejection</VOUCHERTYPENAME>
      <VOUCHERNUMBER>SYN-DN-003</VOUCHERNUMBER>
      <PARTYLEDGERNAME>Synthetic Missing Party</PARTYLEDGERNAME>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Purchase Return</LEDGERNAME><AMOUNT>1000.00</AMOUNT></ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Cash</LEDGERNAME><AMOUNT>-1000.00</AMOUNT></ALLLEDGERENTRIES.LIST>
    </VOUCHER>
  </COLLECTION></DATA></BODY></ENVELOPE>`
	tly := &fakeTally{respond: func(body []byte) []byte {
		if isDutyLedgerRequest(body) {
			return []byte(stubDutyLedgers)
		}
		return []byte(notes)
	}}
	res, err := RunOne(context.Background(), tly, &fakeSender{}, l1DebitNoteReq(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.DroppedOnNormalize != 1 {
		t.Fatalf("DroppedOnNormalize = %d", res.DroppedOnNormalize)
	}
	w := warningByCode(res.SyncWarnings, tally.SyncWarningVouchersNotNormalized)
	if w == nil || !reflect.DeepEqual(w.AffectedVoucherGUIDs, []string{"syn-l1-dn-noparty"}) || !strings.Contains(w.Message, "no party ledger") {
		t.Errorf("sync warnings = %+v", res.SyncWarnings)
	}
}
