package tally

import (
	"strings"
	"testing"
)

// QA v27 L1: companies whose duty-ledger graph was missing sent vouchers with
// GSTIN and taxable value but zero tax, which GST Reco rejected (missing_tax).
// A plainly named tax ledger ("SGST INPUT") is a signal, never evidence: a
// leg is GST only with its duty master, its GST class or Tally's grouping
// under Duties & Taxes. Without that the voucher is still sent, the leg held
// uncertain (name_fallback) and the row flagged tax_legs_uncertain so the
// server quarantines it visibly. All data here is synthetic
// (testdata/synthetic-l1-*.xml).

func l1Ledgers(t *testing.T) *LedgerGSTINs {
	t.Helper()
	ledgers, warnings, err := ParseLedgerGSTINs(readTestdata(t, "synthetic-l1-ledgers.xml"))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("ParseLedgerGSTINs: err=%v warnings=%v", err, warnings)
	}
	tree, err := ParseGroups(readTestdata(t, "synthetic-l1-groups-renamed.xml"))
	if err != nil {
		t.Fatalf("ParseGroups: %v", err)
	}
	ledgers.ApplyGroups(tree)
	return ledgers
}

func l1DebitNotes(t *testing.T) []RawVoucher {
	t.Helper()
	parsed, err := ParseDayBookV3(readTestdata(t, "synthetic-l1-debit-notes.xml"))
	if err != nil {
		t.Fatalf("ParseDayBookV3: %v", err)
	}
	if len(parsed.Vouchers) != 2 {
		t.Fatalf("vouchers = %d, want 2 (warnings %v)", len(parsed.Vouchers), parsed.Warnings)
	}
	return parsed.Vouchers
}

func hasExactFlag(row IngestVoucherRow, flag string) bool {
	for _, f := range row.NormalizationFlags {
		if f == flag {
			return true
		}
	}
	return false
}

func TestParseGroups_RenamedDutiesAndTaxesKeepsItsReservedName(t *testing.T) {
	ledgers := l1Ledgers(t)
	if got := ledgers.DutiesAndTaxesGroup(); got != "Duties and Taxes" {
		t.Errorf("DutiesAndTaxesGroup = %q, want the company's own name", got)
	}
	var names []string
	for _, l := range ledgers.DutyLedgersByGroup() {
		names = append(names, l.Name+"<"+l.Parent)
	}
	want := "CGST INPUT<GST Input,IGST INPUT<Duties and Taxes,SGST INPUT<GST Input"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("duty ledgers by group = %s, want %s", got, want)
	}
	// A GST-named ledger under Current Assets is not a duty ledger.
	for _, l := range ledgers.DutyLedgersByGroup() {
		if l.Name == "IGST Advance Deposit" {
			t.Error("a Current Assets ledger was read as a duty ledger from its name")
		}
	}
	if !ledgers.IsNonParty("SGST INPUT") {
		t.Error("SGST INPUT under the renamed Duties & Taxes group is not marked non-party")
	}
	if got := ledgers.PrimaryGroup("SGST INPUT"); got != "Current Liabilities" {
		t.Errorf("primary group = %q", got)
	}
}

