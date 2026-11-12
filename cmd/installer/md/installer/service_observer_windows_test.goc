//go:build windows

package main

import (
	"golang.org/x/sys/windows/svc"
	"testing"
)

func TestExactSCMStatesNeverCollapsePending(t *testing.T) {
	for _, s := range []svc.State{svc.StartPending, svc.StopPending, svc.PausePending, svc.ContinuePending, svc.Paused, svc.State(999)} {
		state := exactSCMState(s)
		if state == serviceRunning || state == serviceStopped {
			t.Fatalf("SCM %d falsely final: %s", s, state)
		}
	}
	if exactSCMState(svc.Running) != serviceRunning || exactSCMState(svc.Stopped) != serviceStopped {
		t.Fatal("wrong final mapping")
	}
}
