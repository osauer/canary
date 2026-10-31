package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/mandate"
	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
)

// Options at the additions gate (owner decision 2026-10-23 18:22 CEST):
//
//   - An option buy adds risk within the premium limits: the premium at risk
//     on its name may reach at most option_name_act_pct of NLV with the order,
//     and the book's at most the premium budget's act level of the regime set
//     in force. Either breach refuses the order with the figure, the limit
//     and the name (mandate_premium).
//   - The mandate's additions gate (since 2026-10-27, spec TM-04.12, the
//     loss measure against the loss room, the gap caps and the sleeve
//     allocations; before, the volatility budget) keeps gating stock and ETF
//     additions. For an option buy its judgement, with the 1-day expected
//     shortfall and the beta-adjusted exposure, is computed and shown, and
//     carried on the preview as a warning, never a block; the premium limits
//     count protection since 2026-10-27 (the owner: "Count it").
//   - The holds that bound a loss rather than a volatility stay for every
//     addition: at the maximum loss (no room) nothing adds, and the owner
//     sleeve's drawdown stop keeps the sleeve reduce-only while engaged.
//   - The owner's one-shot override of mandate.vol_target_pct (canary policy
//     override, human-only, journaled, expiring under
//     override.max_duration_hours) lifts the additions gate and, since
//     2026-10-29 14:28 CET (owner: "I always need to be able to stay in
//     control"), the premium limits above for the owner's additions. What
//     still bounds risk meanwhile: the drawdown brake and the no-room hold at
//     the 30% maximum loss, the owner sleeve's stop, every order cap, the
//     device confirmation of every order and the override's expiry
//     (risk.MandateAdditionsOverrideControl).
//   - A put bought to protect shares the book holds is a hedge entry, never
//     an addition (protectivePut, owner 2026-10-29 14:28 CET).
//
// A short option opened to sell is no option addition here: its loss has no
// premium to bound it, so it stays on the volatility gate as before.

// mandateOptionAddition reports a BUY of an option: the addition the premium
// limits gate.
func mandateOptionAddition(draft rpc.OrderDraft) bool {
	return strings.EqualFold(strings.TrimSpace(draft.Action), rpc.OrderActionBuy) &&
		strings.EqualFold(strings.TrimSpace(draft.Contract.SecType), "OPT")
}

// mandateAdditionsOverride returns the active override of the volatility
// budget's additions gate for the selected account, the latest-expiring one
// when several are active, or nil.
func (s *Server) mandateAdditionsOverride(now time.Time) *rpc.MandateOverrideView {
	if s == nil || s.riskCapital == nil {
		return nil
	}
	var best *rpc.MandateOverrideView
	for _, o := range s.riskCapital.OverridesSnapshotForScope(s.currentBrokerStateScope()) {
		if !o.Active || o.Control != risk.MandateAdditionsOverrideControl || !now.Before(o.ExpiresAt) {
			continue
		}
		if best == nil || o.ExpiresAt.After(best.ExpiresAt) {
			best = &rpc.MandateOverrideView{ID: o.ID, Control: o.Control, Reason: o.Reason, GrantedAt: o.GrantedAt, ExpiresAt: o.ExpiresAt, GrantedFrom: o.GrantedFrom}
		}
	}
	return best
}

// mandateHardHolds are the holds every addition keeps whatever gates it: at
// the maximum loss no sleeve adds, and the owner sleeve's drawdown stop
// holds its sleeve while engaged. Nil when neither holds.
func mandateHardHolds(v *mandate.Verdict, sleeve, ccy, open string) *rpc.TradingBlocker {
	if v == nil {
		return nil
	}
	if v.Capital.Known && !(v.Capital.Room > 0) {
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: " + mandateStatusSentence(v, ccy) + "; at the maximum loss no sleeve adds." + open,
			Action: "Reduce or close instead; the book goes to its hedges at the maximum loss."}
	}
	if why := mandateOwnerStopHolds(v, sleeve, ccy); why != "" {
		return &rpc.TradingBlocker{Code: mandateBudgetCode, Message: "Mandate budget: " + why + "; this order adds risk." + open,
			Action: fmt.Sprintf("Reduce or close instead; the stop releases when the sleeve's drawdown is back under %g%%. `canary mandate` has the sleeve's drawdown and high-water mark.", v.OwnerStop.ReleasePct)}
	}
	return nil
}

