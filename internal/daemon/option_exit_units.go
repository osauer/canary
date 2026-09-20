package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// A multi-leg unit is one virtual position: every current strategy, whatever
// its source, and every ambiguous underlying. Its exit rules read the unit's
// net premium paid against its net close value at fresh leg quotes, never one
// leg alone, so no leg of a spread is ever sold on its own and no leg needs an
// owner declaration. A unit closes through the strategy workflow or at the
// broker; a single-leg proposal order cannot carry it.

const (
	optionExitUnitExitManagement = "unit"
	optionExitUnitProfitTake     = "profit_take"
	optionExitUnitMethod         = "unit_net_close_value_vs_net_debit"
)

type optionExitUnitLeg struct {
	row   rpc.PositionView
	ratio int
}

type optionExitUnit struct {
	id         string
	revision   int64
	underlying string
	kind       string
	source     string
	units      int
	comboRoute bool
	legs       []optionExitUnitLeg
}

// optionExitUnitsFromBook lists the units the exit engine evaluates. A current
// strategy whose legs are all resolved as independent exits is left to the
// per-leg rules; a strategy whose legs drifted from the book is skipped rather
// than evaluated on stale ratios. Legs of an ambiguous underlying not already
// in a strategy form one unit of their own.
func optionExitUnitsFromBook(pos *rpc.PositionsResult, grouped map[int]bool, ambiguous map[string]bool) []optionExitUnit {
	if pos == nil {
		return nil
	}
	rows := map[int]rpc.PositionView{}
	for _, row := range pos.Options {
		if row.ConID > 0 && row.Quantity != 0 && !math.IsNaN(row.Quantity) && !math.IsInf(row.Quantity, 0) {
			rows[row.ConID] = row
		}
	}
	var out []optionExitUnit
	covered := map[int]bool{}
	for _, strategy := range pos.Strategies {
		if strategy.Status != rpc.PositionStrategyStatusCurrent || len(strategy.Legs) < 2 {
			continue
		}
		unit := optionExitUnit{id: strategy.ID, revision: strategy.Revision, underlying: normSym(strategy.Underlying), kind: strategy.Kind, source: strategy.Source, units: strategy.Units, comboRoute: true}
		ok, inScope := true, false
		for _, leg := range strategy.Legs {
			row, found := rows[leg.Contract.ConID]
			if !found || covered[row.ConID] || leg.Ratio == 0 || (leg.Ratio > 0) != (row.Quantity > 0) ||
				unit.units <= 0 || math.Abs(row.Quantity-float64(leg.Ratio*unit.units)) > 1e-9 {
				ok = false
				break
			}
			inScope = inScope || grouped[row.ConID]
			unit.legs = append(unit.legs, optionExitUnitLeg{row: row, ratio: leg.Ratio})
		}
		if !ok || !inScope {
			continue
		}
		for _, leg := range unit.legs {
			covered[leg.row.ConID] = true
		}
		out = append(out, unit)
	}
	for underlying := range ambiguous {
		var legs []optionExitUnitLeg
		quantities := []int{}
		for _, row := range pos.Options {
			if normSym(row.Symbol) != normSym(underlying) || covered[row.ConID] {
				continue
			}
			row, found := rows[row.ConID]
			if !found || math.Abs(row.Quantity-math.Round(row.Quantity)) > 1e-9 {
				legs = nil
				break
			}
			legs = append(legs, optionExitUnitLeg{row: row})
			quantities = append(quantities, int(math.Round(row.Quantity)))
		}
		if len(legs) < 2 {
			continue
		}
		units := 0
		for _, quantity := range quantities {
			units = unitGCD(units, absUnit(quantity))
		}
		if units <= 0 {
			continue
		}
		slices.SortStableFunc(legs, func(a, b optionExitUnitLeg) int { return a.row.ConID - b.row.ConID })
		var identity strings.Builder
		identity.WriteString(normSym(underlying))
		for i := range legs {
			legs[i].ratio = int(math.Round(legs[i].row.Quantity)) / units
			fmt.Fprintf(&identity, "|%d", legs[i].row.ConID)
			covered[legs[i].row.ConID] = true
		}
		digest := sha256.Sum256([]byte(identity.String()))
		out = append(out, optionExitUnit{id: "unit-" + hex.EncodeToString(digest[:6]), underlying: normSym(underlying), kind: "multi_leg", source: "underlying", units: units, legs: legs})
	}
	slices.SortStableFunc(out, func(a, b optionExitUnit) int {
		if c := strings.Compare(a.underlying, b.underlying); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	return out
}

// optionExitUnitCovered names the legs a unit evaluates, so the per-leg rules
// skip them instead of raising a grouping question about each one.
func optionExitUnitCovered(units []optionExitUnit) map[int]bool {
	covered := map[int]bool{}
	for _, unit := range units {
		for _, leg := range unit.legs {
			covered[leg.row.ConID] = true
		}
	}
	return covered
}

// optionExitCallHedge reports a long call with something to cover: a short
// stock position in its own underlying, or, for an index call, a short stock
// anywhere in the book. A short elsewhere is unrelated exposure.
func optionExitCallHedge(row rpc.PositionView, pos *rpc.PositionsResult, pol risk.RulebookPolicy) bool {
	if pos == nil || !strings.EqualFold(strings.TrimSpace(row.Right), "C") {
		return false
	}
	for _, holding := range pos.Stocks {
		if holding.Quantity < 0 && (normSym(holding.Symbol) == normSym(row.Symbol) || pol.IsHedgeSymbol(row.Symbol)) {
			return true
		}
	}
	return false
}

// optionExitPutHedge reports whether a long put covers a long stock it can
// protect: a long stock of its own underlying, or, for a hedge-listed put, a
// long stock anywhere in the book. A long holding elsewhere is unrelated.
func optionExitPutHedge(row rpc.PositionView, pos *rpc.PositionsResult, pol risk.RulebookPolicy) bool {
	if pos == nil || !strings.EqualFold(strings.TrimSpace(row.Right), "P") {
		return false
	}
	for _, holding := range pos.Stocks {
		if holding.Quantity > 0 && (normSym(holding.Symbol) == normSym(row.Symbol) || pol.IsHedgeSymbol(row.Symbol)) {
			return true
		}
	}
	return false
}

// optionExitUnitHedge reports whether the unit is portfolio protection: a
// long hedge-listed put leg whose measured whole-book role is not directional,
// or a long call leg covering a short. Protection is never sold on a loss.
func (e *proposalEngine) optionExitUnitHedge(unit optionExitUnit, pos *rpc.PositionsResult, pol risk.RulebookPolicy, evidence optionExitBookEvidence) (bool, string) {
	role := risk.IndexPutRoleDirectional
	for _, leg := range unit.legs {
		if leg.ratio <= 0 {
			continue
		}
		row := leg.row
		if strings.EqualFold(strings.TrimSpace(row.Right), "P") && pol.IsHedgeSymbol(row.Symbol) {
			allowed, measured := optionExitEconomicRole(row, pol, optionExitEvidenceAt(evidence, e.clock()))
			if evidence.Closed && e.optionExitPreviouslyProtection(row.ConID) {
				allowed, measured = false, risk.IndexPutRoleProtection
			}
			if !allowed || measured != risk.IndexPutRoleDirectional {
				return true, measured
			}
		}
		if optionExitCallHedge(row, pos, pol) {
			return true, risk.IndexPutRoleProtection
		}
	}
	return false, role
}

// unitHighWater carries the profit trail's high-water mark across refreshes
// from the last published row of the same unit. A daemon restart starts it
// again from the current value; the record says so through its details.
func (e *proposalEngine) unitHighWater(key string) float64 {
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.snapshot.Proposals {
		if p.Key == key && p.Unit != nil && p.Unit.HighWaterPerShare != nil {
			return *p.Unit.HighWaterPerShare
		}
	}
	return 0
}

func optionExitUnitRoute(unit optionExitUnit) string {
	if unit.comboRoute {
		return fmt.Sprintf("Close the unit as one guaranteed combo: canary strategies close %s %d [--limit PRICE] [--submit]. A single-leg proposal order cannot carry a unit.", unit.id, unit.revision)
	}
	return "Canary has no combo route for this set of legs (no strategy record): close them at the broker as one combo order, never leaving a short leg uncovered. A single-leg proposal order cannot carry a unit."
}

// unitExitProposals evaluates every unit with the approved long-premium exit
// rules: the Rulebook loss line on net premium paid, and a profit trail Canary
// manages itself from the unit's high-water close value, since a broker trail
// cannot follow a combo.
func (e *proposalEngine) unitExitProposals(ctx context.Context, policy protectionPolicy, status rpc.ProtectionPolicyStatus, pos *rpc.PositionsResult, sources rpc.TradeProposalSourceFingerprints, marketEvents *rpc.MarketEventsResult, scope brokerStateScope, now time.Time, evidence optionExitBookEvidence, pol risk.RulebookPolicy, units []optionExitUnit, lossExitPct float64) []rpc.TradeProposal {
	cfg := policy.Buckets.TrailingStop.Options
	var out []rpc.TradeProposal
	for _, unit := range units {
		if hedge, _ := e.optionExitUnitHedge(unit, pos, pol, evidence); hedge {
			continue
		}
		type blocker struct{ code, message string }
		var blockers []blocker
		netDebit, currency := 0.0, ""
		for _, leg := range unit.legs {
			row := leg.row
			if row.Multiplier != 100 {
				blockers = append(blockers, blocker{"unit_standard_multiplier_required", "unit exits cover standard 100-multiplier legs only; an adjusted contract needs review"})
			}
			if row.AvgCost <= 0 || math.IsNaN(row.AvgCost) || math.IsInf(row.AvgCost, 0) {
				blockers = append(blockers, blocker{"option_cost_basis_unavailable", optionExitBlockerMessage("option_cost_basis_unavailable", cfg)})
			}
			netDebit += float64(leg.ratio) * row.AvgCost
			legCurrency := strings.ToUpper(strings.TrimSpace(nonEmptyString(row.Currency, "USD")))
			if currency == "" {
				currency = legCurrency
			} else if currency != legCurrency {
				blockers = append(blockers, blocker{"unit_single_currency_required", "unit legs do not share one currency"})
			}
		}
		if len(blockers) == 0 && netDebit <= 0 {
			blockers = append(blockers, blocker{"unit_long_premium_required", "the approved exit rules cover units that paid net premium; a net-credit or zero-cost unit needs its own review"})
		}
		costPerShare := netDebit / 100
		closeValue, openValue := 0.0, 0.0
		live, fresh, twoSided := true, true, true
		sessionOpen := optionSessionOpen(now)
		minDTE := math.MaxInt
		legs := make([]rpc.TradeProposalUnitLeg, 0, len(unit.legs))
		positionValue := 0.0
		for _, leg := range unit.legs {
			row := optionExitWithoutQuote(leg.row)
			if !evidence.Closed && len(blockers) == 0 {
				row = e.optionExitExactQuote(ctx, leg.row)
			}
			if row.SessionContext != nil {
				sessionOpen = row.SessionContext.IsOpen
			}
			live = live && rpc.IsLiveDataType(row.DataType)
			fresh = fresh && !row.Stale && !row.PriceAt.IsZero()
			dte := optionExitDTE(row, now)
			minDTE = min(minDTE, dte)
			positionValue += leg.row.MarketValue
			unitLeg := rpc.TradeProposalUnitLeg{Contract: proposalContractFromPosition(leg.row, "OPT"), Quantity: leg.row.Quantity, Ratio: leg.ratio, AvgCost: leg.row.AvgCost, DTE: dte}
			if row.OptionBid == nil || row.OptionAsk == nil || *row.OptionBid <= 0 || *row.OptionAsk <= 0 || *row.OptionAsk < *row.OptionBid {
				twoSided = false
			} else {
				unitLeg.Bid, unitLeg.Ask = cloneFloat64Ptr(row.OptionBid), cloneFloat64Ptr(row.OptionAsk)
				ratio := math.Abs(float64(leg.ratio))
				if leg.ratio > 0 {
					closeValue += ratio * *row.OptionBid
					openValue += ratio * *row.OptionAsk
				} else {
					closeValue -= ratio * *row.OptionAsk
					openValue -= ratio * *row.OptionBid
				}
			}
			legs = append(legs, unitLeg)
		}
		if len(blockers) == 0 {
			if !live {
				blockers = append(blockers, blocker{"live_option_quote_required", optionExitBlockerMessage("live_option_quote_required", cfg)})
			}
			if !fresh {
				blockers = append(blockers, blocker{"fresh_option_quote_required", optionExitBlockerMessage("fresh_option_quote_required", cfg)})
			}
			if !sessionOpen {
				blockers = append(blockers, blocker{"option_rth_closed", optionExitBlockerMessage("option_rth_closed", cfg)})
			}
			if !twoSided {
				blockers = append(blockers, blocker{"two_sided_option_quote_required", optionExitBlockerMessage("two_sided_option_quote_required", cfg)})
			}
			if minDTE < cfg.MinDTE {
				blockers = append(blockers, blocker{"option_exit_min_dte", optionExitBlockerMessage("option_exit_min_dte", cfg)})
			}
		}
		spread := openValue - closeValue
		if len(blockers) == 0 && twoSided {
			if mid := (openValue + closeValue) / 2; mid > 0 && spread/mid*100 > cfg.MaxSpreadPctOfMid {
				blockers = append(blockers, blocker{"option_spread_too_wide", optionExitBlockerMessage("option_spread_too_wide", cfg)})
			}
		}
		contract := rpc.ContractParams{Symbol: unit.underlying, SecType: "BAG", Exchange: "SMART", Currency: nonEmptyString(currency, "USD"), LocalSymbol: unit.id}
		key := proposalKey(rpc.TradeProposalBucketStrategyExit, contract, rpc.OrderActionSell)
		action, returnPct := "", 0.0
		var highWater, trailStop *float64
		var armedDetail string
		if len(blockers) == 0 {
			returnPct = (closeValue/costPerShare - 1) * 100
			// An armed trail stays armed: the high water carried from the last
			// published row keeps it live after the value retraces below the
			// arming line, exactly as a broker-held trail would.
			carried := e.unitHighWater(key)
			switch {
			case returnPct <= -lossExitPct:
				action = risk.OptionExitActionLoss
			case returnPct >= cfg.ProfitArmGainPct || carried > 0:
				hwm := math.Max(closeValue, carried)
				trail := math.Max(hwm*cfg.DefaultPct/100, math.Max(cfg.MinTrailAbs, cfg.SpreadMultiple*spread))
				stop := hwm - trail
				highWater, trailStop = cloneFloat64Ptr(&hwm), cloneFloat64Ptr(&stop)
				lockPct := (stop/costPerShare - 1) * 100
				if closeValue <= stop+1e-9 && lockPct+1e-9 >= cfg.LockedGainPct {
					action = optionExitUnitProfitTake
				} else {
					armedDetail = fmt.Sprintf("profit trail armed: unit gained %.1f%% versus net premium paid; Canary proposes the close when its net close value falls to %.2f %s per share (high water %.2f, trail %.2f) while keeping at least %.1f%% over cost", returnPct, stop, contract.Currency, hwm, trail, cfg.LockedGainPct)
				}
			}
		}
		reason := "unit exit needs review; fresh leg quotes or cost evidence are incomplete"
		switch action {
		case risk.OptionExitActionLoss:
			reason = fmt.Sprintf("net close value at fresh leg quotes is %.1f%% below net premium paid; Rulebook exits the whole unit at %.1f%% loss", math.Abs(returnPct), lossExitPct)
		case optionExitUnitProfitTake:
			reason = fmt.Sprintf("unit gained %.1f%% versus net premium paid and retraced from its high-water close value; profit trail closes the whole unit", returnPct)
		default:
			if armedDetail != "" {
				reason = "unit profit trail armed; no exit threshold reached"
			}
		}
		p := rpc.TradeProposal{Key: key, State: rpc.TradeProposalStateGenerated, Bucket: rpc.TradeProposalBucketStrategyExit, Symbol: unit.underlying, SecType: "BAG", Action: rpc.OrderActionSell, Quantity: unit.units, MaxQuantity: unit.units, PositionQuantity: float64(unit.units), PositionEffect: rpc.OrderPositionEffectClose, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: contract, Reason: reason, PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, SourceFingerprints: sources, CreatedAt: now,
			Score:                     math.Abs(positionValue),
			PositionMarketValue:       positionValue,
			PositionDayChangeCurrency: contract.Currency}
		if twoSided {
			p.Notional = math.Abs(closeValue) * float64(unit.units) * 100
		}
		p.OptionExit = &rpc.TradeProposalOptionExit{
			Kind: nonEmptyString(action, "review"), Intent: "directional", EconomicRole: risk.IndexPutRoleDirectional, ExitManagement: optionExitUnitExitManagement,
			DTE: minDTE, MinDTE: cfg.MinDTE, LossExitPct: lossExitPct, ProfitArmGainPct: cfg.ProfitArmGainPct,
			LockedGainPct: cfg.LockedGainPct, ProfitTrailPct: cfg.DefaultPct, MinTrailPct: cfg.MinPct, MaxTrailPct: cfg.MaxPct,
			MaxSpreadPctOfMid: cfg.MaxSpreadPctOfMid, MinTrailAbs: cfg.MinTrailAbs, SpreadMultiple: cfg.SpreadMultiple, Method: optionExitUnitMethod,
		}
		if minDTE == math.MaxInt {
			p.OptionExit.DTE = -1
		}
		if costPerShare > 0 {
			p.OptionExit.CostBasisPremium = cloneFloat64Ptr(&costPerShare)
		}
		p.Unit = &rpc.TradeProposalUnit{StrategyID: unit.id, StrategyRevision: unit.revision, Underlying: unit.underlying, Kind: unit.kind, Source: unit.source, Units: unit.units, Legs: legs, NetDebitPerUnit: netDebit, CostPerShare: costPerShare, HighWaterPerShare: highWater, ProfitTrailStop: trailStop, ComboRoute: unit.comboRoute, Route: optionExitUnitRoute(unit)}
		if !unit.comboRoute {
			p.Unit.StrategyID, p.Unit.StrategyRevision = "", 0
		}
		if twoSided {
			p.OptionExit.ReferencePrice = cloneFloat64Ptr(&closeValue)
			p.Unit.CloseValuePerShare = cloneFloat64Ptr(&closeValue)
			if action != "" {
				p.OptionExit.ReturnPct = cloneFloat64Ptr(&returnPct)
			}
		}
		p.Details = append(p.Details, optionExitUnitLegsDetail(unit, contract.Currency),
			fmt.Sprintf("net premium paid per unit %.2f %s (%.2f per share); the unit is evaluated as one position, no leg alone", netDebit, contract.Currency, costPerShare))
		if twoSided {
			p.Details = append(p.Details, fmt.Sprintf("net close value at fresh leg quotes %.2f %s per share (long legs at bid, short legs at ask); spread %.2f", closeValue, contract.Currency, spread))
		}
		if armedDetail != "" {
			p.Details = append(p.Details, armedDetail)
		}
		if action == risk.OptionExitActionLoss {
			p.Details = append(p.Details, fmt.Sprintf("rulebook_loss_exit=%.1f%% from net premium paid on fresh leg quotes", lossExitPct), "order=one DAY combo close at the net close value; may remain unfilled; no resting loss stop and no overnight loss guarantee")
		}
		if action == optionExitUnitProfitTake {
			p.Details = append(p.Details, fmt.Sprintf("profit_arm=+%.1f%% trail=%.1f%% of high water, locked_gain>=+%.1f%%; Canary manages this trail itself because a broker trail cannot follow a combo", cfg.ProfitArmGainPct, cfg.DefaultPct, cfg.LockedGainPct))
		}
		p.Details = append(p.Details, p.Unit.Route)
		for _, b := range blockers {
			optionExitBlock(&p, b.code, b.message)
		}
		if action == "" {
			if armedDetail != "" {
				optionExitBlock(&p, "unit_profit_trail_armed", armedDetail)
			} else if len(blockers) == 0 {
				// Eligible, no threshold reached: no row, like a standalone leg.
				continue
			} else {
				optionExitBlock(&p, "option_exit_measurement_unavailable", "unit exit evidence is incomplete; no threshold or order may be inferred")
			}
		}
		setOptionExitReadiness(&p, evidence.Closed)
		applyMarketEventFlagsToProposal(&p, marketEvents)
		if !e.isIgnored(scope, p.Key) {
			out = append(out, p)
		}
	}
	return out
}

func optionExitUnitLegsDetail(unit optionExitUnit, currency string) string {
	parts := make([]string, 0, len(unit.legs))
	for _, leg := range unit.legs {
		side := "long"
		if leg.ratio < 0 {
			side = "short"
		}
		parts = append(parts, fmt.Sprintf("%s %d × %s %s %g %s (cost %.2f %s)", side, absUnit(leg.ratio), normSym(leg.row.Symbol), strings.TrimSpace(leg.row.Expiry), leg.row.Strike, strings.ToUpper(strings.TrimSpace(leg.row.Right)), leg.row.AvgCost/100, currency))
	}
	return fmt.Sprintf("unit of %d × [%s]", unit.units, strings.Join(parts, "; "))
}

// unitProposalOrderBlockers refuses to route a unit through the single-leg
// proposal order path; the unit's own route is on the record.
func unitProposalOrderBlockers(p rpc.TradeProposal) []rpc.TradingBlocker {
	if p.Unit == nil && !strings.EqualFold(p.SecType, "BAG") {
		return nil
	}
	route := "close the unit through the strategy workflow"
	if p.Unit != nil && p.Unit.Route != "" {
		route = p.Unit.Route
	}
	return []rpc.TradingBlocker{{Code: "strategy_workflow_required", Message: "a multi-leg unit cannot be previewed or submitted as a single-leg proposal order", Action: route}}
}

func unitGCD(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func absUnit(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
