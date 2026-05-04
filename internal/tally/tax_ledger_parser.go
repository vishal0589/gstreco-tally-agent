package tally

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
)

// RawTaxLedger is one month-scoped control ledger as Tally returns it.
type RawTaxLedger struct {
	Name           string
	Parent         string
	OpeningBalance float64
	DebitTotals    float64
	CreditTotals   float64
	ClosingBalance float64
}

type TaxLedgerResult struct {
	Ledgers  []RawTaxLedger
	Status   int
	Warnings []string
}

// ParseTaxLedgerV3 decodes the Duties & Taxes collection export into
// RawTaxLedger rows. It uses the same permissive token walker as the voucher
// and master parsers so wrapper shape changes do not break the agent.
func ParseTaxLedgerV3(raw []byte) (TaxLedgerResult, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return TaxLedgerResult{}, fmt.Errorf("tally: empty tax-ledger response")
	}
	raw = prepareXMLForDecode(raw)
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	result := TaxLedgerResult{Status: -1}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("tally: tax-ledger xml decode: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "STATUS":
			var s string
			if err := dec.DecodeElement(&s, &start); err == nil {
				if v, perr := parseIntStrict(strings.TrimSpace(s)); perr == nil {
					result.Status = v
				}
			}
		case "LEDGER":
			var x xmlTaxLedger
			if err := dec.DecodeElement(&x, &start); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("tax-ledger decode: %v", err))
				continue
			}
			row, warnings := x.toRaw()
			result.Warnings = append(result.Warnings, warnings...)
			if strings.TrimSpace(row.Name) == "" {
				result.Warnings = append(result.Warnings, "tax-ledger dropped: missing NAME")
				continue
			}
			result.Ledgers = append(result.Ledgers, row)
		}
	}
	return result, nil
}

type xmlTaxLedger struct {
	Name                 string `xml:"NAME"`
	Parent               string `xml:"PARENT"`
	PeriodOpeningBalance string `xml:"PERIODOPENINGBALANCE"`
	PeriodDebitTotals    string `xml:"PERIODDEBITTOTALS"`
	PeriodCreditTotals   string `xml:"PERIODCREDITTOTALS"`
	PeriodClosingBalance string `xml:"PERIODCLOSINGBALANCE"`
}

func (x xmlTaxLedger) toRaw() (RawTaxLedger, []string) {
	row := RawTaxLedger{
		Name:   strings.TrimSpace(x.Name),
		Parent: strings.TrimSpace(x.Parent),
	}
	var warnings []string

	if amt, err := parseTallyAmount(x.PeriodOpeningBalance); err == nil {
		row.OpeningBalance = round2(amt)
	} else if strings.TrimSpace(x.PeriodOpeningBalance) != "" {
		warnings = append(warnings, fmt.Sprintf("ledger %q opening balance: %v", row.Name, err))
	}
	if amt, err := parseTallyAmount(x.PeriodDebitTotals); err == nil {
		row.DebitTotals = round2(amt)
	} else if strings.TrimSpace(x.PeriodDebitTotals) != "" {
		warnings = append(warnings, fmt.Sprintf("ledger %q debit totals: %v", row.Name, err))
	}
	if amt, err := parseTallyAmount(x.PeriodCreditTotals); err == nil {
		row.CreditTotals = round2(amt)
	} else if strings.TrimSpace(x.PeriodCreditTotals) != "" {
		warnings = append(warnings, fmt.Sprintf("ledger %q credit totals: %v", row.Name, err))
	}
	if amt, err := parseTallyAmount(x.PeriodClosingBalance); err == nil {
		row.ClosingBalance = round2(amt)
	} else if strings.TrimSpace(x.PeriodClosingBalance) != "" {
		warnings = append(warnings, fmt.Sprintf("ledger %q closing balance: %v", row.Name, err))
	}
	return row, warnings
}

func parseIntStrict(value string) (int, error) {
	n := 0
	if value == "" {
		return 0, fmt.Errorf("empty integer")
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("non-digit %q", r)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
