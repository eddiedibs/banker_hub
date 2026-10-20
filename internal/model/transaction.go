// Package model holds the bank-agnostic domain types shared by providers,
// storage and the HTTP layer.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Direction is the money flow relative to the account owner.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// Valid reports whether d is a known direction.
func (d Direction) Valid() bool { return d == Debit || d == Credit }

// Transaction is the normalized row stored in transaction_history.
// Amount is always positive; Direction carries the sign.
type Transaction struct {
	ID          string          `json:"id"`
	Source      string          `json:"source"`
	ExternalID  string          `json:"external_id"`
	Account     string          `json:"account"`
	Kind        string          `json:"kind"`
	Direction   Direction       `json:"direction"`
	Amount      decimal.Decimal `json:"amount"`
	Asset       string          `json:"asset"`
	Status      string          `json:"status,omitempty"`
	Reference   string          `json:"reference,omitempty"`
	Description string          `json:"description,omitempty"`
	OccurredAt  time.Time       `json:"occurred_at"`
	Raw         string          `json:"-"`
	CreatedAt   time.Time       `json:"created_at,omitzero"`
}

// MakeID builds the deterministic primary key "<source>-<sha256[:16]>"
// from the source and its external id.
func MakeID(source, externalID string) string {
	return source + "-" + HashKey(source, externalID)
}

// HashKey returns the first 16 hex chars of sha256 over parts joined by "|".
func HashKey(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:])[:16]
}

// CurrencyLabel maps an asset code to the display label used by the
// original spec: "Bs" for bolívares, "$" for USD and USD stablecoins,
// otherwise the asset code itself. Keep in sync with the
// transaction_history_display view.
func CurrencyLabel(asset string) string {
	switch strings.ToUpper(asset) {
	case "VES":
		return "Bs"
	case "USD", "USDT", "USDC", "FDUSD", "BUSD", "DAI":
		return "$"
	default:
		return asset
	}
}
