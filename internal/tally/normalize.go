package tally

import (
	"fmt"
	"math"
	"strings"
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
//  2. Classify every LedgerEntry as IGST / CGST / SGST / CESS / party / other,
//     preferring GSTClass over name-pattern. Sum the tax buckets.
//  3. Compute invoice_value from the party ledger's magnitude (Tally's
//     double-entry invariant means |partyAmount| equals invoice total).
//  4. Compute taxable_value = invoice_value - (igst + cgst + sgst + cess).
//  5. For single-bill vouchers, emit one row. For multi-bill, prorate the
//     per-bill amounts by allocation share.
func Normalize(v RawVoucher, opts NormalizeOptions) ([]IngestVoucherRow, error) {
	if v.IsCancelled {
		return nil, nil
	}
	if !shouldNormalizeVoucher(v) {
		return nil, nil
	}

	kind := opts.Kind
	if kind == "" {
		kind = inferKind(v)
	}
	side := inferSide(kind, v)

	party, tax := classifyLedgers(v.LedgerEntries)
	if party == nil {
		return nil, fmt.Errorf("voucher %s: no party ledger (expected LedgerName=%q)", voucherID(v), v.PartyLedgerName)
	}

	invoiceValue := math.Abs(party.Amount)
	taxTotal := tax.IGST + tax.CGST + tax.SGST + tax.CESS
	taxableValue := invoiceValue - taxTotal
	if taxableValue < 0 {
		// Defensive: if totals don't match (missing child ledgers, rounding),
		// fall back to summing the non-tax non-party ledgers so the value is
		// never negative on the wire.
		taxableValue = sumNonTaxNonPartyLedgers(v.LedgerEntries, party)
	}

	base := baseRow(v, kind, side, invoiceValue, taxableValue, tax)

	// Some real Tally setups emit purchase/debit-note vouchers through
	// "Accounting Voucher View" with ISINVOICE=No even though the
	// voucher type itself is still invoice-like and carries the legal
	// supplier reference in VoucherNumber/Reference. For these rows the
	// bill-allocation list often reflects settlement history (Agst Ref /
	// New Ref mix) rather than true consolidated invoice splitting, so
	// forcing the usual bill-ref split would create spurious books rows.
	// Keep them as one legal document keyed by the voucher/reference
	// identity instead.
	if !v.IsInvoice && isInvoiceLikeVoucherType(v.VoucherType) {
		base.InvoiceNumber = firstNonEmpty(
			v.Reference,
			v.VoucherNumber,
			firstRelevantBillRefName(*party),
			v.GUID,
		)
		return []IngestVoucherRow{base}, nil
	}

	bills := relevantBillRefs(*party)
	if len(bills) <= 1 {
		if b := onlyNewRefName(*party); b != "" {
			base.InvoiceNumber = b
		}
		return []IngestVoucherRow{base}, nil
	}

	// Consolidated: one row per bill ref, amounts prorated by share of the
	// party ledger magnitude. parent_ref = v.GUID so the reco inspector can
	// re-stitch the group.
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
		guid := v.GUID
		row.ParentRef = &guid

		if i == last {
			row.InvoiceValue = round2(invoiceValue - accInvoice)
			row.TaxableValue = round2(taxableValue - accTaxable)
			row.IGST = round2(tax.IGST - accIGST)
			row.CGST = round2(tax.CGST - accCGST)
			row.SGST = round2(tax.SGST - accSGST)
			row.CESS = round2(tax.CESS - accCESS)
		} else {
			share := 0.0
			if invoiceValue > 0 {
				share = math.Abs(b.Amount) / invoiceValue
			}
			row.InvoiceValue = round2(math.Abs(b.Amount))
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

// baseRow fills the fields that don't depend on the single-vs-consolidated
// branch. Separated out so both branches can start from the same struct.
func baseRow(v RawVoucher, kind IngestKind, side IngestSide, invoice, taxable float64, tax taxTotals) IngestVoucherRow {
	row := IngestVoucherRow{
		InvoiceNumber: firstNonEmpty(v.VoucherNumber, v.Reference),
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
	if v.PlaceOfSupply != "" {
		p := v.PlaceOfSupply
		row.PlaceOfSupply = &p
	}
	if v.ReverseCharge {
		rc := true
		row.ReverseCharge = &rc
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

func shouldNormalizeVoucher(v RawVoucher) bool {
	if v.IsInvoice {
		return true
	}
	return isInvoiceLikeVoucherType(v.VoucherType)
}

func isInvoiceLikeVoucherType(voucherType string) bool {
	n := strings.ToUpper(strings.TrimSpace(voucherType))
	if n == "" {
		return false
	}
	if strings.Contains(n, "DEBIT NOTE") || strings.Contains(n, "CREDIT NOTE") {
		return true
	}
	if strings.Contains(n, "INVOICE") &&
		!strings.Contains(n, "ORDER") &&
		!strings.Contains(n, "RECEIPT NOTE") {
		return true
	}
	return false
}

// taxTotals is the bucket sum across all tax ledgers in a voucher. All values
// are stored absolute (sign-independent) because invoice_value already uses
// abs(party.Amount); taxes are the positive portions that add up to invoice.
type taxTotals struct {
	IGST, CGST, SGST, CESS float64
}

// classifyLedgers splits LedgerEntries into the single party ledger and the
// tax totals. Non-party non-tax ledgers (purchase / sales accounts, freight,
// discounts) are ignored at this layer — taxable_value is recomputed as the
// residual so rounding and sign conventions don't fight us.
func classifyLedgers(entries []LedgerEntry) (party *LedgerEntry, tax taxTotals) {
	for i := range entries {
		e := &entries[i]
		if e.IsPartyLedger && party == nil {
			party = e
			continue
		}
		switch classifyTaxLedger(e.GSTClass, e.LedgerName) {
		case "IGST":
			tax.IGST += math.Abs(e.Amount)
		case "CGST":
			tax.CGST += math.Abs(e.Amount)
		case "SGST":
			tax.SGST += math.Abs(e.Amount)
		case "CESS":
			tax.CESS += math.Abs(e.Amount)
		}
	}
	return party, tax
}

// classifyTaxLedger labels a single LedgerEntry. Prefers the explicit
// GSTClass field (only populated when the ledger was created via Tally's GST
// masters) and falls back to substring match on the ledger name for legacy
// ledgers that people created manually.
//
// UTGST (Union Territory GST) is folded into SGST — the GSTR-2B schema has
// no separate bucket and the combined tax rate matches SGST everywhere.
func classifyTaxLedger(gstClass, name string) string {
	if c := strings.ToUpper(strings.TrimSpace(gstClass)); c != "" {
		switch {
		case strings.HasPrefix(c, "IGST"):
			return "IGST"
		case strings.HasPrefix(c, "CGST"):
			return "CGST"
		case strings.HasPrefix(c, "SGST"), strings.HasPrefix(c, "UTGST"):
			return "SGST"
		case strings.HasPrefix(c, "CESS"):
			return "CESS"
		}
	}
	n := strings.ToUpper(name)
	switch {
	case strings.Contains(n, "IGST"):
		return "IGST"
	case strings.Contains(n, "CGST"):
		return "CGST"
	case strings.Contains(n, "SGST"), strings.Contains(n, "UTGST"):
		return "SGST"
	case strings.Contains(n, "CESS"):
		return "CESS"
	}
	return ""
}

// relevantBillRefs filters BillAllocations to New Ref and Agst Ref — the
// only two types that correspond to actual invoices. "On Account" and
// "Advance" are cash-on-account, not invoice lines, and shouldn't produce
// IngestVoucherRow entries.
func relevantBillRefs(party LedgerEntry) []BillRef {
	out := make([]BillRef, 0, len(party.BillAllocations))
	for _, b := range party.BillAllocations {
		t := strings.ToLower(strings.TrimSpace(b.BillType))
		if t == "" || t == "new ref" || t == "agst ref" {
			out = append(out, b)
		}
	}
	return out
}

// onlyNewRefName returns the single bill ref's Name when the party ledger
// has exactly one allocation. Lets single-bill vouchers use the BillRef
// name (typically the counterparty's invoice number) instead of Tally's
// internal VoucherNumber.
func onlyNewRefName(party LedgerEntry) string {
	if len(party.BillAllocations) != 1 {
		return ""
	}
	return party.BillAllocations[0].Name
}

func firstRelevantBillRefName(party LedgerEntry) string {
	for _, b := range relevantBillRefs(party) {
		if name := strings.TrimSpace(b.Name); name != "" {
			return name
		}
	}
	return ""
}

// sumNonTaxNonPartyLedgers is the fallback for taxable_value when the
// straight invoice_value - tax subtraction goes negative (damaged data).
func sumNonTaxNonPartyLedgers(entries []LedgerEntry, party *LedgerEntry) float64 {
	total := 0.0
	for i := range entries {
		e := &entries[i]
		if party != nil && e == party {
			continue
		}
		if classifyTaxLedger(e.GSTClass, e.LedgerName) != "" {
			continue
		}
		total += math.Abs(e.Amount)
	}
	return total
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
