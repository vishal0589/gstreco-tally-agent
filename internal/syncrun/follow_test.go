package syncrun

import (
	"errors"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func probe(endpoint string, companies ...string) tally.ProbeResult {
	p := tally.ProbeResult{Endpoint: endpoint, Reachable: true}
	for _, c := range companies {
		p.Companies = append(p.Companies, tally.TallyCompany{Name: c})
	}
	return p
}

func mapping(name, endpoint string) ingest.ActiveMapping {
	return ingest.ActiveMapping{MappingID: name, TallyCompanyName: name, TallyEndpoint: endpoint}
}

const (
	aam = "Aam Exim Private Limited"
	hah = "HAH Intl Shipping Pvt. Ltd. - (from 1-Apr-25)"
)

func TestResolve_ReadsBothCompaniesFromOneSharedPort(t *testing.T) {
	resolved, followed, skipped := ResolveMappingEndpoints(
		[]ingest.ActiveMapping{mapping(aam, "http://127.0.0.1:9012"), mapping(hah, "http://127.0.0.1:9012")},
		[]tally.ProbeResult{probe("http://127.0.0.1:9012", hah, aam)},
	)
	if len(resolved) != 2 || len(followed) != 0 || len(skipped) != 0 {
		t.Fatalf("resolved=%d followed=%d skipped=%d, want 2/0/0", len(resolved), len(followed), len(skipped))
	}
}

func TestResolve_FollowsACompanyToTheOnePortItIsOpenOn(t *testing.T) {
	resolved, followed, skipped := ResolveMappingEndpoints(
		[]ingest.ActiveMapping{mapping(aam, "http://127.0.0.1:9012"), mapping(hah, "http://localhost:9012/")},
		[]tally.ProbeResult{
			{Endpoint: "http://127.0.0.1:9012", Err: errors.New("connection refused")},
			probe("http://127.0.0.1:9552", aam),
			probe("http://127.0.0.1:9553", hah, "KSS INTERIORS INDIA PVT. LTD C"),
		},
	)
	if len(skipped) != 0 || len(followed) != 2 {
		t.Fatalf("followed=%d skipped=%v", len(followed), skipped)
	}
	got := map[string]string{}
	for _, m := range resolved {
		got[m.TallyCompanyName] = m.TallyEndpoint
	}
	if got[aam] != "http://127.0.0.1:9552" || got[hah] != "http://127.0.0.1:9553" {
		t.Fatalf("resolved endpoints = %v", got)
	}
}

func TestResolve_StaysOnTheMappedPortWhenTheCompanyIsAlsoOpenElsewhere(t *testing.T) {
	resolved, followed, _ := ResolveMappingEndpoints(
		[]ingest.ActiveMapping{mapping(aam, "http://127.0.0.1:9012")},
		[]tally.ProbeResult{probe("http://127.0.0.1:9012", aam), probe("http://127.0.0.1:9552", aam)},
	)
	if len(followed) != 0 || resolved[0].TallyEndpoint != "http://127.0.0.1:9012" {
		t.Fatalf("want the mapped port kept; resolved=%v followed=%v", resolved, followed)
	}
}

func TestResolve_DoesNotGuessBetweenTwoOtherCopies(t *testing.T) {
	resolved, _, skipped := ResolveMappingEndpoints(
		[]ingest.ActiveMapping{mapping(aam, "http://127.0.0.1:9012")},
		[]tally.ProbeResult{probe("http://127.0.0.1:9552", aam), probe("http://127.0.0.1:9555", aam)},
	)
	if len(resolved) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "several Tally ports") {
		t.Fatalf("resolved=%v skipped=%v", resolved, skipped)
	}
}

func TestResolve_ExplainsACompanyOpenNowhere(t *testing.T) {
	cases := map[string]tally.ProbeResult{
		"did not answer":   {Endpoint: "http://127.0.0.1:9012", Err: errors.New("connection refused")},
		"no company open":  probe("http://127.0.0.1:9012"),
		"KAMAL BHAI SAREE": probe("http://127.0.0.1:9012", "KAMAL BHAI SAREE SANGAM FY 25-26 & 26-27"),
	}
	for want, p := range cases {
		_, _, skipped := ResolveMappingEndpoints([]ingest.ActiveMapping{mapping(aam, "http://127.0.0.1:9012")}, []tally.ProbeResult{p})
		if len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "not open in any Tally") || !strings.Contains(skipped[0].Reason, want) {
			t.Errorf("%s: skipped=%v", want, skipped)
		}
	}
}

func TestCandidateEndpoints_MappedPortsPlusEveryTallyListener(t *testing.T) {
	got := CandidateEndpoints(
		[]ingest.ActiveMapping{mapping(aam, "http://127.0.0.1:9012"), mapping(hah, "http://localhost:9012")},
		[]int{3037, 9012, 9552},
	)
	want := []string{"http://127.0.0.1:9012", "http://127.0.0.1:3037", "http://127.0.0.1:9552"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got %v, want %v", got, want)
	}
}
