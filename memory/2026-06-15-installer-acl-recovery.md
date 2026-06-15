# 2026-06-15 Installer ACL Recovery

## Symptom

On the Windows Tally host, old paired companies kept working, but newly paired
companies stayed offline in GST Reco. Running the v0.1.38 installer failed with:

`Could not inspect local pairing state: config: read C:\ProgramData\GST Reco\agent\config.yaml: Access is denied`

## Root Cause

There were two related ACL bugs:

1. `secretstore.Set` wrote secrets through a temp file and only applied the
   directory ACL before rename, so some fresh `.bin` files could be readable by
   the interactive pairing user but unreadable by the LocalSystem service.
   Fixed in v0.1.38.
2. The installer read `config.yaml` before doing any local ACL repair. On a host
   already stuck in a broken ACL state, the installer exited before it could
   update binaries, restart the service, or repair permissions. Fixed in
   v0.1.39.

## Fix

v0.1.39 adds a Windows-only installer preflight permission repair for the local
GST Reco agent directory before reading pair state. If pair-state inspection
still fails, it runs the repair again and retries once.

The repair uses `icacls` to grant SYSTEM and BUILTIN\Administrators full
control recursively, with a `takeown` fallback before retrying `icacls`.

## Evidence

- Added regression test:
  `TestRunInstaller_RepairsACLAndRetriesLocalPairState`
- Test proves the screenshot path: first `loadLocalPair` returns Access denied,
  installer repairs ACL, retries, then continues into service repair/update.
- `go test ./...` passed after the fix.
- GitHub release v0.1.39 published with Windows installer/agent/agentctl assets.
- Production `https://gstreco.m2ai.ai/api/tally/agent/version` now serves
  `latest=0.1.39` with the v0.1.39 installer URL and SHA.

## Status

DONE_WITH_CONCERNS: code and production manifest are fixed. The affected Windows
host still needs to run the v0.1.39 installer/recovery command, then discovery
and May syncs should be rechecked live.

## Follow-up Same Day

The affected Windows host still could not read `config.yaml` after v0.1.39
folder repair. New evidence showed the problem was the exact `config.yaml` file
remaining unreadable after recursive folder ACL repair. v0.1.40 therefore:

- repairs exact file targets (`config.yaml`) in addition to the root folder,
  secrets directory, and logs directory;
- treats `icacls` output containing `Failed processing N files` as a real
  failure even when the process exits 0;
- applies ACL and read-verifies after every `config.Save` temp-file rename.

Production now serves agent/installer manifest `latest=0.1.40`; downloaded
installer SHA256 through `https://gstreco.m2ai.ai/api/tally/installer/download`
verified as:

`58e44f81b888893480897397f4e78056d96825f4ecdd41377425ef5e1bf35773`

For the already-broken host, direct recovery is to rebuild the non-secret
`config.yaml` from known connection IDs/endpoints, repair ProgramData ACL, then
restart/discover. Avoid generating new pair codes unless the existing secret
files prove absent after this config rebuild.
