//go:build windows

package secretstore

import (
	"fmt"
	"os/exec"
)

func applyEntryACL(path string) error {
	cmd := exec.Command(
		"icacls.exe", path,
		"/inheritance:e",
		"/grant:r", "SYSTEM:F",
		"/grant:r", "BUILTIN\\Administrators:F",
		"/grant:r", "Everyone:F",
		"/C", "/Q",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("secretstore: icacls %s: %w (output: %s)", path, err, string(out))
	}
	return nil
}
