package binance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
)

// Name is the source identifier for Binance rows.
const Name = "binance"

// Maximum time ranges accepted per request by each endpoint.
const (
	capitalWindow = 90 * 24 * time.Hour
	payWindow     = 90 * 24 * time.Hour
	c2cWindow     = 30 * 24 * time.Hour
)

// Provider pulls deposits, withdrawals, Binance Pay and P2P (C2C) history.
type Provider struct {
	c *client
}

// Option customizes a Provider.
type Option func(*Provider)

// WithHTTPClient replaces the default HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(p *Provider) { p.c.http = h } }

// New returns a Binance provider. Use a read-only API key.
func New(baseURL, apiKey, apiSecret string, opts ...Option) *Provider {
	p := &Provider{c: &client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		apiKey:    apiKey,
		apiSecret: apiSecret,
		http:      &http.Client{Timeout: 30 * time.Second},
		now:       time.Now,
		sleep:     sleepCtx,
	}}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (*Provider) Name() string { return Name }

// Fetch collects every supported history type inside r.
// It keeps going after a failing endpoint and returns the joined errors
// together with the rows that were fetched.
func (p *Provider) Fetch(ctx context.Context, r provider.Range) ([]model.Transaction, error) {
	if err := p.c.syncTime(ctx); err != nil {
		return nil, fmt.Errorf("server time: %w", err)
	}
	until := p.c.now().Add(p.c.offset)
	if !r.To.IsZero() && r.To.Before(until) {
		until = r.To
	}

	var all []model.Transaction
	var errs []error
	for _, f := range []struct {
		name string
		fn   func(context.Context, time.Time, time.Time) ([]model.Transaction, error)
	}{
		{"deposits", p.deposits},
		{"withdrawals", p.withdrawals},
		{"pay", p.pay},
		{"c2c", p.c2c},
	} {
		txs, err := f.fn(ctx, r.From, until)
		all = append(all, txs...)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.name, err))
			if ctx.Err() != nil {
				break
			}
		}
	}
	return all, errors.Join(errs...)
}

// windows splits [from, to) into consecutive ranges no longer than size.
func windows(from, to time.Time, size time.Duration) [][2]time.Time {
	var out [][2]time.Time
	for start := from; start.Before(to); start = start.Add(size) {
		end := start.Add(size)
		if end.After(to) {
			end = to
		}
		out = append(out, [2]time.Time{start, end})
	}
	return out
}

func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func walletName(t int) string {
	if t == 1 {
		return "funding"
	}
	return "spot"
}

// --- deposits ---------------------------------------------------------------

type deposit struct {
	ID         string `json:"id"`
	Amount     string `json:"amount"`
	Coin       string `json:"coin"`
	Network    string `json:"network"`
	Status     int    `json:"status"`
	TxID       string `json:"txId"`
	InsertTime int64  `json:"insertTime"`
	WalletType int    `json:"walletType"`
}

// depositSuccess is the "success" status of /capital/deposit/hisrec.
const depositSuccess = 1

func (p *Provider) deposits(ctx context.Context, from, to time.Time) ([]model.Transaction, error) {
	const limit = 1000
	var out []model.Transaction
	for _, w := range windows(from, to, capitalWindow) {
		for offset := 0; ; offset += limit {
			var page []json.RawMessage
			err := p.c.get(ctx, "/sapi/v1/capital/deposit/hisrec", url.Values{
				"startTime": {ms(w[0])},
				"endTime":   {ms(w[1])},
				"limit":     {strconv.Itoa(limit)},
				"offset":    {strconv.Itoa(offset)},
			}, &page)
			if err != nil {
				return out, err
			}
			for _, raw := range page {
				var d deposit
				if err := json.Unmarshal(raw, &d); err != nil {
					return out, err
				}
				if d.Status != depositSuccess {
					continue
				}
				amount, err := decimal.NewFromString(d.Amount)
				if err != nil {
					return out, fmt.Errorf("deposit %s amount: %w", d.ID, err)
				}
				out = append(out, model.Transaction{
					ID:          model.MakeID(Name, "deposit:"+d.ID),
					Source:      Name,
					ExternalID:  "deposit:" + d.ID,
					Account:     walletName(d.WalletType),
					Kind:        "deposit",
					Direction:   model.Credit,
					Amount:      amount,
					Asset:       d.Coin,
					Status:      "completed",
					Reference:   d.TxID,
					Description: "Deposit via " + d.Network,
					OccurredAt:  time.UnixMilli(d.InsertTime).UTC(),
					Raw:         string(raw),
				})
			}
			if len(page) < limit {
				break
			}
		}
	}
	return out, nil
}

// --- withdrawals ------------------------------------------------------------

type withdrawal struct {
	ID             string `json:"id"`
	Amount         string `json:"amount"`
	TransactionFee string `json:"transactionFee"`
	Coin           string `json:"coin"`
	Network        string `json:"network"`
	Status         int    `json:"status"`
	TxID           string `json:"txId"`
	ApplyTime      string `json:"applyTime"` // "2006-01-02 15:04:05" UTC
	WalletType     int    `json:"walletType"`
}

