package tally

import "strings"

// NormalizeMasters converts a slice of RawMaster (parser-side) into the
// IngestMasterItem shape the server's /api/tally/masters endpoint expects.
// Entries without a non-empty GSTIN are silently dropped — the server
// rejects them anyway, and the agent has no useful way to surface
// "this Tally ledger has no GSTIN" beyond the next-tick refresh.
//
// The kind argument filters by ledger group (Sundry Creditors → vendor,
// Sundry Debtors → customer). Pass MasterVendor / MasterCustomer.
func NormalizeMasters(raw []RawMaster, kind MasterKind) []IngestMasterItem {
	out := make([]IngestMasterItem, 0, len(raw))
	for _, m := range raw {
		if !m.IsKindedAs(kind) {
			continue
		}
		gstin := strings.ToUpper(strings.TrimSpace(m.GSTIN))
		if gstin == "" {
			continue
		}
		item := IngestMasterItem{GSTIN: gstin}
		if v := strings.TrimSpace(m.TradeName); v != "" {
			item.TradeName = &v
		}
		if v := strings.TrimSpace(m.LegalName); v != "" {
			item.LegalName = &v
		}
		if code := StateCode(m.StateName); code != "" {
			item.StateCode = &code
		}
		if v := strings.TrimSpace(m.Email); v != "" {
			item.Email = &v
		}
		// Prefer mobile over landline if both are set — that's what
		// operators actually pick up when an ITC mismatch needs a call.
		phone := strings.TrimSpace(m.Mobile)
		if phone == "" {
			phone = strings.TrimSpace(m.Phone)
		}
		if phone != "" {
			item.Phone = &phone
		}
		out = append(out, item)
	}
	return out
}
