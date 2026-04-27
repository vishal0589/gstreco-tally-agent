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

**v0.1.10 — secretstore: applyDirACL fatal-on-fail + write-verify roundtrip.** DIPL Delhi pilot (2026-04-27) hit the silent-failure variant of `applyDirACL`: the directory-level icacls call failed (probably AV / GPO interference), Set() warned-and-continued, and the file ended up in a directory whose DACL the LocalSystem service couldn't traverse. The agent then 401-looped on every heartbeat with `read hmac secret from keyring failed: Access is denied`, taking 5+ hours to diagnose. v0.1.10 fixes this end-to-end:

- **Fatal**: `applyDirACL` failure now returns from `Set()` instead of warning to stderr. Pair surfaces the error immediately.
- **Write-verify roundtrip**: after `Set()` writes + renames the encrypted file, it does a `getLocked()` round-trip read-back to confirm the resulting file is actually readable + decryptable. Catches EFS / AV / restrictive-DACL cases that cipher succeeds but read fails.
- **Test seam**: `applyDirACLFn` is now a package-level variable so tests can simulate icacls failure deterministically (cross-platform).

Plus `install.ps1` (in GST_Reco repo, separate PR) appends a belt-and-suspenders `icacls /reset /T /grant:r Everyone:(OI)(CI)F /T` on the secrets dir so any prior broken DACL is corrected at install time.

**v0.1.10 — Tally Prime 6.x master-field rename catch-up.** DIPL Delhi pilot (2026-04-27) surfaced 0 % coverage on `trade_name`, `state_code`, `email` despite real GSTINs flowing — the smoking gun that Tally Prime 6.x renamed master-ledger XML elements (the v3 `EMAIL` is now `LEDGEREMAIL`; `STATENAME` is now `LEDSTATENAME`; `LEDGERMOBILE` is now `LEDGERMOBILENO` on some patches). The FETCH list in `internal/tally/master_envelope.go` and the `xmlLedger` struct in `internal/tally/master_parser.go` now ask for + accept all known variants and pick the populated one via the existing `firstNonEmpty` helper. After re-syncing masters on v0.1.10, vendor / customer master coverage on Prime 6.x ledgers jumps from <5 % to ~95 % — vendor portal lists show real names and emails instead of raw GSTINs.

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
