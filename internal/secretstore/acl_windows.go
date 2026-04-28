//go:build windows

package secretstore

import (
	"fmt"
	"os/exec"
)

// applyDirACL tightens the secrets directory's ACL so non-admin users
// on the same box cannot list or read entries. Uses icacls.exe (built
// into Windows) rather than the Win32 SetSecurityInfo API because it's
// a one-shot during first write — readability + portability across
// Windows Server SKUs trumps avoiding a fork/exec.
//
// Final ACL after this runs:
//   - SYSTEM            : Full control (inherited from parent)
//   - Administrators    : Full control (inherited from parent)
//   - Authenticated Users: REMOVED
//   - Users              : REMOVED
//
// /inheritance:r removes inherited permissions; we then re-grant SYSTEM
// + BUILTIN\Administrators explicitly so the service (LocalSystem) and
// the operator's elevated PowerShell (Administrator) both have access.
func applyDirACL(dir string) error {
	cmd := exec.Command(
		"icacls.exe", dir,
		"/inheritance:r",
		"/grant:r", "SYSTEM:(OI)(CI)F",
		"/grant:r", "BUILTIN\\Administrators:(OI)(CI)F",
		"/T", "/C", "/Q",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("secretstore: icacls %s: %w (output: %s)", dir, err, string(out))
	}
	return nil
}
