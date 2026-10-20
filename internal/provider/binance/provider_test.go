package binance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
)

// Example from the Binance API documentation (SIGNED endpoint security).
func TestSignMatchesBinanceExample(t *testing.T) {
	secret := "NhqPtmdSJYdKjVHjA7PZj4Mge3R5YNiP1e3UZjInClVN65XAbvqqM6A7H5fATj0j"
	query := "symbol=LTCBTC&side=BUY&type=LIMIT&timeInForce=GTC&quantity=1&price=0.1&recvWindow=5000&timestamp=1499827319559"
	want := "c8db56825ae71d6d79447849e617115f4a920fa2acdcab2b053c4b2838bd6b71"
	if got := sign(secret, query); got != want {
		t.Fatalf("sign = %s, want %s", got, want)
	}
}

func TestC2CRowsSplitLegs(t *testing.T) {
	raw := json.RawMessage(`{"orderNumber":"2001","tradeType":"BUY","asset":"USDT","fiat":"VES",
		"amount":"50.00","totalPrice":"9750.00","unitPrice":"195.00","orderStatus":"COMPLETED",
		"createTime":1760000000000,"counterPartNickName":"seller1"}`)
	rows, err := c2cRows(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	crypto, fiat := rows[0], rows[1]
	if crypto.Asset != "USDT" || crypto.Direction != model.Credit || crypto.Amount.String() != "50" {
		t.Errorf("crypto leg = %+v", crypto)
	}
	if fiat.Asset != "VES" || fiat.Direction != model.Debit || fiat.Amount.String() != "9750" {
		t.Errorf("fiat leg = %+v", fiat)
	}
	if crypto.ID == fiat.ID || crypto.Reference != fiat.Reference {
		t.Error("legs must have distinct ids and a shared reference")
	}

	cancelled := json.RawMessage(`{"orderNumber":"2002","orderStatus":"CANCELLED"}`)
	if rows, _ := c2cRows(cancelled); len(rows) != 0 {
		t.Error("cancelled order must be skipped")
	}
}

func TestFetchAgainstFakeServer(t *testing.T) {
	const key, secret = "test-key", "test-secret"
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/time" {
			json.NewEncoder(w).Encode(map[string]int64{"serverTime": now.UnixMilli()})
			return
		}
		if r.Header.Get("X-MBX-APIKEY") != key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		// Verify the signature over everything before "&signature=".
		payload, sig, ok := strings.Cut(r.URL.RawQuery, "&signature=")
		if !ok || sign(secret, payload) != sig {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"code":-1022,"msg":"Signature for this request is not valid."}`))
			return
		}
		q, _ := url.ParseQuery(payload)
		switch r.URL.Path {
		case "/sapi/v1/capital/deposit/hisrec":
			if q.Get("offset") != "0" {
				w.Write([]byte(`[]`))
				return
			}
			w.Write([]byte(`[
				{"id":"d1","amount":"25.5","coin":"USDT","network":"TRX","status":1,"txId":"0xabc","insertTime":1760600000000,"walletType":0},
				{"id":"d2","amount":"1","coin":"USDT","network":"TRX","status":0,"txId":"0xdef","insertTime":1760600000000,"walletType":0}]`))
		case "/sapi/v1/capital/withdraw/history":
			w.Write([]byte(`[{"id":"w1","amount":"10","transactionFee":"1","coin":"USDT","network":"BSC","status":6,"txId":"0x1","applyTime":"2026-10-15 08:00:00","walletType":1}]`))
		case "/sapi/v1/pay/transactions":
			w.Write([]byte(`{"code":"000000","message":"success","data":[
				{"orderType":"C2C","transactionId":"p1","transactionTime":1760600000000,"amount":"-5.25","currency":"USDT","receiverInfo":{"name":"Ana"}}]}`))
		case "/sapi/v1/c2c/orderMatch/listUserOrderHistory":
			w.Write([]byte(`{"code":"000000","data":[],"total":0}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	p := New(srv.URL, key, secret)
	p.c.now = func() time.Time { return now }

	txs, err := p.Fetch(context.Background(), provider.Range{From: now.Add(-100 * 24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	// Deposits come once per 90-day window (2 windows), but storage dedups
	// by external id; here only check kinds and filtering.
	kinds := map[string]int{}
	for _, tx := range txs {
		kinds[tx.Kind]++
		if tx.ExternalID == "deposit:d2" {
			t.Error("pending deposit must be skipped")
		}
	}
	if kinds["deposit"] == 0 || kinds["withdrawal"] == 0 || kinds["pay"] == 0 {
		t.Fatalf("kinds = %v", kinds)
	}
	for _, tx := range txs {
		if tx.Kind == "pay" && (tx.Direction != model.Debit || tx.Amount.String() != "5.25") {
			t.Errorf("pay tx = %+v", tx)
		}
	}
}

func TestRetryOn429(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	p := New(srv.URL, "k", "s")
	var slept time.Duration
	p.c.sleep = func(_ context.Context, d time.Duration) error { slept += d; return nil }
	var out []json.RawMessage
	if err := p.c.get(context.Background(), "/x", nil, &out); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || slept != time.Second {
		t.Errorf("calls=%d slept=%v", calls, slept)
	}
}

func TestFetchRespectsRange(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	var starts, ends []int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/time" {
			json.NewEncoder(w).Encode(map[string]int64{"serverTime": now.UnixMilli()})
			return
		}
		if r.URL.Path == "/sapi/v1/capital/deposit/hisrec" {
			q := r.URL.Query()
			s, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
			e, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
			starts, ends = append(starts, s), append(ends, e)
		}
		w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	p := New(srv.URL, "k", "s")
	p.c.now = func() time.Time { return now }

	for _, tc := range []struct {
		name    string
		r       provider.Range
		wantEnd time.Time
	}{
		{"to clamps until", provider.Range{From: from, To: to}, to},
		{"zero to means now", provider.Range{From: from}, now},
		{"future to is capped at now", provider.Range{From: from, To: now.Add(time.Hour)}, now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			starts, ends = nil, nil
			p.Fetch(context.Background(), tc.r)
			if len(starts) == 0 {
				t.Fatal("no deposit request")
			}
			if starts[0] != from.UnixMilli() {
				t.Errorf("first startTime = %d, want %d", starts[0], from.UnixMilli())
			}
			if got := ends[len(ends)-1]; got != tc.wantEnd.UnixMilli() {
				t.Errorf("last endTime = %d, want %d", got, tc.wantEnd.UnixMilli())
			}
		})
	}
}
