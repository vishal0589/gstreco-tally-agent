package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
)

// tallyWithCompanies answers the company-list probe like a Tally that has
// these companies open, and fails the $$Version probe as TallyPrime 6/7 do.
func tallyWithCompanies(t *testing.T, companies ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "List of Companies") {
			_, _ = w.Write([]byte("<RESPONSE>Could not find: $$Version</RESPONSE>"))
			return
		}
		var b strings.Builder
		b.WriteString("<ENVELOPE><BODY><DATA>")
		for _, c := range companies {
			b.WriteString("<COMPANY><NAME>" + c + "</NAME></COMPANY>")
		}
		b.WriteString("</DATA></BODY></ENVELOPE>")
		_, _ = w.Write([]byte(b.String()))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestResolveMappingsForTick_ReadsACompanyWhereItIsOpen(t *testing.T) {
	const aam, hah = "Aam Exim Private Limited", "HAH Intl Shipping Pvt. Ltd. - (from 1-Apr-25)"
	open := tallyWithCompanies(t, hah, aam)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close() // nothing listens here any more

	mappings := []ingest.ActiveMapping{
		{MappingID: "m-aam", TallyCompanyName: aam, TallyEndpoint: closedURL},
		{MappingID: "m-hah", TallyCompanyName: hah, TallyEndpoint: open.URL},
		{MappingID: "m-gone", TallyCompanyName: "Closed Company Ltd", TallyEndpoint: closedURL},
	}
	resolved, err := resolveMappingsForTick(context.Background(), mappings, zerolog.Nop())

	got := map[string]string{}
	for _, m := range resolved {
		got[m.MappingID] = m.TallyEndpoint
	}
	if got["m-aam"] != open.URL || got["m-hah"] != open.URL {
		t.Fatalf("resolved = %v, want AAM and HaH both read from %s", got, open.URL)
	}
	if _, ok := got["m-gone"]; ok {
		t.Fatal("a company open nowhere must not be read")
	}
	if err == nil || !strings.Contains(err.Error(), "Closed Company Ltd") || !strings.Contains(err.Error(), "not open in any Tally") {
		t.Fatalf("err = %v, want it to name the company that is open nowhere", err)
	}
}
