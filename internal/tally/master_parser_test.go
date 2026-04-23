package tally

import (
	"strings"
	"testing"
)

func TestParseMastersV3_Vendors(t *testing.T) {
	got, err := ParseMastersV3(readTestdata(t, "masters-v3-vendors.xml"))
	if err != nil {
		t.Fatalf("ParseMastersV3: %v", err)
	}
	if got.Status != 1 {
		t.Errorf("Status = %d, want 1", got.Status)
	}
	if len(got.Warnings) > 0 {
		t.Errorf("unexpected warnings: %v", got.Warnings)
	}
	if len(got.Masters) != 3 {
		t.Fatalf("Masters = %d, want 3", len(got.Masters))
	}

	abc := got.Masters[0]
	if abc.Name != "ABC Vendors Ltd" || abc.GSTIN != "29ABCDE1234F1Z5" {
		t.Errorf("ABC fields: name=%q gstin=%q", abc.Name, abc.GSTIN)
	}
	if abc.AlterID != 5001 {
		t.Errorf("ABC AlterID = %d, want 5001", abc.AlterID)
	}
	if abc.TradeName != "ABC Vendors Private Limited" {
		t.Errorf("TradeName = %q, want MAILINGNAME-sourced trade name", abc.TradeName)
	}
	if abc.Email != "accounts@abcvendors.example" || abc.Mobile != "9876543210" || abc.Contact != "Ravi Kumar" {
		t.Errorf("ABC contacts: email=%q mobile=%q contact=%q", abc.Email, abc.Mobile, abc.Contact)
	}
	wantAddr := "No. 42, Industrial Layout\nKoramangala\nBengaluru 560095"
	if abc.Address != wantAddr {
		t.Errorf("ABC Address = %q\n want %q", abc.Address, wantAddr)
	}
	if !abc.IsKindedAs(MasterVendor) {
		t.Error("ABC should be MasterVendor")
	}

	// Unregistered vendor: GSTIN empty but row survives so the server's
	// gstin-missing inbox picks it up.
	mno := got.Masters[2]
	if mno.Name != "MNO Transport" {
		t.Errorf("third master = %+v", mno)
	}
	if mno.GSTIN != "" {
		t.Errorf("unregistered vendor GSTIN = %q, want empty", mno.GSTIN)
	}
	if mno.GSTRegistrationType != "Unregistered" {
		t.Errorf("GSTRegistrationType = %q", mno.GSTRegistrationType)
	}
	// TradeName falls back to Name when neither MAILINGNAME list is present.
	if mno.TradeName != "MNO Transport" {
		t.Errorf("TradeName fallback = %q, want ledger Name", mno.TradeName)
	}
}

func TestParseMastersV3_CustomersFallBackToLedgerMailing(t *testing.T) {
	got, err := ParseMastersV3(readTestdata(t, "masters-v3-customers.xml"))
	if err != nil {
		t.Fatalf("ParseMastersV3: %v", err)
	}
	if len(got.Masters) != 2 {
		t.Fatalf("Masters = %d, want 2", len(got.Masters))
	}
	xyz := got.Masters[0]
	// Trade name comes from LEDGERMAILINGNAME.LIST when MAILINGNAME.LIST is absent.
	if xyz.TradeName != "XYZ Retailers LLP" {
		t.Errorf("TradeName = %q, want LEDGERMAILINGNAME fallback", xyz.TradeName)
	}
	if !xyz.IsKindedAs(MasterCustomer) {
		t.Error("XYZ should be MasterCustomer")
	}
	if xyz.IsKindedAs(MasterVendor) {
		t.Error("XYZ should NOT be MasterVendor")
	}
}

func TestParseMastersV3_DropsLedgerWithoutIdentity(t *testing.T) {
	raw := []byte(`<?xml version="1.0"?>
<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION>
  <LEDGER><PARENT>Sundry Creditors</PARENT></LEDGER>
  <LEDGER><NAME>KeepMe</NAME><GUID>g1</GUID><PARENT>Sundry Creditors</PARENT></LEDGER>
</COLLECTION></DATA></BODY></ENVELOPE>`)
	got, err := ParseMastersV3(raw)
	if err != nil {
		t.Fatalf("ParseMastersV3: %v", err)
	}
	if len(got.Masters) != 1 || got.Masters[0].Name != "KeepMe" {
		t.Errorf("Masters = %+v", got.Masters)
	}
	if len(got.Warnings) == 0 || !strings.Contains(got.Warnings[0], "dropped") {
		t.Errorf("expected dropped-ledger warning, got %v", got.Warnings)
	}
}

func TestParseMastersV3_EmptyInputErrors(t *testing.T) {
	if _, err := ParseMastersV3(nil); err == nil {
		t.Error("empty input: want error")
	}
}

func TestParseCompanyListV3(t *testing.T) {
	got, err := ParseCompanyListV3(readTestdata(t, "company-list-v3.xml"))
	if err != nil {
		t.Fatalf("ParseCompanyListV3: %v", err)
	}
	// Expect 3 unique companies — the duplicate (whitespace-differing but
	// trimmed to same name) must be deduped.
	if len(got.Companies) != 3 {
		t.Fatalf("Companies = %d, want 3 (with duplicate deduped)", len(got.Companies))
	}
	names := []string{got.Companies[0].Name, got.Companies[1].Name, got.Companies[2].Name}
	if names[0] != "Acme Retail Pvt Ltd" || names[1] != "Acme Manufacturing Ltd" || names[2] != "Acme Old (do not use)" {
		t.Errorf("company names = %v", names)
	}
	if got.Companies[0].GUID != "company-guid-acme-001" {
		t.Errorf("GUID not captured: %q", got.Companies[0].GUID)
	}
}

func TestParseVersion_V3(t *testing.T) {
	v, full, err := ParseVersion(readTestdata(t, "version-v3.xml"))
	if err != nil {
		t.Fatalf("ParseVersion: %v", err)
	}
	if v != VersionV3 {
		t.Errorf("Version = %d, want VersionV3", v)
	}
	if full != "Release 3.0.1" {
		t.Errorf("full = %q", full)
	}
}

func TestParseVersion_V4(t *testing.T) {
	v, _, err := ParseVersion(readTestdata(t, "version-v4.xml"))
	if err != nil {
		t.Fatalf("ParseVersion: %v", err)
	}
	if v != VersionV4 {
		t.Errorf("Version = %d, want VersionV4", v)
	}
}

func TestParseVersion_Unknown(t *testing.T) {
	raw := []byte(`<ENVELOPE><BODY><DATA><RESULT>Release 5.2</RESULT></DATA></BODY></ENVELOPE>`)
	v, _, err := ParseVersion(raw)
	if err != nil {
		t.Fatalf("ParseVersion: %v", err)
	}
	if v != VersionUnknown {
		t.Errorf("Version = %d, want VersionUnknown (no parser for v5 yet)", v)
	}
}

func TestParseVersion_EmptyAndNoVersionString(t *testing.T) {
	if _, _, err := ParseVersion(nil); err == nil {
		t.Error("empty input: want error")
	}
	raw := []byte(`<ENVELOPE><BODY></BODY></ENVELOPE>`)
	if _, _, err := ParseVersion(raw); err == nil {
		t.Error("no version string: want error")
	}
}
