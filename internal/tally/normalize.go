package tally

import (
	"fmt"
	"math"
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
	// DutyLedgers is the company's Duties & Taxes ledger masters (QA v18 R3).
	// Only membership of this book makes a leg GST. Nil: no master evidence,
	// so GST comes from the GST class alone and a leg merely named after a
	// GST head is uncertain.
	DutyLedgers *DutyLedgerBook
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
//  2. Give every LedgerEntry one role (classifyLeg): party, tax, base,
//     withholding, freight, discount, rounding, adjustment, other duty or
//     uncertain, from the duty-ledger master, then the GST class, then the
//     name.
//  3. Sum each role's signed amount, oriented so legs on the side opposite
//     the party count as positive; the same arithmetic serves purchases,
//     sales, credit notes and debit notes.
//  4. taxable = base + freight + discount + adjustment; GST per head = its
//     tax legs; gross = taxable + GST + rounding + other duty + uncertain.
//     Gross is never the net payable: TDS withheld from the party is
//     reported separately and is neither GST nor taxable.
//  5. For single-bill vouchers, emit one row. For multi-bill, prorate by each
//     bill's signed share of the party amount, the residual on the last row.
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
	// up in the purchase lane OR the journal lane. See breakDownVoucher for the
	// voucher-mode party fallback.

	kind := opts.Kind
	if kind == "" {
		kind = inferKind(v)
	}
	side := inferSide(kind, v)

	b := breakDownVoucher(v, opts.DutyLedgers)
	if b.party == nil {
		return nil, fmt.Errorf("voucher %s: no party ledger (expected LedgerName=%q)", voucherID(v), v.PartyLedgerName)
	}
	amounts := b.wireAmounts()
	legs := b.ingestLegs(opts.Ledgers)

	base := baseRow(v, kind, side, amounts.gross, amounts.taxable, amounts.tax)
	fillPartyFromLedger(&base, side, b.party.LedgerName, v.Date, opts.Ledgers)
	base.VoucherLegs = legs

	bills := relevantBillRefs(*b.party)
	if len(bills) <= 1 {
		if name := onlyNewRefName(*b.party); name != "" {
			base.InvoiceNumber = name
		}
		setSourceAmounts(&base, amounts)
		base.NormalizationFlags = b.flags
		return []IngestVoucherRow{base}, nil
	}

	// Consolidated: one row per bill ref, amounts prorated by the bill's
	// signed share of the party amount. parent_ref carries the best
	// voucher-level identity we have so the reco inspector can still
	// re-stitch the group when Tally omits GUID on the parent voucher.
	//
	// Residual allocation: we prorate the first N-1 rows by share, then
	// assign whatever's left to the last row. Without this, IEEE-754 drift
	// on shares like 1/3 can leave split rows that don't sum to the original
	// totals — e.g. a tax of ₹33 split three ways gives 3×10.99=32.97 with
	// naive rounding. Users see the discrepancy as "my books don't match
	// reco" and we get a support ticket. Residual assignment on the last
	// row guarantees every tax + value bucket sums exactly.
	flags := b.flags
	billTotal := 0.0
	for _, bill := range bills {
		billTotal += bill.Amount
	}
	if math.Abs(billTotal-b.party.Amount) > 0.01 {
		flags = appendFlag(flags, fmt.Sprintf("bill allocations total %.2f but the party amount is %.2f: rows prorated by each bill's share of the party amount", billTotal, b.party.Amount))
	}
	rows := make([]IngestVoucherRow, 0, len(bills))
	var acc wireAmounts
	last := len(bills) - 1
	for i, bill := range bills {
		row := base
		row.InvoiceNumber = bill.Name
		if parentRef := voucherParentRef(v); parentRef != "" {
			parent := parentRef
			row.ParentRef = &parent
		}
		var part wireAmounts
		if i == last {
			part = amounts.minus(acc)
		} else {
			share := 0.0
			if b.party.Amount != 0 {
				share = bill.Amount / b.party.Amount
			}
			part = amounts.scaled(share)
			acc = acc.plus(part)
		}
		row.InvoiceValue = part.gross
		row.TaxableValue = part.taxable
		row.IGST, row.CGST, row.SGST, row.CESS = part.tax.IGST, part.tax.CGST, part.tax.SGST, part.tax.CESS
		setSourceAmounts(&row, part)
		row.NormalizationFlags = flags
		rows = append(rows, row)
	}
	return rows, nil
}

