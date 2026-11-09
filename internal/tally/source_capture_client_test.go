package tally

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSourceCaptureClientPreservesHTTPFailureAndDetectsCap(t *testing.T) {
	for _, status := range []int{200, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); w.Write([]byte{0xff, 0xfe, 0, 4}) }))
			defer s.Close()
			c := NewSourceCaptureClient(s.URL)
			raw, err := c.PostXML(context.Background(), []byte("read"))
			if !bytes.Equal(raw, []byte{0xff, 0xfe, 0, 4}) {
				t.Fatal("changed bytes")
			}
			if (err != nil) != (status != 200) {
				t.Fatal(err)
			}
			c.limit = 2
			raw, err = c.PostXML(context.Background(), []byte("read"))
			if len(raw) != 3 || err == nil {
				t.Fatal("silent truncation", len(raw), err)
			}
		})
	}
}
func TestSourceCaptureClientDoesNotFollowRedirect(t *testing.T) {
	calls := 0
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte("unexpected")) }))
	defer dest.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}))
	defer s.Close()
	_, err := NewSourceCaptureClient(s.URL).PostXML(context.Background(), []byte("read"))
	if err == nil || calls != 0 {
		t.Fatal("followed redirect", calls, err)
	}
}
