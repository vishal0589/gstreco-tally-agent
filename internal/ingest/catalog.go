package ingest

import "context"

// CatalogPath is the server-side endpoint that receives the agent's
// list of (Tally endpoint, company) tuples. Mirrors the route at
// src/app/api/tally/tally-companies/sync/route.ts. Exported so tests
// + A5-2's discover command can assert against the same constant.
const CatalogPath = "/api/tally/tally-companies/sync"

// CatalogItem is one entry the agent reports in the catalog upsert.
// Field tags mirror the server's TallyCompanyItem interface in
// /tally-companies/sync/route.ts (S14). TallyEndpoint is required
// for any A5-2-or-later agent — pre-S14 servers ignore the field
// without erroring, so it's safe to send unconditionally.
type CatalogItem struct {
	TallyCompanyName string  `json:"tally_company_name"`
	TallyCompanyGUID *string `json:"tally_company_guid,omitempty"`
	TallyEndpoint    *string `json:"tally_endpoint,omitempty"`
}

// CatalogRequest is the body shape POSTed to /api/tally/tally-companies/sync.
// Mirrors TallyCompaniesSyncBody server-side. Items can repeat the same
// tally_company_name across different tally_endpoint values — the
// server treats those as distinct mapping rows after S14.
type CatalogRequest struct {
	Items     []CatalogItem `json:"items"`
	RequestID string        `json:"request_id,omitempty"`
	RunID     *string       `json:"run_id,omitempty"`
}

// SendCatalog signs + POSTs a catalog upsert. Same HMAC scheme as
// /ingest. Server upserts on (connection_id, tally_endpoint,
// tally_company_name) — re-running the catalog with identical content
// is idempotent and never overwrites the user's mapping decisions
// (the server omits mapping_status / company_gstin_id from its
// upsert payload by design — see pattern alert #37 in the master
// plan).
//
// Returns SendError on non-2xx, same classification as Send.
// Network errors from a brown-out are retryable; auth/payload
// errors mean the agent should stop pushing this catalog and surface
// the failure for operator attention.
func (c *Client) SendCatalog(ctx context.Context, body CatalogRequest) error {
	return c.PostJSONTo(ctx, CatalogPath, body)
}
