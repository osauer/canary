// Package macrosource reads a fixed public economic-calendar and official-news
// source set. Parsing is independent of broker state and risk policy.
package macrosource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/osauer/canary/v2/internal/publichttp"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Spec identifies one fixed public source, its parser and its polling policy.
// Refresh is the delay after a successful read before the next one. Freshness
// bounds how long that read is served as current; it exceeds Refresh so one
// late or failed read does not make the source stale.
type Spec struct {
	ID, Name, URL, Kind, Timezone, Coverage string
	Refresh, Freshness                      time.Duration
}

// Specs returns the supported sources. Feed coverage is deliberately explicit.
func Specs() []Spec {
	const refresh, freshness = 5 * time.Minute, 30 * time.Minute
	return []Spec{
		// BLS revises this schedule a few times a year and blocks excessive
		// robot traffic, so it is read hourly instead of on every refresh tick.
		{"bls-calendar", "BLS releases", "https://www.bls.gov/schedule/news_release/bls.ics", "ics", "America/New_York", "Scheduled releases in the supplied calendar; not general news", time.Hour, 3 * time.Hour},
		{"nyfed-calendar", "New York Fed key releases", "https://www.newyorkfed.org/research/calendars/nationalecon_cal.html", "nyfed", "America/New_York", "Published key economic releases in the stated months; independent backup, not full BLS coverage", refresh, freshness},
		{"bea-calendar", "BEA releases", "https://apps.bea.gov/API/signup/release_dates.json", "bea", "America/New_York", "Published BEA release dates; revisions replace earlier dates", refresh, freshness},
		{"fed-calendar", "Federal Reserve calendar", "https://www.federalreserve.gov/json/calendar.json", "fed", "America/New_York", "Published speeches, meetings and statistical releases", refresh, freshness},
		{"ecb-calendar", "ECB weekly calendar", "https://www.ecb.europa.eu/press/calendars/weekly/html/index.en.html", "ecb", "Europe/Berlin", "Current published week; literal source times retained when ambiguous", refresh, freshness},
		{"bea-news", "BEA publications", "https://apps.bea.gov/rss/rss.xml", "rss", "America/New_York", "Recent feed items; not exhaustive news coverage", refresh, freshness},
		{"fed-policy", "Fed monetary policy", "https://www.federalreserve.gov/feeds/press_monetary.xml", "rss", "America/New_York", "Recent monetary-policy releases", refresh, freshness},
		{"fed-speeches", "Fed speeches", "https://www.federalreserve.gov/feeds/speeches_and_testimony.xml", "rss", "America/New_York", "Recent speeches and testimony", refresh, freshness},
		{"ecb-news", "ECB publications", "https://www.ecb.europa.eu/rss/press.html", "rss", "Europe/Berlin", "Recent official ECB publications", refresh, freshness},
	}
}

// parserVersion identifies what Parse produces from given source bytes.
// Increase it with any change that can alter parsed records, so batches parsed
// earlier are read in full once instead of being renewed by a 304.
const parserVersion = 1

// Batch is one successful source response, before daemon retention and
// filtering. ETag and LastModified are that response's cache validators, kept
// verbatim so a later read can ask whether the representation changed. Parser
// records the parser version that produced the records; the validators are
// replayed only while it matches the running parser.
type Batch struct {
	Events       []rpc.MacroEvent       `json:"events"`
	Publications []rpc.MacroPublication `json:"publications"`
	WindowStart  string                 `json:"window_start,omitempty"`
	WindowEnd    string                 `json:"window_end,omitempty"`
	ETag         string                 `json:"etag,omitempty"`
	LastModified string                 `json:"last_modified,omitempty"`
	Parser       int                    `json:"parser,omitempty"`
}

// FetchError is a redacted source failure. Code and Stage use the
// rpc.SourceFailure allowlist. Retryable reports whether repeating the same
// request can succeed without a change by Canary or the publisher. Error
// returns producer-authored text only, never response bodies or transport detail.
type FetchError struct {
	Code, Stage string
	Retryable   bool
	msg         string
}

// Error returns the redacted, producer-authored failure description.
func (e *FetchError) Error() string { return e.msg }

func requestFailure(code string, retryable bool, msg string) error {
	return &FetchError{Code: code, Stage: rpc.SourceFailureStagePublicSourceRequest, Retryable: retryable, msg: msg}
}

// transientPayload rejects a document that a later read of the same URL can
// replace without a format change, such as one cut short in transfer.
func transientPayload(msg string) error {
	return &FetchError{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStagePublicSourceParse, Retryable: true, msg: msg}
}

