package bnc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/eddiedibs/banker_hub/internal/model"
	"github.com/eddiedibs/banker_hub/internal/provider"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]string{
		"1.234,56":     "1234.56",
		"-50,00":       "-50",
		"Bs. 1.000,00": "1000",
		"0,5":          "0.5",
	} {
		got, err := parseAmount(in)
		if err != nil || got.String() != want {
			t.Errorf("parseAmount(%q) = %s, %v; want %s", in, got, err, want)
		}
	}
	if _, err := parseAmount("abc"); err == nil {
		t.Error("want error for non-numeric amount")
	}
}

func TestParseAccounts(t *testing.T) {
	accounts, hidden, err := parseAccounts(fixture(t, "last25_page.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].Value != "ACC1" || accounts[0].Number != "01910000000012345678" {
		t.Fatalf("accounts = %+v", accounts)
	}
	if hidden["prv_Hidden"] != "h1" {
		t.Errorf("hidden = %v", hidden)
	}
	if tok, _ := parseToken(fixture(t, "last25_page.html")); tok != "tok-last25" {
		t.Errorf("token = %q", tok)
	}
}

func TestParseTransactions(t *testing.T) {
	acct := account{Value: "ACC1", Number: "01910000000012345678"}
	txs, err := parseTransactions(fixture(t, "last25_list.html"), acct, map[string]int{})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 4 {
		t.Fatalf("got %d rows", len(txs))
	}

	first := txs[0]
	if first.Direction != model.Credit || first.Amount.String() != "1234.56" || first.Asset != "VES" {
		t.Errorf("first = %+v", first)
	}
	if first.Description != "PAGO MOVIL DE JUAN" {
		t.Errorf("description = %q", first.Description)
	}
	wantAt := time.Date(2026, 10, 15, 4, 0, 0, 0, time.UTC) // midnight Caracas
	if !first.OccurredAt.Equal(wantAt) {
		t.Errorf("occurred_at = %s, want %s", first.OccurredAt, wantAt)
	}
	if first.Account != "****5678" {
		t.Errorf("account = %q", first.Account)
	}
	if txs[1].Direction != model.Debit || txs[1].Amount.String() != "50" {
		t.Errorf("second = %+v", txs[1])
	}
	if txs[2].Direction != model.Debit {
		t.Errorf("commission must be a debit: %+v", txs[2])
	}

	// Two identical rows must get different ids.
	if txs[2].ID == txs[3].ID {
		t.Error("identical rows share an id")
	}

	// Parsing again yields the same ids (idempotent ingestion).
	again, _ := parseTransactions(fixture(t, "last25_list.html"), acct, map[string]int{})
	for i := range txs {
		if txs[i].ID != again[i].ID {
			t.Fatal("ids are not deterministic")
		}
	}
}

// fakeBNC simulates the login, Last25 and Custom flows.
type fakeBNC struct {
	t            *testing.T
	page, list   string
	expireOnce   bool
	logins       int
	loggedIn     bool
	listRequests int

	// Custom grid: page form and the list pages served by Page=1, 2.
	customPage   string
	customPages  []string
	customFail   bool // Custom_List answers HTTP 500
	ignorePage   bool // Custom_List always answers page 1
	failPage2    bool // Custom_List answers HTTP 500 for Page=2
	custom500    bool // Custom_List answers AJAX type 500
	customForms  []url.Values
	customHTML   []string // when set, Custom POST serves these raw HTML pages
	loginOnce    bool     // first Custom POST answers the login page
	cycle        bool     // Custom_List cycles through customHTML: p1, p2, p1, ...
	unknownHTML  string   // when set, Custom POST answers this body on page 1
	last25Called bool
}

func (f *fakeBNC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ajax := func(typ int, value string) {
		json.NewEncoder(w).Encode(map[string]any{"Type": typ, "Value": value})
	}
	switch r.URL.Path {
	case "/Auth/LogOut":
		f.loggedIn = false
		w.Write([]byte(`<form id="Frm_Login"></form>`))
	case "/":
		w.Write([]byte(`<input name="__RequestVerificationToken" value="tok0">`))
	case "/Auth/PreLogin_Try":
		r.ParseForm()
		if r.Form.Get("__RequestVerificationToken") != "tok0" || r.Form.Get("CardNumber") != "card" || r.Form.Get("UserID") != "V123" {
			ajax(500, "")
			return
		}
		ajax(200, `<input name="__RequestVerificationToken" value="tok1"><input id="UserPassword">`)
	case "/Auth/Login_Try":
		r.ParseForm()
		if r.Form.Get("__RequestVerificationToken") != "tok1" || r.Form.Get("UserPassword") != "secret" {
			ajax(500, `<span id="LblMessage">Clave inválida</span>`)
			return
		}
		f.logins++
		f.loggedIn = true
		ajax(200, "/Home/BNCNETHB/Welcome")
	case "/Home/BNCNETHB/Welcome":
		if f.loggedIn {
			w.Write([]byte(`<a id="btn-logout">Salir</a> Bienvenido`))
		} else {
			w.Write([]byte(`<form id="Frm_Login"></form>`))
		}
	case "/Accounts/Transactions/Last25":
		w.Write([]byte(f.page))
	case "/Accounts/Transactions/Last25_List":
		f.listRequests++
		f.last25Called = true
		r.ParseForm()
		if f.expireOnce {
			f.expireOnce = false
			ajax(505, "")
			return
		}
		if r.Form.Get("prv_Hidden") != "h1" || r.Form.Get("__RequestVerificationToken") != "tok-last25" {
			f.t.Errorf("list form missing fields: %v", r.Form)
		}
		if r.Form.Get("Account") == "ACC2" {
			ajax(300, "")
			return
		}
		ajax(200, f.list)
	case "/Accounts/Transactions/Custom":
		if r.Method != http.MethodGet {
			f.t.Errorf("unexpected %s to the Custom form page", r.Method)
		}
		w.Write([]byte(f.customPage))
	case "/Accounts/Transactions/Custom_List":
		r.ParseForm()
		if f.customFail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if f.loginOnce {
			f.loginOnce = false
			w.Write([]byte(`<form id="Frm_Login"><input id="UserPassword"></form>`))
			return
		}
		if f.unknownHTML != "" {
			if page, _ := strconv.Atoi(r.Form.Get("Page")); page == 1 {
				w.Write([]byte(f.unknownHTML))
				return
			}
		}
		if len(f.customHTML) > 0 {
			f.customForms = append(f.customForms, r.Form)
			page, _ := strconv.Atoi(r.Form.Get("Page"))
			if f.cycle {
				page = (page-1)%len(f.customHTML) + 1
			}
			if r.Form.Get("Account") == "ACC2" {
				// An empty grid: the table is there, with no rows.
				w.Write([]byte(`<table id="Tbl_Transactions"><tbody></tbody></table>`))
				return
			}
			if page < 1 || page > len(f.customHTML) {
				w.Write([]byte(`<div id="waitTable"></div>`))
				return
			}
			w.Write([]byte(f.customHTML[page-1]))
			return
		}
		if f.expireOnce {
			f.expireOnce = false
			ajax(505, "")
			return
		}
		if f.custom500 {
			ajax(500, "")
			return
		}
		if f.failPage2 && r.Form.Get("Page") == "2" {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if r.Form.Get("prv_Hidden") != "c1" || r.Form.Get("__RequestVerificationToken") != "tok-custom" ||
			r.Form.Get("IdMovementType") != "0" {
			f.t.Errorf("custom form missing fields: %v", r.Form)
		}
		f.customForms = append(f.customForms, r.Form)
		if r.Form.Get("Account") == "ACC2" {
			ajax(300, "")
			return
		}
		page, _ := strconv.Atoi(r.Form.Get("Page"))
		if f.ignorePage {
			page = 1
		}
		if page < 1 || page > len(f.customPages) {
			ajax(300, "")
			return
		}
		ajax(200, f.customPages[page-1])
	default:
		http.NotFound(w, r)
	}
}

func TestFetchFlow(t *testing.T) {
	fake := &fakeBNC{t: t, page: fixture(t, "last25_page.html"), list: fixture(t, "last25_list.html"), expireOnce: true}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := New(srv.URL, Credentials{Card: "card", UserID: "V123", Password: "secret"}, WithDelay(0))
	txs, err := p.Fetch(context.Background(), provider.Range{})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 4 {
		t.Fatalf("got %d transactions, want 4", len(txs))
	}
	if fake.logins != 2 {
		t.Errorf("logins = %d, want 2 (re-login after 505)", fake.logins)
	}
	if fake.loggedIn {
		t.Error("session not logged out after fetch")
	}
	if len(fake.customForms) != 0 {
		t.Error("a zero range must not use the Custom grid")
	}
}

// caracasNoon is "now" for the Custom tests: 2026-10-17 12:00 in Caracas.
var caracasNoon = time.Date(2026, 10, 17, 12, 0, 0, 0, caracas)

func newCustomFake(t *testing.T) (*fakeBNC, *Provider) {
	t.Helper()
	fake := &fakeBNC{
		t: t, page: fixture(t, "last25_page.html"), list: fixture(t, "last25_list.html"),
		customPage:  fixture(t, "custom_page.html"),
		customPages: []string{fixture(t, "custom_list_p1.html"), fixture(t, "custom_list_p2.html")},
	}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	p := New(srv.URL, Credentials{Card: "card", UserID: "V123", Password: "secret"}, WithDelay(0))
	p.now = func() time.Time { return caracasNoon }
	return fake, p
}

func TestFetchCustomPastDays(t *testing.T) {
	fake, p := newCustomFake(t)
	// Ends before today: Custom only.
	txs, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 17, 0, 0, 0, 0, caracas),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 3 {
		t.Fatalf("got %d transactions, want 3 (two pages)", len(txs))
	}
	if fake.last25Called {
		t.Error("Last25 must not be used when the range ends before today")
	}
	// Two accounts, two pages for the first and one empty answer for the second.
	if len(fake.customForms) != 3 {
		t.Fatalf("custom requests = %d, want 3", len(fake.customForms))
	}
	first, second := fake.customForms[0], fake.customForms[1]
	if first.Get("DateBegin") != "01/10/2026" || first.Get("DateEnd") != "16/10/2026" {
		t.Errorf("dates = %s..%s", first.Get("DateBegin"), first.Get("DateEnd"))
	}
	if first.Get("Page") != "1" || first.Get("prv_OldPage") != "0" || first.Get("prv_TotalRows") != "0" {
		t.Errorf("page 1 form = %v", first)
	}
	if second.Get("Page") != "2" || second.Get("prv_OldPage") != "1" || second.Get("prv_TotalRows") != "3" {
		t.Errorf("page 2 form = %v", second)
	}
	if first.Get("Amount") != "" {
		t.Errorf("amount = %q, want empty", first.Get("Amount"))
	}
}

func TestFetchCustomFiltersToRange(t *testing.T) {
	_, p := newCustomFake(t)
	txs, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 14, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 16, 0, 0, 0, 0, caracas),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The 02/10 row is outside the range even though the fake returns it.
	if len(txs) != 2 {
		t.Fatalf("got %d transactions, want 2", len(txs))
	}
}

