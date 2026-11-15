package cli

import (
	"time"

	"github.com/osauer/canary/v3/internal/core"
	"github.com/osauer/canary/v3/internal/rpc"
)

// goldenCore is a synthetic core target on a Xetra line: the 31 August
// decision with the trend on and the 15% floor binding, a first buy cut by
// the mandate's headroom. The policy names the fund by ISIN and currency;
// the contract is IBKR's. Every figure is synthetic.
func goldenCore() rpc.CoreTargetResult {
	conID := 9030
	scale, band, minimum, maxValue, coreVol := 100.0, 10.0, 2500.0, 40.0, 6.0
	holdable := 908
	on := core.Sizing{AllocationPct: 6, ScalePct: 100, MaxValuePct: 40, BudgetBase: 230000, Room: 1}
	size := core.Size{SleeveBudget: 13800, Cap: 92000, Notional: 92000, TargetShares: 958}
	return rpc.CoreTargetResult{AsOf: goldenAt, Active: true, Status: rpc.CoreStatusReady, ReadOnly: false,
		Rule: core.RuleID, SpecID: core.SpecID, SpecHash: core.SpecHash, PolicyRule: core.RuleID,
		Stage:             &rpc.CoreStageView{Written: "live", State: "live", ReservedPct: 100, SpecHashCited: true},
		PolicyFingerprint: &rpc.Fingerprint{Version: rpc.RiskConstitutionFingerprintVersion, Key: "sha256:synthetic"},
		Policy: &rpc.CorePolicyView{Rule: core.RuleID, ISIN: core.ISIN, Currency: core.Currency,
			ScalePct: &scale, BandPct: &band, MinOrderBase: &minimum, MaxValuePct: &maxValue, CoreVolPct: &coreVol},
		Instrument: &rpc.CoreInstrument{Symbol: core.Symbol, ISIN: core.ISIN, ConID: conID, Exchange: "IBIS", Currency: core.Currency, Resolved: true, TradingClass: core.Symbol,
			Calendar: "de_xetra", CalendarLabel: "Xetra", ResolvedAt: goldenAt.Add(-2 * time.Hour)},
		Decision: &rpc.CoreDecision{Status: core.StatusOK, Decision: "2026-08-31", WindowFrom: "2025-08-26",
			MonthEnds:      []string{"2025-11-28", "2025-12-30", "2026-01-30", "2026-02-27", "2026-03-31", "2026-04-30", "2026-05-29", "2026-06-30", "2026-07-31", "2026-08-31"},
			MonthEndCloses: []float64{86.39, 89.83, 88.91, 85.3, 86.39, 89.03, 90.02, 92.62, 90.53, 95.95},
			Close:          95.95, SMA10: 89.497, TrendOn: true, SigmaEWMA: 0.1300579, Sigma252: 0.1158550, Sigma: 0.15, Binding: core.BindingFloor, Digest: "0e98bf0c9b554798",
			FrozenAt: goldenAt.Add(-5 * 24 * time.Hour)},
		Sizing:  &rpc.CoreSizingView{Frozen: size, FrozenOn: on, Current: size, CurrentOn: on, Target: 958, TargetBase: 92063.8, Holdable: &holdable},
		Holding: &rpc.CoreHolding{Shares: 0, Price: 96.1, ValueBase: 0, PriceSource: "close"},
		Action: &core.Decision{Kind: core.ActionBuy, Quantity: 908, Why: "first buy, cut by the mandate's headroom from 958 shares", Diff: 958, BandShares: 95.8,
			OrderValue: 87258.8, HeadroomCut: true},
		Mandate:      &rpc.CoreMandateView{BaseCurrency: "EUR", Status: "within", EvaluatedAt: goldenAt.Add(-time.Minute), BudgetAnnualSD: 34500, RiskAnnualSD: 3267},
		NextDecision: "2026-09-30",
		Evidence:     &rpc.CoreEvidence{SeriesKey: "core:con:9030", Closes: 320, LastClose: "2026-09-04", FetchedAt: goldenAt.Add(-9 * time.Hour), Source: "IBKR historical bars · TRADES", Calendar: "de_xetra"},
		Row:          &rpc.CoreRowView{Key: "core_rebalance:synthetic01", Revision: "r1", Action: "BUY", Quantity: 908, ReferencePrice: 96.1, Session: "2026-09-04", State: rpc.TradeProposalStateGenerated, Readiness: "ready", Terms: goldenCoreTerms()},
		LastDecision: &rpc.CoreDecisionRecord{Decision: "2026-08-31", DecidedAt: goldenAt.Add(-5 * 24 * time.Hour), Stage: "live", Kind: core.ActionBuy, Quantity: 908, Why: "first buy, cut by the mandate's headroom from 958 shares",
			Target: 958, Held: 0, Price: 95.95, Window: goldenCoreTerms().Window, RowKey: "core_rebalance:synthetic01", Outcome: rpc.CoreDecisionOutcome{Served: true}},
	}
}

