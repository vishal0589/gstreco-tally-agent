//go:build windows

package secretstore

import (
	"github.com/vishal0589/gstreco-tally-agent/internal/winacl"
)

// applyEntryACL keeps parent inheritance enabled and grants SYSTEM,
// Administrators and Everyone full access to one entry file. Everyone is
// intentional: entries are AES-GCM sealed (the encryption, not the DACL,
// is the confidentiality boundary) and cross-user access lets the
// LocalSystem service and the operator's elevated shell share one store.
//
// v0.1.43: native SetNamedSecurityInfo instead of an icacls.exe child —
// see acl_windows.go for why the subprocess had to go.
func applyEntryACL(path string) error {
	return winacl.ApplyInheritedEntryFile(path)
}
