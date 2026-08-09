package rpc

import (
	"bytes"
	"encoding/json"
	"github.com/osauer/canary/v2/internal/risk"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCompactPositionsRiskOptionHealthAndHedge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 8, 45, 0, 0, time.UTC)
	nlv := 100_000.0
	aaplDollarDelta := 100_000.0
	spyDollarDelta := -25_000.0
	dailyLoss := -600.0
	delta := -0.20
	theta := -5.0
	p := PositionsResult{
		DataType:  MarketDataFrozen,
		AsOf:      now,
		AccountID: "DU123",
		Stocks: []PositionView{{
			Symbol:       "AAPL",
			SecType:      "STK",
			DataType:     MarketDataFrozen,
			QuoteQuality: "stale",
			Stale:        true,
		}},
		Options: []PositionView{{
			Symbol:       "AAPL",
			SecType:      "OPT",
			Expiry:       "20260605",
			Right:        "P",
			Strike:       190,
			Quantity:     -1,
			Multiplier:   100,
			MarketValue:  -1_200,
			DailyPnLBase: &dailyLoss,
			Delta:        &delta,
			Theta:        &theta,
			DataType:     MarketDataClosed,
			QuoteQuality: "stale",
			PriceAt:      now.Add(-18 * time.Hour),
			WarningDetails: []DataWarning{{
				Code:     "options_closed",
				Severity: "info",
			}},
			MarkOutsideBidAsk: true,
		}},
		Portfolio: &PositionsPortfolio{
			GreeksCoverage:     0,
			GreeksTotal:        1,
			NetLiquidationBase: &nlv,
			ExposureBase: []UnderlyingExposure{
				{Underlying: "AAPL", MarketValueBase: 50_000, DollarDeltaBase: &aaplDollarDelta},
				{Underlying: "SPY", MarketValueBase: -10_000, DollarDeltaBase: &spyDollarDelta},
			},
		},
	}

	out := CompactPositionsRisk(&p, 5)
	health := out.OptionHealth
	if health.GreeksCoverage != 0 || health.GreeksTotal != 1 {
		t.Fatalf("greeks coverage = %d/%d, want 0/1", health.GreeksCoverage, health.GreeksTotal)
	}
	if health.MissingGreeksCount != 1 {
		t.Fatalf("missing greeks count = %d, want 1", health.MissingGreeksCount)
	}
	if health.LowDTECount != 1 || health.OptionsClosedCount != 1 || health.MarkOutsideBidAskCount != 1 || health.LargeStaleDailyLossCount != 1 {
		t.Fatalf("option health counts = %+v, want one low-DTE/options-closed/mark-outside/stale-loss flag", health)
	}
	if health.FlaggedLegCount != 1 || health.FlaggedLegsReturned != 1 || len(out.FlaggedOptionLegs) != 1 {
		t.Fatalf("flagged legs = count %d returned %d len %d, want 1/1/1", health.FlaggedLegCount, health.FlaggedLegsReturned, len(out.FlaggedOptionLegs))
	}
	reasons := out.FlaggedOptionLegs[0].Reasons
	for _, want := range []string{"low_dte", "missing_greeks", "options_closed", "mark_outside_bid_ask", "large_stale_daily_loss"} {
		if !slices.Contains(reasons, want) {
			t.Fatalf("flagged option reasons missing %q: %v", want, reasons)
		}
	}
	if out.SPYHedgeOffsetPct == nil || math.Abs(*out.SPYHedgeOffsetPct-25.0) > 0.01 {
		t.Fatalf("SPY hedge offset = %v, want 25%%", out.SPYHedgeOffsetPct)
	}
}

