package daemon

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestDataHealthFrozenModeIsNotSourceFailure(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTH"}, &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataDelayedFrozen, ReceivedAt: now}, nil, nil, ibkr.ConnectorSessionBinding{})
	row := s.feedDataHealth(quoteSourceID, now)
	if row.State != "current" || len(row.ProblemIDs) != 0 || row.DataType != rpc.MarketDataDelayedFrozen {
		t.Fatalf("mode misclassified as source failure: %+v", row)
	}
}

func TestDataHealthBlockedGammaRemainsConcernOutsideSession(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	r := &rpc.RegimeSnapshotResult{AsOf: now}
	r.VIXTermStructure.Status, r.VolOfVol.Status = "unavailable", "unavailable"
	r.HYGSPYDivergence.Status, r.CreditSpreads.Status, r.FundingStress.Status = "unavailable", "unavailable", "unavailable"
	r.USDJPY.Status, r.Breadth.Status, r.GammaZero.Status = "unavailable", "unavailable", "ok"
	r.GammaZero.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}
	r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked}}
	r.Fingerprint = rpc.BuildRegimeFingerprint(r)
	raw, _ := json.Marshal(r)
	s := &Server{regimeSnapshots: &regimeSnapshotCache{raw: raw, revision: 1, lastSuccessAt: now, freshFor: time.Minute, now: func() time.Time { return now }}}
	if _, err := s.regimeSnapshots.current(); err != nil {
		t.Fatalf("invalid fixture: %v", err)
	}
	for _, row := range s.regimeDataHealth(now) {
		if row.ID == "regime:gamma" && (row.State != "limited" || len(row.ProblemIDs) == 0) {
			t.Fatalf("closed-session cadence hid blocked gamma: %+v", row)
		}
	}
}

func TestGammaHealthNotDueCarriesNextOptionsOpen(t *testing.T) {
	for _, tc := range []struct{ name, at, retainedAt, want string }{
		{name: "before_open", at: "2026-09-24 07:00", retainedAt: "2026-09-23 16:05", want: "2026-09-24 09:30"},
		{name: "weekend", at: "2026-09-25 17:00", retainedAt: "2026-09-25 16:05", want: "2026-09-28 09:30"},
		{name: "thanksgiving", at: "2026-11-25 17:00", retainedAt: "2026-11-25 16:05", want: "2026-11-27 09:30"},
		{name: "labor_day_weekend", at: "2026-09-05 12:00", retainedAt: "2026-09-04 16:05", want: "2026-09-08 09:30"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := gammaRolloverNY(t, tc.at)
			r := &rpc.RegimeSnapshotResult{AsOf: now}
			r.VIXTermStructure.Status, r.VolOfVol.Status = "unavailable", "unavailable"
			r.HYGSPYDivergence.Status, r.CreditSpreads.Status, r.FundingStress.Status = "unavailable", "unavailable", "unavailable"
			r.USDJPY.Status, r.Breadth.Status, r.GammaZero.Status = "unavailable", "unavailable", rpc.RegimeStatusStale
			r.GammaZero.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessNotDue}
			r.GammaZero.Envelope = rpc.GammaZeroSPXResult{Status: rpc.GammaZeroStatusReady, Result: &rpc.GammaZeroComputed{
				AsOf: gammaRolloverNY(t, tc.retainedAt), Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityBlocked},
			}}
			r.Fingerprint = rpc.BuildRegimeFingerprint(r)
			raw, _ := json.Marshal(r)
			s := &Server{regimeSnapshots: &regimeSnapshotCache{raw: raw, revision: 1, lastSuccessAt: now, freshFor: time.Minute, now: func() time.Time { return now }}}
			if _, err := s.regimeSnapshots.current(); err != nil {
				t.Fatalf("invalid fixture: %v", err)
			}
			want := gammaRolloverNY(t, tc.want)
			for _, row := range s.regimeDataHealth(now) {
				if row.ID != "regime:gamma" {
					continue
				}
				if row.CadenceState != "not_due" || row.Usability != "blocked" {
					t.Fatalf("fixture is not a blocked not-due gamma row: %+v", row)
				}
				if !row.NextAttempt.Equal(want) {
					t.Fatalf("next attempt = %v, want the next options open %v", row.NextAttempt, want)
				}
				return
			}
			t.Fatal("regime:gamma row missing")
		})
	}
}