// goldenCoreTerms are the synthetic row's order terms: the five sessions
// after the 31 August decision the US also trades.
func goldenCoreTerms() rpc.CoreRowTerms {
	return rpc.CoreRowTerms{OrderType: "LMT", TIF: "DAY", Limit: "the live mid, snapped to the tick grid, never crossing the mid", Exchange: "SMART", PrimaryExchange: "IBIS",
		Window: []string{"2026-09-01", "2026-09-02", "2026-09-03", "2026-09-04", "2026-09-08"}, Episode: "core:synthetic:paper:2026-08"}
}

// goldenCoreHold is a core that holds: the decision day's close has not
// arrived and a key is unwritten, so no row stands and nothing is recorded.
func goldenCoreHold() rpc.CoreTargetResult {
	res := goldenCore()
	res.Status = rpc.CoreStatusHold
	res.Reasons = []string{"core keys are unwritten: mandate.core.band_pct", "the decision waits for IBCF's close of 2026-08-31"}
	res.Decision, res.Sizing, res.Row, res.LastDecision = nil, nil, nil, nil
	res.Action = &core.Decision{Kind: core.ActionHold, Why: "the core holds: buys stop, and a sale the rule wants goes to you for review"}
	res.Evidence.Stale = true
	return res
}

// goldenCoreTrendOff sells the holding: the trend is off, its sale row is
// blocked while an order of the line works, and the last decision's row was
// sent after the owner's confirmation.
func goldenCoreTrendOff() rpc.CoreTargetResult {
	res := goldenCore()
	res.Row = &rpc.CoreRowView{Key: "core_rebalance:synthetic02", Revision: "r2", Action: "SELL", Quantity: 640, ReferencePrice: 84.3, Session: "2026-09-04", State: rpc.TradeProposalStateBlocked, Readiness: "not_executable", Terms: goldenCoreTerms(),
		Blockers: []rpc.TradingBlocker{{Code: "existing_reduction_order", Message: "an order to sell 200 of this contract is already working (LMT); this row waits until it fills or is cancelled, then recomputes from the new position"}}}
	res.LastDecision = &rpc.CoreDecisionRecord{Decision: "2026-08-31", DecidedAt: goldenAt.Add(-5 * 24 * time.Hour), Stage: "live", Kind: core.ActionSellAll, Quantity: 840, Why: "the trend is off: sell every share; the band and the minimum do not apply",
		Target: 0, Held: 840, Price: 84.1, Window: goldenCoreTerms().Window, RowKey: "core_rebalance:synthetic02", Outcome: rpc.CoreDecisionOutcome{Served: true, Submitted: true, SubmittedAt: goldenAt.Add(-4 * 24 * time.Hour), OrderRef: "canary-synthetic-7"}}
	res.Decision.TrendOn, res.Decision.Close = false, 84.1
	res.Decision.Binding, res.Decision.Sigma, res.Decision.SigmaEWMA = core.BindingEWMA, 0.231, 0.231
	zero := core.Size{SleeveBudget: 13800, Cap: 92000}
	res.Sizing = &rpc.CoreSizingView{Frozen: zero, FrozenOn: res.Sizing.FrozenOn, Current: zero, CurrentOn: res.Sizing.CurrentOn}
	res.Holding = &rpc.CoreHolding{Shares: 640, Price: 84.3, ValueBase: 53952, PriceSource: "mark"}
	res.Action = &core.Decision{Kind: core.ActionSellAll, Quantity: 640, Why: "the trend is off: sell every share; the band and the minimum do not apply", Diff: -640, OrderValue: 53952}
	return res
}

