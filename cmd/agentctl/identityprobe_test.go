package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

type fakeMappings struct{ ms []ingest.ActiveMapping }

func (f fakeMappings) FetchActiveMappings(context.Context) (*ingest.ActiveMappingsResponse, error) {
	return &ingest.ActiveMappingsResponse{Mappings: f.ms}, nil
}

const (
	ipSecret = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	ipBearer = "FAKE-identity-probe-bearer-not-real-0000"
)

func identityProbeFixture(t *testing.T) (identityProbeDeps, string, *tally.DiscoverOptions) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := config.Save(cfgPath, &config.Config{Server: "https://gstreco.example", ConnectionID: "conn-1", TallyEndpoints: []string{"http://127.0.0.1:9558"}}); err != nil {
		t.Fatal(err)
	}
	ks := keyring.NewMemoryStore()
	hk, bk := keyring.ConnectionKeys("conn-1")
	_ = ks.Set(keyring.ServiceName, hk, ipSecret)
	_ = ks.Set(keyring.ServiceName, bk, ipBearer)
	var seen tally.DiscoverOptions
	deps := identityProbeDeps{
		loadConfig:     config.Load,
		newOSKeyring:   func() keyring.Store { return ks },
		listeningPorts: func() ([]int, error) { return []int{9012}, nil },
		discover: func(_ context.Context, opts tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			seen = opts
			return []tally.ProbeResult{
				{Endpoint: "http://127.0.0.1:9558", IsTally: true, Reachable: true,
					Companies: []tally.TallyCompany{{Name: "KBS Exports"}, {Name: "KSS Textiles"}},
					Identity: &tally.IdentityProbe{Companies: []tally.TallyCompany{
						{Name: "KBS Exports", GUID: "5f0c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b"},
						{Name: "KSS Textiles", GUID: "9a8b7c6d-5e4f-4a3b-9c2d-1e0f9a8b7c6d"},
					}}},
				{Endpoint: "http://127.0.0.1:9012", IsTally: true, Reachable: true,
					Companies: []tally.TallyCompany{{Name: "Colouredge Prints"}},
					Identity:  &tally.IdentityProbe{Err: errorString("tally: company-identity request returned STATUS 0")}},
			}, nil
		},
		newMappingsFrom: func(_, _, bearer, secret string) (mappingsFetcher, error) {
			if bearer != ipBearer || secret != ipSecret {
				t.Fatal("wrong credentials handed to the mappings client")
			}
			return fakeMappings{ms: []ingest.ActiveMapping{
				{MappingID: "map-kbs", TallyEndpoint: "http://127.0.0.1:9558", TallyCompanyName: "KBS Exports"},
				{MappingID: "map-kss", TallyEndpoint: "http://127.0.0.1:9558", TallyCompanyName: "KSS Textiles", TallyCompanyGUID: "11112222-3333-4444-5555-666677778888"},
			}}, nil
		},
	}
	return deps, cfgPath, &seen
}

type errorString string

func (e errorString) Error() string { return string(e) }

func TestIdentityProbe_JSON(t *testing.T) {
	deps, cfgPath, seen := identityProbeFixture(t)
	var out, errb bytes.Buffer
	if code := runIdentityProbe(&out, &errb, []string{"--config", cfgPath, "--json"}, deps); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if !seen.IdentityProbe {
		t.Fatal("identity probe not requested")
	}
	if strings.Join(seen.Endpoints, ",") != "http://127.0.0.1:9558,http://127.0.0.1:9012" {
		t.Fatalf("endpoints = %v, want configured+mapped then listening", seen.Endpoints)
	}
	var rep identityProbeReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if rep.Mode != "observe" || len(rep.Endpoints) != 2 || len(rep.Connections) != 1 || rep.Connections[0].Mappings != 2 {
		t.Fatalf("report = %+v", rep)
	}
	e := rep.Endpoints[0]
	if len(e.ListOfCompanies) != 2 || len(e.TypeCompany) != 2 || !e.ProbesAgree || len(e.Mappings) != 2 {
		t.Fatalf("9558 row = %+v", e)
	}
	got := map[string]string{}
	for _, m := range e.Mappings {
		got[m.MappingID] = m.Verdict
	}
	if got["map-kbs"] != "no_stored_guid" || got["map-kss"] != "guid_mismatch" {
		t.Fatalf("verdicts = %v", got)
	}
	if rep.Endpoints[1].TypeCompanyError == "" || rep.Endpoints[1].ProbesAgree || len(rep.Endpoints[1].Mappings) != 0 {
		t.Fatalf("9012 row = %+v", rep.Endpoints[1])
	}
	for _, s := range []string{ipBearer, ipSecret} {
		if strings.Contains(out.String()+errb.String(), s) {
			t.Fatal("output contains a credential")
		}
	}
}

func TestIdentityProbe_Table(t *testing.T) {
	deps, cfgPath, _ := identityProbeFixture(t)
	var out, errb bytes.Buffer
	if code := runIdentityProbe(&out, &errb, []string{"--config", cfgPath}, deps); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"company identity gate: observe",
		"== http://127.0.0.1:9558",
		"List of Companies (2)",
		"Type:Company NAME+GUID (2)",
		"5f0c1a2b-3c4d-4e5f-8a9b-0c1d2e3f4a5b",
		"probes agree on the company set: yes",
		"stored guid: (none)",
		"verdict: no_stored_guid",
		"verdict: guid_mismatch",
		"Type:Company NAME+GUID: FAILED",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("table missing %q:\n%s", want, s)
		}
	}
}

func TestIdentityProbe_NoMappingsSkipsKeyring(t *testing.T) {
	deps, cfgPath, _ := identityProbeFixture(t)
	deps.newOSKeyring = func() keyring.Store { t.Fatal("keyring read with --no-mappings"); return nil }
	var out, errb bytes.Buffer
	if code := runIdentityProbe(&out, &errb, []string{"--config", cfgPath, "--no-mappings", "--endpoint", "http://127.0.0.1:9558"}, deps); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}

func TestIdentityProbe_KeyringMissingIsExit1ButStillProbes(t *testing.T) {
	deps, cfgPath, _ := identityProbeFixture(t)
	deps.newOSKeyring = func() keyring.Store { return keyring.NewMemoryStore() }
	var out, errb bytes.Buffer
	if code := runIdentityProbe(&out, &errb, []string{"--config", cfgPath}, deps); code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(out.String(), "mappings not fetched") || !strings.Contains(out.String(), "List of Companies") {
		t.Fatalf("output:\n%s", out.String())
	}
}
