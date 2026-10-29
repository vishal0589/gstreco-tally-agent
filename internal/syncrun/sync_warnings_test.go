package syncrun

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// SYNTHETIC test data, not client data (QA v21 G-A).

const gaCompany = "Synthetic Traders (SYNTHETIC)"

const tallyNotOpen = `<ENVELOPE><HEADER><STATUS>0</STATUS></HEADER><BODY><DATA>Company not open</DATA></BODY></ENVELOPE>`

// gaPurchases is two live purchases and one cancelled one.
const gaPurchases = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260410</DATE>
      <GUID>syn-ga-p1</GUID>
      <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
      <VOUCHERNUMBER>SYN-GA-1</VOUCHERNUMBER>
      <PARTYLEDGERNAME>Synthetic Supplier One</PARTYLEDGERNAME>
      <PARTYGSTIN>27AAAAA0000A1Z5</PARTYGSTIN>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Supplier One</LEDGERNAME><AMOUNT>11800.00</AMOUNT></ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Purchases</LEDGERNAME><AMOUNT>-10000.00</AMOUNT></ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic IGST Input</LEDGERNAME><AMOUNT>-1800.00</AMOUNT></ALLLEDGERENTRIES.LIST>
    </VOUCHER>
    <VOUCHER>
      <DATE>20260411</DATE>
      <GUID>syn-ga-p2</GUID>
      <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
      <VOUCHERNUMBER>SYN-GA-2</VOUCHERNUMBER>
      <PARTYLEDGERNAME>Synthetic Supplier Two</PARTYLEDGERNAME>
      <PARTYGSTIN>27BBBBB0000B1Z5</PARTYGSTIN>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Supplier Two</LEDGERNAME><AMOUNT>5900.00</AMOUNT></ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Purchases</LEDGERNAME><AMOUNT>-5000.00</AMOUNT></ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic IGST Input</LEDGERNAME><AMOUNT>-900.00</AMOUNT></ALLLEDGERENTRIES.LIST>
    </VOUCHER>
    <VOUCHER>
      <DATE>20260412</DATE>
      <GUID>syn-ga-p3-cancelled</GUID>
      <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
      <VOUCHERNUMBER>SYN-GA-3</VOUCHERNUMBER>
      <PARTYLEDGERNAME>Synthetic Supplier Two</PARTYLEDGERNAME>
      <ISCANCELLED>Yes</ISCANCELLED>
    </VOUCHER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

const gaDutyLedgers = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <LEDGER NAME="Synthetic IGST Input"><NAME>Synthetic IGST Input</NAME><PARENT>Duties &amp; Taxes</PARENT><TAXTYPE>GST</TAXTYPE><GSTDUTYHEAD>Integrated Tax</GSTDUTYHEAD></LEDGER>
</COLLECTION></DATA></BODY></ENVELOPE>`

const gaLedgers = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <LEDGER NAME="Synthetic Supplier One"><NAME>Synthetic Supplier One</NAME><PARENT>Sundry Creditors</PARENT><PARTYGSTIN>27AAAAA0000A1Z5</PARTYGSTIN></LEDGER>
  <LEDGER NAME="Synthetic Party Advance"><NAME>Synthetic Party Advance</NAME><PARENT>Sundry Creditors</PARENT><PARTYGSTIN>07AAAAA0000A1Z5</PARTYGSTIN></LEDGER>
</COLLECTION></DATA></BODY></ENVELOPE>`

const gaGroups = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <GROUP NAME="Sundry Creditors"><NAME>Sundry Creditors</NAME><PARENT>Current Liabilities</PARENT></GROUP>
  <GROUP NAME="Current Liabilities"><NAME>Current Liabilities</NAME><PARENT>&#4; Primary</PARENT></GROUP>
