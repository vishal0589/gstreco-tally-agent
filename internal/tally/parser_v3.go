package tally

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ParseResult is what the parser hands back. Status mirrors the <STATUS>
// header value Tally returns (1 = success, 0 = failure, other values indicate
// specific server errors — codes aren't documented by Tally but are stable
// within a Prime major version). Warnings collects non-fatal issues so the
// agent can surface them in logs and `agentctl status` without losing the
// vouchers that did parse successfully.
type ParseResult struct {
	Vouchers []RawVoucher
	Status   int
	Warnings []string
}

// ParseDayBookV3 decodes a Tally Prime 3.x day-book response into RawVouchers.
// The parser is tolerant by design: malformed or partially-formed vouchers are
// dropped with a warning rather than failing the whole batch, because a single
// broken ledger entry in a 5000-voucher response would otherwise strand an
// entire sync run. Fatal errors (unreadable XML, wrong root element) still
// return an error — the caller retries those.
//
// Tally envelopes sometimes wrap vouchers in TALLYMESSAGE, sometimes in
// COLLECTION, and sometimes at the top level depending on the request ID. The
// parser walks tokens looking for <VOUCHER> at any depth rather than locking
// to a specific nesting shape, which also makes it resilient to future Tally
// Prime minor-version changes that move the wrapper around.
func ParseDayBookV3(raw []byte) (ParseResult, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ParseResult{}, fmt.Errorf("tally: empty response")
	}

	dec := xml.NewDecoder(bytes.NewReader(raw))
	// Tally occasionally emits ampersands and less-than signs that the spec
	// would reject. Strict=false lets the decoder recover; AutoClose is left
	// default because Tally does close every element it opens.
	dec.Strict = false
	// Tally defaults to UTF-8 but some installs with legacy Windows
	// codepages emit ISO-8859-1. If we ever see a charset we don't know,
	// pass the bytes through unchanged and let the caller spot the
	// replacement characters — better than failing the whole batch.
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}

	result := ParseResult{Status: -1}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return result, fmt.Errorf("tally: xml decode: %w", err)
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
		case "VOUCHER":
			var v xmlVoucher
			if err := dec.DecodeElement(&v, &start); err != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("voucher decode: %v", err))
				continue
			}
			raw, warnings := v.toRaw()
			result.Warnings = append(result.Warnings, warnings...)
			if raw.GUID == "" && raw.VoucherNumber == "" {
				result.Warnings = append(result.Warnings, "voucher dropped: missing both GUID and VoucherNumber")
				continue
			}
			result.Vouchers = append(result.Vouchers, raw)
		}
	}
	return result, nil
}

// xmlVoucher mirrors Tally Prime 3.x's <VOUCHER> element. All fields are
// strings because Tally's number/date formatting varies — typed conversion
// happens in toRaw() so the error path is inside the agent, not in
// encoding/xml's unhelpful "cannot unmarshal into int" messages.
type xmlVoucher struct {
	Date             string              `xml:"DATE"`
	GUID             string              `xml:"GUID"`
	AlterID          string              `xml:"ALTERID"`
	VoucherType      string              `xml:"VOUCHERTYPENAME"`
	VoucherNumber    string              `xml:"VOUCHERNUMBER"`
	Reference        string              `xml:"REFERENCE"`
	PartyLedgerName  string              `xml:"PARTYLEDGERNAME"`
	PartyGSTIN       string              `xml:"PARTYGSTIN"`
	PlaceOfSupply    string              `xml:"PLACEOFSUPPLY"`
	IsInvoice        string              `xml:"ISINVOICE"`
	IsCancelled      string              `xml:"ISCANCELLED"`
	IsRcmApplicable  string              `xml:"ISRCMAPPLICABLE"`
	Narration        string              `xml:"NARRATION"`
	LedgerEntries    []xmlLedgerEntry    `xml:"ALLLEDGERENTRIES.LIST"`
	InventoryEntries []xmlInventoryEntry `xml:"INVENTORYENTRIES.LIST"`
}

type xmlLedgerEntry struct {
	LedgerName      string       `xml:"LEDGERNAME"`
	Amount          string       `xml:"AMOUNT"`
	GSTClass        string       `xml:"GSTCLASS"`
	BillAllocations []xmlBillRef `xml:"BILLALLOCATIONS.LIST"`
}

type xmlBillRef struct {
	Name     string `xml:"NAME"`
	Amount   string `xml:"AMOUNT"`
	BillType string `xml:"BILLTYPE"`
}

type xmlInventoryEntry struct {
	StockItem string `xml:"STOCKITEMNAME"`
	ActualQty string `xml:"ACTUALQTY"`
	Rate      string `xml:"RATE"`
	Amount    string `xml:"AMOUNT"`
	GSTHSN    string `xml:"GSTHSNNAME"`
}

