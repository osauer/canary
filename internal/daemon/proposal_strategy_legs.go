package daemon

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v3/internal/risk"
	"github.com/osauer/canary/v3/internal/rpc"
)

// A held strategy trades as whole units. A rule that measures one contract
// (theta hygiene, the issuer trim, the protection trim, the budget governor,
// an exit) knows nothing of the legs that contract is held against: selling a
// spread's long put alone leaves its short put uncovered. So no generated row
// is a sendable single-contract order for a leg of a strategy. Two mechanisms
// hold that, in this file:
//
//   - holdStrategyLegs is the last word on every generated book. It blocks
//     any single-contract option row whose contract is a leg of a strategy or
//     sits on an underlying whose legs cannot be grouped, whatever bucket
//     raised it, so a new rule cannot forget the check.
//   - A rule whose cut translates into whole units proposes the units
//     instead (strategyUnitRow): the protection trim does.
//
// The scope is optionExitStrategyScope's, the one the exits, the budget
// governor and the forced reduction already use: a pair of long options the
// operator declared independent exits for, contract by contract, keeps its
// per-leg rows.

const strategyWorkflowRequiredCode = "strategy_workflow_required"

// strategyLegBlocker names the route a held leg's cut takes instead: the
// strategy's own close or reduction where Canary routes it as one combo, the
// broker otherwise.
func strategyLegBlocker(pos *rpc.PositionsResult, conID int) rpc.TradingBlocker {
	b := rpc.TradingBlocker{Code: strategyWorkflowRequiredCode,
		Message: "this contract is one leg of a held multi-leg position, which trades as whole units; a single-leg proposal order cannot carry it",
		Action:  "Close the legs as one combo order at the broker, never leaving a short leg uncovered."}
	if s, ok := routableStrategyOfLeg(pos, conID); ok {
		b.Action = fmt.Sprintf("Reduce or close the strategy as one order: canary strategies reduce %s %d --units N, or canary strategies close %s %d.", s.ID, s.Revision, s.ID, s.Revision)
	}
	return b
}

// routableStrategyOfLeg finds the current, actionable strategy that holds the
// contract, with every leg matching the book at its ratio. A strategy whose
// legs drifted is no route: its ratios describe a position no longer held.
func routableStrategyOfLeg(pos *rpc.PositionsResult, conID int) (rpc.PositionStrategy, bool) {
	if pos == nil || conID <= 0 {
		return rpc.PositionStrategy{}, false
	}
	held := map[int]float64{}
	for _, row := range pos.Options {
		if row.ConID > 0 {
			held[row.ConID] = row.Quantity
		}
	}
	for _, s := range pos.Strategies {
		if s.Status != rpc.PositionStrategyStatusCurrent || !s.Actionable || s.Units <= 0 || len(s.Legs) < 2 {
			continue
		}
		member, matches := false, true
		for _, leg := range s.Legs {
			member = member || leg.Contract.ConID == conID
			if leg.Ratio == 0 || math.Abs(held[leg.Contract.ConID]-float64(leg.Ratio*s.Units)) > 1e-9 {
				matches = false
			}
		}
		if member {
			return s, matches
		}
	}
	return rpc.PositionStrategy{}, false
}

// holdStrategyLegs blocks every single-contract option row for a leg of a
// held strategy or of an ungroupable underlying. A unit row is the route
// itself and passes.
func holdStrategyLegs(rows []rpc.TradeProposal, pos *rpc.PositionsResult, policy protectionPolicy, now time.Time) {
	legs, ambiguous := optionExitStrategyScope(pos, directionalOptionIntents(policy.Buckets.TrailingStop.Options), now)
	if len(legs) == 0 && len(ambiguous) == 0 {
		return
	}
	for i := range rows {
		p := &rows[i]
		c := p.Contract
		if p.Unit != nil || sameContractSecFamily(nonEmptyString(c.SecType, p.SecType)) != "OPT" {
			continue
		}
		if legs[c.ConID] || ambiguous[normSym(nonEmptyString(c.Symbol, p.Symbol))] {
			proposalBlockWith(p, []rpc.TradingBlocker{strategyLegBlocker(pos, c.ConID)})
		}
	}
}

