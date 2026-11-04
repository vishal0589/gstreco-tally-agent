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

func TestRun_HonoursCfgTallyEndpoints(t *testing.T) {
	// When cfg has explicit endpoints, Run must pass them through to
	// tally.Discover instead of falling back to the default port-range
	// scan. This is the fix for v0.1.2: customer Tally on
	// non-default port (e.g. 2026) gets picked up via the cfg field
	// without needing to widen the global port range.
	cfg := &config.Config{
		TallyEndpoints: []string{"http://127.0.0.1:2026"},
	}
	gotOpts := tally.DiscoverOptions{} // captured
	_, err := Run(context.Background(), Options{
		Cfg:    cfg,
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, opts tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			gotOpts = opts
			return []tally.ProbeResult{
				{
					Endpoint: "http://127.0.0.1:2026", Reachable: true, IsTally: true, Version: tally.VersionV3,
					Companies: []tally.TallyCompany{{Name: "Co"}},
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	// QA 04-Nov: config endpoints come first, then the default scan.
	want := append([]string{"http://127.0.0.1:2026"}, defaultScanWithout("http://127.0.0.1:2026")...)
	if !equalSlices(gotOpts.Endpoints, want) {
		t.Errorf("DiscoverOptions.Endpoints = %v, want %v", gotOpts.Endpoints, want)
	}
}

func TestRun_EmptyCfgEndpoints_FallsBackToDefaultRange(t *testing.T) {
	// With cfg.TallyEndpoints empty, Run still scans
	// DefaultDiscoverPortRange + alternates (now passed explicitly, so it
	// can be unioned with config and mapped endpoints).
	cfg := &config.Config{}
	var gotOpts tally.DiscoverOptions
	_, err := Run(context.Background(), Options{
		Cfg:    cfg,
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, opts tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			gotOpts = opts
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	if !equalSlices(gotOpts.Endpoints, tally.DefaultScanEndpoints()) {
		t.Errorf("Endpoints = %v, want the default scan %v", gotOpts.Endpoints, tally.DefaultScanEndpoints())
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

func TestRun_AcceptsAllReachableTallyVersions(t *testing.T) {
	// Pre-v0.1.3 this test asserted that V4 endpoints were filtered
	// out. v0.1.3 treats Tally Prime 3.x/4.x/5.x/6.x as protocol-
	// identical for the HTTP/XML queries this agent issues (verified
	// against the production-shipping Manual2AI Python adapter and
	// the Tally Prime 6.0 release notes — no breaking changes to the
	// XML envelope shape). The filter now keeps every IsTally probe
	// result.
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
				{
					Endpoint: "http://127.0.0.1:2026", Reachable: true, IsTally: true, Version: tally.VersionUnknown,
					VersionStr: "Release 6.1",
					Companies:  []tally.TallyCompany{{Name: "PrimeV6"}},
				},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("Run err = %v", err)
	}
	want := []string{"http://127.0.0.1:2026", "http://127.0.0.1:9000", "http://127.0.0.1:9001"}
	if !equalSlices(r.Endpoints, want) {
		t.Errorf("Endpoints = %v, want %v (all reachable Tallys accepted)", r.Endpoints, want)
	}
	if r.CompaniesPushed != 3 {
		t.Errorf("CompaniesPushed = %d, want 3 (3.x + 4.x + 6.x all accepted)", r.CompaniesPushed)
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

func defaultScanWithout(skip ...string) []string {
	drop := map[string]bool{}
	for _, s := range skip {
		drop[s] = true
	}
	var out []string
	for _, e := range tally.DefaultScanEndpoints() {
		if !drop[e] {
			out = append(out, e)
		}
	}
	return out
}

// fakeHost answers as Tally on the ports in up, refused elsewhere, and
// records what each sweep probed.
type fakeHost struct {
	up     map[string][]string // endpoint -> companies
	probed [][]string
}

func (h *fakeHost) discover(_ context.Context, opts tally.DiscoverOptions) ([]tally.ProbeResult, error) {
	h.probed = append(h.probed, append([]string(nil), opts.Endpoints...))
	out := make([]tally.ProbeResult, 0, len(opts.Endpoints))
	for _, ep := range opts.Endpoints {
		cos, ok := h.up[ep]
		if !ok {
			out = append(out, tally.ProbeResult{Endpoint: ep, Err: errors.New("connection refused")})
			continue
		}
		r := tally.ProbeResult{Endpoint: ep, Reachable: true, IsTally: true}
		for _, c := range cos {
			r.Companies = append(r.Companies, tally.TallyCompany{Name: c})
		}
		out = append(out, r)
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func catalogEndpoints(req ingest.CatalogRequest) map[string][]string {
	out := map[string][]string{}
	for _, it := range req.Items {
		out[*it.TallyEndpoint] = append(out[*it.TallyEndpoint], it.TallyCompanyName)
	}
	return out
}

const (
	ep9559 = "http://127.0.0.1:9559"
	ep9553 = "http://127.0.0.1:9553"
	ep3036 = "http://127.0.0.1:3036"
)

// QA 04-Nov: a saved port down for one sweep stays saved, is probed on the
// next sweep, and is catalogued once it answers again.
func TestRun_PortDownOneSweep_ProbedAndCataloguedNext(t *testing.T) {
	cfg := &config.Config{TallyEndpoints: []string{ep9553, ep9559}}
	host := &fakeHost{up: map[string][]string{ep9559: {"Hall of Heritage"}}}
	sender := &recordingSender{}
	var saved [][]string
	opts := Options{
		Cfg: cfg, CfgPath: "cfg.yaml", Sender: sender, Logger: quietLogger(), Now: fixedNow,
		DiscoverFn: host.discover,
		SaveFn: func(_ string, c *config.Config) error {
			saved = append(saved, append([]string(nil), c.TallyEndpoints...))
			return nil
		},
	}

	r, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !equalSlices(cfg.TallyEndpoints, []string{ep9553, ep9559}) || len(saved) != 0 {
		t.Fatalf("after a miss: config = %v, saves = %v; want 9553 kept, nothing rewritten", cfg.TallyEndpoints, saved)
	}
	if !equalSlices(r.EndpointsGone, []string{ep9553}) || r.Reachable != 1 || r.Unreachable != r.Probed-1 {
		t.Fatalf("result = %+v", r)
	}

	host.up[ep9553] = []string{"AAM Exports"}
	if _, err := Run(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if !contains(host.probed[1], ep9553) {
		t.Fatalf("second sweep did not probe 9553: %v", host.probed[1])
	}
	got := catalogEndpoints(sender.seen()[1])
	if len(got[ep9553]) != 1 || len(got[ep9559]) != 1 {
		t.Fatalf("second catalog = %v, want both ports", got)
	}
}

// QA 04-Nov: a mapped endpoint missing from config is probed, catalogued
// and added to config.
func TestRun_MappedEndpointMissingFromConfig_IsProbed(t *testing.T) {
	cfg := &config.Config{TallyEndpoints: []string{ep9559}}
	host := &fakeHost{up: map[string][]string{ep9559: {"HaH"}, ep3036: {"KBS"}}}
	sender := &recordingSender{}
	r, err := Run(context.Background(), Options{
		Cfg: cfg, CfgPath: "cfg.yaml", Sender: sender, Logger: quietLogger(), Now: fixedNow,
		DiscoverFn:      host.discover,
		SaveFn:          func(string, *config.Config) error { return nil },
		MappedEndpoints: func(context.Context) []string { return []string{"http://localhost:3036/", ep9559} },
	})
	if err != nil {
		t.Fatal(err)
	}
	p := host.probed[0]
	if !contains(p, ep3036) {
		t.Fatalf("mapped 3036 not probed: %v", p)
	}
	n := 0
	for _, e := range p {
		if e == ep9559 {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("9559 probed %d times (config + mapping must dedupe)", n)
	}
	if !equalSlices(cfg.TallyEndpoints, []string{ep3036, ep9559}) || !r.ConfigSaved {
		t.Fatalf("config = %v saved=%v", cfg.TallyEndpoints, r.ConfigSaved)
	}
	if got := catalogEndpoints(sender.seen()[0]); len(got) != 2 {
		t.Fatalf("catalog = %v, want companies from both ports", got)
	}
}

// QA 04-Nov: empty config still scans the default range, and finds
// what answers there.
func TestRun_EmptyConfig_ScansDefaultRange(t *testing.T) {
	cfg := &config.Config{}
	host := &fakeHost{up: map[string][]string{"http://127.0.0.1:9000": {"Co"}}}
	if _, err := Run(context.Background(), Options{Cfg: cfg, CfgPath: "c", Logger: quietLogger(), Now: fixedNow,
		DiscoverFn: host.discover, SaveFn: func(string, *config.Config) error { return nil }}); err != nil {
		t.Fatal(err)
	}
	if !equalSlices(host.probed[0], tally.DefaultScanEndpoints()) {
		t.Fatalf("probed = %v", host.probed[0])
	}
	if !equalSlices(cfg.TallyEndpoints, []string{"http://127.0.0.1:9000"}) {
		t.Fatalf("config = %v", cfg.TallyEndpoints)
	}
}

// QA 04-Nov: across sweeps with ports coming and going, the saved list
// never loses an entry, including when nothing answers at all.
func TestRun_SavedListNeverShrinks(t *testing.T) {
	cfg := &config.Config{TallyEndpoints: []string{ep3036, ep9553, ep9559}}
	host := &fakeHost{up: map[string][]string{}}
	opts := Options{Cfg: cfg, CfgPath: "c", Logger: quietLogger(), Now: fixedNow, DiscoverFn: host.discover,
		SaveFn: func(string, *config.Config) error { return nil }}
	for _, up := range []map[string][]string{
		{}, // nothing answers
		{ep9559: {"A"}},
		{"http://127.0.0.1:9001": {"B"}},
		{ep3036: {"C"}},
	} {
		host.up = up
		before := append([]string(nil), cfg.TallyEndpoints...)
		if _, err := Run(context.Background(), opts); err != nil {
			t.Fatal(err)
		}
		for _, e := range before {
			if !contains(cfg.TallyEndpoints, e) {
				t.Fatalf("%s dropped from config after a sweep with %v up: %v", e, up, cfg.TallyEndpoints)
			}
		}
	}
	if !contains(cfg.TallyEndpoints, "http://127.0.0.1:9001") {
		t.Fatalf("found port not added: %v", cfg.TallyEndpoints)
	}
}

func TestSweepEndpoints_UnionDedupe(t *testing.T) {
	got := SweepEndpoints([]string{"http://LOCALHOST:9559/", ""}, []string{"http://127.0.0.1:9559", "127.0.0.1:3037"}, []string{"http://127.0.0.1:9000"})
	want := []string{"http://127.0.0.1:9559", "http://127.0.0.1:3037", "http://127.0.0.1:9000"}
	if !equalSlices(got, want) {
		t.Fatalf("SweepEndpoints = %v, want %v", got, want)
	}
}

func TestSweepSizing_WorstCaseUnderBootBudget(t *testing.T) {
	n := len(tally.DefaultScanEndpoints()) + 7 // shared host: 9553-9559, 3036, 3037 minus overlap
	rounds := (n + SweepConcurrency - 1) / SweepConcurrency
	if worst := time.Duration(rounds) * SweepProbeTimeout; worst > 30*time.Second {
		t.Fatalf("worst-case sweep %v for %d endpoints; keep it well under the 90 s boot budget", worst, n)
	}
}

// QA 04-Nov N1: the catalog carries the GUID the Type:Company probe
// returns; never one it is unsure of.
func TestRun_CatalogCarriesIdentityProbeGUID(t *testing.T) {
	sender := &recordingSender{}
	var asked tally.DiscoverOptions
	_, err := Run(context.Background(), Options{
		Cfg:    &config.Config{},
		Sender: sender,
		Logger: quietLogger(),
		Now:    fixedNow,
		DiscoverFn: func(_ context.Context, opts tally.DiscoverOptions) ([]tally.ProbeResult, error) {
			asked = opts
			return []tally.ProbeResult{{
				Endpoint: "http://127.0.0.1:9558", IsTally: true, Reachable: true,
				Companies: []tally.TallyCompany{{Name: "KBS Exports"}, {Name: "KSS Textiles"}, {Name: "Twin"}},
				Identity: &tally.IdentityProbe{Companies: []tally.TallyCompany{
					{Name: "KBS Exports", GUID: "g-kbs"},
					{Name: "KSS Textiles"},
					{Name: "Twin", GUID: "g-1"}, {Name: "Twin", GUID: "g-2"},
					{Name: "Not In List", GUID: "g-x"},
				}},
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !asked.IdentityProbe {
		t.Fatal("autodiscover did not request the identity probe")
	}
	p := sender.seen()
	if len(p) != 1 || len(p[0].Items) != 3 {
		t.Fatalf("payloads = %+v", p)
	}
	guids := map[string]string{}
	for _, it := range p[0].Items {
		g := "<nil>"
		if it.TallyCompanyGUID != nil {
			g = *it.TallyCompanyGUID
		}
		guids[it.TallyCompanyName] = g
	}
	if guids["KBS Exports"] != "g-kbs" || guids["KSS Textiles"] != "<nil>" || guids["Twin"] != "<nil>" {
		t.Fatalf("catalog guids = %v", guids)
	}
}
