package rpc

import (
	"testing"
	"time"
)

// The cash-credit veto is an evidentiary claim: only a recent official read
// that fails to corroborate may soften a row-confirmed HYG red. Absence,
// staleness, a red band, or directional widening all leave the proxy red.
func TestCreditCashVetoRequiresCurrentEvidence(t *testing.T) {
	asOf := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	snap := func(cs RegimeCreditSpreads) *RegimeSnapshotResult {
		r := &RegimeSnapshotResult{AsOf: asOf}
		r.HYGSPYDivergence.RegimeIndicatorMeta = RegimeIndicatorMeta{Band: "red"}
		r.CreditSpreads = cs
		return r
	}
	oas := func(v float64) *float64 { return &v }

	freshGreen := RegimeCreditSpreads{HYOAS: oas(2.7), AsOfDate: "2026-08-10",
		Band: "green"}
	if got := BuildRegimeClusterBands(snap(freshGreen)).Confirmed[RegimeClusterCredit]; got != "yellow" {
		t.Fatalf("fresh green cash must veto the proxy: credit cluster = %q, want yellow", got)
	}

	absent := RegimeCreditSpreads{}
	if got := BuildRegimeClusterBands(snap(absent)).Confirmed[RegimeClusterCredit]; got != "red" {
		t.Fatalf("an absent official gauge must abstain: credit cluster = %q, want red", got)
	}

	stale := RegimeCreditSpreads{HYOAS: oas(2.7), AsOfDate: "2026-07-30",
		Band: "green"}
	if got := BuildRegimeClusterBands(snap(stale)).Confirmed[RegimeClusterCredit]; got != "red" {
		t.Fatalf("a stale official gauge must abstain: credit cluster = %q, want red", got)
	}

	widening := RegimeCreditSpreads{HYOAS: oas(2.9), HY20DChange: oas(0.6), AsOfDate: "2026-08-10",
		Band: "yellow"}
	if got := BuildRegimeClusterBands(snap(widening)).Confirmed[RegimeClusterCredit]; got != "red" {
		t.Fatalf("fresh widening co-signs the proxy: credit cluster = %q, want red", got)
	}

	cashRed := RegimeCreditSpreads{HYOAS: oas(5.8), AsOfDate: "2026-08-10",
		Band: "red"}
	if got := BuildRegimeClusterBands(snap(cashRed)).Confirmed[RegimeClusterCredit]; got != "red" {
		t.Fatalf("red cash corroborates the proxy: credit cluster = %q, want red", got)
	}
}
