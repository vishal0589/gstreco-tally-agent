package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
)

type fakeUI struct {
	confirms []bool
	lines    []string
	logs     []string
}

func (ui *fakeUI) Infof(format string, args ...any)  { ui.logs = append(ui.logs, "info") }
func (ui *fakeUI) Warnf(format string, args ...any)  { ui.logs = append(ui.logs, "warn") }
func (ui *fakeUI) Errorf(format string, args ...any) { ui.logs = append(ui.logs, "error") }
func (ui *fakeUI) Confirm(_ string, _ bool) bool {
	if len(ui.confirms) == 0 {
		return false
	}
	next := ui.confirms[0]
	ui.confirms = ui.confirms[1:]
	return next
}
func (ui *fakeUI) ReadLine(_ string) string {
	if len(ui.lines) == 0 {
		return ""
	}
	next := ui.lines[0]
	ui.lines = ui.lines[1:]
	return next
}

func TestDiscoverEndpointsForPorts(t *testing.T) {
	got, err := discoverEndpointsForPorts("2026, 9000,2026")
	if err != nil {
		t.Fatalf("discoverEndpointsForPorts: %v", err)
	}
	want := []string{"http://127.0.0.1:2026", "http://127.0.0.1:9000"}
	if len(got) != len(want) {
		t.Fatalf("len=%d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d]=%s want %s", i, got[i], want[i])
		}
	}
}

func TestRunDiscoveryFlow_UsesCustomPortFallback(t *testing.T) {
	var commands [][]string
	runCommand := func(_ context.Context, name string, args ...string) (commandResult, error) {
		commands = append(commands, append([]string{name}, args...))
		if len(commands) == 1 {
			return commandResult{Output: "no Tally found", ExitCode: 2}, errors.New("exit 2")
		}
		return commandResult{Output: "ok", ExitCode: 0}, nil
	}
	ui := &fakeUI{confirms: []bool{false}, lines: []string{"2026"}}
	err := runDiscoveryFlow(
		context.Background(),
		ui,
		installerOptions{},
		runCommand,
		"C:/Program Files/GST Reco Agent/gstreco-tally-agentctl.exe",
		"C:/ProgramData/GST Reco/agent/config.yaml",
		"https://example.com",
	)
	if err != nil {
		t.Fatalf("runDiscoveryFlow: %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("commands=%d want 2", len(commands))
	}
	if !strings.Contains(strings.Join(commands[1], " "), "http://127.0.0.1:2026") {
		t.Fatalf("custom-port discover missing endpoint arg: %v", commands[1])
	}
}

func TestRunDiscoveryFlow_StopsOnNonRecoverableError(t *testing.T) {
	var commands [][]string
	runCommand := func(_ context.Context, name string, args ...string) (commandResult, error) {
		commands = append(commands, append([]string{name}, args...))
		return commandResult{Output: "signing secret missing", ExitCode: 1}, errors.New("exit 1")
	}

	err := runDiscoveryFlow(
		context.Background(),
		&fakeUI{confirms: []bool{true}, lines: []string{"2026"}},
		installerOptions{},
		runCommand,
		"C:/Program Files/GST Reco Agent/gstreco-tally-agentctl.exe",
		"C:/ProgramData/GST Reco/agent/config.yaml",
		"https://example.com",
	)
	if err == nil {
		t.Fatal("expected a non-recoverable discovery failure")
	}
	if len(commands) != 1 {
		t.Fatalf("commands=%d want 1", len(commands))
	}
	if !strings.Contains(err.Error(), "signing secret missing") {
		t.Fatalf("err=%v", err)
	}
}

func TestWaitForApproval_Expiry(t *testing.T) {
	client := &installerSessionClient{
		baseURL: "https://example.com",
		http: &http.Client{
			Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return jsonResponse(200, sessionStatusResponse{Status: "expired"}), nil
			}),
		},
	}
	_, err := waitForApproval(context.Background(), &fakeUI{}, client, &sessionRef{ID: "sess", Token: "tok"}, 1)
	if !errors.Is(err, errApprovalExpired) {
		t.Fatalf("err=%v want errApprovalExpired", err)
	}
}

