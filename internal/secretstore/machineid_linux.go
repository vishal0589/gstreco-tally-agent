//go:build linux

package secretstore

import (
	"errors"
	"fmt"
	"os"
)

// readPlatformMachineID reads /etc/machine-id (systemd-managed, populated
// at first boot, stable for the life of the install). Falls back to
// /var/lib/dbus/machine-id which dbus uses; on most modern distros these
// two are symlinks to the same file, but we check both for resilience
// against minimalist container images.
func readPlatformMachineID() (string, error) {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		b, err := os.ReadFile(path)
		if err == nil {
			return string(b), nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("secretstore: read %s: %w", path, err)
		}
	}
	return "", errors.New("secretstore: no machine-id file found (looked at /etc/machine-id and /var/lib/dbus/machine-id)")
}
