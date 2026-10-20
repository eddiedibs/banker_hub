package store

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eddiedibs/banker_hub/internal/model"
)

const timeLayout = time.RFC3339

// UpsertTransactions inserts txs in one database transaction. Existing rows
// (same source and external_id) only get their status refreshed and
// their detail filled in; an empty detail never erases a stored one.
// It returns the number of rows inserted or updated.
func (s *Store) UpsertTransactions(ctx context.Context, txs []model.Transaction) (int64, error) {
	if len(txs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO transaction_history
			(id, source, external_id, account, kind, direction, amount, asset,
			 status, reference, description, detail, occurred_at, raw)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (source, external_id) DO UPDATE SET
			status = excluded.status,
			detail = COALESCE(excluded.detail, detail)
		WHERE status IS NOT excluded.status
			OR (excluded.detail IS NOT NULL AND detail IS NOT excluded.detail)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	var n int64
	for _, t := range txs {
		if !t.Direction.Valid() {
			return 0, fmt.Errorf("transaction %s: invalid direction %q", t.ExternalID, t.Direction)
		}
		res, err := stmt.ExecContext(ctx,
			t.ID, t.Source, t.ExternalID, t.Account, t.Kind, string(t.Direction),
			t.Amount.Abs().String(), t.Asset, nullString(t.Status), nullString(t.Reference),
			nullString(t.Description), nullString(t.Detail), t.OccurredAt.UTC().Format(timeLayout), nullString(t.Raw))
		if err != nil {
			return 0, fmt.Errorf("transaction %s: %w", t.ExternalID, err)
		}
		affected, _ := res.RowsAffected()
		n += affected
	}
	return n, tx.Commit()
}

// Filter narrows List and Summary results. Zero values mean "no filter".
type Filter struct {
	Source    string
	Asset     string
	Direction model.Direction
	From      time.Time
	To        time.Time
	Limit     int
	Cursor    string
}

// Page is one page of List results.
type Page struct {
	Items      []model.Transaction `json:"items"`
	NextCursor string              `json:"next_cursor,omitempty"`
}

// ErrBadCursor is returned when a pagination cursor cannot be decoded.
var ErrBadCursor = errors.New("invalid cursor")

const selectColumns = `id, source, external_id, account, kind, direction, amount, asset,
	status, reference, description, detail, occurred_at, created_at`

// ListTransactions returns transactions newest first using keyset pagination.
func (s *Store) ListTransactions(ctx context.Context, f Filter) (Page, error) {
	where, args := f.where()
	if f.Cursor != "" {
		at, id, err := decodeCursor(f.Cursor)
		if err != nil {
			return Page{}, err
		}
		where = append(where, "(occurred_at, id) < (?, ?)")
		args = append(args, at, id)
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}

	q := "SELECT " + selectColumns + " FROM transaction_history"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY occurred_at DESC, id DESC LIMIT ?"
	// Fetch one extra row to know whether another page exists.
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return Page{}, err
	}
	defer rows.Close()

	page := Page{Items: []model.Transaction{}}
	for rows.Next() {
		t, err := scanTransaction(rows)
		if err != nil {
			return Page{}, err
		}
		page.Items = append(page.Items, t)
	}
	if err := rows.Err(); err != nil {
		return Page{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextCursor = encodeCursor(last.OccurredAt.UTC().Format(timeLayout), last.ID)
	}
	return page, nil
}

// GetTransaction returns one transaction by primary key.
func (s *Store) GetTransaction(ctx context.Context, id string) (model.Transaction, error) {
	row := s.db.QueryRowContext(ctx, "SELECT "+selectColumns+" FROM transaction_history WHERE id = ?", id)
	t, err := scanTransaction(row)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// Total is the sum of amounts for one asset and direction.
type Total struct {
	Asset         string          `json:"asset"`
	CurrencyLabel string          `json:"currency_label"`
	Credit        decimal.Decimal `json:"credit"`
	Debit         decimal.Decimal `json:"debit"`
	Net           decimal.Decimal `json:"net"`
	Count         int             `json:"count"`
}

// Summary returns per-asset totals. Sums use exact decimal math in Go
// because SQLite would convert the TEXT amounts to floating point.
func (s *Store) Summary(ctx context.Context, f Filter) ([]Total, error) {
	where, args := f.where()
	q := "SELECT asset, direction, amount FROM transaction_history"
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY asset"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	totals := []Total{}
	index := map[string]int{}
	for rows.Next() {
		var asset, direction, amount string
		if err := rows.Scan(&asset, &direction, &amount); err != nil {
			return nil, err
		}
		d, err := decimal.NewFromString(amount)
		if err != nil {
			return nil, fmt.Errorf("bad amount %q: %w", amount, err)
		}
		i, ok := index[asset]
		if !ok {
			i = len(totals)
			index[asset] = i
			totals = append(totals, Total{Asset: asset, CurrencyLabel: model.CurrencyLabel(asset)})
		}
		t := &totals[i]
		t.Count++
		if model.Direction(direction) == model.Debit {
			t.Debit = t.Debit.Add(d)
		} else {
			t.Credit = t.Credit.Add(d)
		}
		t.Net = t.Credit.Sub(t.Debit)
	}
	return totals, rows.Err()
}

func (f Filter) where() ([]string, []any) {
	var where []string
	var args []any
	if f.Source != "" {
		where = append(where, "source = ?")
		args = append(args, f.Source)
	}
	if f.Asset != "" {
		where = append(where, "asset = ?")
		args = append(args, strings.ToUpper(f.Asset))
	}
	if f.Direction != "" {
		where = append(where, "direction = ?")
		args = append(args, string(f.Direction))
	}
	if !f.From.IsZero() {
		where = append(where, "occurred_at >= ?")
		args = append(args, f.From.UTC().Format(timeLayout))
	}
	if !f.To.IsZero() {
		where = append(where, "occurred_at < ?")
		args = append(args, f.To.UTC().Format(timeLayout))
	}
	return where, args
}

type scanner interface{ Scan(dest ...any) error }

func scanTransaction(r scanner) (model.Transaction, error) {
	var (
		t                                      model.Transaction
		direction, amount, occurred            string
		created                                string
		status, reference, description, detail sql.NullString
	)
	err := r.Scan(&t.ID, &t.Source, &t.ExternalID, &t.Account, &t.Kind, &direction, &amount,
		&t.Asset, &status, &reference, &description, &detail, &occurred, &created)
	if err != nil {
		return t, err
	}
	t.Direction = model.Direction(direction)
	t.Status, t.Reference, t.Description = status.String, reference.String, description.String
	t.Detail = detail.String
	if t.Amount, err = decimal.NewFromString(amount); err != nil {
		return t, fmt.Errorf("bad amount %q: %w", amount, err)
	}
	if t.OccurredAt, err = time.Parse(timeLayout, occurred); err != nil {
		return t, err
	}
	if t.CreatedAt, err = time.Parse(timeLayout, created); err != nil {
		return t, err
	}
	return t, nil
}

func encodeCursor(occurredAt, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(occurredAt + "|" + id))
}

func decodeCursor(c string) (string, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return "", "", ErrBadCursor
	}
	at, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return "", "", ErrBadCursor
	}
	return at, id, nil
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
