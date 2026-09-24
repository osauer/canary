// Package macrosource reads a fixed public economic-calendar and official-news
// source set. Parsing is independent of broker state and risk policy.
package macrosource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/osauer/canary/v2/internal/publichttp"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Spec identifies one fixed public source and its parser.
type Spec struct{ ID, Name, URL, Kind, Timezone, Coverage string }

// Specs returns the supported sources. Feed coverage is deliberately explicit.
func Specs() []Spec {
	return []Spec{
		{"bls-calendar", "BLS releases", "https://www.bls.gov/schedule/news_release/bls.ics", "ics", "America/New_York", "Scheduled releases in the supplied calendar; not general news"},
		{"nyfed-calendar", "New York Fed key releases", "https://www.newyorkfed.org/research/calendars/nationalecon_cal.html", "nyfed", "America/New_York", "Published key economic releases in the stated months; independent backup, not full BLS coverage"},
		{"bea-calendar", "BEA releases", "https://apps.bea.gov/API/signup/release_dates.json", "bea", "America/New_York", "Published BEA release dates; revisions replace earlier dates"},
		{"fed-calendar", "Federal Reserve calendar", "https://www.federalreserve.gov/json/calendar.json", "fed", "America/New_York", "Published speeches, meetings and statistical releases"},
		{"ecb-calendar", "ECB weekly calendar", "https://www.ecb.europa.eu/press/calendars/weekly/html/index.en.html", "ecb", "Europe/Berlin", "Current published week; literal source times retained when ambiguous"},
		{"bea-news", "BEA publications", "https://apps.bea.gov/rss/rss.xml", "rss", "America/New_York", "Recent feed items; not exhaustive news coverage"},
		{"fed-policy", "Fed monetary policy", "https://www.federalreserve.gov/feeds/press_monetary.xml", "rss", "America/New_York", "Recent monetary-policy releases"},
		{"fed-speeches", "Fed speeches", "https://www.federalreserve.gov/feeds/speeches_and_testimony.xml", "rss", "America/New_York", "Recent speeches and testimony"},
		{"ecb-news", "ECB publications", "https://www.ecb.europa.eu/rss/press.html", "rss", "Europe/Berlin", "Recent official ECB publications"},
	}
}

// Batch is one successful source response, before daemon retention and filtering.
type Batch struct {
	// SkippedItems counts publication-feed items omitted because their title,
	// source link or publication date was invalid. Calendars never skip, and a
	// feed with no valid item fails instead of producing a batch.
	SkippedItems int                    `json:"skipped_items,omitempty"`
	Events       []rpc.MacroEvent       `json:"events"`
	Publications []rpc.MacroPublication `json:"publications"`
	WindowStart  string                 `json:"window_start,omitempty"`
	WindowEnd    string                 `json:"window_end,omitempty"`
}

// Client sends no credentials or account information to its fixed public hosts.
type Client struct{ HTTP *http.Client }

// NewClient constructs a bounded transport; redirects must stay on approved hosts.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || !SafeURL(req.URL.String()) {
			return errors.New("public source redirect refused")
		}
		publichttp.SetUserAgent(req)
		return nil
	}}}
}

// SafeURL permits only HTTPS URLs on the explicit official source hosts.
func SafeURL(raw string) bool {
	u, ok := httpsURL(raw)
	if !ok {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "www.newyorkfed.org", "www.bls.gov", "www.bea.gov", "apps.bea.gov", "www.federalreserve.gov", "www.ecb.europa.eu":
		return true
	}
	return false
}

