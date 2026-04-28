package tally

import "testing"

func TestStateCode(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Karnataka", "29"},
		{"KARNATAKA", "29"},
		{"  karnataka  ", "29"},
		{"Tamil Nadu", "33"},
		{"TamilNadu", "33"}, // no-space form
		{"Orissa", "21"},    // legacy → Odisha
		{"Odisha", "21"},
		{"Pondicherry", "34"}, // legacy → Puducherry
		{"Delhi", "07"},
		{"Foreign Country", "99"},
		{"", ""},
		{"Atlantis", ""}, // unknown → empty, not an error
		{"Jammu  &  Kashmir", "01"}, // collapses internal double-space
	}
	for _, tc := range cases {
		if got := StateCode(tc.in); got != tc.want {
			t.Errorf("StateCode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStateCode_CompleteForMajorStates(t *testing.T) {
	// Spot-check that the map covers every major state the agent is likely
	// to encounter. Missing any of these would break a whole user's master
	// sync; unknown states are OK (server tolerates "").
	required := []string{
		"Andhra Pradesh", "Assam", "Bihar", "Chhattisgarh", "Delhi",
		"Gujarat", "Haryana", "Karnataka", "Kerala", "Madhya Pradesh",
		"Maharashtra", "Odisha", "Punjab", "Rajasthan", "Tamil Nadu",
		"Telangana", "Uttar Pradesh", "West Bengal",
	}
	for _, s := range required {
		if StateCode(s) == "" {
			t.Errorf("major state %q has no code", s)
		}
	}
}
