package macrosource

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func sourceSpec(t *testing.T, id string) Spec {
	t.Helper()
	for _, s := range Specs() {
		if s.ID == id {
			return s
		}
	}
	t.Fatal("unknown synthetic fixture source")
	return Spec{}
}

func TestRSSPublicationTimeAndSourceProvenance(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ id, date, link, want string }{
		{"bea-news", "Thu, 03 Sep 2026 08:30:00 EDT", "https://www.bea.gov/news/synthetic", "2026-09-03T12:30:00Z"},
		{"bea-news", "Thu, 03 Sep 2026 08:30:00 EDT", "www.bea.gov/news/synthetic", "2026-09-03T12:30:00Z"},
		{"fed-policy", "Thu, 3 Sep 2026 08:30:00 GMT", "https://www.federalreserve.gov/synthetic", "2026-09-03T08:30:00Z"},
		{"ecb-news", "Thu, 03 Sep 2026 08:30:00 +0200", "https://www.ecb.europa.eu/synthetic", "2026-09-03T06:30:00Z"},
	} {
		payload := `<?xml version="1.0" encoding="us-ascii"?><rss><channel><item><title>Synthetic &amp; official</title><link>` + tc.link + `</link><pubDate>` + tc.date + `</pubDate></item></channel></rss>`
		batch, err := Parse(sourceSpec(t, tc.id), []byte(payload), now)
		if err != nil || len(batch.Publications) != 1 {
			t.Fatalf("%s parse: %v", tc.id, err)
		}
		item := batch.Publications[0]
		if item.PublishedAt.UTC().Format(time.RFC3339) != tc.want || item.SourceID != tc.id || !item.RetrievedAt.Equal(now) || item.Title != "Synthetic & official" {
			t.Fatal("publication clock or provenance changed")
		}
	}
	for _, payload := range []string{
		`<rss><channel><item><title>Synthetic</title><link>https://attacker.test/news</link></item></channel></rss>`,
		`<rss><channel><item><title>Synthetic</title><link>https://www.bea.gov/news/synthetic</link><pubDate>Thu, 03 Sep 2026 08:30:00 BAD</pubDate></item></channel></rss>`,
		`<rss><channel><item><title>Synthetic</title><link>https://www.bea.gov/news/synthetic</link><pubDate>Thu, 10 Sep 2026 08:30:00 EDT</pubDate></item></channel></rss>`,
	} {
		if _, err := Parse(sourceSpec(t, "bea-news"), []byte(payload), now); err == nil {
			t.Fatal("unsafe or unparseable publication replaced source evidence")
		}
	}
}

func TestCalendarTruncationAndDatePrecisionCannotEraseLastGood(t *testing.T) {
	spec := sourceSpec(t, "bls-calendar")
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	valid := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic release\r\nDTSTART:20260101T010000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	batch, err := Parse(spec, []byte(valid), now)
	if err != nil || len(batch.Events) != 1 {
		t.Fatal(err)
	}
	event := batch.Events[0]
	if event.Date != "2025-12-31" || event.Timezone != "America/New_York" || event.TimePrecision != "instant" {
		t.Fatal("UTC instant was assigned to the wrong source-local day")
	}
	for _, payload := range []string{
		strings.Replace(valid, "END:VCALENDAR\r\n", "", 1),
		strings.Replace(valid, "END:VEVENT\r\n", "", 1),
		strings.Replace(valid, "SUMMARY:", "BEGIN:VEVENT\r\nSUMMARY:", 1),
		strings.Replace(valid, "DTSTART:20260101T010000Z", "DTSTART:20260101T010000Z\r\nRRULE:FREQ=DAILY", 1),
		"BEGIN:VCALENDAR\nEND:VCALENDAR",
	} {
		if _, err := Parse(spec, []byte(payload), now); err == nil {
			t.Fatal("incomplete or unsupported calendar accepted as a replacement")
		}
	}
	onlyDate := strings.Replace(valid, "DTSTART:20260101T010000Z", "DTSTART;VALUE=DATE:20260101", 1)
	batch, err = Parse(spec, []byte(onlyDate), now)
	if err != nil || !batch.Events[0].ScheduledAt.IsZero() || batch.Events[0].TimePrecision != "date" {
		t.Fatal("date-only event gained an invented midnight")
	}
}