func TestParseGroups_UnrenamedTreeNamesTheReservedGroup(t *testing.T) {
	ledgers, _, err := ParseLedgerGSTINs([]byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <LEDGER NAME="SGST INPUT"><NAME>SGST INPUT</NAME><PARENT>Duties &amp; Taxes</PARENT></LEDGER>
</COLLECTION></DATA></BODY></ENVELOPE>`))
	if err != nil {
		t.Fatal(err)
	}
	// Even with no group tree, a ledger whose parent is Duties & Taxes is one.
	ledgers.ApplyGroups(GroupTree{})
	if got := ledgers.DutiesAndTaxesGroup(); got != "Duties & Taxes" {
		t.Errorf("DutiesAndTaxesGroup = %q", got)
	}
	if got := ledgers.DutyLedgersByGroup(); len(got) != 1 || got[0].Name != "SGST INPUT" {
		t.Errorf("duty ledgers by group = %+v", got)
	}
}

func TestBuildDutyLedgerXMLUnder_RenamedGroup(t *testing.T) {
	body, err := BuildDutyLedgerXMLUnder("Synthetic Co", "Duties & GST")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<CHILDOF>Duties &amp; GST</CHILDOF>") {
		t.Errorf("request does not name the renamed group: %s", body)
	}
	body, err = BuildDutyLedgerXML("Synthetic Co")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<CHILDOF>Duties &amp; Taxes</CHILDOF>") {
		t.Errorf("default request = %s", body)
	}
	if _, err := BuildDutyLedgerXMLUnder("Synthetic\x01Co", "Duties & Taxes"); err == nil {
		t.Error("control character accepted")
	}
}

func TestParseDutyLedgers_NamesOnlyInNameLists(t *testing.T) {
	book, warnings, err := ParseDutyLedgers(readTestdata(t, "synthetic-l1-duty-ledgers-namelist.xml"))
	if err != nil || len(warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, warnings)
	}
	if book.Len() != 2 {
		t.Fatalf("book has %d ledgers, want 2", book.Len())
	}
	sgst, ok := book.Lookup("sgst input")
	if !ok || sgst.Parent != "GST Input" || sgst.DutyHead != "SGST/UTGST" {
		t.Errorf("SGST INPUT = %+v, %v", sgst, ok)
	}
	if _, ok := book.Lookup("CGST INPUT"); !ok {
		t.Error("CGST INPUT missing")
	}
}

// Without any graph, a debit note's plainly named tax legs are uncertain: the
// note is still sent, with GSTIN and taxable value, no guessed tax, its legs
// as name_fallback / uncertain and the tax_legs_uncertain flag.
func TestNormalize_L1NoGraphDebitNoteIsSentFlaggedNeverGuessed(t *testing.T) {
	for _, v := range l1DebitNotes(t) {
		t.Run(v.GUID, func(t *testing.T) {
			rows, err := Normalize(v, NormalizeOptions{Kind: IngestKindDebitNote})
			if err != nil {
				t.Fatalf("debit note dropped: %v", err)
			}
			row := rows[0]
			if row.IGST != 0 || row.CGST != 0 || row.SGST != 0 {
				t.Errorf("tax guessed from names: %v/%v/%v", row.IGST, row.CGST, row.SGST)
			}
			if row.VendorGSTIN == nil || row.TaxableValue == 0 {
				t.Errorf("row lost GSTIN or taxable: %+v", row)
			}
			if !hasExactFlag(row, FlagTaxLegsUncertain) {
				t.Errorf("flags = %v, want %s", row.NormalizationFlags, FlagTaxLegsUncertain)
			}
			if row.Side == nil || *row.Side != IngestSidePurchase {
				t.Errorf("side = %v", row.Side)
			}
			n := 0
			for _, leg := range row.VoucherLegs {
				if headFromNameTokens(leg.LedgerName) == "" {
					continue
				}
				n++
				c := leg.Classification
				if c.Role != "uncertain" || c.Method != "name_fallback" || c.AgentRole != LegUncertain || c.Head != nil {
					t.Errorf("%s classified %+v, want uncertain / name_fallback", leg.LedgerName, c)
				}
			}
			if n == 0 {
				t.Error("no tax legs sent")
			}
		})
	}
}

// With the group tree placing them under (renamed) Duties & Taxes, the same
// legs are GST from Tally's grouping, not from their names alone.
func TestNormalize_L1GroupTreeGraphMakesGroupedLegsGST(t *testing.T) {
	ledgers := l1Ledgers(t)
	book := NewDutyLedgerBookFromGroups(ledgers.DutyLedgersByGroup())
	if !book.FromGroups() || book.Len() != 3 {
		t.Fatalf("group book = %+v", book)
	}
	vs := l1DebitNotes(t)
	want := []struct{ invoice, taxable, igst, cgst, sgst float64 }{
		{11800, 10000, 0, 900, 900},
		{5900, 5000, 900, 0, 0},
	}
	for i, v := range vs {
		rows, err := Normalize(v, NormalizeOptions{Kind: IngestKindDebitNote, Ledgers: ledgers, DutyLedgers: book})
		if err != nil {
			t.Fatalf("%s: %v", v.GUID, err)
		}
		row := rows[0]
		w := want[i]
		assertValues(t, row, w.invoice, w.taxable, w.igst, w.cgst, w.sgst)
		if hasExactFlag(row, FlagTaxLegsUncertain) {
			t.Errorf("%s flagged uncertain with group evidence: %v", v.GUID, row.NormalizationFlags)
		}
		if !hasFlag(row, "duty ledgers taken from the group tree") {
			t.Errorf("%s: flags = %v, want the group-tree source named", v.GUID, row.NormalizationFlags)
		}
		for _, leg := range row.VoucherLegs {
			if headFromNameTokens(leg.LedgerName) == "" {
				continue
			}
			c := leg.Classification
			if c.Role != "tax" || c.Method != "group" || c.Evidence != EvidenceDutyGroup || c.Head == nil {
				t.Errorf("%s classified %+v, want tax / group", leg.LedgerName, c)
			}
			if leg.ParentGroup == nil || leg.PrimaryGroup == nil || *leg.PrimaryGroup != "Duties & Taxes" {
				t.Errorf("%s groups = %v / %v", leg.LedgerName, leg.ParentGroup, leg.PrimaryGroup)
			}
		}
	}
}

// A GST-named ledger the group tree keeps outside Duties & Taxes is never
// GST, even with a group-tree book.
func TestNormalize_L1GroupBookNeverTrustsANameOutsideDutiesAndTaxes(t *testing.T) {
	book := NewDutyLedgerBookFromGroups(l1Ledgers(t).DutyLedgersByGroup())
	v := RawVoucher{
		GUID: "l1-outside", Date: parseDate(t, "2026-04-20"), VoucherType: "Purchase", VoucherNumber: "SYN/L1/9",
		PartyLedgerName: "Synthetic Steel Supplier", PartyGSTIN: "27AAAAA0000A1Z5",
		LedgerEntries: []LedgerEntry{
			{LedgerName: "Synthetic Steel Supplier", Amount: 11800, IsPartyLedger: true},
			{LedgerName: "Purchase Account", Amount: -10000},
			{LedgerName: "IGST Advance Deposit", Amount: -1800},
		},
	}
	row := normalizeOneWith(t, v, book)
	if row.IGST != 0 {
		t.Errorf("IGST %v from a name outside Duties & Taxes", row.IGST)
	}
	if leg := legNamed(t, row, "IGST Advance Deposit"); leg.Classification.Role == "tax" {
		t.Errorf("classified %+v", leg.Classification)
	}
}

// With the duty masters the legs are GST as before and nothing is flagged.
func TestNormalize_L1DutyMastersLeaveNoUncertainty(t *testing.T) {
	book, _, err := ParseDutyLedgers(readTestdata(t, "synthetic-l1-duty-ledgers-namelist.xml"))
	if err != nil {
		t.Fatal(err)
	}
	v := l1DebitNotes(t)[0]
	rows, err := Normalize(v, NormalizeOptions{Kind: IngestKindDebitNote, DutyLedgers: book})
	if err != nil {
		t.Fatal(err)
	}
	assertValues(t, rows[0], 11800, 10000, 0, 900, 900)
	if hasExactFlag(rows[0], FlagTaxLegsUncertain) {
		t.Errorf("flags = %v", rows[0].NormalizationFlags)
	}
}

// The party is found whatever case the ledger line spells it in, so such a
// voucher-mode note without a bill allocation is not dropped.
func TestParseDayBook_PartyMatchIgnoresCase(t *testing.T) {
	v := l1DebitNotes(t)[1]
	if !v.LedgerEntries[0].IsPartyLedger {
		t.Fatalf("party line %q not matched to %q", v.LedgerEntries[0].LedgerName, v.PartyLedgerName)
	}
	if _, err := Normalize(v, NormalizeOptions{Kind: IngestKindDebitNote}); err != nil {
		t.Fatalf("dropped: %v", err)
	}
}

// inferKind keeps every debit-note type on the debit-note path.
func TestInferKind_UserDefinedDebitNoteTypes(t *testing.T) {
	for _, name := range []string{"Debit Note - Material Rejection", "DEBIT NOTE (RATE DIFF)", "Debit Note"} {
		if got := inferKind(RawVoucher{VoucherType: name}); got != IngestKindDebitNote {
			t.Errorf("%q -> %s", name, got)
		}
	}
}
