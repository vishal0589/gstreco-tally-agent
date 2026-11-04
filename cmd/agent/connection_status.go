package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/syncrun"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// connStatusTracker is ONE connection's last result (QA v29 L19). Every
// daemonConnectionRuntime owns its own; nothing in it is shared across
// connections, so one connection's 401 or Tally failure never shows on
// another connection's heartbeat. The agent-wide tickState stays for the
// legacy last_tick_status field only.
type connStatusTracker struct {
	mu sync.Mutex
	// sync is the last sync result for this connection (scheduled tick
	// or sync_now), nil until one has run.
	sync *ingest.ConnectionStatus
	// heartbeatAuth is set while this connection's own heartbeat is
	// being rejected (401/403) and cleared by its next accepted one.
	heartbeatAuth *ingest.ConnectionStatus
	now           func() time.Time
}

func newConnStatusTracker() *connStatusTracker {
	return &connStatusTracker{now: func() time.Time { return time.Now().UTC() }}
}

func (t *connStatusTracker) checkedAt() string {
	return t.now().UTC().Format(time.RFC3339)
}

// recordSync stores this connection's latest sync result.
func (t *connStatusTracker) recordSync(st ingest.ConnectionStatus) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st.CheckedAt == "" {
		st.CheckedAt = t.checkedAt()
	}
	t.sync = &st
}

// recordHeartbeat is the heartbeat poller's OnHeartbeatResult for this
// connection. Network errors change nothing (the server said nothing).
func (t *connStatusTracker) recordHeartbeat(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		t.heartbeatAuth = nil
		return
	}
	status, code, ok := ingest.AuthFailure(err)
	if !ok {
		return
	}
	st := ingest.ConnectionStatus{
		Result:     ingest.ConnStatusRejectedAuth,
		CheckedAt:  t.checkedAt(),
		HTTPStatus: &status,
		ErrorCode:  ingest.OptString(code),
		Detail:     ingest.OptString(authDetail("heartbeat", status, code)),
	}
	t.heartbeatAuth = &st
}

// snapshot is the connection_status for this connection's next
// heartbeat: a live heartbeat rejection wins, then the last sync result,
// else idle.
func (t *connStatusTracker) snapshot() *ingest.ConnectionStatus {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	src := t.sync
	if t.heartbeatAuth != nil {
		src = t.heartbeatAuth
	}
	if src == nil {
		return &ingest.ConnectionStatus{Result: ingest.ConnStatusIdle, CheckedAt: t.checkedAt()}
	}
	out := *src
	out.OpenCompanies = append([]ingest.StatusCompany(nil), src.OpenCompanies...)
	if src.OpenCompanies == nil {
		out.OpenCompanies = nil
	}
	return &out
}

func authDetail(what string, status int, code string) string {
	d := fmt.Sprintf("server rejected this connection's credentials on %s (HTTP %d", what, status)
	if code != "" {
		d += " " + code
	}
	return d + "); re-pair this connection"
}

func intPtr(v int) *int { return &v }

// syncOutcome accumulates one connection's sync into a ConnectionStatus.
type syncOutcome struct {
	mappings  []ingest.ActiveMapping
	active    []ingest.ActiveMapping
	skipped   []syncrun.SkippedMapping
	authErr   error  // first 401/403 from the server during the sync
	authWhat  string // which call got it
	otherErr  error  // non-auth, non-routing failure (walk, fetch)
	fetchErr  error  // FetchActiveMappings failure
	noMapping bool
	// identity is the company identity check per mapping (QA 04-Nov
	// N1), reported in detail and open_companies.
	identity        []syncrun.MappingIdentity
	identityEnforce bool
}

// identityNote is the identity checks as one detail clause, "" when none.
func (o *syncOutcome) identityNote() string {
	if len(o.identity) == 0 {
		return ""
	}
	parts := make([]string, 0, len(o.identity))
	for _, c := range o.identity {
		parts = append(parts, c.Summary())
	}
	return "identity[" + identityModeName(o.identityEnforce) + "]: " + strings.Join(parts, "; ")
}

// withIdentity appends the identity note to a detail string.
func (o *syncOutcome) withIdentity(detail string) string {
	note := o.identityNote()
	switch {
	case note == "":
		return detail
	case detail == "":
		return note
	default:
		return detail + " | " + note
	}
}

// identityOpenCompanies is the probed port's companies (with GUIDs from
// the Type:Company probe) when the connection reads exactly one mapping.
func (o *syncOutcome) identityOpenCompanies() []ingest.StatusCompany {
	if len(o.identity) != 1 || o.identity[0].Open == nil {
		return nil
	}
	return statusCompanies(o.identity[0].Open)
}

func (o *syncOutcome) noteAuth(err error, what string) bool {
	if _, _, ok := ingest.AuthFailure(err); !ok {
		return false
	}
	if o.authErr == nil {
		o.authErr, o.authWhat = err, what
	}
	return true
}

