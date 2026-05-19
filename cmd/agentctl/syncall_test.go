package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// fakeMappingsTallyClient returns the same canned XML for every PostXML
// call regardless of envelope shape. Good enough for sync-all tests
// that don't care about the per-kind envelope contents — only that
// the pipeline runs through to ingest.
type fakeMappingsTallyClient struct {
	resp []byte
	err  error
	got  [][]byte
}

func (f *fakeMappingsTallyClient) PostXML(_ context.Context, body []byte) ([]byte, error) {
	f.got = append(f.got, body)
	return f.resp, f.err
}

// fakeSyncAllIngest acts as both ingest sender and mapping fetcher —
// matches the syncAllIngestClient interface so we can inject one fake
// in place of the real ingest.Client.
type fakeSyncAllIngest struct {
	mappings    *ingest.ActiveMappingsResponse
	fetchErr    error
	sent        []tally.IngestRequestBody
	sendErr     error
}

func (f *fakeSyncAllIngest) Send(_ context.Context, body tally.IngestRequestBody) error {
	f.sent = append(f.sent, body)
	return f.sendErr
}
func (f *fakeSyncAllIngest) FetchActiveMappings(_ context.Context) (*ingest.ActiveMappingsResponse, error) {
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	return f.mappings, nil
}

// stubResponse is shared with sync_test.go (same package).

