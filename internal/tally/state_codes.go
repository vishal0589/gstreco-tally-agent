package tally

import "strings"

// stateNameToCode maps Tally's STATENAME export values to India's 2-digit
// GST state codes. Exported names are human-written in Tally so the map
// keys are uppercased and common misspellings (ORISSA → Odisha, PONDICHERRY
// → Puducherry) map to the official code.
//
// Reference: Central Board of Indirect Taxes and Customs, "List of State
// Codes for GSTIN" (https://www.cbic.gov.in). Kept in sync with the server's
// authoritative list at src/lib/parsers/normalize.ts when both have the same
// state. Agent ships with the complete set since Tally users can be in any
// of India's 36 states/UTs whereas the server sees only onboarded users.
var stateNameToCode = map[string]string{
	"JAMMU AND KASHMIR":                          "01",
	"JAMMU & KASHMIR":                            "01",
	"HIMACHAL PRADESH":                           "02",
	"PUNJAB":                                     "03",
	"CHANDIGARH":                                 "04",
	"UTTARAKHAND":                                "05",
	"UTTARANCHAL":                                "05", // legacy name
	"HARYANA":                                    "06",
	"DELHI":                                      "07",
	"RAJASTHAN":                                  "08",
	"UTTAR PRADESH":                              "09",
	"BIHAR":                                      "10",
	"SIKKIM":                                     "11",
	"ARUNACHAL PRADESH":                          "12",
	"NAGALAND":                                   "13",
	"MANIPUR":                                    "14",
	"MIZORAM":                                    "15",
	"TRIPURA":                                    "16",
	"MEGHALAYA":                                  "17",
	"ASSAM":                                      "18",
	"WEST BENGAL":                                "19",
	"JHARKHAND":                                  "20",
	"ODISHA":                                     "21",
	"ORISSA":                                     "21", // legacy name
	"CHHATTISGARH":                               "22",
	"CHATTISGARH":                                "22", // common misspelling
	"MADHYA PRADESH":                             "23",
	"GUJARAT":                                    "24",
	"DADRA AND NAGAR HAVELI AND DAMAN AND DIU":   "26",
	"DADRA & NAGAR HAVELI AND DAMAN & DIU":       "26",
	"DAMAN AND DIU":                              "26", // pre-2020 separate code 25
	"DADRA AND NAGAR HAVELI":                     "26", // merged 2020
	"MAHARASHTRA":                                "27",
	"KARNATAKA":                                  "29",
	"GOA":                                        "30",
	"LAKSHADWEEP":                                "31",
	"KERALA":                                     "32",
	"TAMIL NADU":                                 "33",
	"TAMILNADU":                                  "33", // common no-space form
	"PUDUCHERRY":                                 "34",
	"PONDICHERRY":                                "34", // legacy name
	"ANDAMAN AND NICOBAR ISLANDS":                "35",
	"ANDAMAN & NICOBAR ISLANDS":                  "35",
	"TELANGANA":                                  "36",
	"ANDHRA PRADESH":                             "37",
	"LADAKH":                                     "38",
	"OTHER TERRITORY":                            "97",
	"OTHER COUNTRY":                              "99",
	"FOREIGN COUNTRY":                            "99",
}

// StateCode returns the 2-digit GST state code for a Tally state name, or
// an empty string when the name is unrecognised. Matching is case- and
// whitespace-insensitive so "  karnataka " and "KARNATAKA" both resolve.
// Returns "" rather than an error — the server tolerates missing state
// codes and the agent shouldn't fail a whole master-sync batch because one
// user-typed state name is novel.
func StateCode(name string) string {
	key := strings.ToUpper(strings.TrimSpace(name))
	if key == "" {
		return ""
	}
	if code, ok := stateNameToCode[key]; ok {
		return code
	}
	// Collapse internal whitespace (TamilNadu vs Tamil Nadu, Jammu  Kashmir
	// with double space). Preserves the single-space forms that made it into
	// the map canonically.
	collapsed := strings.Join(strings.Fields(key), " ")
	return stateNameToCode[collapsed]
}
