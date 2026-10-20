ALTER TABLE transaction_history ADD COLUMN detail TEXT;

-- SELECT * is expanded when a view is created, so recreate it to expose detail.
DROP VIEW transaction_history_display;

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
