// Package selfupdate watches GST Reco's /api/tally/agent/version
// endpoint and fires a notification when a newer agent version is
// available. V1 is notify-only — the operator re-runs install.ps1
// to apply. Strong auto-update (download + verify + restart) lands
// after pilot stability data justifies the blast radius.
package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// DefaultCheckInterval is the recurring cadence. The server-side
// /agent/version endpoint suggests 6h via poll_interval_seconds;
// we honour that as the floor. Operators with paid Pro plans pull
// every 6h × ~50 customers = ~10 req/min globally — fine.
const DefaultCheckInterval = 6 * time.Hour

// VersionResponse mirrors the JSON shape of /api/tally/agent/version.
// Only the fields the checker uses are typed — full release metadata
// stays opaque so future server-side additions don't break old
// agents.
type VersionResponse struct {
	Latest              string `json:"latest"`
	ReleasedAt          string `json:"released_at,omitempty"`
	PollIntervalSeconds int    `json:"poll_interval_seconds,omitempty"`
}

// Notification carries the comparison result so the caller can
// decide what to do with it (log, tray badge, future auto-apply).
type Notification struct {
	CurrentVersion string
	LatestVersion  string
	ReleasedAt     string
	// FetchedAt is the agent-side timestamp of the check; useful
	// for tray UIs that want to show "checked X minutes ago".
	FetchedAt time.Time
}

// Handler is the callback fired when a newer version is found. The
// daemon's implementation logs at warn level + sets a flag the
// (future) tray reads.
type Handler func(n Notification)

// HTTPClient is the narrow surface Checker uses. *http.Client
// implements it; tests pass fakes.
type HTTPClient interface {
	Get(url string) (*http.Response, error)
}

