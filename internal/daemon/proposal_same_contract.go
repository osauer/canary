package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// One actionable proposal per exact contract and side (owner report
// 2026-09-28: "the reduction of one call pops up twice, for different
// reasons but same action"). Several buckets can each ask to sell one
// contract: a loss exit and theta hygiene both close a near-expiry call deep
// in loss, and the budget governor and the issuer trim both cut one line.
// Approving both sent two orders, and together they sold the sum of both
// sizes.
//
// The merge keeps one row per contract and side and carries every reason on
// it (Covers). Its size is the largest single requirement, never the sum:
// each immediate reduction asks to sell at least its own quantity now, so the
// largest meets them all. The owner settled two exceptions on 2026-09-28:
//   - An immediate sale goes before a trailing stop, whatever the sizes. The
//     stop is conditional and would hide the trim; it re-proposes for what
//     is left once the sale fills or is cancelled.
//   - A pre-authorised row is never covered by a row that needs approval.
//     Canary still places the smaller automatic order, and whichever order
//     works first blocks the other.
//
// Working-order netting finishes the job. Once an order works at the broker
// for the contract and side, every other row for it blocks until that order
// fills or is cancelled, and the rows then recompute from the new position.

// Blocker codes of the merge and of the reduction netting. Option exits and
// stock trails keep their own netting codes.
const (
	coveredByProposalCode         = "covered_by_proposal"
	reductionOrderExistingCode    = "existing_reduction_order"
	reductionOrderUnknownCode     = "reduction_order_identity_unknown"
	reductionOrderUnavailableCode = "reduction_order_evidence_unavailable"
	reductionIntentExistingCode   = "existing_reduction_intent"
)

// Why a broker open-order inventory read failed; each caller maps these to
// its own blocker codes.
const (
	openOrderInventoryUnbound     = "unbound"
	openOrderInventoryUnavailable = "unavailable"
	openOrderInventoryChanged     = "changed"
)

// sameContractSide names one exact contract and order side: the merge groups
// by it, and a live-intent guard shares it. A positive contract id is the
// exact identity for any security type; without one, the identity fields
// proposalKey hashes stand in.
func sameContractSide(p rpc.TradeProposal) string {
	action := strings.ToUpper(strings.TrimSpace(p.Action))
	if action != rpc.OrderActionBuy && action != rpc.OrderActionSell {
		return ""
	}
	c := p.Contract
	if c.ConID > 0 {
		return "CONID:" + strconv.Itoa(c.ConID) + "|" + action
	}
	symbol := strings.ToUpper(strings.TrimSpace(nonEmptyString(c.Symbol, p.Symbol)))
	if symbol == "" {
		return ""
	}
	return strings.Join([]string{symbol, sameContractSecFamily(nonEmptyString(c.SecType, p.SecType)),
		strings.ToUpper(c.LocalSymbol), strings.ToUpper(c.TradingClass), c.Expiry, strings.ToUpper(c.Right),
		fmt.Sprintf("%.4f", c.Strike), strings.ToUpper(nonEmptyString(c.Currency, "USD")), action}, "|")
}

// sameContractSecFamily folds the position and wire spellings of one
// security type together: an ETF or a STOCK row trades as STK.
func sameContractSecFamily(secType string) string {
	switch s := strings.ToUpper(strings.TrimSpace(secType)); s {
	case "OPT", "OPTION":
		return "OPT"
	case "STK", "STOCK", "ETF":
		return "STK"
	default:
		return s
	}
}

// sameContractMergeable reports a row that takes part in the merge: an
// unblocked, non-shadow, single-contract order. Reviews, multi-leg units and
// blocked rows stay as they are.
func sameContractMergeable(p rpc.TradeProposal) bool {
	if p.State != rpc.TradeProposalStateGenerated || len(p.Blockers) > 0 || p.Shadow || p.Unit != nil || p.Quantity <= 0 {
		return false
	}
	if p.Bucket == rpc.TradeProposalBucketOptionExitReview || p.Bucket == rpc.TradeProposalBucketStrategyExit ||
		p.OptionExit != nil && p.OptionExit.Kind == "review" {
		return false
	}
	return sameContractSide(p) != ""
}

// sameContractStanding reports a standing conditional order (a trailing
// stop), as against an immediate sale.
func sameContractStanding(p rpc.TradeProposal) bool {
	return isTrailOrderType(p.OrderType)
}

