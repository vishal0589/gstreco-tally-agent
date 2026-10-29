package tally

import (
	"encoding/xml"
	"fmt"
	"html/template"
	"strings"
)

// LegRole is the one role every invoice voucher leg gets (QA v18 R3).
type LegRole string

const (
	LegParty LegRole = "party"
	// LegTax is GST: IGST, CGST, SGST or CESS (Head).
	LegTax  LegRole = "tax"
	LegBase LegRole = "base"
	// LegWithholding is TDS withheld from the party or TCS collected by it
	// (Withholding). Never GST and never taxable value.
	LegWithholding LegRole = "withholding"
	LegFreight     LegRole = "freight"
	LegDiscount    LegRole = "discount"
	LegRounding    LegRole = "rounding"
	// LegAdjustment is a non-tax leg on the party's side of the voucher,
	// netted against the taxable value.
	LegAdjustment LegRole = "adjustment"
	// LegOtherDuty is a Duties & Taxes ledger that is neither GST nor TDS/TCS.
	LegOtherDuty LegRole = "other_duty"
	// LegUncertain is a leg whose name mentions a GST head with no evidence
	// that it is GST: kept out of taxable value and GST, and flagged.
	LegUncertain LegRole = "uncertain"
)

// LegEvidence is what decided a leg's role.
type LegEvidence string

const (
	EvidencePartyLedger LegEvidence = "party_ledger"
	// EvidenceDutyMaster: the Duties & Taxes ledger master (TAXTYPE,
	// GSTDUTYHEAD).
	EvidenceDutyMaster LegEvidence = "duty_master"
	// EvidenceNotDutyLedger: the company's duty ledgers are known and this
	// ledger is not one of them, so it is never tax.
	EvidenceNotDutyLedger LegEvidence = "not_duty_ledger"
	EvidenceGSTClass      LegEvidence = "gst_class"
	EvidenceName          LegEvidence = "name"
	EvidenceNone          LegEvidence = "none"
	// EvidenceInventory: the base came from the voucher's inventory entries
	// because the ledger legs alone did not balance.
	EvidenceInventory LegEvidence = "inventory"
)

// Withholding kinds.
const (
	WithholdingTDS = "TDS"
	WithholdingTCS = "TCS"
)

// DutyLedger is one ledger under Duties & Taxes, from its master.
type DutyLedger struct {
	Name     string
	Parent   string
	TaxType  string
	DutyHead string
}

// DutyLedgerBook is a company's Duties & Taxes ledgers by normalized name.
// Only membership of this book can make a voucher leg GST. A nil book means
// no evidence: the fetch failed or came back empty, which is never read as a
// company with no duty ledgers.
type DutyLedgerBook struct {
	byKey map[string]DutyLedger
}

// NewDutyLedgerBook builds a book from ledger masters, or nil when there are
// none.
func NewDutyLedgerBook(ledgers []DutyLedger) *DutyLedgerBook {
	book := &DutyLedgerBook{byKey: map[string]DutyLedger{}}
	for _, l := range ledgers {
		l.Name = strings.TrimSpace(l.Name)
		if key := dutyLedgerKey(l.Name); key != "" {
			l.Parent = strings.TrimSpace(l.Parent)
			l.TaxType = strings.TrimSpace(l.TaxType)
			l.DutyHead = strings.TrimSpace(l.DutyHead)
			book.byKey[key] = l
		}
	}
	if len(book.byKey) == 0 {
		return nil
	}
	return book
}

