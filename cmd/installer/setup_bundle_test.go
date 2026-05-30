package main

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vishal0589/gstreco-tally-agent/internal/pair"
)

func TestSetupBundleCodeFromName(t *testing.T) {
	cases := map[string]string{
		`C:\Users\x\Downloads\gstreco-tally-setup-ABC234.exe`: "ABC234",
		"gstreco-tally-setup-abc234.exe":                      "ABC234",
		"gstreco-tally-setup-ABC234 (1).exe":                  "ABC234",
		"gstreco-tally-setup.exe":                             "",
		"gstreco-tally-installer-windows-amd64.exe":           "",
		"installer.test":                                      "",
		"random.exe":                                          "",
	}
	for in, want := range cases {
		if got := setupBundleCodeFromName(in); got != want {
			t.Errorf("setupBundleCodeFromName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestPromptTallyPorts(t *testing.T) {
	if got := promptTallyPorts(&fakeUI{lines: []string{""}}, ""); got != "9000" {
		t.Errorf("default: got %q want 9000", got)
	}
	if got := promptTallyPorts(&fakeUI{lines: []string{""}}, "2026"); got != "2026" {
		t.Errorf("preset honored on Enter: got %q want 2026", got)
	}
	if got := promptTallyPorts(&fakeUI{lines: []string{"9001, 9002"}}, "9000"); got != "9001, 9002" {
		t.Errorf("explicit entry wins: got %q want '9001, 9002'", got)
	}
}

func TestDiscoverEndpointsForPorts_HostAndPort(t *testing.T) {
	got, err := discoverEndpointsForPorts("9000, 192.168.1.5:9000, http://10.0.0.2:9001")
	if err != nil {
		t.Fatalf("discoverEndpointsForPorts: %v", err)
	}
	want := []string{
		"http://127.0.0.1:9000",
		"http://192.168.1.5:9000",
		"http://10.0.0.2:9001",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRunInstaller_SetupBundleClaimsPairCodeWithoutBrowser(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")

	var claimed []pair.Options
	var commands [][]string
	deps := defaultInstallerDeps()
	deps.isAdmin = func() (bool, error) { return true, nil }
	deps.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/api/tally/agent/version") {
				return jsonResponse(200, versionMetadataResponse{
					Latest:    "0.1.21",
					Platforms: map[string]installerAsset{"windows-amd64": {URL: "https://example.com/agentctl.exe"}},
					Daemon:    map[string]installerAsset{"windows-amd64": {URL: "https://example.com/agent.exe"}},
				}), nil
			}
			return binaryResponse(200, []byte("binary")), nil
		}),
	}
	deps.claimPair = func(_ context.Context, opts pair.Options) (*pair.ClaimResponse, error) {
		claimed = append(claimed, opts)
		return &pair.ClaimResponse{ConnectionID: "conn-9", CompanyID: "co-9", Token: "tok", HmacSecret: "hmac"}, nil
	}
	deps.loadLocalPair = func(string) (localPairState, error) {
		return localPairState{State: "unpaired", ConfigPath: cfgPath}, nil
	}
	deps.runCommand = func(_ context.Context, name string, args ...string) (commandResult, error) {
		commands = append(commands, append([]string{name}, args...))
		if len(args) >= 2 && args[0] == "service" && args[1] == "status" {
			return commandResult{Output: "service status: running", ExitCode: 0}, nil
		}
		return commandResult{Output: "ok", ExitCode: 0}, nil
	}

	// Press Enter at the port prompt → default 9000. The --code stands in for
	// the code that would normally be read from the installer's filename.
	ui := &fakeUI{lines: []string{""}}
	exitCode := runInstaller(context.Background(), ui, installerOptions{
		server:     "https://example.com",
		configPath: cfgPath,
		installDir: tmp,
		code:       "ABC234",
	}, deps)

	if exitCode != 0 {
		t.Fatalf("exitCode=%d", exitCode)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimPair calls=%d want 1", len(claimed))
	}
	if claimed[0].Code != "ABC234" {
		t.Fatalf("claimed code=%q want ABC234", claimed[0].Code)
	}

	var lines []string
	for _, c := range commands {
		lines = append(lines, strings.Join(c, " "))
	}
	joined := strings.Join(lines, " | ")
	if strings.Contains(joined, "/api/tally/installer/sessions/start") {
		t.Fatal("setup-bundle path must not start a browser approval session")
	}
	if !strings.Contains(joined, "http://127.0.0.1:9000") {
		t.Fatalf("expected discovery to check the confirmed port 9000: %v", commands)
	}
}
