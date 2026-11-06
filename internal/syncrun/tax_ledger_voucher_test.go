package syncrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// QA 05-Nov tax_ledger_voucher lane: KSS-shaped synthetic fixtures live in
// internal/tally/testdata.

const kssCompany = "KSS INTERIORS INDIA PVT. LTD C"
const kssCompanyGUID = "54b5dca1-178d-43b8-9aa7-fd652aed0538"

func tallyFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "tally", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// kssTally answers the tax ledger list and the voucher export.
func kssTally(t *testing.T, vouchers []byte) *fakeTally {
	ledgers := tallyFixture(t, "tax-ledgers-kss-092026.xml")
	return &fakeTally{respond: func(body []byte) []byte {
		switch {
		case bytes.Contains(body, []byte("GstrecoTaxLedgerCollection")):
			return ledgers
		case bytes.Contains(body, []byte("GstrecoTaxLedgerVoucherCollection")):
			return vouchers
		}
		t.Errorf("unexpected Tally request: %s", body)
		return nil
	}}
}

func kssReq() Request {
	return Request{
		KindName:      "tax_ledger_voucher",
		TallyCompany:  kssCompany,
		TallyEndpoint: "http://127.0.0.1:9553",
		From:          time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:            time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		RunID:         "00000000-0000-4000-8000-000000000001",
		RunKind:       "incremental",
		CompanyGUID:   kssCompanyGUID,
	}
}

