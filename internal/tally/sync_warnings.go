package tally

// SyncWarning is one entry of an ingest batch's optional top-level
// "sync_warnings" field (QA v21 G-A): a masters fetch or parse failure that
// changed how the run's rows were classified. The server stores it on the
// batch's tally_sync_runs row and shows it in sync health; it never fails a
// sync because of it. Older servers ignore the field.
type SyncWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// TallyCompany defaults to the batch's tally_company on the server.
	TallyCompany string `json:"tally_company,omitempty"`
	// OccurredAt is RFC 3339; the server uses receipt time when absent.
	OccurredAt string `json:"occurred_at,omitempty"`
	// AffectedVoucherGUIDs are the vouchers whose rows were classified
	// without the missing masters.
	AffectedVoucherGUIDs []string `json:"affected_voucher_guids,omitempty"`
}

// Sync warning codes.
const (
	SyncWarningDutyLedgerMastersUnavailable = "duty_ledger_masters_unavailable"
	SyncWarningMastersFetchFailed           = "masters_fetch_failed"
	SyncWarningMastersParseFailed           = "masters_parse_failed"
)

// Server-side caps on sync_warnings; the agent applies them before sending.
const (
	MaxSyncWarnings            = 50
	MaxSyncWarningMessageRunes = 500
	MaxSyncWarningVoucherGUIDs = 500
)
