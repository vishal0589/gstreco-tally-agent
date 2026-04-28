package ingest

import (
	"context"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

// MastersPath is the server-side endpoint that receives party-master
// rows (vendors / customers). Mirrors src/app/api/tally/masters/route.ts.
const MastersPath = "/api/tally/masters"

// SendMasters signs + POSTs a party-master batch. Same HMAC scheme
// as Send and SendCatalog. Server upserts into vendor_master /
// customer_master keyed by (company_id, gstin) — re-running with the
// same items is idempotent. Calling this BEFORE the voucher ingest
// for a tick lets the server back-fill vendor_id on each invoice
// row at upsert time (the trigger already keys on
// vendor_master.gstin).
//
// SendError on non-2xx, same retryable/give-up classification as
// Send. The masters endpoint is tolerant of bad GSTINs (returns
// `findings` with per-item errors but still 200) so a network
// success means "all valid items absorbed".
func (c *Client) SendMasters(ctx context.Context, body tally.IngestMastersRequest) error {
	return c.PostJSONTo(ctx, MastersPath, body)
}
