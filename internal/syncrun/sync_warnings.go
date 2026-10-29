package syncrun

import (
	"errors"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// syncWarningNow stamps a sync warning's occurred_at (tests replace it).
var syncWarningNow = time.Now

// newSyncWarning builds a warning for a failure seen at the given time, with
// the contract's caps applied: the message is cut to 500 characters and the
// affected voucher GUIDs are de-duplicated, blanks dropped and cut to 500.
func newSyncWarning(code, company, message string, at time.Time, guids []string) tally.SyncWarning {
	if r := []rune(message); len(r) > tally.MaxSyncWarningMessageRunes {
		message = string(r[:tally.MaxSyncWarningMessageRunes])
	}
	var kept []string
	seen := map[string]bool{}
	for _, g := range guids {
		if g == "" || seen[g] {
			continue
		}
		if len(kept) == tally.MaxSyncWarningVoucherGUIDs {
			break
		}
		seen[g] = true
		kept = append(kept, g)
	}
	return tally.SyncWarning{
		Code:                 code,
		Message:              message,
		TallyCompany:         company,
		OccurredAt:           at.UTC().Format(time.RFC3339),
		AffectedVoucherGUIDs: kept,
	}
}

// addSyncWarning records a warning for every batch of the run, once per
// (code, company, message) as the server de-duplicates, and at most 50.
func (r *Result) addSyncWarning(w tally.SyncWarning) {
	for _, have := range r.SyncWarnings {
		if have.Code == w.Code && have.TallyCompany == w.TallyCompany && have.Message == w.Message {
			return
		}
	}
	if len(r.SyncWarnings) >= tally.MaxSyncWarnings {
		return
	}
	r.SyncWarnings = append(r.SyncWarnings, w)
}

// mastersError is a failed master read with its sync-warning code. Its text
// is the underlying error's, so the local log line is unchanged.
type mastersError struct {
	code string
	err  error
}

func (e *mastersError) Error() string { return e.err.Error() }
func (e *mastersError) Unwrap() error { return e.err }

func mastersFetchFailed(err error) error {
	return &mastersError{code: tally.SyncWarningMastersFetchFailed, err: err}
}

// mastersParseFailed classes an error from parsing a master export. Tally
// refusing the export (company not open) is a failed fetch, not bad XML.
func mastersParseFailed(err error) error {
	if errors.Is(err, tally.ErrExportRefused) {
		return mastersFetchFailed(err)
	}
	return &mastersError{code: tally.SyncWarningMastersParseFailed, err: err}
}

// mastersErrorCode is the sync-warning code for a failed master read.
func mastersErrorCode(err error) string {
	var me *mastersError
	if errors.As(err, &me) {
		return me.code
	}
	return tally.SyncWarningMastersFetchFailed
}

// liveVoucherGUIDs is the GUIDs of the live vouchers keep selects (all live
// vouchers when keep is nil): the ones normalised without a missing master.
func liveVoucherGUIDs(vouchers []tally.RawVoucher, keep func(tally.RawVoucher) bool) []string {
	var guids []string
	for _, v := range vouchers {
		if v.IsCancelled || (keep != nil && !keep(v)) {
			continue
		}
		guids = append(guids, v.GUID)
	}
	return guids
}
