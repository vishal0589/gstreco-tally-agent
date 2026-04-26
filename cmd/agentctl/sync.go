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
		newOSKeyring:  func() keyring.Store { return keyring.NewOSKeyring() },
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

// tallyPoster is the subset of *tally.Client this command uses. Narrow on
// purpose so tests don't have to construct a full HTTP server.
type tallyPoster interface {
	PostXML(ctx context.Context, body []byte) ([]byte, error)
}

// ingestSender is the subset of *ingest.Client this command uses.
type ingestSender interface {
	Send(ctx context.Context, body tally.IngestRequestBody) error
}

func defaultNewIngestClient(baseURL, connectionID, bearer, secretB64 string) (ingestSender, error) {
	return ingest.NewClient(baseURL, connectionID, bearer, secretB64)
}

func runSync(stdout, stderr io.Writer, args []string, deps syncDeps) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "override config file path")
	serverFlag := fs.String("server", "", "override server URL (default: from config)")
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

	cfgPath := *configPath
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
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

	server := firstNonEmpty(*serverFlag, cfg.Server)
	tallyEndpoint := firstNonEmpty(*tallyURL, cfg.TallyEndpoint, tally.DefaultEndpoint)

	// Build envelope first so flag/template errors fail before we touch the
	// network. Tally rejects requests with control characters in the company
	// name; BuildDayBookXML enforces that.
	envelope, err := tally.BuildDayBookXML(tally.DayBookRequest{
		Company: *tallyCompany,
		From:    from,
		To:      to,
		Kind:    tallyKind,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: build envelope: %v\n", err)
		return 2
	}

	fmt.Fprintf(stdout, "→ fetching %s vouchers from %s for %q (%s..%s)\n",
		*kindFlag, tallyEndpoint, *tallyCompany,
		from.Format("2006-01-02"), to.Format("2006-01-02"))

	tallyClient := deps.newTallyClient(tallyEndpoint)
	fetchCtx, cancelFetch := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelFetch()

	respXML, err := tallyClient.PostXML(fetchCtx, envelope)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: tally fetch failed: %v\n", err)
		return 1
	}

	parsed, err := tally.ParseDayBookV3(respXML)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: parse response: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "← parsed %d vouchers (status=%d, warnings=%d)\n",
		len(parsed.Vouchers), parsed.Status, len(parsed.Warnings))
	for _, w := range parsed.Warnings {
		fmt.Fprintf(stdout, "  ⚠ %s\n", w)
	}

	rows := make([]tally.IngestVoucherRow, 0, len(parsed.Vouchers))
	dropped := 0
	for _, v := range parsed.Vouchers {
		out, err := tally.Normalize(v, tally.NormalizeOptions{
			Kind:    ingestKind,
			RunKind: "manual",
		})
		if err != nil {
			fmt.Fprintf(stdout, "  ⚠ normalize voucher %s: %v\n", voucherDisplayID(v), err)
			dropped++
			continue
		}
		rows = append(rows, out...)
	}
	fmt.Fprintf(stdout, "  → %d ingest rows (dropped %d on normalize)\n", len(rows), dropped)

	if *dryRun {
		fmt.Fprintln(stdout, "✓ dry-run complete (no rows sent)")
		return 0
	}
	if len(rows) == 0 {
		fmt.Fprintln(stdout, "✓ nothing to send (zero ingest rows)")
		return 0
	}

	ks := deps.newOSKeyring()
	hmacKey, bearerKey := keyring.ConnectionKeys(cfg.ConnectionID)
	secret, err := ks.Get(keyring.ServiceName, hmacKey)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: read hmac secret from keyring: %v\n", err)
		return 1
	}
	bearer, err := ks.Get(keyring.ServiceName, bearerKey)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: read bearer token from keyring: %v\n", err)
		return 1
	}

	client, err := deps.newIngestClient(server, cfg.ConnectionID, bearer, secret)
	if err != nil {
		fmt.Fprintf(stderr, "agentctl sync: build ingest client: %v\n", err)
		return 1
	}

	runID := fmt.Sprintf("agentctl-sync-%d", deps.now().Unix())
	chunks := ingest.SplitRows(rows, *batchSize)
	batches := ingest.BuildBatches(runID, runID, ingestKind, *tallyCompany, "manual", chunks)

	sendCtx, cancelSend := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancelSend()

	sent, failed := 0, 0
	for i, body := range batches {
		fmt.Fprintf(stdout, "→ batch %d/%d (%d rows, request_id=%s, is_final=%t)\n",
			i+1, len(batches), len(body.Batch), body.RequestID, body.IsFinal)
		if err := client.Send(sendCtx, body); err != nil {
			failed++
			if se := ingest.IsSendError(err); se != nil {
				fmt.Fprintf(stderr, "  ✗ %s status=%d snippet=%q retryable=%t\n",
					se.Kind, se.Status, se.Snippet, se.Retryable())
			} else {
				fmt.Fprintf(stderr, "  ✗ %v\n", err)
			}
			// Don't stop the run on one batch failure — surface every error so
			// the operator gets a complete picture in one CLI invocation.
			continue
		}
		sent++
		fmt.Fprintln(stdout, "  ✓ accepted")
	}

	fmt.Fprintf(stdout, "\nrun_id=%s  sent=%d  failed=%d  rows=%d\n", runID, sent, failed, len(rows))
	if failed > 0 {
		return 1
	}
	fmt.Fprintln(stdout, "✓ sync complete")
	return 0
}

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

// voucherDisplayID returns a human-friendly identifier for a voucher in CLI
// output. Mirrors voucherID inside parser_v3 but uses public fields so this
// file doesn't need internal access.
func voucherDisplayID(v tally.RawVoucher) string {
	if v.GUID != "" {
		return v.GUID
	}
	if v.VoucherNumber != "" {
		return v.VoucherNumber
	}
	return "(unknown)"
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
