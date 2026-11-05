package syncrun

import (
	"context"
	"fmt"
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
