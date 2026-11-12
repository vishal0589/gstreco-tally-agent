//go:build !windows

package main

import "context"

func observePlatformService(ctx context.Context, run func(context.Context, string, ...string) (commandResult, error), path string) (serviceState, error) {
	return queryServiceState(ctx, run, path)
}
