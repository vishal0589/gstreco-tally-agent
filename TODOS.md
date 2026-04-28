# TODOS — gstreco-tally-agent

Tracked follow-up work. Items here are P2 (nice-to-have, not shipping blockers).

## Open

### DSN URL-encoding for Windows paths
**Where:** `internal/ingest/outbox.go:49` — `dsn := "file:" + path + "?_pragma=..."`
**Issue:** Path is concatenated raw into the SQLite URI DSN. Windows config-dir paths like `C:\ProgramData\GST Reco\agent\outbox.db` contain spaces + backslashes that SQLite URI parsing may or may not handle cleanly.
**Why P2:** modernc.org/sqlite likely accepts raw-path mode; fix without verification risks breaking darwin/linux paths that currently work. No reproduction on dev hardware.
**Fix plan:** During first Windows smoke test (when the runbook runs end-to-end on a real user's machine), verify outbox opens cleanly at the Windows path. If it fails, switch to `url.QueryEscape` on the path portion or use the raw-path form.
**Surfaced in:** `/review all`, 2026-04-23.

### Notify server on failed pair rollback
**Where:** `internal/pair/pair.go` — Claim() rollback paths
**Issue:** When keyring or config save fails after a successful `/api/tally/pair/claim`, the agent deletes its local state but leaves an orphan `tally_connections` row on the server. The connection can't be used (agent has no secret) but takes up a row and clutters the user's Settings → Tally list.
**Why P2:** Not a data-integrity bug — just a UX wart. User can revoke manually.
**Fix plan:** Cleanest is server-side: GC unauthenticated connections after a TTL (e.g. 24h without a single signed request). Alternative: add `DELETE /api/tally/connections/:id` with a first-request-token auth and call it from the agent's rollback path. Server-side GC is simpler; track in server repo under a GST_Reco S10 or S11 follow-up.
**Surfaced in:** `/review all`, 2026-04-23.

## Resolved

_None yet._
