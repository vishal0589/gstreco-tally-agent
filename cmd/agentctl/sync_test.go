package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

func TestMapKind(t *testing.T) {
	cases := []struct {
		in       string
		wantTK   tally.VoucherKind
		wantIK   tally.IngestKind
		wantErr  bool
	}{
		{"purchase", tally.VoucherPurchase, tally.IngestKindPurchase, false},
		{"PURCHASE", tally.VoucherPurchase, tally.IngestKindPurchase, false},
		{"sales", tally.VoucherSales, tally.IngestKindSales, false},
		{"credit_note", tally.VoucherCreditNote, tally.IngestKindCreditNote, false},
		{"credit-note", tally.VoucherCreditNote, tally.IngestKindCreditNote, false},
		{"debit_note", tally.VoucherDebitNote, tally.IngestKindDebitNote, false},
		{"journal", "", "", true},
		{"", "", "", true},
	}
	for _, c := range cases {
		gotTK, gotIK, err := mapKind(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("mapKind(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if gotTK != c.wantTK || gotIK != c.wantIK {
			t.Errorf("mapKind(%q) = (%v,%v), want (%v,%v)",
				c.in, gotTK, gotIK, c.wantTK, c.wantIK)
		}
	}
}

func TestParsePeriod(t *testing.T) {
	cases := []struct {
		in       string
		wantFrom time.Time
		wantTo   time.Time
		wantErr  bool
	}{
		{"042026", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), false},
		{"022024", time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), false}, // leap
		{"022025", time.Date(2025, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 2, 28, 0, 0, 0, 0, time.UTC), false}, // non-leap
		{"122026", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC), false},
		{"132026", time.Time{}, time.Time{}, true}, // bad month
		{"002026", time.Time{}, time.Time{}, true},
		{"04202", time.Time{}, time.Time{}, true},  // wrong length
		{"abcdef", time.Time{}, time.Time{}, true}, // non-numeric
		{"041899", time.Time{}, time.Time{}, true}, // year out of range
	}
	for _, c := range cases {
		from, to, err := parsePeriod(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("parsePeriod(%q) err=%v, wantErr=%v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if !from.Equal(c.wantFrom) || !to.Equal(c.wantTo) {
			t.Errorf("parsePeriod(%q) = (%v,%v), want (%v,%v)",
				c.in, from, to, c.wantFrom, c.wantTo)
		}
	}
}

func TestResolveWindow(t *testing.T) {
	t.Run("period only", func(t *testing.T) {
		from, to, err := resolveWindow("042026", "", "")
		if err != nil {
			t.Fatal(err)
		}
		if from.Day() != 1 || to.Day() != 30 || from.Month() != time.April {
			t.Errorf("got from=%v to=%v", from, to)
		}
	})
	t.Run("from+to only", func(t *testing.T) {
		from, to, err := resolveWindow("", "2026-04-10", "2026-04-20")
		if err != nil {
			t.Fatal(err)
		}
		if from.Day() != 10 || to.Day() != 20 {
			t.Errorf("got from=%v to=%v", from, to)
		}
	})
	t.Run("rejects both", func(t *testing.T) {
		if _, _, err := resolveWindow("042026", "2026-04-01", "2026-04-30"); err == nil {
			t.Error("want error when both period and range given")
		}
	})
	t.Run("rejects neither", func(t *testing.T) {
		if _, _, err := resolveWindow("", "", ""); err == nil {
			t.Error("want error when no window given")
		}
	})
	t.Run("rejects to-before-from", func(t *testing.T) {
		if _, _, err := resolveWindow("", "2026-04-20", "2026-04-10"); err == nil {
			t.Error("want error when to before from")
		}
	})
	t.Run("rejects partial range", func(t *testing.T) {
		if _, _, err := resolveWindow("", "2026-04-01", ""); err == nil {
			t.Error("want error when only --from set")
		}
	})
}

// fakeTallyPoster captures the request body and returns a canned response.
type fakeTallyPoster struct {
	gotBody []byte
	resp    []byte
	err     error
}

func (f *fakeTallyPoster) PostXML(_ context.Context, body []byte) ([]byte, error) {
	f.gotBody = body
	return f.resp, f.err
}