func TestCompactStressAlertPayloadAtLeastHalfSmallerThanFull(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 8, 45, 0, 0, time.UTC)
	full := StressResult{
		AsOf:               now,
		Fingerprint:        Fingerprint{Version: StressFingerprintVersion, Key: "sha256:canary"},
		SourceFingerprints: StressSourceFingerprints{Account: &Fingerprint{Version: AccountFingerprintVersion, Key: "sha256:account"}, Positions: &Fingerprint{Version: PositionsFingerprintVersion, Key: "sha256:positions"}, Regime: &Fingerprint{Version: RegimeFingerprintVersion, Key: "sha256:regime"}},
		SourceHealth: []SourceHealth{
			{Source: "account", Status: "ok", Fingerprint: &Fingerprint{Version: AccountFingerprintVersion, Key: "sha256:account"}, FingerprintStability: FingerprintStabilitySemanticBuckets},
			{Source: "positions", Status: "ok", Fingerprint: &Fingerprint{Version: PositionsFingerprintVersion, Key: "sha256:positions"}, FingerprintStability: FingerprintStabilitySemanticBuckets},
			{Source: "regime", Status: "partial", Fingerprint: &Fingerprint{Version: RegimeFingerprintVersion, Key: "sha256:regime"}, FingerprintStability: FingerprintStabilitySemanticBuckets, Notes: []string{"degraded clusters: gamma"}},
		},
		Policy:             "canary-default",
		Action:             "watch",
		MarketConfirmation: "partial",
		PortfolioFit:       "high",
		InputHealth:        "degraded",
		Direction:          risk.DirectionDefensive,
		Severity:           risk.SeverityWatch,
		PlannerModeHint:    risk.PlannerModeStage,
		PlannerReadiness:   risk.PlannerReadinessPrestage,
		Summary:            "Freeze new risk and stage reductions.",
		Portfolio:          StressPortfolioSummary{BaseCurrency: "USD", NetLiquidation: 100_000},
		Market:             StressMarketSummary{RegimeVerdict: "Elevated stress watch", RankedClusters: 5, YellowClusters: 3},
		NotExecution:       "Read-only stress snapshot; no orders are placed by Canary.",
	}
	for i := range 12 {
		full.Signals = append(full.Signals, risk.Signal{
			ID:        risk.SignalID("signal_" + string(rune('a'+i))),
			Direction: risk.DirectionDefensive,
			Severity:  risk.SeverityWatch,
			Evidence:  "diagnostic signal evidence with threshold, observed value, blocked_by, target, and confidence notes",
		})
		severity := risk.SeverityObserve
		if i == 0 {
			severity = risk.SeverityWatch
		}
		full.Rows = append(full.Rows, StressRow{
			Title:    "Diagnostic evidence row",
			Severity: severity,
			Guidance: "Full payload row used for detailed investigation after the one-call monitor path.",
			Evidence: "row evidence includes context that alert view intentionally omits unless severity is actionable",
		})
		full.MarketIndicators = append(full.MarketIndicators, StressMarketIndicator{
			Name:    "Indicator",
			Status:  "amber",
			AsOf:    "live",
			Reading: "detailed market reading",
			Comment: "full diagnostic comment for regime detail",
		})
	}

	positions := PositionsResult{AsOf: now, Portfolio: &PositionsPortfolio{GreeksCoverage: 0, GreeksTotal: 0}}
	fullBytes, err := json.Marshal(full)
	if err != nil {
		t.Fatalf("marshal full: %v", err)
	}
	alertBytes, err := json.Marshal(CompactStressAlert(&full, &positions))
	if err != nil {
		t.Fatalf("marshal alert: %v", err)
	}
	if len(alertBytes)*2 > len(fullBytes) {
		t.Fatalf("alert canary payload = %d bytes, full = %d bytes; want alert at least 50%% smaller", len(alertBytes), len(fullBytes))
	}
}

