package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v3/internal/rpc"
)

// The list reads a strategy by the daemon's name for the held position, and
// a covered call with the shares it is written against and why no unit order
// carries it.
func TestStrategiesListNamesTheHeldPosition(t *testing.T) {
	res := goldenStrategiesNamed()
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: goldenConn{rpc.MethodPositionsList: res}}, "strategies", []string{"list"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{
		"strat-aaa-1  AAA  2 unit(s)  bull call spread  revision 3",
		"strat-bbb-1  BBB  2 unit(s)  covered call  revision 1",
		"short 1 × 20261218 C 50.00 BBB  conID 900011",
		"against 200 shares of BBB  conID 900010",
		"the call trades on its own order; the shares it is written against stay",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
}

// An add buys the combination, so its positive limit is what is paid and
// the count after it is what is held; a gate's warning is shown with it.
func TestStrategiesAddPreviewSaysWhatIsPaidAndHeld(t *testing.T) {
	preview := goldenStrategyAddPreview()
	var stdout, stderr bytes.Buffer
	if code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: goldenConn{rpc.MethodStrategyPreview: preview}}, "strategies", []string{"add", "strat-aaa-1", "3", "--units", "2"}); code != 0 {
		t.Fatalf("exit %d; stderr %s", code, stderr.String())
	}
	text := stdout.String()
	for _, want := range []string{"Add 2 strategy unit(s) to 3 held", "Pay up to 1.50 per strategy", "5 strategy unit(s) held", "Mandate budget: synthetic warning.", "SELL 2  20261218 P 135.00  -3 → -5"} {
		if !strings.Contains(text, want) {
			t.Errorf("text missing %q:\n%s", want, text)
		}
	}
	if code := Run(t.Context(), &Env{Stdout: &stdout, Stderr: &stderr, Conn: goldenConn{}}, "strategies", []string{"add", "strat-aaa-1", "3"}); code == 0 {
		t.Fatal("an add without --units ran")
	}
}

// goldenStrategiesNamed is a list with a named spread and a covered call
// listed with its shares and the reason no unit order carries it.
func goldenStrategiesNamed() rpc.PositionsResult {
	spread := goldenStrategy()
	spread.Kind, spread.Name = "vertical", "bull call spread"
	covered := rpc.PositionStrategy{ID: "strat-bbb-1", Revision: 1, Underlying: "BBB", Kind: "covered_call", Name: "covered call",
		Source: rpc.PositionStrategySourceInferred, Status: rpc.PositionStrategyStatusCurrent, Units: 2,
		Legs:   []rpc.PositionStrategyLeg{{Contract: rpc.ContractParams{Symbol: "BBB", SecType: "OPT", Expiry: "20261218", Right: "C", Strike: 50, TradingClass: "BBB", ConID: 900011}, Quantity: -2, Ratio: -1}},
		Stock:  &rpc.PositionStrategyLeg{Contract: rpc.ContractParams{Symbol: "BBB", SecType: "STK", ConID: 900010}, Quantity: 200, Ratio: 100},
		Reason: "the call trades on its own order; the shares it is written against stay"}
	return strategiesPositions(rpc.AccountDataScope{AccountID: "U1234567", AccountMode: rpc.AccountModeLive}, rpc.AccountDataAvailable, rpc.AccountDataFreshnessCurrent, "", []rpc.PositionStrategy{spread, covered})
}

// goldenStrategyAddPreview is the preview of two units added to a held put
// spread, with a gate's warning.
func goldenStrategyAddPreview() rpc.OrderPreviewResult {
	leg := func(conID, ratio int, action string, strike, before, after float64) rpc.StrategyOrderLeg {
		return rpc.StrategyOrderLeg{Contract: rpc.ContractParams{Symbol: "AAA", SecType: "OPT", Expiry: "20261218", Right: "P", Strike: strike, ConID: conID},
			Ratio: ratio, Action: action, Quantity: 2, Before: before, After: after}
	}
	return rpc.OrderPreviewResult{SubmitEligible: true, WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted},
		Warnings: []rpc.DataWarning{{Message: "Mandate budget: synthetic warning."}},
		Draft: rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 2, LimitPrice: 1.5, StrategyGroup: &rpc.StrategyOrderDraft{
			StrategyID: "strat-aaa-1", StrategyRevision: 3, Operation: rpc.StrategyOperationAdd, Units: 2, UnitsBefore: 3, UnitsAfter: 5, GuaranteedCombo: true,
			Legs: []rpc.StrategyOrderLeg{leg(900001, 1, rpc.OrderActionBuy, 145, 3, 5), leg(900002, -1, rpc.OrderActionSell, 135, -3, -5)}}}}
}

