package ingest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFetchActiveMappings_DecodesResponse(t *testing.T) {
	secretB64, _ := makeSecret(t)

	want := ActiveMappingsResponse{
		ConnectionID: "conn-1",
		Mappings: []ActiveMapping{
			{
				MappingID:        "m-1",
				TallyEndpoint:    "http://localhost:9000",
				TallyCompanyName: "PLLUM CASA",
				TallyCompanyGUID: "g-A",
				CompanyGSTINID:   "gst-1",
			},
			{
				MappingID:        "m-2",
				TallyEndpoint:    "http://localhost:9001",
				TallyCompanyName: "ACME",
				CompanyGSTINID:   "gst-2",
			},
		},
		FetchedAt: "2026-04-26T10:00:00Z",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method=%s, want GET", r.Method)
		}
		if r.URL.Path != "/api/tally/connections/conn-1/mappings/active" {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "conn-1", "bearer", secretB64,
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, err := c.FetchActiveMappings(context.Background())
	if err != nil {
		t.Fatalf("FetchActiveMappings: %v", err)
	}
	if got.ConnectionID != want.ConnectionID {
		t.Errorf("ConnectionID=%q", got.ConnectionID)
	}
	if len(got.Mappings) != 2 {
		t.Fatalf("Mappings=%d, want 2", len(got.Mappings))
	}
	if got.Mappings[0].TallyEndpoint != "http://localhost:9000" {
		t.Errorf("Mappings[0].TallyEndpoint=%q", got.Mappings[0].TallyEndpoint)
	}
	if got.Mappings[1].TallyCompanyGUID != "" {
		t.Errorf("expected empty GUID for legacy row, got %q", got.Mappings[1].TallyCompanyGUID)
	}
}

func TestFetchActiveMappings_403MismatchClassifiedAsAuth(t *testing.T) {
	secretB64, _ := makeSecret(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"connection_mismatch"}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "conn-evil", "bearer", secretB64)
	_, err := c.FetchActiveMappings(context.Background())
	se := IsSendError(err)
	if se == nil {
		t.Fatalf("err=%v, want SendError", err)
	}
	if se.Kind != ErrorKindAuth {
		t.Errorf("Kind=%s, want auth", se.Kind)
	}
	if se.Retryable() {
		t.Errorf("auth errors should not be retryable")
	}
}

func TestActiveMappingsPathFor_BuildsExpectedURL(t *testing.T) {
	got := ActiveMappingsPathFor("11111111-2222-3333-4444-555555555555")
	want := "/api/tally/connections/11111111-2222-3333-4444-555555555555/mappings/active"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestGetJSONFrom_SignsCanonicalAsGet(t *testing.T) {
	secretB64, _ := makeSecret(t)

	var gotMethod string
	var gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotSig = r.Header.Get("x-tally-signature")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "c", "b", secretB64,
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
	)
	var out struct{}
	if err := c.GetJSONFrom(context.Background(), "/foo/bar", &out); err != nil {
		t.Fatalf("GetJSONFrom: %v", err)
	}
	if gotMethod != "GET" {
		t.Errorf("method=%s", gotMethod)
	}
	if gotSig == "" {
		t.Error("missing x-tally-signature header")
	}
}
