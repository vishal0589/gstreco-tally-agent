// Package secretstore stores agent secrets (HMAC key, bearer token) in a
// file under %ProgramData%\GST Reco\agent\secrets\ on Windows, or
// ~/.gstreco-agent/secrets/ on Unix, encrypted with AES-256-GCM.
//
// Why not the OS keyring (Windows Credential Manager / macOS Keychain /
// Linux Secret Service)?
//
// Per-user scope. The Windows installer pairs the agent under whichever
// account ran `agentctl pair` (typically the operator's logged-in user).
// The Windows service registers as LocalSystem by default. Different
// users → different Credential Manager scopes → service can't read the
// secret the operator just wrote. We hit this on the first pilot install
// and worked around it by re-pairing under LocalSystem via PsExec.
// secretstore makes that workaround unnecessary by writing to a file
// scope that is shared between all admin-level accounts on the machine.
//
// Key derivation. The AES key is derived via HKDF-SHA256 from:
//
//   - the machine ID (Windows registry HKLM\SOFTWARE\Microsoft\Cryptography\
//     MachineGuid, Linux /etc/machine-id, macOS IOPlatformUUID),
//   - a binary-baked salt constant,
//   - the info string "gstreco-tally-agent/v1/secret-key".
//
// The machine ID is local to the box; an attacker who exfiltrates only the
// secrets directory (e.g. a stolen backup) cannot derive the key without
// also knowing the machine ID of the original system. The auth tag in
// AES-GCM detects tampering.
//
// Threat model.
//
//   - Same-box attacker with admin rights: WINS. They can read the secrets
//     file AND derive the machine ID. There is no defence here that the
//     agent process itself can survive — anyone with admin can simply
//     impersonate the service. (Hardware HSM / TPM-bound secrets would
//     mitigate; out of scope for V1.)
//   - Same-box attacker without admin rights: blocked by the secrets
//     directory ACL (SYSTEM + Administrators only) on Windows, or 0o700
//     on Unix. We set the ACL on first write via the platform-specific
//     applyACL function.
//   - Off-box attacker with the secrets file but not the machine ID:
//     blocked by AES-GCM. Without the machine ID they cannot derive the
//     key, and brute-forcing 32 bytes of HKDF output is infeasible.
//   - Off-box attacker with the file AND the machine ID: WINS. This is
//     the realistic worry from a stolen developer laptop; the install
//     should never include the machine ID in any backup. (Same caveat as
//     DPAPI-Machine, which also fails this threat — Microsoft documents
//     it.)
//
// Atomic writes. Set() writes to a sibling .tmp file then renames into
// place so a power loss mid-write can't leave a half-written ciphertext
// that fails to decrypt forever after.
//
// API compatibility. The package exports the same Get/Set/Delete surface
// as internal/keyring.Store, so callers can swap implementations with a
// one-line change. The keyring package's MemoryStore stays in tests.
package secretstore
