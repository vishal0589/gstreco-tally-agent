package tally

// AccountingBatchKind is the batch family POSTed to the accounting-completeness
// endpoints on the GST Reco server.
type AccountingBatchKind string

const (
	AccountingBatchJournal   AccountingBatchKind = "journal"
	AccountingBatchPayment   AccountingBatchKind = "payment"
	AccountingBatchTaxLedger AccountingBatchKind = "tax_ledger"
	// AccountingBatchTaxLedgerVoucher is the voucher evidence behind the
	// tax ledgers (QA 05-Nov, POST /api/tally/tax-ledger-vouchers).
	AccountingBatchTaxLedgerVoucher AccountingBatchKind = "tax_ledger_voucher"
)

// VoucherBillReference is one bill-wise reference linked to a journal/payment
// voucher. Mirrors GST_Reco's src/lib/tally/accounting-ingest-types.ts.
type VoucherBillReference struct {
	Reference string   `json:"reference"`
	Amount    *float64 `json:"amount,omitempty"`
	BillType  *string  `json:"bill_type,omitempty"`
	Direction *string  `json:"direction,omitempty"` // debit|credit
}

type JournalVoucherEntry struct {
	LedgerName    string  `json:"ledger_name"`
	Amount        float64 `json:"amount"`
	EntryType     string  `json:"entry_type,omitempty"` // debit|credit
	GSTClass      string  `json:"gst_class,omitempty"`
	IsPartyLedger *bool   `json:"is_party_ledger,omitempty"`
}

type JournalVoucherRow struct {
	TallyVoucherGUID   *string                `json:"tally_voucher_guid,omitempty"`
	VoucherNumber      string                 `json:"voucher_number"`
	VoucherDate        string                 `json:"voucher_date"` // YYYY-MM-DD
	VoucherTypeName    *string                `json:"voucher_type_name,omitempty"`
	Narration          *string                `json:"narration,omitempty"`
	TotalAmount        *float64               `json:"total_amount,omitempty"`
	PartyName          *string                `json:"party_name,omitempty"`
	PartyGSTIN         *string                `json:"party_gstin,omitempty"`
	Entries            []JournalVoucherEntry  `json:"entries"`
	AgainstInvoiceRefs []VoucherBillReference `json:"against_invoice_refs,omitempty"`
	IsRCM              *bool                  `json:"is_rcm,omitempty"`
	// IsOptional: Tally's ISOPTIONAL; omitted when not reported (QA 07-Nov D21).
	IsOptional *bool `json:"is_optional,omitempty"`
}

type PaymentVoucherEntry struct {
	LedgerName string  `json:"ledger_name"`
	Amount     float64 `json:"amount"`
	EntryType  string  `json:"entry_type,omitempty"` // debit|credit
}

type PaymentVoucherRow struct {
	TallyVoucherGUID   *string                `json:"tally_voucher_guid,omitempty"`
	VoucherNumber      string                 `json:"voucher_number"`
	VoucherDate        string                 `json:"voucher_date"` // YYYY-MM-DD
	VoucherTypeName    *string                `json:"voucher_type_name,omitempty"`
	Narration          *string                `json:"narration,omitempty"`
	PartyName          *string                `json:"party_name,omitempty"`
	PartyGSTIN         *string                `json:"party_gstin,omitempty"`
	InstrumentType     *string                `json:"instrument_type,omitempty"`
	TotalAmount        *float64               `json:"total_amount,omitempty"`
	Entries            []PaymentVoucherEntry  `json:"entries"`
	AgainstInvoiceRefs []VoucherBillReference `json:"against_invoice_refs,omitempty"`
	// IsOptional: Tally's ISOPTIONAL; omitted when not reported (QA 07-Nov D21).
	IsOptional *bool `json:"is_optional,omitempty"`
}

type TaxLedgerMonthlyRow struct {
	Period         string  `json:"period"` // MMYYYY
	LedgerName     string  `json:"ledger_name"`
	ParentGroup    *string `json:"parent_group,omitempty"`
	OpeningBalance float64 `json:"opening_balance"`
	DebitAmount    float64 `json:"debit_amount"`
	CreditAmount   float64 `json:"credit_amount"`
	ClosingBalance float64 `json:"closing_balance"`
}

type AccountingIngestRequestBase struct {
	RunID         string  `json:"run_id,omitempty"`
	TallyCompany  string  `json:"tally_company"`
	TallyEndpoint *string `json:"tally_endpoint,omitempty"`
	RequestID     string  `json:"request_id,omitempty"`
}

type JournalIngestRequestBody struct {
	RunID         string              `json:"run_id,omitempty"`
	Kind          AccountingBatchKind `json:"kind"`
	TallyCompany  string              `json:"tally_company"`
	TallyEndpoint *string             `json:"tally_endpoint,omitempty"`
	RequestID     string              `json:"request_id,omitempty"`
	Batch         []JournalVoucherRow `json:"batch"`
	SyncWarnings  []SyncWarning       `json:"sync_warnings,omitempty"`
	SyncRequestID string              `json:"sync_request_id,omitempty"`
	// QA 05-Nov D12: the run's kind and window, and is_final on its last
	// batch, so the server closes the run when the lane is done instead of
	// leaving it "running" until its idle sweep.
	RunKind    string  `json:"run_kind,omitempty"`
	PeriodFrom *string `json:"period_from,omitempty"`
	PeriodTo   *string `json:"period_to,omitempty"`
	IsFinal    bool    `json:"is_final,omitempty"`
}

