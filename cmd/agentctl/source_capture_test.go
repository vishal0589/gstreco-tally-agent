package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/vishal0589/gstreco-tally-agent/internal/config"
	"github.com/vishal0589/gstreco-tally-agent/internal/keyring"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type allSourceFake struct {
	raw      []byte
	requests [][]byte
}

func (f *allSourceFake) PostXML(ctx context.Context, req []byte) ([]byte, error) {
	f.requests = append(f.requests, append([]byte{}, req...))
	if bytes.Contains(req, []byte("GstrecoSourceCapture")) {
		return f.raw, nil
	}
	return (captureTallyFake{}).PostXML(ctx, req)
}
func captureTestRun(t *testing.T, raw []byte) (int, string, string, *allSourceFake) {
	t.Helper()
	d := filepath.Join(t.TempDir(), "capture")
	f := &allSourceFake{raw: raw}
	deps := syncDeps{now: time.Now, newTallyClient: func(string) tallyPoster { return f }, loadConfig: func(string) (*config.Config, error) {
		return &config.Config{Server: "https://must-not-contact.example", ConnectionID: "conn"}, nil
	}, newOSKeyring: func() keyring.Store { t.Fatal("accessed keyring"); return nil }, newIngestClient: func(_, _, _, _ string) (ingestSender, error) { t.Fatal("created ingest sender"); return nil, nil }}
	var out, err bytes.Buffer
	code := runSync(&out, &err, []string{"--tally-company", "Example Company", "--expected-company-guid", "expected-guid", "--kind", "source_all", "--period", "092026", "--dry-run", "--capture-dir", d}, deps)
	return code, d, err.String(), f
}
func TestCaptureAllSourcePreservesZeroTaxCancelledOptionalUnknownAndMembership(t *testing.T) {
	raw := []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><BODY><DATA><COLLECTION><VOUCHER><GUID TYPE="String">g-zero</GUID><MASTERID TYPE="Number">1</MASTERID><ALTERID TYPE="Number">9</ALTERID><DATE TYPE="Date">20260905</DATE><VOUCHERTYPENAME>Custom zero-tax sales</VOUCHERTYPENAME><VOUCHERNUMBER>66</VOUCHERNUMBER><ISCANCELLED TYPE="Logical">Yes</ISCANCELLED><ISOPTIONAL TYPE="Logical">Yes</ISOPTIONAL></VOUCHER></COLLECTION></DATA></BODY></ENVELOPE>`)
	code, d, e, f := captureTestRun(t, raw)
	if code != 0 {
		t.Fatalf("%d: %s", code, e)
	}
	got, err := os.ReadFile(filepath.Join(d, "0003-response.bin"))
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("raw changed", err)
	}
	var report sourceMembership
	b, _ := os.ReadFile(filepath.Join(d, "membership.json"))
	if err = json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if report.ParsedCount != 1 || report.Members[0].GUID != "g-zero" || report.Members[0].AlterID != 9 || !report.Members[0].Cancelled || report.Members[0].Optional == nil || !*report.Members[0].Optional {
		t.Fatalf("membership=%+v", report)
	}
	if report.SourcePopulationVerified {
		t.Fatal("inferred full source completeness")
	}
	if len(f.requests) != 5 {
		t.Fatalf("got %d reads, wanted preflight x2, vouchers, postflight x2", len(f.requests))
	}
	for _, req := range f.requests {
		if !bytes.Contains(req, []byte("<TALLYREQUEST>Export</TALLYREQUEST>")) {
			t.Fatal("not an export read")
		}
	}
}
func TestCaptureAllSourceWarningsAndRefusalNeverLookSuccessful(t *testing.T) {
	for name, body := range map[string]string{
		"no-status":    `<ENVELOPE><VOUCHER><GUID>x</GUID><DATE>20260901</DATE></VOUCHER></ENVELOPE>`,
		"duplicate":    `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><VOUCHER><GUID>x</GUID><DATE>20260901</DATE></VOUCHER><VOUCHER><GUID>x</GUID><DATE>20260901</DATE></VOUCHER></ENVELOPE>`,
		"outside":      `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><VOUCHER><GUID>x</GUID><DATE>20260801</DATE></VOUCHER></ENVELOPE>`,
		"missing-guid": `<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><VOUCHER><VOUCHERNUMBER>66</VOUCHERNUMBER><DATE>20260901</DATE></VOUCHER></ENVELOPE>`,
		"refusal":      `<ENVELOPE><HEADER><STATUS>0</STATUS></HEADER><LINEERROR>Unavailable</LINEERROR></ENVELOPE>`,
	} {
		t.Run(name, func(t *testing.T) {
			code, d, e, _ := captureTestRun(t, []byte(body))
			if code == 0 {
				t.Fatal("accepted uncertain source", e)
			}
			var m map[string]any
			b, err := os.ReadFile(filepath.Join(d, "manifest.json"))
			if err != nil {
				t.Fatal(err)
			}
			json.Unmarshal(b, &m)
			if m["pipeline_succeeded"] != false || m["source_population_verified"] != false {
				t.Fatal(m)
			}
			if _, err = os.Stat(filepath.Join(d, "0003-response.bin")); err != nil {
				t.Fatal("lost refusal/partial raw", err)
			}
		})
	}
}
func TestCaptureAllSourceMissingParsedMemberFailsClosed(t *testing.T) {
	raw := []byte(`<ENVELOPE><HEADER><STATUS>1</STATUS></HEADER><VOUCHER></VOUCHER><VOUCHER><GUID>x</GUID><DATE>20260901</DATE></VOUCHER></ENVELOPE>`)
	code, d, _, _ := captureTestRun(t, raw)
	if code == 0 {
		t.Fatal("lost voucher membership without failure")
	}
	var report sourceMembership
	b, err := os.ReadFile(filepath.Join(d, "membership.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &report); err != nil {
		t.Fatal(err)
	}
	if report.SourceVoucherElementCount != 2 || report.ParsedCount != 1 || len(report.Warnings) == 0 {
		t.Fatalf("%+v", report)
	}
}

func TestSourceAllRequiresCaptureAndExpectedGUIDBeforeDependencies(t *testing.T) {
	for _, args := range [][]string{
		{"--kind", "source_all", "--tally-company", "Example Company", "--period", "092026"},
		{"--kind", "source_all", "--tally-company", "Example Company", "--period", "092026", "--dry-run"},
		{"--kind", "source_all", "--tally-company", "Example Company", "--period", "092026", "--dry-run", "--capture-dir", "never-used"},
	} {
		var out, err bytes.Buffer
		if code := runSync(&out, &err, args, syncDeps{}); code != 2 {
			t.Fatalf("flags %v exit %d", args, code)
		}
	}
}
