package bnc

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/shopspring/decimal"

	"github.com/eddiedibs/banker_hub/internal/model"
)

// caracas is Venezuela's offset (UTC-4, no DST). A fixed zone avoids a
// tzdata dependency.
var caracas = time.FixedZone("VET", -4*60*60)

// account is one entry of the account selector (Last25 and Custom share it).
type account struct {
	Value  string // form value posted back with the list request
	Label  string
	Number string // 10-20 digit account number, when the label has one
}

var accountNumberRe = regexp.MustCompile(`\b(\d{10,20})\b`)

// parseToken returns the anti-forgery token embedded in a page or fragment.
func parseToken(html string) (string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", err
	}
	tok, ok := doc.Find(`input[name="__RequestVerificationToken"]`).First().Attr("value")
	if !ok || tok == "" {
		return "", errors.New("bnc: __RequestVerificationToken not found")
	}
	return tok, nil
}

// parseAccounts reads the account selector and the hidden fields of the
// Last25 or Custom form. The hidden fields are posted back with every account query.
func parseAccounts(html string) ([]account, map[string]string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, nil, err
	}
	var accounts []account
	doc.Find(`#Frm_Accounts select[name="Account"], select#Account`).First().Find("option").Each(
		func(_ int, opt *goquery.Selection) {
			value, _ := opt.Attr("value")
			value = strings.TrimSpace(value)
			// "0" or empty is the "-- Seleccione --" placeholder.
			if value == "" || value == "0" {
				return
			}
			a := account{Value: value, Label: strings.TrimSpace(opt.Text())}
			if m := accountNumberRe.FindStringSubmatch(a.Label); m != nil {
				a.Number = m[1]
			}
			accounts = append(accounts, a)
		})

	hidden := map[string]string{}
	doc.Find(`#Frm_Accounts input[type="hidden"]`).Each(func(_ int, in *goquery.Selection) {
		name, _ := in.Attr("name")
		value, _ := in.Attr("value")
		if name != "" {
			hidden[name] = value
		}
	})
	return accounts, hidden, nil
}

// parseTotalRows reads the prv_TotalRows hidden input of a paged response.
func parseTotalRows(html string) (int, bool) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return 0, false
	}
	v, ok := doc.Find(`input[name="prv_TotalRows"]`).First().Attr("value")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	return n, err == nil && n > 0
}

// parseTransactions reads the #Tbl_Transactions grid. Each movement is a
// tr.cursor-pointer row (date, type, reference, amount). The page script
// builds the "Detalle de la Operación" panel from a hidden cell of the same
// row, which Detail reads; it is not part of the id hash.
//
// seen counts identical rows so two equal movements on one day keep
// distinct ids. The count is stable because BNC lists rows in order. Pass
// the same map for every page of one account so the count spans pages.
func parseTransactions(html string, acct account, seen map[string]int) ([]model.Transaction, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, err
	}
	table := doc.Find("#Tbl_Transactions")
	if table.Length() == 0 {
		return nil, errors.New("bnc: #Tbl_Transactions not found")
	}

	accountKey := accountKey(acct)

	var out []model.Transaction
	var parseErr error
	table.Find("tbody tr.cursor-pointer").EachWithBreak(func(_ int, row *goquery.Selection) bool {
		cells := row.Find("td")
		if cells.Length() < 4 {
			return true
		}
		dateStr := cellText(cells.Eq(0))
		// Collapse spaces now: they feed the id hash and must not vary.
		typeStr := collapseSpaces(cells.Eq(1).Text())
		reference := collapseSpaces(cells.Eq(2).Text())
		amountStr := strings.TrimSpace(cells.Eq(3).Text())

		description := detailText(row.Next())
		if description == "" {
			description = typeStr
		}

		date, err := parseDate(dateStr)
		if err != nil {
			parseErr = fmt.Errorf("row %q: %w", reference, err)
			return false
		}
		amount, err := parseAmount(amountStr)
		if err != nil {
			parseErr = fmt.Errorf("row %q: %w", reference, err)
			return false
		}
		dir := direction(amount, typeStr)

		key := model.HashKey(date.Format(time.DateOnly), amount.Abs().String(), reference,
			description, string(dir), accountKey)
		n := seen[key]
		seen[key]++
		externalID := key
		if n > 0 {
			externalID = key + "-" + strconv.Itoa(n)
		}

		rawHTML, _ := goquery.OuterHtml(row)
		out = append(out, model.Transaction{
			ID:          model.MakeID(Name, externalID),
			Source:      Name,
			ExternalID:  externalID,
			Account:     maskAccount(acct),
			Kind:        typeStr,
			Direction:   dir,
			Amount:      amount.Abs(),
			Asset:       "VES",
			Status:      "completed",
			Reference:   reference,
			Description: description,
			Detail:      rowDetail(row),
			OccurredAt:  date.UTC(),
			Raw:         rawHTML,
		})
		return true
	})
	return out, parseErr
}

// cellText joins the text nodes of a cell with spaces, so "a<br>b" reads
// "a b", and turns non-breaking spaces (U+00A0, U+202F) into plain ones.
func cellText(cell *goquery.Selection) string {
	var parts []string
	cell.Contents().Each(func(_ int, n *goquery.Selection) {
		if t := collapseSpaces(n.Text()); t != "" {
			parts = append(parts, t)
		}
	})
	return strings.Join(parts, " ")
}

