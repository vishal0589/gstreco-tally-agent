package tally

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// DefaultDiscoverPortRange is the inclusive [start, end] port range
// `agentctl discover` scans by default. 9000 is Tally Prime's
// default; widening to 9020 catches a typical "10+ instances side by
// side" install pattern (each Tally Prime company needs its own port
// when run as a service). Operators can widen further via
// DiscoverOptions.PortRange or pass explicit endpoints for unusual
// setups (DefaultDiscoverAlternates covers the common non-9000-range
// defaults — e.g. 2026 — that we've seen in pilot installs).
var DefaultDiscoverPortRange = [2]int{9000, 9020}

// DefaultDiscoverAlternates are well-known Tally ports outside the
// 9000-range default. Probed in addition to the port range when the
// caller doesn't supply explicit endpoints. Add to this list as
// pilots surface new "house default" ports.
//   - 2026: PLLUM pilot (Tally configured pre-Prime, port carried over)
//   - 8989: legacy Tally 9 (some shops still run it for archival)
var DefaultDiscoverAlternates = []int{2026, 8989}

// DefaultDiscoverHost is the loopback-only default. Tally's HTTP
// server binds to all interfaces by default but we only ever speak
// to it over loopback — there is no scenario where the agent should
// be probing remote machines for Tally. Override via
// DiscoverOptions.Host for a hosted-Tally setup behind a fixed
// internal hostname.
const DefaultDiscoverHost = "127.0.0.1"

// DefaultDiscoverTimeout caps each per-endpoint probe (version +
// company-list combined). Tally on a warm instance answers in
// 50–200 ms over loopback; 5 s gives generous headroom for cold
// process start without strangling the discovery sweep when 8 of
// 10 ports are closed.
const DefaultDiscoverTimeout = 5 * time.Second

// DefaultDiscoverConcurrency limits how many endpoints are probed
// in parallel. Each probe is a separate Tally process when the
// port hits, so concurrency doesn't queue at Tally itself; the
// limit is mostly for avoiding socket-table churn during a wide
// scan. 4 keeps a 10-port sweep responsive without bursting.
const DefaultDiscoverConcurrency = 4

// DiscoverOptions configures a Discover() call. Zero value uses
// loopback ports 9000–9009 with 5-second per-port timeout and 4
// parallel workers — sane for the 99% case.
type DiscoverOptions struct {
	// Endpoints, when non-empty, takes priority over Host+PortRange.
	// Use this for hosted-Tally setups where the agent's config
	// already records explicit URLs.
	Endpoints []string
	// Host is the hostname or IP to probe. Empty = loopback.
	Host string
	// PortRange is [start, end] inclusive. Zero value = 9000–9009.
	PortRange [2]int
	// Timeout caps each per-endpoint probe. Zero = 5 seconds.
	Timeout time.Duration
	// Concurrency limits parallel probes. Zero = 4. Set to 1 for
	// strictly sequential probing (useful in tests + tracing).
	Concurrency int
	// NewClient injects a *Client constructor for tests. Production
	// passes nil and gets the default tally.NewClient.
	NewClient func(endpoint string) *Client
}

// ProbeResult is what Discover returns for one endpoint. Either the
// probe succeeded (Reachable=true with version + companies) or it
// failed (Err set + IsTally hint when we can tell). Always Endpoint-
// scoped — caller correlates by URL.
type ProbeResult struct {
	// Endpoint is the URL that was probed (with scheme + host + port).
	Endpoint string
	// Reachable is true when the probe got a Tally-shaped response on
	// at least one of the version / company-list calls.
	Reachable bool
	// IsTally is true when the version probe returned a string we
	// recognised as Tally (contains "release" + a major version).
	// False positives possible: an HTTP service that happens to echo
	// "Release 4.0" would trip this. The agent only acts on results
	// where IsTally=true AND CompanyList parsed cleanly.
	IsTally bool
	// Version is V3 / V4 / Unknown. V4 endpoints are still recorded
	// but the daemon will refuse to fetch from them until A7
	// (parser_v4) ships — surfaced via the Warnings field.
	Version    Version
	VersionStr string
	// Companies is the list of currently-loaded company files at this
	// endpoint. Empty when Tally is running but no company is loaded
	// (cold start) — still a valid result, just nothing to map yet.
	Companies []TallyCompany
	// Warnings are non-fatal observations the operator should know
	// about ("V4 not yet supported", "0 companies loaded", etc.).
	Warnings []string
	// Err is populated when the probe couldn't talk to the endpoint
	// at all (TCP refused, timeout, non-Tally response). Reachable
	// is false when Err is set.
	Err error
	// LatencyMs is the wall-clock time the probe spent. Useful for
	// diagnostics ("Tally answered in 30 ms but the discover sweep
	// took 5 s — something's blocking").
	LatencyMs int64
}

