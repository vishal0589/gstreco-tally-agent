package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/syncrun"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// syncCmd drives a one-shot fetch → parse → normalize → batch → ingest cycle
// for a single (Tally company, voucher kind, date window) tuple. It is the
// foundation for A5 (the full cron-driven daemon): A5 will call the same
// pipeline on a schedule across every mapping. Shipping the CLI form first
// lets the operator manually trigger syncs during the remote-server pilot
// before the daemon, tray, and Windows service pieces are built.
//
// Exit codes:
//
//	0 — every batch accepted by the server (or --dry-run completed)
//	1 — infrastructure failure (network, server 5xx, keyring, Tally unreachable)
//	2 — user-fixable configuration error (bad flags, agent not paired, unknown kind)
func syncCmd(args []string) int {
	return runSync(os.Stdout, os.Stderr, args, syncDeps{
		now:           time.Now,
		newOSKeyring:  defaultSecretStore,
		newTallyClient: func(endpoint string) tallyPoster {
			return tally.NewClient(endpoint)
		},
		newIngestClient: defaultNewIngestClient,
		loadConfig:     config.Load,
	})
}

// syncDeps is the seam tests hook through. Real code at the top of syncCmd
// passes the production implementations; tests pass fakes that capture
// requests and stub responses.
type syncDeps struct {
	now             func() time.Time
	newOSKeyring    func() keyring.Store
	newTallyClient  func(endpoint string) tallyPoster
	newIngestClient func(baseURL, connectionID, bearer, secretB64 string) (ingestSender, error)
	loadConfig      func(path string) (*config.Config, error)
}

// tallyPoster + ingestSender alias the syncrun interfaces so tests in
// this package can construct fakes without importing syncrun directly.
// The two pipelines (single-mapping `sync` and walks-all `sync-all`)
// both delegate to syncrun.RunOne — the type aliases keep the seam
// stable.
type tallyPoster = syncrun.TallyPoster
type ingestSender = syncrun.IngestSender

func defaultNewIngestClient(baseURL, connectionID, bearer, secretB64 string) (ingestSender, error) {
	return ingest.NewClient(baseURL, connectionID, bearer, secretB64)
}

