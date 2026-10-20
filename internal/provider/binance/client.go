// Package binance fetches transaction history from the Binance REST API.
package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const (
	recvWindow = "5000"
	maxRetries = 3
	maxBackoff = 60 * time.Second
)

// APIError is a non-2xx response from Binance.
type APIError struct {
	Status int
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("binance: http %d: code %d: %s", e.Status, e.Code, e.Msg)
}

// client performs signed requests. It is safe for sequential use only.
type client struct {
	baseURL   string
	apiKey    string
	apiSecret string
	http      *http.Client
	// offset is server time minus local time, refreshed by syncTime.
	offset time.Duration
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
}

// sign returns the hex HMAC-SHA256 of payload with the API secret.
func sign(secret, payload string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// syncTime measures the clock offset so timestamps pass recvWindow checks.
func (c *client) syncTime(ctx context.Context) error {
	var out struct {
		ServerTime int64 `json:"serverTime"`
	}
	before := c.now()
	if err := c.do(ctx, "/api/v3/time", nil, false, &out); err != nil {
		return err
	}
	rtt := c.now().Sub(before)
	c.offset = time.UnixMilli(out.ServerTime).Sub(before.Add(rtt / 2))
	return nil
}

// get performs a signed GET request and decodes the JSON body into out.
func (c *client) get(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, path, params, true, out)
}

func (c *client) do(ctx context.Context, path string, params url.Values, signed bool, out any) error {
	for attempt := 0; ; attempt++ {
		retryAfter, err := c.once(ctx, path, params, signed, out)
		if err == nil || retryAfter == 0 || attempt >= maxRetries {
			return err
		}
		if err := c.sleep(ctx, retryAfter); err != nil {
			return err
		}
	}
}

// once sends a single request. A non-zero duration means the caller may retry.
func (c *client) once(ctx context.Context, path string, params url.Values, signed bool, out any) (time.Duration, error) {
	q := url.Values{}
	for k, v := range params {
		q[k] = v
	}
	var query string
	if signed {
		q.Set("recvWindow", recvWindow)
		q.Set("timestamp", strconv.FormatInt(c.now().Add(c.offset).UnixMilli(), 10))
		query = q.Encode()
		// The signature covers the exact query string and must come last.
		query += "&signature=" + sign(c.apiSecret, query)
	} else {
		query = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+query, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-MBX-APIKEY", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("binance %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return 0, err
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := &APIError{Status: resp.StatusCode}
		json.Unmarshal(body, apiErr)
		// 429 = rate limited; retry after the given delay. 418 = IP banned
		// for ignoring 429s; the ban can last hours, so fail instead.
		if resp.StatusCode == http.StatusTooManyRequests {
			return retryAfter(resp.Header.Get("Retry-After")), apiErr
		}
		return 0, apiErr
	}
	if err := json.Unmarshal(body, out); err != nil {
		return 0, fmt.Errorf("binance %s: decode: %w", path, err)
	}
	return 0, nil
}

func retryAfter(h string) time.Duration {
	secs, err := strconv.Atoi(h)
	if err != nil || secs <= 0 {
		return 5 * time.Second
	}
	return min(time.Duration(secs)*time.Second, maxBackoff)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
