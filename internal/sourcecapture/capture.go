// Package sourcecapture preserves Tally read requests and exact response bytes.
// It has no GST Reco client, credentials, database or cleanup capability.
package sourcecapture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Poster interface {
	PostXML(context.Context, []byte) ([]byte, error)
}
type Entry struct {
	RequestBytes   int    `json:"request_bytes"`
	Sequence       int    `json:"sequence"`
	RequestFile    string `json:"request_file"`
	ResponseFile   string `json:"response_file"`
	RequestSHA256  string `json:"request_sha256"`
	ResponseSHA256 string `json:"response_sha256"`
	ResponseBytes  int    `json:"response_bytes"`
	ObservedAt     string `json:"observed_at"`
	Error          string `json:"error,omitempty"`
}
type Manifest struct {
	Schema                   int     `json:"schema"`
	Company                  string  `json:"company"`
	Endpoint                 string  `json:"endpoint"`
	Kind                     string  `json:"kind"`
	From                     string  `json:"from"`
	To                       string  `json:"to"`
	Complete                 bool    `json:"pipeline_succeeded"`
	SourcePopulationVerified bool    `json:"source_population_verified"`
	CompanyGUID              string  `json:"expected_company_guid,omitempty"`
	Error                    string  `json:"error,omitempty"`
	Entries                  []Entry `json:"entries"`
}
type Capture struct {
	mu       sync.Mutex
	dir      string
	upstream Poster
	manifest Manifest
	failed   error
	finished bool
}

func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func writeNew(path string, b []byte) error {
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e != nil {
		return e
	}
	return ce
}

// New refuses an existing directory: no capture can overwrite earlier evidence.
func New(dir string, upstream Poster, manifest Manifest) (*Capture, error) {
	if e := os.Mkdir(dir, 0700); e != nil {
		return nil, e
	}
	if e := protectDirectory(dir); e != nil {
		return nil, fmt.Errorf("protect source directory: %w", e)
	}
	manifest.Schema = 2
	manifest.Complete = false
	manifest.SourcePopulationVerified = false
	b, e := json.MarshalIndent(manifest, "", "  ")
	if e != nil {
		return nil, e
	}
	if e = writeNew(filepath.Join(dir, "started.json"), append(b, '\n')); e != nil {
		return nil, e
	}
	return &Capture{dir: dir, upstream: upstream, manifest: manifest}, nil
}
func (c *Capture) PostXML(ctx context.Context, body []byte) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return nil, fmt.Errorf("capture already finished")
	}
	if c.failed != nil {
		return nil, fmt.Errorf("capture stopped after preservation failure: %w", c.failed)
	}
	n := len(c.manifest.Entries) + 1
	req := fmt.Sprintf("%04d-request.xml", n)
	resp := fmt.Sprintf("%04d-response.bin", n)
	if e := writeNew(filepath.Join(c.dir, req), body); e != nil {
		c.failed = fmt.Errorf("preserve request: %w", e)
		return nil, c.failed
	}
	raw, err := c.upstream.PostXML(ctx, body)
	entry := Entry{RequestBytes: len(body), Sequence: n, RequestFile: req, ResponseFile: resp, RequestSHA256: hash(body), ResponseSHA256: hash(raw), ResponseBytes: len(raw), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err != nil {
		entry.Error = err.Error()
	}
	if e := writeNew(filepath.Join(c.dir, resp), raw); e != nil {
		c.failed = fmt.Errorf("preserve response: %w", e)
		return nil, c.failed
	}
	c.manifest.Entries = append(c.manifest.Entries, entry)
	b, e := json.MarshalIndent(entry, "", "  ")
	if e == nil {
		e = writeNew(filepath.Join(c.dir, fmt.Sprintf("%04d-entry.json", n)), append(b, '\n'))
	}
	if e != nil {
		c.failed = fmt.Errorf("preserve entry: %w", e)
		return nil, c.failed
	}
	return raw, err
}

// Finish adds a new manifest only. Complete means the CLI pipeline succeeded,
// not that the returned source is a complete statutory population.
func (c *Capture) Finish(runErr error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return fmt.Errorf("capture already finished")
	}
	c.finished = true
	if runErr == nil {
		runErr = c.failed
	}
	c.manifest.Complete = runErr == nil && len(c.manifest.Entries) > 0
	for _, entry := range c.manifest.Entries {
		if entry.Error != "" {
			c.manifest.Complete = false
		}
	}
	if runErr != nil {
		c.manifest.Error = runErr.Error()
	}
	b, e := json.MarshalIndent(c.manifest, "", "  ")
	if e != nil {
		return e
	}
	return writeNew(filepath.Join(c.dir, "manifest.json"), append(b, '\n'))
}

// PreserveMembership writes one derived interpretation separately from exact
// source bytes. A failed write poisons the capture, so it cannot look complete.
func (c *Capture) PreserveMembership(report any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.finished {
		return fmt.Errorf("capture already finished")
	}
	if c.failed != nil {
		return c.failed
	}
	b, err := json.MarshalIndent(report, "", "  ")
	if err == nil {
		err = writeNew(filepath.Join(c.dir, "membership.json"), append(b, '\n'))
	}
	if err != nil {
		c.failed = fmt.Errorf("preserve membership: %w", err)
	}
	return c.failed
}