type PaymentIngestRequestBody struct {
	RunID         string              `json:"run_id,omitempty"`
	Kind          AccountingBatchKind `json:"kind"`
	TallyCompany  string              `json:"tally_company"`
	TallyEndpoint *string             `json:"tally_endpoint,omitempty"`
	RequestID     string              `json:"request_id,omitempty"`
	Batch         []PaymentVoucherRow `json:"batch"`
	SyncWarnings  []SyncWarning       `json:"sync_warnings,omitempty"`
	SyncRequestID string              `json:"sync_request_id,omitempty"`
	// QA 05-Nov D12: the run's kind and window, and is_final on its last
	// batch, so the server closes the run when the lane is done instead of
	// leaving it "running" until its idle sweep.
	RunKind    string  `json:"run_kind,omitempty"`
	PeriodFrom *string `json:"period_from,omitempty"`
	PeriodTo   *string `json:"period_to,omitempty"`
	IsFinal    bool    `json:"is_final,omitempty"`
}

type TaxLedgerIngestRequestBody struct {
	RunID         string                `json:"run_id,omitempty"`
	Kind          AccountingBatchKind   `json:"kind"`
	TallyCompany  string                `json:"tally_company"`
	TallyEndpoint *string               `json:"tally_endpoint,omitempty"`
	RequestID     string                `json:"request_id,omitempty"`
	Batch         []TaxLedgerMonthlyRow `json:"batch"`
	SyncWarnings  []SyncWarning         `json:"sync_warnings,omitempty"`
	SyncRequestID string                `json:"sync_request_id,omitempty"`
	// QA 05-Nov D12: the run's kind and window, and is_final on its last
	// batch, so the server closes the run when the lane is done instead of
	// leaving it "running" until its idle sweep.
	RunKind    string  `json:"run_kind,omitempty"`
	PeriodFrom *string `json:"period_from,omitempty"`
	PeriodTo   *string `json:"period_to,omitempty"`
	IsFinal    bool    `json:"is_final,omitempty"`
}

// TaxLedgerVoucherLeg is one leg of a tax-ledger voucher as Tally returned
// it: ledger_name untrimmed, amount the trimmed AMOUNT text in Tally sign
// (debit negative), null when it is not a plain decimal.
type TaxLedgerVoucherLeg struct {
	LedgerName string  `json:"ledger_name"`
	Amount     *string `json:"amount"`
	GSTClass   *string `json:"gst_class"`
}

// TaxLedgerVoucherRow is one non-cancelled voucher inside the window with
// at least one leg on a tax ledger, sent with all its legs.
type TaxLedgerVoucherRow struct {
	TallyVoucherGUID *string               `json:"tally_voucher_guid"`
	MasterID         *string               `json:"master_id"`
	AlterID          *string               `json:"alter_id"`
	VoucherDate      *string               `json:"voucher_date"` // YYYY-MM-DD, null when unreadable
	VoucherDateRaw   string                `json:"voucher_date_raw"`
	VoucherTypeName  *string               `json:"voucher_type_name"`
	VoucherNumber    *string               `json:"voucher_number"`
	Reference        *string               `json:"reference"`
	PartyLedgerName  *string               `json:"party_ledger_name"`
	PartyGSTIN       *string               `json:"party_gstin"`
	IsInvoice        bool                  `json:"is_invoice"`
	IsOptional       bool                  `json:"is_optional"`
	IsRCMApplicable  bool                  `json:"is_rcm_applicable"`
	Legs             []TaxLedgerVoucherLeg `json:"legs"`
	Problems         []string              `json:"problems"`
}

// TaxLedgerVoucherSnapshot rides on the final batch of a month: whether the
// batches are the complete set of the month's tax-ledger vouchers.
type TaxLedgerVoucherSnapshot struct {
	Complete bool `json:"complete"`
	// VouchersReturned is every VOUCHER Tally returned for the window
	// (any type, cancelled and outside-window ones included).
	VouchersReturned int `json:"vouchers_returned"`
	// VouchersSent is the vouchers the run sends across all its batches:
	// the rows (touching the set, not cancelled, not outside the window;
	// problem and unreadable-date vouchers included).
	VouchersSent      int `json:"vouchers_sent"`
	OutsideWindow     int `json:"outside_window"`
	CancelledTouching int `json:"cancelled_touching"`
	DateUnreadable    int `json:"date_unreadable"`
}

type TaxLedgerVoucherIngestRequestBody struct {
	RunID         string                `json:"run_id,omitempty"`
	Kind          AccountingBatchKind   `json:"kind"`
	TallyCompany  string                `json:"tally_company"`
	TallyEndpoint *string               `json:"tally_endpoint,omitempty"`
	CompanyGUID   *string               `json:"company_guid,omitempty"`
	RequestID     string                `json:"request_id,omitempty"`
	Batch         []TaxLedgerVoucherRow `json:"batch"`
	SyncWarnings  []SyncWarning         `json:"sync_warnings,omitempty"`
	SyncRequestID string                `json:"sync_request_id,omitempty"`
	RunKind       string                `json:"run_kind,omitempty"`
	PeriodFrom    *string               `json:"period_from,omitempty"`
	PeriodTo      *string               `json:"period_to,omitempty"`
	IsFinal       bool                  `json:"is_final,omitempty"`
	// Snapshot is set on the final batch only.
	Snapshot *TaxLedgerVoucherSnapshot `json:"snapshot,omitempty"`
}

// AccountingAcceptedResponse is the 2xx response body from the journal/payment
// and tax-ledger endpoints.
type AccountingAcceptedResponse struct {
	OK        bool                `json:"ok"`
	Kind      AccountingBatchKind `json:"kind"`
	Processed int                 `json:"processed"`
	Errors    []string            `json:"errors,omitempty"`
}
