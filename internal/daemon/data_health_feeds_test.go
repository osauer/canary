package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/broker"
	"github.com/osauer/canary/v3/internal/rpc"
	ibkr "github.com/osauer/canary/v3/pkg/ibkr"
)

func TestDataHealthInstrumentAbsenceDoesNotCreateOrDegradeSource(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	live := &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataLive, ReceivedAt: now}
	s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_LIVE"}, live, nil, nil, broker.SessionBinding{})
	before := s.feedDataHealth(quoteSourceID, now)
	for i := range 100 {
		contract := rpc.ContractParams{Symbol: fmt.Sprintf("SYNTHETIC_INACTIVE_%d", i)}
		s.observeQuoteHealth(contract, nil, ibkr.ErrSymbolInactive, nil, broker.SessionBinding{})
		s.observeQuoteHealth(contract, nil, nil, nil, broker.SessionBinding{})
		s.rememberMarketHistory(rpc.MarketHistoryParams{Contract: rpc.ContractParams{Symbol: contract.Symbol, SecType: "STK", Exchange: "SMART", Currency: "USD"}, Range: "1Y"})
	}
	if after := s.feedDataHealth(quoteSourceID, now); !reflect.DeepEqual(before, after) || after.State != "current" {
		t.Fatalf("instrument absence changed source: %+v", after)
	}
	if len(s.dataHealth.observations) != 1 || len(s.dataHealth.feedObservations[quoteSourceID]) != 1 {
		t.Fatal("health retained per-instrument state")
	}
	rows, _ := s.collectDataHealth(now)
	for _, row := range rows {
		if instrumentHealthID(row.ID) || row.ID == "portfolio_scope" {
			t.Fatal("instrument requirement entered source catalogue")
		}
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), "SYNTHETIC_") {
		t.Fatal("instrument identity escaped into health report")
	}
	if len(s.marketData.interest) == 0 {
		t.Fatal("source fix removed the actual chart owner's interests")
	}
}

func TestDataHealthDelayedReceiptKeepsAccessInformational(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	delayed := projectQuoteSource(&rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataDelayed, ReceivedAt: now}, nil, now)
	s.recordFeedHealth(delayed, nil, broker.SessionBinding{})
	if row := s.feedDataHealth(quoteSourceID, now); row.State != "current" || len(row.ProblemIDs) != 0 || row.DataType != rpc.MarketDataDelayed {
		t.Fatalf("usable delayed receipt became a source problem: %+v", row)
	}
	delayed.Access = &rpc.DataAccessObservation{Code: 354, Reason: "not_subscribed", ObservedAt: now, RetryAt: now.Add(30 * time.Minute)}
	s.recordFeedHealth(delayed, nil, broker.SessionBinding{})
	if row := s.feedDataHealth(quoteSourceID, now); row.State != "current" || len(row.ProblemIDs) != 0 || row.Access == nil {
		t.Fatalf("usable delayed fallback became a source problem or lost access evidence: %+v", row)
	}
	now = now.Add(time.Minute)
	s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_OTHER_DELAYED"}, &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataDelayed, ReceivedAt: now}, nil, nil, broker.SessionBinding{})
	if row := s.feedDataHealth(quoteSourceID, now); row.Access == nil || row.Access.Code != 354 || !row.NextAttempt.Equal(delayed.Access.RetryAt) {
		t.Fatal("same-mode success hid another segment's source restriction")
	}
	s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_LIVE"}, &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataLive, ReceivedAt: now}, nil, nil, broker.SessionBinding{})
	row := s.feedDataHealth(quoteSourceID, now)
	if row.State != "current" || row.DataType != "mixed" || row.Access == nil || row.Access.Code != 354 || len(row.ProblemIDs) != 0 {
		t.Fatalf("mixed receipt became a source problem or lost access evidence: %+v", row)
	}
	if !row.SourceAt.IsZero() || !strings.Contains(row.Receiving, "delayed") || !strings.Contains(row.Receiving, "live") {
		t.Fatal("aggregate invented an instrument clock or lost modes")
	}
	report, err := finalizeDataHealth([]rpc.DataSourceHealth{row}, "current", now, rpc.DataHealthParams{})
	if err != nil || report.Summary.State != "current" || report.Summary.Problems != 0 || report.Summary.Limited != 0 || len(report.Concerns) != 0 {
		t.Fatalf("informational feed mode became an aggregate warning: %+v, %v", report, err)
	}
	now = now.Add(5 * time.Minute)
	if row = s.feedDataHealth(quoteSourceID, now); row.State != "unknown" || row.Access != nil {
		t.Fatal("expired evidence established availability or entitlement")
	}
}

