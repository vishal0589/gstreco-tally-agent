package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/pair"
	"github.com/vishal0589/gstreco-tally-agent/internal/secretstore"
	"github.com/vishal0589/gstreco-tally-agent/internal/version"
)

type installerDeps struct {
	goos             func() string
	version          func() string
	httpClient       *http.Client
	isAdmin          func() (bool, error)
	relaunchAsAdmin  func(args []string) error
	openBrowser      func(url string) error
	runCommand       func(ctx context.Context, name string, args ...string) (commandResult, error)
	persistPair      func(opts pair.PersistOptions) error
	loadLocalPair    func(configPath string) (localPairState, error)
}

func defaultInstallerDeps() installerDeps {
	return installerDeps{
		goos:       func() string { return runtime.GOOS },
		version:    version.String,
		httpClient: &http.Client{Timeout: 120 * time.Second},
		isAdmin:    isAdministrator,
		relaunchAsAdmin: func(args []string) error {
			return relaunchSelfAsAdmin(args)
		},
		openBrowser: openBrowserURL,
		runCommand: func(ctx context.Context, name string, args ...string) (commandResult, error) {
			cmd := exec.CommandContext(ctx, name, args...)
			out, err := cmd.CombinedOutput()
			exitCode := 0
			if err != nil {
				var exitErr *exec.ExitError
				if errors.As(err, &exitErr) {
					exitCode = exitErr.ExitCode()
				} else {
					exitCode = -1
				}
			}
			return commandResult{
				Output:   string(out),
				ExitCode: exitCode,
			}, err
		},
		persistPair: pair.PersistCredentials,
		loadLocalPair: func(configPath string) (localPairState, error) {
			return detectLocalPairState(configPath)
		},
	}
}

type installerUI interface {
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	Confirm(prompt string, defaultYes bool) bool
	ReadLine(prompt string) string
}

type consoleUI struct {
	stdout      io.Writer
	stderr      io.Writer
	reader      *bufio.Reader
	interactive bool
}

func newConsoleUI(stdout, stderr io.Writer, stdin *os.File) *consoleUI {
	interactive := false
	if stdin != nil {
		if info, err := stdin.Stat(); err == nil {
			interactive = (info.Mode() & os.ModeCharDevice) != 0
		}
	}
	return &consoleUI{
		stdout:      stdout,
		stderr:      stderr,
		reader:      bufio.NewReader(stdin),
		interactive: interactive,
	}
}

func (ui *consoleUI) Infof(format string, args ...any) {
	fmt.Fprintf(ui.stdout, format+"\n", args...)
}

func (ui *consoleUI) Warnf(format string, args ...any) {
	fmt.Fprintf(ui.stdout, format+"\n", args...)
}

func (ui *consoleUI) Errorf(format string, args ...any) {
	fmt.Fprintf(ui.stderr, format+"\n", args...)
}

func (ui *consoleUI) Confirm(prompt string, defaultYes bool) bool {
	if !ui.interactive {
		return false
	}
	suffix := "[y/N]"
	if defaultYes {
		suffix = "[Y/n]"
	}
	fmt.Fprintf(ui.stdout, "%s %s ", prompt, suffix)
	line, _ := ui.reader.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	if line == "" {
		return defaultYes
	}
	return line == "y" || line == "yes"
}

func (ui *consoleUI) ReadLine(prompt string) string {
	if !ui.interactive {
		return ""
	}
	fmt.Fprint(ui.stdout, prompt)
	line, _ := ui.reader.ReadString('\n')
	return strings.TrimSpace(line)
}

type installerSessionClient struct {
	baseURL string
	http    *http.Client
}

func newInstallerSessionClient(baseURL string, httpClient *http.Client) *installerSessionClient {
	return &installerSessionClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    httpClient,
	}
}

type startSessionRequest struct {
	DeviceName       string `json:"device_name,omitempty"`
	AgentVersion     string `json:"agent_version,omitempty"`
	InstallerVersion string `json:"installer_version,omitempty"`
	OS               string `json:"os,omitempty"`
}

