package syncrun

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// KindPair pairs a user-facing kind name with its Tally envelope filter
// and the ingest enum the server expects. Resolved once and passed
// down to WalkAll so the per-mapping inner loop doesn't re-parse the
// kind string on every iteration.
type KindPair struct {
	Name   string
	Tally  tally.VoucherKind
	Ingest tally.IngestKind
}

// DefaultKinds is the full set the daemon and `agentctl sync-all`
// cycle through on each run. The order matches how an operator
// investigates GST completeness: invoice books first, then journal /
// payment evidence, then the month-end Duties & Taxes control ledgers.
var DefaultKinds = []KindPair{
	{Name: "purchase", Tally: tally.VoucherPurchase, Ingest: tally.IngestKindPurchase},
	{Name: "sales", Tally: tally.VoucherSales, Ingest: tally.IngestKindSales},
	{Name: "credit_note", Tally: tally.VoucherCreditNote, Ingest: tally.IngestKindCreditNote},
	{Name: "debit_note", Tally: tally.VoucherDebitNote, Ingest: tally.IngestKindDebitNote},
	{Name: "journal", Tally: tally.VoucherJournal},
	{Name: "payment", Tally: tally.VoucherPayment},
	{Name: "tax_ledger"},
}

// WalkOptions configures one WalkAll invocation. Callers pre-resolve
// dependencies (TallyClient factory, IngestSender, mapping list) so
// WalkAll itself stays pure orchestration — easy to test, easy to
// share between the CLI (`agentctl sync-all`) and the daemon
// (`cmd/agent`).
type WalkOptions struct {
	// Mappings is the list to walk. Caller fetches via
	// ingest.Client.FetchActiveMappings (or builds in tests).
	Mappings []ingest.ActiveMapping
	// Kinds is the inner-loop set per mapping. Empty = DefaultKinds.
	Kinds []KindPair
	// From, To define the inclusive date window. Daemon picks current
	// month; CLI takes from --period or --from/--to.
	From, To time.Time
	// BatchSize caps rows per ingest request. Zero = ingest default.
	BatchSize int
	// RunIDPrefix is prepended to every per-(mapping, kind) RunID.
	// Daemon uses "daemon-<unix-tick>"; CLI uses "syncall-<unix>".
	RunIDPrefix string
	// RunKind is "manual" | "incremental" | "full". Server stamps it
	// on the first batch of a run.
	RunKind string
	// PerRunTimeout caps each (mapping, kind) RunOne call. Zero = 10
	// minutes (matches CLI default).
	PerRunTimeout time.Duration
	// ContinueOnError = true (default in CLI) keeps walking after a
	// per-mapping fatal error. False stops at the first failure.
	ContinueOnError bool
	// NewTallyClient builds a tally client per endpoint. Real callers
	// pass a factory wrapping tally.NewClient; tests pass a fake.
	NewTallyClient func(endpoint string) TallyPoster
	// Sender is the ingest sender (one client, reused across mappings
	// — saves keyring reads + connection pooling).
	Sender IngestSender
	// PerRunProgress, when non-nil, is invoked once per
	// (mapping, kind) run with a fresh syncrun.Progress callback the
	// caller can use to render per-run progress lines. The CLI uses
	// this to prefix output with the kind name; the daemon uses it
	// to log to zerolog at info level.
	PerRunProgress func(mapping ingest.ActiveMapping, kind KindPair) Progress
	// PerMappingHook fires once before each mapping's kind loop.
	// Useful for the CLI to print a "[1/6] PLLUM CASA @ ..." header.
	// Optional.
	PerMappingHook func(idx, total int, mapping ingest.ActiveMapping)
	// PerKindResultHook fires after each (mapping, kind) RunOne with
	// the result + any fatal error. Useful for the CLI to print a
	// per-kind summary line. Optional.
	PerKindResultHook func(mapping ingest.ActiveMapping, kind KindPair, res Result, fatalErr error)
}

// WalkResult aggregates counts across the whole walk. Per-mapping
// detail is delivered via the hooks; the daemon doesn't need it once
// the walk is over.
type WalkResult struct {
	MappingsRun        int
	TotalRows          int
	TotalBatchesSent   int
	TotalBatchesFailed int
	TotalServerSkipped int
	TotalServerErrors  int
	FatalErrors        int
	// StoppedEarly is true when ContinueOnError=false and a fatal
	// error short-circuited the walk.
	StoppedEarly bool
}