// sameContractPrecedence breaks a tie between immediate rows of one size.
// Their orders are then identical, a DAY patient limit for the same
// quantity. The Rulebook's exits lead because they carry the exact-contract
// quote requirement and re-check their economics at preview; the issuer trim,
// the budget governor and theta hygiene follow.
func sameContractPrecedence(p rpc.TradeProposal) int {
	switch p.Bucket {
	case rpc.TradeProposalBucketOptionLossExit:
		return 0
	case rpc.TradeProposalBucketOptionExpiryClose:
		return 1
	case rpc.TradeProposalBucketTrailingStop:
		return 2
	case rpc.TradeProposalBucketRiskReduction:
		return 3
	case rpc.TradeProposalBucketBudgetReduction:
		return 4
	case rpc.TradeProposalBucketThetaHygiene:
		return 5
	default:
		return 6
	}
}

// compareSameContract orders rows for primacy: an immediate sale before a
// standing stop, then the larger quantity, then precedence, then key.
func compareSameContract(a, b rpc.TradeProposal) int {
	if sa, sb := sameContractStanding(a), sameContractStanding(b); sa != sb {
		if sa {
			return 1
		}
		return -1
	}
	if c := cmp.Compare(b.Quantity, a.Quantity); c != 0 {
		return c
	}
	if c := cmp.Compare(sameContractPrecedence(a), sameContractPrecedence(b)); c != 0 {
		return c
	}
	return strings.Compare(a.Key, b.Key)
}

// sameContractMeets reports whether primary's order meets row's
// requirement: an immediate sale meets any stop and any sale no larger than
// itself. A stop meets nothing, being conditional.
func sameContractMeets(primary, row rpc.TradeProposal) bool {
	if sameContractStanding(primary) {
		return false
	}
	return sameContractStanding(row) || primary.Quantity >= row.Quantity
}

func sameContractBest(proposals []rpc.TradeProposal, rows []int) int {
	if len(rows) == 0 {
		return -1
	}
	return slices.MinFunc(rows, func(a, b int) int { return compareSameContract(proposals[a], proposals[b]) })
}