// mandatePremiumGate judges an option buy by the premium limits on the
// exposure the verdict was measured with: the order's premium at its limit
// (contracts × multiplier × limit, in base currency) added to the name's and
// the book's premium at risk. Fails closed: no limit, no exchange rate, no
// NLV or an unmeasured premium refuses the order, never passes it.
func (s *Server) mandatePremiumGate(draft rpc.OrderDraft, result *rpc.MandateRiskResult, input *mandate.Input, ccy, open string) *rpc.TradingBlocker {
	refuse := func(msg, act string) *rpc.TradingBlocker {
		return &rpc.TradingBlocker{Code: rpc.MandatePremiumBlockerCode, Message: "Mandate premium: " + msg + open, Action: act}
	}
	const measure = "`canary mandate` has each name's premium at risk against its cap."
	sym := normSym(draft.Contract.Symbol)
	exposure := result.Exposure
	if exposure == nil {
		return refuse("this option buy cannot be measured: no current positions read carries the book's premium at risk.", "Retry once the positions read is current; "+measure)
	}
	// Holdings belong to the measured exposure, but owner limits and the
	// regime set are mutable authority. Re-read them at every gate, including
	// the final wire guard, without changing the cached exposure.
	policy := s.rulebookPolicy()
	stage, carried := s.rulebookRegimeStage(policy, s.nowUTC())
	nameCapPct := policy.OptionNameActPct
	_, bookCapPct, bookSet := policy.PremiumBudgetInForce(stage.Bucket, carried)
	if !finiteProtectionOptionPolicyValue(nameCapPct) || nameCapPct < 0 || !finiteProtectionOptionPolicyValue(bookCapPct) || bookCapPct < 0 {
		return refuse("the current premium limits cannot be measured.", "Restore a valid Rulebook policy before adding option premium.")
	}
	if exposure.NLVBase == nil || !(*exposure.NLVBase > 0) {
		return refuse("this option buy cannot be measured: "+nonEmptyString(exposure.NLVReason, "net liquidation value is unknown")+".", "Retry once the account has been read; "+measure)
	}
	if !(draft.LimitPrice > 0) {
		return refuse("this option buy cannot be measured: without a limit there is no premium to bound its loss.", "Buy at a limit; "+measure)
	}
	fx := 1.0
	if c := normCcy(draft.Contract.Currency); c != "" && c != normCcy(ccy) {
		rate, ok := input.FXNow[c]
		if !ok || !(rate > 0) {
			return refuse("this option buy cannot be measured: no "+ccy+"/"+c+" exchange rate for its premium.", "Retry once the rate has been read; "+measure)
		}
		fx = rate
	}
	mult := float64(draft.Contract.Multiplier)
	if mult <= 0 {
		mult = 100
	}
	premium := float64(draft.Quantity) * mult * draft.LimitPrice * fx
	pending, err := s.mandatePendingPremium(draft, input, exposure, ccy)
	if err != nil {
		return refuse("this option buy cannot be measured: "+err.Error()+".", "Refresh the orders and positions reading; "+measure)
	}
	nlv := *exposure.NLVBase
	nameNow := 0.0
	for _, row := range exposure.Names {
		if row.Underlying != sym {
			continue
		}
		if row.PremiumAtRiskBase == nil {
			return refuse("this option buy cannot be measured: the premium at risk in "+sym+" is unknown ("+strings.Join(row.Unknown, "; ")+").", "Retry once every "+sym+" leg carries a value or a price paid; "+measure)
		}
		nameNow = *row.PremiumAtRiskBase
	}
	nameNow += pending[sym]
	nameCap := nameCapPct / 100 * nlv
	if after := nameNow + premium; after > nameCap+mandateReductionEpsilon {
		return refuse(fmt.Sprintf("with this order the premium at risk in %s would be %s, %s of net liquidation %s, over the %s limit for one name (now %s).",
			sym, mandateMoney(after, ccy), mandatePremiumPct(after/nlv*100), mandateMoney(nlv, ccy), mandatePremiumPct(nameCapPct), mandatePremiumPct(nameNow/nlv*100)),
			fmt.Sprintf("Size it smaller: %s has room for %s more premium under the limit. %s", sym, mandateMoney(max(0, nameCap-nameNow), ccy), measure))
	}
	if exposure.Book.PremiumAtRiskBase == nil {
		return refuse("this option buy cannot be measured: the book's premium at risk is unknown ("+strings.Join(exposure.Book.Unknown, "; ")+").", "Retry once every long option carries a value or a price paid; "+measure)
	}
	bookNow := *exposure.Book.PremiumAtRiskBase
	for _, amount := range pending {
		bookNow += amount
	}
	bookCap := bookCapPct / 100 * nlv
	if after := bookNow + premium; after > bookCap+mandateReductionEpsilon {
		set := ""
		if bookSet != "" {
			set = " of the " + bookSet + " set"
		}
		return refuse(fmt.Sprintf("with this order the book's premium at risk would be %s, %s of net liquidation %s, over the %s premium budget%s (now %s).",
			mandateMoney(after, ccy), mandatePremiumPct(after/nlv*100), mandateMoney(nlv, ccy), mandatePremiumPct(bookCapPct), set, mandatePremiumPct(bookNow/nlv*100)),
			fmt.Sprintf("Size it smaller: the book has room for %s more premium under the budget, or sell premium first. %s", mandateMoney(max(0, bookCap-bookNow), ccy), measure))
	}
	return nil
}

