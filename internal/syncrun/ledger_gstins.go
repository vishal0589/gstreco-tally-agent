package syncrun

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// ledgerGSTINTTL bounds how stale a company's ledger GSTINs may be. One sync
// tick runs every lane of a company well within it, so the lanes share one
// Tally request, and a GSTIN added in Tally is picked up by a later tick.
const ledgerGSTINTTL = 10 * time.Minute

type ledgerGSTINEntry struct {
	at      time.Time
	ledgers *tally.LedgerGSTINs
}

var ledgerGSTINCache = struct {
	sync.Mutex
	now     func() time.Time
	entries map[string]ledgerGSTINEntry
}{now: time.Now, entries: map[string]ledgerGSTINEntry{}}

// resetLedgerGSTINCache clears the cache (tests).
func resetLedgerGSTINCache() {
	ledgerGSTINCache.Lock()
	defer ledgerGSTINCache.Unlock()
	ledgerGSTINCache.entries = map[string]ledgerGSTINEntry{}
}

// ledgerGSTINsFor returns the company's ledger GSTINs, from the cache when
// fresh. The Tally request runs outside the lock.
func ledgerGSTINsFor(ctx context.Context, tallyClient TallyPoster, req Request) (*tally.LedgerGSTINs, error) {
	key := req.TallyEndpoint + "\x00" + req.TallyCompany
	ledgerGSTINCache.Lock()
	if e, ok := ledgerGSTINCache.entries[key]; ok && ledgerGSTINCache.now().Sub(e.at) < ledgerGSTINTTL {
		ledgerGSTINCache.Unlock()
		return e.ledgers, nil
	}
	ledgerGSTINCache.Unlock()

	body, err := tally.BuildLedgerGSTINXML(req.TallyCompany)
	if err != nil {
		return nil, err
	}
	resp, err := tallyClient.PostXML(ctx, body)
	if err != nil {
		return nil, fmt.Errorf("tally fetch: %w", err)
	}
	ledgers, _, err := tally.ParseLedgerGSTINs(resp)
	if err != nil {
		return nil, err
	}

	ledgerGSTINCache.Lock()
	ledgerGSTINCache.entries[key] = ledgerGSTINEntry{at: ledgerGSTINCache.now(), ledgers: ledgers}
	ledgerGSTINCache.Unlock()
	return ledgers, nil
}

// needsLedgerGSTINs reports whether any live voucher lacks a voucher-level
// party GSTIN, the only case the ledger lookup can help.
func needsLedgerGSTINs(vouchers []tally.RawVoucher) bool {
	for _, v := range vouchers {
		if !v.IsCancelled && v.PartyGSTIN == "" {
			return true
		}
	}
	return false
}

// ledgerGSTINLookup returns the lookup the normalisers use, or nil when no
// voucher needs it. A failed lookup is recorded and the run continues: those
// vouchers are sent without a GSTIN, as before.
func ledgerGSTINLookup(
	ctx context.Context,
	tallyClient TallyPoster,
	req Request,
	vouchers []tally.RawVoucher,
	res *Result,
	emit emitFn,
) func(string) string {
	if !needsLedgerGSTINs(vouchers) {
		return nil
	}
	ledgers, err := ledgerGSTINsFor(ctx, tallyClient, req)
	if err != nil {
		msg := fmt.Sprintf("ledger GSTIN lookup failed; vouchers without a voucher GSTIN are sent without one: %v", err)
		res.ParseWarnings = append(res.ParseWarnings, msg)
		emit("ledger_gstin", msg)
		return nil
	}
	emit("ledger_gstin", fmt.Sprintf("%d ledgers carry a GSTIN", ledgers.Len()))
	return ledgers.Lookup
}
