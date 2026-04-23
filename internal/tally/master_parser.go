package tally

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// MasterResult is what ParseMastersV3 hands back. Like ParseDayBookV3, it
// carries warnings per-row so one broken ledger entry in a 5 000-ledger
// master export doesn't fail the whole sync.
type MasterResult struct {
	Masters  []RawMaster
	Status   int
	Warnings []string
}

// ParseMastersV3 decodes a Tally Prime 3.x master-ledger response into
// RawMasters. Uses the same permissive token-walker as ParseDayBookV3 so
// wrapper elements (TALLYMESSAGE, IMPORTDATA) don't tie the parser to a
// specific envelope shape.
//
// Ledgers missing both NAME and GUID are dropped with a warning — without
// either we can't join back to the voucher's party-ledger reference on
// later ingest runs.
func ParseMastersV3(raw []byte) (MasterResult, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return MasterResult{}, fmt.Errorf("tally: empty masters response")
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	result := MasterResult{Status: -1}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("tally: masters xml decode: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "STATUS":
			var s string
			if err := dec.DecodeElement(&s, &start); err == nil {
				if v, perr := strconv.Atoi(strings.TrimSpace(s)); perr == nil {
					result.Status = v
				}
			}
		case "LEDGER":
			var x xmlLedger
			if err := dec.DecodeElement(&x, &start); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("ledger decode: %v", err))
				continue
			}
			m := x.toRaw()
			if m.Name == "" && m.GUID == "" {
				result.Warnings = append(result.Warnings, "ledger dropped: missing both NAME and GUID")
				continue
			}
			result.Masters = append(result.Masters, m)
		}
	}
	return result, nil
}

// CompanyListResult is the company-list parse output. Separated from the
// master parser so callers can surface "no companies loaded" distinctly
// from "no vendors found".
type CompanyListResult struct {
	Companies []TallyCompany
	Warnings  []string
}

// ParseCompanyListV3 decodes the list-of-companies XML. Tally's shape for
// this endpoint is unusually varied across Prime patches — walker approach
// finds COMPANY / COMPANYONDISK / REMOTECMPINFO elements regardless of how
// they're nested.
func ParseCompanyListV3(raw []byte) (CompanyListResult, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return CompanyListResult{}, fmt.Errorf("tally: empty company-list response")
	}
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	seen := map[string]bool{}
	var out CompanyListResult
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, fmt.Errorf("tally: company-list xml decode: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		// Tally emits company info under several element names depending on
		// which report serves the request. Handle all the known forms so the
		// agent doesn't care which Prime patch answered.
		switch start.Name.Local {
		case "COMPANY", "COMPANYONDISK", "REMOTECMPINFO":
			var x xmlCompanyRecord
			if err := dec.DecodeElement(&x, &start); err != nil {
				out.Warnings = append(out.Warnings, fmt.Sprintf("company decode: %v", err))
				continue
			}
			name := strings.TrimSpace(x.firstNonEmptyName())
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out.Companies = append(out.Companies, TallyCompany{
				Name: name,
				GUID: strings.TrimSpace(x.GUID),
			})
		}
	}
	return out, nil
}

// ParseVersion extracts the Tally Prime major version from a $$Version
// response. Returns VersionV3 / VersionV4 / VersionUnknown. The function is
// deliberately small — newer Tally versions will need a new parser path
// wired in A7, and we want that change to be a single switch case here.
func ParseVersion(raw []byte) (Version, string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return VersionUnknown, "", fmt.Errorf("tally: empty version response")
	}
	// Tally's $$Version replies with something like "Release 3.0.1" or
	// "Release 4.1" in a variety of wrapper tags (<VERSIONNUMBER>,
	// <VERSION>, plain text inside <ENVELOPE>). Scan tokens for a string
	// containing "release" and extract the number.
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	var full string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return VersionUnknown, "", fmt.Errorf("tally: version xml decode: %w", err)
		}
		if cd, ok := tok.(xml.CharData); ok {
			text := strings.TrimSpace(string(cd))
			if strings.Contains(strings.ToLower(text), "release") {
				full = text
				break
			}
		}
	}
	if full == "" {
		return VersionUnknown, "", fmt.Errorf("tally: no version string in response")
	}
	// Extract major number. "Release 3.0.1" → 3; "Release 4.0" → 4.
	// Walk chars until we hit a digit, then collect contiguous digits.
	maj := 0
	for i := 0; i < len(full); i++ {
		if full[i] >= '0' && full[i] <= '9' {
			for i < len(full) && full[i] >= '0' && full[i] <= '9' {
				maj = maj*10 + int(full[i]-'0')
				i++
			}
			break
		}
	}
	switch maj {
	case 3:
		return VersionV3, full, nil
	case 4:
		return VersionV4, full, nil
	default:
		return VersionUnknown, full, nil
	}
}

