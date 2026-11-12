//go:build windows

package main

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func exactSCMState(raw svc.State) serviceState {
	switch raw {
	case svc.Running:
		return serviceRunning
	case svc.Stopped:
		return serviceStopped
	case svc.StartPending, svc.StopPending, svc.PausePending, svc.ContinuePending:
		return servicePending
	case svc.Paused:
		return servicePaused
	default:
		return serviceUnknown
	}
}
func observeNamedWindowsService(ctx context.Context, name string) (serviceState, error) {
	if err := ctx.Err(); err != nil {
		return serviceUnknown, err
	}
	manager, err := mgr.Connect()
	if err != nil {
		return serviceUnknown, err
	}
	defer manager.Disconnect()
	service, err := manager.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return serviceNotInstalled, nil
	}
	if err != nil {
		return serviceUnknown, err
	}
	defer service.Close()
	status, err := service.Query()
	if err != nil {
		return serviceUnknown, err
	}
	return exactSCMState(status.State), nil
}
func observePlatformService(ctx context.Context, _ func(context.Context, string, ...string) (commandResult, error), _ string) (serviceState, error) {
	return observeNamedWindowsService(ctx, "gstreco-tally-agent")
}
