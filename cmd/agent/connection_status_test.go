package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/heartbeat"
	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// All credentials below are obviously fake test values.
const (
	fakeSecretA = "ZmFrZS1obWFjLXNlY3JldC1mb3ItY29ubi1h" // base64("fake-hmac-secret-for-conn-a")
	fakeSecretB = "ZmFrZS1obWFjLXNlY3JldC1mb3ItY29ubi1i" // base64("fake-hmac-secret-for-conn-b")
	fakeBearerA = "FAKE-TEST-BEARER-AAAA-not-a-real-token-0001"
	fakeBearerB = "FAKE-TEST-BEARER-BBBB-not-a-real-token-0002"
	staleBearer = "FAKE-TEST-BEARER-STALE-not-a-real-token-9999"
)

const emptyCollection = `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION></COLLECTION></DATA></BODY></ENVELOPE>`

type tallyCo struct{ name, guid string }

// fakeTally answers the company-list probe with these companies and any
// other request with an empty collection, counting voucher fetches.
type fakeTallyServer struct {
	srv           *httptest.Server
	mu            sync.Mutex
	voucherFetchs int
	otherFetchs   int
}

func newFakeTally(t *testing.T, companies ...tallyCo) *fakeTallyServer {
	t.Helper()
	ft := &fakeTallyServer{}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case bytes.Contains(body, []byte("List of Companies")):
			var b strings.Builder
			b.WriteString("<ENVELOPE><BODY><DATA>")
			for _, c := range companies {
				b.WriteString("<COMPANY><NAME>" + c.name + "</NAME>")
				if c.guid != "" {
					b.WriteString("<GUID>" + c.guid + "</GUID>")
				}
				b.WriteString("</COMPANY>")
			}
			b.WriteString("</DATA></BODY></ENVELOPE>")
			_, _ = w.Write([]byte(b.String()))
		case bytes.Contains(body, []byte("$$Version")):
			_, _ = w.Write([]byte("<RESPONSE>Could not find: $$Version</RESPONSE>"))
		default:
			ft.mu.Lock()
			if bytes.Contains(body, []byte("GstrecoVoucherCollection")) {
				ft.voucherFetchs++
			} else {
				ft.otherFetchs++
			}
			ft.mu.Unlock()
			_, _ = w.Write([]byte(emptyCollection))
		}
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTallyServer) fetches() (vouchers, other int) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.voucherFetchs, ft.otherFetchs
}

// fakeGSTReco is a server that knows each connection's bearer and active
// mappings, rejects a mismatched bearer like the real one does, and
// records which bearer each connection presented.
type fakeGSTReco struct {
	srv *httptest.Server
	mu  sync.Mutex
	// validBearer: connection_id -> bearer the server accepts.
	validBearer map[string]string
	mappings    map[string][]ingest.ActiveMapping
	// presented: connection_id -> bearers seen (compared in-test, never printed).
	presented  map[string]map[string]bool
	ingests    map[string]int
	heartbeats map[string][]ingest.HeartbeatRequest
	// echoAuth makes the 401 body echo the presented Authorization
	// header, as a misbehaving proxy might (redaction test).
	echoAuth bool
}

func newFakeGSTReco(t *testing.T) *fakeGSTReco {
	t.Helper()
	f := &fakeGSTReco{
		validBearer: map[string]string{},
		mappings:    map[string][]ingest.ActiveMapping{},
		presented:   map[string]map[string]bool{},
		ingests:     map[string]int{},
		heartbeats:  map[string][]ingest.HeartbeatRequest{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connID := r.Header.Get(tally.HeaderConnectionID)
		auth := r.Header.Get(tally.HeaderAuth)
		bearer := strings.TrimPrefix(auth, "Bearer ")
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		if f.presented[connID] == nil {
			f.presented[connID] = map[string]bool{}
		}
		f.presented[connID][bearer] = true
		ok := bearer != "" && bearer == f.validBearer[connID]
		echo := f.echoAuth
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			msg := `{"error":"invalid_bearer","message":"bearer token does not match this connection"`
			if echo {
				msg += `,"seen":"` + auth + `","token":"` + bearer + `"`
			}
			_, _ = w.Write([]byte(msg + "}"))
			return
		}
		switch {
		case r.URL.Path == ingest.ActiveMappingsPathFor(connID):
			f.mu.Lock()
			ms := f.mappings[connID]
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(ingest.ActiveMappingsResponse{ConnectionID: connID, Mappings: ms})
		case r.URL.Path == ingest.HeartbeatPath:
			var hb ingest.HeartbeatRequest
			_ = json.Unmarshal(body, &hb)
			f.mu.Lock()
			f.heartbeats[connID] = append(f.heartbeats[connID], hb)
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"pending_actions":[],"schedule_cron":"","server_time":""}`))
		case r.URL.Path == ingest.Path:
			f.mu.Lock()
			f.ingests[connID]++
			f.mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true,"run_id":"r","counters":{}}`))
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGSTReco) sawOnly(connID, bearer string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	seen := f.presented[connID]
	return len(seen) == 1 && seen[bearer]
}

