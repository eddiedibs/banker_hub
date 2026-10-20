// Package bnc scrapes Banco Nacional de Crédito (Venezuela) online banking.
//
// BNC has no public API. This is a Go port of the pure-HTTP flow used by
// github.com/danicanod/Banker: cookie session, anti-forgery token, a
// two-step login, then the movement grids for each account.
//
// Past days come from the paged "Custom" date-range grid (Custom_List,
// 100 rows per page). The Custom date picker stops at yesterday, so a range
// that includes today also reads the "last 25 movements" grid. Without a
// range only the last 25 rows exist, so run it often.
package bnc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
)

// Name is the source identifier for BNC rows.
const Name = "bnc"

const userAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

const (
	pathLogin        = "/"
	pathPreLogin     = "/Auth/PreLogin_Try"
	pathLoginTry     = "/Auth/Login_Try"
	pathLogout       = "/Auth/LogOut"
	pathWelcome      = "/Home/BNCNETHB/Welcome"
	pathLast25       = "/Accounts/Transactions/Last25"
	pathLast25List   = "/Accounts/Transactions/Last25_List"
	maxResponseBytes = 8 << 20
)

// pathCustom serves the date-range form. The page script (Custom.js, List())
// posts the serialized #Frm_Accounts to pathCustomList, which answers with
// the {Type, Value} envelope; Value holds the paged grid object.
const (
	pathCustom     = "/Accounts/Transactions/Custom"
	pathCustomList = "/Accounts/Transactions/Custom_List"
)

const (
	customPageSize = 100
	// maxCustomPages bounds the paging loop in case the server never ends it.
	maxCustomPages = 200
	// customDateFormat is the d/m/Y format of the DateBegin/DateEnd fields.
	customDateFormat = "02/01/2006"
)

// customMinDay is the earliest day the Custom date picker accepts.
var customMinDay = time.Date(2013, 1, 1, 0, 0, 0, 0, caracas)

// BNC wraps AJAX results as {"Type": code, "Value": html}.
const (
	typeSuccess        = 200
	typeSessionExpired = 505
)

var (
	// ErrSessionExpired is returned when BNC ends the session mid-scrape.
	ErrSessionExpired = errors.New("bnc: session expired")
	// ErrLogin is returned when BNC rejects the credentials or the session.
	ErrLogin = errors.New("bnc: login failed")
)

type ajaxResponse struct {
	Type    int
	Value   string // HTML; for paged grids, the Content field
	Message string
	// Total is PaginationInfo.prv_TotalRows of a paged grid, else 0.
	Total int
}

