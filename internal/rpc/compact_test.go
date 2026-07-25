package rpc

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/ibkr/v2/internal/risk"
)

func TestOptionDTECalendarDays(t *testing.T) {
	t.Parallel()

	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("load Europe/Berlin: %v", err)
	}

	tests := []struct {
		name   string
		raw    string
		asOf   time.Time
		want   int
		wantOK bool
	}{
		{
			name:   "positive span across spring-forward compact layout",
			raw:    "20260331",
			asOf:   time.Date(2026, time.March, 25, 12, 0, 0, 0, berlin),
			want:   6,
			wantOK: true,
		},
		{
			name:   "negative span across spring-forward dashed layout",
			raw:    "2026-03-25",
			asOf:   time.Date(2026, time.March, 31, 12, 0, 0, 0, berlin),
			want:   -6,
			wantOK: true,
		},
		{
			name:   "positive span across fall-back",
			raw:    "20261027",
			asOf:   time.Date(2026, time.October, 21, 12, 0, 0, 0, berlin),
			want:   6,
			wantOK: true,
		},
		{
			name:   "same day",
			raw:    "2026-07-15",
			asOf:   time.Date(2026, time.July, 15, 23, 59, 0, 0, berlin),
			want:   0,
			wantOK: true,
		},
		{
			name:   "mid-summer span without transition",
			raw:    "20260710",
			asOf:   time.Date(2026, time.July, 1, 8, 30, 0, 0, berlin),
			want:   9,
			wantOK: true,
		},
		{
			name:   "invalid raw",
			raw:    "not-a-date",
			asOf:   time.Date(2026, time.July, 1, 8, 30, 0, 0, berlin),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := optionDTE(tt.raw, tt.asOf)
			if ok != tt.wantOK {
				t.Fatalf("optionDTE(%q, %v) ok = %v, want %v", tt.raw, tt.asOf, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("optionDTE(%q, %v) = %d, want %d", tt.raw, tt.asOf, got, tt.want)
			}
		})
	}
}

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