// Discover scans a set of endpoints and reports which ones are
// running Tally. Sequential vs parallel is controlled by
// opts.Concurrency. Returns one ProbeResult per endpoint in the
// same order the endpoints were probed (sorted ascending for the
// port-range case; preserved for the explicit-endpoints case).
//
// ctx applies to the WHOLE sweep; per-probe timeout is
// opts.Timeout. Cancel ctx to abort the rest of the sweep early —
// already-completed results stay in the slice.
func Discover(ctx context.Context, opts DiscoverOptions) ([]ProbeResult, error) {
	endpoints, err := resolveEndpoints(opts)
	if err != nil {
		return nil, err
	}
	if len(endpoints) == 0 {
		return nil, errors.New("tally: no endpoints to probe (port range empty + Endpoints empty)")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultDiscoverTimeout
	}
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultDiscoverConcurrency
	}
	if concurrency > len(endpoints) {
		concurrency = len(endpoints)
	}
	newClient := opts.NewClient
	if newClient == nil {
		newClient = func(endpoint string) *Client {
			// Per-probe timeout caps the whole probe (version +
			// company-list); the per-endpoint context derived below
			// enforces it.
			return NewClient(endpoint)
		}
	}

	results := make([]ProbeResult, len(endpoints))
	work := make(chan int)
	var wg sync.WaitGroup

	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range work {
				probeCtx, cancel := context.WithTimeout(ctx, timeout)
				results[idx] = probeOne(probeCtx, endpoints[idx], newClient(endpoints[idx]))
				cancel()
			}
		}()
	}

	for i := range endpoints {
		select {
		case <-ctx.Done():
			// Stop dispatching new work; let in-flight probes complete
			// or hit their own ctx.Done() shortly.
			close(work)
			wg.Wait()
			// Mark unprobed slots so the caller sees what was skipped.
			for j := i; j < len(endpoints); j++ {
				if results[j].Endpoint == "" {
					results[j] = ProbeResult{
						Endpoint: endpoints[j],
						Err:      ctx.Err(),
					}
				}
			}
			return results, nil
		case work <- i:
		}
	}
	close(work)
	wg.Wait()
	return results, nil
}

