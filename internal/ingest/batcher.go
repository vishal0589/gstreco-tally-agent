package ingest

import "github.com/vishal0589/gstreco-tally-agent/internal/tally"

// DefaultBatchSize is the number of IngestVoucherRows per HTTP request. The
// server's validator caps at 1000 rows per call; 500 gives us headroom while
// keeping the JSON body under ~500 KiB for median row sizes. Setting is
// exposed so integration tests can exercise the chunk-boundary path with
// small inputs.
const DefaultBatchSize = 500

// SplitRows chunks a normalised row slice into batches of at most size. The
// last chunk may be shorter. Zero or negative size falls back to
// DefaultBatchSize so callers can't accidentally ship one-row-per-request.
func SplitRows(rows []tally.IngestVoucherRow, size int) [][]tally.IngestVoucherRow {
	if size <= 0 {
		size = DefaultBatchSize
	}
	if len(rows) == 0 {
		return nil
	}
	chunks := make([][]tally.IngestVoucherRow, 0, (len(rows)+size-1)/size)
	for start := 0; start < len(rows); start += size {
		end := start + size
		if end > len(rows) {
			end = len(rows)
		}
		chunks = append(chunks, rows[start:end])
	}
	return chunks
}

// BuildBatches wraps each chunk in an IngestRequestBody with the shared run
// metadata. The last batch carries is_final=true so the server can close the
// tally_sync_run and stamp finished_at. Caller is responsible for generating
// a unique request_id per batch (e.g. via uuid); this layer passes it through.
func BuildBatches(runID, requestIDPrefix string, kind tally.IngestKind, tallyCompany, runKind string, chunks [][]tally.IngestVoucherRow) []tally.IngestRequestBody {
	out := make([]tally.IngestRequestBody, 0, len(chunks))
	for i, chunk := range chunks {
		body := tally.IngestRequestBody{
			RunID:        runID,
			Kind:         kind,
			TallyCompany: tallyCompany,
			Batch:        chunk,
			RunKind:      runKind,
			RequestID:    requestIDPrefix + "-" + intToStr(i+1),
			IsFinal:      i == len(chunks)-1,
		}
		out = append(out, body)
	}
	return out
}

// intToStr avoids importing strconv in this tiny helper file. Not
// performance-critical; just readable.
func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
