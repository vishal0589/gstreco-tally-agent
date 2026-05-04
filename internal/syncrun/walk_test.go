package syncrun

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/ingest"
)

// Reuse fakeTally + fakeSender + stubResponse + makeReq from runner_test.go
// (they're in the same package).

func makeMappings(n int) []ingest.ActiveMapping {
	out := make([]ingest.ActiveMapping, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, ingest.ActiveMapping{
			MappingID:        "m-" + string(rune('a'+i)),
			TallyEndpoint:    "http://localhost:" + string(rune('0'+i)),
			TallyCompanyName: "Co " + string(rune('A'+i)),
			CompanyGSTINID:   "g-" + string(rune('a'+i)),
		})
	}
	return out
}

const stubTaxLedgerResponse = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY><DATA><COLLECTION>
    <LEDGER>
      <NAME>Input IGST</NAME>
      <PARENT>Duties &amp; Taxes</PARENT>
      <PERIODOPENINGBALANCE>100.00</PERIODOPENINGBALANCE>
      <PERIODDEBITTOTALS>1200.00</PERIODDEBITTOTALS>
      <PERIODCREDITTOTALS>300.00</PERIODCREDITTOTALS>
      <PERIODCLOSINGBALANCE>1000.00</PERIODCLOSINGBALANCE>
    </LEDGER>
  </COLLECTION></DATA></BODY>