// goldenCoreResolve lists two synthetic listings of the rule's ISIN, none
// pinned, with every form's answer.
func goldenCoreResolve() rpc.CoreResolveResult {
	return rpc.CoreResolveResult{AsOf: goldenAt, ISIN: core.ISIN, ISINSource: "rule",
		Listings: []rpc.CoreListing{
			{ConID: 900001, Symbol: "IBCF", LocalSymbol: "IBCF", SecType: "STK", Exchange: "SMART", PrimaryExch: "IBIS", Currency: "EUR", TradingClass: "IBCF", LongName: "Synthetic Index Fund IBCF", ISIN: core.ISIN, Forms: []string{"STK by ISIN on SMART", "STK IBCF on IBIS"}},
			{ConID: 900002, Symbol: "IUSE", LocalSymbol: "IUSE", SecType: "STK", Exchange: "SMART", PrimaryExch: "LSEETF", Currency: "EUR", TradingClass: "IUSE", LongName: "Synthetic Index Fund IUSE", ISIN: core.ISIN, Forms: []string{"STK by ISIN on SMART"}},
		},
		Attempts: []rpc.CoreResolveAttempt{
			{Form: "STK by ISIN on SMART", Outcome: "line", Lines: 2},
			{Form: "STK by ISIN, no exchange", Outcome: "rejected", Code: 321, Message: "Error validating request: exchange required"},
			{Form: "STK IBCF on IBIS", Outcome: "line", Lines: 1},
			{Form: "STK IBCF on IBIS2", Outcome: "rejected", Code: 200, Message: "No security definition has been found for the request"},
			{Form: "STK IBCF on SMART (primary IBIS)", Outcome: "no_line", Message: "the 1 lines IBKR answered name another ISIN"},
		},
		Message: "2 listings match; none is chosen for you. While more than one is in the policy's currency, the core holds until you write the con_id of its line into [mandate.core] with a higher policy_version."}
}

// goldenCoreResolvePinned is the same lookup with the policy's con_id among
// the listings.
func goldenCoreResolvePinned() rpc.CoreResolveResult {
	res := goldenCoreResolve()
	res.ISINSource = "policy"
	res.PinnedConID, res.PinnedFound = new(900001), true
	res.Listings[0].Pinned, res.Listings[0].Used = true, true
	res.Message = "[mandate.core] con_id 900001 is among the listings; nothing to write."
	return res
}

// goldenCoreResolveNone found nothing.
func goldenCoreResolveNone() rpc.CoreResolveResult {
	return rpc.CoreResolveResult{AsOf: goldenAt, ISIN: "IE00B5BMR087", ISINSource: "flag", Currency: "EUR", Listings: []rpc.CoreListing{},
		Attempts: []rpc.CoreResolveAttempt{
			{Form: "STK by ISIN on SMART", Outcome: "rejected", Code: 200, Message: "No security definition has been found for the request"},
			{Form: "STK by ISIN, no exchange", Outcome: "no_line", Message: "the search ended without a line"},
		},
		Message: "IBKR found no listing for IE00B5BMR087; every form's answer is listed. Check the ISIN, or ask again with --currency."}
}