// UnmarshalJSON accepts Value as null, an HTML string, or the paged grid
// object {Content, PaginationView, PaginationInfo{Page, PageSize, prv_TotalRows}}.
func (r *ajaxResponse) UnmarshalJSON(b []byte) error {
	var raw struct {
		Type    int             `json:"Type"`
		Value   json.RawMessage `json:"Value"`
		Message *string         `json:"Message"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*r = ajaxResponse{Type: raw.Type}
	if raw.Message != nil {
		r.Message = *raw.Message
	}
	v := bytes.TrimSpace(raw.Value)
	switch {
	case len(v) == 0 || string(v) == "null":
	case v[0] == '{':
		var paged struct {
			Content        string `json:"Content"`
			PaginationInfo struct {
				TotalRows json.Number `json:"prv_TotalRows"`
			} `json:"PaginationInfo"`
		}
		d := json.NewDecoder(bytes.NewReader(v))
		d.UseNumber()
		if err := d.Decode(&paged); err != nil {
			return err
		}
		r.Value = paged.Content
		if n, err := paged.PaginationInfo.TotalRows.Int64(); err == nil {
			r.Total = int(n)
		}
	default:
		return json.Unmarshal(v, &r.Value)
	}
	return nil
}

// Credentials are the three values BNC asks for at login.
type Credentials struct {
	Card     string // card number
	UserID   string // cédula
	Password string
}

// Provider fetches BNC movements. Each Fetch opens and closes its own session.
type Provider struct {
	baseURL string
	creds   Credentials
	timeout time.Duration
	// delay is the pause between requests, to look less like a bot.
	delay time.Duration
	now   func() time.Time
	// debugDir, when set, receives every response body. Local use only:
	// the files hold account data.
	debugDir string
}

// Option customizes a Provider.
type Option func(*Provider)

// WithDelay sets the pause between requests (default 500ms).
func WithDelay(d time.Duration) Option { return func(p *Provider) { p.delay = d } }

// WithDebugDir writes every response body into dir (mode 0600) to help
// adapt the parser to the live site. The files hold account data.
func WithDebugDir(dir string) Option { return func(p *Provider) { p.debugDir = dir } }

// New returns a BNC provider.
func New(baseURL string, creds Credentials, opts ...Option) *Provider {
	p := &Provider{
		baseURL: strings.TrimRight(baseURL, "/"),
		creds:   creds,
		timeout: 30 * time.Second,
		delay:   500 * time.Millisecond,
		now:     time.Now,
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

func (*Provider) Name() string { return Name }

// fetchPlan says which grids a range needs. Days are Caracas midnights.
type fetchPlan struct {
	from, to time.Time // first and last day wanted; zero from means unbounded
	custom   bool      // range covers past days
	last25   bool      // range covers today, or no range was given
	customTo time.Time // last day to ask Custom for (yesterday at most)
}

func newFetchPlan(r provider.Range, now time.Time) fetchPlan {
	today := caracasDay(now)
	end := r.To
	if end.IsZero() || end.After(now) {
		end = now
	}
	// To is exclusive: a range ending exactly at midnight stops the day before.
	pl := fetchPlan{to: caracasDay(end.Add(-time.Nanosecond))}
	if r.From.IsZero() {
		pl.last25 = true
		return pl
	}
	pl.from = caracasDay(r.From)
	pl.last25 = !pl.to.Before(today)
	pl.customTo = pl.to
	if !pl.customTo.Before(today) {
		pl.customTo = today.AddDate(0, 0, -1)
	}
	pl.custom = !pl.from.After(pl.customTo)
	return pl
}

// MonthStart returns the first day of now's month at 00:00 Caracas time.
func MonthStart(now time.Time) time.Time {
	y, m, _ := now.In(caracas).Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, caracas)
}

// caracasDay truncates t to midnight Caracas time.
func caracasDay(t time.Time) time.Time {
	y, m, d := t.In(caracas).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, caracas)
}

// inRange reports whether a row falls on a wanted day. BNC dates have day
// granularity, so the comparison is by Caracas date.
func (pl fetchPlan) inRange(t time.Time) bool {
	d := caracasDay(t)
	return (pl.from.IsZero() || !d.Before(pl.from)) && !d.After(pl.to)
}

// Fetch logs in, reads the movements of every account inside r and logs out.
// Past days use the Custom grid; today (or a zero r.From) uses Last25.
// Rows are filtered to r by Caracas date and deduplicated by id.
func (p *Provider) Fetch(ctx context.Context, r provider.Range) ([]model.Transaction, error) {
	pl := newFetchPlan(r, p.now())
	if !pl.custom && !pl.last25 {
		return nil, nil // empty range
	}

	s, err := p.newSession()
	if err != nil {
		return nil, err
	}
	if err := s.login(ctx); err != nil {
		return nil, err
	}
	// Always end the session: BNC rejects new logins while one is open.
	defer s.logout(context.WithoutCancel(ctx))

	// forms caches the page form of each grid; a re-login clears it.
	forms := map[string]pageForm{}
	loadForm := func(path, referer string) ([]account, pageForm, error) {
		accts, f, err := s.accounts(ctx, path, referer)
		if err == nil {
			forms[path] = f
		}
		return accts, f, err
	}
	formFor := func(path, referer string) (pageForm, error) {
		if f, ok := forms[path]; ok {
			return f, nil
		}
		_, f, err := loadForm(path, referer)
		return f, err
	}

	var accounts []account
	if pl.custom {
		accounts, _, err = loadForm(pathCustom, pathWelcome)
	} else {
		accounts, _, err = loadForm(pathLast25, pathWelcome)
	}
	if err != nil {
		return nil, err
	}
	if len(accounts) == 0 {
		// Usually an expired session that served the login page.
		return nil, errors.New("bnc: no accounts in the form (session may have expired)")
	}

	scrape := func(acct account) ([]model.Transaction, error) {
		var out []model.Transaction
		var customErr error
		last25 := pl.last25
		if pl.custom {
			f, err := formFor(pathCustom, pathWelcome)
			var txs []model.Transaction
			if err == nil {
				txs, err = s.customTransactions(ctx, acct, f, pl)
			}
			switch {
			case err == nil:
				out = txs
			case errors.Is(err, ErrSessionExpired), ctx.Err() != nil:
				return nil, err
			default:
				// Keep the last 25 rows (and any pages already read) flowing, but
				// report the failure so the sync does not look clean.
				slog.Warn("bnc custom grid failed, using last 25", "account", maskAccount(acct), "err", err)
				out, customErr = txs, err
				last25 = true
			}
		}
		if last25 {
			f, err := formFor(pathLast25, pathWelcome)
			if err != nil {
				return out, errors.Join(customErr, err)
			}
			txs, err := s.transactions(ctx, acct, f)
			out = append(out, txs...)
			if err != nil {
				return out, errors.Join(customErr, err)
			}
		}
		return out, customErr
	}

	var all []model.Transaction
	var errs []error
	seenIDs := map[string]bool{}
	for _, acct := range accounts {
		txs, err := scrape(acct)
		if errors.Is(err, ErrSessionExpired) {
			// Re-login once, reload the forms for fresh tokens, and retry.
			if err = s.login(ctx); err == nil {
				clear(forms)
				txs, err = scrape(acct)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("account %s: %w", maskAccount(acct), err))
		}
		// Keep rows read before a failure; storage deduplicates reruns.
		for _, tx := range txs {
			if pl.inRange(tx.OccurredAt) && !seenIDs[tx.ID] {
				seenIDs[tx.ID] = true
				all = append(all, tx)
			}
		}
		if err != nil && ctx.Err() != nil {
			break
		}
	}
	return all, errors.Join(errs...)
}

// CheckLogin logs in and out once to verify the credentials.
// It ends any BNC session open in a browser.
func (p *Provider) CheckLogin(ctx context.Context) error {
	s, err := p.newSession()
	if err != nil {
		return err
	}
	if err := s.login(ctx); err != nil {
		return err
	}
	s.logout(context.WithoutCancel(ctx))
	return nil
}

// session is one logged-in BNC browser-like session.
type session struct {
	p     *Provider
	http  *http.Client
	token string
	// started and dumps name the debug files of this session.
	started time.Time
	dumps   int
}

func (p *Provider) newSession() (*session, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	return &session{p: p, http: &http.Client{Jar: jar, Timeout: p.timeout}, started: p.now()}, nil
}

// login runs: logout (clears a stale session), load login page for the
// token, PreLogin (card + id), Login (password), and verifies the welcome page.
func (s *session) login(ctx context.Context) error {
	s.logout(ctx)

	page, err := s.get(ctx, pathLogin, "")
	if err != nil {
		return err
	}
	if s.token, err = parseToken(page); err != nil {
		return err
	}

	pre, err := s.postAJAX(ctx, pathPreLogin, pathLogin, url.Values{
		"__RequestVerificationToken": {s.token},
		"prv_LoginType":              {"NATURAL"},
		"prv_InnerLoginType":         {"1"},
		"CardNumber":                 {s.p.creds.Card},
		"UserID":                     {s.p.creds.UserID},
	})
	if err != nil {
		return fmt.Errorf("prelogin: %w", err)
	}
	if pre.Type != typeSuccess {
		return fmt.Errorf("%w: prelogin type %d: %s", ErrLogin, pre.Type, pre.Message)
	}
	// The password form comes with a fresh token.
	if tok, err := parseToken(pre.Value); err == nil {
		s.token = tok
	}

	res, err := s.postAJAX(ctx, pathLoginTry, pathLogin, url.Values{
		"__RequestVerificationToken": {s.token},
		"prv_InnerLoginType":         {"1"},
		"UserPassword":               {s.p.creds.Password},
	})
	if err != nil {
		return fmt.Errorf("login: %w", err)
	}
	if res.Type != typeSuccess {
		msg := res.Message
		if strings.Contains(res.Value, "sesión previa activa") {
			msg = "another session is already active"
		}
		return fmt.Errorf("%w: login type %d: %s", ErrLogin, res.Type, msg)
	}

	welcome, err := s.get(ctx, pathWelcome, pathLogin)
	if err != nil {
		return err
	}
	if !strings.Contains(welcome, "btn-logout") && !strings.Contains(welcome, "Bienvenido") {
		return fmt.Errorf("%w: welcome page not reached", ErrLogin)
	}
	return nil
}

func (s *session) logout(ctx context.Context) {
	// Best effort: errors here do not matter.
	s.get(ctx, pathLogout, pathWelcome)
}

// pageForm is what a grid page gives back: the hidden fields and the
// anti-forgery token to post with each account query.
type pageForm struct {
	hidden map[string]string
	token  string
}

// accounts loads a grid page (Last25 or Custom) and reads its form.
func (s *session) accounts(ctx context.Context, path, referer string) ([]account, pageForm, error) {
	page, err := s.get(ctx, path, referer)
	if err != nil {
		return nil, pageForm{}, err
	}
	accts, hidden, err := parseAccounts(page)
	if err != nil {
		return nil, pageForm{}, err
	}
	tok, err := parseToken(page)
	if err != nil {
		// Keep the login token, as before.
		tok = s.token
	}
	return accts, pageForm{hidden: hidden, token: tok}, nil
}

func (f pageForm) values(acct account) url.Values {
	form := url.Values{}
	for k, v := range f.hidden {
		form.Set(k, v)
	}
	form.Set("__RequestVerificationToken", f.token)
	form.Set("Account", acct.Value)
	return form
}

// listRows turns an AJAX list response into rows. Codes that mean "nothing
// to list" give no rows and no error. Type 500 counts as empty on Last25
// only; on Custom it is an error.
func listRows(res ajaxResponse, acct account, seen map[string]int, last25 bool) ([]model.Transaction, error) {
	switch res.Type {
	case typeSuccess:
		return parseTransactions(res.Value, acct, seen)
	case typeSessionExpired:
		return nil, ErrSessionExpired
	case 300, 350:
		return nil, nil // no movements
	case 500:
		if last25 {
			return nil, nil
		}
		return nil, fmt.Errorf("bnc: unexpected type %d: %s", res.Type, res.Message)
	default:
		return nil, fmt.Errorf("bnc: unexpected type %d: %s", res.Type, res.Message)
	}
}

func (s *session) transactions(ctx context.Context, acct account, f pageForm) ([]model.Transaction, error) {
	res, err := s.postAJAX(ctx, pathLast25List, pathLast25, f.values(acct))
	if err != nil {
		return nil, err
	}
	return listRows(res, acct, map[string]int{}, true)
}

// customTransactions reads the Custom grid for pl.from..pl.customTo, page by
// page. The identical-row counter spans all pages of the account.
func (s *session) customTransactions(ctx context.Context, acct account, f pageForm, pl fetchPlan) ([]model.Transaction, error) {
	begin := pl.from
	if begin.Before(customMinDay) {
		begin = customMinDay
	}
	form := f.values(acct)
	form.Set("IdMovementType", "0") // all movement types
	form.Set("Amount", "")
	form.Set("DateBegin", begin.Format(customDateFormat))
	form.Set("DateEnd", pl.customTo.Format(customDateFormat))
	form.Set("PageSize", strconv.Itoa(customPageSize))

	var all []model.Transaction
	seen := map[string]int{}
	seenRaw := map[string]bool{} // row HTML of all earlier pages
	var firstLen, total int
	for page := 1; page <= maxCustomPages; page++ {
		form.Set("Page", strconv.Itoa(page))
		form.Set("prv_OldPage", strconv.Itoa(page-1))
		form.Set("prv_TotalRows", strconv.Itoa(total))

		// Errors after page 1 return the rows already read.
		body, err := s.postForm(ctx, pathCustomList, pathCustom, form)
		if err != nil {
			return all, err
		}
		// Parse into a copy: a repeated page must not advance the counter.
		trial := maps.Clone(seen)
		txs, pageTotal, err := customRows(body, acct, trial, page == 1)
		if err != nil {
			return all, err
		}
		if len(txs) == 0 {
			break
		}
		// A row from an earlier page means the server clamped or cycled
		// Page and is repeating itself: drop the page and stop.
		if repeatsPage(txs, seenRaw) {
			break
		}
		for _, tx := range txs {
			seenRaw[tx.Raw] = true
		}
		seen = trial
		all = append(all, txs...)

		if pageTotal > 0 {
			total = pageTotal
			if len(all) >= total {
				break
			}
		}
		// The server may ignore PageSize, so the first page sets the size.
		if page == 1 {
			firstLen = len(txs)
		}
		if len(txs) < firstLen {
			break
		}
	}
	// The server announced more rows than it served (page cap, repeated or
	// empty page): return what we have, but flag it.
	if total > 0 && len(all) < total {
		return all, fmt.Errorf("bnc: custom grid returned %d of %d rows", len(all), total)
	}
	return all, nil
}

// repeatsPage reports whether any row of txs was already read on an
// earlier page. Rows are compared by their source HTML.
func repeatsPage(txs []model.Transaction, seenRaw map[string]bool) bool {
	for _, tx := range txs {
		if seenRaw[tx.Raw] {
			return true
		}
	}
	return false
}

func (s *session) get(ctx context.Context, path, referer string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.p.baseURL+path, nil)
	if err != nil {
		return "", err
	}
	s.headers(req, referer)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	return s.do(req)
}

// postForm posts form as an AJAX request and returns the raw body.
func (s *session) postForm(ctx context.Context, path, referer string, form url.Values) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.p.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	s.headers(req, referer)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	return s.do(req)
}

func (s *session) postAJAX(ctx context.Context, path, referer string, form url.Values) (ajaxResponse, error) {
	body, err := s.postForm(ctx, path, referer, form)
	if err != nil {
		return ajaxResponse{}, err
	}
	return decodeAJAX(path, body)
}

func decodeAJAX(path, body string) (ajaxResponse, error) {
	var res ajaxResponse
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		// Do not include the body: it can hold account data.
		return res, fmt.Errorf("bnc %s: response is not JSON (%d bytes)", path, len(body))
	}
	return res, nil
}

// customRows reads a Custom POST body: the HTML grid, or an AJAX envelope
// (kept for error and "no movements" answers). It also returns the total
// row count when the page announces one.
//
// A body that is neither is an error on the first page (the form or the
// session is not what we expect) and just the end of paging after that.
func customRows(body string, acct account, seen map[string]int, first bool) ([]model.Transaction, int, error) {
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		res, err := decodeAJAX(pathCustomList, body)
		if err != nil {
			return nil, 0, err
		}
		txs, err := listRows(res, acct, seen, false)
		total := res.Total
		if total == 0 {
			total, _ = parseTotalRows(res.Value)
		}
		return txs, total, err
	}
	switch {
	case strings.Contains(body, "Tbl_Transactions"):
		txs, err := parseTransactions(body, acct, seen)
		total, _ := parseTotalRows(body)
		return txs, total, err
	case strings.Contains(body, "Frm_Login"), strings.Contains(body, "UserPassword"),
		strings.Contains(body, "CardNumber"):
		return nil, 0, ErrSessionExpired // the login page
	case first:
		// Do not include the body: it can hold account data.
		return nil, 0, fmt.Errorf("bnc %s: unrecognized response (%d bytes)", pathCustomList, len(body))
	default:
		return nil, 0, nil
	}
}

func (s *session) headers(req *http.Request, referer string) {
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept-Language", "es-VE,es;q=0.9")
	if referer != "" {
		req.Header.Set("Referer", s.p.baseURL+referer)
	}
}

func (s *session) do(req *http.Request) (string, error) {
	if s.p.delay > 0 {
		t := time.NewTimer(s.p.delay)
		select {
		case <-req.Context().Done():
			t.Stop()
			return "", req.Context().Err()
		case <-t.C:
		}
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("bnc %s: %w", req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	s.dump(req, resp.StatusCode, body)
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("bnc %s: http %d", req.URL.Path, resp.StatusCode)
	}
	return string(body), nil
}

// dump saves one response body when debugDir is set. Request bodies are
// never written: login posts carry the password.
func (s *session) dump(req *http.Request, status int, body []byte) {
	if s.p.debugDir == "" {
		return
	}
	if err := os.MkdirAll(s.p.debugDir, 0o700); err != nil {
		slog.Warn("bnc debug dump", "err", err)
		return
	}
	s.dumps++
	name := fmt.Sprintf("%s_%03d_%s_%s_%d.html", s.started.Format("20060102T150405"), s.dumps,
		req.Method, strings.ReplaceAll(strings.Trim(req.URL.Path, "/"), "/", "_"), status)
	if err := os.WriteFile(filepath.Join(s.p.debugDir, name), body, 0o600); err != nil {
		slog.Warn("bnc debug dump", "err", err)
	}
}
