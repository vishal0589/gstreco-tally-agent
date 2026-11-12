# Existing-service installer transaction

The installer stages and checksums both new binaries before stopping the shared
service. It keeps complete copies of both old binaries in a uniquely named
`.gstreco-backup-*` directory before any service mutation. A missing original
binary fails closed. Existing service registration is never uninstalled as a
repair fallback.

Success deliberately starts the service even when it was stopped before the
installer ran. This activates the paired agent. Recovery, in contrast, restores
the original running or stopped state. There is no silent success with a stopped
agent after pairing.

On replacement, startup, or health failure, recovery has an independent
three-minute context, unaffected by cancellation of the installer request. The
service must be confirmed stopped before restoring binaries. Each binary restore
is attempted even if another restore fails. Errors are collected, and the service
is not restarted if the final restore set is incomplete. The full backup path is
included in the failure report for manual repair. Both originals are retained.

Only after successful health verification, cleanup retains this transaction's
backup plus the two newest older backup directories. Failed transactions do not
prune backups. Cleanup failure is a visible warning, not a service-health failure.
A later successful install may prune older failed-transaction backups. The fixed
retention limit is three backup sets. Unrelated directories are not touched.

Health means three exact native SCM Running observations over 500 ms, not a heartbeat or Tally
readiness check. This is not crash/power-loss recovery, and there is no cross-
installer lock. Fresh-install rollback remains unchanged. Config and pairing
credentials written before the binary transaction are not undone by rollback.
Existing and newly paired connections therefore remain in config after rollback.

The .bak used during one rename is separate from durable rollback backups.
Failure to remove a stale .bak fails closed before replacing the destination;
cleanup errors are no longer discarded.

## Independent controller and executable preflight

Before inspecting or stopping the service, the staged daemon must successfully
execute `--version` under a ten-second context. Its executable staging filename
ends in .exe. A failed probe leaves the installed files and service untouched.

After both old files are preserved, the installer executes the preserved old
daemon to confirm its status matches the original state. That saved executable
is the recovery controller for status, stop, start, and verification, independent
of either replaced installed binary. It addresses the same fixed service name
and never installs/uninstalls or changes the registered executable path. Normal
new-service startup still uses the new daemon; failure there invokes recovery
through the preserved old controller. Preflight is additional protection: tests
also invalidate the installed new daemon after preflight, proving recovery does
not require the new executable to run. Windows regression uses an isolated
synthetic SCM service only; its new exact-candidate native run remains required.


## Exact Windows SCM observation

Windows status checks query the named service directly through the Windows SCM
API, independent of new and preserved daemon output. Only exact Running and
Stopped count as final states. StartPending, StopPending, PausePending,
ContinuePending, Paused and unknown values cannot pass health or stop gates.
Before replacement and recovery, stop confirmation waits up to 45 seconds
(within the caller/recovery context) for exact Stopped. A timeout leaves backups
in place and refuses replacement or restoration while a service may still run.
Health remains fail-closed: pending startup fails health and invokes recovery,
not a false successful installation. Initial nonfinal states are rejected before
backup or service mutations. The preserved executable is used for service
control only; status truth comes directly from SCM on Windows.
