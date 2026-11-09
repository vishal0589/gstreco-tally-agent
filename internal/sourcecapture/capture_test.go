package sourcecapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type fake struct {
	raw   []byte
	err   error
	calls int
}

func (f *fake) PostXML(context.Context, []byte) ([]byte, error) { f.calls++; return f.raw, f.err }
func TestExactBytesAndNoOverwrite(t *testing.T) {
	d := filepath.Join(t.TempDir(), "capture")
	raw := []byte{0xff, 0xfe, 0, 4, 0x80}
	f := &fake{raw: raw}
	c, e := New(d, f, Manifest{Company: "Example Company", Kind: "sales", From: "2026-09-01", To: "2026-09-30"})
	if e != nil {
		t.Fatal(e)
	}
	got, e := c.PostXML(context.Background(), []byte("<read/>"))
	if e != nil || !bytes.Equal(raw, got) {
		t.Fatal(e)
	}
	if e = c.Finish(nil); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "0001-response.bin"))
	if !bytes.Equal(b, raw) {
		t.Fatal("raw bytes changed")
	}
	b, _ = os.ReadFile(filepath.Join(d, "manifest.json"))
	var m Manifest
	if e = json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	if !m.Complete || len(m.Entries) != 1 || m.Entries[0].ResponseSHA256 != hash(raw) {
		t.Fatal(m)
	}
	if _, e = New(d, f, Manifest{}); e == nil {
		t.Fatal("overwrote capture")
	}
	if e = c.Finish(nil); e == nil {
		t.Fatal("overwrote manifest")
	}
}
func TestFailedReadRetainsPartialEvidence(t *testing.T) {
	d := filepath.Join(t.TempDir(), "capture")
	f := &fake{raw: []byte("partial"), err: errors.New("read failed")}
	c, _ := New(d, f, Manifest{})
	_, e := c.PostXML(context.Background(), []byte("request"))
	if e == nil {
		t.Fatal("lost error")
	}
	if e = c.Finish(errors.New("pipeline failed")); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "manifest.json"))
	var m Manifest
	json.Unmarshal(b, &m)
	if m.Complete || m.Error == "" || m.Entries[0].Error == "" {
		t.Fatal(m)
	}
}
func TestInterruptedCaptureStartsIncompleteAndKeepsEntry(t *testing.T) {
	d := filepath.Join(t.TempDir(), "capture")
	c, e := New(d, &fake{raw: []byte("response")}, Manifest{})
	if e != nil {
		t.Fatal(e)
	}
	_, e = c.PostXML(context.Background(), []byte("read"))
	if e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(d, "started.json"))
	if e != nil {
		t.Fatal(e)
	}
	var m Manifest
	if e = json.Unmarshal(b, &m); e != nil {
		t.Fatal(e)
	}
	if m.Complete || m.SourcePopulationVerified {
		t.Fatal("interrupted capture looks complete")
	}
	if _, e = os.Stat(filepath.Join(d, "0001-entry.json")); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(d, "manifest.json")); !os.IsNotExist(e) {
		t.Fatal("unfinalized capture has final manifest")
	}
}
func TestPreservationFailurePoisonsCaptureBeforeMoreReads(t *testing.T) {
	d := filepath.Join(t.TempDir(), "capture")
	f := &fake{raw: []byte("raw")}
	c, e := New(d, f, Manifest{})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(d, "0001-response.bin"), []byte("existing"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = c.PostXML(context.Background(), []byte("read")); e == nil {
		t.Fatal("ignored failed preservation")
	}
	if _, e = c.PostXML(context.Background(), []byte("read again")); e == nil {
		t.Fatal("resumed failed capture")
	}
	if f.calls != 1 {
		t.Fatal("retried upstream after preservation failure")
	}
	if e = c.Finish(nil); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "manifest.json"))
	var m Manifest
	json.Unmarshal(b, &m)
	if m.Complete || m.Error == "" {
		t.Fatal("preservation failure marked successful")
	}
}
func TestEmptyCaptureCannotBeSuccessful(t *testing.T) {
	d := filepath.Join(t.TempDir(), "capture")
	c, e := New(d, &fake{}, Manifest{})
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Finish(nil); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "manifest.json"))
	var m Manifest
	json.Unmarshal(b, &m)
	if m.Complete {
		t.Fatal("empty capture marked successful")
	}
}
