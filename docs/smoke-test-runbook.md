# Smoke test runbook — agent end-to-end

> **Phase A note (2026-04-26):** for the **multi-port remote-server pilot**
> with 5–6 Tally Prime instances on different ports, follow
> [`pilot-remote-install.md`](./pilot-remote-install.md) instead. That doc
> uses the new `agentctl pair → discover → sync-all` flow shipped in
> A5-1/A5-2/A5-3 plus server S14/S15. THIS runbook validates the
> pre-Phase-A baseline (hand-crafted Go program against /api/tally/ingest)
> and is still the right tool for verifying A4-era ingest primitives in
> isolation.

Once PRs A1–A4 land on `dev`, this runbook validates the full pairing → sign → ingest loop against a live GST Reco server. It's the minimum check that the 6 agent PRs wire together correctly before A5 (scheduler) starts driving them.

**Pre-requisites:** A1, A2a, A2b, A3, A4 merged to `dev`. Server S1–S7 already live on GST_Reco `dev`. Don't attempt this until the stack is complete — partial merges miss pieces (e.g. without A4 there's no HMAC client, without A3 there's no keyring).

## 1 — Build the agent

```bash
cd ~/gstreco-tally-agent
git fetch origin dev && git reset --hard origin/dev
make release
```

Three platform binaries should land under `dist/`:

- `dist/windows-amd64/gstreco-tally-{agent,tray,agentctl}.exe`
- `dist/linux-amd64/gstreco-tally-{agent,tray,agentctl}`
- `dist/darwin-arm64/gstreco-tally-{agent,tray,agentctl}`

Smoke the host binary too:

```bash
./bin/gstreco-tally-agent --version    # should print "gstreco-tally-agent <sha> …"
./bin/gstreco-tally-agentctl help
./bin/gstreco-tally-agentctl status    # "agent not paired (run …)"
```

## 2 — Generate a pair code (server side)

Option A — web UI: log into `https://gstreco.m2ai.ai` as the test company's admin, go to **Settings → Tally**, click **+ Pair new agent**. Copy the 6-character code (format `ABC-D23`).

Option B — curl (faster for repeated smokes):

```bash
curl -X POST https://gstreco.m2ai.ai/api/tally/pair/init \
  -H "cookie: $(cat ~/.gstreco-session-cookie)" \
  -H "content-type: application/json" \
  -d '{}'
```

Response body includes `{code_id, code, expires_at}`. Codes live for 15 minutes (per `src/lib/tally/pair-code.ts`), so pair promptly.

## 3 — Pair the agent

```bash
./bin/gstreco-tally-agentctl pair \
  --code ABC-D23 \
  --server https://gstreco.m2ai.ai
```

Expected output:

```
✓ paired successfully (connection_id=<uuid> company_id=<uuid>)
```

Verification:

```bash
./bin/gstreco-tally-agentctl status
# paired
#   server:        https://gstreco.m2ai.ai
#   connection_id: <uuid>
#   device_name:   <hostname>
#   paired_at:     2026-04-23T10:30:00Z
```

For a shared Tally machine, repeat `pair` once per GST Reco tenant.
The agent keeps multiple stored connections in one config/keyring and
`status` prints them all.

On darwin/linux the config lives at `~/.gstreco-agent/config.yaml`. On Windows it's `%ProgramData%\GST Reco\agent\config.yaml`. The HMAC secret + bearer token land in the OS keyring under service `gstreco-tally-agent` with keys `<connection_id>.hmac_secret` and `<connection_id>.bearer_token` — they must NOT appear on disk.

Sanity check the keyring write on darwin:

```bash
security find-generic-password -s gstreco-tally-agent -w 2>&1 | head -c 20
# Prints the first 20 chars of a base64 secret.
```

Expected failure modes:

| Agent exit | Meaning | Action |
|---|---|---|
| 0 | paired | — |
| 2 + `invalid code format` | typo — not 6 Crockford chars | re-check the code |
| 2 + `code_gone` | expired or already claimed | generate a fresh code |
| 1 + network error | server unreachable | check VPN / firewall |
| 1 + keyring error | macOS Keychain locked | `security unlock-keychain` |

## 4 — Craft a test ingest batch

Write a tiny Go program that:

1. Loads the config (`internal/config.Load`) and keyring secret.
2. Builds a minimal `tally.IngestRequestBody` with one `IngestVoucherRow`.
3. Calls `ingest.Client.Send()`.

This historical smoke snippet assumes a single stored connection. If the
machine now holds more than one tenant pairing, resolve the intended
`connection_id` from `cfg.PairedConnections()` instead of reading only the
legacy top-level fields.