// typedFailure keeps a classified failure and treats any other error as a
// structural payload rejection: the request path classifies every transport
// and HTTP outcome, and parsers mark their transient rejections, so a plain
// error is a format or validation failure that repeats until the parser or the
// publisher changes.
func typedFailure(err error) error {
	if _, ok := errors.AsType[*FetchError](err); ok {
		return err
	}
	return &FetchError{Code: rpc.SourceFailureInvalidPayload, Stage: rpc.SourceFailureStagePublicSourceParse, Retryable: false, msg: err.Error()}
}

var errRedirectRefused = errors.New("public source redirect refused")

func transportFailure(err error) error {
	if errors.Is(err, errRedirectRefused) {
		return requestFailure(rpc.SourceFailureProtocolRejected, false, errRedirectRefused.Error())
	}
	code := rpc.SourceFailureTransportFailed
	netErr, isNet := errors.AsType[net.Error](err)
	switch _, isDNS := errors.AsType[*net.DNSError](err); {
	case isDNS:
		code = rpc.SourceFailureDNSFailed
	case errors.Is(err, context.DeadlineExceeded), isNet && netErr.Timeout():
		code = rpc.SourceFailureTimeout
	case errors.Is(err, syscall.ECONNREFUSED):
		code = rpc.SourceFailureConnectionRefused
	}
	return requestFailure(code, true, "public source request failed")
}

func statusFailure(status int) error {
	msg := fmt.Sprintf("source returned HTTP %d", status)
	switch {
	case status == http.StatusTooManyRequests:
		return requestFailure(rpc.SourceFailurePacing, true, msg)
	case status >= 500, status == http.StatusRequestTimeout:
		return requestFailure(rpc.SourceFailureProtocolRejected, true, msg)
	case status >= 400:
		// Other client errors repeat until the request or the publisher changes.
		return requestFailure(rpc.SourceFailureProtocolRejected, false, msg)
	default:
		return requestFailure(rpc.SourceFailureProtocolRejected, true, msg)
	}
}

// Client sends no credentials or account information to its fixed public hosts.
type Client struct{ HTTP *http.Client }

// NewClient constructs a bounded transport; redirects must stay on approved hosts.
func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 4 || !SafeURL(req.URL.String()) {
			return errRedirectRefused
		}
		publichttp.SetUserAgent(req)
		return nil
	}}}
}

// SafeURL permits only HTTPS URLs on the explicit official source hosts.
func SafeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	switch strings.ToLower(u.Hostname()) {
	case "www.newyorkfed.org", "www.bls.gov", "www.bea.gov", "apps.bea.gov", "www.federalreserve.gov", "www.ecb.europa.eu":
		return true
	}
	return false
}

// Fetch reads one feed without modifying daemon or broker state. prior is the
// caller's retained batch for s. When it carries cache validators from the
// running parser version the read is conditional, and a 304 Not Modified answer
// returns prior re-stamped as retrieved at now. Fetch never modifies prior.
// Every error is a *FetchError.
func (c *Client) Fetch(ctx context.Context, s Spec, now time.Time, prior Batch) (Batch, error) {
	var batch Batch
	var err error
	if s.Kind == "nyfed" {
		batch, err = c.fetchNYFed(ctx, s, now)
	} else {
		batch, err = c.fetch(ctx, s, now, prior)
	}
	if err != nil {
		return Batch{}, typedFailure(err)
	}
	return batch, nil
}
func (c *Client) fetch(ctx context.Context, s Spec, now time.Time, prior Batch) (Batch, error) {
	res, err := c.read(ctx, s.URL, prior)
	if err != nil {
		return Batch{}, err
	}
	if res.notModified {
		return revalidated(s, prior, now)
	}
	batch, err := Parse(s, res.body, now)
	if err != nil {
		return Batch{}, err
	}
	batch.ETag, batch.LastModified = res.etag, res.lastModified
	return batch, nil
}

// revalidated renews prior after the publisher confirmed it unchanged. Record
// identities exclude the retrieval clock, so re-stamping keeps them stable.
func revalidated(s Spec, prior Batch, now time.Time) (Batch, error) {
	out := prior
	out.Events = slices.Clone(prior.Events)
	for i := range out.Events {
		out.Events[i].RetrievedAt = now
	}
	out.Publications = slices.Clone(prior.Publications)
	for i := range out.Publications {
		out.Publications[i].RetrievedAt = now
	}
	if err := ValidateBatch(s, out, now); err != nil {
		return Batch{}, err
	}
	return out, nil
}

type response struct {
	body               []byte
	etag, lastModified string
	notModified        bool
}

