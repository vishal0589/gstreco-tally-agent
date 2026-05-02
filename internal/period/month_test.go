package period

import (
	"testing"
	"time"
)

func TestParseMonthCode(t *testing.T) {
	cases := []struct {
		in       string
		wantFrom time.Time
		wantTo   time.Time
		wantErr  bool
	}{
		{"042026", time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC), false},
		{"022024", time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC), false},
		{"132026", time.Time{}, time.Time{}, true},
		{"002026", time.Time{}, time.Time{}, true},
		{"04202", time.Time{}, time.Time{}, true},
		{"abcdef", time.Time{}, time.Time{}, true},
		{"041899", time.Time{}, time.Time{}, true},
	}
	for _, tc := range cases {
		from, to, err := ParseMonthCode(tc.in)
		if (err != nil) != tc.wantErr {
			t.Fatalf("ParseMonthCode(%q) err=%v wantErr=%v", tc.in, err, tc.wantErr)
		}
		if tc.wantErr {
			continue
		}
		if !from.Equal(tc.wantFrom) || !to.Equal(tc.wantTo) {
			t.Fatalf("ParseMonthCode(%q) = (%v,%v), want (%v,%v)", tc.in, from, to, tc.wantFrom, tc.wantTo)
		}
	}
}
