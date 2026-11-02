package tally

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// LedgerDirectory answers what the normalisers need from the ledger masters
// when a voucher itself is silent about its party's GSTIN.
type LedgerDirectory interface {
	// GSTIN returns the GSTIN the named ledger was registered under on the
	// given date, or "".
	GSTIN(ledgerName string, on time.Time) string
	// IsNonParty reports whether the ledger sits under a group that is never
	// a trading party: bank, cash, duties and taxes, branches.
	IsNonParty(ledgerName string) bool
}

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
	byName map[string]*ledgerRecord
	byFold map[string]*ledgerRecord
	// dutiesGroup is the company's own name for Tally's reserved Duties &
	// Taxes group when it was renamed, once ApplyGroups has run.
	dutiesGroup string
}

type ledgerRecord struct {
	// flat is the PARTYGSTIN / GSTIN / PARTYGSTNUMBER value.
	flat string
	// registrations are the dated GST registrations (LEDGSTREGDETAILS.LIST),
	// oldest first. An entry with an empty GSTIN means "unregistered from".
	registrations []ledgerRegistration
	parent        string
	// primary is the top-level group the parent chain ends in, once
	// ApplyGroupTree has run.
	primary  string
	nonParty bool
	// duty is true when the ledger's group chain reaches Tally's reserved
	// Duties & Taxes group (by name or, when renamed, by its reserved name).
	duty bool
}

type ledgerRegistration struct {
	from  string // YYYYMMDD, "" when Tally gave no date
	gstin string
}

var (
	_ LedgerDirectory = (*LedgerGSTINs)(nil)
	_ LedgerGroups    = (*LedgerGSTINs)(nil)
)

