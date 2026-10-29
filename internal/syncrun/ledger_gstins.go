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
	// groupFailure is why the group tree was not applied, kept with the
	// entry so every lane served from the cache still reports it.
	groupFailure *groupTreeFailure
}

// groupTreeFailure is a failed group-tree read and when it happened.
type groupTreeFailure struct {
	err error
	at  time.Time
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
// fresh, and why the group tree is missing from them, if it is. The Tally
// requests run outside the lock. Errors carry their sync-warning code.
func ledgerGSTINsFor(ctx context.Context, tallyClient TallyPoster, req Request) (*tally.LedgerGSTINs, *groupTreeFailure, error) {
	key := req.TallyEndpoint + "\x00" + req.TallyCompany
	ledgerGSTINCache.Lock()
	if e, ok := ledgerGSTINCache.entries[key]; ok && ledgerGSTINCache.now().Sub(e.at) < ledgerGSTINTTL {
		ledgerGSTINCache.Unlock()
		return e.ledgers, e.groupFailure, nil
	}
	ledgerGSTINCache.Unlock()

	body, err := tally.BuildLedgerGSTINXML(req.TallyCompany)
	if err != nil {
		return nil, nil, mastersFetchFailed(err)
	}
	resp, err := tallyClient.PostXML(ctx, body)
	if err != nil {
		return nil, nil, mastersFetchFailed(fmt.Errorf("tally fetch: %w", err))
	}
	ledgers, _, err := tally.ParseLedgerGSTINs(resp)
	if err != nil {
		return nil, nil, mastersParseFailed(err)
	}
	// The group tree tells bank, cash and tax ledgers apart from parties by
	// Tally's own grouping, whatever they are named. Without it, names decide,
	// and the run says so.
	var groupFailure *groupTreeFailure
	if groups, gerr := groupTreeFor(ctx, tallyClient, req); gerr != nil {
		groupFailure = &groupTreeFailure{err: gerr, at: syncWarningNow()}
	} else {
		ledgers.ApplyGroupTree(groups)
	}

	ledgerGSTINCache.Lock()
	ledgerGSTINCache.entries[key] = ledgerGSTINEntry{at: ledgerGSTINCache.now(), ledgers: ledgers, groupFailure: groupFailure}
	ledgerGSTINCache.Unlock()
	return ledgers, groupFailure, nil
}

// groupTreeFor reads the company's group tree. Errors carry their
// sync-warning code.
func groupTreeFor(ctx context.Context, tallyClient TallyPoster, req Request) (map[string]string, error) {
	body, err := tally.BuildGroupTreeXML(req.TallyCompany)
	if err != nil {
		return nil, mastersFetchFailed(err)
	}
	resp, err := tallyClient.PostXML(ctx, body)
	if err != nil {
		return nil, mastersFetchFailed(fmt.Errorf("tally fetch: %w", err))
	}
	groups, err := tally.ParseGroupTree(resp)
	if err != nil {
		return nil, mastersParseFailed(err)
	}
	return groups, nil
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
// as before, and the batches name them.
func ledgerGSTINLookup(
	ctx context.Context,
	tallyClient TallyPoster,
	req Request,
	vouchers []tally.RawVoucher,
	res *Result,
	emit emitFn,
) tally.LedgerDirectory {
	affected := liveVoucherGUIDs(vouchers, func(v tally.RawVoucher) bool { return v.PartyGSTIN == "" })
	return lookupLedgerDirectory(ctx, tallyClient, req, needsLedgerGSTINs(vouchers), affected, res, emit,
		"vouchers without a voucher GSTIN are sent without one",
		"bank, cash and tax ledgers are told from parties by name")
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
	return lookupLedgerDirectory(ctx, tallyClient, req, hasLiveVoucher(vouchers), liveVoucherGUIDs(vouchers, nil), res, emit,
		"vouchers without a voucher GSTIN are sent without one, and legs without their Tally group",
		"legs are sent without their Tally group")
}

// lookupLedgerDirectory reads the ledger directory when needed. A failed
// ledger or group-tree read is logged and recorded for every batch of the
// run (QA v21 G-A), naming the affected vouchers.
func lookupLedgerDirectory(
	ctx context.Context,
	tallyClient TallyPoster,
	req Request,
	needed bool,
	affected []string,
	res *Result,
	emit emitFn,
	onFailure, onGroupFailure string,
) tally.LedgerDirectory {
	if !needed {
		return nil
	}
	ledgers, groupFailure, err := ledgerGSTINsFor(ctx, tallyClient, req)
	if err != nil {
		msg := fmt.Sprintf("ledger GSTIN lookup failed; %s: %v", onFailure, err)
		res.ParseWarnings = append(res.ParseWarnings, msg)
		emit("ledger_gstin", msg)
		res.addSyncWarning(newSyncWarning(mastersErrorCode(err), req.TallyCompany, msg, syncWarningNow(), affected))
		return nil
	}
	if groupFailure != nil {
		msg := fmt.Sprintf("ledger group tree unavailable; %s: %v", onGroupFailure, groupFailure.err)
		res.ParseWarnings = append(res.ParseWarnings, msg)
		emit("ledger_gstin", msg)
		res.addSyncWarning(newSyncWarning(mastersErrorCode(groupFailure.err), req.TallyCompany, msg, groupFailure.at, affected))
	}
	emit("ledger_gstin", fmt.Sprintf("%d ledgers carry a GSTIN", ledgers.Len()))
	return ledgers
}
