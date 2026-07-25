package rpc

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestCompactRegimeSnapshotKeepsAgentSurfaceAndDropsMethodology(t *testing.T) {
	t.Parallel()
	v := 0.91
	r := &RegimeSnapshotResult{
		AsOf: time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC),
		Summary: RegimeSummary{
			Label: "Normal regime", Evidence: "1 green", PunchLine: "volatility term structure is constructive.",
		},
		VIXTermStructure: RegimeVIXTerm{
			RegimeIndicatorMeta: RegimeIndicatorMeta{
				Band:       "green",
				BandReason: "<0.92 contango",
				Thresholds: &RegimeThresholds{
					Label: "vix_term_structure_v1", Green: "VIX/VIX3M < 0.92", Yellow: "0.92-1.00", Red: ">=1.00",
					Heuristic: true, PendingBacktest: true,
				},
				AsOf: &RegimeAsOfSummary{Label: "live", Time: time.Date(2026, 5, 24, 10, 0, 0, 0, time.UTC), Freshness: FreshnessLive, Source: "VIX tick"},
			},
			Status: RegimeStatusOK,
			Ratio:  &v,
			Notes:  strings.Repeat("methodology ", 50),
			Streak: &StreakInfo{Band: "green", Sessions: 2, Since: "2026-05-23"},
		},
		VolOfVol: RegimeVolOfVol{
			Status: RegimeStatusOK,
			Last:   &v,
			Notes:  strings.Repeat("VVIX methodology ", 20),
		},
		CreditSpreads: RegimeCreditSpreads{
			Status: RegimeStatusOK,
			HYOAS:  &v,
			Notes:  strings.Repeat("OAS methodology ", 20),
		},
		FundingStress: RegimeFundingStress{
			Status:    RegimeStatusOK,
			SpreadBps: &v,
			Notes:     strings.Repeat("funding methodology ", 20),
		},
		Breadth: RegimeBreadth{
			Status: RegimeStatusOK,
			Notes:  strings.Repeat("breadth methodology ", 50),
			Envelope: BreadthSPXResult{
				State:   BreadthStateReady,
				History: []BreadthDailyValue{{Date: "2026-05-23", PctAbove50DMA: 55}},
			},
		},
		WarningDetails: []RegimeWarning{{
			Code: "usd_jpy_unavailable", Scope: "usd_jpy", Severity: "warning",
			Message: "no FX tick", Impact: "FX is unranked", Action: "check entitlement",
		}},
		SpecDoc: "docs/docs/internals/regime-dashboard.md",
	}

	CompactRegimeSnapshot(r)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	wire := string(b)
	for _, want := range []string{"summary", "punch_line", "warning_details", "streak", "spec_doc", "band_reason", "thresholds", "as_of"} {
		if !strings.Contains(wire, want) {
			t.Errorf("compact snapshot missing %q: %s", want, wire)
		}
	}
	for _, notWant := range []string{"methodology methodology", "VVIX methodology", "OAS methodology", "funding methodology", "breadth methodology", "history"} {
		if strings.Contains(wire, notWant) {
			t.Errorf("compact snapshot should drop %q: %s", notWant, wire)
		}
	}
}

func TestCompactRegimeSnapshotStripsGammaProfilesRecursively(t *testing.T) {
	t.Parallel()
	pt := GammaProfilePoint{Spot: 500, GEX: 1.2}
	r := &RegimeSnapshotResult{
		Summary: RegimeSummary{Label: "Normal regime", Evidence: "1 green"},
		GammaZero: RegimeGammaZero{
			Status: RegimeStatusOK,
			Envelope: GammaZeroSPXResult{
				Status: GammaZeroStatusReady,
				Result: &GammaZeroComputed{
					Scope:       GammaZeroScopeCombined,
					Profile:     []GammaProfilePoint{pt},
					Profile0DTE: []GammaProfilePoint{pt},
					Profile1to7: []GammaProfilePoint{pt},
					ProfileTerm: []GammaProfilePoint{pt},
					TopStrikes:  []StrikeConcentration{{Underlying: "SPX", Strike: 6000, Expiry: "2026-05-29", Right: "C", AbsGEX: 42}},
					PerIndex: map[string]*GammaZeroComputed{
						"SPY": {
							Scope:       GammaZeroScopeSPY,
							Profile:     []GammaProfilePoint{pt},
							Profile0DTE: []GammaProfilePoint{pt},
							TopStrikes:  []StrikeConcentration{{Underlying: "SPY", Strike: 600, Expiry: "2026-05-29", Right: "P", AbsGEX: 21}},
						},
					},
				},
			},
		},
	}

	CompactRegimeSnapshot(r)
	got := r.GammaZero.Envelope.Result
	if got == nil {
		t.Fatal("gamma result missing after compact")
	}
	if len(got.Profile) != 0 || len(got.Profile0DTE) != 0 || len(got.Profile1to7) != 0 || len(got.ProfileTerm) != 0 {
		t.Fatalf("top-level profiles should be stripped, got profile lens %d/%d/%d/%d",
			len(got.Profile), len(got.Profile0DTE), len(got.Profile1to7), len(got.ProfileTerm))
	}
	if len(got.TopStrikes) != 1 {
		t.Fatalf("top strikes should remain for agent diagnostics, got %+v", got.TopStrikes)
	}
	sub := got.PerIndex["SPY"]
	if sub == nil {
		t.Fatal("per-index SPY result missing after compact")
	}
	if len(sub.Profile) != 0 || len(sub.Profile0DTE) != 0 {
		t.Fatalf("per-index profiles should be stripped, got lens %d/%d", len(sub.Profile), len(sub.Profile0DTE))
	}
	if len(sub.TopStrikes) != 1 {
		t.Fatalf("per-index top strikes should remain, got %+v", sub.TopStrikes)
	}
}
