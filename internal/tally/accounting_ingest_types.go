package tally

// AccountingBatchKind is the batch family POSTed to the accounting-completeness
// endpoints on the GST Reco server.
type AccountingBatchKind string

const (
	AccountingBatchJournal   AccountingBatchKind = "journal"
	AccountingBatchPayment   AccountingBatchKind = "payment"
	AccountingBatchTaxLedger AccountingBatchKind = "tax_ledger"
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
	IsFinal       bool    `json:"is_final,omitempty"`
	PeriodFrom    *string `json:"period_from,omitempty"`
	PeriodTo      *string `json:"period_to,omitempty"`
	RequestID     string  `json:"request_id,omitempty"`
}

type JournalIngestRequestBody struct {
	RunID         string              `json:"run_id,omitempty"`
	Kind          AccountingBatchKind `json:"kind"`
	TallyCompany  string              `json:"tally_company"`
	TallyEndpoint *string             `json:"tally_endpoint,omitempty"`
	IsFinal       bool                `json:"is_final,omitempty"`
	PeriodFrom    *string             `json:"period_from,omitempty"`
	PeriodTo      *string             `json:"period_to,omitempty"`
	RequestID     string              `json:"request_id,omitempty"`
	Batch         []JournalVoucherRow `json:"batch"`
}

type PaymentIngestRequestBody struct {
	RunID         string              `json:"run_id,omitempty"`
	Kind          AccountingBatchKind `json:"kind"`
	TallyCompany  string              `json:"tally_company"`
	TallyEndpoint *string             `json:"tally_endpoint,omitempty"`
	IsFinal       bool                `json:"is_final,omitempty"`
	PeriodFrom    *string             `json:"period_from,omitempty"`
	PeriodTo      *string             `json:"period_to,omitempty"`
	RequestID     string              `json:"request_id,omitempty"`
	Batch         []PaymentVoucherRow `json:"batch"`
}

type TaxLedgerIngestRequestBody struct {
	RunID         string                `json:"run_id,omitempty"`
	Kind          AccountingBatchKind   `json:"kind"`
	TallyCompany  string                `json:"tally_company"`
	TallyEndpoint *string               `json:"tally_endpoint,omitempty"`
	IsFinal       bool                  `json:"is_final,omitempty"`
	PeriodFrom    *string               `json:"period_from,omitempty"`
	PeriodTo      *string               `json:"period_to,omitempty"`
	RequestID     string                `json:"request_id,omitempty"`
	Batch         []TaxLedgerMonthlyRow `json:"batch"`
}

// AccountingAcceptedResponse is the 2xx response body from the journal/payment
// and tax-ledger endpoints.
type AccountingAcceptedResponse struct {
	OK        bool                `json:"ok"`
	Kind      AccountingBatchKind `json:"kind"`
	Processed int                 `json:"processed"`
	Errors    []string            `json:"errors,omitempty"`
}
