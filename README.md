# gstreco-tally-agent

Windows agent that polls Tally Prime (3.x+) on the customer's LAN and pushes vouchers / masters to GST Reco over HTTPS. Written in Go, distributed as a single ~10 MB binary, installed as a Windows service with a systray companion.

See `~/GST_Reco/docs/plans/2026-04-tally-agent/master-plan.md` for the full architecture + decisions log (server-side repo, master plan).

## Status

Phase A + Phase B + v0.1.2 shipped. Pilot install on PLLUM uncovered four pre-existing install-path bugs (TLS 1.2 not forced; `2>$null` not suppressing native errors under `ErrorActionPreference=Stop`; `$env:ProgramFiles` resolving to `(x86)` under 32-bit PowerShell; Windows Credential Manager scoped per-user so `LocalSystem`-running service couldn't read what `Administrator`-running pair wrote). v0.1.2 fixes all four:

- New `internal/secretstore` package replaces the per-user OS keyring with a file-based AES-GCM store at `%ProgramData%\GST Reco\agent\secrets\`, key derived via HKDF-SHA256 from the machine ID. Cross-user accessible by design, so the service-as-LocalSystem reads what pair-as-Administrator wrote. ACL'd to SYSTEM + Administrators on first write via `icacls`.
- `internal/autodiscover` now honours `cfg.TallyEndpoints` first (set by `agentctl discover --ports` or by the install-time `GSTRECO_TALLY_PORTS` env var), falls back to the default port range only when the config is empty.
- Default discovery range widened from 9000–9009 to 9000–9020 plus alternates `2026, 8989` to cover the common non-default ports we've seen in pilot installs.
- `install.ps1` (in the GST_Reco repo) prepends TLS 1.2, uses `Get-Service` existence check before stop/uninstall, hardcodes the install dir to `C:\Program Files\GST Reco Agent`, and accepts `GSTRECO_TALLY_PORTS` for non-default-port customers.

The pair modal in `/settings/tally` now has an optional "Tally port(s)" field that bakes the value into the install one-liner.

A fresh customer install: paste one PowerShell line, type your Tally port (or leave blank), wait ~60s. Agent finds the companies, populates the mapping UI, the operator maps GSTINs to companies in the web app, scheduled syncs run.

**v0.1.3 — Tally Prime 6.x discovery hardening.** The PLLUM pilot install on Tally Prime 6.1 surfaced three protocol assumptions baked into v0.1.2's discovery path that only held for Tally Prime 3.x:

- **Company-list `<TYPE>Data</TYPE>` rejected.** Tally Prime 6.x is strict — built-in reports like "List of Companies" must be requested with `<TYPE>Collection</TYPE>` (cross-checked against the Manual2AI Python adapter, which has shipped against the same Tally Prime 6.1 install since 2026-04-19).
- **`$$Version` Function probe doesn't answer reliably.** Tally Prime 6.x returns `STATUS=0 + DESC not found` for the `<TYPE>Function</TYPE><ID>$$Version</ID>` shape. v0.1.3 trusts the company-list response as the authoritative "is this Tally" signal and treats the version probe as best-effort telemetry.
- **V3-only filter dropped Tally Prime 4.x+.** The HTTP/XML protocol for the queries this agent issues is identical across Tally Prime 3.x/4.x/5.x/6.x. v0.1.3 keeps every probe result that responds with a parseable Tally envelope.

Voucher fetch + ingest hardening (UTF-16LE response decoding, inline-TDL voucher collections, strict-OK ingest predicate, voucher-type-name configurability) is queued for v0.1.4. v0.1.3 unblocks discovery; v0.1.4 will harden the actual sync path.

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