func TestRunInstaller_RerunRepairsServiceWithoutApproval(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := config.Save(cfgPath, &config.Config{
		Server:       "https://example.com",
		ConnectionID: "conn-1",
		DeviceName:   "DESKTOP-1",
		PairedAt:     time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	ui := &fakeUI{}
	var commands [][]string
	deps := defaultInstallerDeps()
	deps.isAdmin = func() (bool, error) { return true, nil }
	deps.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if strings.HasSuffix(req.URL.Path, "/api/tally/agent/version") {
				return jsonResponse(200, versionMetadataResponse{
					Latest: "0.1.15",
					Platforms: map[string]installerAsset{
						"windows-amd64": {URL: "https://example.com/agentctl.exe"},
					},
					Daemon: map[string]installerAsset{
						"windows-amd64": {URL: "https://example.com/agent.exe"},
					},
				}), nil
			}
			return binaryResponse(200, []byte("binary")), nil
		}),
	}
	deps.loadLocalPair = func(string) (localPairState, error) {
		return localPairState{
			State:      "paired",
			ConfigPath: cfgPath,
			Config: &config.Config{
				Server:       "https://example.com",
				ConnectionID: "conn-1",
				DeviceName:   "DESKTOP-1",
			},
		}, nil
	}
	deps.runCommand = func(_ context.Context, name string, args ...string) (commandResult, error) {
		commands = append(commands, append([]string{name}, args...))
		switch {
		case len(args) >= 2 && args[0] == "service" && args[1] == "status":
			return commandResult{Output: "service status: running", ExitCode: 0}, nil
		default:
			return commandResult{Output: "ok", ExitCode: 0}, nil
		}
	}

	exitCode := runInstaller(context.Background(), ui, installerOptions{
		server:     "https://example.com",
		configPath: cfgPath,
		installDir: tmp,
	}, deps)
	if exitCode != 0 {
		t.Fatalf("exitCode=%d", exitCode)
	}
	if len(commands) == 0 {
		t.Fatal("expected service/discover commands")
	}
	if strings.Contains(strings.Join(flattenCommands(commands), " "), "/api/tally/installer/sessions/start") {
		t.Fatal("rerun should not start a fresh installer session when already paired")
	}
}

func TestRunInstallerMain_HelpBypassesWindowsGate(t *testing.T) {
	deps := defaultInstallerDeps()
	deps.goos = func() string { return "darwin" }

	exitCode := runInstallerMain(os.Stdout, os.Stderr, os.Stdin, []string{"--help"}, deps)
	if exitCode != 0 {
		t.Fatalf("exitCode=%d want 0", exitCode)
	}
}

func TestDownloadAsset_DoesNotClobberExistingBinaryOnChecksumMismatch(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "gstreco-tally-agent.exe")
	if err := os.WriteFile(dest, []byte("old-binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	httpClient := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return binaryResponse(200, []byte("new-binary")), nil
		}),
	}

	err := downloadAsset(
		context.Background(),
		httpClient,
		"https://example.com/gstreco-tally-agent.exe",
		dest,
		strings.Repeat("0", 64),
	)
	if err == nil {
		t.Fatal("expected checksum mismatch")
	}

	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read existing binary: %v", readErr)
	}
	if string(got) != "old-binary" {
		t.Fatalf("binary was clobbered: got %q", string(got))
	}
}

func TestDownloadAsset_ReplacesExistingBinaryAfterVerification(t *testing.T) {
	tmp := t.TempDir()
	dest := filepath.Join(tmp, "gstreco-tally-agent.exe")
	if err := os.WriteFile(dest, []byte("old-binary"), 0o644); err != nil {
		t.Fatal(err)
	}

	payload := []byte("new-binary")
	sum := sha256.Sum256(payload)
	httpClient := &http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return binaryResponse(200, payload), nil
		}),
	}

	err := downloadAsset(
		context.Background(),
		httpClient,
		"https://example.com/gstreco-tally-agent.exe",
		dest,
		hex.EncodeToString(sum[:]),
	)
	if err != nil {
		t.Fatalf("downloadAsset: %v", err)
	}

	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read replaced binary: %v", readErr)
	}
	if string(got) != string(payload) {
		t.Fatalf("binary mismatch: got %q want %q", string(got), string(payload))
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func jsonResponse(status int, body any) *http.Response {
	payload, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(payload))),
		Header:     make(http.Header),
	}
}

func binaryResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(string(body))),
		Header:     make(http.Header),
	}
}

func flattenCommands(commands [][]string) []string {
	out := make([]string, 0, len(commands))
	for _, command := range commands {
		out = append(out, strings.Join(command, " "))
	}
	return out
}
