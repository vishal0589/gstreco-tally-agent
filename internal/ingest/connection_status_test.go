package ingest

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStatusEndpoint_HostPortOnly(t *testing.T) {
	for in, want := range map[string]string{
		"http://127.0.0.1:9000":             "127.0.0.1:9000",
		"http://user:pw@localhost:9001/x?y": "localhost:9001",
		"localhost:9002":                    "localhost:9002",
		"":                                  "",
	} {
		if got := StatusEndpoint(in); got != want {
			t.Errorf("StatusEndpoint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactDetail_RemovesTokensKeepsUUIDsAndCaps(t *testing.T) {
	const fake = "FAKE-TEST-BEARER-not-a-real-token-1234567890"
	in := `401 {"error":"invalid_bearer","token":"abc","seen":"Bearer ` + fake + `"} request 11111111-2222-3333-4444-555555555555 own ` + fake
	got := RedactDetail(in, fake)
	if strings.Contains(got, fake) || strings.Contains(got, `"abc"`) {
		t.Fatalf("token survived redaction: %q", got)
	}
	if !strings.Contains(got, "invalid_bearer") || !strings.Contains(got, "11111111-2222-3333-4444-555555555555") {
		t.Fatalf("useful detail lost: %q", got)
	}
	long := RedactDetail(strings.Repeat("Colouredge é ", 60))
	if len(long) > MaxConnectionStatusDetail || !json.Valid([]byte(`"`+long+`"`)) {
		t.Fatalf("detail not capped on a rune boundary: %d bytes", len(long))
	}
}

func TestErrorCodeFromSnippet(t *testing.T) {
	for in, want := range map[string]string{
		`{"error":"invalid_bearer","message":"bearer token does not match this connection"}`: "invalid_bearer",
		`{"code":"connection_revoked"}`:                     "connection_revoked",
		`{"error":"Bearer token does not match, see logs"}`: "",
		`not json`: "",
	} {
		if got := ErrorCodeFromSnippet(in); got != want {
			t.Errorf("ErrorCodeFromSnippet(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthFailure(t *testing.T) {
	st, code, ok := AuthFailure(&SendError{Kind: ErrorKindAuth, Status: 401, Snippet: `{"error":"invalid_bearer"}`})
	if !ok || st != 401 || code != "invalid_bearer" {
		t.Fatalf("AuthFailure = %d %q %v", st, code, ok)
	}
	if _, _, ok := AuthFailure(&SendError{Kind: ErrorKindServer, Status: 500}); ok {
		t.Fatal("5xx is not an auth failure")
	}
}

func TestConnectionStatusJSONShape(t *testing.T) {
	b, _ := json.Marshal(ConnectionStatus{Result: ConnStatusIdle, CheckedAt: "2026-11-03T00:00:00Z"})
	for _, key := range []string{`"result":"idle"`, `"checked_at"`, `"tally_endpoint":null`, `"mapping_id":null`,
		`"expected_company":null`, `"open_companies":null`, `"http_status":null`, `"error_code":null`,
		`"detail":null`, `"sync_request_id":null`} {
		if !strings.Contains(string(b), key) {
			t.Errorf("JSON %s missing %s", b, key)
		}
	}
}
