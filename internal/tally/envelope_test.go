package tally

import (
	"strings"
	"testing"
	"time"
)

func TestBuildDayBookXML_Purchase(t *testing.T) {
	r := DayBookRequest{
		Company: "Acme Retail Pvt Ltd",
		From:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:      time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		Kind:    VoucherPurchase,
	}
	body, err := BuildDayBookXML(r)
	if err != nil {
		t.Fatalf("BuildDayBookXML: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"<SVCURRENTCOMPANY>Acme Retail Pvt Ltd</SVCURRENTCOMPANY>",
		"<SVFROMDATE TYPE=\"Date\">20260401</SVFROMDATE>",
		"<SVTODATE TYPE=\"Date\">20260430</SVTODATE>",
		"$$IsPurchase:$VoucherTypeName",
		"ALLLEDGERENTRIES.LEDGERNAME",
		"INVENTORYENTRIES.STOCKITEMNAME",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("envelope missing %q; body was:\n%s", want, s)
		}
	}
}

func TestBuildDayBookXML_Sales(t *testing.T) {
	body, err := BuildDayBookXML(DayBookRequest{
		Company: "Acme",
		From:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:      time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC),
		Kind:    VoucherSales,
	})
	if err != nil {
		t.Fatalf("BuildDayBookXML: %v", err)
	}
	if !strings.Contains(string(body), "$$IsSales:$VoucherTypeName") {
		t.Errorf("expected $$IsSales filter in sales envelope")
	}
}

func TestBuildDayBookXML_EscapesCompanyName(t *testing.T) {
	// Ampersands in company names ("Smith & Sons") must be XML-escaped.
	body, err := BuildDayBookXML(DayBookRequest{
		Company: "Smith & Sons <Ltd>",
		From:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:      time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		Kind:    VoucherPurchase,
	})
	if err != nil {
		t.Fatalf("BuildDayBookXML: %v", err)
	}
	s := string(body)
	if strings.Contains(s, "Smith & Sons <Ltd>") {
		t.Errorf("company name was not escaped; body:\n%s", s)
	}
	if !strings.Contains(s, "Smith &amp; Sons &lt;Ltd&gt;") {
		t.Errorf("expected escaped company name; body:\n%s", s)
	}
}

func TestBuildDayBookXML_Validation(t *testing.T) {
	cases := []struct {
		name string
		r    DayBookRequest
	}{
		{"missing company", DayBookRequest{From: time.Now(), To: time.Now(), Kind: VoucherPurchase}},
		{"zero from", DayBookRequest{Company: "Acme", To: time.Now(), Kind: VoucherPurchase}},
		{"zero to", DayBookRequest{Company: "Acme", From: time.Now(), Kind: VoucherPurchase}},
		{"inverted range", DayBookRequest{
			Company: "Acme",
			From:    time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
			To:      time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			Kind:    VoucherPurchase,
		}},
		{"unknown kind", DayBookRequest{
			Company: "Acme",
			From:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			To:      time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
			Kind:    VoucherKind("mystery_kind"),
		}},
		{"control char", DayBookRequest{
			Company: "Acme\x01Corp",
			From:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
			To:      time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
			Kind:    VoucherPurchase,
		}},
	}
	for _, tc := range cases {
		if _, err := BuildDayBookXML(tc.r); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}
