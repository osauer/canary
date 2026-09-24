package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/macrosource"
	"github.com/osauer/canary/v2/internal/rpc"
)

type macroFixtureFetcher struct {
	fail  bool
	calls int
}

type macroHTTPTransport func(*http.Request) (*http.Response, error)

func (f macroHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Exercise the production HTTP reader and parser through durable refreshes,
// including replacement of a rescheduled release and recovery after an outage.
func TestMacroHTTPRefreshRevisionOutageRecoveryAndRestart(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	s := &Server{coreStore: openMarketTestCoreStore(t), now: func() time.Time { return now }}
	c := s.loadMacroSources()
	s.macro = c
	var spec macrosource.Spec
	for _, candidate := range macrosource.Specs() {
		if candidate.ID == "nyfed-calendar" {
			spec = candidate
		}
	}
	day, calls, status := 14, 0, http.StatusOK
	client := macrosource.NewClient()
	client.HTTP.Transport = macroHTTPTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Method != http.MethodGet || req.URL.String() != spec.URL || req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" {
			t.Fatal("unexpected public acquisition request")
		}
		var body strings.Builder
		body.WriteString(`<p>all Eastern Time</p><td class="ts-data-table-head"><div>September 2026</div></td><table class="research-table-1col greyborder">`)
		for d := 1; d <= 30; d++ {
			fmt.Fprintf(&body, `<td><div>%02d<br/>`, d)
			if d == day {
				body.WriteString(`<a href="https://www.bls.gov/">Synthetic release</a><br/>(08:30)`)
			}
			body.WriteString(`</div></td>`)
		}
		body.WriteString(`</table>`)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body.String())), Request: req}, nil
	})
	c.client = client
	s.refreshMacroSource(t.Context(), c, spec)
	first := c.records[spec.ID]
	if first.Source.Availability != "available" || len(first.Batch.Events) != 1 || first.Batch.Events[0].Date != "2026-09-14" {
		t.Fatal("HTTP acquisition did not establish source-owned evidence")
	}
	now = first.Source.NextAttempt
	day = 15
	s.refreshMacroSource(t.Context(), c, spec)
	revised := c.records[spec.ID]
	if revised.Source.LastSuccess != now || len(revised.Batch.Events) != 1 || revised.Batch.Events[0].Date != "2026-09-15" || revised.Batch.Events[0].ID == first.Batch.Events[0].ID {
		t.Fatal("successful refresh retained a superseded release date")
	}
	status = http.StatusServiceUnavailable
	for range 6 {
		now = c.records[spec.ID].Source.NextAttempt
		s.refreshMacroSource(t.Context(), c, spec)
	}
	failed := c.records[spec.ID]
	if failed.Source.NextAttempt.Sub(now) != time.Hour || failed.Source.ConsecutiveFailures != 6 || failed.Source.LastSuccess != revised.Source.LastSuccess || failed.Source.ValidUntil != revised.Source.ValidUntil {
		t.Fatal("outage changed last-good clocks or exceeded retry cap")
	}
	restored := s.loadMacroSources()
	restored.client = client
	s.macro = restored
	before := calls
	s.refreshMacroSource(t.Context(), restored, spec)
	if calls != before || restored.records[spec.ID].Batch.Events[0].ID != revised.Batch.Events[0].ID {
		t.Fatal("restart renewed acquisition or lost the last-good event")
	}
	view := s.macroSnapshotWindow("2026-09-15", "2026-09-15")
	if len(view.Events) != 1 || view.CoverageStatus != "partial" {
		t.Fatal("outage hid known events or claimed complete coverage")
	}
	for _, source := range view.Sources {
		if source.ID == spec.ID && (!source.Stale || source.Availability != "unavailable") {
			t.Fatal("expired last-good source became current after restart")
		}
	}
	now = failed.Source.NextAttempt
	status = http.StatusOK
	s.refreshMacroSource(t.Context(), restored, spec)
	recovered := s.loadMacroSources().records[spec.ID]
	if recovered.Source.Availability != "available" || recovered.Source.LastSuccess != now || recovered.Source.ConsecutiveFailures != 0 || !recovered.Source.FirstFailure.IsZero() || recovered.Batch.Events[0].RetrievedAt != now {
		t.Fatal("successful recovery did not persist fresh evidence and clear failure state")
	}
}

