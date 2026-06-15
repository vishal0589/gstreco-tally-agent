//go:build windows

package config

import (
	"fmt"
	"os/exec"
)

func applyConfigDirACL(dir string) error {
	return runICACLS(
		dir,
		"/inheritance:r",
		"/grant:r", "SYSTEM:(OI)(CI)F",
		"/grant:r", "BUILTIN\\Administrators:(OI)(CI)F",
		"/T", "/C",
	)
}

func applyConfigFileACL(path string) error {
	return runICACLS(
		path,
		"/inheritance:r",
		"/grant:r", "SYSTEM:F",
		"/grant:r", "BUILTIN\\Administrators:F",
		"/C",
	)
}

func runICACLS(target string, args ...string) error {
	cmdArgs := append([]string{target}, args...)
	out, err := exec.Command("icacls.exe", cmdArgs...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("config: icacls %s: %w (output: %s)", target, err, string(out))
	}
	if icaclsReportedFailures(string(out)) {
		return fmt.Errorf("config: icacls %s reported failed file processing (output: %s)", target, string(out))
	}
	return nil
}
