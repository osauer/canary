package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestStatusGatewayPhaseDistinguishesConnectivityBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                                       string
		local, ready, setup, inFlight, backendDown bool
		lastError                                  string
		uptime                                     time.Duration
		want                                       string
	}{
		{"startup", false, false, false, true, false, "", time.Second, rpc.GatewayPhaseConnecting},
		{"startup grace", false, false, false, false, false, "", time.Second, rpc.GatewayPhaseConnecting},
		{"port down", false, false, false, false, false, "dial refused", time.Minute, rpc.GatewayPhasePortDown},
		{"local socket without API readiness", true, false, false, true, false, "", time.Second, rpc.GatewayPhaseAPINotReady},
		{"post-handshake setup", true, true, false, true, false, "", time.Second, rpc.GatewayPhaseAPINotReady},
		{"backend link down", true, true, true, false, true, "", time.Minute, rpc.GatewayPhaseBackendLinkDown},
		{"local port loss wins over stale backend latch", false, false, true, false, true, "", time.Minute, rpc.GatewayPhasePortDown},
		{"ready", true, true, true, false, false, "", time.Minute, rpc.GatewayPhaseReady},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := statusGatewayPhase(tc.local, tc.ready, tc.setup, tc.inFlight, tc.backendDown, tc.lastError, tc.uptime)
			if got != tc.want {
				t.Fatalf("phase=%q want %q", got, tc.want)
			}
		})
	}
}

func TestRegimeStatusQualityClustersStaleInputs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	hyg := 81.0
	hyg50 := 80.0
	weekly := -0.4
	res := &rpc.RegimeSnapshotResult{
		AsOf: now,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusStale,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status:   rpc.RegimeStatusStale,
			HYGPrice: &hyg,
			HYG50DMA: &hyg50,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			Status: rpc.RegimeStatusOK,
		},
		FundingStress: rpc.RegimeFundingStress{
			Status: rpc.RegimeStatusOK,
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status:       rpc.RegimeStatusStale,
			WeeklyChange: &weekly,
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					AsOf:    now,
					Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
					WarningDetails: []rpc.GammaWarningDetail{{
						Code:     "spx_unavailable:354",
						Severity: "data_quality",
					}},
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
		},
	}

	got := regimeStatusQuality(res)
	if len(got) != 1 {
		t.Fatalf("regimeStatusQuality len=%d, want 1: %+v", len(got), got)
	}
	q := got[0]
	if q.Surface != "regime" || q.Status != "stale" {
		t.Fatalf("quality header = %+v, want regime/stale", q)
	}
	wantSummary := "stale: vol, credit, FX"
	if q.Summary != wantSummary {
		t.Fatalf("summary = %q, want %q", q.Summary, wantSummary)
	}
	if got, want := q.StaleClusters, []string{"vol", "credit", "FX"}; !equalStrings(got, want) {
		t.Fatalf("stale clusters = %#v, want %#v", got, want)
	}
	if len(q.DegradedClusters) != 0 {
		t.Fatalf("degraded clusters = %#v, want none", q.DegradedClusters)
	}
}

func TestRegimeStatusQualityClustersPartialInputs(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 15, 0, 0, 0, time.UTC)
	vvix := 102.0
	res := &rpc.RegimeSnapshotResult{
		AsOf: now,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusOK,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusStale,
			Last:   &vvix,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status: rpc.RegimeStatusOK,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			Status: rpc.RegimeStatusUnavailable,
		},
		FundingStress: rpc.RegimeFundingStress{
			Status: rpc.RegimeStatusOK,
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status: rpc.RegimeStatusComputing,
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
		},
	}

	got := regimeStatusQuality(res)
	if len(got) != 1 {
		t.Fatalf("regimeStatusQuality len=%d, want 1: %+v", len(got), got)
	}
	q := got[0]
	if q.Surface != "regime" || q.Status != "partial" {
		t.Fatalf("quality header = %+v, want regime/partial", q)
	}
	wantSummary := "partial: credit, FX; stale: vol"
	if q.Summary != wantSummary {
		t.Fatalf("summary = %q, want %q", q.Summary, wantSummary)
	}
	if got, want := q.PartialClusters, []string{"credit", "FX"}; !equalStrings(got, want) {
		t.Fatalf("partial clusters = %#v, want %#v", got, want)
	}
	if got, want := q.StaleClusters, []string{"vol"}; !equalStrings(got, want) {
		t.Fatalf("stale clusters = %#v, want %#v", got, want)
	}
}