func (l *LedgerGSTINs) record(name string) *ledgerRecord {
	if l == nil {
		return nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if r, ok := l.byName[name]; ok {
		return r
	}
	// Tally ledger names are case-insensitive.
	return l.byFold[strings.ToLower(name)]
}

// GSTIN returns the GSTIN the ledger was registered under on the given date.
// Dated registrations decide when the ledger has them: the latest one
// effective on that date, which may say "unregistered". A ledger with only
// later registrations was not registered yet. Without dated registrations
// the flat GSTIN field applies.
func (l *LedgerGSTINs) GSTIN(name string, on time.Time) string {
	r := l.record(name)
	if r == nil {
		return ""
	}
	if len(r.registrations) == 0 {
		return r.flat
	}
	day := on.Format("20060102")
	effective, found := "", false
	for _, reg := range r.registrations {
		if reg.from == "" || on.IsZero() || reg.from <= day {
			effective, found = reg.gstin, true
		}
	}
	if !found {
		return ""
	}
	return effective
}

// IsNonParty reports whether the ledger's group chain includes a group that
// is never a trading party.
func (l *LedgerGSTINs) IsNonParty(name string) bool {
	r := l.record(name)
	return r != nil && r.nonParty
}

// ParentGroup is the ledger's group in Tally, or "".
func (l *LedgerGSTINs) ParentGroup(name string) string {
	if r := l.record(name); r != nil {
		return r.parent
	}
	return ""
}

// PrimaryGroup is the top-level group the ledger's group chain ends in
// ("Purchase Accounts", "Indirect Expenses"), or "" before ApplyGroupTree.
func (l *LedgerGSTINs) PrimaryGroup(name string) string {
	if r := l.record(name); r != nil {
		return r.primary
	}
	return ""
}

// Len is the number of ledgers that carry a GSTIN.
func (l *LedgerGSTINs) Len() int {
	if l == nil {
		return 0
	}
	n := 0
	for _, r := range l.byName {
		if r.flat != "" || len(r.registrations) > 0 {
			n++
		}
	}
	return n
}

// nonPartyGroups are Tally's reserved groups whose ledgers are never a
// trading party, matched anywhere in a ledger's group chain.
var nonPartyGroups = map[string]bool{
	"bank accounts":      true,
	"bank od a/c":        true,
	"bank occ a/c":       true,
	"cash-in-hand":       true,
	"duties & taxes":     true,
	"branch / divisions": true,
}

// GroupTree is a company's groups: each group's parent and, for Tally's
// reserved groups, the reserved name (RESERVEDNAME), which stays "Duties &
// Taxes" when a company renames the group.
type GroupTree struct {
	Parents  map[string]string
	Reserved map[string]string
}

// ApplyGroupTree marks ledgers whose group chain reaches a non-party group
// and records the top-level group each chain ends in. groups maps each group
// name to its parent.
func (l *LedgerGSTINs) ApplyGroupTree(groups map[string]string) {
	l.ApplyGroups(GroupTree{Parents: groups})
}

// ApplyGroups is ApplyGroupTree with reserved names: a renamed reserved group
// counts as the reserved group (QA v27 L1), so a company that renamed Duties
// & Taxes still has its duty ledgers recognised.
func (l *LedgerGSTINs) ApplyGroups(tree GroupTree) {
	if l == nil {
		return
	}
	fold := make(map[string]string, len(tree.Parents))
	spelled := make(map[string]string, len(tree.Parents))
	reserved := make(map[string]string, len(tree.Reserved))
	for name, parent := range tree.Parents {
		key := strings.ToLower(strings.TrimSpace(name))
		fold[key] = strings.ToLower(strings.TrimSpace(parent))
		spelled[key] = strings.TrimSpace(name)
	}
	for name, res := range tree.Reserved {
		key := strings.ToLower(strings.TrimSpace(name))
		res = strings.TrimSpace(res)
		if key == "" || res == "" {
			continue
		}
		reserved[key] = res
		if strings.EqualFold(res, dutiesAndTaxesGroup) && !strings.EqualFold(strings.TrimSpace(name), dutiesAndTaxesGroup) {
			l.dutiesGroup = strings.TrimSpace(name)
		}
	}
	// canonical is the reserved name a group stands for, else its own name.
	canonical := func(g string) string {
		if res, ok := reserved[g]; ok {
			return strings.ToLower(res)
		}
		return g
	}
	for _, r := range l.byName {
		g := strings.ToLower(strings.TrimSpace(r.parent))
		for hops := 0; g != "" && hops < 32; hops++ {
			c := canonical(g)
			if nonPartyGroups[c] {
				r.nonParty = true
			}
			if c == strings.ToLower(dutiesAndTaxesGroup) {
				r.duty = true
			}
			parent, known := fold[g]
			if !known {
				break
			}
			if isPrimaryGroupParent(parent) {
				r.primary = spelled[g]
				if res, ok := reserved[g]; ok {
					r.primary = res
				}
				break
			}
			g = parent
		}
	}
}

// DutiesAndTaxesGroup is the name of the company's Duties & Taxes group as
// the group tree spells it: "Duties & Taxes", or the company's own name for
// it when renamed.
func (l *LedgerGSTINs) DutiesAndTaxesGroup() string {
	if l == nil || l.dutiesGroup == "" {
		return dutiesAndTaxesGroup
	}
	return l.dutiesGroup
}

// DutyLedgersByGroup is every ledger whose group chain reaches Duties &
// Taxes, with its parent group: Tally's own grouping, not the ledger's name.
// It carries no type of duty or duty head; those come only from the duty
// ledger masters.
func (l *LedgerGSTINs) DutyLedgersByGroup() []DutyLedger {
	if l == nil {
		return nil
	}
	var out []DutyLedger
	for name, r := range l.byName {
		if !r.duty {
			continue
		}
		out = append(out, DutyLedger{Name: name, Parent: r.parent})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// isPrimaryGroupParent is the parent Tally gives a top-level group: none, or
// its reserved "Primary" (exported with a leading control character).
func isPrimaryGroupParent(parent string) bool {
	p := strings.Trim(parent, "\x04 ")
	return p == "" || p == "primary"
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

var digitsOnly = regexp.MustCompile(`[^0-9]`)

// registrationDay normalises a Tally date to YYYYMMDD, or "" when it can't.
func registrationDay(raw string) string {
	d := digitsOnly.ReplaceAllString(raw, "")
	if len(d) == 8 {
		return d
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
            <FETCH>NAME, PARENT, PARTYGSTIN, GSTIN, PARTYGSTNUMBER, LEDGSTREGDETAILS.LIST</FETCH>
          </COLLECTION>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`))

var groupTreeTmpl = template.Must(template.New("group-tree").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>GstrecoGroupTreeCollection</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVCURRENTCOMPANY>{{.Company}}</SVCURRENTCOMPANY>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
      <TDL>
        <TDLMESSAGE>
          <COLLECTION NAME="GstrecoGroupTreeCollection" ISINITIALIZE="Yes">
            <TYPE>Group</TYPE>
            <FETCH>NAME, PARENT, RESERVEDNAME</FETCH>
          </COLLECTION>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`))

func renderCompanyEnvelope(tmpl *template.Template, company string) ([]byte, error) {
	if company == "" {
		return nil, fmt.Errorf("tally: Company is required")
	}
	for _, b := range []byte(company) {
		if b < 0x20 && b != '\t' {
			return nil, fmt.Errorf("tally: Company contains control character 0x%02x", b)
		}
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, struct{ Company string }{company}); err != nil {
		return nil, fmt.Errorf("tally: render %s envelope: %w", tmpl.Name(), err)
	}
	return buf.Bytes(), nil
}

// BuildLedgerGSTINXML renders the request for every ledger's GSTIN and
// parent group. It asks for each field Tally has used for a ledger's GSTIN:
// PARTYGSTIN (Prime 3.x), GSTIN / PARTYGSTNUMBER (some Prime 6.x patches)
// and the dated registration list LEDGSTREGDETAILS.LIST (Prime 3.0+). Tally
// emits empty elements for fields it doesn't have.
func BuildLedgerGSTINXML(company string) ([]byte, error) {
	return renderCompanyEnvelope(ledgerGSTINTmpl, company)
}

// BuildGroupTreeXML renders the request for every group's parent.
func BuildGroupTreeXML(company string) ([]byte, error) {
	return renderCompanyEnvelope(groupTreeTmpl, company)
}

type xmlLedgerGSTIN struct {
	NameAttr       string `xml:"NAME,attr"`
	Name           string `xml:"NAME"`
	Parent         string `xml:"PARENT"`
	PartyGSTIN     string `xml:"PARTYGSTIN"`
	GSTIN          string `xml:"GSTIN"`
	PartyGSTNumber string `xml:"PARTYGSTNUMBER"`
	Registrations  []struct {
		ApplicableFrom string `xml:"APPLICABLEFROM"`
		GSTIN          string `xml:"GSTIN"`
	} `xml:"LEDGSTREGDETAILS.LIST"`
}

type xmlGroup struct {
	NameAttr         string `xml:"NAME,attr"`
	Name             string `xml:"NAME"`
	Parent           string `xml:"PARENT"`
	ReservedNameAttr string `xml:"RESERVEDNAME,attr"`
	ReservedName     string `xml:"RESERVEDNAME"`
}

// ErrExportRefused is Tally answering a master export with an error (STATUS 0
// or a LINEERROR, e.g. the company is not open): the masters were not
// fetched, as opposed to a reply that could not be parsed.
var ErrExportRefused = errors.New("tally: export failed")

// walkTallyExport decodes a Tally export, calls onElement for each element
// named elem, and fails on Tally's own error replies (STATUS 0, LINEERROR):
// an HTTP 200 carrying an error must not read as an empty, successful
// export.
func walkTallyExport(raw []byte, elem string, onElement func(*xml.Decoder, *xml.StartElement) error) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return fmt.Errorf("tally: empty response")
	}
	raw = prepareXMLForDecode(raw)
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tally: xml decode: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "STATUS":
			var s string
			if err := dec.DecodeElement(&s, &start); err == nil {
				if v, perr := strconv.Atoi(strings.TrimSpace(s)); perr == nil && v == 0 {
					return fmt.Errorf("%w (STATUS 0)", ErrExportRefused)
				}
			}
		case "LINEERROR":
			var s string
			_ = dec.DecodeElement(&s, &start)
			return fmt.Errorf("%w: %s", ErrExportRefused, strings.TrimSpace(s))
		case elem:
			if err := onElement(dec, &start); err != nil {
				return err
			}
		}
	}
}

