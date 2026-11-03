package cli

import (
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Synthetic results for the read-only desk commands pinned in
// TestRenderGolden: the economic calendar, the rulebook and its history, data
// health, broker reporting, the account and the order reads. Every figure,
// limit, account id and order reference is invented and non-round; none comes
// from a real book or from the compiled rulebook baseline.

func goldenMacro() rpc.MacroSnapshotResult {
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 9, day, hour, minute, 0, 0, goldenZone) }
	return rpc.MacroSnapshotResult{AsOf: goldenAt, WindowStart: "2026-09-05", WindowEnd: "2026-09-18", CoverageStatus: "partial",
		Events: []rpc.MacroEvent{
			{ID: "bls-1", Title: "Employment Situation", SourceID: "bls", Date: "2026-09-07", ScheduledAt: at(7, 14, 30), TimePrecision: "exact", Timezone: "America/New_York"},
			{ID: "ecb-1", Title: "Monetary policy decision", SourceID: "ecb", Date: "2026-09-10", ScheduledAt: at(10, 14, 15), TimePrecision: "exact", Timezone: "Europe/Frankfurt"},
			{ID: "bea-1", Title: "Personal Income and Outlays", SourceID: "bea", Date: "2026-09-11", TimeLabel: "8:30 AM", TimePrecision: "label", Timezone: "America/New_York"},
			{ID: "fed-1", Title: "FOMC meeting, day one", SourceID: "fed", Date: "2026-09-15", TimePrecision: "date", Timezone: "America/New_York"},
		},
		Publications: []rpc.MacroPublication{
			{ID: "fed-p1", Title: "Speech on the balance sheet outlook", SourceID: "fed", SourceURL: "https://example.org/synthetic/speech"},
			{ID: "nyfed-p1", Title: "Liberty Street note on reserve demand", SourceID: "nyfed", SourceURL: "https://example.org/synthetic/note"},
		},
		Sources: []rpc.MacroSource{
			{ID: "bls", Name: "BLS release calendar", Availability: "available", Coverage: "release_calendar", LastSuccess: goldenAt.Add(-47 * time.Minute)},
			{ID: "bea", Name: "BEA release schedule", Availability: "available", Stale: true, Coverage: "release_calendar", WindowStart: "2026-09-01", WindowEnd: "2026-09-12",
				ConsecutiveFailures: 3, LastSuccess: goldenAt.Add(-31 * time.Hour)},
			{ID: "ecb", Name: "ECB meeting calendar", Availability: "available", Coverage: "meeting_calendar", LastSuccess: goldenAt.Add(-2 * time.Hour)},
			{ID: "fed", Name: "Federal Reserve calendar", Availability: "unavailable", Detail: "The publisher answered with a server error.", Coverage: "meeting_calendar",
				ConsecutiveFailures: 2, LastSuccess: goldenAt.Add(-26 * time.Hour)},
		}}
}

