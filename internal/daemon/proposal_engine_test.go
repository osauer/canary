package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/discover"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

func TestOptionDTECalendarDays(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("load Europe/Berlin: %v", err)
	}

	tests := []struct {
		name   string
		raw    string
		asOf   time.Time
		want   int
		wantOK bool
	}{
		{
			name:   "positive span across spring-forward compact layout",
			raw:    "20260331",
			asOf:   time.Date(2026, time.March, 25, 12, 0, 0, 0, berlin),
			want:   6,
			wantOK: true,
		},
		{
			name:   "negative span across spring-forward dashed layout",
			raw:    "2026-03-25",
			asOf:   time.Date(2026, time.March, 31, 12, 0, 0, 0, berlin),
			want:   -6,
			wantOK: true,
		},
		{
			name:   "positive span across fall-back",
			raw:    "20261027",
			asOf:   time.Date(2026, time.October, 21, 12, 0, 0, 0, berlin),
			want:   6,
			wantOK: true,
		},
		{
			name:   "same day",
			raw:    "2026-07-15",
			asOf:   time.Date(2026, time.July, 15, 23, 59, 0, 0, berlin),
			want:   0,
			wantOK: true,
		},
		{
			name:   "mid-summer span without transition",
			raw:    "20260710",
			asOf:   time.Date(2026, time.July, 1, 8, 30, 0, 0, berlin),
			want:   9,
			wantOK: true,
		},
		{
			name:   "invalid raw",
			raw:    "not-a-date",
			asOf:   time.Date(2026, time.July, 1, 8, 30, 0, 0, berlin),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := optionDTE(tt.raw, tt.asOf)
			if ok != tt.wantOK {
				t.Fatalf("optionDTE(%q, %v) ok = %v, want %v", tt.raw, tt.asOf, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("optionDTE(%q, %v) = %d, want %d", tt.raw, tt.asOf, got, tt.want)
			}
		})
	}
}

func TestEnrichProposalPositionContext(t *testing.T) {
	mv, mvBase, day, dayPct := 38636.0, 33925.0, 2828.0, 7.9
	row := rpc.PositionView{
		Symbol: "NOW", SecType: "STOCK", Quantity: 400, Currency: "USD",
		MarketValue: mv, MarketValueBase: &mvBase, DayChangeMoney: &day, DayChangePct: &dayPct,
	}
	acct := &rpc.AccountResult{NetLiquidation: 247000, BaseCurrency: "EUR"}
	var p rpc.TradeProposal
	p.Contract.Currency = "USD"
	enrichProposalPositionContext(&p, row, acct)
	if p.PositionMarketValue != mv {
		t.Fatalf("PositionMarketValue=%v, want %v", p.PositionMarketValue, mv)
	}
	if p.MarketValuePctNLV == nil || *p.MarketValuePctNLV <= 0 {
		t.Fatalf("MarketValuePctNLV=%v, want positive", p.MarketValuePctNLV)
	}
	if p.PositionDayChangeMoney == nil || *p.PositionDayChangeMoney != day {
		t.Fatalf("PositionDayChangeMoney=%v, want %v", p.PositionDayChangeMoney, day)
	}
	if p.PositionDayChangeCurrency != "USD" {
		t.Fatalf("PositionDayChangeCurrency=%q, want USD", p.PositionDayChangeCurrency)
	}
}

func TestEnrichRiskReductionContextUsesGroup(t *testing.T) {
	gmv, gmvBase, gday := 84501.67, 74200.0, 14227.98
	group := rpc.PositionGroup{
		Underlying: "NOW", GroupMarketValue: gmv,
		GroupMarketValueBase: &gmvBase, GroupDailyPnLBase: &gday,
	}
	acct := &rpc.AccountResult{NetLiquidation: 247000, BaseCurrency: "EUR"}
	var p rpc.TradeProposal
	enrichRiskReductionContext(&p, group, acct)
	if p.PositionMarketValue != gmv {
		t.Fatalf("PositionMarketValue=%v, want group %v", p.PositionMarketValue, gmv)
	}
	if p.PositionDayChangeCurrency != "EUR" {
		t.Fatalf("PositionDayChangeCurrency=%q, want base EUR", p.PositionDayChangeCurrency)
	}
	if p.PositionDayChangeMoney == nil || *p.PositionDayChangeMoney != gday {
		t.Fatalf("PositionDayChangeMoney=%v, want %v", p.PositionDayChangeMoney, gday)
	}
	if p.PositionDayChangePct == nil {
		t.Fatal("PositionDayChangePct nil, want computed from group base values")
	}
}

func thetaTestStatus() rpc.ProtectionPolicyStatus {
	policy := defaultProtectionPolicy()
	return protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
}

var thetaTestNow = time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC)

// A genuinely time-value-dominated near-expiry long is the legitimate theta
// hygiene target: it fires a close-only proposal.
func TestThetaProposalFiresOnTimeValueBleed(t *testing.T) {
	theta := -0.08
	underlying := 200.0
	row := rpc.PositionView{
		Symbol: "AAPL", SecType: "OPTION", Quantity: 2, Multiplier: 100,
		Mark: 1.25, Expiry: "20260619", Right: "C", Strike: 200,
		Theta: &theta, Underlying: &underlying, // ATM: mark is all extrinsic
	}
	prop, ok, supp := thetaProposal(defaultProtectionPolicy(), thetaTestStatus(), row, rpc.TradeProposalSourceFingerprints{}, thetaTestNow)
	if !ok || supp != nil {
		t.Fatalf("ATM bleeder should fire a proposal: ok=%v supp=%v", ok, supp)
	}
	if prop.State != rpc.TradeProposalStateGenerated {
		t.Fatalf("state=%q, want generated", prop.State)
	}
	if prop.Action != rpc.OrderActionSell || prop.PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatalf("action/effect = %s/%s, want SELL/close", prop.Action, prop.PositionEffect)
	}
	if prop.Quantity != 2 || prop.MaxQuantity != 2 {
		t.Fatalf("qty=%d max=%d, want 2/2", prop.Quantity, prop.MaxQuantity)
	}
}

// The reported live bug: a deep-ish ITM long (BB Jul-17 $10 call, ~79% of the
// premium intrinsic) must be suppressed, not recommended for a close.
func TestThetaProposalSuppressesIntrinsicDominated(t *testing.T) {
	theta := -0.0207
	underlying := 11.286
	row := rpc.PositionView{
		Symbol: "BB", SecType: "OPTION", Quantity: 300, Multiplier: 100,
		Mark: 1.62, Expiry: "20260626", Right: "C", Strike: 10,
		Theta: &theta, Underlying: &underlying,
	}
	prop, ok, supp := thetaProposal(defaultProtectionPolicy(), thetaTestStatus(), row, rpc.TradeProposalSourceFingerprints{}, thetaTestNow)
	if ok {
		t.Fatalf("intrinsic-dominated ITM long must not produce a proposal, got %+v", prop)
	}
	if supp == nil || supp.Reason != "intrinsic_dominated" {
		t.Fatalf("want intrinsic_dominated suppression, got %v", supp)
	}
}

// When intrinsic cannot be separated from time value (no underlying spot, no
// mark, or a stale row), surface a blocked row with remediation rather than
// silently dropping a previously-visible proposal.
func TestThetaProposalBlocksWhenExtrinsicUncomputable(t *testing.T) {
	theta := -0.08
	row := rpc.PositionView{
		Symbol: "AAPL", SecType: "OPTION", Quantity: 2, Multiplier: 100,
		Mark: 1.25, Expiry: "20260619", Right: "C", Strike: 200,
		Theta: &theta, // Underlying nil -> uncomputable
	}
	prop, ok, supp := thetaProposal(defaultProtectionPolicy(), thetaTestStatus(), row, rpc.TradeProposalSourceFingerprints{}, thetaTestNow)
	if !ok || supp != nil {
		t.Fatalf("uncomputable extrinsic should emit a blocked proposal: ok=%v supp=%v", ok, supp)
	}
	if prop.State != rpc.TradeProposalStateBlocked {
		t.Fatalf("state=%q, want blocked", prop.State)
	}
	if len(prop.Blockers) == 0 || prop.Blockers[0].Code != "extrinsic_uncomputable" {
		t.Fatalf("want extrinsic_uncomputable blocker, got %+v", prop.Blockers)
	}
}

// A stale mark sitting below intrinsic must not divide-by-zero into a +Inf
// ratio and the most-confident possible close.
func TestThetaProposalStaleMarkBelowIntrinsicSuppresses(t *testing.T) {
	theta := -0.05
	underlying := 11.29
	row := rpc.PositionView{
		Symbol: "BB", SecType: "OPTION", Quantity: 300, Multiplier: 100,
		Mark: 1.20, Expiry: "20260626", Right: "C", Strike: 10, // mark < intrinsic 1.29
		Theta: &theta, Underlying: &underlying,
	}
	prop, ok, supp := thetaProposal(defaultProtectionPolicy(), thetaTestStatus(), row, rpc.TradeProposalSourceFingerprints{}, thetaTestNow)
	if ok {
		t.Fatalf("mark<intrinsic must not produce a close proposal, got %+v", prop)
	}
	if supp == nil {
		t.Fatal("want a suppression record")
	}
}

// Rank must surface high-dollar time value at risk, not the fastest-bleeding
// cheap far-OTM lottery ticket.
func TestThetaProposalRanksExtrinsicDollarsOverLotto(t *testing.T) {
	atmTheta, atmUnder := -0.30, 100.0
	atm := rpc.PositionView{
		Symbol: "ATM", SecType: "OPTION", Quantity: 50, Multiplier: 100,
		Mark: 3.00, Expiry: "20260616", Right: "C", Strike: 100,
		Theta: &atmTheta, Underlying: &atmUnder,
	}
	lottoTheta, lottoUnder := -0.01, 100.0
	lotto := rpc.PositionView{
		Symbol: "LOTTO", SecType: "OPTION", Quantity: 5, Multiplier: 100,
		Mark: 0.05, Expiry: "20260616", Right: "C", Strike: 130, // far OTM
		Theta: &lottoTheta, Underlying: &lottoUnder,
	}
	policy, status := defaultProtectionPolicy(), thetaTestStatus()
	atmProp, atmOK, _ := thetaProposal(policy, status, atm, rpc.TradeProposalSourceFingerprints{}, thetaTestNow)
	lottoProp, lottoOK, _ := thetaProposal(policy, status, lotto, rpc.TradeProposalSourceFingerprints{}, thetaTestNow)
	if !atmOK || !lottoOK {
		t.Fatalf("both should fire: atm=%v lotto=%v", atmOK, lottoOK)
	}
	if atmProp.Score <= lottoProp.Score {
		t.Fatalf("ATM score %.2f must outrank lotto score %.2f", atmProp.Score, lottoProp.Score)
	}
}

// Theta is favorable for shorts; a deep-ITM short with negligible extrinsic
// must never be turned into a theta-framed buy-to-close.
func TestThetaProposalShortDeepITMNoThetaClose(t *testing.T) {
	theta := -0.05
	underlying := 120.0
	row := rpc.PositionView{
		Symbol: "XYZ", SecType: "OPTION", Quantity: -3, Multiplier: 100,
		Mark: 20.10, Expiry: "20260619", Right: "C", Strike: 100, // ITM 20, extrinsic 0.10
		Theta: &theta, Underlying: &underlying,
	}
	prop, ok, supp := thetaProposal(defaultProtectionPolicy(), thetaTestStatus(), row, rpc.TradeProposalSourceFingerprints{}, thetaTestNow)
	if ok {
		t.Fatalf("deep-ITM short must not produce a theta close, got %+v", prop)
	}
	if supp == nil {
		t.Fatal("want a suppression record for the deep-ITM short")
	}
}

// The wide-spread transaction-cost guard must work off the option's own
// bid/ask: row.SpreadPct is never populated for option legs, so the old guard
// reading it was a no-op.
func TestThetaProposalWideSpreadBlocksFromOptionQuote(t *testing.T) {
	theta := -0.08
	underlying, bid, ask := 50.0, 1.50, 2.50 // mid 2.00, spread 50% of mid
	row := rpc.PositionView{
		Symbol: "WIDE", SecType: "OPTION", Quantity: 4, Multiplier: 100,
		Mark: 2.00, Expiry: "20260619", Right: "C", Strike: 50,
		Theta: &theta, Underlying: &underlying, OptionBid: &bid, OptionAsk: &ask,
		// SpreadPct intentionally nil — proves the guard no longer relies on it.
	}
	prop, ok, supp := thetaProposal(defaultProtectionPolicy(), thetaTestStatus(), row, rpc.TradeProposalSourceFingerprints{}, thetaTestNow)
	if !ok || supp != nil {
		t.Fatalf("wide-spread case still emits a (blocked) proposal: ok=%v supp=%v", ok, supp)
	}
	if prop.State != rpc.TradeProposalStateBlocked {
		t.Fatalf("state=%q, want blocked", prop.State)
	}
	if len(prop.Blockers) == 0 || prop.Blockers[0].Code != "wide_spread" {
		t.Fatalf("want wide_spread blocker, got %+v", prop.Blockers)
	}
}

