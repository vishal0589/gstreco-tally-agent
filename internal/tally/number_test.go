package tally

import "testing"

func TestParseTallyAmount(t *testing.T) {
	cases := []struct {
		in      string
		want    float64
		wantErr bool
	}{
		{"", 0, false},
		{"  ", 0, false},
		{"100", 100, false},
		{"100.25", 100.25, false},
		{"-118000.00", -118000, false},
		{"1,18,000.00", 118000, false},  // Indian grouping
		{"118,000.00", 118000, false},   // Western grouping
		{"-1,18,000.00", -118000, false},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, err := parseTallyAmount(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseTallyAmount(%q): want error, got nil", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTallyAmount(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("parseTallyAmount(%q) = %f, want %f", tc.in, got, tc.want)
		}
	}
}

func TestParseTallyQuantity(t *testing.T) {
	cases := []struct {
		in       string
		wantQty  float64
		wantUnit string
	}{
		{"", 0, ""},
		{"10 NOS", 10, "NOS"},
		{"10.000 NOS", 10, "NOS"},
		{"1,000 KG", 1000, "KG"},
		{"-5 NOS", -5, "NOS"},  // returns
		{"100", 100, ""},        // unit omitted
		{"100 SQ FT", 100, "SQ FT"},
	}
	for _, tc := range cases {
		q, u := parseTallyQuantity(tc.in)
		if q != tc.wantQty || u != tc.wantUnit {
			t.Errorf("parseTallyQuantity(%q) = (%f, %q), want (%f, %q)", tc.in, q, u, tc.wantQty, tc.wantUnit)
		}
	}
}

func TestParseTallyRate(t *testing.T) {
	cases := []struct {
		in       string
		wantRate float64
		wantUnit string
	}{
		{"", 0, ""},
		{"500.00/NOS", 500, "NOS"},
		{"1,000.00/NOS", 1000, "NOS"},
		{"500", 500, ""},
		{"500.00 / NOS", 500, "NOS"},
	}
	for _, tc := range cases {
		r, u := parseTallyRate(tc.in)
		if r != tc.wantRate || u != tc.wantUnit {
			t.Errorf("parseTallyRate(%q) = (%f, %q), want (%f, %q)", tc.in, r, u, tc.wantRate, tc.wantUnit)
		}
	}
}

func TestParseTallyBool(t *testing.T) {
	for _, in := range []string{"Yes", "yes", "YES", " Yes ", "true", "1"} {
		if !parseTallyBool(in) {
			t.Errorf("parseTallyBool(%q) = false, want true", in)
		}
	}
	for _, in := range []string{"", "No", "no", "false", "0", "maybe"} {
		if parseTallyBool(in) {
			t.Errorf("parseTallyBool(%q) = true, want false", in)
		}
	}
}