func TestFetchCustomPlusToday(t *testing.T) {
	fake, p := newCustomFake(t)
	// Zero To means now: past days from Custom, today from Last25.
	txs, err := p.Fetch(context.Background(), provider.Range{From: time.Date(2026, 10, 1, 0, 0, 0, 0, caracas)})
	if err != nil {
		t.Fatal(err)
	}
	// 3 custom + 4 last25, minus the one row both grids return.
	if len(txs) != 6 {
		t.Fatalf("got %d transactions, want 6", len(txs))
	}
	if !fake.last25Called {
		t.Error("range including today must read Last25")
	}
	ids := map[string]bool{}
	for _, tx := range txs {
		if ids[tx.ID] {
			t.Errorf("duplicate id %s", tx.ID)
		}
		ids[tx.ID] = true
	}
}

func TestFetchCustomFallsBackToLast25(t *testing.T) {
	fake, p := newCustomFake(t)
	fake.customFail = true
	txs, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 17, 0, 0, 0, 0, caracas),
	})
	// The Last25 rows are returned, but the Custom failure is reported.
	if err == nil {
		t.Fatal("want the Custom error alongside the Last25 rows")
	}
	if len(txs) != 4 || !fake.last25Called {
		t.Fatalf("got %d transactions (last25 used: %v), want 4 from Last25", len(txs), fake.last25Called)
	}
}