// classifiedLeg is one ledger leg with its role.
type classifiedLeg struct {
	entry    *LedgerEntry
	decision legDecision
	// oriented is the leg's amount signed so the side opposite the party is
	// positive.
	oriented float64
}

// voucherBreakdown is an invoice voucher split by role.
type voucherBreakdown struct {
	party *LedgerEntry
	legs  []classifiedLeg
	sums  roleSums
	flags []string
	book  *DutyLedgerBook
}

// roleSums are the oriented sums of each role.
type roleSums struct {
	base, freight, discount, adjustment, rounding, otherDuty, uncertain float64
	// inventory is the base taken from inventory entries when the ledger legs
	// alone do not balance (item invoices whose purchase / sales ledger sits
	// only in the inventory allocations).
	inventory float64
	tax       taxTotals
	// rcmLiability is GST on the party's side of a reverse-charge voucher:
	// the recipient's own liability, not the supplier's tax.
	rcmLiability float64
	// tds is withheld from the party, tcs collected by it: positive in the
	// usual direction.
	tds, tcs float64
	// party is the party ledger's magnitude, the net payable.
	party float64
}

func (s roleSums) taxable() float64 {
	return s.base + s.freight + s.discount + s.adjustment + s.inventory
}

func (s roleSums) gross() float64 {
	return s.taxable() + s.tax.total() + s.rounding + s.otherDuty + s.uncertain
}