// goldenCoreAmbiguous holds: IBKR lists two lines under the ISIN in the
// policy's currency, and none is chosen for the owner.
func goldenCoreAmbiguous() rpc.CoreTargetResult {
	res := goldenCoreHold()
	res.Reasons = []string{"IBKR lists 2 EUR lines under IE00B3ZW0K18: contract 900001 IBCF on IBIS; contract 900002 IXSE (class EXXT) on LSEETF. None is chosen for you: write the con_id of the core's line into [mandate.core] with a higher policy_version"}
	res.Instrument = &rpc.CoreInstrument{Symbol: core.Symbol, ISIN: core.ISIN, Currency: core.Currency}
	res.Evidence, res.NextDecision, res.Holding = nil, "", &rpc.CoreHolding{}
	return res
}

// goldenCoreLondon holds on a London line whose series read IBKR refused.
func goldenCoreLondon() rpc.CoreTargetResult {
	res := goldenCoreHold()
	refusedAt := goldenAt.Add(-3 * time.Hour)
	refusal := "No security definition has been found for the request"
	res.Reasons = []string{"no IXSE daily history: the last read of contract 900002 IXSE (class EXXT) on LSEETF failed at " + refusedAt.UTC().Format(time.DateTime) + " UTC: " + refusal}
	res.Instrument = &rpc.CoreInstrument{Symbol: "IXSE", ISIN: core.ISIN, ConID: 900002, Exchange: "LSEETF", Currency: core.Currency, Resolved: true, TradingClass: "EXXT",
		Calendar: "uk_lse", CalendarLabel: "London SETS", ResolvedAt: goldenAt.Add(-2 * time.Hour)}
	res.Holding = &rpc.CoreHolding{}
	res.Evidence = &rpc.CoreEvidence{SeriesKey: "core:con:900002", Missing: true, Calendar: "uk_lse", ReadError: refusal, ReadErrorAt: refusedAt}
	return res
}

// goldenCoreShadow is the core in shadow, as the owner wrote it: its own
// verdict waits, and its shadow record holds a month's decision, intent and
// fill on the paper position, newest first, and an earlier month's fill
// whose close was missing.
func goldenCoreShadow() rpc.CoreTargetResult {
	res := goldenCore()
	res.Stage = &rpc.CoreStageView{Written: "shadow", State: "shadow", SpecHashCited: true}
	res.Row, res.LastDecision, res.ReadOnly = nil, nil, true
	res.Action = &core.Decision{Kind: core.ActionWait, Why: "no buy: the core runs in shadow and sends no orders, so it reserves no allocation", Diff: 958, BandShares: 95.8}
	base := rpc.ShadowRecord{Version: 1, Strategy: "core", Rule: core.RuleID, SpecHash: core.SpecHash, SpecCited: true, Stage: "shadow", RecordedAt: goldenAt}
	fill, intent, decision, missed := base, base, base, base
	fill.Kind, fill.Session, fill.Fill = rpc.ShadowKindFill, "2026-09-01", &rpc.ShadowFill{Quantity: 958, Price: 96.12, HoldingAfter: 958, Convention: rpc.ShadowFillConvention}
	intent.Kind, intent.Session, intent.Intent = rpc.ShadowKindIntent, "2026-08-31", &rpc.ShadowIntent{ForSession: "2026-09-01", Kind: core.ActionBuy, Quantity: 958, Why: "first buy toward the target", Diff: 958, BandShares: 95.8}
	decision.Kind, decision.Session, decision.Decision = rpc.ShadowKindDecision, "2026-08-31", &rpc.ShadowDecision{TrendOn: true, Sigma: 0.15, Binding: core.BindingFloor, TargetShares: 958, ScalePct: 100}
	missed.Kind, missed.Session, missed.Fill = rpc.ShadowKindFill, "2026-08-03", &rpc.ShadowFill{Convention: rpc.ShadowFillConvention, Unobserved: "no close of 2026-08-03 in the series; nothing is guessed"}
	res.Shadow = &rpc.ShadowView{Strategy: "core", Stage: "shadow", Holding: 958, Records: 6, Recent: []rpc.ShadowRecord{fill, intent, decision, missed}}
	return res
}

