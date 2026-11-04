package syncrun

import (
	"errors"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

const (
	guidKBS   = "5f0c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b"
	guidOther = "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d"
	ep9000    = "http://127.0.0.1:9000"
	ep9001    = "http://127.0.0.1:9001"
)

func cos(names ...string) []tally.TallyCompany {
	out := make([]tally.TallyCompany, 0, len(names))
	for _, n := range names {
		out = append(out, tally.TallyCompany{Name: n})
	}
	return out
}

// probe builds a port's two answers: list = List of Companies names (no
// GUIDs, as real Tally), ids = Type:Company name/GUID pairs.
func idProbe(ep string, list []string, ids ...tally.TallyCompany) tally.ProbeResult {
	return tally.ProbeResult{Endpoint: ep, IsTally: true, Reachable: true, Companies: cos(list...),
		Identity: &tally.IdentityProbe{Companies: ids}}
}

func kbsMapping(guid string) ingest.ActiveMapping {
	return ingest.ActiveMapping{MappingID: "map-kbs", TallyEndpoint: ep9000, TallyCompanyName: "KBS Exports", TallyCompanyGUID: guid}
}

func TestCheckMappingIdentity_Verdicts(t *testing.T) {
	kbs := tally.TallyCompany{Name: "KBS Exports", GUID: guidKBS}
	cases := []struct {
		name string
		m    ingest.ActiveMapping
		p    tally.ProbeResult
		want IdentityVerdict
	}{
		{"match", kbsMapping(guidKBS), idProbe(ep9000, []string{"KBS Exports"}, kbs), IdentityMatch},
		{"match case-insensitive guid", kbsMapping(strings.ToUpper(guidKBS)), idProbe(ep9000, []string{"KBS Exports"}, kbs), IdentityMatch},
		{"no stored guid", kbsMapping(""), idProbe(ep9000, []string{"KBS Exports"}, kbs), IdentityNoStoredGUID},
		{"same name other guid", kbsMapping(guidOther), idProbe(ep9000, []string{"KBS Exports"}, kbs), IdentityGUIDMismatch},
		{"guid probe lacks company", kbsMapping(guidKBS), idProbe(ep9000, []string{"KBS Exports"}, tally.TallyCompany{Name: "Other", GUID: guidOther}), IdentityProbesDisagree},
		{"list lacks company", kbsMapping(guidKBS), idProbe(ep9000, []string{"Other"}, kbs), IdentityProbesDisagree},
		{"sets differ elsewhere", kbsMapping(guidKBS), idProbe(ep9000, []string{"KBS Exports"}, kbs, tally.TallyCompany{Name: "Closed Co", GUID: guidOther}), IdentityProbesDisagree},
		{"blank guid", kbsMapping(guidKBS), idProbe(ep9000, []string{"KBS Exports"}, tally.TallyCompany{Name: "KBS Exports"}), IdentityGUIDNotReturned},
		{"two guids", kbsMapping(guidKBS), idProbe(ep9000, []string{"KBS Exports"}, kbs, tally.TallyCompany{Name: "KBS Exports", GUID: guidOther}), IdentityAmbiguous},
		{"not open", kbsMapping(guidKBS), idProbe(ep9000, []string{"Other"}, tally.TallyCompany{Name: "Other", GUID: guidOther}), IdentityNotOpen},
		{"unreachable", kbsMapping(guidKBS), tally.ProbeResult{Endpoint: ep9000, Err: errors.New("refused")}, IdentityUnreachable},
		{"guid probe failed", kbsMapping(guidKBS), tally.ProbeResult{Endpoint: ep9000, Companies: cos("KBS Exports"), Identity: &tally.IdentityProbe{Err: errors.New("LINEERROR")}}, IdentityUnavailable},
		{"guid probe not run", kbsMapping(guidKBS), tally.ProbeResult{Endpoint: ep9000, Companies: cos("KBS Exports")}, IdentityUnavailable},
	}
	for _, tc := range cases {
		got := CheckMappingIdentity(tc.m, tc.p)
		if got.Verdict != tc.want {
			t.Errorf("%s: verdict = %s (%s), want %s", tc.name, got.Verdict, got.Reason, tc.want)
		}
		if got.Reason == "" {
			t.Errorf("%s: no reason", tc.name)
		}
	}
}

func TestCheckMappingIdentity_OpenCarriesGUIDs(t *testing.T) {
	c := CheckMappingIdentity(kbsMapping(""), idProbe(ep9000, []string{"KBS Exports"}, tally.TallyCompany{Name: "KBS Exports", GUID: guidKBS}))
	if len(c.Open) != 1 || c.Open[0].GUID != guidKBS {
		t.Fatalf("Open = %+v", c.Open)
	}
	if s := c.Summary(); !strings.Contains(s, "no_stored_guid") || !strings.Contains(s, "5f0c1a2b") || !strings.Contains(s, "stored: none") {
		t.Fatalf("Summary = %q", s)
	}
}

// Observe mode is today's gate: a GUID the new probe proves is another
// company does not block, and a legacy mapping keeps syncing.
func TestObserveMode_GateUnchanged(t *testing.T) {
	probes := []tally.ProbeResult{idProbe(ep9000, []string{"KBS Exports"}, tally.TallyCompany{Name: "KBS Exports", GUID: guidKBS})}
	for _, guid := range []string{"", guidOther} {
		resolved, _, skipped := ResolveMappingEndpoints([]ingest.ActiveMapping{kbsMapping(guid)}, probes)
		if len(resolved) != 1 || len(skipped) != 0 {
			t.Fatalf("guid %q: observe mode changed the gate: resolved=%d skipped=%+v", guid, len(resolved), skipped)
		}
	}
	obs := ObserveIdentity([]ingest.ActiveMapping{kbsMapping(guidOther)}, probes)
	if len(obs) != 1 || obs[0].Verdict != IdentityGUIDMismatch {
		t.Fatalf("observation = %+v", obs)
	}
}

func TestEnforced_MatchOnMappedPort(t *testing.T) {
	probes := []tally.ProbeResult{idProbe(ep9000, []string{"KBS Exports"}, tally.TallyCompany{Name: "KBS Exports", GUID: guidKBS})}
	resolved, followed, skipped, checks := ResolveMappingEndpointsEnforced([]ingest.ActiveMapping{kbsMapping(guidKBS)}, probes)
	if len(resolved) != 1 || resolved[0].FetchEndpoint != "" || len(followed) != 0 || len(skipped) != 0 {
		t.Fatalf("resolved=%+v followed=%+v skipped=%+v", resolved, followed, skipped)
	}
	if len(checks) != 1 || checks[0].Verdict != IdentityMatch {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestEnforced_DoubtsBlock(t *testing.T) {
	kbs := tally.TallyCompany{Name: "KBS Exports", GUID: guidKBS}
	cases := []struct {
		name       string
		guid       string
		probes     []tally.ProbeResult
		wantResult string
		wantReason string
	}{
		{"no stored guid", "", []tally.ProbeResult{idProbe(ep9000, []string{"KBS Exports"}, kbs)}, ingest.ConnStatusMappingMismatch, "no stored Tally company GUID"},
		{"same name other guid", guidOther, []tally.ProbeResult{idProbe(ep9000, []string{"KBS Exports"}, kbs)}, ingest.ConnStatusWrongCompanyOpen, "another company with the same name"},
		{"stored guid not returned", guidKBS, []tally.ProbeResult{idProbe(ep9000, []string{"KBS Exports"})}, ingest.ConnStatusWrongCompanyOpen, "identity unverified"},
		{"probes disagree", guidKBS, []tally.ProbeResult{idProbe(ep9000, []string{"KBS Exports"}, kbs, tally.TallyCompany{Name: "Closed", GUID: guidOther})}, ingest.ConnStatusWrongCompanyOpen, "list different companies"},
		{"guid probe failed", guidKBS, []tally.ProbeResult{{Endpoint: ep9000, Companies: cos("KBS Exports"), Identity: &tally.IdentityProbe{Err: errors.New("STATUS 0")}}}, ingest.ConnStatusWrongCompanyOpen, "GUID probe on 127.0.0.1:9000 failed"},
		{"blank guid", guidKBS, []tally.ProbeResult{idProbe(ep9000, []string{"KBS Exports"}, tally.TallyCompany{Name: "KBS Exports"})}, ingest.ConnStatusWrongCompanyOpen, "without a GUID"},
		// Mapped port is clean-not-open, the company's other copy is
		// doubtful: never follow on doubt, even with a clean match too.
		{"doubt elsewhere blocks follow", guidKBS, []tally.ProbeResult{
			idProbe(ep9000, []string{"Other"}, tally.TallyCompany{Name: "Other", GUID: guidOther}),
			idProbe(ep9001, []string{"KBS Exports"}, kbs),
			idProbe("http://127.0.0.1:9002", []string{"KBS Exports"}),
		}, ingest.ConnStatusWrongCompanyOpen, "9002"},
	}
	for _, tc := range cases {
		resolved, followed, skipped, _ := ResolveMappingEndpointsEnforced([]ingest.ActiveMapping{kbsMapping(tc.guid)}, tc.probes)
		if len(resolved) != 0 || len(followed) != 0 || len(skipped) != 1 {
			t.Errorf("%s: resolved=%+v followed=%+v skipped=%+v; want blocked", tc.name, resolved, followed, skipped)
			continue
		}
		sk := skipped[0]
		if sk.Result != tc.wantResult || !strings.Contains(sk.Reason, tc.wantReason) || !strings.HasPrefix(sk.Reason, "identity check (enforced): ") {
			t.Errorf("%s: skipped = %s %q; want %s containing %q", tc.name, sk.Result, sk.Reason, tc.wantResult, tc.wantReason)
		}
	}
}

func TestEnforced_FollowsOnlyAVerifiedCopy(t *testing.T) {
	kbs := tally.TallyCompany{Name: "KBS Exports", GUID: guidKBS}
	probes := []tally.ProbeResult{
		idProbe(ep9000, []string{"Other"}, tally.TallyCompany{Name: "Other", GUID: guidOther}),
		idProbe(ep9001, []string{"KBS Exports"}, kbs),
	}
	resolved, followed, skipped, _ := ResolveMappingEndpointsEnforced([]ingest.ActiveMapping{kbsMapping(guidKBS)}, probes)
	if len(resolved) != 1 || resolved[0].FetchEndpoint != ep9001 || len(followed) != 1 || len(skipped) != 0 {
		t.Fatalf("resolved=%+v followed=%+v skipped=%+v", resolved, followed, skipped)
	}

	// Two verified copies: not guessing.
	probes = append(probes, idProbe("http://127.0.0.1:9002", []string{"KBS Exports"}, kbs))
	resolved, _, skipped, _ = ResolveMappingEndpointsEnforced([]ingest.ActiveMapping{kbsMapping(guidKBS)}, probes)
	if len(resolved) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0].Reason, "several Tally ports") {
		t.Fatalf("resolved=%+v skipped=%+v", resolved, skipped)
	}
}

func TestEnforced_NotOpenAndUnreachableAsBefore(t *testing.T) {
	_, _, skipped, _ := ResolveMappingEndpointsEnforced([]ingest.ActiveMapping{kbsMapping(guidKBS)},
		[]tally.ProbeResult{idProbe(ep9000, []string{"Other"}, tally.TallyCompany{Name: "Other", GUID: guidOther})})
	if len(skipped) != 1 || skipped[0].Result != ingest.ConnStatusWrongCompanyOpen || len(skipped[0].OpenCompanies) != 1 || skipped[0].OpenCompanies[0].GUID != guidOther {
		t.Fatalf("not open: %+v", skipped)
	}
	_, _, skipped, _ = ResolveMappingEndpointsEnforced([]ingest.ActiveMapping{kbsMapping(guidKBS)},
		[]tally.ProbeResult{{Endpoint: ep9000, Err: errors.New("refused")}})
	if len(skipped) != 1 || skipped[0].Result != ingest.ConnStatusEndpointUnreachable {
		t.Fatalf("unreachable: %+v", skipped)
	}
}
