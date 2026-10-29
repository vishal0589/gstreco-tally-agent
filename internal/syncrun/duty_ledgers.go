package syncrun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// dutyLedgerTTL bounds how stale a company's Duties & Taxes masters may be.
// One sync tick runs every invoice lane of a company well within it, so the
// lanes share one Tally request.
const dutyLedgerTTL = 10 * time.Minute

type dutyLedgerEntry struct {
	at   time.Time
	book *tally.DutyLedgerBook
}

var dutyLedgerCache = struct {
	sync.Mutex
	now     func() time.Time
	entries map[string]dutyLedgerEntry
}{now: time.Now, entries: map[string]dutyLedgerEntry{}}

// resetDutyLedgerCache clears the cache (tests).
func resetDutyLedgerCache() {
	dutyLedgerCache.Lock()
	defer dutyLedgerCache.Unlock()
	dutyLedgerCache.entries = map[string]dutyLedgerEntry{}
}

// dutyLedgersFor returns the company's Duties & Taxes masters, from the cache
// when fresh. The Tally request runs outside the lock. A failed request is
// not cached; an empty export is cached as nil (no evidence).
func dutyLedgersFor(ctx context.Context, tallyClient TallyPoster, req Request) (*tally.DutyLedgerBook, error) {
	key := req.TallyEndpoint + "\x00" + req.TallyCompany
	dutyLedgerCache.Lock()
	if e, ok := dutyLedgerCache.entries[key]; ok && dutyLedgerCache.now().Sub(e.at) < dutyLedgerTTL {
		dutyLedgerCache.Unlock()
		return e.book, nil
	}
	dutyLedgerCache.Unlock()

	body, err := tally.BuildDutyLedgerXML(req.TallyCompany)
	if err != nil {
		return nil, err
	}
	resp, err := tallyClient.PostXML(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("tally fetch: %w", err)
	}
	book, _, err := tally.ParseDutyLedgers(resp)
	if err != nil {
		return nil, err
	}

	dutyLedgerCache.Lock()
	dutyLedgerCache.entries[key] = dutyLedgerEntry{at: dutyLedgerCache.now(), book: book}
	dutyLedgerCache.Unlock()
	return book, nil
}

// dutyLedgerLookup returns the duty-ledger book the invoice normaliser uses,
// fetched once per company before any voucher is normalised. A failed or
// empty fetch is recorded and the run continues with no book: legs named
// after a GST head are then uncertain and every row says the masters were
// missing. The failure is logged and sent to GST Reco's sync health with the
// run's batches (QA v21 G-A).
func dutyLedgerLookup(
	ctx context.Context,
	tallyClient TallyPoster,
	req Request,
	vouchers []tally.RawVoucher,
	res *Result,
	emit emitFn,
) *tally.DutyLedgerBook {
	if !hasLiveVoucher(vouchers) {
		return nil
	}
	book, err := dutyLedgersFor(ctx, tallyClient, req)
	if err != nil {
		dutyLedgersUnavailable(req, vouchers, res, emit,
			fmt.Sprintf("duty ledger masters unavailable; GST taken from the GST class alone and GST-named legs left uncertain: %v", err))
		return nil
	}
	if book == nil {
		dutyLedgersUnavailable(req, vouchers, res, emit,
			"duty ledger masters came back empty; GST taken from the GST class alone and GST-named legs left uncertain")
		return nil
	}
	emit("duty_ledgers", fmt.Sprintf("%d Duties & Taxes ledgers", book.Len()))
	return book
}

// dutyLedgersUnavailable logs the missing masters locally and records them for
// every batch of the run, naming each live voucher: all were normalised
// without the book.
func dutyLedgersUnavailable(req Request, vouchers []tally.RawVoucher, res *Result, emit emitFn, msg string) {
	res.ParseWarnings = append(res.ParseWarnings, msg)
	emit("duty_ledgers", msg)
	res.addSyncWarning(newSyncWarning(tally.SyncWarningDutyLedgerMastersUnavailable,
		req.TallyCompany, msg, syncWarningNow(), liveVoucherGUIDs(vouchers, nil)))
}
