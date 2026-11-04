package main

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// Company identity gate (QA 04-Nov N1), end to end through runWindowSync
// against a fake Tally whose List of Companies has names only (as real
// Tally) and whose Type:Company probe returns GUIDs.

const (
	gateGUID  = "5f0c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b"
	otherGUID = "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d"
)

func runGate(t *testing.T, enforce bool, storedGUID string, list, identity []tallyCo) (*fakeTallyServer, *ingest.ConnectionStatus) {
	t.Helper()
	tl := newFakeTallySplit(t, list, identity)
	srv := newFakeGSTReco(t)
	srv.validBearer["conn-kbs"] = fakeBearerA
	srv.mappings["conn-kbs"] = []ingest.ActiveMapping{{
		MappingID: "map-kbs", TallyEndpoint: tl.srv.URL, TallyCompanyName: "KBS Exports", TallyCompanyGUID: storedGUID,
	}}
	rt := newTestRuntime(t, srv.srv.URL, "conn-kbs", fakeBearerA, fakeSecretA)
	rt.identityEnforce = enforce
	from, to := syncWindow()
	_ = runWindowSync(context.Background(), rt, tally.NewClient, zerolog.Nop(), from, to, "manual", scheduledScope)
	return tl, rt.status.snapshot()
}

var kbsNameOnly = []tallyCo{{"KBS Exports", ""}}

// Observe (default): a legacy mapping with no GUID syncs exactly as
// before, and connection_status reports both probes.
func TestIdentityObserve_LegacyMappingStillSyncsAndReports(t *testing.T) {
	tl, st := runGate(t, false, "", kbsNameOnly, []tallyCo{{"KBS Exports", gateGUID}})
	if v, _ := tl.fetches(); v == 0 {
		t.Fatal("observe mode stopped a sync that ran before")
	}
	if st.Result != ingest.ConnStatusSuccess {
		t.Fatalf("result = %s %q", st.Result, deref(st.Detail))
	}
	d := deref(st.Detail)
	if !strings.Contains(d, "identity[observe]") || !strings.Contains(d, "no_stored_guid") || !strings.Contains(d, "5f0c1a2b") {
		t.Fatalf("detail = %q, want the observe note", d)
	}
	if len(st.OpenCompanies) != 1 || deref(st.OpenCompanies[0].GUID) != gateGUID {
		t.Fatalf("open_companies = %+v, want the GUID from the Type:Company probe", st.OpenCompanies)
	}
}

// Observe: even a GUID mismatch the new probe proves does not change the
// gate (it is only reported) — enforcement is opt-in.
func TestIdentityObserve_MismatchOnlyReported(t *testing.T) {
	tl, st := runGate(t, false, otherGUID, kbsNameOnly, []tallyCo{{"KBS Exports", gateGUID}})
	if v, _ := tl.fetches(); v == 0 {
		t.Fatal("observe mode blocked the sync")
	}
	if !strings.Contains(deref(st.Detail), "guid_mismatch") {
		t.Fatalf("detail = %q", deref(st.Detail))
	}
}

// Observe: the GUID probe failing (older Tally) changes nothing.
func TestIdentityObserve_ProbeRefusedChangesNothing(t *testing.T) {
	tl, st := runGate(t, false, "", kbsNameOnly, nil)
	if v, _ := tl.fetches(); v == 0 || st.Result != ingest.ConnStatusSuccess {
		t.Fatalf("result = %s, vouchers = %d", st.Result, v)
	}
	if !strings.Contains(deref(st.Detail), "identity_unavailable") {
		t.Fatalf("detail = %q", deref(st.Detail))
	}
}

func TestIdentityEnforce_MatchSyncs(t *testing.T) {
	tl, st := runGate(t, true, gateGUID, kbsNameOnly, []tallyCo{{"KBS Exports", gateGUID}})
	if v, _ := tl.fetches(); v == 0 || st.Result != ingest.ConnStatusSuccess {
		t.Fatalf("result = %s %q, vouchers = %d", st.Result, deref(st.Detail), v)
	}
	if !strings.Contains(deref(st.Detail), "identity[enforce]") || !strings.Contains(deref(st.Detail), "match") {
		t.Fatalf("detail = %q", deref(st.Detail))
	}
}

// Enforce: every identity doubt blocks before any Tally fetch, with a
// visible reason; never name-only trust.
func TestIdentityEnforce_DoubtBlocks(t *testing.T) {
	cases := []struct {
		name       string
		stored     string
		identity   []tallyCo
		wantResult string
		wantDetail string
	}{
		{"no stored guid", "", []tallyCo{{"KBS Exports", gateGUID}}, ingest.ConnStatusMappingMismatch, "no stored Tally company GUID"},
		{"same name other guid", otherGUID, []tallyCo{{"KBS Exports", gateGUID}}, ingest.ConnStatusWrongCompanyOpen, "same name"},
		{"stored guid not returned", gateGUID, []tallyCo{}, ingest.ConnStatusWrongCompanyOpen, "identity unverified"},
		{"probes disagree", gateGUID, []tallyCo{{"KBS Exports", gateGUID}, {"Closed Co", otherGUID}}, ingest.ConnStatusWrongCompanyOpen, "list different companies"},
		{"guid probe refused", gateGUID, nil, ingest.ConnStatusWrongCompanyOpen, "GUID probe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl, st := runGate(t, true, tc.stored, kbsNameOnly, tc.identity)
			if v, o := tl.fetches(); v != 0 || o != 0 {
				t.Fatalf("Tally fetched (%d vouchers, %d other) despite an identity doubt", v, o)
			}
			d := deref(st.Detail)
			if st.Result != tc.wantResult || !strings.Contains(d, tc.wantDetail) || !strings.Contains(d, "identity check (enforced)") {
				t.Fatalf("status = %s %q, want %s containing %q", st.Result, d, tc.wantResult, tc.wantDetail)
			}
			if len(d) > ingest.MaxConnectionStatusDetail {
				t.Fatalf("detail too long: %d", len(d))
			}
		})
	}
}
