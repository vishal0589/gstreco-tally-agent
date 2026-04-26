package ingest

import (
	"context"
	"fmt"
)

// ActiveMappingsPathFor returns the GET URL for the active-mappings
// endpoint. Format mirrors the server route at
// src/app/api/tally/connections/[id]/mappings/active/route.ts.
func ActiveMappingsPathFor(connectionID string) string {
	return fmt.Sprintf("/api/tally/connections/%s/mappings/active", connectionID)
}

// ActiveMapping is one row the agent's daemon walks per scheduled
// sync. Mirrors the server's TallyActiveMappingItem (S15). Optional
// GUID is a string here (not *string) because Go's JSON omitempty
// works correctly on the empty-string sentinel for this field — the
// server sends "" rather than null in the legacy case.
type ActiveMapping struct {
	MappingID        string `json:"mapping_id"`
	TallyEndpoint    string `json:"tally_endpoint"`
	TallyCompanyName string `json:"tally_company_name"`
	TallyCompanyGUID string `json:"tally_company_guid,omitempty"`
	CompanyGSTINID   string `json:"company_gstin_id"`
}

// ActiveMappingsResponse is the body shape of GET
// /api/tally/connections/[id]/mappings/active. Mirrors
// TallyActiveMappingsResponse on the server side (S15).
type ActiveMappingsResponse struct {
	ConnectionID string          `json:"connection_id"`
	Mappings     []ActiveMapping `json:"mappings"`
	FetchedAt    string          `json:"fetched_at"`
}

// FetchActiveMappings GETs the agent-facing mapping list and decodes
// the response. Returns SendError (with Kind=Auth) if the URL
// connection_id and the HMAC connection_id don't match — that's the
// server's cross-tenant guard kicking in, and almost always indicates
// a programming bug rather than a transient failure.
func (c *Client) FetchActiveMappings(ctx context.Context) (*ActiveMappingsResponse, error) {
	var out ActiveMappingsResponse
	if err := c.GetJSONFrom(ctx, ActiveMappingsPathFor(c.connectionID), &out); err != nil {
		return nil, err
	}
	return &out, nil
}
