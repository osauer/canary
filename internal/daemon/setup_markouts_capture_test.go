package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

func markoutTarget(t *testing.T, side string, qty float64) rpc.SetupMarkoutTarget {
	t.Helper()
	return rpc.SetupMarkoutTarget{
		OrderRef: "ref-1", ExecID: "exec-1", Horizon: rpc.SetupMarkoutHorizonT30, Status: rpc.SetupMarkoutPending,
		Contract: rpc.ContractParams{ConID: 9001, Symbol: "SYNX", SecType: "STK", Currency: "USD", Multiplier: 1},
		Side:     side, FillQuantity: qty, FillPrice: 10, TargetAt: markoutET(t, "2026-10-01 10:30"),
	}
}

func markoutQuote(t *testing.T, bid, ask, bidSize, askSize float64, age time.Duration) setupMarkoutQuote {
	t.Helper()
	return setupMarkoutQuote{Bid: new(bid), Ask: new(ask), BidSize: new(bidSize), AskSize: new(askSize),
		AsOf: markoutET(t, "2026-10-01 10:30").Add(-age), DataType: rpc.MarketDataLive}
}

func TestSetupMarkoutRuleMarksLongsAtTheBidAndShortsAtTheAsk(t *testing.T) {
	t.Parallel()
	now := markoutET(t, "2026-10-01 10:30")
	long := applySetupMarkoutRule(markoutTarget(t, rpc.OrderActionBuy, 100), markoutQuote(t, 10.40, 10.45, 300, 100, 3*time.Second), now)
	if long.Status != rpc.SetupMarkoutCaptured || *long.MarkPrice != 10.40 || *long.Markout != 40 || *long.BidSize != 300 {
		t.Fatalf("long capture = %+v", long)
	}
	short := applySetupMarkoutRule(markoutTarget(t, rpc.OrderActionSell, 100), markoutQuote(t, 10.40, 10.45, 50, 100, 3*time.Second), now)
	if short.Status != rpc.SetupMarkoutCaptured || *short.MarkPrice != 10.45 || *short.Markout != -45 {
		t.Fatalf("short capture = %+v", short)
	}
	call := markoutTarget(t, rpc.OrderActionBuy, 2)
	call.Contract = rpc.ContractParams{ConID: 9101, Symbol: "SYNX", SecType: "OPT", Currency: "USD", Expiry: "20261120", Strike: 25, Right: "C", Multiplier: 100}
	call.FillPrice = 1.20
	got := applySetupMarkoutRule(call, markoutQuote(t, 1.10, 1.25, 2, 40, 0), now)
	if got.Status != rpc.SetupMarkoutCaptured || *got.Markout != -20 || got.Contract.Right != "C" || got.Contract.SecType != "OPT" {
		t.Fatalf("long call capture = %+v", got)
	}
	if got.Commission != nil {
		t.Fatal("markout invented a commission")
	}
}

func TestSetupMarkoutRuleRecordsTheFailingConditionWithoutPrices(t *testing.T) {
	t.Parallel()
	now := markoutET(t, "2026-10-01 10:30")
	for reason, mutate := range map[string]func(*rpc.SetupMarkoutTarget, *setupMarkoutQuote){
		"quote_not_two_sided":         func(_ *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.Ask = nil },
		"quote_not_positive_finite":   func(_ *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.Bid = new(-1.0) },
		"quote_crossed":               func(_ *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.Bid = new(10.50) },
		"quote_not_live":              func(_ *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.DataType = rpc.MarketDataDelayed },
		"quote_time_unavailable":      func(_ *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.AsOf = time.Time{} },
		"quote_after_target":          func(t *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.AsOf = t.TargetAt.Add(time.Millisecond) },
		"quote_too_old":               func(t *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.AsOf = t.TargetAt.Add(-61 * time.Second) },
		"displayed_size_unavailable":  func(_ *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.BidSize = nil },
		"displayed_size_insufficient": func(_ *rpc.SetupMarkoutTarget, q *setupMarkoutQuote) { q.BidSize = new(99.0) },
	} {
		target, quote := markoutTarget(t, rpc.OrderActionBuy, 100), markoutQuote(t, 10.40, 10.45, 300, 100, time.Second)
		mutate(&target, &quote)
		got := applySetupMarkoutRule(target, quote, now)
		if got.Status != rpc.SetupMarkoutMissing || got.Reason != reason {
			t.Fatalf("%s: status=%s reason=%s", reason, got.Status, got.Reason)
		}
		if got.Bid != nil || got.Ask != nil || got.BidSize != nil || got.AskSize != nil || got.MarkPrice != nil || got.Markout != nil {
			t.Fatalf("%s: missing target carried prices: %+v", reason, got)
		}
	}
	// Exactly sixty seconds old and a locked market are still valid.
	got := applySetupMarkoutRule(markoutTarget(t, rpc.OrderActionBuy, 100), markoutQuote(t, 10.40, 10.40, 100, 100, 60*time.Second), now)
	if got.Status != rpc.SetupMarkoutCaptured {
		t.Fatalf("boundary quote = %+v", got)
	}
}

