package daemon

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func TestReconBacktestReplayAndFlows(t *testing.T) {
	s := newReconTestServer(t)
	body := strings.Join([]string{
		cashLine("pre", "Deposits/Withdrawals", 500, "20260701"),
		cashLine("match", "Deposits/Withdrawals", 10000, "20260704"),
		cashLine("dividend", "Dividends", 42, "20260705"),
		cashLine("unknown", "Future Mystery", 12, "20260706"),
		equityRow("20260703", 100000),
		equityRow("20260704", 110000),
		equityRow("20260705", 102000),
		equityRow("20260706", 99000),
		equityRow("20260707", 94000),
	}, "\n")
	writeFlexFixture(t, "flex-backtest.xml", recentGenerated(), "20260701", "20260707", body)
	declare(t, s, "deposit", 10000, "2026-07-04")

	genesis := time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC)
	latched := time.Date(2026, 7, 7, 14, 0, 0, 0, time.UTC)
	s.riskCapital.mu.Lock()
	s.riskCapital.state.GenesisAt = genesis
	s.riskCapital.state.Seeded = true
	s.riskCapital.state.AdjustedPeakBase = 100200
	s.riskCapital.state.PeakAsOf = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	s.riskCapital.state.LatchedAt = latched
	s.riskCapital.state.DailyEquity = map[string]float64{"2026-07-05": 102100}
	s.riskCapital.mu.Unlock()
	lateWarn := time.Date(2026, 7, 6, 16, 0, 0, 0, time.UTC)
	earlyWarn := time.Date(2026, 7, 5, 15, 0, 0, 0, time.UTC)
	appendRiskPolicyJournal(map[string]any{"version": 1, "at": lateWarn, "kind": "capital_tier", "to": "warn"})
	appendRiskPolicyJournal(map[string]any{"version": 1, "at": earlyWarn, "kind": "capital_tier", "to": "warn"})

	res := s.buildReconBacktest()
	if res.Status != rpc.ReconStatusActive || res.Replay == nil {
		t.Fatalf("backtest status=%s message=%q replay=%+v", res.Status, res.Message, res.Replay)
	}
	if res.GenesisAt != genesis || res.EquityDays != 5 {
		t.Fatalf("genesis=%s equity days=%d", res.GenesisAt, res.EquityDays)
	}
	if res.FlowCounts["matched"] != 1 || res.FlowCounts[rpc.ReconBaseline] != 1 || res.FlowCounts[rpc.ReconMissingFromLedger] != 0 {
		t.Fatalf("flow counts = %v", res.FlowCounts)
	}
	flows := make(map[string]rpc.ReconBacktestFlow)
	for _, flow := range res.Flows {
		flows[flow.LineID] = flow
	}
	if len(res.Flows) != 2 || res.Flows[0].LineID != "cash-pre" || res.Flows[1].LineID != "cash-match" {
		t.Fatalf("flow order = %+v, want value date then line id", res.Flows)
	}
	if got := flows["cash-match"]; got.Status != "matched" || got.PreGenesis {
		t.Fatalf("matched flow = %+v", got)
	}
	if got := flows["cash-pre"]; got.Status != rpc.ReconBaseline || !got.PreGenesis {
		t.Fatalf("pre-genesis flow = %+v", got)
	}
	if res.ClassifiedCounts["Dividends"] != 1 || res.UncategorizedCount != 1 {
		t.Fatalf("classified=%v uncategorized=%d", res.ClassifiedCounts, res.UncategorizedCount)
	}

	replay := res.Replay
	if replay.Days != 5 || replay.FirstDay.Format("2006-01-02") != "2026-07-03" || replay.LastDay.Format("2006-01-02") != "2026-07-07" ||
		replay.ReplayedPeakBase != 100000 || replay.ReplayedPeakAt.Format("2006-01-02") != "2026-07-03" {
		t.Fatalf("replay peak/days = %+v", replay)
	}
	if replay.RuntimePeakBase == nil || *replay.RuntimePeakBase != 100200 || replay.RuntimePeakAt.IsZero() || replay.PeakDivergencePct == nil {
		t.Fatalf("runtime peak comparison = %+v", replay)
	}
	wantPeakDivergence := (100000.0 - 100200.0) / 100200.0 * 100
	if math.Abs(*replay.PeakDivergencePct-wantPeakDivergence) > 1e-9 {
		t.Fatalf("peak divergence = %v, want %v", *replay.PeakDivergencePct, wantPeakDivergence)
	}
	if len(replay.Crossings) != 2 {
		t.Fatalf("crossings = %+v, want warn and block", replay.Crossings)
	}
	warn, block := replay.Crossings[0], replay.Crossings[1]
	if warn.Tier != "warn" || warn.ReplayedAt.Format("2006-01-02") != "2026-07-05" || math.Abs(warn.ReplayedConsumedPct-16) > 1e-9 || !warn.RuntimeAt.Equal(earlyWarn) {
		t.Fatalf("warn crossing = %+v", warn)
	}
	if block.Tier != "block" || block.ReplayedAt.Format("2006-01-02") != "2026-07-07" || math.Abs(block.ReplayedConsumedPct-32) > 1e-9 || !block.RuntimeAt.Equal(latched) {
		t.Fatalf("block crossing = %+v", block)
	}
	if replay.SameDayComparisons != 1 || replay.MaxSameDayDivergencePct == nil || math.Abs(*replay.MaxSameDayDivergencePct-(10000.0/102000)) > 1e-9 {
		t.Fatalf("same-day replay = count %d max %v", replay.SameDayComparisons, replay.MaxSameDayDivergencePct)
	}
	if len(replay.Notes) != 2 || replay.Notes[0] != backtestEODNote || replay.Notes[1] != backtestGenesisNote {
		t.Fatalf("notes = %v", replay.Notes)
	}
	if !strings.HasPrefix(res.ReportID, "backtest-") {
		t.Fatalf("report id = %q", res.ReportID)
	}
	if again := s.buildReconBacktest(); again.ReportID != res.ReportID {
		t.Fatalf("report id not deterministic: %s vs %s", res.ReportID, again.ReportID)
	}
}

func TestReconBacktestUnapprovedAndUnavailable(t *testing.T) {
	minimal := `
kind = "ibkr.risk_policy"
schema_version = 1
policy_id = "risk-constitution"
policy_version = 1
`
	unapproved := newRiskPolicyTestServer(t, minimal).buildReconBacktest()
	if unapproved.Status != rpc.ReconStatusUnapproved || !strings.Contains(unapproved.Message, "unapproved") {
		t.Fatalf("unapproved result = %+v", unapproved)
	}

	unavailable := newReconTestServer(t).buildReconBacktest()
	if unavailable.Status != rpc.ReconStatusUnavailable || !strings.Contains(unavailable.Message, "no retained Flex statements") {
		t.Fatalf("unavailable result = %+v", unavailable)
	}
}