// toRaw converts the parsed XML shape to the agent-native RawVoucher. It
// returns a slice of per-field warnings so the caller can distinguish
// "dropped the whole voucher" from "voucher intact but date was unparsable".
func (x xmlVoucher) toRaw() (RawVoucher, []string) {
	var warnings []string

	r := RawVoucher{
		GUID:            strings.TrimSpace(x.GUID),
		VoucherType:     strings.TrimSpace(x.VoucherType),
		VoucherNumber:   strings.TrimSpace(x.VoucherNumber),
		Reference:       strings.TrimSpace(x.Reference),
		PartyLedgerName: strings.TrimSpace(x.PartyLedgerName),
		PartyGSTIN:      strings.ToUpper(strings.TrimSpace(x.PartyGSTIN)),
		PlaceOfSupply:   strings.TrimSpace(x.PlaceOfSupply),
		IsInvoice:       parseTallyBool(x.IsInvoice),
		IsCancelled:     parseTallyBool(x.IsCancelled),
		ReverseCharge:   parseTallyBool(x.IsRcmApplicable),
		Narration:       strings.TrimSpace(x.Narration),
	}

	if x.AlterID != "" {
		if v, err := strconv.ParseInt(strings.TrimSpace(x.AlterID), 10, 64); err == nil {
			r.AlterID = v
		} else {
			warnings = append(warnings, fmt.Sprintf("voucher %s: unparsable ALTERID %q", voucherID(r), x.AlterID))
		}
	}

	if x.Date != "" {
		if t, err := ParseTallyDate(x.Date); err == nil {
			r.Date = t
		} else {
			warnings = append(warnings, fmt.Sprintf("voucher %s: %v", voucherID(r), err))
		}
	}

	for _, le := range x.LedgerEntries {
		entry := LedgerEntry{
			LedgerName: strings.TrimSpace(le.LedgerName),
			GSTClass:   strings.TrimSpace(le.GSTClass),
		}
		entry.IsPartyLedger = entry.LedgerName != "" && entry.LedgerName == r.PartyLedgerName

		if amt, err := parseTallyAmount(le.Amount); err == nil {
			entry.Amount = amt
		} else if le.Amount != "" {
			warnings = append(warnings, fmt.Sprintf("voucher %s ledger %q: %v", voucherID(r), entry.LedgerName, err))
		}

		for _, ba := range le.BillAllocations {
			ref := BillRef{
				Name:     strings.TrimSpace(ba.Name),
				BillType: strings.TrimSpace(ba.BillType),
			}
			if amt, err := parseTallyAmount(ba.Amount); err == nil {
				ref.Amount = amt
			} else if ba.Amount != "" {
				warnings = append(warnings, fmt.Sprintf("voucher %s bill %q: %v", voucherID(r), ref.Name, err))
			}
			entry.BillAllocations = append(entry.BillAllocations, ref)
		}
		r.LedgerEntries = append(r.LedgerEntries, entry)
	}

	for _, ie := range x.InventoryEntries {
		entry := InventoryEntry{
			StockItem: strings.TrimSpace(ie.StockItem),
			HSN:       strings.TrimSpace(ie.GSTHSN),
		}
		entry.Quantity, entry.Unit = parseTallyQuantity(ie.ActualQty)
		entry.Rate, _ = parseTallyRate(ie.Rate)
		if amt, err := parseTallyAmount(ie.Amount); err == nil {
			entry.Amount = amt
		}
		r.InventoryEntries = append(r.InventoryEntries, entry)
	}

	return r, warnings
}

// voucherID returns a human-readable id for logging. Prefers GUID but falls
// back to VoucherNumber so warnings point somewhere useful even when Tally
// sent an incomplete voucher.
func voucherID(r RawVoucher) string {
	if r.GUID != "" {
		return r.GUID
	}
	if r.VoucherNumber != "" {
		return r.VoucherNumber
	}
	return "(unknown)"
}

// parseTallyBool normalises Tally's "Yes"/"No"/"" (and occasional 1/0) into a
// Go bool. Trim and casefold so user-edited TDLs that emit "YES" or "yes"
// still work.
func parseTallyBool(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "yes", "true", "1":
		return true
	default:
		return false
	}
}

// parseTallyAmount handles signed decimals with optional thousands
// separators in either Indian ("1,18,000.00") or Western ("118,000.00") style.
// Empty input is treated as a valid zero so missing <AMOUNT/> elements in
// edge-case Tally exports don't fail the whole voucher.
func parseTallyAmount(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	s = strings.ReplaceAll(s, ",", "")
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("unparsable amount %q", s)
	}
	return v, nil
}

// parseTallyQuantity splits "10 NOS" or "10.000 NOS" into (10.0, "NOS").
// Quantities can have negative signs for return vouchers. Tally sometimes
// omits the unit ("10" alone) — the function returns ("", 10) in that case
// rather than erroring.
func parseTallyQuantity(s string) (qty float64, unit string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ""
	}
	parts := strings.Fields(s)
	if v, err := strconv.ParseFloat(strings.ReplaceAll(parts[0], ",", ""), 64); err == nil {
		qty = v
	}
	if len(parts) > 1 {
		unit = strings.Join(parts[1:], " ")
	}
	return qty, unit
}

// parseTallyRate splits "1000.00/NOS" into (1000.0, "NOS"). Same tolerance
// rules as parseTallyQuantity — unit is optional.
func parseTallyRate(s string) (rate float64, unit string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ""
	}
	parts := strings.SplitN(s, "/", 2)
	if v, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(parts[0]), ",", ""), 64); err == nil {
		rate = v
	}
	if len(parts) > 1 {
		unit = strings.TrimSpace(parts[1])
	}
	return rate, unit
}
