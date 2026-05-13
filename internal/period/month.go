package period

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseMonthCode accepts MMYYYY and returns the inclusive first..last
// day of that calendar month in UTC.
func ParseMonthCode(s string) (time.Time, time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) != 6 {
		return time.Time{}, time.Time{}, fmt.Errorf("want MMYYYY (6 digits)")
	}
	mm, err := strconv.Atoi(s[:2])
	if err != nil || mm < 1 || mm > 12 {
		return time.Time{}, time.Time{}, fmt.Errorf("month must be 01..12")
	}
	yyyy, err := strconv.Atoi(s[2:])
	if err != nil || yyyy < 2000 || yyyy > 2100 {
		return time.Time{}, time.Time{}, fmt.Errorf("year out of range")
	}
	from := time.Date(yyyy, time.Month(mm), 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, -1)
	return from, to, nil
}
