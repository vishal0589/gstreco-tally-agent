// Package autodiscover wraps tally.Discover + config persistence +
// catalog push so the daemon can run a full first-boot discovery
// without the operator ever invoking `agentctl discover` by hand.
//
// The CLI command `agentctl discover` still exists for the diagnostic
// case ("show me what you'd find right now") and for force-rerunning
// discovery from a desk; the daemon path here is what makes the
// install one-line for any customer with one or more Tally Prime
// instances on the default port range.
//
// Lifecycle:
//   - Daemon startup: if cfg.TallyEndpoints is empty, run autodiscover
//     synchronously before the scheduler starts so the first sync
//     tick sees mappings.
//   - Periodic: a timer fires every PeriodicInterval (default 1h) so
//     a customer who later opens a new Tally instance on a different
//     port doesn't have to restart the agent.
//
// All failures are non-fatal — the daemon keeps running with whatever
// state it had before the failed call. Errors and changes are logged
// at info level so the operator's Event Viewer / journalctl shows the
// activity.
package autodiscover

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// DefaultPeriodicInterval is how often the daemon re-runs autodiscover
// after the initial boot probe. 1 hour balances "pick up new Tally
// instances quickly" against "don't hammer the loopback port range".
// Override via Options.PeriodicInterval.
const DefaultPeriodicInterval = 1 * time.Hour

// Sweep sizing (QA 04-Nov). Every sweep probes the saved config endpoints,
// every active mapping's endpoint and the default range + alternates
// (about 30 endpoints on a shared host). A closed loopback port refuses in
// well under a millisecond; only a port that accepts and then hangs costs
// SweepProbeTimeout. Worst case, every endpoint hanging:
// ceil(N / SweepConcurrency) × SweepProbeTimeout — 4 × 5 s = 20 s for 30
// endpoints, inside the daemon's 90 s boot budget and far below the hourly
// interval.
const (
	SweepProbeTimeout = tally.DefaultDiscoverTimeout
	SweepConcurrency  = 8
)

// CatalogSender is the narrow surface autodiscover needs from
// ingest.Client. Defined as an interface so tests can substitute a
// recorder without spinning up an HTTP server.
type CatalogSender interface {
	SendCatalog(ctx context.Context, body ingest.CatalogRequest) error
}

// Options configures one Run() invocation. Zero-valued fields default
// to production defaults (real Discover, real config.Save, real
// time.Now). All fields are read-only — Run does not mutate Options.
type Options struct {
	// Cfg is the loaded agent config. Run reads TallyEndpoints to
	// compute add/remove diffs and writes back the new list when it
	// changes. Caller is responsible for not racing other writers.
	Cfg *config.Config
	// CfgPath is where to persist the updated config. Empty disables
	// persistence (the diff is still computed and pushed, just not
	// saved to disk — useful for tests).
	CfgPath string
	// Sender is the catalog uploader. Nil disables the push step
	// (useful when running just for the diff side-effect).
	Sender CatalogSender
	// Logger is used at info level for "found N endpoints" lines and
	// at warn level for non-fatal failures. Zero value is a discard
	// logger.
	Logger zerolog.Logger

	// MappedEndpoints, when non-nil, returns the Tally endpoint of every
	// active mapping across this host's paired connections. They are
	// probed every sweep even when config does not list them (a company
	// on 9559 whose port fell out of config is still catalogued).
	// Errors are the caller's to log; return what was fetched.
	MappedEndpoints func(context.Context) []string

	// Test seams.
	DiscoverFn func(context.Context, tally.DiscoverOptions) ([]tally.ProbeResult, error)
	SaveFn     func(string, *config.Config) error
	Now        func() time.Time
}

