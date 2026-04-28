package ingest

import (
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func makeRows(n int) []tally.IngestVoucherRow {
	rows := make([]tally.IngestVoucherRow, n)
	for i := range rows {
		rows[i] = tally.IngestVoucherRow{InvoiceNumber: "X", InvoiceDate: "2026-04-10"}
	}
	return rows
}

func TestSplitRows(t *testing.T) {
	cases := []struct {
		name       string
		rows, size int
		wantChunks []int // size of each chunk
	}{
		{"empty", 0, 500, nil},
		{"single full chunk", 10, 500, []int{10}},
		{"exact multiple", 1000, 500, []int{500, 500}},
		{"one extra", 501, 500, []int{500, 1}},
		{"tiny size", 5, 2, []int{2, 2, 1}},
		{"zero size defaults", 10, 0, []int{10}}, // defaults to DefaultBatchSize (500)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chunks := SplitRows(makeRows(tc.rows), tc.size)
			if len(chunks) != len(tc.wantChunks) {
				t.Fatalf("len(chunks) = %d, want %d", len(chunks), len(tc.wantChunks))
			}
			for i, want := range tc.wantChunks {
				if len(chunks[i]) != want {
					t.Errorf("chunk %d size = %d, want %d", i, len(chunks[i]), want)
				}
			}
		})
	}
}

func TestBuildBatches_MarksOnlyLastFinal(t *testing.T) {
	chunks := SplitRows(makeRows(1500), 500)
	batches := BuildBatches("run-42", "req-abc", tally.IngestKindPurchase, "Acme", "incremental", chunks)
	if len(batches) != 3 {
		t.Fatalf("len = %d, want 3", len(batches))
	}
	for i, b := range batches {
		if b.RunID != "run-42" || b.TallyCompany != "Acme" || b.Kind != tally.IngestKindPurchase || b.RunKind != "incremental" {
			t.Errorf("batch %d metadata wrong: %+v", i, b)
		}
		if b.RequestID != "req-abc-"+intToStr(i+1) {
			t.Errorf("batch %d request_id = %q", i, b.RequestID)
		}
		wantFinal := i == len(batches)-1
		if b.IsFinal != wantFinal {
			t.Errorf("batch %d IsFinal = %v, want %v", i, b.IsFinal, wantFinal)
		}
	}
}

func TestBuildBatches_EmptyInput(t *testing.T) {
	batches := BuildBatches("r", "req", tally.IngestKindSales, "Co", "full", nil)
	if len(batches) != 0 {
		t.Errorf("empty chunks → len(batches) = %d, want 0", len(batches))
	}
}

func TestIntToStr(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"}, {1, "1"}, {10, "10"}, {99, "99"}, {-5, "-5"},
	}
	for _, tc := range cases {
		if got := intToStr(tc.in); got != tc.want {
			t.Errorf("intToStr(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
