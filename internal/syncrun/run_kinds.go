package syncrun

import (
	"context"
	"fmt"

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

	rows := make([]tally.IngestVoucherRow, 0, len(parsed.Vouchers))
	for _, v := range parsed.Vouchers {
		out, normErr := tally.Normalize(v, tally.NormalizeOptions{
			Kind:    req.IngestKind,
			RunKind: req.RunKind,
		})
		if normErr != nil {
			res.DroppedOnNormalize++
			continue
		}
		rows = append(rows, out...)
	}
	res.RowCount = len(rows)
	emit("normalized", fmt.Sprintf("%d ingest rows (dropped %d on normalize)",
		len(rows), res.DroppedOnNormalize))

	chunks := ingest.SplitRows(rows, req.BatchSize)
	var tallyEndpoint *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	periodFrom := req.From.Format("2006-01-02")
	periodTo := req.To.Format("2006-01-02")
	if len(chunks) == 0 {
		chunks = [][]tally.IngestVoucherRow{{}}
	}
	batches := ingest.BuildBatches(req.RunID, req.RunID, req.IngestKind,
		req.TallyCompany, tallyEndpoint, req.RunKind, &periodFrom, &periodTo, chunks)

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

	rows := make([]tally.JournalVoucherRow, 0, len(parsed.Vouchers))
	for _, v := range parsed.Vouchers {
		out, normErr := tally.NormalizeJournal(v)
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

	var tallyEndpoint *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	periodFrom := req.From.Format("2006-01-02")
	periodTo := req.To.Format("2006-01-02")
	batches := buildJournalBatches(
		req.RunID,
		req.RunID,
		req.TallyCompany,
		tallyEndpoint,
		&periodFrom,
		&periodTo,
		rows,
		req.BatchSize,
	)
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

	rows := make([]tally.PaymentVoucherRow, 0, len(parsed.Vouchers))
	for _, v := range parsed.Vouchers {
		out, normErr := tally.NormalizePayment(v)
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

	var tallyEndpoint *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	periodFrom := req.From.Format("2006-01-02")
	periodTo := req.To.Format("2006-01-02")
	batches := buildPaymentBatches(
		req.RunID,
		req.RunID,
		req.TallyCompany,
		tallyEndpoint,
		&periodFrom,
		&periodTo,
		rows,
		req.BatchSize,
	)
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

	var tallyEndpoint *string
	if req.TallyEndpoint != "" {
		tallyEndpoint = &req.TallyEndpoint
	}
	periodFrom := req.From.Format("2006-01-02")
	periodTo := req.To.Format("2006-01-02")
	batches := buildTaxLedgerBatches(
		req.RunID,
		req.RunID,
		req.TallyCompany,
		tallyEndpoint,
		&periodFrom,
		&periodTo,
		rows,
		req.BatchSize,
	)
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
	periodFrom, periodTo *string,
	rows []tally.JournalVoucherRow,
	size int,
) []tally.JournalIngestRequestBody {
	chunks := splitGenericRows(rows, size)
	if len(chunks) == 0 {
		chunks = [][]tally.JournalVoucherRow{{}}
	}
	out := make([]tally.JournalIngestRequestBody, 0, len(chunks))
	for i, chunk := range chunks {
		out = append(out, tally.JournalIngestRequestBody{
			RunID:         runID,
			Kind:          tally.AccountingBatchJournal,
			TallyCompany:  tallyCompany,
			TallyEndpoint: tallyEndpoint,
			IsFinal:       i == len(chunks)-1,
			PeriodFrom:    periodFrom,
			PeriodTo:      periodTo,
			RequestID:     requestIDPrefix + "-" + intToStr(i+1),
			Batch:         chunk,
		})
	}
	return out
}

func buildPaymentBatches(
	runID, requestIDPrefix, tallyCompany string,
	tallyEndpoint *string,
	periodFrom, periodTo *string,
	rows []tally.PaymentVoucherRow,
	size int,
) []tally.PaymentIngestRequestBody {
	chunks := splitGenericRows(rows, size)
	if len(chunks) == 0 {
		chunks = [][]tally.PaymentVoucherRow{{}}
	}
	out := make([]tally.PaymentIngestRequestBody, 0, len(chunks))
	for i, chunk := range chunks {
		out = append(out, tally.PaymentIngestRequestBody{
			RunID:         runID,
			Kind:          tally.AccountingBatchPayment,
			TallyCompany:  tallyCompany,
			TallyEndpoint: tallyEndpoint,
			IsFinal:       i == len(chunks)-1,
			PeriodFrom:    periodFrom,
			PeriodTo:      periodTo,
			RequestID:     requestIDPrefix + "-" + intToStr(i+1),
			Batch:         chunk,
		})
	}
	return out
}

func buildTaxLedgerBatches(
	runID, requestIDPrefix, tallyCompany string,
	tallyEndpoint *string,
	periodFrom, periodTo *string,
	rows []tally.TaxLedgerMonthlyRow,
	size int,
) []tally.TaxLedgerIngestRequestBody {
	chunks := splitGenericRows(rows, size)
	if len(chunks) == 0 {
		chunks = [][]tally.TaxLedgerMonthlyRow{{}}
	}
	out := make([]tally.TaxLedgerIngestRequestBody, 0, len(chunks))
	for i, chunk := range chunks {
		out = append(out, tally.TaxLedgerIngestRequestBody{
			RunID:         runID,
			Kind:          tally.AccountingBatchTaxLedger,
			TallyCompany:  tallyCompany,
			TallyEndpoint: tallyEndpoint,
			IsFinal:       i == len(chunks)-1,
			PeriodFrom:    periodFrom,
			PeriodTo:      periodTo,
			RequestID:     requestIDPrefix + "-" + intToStr(i+1),
			Batch:         chunk,
		})
	}
	return out
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
