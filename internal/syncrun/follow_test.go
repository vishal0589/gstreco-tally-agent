package syncrun

import (
	"context"
	"errors"
	"os"
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
		got[m.TallyCompanyName] = m.ReadEndpoint()
		// Ingest keeps the mapped endpoint: the server finds the mapping by it.
		if m.TallyEndpoint != "http://127.0.0.1:9012" && m.TallyEndpoint != "http://localhost:9012/" {
			t.Errorf("%s: TallyEndpoint changed to %s", m.TallyCompanyName, m.TallyEndpoint)
		}
	}
	if got[aam] != "http://127.0.0.1:9552" || got[hah] != "http://127.0.0.1:9553" {
		t.Fatalf("read endpoints = %v", got)
	}
}

func TestResolve_NeverFollowsAnotherCompanyWithTheSameName(t *testing.T) {
	m := mapping(aam, "http://127.0.0.1:9012")
	m.TallyCompanyGUID = "guid-books"
	other := probe("http://127.0.0.1:9552")
	other.Companies = []tally.TallyCompany{{Name: aam, GUID: "guid-old-copy"}}
	resolved, _, skipped := ResolveMappingEndpoints([]ingest.ActiveMapping{m}, []tally.ProbeResult{other})
	if len(resolved) != 0 || len(skipped) != 1 {
		t.Fatalf("resolved=%v skipped=%v: a different GUID must not be followed", resolved, skipped)
	}
	same := probe("http://127.0.0.1:9552")
	same.Companies = []tally.TallyCompany{{Name: aam, GUID: "GUID-BOOKS"}}
	resolved, _, _ = ResolveMappingEndpoints([]ingest.ActiveMapping{m}, []tally.ProbeResult{same})
	if len(resolved) != 1 || resolved[0].ReadEndpoint() != "http://127.0.0.1:9552" {
		t.Fatalf("the same GUID must be followed; resolved=%v", resolved)
	}
}

func TestResolve_TreatsLocalAliasesAsOneTally(t *testing.T) {
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no host name")
	}
	// A mapping by host name and a probe by loopback reach the same Tally:
	// one copy, read where it is mapped, not "several ports".
	resolved, followed, skipped := ResolveMappingEndpoints(
		[]ingest.ActiveMapping{mapping(aam, "http://"+host+":9012")},
		[]tally.ProbeResult{probe("http://127.0.0.1:9012", aam), probe("http://localhost:9012", aam)},
	)
	if len(resolved) != 1 || len(followed) != 0 || len(skipped) != 0 {
		t.Fatalf("resolved=%v followed=%v skipped=%v", resolved, followed, skipped)
	}
	if got := CandidateEndpoints([]ingest.ActiveMapping{mapping(aam, "http://"+host+":9012")}, []int{9012}); len(got) != 1 {
		t.Fatalf("aliases must be probed once, got %v", got)
	}
}

func TestResolve_StaysOnTheMappedPortWhenTheCompanyIsAlsoOpenElsewhere(t *testing.T) {
	resolved, followed, _ := ResolveMappingEndpoints(
		[]ingest.ActiveMapping{mapping(aam, "http://127.0.0.1:9012")},
		[]tally.ProbeResult{probe("http://127.0.0.1:9012", aam), probe("http://127.0.0.1:9552", aam)},
	)
	if len(followed) != 0 || resolved[0].ReadEndpoint() != "http://127.0.0.1:9012" {
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

func TestWalkAll_ReadsTheFollowedPortButReportsTheMappedOne(t *testing.T) {
	var readFrom []string
	snd := &fakeSender{}
	m := mapping("PLLUM CASA", "http://127.0.0.1:9012")
	m.FetchEndpoint = "http://127.0.0.1:9552"
	WalkAll(context.Background(), WalkOptions{
		Mappings: []ingest.ActiveMapping{m},
		Kinds:    []KindPair{{Name: "purchase", Tally: tally.VoucherPurchase, Ingest: tally.IngestKindPurchase}},
		From:     makeReq().From,
		To:       makeReq().To,
		RunKind:  "manual",
		NewTallyClient: func(endpoint string) TallyPoster {
			readFrom = append(readFrom, endpoint)
			return &fakeTally{resp: []byte(stubResponse)}
		},
		Sender: snd,
	})
	if len(readFrom) != 1 || readFrom[0] != "http://127.0.0.1:9552" {
		t.Fatalf("read from %v, want the followed port", readFrom)
	}
	if len(snd.sent) == 0 || snd.sent[0].TallyEndpoint == nil || *snd.sent[0].TallyEndpoint != "http://127.0.0.1:9012" {
		t.Fatalf("ingest must report the mapped endpoint; sent=%+v", snd.sent)
	}
}