// Fetch reads one feed without modifying daemon or broker state.
func (c *Client) Fetch(ctx context.Context, s Spec, now time.Time) (Batch, error) {
	if s.Kind == "nyfed" {
		return c.fetchNYFed(ctx, s, now)
	}
	return c.fetch(ctx, s, now)
}
func (c *Client) fetch(ctx context.Context, s Spec, now time.Time) (Batch, error) {
	raw, err := c.read(ctx, s.URL)
	if err != nil {
		return Batch{}, err
	}
	return Parse(s, raw, now)
}
func (c *Client) read(ctx context.Context, rawURL string) ([]byte, error) {
	if !SafeURL(rawURL) {
		return nil, errors.New("public source host refused")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	publichttp.SetUserAgent(req)
	req.Header.Set("Accept", "text/calendar, application/rss+xml, application/json, application/xml, text/xml, text/html")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, errors.New("public source request failed")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		// A 403 alone does not identify its cause. Only the witnessed BLS
		// policy page earns this actionable diagnosis; never expose its body.
		if res.StatusCode == http.StatusForbidden && res.Request != nil && res.Request.URL != nil && res.Request.URL.Hostname() == "www.bls.gov" {
			body, readErr := io.ReadAll(io.LimitReader(res.Body, (8<<10)+1))
			text := html.UnescapeString(strings.Join(strings.Fields(string(body)), " "))
			if readErr == nil && len(body) <= 8<<10 && strings.Contains(text, "Bureau of Labor Statistics") && strings.Contains(text, "Access Denied") && strings.Contains(text, "bot activity that doesn't conform to BLS usage policy is prohibited.") {
				return nil, errors.New("source returned HTTP 403: BLS rejected this request under its automated-access policy")
			}
		}
		return nil, fmt.Errorf("source returned HTTP %d", res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil {
		return nil, errors.New("public source read failed")
	}
	if len(b) > 2<<20 {
		return nil, errors.New("public source exceeds size limit")
	}
	return b, nil
}
func identity(values ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return hex.EncodeToString(sum[:12])
}
func tidy(v string) string {
	v = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, v)
	v = strings.Join(strings.Fields(v), " ")
	runes := []rune(v)
	if len(runes) > 500 {
		v = string(runes[:500])
	}
	return v
}

// httpsURL parses raw when it is HTTPS without user information or an explicit port.
func httpsURL(raw string) (*url.URL, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return nil, false
	}
	return u, true
}

// sourceLink accepts an item link on its feed's own host or, for BEA feeds, on
// any official BEA host. Canary records these links as published and never
// fetches them, so BEA's apex host is accepted here without joining SafeURL's
// fetch allowlist.
func sourceLink(s Spec, raw string) bool {
	link, ok := httpsURL(raw)
	feed, err := url.Parse(s.URL)
	if !ok || err != nil {
		return false
	}
	if link.Hostname() == feed.Hostname() {
		return true
	}
	return beaHost(feed.Hostname()) && beaHost(link.Hostname())
}

// beaHost reports whether host is one of BEA's official publication hosts.
func beaHost(host string) bool {
	return host == "apps.bea.gov" || host == "www.bea.gov" || host == "bea.gov"
}

func eventID(s Spec, e rpc.MacroEvent) string {
	return identity(s.ID, e.Title, e.Date, e.TimeLabel, e.ScheduledAt.Format(time.RFC3339Nano))
}
func publicationID(s Spec, p rpc.MacroPublication) string {
	return identity(s.ID, p.SourceURL, p.Title, p.PublishedAt.Format(time.RFC3339Nano))
}