func TestFetchCustomType500IsError(t *testing.T) {
	fake, p := newCustomFake(t)
	fake.custom500 = true
	txs, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 17, 0, 0, 0, 0, caracas),
	})
	if err == nil {
		t.Fatal("type 500 on Custom must not read as an empty result")
	}
	if len(txs) != 4 {
		t.Errorf("got %d transactions, want the 4 Last25 rows", len(txs))
	}
}

func TestFetchCustomKeepsRowsWhenLaterPageFails(t *testing.T) {
	fake, p := newCustomFake(t)
	fake.failPage2 = true
	txs, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 17, 0, 0, 0, 0, caracas),
	})
	if err == nil {
		t.Fatal("want an error for the failed page")
	}
	// Page 1 (2 rows) plus Last25 (4 rows) with one row in common.
	if len(txs) != 5 {
		t.Fatalf("got %d transactions, want 5", len(txs))
	}
}

func TestFetchNoAccountsIsError(t *testing.T) {
	fake, p := newCustomFake(t)
	fake.customPage = `<form id="Frm_Login"></form>`
	_, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 17, 0, 0, 0, 0, caracas),
	})
	if err == nil {
		t.Fatal("want an error when the form has no accounts")
	}
}

func TestFetchCustomReloginOnExpiry(t *testing.T) {
	fake, p := newCustomFake(t)
	fake.expireOnce = true
	txs, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 17, 0, 0, 0, 0, caracas),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 3 || fake.logins != 2 {
		t.Errorf("transactions = %d, logins = %d; want 3 and 2", len(txs), fake.logins)
	}
}

