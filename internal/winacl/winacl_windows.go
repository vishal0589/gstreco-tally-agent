//go:build windows

package winacl

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// SDDL strings use well-known SID aliases (SY = LocalSystem, BA =
// BUILTIN\Administrators, WD = Everyone), so they are locale-independent —
// unlike the icacls.exe account-name arguments they replace. FA is
// FILE_ALL_ACCESS; OICI marks the ACE inheritable by child files and
// directories; the "P" control flag protects the DACL (severs inheritance
// from the parent — the equivalent of `icacls /inheritance:r`).
const (
	protectedDirSDDL   = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	protectedFileSDDL  = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	inheritedEntrySDDL = "D:(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;WD)"
)

func setDACL(path, sddl string, flags windows.SECURITY_INFORMATION) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return fmt.Errorf("winacl: parse sddl %q: %w", sddl, err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return fmt.Errorf("winacl: dacl from sddl %q: %w", sddl, err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|flags,
		nil, nil, dacl, nil,
	); err != nil {
		return fmt.Errorf("winacl: set dacl on %s: %w", path, err)
	}
	return nil
}

// ApplyProtectedDirTree is the native equivalent of
//
//	icacls <dir> /inheritance:r /grant:r SYSTEM:(OI)(CI)F
//	    /grant:r BUILTIN\Administrators:(OI)(CI)F /T
//
// The directory gets a protected DACL granting SYSTEM + Administrators
// full control with inheritable ACEs, and every existing child is
// re-stamped to match (new children inherit automatically). The walk is
// intentionally fail-fast: a child we cannot re-stamp leaves the tree in
// a partially-tightened state the caller must treat as fatal, exactly as
// the icacls exit-code path did.
func ApplyProtectedDirTree(dir string) error {
	if err := setDACL(dir, protectedDirSDDL, windows.PROTECTED_DACL_SECURITY_INFORMATION); err != nil {
		return err
	}
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		if d.IsDir() {
			return setDACL(path, protectedDirSDDL, windows.PROTECTED_DACL_SECURITY_INFORMATION)
		}
		return setDACL(path, protectedFileSDDL, windows.PROTECTED_DACL_SECURITY_INFORMATION)
	})
}

// ApplyProtectedFile is the native equivalent of
//
//	icacls <file> /inheritance:r /grant:r SYSTEM:F
//	    /grant:r BUILTIN\Administrators:F
func ApplyProtectedFile(path string) error {
	return setDACL(path, protectedFileSDDL, windows.PROTECTED_DACL_SECURITY_INFORMATION)
}

// ApplyInheritedEntryFile is the native equivalent of
//
//	icacls <file> /inheritance:e /grant:r SYSTEM:F
//	    /grant:r BUILTIN\Administrators:F /grant:r Everyone:F
//
// Parent inheritance stays enabled (UNPROTECTED re-enables it if a prior
// run severed it) and the explicit grants are replaced. Everyone keeps
// full access to entry files by design: entries are AES-GCM sealed and
// the encryption — not the DACL — is the confidentiality boundary, while
// cross-user access lets the LocalSystem service and the operator's
// elevated shell share one store.
func ApplyInheritedEntryFile(path string) error {
	return setDACL(path, inheritedEntrySDDL, windows.UNPROTECTED_DACL_SECURITY_INFORMATION)
}
