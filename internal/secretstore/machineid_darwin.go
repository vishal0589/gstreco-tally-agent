//go:build darwin

package secretstore

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
)

// readPlatformMachineID asks ioreg for the IOPlatformUUID. Stable for
// the life of the hardware (changes only on logic-board replacement
// per Apple's docs). Used by Apple itself for system-level licensing
// fingerprints, so it's the closest macOS equivalent to Windows'
// MachineGuid.
func readPlatformMachineID() (string, error) {
	out, err := exec.Command(
		"ioreg", "-rd1", "-c", "IOPlatformExpertDevice",
	).Output()
	if err != nil {
		return "", fmt.Errorf("secretstore: ioreg: %w", err)
	}
	// Output line we care about looks like:
	//   "IOPlatformUUID" = "12345678-ABCD-..."
	re := regexp.MustCompile(`(?m)^\s*"IOPlatformUUID"\s*=\s*"([^"]+)"`)
	m := re.FindSubmatch(bytes.TrimSpace(out))
	if len(m) < 2 {
		return "", fmt.Errorf("secretstore: IOPlatformUUID not found in ioreg output")
	}
	return string(m[1]), nil
}
