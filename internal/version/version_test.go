package version

import (
	"strings"
	"testing"
)

func TestStringIncludesAllParts(t *testing.T) {
	Version = "v1.2.3"
	Commit = "abc1234"
	Date = "2026-04-23"
	defer func() {
		Version, Commit, Date = "dev", "unknown", "unknown"
	}()

	got := String()
	for _, want := range []string{"v1.2.3", "abc1234", "2026-04-23"} {
		if !strings.Contains(got, want) {
			t.Errorf("String() = %q, want to contain %q", got, want)
		}
	}
}

func TestStringDefaultsAreSafe(t *testing.T) {
	// Defaults must not panic and must produce a non-empty string so logs
	// always have something to print even without ldflags injection.
	if String() == "" {
		t.Fatal("String() returned empty for default values")
	}
}