type startSessionResponse struct {
	SessionID      string `json:"session_id"`
	SessionToken   string `json:"session_token"`
	UserCode       string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresAt      string `json:"expires_at"`
	PollIntervalMS int    `json:"poll_interval_ms"`
	Status         string `json:"status"`
}

type sessionStatusResponse struct {
	SessionID           string `json:"session_id"`
	UserCode            string `json:"user_code"`
	Status              string `json:"status"`
	DeviceName          string `json:"device_name"`
	AgentVersion        string `json:"agent_version"`
	InstallerVersion    string `json:"installer_version"`
	OS                  string `json:"os"`
	ClaimedConnectionID string `json:"claimed_connection_id"`
	FailureCode         string `json:"failure_code"`
	FailureMessage      string `json:"failure_message"`
	PollIntervalMS      int    `json:"poll_interval_ms"`
}

type updateSessionRequest struct {
	Status         string `json:"status"`
	FailureCode    string `json:"failure_code,omitempty"`
	FailureMessage string `json:"failure_message,omitempty"`
}

type claimSessionRequest struct {
	SessionToken string `json:"session_token"`
}

type claimSessionResponse struct {
	SessionID    string `json:"session_id"`
	Status       string `json:"status"`
	ConnectionID string `json:"connection_id"`
	CompanyID    string `json:"company_id"`
	Token        string `json:"token"`
	HmacSecret   string `json:"hmac_secret"`
}

type installerAsset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

type versionMetadataResponse struct {
	Latest                       string                    `json:"latest"`
	MinimumSupportedAgentVersion string                    `json:"minimum_supported_agent_version"`
	Platforms                    map[string]installerAsset `json:"platforms"`
	Daemon                       map[string]installerAsset `json:"daemon"`
}

type httpError struct {
	StatusCode int
	Message    string
}

func (e *httpError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("server returned %d", e.StatusCode)
	}
	return fmt.Sprintf("server returned %d: %s", e.StatusCode, e.Message)
}

func (c *installerSessionClient) Start(ctx context.Context, req startSessionRequest) (*startSessionResponse, error) {
	var out startSessionResponse
	if err := c.doJSON(ctx, http.MethodPost, "/api/tally/installer/sessions/start", "", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *installerSessionClient) Status(ctx context.Context, sessionID, sessionToken string) (*sessionStatusResponse, error) {
	var out sessionStatusResponse
	if err := c.doJSON(ctx, http.MethodGet, "/api/tally/installer/sessions/"+sessionID, sessionToken, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *installerSessionClient) Update(ctx context.Context, sessionID, sessionToken, status, failureCode, failureMessage string) error {
	return c.doJSON(
		ctx,
		http.MethodPatch,
		"/api/tally/installer/sessions/"+sessionID,
		sessionToken,
		updateSessionRequest{
			Status:         status,
			FailureCode:    failureCode,
			FailureMessage: failureMessage,
		},
		nil,
	)
}

func (c *installerSessionClient) Claim(ctx context.Context, sessionID, sessionToken string) (*claimSessionResponse, error) {
	var out claimSessionResponse
	if err := c.doJSON(
		ctx,
		http.MethodPost,
		"/api/tally/installer/sessions/"+sessionID+"/claim",
		"",
		claimSessionRequest{SessionToken: sessionToken},
		&out,
	); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *installerSessionClient) FetchVersion(ctx context.Context) (*versionMetadataResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/tally/agent/version", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, &httpError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(body))}
	}
	var out versionMetadataResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *installerSessionClient) doJSON(ctx context.Context, method, path, sessionToken string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(payload))
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sessionToken != "" {
		req.Header.Set("x-tally-installer-session-token", sessionToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := ""
		var errBody map[string]any
		if decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 2048)).Decode(&errBody); decodeErr == nil {
			if raw, ok := errBody["message"].(string); ok {
				message = raw
			}
		}
		return &httpError{StatusCode: resp.StatusCode, Message: message}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type localPairState struct {
	State        string
	ConfigPath   string
	Config       *config.Config
}

func detectLocalPairState(configPath string) (localPairState, error) {
	if configPath == "" {
		configPath = config.DefaultPath()
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			return localPairState{State: "unpaired", ConfigPath: configPath}, nil
		}
		return localPairState{}, err
	}
	if !cfg.IsPaired() {
		return localPairState{State: "incomplete", ConfigPath: configPath, Config: cfg}, nil
	}
	store := secretstore.NewFileStore(
		secretstore.DefaultDir(filepath.Dir(configPath)),
		secretstore.ReadMachineID,
	)
	hmacKey, bearerKey := keyring.ConnectionKeys(cfg.ConnectionID)
	if _, err := store.Get(keyring.ServiceName, hmacKey); err != nil {
		return localPairState{State: "incomplete", ConfigPath: configPath, Config: cfg}, nil
	}
	if _, err := store.Get(keyring.ServiceName, bearerKey); err != nil {
		return localPairState{State: "incomplete", ConfigPath: configPath, Config: cfg}, nil
	}
	return localPairState{State: "paired", ConfigPath: configPath, Config: cfg}, nil
}

