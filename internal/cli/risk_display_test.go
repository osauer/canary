package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

func TestRiskDisplayPreservesAuthorityAndFitsTerminal(t *testing.T) {
	for _, width := range []string{"80", "40"} {
		t.Setenv("COLUMNS", width)
		for _, color := range []bool{false, true} {
			var out bytes.Buffer
			env := &Env{Stdout: &out, Color: color}
			res := rpc.RegimeSnapshotResult{
				AuthorityHealth: &rpc.RegimeAuthorityHealth{Status: rpc.RegimeAuthorityFresh},
				Summary:         rpc.RegimeSummary{PunchLine: "Served interpretation\x1b[2J\nforged"},
				// Deliberately contradict the numeric threshold: the served band wins.
				VIXTermStructure: rpc.RegimeVIXTerm{Status: rpc.RegimeStatusOK, Ratio: new(1.2), RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "green", BandReason: "served reason"}},
				CreditSpreads:    rpc.RegimeCreditSpreads{Status: rpc.RegimeStatusOK, HYOAS: new(3.2), HY20DChange: new(.6), RegimeIndicatorMeta: rpc.RegimeIndicatorMeta{Band: "yellow"}},
			}
			renderRegime(env, res, false)
			got := strings.Join(strings.Fields(out.String()), " ")
			for _, want := range []string{"1.20", "30-day", "served reason", "+0.60 pp", "unavailable"} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q: %s", want, got)
				}
			}
			if strings.Contains(out.String(), "\x1b[2J") || strings.Contains(out.String(), "\nforged") {
				t.Fatal("terminal control escaped sanitization")
			}
			if strings.Contains(out.String(), ansiGreen) != color || strings.Contains(out.String(), ansiYellow) != color {
				t.Fatal("served green/amber color not preserved")
			}
			for line := range strings.SplitSeq(out.String(), "\n") {
				if visibleLen(line) > outputColumns(env.Stdout) {
					t.Fatalf("line exceeds %s columns: %q", width, line)
				}
			}
			out.Reset()
			res.AuthorityHealth.Status = rpc.RegimeAuthorityStale
			renderRegime(env, res, false)
			if strings.Contains(out.String(), ansiGreen) || strings.Contains(out.String(), ansiYellow) || strings.Contains(out.String(), "Served interpretation") || !strings.Contains(out.String(), "recorded:") {
				t.Fatal("retained interpretation was promoted to current")
			}
		}
	}
}

func TestStressDisplayRanksFindingsWithoutChangingAssessment(t *testing.T) {
	var out bytes.Buffer
	res := rpc.StressResult{Action: "watch", Severity: risk.SeverityWatch, Rows: []rpc.StressRow{
		{Title: "Portfolio stress", Severity: risk.SeverityWatch},
		{Title: "Concentration", Severity: risk.SeverityWatch, Evidence: "25% of net liquidation"},
		{Title: "Exposure", Severity: risk.SeverityUrgent, Evidence: "130% of net liquidation"},
		{Title: "Missing quotes", Severity: risk.SeverityWatch, Direction: risk.DirectionDataQuality, Evidence: "unknown"},
	}}
	original := append([]rpc.StressRow(nil), res.Rows...)
	renderStress(&Env{Stdout: &out, Color: true}, res, false)
	got := out.String()
	for _, want := range []string{"WATCH", "URGENT", "Individual findings exceed", "Coverage gaps", "unknown", ansiRed, ansiYellow} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q: %s", want, got)
		}
	}
	if strings.Index(got, "Exposure") > strings.Index(got, "Concentration") || !reflect.DeepEqual(res.Rows, original) {
		t.Fatal("sort failed or mutated producer evidence")
	}
}

