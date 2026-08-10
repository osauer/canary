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
)

// WikipediaURL is the canonical source the project scrapes for the
// S&P-500 constituent list. Single constant so the release-time script
// and the daemon's runtime refresher land on the same page; changing
// one without the other would silently desync.
const WikipediaURL = "https://en.wikipedia.org/wiki/List_of_S%26P_500_companies"

// UserAgent identifies our scraper to Wikipedia ops. Their bot policy
func UserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return fmt.Sprintf("canary/%s (https://github.com/osauer/canary; +breadth indicator)", version)
}

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

// ParseHTML extracts the S&P-500 ticker list from a Wikipedia
// runtime refresher) handle a bounds-fail differently (script
func ParseHTML(html []byte) ([]string, error) {
	tbl := constituentsTableRE.FindSubmatch(html)
	if tbl == nil {
		return nil, fmt.Errorf("constituents table not found (Wikipedia structure may have changed)")
	}
	rows := rowRE.FindAllSubmatch(tbl[1], -1)
	if len(rows) == 0 {
		return nil, fmt.Errorf("no rows found inside constituents table")
	}

	seen := make(map[string]struct{}, MaxMembers)
	out := make([]string, 0, MaxMembers)
	for _, row := range rows {
		cell := firstCellRE.FindSubmatch(row[1])
		if cell == nil {
			// Header row (only <th> cells), or malformed — skip.
			continue
		}
		link := linkTextRE.FindSubmatch(cell[1])
		if link == nil {
			// Some cells contain plain text rather than a link. Try
			text := stripTags(string(cell[1]))
			if tickerRE.MatchString(text) {
				if _, dup := seen[text]; !dup {
					seen[text] = struct{}{}
					out = append(out, text)
				}
			}
			continue
		}
		ticker := strings.TrimSpace(stripTags(string(link[1])))
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
	}
	sort.Strings(out)
	return slices.Clip(out), nil
}

// FetchAndParse pulls the constituent list from url (typically
// WikipediaURL) and parses it. Returns the symbols plus the wall-clock
// time the fetch completed (UTC) — the latter is what callers stamp
// into their on-disk envelope as `as_of`.
//
// Network errors, non-200 responses, and parse failures all surface as
// errors; sanity-bound enforcement (MinMembers ≤ N ≤ MaxMembers) is the
// CALLER's job because release-time and runtime want different
// behaviour on bounds-fail. The version argument is folded into the
// User-Agent so Wikipedia ops can correlate scrapes to releases.
func FetchAndParse(ctx context.Context, url, version string) ([]string, time.Time, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("User-Agent", UserAgent(version))
	req.Header.Set("Accept", "text/html")

	client := &http.Client{Timeout: HTTPTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("read body: %w", err)
	}
	symbols, err := ParseHTML(body)
	if err != nil {
		return nil, time.Time{}, err
	}
	return symbols, time.Now().UTC(), nil
}

func stripTags(s string) string {
	return strings.TrimSpace(tagRE.ReplaceAllString(s, ""))
}
