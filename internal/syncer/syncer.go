// Package syncer pulls transactions from providers into the store.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
)

var (
	// ErrBusy is returned when a sync for the provider is already running.
	ErrBusy = errors.New("sync already running")
	// ErrUnknownProvider is returned for a provider name that is not registered.
	ErrUnknownProvider = errors.New("unknown provider")
)

// overlap re-fetches a window before the newest stored row so that late
// or edited rows are not missed. Storage deduplicates the overlap.
const overlap = 72 * time.Hour

// Store is the subset of the storage layer the syncer needs.
type Store interface {
	UpsertTransactions(ctx context.Context, txs []model.Transaction) (int64, error)
	LatestOccurredAt(ctx context.Context, source string) (time.Time, error)
	RecordSync(ctx context.Context, source string, at time.Time, rows int64, syncErr error) error
}

// Result is the outcome of one provider sync.
// Transactions holds every row the provider returned for the range, new or
// already stored, newest first. On a partial failure it holds what came back.
type Result struct {
	Provider     string              `json:"provider"`
	Rows         int64               `json:"rows_upserted"`
	Fetched      int                 `json:"rows_fetched"`
	Error        string              `json:"error,omitempty"`
	Transactions []model.Transaction `json:"transactions"`
}

// Syncer runs providers. At most one sync per provider runs at a time;
// this matters for BNC, which rejects concurrent sessions.
type Syncer struct {
	store     Store
	providers map[string]provider.Provider
	locks     map[string]*sync.Mutex
	lookback  time.Duration
	// startFrom widens the first zero-range sync of a provider that fetches
	// rows. Both maps are only written before Loop or Run starts; the
	// atomic flags make concurrent providers safe.
	startFrom map[string]func(now time.Time) time.Time
	startedOK map[string]*atomic.Bool
	log       *slog.Logger
	now       func() time.Time
}

// New builds a Syncer. lookback is how far back the first sync reaches.
func New(store Store, lookback time.Duration, log *slog.Logger, providers ...provider.Provider) *Syncer {
	s := &Syncer{
		store:     store,
		providers: map[string]provider.Provider{},
		locks:     map[string]*sync.Mutex{},
		startFrom: map[string]func(time.Time) time.Time{},
		startedOK: map[string]*atomic.Bool{},
		lookback:  lookback,
		log:       log,
		now:       time.Now,
	}
	for _, p := range providers {
		s.providers[p.Name()] = p
		s.locks[p.Name()] = &sync.Mutex{}
		s.startedOK[p.Name()] = &atomic.Bool{}
	}
	return s
}

// StartFrom makes the first zero-range sync of the named provider start at
// fn(now) when that is earlier than the usual start (newest stored row minus
// the overlap). It stays armed until a zero-range sync succeeds or fetches
// rows, so a partial failure (BNC rows plus a Last25 error) does not refetch
// the range on every tick. Call it before Loop or any Run.
//
// POST /sync without a range is a zero range too, so before the first
// success it also gets the wider start.
func (s *Syncer) StartFrom(name string, fn func(now time.Time) time.Time) {
	s.startFrom[name] = fn
}

// Names returns the registered provider names in sorted order.
func (s *Syncer) Names() []string {
	names := make([]string, 0, len(s.providers))
	for n := range s.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// RunAll syncs every provider concurrently. One failure does not stop others.
// A zero r.From syncs from the newest stored row (see Run).
func (s *Syncer) RunAll(ctx context.Context, r provider.Range) []Result {
	names := s.Names()
	results := make([]Result, len(names))
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Go(func() {
			results[i], _ = s.Run(ctx, name, r)
		})
	}
	wg.Wait()
	return results
}

// Run syncs one provider and records the outcome (except for an explicit
// range, which leaves the sync state alone). With a zero r.From it
// resumes from the newest stored row (minus the overlap); otherwise it
// fetches exactly r. The Result is filled even when err is set.
func (s *Syncer) Run(ctx context.Context, name string, r provider.Range) (Result, error) {
	res := Result{Provider: name, Transactions: []model.Transaction{}}
	p, ok := s.providers[name]
	if !ok {
		res.Error = ErrUnknownProvider.Error()
		return res, ErrUnknownProvider
	}
	lock := s.locks[name]
	if !lock.TryLock() {
		res.Error = ErrBusy.Error()
		return res, ErrBusy
	}
	defer lock.Unlock()

	started := s.now()
	explicit := !r.From.IsZero()
	rows, txs, err := s.run(ctx, p, r)
	if errors.Is(err, provider.ErrNotImplemented) {
		s.log.Debug("provider not implemented, skipping", "provider", name)
		res.Error = err.Error()
		return res, err
	}
	// An explicit range (backfill, re-pull) is not the regular sync, so it
	// must not overwrite last_success_at / last_error.
	if !explicit {
		if err == nil || len(txs) > 0 {
			s.startedOK[name].Store(true)
		}
		if recErr := s.store.RecordSync(context.WithoutCancel(ctx), name, started, rows, err); recErr != nil {
			s.log.Error("record sync state", "provider", name, "err", recErr)
		}
	}
	if err != nil {
		s.log.Warn("sync failed", "provider", name, "err", err, "duration", time.Since(started))
		res.Error = err.Error()
	} else {
		s.log.Info("sync done", "provider", name, "rows", rows, "duration", time.Since(started))
	}
	res.Rows, res.Fetched = rows, len(txs)
	if len(txs) > 0 {
		// Newest first, like GET /transactions.
		sort.Slice(txs, func(i, j int) bool {
			if !txs[i].OccurredAt.Equal(txs[j].OccurredAt) {
				return txs[i].OccurredAt.After(txs[j].OccurredAt)
			}
			return txs[i].ID > txs[j].ID
		})
		res.Transactions = txs
	}
	return res, err
}

// run returns the rows upserted and the rows fetched.
func (s *Syncer) run(ctx context.Context, p provider.Provider, r provider.Range) (int64, []model.Transaction, error) {
	if r.From.IsZero() {
		since, err := s.store.LatestOccurredAt(ctx, p.Name())
		if err != nil {
			return 0, nil, fmt.Errorf("latest occurred_at: %w", err)
		}
		if since.IsZero() {
			since = s.now().Add(-s.lookback)
		} else {
			since = since.Add(-overlap)
		}
		if fn := s.startFrom[p.Name()]; fn != nil && !s.startedOK[p.Name()].Load() {
			if start := fn(s.now()); start.Before(since) {
				since = start
			}
		}
		r.From = since
	}

	txs, err := p.Fetch(ctx, r)
	txs = dedupe(txs)
	// Store whatever was fetched even on partial failure.
	rows, upErr := s.store.UpsertTransactions(ctx, txs)
	if upErr != nil {
		// The batch was rolled back: report no rows, so the response does
		// not list unstored rows and the startup start stays armed.
		return rows, nil, errors.Join(err, fmt.Errorf("upsert: %w", upErr))
	}
	return rows, txs, err
}

// dedupe drops repeated ids, keeping the first, so rows_fetched and the
// returned list match what storage holds.
func dedupe(txs []model.Transaction) []model.Transaction {
	seen := make(map[string]bool, len(txs))
	out := txs[:0]
	for _, t := range txs {
		if !seen[t.ID] {
			seen[t.ID] = true
			out = append(out, t)
		}
	}
	return out
}

// Loop runs RunAll every interval until ctx is cancelled. It runs once at start.
func (s *Syncer) Loop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		s.RunAll(ctx, provider.Range{})
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