// withdrawCompleted is the "completed" status of /capital/withdraw/history.
const withdrawCompleted = 6

func (p *Provider) withdrawals(ctx context.Context, from, to time.Time) ([]model.Transaction, error) {
	const limit = 1000
	var out []model.Transaction
	for _, w := range windows(from, to, capitalWindow) {
		for offset := 0; ; offset += limit {
			var page []json.RawMessage
			err := p.c.get(ctx, "/sapi/v1/capital/withdraw/history", url.Values{
				"startTime": {ms(w[0])},
				"endTime":   {ms(w[1])},
				"limit":     {strconv.Itoa(limit)},
				"offset":    {strconv.Itoa(offset)},
			}, &page)
			if err != nil {
				return out, err
			}
			for _, raw := range page {
				var wd withdrawal
				if err := json.Unmarshal(raw, &wd); err != nil {
					return out, err
				}
				if wd.Status != withdrawCompleted {
					continue
				}
				amount, err := decimal.NewFromString(wd.Amount)
				if err != nil {
					return out, fmt.Errorf("withdrawal %s amount: %w", wd.ID, err)
				}
				at, err := time.Parse(time.DateTime, wd.ApplyTime)
				if err != nil {
					return out, fmt.Errorf("withdrawal %s applyTime: %w", wd.ID, err)
				}
				desc := "Withdrawal via " + wd.Network
				if wd.TransactionFee != "" {
					desc += " (fee " + wd.TransactionFee + " " + wd.Coin + ")"
				}
				out = append(out, model.Transaction{
					ID:          model.MakeID(Name, "withdrawal:"+wd.ID),
					Source:      Name,
					ExternalID:  "withdrawal:" + wd.ID,
					Account:     walletName(wd.WalletType),
					Kind:        "withdrawal",
					Direction:   model.Debit,
					Amount:      amount,
					Asset:       wd.Coin,
					Status:      "completed",
					Reference:   wd.TxID,
					Description: desc,
					OccurredAt:  at.UTC(),
					Raw:         string(raw),
				})
			}
			if len(page) < limit {
				break
			}
		}
	}
	return out, nil
}

// --- Binance Pay ------------------------------------------------------------

type party struct {
	Name string `json:"name"`
}

type payTx struct {
	OrderType       string `json:"orderType"`
	TransactionID   string `json:"transactionId"`
	TransactionTime int64  `json:"transactionTime"`
	Amount          string `json:"amount"` // signed
	Currency        string `json:"currency"`
	PayerInfo       party  `json:"payerInfo"`
	ReceiverInfo    party  `json:"receiverInfo"`
}

type sapiEnvelope struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Data    []json.RawMessage `json:"data"`
	Total   int               `json:"total"`
}

func (e sapiEnvelope) err() error {
	if e.Code != "" && e.Code != "000000" {
		return fmt.Errorf("binance: code %s: %s", e.Code, e.Message)
	}
	return nil
}

