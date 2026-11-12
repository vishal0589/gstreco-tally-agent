package syncrun

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

type emitFn func(stage, msg string)
type emitBatchFn func(stage string, idx, total int, msg string, err error)

func runInvoiceSync(
	ctx context.Context,
	tallyClient TallyPoster,
	sender IngestSender,
	req Request,
	res Result,
	emit emitFn,
	emitBatch emitBatchFn,
) (Result, error) {
	envelope, err := tally.BuildDayBookXML(tally.DayBookRequest{
		Company: req.TallyCompany,
		From:    req.From,
		To:      req.To,
		Kind:    req.TallyKind,
	})
	if err != nil {
		return res, fmt.Errorf("build envelope: %w", err)
	}

	emit("fetching", fmt.Sprintf("fetching %s vouchers for %q (%s..%s)",
		req.TallyKind, req.TallyCompany,
		req.From.Format("2006-01-02"), req.To.Format("2006-01-02")))

	respXML, err := tallyClient.PostXML(ctx, envelope)
	if err != nil {
		return res, fmt.Errorf("tally fetch: %w", err)
	}

	parsed, err := tally.ParseDayBookV3(respXML)
	if err != nil {
		return res, fmt.Errorf("parse response: %w", err)
	}
	res.ParseStatus = parsed.Status
	res.ParseWarnings = parsed.Warnings
	emit("parsed", fmt.Sprintf("parsed %d vouchers (status=%d, warnings=%d)",
		len(parsed.Vouchers), parsed.Status, len(parsed.Warnings)))

	// QA v18 R3: only a Duties & Taxes ledger makes a leg GST, so the
	// company's duty-ledger masters are read once before any voucher. The
	// ledger directory is read first: its group tree names the company's
	// Duties & Taxes group and stands in for the masters when they cannot be
	// read (QA v27 L1).
	lookup := invoiceLedgerLookup(ctx, tallyClient, req, parsed.Vouchers, &res, emit)
	dutyLedgers := dutyLedgerLookup(ctx, tallyClient, req, parsed.Vouchers, lookup, &res, emit)
	rows := make([]tally.IngestVoucherRow, 0, len(parsed.Vouchers))
	var notNormalized []string
	var notNormalizedWhy []string
	for _, v := range parsed.Vouchers {
		out, normErr := tally.Normalize(v, tally.NormalizeOptions{
			Kind:        req.IngestKind,
			RunKind:     req.RunKind,
			Ledgers:     lookup,
			DutyLedgers: dutyLedgers,
		})
		if normErr != nil {
			res.DroppedOnNormalize++
			res.ParseWarnings = append(res.ParseWarnings, normErr.Error())
			notNormalized = append(notNormalized, v.GUID)
			if len(notNormalizedWhy) < 3 {
				notNormalizedWhy = append(notNormalizedWhy, normErr.Error())
			}
			continue
		}
		rows = append(rows, out...)
	}
	// A voucher the agent cannot turn into a row is never dropped silently:
	// sync health names it (QA v27 L1).
	if len(notNormalized) > 0 {
		res.addSyncWarning(newSyncWarning(tally.SyncWarningVouchersNotNormalized, req.TallyCompany,
			fmt.Sprintf("%d %s vouchers could not be read into rows and were not sent: %s",
				len(notNormalized), req.IngestKind, strings.Join(notNormalizedWhy, "; ")),
			syncWarningNow(), notNormalized))
	}
	// QA v27 B25: one run is one company and period, so two vouchers of a
	// party keyed to the same invoice number are flagged here.
	tally.FlagDuplicateInvoiceNumbers(rows)
	res.RowCount = len(rows)
	emit("normalized", fmt.Sprintf("%d ingest rows (dropped %d on normalize)",
		len(rows), res.DroppedOnNormalize))

	zeroLane := zeroRowLane(req, len(rows), len(parsed.Vouchers))
	if len(rows) == 0 && !zeroLane {
		emit("complete", "nothing to send")
		return res, nil
	}

	chunks := ingest.SplitRows(rows, req.BatchSize)
	if zeroLane {
		// QA 05-Nov D12: one empty final batch says "zero rows" for this lane.
		chunks = [][]tally.IngestVoucherRow{{}}
		emit("zero_rows", "no vouchers in the window; sending an empty final batch")
	}
	var tallyEndpoint *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	periodFrom := req.From.Format("2006-01-02")
	periodTo := req.To.Format("2006-01-02")
	batches := ingest.BuildBatches(req.RunID, req.RunID, req.IngestKind,
		req.TallyCompany, tallyEndpoint, req.RunKind, &periodFrom, &periodTo, chunks)
	// Every batch carries the run's masters warnings, so a run whose first
	// batch fails still records them; the server de-duplicates (QA v21 G-A).
	for i := range batches {
		batches[i].SyncWarnings = res.SyncWarnings
		batches[i].SyncRequestID = req.SyncRequestID
	}

	for i, body := range batches {
		if err := ctx.Err(); err != nil {
			emit("complete", fmt.Sprintf("aborted: %v (sent=%d, remaining=%d)",
				err, res.BatchesSent, len(batches)-i))
			return res, err
		}
		emitBatch("batch_sending", i+1, len(batches),
			fmt.Sprintf("batch %d/%d (%d rows, request_id=%s, is_final=%t)",
				i+1, len(batches), len(body.Batch), body.RequestID, body.IsFinal),
			nil)
		accepted, sendErr := sender.Send(ctx, body)
		if sendErr != nil {
			res.BatchesFailed++
			res.BatchErrors = append(res.BatchErrors, BatchError{
				Index:     i + 1,
				Total:     len(batches),
				RequestID: body.RequestID,
				Err:       sendErr,
			})
			emitBatch("batch_failed", i+1, len(batches), sendErr.Error(), sendErr)
			continue
		}
		res.ServerInserted += accepted.Counters.Inserted
		res.ServerAmended += accepted.Counters.Amended
		res.ServerSkipped += accepted.Counters.Skipped
		res.ServerErrors += accepted.Counters.Errors
		if findings := accepted.AllFindings(); len(findings) > 0 {
			res.ServerFindings = append(res.ServerFindings, findings...)
		}
		res.BatchesSent++
		emitBatch("batch_sent", i+1, len(batches),
			fmt.Sprintf("accepted (inserted=%d amended=%d skipped=%d errors=%d)",
				accepted.Counters.Inserted,
				accepted.Counters.Amended,
				accepted.Counters.Skipped,
				accepted.Counters.Errors),
			nil)
	}

	emit("complete", fmt.Sprintf("sent=%d failed=%d rows=%d",
		res.BatchesSent, res.BatchesFailed, res.RowCount))
	return res, nil
}