func statusCompanies(cs []tally.TallyCompany) []ingest.StatusCompany {
	out := make([]ingest.StatusCompany, 0, len(cs))
	for _, c := range cs {
		out = append(out, ingest.NewStatusCompany(c.Name, c.GUID))
	}
	return out
}

func expectedCompany(m ingest.ActiveMapping) *ingest.StatusCompany {
	c := ingest.NewStatusCompany(m.TallyCompanyName, m.TallyCompanyGUID)
	return &c
}

// status turns the outcome into this connection's connection_status.
// Priority: auth rejection, then the mapping list, then company routing,
// then other failures, then success.
func (o *syncOutcome) status() ingest.ConnectionStatus {
	switch {
	case o.authErr != nil:
		st, code, _ := ingest.AuthFailure(o.authErr)
		out := ingest.ConnectionStatus{
			Result:     ingest.ConnStatusRejectedAuth,
			HTTPStatus: intPtr(st),
			ErrorCode:  ingest.OptString(code),
			Detail:     ingest.OptString(authDetail(o.authWhat, st, code)),
		}
		o.fillSingleMapping(&out)
		return out
	case o.fetchErr != nil:
		out := ingest.ConnectionStatus{
			Result: ingest.ConnStatusMappingMismatch,
			Detail: ingest.OptString(ingest.RedactDetail("could not fetch active mappings: " + o.fetchErr.Error())),
		}
		if se := ingest.IsSendError(o.fetchErr); se != nil && se.Status != 0 {
			out.HTTPStatus = intPtr(se.Status)
			out.ErrorCode = ingest.OptString(ingest.ErrorCodeFromSnippet(se.Snippet))
		}
		return out
	case o.noMapping:
		return ingest.ConnectionStatus{
			Result:     ingest.ConnStatusMappingMismatch,
			HTTPStatus: intPtr(200),
			Detail:     ingest.OptString("no active mapping for this connection; map its Tally company in /settings/tally"),
		}
	case len(o.skipped) > 0:
		first := o.skipped[0]
		out := ingest.ConnectionStatus{
			Result:          first.Result,
			HTTPStatus:      intPtr(200),
			TallyEndpoint:   ingest.OptString(ingest.StatusEndpoint(first.Endpoint)),
			MappingID:       ingest.OptString(first.Mapping.MappingID),
			ExpectedCompany: expectedCompany(first.Mapping),
		}
		if first.Result == "" {
			out.Result = ingest.ConnStatusMappingMismatch
		}
		if first.Result == ingest.ConnStatusWrongCompanyOpen {
			out.OpenCompanies = statusCompanies(first.OpenCompanies)
		}
		reasons := make([]string, 0, len(o.skipped))
		for _, sk := range o.skipped {
			reasons = append(reasons, sk.Reason)
		}
		detail := strings.Join(reasons, "; ")
		if len(o.skipped) > 1 {
			detail = fmt.Sprintf("%d of %d mapped companies not read: %s", len(o.skipped), len(o.mappings), detail)
		}
		if !o.identityEnforce {
			// Enforced skips already say why; observe adds what the
			// GUID probe saw.
			detail = o.withIdentity(detail)
		}
		out.Detail = ingest.OptString(ingest.RedactDetail(detail))
		return out
	case o.otherErr != nil:
		out := ingest.ConnectionStatus{
			Result:     ingest.ConnStatusAcceptedTick,
			HTTPStatus: intPtr(200),
			Detail:     ingest.OptString(ingest.RedactDetail(o.withIdentity(o.otherErr.Error()))),
		}
		o.fillSingleMapping(&out)
		out.OpenCompanies = o.identityOpenCompanies()
		return out
	default:
		out := ingest.ConnectionStatus{Result: ingest.ConnStatusSuccess, HTTPStatus: intPtr(200)}
		o.fillSingleMapping(&out)
		detail := ""
		if len(o.active) > 1 {
			detail = fmt.Sprintf("%d mapped companies read", len(o.active))
		}
		out.Detail = ingest.OptString(ingest.RedactDetail(o.withIdentity(detail)))
		out.OpenCompanies = o.identityOpenCompanies()
		return out
	}
}

// fillSingleMapping names the mapping and endpoint when the connection
// has exactly one active mapping (the common case); with several, the
// per-mapping detail is in the agent log.
func (o *syncOutcome) fillSingleMapping(out *ingest.ConnectionStatus) {
	ms := o.active
	if len(ms) == 0 {
		ms = o.mappings
	}
	if len(ms) != 1 {
		return
	}
	m := ms[0]
	out.MappingID = ingest.OptString(m.MappingID)
	out.TallyEndpoint = ingest.OptString(ingest.StatusEndpoint(m.ReadEndpoint()))
	out.ExpectedCompany = expectedCompany(m)
}

// walkAuthError returns the first server auth rejection in a per-kind
// result, if any.
func walkAuthError(r syncrun.Result, fatalErr error) error {
	if _, _, ok := ingest.AuthFailure(fatalErr); ok {
		return fatalErr
	}
	for _, be := range r.BatchErrors {
		if _, _, ok := ingest.AuthFailure(be.Err); ok {
			return be.Err
		}
	}
	return nil
}
