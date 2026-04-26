package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// discoverCmd scans for Tally instances on the local machine, reports
// what it found, persists the reachable endpoints to the agent config,
// and (by default) posts the catalog to /api/tally/tally-companies/sync
// so the user's mapping UI sees every (port, company) pair.
//
// This is the keystone of A5-2: it converts a raw "5–6 Tally Prime
// processes on different ports" reality into a clean list of
// (endpoint, company) tuples that A5-3's sync-all driver and A5's
// future cron daemon can iterate over without any manual
// configuration on the operator's part.
//
// Exit codes:
//
//	0 — found at least one reachable V3 Tally; save + push succeeded
//	1 — infrastructure failure (keyring, network to GST Reco server)
//	2 — user-fixable: agent not paired, no Tally reachable, bad flags
func discoverCmd(args []string) int {
	return runDiscover(os.Stdout, os.Stderr, args, discoverDeps{
		now:              time.Now,
		newOSKeyring:     defaultSecretStore,
		discover:         tally.Discover,
		newCatalogClient: defaultNewCatalogClient,
		loadConfig:       config.Load,
		saveConfig:       config.Save,
	})
}

// discoverDeps is the test seam. Production passes real implementations;
// tests pass fakes that capture catalog payloads + assert config writes.
type discoverDeps struct {
	now              func() time.Time
	newOSKeyring     func() keyring.Store
	discover         func(ctx context.Context, opts tally.DiscoverOptions) ([]tally.ProbeResult, error)
	newCatalogClient func(baseURL, connectionID, bearer, secretB64 string) (catalogSender, error)
	loadConfig       func(path string) (*config.Config, error)
	saveConfig       func(path string, c *config.Config) error
}

// catalogSender is the narrow surface this command needs from the
// ingest client.
type catalogSender interface {
	SendCatalog(ctx context.Context, body ingest.CatalogRequest) error
}

func defaultNewCatalogClient(baseURL, connectionID, bearer, secretB64 string) (catalogSender, error) {
	return ingest.NewClient(baseURL, connectionID, bearer, secretB64)
}

func runDiscover(stdout, stderr io.Writer, args []string, deps discoverDeps) int {
	fs := flag.NewFlagSet("discover", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "override config file path")
	serverFlag := fs.String("server", "", "override server URL (default: from config)")
	host := fs.String("host", tally.DefaultDiscoverHost, "host to probe")
	portsFlag := fs.String("ports", fmt.Sprintf("%d-%d", tally.DefaultDiscoverPortRange[0], tally.DefaultDiscoverPortRange[1]),
		"port range to scan, e.g. 9000-9009")
	var endpointsFlag stringSlice
	fs.Var(&endpointsFlag, "endpoint", "explicit endpoint URL to probe (repeatable; takes precedence over --ports)")
	timeout := fs.Duration("timeout", tally.DefaultDiscoverTimeout, "per-endpoint probe timeout")
	concurrency := fs.Int("concurrency", tally.DefaultDiscoverConcurrency, "max parallel probes")
	noPush := fs.Bool("no-push", false, "do NOT POST catalog to the server (just probe + persist locally)")
	noSave := fs.Bool("no-save", false, "do NOT update config.tally_endpoints (just probe + push)")
	dryRun := fs.Bool("dry-run", false, "probe and print only — implies --no-push and --no-save")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *dryRun {
		*noPush = true
		*noSave = true
	}

	portRange, err := parsePortRange(*portsFlag)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl discover: %v\n", err)
		return 2
	}

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	cfg, err := deps.loadConfig(cfgPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			fmt.Fprintln(stderr, "agentctl discover: agent is not paired (run `agentctl pair --code <CODE>` first)")
			return 2
		}
		fmt.Fprintf(stderr, "agentctl discover: load config: %v\n", err)
		return 1
	}
	if !cfg.IsPaired() {
		fmt.Fprintln(stderr, "agentctl discover: config exists but is incomplete — re-run `agentctl pair`")
		return 2
	}

	scanCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	results, err := deps.discover(scanCtx, tally.DiscoverOptions{
		Endpoints:   []string(endpointsFlag),
		Host:        *host,
		PortRange:   portRange,
		Timeout:     *timeout,
		Concurrency: *concurrency,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agentctl discover: %v\n", err)
		return 1
	}

	printSummary(stdout, results)

	v3Reachable := filterV3Reachable(results)
	if len(v3Reachable) == 0 {
		fmt.Fprintln(stderr, "\nagentctl discover: no Tally Prime 3.x instances found.")
		fmt.Fprintln(stderr, "Check that Tally is running, that ODBC HTTP server is enabled (F1 → Settings → Connectivity → Client/Server),")
		fmt.Fprintln(stderr, "and that the port is in the scanned range. Use --ports or --endpoint for non-default setups.")
		return 2
	}

	// Save discovered endpoints to config so the daemon can find them
	// without re-running discover. Sorted for deterministic YAML diffs.
	if !*noSave {
		endpoints := make([]string, 0, len(v3Reachable))
		for _, r := range v3Reachable {
			endpoints = append(endpoints, r.Endpoint)
		}
		sort.Strings(endpoints)
		cfg.TallyEndpoints = endpoints
		if err := deps.saveConfig(cfgPath, cfg); err != nil {
			fmt.Fprintf(stderr, "agentctl discover: save config: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "\n✓ saved %d endpoint(s) to %s\n", len(endpoints), cfgPath)
	}

	if *noPush {
		fmt.Fprintln(stdout, "✓ catalog push skipped (--no-push or --dry-run)")
		return 0
	}

	// Build the catalog payload. One CatalogItem per (endpoint,
	// company) pair across all V3-reachable endpoints. Same name at
	// two different endpoints emits two items — server's S14
	// constraint treats them as distinct mapping rows.
	items := buildCatalogItems(v3Reachable)
	if len(items) == 0 {
		fmt.Fprintln(stdout, "✓ no companies to push (Tally instances responded but no company is loaded)")
		return 0
	}

	server := firstNonEmpty(*serverFlag, cfg.Server)
	ks := deps.newOSKeyring()
	hmacKey, bearerKey := keyring.ConnectionKeys(cfg.ConnectionID)
	secret, err := ks.Get(keyring.ServiceName, hmacKey)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl discover: read hmac secret from keyring: %v\n", err)
		return 1
	}
	bearer, err := ks.Get(keyring.ServiceName, bearerKey)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl discover: read bearer token from keyring: %v\n", err)
		return 1
	}

	client, err := deps.newCatalogClient(server, cfg.ConnectionID, bearer, secret)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl discover: build ingest client: %v\n", err)
		return 1
	}

	pushCtx, pushCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pushCancel()
	requestID := fmt.Sprintf("discover-%d", deps.now().Unix())
	if err := client.SendCatalog(pushCtx, ingest.CatalogRequest{
		Items:     items,
		RequestID: requestID,
	}); err != nil {
		fmt.Fprintf(stderr, "agentctl discover: push catalog: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "✓ pushed catalog (%d companies across %d endpoints, request_id=%s)\n",
		len(items), len(v3Reachable), requestID)
	fmt.Fprintf(stdout, "→ next step: open the mapping UI at %s/settings/tally and choose a GSTIN per company.\n", server)
	return 0
}

