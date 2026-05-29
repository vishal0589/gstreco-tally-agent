// Package tally talks to Tally Prime's HTTP XML API. This file defines the
// agent-internal "Raw" domain model — a faithful mirror of a Tally voucher as
// it arrives over the wire. Normalisation into the server's IngestVoucherRow
// schema (tax aggregation, consolidated-voucher splitting, side inference)
// lives in the ingest layer (A4) so parsing stays a pure XML → struct step.
package tally

import "time"

// VoucherKind is the filtered slice of the day-book the agent is asking for.
// Tally exposes many voucher types (Journal, Payment, Receipt, Contra, …).
// GST Reco's invoice ingest uses purchase/sales/credit_note/debit_note, while
// journal/payment vouchers ship to their own accounting-completeness routes.
type VoucherKind string

const (
	VoucherPurchase   VoucherKind = "purchase"
	VoucherSales      VoucherKind = "sales"
	VoucherCreditNote VoucherKind = "credit_note"
	VoucherDebitNote  VoucherKind = "debit_note"
	VoucherJournal    VoucherKind = "journal"
	VoucherPayment    VoucherKind = "payment"
)

// RawVoucher is the unnormalised shape of a Tally voucher. Every field is
// optional — real Tally data is messy — so parsers must treat zero values as
// "absent" and the ingest layer decides which gaps are fatal. Field names and
// XML tags mirror Tally Prime 3.x's collection output.
type RawVoucher struct {
	// RawXML preserves the sanitized voucher XML subtree that produced this
	// voucher. It is intentionally separate from parsed fields so audit/debug
	// paths can inspect original Tally shape without flattening tag names.
	RawXML string
	// GUID is Tally's preferred stable voucher identifier when the export
	// includes it. Some real-world invoice vouchers omit GUID entirely, so
	// downstream logic must tolerate it being blank.
	GUID string
	// MasterID is Tally's internal transaction master identifier. Unlike
	// AlterID, it does not change on every edit, so it is the safest fallback
	// identity when invoice-facing fields are absent.
	MasterID string
	// AlterID increases every time the voucher is touched. Used as the
	// incremental sync cursor ($AlterID watermark, pain #13).
	AlterID int64
	// Date of the voucher in Tally's company timezone. Parsed from Tally's
	// various date formats via dateparse.go.
	Date time.Time
	// VoucherType is the user-defined voucher type name ("Purchase", "GST
	// Purchase", "Stock Journal", etc). The agent maps this to IngestKind via
	// Tally's built-in classification ($$IsSales / $$IsPurchase / …).
	VoucherType string
	// VoucherNumber is the human-readable number ("PI/001/26-27"). Not unique
	// across companies or fiscal years; when GUID is missing the server still
	// dedups on GSTIN + normalized invoice number + invoice date, so this is
	// one fallback identity signal, not a universal primary key.
	VoucherNumber string
	// Reference is the customer/supplier invoice number as entered in Tally
	// ("Reference / Ref. No." field). Often the number the counterparty sees.
	Reference string
	// PartyLedgerName is the primary party ledger for this voucher — typically
	// the customer (sales) or vendor (purchase). Tally stores this explicitly
	// on invoice-style vouchers; on journal/payment it may be empty.
	PartyLedgerName string
	// PartyGSTIN is captured from the party ledger at voucher-time (ledgers can
	// change later). Optional — pain point #2 (GSTIN missing on ledgers).
	PartyGSTIN string
	// PlaceOfSupply is the destination state for GST purposes. Tally ships
	// this as the state name ("Karnataka", "Maharashtra"); the ingest layer
	// maps it to the 2-digit state code used by recipient_gstin inference.
	PlaceOfSupply string
	// IsInvoice differentiates invoice-style vouchers (which carry taxable
	// values and GST) from cash/journal vouchers. Most ingestable vouchers set
	// IsInvoice=true, but some real Tally setups emit invoice-like purchase /
	// note vouchers through Accounting Voucher View with IsInvoice=false. The
	// normaliser therefore treats IsInvoice as a strong signal, not the sole
	// gate.
	IsInvoice bool
	// IsCancelled means Tally has marked the voucher cancelled. Agent still
	// forwards these so the server can amend prior batches (pain #9).
	IsCancelled bool
	// ReverseCharge is Tally's own `IsRcmApplicable` flag. When true, the
	// voucher is an RCM self-invoice; the ingest layer routes it to
	// IngestKind="rcm_self_invoice".
	ReverseCharge bool
	// Narration is the free-text memo the user typed into Tally. Surfaced in
	// the reco inspector as vendor-side context.
	Narration string
	// LedgerEntries are the debit/credit rows that make up the voucher. Tally
	// returns these under ALLLEDGERENTRIES.LIST (double-entry bookkeeping, one
	// entry per ledger touched). The normaliser sums GST-tagged ledgers to
	// derive igst/cgst/sgst/cess (pain #4).
	LedgerEntries []LedgerEntry
	// InventoryEntries are stock-item rows for inventory vouchers. Optional:
	// service-only purchases have none. Used for tax-rate reconciliation and
	// line-item drill-down in the reco inspector.
	InventoryEntries []InventoryEntry
	// UnknownChildren preserves direct voucher child nodes not modelled above,
	// including UDF nodes. Tally customisations are common; keeping the raw
	// child XML prevents parser upgrades from becoming lossy.
	UnknownChildren []UnknownXMLNode
}

