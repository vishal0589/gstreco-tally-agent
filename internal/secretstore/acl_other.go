//go:build !windows

package secretstore

import (
	"fmt"
	"os"
)

// applyDirACL ensures the directory mode is 0700. On Unix, MkdirAll
// already creates with the requested mode if the dir doesn't exist —
// this only matters when we're tightening an existing dir that was
// created with a looser mode. Idempotent.
func applyDirACL(dir string) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secretstore: chmod %s: %w", dir, err)
	}
	return nil
}