func newTestRuntime(t *testing.T, server, connID, bearer, secret string) *daemonConnectionRuntime {
	t.Helper()
	c, err := ingest.NewClient(server, connID, bearer, secret)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return newDaemonConnectionRuntime(config.PairedConnection{Server: server, ConnectionID: connID}, c)
}

func syncWindow() (time.Time, time.Time) {
	return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)
}

// Two connections in one process: A's token is stale (401), B is fine.
// B's sync and B's connection_status must be untouched by A's failure,
// and each heartbeat must carry only its own connection's status.
func TestTwoConnections_401OnOneNeverBecomesTheOthersStatus(t *testing.T) {
	tl := newFakeTally(t, tallyCo{"Looms Guild Pvt Ltd", "11111111-1111-1111-1111-111111111111"})
	srv := newFakeGSTReco(t)
	srv.validBearer["conn-a"] = fakeBearerA // A presents a stale one below
	srv.validBearer["conn-b"] = fakeBearerB
	srv.mappings["conn-b"] = []ingest.ActiveMapping{{
		MappingID: "map-looms", TallyEndpoint: tl.srv.URL,
		TallyCompanyName: "Looms Guild Pvt Ltd", TallyCompanyGUID: "11111111-1111-1111-1111-111111111111",
		CompanyGSTINID: "gstin-looms",
	}}

	a := newTestRuntime(t, srv.srv.URL, "conn-a", staleBearer, fakeSecretA)
	b := newTestRuntime(t, srv.srv.URL, "conn-b", fakeBearerB, fakeSecretB)
	runtimes := []*daemonConnectionRuntime{a, b}

	// Scheduled tick across both, recorded in the legacy global state.
	ts := newTickState()
	from, to := syncWindow()
	tick := wrapTickWithState(func(ctx context.Context) error {
		return runWindowSyncAllConnections(ctx, runtimes, tally.NewClient, zerolog.Nop(), from, to, "incremental")
	}, ts)
	if err := tick(context.Background()); err == nil {
		t.Fatal("tick with a rejected connection should report an error")
	}

	sa, sb := a.status.snapshot(), b.status.snapshot()
	if sa.Result != ingest.ConnStatusRejectedAuth || sa.HTTPStatus == nil || *sa.HTTPStatus != 401 ||
		sa.ErrorCode == nil || *sa.ErrorCode != "invalid_bearer" {
		t.Fatalf("A status = %+v, want rejected_auth 401 invalid_bearer", sa)
	}
	if sb.Result != ingest.ConnStatusSuccess {
		t.Fatalf("B status = %s (%v), want success: A's 401 leaked into B", sb.Result, deref(sb.Detail))
	}
	if sb.ErrorCode != nil || (sb.Detail != nil && strings.Contains(*sb.Detail, "bearer")) {
		t.Fatalf("B status carries A's auth failure: %+v", sb)
	}
	if sb.MappingID == nil || *sb.MappingID != "map-looms" {
		t.Fatalf("B mapping_id = %v, want map-looms", sb.MappingID)
	}
	if v, _ := tl.fetches(); v == 0 {
		t.Fatal("B's voucher fetch did not run")
	}
	// Each connection presented only its own bearer.
	if !srv.sawOnly("conn-a", staleBearer) || !srv.sawOnly("conn-b", fakeBearerB) {
		t.Fatal("a connection presented another connection's bearer")
	}
	// Legacy global status still exists (compat) — it is no longer the
	// only signal.
	if _, legacy := ts.report(); !strings.HasPrefix(legacy, "error: ") {
		t.Fatalf("legacy last_tick_status = %q, want the agent-wide error kept", legacy)
	}

	// Now B's own sync_now runs; A's earlier failure must not touch it.
	single := makeSingleConnectionTickFunc(b, tally.NewClient, zerolog.Nop())
	_ = single(context.Background())
	if got := b.status.snapshot().Result; got != sb.Result {
		t.Fatalf("B status after its own sync = %s, want %s", got, sb.Result)
	}

	// Heartbeats: each poller reports its own connection's status.
	srv.validBearer["conn-a"] = staleBearer // let A's heartbeat through to inspect its body
	for _, rt := range runtimes {
		p, err := heartbeat.New(heartbeat.Options{
			Client:                   rt.client,
			Handler:                  noopHandler{},
			Logger:                   zerolog.Nop(),
			Interval:                 time.Hour,
			LastTickReporter:         ts.report,
			ConnectionStatusReporter: rt.status.snapshot,
			OnHeartbeatResult:        rt.status.recordHeartbeat,
		})
		if err != nil {
			t.Fatal(err)
		}
		p.Start(context.Background())
		waitFor(t, func() bool {
			srv.mu.Lock()
			defer srv.mu.Unlock()
			return len(srv.heartbeats[rt.info.ConnectionID]) > 0
		})
		p.Stop()
	}
	srv.mu.Lock()
	hbA, hbB := srv.heartbeats["conn-a"][0], srv.heartbeats["conn-b"][0]
	srv.mu.Unlock()
	if hbA.ConnectionStatus == nil || hbA.ConnectionStatus.Result != ingest.ConnStatusRejectedAuth {
		t.Fatalf("A heartbeat connection_status = %+v, want rejected_auth", hbA.ConnectionStatus)
	}
	if hbB.ConnectionStatus == nil || hbB.ConnectionStatus.Result != sb.Result {
		t.Fatalf("B heartbeat connection_status = %+v, want %s", hbB.ConnectionStatus, sb.Result)
	}
	if hbB.ConnectionStatus.SyncRequestID != nil {
		t.Fatal("sync_request_id must stay null until the server sends one")
	}
}