func goldenRules() rpc.RulesResult {
	stageAt := goldenAt.Add(-23 * time.Minute)
	return rpc.RulesResult{AsOf: goldenAt, Enabled: true, Status: "ok", PolicyID: "rulebook-synthetic", PolicyVersion: 3, BaseCurrency: "EUR",
		PolicyStatus: &rpc.RulebookPolicyStatus{Status: "ok", Source: "file", PolicyID: "rulebook-synthetic", PolicyVersion: 3},
		SellOnly:     risk.RuleSellOnly{Active: true, Rules: []string{risk.RuleCashSellOnly}},
		Rules: []risk.RuleRow{
			{ID: risk.RuleSingleNameExposure, Number: 1, Title: "Worst-case loss on one issuer", Status: risk.RuleStatusAct, Unit: "% NLV",
				Observed: new(13.7), Threshold: new(11.5), WatchThreshold: new(8.5), ActThreshold: new(11.5),
				Evidence: "Worst-case loss on the largest issuer is 13.7% of net liquidation, above the 11.5% act level.",
				Offenders: []risk.RuleOffender{
					{Symbol: "SYNA", Observed: 13.7, Status: risk.RuleStatusAct, Note: "largest issuer"},
					{Symbol: "SYNB", Observed: 9.3, Status: risk.RuleStatusWatch},
				}},
			{ID: risk.RuleCashSellOnly, Number: 3, Title: "Premium budget", Status: risk.RuleStatusWatch, Unit: "% NLV",
				Observed: new(6.3), Threshold: new(5.75), WatchThreshold: new(5.75), ActThreshold: new(8.25),
				Evidence:  "Option premium held is 6.3% of net liquidation, above the 5.75% watch level.",
				Offenders: []risk.RuleOffender{{Symbol: "SYNA", Leg: "SYNA 20261218 C 47", Observed: 3.4, Note: "largest line"}}},
			{ID: risk.RuleHedgeIntegrity, Number: 12, Title: "Index protection size", Status: risk.RuleStatusWatch, Unit: "% gross long",
				Observed: new(17.9), Threshold: new(27.5), WatchThreshold: new(27.5), ActThreshold: new(93.0),
				Evidence:  "Protection short delta is 17.9% of gross long exposure, below the 27.5–46.5% range.",
				Exempt:    []risk.RuleOffender{{Symbol: "SPY", Leg: "SPY 20261218 P 605", Note: "portfolio protection"}},
				RegimeSet: risk.RegimeBucketEarlyWarning,
				Notes:     []string{"thresholds: early_warning regime set"}},
			{ID: risk.RuleExpiryRunway, Number: 5, Title: "Options nearing expiry", Status: risk.RuleStatusUnknown,
				Evidence: "Option expiries could not be read; the rule stays unknown, never pass."},
			{ID: risk.RuleRedOnGreen, Number: 9, Title: "Holding falls while the market rises", Status: risk.RuleStatusNotEvaluated,
				Reason: risk.RuleReasonRuleOff, Evidence: "Turned off in the Rulebook policy."},
			{ID: risk.RuleClusterStress, Number: 17, Title: "Cluster falling together", Status: risk.RuleStatusNotEvaluated,
				Reason: risk.RuleReasonNoClusters, Evidence: "No cluster is declared in the Rulebook policy."},
			{ID: risk.RuleOptionLinePremium, Number: 2, Title: "Premium at risk in one option position", Status: risk.RuleStatusPass,
				Evidence: "No option line holds premium above its limit."},
		},
		Ranked:       []int{0, 1, 2, 3, 4, 5, 6},
		BreachCounts: map[string]int{risk.RuleStatusAct: 1, risk.RuleStatusWatch: 2, risk.RuleStatusUnknown: 1, risk.RuleStatusPass: 1, risk.RuleStatusNotEvaluated: 2},
		InputHealth: []rpc.SourceHealth{
			{Source: "regime_stage", Status: "ok", AsOf: stageAt},
			{Source: "tape", Status: "stale", Notes: []string{"last bar 41 minutes old"}},
		},
		Earnings: []rpc.EarningsInfo{
			{Symbol: "SYNB", Source: "unknown", Status: rpc.EarningsStatusTransportFailure},
		}}
}

