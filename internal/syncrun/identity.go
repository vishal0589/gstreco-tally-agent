package syncrun

import (
	"fmt"
	"sort"
	"strings"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// Company identity check (QA 04-Nov N1).
//
// Two probes per Tally port: the built-in List of Companies (names only;
// what the gate has always used) and the Type:Company NAME+GUID collection
// (tally.ProbeCompanyIdentity). In observe mode (the default) the gate is
// unchanged and both answers are only reported. In enforce mode (config
// company_identity_enforce: true) a mapping is read only where both probes
// agree and the Type:Company probe returns the mapping's stored GUID for
// its name; any doubt blocks it with a visible reason, never falling back
// to name-only trust.

// IdentityVerdict classifies one mapping against one port's two probes.
type IdentityVerdict string

const (
	// IdentityMatch: both probes list the company, they agree on the
	// port's company set, and the GUID probe returns the stored GUID.
	IdentityMatch IdentityVerdict = "match"
	// IdentityNoStoredGUID: everything agrees, but the mapping has no
	// stored GUID to compare (legacy mapping; repair captures one).
	IdentityNoStoredGUID IdentityVerdict = "no_stored_guid"
	// IdentityGUIDMismatch: the GUID probe returns the company's name
	// with a different GUID — another company with the same name.
	IdentityGUIDMismatch IdentityVerdict = "guid_mismatch"
	// IdentityGUIDNotReturned: the GUID probe lists the name without a
	// GUID.
	IdentityGUIDNotReturned IdentityVerdict = "guid_not_returned"
	// IdentityAmbiguous: the GUID probe returns the name with several
	// GUIDs.
	IdentityAmbiguous IdentityVerdict = "ambiguous"
	// IdentityProbesDisagree: one probe lists the company and the other
	// doesn't, or the two list different company sets on this port.
	IdentityProbesDisagree IdentityVerdict = "probes_disagree"
	// IdentityUnavailable: the GUID probe did not run or failed.
	IdentityUnavailable IdentityVerdict = "identity_unavailable"
	// IdentityNotOpen: neither probe lists the company on this port.
	IdentityNotOpen IdentityVerdict = "not_open"
	// IdentityUnreachable: List of Companies did not answer.
	IdentityUnreachable IdentityVerdict = "unreachable"
)

// isDoubt reports whether the verdict is an identity doubt: the company
// might be here, but the agent can't prove it is the mapped one.
func (v IdentityVerdict) isDoubt() bool {
	switch v {
	case IdentityMatch, IdentityNotOpen, IdentityUnreachable:
		return false
	}
	return true
}

// MappingIdentity is one mapping checked against one port.
type MappingIdentity struct {
	MappingID string
	Company   string
	Endpoint  string
	// InList: List of Companies names the company.
	InList bool
	// GUIDs the Type:Company probe returned for the company's name.
	GUIDs []string
	// StoredGUID is the mapping's tally_company_guid ("" = none).
	StoredGUID string
	Verdict    IdentityVerdict
	// Reason is a one-line operator explanation.
	Reason string
	// Open is the port's List-of-Companies answer with GUIDs from the
	// Type:Company probe (tally.ProbeResult.CompaniesWithGUID); nil when unreachable.
	Open []tally.TallyCompany
}

// Summary is the check in one short clause for connection_status
// detail: verdict, what each probe said and the stored GUID.
func (c MappingIdentity) Summary() string {
	list := "no"
	if c.InList {
		list = "yes"
	}
	guid := "none"
	if len(c.GUIDs) > 0 {
		short := make([]string, 0, len(c.GUIDs))
		for _, g := range c.GUIDs {
			short = append(short, ShortGUID(g))
		}
		guid = strings.Join(short, "/")
	}
	return fmt.Sprintf("%q @%s %s (list: %s, guid probe: %s, stored: %s)",
		c.Company, ingest.StatusEndpoint(c.Endpoint), c.Verdict, list, guid, ShortGUID(c.StoredGUID))
}

// EndpointProbesAgree reports whether a port's two probes list the same
// company names. False when either probe did not answer.
func EndpointProbesAgree(p tally.ProbeResult) bool {
	if p.Err != nil || p.Identity == nil || p.Identity.Err != nil {
		return false
	}
	return sameNames(p.Companies, p.Identity.Companies)
}

func sameNames(a, b []tally.TallyCompany) bool {
	set := func(cs []tally.TallyCompany) map[string]bool {
		m := map[string]bool{}
		for _, c := range cs {
			if n := strings.TrimSpace(c.Name); n != "" {
				m[n] = true
			}
		}
		return m
	}
	sa, sb := set(a), set(b)
	if len(sa) != len(sb) {
		return false
	}
	for n := range sa {
		if !sb[n] {
			return false
		}
	}
	return true
}

// CheckMappingIdentity compares one mapping with one port's probes.
func CheckMappingIdentity(m ingest.ActiveMapping, p tally.ProbeResult) MappingIdentity {
	name := strings.TrimSpace(m.TallyCompanyName)
	out := MappingIdentity{
		MappingID:  m.MappingID,
		Company:    name,
		Endpoint:   normalizeEndpoint(p.Endpoint),
		StoredGUID: strings.TrimSpace(m.TallyCompanyGUID),
	}
	ep := ingest.StatusEndpoint(out.Endpoint)
	if p.Err != nil {
		out.Verdict = IdentityUnreachable
		out.Reason = fmt.Sprintf("the Tally at %s did not answer", ep)
		return out
	}
	out.Open = p.CompaniesWithGUID()
	for _, c := range p.Companies {
		if strings.TrimSpace(c.Name) == name {
			out.InList = true
		}
	}
	if p.Identity == nil || p.Identity.Err != nil {
		out.Verdict = IdentityUnavailable
		why := "was not run"
		if p.Identity != nil {
			why = "failed: " + p.Identity.Err.Error()
		}
		out.Reason = fmt.Sprintf("the company GUID probe on %s %s; cannot verify %q is the mapped company", ep, why, name)
		if !out.InList {
			out.Verdict = IdentityNotOpen
			out.Reason = fmt.Sprintf("%q is not open on %s", name, ep)
		}
		return out
	}
	for _, c := range p.Identity.Companies {
		if strings.TrimSpace(c.Name) == name {
			out.GUIDs = append(out.GUIDs, strings.TrimSpace(c.GUID))
		}
	}
	inGUID := len(out.GUIDs) > 0
	switch {
	case !out.InList && !inGUID:
		out.Verdict = IdentityNotOpen
		out.Reason = fmt.Sprintf("%q is not open on %s", name, ep)
	case out.InList != inGUID:
		out.Verdict = IdentityProbesDisagree
		if out.InList {
			out.Reason = fmt.Sprintf("List of Companies on %s shows %q but the company GUID probe does not; identity unverified", ep, name)
		} else {
			out.Reason = fmt.Sprintf("the company GUID probe on %s shows %q but List of Companies does not; identity unverified", ep, name)
		}
	case len(out.GUIDs) > 1:
		out.Verdict = IdentityAmbiguous
		out.Reason = fmt.Sprintf("the company GUID probe on %s returns %q with %d different GUIDs; not guessing which is the mapped company", ep, name, len(out.GUIDs))
	case out.GUIDs[0] == "":
		out.Verdict = IdentityGUIDNotReturned
		out.Reason = fmt.Sprintf("the company GUID probe on %s returns %q without a GUID; identity unverified", ep, name)
	case out.StoredGUID != "" && !strings.EqualFold(out.GUIDs[0], out.StoredGUID):
		out.Verdict = IdentityGUIDMismatch
		out.Reason = fmt.Sprintf("%q on %s has Tally company GUID %s, not the mapping's %s (another company with the same name); not reading it", name, ep, ShortGUID(out.GUIDs[0]), ShortGUID(out.StoredGUID))
	case !sameNames(p.Companies, p.Identity.Companies):
		out.Verdict = IdentityProbesDisagree
		out.Reason = fmt.Sprintf("List of Companies and the company GUID probe on %s list different companies (%s vs %s); identity unverified", ep, companyNames(p.Companies), companyNames(p.Identity.Companies))
	case out.StoredGUID == "":
		out.Verdict = IdentityNoStoredGUID
		out.Reason = fmt.Sprintf("mapping %s for %q has no stored Tally company GUID (Tally on %s reports %s); re-run setup/repair so it captures one", m.MappingID, name, ep, ShortGUID(out.GUIDs[0]))
	default:
		out.Verdict = IdentityMatch
		out.Reason = fmt.Sprintf("%q on %s matches the mapping's GUID %s", name, ep, ShortGUID(out.StoredGUID))
	}
	return out
}

// ShortGUID is the first 8 characters of a GUID, for one-line detail
// (full GUIDs go in open_companies).
func ShortGUID(g string) string {
	g = strings.TrimSpace(g)
	if g == "" {
		return "none"
	}
	if len(g) > 8 {
		return g[:8] + "…"
	}
	return g
}

func companyNames(cs []tally.TallyCompany) string {
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, fmt.Sprintf("%q", strings.TrimSpace(c.Name)))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// ObserveIdentity checks each mapping against the port it was (or would
// have been) read from: FetchEndpoint when followed, else the mapped one.
// Reporting only; the gate is unchanged.
func ObserveIdentity(mappings []ingest.ActiveMapping, probes []tally.ProbeResult) []MappingIdentity {
	byEndpoint := map[string]tally.ProbeResult{}
	for _, p := range probes {
		byEndpoint[normalizeEndpoint(p.Endpoint)] = p
	}
	out := make([]MappingIdentity, 0, len(mappings))
	for _, m := range mappings {
		ep := normalizeEndpoint(m.ReadEndpoint())
		p, ok := byEndpoint[ep]
		if !ok {
			out = append(out, MappingIdentity{
				MappingID: m.MappingID, Company: strings.TrimSpace(m.TallyCompanyName),
				Endpoint: ep, StoredGUID: strings.TrimSpace(m.TallyCompanyGUID),
				Verdict: IdentityUnreachable,
				Reason:  fmt.Sprintf("%s was not probed", ingest.StatusEndpoint(ep)),
			})
			continue
		}
		out = append(out, CheckMappingIdentity(m, p))
	}
	return out
}

// ResolveMappingEndpointsEnforced is ResolveMappingEndpoints with the
// company GUID probe as the gate. A mapping is read on its mapped port when
// that port's verdict is IdentityMatch; else on the one other port that
// matches, provided no port shows an identity doubt for it. Any doubt —
// probes disagree, the GUID probe failed or returned no GUID, a stored GUID
// not returned, same name with a different GUID, or no stored GUID at all —
// skips the mapping with that reason.
func ResolveMappingEndpointsEnforced(mappings []ingest.ActiveMapping, probes []tally.ProbeResult) (resolved []ingest.ActiveMapping, followed []FollowedMapping, skipped []SkippedMapping, checks []MappingIdentity) {
	byEndpoint := map[string]tally.ProbeResult{}
	var order []string
	for _, p := range probes {
		ep := normalizeEndpoint(p.Endpoint)
		if _, dup := byEndpoint[ep]; !dup {
			order = append(order, ep)
		}
		byEndpoint[ep] = p
	}
	sort.Strings(order)

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
		skip := func(c MappingIdentity) {
			sk := SkippedMapping{Mapping: m, Endpoint: m.TallyEndpoint, Reason: "identity check (enforced): " + c.Reason, Result: ingest.ConnStatusWrongCompanyOpen}
			if c.Verdict == IdentityNoStoredGUID {
				sk.Result = ingest.ConnStatusMappingMismatch
			}
			if p, ok := byEndpoint[normalizeEndpoint(c.Endpoint)]; ok && p.Err == nil {
				sk.OpenCompanies = p.CompaniesWithGUID()
			}
			skipped = append(skipped, sk)
			checks = append(checks, c)
		}

		var mappedCheck *MappingIdentity
		if p, ok := byEndpoint[mapped]; ok {
			c := CheckMappingIdentity(m, p)
			mappedCheck = &c
			if c.Verdict == IdentityMatch {
				resolved = append(resolved, m)
				checks = append(checks, c)
				continue
			}
			if c.Verdict.isDoubt() {
				skip(c)
				continue
			}
		}
		var matches []MappingIdentity
		var doubt *MappingIdentity
		for _, ep := range order {
			if ep == mapped {
				continue
			}
			c := CheckMappingIdentity(m, byEndpoint[ep])
			switch {
			case c.Verdict == IdentityMatch:
				matches = append(matches, c)
			case c.Verdict.isDoubt() && doubt == nil:
				doubt = &c
			}
		}
		switch {
		case doubt != nil:
			skip(*doubt)
		case len(matches) == 1:
			moved := m
			moved.FetchEndpoint = matches[0].Endpoint
			resolved = append(resolved, moved)
			followed = append(followed, FollowedMapping{Mapping: m, From: m.TallyEndpoint, To: matches[0].Endpoint})
			checks = append(checks, matches[0])
		case len(matches) > 1:
			where := make([]string, 0, len(matches))
			for _, c := range matches {
				where = append(where, c.Endpoint)
			}
			sk := SkippedMapping{
				Mapping:  m,
				Endpoint: m.TallyEndpoint,
				Result:   ingest.ConnStatusWrongCompanyOpen,
				Reason:   fmt.Sprintf("%q (GUID %s) is open on several Tally ports (%s), none of them its mapped %s; not guessing which copy holds the books", name, ShortGUID(m.TallyCompanyGUID), strings.Join(where, ", "), mapped),
			}
			if p, ok := byEndpoint[mapped]; ok && p.Err == nil {
				sk.OpenCompanies = p.CompaniesWithGUID()
			}
			skipped = append(skipped, sk)
			if mappedCheck != nil {
				checks = append(checks, *mappedCheck)
			}
		default:
			sk := SkippedMapping{Mapping: m, Endpoint: m.TallyEndpoint, Reason: notOpenReason(name, mapped, byEndpoint)}
			if p, probed := byEndpoint[mapped]; probed && p.Err == nil {
				sk.Result = ingest.ConnStatusWrongCompanyOpen
				sk.OpenCompanies = p.CompaniesWithGUID()
			} else {
				sk.Result = ingest.ConnStatusEndpointUnreachable
			}
			skipped = append(skipped, sk)
			if mappedCheck != nil {
				checks = append(checks, *mappedCheck)
			}
		}
	}
	return resolved, followed, skipped, checks
}
