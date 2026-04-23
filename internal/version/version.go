// Package version exposes the agent version string. The value is injected at
// build time via -ldflags "-X github.com/vishal0589/gstreco-tally-agent/internal/version.Version=v1.2.3".
// Unreleased local builds report "dev".
package version

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns "v1.2.3 (abc1234, 2026-04-23)" style version info suitable for
// logging and agentctl --version output.
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
