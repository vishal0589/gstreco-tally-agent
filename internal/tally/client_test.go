package tally

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClient_PostXML_Success(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "text/xml" {
			t.Errorf("Content-Type = %q", ct)
		}
		if ua := r.Header.Get("User-Agent"); ua != "gstreco-tally-agent" {
			t.Errorf("User-Agent = %q", ua)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER></ENVELOPE>`))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	resp, err := c.PostXML(context.Background(), []byte("<PING/>"))
	if err != nil {
		t.Fatalf("PostXML: %v", err)
	}
	if !strings.Contains(string(resp), "STATUS>1<") {
		t.Errorf("response = %s", resp)
	}
	if string(gotBody) != "<PING/>" {
		t.Errorf("server received body = %q", gotBody)
	}
}

func TestClient_PostXML_Non2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("tally panicked"))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL).PostXML(context.Background(), []byte("<X/>"))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("want 500 error, got %v", err)
	}
}

func TestClient_PostXML_Validation(t *testing.T) {
	c := NewClient("http://localhost:0")
	//nolint:staticcheck // explicit nil ctx is the unit under test
	if _, err := c.PostXML(nil, []byte("<X/>")); err == nil {
		t.Error("nil ctx: want error")
	}
	if _, err := c.PostXML(context.Background(), nil); err == nil {
		t.Error("empty body: want error")
	}
}

func TestClient_PostXML_RespectsContextCancel(t *testing.T) {
	// Handler waits briefly, long enough that the client ctx deadline fires
	// first. Server eventually returns on its own so srv.Close() doesn't hang
	// on a kept-alive connection — we intentionally don't rely on the
	// client's cancel propagating through to the server's request context
	// fast enough for httptest's 5s Close() warning window.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	t.Cleanup(srv.CloseClientConnections)
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := NewClient(srv.URL).PostXML(ctx, []byte("<X/>"))
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

func TestNewClient_DefaultEndpoint(t *testing.T) {
	c := NewClient("")
	if c.Endpoint() != DefaultEndpoint {
		t.Errorf("endpoint = %q, want %q", c.Endpoint(), DefaultEndpoint)
	}
}
