package tally

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildCompanyIdentityXML(t *testing.T) {
	env := string(BuildCompanyIdentityXML())
	for _, want := range []string{
		"<TYPE>Collection</TYPE>",
		"<ID>GstrecoCompanyIdentity</ID>",
		`<COLLECTION NAME="GstrecoCompanyIdentity" ISINITIALIZE="Yes">`,
		"<TYPE>Company</TYPE>",
		"<FETCH>NAME, GUID</FETCH>",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("envelope missing %q:\n%s", want, env)
		}
	}
	// Must enumerate companies, not pick one by name.
	if strings.Contains(env, "SVCURRENTCOMPANY") {
		t.Fatal("identity probe must not scope by SVCURRENTCOMPANY")
	}
}

func TestParseCompanyIdentity_PrimeShape(t *testing.T) {
	got, err := ParseCompanyIdentity(readTestdata(t, "company-identity-prime.xml"))
	if err != nil {
		t.Fatalf("ParseCompanyIdentity: %v", err)
	}
	want := []TallyCompany{
		{Name: "Acme Retail Pvt Ltd", GUID: "5f0c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b"},
		{Name: "Acme Manufacturing Ltd", GUID: "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d"},
		// Name only in the NAME attribute; entity decoded.
		{Name: "Acme & Sons (Trading)", GUID: "11112222-3333-4444-5555-666677778888"},
	}
	if len(got.Companies) != len(want) {
		t.Fatalf("companies = %+v, want %+v", got.Companies, want)
	}
	for i := range want {
		if got.Companies[i] != want[i] {
			t.Fatalf("company[%d] = %+v, want %+v", i, got.Companies[i], want[i])
		}
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("warnings = %v", got.Warnings)
	}
}

func TestParseCompanyIdentity_SameNameTwoGUIDsKeptBoth(t *testing.T) {
	raw := `<ENVELOPE><BODY><DATA><COLLECTION>
<COMPANY NAME="KBS"><NAME>KBS</NAME><GUID>aaaa</GUID></COMPANY>
<COMPANY NAME="KBS"><NAME>KBS</NAME><GUID>bbbb</GUID></COMPANY>
</COLLECTION></DATA></BODY></ENVELOPE>`
	got, err := ParseCompanyIdentity([]byte(raw))
	if err != nil || len(got.Companies) != 2 {
		t.Fatalf("got %+v, %v; want both copies (ambiguity is the caller's call)", got.Companies, err)
	}
}

func TestParseCompanyIdentity_MissingGUIDWarns(t *testing.T) {
	raw := `<ENVELOPE><BODY><DATA><COLLECTION><COMPANY NAME="KSS"><NAME>KSS</NAME></COMPANY></COLLECTION></DATA></BODY></ENVELOPE>`
	got, err := ParseCompanyIdentity([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Companies) != 1 || got.Companies[0].GUID != "" || len(got.Warnings) != 1 {
		t.Fatalf("got %+v warnings %v", got.Companies, got.Warnings)
	}
}

func TestParseCompanyIdentity_EmptyCollectionIsNoCompanies(t *testing.T) {
	raw := `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION></COLLECTION></DATA></BODY></ENVELOPE>`
	got, err := ParseCompanyIdentity([]byte(raw))
	if err != nil || len(got.Companies) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParseCompanyIdentity_RefusalsAreErrors(t *testing.T) {
	cases := map[string]string{
		"empty":         "  ",
		"not envelope":  "<html><body>IIS</body></html>",
		"lineerror":     `<ENVELOPE><LINEERROR>Could not find Collection 'GstrecoCompanyIdentity'</LINEERROR></ENVELOPE>`,
		"status 0":      `<ENVELOPE><HEADER><STATUS>0</STATUS></HEADER><BODY><DESC></DESC></BODY></ENVELOPE>`,
		"unknown req":   `<ENVELOPE><RESPONSE>Unknown Request, cannot be processed</RESPONSE></ENVELOPE>`,
		"no collection": `<ENVELOPE><HEADER><VERSION>1</VERSION></HEADER></ENVELOPE>`,
	}
	for name, raw := range cases {
		if got, err := ParseCompanyIdentity([]byte(raw)); err == nil {
			t.Errorf("%s: want error, got %+v", name, got)
		}
	}
}

// Discover with IdentityProbe records the GUID probe next to the List of
// Companies answer and never changes Companies.
func TestDiscover_IdentityProbe(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("List of Companies")):
			_, _ = w.Write([]byte(`<ENVELOPE><BODY><DATA><COMPANY><NAME>KBS Exports</NAME></COMPANY></DATA></BODY></ENVELOPE>`))
		case bytes.Contains(body, []byte("GstrecoCompanyIdentity")):
			_, _ = w.Write([]byte(`<ENVELOPE><BODY><DATA><COLLECTION><COMPANY NAME="KBS Exports"><GUID>g-kbs</GUID></COMPANY><COMPANY NAME="Other"><GUID>g-o</GUID></COMPANY></COLLECTION></DATA></BODY></ENVELOPE>`))
		default:
			_, _ = w.Write([]byte(`<RESPONSE>Unknown Request, cannot be processed</RESPONSE>`))
		}
	}))
	defer srv.Close()

	res, err := Discover(context.Background(), DiscoverOptions{Endpoints: []string{srv.URL}, IdentityProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	p := res[0]
	if len(p.Companies) != 1 || p.Companies[0].GUID != "" {
		t.Fatalf("Companies = %+v, want List of Companies untouched", p.Companies)
	}
	if p.Identity == nil || p.Identity.Err != nil || len(p.Identity.Companies) != 2 {
		t.Fatalf("Identity = %+v", p.Identity)
	}
	if g := p.IdentityGUID("KBS Exports"); g != "g-kbs" {
		t.Fatalf("IdentityGUID = %q", g)
	}
	if cs := p.CompaniesWithGUID(); len(cs) != 1 || cs[0].GUID != "g-kbs" {
		t.Fatalf("CompaniesWithGUID = %+v (must not add identity-only companies)", cs)
	}

	res, _ = Discover(context.Background(), DiscoverOptions{Endpoints: []string{srv.URL}})
	if res[0].Identity != nil {
		t.Fatal("identity probe ran without DiscoverOptions.IdentityProbe")
	}
}

func TestIdentityGUID_AmbiguousOrFailedIsBlank(t *testing.T) {
	amb := ProbeResult{Identity: &IdentityProbe{Companies: []TallyCompany{{Name: "A", GUID: "1"}, {Name: "A", GUID: "2"}}}}
	if g := amb.IdentityGUID("A"); g != "" {
		t.Fatalf("ambiguous: %q", g)
	}
	blank := ProbeResult{Identity: &IdentityProbe{Companies: []TallyCompany{{Name: "A"}}}}
	if g := blank.IdentityGUID("A"); g != "" {
		t.Fatalf("blank: %q", g)
	}
	failed := ProbeResult{Identity: &IdentityProbe{Err: io.EOF, Companies: []TallyCompany{{Name: "A", GUID: "1"}}}}
	if g := failed.IdentityGUID("A"); g != "" {
		t.Fatalf("failed: %q", g)
	}
	if g := (ProbeResult{}).IdentityGUID("A"); g != "" {
		t.Fatalf("not run: %q", g)
	}
}