// Each connection must read its own keyring entries (by key name).
func TestLoadConnectionRuntimes_EachConnectionUsesItsOwnEntries(t *testing.T) {
	srv := newFakeGSTReco(t)
	srv.validBearer["conn-kbs"] = fakeBearerA
	srv.validBearer["conn-kss"] = fakeBearerB
	ks := keyring.NewMemoryStore()
	for id, vals := range map[string][2]string{
		"conn-kbs": {fakeSecretA, fakeBearerA},
		"conn-kss": {fakeSecretB, fakeBearerB},
	} {
		hk, bk := keyring.ConnectionKeys(id)
		_ = ks.Set(keyring.ServiceName, hk, vals[0])
		_ = ks.Set(keyring.ServiceName, bk, vals[1])
	}
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	conns := []config.PairedConnection{
		{Server: srv.srv.URL, ConnectionID: "conn-kbs"},
		{Server: srv.srv.URL, ConnectionID: "conn-kss"},
		{Server: srv.srv.URL, ConnectionID: "conn-missing"},
	}
	runtimes := loadConnectionRuntimes(conns, ks, logger)
	if len(runtimes) != 2 {
		t.Fatalf("runtimes = %d, want 2 (missing entry skipped)", len(runtimes))
	}
	for _, rt := range runtimes {
		if _, err := rt.client.FetchActiveMappings(context.Background()); err != nil {
			t.Fatalf("%s: server rejected its own bearer: %v", rt.info.ConnectionID, err)
		}
	}
	if !srv.sawOnly("conn-kbs", fakeBearerA) || !srv.sawOnly("conn-kss", fakeBearerB) {
		t.Fatal("a connection was given another connection's bearer")
	}
	out := logs.String()
	for _, want := range []string{"conn-kbs.bearer_token", "conn-kss.hmac_secret"} {
		if !strings.Contains(out, want) {
			t.Fatalf("log should name key %q; got %s", want, out)
		}
	}
	for _, secret := range []string{fakeBearerA, fakeBearerB, fakeSecretA, fakeSecretB} {
		if strings.Contains(out, secret) {
			t.Fatal("log contains a credential value")
		}
	}
}

