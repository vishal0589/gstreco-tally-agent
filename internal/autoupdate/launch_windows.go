//go:build windows

package autoupdate

import (
	"os/exec"
	"syscall"
)

const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
)

// launchDetached starts the installer outside the daemon's console, process
// group and (where the service's job allows it) job, so it lives on when the
// installer restarts the service it was started from.
func launchDetached(path string, args []string) error {
	start := func(flags uint32) error {
		cmd := exec.Command(path, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: flags}
		if err := cmd.Start(); err != nil {
			return err
		}
		return cmd.Process.Release()
	}
	if err := start(detachedProcess | createNewProcessGroup | createBreakawayFromJob); err == nil {
		return nil
	}
	// A job that forbids breakaway refuses the flag; services are not
	// normally in a kill-on-close job, so start without it.
	return start(detachedProcess | createNewProcessGroup)
}
