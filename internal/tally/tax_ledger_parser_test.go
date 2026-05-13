package tally

import "testing"

func TestParseTaxLedgerV3_UsesNameAttributeWhenNameElementMissing(t *testing.T) {
	raw := []byte(`<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY>
    <DATA>
      <COLLECTION>
        <LEDGER NAME="Input CGST">
          <PARENT>Duties &amp; Taxes</PARENT>
          <PERIODOPENINGBALANCE>0</PERIODOPENINGBALANCE>
          <PERIODDEBITTOTALS>120.50</PERIODDEBITTOTALS>
          <PERIODCREDITTOTALS>10.00</PERIODCREDITTOTALS>
          <PERIODCLOSINGBALANCE>110.50</PERIODCLOSINGBALANCE>
        </LEDGER>
      </COLLECTION>
    </DATA>
  </BODY>
</ENVELOPE>`)

	got, err := ParseTaxLedgerV3(raw)
	if err != nil {
		t.Fatalf("ParseTaxLedgerV3 error: %v", err)
	}
	if got.Status != 1 {
		t.Fatalf("Status=%d, want 1", got.Status)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("Warnings=%v, want none", got.Warnings)
	}
	if len(got.Ledgers) != 1 {
		t.Fatalf("Ledgers=%d, want 1", len(got.Ledgers))
	}

	row := got.Ledgers[0]
	if row.Name != "Input CGST" {
		t.Fatalf("Name=%q, want %q", row.Name, "Input CGST")
	}
	if row.Parent != "Duties & Taxes" {
		t.Fatalf("Parent=%q, want %q", row.Parent, "Duties & Taxes")
	}
	if row.OpeningBalance != 0 {
		t.Fatalf("OpeningBalance=%v, want 0", row.OpeningBalance)
	}
	if row.DebitTotals != 120.50 {
		t.Fatalf("DebitTotals=%v, want 120.50", row.DebitTotals)
	}
	if row.CreditTotals != 10 {
		t.Fatalf("CreditTotals=%v, want 10", row.CreditTotals)
	}
	if row.ClosingBalance != 110.50 {
		t.Fatalf("ClosingBalance=%v, want 110.50", row.ClosingBalance)
	}
}
