// Package syncrun is the shared fetch → parse → normalize → batch →
// ingest pipeline. Both `agentctl sync` (one-shot, single mapping)
// and `agentctl sync-all` (walks every active mapping) drive this
// runner; A5's full cron daemon will be a third caller.
//
// Keeping the pipeline in one place means parser bugs, normaliser
// changes, and HMAC quirks land in one tested module instead of
// drifting across CLI commands.
package syncrun

import (
	"context"
	"fmt"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// TallyPoster is the narrow surface RunOne uses to talk to Tally.
// Real callers pass a *tally.Client; tests pass fakes.
type TallyPoster interface {
	PostXML(ctx context.Context, body []byte) ([]byte, error)
}

// IngestSender is the narrow surface RunOne uses to talk to GST Reco.
// Real callers pass a *ingest.Client; tests pass fakes.
type IngestSender interface {
	Send(ctx context.Context, body tally.IngestRequestBody) (ingest.AcceptedResponse, error)
	SendJournal(ctx context.Context, body tally.JournalIngestRequestBody) (tally.AccountingAcceptedResponse, error)
	SendPayment(ctx context.Context, body tally.PaymentIngestRequestBody) (tally.AccountingAcceptedResponse, error)
	SendTaxLedgers(ctx context.Context, body tally.TaxLedgerIngestRequestBody) (tally.AccountingAcceptedResponse, error)
}

// Request describes one fetch-and-ingest cycle: a single (Tally
// company, voucher kind, date window) tuple.
type Request struct {
	// KindName is the user-facing kind label ("purchase", "journal",
	// "tax_ledger"). RunOne dispatches on this so invoice, accounting-voucher,
	// and ledger-control syncs can share one orchestration surface.
	KindName string
	// TallyCompany is the SVCURRENTCOMPANY value the envelope uses.
	// Whitespace matters — pass exactly what Tally exports.
	TallyCompany string
	// TallyEndpoint is the exact local Tally HTTP URL this run used.
	// The server can use it to disambiguate duplicate company names
	// discovered on different ports.
	TallyEndpoint string
	// TallyKind picks the Tally envelope filter (purchase / sales /
	// credit_note / debit_note).
	TallyKind tally.VoucherKind
	// IngestKind tells the server which table the rows belong in.
	// Usually the same as TallyKind in spirit but uses the ingest
	// enum (which has rcm_self_invoice as a separate value).
	IngestKind tally.IngestKind
	// From and To are the inclusive date window passed to
	// SVFROMDATE / SVTODATE.
	From, To time.Time
	// RunID identifies this run on the server. Reused as the
	// request_id_prefix so the per-batch request_id has the form
	// "<run_id>-<n>".
	RunID string
	// RunKind is "full" | "incremental" | "manual" — passed through
	// to the IngestRequestBody.
	RunKind string
	// BatchSize caps rows per ingest request. Zero = ingest.DefaultBatchSize.
	BatchSize int
}

// Result reports what happened. Always populated even when err is
// non-nil — the caller can show partial counts on failure.
type Result struct {
	// BatchesSent is the count of POST /ingest calls that returned 2xx.
	BatchesSent int
	// BatchesFailed counts non-2xx ingest responses (auth, payload,
	// 5xx, network). Each failure is in BatchErrors with detail.
	BatchesFailed int
	// BatchErrors carries per-batch failure detail so the caller can
	// surface every failure in one CLI invocation rather than one at
	// a time.
	BatchErrors []BatchError
	// RowCount is the total IngestVoucherRow count after normalize +
	// consolidated-voucher explosion. Sum of len(batch) across all
	// sent + failed batches.
	RowCount int
	// DroppedOnNormalize counts vouchers the normaliser couldn't
	// process (no party ledger, malformed data). Surfaced for the
	// operator's attention but not fatal — sync continues.
	DroppedOnNormalize int
	// ParseStatus is Tally's <STATUS> value (1=success). Surfaced so
	// the caller can spot sync runs against a Tally that returned a
	// real error envelope.
	ParseStatus int
	// ParseWarnings collects per-voucher warnings from the parser
	// (unparsable date, malformed amount, dropped voucher). Same
	// purpose as DroppedOnNormalize at parser-level.
	ParseWarnings []string
	// ServerInserted / ServerAmended / ServerSkipped / ServerErrors are
	// the counters returned by the app on 2xx ingest responses. They are
	// authoritative for what the server says actually landed.
	ServerInserted int
	ServerAmended  int
	ServerSkipped  int
	ServerErrors   int
	// ServerFindings are the flattened validation findings returned by
	// the app on 2xx ingest responses (missing GSTIN, rejected purchase
	// row, rejected sales row, etc).
	ServerFindings []ingest.ValidationFinding
}

// BatchError carries detail for one failed ingest batch. Index is
// 1-based (matches the user-facing log line "batch 3/10").
type BatchError struct {
	Index     int
	Total     int
	RequestID string
	Err       error
}

// Progress is an optional callback the caller can use to render live
// updates as the run progresses. Useful for `agentctl sync` which
// prints per-batch lines as it goes; sync-all uses a quieter
// callback to keep the per-mapping output compact.
type Progress func(event Event)

// Event is one progress notification from RunOne.
type Event struct {
	// Stage is one of: "fetching", "parsed", "normalized",
	// "batch_sending", "batch_sent", "batch_failed", "complete".
	Stage string
	// Message is a human-readable line for the operator.
	Message string
	// BatchIndex / BatchTotal are populated for batch_* stages.
	BatchIndex int
	BatchTotal int
	// Err is set only on batch_failed.
	Err error
}

// RunOne is the pipeline. Returns the last fatal error that stopped
// the run (envelope build, parse, tally fetch). Per-batch ingest
// failures are captured in Result.BatchErrors and do NOT stop the
// run — the caller decides what to do with partial success.
//
// The progress callback is optional (pass nil to suppress).
func RunOne(
	ctx context.Context,
	tallyClient TallyPoster,
	sender IngestSender,
	req Request,
	progress Progress,
) (Result, error) {
	var res Result
	if req.BatchSize <= 0 {
		req.BatchSize = ingest.DefaultBatchSize
	}
	if req.RunID == "" {
		return res, fmt.Errorf("syncrun: Request.RunID is required")
	}
	if req.KindName == "" {
		switch {
		case req.IngestKind != "":
			req.KindName = string(req.IngestKind)
		case req.TallyKind != "":
			req.KindName = string(req.TallyKind)
		default:
			return res, fmt.Errorf("syncrun: Request.KindName is required")
		}
	}

	emit := func(stage, msg string) {
		if progress != nil {
			progress(Event{Stage: stage, Message: msg})
		}
	}
	emitBatch := func(stage string, idx, total int, msg string, err error) {
		if progress != nil {
			progress(Event{
				Stage:      stage,
				Message:    msg,
				BatchIndex: idx,
				BatchTotal: total,
				Err:        err,
			})
		}
	}

	switch req.KindName {
	case "journal":
		return runJournalSync(ctx, tallyClient, sender, req, res, emit, emitBatch)
	case "payment":
		return runPaymentSync(ctx, tallyClient, sender, req, res, emit, emitBatch)
	case "tax_ledger":
		return runTaxLedgerSync(ctx, tallyClient, sender, req, res, emit, emitBatch)
	default:
		return runInvoiceSync(ctx, tallyClient, sender, req, res, emit, emitBatch)
	}
}