func runSync(stdout, stderr io.Writer, args []string, deps syncDeps) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "override config file path")
	serverFlag := fs.String("server", "", "override server URL (default: from config)")
	connectionID := fs.String("connection-id", "", "paired connection id to use when config stores multiple connections")
	tallyURL := fs.String("tally-url", "", "Tally HTTP endpoint (default: from config or http://localhost:9000)")
	tallyCompany := fs.String("tally-company", "", "Tally company name as it appears in Tally (required)")
	kindFlag := fs.String("kind", "", "voucher kind: purchase | sales | credit_note | debit_note (required)")
	period := fs.String("period", "", "month shorthand MMYYYY (e.g. 042026 for Apr 2026); mutually exclusive with --from/--to")
	fromFlag := fs.String("from", "", "window start YYYY-MM-DD (use with --to instead of --period)")
	toFlag := fs.String("to", "", "window end YYYY-MM-DD inclusive")
	batchSize := fs.Int("batch-size", ingest.DefaultBatchSize, "max rows per ingest request (server caps at 1000)")
	dryRun := fs.Bool("dry-run", false, "fetch + parse only; do not POST to the server")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *tallyCompany == "" {
		fmt.Fprintln(stderr, "agentctl sync: --tally-company is required")
		return 2
	}
	if *kindFlag == "" {
		fmt.Fprintln(stderr, "agentctl sync: --kind is required (one of: purchase, sales, credit_note, debit_note)")
		return 2
	}

	tallyKind, ingestKind, err := mapKind(*kindFlag)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: %v\n", err)
		return 2
	}

	from, to, err := resolveWindow(*period, *fromFlag, *toFlag)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: %v\n", err)
		return 2
	}

	cfgPath := resolveConfigPath(*configPath)
	cfg, err := deps.loadConfig(cfgPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			fmt.Fprintln(stderr, "agentctl sync: agent is not paired (run `agentctl pair --code <CODE>` first)")
			return 2
		}
		fmt.Fprintf(stderr, "agentctl sync: load config: %v\n", err)
		return 1
	}
	if !cfg.IsPaired() {
		fmt.Fprintln(stderr, "agentctl sync: config exists but is incomplete — re-run `agentctl pair`")
		return 2
	}

	conn, err := requireOneConnection(cfg, *connectionID)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: %v\n", err)
		return 2
	}

	server := firstNonEmpty(*serverFlag, conn.Server)
	// Multi-endpoint config (A5-2): if more than one endpoint is
	// configured the operator MUST pass --tally-url to disambiguate.
	// Falling back silently to the first entry would mask a wrong
	// port for the requested company.
	endpoints := cfg.ResolveTallyEndpoints(tally.DefaultEndpoint)
	tallyEndpoint := firstNonEmpty(*tallyURL)
	if tallyEndpoint == "" {
		if len(endpoints) > 1 {
			fmt.Fprintf(stderr, "agentctl sync: %d Tally endpoints configured; pass --tally-url to pick one (configured: %s)\n",
				len(endpoints), strings.Join(endpoints, ", "))
			return 2
		}
		if len(endpoints) == 1 {
			tallyEndpoint = endpoints[0]
		} else {
			tallyEndpoint = tally.DefaultEndpoint
		}
	}

	// Build the tally client up-front. Sender comes from the keyring
	// unless --dry-run; in that case we use a noop sender and short-
	// circuit before sending. RunOne short-circuits when rows==0
	// anyway, but the explicit dry-run path lets us skip keyring
	// access too — useful for verifying the parser on a brand-new
	// install before pairing.
	tallyClient := deps.newTallyClient(tallyEndpoint)

	var sender ingestSender
	if !*dryRun {
		ks := deps.newOSKeyring()
		hmacKey, bearerKey := keyring.ConnectionKeys(conn.ConnectionID)
		secret, secErr := ks.Get(keyring.ServiceName, hmacKey)
		if secErr != nil {
			fmt.Fprintf(stderr, "agentctl sync: read hmac secret from keyring: %v\n", secErr)
			return 1
		}
		bearer, bearerErr := ks.Get(keyring.ServiceName, bearerKey)
		if bearerErr != nil {
			fmt.Fprintf(stderr, "agentctl sync: read bearer token from keyring: %v\n", bearerErr)
			return 1
		}
		client, clientErr := deps.newIngestClient(server, conn.ConnectionID, bearer, secret)
		if clientErr != nil {
			fmt.Fprintf(stderr, "agentctl sync: build ingest client: %v\n", clientErr)
			return 1
		}
		sender = client
	} else {
		sender = noopSender{}
	}

	runID := fmt.Sprintf("agentctl-sync-%d", deps.now().Unix())

	// Progress callback prints the same per-stage lines the original
	// inline pipeline did, so existing operator muscle memory holds.
	progress := func(e syncrun.Event) {
		switch e.Stage {
		case "fetching":
			fmt.Fprintf(stdout, "→ fetching %s vouchers from %s for %q (%s..%s)\n",
				*kindFlag, tallyEndpoint, *tallyCompany,
				from.Format("2006-01-02"), to.Format("2006-01-02"))
		case "parsed":
			fmt.Fprintf(stdout, "← %s\n", e.Message)
		case "normalized":
			fmt.Fprintf(stdout, "  → %s\n", e.Message)
		case "batch_sending":
			fmt.Fprintf(stdout, "→ %s\n", e.Message)
		case "batch_sent":
			fmt.Fprintln(stdout, "  ✓ accepted")
		case "batch_failed":
			if se := ingest.IsSendError(e.Err); se != nil {
				fmt.Fprintf(stderr, "  ✗ %s status=%d snippet=%q retryable=%t\n",
					se.Kind, se.Status, se.Snippet, se.Retryable())
			} else {
				fmt.Fprintf(stderr, "  ✗ %v\n", e.Err)
			}
		}
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelRun()

	res, runErr := syncrun.RunOne(runCtx, tallyClient, sender, syncrun.Request{
		TallyCompany: *tallyCompany,
		TallyKind:    tallyKind,
		IngestKind:   ingestKind,
		From:         from,
		To:           to,
		RunID:        runID,
		RunKind:      "manual",
		BatchSize:    *batchSize,
	}, progress)
	if runErr != nil {
		fmt.Fprintf(stderr, "agentctl sync: %v\n", runErr)
		return 1
	}

	for _, w := range res.ParseWarnings {
		fmt.Fprintf(stdout, "  ⚠ %s\n", w)
	}

	if *dryRun {
		fmt.Fprintf(stdout, "\nrun_id=%s  rows=%d (dropped %d on normalize)\n",
			runID, res.RowCount, res.DroppedOnNormalize)
		fmt.Fprintln(stdout, "✓ dry-run complete (no rows sent)")
		return 0
	}
	if res.RowCount == 0 {
		fmt.Fprintln(stdout, "✓ nothing to send (zero ingest rows)")
		return 0
	}

	fmt.Fprintf(stdout, "\nrun_id=%s  sent=%d  failed=%d  rows=%d\n",
		runID, res.BatchesSent, res.BatchesFailed, res.RowCount)
	if res.BatchesFailed > 0 {
		return 1
	}
	fmt.Fprintln(stdout, "✓ sync complete")
	return 0
}

