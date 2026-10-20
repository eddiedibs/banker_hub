package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
	"github.com/eddiedibs/banker_hub/internal/provider/paypal"
	"github.com/eddiedibs/banker_hub/internal/store"
	"github.com/eddiedibs/banker_hub/internal/syncer"
)

// fakeProvider records the last range it was asked for.
type fakeProvider struct {
	txs []model.Transaction
	got *provider.Range
}

func (fakeProvider) Name() string { return "fake" }
func (f fakeProvider) Fetch(_ context.Context, r provider.Range) ([]model.Transaction, error) {
	*f.got = r
	return f.txs, nil
}

const token = "test-token"

func newTestAPI(t *testing.T) http.Handler {
	t.Helper()
	h, _ := newTestAPIRange(t)
	return h
}

// newTestAPIRange also returns the range the fake provider last received.
func newTestAPIRange(t *testing.T) (http.Handler, *provider.Range) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	got := &provider.Range{}
	fake := fakeProvider{got: got, txs: []model.Transaction{{
		ID: model.MakeID("fake", "1"), Source: "fake", ExternalID: "1", Account: "main",
		Kind: "transfer", Direction: model.Credit, Amount: decimal.RequireFromString("12.34"),
		Asset: "VES", OccurredAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}}}
	sy := syncer.New(st, 24*time.Hour, log, fake, paypal.New())
	return NewHandler(st, sy, log, token), got
}

func do(t *testing.T, h http.Handler, method, path string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if auth {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestAuthRequired(t *testing.T) {
	h := newTestAPI(t)
	if rec := do(t, h, "GET", "/transactions", false); rec.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/healthz", false); rec.Code != http.StatusOK {
		t.Errorf("healthz must be public: %d", rec.Code)
	}
}

func TestSyncThenList(t *testing.T) {
	h := newTestAPI(t)

	rec := do(t, h, "POST", "/sync/fake", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	if rec := do(t, h, "POST", "/sync/paypal", true); rec.Code != http.StatusNotImplemented {
		t.Errorf("paypal sync: %d", rec.Code)
	}
	if rec := do(t, h, "POST", "/sync/nope", true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown provider: %d", rec.Code)
	}

	rec = do(t, h, "GET", "/transactions?asset=ves", true)
	var page store.Page
	if err := json.NewDecoder(rec.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Amount.String() != "12.34" {
		t.Fatalf("page = %+v", page)
	}

	id := page.Items[0].ID
	if rec := do(t, h, "GET", "/transactions/"+id, true); rec.Code != http.StatusOK {
		t.Errorf("get: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/transactions/missing", true); rec.Code != http.StatusNotFound {
		t.Errorf("missing: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/transactions?direction=up", true); rec.Code != http.StatusBadRequest {
		t.Errorf("bad direction: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/summary", true); rec.Code != http.StatusOK {
		t.Errorf("summary: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/providers", true); rec.Code != http.StatusOK {
		t.Errorf("providers: %d", rec.Code)
	}
}

func TestSyncRange(t *testing.T) {
	h, got := newTestAPIRange(t)

	rec := do(t, h, "POST", "/sync/fake?from=2026-09-01&to=2026-09-30", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	// A date-only to includes that day: the range ends at the next midnight.
	if want := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); !got.From.Equal(want) {
		t.Errorf("from = %s, want %s", got.From, want)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !got.To.Equal(want) {
		t.Errorf("to = %s, want %s", got.To, want)
	}

	rec = do(t, h, "POST", "/sync/fake?from=2026-09-01T10:00:00Z&to=2026-09-02T10:00:00Z", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync rfc3339: %d %s", rec.Code, rec.Body)
	}
	if want := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC); !got.To.Equal(want) {
		t.Errorf("rfc3339 to = %s, want %s", got.To, want)
	}

	// POST /sync passes the range to every provider too.
	*got = provider.Range{}
	if rec := do(t, h, "POST", "/sync?from=2026-09-01", true); rec.Code != http.StatusOK {
		t.Fatalf("sync all: %d %s", rec.Code, rec.Body)
	}
	if got.From.IsZero() || !got.To.IsZero() {
		t.Errorf("sync all range = %+v", *got)
	}

	// No query: the syncer picks its own start (never zero by the time it
	// reaches the provider).
	*got = provider.Range{}
	do(t, h, "POST", "/sync/fake", true)
	if got.From.IsZero() {
		t.Error("default sync must still get a from")
	}
}

func TestSyncRangeBadInput(t *testing.T) {
	h := newTestAPI(t)
	future := time.Now().AddDate(0, 0, 3).Format(time.DateOnly)
	for name, path := range map[string]string{
		"bad from":               "/sync/fake?from=yesterday",
		"bad to":                 "/sync/fake?from=2026-09-01&to=soon",
		"to without from":        "/sync/fake?to=2026-09-30",
		"from after to":          "/sync/fake?from=2026-09-30&to=2026-09-01",
		"from equals to rfc3339": "/sync/fake?from=2026-09-01T00:00:00Z&to=2026-09-01T00:00:00Z",
		"future from":            "/sync/fake?from=" + future,
		"all: bad from":          "/sync?from=nope",
	} {
		if rec := do(t, h, "POST", path, true); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
}

func TestSyncResponseIncludesTransactions(t *testing.T) {
	h := newTestAPI(t)

	rec := do(t, h, "POST", "/sync/fake?from=2026-09-01&to=2026-09-30", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("sync: %d %s", rec.Code, rec.Body)
	}
	var res syncer.Result
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.Fetched != 1 || len(res.Transactions) != 1 {
		t.Fatalf("result = %+v", res)
	}
	if tx := res.Transactions[0]; tx.ID == "" || tx.Amount.String() != "12.34" || tx.Direction != model.Credit {
		t.Errorf("tx = %+v", tx)
	}
	// Fetched rows have no store timestamp and never expose raw data.
	var generic struct {
		Transactions []map[string]any `json:"transactions"`
	}
	json.Unmarshal(rec.Body.Bytes(), &generic)
	for _, bad := range []string{"raw", "created_at"} {
		if _, ok := generic.Transactions[0][bad]; ok {
			t.Errorf("%q must not be in a fetched row", bad)
		}
	}

	rec = do(t, h, "POST", "/sync", true)
	var all struct {
		Results []syncer.Result `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Results) == 0 || all.Results[0].Provider != "fake" || len(all.Results[0].Transactions) != 1 {
		t.Errorf("results = %+v", all.Results)
	}
	// PayPal has nothing: the field is [], not null.
	if !strings.Contains(rec.Body.String(), `"transactions":[]`) {
		t.Errorf("empty transactions must be []: %s", rec.Body)
	}
}