func TestRiskReductionEmitsReduceOnly(t *testing.T) {
	pct := 40.0
	mv := 40000.0
	group := rpc.PositionGroup{
		Underlying:             "MSFT",
		GroupMarketValueBase:   &mv,
		GroupMarketValuePctNLV: &pct,
		GroupMarketValue:       40000,
		Stock:                  &rpc.PositionView{Symbol: "MSFT", SecType: "STOCK", Quantity: 100, Mark: 400, Multiplier: 1, Currency: "USD"},
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	prop, ok := riskReductionProposal(policy, status, group, rpc.TradeProposalSourceFingerprints{}, time.Now())
	if !ok {
		t.Fatal("risk proposal missing")
	}
	if prop.PositionEffect != rpc.OrderPositionEffectReduce && prop.PositionEffect != rpc.OrderPositionEffectClose {
		t.Fatalf("position effect=%q, want reduce/close", prop.PositionEffect)
	}
	if prop.Action != rpc.OrderActionSell {
		t.Fatalf("action=%q, want SELL", prop.Action)
	}
	if prop.Quantity <= 0 || prop.Quantity > prop.MaxQuantity {
		t.Fatalf("quantity=%d max=%d", prop.Quantity, prop.MaxQuantity)
	}
	if prop.RiskExcessNotional != 15000 {
		t.Fatalf("risk excess notional=%v, want 15000", prop.RiskExcessNotional)
	}
	if prop.RiskExcessCurrency != "USD" {
		t.Fatalf("risk excess currency=%q, want USD", prop.RiskExcessCurrency)
	}
	counts := proposalCounts([]rpc.TradeProposal{prop}, "")
	if counts.RiskReductionExcessNotional != prop.RiskExcessNotional {
		t.Fatalf("risk excess aggregate=%v, want %v", counts.RiskReductionExcessNotional, prop.RiskExcessNotional)
	}
	if counts.RiskReductionExcessCurrency != "USD" {
		t.Fatalf("risk excess aggregate currency=%q, want USD", counts.RiskReductionExcessCurrency)
	}
}

func TestRiskReductionSkipsUnsupportedSecurityTypes(t *testing.T) {
	pct := 40.0
	mv := 40000.0
	group := rpc.PositionGroup{
		Underlying:             "ES",
		GroupMarketValueBase:   &mv,
		GroupMarketValuePctNLV: &pct,
		GroupMarketValue:       40000,
		Stock:                  &rpc.PositionView{Symbol: "ES", SecType: "FUT", Quantity: 1, Mark: 5000, Multiplier: 50, Currency: "USD"},
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	if prop, ok := riskReductionProposal(policy, status, group, rpc.TradeProposalSourceFingerprints{}, time.Now()); ok {
		t.Fatalf("unsupported security emitted proposal: %+v", prop)
	}
}

func TestTrailingStopStockProposalUsesBidAskAndBlocksWideSpread(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	bid, ask, spreadPct := 100.0, 101.0, 3.0
	longRow := rpc.PositionView{
		Symbol:     "MSFT",
		SecType:    "STK",
		Quantity:   10,
		Bid:        &bid,
		Ask:        &ask,
		Mark:       106,
		SpreadPct:  &spreadPct,
		Multiplier: 1,
		Currency:   "USD",
	}
	prop, ok := trailingStopStockProposal(policy, status, longRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("stock trail proposal missing")
	}
	if prop.OrderType != rpc.OrderTypeTRAIL || prop.Trail == nil || prop.Trail.TrailingAmount == nil || *prop.Trail.TrailingAmount != 10 {
		t.Fatalf("trail = %+v orderType=%q, want bid-derived 10.00 TRAIL amount", prop.Trail, prop.OrderType)
	}
	if prop.Trail.TrailingPercent != nil || prop.Trail.OffsetType != rpc.OrderTrailOffsetAmount {
		t.Fatalf("trail = %+v, want amount offset without broker percent", prop.Trail)
	}
	if prop.Trail.InitialStopPrice != 90 {
		t.Fatalf("initial stop = %.2f, want bid-based 90.00", prop.Trail.InitialStopPrice)
	}
	if prop.TriggerMethod != rpc.OrderTriggerMethodLast {
		t.Fatalf("trigger method = %d, want LAST", prop.TriggerMethod)
	}
	if prop.ExecutionSemantics == nil {
		t.Fatalf("execution semantics missing: %+v", prop)
	}
	if prop.ExecutionSemantics.ReferenceSide != "bid" || prop.ExecutionSemantics.TriggerMethod != rpc.OrderTriggerMethodLast || prop.ExecutionSemantics.TriggerMethodLabel != "last" {
		t.Fatalf("execution semantics = %+v, want bid/LAST disclosure", prop.ExecutionSemantics)
	}
	if prop.ExecutionSemantics.TriggerEffect != "market_order_when_triggered" || prop.ExecutionSemantics.PriceGuarantee != "stop_price_is_not_execution_price" {
		t.Fatalf("execution semantics = %+v, want TRAIL market-conversion warning", prop.ExecutionSemantics)
	}
	if prop.StopRisk == nil || prop.StopRisk.DistancePct == nil || *prop.StopRisk.DistancePct != 10 {
		t.Fatalf("stop risk = %+v, want 10%% stop distance", prop.StopRisk)
	}
	if prop.StopRisk.EstimatedLoss == nil || *prop.StopRisk.EstimatedLoss != 100 {
		t.Fatalf("stop estimated loss = %+v, want 100 USD", prop.StopRisk.EstimatedLoss)
	}
	if prop.StopRisk.GapScenario == nil || prop.StopRisk.GapScenario.AssumedExecutionPrice == nil || *prop.StopRisk.GapScenario.AssumedExecutionPrice != 85.5 {
		t.Fatalf("gap scenario = %+v, want execution 5%% beyond stop at 85.50", prop.StopRisk.GapScenario)
	}
	if prop.StopRisk.GapScenario.EstimatedLoss == nil || *prop.StopRisk.GapScenario.EstimatedLoss != 145 {
		t.Fatalf("gap loss = %+v, want 145 USD", prop.StopRisk.GapScenario.EstimatedLoss)
	}
	if !hasStopLadderStep(prop.StopLadder, "fixed_5pct", 5) || !hasStopLadderStep(prop.StopLadder, "fixed_10pct", 10) || !hasStopLadderStep(prop.StopLadder, "policy_chosen", 10) {
		t.Fatalf("stop ladder = %+v, want fixed 5%%/10%% and policy chosen steps", prop.StopLadder)
	}
	if prop.LimitPrice != nil {
		t.Fatalf("trail proposal limit price = %v, want nil", *prop.LimitPrice)
	}
	if prop.TrailSizing == nil || !prop.TrailSizing.Fallback || prop.TrailSizing.ChosenPct != 10 || prop.TrailSizing.DataQuality != "fallback" {
		t.Fatalf("trail sizing = %+v, want 10%% fallback sizing", prop.TrailSizing)
	}
	if got := strings.Join(prop.Details, "\n"); !strings.Contains(got, "fixed 10.00 USD broker trail") ||
		!strings.Contains(got, "trail_sizing=fallback 10.0%") {
		t.Fatalf("details = %q, want fixed trail disclosure", got)
	}
	if !hasBlocker(prop.Blockers, "wide_spread") {
		t.Fatalf("blockers = %+v, want wide_spread", prop.Blockers)
	}

	shortRow := longRow
	shortRow.Quantity = -5
	shortRow.SpreadPct = nil
	prop, ok = trailingStopStockProposal(policy, status, shortRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("short stock trail proposal missing")
	}
	if prop.Action != rpc.OrderActionBuy || prop.Trail.InitialStopPrice != 111.1 {
		t.Fatalf("short stock action/stop = %s/%.2f, want BUY ask-based 111.10", prop.Action, prop.Trail.InitialStopPrice)
	}
	if prop.ExecutionSemantics == nil || prop.ExecutionSemantics.ReferenceSide != "ask" {
		t.Fatalf("short execution semantics = %+v, want ask reference", prop.ExecutionSemantics)
	}

	offHoursRow := longRow
	offHoursRow.Bid = nil
	offHoursRow.Ask = nil
	offHoursRow.SpreadPct = nil
	prop, ok = trailingStopStockProposal(policy, status, offHoursRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("off-hours stock trail proposal missing")
	}
	if hasBlocker(prop.Blockers, "missing_reference_price") {
		t.Fatalf("blockers = %+v, did not want missing_reference_price for percent broker trail", prop.Blockers)
	}
	if prop.Trail == nil || prop.Trail.TrailingAmount == nil || *prop.Trail.TrailingAmount != 10.6 || prop.Trail.InitialStopPrice != 95.4 {
		t.Fatalf("off-hours trail = %+v, want amount trail seeded from portfolio mark", prop.Trail)
	}

	zombieRow := rpc.PositionView{
		Symbol:      "ZVZZT",
		SecType:     "STOCK",
		Quantity:    20000,
		Mark:        0,
		MarketValue: 0,
		Currency:    "USD",
	}
	if prop, ok := trailingStopStockProposal(policy, status, zombieRow, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0); ok {
		t.Fatalf("mark-zero zombie emitted proposal: %+v", prop)
	}
}

func TestProtectiveStopRiskBaseCurrencyAndNLV(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	bid, ask, fx := 100.0, 101.0, 0.9
	row := rpc.PositionView{
		Symbol:     "MSFT",
		SecType:    "STK",
		Quantity:   10,
		Bid:        &bid,
		Ask:        &ask,
		Mark:       100,
		Multiplier: 1,
		Currency:   "USD",
		FXRate:     &fx,
	}
	prop, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("stock trail proposal missing")
	}
	enrichProtectiveStopProposal(&prop, row, &rpc.AccountResult{BaseCurrency: "EUR", NetLiquidation: 1000})
	if prop.StopRisk == nil || prop.StopRisk.EstimatedLossBase == nil || *prop.StopRisk.EstimatedLossBase != 90 {
		t.Fatalf("stop risk = %+v, want EUR 90 base loss", prop.StopRisk)
	}
	if prop.StopRisk.EstimatedLossPctNLV == nil || *prop.StopRisk.EstimatedLossPctNLV != 9 {
		t.Fatalf("stop risk NLV = %+v, want 9%%", prop.StopRisk)
	}
	if prop.StopRisk.BaseCurrency != "EUR" || prop.StopRisk.Currency != "USD" {
		t.Fatalf("stop risk currencies = %q/%q, want USD/EUR", prop.StopRisk.Currency, prop.StopRisk.BaseCurrency)
	}
}

func hasStopLadderStep(steps []rpc.TradeProposalStopLadderStep, kind string, pct float64) bool {
	for _, step := range steps {
		if step.Kind == kind && step.Percent != nil && *step.Percent == pct {
			return true
		}
	}
	return false
}

func TestProposalCountsSerializesZeroTheta(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(proposalCounts(nil, ""))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"theta_per_day":0`)) {
		t.Fatalf("counts JSON = %s, want explicit zero theta_per_day", raw)
	}
}

func TestTrailingStopStockProposalRoutesXetraPositionForPreview(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	bid, ask := 156.0, 156.04
	row := rpc.PositionView{
		Symbol:       "SAP",
		SecType:      "STOCK",
		ConID:        14204,
		Exchange:     "IBIS",
		Currency:     "EUR",
		LocalSymbol:  "SAP",
		TradingClass: "XETRA",
		Quantity:     1,
		Multiplier:   1,
		Mark:         156.02,
		Bid:          &bid,
		Ask:          &ask,
	}
	prop, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("stock trail proposal missing")
	}
	if prop.Contract.Market != "de" || prop.Contract.Exchange != "SMART" || prop.Contract.PrimaryExch != "IBIS" {
		t.Fatalf("proposal contract route = market %q exchange %q primary %q, want de/SMART/IBIS", prop.Contract.Market, prop.Contract.Exchange, prop.Contract.PrimaryExch)
	}
	if prop.Trail == nil || prop.Trail.TrailingAmount == nil || *prop.Trail.TrailingAmount != 15.60 {
		t.Fatalf("trailing amount = %+v, want 15.60", prop.Trail)
	}
	if prop.Trail.TrailingPercent != nil {
		t.Fatalf("trailing percent = %+v, want no broker percent", prop.Trail)
	}
	if prop.Trail.InitialStopPrice != 140.40 {
		t.Fatalf("initial stop = %.4f, want cent-rounded 140.40", prop.Trail.InitialStopPrice)
	}
}

func TestBuildStockTrailSizingUsesTenPctFallbackWithoutDynamicData(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	row := rpc.PositionView{Symbol: "AMD", SecType: "STK", Quantity: 10, Mark: 100, Currency: "USD"}
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	sizing := buildStockTrailSizing(policy.Buckets.TrailingStop.StockETF, row, 100, "mark", now, stockTrailVolatility{}, now)
	if sizing == nil || !sizing.Fallback || sizing.ChosenPct != 10 || sizing.PolicyFallbackPct != 10 || sizing.DataQuality != "fallback" {
		t.Fatalf("sizing = %+v, want explicit 10%% fallback", sizing)
	}
	if !slices.Contains(sizing.MissingReasons, "atr_14_unavailable") {
		t.Fatalf("missing reasons = %+v, want atr_14_unavailable", sizing.MissingReasons)
	}
}

func TestBuildStockTrailSizingUsesATRAndCapsToPolicyMax(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	atr, atrPct := 4.2, 14.0
	row := rpc.PositionView{Symbol: "AMD", SecType: "STK", Quantity: 10, Mark: 100, Currency: "USD"}
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	sizing := buildStockTrailSizing(policy.Buckets.TrailingStop.StockETF, row, 100, "mark", now, stockTrailVolatility{
		ATR14:  &atr,
		ATRPct: &atrPct,
		AsOf:   now.Add(-time.Hour),
	}, now)
	if sizing == nil || sizing.Fallback || !sizing.Capped || sizing.SelectedBy != "atr" || sizing.ChosenPct != policy.Buckets.TrailingStop.StockETF.MaxPct {
		t.Fatalf("sizing = %+v, want ATR sizing capped to policy max", sizing)
	}
	if sizing.ATRCandidatePct == nil || *sizing.ATRCandidatePct != atrPct*stockTrailATRMultiplier {
		t.Fatalf("ATR candidate = %+v, want %.2f", sizing.ATRCandidatePct, atrPct*stockTrailATRMultiplier)
	}
}

func TestProposalEnginePreservesSnapshotOnTransientRefreshFailure(t *testing.T) {
	t.Parallel()
	oldAt := time.Date(2026, 6, 9, 14, 0, 0, 0, time.UTC)
	now := oldAt.Add(10 * time.Minute)
	policyFP := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	prop := rpc.TradeProposal{
		Key:               "trailing_stop:abc",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFP,
		CreatedAt:         oldAt,
	}
	srv := &Server{now: func() time.Time { return now }}
	engine := &proposalEngine{
		server: srv,
		store:  testProposalStore(t),
		now:    func() time.Time { return now },
		scope: func() brokerStateScope {
			return brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
		},
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              oldAt,
			Revision:          "sha256:rev",
			AccountID:         "DU1234567",
			AccountMode:       rpc.AccountModePaper,
			PolicyID:          "protection-mvp",
			PolicyVersion:     1,
			PolicyFingerprint: policyFP,
			PolicyStatus: rpc.ProtectionPolicyStatus{
				Status:      rpc.ProtectionPolicyStatusDefault,
				PolicyID:    "protection-mvp",
				Fingerprint: policyFP,
			},
			Proposals: []rpc.TradeProposal{prop},
			Counts:    proposalCounts([]rpc.TradeProposal{prop}, ""),
		},
		ignored: map[string]struct{}{},
	}

	snap, ok := engine.preserveSnapshotOnRefreshFailure(
		brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper},
		rpc.AutoTradeStatus{Trading: rpc.TradingStatus{Mode: "paper", CanPreview: true}},
		rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault, PolicyID: "protection-mvp", PolicyVersion: 1, Fingerprint: policyFP},
		[]rpc.TradingBlocker{{Code: "account_unavailable", Message: "IBKR connection unavailable"}},
		false,
	)
	if !ok {
		t.Fatal("preserveSnapshotOnRefreshFailure ok=false, want preserved snapshot")
	}
	if !snap.AsOf.Equal(oldAt) {
		t.Fatalf("preserved AsOf = %s, want last healthy %s", snap.AsOf, oldAt)
	}
	if len(snap.Proposals) != 1 || snap.Proposals[0].Key != prop.Key {
		t.Fatalf("preserved proposals = %+v, want prior proposal", snap.Proposals)
	}
	if !hasBlocker(snap.Blockers, "account_unavailable") {
		t.Fatalf("preserved blockers = %+v, want account_unavailable", snap.Blockers)
	}
	current := engine.Snapshot(false)
	if len(current.Proposals) != 1 || !hasBlocker(current.Blockers, "account_unavailable") {
		t.Fatalf("installed snapshot = %+v, want proposal plus transient blocker", current)
	}
}

func TestTrailingStopOptionProposalRequiresOptInAndBlocksUnsafeShapes(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	if policy.Buckets.TrailingStop.Options.Enabled {
		t.Fatal("option trailing stop default enabled, want disabled")
	}
	policy.Buckets.TrailingStop.Options.Enabled = true
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", time.Now())
	bid, ask, spreadPct := 2.00, 2.10, 4.9
	open := &rpc.MarketSession{Market: "us-options", IsOpen: true}
	row := rpc.PositionView{
		Symbol:         "SPY",
		SecType:        "OPT",
		Quantity:       1,
		Multiplier:     100,
		Currency:       "USD",
		Expiry:         "20260619",
		Right:          "C",
		Strike:         520,
		OptionBid:      &bid,
		OptionAsk:      &ask,
		SpreadPct:      &spreadPct,
		SessionContext: open,
	}
	prop, ok := trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), false, 0)
	if !ok {
		t.Fatal("option trail proposal missing")
	}
	if prop.State == rpc.TradeProposalStateBlocked {
		t.Fatalf("long option proposal blocked: %+v", prop.Blockers)
	}
	if prop.OrderType != rpc.OrderTypeTRAILLIMIT || prop.Trail == nil || prop.Trail.LimitOffset == nil || *prop.Trail.LimitOffset != 0.05 || prop.Trail.TrailingAmount == nil || *prop.Trail.TrailingAmount != 0.6 {
		t.Fatalf("option trail = %+v orderType=%q, want 0.60 TRAIL LIMIT amount offset 0.05", prop.Trail, prop.OrderType)
	}
	if prop.Trail.TrailingPercent != nil {
		t.Fatalf("option trail = %+v, want amount offset without broker percent", prop.Trail)
	}
	if math.Abs(prop.Trail.InitialStopPrice-1.40) > 0.0001 {
		t.Fatalf("long option stop = %.4f, want bid-premium 1.4000", prop.Trail.InitialStopPrice)
	}
	if prop.ExecutionSemantics == nil || prop.ExecutionSemantics.ReferenceSide != "bid" {
		t.Fatalf("option execution semantics = %+v, want bid reference", prop.ExecutionSemantics)
	}
	if prop.ExecutionSemantics.TriggerEffect != "limit_order_when_triggered" || prop.ExecutionSemantics.PriceGuarantee != "stop_limit_can_leave_position_unfilled" {
		t.Fatalf("option execution semantics = %+v, want TRAIL LIMIT non-fill warning", prop.ExecutionSemantics)
	}
	if prop.StopRisk == nil || prop.StopRisk.EstimatedLoss == nil || math.Abs(*prop.StopRisk.EstimatedLoss-60) > 0.0001 {
		t.Fatalf("option stop risk = %+v, want 60 USD estimated loss", prop.StopRisk)
	}

	row.Quantity = -1
	prop, ok = trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), false, 0)
	if !ok {
		t.Fatal("short option trail proposal missing")
	}
	if !hasBlocker(prop.Blockers, "short_option_trail_disabled") {
		t.Fatalf("short-option blockers = %+v, want short_option_trail_disabled", prop.Blockers)
	}
	if math.Abs(prop.Trail.InitialStopPrice-2.73) > 0.0001 {
		t.Fatalf("short option stop = %.4f, want ask-premium 2.7300", prop.Trail.InitialStopPrice)
	}

	row.Quantity = 1
	row.SessionContext = nil
	prop, ok = trailingStopOptionProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, time.Now(), true, 0)
	if !ok {
		t.Fatal("nil-session option trail proposal missing")
	}
	if !hasBlocker(prop.Blockers, "option_rth_closed") || !hasBlocker(prop.Blockers, "multi_leg_option_trail_unsupported") {
		t.Fatalf("blockers = %+v, want RTH and multi-leg blockers", prop.Blockers)
	}
}

func TestProposalPreviewParamsCarryOrderSource(t *testing.T) {
	prop := rpc.TradeProposal{
		Action:   rpc.OrderActionSell,
		Contract: rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
	}
	params := proposalOrderPreviewParams(prop, 3, 5000)
	if params.Source != proposalOrderSource {
		t.Fatalf("proposal preview source=%q, want %q", params.Source, proposalOrderSource)
	}
}

func TestProposalPreviewSafetyBlocksOpenEffect(t *testing.T) {
	prop := rpc.TradeProposal{
		Action:         rpc.OrderActionSell,
		MaxQuantity:    1,
		PositionEffect: rpc.OrderPositionEffectClose,
		SecType:        "STK",
	}
	preview := &rpc.OrderPreviewResult{
		Mode: "paper",
		Draft: rpc.OrderDraft{
			Action:    rpc.OrderActionSell,
			Contract:  rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:  1,
			OrderType: rpc.OrderTypeLMT,
			TIF:       rpc.OrderTIFDay,
			Source:    proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectOpen},
	}
	blockers := proposalPreviewSafetyBlockers(prop, preview)
	if !hasBlocker(blockers, "preview_effect_not_close_reduce") {
		t.Fatalf("blockers = %+v, want preview_effect_not_close_reduce", blockers)
	}
}

func TestProposalPreviewSafetyDoesNotOwnExecutionRoute(t *testing.T) {
	prop := rpc.TradeProposal{
		Action:         rpc.OrderActionBuy,
		MaxQuantity:    1,
		PositionEffect: rpc.OrderPositionEffectClose,
		SecType:        "OPT",
	}
	preview := &rpc.OrderPreviewResult{
		Mode: "live",
		Draft: rpc.OrderDraft{
			Action:     rpc.OrderActionBuy,
			Contract:   rpc.ContractParams{Symbol: "SPY", SecType: "OPT", Exchange: "SMART", Currency: "USD", Expiry: "20260619", Right: "C", Strike: 520, Multiplier: 100},
			Quantity:   1,
			OrderType:  rpc.OrderTypeLMT,
			TIF:        rpc.OrderTIFDay,
			OutsideRTH: false,
			Source:     proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectClose},
	}
	blockers := proposalPreviewSafetyBlockers(prop, preview)
	if hasBlocker(blockers, "proposal_not_paper") {
		t.Fatalf("blockers = %+v, proposal safety should not own paper/live routing", blockers)
	}
	if len(blockers) != 0 {
		t.Fatalf("blockers = %+v, want route-neutral proposal safety", blockers)
	}
}

func TestProposalPreviewSafetyBlocksTrailDrift(t *testing.T) {
	t.Parallel()
	propPct, previewPct, limitOffset := 8.0, 5.0, 0.05
	prop := rpc.TradeProposal{
		Action:         rpc.OrderActionSell,
		MaxQuantity:    1,
		PositionEffect: rpc.OrderPositionEffectClose,
		SecType:        "STK",
		OrderType:      rpc.OrderTypeTRAIL,
		Trail:          &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &propPct, InitialStopPrice: 92},
	}
	preview := &rpc.OrderPreviewResult{
		Mode: "paper",
		Draft: rpc.OrderDraft{
			Action:    rpc.OrderActionSell,
			Contract:  rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:  1,
			OrderType: rpc.OrderTypeTRAIL,
			TIF:       rpc.OrderTIFDay,
			Trail:     &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &previewPct, LimitOffset: &limitOffset, InitialStopPrice: 95},
			Source:    proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectClose},
	}
	blockers := proposalPreviewSafetyBlockers(prop, preview)
	if !hasBlocker(blockers, "trail_percent_drift") || !hasBlocker(blockers, "trail_limit_offset_drift") || !hasBlocker(blockers, "trail_initial_stop_drift") {
		t.Fatalf("blockers = %+v, want trail percent, limit offset, and initial stop drift", blockers)
	}
}

func TestProposalOrderPreviewParamsPreserveTrailStopPrice(t *testing.T) {
	t.Parallel()
	amount := 3.84
	prop := rpc.TradeProposal{
		Action:    rpc.OrderActionSell,
		Quantity:  1,
		OrderType: rpc.OrderTypeTRAIL,
		Contract:  rpc.ContractParams{Symbol: "MBG", SecType: "STK", Exchange: "SMART", Currency: "EUR"},
		Trail:     &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetAmount, TrailingAmount: &amount, InitialStopPrice: 44.04},
	}
	params := proposalOrderPreviewParams(prop, 1, 5000)
	if params.Trail == nil || params.Trail.InitialStopPrice != 44.04 {
		t.Fatalf("preview params trail = %+v, want initial stop preserved", params.Trail)
	}
	if params.Trail.TrailingAmount == nil || *params.Trail.TrailingAmount != amount {
		t.Fatalf("preview params trail = %+v, want trailing amount preserved", params.Trail)
	}
	if params.TriggerMethod != rpc.OrderTriggerMethodLast {
		t.Fatalf("preview params trigger method = %d, want stock trail LAST", params.TriggerMethod)
	}
}

func TestProposalPreviewSafetyBlocksTriggerMethodDrift(t *testing.T) {
	t.Parallel()
	amount := 8.0
	prop := rpc.TradeProposal{
		Action:         rpc.OrderActionSell,
		MaxQuantity:    1,
		PositionEffect: rpc.OrderPositionEffectClose,
		SecType:        "STK",
		OrderType:      rpc.OrderTypeTRAIL,
		Contract:       rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
		Trail:          &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetAmount, TrailingAmount: &amount, InitialStopPrice: 92},
		TriggerMethod:  rpc.OrderTriggerMethodLast,
	}
	preview := &rpc.OrderPreviewResult{
		Draft: rpc.OrderDraft{
			Action:        rpc.OrderActionSell,
			Contract:      prop.Contract,
			Quantity:      1,
			OrderType:     rpc.OrderTypeTRAIL,
			TIF:           rpc.OrderTIFDay,
			Trail:         cloneTrailSpec(prop.Trail),
			TriggerMethod: rpc.OrderTriggerMethodBidAsk,
			Source:        proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectClose},
	}
	if blockers := proposalPreviewSafetyBlockers(prop, preview); !hasBlocker(blockers, "trigger_method_drift") {
		t.Fatalf("blockers = %+v, want trigger_method_drift", blockers)
	}
}

func TestProposalOrderPreviewParamsCarriesProposalTIF(t *testing.T) {
	t.Parallel()
	prop := rpc.TradeProposal{Action: rpc.OrderActionSell, Quantity: 1, OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC}
	if params := proposalOrderPreviewParams(prop, 1, 5000); params.TIF != rpc.OrderTIFGTC {
		t.Fatalf("params TIF = %q, want GTC", params.TIF)
	}
	// Proposals persisted before the TIF field existed mean DAY.
	prop.TIF = ""
	if params := proposalOrderPreviewParams(prop, 1, 5000); params.TIF != rpc.OrderTIFDay {
		t.Fatalf("legacy params TIF = %q, want DAY", params.TIF)
	}
}

func TestProposalPreviewSafetyBlocksTIFDrift(t *testing.T) {
	t.Parallel()
	pct := 8.0
	mkTrail := func() *rpc.OrderTrailSpec {
		return &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &pct}
	}
	prop := rpc.TradeProposal{
		Action: rpc.OrderActionSell, MaxQuantity: 1, PositionEffect: rpc.OrderPositionEffectClose,
		SecType: "STK", OrderType: rpc.OrderTypeTRAIL, TIF: rpc.OrderTIFGTC, Trail: mkTrail(),
	}
	preview := &rpc.OrderPreviewResult{
		Mode: "paper",
		Draft: rpc.OrderDraft{
			Action:    rpc.OrderActionSell,
			Contract:  rpc.ContractParams{Symbol: "MSFT", SecType: "STK", Exchange: "SMART", Currency: "USD"},
			Quantity:  1,
			OrderType: rpc.OrderTypeTRAIL,
			TIF:       rpc.OrderTIFDay,
			Trail:     mkTrail(),
			Source:    proposalOrderSource,
		},
		Position: rpc.OrderPositionImpact{Effect: rpc.OrderPositionEffectClose},
	}
	if blockers := proposalPreviewSafetyBlockers(prop, preview); !hasBlocker(blockers, "tif_drift") {
		t.Fatalf("blockers = %+v, want tif_drift", blockers)
	}
	preview.Draft.TIF = rpc.OrderTIFGTC
	if blockers := proposalPreviewSafetyBlockers(prop, preview); len(blockers) != 0 {
		t.Fatalf("matched GTC blockers = %+v, want none", blockers)
	}
	preview.Draft.TIF = "IOC"
	if blockers := proposalPreviewSafetyBlockers(prop, preview); !hasBlocker(blockers, "unsupported_tif") {
		t.Fatalf("blockers = %+v, want unsupported_tif", blockers)
	}
}

func TestTrailingStopProposalTIFFromPolicy(t *testing.T) {
	t.Parallel()
	policy := defaultProtectionPolicy()
	now := time.Date(2026, 6, 10, 9, 0, 0, 0, time.UTC)
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusActive, "test", "", now)
	bid, ask := 99.0, 100.0
	row := rpc.PositionView{Symbol: "MBG", SecType: "STK", ConID: 29622935, Exchange: "IBIS", Currency: "EUR", Quantity: 10, Mark: 99.5, MarketValue: 995, Multiplier: 1, Bid: &bid, Ask: &ask}

	day, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, now, true, 0.01)
	if !ok {
		t.Fatal("expected stock trailing proposal")
	}
	if day.TIF != rpc.OrderTIFDay {
		t.Fatalf("default policy proposal TIF = %q, want DAY", day.TIF)
	}
	if !hasDetailContaining(day.Details, "tif=DAY") || !hasDetailContaining(day.Details, "overnight gaps") {
		t.Fatalf("details = %+v, want DAY session-close caveat", day.Details)
	}

	policy.Buckets.TrailingStop.TIF = rpc.OrderTIFGTC
	gtc, ok := trailingStopStockProposal(policy, status, row, rpc.TradeProposalSourceFingerprints{}, now, true, 0.01)
	if !ok {
		t.Fatal("expected GTC stock trailing proposal")
	}
	if gtc.TIF != rpc.OrderTIFGTC {
		t.Fatalf("GTC policy proposal TIF = %q, want GTC", gtc.TIF)
	}
	if !hasDetailContaining(gtc.Details, "tif=GTC") || hasDetailContaining(gtc.Details, "overnight gaps") {
		t.Fatalf("details = %+v, want GTC persistence note without the DAY caveat", gtc.Details)
	}
}

func hasDetailContaining(details []string, substr string) bool {
	for _, d := range details {
		if strings.Contains(d, substr) {
			return true
		}
	}
	return false
}

func TestTrailingStopFastPathPreviewUsesCurrentSnapshot(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000, AllowStockShort: true})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	policyFingerprint := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	trailPercent := 8.0
	prop := rpc.TradeProposal{
		Key:               "trailing_stop:sap",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionQuantity:  1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		Trail:             &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &trailPercent, InitialStopPrice: 92},
		TIF:               rpc.OrderTIFDay,
		Contract:          rpc.ContractParams{Symbol: "SAP", SecType: "STK", Exchange: "SMART", Currency: "EUR", Multiplier: 1},
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFingerprint,
		CreatedAt:         now,
	}
	srv.tradeProposals = &proposalEngine{
		server:  srv,
		store:   testProposalStore(t),
		now:     func() time.Time { return now },
		ignored: map[string]struct{}{},
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              now,
			Revision:          "sha256:rev",
			AccountID:         "DU1234567",
			AccountMode:       rpc.AccountModePaper,
			PolicyID:          "protection-mvp",
			PolicyVersion:     1,
			PolicyFingerprint: policyFingerprint,
			PolicyStatus: rpc.ProtectionPolicyStatus{
				Status:      rpc.ProtectionPolicyStatusDefault,
				PolicyID:    "protection-mvp",
				Fingerprint: policyFingerprint,
			},
			AutoTrade: rpc.AutoTradeStatus{Trading: srv.tradingStatus(srv.endpoint), ProposalsEnabled: true, FastPathEnabled: true},
			Trading:   srv.tradingStatus(srv.endpoint),
			Proposals: []rpc.TradeProposal{prop},
		},
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{
		Key:       prop.Key,
		Revision:  prop.Revision,
		Quantity:  1,
		TimeoutMs: 20,
		FastPath:  true,
	})
	if err != nil {
		t.Fatalf("fast preview err = %v", err)
	}
	if !res.Accepted || !res.SubmitEligible || res.Preview == nil {
		t.Fatalf("fast preview = %+v, want accepted submit-eligible preview", res)
	}
	if res.Proposal.Key != prop.Key || res.Preview.Draft.OrderType != rpc.OrderTypeTRAIL {
		t.Fatalf("preview proposal/draft = %s/%s, want %s/TRAIL", res.Proposal.Key, res.Preview.Draft.OrderType, prop.Key)
	}
}

// A GTC trailing-stop proposal must clear the whole preview chain — params,
// daemon preview validator, WhatIf, and the proposal-vs-preview drift gate —
// with zero blockers; each unit gate passing individually does not prove a
// missed DAY assumption isn't hiding between them.
func TestTrailingStopFastPathPreviewGTCEndToEnd(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000, AllowStockShort: true})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	now := time.Date(2026, 6, 10, 13, 0, 0, 0, time.UTC)
	policyFingerprint := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	trailPercent := 8.0
	prop := rpc.TradeProposal{
		Key:               "trailing_stop:sap",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionQuantity:  1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		Trail:             &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &trailPercent, InitialStopPrice: 92},
		TIF:               rpc.OrderTIFGTC,
		Contract:          rpc.ContractParams{Symbol: "SAP", SecType: "STK", Exchange: "SMART", Currency: "EUR", Multiplier: 1},
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFingerprint,
		CreatedAt:         now,
	}
	srv.tradeProposals = &proposalEngine{
		server:  srv,
		store:   testProposalStore(t),
		now:     func() time.Time { return now },
		ignored: map[string]struct{}{},
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              now,
			Revision:          "sha256:rev",
			AccountID:         "DU1234567",
			AccountMode:       rpc.AccountModePaper,
			PolicyID:          "protection-mvp",
			PolicyVersion:     1,
			PolicyFingerprint: policyFingerprint,
			PolicyStatus:      rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault, PolicyID: "protection-mvp", Fingerprint: policyFingerprint},
			AutoTrade:         rpc.AutoTradeStatus{Trading: srv.tradingStatus(srv.endpoint), ProposalsEnabled: true, FastPathEnabled: true},
			Trading:           srv.tradingStatus(srv.endpoint),
			Proposals:         []rpc.TradeProposal{prop},
		},
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{
		Key:       prop.Key,
		Revision:  prop.Revision,
		Quantity:  1,
		TimeoutMs: 20,
		FastPath:  true,
	})
	if err != nil {
		t.Fatalf("GTC fast preview err = %v", err)
	}
	if !res.Accepted || !res.SubmitEligible || res.Preview == nil || len(res.Blockers) != 0 {
		t.Fatalf("GTC fast preview = %+v, want accepted submit-eligible with no blockers", res)
	}
	if res.Preview.Draft.TIF != rpc.OrderTIFGTC {
		t.Fatalf("GTC preview draft TIF = %q, want GTC end-to-end", res.Preview.Draft.TIF)
	}
}

func TestTrailingStopPreviewBlocksWhenWhatIfNotSubmitEligible(t *testing.T) {
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000, AllowStockShort: true})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusUnavailable, Available: false, Message: "timeout waiting for broker WhatIf response"}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	policyFingerprint := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	trailAmount := 8.0
	prop := rpc.TradeProposal{
		Key:               "trailing_stop:sap",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionQuantity:  1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		Trail:             &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetAmount, TrailingAmount: &trailAmount, InitialStopPrice: 92},
		TIF:               rpc.OrderTIFDay,
		Contract:          rpc.ContractParams{Symbol: "SAP", SecType: "STK", Exchange: "SMART", Currency: "EUR", Multiplier: 1},
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFingerprint,
		CreatedAt:         now,
	}
	srv.tradeProposals = &proposalEngine{
		server: srv,
		store:  testProposalStore(t),
		now:    func() time.Time { return now },
		snapshot: rpc.TradeProposalSnapshot{
			Kind:              rpc.TradeProposalSnapshotKind,
			SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
			AsOf:              now,
			Revision:          "sha256:rev",
			AccountID:         "DU1234567",
			AccountMode:       rpc.AccountModePaper,
			PolicyID:          "protection-mvp",
			PolicyVersion:     1,
			PolicyFingerprint: policyFingerprint,
			PolicyStatus:      rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault, PolicyID: "protection-mvp", Fingerprint: policyFingerprint},
			AutoTrade:         rpc.AutoTradeStatus{Trading: srv.tradingStatus(srv.endpoint), ProposalsEnabled: true, FastPathEnabled: true},
			Trading:           srv.tradingStatus(srv.endpoint),
			Proposals:         []rpc.TradeProposal{prop},
		},
		ignored: map[string]struct{}{},
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{Key: prop.Key, Revision: prop.Revision, Quantity: 1, TimeoutMs: 20, FastPath: true})
	if err != nil {
		t.Fatalf("fast preview err = %v", err)
	}
	if res.Accepted || res.SubmitEligible || !hasBlocker(res.Blockers, "preview_not_submit_eligible") {
		t.Fatalf("fast preview = %+v, want blocked not-submit-eligible result", res)
	}
	if res.Preview == nil || res.Preview.WhatIf.Status != rpc.OrderWhatIfStatusUnavailable {
		t.Fatalf("preview = %+v, want unavailable WhatIf context", res.Preview)
	}
}

// TestProposalPreviewCapsEveryProtectiveOrder pins the deliberate fail-closed
// posture for apparent exits. This client cannot prove that a manual TWS order
// has not already consumed exit capacity, so all three protection buckets stay
// subject to max_notional/max_option_contracts at preview.
func TestProposalPreviewCapsEveryProtectiveOrder(t *testing.T) {
	now := time.Date(2026, 6, 11, 13, 0, 0, 0, time.UTC)
	policyFingerprint := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	trailAmount := 8.0
	cases := []struct {
		name     string
		bid, ask float64
		position func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error)
		prop     rpc.TradeProposal
		quantity int
		wantCap  string
	}{
		{
			name: "trailing stop full position",
			bid:  479.90, ask: 480.10,
			position: fixedPreviewPosition(150, 0, rpc.OrderPositionEffectClose),
			prop: rpc.TradeProposal{
				Key: "trailing_stop:amd", Bucket: rpc.TradeProposalBucketTrailingStop,
				Symbol: "AMD", SecType: "STK", Action: rpc.OrderActionSell,
				Quantity: 150, MaxQuantity: 150, PositionQuantity: 150,
				PositionEffect: rpc.OrderPositionEffectClose,
				OrderType:      rpc.OrderTypeTRAIL,
				Trail:          &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetAmount, TrailingAmount: &trailAmount, InitialStopPrice: 441.50},
				TIF:            rpc.OrderTIFDay,
				Contract:       rpc.ContractParams{Symbol: "AMD", SecType: "STK", Exchange: "SMART", Currency: "USD", Multiplier: 1},
			},
			quantity: 150,
			wantCap:  "exceeds [trading].max_notional",
		},
		{
			name: "risk reduction half position",
			bid:  479.90, ask: 480.10,
			position: fixedPreviewPosition(150, 75, rpc.OrderPositionEffectReduce),
			prop: rpc.TradeProposal{
				Key: "risk_reduction:amd", Bucket: rpc.TradeProposalBucketRiskReduction,
				Symbol: "AMD", SecType: "STK", Action: rpc.OrderActionSell,
				Quantity: 75, MaxQuantity: 150, PositionQuantity: 150,
				PositionEffect: rpc.OrderPositionEffectReduce,
				OrderType:      rpc.OrderTypeLMT,
				TIF:            rpc.OrderTIFDay,
				Contract:       rpc.ContractParams{Symbol: "AMD", SecType: "STK", Exchange: "SMART", Currency: "USD", Multiplier: 1},
			},
			quantity: 75,
			wantCap:  "exceeds [trading].max_notional",
		},
		{
			name: "theta hygiene 30 contracts",
			bid:  3.95, ask: 4.05,
			position: fixedPreviewPosition(30, 0, rpc.OrderPositionEffectClose),
			prop: rpc.TradeProposal{
				Key: "theta_hygiene:spy", Bucket: rpc.TradeProposalBucketThetaHygiene,
				Symbol: "SPY", SecType: "OPT", Action: rpc.OrderActionSell,
				Quantity: 30, MaxQuantity: 30, PositionQuantity: 30,
				PositionEffect: rpc.OrderPositionEffectClose,
				OrderType:      rpc.OrderTypeLMT,
				TIF:            rpc.OrderTIFDay,
				Contract:       rpc.ContractParams{Symbol: "SPY", SecType: "OPT", Expiry: "20260619", Right: "C", Strike: 520, Multiplier: 100},
			},
			quantity: 30,
			wantCap:  "exceeds [trading].max_option_contracts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
			srv.orderPreviewQuote = fixedPreviewQuote(tc.bid, tc.ask)
			srv.orderPreviewPositionImpact = tc.position
			srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
				return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
			}
			prop := tc.prop
			prop.Revision = "sha256:rev"
			prop.State = rpc.TradeProposalStateGenerated
			prop.PolicyID = "protection-mvp"
			prop.PolicyVersion = 1
			prop.PolicyFingerprint = policyFingerprint
			prop.CreatedAt = now
			if prop.Bucket != rpc.TradeProposalBucketTrailingStop {
				// The preview fast path serves trailing-stop buckets only and
				// the slow path's Refresh needs a live gateway, so exercise
				// the identical post-resolution chain Preview runs.
				preview, err := srv.previewOrder(context.Background(), proposalOrderPreviewParams(prop, selectedProposalQty(prop, tc.quantity), 20))
				if err == nil || !strings.Contains(err.Error(), tc.wantCap) {
					t.Fatalf("previewOrder = %+v err=%v, want cap refusal containing %q", preview, err, tc.wantCap)
				}
				return
			}
			srv.tradeProposals = &proposalEngine{
				server:  srv,
				store:   testProposalStore(t),
				now:     func() time.Time { return now },
				ignored: map[string]struct{}{},
				snapshot: rpc.TradeProposalSnapshot{
					Kind:              rpc.TradeProposalSnapshotKind,
					SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
					AsOf:              now,
					Revision:          "sha256:rev",
					AccountID:         "DU1234567",
					AccountMode:       rpc.AccountModePaper,
					PolicyID:          "protection-mvp",
					PolicyVersion:     1,
					PolicyFingerprint: policyFingerprint,
					PolicyStatus:      rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault, PolicyID: "protection-mvp", Fingerprint: policyFingerprint},
					AutoTrade:         rpc.AutoTradeStatus{Trading: srv.tradingStatus(srv.endpoint), ProposalsEnabled: true, FastPathEnabled: true},
					Trading:           srv.tradingStatus(srv.endpoint),
					Proposals:         []rpc.TradeProposal{prop},
				},
			}

			res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{
				Key:       prop.Key,
				Revision:  prop.Revision,
				Quantity:  tc.quantity,
				TimeoutMs: 20,
				FastPath:  true,
			})
			if err != nil {
				t.Fatalf("fast preview err = %v", err)
			}
			if res.Accepted || res.SubmitEligible || res.Preview != nil || !hasBlocker(res.Blockers, "preview_failed") {
				t.Fatalf("fast preview = %+v, want typed preview_failed cap refusal", res)
			}
			if len(res.Blockers) == 0 || !strings.Contains(res.Blockers[0].Message, tc.wantCap) {
				t.Fatalf("fast preview blockers = %+v, want cap refusal containing %q", res.Blockers, tc.wantCap)
			}
		})
	}
}

func preservedTrailingStopProposal(now time.Time) rpc.TradeProposal {
	trailPercent := 8.0
	policyFingerprint := fingerprintProtectionPolicy(defaultProtectionPolicy())
	return rpc.TradeProposal{
		Key:               "trailing_stop:sap",
		Revision:          "sha256:rev",
		State:             rpc.TradeProposalStateGenerated,
		Bucket:            rpc.TradeProposalBucketTrailingStop,
		Symbol:            "SAP",
		SecType:           "STK",
		Action:            rpc.OrderActionSell,
		Quantity:          1,
		MaxQuantity:       1,
		PositionQuantity:  1,
		PositionEffect:    rpc.OrderPositionEffectClose,
		OrderType:         rpc.OrderTypeTRAIL,
		Trail:             &rpc.OrderTrailSpec{Basis: rpc.OrderTrailBasisInstrumentPrice, OffsetType: rpc.OrderTrailOffsetPercent, TrailingPercent: &trailPercent, InitialStopPrice: 92},
		TIF:               rpc.OrderTIFDay,
		Contract:          rpc.ContractParams{Symbol: "SAP", SecType: "STK", Exchange: "SMART", Currency: "EUR", Multiplier: 1},
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: policyFingerprint,
		CreatedAt:         now,
	}
}

func preservedProposalSnapshot(now time.Time, prop rpc.TradeProposal, blockers []rpc.TradingBlocker) rpc.TradeProposalSnapshot {
	return rpc.TradeProposalSnapshot{
		Kind:              rpc.TradeProposalSnapshotKind,
		SchemaVersion:     rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:              now,
		Revision:          prop.Revision,
		AccountID:         "DU1234567",
		AccountMode:       rpc.AccountModePaper,
		PolicyID:          prop.PolicyID,
		PolicyVersion:     prop.PolicyVersion,
		PolicyFingerprint: prop.PolicyFingerprint,
		PolicyStatus: rpc.ProtectionPolicyStatus{
			Status:        rpc.ProtectionPolicyStatusDefault,
			PolicyID:      prop.PolicyID,
			PolicyVersion: prop.PolicyVersion,
			Fingerprint:   prop.PolicyFingerprint,
		},
		Proposals: []rpc.TradeProposal{prop},
		Counts:    proposalCounts([]rpc.TradeProposal{prop}, ""),
		Blockers:  append([]rpc.TradingBlocker(nil), blockers...),
	}
}

func hasOrderJournalEvent(events []orderJournalEvent, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func testProposalStore(t *testing.T) *proposalStore {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("secure proposal test authority directory: %v", err)
	}
	core, err := corestore.Open(t.Context(), corestore.Options{Path: filepath.Join(dir, "daemon.db")})
	if err != nil {
		t.Fatalf("open proposal test authority: %v", err)
	}
	t.Cleanup(func() { _ = core.Close() })
	if err := initializeCleanProposalOpportunityAuthority(t.Context(), core); err != nil {
		t.Fatalf("initialize proposal test authority: %v", err)
	}
	store := &proposalStore{}
	if _, _, err := store.bindCore(t.Context(), core); err != nil {
		t.Fatalf("attach proposal test authority: %v", err)
	}
	return store
}

func TestTrailingStopFastPathPreviewBlocksPreservedSnapshotBlockers(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("preview must not fetch quotes while preserved snapshot has blockers")
		return rpc.OrderQuoteSnapshot{}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	prop := preservedTrailingStopProposal(now)
	srv.tradeProposals = &proposalEngine{
		server:  srv,
		store:   testProposalStore(t),
		now:     func() time.Time { return now },
		ignored: map[string]struct{}{},
		snapshot: preservedProposalSnapshot(now, prop, []rpc.TradingBlocker{
			{Code: "account_unavailable", Message: "account snapshot failed"},
		}),
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{
		Key:      prop.Key,
		Revision: prop.Revision,
		Quantity: 1,
		FastPath: true,
	})
	if err != nil {
		t.Fatalf("preview err = %v", err)
	}
	if res.Accepted || res.Preview != nil || !hasBlocker(res.Blockers, "account_unavailable") {
		t.Fatalf("preview = %+v, want blocked by preserved snapshot blocker", res)
	}
}

func TestTrailingStopPreviewBlocksStaleRevisionBeforeToken(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("stale revision must block before preview quote")
		return rpc.OrderQuoteSnapshot{}, nil
	}
	srv.orderPreviewPositionImpact = func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error) {
		t.Fatal("stale revision must block before preview position")
		return rpc.OrderPositionImpact{}, nil
	}
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		t.Fatal("stale revision must block before broker WhatIf preview")
		return rpc.OrderWhatIfResult{}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	prop := preservedTrailingStopProposal(now)
	submittedRevision := prop.Revision
	prop.Revision = "sha256:changed-material-intent"
	prop.Quantity = 2
	prop.MaxQuantity = 2
	prop.PositionQuantity = 2
	srv.tradeProposals = &proposalEngine{
		server:   srv,
		store:    testProposalStore(t),
		now:      func() time.Time { return now },
		ignored:  map[string]struct{}{},
		snapshot: preservedProposalSnapshot(now, prop, nil),
	}

	res, err := srv.tradeProposals.Preview(context.Background(), rpc.TradeProposalPreviewParams{
		Key:       prop.Key,
		Revision:  submittedRevision,
		Quantity:  1,
		TimeoutMs: 20,
		FastPath:  true,
	})
	if err != nil {
		t.Fatalf("preview err = %v", err)
	}
	if res.Accepted || res.Preview != nil || res.PreviewTokenID != "" || res.SubmitEligible || !hasBlocker(res.Blockers, "stale_revision") {
		t.Fatalf("preview = %+v, want stale_revision before token mint", res)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("order journal events = %+v, want none before stale-revision blocker", events)
	}
}

func TestTrailingStopSubmitBlocksPreservedRefreshFailureBeforePreview(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("submit must not preview while refresh preserved stale snapshot with blockers")
		return rpc.OrderQuoteSnapshot{}, nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	prop := preservedTrailingStopProposal(now)
	snap := preservedProposalSnapshot(now.Add(-time.Hour), prop, nil)
	snap.LoadedFromState = true
	srv.tradeProposals = &proposalEngine{
		server:   srv,
		store:    testProposalStore(t),
		now:      func() time.Time { return now },
		ignored:  map[string]struct{}{},
		snapshot: snap,
	}

	res, err := srv.tradeProposals.Submit(context.Background(), rpc.TradeProposalSubmitParams{
		Key:      prop.Key,
		Revision: prop.Revision,
		Quantity: 1,
		FastPath: true,
	})
	if err != nil {
		t.Fatalf("submit err = %v", err)
	}
	if res.Accepted || res.Preview != nil || !hasBlocker(res.Blockers, "account_unavailable") {
		t.Fatalf("submit = %+v, want blocked by preserved refresh failure", res)
	}
}

func TestTrailingStopSubmitUsesFreshCachedSnapshotWhenRevisionMatches(t *testing.T) {
	t.Parallel()
	if !orderWritesAvailable {
		t.Skip("proposal submit place path requires trading build")
	}
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000, AllowStockShort: true})
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusAccepted, Available: true}, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) { return 1001, nil }
	var sentOrder *ibkrlib.RawOrder
	srv.orderPlaceBroker = func(_ context.Context, _ *ibkrlib.Contract, order *ibkrlib.RawOrder) error {
		copy := *order
		sentOrder = &copy
		return nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	prop := preservedTrailingStopProposal(now)
	srv.tradeProposals = &proposalEngine{
		server:   srv,
		store:    testProposalStore(t),
		now:      func() time.Time { return now },
		ignored:  map[string]struct{}{},
		snapshot: preservedProposalSnapshot(now, prop, nil),
	}

	res, err := srv.tradeProposals.Submit(context.Background(), rpc.TradeProposalSubmitParams{
		Key:       prop.Key,
		Revision:  prop.Revision,
		Quantity:  1,
		TimeoutMs: 20,
		FastPath:  true,
		Origin:    rpc.OrderOriginAgent,
	})
	if err != nil {
		t.Fatalf("submit err = %v", err)
	}
	if !res.Accepted || res.Preview == nil || res.Place == nil {
		t.Fatalf("submit = %+v, want accepted preview-backed place", res)
	}
	if res.Proposal.Revision != prop.Revision {
		t.Fatalf("submit proposal revision = %q, want current cached revision %q", res.Proposal.Revision, prop.Revision)
	}
	if res.Place.ReservedOrderID != 1001 || res.Place.Mode != config.TradingModePaper {
		t.Fatalf("place = %+v, want paper order id 1001", res.Place)
	}
	if sentOrder == nil || sentOrder.OrderID != 1001 || sentOrder.Action != rpc.OrderActionSell || sentOrder.OrderType != rpc.OrderTypeTRAIL {
		t.Fatalf("sent order = %+v, want paper trailing stop sell", sentOrder)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if !hasOrderJournalEvent(events, orderJournalEventPreviewed) ||
		!hasOrderJournalEvent(events, orderJournalEventTokenConfirmed) ||
		!hasOrderJournalEvent(events, orderJournalEventSendAttempted) {
		t.Fatalf("order journal events = %+v, want previewed, token-confirmed, send-attempted", events)
	}
}

func TestTrailingStopSubmitBlocksStaleRevisionBeforePreview(t *testing.T) {
	t.Parallel()
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModePaper, MaxNotional: 10_000})
	srv.orderPreviewQuote = func(context.Context, rpc.ContractParams, time.Duration) (rpc.OrderQuoteSnapshot, error) {
		t.Fatal("stale revision must block before preview quote")
		return rpc.OrderQuoteSnapshot{}, nil
	}
	srv.orderPreviewPositionImpact = func(context.Context, rpc.ContractParams, string, int) (rpc.OrderPositionImpact, error) {
		t.Fatal("stale revision must block before preview position")
		return rpc.OrderPositionImpact{}, nil
	}
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		t.Fatal("stale revision must block before broker WhatIf preview")
		return rpc.OrderWhatIfResult{}, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		t.Fatal("stale revision must block before reserving broker order id")
		return 0, nil
	}
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("stale revision must block before broker place")
		return nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	prop := preservedTrailingStopProposal(now)
	submittedRevision := prop.Revision
	prop.Revision = "sha256:changed-material-intent"
	prop.Quantity = 2
	prop.MaxQuantity = 2
	prop.PositionQuantity = 2
	srv.tradeProposals = &proposalEngine{
		server:   srv,
		store:    testProposalStore(t),
		now:      func() time.Time { return now },
		ignored:  map[string]struct{}{},
		snapshot: preservedProposalSnapshot(now, prop, nil),
	}

	res, err := srv.tradeProposals.Submit(context.Background(), rpc.TradeProposalSubmitParams{
		Key:       prop.Key,
		Revision:  submittedRevision,
		Quantity:  1,
		TimeoutMs: 20,
		FastPath:  true,
		Origin:    rpc.OrderOriginAgent,
	})
	if err != nil {
		t.Fatalf("submit err = %v", err)
	}
	if res.Accepted || res.Preview != nil || res.Place != nil || !hasBlocker(res.Blockers, "stale_revision") {
		t.Fatalf("submit = %+v, want stale_revision before preview/place", res)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("order journal events = %+v, want none before stale-revision blocker", events)
	}
}

func TestTrailingStopSubmitLiveAgentUsesGatedPreview(t *testing.T) {
	t.Parallel()
	if !orderWritesAvailable {
		t.Skip("live-agent submit preview path requires trading build")
	}
	srv := newOrderPreviewTestServer(t, config.Trading{Mode: config.TradingModeLive, MaxNotional: 10_000, AllowStockShort: true})
	livePort := 4001
	srv.cfg.Gateway.Port = &livePort
	srv.cfg.Gateway.Account = "U1234567"
	srv.endpoint.Port = livePort
	srv.endpoint.Account = "U1234567"
	srv.orderPreviewQuote = fixedPreviewQuote(100, 101)
	srv.orderPreviewPositionImpact = fixedPreviewPosition(1, 0, rpc.OrderPositionEffectClose)
	var whatIfReached bool
	srv.orderPreviewWhatIf = func(context.Context, rpc.OrderDraft) (rpc.OrderWhatIfResult, error) {
		whatIfReached = true
		return rpc.OrderWhatIfResult{Status: rpc.OrderWhatIfStatusRejected, Available: true, RequiredForSubmit: true, Message: "test rejection"}, nil
	}
	srv.orderReserveBrokerID = func(context.Context) (int, error) {
		t.Fatal("non-submit-eligible preview must block before reserving broker order id")
		return 0, nil
	}
	srv.orderPlaceBroker = func(context.Context, *ibkrlib.Contract, *ibkrlib.RawOrder) error {
		t.Fatal("non-submit-eligible preview must block before broker place")
		return nil
	}
	now := time.Date(2026, 6, 9, 13, 0, 0, 0, time.UTC)
	prop := preservedTrailingStopProposal(now)
	snap := preservedProposalSnapshot(now, prop, nil)
	snap.AccountID = "U1234567"
	snap.AccountMode = rpc.AccountModeLive
	srv.tradeProposals = &proposalEngine{
		server:   srv,
		store:    testProposalStore(t),
		now:      func() time.Time { return now },
		ignored:  map[string]struct{}{},
		snapshot: snap,
	}

	res, err := srv.tradeProposals.Submit(context.Background(), rpc.TradeProposalSubmitParams{
		Key:       prop.Key,
		Revision:  prop.Revision,
		Quantity:  1,
		TimeoutMs: 20,
		FastPath:  true,
		Origin:    rpc.OrderOriginAgent,
	})
	if err != nil {
		t.Fatalf("submit err = %v", err)
	}
	if !whatIfReached {
		t.Fatalf("live agent-origin submit did not reach gated WhatIf preview; result=%+v", res)
	}
	if res.Accepted || res.Preview == nil || res.Place != nil || !hasBlocker(res.Blockers, "preview_not_submit_eligible") {
		t.Fatalf("submit = %+v, want preview_not_submit_eligible after gated preview", res)
	}
	events, err := srv.orderJournal.LoadEvents(0)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("order journal events = %+v, want preview evidence before submit blocker", events)
	}
	for _, ev := range events {
		if ev.Type == orderJournalEventSendAttempted {
			t.Fatalf("order journal events = %+v, want no send attempt after rejected WhatIf", events)
		}
	}
}

func TestProposalRevisionIgnoresRegimeLifecycleChurn(t *testing.T) {
	policy := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	sources := rpc.TradeProposalSourceFingerprints{
		Account:   &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
		Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
		Regime:    &rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime-a"},
	}
	proposals := []rpc.TradeProposal{{Key: "theta_hygiene:abc", Quantity: 1, PositionEffect: rpc.OrderPositionEffectClose}}
	scope := brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}
	a := proposalRevision(policy, sources, scope, proposals)
	sources.Regime = &rpc.Fingerprint{Version: rpc.RegimeFingerprintVersion, Key: "sha256:regime-b"}
	b := proposalRevision(policy, sources, scope, proposals)
	if a != b {
		t.Fatalf("revision changed on regime-only churn: %s != %s", a, b)
	}
	sources.Positions = &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions-b"}
	c := proposalRevision(policy, sources, scope, proposals)
	if c == a {
		t.Fatalf("revision did not change on positions fingerprint change: %s", c)
	}
}

func TestProposalRevisionIgnoresMarketEventSourceChurn(t *testing.T) {
	policy := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	sources := rpc.TradeProposalSourceFingerprints{
		Account:      &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
		Positions:    &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
		MarketEvents: &rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-a"},
	}
	proposals := []rpc.TradeProposal{{Key: "risk_reduction:abc", Quantity: 1, PositionEffect: rpc.OrderPositionEffectReduce}}
	scope := brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}
	a := proposalRevision(policy, sources, scope, proposals)
	sources.MarketEvents = &rpc.Fingerprint{Version: rpc.MarketEventsFingerprintVersion, Key: "sha256:market-b"}
	if b := proposalRevision(policy, sources, scope, proposals); b != a {
		t.Fatalf("revision changed on market-event-only churn: %s != %s", a, b)
	}
}

func TestMarketEventHardBlockerBlocksProposal(t *testing.T) {
	prop := rpc.TradeProposal{
		Symbol:         "CRWV",
		Action:         rpc.OrderActionSell,
		PositionEffect: rpc.OrderPositionEffectReduce,
	}
	events := &rpc.MarketEventsResult{BySymbol: map[string][]rpc.MarketEventFlag{
		"CRWV": {{
			ID:       rpc.MarketEventHaltRegulatoryOrNews,
			Symbol:   "CRWV",
			Label:    "Halt",
			Status:   rpc.MarketEventStatusActive,
			Severity: rpc.MarketEventSeverityBlock,
			Role:     rpc.MarketEventRoleHardBlocker,
			Source:   "Nasdaq trade halt RSS",
		}},
	}}
	applyMarketEventFlagsToProposal(&prop, events)
	if prop.State != rpc.TradeProposalStateBlocked {
		t.Fatalf("state=%q, want blocked", prop.State)
	}
	if !hasBlocker(prop.Blockers, "market_event_"+rpc.MarketEventHaltRegulatoryOrNews) {
		t.Fatalf("blockers=%+v, want market-event blocker", prop.Blockers)
	}
}

func TestBorrowMarketFlagOnlyAppliesToShortBuyToCover(t *testing.T) {
	for _, flag := range []rpc.MarketEventFlag{
		{
			ID:       rpc.MarketEventBorrowInventoryTight,
			Symbol:   "CRWV",
			Label:    "Borrow tight",
			Status:   rpc.MarketEventStatusActive,
			Severity: rpc.MarketEventSeverityWatch,
			Role:     rpc.MarketEventRoleProposalModifier,
		},
		{
			ID:       rpc.MarketEventBorrowFeeExtreme,
			Symbol:   "CRWV",
			Label:    "Fee extreme",
			Status:   rpc.MarketEventStatusActive,
			Severity: rpc.MarketEventSeverityAct,
			Role:     rpc.MarketEventRoleProposalModifier,
		},
	} {
		events := &rpc.MarketEventsResult{BySymbol: map[string][]rpc.MarketEventFlag{"CRWV": {flag}}}
		longSell := rpc.TradeProposal{
			Symbol:           "CRWV",
			Action:           rpc.OrderActionSell,
			PositionQuantity: 100,
			PositionEffect:   rpc.OrderPositionEffectReduce,
		}
		if got := proposalMarketEventFlags(longSell, events); len(got) != 0 {
			t.Fatalf("long sell %s flags=%+v, want none", flag.ID, got)
		}
		shortCover := rpc.TradeProposal{
			Symbol:           "CRWV",
			Action:           rpc.OrderActionBuy,
			PositionQuantity: -100,
			PositionEffect:   rpc.OrderPositionEffectReduce,
		}
		if got := proposalMarketEventFlags(shortCover, events); len(got) != 1 || got[0].ID != flag.ID {
			t.Fatalf("short cover %s flags=%+v, want borrow flag", flag.ID, got)
		}
	}
}

func TestProposalOutcomeMarksAreIdempotentPerProposalDate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trade-proposal-outcomes.jsonl")
	store := newProposalOutcomeStore(path)
	mark := proposalOutcomeMark{
		At:                time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC),
		MarkDate:          "2026-06-06",
		State:             proposalOutcomeStateMarked,
		ProposalKey:       "theta_hygiene:abc",
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:test"},
		MarkPrice:         1.23,
	}
	if err := store.AppendMark(mark); err != nil {
		t.Fatalf("append first outcome: %v", err)
	}
	if err := store.AppendMark(mark); err != nil {
		t.Fatalf("append duplicate outcome: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read outcomes: %v", err)
	}
	if got := strings.Count(string(raw), "\n"); got != 1 {
		t.Fatalf("outcome rows=%d, want 1; file=%s", got, raw)
	}
}

func TestProposalDailyMarkCarriesPolicyIdentity(t *testing.T) {
	price := 1.25
	prop := rpc.TradeProposal{
		Key:               "theta_hygiene:abc",
		Revision:          "sha256:rev",
		Bucket:            rpc.TradeProposalBucketThetaHygiene,
		Symbol:            "AAPL",
		SecType:           "OPT",
		Action:            rpc.OrderActionSell,
		Quantity:          2,
		LimitPrice:        &price,
		PolicyID:          "protection-mvp",
		PolicyVersion:     1,
		PolicyFingerprint: rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"},
		SourceFingerprints: rpc.TradeProposalSourceFingerprints{
			Account:   &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
			Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
		},
	}
	mark := proposalOutcomeMarked(prop, time.Date(2026, 6, 6, 10, 0, 0, 0, time.UTC))
	if mark.State != proposalOutcomeStateMarked || mark.MarkDate != "2026-06-06" {
		t.Fatalf("daily mark state/date = %q/%q", mark.State, mark.MarkDate)
	}
	if mark.PolicyID != prop.PolicyID || mark.PolicyFingerprint.Key != prop.PolicyFingerprint.Key {
		t.Fatalf("daily mark missing policy identity: %+v", mark)
	}
	if mark.MarkPrice != price || mark.BaselinePrice != price {
		t.Fatalf("daily mark price/baseline = %.2f/%.2f, want %.2f", mark.MarkPrice, mark.BaselinePrice, price)
	}
}

func TestProposalFillOutcomeCarriesPolicyIdentity(t *testing.T) {
	submitted := proposalEvent{
		Type:              "submitted",
		Key:               "risk_reduction:def",
		Revision:          "sha256:rev",
		Bucket:            rpc.TradeProposalBucketRiskReduction,
		PolicyID:          "protection-mvp",
		PolicyVersion:     2,
		PolicyFingerprint: rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"},
		SourceFingerprints: rpc.TradeProposalSourceFingerprints{
			Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
		},
	}
	ev := orderJournalEvent{
		Source:         proposalOrderSource,
		OrderRef:       "ibkr-20260606-100000",
		PreviewTokenID: "ptok_123",
		ExecID:         "exec-1",
		Symbol:         "MSFT",
		SecType:        "STK",
		Action:         rpc.OrderActionSell,
		Quantity:       5,
		Filled:         5,
		LimitPrice:     100,
		AvgFillPrice:   101,
		Multiplier:     1,
	}
	mark := proposalOutcomeFilledFromJournal(ev, submitted, time.Date(2026, 6, 6, 10, 1, 0, 0, time.UTC))
	if mark.ProposalKey != submitted.Key || mark.PolicyID != submitted.PolicyID || mark.PolicyVersion != submitted.PolicyVersion || mark.PolicyFingerprint.Key != submitted.PolicyFingerprint.Key {
		t.Fatalf("fill outcome missing submitted policy identity: %+v", mark)
	}
	if mark.ExecutionPnL != 5 {
		t.Fatalf("execution pnl=%.2f, want 5.00", mark.ExecutionPnL)
	}
}

// newProposalScopeTestServer builds the minimal Server a proposalEngine
// Refresh needs before it touches the gateway: resolved config (trading mode
// defaults to disabled, so tradingStatus never reaches the order journal),
// an embedded-default protection policy, and a discovery endpoint that
// currentBrokerStateScope falls back to while no connector is attached.
func newProposalScopeTestServer(t *testing.T, ep discover.Endpoint, now time.Time) *Server {
	t.Helper()
	pm := newProtectionPolicyManager("", false, time.Second, func() time.Time { return now })
	pm.reload()
	return &Server{
		cfg:                &config.Resolved{},
		protectionPolicies: pm,
		endpoint:           ep,
		now:                func() time.Time { return now },
	}
}

func newProposalScopeTestEngine(t *testing.T, srv *Server) *proposalEngine {
	t.Helper()
	return &proposalEngine{
		server:  srv,
		store:   testProposalStore(t),
		now:     srv.now,
		ignored: map[string]struct{}{},
	}
}

func scopedTestSnapshot(account, mode string, asOf time.Time) rpc.TradeProposalSnapshot {
	return rpc.TradeProposalSnapshot{
		Kind:          rpc.TradeProposalSnapshotKind,
		SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:          asOf,
		Revision:      "sha256:test",
		AccountID:     account,
		AccountMode:   mode,
		Proposals: []rpc.TradeProposal{{
			Key:            "theta_hygiene:abc",
			Revision:       "sha256:test",
			State:          rpc.TradeProposalStateGenerated,
			Bucket:         rpc.TradeProposalBucketThetaHygiene,
			Symbol:         "SYNTH",
			SecType:        "STK",
			Action:         rpc.OrderActionSell,
			Quantity:       1,
			MaxQuantity:    1,
			PositionEffect: rpc.OrderPositionEffectClose,
		}},
	}
}

func TestBrokerScopeConcreteRejectsAggregateIdentities(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		scope brokerStateScope
		want  bool
	}{
		{"paper account", brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, true},
		{"live account", brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}, true},
		{"aggregate All", brokerStateScope{Account: "All", Mode: rpc.AccountModeLive}, false},
		{"aggregate All padded", brokerStateScope{Account: " All ", Mode: rpc.AccountModeLive}, false},
		{"empty account", brokerStateScope{Account: "", Mode: rpc.AccountModeLive}, false},
		{"multi-account list", brokerStateScope{Account: "DU7654321,U1234567", Mode: rpc.AccountModeLive}, false},
		{"unknown mode", brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeUnknown}, false},
		{"empty mode", brokerStateScope{Account: "U1234567", Mode: ""}, false},
	}
	for _, tc := range cases {
		if got := brokerScopeConcrete(tc.scope); got != tc.want {
			t.Errorf("%s: brokerScopeConcrete(%+v) = %v, want %v", tc.name, tc.scope, got, tc.want)
		}
	}
}

func TestProposalRefreshRejectsUnscopedAccountIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	// Aggregate "All" account on a live port: the exact identity the
	// leaked snapshot was persisted under. The nil connector means a
	// pass-through gate would fail with account_unavailable instead —
	// asserting on the blocker code proves the scope gate runs first.
	srv := newProposalScopeTestServer(t, discover.Endpoint{Host: "127.0.0.1", Port: 7496, Account: "All"}, now)
	e := newProposalScopeTestEngine(t, srv)

	snap, err := e.Refresh(context.Background(), false)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(snap.Proposals) != 0 {
		t.Fatalf("proposals = %+v, want none", snap.Proposals)
	}
	if !hasBlocker(snap.Blockers, "account_identity_unscoped") {
		t.Fatalf("blockers = %+v, want account_identity_unscoped", snap.Blockers)
	}
	if hasBlocker(snap.Blockers, "account_unavailable") {
		t.Fatalf("blockers = %+v, scope gate must run before the account summary", snap.Blockers)
	}
	if snap.AccountID != "" || snap.AccountMode != "" {
		t.Fatalf("unscoped shell stamped with identity %q/%q, want empty", snap.AccountID, snap.AccountMode)
	}
}

func TestProposalSnapshotServeRefusesScopeMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	e.snapshot = scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive} }

	got := e.Snapshot(true)
	if len(got.Proposals) != 0 {
		t.Fatalf("served %d paper proposals into a live session", len(got.Proposals))
	}
	if !hasBlocker(got.Blockers, "proposal_scope_mismatch") {
		t.Fatalf("blockers = %+v, want proposal_scope_mismatch", got.Blockers)
	}
	if got.AccountID != "U1234567" || got.AccountMode != rpc.AccountModeLive {
		t.Fatalf("refusal shell identity %q/%q, want connected session", got.AccountID, got.AccountMode)
	}
	// Refusal must not mark the cached proposals as shown nor overwrite the
	// clean authoritative current document with the refusal shell.
	e.store.mu.Lock()
	for _, event := range e.store.events {
		if event.Type == "shown" {
			e.store.mu.Unlock()
			t.Fatalf("refused serve appended shown event: %+v", event)
		}
	}
	e.store.mu.Unlock()
	persisted, err := e.store.LoadCurrent()
	if err != nil {
		t.Fatalf("load authoritative current: %v", err)
	}
	if persisted.Revision != "empty" || len(persisted.Proposals) != 0 {
		t.Fatalf("refused serve changed authoritative current: %+v", persisted)
	}
	if e.snapshot.AccountID != "DU7654321" || len(e.snapshot.Proposals) != 1 {
		t.Fatalf("refused serve mutated stored snapshot: %+v", e.snapshot)
	}

	// Matching session serves the stored proposals (case-insensitively).
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "du7654321", Mode: rpc.AccountModePaper} }
	got = e.Snapshot(false)
	if len(got.Proposals) != 1 || len(got.Blockers) != 0 {
		t.Fatalf("matching scope refused: proposals=%d blockers=%+v", len(got.Proposals), got.Blockers)
	}
}

func TestProposalSnapshotServeRefusesWhenCurrentScopeUnknown(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	e.snapshot = scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)
	e.scope = func() brokerStateScope { return brokerStateScope{} }

	got := e.Snapshot(false)
	if len(got.Proposals) != 0 {
		t.Fatalf("served proposals while session identity is unknown: %+v", got.Proposals)
	}
	if !hasBlocker(got.Blockers, "account_identity_unscoped") {
		t.Fatalf("blockers = %+v, want account_identity_unscoped (not a fabricated mismatch)", got.Blockers)
	}
}

func TestProposalServeGuardPassesBlockerShells(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	shell := emptyProposalSnapshot(now)
	shell.Blockers = []rpc.TradingBlocker{{Code: "proposals_disabled", Message: "manual protection proposals are disabled by config"}}
	e.snapshot = shell
	e.scope = func() brokerStateScope { return brokerStateScope{} }

	got := e.Snapshot(false)
	if !hasBlocker(got.Blockers, "proposals_disabled") {
		t.Fatalf("blockers = %+v, want session-independent shell served as-is", got.Blockers)
	}
}

func TestProposalInstallScopedFailsClosedOnScopeChange(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	// Session switched between refresh-start (paper scope the data was
	// fetched under) and install: the generated snapshot must never be
	// installed or persisted.
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive} }
	snap := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)

	got, err := e.installScoped(snap, brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, false, nil)
	if err != nil {
		t.Fatalf("scope-change install: %v", err)
	}
	if len(got.Proposals) != 0 || !hasBlocker(got.Blockers, "proposal_scope_mismatch") {
		t.Fatalf("installScoped result = %+v, want proposal_scope_mismatch shell", got)
	}
	// The wrong-scope generated snapshot must never replace the clean SQLite
	// current document. The mismatch shell remains a transient served view.
	persisted, err := e.store.LoadCurrent()
	if err != nil {
		t.Fatalf("load current after scope mismatch: %v", err)
	}
	if persisted.Revision != "empty" || len(persisted.Proposals) != 0 {
		t.Fatalf("scope mismatch changed authoritative current: %+v", persisted)
	}

	// Stable scope installs the generated snapshot unchanged — and that
	// one IS persisted for warm-start adoption.
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper} }
	got, err = e.installScoped(snap, brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, false, nil)
	if err != nil {
		t.Fatalf("stable-scope install: %v", err)
	}
	if len(got.Proposals) != 1 || len(got.Blockers) != 0 {
		t.Fatalf("stable scope install = %+v, want generated snapshot", got)
	}
	persisted, err = e.store.LoadCurrent()
	if err != nil {
		t.Fatalf("load stable-scope current: %v", err)
	}
	if persisted.Revision != snap.Revision || len(persisted.Proposals) != 1 || persisted.Proposals[0].Key != "theta_hygiene:abc" {
		t.Fatalf("authoritative current missing generated proposal: %+v", persisted)
	}
}

func TestInstallProposalEngineNeverReadsLegacySnapshot(t *testing.T) {
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	writeCurrent := func(t *testing.T, body string) *Server {
		t.Helper()
		dir := t.TempDir()
		t.Setenv("XDG_STATE_HOME", dir)
		path := filepath.Join(dir, "ibkr", "trade-proposals-current.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
		srv.installProposalEngine()
		return srv
	}

	// The exact unscoped shape from the originating incident: schema v1,
	// account_id "All", no account_mode.
	legacy := `{"kind":"ibkr.trade_proposal_snapshot","schema_version":"trade-proposal-snapshot-v1","as_of":"2026-06-10T12:54:00Z","revision":"sha256:legacy","account_id":"All","policy_id":"protection-mvp","policy_status":{"status":"default"},"auto_trade":{"trading":{},"proposals_enabled":true,"enabled":false,"auto_submit":false,"fast_path_enabled":true,"hot_reload":true,"blocked":false,"policy":{"status":"default"}},"trading":{},"proposals":[{"key":"theta_hygiene:abc","revision":"sha256:legacy","state":"generated","bucket":"theta_hygiene","rank":1,"symbol":"SAP","sec_type":"STK","action":"SELL","quantity":1,"max_quantity":1,"position_quantity":1,"position_effect":"close","order_type":"LMT","tif":"DAY","outside_rth":false,"contract":{"symbol":"SAP"},"reason":"test"}],"counts":{"total":1,"actionable":1,"theta_hygiene":1,"risk_reduction":0}}`
	srv := writeCurrent(t, legacy)
	if srv.tradeProposals == nil {
		t.Fatal("proposal engine not installed")
	}
	if got := srv.tradeProposals.snapshot; got.Kind != "" {
		t.Fatalf("legacy unscoped snapshot adopted at load: %+v", got)
	}

	scoped, err := json.Marshal(scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now))
	if err != nil {
		t.Fatal(err)
	}
	srv = writeCurrent(t, string(scoped))
	if got := srv.tradeProposals.snapshot; got.Kind != "" {
		t.Fatalf("constructor adopted a legacy scoped snapshot before SQLite attach: %+v", got)
	}

	shell, err := json.Marshal(emptyProposalSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	srv = writeCurrent(t, string(shell))
	if got := srv.tradeProposals.snapshot; got.Kind != "" {
		t.Fatalf("constructor adopted a legacy shell before SQLite attach: %+v", got)
	}
}

func TestProposalSubmitBlockedOnUnscopedAccountIdentity(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{Host: "127.0.0.1", Port: 7496, Account: "All"}, now)
	e := newProposalScopeTestEngine(t, srv)

	res, err := e.Submit(context.Background(), rpc.TradeProposalSubmitParams{Key: "theta_hygiene:abc", Revision: "sha256:test", FastPath: true})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.Accepted {
		t.Fatal("submit accepted under unscoped account identity")
	}
	if !hasBlocker(res.Blockers, "account_identity_unscoped") {
		t.Fatalf("blockers = %+v, want account_identity_unscoped", res.Blockers)
	}
	if res.Preview != nil || res.Place != nil {
		t.Fatalf("submit reached preview/place despite unscoped identity: %+v", res)
	}
}

func TestProposalFastPathPreviewRefusesScopeMismatch(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	e.snapshot = scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)
	e.scope = func() brokerStateScope { return brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive} }

	prop, blockers, ok := e.fastPathPreviewProposal("theta_hygiene:abc", "sha256:test")
	if !ok {
		t.Fatal("fast path fell through to revalidation; scope mismatch must fail closed in the fast path")
	}
	if prop.Key != "" {
		t.Fatalf("fast path returned a foreign-session proposal: %+v", prop)
	}
	if !hasBlocker(blockers, "proposal_scope_mismatch") {
		t.Fatalf("blockers = %+v, want proposal_scope_mismatch", blockers)
	}
}

func TestProposalPreserveOnFailureDropsForeignScopeSnapshot(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	snap := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now)
	snap.PolicyStatus = rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault}
	e.snapshot = snap

	// Paper→live switch with a transient account fetch failure: the old
	// paper snapshot must not be preserved into the live session.
	_, ok := e.preserveSnapshotOnRefreshFailure(
		brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive},
		rpc.AutoTradeStatus{},
		rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault},
		[]rpc.TradingBlocker{{Code: "account_unavailable", Message: "transient"}},
		false,
	)
	if ok {
		t.Fatal("foreign-scope snapshot preserved across a session switch")
	}

	// Same session: preservation still works.
	preserved, ok := e.preserveSnapshotOnRefreshFailure(
		brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper},
		rpc.AutoTradeStatus{},
		rpc.ProtectionPolicyStatus{Status: rpc.ProtectionPolicyStatusDefault},
		[]rpc.TradingBlocker{{Code: "account_unavailable", Message: "transient"}},
		false,
	)
	if !ok || len(preserved.Proposals) != 1 {
		t.Fatalf("same-scope snapshot not preserved: ok=%v %+v", ok, preserved)
	}
}

func TestProposalIgnoreIsScopedPerAccountMode(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 10, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, now)
	e := newProposalScopeTestEngine(t, srv)
	paper := brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}
	live := brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}

	e.scope = func() brokerStateScope { return paper }
	e.Ignore(rpc.TradeProposalIgnoreParams{Key: "theta_hygiene:abc", Reason: "test"})
	if !e.isIgnored(paper, "theta_hygiene:abc") {
		t.Fatal("ignore not effective in its own scope")
	}
	if e.isIgnored(live, "theta_hygiene:abc") {
		t.Fatal("paper ignore suppressed the same contract on the live session")
	}

	// Ignores recorded while the session identity is unknown must never
	// suppress proposals in a concrete session.
	e.scope = func() brokerStateScope { return brokerStateScope{} }
	e.Ignore(rpc.TradeProposalIgnoreParams{Key: "theta_hygiene:def"})
	if e.isIgnored(paper, "theta_hygiene:def") || e.isIgnored(live, "theta_hygiene:def") {
		t.Fatal("unscoped ignore leaked into a concrete session")
	}
}

func TestProposalRevisionChangesWithScope(t *testing.T) {
	t.Parallel()
	policy := rpc.Fingerprint{Version: rpc.ProtectionPolicyFingerprintVersion, Key: "sha256:policy"}
	sources := rpc.TradeProposalSourceFingerprints{
		Account:   &rpc.Fingerprint{Version: rpc.AccountFingerprintVersion, Key: "sha256:account"},
		Positions: &rpc.Fingerprint{Version: rpc.PositionsFingerprintVersion, Key: "sha256:positions"},
	}
	proposals := []rpc.TradeProposal{{Key: "theta_hygiene:abc", Quantity: 1, PositionEffect: rpc.OrderPositionEffectClose}}
	paper := proposalRevision(policy, sources, brokerStateScope{Account: "DU7654321", Mode: rpc.AccountModePaper}, proposals)
	live := proposalRevision(policy, sources, brokerStateScope{Account: "U1234567", Mode: rpc.AccountModeLive}, proposals)
	if paper == live {
		t.Fatalf("revision identical across sessions with bucket-equal sources: %s", paper)
	}
	again := proposalRevision(policy, sources, brokerStateScope{Account: "du7654321", Mode: rpc.AccountModePaper}, proposals)
	if paper != again {
		t.Fatalf("revision not case-stable for the same scope: %s != %s", paper, again)
	}
}

// TestReplaceSnapshotDoesNotPersistShells pins the restart-survival rule:
// a transient error/unscoped shell installed by the startup refresh
// (which races the gateway connect) must not overwrite the persisted
// last-good snapshot. That overwrite made installProposalEngine warn
// "ignoring persisted snapshot without a concrete account/mode scope"
// on every start, so warm-start adoption never happened.
func TestReplaceSnapshotDoesNotPersistShells(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 11, 13, 39, 0, 0, time.UTC)
	store := testProposalStore(t)
	engine := &proposalEngine{
		store:   store,
		now:     func() time.Time { return now },
		ignored: map[string]struct{}{},
	}

	good := rpc.TradeProposalSnapshot{
		Kind:          rpc.TradeProposalSnapshotKind,
		SchemaVersion: rpc.TradeProposalSnapshotSchemaVersion,
		AsOf:          now,
		Revision:      "sha256:good",
		AccountID:     "U1234567",
		AccountMode:   rpc.AccountModeLive,
		Proposals:     []rpc.TradeProposal{},
	}
	if err := engine.installSnapshot(good, false); err != nil {
		t.Fatalf("install generated snapshot: %v", err)
	}

	shell := emptyProposalSnapshot(now.Add(time.Minute))
	shell.Blockers = []rpc.TradingBlocker{{Code: "account_unavailable", Message: "IBKR connection unavailable"}}
	if err := engine.installSnapshot(shell, false); err != nil {
		t.Fatalf("install transient shell: %v", err)
	}

	persisted, err := store.LoadCurrent()
	if err != nil {
		t.Fatalf("LoadCurrent: %v", err)
	}
	if persisted.Revision != "sha256:good" {
		t.Fatalf("persisted revision = %q, want the generated snapshot to survive the shell install", persisted.Revision)
	}
	if got := engine.Snapshot(false).Revision; got != "empty" {
		t.Fatalf("in-memory snapshot revision = %q, want the shell to keep serving in-memory", got)
	}
}

// TestProposalRefreshWaitBacksOffTransientFailures pins the Run-loop
// retry schedule: a clean refresh waits the full cadence, transient
// failures retry at 30s doubling up to proposalRefreshBackoffCap — NOT
// the cadence, so the 30s default does not retry a dead session twice a
// minute for the length of an outage. Without the
// quick retry, a daemon restart that races the gateway connect serves
// the "IBKR connection unavailable" blocker for a full cadence
// (observed 2026-06-11 in the SPA protection panel).
func TestProposalRefreshWaitBacksOffTransientFailures(t *testing.T) {
	t.Parallel()
	cadence := 30 * time.Second
	cases := []struct {
		failures int
		want     time.Duration
	}{
		{0, cadence},
		{1, 30 * time.Second},
		{2, time.Minute},
		{3, 2 * time.Minute},
		{4, 4 * time.Minute},
		{5, 8 * time.Minute},
		{6, proposalRefreshBackoffCap},   // 16m, capped at 15m
		{200, proposalRefreshBackoffCap}, // shift-overflow guard
	}
	for _, tc := range cases {
		if got := proposalRefreshWait(cadence, tc.failures); got != tc.want {
			t.Errorf("proposalRefreshWait(%v, %d) = %v, want %v", cadence, tc.failures, got, tc.want)
		}
	}
	// A cadence above the backoff cap keeps the cap-at-cadence behavior:
	// sustained failure retries never become less frequent than the healthy
	// schedule.
	slow := 30 * time.Minute
	if got := proposalRefreshWait(slow, 7); got != slow {
		t.Errorf("slow-cadence failure cap = %v, want %v", got, slow)
	}
	if got := proposalRefreshWait(slow, 5); got != 8*time.Minute {
		t.Errorf("slow-cadence mid-ladder = %v, want 8m", got)
	}
	fast := 10 * time.Second
	if got := proposalRefreshWait(fast, 0); got != fast {
		t.Errorf("sub-base healthy cadence = %v, want %v", got, fast)
	}
	if got := proposalRefreshWait(fast, 3); got != 2*time.Minute {
		t.Errorf("sub-base transient failure stays on retry ladder = %v, want 2m", got)
	}
}

func TestProposalRefreshTransientClassifiesBlockers(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"account_identity_unscoped", "account_unavailable", "positions_unavailable", "positions_pending", "proposal_scope_mismatch"} {
		snap := rpc.TradeProposalSnapshot{Blockers: []rpc.TradingBlocker{{Code: code}}}
		if !proposalRefreshTransient(snap) {
			t.Errorf("blocker %q should classify as transient", code)
		}
	}
	for _, snap := range []rpc.TradeProposalSnapshot{
		{},
		{Blockers: []rpc.TradingBlocker{{Code: "proposals_disabled"}}},
		{Blockers: []rpc.TradingBlocker{{Code: "policy_drift"}}},
	} {
		if proposalRefreshTransient(snap) {
			t.Errorf("snapshot with blockers %+v should not classify as transient", snap.Blockers)
		}
	}
}

// failedRefreshSnapshot mimics the preserve path's output: last-good
// proposals with the transient blocker merged in and as_of frozen at the
// original generation time.
func failedRefreshSnapshot(asOf time.Time, code string) rpc.TradeProposalSnapshot {
	snap := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, asOf)
	snap.Blockers = []rpc.TradingBlocker{{Code: code, Message: "fetch failed"}}
	return snap
}

// Not parallel: NewLogger redirects the global pkg/ibkr logger, so a
// concurrent test's library output could land in this buffer.
func TestNoteRefreshOutcomeWarnsAfterStreakAndLogsRecovery(t *testing.T) {
	start := time.Date(2026, 6, 11, 18, 23, 41, 0, time.UTC)
	now := start
	var buf bytes.Buffer
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, start)
	srv.logger = NewLogger(&buf, "info")
	e := newProposalScopeTestEngine(t, srv)
	e.now = func() time.Time { return now }
	good := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, start)

	// Failures below proposalRefreshWarnStreak stay quiet: startup
	// refreshes race the gateway connect by design.
	for range proposalRefreshWarnStreak - 1 {
		e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
		now = now.Add(time.Minute)
	}
	if got := buf.String(); strings.Contains(got, "refresh blocked") {
		t.Fatalf("warned before the streak threshold: %s", got)
	}

	// The threshold failure warns with the streak, blocker codes, and the
	// age of the snapshot still being served.
	e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
	if got := buf.String(); !strings.Contains(got, "refresh blocked 3 consecutive times over 2m0s") ||
		!strings.Contains(got, "codes: account_unavailable") ||
		!strings.Contains(got, "(2m0s old)") {
		t.Fatalf("threshold warn missing streak/codes/age: %s", got)
	}

	// Every further failed attempt warns again — Run's backoff paces these
	// at the escalation/cadence rate, so this is one line per escalation.
	now = now.Add(time.Minute)
	e.noteRefreshOutcome(failedRefreshSnapshot(start, "positions_unavailable"), nil)
	if got := buf.String(); strings.Count(got, "refresh blocked") != 2 ||
		!strings.Contains(got, "codes: positions_unavailable") {
		t.Fatalf("escalation warn missing: %s", got)
	}

	// Recovery closes the streak with one info line and resets the state.
	now = now.Add(time.Minute)
	e.noteRefreshOutcome(good, nil)
	if got := buf.String(); !strings.Contains(got, "refresh recovered after 4 blocked attempts over 4m0s") {
		t.Fatalf("recovery info missing: %s", got)
	}
	if h := e.RefreshHealth(); h.Streak != 0 || len(h.Codes) != 0 || !h.Since.IsZero() {
		t.Fatalf("streak not reset on recovery: %+v", h)
	}

	// A short blip that never crossed the threshold recovers silently.
	buf.Reset()
	e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
	e.noteRefreshOutcome(good, nil)
	if got := buf.String(); strings.Contains(got, "refresh blocked") || strings.Contains(got, "refresh recovered") {
		t.Fatalf("short blip should stay quiet: %s", got)
	}
}

func TestProposalBlockerCodesFlattensAndFallsBackToError(t *testing.T) {
	t.Parallel()
	snap := rpc.TradeProposalSnapshot{Blockers: []rpc.TradingBlocker{
		{Code: "account_unavailable"}, {Code: "account_unavailable"}, {Code: "wide_spread"}, {Code: ""},
	}}
	if got := strings.Join(proposalBlockerCodes(snap, nil), ","); got != "account_unavailable,wide_spread" {
		t.Fatalf("codes = %q, want deduped account_unavailable,wide_spread", got)
	}
	if got := proposalBlockerCodes(rpc.TradeProposalSnapshot{}, errors.New("dial tcp: refused")); len(got) != 1 || got[0] != "dial tcp: refused" {
		t.Fatalf("blocker-less failure should fall back to the error: %v", got)
	}
}

func TestProposalSubsystemHealthReportsRefreshStreak(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 6, 11, 18, 23, 41, 0, time.UTC)
	now := start
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, start)
	e := newProposalScopeTestEngine(t, srv)
	e.now = func() time.Time { return now }
	srv.tradeProposals = e
	e.replaceSnapshot(scopedTestSnapshot("DU7654321", rpc.AccountModePaper, start))

	find := func() (rpc.SubsystemHealth, bool) {
		for _, sub := range srv.subsystemHealth(true, nil) {
			if sub.Name == "proposals" {
				return sub, true
			}
		}
		return rpc.SubsystemHealth{}, false
	}

	sub, ok := find()
	if !ok || sub.Status != "ready" || sub.Message != "" {
		t.Fatalf("clean engine should report ready: %+v ok=%v", sub, ok)
	}

	for range proposalRefreshWarnStreak - 1 {
		e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
		now = now.Add(time.Minute)
	}
	if sub, _ := find(); sub.Status != "ready" {
		t.Fatalf("sub-threshold streak should stay ready: %+v", sub)
	}

	e.noteRefreshOutcome(failedRefreshSnapshot(start, "account_unavailable"), nil)
	sub, _ = find()
	if sub.Status != "degraded" {
		t.Fatalf("threshold streak should degrade: %+v", sub)
	}
	if !strings.Contains(sub.Message, "blocked 3 consecutive times") ||
		!strings.Contains(sub.Message, start.Format(time.RFC3339)) {
		t.Fatalf("degraded message missing streak/as_of: %q", sub.Message)
	}
	if sub.LastError != "account_unavailable" || !sub.LastErrorAt.Equal(start) {
		t.Fatalf("degraded row missing codes/since: %+v", sub)
	}

	e.noteRefreshOutcome(scopedTestSnapshot("DU7654321", rpc.AccountModePaper, now), nil)
	if sub, _ := find(); sub.Status != "ready" || sub.LastError != "" {
		t.Fatalf("recovered engine should report ready: %+v", sub)
	}

	srv.cfg.AutoTrade.ProposalsEnabled = new(false)
	if sub, _ := find(); sub.Status != "disabled" {
		t.Fatalf("disabled config should report disabled: %+v", sub)
	}

	srv.tradeProposals = nil
	if _, ok := find(); ok {
		t.Fatal("nil engine must not report a proposals subsystem")
	}
}

// TestGeneratePreservesOverCapProtectiveCandidateForPreviewGate keeps proposal
// generation advisory: it reports the full candidate quantity and notional,
// while the session-bound preview above remains the authority that applies the
// every-order cap. Generated never means submit eligible.
func TestGeneratePreservesOverCapProtectiveCandidateForPreviewGate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 6, 11, 14, 0, 0, 0, time.UTC)
	srv := newPlatformSettingsTestServer(t, config.Trading{Mode: config.TradingModePaper})
	srv.now = func() time.Time { return now }
	_, err := srv.handleSettingsUpdate(context.Background(), &rpc.Request{Params: []byte(`{"origin":"human-tty","trading":{"limits":{"max_notional":5000}}}`)})
	if orderWritesAvailable && err != nil {
		t.Fatalf("set runtime max_notional override: %v", err)
	}
	if !orderWritesAvailable && err == nil {
		t.Fatal("stable build accepted a trading.limits write, want read-only refusal")
	}

	engine := &proposalEngine{
		server:  srv,
		now:     srv.now,
		ignored: map[string]struct{}{},
	}
	policy := defaultProtectionPolicy()
	status := protectionPolicyStatus(policy, rpc.ProtectionPolicyStatusDefault, "test", "", now)
	pos := &rpc.PositionsResult{Stocks: []rpc.PositionView{{
		Symbol:     "AMD",
		SecType:    "STK",
		Quantity:   80,
		Mark:       100,
		Multiplier: 1,
		Currency:   "USD",
	}}}
	scope := brokerStateScope{Account: "DU1234567", Mode: rpc.AccountModePaper}
	props, _ := engine.generate(context.Background(), policy, status, nil, pos, rpc.TradeProposalSourceFingerprints{}, nil, scope, now)
	if len(props) != 1 {
		t.Fatalf("generated %d proposals, want 1: %+v", len(props), props)
	}
	p := props[0]
	if p.Notional != 8000 {
		t.Fatalf("proposal notional = %.2f, want mark-based 8000", p.Notional)
	}
	if p.State != rpc.TradeProposalStateGenerated || len(p.Blockers) != 0 {
		t.Fatalf("proposal = %+v, want an unmodified advisory candidate for the preview gate", p)
	}
}

func TestProposalPositionsUnprimedDecision(t *testing.T) {
	t.Parallel()
	flat := &rpc.PositionsResult{Stocks: []rpc.PositionView{}, Options: []rpc.PositionView{}}
	held := &rpc.PositionsResult{Stocks: []rpc.PositionView{{Symbol: "MSFT", Quantity: 10}}}
	receipt := ibkrlib.PortfolioStreamHealth{InitialCompletedAt: time.Now()}
	cases := []struct {
		name   string
		pos    *rpc.PositionsResult
		acct   *rpc.AccountResult
		health ibkrlib.PortfolioStreamHealth
		want   bool
	}{
		{"empty cache, no completion receipt", flat, &rpc.AccountResult{}, ibkrlib.PortfolioStreamHealth{}, true},
		{"empty cache, no receipt, absent summary flattened to zero", flat, &rpc.AccountResult{GrossPositionValue: 0}, ibkrlib.PortfolioStreamHealth{}, true},
		{"empty cache, no receipt, nil account", flat, nil, ibkrlib.PortfolioStreamHealth{}, true},
		{"empty cache, receipt but summary shows positions", flat, &rpc.AccountResult{GrossPositionValue: 120_000}, receipt, true},
		{"empty cache, receipt and genuinely flat summary", flat, &rpc.AccountResult{GrossPositionValue: 0}, receipt, false},
		{"empty cache, receipt, nil account", flat, nil, receipt, false},
		{"primed cache", held, &rpc.AccountResult{GrossPositionValue: 120_000}, ibkrlib.PortfolioStreamHealth{}, false},
		{"nil positions", nil, &rpc.AccountResult{GrossPositionValue: 120_000}, ibkrlib.PortfolioStreamHealth{}, false},
	}
	for _, tc := range cases {
		if got := proposalPositionsUnprimed(tc.pos, tc.acct, tc.health); got != tc.want {
			t.Errorf("%s: proposalPositionsUnprimed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestProposalCountsOmitsMixedCurrencyExcess(t *testing.T) {
	t.Parallel()
	mixed := []rpc.TradeProposal{
		{Bucket: rpc.TradeProposalBucketRiskReduction, RiskExcessNotional: 15_000, RiskExcessCurrency: "USD"},
		{Bucket: rpc.TradeProposalBucketRiskReduction, RiskExcessNotional: 9_000, RiskExcessCurrency: "EUR"},
	}
	counts := proposalCounts(mixed, "")
	if counts.RiskReduction != 2 {
		t.Fatalf("risk reduction count = %d, want 2", counts.RiskReduction)
	}
	if counts.RiskReductionExcessNotional != 0 || counts.RiskReductionExcessCurrency != "" {
		t.Fatalf("mixed-currency aggregate = %v %q, want omitted: a EUR+USD raw sum is not a number in any currency",
			counts.RiskReductionExcessNotional, counts.RiskReductionExcessCurrency)
	}
	single := proposalCounts(mixed[1:], "")
	if single.RiskReductionExcessNotional != 9_000 || single.RiskReductionExcessCurrency != "EUR" {
		t.Fatalf("single-currency aggregate = %v %q, want 9000 EUR", single.RiskReductionExcessNotional, single.RiskReductionExcessCurrency)
	}
}

// TestInstallSnapshotGatesJournalOnRevisionChange pins the fast-cadence
// journal-growth guard: revision-identical installs append no "generated"
// events and no outcome marks, while a date rollover still owes the new
// day's mark even when the revision is frozen across midnight.
func TestInstallSnapshotGatesJournalOnRevisionChange(t *testing.T) {
	t.Parallel()
	day1 := time.Date(2026, 6, 12, 14, 0, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{}, day1)
	srv.proposalOutcomes = &proposalOutcomeStore{Path: filepath.Join(t.TempDir(), "outcomes.jsonl")}
	e := newProposalScopeTestEngine(t, srv)

	countLines := func(path, needle string) int {
		raw, err := os.ReadFile(path)
		if err != nil {
			return 0
		}
		return strings.Count(string(raw), needle)
	}
	countProposalEvents := func(eventType string) int {
		e.store.mu.Lock()
		defer e.store.mu.Unlock()
		count := 0
		for _, event := range e.store.events {
			if event.Type == eventType {
				count++
			}
		}
		return count
	}

	snap := scopedTestSnapshot("DU7654321", rpc.AccountModePaper, day1)
	if err := e.installSnapshot(snap, false); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if got := countProposalEvents("generated"); got != 0 {
		t.Fatalf("first install retained %d generated events, want none", got)
	}
	if got := countLines(srv.proposalOutcomes.Path, `"marked"`); got != 1 {
		t.Fatalf("first install appended %d marks, want 1", got)
	}

	// Same revision, same day: a 2m-cadence re-derive must not append.
	snap.AsOf = day1.Add(2 * time.Minute)
	if err := e.installSnapshot(snap, false); err != nil {
		t.Fatalf("same-revision install: %v", err)
	}
	if got := countProposalEvents("generated"); got != 0 {
		t.Fatalf("revision-identical install retained events: %d, want none", got)
	}
	if got := countLines(srv.proposalOutcomes.Path, `"marked"`); got != 1 {
		t.Fatalf("revision-identical install appended marks: %d, want 1", got)
	}

	// Same revision across midnight: marks are daily, so the new date
	// passes; generated events stay gated.
	snap.AsOf = day1.Add(24 * time.Hour)
	if err := e.installSnapshot(snap, false); err != nil {
		t.Fatalf("date-rollover install: %v", err)
	}
	if got := countProposalEvents("generated"); got != 0 {
		t.Fatalf("date rollover retained generated events: %d, want none", got)
	}
	if got := countLines(srv.proposalOutcomes.Path, `"marked"`); got != 2 {
		t.Fatalf("date rollover appended %d marks total, want 2", got)
	}

	// A new revision appends again.
	snap.Revision = "sha256:test-2"
	snap.Proposals[0].Revision = snap.Revision
	if err := e.installSnapshot(snap, false); err != nil {
		t.Fatalf("new-revision install: %v", err)
	}
	if got := countProposalEvents("generated"); got != 0 {
		t.Fatalf("new revision retained %d generated events total, want none", got)
	}
}

// TestProposalEngineKickWakesRunAndResetsLadder pins the reconnect-kick
// contract: Kick is non-blocking (dropped when one is already pending,
// safe on a nil engine) and a kicked Run iteration refreshes immediately
// instead of waiting out the backed-off timer.
func TestProposalEngineKickWakesRunAndResetsLadder(t *testing.T) {
	t.Parallel()
	var nilEngine *proposalEngine
	nilEngine.Kick() // must not panic

	now := time.Date(2026, 6, 12, 10, 53, 0, 0, time.UTC)
	srv := newProposalScopeTestServer(t, discover.Endpoint{Host: "127.0.0.1", Port: 7496, Account: "All"}, now)
	e := newProposalScopeTestEngine(t, srv)
	e.cadence = time.Hour // Run must not advance on the timer in this test

	e.Kick()
	e.Kick() // buffered: second kick drops instead of blocking

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		e.Run(ctx)
		close(done)
	}()

	// First iteration refreshes unconditionally; the pending kick lets the
	// loop pass the hour-long wait for a second refresh. Each refresh on
	// this unscoped endpoint installs an account_identity_unscoped shell.
	deadline := time.After(5 * time.Second)
	for {
		if e.RefreshHealth().Streak >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("kicked Run never reached a second refresh: streak=%d", e.RefreshHealth().Streak)
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop on context cancel")
	}
}

func TestPreviewNotSubmitEligibleBlockersCarriesWhatIfCause(t *testing.T) {
	generic := previewNotSubmitEligibleBlockers(nil)[0]
	if generic.Code != "preview_not_submit_eligible" || !strings.Contains(generic.Message, "did not make this proposal submit-eligible") {
		t.Fatalf("nil preview blocker = %+v, want generic message with stable code", generic)
	}
	if got := previewNotSubmitEligibleBlockers(&rpc.OrderPreviewResult{})[0]; got.Message != generic.Message {
		t.Fatalf("empty WhatIf blocker message = %q, want generic %q", got.Message, generic.Message)
	}
	enriched := previewNotSubmitEligibleBlockers(&rpc.OrderPreviewResult{WhatIf: rpc.OrderWhatIfResult{
		Status:  "rejected",
		Message: "broker rejected WhatIf request with error code 110: The price does not conform to the minimum price variation for this contract.",
		Action:  "Adjust the draft and run order preview again before any submit attempt.",
	}})[0]
	if enriched.Code != "preview_not_submit_eligible" {
		t.Fatalf("enriched blocker code = %q, want stable preview_not_submit_eligible", enriched.Code)
	}
	if !strings.Contains(enriched.Message, "error code 110") {
		t.Fatalf("enriched blocker message = %q, want the broker WhatIf cause", enriched.Message)
	}
	if !strings.Contains(enriched.Action, "Adjust the draft") {
		t.Fatalf("enriched blocker action = %q, want the WhatIf action", enriched.Action)
	}
	long := previewNotSubmitEligibleBlockers(&rpc.OrderPreviewResult{WhatIf: rpc.OrderWhatIfResult{
		Message: strings.Repeat("x", 400),
	}})[0]
	if n := len([]rune(long.Message)); n != 200 {
		t.Fatalf("truncated blocker message length = %d runes, want 200", n)
	}
}