// prepareMandatePremium warms the shared broker inventory before admission.
// The final physical-wire guard reads only that receipt and durable journal;
// it must never initiate broker I/O while holding the transport lock.
func (s *Server) prepareMandatePremium(ctx context.Context, draft rpc.OrderDraft, position rpc.OrderPositionImpact) (*protectionOrderSnapshotBinding, error) {
	if s == nil || s.riskPolicies == nil || s.riskCapital == nil || !mandateOptionAddition(draft) || isRiskReducing(position.Effect) || !s.riskPolicies.snapshot().policy.MandateActive() {
		return nil, nil
	}
	putCover := strings.EqualFold(draft.Contract.Right, "P") && !s.rulebookPolicy().IsHedgeSymbol(draft.Contract.Symbol)
	if !putCover && !s.riskAddingOrder(draft, position) {
		return nil, nil
	}
	snapshot, snapshotScope, err := s.brokerOpenOrderInventory(ctx, false)
	if err != nil {
		return nil, err
	}
	if !sameBrokerScope(snapshotScope, s.currentBrokerStateScope()) || !protectionOrderSnapshotUsable(snapshot, s.orderNow()) {
		return nil, fmt.Errorf("current broker purchase commitments are unavailable")
	}
	_, input, _, _ := s.currentMandateVerdict()
	if input == nil {
		return nil, fmt.Errorf("premium positions are unavailable")
	}
	before := s.reduceFillBinding()
	if _, ok := s.journalOrderViewsForInventory(); !ok {
		return nil, fmt.Errorf("premium fill evidence is unavailable")
	}
	// An external fill may disappear from open orders without entering our
	// journal, before updatePortfolio reflects it. Compare the complete option
	// book and this put's shares with a request-correlated positions receipt;
	// checking only contracts with observed fill callbacks misses that race.
	modeled := map[int]reduceHeldPosition{}
	for _, leg := range input.Legs {
		if leg.Kind != mandate.KindOption && !(putCover && leg.Kind == mandate.KindStock && normSym(leg.Underlying) == normSym(draft.Contract.Symbol)) {
			continue
		}
		id := mandateLegConID(leg.ID)
		if id <= 0 || math.IsNaN(leg.Quantity) || math.IsInf(leg.Quantity, 0) {
			return nil, fmt.Errorf("premium or protection position identity is unknown")
		}
		sec := "STK"
		if leg.Kind == mandate.KindOption {
			sec = "OPT"
		}
		row := modeled[id]
		contract := rpc.ContractParams{ConID: id, Symbol: leg.Underlying, SecType: sec, Currency: leg.Currency}
		if row.contract.ConID != 0 && row.contract != contract {
			return nil, fmt.Errorf("premium or protection position identity conflicts")
		}
		row.contract = contract
		row.quantity += leg.Quantity
		modeled[id] = row
	}
	receipt, err := s.orderFillPositionsReceipt(ctx, before, 0)
	if err != nil {
		return nil, err
	}
	received := map[int]float64{}
	for _, row := range receipt {
		if row == nil {
			continue
		}
		if strings.TrimSpace(row.Account) == "" && row.Position != 0 {
			return nil, fmt.Errorf("premium positions receipt has unknown account scope")
		}
		if !strings.EqualFold(row.Account, before.scope.Account) {
			continue
		}
		c := row.Contract
		sec := strings.ToUpper(strings.TrimSpace(c.SecType))
		if sec == rpc.SecTypeOption {
			sec = "OPT"
		} else if sec == rpc.SecTypeStock {
			sec = "STK"
		}
		switch sec {
		case "OPT", "STK", "ETF", "FUT", rpc.SecTypeFuture, "FOP", "CASH", "BOND", "BILL", "IND", "CFD":
		default:
			if row.Position != 0 {
				return nil, fmt.Errorf("premium positions receipt has unknown security identity")
			}
		}
		prior, known := modeled[c.ConID]
		if !known && sec != "OPT" && !(putCover && normSym(c.Symbol) == normSym(draft.Contract.Symbol) && (sec == "STK" || sec == "ETF")) {
			continue
		}
		if c.ConID <= 0 || math.IsNaN(row.Position) || math.IsInf(row.Position, 0) {
			return nil, fmt.Errorf("premium positions receipt has unknown identity or quantity")
		}
		if _, duplicate := received[c.ConID]; duplicate {
			return nil, fmt.Errorf("premium positions receipt has duplicate contracts")
		}
		if known && (!riskSecTypeConsistent(prior.contract.SecType, sec) || normSym(prior.contract.Symbol) != normSym(c.Symbol) || normCcy(prior.contract.Currency) != normCcy(c.Currency)) {
			return nil, fmt.Errorf("premium positions receipt has conflicting contract identity")
		}
		received[c.ConID] = row.Position
		if math.Abs(prior.quantity-row.Position) > 1e-9 {
			return nil, fmt.Errorf("premium or protection holdings have not caught up with broker fills; wait for positions to agree")
		}
	}
	for id, row := range modeled {
		if math.Abs(received[id]-row.quantity) > 1e-9 {
			return nil, fmt.Errorf("premium or protection holdings have not caught up with broker fills; wait for positions to agree")
		}
	}
	return &before, nil
}

