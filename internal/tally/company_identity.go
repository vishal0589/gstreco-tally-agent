package tally

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Company identity probe (QA 04-Nov N1).
//
// The built-in "List of Companies" report (BuildCompanyListXML) returns
// company names only, never GUIDs, so the gate's same-name-different-GUID
// refusal could never fire and mappings were stored without a GUID. QA
// validated live (Tally Prime, shared port 9558) that an in-request TDL
// collection of Type Company fetching NAME and GUID does return a GUID.
// Still unproven: whether Type:Company enumerates exactly the companies
// that are loaded on every Prime version. So the agent runs this probe
// next to List of Companies, reports both, and only trusts it as the gate
// when the operator turns enforcement on (config
// company_identity_enforce) — and then only when both probes agree.

// companyIdentityCollection is the TDL collection name we define inline.
// Prefixed so it can never collide with a customer TDL's collection.
const companyIdentityCollection = "GstrecoCompanyIdentity"

// companyIdentityTmpl defines a one-off collection of Type Company with an
// explicit FETCH of NAME and GUID. No SVCURRENTCOMPANY: the point is to see
// every company Tally enumerates, not one picked by name. ISINITIALIZE
// forces a fresh collection rather than a cached one from an earlier
// request on the same Tally session.
var companyIdentityTmpl = []byte(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>` + companyIdentityCollection + `</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
      <TDL>
        <TDLMESSAGE>
          <COLLECTION NAME="` + companyIdentityCollection + `" ISINITIALIZE="Yes">
            <TYPE>Company</TYPE>
            <FETCH>NAME, GUID</FETCH>
          </COLLECTION>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`)

// BuildCompanyIdentityXML renders the Type:Company NAME+GUID request.
func BuildCompanyIdentityXML() []byte {
	return append([]byte(nil), companyIdentityTmpl...)
}

// CompanyIdentityResult is what ParseCompanyIdentity returns: every
// company the collection enumerated, each with the GUID Tally gave it
// (blank when Tally returned the company without one — a warning says so).
type CompanyIdentityResult struct {
	Companies []TallyCompany
	Warnings  []string
}

// ParseCompanyIdentity decodes the Type:Company collection response.
//
// Tally emits each object as <COMPANY NAME="…"> with child <NAME> (or
// <NAME.LIST><NAME>) and <GUID> elements; all three name spellings are
// accepted. A response Tally could not process — no <ENVELOPE>, a
// <LINEERROR>, STATUS 0, an "Unknown Request" <RESPONSE>, or no
// collection at all — is an error, never "no company loaded": a probe
// that did not run must not look like one that found nothing.
func ParseCompanyIdentity(raw []byte) (CompanyIdentityResult, error) {
	var out CompanyIdentityResult
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return out, fmt.Errorf("tally: empty company-identity response")
	}
	head := trimmed
	if len(head) > 512 {
		head = head[:512]
	}
	if !bytes.Contains(bytes.ToUpper(head), []byte("<ENVELOPE")) {
		return out, fmt.Errorf("tally: company-identity response is not a Tally envelope")
	}
	raw = prepareXMLForDecode(raw)
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	sawCollection := false
	seen := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, fmt.Errorf("tally: company-identity xml decode: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch strings.ToUpper(start.Name.Local) {
		case "LINEERROR":
			var s string
			_ = dec.DecodeElement(&s, &start)
			return CompanyIdentityResult{}, fmt.Errorf("tally: company-identity request refused: %s", strings.TrimSpace(s))
		case "STATUS":
			var s string
			if err := dec.DecodeElement(&s, &start); err == nil {
				if v, perr := strconv.Atoi(strings.TrimSpace(s)); perr == nil && v == 0 {
					return CompanyIdentityResult{}, fmt.Errorf("tally: company-identity request returned STATUS 0")
				}
			}
		case "RESPONSE":
			var s string
			if err := dec.DecodeElement(&s, &start); err == nil {
				if strings.Contains(strings.ToLower(s), "unknown request") {
					return CompanyIdentityResult{}, fmt.Errorf("tally: company-identity request not understood: %s", strings.TrimSpace(s))
				}
			}
		case "COLLECTION", "DATA":
			sawCollection = true
		case "COMPANY":
			sawCollection = true
			var x xmlIdentityCompany
			if err := dec.DecodeElement(&x, &start); err != nil {
				out.Warnings = append(out.Warnings, fmt.Sprintf("company decode: %v", err))
				continue
			}
			name := strings.TrimSpace(firstNonEmpty(x.NameElem, x.NameList, x.NameAttr))
			guid := strings.TrimSpace(x.GUID)
			if name == "" {
				if guid != "" {
					out.Warnings = append(out.Warnings, fmt.Sprintf("company with GUID %s has no name; ignored", guid))
				}
				continue
			}
			key := name + "\x00" + strings.ToLower(guid)
			if seen[key] {
				continue
			}
			seen[key] = true
			if guid == "" {
				out.Warnings = append(out.Warnings, fmt.Sprintf("company %q returned without a GUID", name))
			}
			out.Companies = append(out.Companies, TallyCompany{Name: name, GUID: guid})
		}
	}
	if !sawCollection {
		return CompanyIdentityResult{}, fmt.Errorf("tally: company-identity response has no collection")
	}
	return out, nil
}

type xmlIdentityCompany struct {
	NameAttr string `xml:"NAME,attr"`
	NameElem string `xml:"NAME"`
	NameList string `xml:"NAME.LIST>NAME"`
	GUID     string `xml:"GUID"`
}