type serviceState string

const (
	serviceNotInstalled serviceState = "not_installed"
	serviceRunning      serviceState = "running"
	serviceStopped      serviceState = "stopped"
	serviceUnknown      serviceState = "unknown"
)

type commandResult struct {
	Output   string
	ExitCode int
}

type sessionRef struct {
	ID    string
	Token string
}

type discoverCommandError struct {
	recoverable bool
	message     string
	cause       error
}

func (e *discoverCommandError) Error() string {
	if e.message != "" {
		return e.message
	}
	if e.cause != nil {
		return e.cause.Error()
	}
	return "discover command failed"
}

func (e *discoverCommandError) Unwrap() error {
	return e.cause
}

func runInstaller(ctx context.Context, ui installerUI, opts installerOptions, deps installerDeps) int {
	isAdmin, err := deps.isAdmin()
	if err != nil {
		ui.Errorf("Could not determine whether the installer is elevated: %v", err)
		return 1
	}
	if !isAdmin {
		ui.Infof("Requesting administrator access for service installation…")
		if err := deps.relaunchAsAdmin(os.Args[1:]); err != nil {
			ui.Errorf("Could not relaunch as administrator: %v", err)
			return 1
		}
		return 0
	}

	if opts.configPath == "" {
		opts.configPath = config.DefaultPath()
	}
	serverURL := strings.TrimRight(opts.server, "/")
	api := newInstallerSessionClient(serverURL, deps.httpClient)
	deviceName := localHostname()
	pairState, err := deps.loadLocalPair(opts.configPath)
	if err != nil {
		ui.Errorf("Could not inspect local pairing state: %v", err)
		return 1
	}

	var activeSession *sessionRef
	if pairState.State != "paired" {
		session, claim, exitCode, err := runApprovalFlow(ctx, ui, opts, deps, api, deviceName)
		if err != nil {
			ui.Errorf("%v", err)
			return exitCode
		}
		if err := deps.persistPair(pair.PersistOptions{
			ConfigPath:   opts.configPath,
			Keyring:      secretstore.NewFileStore(secretstore.DefaultDir(filepath.Dir(opts.configPath)), secretstore.ReadMachineID),
			Server:       serverURL,
			DeviceName:   deviceName,
			ConnectionID: claim.ConnectionID,
			Token:        claim.Token,
			HmacSecret:   claim.HmacSecret,
			PairedAt:     time.Now().UTC(),
		}); err != nil {
			_ = api.Update(ctx, session.ID, session.Token, "failed", "persist_failed", err.Error())
			ui.Errorf("Could not persist claimed credentials: %v", err)
			return 1
		}
		activeSession = session
		pairState, err = deps.loadLocalPair(opts.configPath)
		if err != nil {
			ui.Errorf("Could not reload local pairing state after claim: %v", err)
			return 1
		}
	}

	versionMeta, err := api.FetchVersion(ctx)
	if err != nil {
		failSession(ctx, api, activeSession, "metadata_fetch_failed", err.Error())
		ui.Errorf("Could not fetch agent download metadata: %v", err)
		return 1
	}
	agentctlAsset, ok := versionMeta.Platforms["windows-amd64"]
	if !ok || strings.TrimSpace(agentctlAsset.URL) == "" {
		failSession(ctx, api, activeSession, "missing_asset", "windows-amd64 agentctl asset missing from version metadata")
		ui.Errorf("Windows agentctl asset missing from version metadata.")
		return 1
	}
	daemonAsset, ok := versionMeta.Daemon["windows-amd64"]
	if !ok || strings.TrimSpace(daemonAsset.URL) == "" {
		failSession(ctx, api, activeSession, "missing_asset", "windows-amd64 daemon asset missing from version metadata")
		ui.Errorf("Windows daemon asset missing from version metadata.")
		return 1
	}

	if err := os.MkdirAll(opts.installDir, 0o755); err != nil {
		failSession(ctx, api, activeSession, "install_dir_failed", err.Error())
		ui.Errorf("Could not create install directory %s: %v", opts.installDir, err)
		return 1
	}
	agentctlPath := filepath.Join(opts.installDir, "gstreco-tally-agentctl.exe")
	agentPath := filepath.Join(opts.installDir, "gstreco-tally-agent.exe")

	ui.Infof("Downloading the latest agent binaries…")
	if err := downloadAsset(ctx, deps.httpClient, agentctlAsset.URL, agentctlPath, agentctlAsset.SHA256); err != nil {
		failSession(ctx, api, activeSession, "download_failed", err.Error())
		ui.Errorf("Could not download agentctl: %v", err)
		return 1
	}
	if err := downloadAsset(ctx, deps.httpClient, daemonAsset.URL, agentPath, daemonAsset.SHA256); err != nil {
		failSession(ctx, api, activeSession, "download_failed", err.Error())
		ui.Errorf("Could not download the daemon: %v", err)
		return 1
	}

	if activeSession != nil {
		_ = api.Update(ctx, activeSession.ID, activeSession.Token, "installing_service", "", "")
	}
	if err := ensureServiceHealthy(ctx, deps.runCommand, agentPath); err != nil {
		failSession(ctx, api, activeSession, "service_repair_failed", err.Error())
		ui.Errorf("Could not install or repair the Windows service: %v", err)
		return 1
	}

	if activeSession != nil {
		_ = api.Update(ctx, activeSession.ID, activeSession.Token, "discovering_tally", "", "")
	}
	if err := runDiscoveryFlow(ctx, ui, opts, deps.runCommand, agentctlPath, opts.configPath, serverURL); err != nil {
		failSession(ctx, api, activeSession, "discover_failed", err.Error())
		ui.Errorf("Tally discovery failed: %v", err)
		return 1
	}

	if activeSession != nil {
		_ = api.Update(ctx, activeSession.ID, activeSession.Token, "ready", "", "")
	}
	ui.Infof("GST Reco is paired and ready on this machine.")
	return 0
}

