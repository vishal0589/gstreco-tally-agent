package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/syncrun"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// syncAllCmd walks every active mapping the server reports for this
// connection and runs the syncrun pipeline per (endpoint, company,
// kind) tuple. This is the operational driver for the remote-server
// pilot — the operator runs it once per period and gets every Tally
// instance pushed in one command.
//
// Pairs with server S14 (tally_endpoint on mappings) and S15 (the
// /mappings/active endpoint this command GETs). The full A5 cron
// daemon will be a third caller of the same syncrun.RunOne pipeline
// — sync-all is the manual rehearsal that proves the loop before
// the daemon ships.
//
// Exit codes:
//
//	0 — every mapping × kind succeeded (or zero rows in window)
//	1 — infrastructure failure: keyring, server unreachable, every
//	    mapping had at least one fatal error
//	2 — user-fixable: not paired, no active mappings, bad flags
func syncAllCmd(args []string) int {
	return runSyncAll(os.Stdout, os.Stderr, args, syncAllDeps{
		now:          time.Now,
		newOSKeyring: defaultSecretStore,
		newTallyClient: func(endpoint string) tallyPoster {
			return tally.NewClient(endpoint)
		},
		newIngestClient: defaultNewSyncAllIngestClient,
		loadConfig:      config.Load,
	})
}

type syncAllDeps struct {
	now             func() time.Time
	newOSKeyring    func() keyring.Store
	newTallyClient  func(endpoint string) tallyPoster
	newIngestClient func(baseURL, connectionID, bearer, secretB64 string) (syncAllIngestClient, error)
	loadConfig      func(path string) (*config.Config, error)
}

// syncAllIngestClient is the union of operations sync-all needs:
// fetch the active mapping list AND send ingest batches. The two are
// always handled by the same authenticated client — splitting them
// would force tests to construct two fakes when one suffices.
type syncAllIngestClient interface {
	syncrun.IngestSender
	FetchActiveMappings(ctx context.Context) (*ingest.ActiveMappingsResponse, error)
}

func defaultNewSyncAllIngestClient(baseURL, connectionID, bearer, secretB64 string) (syncAllIngestClient, error) {
	return ingest.NewClient(baseURL, connectionID, bearer, secretB64)
}

func runSyncAll(stdout, stderr io.Writer, args []string, deps syncAllDeps) int {
	fs := flag.NewFlagSet("sync-all", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "override config file path")
	serverFlag := fs.String("server", "", "override server URL (default: from config)")
	period := fs.String("period", "", "month shorthand MMYYYY (e.g. 042026); mutually exclusive with --from/--to")
	fromFlag := fs.String("from", "", "window start YYYY-MM-DD (use with --to instead of --period)")
	toFlag := fs.String("to", "", "window end YYYY-MM-DD inclusive")
	kindsFlag := fs.String("kinds", "purchase,sales,credit_note,debit_note",
		"comma-separated voucher kinds to fetch per mapping")
	batchSize := fs.Int("batch-size", ingest.DefaultBatchSize, "max rows per ingest request")
	dryRun := fs.Bool("dry-run", false, "fetch + parse only; do not POST ingest")
	continueOnError := fs.Bool("continue-on-error", true,
		"keep walking the remaining mappings after a fatal error in one")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	from, to, err := resolveWindow(*period, *fromFlag, *toFlag)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync-all: %v\n", err)
		return 2
	}

	kinds, err := parseKinds(*kindsFlag)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync-all: %v\n", err)
		return 2
	}

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	cfg, err := deps.loadConfig(cfgPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			fmt.Fprintln(stderr, "agentctl sync-all: agent is not paired (run `agentctl pair --code <CODE>` first)")
			return 2
		}
		fmt.Fprintf(stderr, "agentctl sync-all: load config: %v\n", err)
		return 1
	}
	if !cfg.IsPaired() {
		fmt.Fprintln(stderr, "agentctl sync-all: config exists but is incomplete — re-run `agentctl pair`")
		return 2
	}

	server := firstNonEmpty(*serverFlag, cfg.Server)

	// Build the ingest client even in --dry-run because we still
	// need FetchActiveMappings to know what to fetch from Tally. Only
	// the per-batch Send is suppressed in dry-run mode.
	ks := deps.newOSKeyring()
	hmacKey, bearerKey := keyring.ConnectionKeys(cfg.ConnectionID)
	secret, err := ks.Get(keyring.ServiceName, hmacKey)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync-all: read hmac secret from keyring: %v\n", err)
		return 1
	}
	bearer, err := ks.Get(keyring.ServiceName, bearerKey)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync-all: read bearer token from keyring: %v\n", err)
		return 1
	}

	client, err := deps.newIngestClient(server, cfg.ConnectionID, bearer, secret)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync-all: build ingest client: %v\n", err)
		return 1
	}

	fetchCtx, cancelFetch := context.WithTimeout(context.Background(), 30*time.Second)
	mappings, err := client.FetchActiveMappings(fetchCtx)
	cancelFetch()
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync-all: fetch mappings: %v\n", err)
		return 1
	}
	if len(mappings.Mappings) == 0 {
		fmt.Fprintln(stderr, "agentctl sync-all: no active mappings.")
		fmt.Fprintln(stderr, "Run `agentctl discover` to enumerate Tally instances, then map each company to a GSTIN at")
		fmt.Fprintf(stderr, "%s/settings/tally before running sync-all.\n", server)
		return 2
	}

	fmt.Fprintf(stdout, "→ %d active mapping(s) for connection %s (fetched %s)\n",
		len(mappings.Mappings), mappings.ConnectionID, mappings.FetchedAt)
	fmt.Fprintf(stdout, "  window: %s..%s · kinds: %s · dry-run: %t\n\n",
		from.Format("2006-01-02"), to.Format("2006-01-02"),
		strings.Join(kindNamesFromResolved(kinds), ","), *dryRun)

	var sender syncrun.IngestSender = client
	if *dryRun {
		sender = noopSender{}
	}

	runIDPrefix := fmt.Sprintf("syncall-%d", deps.now().Unix())
	res := syncrun.WalkAll(context.Background(), syncrun.WalkOptions{
		Mappings:        mappings.Mappings,
		Kinds:           kindsResolvedToWalk(kinds),
		From:            from,
		To:              to,
		BatchSize:       *batchSize,
		RunIDPrefix:     runIDPrefix,
		RunKind:         "manual",
		PerRunTimeout:   10 * time.Minute,
		ContinueOnError: *continueOnError,
		NewTallyClient: func(endpoint string) syncrun.TallyPoster {
			return deps.newTallyClient(endpoint)
		},
		Sender: sender,
		PerMappingHook: func(idx, total int, m ingest.ActiveMapping) {
			fmt.Fprintf(stdout, "[%d/%d] %s @ %s\n",
				idx+1, total, m.TallyCompanyName, m.TallyEndpoint)
		},
		PerKindResultHook: func(m ingest.ActiveMapping, k syncrun.KindPair, r syncrun.Result, fatalErr error) {
			if fatalErr != nil {
				fmt.Fprintf(stderr, "  ✗ %s: %v\n", k.Name, fatalErr)
				return
			}
			fmt.Fprintf(stdout, "    %s: rows=%d sent=%d failed=%d\n",
				k.Name, r.RowCount, r.BatchesSent, r.BatchesFailed)
			for _, w := range r.ParseWarnings {
				fmt.Fprintf(stdout, "      ⚠ %s\n", w)
			}
		},
		PerRunProgress: func(_ ingest.ActiveMapping, k syncrun.KindPair) syncrun.Progress {
			return makeSyncAllProgress(stdout, stderr, k.Name)
		},
	})

	if res.StoppedEarly {
		fmt.Fprintln(stderr, "stopping (--continue-on-error=false)")
		return 1
	}

	fmt.Fprintf(stdout,
		"\nsummary: %d/%d mapping(s) ran · rows=%d · batches sent=%d failed=%d · fatal errors=%d\n",
		res.MappingsRun, len(mappings.Mappings), res.TotalRows,
		res.TotalBatchesSent, res.TotalBatchesFailed, res.FatalErrors)

	if res.FatalErrors > 0 || res.TotalBatchesFailed > 0 {
		return 1
	}
	if *dryRun {
		fmt.Fprintln(stdout, "✓ dry-run sync-all complete (no rows sent)")
	} else {
		fmt.Fprintln(stdout, "✓ sync-all complete")
	}
	return 0
}

