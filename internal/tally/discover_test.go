package tally

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTallyServer responds to version + company-list envelopes with
// canned XML keyed by request body content. Anything else returns
// 500 so a misrouted probe is loud.
func fakeTallyServer(t *testing.T, version string, companies []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		s := string(body)
		switch {
		case strings.Contains(s, "$$Version"):
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte("<ENVELOPE><VERSIONNUMBER>" + version + "</VERSIONNUMBER></ENVELOPE>"))
		case strings.Contains(s, "List of Companies"):
			var coBlock strings.Builder
			coBlock.WriteString("<ENVELOPE><BODY><DATA>")
			for i, c := range companies {
				coBlock.WriteString("<COMPANY><NAME>")
				coBlock.WriteString(c)
				coBlock.WriteString("</NAME><GUID>guid-")
				coBlock.WriteByte(byte('A' + i))
				coBlock.WriteString("</GUID></COMPANY>")
			}
			coBlock.WriteString("</DATA></BODY></ENVELOPE>")
			w.Header().Set("Content-Type", "text/xml")
			_, _ = w.Write([]byte(coBlock.String()))
		default:
			http.Error(w, "unexpected envelope", http.StatusInternalServerError)
		}
	}))
}

func TestDiscover_PicksUpRunningTally(t *testing.T) {
	srv := fakeTallyServer(t, "Release 3.0.1", []string{"PLLUM CASA", "PLLUM LEGNO"})
	defer srv.Close()

	results, err := Discover(context.Background(), DiscoverOptions{
		Endpoints: []string{srv.URL},
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if !r.Reachable || !r.IsTally {
		t.Errorf("Reachable=%v IsTally=%v", r.Reachable, r.IsTally)
	}
	if r.Version != VersionV3 {
		t.Errorf("Version=%v, want VersionV3", r.Version)
	}
	if len(r.Companies) != 2 {
		t.Fatalf("Companies=%d, want 2", len(r.Companies))
	}
	if r.Companies[0].Name != "PLLUM CASA" || r.Companies[1].Name != "PLLUM LEGNO" {
		t.Errorf("Companies = %+v", r.Companies)
	}
	if r.LatencyMs < 0 {
		t.Errorf("LatencyMs negative: %d", r.LatencyMs)
	}
	if r.Err != nil {
		t.Errorf("Err = %v", r.Err)
	}
}

func TestDiscover_FlagsV4WithWarning(t *testing.T) {
	srv := fakeTallyServer(t, "Release 4.1", []string{"X Co"})
	defer srv.Close()

	results, _ := Discover(context.Background(), DiscoverOptions{
		Endpoints: []string{srv.URL},
	})
	r := results[0]
	if r.Version != VersionV4 {
		t.Fatalf("Version = %v, want VersionV4", r.Version)
	}
	found := false
	for _, w := range r.Warnings {
		if strings.Contains(w, "4.x") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a 4.x warning, got %v", r.Warnings)
	}
}

func TestDiscover_NonTallyEndpointMarkedNotTally(t *testing.T) {
	// Some other HTTP service that returns a 200 with HTML.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html><body>Jenkins</body></html>"))
	}))
	defer srv.Close()

	results, _ := Discover(context.Background(), DiscoverOptions{
		Endpoints: []string{srv.URL},
	})
	r := results[0]
	if !r.Reachable {
		t.Errorf("HTTP 200 endpoint should be Reachable=true")
	}
	if r.IsTally {
		t.Errorf("non-Tally response should NOT set IsTally=true")
	}
	if r.Err == nil {
		t.Errorf("expected an Err describing 'not Tally-shaped'")
	}
}

func TestDiscover_RefusedConnectionIsErrNotTally(t *testing.T) {
	results, _ := Discover(context.Background(), DiscoverOptions{
		// Reserved-for-tests port that won't be listening.
		Endpoints: []string{"http://127.0.0.1:1"},
		Timeout:   500 * time.Millisecond,
	})
	r := results[0]
	if r.Reachable {
		t.Errorf("closed port should not be Reachable")
	}
	if r.Err == nil {
		t.Errorf("expected an Err for closed port")
	}
	if r.IsTally {
		t.Errorf("closed port should not be IsTally")
	}
}

func TestDiscover_ParallelScan(t *testing.T) {
	srvA := fakeTallyServer(t, "Release 3.0.1", []string{"Co A"})
	defer srvA.Close()
	srvB := fakeTallyServer(t, "Release 3.0.1", []string{"Co B"})
	defer srvB.Close()
	srvC := fakeTallyServer(t, "Release 3.0.1", []string{"Co C"})
	defer srvC.Close()

	results, _ := Discover(context.Background(), DiscoverOptions{
		Endpoints:   []string{srvA.URL, srvB.URL, srvC.URL},
		Concurrency: 3,
	})
	if len(results) != 3 {
		t.Fatalf("len=%d, want 3", len(results))
	}
	// Order preserved by index even with parallel workers.
	if results[0].Companies[0].Name != "Co A" {
		t.Errorf("results[0] = %+v", results[0].Companies)
	}
	if results[1].Companies[0].Name != "Co B" {
		t.Errorf("results[1] = %+v", results[1].Companies)
	}
	if results[2].Companies[0].Name != "Co C" {
		t.Errorf("results[2] = %+v", results[2].Companies)
	}
}

