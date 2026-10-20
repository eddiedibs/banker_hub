package store

import (
	"context"
	"database/sql"
	"time"
)

// SyncState records the outcome of the latest sync for a source.
type SyncState struct {
	Source        string     `json:"source"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
	RowsUpserted  int64      `json:"rows_upserted"`
}

// RecordSync stores the result of a sync run. A nil syncErr marks success.
func (s *Store) RecordSync(ctx context.Context, source string, at time.Time, rows int64, syncErr error) error {
	ts := at.UTC().Format(timeLayout)
	if syncErr == nil {
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO sync_state (source, last_run_at, last_success_at, last_error, rows_upserted)
			VALUES (?, ?, ?, NULL, ?)
			ON CONFLICT (source) DO UPDATE SET
				last_run_at = excluded.last_run_at,
				last_success_at = excluded.last_success_at,
				last_error = NULL,
				rows_upserted = excluded.rows_upserted`,
			source, ts, ts, rows)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_state (source, last_run_at, last_error, rows_upserted)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (source) DO UPDATE SET
			last_run_at = excluded.last_run_at,
			last_error = excluded.last_error,
			rows_upserted = excluded.rows_upserted`,
		source, ts, syncErr.Error(), rows)
	return err
}

// SyncStates returns the sync state of every source that has run.
func (s *Store) SyncStates(ctx context.Context) (map[string]SyncState, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT source, last_run_at, last_success_at, last_error, rows_upserted FROM sync_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]SyncState{}
	for rows.Next() {
		var st SyncState
		var run, success, lastErr sql.NullString
		if err := rows.Scan(&st.Source, &run, &success, &lastErr, &st.RowsUpserted); err != nil {
			return nil, err
		}
		st.LastRunAt = parseNullTime(run)
		st.LastSuccessAt = parseNullTime(success)
		st.LastError = lastErr.String
		out[st.Source] = st
	}
	return out, rows.Err()
}

// LatestOccurredAt returns the newest occurred_at stored for source,
// or the zero time when the source has no rows.
func (s *Store) LatestOccurredAt(ctx context.Context, source string) (time.Time, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT max(occurred_at) FROM transaction_history WHERE source = ?`, source).Scan(&v)
	if err != nil || !v.Valid {
		return time.Time{}, err
	}
	return time.Parse(timeLayout, v.String)
}

func parseNullTime(v sql.NullString) *time.Time {
	if !v.Valid {
		return nil
	}
	t, err := time.Parse(timeLayout, v.String)
	if err != nil {
		return nil
	}
	return &t
}
