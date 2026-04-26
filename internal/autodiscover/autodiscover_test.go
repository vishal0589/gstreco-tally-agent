package autodiscover

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// recordingSender captures every catalog payload so tests can assert
// shape without spinning up an HTTP server.
type recordingSender struct {
	mu       sync.Mutex
	payloads []ingest.CatalogRequest
	err      error
}

func (r *recordingSender) SendCatalog(_ context.Context, body ingest.CatalogRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloads = append(r.payloads, body)
	return r.err
}

func (r *recordingSender) seen() []ingest.CatalogRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ingest.CatalogRequest, len(r.payloads))
	copy(out, r.payloads)
	return out
}

func quietLogger() zerolog.Logger {
	return zerolog.New(io.Discard)
}

func fixedNow() time.Time {
	return time.Date(2026, 4, 26, 12, 0, 0, 0, time.UTC)
}

func TestRun_NoTallyFound_SkippedWithReason(t *testing.T) {
	cfg := &config.Config{}
	sender := &recordingSender{}
	r, err := Run(context.Background(), Options{
		Cfg:    cfg,
		Sender: sender,
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return []tally.ProbeResult{
				{Endpoint: "http://127.0.0.1:9000", Reachable: false, Err: errors.New("tcp refused")},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if r.SkippedReason == "" {
		t.Errorf("expected SkippedReason, got empty")
	}
	if r.CatalogPushed {
		t.Errorf("did not expect catalog push")
	}
	if len(sender.seen()) != 0 {
		t.Errorf("sender called %d times, expected 0", len(sender.seen()))
	}
}

func TestRun_TallyButNoCompanies_NoPushEmitsWarning(t *testing.T) {
	cfg := &config.Config{}
	sender := &recordingSender{}
	r, err := Run(context.Background(), Options{
		Cfg:    cfg,
		Sender: sender,
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return []tally.ProbeResult{
				{
					Endpoint:  "http://127.0.0.1:9000",
					Reachable: true,
					IsTally:   true,
					Version:   tally.VersionV3,
					Companies: nil,
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if r.CatalogPushed {
		t.Errorf("expected no push when 0 companies")
	}
	if len(r.Warnings) == 0 {
		t.Errorf("expected warning about empty companies, got none")
	}
	if len(sender.seen()) != 0 {
		t.Errorf("sender called when it shouldn't have been")
	}
}

func TestRun_PushesCatalog_AndPersistsConfig(t *testing.T) {
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfg := &config.Config{
		Server:       "https://gstreco.example",
		ConnectionID: "conn-1",
	}
	sender := &recordingSender{}
	r, err := Run(context.Background(), Options{
		Cfg:     cfg,
		CfgPath: cfgPath,
		Sender:  sender,
		Logger:  quietLogger(),
		Now:     fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return []tally.ProbeResult{
				{
					Endpoint:  "http://127.0.0.1:9001",
					Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "PLLUM CASA", GUID: "g-CASA"}},
				},
				{
					Endpoint:  "http://127.0.0.1:9002",
					Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "PLLUM LEGNO"}},
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if !r.CatalogPushed {
		t.Errorf("expected catalog pushed")
	}
	if r.CompaniesPushed != 2 {
		t.Errorf("CompaniesPushed = %d, want 2", r.CompaniesPushed)
	}
	if !r.ConfigSaved {
		t.Errorf("expected ConfigSaved = true")
	}

	wantEndpoints := []string{"http://127.0.0.1:9001", "http://127.0.0.1:9002"}
	if !equalSlices(r.Endpoints, wantEndpoints) {
		t.Errorf("Endpoints = %v, want %v", r.Endpoints, wantEndpoints)
	}
	if !equalSlices(r.EndpointsAdded, wantEndpoints) {
		t.Errorf("EndpointsAdded = %v, want %v", r.EndpointsAdded, wantEndpoints)
	}

	// Catalog payload shape — one item per (endpoint, company).
	got := sender.seen()
	if len(got) != 1 {
		t.Fatalf("sender called %d times, want 1", len(got))
	}
	if len(got[0].Items) != 2 {
		t.Fatalf("payload has %d items, want 2", len(got[0].Items))
	}
	if got[0].Items[0].TallyEndpoint == nil || *got[0].Items[0].TallyEndpoint != "http://127.0.0.1:9001" {
		t.Errorf("Items[0].TallyEndpoint = %v", got[0].Items[0].TallyEndpoint)
	}
	if got[0].RequestID == "" {
		t.Errorf("RequestID is empty — should be autodiscover-<unix>")
	}

	// Config persisted to disk.
	loaded, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load err = %v", err)
	}
	if !equalSlices(loaded.TallyEndpoints, wantEndpoints) {
		t.Errorf("persisted endpoints = %v, want %v", loaded.TallyEndpoints, wantEndpoints)
	}
}

func TestRun_NoChange_DoesNotResaveConfig(t *testing.T) {
	cfg := &config.Config{
		TallyEndpoints: []string{"http://127.0.0.1:9001"},
	}
	saveCalls := 0
	r, err := Run(context.Background(), Options{
		Cfg:     cfg,
		CfgPath: "/tmp/should-not-be-written.yaml",
		Sender:  &recordingSender{},
		Logger:  quietLogger(),
		Now:     fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return []tally.ProbeResult{
				{
					Endpoint: "http://127.0.0.1:9001", Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "PLLUM CASA"}},
				},
			}, nil
		},
		SaveFn: func(_ string, _ *config.Config) error {
			saveCalls++
			return nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if r.ConfigSaved {
		t.Errorf("expected ConfigSaved = false (endpoint set unchanged)")
	}
	if saveCalls != 0 {
		t.Errorf("save called %d times when nothing changed", saveCalls)
	}
}

func TestRun_SenderErr_NonFatalWithWarning(t *testing.T) {
	sender := &recordingSender{err: errors.New("network down")}
	r, err := Run(context.Background(), Options{
		Cfg:    &config.Config{},
		Sender: sender,
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return []tally.ProbeResult{
				{
					Endpoint: "http://127.0.0.1:9000", Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "Co"}},
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v (sender errors should not bubble)", err)
	}
	if r.CatalogPushed {
		t.Errorf("expected CatalogPushed = false on sender err")
	}
	if len(r.Warnings) == 0 {
		t.Errorf("expected warning when sender failed")
	}
}

func TestRun_SaveErr_NonFatalAndStillPushes(t *testing.T) {
	saveErr := errors.New("disk full")
	sender := &recordingSender{}
	r, err := Run(context.Background(), Options{
		Cfg:     &config.Config{},
		CfgPath: "/tmp/whatever.yaml",
		Sender:  sender,
		Logger:  quietLogger(),
		Now:     fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return []tally.ProbeResult{
				{
					Endpoint: "http://127.0.0.1:9000", Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "Co"}},
				},
			}, nil
		},
		SaveFn: func(_ string, _ *config.Config) error { return saveErr },
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if r.ConfigSaved {
		t.Errorf("expected ConfigSaved = false on save err")
	}
	if !r.CatalogPushed {
		t.Errorf("expected CatalogPushed = true even when save failed")
	}
	if len(r.Warnings) == 0 {
		t.Errorf("expected warning when save failed")
	}
}

func TestRun_DiscoverErr_Bubbles(t *testing.T) {
	probeErr := errors.New("scan exploded")
	_, err := Run(context.Background(), Options{
		Cfg:    &config.Config{},
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return nil, probeErr
		},
	})
	if err == nil {
		t.Fatalf("expected error when discover errors")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("expected wrapped probeErr, got %v", err)
	}
}

func TestRun_NilCfg_Errors(t *testing.T) {
	_, err := Run(context.Background(), Options{Logger: quietLogger()})
	if err == nil {
		t.Fatal("expected error with nil Cfg")
	}
}

func TestRun_DiffEndpoints_AddedAndGone(t *testing.T) {
	cfg := &config.Config{
		TallyEndpoints: []string{"http://127.0.0.1:9000", "http://127.0.0.1:9001"},
	}
	r, err := Run(context.Background(), Options{
		Cfg:    cfg,
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return []tally.ProbeResult{
				{
					Endpoint: "http://127.0.0.1:9001", Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "Co1"}},
				},
				{
					Endpoint: "http://127.0.0.1:9002", Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "Co2"}},
				},
			}, nil
		},
		SaveFn: func(_ string, _ *config.Config) error { return nil },
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if !equalSlices(r.EndpointsAdded, []string{"http://127.0.0.1:9002"}) {
		t.Errorf("EndpointsAdded = %v", r.EndpointsAdded)
	}
	if !equalSlices(r.EndpointsGone, []string{"http://127.0.0.1:9000"}) {
		t.Errorf("EndpointsGone = %v", r.EndpointsGone)
	}
}

func TestRun_FiltersOutV4Endpoints(t *testing.T) {
	sender := &recordingSender{}
	r, err := Run(context.Background(), Options{
		Cfg:    &config.Config{},
		Sender: sender,
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			return []tally.ProbeResult{
				{
					Endpoint: "http://127.0.0.1:9000", Reachable: true, IsTally: true, Version: tally.VersionV4,
					Companies: []tally.TallyCompany{{Name: "PrimeV4"}},
				},
				{
					Endpoint: "http://127.0.0.1:9001", Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "PrimeV3"}},
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if !equalSlices(r.Endpoints, []string{"http://127.0.0.1:9001"}) {
		t.Errorf("expected only V3 endpoint, got %v", r.Endpoints)
	}
	if r.CompaniesPushed != 1 {
		t.Errorf("CompaniesPushed = %d, want 1 (V4 filtered)", r.CompaniesPushed)
	}
}

func TestLoop_TicksAndStopsOnCtx(t *testing.T) {
	calls := atomic.Int32{}
	cfg := &config.Config{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		Loop(ctx, 50*time.Millisecond, Options{
			Cfg:    cfg,
			Logger: quietLogger(),
			Now:    fixedNow,
			DiscoverFn: func(_ context.Context, _ tally.DiscoverOptions) ([]tally.ProbeResult, error) {
				calls.Add(1)
				return nil, nil
			},
		})
		close(done)
	}()

	// Loop should NOT fire immediately — first tick lands at 50ms.
	// Wait long enough to see at least 2 ticks.
	time.Sleep(170 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Loop did not exit on ctx cancel within 2s")
	}

	got := calls.Load()
	if got < 2 {
		t.Errorf("expected >=2 ticks within 170ms (interval=50ms), got %d", got)
	}
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
