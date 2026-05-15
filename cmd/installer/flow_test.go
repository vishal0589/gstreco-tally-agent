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
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/pair"
	"github.com/vishal0589/gstreco-tally-agent/internal/secretstore"
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

func TestRunInstaller_AddTenantStartsFreshApprovalOnPairedMachine(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	if err := config.Save(cfgPath, &config.Config{
		Connections: []config.PairedConnection{
			{
				Server:       "https://example.com",
				ConnectionID: "conn-1",
				CompanyID:    "company-1",
				DeviceName:   "DESKTOP-1",
				PairedAt:     time.Now().UTC(),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	ui := &fakeUI{}
	var commands [][]string
	var persistCalls []pair.PersistOptions
	loadCalls := 0
	deps := defaultInstallerDeps()
	deps.isAdmin = func() (bool, error) { return true, nil }
	deps.openBrowser = func(string) error { return nil }
	deps.httpClient = &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			switch {
			case strings.HasSuffix(req.URL.Path, "/api/tally/installer/sessions/start"):
				return jsonResponse(200, startSessionResponse{
					SessionID:       "sess-1",
					SessionToken:    "session-token-1",
					UserCode:        "ABC-D23",
					VerificationURL: "https://example.com/approve",
					PollIntervalMS:  1,
					Status:          "pending_approval",
				}), nil
			case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/api/tally/installer/sessions/sess-1"):
				return jsonResponse(200, sessionStatusResponse{Status: "approved"}), nil
			case strings.HasSuffix(req.URL.Path, "/api/tally/installer/sessions/sess-1/claim"):
				return jsonResponse(200, claimSessionResponse{
					SessionID:    "sess-1",
					Status:       "claimed",
					ConnectionID: "conn-2",
					CompanyID:    "company-2",
					Token:        "bearer-2",
					HmacSecret:   "hmac-2",
				}), nil
			case strings.HasSuffix(req.URL.Path, "/api/tally/agent/version"):
				return jsonResponse(200, versionMetadataResponse{
					Latest: "0.1.21",
					Platforms: map[string]installerAsset{
						"windows-amd64": {URL: "https://example.com/agentctl.exe"},
					},
					Daemon: map[string]installerAsset{
						"windows-amd64": {URL: "https://example.com/agent.exe"},
					},
				}), nil
			default:
				return binaryResponse(200, []byte("binary")), nil
			}
		}),
	}
	deps.persistPair = func(opts pair.PersistOptions) error {
		persistCalls = append(persistCalls, opts)
		return nil
	}
	deps.loadLocalPair = func(string) (localPairState, error) {
		loadCalls++
		return localPairState{
			State:      "paired",
			ConfigPath: cfgPath,
			Config: &config.Config{
				Connections: []config.PairedConnection{
					{
						Server:       "https://example.com",
						ConnectionID: "conn-1",
						CompanyID:    "company-1",
						DeviceName:   "DESKTOP-1",
						PairedAt:     time.Now().UTC(),
					},
				},
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
		addTenant:  true,
	}, deps)
	if exitCode != 0 {
		t.Fatalf("exitCode=%d", exitCode)
	}
	if len(persistCalls) != 1 {
		t.Fatalf("persistCalls=%d want 1", len(persistCalls))
	}
	if persistCalls[0].ConnectionID != "conn-2" {
		t.Fatalf("persisted connection=%q want conn-2", persistCalls[0].ConnectionID)
	}
	if persistCalls[0].CompanyID != "company-2" {
		t.Fatalf("persisted company=%q want company-2", persistCalls[0].CompanyID)
	}
	if loadCalls < 2 {
		t.Fatalf("loadLocalPair called %d times, want at least 2", loadCalls)
	}
	if len(commands) == 0 {
		t.Fatal("expected service/discover commands")
	}
}

func TestDetectLocalPairState_MultiConnectionPairedIfAnyConnectionSecretsExist(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.yaml")
	cfg := &config.Config{}
	cfg.SetPairedConnections([]config.PairedConnection{
		{
			Server:       "https://example.com",
			ConnectionID: "conn-1",
			CompanyID:    "company-1",
			DeviceName:   "DESKTOP-1",
			PairedAt:     time.Now().UTC(),
		},
		{
			Server:       "https://example.com",
			ConnectionID: "conn-2",
			CompanyID:    "company-2",
			DeviceName:   "DESKTOP-1",
			PairedAt:     time.Now().UTC(),
		},
	})
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	store := secretstore.NewFileStore(
		secretstore.DefaultDir(filepath.Dir(cfgPath)),
		secretstore.ReadMachineID,
	)
	hmacKey, bearerKey := keyring.ConnectionKeys("conn-2")
	if err := store.Set(keyring.ServiceName, hmacKey, "hmac-2"); err != nil {
		t.Fatalf("store hmac: %v", err)
	}
	if err := store.Set(keyring.ServiceName, bearerKey, "bearer-2"); err != nil {
		t.Fatalf("store bearer: %v", err)
	}

	state, err := detectLocalPairState(cfgPath)
	if err != nil {
		t.Fatalf("detectLocalPairState: %v", err)
	}
	if state.State != "paired" {
		t.Fatalf("state=%q want paired", state.State)
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