func TestRegimeStatusQualityTreatsMissingRequiredFieldsAsPartial(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 4, 9, 15, 0, 0, time.UTC)
	res := &rpc.RegimeSnapshotResult{
		AsOf: now,
		USDJPY: rpc.RegimeUSDJPY{
			RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "unranked"},
			Status:              rpc.RegimeStatusOK,
			FieldsMissing:       []string{"close_7d_ago", "weekly_change_pct"},
		},
	}

	got := regimeStatusQuality(res)
	if len(got) != 1 {
		t.Fatalf("regimeStatusQuality len=%d, want 1: %+v", len(got), got)
	}
	q := got[0]
	if q.Surface != "regime" || q.Status != "partial" {
		t.Fatalf("quality header = %+v, want regime/partial", q)
	}
	if got, want := q.PartialClusters, []string{"FX"}; !equalStrings(got, want) {
		t.Fatalf("partial clusters = %#v, want %#v", got, want)
	}
	if q.Summary != "partial: FX" {
		t.Fatalf("summary = %q, want partial: FX", q.Summary)
	}
}

func TestStatusDataFarmsKeepsOnlyUnhealthyFarms(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 8, 20, 0, 0, time.UTC)
	got := statusDataFarms([]ibkrlib.DataFarmStatus{
		{Name: "usopt", Type: "market", Status: "ok", Code: 2104, AsOf: now},
		{Name: "euhmds", Type: "historical", Status: "inactive", Code: 2107, AsOf: now},
		{Name: "secdefeu", Type: "security_definition", Status: "disconnected", Code: 2157, Message: "Sec-def data farm connection is broken:secdefeu", AsOf: now},
		{Name: "tws-server", Type: "connectivity", Status: "broken", Code: 2110, Message: "Connectivity between TWS and server is broken", AsOf: now},
	})
	if len(got) != 2 {
		t.Fatalf("data farms len=%d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "secdefeu" || got[0].Status != "disconnected" {
		t.Fatalf("first farm = %+v, want secdefeu disconnected", got[0])
	}
	if got[1].Name != "tws-server" || got[1].Status != "broken" {
		t.Fatalf("second farm = %+v, want tws-server broken", got[1])
	}
}

func TestSubsystemHealthDegradesWhenFarmNoticesAreMissing(t *testing.T) {
	t.Parallel()
	subs := (&Server{}).subsystemHealth(true, nil)
	storage := mustFindSubsystem(t, subs, "storage")
	if storage.Status != "unavailable" {
		t.Fatalf("storage subsystem = %+v, want unavailable without daemon.db", storage)
	}
	for _, name := range []string{"quote", "scanner", "chain"} {
		sub := mustFindSubsystem(t, subs, name)
		if sub.Status != "degraded" || !strings.Contains(sub.Message, "no market-data farm connection notice observed") {
			t.Fatalf("%s subsystem = %+v, want degraded missing market-data notice", name, sub)
		}
	}
	history := mustFindSubsystem(t, subs, "history")
	if history.Status != "degraded" || !strings.Contains(history.Message, "no historical-data farm connection notice observed") {
		t.Fatalf("history subsystem = %+v, want degraded missing historical-data notice", history)
	}
}

func TestSubsystemHealthUsesHealthyFarmNotices(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 8, 20, 0, 0, time.UTC)
	subs := (&Server{}).subsystemHealth(true, []ibkrlib.DataFarmStatus{
		{Name: "usfarm", Type: "market", Status: "ok", Code: 2104, AsOf: now},
		{Name: "ushmds", Type: "historical", Status: "inactive", Code: 2107, AsOf: now},
	})
	for _, name := range []string{"quote", "scanner", "history", "chain"} {
		sub := mustFindSubsystem(t, subs, name)
		if sub.Status != "ready" {
			t.Fatalf("%s subsystem = %+v, want ready", name, sub)
		}
	}
}