func runApprovalFlow(
	ctx context.Context,
	ui installerUI,
	opts installerOptions,
	deps installerDeps,
	api *installerSessionClient,
	deviceName string,
) (*sessionRef, *claimSessionResponse, int, error) {
	for {
		started, err := api.Start(ctx, startSessionRequest{
			DeviceName:       deviceName,
			AgentVersion:     version.Version,
			InstallerVersion: version.Version,
			OS:               "windows-amd64",
		})
		if err != nil {
			return nil, nil, 1, fmt.Errorf("could not start the installer session: %w", err)
		}
		session := &sessionRef{ID: started.SessionID, Token: started.SessionToken}

		ui.Infof("Approval code: %s", started.UserCode)
		ui.Infof("Approve this machine in GST Reco: %s", started.VerificationURL)
		if opts.noBrowser {
			_ = api.Update(ctx, session.ID, session.Token, "pending_approval", "", "")
		} else if err := deps.openBrowser(started.VerificationURL); err != nil {
			ui.Warnf("Could not open the browser automatically: %v", err)
			_ = api.Update(ctx, session.ID, session.Token, "pending_approval", "", "")
		} else {
			_ = api.Update(ctx, session.ID, session.Token, "browser_opened", "", "")
		}

		approval, err := waitForApproval(ctx, ui, api, session, started.PollIntervalMS)
		if err != nil {
			if errors.Is(err, errApprovalExpired) && ui.Confirm("Approval expired. Start a fresh browser approval session?", true) {
				continue
			}
			return nil, nil, 2, err
		}
		if approval.Status == "failed" {
			return nil, nil, 2, fmt.Errorf("approval was cancelled or failed: %s %s", approval.FailureCode, approval.FailureMessage)
		}

		claim, err := api.Claim(ctx, session.ID, session.Token)
		if err != nil {
			var httpErr *httpError
			if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusGone {
				if ui.Confirm("Approval session expired while claiming. Start a fresh session?", true) {
					continue
				}
				return nil, nil, 2, fmt.Errorf("installer session expired while claiming")
			}
			return nil, nil, 1, fmt.Errorf("could not claim the approved installer session: %w", err)
		}
		return session, claim, 0, nil
	}
}