func TestParsedFeedCannotRestoreTamperedChildEvidence(t *testing.T) {
	spec := sourceSpec(t, "bea-calendar")
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	batch, err := Parse(spec, []byte(`{"file_last_updated":"2026-09-01T08:00:00", "Synthetic release":{"release_dates":["2026-09-10T12:30:00Z"]}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Batch){
		func(b *Batch) { b.Events[0].SourceURL = "https://attacker.test/calendar" },
		func(b *Batch) { b.Events[0].SourceID = "fed-calendar" },
		func(b *Batch) { b.Events[0].Date = "2026-09-11" },
		func(b *Batch) { b.Events[0].RetrievedAt = now.Add(time.Hour) },
		func(b *Batch) { b.Events[0].TimePrecision = "date" },
		func(b *Batch) { b.Events[0].ID = "forged" },
	} {
		raw, _ := json.Marshal(batch)
		var changed Batch
		_ = json.Unmarshal(raw, &changed)
		mutate(&changed)
		if ValidateBatch(spec, changed, now) == nil {
			t.Fatal("tampered source evidence restored as valid")
		}
	}
}

type publicTransport func(*http.Request) (*http.Response, error)

func (f publicTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestPublicClientRejectsFailuresAndRedirectsWithoutCredentials(t *testing.T) {
	client := NewClient()
	requests := 0
	client.HTTP.Transport = publicTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.Method != "GET" || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("public source request carried authority")
		}
		if r.UserAgent() != "Canary-public-feeds/1.0 (+https://osauer.dev/canary/)" {
			t.Fatal("BLS lost its owner-approved product identity")
		}
		// BEA's RSS server negotiates text/xml and otherwise returns HTTP 406.
		if !strings.Contains(r.Header.Get("Accept"), "text/xml") {
			t.Fatal("official RSS XML response excluded by content negotiation")
		}
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("blocked")), Request: r}, nil
	})
	_, err := client.Fetch(context.Background(), sourceSpec(t, "bls-calendar"), time.Now(), Batch{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || requests != 1 {
		t.Fatal("source failure was hidden or retried without bounds")
	}
	client.HTTP.Transport = publicTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": []string{"http://127.0.0.1/private"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})
	if _, err = client.Fetch(context.Background(), sourceSpec(t, "bls-calendar"), time.Now(), Batch{}); err == nil {
		t.Fatal("official feed redirected into a private endpoint")
	}
	client.HTTP.Transport = publicTransport(func(*http.Request) (*http.Response, error) { return nil, errors.New("private diagnostic") })
	_, err = client.Fetch(context.Background(), sourceSpec(t, "bls-calendar"), time.Now(), Batch{})
	if err == nil || strings.Contains(err.Error(), "private diagnostic") {
		t.Fatal("raw transport details escaped public source status")
	}
}

// TestBLSAccessDenialNeedsResponseEvidence prevents a generic failure from
// becoming an invented entitlement diagnosis or leaking the denial page.
func TestBLSAccessDenialNeedsResponseEvidence(t *testing.T) {
	const denial = `<html><h1>Bureau of Labor Statistics</h1><h2>Access Denied</h2><p>bot activity that doesn&#39;t conform to BLS usage policy is prohibited.</p><p>response-only-marker</p></html>`
	const classified = "source returned HTTP 403: BLS rejected this request under its automated-access policy"
	for _, tc := range []struct {
		name, source, body string
		status             int
		readFailure        bool
		want               string
	}{
		{"witnessed BLS denial", "bls-calendar", denial, 403, false, classified},
		{"generic BLS forbidden", "bls-calendar", "Access Denied", 403, false, "source returned HTTP 403"},
		{"BLS identity absent", "bls-calendar", strings.ReplaceAll(denial, "Bureau of Labor Statistics", "unrelated service"), 403, false, "source returned HTTP 403"},
		{"policy evidence absent", "bls-calendar", `<h1>Bureau of Labor Statistics</h1><h2>Access Denied</h2>`, 403, false, "source returned HTTP 403"},
		{"foreign source", "bea-calendar", denial, 403, false, "source returned HTTP 403"},
		{"different status", "bls-calendar", denial, 503, false, "source returned HTTP 503"},
		{"failed body read", "bls-calendar", denial, 403, true, "source returned HTTP 403"},
		{"oversized body", "bls-calendar", denial + strings.Repeat("x", 8<<10), 403, false, "source returned HTTP 403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient()
			requests := 0
			client.HTTP.Transport = publicTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				var body io.Reader = strings.NewReader(tc.body)
				if tc.readFailure {
					body = io.MultiReader(body, publicReadFailure{})
				}
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(body), Request: r}, nil
			})
			batch, err := client.Fetch(t.Context(), sourceSpec(t, tc.source), time.Now(), Batch{})
			if err == nil || err.Error() != tc.want || requests != 1 || len(batch.Events)+len(batch.Publications) != 0 {
				t.Fatalf("unwitnessed diagnosis, response leak or fabricated recovery: %v", err)
			}
		})
	}
}

