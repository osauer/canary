package daemon

import (
	"strings"
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

// closedMarketSnapshot is a Saturday read: VIX/VIX3M ranks from Friday's
// closes and USD/JPY from the frozen IDEALPRO quote, both status stale by data
// type, with the cadence class each row's own schedule gives it.
func closedMarketSnapshot(class string) *rpc.RegimeSnapshotResult {
	r := coSignRedSnapshot()
	r.VIXTermStructure = rpc.RegimeVIXTerm{Status: rpc.RegimeStatusStale, DataType: rpc.MarketDataFrozen, Ratio: new(1.16), VIX: new(35.89), VIX3M: new(30.94)}
	r.USDJPY = rpc.RegimeUSDJPY{Status: rpc.RegimeStatusStale, DataType: rpc.MarketDataFrozen, Last: new(158.55), WeeklyChange: new(-0.78)}
	annotateCurrentRows(r)
	r.VIXTermStructure.Freshness = &rpc.RegimeFreshness{Class: class}
	r.USDJPY.Freshness = &rpc.RegimeFreshness{Class: class}
	return r
}

// A frozen reading that is the newest its schedule allows (not_due) is a
// current rating: the summary does not call it stale and no stale warning is
// raised for it. Inside its own window (overdue) the same frozen reading is a
// failed refresh and stays named as stale.
func TestClosedMarketFrozenRowsAreNotStale(t *testing.T) {
	r := closedMarketSnapshot(rpc.RegimeFreshnessNotDue)
	summary := buildRegimeSummary(r)
	if strings.Contains(summary.PunchLine, "stale") {
		t.Fatalf("punch line %q calls not-due closes stale", summary.PunchLine)
	}
	for _, w := range buildRegimeWarnings(r) {
		if w.Code == "vix_term_structure_stale" || w.Code == "usd_jpy_stale" {
			t.Fatalf("warning %q raised for a not-due close", w.Code)
		}
	}

	r = closedMarketSnapshot(rpc.RegimeFreshnessOverdue)
	summary = buildRegimeSummary(r)
	if !strings.Contains(summary.PunchLine, "volatility term structure and FX carry proxy are ranked from stale data") {
		t.Fatalf("punch line %q hides an overdue frozen reading", summary.PunchLine)
	}
	codes := map[string]bool{}
	for _, w := range buildRegimeWarnings(r) {
		codes[w.Code] = true
	}
	if !codes["vix_term_structure_stale"] || !codes["usd_jpy_stale"] {
		t.Fatalf("warnings %v; want both overdue rows warned as stale", codes)
	}
}