// fakeIngestSender records every batch the command sends.
type fakeIngestSender struct {
	sent []tally.IngestRequestBody
	err  error
}

func (f *fakeIngestSender) Send(_ context.Context, body tally.IngestRequestBody) error {
	f.sent = append(f.sent, body)
	return f.err
}

// stubResponse is a minimal valid Tally Prime 3.x day-book response with one
// purchase voucher. Crafted by hand to exercise ParseDayBookV3 → Normalize →
// IngestVoucherRow without needing a real Tally instance.
const stubResponse = `<ENVELOPE>
  <HEADER><STATUS>1</STATUS></HEADER>
  <BODY>
    <DATA>
      <COLLECTION>
        <VOUCHER>
          <DATE>20260410</DATE>
          <GUID>aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee-0001</GUID>
          <ALTERID>42</ALTERID>
          <VOUCHERTYPENAME>Purchase</VOUCHERTYPENAME>
          <VOUCHERNUMBER>PI/001/26-27</VOUCHERNUMBER>
          <PARTYLEDGERNAME>SMOKE Vendors Ltd</PARTYLEDGERNAME>
          <PARTYGSTIN>29ABCDE1234F1Z5</PARTYGSTIN>
          <PLACEOFSUPPLY>Karnataka</PLACEOFSUPPLY>
          <ISINVOICE>Yes</ISINVOICE>
          <ISCANCELLED>No</ISCANCELLED>
          <ALLLEDGERENTRIES.LIST>
            <LEDGERNAME>SMOKE Vendors Ltd</LEDGERNAME>
            <AMOUNT>11800.00</AMOUNT>
          </ALLLEDGERENTRIES.LIST>
          <ALLLEDGERENTRIES.LIST>
            <LEDGERNAME>Purchases A/c</LEDGERNAME>
            <AMOUNT>-10000.00</AMOUNT>
          </ALLLEDGERENTRIES.LIST>
          <ALLLEDGERENTRIES.LIST>
            <LEDGERNAME>IGST @ 18%</LEDGERNAME>
            <GSTCLASS>IGST@18</GSTCLASS>
            <AMOUNT>-1800.00</AMOUNT>
          </ALLLEDGERENTRIES.LIST>
        </VOUCHER>
      </COLLECTION>
    </DATA>
  </BODY>
</ENVELOPE>`

