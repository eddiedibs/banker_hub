package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
)

type fakeStore struct {
	latest   time.Time
	recorded *int
}

func (fakeStore) UpsertTransactions(context.Context, []model.Transaction) (int64, error) {
	return 0, nil
}
func (f fakeStore) LatestOccurredAt(context.Context, string) (time.Time, error) {
	return f.latest, nil
}
func (f fakeStore) RecordSync(context.Context, string, time.Time, int64, error) error {
	if f.recorded != nil {
		*f.recorded++
	}
	return nil
}

type fakeProvider struct{ got provider.Range }

func (*fakeProvider) Name() string { return "fake" }
func (f *fakeProvider) Fetch(_ context.Context, r provider.Range) ([]model.Transaction, error) {
	f.got = r
	return nil, nil
}

func TestRunRange(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	latest := time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC)
	explicit := provider.Range{
		From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		To:   time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}

	for _, tc := range []struct {
		name   string
		latest time.Time
		in     provider.Range
		want   provider.Range
	}{
		{"zero range resumes from latest minus overlap", latest, provider.Range{}, provider.Range{From: latest.Add(-overlap)}},
		{"zero range on empty store uses lookback", time.Time{}, provider.Range{}, provider.Range{From: now.Add(-24 * time.Hour)}},
		{"explicit range is passed through", latest, explicit, explicit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProvider{}
			s := New(fakeStore{latest: tc.latest}, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), fp)
			s.now = func() time.Time { return now }
			if _, err := s.Run(context.Background(), "fake", tc.in); err != nil {
				t.Fatal(err)
			}
			if !fp.got.From.Equal(tc.want.From) || !fp.got.To.Equal(tc.want.To) {
				t.Errorf("fetched %+v, want %+v", fp.got, tc.want)
			}
		})
	}
}

func TestExplicitRangeSkipsRecordSync(t *testing.T) {
	var recorded int
	fp := &fakeProvider{}
	s := New(fakeStore{recorded: &recorded}, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), fp)

	explicit := provider.Range{From: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)}
	if _, err := s.Run(context.Background(), "fake", explicit); err != nil {
		t.Fatal(err)
	}
	if recorded != 0 {
		t.Errorf("explicit range recorded sync state %d times", recorded)
	}
	if _, err := s.Run(context.Background(), "fake", provider.Range{}); err != nil {
		t.Fatal(err)
	}
	if recorded != 1 {
		t.Errorf("regular sync recorded %d times, want 1", recorded)
	}
}

type failingProvider struct {
	fakeProvider
	fail bool
}

func (f *failingProvider) Fetch(ctx context.Context, r provider.Range) ([]model.Transaction, error) {
	_, _ = f.fakeProvider.Fetch(ctx, r)
	if f.fail {
		return nil, errors.New("boom")
	}
	return nil, nil
}

func TestStartFromWidensFirstSuccessfulRun(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	latest := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)

	fp := &failingProvider{fail: true}
	s := New(fakeStore{latest: latest}, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), fp)
	s.now = func() time.Time { return now }
	s.StartFrom("fake", func(time.Time) time.Time { return monthStart })

	run := func() time.Time {
		t.Helper()
		s.Run(context.Background(), "fake", provider.Range{})
		return fp.got.From
	}
	// A failed first run keeps the month armed for the next tick.
	if got := run(); !got.Equal(monthStart) {
		t.Errorf("first run from %s, want month start", got)
	}
	fp.fail = false
	if got := run(); !got.Equal(monthStart) {
		t.Errorf("retry from %s, want month start", got)
	}
	// After a success the normal incremental start applies.
	if got, want := run(), latest.Add(-overlap); !got.Equal(want) {
		t.Errorf("later run from %s, want %s", got, want)
	}
}

func TestStartFromIgnoredWhenLaterThanNormal(t *testing.T) {
	latest := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	fp := &fakeProvider{}
	s := New(fakeStore{latest: latest}, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), fp)
	s.StartFrom("fake", func(time.Time) time.Time { return latest.AddDate(0, 0, 10) })
	s.Run(context.Background(), "fake", provider.Range{})
	if want := latest.Add(-overlap); !fp.got.From.Equal(want) {
		t.Errorf("from %s, want %s", fp.got.From, want)
	}
}

type partialProvider struct{ fakeProvider }

func (f *partialProvider) Fetch(ctx context.Context, r provider.Range) ([]model.Transaction, error) {
	_, _ = f.fakeProvider.Fetch(ctx, r)
	return []model.Transaction{{ID: "x"}}, errors.New("last 25: http 403")
}