</COLLECTION></DATA></BODY></ENVELOPE>`

// gaJournals is one journal with a party GSTIN and one without.
const gaJournals = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <VOUCHER>
      <DATE>20260402</DATE>
      <GUID>syn-ga-j1</GUID>
      <VOUCHERTYPENAME>Journal</VOUCHERTYPENAME>
      <VOUCHERNUMBER>SYN-GA-J1</VOUCHERNUMBER>
      <PARTYLEDGERNAME>Synthetic Party Advance</PARTYLEDGERNAME>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Party Advance</LEDGERNAME><AMOUNT>1180.00</AMOUNT></ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Handling Charges</LEDGERNAME><AMOUNT>-1180.00</AMOUNT></ALLLEDGERENTRIES.LIST>
    </VOUCHER>
    <VOUCHER>
      <DATE>20260403</DATE>
      <GUID>syn-ga-j2</GUID>
      <VOUCHERTYPENAME>Journal</VOUCHERTYPENAME>
      <VOUCHERNUMBER>SYN-GA-J2</VOUCHERNUMBER>
      <PARTYLEDGERNAME>Synthetic Supplier One</PARTYLEDGERNAME>
      <PARTYGSTIN>27AAAAA0000A1Z5</PARTYGSTIN>
      <ISCANCELLED>No</ISCANCELLED>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Supplier One</LEDGERNAME><AMOUNT>500.00</AMOUNT></ALLLEDGERENTRIES.LIST>
      <ALLLEDGERENTRIES.LIST><LEDGERNAME>Synthetic Handling Charges</LEDGERNAME><AMOUNT>-500.00</AMOUNT></ALLLEDGERENTRIES.LIST>
    </VOUCHER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

// gaMasters answers each master request with its fixture, or with "company
// not open" for the masters named in failing.
func gaMasters(vouchers string, failing ...string) *fakeTally {
	fails := map[string]bool{}
	for _, f := range failing {
		fails[f] = true
	}
	answer := func(kind, ok string) []byte {
		if fails[kind] {
			return []byte(tallyNotOpen)
		}
		return []byte(ok)
	}
	return &fakeTally{respond: func(body []byte) []byte {
		switch b := string(body); {
		case isDutyLedgerRequest(body):
			return answer("duty", gaDutyLedgers)
		case strings.Contains(b, "GstrecoLedgerGstinCollection"):
			return answer("ledgers", gaLedgers)
		case strings.Contains(b, "GstrecoGroupTreeCollection"):
			return answer("groups", gaGroups)
		}
		return []byte(vouchers)
	}}
}

func gaReq(kind string) Request {
	req := makeReq()
	req.TallyCompany = gaCompany
	req.BatchSize = 1
	if kind != "" {
		req.KindName = kind
		req.TallyKind = tally.VoucherKind(kind)
	}
	return req
}

func resetMastersCaches(t *testing.T) {
	t.Helper()
	resetDutyLedgerCache()
	resetLedgerGSTINCache()
	t.Cleanup(func() {
		resetDutyLedgerCache()
		resetLedgerGSTINCache()
	})
}

var gaNow = time.Date(2026, 10, 29, 9, 30, 0, 0, time.UTC)

func fixSyncWarningClock(t *testing.T) {
	t.Helper()
	prev := syncWarningNow
	syncWarningNow = func() time.Time { return gaNow }
	t.Cleanup(func() { syncWarningNow = prev })
}

// jsonKeys is a request body's top-level JSON keys.
func jsonKeys(t *testing.T, body any) []string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func warningByCode(ws []tally.SyncWarning, code string) *tally.SyncWarning {
	for i := range ws {
		if ws[i].Code == code {
			return &ws[i]
		}
	}
	return nil
}

func TestRunOne_DutyLedgerMastersUnavailableReachesEveryBatch(t *testing.T) {
	resetMastersCaches(t)
	fixSyncWarningClock(t)

	snd := &fakeSender{}
	res, err := RunOne(context.Background(), gaMasters(gaPurchases, "duty"), snd, gaReq(""), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	// The local log line stays.
	logged := false
	for _, w := range res.ParseWarnings {
		logged = logged || strings.Contains(w, "duty ledger masters unavailable")
	}
	if !logged {
		t.Errorf("ParseWarnings = %v, want the local log line kept", res.ParseWarnings)
	}
	if len(snd.sent) != 2 {
		t.Fatalf("sent %d batches, want 2", len(snd.sent))
	}
	for i, body := range snd.sent {
		w := warningByCode(body.SyncWarnings, tally.SyncWarningDutyLedgerMastersUnavailable)
		if w == nil {
			t.Fatalf("batch %d sync_warnings = %+v, want %s", i+1, body.SyncWarnings, tally.SyncWarningDutyLedgerMastersUnavailable)
		}
		if w.TallyCompany != gaCompany {
			t.Errorf("batch %d company = %q, want %q", i+1, w.TallyCompany, gaCompany)
		}
		if w.OccurredAt != "2026-10-29T09:30:00Z" {
			t.Errorf("batch %d occurred_at = %q", i+1, w.OccurredAt)
		}
		if !strings.Contains(w.Message, "duty ledger masters unavailable") {
			t.Errorf("batch %d message = %q", i+1, w.Message)
		}
		// Every live voucher was classified without the masters; the
		// cancelled one was not.
		if want := []string{"syn-ga-p1", "syn-ga-p2"}; !reflect.DeepEqual(w.AffectedVoucherGUIDs, want) {
			t.Errorf("batch %d GUIDs = %v, want %v", i+1, w.AffectedVoucherGUIDs, want)
		}
		if len(body.SyncWarnings) != 1 {
			t.Errorf("batch %d sync_warnings = %+v, want only the duty-ledger warning", i+1, body.SyncWarnings)
		}
	}
	if len(res.SyncWarnings) != 1 {
		t.Errorf("Result.SyncWarnings = %+v", res.SyncWarnings)
	}

	// Without the masters, the GST-named leg is held unclassified, never
	// counted as GST or as taxable value from its name.
	for _, body := range snd.sent {
		for _, row := range body.Batch {
			if row.IGST != 0 {
				t.Errorf("row %s: igst %v, want 0 without the masters", *row.TallyVoucherGUID, row.IGST)
			}
			for _, leg := range row.VoucherLegs {
				if leg.LedgerName == "Synthetic IGST Input" && (leg.Classification.AgentRole != tally.LegUncertain || leg.Classification.Method != "unclassified") {
					t.Errorf("row %s: IGST leg classified %+v, want uncertain / unclassified", *row.TallyVoucherGUID, leg.Classification)
				}
			}
		}
	}
}

func TestRunOne_EmptyDutyLedgerMastersWarn(t *testing.T) {
	resetMastersCaches(t)
	fixSyncWarningClock(t)

	tly := gaMasters(gaPurchases)
	inner := tly.respond
	tly.respond = func(body []byte) []byte {
		if isDutyLedgerRequest(body) {
			return []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION></COLLECTION></DATA></BODY></ENVELOPE>`)
		}
		return inner(body)
	}
	snd := &fakeSender{}
	if _, err := RunOne(context.Background(), tly, snd, gaReq(""), nil); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	w := warningByCode(snd.sent[len(snd.sent)-1].SyncWarnings, tally.SyncWarningDutyLedgerMastersUnavailable)
	if w == nil || !strings.Contains(w.Message, "came back empty") {
		t.Fatalf("final batch sync_warnings = %+v", snd.sent[len(snd.sent)-1].SyncWarnings)
	}
}