// dutyLedgerKey folds case and runs of whitespace: Tally ledger names are
// case-insensitive.
func dutyLedgerKey(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// Lookup returns the ledger's master when it is a Duties & Taxes ledger.
func (b *DutyLedgerBook) Lookup(name string) (DutyLedger, bool) {
	if b == nil {
		return DutyLedger{}, false
	}
	l, ok := b.byKey[dutyLedgerKey(name)]
	return l, ok
}

// Len is the number of duty ledgers in the book.
func (b *DutyLedgerBook) Len() int {
	if b == nil {
		return 0
	}
	return len(b.byKey)
}

var dutyLedgerTmpl = template.Must(template.New("duty-ledgers").Parse(`<ENVELOPE>
  <HEADER>
    <VERSION>1</VERSION>
    <TALLYREQUEST>Export</TALLYREQUEST>
    <TYPE>Collection</TYPE>
    <ID>GstrecoDutyLedgerCollection</ID>
  </HEADER>
  <BODY>
    <DESC>
      <STATICVARIABLES>
        <SVCURRENTCOMPANY>{{.Company}}</SVCURRENTCOMPANY>
        <SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
      </STATICVARIABLES>
      <TDL>
        <TDLMESSAGE>
          <COLLECTION NAME="GstrecoDutyLedgerCollection" ISINITIALIZE="Yes">
            <TYPE>Ledger</TYPE>
            <CHILDOF>Duties &amp; Taxes</CHILDOF>
            <BELONGSTO>Yes</BELONGSTO>
            <FETCH>NAME, PARENT, TAXTYPE, GSTDUTYHEAD</FETCH>
          </COLLECTION>
        </TDLMESSAGE>
      </TDL>
    </DESC>
  </BODY>
</ENVELOPE>`))

// BuildDutyLedgerXML renders the read-only request for every ledger under
// Duties & Taxes, at any depth (BELONGSTO), with its parent group, type of
// duty (TAXTYPE) and GST duty head.
func BuildDutyLedgerXML(company string) ([]byte, error) {
	return renderCompanyEnvelope(dutyLedgerTmpl, company)
}

type xmlDutyLedger struct {
	NameAttr string `xml:"NAME,attr"`
	Name     string `xml:"NAME"`
	Parent   string `xml:"PARENT"`
	TaxType  string `xml:"TAXTYPE"`
	DutyHead string `xml:"GSTDUTYHEAD"`
}

// ParseDutyLedgers decodes the BuildDutyLedgerXML response. An export with no
// ledgers returns a nil book: no evidence, not "no duty ledgers".
func ParseDutyLedgers(raw []byte) (*DutyLedgerBook, []string, error) {
	var ledgers []DutyLedger
	var warnings []string
	err := walkTallyExport(raw, "LEDGER", func(dec *xml.Decoder, start *xml.StartElement) error {
		var x xmlDutyLedger
		if err := dec.DecodeElement(&x, start); err != nil {
			warnings = append(warnings, fmt.Sprintf("duty ledger decode: %v", err))
			return nil
		}
		ledgers = append(ledgers, DutyLedger{
			Name:     firstNonEmpty(x.Name, x.NameAttr),
			Parent:   x.Parent,
			TaxType:  x.TaxType,
			DutyHead: x.DutyHead,
		})
		return nil
	})
	if err != nil {
		return nil, warnings, err
	}
	return NewDutyLedgerBook(ledgers), warnings, nil
}

// legDecision is one leg's role and why.
type legDecision struct {
	role LegRole
	// head is IGST / CGST / SGST / CESS for a tax leg.
	head string
	// withholding is TDS or TCS for a withholding leg.
	withholding string
	evidence    LegEvidence
	flags       []string
	// From the duty master, when the leg is a Duties & Taxes ledger.
	parent, taxType, dutyHead string
}

// classifyLeg decides a non-party leg's role from the evidence, in order:
//  1. The duty-ledger master, when the book exists. Only membership of
//     Duties & Taxes makes a leg GST; a ledger outside it is never tax,
//     whatever its name.
//  2. The GST class, when there is no book: prefix only ("IGST@18").
//  3. The name, whole words only, to spot withholding, freight, discount and
//     rounding, or to raise uncertainty. A name alone never makes a leg GST.
func classifyLeg(e LedgerEntry, book *DutyLedgerBook) legDecision {
	name := strings.TrimSpace(e.LedgerName)
	words := ledgerWords(name)
	nameHead := headFromNameTokens(name)
	nameWithholding := withholdingFromName(words)
	// A taxable purchase / sales account named after its tax ("Purchase
	// (IGST)@18%") is the base, never uncertain.
	commercial := looksLikeCommercialAccountLedger(compactTaxLedgerToken(name))

	if book != nil {
		m, member := book.Lookup(name)
		if !member {
			d := classifyByName(words, nameWithholding)
			d.evidence = evidenceOr(d.evidence, EvidenceNotDutyLedger)
			if nameHead != "" && d.role != LegWithholding && !commercial {
				d.flags = append(d.flags, fmt.Sprintf("%q is named after %s but is not a Duties & Taxes ledger: counted as %s, not GST", name, nameHead, d.role))
			}
			if classHead := headFromGSTClass(e.GSTClass); classHead != "" && d.role != LegWithholding {
				d.flags = append(d.flags, fmt.Sprintf("%q has GST class %q but is not a Duties & Taxes ledger: counted as %s, not %s", name, e.GSTClass, d.role, classHead))
			}
			return d
		}
		d := legDecision{evidence: EvidenceDutyMaster, parent: m.Parent, taxType: m.TaxType, dutyHead: m.DutyHead}
		taxType := strings.ToUpper(strings.TrimSpace(m.TaxType))
		switch {
		case taxType == WithholdingTDS || taxType == WithholdingTCS:
			d.role, d.withholding = LegWithholding, taxType
		case nameWithholding != "":
			// GST TDS / TCS (sections 51 / 52) is withheld from the party,
			// not input credit, whatever the master's type of duty says.
			d.role, d.withholding = LegWithholding, nameWithholding
			d.flags = append(d.flags, fmt.Sprintf("%q: duty master type %q, but its name is %s: counted as withholding", name, m.TaxType, nameWithholding))
		case taxType == "GST":
			if head := headFromDutyHead(m.DutyHead); head != "" {
				d.role, d.head = LegTax, head
			} else {
				d.role = LegUncertain
				d.flags = append(d.flags, fmt.Sprintf("%q is a GST duty ledger with no GST duty head (%q): kept out of GST and taxable value", name, m.DutyHead))
			}
		default:
			d.role = LegOtherDuty
			if nameHead != "" {
				d.flags = append(d.flags, fmt.Sprintf("%q is named after %s but its duty master type is %q: counted as other duty, not GST", name, nameHead, m.TaxType))
			}
		}
		return d
	}

	if head := headFromGSTClass(e.GSTClass); head != "" {
		if nameWithholding != "" {
			return legDecision{role: LegWithholding, withholding: nameWithholding, evidence: EvidenceName,
				flags: []string{fmt.Sprintf("%q: GST class %q, but its name is %s: counted as withholding", name, e.GSTClass, nameWithholding)}}
		}
		return legDecision{role: LegTax, head: head, evidence: EvidenceGSTClass}
	}
	d := classifyByName(words, nameWithholding)
	if nameHead == "" || d.role == LegWithholding {
		return d
	}
	if commercial {
		d.evidence = EvidenceName
		return d
	}
	return legDecision{role: LegUncertain, evidence: EvidenceName, flags: []string{
		fmt.Sprintf("%q is named after %s with no duty-ledger or GST-class evidence: kept out of GST and taxable value", name, nameHead),
	}}
}

// classifyByName is the name step for a leg that is not GST: withholding,
// freight, discount, rounding, else base.
func classifyByName(words []string, withholding string) legDecision {
	switch {
	case withholding != "":
		return legDecision{role: LegWithholding, withholding: withholding, evidence: EvidenceName}
	case isRoundingName(words):
		return legDecision{role: LegRounding, evidence: EvidenceName}
	case hasAnyWord(words, discountWords):
		return legDecision{role: LegDiscount, evidence: EvidenceName}
	case hasAnyWord(words, freightWords):
		return legDecision{role: LegFreight, evidence: EvidenceName}
	}
	return legDecision{role: LegBase, evidence: EvidenceNone}
}

func evidenceOr(e, fallback LegEvidence) LegEvidence {
	if e == "" || e == EvidenceNone {
		return fallback
	}
	return e
}

// headFromGSTClass is the GST head Tally's GST class starts with ("IGST@18"
// -> IGST), or "". UTGST counts as SGST: GSTR-2B has no separate bucket.
func headFromGSTClass(gstClass string) string {
	c := strings.ToUpper(strings.TrimSpace(gstClass))
	switch {
	case strings.HasPrefix(c, "IGST"):
		return "IGST"
	case strings.HasPrefix(c, "CGST"):
		return "CGST"
	case strings.HasPrefix(c, "SGST"), strings.HasPrefix(c, "UTGST"):
		return "SGST"
	case strings.HasPrefix(c, "CESS"):
		return "CESS"
	}
	return ""
}

// headFromNameTokens is the GST head a ledger name mentions, read word by
// word: a word that is or starts with a head ("IGST", "IGST18",
// "IGSTPAYABLE"), "INPUT"/"OUTPUT" glued in front ("OUTPUTIGST"), or the
// whole word CESS ("Processing" is not CESS). "" when none. A mention is
// never evidence that a leg is GST.
func headFromNameTokens(name string) string {
	words := ledgerWords(name)
	for _, head := range []string{"IGST", "CGST", "SGST"} {
		for _, w := range words {
			if gstHeadOfWord(w) == head {
				return head
			}
		}
	}
	for _, w := range words {
		if w == "CESS" {
			return "CESS"
		}
	}
	return ""
}

// headFromDutyHead maps Tally's GSTDUTYHEAD to a GST head.
func headFromDutyHead(dutyHead string) string {
	switch strings.Join(strings.Fields(strings.ToUpper(dutyHead)), " ") {
	case "INTEGRATED TAX", "IGST":
		return "IGST"
	case "CENTRAL TAX", "CGST":
		return "CGST"
	case "STATE TAX", "UT TAX", "UNION TERRITORY TAX", "STATE/UT TAX", "SGST", "UTGST":
		return "SGST"
	case "CESS", "COMPENSATION CESS", "GST CESS":
		return "CESS"
	}
	return ""
}

// withholdingFromName is TDS or TCS when the name says so: a word starting
// with TDS / TCS, "Tax Deducted" / "Tax Collected", or "Withholding" (TDS).
func withholdingFromName(words []string) string {
	for i, w := range words {
		switch {
		case strings.HasPrefix(w, "TDS"), w == "WITHHOLDING":
			return WithholdingTDS
		case strings.HasPrefix(w, "TCS"):
			return WithholdingTCS
		case w == "TAX" && i+1 < len(words) && words[i+1] == "DEDUCTED":
			return WithholdingTDS
		case w == "TAX" && i+1 < len(words) && words[i+1] == "COLLECTED":
			return WithholdingTCS
		}
	}
	return ""
}

var (
	discountWords = map[string]bool{"DISCOUNT": true, "DISCOUNTS": true, "REBATE": true}
	freightWords  = map[string]bool{"FREIGHT": true, "CARRIAGE": true, "CARTAGE": true}
)

// isRoundingName is "Round Off", "Rounded Off", "Rounding", "RoundOff".
func isRoundingName(words []string) bool {
	for i, w := range words {
		switch w {
		case "ROUNDING", "ROUNDOFF":
			return true
		case "ROUND", "ROUNDED":
			if i+1 < len(words) && words[i+1] == "OFF" {
				return true
			}
		}
	}
	return false
}

func hasAnyWord(words []string, set map[string]bool) bool {
	for _, w := range words {
		if set[w] {
			return true
		}
	}
	return false
}
