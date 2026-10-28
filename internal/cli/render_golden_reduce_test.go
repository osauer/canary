package cli

import (
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// The reduce goldens are a synthetic book: SYNA 100 shares and a SYNB call,
// marks of 25 and 2.50, tokens that are visibly made up.
const (
	goldenReduceToken     = "ibkrp4.synthetic-reduce-leg-1.sig"
	goldenReduceTokenB    = "ibkrp4.synthetic-reduce-leg-2.sig"
	goldenReduceTokenID   = "cmVkdWNlLXRva2VuLTE"
	goldenReduceTokenIDB  = "cmVkdWNlLXRva2VuLTI"
	goldenReduceStockCon  = 700001
	goldenReduceOptionCon = 700002
)

func goldenReduceStockContract() rpc.ContractParams {
	return rpc.ContractParams{ConID: goldenReduceStockCon, Symbol: "SYNA", SecType: "STK", Exchange: "SMART", Currency: "USD"}
}

func goldenReduceTerms() *rpc.TradeProposalReduceTerms {
	return &rpc.TradeProposalReduceTerms{PreviewTokenID: goldenReduceTokenID, ExpiresAt: time.Date(2026, 9, 5, 12, 10, 0, 0, time.UTC),
		Contract: goldenReduceStockContract(), Action: rpc.OrderActionSell, Quantity: 50, OrderType: rpc.OrderTypeLMT, LimitPrice: 24.66, TIF: rpc.OrderTIFDay,
		Algo: "Adaptive", AdaptivePriority: "Normal",
		PriceBound: rpc.TradeProposalReducePriceBound{Side: "bid", Reference: 24.9, ReferenceMid: 25, AllowancePct: 1, WorstPrice: 24.66}}
}

func goldenReduceOptionTerms() *rpc.TradeProposalReduceTerms {
	return &rpc.TradeProposalReduceTerms{PreviewTokenID: goldenReduceTokenIDB, ExpiresAt: time.Date(2026, 9, 5, 12, 10, 0, 0, time.UTC),
		Contract: rpc.ContractParams{ConID: goldenReduceOptionCon, Symbol: "SYNB", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20261218", Strike: 30, Right: "C", Multiplier: 100},
		Action:   rpc.OrderActionSell, Quantity: 2, OrderType: rpc.OrderTypeLMT, LimitPrice: 2.32, TIF: rpc.OrderTIFDay, AlgoNote: "Adaptive not supported for this contract",
		PriceBound: rpc.TradeProposalReducePriceBound{Side: "bid", Reference: 2.45, ReferenceMid: 2.5, AllowancePct: 5, WorstPrice: 2.32}}
}

func goldenReducePreview() *rpc.TradeProposalOrderPreview {
	bid, ask, mid := 24.9, 25.1, 25.0
	terms := goldenReduceTerms()
	return &rpc.TradeProposalOrderPreview{PreviewTokenID: goldenReduceTokenID, PreviewTokenScope: rpc.OrderTokenScopePlace, PreviewTokenExpiresAt: terms.ExpiresAt,
		TokenMinted: true, SubmitEligible: true, Mode: "paper", Account: "DU0000000", Endpoint: "127.0.0.1:4002", ClientID: 31,
		Draft: rpc.OrderDraft{AdaptivePriority: "Normal", Action: rpc.OrderActionSell, Contract: goldenReduceStockContract(), Quantity: 50, OrderType: rpc.OrderTypeLMT, LimitPrice: 24.66,
			TIF: rpc.OrderTIFDay, Strategy: rpc.OrderStrategyAllowanceLimit, Allowance: &rpc.OrderAllowanceLimit{Pct: 1, Reference: 24.9}, OrderRef: "canary-20260905-140000", OpenClose: "C", Source: "manual_reduce"},
		Quote:    rpc.OrderQuoteSnapshot{Symbol: "SYNA", Bid: &bid, Ask: &ask, Midpoint: &mid, DataType: rpc.MarketDataLive},
		Position: rpc.OrderPositionImpact{Before: 100, After: 50, Effect: rpc.OrderPositionEffectReduce},
		Notional: 1250, WhatIf: rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, AsOf: goldenAt}
}

func goldenReducePreviewResult() rpc.TradeProposalReduceResult {
	terms := goldenReduceTerms()
	return rpc.TradeProposalReduceResult{Accepted: true, ConID: goldenReduceStockCon, Symbol: "SYNA", SecType: "STK", Action: rpc.OrderActionSell, Percent: 50,
		PositionQuantity: 100, ReduceQuantity: 50, PreviewToken: goldenReduceToken, PreviewTokenID: goldenReduceTokenID, PreviewTokenExpiresAt: terms.ExpiresAt,
		Terms: terms, SubmitEligible: true, Preview: goldenReducePreview(), AsOf: goldenAt}
}

func goldenReducePlace() *rpc.OrderPlaceResult {
	return &rpc.OrderPlaceResult{Accepted: true, Mode: "paper", Account: "DU0000000", Endpoint: "127.0.0.1:4002", ClientID: 31, OrderRef: "canary-20260905-140000",
		PreviewTokenID: goldenReduceTokenID, ReservedOrderID: 1003, Draft: goldenReducePreview().Draft, OCAGroup: "canary-oca-canary-20260905-120000", OCAType: 2,
		LifecycleStatus: rpc.OrderLifecyclePendingSubmit, SendState: "send_attempted", Message: "paper broker placeOrder transmit attempted; waiting for broker lifecycle callback", AsOf: goldenAt}
}

func goldenReduceSubmitResult() rpc.TradeProposalReduceResult {
	res := goldenReducePreviewResult()
	res.PreviewToken, res.Preview = "", nil
	res.Place, res.OrderRef, res.Message, res.RequestRef = goldenReducePlace(), "canary-20260905-140000", "paper broker placeOrder transmit attempted; waiting for broker lifecycle callback", "desk-7f3a"
	return res
}

func goldenReduceSubmitRefused() rpc.TradeProposalReduceResult {
	res := goldenReduceSubmitResult()
	res.Accepted, res.Place, res.OrderRef, res.Message = false, nil, "", ""
	res.Blockers = []rpc.TradingBlocker{{Code: "price_moved", Message: "the mid moved to 25.4000, outside the reviewed 25.0000 ±0.50% (24.8750 to 25.1250)", Action: "Preview again at the current market; the reviewed limit was not sent."}}
	return res
}

func goldenReducePortfolioPreview() rpc.TradeProposalReducePortfolioResult {
	stock, option := goldenReduceTerms(), goldenReduceOptionTerms()
	pct := 100.0
	return rpc.TradeProposalReducePortfolioResult{Accepted: true, Percent: 50, NetDollarDeltaBefore: 3750, TargetDollarDelta: 1875, AchievedDollarDelta: 1875, AchievedPctOfTarget: &pct,
		LegCount: 2, EligibleCount: 2, TotalNotional: 1750, BaseCurrency: "USD", AsOf: goldenAt, NetDollarDeltaAfter: new(1875.0),
		IndexShockPct: 10, IndexLossBeforeBase: new(-1200.0), IndexLossAfterBase: new(300.0), IndexRisePct: 10, IndexRiseLossBeforeBase: new(900.0), IndexRiseLossAfterBase: new(-200.0),
		UntouchedPositions: []rpc.TradeProposalReduceUntouchedPosition{{
			Contract:         rpc.ContractParams{ConID: 700003, Symbol: "SYNC", SecType: "OPT", Expiry: "20261218", Strike: 35, Right: "P", Currency: "USD"},
			PositionQuantity: 2, Code: "locked", Reason: "Locked by you; never in a bulk reduction.",
		}},
		Legs: []rpc.TradeProposalReduceLeg{
			{ConID: goldenReduceStockCon, Symbol: "SYNA", SecType: "STK", Action: rpc.OrderActionSell, PositionQuantity: 100, ReduceQuantity: 50, DollarDelta: 2500, RiskContributionCut: 1250,
				Notional: 1250, NotionalCurrency: "USD", NotionalBase: new(1250.0), PreviewToken: goldenReduceToken, PreviewTokenID: goldenReduceTokenID, Terms: stock, SubmitEligible: true},
			{ConID: goldenReduceOptionCon, Symbol: "SYNB", SecType: "OPT", Action: rpc.OrderActionSell, PositionQuantity: 5, ReduceQuantity: 2, DollarDelta: 1250, RiskContributionCut: 625,
				Notional: 500, NotionalCurrency: "USD", NotionalBase: new(500.0), PreviewToken: goldenReduceTokenB, PreviewTokenID: goldenReduceTokenIDB, Terms: option, SubmitEligible: true},
		},
		Message: "2 eligible · 0 blocked of 2 legs · removing ~1875 of 1875 USD targeted net delta (100%)"}
}

// goldenReducePortfolioPlanLongs is a 25% longs plan: sized from the marks,
// no tokens, a locked hedge and a short kept, and scored on the index fall
// and rise, the loss measure and the trading cost.
// goldenReducePortfolioPlanProposals prices two of Canary's own proposals
// by key, each at its own size; its loss measure and cost are unknown.
func goldenReducePortfolioPlanProposals() rpc.TradeProposalReducePortfolioResult {
	before, after := 8300.0, 6900.0
	return rpc.TradeProposalReducePortfolioResult{Mode: rpc.ReduceModeProposals, PlanOnly: true, BaseCurrency: "USD", AsOf: goldenAt, LegCount: 2,
		IndexShockPct: 10, IndexLossBeforeBase: &before, IndexLossAfterBase: &after, IndexRisePct: 10, IndexRiseLossBeforeBase: new(-8100.0), IndexRiseLossAfterBase: new(-6800.0),
		LossMeasureUnknown: "SYNB 20261218 40 C: premium at risk unmeasured: no price paid",
		TradeCostUnknown:   "SYNC has no two-sided quote",
		Legs: []rpc.TradeProposalReduceLeg{
			{ConID: goldenReduceStockCon, Symbol: "SYNA", SecType: "STK", Action: rpc.OrderActionSell, PositionQuantity: 100, ReduceQuantity: 40},
			{ConID: 5151, Symbol: "SYNC", SecType: "STK", Action: rpc.OrderActionSell, PositionQuantity: 40, ReduceQuantity: 10},
		}}
}

func goldenReducePortfolioPlanLongs() rpc.TradeProposalReducePortfolioResult {
	before, after := 8300.0, 5100.0
	return rpc.TradeProposalReducePortfolioResult{Percent: 25, Mode: rpc.ReduceModeLongs, PlanOnly: true, BaseCurrency: "USD", AsOf: goldenAt,
		LegCount: 2, BlockedCount: 0, TotalNotional: 0,
		IndexShockPct: 10, IndexLossBeforeBase: &before, IndexLossAfterBase: &after, IndexRisePct: 10, IndexRiseLossBeforeBase: new(-8100.0), IndexRiseLossAfterBase: new(-4900.0),
		LossMeasureBeforeBase: new(41000.0), LossMeasureAfterBase: new(33500.0), LossRoomBase: new(69000.0),
		TradeCostBase: new(12.35),
		Kept:          []rpc.TradeProposalReduceKept{{ConID: goldenReduceOptionCon, Symbol: "SYNB", Reason: "locked", Role: rpc.HoldingLockRoleHedge}, {ConID: 7070, Symbol: "SYNS", Reason: "short"}},
		Legs: []rpc.TradeProposalReduceLeg{
			{ConID: goldenReduceStockCon, Symbol: "SYNA", SecType: "STK", Action: rpc.OrderActionSell, PositionQuantity: 100, ReduceQuantity: 25, DollarDelta: 2500, RiskContributionCut: 625, Notional: 625, NotionalCurrency: "USD", NotionalBase: new(625.0)},
			{ConID: 5151, Symbol: "SYNC", SecType: "STK", Action: rpc.OrderActionSell, PositionQuantity: 40, ReduceQuantity: 10, DollarDelta: 4000, RiskContributionCut: 1000, Notional: 1000, NotionalCurrency: "USD", NotionalBase: new(1000.0)},
		}}
}

// goldenReducePortfolioPlanHedge sells five Micro S&P contracts to neutral:
// the fall's loss falls to under one contract's offset, the rise's gain by
// as much, and the hedge's loss in the rise spends loss room (owner
// decision 2026-10-28 20:07 CET).
func goldenReducePortfolioPlanHedge() rpc.TradeProposalReducePortfolioResult {
	contract := rpc.ContractParams{ConID: 750001, Symbol: "MES", SecType: "FUT", Exchange: "CME", Currency: "USD", Expiry: "20261218", LocalSymbol: "MESZ6", TradingClass: "MES", Multiplier: 5}
	return rpc.TradeProposalReducePortfolioResult{Mode: rpc.ReduceModeHedge, PlanOnly: true, BaseCurrency: "EUR", AsOf: goldenAt, LegCount: 1, TotalNotional: 135000,
		IndexShockPct: 10, IndexLossBeforeBase: new(15000.0), IndexLossAfterBase: new(1500.0), IndexRisePct: 10, IndexRiseLossBeforeBase: new(-15000.0), IndexRiseLossAfterBase: new(-1500.0),
		LossMeasureBeforeBase: new(41000.0), LossMeasureAfterBase: new(54500.0), LossRoomBase: new(69000.0), TradeCostBase: new(5.63),
		Legs: []rpc.TradeProposalReduceLeg{{ConID: 750001, Symbol: "MES", SecType: "FUT", Action: rpc.OrderActionSell, ReduceQuantity: 5, DollarDelta: -135000,
			Notional: 150000, NotionalCurrency: "USD", NotionalBase: new(135000.0), Contract: &contract}},
		Message: "sell 5 MESZ6 to neutral"}
}

// goldenReducePortfolioPreviewHedge is the hedge previewed: the same leg
// with its token and exact terms, a limit at the bid, good for the day.
func goldenReducePortfolioPreviewHedge() rpc.TradeProposalReducePortfolioResult {
	res := goldenReducePortfolioPlanHedge()
	res.PlanOnly, res.Accepted, res.EligibleCount = false, true, 1
	res.RollDue, res.RollDueMessage = true, "MESZ6 expires 2026-12-18: roll the hedge to the next contract (due from 2026-12-08)."
	res.HedgeNotNeeded, res.HedgeNotNeededMessage = true, "The book without the hedge reads within the mandate: its loss measure € 41,000 is inside the loss room € 69,000. Buy the hedge back when you choose."
	leg := &res.Legs[0]
	leg.Notional, leg.PreviewToken, leg.PreviewTokenID, leg.SubmitEligible = 149993.75, goldenReduceToken, goldenReduceTokenID, true
	leg.Terms = &rpc.TradeProposalReduceTerms{PreviewTokenID: goldenReduceTokenID, ExpiresAt: time.Date(2026, 9, 5, 12, 10, 0, 0, time.UTC),
		Contract: *leg.Contract, Action: rpc.OrderActionSell, Quantity: 5, OrderType: rpc.OrderTypeLMT, LimitPrice: 5999.75, TIF: rpc.OrderTIFDay,
		PriceBound: rpc.TradeProposalReducePriceBound{Side: "bid", Reference: 5999.75, ReferenceMid: 6000, WorstPrice: 5999.75}}
	return res
}

// goldenReducePortfolioPlanHedgeNone is a book that already gains in the
// fall: nothing to sell, the figures unchanged.
func goldenReducePortfolioPlanHedgeNone() rpc.TradeProposalReducePortfolioResult {
	return rpc.TradeProposalReducePortfolioResult{Mode: rpc.ReduceModeHedge, PlanOnly: true, BaseCurrency: "EUR", AsOf: goldenAt, Legs: []rpc.TradeProposalReduceLeg{},
		IndexShockPct: 10, IndexLossBeforeBase: new(-2400.0), IndexLossAfterBase: new(-2400.0), IndexRisePct: 10, IndexRiseLossBeforeBase: new(1900.0), IndexRiseLossAfterBase: new(1900.0),
		LossMeasureBeforeBase: new(41000.0), LossMeasureAfterBase: new(41000.0), LossRoomBase: new(69000.0), TradeCostBase: new(0.0),
		Blockers: []rpc.TradingBlocker{{Code: "nothing_to_hedge", Message: "a 10% S&P fall already leaves the book even or ahead; selling futures would add a short", Action: "Keep the book as it is."}}}
}

func goldenReducePortfolioSubmit() rpc.TradeProposalReducePortfolioResult {
	res := goldenReducePortfolioPreview()
	res.Accepted, res.EligibleCount, res.BlockedCount, res.TotalNotional, res.RequestRef = false, 1, 1, 1250, "desk-basket-7f3a"
	res.NetDollarDeltaBefore, res.TargetDollarDelta, res.AchievedDollarDelta, res.AchievedPctOfTarget = 0, 0, 0, nil
	res.NetDollarDeltaAfter, res.UntouchedPositions = nil, nil
	res.Message = "1 placed · 1 blocked of 2 legs"
	for i := range res.Legs {
		res.Legs[i].PreviewToken, res.Legs[i].DollarDelta, res.Legs[i].RiskContributionCut = "", 0, 0
	}
	res.Legs[0].Placed, res.Legs[0].Place, res.Legs[0].OrderRef, res.Legs[0].Message = true, goldenReducePlace(), "canary-20260905-140000", "paper broker placeOrder transmit attempted; waiting for broker lifecycle callback"
	res.Legs[1].PositionQuantity = 1
	res.Legs[1].Blockers = []rpc.TradingBlocker{{Code: "position_changed", Message: "SYNB is now 1 held; the reviewed reduction of 2 exceeds it", Action: "Preview the reduction again at the current position; nothing was sent."}}
	return res
}