type publicReadFailure struct{}

func (publicReadFailure) Read([]byte) (int, error) {
	return 0, errors.New("response-only-reader-error")
}

func TestPublicClientRedirectReappliesDestinationIdentity(t *testing.T) {
	for _, hosts := range [][2]string{
		{"www.bls.gov", "www.newyorkfed.org"},
		{"www.newyorkfed.org", "www.bls.gov"},
	} {
		t.Run(hosts[0]+"-to-"+hosts[1], func(t *testing.T) {
			client := NewClient()
			requests := 0
			client.HTTP.Transport = publicTransport(func(req *http.Request) (*http.Response, error) {
				if requests >= len(hosts) || req.URL.Hostname() != hosts[requests] {
					t.Fatal("unexpected redirect request")
				}
				want := "Go-http-client/1.1"
				if req.URL.Hostname() == "www.bls.gov" {
					want = "Canary-public-feeds/1.0 (+https://osauer.dev/canary/)"
				}
				if req.UserAgent() != want {
					t.Fatal("redirect inherited the previous destination's identity")
				}
				requests++
				if requests == 1 {
					return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://" + hosts[1] + "/calendar"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("public fixture")), Request: req}, nil
			})
			res, err := client.read(t.Context(), "https://"+hosts[0]+"/calendar", Batch{})
			if err != nil || string(res.body) != "public fixture" || requests != 2 {
				t.Fatalf("permitted redirect failed: requests=%d err=%v", requests, err)
			}
		})
	}
}

// TestConditionalReadRenewsOnlySolicitedNotModified keeps a 304 from renewing
// evidence the client never asked about, drops malformed validators instead of
// replaying them, and renews a copy rather than the caller's retained batch.
func TestConditionalReadRenewsOnlySolicitedNotModified(t *testing.T) {
	spec := sourceSpec(t, "bls-calendar")
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	status := http.StatusOK
	header := http.Header{"Etag": {`"v1" X-Injected: 1`}, "Last-Modified": {"yesterday"}}
	client := NewClient()
	client.HTTP.Transport = publicTransport(func(r *http.Request) (*http.Response, error) {
		body := "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic release\r\nDTSTART:20260101T010000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	batch, err := client.Fetch(t.Context(), spec, now, Batch{})
	if err != nil || batch.ETag != "" || batch.LastModified != "" {
		t.Fatalf("malformed validators retained: %+v %v", batch, err)
	}
	status = http.StatusNotModified
	if _, err := client.Fetch(t.Context(), spec, now.Add(time.Hour), batch); err == nil {
		t.Fatal("an unsolicited 304 renewed retained evidence")
	}
	batch.ETag = `W/"v1"`
	renewed, err := client.Fetch(t.Context(), spec, now.Add(time.Hour), batch)
	if err != nil || !renewed.Events[0].RetrievedAt.Equal(now.Add(time.Hour)) || renewed.ETag != batch.ETag {
		t.Fatalf("solicited 304 did not renew the batch: %v", err)
	}
	if !batch.Events[0].RetrievedAt.Equal(now) {
		t.Fatal("renewal mutated the caller's retained batch")
	}
	for _, tampered := range []Batch{{ETag: "\"v1\"\r\nX-Injected: 1"}, {LastModified: "Wed, 10 Jun 2026 16:56:37 GMT\r\nX: 1"}} {
		tampered.Events = batch.Events
		if ValidateBatch(spec, tampered, now) == nil {
			t.Fatal("a restored validator could inject request headers")
		}
	}
}

// TestConditionalReadNeedsTheRunningParser keeps a 304 from renewing records an
// earlier parser produced: the publisher confirms its bytes, not Canary's
// reading of them, so such a batch is read in full once and re-parsed.
func TestConditionalReadNeedsTheRunningParser(t *testing.T) {
	spec := sourceSpec(t, "bls-calendar")
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	const body = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic release\r\nDTSTART:20260101T010000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	var conditional []bool
	client := NewClient()
	client.HTTP.Transport = publicTransport(func(r *http.Request) (*http.Response, error) {
		asked := r.Header.Get("If-None-Match") != "" || r.Header.Get("If-Modified-Since") != ""
		conditional = append(conditional, asked)
		if asked {
			return &http.Response{StatusCode: http.StatusNotModified, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
		}
		header := http.Header{"Etag": {`"v1"`}, "Last-Modified": {"Wed, 10 Jun 2026 16:56:37 GMT"}}
		return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	current, err := client.Fetch(t.Context(), spec, now, Batch{})
	if err != nil || current.ETag != `"v1"` {
		t.Fatalf("initial read kept no validators: %+v %v", current, err)
	}
	if _, err := client.Fetch(t.Context(), spec, now.Add(time.Hour), current); err != nil || len(conditional) != 2 || !conditional[1] {
		t.Fatalf("a batch from the running parser was not revalidated: %v %v", conditional, err)
	}

	// A batch persisted without the running parser's stamp, holding a field
	// an older parser extracted differently. Its validators still match.
	raw, _ := json.Marshal(current)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	delete(fields, "parser")
	raw, _ = json.Marshal(fields)
	var older Batch
	if err := json.Unmarshal(raw, &older); err != nil {
		t.Fatal(err)
	}
	older.Events[0].Category = "Output of an older parser"
	if err := ValidateBatch(spec, older, now); err != nil || older.ETag != current.ETag {
		t.Fatalf("older batch fixture must restore with its validators: %v", err)
	}
	reread, err := client.Fetch(t.Context(), spec, now.Add(2*time.Hour), older)
	if err != nil {
		t.Fatal(err)
	}
	if len(conditional) != 3 || conditional[2] || reread.Events[0].Category != "Economic release" {
		t.Fatalf("a 304 renewed an older parser's output: conditional=%v category=%q", conditional, reread.Events[0].Category)
	}
	if _, err := client.Fetch(t.Context(), spec, now.Add(3*time.Hour), reread); err != nil || len(conditional) != 4 || !conditional[3] {
		t.Fatalf("the re-parsed batch did not resume conditional reads: %v %v", conditional, err)
	}
}

// TestFetchFailureIsRetryableOnlyWhenRepeatingCanSucceed separates responses a
// later identical read can fix from rejections that repeat until the parser or
// the publisher changes, so health never promises self-healing for a format
// change.
func TestFetchFailureIsRetryableOnlyWhenRepeatingCanSucceed(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	const ics = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic release\r\nDTSTART:20260101T010000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
	rss := func(link, published string) string {
		return `<rss version="2.0"><channel><item><title>Synthetic statement</title><link>` + link + `</link><pubDate>` + published + `</pubDate></item></channel></rss>`
	}
	const request, parse = rpc.SourceFailureStagePublicSourceRequest, rpc.SourceFailureStagePublicSourceParse
	for _, tc := range []struct {
		name, source, body string
		stage              string
		retryable          bool
	}{
		{"recurring calendar", "bls-calendar", strings.Replace(ics, "END:VEVENT", "RRULE:FREQ=MONTHLY\r\nEND:VEVENT", 1), parse, false},
		{"calendar replaced by a page", "bls-calendar", "<html><body>Calendar moved</body></html>", parse, false},
		{"calendar without events", "bls-calendar", "BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n", parse, false},
		{"BEA root changed", "bea-calendar", `["Synthetic release"]`, parse, false},
		{"RSS root changed", "fed-policy", `<feed xmlns="http://www.w3.org/2005/Atom"></feed>`, parse, false},
		{"publication provenance", "fed-policy", rss("https://attacker.test/statement.htm", "Thu, 24 Sep 2026 11:00:00 GMT"), parse, false},
		{"size limit", "bls-calendar", strings.Repeat("x", 2<<20+1), request, false},
		{"calendar cut short", "bls-calendar", strings.TrimSuffix(ics, "END:VCALENDAR\r\n"), parse, true},
		{"empty body", "bls-calendar", " \r\n", request, true},
		{"publication ahead of the clock", "fed-policy", rss("https://www.federalreserve.gov/newsevents/pressreleases/synthetic.htm", "Thu, 24 Sep 2026 13:00:00 GMT"), parse, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient()
			client.HTTP.Transport = publicTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
			})
			_, err := client.Fetch(t.Context(), sourceSpec(t, tc.source), now, Batch{})
			typed, ok := errors.AsType[*FetchError](err)
			if !ok {
				t.Fatalf("untyped failure %v", err)
			}
			got := rpc.SourceFailure{Code: typed.Code, Stage: typed.Stage, FailedAt: now, Retryable: typed.Retryable}
			if want := (rpc.SourceFailure{Code: rpc.SourceFailureInvalidPayload, Stage: tc.stage, FailedAt: now, Retryable: tc.retryable}); got != want || !rpc.ValidSourceFailure(&got) {
				t.Fatalf("%q classified as %+v, want %+v", err, got, want)
			}
		})
	}
}