// --- private XML shapes ---

type xmlLedger struct {
	Name             string              `xml:"NAME"`
	GUID             string              `xml:"GUID"`
	AlterID          string              `xml:"ALTERID"`
	Parent           string              `xml:"PARENT"`
	GSTIN            string              `xml:"PARTYGSTIN"`
	GSTRegType       string              `xml:"GSTREGISTRATIONTYPE"`
	StateName        string              `xml:"STATENAME"`
	Email            string              `xml:"EMAIL"`
	Phone            string              `xml:"LEDGERPHONE"`
	Mobile           string              `xml:"LEDGERMOBILE"`
	Contact          string              `xml:"LEDGERCONTACT"`
	MailingNames     []string            `xml:"MAILINGNAME.LIST>MAILINGNAME"`
	LedgerMailNames  []string            `xml:"LEDGERMAILINGNAME.LIST>MAILINGNAME"`
	AddressLines     []string            `xml:"ADDRESS.LIST>ADDRESS"`
}

func (x xmlLedger) toRaw() RawMaster {
	m := RawMaster{
		Name:                strings.TrimSpace(x.Name),
		GUID:                strings.TrimSpace(x.GUID),
		Parent:              strings.TrimSpace(x.Parent),
		GSTIN:               strings.ToUpper(strings.TrimSpace(x.GSTIN)),
		GSTRegistrationType: strings.TrimSpace(x.GSTRegType),
		StateName:           strings.TrimSpace(x.StateName),
		Email:               strings.TrimSpace(x.Email),
		Phone:               strings.TrimSpace(x.Phone),
		Mobile:              strings.TrimSpace(x.Mobile),
		Contact:             strings.TrimSpace(x.Contact),
	}
	if x.AlterID != "" {
		if v, err := strconv.ParseInt(strings.TrimSpace(x.AlterID), 10, 64); err == nil {
			m.AlterID = v
		}
	}
	// Trade name: prefer MAILINGNAME (first non-empty), fall back to
	// LEDGERMAILINGNAME (older Prime variant), then ledger Name.
	for _, n := range x.MailingNames {
		if n = strings.TrimSpace(n); n != "" {
			m.TradeName = n
			break
		}
	}
	if m.TradeName == "" {
		for _, n := range x.LedgerMailNames {
			if n = strings.TrimSpace(n); n != "" {
				m.TradeName = n
				break
			}
		}
	}
	if m.TradeName == "" {
		m.TradeName = m.Name
	}
	// Address: concatenate non-empty lines with newlines.
	lines := make([]string, 0, len(x.AddressLines))
	for _, a := range x.AddressLines {
		if a = strings.TrimSpace(a); a != "" {
			lines = append(lines, a)
		}
	}
	m.Address = strings.Join(lines, "\n")
	return m
}

type xmlCompanyRecord struct {
	// Tally puts the company name in several possible places depending on
	// the patch version — capture all we've seen and return the first
	// non-empty one.
	Name            string `xml:"NAME"`
	CompanyName     string `xml:"COMPANYNAME"`
	BasicCompanyName string `xml:"BASICCOMPANYNAME"`
	GUID            string `xml:"GUID"`
	RemoteInfoName  string `xml:"REMOTECMPINFO.LIST>NAME"`
}

func (x xmlCompanyRecord) firstNonEmptyName() string {
	for _, n := range []string{x.Name, x.CompanyName, x.BasicCompanyName, x.RemoteInfoName} {
		if s := strings.TrimSpace(n); s != "" {
			return s
		}
	}
	return ""
}