func TestFetchCustomStopsOnRepeatedPage(t *testing.T) {
	fake, p := newCustomFake(t)
	fake.ignorePage = true // server keeps answering page 1
	txs, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 17, 0, 0, 0, 0, caracas),
	})
	// The repeat is dropped, and the shortfall against the announced total
	// of 3 rows is reported.
	if err == nil {
		t.Fatal("want an error for the incomplete result")
	}
	// 2 Custom rows (repeat dropped) plus Last25 (fallback), one row shared.
	if len(txs) != 5 {
		t.Fatalf("got %d transactions, want 5 (repeat must be dropped)", len(txs))
	}
}

func TestFetchEmptyRange(t *testing.T) {
	fake, p := newCustomFake(t)
	txs, err := p.Fetch(context.Background(), provider.Range{
		From: time.Date(2026, 10, 10, 0, 0, 0, 0, caracas),
		To:   time.Date(2026, 10, 9, 0, 0, 0, 0, caracas),
	})
	if err != nil || len(txs) != 0 || fake.logins != 0 {
		t.Errorf("txs=%d err=%v logins=%d; want a no-op", len(txs), err, fake.logins)
	}
}

// A row seen on Last25 and Custom must get the same id, or every sync that
// mixes the grids would duplicate it.
func TestCustomAndLast25IDsMatch(t *testing.T) {
	acct := account{Value: "ACC1", Number: "01910000000012345678"}
	last25, err := parseTransactions(fixture(t, "last25_list.html"), acct, map[string]int{})
	if err != nil {
		t.Fatal(err)
	}
	custom, err := parseTransactions(fixture(t, "custom_list_p1.html"), acct, map[string]int{})
	if err != nil {
		t.Fatal(err)
	}
	if last25[0].ID != custom[0].ID {
		t.Errorf("ids differ: %s vs %s", last25[0].ID, custom[0].ID)
	}
	if n, ok := parseTotalRows(fixture(t, "custom_list_p1.html")); !ok || n != 3 {
		t.Errorf("total rows = %d, %v", n, ok)
	}
}

