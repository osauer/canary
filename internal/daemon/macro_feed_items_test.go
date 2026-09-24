package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/macrosource"
	"github.com/osauer/canary/v2/internal/rpc"
)

// preSkipMacroRecord is the record shape persisted before publication feeds
// counted skipped items: its batch has no skipped_items member.
type preSkipMacroRecord struct {
	Source rpc.MacroSource `json:"source"`
	Batch  struct {
		Events       []rpc.MacroEvent       `json:"events"`
		Publications []rpc.MacroPublication `json:"publications"`
		WindowStart  string                 `json:"window_start,omitempty"`
		WindowEnd    string                 `json:"window_end,omitempty"`
	} `json:"batch"`
}

// Restore validation binds the success envelope, receipts and source identity.
// A retained publication batch must reload from both the pre-change record and
// one that skipped an item, and the skip note must never enter the persisted
// success envelope, where restore would reject it as a failure.
func TestMacroPublicationBatchSurvivesRestartBeforeAndAfterSkippedItems(t *testing.T) {
	store := openMarketTestCoreStore(t)
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	s := &Server{coreStore: store, now: func() time.Time { return now }}
	var spec macrosource.Spec
	for _, candidate := range macrosource.Specs() {
		if candidate.ID == "bea-news" {
			spec = candidate
		}
	}
	const (
		pubDate = "Thu, 24 Sep 2026 08:30:00 EDT"
		trade   = "https://www.bea.gov/news/2026/synthetic-trade"
		apex    = "https://bea.gov/news/2026/synthetic-transactions"
		note    = "1 feed item skipped: invalid title, link or publication date"
	)
	item := func(title, link string) string {
		return "<item><title>" + title + "</title><link>" + link + "</link><pubDate>" + pubDate + "</pubDate></item>"
	}
	feed := func(items ...string) []byte {
		return []byte(`<?xml version="1.0" encoding="us-ascii"?><rss version="2.0"><channel>` + strings.Join(items, "") + `</channel></rss>`)
	}
	reload := func() (macroRecord, rpc.MacroSource) {
		t.Helper()
		restarted := &Server{coreStore: store, now: func() time.Time { return now }}
		restarted.macro = restarted.loadMacroSources()
		for _, source := range restarted.handleMacroSnapshot().Sources {
			if source.ID == spec.ID {
				return restarted.macro.records[spec.ID], source
			}
		}
		t.Fatal("snapshot omitted the source")
		return macroRecord{}, rpc.MacroSource{}
	}

	// A record the pre-change daemon could write: every item valid, no count.
	parsed, err := macrosource.Parse(spec, feed(item("Synthetic trade release", trade)), now)
	if err != nil {
		t.Fatal(err)
	}
	var legacy preSkipMacroRecord
	legacy.Source = rpc.MacroSource{ID: spec.ID, Name: spec.Name, URL: spec.URL, Kind: spec.Kind, Coverage: spec.Coverage, Availability: "available", LastAttempt: now, LastSuccess: now, ValidUntil: now.Add(30 * time.Minute), NextAttempt: now.Add(5 * time.Minute)}
	legacy.Batch.Publications = parsed.Publications
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveMarketDocument(t.Context(), store, "public-macro:"+spec.ID, macroStateKind, raw); err != nil {
		t.Fatal(err)
	}
	old, oldView := reload()
	if old.Source.Availability != "available" || len(old.Batch.Publications) != 1 || old.Batch.Publications[0].ID != parsed.Publications[0].ID || oldView.Detail != "" {
		t.Fatalf("pre-change record lost its batch or gained a note on restart: %+v detail=%q", old.Source, oldView.Detail)
	}

	// The production reader skips a foreign-host item and keeps the apex link.
	now = old.Source.NextAttempt
	status := http.StatusOK
	client := macrosource.NewClient()
	client.HTTP.Transport = macroHTTPTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != spec.URL {
			t.Fatal("unexpected public acquisition request")
		}
		body := feed(item("Synthetic trade release", trade), item("Synthetic mirror", "https://mirror.example/news/synthetic"), item("Synthetic transactions release", apex))
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body))), Request: req}, nil
	})
	c := s.loadMacroSources()
	c.client = client
	s.refreshMacroSource(t.Context(), c, spec)
	if row := c.records[spec.ID]; row.Source.Availability != "available" || row.Source.Detail != "" || len(row.Batch.Publications) != 2 {
		t.Fatalf("one invalid item discarded the feed or entered the success envelope: %+v", row.Source)
	}
	stored, ok, err := loadMarketState(store, "public-macro:"+spec.ID, macroStateKind)
	if err != nil || !ok || !strings.Contains(string(stored), `"skipped_items":1`) {
		t.Fatalf("skipped-item count not persisted with its batch: %v", err)
	}
	restored, view := reload()
	if restored.Source.Availability != "available" || !restored.Source.LastSuccess.Equal(now) || len(restored.Batch.Publications) != 2 || view.Detail != note || view.Stale {
		t.Fatalf("restart discarded the partial batch or its disclosure: %+v detail=%q", restored.Source, view.Detail)
	}
	if restored.Batch.Publications[1].SourceURL != apex {
		t.Fatal("apex publication link rewritten across restart")
	}
	health := &Server{coreStore: store, now: func() time.Time { return now }}
	health.macro = health.loadMacroSources()
	rows, _ := health.collectDataHealth(now)
	disclosed := false
	for _, row := range rows {
		if row.ID == "macro:"+spec.ID {
			disclosed = row.State == "current" && strings.Contains(row.Detail, note)
			if !disclosed {
				t.Fatalf("health row hid the omission: state=%s detail=%q", row.State, row.Detail)
			}
		}
	}
	if !disclosed {
		t.Fatal("health report omitted the source")
	}

	// A later failure keeps the retained batch and its note across restart.
	now = c.records[spec.ID].Source.NextAttempt
	status = http.StatusServiceUnavailable
	s.refreshMacroSource(t.Context(), c, spec)
	failed, failedView := reload()
	if failed.Source.Availability != "unavailable" || len(failed.Batch.Publications) != 2 || !strings.Contains(failedView.Detail, "HTTP 503") || !strings.Contains(failedView.Detail, note) {
		t.Fatalf("failure lost the retained batch or its disclosure: %+v detail=%q", failed.Source, failedView.Detail)
	}
}