func runJournalSync(
	ctx context.Context,
	tallyClient TallyPoster,
	sender IngestSender,
	req Request,
	res Result,
	emit emitFn,
	emitBatch emitBatchFn,
) (Result, error) {
	envelope, err := tally.BuildDayBookXML(tally.DayBookRequest{
		Company: req.TallyCompany,
		From:    req.From,
		To:      req.To,
		Kind:    req.TallyKind,
	})
	if err != nil {
		return res, fmt.Errorf("build envelope: %w", err)
	}

	emit("fetching", fmt.Sprintf("fetching %s vouchers for %q (%s..%s)",
		req.KindName, req.TallyCompany,
		req.From.Format("2006-01-02"), req.To.Format("2006-01-02")))

	respXML, err := tallyClient.PostXML(ctx, envelope)
	if err != nil {
		return res, fmt.Errorf("tally fetch: %w", err)
	}

	parsed, err := tally.ParseDayBookV3(respXML)
	if err != nil {
		return res, fmt.Errorf("parse response: %w", err)
	}
	res.ParseStatus = parsed.Status
	res.ParseWarnings = parsed.Warnings
	emit("parsed", fmt.Sprintf("parsed %d vouchers (status=%d, warnings=%d)",
		len(parsed.Vouchers), parsed.Status, len(parsed.Warnings)))

	opts := tally.AccountingNormalizeOptions{
		Ledgers: ledgerGSTINLookup(ctx, tallyClient, req, parsed.Vouchers, &res, emit),
	}
	rows := make([]tally.JournalVoucherRow, 0, len(parsed.Vouchers))
	for _, v := range parsed.Vouchers {
		out, normErr := tally.NormalizeJournalWith(v, opts)
		if normErr != nil {
			res.DroppedOnNormalize++
			continue
		}
		if out == nil {
			continue
		}
		rows = append(rows, *out)
	}
	res.RowCount = len(rows)
	emit("normalized", fmt.Sprintf("%d %s rows (dropped %d on normalize)",
		len(rows), req.KindName, res.DroppedOnNormalize))
	zeroLane := zeroRowLane(req, len(rows), len(parsed.Vouchers))
	if len(rows) == 0 && !zeroLane {
		emit("complete", "nothing to send")
		return res, nil
	}

	var tallyEndpoint *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	batches := buildJournalBatches(req.RunID, req.RunID, req.TallyCompany, tallyEndpoint, rows, req.BatchSize, zeroLane)
	meta := accountingRunMeta(req)
	for i := range batches {
		batches[i].SyncWarnings = res.SyncWarnings
		batches[i].SyncRequestID = req.SyncRequestID
		batches[i].RunKind, batches[i].PeriodFrom, batches[i].PeriodTo = meta.runKind, meta.from, meta.to
		batches[i].IsFinal = i == len(batches)-1
	}
	for i, body := range batches {
		if err := ctx.Err(); err != nil {
			emit("complete", fmt.Sprintf("aborted: %v (sent=%d, remaining=%d)",
				err, res.BatchesSent, len(batches)-i))
			return res, err
		}
		emitBatch("batch_sending", i+1, len(batches),
			fmt.Sprintf("batch %d/%d (%d rows, request_id=%s)",
				i+1, len(batches), len(body.Batch), body.RequestID),
			nil)
		accepted, sendErr := sender.SendJournal(ctx, body)
		if sendErr != nil {
			res.BatchesFailed++
			res.BatchErrors = append(res.BatchErrors, BatchError{
				Index:     i + 1,
				Total:     len(batches),
				RequestID: body.RequestID,
				Err:       sendErr,
			})
			emitBatch("batch_failed", i+1, len(batches), sendErr.Error(), sendErr)
			continue
		}
		res.ServerInserted += accepted.Processed
		res.BatchesSent++
		emitBatch("batch_sent", i+1, len(batches),
			fmt.Sprintf("accepted (processed=%d)", accepted.Processed), nil)
	}

	emit("complete", fmt.Sprintf("sent=%d failed=%d rows=%d",
		res.BatchesSent, res.BatchesFailed, res.RowCount))
	return res, nil
}