// Identical rows on different pages of one account keep distinct ids.
func TestSeenSpansPages(t *testing.T) {
	acct := account{Value: "ACC1"}
	seen := map[string]int{}
	a, _ := parseTransactions(fixture(t, "custom_list_p2.html"), acct, seen)
	b, _ := parseTransactions(fixture(t, "custom_list_p2.html"), acct, seen)
	if a[0].ID == b[0].ID {
		t.Error("shared counter must separate identical rows across pages")
	}
}

func TestFetchBadPassword(t *testing.T) {
	fake := &fakeBNC{t: t}
	srv := httptest.NewServer(fake)
	defer srv.Close()

	p := New(srv.URL, Credentials{Card: "card", UserID: "V123", Password: "wrong"}, WithDelay(0))
	if _, err := p.Fetch(context.Background(), provider.Range{}); err == nil {
		t.Fatal("want login error")
	}
}

func TestMaskAccountNeverReturnsLabel(t *testing.T) {
	for _, tc := range []struct {
		acct account
		want string
	}{
		{account{Number: "01910000001234567890"}, "****7890"},
		{account{Label: "Cuenta Ahorro ***0838 - JANE DOE"}, "****0838"},
		{account{Label: "Cuenta Corriente - JANE DOE"}, "****"},
	} {
		if got := maskAccount(tc.acct); got != tc.want {
			t.Errorf("maskAccount(%q) = %q, want %q", tc.acct.Label, got, tc.want)
		}
	}
}

func newGridFake(t *testing.T) (*fakeBNC, *Provider) {
	t.Helper()
	fake, p := newCustomFake(t)
	fake.customHTML = []string{fixture(t, "custom_grid_p1.html"), fixture(t, "custom_grid_p2.html")}
	return fake, p
}

var gridRange = provider.Range{
	From: time.Date(2026, 8, 30, 0, 0, 0, 0, caracas),
	To:   time.Date(2026, 9, 2, 0, 0, 0, 0, caracas),
}

func TestFetchCustomHTMLGrid(t *testing.T) {
	fake, p := newGridFake(t)
	txs, err := p.Fetch(context.Background(), gridRange)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 4 || fake.last25Called {
		t.Fatalf("got %d transactions (last25: %v), want 4 from Custom only", len(txs), fake.last25Called)
	}
	byRef := map[string]model.Transaction{}
	for _, tx := range txs {
		byRef[tx.Reference] = tx
	}
	first := byRef["90000000001"]
	if first.Kind != "Credito Inmediato Recibido" || first.Description != first.Kind {
		t.Errorf("kind/description = %q / %q", first.Kind, first.Description)
	}
	if first.Direction != model.Credit || first.Amount.String() != "51000" {
		t.Errorf("first = %+v", first)
	}
	if want := time.Date(2026, 9, 1, 13, 8, 5, 0, caracas); !first.OccurredAt.Equal(want) {
		t.Errorf("occurred_at = %s, want %s", first.OccurredAt, want)
	}
	debit := byRef["90000000002"]
	if debit.Direction != model.Debit || debit.Amount.String() != "27941.41" {
		t.Errorf("debit = %+v", debit)
	}
	if want := time.Date(2026, 9, 1, 9, 15, 0, 0, caracas); !debit.OccurredAt.Equal(want) {
		t.Errorf("a. m. time = %s, want %s", debit.OccurredAt, want)
	}
	// 12:00:00 a. m. is midnight.
	if want := time.Date(2026, 8, 31, 0, 0, 0, 0, caracas); !byRef["90000000003"].OccurredAt.Equal(want) {
		t.Errorf("midnight = %s", byRef["90000000003"].OccurredAt)
	}
	for _, tx := range txs {
		if strings.Contains(tx.Description, "de agosto") || strings.Contains(tx.Description, "de septiembre") {
			t.Errorf("description taken from a group row: %q", tx.Description)
		}
	}
	if len(fake.customForms) != 3 { // ACC1 pages 1 and 2, ACC2 page 1
		t.Errorf("custom requests = %d, want 3", len(fake.customForms))
	}
}

