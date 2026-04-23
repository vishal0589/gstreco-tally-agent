# gstreco-tally-agent

Windows agent that polls Tally Prime (3.x+) on the customer's LAN and pushes vouchers / masters to GST Reco over HTTPS. Written in Go, distributed as a single ~10 MB binary, installed as a Windows service with a systray companion.

See `~/GST_Reco/docs/plans/2026-04-tally-agent/master-plan.md` for the full architecture + decisions log (server-side repo, master plan).

## Status

Week 1 bootstrap. A1 scaffolds the Go module, `cmd/` layout, cross-compile Makefile, and GitHub Actions. Real Tally HTTP calls, pairing, HMAC signing, and the SQLite outbox land in A2–A4.

## Layout

```
cmd/
  agent/       long-running service entrypoint
  tray/        systray companion process
  agentctl/    CLI: `pair`, `status`
internal/
  version/     version string injected via -ldflags
  log/         zerolog + lumberjack rotation
```

The full target layout (ingest, pair, tally, scheduler, keyring, service, update) is documented in the master plan and filled in by later PRs.

## Build

```bash
make              # build all three binaries for the host platform → ./bin
make windows      # cross-compile for windows/amd64 → ./dist/windows-amd64
make release      # build all platforms → ./dist
go test ./...
go vet ./...
```

Go 1.24+ required. CI builds on `ubuntu-latest`.

## Signing

V1 ships **unsigned**. Users see a Windows SmartScreen warning on first run and walk through "More info → Run anyway". The server-side pair modal (GST Reco PR #98) renders the walkthrough. Code-signing options (Certum OSS, Microsoft Store MSIX, DigiCert EV) are parked in the master plan's "future purchase" section.

## License

Proprietary. All rights reserved. Not for redistribution.
