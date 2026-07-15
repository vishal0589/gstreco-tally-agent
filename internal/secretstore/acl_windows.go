//go:build windows

package secretstore

import (
	"github.com/vishal0589/gstreco-tally-agent/internal/winacl"
)

// applyDirACL tightens the secrets directory's ACL so non-admin users
// on the same box cannot list or read entries.
//
// Final ACL after this runs:
//   - SYSTEM            : Full control
//   - Administrators    : Full control
//   - Authenticated Users: REMOVED
//   - Users              : REMOVED
//
// v0.1.43: applied natively via SetNamedSecurityInfo instead of spawning
// icacls.exe. Endpoint-protection filter drivers on pilot machines
// suspended the icacls child indefinitely when spawned from the agent
// (while the identical command ran instantly from an operator shell),
// wedging every pair attempt with no error; the native call removes the
// subprocess entirely and is also locale-independent (SDDL SID aliases
// instead of the English-only "BUILTIN\Administrators" account name).
func applyDirACL(dir string) error {
	return winacl.ApplyProtectedDirTree(dir)
}