func emptyVoucherReply() []byte {
	return []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION></COLLECTION></DATA></BODY></ENVELOPE>`)
}

func TestTaxLedgerVoucher_KSSMonthOneFinalBatchWithSnapshot(t *testing.T) {
	snd := &fakeSender{}
	res, err := RunOne(context.Background(), kssTally(t, tallyFixture(t, "tax-ledger-vouchers-kss-092026.xml")), snd, kssReq(), nil)
	if err != nil {
		t.Fatalf("RunOne: %v", err)
	}
	if len(snd.taxVoucherSent) != 1 || res.BatchesSent != 1 || res.RowCount != 5 {
		t.Fatalf("sends=%d sent=%d rows=%d, want 1/1/5", len(snd.taxVoucherSent), res.BatchesSent, res.RowCount)
	}
	if len(snd.sent)+len(snd.taxLedgerSent) != 0 {
		t.Fatal("only the tax-ledger voucher endpoint may be used")
	}
	b := snd.taxVoucherSent[0]
	if b.Kind != tally.AccountingBatchTaxLedgerVoucher || !b.IsFinal || b.Snapshot == nil {
		t.Fatalf("kind=%q is_final=%t snapshot=%v", b.Kind, b.IsFinal, b.Snapshot)
	}
	want := tally.TaxLedgerVoucherSnapshot{Complete: true, VouchersReturned: 7, VouchersSent: 5, CancelledTouching: 1}
	if *b.Snapshot != want {
		t.Errorf("snapshot=%+v, want %+v", *b.Snapshot, want)
	}
	if b.CompanyGUID == nil || *b.CompanyGUID != kssCompanyGUID || *b.TallyEndpoint != "http://127.0.0.1:9553" ||
		b.RunKind != "incremental" || *b.PeriodFrom != "2026-09-01" || *b.PeriodTo != "2026-09-30" ||
		b.RunID != kssReq().RunID || b.RequestID != kssReq().RunID+"-1" || len(b.SyncWarnings) != 0 {
		t.Errorf("batch fields: %+v", b)
	}

	raw, _ := json.Marshal(b)
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	if wire["kind"] != "tax_ledger_voucher" || wire["company_guid"] != kssCompanyGUID || wire["is_final"] != true {
		t.Errorf("wire: %s", raw)
	}
	snap := wire["snapshot"].(map[string]any)
	for _, k := range []string{"complete", "vouchers_returned", "vouchers_sent", "outside_window", "cancelled_touching", "date_unreadable"} {
		if _, ok := snap[k]; !ok {
			t.Errorf("snapshot missing %s: %s", k, raw)
		}
	}
	first := wire["batch"].([]any)[0].(map[string]any)
	if first["tally_voucher_guid"] != "54b5dca1-178d-43b8-9aa7-fd652aed0538-000024da" || first["voucher_date"] != "2026-09-30" {
		t.Errorf("first voucher: %v", first)
	}
	for _, k := range []string{"master_id", "alter_id", "voucher_date_raw", "voucher_type_name", "voucher_number", "reference",
		"party_ledger_name", "party_gstin", "is_invoice", "is_optional", "is_rcm_applicable", "legs", "problems"} {
		if _, ok := first[k]; !ok {
			t.Errorf("voucher missing key %s", k)
		}
	}
	leg := first["legs"].([]any)[0].(map[string]any)
	if leg["ledger_name"] != "IGST OUTPUT" || leg["amount"] != "-162000.00" {
		t.Errorf("leg must carry the amount as a string in Tally sign: %v", leg)
	}
	if !strings.Contains(string(raw), `"problems":[]`) {
		t.Errorf("problems must marshal as []: %s", raw)
	}
}

// The lane's ledger set is the tax_ledger lane's: the exact same request.
func TestTaxLedgerVoucher_LedgerSetIsTheTaxLedgerLaneRequest(t *testing.T) {
	var got [][]byte
	inner := kssTally(t, emptyVoucherReply())
	tly := &fakeTally{respond: func(body []byte) []byte {
		got = append(got, append([]byte(nil), body...))
		return inner.respond(body)
	}}
	if _, err := RunOne(context.Background(), tly, &fakeSender{}, kssReq(), nil); err != nil {
		t.Fatal(err)
	}
	want, _ := tally.BuildTaxLedgerXML(tally.TaxLedgerRequest{Company: kssCompany, From: kssReq().From, To: kssReq().To})
	if len(got) != 2 || !bytes.Equal(got[0], want) {
		t.Fatalf("first Tally request must be the tax_ledger lane's own envelope; got %d requests", len(got))
	}
}

// A month with no matching voucher still sends one final empty batch with
// complete=true, on a scheduled tick too (no sync request).
func TestTaxLedgerVoucher_ProvenEmptyMonthOnScheduledTick(t *testing.T) {
	snd := &fakeSender{}
	req := kssReq()
	req.SyncRequestID = ""
	res, err := RunOne(context.Background(), kssTally(t, emptyVoucherReply()), snd, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(snd.taxVoucherSent) != 1 || res.BatchesSent != 1 {
		t.Fatalf("sends=%d, want 1", len(snd.taxVoucherSent))
	}
	b := snd.taxVoucherSent[0]
	if !b.IsFinal || b.Snapshot == nil || !b.Snapshot.Complete || len(b.Batch) != 0 || b.Snapshot.VouchersSent != 0 {
		t.Fatalf("batch: %+v", b)
	}
	raw, _ := json.Marshal(b)
	if !strings.Contains(string(raw), `"batch":[]`) {
		t.Errorf("empty batch must marshal as []: %s", raw)
	}
}

func TestTaxLedgerVoucher_OutsideWindowIncompleteWithWarning(t *testing.T) {
	vouchers := []byte(`<ENVELOPE><BODY><DATA><COLLECTION>
		<VOUCHER><GUID>a</GUID><DATE>20260910</DATE><ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>
		<VOUCHER><GUID>b</GUID><DATE>20261001</DATE><ALLLEDGERENTRIES.LIST><LEDGERNAME>CGST INPUT</LEDGERNAME><AMOUNT>1</AMOUNT></ALLLEDGERENTRIES.LIST></VOUCHER>
	</COLLECTION></DATA></BODY></ENVELOPE>`)
	snd := &fakeSender{}
	if _, err := RunOne(context.Background(), kssTally(t, vouchers), snd, kssReq(), nil); err != nil {
		t.Fatal(err)
	}
	b := snd.taxVoucherSent[0]
	if len(b.Batch) != 1 || b.Snapshot.Complete || b.Snapshot.OutsideWindow != 1 {
		t.Fatalf("rows=%d snapshot=%+v", len(b.Batch), *b.Snapshot)
	}
	if len(b.SyncWarnings) != 1 || b.SyncWarnings[0].Code != tally.SyncWarningTaxLedgerVouchersIncomplete ||
		strings.Join(b.SyncWarnings[0].AffectedVoucherGUIDs, ",") != "b" {
		t.Errorf("sync warnings: %+v", b.SyncWarnings)
	}
}

// An unreadable reply sends one empty final batch with complete=false and a
// sync warning, and the run still fails like the tax_ledger lane.
func TestTaxLedgerVoucher_UnreadableReplySendsIncompleteMarker(t *testing.T) {
	for name, reply := range map[string][]byte{
		"foreign prefix": []byte(`<ENVELOPE><VOUCHER><GUID>g</GUID><FOO:X/></VOUCHER></ENVELOPE>`),
		"lineerror":      []byte(`<ENVELOPE><LINEERROR>Could not set 'SVCurrentCompany'</LINEERROR></ENVELOPE>`),
	} {
		t.Run(name, func(t *testing.T) {
			snd := &fakeSender{}
			res, err := RunOne(context.Background(), kssTally(t, reply), snd, kssReq(), nil)
			if err == nil || !errors.Is(err, tally.ErrTaxLedgerVoucherReply) {
				t.Fatalf("err=%v, want the parse error", err)
			}
			if len(snd.taxVoucherSent) != 1 || res.BatchesSent != 1 {
				t.Fatalf("sends=%d, want one incomplete marker", len(snd.taxVoucherSent))
			}
			b := snd.taxVoucherSent[0]
			if !b.IsFinal || b.Snapshot == nil || b.Snapshot.Complete || len(b.Batch) != 0 {
				t.Fatalf("marker: %+v", b)
			}
			if len(b.SyncWarnings) != 1 || b.SyncWarnings[0].Code != tally.SyncWarningTaxLedgerVouchersIncomplete {
				t.Errorf("warnings: %+v", b.SyncWarnings)
			}
		})
	}
}

func TestTaxLedgerVoucher_TallyDownSendsNothing(t *testing.T) {
	snd := &fakeSender{}
	_, err := RunOne(context.Background(), &fakeTally{err: errors.New("connection refused")}, snd, kssReq(), nil)
	if err == nil || len(snd.taxVoucherSent) != 0 {
		t.Fatalf("err=%v sends=%d, want error and nothing sent", err, len(snd.taxVoucherSent))
	}
}

func TestTaxLedgerVoucher_NoTaxLedgersIsIncomplete(t *testing.T) {
	tly := &fakeTally{respond: func(body []byte) []byte {
		if bytes.Contains(body, []byte("GstrecoTaxLedgerVoucherCollection")) {
			t.Error("vouchers must not be fetched without a ledger set")
		}
		return emptyVoucherReply()
	}}
	snd := &fakeSender{}
	if _, err := RunOne(context.Background(), tly, snd, kssReq(), nil); err != nil {
		t.Fatal(err)
	}
	if len(snd.taxVoucherSent) != 1 || snd.taxVoucherSent[0].Snapshot.Complete {
		t.Fatalf("want one incomplete marker, got %+v", snd.taxVoucherSent)
	}
}

func TestTaxLedgerVoucher_BatchesOnlyFinalCarriesSnapshot(t *testing.T) {
	snd := &fakeSender{}
	req := kssReq()
	req.BatchSize = 2
	if _, err := RunOne(context.Background(), kssTally(t, tallyFixture(t, "tax-ledger-vouchers-kss-092026.xml")), snd, req, nil); err != nil {
		t.Fatal(err)
	}
	if len(snd.taxVoucherSent) != 3 {
		t.Fatalf("sends=%d, want 3 (5 vouchers / 2)", len(snd.taxVoucherSent))
	}
	if s := snd.taxVoucherSent[2].Snapshot; s == nil || s.VouchersSent != 5 || s.VouchersReturned != 7 {
		t.Errorf("vouchers_sent must sum every batch of the run: %+v", s)
	}
	for i, b := range snd.taxVoucherSent {
		last := i == 2
		if b.IsFinal != last || (b.Snapshot != nil) != last {
			t.Errorf("batch %d: is_final=%t snapshot=%v", i+1, b.IsFinal, b.Snapshot)
		}
	}
}

// failFirstSender fails the first tax-ledger voucher batch with err.
type failFirstSender struct {
	fakeSender
	first error
	n     int
}

func (f *failFirstSender) SendTaxLedgerVouchers(ctx context.Context, body tally.TaxLedgerVoucherIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	f.n++
	if f.n == 1 {
		f.taxVoucherSent = append(f.taxVoucherSent, body)
		return tally.AccountingAcceptedResponse{}, f.first
	}
	return f.fakeSender.SendTaxLedgerVouchers(ctx, body)
}

func TestTaxLedgerVoucher_EarlierBatchFailureMakesSnapshotIncomplete(t *testing.T) {
	snd := &failFirstSender{first: &ingest.SendError{Kind: ingest.ErrorKindServer, Status: 500}}
	req := kssReq()
	req.BatchSize = 2
	res, err := RunOne(context.Background(), kssTally(t, tallyFixture(t, "tax-ledger-vouchers-kss-092026.xml")), snd, req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.BatchesFailed != 1 || res.BatchesSent != 2 || res.LaneUnsupported {
		t.Fatalf("failed=%d sent=%d unsupported=%t", res.BatchesFailed, res.BatchesSent, res.LaneUnsupported)
	}
	final := snd.taxVoucherSent[len(snd.taxVoucherSent)-1]
	if final.Snapshot == nil || final.Snapshot.Complete {
		t.Fatalf("final snapshot must be incomplete after a failed batch: %+v", final.Snapshot)
	}
}

// An older server without the route answers 404/405: the lane is
// unsupported this tick, never a failure, and nothing more is sent.
func TestTaxLedgerVoucher_OlderServerLaneUnsupported(t *testing.T) {
	for _, status := range []int{404, 405} {
		snd := &fakeSender{taxVoucherErr: &ingest.SendError{Kind: ingest.ErrorKindUnknown, Status: status, Snippet: "Not Found"}}
		req := kssReq()
		req.BatchSize = 2
		res, err := RunOne(context.Background(), kssTally(t, tallyFixture(t, "tax-ledger-vouchers-kss-092026.xml")), snd, req, nil)
		if err != nil {
			t.Fatalf("%d: err=%v", status, err)
		}
		if !res.LaneUnsupported || res.BatchesFailed != 0 || len(res.BatchErrors) != 0 || len(snd.taxVoucherSent) != 1 {
			t.Fatalf("%d: unsupported=%t failed=%d sends=%d", status, res.LaneUnsupported, res.BatchesFailed, len(snd.taxVoucherSent))
		}
	}
}

func TestTaxLedgerVoucher_WalkWithOlderServerIsClean(t *testing.T) {
	ledgers := tallyFixture(t, "tax-ledgers-kss-092026.xml")
	vouchers := tallyFixture(t, "tax-ledger-vouchers-kss-092026.xml")
	tly := &fakeTally{respond: func(body []byte) []byte {
		switch {
		case bytes.Contains(body, []byte("GstrecoTaxLedgerCollection")):
			return ledgers
		case bytes.Contains(body, []byte("GstrecoTaxLedgerVoucherCollection")):
			return vouchers
		}
		return []byte(stubResponse)
	}}
	snd := &fakeSender{taxVoucherErr: &ingest.SendError{Kind: ingest.ErrorKindUnknown, Status: 404}}
	var unsupported int
	res := WalkAll(context.Background(), WalkOptions{
		Mappings:        []ingest.ActiveMapping{{MappingID: "m-kss", TallyEndpoint: "http://127.0.0.1:9553", TallyCompanyName: kssCompany, IdentityGUID: kssCompanyGUID}},
		From:            kssReq().From,
		To:              kssReq().To,
		RunKind:         "incremental",
		ContinueOnError: true,
		NewTallyClient:  func(string) TallyPoster { return tly },
		Sender:          snd,
		PerKindResultHook: func(_ ingest.ActiveMapping, k KindPair, r Result, err error) {
			if k.Name == "tax_ledger_voucher" && r.LaneUnsupported && err == nil {
				unsupported++
			}
		},
	})
	if res.FatalErrors != 0 || res.TotalBatchesFailed != 0 || res.TotalServerErrors != 0 {
		t.Fatalf("walk must stay clean on a 404 lane: %+v", res)
	}
	if unsupported != 1 {
		t.Errorf("lane unsupported reported %d times, want 1", unsupported)
	}
	if len(snd.taxVoucherSent) != 1 || snd.taxVoucherSent[0].CompanyGUID == nil || *snd.taxVoucherSent[0].CompanyGUID != kssCompanyGUID {
		t.Errorf("company_guid from the identity check must reach the batch: %+v", snd.taxVoucherSent)
	}
}

func TestApplyIdentityGUIDs(t *testing.T) {
	ms := []ingest.ActiveMapping{{MappingID: "a"}, {MappingID: "b"}, {MappingID: "c"}, {MappingID: "d"}}
	checks := []MappingIdentity{
		{MappingID: "a", Verdict: IdentityMatch, GUIDs: []string{"g-a"}},
		{MappingID: "b", Verdict: IdentityNoStoredGUID, GUIDs: []string{" g-b "}},
		{MappingID: "c", Verdict: IdentityGUIDMismatch, GUIDs: []string{"g-c"}},
		{MappingID: "d", Verdict: IdentityUnavailable},
	}
	got := ApplyIdentityGUIDs(ms, checks)
	if got[0].IdentityGUID != "g-a" || got[1].IdentityGUID != "g-b" || got[2].IdentityGUID != "" || got[3].IdentityGUID != "" {
		t.Errorf("guids: %+v", got)
	}
}

func TestMapKindByName_TaxLedgerVoucher(t *testing.T) {
	for _, n := range []string{"tax_ledger_voucher", "tax-ledger-vouchers"} {
		if k, err := MapKindByName(n); err != nil || k.Name != "tax_ledger_voucher" {
			t.Errorf("%s: %v %v", n, k, err)
		}
	}
	if DefaultKinds[len(DefaultKinds)-2].Name != "tax_ledger" || DefaultKinds[len(DefaultKinds)-1].Name != "tax_ledger_voucher" {
		t.Errorf("tax_ledger_voucher must follow tax_ledger in DefaultKinds: %v", DefaultKinds)
	}
}
