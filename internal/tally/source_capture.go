package tally

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"time"
)

// SourceCaptureRequest requests all voucher types in the window. It is separate
// from ingest: zero-tax, cancelled and unknown voucher types are not filtered.
// This requests source evidence, not a declaration of statutory completeness.
func BuildSourceCaptureXML(company string, from, to time.Time) ([]byte, error) {
	// Reuse the existing input validation, not its voucher-family filter.
	if _, err := BuildDayBookXML(DayBookRequest{Company: company, From: from, To: to, Kind: VoucherPurchase}); err != nil {
		return nil, err
	}
	params := struct{ Company, From, To string }{company, FormatTallyDate(from), FormatTallyDate(to)}
	var b bytes.Buffer
	if err := sourceCaptureTmpl.Execute(&b, params); err != nil {
		return nil, fmt.Errorf("tally: render source capture: %w", err)
	}
	return b.Bytes(), nil
}

var sourceCaptureTmpl = template.Must(template.New("sourcecapture").Parse(`<ENVELOPE>
 <HEADER><VERSION>1</VERSION><TALLYREQUEST>Export</TALLYREQUEST><TYPE>Collection</TYPE><ID>GstrecoSourceCapture</ID></HEADER>
 <BODY><DESC><STATICVARIABLES>
 <SVCURRENTCOMPANY>{{.Company}}</SVCURRENTCOMPANY><SVFROMDATE TYPE="Date">{{.From}}</SVFROMDATE><SVTODATE TYPE="Date">{{.To}}</SVTODATE><SVEXPORTFORMAT>$$SysName:XML</SVEXPORTFORMAT>
 </STATICVARIABLES><TDL><TDLMESSAGE>
 <COLLECTION NAME="GstrecoSourceCapture" ISINITIALIZE="Yes"><TYPE>Voucher</TYPE>
 <FETCH>*</FETCH>
 <FETCH>GUID, MASTERID, ALTERID, DATE, VOUCHERTYPENAME, VOUCHERNUMBER, REFERENCE, PARTYLEDGERNAME, PARTYGSTIN, PLACEOFSUPPLY, ISINVOICE, ISCANCELLED, ISOPTIONAL, ISRCMAPPLICABLE, NARRATION</FETCH>
 <FETCH>ALLLEDGERENTRIES.*, ALLLEDGERENTRIES.BILLALLOCATIONS.*</FETCH>
 <FETCH>INVENTORYENTRIES.*, ALLINVENTORYENTRIES.*, INVENTORYENTRIES.ACCOUNTINGALLOCATIONS.*</FETCH>
 <FILTER>GstrecoSourceCaptureWindow</FILTER></COLLECTION>
 <SYSTEM TYPE="Formulae" NAME="GstrecoSourceCaptureWindow">($Date &gt;= ##SVFROMDATE) AND ($Date &lt;= ##SVTODATE)</SYSTEM>
 </TDLMESSAGE></TDL></DESC></BODY></ENVELOPE>`))

// CountSourceVoucherElements counts source VOUCHER elements independently of
// the tolerant parser, which may drop malformed vouchers. Raw bytes remain the
// evidence; a count match is not a completeness certificate.
func CountSourceVoucherElements(raw []byte) (int, error) {
	dec := xml.NewDecoder(bytes.NewReader(prepareXMLForDecode(raw)))
	count := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("source XML not well-formed: %w", err)
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "VOUCHER" {
			count++
		}
	}
}