</ENVELOPE>`

func TestWalkAll_HappyPath_AllMappingsAllKinds(t *testing.T) {
	tly := &fakeTally{
		respond: func(body []byte) []byte {
			if bytes.Contains(body, []byte("GstrecoTaxLedgerCollection")) {
				return []byte(stubTaxLedgerResponse)
			}
			return []byte(stubResponse)
		},
	}
	snd := &fakeSender{}
	mappings := makeMappings(2)

	res := WalkAll(context.Background(), WalkOptions{
		Mappings:        mappings,
		Kinds:           DefaultKinds,
		From:            time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:              time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		RunIDPrefix:     "test",
		RunKind:         "manual",
		ContinueOnError: true,
		NewTallyClient: func(string) TallyPoster {
			return tly
		},
		Sender: snd,
	})
	if res.MappingsRun != 2 {
		t.Errorf("MappingsRun=%d, want 2", res.MappingsRun)
	}
	// 2 mappings × 7 kinds × 1 row per stub response = 14 rows total.
	if res.TotalRows != 14 {
		t.Errorf("TotalRows=%d, want 14", res.TotalRows)
	}
	if res.TotalBatchesSent != 14 {
		t.Errorf("TotalBatchesSent=%d, want 14", res.TotalBatchesSent)
	}
	if res.FatalErrors != 0 {
		t.Errorf("FatalErrors=%d, want 0", res.FatalErrors)
	}
}

func TestWalkAll_ContinuesOnFatalErrorWhenFlagTrue(t *testing.T) {
	// Tally returns malformed XML → ParseDayBookV3 errors fatally for
	// every kind. With ContinueOnError=true, all 2 mappings × 4 kinds
	// = 8 attempts hit fatal errors, but the walk doesn't stop early.
	tly := &fakeTally{resp: []byte("<broken")}
	snd := &fakeSender{}
	mappings := makeMappings(2)

	res := WalkAll(context.Background(), WalkOptions{
		Mappings:        mappings,
		Kinds:           []KindPair{{Name: "purchase"}}, // narrow to 1 kind for clarity
		ContinueOnError: true,
		NewTallyClient:  func(string) TallyPoster { return tly },
		Sender:          snd,
		From:            time.Now(),
		To:              time.Now(),
		RunIDPrefix:     "test",
		RunKind:         "manual",
	})
	if res.MappingsRun != 0 {
		t.Errorf("MappingsRun=%d, want 0", res.MappingsRun)
	}
	if res.FatalErrors != 2 {
		t.Errorf("FatalErrors=%d, want 2 (one per mapping)", res.FatalErrors)
	}
	if res.StoppedEarly {
		t.Error("StoppedEarly=true; want false with ContinueOnError")
	}
}

func TestWalkAll_StopsEarlyWhenContinueFalse(t *testing.T) {
	tly := &fakeTally{resp: []byte("<broken")}
	snd := &fakeSender{}
	mappings := makeMappings(3)
	hookCalls := int32(0)

	res := WalkAll(context.Background(), WalkOptions{
		Mappings:        mappings,
		Kinds:           []KindPair{{Name: "purchase"}},
		ContinueOnError: false,
		NewTallyClient:  func(string) TallyPoster { return tly },
		Sender:          snd,
		From:            time.Now(),
		To:              time.Now(),
		RunIDPrefix:     "test",
		RunKind:         "manual",
		PerMappingHook: func(_, _ int, _ ingest.ActiveMapping) {
			atomic.AddInt32(&hookCalls, 1)
		},
	})
	if !res.StoppedEarly {
		t.Error("StoppedEarly=false; want true")
	}
	if res.FatalErrors != 1 {
		t.Errorf("FatalErrors=%d, want 1 (stops after first fatal)", res.FatalErrors)
	}
	if got := atomic.LoadInt32(&hookCalls); got != 1 {
		t.Errorf("PerMappingHook fired %d times, want 1 (stopped after first mapping's fatal)", got)
	}
}

func TestWalkAll_HookFiringOrder(t *testing.T) {
	tly := &fakeTally{resp: []byte(stubResponse)}
	snd := &fakeSender{}
	mappings := makeMappings(2)
	events := []string{}

	WalkAll(context.Background(), WalkOptions{
		Mappings:        mappings,
		Kinds:           []KindPair{{Name: "purchase"}, {Name: "sales"}},
		ContinueOnError: true,
		NewTallyClient:  func(string) TallyPoster { return tly },
		Sender:          snd,
		From:            time.Now(),
		To:              time.Now(),
		RunIDPrefix:     "test",
		RunKind:         "manual",
		PerMappingHook: func(idx, total int, m ingest.ActiveMapping) {
			events = append(events, "mapping:"+m.TallyCompanyName)
		},
		PerKindResultHook: func(m ingest.ActiveMapping, k KindPair, _ Result, _ error) {
			events = append(events, "kind:"+m.TallyCompanyName+":"+k.Name)
		},
	})
	want := []string{
		"mapping:Co A",
		"kind:Co A:purchase",
		"kind:Co A:sales",
		"mapping:Co B",
		"kind:Co B:purchase",
		"kind:Co B:sales",
	}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i] != w {
			t.Errorf("event[%d]=%q, want %q", i, events[i], w)
		}
	}
}

func TestWalkAll_EmptyMappingsReturnsZeroes(t *testing.T) {
	res := WalkAll(context.Background(), WalkOptions{Mappings: nil})
	if res.MappingsRun != 0 || res.TotalRows != 0 || res.FatalErrors != 0 {
		t.Errorf("non-zero result on empty input: %+v", res)
	}
}

func TestWalkAll_DefaultKindsFallback(t *testing.T) {
	tly := &fakeTally{
		respond: func(body []byte) []byte {
			if bytes.Contains(body, []byte("GstrecoTaxLedgerCollection")) {
				return []byte(stubTaxLedgerResponse)
			}
			return []byte(stubResponse)
		},
	}
	snd := &fakeSender{}
	res := WalkAll(context.Background(), WalkOptions{
		Mappings: makeMappings(1),
		// Kinds intentionally nil — should default to the full supported set.
		ContinueOnError: true,
		NewTallyClient:  func(string) TallyPoster { return tly },
		Sender:          snd,
		From:            time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
		To:              time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC),
		RunIDPrefix:     "test",
		RunKind:         "manual",
	})
	if res.TotalRows != 7 {
		t.Errorf("TotalRows=%d, want 7 (1 mapping × 7 default kinds × 1 row per stub)", res.TotalRows)
	}
	if res.FatalErrors != 0 {
		t.Errorf("FatalErrors=%d, want 0", res.FatalErrors)
	}
}

func TestMapKindByName(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"purchase", "purchase", false},
		{"sales", "sales", false},
		{"credit_note", "credit_note", false},
		{"credit-note", "credit_note", false},
		{"creditnote", "credit_note", false},
		{"debit_note", "debit_note", false},
		{"debit-note", "debit_note", false},
		{"journal", "journal", false},
		{"payment", "payment", false},
		{"tax_ledger", "tax_ledger", false},
		{"tax-ledger", "tax_ledger", false},
		{"", "", true},
	}
	for _, c := range cases {
		k, err := MapKindByName(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("MapKindByName(%q) err=%v wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if !c.wantErr && k.Name != c.want {
			t.Errorf("MapKindByName(%q).Name = %q, want %q", c.in, k.Name, c.want)
		}
	}
}

// Verify that the per-run progress callback receives the right
// kind name so the CLI/daemon can prefix log lines correctly.
func TestWalkAll_PerRunProgressIsKeyed(t *testing.T) {
	tly := &fakeTally{resp: []byte(stubResponse)}
	snd := &fakeSender{}
	progressKinds := []string{}

	WalkAll(context.Background(), WalkOptions{
		Mappings:        makeMappings(1),
		Kinds:           []KindPair{{Name: "purchase"}, {Name: "sales"}},
		ContinueOnError: true,
		From:            time.Now(),
		To:              time.Now(),
		RunIDPrefix:     "test",
		RunKind:         "manual",
		NewTallyClient:  func(string) TallyPoster { return tly },
		Sender:          snd,
		PerRunProgress: func(_ ingest.ActiveMapping, k KindPair) Progress {
			progressKinds = append(progressKinds, k.Name)
			return func(Event) {}
		},
	})
	if len(progressKinds) != 2 {
		t.Errorf("PerRunProgress called %d times, want 2", len(progressKinds))
	}
	if progressKinds[0] != "purchase" || progressKinds[1] != "sales" {
		t.Errorf("PerRunProgress order = %v", progressKinds)
	}
}

// Sanity check that ctx cancellation propagates to RunOne via the
// per-run derived ctx.
func TestWalkAll_CtxCancellationPropagates(t *testing.T) {
	// Tally blocks until ctx done.
	tly := &slowTally{}
	snd := &fakeSender{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	res := WalkAll(ctx, WalkOptions{
		Mappings:        makeMappings(1),
		Kinds:           []KindPair{{Name: "purchase"}},
		ContinueOnError: true,
		From:            time.Now(),
		To:              time.Now(),
		RunIDPrefix:     "test",
		RunKind:         "manual",
		PerRunTimeout:   100 * time.Millisecond,
		NewTallyClient:  func(string) TallyPoster { return tly },
		Sender:          snd,
	})
	// At least one fatal error from the cancelled fetch.
	if res.FatalErrors == 0 {
		t.Errorf("FatalErrors=0; want >=1 from ctx cancel")
	}
}

type slowTally struct{}

func (s *slowTally) PostXML(ctx context.Context, _ []byte) ([]byte, error) {
	<-ctx.Done()
	return nil, errors.New("ctx done")
}
