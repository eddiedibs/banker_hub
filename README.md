# banker_hub

A small Go API that collects transaction history from several sources into one SQLite table and serves it over HTTP.

| Source  | How                                              | Status      |
|---------|--------------------------------------------------|-------------|
| Binance | Official REST API (HMAC-SHA256 signed)           | Implemented |
| BNC     | Scraping of BNC online banking over plain HTTP   | Implemented |
| PayPal  | Transaction Search API                           | Placeholder |

The BNC client is a Go port of the pure-HTTP flow in [danicanod/Banker](https://github.com/danicanod/Banker).

## Quick start

```sh
make setup   # interactive first-time setup
make run
```

`make setup` runs `cmd/quickstart`. It does the following:

1. Asks for the server settings.
2. Generates an API token.
3. Asks for the Binance and BNC credentials. Secrets are typed without echo.
4. Checks the credentials against the live services. It flags a Binance key that can trade or withdraw, or that has no IP whitelist.
5. Writes `.env` with mode 0600. An existing `.env` is backed up first.
6. Creates the database.
7. Optionally runs the first sync.

Run `make setup` again at any time: it uses the current `.env` values as defaults.

To set up by hand instead, copy `.env.example` to `.env` and fill it in. Providers without credentials are disabled; the server still starts.

```sh
curl -H "Authorization: Bearer $API_TOKEN" -X POST localhost:8080/sync
curl -H "Authorization: Bearer $API_TOKEN" "localhost:8080/transactions?source=bnc&limit=20"
```

## How it works

A background job runs every `SYNC_INTERVAL` and stores new rows in SQLite. The API only reads from SQLite, so the API stays fast and keeps working when a source is down.

At most one sync per provider runs at a time. This matters for BNC, because BNC rejects a new login while another session is open.

Each row has a deterministic id, so syncing the same data twice does not create duplicates. Each sync also re-reads 72 hours before the newest stored row. The first successful sync after every start re-reads BNC from the first day of the current month (Caracas time), so movements missed while the server was down are filled in; if that first sync fails, the next one retries the month. A `POST /sync` without `from` before that first success also gets the month.

BNC is read through its paged "Custom" date-range grid, so a sync no longer loses movements when more than 25 happen between syncs.

BNC sits behind Cloudflare. Many logins in a short time trigger a "Just a moment..." challenge, which shows up as `http 403`. Keep the sync interval moderate and avoid back-to-back BNC syncs. The Custom date picker stops at yesterday, so today's movements come from the "last 25 movements" grid; if the Custom grid fails, a sync falls back to that grid alone. BNC dates have day granularity and are compared as Caracas calendar days. A date-only `from` is read as UTC midnight, which is the previous day in Caracas, so BNC may fetch one extra day (storage deduplicates it).

The Custom grid is read the way the page script does it: the form is posted to `/Accounts/Transactions/Custom_List`, which answers with `{Type, Value}` where `Value` holds the grid HTML and the paging info (`prv_TotalRows`). Pages hold 100 rows. Rows get their time of day from the grid; ids ignore it, so a row seen on both grids is stored once.

## API

All endpoints except `/healthz` require `Authorization: Bearer $API_TOKEN`.

| Method | Path                    | Description |
|--------|-------------------------|-------------|
| GET    | `/healthz`              | Liveness and database check |
| GET    | `/transactions`         | Filters: `source`, `asset`, `direction`, `from`, `to` (RFC3339 or YYYY-MM-DD), `limit`, `cursor` |
| GET    | `/transactions/{id}`    | One transaction |
| GET    | `/summary`              | Credit, debit and net per asset (same filters) |
| GET    | `/providers`            | Registered providers and their last sync state |
| POST   | `/sync`                 | Sync all providers now. Optional `from`, `to` (RFC3339 or YYYY-MM-DD) fetch that window instead of resuming from the newest stored row |
| POST   | `/sync/{provider}`      | Sync one provider, with the same `from` and `to`. Returns 409 if a sync is already running and 501 for PayPal |

Sync responses include `rows_upserted` (rows inserted or updated), `rows_fetched` and `transactions`: every row the provider returned for the window, new or already stored, newest first, in the shape of `GET /transactions` items. If a sync fails part way, `transactions` holds the rows that came back and `error` is set. Example:

```json
{"provider": "bnc", "rows_upserted": 9, "rows_fetched": 9, "transactions": [{"id": "bnc-...", "source": "bnc", "direction": "credit", "amount": "51000", "asset": "VES", "occurred_at": "2026-09-01T17:08:05Z"}]}
```

For `from` and `to`, the window is half-open: `[from, to)`. A date-only `to` includes that whole day. `to` needs `from`, `from` must be before `to`, and `from` cannot be in the future; otherwise the API answers 400. Example, to backfill September:

```sh
curl -H "Authorization: Bearer $API_TOKEN" -X POST "localhost:8080/sync/bnc?from=2026-09-01&to=2026-09-30"
```

`/transactions` is paginated newest first. To get the next page, pass the `next_cursor` value from the response as `cursor`.

## Data model

There is one table, `transaction_history`. Its key columns are:

- `source`: the provider the row came from.
- `external_id`: the provider's id for the row. `source` and `external_id` together are unique.
- `asset`: the real currency or coin code, for example `VES`, `USD`, `USDT` or `BTC`.
- `amount`: a positive exact decimal stored as text, so crypto precision is kept.
- `direction`: `debit` or `credit`.
- `detail`: for BNC, the "Detalle de la Operación" text of the movement (for example the counterparty phone, cédula and bank). It holds personal data, so treat the database and API responses as sensitive. Re-syncing a date range fills it in for rows stored earlier.
- `raw`: the original JSON or HTML row.

The view `transaction_history_display` adds `currency_label`: `Bs` for VES, `$` for USD and USD stablecoins, otherwise the asset code.

Binance P2P orders are stored as two rows that share a `reference`: the crypto leg and the fiat leg. If you also sync the bank account that paid the order, the fiat payment shows up in both sources.

## Security notes

- Use a **read-only** Binance API key with an IP whitelist.
- Credentials come only from environment variables. They are never logged or stored in the database.
- The server binds to `127.0.0.1` by default. Put a TLS reverse proxy in front if you expose it.
- **BNC scraping is not an official integration.** Automated access probably violates BNC's online-banking terms and can get your access locked. Use it only for your own account, and keep the sync interval moderate.
- Each BNC sync logs out at the end. A sync that runs while you are logged in to BNC from a browser can end that browser session.

## Development

Set `BNC_DEBUG_DIR` to save every BNC response body to that directory (mode 0600). Use it only to adapt the parser: the files hold account data, and `debug/` is git-ignored.


```sh
make test   # go test -race ./...
make lint
```

The BNC parser tests use synthetic HTML in `internal/provider/bnc/testdata`. Replace those files with sanitized real captures (with account numbers redacted) once you have them.
