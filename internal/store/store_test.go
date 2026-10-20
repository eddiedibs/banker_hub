package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eddiedibs/banker_hub/internal/model"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func tx(ext string, at time.Time, dir model.Direction, amount, asset string) model.Transaction {
	return model.Transaction{
		ID:         model.MakeID("test", ext),
		Source:     "test",
		ExternalID: ext,
		Account:    "main",
		Kind:       "transfer",
		Direction:  dir,
		Amount:     decimal.RequireFromString(amount),
		Asset:      asset,
		Status:     "completed",
		OccurredAt: at,
	}
}

func TestUpsertIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	batch := []model.Transaction{
		tx("a", base, model.Credit, "100.50", "VES"),
		tx("b", base.Add(time.Hour), model.Debit, "0.00012345", "BTC"),
	}

	n, err := s.UpsertTransactions(ctx, batch)
	if err != nil || n != 2 {
		t.Fatalf("first upsert: n=%d err=%v", n, err)
	}
	n, err = s.UpsertTransactions(ctx, batch)
	if err != nil || n != 0 {
		t.Fatalf("second upsert: n=%d err=%v, want 0 changed rows", n, err)
	}

	page, err := s.ListTransactions(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 {
		t.Fatalf("got %d rows, want 2", len(page.Items))
	}
	got, err := s.GetTransaction(ctx, batch[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Amount.Equal(decimal.RequireFromString("0.00012345")) {
		t.Errorf("amount lost precision: %s", got.Amount)
	}
}

func TestUpsertUpdatesStatus(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	row := tx("a", time.Now().UTC().Truncate(time.Second), model.Credit, "1", "USDT")
	row.Status = "pending"
	if _, err := s.UpsertTransactions(ctx, []model.Transaction{row}); err != nil {
		t.Fatal(err)
	}
	row.Status = "completed"
	if n, err := s.UpsertTransactions(ctx, []model.Transaction{row}); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	got, _ := s.GetTransaction(ctx, row.ID)
	if got.Status != "completed" {
		t.Errorf("status = %q", got.Status)
	}
}

func TestListPaginatesNewestFirst(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var batch []model.Transaction
	for i := range 5 {
		batch = append(batch, tx(string(rune('a'+i)), base.Add(time.Duration(i)*time.Hour), model.Credit, "1", "VES"))
	}
	if _, err := s.UpsertTransactions(ctx, batch); err != nil {
		t.Fatal(err)
	}

	var seen []string
	cursor := ""
	for {
		page, err := s.ListTransactions(ctx, Filter{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			seen = append(seen, it.ExternalID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	want := []string{"e", "d", "c", "b", "a"}
	if len(seen) != len(want) {
		t.Fatalf("seen %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("seen %v, want %v", seen, want)
		}
	}

	if _, err := s.ListTransactions(ctx, Filter{Cursor: "!!"}); err != ErrBadCursor {
		t.Errorf("bad cursor err = %v", err)
	}
}

func TestSummaryUsesExactDecimals(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	now := time.Now().UTC().Truncate(time.Second)
	batch := []model.Transaction{
		tx("1", now, model.Credit, "0.1", "VES"),
		tx("2", now, model.Credit, "0.2", "VES"),
		tx("3", now, model.Debit, "0.05", "VES"),
		tx("4", now, model.Credit, "10", "USDT"),
	}
	if _, err := s.UpsertTransactions(ctx, batch); err != nil {
		t.Fatal(err)
	}
	totals, err := s.Summary(ctx, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(totals) != 2 {
		t.Fatalf("totals = %+v", totals)
	}
	ves := totals[1]
	if ves.Asset != "VES" || ves.CurrencyLabel != "Bs" || ves.Net.String() != "0.25" || ves.Count != 3 {
		t.Errorf("VES total = %+v", ves)
	}
	if totals[0].CurrencyLabel != "$" {
		t.Errorf("USDT label = %q", totals[0].CurrencyLabel)
	}
}

func TestSyncState(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	at := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	if err := s.RecordSync(ctx, "bnc", at, 3, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordSync(ctx, "bnc", at.Add(time.Hour), 0, context.DeadlineExceeded); err != nil {
		t.Fatal(err)
	}
	states, err := s.SyncStates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st := states["bnc"]
	if st.LastSuccessAt == nil || !st.LastSuccessAt.Equal(at) || st.LastError == "" {
		t.Errorf("state = %+v", st)
	}
}