func TestDiscover_DedupesExplicitEndpoints(t *testing.T) {
	srv := fakeTallyServer(t, "Release 3.0.1", []string{"X"})
	defer srv.Close()

	var hits int32
	mw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		if strings.Contains(string(body), "$$Version") {
			_, _ = w.Write([]byte("<ENVELOPE><VERSIONNUMBER>Release 3.0</VERSIONNUMBER></ENVELOPE>"))
			return
		}
		_, _ = w.Write([]byte("<ENVELOPE><BODY><DATA><COMPANY><NAME>X</NAME></COMPANY></DATA></BODY></ENVELOPE>"))
	}))
	defer mw.Close()

	results, _ := Discover(context.Background(), DiscoverOptions{
		// Same URL repeated three times → one probe.
		Endpoints: []string{mw.URL, mw.URL, mw.URL},
	})
	if len(results) != 1 {
		t.Errorf("dedup failed: got %d results", len(results))
	}
	// Each endpoint probe makes 2 HTTP calls (version + company-list).
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits = %d, want 2 (= 1 dedupd probe × 2 calls)", got)
	}
}

func TestResolveEndpoints_PortRangeFromZeroValue(t *testing.T) {
	endpoints, err := resolveEndpoints(DiscoverOptions{})
	if err != nil {
		t.Fatalf("resolveEndpoints: %v", err)
	}
	rangeCount := DefaultDiscoverPortRange[1] - DefaultDiscoverPortRange[0] + 1
	want := rangeCount + len(DefaultDiscoverAlternates)
	if len(endpoints) != want {
		t.Errorf("len=%d, want %d (range %d + alternates %d)",
			len(endpoints), want, rangeCount, len(DefaultDiscoverAlternates))
	}
	if !strings.HasPrefix(endpoints[0], "http://"+DefaultDiscoverHost+":9000") {
		t.Errorf("first endpoint = %s", endpoints[0])
	}
	// Alternates appear after the port-range block.
	hasPort := func(port int) bool {
		needle := fmt.Sprintf(":%d", port)
		for _, e := range endpoints {
			if strings.Contains(e, needle) {
				return true
			}
		}
		return false
	}
	for _, alt := range DefaultDiscoverAlternates {
		if !hasPort(alt) {
			t.Errorf("expected alternate port %d in default sweep, got %v", alt, endpoints)
		}
	}
}

func TestResolveEndpoints_NarrowRange_NoAlternates(t *testing.T) {
	// A caller who passes an explicit narrow port range like 5000-5010
	// should NOT get the default alternates grafted in.
	endpoints, err := resolveEndpoints(DiscoverOptions{PortRange: [2]int{5000, 5002}})
	if err != nil {
		t.Fatalf("resolveEndpoints: %v", err)
	}
	if len(endpoints) != 3 {
		t.Errorf("len=%d, want 3 (just the explicit range)", len(endpoints))
	}
	for _, e := range endpoints {
		if strings.Contains(e, ":2026") || strings.Contains(e, ":8989") {
			t.Errorf("explicit narrow range should not include alternate ports: %s", e)
		}
	}
}

func TestResolveEndpoints_RejectsBadRange(t *testing.T) {
	cases := [][2]int{
		{9100, 9000}, // end before start
		{0, 9999},    // start out of range (after PortRange[0]=0 promotes to default? no — only zero-value)
		{70000, 70001},
	}
	for i, pr := range cases {
		_, err := resolveEndpoints(DiscoverOptions{PortRange: pr})
		// The (0, 9999) case should NOT error — zero in PortRange[0]
		// is the default sentinel, but only when PortRange[1] is also
		// zero. Here PortRange[1]=9999 means start<1 triggers.
		if pr == [2]int{0, 9999} {
			if err == nil {
				t.Errorf("case %d (%v): expected error for start=0", i, pr)
			}
			continue
		}
		if err == nil {
			t.Errorf("case %d (%v): expected error", i, pr)
		}
	}
}

func TestDiscover_ContextCancellation(t *testing.T) {
	// Hangs forever — probe must respect ctx.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err := Discover(ctx, DiscoverOptions{
		Endpoints: []string{srv.URL},
		Timeout:   5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len=%d", len(results))
	}
	if results[0].Err == nil || !errors.Is(results[0].Err, context.Canceled) && !strings.Contains(results[0].Err.Error(), "deadline") && !strings.Contains(results[0].Err.Error(), "context") {
		t.Errorf("expected cancellation error, got %v", results[0].Err)
	}
}
