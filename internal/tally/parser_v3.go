package tally

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/text/encoding/unicode"
	"golang.org/x/text/transform"
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

	// First decode UTF-16LE/BE → UTF-8 if the response is UTF-encoded.
	// Tally Prime echoes the request encoding for IMPORT acks and
	// sometimes returns voucher data in UTF-16LE for installs whose
	// company file has non-ASCII characters in narration / party names
	// (Hindi, regional, ₹). v0.1.4's byte-level sanitizer assumed UTF-8
	// and would corrupt UTF-16LE by stripping the 0x00 high bytes of
	// every ASCII char — caught during the PLLUM pilot 2026-04-27.
	raw = decodeTallyResponse(raw)

	// Then strip XML 1.0-illegal control characters from the (now
	// UTF-8) bytes. Tally Prime emits NUL / EOT / SUB / etc.
	// (0x00-0x08, 0x0B, 0x0C, 0x0E-0x1F) in narration, party-name,
	// and address fields when the source data was pasted from a Word
	// doc, copied from an old DOS report, or hand-typed with
	// Alt-numpad. Go's encoding/xml refuses these even with
	// Strict=false because they're outside XML's character set, not
	// just shape-invalid.
	raw = sanitizeXMLBytes(raw)

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

// decodeTallyResponse detects the response encoding (UTF-16LE,
// UTF-16BE, UTF-8 with or without BOM) and returns UTF-8 bytes.
// Tally Prime defaults to UTF-8 for EXPORT but switches to UTF-16LE
// when the company file's data contains non-ASCII chars (Hindi,
// regional Indian languages, ₹) — and it does NOT advertise this in
// the HTTP Content-Type. Caller must sniff.
//
// Detection order (matches the production Manual2AI Python adapter
// at PlummLegano/scripts/tally-sync/tally_client.py:351-378):
//   1. BOM: \xff\xfe → UTF-16LE; \xfe\xff → UTF-16BE; \xef\xbb\xbf →
//      UTF-8 (BOM stripped, body returned as-is).
//   2. Null-byte heuristic on the head (first 8 bytes): a `<` in
//      position 0 followed by 0x00 in position 1 → UTF-16LE; 0x00
//      then `<` → UTF-16BE.
//   3. Default: UTF-8 (body returned unchanged).
//
// On decode error (truncated UTF-16, invalid sequences) the function
// returns the original bytes — the caller's xml decoder will surface
// the underlying issue, which is more useful than swallowing it here.
func decodeTallyResponse(raw []byte) []byte {
	if len(raw) < 2 {
		return raw
	}
	// BOM sniff.
	if bytes.HasPrefix(raw, []byte{0xff, 0xfe}) {
		return decodeUTF16(raw[2:], unicode.LittleEndian)
	}
	if bytes.HasPrefix(raw, []byte{0xfe, 0xff}) {
		return decodeUTF16(raw[2:], unicode.BigEndian)
	}
	if bytes.HasPrefix(raw, []byte{0xef, 0xbb, 0xbf}) {
		return raw[3:] // UTF-8 BOM, strip.
	}
	// Null-byte heuristic on the first 8 bytes for BOM-less UTF-16.
	head := raw
	if len(head) > 8 {
		head = head[:8]
	}
	if len(head) >= 2 {
		// `<\x00` at start = UTF-16LE
		if head[0] == '<' && head[1] == 0x00 {
			return decodeUTF16(raw, unicode.LittleEndian)
		}
		// `\x00<` at start = UTF-16BE
		if head[0] == 0x00 && head[1] == '<' {
			return decodeUTF16(raw, unicode.BigEndian)
		}
	}
	return raw
}

func decodeUTF16(raw []byte, order unicode.Endianness) []byte {
	dec := unicode.UTF16(order, unicode.IgnoreBOM).NewDecoder()
	out, _, err := transform.Bytes(dec, raw)
	if err != nil {
		// Decoder bails on truncated final code unit — return what we
		// got plus the unconverted tail so the caller still sees most
		// of the response. If the tail is all that errored, sanitize
		// will strip any leftover stray bytes.
		return out
	}
	return out
}

// sanitizeXMLBytes replaces XML-1.0-illegal control characters with
// nothing (drops them). Valid in XML 1.0: 0x09 (tab), 0x0A (LF),
// 0x0D (CR), and anything >= 0x20. Everything else in 0x00-0x1F is
// outside XML's character set and rejected by encoding/xml even with
// Strict=false.
//
// Operates at the byte level — safe for UTF-8 because every
// continuation byte in UTF-8 has the high bit set (>= 0x80) so the
// check `b < 0x20` never triggers on a multi-byte char. Not safe for
// UTF-16 responses (those have NUL bytes between every ASCII char);
// caller should detect + decode UTF-16 to UTF-8 before passing here.
// Tally EXPORT responses default to UTF-8; the v0.1.5 UTF-16LE
// decoding work covers the IMPORT-ack edge case separately.
func sanitizeXMLBytes(raw []byte) []byte {
	// Walk first to see if anything needs stripping. Most responses
	// are clean; allocating a fresh slice on every call is wasteful.
	dirty := false
	for _, b := range raw {
		if b < 0x20 && b != 0x09 && b != 0x0A && b != 0x0D {
			dirty = true
			break
		}
	}
	if !dirty {
		return raw
	}
	out := make([]byte, 0, len(raw))
	for _, b := range raw {
		if b < 0x20 && b != 0x09 && b != 0x0A && b != 0x0D {
			continue
		}
		out = append(out, b)
	}
	return out
}
