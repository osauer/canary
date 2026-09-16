package daemon

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkr "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestDataHealthInstrumentAbsenceDoesNotCreateOrDegradeSource(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	live := &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataLive, ReceivedAt: now}
	s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_LIVE"}, live, nil, nil, ibkr.ConnectorSessionBinding{})
	before := s.feedDataHealth(quoteSourceID, now)
	for i := range 100 {
		contract := rpc.ContractParams{Symbol: fmt.Sprintf("SYNTHETIC_INACTIVE_%d", i)}
		s.observeQuoteHealth(contract, nil, ibkr.ErrSymbolInactive, nil, ibkr.ConnectorSessionBinding{})
		s.observeQuoteHealth(contract, nil, nil, nil, ibkr.ConnectorSessionBinding{})
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

func TestDataHealthSourceKeepsRestrictionsBesideLiveSuccess(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	delayed := projectQuoteSource(&rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataDelayed, ReceivedAt: now}, nil, now)
	delayed.Access = &rpc.DataAccessObservation{Code: 354, Reason: "not_subscribed", ObservedAt: now, RetryAt: now.Add(30 * time.Minute)}
	s.recordFeedHealth(delayed, nil, ibkr.ConnectorSessionBinding{})
	now = now.Add(time.Minute)
	s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_OTHER_DELAYED"}, &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataDelayed, ReceivedAt: now}, nil, nil, ibkr.ConnectorSessionBinding{})
	if row := s.feedDataHealth(quoteSourceID, now); row.Access == nil || row.Access.Code != 354 || !row.NextAttempt.Equal(delayed.Access.RetryAt) {
		t.Fatal("same-mode success hid another segment's source restriction")
	}
	s.observeQuoteHealth(rpc.ContractParams{Symbol: "SYNTHETIC_LIVE"}, &rpc.Quote{Price: new(100.0), DataType: rpc.MarketDataLive, ReceivedAt: now}, nil, nil, ibkr.ConnectorSessionBinding{})
	row := s.feedDataHealth(quoteSourceID, now)
	if row.State != "limited" || row.DataType != "mixed" || row.Access == nil || row.Access.Code != 354 || len(row.ProblemIDs) != 1 {
		t.Fatalf("live receipt hid source restriction: %+v", row)
	}
	if !row.SourceAt.IsZero() || !strings.Contains(row.Receiving, "delayed") || !strings.Contains(row.Receiving, "live") {
		t.Fatal("aggregate invented an instrument clock or lost modes")
	}
	now = now.Add(5 * time.Minute)
	if row = s.feedDataHealth(quoteSourceID, now); row.State != "unknown" || row.Access != nil {
		t.Fatal("expired evidence established availability or entitlement")
	}
}

func TestDataHealthHistoryTracksServiceNotRequestedWindow(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	s.observeHistorySource(true, nil, nil, ibkr.ConnectorSessionBinding{})
	s.observeHistorySource(false, ibkr.ErrContractNoDefinition, nil, ibkr.ConnectorSessionBinding{})
	s.observeHistorySource(false, nil, nil, ibkr.ConnectorSessionBinding{})
	if row := s.feedDataHealth(historySourceID, now); row.State != "current" || row.Failure != nil {
		t.Fatalf("empty instrument history poisoned service: %+v", row)
	}
	s.observeHistorySource(false, &ibkr.HistoricalRequestError{Category: rpc.SourceFailurePacing}, nil, ibkr.ConnectorSessionBinding{})
	if row := s.feedDataHealth(historySourceID, now); row.State != "limited" || row.Failure == nil || row.Failure.Code != rpc.SourceFailurePacing {
		t.Fatalf("provider failure lost: %+v", row)
	}
}

func TestDataHealthDeniedQuoteShellKeepsSourceRestriction(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	row := projectQuoteSource(&rpc.Quote{}, nil, now)
	row.Access = &rpc.DataAccessObservation{Code: 200, ObservedAt: now}
	s.recordFeedHealth(row, nil, ibkr.ConnectorSessionBinding{})
	if len(s.dataHealth.feedObservations) != 0 {
		t.Fatal("contract refusal became a source problem")
	}
	row.Access = &rpc.DataAccessObservation{Code: 354, Reason: "not_subscribed", ObservedAt: now}
	s.recordFeedHealth(row, nil, ibkr.ConnectorSessionBinding{})
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
	s.recordDataHealth(rpc.DataSourceHealth{ID: "quote:legacy"}, nil, ibkr.ConnectorSessionBinding{}, false)
	s.persistDataHealthHistory(t.Context())
	raw, _, err := loadMarketState(s.coreStore, "data-health-diagnostics", dataHealthHistoryKind)
	if err != nil || strings.Contains(string(raw), "legacy") {
		t.Fatal("instrument diagnostics persisted after migration")
	}
}

func TestDataHealthCatalogueUsesProducerSourceIdentity(t *testing.T) {
	now := time.Now()
	s := &Server{now: func() time.Time { return now }}
	s.observeEventHealth(rpc.MarketEventsResult{AsOf: now, SourceHealth: []rpc.SourceHealth{{Source: "borrow_fee", Status: rpc.SourceStatusOK, AsOf: now, RefreshState: rpc.SourceRefreshNotDue}}})
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
