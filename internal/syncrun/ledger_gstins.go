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
	// The group tree tells bank, cash and tax ledgers apart from parties by
	// Tally's own grouping, whatever they are named. Without it, names decide.
	if groupBody, gerr := tally.BuildGroupTreeXML(req.TallyCompany); gerr == nil {
		if groupResp, gerr := tallyClient.PostXML(ctx, groupBody); gerr == nil {
			if groups, gerr := tally.ParseGroupTree(groupResp); gerr == nil {
				ledgers.ApplyGroupTree(groups)
			}
		}
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

// hasLiveVoucher reports whether any voucher is not cancelled.
func hasLiveVoucher(vouchers []tally.RawVoucher) bool {
	for _, v := range vouchers {
		if !v.IsCancelled {
			return true
		}
	}
	return false
}

// ledgerGSTINLookup returns the ledger directory the journal and payment
// normalisers use, or nil when no voucher needs it. A failed lookup is
// recorded and the run continues: those vouchers are sent without a GSTIN,
// as before.
func ledgerGSTINLookup(
	ctx context.Context,
	tallyClient TallyPoster,
	req Request,
	vouchers []tally.RawVoucher,
	res *Result,
	emit emitFn,
) tally.LedgerDirectory {
	return lookupLedgerDirectory(ctx, tallyClient, req, needsLedgerGSTINs(vouchers), res, emit,
		"vouchers without a voucher GSTIN are sent without one")
}

// invoiceLedgerLookup returns the ledger directory for an invoice lane. It is
// read for every live voucher, not only those without a voucher GSTIN: each
// leg sent to GST Reco carries its Tally group, which is what the server
// classes legs by (QA v18 R3). A leg without one is logged as "classified by
// name - needs confirmation", and the filing gate waits on those.
func invoiceLedgerLookup(
	ctx context.Context,
	tallyClient TallyPoster,
	req Request,
	vouchers []tally.RawVoucher,
	res *Result,
	emit emitFn,
) tally.LedgerDirectory {
	return lookupLedgerDirectory(ctx, tallyClient, req, hasLiveVoucher(vouchers), res, emit,
		"vouchers without a voucher GSTIN are sent without one, and legs without their Tally group")
}

func lookupLedgerDirectory(
	ctx context.Context,
	tallyClient TallyPoster,
	req Request,
	needed bool,
	res *Result,
	emit emitFn,
	onFailure string,
) tally.LedgerDirectory {
	if !needed {
		return nil
	}
	ledgers, err := ledgerGSTINsFor(ctx, tallyClient, req)
	if err != nil {
		msg := fmt.Sprintf("ledger GSTIN lookup failed; %s: %v", onFailure, err)
		res.ParseWarnings = append(res.ParseWarnings, msg)
		emit("ledger_gstin", msg)
		return nil
	}
	emit("ledger_gstin", fmt.Sprintf("%d ledgers carry a GSTIN", ledgers.Len()))
	return ledgers
}
