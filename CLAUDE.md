# CLAUDE.md — gstreco-tally-agent

Instructions for AI sessions working in this repo.

## What this is

The Windows side of GST Reco's Tally sync. The server side lives at `~/GST_Reco`. The full plan is at `~/GST_Reco/docs/plans/2026-04-tally-agent/master-plan.md` — read it before changing architecture.

## Workflow

- Target branch: **`dev`** (mirrors GST_Reco convention). Promote dev → main via a release PR once the cluster is ready.
- One PR per agent ticket (A1, A2a, A2b, …). Never bundle.
- Never self-merge. `gh pr create` then stop — user reviews and merges.
- Update this file, the README, and the master plan's ship log **in the same PR** as the code change. Docs must not drift from code.

## Toolchain

- Go 1.24+ (currently 1.26.2 via Homebrew on dev machine).
- No external Go deps beyond what the plan lists: `zerolog`, `lumberjack`, `kardianos/service`, `zalando/go-keyring`, `getlantern/systray`, `modernc.org/sqlite`, `robfig/cron`. Justify any additional dep in the PR body.
- CI: `.github/workflows/ci.yml` runs `go vet ./...` and `go test ./...` on every PR.

## Build

`make` for host, `make windows` for cross-compile. Cross-compilation must stay CGO-free (`CGO_ENABLED=0`) so GH Actions can build Windows binaries from a Linux runner.

## Testing

- Unit tests live next to code (`foo.go` + `foo_test.go`).
- Tally XML fixtures go under `fixtures/` and are committed (small, anonymized). Real customer captures go under `fixtures/local/` which is gitignored.
- Integration tests against a real Tally instance go under `tests/integration/` and are skipped in CI (build tag `integration`).

## Security

- Agent secret (HMAC key) lives in the OS keyring, never on disk, never in logs.
- Config file (`config.yaml` at `%ProgramData%\GST Reco\agent\`) holds non-secret connection metadata only. It may store more than one paired connection on shared Tally machines, but never stores bearer tokens or HMAC secrets.
- All outbound HTTP requests to the server are HMAC-signed per the scheme in the master plan's "HMAC scheme" section.
- Logs rotate via lumberjack, 10 MB × 5 files, stored under `%ProgramData%\GST Reco\agent\logs\`.

## Per-connection state

One agent process serves every paired connection on the machine. Anything a connection reports to the server (heartbeat `connection_status`, its sync result) must come from that connection's own `daemonConnectionRuntime` (`cmd/agent/connection_status.go`), never from shared state. The agent-wide `tickState` only feeds the legacy `last_tick_status` field.

## Schedule

The daemon's cron spec is `--schedule`, else `config.yaml` `schedule_cron`, else `scheduler.DefaultSpec`; the server's `schedule_cron` is not applied. Each heartbeat reports what is in use as `schedule: {cron, source}` (`config` / `default` / `override`, `cmd/agent/main.go` `scheduleSource`). If the selection ever changes, change `scheduleSource` with it.

## Server coordination

Every agent PR that changes the wire protocol must reference the paired server PR (or note that the server already exposes the endpoint). Breaking wire changes need a server migration first.
