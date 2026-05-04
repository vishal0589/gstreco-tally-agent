package tally

import (
	"fmt"
	"math"
	"strings"
)

// NormalizeJournal converts one raw Tally journal voucher into the agent's
// journal-ingest shape. Returns nil,nil for cancelled vouchers so callers can
// skip them without treating the row as malformed.
func NormalizeJournal(v RawVoucher) (*JournalVoucherRow, error) {
	if v.IsCancelled {
		return nil, nil
	}
	if v.Date.IsZero() {
		return nil, fmt.Errorf("voucher %s: missing voucher date", voucherID(v))
	}

	entries := make([]JournalVoucherEntry, 0, len(v.LedgerEntries))
	totalDebit, totalCredit := 0.0, 0.0
	for _, le := range v.LedgerEntries {
		amount := round2(le.Amount)
		entryType := entryDirection(amount)
		if amount > 0 {
			totalDebit += amount
		} else if amount < 0 {
			totalCredit += math.Abs(amount)
		}
		entry := JournalVoucherEntry{
			LedgerName: strings.TrimSpace(le.LedgerName),
			Amount:     amount,
			EntryType:  entryType,
			GSTClass:   strings.TrimSpace(le.GSTClass),
		}
		if le.IsPartyLedger {
			isParty := true
			entry.IsPartyLedger = &isParty
		}
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("voucher %s: no ledger entries", voucherID(v))
	}

	partyName, partyGSTIN := chooseAccountingParty(v)
	total := round2(math.Max(totalDebit, totalCredit))
	refs := extractBillReferences(v)
	voucherType := firstNonEmpty(strings.TrimSpace(v.VoucherType), "Journal Voucher")

	row := &JournalVoucherRow{
		VoucherNumber: v.VoucherNumber,
		VoucherDate:   v.Date.Format("2006-01-02"),
		Entries:       entries,
	}
	if row.VoucherNumber == "" {
		row.VoucherNumber = preferredInvoiceNumber(v)
	}
	if row.VoucherNumber == "" {
		row.VoucherNumber = stableInternalVoucherID(v)
	}
	if row.VoucherNumber == "" {
		return nil, fmt.Errorf("voucher %s: missing voucher number", voucherID(v))
	}
	if v.GUID != "" {
		row.TallyVoucherGUID = strPtr(v.GUID)
	}
	row.VoucherTypeName = strPtr(voucherType)
	if narration := strings.TrimSpace(v.Narration); narration != "" {
		row.Narration = &narration
	}
	if total > 0 {
		row.TotalAmount = float64Ptr(total)
	}
	if partyName != "" {
		row.PartyName = &partyName
	}
	if partyGSTIN != "" {
		row.PartyGSTIN = &partyGSTIN
	}
	if len(refs) > 0 {
		row.AgainstInvoiceRefs = refs
	}
	if isRCMJournal(v) {
		row.IsRCM = boolPtr(true)
	}
	return row, nil
}

// NormalizePayment converts one raw Tally payment voucher into the
// payment-ingest shape. Cancelled vouchers are skipped with nil,nil.
func NormalizePayment(v RawVoucher) (*PaymentVoucherRow, error) {
	if v.IsCancelled {
		return nil, nil
	}
	if v.Date.IsZero() {
		return nil, fmt.Errorf("voucher %s: missing voucher date", voucherID(v))
	}

	entries := make([]PaymentVoucherEntry, 0, len(v.LedgerEntries))
	totalDebit, totalCredit := 0.0, 0.0
	for _, le := range v.LedgerEntries {
		amount := round2(le.Amount)
		if amount > 0 {
			totalDebit += amount
		} else if amount < 0 {
			totalCredit += math.Abs(amount)
		}
		entries = append(entries, PaymentVoucherEntry{
			LedgerName: strings.TrimSpace(le.LedgerName),
			Amount:     amount,
			EntryType:  entryDirection(amount),
		})
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("voucher %s: no ledger entries", voucherID(v))
	}

	partyName, partyGSTIN := chooseAccountingParty(v)
	total := round2(math.Max(totalDebit, totalCredit))
	refs := extractBillReferences(v)
	voucherType := firstNonEmpty(strings.TrimSpace(v.VoucherType), "Payment Voucher")

	row := &PaymentVoucherRow{
		VoucherNumber: v.VoucherNumber,
		VoucherDate:   v.Date.Format("2006-01-02"),
		Entries:       entries,
	}
	if row.VoucherNumber == "" {
		row.VoucherNumber = preferredInvoiceNumber(v)
	}
	if row.VoucherNumber == "" {
		row.VoucherNumber = stableInternalVoucherID(v)
	}
	if row.VoucherNumber == "" {
		return nil, fmt.Errorf("voucher %s: missing voucher number", voucherID(v))
	}
	if v.GUID != "" {
		row.TallyVoucherGUID = strPtr(v.GUID)
	}
	row.VoucherTypeName = strPtr(voucherType)
	if narration := strings.TrimSpace(v.Narration); narration != "" {
		row.Narration = &narration
	}
	if partyName != "" {
		row.PartyName = &partyName
	}
	if partyGSTIN != "" {
		row.PartyGSTIN = &partyGSTIN
	}
	if instrumentType := inferInstrumentType(v); instrumentType != "" {
		row.InstrumentType = &instrumentType
	}
	if total > 0 {
		row.TotalAmount = float64Ptr(total)
	}
	if len(refs) > 0 {
		row.AgainstInvoiceRefs = refs
	}
	return row, nil
}