// ValidateBatch checks source identity, clocks and date precision before a
// parsed or restored feed can replace retained public evidence.
func ValidateBatch(s Spec, batch Batch, now time.Time) error {
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil || !SafeURL(s.URL) || s.ID == "" || now.IsZero() {
		return errors.New("invalid public source specification")
	}
	if len(batch.Events)+len(batch.Publications) == 0 || len(batch.Events) > 10000 || len(batch.Publications) > 2000 {
		return errors.New("public source record count invalid")
	}
	if s.Kind == "rss" && len(batch.Events) != 0 || s.Kind != "rss" && len(batch.Publications) != 0 {
		return errors.New("public source record kind invalid")
	}
	if batch.SkippedItems < 0 || s.Kind != "rss" && batch.SkippedItems != 0 {
		return errors.New("public source skipped-item count invalid")
	}
	if s.Kind == "nyfed" {
		start, startErr := time.Parse(time.DateOnly, batch.WindowStart)
		end, endErr := time.Parse(time.DateOnly, batch.WindowEnd)
		if startErr != nil || endErr != nil || start.Day() != 1 || (!end.Equal(start.AddDate(0, 1, -1)) && !end.Equal(start.AddDate(0, 2, -1))) {
			return errors.New("calendar coverage interval invalid")
		}
	} else if batch.WindowStart != "" || batch.WindowEnd != "" {
		return errors.New("unexpected calendar coverage interval")
	}
	seen := map[string]bool{}
	validText := func(v string) bool { return utf8.ValidString(v) && v == tidy(v) && len([]rune(v)) <= 500 }
	for _, e := range batch.Events {
		if e.SourceID != s.ID || !calendarSourceURL(s, e.SourceURL) || e.Timezone != s.Timezone || e.Title == "" || !validText(e.Title) || !validText(e.Category) || !validText(e.TimeLabel) || e.RetrievedAt.IsZero() || e.RetrievedAt.After(now.Add(time.Minute)) {
			return errors.New("calendar provenance invalid")
		}
		if _, err := time.Parse(time.DateOnly, e.Date); err != nil {
			return errors.New("calendar date invalid")
		}
		if s.Kind == "nyfed" && (e.Date < batch.WindowStart || e.Date > batch.WindowEnd) {
			return errors.New("calendar event outside published coverage")
		}
		if s.Kind == "nyfed" {
			u, _ := url.Parse(e.SourceURL)
			d, _ := time.Parse(time.DateOnly, e.Date)
			if nyfedMonthPath.MatchString(u.Path) && u.Path != "/research/calendars/i-"+strings.ToLower(d.Format("Jan06"))+".html" {
				return errors.New("calendar event source month disagrees")
			}
		}
		switch e.TimePrecision {
		case "instant":
			if e.ScheduledAt.IsZero() || e.Date != e.ScheduledAt.In(loc).Format(time.DateOnly) {
				return errors.New("calendar instant and source date disagree")
			}
		case "date":
			if !e.ScheduledAt.IsZero() || e.TimeLabel != "" {
				return errors.New("calendar date precision invalid")
			}
		case "source_label":
			if !e.ScheduledAt.IsZero() || e.TimeLabel == "" {
				return errors.New("calendar source time invalid")
			}
		default:
			return errors.New("calendar time precision invalid")
		}
		if e.ID != eventID(s, e) || seen[e.ID] {
			return errors.New("calendar record identity invalid")
		}
		seen[e.ID] = true
	}
	for _, p := range batch.Publications {
		if p.SourceID != s.ID || !sourceLink(s, p.SourceURL) || p.Title == "" || !validText(p.Title) || p.RetrievedAt.IsZero() || p.RetrievedAt.After(now.Add(time.Minute)) || p.PublishedAt.After(p.RetrievedAt.Add(time.Minute)) {
			return errors.New("publication provenance invalid")
		}
		if p.ID != publicationID(s, p) || seen[p.ID] {
			return errors.New("publication record identity invalid")
		}
		seen[p.ID] = true
	}
	return nil
}

// Parse preserves source dates and rejects malformed feeds instead of clearing
// previously retained records. A publication-feed item without a title, an
// official source link or a readable publication date is skipped and counted
// in Batch.SkippedItems; the feed fails when no valid item remains. Parse never
// fetches links carried inside a feed.
func Parse(s Spec, b []byte, now time.Time) (Batch, error) {
	var out Batch
	var err error
	switch s.Kind {
	case "nyfed":
		out, err = parseNYFed(s, string(b), now)
	case "ics":
		out, err = parseICS(s, string(b), now)
	case "bea":
		out, err = parseBEA(s, b, now)
	case "fed":
		out, err = parseFed(s, b, now)
	case "ecb":
		out, err = parseECB(s, b, now)
	case "rss":
		out, err = parseRSS(s, b, now)
	default:
		err = errors.New("unknown public source format")
	}
	if err != nil {
		return Batch{}, err
	}
	if len(out.Events)+len(out.Publications) == 0 {
		return Batch{}, errors.New("source supplied no usable records")
	}
	loc, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return Batch{}, errors.New("public source timezone invalid")
	}
	events := make([]rpc.MacroEvent, 0, len(out.Events))
	seen := map[string]bool{}
	for _, e := range out.Events {
		e.SourceID = s.ID
		e.SourceURL = s.URL
		e.Timezone = s.Timezone
		e.RetrievedAt = now
		e.Title = plain(e.Title)
		e.Category = plain(e.Category)
		e.TimeLabel = plain(e.TimeLabel)
		if !e.ScheduledAt.IsZero() {
			e.Date = e.ScheduledAt.In(loc).Format(time.DateOnly)
		}
		if e.TimePrecision == "date" && e.TimeLabel != "" {
			e.TimePrecision = "source_label"
		}
		e.ID = eventID(s, e)
		if !seen[e.ID] {
			events = append(events, e)
			seen[e.ID] = true
		}
	}
	publications := make([]rpc.MacroPublication, 0, len(out.Publications))
	for _, p := range out.Publications {
		p.SourceID = s.ID
		p.RetrievedAt = now
		p.Title = plain(p.Title)
		p.ID = publicationID(s, p)
		if !seen[p.ID] {
			publications = append(publications, p)
			seen[p.ID] = true
		}
	}
	out.Events, out.Publications = events, publications
	if err := ValidateBatch(s, out, now); err != nil {
		return Batch{}, err
	}
	return out, nil
}