func (f *macroFixtureFetcher) Fetch(_ context.Context, s macrosource.Spec, at time.Time, _ macrosource.Batch) (macrosource.Batch, error) {
	f.calls++
	if f.fail {
		return macrosource.Batch{}, errors.New("source returned HTTP 403")
	}
	return macrosource.Parse(s, []byte(`{"Synthetic release":{"release_dates":["2026-09-10T12:30:00Z"]}}`), at)
}
func beaMacroSpec(t *testing.T) macrosource.Spec {
	t.Helper()
	for _, s := range macrosource.Specs() {
		if s.ID == "bea-calendar" {
			return s
		}
	}
	t.Fatal("fixture source absent")
	return macrosource.Spec{}
}

func TestMacroFailureRetainsEvidenceAcrossRestartAndNeverExtendsFreshness(t *testing.T) {
	store := openMarketTestCoreStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	s := &Server{coreStore: store, now: func() time.Time { return now }}
	c := s.loadMacroSources()
	fixture := &macroFixtureFetcher{}
	c.client = fixture
	s.macro = c
	spec := beaMacroSpec(t)
	s.refreshMacroSource(t.Context(), c, spec)
	first := c.records[spec.ID]
	if first.Source.Availability != "available" || len(first.Batch.Events) != 1 {
		t.Fatal("valid source was not retained")
	}
	now = now.Add(5 * time.Minute)
	fixture.fail = true
	s.refreshMacroSource(t.Context(), c, spec)
	failed := c.records[spec.ID]
	if failed.Source.Availability != "unavailable" || !strings.Contains(failed.Source.Detail, "403") || !failed.Source.LastSuccess.Equal(first.Source.LastSuccess) || !failed.Source.ValidUntil.Equal(first.Source.ValidUntil) || failed.Batch.Events[0].ID != first.Batch.Events[0].ID {
		t.Fatal("failed read replaced or refreshed last-good evidence")
	}
	restored := &Server{coreStore: store, now: func() time.Time { return now }}
	restored.macro = restored.loadMacroSources()
	row := restored.macro.records[spec.ID]
	if row.Source.Availability != "unavailable" || row.Batch.Events[0].ID != first.Batch.Events[0].ID {
		t.Fatal("restart erased source failure or retained event")
	}
	now = now.Add(time.Hour)
	snapshot := restored.handleMacroSnapshot()
	for _, source := range snapshot.Sources {
		if source.ID == spec.ID && !source.Stale {
			t.Fatal("retained calendar did not expire")
		}
	}
	if snapshot.CoverageStatus != "partial" || fixture.calls != 2 {
		t.Fatal("snapshot fabricated complete coverage or dispatched collection")
	}
	fixture.fail = false
	s.refreshMacroSource(t.Context(), c, spec)
	if c.records[spec.ID].Source.Availability != "available" || !c.records[spec.ID].Source.LastSuccess.Equal(now) {
		t.Fatal("successful source recovery did not replace the failure")
	}
}

func TestMacroRestoreRejectsCorruptProvenanceAndIsolatedStartsStayOffline(t *testing.T) {
	store := openMarketTestCoreStore(t)
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	s := &Server{coreStore: store, now: func() time.Time { return now }}
	c := s.loadMacroSources()
	c.client = &macroFixtureFetcher{}
	spec := beaMacroSpec(t)
	s.refreshMacroSource(t.Context(), c, spec)
	valid := c.records[spec.ID]
	for _, mutate := range []func(*macroRecord){
		func(r *macroRecord) { r.Source.Kind = "rss" },
		func(r *macroRecord) { r.Source.ValidUntil = now.AddDate(1, 0, 0) },
		func(r *macroRecord) { r.Batch.Events[0].SourceURL = "https://attacker.test/" },
		func(r *macroRecord) { r.Batch.Events[0].RetrievedAt = now.Add(-time.Hour) },
		func(r *macroRecord) { r.Source.LastSuccess = time.Time{} },
	} {
		raw, _ := json.Marshal(valid)
		var corrupt macroRecord
		_ = json.Unmarshal(raw, &corrupt)
		mutate(&corrupt)
		raw, _ = json.Marshal(corrupt)
		if err := saveMarketDocument(t.Context(), store, "public-macro:"+spec.ID, macroStateKind, raw); err != nil {
			t.Fatal(err)
		}
		restored := s.loadMacroSources().records[spec.ID]
		if restored.Source.Availability != "unavailable" || len(restored.Batch.Events) != 0 || !strings.Contains(restored.Source.Detail, "invalid") {
			t.Fatal("corrupt cached provenance became source authority")
		}
	}
	for _, productionPath := range []bool{false, true} {
		// Both isolated stores and the linker-configured hermetic CLI must load
		// retained records without starting a network worker.
		s.productionStateDatabase = productionPath
		s.disableMacroSources = productionPath
		ctx, cancel := context.WithCancel(context.Background())
		s.startMacroSources(ctx)
		done := make(chan struct{})
		go func() { s.macroLoopWG.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
			cancel()
			<-done
			t.Fatal("offline daemon started a public source worker")
		}
		cancel()
		if !s.macro.records[spec.ID].Source.LastAttempt.IsZero() {
			t.Fatal("offline daemon contacted an external source")
		}
	}
}