func TestStartFromDisarmsOnPartialFailure(t *testing.T) {
	latest := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	monthStart := time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC)
	fp := &partialProvider{}
	s := New(fakeStore{latest: latest}, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), fp)
	s.StartFrom("fake", func(time.Time) time.Time { return monthStart })

	s.Run(context.Background(), "fake", provider.Range{})
	if !fp.got.From.Equal(monthStart) {
		t.Fatalf("first run from %s, want month start", fp.got.From)
	}
	// Rows came back, so the month is not fetched again on the next tick.
	s.Run(context.Background(), "fake", provider.Range{})
	if want := latest.Add(-overlap); !fp.got.From.Equal(want) {
		t.Errorf("second run from %s, want %s", fp.got.From, want)
	}
}

type namedProvider struct{ name string }

func (p namedProvider) Name() string { return p.name }
func (namedProvider) Fetch(context.Context, provider.Range) ([]model.Transaction, error) {
	return []model.Transaction{{ID: "x"}}, nil
}

// TestRunAllConcurrentStartFlags is meant for -race: providers finishing at
// the same time must not race on the start flags.
func TestRunAllConcurrentStartFlags(t *testing.T) {
	var ps []provider.Provider
	for i := range 8 {
		ps = append(ps, namedProvider{name: string(rune('a' + i))})
	}
	s := New(fakeStore{}, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), ps...)
	s.StartFrom("a", func(now time.Time) time.Time { return now.AddDate(0, -1, 0) })
	for range 3 {
		s.RunAll(context.Background(), provider.Range{})
	}
}

type rowsProvider struct {
	txs []model.Transaction
	err error
}

func (rowsProvider) Name() string { return "rows" }
func (r rowsProvider) Fetch(context.Context, provider.Range) ([]model.Transaction, error) {
	return r.txs, r.err
}

func newRowsSyncer(p rowsProvider) *Syncer {
	return New(fakeStore{}, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), p)
}

func TestRunReturnsRowsNewestFirst(t *testing.T) {
	at := func(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }
	s := newRowsSyncer(rowsProvider{txs: []model.Transaction{
		{ID: "a", OccurredAt: at(1)}, {ID: "c", OccurredAt: at(3)}, {ID: "b", OccurredAt: at(3)},
	}})
	res, err := s.Run(context.Background(), "rows", provider.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 3 || len(res.Transactions) != 3 {
		t.Fatalf("fetched = %d, transactions = %d", res.Fetched, len(res.Transactions))
	}
	var ids string
	for _, tx := range res.Transactions {
		ids += tx.ID
	}
	if ids != "cba" {
		t.Errorf("order = %q, want cba", ids)
	}
}

func TestRunPartialFailureKeepsRows(t *testing.T) {
	s := newRowsSyncer(rowsProvider{
		txs: []model.Transaction{{ID: "a"}},
		err: errors.New("boom"),
	})
	res, err := s.Run(context.Background(), "rows", provider.Range{})
	if err == nil || res.Error != "boom" {
		t.Fatalf("err = %v, result error = %q", err, res.Error)
	}
	if res.Fetched != 1 || len(res.Transactions) != 1 {
		t.Errorf("partial rows lost: %+v", res)
	}
}

func TestRunEmptyFetchIsEmptySlice(t *testing.T) {
	s := newRowsSyncer(rowsProvider{})
	res, err := s.Run(context.Background(), "rows", provider.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Transactions == nil || len(res.Transactions) != 0 {
		t.Errorf("transactions = %#v, want empty non-nil slice", res.Transactions)
	}
	if _, err := s.Run(context.Background(), "nope", provider.Range{}); !errors.Is(err, ErrUnknownProvider) {
		t.Errorf("err = %v", err)
	}
}

type dupProvider struct{}

func (dupProvider) Name() string { return "fake" }
func (dupProvider) Fetch(context.Context, provider.Range) ([]model.Transaction, error) {
	return []model.Transaction{{ID: "a"}, {ID: "b"}, {ID: "a"}}, nil
}

func TestRunDedupesFetchedRows(t *testing.T) {
	s := New(fakeStore{}, 24*time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)), dupProvider{})
	res, err := s.Run(context.Background(), "fake", provider.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 2 || len(res.Transactions) != 2 {
		t.Errorf("fetched %d, %d transactions, want 2", res.Fetched, len(res.Transactions))
	}
}