func TestDataHealthHistoryKeepsWeekAndActualSuccessClock(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	observedAt := now
	s := &Server{now: func() time.Time { return observedAt }}
	for i := range 12 {
		observedAt = now.Add(time.Duration(i) * time.Hour)
		row := rpc.DataSourceHealth{ID: "synthetic", State: "current", CheckedAt: now.Add(time.Duration(i) * time.Hour), ReceivedAt: now}
		if i%2 != 0 {
			row.State = "not_due"
		}
		s.recordDataHealth(row, nil, ibkr.ConnectorSessionBinding{}, false)
	}
	row, _ := s.observedDataHealth("synthetic", now.Add(12*time.Hour))
	if len(row.History) != 12 {
		t.Fatalf("retained chronology too short for diagnosis: %d", len(row.History))
	}
	if row.LastSuccess != now {
		t.Fatalf("reading/not-due renewed the success clock: %s", row.LastSuccess)
	}
}

func TestDataHealthMaxHistoryPagination(t *testing.T) {
	now := time.Now().UTC()
	var rows []rpc.DataSourceHealth
	for i := range 36 {
		row := rpc.DataSourceHealth{ID: fmt.Sprintf("synthetic:%02d", i), Name: "Synthetic provider", State: "unknown", Required: true}
		for j := range 128 {
			row.History = append(row.History, rpc.DataHealthTransition{At: now.Add(-time.Duration(j) * time.Minute), State: "unavailable", DataType: "delayed-frozen", Reason: strings.Repeat("x", 64)})
		}
		rows = append(rows, row)
	}
	params := rpc.DataHealthParams{Limit: 24}
	seen := make(map[string]bool)
	for {
		page, err := finalizeDataHealth(rows, "current", now, params)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(page)
		if len(raw) > 30*1024 {
			t.Fatalf("page exceeds consumer envelope: %d", len(raw))
		}
		if page.Summary.Total != len(rows) {
			t.Fatal("pagination truncated source catalogue")
		}
		for _, row := range page.Sources {
			if seen[row.ID] || len(row.History) != 128 {
				t.Fatal("lost/duplicated history or row")
			}
			seen[row.ID] = true
		}
		if page.Complete {
			break
		}
		if page.NextOffset == nil || *page.NextOffset <= params.Offset {
			t.Fatal("page does not advance")
		}
		params.Revision, params.Offset = page.Revision, *page.NextOffset
	}
	if len(seen) != len(rows) {
		t.Fatal("source coverage lost")
	}
}

func TestDataHealthRestartGapSurvivesOldProducerClock(t *testing.T) {
	s, _, _, now, _ := historyFixture(t)
	s.now = func() time.Time { return now }
	old := now.Add(-time.Hour)
	records := []dataHealthDiagnostic{{ID: "macro:synthetic", FirstObserved: old.Add(-time.Hour), LastSuccess: old, Transitions: []rpc.DataHealthTransition{{At: old, State: "limited", Reason: "timeout"}}}}
	raw, _ := json.Marshal(records)
	if err := saveMarketDocument(t.Context(), s.coreStore, "data-health-diagnostics", dataHealthHistoryKind, raw); err != nil {
		t.Fatal(err)
	}
	s.loadDataHealthHistory()
	s.recordDataHealth(rpc.DataSourceHealth{ID: "macro:synthetic", State: "limited", CheckedAt: old, ReceivedAt: old}, nil, ibkr.ConnectorSessionBinding{}, false)
	got, _ := s.observedDataHealth("macro:synthetic", now)
	if len(got.History) != 3 || got.History[1].Reason != "daemon_restart_observation_gap" || got.History[2].At != now || got.LastSuccess != old || !got.HistoryTruncated {
		t.Fatalf("old attempt erased/reordered restart evidence or hid legacy truncation: %+v", got)
	}
	now = now.Add(8 * 24 * time.Hour)
	got, _ = s.observedDataHealth("macro:synthetic", now)
	if len(got.History) != 0 || !got.HistoryTruncated {
		t.Fatal("history horizon or truncation lost")
	}
}

func TestDataHealthHistoryLimitsAndOversizeAreExplicit(t *testing.T) {
	now := time.Now().UTC()
	s := &Server{now: func() time.Time { return now }}
	for i := range 160 {
		now = now.Add(time.Minute)
		state := "current"
		if i%2 != 0 {
			state = "limited"
		}
		s.recordDataHealth(rpc.DataSourceHealth{ID: "synthetic", State: state, CheckedAt: now}, nil, ibkr.ConnectorSessionBinding{}, false)
	}
	row, _ := s.observedDataHealth("synthetic", now)
	if len(row.History) != 128 || !row.HistoryTruncated {
		t.Fatal("bounded history truncation hidden")
	}
	row.Detail = strings.Repeat("x", dataHealthPageBytes)
	if _, err := finalizeDataHealth([]rpc.DataSourceHealth{row}, "current", now, rpc.DataHealthParams{}); err == nil {
		t.Fatal("single oversized source silently emitted/dropped")
	}
}