// kindsResolvedToWalk maps the CLI's local kindResolved struct onto
// syncrun.KindPair so WalkAll consumes a single canonical type. Kept
// as a small adapter rather than collapsing the two — the CLI's
// kindResolved still drives flag parsing here, while syncrun.KindPair
// is the package-public contract for downstream callers (daemon, A5
// scheduler).
func kindsResolvedToWalk(kinds []kindResolved) []syncrun.KindPair {
	out := make([]syncrun.KindPair, len(kinds))
	for i, k := range kinds {
		out[i] = syncrun.KindPair{Name: k.name, Tally: k.tally, Ingest: k.ingest}
	}
	return out
}

// kindNamesFromResolved returns just the names — used for the
// "kinds: purchase,sales,..." header.
func kindNamesFromResolved(kinds []kindResolved) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = k.name
	}
	return out
}

// kindResolved is a kind string + its tally + ingest enums. Resolved
// once at flag-parse time so the per-mapping inner loop doesn't
// repeat the mapKind lookup per iteration.
type kindResolved struct {
	name   string
	tally  tally.VoucherKind
	ingest tally.IngestKind
}

// parseKinds expands the comma-separated --kinds flag into resolved
// pairs. Order is preserved so operators can run kinds in the order
// they specified ("--kinds sales,purchase" → sales first).
func parseKinds(s string) ([]kindResolved, error) {
	out := make([]kindResolved, 0, 4)
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(s, ",") {
		k := strings.TrimSpace(raw)
		if k == "" {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		tk, ik, err := mapKind(k)
		if err != nil {
			return nil, err
		}
		out = append(out, kindResolved{name: k, tally: tk, ingest: ik})
	}
	if len(out) == 0 {
		return nil, errors.New("--kinds is empty after parsing (use a comma-separated list of: purchase, sales, credit_note, debit_note)")
	}
	return out, nil
}

// makeSyncAllProgress returns a progress callback that prefixes its
// output with the kind name. Stage names match the syncrun events;
// "fetching" / "parsed" are quiet (they fire 4× per mapping for the 4
// kinds — too noisy), the per-batch lines stay because each batch is
// real work the operator wants to see streaming through.
func makeSyncAllProgress(stdout, stderr io.Writer, kindName string) syncrun.Progress {
	return func(e syncrun.Event) {
		switch e.Stage {
		case "batch_sending":
			fmt.Fprintf(stdout, "    → %s %s\n", kindName, e.Message)
		case "batch_sent":
			fmt.Fprintf(stdout, "      ✓ accepted\n")
		case "batch_failed":
			if se := ingest.IsSendError(e.Err); se != nil {
				fmt.Fprintf(stderr, "      ✗ %s status=%d snippet=%q retryable=%t\n",
					se.Kind, se.Status, se.Snippet, se.Retryable())
			} else {
				fmt.Fprintf(stderr, "      ✗ %v\n", e.Err)
			}
		}
	}
}
