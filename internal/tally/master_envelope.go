package tally

import (
	"bytes"
	"fmt"
	"html/template"
)

// MasterRequest asks Tally for its party-master ledgers. The agent fires
// one request per (company, kind) pair; Tally's default exports don't let
// us filter to two parent groups in one collection cleanly.
type MasterRequest struct {
	// Company is the Tally company name. Required; Tally will silently
	// return masters from whichever company is currently loaded if
	// SVCURRENTCOMPANY is wrong.
	Company string
	// Kind is vendor or customer. The envelope's FILTER narrows by parent
	// group so the response contains only the relevant ledgers.
	Kind MasterKind
}

// masterTmpl fetches all Ledger objects in the requested parent group. The
// FETCH list is explicit rather than `*` so Prime patch-version differences
// don't surprise us. `ISINITIALIZE="Yes"` forces Tally to rebuild the
// collection (vs serving a cached one from a previous request) — a
// diagnostic aid when the agent is chasing a master that isn't showing up.
//
// FETCH list covers v3 element names and Tally Prime 6.x variants for the
// same fields. Tally returns whatever subset it has; the parser's
// xmlLedger struct + firstNonEmpty in toRaw picks the populated value.
// Asking for both names is cheap (Tally just emits empty elements for
// the ones it doesn't have) and avoids 0% field coverage on Prime 6.x.
var masterTmpl = template.Must(template.New("masters").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>GstrecoMasterCollection</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVCURRENTCOMPANY>{{.Company}}</SVCURRENTCOMPANY>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
      <TDL>
        <TDLMESSAGE>
          <COLLECTION NAME="GstrecoMasterCollection" ISINITIALIZE="Yes">
            <TYPE>Ledger</TYPE>
            <FETCH>NAME, GUID, ALTERID, PARENT, LEDGERCONTACT, CONTACTPERSON, COUNTRYNAME</FETCH>
            <FETCH>EMAIL, EMAILID, LEDGEREMAIL, LEDGERMAIL</FETCH>
            <FETCH>LEDGERPHONE, PHONENO, PHONENUMBER, CONTACTNUMBER</FETCH>
            <FETCH>LEDGERMOBILE, LEDGERMOBILENO, MOBILENO, MOBILENUMBER</FETCH>
            <FETCH>PARTYGSTIN, GSTIN, PARTYGSTNUMBER, GSTREGISTRATIONTYPE, PARTYREGISTRATIONTYPE</FETCH>
            <FETCH>STATENAME, LEDSTATENAME, LEDGERSTATENAME, GSTSTATENAME, PARTYSTATENAME</FETCH>
            <FETCH>MAILINGNAME.LIST, LEDGERMAILINGNAME.LIST, ADDRESS.LIST, LEDGERMAILINGADDRESS.LIST</FETCH>
            <FILTER>{{.Filter}}</FILTER>
          </COLLECTION>
          <SYSTEM TYPE="Formulae" NAME="{{.Filter}}">{{.FilterExpr}}</SYSTEM>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`))

// BuildMasterXML renders the master-fetch envelope. Errors for missing
// company, unknown kind, and control-char injection attempts (same
// validation surface as BuildDayBookXML).
func BuildMasterXML(r MasterRequest) ([]byte, error) {
	if r.Company == "" {
		return nil, fmt.Errorf("tally: MasterRequest.Company is required")
	}
	for _, b := range []byte(r.Company) {
		if b < 0x20 && b != '\t' {
			return nil, fmt.Errorf("tally: Company contains control character 0x%02x", b)
		}
	}
	filterName, filterExpr, err := masterFilterForKind(r.Kind)
	if err != nil {
		return nil, err
	}
	params := struct {
		Company    string
		Filter     string
		FilterExpr string
	}{r.Company, filterName, filterExpr}
	var buf bytes.Buffer
	if err := masterTmpl.Execute(&buf, params); err != nil {
		return nil, fmt.Errorf("tally: render master envelope: %w", err)
	}
	return buf.Bytes(), nil
}

// masterFilterForKind maps MasterKind to Tally's group filter. The filter
// expression compares the ledger's Parent field against the canonical group
// name. Users who rename "Sundry Creditors" to "Vendors" will need to add
// their custom group to this mapping in a follow-up; for V1 we assume
// Tally defaults.
func masterFilterForKind(k MasterKind) (name, expr string, err error) {
	switch k {
	case MasterVendor:
		return "IsGstrecoVendor", "$Parent = \"Sundry Creditors\"", nil
	case MasterCustomer:
		return "IsGstrecoCustomer", "$Parent = \"Sundry Debtors\"", nil
	default:
		return "", "", fmt.Errorf("tally: unsupported master kind %q", k)
	}
}

// CompanyListRequest has no parameters — we always ask for every loaded
// company. Kept as a typed value for future extension (filter by OS user,
// filter by path on disk).
type CompanyListRequest struct{}

// companyListTmpl asks Tally for the list of currently-loaded companies.
// Uses the built-in `List of Companies` report so we don't have to roll
// our own TDL COLLECTION definition.
//
// TYPE=Collection (NOT Data). Tally Prime 6.x is strict: built-in
// reports like "List of Companies" must be requested as Collection,
// not Data. The pre-v0.1.3 envelope used Data (which Tally Prime 3.x
// silently accepted) and 6.x returned `STATUS=0 + DESC not found` —
// the agent's discover then logged "responded but not Tally" and
// gave up. Cross-checked against the production-shipping Manual2AI
// Python adapter (PlummLegano/scripts/tally-sync/tally_client.py:123)
// which has run against PLLUM's Tally Prime 6.1 install since the
// 2026-04-19 ship.
var companyListTmpl = template.Must(template.New("companies").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>List of Companies</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
    </DESC>
  </BODY>
</ENVELOPE>`))

// BuildCompanyListXML renders the company-list request. Takes no parameters
// but returns an error for symmetry with the rest of the envelope helpers.
func BuildCompanyListXML(_ CompanyListRequest) ([]byte, error) {
	var buf bytes.Buffer
	if err := companyListTmpl.Execute(&buf, nil); err != nil {
		return nil, fmt.Errorf("tally: render company-list envelope: %w", err)
	}
	return buf.Bytes(), nil
}

// versionTmpl asks Tally for its runtime $$Version. Uses a Function
// evaluation TDL which Prime 3.x and 4.x both support. Response is a short
// XML with the version string at any of several tag names depending on
// install-time configuration, so the parser walks tokens rather than
// binding to a single path.
var versionTmpl = template.Must(template.New("version").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Function</TYPE>
    <ID>$$Version</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
    </DESC>
  </BODY>
</ENVELOPE>`))

// BuildVersionXML renders the $$Version request. Used once per agent session
// at startup to pick parser_v3 vs parser_v4.
func BuildVersionXML() ([]byte, error) {
	var buf bytes.Buffer
	if err := versionTmpl.Execute(&buf, nil); err != nil {
		return nil, fmt.Errorf("tally: render version envelope: %w", err)
	}
	return buf.Bytes(), nil
}
