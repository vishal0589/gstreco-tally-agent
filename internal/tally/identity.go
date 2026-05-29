package tally

import (
	"fmt"
	"strings"
	"unicode"
)

var narrationInvoiceKeywords = map[string]struct{}{
	"AGAINST":   {},
	"BILL":      {},
	"DOC":       {},
	"DOCUMENT":  {},
	"FROM":      {},
	"INV":       {},
	"INVOICE":   {},
	"NO":        {},
	"NUMBER":    {},
	"REF":       {},
	"REFERENCE": {},
	"SUPPLIER":  {},
	"VENDOR":    {},
}

var narrationInvoiceStopTokens = map[string]struct{}{
	"CGST":  {},
	"CESS":  {},
	"GSTIN": {},
	"HSN":   {},
	"IGST":  {},
	"NOS":   {},
	"RCM":   {},
	"SAC":   {},
	"SGST":  {},
	"UTGST": {},
}

func stableInternalVoucherID(v RawVoucher) string {
	if v.GUID != "" {
		return v.GUID
	}
	if masterID := sanitizeSyntheticIdentityPart(v.MasterID); masterID != "" {
		return "TALLYMID-" + masterID
	}
	return ""
}

func preferredInvoiceNumber(v RawVoucher) string {
	return firstNonEmpty(
		v.VoucherNumber,
		v.Reference,
		firstUsableBillRefName(v),
		invoiceNumberHintFromNarration(v.Narration),
		stableInternalVoucherID(v),
	)
}

func bestVoucherReference(v RawVoucher) string {
	return firstNonEmpty(
		v.GUID,
		v.VoucherNumber,
		v.Reference,
		firstUsableBillRefName(v),
		invoiceNumberHintFromNarration(v.Narration),
		stableInternalVoucherID(v),
	)
}

func voucherID(v RawVoucher) string {
	if ref := bestVoucherReference(v); ref != "" {
		return ref
	}
	if v.AlterID != 0 {
		return fmt.Sprintf("alterid-%d", v.AlterID)
	}
	return "(unknown)"
}

func hasUsableVoucherIdentity(v RawVoucher) bool {
	return bestVoucherReference(v) != ""
}

// firstUsableBillRefName returns the first non-empty bill reference that maps
// to an actual invoice. Parser-side drop logic uses the same definition as the
// normalizer so we don't discard vouchers the server can dedupe safely.
func firstUsableBillRefName(v RawVoucher) string {
	for _, entry := range v.LedgerEntries {
		if name := firstRelevantBillRefName(entry); name != "" {
			return name
		}
	}
	return ""
}

func invoiceNumberHintFromNarration(narration string) string {
	tokens := narrationInvoiceTokens(narration)
	if len(tokens) == 0 {
		return ""
	}

	best := ""
	bestScore := 0
	for i, token := range tokens {
		score := narrationInvoiceTokenScore(token, tokenKeywordContext(tokens, i))
		if score > bestScore {
			bestScore = score
			best = token
		}
	}
	if bestScore < 4 {
		return ""
	}
	return best
}

func narrationInvoiceTokens(narration string) []string {
	rawTokens := strings.FieldsFunc(strings.ToUpper(strings.TrimSpace(narration)), func(r rune) bool {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			return false
		case r == '/', r == '-', r == '_', r == '#':
			return false
		default:
			return true
		}
	})

	out := make([]string, 0, len(rawTokens))
	for _, raw := range rawTokens {
		token := strings.Trim(raw, "#-_/.")
		if token == "" {
			continue
		}
		out = append(out, token)
	}
	return out
}

func narrationInvoiceTokenScore(token string, keywordContext bool) int {
	if !isPlausibleNarrationInvoiceToken(token) {
		return 0
	}

	score := 1
	if strings.ContainsAny(token, "/-_") {
		score += 2
	}
	if hasInvoicePrefix(token) {
		score += 2
	}
	if keywordContext {
		score += 2
	}
	if l := len(token); l >= 6 && l <= 24 {
		score++
	}
	return score
}

func tokenKeywordContext(tokens []string, idx int) bool {
	if idx > 0 {
		if _, ok := narrationInvoiceKeywords[tokens[idx-1]]; ok {
			return true
		}
	}
	if idx > 1 {
		if _, ok := narrationInvoiceKeywords[tokens[idx-2]]; ok {
			return true
		}
	}
	return false
}

func isPlausibleNarrationInvoiceToken(token string) bool {
	if len(token) < 5 || len(token) > 40 {
		return false
	}
	if _, blocked := narrationInvoiceStopTokens[token]; blocked {
		return false
	}

	hasLetter := false
	hasDigit := false
	for _, r := range token {
		switch {
		case unicode.IsLetter(r):
			hasLetter = true
		case unicode.IsDigit(r):
			hasDigit = true
		case r == '/', r == '-', r == '_':
			continue
		default:
			return false
		}
	}
	return hasLetter && hasDigit
}

func hasInvoicePrefix(token string) bool {
	prefixes := []string{"INV", "BILL", "REF", "DOC", "SUPP", "CN", "DN"}
	for _, prefix := range prefixes {
		if strings.HasPrefix(token, prefix) {
			return true
		}
	}
	return false
}

func sanitizeSyntheticIdentityPart(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	var b strings.Builder
	lastDash := false
	for _, r := range raw {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			b.WriteRune(r)
			lastDash = false
		case r == '-', r == '_', r == '/', r == '.':
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