func TestMacroFailureBackoffAndFirstFailureSurviveRestart(t *testing.T) {
	store := openMarketTestCoreStore(t)
	now := time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC)
	s := &Server{coreStore: store, now: func() time.Time { return now }}
	c := s.loadMacroSources()
	fixture := &macroFixtureFetcher{fail: true}
	c.client = fixture
	spec := beaMacroSpec(t)
	s.refreshMacroSource(t.Context(), c, spec)
	first := c.records[spec.ID].Source
	if first.ConsecutiveFailures != 1 || !first.FirstFailure.Equal(now) || !first.LastSuccess.IsZero() {
		t.Fatal("failed first read acquired fictitious success or omitted failure")
	}
	now = now.Add(time.Minute)
	s.refreshMacroSource(t.Context(), c, spec)
	if fixture.calls != 1 {
		t.Fatal("source retried before its bounded retry deadline")
	}
	now = first.NextAttempt
	s.refreshMacroSource(t.Context(), c, spec)
	restored := s.loadMacroSources().records[spec.ID].Source
	if restored.ConsecutiveFailures != 2 || !restored.FirstFailure.Equal(first.FirstFailure) || !restored.NextAttempt.Equal(now.Add(10*time.Minute)) {
		t.Fatal("restart erased repeat-failure health or backoff")
	}
}

func TestMacroRequestedWindowFiltersBeforeResponseLimits(t *testing.T) {
	now := time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }}
	spec := beaMacroSpec(t)
	c := &macroCache{records: map[string]macroRecord{}}
	row := coldMacroRecord(spec)
	row.Source.LastSuccess = now
	row.Source.ValidUntil = now.Add(spec.Freshness)
	row.Source.Availability = "available"
	for i := range 60 {
		row.Batch.Events = append(row.Batch.Events, rpc.MacroEvent{ID: fmt.Sprintf("old-%02d", i), Date: "2026-09-10"})
	}
	row.Batch.Events = append(row.Batch.Events, rpc.MacroEvent{ID: "overnight-release", Date: "2026-09-11"})
	c.records[spec.ID] = row
	s.macro = c
	raw, _ := json.Marshal(rpc.MacroSnapshotParams{WindowStart: "2026-09-11", WindowEnd: "2026-09-11"})
	out, err := s.handleMacroRequest(rpc.Request{Params: raw})
	if err != nil || len(out.Events) != 1 || out.Events[0].ID != "overnight-release" || out.Truncated {
		t.Fatalf("earlier releases crowded out requested window: %v", err)
	}
	if out.CoverageStatus != "partial" {
		t.Fatal("filtered events hid failed providers")
	}
	for _, bad := range []rpc.MacroSnapshotParams{{WindowStart: "2026-09-11"}, {WindowStart: "2026-09-12", WindowEnd: "2026-09-11"}, {WindowStart: "2026-09-01", WindowEnd: "2026-10-02"}} {
		raw, _ := json.Marshal(bad)
		if _, err := s.handleMacroRequest(rpc.Request{Params: raw}); err == nil {
			t.Fatal("unbounded or ambiguous window accepted")
		}
	}
}