func (p *Provider) pay(ctx context.Context, from, to time.Time) ([]model.Transaction, error) {
	var out []model.Transaction
	for _, w := range windows(from, to, payWindow) {
		txs, err := p.payRange(ctx, w[0], w[1])
		out = append(out, txs...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// payRange fetches one range. The endpoint has no paging, so a full page
// means the range may be truncated and is split in half.
func (p *Provider) payRange(ctx context.Context, from, to time.Time) ([]model.Transaction, error) {
	const limit = 100
	var env sapiEnvelope
	err := p.c.get(ctx, "/sapi/v1/pay/transactions", url.Values{
		"startTime": {ms(from)},
		"endTime":   {ms(to)},
		"limit":     {strconv.Itoa(limit)},
	}, &env)
	if err != nil {
		return nil, err
	}
	if err := env.err(); err != nil {
		return nil, err
	}
	if len(env.Data) >= limit && to.Sub(from) > time.Minute {
		mid := from.Add(to.Sub(from) / 2)
		first, err := p.payRange(ctx, from, mid)
		if err != nil {
			return first, err
		}
		second, err := p.payRange(ctx, mid, to)
		return append(first, second...), err
	}

	out := make([]model.Transaction, 0, len(env.Data))
	for _, raw := range env.Data {
		var t payTx
		if err := json.Unmarshal(raw, &t); err != nil {
			return out, err
		}
		amount, err := decimal.NewFromString(t.Amount)
		if err != nil {
			return out, fmt.Errorf("pay %s amount: %w", t.TransactionID, err)
		}
		dir, counterparty := model.Credit, t.PayerInfo.Name
		if amount.IsNegative() {
			dir, counterparty = model.Debit, t.ReceiverInfo.Name
		}
		desc := "Binance Pay " + t.OrderType
		if counterparty != "" {
			desc += " - " + counterparty
		}
		out = append(out, model.Transaction{
			ID:          model.MakeID(Name, "pay:"+t.TransactionID),
			Source:      Name,
			ExternalID:  "pay:" + t.TransactionID,
			Account:     "funding",
			Kind:        "pay",
			Direction:   dir,
			Amount:      amount.Abs(),
			Asset:       t.Currency,
			Status:      "completed",
			Reference:   t.TransactionID,
			Description: desc,
			OccurredAt:  time.UnixMilli(t.TransactionTime).UTC(),
			Raw:         string(raw),
		})
	}
	return out, nil
}

// --- P2P (C2C) --------------------------------------------------------------

type c2cOrder struct {
	OrderNumber         string `json:"orderNumber"`
	TradeType           string `json:"tradeType"` // BUY or SELL (of the crypto asset)
	Asset               string `json:"asset"`
	Fiat                string `json:"fiat"`
	Amount              string `json:"amount"`     // crypto quantity
	TotalPrice          string `json:"totalPrice"` // fiat total
	UnitPrice           string `json:"unitPrice"`
	OrderStatus         string `json:"orderStatus"`
	CreateTime          int64  `json:"createTime"`
	CounterPartNickName string `json:"counterPartNickName"`
}

func (p *Provider) c2c(ctx context.Context, from, to time.Time) ([]model.Transaction, error) {
	const rows = 100
	var out []model.Transaction
	for _, tradeType := range []string{"BUY", "SELL"} {
		for _, w := range windows(from, to, c2cWindow) {
			for page := 1; ; page++ {
				var env sapiEnvelope
				err := p.c.get(ctx, "/sapi/v1/c2c/orderMatch/listUserOrderHistory", url.Values{
					"tradeType":      {tradeType},
					"startTimestamp": {ms(w[0])},
					"endTimestamp":   {ms(w[1])},
					"page":           {strconv.Itoa(page)},
					"rows":           {strconv.Itoa(rows)},
				}, &env)
				if err != nil {
					return out, err
				}
				if err := env.err(); err != nil {
					return out, err
				}
				for _, raw := range env.Data {
					txs, err := c2cRows(raw)
					if err != nil {
						return out, err
					}
					out = append(out, txs...)
				}
				if len(env.Data) < rows {
					break
				}
			}
		}
	}
	return out, nil
}

// c2cRows turns one completed P2P order into two rows that share a
// reference: the crypto leg and the fiat leg (for example USDT and VES).
func c2cRows(raw json.RawMessage) ([]model.Transaction, error) {
	var o c2cOrder
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	if o.OrderStatus != "COMPLETED" {
		return nil, nil
	}
	crypto, err := decimal.NewFromString(o.Amount)
	if err != nil {
		return nil, fmt.Errorf("c2c %s amount: %w", o.OrderNumber, err)
	}
	fiat, err := decimal.NewFromString(o.TotalPrice)
	if err != nil {
		return nil, fmt.Errorf("c2c %s totalPrice: %w", o.OrderNumber, err)
	}

	cryptoDir, fiatDir := model.Credit, model.Debit
	if o.TradeType == "SELL" {
		cryptoDir, fiatDir = model.Debit, model.Credit
	}
	desc := fmt.Sprintf("P2P %s %s %s @ %s %s", o.TradeType, o.Amount, o.Asset, o.UnitPrice, o.Fiat)
	if o.CounterPartNickName != "" {
		desc += " - " + o.CounterPartNickName
	}
	at := time.UnixMilli(o.CreateTime).UTC()

	leg := func(suffix string, dir model.Direction, amount decimal.Decimal, asset string) model.Transaction {
		ext := "c2c:" + o.OrderNumber + ":" + suffix
		return model.Transaction{
			ID:          model.MakeID(Name, ext),
			Source:      Name,
			ExternalID:  ext,
			Account:     "funding",
			Kind:        "c2c",
			Direction:   dir,
			Amount:      amount,
			Asset:       asset,
			Status:      "completed",
			Reference:   o.OrderNumber,
			Description: desc,
			OccurredAt:  at,
			Raw:         string(raw),
		}
	}
	return []model.Transaction{
		leg("crypto", cryptoDir, crypto, o.Asset),
		leg("fiat", fiatDir, fiat, o.Fiat),
	}, nil
}

// Restrictions are the permissions of the API key, from
// /sapi/v1/account/apiRestrictions.
type Restrictions struct {
	IPRestrict                 bool `json:"ipRestrict"`
	EnableReading              bool `json:"enableReading"`
	EnableWithdrawals          bool `json:"enableWithdrawals"`
	EnableInternalTransfer     bool `json:"enableInternalTransfer"`
	PermitsUniversalTransfer   bool `json:"permitsUniversalTransfer"`
	EnableSpotAndMarginTrading bool `json:"enableSpotAndMarginTrading"`
	EnableMargin               bool `json:"enableMargin"`
	EnableFutures              bool `json:"enableFutures"`
	EnableVanillaOptions       bool `json:"enableVanillaOptions"`
}

// CheckKey verifies the credentials and returns the key's permissions.
func (p *Provider) CheckKey(ctx context.Context) (Restrictions, error) {
	var r Restrictions
	if err := p.c.syncTime(ctx); err != nil {
		return r, fmt.Errorf("server time: %w", err)
	}
	err := p.c.get(ctx, "/sapi/v1/account/apiRestrictions", nil, &r)
	return r, err
}