// A skipped item leaves the publisher's feed only partly represented, so the
// overview must not claim complete coverage while the source is otherwise
// current; the note is a projection and never enters the stored record.
func TestMacroSnapshotCoverageDisclosesSkippedFeedItems(t *testing.T) {
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }, macro: &macroCache{records: map[string]macroRecord{}}}
	for _, spec := range macrosource.Specs() {
		row := coldMacroRecord(spec)
		row.Source.Availability, row.Source.Detail = "available", ""
		row.Source.LastAttempt, row.Source.LastSuccess, row.Source.ValidUntil = now, now, now.Add(30*time.Minute)
		if spec.ID == "nyfed-calendar" {
			row.Source.WindowStart, row.Source.WindowEnd = "2026-09-01", "2026-09-30"
		}
		s.macro.records[spec.ID] = row
	}
	if out := s.macroSnapshotWindow("2026-09-24", "2026-09-24"); out.CoverageStatus != "available" {
		t.Fatalf("fixture is not complete coverage: %s", out.CoverageStatus)
	}
	row := s.macro.records["bea-news"]
	row.Batch.SkippedItems = 2
	s.macro.records["bea-news"] = row
	out := s.macroSnapshotWindow("2026-09-24", "2026-09-24")
	if out.CoverageStatus != "partial" {
		t.Fatal("overview claimed complete coverage after a feed skipped items")
	}
	for _, source := range out.Sources {
		if source.ID == "bea-news" && (source.Availability != "available" || source.Detail != "2 feed items skipped: invalid title, link or publication date") {
			t.Fatalf("skipped items hidden or reported as a failure: %+v", source)
		}
	}
	if s.macro.records["bea-news"].Source.Detail != "" {
		t.Fatal("projection wrote the note into the retained success envelope")
	}
}

// Batches are assigned only on success, so a never-successful record can carry
// no skipped-item count. A stored one is forged or corrupt and must not reload
// to announce omissions from a feed that was never read, for publication feeds
// and calendars alike.
func TestMacroRestoreRejectsSkippedCountWithoutSuccess(t *testing.T) {
	store := openMarketTestCoreStore(t)
	now := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	s := &Server{coreStore: store, now: func() time.Time { return now }}
	for _, spec := range macrosource.Specs() {
		if spec.ID != "bea-news" && spec.ID != "bea-calendar" {
			continue
		}
		for _, skipped := range []int{0, 7} {
			row := coldMacroRecord(spec)
			row.Source.Detail = "source returned HTTP 503"
			row.Source.LastAttempt, row.Source.FirstFailure, row.Source.ConsecutiveFailures = now, now, 1
			row.Source.NextAttempt = now.Add(5 * time.Minute)
			row.Batch.SkippedItems = skipped
			raw, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			if err := saveMarketDocument(t.Context(), store, "public-macro:"+spec.ID, macroStateKind, raw); err != nil {
				t.Fatal(err)
			}
			s.macro = s.loadMacroSources()
			restored := s.macro.records[spec.ID]
			var view rpc.MacroSource
			for _, source := range s.handleMacroSnapshot().Sources {
				if source.ID == spec.ID {
					view = source
				}
			}
			switch {
			case skipped == 0 && (restored.Source.Detail != row.Source.Detail || restored.Source.ConsecutiveFailures != 1):
				t.Errorf("%s: valid failed record did not restore: %+v", spec.ID, restored.Source)
			case skipped != 0 && (restored.Source.Detail != "Saved public source record is invalid" || restored.Batch.SkippedItems != 0 || strings.Contains(view.Detail, "skipped")):
				t.Errorf("%s: forged skipped count restored without a batch: detail=%q", spec.ID, view.Detail)
			}
		}
	}
}