func TestMacroBackupDoesNotHealPrimaryFailureAndCoverageExpires(t *testing.T) {
	now := time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC)
	s := &Server{now: func() time.Time { return now }, macro: &macroCache{records: map[string]macroRecord{}}}
	for _, spec := range macrosource.Specs() {
		row := coldMacroRecord(spec)
		row.Source.Availability = "available"
		row.Source.LastSuccess = now
		row.Source.ValidUntil = now.Add(spec.Freshness)
		if spec.ID == "bls-calendar" {
			row.Source.Availability = "unavailable"
			row.Source.Detail = "source returned HTTP 403"
			row.Source.LastSuccess = time.Time{}
		}
		if spec.ID == "nyfed-calendar" {
			row.Source.WindowStart = "2026-09-01"
			row.Source.WindowEnd = "2026-09-30"
			row.Batch.Events = []rpc.MacroEvent{{ID: "backup-release", SourceID: spec.ID, Date: "2026-09-11"}}
		}
		s.macro.records[spec.ID] = row
	}
	out := s.macroSnapshotWindow("2026-09-11", "2026-09-11")
	if out.CoverageStatus != "partial" || len(out.Events) != 1 || out.Events[0].SourceID != "nyfed-calendar" {
		t.Fatal("backup healed the failed primary or discarded useful release")
	}
	row := s.macro.records["bls-calendar"]
	row.Source.Availability = "available"
	row.Source.LastSuccess = now
	s.macro.records["bls-calendar"] = row
	if s.macroSnapshotWindow("2026-09-30", "2026-10-01").CoverageStatus != "partial" {
		t.Fatal("calendar claimed an unobserved next month")
	}
	now = s.macro.records["nyfed-calendar"].Source.ValidUntil
	if s.macroSnapshotWindow("2026-09-11", "2026-09-11").CoverageStatus != "partial" {
		t.Fatal("source coverage outlived evidence")
	}
}

func TestMacroTruncationIdentifiesOmittedList(t *testing.T) {
	now := time.Date(2026, 9, 10, 19, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name                             string
		events, publications, titleBytes int
		wantEvents, wantPublications     bool
	}{
		{"publication_count", 1, 13, 10, false, true},
		{"event_count", 49, 0, 10, true, false},
		{"publication_bytes", 30, 12, 500, false, true},
		{"event_bytes", 48, 0, 500, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{now: func() time.Time { return now }, macro: &macroCache{records: map[string]macroRecord{}}}
			for _, spec := range macrosource.Specs() {
				row := coldMacroRecord(spec)
				if spec.ID == "bea-calendar" {
					for i := range tc.events {
						row.Batch.Events = append(row.Batch.Events, rpc.MacroEvent{ID: fmt.Sprintf("release-%02d", i), Title: strings.Repeat("x", tc.titleBytes), SourceID: spec.ID, SourceURL: spec.URL, Date: "2026-09-11", RetrievedAt: now})
					}
				}
				if spec.ID == "bea-news" {
					for i := range tc.publications {
						row.Batch.Publications = append(row.Batch.Publications, rpc.MacroPublication{ID: fmt.Sprintf("publication-%02d", i), Title: strings.Repeat("x", tc.titleBytes), SourceID: spec.ID, SourceURL: spec.URL, PublishedAt: now, RetrievedAt: now})
					}
				}
				s.macro.records[spec.ID] = row
			}
			out := s.macroSnapshotWindow("2026-09-11", "2026-09-11")
			if out.EventsTruncated != tc.wantEvents || out.PublicationsTruncated != tc.wantPublications || out.Truncated != (out.EventsTruncated || out.PublicationsTruncated) {
				t.Fatalf("list truncation disagrees: events=%v publications=%v union=%v retained=%d/%d", out.EventsTruncated, out.PublicationsTruncated, out.Truncated, len(out.Events), len(out.Publications))
			}
			if !tc.wantEvents && len(out.Events) != tc.events {
				t.Fatal("publication limit omitted an event without reporting it")
			}
			raw, _ := json.Marshal(out)
			if len(raw) > 28<<10 || !strings.Contains(string(raw), `"events_truncated":`) || !strings.Contains(string(raw), `"publications_truncated":`) {
				t.Fatal("response exceeded bound or omitted authoritative false flags")
			}
		})
	}
}

const syntheticMacroICS = "BEGIN:VCALENDAR\r\nBEGIN:VEVENT\r\nSUMMARY:Synthetic release\r\nDTSTART:20260101T010000Z\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"

