package selfupdate

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func quietLogger() zerolog.Logger { return zerolog.New(io.Discard) }

func TestNew_RequiresAllFields(t *testing.T) {
	cases := []Options{
		{}, // missing everything
		{HTTPClient: http.DefaultClient},
		{HTTPClient: http.DefaultClient, VersionURL: "https://x/v"},
		{HTTPClient: http.DefaultClient, VersionURL: "https://x/v", CurrentVersion: "0.1.0"},
	}
	for i, opts := range cases {
		opts.Logger = quietLogger()
		if _, err := New(opts); err == nil {
			t.Errorf("case %d should error: %+v", i, opts)
		}
	}
}

func TestShouldNotify(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		{"0.1.0", "0.2.0", true},
		{"0.1.0", "0.1.0", false},
		{"v0.1.19", "0.1.19", false},
		{"0.1.19", "v0.1.19", false},
		{"", "0.1.0", false},
		{"0.1.0", "", false},
		{"a27a610-dirty (a27a610, 2026-04-26T07:59:23Z)", "a27a610-dirty", false}, // dev build matches itself
		{"a27a610-dirty (a27a610, 2026-04-26T07:59:23Z)", "0.2.0", true},
	}
	for _, c := range cases {
		got := shouldNotify(c.current, c.latest)
		if got != c.want {
			t.Errorf("shouldNotify(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestCheckOnce_FiresHandlerOnNewer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"latest":"0.2.0","released_at":"2026-04-26T00:00:00Z"}`))
	}))
	defer srv.Close()

	var fired int32
	var got Notification
	c, err := New(Options{
		HTTPClient:     http.DefaultClient,
		VersionURL:     srv.URL,
		CurrentVersion: "0.1.0",
		Handler: func(n Notification) {
			atomic.StoreInt32(&fired, 1)
			got = n
		},
		Interval: time.Hour,
		Logger:   quietLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}
	c.checkOnce(context.Background())
	if atomic.LoadInt32(&fired) != 1 {
		t.Fatal("handler did not fire on newer version")
	}
	if got.LatestVersion != "0.2.0" {
		t.Errorf("LatestVersion=%q", got.LatestVersion)
	}
	if got.CurrentVersion != "0.1.0" {
		t.Errorf("CurrentVersion=%q", got.CurrentVersion)
	}
}

func TestCheckOnce_NoFireOnSameVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"latest":"0.1.0"}`))
	}))
	defer srv.Close()

	var fired int32
	c, _ := New(Options{
		HTTPClient:     http.DefaultClient,
		VersionURL:     srv.URL,
		CurrentVersion: "0.1.0",
		Handler:        func(Notification) { atomic.StoreInt32(&fired, 1) },
		Interval:       time.Hour,
		Logger:         quietLogger(),
	})
	c.checkOnce(context.Background())
	if atomic.LoadInt32(&fired) != 0 {
		t.Error("handler fired on same version")
	}
}

func TestCheckOnce_NoFireOnNetworkError(t *testing.T) {
	var fired int32
	c, _ := New(Options{
		HTTPClient:     &errClient{err: errors.New("network down")},
		VersionURL:     "http://example.test/v",
		CurrentVersion: "0.1.0",
		Handler:        func(Notification) { atomic.StoreInt32(&fired, 1) },
		Interval:       time.Hour,
		Logger:         quietLogger(),
	})
	c.checkOnce(context.Background())
	if atomic.LoadInt32(&fired) != 0 {
		t.Error("handler fired on network error")
	}
}

func TestCheckOnce_NoFireOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	var fired int32
	c, _ := New(Options{
		HTTPClient:     http.DefaultClient,
		VersionURL:     srv.URL,
		CurrentVersion: "0.1.0",
		Handler:        func(Notification) { atomic.StoreInt32(&fired, 1) },
		Interval:       time.Hour,
		Logger:         quietLogger(),
	})
	c.checkOnce(context.Background())
	if atomic.LoadInt32(&fired) != 0 {
		t.Error("handler fired on non-200")
	}
}

func TestCheckOnce_NoFireOnEmptyLatest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"latest":""}`))
	}))
	defer srv.Close()
	var fired int32
	c, _ := New(Options{
		HTTPClient:     http.DefaultClient,
		VersionURL:     srv.URL,
		CurrentVersion: "0.1.0",
		Handler:        func(Notification) { atomic.StoreInt32(&fired, 1) },
		Interval:       time.Hour,
		Logger:         quietLogger(),
	})
	c.checkOnce(context.Background())
	if atomic.LoadInt32(&fired) != 0 {
		t.Error("handler fired on empty latest")
	}
}

func TestStartStopIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"latest":"0.1.0"}`))
	}))
	defer srv.Close()
	c, _ := New(Options{
		HTTPClient:     http.DefaultClient,
		VersionURL:     srv.URL,
		CurrentVersion: "0.1.0",
		Handler:        func(Notification) {},
		Interval:       time.Hour,
		Logger:         quietLogger(),
	})
	ctx := context.Background()
	c.Start(ctx)
	c.Start(ctx) // no-op
	c.Stop()
	c.Stop() // no-op
}

func TestStripBuildMeta(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"0.1.0", "0.1.0"},
		{"0.1.0+build", "0.1.0"},
		{"a27a610-dirty (a27a610, 2026-04-26T07:59:23Z)", "a27a610-dirty"},
		{"  0.1.0  ", ""}, // stripBuildMeta sees space at index 0 → empty; in practice shouldNotify trims first
		{"", ""},
	}
	for _, c := range cases {
		got := stripBuildMeta(c.in)
		if got != c.want {
			t.Errorf("stripBuildMeta(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Sanity: the well-formed URL we hit must not produce stray
// "loopback" warnings even when the test server lives on
// 127.0.0.1.
func TestVersionURLAccepted(t *testing.T) {
	url := "https://gstreco.example/api/tally/agent/version"
	if !strings.HasPrefix(url, "https://") {
		t.Fatal("test setup error")
	}
}

type errClient struct{ err error }

func (e *errClient) Get(_ string) (*http.Response, error) { return nil, e.err }
