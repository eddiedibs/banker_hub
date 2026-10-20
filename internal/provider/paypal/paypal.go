// Package paypal is a placeholder for the PayPal transaction provider.
package paypal

import (
	"context"

	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
)

// Name is the source identifier for PayPal rows.
const Name = "paypal"

// Provider is not implemented yet.
// TODO: implement with the PayPal Transaction Search API
// (GET /v1/reporting/transactions, OAuth2 client credentials, 31-day windows).
type Provider struct{}

func New() *Provider { return &Provider{} }

func (*Provider) Name() string { return Name }

func (*Provider) Fetch(context.Context, provider.Range) ([]model.Transaction, error) {
	return nil, provider.ErrNotImplemented
}
