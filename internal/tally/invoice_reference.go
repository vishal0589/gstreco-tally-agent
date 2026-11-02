package tally

import (
	"strconv"
	"strings"
	"unicode"
)

// Normalization flags for how a row's invoice_number was keyed (QA v27 L3).
// GST Reco's ingest contract accepts them in normalization_flags; servers
// that predate them store them as opaque text.
const (
	// FlagReferenceConflict: the voucher header number and the party's
	// bill-allocation reference disagree. invoice_number is the header's;
	// the bill reference travels in bill_reference.
	FlagReferenceConflict = "reference_conflict"
	// FlagPlaceholderReference: a reference field held a placeholder literal
	// ("No", "NA", "-", "0", …) and no header or bill reference was usable,
	// so invoice_number is a fallback (narration hint or internal id).
	FlagPlaceholderReference = "placeholder_reference"
	// FlagDuplicateNumberInPeriod: another voucher of the same company,
	// period and party keys to the same normalised invoice number.
	FlagDuplicateNumberInPeriod = "duplicate_number_in_period"
)

// placeholderReferences are literals operators type into Tally's reference
// fields to mean "none". Compared after upper-casing and dropping everything
// but letters and digits, so "N/A", "n.a." and "NA" all match.
var placeholderReferences = map[string]struct{}{
	"NO":    {},
	"NA":    {},
	"NIL":   {},
	"NONE":  {},
	"NULL":  {},
	"NOREF": {},
}

// isPlaceholderReference reports whether s carries no document number:
// empty, punctuation only ("-", "."), all zeros ("0", "000") or a
// placeholder word (QA v27 B24: a bill ref of "No" became the invoice no.).
func isPlaceholderReference(s string) bool {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	core := b.String()
	if core == "" || strings.Trim(core, "0") == "" {
		return true
	}
	_, ok := placeholderReferences[core]
	return ok
}

// usableReference returns s trimmed, or "" when it is a placeholder.
func usableReference(s string) string {
	s = strings.TrimSpace(s)
	if isPlaceholderReference(s) {
		return ""
	}
	return s
}

// invoiceNumberKey normalises an invoice number for comparison: case and
// separators are ignored and leading zeros of each digit group dropped, so
// "INV/0012", "inv-12" and "INV12" all give "INV12", and "SP/846/HR/26-27"
// matches "SP846HR2627".
func invoiceNumberKey(s string) string {
	var runs []string
	var cur strings.Builder
	curDigit := false
	flush := func() {
		if cur.Len() == 0 {
			return
		}
		run := cur.String()
		if curDigit {
			run = strings.TrimLeft(run, "0")
			if run == "" {
				run = "0"
			}
		}
		runs = append(runs, run)
		cur.Reset()
	}
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		isDigit := unicode.IsDigit(r)
		if !isDigit && !unicode.IsLetter(r) {
			flush()
			continue
		}
		if cur.Len() > 0 && isDigit != curDigit {
			flush()
		}
		curDigit = isDigit
		cur.WriteRune(r)
	}
	flush()
	return strings.Join(runs, "")
}

// invoiceKeying is how one voucher's invoice_number was chosen.
type invoiceKeying struct {
	// number is the invoice_number.
	number string
	// billRef is the party's single bill-allocation reference, when it is
	// not a placeholder; shipped as bill_reference.
	billRef string
	flags   []string
}