// breakDownVoucher gives every leg a role and sums the roles.
func breakDownVoucher(v RawVoucher, book *DutyLedgerBook) voucherBreakdown {
	b := voucherBreakdown{book: book}
	entries := v.LedgerEntries
	decisions := make([]legDecision, len(entries))
	partyIdx := -1
	for i := range entries {
		if entries[i].IsPartyLedger && partyIdx < 0 {
			partyIdx = i
			decisions[i] = legDecision{role: LegParty, evidence: EvidencePartyLedger}
			continue
		}
		decisions[i] = classifyLeg(entries[i], book)
	}
	// Voucher-mode fallback: no ledger matched PARTYLEDGERNAME, so recover the
	// party as the bill-bearing ledger. In a purchase/sales voucher only the
	// party ledger (Sundry Creditor / Debtor) carries bills. If Tally spread
	// bills across more than one line, take the largest-magnitude bearer (the
	// true invoice total).
	if partyIdx < 0 {
		for i := range entries {
			switch decisions[i].role {
			case LegTax, LegWithholding, LegOtherDuty:
				continue
			}
			if len(entries[i].BillAllocations) == 0 {
				continue
			}
			if partyIdx < 0 || math.Abs(entries[i].Amount) > math.Abs(entries[partyIdx].Amount) {
				partyIdx = i
			}
		}
		if partyIdx >= 0 {
			decisions[partyIdx] = legDecision{role: LegParty, evidence: EvidencePartyLedger}
		}
	}
	if partyIdx < 0 {
		return b
	}
	b.party = &entries[partyIdx]
	if book == nil {
		b.flags = appendFlag(b.flags, "no Duties & Taxes ledger masters: GST taken from the GST class alone")
	}

	// orient makes the side opposite the party positive.
	orient := -1.0
	switch {
	case b.party.Amount < 0:
		orient = 1
	case b.party.Amount == 0:
		orient = 1
		b.flags = appendFlag(b.flags, fmt.Sprintf("party ledger %q has a zero amount: legs oriented as if it were a debit", b.party.LedgerName))
	}
	s := &b.sums
	s.party = math.Abs(b.party.Amount)

	ledgerSum := 0.0
	for i := range entries {
		e := &entries[i]
		ledgerSum += e.Amount
		d := decisions[i]
		for _, f := range d.flags {
			b.flags = appendFlag(b.flags, f)
		}
		o := orient * e.Amount
		if i == partyIdx {
			b.legs = append(b.legs, classifiedLeg{entry: e, decision: d, oriented: o})
			continue
		}
		switch d.role {
		case LegBase, LegFreight:
			if o < 0 {
				b.flags = appendFlag(b.flags, fmt.Sprintf("%q (%.2f) is on the party's side of the voucher: netted as an adjustment", e.LedgerName, e.Amount))
				d.role = LegAdjustment
			}
		}
		switch d.role {
		case LegBase:
			s.base += o
		case LegFreight:
			s.freight += o
		case LegDiscount:
			s.discount += o
		case LegAdjustment:
			s.adjustment += o
		case LegRounding:
			s.rounding += o
		case LegOtherDuty:
			s.otherDuty += o
		case LegUncertain:
			s.uncertain += o
		case LegWithholding:
			if d.withholding == WithholdingTCS {
				s.tcs += o
				if o < 0 {
					b.flags = appendFlag(b.flags, fmt.Sprintf("TCS leg %q (%.2f) is on the party's side: netted", e.LedgerName, e.Amount))
				}
			} else {
				s.tds -= o
				if o > 0 {
					b.flags = appendFlag(b.flags, fmt.Sprintf("TDS leg %q (%.2f) is on the side opposite the party: netted", e.LedgerName, e.Amount))
				}
			}
		case LegTax:
			switch {
			case o >= 0:
				s.tax.add(d.head, o)
			case v.ReverseCharge:
				s.rcmLiability -= o
				b.flags = appendFlag(b.flags, fmt.Sprintf("%q (%.2f) is the reverse-charge liability on the party's side: kept out of GST and gross", e.LedgerName, e.Amount))
			default:
				s.tax.add(d.head, o)
				b.flags = appendFlag(b.flags, fmt.Sprintf("%q (%.2f) is a tax leg on the party's side: netted as a reversal", e.LedgerName, e.Amount))
			}
		}
		decisions[i] = d
		b.legs = append(b.legs, classifiedLeg{entry: e, decision: d, oriented: o})
	}

	// Item invoices can carry the purchase / sales ledger only inside the
	// inventory allocations, so the ledger legs alone don't balance. Take the
	// base from the inventory entries only when they close that gap.
	if math.Abs(ledgerSum) > 0.01 && len(v.InventoryEntries) > 0 {
		items := 0.0
		for _, ie := range v.InventoryEntries {
			items += ie.Amount
		}
		if math.Abs(ledgerSum+items) <= 0.01 {
			s.inventory = orient * items
			b.flags = appendFlag(b.flags, fmt.Sprintf("taxable base %.2f taken from %d inventory entries: the ledger legs alone did not balance", s.inventory, len(v.InventoryEntries)))
		}
	}

	if residual := s.gross() - s.tds + s.tcs - s.rcmLiability - s.party; math.Abs(residual) > 0.01 {
		b.flags = appendFlag(b.flags, fmt.Sprintf("legs do not balance: they establish %.2f net of TDS/TCS against a party amount of %.2f", s.gross()-s.tds+s.tcs-s.rcmLiability, s.party))
	}
	// No GST rate is above 100%: tax larger than the taxable value means a
	// leg's evidence is wrong. Flagged, not guessed away.
	if gst := s.tax.total(); gst > s.taxable()+0.01 {
		b.flags = appendFlag(b.flags, fmt.Sprintf("GST %.2f exceeds the taxable value %.2f: check the ledger evidence", gst, s.taxable()))
	}
	return b
}

// wireAmounts are the numbers one row carries.
type wireAmounts struct {
	gross, taxable float64
	tax            taxTotals
	tds, tcs, net  float64
}