// probeOne does the actual two-call probe (version, then company
// list) for a single endpoint. Built to never panic — every error
// path returns a structured ProbeResult.
func probeOne(ctx context.Context, endpoint string, client *Client) ProbeResult {
	start := time.Now()
	r := ProbeResult{Endpoint: endpoint}

	defer func() {
		r.LatencyMs = time.Since(start).Milliseconds()
	}()

	versionEnv, err := BuildVersionXML()
	if err != nil {
		r.Err = fmt.Errorf("build version envelope: %w", err)
		return r
	}
	versionResp, err := client.PostXML(ctx, versionEnv)
	if err != nil {
		r.Err = fmt.Errorf("version probe: %w", err)
		return r
	}
	v, vstr, vErr := ParseVersion(versionResp)
	r.Version = v
	r.VersionStr = vstr
	if vErr != nil {
		// Reachable but the body wasn't Tally-shaped (e.g. someone
		// runs Jenkins on this port). Surface it but don't error the
		// whole sweep — discover keeps moving.
		r.Reachable = true
		r.IsTally = false
		r.Err = fmt.Errorf("version response is not Tally-shaped: %w", vErr)
		return r
	}
	r.Reachable = true
	r.IsTally = true
	if v == VersionV4 {
		r.Warnings = append(r.Warnings,
			"Tally Prime 4.x detected — agent's parser_v3 cannot fetch from this endpoint until A7 ships.")
	} else if v == VersionUnknown {
		r.Warnings = append(r.Warnings,
			fmt.Sprintf("unrecognised Tally version %q — proceeding with parser_v3 (compatibility not guaranteed)", vstr))
	}

	companyEnv, err := BuildCompanyListXML(CompanyListRequest{})
	if err != nil {
		// Should be impossible — the template has no parameters — but
		// surface the error rather than panic.
		r.Err = fmt.Errorf("build company-list envelope: %w", err)
		return r
	}
	companyResp, err := client.PostXML(ctx, companyEnv)
	if err != nil {
		r.Err = fmt.Errorf("company-list probe: %w", err)
		return r
	}
	companies, cErr := ParseCompanyListV3(companyResp)
	if cErr != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("company-list parse: %v", cErr))
		return r
	}
	r.Companies = companies.Companies
	for _, w := range companies.Warnings {
		r.Warnings = append(r.Warnings, "company-list: "+w)
	}
	if len(r.Companies) == 0 {
		r.Warnings = append(r.Warnings,
			"Tally responded but no company is loaded — open a company in Tally and re-run discover.")
	}
	return r
}

// resolveEndpoints merges DiscoverOptions into a sorted, deduped
// slice of endpoint URLs. Explicit Endpoints take priority over the
// port-range scan; mixing is intentional out-of-scope (one or the
// other, never both — if you have explicit endpoints you don't
// need a sweep).
func resolveEndpoints(opts DiscoverOptions) ([]string, error) {
	if len(opts.Endpoints) > 0 {
		out := make([]string, 0, len(opts.Endpoints))
		seen := map[string]struct{}{}
		for _, e := range opts.Endpoints {
			e = strings.TrimSpace(e)
			if e == "" {
				continue
			}
			if _, dup := seen[e]; dup {
				continue
			}
			seen[e] = struct{}{}
			out = append(out, e)
		}
		return out, nil
	}
	host := strings.TrimSpace(opts.Host)
	if host == "" {
		host = DefaultDiscoverHost
	}
	startPort, endPort := opts.PortRange[0], opts.PortRange[1]
	if startPort == 0 && endPort == 0 {
		startPort, endPort = DefaultDiscoverPortRange[0], DefaultDiscoverPortRange[1]
	}
	if startPort < 1 || startPort > 65535 || endPort < 1 || endPort > 65535 {
		return nil, fmt.Errorf("tally: port range %d-%d out of valid 1..65535 range", startPort, endPort)
	}
	if endPort < startPort {
		return nil, fmt.Errorf("tally: PortRange end (%d) before start (%d)", endPort, startPort)
	}
	out := make([]string, 0, endPort-startPort+1+len(DefaultDiscoverAlternates))
	seen := make(map[int]struct{}, endPort-startPort+1+len(DefaultDiscoverAlternates))
	for p := startPort; p <= endPort; p++ {
		out = append(out, fmt.Sprintf("http://%s:%d", host, p))
		seen[p] = struct{}{}
	}
	// Append alternates (only when we're using the default port
	// range — a caller who passes an explicit narrow range like
	// 5000-5010 doesn't want 2026 grafted in). The dedup map
	// protects against future cases where an alternate falls inside
	// a widened DefaultDiscoverPortRange.
	usingDefaults := startPort == DefaultDiscoverPortRange[0] && endPort == DefaultDiscoverPortRange[1]
	if usingDefaults {
		for _, p := range DefaultDiscoverAlternates {
			if _, dup := seen[p]; dup {
				continue
			}
			out = append(out, fmt.Sprintf("http://%s:%d", host, p))
		}
	}
	return out, nil
}
