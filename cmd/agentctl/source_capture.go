package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/vishal0589/gstreco-tally-agent/internal/sourcecapture"
	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
	"time"
)

type sourceMember struct {
	GUID      string `json:"guid"`
	MasterID  string `json:"master_id"`
	AlterID   int64  `json:"alter_id"`
	Date      string `json:"date"`
	Type      string `json:"voucher_type"`
	Number    string `json:"voucher_number"`
	Cancelled bool   `json:"cancelled"`
	Optional  *bool  `json:"optional"`
}
type sourceMembership struct {
	SourceVoucherElementCount int            `json:"source_voucher_element_count"`
	RawSHA256                 string         `json:"raw_response_sha256"`
	ParsedCount               int            `json:"parsed_count"`
	SourcePopulationVerified  bool           `json:"source_population_verified"`
	Warnings                  []string       `json:"warnings"`
	Members                   []sourceMember `json:"members"`
}

// captureAllSource has no server sender. It preserves a full-window voucher
// reply before parsing, without normalizing, filtering or ingesting any row.
func captureAllSource(ctx context.Context, capture *sourcecapture.Capture, company, guid, endpoint string, from, to time.Time) error {
	req, err := tally.BuildSourceCaptureXML(company, from, to)
	if err != nil {
		return err
	}
	raw, err := capture.PostXML(ctx, req)
	if err != nil {
		return err
	}
	parsed, parseErr := tally.ParseDayBookV3(raw)
	report := sourceMembership{RawSHA256: fmt.Sprintf("%x", sha256.Sum256(raw)), ParsedCount: len(parsed.Vouchers), Warnings: append([]string{}, parsed.Warnings...), Members: []sourceMember{}}
	sourceCount, countErr := tally.CountSourceVoucherElements(raw)
	report.SourceVoucherElementCount = sourceCount
	if countErr != nil {
		report.Warnings = append(report.Warnings, countErr.Error())
	}
	if sourceCount != len(parsed.Vouchers) {
		report.Warnings = append(report.Warnings, fmt.Sprintf("source voucher elements %d differ from parsed membership %d", sourceCount, len(parsed.Vouchers)))
	}
	seen := map[string]bool{}
	for _, v := range parsed.Vouchers {
		date := ""
		if !v.Date.IsZero() {
			date = v.Date.Format("2006-01-02")
		}
		report.Members = append(report.Members, sourceMember{v.GUID, v.MasterID, v.AlterID, date, v.VoucherType, v.VoucherNumber, v.IsCancelled, v.IsOptional})
		if v.GUID == "" {
			report.Warnings = append(report.Warnings, "voucher has no GUID")
		} else if seen[v.GUID] {
			report.Warnings = append(report.Warnings, "duplicate voucher GUID: "+v.GUID)
		}
		seen[v.GUID] = true
		if v.Date.IsZero() || v.Date.Before(from) || v.Date.After(to) {
			report.Warnings = append(report.Warnings, "voucher date missing or outside requested window: "+v.GUID)
		}
	}
	if parseErr != nil {
		report.Warnings = append(report.Warnings, parseErr.Error())
	}
	if err = capture.PreserveMembership(report); err != nil {
		return err
	}
	if parseErr != nil {
		return parseErr
	}
	if parsed.Status != 1 {
		return fmt.Errorf("source response did not report successful STATUS 1 (got %d)", parsed.Status)
	}
	if len(report.Warnings) > 0 {
		return fmt.Errorf("source membership has %d warnings; raw evidence retained, population unverified", len(report.Warnings))
	}
	// Recheck company identity after reading. This detects a changed/open copy;
	// it does not certify that Tally's returned voucher population is complete.
	return verifyCaptureIdentity(ctx, capture, company, guid, endpoint)
}