var errApprovalExpired = errors.New("installer approval expired")

func waitForApproval(
	ctx context.Context,
	ui installerUI,
	api *installerSessionClient,
	session *sessionRef,
	pollMS int,
) (*sessionStatusResponse, error) {
	if pollMS <= 0 {
		pollMS = 2000
	}
	ticker := time.NewTicker(time.Duration(pollMS) * time.Millisecond)
	defer ticker.Stop()

	for {
		status, err := api.Status(ctx, session.ID, session.Token)
		if err != nil {
			return nil, err
		}
		switch status.Status {
		case "approved":
			ui.Infof("Browser approval complete.")
			return status, nil
		case "expired":
			return nil, errApprovalExpired
		case "failed":
			return status, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func failSession(ctx context.Context, api *installerSessionClient, session *sessionRef, code, message string) {
	if session == nil {
		return
	}
	_ = api.Update(ctx, session.ID, session.Token, "failed", code, message)
}

func ensureServiceHealthy(
	ctx context.Context,
	runCommand func(context.Context, string, ...string) (commandResult, error),
	agentPath string,
) error {
	state, _ := queryServiceState(ctx, runCommand, agentPath)
	switch state {
	case serviceRunning:
		if _, err := runCommand(ctx, agentPath, "service", "restart"); err == nil {
			return nil
		}
	case serviceStopped:
		if _, err := runCommand(ctx, agentPath, "service", "start"); err == nil {
			return nil
		}
	case serviceNotInstalled:
		if _, err := runCommand(ctx, agentPath, "service", "install"); err == nil {
			if _, startErr := runCommand(ctx, agentPath, "service", "start"); startErr == nil {
				return nil
			}
		}
	}

	_, _ = runCommand(ctx, agentPath, "service", "uninstall")
	if _, err := runCommand(ctx, agentPath, "service", "install"); err != nil {
		return fmt.Errorf("service install: %w", err)
	}
	if _, err := runCommand(ctx, agentPath, "service", "start"); err != nil {
		return fmt.Errorf("service start: %w", err)
	}
	return nil
}

func queryServiceState(
	ctx context.Context,
	runCommand func(context.Context, string, ...string) (commandResult, error),
	agentPath string,
) (serviceState, error) {
	result, err := runCommand(ctx, agentPath, "service", "status")
	if err != nil && result.ExitCode == -1 {
		return serviceUnknown, err
	}
	output := strings.ToLower(result.Output)
	switch {
	case strings.Contains(output, "service not installed"):
		return serviceNotInstalled, nil
	case strings.Contains(output, "service status: running"):
		return serviceRunning, nil
	case strings.Contains(output, "service status: stopped"):
		return serviceStopped, nil
	default:
		return serviceUnknown, nil
	}
}

func runDiscoveryFlow(
	ctx context.Context,
	ui installerUI,
	opts installerOptions,
	runCommand func(context.Context, string, ...string) (commandResult, error),
	agentctlPath string,
	configPath string,
	serverURL string,
) error {
	ui.Infof("Trying the common local Tally ports first…")
	err := runDiscoverCommand(ctx, runCommand, agentctlPath, configPath, serverURL, nil)
	if err == nil {
		return nil
	}
	var discoverErr *discoverCommandError
	if !errors.As(err, &discoverErr) || !discoverErr.recoverable {
		return err
	}

	ui.Warnf("GST Reco could not find Tally on the common ports yet.")
	if ui.Confirm("Retry the automatic Tally scan now?", true) {
		err = runDiscoverCommand(ctx, runCommand, agentctlPath, configPath, serverURL, nil)
		if err == nil {
			return nil
		}
		if !errors.As(err, &discoverErr) || !discoverErr.recoverable {
			return err
		}
	}

	customPorts := strings.TrimSpace(opts.customPorts)
	if customPorts == "" {
		customPorts = ui.ReadLine("Enter custom Tally ports (comma-separated), or press Enter to stop: ")
	}
	if customPorts == "" {
		return fmt.Errorf("Tally was not found on the common ports and no custom ports were provided")
	}
	endpoints, err := discoverEndpointsForPorts(customPorts)
	if err != nil {
		return err
	}
	ui.Infof("Trying the custom Tally ports: %s", customPorts)
	return runDiscoverCommand(ctx, runCommand, agentctlPath, configPath, serverURL, endpoints)
}

func runDiscoverCommand(
	ctx context.Context,
	runCommand func(context.Context, string, ...string) (commandResult, error),
	agentctlPath string,
	configPath string,
	serverURL string,
	endpoints []string,
) error {
	args := []string{"discover", "--config", configPath, "--server", serverURL}
	for _, endpoint := range endpoints {
		args = append(args, "--endpoint", endpoint)
	}
	result, err := runCommand(ctx, agentctlPath, args...)
	if err == nil {
		return nil
	}
	if result.ExitCode == 2 {
		message := strings.TrimSpace(result.Output)
		if message == "" {
			message = "discover command reported a recoverable failure"
		}
		return &discoverCommandError{
			recoverable: true,
			message:     message,
			cause:       err,
		}
	}
	return &discoverCommandError{
		recoverable: false,
		message:     strings.TrimSpace(result.Output),
		cause:       fmt.Errorf("discover command failed: %w", err),
	}
}

func discoverEndpointsForPorts(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	endpoints := make([]string, 0, len(parts))
	seen := map[string]struct{}{}
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return nil, fmt.Errorf("custom port %q is not numeric", part)
			}
		}
		port := "http://127.0.0.1:" + part
		if _, ok := seen[port]; ok {
			continue
		}
		seen[port] = struct{}{}
		endpoints = append(endpoints, port)
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no custom ports were provided")
	}
	return endpoints, nil
}

func downloadAsset(ctx context.Context, httpClient *http.Client, url, dest, expectedSHA string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("download %s failed: %s", url, strings.TrimSpace(string(body)))
	}
	file, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(io.MultiWriter(file, hasher), resp.Body); err != nil {
		return err
	}
	if strings.TrimSpace(expectedSHA) != "" {
		actual := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(actual, strings.TrimSpace(expectedSHA)) {
			return fmt.Errorf("sha256 mismatch for %s", dest)
		}
	}
	return nil
}

func defaultInstallDir() string {
	if runtime.GOOS == "windows" {
		return `C:\Program Files\GST Reco Agent`
	}
	return filepath.Join(os.TempDir(), "GST Reco Agent")
}

func localHostname() string {
	if host, err := os.Hostname(); err == nil && host != "" {
		if len(host) > 128 {
			return host[:128]
		}
		return host
	}
	return "windows-tally-machine"
}