// NormalizeTaxLedgerRows converts parsed ledger balances into the month-wise
// request rows the GST Reco server expects.
func NormalizeTaxLedgerRows(raw []RawTaxLedger, period string) []TaxLedgerMonthlyRow {
	out := make([]TaxLedgerMonthlyRow, 0, len(raw))
	for _, row := range raw {
		name := strings.TrimSpace(row.Name)
		if name == "" {
			continue
		}
		item := TaxLedgerMonthlyRow{
			Period:         period,
			LedgerName:     name,
			OpeningBalance: round2(row.OpeningBalance),
			DebitAmount:    round2(row.DebitTotals),
			CreditAmount:   round2(row.CreditTotals),
			ClosingBalance: round2(row.ClosingBalance),
		}
		if parent := strings.TrimSpace(row.Parent); parent != "" {
			item.ParentGroup = &parent
		}
		out = append(out, item)
	}
	return out
}

func chooseAccountingParty(v RawVoucher) (name, gstin string) {
	if n := strings.TrimSpace(v.PartyLedgerName); n != "" {
		return n, strings.ToUpper(strings.TrimSpace(v.PartyGSTIN))
	}
	for _, le := range v.LedgerEntries {
		if len(relevantBillRefs(le)) == 0 {
			continue
		}
		if n := strings.TrimSpace(le.LedgerName); n != "" {
			return n, strings.ToUpper(strings.TrimSpace(v.PartyGSTIN))
		}
	}
	for _, le := range v.LedgerEntries {
		name := strings.TrimSpace(le.LedgerName)
		if name == "" || isBankOrCashLedger(name) || classifyTaxLedger(le.GSTClass, name) != "" {
			continue
		}
		return name, strings.ToUpper(strings.TrimSpace(v.PartyGSTIN))
	}
	return "", strings.ToUpper(strings.TrimSpace(v.PartyGSTIN))
}

func extractBillReferences(v RawVoucher) []VoucherBillReference {
	out := make([]VoucherBillReference, 0, len(v.LedgerEntries))
	seen := make(map[string]struct{})
	for _, le := range v.LedgerEntries {
		direction := entryDirection(le.Amount)
		for _, bill := range relevantBillRefs(le) {
			reference := strings.TrimSpace(bill.Name)
			if reference == "" {
				continue
			}
			billType := strings.TrimSpace(bill.BillType)
			key := strings.ToUpper(reference) + "|" + strings.ToUpper(billType) + "|" + fmt.Sprintf("%.2f", round2(math.Abs(bill.Amount)))
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}

			item := VoucherBillReference{Reference: reference}
			if amount := round2(math.Abs(bill.Amount)); amount > 0 {
				item.Amount = float64Ptr(amount)
			}
			if billType != "" {
				item.BillType = &billType
			}
			if direction != "" {
				item.Direction = &direction
			}
			out = append(out, item)
		}
	}
	return out
}

func entryDirection(amount float64) string {
	switch {
	case amount > 0:
		return "debit"
	case amount < 0:
		return "credit"
	default:
		return ""
	}
}

func inferInstrumentType(v RawVoucher) string {
	n := strings.ToUpper(strings.TrimSpace(v.VoucherType))
	switch {
	case strings.Contains(n, "CASH"):
		return "cash"
	case strings.Contains(n, "BANK"):
		return "bank"
	}
	for _, le := range v.LedgerEntries {
		name := strings.ToUpper(strings.TrimSpace(le.LedgerName))
		switch {
		case strings.Contains(name, "CASH"):
			return "cash"
		case strings.Contains(name, "BANK"):
			return "bank"
		}
	}
	return ""
}

func isBankOrCashLedger(name string) bool {
	n := strings.ToUpper(strings.TrimSpace(name))
	return strings.Contains(n, "BANK") || strings.Contains(n, "CASH")
}

func isRCMJournal(v RawVoucher) bool {
	if v.ReverseCharge {
		return true
	}
	texts := []string{v.VoucherType, v.Narration}
	for _, le := range v.LedgerEntries {
		texts = append(texts, le.LedgerName, le.GSTClass)
	}
	for _, value := range texts {
		if strings.Contains(strings.ToUpper(strings.TrimSpace(value)), "RCM") {
			return true
		}
	}
	return false
}

func strPtr(value string) *string { return &value }

func float64Ptr(value float64) *float64 { return &value }

func boolPtr(value bool) *bool { return &value }