// KSS mapped, Colouredge open on the selected port: no fetch, and a
// wrong_company_open status naming both companies.
func TestWrongCompanyOpen_SkipsTheFetch(t *testing.T) {
	tl := newFakeTally(t, tallyCo{"Colouredge Prints", "22222222-2222-2222-2222-222222222222"})
	srv := newFakeGSTReco(t)
	srv.validBearer["conn-kss"] = fakeBearerB
	srv.mappings["conn-kss"] = []ingest.ActiveMapping{{
		MappingID: "map-kss", TallyEndpoint: tl.srv.URL, TallyCompanyName: "KSS Textiles",
		TallyCompanyGUID: "33333333-3333-3333-3333-333333333333",
	}}
	rt := newTestRuntime(t, srv.srv.URL, "conn-kss", fakeBearerB, fakeSecretB)
	from, to := syncWindow()
	err := runWindowSync(context.Background(), rt, tally.NewClient, zerolog.Nop(), from, to, "manual")
	if err == nil {
		t.Fatal("want an error naming the unreadable company")
	}
	if v, o := tl.fetches(); v != 0 || o != 0 {
		t.Fatalf("Tally fetches = %d vouchers, %d other; want none for a company that is not open", v, o)
	}
	if srv.ingests["conn-kss"] != 0 {
		t.Fatal("nothing should be ingested")
	}
	st := rt.status.snapshot()
	if st.Result != ingest.ConnStatusWrongCompanyOpen {
		t.Fatalf("result = %s, want wrong_company_open", st.Result)
	}
	if st.ExpectedCompany == nil || deref(st.ExpectedCompany.Name) != "KSS Textiles" ||
		deref(st.ExpectedCompany.GUID) != "33333333-3333-3333-3333-333333333333" {
		t.Fatalf("expected_company = %+v", st.ExpectedCompany)
	}
	if len(st.OpenCompanies) != 1 || deref(st.OpenCompanies[0].Name) != "Colouredge Prints" {
		t.Fatalf("open_companies = %+v, want Colouredge", st.OpenCompanies)
	}
	d := deref(st.Detail)
	if !strings.Contains(d, "KSS Textiles") || !strings.Contains(d, "Colouredge Prints") || len(d) > ingest.MaxConnectionStatusDetail {
		t.Fatalf("detail = %q, want both companies named, <= 300 chars", d)
	}
	if ep := deref(st.TallyEndpoint); strings.Contains(ep, "://") || ep != strings.TrimPrefix(tl.srv.URL, "http://") {
		t.Fatalf("tally_endpoint = %q, want host:port", ep)
	}
}

// Same name on the mapped port, different GUID: another company, no fetch.
func TestSameNameDifferentGUID_SkipsTheFetch(t *testing.T) {
	tl := newFakeTally(t, tallyCo{"KBS Exports", "44444444-4444-4444-4444-444444444444"})
	srv := newFakeGSTReco(t)
	srv.validBearer["conn-kbs"] = fakeBearerA
	srv.mappings["conn-kbs"] = []ingest.ActiveMapping{{
		MappingID: "map-kbs", TallyEndpoint: tl.srv.URL, TallyCompanyName: "KBS Exports",
		TallyCompanyGUID: "55555555-5555-5555-5555-555555555555",
	}}
	rt := newTestRuntime(t, srv.srv.URL, "conn-kbs", fakeBearerA, fakeSecretA)
	from, to := syncWindow()
	_ = runWindowSync(context.Background(), rt, tally.NewClient, zerolog.Nop(), from, to, "manual")
	if v, o := tl.fetches(); v != 0 || o != 0 {
		t.Fatalf("fetched from a same-name company with a different GUID (%d/%d)", v, o)
	}
	if st := rt.status.snapshot(); st.Result != ingest.ConnStatusWrongCompanyOpen || !strings.Contains(deref(st.Detail), "GUID") {
		t.Fatalf("status = %s %q, want wrong_company_open naming the GUID", st.Result, deref(st.Detail))
	}
}

// Port refused: endpoint_unreachable.
func TestEndpointRefused_IsEndpointUnreachable(t *testing.T) {
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	srv := newFakeGSTReco(t)
	srv.validBearer["conn-kbs"] = fakeBearerA
	srv.mappings["conn-kbs"] = []ingest.ActiveMapping{{MappingID: "map-kbs", TallyEndpoint: closedURL, TallyCompanyName: "KBS Exports"}}
	rt := newTestRuntime(t, srv.srv.URL, "conn-kbs", fakeBearerA, fakeSecretA)
	from, to := syncWindow()
	_ = runWindowSync(context.Background(), rt, tally.NewClient, zerolog.Nop(), from, to, "manual")
	st := rt.status.snapshot()
	if st.Result != ingest.ConnStatusEndpointUnreachable {
		t.Fatalf("result = %s (%s), want endpoint_unreachable", st.Result, deref(st.Detail))
	}
	if st.OpenCompanies != nil {
		t.Fatalf("open_companies = %+v, want null when Tally did not answer", st.OpenCompanies)
	}
	if deref(st.MappingID) != "map-kbs" || !strings.Contains(deref(st.Detail), "KBS Exports") {
		t.Fatalf("status = %+v", st)
	}
}