```go
// smoke/main.go
package main

import (
    "context"
    "fmt"
    "os"
    "time"

    "github.com/vishal0589/gstreco-tally-agent/internal/config"
    "github.com/vishal0589/gstreco-tally-agent/internal/ingest"
    "github.com/vishal0589/gstreco-tally-agent/internal/keyring"
    "github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func main() {
    cfg, err := config.Load(config.DefaultPath())
    if err != nil {
        fmt.Fprintln(os.Stderr, "load config:", err)
        os.Exit(1)
    }
    ks := keyring.NewOSKeyring()
    hmacKey, bearerKey := keyring.ConnectionKeys(cfg.ConnectionID)
    secret, _ := ks.Get(keyring.ServiceName, hmacKey)
    bearer, _ := ks.Get(keyring.ServiceName, bearerKey)

    c, err := ingest.NewClient(cfg.Server, cfg.ConnectionID, bearer, secret)
    if err != nil {
        fmt.Fprintln(os.Stderr, "new client:", err)
        os.Exit(1)
    }

    vendor := "SMOKE Vendors Ltd"
    gstin := "29ABCDE1234F1Z5"
    body := tally.IngestRequestBody{
        RunID:        fmt.Sprintf("smoke-%d", time.Now().Unix()),
        Kind:         tally.IngestKindPurchase,
        TallyCompany: "Smoke Test Co",
        Batch: []tally.IngestVoucherRow{{
            InvoiceNumber: "SMOKE-001",
            InvoiceDate:   time.Now().Format("2006-01-02"),
            InvoiceValue:  11800,
            TaxableValue:  10000,
            IGST:          1800,
            VendorName:    &vendor,
            VendorGSTIN:   &gstin,
        }},
        RunKind:   "manual",
        IsFinal:   true,
        RequestID: fmt.Sprintf("smoke-%d-1", time.Now().Unix()),
    }
    if err := c.Send(context.Background(), body); err != nil {
        fmt.Fprintln(os.Stderr, "send:", err)
        os.Exit(1)
    }
    fmt.Println("✓ ingest accepted")
}
```

Build + run:

```bash
go run ./smoke
# ✓ ingest accepted
```

## 5 — Verify in Supabase

The smoke should produce three rows:

```sql
-- 1. The tally_sync_run (opened on first batch, closed on is_final=true)
select run_id, kind, status, batches_received, rows_upserted
from tally_sync_runs
where run_id like 'smoke-%'
order by started_at desc
limit 1;
-- status should be 'completed', batches_received=1, rows_upserted=1

-- 2. The purchase_invoice
select invoice_number, invoice_value, igst, source
from purchase_invoices
where invoice_number = 'SMOKE-001'
order by created_at desc
limit 1;
-- source should be 'tally_agent'

-- 3. The ingest log row (retention: 90 days)
select request_id, http_status, row_count, error_code
from tally_ingest_log
where request_id like 'smoke-%'
order by created_at desc
limit 1;
-- http_status=200, error_code=null
```

## 6 — Dedup check (idempotency)

Re-run step 4 with the **same** `run_id` and `request_id`. The server's nonce check should short-circuit the second request without creating a duplicate row:

```sql
select count(*) from purchase_invoices where invoice_number = 'SMOKE-001';
-- should still be 1, not 2
```

## 7 — Outbox flow (offline resumption)

With the network up, the outbox stays empty:

```bash
sqlite3 ~/.gstreco-agent/outbox.db 'select count(*) from outbox;'
# 0
```

Simulate a server outage by running smoke against an unreachable URL in config (edit `server:` in `config.yaml` temporarily). The `Send()` will return a `SendError{Kind: network}` which is retryable. When A5 lands it will enqueue via `outbox.Enqueue`; for smoke-purposes, call `outbox.Enqueue` directly from a test and confirm:

```bash
sqlite3 ~/.gstreco-agent/outbox.db 'select run_id, kind, attempts, last_error from outbox;'
```

Restore the correct server URL, wait for A5's scheduler to flush (or call the runner manually once A5 lands), and confirm the entry disappears.

## 8 — Tear down

```bash
# Delete config so the next smoke starts clean.
rm -rf ~/.gstreco-agent/

# Delete keyring entries (darwin).
security delete-generic-password -s gstreco-tally-agent

# On the server, revoke the connection via Settings → Tally → Revoke.
```

## Success criteria

- [ ] `agentctl pair` exits 0; `agentctl status` shows paired.
- [ ] HMAC secret lives only in the keyring, never in `config.yaml`.
- [ ] Smoke `Send()` gets `200` from `/api/tally/ingest`.
- [ ] `tally_sync_runs` row shows `status='completed'` after `is_final=true` batch.
- [ ] `purchase_invoices` row exists with `source='tally_agent'`.
- [ ] Re-running the same batch produces no duplicate row (nonce + inline-unique dedup).
- [ ] Outbox survives process restart (enqueue → close → reopen → Pending returns the entry).
- [ ] All three platform binaries run `--version` cleanly.

Failing any of these means don't proceed to A5 until the failure is understood — A5's scheduler trusts these primitives.
