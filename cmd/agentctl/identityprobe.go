package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/syncrun"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// identityProbeCmd runs both company probes (List of Companies and the
// Type:Company NAME+GUID collection) on every configured, mapped and
// listening Tally port and prints, per port, what each probe returned and
// how every mapped company compares (QA 04-Nov N1 step (a)). Read-only:
// it never pushes a catalog, saves config or syncs. Prints no secret.
//
// Exit codes:
//
//	0 — every port was probed (whatever it answered)
//	1 — infrastructure failure (keyring, fetching mappings)
//	2 — user-fixable: not paired, bad flags, nothing to probe
func identityProbeCmd(args []string) int {
	return runIdentityProbe(os.Stdout, os.Stderr, args, identityProbeDeps{
		loadConfig:      config.Load,
		newOSKeyring:    defaultSecretStore,
		discover:        tally.Discover,
		listeningPorts:  tally.TallyListeningPorts,
		newMappingsFrom: defaultNewMappingsClient,
	})
}

type mappingsFetcher interface {
	FetchActiveMappings(ctx context.Context) (*ingest.ActiveMappingsResponse, error)
}

func defaultNewMappingsClient(baseURL, connectionID, bearer, secretB64 string) (mappingsFetcher, error) {
	return ingest.NewClient(baseURL, connectionID, bearer, secretB64)
}

type identityProbeDeps struct {
	loadConfig      func(path string) (*config.Config, error)
	newOSKeyring    func() keyring.Store
	discover        func(ctx context.Context, opts tally.DiscoverOptions) ([]tally.ProbeResult, error)
	listeningPorts  func() ([]int, error)
	newMappingsFrom func(baseURL, connectionID, bearer, secretB64 string) (mappingsFetcher, error)
}

// identityProbeReport is the --json shape.
type identityProbeReport struct {
	Mode        string                  `json:"mode"`
	Endpoints   []identityProbeEndpoint `json:"endpoints"`
	Connections []identityProbeConn     `json:"connections"`
}

type identityProbeConn struct {
	ConnectionID string `json:"connection_id"`
	Mappings     int    `json:"mappings"`
	Error        string `json:"error,omitempty"`
}

type identityProbeCompany struct {
	Name string `json:"name"`
	GUID string `json:"guid,omitempty"`
}

type identityProbeEndpoint struct {
	Endpoint         string                 `json:"endpoint"`
	Error            string                 `json:"error,omitempty"`
	ListOfCompanies  []identityProbeCompany `json:"list_of_companies"`
	TypeCompany      []identityProbeCompany `json:"type_company"`
	TypeCompanyError string                 `json:"type_company_error,omitempty"`
	TypeCompanyWarn  []string               `json:"type_company_warnings,omitempty"`
	ProbesAgree      bool                   `json:"probes_agree"`
	Mappings         []identityProbeMapping `json:"mappings"`
}

type identityProbeMapping struct {
	ConnectionID   string   `json:"connection_id"`
	MappingID      string   `json:"mapping_id"`
	Company        string   `json:"company"`
	MappedEndpoint string   `json:"mapped_endpoint"`
	StoredGUID     string   `json:"stored_guid"`
	InList         bool     `json:"in_list_of_companies"`
	GUIDProbeGUIDs []string `json:"type_company_guids"`
	Verdict        string   `json:"verdict"`
	Reason         string   `json:"reason"`
}

type connMapping struct {
	connectionID string
	m            ingest.ActiveMapping
}

