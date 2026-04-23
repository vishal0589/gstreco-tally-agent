package tally

import "strings"

// MasterKind tags a ledger as a vendor (Sundry Creditors) or customer (Sundry
// Debtors) for the server's `/api/tally/masters` endpoint. Other ledger
// groups (Bank, Cash, Expenses, …) are ignored — the server only stores
// party-master data.
type MasterKind string

const (
	MasterVendor   MasterKind = "vendor"
	MasterCustomer MasterKind = "customer"
)

// RawMaster is one party ledger as Tally returns it. All fields are captured
// faithfully at parse time; normalisation (trim, uppercase GSTIN, derive
// 2-digit state code from state name) happens in a separate step so the
// parser stays side-effect-free.
type RawMaster struct {
	// Name is the ledger's internal Tally name — what Tally uses to match
	// voucher party-ledger references. Unique within a Tally company.
	Name string
	// Parent is the ledger group name ("Sundry Creditors", "Sundry Debtors",
	// "Bank Accounts", …). The agent uses this to derive MasterKind.
	Parent string
	// GUID is Tally's stable identifier; same across edits to name/address.
	GUID string
	// AlterID advances when the ledger is edited. Used as the masters cursor
	// so incremental syncs don't re-ship unchanged rows.
	AlterID int64
	// TradeName is the user-visible business name (MAILINGNAME[0] in the XML).
	// Falls back to Name when MAILINGNAME.LIST is absent.
	TradeName string
	// LegalName is the legal entity name on record. Optional — not every
	// ledger has one, especially for long-standing Tally installs.
	LegalName string
	// GSTIN may be missing on older ledgers (pain #2). Agent still forwards
	// the master with GSTIN="" so the server's gstin-missing inbox can pick
	// it up.
	GSTIN string
	// GSTRegistrationType is "Regular", "Composition", "Consumer", "Unregistered"
	// (or blank on legacy ledgers). Server uses it to short-circuit invalid-
	// GSTIN warnings for legitimately-unregistered vendors.
	GSTRegistrationType string
	// StateName is what Tally exports ("Karnataka", "Maharashtra"). The agent
	// maps it to the 2-digit state code via state_codes.go before shipping.
	StateName string
	// Email / Phone / Mobile carry contact details, used by the server's
	// follow-up workflows (ITC mismatch notifications).
	Email  string
	Phone  string
	Mobile string
	// Contact is the free-text contact person name, if the user set one.
	Contact string
	// Address is the full multi-line address concatenated with newlines; the
	// server stores it as a single column and renders it as-is.
	Address string
}

// IsKindedAs returns true when the ledger's Parent group matches the given
// MasterKind. "Sundry Creditors" → vendor; "Sundry Debtors" → customer.
// Both checks are case-insensitive to handle installs where the user
// capitalised or renamed the default groups.
func (m RawMaster) IsKindedAs(k MasterKind) bool {
	switch k {
	case MasterVendor:
		return matchesGroup(m.Parent, "SUNDRY CREDITORS")
	case MasterCustomer:
		return matchesGroup(m.Parent, "SUNDRY DEBTORS")
	}
	return false
}

// matchesGroup is a case-insensitive equality check with whitespace tolerance,
// used by IsKindedAs. Handles installs where the user capitalised the default
// group name ("sundry creditors" vs "Sundry Creditors") or added padding.
func matchesGroup(a, want string) bool {
	return strings.EqualFold(strings.TrimSpace(a), want)
}

// TallyCompany is one entry from Tally's list-of-loaded-companies response.
// Used by the /tally-companies/sync route so the user's mapping UI sees
// every company the agent can see — including misspelled variants and
// obsolete copies the user forgot to unload.
type TallyCompany struct {
	// Name is the exact display name (whitespace preserved — Tally is
	// sensitive to it; passing "Acme Corp" when Tally knows the company
	// as "Acme Corp " returns no vouchers).
	Name string
	// GUID is Tally's internal company id. Optional — not every export
	// includes it, but useful for the server's mapping dedup when a user
	// renames a company.
	GUID string
}

// Version is the parsed major version of Tally Prime the agent is talking
// to. parser_v3 handles Prime 3.x; parser_v4 (A7) handles 4.x. The agent
// detects at startup and picks the right parser per-company.
type Version int

const (
	VersionUnknown Version = 0
	VersionV3      Version = 3
	VersionV4      Version = 4
)
