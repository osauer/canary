package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/publichttp"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
	"golang.org/x/net/html"
)

const germanBillFactsheetBase = "https://www.deutsche-finanzagentur.de/bundeswertpapiere/factsheet/isin/"
const germanBillMaxBody = 1 << 20

// germanBill is issuer evidence only. It never fills broker-reported identity,
// currency or order units. A caller must independently bind the exact ISIN to
// the broker contract before using its maturity for an order or holding.
type germanBill struct {
	ISIN                               string
	IssueDate, MaturityDate, FetchedAt time.Time
}

var germanBillHTTPClient = &http.Client{
	Timeout:       5 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// fetchGermanBill reads one exact official factsheet, without credentials,
// redirects, persistent cache or stale fallback. Dates are checked at receipt.
func fetchGermanBill(ctx context.Context, isin string) (germanBill, error) {
	return fetchGermanBillWith(ctx, germanBillHTTPClient, isin, time.Now)
}

func fetchGermanBillWith(ctx context.Context, client *http.Client, isin string, now func() time.Time) (germanBill, error) {
	if !strings.HasPrefix(isin, "DE") || !ibkrlib.ValidISIN(isin) {
		return germanBill{}, errors.New("german bill source requires an exact valid German ISIN")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, germanBillFactsheetBase+isin, nil)
	if err != nil {
		return germanBill{}, errors.New("german bill factsheet request is invalid")
	}
	publichttp.SetUserAgent(req)
	resp, err := client.Do(req)
	if err != nil {
		return germanBill{}, errors.New("german bill factsheet is unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return germanBill{}, fmt.Errorf("german bill factsheet returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, germanBillMaxBody+1))
	if err != nil || len(body) > germanBillMaxBody {
		return germanBill{}, errors.New("german bill factsheet is unreadable or too large")
	}
	return parseGermanBillFactsheet(string(body), isin, now().UTC())
}

func parseGermanBillFactsheet(body, isin string, received time.Time) (germanBill, error) {
	if len(body) > germanBillMaxBody || received.IsZero() || !strings.HasPrefix(isin, "DE") || !ibkrlib.ValidISIN(isin) {
		return germanBill{}, errors.New("german bill factsheet input is invalid")
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return germanBill{}, errors.New("german bill factsheet HTML is invalid")
	}
	fields := map[string][]string{}
	var walk func(*html.Node, int) error
	walk = func(n *html.Node, depth int) error {
		if depth > 128 {
			return errors.New("german bill factsheet nesting is excessive")
		}
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style" || n.Data == "template") {
			return nil
		}
		if n.Type == html.ElementNode && n.Data == "div" && germanBillHTMLClass(n, "bb-text-with-label") {
			var label, value []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if germanBillHTMLClass(c, "bb-text-with-label__label") {
					label = append(label, germanBillHTMLText(c))
				}
				if germanBillHTMLClass(c, "bb-text-with-label__text") {
					value = append(value, germanBillHTMLText(c))
				}
			}
			for _, key := range label {
				if key != "ISIN" && key != "Art" {
					continue
				}
				text := ""
				if len(label) == 1 && len(value) == 1 {
					text = value[0]
				}
				fields[key] = append(fields[key], text)
			}
		}
		if n.Type == html.ElementNode && n.Data == "tr" {
			var label, value []string
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if c.Type == html.ElementNode && c.Data == "th" {
					key := germanBillHTMLText(c)
					switch key {
					case "Emittent", "Emissionswährung", "Emissionsdatum", "Fälligkeit":
						if germanBillHTMLAttribute(c, "scope") == "row" {
							label = append(label, key)
						}
					}
				}
				if c.Type == html.ElementNode && c.Data == "td" {
					value = append(value, germanBillHTMLText(c))
				}
			}
			for _, key := range label {
				text := ""
				if len(label) == 1 && len(value) == 1 {
					text = value[0]
				}
				fields[key] = append(fields[key], text)
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if err := walk(c, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc, 0); err != nil {
		return germanBill{}, err
	}
	get := func(key string) string {
		if len(fields[key]) == 1 {
			return fields[key][0]
		}
		return ""
	}
	if get("ISIN") != isin || get("Emittent") != "Bundesrepublik Deutschland" || get("Emissionswährung") != "€" || get("Art") != "Unverzinsliche Schatzanweisung 12 Monate" {
		return germanBill{}, errors.New("german bill factsheet lacks unique exact ISIN, sovereign issuer, EUR or Bubill type")
	}
	issue, errIssue := time.Parse("02.01.2006", get("Emissionsdatum"))
	maturity, errMaturity := time.Parse("02.01.2006", get("Fälligkeit"))
	day := received.UTC().Truncate(24 * time.Hour)
	if errIssue != nil || errMaturity != nil || issue.After(day) || !maturity.After(day) || !maturity.After(issue) {
		return germanBill{}, errors.New("german bill factsheet does not identify an issued outstanding bill")
	}
	return germanBill{ISIN: isin, IssueDate: issue, MaturityDate: maturity, FetchedAt: received.UTC()}, nil
}

func germanBillHTMLClass(n *html.Node, class string) bool {
	if n.Type != html.ElementNode {
		return false
	}
	for _, a := range n.Attr {
		if a.Key == "class" {
			return slices.Contains(strings.Fields(a.Val), class)
		}
	}
	return false
}

func germanBillHTMLText(n *html.Node) string {
	var text strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style" || n.Data == "template") {
			return
		}
		if n.Type == html.TextNode {
			text.WriteString(n.Data)
			text.WriteByte(' ')
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(text.String()), " ")
}

// Only row-scoped factsheet metadata is issuer evidence. Auction table column
// headers (including Art) carry no security identity or maturity authority.
func germanBillHTMLAttribute(n *html.Node, key string) string {
	var values []string
	for _, a := range n.Attr {
		if a.Key == key {
			values = append(values, a.Val)
		}
	}
	if len(values) == 1 {
		return values[0]
	}
	return ""
}
