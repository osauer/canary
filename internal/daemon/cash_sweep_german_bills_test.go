package daemon

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func syntheticGermanBillFactsheet() string {
	return `<html><body><div class="bb-text-with-label"><div class="bb-text-with-label__label">ISIN</div><div class="bb-text-with-label__text">` + synthDEBill + `</div></div>
<div class="bb-text-with-label"><div class="bb-text-with-label__label">Art</div><div class="bb-text-with-label__text">Unverzinsliche Schatzanweisung 12 Monate</div></div>
<table><tr><th scope="row">Emittent</th><td>Bundesrepublik Deutschland</td></tr>
<tr><th scope="row">Emissionswährung</th><td>&euro;</td></tr>
<tr><th scope="row">Emissionsdatum</th><td>01.06.2026</td></tr>
<tr><th scope="row">Fälligkeit</th><td>15.01.2027</td></tr></table></body></html>`
}

func TestGermanBillFactsheetStrictEvidence(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	body := syntheticGermanBillFactsheet()
	// Auction history tables reuse "Art" as a column heading. Those headers
	// are not the labelled factsheet security type and must not pollute it.
	body = strings.ReplaceAll(body, "</body>", `<table><tr><th>Datum</th><th>Art</th><th>Rendite</th></tr><tr><td>01.06.2026</td><td>Neuemission</td><td>2%</td></tr></table></body>`)
	bill, err := parseGermanBillFactsheet(body, synthDEBill, now)
	if err != nil || bill.ISIN != synthDEBill || bill.MaturityDate.Format(time.DateOnly) != "2027-01-15" || !bill.FetchedAt.Equal(now) {
		t.Fatalf("bill = %+v, err = %v", bill, err)
	}
	for _, tc := range []struct{ name, old, replacement string }{
		{"different ISIN", synthDEBill, synthDEBill2},
		{"other sovereign", "Bundesrepublik Deutschland", "Other Sovereign"},
		{"foreign currency", "&euro;", "USD"},
		{"coupon bond", "Unverzinsliche Schatzanweisung 12 Monate", "Bundesanleihe"},
		{"future issue", "01.06.2026", "02.10.2026"},
		{"matured", "15.01.2027", "01.10.2026"},
		{"invalid date", "15.01.2027", "31.02.2027"},
		{"unscoped date", `<th scope="row">Fälligkeit`, `<th>Fälligkeit`},
		{"duplicate maturity", `</table>`, `<tr><th scope="row">Fälligkeit</th><td>15.01.2027</td></tr></table>`},
		{"contradictory maturity", `</table>`, `<tr><th scope="row">Fälligkeit</th><td>20.02.2027</td></tr></table>`},
		{"malformed repeated maturity", `</table>`, `<tr><th scope="row">Fälligkeit</th><td>15.01.2027</td><td>20.02.2027</td></tr></table>`},
		{"duplicate identity", `</body>`, `<div class="bb-text-with-label"><div class="bb-text-with-label__label">ISIN</div><div class="bb-text-with-label__text">` + synthDEBill + `</div></div></body>`},
		{"script identity", `<div class="bb-text-with-label"><div class="bb-text-with-label__label">ISIN</div><div class="bb-text-with-label__text">` + synthDEBill + `</div></div>`, `<script><div class="bb-text-with-label"><div class="bb-text-with-label__label">ISIN</div><div class="bb-text-with-label__text">` + synthDEBill + `</div></div></script>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseGermanBillFactsheet(strings.ReplaceAll(body, tc.old, tc.replacement), synthDEBill, now); err == nil {
				t.Fatal("accepted contradictory or absent issuer evidence")
			}
		})
	}
	if _, err := parseGermanBillFactsheet(strings.Repeat(" ", germanBillMaxBody+1), synthDEBill, now); err == nil {
		t.Fatal("accepted oversized body")
	}
	if _, err := parseGermanBillFactsheet(body, synthDEBill, time.Time{}); err == nil {
		t.Fatal("accepted missing receipt timestamp")
	}
}

type germanBillRoundTripper func(*http.Request) (*http.Response, error)

func (f germanBillRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGermanBillFetchBoundedReadOnlySource(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"success", http.StatusOK, syntheticGermanBillFactsheet(), false},
		{"redirect", http.StatusFound, syntheticGermanBillFactsheet(), true},
		{"source failure", http.StatusServiceUnavailable, syntheticGermanBillFactsheet(), true},
		{"oversize", http.StatusOK, strings.Repeat("x", germanBillMaxBody+1), true},
		{"empty", http.StatusOK, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: germanBillRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.String() != germanBillFactsheetBase+synthDEBill {
					t.Fatal("request escaped exact public issuer factsheet")
				}
				for _, h := range []string{"Cookie", "Authorization", "Referer"} {
					if r.Header.Get(h) != "" {
						t.Fatal("private request identity leaked")
					}
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})}
			bill, err := fetchGermanBillWith(context.Background(), client, synthDEBill, func() time.Time { return now })
			if (err != nil) != tc.wantError || calls != 1 {
				t.Fatalf("bill=%+v err=%v calls=%d", bill, err, calls)
			}
			if tc.wantError && !bill.FetchedAt.IsZero() {
				t.Fatal("failure returned reusable evidence")
			}
		})
	}
}