// goldenStrategyShadow is a register entry on the parameterised rule, a
// synthetic trend fund on a London line in the overlay, in shadow: its
// month's decision, the first paper buy and its fill, and the kill rule's
// drawdown on the record with no bound written. Every figure is synthetic.
func goldenStrategyShadow() rpc.StrategyTargetResult {
	res := goldenCore()
	// An entry's rows are not built: read-only, no row, no decision record.
	res.Row, res.LastDecision, res.ReadOnly = nil, nil, true
	hash := "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	scale, band, minimum, maxValue, allocation := 25.0, 10.0, 2500.0, 40.0, 4.0
	res.Strategy, res.Sleeve, res.Rule, res.SpecID, res.SpecHash, res.PolicyRule, res.Signal = "trend-fund", "overlay", "timed-holding-v1", "", hash, "timed-holding-v1", "trend-sma10m"
	res.Stage = &rpc.CoreStageView{Written: "shadow", State: "shadow", SpecHashCited: true}
	res.Policy = &rpc.CorePolicyView{Rule: "timed-holding-v1", ISIN: "ZZ0000000001", Currency: "EUR", Signal: "trend-sma10m", ScalePct: &scale, BandPct: &band, MinOrderBase: &minimum, MaxValuePct: &maxValue, AllocationPct: &allocation}
	res.Instrument = &rpc.CoreInstrument{Symbol: "SYNF", ISIN: "ZZ0000000001", ConID: 9040, Exchange: "LSEETF", Currency: "EUR", Resolved: true, TradingClass: "SYNF",
		Calendar: "uk_lse", CalendarLabel: "London SETS", ResolvedAt: goldenAt.Add(-2 * time.Hour)}
	on := core.Sizing{AllocationPct: 4, ScalePct: 25, MaxValuePct: 40, BudgetBase: 230000, Room: 1}
	size := core.Size{SleeveBudget: 9200, Cap: 92000, Notional: 61333.33, TargetShares: 159}
	res.Sizing = &rpc.CoreSizingView{Frozen: size, FrozenOn: on, Current: size, CurrentOn: on, Target: 159, TargetBase: 15280.5}
	res.Action = &core.Decision{Kind: core.ActionWait, Why: "no buy: the trend-fund strategy runs in shadow and sends no orders, so it reserves no allocation", Diff: 159, BandShares: 15.9}
	res.Kill = &rpc.KillView{DrawdownK: 0.0312, Measured: true, Decisions: 2, Note: "kill_drawdown_k unwritten: the kill rule cannot fire until the lab's design run writes its bound"}
	res.Evidence = &rpc.CoreEvidence{SeriesKey: "strategy:trend-fund:con:9040", Closes: 320, LastClose: "2026-09-04", FetchedAt: goldenAt.Add(-9 * time.Hour), Source: "IBKR historical bars · TRADES", Calendar: "uk_lse"}
	base := rpc.ShadowRecord{Version: 1, Strategy: "trend-fund", Rule: "timed-holding-v1", SpecHash: hash, SpecCited: true, Stage: "shadow", RecordedAt: goldenAt}
	fill, intent, decision := base, base, base
	fill.Kind, fill.Session, fill.Fill = rpc.ShadowKindFill, "2026-09-01", &rpc.ShadowFill{Quantity: 159, Price: 96.12, HoldingAfter: 159, Convention: rpc.ShadowFillConvention}
	intent.Kind, intent.Session, intent.Intent = rpc.ShadowKindIntent, "2026-08-31", &rpc.ShadowIntent{ForSession: "2026-09-01", Kind: core.ActionBuy, Quantity: 159, Why: "first buy toward the target", Diff: 159, BandShares: 15.9}
	decision.Kind, decision.Session, decision.Decision = rpc.ShadowKindDecision, "2026-08-31", &rpc.ShadowDecision{TrendOn: true, Sigma: 0.15, Binding: core.BindingFloor, TargetShares: 159, ScalePct: 25, Close: 95.95, SleeveBudget: 9200}
	res.Shadow = &rpc.ShadowView{Strategy: "trend-fund", Stage: "shadow", Holding: 159, Records: 3, Recent: []rpc.ShadowRecord{fill, intent, decision}}
	return res
}