func TestRunSync_HappyPath(t *testing.T) {
	tally := &fakeTallyPoster{resp: []byte(stubResponse)}
	sender := &fakeIngestSender{}
	store := keyring.NewMemoryStore()
	hk, bk := keyring.ConnectionKeys("conn-123")
	_ = store.Set(keyring.ServiceName, hk, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	_ = store.Set(keyring.ServiceName, bk, "bearer-token")

	deps := syncDeps{
		now:            func() time.Time { return time.Unix(1700000000, 0) },
		newOSKeyring:   func() keyring.Store { return store },
		newTallyClient: func(string) tallyPoster { return tally },
		newIngestClient: func(_, _, _, _ string) (ingestSender, error) {
			return sender, nil
		},
		loadConfig: func(string) (*config.Config, error) {
			return &config.Config{
				Server:       "https://example.test",
				ConnectionID: "conn-123",
			}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := runSync(&stdout, &stderr, []string{
		"--tally-company", "PLLUM CASA",
		"--kind", "purchase",
		"--period", "042026",
	}, deps)

	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s, stdout=%s", code, stderr.String(), stdout.String())
	}
	if len(sender.sent) != 1 {
		t.Fatalf("want 1 batch sent, got %d", len(sender.sent))
	}
	body := sender.sent[0]
	if body.TallyCompany != "PLLUM CASA" {
		t.Errorf("TallyCompany=%q", body.TallyCompany)
	}
	if body.Kind != "purchase" {
		t.Errorf("Kind=%q", body.Kind)
	}
	if !body.IsFinal {
		t.Errorf("IsFinal=false; single-batch run should mark final")
	}
	if len(body.Batch) != 1 {
		t.Fatalf("want 1 row in batch, got %d", len(body.Batch))
	}
	row := body.Batch[0]
	if row.InvoiceNumber != "PI/001/26-27" {
		t.Errorf("InvoiceNumber=%q", row.InvoiceNumber)
	}
	if row.InvoiceValue != 11800 {
		t.Errorf("InvoiceValue=%v", row.InvoiceValue)
	}
	if row.IGST != 1800 {
		t.Errorf("IGST=%v", row.IGST)
	}
	if !strings.Contains(stdout.String(), "✓ sync complete") {
		t.Errorf("missing success line in stdout: %s", stdout.String())
	}
	if !bytes.Contains(tally.gotBody, []byte("PLLUM CASA")) {
		t.Errorf("envelope did not include company name; body=%s", string(tally.gotBody))
	}
	if !bytes.Contains(tally.gotBody, []byte("20260401")) || !bytes.Contains(tally.gotBody, []byte("20260430")) {
		t.Errorf("envelope did not include period dates; body=%s", string(tally.gotBody))
	}
}

func TestRunSync_DryRunSkipsKeyringAndSend(t *testing.T) {
	tally := &fakeTallyPoster{resp: []byte(stubResponse)}
	sender := &fakeIngestSender{err: errors.New("should not be called")}
	store := keyring.NewMemoryStore() // intentionally empty — Get would fail

	deps := syncDeps{
		now:            time.Now,
		newOSKeyring:   func() keyring.Store { return store },
		newTallyClient: func(string) tallyPoster { return tally },
		newIngestClient: func(_, _, _, _ string) (ingestSender, error) {
			return sender, nil
		},
		loadConfig: func(string) (*config.Config, error) {
			return &config.Config{Server: "https://example.test", ConnectionID: "conn-123"}, nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := runSync(&stdout, &stderr, []string{
		"--tally-company", "PLLUM CASA",
		"--kind", "purchase",
		"--period", "042026",
		"--dry-run",
	}, deps)

	if code != 0 {
		t.Fatalf("exit=%d, stderr=%s", code, stderr.String())
	}
	if len(sender.sent) != 0 {
		t.Errorf("dry-run sent %d batches; want 0", len(sender.sent))
	}
	if !strings.Contains(stdout.String(), "dry-run complete") {
		t.Errorf("missing dry-run line: %s", stdout.String())
	}
}

func TestRunSync_NotPairedExits2(t *testing.T) {
	deps := syncDeps{
		now:             time.Now,
		newOSKeyring:    func() keyring.Store { return keyring.NewMemoryStore() },
		newTallyClient:  func(string) tallyPoster { return &fakeTallyPoster{} },
		newIngestClient: func(_, _, _, _ string) (ingestSender, error) { return &fakeIngestSender{}, nil },
		loadConfig: func(string) (*config.Config, error) {
			return nil, config.ErrNotFound
		},
	}
	var stdout, stderr bytes.Buffer
	code := runSync(&stdout, &stderr, []string{
		"--tally-company", "X",
		"--kind", "purchase",
		"--period", "042026",
	}, deps)
	if code != 2 {
		t.Fatalf("exit=%d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "not paired") {
		t.Errorf("missing helpful 'not paired' message: %s", stderr.String())
	}
}

func TestRunSync_BadFlags(t *testing.T) {
	deps := syncDeps{
		now:             time.Now,
		newOSKeyring:    func() keyring.Store { return keyring.NewMemoryStore() },
		newTallyClient:  func(string) tallyPoster { return &fakeTallyPoster{} },
		newIngestClient: func(_, _, _, _ string) (ingestSender, error) { return &fakeIngestSender{}, nil },
		loadConfig:      func(string) (*config.Config, error) { return &config.Config{Server: "x", ConnectionID: "y"}, nil },
	}
	cases := [][]string{
		{},                                                     // missing required flags
		{"--tally-company", "X"},                               // missing --kind
		{"--tally-company", "X", "--kind", "journal", "--period", "042026"}, // bad kind
		{"--tally-company", "X", "--kind", "purchase"},         // missing window
		{"--tally-company", "X", "--kind", "purchase", "--period", "042026", "--from", "2026-04-01", "--to", "2026-04-30"}, // both
	}
	for i, args := range cases {
		var stdout, stderr bytes.Buffer
		code := runSync(&stdout, &stderr, args, deps)
		if code != 2 {
			t.Errorf("case %d args=%v: exit=%d, want 2 (stderr=%s)", i, args, code, stderr.String())
		}
	}
}