func goldenRulesHistory() rpc.RulesHistoryResult {
	since := time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)
	at := func(day, hour, minute int) time.Time { return time.Date(2026, 9, day, hour, minute, 0, 0, time.UTC) }
	entry := func(t time.Time, rule, was, status, evidence string) rpc.RuleTransitionEntry {
		return rpc.RuleTransitionEntry{At: t, Rule: rule, Was: was, Status: status, Evidence: evidence, PolicyID: "rulebook-synthetic", PolicyVersion: 3}
	}
	entries := []rpc.RuleTransitionEntry{
		entry(at(5, 11, 37), risk.RuleSingleNameExposure, risk.RuleStatusWatch, risk.RuleStatusAct, "Worst-case loss on the largest issuer is 13.7% of net liquidation, above the 11.5% act level."),
		entry(at(4, 15, 2), risk.RuleCashSellOnly, risk.RuleStatusPass, risk.RuleStatusWatch, "Option premium held is 6.3% of net liquidation, above the 5.75% watch level."),
		entry(at(3, 9, 48), risk.RuleSingleNameExposure, risk.RuleStatusPass, risk.RuleStatusWatch, "Worst-case loss on the largest issuer is 9.1% of net liquidation."),
		entry(at(1, 13, 26), risk.RuleHedgeIntegrity, "", risk.RuleStatusWatch, "Protection short delta is 17.9% of gross long exposure."),
	}
	return rpc.RulesHistoryResult{AsOf: goldenAt, Since: since, Until: at(5, 23, 59), Entries: entries, Count: len(entries), TotalCount: 7, Limit: 4, Truncated: true}
}

func goldenDataHealth() rpc.DataHealthResult {
	next := 3
	return rpc.DataHealthResult{SchemaVersion: 1, Revision: "rev-synthetic-0905", AsOf: goldenAt, ValidUntil: goldenAt.Add(5 * time.Minute), ScopeState: "current",
		Summary: rpc.DataHealthSummary{State: "limited", Label: "Some sources need attention", Total: 7, Required: 5, Current: 4, Limited: 2, Unavailable: 1, Problems: 2, ExpectedDelays: 1},
		Concerns: []rpc.DataHealthConcern{
			{SourceID: "option_quotes", State: "unavailable", Label: "Option quotes are not arriving"},
			{SourceID: "hy_oas", State: "limited", Label: "Credit spread awaits the publisher"},
		},
		Sources: []rpc.DataSourceHealth{
			{ID: "account", Name: "Account values", Required: true, State: "current", Receiving: "updating every few seconds"},
			{ID: "option_quotes", Name: "Option quotes", Required: true, State: "unavailable", Receiving: "nothing for 17 minutes",
				Detail: "The broker refused the market-data request.", Action: "Check the option market-data subscription in the broker portal.",
				Access: &rpc.DataAccessObservation{Code: 354, Reason: "not subscribed", ObservedAt: goldenAt.Add(-17 * time.Minute), RetryAt: goldenAt.Add(13 * time.Minute)}},
			{ID: "hy_oas", Name: "High-yield credit spread", State: "limited", Receiving: "last value from 3 Sep", Cause: rpc.DataHealthCauseUpstreamPublicationPending,
				Detail: "The publisher has not released the 4 Sep value yet.", Action: "No action; the value arrives when the publisher releases it."},
		},
		Offset: 0, NextOffset: &next}
}

func goldenReportingStatus() rpc.ReportingStatusResult {
	return rpc.ReportingStatusResult{SchemaVersion: rpc.ReportingSchemaVersion, State: rpc.ReportingStateActionRequired, Reason: rpc.ReportingReasonFlexQueryIncomplete, ManifestVersion: "synthetic-3",
		Local:    rpc.ReportingLocalStatus{Enabled: true, QueryConfigured: true, TokenFilePresent: true, TokenFilePrivate: true},
		Broker:   rpc.ReportingBrokerStatus{State: rpc.ReconReportStateCurrent, Reachability: rpc.ReportingReachabilityReachable, NextAttempt: goldenAt.Add(97 * time.Minute)},
		Evidence: rpc.ReportingEvidenceStatus{State: rpc.ReportingEvidenceObserved, SchemaFingerprint: "flex_schema_5ynth371c", CoverageTo: goldenAt.AddDate(0, 0, -1)},
		Requirements: []rpc.ReportingSectionRequirement{
			{Key: "trades", Label: "Trades", Status: "observed", LevelOfDetail: "Executions", Fields: []string{"tradeID", "dateTime"}},
			{Key: "open_positions", Label: "Open Positions", Status: "missing", LevelOfDetail: "Summary", Fields: []string{"conid", "markPrice", "costBasisMoney"}, MissingFields: []string{"markPrice", "costBasisMoney"}},
			{Key: "transfers", Label: "Transfers", Status: "absent", Fields: []string{"transactionID", "direction"}},
			{Key: "cash_transactions", Label: "Cash Transactions", Status: "empty", Fields: []string{"transactionID", "amount"}},
		},
		MissingRequirements: []string{"open_positions.markPrice", "open_positions.costBasisMoney"},
		UnprovedSections:    []string{"transfers", "cash_transactions"},
		Action:              "Add the missing fields to the Activity Flex Query, then run canary setup reporting."}
}

