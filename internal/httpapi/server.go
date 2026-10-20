// Package httpapi exposes stored transactions and sync controls over HTTP.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
	"github.com/eddiedibs/banker_hub/internal/store"
	"github.com/eddiedibs/banker_hub/internal/syncer"
)

// Store is the subset of the storage layer the API reads.
type Store interface {
	Ping(ctx context.Context) error
	ListTransactions(ctx context.Context, f store.Filter) (store.Page, error)
	GetTransaction(ctx context.Context, id string) (model.Transaction, error)
	Summary(ctx context.Context, f store.Filter) ([]store.Total, error)
	SyncStates(ctx context.Context) (map[string]store.SyncState, error)
}

// Syncer is the subset of the sync service the API triggers.
type Syncer interface {
	Names() []string
	RunAll(ctx context.Context, r provider.Range) []syncer.Result
	Run(ctx context.Context, name string, r provider.Range) (syncer.Result, error)
}

type API struct {
	store  Store
	syncer Syncer
	log    *slog.Logger
}

// NewHandler returns the root handler with middleware applied.
func NewHandler(st Store, sy Syncer, log *slog.Logger, apiToken string) http.Handler {
	a := &API{store: st, syncer: sy, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /transactions", a.listTransactions)
	mux.HandleFunc("GET /transactions/{id}", a.getTransaction)
	mux.HandleFunc("GET /summary", a.summary)
	mux.HandleFunc("GET /providers", a.providers)
	mux.HandleFunc("POST /sync", a.syncAll)
	mux.HandleFunc("POST /sync/{provider}", a.syncOne)

	var h http.Handler = mux
	h = bearerAuth(apiToken, "/healthz")(h)
	h = recoverPanics(log)(h)
	h = logRequests(log)(h)
	h = requestID(h)
	return h
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) listTransactions(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	page, err := a.store.ListTransactions(r.Context(), f)
	if errors.Is(err, store.ErrBadCursor) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (a *API) getTransaction(w http.ResponseWriter, r *http.Request) {
	t, err := a.store.GetTransaction(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "transaction not found")
		return
	}
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (a *API) summary(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilter(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	totals, err := a.store.Summary(r.Context(), f)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"totals": totals})
}

type providerInfo struct {
	Name  string           `json:"name"`
	State *store.SyncState `json:"sync_state,omitempty"`
}

func (a *API) providers(w http.ResponseWriter, r *http.Request) {
	states, err := a.store.SyncStates(r.Context())
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out := []providerInfo{}
	for _, name := range a.syncer.Names() {
		info := providerInfo{Name: name}
		if st, ok := states[name]; ok {
			info.State = &st
		}
		out = append(out, info)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": out})
}

func (a *API) syncAll(w http.ResponseWriter, r *http.Request) {
	rng, err := parseRange(r, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": a.syncer.RunAll(r.Context(), rng)})
}

func (a *API) syncOne(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	rng, err := parseRange(r, time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := a.syncer.Run(r.Context(), name, rng)
	switch {
	case errors.Is(err, syncer.ErrUnknownProvider):
		writeError(w, http.StatusNotFound, "unknown provider")
	case errors.Is(err, syncer.ErrBusy):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, provider.ErrNotImplemented):
		writeError(w, http.StatusNotImplemented, err.Error())
	case err != nil:
		writeJSON(w, http.StatusBadGateway, res)
	default:
		writeJSON(w, http.StatusOK, res)
	}
}

func (a *API) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("request failed", "path", r.URL.Path, "err", err, "request_id", r.Context().Value(requestIDKey))
	writeError(w, http.StatusInternalServerError, "internal error")
}

func parseFilter(r *http.Request) (store.Filter, error) {
	q := r.URL.Query()
	f := store.Filter{
		Source: q.Get("source"),
		Asset:  q.Get("asset"),
		Cursor: q.Get("cursor"),
	}
	if d := q.Get("direction"); d != "" {
		f.Direction = model.Direction(d)
		if !f.Direction.Valid() {
			return f, errors.New("direction must be debit or credit")
		}
	}
	var err error
	if f.From, err = parseTime(q.Get("from")); err != nil {
		return f, errors.New("from: use RFC3339 or YYYY-MM-DD")
	}
	if f.To, err = parseTime(q.Get("to")); err != nil {
		return f, errors.New("to: use RFC3339 or YYYY-MM-DD")
	}
	if l := q.Get("limit"); l != "" {
		if f.Limit, err = strconv.Atoi(l); err != nil || f.Limit < 1 {
			return f, errors.New("limit must be a positive integer")
		}
	}
	return f, nil
}

// parseRange reads the optional from/to query of the sync endpoints.
// Without from, the zero Range lets the syncer pick its own start.
// A date-only to includes that whole day, so it becomes the next midnight
// (the range itself is half-open, like the to filter of GET /transactions).
func parseRange(r *http.Request, now time.Time) (provider.Range, error) {
	q := r.URL.Query()
	var rng provider.Range
	var err error
	if rng.From, err = parseTime(q.Get("from")); err != nil {
		return rng, errors.New("from: use RFC3339 or YYYY-MM-DD")
	}
	if rng.To, err = parseTime(q.Get("to")); err != nil {
		return rng, errors.New("to: use RFC3339 or YYYY-MM-DD")
	}
	if rng.To.IsZero() {
		if rng.From.After(now) {
			return rng, errors.New("from must not be in the future")
		}
		return rng, nil
	}
	if rng.From.IsZero() {
		return rng, errors.New("to requires from")
	}
	if _, derr := time.Parse(time.DateOnly, q.Get("to")); derr == nil {
		rng.To = rng.To.AddDate(0, 0, 1)
	}
	if !rng.From.Before(rng.To) {
		return rng, errors.New("from must be before to")
	}
	if rng.From.After(now) {
		return rng, errors.New("from must not be in the future")
	}
	return rng, nil
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse(time.DateOnly, s)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
