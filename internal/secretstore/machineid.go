package secretstore

import (
	"errors"
	"strings"
)

// ReadMachineID returns a stable per-host identifier. Each platform has
// its own backend (see machineid_*.go); this top-level dispatcher exists
// so callers can use the package without caring about build tags.
//
// The returned identifier is treated as confidential — never log it,
// never include it in error messages — but it is not a cryptographic
// secret. Any local user can derive it. We rely on it as a salt-like
// input to HKDF, not as the secret itself.
func ReadMachineID() (string, error) {
	id, err := readPlatformMachineID()
	if err != nil {
		return "", err
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return "", errors.New("secretstore: platform returned empty machine id")
	}
	// Lowercase for consistency. Some platforms return mixed case
	// (Windows MachineGuid is hex with mixed case); we normalise so
	// HKDF output is identical regardless of how the registry/file
	// stores it.
	return strings.ToLower(id), nil
}