// mandatePendingPremium joins all-client working orders to local durable
// intents. An absent broker echo cannot erase an uncertain send. Matched rows
// count once, only unfilled units count, and the authenticated draft reference
// excludes the order being replaced (or the current staged send itself).
func (s *Server) mandatePendingPremium(draft rpc.OrderDraft, input *mandate.Input, exposure *rpc.MandateExposureView, base string) (map[string]float64, error) {
	scope := s.currentBrokerStateScope()
	snapshot, journal, events, err := s.currentMandateCommitments()
	if err != nil {
		return nil, err
	}
	fills := mandatePremiumFillClock(events)
	out := map[string]float64{}
	add := func(view rpc.OrderView, remaining float64, legs map[int]rpc.StrategyOrderLeg, filledAt time.Time) error {
		if strings.EqualFold(view.SecType, "BAG") {
			if len(legs) == 0 {
				return fmt.Errorf("a working combination has unknown option premium commitments")
			}
			for _, leg := range legs {
				if strings.EqualFold(leg.Contract.SecType, "OPT") && strings.EqualFold(leg.Action, rpc.OrderActionBuy) {
					return fmt.Errorf("a working combination's net limit cannot bound its bought option premiums")
				}
			}
			return nil
		}
		if !strings.EqualFold(view.SecType, "OPT") || !strings.EqualFold(view.Action, rpc.OrderActionBuy) {
			return nil
		}
		// A fill after the exposure reading may not yet be in held premium. Do
		// not guess whether it is included or count those units twice.
		if view.Filled > 0 && (filledAt.IsZero() || filledAt.After(exposure.AsOf)) {
			return fmt.Errorf("an option purchase filled after the premium reading")
		}
		if view.OrderRef != "" && view.OrderRef == draft.OrderRef {
			return nil
		}
		orderType := strings.ToUpper(strings.TrimSpace(view.OrderType))
		if (orderType != "LMT" && orderType != "STP LMT") || !positiveFinite(remaining) || !positiveFinite(view.LimitPrice) || view.Multiplier <= 0 || normSym(view.Symbol) == "" || normCcy(view.Currency) == "" {
			return fmt.Errorf("a working option purchase has no bounded premium")
		}
		fx := 1.0
		if normCcy(view.Currency) != normCcy(base) {
			fx = input.FXNow[normCcy(view.Currency)]
		}
		amount := remaining * float64(view.Multiplier) * view.LimitPrice * fx
		if !positiveFinite(fx) || !positiveFinite(amount) || !positiveFinite(out[normSym(view.Symbol)]+amount) {
			return fmt.Errorf("a working option purchase has no finite premium in %s", base)
		}
		out[normSym(view.Symbol)] += amount
		return nil
	}
	seen := map[int]bool{}
	for _, row := range brokerWorkingOrders(snapshot, journal.views, scope) {
		view := untrackedOrderView(row.Order, scope, snapshot.AsOf)
		filledAt := snapshot.AsOf
		if row.Journal != nil {
			fill := fills[orderViewKey(*row.Journal)]
			if fill.quantity >= view.Filled {
				filledAt = fill.at
			}
			view.OrderRef = row.Journal.OrderRef
			view.UpdatedAt = row.Journal.UpdatedAt
			if strings.EqualFold(view.SecType, "OPT") && strings.EqualFold(view.Action, rpc.OrderActionBuy) && view.OrderRef != draft.OrderRef && (view.Quantity != row.Journal.Quantity || view.LimitPrice != row.Journal.LimitPrice || view.OrderType != row.Journal.OrderType) {
				return nil, fmt.Errorf("a modified option purchase has not been reconciled with broker terms")
			}
		}
		if err := add(view, brokerOrderRemaining(row.Order), journal.legsOf(row.Journal), filledAt); err != nil {
			return nil, err
		}
		for i := range journal.views {
			if row.Journal == &journal.views[i] {
				seen[i] = true
			}
		}
	}
	for i, view := range journal.views {
		if !orderViewMatchesBrokerScope(view, scope) || seen[i] {
			continue
		}
		// Even a terminal fill must wait for the positions measurement to catch
		// up; otherwise a fast fill releases its reservation before becoming held.
		filledAt := fills[orderViewKey(view)].at
		if strings.EqualFold(view.SecType, "OPT") && strings.EqualFold(view.Action, rpc.OrderActionBuy) && view.Filled > 0 && (filledAt.IsZero() || filledAt.After(exposure.AsOf)) {
			return nil, fmt.Errorf("an option purchase filled after the premium reading")
		}
		if !view.Open {
			continue
		}
		remaining := view.Remaining
		if remaining <= 0 {
			remaining = view.Quantity - view.Filled
		}
		if err := add(view, remaining, journal.legsOf(&view), filledAt); err != nil {
			return nil, err
		}
	}
	return out, nil
}

