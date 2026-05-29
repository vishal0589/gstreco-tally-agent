package tally

import "testing"

func TestParseImportResponse_CountersAndLineErrorsFromXMLBody(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<ENVELOPE>
  <HEADER><VERSION>1</VERSION><STATUS>0</STATUS></HEADER>
  <BODY><DATA><IMPORTRESULT>
    <CREATED>0</CREATED>
    <ALTERED>1</ALTERED>
    <DELETED>0</DELETED>
    <COMBINED>0</COMBINED>
    <IGNORED>2</IGNORED>
    <ERRORS>1</ERRORS>
    <LASTVCHID>42</LASTVCHID>
    <LASTMID>7</LASTMID>
    <LINEERROR>Ledger IPAF Vendor does not exist</LINEERROR>
  </IMPORTRESULT></DATA></BODY>
</ENVELOPE>`)

	got, err := ParseImportResponse(raw)
	if err != nil {
		t.Fatalf("ParseImportResponse: %v", err)
	}
	if got.Status != 0 || got.Altered != 1 || got.Ignored != 2 || got.Errors != 1 || got.LastVoucher != 42 || got.LastMaster != 7 {
		t.Fatalf("counters parsed incorrectly: %+v", got)
	}
	if len(got.LineErrors) != 1 || got.LineErrors[0] != "Ledger IPAF Vendor does not exist" {
		t.Fatalf("LineErrors = %+v", got.LineErrors)
	}
}

func TestParseStockSummaryReport_InheritsSiblingItemBatchAndGodownState(t *testing.T) {
	raw := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<ENVELOPE>
  <DSPACCNAME><DSPDISPNAME>Archive Camera</DSPDISPNAME></DSPACCNAME>
  <DSPSTKINFO>
    <DSPSTKOP><DSPOPQTY>2 PCS</DSPOPQTY><DSPOPRATE>500</DSPOPRATE><DSPOPAMTA>1000</DSPOPAMTA></DSPSTKOP>
    <DSPSTKIN><DSPINQTY>1 PCS</DSPINQTY><DSPINRATE>550</DSPINRATE><DSPDRAMTA>550</DSPDRAMTA></DSPSTKIN>
    <DSPSTKOUT><DSPOUTQTY>0 PCS</DSPOUTQTY><DSPOUTRATE>0</DSPOUTRATE><DSPNETTCRAMTA>0</DSPNETTCRAMTA></DSPSTKOUT>
    <DSPSTKCL><DSPCLQTY>3 PCS</DSPCLQTY><DSPCLRATE>516.67</DSPCLRATE><DSPCLAMTA>1550</DSPCLAMTA></DSPSTKCL>
  </DSPSTKINFO>
  <SSBATCHNAME><SSBATCH>B-001</SSBATCH><SSGODOWN>Main Store</SSGODOWN></SSBATCHNAME>
  <DSPSTKINFO>
    <DSPSTKCL><DSPCLQTY>1 PCS</DSPCLQTY><DSPCLRATE>500</DSPCLRATE><DSPCLAMTA>500</DSPCLAMTA></DSPSTKCL>
  </DSPSTKINFO>
</ENVELOPE>`)

	got, err := ParseStockSummaryReport(raw)
	if err != nil {
		t.Fatalf("ParseStockSummaryReport: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows = %d, want 2", len(got))
	}
	if got[0].ItemName != "Archive Camera" || got[0].BatchName != "" || got[0].GodownName != "" {
		t.Fatalf("item-level row state = %+v", got[0])
	}
	if got[0].Opening.Quantity != 2 || got[0].Inward.Quantity != 1 || got[0].Closing.Amount != 1550 {
		t.Fatalf("item-level quantities = %+v", got[0])
	}
	if got[1].ItemName != "Archive Camera" || got[1].BatchName != "B-001" || got[1].GodownName != "Main Store" {
		t.Fatalf("batch row state = %+v", got[1])
	}
	if got[1].Closing.Quantity != 1 || got[1].Closing.Unit != "PCS" || got[1].Closing.Amount != 500 {
		t.Fatalf("batch closing = %+v", got[1].Closing)
	}
}
