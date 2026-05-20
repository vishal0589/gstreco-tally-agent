package ingest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func makeSecret(t *testing.T) (string, []byte) {
	t.Helper()
	raw := []byte("0123456789abcdef0123456789abcdef") // 32 bytes, matches server keysize
	return base64.StdEncoding.EncodeToString(raw), raw
}

func fixedBody() tally.IngestRequestBody {
	return tally.IngestRequestBody{
		RunID:        "r1",
		Kind:         tally.IngestKindPurchase,
		TallyCompany: "Acme",
		Batch: []tally.IngestVoucherRow{
			{InvoiceNumber: "PI-1", InvoiceDate: "2026-04-10", InvoiceValue: 100, TaxableValue: 100},
		},
		RequestID: "req-abc",
		IsFinal:   true,
	}
}

func TestSend_SignsAndSendsHeaders(t *testing.T) {
	secretB64, secretBytes := makeSecret(t)

	var gotHeaders http.Header
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c, err := NewClient(srv.URL, "conn-42", "bearer-tok", secretB64,
		WithClock(func() time.Time { return time.Date(2026, 4, 23, 10, 0, 0, 0, time.UTC) }),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	body := fixedBody()
	if _, err := c.Send(context.Background(), body); err != nil {
		t.Fatalf("Send: %v", err)
	}

	for _, h := range []string{tally.HeaderConnectionID, tally.HeaderTimestamp, tally.HeaderNonce, tally.HeaderSignature, tally.HeaderAuth} {
		if gotHeaders.Get(h) == "" {
			t.Errorf("missing header %s", h)
		}
	}
	if got := gotHeaders.Get(tally.HeaderAuth); got != "Bearer bearer-tok" {
		t.Errorf("authorization = %q", got)
	}
	if got := gotHeaders.Get(tally.HeaderConnectionID); got != "conn-42" {
		t.Errorf("connection id = %q", got)
	}

	// Recompute the signature with the same inputs and assert the client
	// produced it correctly — this is the canonical cross-check with the
	// server's verifier.
	expectedSig := tally.Sign(
		secretBytes,
		"POST", Path,
		gotHeaders.Get(tally.HeaderTimestamp),
		gotHeaders.Get(tally.HeaderNonce),
		gotBody,
	)
	if got := gotHeaders.Get(tally.HeaderSignature); got != expectedSig {
		t.Errorf("signature mismatch\n got  %s\n want %s", got, expectedSig)
	}

	// Body on the wire is valid JSON and parses back to the input.
	var round tally.IngestRequestBody
	if err := json.Unmarshal(gotBody, &round); err != nil {
		t.Fatalf("server received non-JSON body: %v", err)
	}
	if round.RunID != body.RunID || round.RequestID != body.RequestID || len(round.Batch) != 1 {
		t.Errorf("body round-trip broken: %+v", round)
	}
}

func TestSend_ClassifiesErrorsForRetryDecision(t *testing.T) {
	secretB64, _ := makeSecret(t)

	cases := []struct {
		name       string
		status     int
		wantKind   ErrorKind
		wantRetry  bool
	}{
		{"500 is server, retryable", 500, ErrorKindServer, true},
		{"503 is server, retryable", 503, ErrorKindServer, true},
		{"429 is rate-limited, retryable", 429, ErrorKindRateLimited, true},
		{"401 is auth, give up", 401, ErrorKindAuth, false},
		{"403 is auth, give up", 403, ErrorKindAuth, false},
		{"400 is payload, give up", 400, ErrorKindPayload, false},
		{"422 is payload, give up", 422, ErrorKindPayload, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte("failure body"))
			}))
			defer srv.Close()

			c, _ := NewClient(srv.URL, "c", "b", secretB64)
			_, err := c.Send(context.Background(), fixedBody())
			se := IsSendError(err)
			if se == nil {
				t.Fatalf("err = %v, not a SendError", err)
			}
			if se.Kind != tc.wantKind {
				t.Errorf("Kind = %s, want %s", se.Kind, tc.wantKind)
			}
			if se.Retryable() != tc.wantRetry {
				t.Errorf("Retryable = %v, want %v", se.Retryable(), tc.wantRetry)
			}
			if !strings.Contains(se.Error(), "failure body") {
				t.Errorf("Error = %q, want includes server snippet", se.Error())
			}
		})
	}
}

func TestSend_NetworkErrorIsRetryable(t *testing.T) {
	secretB64, _ := makeSecret(t)
	c, _ := NewClient("http://127.0.0.1:1", "c", "b", secretB64)
	_, err := c.Send(context.Background(), fixedBody())
	se := IsSendError(err)
	if se == nil || se.Kind != ErrorKindNetwork {
		t.Fatalf("err = %v, want SendError{Kind: network}", err)
	}
	if !se.Retryable() {
		t.Error("network error should be retryable")
	}
	if !errors.Is(err, err.(*SendError).Wrapped) {
		t.Error("Unwrap chain broken")
	}
}

func TestNewClient_Validation(t *testing.T) {
	cases := []struct{ name, base, id, bearer, secret string }{
		{"no base", "", "c", "b", "AAAA"},
		{"no id", "http://x", "", "b", "AAAA"},
		{"no bearer", "http://x", "c", "", "AAAA"},
		{"no secret", "http://x", "c", "b", ""},
		{"bad secret", "http://x", "c", "b", "not-base64!!!"},
	}
	for _, tc := range cases {
		if _, err := NewClient(tc.base, tc.id, tc.bearer, tc.secret); err == nil {
			t.Errorf("%s: want error, got nil", tc.name)
		}
	}
}

func TestBackoffMonotonicUpToCap(t *testing.T) {
	// Full-jitter backoff: uniform [0, exp). We assert the CEILING grows
	// exponentially up to the cap — not the specific draws (those are
	// randomized).
	base := 100 * time.Millisecond
	cap := 2 * time.Second

	// Run enough draws to see the distribution's upper-tail behaviour.
	maxSeen := make(map[int]time.Duration)
	for i := 0; i < 200; i++ {
		for attempt := 1; attempt <= 6; attempt++ {
			d := Backoff(attempt, base, cap)
			if d > maxSeen[attempt] {
				maxSeen[attempt] = d
			}
			if d > cap {
				t.Errorf("attempt %d draw %d exceeded cap (%v > %v)", attempt, i, d, cap)
			}
		}
	}
	// By attempt 5 we should see draws close to the cap (exp = base << 4 =
	// 1600ms, still under 2s cap).
	if maxSeen[5] < time.Second {
		t.Errorf("attempt 5 max = %v, expected to see samples approaching the cap", maxSeen[5])
	}
}