// noopSender is the dry-run stand-in. RunOne short-circuits the send
// loop when rows==0; for non-empty payloads dry-run paths also skip
// keyring access so a brand-new agent can verify the parser before
// pairing. This guards against future runner changes that reach the
// sender on dry-run paths.
type noopSender struct{}

func (noopSender) Send(_ context.Context, _ tally.IngestRequestBody) error { return nil }

// mapKind translates the user-facing --kind string to the (Tally envelope
// filter, server-side ingest kind) pair. The two enums diverge for
// `rcm_self_invoice`, which the agent infers per-voucher from the
// IsRcmApplicable flag rather than asking the user to know in advance.
func mapKind(s string) (tally.VoucherKind, tally.IngestKind, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "purchase":
		return tally.VoucherPurchase, tally.IngestKindPurchase, nil
	case "sales":
		return tally.VoucherSales, tally.IngestKindSales, nil
	case "credit_note", "credit-note", "creditnote":
		return tally.VoucherCreditNote, tally.IngestKindCreditNote, nil
	case "debit_note", "debit-note", "debitnote":
		return tally.VoucherDebitNote, tally.IngestKindDebitNote, nil
	default:
		return "", "", fmt.Errorf("unknown --kind %q (want one of: purchase, sales, credit_note, debit_note)", s)
	}
}

// resolveWindow accepts EITHER --period MMYYYY OR (--from + --to) and returns
// inclusive UTC dates. Mutually exclusive on purpose: passing both is
// ambiguous and we want the operator to be explicit.
func resolveWindow(period, fromStr, toStr string) (time.Time, time.Time, error) {
	hasPeriod := period != ""
	hasRange := fromStr != "" || toStr != ""
	if hasPeriod && hasRange {
		return time.Time{}, time.Time{}, errors.New("pass either --period OR --from+--to, not both")
	}
	if !hasPeriod && !hasRange {
		return time.Time{}, time.Time{}, errors.New("--period MMYYYY or --from YYYY-MM-DD --to YYYY-MM-DD is required")
	}
	if hasPeriod {
		return parsePeriod(period)
	}
	if fromStr == "" || toStr == "" {
		return time.Time{}, time.Time{}, errors.New("--from and --to must both be set")
	}
	from, err := time.Parse("2006-01-02", fromStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--from %q: want YYYY-MM-DD", fromStr)
	}
	to, err := time.Parse("2006-01-02", toStr)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--to %q: want YYYY-MM-DD", toStr)
	}
	if to.Before(from) {
		return time.Time{}, time.Time{}, fmt.Errorf("--to %s is before --from %s", toStr, fromStr)
	}
	return from, to, nil
}

// parsePeriod accepts MMYYYY and returns the inclusive first..last day of that
// calendar month in UTC. Picks UTC over IST because every other timestamp in
// the agent uses UTC; Tally's date filter is calendar-day so the timezone
// doesn't change which vouchers come back.
func parsePeriod(s string) (time.Time, time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) != 6 {
		return time.Time{}, time.Time{}, fmt.Errorf("--period %q: want MMYYYY (6 digits)", s)
	}
	mm, err := strconv.Atoi(s[:2])
	if err != nil || mm < 1 || mm > 12 {
		return time.Time{}, time.Time{}, fmt.Errorf("--period %q: month must be 01..12", s)
	}
	yyyy, err := strconv.Atoi(s[2:])
	if err != nil || yyyy < 2000 || yyyy > 2100 {
		return time.Time{}, time.Time{}, fmt.Errorf("--period %q: year out of range", s)
	}
	from := time.Date(yyyy, time.Month(mm), 1, 0, 0, 0, 0, time.UTC)
	// Last day = first day of next month minus 1 day. Handles February + leap
	// years correctly without a calendar table.
	to := from.AddDate(0, 1, -1)
	return from, to, nil
}

// firstNonEmpty returns the first non-empty string. Used to layer flag
// override → config value → built-in default.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