func TestCompactRegimeMonitorCarriesAuthorityHealth(t *testing.T) {
	t.Parallel()
	lastSuccess := time.Date(2026, 7, 20, 18, 30, 0, 0, time.UTC)
	age := int64(420)
	regime := RegimeSnapshotResult{
		AsOf: lastSuccess,
		AuthorityHealth: &RegimeAuthorityHealth{
			Status: RegimeAuthorityStale, Refreshing: true,
			LastSuccessAt: &lastSuccess, LastSuccessAgeSeconds: &age,
			FailureCode: RegimeAuthorityFailureRefreshIncomplete,
		},
	}
	got := CompactRegimeMonitor(&regime)
	if got.AuthorityHealth == nil || got.AuthorityHealth.Status != RegimeAuthorityStale || !got.AuthorityHealth.Refreshing || got.AuthorityHealth.LastSuccessAgeSeconds == nil || *got.AuthorityHealth.LastSuccessAgeSeconds != age {
		t.Fatalf("compact authority health = %#v", got.AuthorityHealth)
	}
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"authority_health"`)) {
		t.Fatalf("compact JSON dropped authority health: %s", raw)
	}
}

func TestAccountAuthorityJSONEmitsFalseFieldAvailability(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 4, 9, 30, 0, 0, time.UTC)
	result := AccountResult{
		AccountID: "DU123", BaseCurrency: "USD", NetLiquidation: 0, TotalCash: 0, AsOf: now,
		Authority: &AccountDataAuthority{
			Scope:  AccountDataScope{AccountID: "DU123", AccountMode: AccountModePaper},
			Source: AccountDataSourceAccountSummaryRequest, Availability: AccountDataAvailable,
			Freshness: AccountDataFreshnessCurrent, AsOf: now,
			Fields: &AccountFieldAvailability{BaseCurrency: true, NetLiquidation: true, TotalCash: false},
		},
	}
	wire, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	text := string(wire)
	for _, want := range []string{
		`"authority":{"scope":{"account_id":"DU123","account_mode":"paper"}`,
		`"net_liquidation":true`,
		`"total_cash":false`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("wire missing %s: %s", want, text)
		}
	}
}

func TestAlertCandidateRPCJSONUsesRiskValidation(t *testing.T) {
	now := time.Date(2026, time.July, 20, 20, 0, 0, 0, time.UTC)
	key, err := BuildAlertEpisodeKey(AlertSourceRegime, AlertKindMarketState, "classified-regime-episode")
	if err != nil {
		t.Fatal(err)
	}
	candidate := AlertCandidate{
		EpisodeKey:          key,
		OccurrenceKey:       mustRPCAlertOccurrenceKey(t, key, "occurrence-1"),
		EvidenceFingerprint: "sha256:" + strings.Repeat("b", 64),
		Source:              AlertSourceRegime,
		Kind:                AlertKindMarketState,
		PresentationCode:    AlertPresentationRegimeMarketStress,
		State:               AlertEpisodeOpen,
		Severity:            AlertSeverityWatch,
		EvidenceHealth:      AlertEvidenceCurrent,
		Destination:         AlertDestinationMonitor,
		EvidenceAsOf:        now.Add(-time.Minute),
		StateChangedAt:      now.Add(-2 * time.Minute),
		ObservedAt:          now,
	}
	snapshot := AlertCandidateSnapshot{
		SchemaVersion: AlertCandidateSnapshotVersion,
		AuthorityScope: func() string {
			scope, err := BuildAlertAuthorityScope("DU-TEST", "paper")
			if err != nil {
				t.Fatal(err)
			}
			return scope
		}(),
		AsOf:         now,
		CurrentState: AlertSnapshotActive,
		Coverage: AlertCoverage{
			State:           AlertCoverageComplete,
			Freshness:       AlertCoverageCurrent,
			AsOf:            now,
			ExpectedSources: []AlertSource{AlertSourceRegime},
			CoveredSources:  []AlertSource{AlertSourceRegime},
		},
		Sources: []AlertSourceCoverage{{
			Source: AlertSourceRegime, Status: "current", Reason: "current", EvidenceHealth: AlertEvidenceCurrent,
			InputAsOf: now, ObservedAt: now, EvidenceAsOf: now, FreshUntil: now.Add(time.Minute), Covered: true,
		}},
		Candidates: []AlertCandidate{candidate},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded AlertCandidateSnapshot
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, snapshot) {
		t.Fatalf("RPC JSON round trip mismatch: got %#v want %#v", decoded, snapshot)
	}
	if err := ValidateAlertCandidateSnapshot(decoded); err != nil {
		t.Fatal(err)
	}

	hostile := strings.Replace(string(raw), `"destination":"monitor"`, `"destination":"monitor","device_id":"private"`, 1)
	if err := json.Unmarshal([]byte(hostile), &decoded); err == nil {
		t.Fatal("RPC alias accepted private delivery target extension")
	}
}

func mustRPCAlertOccurrenceKey(t *testing.T, episodeKey string, identity string) string {
	t.Helper()
	key, err := BuildAlertOccurrenceKey(episodeKey, identity)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestRegimeAuthorityHealthJSONIsExact(t *testing.T) {
	t.Parallel()
	valid := `{"status":"unavailable","refreshing":false,"failure_code":"no_last_good"}`
	tests := []struct {
		name string
		data string
	}{
		{name: "not object", data: `[]`},
		{name: "missing status", data: `{"refreshing":true}`},
		{name: "missing refreshing", data: `{"status":"unavailable","failure_code":"no_last_good"}`},
		{name: "unknown key", data: strings.TrimSuffix(valid, "}") + `,"error":"private upstream text"}`},
		{name: "duplicate key", data: `{"status":"unavailable","status":"stale","refreshing":true}`},
		{name: "null field", data: `{"status":"unavailable","refreshing":false,"failure_code":null}`},
		{name: "trailing json", data: valid + `{}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var health RegimeAuthorityHealth
			if err := json.Unmarshal([]byte(tc.data), &health); err == nil {
				t.Fatalf("unmarshal unexpectedly accepted %s", tc.data)
			}
		})
	}
}