func TestRunOne_MastersFineSendsNoSyncWarnings(t *testing.T) {
	for _, kind := range []string{"", "journal"} {
		t.Run("kind="+kind, func(t *testing.T) {
			resetMastersCaches(t)
			vouchers := gaPurchases
			if kind == "journal" {
				vouchers = gaJournals
			}
			snd := &fakeSender{}
			res, err := RunOne(context.Background(), gaMasters(vouchers), snd, gaReq(kind), nil)
			if err != nil {
				t.Fatalf("RunOne: %v", err)
			}
			if len(res.SyncWarnings) != 0 {
				t.Errorf("Result.SyncWarnings = %+v, want none", res.SyncWarnings)
			}
			var bodies []any
			for _, b := range snd.sent {
				bodies = append(bodies, b)
			}
			for _, b := range snd.journalSent {
				bodies = append(bodies, b)
			}
			if len(bodies) == 0 {
				t.Fatal("nothing sent")
			}
			for i, b := range bodies {
				for _, k := range jsonKeys(t, b) {
					if k == "sync_warnings" {
						t.Errorf("batch %d carries sync_warnings with healthy masters", i+1)
					}
				}
			}
		})
	}
}

// The field is the only change to the payload's shape.
func TestRunOne_SyncWarningsLeaveThePayloadShapeUnchanged(t *testing.T) {
	resetMastersCaches(t)
	healthy := &fakeSender{}
	if _, err := RunOne(context.Background(), gaMasters(gaPurchases), healthy, gaReq(""), nil); err != nil {
		t.Fatal(err)
	}
	resetMastersCaches(t)
	failing := &fakeSender{}
	if _, err := RunOne(context.Background(), gaMasters(gaPurchases, "duty"), failing, gaReq(""), nil); err != nil {
		t.Fatal(err)
	}
	for i := range healthy.sent {
		want := append(jsonKeys(t, healthy.sent[i]), "sync_warnings")
		sort.Strings(want)
		if got := jsonKeys(t, failing.sent[i]); !reflect.DeepEqual(got, want) {
			t.Errorf("batch %d keys = %v, want %v", i+1, got, want)
		}
		h, f := healthy.sent[i], failing.sent[i]
		if h.RunID != f.RunID || h.RequestID != f.RequestID || h.IsFinal != f.IsFinal || h.TallyCompany != f.TallyCompany || len(h.Batch) != len(f.Batch) {
			t.Errorf("batch %d envelope changed: %+v vs %+v", i+1, h, f)
		}
	}
}