// Result captures what one Run produced. Useful for tests + for the
// caller to log a one-line summary.
type Result struct {
	// Endpoints is the V3-reachable endpoints discovered, sorted.
	Endpoints []string
	// EndpointsAdded are the new endpoints (in Endpoints, not in
	// the previous TallyEndpoints).
	EndpointsAdded []string
	// EndpointsGone are saved endpoints that did not answer this sweep.
	// They are KEPT in config (a miss never removes an endpoint; only
	// `agentctl discover --forget/--replace` does) and probed again on
	// the next sweep.
	EndpointsGone []string
	// Probed, Reachable and Unreachable count this sweep's endpoints:
	// all probed, those that answered as Tally, and the rest.
	Probed, Reachable, Unreachable int
	// CompaniesPushed is the number of (endpoint, company) tuples
	// the catalog push uploaded. 0 when Tally responded but no
	// company is loaded.
	CompaniesPushed int
	// CatalogPushed is true when the SendCatalog call succeeded.
	CatalogPushed bool
	// ConfigSaved is true when cfg was rewritten to disk this run.
	ConfigSaved bool
	// SkippedReason is set when Run returned early without doing
	// useful work (e.g. "no Tally found"). Empty on a successful
	// run.
	SkippedReason string
	// Warnings collects non-fatal observations the caller should
	// surface to the operator.
	Warnings []string
}

// Run performs one discovery sweep + persistence + catalog push.
// Returns a Result regardless of whether the catalog push happened —
// callers can inspect CatalogPushed to know.
//
// Cancelling ctx aborts the sweep gracefully; partial results are
// discarded (we'd rather skip persistence than save a half-discovered
// endpoint set).
func Run(ctx context.Context, opts Options) (Result, error) {
	if opts.Cfg == nil {
		return Result{}, fmt.Errorf("autodiscover: nil config")
	}
	discover := opts.DiscoverFn
	if discover == nil {
		discover = tally.Discover
	}
	save := opts.SaveFn
	if save == nil {
		save = config.Save
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	// QA 04-Nov: the sweep is the union of the saved config endpoints,
	// every active mapping's endpoint and the default range + alternates.
	// Probing only the saved list (as before) meant a port missed once was
	// never probed again, and ports outside 9000-9020 were never found
	// unless configured.
	var mapped []string
	if opts.MappedEndpoints != nil {
		mapped = opts.MappedEndpoints(ctx)
	}
	sweep := SweepEndpoints(opts.Cfg.ResolveTallyEndpoints(""), mapped, tally.DefaultScanEndpoints())
	probes, err := discover(ctx, tally.DiscoverOptions{
		Endpoints:   sweep,
		Timeout:     SweepProbeTimeout,
		Concurrency: SweepConcurrency,
	})
	if err != nil {
		return Result{}, fmt.Errorf("autodiscover: probe sweep: %w", err)
	}
	if ctx.Err() != nil {
		return Result{SkippedReason: "ctx cancelled mid-sweep"}, ctx.Err()
	}

	v3 := filterReachableTally(probes)
	foundEndpoints := make([]string, 0, len(v3))
	for _, p := range v3 {
		foundEndpoints = append(foundEndpoints, p.Endpoint)
	}
	sort.Strings(foundEndpoints)

	previous := append([]string(nil), opts.Cfg.TallyEndpoints...)
	added, gone := diffEndpoints(previous, foundEndpoints)

	result := Result{
		Endpoints:      foundEndpoints,
		EndpointsAdded: added,
		EndpointsGone:  gone,
		Probed:         len(probes),
		Reachable:      len(v3),
		Unreachable:    len(probes) - len(v3),
	}
	opts.Logger.Info().
		Int("probed", result.Probed).
		Int("reachable", result.Reachable).
		Int("unreachable", result.Unreachable).
		Int("mapped_endpoints", len(mapped)).
		Strs("reachable_endpoints", foundEndpoints).
		Strs("saved_not_answering", gone).
		Msg("autodiscover: sweep")

	if len(v3) == 0 {
		result.SkippedReason = "no Tally instance answered on any probed endpoint"
		return result, nil
	}

	// Persist previous ∪ found. A miss never removes an endpoint: a Tally
	// closed during one sweep is probed again next time. Save failure is
	// non-fatal — the daemon can run with the in-memory list.
	next := config.MergeEndpoints(previous, foundEndpoints)
	if !slicesEqual(previous, next) && opts.CfgPath != "" {
		opts.Cfg.TallyEndpoints = next
		if err := save(opts.CfgPath, opts.Cfg); err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("save config: %v", err))
			opts.Logger.Warn().Err(err).Msg("autodiscover: save config failed (non-fatal)")
		} else {
			result.ConfigSaved = true
			opts.Logger.Info().
				Strs("endpoints", next).
				Strs("added", added).
				Msg("autodiscover: persisted endpoint list")
		}
	}

	if opts.Sender != nil {
		items := buildCatalogItems(v3)
		if len(items) == 0 {
			result.Warnings = append(result.Warnings,
				"Tally responded on at least one port but no company is loaded — open a company to map it")
			opts.Logger.Warn().
				Int("endpoints", len(v3)).
				Msg("autodiscover: skipping catalog push (0 companies loaded across all endpoints)")
			return result, nil
		}
		requestID := fmt.Sprintf("autodiscover-%d", now().Unix())
		pushCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := opts.Sender.SendCatalog(pushCtx, ingest.CatalogRequest{
			Items:     items,
			RequestID: requestID,
		}); err != nil {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("push catalog: %v", err))
			opts.Logger.Warn().Err(err).Msg("autodiscover: catalog push failed (will retry next interval)")
			return result, nil
		}
		result.CatalogPushed = true
		result.CompaniesPushed = len(items)
		opts.Logger.Info().
			Int("endpoints", len(v3)).
			Int("companies", len(items)).
			Str("request_id", requestID).
			Msg("autodiscover: pushed catalog")
	}

	return result, nil
}

