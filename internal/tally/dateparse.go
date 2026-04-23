package tally

import (
	"fmt"
	"strings"
	"time"
)

// tallyDateLayouts are the formats Tally emits across its XML surfaces. The
// canonical day-book format is YYYYMMDD ("20260410") but custom reports and
// user locales produce DD-MMM-YY ("10-Apr-26"), DD/MM/YY ("10/04/26"), and
// occasionally ISO-8601. Order matters: the parser tries each layout in turn
// and returns the first hit.
//
// Background on pain point #5 (Date format quirks): Tally respects the OS
// short-date setting for some reports but not others, so two installs can
// produce different date strings from the same TDL request. This function is
// the one place the agent handles that drift.
var tallyDateLayouts = []string{
	"20060102",    // YYYYMMDD — day-book canonical
	"2-Jan-06",    // D-MMM-YY
	"02-Jan-06",   // DD-MMM-YY
	"2-Jan-2006",  // D-MMM-YYYY
	"02-Jan-2006", // DD-MMM-YYYY
	"2/1/06",      // D/M/YY
	"02/01/06",    // DD/MM/YY
	"2/1/2006",    // D/M/YYYY
	"02/01/2006",  // DD/MM/YYYY
	"2006-01-02",  // ISO-8601
}

// ParseTallyDate converts Tally's wire-format date strings to a time.Time.
// Returns a zero time and an error for empty input or unrecognised formats.
// The returned time has the UTC location; the normaliser attaches the
// company's real timezone before shipping to the server.
func ParseTallyDate(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, fmt.Errorf("empty date")
	}
	for _, layout := range tallyDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognised Tally date format: %q", s)
}

// FormatTallyDate formats a time.Time for Tally's day-book request. Tally
// expects YYYYMMDD in the SVFROMDATE/SVTODATE static variables regardless of
// the user's locale — this is the one format all Tally Prime versions accept
// on the request side.
func FormatTallyDate(t time.Time) string {
	return t.Format("20060102")
}
