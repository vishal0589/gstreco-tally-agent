package tally

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// DefaultEndpoint is where Tally Prime's embedded HTTP server listens by
// default. Users can change the port in F1 > TCP Port; the agent's config
// file exposes this as `tally_endpoint` and passes it through to NewClient.
const DefaultEndpoint = "http://localhost:9000"

// defaultTimeout is generous because Tally streams large day-book responses
// synchronously — a month of vouchers on a laptop can take ~60s and we'd
// rather wait than retry. Short per-request RTTs (pair check, company list)
// use the context to deadline-shorten as needed.
const defaultTimeout = 5 * time.Minute

// Client is the Tally HTTP XML API wrapper. Safe for concurrent use; the
// underlying http.Client pools connections. Create once per agent process.
type Client struct {
	endpoint string
	http     *http.Client
}

// ClientOption customises a Client at construction. Use WithHTTPClient to
// swap the transport for tests or instrumentation; use WithTimeout to
// override the (long) default when calling Tally for short metadata queries.
type ClientOption func(*Client)

// WithHTTPClient supplies a custom http.Client. Useful for tests that need to
// intercept requests, and for A4 to wire in retry/backoff middleware. If not
// set, Client builds its own http.Client with the default timeout and
// connection-reuse behaviour appropriate for Tally's long streaming
// responses.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) { c.http = h }
}

// WithTimeout caps the total request duration. 0 disables (falls back to
// context deadline only). Default is 5 minutes to accommodate multi-thousand
// voucher day-books.
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		if c.http == nil {
			c.http = newDefaultHTTPClient(d)
		} else {
			c.http.Timeout = d
		}
	}
}

// NewClient builds a Client for the given Tally endpoint. Endpoint is
// normally "http://localhost:9000" — the agent only reaches Tally over
// loopback, never the public internet.
func NewClient(endpoint string, opts ...ClientOption) *Client {
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	c := &Client{endpoint: endpoint}
	for _, opt := range opts {
		opt(c)
	}
	if c.http == nil {
		c.http = newDefaultHTTPClient(defaultTimeout)
	}
	return c
}

// newDefaultHTTPClient returns an http.Client tuned for Tally's localhost
// behaviour: short DialContext (we're on loopback), no proxy, generous total
// timeout, and HTTP keep-alive so repeated calls during one sync run reuse
// the same TCP connection.
func newDefaultHTTPClient(timeout time.Duration) *http.Client {
	transport := &http.Transport{
		Proxy: nil, // never use the system proxy to reach localhost
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   timeout,
	}
}

// Endpoint returns the server URL the client posts to. Useful for logs and
// for A3's pair flow which records the endpoint in the connection metadata.
func (c *Client) Endpoint() string { return c.endpoint }

// PostXML sends a rendered TDL envelope to Tally and returns the response
// body. Errors cover network failures, non-2xx responses, and oversized
// bodies. On success the caller hands the bytes to ParseDayBookV3 (or a
// master-list parser in A2b).
//
// Content-type is "text/xml" — Tally doesn't care about the charset but
// sending one triggers the wrong response path on some Prime patch releases,
// so we omit it. The request body must be valid XML; BuildDayBookXML
// produces compliant output.
func (c *Client) PostXML(ctx context.Context, body []byte) ([]byte, error) {
	if ctx == nil {
		return nil, fmt.Errorf("tally: nil context")
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("tally: empty request body")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tally: build request: %w", err)
	}
	req.Header.Set("Content-Type", "text/xml")
	req.Header.Set("User-Agent", "gstreco-tally-agent")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tally: POST %s: %w", c.endpoint, err)
	}
	defer resp.Body.Close()

	// Tally's HTTP server returns 200 for both success and application-level
	// failures — the <STATUS> field in the body tells the real story. Still
	// surface non-2xx as an error so network-level faults are distinguishable
	// from "Tally said no".
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("tally: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	// Cap at 64 MiB — a single day-book request for a month of data on a
	// mid-size SMB's books is ~5 MiB; 64 MiB is a defensive ceiling that
	// prevents a rogue or misconfigured Tally from exhausting agent memory.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("tally: read response: %w", err)
	}
	return payload, nil
}
