package tally

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// NormalizeOptions tunes Normalize's behaviour. Zero value is sensible:
// default kind = IngestKindPurchase, derive from VoucherType if RCM is true.
type NormalizeOptions struct {
	// Kind forces the IngestKind for every row in this batch. Leave empty to
	// let Normalize infer per-voucher from VoucherType + ReverseCharge.
	Kind IngestKind
	// RunKind is "full" | "incremental" | "manual"; passed through to the
	// IngestRequestBody the ingest client builds.
	RunKind string
	// Ledgers fills the party GSTIN from the party ledger's master when the
	// voucher carries no PARTYGSTIN (voucher-mode purchases and sales often
	// don't). Nil: voucher data only.
	Ledgers LedgerDirectory
}

// Normalize converts one RawVoucher into zero or more IngestVoucherRows.
// Zero rows when the voucher is cancelled or isn't invoice-shaped (journal,
// contra, etc) — callers filter those out of the batch entirely.
// Multiple rows when the voucher is "consolidated" (pain #8): one IngestVoucherRow
// per BillAllocation, each with parent_ref = v.GUID so the server keeps the
// relationship.
//
// Responsibilities, in order:
//  1. Infer kind (purchase|sales|credit_note|debit_note|rcm_self_invoice) from
//     the voucher type name and ReverseCharge flag, unless overridden in opts.
//  2. Classify every LedgerEntry as IGST / CGST / SGST / CESS / party / TDS /
//     base, preferring GSTClass over name-pattern and checking the tax legs
//     against the whole voucher (classifyLedgers). Sum the tax buckets.
//  3. Compute invoice_value from the party ledger's magnitude (Tally's
//     double-entry invariant means |partyAmount| equals invoice total) plus
//     any TDS withheld from it.
//  4. Compute taxable_value = invoice_value - (igst + cgst + sgst + cess).
//  5. For single-bill vouchers, emit one row. For multi-bill, prorate the
//     per-bill amounts by allocation share.
func Normalize(v RawVoucher, opts NormalizeOptions) ([]IngestVoucherRow, error) {
	if v.IsCancelled {
		return nil, nil
	}
	// NB: we deliberately do NOT gate on v.IsInvoice here. A purchase or sales
	// booked in Tally's "as voucher" (accounting) mode exports ISINVOICE=No but
	// is a real invoice-family document that must reconcile against GSTR-2B.
	// The per-lane $$IsPurchase/$$IsSales/... family filter already guarantees
	// every voucher on this lane belongs to the right parent class, so the only
	// question is party/tax extraction (handled below). Dropping on ISINVOICE
	// silently lost every voucher-mode purchase (e.g. purchases booked through a
	// user-defined purchase voucher type in accounting mode), which never showed
	// up in the purchase lane OR the journal lane. See classifyLedgers for the
	// voucher-mode party fallback.

	kind := opts.Kind
	if kind == "" {
		kind = inferKind(v)
	}
	side := inferSide(kind, v)

	legs := classifyLedgers(v.LedgerEntries)
	party, tax := legs.party, legs.tax
	if party == nil {
		return nil, fmt.Errorf("voucher %s: no party ledger (expected LedgerName=%q)", voucherID(v), v.PartyLedgerName)
	}

	// TDS the buyer withholds comes off what it owes the supplier, not off the
	// invoice: the supplier's invoice (what GSTR-2B lists) is the party amount
	// plus the TDS.
	partyValue := math.Abs(party.Amount)
	invoiceValue := partyValue + legs.withheld
	taxTotal := tax.total()
	taxableValue := invoiceValue - taxTotal
	if taxableValue < 0 {
		// Defensive: if totals don't match (missing child ledgers, rounding),
		// fall back to the base legs so the value is never negative on the
		// wire. TDS is never part of it.
		taxableValue = legs.base
	}

	base := baseRow(v, kind, side, invoiceValue, taxableValue, tax)
	fillPartyFromLedger(&base, side, party.LedgerName, v.Date, opts.Ledgers)

	bills := relevantBillRefs(*party)
	if len(bills) <= 1 {
		if b := onlyNewRefName(*party); b != "" {
			base.InvoiceNumber = b
		}
		return []IngestVoucherRow{base}, nil
	}

	// Consolidated: one row per bill ref, amounts prorated by the bill's share
	// of the party ledger magnitude. parent_ref carries the best voucher-level
	// identity we have so the reco inspector can still re-stitch the group
	// when Tally omits GUID on the parent voucher.
	//
	// Residual allocation: we prorate the first N-1 rows by share, then
	// assign whatever's left to the last row. Without this, IEEE-754 drift
	// on shares like 1/3 can leave split rows that don't sum to the original
	// totals — e.g. a tax of ₹33 split three ways gives 3×10.99=32.97 with
	// naive rounding. Users see the discrepancy as "my books don't match
	// reco" and we get a support ticket. Residual assignment on the last
	// row guarantees every tax + value bucket sums exactly.
	rows := make([]IngestVoucherRow, 0, len(bills))
	var accInvoice, accTaxable, accIGST, accCGST, accSGST, accCESS float64
	last := len(bills) - 1
	for i, b := range bills {
		row := base
		row.InvoiceNumber = b.Name
		if parentRef := voucherParentRef(v); parentRef != "" {
			parent := parentRef
			row.ParentRef = &parent
		}

		if i == last {
			row.InvoiceValue = round2(invoiceValue - accInvoice)
			row.TaxableValue = round2(taxableValue - accTaxable)
			row.IGST = round2(tax.IGST - accIGST)
			row.CGST = round2(tax.CGST - accCGST)
			row.SGST = round2(tax.SGST - accSGST)
			row.CESS = round2(tax.CESS - accCESS)
		} else {
			share := 0.0
			if partyValue > 0 {
				share = math.Abs(b.Amount) / partyValue
			}
			row.InvoiceValue = round2(invoiceValue * share)
			row.TaxableValue = round2(taxableValue * share)
			row.IGST = round2(tax.IGST * share)
			row.CGST = round2(tax.CGST * share)
			row.SGST = round2(tax.SGST * share)
			row.CESS = round2(tax.CESS * share)
			accInvoice += row.InvoiceValue
			accTaxable += row.TaxableValue
			accIGST += row.IGST
			accCGST += row.CGST
			accSGST += row.SGST
			accCESS += row.CESS
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// fillPartyFromLedger sets the party GSTIN from the party ledger's master
// when the voucher left it empty, and the party name from the ledger line
// when the voucher named no party.
func fillPartyFromLedger(row *IngestVoucherRow, side IngestSide, partyLedger string, on time.Time, ledgers LedgerDirectory) {
	partyLedger = strings.TrimSpace(partyLedger)
	if ledgers == nil || partyLedger == "" {
		return
	}
	lookup := func(name string) string { return ledgers.GSTIN(name, on) }
	nameField, gstinField := &row.VendorName, &row.VendorGSTIN
	if side == IngestSideSales {
		nameField, gstinField = &row.CustomerName, &row.CustomerGSTIN
	}
	if *gstinField != nil && strings.TrimSpace(**gstinField) != "" {
		return
	}
	name := partyLedger
	if *nameField != nil && strings.TrimSpace(**nameField) != "" {
		name = **nameField
	}
	g := lookup(name)
	if g == "" && name != partyLedger {
		g = lookup(partyLedger)
	}
	if g == "" {
		return
	}
	*gstinField = &g
	if *nameField == nil {
		n := partyLedger
		*nameField = &n
	}
}

// baseRow fills the fields that don't depend on the single-vs-consolidated
// branch. Separated out so both branches can start from the same struct.
func baseRow(v RawVoucher, kind IngestKind, side IngestSide, invoice, taxable float64, tax taxTotals) IngestVoucherRow {
	row := IngestVoucherRow{
		InvoiceNumber: preferredInvoiceNumber(v),
		InvoiceDate:   v.Date.Format("2006-01-02"),
		InvoiceValue:  round2(invoice),
		TaxableValue:  round2(taxable),
		IGST:          round2(tax.IGST),
		CGST:          round2(tax.CGST),
		SGST:          round2(tax.SGST),
		CESS:          round2(tax.CESS),
	}
	if v.GUID != "" {
		g := v.GUID
		row.TallyVoucherGUID = &g
	}
	if v.VoucherNumber != "" {
		n := strings.TrimSpace(v.VoucherNumber)
		if n != "" {
			row.VoucherNumber = &n
		}
	}
	if !v.Date.IsZero() {
		d := v.Date.Format("2006-01-02")
		row.VoucherDate = &d
	}
	if v.VoucherType != "" {
		vt := strings.TrimSpace(v.VoucherType)
		if vt != "" {
			row.VoucherTypeName = &vt
		}
	}
	if v.Reference != "" {
		ref := strings.TrimSpace(v.Reference)
		if ref != "" {
			row.VoucherReference = &ref
		}
	}
	if v.PlaceOfSupply != "" {
		p := v.PlaceOfSupply
		row.PlaceOfSupply = &p
	}
	if v.ReverseCharge {
		rc := true
		row.ReverseCharge = &rc
	}
	if v.Narration != "" {
		n := v.Narration
		row.Narration = &n
	}
	// Party fields differ by side. For purchase-side (and RCM), the party is
	// the vendor; for sales-side, the party is the customer.
	if side == IngestSideSales {
		if v.PartyLedgerName != "" {
			n := v.PartyLedgerName
			row.CustomerName = &n
		}
		if v.PartyGSTIN != "" {
			g := v.PartyGSTIN
			row.CustomerGSTIN = &g
		}
	} else {
		if v.PartyLedgerName != "" {
			n := v.PartyLedgerName
			row.VendorName = &n
		}
		if v.PartyGSTIN != "" {
			g := v.PartyGSTIN
			row.VendorGSTIN = &g
		}
	}
	// Server optionally uses Side on notes; harmless to set on every row,
	// but save bytes on purchase/sales where kind already disambiguates.
	if kind == IngestKindCreditNote || kind == IngestKindDebitNote {
		s := side
		row.Side = &s
	}
	// Tax rate, when unambiguous, helps the server's reconciliation. Derive
	// from the GSTClass of the first matched tax ledger ("IGST@18" → 18.0).
	if rate := taxRateFromClass(v.LedgerEntries); rate > 0 {
		r := rate
		row.TaxRate = &r
	}
	// HSN — first non-empty inventory entry's GSTHSNNAME. Service-only
	// purchases (no inventory) leave it nil, which the server treats as
	// "unknown" and defaults to is_capital_good=false (correct: services
	// are never capital goods).
	//
	// We pick the FIRST non-empty rather than aggregating because:
	//   1. The capital-goods classifier is chapter-prefix matching
	//      (chapters 84/85/90). Mixed-HSN vouchers are rare; when they
	//      happen, the first item dominates the bucket choice.
	//   2. Aggregation (e.g. most common HSN, or first-mode) would need
	//      a lookup table on the server side that doesn't exist. The
	//      first-non-empty rule is monotonic with what Tally itself
	//      shows in the voucher report.
	if hsn := firstInventoryHSN(v.InventoryEntries); hsn != "" {
		h := hsn
		row.HSN = &h
	}
	return row
}

// firstInventoryHSN returns the first non-empty HSN across inventory
// entries, or "" when none. Whitespace-only HSNs are skipped — Tally
// occasionally serialises legacy stock-items with " " when the HSN
// master field is blank, and we don't want to send those upstream.
func firstInventoryHSN(entries []InventoryEntry) string {
	for _, e := range entries {
		if h := strings.TrimSpace(e.HSN); h != "" {
			return h
		}
	}
	return ""
}

// inferKind derives IngestKind from the voucher's type name + ReverseCharge
// flag. Conservative: if nothing matches, defaults to purchase so downstream
// side inference picks the vendor path.
func inferKind(v RawVoucher) IngestKind {
	if v.ReverseCharge {
		return IngestKindRCMSelfInvoice
	}
	n := strings.ToUpper(v.VoucherType)
	switch {
	case strings.Contains(n, "CREDIT NOTE"):
		return IngestKindCreditNote
	case strings.Contains(n, "DEBIT NOTE"):
		return IngestKindDebitNote
	case strings.Contains(n, "SALES"):
		return IngestKindSales
	default:
		return IngestKindPurchase
	}
}

// inferSide maps kind to purchase/sales bookkeeping. Credit notes are
// typically sales-side (customer returns goods), debit notes are purchase-
// side (vendor issues debit). RCM is always purchase-side.
func inferSide(kind IngestKind, v RawVoucher) IngestSide {
	switch kind {
	case IngestKindSales, IngestKindCreditNote:
		return IngestSideSales
	case IngestKindPurchase, IngestKindRCMSelfInvoice, IngestKindDebitNote:
		return IngestSidePurchase
	}
	// Defensive fallback — voucher-type text is the only clue left.
	if strings.Contains(strings.ToUpper(v.VoucherType), "SALES") {
		return IngestSideSales
	}
	return IngestSidePurchase
}

// taxTotals is the bucket sum across all tax ledgers in a voucher. All values
// are stored absolute (sign-independent) because invoice_value already uses
// abs(party.Amount); taxes are the positive portions that add up to invoice.
type taxTotals struct {
	IGST, CGST, SGST, CESS float64
}

func (t taxTotals) total() float64 {
	return t.IGST + t.CGST + t.SGST + t.CESS
}

func (t *taxTotals) add(head string, amount float64) {
	switch head {
	case "IGST":
		t.IGST += math.Abs(amount)
	case "CGST":
		t.CGST += math.Abs(amount)
	case "SGST":
		t.SGST += math.Abs(amount)
	case "CESS":
		t.CESS += math.Abs(amount)
	}
}

// voucherLegs is an invoice voucher split into its party ledger, its GST tax,
// its taxable base and the TDS withheld from the party.
type voucherLegs struct {
	party *LedgerEntry
	tax   taxTotals
	// base sums the purchase / sales accounts, freight and other charges: the
	// fallback taxable value when invoice - tax does not add up.
	base float64
	// withheld is the TDS on the party's side of the voucher (negative when a
	// reversal books it on the other side). It is never taxable value or tax.
	withheld float64
}

// taxLedgerGrade is how sure a leg's GST class and name are that it is a GST
// tax ledger. Higher is surer.
type taxLedgerGrade int

const (
	notTaxLedger taxLedgerGrade = iota
	// namedTaxLedger carries a GST head in its name next to other words
	// ("Exhibition Expenses IGST-18%", "IGST Exp 17(5)"): accountants name
	// expense ledgers after the tax that goes with them.
	namedTaxLedger
	// classTaggedLedger has a head in Tally's GST class but none in its name.
	classTaggedLedger
	// taxLedger is named as a tax ledger: a head with only tax-ledger words
	// and rates ("IGST Input Credit", "IGST ITC @ 18%", "Output IGST @ 18%",
	// "IGST 18%"), or a head in both its GST class and its name. CESS
	// ledgers are matched word for word and always count here.
	taxLedger
)

type taxCandidate struct {
	entry *LedgerEntry
	head  string
	grade taxLedgerGrade
}

// classifyLedgers splits an invoice voucher's LedgerEntries into the party
// ledger, the GST tax, the taxable base and the TDS withheld. taxable_value is
// recomputed by the caller as the residual so rounding and sign conventions
// don't fight us.
//
// Whether a leg is tax is decided on the whole voucher, not on its name alone:
//   - TDS is never tax or taxable value (isWithholdingLedger).
//   - A leg only named after a GST head is the taxable base when the voucher
//     also carries a tax ledger: next to "IGST ITC @ 18%", "Exhibition
//     Expenses IGST-18%" is the expense.
//   - A GST tax leg is never as large as the taxable value. While the tax
//     would be at least the rest of the invoice, the least sure, largest leg
//     counted as tax is the base. Legs named as tax ledgers are never moved.
func classifyLedgers(entries []LedgerEntry) voucherLegs {
	var legs voucherLegs
	// billBearers collects non-tax ledgers that carry bill allocations. In a
	// purchase/sales voucher only the party ledger (Sundry Creditor / Debtor)
	// carries bills — the purchase/sales account and tax ledgers never do. We
	// use this as a deterministic party fallback for "as voucher" accounting
	// mode, where PARTYLEDGERNAME is blank so IsPartyLedger is never set.
	var billBearers, others, withholding []*LedgerEntry
	var candidates []taxCandidate
	hasTaxLedger := false
	for i := range entries {
		e := &entries[i]
		if e.IsPartyLedger && legs.party == nil {
			legs.party = e
			continue
		}
		if isWithholdingLedger(e.LedgerName) {
			withholding = append(withholding, e)
			continue
		}
		if head, grade := gradeTaxLedger(e.GSTClass, e.LedgerName); grade != notTaxLedger {
			candidates = append(candidates, taxCandidate{entry: e, head: head, grade: grade})
			hasTaxLedger = hasTaxLedger || grade == taxLedger
			continue
		}
		// Non-tax, non-flagged-party ledger: a purchase/sales account, a
		// charge (freight/discount), or — in voucher mode — the unflagged
		// party ledger itself. Only the party carries bill allocations.
		others = append(others, e)
		if len(e.BillAllocations) > 0 {
			billBearers = append(billBearers, e)
		}
	}
	// Voucher-mode fallback: no ledger matched PARTYLEDGERNAME, so recover the
	// party as the bill-bearing ledger. If Tally spread bills across more than
	// one line, take the largest-magnitude bearer (the true invoice total).
	if legs.party == nil {
		for _, e := range billBearers {
			if legs.party == nil || math.Abs(e.Amount) > math.Abs(legs.party.Amount) {
				legs.party = e
			}
		}
	}
	if legs.party == nil {
		return legs
	}

	for _, e := range others {
		if e != legs.party {
			legs.base += math.Abs(e.Amount)
		}
	}
	for _, e := range withholding {
		if sameSide(e.Amount, legs.party.Amount) {
			legs.withheld += math.Abs(e.Amount)
		} else {
			legs.withheld -= math.Abs(e.Amount)
		}
	}

	var taxed []taxCandidate
	for _, c := range candidates {
		if c.grade == namedTaxLedger && hasTaxLedger {
			legs.base += math.Abs(c.entry.Amount)
			continue
		}
		taxed = append(taxed, c)
	}
	// Least sure first, then largest: the order in which legs counted as tax
	// give way to the base.
	sort.SliceStable(taxed, func(i, j int) bool {
		if taxed[i].grade != taxed[j].grade {
			return taxed[i].grade < taxed[j].grade
		}
		return math.Abs(taxed[i].entry.Amount) > math.Abs(taxed[j].entry.Amount)
	})
	gross := math.Abs(legs.party.Amount) + legs.withheld
	taxSum := 0.0
	for _, c := range taxed {
		taxSum += math.Abs(c.entry.Amount)
	}
	moved := 0
	for moved < len(taxed) && taxed[moved].grade != taxLedger && gross > 0 && 2*taxSum >= gross {
		amount := math.Abs(taxed[moved].entry.Amount)
		taxSum -= amount
		legs.base += amount
		moved++
	}
	for _, c := range taxed[moved:] {
		legs.tax.add(c.head, c.entry.Amount)
	}
	return legs
}

func sameSide(a, b float64) bool {
	return (a > 0 && b > 0) || (a < 0 && b < 0)
}

// classifyTaxLedger labels a single LedgerEntry with its GST head, or "" when
// it is not a tax ledger. Whether a named leg really is tax depends on the
// rest of the voucher; see classifyLedgers.
func classifyTaxLedger(gstClass, name string) string {
	head, _ := gradeTaxLedger(gstClass, name)
	return head
}

// gradeTaxLedger reads one leg's GST head and how sure we are that it is a
// tax ledger. Tally's GST class wins when it names a head (it is only
// populated when the ledger was created via Tally's GST masters); the name is
// read word by word, so a head glued into another word ("Expenses GST" ->
// "EXPENSESGST", "Big Stall" -> "BIGSTALL") is not one.
//
// UTGST (Union Territory GST) is folded into SGST — the GSTR-2B schema has
// no separate bucket and the combined tax rate matches SGST everywhere.
func gradeTaxLedger(gstClass, name string) (string, taxLedgerGrade) {
	words := ledgerWords(name)
	nameHead := ""
	for _, head := range []string{"IGST", "CGST", "SGST"} {
		for _, w := range words {
			if gstHeadOfWord(w) == head {
				nameHead = head
				break
			}
		}
		if nameHead != "" {
			break
		}
	}

	if c := strings.ToUpper(strings.TrimSpace(gstClass)); c != "" {
		classHead := ""
		switch {
		case strings.HasPrefix(c, "IGST"):
			classHead = "IGST"
		case strings.HasPrefix(c, "CGST"):
			classHead = "CGST"
		case strings.HasPrefix(c, "SGST"), strings.HasPrefix(c, "UTGST"):
			classHead = "SGST"
		case strings.HasPrefix(c, "CESS"):
			classHead = "CESS"
		}
		if classHead != "" {
			if nameHead != "" || classHead == "CESS" || isTaxLedgerName(words) {
				return classHead, taxLedger
			}
			return classHead, classTaggedLedger
		}
	}
	if looksLikeCommercialAccountLedger(compactTaxLedgerToken(name)) {
		return "", notTaxLedger
	}
	if nameHead == "" {
		if hasCessTaxToken(name) {
			return "CESS", taxLedger
		}
		return "", notTaxLedger
	}
	if isTaxLedgerName(words) {
		return nameHead, taxLedger
	}
	return nameHead, namedTaxLedger
}

// ledgerWords is a ledger name upper-cased and split on everything but
// letters and digits.
func ledgerWords(value string) []string {
	return strings.FieldsFunc(strings.ToUpper(value), func(r rune) bool {
		return !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
}

// taxLedgerWords mark a ledger that holds tax rather than an expense.
var taxLedgerWords = map[string]bool{
	"INPUT": true, "OUTPUT": true, "ITC": true, "CREDIT": true, "PAYABLE": true,
	"RECEIVABLE": true, "LIABILITY": true, "TAX": true, "TAXES": true,
	"DUTY": true, "DUTIES": true, "RCM": true,
}

// gstHeadOfWord is the head a word names: the head itself, a head with a rate
// or tax word glued on ("IGST18", "IGSTPAYABLE"), or "INPUT"/"OUTPUT" glued in
// front ("OUTPUTIGST").
func gstHeadOfWord(w string) string {
	for _, h := range []struct{ word, head string }{
		{"IGST", "IGST"}, {"CGST", "CGST"}, {"SGST", "SGST"}, {"UTGST", "SGST"},
	} {
		if strings.HasPrefix(w, h.word) {
			return h.head
		}
		if rest, ok := strings.CutSuffix(w, h.word); ok && (rest == "INPUT" || rest == "OUTPUT") {
			return h.head
		}
	}
	return ""
}

// isTaxLedgerName is a name made of a GST head and tax-ledger words or rates
// only, or one with a tax-ledger word in it.
func isTaxLedgerName(words []string) bool {
	plain := true
	for _, w := range words {
		if taxLedgerWords[w] {
			return true
		}
		if head := gstHeadOfWord(w); head != "" {
			for _, h := range []string{"IGST", "CGST", "SGST", "UTGST"} {
				if rest, ok := strings.CutPrefix(w, h); ok && taxLedgerWords[rest] {
					return true
				}
			}
			continue
		}
		if w == "GST" || len(w) == 1 || isDigits(w) {
			continue
		}
		plain = false
	}
	return plain
}

func isDigits(w string) bool {
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return w != ""
}

// isWithholdingLedger is a TDS ledger: income-tax TDS ("TDS on Contractor -
// 1%", "Tax Deducted at Source 194C") or GST TDS ("TDS Payable Under CGST @
// 1%"). The buyer withholds it from what it owes the supplier, so it is never
// the invoice's taxable value or GST.
func isWithholdingLedger(name string) bool {
	words := ledgerWords(name)
	for i, w := range words {
		if strings.HasPrefix(w, "TDS") || w == "WITHHOLDING" {
			return true
		}
		if w == "TAX" && i+1 < len(words) && words[i+1] == "DEDUCTED" {
			return true
		}
	}
	return false
}

// mentionsGSTHead is deliberately broad: a GST head anywhere in the GST class
// or the name, even glued into another word. The journal lane uses it only to
// keep a GST-looking ledger from being guessed as a voucher's party.
func mentionsGSTHead(gstClass, name string) bool {
	if c := strings.ToUpper(strings.TrimSpace(gstClass)); c != "" {
		for _, head := range []string{"IGST", "CGST", "SGST", "UTGST", "CESS"} {
			if strings.HasPrefix(c, head) {
				return true
			}
		}
	}
	n := compactTaxLedgerToken(name)
	if looksLikeCommercialAccountLedger(n) {
		return false
	}
	return strings.Contains(n, "IGST") ||
		strings.Contains(n, "CGST") ||
		strings.Contains(n, "SGST") ||
		strings.Contains(n, "UTGST") ||
		hasCessTaxToken(name)
}

func compactTaxLedgerToken(value string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(value) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func hasTaxLedgerWord(value, word string) bool {
	for _, field := range ledgerWords(value) {
		if field == word {
			return true
		}
	}
	return false
}

func hasCessTaxToken(value string) bool {
	compact := compactTaxLedgerToken(value)
	return hasTaxLedgerWord(value, "CESS") ||
		strings.Contains(compact, "GSTCESS") ||
		strings.Contains(compact, "COMPENSATIONCESS")
}

// looksLikeCommercialAccountLedger suppresses false tax classification for
// purchase/sales account ledgers whose names embed the tax family, such as
// "Purchase (IGST)@18%". Those are taxable-base ledgers, not the actual tax
// ledgers, and treating them as tax collapses taxable value toward zero.
func looksLikeCommercialAccountLedger(name string) bool {
	name = strings.TrimSpace(strings.ToUpper(name))
	if name == "" {
		return false
	}
	switch {
	case strings.HasPrefix(name, "PURCHASE"),
		strings.HasPrefix(name, "PURCHASES"),
		strings.HasPrefix(name, "SALE"),
		strings.HasPrefix(name, "SALES"):
		return strings.Contains(name, "IGST") ||
			strings.Contains(name, "CGST") ||
			strings.Contains(name, "SGST") ||
			strings.Contains(name, "UTGST") ||
			strings.Contains(name, "CESS")
	default:
		return false
	}
}

// relevantBillRefs filters BillAllocations to New Ref and Agst Ref — the
// only two types that correspond to actual invoices. "On Account" and
// "Advance" are cash-on-account, not invoice lines, and shouldn't produce
// IngestVoucherRow entries.
func relevantBillRefs(party LedgerEntry) []BillRef {
	out := make([]BillRef, 0, len(party.BillAllocations))
	for _, b := range party.BillAllocations {
		if isUsableBillRef(b) {
			out = append(out, b)
		}
	}
	return out
}

func isUsableBillRef(b BillRef) bool {
	t := strings.ToLower(strings.TrimSpace(b.BillType))
	return t == "" || t == "new ref" || t == "agst ref"
}

// firstUsableBillRefName returns the first non-empty bill reference that maps
// to an actual invoice. Parser-side drop logic uses the same definition as the
// normalizer so we don't discard vouchers the server can dedupe safely.
func firstUsableBillRefName(v RawVoucher) string {
	for _, entry := range v.LedgerEntries {
		for _, bill := range entry.BillAllocations {
			if !isUsableBillRef(bill) {
				continue
			}
			if name := strings.TrimSpace(bill.Name); name != "" {
				return name
			}
		}
	}
	return ""
}

// onlyNewRefName returns the single invoice-producing bill ref's Name when the
// party ledger has exactly one allocation. Lets single-bill vouchers use the
// BillRef name (typically the counterparty's invoice number) instead of
// Tally's internal VoucherNumber, while ignoring cash-on-account allocations.
func onlyNewRefName(party LedgerEntry) string {
	if len(party.BillAllocations) != 1 {
		return ""
	}
	bill := party.BillAllocations[0]
	if !isUsableBillRef(bill) {
		return ""
	}
	return strings.TrimSpace(bill.Name)
}

// voucherParentRef returns the best voucher-level reference available for
// grouping consolidated split rows. GUID is ideal, but when Tally omits it we
// still keep the split rows tied together via voucher number, reference, or
// the first usable bill ref.
func voucherParentRef(v RawVoucher) string {
	return bestVoucherReference(v)
}

// taxRateFromClass extracts the numeric rate from the first tax ledger's
// GSTClass ("IGST@18" → 18.0). Returns 0 when no explicit class is present
// — the server can derive the rate from its own masters if needed.
func taxRateFromClass(entries []LedgerEntry) float64 {
	for _, e := range entries {
		c := strings.TrimSpace(e.GSTClass)
		if at := strings.Index(c, "@"); at > 0 {
			var r float64
			// Suppress error — if it isn't parseable, we just skip.
			_, _ = fmt.Sscanf(c[at+1:], "%f", &r)
			if r > 0 {
				return r
			}
		}
	}
	return 0
}

// round2 rounds a float to 2 decimal places for wire-format consistency.
// Avoids sending "0.30000000000000004" because of IEEE-754 behaviour when
// proration hits recurring decimals.
func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// firstNonEmpty returns the first non-empty string. Used to pick voucher
// number over reference when both are present.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