func runPaymentSync(
	ctx context.Context,
	tallyClient TallyPoster,
	sender IngestSender,
	req Request,
	res Result,
	emit emitFn,
	emitBatch emitBatchFn,
) (Result, error) {
	envelope, err := tally.BuildDayBookXML(tally.DayBookRequest{
		Company: req.TallyCompany,
		From:    req.From,
		To:      req.To,
		Kind:    req.TallyKind,
	})
	if err != nil {
		return res, fmt.Errorf("build envelope: %w", err)
	}

	emit("fetching", fmt.Sprintf("fetching %s vouchers for %q (%s..%s)",
		req.KindName, req.TallyCompany,
		req.From.Format("2006-01-02"), req.To.Format("2006-01-02")))

	respXML, err := tallyClient.PostXML(ctx, envelope)
	if err != nil {
		return res, fmt.Errorf("tally fetch: %w", err)
	}

	parsed, err := tally.ParseDayBookV3(respXML)
	if err != nil {
		return res, fmt.Errorf("parse response: %w", err)
	}
	res.ParseStatus = parsed.Status
	res.ParseWarnings = parsed.Warnings
	emit("parsed", fmt.Sprintf("parsed %d vouchers (status=%d, warnings=%d)",
		len(parsed.Vouchers), parsed.Status, len(parsed.Warnings)))

	opts := tally.AccountingNormalizeOptions{
		Ledgers: ledgerGSTINLookup(ctx, tallyClient, req, parsed.Vouchers, &res, emit),
	}
	rows := make([]tally.PaymentVoucherRow, 0, len(parsed.Vouchers))
	for _, v := range parsed.Vouchers {
		out, normErr := tally.NormalizePaymentWith(v, opts)
		if normErr != nil {
			res.DroppedOnNormalize++
			continue
		}
		if out == nil {
			continue
		}
		rows = append(rows, *out)
	}
	res.RowCount = len(rows)
	emit("normalized", fmt.Sprintf("%d %s rows (dropped %d on normalize)",
		len(rows), req.KindName, res.DroppedOnNormalize))
	zeroLane := zeroRowLane(req, len(rows), len(parsed.Vouchers))
	if len(rows) == 0 && !zeroLane {
		emit("complete", "nothing to send")
		return res, nil
	}

	var tallyEndpoint *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	batches := buildPaymentBatches(req.RunID, req.RunID, req.TallyCompany, tallyEndpoint, rows, req.BatchSize, zeroLane)
	meta := accountingRunMeta(req)
	for i := range batches {
		batches[i].SyncWarnings = res.SyncWarnings
		batches[i].SyncRequestID = req.SyncRequestID
		batches[i].RunKind, batches[i].PeriodFrom, batches[i].PeriodTo = meta.runKind, meta.from, meta.to
		batches[i].IsFinal = i == len(batches)-1
	}
	for i, body := range batches {
		if err := ctx.Err(); err != nil {
			emit("complete", fmt.Sprintf("aborted: %v (sent=%d, remaining=%d)",
				err, res.BatchesSent, len(batches)-i))
			return res, err
		}
		emitBatch("batch_sending", i+1, len(batches),
			fmt.Sprintf("batch %d/%d (%d rows, request_id=%s)",
				i+1, len(batches), len(body.Batch), body.RequestID),
			nil)
		accepted, sendErr := sender.SendPayment(ctx, body)
		if sendErr != nil {
			res.BatchesFailed++
			res.BatchErrors = append(res.BatchErrors, BatchError{
				Index:     i + 1,
				Total:     len(batches),
				RequestID: body.RequestID,
				Err:       sendErr,
			})
			emitBatch("batch_failed", i+1, len(batches), sendErr.Error(), sendErr)
			continue
		}
		res.ServerInserted += accepted.Processed
		res.BatchesSent++
		emitBatch("batch_sent", i+1, len(batches),
			fmt.Sprintf("accepted (processed=%d)", accepted.Processed), nil)
	}

	emit("complete", fmt.Sprintf("sent=%d failed=%d rows=%d",
		res.BatchesSent, res.BatchesFailed, res.RowCount))
	return res, nil
}

