CREATE TABLE transaction_history (
  id          TEXT PRIMARY KEY,                -- "<source>-<sha256[:16]>"
  source      TEXT NOT NULL,                   -- binance | bnc | paypal
  external_id TEXT NOT NULL,                   -- provider id or derived hash
  account     TEXT NOT NULL,
  kind        TEXT NOT NULL,                   -- deposit | withdrawal | c2c | pay | ...
  direction   TEXT NOT NULL CHECK (direction IN ('debit', 'credit')),
  amount      TEXT NOT NULL,                   -- positive decimal string
  asset       TEXT NOT NULL,                   -- VES, USD, USDT, BTC, ...
  status      TEXT,
  reference   TEXT,
  description TEXT,
  occurred_at TEXT NOT NULL,                   -- RFC3339 UTC
  raw         TEXT,                            -- original JSON / HTML row
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
  UNIQUE (source, external_id)
);

CREATE INDEX idx_tx_occurred ON transaction_history (occurred_at DESC, id DESC);
CREATE INDEX idx_tx_source ON transaction_history (source, account);

CREATE VIEW transaction_history_display AS
SELECT
  *,
  CASE upper(asset)
    WHEN 'VES' THEN 'Bs'
    WHEN 'USD' THEN '$'
    WHEN 'USDT' THEN '$'
    WHEN 'USDC' THEN '$'
    WHEN 'FDUSD' THEN '$'
    WHEN 'BUSD' THEN '$'
    WHEN 'DAI' THEN '$'
    ELSE asset
  END AS currency_label
FROM transaction_history;

CREATE TABLE sync_state (
  source          TEXT PRIMARY KEY,
  last_run_at     TEXT,
  last_success_at TEXT,
  last_error      TEXT,
  rows_upserted   INTEGER NOT NULL DEFAULT 0
);