func TestStatusSeparatesReceivedDelayedDataFromMissingAccess(t *testing.T) {
	now := time.Now()
	ndx := rpc.MarketDataAccessHealth{Symbol: "NDX", Reason: rpc.MarketDataAccessNotSubscribed, FallbackDataType: rpc.MarketDataDelayed, FallbackReceivedAt: now}
	for _, mode := range []string{rpc.MarketDataDelayed, rpc.MarketDataDelayedFrozen} {
		ndx.FallbackDataType = mode
		got := formatMarketDataAccessStatus(&Env{Color: true}, []rpc.MarketDataAccessHealth{ndx})
		if strings.Contains(got, ansiYellow) || !strings.Contains(got, "INFO") || !strings.Contains(got, "delayed") {
			t.Fatalf("received delayed fallback reads as failure: %q", got)
		}
	}
	ndx.FallbackReceivedAt = time.Time{}
	if got := formatMarketDataAccessStatus(&Env{Color: true}, []rpc.MarketDataAccessHealth{ndx}); !strings.Contains(got, ansiYellow+"CHECK") {
		t.Fatal("unwitnessed fallback appears received", got)
	}
	ndx.FallbackReceivedAt = now
	items := []rpc.MarketDataAccessHealth{ndx, ndx, ndx, {Symbol: "TEST", Reason: rpc.MarketDataAccessNotSubscribed}}
	got := formatMarketDataAccessStatus(&Env{Color: true}, items)
	if !strings.Contains(got, ansiYellow+"CHECK · TEST") {
		t.Fatal("access failure hidden behind delayed fallbacks", got)
	}
}

func TestStatusExplainsPushAndEverySourceConcern(t *testing.T) {
	var out bytes.Buffer
	now := time.Now()
	res := rpc.HealthResult{
		Connected: true, Verdict: rpc.HealthVerdict{State: "READY", Reason: "Required sources are available"},
		DataHealth:   &rpc.DataHealthResult{Summary: rpc.DataHealthSummary{Label: "Required sources are available"}},
		PushDelivery: &rpc.PushDeliveryStatus{ReceivedAt: now, Proof: rpc.PushDeliveryProof{Mode: "watch_and_act", Dispatcher: "unavailable", DispatcherClass: "no_active_subscription"}},
	}
	renderStatusText(&Env{Stdout: &out}, &res, nil)
	if !strings.Contains(out.String(), "Phone push     No push subscription;") || strings.Contains(out.String(), "Next concern") || strings.Count(out.String(), "Required sources are available") != 1 {
		t.Fatal("status hides push blocker or duplicates healthy verdict", out.String())
	}
	out.Reset()
	res.Verdict = rpc.HealthVerdict{State: "ATTENTION", Reason: "Two sources missing"}
	res.DataHealth.Concerns = []rpc.DataHealthConcern{{Label: "First source unavailable"}, {Label: "Second source unverified"}}
	renderStatusText(&Env{Stdout: &out}, &res, nil)
	for _, want := range []string{"First source unavailable", "Second source unverified", "Next concern"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal("lost status concern", want)
		}
	}
	for line := range strings.SplitSeq(out.String(), "\n") {
		if visibleLen(line) > 80 {
			t.Fatal("status line exceeds 80 columns", line)
		}
	}
}

func TestLiveReadyDoesNotLookLikeABlocker(t *testing.T) {
	env := &Env{Color: true}
	st := rpc.TradingStatus{Mode: "live"}
	if got := formatTradingStatusValue(env, st); strings.Contains(got, ansiYellow) || !strings.Contains(got, "LIVE") || !strings.Contains(got, "ready") {
		t.Fatal("healthy live mode looks like a warning", got)
	}
	st.Blocked = true
	if got := formatTradingStatusValue(env, st); !strings.Contains(got, ansiYellow) || !strings.Contains(got, "blocked") {
		t.Fatal("actual blocker lost its warning", got)
	}
	for _, state := range []string{"computing", "degraded"} {
		got := formatSubsystemsValue(env, []rpc.SubsystemHealth{{Name: "gamma", Status: state}})
		if strings.Contains(got, ansiYellow) != (state == "degraded") {
			t.Fatal("routine computation and degradation share a warning color", got)
		}
	}
}
