package syncrun

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// FollowedMapping is a mapping read from a different Tally port than the
// one it is mapped to, because that is where its company is open now.
type FollowedMapping struct {
	Mapping ingest.ActiveMapping
	From    string
	To      string
}

// SkippedMapping is a mapping not read this tick, with the reason.
type SkippedMapping struct {
	Mapping ingest.ActiveMapping
	Reason  string
	// Result classifies the skip with the connection_status result
	// names: ingest.ConnStatusWrongCompanyOpen,
	// ingest.ConnStatusEndpointUnreachable or
	// ingest.ConnStatusMappingMismatch.
	Result string
	// Endpoint is the mapped endpoint the decision was made for.
	Endpoint string
	// OpenCompanies is what the Tally at Endpoint reported open (nil
	// when it did not answer).
	OpenCompanies []tally.TallyCompany
}

// ResolveMappingEndpoints decides which Tally port to read for each mapping
// this tick, from a fresh probe of the machine's Tally ports.
//
// A company is often not where its mapping says: Tally instances on a shared
// server start and stop through the day, two companies share one install,
// and a company opened in a second copy of an install moves to whichever
// copy holds the port. Reading only the mapped port meant a company open on
// another port was never synced, and one that was open nowhere produced a
// failed request per voucher kind with no plain explanation.
//
//   - The company is open on its mapped port: read it there.
//   - It isn't, and exactly one other port has it open: read it there
//     (FetchEndpoint). Ingest keeps reporting the mapped endpoint, which is
//     how the server finds the mapping and its GSTIN.
//   - It is open on several other ports: skip; two copies may hold
//     different data, and the agent must not guess which is the books.
//   - It is open nowhere: skip, saying so (and what the mapped port has).
//
// Names match exactly after trimming, as Tally shows them. When both the
// mapping and the open company carry a Tally company GUID and they differ,
// it is another company with the same name, never followed.
func ResolveMappingEndpoints(mappings []ingest.ActiveMapping, probes []tally.ProbeResult) (resolved []ingest.ActiveMapping, followed []FollowedMapping, skipped []SkippedMapping) {
	type openCopy struct{ endpoint, guid string }
	byEndpoint := map[string]tally.ProbeResult{}
	openOn := map[string][]openCopy{}
	for _, p := range probes {
		ep := normalizeEndpoint(p.Endpoint)
		byEndpoint[ep] = p
		if p.Err != nil {
			continue
		}
		for _, c := range p.Companies {
			name := strings.TrimSpace(c.Name)
			if name == "" {
				continue
			}
			dup := false
			for _, oc := range openOn[name] {
				dup = dup || oc.endpoint == ep
			}
			if !dup {
				openOn[name] = append(openOn[name], openCopy{endpoint: ep, guid: strings.TrimSpace(c.GUID)})
			}
		}
	}

	for _, m := range mappings {
		name := strings.TrimSpace(m.TallyCompanyName)
		mapped := normalizeEndpoint(m.TallyEndpoint)
		if name == "" || mapped == "" {
			skipped = append(skipped, SkippedMapping{
				Mapping:  m,
				Reason:   fmt.Sprintf("mapping %s has no Tally company name or endpoint; fix it in /settings/tally", m.MappingID),
				Result:   ingest.ConnStatusMappingMismatch,
				Endpoint: m.TallyEndpoint,
			})
			continue
		}
		wantGUID := strings.TrimSpace(m.TallyCompanyGUID)
		var where []string
		onMapped := false
		guidMismatchOnMapped := false
		for _, oc := range openOn[name] {
			// Same name but a different Tally company GUID is another
			// company — on the mapped port as much as anywhere else.
			if wantGUID != "" && oc.guid != "" && !strings.EqualFold(oc.guid, wantGUID) {
				if oc.endpoint == mapped {
					guidMismatchOnMapped = true
				}
				continue
			}
			if oc.endpoint == mapped {
				onMapped = true
				break
			}
			where = append(where, oc.endpoint)
		}
		sort.Strings(where)
		if onMapped {
			resolved = append(resolved, m)
			continue
		}
		switch len(where) {
		case 1:
			moved := m
			moved.FetchEndpoint = where[0]
			resolved = append(resolved, moved)
			followed = append(followed, FollowedMapping{Mapping: m, From: m.TallyEndpoint, To: where[0]})
		case 0:
			sk := SkippedMapping{Mapping: m, Endpoint: m.TallyEndpoint, Reason: notOpenReason(name, mapped, byEndpoint)}
			p, probed := byEndpoint[mapped]
			switch {
			case probed && p.Err == nil:
				sk.Result = ingest.ConnStatusWrongCompanyOpen
				sk.OpenCompanies = p.CompaniesWithGUID()
				if guidMismatchOnMapped {
					sk.Reason = fmt.Sprintf("%q on %s has a different Tally company GUID than the mapping (another company with the same name); not reading it", name, mapped)
				}
			default:
				sk.Result = ingest.ConnStatusEndpointUnreachable
			}
			skipped = append(skipped, sk)
		default:
			sk := SkippedMapping{
				Mapping:  m,
				Endpoint: m.TallyEndpoint,
				Result:   ingest.ConnStatusWrongCompanyOpen,
				Reason:   fmt.Sprintf("%q is open on several Tally ports (%s), none of them its mapped %s; not guessing which copy holds the books", name, strings.Join(where, ", "), mapped),
			}
			if p, ok := byEndpoint[mapped]; ok && p.Err == nil {
				sk.OpenCompanies = p.CompaniesWithGUID()
			}
			skipped = append(skipped, sk)
		}
	}
	return resolved, followed, skipped
}