func TestSetupMarkoutCaptureReadsOnlyInsideTheWindowAndResolvesEachFill(t *testing.T) {
	t.Parallel()
	fillAt := markoutET(t, "2026-10-01 10:00")
	clock := &markoutTestClock{at: fillAt.Add(time.Second)}
	s, _ := newMarkoutTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), clock)
	var reads atomic.Int32
	target := markoutET(t, "2026-10-01 10:30")
	s.setupMarkouts.quote = func(ctx context.Context, contract rpc.ContractParams, budget time.Duration) (setupMarkoutQuote, error) {
		reads.Add(1)
		if _, ok := ctx.Deadline(); !ok || budget != setupMarkoutQuoteBudget {
			t.Error("markout read is not bounded by the 5-second budget")
		}
		switch contract.ConID {
		case 9001:
			return setupMarkoutQuote{Bid: new(10.20), Ask: new(10.25), BidSize: new(500.0), AskSize: new(500.0), AsOf: target.Add(-3 * time.Second), DataType: rpc.MarketDataLive}, nil
		case 9101:
			return setupMarkoutQuote{}, errSetupMarkoutBrokerUnavailable
		default:
			return setupMarkoutQuote{}, errors.New("exact contract quote unavailable")
		}
	}
	stock, add := markoutFill(fillAt, "ref-1", "exec-1"), markoutFill(fillAt, "ref-2", "exec-2")
	add.ExecShares, add.LastFillPrice = 300, 10.10
	call := markoutFill(fillAt, "ref-3", "exec-3")
	call.SecType, call.ConID, call.Expiry, call.Strike, call.Right, call.Multiplier = "OPT", 9101, "20261120", 25, "C", 100
	other := markoutFill(fillAt, "ref-4", "exec-4")
	other.ConID = 9002
	for _, ev := range []orderJournalEvent{stock, add, call, other} {
		ev.At = clock.at
		s.scheduleSetupMarkoutFill(t.Context(), ev)
	}
	for _, at := range []time.Time{target.Add(-6 * time.Second), target.Add(-5 * time.Second)} {
		clock.at = at
		s.captureDueSetupMarkouts(t.Context(), at)
		s.setupMarkouts.wg.Wait()
	}
	if reads.Load() != 3 {
		t.Fatalf("reads = %d, want one per contract inside the window only", reads.Load())
	}
	s.sweepSetupMarkouts(t.Context(), target.Add(time.Second))
	rows := markoutsByKey(t, s)
	if got := rows["exec-1/t30"]; got.Status != rpc.SetupMarkoutCaptured || *got.Markout != 20 || got.ReadStartedAt.IsZero() || !got.QuoteAsOf.Equal(target.Add(-3*time.Second).UTC()) {
		t.Fatalf("stock capture = %+v", got)
	}
	if got := rows["exec-2/t30"]; got.Status != rpc.SetupMarkoutCaptured || *got.Markout != 30 {
		t.Fatalf("add on the same contract and clock = %+v", got)
	}
	if got := rows["exec-3/t30"]; got.Status != rpc.SetupMarkoutMissing || got.Reason != "broker_unavailable" || got.Contract.SecType != "OPT" {
		t.Fatalf("option without broker = %+v", got)
	}
	if got := rows["exec-4/t30"]; got.Status != rpc.SetupMarkoutMissing || got.Reason != "quote_unavailable" || got.Bid != nil {
		t.Fatalf("failed read = %+v", got)
	}
	for _, key := range []string{"exec-1/next_close", "exec-3/next_close"} {
		if rows[key].Status != rpc.SetupMarkoutPending {
			t.Fatalf("%s resolved before its window: %+v", key, rows[key])
		}
	}
}

func TestSetupMarkoutTargetWithoutAReadLineIsMissedNotCapturedLate(t *testing.T) {
	t.Parallel()
	fillAt := markoutET(t, "2026-10-01 10:00")
	clock := &markoutTestClock{at: fillAt.Add(time.Second)}
	s, _ := newMarkoutTestServer(t, filepath.Join(privateTestDir(t), "daemon.db"), clock)
	s.setupMarkouts.quote = func(context.Context, rpc.ContractParams, time.Duration) (setupMarkoutQuote, error) {
		t.Error("a target was read without a free market-data line")
		return setupMarkoutQuote{}, nil
	}
	ev := markoutFill(fillAt, "ref-1", "exec-1")
	ev.At = clock.at
	s.scheduleSetupMarkoutFill(t.Context(), ev)
	for range setupMarkoutMaxReads {
		s.setupMarkouts.reads <- struct{}{}
	}
	target := markoutET(t, "2026-10-01 10:30")
	s.captureDueSetupMarkouts(t.Context(), target.Add(-2*time.Second))
	s.sweepSetupMarkouts(t.Context(), target.Add(-2*time.Second))
	if rows := markoutsByKey(t, s); rows["exec-1/t30"].Status != rpc.SetupMarkoutPending {
		t.Fatal("a target inside its window resolved without a read")
	}
	s.captureDueSetupMarkouts(t.Context(), target.Add(time.Second))
	s.sweepSetupMarkouts(t.Context(), target.Add(time.Second))
	if got := markoutsByKey(t, s)["exec-1/t30"]; got.Status != rpc.SetupMarkoutMissing || got.Reason != "capture_window_missed" {
		t.Fatalf("passed target = %+v", got)
	}
}
