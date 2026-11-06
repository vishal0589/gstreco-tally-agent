package tally

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"html/template"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The tax_ledger_voucher lane (QA 05-Nov, probe KSS-Tally-ReadOnly-v13):
// every voucher of a month that touches a Duties & Taxes ledger, with every
// leg exactly as Tally returned it, so the server can tie each tax ledger's
// voucher total to the month's closing − opening.

// TaxLedgerVoucherRequest fetches every voucher of one company and window.
type TaxLedgerVoucherRequest struct {
	Company string
	From    time.Time
	To      time.Time
}

// taxLedgerVoucherTmpl is the probe's one TDL collection export of Voucher.
// No FILTER: every voucher type is read, and the agent selects the ones that
// touch the tax ledger set. Inventory is not needed for the tie-out.
var taxLedgerVoucherTmpl = template.Must(template.New("tax-ledger-vouchers").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>GstrecoTaxLedgerVoucherCollection</ID>
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
          <COLLECTION NAME="GstrecoTaxLedgerVoucherCollection" ISINITIALIZE="Yes">
            <TYPE>Voucher</TYPE>
            <FETCH>GUID, MASTERID, ALTERID, DATE, VOUCHERTYPENAME, VOUCHERNUMBER, REFERENCE, PARTYLEDGERNAME, PARTYGSTIN, PLACEOFSUPPLY, ISINVOICE, ISCANCELLED, ISOPTIONAL, ISRCMAPPLICABLE, NARRATION</FETCH>
            <FETCH>ALLLEDGERENTRIES.LEDGERNAME, ALLLEDGERENTRIES.AMOUNT, ALLLEDGERENTRIES.GSTCLASS, ALLLEDGERENTRIES.BILLALLOCATIONS</FETCH>
          </COLLECTION>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`))

// BuildTaxLedgerVoucherXML renders the voucher request for one window.
func BuildTaxLedgerVoucherXML(r TaxLedgerVoucherRequest) ([]byte, error) {
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
	params := struct{ Company, From, To string }{
		Company: r.Company,
		From:    FormatTallyDate(r.From),
		To:      FormatTallyDate(r.To),
	}
	var buf bytes.Buffer
	if err := taxLedgerVoucherTmpl.Execute(&buf, params); err != nil {
		return nil, fmt.Errorf("tally: render tax-ledger voucher envelope: %w", err)
	}
	return buf.Bytes(), nil
}

// TaxLedgerVoucherLegRaw is one ledger leg exactly as returned.
type TaxLedgerVoucherLegRaw struct {
	// LedgerName is the raw element text (not trimmed).
	LedgerName string
	// Amount is the raw AMOUNT text (not trimmed).
	Amount   string
	GSTClass string
}

// TaxLedgerVoucherRaw is one VOUCHER of the reply. Every string is the
// element's text as returned; the selection step trims where it compares.
type TaxLedgerVoucherRaw struct {
	GUID, MasterID, AlterID, Date      string
	VoucherTypeName, VoucherNumber     string
	Reference, PartyLedgerName         string
	PartyGSTIN, PlaceOfSupply          string
	IsInvoice, IsCancelled, IsOptional bool
	IsRCMApplicable                    bool
	Legs                               []TaxLedgerVoucherLegRaw
}

// TaxLedgerVoucherReply is the parsed voucher export.
type TaxLedgerVoucherReply struct {
	Vouchers []TaxLedgerVoucherRaw
	// Status is Tally's <STATUS> (-1 when the reply carries none).
	Status int
}

// ErrTaxLedgerVoucherReply marks a voucher reply the agent refuses to read.
var ErrTaxLedgerVoucherReply = errors.New("tally: tax-ledger voucher reply unreadable")

// udfNamespace is what the probe declares for Tally's undeclared UDF: prefix.
const udfNamespace = "TallyUDF"

// coreVoucherElements must never carry a namespace: a namespaced VOUCHER or
// AMOUNT is not something the agent knows how to read, so it stops.
var coreVoucherElements = map[string]bool{
	"VOUCHER": true, "GUID": true, "MASTERID": true, "ALTERID": true, "DATE": true,
	"VOUCHERTYPENAME": true, "VOUCHERNUMBER": true, "REFERENCE": true,
	"PARTYLEDGERNAME": true, "PARTYGSTIN": true, "PLACEOFSUPPLY": true,
	"ISINVOICE": true, "ISCANCELLED": true, "ISOPTIONAL": true, "ISRCMAPPLICABLE": true,
	"ALLLEDGERENTRIES.LIST": true, "LEDGERENTRIES.LIST": true,
	"LEDGERNAME": true, "AMOUNT": true, "GSTCLASS": true,
}

// ParseTaxLedgerVoucherReply reads the voucher export without repairing it
// (the probe's rules): UTF-16 is decoded when the reply starts with a BOM or
// its first or second byte is zero; character references to control
// characters (&#4;) are accepted, literal control bytes are not; Tally's
// undeclared UDF: prefix is read as if declared, any other undeclared prefix
// is refused; a LINEERROR or STATUS 0 is an error; a namespaced core voucher
// element stops the parse. Any error wraps ErrTaxLedgerVoucherReply.
func ParseTaxLedgerVoucherReply(raw []byte) (TaxLedgerVoucherReply, error) {
	out := TaxLedgerVoucherReply{Status: -1}
	text, err := decodeVoucherReplyText(raw)
	if err != nil {
		return out, err
	}
	if len(bytes.TrimSpace(text)) == 0 {
		return out, fmt.Errorf("%w: empty reply", ErrTaxLedgerVoucherReply)
	}
	text, err = protectControlCharRefs(text)
	if err != nil {
		return out, err
	}
	dec := xml.NewDecoder(bytes.NewReader(text))
	dec.Strict = true
	// The UTF-16 reply is already UTF-8 here; an encoding="..." declaration
	// is not a reason to transcode again.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	ns := &nsScopes{}
	fail := func(format string, a ...any) (TaxLedgerVoucherReply, error) {
		return out, fmt.Errorf("%w: "+format, append([]any{ErrTaxLedgerVoucherReply}, a...)...)
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail("xml: %v", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if err := ns.push(t); err != nil {
				return fail("%v", err)
			}
			switch t.Name.Local {
			case "STATUS":
				s, err := readText(dec, ns)
				if err != nil {
					return fail("%v", err)
				}
				if v, perr := parseIntStrict(strings.TrimSpace(s)); perr == nil {
					out.Status = v
					if v == 0 {
						return fail("Tally answered STATUS 0")
					}
				}
			case "LINEERROR":
				s, _ := readText(dec, ns)
				return fail("Tally LINEERROR: %s", strings.TrimSpace(s))
			case "VOUCHER":
				v, err := readVoucher(dec, ns)
				if err != nil {
					return fail("%v", err)
				}
				out.Vouchers = append(out.Vouchers, v)
			}
		case xml.EndElement:
			ns.pop()
		}
	}
	return out, nil
}

// decodeVoucherReplyText returns the reply as UTF-8. A UTF-16 BOM, or a zero
// first or second byte, means UTF-16 (BE / LE); a UTF-8 BOM is dropped.
// Invalid UTF-8 is refused, not repaired.
func decodeVoucherReplyText(raw []byte) ([]byte, error) {
	if len(raw) >= 2 {
		switch {
		case raw[0] == 0xff && raw[1] == 0xfe:
			return utf16ToUTF8(raw[2:], false)
		case raw[0] == 0xfe && raw[1] == 0xff:
			return utf16ToUTF8(raw[2:], true)
		case raw[1] == 0x00:
			return utf16ToUTF8(raw, false)
		case raw[0] == 0x00:
			return utf16ToUTF8(raw, true)
		}
	}
	raw = bytes.TrimPrefix(raw, []byte{0xef, 0xbb, 0xbf})
	if !utf8.Valid(raw) {
		return nil, fmt.Errorf("%w: reply is not valid UTF-8", ErrTaxLedgerVoucherReply)
	}
	return raw, nil
}

func utf16ToUTF8(b []byte, bigEndian bool) ([]byte, error) {
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("%w: UTF-16 reply has an odd byte count", ErrTaxLedgerVoucherReply)
	}
	var buf bytes.Buffer
	buf.Grow(len(b) / 2)
	for i := 0; i < len(b); i += 2 {
		var u uint16
		if bigEndian {
			u = uint16(b[i])<<8 | uint16(b[i+1])
		} else {
			u = uint16(b[i+1])<<8 | uint16(b[i])
		}
		r := rune(u)
		if u >= 0xd800 && u < 0xdc00 {
			if i+3 >= len(b) {
				return nil, fmt.Errorf("%w: truncated UTF-16 surrogate", ErrTaxLedgerVoucherReply)
			}
			var lo uint16
			if bigEndian {
				lo = uint16(b[i+2])<<8 | uint16(b[i+3])
			} else {
				lo = uint16(b[i+3])<<8 | uint16(b[i+2])
			}
			if lo < 0xdc00 || lo > 0xdfff {
				return nil, fmt.Errorf("%w: invalid UTF-16 surrogate pair", ErrTaxLedgerVoucherReply)
			}
			r = (rune(u)-0xd800)<<10 + (rune(lo) - 0xdc00) + 0x10000
			i += 2
		} else if u >= 0xdc00 && u <= 0xdfff {
			return nil, fmt.Errorf("%w: unpaired UTF-16 surrogate", ErrTaxLedgerVoucherReply)
		}
		buf.WriteRune(r)
	}
	return buf.Bytes(), nil
}

// encoding/xml refuses a character reference to a control character
// (&#4;), which Tally writes in GSTCLASS and elsewhere. The probe reads such
// references as the character itself (XmlReader CheckCharacters=false), so
// the agent does the same without losing anything: each reference to
// U+0000..U+001F (other than tab, LF, CR) becomes a sentinel rune in plane
// 15 before decoding and the original character again in every value read.
// Literal control bytes are still refused, and so is a reply that already
// holds a sentinel rune.
const controlSentinelBase = 0xF0000

func isSentinel(r rune) bool { return r >= controlSentinelBase && r < controlSentinelBase+0x20 }

func protectControlCharRefs(text []byte) ([]byte, error) {
	if !bytes.Contains(text, []byte("&#")) {
		if bytes.ContainsFunc(text, isSentinel) {
			return nil, fmt.Errorf("%w: reply holds a reserved plane-15 character", ErrTaxLedgerVoucherReply)
		}
		return text, nil
	}
	if bytes.ContainsFunc(text, isSentinel) {
		return nil, fmt.Errorf("%w: reply holds a reserved plane-15 character", ErrTaxLedgerVoucherReply)
	}
	var out bytes.Buffer
	out.Grow(len(text))
	for i := 0; i < len(text); {
		rest := text[i:]
		switch {
		case bytes.HasPrefix(rest, []byte("<![CDATA[")):
			end := bytes.Index(rest, []byte("]]>"))
			if end < 0 {
				out.Write(rest)
				return out.Bytes(), nil
			}
			out.Write(rest[:end+3])
			i += end + 3
			continue
		case bytes.HasPrefix(rest, []byte("<!--")):
			end := bytes.Index(rest, []byte("-->"))
			if end < 0 {
				out.Write(rest)
				return out.Bytes(), nil
			}
			out.Write(rest[:end+3])
			i += end + 3
			continue
		case bytes.HasPrefix(rest, []byte("&#")):
			if end := bytes.IndexByte(rest, ';'); end > 2 && end < 12 {
				if n, ok := charRefValue(string(rest[2:end])); ok && n < 0x20 && n != 0x09 && n != 0x0a && n != 0x0d {
					out.WriteRune(rune(controlSentinelBase + n))
					i += end + 1
					continue
				}
			}
		}
		out.WriteByte(text[i])
		i++
	}
	return out.Bytes(), nil
}

func charRefValue(s string) (int, bool) {
	base := 10
	if strings.HasPrefix(s, "x") {
		base, s = 16, s[1:]
	}
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range strings.ToLower(s) {
		var d int
		switch {
		case r >= '0' && r <= '9':
			d = int(r - '0')
		case base == 16 && r >= 'a' && r <= 'f':
			d = int(r-'a') + 10
		default:
			return 0, false
		}
		n = n*base + d
		if n > 0x10FFFF {
			return 0, false
		}
	}
	return n, true
}

func restoreControlChars(s string) string {
	if !strings.ContainsFunc(s, isSentinel) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isSentinel(r) {
			return r - controlSentinelBase
		}
		return r
	}, s)
}

// nsScopes tracks the namespace prefixes declared in scope. encoding/xml
// replaces a declared prefix with its URI and leaves an undeclared one as
// the bare prefix, so a Space that is no in-scope URI is an undeclared
// prefix: UDF is read as if declared, anything else is refused.
type nsScopes struct {
	stack []map[string]bool // URIs declared at each open element
}

func (n *nsScopes) push(t xml.StartElement) error {
	decl := map[string]bool{}
	for _, a := range t.Attr {
		if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
			decl[a.Value] = true
		}
	}
	n.stack = append(n.stack, decl)
	if err := n.check(t.Name, true); err != nil {
		return err
	}
	for _, a := range t.Attr {
		if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
			continue
		}
		if err := n.check(a.Name, false); err != nil {
			return err
		}
	}
	return nil
}

func (n *nsScopes) pop() {
	if len(n.stack) > 0 {
		n.stack = n.stack[:len(n.stack)-1]
	}
}

func (n *nsScopes) declared(uri string) bool {
	for i := len(n.stack) - 1; i >= 0; i-- {
		if n.stack[i][uri] {
			return true
		}
	}
	return false
}

func (n *nsScopes) check(name xml.Name, element bool) error {
	if name.Space == "" || name.Space == "http://www.w3.org/XML/1998/namespace" {
		return nil
	}
	if element && coreVoucherElements[name.Local] {
		return fmt.Errorf("core element %s carries namespace %q", name.Local, name.Space)
	}
	if n.declared(name.Space) || name.Space == "UDF" || name.Space == udfNamespace {
		return nil
	}
	return fmt.Errorf("undeclared namespace prefix %q on %s", name.Space, name.Local)
}

// readText reads an element's character data up to its end tag. Child
// elements are walked (and namespace-checked) but their text is ignored.
func readText(dec *xml.Decoder, ns *nsScopes) (string, error) {
	var sb strings.Builder
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.CharData:
			if depth == 0 {
				sb.Write(t)
			}
		case xml.StartElement:
			if err := ns.push(t); err != nil {
				return "", err
			}
			depth++
		case xml.EndElement:
			ns.pop()
			if depth == 0 {
				return restoreControlChars(sb.String()), nil
			}
			depth--
		}
	}
}

// skipElement walks to the current element's end, namespace-checking it.
func skipElement(dec *xml.Decoder, ns *nsScopes) error {
	_, err := readText(dec, ns)
	return err
}

func readVoucher(dec *xml.Decoder, ns *nsScopes) (TaxLedgerVoucherRaw, error) {
	var v TaxLedgerVoucherRaw
	var all, plain []TaxLedgerVoucherLegRaw
	seen := map[string]bool{}
	field := func(dst *string, name string, s string) {
		if !seen[name] {
			*dst = s
			seen[name] = true
		}
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return v, fmt.Errorf("xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			ns.pop()
			// ALLLEDGERENTRIES is every leg of the voucher; LEDGERENTRIES
			// holds the same legs on the vouchers that export only it.
			// Reading both on one voucher would count its legs twice.
			v.Legs = all
			if len(all) == 0 {
				v.Legs = plain
			}
			return v, nil
		case xml.StartElement:
			if err := ns.push(t); err != nil {
				return v, err
			}
			if t.Name.Space != "" { // a UDF: element
				if err := skipElement(dec, ns); err != nil {
					return v, err
				}
				continue
			}
			switch t.Name.Local {
			case "ALLLEDGERENTRIES.LIST", "LEDGERENTRIES.LIST":
				leg, err := readLeg(dec, ns)
				if err != nil {
					return v, err
				}
				if t.Name.Local == "ALLLEDGERENTRIES.LIST" {
					all = append(all, leg)
				} else {
					plain = append(plain, leg)
				}
				continue
			case "VOUCHER":
				return v, fmt.Errorf("VOUCHER nested in VOUCHER")
			}
			s, err := readText(dec, ns)
			if err != nil {
				return v, err
			}
			switch t.Name.Local {
			case "GUID":
				field(&v.GUID, "GUID", s)
			case "MASTERID":
				field(&v.MasterID, "MASTERID", s)
			case "ALTERID":
				field(&v.AlterID, "ALTERID", s)
			case "DATE":
				field(&v.Date, "DATE", s)
			case "VOUCHERTYPENAME":
				field(&v.VoucherTypeName, "VOUCHERTYPENAME", s)
			case "VOUCHERNUMBER":
				field(&v.VoucherNumber, "VOUCHERNUMBER", s)
			case "REFERENCE":
				field(&v.Reference, "REFERENCE", s)
			case "PARTYLEDGERNAME":
				field(&v.PartyLedgerName, "PARTYLEDGERNAME", s)
			case "PARTYGSTIN":
				field(&v.PartyGSTIN, "PARTYGSTIN", s)
			case "PLACEOFSUPPLY":
				field(&v.PlaceOfSupply, "PLACEOFSUPPLY", s)
			case "ISINVOICE":
				v.IsInvoice = parseTallyBool(s)
			case "ISCANCELLED":
				v.IsCancelled = parseTallyBool(s)
			case "ISOPTIONAL":
				v.IsOptional = parseTallyBool(s)
			case "ISRCMAPPLICABLE":
				v.IsRCMApplicable = parseTallyBool(s)
			}
		}
	}
}

func readLeg(dec *xml.Decoder, ns *nsScopes) (TaxLedgerVoucherLegRaw, error) {
	var leg TaxLedgerVoucherLegRaw
	seen := map[string]bool{}
	for {
		tok, err := dec.Token()
		if err != nil {
			return leg, fmt.Errorf("xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.EndElement:
			ns.pop()
			return leg, nil
		case xml.StartElement:
			if err := ns.push(t); err != nil {
				return leg, err
			}
			if t.Name.Space != "" {
				if err := skipElement(dec, ns); err != nil {
					return leg, err
				}
				continue
			}
			s, err := readText(dec, ns)
			if err != nil {
				return leg, err
			}
			if seen[t.Name.Local] {
				continue
			}
			switch t.Name.Local {
			case "LEDGERNAME":
				leg.LedgerName = s
			case "AMOUNT":
				leg.Amount = s
			case "GSTCLASS":
				leg.GSTClass = s
			default:
				continue
			}
			seen[t.Name.Local] = true
		}
	}
}

// ---- selection ----

// Problem codes on a sent voucher (the server shows them; the voucher is
// never dropped for them).
const (
	TLVProblemMissingGUID          = "missing_guid"
	TLVProblemDuplicateGUID        = "duplicate_guid"
	TLVProblemLegWithoutLedgerName = "leg_without_ledger_name"
	TLVProblemAmountUnreadable     = "amount_unreadable"
	TLVProblemLedgerNameControl    = "ledger_name_control_char"
)

// TaxLedgerKey is how a ledger name is compared with the tax ledger set:
// trimmed, control characters removed, case folded (Tally ledger names are
// unique ignoring case).
func TaxLedgerKey(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	return strings.ToLower(strings.TrimSpace(name))
}

// hasControlChar reports a control character inside the trimmed name.
func hasControlChar(name string) bool {
	for _, r := range strings.TrimSpace(name) {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// tallyDecimalRE is a decimal the agent can pass on as-is.
var tallyDecimalRE = regexp.MustCompile(`^[+-]?(\d+(\.\d*)?|\.\d+)$`)

// TaxLedgerVoucherSelection is what one month's reply yields.
type TaxLedgerVoucherSelection struct {
	Rows     []TaxLedgerVoucherRow
	Snapshot TaxLedgerVoucherSnapshot
	// OutsideWindowGUIDs / DateUnreadableGUIDs name the vouchers behind the
	// incomplete snapshot (for the sync warning).
	OutsideWindowGUIDs  []string
	DateUnreadableGUIDs []string
	// Unbalanced counts selected vouchers whose legs do not sum to zero
	// (sent as they are; the server flags them).
	Unbalanced int
}

// SelectTaxLedgerVouchers applies the probe's rules to one reply. ledgerSet
// holds TaxLedgerKey of each tax ledger; from/to is the inclusive window.
//
//   - A voucher dated outside the window is counted, never sent, and makes
//     the snapshot incomplete; so does a voucher whose date can't be read
//     (sent with voucher_date null when it touches the set).
//   - A cancelled voucher is never sent; it is counted when it touches the set.
//   - Every other voucher with a leg on the set is sent with all its legs.
func SelectTaxLedgerVouchers(reply TaxLedgerVoucherReply, ledgerSet map[string]bool, from, to time.Time) TaxLedgerVoucherSelection {
	sel := TaxLedgerVoucherSelection{Rows: []TaxLedgerVoucherRow{}}
	sel.Snapshot.VouchersReturned = len(reply.Vouchers)
	guidCount := map[string]int{}
	for _, v := range reply.Vouchers {
		if g := strings.TrimSpace(v.GUID); g != "" {
			guidCount[strings.ToLower(g)]++
		}
	}
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	toDay := time.Date(to.Year(), to.Month(), to.Day(), 0, 0, 0, 0, time.UTC)

	for _, v := range reply.Vouchers {
		guid := strings.TrimSpace(v.GUID)
		var date *string
		d, derr := ParseTallyDate(v.Date)
		if derr != nil {
			sel.Snapshot.DateUnreadable++
			sel.DateUnreadableGUIDs = append(sel.DateUnreadableGUIDs, guid)
		} else {
			if d.Before(fromDay) || d.After(toDay) {
				sel.Snapshot.OutsideWindow++
				sel.OutsideWindowGUIDs = append(sel.OutsideWindowGUIDs, guid)
				continue
			}
			s := d.Format("2006-01-02")
			date = &s
		}

		touches, controlOnTarget := false, false
		for _, leg := range v.Legs {
			if key := TaxLedgerKey(leg.LedgerName); key != "" && ledgerSet[key] {
				touches = true
				if hasControlChar(leg.LedgerName) {
					controlOnTarget = true
				}
			}
		}
		if !touches {
			continue
		}
		if v.IsCancelled {
			sel.Snapshot.CancelledTouching++
			continue
		}

		row := TaxLedgerVoucherRow{
			TallyVoucherGUID: optionalString(guid),
			MasterID:         optionalString(strings.TrimSpace(v.MasterID)),
			AlterID:          optionalString(strings.TrimSpace(v.AlterID)),
			VoucherDate:      date,
			VoucherDateRaw:   v.Date,
			VoucherTypeName:  optionalString(strings.TrimSpace(v.VoucherTypeName)),
			VoucherNumber:    optionalString(strings.TrimSpace(v.VoucherNumber)),
			Reference:        optionalString(strings.TrimSpace(v.Reference)),
			PartyLedgerName:  optionalString(strings.TrimSpace(v.PartyLedgerName)),
			PartyGSTIN:       optionalString(strings.TrimSpace(v.PartyGSTIN)),
			IsInvoice:        v.IsInvoice,
			IsOptional:       v.IsOptional,
			IsRCMApplicable:  v.IsRCMApplicable,
			Legs:             make([]TaxLedgerVoucherLeg, 0, len(v.Legs)),
			Problems:         []string{},
		}
		addProblem := func(p string) {
			for _, have := range row.Problems {
				if have == p {
					return
				}
			}
			row.Problems = append(row.Problems, p)
		}
		if guid == "" {
			addProblem(TLVProblemMissingGUID)
		} else if guidCount[strings.ToLower(guid)] > 1 {
			addProblem(TLVProblemDuplicateGUID)
		}
		var sum decimalSum
		balanced := true
		for _, leg := range v.Legs {
			out := TaxLedgerVoucherLeg{
				LedgerName: leg.LedgerName,
				GSTClass:   optionalString(strings.TrimSpace(leg.GSTClass)),
			}
			if strings.TrimSpace(leg.LedgerName) == "" {
				addProblem(TLVProblemLegWithoutLedgerName)
			}
			amt := strings.TrimSpace(leg.Amount)
			if tallyDecimalRE.MatchString(amt) {
				out.Amount = &amt
				if !sum.add(amt) {
					balanced = false
				}
			} else {
				addProblem(TLVProblemAmountUnreadable)
				balanced = false
			}
			row.Legs = append(row.Legs, out)
		}
		if controlOnTarget {
			addProblem(TLVProblemLedgerNameControl)
		}
		if balanced && !sum.zero() {
			sel.Unbalanced++
		}
		sel.Rows = append(sel.Rows, row)
	}
	sel.Snapshot.Complete = sel.Snapshot.OutsideWindow == 0 && sel.Snapshot.DateUnreadable == 0
	return sel
}

// decimalSum adds decimal strings exactly, in units of 10^-6.
type decimalSum struct{ micros int64 }

func (s *decimalSum) add(dec string) bool {
	neg := false
	switch {
	case strings.HasPrefix(dec, "-"):
		neg, dec = true, dec[1:]
	case strings.HasPrefix(dec, "+"):
		dec = dec[1:]
	}
	intPart, frac, _ := strings.Cut(dec, ".")
	if len(frac) > 6 || len(intPart) > 12 {
		return false
	}
	frac += strings.Repeat("0", 6-len(frac))
	var v int64
	for _, r := range intPart + frac {
		v = v*10 + int64(r-'0')
	}
	if neg {
		v = -v
	}
	s.micros += v
	return true
}

func (s decimalSum) zero() bool { return s.micros == 0 }
