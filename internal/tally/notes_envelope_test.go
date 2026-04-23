package tally

import (
	"strings"
	"testing"
	"time"
)

func TestBuildDayBookXML_CreditNoteFilter(t *testing.T) {
	body, err := BuildDayBookXML(DayBookRequest{
		Company: "Acme",
		From:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:      time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		Kind:    VoucherCreditNote,
	})
	if err != nil {
		t.Fatalf("BuildDayBookXML: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "$$IsCreditNote:$VoucherTypeName") {
		t.Error("credit-note envelope missing $$IsCreditNote filter")
	}
	if !strings.Contains(s, "IsGstrecoCreditNote") {
		t.Error("credit-note envelope missing filter name")
	}
}

func TestBuildDayBookXML_DebitNoteFilter(t *testing.T) {
	body, err := BuildDayBookXML(DayBookRequest{
		Company: "Acme",
		From:    time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:      time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		Kind:    VoucherDebitNote,
	})
	if err != nil {
		t.Fatalf("BuildDayBookXML: %v", err)
	}
	if !strings.Contains(string(body), "$$IsDebitNote:$VoucherTypeName") {
		t.Error("debit-note envelope missing $$IsDebitNote filter")
	}
}
