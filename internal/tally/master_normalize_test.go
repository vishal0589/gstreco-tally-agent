package tally

import "testing"

func TestNormalizeMasters_FiltersByKindAndDropsBadRows(t *testing.T) {
	raw := []RawMaster{
		{Name: "Vendor With GSTIN", Parent: "Sundry Creditors", GSTIN: "27ABCDE1234F1Z5", TradeName: "Vendor Co", StateName: "Maharashtra", Email: "v@x.com", Mobile: "9999"},
		{Name: "Vendor No GSTIN", Parent: "Sundry Creditors", GSTIN: "", TradeName: "Skipped"},
		{Name: "Customer", Parent: "Sundry Debtors", GSTIN: "29FGHIJ5678K1Z9", TradeName: "Cust Co"},
		{Name: "Bank", Parent: "Bank Accounts", GSTIN: "27NOTUSED1234A1Z5"}, // wrong group
	}

	vendors := NormalizeMasters(raw, MasterVendor)
	if len(vendors) != 1 {
		t.Fatalf("expected 1 vendor, got %d", len(vendors))
	}
	v := vendors[0]
	if v.GSTIN != "27ABCDE1234F1Z5" {
		t.Errorf("GSTIN = %q", v.GSTIN)
	}
	if v.TradeName == nil || *v.TradeName != "Vendor Co" {
		t.Errorf("TradeName = %v", v.TradeName)
	}
	if v.StateCode == nil || *v.StateCode != "27" {
		t.Errorf("StateCode = %v (want 27 for Maharashtra)", v.StateCode)
	}
	if v.Email == nil || *v.Email != "v@x.com" {
		t.Errorf("Email = %v", v.Email)
	}
	if v.Phone == nil || *v.Phone != "9999" {
		t.Errorf("Phone = %v (Mobile preferred over Phone)", v.Phone)
	}

	customers := NormalizeMasters(raw, MasterCustomer)
	if len(customers) != 1 {
		t.Fatalf("expected 1 customer, got %d", len(customers))
	}
	if customers[0].GSTIN != "29FGHIJ5678K1Z9" {
		t.Errorf("customer GSTIN = %q", customers[0].GSTIN)
	}
}

func TestNormalizeMasters_PrefersMobileOverPhone(t *testing.T) {
	raw := []RawMaster{
		{Parent: "Sundry Creditors", GSTIN: "27A", Phone: "0221234567", Mobile: "9876543210"},
	}
	got := NormalizeMasters(raw, MasterVendor)
	if got[0].Phone == nil || *got[0].Phone != "9876543210" {
		t.Errorf("Phone = %v, want mobile", got[0].Phone)
	}
}

func TestNormalizeMasters_DropsEmptyGSTIN(t *testing.T) {
	raw := []RawMaster{
		{Parent: "Sundry Creditors", GSTIN: "", TradeName: "No GSTIN"},
		{Parent: "Sundry Creditors", GSTIN: "  ", TradeName: "Whitespace GSTIN"},
	}
	got := NormalizeMasters(raw, MasterVendor)
	if len(got) != 0 {
		t.Errorf("expected 0 items, got %d", len(got))
	}
}