func runIdentityProbe(stdout, stderr io.Writer, args []string, deps identityProbeDeps) int {
	fs := flag.NewFlagSet("identity-probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "override config file path")
	serverFlag := fs.String("server", "", "override server URL (default: from config)")
	connectionID := fs.String("connection-id", "", "only this paired connection's mappings")
	var endpointsFlag stringSlice
	fs.Var(&endpointsFlag, "endpoint", "Tally endpoint to probe (repeatable; default: configured + mapped + listening ports)")
	noMappings := fs.Bool("no-mappings", false, "do not fetch mappings from the server (probe only)")
	asJSON := fs.Bool("json", false, "print JSON instead of a table")
	timeout := fs.Duration("timeout", 8*time.Second, "per-endpoint probe timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	cfgPath := resolveConfigPath(*configPath)
	cfg, err := deps.loadConfig(cfgPath)
	if err != nil {
		if errors.Is(err, config.ErrNotFound) {
			fmt.Fprintln(stderr, "agentctl identity-probe: agent is not paired (run `agentctl pair --code <CODE>` first)")
			return 2
		}
		fmt.Fprintf(stderr, "agentctl identity-probe: load config: %v\n", err)
		return 1
	}

	report := identityProbeReport{Mode: "observe"}
	if cfg.CompanyIdentityEnforce {
		report.Mode = "enforce"
	}
	exit := 0

	var mapped []connMapping
	if !*noMappings {
		conns := cfg.PairedConnections()
		if strings.TrimSpace(*connectionID) != "" {
			conn, cErr := requireOneConnection(cfg, *connectionID)
			if cErr != nil {
				fmt.Fprintf(stderr, "agentctl identity-probe: %v\n", cErr)
				return 2
			}
			conns = []config.PairedConnection{conn}
		}
		ks := deps.newOSKeyring()
		for _, conn := range conns {
			rc := identityProbeConn{ConnectionID: conn.ConnectionID}
			ms, mErr := fetchMappingsFor(conn, firstNonEmpty(*serverFlag, conn.Server), ks, deps)
			if mErr != nil {
				// The message names the connection and the key, never
				// the value; the server's body is already redacted.
				rc.Error = ingest.RedactSecrets(mErr.Error())
				exit = 1
			}
			rc.Mappings = len(ms)
			for _, m := range ms {
				mapped = append(mapped, connMapping{connectionID: conn.ConnectionID, m: m})
			}
			report.Connections = append(report.Connections, rc)
		}
	}

	endpoints := []string(endpointsFlag)
	if len(endpoints) == 0 {
		endpoints = defaultIdentityEndpoints(cfg, mapped, deps)
	}
	if len(endpoints) == 0 {
		fmt.Fprintln(stderr, "agentctl identity-probe: no Tally endpoint to probe (pass --endpoint)")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	probes, err := deps.discover(ctx, tally.DiscoverOptions{
		Endpoints:     endpoints,
		Timeout:       *timeout,
		Concurrency:   4,
		IdentityProbe: true,
	})
	if err != nil {
		fmt.Fprintf(stderr, "agentctl identity-probe: probe: %v\n", err)
		return 1
	}
	for _, p := range probes {
		report.Endpoints = append(report.Endpoints, identityEndpointRow(p, mapped))
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "agentctl identity-probe: encode: %v\n", err)
			return 1
		}
		return exit
	}
	printIdentityReport(stdout, report)
	return exit
}

func fetchMappingsFor(conn config.PairedConnection, server string, ks keyring.Store, deps identityProbeDeps) ([]ingest.ActiveMapping, error) {
	hmacKey, bearerKey := keyring.ConnectionKeys(conn.ConnectionID)
	secret, err := ks.Get(keyring.ServiceName, hmacKey)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", hmacKey, err)
	}
	bearer, err := ks.Get(keyring.ServiceName, bearerKey)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", bearerKey, err)
	}
	client, err := deps.newMappingsFrom(server, conn.ConnectionID, bearer, secret)
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := client.FetchActiveMappings(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetch mappings: %w", err)
	}
	return resp.Mappings, nil
}

// defaultIdentityEndpoints: configured endpoints, every mapped endpoint
// and every port a tally.exe is listening on, deduped.
func defaultIdentityEndpoints(cfg *config.Config, mapped []connMapping, deps identityProbeDeps) []string {
	ms := make([]ingest.ActiveMapping, 0, len(mapped))
	for _, cm := range mapped {
		ms = append(ms, cm.m)
	}
	for _, ep := range cfg.ResolveTallyEndpoints(tally.DefaultEndpoint) {
		ms = append(ms, ingest.ActiveMapping{TallyEndpoint: ep})
	}
	var ports []int
	if deps.listeningPorts != nil {
		ports, _ = deps.listeningPorts()
	}
	return syncrun.CandidateEndpoints(ms, ports)
}

