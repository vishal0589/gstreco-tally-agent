package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Only release versions can be ordered safely. Unknown, custom prerelease and
// dev builds require an explicit release/repair decision, not a guessed order.
const versionRepairAdvice = "Leave this setup in place and contact your GST Reco administrator for a matching release or repair package. Do not delete binaries or pairing settings to bypass this check."

var releaseVersionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(\+[0-9A-Za-z.-]+)?$`)

func parseReleaseVersion(raw string) ([3]uint64, error) {
	var version [3]uint64
	match := releaseVersionPattern.FindStringSubmatch(strings.TrimSpace(raw))
	if match == nil {
		return version, fmt.Errorf("unrecognized release version %q", raw)
	}
	for i := range version {
		n, err := strconv.ParseUint(match[i+1], 10, 64)
		if err != nil {
			return version, fmt.Errorf("invalid release version %q", raw)
		}
		version[i] = n
	}
	return version, nil
}

func checkInstalledVersions(ctx context.Context, runCommand func(context.Context, string, ...string) (commandResult, error), installDir, latest string) error {
	target, err := parseReleaseVersion(latest)
	if err != nil {
		return fmt.Errorf("server metadata: %w. %s", err, versionRepairAdvice)
	}
	for _, binary := range []struct{ name, prefix string }{
		{"gstreco-tally-agentctl.exe", "agentctl "},
		{"gstreco-tally-agent.exe", "gstreco-tally-agent "},
	} {
		path := filepath.Join(installDir, binary.name)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		} // fresh install or missing component
		if err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular binary", path)
		}
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		result, err := runCommand(probeCtx, path, "--version")
		cancel()
		if err != nil || result.ExitCode != 0 {
			return fmt.Errorf("cannot verify installed version of %s; leaving existing binaries unchanged. %s", path, versionRepairAdvice)
		}
		output := strings.TrimSpace(result.Output)
		if !strings.HasPrefix(output, binary.prefix) {
			return fmt.Errorf("unrecognized version output from %s. %s", path, versionRepairAdvice)
		}
		fields := strings.Fields(strings.TrimPrefix(output, binary.prefix))
		if len(fields) == 0 {
			return fmt.Errorf("missing installed version from %s. %s", path, versionRepairAdvice)
		}
		installed, err := parseReleaseVersion(fields[0])
		if err != nil {
			return fmt.Errorf("%s: %w; leaving existing binaries unchanged. %s", path, err, versionRepairAdvice)
		}
		for i := range target {
			if installed[i] > target[i] {
				return fmt.Errorf("refusing downgrade of %s from %s to %s; publish matching or newer downloads first", path, fields[0], latest)
			}
			if installed[i] < target[i] {
				break
			}
		}
	}
	return nil
}