func TestSubsystemHealthDegradesOnDisconnectedFarms(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.July, 1, 15, 0, 0, 0, time.UTC)
	// A recorded farm that has flipped back to disconnected (2103 market /
	// 2105 historical) must degrade the dependent subsystems, not linger
	// "ready" off a stale OK notice.
	subs := (&Server{}).subsystemHealth(true, []ibkrlib.DataFarmStatus{
		{Name: "usfarm", Type: "market", Status: "disconnected", Code: 2103, Message: "Market data farm connection is broken:usfarm", AsOf: now},
		{Name: "ushmds", Type: "historical", Status: "disconnected", Code: 2105, Message: "HMDS data farm connection is broken:ushmds", AsOf: now},
	})
	for _, name := range []string{"quote", "scanner", "chain"} {
		sub := mustFindSubsystem(t, subs, name)
		if sub.Status != "degraded" || !strings.Contains(sub.Message, "market-data farm usfarm disconnected") {
			t.Fatalf("%s subsystem = %+v, want degraded market-data farm disconnected", name, sub)
		}
		if sub.LastError != "IBKR 2103 disconnected" || !sub.LastErrorAt.Equal(now) {
			t.Fatalf("%s error = %q at %s, want IBKR 2103 at %s", name, sub.LastError, sub.LastErrorAt, now)
		}
	}
	history := mustFindSubsystem(t, subs, "history")
	if history.Status != "degraded" || !strings.Contains(history.Message, "historical-data farm ushmds disconnected") {
		t.Fatalf("history subsystem = %+v, want degraded historical-data farm disconnected", history)
	}
}

func TestSubsystemHealthDegradesChainOnSecurityDefinitionFarmIssue(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 8, 20, 0, 0, time.UTC)
	subs := (&Server{}).subsystemHealth(true, []ibkrlib.DataFarmStatus{
		{Name: "usfarm", Type: "market", Status: "ok", Code: 2104, AsOf: now},
		{Name: "ushmds", Type: "historical", Status: "ok", Code: 2106, AsOf: now},
		{Name: "secdefeu", Type: "security_definition", Status: "disconnected", Code: 2157, Message: "Sec-def data farm connection is broken:secdefeu", AsOf: now},
	})
	chain := mustFindSubsystem(t, subs, "chain")
	if chain.Status != "degraded" || !strings.Contains(chain.Message, "security-definition farm secdefeu disconnected") {
		t.Fatalf("chain subsystem = %+v, want degraded security-definition farm", chain)
	}
	if chain.LastError != "IBKR 2157 disconnected" || !chain.LastErrorAt.Equal(now) {
		t.Fatalf("chain error = %q at %s, want IBKR 2157 at %s", chain.LastError, chain.LastErrorAt, now)
	}
}

func mustFindSubsystem(t *testing.T, subs []rpc.SubsystemHealth, name string) rpc.SubsystemHealth {
	t.Helper()
	for _, sub := range subs {
		if sub.Name == name {
			return sub
		}
	}
	t.Fatalf("subsystem %q not found in %+v", name, subs)
	return rpc.SubsystemHealth{}
}

func TestGammaStatusQualityReportsSPXExcluded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			AsOf:    now,
			Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
			WarningDetails: []rpc.GammaWarningDetail{{
				Code: "spx_unavailable:354",
			}},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: SPX excluded" {
		t.Fatalf("quality = %+v, want gamma degraded SPX excluded", got)
	}
}

func TestGammaStatusQualityReportsSPYExcluded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			AsOf:    now,
			Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
			WarningDetails: []rpc.GammaWarningDetail{{
				Code: "spy_unavailable:zero_magnitude",
			}},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: SPY excluded" {
		t.Fatalf("quality = %+v, want gamma degraded SPY excluded", got)
	}
}

func TestGammaStatusQualityTreatsRankableSPXAsStableWithSPYExcluded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 2, 15, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeSPX,
			AsOf:  now,
			Quality: &rpc.GammaSignalQuality{
				Rankability:       rpc.GammaRankabilityRankable,
				RankabilityReason: "all rankability gates passed",
			},
			Summary: &rpc.GammaZeroSummary{Confidence: "estimate"},
			WarningDetails: []rpc.GammaWarningDetail{
				{Code: "strike_budget_capped"},
				{Code: "no_crossing_in_window"},
				{Code: "spy_unavailable:throttled"},
			},
		},
	})
	if ok {
		t.Fatalf("gammaStatusQuality = %+v, want no data-quality warning for rankable SPX canonical signal", got)
	}
}