// Loop ticks Run every interval until ctx is done. Does NOT fire an
// immediate run — caller is expected to have done a synchronous
// Run() at boot before starting the loop, so the scheduler's first
// tick sees mappings without racing the loop's first iteration.
//
// Errors from Run are logged but never stop the loop — autodiscover is
// best-effort and "agent kept running without rediscovery for N hours"
// is preferable to "agent died because Tally was briefly offline".
func Loop(ctx context.Context, interval time.Duration, opts Options) {
	if interval <= 0 {
		interval = DefaultPeriodicInterval
	}
	logger := opts.Logger
	logger.Info().Dur("interval", interval).Msg("autodiscover: refresh loop started")
	defer logger.Info().Msg("autodiscover: refresh loop stopped")

	t := time.NewTicker(interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := Run(ctx, opts); err != nil {
				logger.Warn().Err(err).Msg("autodiscover: periodic run errored (non-fatal)")
			}
		}
	}
}

// filterReachableTally keeps every probe result we identified as a
// real Tally endpoint. Pre-v0.1.3 this was V3-only because we paired
// the version probe (now relaxed; see discover.go) with hard parser
// gating; the actual Tally HTTP/XML protocol is identical across
// Prime 3.x/4.x/5.x/6.x for the queries this agent issues, so
// filtering on a version string the agent can't reliably read just
// dropped real customers' Tallys.
func filterReachableTally(probes []tally.ProbeResult) []tally.ProbeResult {
	out := make([]tally.ProbeResult, 0, len(probes))
	for _, p := range probes {
		if p.IsTally {
			out = append(out, p)
		}
	}
	return out
}

// SweepEndpoints is the union of the given endpoint lists, deduplicated by
// tally.CanonicalEndpoint, in first-seen order (config first, then mapped,
// then the default scan). Blank entries are dropped.
func SweepEndpoints(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, ep := range list {
			key := tally.CanonicalEndpoint(ep)
			if key == "" || seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// diffEndpoints returns sorted (added, gone). Nil slices when no diff
// — caller can check len() rather than nil-vs-empty.
func diffEndpoints(prev, next []string) (added, gone []string) {
	prevSet := make(map[string]struct{}, len(prev))
	for _, e := range prev {
		prevSet[tally.CanonicalEndpoint(e)] = struct{}{}
	}
	nextSet := make(map[string]struct{}, len(next))
	for _, e := range next {
		nextSet[tally.CanonicalEndpoint(e)] = struct{}{}
	}
	for _, e := range next {
		if _, ok := prevSet[tally.CanonicalEndpoint(e)]; !ok {
			added = append(added, e)
		}
	}
	for _, e := range prev {
		if _, ok := nextSet[tally.CanonicalEndpoint(e)]; !ok {
			gone = append(gone, e)
		}
	}
	sort.Strings(added)
	sort.Strings(gone)
	return added, gone
}

func slicesEqual(a, b []string) bool {
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
