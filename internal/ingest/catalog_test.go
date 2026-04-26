package ingest

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func TestSendCatalog_PostsToCatalogPath(t *testing.T) {
	secretB64, _ := makeSecret(t)

	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "conn-1", "bearer-x", secretB64,
		WithClock(func() time.Time { return time.Date(2026, 4, 26, 10, 0, 0, 0, time.UTC) }),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	endpoint := "http://localhost:9001"
	guid := "guid-A"
	body := CatalogRequest{
		Items: []CatalogItem{
			{TallyCompanyName: "PLLUM CASA", TallyEndpoint: &endpoint, TallyCompanyGUID: &guid},
		},
		RequestID: "req-discover-1",
	}
	if err := c.SendCatalog(context.Background(), body); err != nil {
		t.Fatalf("SendCatalog: %v", err)
	}

	if gotPath != CatalogPath {
		t.Errorf("path = %q, want %q", gotPath, CatalogPath)
	}

	var round CatalogRequest
	if err := json.Unmarshal(gotBody, &round); err != nil {
		t.Fatalf("body not JSON: %v", err)
	}
	if len(round.Items) != 1 {
		t.Fatalf("Items = %d, want 1", len(round.Items))
	}
	if round.Items[0].TallyCompanyName != "PLLUM CASA" {
		t.Errorf("TallyCompanyName = %q", round.Items[0].TallyCompanyName)
	}
	if round.Items[0].TallyEndpoint == nil || *round.Items[0].TallyEndpoint != endpoint {
		t.Errorf("TallyEndpoint = %v", round.Items[0].TallyEndpoint)
	}
	if round.Items[0].TallyCompanyGUID == nil || *round.Items[0].TallyCompanyGUID != guid {
		t.Errorf("TallyCompanyGUID = %v", round.Items[0].TallyCompanyGUID)
	}
	if round.RequestID != "req-discover-1" {
		t.Errorf("RequestID = %q", round.RequestID)
	}
}

func TestSendCatalog_ServerErrorPropagates(t *testing.T) {
	secretB64, _ := makeSecret(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upsert_errors"))
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "c", "b", secretB64)
	err := c.SendCatalog(context.Background(), CatalogRequest{
		Items: []CatalogItem{{TallyCompanyName: "X"}},
	})
	se := IsSendError(err)
	if se == nil {
		t.Fatalf("err = %v, want SendError", err)
	}
	if se.Kind != ErrorKindServer {
		t.Errorf("Kind = %s, want server_5xx", se.Kind)
	}
	if !se.Retryable() {
		t.Errorf("server_5xx should be retryable")
	}
}

func TestPostJSONTo_SignsAtSpecifiedPath(t *testing.T) {
	secretB64, secretBytes := makeSecret(t)

	var gotHeaders http.Header
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c, _ := NewClient(srv.URL, "conn", "bearer", secretB64,
		WithClock(func() time.Time { return time.Unix(1700000000, 0).UTC() }),
	)

	customPath := "/api/tally/heartbeat" // future endpoint; verifies path-agnostic signing
	if err := c.PostJSONTo(context.Background(), customPath, map[string]string{"hello": "world"}); err != nil {
		t.Fatalf("PostJSONTo: %v", err)
	}
	if gotPath != customPath {
		t.Errorf("path=%q, want %q", gotPath, customPath)
	}

	expected := tally.Sign(secretBytes, "POST", customPath,
		gotHeaders.Get(tally.HeaderTimestamp),
		gotHeaders.Get(tally.HeaderNonce),
		gotBody)
	if got := gotHeaders.Get(tally.HeaderSignature); got != expected {
		t.Errorf("signature mismatch — canonical string must include the actual path\n got  %s\n want %s", got, expected)
	}
}