// accountKey identifies the account inside the id hash. It must not change
// between sessions: the form value can be a per-session encrypted token, so
// it is only the last resort.
func accountKey(a account) string {
	if a.Number != "" {
		return a.Number
	}
	if m := maskedDigitsRe.FindStringSubmatch(a.Label); m != nil {
		return m[1]
	}
	return a.Value
}

// rowDetail reads the "Detalle de la Operación" text from the hidden cell of
// a grid row. It is empty when the row has no such cell.
func rowDetail(row *goquery.Selection) string {
	return collapseSpaces(row.Find("td .font-size-custom").First().Text())
}

func detailText(row *goquery.Selection) string {
	// The Custom grid has no detail rows; the next row may be a date group.
	if row.Length() == 0 || row.HasClass("cursor-pointer") || row.HasClass("dtrg-group") {
		return ""
	}
	for _, sel := range []string{".font-size-custom", ".collapse", "div"} {
		if t := strings.TrimSpace(row.Find(sel).First().Text()); t != "" {
			return collapseSpaces(t)
		}
	}
	return collapseSpaces(strings.TrimSpace(row.Find("td").Text()))
}

var spacesRe = regexp.MustCompile(`\s+`)

// spaceReplacer turns Unicode spaces that \s does not match into plain ones.
var spaceReplacer = strings.NewReplacer("\u00a0", " ", "\u202f", " ")

// collapseSpaces trims s and reduces every run of spaces to one.
func collapseSpaces(s string) string {
	return strings.TrimSpace(spacesRe.ReplaceAllString(spaceReplacer.Replace(s), " "))
}

var dateRe = regexp.MustCompile(`(\d{1,2})[/-](\d{1,2})[/-](\d{2,4})`)

var timeRe = regexp.MustCompile(`(?i)(\d{1,2}):(\d{2})(?::(\d{2}))?\s*(?:([ap])\.?\s*m\.?)?`)

// parseDate reads "DD/MM/YYYY" or "DD-MM-YY" as Caracas time. An optional
// "hh:mm:ss a. m." after the date sets the time of day; without it the
// result is midnight.
func parseDate(s string) (time.Time, error) {
	m := dateRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("bad date %q", s)
	}
	day, _ := strconv.Atoi(m[1])
	month, _ := strconv.Atoi(m[2])
	year, _ := strconv.Atoi(m[3])
	if len(m[3]) == 2 {
		year += 2000
	}
	if month < 1 || month > 12 || day < 1 || day > 31 {
		return time.Time{}, fmt.Errorf("bad date %q", s)
	}
	var hour, min, sec int
	if t := timeRe.FindStringSubmatch(s[strings.Index(s, m[0])+len(m[0]):]); t != nil {
		hour, _ = strconv.Atoi(t[1])
		min, _ = strconv.Atoi(t[2])
		sec, _ = strconv.Atoi(t[3])
		switch strings.ToLower(t[4]) {
		case "a":
			hour %= 12
		case "p":
			hour = hour%12 + 12
		}
		if hour > 23 || min > 59 || sec > 59 {
			return time.Time{}, fmt.Errorf("bad time in %q", s)
		}
	}
	return time.Date(year, time.Month(month), day, hour, min, sec, 0, caracas), nil
}

// parseAmount reads Venezuelan formatted numbers such as "-1.234,56" or
// "Bs. 1.234,56". The result keeps the sign.
func parseAmount(s string) (decimal.Decimal, error) {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == ',':
			b.WriteRune('.')
		}
		// '.' is the thousands separator; drop it with any other symbol.
	}
	d, err := decimal.NewFromString(b.String())
	if err != nil {
		return d, fmt.Errorf("bad amount %q", s)
	}
	return d, nil
}

var (
	debitWords  = []string{"débito", "debito", "cargo", "retiro", "pago", "transferencia enviada", "comisi"}
	creditWords = []string{"crédito", "credito", "abono", "depósito", "deposito", "transferencia recibida"}
)

// direction uses the amount sign first, then keywords of the type column.
func direction(amount decimal.Decimal, typeStr string) model.Direction {
	if amount.IsNegative() {
		return model.Debit
	}
	t := strings.ToLower(typeStr)
	for _, w := range creditWords {
		if strings.Contains(t, w) {
			return model.Credit
		}
	}
	for _, w := range debitWords {
		if strings.Contains(t, w) {
			return model.Debit
		}
	}
	return model.Credit
}

var maskedDigitsRe = regexp.MustCompile(`\*+\s*(\d{4})\b`)

// maskAccount keeps the last 4 digits of the account number. Labels such
// as "Cuenta Ahorro ***0838 - HOLDER NAME" carry only masked digits; the
// label itself is never returned because it holds the holder's name.
func maskAccount(a account) string {
	if len(a.Number) >= 4 {
		return "****" + a.Number[len(a.Number)-4:]
	}
	if m := maskedDigitsRe.FindStringSubmatch(a.Label); m != nil {
		return "****" + m[1]
	}
	return "****"
}