type mandatePremiumFill struct {
	quantity float64
	at       time.Time
}

// A modification, cancel request, or repeated broker echo can carry an old
// Filled total with a new event time. Only an increased cumulative fill moves
// this clock. Canonical journal aliases retain the exact account/order identity
// across callbacks and endpoint changes. Correlated position agreement and the
// first-byte lifecycle fence remain the proof that those fills are reflected.
func mandatePremiumFillClock(events []orderJournalEvent) map[string]mandatePremiumFill {
	aliases := orderJournalKeyAliases(events)
	clocks := map[string]mandatePremiumFill{}
	for _, event := range events {
		key := orderJournalCanonicalKey(event, aliases)
		if key == "" {
			continue
		}
		prior := clocks[key]
		if event.Filled > prior.quantity {
			prior.quantity = event.Filled
			if event.At.IsZero() {
				// An undated new fill cannot borrow an older fill's date.
				prior.at = time.Time{}
			} else if event.At.After(prior.at) {
				prior.at = event.At
			}
			clocks[key] = prior
		}
	}
	return clocks
}

// mandateOptionsGateView reads the premium limits for an option buy now:
// refused while the book's premium at risk is at or over its cap or cannot
// be measured, else open, with the volatility gate's refusal as a warning.
func mandateOptionsGateView(res *rpc.MandateRiskResult, vol *rpc.MandateGateView) *rpc.MandateOptionsGateView {
	view := &rpc.MandateOptionsGateView{}
	if vol != nil && vol.RefusesAdditions {
		view.Warning = vol.Reason
	}
	e := res.Exposure
	switch {
	case e == nil:
		view.RefusesAdditions, view.Reason = true, "no current positions read carries the book's premium at risk"
	case e.NLVBase == nil:
		view.RefusesAdditions, view.Reason = true, nonEmptyString(e.NLVReason, "net liquidation value is unknown")
	case e.Book.PremiumAtRiskBase == nil:
		view.RefusesAdditions, view.Reason = true, "the book's premium at risk is unknown: "+strings.Join(e.Book.Unknown, "; ")
	case e.Book.PremiumOver:
		view.RefusesAdditions = true
		view.Reason = fmt.Sprintf("the book's premium at risk %s is %s of net liquidation, at the %s premium budget", mandateMoney(*e.Book.PremiumAtRiskBase, res.Verdict.BaseCurrency), mandatePremiumPct(policyCheckDeref(e.Book.PremiumPctNLV)), mandatePremiumPct(e.BookPremiumCapPct))
	}
	return view
}