// mergeSameContractProposals merges the rows in place. automatic reports the
// rows Canary places itself (pre-authorised under the active policy); nil
// means none.
func mergeSameContractProposals(proposals []rpc.TradeProposal, automatic func(rpc.TradeProposal) bool) {
	groups := map[string][]int{}
	var order []string
	for i, p := range proposals {
		if !sameContractMergeable(p) {
			continue
		}
		key := sameContractSide(p)
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	for _, key := range order {
		rows := groups[key]
		if len(rows) < 2 {
			continue
		}
		var auto, manual []int
		for _, i := range rows {
			if automatic != nil && automatic(proposals[i]) {
				auto = append(auto, i)
			} else {
				manual = append(manual, i)
			}
		}
		a := sameContractBest(proposals, auto)
		if a >= 0 {
			for _, i := range auto {
				if i != a {
					coverSameContract(&proposals[a], &proposals[i])
				}
			}
			manual = slices.DeleteFunc(manual, func(i int) bool {
				if !sameContractMeets(proposals[a], proposals[i]) {
					return false
				}
				coverSameContract(&proposals[a], &proposals[i])
				return true
			})
		}
		m := sameContractBest(proposals, manual)
		if m < 0 {
			continue
		}
		for _, i := range manual {
			if i != m {
				coverSameContract(&proposals[m], &proposals[i])
			}
		}
		if a >= 0 {
			// The pre-authorised row stays live beside the one that needs
			// approval; its reason shows here, and netting orders the two.
			proposals[m].Covers = append(proposals[m].Covers, sameContractCoverage(proposals[a], true))
		}
	}
}

func sameContractCoverage(p rpc.TradeProposal, automatic bool) rpc.TradeProposalCoverage {
	return rpc.TradeProposalCoverage{Bucket: p.Bucket, Key: p.Key, Quantity: p.Quantity, OrderType: p.OrderType, Reason: p.Reason, Automatic: automatic}
}

// coverSameContract records that primary stands for covered: covered's
// reason moves onto primary, and covered blocks, so it can be neither
// approved nor placed automatically while primary is proposed.
func coverSameContract(primary, covered *rpc.TradeProposal) {
	primary.Covers = append(primary.Covers, sameContractCoverage(*covered, false))
	covered.CoveredBy = primary.Key
	sale := fmt.Sprintf("%s %d", strings.ToLower(primary.Action), primary.Quantity)
	blocker := rpc.TradingBlocker{
		Code:    coveredByProposalCode,
		Message: fmt.Sprintf("covered by %s: %s asks to %s now, which meets this rule too", primary.Key, sameContractRule(*primary), sale),
		Action:  "Review the covering proposal. This row recomputes once its order fills or is cancelled.",
	}
	if sameContractStanding(*covered) && !sameContractStanding(*primary) {
		blocker.Message = fmt.Sprintf("covered by %s: %s asks to %s now; the sale goes first, and this trailing stop re-proposes for what is left once it fills or is cancelled", primary.Key, sameContractRule(*primary), sale)
	}
	covered.State = rpc.TradeProposalStateBlocked
	covered.Blockers = appendTradingBlockerOnce(covered.Blockers, blocker)
	if covered.OptionExit != nil {
		setOptionExitReadiness(covered, false)
	}
}

// sameContractRule names a row's rule in plain words for a blocker message.
func sameContractRule(p rpc.TradeProposal) string {
	switch p.Bucket {
	case rpc.TradeProposalBucketOptionLossExit:
		return "the loss exit"
	case rpc.TradeProposalBucketOptionExpiryClose:
		return "the expiry close"
	case rpc.TradeProposalBucketTrailingStop:
		if p.OptionExit != nil && p.OptionExit.Kind == risk.OptionExitActionProfitTake {
			return "the profit take"
		}
		return "the trailing stop"
	case rpc.TradeProposalBucketRiskReduction:
		return "the issuer trim"
	case rpc.TradeProposalBucketBudgetReduction:
		return "the budget governor"
	case rpc.TradeProposalBucketThetaHygiene:
		return "theta hygiene"
	default:
		return strings.ReplaceAll(p.Bucket, "_", " ")
	}
}

// preAuthorisedRow is the merge's test for rows Canary places itself: the
// one decorateAutomatic serves. It is nil while no active policy
// pre-authorises anything.
func (e *proposalEngine) preAuthorisedRow() func(rpc.TradeProposal) bool {
	policy, ok := e.automaticPolicy()
	if !ok || len(policy.Authority.PreAuthorised) == 0 {
		return nil
	}
	return func(p rpc.TradeProposal) bool { return policy.Authority.preAuthorised(automaticBucketFor(p)) }
}

// proposalIsReduction reports the rows that sell now without an exit's own
// netting: theta hygiene, the issuer trim and the budget governor.
func proposalIsReduction(p rpc.TradeProposal) bool {
	switch p.Bucket {
	case rpc.TradeProposalBucketThetaHygiene, rpc.TradeProposalBucketRiskReduction, rpc.TradeProposalBucketBudgetReduction:
		return p.Unit == nil
	default:
		return false
	}
}

// reductionOrderBlockers nets a reduction row against the broker's working
// orders the way option exits are netted: any same-side order still working
// for the exact contract, from any client and of any type, blocks the row
// until that order fills or is cancelled; the row then recomputes from the
// new position. A trailing stop counts: selling beside it would leave the
// stop larger than the position. Missing or stale inventory fails closed.
// forceCurrent reads the broker afresh (preview and submit); generation
// reuses the protection heartbeat's receipt.
func (e *proposalEngine) reductionOrderBlockers(ctx context.Context, p rpc.TradeProposal, forceCurrent bool) []rpc.TradingBlocker {
	block := func(code, message string) []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: code, Message: message, Action: "Refresh and retry once complete, current broker open-order evidence is available."}}
	}
	if p.Contract.ConID <= 0 {
		return block(reductionOrderUnavailableCode, "the row has no exact contract id, so an order already working for this contract cannot be ruled out")
	}
	snapshot, failure := e.brokerOpenOrderInventory(ctx, forceCurrent)
	if failure != "" {
		return block(reductionOrderUnavailableCode, "complete, current open-order inventory from every client is unavailable, so an order already working for this contract cannot be ruled out")
	}
	family := sameContractSecFamily(nonEmptyString(p.Contract.SecType, p.SecType))
	for _, order := range snapshot.Orders {
		remaining := optionExitSnapshotRemaining(order)
		if order.Type != ibkrlib.OrderLifecycleEventOpenOrder || order.WhatIf || !strings.EqualFold(order.Action, p.Action) ||
			sameContractSecFamily(order.SecType) != family || remaining <= 0 {
			continue
		}
		if order.ConID == p.Contract.ConID {
			return []rpc.TradingBlocker{{
				Code: reductionOrderExistingCode,
				Message: fmt.Sprintf("an order to %s %s of this contract is already working (%s); this row waits until it fills or is cancelled, then recomputes from the new position",
					strings.ToLower(p.Action), strconv.FormatFloat(remaining, 'f', -1, 64), nonEmptyString(strings.ToUpper(strings.TrimSpace(order.OrderType)), "order")),
				Action: "Keep the working order, or cancel it at the broker first; this row then recomputes.",
			}}
		}
		if order.ConID <= 0 && optionExitSnapshotContractCouldMatch(order, p.Contract) {
			return block(reductionOrderUnknownCode, "a working order may be for this contract but carries no exact contract id")
		}
	}
	return nil
}

