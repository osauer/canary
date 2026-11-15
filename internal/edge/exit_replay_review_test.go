package edge

import (
	"testing"

	"github.com/osauer/canary/v3/internal/rpc"
)

func TestExitReplayExplicitFromDoesNotIncludePriorTrades(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9001, "SYNX", "C", 100, "20261016")
	b.steadyBars("SYNX", 100, nil)
	b.held("2026-09-01")
	b.trade(9001, "2026-09-02", 1, 2, 0, "O", 0, 0)
	b.held("2026-09-04", position(9001, 1, 2, 200))
	in := ExitReplayInput{Statements: b.statements(), BaseCurrency: "USD", From: replayDay("2026-09-03"), Rules: []string{rpc.EdgeReplaySetActual}, Policy: replayTestPolicy(), Underlyings: b.bars}
	got := ExitReplay(in)
	if got.State != rpc.EdgeStateInsufficient || got.Reason != "start_snapshot_unavailable" {
		t.Fatalf("missing baseline must refuse the requested window: state=%s reason=%s from=%s", got.State, got.Reason, got.From)
	}
	b.held("2026-09-02", position(9001, 1, 2, 200))
	in.Statements = b.statements()
	got = ExitReplay(in)
	if got.State != rpc.EdgeStateCurrent || got.From.Before(in.From) {
		t.Fatalf("adjacent baseline should allow the window: state=%s reason=%s from=%s", got.State, got.Reason, got.From)
	}
}
func TestExitReplayPreheldHighWaterExcludesClosedPriorCycle(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9001, "SYNX", "C", 100, "20261016")
	b.steadyBars("SYNX", 100, nil)
	b.held("2026-09-01")
	b.trade(9001, "2026-09-02", 1, 2, 0, "O", 0, 0)
	b.held("2026-09-02", position(9001, 1, 10, 200))
	b.trade(9001, "2026-09-03", -1, 10, 0, "C", 0, 800)
	b.held("2026-09-03")
	b.trade(9001, "2026-09-10", 1, 2, 0, "O", 0, 0)
	b.held("2026-09-11", position(9001, 1, 2, 200))
	b.held("2026-09-14", position(9001, 1, 2, 200))
	got := ExitReplay(ExitReplayInput{Statements: b.statements(), BaseCurrency: "USD", From: replayDay("2026-09-14"), Rules: []string{rpc.EdgeReplaySetActual, rpc.EdgeReplayRuleProfitTrail}, Policy: replayTestPolicy(), Underlyings: b.bars})
	run := replayRun(t, got, rpc.EdgeReplayRuleProfitTrail, rpc.EdgeReplayPricesRecorded)
	if run.Exits != 0 {
		t.Fatalf("unchanged new cycle exited from earlier closed cycle's high: exits=%d", run.Exits)
	}
}

func TestExitReplayPreheldHighWaterResetsAfterHistoricalAdd(t *testing.T) {
	b := newReplayBook(t, "USD")
	b.option(9001, "SYNX", "C", 100, "20261016")
	b.steadyBars("SYNX", 100, nil)
	b.held("2026-09-01")
	b.trade(9001, "2026-09-02", 1, 2, 0, "O", 0, 0)
	b.held("2026-09-02", position(9001, 1, 10, 200))
	b.trade(9001, "2026-09-10", 1, 2, 0, "O", 0, 0)
	b.held("2026-09-11", position(9001, 2, 2, 400))
	b.held("2026-09-14", position(9001, 2, 2, 400))
	got := ExitReplay(ExitReplayInput{Statements: b.statements(), BaseCurrency: "USD", From: replayDay("2026-09-14"), Rules: []string{rpc.EdgeReplaySetActual, rpc.EdgeReplayRuleProfitTrail}, Policy: replayTestPolicy(), Underlyings: b.bars})
	run := replayRun(t, got, rpc.EdgeReplayRuleProfitTrail, rpc.EdgeReplayPricesRecorded)
	if run.Exits != 0 {
		t.Fatalf("historical add failed to reset high water: exits=%d", run.Exits)
	}
}