func macroSpecByID(t *testing.T, id string) macrosource.Spec {
	t.Helper()
	for _, s := range macrosource.Specs() {
		if s.ID == id {
			return s
		}
	}
	t.Fatal("fixture source absent")
	return macrosource.Spec{}
}

func macroHealthRow(t *testing.T, s *Server, id string, now time.Time) rpc.DataSourceHealth {
	t.Helper()
	rows, _ := s.collectDataHealth(now)
	for _, row := range rows {
		if row.ID == "macro:"+id {
			return row
		}
	}
	t.Fatal("macro source missing from data health")
	return rpc.DataSourceHealth{}
}

// TestMacroFailureSurfacesOnsetAndTypedCause drives the production reader
// through a BLS policy rejection streak, a restart, recovery and a transient
// outage. Health must carry the typed cause dated at the streak's onset and
// count the streak; the log records each transition once, without the body.
func TestMacroFailureSurfacesOnsetAndTypedCause(t *testing.T) {
	const denial = `<html><h1>Bureau of Labor Statistics</h1><h2>Access Denied</h2><p>bot activity that doesn&#39;t conform to BLS usage policy is prohibited.</p><p>response-only-marker</p></html>`
	store := openMarketTestCoreStore(t)
	now := time.Date(2026, 9, 22, 17, 23, 0, 0, time.UTC)
	var logs bytes.Buffer
	s := &Server{coreStore: store, now: func() time.Time { return now }, logger: &Logger{l: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))}}
	spec := macroSpecByID(t, "bls-calendar")
	status, body := http.StatusOK, syntheticMacroICS
	client := macrosource.NewClient()
	client.HTTP.Transport = macroHTTPTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})
	s.macro = s.loadMacroSources()
	s.macro.client = client
	s.refreshMacroSource(t.Context(), s.macro, spec)

	status, body = http.StatusForbidden, denial
	onset := s.macro.records[spec.ID].Source.NextAttempt
	for range 3 {
		now = s.macro.records[spec.ID].Source.NextAttempt
		s.refreshMacroSource(t.Context(), s.macro, spec)
	}
	want := rpc.SourceFailure{Code: rpc.SourceFailureProtocolRejected, Stage: rpc.SourceFailureStagePublicSourceRequest, FailedAt: onset, Retryable: false}
	row := macroHealthRow(t, s, spec.ID, now)
	if row.Failure == nil || *row.Failure != want || !rpc.ValidSourceFailure(row.Failure) {
		t.Fatalf("health failure = %+v, want %+v", row.Failure, want)
	}
	if row.State != "limited" || !strings.Contains(row.Detail, "3 consecutive failed refreshes") || row.Action == "Canary refreshes this source automatically" {
		t.Fatalf("policy rejection reported as self-healing or without its streak: %+v", row)
	}
	restarted := &Server{coreStore: store, now: func() time.Time { return now }}
	restarted.macro = restarted.loadMacroSources()
	if got := macroHealthRow(t, restarted, spec.ID, now).Failure; got == nil || *got != want {
		t.Fatalf("restart lost the typed cause or its onset: %+v", got)
	}
	if out := logs.String(); strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "macro source bls-calendar refresh failed (protocol_rejected at public_source_request, retryable=false)") || strings.Contains(out, "response-only-marker") {
		t.Fatalf("failure onset was not logged exactly once without the response body:\n%s", out)
	}

	status, body = http.StatusOK, syntheticMacroICS
	now = s.macro.records[spec.ID].Source.NextAttempt
	s.refreshMacroSource(t.Context(), s.macro, spec)
	if row := macroHealthRow(t, s, spec.ID, now); row.Failure != nil || row.State != "current" || strings.Contains(row.Detail, "failed refresh") {
		t.Fatalf("recovery kept a failure: %+v", row)
	}
	if out := logs.String(); strings.Count(out, "level=WARN") != 1 || strings.Count(out, "level=INFO") != 1 || !strings.Contains(out, "macro source bls-calendar recovered after 3 failed refreshes since "+onset.Format(time.RFC3339)) {
		t.Fatalf("recovery was not logged exactly once:\n%s", out)
	}

	status, body = http.StatusServiceUnavailable, "maintenance"
	now = s.macro.records[spec.ID].Source.NextAttempt
	s.refreshMacroSource(t.Context(), s.macro, spec)
	row = macroHealthRow(t, s, spec.ID, now)
	if row.Failure == nil || !row.Failure.Retryable || !row.Failure.FailedAt.Equal(now) || row.Action != "Canary refreshes this source automatically" || !strings.Contains(row.Detail, "1 failed refresh") {
		t.Fatalf("transient outage misclassified: %+v", row)
	}
	if strings.Count(logs.String(), "level=WARN") != 2 {
		t.Fatal("a new failure streak was not logged")
	}
}