// One instrument's problem beside a working feed belongs to that instrument,
// not to the source (owner instruction 2026-11-21 08:38 CET: "one instrument
// failing should not mark the data sources as unhealthy"). An untyped line, a
// request that timed out and an instrument the account is not entitled to stay
// visible on the row, its failure and its access evidence, but the row reads
// current. On 2026-11-21 one held option line with only TWS's model mark held
// the required quote source limited for a whole daemon run.
func TestDataHealthOneInstrumentProblemStaysOnTheInstrument(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name    string
		quote   *rpc.Quote
		err     error
		access  *rpc.DataAccessObservation
		wording string
	}{
		{name: "unknown mode", quote: &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataUnknown, ReceivedAt: now}, wording: "unverified mode"},
		{name: "request timeout", err: context.DeadlineExceeded, wording: "some instrument requests failed"},
		{name: "denied without fallback", quote: &rpc.Quote{}, access: &rpc.DataAccessObservation{Code: 354, Reason: "not_subscribed", ObservedAt: now}, wording: "some instrument requests failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{now: func() time.Time { return now }}
			s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_DELAYED"}, &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataDelayed, ReceivedAt: now}, nil, nil, broker.SessionBinding{})
			problem := projectQuoteSource(tc.quote, tc.err, now)
			problem.Access = tc.access
			s.recordFeedHealth(problem, nil, broker.SessionBinding{})
			// The service then answers another instrument.
			later := now.Add(time.Second)
			s.now = func() time.Time { return later }
			s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_OTHER"}, &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataDelayed, ReceivedAt: later}, nil, nil, broker.SessionBinding{})
			row := s.feedDataHealth(quoteSourceID, later)
			if row.State != "current" || len(row.ProblemIDs) != 0 || !strings.Contains(row.Receiving, "delayed") || !strings.Contains(row.Receiving, tc.wording) {
				t.Fatalf("one instrument's problem degraded the source or vanished: %+v", row)
			}
			if tc.err != nil && (row.Failure == nil || row.Failure.Code != rpc.SourceFailureTimeout) {
				t.Fatalf("timeout evidence lost: %+v", row)
			}
			if tc.access != nil && (row.Access == nil || row.Failure == nil || row.Failure.Code != rpc.SourceFailureNotEntitled) {
				t.Fatalf("entitlement evidence lost: %+v", row)
			}
			report, err := finalizeDataHealth([]rpc.DataSourceHealth{row}, "current", later, rpc.DataHealthParams{})
			if err != nil || report.Summary.State != "current" || report.Summary.Problems != 0 || len(report.Concerns) != 0 {
				t.Fatalf("one instrument's problem reached the aggregate: %+v, %v", report, err)
			}
		})
	}
}

// What the service itself does still marks it: every line untyped, a
// service-wide failure beside a success, a declared history stall beside an
// older success, and a failure with nothing received.
func TestDataHealthServiceProblemsStillMarkTheSource(t *testing.T) {
	now := time.Now()
	t.Run("every line untyped", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now }}
		s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_A"}, &rpc.Quote{Price: new(100.0), ReceivedAt: now}, nil, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(quoteSourceID, now); row.State != "limited" || len(row.ProblemIDs) != 1 {
			t.Fatalf("an untyped feed read healthy: %+v", row)
		}
	})
	t.Run("service-wide failure beside a success", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now }}
		s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_A"}, &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataFrozen, ReceivedAt: now}, nil, nil, broker.SessionBinding{})
		s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_B"}, nil, ibkr.ErrIBKRUnavailable, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(quoteSourceID, now); row.State != "limited" || len(row.ProblemIDs) != 1 {
			t.Fatalf("a gateway failure beside a success read healthy: %+v", row)
		}
	})
	t.Run("declared history stall beside an older success", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now.Add(-10 * time.Minute) }}
		s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
		s.now = func() time.Time { return now }
		s.observeHistorySource(false, ibkr.ErrHistoricalServiceStalled, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(historySourceID, now); row.State == "current" || len(row.ProblemIDs) != 1 {
			t.Fatalf("a declared stall hid behind a ten-minute-old success: %+v", row)
		}
	})
	t.Run("one slow history request beside a success", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now }}
		s.observeHistorySource(false, context.DeadlineExceeded, nil, broker.SessionBinding{})
		s.now = func() time.Time { return now.Add(time.Second) }
		s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(historySourceID, now.Add(time.Second)); row.State != "current" || len(row.ProblemIDs) != 0 {
			t.Fatalf("one timed-out history request degraded the source: %+v", row)
		}
	})
	t.Run("stall refusal then the probe's ordinary timeout", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now.Add(-10 * time.Minute) }}
		s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
		s.now = func() time.Time { return now.Add(-time.Minute) }
		s.observeHistorySource(false, ibkr.ErrHistoricalServiceStalled, nil, broker.SessionBinding{})
		s.now = func() time.Time { return now }
		s.observeHistorySource(false, context.DeadlineExceeded, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(historySourceID, now); row.State == "current" {
			t.Fatalf("the probe's timeout cleared a stall: %+v", row)
		}
	})
	t.Run("a history answer after a stall ends it", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now.Add(-time.Minute) }}
		s.observeHistorySource(false, ibkr.ErrHistoricalServiceStalled, nil, broker.SessionBinding{})
		s.now = func() time.Time { return now }
		s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(historySourceID, now); row.State != "current" {
			t.Fatalf("a stall outlived the answer that ended it: %+v", row)
		}
	})
	t.Run("every request failing beside an old success", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now.Add(-20 * time.Minute) }}
		s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
		s.now = func() time.Time { return now }
		s.observeHistorySource(false, ibkr.ErrContractDetailsTimeout, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(historySourceID, now); row.State == "current" || len(row.ProblemIDs) != 1 {
			t.Fatalf("failures after the last success read healthy: %+v", row)
		}
	})
	t.Run("timeouts while a history farm is down", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now }, historicalFarmOutage: func() bool { return true }}
		s.observeHistorySource(false, context.DeadlineExceeded, nil, broker.SessionBinding{})
		s.now = func() time.Time { return now.Add(time.Second) }
		s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(historySourceID, now.Add(time.Second)); row.State == "current" {
			t.Fatalf("a broken history farm read healthy beside another farm's answers: %+v", row)
		}
	})
	t.Run("failure with nothing received", func(t *testing.T) {
		s := &Server{now: func() time.Time { return now }}
		s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_B"}, nil, context.DeadlineExceeded, nil, broker.SessionBinding{})
		if row := s.feedDataHealth(quoteSourceID, now); row.State != "unavailable" {
			t.Fatalf("a failing feed read available: %+v", row)
		}
	})
}