func runTaxLedgerSync(
	ctx context.Context,
	tallyClient TallyPoster,
	sender IngestSender,
	req Request,
	res Result,
	emit emitFn,
	emitBatch emitBatchFn,
) (Result, error) {
	envelope, err := tally.BuildTaxLedgerXML(tally.TaxLedgerRequest{
		Company: req.TallyCompany,
		From:    req.From,
		To:      req.To,
	})
	if err != nil {
		return res, fmt.Errorf("build tax-ledger envelope: %w", err)
	}

	emit("fetching", fmt.Sprintf("fetching tax ledgers for %q (%s..%s)",
		req.TallyCompany,
		req.From.Format("2006-01-02"), req.To.Format("2006-01-02")))

	respXML, err := tallyClient.PostXML(ctx, envelope)
	if err != nil {
		return res, fmt.Errorf("tally fetch: %w", err)
	}

	parsed, err := tally.ParseTaxLedgerV3(respXML)
	if err != nil {
		return res, fmt.Errorf("parse response: %w", err)
	}
	res.ParseStatus = parsed.Status
	res.ParseWarnings = parsed.Warnings
	emit("parsed", fmt.Sprintf("parsed %d tax ledgers (status=%d, warnings=%d)",
		len(parsed.Ledgers), parsed.Status, len(parsed.Warnings)))

	period := req.From.Format("012006")
	rows := tally.NormalizeTaxLedgerRows(parsed.Ledgers, period)
	res.RowCount = len(rows)
	emit("normalized", fmt.Sprintf("%d tax-ledger rows prepared", len(rows)))
	zeroLane := zeroRowLane(req, len(rows), len(parsed.Ledgers))
	if len(rows) == 0 && !zeroLane {
		emit("complete", "nothing to send")
		return res, nil
	}

	var tallyEndpoint *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	batches := buildTaxLedgerBatches(req.RunID, req.RunID, req.TallyCompany, tallyEndpoint, rows, req.BatchSize, zeroLane)
	meta := accountingRunMeta(req)
	for i := range batches {
		batches[i].SyncWarnings = res.SyncWarnings
		batches[i].SyncRequestID = req.SyncRequestID
		batches[i].RunKind, batches[i].PeriodFrom, batches[i].PeriodTo = meta.runKind, meta.from, meta.to
		batches[i].IsFinal = i == len(batches)-1
	}
	for i, body := range batches {
		if err := ctx.Err(); err != nil {
			emit("complete", fmt.Sprintf("aborted: %v (sent=%d, remaining=%d)",
				err, res.BatchesSent, len(batches)-i))
			return res, err
		}
		emitBatch("batch_sending", i+1, len(batches),
			fmt.Sprintf("batch %d/%d (%d rows, request_id=%s)",
				i+1, len(batches), len(body.Batch), body.RequestID),
			nil)
		accepted, sendErr := sender.SendTaxLedgers(ctx, body)
		if sendErr != nil {
			res.BatchesFailed++
			res.BatchErrors = append(res.BatchErrors, BatchError{
				Index:     i + 1,
				Total:     len(batches),
				RequestID: body.RequestID,
				Err:       sendErr,
			})
			emitBatch("batch_failed", i+1, len(batches), sendErr.Error(), sendErr)
			continue
		}
		res.ServerInserted += accepted.Processed
		res.BatchesSent++
		emitBatch("batch_sent", i+1, len(batches),
			fmt.Sprintf("accepted (processed=%d)", accepted.Processed), nil)
	}

	emit("complete", fmt.Sprintf("sent=%d failed=%d rows=%d",
		res.BatchesSent, res.BatchesFailed, res.RowCount))
	return res, nil
}