type macroCountingFetcher struct{ calls map[string]int }

func (f *macroCountingFetcher) Fetch(_ context.Context, s macrosource.Spec, at time.Time, _ macrosource.Batch) (macrosource.Batch, error) {
	f.calls[s.ID]++
	payload := `{"Synthetic release":{"release_dates":["2026-09-10T12:30:00Z"]}}`
	if s.Kind == "ics" {
		payload = syntheticMacroICS
	}
	return macrosource.Parse(s, []byte(payload), at)
}

// TestMacroBLSRefreshesHourlyWhileOtherSourcesKeepCadence witnesses the BLS
// politeness interval: one read per hour rather than per five-minute tick, and
// a freshness window that outlives one missed read. Other sources keep their
// cadence, and a record kept under the former 30-minute window still restores.
func TestMacroBLSRefreshesHourlyWhileOtherSourcesKeepCadence(t *testing.T) {
	store := openMarketTestCoreStore(t)
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	now := start
	s := &Server{coreStore: store, now: func() time.Time { return now }}
	s.macro = s.loadMacroSources()
	fetcher := &macroCountingFetcher{calls: map[string]int{}}
	s.macro.client = fetcher
	specs := []macrosource.Spec{macroSpecByID(t, "bls-calendar"), macroSpecByID(t, "bea-calendar")}
	for tick := range 13 {
		now = start.Add(time.Duration(tick) * 5 * time.Minute)
		for _, spec := range specs {
			s.refreshMacroSource(t.Context(), s.macro, spec)
		}
	}
	if fetcher.calls["bls-calendar"] != 2 || fetcher.calls["bea-calendar"] != 13 {
		t.Fatalf("reads over one hour of ticks = %v, want BLS 2 and BEA 13", fetcher.calls)
	}
	bls, bea := s.macro.records["bls-calendar"].Source, s.macro.records["bea-calendar"].Source
	if bls.NextAttempt.Sub(bls.LastSuccess) != time.Hour || bls.ValidUntil.Sub(bls.LastSuccess) != 3*time.Hour {
		t.Fatalf("BLS schedule next=%s valid=%s after success", bls.NextAttempt.Sub(bls.LastSuccess), bls.ValidUntil.Sub(bls.LastSuccess))
	}
	if bea.NextAttempt.Sub(bea.LastSuccess) != 5*time.Minute || bea.ValidUntil.Sub(bea.LastSuccess) != 30*time.Minute {
		t.Fatal("another source's cadence or freshness changed")
	}
	if restored := s.loadMacroSources().records["bls-calendar"]; restored.Source.Availability != "available" || !restored.Source.NextAttempt.Equal(bls.NextAttempt) {
		t.Fatal("restart rejected the hourly schedule or renewed acquisition")
	}
	legacy := s.macro.records["bls-calendar"]
	legacy.Source.ValidUntil = legacy.Source.LastSuccess.Add(30 * time.Minute)
	raw, _ := json.Marshal(legacy)
	if err := saveMarketDocument(t.Context(), store, "public-macro:bls-calendar", macroStateKind, raw); err != nil {
		t.Fatal(err)
	}
	if got := s.loadMacroSources().records["bls-calendar"]; len(got.Batch.Events) == 0 || !got.Source.ValidUntil.Equal(legacy.Source.ValidUntil) {
		t.Fatal("a record kept under the former window was discarded or gained freshness")
	}
}