func setupPairedConfig(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := config.Save(cfgPath, &config.Config{
		Server:       "https://gstreco.example",
		ConnectionID: "conn-1",
		DeviceName:   "WIN-DESK",
	}); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

func setupKeyring(t *testing.T, connectionID string) keyring.Store {
	t.Helper()
	ks := keyring.NewMemoryStore()
	hk, bk := keyring.ConnectionKeys(connectionID)
	_ = ks.Set(keyring.ServiceName, hk, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	_ = ks.Set(keyring.ServiceName, bk, "bearer-tok")
	return ks
}

func TestParseKinds(t *testing.T) {
	cases := []struct {
		in       string
		wantLen  int
		wantErr  bool
		wantNames []string
	}{
		{"purchase,sales", 2, false, []string{"purchase", "sales"}},
		{"sales, purchase, credit_note, debit_note", 4, false, []string{"sales", "purchase", "credit_note", "debit_note"}},
		{"sales,sales,sales", 1, false, []string{"sales"}}, // dedup
		{" purchase ", 1, false, []string{"purchase"}},
		{"", 0, true, nil},
		{",,,", 0, true, nil},
		{"journal", 0, true, nil},
	}
	for _, c := range cases {
		got, err := parseKinds(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parseKinds(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if len(got) != c.wantLen {
			t.Errorf("parseKinds(%q) len=%d, want %d", c.in, len(got), c.wantLen)
			continue
		}
		for i, n := range c.wantNames {
			if got[i].Name != n {
				t.Errorf("parseKinds(%q)[%d].Name=%q, want %q", c.in, i, got[i].Name, n)
			}
		}
	}
}

func TestRunSyncAll_HappyPath_WalksAllMappingsAndKinds(t *testing.T) {
	cfgPath := setupPairedConfig(t)
	ks := setupKeyring(t, "conn-1")

	tly := &fakeMappingsTallyClient{resp: []byte(stubResponse)}
	srvIngest := &fakeSyncAllIngest{
		mappings: &ingest.ActiveMappingsResponse{
			ConnectionID: "conn-1",
			Mappings: []ingest.ActiveMapping{
				{MappingID: "m-1", TallyEndpoint: "http://localhost:9000", TallyCompanyName: "PLLUM CASA", CompanyGSTINID: "gst-1"},
				{MappingID: "m-2", TallyEndpoint: "http://localhost:9001", TallyCompanyName: "PLLUM LEGNO", CompanyGSTINID: "gst-2"},
			},
			FetchedAt: "2026-04-26T10:00:00Z",
		},
	}

	deps := syncAllDeps{
		now:             func() time.Time { return time.Unix(1700000000, 0) },
		newOSKeyring:    func() keyring.Store { return ks },
		newTallyClient:  func(string) tallyPoster { return tly },
		newIngestClient: func(_, _, _, _ string) (syncAllIngestClient, error) { return srvIngest, nil },
		loadConfig:      config.Load,
	}

	var stdout, stderr bytes.Buffer
	code := runSyncAll(&stdout, &stderr, []string{
		"--config", cfgPath,
		"--period", "042026",
		"--kinds", "purchase,sales",
	}, deps)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s, stdout=%s", code, stderr.String(), stdout.String())
	}
	// 2 mappings × 2 kinds = 4 RunOne calls = 4 ingest sends (each
	// stub voucher produces one row → one batch).
	if got := len(srvIngest.sent); got != 4 {
		t.Errorf("ingest sends = %d, want 4", got)
	}
	// 2 mappings × 2 kinds = 4 Tally PostXML calls.
	if got := len(tly.got); got != 4 {
		t.Errorf("tally fetches = %d, want 4", got)
	}
	if !strings.Contains(stdout.String(), "summary: connections_with_mappings=1/1 · mappings_ran=2/2") {
		t.Errorf("stdout missing summary: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), "sync-all complete") {
		t.Errorf("stdout missing success line: %s", stdout.String())
	}
}

func TestRunSyncAll_NoActiveMappingsExits2(t *testing.T) {
	cfgPath := setupPairedConfig(t)
	ks := setupKeyring(t, "conn-1")

	srvIngest := &fakeSyncAllIngest{
		mappings: &ingest.ActiveMappingsResponse{
			ConnectionID: "conn-1",
			Mappings:     nil,
		},
	}

	deps := syncAllDeps{
		now:             time.Now,
		newOSKeyring:    func() keyring.Store { return ks },
		newTallyClient:  func(string) tallyPoster { return &fakeMappingsTallyClient{} },
		newIngestClient: func(_, _, _, _ string) (syncAllIngestClient, error) { return srvIngest, nil },
		loadConfig:      config.Load,
	}

	var stdout, stderr bytes.Buffer
	code := runSyncAll(&stdout, &stderr, []string{"--config", cfgPath, "--period", "042026"}, deps)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "no active mappings") {
		t.Errorf("stderr missing helpful message: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "agentctl discover") {
		t.Errorf("stderr should suggest discover: %s", stderr.String())
	}
}

func TestRunSyncAll_DryRunSkipsSends(t *testing.T) {
	cfgPath := setupPairedConfig(t)
	ks := setupKeyring(t, "conn-1")

	tly := &fakeMappingsTallyClient{resp: []byte(stubResponse)}
	srvIngest := &fakeSyncAllIngest{
		mappings: &ingest.ActiveMappingsResponse{
			ConnectionID: "conn-1",
			Mappings: []ingest.ActiveMapping{
				{MappingID: "m-1", TallyEndpoint: "http://localhost:9000", TallyCompanyName: "X", CompanyGSTINID: "g"},
			},
		},
		// If sender is called this would error — confirms dry-run path
		// uses noopSender instead of the fake.
		sendErr: errors.New("should not send during dry-run"),
	}

	deps := syncAllDeps{
		now:             time.Now,
		newOSKeyring:    func() keyring.Store { return ks },
		newTallyClient:  func(string) tallyPoster { return tly },
		newIngestClient: func(_, _, _, _ string) (syncAllIngestClient, error) { return srvIngest, nil },
		loadConfig:      config.Load,
	}

	var stdout, stderr bytes.Buffer
	code := runSyncAll(&stdout, &stderr, []string{
		"--config", cfgPath,
		"--period", "042026",
		"--kinds", "purchase",
		"--dry-run",
	}, deps)
	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	if len(srvIngest.sent) != 0 {
		t.Errorf("dry-run sent %d batches; want 0", len(srvIngest.sent))
	}
	if !strings.Contains(stdout.String(), "dry-run sync-all complete") {
		t.Errorf("missing dry-run line: %s", stdout.String())
	}
}

func TestRunSyncAll_ContinuesOnPerMappingFailureWhenFlagTrue(t *testing.T) {
	cfgPath := setupPairedConfig(t)
	ks := setupKeyring(t, "conn-1")

	// Tally returns malformed XML → ParseDayBookV3 errors fatally
	// for that endpoint. With --continue-on-error (default), other
	// mappings should still run.
	tly := &fakeMappingsTallyClient{resp: []byte("<broken")}
	srvIngest := &fakeSyncAllIngest{
		mappings: &ingest.ActiveMappingsResponse{
			ConnectionID: "conn-1",
			Mappings: []ingest.ActiveMapping{
				{MappingID: "m-1", TallyEndpoint: "http://localhost:9000", TallyCompanyName: "BadCo", CompanyGSTINID: "g1"},
				{MappingID: "m-2", TallyEndpoint: "http://localhost:9001", TallyCompanyName: "OkCo", CompanyGSTINID: "g2"},
			},
		},
	}

	deps := syncAllDeps{
		now:             time.Now,
		newOSKeyring:    func() keyring.Store { return ks },
		newTallyClient:  func(string) tallyPoster { return tly },
		newIngestClient: func(_, _, _, _ string) (syncAllIngestClient, error) { return srvIngest, nil },
		loadConfig:      config.Load,
	}

	var stdout, stderr bytes.Buffer
	code := runSyncAll(&stdout, &stderr, []string{
		"--config", cfgPath,
		"--period", "042026",
		"--kinds", "purchase",
	}, deps)
	if code != 1 {
		t.Fatalf("exit=%d, want 1 (fatal errors > 0)", code)
	}
	// fatal errors counted; both mappings attempted (4 PostXML calls
	// = 2 mappings × 1 kind × 2 fetches because malformed body still
	// triggers attempt — actually: 2 mappings × 1 kind = 2 PostXML
	// calls).
	if len(tly.got) != 2 {
		t.Errorf("PostXML attempts=%d, want 2", len(tly.got))
	}
	if !strings.Contains(stdout.String(), "fatal errors=2") {
		t.Errorf("summary should show fatal errors: %s", stdout.String())
	}
}

func TestRunSyncAll_StopsOnFatalWhenContinueOff(t *testing.T) {
	cfgPath := setupPairedConfig(t)
	ks := setupKeyring(t, "conn-1")

	tly := &fakeMappingsTallyClient{resp: []byte("<broken")}
	srvIngest := &fakeSyncAllIngest{
		mappings: &ingest.ActiveMappingsResponse{
			ConnectionID: "conn-1",
			Mappings: []ingest.ActiveMapping{
				{MappingID: "m-1", TallyEndpoint: "http://localhost:9000", TallyCompanyName: "BadCo", CompanyGSTINID: "g1"},
				{MappingID: "m-2", TallyEndpoint: "http://localhost:9001", TallyCompanyName: "OkCo", CompanyGSTINID: "g2"},
			},
		},
	}

	deps := syncAllDeps{
		now:             time.Now,
		newOSKeyring:    func() keyring.Store { return ks },
		newTallyClient:  func(string) tallyPoster { return tly },
		newIngestClient: func(_, _, _, _ string) (syncAllIngestClient, error) { return srvIngest, nil },
		loadConfig:      config.Load,
	}

	var stdout, stderr bytes.Buffer
	code := runSyncAll(&stdout, &stderr, []string{
		"--config", cfgPath,
		"--period", "042026",
		"--kinds", "purchase",
		"--continue-on-error=false",
	}, deps)
	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if len(tly.got) != 1 {
		t.Errorf("expected sync-all to stop after first failure; got %d PostXML attempts", len(tly.got))
	}
	if !strings.Contains(stderr.String(), "stopping") {
		t.Errorf("stderr should explain why: %s", stderr.String())
	}
}

func TestRunSyncAll_NotPairedExits2(t *testing.T) {
	deps := syncAllDeps{
		now:             time.Now,
		newOSKeyring:    func() keyring.Store { return keyring.NewMemoryStore() },
		newTallyClient:  func(string) tallyPoster { return &fakeMappingsTallyClient{} },
		newIngestClient: func(_, _, _, _ string) (syncAllIngestClient, error) { return &fakeSyncAllIngest{}, nil },
		loadConfig:      func(string) (*config.Config, error) { return nil, config.ErrNotFound },
	}
	var stdout, stderr bytes.Buffer
	code := runSyncAll(&stdout, &stderr, []string{"--period", "042026"}, deps)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "not paired") {
		t.Errorf("stderr missing 'not paired': %s", stderr.String())
	}
}

func TestRunSyncAll_FetchMappingsErrorIsInfra(t *testing.T) {
	cfgPath := setupPairedConfig(t)
	ks := setupKeyring(t, "conn-1")

	srvIngest := &fakeSyncAllIngest{fetchErr: errors.New("network down")}

	deps := syncAllDeps{
		now:             time.Now,
		newOSKeyring:    func() keyring.Store { return ks },
		newTallyClient:  func(string) tallyPoster { return &fakeMappingsTallyClient{} },
		newIngestClient: func(_, _, _, _ string) (syncAllIngestClient, error) { return srvIngest, nil },
		loadConfig:      config.Load,
	}

	var stdout, stderr bytes.Buffer
	code := runSyncAll(&stdout, &stderr, []string{"--config", cfgPath, "--period", "042026"}, deps)
	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "fetch mappings") {
		t.Errorf("stderr should explain fetch failure: %s", stderr.String())
	}
}