// ParseLedgerGSTINs decodes the BuildLedgerGSTINXML response. Ledgers
// without any GSTIN are kept only for their group (bank detection); a
// ledger that fails to decode is a warning, not a failure.
func ParseLedgerGSTINs(raw []byte) (*LedgerGSTINs, []string, error) {
	out := &LedgerGSTINs{byName: map[string]*ledgerRecord{}, byFold: map[string]*ledgerRecord{}}
	var warnings []string
	err := walkTallyExport(raw, "LEDGER", func(dec *xml.Decoder, start *xml.StartElement) error {
		var x xmlLedgerGSTIN
		if err := dec.DecodeElement(&x, start); err != nil {
			warnings = append(warnings, fmt.Sprintf("ledger decode: %v", err))
			return nil
		}
		name := strings.TrimSpace(firstNonEmpty(x.Name, x.NameAttr))
		if name == "" {
			return nil
		}
		r := &ledgerRecord{
			flat:   cleanGSTIN(firstNonEmpty(x.PartyGSTIN, x.GSTIN, x.PartyGSTNumber)),
			parent: strings.TrimSpace(x.Parent),
		}
		for _, reg := range x.Registrations {
			raw := strings.TrimSpace(reg.GSTIN)
			g := cleanGSTIN(raw)
			if raw != "" && g == "" {
				continue // a malformed value, not an "unregistered" marker
			}
			r.registrations = append(r.registrations, ledgerRegistration{from: registrationDay(reg.ApplicableFrom), gstin: g})
		}
		sort.SliceStable(r.registrations, func(i, j int) bool { return r.registrations[i].from < r.registrations[j].from })
		// A dated list with no GSTIN anywhere carries no information.
		hasRegistered := false
		for _, reg := range r.registrations {
			if reg.gstin != "" {
				hasRegistered = true
			}
		}
		if !hasRegistered {
			r.registrations = nil
		}
		out.byName[name] = r
		out.byFold[strings.ToLower(name)] = r
		return nil
	})
	if err != nil {
		return nil, warnings, err
	}
	return out, warnings, nil
}

// ParseGroupTree decodes the BuildGroupTreeXML response into group → parent.
func ParseGroupTree(raw []byte) (map[string]string, error) {
	tree, err := ParseGroups(raw)
	if err != nil {
		return nil, err
	}
	return tree.Parents, nil
}

// ParseGroups decodes the BuildGroupTreeXML response with each reserved
// group's reserved name.
func ParseGroups(raw []byte) (GroupTree, error) {
	tree := GroupTree{Parents: map[string]string{}, Reserved: map[string]string{}}
	err := walkTallyExport(raw, "GROUP", func(dec *xml.Decoder, start *xml.StartElement) error {
		var x xmlGroup
		if err := dec.DecodeElement(&x, start); err != nil {
			return nil
		}
		if name := strings.TrimSpace(firstNonEmpty(x.Name, x.NameAttr)); name != "" {
			tree.Parents[name] = strings.TrimSpace(x.Parent)
			if res := strings.TrimSpace(firstNonEmpty(x.ReservedName, x.ReservedNameAttr)); res != "" {
				tree.Reserved[name] = res
			}
		}
		return nil
	})
	if err != nil {
		return GroupTree{}, err
	}
	return tree, nil
}
