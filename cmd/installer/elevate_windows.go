//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func isAdministrator() (bool, error) {
	return windows.GetCurrentProcessToken().IsElevated(), nil
}

func relaunchSelfAsAdmin(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	return windows.ShellExecute(
		0,
		windows.StringToUTF16Ptr("runas"),
		windows.StringToUTF16Ptr(exe),
		windows.StringToUTF16Ptr(windows.ComposeCommandLine(args)),
		windows.StringToUTF16Ptr(cwd),
		windows.SW_NORMAL,
	)
}
