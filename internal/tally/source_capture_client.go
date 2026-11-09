package tally

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SourceCaptureClient returns received bytes even for HTTP/read errors so the
// preservation wrapper retains failure evidence. It never silently truncates
// at the normal client's 64 MiB ceiling, follows redirects, or decompresses.
// It is only used by the explicit capture-only CLI path.
type SourceCaptureClient struct {
	endpoint string
	http     *http.Client
	limit    int64
}

func NewSourceCaptureClient(endpoint string) *SourceCaptureClient {
	h := newDefaultHTTPClient(5 * time.Minute)
	h.Transport.(*http.Transport).DisableCompression = true
	h.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &SourceCaptureClient{endpoint: endpoint, http: h, limit: 64 << 20}
}
func (c *SourceCaptureClient) PostXML(ctx context.Context, body []byte) ([]byte, error) {
	if ctx == nil || len(body) == 0 {
		return nil, fmt.Errorf("source capture: context and request are required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml")
	req.Header.Set("User-Agent", "gstreco-tally-agent-source-capture")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, c.limit+1))
	if readErr != nil {
		return raw, fmt.Errorf("source capture: incomplete HTTP body: %w", readErr)
	}
	if int64(len(raw)) > c.limit {
		return raw, fmt.Errorf("source capture: response exceeds %d-byte cap; retained prefix only, capture incomplete", c.limit)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, fmt.Errorf("source capture: HTTP status %d; body retained", resp.StatusCode)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && enc != "identity" {
		return raw, fmt.Errorf("source capture: unsupported Content-Encoding %s; received bytes retained", enc)
	}
	return raw, nil
}