func identityEndpointRow(p tally.ProbeResult, mapped []connMapping) identityProbeEndpoint {
	row := identityProbeEndpoint{
		Endpoint:        p.Endpoint,
		ListOfCompanies: []identityProbeCompany{},
		TypeCompany:     []identityProbeCompany{},
		Mappings:        []identityProbeMapping{},
		ProbesAgree:     syncrun.EndpointProbesAgree(p),
	}
	if p.Err != nil {
		row.Error = p.Err.Error()
	}
	for _, c := range p.Companies {
		row.ListOfCompanies = append(row.ListOfCompanies, identityProbeCompany{Name: c.Name, GUID: c.GUID})
	}
	if p.Identity != nil {
		if p.Identity.Err != nil {
			row.TypeCompanyError = p.Identity.Err.Error()
		}
		row.TypeCompanyWarn = p.Identity.Warnings
		for _, c := range p.Identity.Companies {
			row.TypeCompany = append(row.TypeCompany, identityProbeCompany{Name: c.Name, GUID: c.GUID})
		}
	} else if p.Err == nil {
		row.TypeCompanyError = "not run"
	}
	ep := syncrun.NormalizeEndpoint(p.Endpoint)
	for _, cm := range mapped {
		c := syncrun.CheckMappingIdentity(cm.m, p)
		mappedHere := syncrun.NormalizeEndpoint(cm.m.TallyEndpoint) == ep
		// A mapping is listed on its own port always, elsewhere only
		// when its company shows up there.
		if !mappedHere && (c.Verdict == syncrun.IdentityNotOpen || c.Verdict == syncrun.IdentityUnreachable) {
			continue
		}
		row.Mappings = append(row.Mappings, identityProbeMapping{
			ConnectionID:   cm.connectionID,
			MappingID:      cm.m.MappingID,
			Company:        cm.m.TallyCompanyName,
			MappedEndpoint: cm.m.TallyEndpoint,
			StoredGUID:     strings.TrimSpace(cm.m.TallyCompanyGUID),
			InList:         c.InList,
			GUIDProbeGUIDs: append([]string{}, c.GUIDs...),
			Verdict:        string(c.Verdict),
			Reason:         c.Reason,
		})
	}
	sort.SliceStable(row.Mappings, func(i, j int) bool { return row.Mappings[i].Company < row.Mappings[j].Company })
	return row
}

func printIdentityReport(w io.Writer, r identityProbeReport) {
	fmt.Fprintf(w, "company identity gate: %s (config company_identity_enforce)\n", r.Mode)
	for _, c := range r.Connections {
		if c.Error != "" {
			fmt.Fprintf(w, "connection %s: mappings not fetched: %s\n", c.ConnectionID, c.Error)
			continue
		}
		fmt.Fprintf(w, "connection %s: %d active mapping(s)\n", c.ConnectionID, c.Mappings)
	}
	for _, e := range r.Endpoints {
		fmt.Fprintf(w, "\n== %s\n", e.Endpoint)
		if e.Error != "" {
			fmt.Fprintf(w, "  unreachable: %s\n", e.Error)
			continue
		}
		fmt.Fprintf(w, "  List of Companies (%d):\n", len(e.ListOfCompanies))
		for _, c := range e.ListOfCompanies {
			fmt.Fprintf(w, "    %q\n", c.Name)
		}
		if e.TypeCompanyError != "" {
			fmt.Fprintf(w, "  Type:Company NAME+GUID: FAILED: %s\n", e.TypeCompanyError)
		} else {
			fmt.Fprintf(w, "  Type:Company NAME+GUID (%d):\n", len(e.TypeCompany))
			for _, c := range e.TypeCompany {
				g := c.GUID
				if g == "" {
					g = "(no GUID)"
				}
				fmt.Fprintf(w, "    %-40q %s\n", c.Name, g)
			}
		}
		for _, wn := range e.TypeCompanyWarn {
			fmt.Fprintf(w, "    warning: %s\n", wn)
		}
		agree := "NO"
		if e.ProbesAgree {
			agree = "yes"
		}
		fmt.Fprintf(w, "  probes agree on the company set: %s\n", agree)
		if len(e.Mappings) == 0 {
			continue
		}
		fmt.Fprintln(w, "  mappings:")
		for _, m := range e.Mappings {
			stored := m.StoredGUID
			if stored == "" {
				stored = "(none)"
			}
			guids := strings.Join(m.GUIDProbeGUIDs, ",")
			if guids == "" {
				guids = "-"
			}
			list := "no"
			if m.InList {
				list = "yes"
			}
			fmt.Fprintf(w, "    %s %s %q mapped=%s\n      stored guid: %s\n      in List of Companies: %s   Type:Company guid: %s\n      verdict: %s — %s\n",
				m.ConnectionID, m.MappingID, m.Company, ingest.StatusEndpoint(m.MappedEndpoint), stored, list, guids, m.Verdict, m.Reason)
		}
	}
}