// goldenStrategyLive is a live entry with its month's row pending, its last
// decision served, and its kill rule measured on its own fills against a
// written bound.
func goldenStrategyLive() rpc.StrategyTargetResult {
	res := goldenStrategyShadow()
	res.ReadOnly, res.Shadow = false, nil
	res.Sleeve = "trend"
	res.Stage = &rpc.CoreStageView{Written: "live", State: "live", ReservedPct: 25, SpecHashCited: true}
	res.Action = &core.Decision{Kind: core.ActionBuy, Quantity: 159, Why: "first buy toward the target", Diff: 159, BandShares: 15.9}
	terms := goldenCoreTerms()
	terms.PrimaryExchange, terms.Episode = "LSEETF", "strategy:trend-fund:synthetic:paper:2026-08"
	res.Row = &rpc.CoreRowView{Key: "strategy_rebalance:synthetic03", Revision: "r1", Action: "BUY", Quantity: 159, ReferencePrice: 96.1, Session: "2026-09-04", State: rpc.TradeProposalStateGenerated, Readiness: "opening_window", Terms: terms}
	res.LastDecision = &rpc.CoreDecisionRecord{Decision: "2026-08-31", DecidedAt: goldenAt.Add(-5 * 24 * time.Hour), Stage: "live", Kind: core.ActionBuy, Quantity: 159, Why: "first buy toward the target",
		Target: 159, Held: 0, Price: 95.95, Window: terms.Window, RowKey: "strategy_rebalance:synthetic03", Outcome: rpc.CoreDecisionOutcome{Served: true}}
	bound := 0.35 // a bound of 1 K or more refuses a live entry's file (2026-10-19)
	res.Kill = &rpc.KillView{MaxDrawdownK: &bound, DrawdownK: 0.0125, Measured: true, Decisions: 1, Source: rpc.KillSourceOwnFills, ScalePct: 25}
	return res
}

// goldenStrategySignal is the live entry measured on its fund's older
// share class while its own line's closes do not fill the window.
func goldenStrategySignal() rpc.StrategyTargetResult {
	res := goldenStrategyLive()
	res.Decision.SignalFrom, res.Decision.TradedClose = "SYNS (ZZ0000000002) in USD", 96.1
	res.Evidence.Signal = &rpc.CoreSignalEvidence{ISIN: "ZZ0000000002", Currency: "GBP", MeasuredIn: "USD", Symbol: "SYNS", ConID: 9050, Calendar: "uk_lse",
		SeriesKey: "strategy:trend-fund:signal:con:9050", Closes: 320, LastClose: "2026-09-04", FXKey: "strategy:trend-fund:fx:GBPUSD", InUse: true, OwnSessions: 109}
	return res
}

// goldenStrategyHold is an entry that holds: a key unwritten, so nothing is
// resolved or decided.
func goldenStrategyHold() rpc.StrategyTargetResult {
	res := goldenStrategyShadow()
	res.Status = rpc.CoreStatusHold
	res.Stage = &rpc.CoreStageView{Written: "shadow", State: "hold", SpecHashCited: true}
	res.Reasons = []string{"mandate.strategies.trend-fund.band_pct unwritten"}
	res.Policy.BandPct, res.Policy.Unapproved = nil, []string{"mandate.strategies.trend-fund.band_pct"}
	res.Instrument = &rpc.CoreInstrument{Symbol: "ZZ0000000001", ISIN: "ZZ0000000001", Currency: "EUR"}
	res.Decision, res.Sizing, res.Evidence, res.Kill, res.NextDecision = nil, nil, nil, nil, ""
	res.Holding = &rpc.CoreHolding{}
	res.Action = &core.Decision{Kind: core.ActionHold, Why: "the trend-fund strategy holds: buys stop, and a sale the rule wants goes to you for review"}
	res.Shadow = &rpc.ShadowView{Strategy: "trend-fund", Stage: "hold", Holding: 159, Records: 3, Recent: res.Shadow.Recent}
	return res
}

