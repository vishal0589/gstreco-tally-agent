//go:build windows

package secretstore

import (
	"fmt"

	"golang.org/x/sys/windows/registry"
)

// readPlatformMachineID reads the MachineGuid from
// HKLM\SOFTWARE\Microsoft\Cryptography. This GUID is generated at OS
// install time, persists across reboots, survives most software changes
// short of an OS reinstall. Same value across all users on the box —
// which is exactly what we need for our cross-user secret store.
//
// Open with KEY_READ + WOW64_64KEY to avoid the WOW6432Node redirection
// that bites 32-bit processes on 64-bit Windows; the MachineGuid lives
// in the 64-bit hive regardless of the agent binary's bitness.
func readPlatformMachineID() (string, error) {
	const path = `SOFTWARE\Microsoft\Cryptography`
	k, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		path,
		registry.QUERY_VALUE|registry.WOW64_64KEY,
	)
	if err != nil {
		return "", fmt.Errorf("secretstore: open registry %s: %w", path, err)
	}
	defer k.Close()

	v, _, err := k.GetStringValue("MachineGuid")
	if err != nil {
		return "", fmt.Errorf("secretstore: read MachineGuid: %w", err)
	}
	return v, nil
}
