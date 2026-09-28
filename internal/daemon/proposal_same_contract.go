package daemon

import (
	"cmp"
	"context"
	"errors"
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
//   - A pre-authorised row Canary will still place is never covered by a
//     row that needs approval. Unless it already meets the larger
//     requirement, Canary places it beside the row the owner approves, and
//     whichever order works first blocks the other.
//
// Working-order netting finishes the job. Once an order works at the broker
// for the contract and side, the exits, trims, budget and theta rows for it
// block until that order fills or is cancelled, and then recompute from the
// new position. A stock trailing stop waits only for a sale Canary proposed.

// Blocker codes of the merge and of the reduction netting. Option exits and
// stock trails keep their own netting codes.
const (
	coveredByProposalCode         = "covered_by_proposal"
	reductionOrderExistingCode    = "existing_reduction_order"
	reductionOrderUnknownCode     = "reduction_order_identity_unknown"
	reductionOrderUnavailableCode = "reduction_order_evidence_unavailable"
	reductionIntentExistingCode   = "existing_reduction_intent"
)

// sameContractChannel is how the merge treats a row.
type sameContractChannel int

const (
	// sameContractManual rows wait for the owner's approval.
	sameContractManual sameContractChannel = iota
	// sameContractAutomatic rows Canary will still place itself: the bucket
	// is pre-authorised, the automatic store runs, and the row's record for
	// its revision is absent (the next cycle creates it), waiting or
	// submitting.
	sameContractAutomatic
	// sameContractExcluded rows are pre-authorised but will not be placed:
	// their record for this revision has ended (vetoed, failed, superseded or
	// submitted) or no automatic store runs. The owner's surfaces hide every
	// pre-authorised row from approval, so such a row neither covers another
	// row, which would strand it, nor is covered; it stays as generated.
	sameContractExcluded
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

// mergeSameContractProposals merges the rows in place. channel says which
// rows Canary places itself; nil treats every row as waiting for approval.
func mergeSameContractProposals(proposals []rpc.TradeProposal, channel func(rpc.TradeProposal) sameContractChannel) {
	if channel == nil {
		channel = func(rpc.TradeProposal) sameContractChannel { return sameContractManual }
	}
	channels := make([]sameContractChannel, len(proposals))
	groups := map[string][]int{}
	var order []string
	for i, p := range proposals {
		if !sameContractMergeable(p) {
			continue
		}
		if channels[i] = channel(p); channels[i] == sameContractExcluded {
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
			if channels[i] == sameContractAutomatic {
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

// sameContractChannels classifies rows for the merge with the same policy
// test decorateAutomatic serves, plus each row's automatic record for its
// revision, so the merge never relies on an automatic submission that has
// already ended.
func (e *proposalEngine) sameContractChannels() func(rpc.TradeProposal) sameContractChannel {
	policy, ok := e.automaticPolicy()
	if !ok || len(policy.Authority.PreAuthorised) == 0 {
		return func(rpc.TradeProposal) sameContractChannel { return sameContractManual }
	}
	attached := e.automatic.attached()
	return func(p rpc.TradeProposal) sameContractChannel {
		if !policy.Authority.preAuthorised(automaticBucketFor(p)) {
			return sameContractManual
		}
		if !attached {
			return sameContractExcluded
		}
		if rec, found := e.automatic.get(p.Key, p.Revision); found && !rec.waiting() && rec.State != rpc.TradeProposalAutomaticSubmitting {
			return sameContractExcluded
		}
		return sameContractAutomatic
	}
}

// mergeSameContract merges a generated set whose revision is assigned, so
// each pre-authorised row is judged by its own automatic record, and
// remembers which rows it treated as automatic.
func (e *proposalEngine) mergeSameContract(proposals []rpc.TradeProposal) {
	channel := e.sameContractChannels()
	automatic := map[string]bool{}
	for _, p := range proposals {
		if channel(p) == sameContractAutomatic {
			automatic[p.Key] = true
		}
	}
	mergeSameContractProposals(proposals, channel)
	e.mu.Lock()
	e.mergedAutomatic = automatic
	e.mu.Unlock()
}

// kickIfCoverageStale asks for an immediate refresh when a served row's
// automatic standing differs from the one the last merge used: the policy
// changed, or a record ended (vetoed, failed) or began since. Until then a
// row covered by an automatic row that will no longer be placed would stay
// covered for a whole cadence.
func (e *proposalEngine) kickIfCoverageStale(proposals []rpc.TradeProposal) {
	if e == nil || len(proposals) == 0 {
		return
	}
	channel := e.sameContractChannels()
	e.mu.Lock()
	merged := e.mergedAutomatic
	e.mu.Unlock()
	for _, p := range proposals {
		if (channel(p) == sameContractAutomatic) != merged[p.Key] {
			e.Kick()
			return
		}
	}
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
// for the exact contract (sameContractWorkingOrder), from any client and of
// any type, blocks the row until that order fills or is cancelled; the row
// then recomputes from the new position. A trailing stop counts: selling
// beside it would leave the stop larger than the position. Missing or stale
// inventory fails closed. forceCurrent reads the broker afresh (preview and
// submit); generation reuses the protection heartbeat's receipt.
func (e *proposalEngine) reductionOrderBlockers(ctx context.Context, p rpc.TradeProposal, forceCurrent bool) []rpc.TradingBlocker {
	block := func(code, message string) []rpc.TradingBlocker {
		return []rpc.TradingBlocker{{Code: code, Message: message, Action: "Refresh and retry once complete, current broker open-order evidence is available."}}
	}
	if p.Contract.ConID <= 0 {
		return block(reductionOrderUnavailableCode, "the row has no exact contract id, so an order already working for this contract cannot be ruled out")
	}
	snapshot, scope, err := e.server.brokerOpenOrderInventory(ctx, forceCurrent)
	if err != nil {
		return block(reductionOrderUnavailableCode, "complete, current open-order inventory from every client is unavailable, so an order already working for this contract cannot be ruled out")
	}
	order, found, unknown := sameContractWorkingOrder(snapshot, scope, p)
	switch {
	case found:
		return []rpc.TradingBlocker{{
			Code: reductionOrderExistingCode,
			Message: fmt.Sprintf("an order to %s %s of this contract is already working (%s); this row waits until it fills or is cancelled, then recomputes from the new position",
				strings.ToLower(p.Action), strconv.FormatFloat(optionExitSnapshotRemaining(order), 'f', -1, 64), nonEmptyString(strings.ToUpper(strings.TrimSpace(order.OrderType)), "order")),
			Action: "Keep the working order, or cancel it at the broker first; this row then recomputes.",
		}}
	case unknown:
		return block(reductionOrderUnknownCode, "a working order may be for this contract but carries no exact contract id")
	}
	return nil
}

// sameContractWorkingOrder finds an order a complete broker inventory shows
// working for p's exact contract and side: from any client and of any type,
// in the inventory's account (brokerOrderWorking: what-if, terminal and
// filled orders are not working). The contract id is compared before the
// security type, so an order listed without one still counts. unknown
// reports a working order without a contract id that could be p's contract.
func sameContractWorkingOrder(snapshot ibkrlib.OpenOrderSnapshot, scope brokerStateScope, p rpc.TradeProposal) (match ibkrlib.OrderLifecycleEvent, found, unknown bool) {
	family := sameContractSecFamily(nonEmptyString(p.Contract.SecType, p.SecType))
	for _, order := range snapshot.Orders {
		if !brokerOrderWorking(order) || !strings.EqualFold(order.Action, p.Action) {
			continue
		}
		if account := strings.TrimSpace(order.Account); account != "" && strings.TrimSpace(scope.Account) != "" && !strings.EqualFold(account, strings.TrimSpace(scope.Account)) {
			continue
		}
		if order.ConID > 0 {
			if order.ConID == p.Contract.ConID {
				return order, true, false
			}
			continue
		}
		if (strings.TrimSpace(order.SecType) == "" || sameContractSecFamily(order.SecType) == family) && optionExitSnapshotContractCouldMatch(order, p.Contract) {
			unknown = true
		}
	}
	return ibkrlib.OrderLifecycleEvent{}, false, unknown
}

// openOrderInventoryFailure names why brokerOpenOrderInventory failed:
// unbound (no concrete account session), changed (the session changed
// during the read) or unavailable.
func openOrderInventoryFailure(err error) string {
	switch {
	case errors.Is(err, errBrokerOpenOrderInventoryUnbound):
		return "unbound"
	case errors.Is(err, errBrokerOpenOrderInventoryChanged):
		return "changed"
	default:
		return "unavailable"
	}
}

// liveIntentFor reports an authorised intent that has not reached the broker
// yet for one contract and side, and its proposal key: a queued
// authorisation the owner armed, while it waits for its window, is held or is
// being sent (proposal_queue.go). Every other row for the contract and side
// blocks behind it; once its order works at the broker, working-order
// netting takes over.
func (e *proposalEngine) liveIntentFor(contractSide string) (string, bool) {
	return e.queuedLiveIntentFor(contractSide)
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
