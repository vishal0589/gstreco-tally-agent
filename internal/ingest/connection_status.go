package ingest

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Connection status results (QA v29 L19 shared wire contract). Each paired
// connection reports its own last result on its own heartbeat, so one
// connection's 401 or Tally failure is never shown as another's.
const (
	// ConnStatusSuccess: the last sync for this connection read every
	// mapped company and every batch landed.
	ConnStatusSuccess = "success"
	// ConnStatusAcceptedTick: the server accepted this connection's
	// credentials and the sync ran, but some batches or kinds failed for a
	// reason other than auth or company routing.
	ConnStatusAcceptedTick = "accepted_tick"
	// ConnStatusRejectedAuth: the server rejected this connection's
	// bearer/HMAC (401/403).
	ConnStatusRejectedAuth = "rejected_auth"
	// ConnStatusMappingMismatch: no active mapping, an invalid one, or
	// the mapping list could not be fetched.
	ConnStatusMappingMismatch = "mapping_mismatch"
	// ConnStatusEndpointUnreachable: the Tally port refused or timed out.
	ConnStatusEndpointUnreachable = "endpoint_unreachable"
	// ConnStatusWrongCompanyOpen: Tally answered, but the mapped company
	// (name + GUID) is not the one open there.
	ConnStatusWrongCompanyOpen = "wrong_company_open"
	// ConnStatusIdle: no sync has run for this connection yet.
	ConnStatusIdle = "idle"
)

// MaxConnectionStatusDetail caps ConnectionStatus.Detail (contract: <= 300).
const MaxConnectionStatusDetail = 300

// StatusCompany names a Tally company in a ConnectionStatus.
type StatusCompany struct {
	Name *string `json:"name"`
	GUID *string `json:"guid"`
}

// NewStatusCompany builds a StatusCompany; blank values become null.
func NewStatusCompany(name, guid string) StatusCompany {
	return StatusCompany{Name: optString(name), GUID: optString(guid)}
}

// ConnectionStatus is the per-connection heartbeat field
// `connection_status`. Built only from this connection's own last result.
// Never carries a bearer, HMAC secret or keyring value.
type ConnectionStatus struct {
	Result          string          `json:"result"`
	CheckedAt       string          `json:"checked_at"`
	TallyEndpoint   *string         `json:"tally_endpoint"`
	MappingID       *string         `json:"mapping_id"`
	ExpectedCompany *StatusCompany  `json:"expected_company"`
	OpenCompanies   []StatusCompany `json:"open_companies"`
	HTTPStatus      *int            `json:"http_status"`
	ErrorCode       *string         `json:"error_code"`
	Detail          *string         `json:"detail"`
	SyncRequestID   *string         `json:"sync_request_id"`
}

func optString(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// OptString returns nil for a blank string, else a pointer to the trimmed
// value. Exported for building ConnectionStatus values.
func OptString(s string) *string { return optString(s) }

// StatusEndpoint renders a Tally endpoint as "host:port" — no scheme,
// credentials or path — for ConnectionStatus.TallyEndpoint.
func StatusEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	raw := endpoint
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		// Last resort: drop anything that could be credentials or a path.
		if i := strings.LastIndex(endpoint, "@"); i >= 0 {
			endpoint = endpoint[i+1:]
		}
		if i := strings.IndexAny(endpoint, "/?#"); i >= 0 {
			endpoint = endpoint[:i]
		}
		return endpoint
	}
	return u.Host // url.URL.Host never includes userinfo
}

var (
	bearerPattern = regexp.MustCompile(`(?i)bearer\s+[^\s"',;]+`)
	// Long opaque runs (base64 / hex / url-safe tokens) are almost never
	// useful detail and could be a secret echoed back by a proxy.
	opaquePattern = regexp.MustCompile(`[A-Za-z0-9+/_=\-]{32,}`)
	uuidPattern   = regexp.MustCompile(`^[0-9a-fA-F]{8}(-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}$`)
	secretKV      = regexp.MustCompile(`(?i)("?(?:bearer_token|bearer|access_token|token|secret|hmac_secret|hmac|authorization|signature|x-tally-signature)"?\s*[:=]\s*)("[^"]*"|[^\s,;}]+)`)
	errorCodeRe   = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)
)

// RedactSecrets removes anything token-shaped from s: each value in
// secrets verbatim (the connection's own bearer, for example), "Bearer
// <x>" pairs, token/secret/signature key-value pairs, and long opaque
// runs (UUIDs are kept: they are request ids and company GUIDs). Applied
// to every server error body the client keeps, so logs and heartbeat
// detail never carry a credential even if a proxy echoes one back.
func RedactSecrets(s string, secrets ...string) string {
	for _, sec := range secrets {
		// Real bearers and secrets are long; a short value (a test
		// fixture's "b") would otherwise shred ordinary words.
		if len(strings.TrimSpace(sec)) >= 8 {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	s = bearerPattern.ReplaceAllString(s, "Bearer [redacted]")
	s = secretKV.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretKV.FindStringSubmatch(m)
		if strings.HasPrefix(sub[2], `"`) {
			return sub[1] + `"[redacted]"`
		}
		return sub[1] + "[redacted]"
	})
	s = opaquePattern.ReplaceAllStringFunc(s, func(m string) string {
		if uuidPattern.MatchString(m) {
			return m
		}
		return "[redacted]"
	})
	return s
}

// RedactDetail is RedactSecrets plus whitespace folding and the
// MaxConnectionStatusDetail cap. Use it on every ConnectionStatus.Detail.
func RedactDetail(s string, secrets ...string) string {
	s = strings.Join(strings.Fields(RedactSecrets(s, secrets...)), " ")
	if len(s) > MaxConnectionStatusDetail {
		cut := MaxConnectionStatusDetail
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut-- // never split a multi-byte company name
		}
		s = s[:cut]
	}
	return s
}

// ErrorCodeFromSnippet extracts a machine error code ("invalid_bearer")
// from a server error body. Looks at error_code, code, then error. Only
// identifier-shaped values are returned, so free text never leaks through.
func ErrorCodeFromSnippet(snippet string) string {
	var body map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(snippet)), &body); err != nil {
		return ""
	}
	for _, k := range []string{"error_code", "code", "error"} {
		if v, ok := body[k].(string); ok && errorCodeRe.MatchString(v) {
			return v
		}
	}
	return ""
}

// AuthFailure reports whether err is the server rejecting this
// connection's credentials, with the HTTP status and error code.
func AuthFailure(err error) (status int, code string, ok bool) {
	var se *SendError
	if !errors.As(err, &se) || se.Kind != ErrorKindAuth {
		return 0, "", false
	}
	return se.Status, ErrorCodeFromSnippet(se.Snippet), true
}
