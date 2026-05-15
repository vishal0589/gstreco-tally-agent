# Pilot — remote-server install runbook

> Historical note: this document describes the older `agentctl` / PowerShell
> pilot workflow. The current product direction is installer-first on Windows:
> download `gstreco-tally-installer.exe`, approve the machine in the browser,
> and let the installer handle service repair plus discovery-first setup.
> On a machine that already serves one GST Reco tenant, rerun the installer
> with `--add-tenant` (or use the fallback `agentctl pair`) before discovery.
> Keep this runbook for support recovery, diagnostics, and direct `agentctl`
> usage when the installer path cannot complete.

This is the operator-facing runbook for installing the GST Reco Tally
agent on a remote Windows server that runs **multiple Tally Prime
instances on different ports** (the multi-tenant SaaS shape — one
agent install, many endpoints, one pair per tenant).

This runbook supersedes `docs/smoke-test-runbook.md` for the
remote-server pilot. The smoke runbook is still valid for end-to-end
validation against the *current* `dev` baseline (hand-crafted JSON
ingest); use this one for pilots after Phase A merges (S14 + S15 +
A5-1 + A5-2 + A5-3 + migration 00036 applied).

## Pre-requisites

Before running anything on the remote box, the following must be true:

- [ ] **Server PRs merged to dev:** S14 (#104), S15 (#105) on `vishal0589/GST_Reco`.
- [ ] **Migration 00036 applied** to Supabase project `tfehukahfozuihcqrmoz`. SQL inline in the S14 PR body.
- [ ] **Agent PRs merged to dev:** A5-1 (#11), A5-2 (#12), A5-3 (#13) on `vishal0589/gstreco-tally-agent`.
- [ ] **Windows binary** built off the latest `dev` (or the A5-3 PR branch). See "Build" below.
- [ ] **Pair code** generated in the GST Reco Settings → Tally page on the dev preview URL.
- [ ] **Tally Prime 3.x** confirmed on the remote box (4.x = STOP, parser_v4 not built).
- [ ] **Tally ODBC HTTP server** enabled on each instance you want to sync.

## Pilot endpoint

```
https://gst-reco-git-dev-vishal-srivastavas-projects-9b402fcc.vercel.app
```

Stable Vercel alias for the `dev` branch. NOT prod (`gstreco.m2ai.ai`). Per memory, prod stays untouched until the pilot is green for 7 days and the user explicitly authorises promotion.

## Build (operator's machine, not the remote box)

```bash
cd ~/gstreco-tally-agent
git fetch origin dev && git checkout dev && git pull --ff-only
make windows
ls -la dist/windows-amd64/
# Expect: gstreco-tally-{agent,tray,agentctl}.exe
```

Transfer `dist/windows-amd64/gstreco-tally-agentctl.exe` to the remote
box via AnyDesk file transfer or any other method. Place it somewhere
in the operator's PATH (or run with the absolute path). The `agent.exe`
+ `tray.exe` are not needed for the pilot — those are the future
daemon (A5) + tray (A5) entrypoints.

## Step 1 — Confirm Tally version + ODBC + ports

Before pairing, verify the remote box. **All three checks must pass.**

### 1a. Tally Prime version

In Tally on the remote box: press **F1** → look for the version line
at the top of the help panel (e.g. `Release 3.0.1`). Or F1 → About if
your build has it.

Required: **Release 3.x.** If 4.x, STOP — parser_v4 (A7) is not
built. If ERP 9, STOP — out of V1 scope per master plan decision #5.

### 1b. ODBC HTTP server enabled

For each Tally instance you want to sync:

In Tally: **F1** → **Settings** → **Connectivity** → **Client/Server
Configuration**. Confirm:

| Setting | Required value |
|---|---|
| TallyPrime acts as | `Both` or `Server` (NOT `Client`) |
| Enable ODBC | `Yes` |
| Port | (default `9000`; varies per instance — record each one) |

### 1c. Enumerate ports + processes

Open PowerShell on the remote box:

```powershell
Get-Process tally* -ErrorAction SilentlyContinue | ForEach-Object {
  $proc = $_
  Get-NetTCPConnection -OwningProcess $proc.Id -State Listen -ErrorAction SilentlyContinue |
    Where-Object { $_.LocalAddress -in '0.0.0.0','127.0.0.1','::' } |
    Select-Object @{n='ProcessName';e={$proc.ProcessName}},
                  @{n='PID';e={$proc.Id}},
                  LocalAddress, LocalPort
} | Sort-Object LocalPort | Format-Table
```

Save the output. The agent's discover sweep covers `9000-9020` by
default; if any of your Tally ports fall outside that range, you'll
need to pass `--ports` or `--endpoint` to discover.

Sanity check one of the ports responds:

```powershell
Invoke-WebRequest http://localhost:9000 -UseBasicParsing -TimeoutSec 5 |
  Select-Object StatusCode, StatusDescription
```

Expect `200 OK` with a tiny XML body.

## Step 2 — Pair the agent (one-time)

Generate a pair code in the dev preview UI: log in as the test
company's admin → Settings → Tally → **+ Pair new agent** → copy the
6-character code (format `ABC-D23`).

On the remote box (PowerShell or cmd):

```bash
gstreco-tally-agentctl.exe pair ^
  --code ABC-D23 ^
  --server https://gst-reco-git-dev-vishal-srivastavas-projects-9b402fcc.vercel.app
```

(Use `^` for line continuation in cmd.exe; `` ` `` in PowerShell.)

Expected output:

```
✓ paired successfully (connection_id=<uuid> company_id=<uuid>)
```

Verify:

```bash
gstreco-tally-agentctl.exe status
# paired
#   server:        https://gst-reco-git-dev-vishal-srivastavas-projects-9b402fcc.vercel.app
#   connection_id: <uuid>
#   device_name:   <hostname>
#   paired_at:     2026-04-26T...Z
```

If the same Windows machine needs to serve multiple GST Reco tenants,
repeat `pair` once per tenant. The agent keeps one stored connection
per tenant in the same config/keyring, and `status` will list every
stored connection.

The HMAC secret + bearer token are stored in **Windows Credential
Manager** under service `gstreco-tally-agent` (NOT on disk). The
config file at `%ProgramData%\GST Reco\agent\config.yaml` holds only
non-secret state.

If the pair fails:

| Exit | Meaning | Action |
|---|---|---|
| `2` + `invalid code format` | typo — not 6 Crockford chars | re-check the code |
| `2` + `code_gone` | expired (15 min TTL) or already claimed | regenerate in the UI |
| `1` + network error | server unreachable / VPN / firewall | check connectivity |
| `1` + keyring error | Credential Manager unreachable | check Windows user session |

## Step 3 — Discover Tally instances + companies

```bash
gstreco-tally-agentctl.exe discover
```

Default flags scan `127.0.0.1:9000-9020` with 5-second per-port
timeout, 4 parallel workers. Override if needed:

```bash
# Wider port range
gstreco-tally-agentctl.exe discover --ports 9000-9099

# Specific endpoints (skips the sweep)
gstreco-tally-agentctl.exe discover ^
  --endpoint http://localhost:9000 ^
  --endpoint http://localhost:9001 ^
  --endpoint http://localhost:9002

# Probe but don't push or save (sanity check first)
gstreco-tally-agentctl.exe discover --dry-run
```

Output (example with 5 Tally instances on the box):

```
discover scan: 10 endpoint(s)

ENDPOINT                VERSION        COMPANIES  LAT     STATUS
http://127.0.0.1:9000   Release 3.0.1  2          47ms    ok
http://127.0.0.1:9001   Release 3.0.1  1          51ms    ok
http://127.0.0.1:9002   Release 3.0.1  1          39ms    ok
http://127.0.0.1:9003   Release 3.0.1  1          43ms    ok
http://127.0.0.1:9004   Release 3.0.1  1          46ms    ok
http://127.0.0.1:9005   —              —          0ms     connect refused
http://127.0.0.1:9006   —              —          0ms     connect refused
...

http://127.0.0.1:9000
  - PLLUM CASA  (guid=...)
  - PLLUM LEGNO PRIVATE LIMITED  (guid=...)
http://127.0.0.1:9001
  - <COMPANY NAME>  (guid=...)
...

✓ saved 5 endpoint(s) to C:\ProgramData\GST Reco\agent\config.yaml
✓ pushed catalog (6 companies across 5 endpoints, request_id=discover-1714134567)
→ next step: open the mapping UI at https://...vercel.app/settings/tally and choose a GSTIN per company.
```

If discover exits 2 with **"no Tally Prime 3.x instances found"**:
double-check Tally is running, ODBC is enabled (Step 1b), and the
port range covers your instances.

## Step 4 — Map each Tally company to a GSTIN

In the dev preview UI: **Settings → Tally → click your connection →
"Map your Tally companies"**. For each row in the catalog:

1. Pick the right **GSTIN** from the dropdown (the GSTIN in the
   real-world books that this Tally company corresponds to).
2. OR pick **— ignore this company —** if it shouldn't sync (test
   files, archive copies, etc.).
3. Click **Save**.

The mapping page groups rows by Tally endpoint (one section per port)
so you can see at a glance which company lives behind which Tally
instance.

## Step 5 — First sync

Pick a small period to start (one month is fine). Run:

```bash
gstreco-tally-agentctl.exe sync-all --period 042026
```

Expected: per-mapping → per-kind output streaming through, ending in
a grand-total summary. Default `--kinds` is all 4 (purchase, sales,
credit_note, debit_note).

```
→ 6 active mapping(s) for connection <uuid> (fetched 2026-04-26T...)
  window: 2026-04-01..2026-04-30 · kinds: purchase,sales,credit_note,debit_note · dry-run: false

[1/6] PLLUM CASA @ http://127.0.0.1:9000
    → purchase batch 1/1 (12 rows, request_id=syncall-...-purchase-1-1, is_final=true)
      ✓ accepted
    purchase: rows=12 sent=1 failed=0
    → sales batch 1/1 (5 rows, request_id=..., is_final=true)
      ✓ accepted
    sales: rows=5 sent=1 failed=0
    credit_note: rows=0 sent=0 failed=0
    debit_note: rows=0 sent=0 failed=0

[2/6] PLLUM LEGNO ... @ http://127.0.0.1:9000
    ...

summary: 6/6 mapping(s) ran · rows=N · batches sent=M failed=0 · fatal errors=0
✓ sync-all complete
```

Cross-check in the GST Reco UI: **Reports → Reconciliation Runs**
(or directly query Supabase):

```sql
select run_id, kind, status, vouchers_ingested, errors_count
from tally_sync_runs
where started_at > now() - interval '1 hour'
order by started_at desc;
```

```sql
select count(*) from purchase_invoices
where source = 'tally_agent'
  and created_at > now() - interval '1 hour';
```

## Step 6 — Repeat for other periods

```bash
# Specific date range
gstreco-tally-agentctl.exe sync-all ^
  --from 2026-04-01 --to 2026-04-15

# One kind only (e.g., re-sync sales after fixing a Tally voucher)
gstreco-tally-agentctl.exe sync-all ^
  --period 042026 --kinds sales

# Dry-run (fetch + parse only — confirm what *would* be sent)
gstreco-tally-agentctl.exe sync-all ^
  --period 042026 --dry-run
```

## Step 7 — Schedule (interim, pre-A5 daemon)

The full daemon (A5) with cron + tray + Windows service is not yet
built. For the pilot, schedule `sync-all` via Windows Task Scheduler:

```powershell
# Daily at 11:00 IST (working hours — Tally is reliably open)
# Pre-v0.1.12 this example used 02:00; changed after the
# 2026-04-29 DIPL incident demonstrated overnight ticks silently
# fail when Tally is closed at the scheduled minute.
schtasks /Create /TN "GST Reco Tally Sync" /TR "C:\path\to\gstreco-tally-agentctl.exe sync-all --period 042026" /SC DAILY /ST 11:00 /RU SYSTEM
```

Note: `--period` baked in once means the task syncs the SAME month
forever. For a real schedule, replace with a wrapper script that
computes the current month. The pilot is a one-shot smoke; A5 ships
the proper scheduler with cron expressions per connection.

## Step 8 — Tear down (after the pilot)

To revoke the agent without deleting historical data:

In the GST Reco UI: **Settings → Tally → Revoke**. The connection's
`status` flips to `revoked`. Future ingest requests return 403; all
previously-ingested rows stay.

To unpair locally on the remote box (delete agent state):

```bash
# Delete config
del "%ProgramData%\GST Reco\agent\config.yaml"
# Delete keyring entries (Windows)
cmdkey /delete:gstreco-tally-agent
```

(The `cmdkey` syntax may vary by the underlying credential manager
provider; `zalando/go-keyring`'s Windows backend uses Generic
Credentials.)

## Failure modes & diagnostics

| Symptom | Likely cause | Fix |
|---|---|---|
| `agentctl discover: no Tally Prime 3.x instances found` | ODBC not enabled OR port outside default range OR Tally not running | Re-verify Step 1; pass `--ports` if needed |
| Discover finds Tally but `version: v4 (parser pending — A7)` | This box is on Tally Prime 4.x | STOP — escalate. Parser_v4 is unbuilt. |
| `sync-all: no active mappings` | Catalog pushed but no mapping decisions made yet | Open the mapping UI (Step 4) and choose a GSTIN per company |
| Per-batch `auth: status=403 connection_not_active` | Connection was revoked or paused | Re-pair via the UI |
| Per-batch `auth: status=401 invalid_signature` | Local clock drift > 5 min | `w32tm /resync` on the remote box |
| Per-batch `auth: status=409 replay_detected` | Same nonce reused (programming bug) | Report — this should never happen in normal use |
| `payload: status=400 bad_body` | Payload validation rejected the batch | Inspect server `tally_ingest_log.error_message` for the row count + reject reason |
| Sync hangs at "fetching ..." | Tally is processing a huge envelope OR Tally crashed | 10-min per-run timeout will fire; check Tally is responsive |

## Where the data ends up

| Server-side table | What lands |
|---|---|
| `tally_connections` | One row per agent install. `last_seen_at` advances on every signed request. |
| `tally_company_mappings` | One row per `(connection, endpoint, tally_company_name)`. Mapping decisions live here. |
| `tally_sync_runs` | One row per `sync-all` mapping × kind tuple. Status, vouchers_ingested, started/finished. |
| `tally_ingest_log` | One row per HTTP request from the agent. Endpoint, batch_kind, http_status, error_message. 90-day retention. |
| `purchase_invoices` (`source='tally_agent'`) | Per-voucher rows from purchase + RCM kinds. Dedup by `(company_id, vendor_gstin, invoice_number_normalized, invoice_date)`. |
| `sales_invoices` (`source='tally_agent'`) | Same shape on the sales side. |

For the pilot, the most useful query is:

```sql
-- Per-mapping ingest summary, last 24h
select
  m.tally_endpoint,
  m.tally_company_name,
  count(*) filter (where r.kind='full' or r.kind='manual') as runs,
  sum(r.vouchers_ingested) as vouchers,
  sum(r.errors_count) as errors,
  max(r.finished_at) as last_run
from tally_sync_runs r
join tally_company_mappings m
  on m.tally_company_name = r.tally_company_name
 and m.connection_id = r.connection_id
where r.started_at > now() - interval '24 hours'
group by m.tally_endpoint, m.tally_company_name
order by m.tally_endpoint, m.tally_company_name;
```

## Next phase (post-pilot)

Once the pilot returns clean (7 days of daily Task Scheduler runs
with no errors that aren't operator-caused), the next push is:

1. **A5 proper** — cron daemon + tray UI + IPC, replacing the
   schtasks workaround.
2. **A6** — Windows service registration so the daemon survives
   reboot without an interactive login.
3. **S8** — sync-status dashboard in the GST Reco UI so the operator
   sees per-mapping run history + errors without SQL.
4. **A8** — self-update so we can ship agent fixes without manual
   re-deploy.
5. **A9** — MSI installer with code-signing (requires the parked
   purchase decision per master plan decision #11).

If the pilot reveals Tally Prime 4.x in the wild: A7 (parser_v4)
jumps the queue.
