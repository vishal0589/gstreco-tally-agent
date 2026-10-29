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
	TallyVoucherGUID *string  `json:"tally_voucher_guid,omitempty"`
	VoucherNumber    *string  `json:"voucher_number,omitempty"`
	VoucherDate      *string  `json:"voucher_date,omitempty"` // YYYY-MM-DD
	VoucherTypeName  *string  `json:"voucher_type_name,omitempty"`
	VoucherReference *string  `json:"voucher_reference,omitempty"`
	VendorGSTIN      *string  `json:"vendor_gstin,omitempty"`
	VendorName       *string  `json:"vendor_name,omitempty"`
	CustomerGSTIN    *string  `json:"customer_gstin,omitempty"`
	CustomerName     *string  `json:"customer_name,omitempty"`
	RecipientGSTIN   *string  `json:"recipient_gstin,omitempty"`
	InvoiceNumber    string   `json:"invoice_number"`
	InvoiceDate      string   `json:"invoice_date"` // YYYY-MM-DD
	InvoiceValue     float64  `json:"invoice_value"`
	TaxableValue     float64  `json:"taxable_value"`
	IGST             float64  `json:"igst"`
	CGST             float64  `json:"cgst"`
	SGST             float64  `json:"sgst"`
	CESS             float64  `json:"cess"`
	TaxRate          *float64 `json:"tax_rate,omitempty"`
	PlaceOfSupply    *string  `json:"place_of_supply,omitempty"`
	ReverseCharge    *bool    `json:"reverse_charge,omitempty"`
	Narration        *string  `json:"narration,omitempty"`
	// ParentRef is the best voucher-level reference the agent has when this
	// row is one slice of a consolidated voucher (pain #8). GUID is preferred,
	// but Tally sometimes omits it on invoice vouchers so the agent falls back
	// to voucher/invoice-facing references instead. The server persists
	// parent_ref as-is; the inline-unique index on (company_id, gstin,
	// invoice_number_normalized, invoice_date) keeps each bill ref from
	// landing twice.
	ParentRef *string `json:"parent_ref,omitempty"`
	// Side pins the note to purchase or sales books when Tally's voucher
	// doesn't make the direction obvious. For purchase/sales kinds the
	// server ignores the field; for credit_note/debit_note the agent sets
	// it from the party ledger's group in Tally.
	Side *IngestSide `json:"side,omitempty"`
	// HSN/SAC code from the voucher's first inventory entry. Purchase-side
	// only (sales_invoices has no hsn column). Drives the server's v0.1.11
	// capital-goods classifier (chapters 84/85/90) and the Sec 17(5)
	// HSN-based blocked-credit detector. Vouchers without inventory entries
	// (service-only purchases) leave it nil; the server-side classifier
	// defaults is_capital_good=false there, which is correct since services
	// are never capital goods.
	HSN *string `json:"hsn,omitempty"`

	// QA v18 R3 source fields. All optional: servers that predate them ignore
	// unknown keys. Names follow GST Reco's ingest contract (QA 28 Oct P1-C).
	//
	// GrossAmount is the invoice gross the signed legs establish (equals
	// InvoiceValue); never the net payable.
	GrossAmount *float64 `json:"gross_amount,omitempty"`
	// TDSAmount is TDS withheld from the party; TCSAmount is TCS the party
	// collected. Neither is GST nor taxable value.
	TDSAmount *float64 `json:"tds_amount,omitempty"`
	TCSAmount *float64 `json:"tcs_amount,omitempty"`
	// NetPayable is the party ledger amount: gross less TDS, plus TCS.
	NetPayable *float64 `json:"net_payable,omitempty"`
	// VoucherLegs are the voucher's ledger legs as Tally sent them, with the
	// role each one got. Every split row of a consolidated voucher carries
	// the whole voucher's legs.
	VoucherLegs []IngestVoucherLeg `json:"voucher_legs,omitempty"`
	// NormalizationFlags explain each leg kept out of GST or taxable value
	// and each gap in the evidence.
	NormalizationFlags []string `json:"normalization_flags,omitempty"`
}

// IngestVoucherLeg is one ledger leg of an invoice voucher (GST Reco's
// TallyVoucherLeg).
type IngestVoucherLeg struct {
	LedgerName string `json:"ledger_name"`
	// ParentGroup / PrimaryGroup are the ledger's Tally group and the
	// reserved group its chain ends in, when the agent knows them.
	ParentGroup  *string `json:"parent_group,omitempty"`
	PrimaryGroup *string `json:"primary_group,omitempty"`
	// Amount is Tally's signed AMOUNT, unchanged.
	Amount        float64 `json:"amount"`
	IsPartyLedger bool    `json:"is_party_ledger,omitempty"`
	GSTClass      *string `json:"gst_class,omitempty"`
	// TaxType / GSTDutyHead come from the Duties & Taxes ledger master.
	TaxType        *string                 `json:"tax_type,omitempty"`
	GSTDutyHead    *string                 `json:"gst_duty_head,omitempty"`
	Classification IngestLegClassification `json:"classification"`
}

// IngestLegClassification is the agent's decision on one leg. Role, Method
// and Head use GST Reco's words (party, tax, tds, tcs, base, round_off,
// unclassified; group, duty_field, name_fallback, unclassified; igst, cgst,
// sgst, cess) so the server compares like with like. AgentRole and Evidence
// are the agent's own finer role and what decided it.
type IngestLegClassification struct {
	Role      string      `json:"role"`
	Method    string      `json:"method"`
	Head      *string     `json:"head,omitempty"`
	AgentRole LegRole     `json:"agent_role"`
	Evidence  LegEvidence `json:"evidence"`
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
	RunID         string             `json:"run_id"`
	Kind          IngestKind         `json:"kind"`
	TallyCompany  string             `json:"tally_company"`
	TallyEndpoint *string            `json:"tally_endpoint,omitempty"`
	Batch         []IngestVoucherRow `json:"batch"`
	CursorBefore  any                `json:"cursor_before,omitempty"`
	CursorAfter   any                `json:"cursor_after,omitempty"`
	IsFinal       bool               `json:"is_final,omitempty"`
	// PeriodFrom/PeriodTo are YYYY-MM-DD bounds for the Tally window that
	// produced this batch. The app stores them on tally_sync_runs so support
	// and operators can prove which month a Sync now actually fetched.
	PeriodFrom *string `json:"period_from,omitempty"`
	PeriodTo   *string `json:"period_to,omitempty"`
	// RunKind is "full" | "incremental" | "manual". The server stamps it on
	// the first batch of a run and ignores it on subsequent batches.
	RunKind   string `json:"run_kind,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}