func TestFetchCustomHTMLReloginOnLoginPage(t *testing.T) {
	fake, p := newGridFake(t)
	fake.loginOnce = true
	txs, err := p.Fetch(context.Background(), gridRange)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 4 || fake.logins != 2 {
		t.Errorf("transactions = %d, logins = %d; want 4 and 2", len(txs), fake.logins)
	}
}

// A Custom grid row and the same Last25 row (no detail text, so the
// description is the type) must share an id.
func TestCustomGridIDMatchesLast25(t *testing.T) {
	acct := account{Value: "ACC1", Number: "01910000000012345678"}
	grid, err := parseTransactions(fixture(t, "custom_grid_p1.html"), acct, map[string]int{})
	if err != nil {
		t.Fatal(err)
	}
	last25, err := parseTransactions(`<table id="Tbl_Transactions"><tbody>
		<tr class="cursor-pointer"><td>01/09/2026</td><td>Credito Inmediato Recibido</td><td>90000000001</td><td>51.000,00</td><td></td></tr>
		</tbody></table>`, acct, map[string]int{})
	if err != nil {
		t.Fatal(err)
	}
	if grid[0].ID != last25[0].ID {
		t.Errorf("ids differ: %s vs %s", grid[0].ID, last25[0].ID)
	}
}

func TestAccountKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    account
		want string
	}{
		{"full number", account{Value: "v", Number: "01910000000012345678"}, "01910000000012345678"},
		{"masked label", account{Value: "0x02ab", Label: "Cuenta Ahorro ***0838 - TITULAR PRUEBA"}, "0838"},
		{"form value last", account{Value: "0x02ab", Label: "Cuenta"}, "0x02ab"},
	} {
		if got := accountKey(tc.a); got != tc.want {
			t.Errorf("%s: accountKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestFetchCustomStopsOnCycledPages(t *testing.T) {
	fake, p := newGridFake(t)
	fake.cycle = true // p1, p2, p1, ...
	// Make page 2 as long as page 1 so paging does not stop on a short page.
	fake.customHTML[1] = strings.ReplaceAll(fake.customHTML[0], "9000000000", "9100000000")
	txs, err := p.Fetch(context.Background(), gridRange)
	if err != nil {
		t.Fatal(err)
	}
	if len(txs) != 6 {
		t.Fatalf("got %d transactions, want 6 (the third page must be dropped)", len(txs))
	}
	if n := len(fake.customForms); n != 4 { // ACC1 pages 1-3, ACC2 page 1
		t.Errorf("custom requests = %d, want 4", n)
	}
}

func TestFetchCustomUnknownHTML(t *testing.T) {
	// First page: an error that does not echo the body.
	fake, p := newGridFake(t)
	fake.unknownHTML = `<html>secret-marker</html>`
	_, err := p.Fetch(context.Background(), gridRange)
	if err == nil {
		t.Fatal("want an error for an unrecognized first page")
	}
	if strings.Contains(err.Error(), "secret-marker") {
		t.Errorf("error leaks the body: %v", err)
	}

	// Later pages: quietly the end of paging.
	rows, _, err := customRows(`<html>nothing here</html>`, account{}, map[string]int{}, false)
	if err != nil || rows != nil {
		t.Errorf("page 2 unknown HTML = %v, %v; want no rows and no error", rows, err)
	}
	for _, login := range []string{`<form id="Frm_Login">`, `<input id="UserPassword">`, `<input name="CardNumber">`} {
		if _, _, err := customRows(login, account{}, map[string]int{}, false); !errors.Is(err, ErrSessionExpired) {
			t.Errorf("%s: err = %v, want ErrSessionExpired", login, err)
		}
	}
}

func TestDateTimeWithNarrowNBSP(t *testing.T) {
	for _, cell := range []string{
		"<td>01/09/2026<br>01:08:05 p.\u202fm.</td>",
		"<td>01/09/2026<br>01:08:05 p.&nbsp;m.</td>",
	} {
		doc, err := goquery.NewDocumentFromReader(strings.NewReader("<table><tr>" + cell + "</tr></table>"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := parseDate(cellText(doc.Find("td")))
		if err != nil {
			t.Fatal(err)
		}
		if want := time.Date(2026, 9, 1, 13, 8, 5, 0, caracas); !got.Equal(want) {
			t.Errorf("%q: got %s, want %s", cell, got, want)
		}
	}
}

func TestParseTransactionsNormalizesSpaces(t *testing.T) {
	acct := account{Value: "ACC1", Number: "01910000000012345678"}
	row := func(typ, ref string) string {
		return `<table id="Tbl_Transactions"><tbody><tr class="cursor-pointer"><td>01/09/2026</td><td>` + typ +
			`</td><td>` + ref + `</td><td>5,00</td></tr></tbody></table>`
	}
	a, _ := parseTransactions(row("Cargo Pago", "123"), acct, map[string]int{})
	b, _ := parseTransactions(row("  Cargo&nbsp;&nbsp;Pago ", " 123\u00a0"), acct, map[string]int{})
	if a[0].ID != b[0].ID {
		t.Error("extra or non-breaking spaces changed the id")
	}
}

func TestAjaxResponsePagedValue(t *testing.T) {
	for _, tc := range []struct {
		body, value string
		total       int
	}{
		{`{"Type":200,"Value":{"Content":"<table id=\"Tbl_Transactions\"></table>","PaginationView":"<ul></ul>","PaginationInfo":{"Page":1,"PageSize":100,"prv_TotalRows":137}},"Message":null}`,
			`<table id="Tbl_Transactions"></table>`, 137},
		{`{"Type":200,"Value":{"Content":"x","PaginationInfo":{"prv_TotalRows":"42"}}}`, "x", 42},
		{`{"Type":200,"Value":"<p>html</p>","Message":"ok"}`, "<p>html</p>", 0},
		{`{"Type":200,"Value":null,"Code":null,"Message":null}`, "", 0},
	} {
		var res ajaxResponse
		if err := json.Unmarshal([]byte(tc.body), &res); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if res.Type != 200 || res.Value != tc.value || res.Total != tc.total {
			t.Errorf("%s: got %+v", tc.body, res)
		}
	}
}

func TestMonthStart(t *testing.T) {
	for _, tc := range []struct {
		now  time.Time
		want time.Time
	}{
		// Still 30 Sep in Caracas (UTC-4), so the month is September.
		{time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC), time.Date(2026, 9, 1, 0, 0, 0, 0, caracas)},
		// 04:00Z is midnight 1 Oct in Caracas.
		{time.Date(2026, 10, 1, 4, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, caracas)},
		{time.Date(2026, 10, 17, 18, 30, 0, 0, time.UTC), time.Date(2026, 10, 1, 0, 0, 0, 0, caracas)},
	} {
		if got := MonthStart(tc.now); !got.Equal(tc.want) {
			t.Errorf("MonthStart(%s) = %s, want %s", tc.now, got, tc.want)
		}
	}
}