func goldenReportingPerformance() rpc.ReportingPerformanceResult {
	return rpc.ReportingPerformanceResult{SchemaVersion: rpc.ReportingPerformanceSchemaVersion, AsOf: goldenAt, BaseCurrency: "EUR",
		Evidence: rpc.ReportingPerformanceEvidence{State: rpc.ReportingEvidenceObserved, CoverageFrom: "2026-01-02", CoverageTo: "2026-09-04", EquityDays: 173, UnclassifiedLines: 2},
		Days:     []rpc.PerformanceDay{{Day: "2026-09-03", EquityBase: 48217.63}, {Day: "2026-09-04", EquityBase: 48391.17}},
		Flows: []rpc.PerformanceFlow{
			{Day: "2026-03-17", Type: rpc.PerformanceFlowDeposit, AmountBase: 2731.40},
			{Day: "2026-07-08", Type: rpc.PerformanceFlowWithdrawal, AmountBase: -1185.25},
		},
		YearToDate: &rpc.PerformanceYearToDate{From: "2026-01-02", Through: "2026-09-04", RealisedBase: 1873.29, Closes: 23, CommissionsBase: -61.37,
			DividendsBase: 214.83, InterestBase: 37.19, WithholdingTaxBase: -32.22, FeesBase: -11.47, OtherBase: 3.05, UnconvertedTrades: 1}}
}

func goldenAccount() rpc.AccountResult {
	daily, unrealized, realized := 217.43, 3184.27, 642.91
	rate := func(r float64) *float64 { return &r }
	return rpc.AccountResult{AccountID: "DU0000000", AccountType: "INDIVIDUAL", BaseCurrency: "EUR",
		NetLiquidation: 48391.17, BuyingPower: 61374.83, AvailableFunds: 15343.71, ExcessLiquidity: 17219.06, TotalCash: 9137.52,
		GrossPositionValue: 39253.65, UnrealizedPnL: 3184.27, RealizedPnL: -48.36, Cushion: 0.3558,
		InitialMargin: 33047.46, MaintenanceMargin: 31172.11,
		LookAheadInitMargin: 33211.84, LookAheadMaintMargin: 31309.62, LookAheadAvailable: 15179.33, LookAheadExcess: 17081.55,
		DailyPnL: &daily, PnLUnrealizedTotal: &unrealized, PnLRealizedTotal: &realized,
		CurrencyExposure: []rpc.CurrencyExposure{
			{Currency: "USD", NetLiquidationCcy: 31942.18, ExchangeRate: 0.8573, NetLiquidationBase: 27384.03,
				Interest: &rpc.CashInterestRates{Source: "flex", Credit: rpc.CashInterestSide{State: rpc.CashInterestStateAvailable, Rate: rate(0.03137), Tier: "0-10000", ValueDate: "2026-09-04"},
					Debit: rpc.CashInterestSide{State: "unavailable", Reason: "no debit tier in the statement"}}},
			{Currency: "GBP", NetLiquidationCcy: 1217.36, ExchangeRate: 1.1694, NetLiquidationBase: 1423.58},
		},
		BaseCurrencyLedger: &rpc.CurrencyExposure{Currency: "EUR", Interest: &rpc.CashInterestRates{Source: "flex",
			Credit: rpc.CashInterestSide{State: rpc.CashInterestStateAvailable, Rate: rate(0.01412), Tier: "0-10000", ValueDate: "2026-09-04"},
			Debit:  rpc.CashInterestSide{State: rpc.CashInterestStateAvailable, Rate: rate(0.05163), Tier: "0-100000", ValueDate: "2026-09-04"}}},
		AsOf: goldenAt,
		Authority: &rpc.AccountDataAuthority{Scope: rpc.AccountDataScope{AccountID: "DU0000000", AccountMode: "paper"},
			Availability: rpc.AccountDataAvailable, Freshness: rpc.AccountDataFreshnessCurrent, AsOf: goldenAt,
			Fields: &rpc.AccountFieldAvailability{AccountType: true, BaseCurrency: true, NetLiquidation: true, BuyingPower: true, AvailableFunds: true,
				ExcessLiquidity: true, TotalCash: true, MaintenanceMargin: true, InitialMargin: true, GrossPositionValue: true, UnrealizedPnL: true,
				RealizedPnL: true, Cushion: true, LookAheadInitMargin: true, LookAheadMaintMargin: true, LookAheadAvailable: true, LookAheadExcess: true,
				DailyPnL: true, PnLUnrealizedTotal: true, PnLRealizedTotal: true, CurrencyExposure: true}}}
}

