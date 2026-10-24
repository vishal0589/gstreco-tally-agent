package tally

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"strings"
)

// LedgerGSTINs maps Tally ledger names to the GSTIN on each ledger master.
//
// Journal and payment vouchers rarely carry a voucher-level party GSTIN
// (PARTYGSTIN); Tally fills that on invoice-mode vouchers. The GSTIN still
// sits on the party's ledger master, which is where Tally's own registers
// read it from. Without this map a journal booked against
// "Vendor Pvt Ltd-ADVANCE" reached GST Reco with no GSTIN and could never
// match its GSTR-2B line, although the same Tally showed the GSTIN.
//
// Every ledger is read, whatever its group: accountants keep parties under
// Sundry Creditors, its sub-groups, advances, expense groups and more.
type LedgerGSTINs struct {
	byName map[string]string
	byFold map[string]string
}

// Lookup returns the GSTIN on the named ledger, or "". Tally ledger names
// are case-insensitive, so an exact miss falls back to a case-folded match.
func (l *LedgerGSTINs) Lookup(name string) string {
	if l == nil {
		return ""
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if g, ok := l.byName[name]; ok {
		return g
	}
	return l.byFold[strings.ToLower(name)]
}

// Len is the number of ledgers that carry a GSTIN.
func (l *LedgerGSTINs) Len() int {
	if l == nil {
		return 0
	}
	return len(l.byName)
}

// gstinShape accepts a 15-character registration: two-digit state code then
// thirteen alphanumerics. Deliberately permissive (UIN, TDS and TCS
// registrations differ from the regular pattern); it only screens out
// placeholders such as "NA" or "Unregistered".
var gstinShape = regexp.MustCompile(`^[0-9]{2}[A-Z0-9]{13}$`)

func cleanGSTIN(raw string) string {
	g := strings.ToUpper(strings.TrimSpace(raw))
	if gstinShape.MatchString(g) {
		return g
	}
	return ""
}

var ledgerGSTINTmpl = template.Must(template.New("ledger-gstin").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>GstrecoLedgerGstinCollection</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVCURRENTCOMPANY>{{.Company}}</SVCURRENTCOMPANY>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
      <TDL>
        <TDLMESSAGE>
          <COLLECTION NAME="GstrecoLedgerGstinCollection" ISINITIALIZE="Yes">
            <TYPE>Ledger</TYPE>
            <FETCH>NAME, PARTYGSTIN, GSTIN, PARTYGSTNUMBER, LEDGSTREGDETAILS.LIST</FETCH>
          </COLLECTION>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`))

// BuildLedgerGSTINXML renders the request for every ledger's GSTIN. It asks
// for each field Tally has used for a ledger's GSTIN: PARTYGSTIN (Prime
// 3.x), GSTIN / PARTYGSTNUMBER (some Prime 6.x patches) and the dated
// registration list LEDGSTREGDETAILS.LIST (Prime 3.0+). Tally emits empty
// elements for fields it doesn't have.
func BuildLedgerGSTINXML(company string) ([]byte, error) {
	if company == "" {
		return nil, fmt.Errorf("tally: Company is required")
	}
	for _, b := range []byte(company) {
		if b < 0x20 && b != '\t' {
			return nil, fmt.Errorf("tally: Company contains control character 0x%02x", b)
		}
	}
	var buf bytes.Buffer
	if err := ledgerGSTINTmpl.Execute(&buf, struct{ Company string }{company}); err != nil {
		return nil, fmt.Errorf("tally: render ledger-gstin envelope: %w", err)
	}
	return buf.Bytes(), nil
}

type xmlLedgerGSTIN struct {
	NameAttr       string `xml:"NAME,attr"`
	Name           string `xml:"NAME"`
	PartyGSTIN     string `xml:"PARTYGSTIN"`
	GSTIN          string `xml:"GSTIN"`
	PartyGSTNumber string `xml:"PARTYGSTNUMBER"`
	Registrations  []struct {
		ApplicableFrom string `xml:"APPLICABLEFROM"`
		GSTIN          string `xml:"GSTIN"`
	} `xml:"LEDGSTREGDETAILS.LIST"`
}

// gstin picks the ledger's GSTIN: the flat field first (what the masters
// lane already reads), else the latest dated registration.
func (x xmlLedgerGSTIN) gstin() string {
	if g := cleanGSTIN(firstNonEmpty(x.PartyGSTIN, x.GSTIN, x.PartyGSTNumber)); g != "" {
		return g
	}
	latest, latestFrom := "", ""
	for _, r := range x.Registrations {
		g := cleanGSTIN(r.GSTIN)
		if g == "" {
			continue
		}
		from := strings.TrimSpace(r.ApplicableFrom)
		if latest == "" || from >= latestFrom {
			latest, latestFrom = g, from
		}
	}
	return latest
}

// ParseLedgerGSTINs decodes the BuildLedgerGSTINXML response. Ledgers
// without a usable GSTIN are skipped; a ledger that fails to decode is a
// warning, not a failure.
func ParseLedgerGSTINs(raw []byte) (*LedgerGSTINs, []string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil, fmt.Errorf("tally: empty ledger-gstin response")
	}
	raw = prepareXMLForDecode(raw)
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	out := &LedgerGSTINs{byName: map[string]string{}, byFold: map[string]string{}}
	var warnings []string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, warnings, fmt.Errorf("tally: ledger-gstin xml decode: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok || start.Name.Local != "LEDGER" {
			continue
		}
		var x xmlLedgerGSTIN
		if err := dec.DecodeElement(&x, &start); err != nil {
			warnings = append(warnings, fmt.Sprintf("ledger decode: %v", err))
			continue
		}
		name := strings.TrimSpace(firstNonEmpty(x.Name, x.NameAttr))
		g := x.gstin()
		if name == "" || g == "" {
			continue
		}
		out.byName[name] = g
		out.byFold[strings.ToLower(name)] = g
	}
	return out, warnings, nil
}