// goldenStrategyOpenInput is the two exact contracts of a bull call spread.
const goldenStrategyOpenInput = `{"long": {"con_id": 900011, "symbol": "AAA", "sec_type": "OPT", "exchange": "SMART", "currency": "USD", "expiry": "20261218", "strike": 100, "right": "C", "multiplier": 100, "trading_class": "AAA"},
 "short": {"con_id": 900012, "symbol": "AAA", "sec_type": "OPT", "exchange": "SMART", "currency": "USD", "expiry": "20261218", "strike": 110, "right": "C", "multiplier": 100, "trading_class": "AAA"}}`

// goldenStrategyOpenPreview is the preview of two units of a bull call
// spread opened from flat.
func goldenStrategyOpenPreview() rpc.OrderPreviewResult {
	leg := func(conID, ratio int, action string, strike, after float64) rpc.StrategyOrderLeg {
		return rpc.StrategyOrderLeg{Contract: rpc.ContractParams{Symbol: "AAA", SecType: "OPT", Expiry: "20261218", Right: "C", Strike: strike, ConID: conID},
			Ratio: ratio, Action: action, Quantity: 2, After: after}
	}
	return rpc.OrderPreviewResult{SubmitEligible: true, WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted},
		Draft: rpc.OrderDraft{Action: rpc.OrderActionBuy, Quantity: 2, LimitPrice: 3.45, Contract: rpc.ContractParams{Symbol: "AAA", SecType: "BAG"}, StrategyGroup: &rpc.StrategyOrderDraft{
			StrategyID: "strategy-aaa", Operation: rpc.StrategyOperationOpen, Units: 2, UnitsAfter: 2, GuaranteedCombo: true,
			Legs: []rpc.StrategyOrderLeg{leg(900011, 1, rpc.OrderActionBuy, 100, 2), leg(900012, -1, rpc.OrderActionSell, 110, -2)}}}}
}

// goldenOrdersOpenCombination is a working bull call spread Canary opened
// and a combination placed in TWS, as orders.open serves them.
func goldenOrdersOpenCombination() rpc.OrdersOpenResult {
	leg := func(action string, conID int, strike float64) rpc.OrderCombinationLeg {
		return rpc.OrderCombinationLeg{Action: action, Ratio: 1, Quantity: 1, ConID: conID, Symbol: "AAA", SecType: "OPT", Expiry: "20261218", Strike: strike, Right: "C", Multiplier: 100, TradingClass: "AAA", Exchange: "SMART"}
	}
	return rpc.OrdersOpenResult{AsOf: goldenAt, Account: "DU0000000", Mode: "paper", NotBrokerStatement: "local journal and broker session; not a broker statement",
		Orders: []rpc.OrderView{{OrderRef: "canary-spread-1", Symbol: "AAA", SecType: "BAG", Action: "BUY", OrderType: "LMT", TIF: "DAY", Quantity: 1, LimitPrice: 2.9,
			Status: "Submitted", LifecycleStatus: "submitted", UpdatedAt: goldenAt.Add(-3 * time.Minute), Origin: "agent",
			Combination: &rpc.OrderCombination{Source: rpc.OrderCombinationCanary, Strategy: "bull_call_spread", Kind: "vertical", Operation: "open", StrategyID: "strategy-aaa", Units: 1,
				Legs: []rpc.OrderCombinationLeg{leg("BUY", 900011, 100), leg("SELL", 900012, 110)}}}},
		Untracked: []rpc.OrderView{{PermID: 900000584, Symbol: "AAA", SecType: "BAG", Action: "SELL", OrderType: "LMT", TIF: "DAY", Quantity: 2, LimitPrice: 1.1,
			Status: "Submitted", LifecycleStatus: "working",
			Combination: &rpc.OrderCombination{Source: rpc.OrderCombinationBroker, Legs: []rpc.OrderCombinationLeg{{Action: "SELL", Ratio: 1, ConID: 900021, Exchange: "SMART"}, {Action: "BUY", Ratio: 1, ConID: 900022, Exchange: "SMART"}}}}},
		UntrackedStatus: rpc.OrdersUntrackedCurrent, UntrackedAsOf: goldenAt.Add(-4 * time.Minute), UntrackedControl: "on"}
}