// WalkAll iterates Mappings × Kinds, calling RunOne for each pair
// against a per-endpoint Tally client. Sequential by design — multi-
// port hosts run independent Tally processes per port so concurrency
// would help, but adding parallelism without a rate-limiter for the
// GST Reco server is a footgun. Address that in the daemon's
// rate-limit stanza, not here.
//
// Returns aggregate counts. The caller decides what to do with
// non-zero FatalErrors / TotalBatchesFailed (CLI exits 1, daemon logs
// + continues).
func WalkAll(ctx context.Context, opts WalkOptions) WalkResult {
	var res WalkResult
	if len(opts.Mappings) == 0 {
		return res
	}
	kinds := opts.Kinds
	if len(kinds) == 0 {
		kinds = DefaultKinds
	}
	timeout := opts.PerRunTimeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}

	for mIdx, m := range opts.Mappings {
		if opts.PerMappingHook != nil {
			opts.PerMappingHook(mIdx, len(opts.Mappings), m)
		}
		tallyClient := opts.NewTallyClient(m.TallyEndpoint)
		mappingRan := false

		for _, kind := range kinds {
			// run_id MUST be a UUID — server's tally_sync_runs.id and
			// tally_ingest_log.run_id are uuid-typed columns. Pre-v0.1.8
			// we built a human-readable string ("daemon-1777274014-
			// purchase-1") which silently failed the upsert (server
			// logged the error but returned 200, so the agent thought
			// success). Net effect: vouchers landed in
			// purchase_invoices/sales_invoices via the import_history
			// path, but tally_sync_runs stayed empty so the operator
			// dashboard showed "no runs" despite real data flowing.
			// Caught during PLLUM pilot 2026-04-27: 86 vouchers in DB,
			// zero runs in UI. The RunIDPrefix is preserved for log
			// correlation only — not sent on the wire.
			runID := uuid.NewString()

			var progress Progress
			if opts.PerRunProgress != nil {
				progress = opts.PerRunProgress(m, kind)
			}

			runCtx, cancelRun := context.WithTimeout(ctx, timeout)
			runRes, runErr := RunOne(runCtx, tallyClient, opts.Sender, Request{
				KindName:      kind.Name,
				TallyCompany:  m.TallyCompanyName,
				TallyEndpoint: m.TallyEndpoint,
				TallyKind:     kind.Tally,
				IngestKind:    kind.Ingest,
				From:          opts.From,
				To:            opts.To,
				RunID:         runID,
				RunKind:       opts.RunKind,
				BatchSize:     opts.BatchSize,
			}, progress)
			cancelRun()

			if opts.PerKindResultHook != nil {
				opts.PerKindResultHook(m, kind, runRes, runErr)
			}

			if runErr != nil {
				res.FatalErrors++
				if !opts.ContinueOnError {
					res.StoppedEarly = true
					return res
				}
				continue
			}
			mappingRan = true
			res.TotalRows += runRes.RowCount
			res.TotalBatchesSent += runRes.BatchesSent
			res.TotalBatchesFailed += runRes.BatchesFailed
			res.TotalServerSkipped += runRes.ServerSkipped
			res.TotalServerErrors += runRes.ServerErrors
		}
		if mappingRan {
			res.MappingsRun++
		}
	}
	return res
}

// MapKindByName resolves a comma-tolerant string to a KindPair. Used
// by the CLI's --kinds flag and any other code that takes a
// user-supplied kind name.
func MapKindByName(name string) (KindPair, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "purchase":
		return KindPair{Name: "purchase", Tally: tally.VoucherPurchase, Ingest: tally.IngestKindPurchase}, nil
	case "sales":
		return KindPair{Name: "sales", Tally: tally.VoucherSales, Ingest: tally.IngestKindSales}, nil
	case "credit_note", "credit-note", "creditnote":
		return KindPair{Name: "credit_note", Tally: tally.VoucherCreditNote, Ingest: tally.IngestKindCreditNote}, nil
	case "debit_note", "debit-note", "debitnote":
		return KindPair{Name: "debit_note", Tally: tally.VoucherDebitNote, Ingest: tally.IngestKindDebitNote}, nil
	case "journal":
		return KindPair{Name: "journal", Tally: tally.VoucherJournal}, nil
	case "payment":
		return KindPair{Name: "payment", Tally: tally.VoucherPayment}, nil
	case "tax_ledger", "tax-ledger", "taxledger":
		return KindPair{Name: "tax_ledger"}, nil
	default:
		return KindPair{}, fmt.Errorf("unknown kind %q", name)
	}
}
