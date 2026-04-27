package tally

// Server-side ingest types mirrored from GST_Reco's src/lib/tally/
// ingest-types.ts. Keep struct tags and field types in lock-step with the
// TypeScript source — the two files have no codegen and drift is caught only
// by targeted unit tests on both sides.

// IngestKind is the voucher category the agent is shipping in this batch.
// Mirrors the server's IngestKind union.
type IngestKind string

const (
	IngestKindPurchase       IngestKind = "purchase"
	IngestKindSales          IngestKind = "sales"
	IngestKindCreditNote     IngestKind = "credit_note"
	IngestKindDebitNote      IngestKind = "debit_note"
	IngestKindRCMSelfInvoice IngestKind = "rcm_self_invoice"
)

// IngestSide tells the server which books this credit/debit note belongs to.
// Only meaningful for note kinds; purchase/sales infer from kind alone.
type IngestSide string

const (
	IngestSidePurchase IngestSide = "purchase"
	IngestSideSales    IngestSide = "sales"
)

// IngestVoucherRow is one voucher as seen by the server. Numeric fields are
// typed here (the server trusts the agent's coercion), so the agent MUST
// parse Tally's string-first XML into real numbers before submitting.
type IngestVoucherRow struct {
	TallyVoucherGUID *string `json:"tally_voucher_guid,omitempty"`
	VendorGSTIN      *string `json:"vendor_gstin,omitempty"`
	VendorName       *string `json:"vendor_name,omitempty"`
	CustomerGSTIN    *string `json:"customer_gstin,omitempty"`
	CustomerName     *string `json:"customer_name,omitempty"`
	RecipientGSTIN   *string `json:"recipient_gstin,omitempty"`
	InvoiceNumber    string  `json:"invoice_number"`
	InvoiceDate      string  `json:"invoice_date"` // YYYY-MM-DD
	InvoiceValue     float64 `json:"invoice_value"`
	TaxableValue     float64 `json:"taxable_value"`
	IGST             float64 `json:"igst"`
	CGST             float64 `json:"cgst"`
	SGST             float64 `json:"sgst"`
	CESS             float64 `json:"cess"`
	TaxRate          *float64 `json:"tax_rate,omitempty"`
	PlaceOfSupply    *string  `json:"place_of_supply,omitempty"`
	ReverseCharge    *bool    `json:"reverse_charge,omitempty"`
	// ParentRef is the Tally VoucherGuid when this row is one slice of a
	// consolidated voucher (pain #8). The server persists parent_ref as-is;
	// the inline-unique index on (company_id, gstin, invoice_number_normalized,
	// invoice_date) keeps each bill ref from landing twice.
	ParentRef *string `json:"parent_ref,omitempty"`
	// Side pins the note to purchase or sales books when Tally's voucher
	// doesn't make the direction obvious. For purchase/sales kinds the
	// server ignores the field; for credit_note/debit_note the agent sets
	// it from the party ledger's group in Tally.
	Side *IngestSide `json:"side,omitempty"`
}

// IngestMasterItem is one party-master row sent to /api/tally/masters.
// Mirrors the server's MasterItem in src/lib/tally/masters-upsert.ts.
// All optional fields use *string so the JSON omits them when empty —
// the server treats null and missing identically and the wire payload
// stays compact for chatty masters refreshes.
type IngestMasterItem struct {
	GSTIN     string  `json:"gstin"`
	TradeName *string `json:"trade_name,omitempty"`
	LegalName *string `json:"legal_name,omitempty"`
	StateCode *string `json:"state_code,omitempty"`
	Email     *string `json:"email,omitempty"`
	Phone     *string `json:"phone,omitempty"`
}

// IngestMastersRequest is the body POSTed to /api/tally/masters.
// run_id is optional (masters are configuration not invoice data, so
// no tally_sync_runs row is created from this endpoint).
type IngestMastersRequest struct {
	RunID     string             `json:"run_id,omitempty"`
	Kind      MasterKind         `json:"kind"` // vendor|customer
	Items     []IngestMasterItem `json:"items"`
	RequestID string             `json:"request_id,omitempty"`
}

// IngestRequestBody is the full request shape. One run_id can span many
// batches (each is_final=false), with the last batch carrying is_final=true.
type IngestRequestBody struct {
	RunID        string             `json:"run_id"`
	Kind         IngestKind         `json:"kind"`
	TallyCompany string             `json:"tally_company"`
	Batch        []IngestVoucherRow `json:"batch"`
	CursorBefore any                `json:"cursor_before,omitempty"`
	CursorAfter  any                `json:"cursor_after,omitempty"`
	IsFinal      bool               `json:"is_final,omitempty"`
	// RunKind is "full" | "incremental" | "manual". The server stamps it on
	// the first batch of a run and ignores it on subsequent batches.
	RunKind   string `json:"run_kind,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}
