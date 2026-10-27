//go:build !windows

package autoupdate

import "os/exec"

// launchDetached starts the installer. Consider only launches on Windows;
// this keeps the package building elsewhere.
func launchDetached(path string, args []string) error {
	cmd := exec.Command(path, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
