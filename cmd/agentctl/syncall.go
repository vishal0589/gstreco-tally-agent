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
	connectionID := fs.String("connection-id", "", "paired connection id to use; omit to walk all stored connections")
	period := fs.String("period", "", "month shorthand MMYYYY (e.g. 042026); mutually exclusive with --from/--to")
	fromFlag := fs.String("from", "", "window start YYYY-MM-DD (use with --to instead of --period)")
	toFlag := fs.String("to", "", "window end YYYY-MM-DD inclusive")
	kindsFlag := fs.String("kinds", "purchase,sales,credit_note,debit_note,journal,payment,tax_ledger",
		"comma-separated sync kinds to fetch per mapping")
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

	cfgPath := resolveConfigPath(*configPath)
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

	allConnections, err := requireAllConnections(cfg)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync-all: %v\n", err)
		return 2
	}
	selectedConnections := allConnections
	if strings.TrimSpace(*connectionID) != "" {
		conn, connErr := requireOneConnection(cfg, *connectionID)
		if connErr != nil {
			fmt.Fprintf(stderr, "agentctl sync-all: %v\n", connErr)
			return 2
		}
		selectedConnections = []config.PairedConnection{conn}
	}

	ks := deps.newOSKeyring()
	totalMappings := 0
	totalMappingsRun := 0
	totalRows := 0
	totalBatchesSent := 0
	totalBatchesFailed := 0
	totalServerSkipped := 0
	totalServerErrors := 0
	totalFatalErrors := 0
	connectionsWithMappings := 0
	hadFetchMappingsError := false

	for idx, conn := range selectedConnections {
		server := firstNonEmpty(*serverFlag, conn.Server)
		hmacKey, bearerKey := keyring.ConnectionKeys(conn.ConnectionID)
		secret, secErr := ks.Get(keyring.ServiceName, hmacKey)
		if secErr != nil {
			fmt.Fprintf(stderr, "agentctl sync-all: read hmac secret for %s: %v\n", conn.ConnectionID, secErr)
			return 1
		}
		bearer, bearerErr := ks.Get(keyring.ServiceName, bearerKey)
		if bearerErr != nil {
			fmt.Fprintf(stderr, "agentctl sync-all: read bearer token for %s: %v\n", conn.ConnectionID, bearerErr)
			return 1
		}

		client, clientErr := deps.newIngestClient(server, conn.ConnectionID, bearer, secret)
		if clientErr != nil {
			fmt.Fprintf(stderr, "agentctl sync-all: build ingest client for %s: %v\n", conn.ConnectionID, clientErr)
			return 1
		}

		fetchCtx, cancelFetch := context.WithTimeout(context.Background(), 30*time.Second)
		mappings, fetchErr := client.FetchActiveMappings(fetchCtx)
		cancelFetch()
		if fetchErr != nil {
			fmt.Fprintf(stderr, "agentctl sync-all: fetch mappings for %s: %v\n", conn.ConnectionID, fetchErr)
			hadFetchMappingsError = true
			totalFatalErrors++
			if !*continueOnError {
				return 1
			}
			continue
		}
		if len(mappings.Mappings) == 0 {
			fmt.Fprintf(stdout, "→ connection %s has no active mappings (server=%s)\n", conn.ConnectionID, server)
			continue
		}

		connectionsWithMappings++
		totalMappings += len(mappings.Mappings)
		fmt.Fprintf(stdout, "→ connection %d/%d: %s (%d active mapping(s), fetched %s)\n",
			idx+1, len(selectedConnections), mappings.ConnectionID, len(mappings.Mappings), mappings.FetchedAt)
		fmt.Fprintf(stdout, "  window: %s..%s · kinds: %s · dry-run: %t\n",
			from.Format("2006-01-02"), to.Format("2006-01-02"),
			strings.Join(kindNamesFromResolved(kinds), ","), *dryRun)

		var sender syncrun.IngestSender = client
		if *dryRun {
			sender = noopSender{}
		}

		runIDPrefix := fmt.Sprintf("syncall-%d", deps.now().Unix())
		res := syncrun.WalkAll(context.Background(), syncrun.WalkOptions{
			Mappings:        mappings.Mappings,
			Kinds:           kinds,
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
			PerMappingHook: func(mappingIdx, total int, m ingest.ActiveMapping) {
				fmt.Fprintf(stdout, "[%d/%d] %s @ %s\n",
					mappingIdx+1, total, m.TallyCompanyName, m.TallyEndpoint)
			},
			PerKindResultHook: func(m ingest.ActiveMapping, k syncrun.KindPair, r syncrun.Result, fatalErr error) {
				if fatalErr != nil {
					fmt.Fprintf(stderr, "  ✗ %s: %v\n", k.Name, fatalErr)
					return
				}
				fmt.Fprintf(stdout, "    %s: rows=%d sent=%d failed=%d landed=%d skipped=%d server_errors=%d\n",
					k.Name,
					r.RowCount,
					r.BatchesSent,
					r.BatchesFailed,
					r.ServerInserted+r.ServerAmended,
					r.ServerSkipped,
					r.ServerErrors)
				for _, w := range r.ParseWarnings {
					fmt.Fprintf(stdout, "      ⚠ %s\n", w)
				}
				for _, f := range r.ServerFindings {
					fmt.Fprintf(stdout, "      ⚠ %s\n", formatServerFinding(f))
				}
			},
			PerRunProgress: func(_ ingest.ActiveMapping, k syncrun.KindPair) syncrun.Progress {
				return makeSyncAllProgress(stdout, stderr, k.Name)
			},
		})

		totalMappingsRun += res.MappingsRun
		totalRows += res.TotalRows
		totalBatchesSent += res.TotalBatchesSent
		totalBatchesFailed += res.TotalBatchesFailed
		totalServerSkipped += res.TotalServerSkipped
		totalServerErrors += res.TotalServerErrors
		totalFatalErrors += res.FatalErrors

		if res.StoppedEarly {
			fmt.Fprintln(stderr, "stopping (--continue-on-error=false)")
			return 1
		}

		fmt.Fprintf(stdout,
			"  connection summary: mappings_ran=%d rows=%d batches_sent=%d failed=%d skipped=%d server_errors=%d fatal_errors=%d\n\n",
			res.MappingsRun,
			res.TotalRows,
			res.TotalBatchesSent,
			res.TotalBatchesFailed,
			res.TotalServerSkipped,
			res.TotalServerErrors,
			res.FatalErrors,
		)
	}

	if connectionsWithMappings == 0 {
		if hadFetchMappingsError {
			fmt.Fprintln(stderr, "agentctl sync-all: could not fetch mappings for any selected connection.")
			return 1
		}
		fmt.Fprintln(stderr, "agentctl sync-all: no active mappings.")
		fmt.Fprintln(stderr, "Run `agentctl discover` to enumerate Tally instances, then map each company to a GSTIN at the tenant's Settings → Tally page before running sync-all.")
		return 2
	}

	fmt.Fprintf(stdout,
		"summary: connections_with_mappings=%d/%d · mappings_ran=%d/%d · rows=%d · batches sent=%d failed=%d · skipped=%d · server errors=%d · fatal errors=%d\n",
		connectionsWithMappings, len(selectedConnections),
		totalMappingsRun, totalMappings, totalRows,
		totalBatchesSent, totalBatchesFailed,
		totalServerSkipped, totalServerErrors, totalFatalErrors)

	if totalFatalErrors > 0 || totalBatchesFailed > 0 || totalServerErrors > 0 {
		return 1
	}
	if *dryRun {
		fmt.Fprintln(stdout, "✓ dry-run sync-all complete (no rows sent)")
	} else {
		fmt.Fprintln(stdout, "✓ sync-all complete")
	}
	return 0
}

// kindNamesFromResolved returns just the names — used for the
// "kinds: purchase,sales,..." header.
func kindNamesFromResolved(kinds []syncrun.KindPair) []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = k.Name
	}
	return out
}

// parseKinds expands the comma-separated --kinds flag into resolved
// pairs. Order is preserved so operators can run kinds in the order
// they specified ("--kinds sales,purchase" → sales first).
func parseKinds(s string) ([]syncrun.KindPair, error) {
	out := make([]syncrun.KindPair, 0, 7)
	seen := map[string]struct{}{}
	for _, raw := range strings.Split(s, ",") {
		k := strings.TrimSpace(raw)
		if k == "" {
			continue
		}
		pair, err := mapKind(k)
		if err != nil {
			return nil, err
		}
		if _, dup := seen[pair.Name]; dup {
			continue
		}
		seen[pair.Name] = struct{}{}
		out = append(out, pair)
	}
	if len(out) == 0 {
		return nil, errors.New("--kinds is empty after parsing (use a comma-separated list of: purchase, sales, credit_note, debit_note, journal, payment, tax_ledger)")
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
			fmt.Fprintf(stdout, "      ✓ %s\n", e.Message)
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