func buildJournalBatches(
	runID, requestIDPrefix, tallyCompany string,
	tallyEndpoint *string,
	rows []tally.JournalVoucherRow,
	size int,
	emptyFinal bool,
) []tally.JournalIngestRequestBody {
	chunks := splitOrEmptyFinal(rows, size, emptyFinal)
	out := make([]tally.JournalIngestRequestBody, 0, len(chunks))
	for i, chunk := range chunks {
		out = append(out, tally.JournalIngestRequestBody{
			RunID:         runID,
			Kind:          tally.AccountingBatchJournal,
			TallyCompany:  tallyCompany,
			TallyEndpoint: tallyEndpoint,
			RequestID:     requestIDPrefix + "-" + intToStr(i+1),
			Batch:         chunk,
		})
	}
	return out
}

func buildPaymentBatches(
	runID, requestIDPrefix, tallyCompany string,
	tallyEndpoint *string,
	rows []tally.PaymentVoucherRow,
	size int,
	emptyFinal bool,
) []tally.PaymentIngestRequestBody {
	chunks := splitOrEmptyFinal(rows, size, emptyFinal)
	out := make([]tally.PaymentIngestRequestBody, 0, len(chunks))
	for i, chunk := range chunks {
		out = append(out, tally.PaymentIngestRequestBody{
			RunID:         runID,
			Kind:          tally.AccountingBatchPayment,
			TallyCompany:  tallyCompany,
			TallyEndpoint: tallyEndpoint,
			RequestID:     requestIDPrefix + "-" + intToStr(i+1),
			Batch:         chunk,
		})
	}
	return out
}

