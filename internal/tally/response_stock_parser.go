package tally

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// ImportResponse is Tally's XML import acknowledgement. HTTP 200 only means
// Tally accepted the request transport; these counters are the business truth.
type ImportResponse struct {
	Status      int
	Created     int
	Altered     int
	Deleted     int
	Combined    int
	Ignored     int
	Cancelled   int
	Exceptions  int
	Errors      int
	LastVoucher int
	LastMaster  int
	LineErrors  []string
}

// ParseImportResponse parses Tally import response counters from the XML body.
func ParseImportResponse(raw []byte) (ImportResponse, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ImportResponse{}, fmt.Errorf("tally: empty import response")
	}
	raw = prepareXMLForDecode(raw)
	dec := newTallyDecoder(raw)
	out := ImportResponse{Status: -1}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return out, fmt.Errorf("tally: import response xml decode: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "STATUS":
			out.Status = decodeIntElement(dec, start, out.Status)
		case "CREATED":
			out.Created = decodeIntElement(dec, start, 0)
		case "ALTERED":
			out.Altered = decodeIntElement(dec, start, 0)
		case "DELETED":
			out.Deleted = decodeIntElement(dec, start, 0)
		case "COMBINED":
			out.Combined = decodeIntElement(dec, start, 0)
		case "IGNORED":
			out.Ignored = decodeIntElement(dec, start, 0)
		case "CANCELLED":
			out.Cancelled = decodeIntElement(dec, start, 0)
		case "EXCEPTIONS":
			out.Exceptions = decodeIntElement(dec, start, 0)
		case "ERRORS":
			out.Errors = decodeIntElement(dec, start, 0)
		case "LASTVCHID":
			out.LastVoucher = decodeIntElement(dec, start, 0)
		case "LASTMID":
			out.LastMaster = decodeIntElement(dec, start, 0)
		case "LINEERROR":
			var value string
			if err := dec.DecodeElement(&value, &start); err == nil {
				if msg := strings.TrimSpace(value); msg != "" {
					out.LineErrors = append(out.LineErrors, msg)
				}
			}
		}
	}
	return out, nil
}

// StockSummaryRow is one DSPSTKINFO row from Tally's report-style stock XML.
// Item/batch/godown are inherited from preceding sibling nodes, because the
// DSPSTKINFO node itself does not carry native STOCKITEM identity.
type StockSummaryRow struct {
	ItemName   string
	BatchName  string
	GodownName string
	Opening    StockBucket
	Inward     StockBucket
	Outward    StockBucket
	Closing    StockBucket
}

type StockBucket struct {
	Quantity float64
	Unit     string
	Rate     float64
	Amount   float64
}

// ParseStockSummaryReport parses report-style stock summary XML separately
// from native STOCKITEM/VOUCHER XML.
func ParseStockSummaryReport(raw []byte) ([]StockSummaryRow, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("tally: empty stock summary response")
	}
	raw = prepareXMLForDecode(raw)
	dec := newTallyDecoder(raw)
	var rows []StockSummaryRow
	var currentItem, currentBatch, currentGodown string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return rows, fmt.Errorf("tally: stock summary xml decode: %w", err)
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "DSPACCNAME":
			var item xmlDSPAccName
			if err := dec.DecodeElement(&item, &start); err != nil {
				return rows, fmt.Errorf("tally: stock item state decode: %w", err)
			}
			currentItem = strings.TrimSpace(item.DisplayName)
			currentBatch = ""
			currentGodown = ""
		case "SSBATCHNAME":
			var batch xmlSSBatchName
			if err := dec.DecodeElement(&batch, &start); err != nil {
				return rows, fmt.Errorf("tally: stock batch state decode: %w", err)
			}
			currentBatch = strings.TrimSpace(batch.Batch)
			currentGodown = strings.TrimSpace(batch.Godown)
		case "DSPSTKINFO":
			var info xmlDSPStockInfo
			if err := dec.DecodeElement(&info, &start); err != nil {
				return rows, fmt.Errorf("tally: stock info decode: %w", err)
			}
			rows = append(rows, StockSummaryRow{
				ItemName:   currentItem,
				BatchName:  currentBatch,
				GodownName: currentGodown,
				Opening:    stockBucket(info.Opening),
				Inward:     stockBucket(info.Inward),
				Outward:    stockBucket(info.Outward),
				Closing:    stockBucket(info.Closing),
			})
		}
	}
	return rows, nil
}

type xmlDSPAccName struct {
	DisplayName string `xml:"DSPDISPNAME"`
}

type xmlSSBatchName struct {
	Batch  string `xml:"SSBATCH"`
	Godown string `xml:"SSGODOWN"`
}

type xmlDSPStockInfo struct {
	Opening xmlStockBucket `xml:"DSPSTKOP"`
	Inward  xmlStockBucket `xml:"DSPSTKIN"`
	Outward xmlStockBucket `xml:"DSPSTKOUT"`
	Closing xmlStockBucket `xml:"DSPSTKCL"`
}

type xmlStockBucket struct {
	OpeningQty    string `xml:"DSPOPQTY"`
	OpeningRate   string `xml:"DSPOPRATE"`
	OpeningAmount string `xml:"DSPOPAMTA"`
	InwardQty     string `xml:"DSPINQTY"`
	InwardRate    string `xml:"DSPINRATE"`
	InwardAmount  string `xml:"DSPDRAMTA"`
	OutwardQty    string `xml:"DSPOUTQTY"`
	OutwardRate   string `xml:"DSPOUTRATE"`
	OutwardAmount string `xml:"DSPNETTCRAMTA"`
	ClosingQty    string `xml:"DSPCLQTY"`
	ClosingRate   string `xml:"DSPCLRATE"`
	ClosingAmount string `xml:"DSPCLAMTA"`
}

func newTallyDecoder(raw []byte) *xml.Decoder {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	dec.Strict = false
	dec.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		return input, nil
	}
	return dec
}

func decodeIntElement(dec *xml.Decoder, start xml.StartElement, fallback int) int {
	var value string
	if err := dec.DecodeElement(&value, &start); err != nil {
		return fallback
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return n
}

func stockBucket(x xmlStockBucket) StockBucket {
	qty, unit := parseTallyQuantity(firstNonEmpty(x.OpeningQty, x.InwardQty, x.OutwardQty, x.ClosingQty))
	rate, _ := parseTallyRate(firstNonEmpty(x.OpeningRate, x.InwardRate, x.OutwardRate, x.ClosingRate))
	amount, _ := parseTallyAmount(firstNonEmpty(x.OpeningAmount, x.InwardAmount, x.OutwardAmount, x.ClosingAmount))
	return StockBucket{
		Quantity: qty,
		Unit:     unit,
		Rate:     rate,
		Amount:   amount,
	}
}
