package spx

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/publichttp"
)

// WikipediaURL is the canonical source the project scrapes for the
// S&P-500 constituent list. Single constant so the release-time script
// and the daemon's runtime refresher land on the same page; changing
// one without the other would silently desync.
const WikipediaURL = "https://en.wikipedia.org/wiki/List_of_S%26P_500_companies"

// HTTPTimeout bounds the Wikipedia fetch. 15 s comfortably covers a
// than help the caller. A failed fetch falls back to whatever's already
const HTTPTimeout = 15 * time.Second

// MinMembers / MaxMembers bound a "looks like the S&P-500" sanity
const (
	MinMembers = 450
	MaxMembers = 520
)

// constituentsTableRE locates the constituents table on the Wikipedia
var constituentsTableRE = regexp.MustCompile(`(?s)<table[^>]*id="constituents"[^>]*>(.*?)</table>`)

// rowRE breaks the table body into <tr>...</tr> blocks. (?s) makes .
var rowRE = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)

// firstCellRE captures the first <td>...</td> of each row. Ticker
var firstCellRE = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)

// linkTextRE captures the visible text of the first <a> tag inside a
// cell. The ticker can be wrapped in either an external NYSE/NASDAQ
// quote link or an internal /wiki/ link; both forms have the symbol
// as the visible link text.
var linkTextRE = regexp.MustCompile(`(?s)<a[^>]*>(.*?)</a>`)

// tickerRE validates the extracted text looks like a ticker: 1-6
var tickerRE = regexp.MustCompile(`^[A-Z][A-Z0-9.\-]{0,5}$`)

// tagRE strips inline HTML tags so a cell like `<b>FOO</b>` collapses
var tagRE = regexp.MustCompile(`<[^>]*>`)

// headerCellRE captures each <th>...</th> of the header row so the GICS
// sector column is located by name rather than by a fixed position.
var headerCellRE = regexp.MustCompile(`(?s)<th[^>]*>(.*?)</th>`)

// ParseHTML extracts the S&P-500 ticker list from a Wikipedia
// runtime refresher) handle a bounds-fail differently (script
func ParseHTML(html []byte) ([]string, error) {
	members, _, err := ParseHTMLWithSectors(html)
	return members, err
}

// ParseHTMLWithSectors extracts the ticker list and, when the table carries a
// "GICS Sector" column, each ticker's GICS sector. A page without that column
// still yields the members with an empty sector map, so the membership path
// never depends on the classification path.
func ParseHTMLWithSectors(html []byte) ([]string, map[string]string, error) {
	tbl := constituentsTableRE.FindSubmatch(html)
	if tbl == nil {
		return nil, nil, fmt.Errorf("constituents table not found (Wikipedia structure may have changed)")
	}
	rows := rowRE.FindAllSubmatch(tbl[1], -1)
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("no rows found inside constituents table")
	}

	sectorCol := -1
	for _, row := range rows {
		heads := headerCellRE.FindAllSubmatch(row[1], -1)
		if len(heads) == 0 {
			continue
		}
		for i, h := range heads {
			if strings.EqualFold(strings.TrimSpace(stripTags(string(h[1]))), "GICS Sector") {
				sectorCol = i
			}
		}
		break
	}

	seen := make(map[string]struct{}, MaxMembers)
	out := make([]string, 0, MaxMembers)
	sectors := map[string]string{}
	for _, row := range rows {
		cells := firstCellRE.FindAllSubmatch(row[1], -1)
		if len(cells) == 0 {
			// Header row (only <th> cells), or malformed — skip.
			continue
		}
		cell := cells[0]
		var ticker string
		link := linkTextRE.FindSubmatch(cell[1])
		if link == nil {
			// Some cells contain plain text rather than a link. Try
			ticker = stripTags(string(cell[1]))
		} else {
			ticker = strings.TrimSpace(stripTags(string(link[1])))
		}
		// Wikipedia uses ASCII hyphens internally — but a class-share
		ticker = strings.ReplaceAll(ticker, " ", "")
		if !tickerRE.MatchString(ticker) {
			continue
		}
		if _, dup := seen[ticker]; dup {
			continue
		}
		seen[ticker] = struct{}{}
		out = append(out, ticker)
		if sectorCol >= 0 && sectorCol < len(cells) {
			if sector := strings.TrimSpace(stripTags(string(cells[sectorCol][1]))); sector != "" {
				sectors[ticker] = sector
			}
		}
	}
	sort.Strings(out)
	return slices.Clip(out), sectors, nil
}

// FetchAndParse pulls the constituent list from url (typically
// WikipediaURL) and parses it. Returns the symbols plus the wall-clock
// time the fetch completed (UTC) — the latter is what callers stamp
// into their on-disk envelope as `as_of`.
//
// Network errors, non-200 responses, and parse failures all surface as
// errors; sanity-bound enforcement (MinMembers ≤ N ≤ MaxMembers) is the
// CALLER's job because release-time and runtime want different
// behaviour on bounds-fail. Request identity follows the shared anonymous
// public-data policy used by both runtime and release-time collection.
func FetchAndParse(ctx context.Context, url string) ([]string, time.Time, error) {
	symbols, _, asOf, err := FetchAndParseWithSectors(ctx, url)
	return symbols, asOf, err
}

// FetchAndParseWithSectors is FetchAndParse plus the GICS sector map the same
// page carries. One round-trip serves both membership and classification.
func FetchAndParseWithSectors(ctx context.Context, url string) ([]string, map[string]string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	publichttp.SetUserAgent(req)
	req.Header.Set("Accept", "text/html")

	client := &http.Client{Timeout: HTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, time.Time{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("read body: %w", err)
	}
	symbols, sectors, err := ParseHTMLWithSectors(body)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return symbols, sectors, time.Now().UTC(), nil
}

func stripTags(s string) string {
	return strings.TrimSpace(tagRE.ReplaceAllString(s, ""))
}