// mandateOptionWarning carries the volatility gate's refusal on an option
// buy that the premium limits pass.
func mandateOptionWarning(vol *rpc.TradingBlocker) *rpc.DataWarning {
	return &rpc.DataWarning{Code: mandateBudgetCode, Scope: "risk_policy", Severity: "warning",
		Message: strings.TrimSuffix(vol.Message, " Reductions, closes, cancels and hedges stay open.") + " This option buy is judged by the premium limits instead; the volatility budget is its warning.",
		Action:  vol.Action}
}

// mandateOverrideSentence names an active override on the gate view.
func mandateOverrideSentence(o *rpc.MandateOverrideView, v *mandate.Verdict, ccy string) string {
	return fmt.Sprintf("your override of %s lifts the volatility budget and the premium limits until %s (%q); the verdict reads: %s", o.Control, o.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC"), o.Reason, mandateStatusSentence(v, ccy))
}

// mandateOverrideWarning carries an active override on a preview the
// volatility gate or the premium limits would otherwise have refused or
// judged.
func mandateOverrideWarning(o *rpc.MandateOverrideView, v *mandate.Verdict, ccy string) *rpc.DataWarning {
	return &rpc.DataWarning{Code: mandateBudgetCode, Scope: "risk_policy", Severity: "warning",
		Message: "Mandate budget: " + mandateOverrideSentence(o, v, ccy) + ". The maximum loss, the owner sleeve's stop, the order caps and the device confirmation still bind.",
		Action:  "Let the override expire, or revoke it by a policy revision; `canary mandate` has the measurement."}
}