// keyInvoiceNumber picks a single-bill voucher's invoice_number (QA v27
// B19/B24/B25). The voucher header wins: on the purchase side the supplier
// invoice number (REFERENCE) then the voucher number; on the sales side the
// voucher number (the company's own invoice number) then REFERENCE. The
// party's bill-allocation reference is used only when the header has
// neither, and otherwise travels separately with a reference_conflict flag
// when it names a different number. Placeholder literals are never a key.
func keyInvoiceNumber(v RawVoucher, side IngestSide, party LedgerEntry) invoiceKeying {
	headers := []string{v.Reference, v.VoucherNumber}
	if side == IngestSideSales {
		headers = []string{v.VoucherNumber, v.Reference}
	}
	rawBill := onlyNewRefName(party)
	k := invoiceKeying{billRef: usableReference(rawBill)}

	sawPlaceholder := false
	for _, raw := range []string{v.Reference, v.VoucherNumber, rawBill} {
		if strings.TrimSpace(raw) != "" && isPlaceholderReference(raw) {
			sawPlaceholder = true
		}
	}

	for _, h := range headers {
		if n := usableReference(h); n != "" {
			k.number = n
			if k.billRef != "" && invoiceNumberKey(k.billRef) != invoiceNumberKey(n) {
				k.flags = append(k.flags, FlagReferenceConflict)
			}
			return k
		}
	}
	if k.billRef != "" {
		k.number = k.billRef
		return k
	}
	k.number = firstNonEmpty(
		usableReference(firstUsableBillRefName(v)),
		usableReference(invoiceNumberHintFromNarration(v.Narration)),
		stableInternalVoucherID(v),
	)
	if sawPlaceholder {
		k.flags = append(k.flags, FlagPlaceholderReference)
	}
	return k
}

// withFlags returns flags plus extra, never writing into flags' backing
// array (split rows of one voucher share it).
func withFlags(flags []string, extra ...string) []string {
	if len(extra) == 0 {
		return flags
	}
	out := append([]string(nil), flags...)
	for _, f := range extra {
		out = appendFlag(out, f)
	}
	return out
}

// FlagDuplicateInvoiceNumbers flags every row whose party and normalised
// invoice number are shared with a row of a different voucher (QA v27 B25:
// GK Glass vouchers 62 and 63 both keyed "62"). Call it once per sync run,
// whose rows are one company and one period. Rows are changed in place;
// rows of one consolidated voucher are one voucher and never flag each
// other.
func FlagDuplicateInvoiceNumbers(rows []IngestVoucherRow) {
	type group struct {
		vouchers map[string]struct{}
		idx      []int
	}
	groups := map[string]*group{}
	var order []string
	for i := range rows {
		r := &rows[i]
		num := invoiceNumberKey(r.InvoiceNumber)
		if num == "" {
			continue
		}
		key := rowPartyKey(r) + "\x00" + num
		g, ok := groups[key]
		if !ok {
			g = &group{vouchers: map[string]struct{}{}}
			groups[key] = g
			order = append(order, key)
		}
		g.vouchers[rowVoucherIdentity(r, i)] = struct{}{}
		g.idx = append(g.idx, i)
	}
	for _, key := range order {
		g := groups[key]
		if len(g.vouchers) < 2 {
			continue
		}
		for _, i := range g.idx {
			rows[i].NormalizationFlags = withFlags(rows[i].NormalizationFlags, FlagDuplicateNumberInPeriod)
		}
	}
}

// rowPartyKey is the row's counterparty: its GSTIN, else its name.
func rowPartyKey(r *IngestVoucherRow) string {
	side := "p"
	gstin, name := r.VendorGSTIN, r.VendorName
	if r.CustomerGSTIN != nil || r.CustomerName != nil {
		side = "s"
		gstin, name = r.CustomerGSTIN, r.CustomerName
	}
	if gstin != nil && strings.TrimSpace(*gstin) != "" {
		return side + ":g:" + strings.ToUpper(strings.TrimSpace(*gstin))
	}
	if name != nil && strings.TrimSpace(*name) != "" {
		return side + ":n:" + strings.ToLower(strings.Join(strings.Fields(*name), " "))
	}
	return side + ":"
}

// rowVoucherIdentity names the Tally voucher a row came from: its GUID,
// else type + number + date. A row with none of these is its own voucher.
func rowVoucherIdentity(r *IngestVoucherRow, i int) string {
	if r.TallyVoucherGUID != nil && *r.TallyVoucherGUID != "" {
		return "guid:" + *r.TallyVoucherGUID
	}
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if vn := deref(r.VoucherNumber); vn != "" {
		return "vch:" + deref(r.VoucherTypeName) + "\x00" + vn + "\x00" + deref(r.VoucherDate)
	}
	if pr := deref(r.ParentRef); pr != "" {
		return "parent:" + pr
	}
	return "row:" + strconv.Itoa(i)
}