func TestDataHealthFailureDoesNotClaimAvailable(t *testing.T) {
	now := time.Now().UTC()
	health := rpc.SourceHealth{Status: rpc.SourceStatusOK, AsOf: now.Add(-time.Minute), LastFailure: &rpc.SourceFailure{Code: rpc.SourceFailureTimeout, FailedAt: now}, RefreshState: rpc.SourceRefreshNotDue}
	row := projectSourceHealth("synthetic", "Synthetic", "test", "market_events", health, now)
	if row.State != "limited" || row.Availability != "unavailable" || row.CadenceState != "not_due" {
		t.Fatalf("schedule hid failed acquisition: %+v", row)
	}
	health.Applicability = "not_relevant"
	row = projectSourceHealth("synthetic", "Synthetic", "test", "market_events", health, now)
	if row.Required || row.State != "not_relevant" || row.Failure == nil || !row.ReceivedAt.IsZero() {
		t.Fatal("irrelevant scope established provider recovery")
	}
}

func TestDataHealthInventoryCannotOutliveBrokerSession(t *testing.T) {
	now := time.Now().UTC()
	s := &Server{now: func() time.Time { return now }}
	result := rpc.MarketEventsResult{AsOf: now, SourceHealth: []rpc.SourceHealth{
		{Source: "borrow_inventory", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: 120},
		{Source: "borrow_fee", Status: rpc.SourceStatusOK, AsOf: now, MaxAgeSeconds: 5400},
	}}
	connector := ibkr.NewConnector(&ibkr.ConnectorConfig{})
	s.observeEventHealth(result, connector, ibkr.ConnectorSessionBinding{})
	observation := s.dataHealth.observations["events:borrow_inventory"]
	if !observation.broker || observation.connector != connector {
		t.Fatal("inventory health lost acquisition session ownership")
	}
	// There is no current binding after session loss. A just-received row's
	// remaining clock lifetime alone must not establish current availability.
	row, _ := s.observedDataHealth("events:borrow_inventory", now.Add(time.Second))
	if row.State != "unknown" || row.Availability != "unknown" {
		t.Fatalf("inventory health survived its broker session: %+v", row)
	}
	fee, _ := s.observedDataHealth("events:borrow_fee", now.Add(time.Second))
	if fee.State != "current" {
		t.Fatal("broker session incorrectly invalidated independent FTP evidence")
	}
}

func TestDataHealthGammaUsabilityRequiresCurrentPublicationAndInput(t *testing.T) {
	now := time.Date(2026, 9, 24, 16, 0, 0, 0, time.UTC)
	for _, kind := range []string{"current", "failed_publication", "stale_publication", "stale_input", "unavailable_input", "future_input"} {
		t.Run(kind, func(t *testing.T) {
			r := &rpc.RegimeSnapshotResult{AsOf: now}
			r.VIXTermStructure.Status, r.VolOfVol.Status = "unavailable", "unavailable"
			r.HYGSPYDivergence.Status, r.CreditSpreads.Status, r.FundingStress.Status = "unavailable", "unavailable", "unavailable"
			r.USDJPY.Status, r.Breadth.Status, r.GammaZero.Status = "unavailable", "unavailable", "ok"
			r.GammaZero.AsOf = &rpc.RegimeAsOfSummary{Time: now}
			r.GammaZero.Freshness = &rpc.RegimeFreshness{Class: rpc.RegimeFreshnessFresh, MaxAgeSeconds: 300}
			r.GammaZero.Envelope.Result = &rpc.GammaZeroComputed{AsOf: now, Quality: &rpc.GammaSignalQuality{Rankability: rpc.GammaRankabilityRankable}}
			cache := &regimeSnapshotCache{revision: 1, lastSuccessAt: now, freshFor: time.Minute, now: func() time.Time { return now }}
			switch kind {
			case "failed_publication":
				cache.failureCode = rpc.RegimeAuthorityFailureRefreshFailed
			case "stale_publication":
				cache.lastSuccessAt = now.Add(-time.Hour)
			case "stale_input":
				r.GammaZero.AsOf.Time = now.Add(-time.Hour)
			case "future_input":
				r.GammaZero.AsOf.Time = now.Add(time.Minute)
			case "unavailable_input":
				r.GammaZero.Status = "unavailable"
			}
			r.Fingerprint = rpc.BuildRegimeFingerprint(r)
			cache.fingerprint = r.Fingerprint
			cache.raw, _ = json.Marshal(r)
			s := &Server{regimeSnapshots: cache}
			for _, row := range s.regimeDataHealth(now) {
				if row.ID != "regime:gamma" {
					continue
				}
				want := "unknown"
				if kind == "current" {
					want = "usable"
				}
				if row.Usability != want {
					t.Fatalf("%s publication/input usability: %+v", kind, row)
				}
			}
		})
	}
}