func buildTaxLedgerBatches(
	runID, requestIDPrefix, tallyCompany string,
	tallyEndpoint *string,
	rows []tally.TaxLedgerMonthlyRow,
	size int,
	emptyFinal bool,
) []tally.TaxLedgerIngestRequestBody {
	chunks := splitOrEmptyFinal(rows, size, emptyFinal)
	out := make([]tally.TaxLedgerIngestRequestBody, 0, len(chunks))
	for i, chunk := range chunks {
		out = append(out, tally.TaxLedgerIngestRequestBody{
			RunID:         runID,
			Kind:          tally.AccountingBatchTaxLedger,
			TallyCompany:  tallyCompany,
			TallyEndpoint: tallyEndpoint,
			RequestID:     requestIDPrefix + "-" + intToStr(i+1),
			Batch:         chunk,
		})
	}
	return out
}

// zeroRowLane is true when a lane served for a sync request found nothing in
// Tally for the window (QA 05-Nov D12): the agent then sends one empty final
// batch so the server records "zero rows" instead of "no batch received".
// Vouchers Tally returned but the agent could not read are never zero rows;
// scheduled and CLI runs send nothing, as before.
func zeroRowLane(req Request, rows, sourceItems int) bool {
	return req.SyncRequestID != "" && rows == 0 && sourceItems == 0
}

// splitOrEmptyFinal splits rows into batches; with no rows and emptyFinal it
// returns one empty (non-nil) batch, so the JSON carries "batch": [].
func splitOrEmptyFinal[T any](rows []T, size int, emptyFinal bool) [][]T {
	chunks := splitGenericRows(rows, size)
	if len(chunks) == 0 && emptyFinal {
		return [][]T{make([]T, 0)}
	}
	return chunks
}

type accountingMeta struct {
	runKind string
	from    *string
	to      *string
}

// accountingRunMeta is the run kind and window the accounting batches carry,
// as the invoice batches always have.
func accountingRunMeta(req Request) accountingMeta {
	meta := accountingMeta{runKind: req.RunKind}
	if !req.From.IsZero() {
		from := req.From.Format("2006-01-02")
		meta.from = &from
	}
	if !req.To.IsZero() {
		to := req.To.Format("2006-01-02")
		meta.to = &to
	}
	return meta
}

