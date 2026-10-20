// Package provider defines the contract every transaction source implements.
package provider

import (
	"context"
	"errors"
	"time"

	"github.com/eddiedibs/banker_hub/internal/model"
)

// ErrNotImplemented is returned by providers that are placeholders.
var ErrNotImplemented = errors.New("provider not implemented")

// Provider fetches transactions from one external source.
// Implementations return data only and never touch storage.
type Provider interface {
	// Name is the stable source identifier stored in transaction_history.source.
	Name() string
	// Fetch returns transactions that occurred inside r.
	// Returning rows outside r is allowed; storage deduplicates.
	Fetch(ctx context.Context, r Range) ([]model.Transaction, error)
}

// Range is a half-open time window [From, To). A zero To means "now".
type Range struct{ From, To time.Time }
