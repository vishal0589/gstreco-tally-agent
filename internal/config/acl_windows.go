//go:build windows

package config

import (
	"github.com/vishal0589/gstreco-tally-agent/internal/winacl"
)

// v0.1.43: both config ACL helpers now apply DACLs natively via
// SetNamedSecurityInfo (internal/winacl) instead of spawning icacls.exe.
// The icacls child process wedged indefinitely under endpoint-protection
// filter drivers on pilot machines — pair flows hung forever inside
// config.Save with no error — and its "BUILTIN\Administrators" account
// name argument was English-locale-only. SDDL SID aliases fix both.

func applyConfigDirACL(dir string) error {
	return winacl.ApplyProtectedDirTree(dir)
}

func applyConfigFileACL(path string) error {
	return winacl.ApplyProtectedFile(path)
}
