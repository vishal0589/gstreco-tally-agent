package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func TestParsePortRange(t *testing.T) {
	cases := []struct {
		in      string
		want    [2]int
		wantErr bool
	}{
		{"9000-9009", [2]int{9000, 9009}, false},
		{"9000", [2]int{9000, 9000}, false},
		{"  9000-9009  ", [2]int{9000, 9009}, false},
		{"", tally.DefaultDiscoverPortRange, false},
		{"abc-def", [2]int{0, 0}, true},
		{"9000-abc", [2]int{0, 0}, true},
	}
	for _, c := range cases {
		got, err := parsePortRange(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parsePortRange(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if got != c.want {
			t.Errorf("parsePortRange(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStringSlice_AccumulatesEndpoints(t *testing.T) {
	var s stringSlice
	if err := s.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual([]string(s), []string{"a", "b"}) {
		t.Errorf("got %v", s)
	}
	if s.String() != "a,b" {
		t.Errorf("String=%q", s.String())
	}
}

type fakeCatalogSender struct {
	bodies []ingest.CatalogRequest
	err    error
}

func (f *fakeCatalogSender) SendCatalog(_ context.Context, body ingest.CatalogRequest) error {
	f.bodies = append(f.bodies, body)
	return f.err
}

// stubDiscover returns a fixed list of probe results — caller controls
// what discoverCmd sees from "the network."
func stubDiscover(results []tally.ProbeResult) func(context.Context, tally.DiscoverOptions) ([]tally.ProbeResult, error) {
	return func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
		out := make([]tally.ProbeResult, len(results))
		copy(out, results)
		return out, nil
	}
}

func TestRunDiscover_HappyPath_SavesAndPushes(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := config.Save(cfgPath, &config.Config{
		Server:       "https://gstreco.example",
		ConnectionID: "conn-1",
		DeviceName:   "WIN-DESK",
	}); err != nil {
		t.Fatal(err)
	}

	ks := keyring.NewMemoryStore()
	hk, bk := keyring.ConnectionKeys("conn-1")
	_ = ks.Set(keyring.ServiceName, hk, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	_ = ks.Set(keyring.ServiceName, bk, "bearer-tok")

	sender := &fakeCatalogSender{}
	results := []tally.ProbeResult{
		{
			Endpoint:   "http://127.0.0.1:9000",
			Reachable:  true,
			IsTally:    true,
			Version:    tally.VersionV3,
			VersionStr: "Release 3.0.1",
			Companies: []tally.TallyCompany{
				{Name: "PLLUM CASA", GUID: "g1"},
				{Name: "PLLUM LEGNO", GUID: "g2"},
			},
			LatencyMs: 32,
		},
		{
			Endpoint:   "http://127.0.0.1:9001",
			Reachable:  true,
			IsTally:    true,
			Version:    tally.VersionV3,
			VersionStr: "Release 3.0.1",
			Companies:  []tally.TallyCompany{{Name: "ACME"}},
			LatencyMs:  41,
		},
		{
			Endpoint: "http://127.0.0.1:9002",
			Err:      errors.New("connect refused"),
		},
	}

	deps := discoverDeps{
		now:          func() time.Time { return time.Unix(1700000000, 0) },
		newOSKeyring: func() keyring.Store { return ks },
		discover:     stubDiscover(results),
		newCatalogClient: func(_, _, _, _ string) (catalogSender, error) {
			return sender, nil
		},
		loadConfig: config.Load,
		saveConfig: config.Save,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscover(&stdout, &stderr, []string{"--config", cfgPath}, deps)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s, stdout=%s", code, stderr.String(), stdout.String())
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://127.0.0.1:9000", "http://127.0.0.1:9001"}
	if !reflect.DeepEqual(cfg.TallyEndpoints, want) {
		t.Errorf("TallyEndpoints persisted = %v, want %v", cfg.TallyEndpoints, want)
	}

	if len(sender.bodies) != 1 {
		t.Fatalf("catalog pushes = %d, want 1", len(sender.bodies))
	}
	body := sender.bodies[0]
	if len(body.Items) != 3 {
		t.Errorf("Items = %d, want 3 (2 + 1)", len(body.Items))
	}
	gotPaired := map[string]string{}
	for _, it := range body.Items {
		var ep string
		if it.TallyEndpoint != nil {
			ep = *it.TallyEndpoint
		}
		gotPaired[it.TallyCompanyName] = ep
	}
	if gotPaired["PLLUM CASA"] != "http://127.0.0.1:9000" {
		t.Errorf("PLLUM CASA endpoint = %q", gotPaired["PLLUM CASA"])
	}
	if gotPaired["ACME"] != "http://127.0.0.1:9001" {
		t.Errorf("ACME endpoint = %q", gotPaired["ACME"])
	}

	if !strings.Contains(stdout.String(), "saved 2 endpoint(s)") {
		t.Errorf("stdout missing save line: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "pushed catalog (3 companies across 2 endpoints") {
		t.Errorf("stdout missing push line: %s", stdout.String())
	}
}

func TestRunDiscover_DryRunSkipsSaveAndPush(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := config.Save(cfgPath, &config.Config{
		Server: "https://x", ConnectionID: "c",
	}); err != nil {
		t.Fatal(err)
	}

	sender := &fakeCatalogSender{}
	deps := discoverDeps{
		now:          time.Now,
		newOSKeyring: func() keyring.Store { return keyring.NewMemoryStore() },
		discover: stubDiscover([]tally.ProbeResult{{
			Endpoint:  "http://127.0.0.1:9000",
			IsTally:   true,
			Version:   tally.VersionV3,
			Companies: []tally.TallyCompany{{Name: "X"}},
			Reachable: true,
		}}),
		newCatalogClient: func(_, _, _, _ string) (catalogSender, error) { return sender, nil },
		loadConfig:       config.Load,
		saveConfig:       config.Save,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscover(&stdout, &stderr, []string{"--config", cfgPath, "--dry-run"}, deps)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	if len(sender.bodies) != 0 {
		t.Errorf("dry-run pushed %d bodies; want 0", len(sender.bodies))
	}
	cfg, _ := config.Load(cfgPath)
	if len(cfg.TallyEndpoints) != 0 {
		t.Errorf("dry-run wrote endpoints to config: %v", cfg.TallyEndpoints)
	}
}

func TestRunDiscover_NoTallyExits2(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	_ = config.Save(cfgPath, &config.Config{Server: "https://x", ConnectionID: "c"})

	deps := discoverDeps{
		now:          time.Now,
		newOSKeyring: func() keyring.Store { return keyring.NewMemoryStore() },
		discover: stubDiscover([]tally.ProbeResult{
			{Endpoint: "http://127.0.0.1:9000", Err: errors.New("refused")},
			{Endpoint: "http://127.0.0.1:9001", Err: errors.New("refused")},
		}),
		newCatalogClient: func(_, _, _, _ string) (catalogSender, error) { return &fakeCatalogSender{}, nil },
		loadConfig:       config.Load,
		saveConfig:       config.Save,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscover(&stdout, &stderr, []string{"--config", cfgPath}, deps)
	if code != 2 {
		t.Fatalf("exit=%d, want 2 (no Tally found)", code)
	}
	if !strings.Contains(stderr.String(), "no Tally instances were found on the scanned ports") {
		t.Errorf("stderr missing helpful message: %s", stderr.String())
	}
}

func TestRunDiscover_V4OnlyExits2WithWarning(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	_ = config.Save(cfgPath, &config.Config{Server: "https://x", ConnectionID: "c"})

	// v0.1.3: V4 endpoint is no longer filtered out, so the discover
	// flow now reaches the catalog-push step. Pre-populate the
	// keyring with the expected secret material so the push doesn't
	// fall over reading missing entries.
	ks := keyring.NewMemoryStore()
	hmacKey, bearerKey := keyring.ConnectionKeys("c")
	_ = ks.Set(keyring.ServiceName, hmacKey, "hmac-secret")
	_ = ks.Set(keyring.ServiceName, bearerKey, "bearer-token")

	deps := discoverDeps{
		now:          time.Now,
		newOSKeyring: func() keyring.Store { return ks },
		discover: stubDiscover([]tally.ProbeResult{
			{
				Endpoint:   "http://127.0.0.1:9000",
				Reachable:  true,
				IsTally:    true,
				Version:    tally.VersionV4,
				VersionStr: "Release 4.1",
				Companies:  []tally.TallyCompany{{Name: "X"}},
				Warnings:   []string{"Tally Prime 4.x detected — parser compatibility not guaranteed."},
			},
		}),
		newCatalogClient: func(_, _, _, _ string) (catalogSender, error) { return &fakeCatalogSender{}, nil },
		loadConfig:       config.Load,
		saveConfig:       config.Save,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscover(&stdout, &stderr, []string{"--config", cfgPath}, deps)
	// Pre-v0.1.3 we exited 2 here ("V4-only is not pilotable"). v0.1.3
	// treats Tally Prime 3.x/4.x/5.x/6.x as protocol-identical for the
	// HTTP/XML queries this agent issues — verified against the
	// production-shipping Manual2AI Python adapter, which doesn't
	// version-gate at all. Expectation now: exit 0, V4 endpoint
	// surfaces in the catalog along with its warning.
	if code != 0 {
		t.Fatalf("exit=%d, want 0 (v0.1.3: any reachable Tally is pilotable)", code)
	}
	if !strings.Contains(stdout.String(), "4.x detected") {
		t.Errorf("stdout missing V4 warning (operator should still see version note): %s", stdout.String())
	}
}

func TestRunDiscover_NotPairedExits2(t *testing.T) {
	deps := discoverDeps{
		now:              time.Now,
		newOSKeyring:     func() keyring.Store { return keyring.NewMemoryStore() },
		discover:         stubDiscover(nil),
		newCatalogClient: func(_, _, _, _ string) (catalogSender, error) { return &fakeCatalogSender{}, nil },
		loadConfig: func(string) (*config.Config, error) {
			return nil, config.ErrNotFound
		},
		saveConfig: config.Save,
	}
	var stdout, stderr bytes.Buffer
	code := runDiscover(&stdout, &stderr, []string{}, deps)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "not paired") {
		t.Errorf("stderr missing 'not paired': %s", stderr.String())
	}
}

func TestRunDiscover_NoCompaniesLoadedSucceedsWithoutPush(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	_ = config.Save(cfgPath, &config.Config{Server: "https://x", ConnectionID: "c"})

	sender := &fakeCatalogSender{}
	deps := discoverDeps{
		now:          time.Now,
		newOSKeyring: func() keyring.Store { return keyring.NewMemoryStore() },
		discover: stubDiscover([]tally.ProbeResult{{
			Endpoint:   "http://127.0.0.1:9000",
			Reachable:  true,
			IsTally:    true,
			Version:    tally.VersionV3,
			VersionStr: "Release 3.0.1",
			// Tally is up but no company is loaded. Common during a
			// post-reboot cold start.
		}}),
		newCatalogClient: func(_, _, _, _ string) (catalogSender, error) { return sender, nil },
		loadConfig:       config.Load,
		saveConfig:       config.Save,
	}

	var stdout, stderr bytes.Buffer
	code := runDiscover(&stdout, &stderr, []string{"--config", cfgPath}, deps)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	if len(sender.bodies) != 0 {
		t.Errorf("expected no catalog push when no companies loaded; got %d", len(sender.bodies))
	}
	if !strings.Contains(stdout.String(), "no companies to push") {
		t.Errorf("stdout missing 'no companies' line: %s", stdout.String())
	}
	// But the endpoint should still be saved — the daemon will pick
	// it up once the operator opens a company in Tally.
	cfg, _ := config.Load(cfgPath)
	if len(cfg.TallyEndpoints) != 1 {
		t.Errorf("endpoint not saved: %v", cfg.TallyEndpoints)
	}
}

func TestBuildCatalogItems_FlattensAcrossEndpoints(t *testing.T) {
	results := []tally.ProbeResult{
		{Endpoint: "http://127.0.0.1:9000", Companies: []tally.TallyCompany{{Name: "A"}, {Name: "B", GUID: "gB"}}},
		{Endpoint: "http://127.0.0.1:9001", Companies: []tally.TallyCompany{{Name: "C"}}},
	}
	items := buildCatalogItems(results)
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	for _, it := range items {
		if it.TallyEndpoint == nil {
			t.Errorf("item %s has nil endpoint", it.TallyCompanyName)
		}
	}
	if items[1].TallyCompanyGUID == nil || *items[1].TallyCompanyGUID != "gB" {
		t.Errorf("item B GUID = %v", items[1].TallyCompanyGUID)
	}
}
