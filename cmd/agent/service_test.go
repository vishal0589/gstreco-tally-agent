package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunServiceControl_NoArgsExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServiceControl(&stdout, &stderr, []string{})
	if code != 2 {
		t.Errorf("exit=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Errorf("stderr should print usage on no-args, got: %s", stderr.String())
	}
}

func TestRunServiceControl_HelpExits0(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		var stdout, stderr bytes.Buffer
		code := runServiceControl(&stdout, &stderr, []string{arg})
		if code != 0 {
			t.Errorf("%q: exit=%d, want 0", arg, code)
		}
		if !strings.Contains(stdout.String(), "install") {
			t.Errorf("%q: help output missing 'install': %s", arg, stdout.String())
		}
	}
}

func TestRunServiceControl_UnknownActionExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := runServiceControl(&stdout, &stderr, []string{"banana"})
	if code != 2 {
		t.Errorf("exit=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown action") {
		t.Errorf("stderr should mention unknown action, got: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "install") {
		t.Errorf("stderr should suggest valid actions, got: %s", stderr.String())
	}
}

func TestServiceConfig_PreservesArgs(t *testing.T) {
	cfg := serviceConfig([]string{"--config", "/path/to/cfg.yaml"})
	if cfg.Name != serviceName {
		t.Errorf("Name=%q", cfg.Name)
	}
	if cfg.DisplayName != serviceDisplayName {
		t.Errorf("DisplayName=%q", cfg.DisplayName)
	}
	if len(cfg.Arguments) != 2 || cfg.Arguments[0] != "--config" || cfg.Arguments[1] != "/path/to/cfg.yaml" {
		t.Errorf("Arguments=%v", cfg.Arguments)
	}
}

func TestAllowedServiceActions_CoversHelp(t *testing.T) {
	// Every action listed in usage text must exist in the allowed
	// set, otherwise an operator following the help would hit the
	// "unknown action" branch.
	for _, action := range []string{"install", "uninstall", "start", "stop", "restart", "status"} {
		if _, ok := allowedServiceActions[action]; !ok {
			t.Errorf("%q in usage but not in allowedServiceActions", action)
		}
	}
}