// goldenStrategyReplay is a core replay over a synthetic year: a not_live
// month, a first buy, a month inside the band, a wait below the minimum, a
// sale of every share when the trend turns off, and a bar on a closed day.
func goldenStrategyReplay() rpc.StrategyReplayResult {
	in := goldenCore().Instrument
	closes := []float64{95.1, 96.4, 97.2, 98.9, 101.3, 104.2, 103.9, 105.5, 101.2, 97.8}
	rows := []core.ReplayRow{
		{Decision: "2025-01-31", Status: core.StatusNotLive, Reason: "needs 253 sessions to 2025-01-31, has 210", Action: core.ActionHold},
		{Decision: "2025-02-28", Status: core.StatusOK, WindowFrom: "2024-02-29", Close: 101.3, SMA10: 97.47, TrendOn: true, SigmaEWMA: 0.1312, Sigma252: 0.1388, Sigma: 0.15, Binding: core.BindingFloor, Notional: 100000, TargetShares: 987, Action: core.ActionBuy, Quantity: 987, HeldAfter: 987},
		{Decision: "2025-03-31", Status: core.StatusOK, WindowFrom: "2024-03-28", Close: 104.2, SMA10: 98.9, TrendOn: true, SigmaEWMA: 0.1402, Sigma252: 0.1391, Sigma: 0.15, Binding: core.BindingFloor, Notional: 100000, TargetShares: 959, Action: core.ActionNone, HeldBefore: 987, HeldAfter: 987},
		{Decision: "2025-04-30", Status: core.StatusOK, WindowFrom: "2024-04-30", Close: 103.9, SMA10: 99.6, TrendOn: true, SigmaEWMA: 0.1512, Sigma252: 0.1405, Sigma: 0.1512, Binding: core.BindingEWMA, Notional: 99206.35, TargetShares: 954, Action: core.ActionNone, HeldBefore: 987, HeldAfter: 987},
		{Decision: "2025-05-30", Status: core.StatusOK, WindowFrom: "2024-05-31", Close: 105.5, SMA10: 100.3, TrendOn: true, SigmaEWMA: 0.1498, Sigma252: 0.1402, Sigma: 0.15, Binding: core.BindingFloor, Notional: 100000, TargetShares: 947, Action: core.ActionNone, HeldBefore: 987, HeldAfter: 987},
		{Decision: "2025-06-30", Status: core.StatusHold, Reason: "no close on 2025-06-12", Action: core.ActionHold, HeldBefore: 987, HeldAfter: 987},
		{Decision: "2025-07-31", Status: core.StatusOK, WindowFrom: "2024-08-01", Close: 97.8, SMA10: 101.1, TrendOn: false, SigmaEWMA: 0.1710, Sigma252: 0.1455, Sigma: 0.1710, Binding: core.BindingEWMA, Action: core.ActionSellAll, Quantity: 987, HeldBefore: 987},
	}
	return rpc.StrategyReplayResult{AsOf: goldenAt, Strategy: "core", Rule: core.RuleID, SpecHash: core.SpecHash, Signal: core.TrendSMA10.ID, Instrument: in,
		From: "2025-01-01", To: "2026-09-30", SeriesFrom: "2024-02-05", SeriesTo: "2025-08-04", Calendar: "de_xetra", CalendarLabel: "Xetra",
		Source: "IBKR historical bars · TRADES", FetchedAt: goldenAt.Add(-time.Minute),
		Dates: []string{"2024-02-05", "2024-02-06", "2024-02-07", "2024-02-08", "2024-02-09", "2024-02-12", "2024-02-13", "2024-02-14", "2024-02-15", "2025-08-04"}, Closes: closes,
		OffCalendar: []string{"2025-05-01"}, Sizing: core.ReplaySizing, Terms: core.ReplayTerms{BandPct: 10, MinOrderBase: 2500},
		Note: "Paper table: fixed sizing, the paper holding carried under the entry's band and minimum from zero; nothing is frozen, recorded or sent. Compare with `lab replay-compare`.", Rows: rows}
}
