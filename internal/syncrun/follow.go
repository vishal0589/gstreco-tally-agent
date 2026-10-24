package syncrun

import (
	"fmt"
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
//   - It isn't, and exactly one other port has it open: read it there.
//   - It is open on several other ports: skip; two copies may hold
//     different data, and the agent must not guess which is the books.
//   - It is open nowhere: skip, saying so (and what the mapped port has).
//
// Names match exactly after trimming, as Tally shows them.
func ResolveMappingEndpoints(mappings []ingest.ActiveMapping, probes []tally.ProbeResult) (resolved []ingest.ActiveMapping, followed []FollowedMapping, skipped []SkippedMapping) {
	byEndpoint := map[string]tally.ProbeResult{}
	openOn := map[string][]string{}
	for _, p := range probes {
		ep := normalizeEndpoint(p.Endpoint)
		byEndpoint[ep] = p
		if p.Err != nil {
			continue
		}
		for _, c := range p.Companies {
			name := strings.TrimSpace(c.Name)
			if name != "" && !contains(openOn[name], ep) {
				openOn[name] = append(openOn[name], ep)
			}
		}
	}
	for name := range openOn {
		sort.Strings(openOn[name])
	}

	for _, m := range mappings {
		name := strings.TrimSpace(m.TallyCompanyName)
		mapped := normalizeEndpoint(m.TallyEndpoint)
		where := openOn[name]
		if contains(where, mapped) {
			resolved = append(resolved, m)
			continue
		}
		switch len(where) {
		case 1:
			moved := m
			moved.TallyEndpoint = where[0]
			resolved = append(resolved, moved)
			followed = append(followed, FollowedMapping{Mapping: m, From: m.TallyEndpoint, To: where[0]})
		case 0:
			skipped = append(skipped, SkippedMapping{Mapping: m, Reason: notOpenReason(name, mapped, byEndpoint)})
		default:
			skipped = append(skipped, SkippedMapping{
				Mapping: m,
				Reason:  fmt.Sprintf("%q is open on several Tally ports (%s), none of them its mapped %s; not guessing which copy holds the books", name, strings.Join(where, ", "), mapped),
			})
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

// normalizeEndpoint makes endpoints comparable: lower-case scheme and host,
// localhost as 127.0.0.1, no trailing slash.
func normalizeEndpoint(ep string) string {
	ep = strings.TrimRight(strings.TrimSpace(strings.ToLower(ep)), "/")
	return strings.Replace(ep, "://localhost:", "://127.0.0.1:", 1)
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
