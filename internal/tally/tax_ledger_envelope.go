package tally

import (
	"bytes"
	"fmt"
	"html/template"
	"time"
)

// TaxLedgerRequest fetches month-wise ledger movement for Duties & Taxes /
// balance-sheet control ledgers. The request is intentionally month-shaped:
// partial windows would produce misleading control totals for filing support.
type TaxLedgerRequest struct {
	Company string
	From    time.Time
	To      time.Time
}

var taxLedgerTmpl = template.Must(template.New("tax-ledgers").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>GstrecoTaxLedgerCollection</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVCURRENTCOMPANY>{{.Company}}</SVCURRENTCOMPANY>
        <SVFROMDATE TYPE="Date">{{.FromDate}}</SVFROMDATE>
        <SVTODATE TYPE="Date">{{.ToDate}}</SVTODATE>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
      <TDL>
        <TDLMESSAGE>
          <COLLECTION NAME="GstrecoTaxLedgerCollection" ISINITIALIZE="Yes">
            <TYPE>Ledger</TYPE>
            <CHILDOF>Duties &amp; Taxes</CHILDOF>
            <BELONGSTO>Yes</BELONGSTO>
            <FETCH>NAME, PARENT, PERIODOPENINGBALANCE, PERIODDEBITTOTALS, PERIODCREDITTOTALS, PERIODCLOSINGBALANCE</FETCH>
            <COMPUTE>PERIODOPENINGBALANCE : $$FromValue:"{{.FromHuman}}":$$ToValue:"{{.ToHuman}}":$OpeningBalance</COMPUTE>
            <COMPUTE>PERIODDEBITTOTALS : $$FromValue:"{{.FromHuman}}":$$ToValue:"{{.ToHuman}}":$DebitTotals</COMPUTE>
            <COMPUTE>PERIODCREDITTOTALS : $$FromValue:"{{.FromHuman}}":$$ToValue:"{{.ToHuman}}":$CreditTotals</COMPUTE>
            <COMPUTE>PERIODCLOSINGBALANCE : $$FromValue:"{{.FromHuman}}":$$ToValue:"{{.ToHuman}}":$ClosingBalance</COMPUTE>
          </COLLECTION>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`))

// BuildTaxLedgerXML renders the Duties & Taxes ledger request envelope.
func BuildTaxLedgerXML(r TaxLedgerRequest) ([]byte, error) {
	if r.Company == "" {
		return nil, fmt.Errorf("tally: Company is required")
	}
	if r.From.IsZero() || r.To.IsZero() {
		return nil, fmt.Errorf("tally: From and To dates are required")
	}
	if r.To.Before(r.From) {
		return nil, fmt.Errorf("tally: To (%s) is before From (%s)", r.To, r.From)
	}
	if !isFullCalendarMonth(r.From, r.To) {
		return nil, fmt.Errorf("tally: tax-ledger export requires a full calendar month window")
	}
	for _, b := range []byte(r.Company) {
		if b < 0x20 && b != '\t' {
			return nil, fmt.Errorf("tally: Company contains control character 0x%02x", b)
		}
	}

	params := struct {
		Company   string
		FromDate  string
		ToDate    string
		FromHuman string
		ToHuman   string
	}{
		Company:   r.Company,
		FromDate:  FormatTallyDate(r.From),
		ToDate:    FormatTallyDate(r.To),
		FromHuman: r.From.Format("02-01-2006"),
		ToHuman:   r.To.Format("02-01-2006"),
	}
	var buf bytes.Buffer
	if err := taxLedgerTmpl.Execute(&buf, params); err != nil {
		return nil, fmt.Errorf("tally: render tax-ledger envelope: %w", err)
	}
	return buf.Bytes(), nil
}

func isFullCalendarMonth(from, to time.Time) bool {
	if from.Year() != to.Year() || from.Month() != to.Month() {
		return false
	}
	if from.Day() != 1 {
		return false
	}
	return to.Day() == from.AddDate(0, 1, -1).Day()
}