func TestGammaStatusQualityTreatsContextOnlyAsContextNotDegraded(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 2, 22, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeCombined,
			AsOf:  now,
			Quality: &rpc.GammaSignalQuality{
				Rankability:       rpc.GammaRankabilityContextOnly,
				RankabilityReason: "freshness: market is closed; cached gamma is context only",
			},
			Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
			WarningDetails: []rpc.GammaWarningDetail{{
				Code:  "spx_cache_fallback:no_data",
				Scope: "SPX",
			}},
		},
	})
	if ok {
		t.Fatalf("gammaStatusQuality = %+v, want no high-level degraded data quality for context-only gamma", got)
	}
}

func TestGammaStatusQualityNamesCombinedSPXModelSourceBlocker(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 2, 15, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeCombined,
			AsOf:  now,
			Quality: &rpc.GammaSignalQuality{
				Rankability:       rpc.GammaRankabilityBlocked,
				RankabilityReason: "spx_coverage: SPX slice is not rankable",
				ByUnderlying: map[string]rpc.GammaSignalQuality{
					"SPX": {
						Rankability:       rpc.GammaRankabilityBlocked,
						RankabilityReason: "derived_iv_share: 100.0% of priced legs used derived IV",
						Coverage: rpc.GammaQualityCoverage{
							PricedLegs:   865,
							DerivedIVPct: 100,
						},
						Blockers: []string{
							"derived_iv_share: 100.0% of priced legs used derived IV",
							"model_source: no gateway model IV ticks landed; all IVs were derived from quotes/closes",
						},
					},
				},
			},
			Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "partial" {
		t.Fatalf("quality header = %+v, want gamma partial", got)
	}
	for _, want := range []string{"SPX model source blocked", "100.0% of priced legs used derived IV"} {
		if !strings.Contains(got.Summary, want) {
			t.Fatalf("summary = %q, missing %q", got.Summary, want)
		}
	}
	if strings.Contains(got.Summary, "SPX slice is not rankable") {
		t.Fatalf("summary should not preserve generic combined blocker: %q", got.Summary)
	}
}

func TestGammaStatusQualityReportsSPXCacheFallbackWithoutSummary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			AsOf: now,
			WarningDetails: []rpc.GammaWarningDetail{{
				Code: "spx_cache_fallback:timeout",
			}},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: SPX cache fallback" {
		t.Fatalf("quality = %+v, want gamma degraded SPX cache fallback", got)
	}
}

func TestGammaStatusQualityReportsNonReadyAsPartial(t *testing.T) {
	t.Parallel()
	started := time.Date(2026, time.June, 1, 15, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		env     rpc.GammaZeroSPXResult
		summary string
	}{
		{
			name: "computing",
			env: rpc.GammaZeroSPXResult{
				Status:    rpc.GammaZeroStatusComputing,
				StartedAt: &started,
			},
			summary: "partial: gamma computing",
		},
		{
			name: "cold",
			env: rpc.GammaZeroSPXResult{
				Status:     rpc.GammaZeroStatusCold,
				ColdReason: "option chain cache is cold",
			},
			summary: "partial: option chain cache is cold",
		},
		{
			name: "error",
			env: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusError,
				Error:  "context deadline exceeded",
			},
			summary: "partial: gamma error: context deadline exceeded",
		},
		{
			name: "ready nil result",
			env: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
			},
			summary: "partial: gamma ready envelope missing result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := gammaStatusQuality(tt.env)
			if !ok {
				t.Fatal("gammaStatusQuality ok=false, want true")
			}
			if got.Surface != "gamma" || got.Status != "partial" || got.Summary != tt.summary {
				t.Fatalf("quality = %+v, want gamma partial %q", got, tt.summary)
			}
			if got.PartialClusters == nil || len(got.PartialClusters) != 1 || got.PartialClusters[0] != "gamma" {
				t.Fatalf("partial clusters = %+v, want gamma", got.PartialClusters)
			}
		})
	}
}

func TestGammaStatusQualityReportsPartialOptionOI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeCombined,
			AsOf:  now,
			Summary: &rpc.GammaZeroSummary{
				Confidence: "degraded",
			},
			PerIndex: map[string]*rpc.GammaZeroComputed{
				"SPY": {
					Scope: rpc.GammaZeroScopeSPY,
					WarningDetails: []rpc.GammaWarningDetail{{
						Code: "oi_missing",
					}},
				},
				"SPX": {
					Scope: rpc.GammaZeroScopeSPX,
				},
			},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: partial SPY option OI (expected: sampled outside RTH)" {
		t.Fatalf("quality = %+v, want gamma degraded expected outside-RTH SPY partial option OI", got)
	}
}