// LedgerEntry is one debit/credit row inside a voucher. Tally uses sign to
// indicate direction: negative amount = credit, positive = debit for purchase-
// side, flipped for sales-side. The parser preserves Tally's sign as-is; the
// ingest layer applies the side inversion when it derives IngestVoucherRow.
type LedgerEntry struct {
	// LedgerName is the user-defined ledger name ("IGST 18%", "Freight",
	// "ABC Vendors Ltd"). The ingest layer uses pattern matching on the name
	// to classify tax ledgers.
	LedgerName string
	// Amount is signed. Tally sends it as a string like "-118000.00" — the
	// parser converts to float64 after stripping commas. Absolute magnitude is
	// always the rupee amount; sign encodes debit/credit.
	Amount float64
	// GSTClass is Tally's own tax classification tag ("IGST@18", "CGST@9").
	// Present on GST-tagged ledgers created via Tally's GST masters, absent on
	// legacy ledgers. When present, the normaliser skips pattern-matching.
	GSTClass string
	// IsPartyLedger is true when this row is the party ledger (matches
	// Voucher.PartyLedgerName). Convenience flag set by the parser to save
	// callers a string comparison.
	IsPartyLedger bool
	// BillAllocations is how Tally splits a single ledger line across multiple
	// bills ("consolidated voucher", pain #8). One ledger entry + N bill
	// allocations = N IngestVoucherRow rows, each with parent_ref carrying the
	// best voucher-level identity the agent has available.
	BillAllocations []BillRef
	// BankAllocations are payment/banking child rows under this exact ledger
	// entry. AMOUNT/DATE/NAME inside this list have bank-allocation meaning,
	// not voucher or bill meaning.
	BankAllocations []BankAllocation
	// RateDetails are GST rate child rows under this exact ledger entry.
	RateDetails []RateDetail
	// IsDeemedPositive preserves Tally's debit/credit sign hint beside Amount.
	IsDeemedPositive bool
	// IsDeemedPositiveSet distinguishes an omitted tag from an explicit No.
	IsDeemedPositiveSet bool
	UnknownChildren     []UnknownXMLNode
}

// BillRef is one bill allocation under a ledger entry. When multiple bills are
// tagged on one voucher, the agent emits one IngestVoucherRow per bill ref.
type BillRef struct {
	// Name is the bill reference string the user typed ("INV-042/26-27").
	// This becomes the IngestVoucherRow.invoice_number for split rows,
	// overriding the voucher's VoucherNumber.
	Name string
	// Amount is the portion of the ledger entry allocated to this bill. Signed
	// the same way as LedgerEntry.Amount.
	Amount float64
	// BillType is Tally's bill-allocation type ("New Ref", "Agst Ref", "On
	// Account", "Advance"). Informational; the normaliser keeps only "New Ref"
	// and "Agst Ref" since advances and on-account payments aren't invoices.
	BillType string
}

// InventoryEntry is one stock-item row inside an inventory voucher. Optional
// fields left zero when Tally omits them (service-only purchases).
type InventoryEntry struct {
	StockItem  string
	Quantity   float64
	Unit       string
	BilledQty  float64
	BilledUnit string
	Rate       float64
	Amount     float64
	// HSN from the stock-item master. Used as a fallback when the voucher
	// doesn't carry a line-level HSN.
	HSN                   string
	IsDeemedPositive      bool
	IsDeemedPositiveSet   bool
	BatchAllocations      []BatchAllocation
	AccountingAllocations []AccountingAllocation
	RateDetails           []RateDetail
	UnknownChildren       []UnknownXMLNode
}

// BankAllocation is one BANKALLOCATIONS.LIST row under a ledger entry. It is
// deliberately not merged with BillRef: DATE, NAME and AMOUNT here describe
// bank/payment metadata, not invoice references.
type BankAllocation struct {
	Name                  string
	Date                  time.Time
	InstrumentDate        time.Time
	TransactionType       string
	PaymentMode           string
	BankName              string
	BankPartyName         string
	PaymentFavouring      string
	InstrumentNumber      string
	UniqueReferenceNumber string
	Status                string
	Amount                float64
	UnknownChildren       []UnknownXMLNode
}

// BatchAllocation is one BATCHALLOCATIONS.LIST row under an inventory entry.
// AMOUNT/ACTUALQTY/BILLEDQTY here are batch-scoped, not voucher totals.
type BatchAllocation struct {
	BatchName             string
	GodownName            string
	DestinationGodownName string
	TrackingNumber        string
	OrderNumber           string
	ActualQty             float64
	ActualUnit            string
	BilledQty             float64
	BilledUnit            string
	Amount                float64
	UnknownChildren       []UnknownXMLNode
}

// AccountingAllocation is one ACCOUNTINGALLOCATIONS.LIST row under an
// inventory line. LEDGERNAME/AMOUNT here belong to the stock item allocation,
// not the voucher's top-level ledger rows.
type AccountingAllocation struct {
	LedgerName          string
	Amount              float64
	GSTClass            string
	IsDeemedPositive    bool
	IsDeemedPositiveSet bool
	BillAllocations     []BillRef
	BankAllocations     []BankAllocation
	RateDetails         []RateDetail
	UnknownChildren     []UnknownXMLNode
}

// RateDetail is one RATEDETAILS.LIST row under a ledger, inventory, or
// accounting allocation parent. Its parent path is what gives RATE meaning.
type RateDetail struct {
	DutyHead        string
	ValuationType   string
	Rate            float64
	RatePerUnit     float64
	UnknownChildren []UnknownXMLNode
}

// UnknownXMLNode preserves unmodelled direct child nodes with their parent
// path and sibling index so future parser work can recover meaning without
// going back to Tally.
type UnknownXMLNode struct {
	Path         string
	Name         string
	SiblingIndex int
	InnerXML     string
	Text         string
}