// liveIntentFor reports an authorised intent that has not reached the broker
// yet (a queued authorisation) for one contract and side, and its proposal
// key. Nothing is queued before the queued-authorisation phase lands, so it
// reports none; that phase fills it in, and every row for the contract and
// side then blocks behind the intent.
func (e *proposalEngine) liveIntentFor(contractSide string) (string, bool) {
	return "", false
}

// liveIntentBlockers blocks a row while another row's authorised intent for
// the same contract and side waits to reach the broker.
func (e *proposalEngine) liveIntentBlockers(p rpc.TradeProposal) []rpc.TradingBlocker {
	side := sameContractSide(p)
	if side == "" {
		return nil
	}
	key, ok := e.liveIntentFor(side)
	if !ok || key == p.Key {
		return nil
	}
	return []rpc.TradingBlocker{{
		Code:    reductionIntentExistingCode,
		Message: fmt.Sprintf("an authorised order for this contract (%s) waits to be placed; this row waits until it is placed and has filled or been cancelled", key),
		Action:  "Keep the authorised order, or withdraw it first; this row then recomputes.",
	}}
}

// brokerOpenOrderInventory reads the complete all-client open-order
// inventory bound to the current broker session: a fresh read when
// forceCurrent (preview and submit), else the protection heartbeat's
// short-lived receipt. A failure names why; the caller maps it to its own
// blocker.
func (e *proposalEngine) brokerOpenOrderInventory(ctx context.Context, forceCurrent bool) (ibkrlib.OpenOrderSnapshot, string) {
	// The clock is read after the inventory, so a receipt taken during the
	// read is not mistaken for one from the future.
	usable := func(snapshot ibkrlib.OpenOrderSnapshot, err error) bool {
		now := e.clock().UTC()
		return err == nil && snapshot.Complete && !snapshot.AsOf.IsZero() && !snapshot.AsOf.After(now) && now.Sub(snapshot.AsOf.UTC()) <= protectionOrderSnapshotMaxAge
	}
	if e.openOrdersForTest != nil {
		snapshot, err := e.openOrdersForTest(ctx)
		if !usable(snapshot, err) {
			return ibkrlib.OpenOrderSnapshot{}, openOrderInventoryUnavailable
		}
		return snapshot, ""
	}
	binding := e.server.currentProtectionOrderSnapshotBinding()
	if binding.connector == nil || !brokerScopeConcrete(binding.scope) {
		return ibkrlib.OpenOrderSnapshot{}, openOrderInventoryUnbound
	}
	var snapshot ibkrlib.OpenOrderSnapshot
	var err error
	if forceCurrent {
		snapshot, err = e.server.snapshotOpenOrdersFrom(ctx, binding.connector)
	} else {
		snapshot, err = e.server.protectionSnapshotOpenOrders(ctx, binding)
	}
	if !usable(snapshot, err) {
		return ibkrlib.OpenOrderSnapshot{}, openOrderInventoryUnavailable
	}
	receipt := binding
	receipt.session = snapshot.Session
	receipt.generation = snapshot.Generation
	if e.server.orderSnapshotFn != nil && receipt.session == (ibkrlib.ConnectorSessionBinding{}) {
		receipt.session = binding.session
	}
	if !e.server.protectionOrderSnapshotBindingCurrent(receipt) {
		return ibkrlib.OpenOrderSnapshot{}, openOrderInventoryChanged
	}
	return snapshot, ""
}

// proposalDuplicateOrderIsCanaryReduction reports a working immediate sale
// Canary proposed for the same position and side. A trailing stop waits for
// it (owner decision 2026-09-28) and re-proposes for what is left once the
// sale fills or is cancelled, instead of standing oversized beside it.
func proposalDuplicateOrderIsCanaryReduction(v rpc.OrderView, p rpc.TradeProposal) bool {
	return v.Open && strings.EqualFold(v.Action, p.Action) && strings.EqualFold(v.Source, proposalOrderSource) &&
		!protectionCoverageOrderIsStopLike(v) && orderViewMatchesProposalContract(v, p) && orderViewRemainingQuantity(v) > 0
}

func proposalBlockWith(p *rpc.TradeProposal, blockers []rpc.TradingBlocker) {
	for _, b := range blockers {
		p.State = rpc.TradeProposalStateBlocked
		p.Blockers = appendTradingBlockerOnce(p.Blockers, b)
	}
}
