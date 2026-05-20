package tally

import "strings"

func stableInternalVoucherID(v RawVoucher) string {
	return strings.TrimSpace(v.GUID)
}

func preferredInvoiceNumber(v RawVoucher) string {
	for _, le := range v.LedgerEntries {
		if ref := firstRelevantBillRefName(le); ref != "" {
			return firstNonEmpty(
				strings.TrimSpace(v.VoucherNumber),
				strings.TrimSpace(v.Reference),
				ref,
				stableInternalVoucherID(v),
			)
		}
	}
	return firstNonEmpty(
		strings.TrimSpace(v.VoucherNumber),
		strings.TrimSpace(v.Reference),
		stableInternalVoucherID(v),
	)
}
