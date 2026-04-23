package tally

import (
	"strings"
	"testing"
)

func TestBuildMasterXML_Vendor(t *testing.T) {
	body, err := BuildMasterXML(MasterRequest{Company: "Acme & Co <Ltd>", Kind: MasterVendor})
	if err != nil {
		t.Fatalf("BuildMasterXML: %v", err)
	}
	s := string(body)
	for _, want := range []string{
		"<SVCURRENTCOMPANY>Acme &amp; Co &lt;Ltd&gt;</SVCURRENTCOMPANY>",
		`$Parent = &#34;Sundry Creditors&#34;`,
		"IsGstrecoVendor",
		"MAILINGNAME.LIST",
		"ADDRESS.LIST",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("envelope missing %q\nbody:\n%s", want, s)
		}
	}
}

func TestBuildMasterXML_Customer(t *testing.T) {
	body, err := BuildMasterXML(MasterRequest{Company: "Acme", Kind: MasterCustomer})
	if err != nil {
		t.Fatalf("BuildMasterXML: %v", err)
	}
	if !strings.Contains(string(body), "Sundry Debtors") {
		t.Error("customer envelope missing Sundry Debtors filter")
	}
}

func TestBuildMasterXML_Validation(t *testing.T) {
	cases := []struct {
		name string
		r    MasterRequest
	}{
		{"no company", MasterRequest{Kind: MasterVendor}},
		{"bad kind", MasterRequest{Company: "Acme", Kind: MasterKind("random")}},
		{"control char", MasterRequest{Company: "Acme\x00Co", Kind: MasterVendor}},
	}
	for _, tc := range cases {
		if _, err := BuildMasterXML(tc.r); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

func TestBuildCompanyListXML(t *testing.T) {
	body, err := BuildCompanyListXML(CompanyListRequest{})
	if err != nil {
		t.Fatalf("BuildCompanyListXML: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, "<ID>List of Companies</ID>") {
		t.Errorf("envelope missing report id\n%s", s)
	}
}

func TestBuildVersionXML(t *testing.T) {
	body, err := BuildVersionXML()
	if err != nil {
		t.Fatalf("BuildVersionXML: %v", err)
	}
	if !strings.Contains(string(body), "$$Version") {
		t.Error("version envelope missing $$Version id")
	}
}