func TestCompactStressAlertDropsDiagnosticArraysAndKeepsFlags(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 8, 45, 0, 0, time.UTC)
	nextAttempt := now.Add(15 * time.Minute)
	stress := StressResult{
		AsOf:               now,
		Fingerprint:        Fingerprint{Version: StressFingerprintVersion, Key: "sha256:canary"},
		SourceFingerprints: StressSourceFingerprints{Positions: &Fingerprint{Version: PositionsFingerprintVersion, Key: "sha256:positions"}},
		SourceHealth: []SourceHealth{{
			Source: "positions", Status: "ok", RefreshState: SourceRefreshFetchFailedBackoff, NextAttempt: &nextAttempt,
			Fingerprint: &Fingerprint{Version: PositionsFingerprintVersion, Key: "sha256:positions"},
		}},
		Action:             "watch",
		MarketConfirmation: "partial",
		PortfolioFit:       "high",
		InputHealth:        "ok",
		Direction:          risk.DirectionDefensive,
		Severity:           risk.SeverityWatch,
		Summary:            "Freeze new risk.",
		Signals:            []risk.Signal{{ID: risk.SignalMarginCushionLow, Severity: risk.SeverityWatch}},
		Rows: []StressRow{
			{Title: "Context only", Severity: risk.SeverityObserve, Guidance: "No action."},
			{Title: "Margin cushion", Severity: risk.SeverityWatch, Guidance: "Watch cushion."},
		},
		Portfolio:    StressPortfolioSummary{BaseCurrency: "USD", NetLiquidation: 100_000},
		Market:       StressMarketSummary{RegimeVerdict: "Normal regime", RankedClusters: 6},
		NotExecution: "Read-only recommendation; no orders are placed by ibkr.",
	}
	positions := PositionsResult{AsOf: now, Portfolio: &PositionsPortfolio{GreeksCoverage: 0, GreeksTotal: 0}}

	out := CompactStressAlert(&stress, &positions)
	if len(out.Flags) != 1 || out.Flags[0].Title != "Margin cushion" {
		t.Fatalf("flags = %+v, want only the non-observe row", out.Flags)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(b)
	for _, absent := range []string{`"signals"`, `"rows"`, `"market_indicators"`, `"policy"`} {
		if strings.Contains(wire, absent) {
			t.Fatalf("compact stress alert should omit %s: %s", absent, wire)
		}
	}
	for _, present := range []string{`"flags"`, `"option_health"`, `"source_health"`, `"source_fingerprints"`} {
		if !strings.Contains(wire, present) {
			t.Fatalf("compact stress alert missing %s: %s", present, wire)
		}
	}
	if !strings.Contains(wire, `"refresh_state":"fetch_failed_backoff"`) || !strings.Contains(wire, `"next_attempt"`) {
		t.Fatalf("compact source health dropped refresh scheduling: %s", wire)
	}
	if strings.Contains(wire, `"fingerprint"`) && strings.Contains(wire, `"source_health":[{"source":"positions","status":"ok","fingerprint"`) {
		t.Fatalf("source_health should drop nested fingerprints in alert view: %s", wire)
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
		NotExecution:       "Read-only canary snapshot; no orders are placed by ibkr.",
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

func TestCompactRegimeMonitorDropsFullIndicatorObjects(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 6, 3, 8, 45, 0, 0, time.UTC)
	vix := 14.5
	ratio := 0.91
	regime := RegimeSnapshotResult{
		AsOf:        now,
		Fingerprint: Fingerprint{Version: RegimeFingerprintVersion, Key: "sha256:regime"},
		Summary:     RegimeSummary{Label: "Normal regime", Evidence: "3 green", PunchLine: "volatility is constructive", Confidence: "high"},
		Composite:   RegimeComposite{Verdict: "Normal regime", GreenCount: 3, RankedCount: 3, ClusterGreenCount: 3, ClusterRankedCount: 3},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band:       "green",
				Thresholds: &RegimeThresholds{Label: "vix_term_structure_v1", Green: "<0.92"},
				AsOf:       &RegimeAsOfSummary{Label: "live", Time: now, Freshness: FreshnessLive},
			},
			Status: RegimeStatusOK,
			VIX:    &vix,
			Ratio:  &ratio,
		},
		VolOfVol:         RegimeVolOfVol{Status: RegimeStatusUnavailable},
		HYGSPYDivergence: RegimeHYGSPYDivergence{Status: RegimeStatusUnavailable},
		CreditSpreads:    RegimeCreditSpreads{Status: RegimeStatusUnavailable},
		FundingStress:    RegimeFundingStress{Status: RegimeStatusUnavailable},
		USDJPY:           RegimeUSDJPY{Status: RegimeStatusUnavailable},
		GammaZero:        RegimeGammaZero{Status: RegimeStatusUnavailable},
		Breadth:          RegimeBreadth{Status: RegimeStatusUnavailable},
	}

	out := CompactRegimeMonitor(&regime)
	if len(out.Indicators) != 8 {
		t.Fatalf("indicators len = %d, want 8", len(out.Indicators))
	}
	if out.Indicators[0].Name != "VIX/VIX3M" || out.Indicators[0].Status != RegimeStatusOK || out.Indicators[0].Band != "green" {
		t.Fatalf("first indicator = %+v, want compact VIX/VIX3M row", out.Indicators[0])
	}
	if out.Posture.Label != "Normal regime" || out.Posture.Tone != RegimeToneNormal {
		t.Fatalf("posture = %+v, want Normal regime/normal", out.Posture)
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(b)
	for _, absent := range []string{`"vix_term_structure"`, `"thresholds"`, `"envelope"`, `"spec_doc"`} {
		if strings.Contains(wire, absent) {
			t.Fatalf("compact regime monitor should omit %s: %s", absent, wire)
		}
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