// wireAmounts rounds the sums for the wire. Nothing negative goes out: a
// negative taxable value or tax head is sent as zero and flagged, never
// turned into a positive base.
func (b *voucherBreakdown) wireAmounts() wireAmounts {
	s := b.sums
	w := wireAmounts{
		gross:   round2(s.gross()),
		taxable: round2(s.taxable()),
		tax: taxTotals{
			IGST: round2(s.tax.IGST), CGST: round2(s.tax.CGST),
			SGST: round2(s.tax.SGST), CESS: round2(s.tax.CESS),
		},
		tds: round2(s.tds),
		tcs: round2(s.tcs),
		net: round2(s.party),
	}
	clamp := func(label string, v *float64) {
		if *v < 0 {
			b.flags = appendFlag(b.flags, fmt.Sprintf("%s from the legs is negative (%.2f): sent as 0", label, *v))
			*v = 0
		}
	}
	clamp("taxable value", &w.taxable)
	clamp("gross", &w.gross)
	clamp("IGST", &w.tax.IGST)
	clamp("CGST", &w.tax.CGST)
	clamp("SGST", &w.tax.SGST)
	clamp("CESS", &w.tax.CESS)
	clamp("TDS", &w.tds)
	clamp("TCS", &w.tcs)
	return w
}

func (w wireAmounts) scaled(share float64) wireAmounts {
	return wireAmounts{
		gross:   round2(w.gross * share),
		taxable: round2(w.taxable * share),
		tax: taxTotals{
			IGST: round2(w.tax.IGST * share), CGST: round2(w.tax.CGST * share),
			SGST: round2(w.tax.SGST * share), CESS: round2(w.tax.CESS * share),
		},
		tds: round2(w.tds * share),
		tcs: round2(w.tcs * share),
		net: round2(w.net * share),
	}
}

func (w wireAmounts) plus(o wireAmounts) wireAmounts {
	return wireAmounts{
		gross: w.gross + o.gross, taxable: w.taxable + o.taxable,
		tax: taxTotals{
			IGST: w.tax.IGST + o.tax.IGST, CGST: w.tax.CGST + o.tax.CGST,
			SGST: w.tax.SGST + o.tax.SGST, CESS: w.tax.CESS + o.tax.CESS,
		},
		tds: w.tds + o.tds, tcs: w.tcs + o.tcs, net: w.net + o.net,
	}
}

func (w wireAmounts) minus(o wireAmounts) wireAmounts {
	return wireAmounts{
		gross: round2(w.gross - o.gross), taxable: round2(w.taxable - o.taxable),
		tax: taxTotals{
			IGST: round2(w.tax.IGST - o.tax.IGST), CGST: round2(w.tax.CGST - o.tax.CGST),
			SGST: round2(w.tax.SGST - o.tax.SGST), CESS: round2(w.tax.CESS - o.tax.CESS),
		},
		tds: round2(w.tds - o.tds), tcs: round2(w.tcs - o.tcs), net: round2(w.net - o.net),
	}
}

// setSourceAmounts fills the optional gross / TDS / TCS / net payable fields.
func setSourceAmounts(row *IngestVoucherRow, w wireAmounts) {
	gross, net := w.gross, w.net
	row.GrossAmount, row.NetPayable = &gross, &net
	row.TDSAmount, row.TCSAmount = nil, nil
	if w.tds > 0 {
		tds := w.tds
		row.TDSAmount = &tds
	}
	if w.tcs > 0 {
		tcs := w.tcs
		row.TCSAmount = &tcs
	}
}

// LedgerGroups is implemented by a ledger directory that knows each ledger's
// parent group and the reserved group its chain ends in.
type LedgerGroups interface {
	ParentGroup(ledgerName string) string
	PrimaryGroup(ledgerName string) string
}

// dutiesAndTaxesGroup is Tally's reserved group for duty ledgers.
const dutiesAndTaxesGroup = "Duties & Taxes"