func TestNoActiveMapping_IsMappingMismatch(t *testing.T) {
	srv := newFakeGSTReco(t)
	srv.validBearer["conn-x"] = fakeBearerA
	rt := newTestRuntime(t, srv.srv.URL, "conn-x", fakeBearerA, fakeSecretA)
	from, to := syncWindow()
	_ = runWindowSync(context.Background(), rt, tally.NewClient, zerolog.Nop(), from, to, "manual")
	if st := rt.status.snapshot(); st.Result != ingest.ConnStatusMappingMismatch {
		t.Fatalf("result = %s, want mapping_mismatch", st.Result)
	}
}

func TestConnectionStatusIsIdleBeforeAnySync(t *testing.T) {
	if st := newConnStatusTracker().snapshot(); st.Result != ingest.ConnStatusIdle || st.CheckedAt == "" {
		t.Fatalf("snapshot = %+v, want idle with checked_at", st)
	}
}

// A server (or proxy) echoing the bearer back must not put it in the
// agent log or in connection_status.
func TestRedaction_NoTokenInLogsOrConnectionStatus(t *testing.T) {
	srv := newFakeGSTReco(t)
	srv.echoAuth = true
	srv.validBearer["conn-dw"] = fakeBearerA // presented: staleBearer
	rt := newTestRuntime(t, srv.srv.URL, "conn-dw", staleBearer, fakeSecretA)
	var logs bytes.Buffer
	logger := zerolog.New(&logs)
	from, to := syncWindow()
	_ = runWindowSyncAllConnections(context.Background(), []*daemonConnectionRuntime{rt}, tally.NewClient, logger, from, to, "incremental")
	rt.status.recordHeartbeat(func() error {
		_, err := rt.client.Heartbeat(context.Background(), ingest.HeartbeatRequest{})
		return err
	}())
	st := rt.status.snapshot()
	body, _ := json.Marshal(st)
	if !strings.Contains(logs.String(), "invalid_bearer") {
		t.Fatalf("log should still show the error code; got %s", logs.String())
	}
	for _, where := range map[string]string{"log": logs.String(), "connection_status": string(body)} {
		for _, secret := range []string{staleBearer, fakeBearerA, fakeSecretA} {
			if strings.Contains(where, secret) {
				t.Fatal("a credential value leaked into the log or connection_status")
			}
		}
	}
	if st.Result != ingest.ConnStatusRejectedAuth {
		t.Fatalf("result = %s, want rejected_auth", st.Result)
	}
}

func TestHeartbeatAuthClearsOnNextAcceptedHeartbeat(t *testing.T) {
	tr := newConnStatusTracker()
	tr.recordSync(ingest.ConnectionStatus{Result: ingest.ConnStatusSuccess})
	tr.recordHeartbeat(&ingest.SendError{Kind: ingest.ErrorKindAuth, Status: 401, Snippet: `{"error":"invalid_bearer"}`})
	if got := tr.snapshot(); got.Result != ingest.ConnStatusRejectedAuth || deref(got.ErrorCode) != "invalid_bearer" {
		t.Fatalf("after 401 heartbeat: %+v", got)
	}
	tr.recordHeartbeat(&ingest.SendError{Kind: ingest.ErrorKindNetwork})
	if got := tr.snapshot(); got.Result != ingest.ConnStatusRejectedAuth {
		t.Fatal("a network error must not clear a server rejection")
	}
	tr.recordHeartbeat(nil)
	if got := tr.snapshot(); got.Result != ingest.ConnStatusSuccess {
		t.Fatalf("after accepted heartbeat: %s, want last sync result", got.Result)
	}
}

type noopHandler struct{}

func (noopHandler) OnSyncNow(context.Context, string) error         { return nil }
func (noopHandler) OnPause(context.Context) error                   { return nil }
func (noopHandler) OnRevoke(context.Context) error                  { return nil }
func (noopHandler) OnRefetchMappings(context.Context) error         { return nil }
func (noopHandler) OnScheduleChanged(context.Context, string) error { return nil }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