// strategyUnitCut turns a cut of cut contracts of one leg into whole units of
// its strategy, held to the caps a forced cut is held to. Units are rounded
// down, so the order never trades more of the leg than the rule asked for.
// The money caps read the order's gross premium (every leg at its mark, as
// the strategy preview prices it), and the option contract cap binds the
// largest leg; the delta-reducing exemption is a single contract's test and
// does not lift it here. ok is false where no whole unit fits: the leg's row
// then stays, blocked by holdStrategyLegs.
func strategyUnitCut(pos *rpc.PositionsResult, s rpc.PositionStrategy, row rpc.PositionView, cut int, sizing mandateReductionInput, baseCcy string) (units int, waits string, ok bool) {
	rows := map[int]rpc.PositionView{}
	for _, held := range pos.Options {
		rows[held.ConID] = held
	}
	legRatio, maxRatio, gross := 0, 0, 0.0
	for _, leg := range s.Legs {
		held, found := rows[leg.Contract.ConID]
		if !found || held.Multiplier != row.Multiplier || !strings.EqualFold(strings.TrimSpace(held.Currency), strings.TrimSpace(row.Currency)) {
			return 0, "", false
		}
		ratio := absUnit(leg.Ratio)
		maxRatio = max(maxRatio, ratio)
		if leg.Contract.ConID == row.ConID {
			legRatio = leg.Ratio
		}
		mark := math.Abs(held.Mark)
		if mark <= 0 {
			mark = math.Abs(held.ValuationMark)
		}
		if mark <= 0 {
			return 0, "", false
		}
		gross += float64(ratio) * mark
	}
	if legRatio <= 0 || maxRatio <= 0 {
		return 0, "", false
	}
	wanted := min(cut/legRatio, s.Units)
	if wanted <= 0 {
		return 0, "", false
	}
	if sizing.MaxOptionContracts > 0 {
		if sizing.MaxOptionContracts = sizing.MaxOptionContracts / maxRatio; sizing.MaxOptionContracts <= 0 {
			return 0, "", false
		}
	}
	sizing.Exits = nil
	unit := row
	unit.Mark, unit.ValuationMark, unit.Quantity = gross, gross, float64(s.Units)
	units, waits = mandateCycleQuantity(&mandateCut{row: unit, full: s.Units, step: 1, units: wanted}, sizing, baseCcy)
	return units, waits, units > 0
}

// strategyUnitRow is a rule's cut proposed as whole units of the held
// strategy: one guaranteed combo that sells the long legs and buys back the
// short ones in the strategy's ratio. It is a unit row like the strategy
// exit's, so the single-leg proposal order path refuses it
// (unitProposalOrderBlockers) and its route is the strategy workflow.
func strategyUnitRow(policy protectionPolicy, status rpc.ProtectionPolicyStatus, sources rpc.TradeProposalSourceFingerprints, now time.Time, bucket string, pos *rpc.PositionsResult, s rpc.PositionStrategy, units int, reason string) rpc.TradeProposal {
	rows := map[int]rpc.PositionView{}
	for _, held := range pos.Options {
		rows[held.ConID] = held
	}
	currency, netDebit, gross, value := "", 0.0, 0.0, 0.0
	legs := make([]rpc.TradeProposalUnitLeg, 0, len(s.Legs))
	for _, leg := range s.Legs {
		held := rows[leg.Contract.ConID]
		currency = nonEmptyString(currency, strings.ToUpper(strings.TrimSpace(nonEmptyString(held.Currency, "USD"))))
		netDebit += float64(leg.Ratio) * held.AvgCost
		gross += float64(absUnit(leg.Ratio)) * math.Abs(held.Mark) * float64(max(held.Multiplier, 1))
		value += held.MarketValue
		legs = append(legs, rpc.TradeProposalUnitLeg{Contract: proposalContractFromPosition(held, "OPT"), Quantity: held.Quantity, Ratio: leg.Ratio, AvgCost: held.AvgCost, DTE: optionExitDTE(held, now)})
	}
	underlying := normSym(s.Underlying)
	contract := rpc.ContractParams{Symbol: underlying, SecType: "BAG", Exchange: "SMART", Currency: currency, LocalSymbol: s.ID}
	effect, route := rpc.OrderPositionEffectReduce, fmt.Sprintf("canary strategies reduce %s %d --units %d", s.ID, s.Revision, units)
	if units == s.Units {
		effect, route = rpc.OrderPositionEffectClose, fmt.Sprintf("canary strategies close %s %d", s.ID, s.Revision)
	}
	p := rpc.TradeProposal{Key: proposalKey(bucket, contract, rpc.OrderActionSell), State: rpc.TradeProposalStateGenerated, Bucket: bucket,
		Symbol: underlying, SecType: "BAG", Action: rpc.OrderActionSell, Quantity: units, MaxQuantity: s.Units, PositionQuantity: float64(s.Units),
		PositionEffect: effect, OrderType: rpc.OrderTypeLMT, TIF: rpc.OrderTIFDay, Contract: contract, Reason: reason,
		PolicyID: policy.PolicyID, PolicyVersion: policy.PolicyVersion, PolicyFingerprint: status.Fingerprint, SourceFingerprints: sources, CreatedAt: now,
		Notional: gross * float64(units), PositionMarketValue: value, PositionDayChangeCurrency: currency}
	p.Unit = &rpc.TradeProposalUnit{StrategyID: s.ID, StrategyRevision: s.Revision, Underlying: underlying, Kind: s.Kind, Name: s.Name, Source: s.Source, Units: s.Units,
		Legs: legs, NetDebitPerUnit: netDebit, CostPerShare: netDebit / 100, ComboRoute: true,
		Route: "Trade the units as one guaranteed combo: " + route + " [--limit PRICE] [--submit]. A single-leg proposal order cannot carry a unit."}
	return p
}

// unitSellsHedgePut reports a unit row whose sale sells a hedge-listed long
// put: protection, which only the owner sells.
func unitSellsHedgePut(pol risk.RulebookPolicy, p rpc.TradeProposal) bool {
	if p.Unit == nil || !strings.EqualFold(p.Action, rpc.OrderActionSell) {
		return false
	}
	for _, leg := range p.Unit.Legs {
		if leg.Ratio > 0 && strings.EqualFold(strings.TrimSpace(leg.Contract.Right), "P") && pol.IsHedgeSymbol(leg.Contract.Symbol) {
			return true
		}
	}
	return false
}
