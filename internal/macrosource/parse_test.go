package macrosource

import (
	"strings"
	"testing"
	"time"
)

type rssItem struct{ title, link, date string }

// rssFeed builds a synthetic us-ascii RSS document shaped like the BEA feed.
func rssFeed(items ...rssItem) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="us-ascii"?><rss version="2.0"><channel><title>Synthetic</title>`)
	for _, it := range items {
		b.WriteString("<item><title>" + it.title + "</title><link>" + it.link + "</link><pubDate>" + it.date + "</pubDate></item>")
	}
	b.WriteString("</channel></rss>")
	return []byte(b.String())
}

const syntheticBEADate = "Thu, 24 Sep 2026 08:30:00 EDT"

// BEA published a release linked on its apex host; rejecting it cost the whole
// feed. The link is recorded verbatim and must survive restore validation,
// while the fetch allowlist and every other feed's host rule stay unchanged.
func TestRSSAcceptsBEAApexPublicationLink(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	spec := sourceSpec(t, "bea-news")
	const apex = "https://bea.gov/news/2026/synthetic-transactions-2nd-quarter-2026"
	batch, err := Parse(spec, rssFeed(rssItem{"Synthetic transactions, 2nd Quarter 2026", apex, syntheticBEADate}), now)
	if err != nil || len(batch.Publications) != 1 {
		t.Fatalf("official BEA apex link rejected the feed: %v", err)
	}
	if got := batch.Publications[0].SourceURL; got != apex {
		t.Fatalf("publication provenance rewritten: %q", got)
	}
	if err := ValidateBatch(spec, batch, now); err != nil {
		t.Fatalf("retained apex publication fails restore validation: %v", err)
	}
	if SafeURL(apex) {
		t.Fatal("recording an apex link widened the fetch allowlist")
	}
	for _, tc := range []struct{ source, link string }{
		{"fed-policy", apex},
		{"bea-news", "http://bea.gov/news/synthetic"},
		{"bea-news", "https://bea.gov:8443/news/synthetic"},
		{"bea-news", "https://user@bea.gov/news/synthetic"},
		{"bea-news", "https://bea.gov.attacker.test/news/synthetic"},
		{"bea-news", "https://attacker.test/bea.gov/news/synthetic"},
	} {
		if _, err := Parse(sourceSpec(t, tc.source), rssFeed(rssItem{"Synthetic", tc.link, syntheticBEADate}), now); err == nil {
			t.Fatalf("%s accepted a non-official publication link %q", tc.source, tc.link)
		}
	}
}

// One malformed item used to reject the whole feed, leaving every valid BEA
// publication stale. Invalid items are skipped, counted and disclosed.
func TestRSSSkipsInvalidItemsAndKeepsFeed(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	spec := sourceSpec(t, "bea-news")
	batch, err := Parse(spec, rssFeed(
		rssItem{"Synthetic trade release", "https://www.bea.gov/news/2026/synthetic-trade", syntheticBEADate},
		rssItem{"Synthetic mirror", "https://mirror.example/news/synthetic", syntheticBEADate},
		rssItem{"Synthetic transactions release", "https://bea.gov/news/2026/synthetic-transactions", syntheticBEADate},
		rssItem{"", "https://www.bea.gov/news/2026/untitled", syntheticBEADate},
		rssItem{"Synthetic undated release", "https://www.bea.gov/news/2026/undated", "Thu, 24 Sep 2026 08:30:00 BAD"},
	), now)
	if err != nil {
		t.Fatalf("invalid items discarded the valid feed: %v", err)
	}
	var urls []string
	for _, p := range batch.Publications {
		urls = append(urls, p.SourceURL)
	}
	if strings.Join(urls, " ") != "https://www.bea.gov/news/2026/synthetic-trade https://bea.gov/news/2026/synthetic-transactions" {
		t.Fatalf("kept publications = %v", urls)
	}
	if batch.SkippedItems != 3 || batch.SkippedDisclosure(spec) != "3 feed items skipped: invalid title, link or publication date" {
		t.Fatalf("omission hidden: skipped=%d disclosure=%q", batch.SkippedItems, batch.SkippedDisclosure(spec))
	}
	if err := ValidateBatch(spec, batch, now); err != nil {
		t.Fatalf("partial feed fails restore validation: %v", err)
	}
	one, err := Parse(spec, rssFeed(
		rssItem{"Synthetic trade release", "https://www.bea.gov/news/2026/synthetic-trade", syntheticBEADate},
		rssItem{"Synthetic mirror", "https://mirror.example/news/synthetic", syntheticBEADate},
	), now)
	if err != nil || one.SkippedDisclosure(spec) != "1 feed item skipped: invalid title, link or publication date" {
		t.Fatalf("single omission disclosure = %q, %v", one.SkippedDisclosure(spec), err)
	}
}

// Skipping is per item. A feed with nothing usable, an unreadable document or a
// publication dated after Canary's receipt still fails as a whole.
func TestRSSFailsWhenNoValidItemRemains(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	spec := sourceSpec(t, "bea-news")
	valid := rssItem{"Synthetic trade release", "https://www.bea.gov/news/2026/synthetic-trade", syntheticBEADate}
	for _, tc := range []struct {
		name string
		feed []byte
		want string
	}{
		{"every link or title invalid", rssFeed(rssItem{"Synthetic mirror", "https://mirror.example/news", syntheticBEADate}, rssItem{"", valid.link, syntheticBEADate}), "publication title or source link invalid"},
		{"every date unreadable", rssFeed(rssItem{valid.title, valid.link, "24/09/2026"}), "publication date format changed"},
		{"future publication beside a valid one", rssFeed(valid, rssItem{"Synthetic embargoed release", "https://www.bea.gov/news/2026/embargoed", "Fri, 25 Sep 2026 08:30:00 EDT"}), "publication time is in the future"},
		{"no items", rssFeed(), "source supplied no usable records"},
		{"feed format changed", []byte(`<feed><entry><title>Synthetic</title></entry></feed>`), "publication feed format changed"},
		{"encoding unsupported", []byte(strings.Replace(string(rssFeed(valid)), "us-ascii", "iso-8859-1", 1)), "publication feed format changed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			batch, err := Parse(spec, tc.feed, now)
			if err == nil || err.Error() != tc.want || len(batch.Publications) != 0 || batch.SkippedItems != 0 {
				t.Fatalf("got batch=%+v err=%v, want %q", batch, err, tc.want)
			}
		})
	}
}

// A restored record must not claim omissions its parser cannot produce.
func TestValidateBatchRejectsForgedSkippedCount(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	news := sourceSpec(t, "bea-news")
	batch, err := Parse(news, rssFeed(rssItem{"Synthetic trade release", "https://www.bea.gov/news/2026/synthetic-trade", syntheticBEADate}), now)
	if err != nil {
		t.Fatal(err)
	}
	batch.SkippedItems = -1
	if ValidateBatch(news, batch, now) == nil {
		t.Fatal("negative skipped count restored as valid")
	}
	calendar := sourceSpec(t, "bea-calendar")
	events, err := Parse(calendar, []byte(`{"Synthetic release":{"release_dates":["2026-09-25T12:30:00Z"]}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	events.SkippedItems = 1
	if ValidateBatch(calendar, events, now) == nil {
		t.Fatal("calendar restored a skipped-item count it cannot produce")
	}
}
