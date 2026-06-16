//go:build !windows

package secretstore

import "os"

func applyEntryACL(path string) error {
	return os.Chmod(path, 0o600)
}