// ingestLegs is every ledger leg as Tally sent it, with its role.
func (b *voucherBreakdown) ingestLegs(ledgers LedgerDirectory) []IngestVoucherLeg {
	groups, _ := ledgers.(LedgerGroups)
	out := make([]IngestVoucherLeg, 0, len(b.legs))
	for _, l := range b.legs {
		d := l.decision
		leg := IngestVoucherLeg{
			LedgerName:    l.entry.LedgerName,
			Amount:        round2(l.entry.Amount),
			IsPartyLedger: d.role == LegParty,
			GSTClass:      optionalString(l.entry.GSTClass),
			TaxType:       optionalString(d.taxType),
			GSTDutyHead:   optionalString(d.dutyHead),
			Classification: IngestLegClassification{
				Role:      serverLegRole(d),
				Method:    serverLegMethod(d),
				AgentRole: d.role,
				Evidence:  d.evidence,
			},
		}
		if d.head != "" {
			head := strings.ToLower(d.head)
			leg.Classification.Head = &head
		}
		if d.evidence == EvidenceDutyMaster {
			leg.ParentGroup = optionalString(d.parent)
			primary := dutiesAndTaxesGroup
			leg.PrimaryGroup = &primary
		} else if groups != nil {
			leg.ParentGroup = optionalString(groups.ParentGroup(l.entry.LedgerName))
			leg.PrimaryGroup = optionalString(groups.PrimaryGroup(l.entry.LedgerName))
		}
		out = append(out, leg)
	}
	return out
}

// serverLegRole maps a role to GST Reco's leg roles.
func serverLegRole(d legDecision) string {
	switch d.role {
	case LegParty:
		return "party"
	case LegTax:
		return "tax"
	case LegWithholding:
		if d.withholding == WithholdingTCS {
			return "tcs"
		}
		return "tds"
	case LegBase, LegFreight, LegDiscount, LegAdjustment:
		return "base"
	case LegRounding:
		return "round_off"
	}
	return "unclassified"
}

// serverLegMethod maps the evidence to GST Reco's classification methods.
func serverLegMethod(d legDecision) string {
	switch d.evidence {
	case EvidencePartyLedger, EvidenceNotDutyLedger:
		return "group"
	case EvidenceDutyMaster, EvidenceGSTClass:
		return "duty_field"
	}
	if d.role == LegUncertain || d.role == LegOtherDuty {
		return "unclassified"
	}
	return "name_fallback"
}

func optionalString(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// appendFlag adds a flag once.
func appendFlag(flags []string, flag string) []string {
	for _, f := range flags {
		if f == flag {
			return flags
		}
	}
	return append(flags, flag)
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

// taxTotals is the bucket sum across all tax legs in a voucher, signed:
// legs on the side opposite the party are positive, so a reversal nets.
type taxTotals struct {
	IGST, CGST, SGST, CESS float64
}

func (t taxTotals) total() float64 {
	return t.IGST + t.CGST + t.SGST + t.CESS
}

func (t *taxTotals) add(head string, amount float64) {
	switch head {
	case "IGST":
		t.IGST += amount
	case "CGST":
		t.CGST += amount
	case "SGST":
		t.SGST += amount
	case "CESS":
		t.CESS += amount
	}
}

// classifyTaxLedger is the GST head a leg's GST class names, or "". A name is
// never read here: whether a leg is GST is decided per voucher with the
// duty-ledger masters (classifyLeg).
func classifyTaxLedger(gstClass, _ string) string {
	return headFromGSTClass(gstClass)
}

// namesGSTLedger keeps a GST-looking ledger from being guessed as a journal
// or payment voucher's party: a GST class head, a head in the name read
// word by word, or the word GST. Taxable purchase / sales accounts named
// after their tax ("Purchase (IGST)@18%") are not GST ledgers.
func namesGSTLedger(gstClass, name string) bool {
	if headFromGSTClass(gstClass) != "" {
		return true
	}
	if looksLikeCommercialAccountLedger(compactTaxLedgerToken(name)) {
		return false
	}
	return headFromNameTokens(name) != "" || hasTaxLedgerWord(name, "GST")
}

// ledgerWords is a ledger name upper-cased and split on everything but
// letters and digits.
func ledgerWords(value string) []string {
	return strings.FieldsFunc(strings.ToUpper(value), func(r rune) bool {
		return !((r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
	})
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
