// Package winacl applies Windows DACLs natively via the Win32 security
// APIs (SetNamedSecurityInfo) instead of shelling out to icacls.exe.
//
// The subprocess approach broke in the field two ways:
//
//  1. Endpoint-protection filter drivers commonly allow icacls.exe from an
//     interactive shell but suspend it indefinitely when spawned by an
//     unknown unsigned binary — the agent's pair flow then hangs forever
//     inside exec.Command with no error and no timeout (observed on a
//     pilot machine where every `agentctl pair` wedged at the first
//     icacls child while the same command ran instantly from PowerShell).
//  2. icacls account-name arguments like "BUILTIN\Administrators" are
//     locale-dependent and do not exist under that name on non-English
//     Windows; SDDL well-known SID aliases (SY, BA, WD) are not.
//
// Only the _windows.go file contains real logic; this doc file keeps the
// package buildable on non-Windows hosts (nothing imports it there).
package winacl