func TestDataHealthHistoryTracksServiceNotRequestedWindow(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	s.observeHistorySource(true, nil, nil, broker.SessionBinding{})
	s.observeHistorySource(false, ibkr.ErrContractNoDefinition, nil, broker.SessionBinding{})
	s.observeHistorySource(false, nil, nil, broker.SessionBinding{})
	if row := s.feedDataHealth(historySourceID, now); row.State != "current" || row.Failure != nil {
		t.Fatalf("empty instrument history poisoned service: %+v", row)
	}
	s.observeHistorySource(false, &ibkr.HistoricalRequestError{Category: rpc.SourceFailurePacing}, nil, broker.SessionBinding{})
	if row := s.feedDataHealth(historySourceID, now); row.State != "limited" || row.Failure == nil || row.Failure.Code != rpc.SourceFailurePacing {
		t.Fatalf("provider failure lost: %+v", row)
	}
}

func TestDataHealthDeniedQuoteShellKeepsSourceRestriction(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	row := projectQuoteSource(&rpc.Quote{}, nil, now)
	row.Access = &rpc.DataAccessObservation{Code: 200, ObservedAt: now}
	s.recordFeedHealth(row, nil, broker.SessionBinding{})
	if len(s.dataHealth.feedObservations) != 0 {
		t.Fatal("contract refusal became a source problem")
	}
	row.Access = &rpc.DataAccessObservation{Code: 354, Reason: "not_subscribed", ObservedAt: now}
	s.recordFeedHealth(row, nil, broker.SessionBinding{})
	got := s.feedDataHealth(quoteSourceID, now)
	if got.State == "current" || got.Failure == nil || got.Failure.Code != rpc.SourceFailureNotEntitled || got.Access == nil || len(got.ProblemIDs) != 1 {
		t.Fatalf("quote shell hid a source restriction: %+v", got)
	}
}

func TestDataHealthDiscardsLegacyInstrumentDiagnostics(t *testing.T) {
	s, _, _, now, _ := historyFixture(t)
	s.now = func() time.Time { return now }
	legacy := []dataHealthDiagnostic{{ID: "quote:legacy", FirstObserved: now}, {ID: "history:legacy", FirstObserved: now}, {ID: quoteSourceID, FirstObserved: now}}
	raw, _ := json.Marshal(legacy)
	if err := saveMarketDocument(t.Context(), s.coreStore, "data-health-diagnostics", dataHealthHistoryKind, raw); err != nil {
		t.Fatal(err)
	}
	s.loadDataHealthHistory()
	if len(s.dataHealth.observations) != 1 {
		t.Fatal("legacy instrument health was restored")
	}
	s.recordDataHealth(rpc.DataSourceHealth{ID: "quote:legacy"}, nil, broker.SessionBinding{}, false)
	s.persistDataHealthHistory(t.Context())
	raw, _, err := loadMarketState(s.coreStore, "data-health-diagnostics", dataHealthHistoryKind)
	if err != nil || strings.Contains(string(raw), "legacy") {
		t.Fatal("instrument diagnostics persisted after migration")
	}
}

func TestDataHealthCatalogueUsesProducerSourceIdentity(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	s.observeEventHealth(rpc.MarketEventsResult{AsOf: now, SourceHealth: []rpc.SourceHealth{{Source: "borrow_fee", Status: rpc.SourceStatusOK, AsOf: now, RefreshState: rpc.SourceRefreshNotDue}}}, nil, broker.SessionBinding{})
	rows, _ := s.collectDataHealth(now)
	matched := 0
	for _, row := range rows {
		if row.ID == "events:borrow_fees" {
			t.Fatal("catalogue invented a different producer identity")
		}
		if row.ID == "events:borrow_fee" {
			matched++
			if row.State != "not_due" || row.Name != "Borrow fees" || row.Provider != "IBKR" {
				t.Fatalf("producer state or source identity lost: %+v", row)
			}
		}
	}
	if matched != 1 {
		t.Fatal("source omitted or duplicated")
	}
}