func (c *Client) read(ctx context.Context, rawURL string, prior Batch) (response, error) {
	if !SafeURL(rawURL) {
		return response{}, requestFailure(rpc.SourceFailureProtocolRejected, false, "public source host refused")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return response{}, requestFailure(rpc.SourceFailureProtocolRejected, false, "public source request invalid")
	}
	publichttp.SetUserAgent(req)
	req.Header.Set("Accept", "text/calendar, application/rss+xml, application/json, application/xml, text/xml, text/html")
	// A 304 renews parsed records, not source bytes, so records from another
	// parser version must be re-read in full.
	conditional := prior.Parser == parserVersion && (prior.ETag != "" || prior.LastModified != "")
	if conditional && prior.ETag != "" {
		req.Header.Set("If-None-Match", prior.ETag)
	}
	if conditional && prior.LastModified != "" {
		req.Header.Set("If-Modified-Since", prior.LastModified)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return response{}, transportFailure(err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotModified && conditional {
		return response{notModified: true}, nil
	}
	if res.StatusCode != 200 {
		// A 403 alone does not identify its cause. Only the witnessed BLS
		// policy page earns this actionable diagnosis; never expose its body.
		if res.StatusCode == http.StatusForbidden && res.Request != nil && res.Request.URL != nil && res.Request.URL.Hostname() == "www.bls.gov" {
			body, readErr := io.ReadAll(io.LimitReader(res.Body, (8<<10)+1))
			text := html.UnescapeString(strings.Join(strings.Fields(string(body)), " "))
			if readErr == nil && len(body) <= 8<<10 && strings.Contains(text, "Bureau of Labor Statistics") && strings.Contains(text, "Access Denied") && strings.Contains(text, "bot activity that doesn't conform to BLS usage policy is prohibited.") {
				return response{}, requestFailure(rpc.SourceFailureProtocolRejected, false, "source returned HTTP 403: BLS rejected this request under its automated-access policy")
			}
		}
		return response{}, statusFailure(res.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(res.Body, (2<<20)+1))
	if err != nil {
		return response{}, requestFailure(rpc.SourceFailureTransportFailed, true, "public source read failed")
	}
	if len(b) > 2<<20 {
		return response{}, requestFailure(rpc.SourceFailureInvalidPayload, false, "public source exceeds size limit")
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return response{}, requestFailure(rpc.SourceFailureInvalidPayload, true, "source returned an empty response")
	}
	out := response{body: b, etag: res.Header.Get("ETag"), lastModified: res.Header.Get("Last-Modified")}
	// A malformed validator only costs a full read later; it never fails the body.
	if !validETag(out.etag) {
		out.etag = ""
	}
	if !validLastModified(out.lastModified) {
		out.lastModified = ""
	}
	return out, nil
}

// validETag accepts only an RFC 9110 entity tag, so a restored validator cannot
// carry header syntax into a later request.
func validETag(v string) bool {
	v = strings.TrimPrefix(v, "W/")
	if len(v) < 2 || len(v) > 256 || v[0] != '"' || v[len(v)-1] != '"' {
		return false
	}
	for _, c := range []byte(v[1 : len(v)-1]) {
		if c < 0x21 || c == '"' || c > 0x7e {
			return false
		}
	}
	return true
}

func validLastModified(v string) bool {
	_, err := http.ParseTime(v)
	return err == nil && len(v) <= 64
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

func sourceLink(s Spec, raw string) bool {
	if !SafeURL(raw) {
		return false
	}
	link, _ := url.Parse(raw)
	feed, _ := url.Parse(s.URL)
	if link.Hostname() == feed.Hostname() {
		return true
	}
	return (feed.Hostname() == "apps.bea.gov" || feed.Hostname() == "www.bea.gov") && (link.Hostname() == "apps.bea.gov" || link.Hostname() == "www.bea.gov")
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
	if err != nil || !SafeURL(s.URL) || s.ID == "" || now.IsZero() || s.Refresh <= 0 || s.Freshness <= s.Refresh {
		return errors.New("invalid public source specification")
	}
	if s.Kind == "nyfed" && (batch.ETag != "" || batch.LastModified != "") || batch.ETag != "" && !validETag(batch.ETag) || batch.LastModified != "" && !validLastModified(batch.LastModified) {
		return errors.New("public source cache validator invalid")
	}
	if len(batch.Events)+len(batch.Publications) == 0 || len(batch.Events) > 10000 || len(batch.Publications) > 2000 {
		return errors.New("public source record count invalid")
	}
	if s.Kind == "rss" && len(batch.Events) != 0 || s.Kind != "rss" && len(batch.Publications) != 0 {
		return errors.New("public source record kind invalid")
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
// previously retained records. It never fetches links carried inside a feed.
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
	out.Parser = parserVersion
	if err := ValidateBatch(s, out, now); err != nil {
		return Batch{}, err
	}
	return out, nil
}