// TestMacroConditionalRefreshKeepsBatchAcrossRestart serves the BLS calendar
// over TLS with cache validators. Unchanged content answers 304; the retained
// batch must be renewed coherently with the persisted envelope, survive
// restart with its validators, and yield to a revised calendar.
func TestMacroConditionalRefreshKeepsBatchAcrossRestart(t *testing.T) {
	const firstTag, modified = `"synthetic-v1"`, "Wed, 10 Jun 2026 16:56:37 GMT"
	var mu sync.Mutex
	var seen []http.Header
	content, tag := syntheticMacroICS, firstTag
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, r.Header.Clone())
		if r.Host != "www.bls.gov" || r.URL.Path != "/schedule/news_release/bls.ics" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		if r.Header.Get("If-None-Match") == tag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", tag)
		w.Header().Set("Last-Modified", modified)
		_, _ = io.WriteString(w, content)
	}))
	defer srv.Close()
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return new(net.Dialer).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	client := macrosource.NewClient()
	client.HTTP.Transport = transport

	store := openMarketTestCoreStore(t)
	start := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	now := start
	s := &Server{coreStore: store, now: func() time.Time { return now }}
	s.macro = s.loadMacroSources()
	s.macro.client = client
	spec := macroSpecByID(t, "bls-calendar")
	s.refreshMacroSource(t.Context(), s.macro, spec)
	first := s.macro.records[spec.ID]
	if first.Source.Availability != "available" || first.Batch.ETag != firstTag || first.Batch.LastModified != modified {
		t.Fatalf("validators were not retained with the batch: %+v", first.Batch)
	}

	now = first.Source.NextAttempt
	s.refreshMacroSource(t.Context(), s.macro, spec)
	renewed := s.macro.records[spec.ID]
	if !renewed.Source.LastSuccess.Equal(now) || !renewed.Source.ValidUntil.Equal(now.Add(spec.Freshness)) || renewed.Batch.ETag != firstTag || renewed.Batch.Events[0].ID != first.Batch.Events[0].ID || !renewed.Batch.Events[0].RetrievedAt.Equal(now) {
		t.Fatalf("304 did not renew the retained batch coherently: %+v", renewed.Source)
	}
	if !first.Batch.Events[0].RetrievedAt.Equal(start) {
		t.Fatal("revalidation mutated evidence shared with concurrent readers")
	}

	restarted := &Server{coreStore: store, now: func() time.Time { return now }}
	restarted.macro = restarted.loadMacroSources()
	restarted.macro.client = client
	restored := restarted.macro.records[spec.ID]
	if restored.Source.Availability != "available" || restored.Batch.ETag != firstTag || !restored.Source.LastSuccess.Equal(now) {
		t.Fatalf("restart discarded the revalidated record or its validators: %+v", restored.Source)
	}
	now = restored.Source.NextAttempt
	restarted.refreshMacroSource(t.Context(), restarted.macro, spec)
	if got := restarted.macro.records[spec.ID]; !got.Source.LastSuccess.Equal(now) || got.Batch.Events[0].ID != first.Batch.Events[0].ID {
		t.Fatal("restored validators did not renew the batch")
	}

	mu.Lock()
	content, tag = strings.Replace(syntheticMacroICS, "20260101T010000Z", "20260102T150000Z", 1), `"synthetic-v2"`
	mu.Unlock()
	now = restarted.macro.records[spec.ID].Source.NextAttempt
	restarted.refreshMacroSource(t.Context(), restarted.macro, spec)
	revised := restarted.loadMacroSources().records[spec.ID]
	if revised.Batch.ETag != `"synthetic-v2"` || len(revised.Batch.Events) != 1 || revised.Batch.Events[0].Date != "2026-01-02" || !revised.Source.LastSuccess.Equal(now) {
		t.Fatalf("revised calendar did not replace the validated batch: %+v", revised.Batch)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 4 || seen[0].Get("If-None-Match") != "" || seen[0].Get("If-Modified-Since") != "" {
		t.Fatalf("requests = %d; first read must be unconditional", len(seen))
	}
	for _, h := range seen[1:] {
		if h.Get("If-None-Match") != firstTag || h.Get("If-Modified-Since") != modified {
			t.Fatalf("conditional read sent validators %q / %q", h.Get("If-None-Match"), h.Get("If-Modified-Since"))
		}
	}
	for _, h := range seen {
		if h.Get("User-Agent") != "Canary-public-feeds/1.0 (+https://osauer.dev/canary/)" || h.Get("Cookie") != "" || h.Get("Authorization") != "" {
			t.Fatalf("BLS request identity on the wire = %q", h.Get("User-Agent"))
		}
	}
}