// goldenWorkingOrder is a Canary limit buy resting at the broker.
func goldenWorkingOrder() rpc.OrderView {
	return rpc.OrderView{OrderRef: "canary-20260905-113702", ReservedOrderID: 417, PermID: 900000417, Account: "DU0000000",
		Symbol: "SYNA", SecType: "STK", Action: "BUY", OrderType: "LMT", TIF: "DAY", Quantity: 37, LimitPrice: 46.83,
		Status: "Submitted", LifecycleStatus: "working", Filled: 12, Remaining: 25, Origin: "canary",
		LastMessage: "Partially filled at the limit.", UpdatedAt: goldenAt.Add(-23 * time.Minute)}
}

func goldenOrdersOpen() rpc.OrdersOpenResult {
	return rpc.OrdersOpenResult{AsOf: goldenAt, Account: "DU0000000", Mode: "paper", LastLocalEventAt: goldenAt.Add(-23 * time.Minute),
		NotBrokerStatement: "local journal and broker session; not a broker statement",
		Limitations:        []string{"Orders placed in another session appear only once the broker reports them."},
		Orders:             []rpc.OrderView{goldenWorkingOrder()},
		Untracked: []rpc.OrderView{{PermID: 900000583, Symbol: "SYNB", Action: "SELL", OrderType: "STP", TIF: "GTC", Quantity: 19,
			Status: "PreSubmitted", LifecycleStatus: "working", TriggerMethod: rpc.OrderTriggerMethodLast, CancelEligible: true}},
		UntrackedStatus: rpc.OrdersUntrackedCurrent, UntrackedAsOf: goldenAt.Add(-4 * time.Minute), UntrackedControl: "on"}
}

func goldenOrderStatus() rpc.OrderStatusResult {
	order := goldenWorkingOrder()
	return rpc.OrderStatusResult{Found: true, Order: order, AsOf: goldenAt, Account: "DU0000000", Mode: "paper",
		LastLocalEventAt: order.UpdatedAt, NotBrokerStatement: "local journal and broker session; not a broker statement",
		Events: []rpc.OrderEvent{
			{At: goldenAt.Add(-142 * time.Minute), Type: "submitted", LifecycleStatus: "submitted"},
			{At: goldenAt.Add(-141 * time.Minute), Type: "status", LifecycleStatus: "working", Message: "Order accepted by the broker."},
			{At: order.UpdatedAt, Type: "fill", LifecycleStatus: "working", Message: "Partially filled at the limit."},
		}}
}
