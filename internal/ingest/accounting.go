package ingest

import (
	"context"

	"github.com/vishal0589/gstreco-tally-agent/internal/tally"
)

const (
	JournalPath   = "/api/tally/journal"
	PaymentPath   = "/api/tally/payment"
	TaxLedgerPath = "/api/tally/tax-ledgers"
)

func (c *Client) SendJournal(ctx context.Context, body tally.JournalIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	var out tally.AccountingAcceptedResponse
	err := c.PostJSONExpect(ctx, JournalPath, body, &out)
	return out, err
}

func (c *Client) SendPayment(ctx context.Context, body tally.PaymentIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	var out tally.AccountingAcceptedResponse
	err := c.PostJSONExpect(ctx, PaymentPath, body, &out)
	return out, err
}

func (c *Client) SendTaxLedgers(ctx context.Context, body tally.TaxLedgerIngestRequestBody) (tally.AccountingAcceptedResponse, error) {
	var out tally.AccountingAcceptedResponse
	err := c.PostJSONExpect(ctx, TaxLedgerPath, body, &out)
	return out, err
}