// printSummary writes a tabular summary of the probe results to w.
// Format is hand-tuned for terminal display; structured machine
// output is left for future --json flag if needed.
func printSummary(w io.Writer, results []tally.ProbeResult) {
	fmt.Fprintf(w, "discover scan: %d endpoint(s)\n\n", len(results))

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ENDPOINT\tVERSION\tCOMPANIES\tLAT\tSTATUS")
	for _, r := range results {
		status := statusFor(r)
		ver := r.VersionStr
		if ver == "" {
			ver = "—"
		}
		coCount := "—"
		if r.IsTally {
			coCount = strconv.Itoa(len(r.Companies))
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%dms\t%s\n",
			r.Endpoint, ver, coCount, r.LatencyMs, status)
	}
	_ = tw.Flush()

	for _, r := range results {
		if !r.IsTally {
			continue
		}
		if len(r.Companies) > 0 {
			fmt.Fprintf(w, "\n%s\n", r.Endpoint)
			for _, c := range r.Companies {
				if c.GUID != "" {
					fmt.Fprintf(w, "  - %s  (guid=%s)\n", c.Name, c.GUID)
				} else {
					fmt.Fprintf(w, "  - %s\n", c.Name)
				}
			}
		}
		for _, warn := range r.Warnings {
			fmt.Fprintf(w, "  ⚠ %s\n", warn)
		}
	}
}

func statusFor(r tally.ProbeResult) string {
	if r.IsTally {
		if r.Version == tally.VersionV4 {
			return "v4 (parser pending — A7)"
		}
		if r.Version == tally.VersionUnknown {
			return "unknown version"
		}
		if len(r.Warnings) > 0 {
			return "ok (warnings)"
		}
		return "ok"
	}
	if r.Reachable {
		return "responded but not Tally"
	}
	if r.Err != nil {
		// Trim noisy network-error wrapping for the summary line.
		msg := r.Err.Error()
		if i := strings.Index(msg, ":"); i > 0 && i < 60 {
			msg = strings.TrimSpace(msg[i+1:])
		}
		if len(msg) > 50 {
			msg = msg[:47] + "..."
		}
		return msg
	}
	return "unknown"
}

func filterV3Reachable(results []tally.ProbeResult) []tally.ProbeResult {
	out := make([]tally.ProbeResult, 0, len(results))
	for _, r := range results {
		if r.IsTally && r.Version == tally.VersionV3 {
			out = append(out, r)
		}
	}
	return out
}

func buildCatalogItems(results []tally.ProbeResult) []ingest.CatalogItem {
	out := make([]ingest.CatalogItem, 0)
	for _, r := range results {
		endpoint := r.Endpoint
		for _, c := range r.Companies {
			item := ingest.CatalogItem{
				TallyCompanyName: c.Name,
				TallyEndpoint:    &endpoint,
			}
			if c.GUID != "" {
				guid := c.GUID
				item.TallyCompanyGUID = &guid
			}
			out = append(out, item)
		}
	}
	return out
}

// parsePortRange accepts "9000-9009" and returns [9000, 9009]. Single
// port "9000" is also accepted as [9000, 9000]. Leading/trailing
// whitespace ignored.
func parsePortRange(s string) ([2]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return tally.DefaultDiscoverPortRange, nil
	}
	parts := strings.SplitN(s, "-", 2)
	startStr := strings.TrimSpace(parts[0])
	start, err := strconv.Atoi(startStr)
	if err != nil {
		return [2]int{}, fmt.Errorf("--ports %q: %v", s, err)
	}
	end := start
	if len(parts) == 2 {
		endStr := strings.TrimSpace(parts[1])
		end, err = strconv.Atoi(endStr)
		if err != nil {
			return [2]int{}, fmt.Errorf("--ports %q: %v", s, err)
		}
	}
	return [2]int{start, end}, nil
}

// stringSlice lets a flag accept multiple --endpoint values.
type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(v string) error {
	*s = append(*s, v)
	return nil
}
