package tally

import (
	"testing"
	"time"
)

func TestParseTallyDate(t *testing.T) {
	cases := []struct {
		in   string
		want time.Time
	}{
		{"20260410", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)},
		{"10-Apr-26", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)},
		{"1-Apr-26", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)},
		{"10-Apr-2026", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)},
		{"10/04/26", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)},
		{"10/04/2026", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)},
		{"2026-04-10", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)},
		{"  20260410  ", time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		got, err := ParseTallyDate(tc.in)
		if err != nil {
			t.Errorf("ParseTallyDate(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("ParseTallyDate(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestParseTallyDate_Errors(t *testing.T) {
	for _, in := range []string{"", "   ", "not-a-date", "2026/13/40", "20261340"} {
		if _, err := ParseTallyDate(in); err == nil {
			t.Errorf("ParseTallyDate(%q): want error, got nil", in)
		}
	}
}

func TestFormatTallyDate(t *testing.T) {
	got := FormatTallyDate(time.Date(2026, 4, 10, 12, 30, 0, 0, time.UTC))
	if got != "20260410" {
		t.Errorf("FormatTallyDate = %q, want 20260410", got)
	}
}