func TestRunOne_LedgerMastersFetchFailureReachesJournalBatches(t *testing.T) {
	resetMastersCaches(t)
	fixSyncWarningClock(t)

	snd := &fakeSender{}
	res, err := RunOne(context.Background(), gaMasters(gaJournals, "ledgers"), snd, gaReq("journal"), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(snd.journalSent) != 2 {
		t.Fatalf("sent %d journal batches, want 2", len(snd.journalSent))
	}
	for i, body := range snd.journalSent {
		w := warningByCode(body.SyncWarnings, tally.SyncWarningMastersFetchFailed)
		if w == nil {
			t.Fatalf("batch %d sync_warnings = %+v, want %s", i+1, body.SyncWarnings, tally.SyncWarningMastersFetchFailed)
		}
		if w.TallyCompany != gaCompany || w.OccurredAt != "2026-10-29T09:30:00Z" {
			t.Errorf("batch %d warning = %+v", i+1, w)
		}
		// Only the journal without a voucher GSTIN needed the ledger masters.
		if want := []string{"syn-ga-j1"}; !reflect.DeepEqual(w.AffectedVoucherGUIDs, want) {
			t.Errorf("batch %d GUIDs = %v, want %v", i+1, w.AffectedVoucherGUIDs, want)
		}
	}
	logged := false
	for _, w := range res.ParseWarnings {
		logged = logged || strings.Contains(w, "ledger GSTIN lookup failed")
	}
	if !logged {
		t.Errorf("ParseWarnings = %v, want the local log line kept", res.ParseWarnings)
	}
}

func TestRunOne_LedgerMastersFetchFailureReachesPaymentBatches(t *testing.T) {
	resetMastersCaches(t)
	payments := strings.ReplaceAll(gaJournals, "<VOUCHERTYPENAME>Journal</VOUCHERTYPENAME>", "<VOUCHERTYPENAME>Payment</VOUCHERTYPENAME>")
	snd := &fakeSender{}
	if _, err := RunOne(context.Background(), gaMasters(payments, "ledgers"), snd, gaReq("payment"), nil); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(snd.paymentSent) == 0 {
		t.Fatal("no payment batch sent")
	}
	for i, body := range snd.paymentSent {
		if warningByCode(body.SyncWarnings, tally.SyncWarningMastersFetchFailed) == nil {
			t.Errorf("batch %d sync_warnings = %+v", i+1, body.SyncWarnings)
		}
	}
}

// The group tree failure used to be silent: legs were then classed by name.
// It is reported, and still reported by a later lane served from the cache.
func TestRunOne_GroupTreeFailureIsReportedAndCached(t *testing.T) {
	resetMastersCaches(t)
	fixSyncWarningClock(t)

	tly := gaMasters(gaPurchases, "groups")
	for run := 0; run < 2; run++ {
		snd := &fakeSender{}
		if _, err := RunOne(context.Background(), tly, snd, gaReq(""), nil); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
		if run == 1 {
			// Later lanes read the cache; the warning keeps its time.
			syncWarningNow = func() time.Time { return gaNow.Add(time.Minute) }
		}
		for i, body := range snd.sent {
			w := warningByCode(body.SyncWarnings, tally.SyncWarningMastersFetchFailed)
			if w == nil || !strings.Contains(w.Message, "group") {
				t.Fatalf("run %d batch %d sync_warnings = %+v", run, i+1, body.SyncWarnings)
			}
			if w.OccurredAt != "2026-10-29T09:30:00Z" {
				t.Errorf("run %d occurred_at = %q, want the failure's time", run, w.OccurredAt)
			}
			if want := []string{"syn-ga-p1", "syn-ga-p2"}; !reflect.DeepEqual(w.AffectedVoucherGUIDs, want) {
				t.Errorf("run %d GUIDs = %v, want %v", run, w.AffectedVoucherGUIDs, want)
			}
		}
	}
}

func TestRunOne_LedgerMastersParseFailureIsMastersParseFailed(t *testing.T) {
	resetMastersCaches(t)
	tly := gaMasters(gaPurchases)
	inner := tly.respond
	tly.respond = func(body []byte) []byte {
		if strings.Contains(string(body), "GstrecoLedgerGstinCollection") {
			return []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION><LEDGER NAME="x"><NAME>x</NAME></COLL`)
		}
		return inner(body)
	}
	snd := &fakeSender{}
	if _, err := RunOne(context.Background(), tly, snd, gaReq(""), nil); err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if w := warningByCode(snd.sent[0].SyncWarnings, tally.SyncWarningMastersParseFailed); w == nil {
		t.Errorf("sync_warnings = %+v, want %s", snd.sent[0].SyncWarnings, tally.SyncWarningMastersParseFailed)
	}
}

func TestAddSyncWarning_CapsAndDeduplicates(t *testing.T) {
	var res Result

	long := strings.Repeat("é", tally.MaxSyncWarningMessageRunes+200)
	guids := make([]string, 0, tally.MaxSyncWarningVoucherGUIDs+200)
	for i := 0; i < tally.MaxSyncWarningVoucherGUIDs+200; i++ {
		guids = append(guids, fmt.Sprintf("syn-guid-%d", i))
	}
	guids = append(guids, "syn-guid-0", "")
	res.addSyncWarning(newSyncWarning(tally.SyncWarningMastersFetchFailed, gaCompany, long, gaNow, guids))

	w := res.SyncWarnings[0]
	if n := len([]rune(w.Message)); n != tally.MaxSyncWarningMessageRunes {
		t.Errorf("message runes = %d, want %d", n, tally.MaxSyncWarningMessageRunes)
	}
	if len(w.AffectedVoucherGUIDs) != tally.MaxSyncWarningVoucherGUIDs {
		t.Errorf("GUIDs = %d, want %d", len(w.AffectedVoucherGUIDs), tally.MaxSyncWarningVoucherGUIDs)
	}
	if w.AffectedVoucherGUIDs[0] != "syn-guid-0" || w.AffectedVoucherGUIDs[1] != "syn-guid-1" {
		t.Errorf("GUIDs out of order: %v", w.AffectedVoucherGUIDs[:2])
	}

	// The same (code, company, message) is stored once.
	res.addSyncWarning(newSyncWarning(tally.SyncWarningMastersFetchFailed, gaCompany, long, gaNow, nil))
	if len(res.SyncWarnings) != 1 {
		t.Errorf("duplicate stored: %d warnings", len(res.SyncWarnings))
	}

	for i := 0; i < tally.MaxSyncWarnings+20; i++ {
		res.addSyncWarning(newSyncWarning(tally.SyncWarningMastersParseFailed, gaCompany, fmt.Sprintf("warning %d", i), gaNow, nil))
	}
	if len(res.SyncWarnings) != tally.MaxSyncWarnings {
		t.Errorf("warnings = %d, want %d", len(res.SyncWarnings), tally.MaxSyncWarnings)
	}

	// No GUIDs: the field is omitted, not sent empty.
	raw, _ := json.Marshal(res.SyncWarnings[1])
	if strings.Contains(string(raw), "affected_voucher_guids") {
		t.Errorf("empty GUID list sent: %s", raw)
	}
}