func splitGenericRows[T any](rows []T, size int) [][]T {
	if size <= 0 {
		size = ingest.DefaultBatchSize
	}
	if len(rows) == 0 {
		return nil
	}
	chunks := make([][]T, 0, (len(rows)+size-1)/size)
	for start := 0; start < len(rows); start += size {
		end := start + size
		if end > len(rows) {
			end = len(rows)
		}
		chunks = append(chunks, rows[start:end])
	}
	return chunks
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// runTaxLedgerVoucherSync is the tax_ledger_voucher lane (QA 05-Nov, probe
// KSS-Tally-ReadOnly-v13): every non-cancelled voucher of the month, of any
// voucher type, with a leg on a tax ledger, sent with all its legs as Tally
// returned them, so the server can tie each ledger's voucher total to the
// month's closing − opening.
//
// The tax ledger set is read in the same run with the tax_ledger lane's own
// request and parser (BuildTaxLedgerXML / ParseTaxLedgerV3), so the two
// lanes can never disagree on which ledgers are tax ledgers.
//
// Every month is sent as a complete snapshot, scheduled ticks included: the
// final batch always goes (empty when nothing matched) and carries
// snapshot{complete,...}, so "proven empty" differs from "not synced".
// When Tally answers but the agent can't read the answer, one empty final
// batch with complete=false and a sync warning replaces any earlier
// snapshot's claim, and the run still fails like the tax_ledger lane.
func runTaxLedgerVoucherSync(
	ctx context.Context,
	tallyClient TallyPoster,
	sender IngestSender,
	req Request,
	res Result,
	emit emitFn,
	emitBatch emitBatchFn,
) (Result, error) {
	ledgerEnv, err := tally.BuildTaxLedgerXML(tally.TaxLedgerRequest{
		Company: req.TallyCompany, From: req.From, To: req.To,
	})
	if err != nil {
		return res, fmt.Errorf("build tax-ledger envelope: %w", err)
	}
	voucherEnv, err := tally.BuildTaxLedgerVoucherXML(tally.TaxLedgerVoucherRequest{
		Company: req.TallyCompany, From: req.From, To: req.To,
	})
	if err != nil {
		return res, fmt.Errorf("build tax-ledger voucher envelope: %w", err)
	}
	window := fmt.Sprintf("%s..%s", req.From.Format("2006-01-02"), req.To.Format("2006-01-02"))

	// incomplete sends the one empty final batch that says this month's
	// snapshot is not complete, with the reason as a sync warning.
	incomplete := func(snap tally.TaxLedgerVoucherSnapshot, why string) Result {
		res.addSyncWarning(newSyncWarning(tally.SyncWarningTaxLedgerVouchersIncomplete, req.TallyCompany,
			fmt.Sprintf("tax-ledger vouchers for %s not complete: %s", window, why), syncWarningNow(), nil))
		snap.Complete = false
		return sendTaxLedgerVoucherBatches(ctx, sender, req, res, nil, snap, emitBatch)
	}

	emit("fetching", fmt.Sprintf("fetching tax ledgers for %q (%s)", req.TallyCompany, window))
	ledgerXML, err := tallyClient.PostXML(ctx, ledgerEnv)
	if err != nil {
		return res, fmt.Errorf("tally fetch tax ledgers: %w", err)
	}
	ledgers, err := tally.ParseTaxLedgerV3(ledgerXML)
	if err != nil {
		res = incomplete(tally.TaxLedgerVoucherSnapshot{}, "the tax ledger list could not be read: "+err.Error())
		return res, fmt.Errorf("parse tax ledgers: %w", err)
	}
	ledgerSet := make(map[string]bool, len(ledgers.Ledgers))
	for _, l := range ledgers.Ledgers {
		if key := tally.TaxLedgerKey(l.Name); key != "" {
			ledgerSet[key] = true
		}
	}
	if len(ledgerSet) == 0 {
		emit("complete", "no Duties & Taxes ledgers listed; snapshot not complete")
		return incomplete(tally.TaxLedgerVoucherSnapshot{}, "Tally listed no Duties & Taxes ledgers"), nil
	}

	emit("fetching", fmt.Sprintf("fetching vouchers touching %d tax ledgers for %q (%s)",
		len(ledgerSet), req.TallyCompany, window))
	// The real HTTP client uses Tally's documented Unicode transport. Keep
	// PostXML-only adapters supported; parser safety is unchanged either way.
	postVouchers := tallyClient.PostXML
	if unicodeClient, ok := tallyClient.(interface {
		PostXMLUnicode(context.Context, []byte) ([]byte, error)
	}); ok {
		postVouchers = unicodeClient.PostXMLUnicode
	}
	voucherXML, err := postVouchers(ctx, voucherEnv)
	if err != nil {
		return res, fmt.Errorf("tally fetch vouchers: %w", err)
	}
	reply, err := tally.ParseTaxLedgerVoucherReply(voucherXML)
	if err != nil {
		res.ParseWarnings = append(res.ParseWarnings, err.Error())
		res = incomplete(tally.TaxLedgerVoucherSnapshot{}, "the voucher reply could not be read: "+err.Error())
		return res, fmt.Errorf("parse response: %w", err)
	}
	res.ParseStatus = reply.Status

	sel := tally.SelectTaxLedgerVouchers(reply, ledgerSet, req.From, req.To)
	snap := sel.Snapshot
	if snap.OutsideWindow > 0 {
		res.addSyncWarning(newSyncWarning(tally.SyncWarningTaxLedgerVouchersIncomplete, req.TallyCompany,
			fmt.Sprintf("tax-ledger vouchers for %s not complete: Tally returned %d vouchers dated outside the window (the date range was not honoured)",
				window, snap.OutsideWindow), syncWarningNow(), sel.OutsideWindowGUIDs))
	}
	if snap.DateUnreadable > 0 {
		res.addSyncWarning(newSyncWarning(tally.SyncWarningTaxLedgerVouchersIncomplete, req.TallyCompany,
			fmt.Sprintf("tax-ledger vouchers for %s not complete: %d vouchers have a date the agent cannot read",
				window, snap.DateUnreadable), syncWarningNow(), sel.DateUnreadableGUIDs))
	}
	res.RowCount = len(sel.Rows)
	emit("normalized", fmt.Sprintf("%d of %d vouchers touch a tax ledger (cancelled %d, outside window %d, date unreadable %d, unbalanced %d)",
		len(sel.Rows), snap.VouchersReturned, snap.CancelledTouching, snap.OutsideWindow, snap.DateUnreadable, sel.Unbalanced))

	res = sendTaxLedgerVoucherBatches(ctx, sender, req, res, sel.Rows, snap, emitBatch)
	if err := ctx.Err(); err != nil {
		return res, err
	}
	emit("complete", fmt.Sprintf("sent=%d failed=%d rows=%d unsupported=%t",
		res.BatchesSent, res.BatchesFailed, res.RowCount, res.LaneUnsupported))
	return res, nil
}

// sendTaxLedgerVoucherBatches sends the month's rows; the final batch (an
// empty one when there are no rows) carries the snapshot. A batch that
// failed before it turns the snapshot's complete to false. A 404/405 means
// the server predates the lane: the lane is unsupported this tick, nothing
// more is sent and nothing counts as failed.
func sendTaxLedgerVoucherBatches(
	ctx context.Context,
	sender IngestSender,
	req Request,
	res Result,
	rows []tally.TaxLedgerVoucherRow,
	snap tally.TaxLedgerVoucherSnapshot,
	emitBatch emitBatchFn,
) Result {
	chunks := splitOrEmptyFinal(rows, req.BatchSize, true)
	var tallyEndpoint, companyGUID *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	if g := strings.TrimSpace(req.CompanyGUID); g != "" {
		companyGUID = &g
	}
	meta := accountingRunMeta(req)
	for i, chunk := range chunks {
		if ctx.Err() != nil {
			return res
		}
		body := tally.TaxLedgerVoucherIngestRequestBody{
			RunID:         req.RunID,
			Kind:          tally.AccountingBatchTaxLedgerVoucher,
			TallyCompany:  req.TallyCompany,
			TallyEndpoint: tallyEndpoint,
			CompanyGUID:   companyGUID,
			RequestID:     req.RunID + "-" + intToStr(i+1),
			Batch:         chunk,
			SyncWarnings:  res.SyncWarnings,
			SyncRequestID: req.SyncRequestID,
			RunKind:       meta.runKind,
			PeriodFrom:    meta.from,
			PeriodTo:      meta.to,
			IsFinal:       i == len(chunks)-1,
		}
		if body.IsFinal {
			s := snap
			s.VouchersSent = len(rows)
			s.Complete = s.Complete && res.BatchesFailed == 0
			body.Snapshot = &s
		}
		emitBatch("batch_sending", i+1, len(chunks),
			fmt.Sprintf("batch %d/%d (%d vouchers, request_id=%s, is_final=%t)",
				i+1, len(chunks), len(chunk), body.RequestID, body.IsFinal), nil)
		accepted, sendErr := sender.SendTaxLedgerVouchers(ctx, body)
		if sendErr != nil {
			if se := ingest.IsSendError(sendErr); se != nil &&
				(se.Status == http.StatusNotFound || se.Status == http.StatusMethodNotAllowed) {
				res.LaneUnsupported = true
				emitBatch("lane_unsupported", i+1, len(chunks),
					fmt.Sprintf("server has no %s endpoint (HTTP %d); lane skipped this tick", ingest.TaxLedgerVoucherPath, se.Status), nil)
				return res
			}
			res.BatchesFailed++
			res.BatchErrors = append(res.BatchErrors, BatchError{
				Index: i + 1, Total: len(chunks), RequestID: body.RequestID, Err: sendErr,
			})
			emitBatch("batch_failed", i+1, len(chunks), sendErr.Error(), sendErr)
			continue
		}
		res.ServerInserted += accepted.Processed
		res.BatchesSent++
		emitBatch("batch_sent", i+1, len(chunks),
			fmt.Sprintf("accepted (processed=%d)", accepted.Processed), nil)
	}
	return res
}
