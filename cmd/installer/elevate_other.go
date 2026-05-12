//go:build !windows

package main

import "fmt"

func isAdministrator() (bool, error) {
	return true, nil
}

func relaunchSelfAsAdmin(_ []string) error {
	return fmt.Errorf("administrator relaunch is only supported on Windows")
}
