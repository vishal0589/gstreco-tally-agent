package tally

import (
	"bytes"
	"fmt"
	"html/template"
	"time"
)

// DayBookRequest specifies a day-book fetch. The agent issues one
// DayBookRequest per (company, voucher kind, date window) tuple per run.
type DayBookRequest struct {
	// Company is the Tally company name as shown in the company selector
	// (Tally calls this the SVCURRENTCOMPANY static variable). Whitespace
	// matters; agent must pass the user's exact string.
	Company string
	// From and To are inclusive. Tally filters at the server side so an
	// empty range returns an empty collection (valid, not an error).
	From time.Time
	To   time.Time
	// Kind is purchase/sales/credit_note/debit_note/journal/payment. The
	// envelope filter uses Tally's built-in voucher-family functions so custom
	// voucher types that inherit from the underlying family are picked up too.
	Kind VoucherKind
}

// dayBookTmpl is the request envelope. It fetches the voucher collection with
// its ALLLEDGERENTRIES and INVENTORYENTRIES children so the parser has
// everything needed for tax aggregation in one round-trip. FETCH uses
// explicit field lists rather than `*` because `*` is unreliable across Tally
// Prime patch versions (some custom TDLs suppress unlisted fields).
//
// StaticVariables:
//   - SVCURRENTCOMPANY: selects the company; required.
//   - SVFROMDATE / SVTODATE: YYYYMMDD, inclusive; Tally Prime only accepts
//     this format on the request side regardless of OS locale.
//   - SVEXPORTFORMAT: $$SysName:XML forces XML even if the user's default
//     export format is HTML or JSON.
//
// FILTER uses Tally's built-in $$IsSales / $$IsPurchase which return true for
// user-defined voucher types that inherit from Sales/Purchase. A custom type
// named "GST Export Sales" that's based on Sales matches $$IsSales.
var dayBookTmpl = template.Must(template.New("daybook").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>GstrecoVoucherCollection</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVCURRENTCOMPANY>{{.Company}}</SVCURRENTCOMPANY>
        <SVFROMDATE TYPE="Date">{{.From}}</SVFROMDATE>
        <SVTODATE TYPE="Date">{{.To}}</SVTODATE>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
      <TDL>
        <TDLMESSAGE>
          <COLLECTION NAME="GstrecoVoucherCollection" ISINITIALIZE="Yes">
            <TYPE>Voucher</TYPE>
            <FETCH>GUID, ALTERID, DATE, VOUCHERTYPENAME, VOUCHERNUMBER, REFERENCE, PARTYLEDGERNAME, PARTYGSTIN, PLACEOFSUPPLY, ISINVOICE, ISCANCELLED, ISRCMAPPLICABLE, NARRATION</FETCH>
            <FETCH>ALLLEDGERENTRIES.LEDGERNAME, ALLLEDGERENTRIES.AMOUNT, ALLLEDGERENTRIES.GSTCLASS, ALLLEDGERENTRIES.BILLALLOCATIONS</FETCH>
            <FETCH>INVENTORYENTRIES.STOCKITEMNAME, INVENTORYENTRIES.ACTUALQTY, INVENTORYENTRIES.RATE, INVENTORYENTRIES.AMOUNT, INVENTORYENTRIES.GSTHSNNAME</FETCH>
            <FILTER>{{.Filter}}</FILTER>
          </COLLECTION>
          <SYSTEM TYPE="Formulae" NAME="{{.Filter}}">{{.FilterExpr}}</SYSTEM>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`))

// BuildDayBookXML renders the request envelope for a DayBookRequest. Returns
// an error if the request has zero dates, an unsupported Kind, or a Company
// name containing characters that Tally can't round-trip in XML (the function
// relies on html/template's built-in escaping so ampersands and angle
// brackets are safe; null bytes and control characters are rejected).
func BuildDayBookXML(r DayBookRequest) ([]byte, error) {
	if r.Company == "" {
		return nil, fmt.Errorf("tally: Company is required")
	}
	if r.From.IsZero() || r.To.IsZero() {
		return nil, fmt.Errorf("tally: From and To dates are required")
	}
	if r.To.Before(r.From) {
		return nil, fmt.Errorf("tally: To (%s) is before From (%s)", r.To, r.From)
	}
	for _, b := range []byte(r.Company) {
		if b < 0x20 && b != '\t' {
			return nil, fmt.Errorf("tally: Company contains control character 0x%02x", b)
		}
	}

	filter, filterExpr, err := filterForKind(r.Kind)
	if err != nil {
		return nil, err
	}

	params := struct {
		Company    string
		From       string
		To         string
		Filter     string
		FilterExpr string
	}{
		Company:    r.Company,
		From:       FormatTallyDate(r.From),
		To:         FormatTallyDate(r.To),
		Filter:     filter,
		FilterExpr: filterExpr,
	}
	var buf bytes.Buffer
	if err := dayBookTmpl.Execute(&buf, params); err != nil {
		return nil, fmt.Errorf("tally: render envelope: %w", err)
	}
	return buf.Bytes(), nil
}

// filterForKind returns the TDL filter name and its expression for a given
// VoucherKind. Keeping the name and expression paired here means adding
// new voucher families is a one-line addition.
func filterForKind(k VoucherKind) (name, expr string, err error) {
	switch k {
	case VoucherPurchase:
		return "IsGstrecoPurchase", "$$IsPurchase:$VoucherTypeName", nil
	case VoucherSales:
		return "IsGstrecoSales", "$$IsSales:$VoucherTypeName", nil
	case VoucherCreditNote:
		// $$IsCreditNote picks up user-defined types that inherit from Credit
		// Note (Tally lets users name them "Sales Return" or similar). The
		// side (purchase vs sales) is inferred downstream from the party
		// ledger's group — this filter only narrows the collection.
		return "IsGstrecoCreditNote", "$$IsCreditNote:$VoucherTypeName", nil
	case VoucherDebitNote:
		return "IsGstrecoDebitNote", "$$IsDebitNote:$VoucherTypeName", nil
	case VoucherJournal:
		return "IsGstrecoJournal", "$$IsJournal:$VoucherTypeName", nil
	case VoucherPayment:
		return "IsGstrecoPayment", "$$IsPayment:$VoucherTypeName", nil
	default:
		return "", "", fmt.Errorf("tally: unsupported voucher kind %q", k)
	}
}
