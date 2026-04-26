// Package ingest ships normalised voucher batches to the GST Reco server.
// Every request is Bearer-authenticated AND HMAC-signed — the same request
// can be verified offline from the canonical string even after it's been
// written to the outbox and replayed minutes later.
package ingest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// Path is the server ingest endpoint. Exported so tests can assert against
// the same constant the canonical HMAC signature is built over.
const Path = "/api/tally/ingest"

// Client is the outbound ingest HTTP client. One instance per connection —
// the connection_id, bearer, and HMAC secret are bound at construction so
// Send() can't accidentally be called with mis-paired credentials.
type Client struct {
	baseURL      string
	connectionID string
	bearer       string
	secret       []byte
	http         *http.Client
	now          func() time.Time // injectable for tests
}

// Option customises a Client at construction.
type Option func(*Client)

// WithHTTPClient overrides the default http.Client. Caller owns the Transport
// so tests can intercept and production can swap in instrumentation.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithClock overrides time.Now — used by tests to freeze x-tally-timestamp.
// Default is time.Now in UTC.
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// NewClient builds an ingest client. secretB64 is the base64-encoded HMAC
// secret as returned by /api/tally/pair/claim and stored in the OS keyring.
// It's decoded once here so Send() doesn't pay the cost per request.
func NewClient(baseURL, connectionID, bearer, secretB64 string, opts ...Option) (*Client, error) {
	if baseURL == "" || connectionID == "" || bearer == "" || secretB64 == "" {
		return nil, fmt.Errorf("ingest: baseURL, connectionID, bearer, and secret are all required")
	}
	secret, err := base64.StdEncoding.DecodeString(secretB64)
	if err != nil {
		return nil, fmt.Errorf("ingest: decode hmac secret: %w", err)
	}
	c := &Client{
		baseURL:      strings.TrimRight(baseURL, "/"),
		connectionID: connectionID,
		bearer:       bearer,
		secret:       secret,
		now:          func() time.Time { return time.Now().UTC() },
	}
	for _, o := range opts {
		o(c)
	}
	if c.http == nil {
		// 120s timeout: day-book batches can be ~500 rows @ a few KB each; a
		// slow-network push takes a couple of seconds; 120s is conservative.
		c.http = &http.Client{
			Timeout: 120 * time.Second,
			// Never follow redirects on a signed POST. The HMAC signature is
			// bound to the original path; a 301/302/307/308 would hit a new
			// endpoint the signature doesn't cover, silently either dropping
			// the body (301/302 → GET) or presenting a canonical path that
			// mismatches the new URL. Surface any redirect as an error so
			// operators see it instead of puzzling over 401s at a mirror.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	return c, nil
}

// Send posts one ingest batch to /api/tally/ingest. Thin wrapper over
// PostJSONTo kept as the public surface so existing callers (the smoke
// runbook, A4 outbox runner, A5-1 agentctl sync) don't have to learn
// about path routing.
func (c *Client) Send(ctx context.Context, body tally.IngestRequestBody) error {
	return c.PostJSONTo(ctx, Path, body)
}

// PostJSONTo signs and POSTs an arbitrary JSON body to a server path.
// Same HMAC + bearer scheme as Send, just decoupled from the /ingest
// path so A5-2's catalog upsert and future endpoints (heartbeat,
// rotate-secret) reuse the canonical signing logic. Returns
// SendError on non-2xx so the same retryable/give-up classification
// works regardless of which endpoint is being called.
func (c *Client) PostJSONTo(ctx context.Context, path string, body any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("ingest: marshal: %w", err)
	}

	nonce, err := tally.NewNonce()
	if err != nil {
		return err
	}
	ts := tally.NowTimestamp(c.now())
	sig := tally.Sign(c.secret, http.MethodPost, path, ts, nonce, payload)

	url := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("ingest: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(tally.HeaderAuth, "Bearer "+c.bearer)
	req.Header.Set(tally.HeaderConnectionID, c.connectionID)
	req.Header.Set(tally.HeaderTimestamp, ts)
	req.Header.Set(tally.HeaderNonce, nonce)
	req.Header.Set(tally.HeaderSignature, sig)

	resp, err := c.http.Do(req)
	if err != nil {
		return &SendError{Kind: ErrorKindNetwork, Wrapped: err}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}

	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	e := &SendError{
		Status:  resp.StatusCode,
		Snippet: strings.TrimSpace(string(snippet)),
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		e.Kind = ErrorKindAuth
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusUnprocessableEntity:
		e.Kind = ErrorKindPayload
	case resp.StatusCode == http.StatusTooManyRequests:
		e.Kind = ErrorKindRateLimited
	case resp.StatusCode >= 500:
		e.Kind = ErrorKindServer
	default:
		e.Kind = ErrorKindUnknown
	}
	return e
}

// SendError describes a failed Send. Kind drives retry behaviour in the
// outbox runner: network + server + rate-limited → retry with backoff; auth
// + payload → give up (re-pair or fix the payload).
type SendError struct {
	Kind    ErrorKind
	Status  int
	Snippet string
	Wrapped error
}

func (e *SendError) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("ingest: %s: %v", e.Kind, e.Wrapped)
	}
	return fmt.Sprintf("ingest: %s: status %d — %s", e.Kind, e.Status, e.Snippet)
}

func (e *SendError) Unwrap() error { return e.Wrapped }

// Retryable reports whether the ingest runner should schedule another attempt.
// Kept as a method so the classification logic stays next to ErrorKind.
func (e *SendError) Retryable() bool {
	switch e.Kind {
	case ErrorKindNetwork, ErrorKindServer, ErrorKindRateLimited:
		return true
	default:
		return false
	}
}

// ErrorKind categorises a failed Send.
type ErrorKind string

const (
	ErrorKindNetwork     ErrorKind = "network"
	ErrorKindServer      ErrorKind = "server_5xx"
	ErrorKindRateLimited ErrorKind = "rate_limited"
	ErrorKindAuth        ErrorKind = "auth"
	ErrorKindPayload     ErrorKind = "payload"
	ErrorKindUnknown     ErrorKind = "unknown"
)

// Backoff returns the next delay for an attempt number, starting at attempts=1.
// Exponential with a full-jitter cap — derived from AWS's canonical
// "Exponential Backoff and Jitter" post. Keeps the agent from synchronising
// with other agents during a server brown-out.
func Backoff(attempts int, base, cap time.Duration) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	// base * 2^(attempts-1), capped
	exp := base << (attempts - 1)
	if exp <= 0 || exp > cap {
		exp = cap
	}
	// Full jitter: uniform [0, exp)
	jitter := time.Duration(rand.Int63n(int64(exp) + 1))
	return jitter
}

// IsSendError returns the SendError if err wraps one, or nil otherwise. Lets
// the outbox runner do `if se := IsSendError(err); se != nil && se.Retryable()`.
func IsSendError(err error) *SendError {
	var se *SendError
	if errors.As(err, &se) {
		return se
	}
	return nil
}
