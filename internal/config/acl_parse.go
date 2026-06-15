package config

import "regexp"

var icaclsFailedProcessingRE = regexp.MustCompile(`(?i)failed processing\s+([1-9][0-9]*)\s+files?`)

func icaclsReportedFailures(output string) bool {
	return icaclsFailedProcessingRE.MatchString(output)
}