// Checker periodically GETs /api/tally/agent/version and fires
// Handler when the response's `latest` differs from the agent's
// own version. One Checker per daemon process.
type Checker struct {
	httpClient     HTTPClient
	versionURL     string
	currentVersion string
	handler        Handler
	interval       time.Duration
	logger         zerolog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// Options configures a new Checker.
type Options struct {
	HTTPClient HTTPClient
	// VersionURL is the full URL of /api/tally/agent/version.
	// Server URL + the route path; daemon's caller composes it.
	VersionURL string
	// CurrentVersion is the agent's own semver string. Comparison
	// is plain string equality — semver-aware logic ships if/when
	// we have downgrade-block scenarios. Today the server only
	// ever publishes monotonically increasing versions so equality
	// is enough.
	CurrentVersion string
	Handler        Handler
	Interval       time.Duration
	Logger         zerolog.Logger
}

// New builds a Checker. Required: HTTPClient, VersionURL,
// CurrentVersion, Handler, Logger. Interval defaults to
// DefaultCheckInterval.
func New(opts Options) (*Checker, error) {
	if opts.HTTPClient == nil {
		return nil, errors.New("selfupdate: HTTPClient is required")
	}
	if opts.VersionURL == "" {
		return nil, errors.New("selfupdate: VersionURL is required")
	}
	if opts.CurrentVersion == "" {
		return nil, errors.New("selfupdate: CurrentVersion is required")
	}
	if opts.Handler == nil {
		return nil, errors.New("selfupdate: Handler is required")
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = DefaultCheckInterval
	}
	return &Checker{
		httpClient:     opts.HTTPClient,
		versionURL:     opts.VersionURL,
		currentVersion: opts.CurrentVersion,
		handler:        opts.Handler,
		interval:       interval,
		logger:         opts.Logger,
	}, nil
}

// Start launches the check loop. Fires immediately so a freshly-
// started agent learns about updates without waiting for the first
// 6h boundary, then on every Interval tick.
func (c *Checker) Start(parent context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.done != nil {
		return // already started
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	c.cancel = cancel
	c.done = done
	// Pass the channel by value into the goroutine — Stop() may
	// nil c.done before the goroutine completes, and the deferred
	// close needs a non-nil reference.
	go c.loop(ctx, done)
}

// Stop signals the loop and waits for it to drain.
func (c *Checker) Stop() {
	c.mu.Lock()
	cancel := c.cancel
	done := c.done
	c.cancel = nil
	c.done = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
}

func (c *Checker) loop(ctx context.Context, done chan struct{}) {
	defer close(done)
	c.checkOnce(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.checkOnce(ctx)
		}
	}
}

// checkOnce fetches the latest version + compares. Network errors
// are logged and not retried — the next interval handles them.
// Returns the Notification when the handler fires, or nil/false
// otherwise. Exposed for tests.
func (c *Checker) checkOnce(_ context.Context) {
	resp, err := c.httpClient.Get(c.versionURL)
	if err != nil {
		c.logger.Debug().Err(err).Str("url", c.versionURL).Msg("selfupdate: version fetch failed (will retry next interval)")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		c.logger.Debug().Int("status", resp.StatusCode).Msg("selfupdate: non-200 from version endpoint")
		return
	}
	body, decodeErr := decodeJSON(resp)
	if decodeErr != nil {
		c.logger.Debug().Err(decodeErr).Msg("selfupdate: decode failed")
		return
	}
	if strings.TrimSpace(body.Latest) == "" {
		c.logger.Debug().Msg("selfupdate: server returned empty latest")
		return
	}
	if !shouldNotify(c.currentVersion, body.Latest) {
		// Up-to-date — no notification.
		return
	}
	c.handler(Notification{
		CurrentVersion: c.currentVersion,
		LatestVersion:  body.Latest,
		ReleasedAt:     body.ReleasedAt,
		FetchedAt:      time.Now(),
	})
}

// shouldNotify returns true when latest != current AND latest is
// not the legacy "0.0.0" placeholder. Plain string equality — every
// release in the wild bumps the version, so it's enough. If we ever
// publish a v2 that's actually older (downgrade), the comparison
// would still notify; semver-aware logic is a separate concern that
// only matters once we ship downgrade scenarios.
func shouldNotify(current, latest string) bool {
	current = strings.TrimSpace(current)
	latest = strings.TrimSpace(latest)
	if current == "" || latest == "" {
		return false
	}
	// Strip dev-build suffix from current ("a27a610-dirty (a27a610,
	// 2026-04-26T...)") — just match the semver/short-sha prefix.
	currentBase := normalizeComparableVersion(stripBuildMeta(current))
	latestBase := normalizeComparableVersion(stripBuildMeta(latest))
	if currentBase == latestBase {
		return false
	}
	return true
}

func normalizeComparableVersion(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "v")
}

// stripBuildMeta returns the prefix before the first space or '+'.
// "a27a610-dirty (a27a610, 2026-04-26)" → "a27a610-dirty".
// "0.1.0+build123" → "0.1.0". Operator-friendly heuristic; not a
// full semver parser.
func stripBuildMeta(s string) string {
	for i, c := range s {
		if c == ' ' || c == '+' {
			return s[:i]
		}
	}
	return s
}

// decodeJSON unmarshals a small JSON body. Wrapped function so tests
// can replace the http.Response without faking the io.Reader chain.
func decodeJSON(resp *http.Response) (*VersionResponse, error) {
	var out VersionResponse
	dec := jsonDecoder(resp)
	if err := dec(&out); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return &out, nil
}

// jsonDecoder returns a one-shot decode function. Indirected so
// tests can hot-swap if needed.
func jsonDecoder(resp *http.Response) func(any) error {
	return func(out any) error {
		return jsonNewDecoder(resp).Decode(out)
	}
}

// jsonNewDecoder is a tiny shim over encoding/json. Kept as a free
// function so tests can replace it (rare, but possible).
var jsonNewDecoder = func(resp *http.Response) interface {
	Decode(v any) error
} {
	return jsonDecoderImpl{r: resp}
}

type jsonDecoderImpl struct{ r *http.Response }

func (j jsonDecoderImpl) Decode(v any) error {
	return jsonDecodeFromBody(j.r, v)
}

func jsonDecodeFromBody(resp *http.Response, v any) error {
	return decodeBody(resp, v)
}