func TestGammaStatusQualityReportsWarningOnlyPartialOptionOI(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeCombined,
			AsOf:  now,
			PerIndex: map[string]*rpc.GammaZeroComputed{
				"SPY": {
					Scope: rpc.GammaZeroScopeSPY,
					WarningDetails: []rpc.GammaWarningDetail{{
						Code: "oi_missing",
					}},
				},
			},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: partial SPY option OI (expected: sampled outside RTH)" {
		t.Fatalf("quality = %+v, want warning-only gamma degraded expected outside-RTH SPY partial option OI", got)
	}
}

func TestGammaStatusQualityReportsUnexpectedPartialOptionOIDuringRTH(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 15, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeCombined,
			AsOf:  now,
			Summary: &rpc.GammaZeroSummary{
				Confidence: "degraded",
			},
			PerIndex: map[string]*rpc.GammaZeroComputed{
				"SPX": {
					Scope: rpc.GammaZeroScopeSPX,
					AsOf:  now,
					WarningDetails: []rpc.GammaWarningDetail{{
						Code: "oi_missing",
					}},
				},
			},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: partial SPX option OI (unexpected: SPX OI should be session-stable)" {
		t.Fatalf("quality = %+v, want gamma degraded unexpected SPX partial option OI", got)
	}
}

func TestGammaStatusQualityReportsUnexpectedSPXPartialOptionOIOutsideRTH(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.June, 1, 22, 0, 0, 0, time.UTC)
	got, ok := gammaStatusQuality(rpc.GammaZeroSPXResult{
		Status: rpc.GammaZeroStatusReady,
		Result: &rpc.GammaZeroComputed{
			Scope: rpc.GammaZeroScopeSPX,
			AsOf:  now,
			Summary: &rpc.GammaZeroSummary{
				Confidence: "degraded",
			},
			WarningDetails: []rpc.GammaWarningDetail{{
				Code: "oi_missing",
			}},
		},
	})
	if !ok {
		t.Fatal("gammaStatusQuality ok=false, want true")
	}
	if got.Surface != "gamma" || got.Status != "degraded" || got.Summary != "degraded: partial SPX option OI (unexpected: SPX OI should be session-stable)" {
		t.Fatalf("quality = %+v, want off-hours SPX partial option OI to remain unexpected", got)
	}
}

func TestRegimeSnapshotDataQualityCombinesGammaAndRegime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.May, 30, 12, 0, 0, 0, time.UTC)
	res := &rpc.RegimeSnapshotResult{
		AsOf: now,
		VIXTermStructure: rpc.RegimeVIXTerm{
			Status: rpc.RegimeStatusStale,
		},
		VolOfVol: rpc.RegimeVolOfVol{
			Status: rpc.RegimeStatusOK,
		},
		HYGSPYDivergence: rpc.RegimeHYGSPYDivergence{
			Status: rpc.RegimeStatusOK,
		},
		CreditSpreads: rpc.RegimeCreditSpreads{
			Status: rpc.RegimeStatusOK,
		},
		FundingStress: rpc.RegimeFundingStress{
			Status: rpc.RegimeStatusOK,
		},
		USDJPY: rpc.RegimeUSDJPY{
			Status: rpc.RegimeStatusOK,
		},
		GammaZero: rpc.RegimeGammaZero{
			Status: rpc.RegimeStatusOK,
			Envelope: rpc.GammaZeroSPXResult{
				Status: rpc.GammaZeroStatusReady,
				Result: &rpc.GammaZeroComputed{
					AsOf:    now,
					Summary: &rpc.GammaZeroSummary{Confidence: "degraded"},
					WarningDetails: []rpc.GammaWarningDetail{{
						Code: "spx_unavailable:354",
					}},
				},
			},
		},
		Breadth: rpc.RegimeBreadth{
			Status: rpc.RegimeStatusOK,
		},
	}

	got := regimeSnapshotDataQuality(res)
	if len(got) != 2 {
		t.Fatalf("regimeSnapshotDataQuality len=%d, want 2: %+v", len(got), got)
	}
	if got[0].Surface != "gamma" || got[0].Summary != "degraded: SPX excluded" {
		t.Fatalf("first quality = %+v, want gamma degraded", got[0])
	}
	if got[1].Surface != "regime" || got[1].Summary != "stale: vol" {
		t.Fatalf("second quality = %+v, want regime stale vol", got[1])
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