func notOpenReason(name, mapped string, byEndpoint map[string]tally.ProbeResult) string {
	p, probed := byEndpoint[mapped]
	switch {
	case !probed:
		return fmt.Sprintf("%q is not open in any Tally on this machine", name)
	case p.Err != nil:
		return fmt.Sprintf("%q is not open in any Tally on this machine; the Tally at %s did not answer (%v)", name, mapped, p.Err)
	case len(p.Companies) == 0:
		return fmt.Sprintf("%q is not open in any Tally on this machine; the Tally at %s has no company open", name, mapped)
	default:
		open := make([]string, 0, len(p.Companies))
		for _, c := range p.Companies {
			open = append(open, c.Name)
		}
		return fmt.Sprintf("%q is not open in any Tally on this machine; the Tally at %s has %s open", name, mapped, strings.Join(open, ", "))
	}
}

// normalizeEndpoint makes endpoints comparable: lower case, no trailing
// slash, and every name for this machine (localhost, loopback addresses,
// 0.0.0.0, its host name) as 127.0.0.1, so one local Tally is never counted
// as two copies or probed twice.
func normalizeEndpoint(ep string) string {
	ep = strings.TrimRight(strings.TrimSpace(strings.ToLower(ep)), "/")
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return ep
	}
	if isLocalHost(u.Hostname()) {
		port := u.Port()
		u.Host = "127.0.0.1"
		if port != "" {
			u.Host += ":" + port
		}
	}
	return u.String()
}

// NormalizeEndpoint is normalizeEndpoint for callers outside the package
// (agentctl identity-probe matches mappings to probed ports with it).
func NormalizeEndpoint(ep string) string { return normalizeEndpoint(ep) }

func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "0.0.0.0":
		return true
	}
	if name, err := os.Hostname(); err == nil && strings.EqualFold(host, name) {
		return true
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CandidateEndpoints is every endpoint worth probing: the mapped ones plus
// each port a tally.exe is listening on. Loopback only.
func CandidateEndpoints(mappings []ingest.ActiveMapping, tallyPorts []int) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ep string) {
		ep = normalizeEndpoint(ep)
		if ep != "" && !seen[ep] {
			seen[ep] = true
			out = append(out, ep)
		}
	}
	for _, m := range mappings {
		add(m.TallyEndpoint)
	}
	for _, port := range tallyPorts {
		add(fmt.Sprintf("http://127.0.0.1:%d", port))
	}
	return out
}
