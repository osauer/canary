package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/risk"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

// reduceOrderSource tags the order journal so a discretionary percentage trim
const reduceOrderSource = "manual_reduce"

// A percent reduction is priced as a worst price (owner decision 2026-10-22
// 15:26 CEST, which replaces the ±0.5%/2% band around the reviewed mid of
// 2026-10-13): its limit sits the policy's allowance beyond the touch
// (risk-policy.toml [order_limits] reduce_allowance_pct, and
// reduce_option_allowance_pct with at least one tick), so it can fill at once
// in a fast market while the broker fills it at the best price available
// and Adaptive, where the order carries it, works toward the mid first. The
// limit the owner confirmed never moves. A tokened submit refuses only when
// the market is already beyond it (beyond_worst_price); a market that moved
// the other way is no reason to refuse.

// reduceAdaptiveNote is the terms' plain statement when a reduction asked for
// or would default to Adaptive on a contract the broker layer does not send
// it for; the order is then a plain limit at the mid.
const reduceAdaptiveNote = "Adaptive not supported for this contract"

// reduceDedupeTTL is how long a submit RequestRef is remembered so a
// double-tap or client retry replays the prior result instead of placing again.
const reduceDedupeTTL = 90 * time.Second

// reduceDedupeEntry is one remembered reduce submit, single or basket.
type reduceDedupeEntry struct {
	at     time.Time
	single *rpc.TradeProposalReduceResult
	basket *rpc.TradeProposalReducePortfolioResult
}

// isProtectiveShort reports whether a holding carries short (bearish) delta
// short stock. Used only by the single-position reduce path (IncludeHedges
// sign-matching, with no separate hedge flag. When the broker delta is known
// exclusion fails safe toward protecting the position rather than silently
func isProtectiveShort(row rpc.PositionView) bool {
	if row.Quantity == 0 {
		return false
	}
	if positionWireSecType(row.SecType) == "OPT" {
		if row.Delta != nil {
			return *row.Delta < 0
		}
		right := strings.ToUpper(strings.TrimSpace(row.Right))
		return (right == "P" && row.Quantity > 0) || (right == "C" && row.Quantity < 0)
	}
	if row.Delta != nil {
		return *row.Delta < 0
	}
	return row.Quantity < 0
}

// reduceEligible scopes the discretionary trim to stocks/ETFs (long or short)
func reduceEligible(row rpc.PositionView) bool {
	switch strings.ToUpper(strings.TrimSpace(row.SecType)) {
	case rpc.SecTypeOption, "OPT":
		return row.Quantity > 0
	case rpc.SecTypeStock, "STK", "ETF":
		return row.Quantity != 0
	default:
		return false
	}
}

// findReducePosition resolves the holding the trim acts on. ConID is the
// unambiguous key; a bare Symbol is accepted only when it matches exactly one
func findReducePosition(pos *rpc.PositionsResult, conID int, symbol string) (rpc.PositionView, []rpc.TradingBlocker) {
	if pos == nil {
		return rpc.PositionView{}, []rpc.TradingBlocker{{Code: "positions_unavailable", Message: "current positions are unavailable", Action: "Retry once the daemon has refreshed positions."}}
	}
	rows := make([]rpc.PositionView, 0, len(pos.Stocks)+len(pos.Options))
	rows = append(rows, pos.Stocks...)
	rows = append(rows, pos.Options...)
	if conID > 0 {
		for _, row := range rows {
			if row.ConID == conID {
				return row, nil
			}
		}
		return rpc.PositionView{}, []rpc.TradingBlocker{{Code: "position_not_found", Message: fmt.Sprintf("no held position matches con_id %d", conID), Action: "Refresh positions and choose a current holding."}}
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" {
		return rpc.PositionView{}, []rpc.TradingBlocker{{Code: "bad_request", Message: "reduce requires con_id or a stock symbol", Action: "Pass con_id (preferred) or a unique stock symbol."}}
	}
	var matches []rpc.PositionView
	for _, row := range pos.Stocks {
		if strings.EqualFold(strings.TrimSpace(row.Symbol), symbol) {
			matches = append(matches, row)
		}
	}
	switch len(matches) {
	case 0:
		return rpc.PositionView{}, []rpc.TradingBlocker{{Code: "position_not_found", Message: fmt.Sprintf("no held stock matches symbol %q", symbol), Action: "Pass con_id to target an option leg, or choose a current stock holding."}}
	case 1:
		return matches[0], nil
	default:
		return rpc.PositionView{}, []rpc.TradingBlocker{{Code: "ambiguous_symbol", Message: fmt.Sprintf("symbol %q matches %d stock positions", symbol, len(matches)), Action: "Pass con_id to disambiguate."}}
	}
}

// reduceQuantityForPercent floors percent×|position| to whole units so the trim
// never rounds up into a flip, and treats 100 as an exact close. It returns a
// blocker when the position is not reducible or the percentage rounds to zero.
func reduceQuantityForPercent(position float64, percent int) (int, []rpc.TradingBlocker) {
	posAbsInt, _ := closeReduceQuantity(position)
	unit := "shares"
	if posAbsInt == 1 {
		unit = "share"
	}
	if posAbsInt < 1 {
		return 0, []rpc.TradingBlocker{{Code: "position_not_reducible", Message: "position magnitude is below one whole unit", Action: "There is nothing whole to reduce."}}
	}
	if percent <= 0 || percent > 100 {
		return 0, []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("percent %d must be between 1 and 100", percent), Action: "Choose 25, 50, 75, or 100."}}
	}
	if percent >= 100 {
		return posAbsInt, nil
	}
	qty := int(math.Floor(float64(percent)/100.0*float64(posAbsInt) + 1e-9))
	if qty < 1 {
		return 0, []rpc.TradingBlocker{{Code: "percent_too_small", Message: fmt.Sprintf("reducing %d%% of %d %s rounds to zero units", percent, posAbsInt, unit), Action: "Choose a larger percentage, or 100% to close the position."}}
	}
	if qty > posAbsInt {
		qty = posAbsInt
	}
	return qty, nil
}

// reduceAdaptivePriority resolves the priority a reduce asks for. Empty means
// Normal (the reduction default); "none" asks for a plain limit; Normal,
// Patient and Urgent are IBKR's priorities. Anything else is refused rather
// than silently mapped to a default.
func reduceAdaptivePriority(requested string) (string, []rpc.TradingBlocker) {
	switch strings.ToLower(strings.TrimSpace(requested)) {
	case "":
		return "Normal", nil
	case "none", "off", "plain":
		return "", nil
	case "normal":
		return "Normal", nil
	case "patient":
		return "Patient", nil
	case "urgent":
		return "Urgent", nil
	default:
		return "", []rpc.TradingBlocker{{Code: "bad_request", Message: fmt.Sprintf("adaptive_priority %q is not Normal, Patient, Urgent or none", strings.TrimSpace(requested)), Action: "Choose Normal, Patient or Urgent, or none for a plain limit."}}
	}
}

// reduceAdaptiveSupported reports whether the reduce order for this contract
// can carry IBKR's Adaptive algorithm as Canary sends it: a US stock or ETF
// limit routed SMART for the regular session, the one shape the broker layer
// admits (pkg/ibkr validateAdaptiveOrder). An option cannot: the broker layer
// refuses an Adaptive option order, so the reduction sends a plain limit at
// the mid and its terms say so.
func reduceAdaptiveSupported(contract rpc.ContractParams) bool {
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF":
	default:
		return false
	}
	if exchange := strings.ToUpper(strings.TrimSpace(contract.Exchange)); exchange != "" && exchange != "SMART" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(contract.Currency), "USD")
}

// preparedReduce holds the resolved order intent and disclosure for a trim.
type preparedReduce struct {
	row      rpc.PositionView
	params   rpc.OrderPreviewParams
	secType  string
	action   string
	qty      int
	hedge    bool
	algoNote string
	// committed is what Canary's own working orders already sell of the
	// line, disclosed beside the size.
	committed float64
}

// prepareReduce resolves the holding for a single-position trim and delegates to
// prepareReduceForRow. Any returned blockers are terminal (no token minted) and
// carry remediation. It performs no broker write — the caller routes the result
func (s *Server) prepareReduce(ctx context.Context, p rpc.TradeProposalReduceParams) (preparedReduce, []rpc.TradingBlocker, error) {
	posResult, err := s.reducePositions(ctx)
	if err != nil {
		return preparedReduce{}, nil, err
	}
	row, blockers := findReducePosition(posResult, p.ConID, p.Symbol)
	if len(blockers) > 0 {
		return preparedReduce{}, blockers, nil
	}
	free := row.Quantity
	if reduceSingleEligible(row) {
		action := rpc.OrderActionSell
		if row.Quantity < 0 {
			action = rpc.OrderActionBuy
		}
		committed, blockers := s.reduceCommittedSales(proposalContractFromPosition(row, positionWireSecType(row.SecType)), action)
		if len(blockers) == 0 {
			free, blockers = reduceFreeQuantity(row.Quantity, committed, row.Symbol)
		}
		if len(blockers) > 0 {
			return preparedReduce{row: row, hedge: isProtectiveShort(row)}, blockers, nil
		}
	}
	prep, blockers := prepareReduceForRow(row, p, s.reduceOrderLimits(), free)
	prep.committed = math.Abs(row.Quantity) - math.Abs(free)
	return prep, blockers, nil
}

// reduceOrderLimits is the loaded policy's [order_limits] table, nil when no
// policy is loaded; a reduction then cannot be priced and says so.
func (s *Server) reduceOrderLimits() *risk.ConstitutionOrderLimits {
	if s == nil {
		return nil
	}
	if snap := s.riskPolicies.snapshot(); snap.policy != nil {
		return snap.policy.OrderLimits
	}
	return nil
}

// reduceAllowanceFor is the policy's allowance for the holding's security
// type, or the blocker naming the key that is not written.
func reduceAllowanceFor(limits *risk.ConstitutionOrderLimits, row rpc.PositionView) (float64, []rpc.TradingBlocker) {
	pct, err := limits.ReduceAllowancePctFor(positionWireSecType(row.SecType))
	if err != nil {
		return 0, []rpc.TradingBlocker{{Code: "reduce_allowance_unset", Message: err.Error(), Action: "Restart Canary so it writes the owner's allowances, then preview again; nothing was sent."}}
	}
	return pct, nil
}

// reducePositions is the one positions read behind a reduce preview, sweep
// and tokened submit; tests replace it through reducePositionsForTest.
func (s *Server) reducePositions(ctx context.Context) (*rpc.PositionsResult, error) {
	var pos *rpc.PositionsResult
	var err error
	if s.reducePositionsForTest != nil {
		pos, err = s.reducePositionsForTest(ctx)
	} else {
		pos, err = s.handlePositionsList(ctx, &rpc.Request{})
	}
	if err != nil || pos == nil {
		return pos, err
	}
	var held []reduceHeldPosition
	for _, rows := range [][]rpc.PositionView{pos.Stocks, pos.Options} {
		for _, row := range rows {
			if reduceSingleEligible(row) {
				held = append(held, reduceHeldPosition{proposalContractFromPosition(row, positionWireSecType(row.SecType)), row.Quantity})
			}
		}
	}
	if _, err := s.ensureReduceFillCoherence(ctx, held, 0); err != nil {
		return nil, err
	}
	return pos, nil
}

// prepareReduceForRow applies the hedge exclusion, sizes the order, and builds
// the gated preview params for an already-resolved position row. It is the
// single-position trim's primitive: it never reads positions or writes to the
// broker. Terminal blockers (not_reducible, hedge_excluded, percent_too_small,
// ...) come back with the row echoed so callers can disclose what was acted on.
// free is the part of the line Canary's own working orders do not already
// sell; the percentage applies to it.
func prepareReduceForRow(row rpc.PositionView, p rpc.TradeProposalReduceParams, limits *risk.ConstitutionOrderLimits, free float64) (preparedReduce, []rpc.TradingBlocker) {
	hedge := isProtectiveShort(row)
	if !reduceSingleEligible(row) {
		return preparedReduce{row: row, hedge: hedge}, []rpc.TradingBlocker{{Code: "not_reducible", Message: fmt.Sprintf("%s %s is not eligible for a percentage reduce", row.Symbol, positionWireSecType(row.SecType)), Action: "The reduce action covers stocks/ETFs, long options and a held short index hedge."}}
	}
	if hedge && !p.IncludeHedges {
		return preparedReduce{row: row, hedge: hedge}, []rpc.TradingBlocker{{Code: "hedge_excluded", Message: fmt.Sprintf("%s is a protective short (hedge) and is excluded from the reduce workflow", row.Symbol), Action: "Set include_hedges to trim this hedge deliberately."}}
	}
	adaptive, blockers := reduceAdaptivePriority(p.AdaptivePriority)
	if len(blockers) > 0 {
		return preparedReduce{row: row, hedge: hedge}, blockers
	}
	qty, blockers := reduceQuantityForPercent(free, p.Percent)
	if len(blockers) > 0 {
		return preparedReduce{row: row, hedge: hedge}, blockers
	}
	allowance, blockers := reduceAllowanceFor(limits, row)
	if len(blockers) > 0 {
		return preparedReduce{row: row, hedge: hedge}, blockers
	}
	prep := buildPreparedReduce(row, qty, p.TimeoutMs, adaptive, allowance)
	prep.hedge = hedge
	return prep, nil
}

// buildPreparedReduce assembles the gated order-preview params for a row whose
// quantity has already been resolved — by reduceQuantityForPercent (single
// position) or by the portfolio sweep's dollar-delta pro-rata allocator. It is
// the shared tail both sizing paths converge on. adaptive is the resolved
// priority ("" for a plain limit); it rides on the draft only where the
// contract supports Adaptive, otherwise the terms carry reduceAdaptiveNote.
// allowance is the policy's percent beyond the touch the limit is priced at.
func buildPreparedReduce(row rpc.PositionView, qty int, timeoutMs int, adaptive string, allowance float64) preparedReduce {
	secType := positionWireSecType(row.SecType)
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	contract := proposalContractFromPosition(row, secType)
	params := rpc.OrderPreviewParams{
		Action:    action,
		Contract:  contract,
		Quantity:  qty,
		OrderType: rpc.OrderTypeLMT,
		Strategy:  rpc.OrderStrategyAllowanceLimit,
		Allowance: &rpc.OrderAllowanceLimit{Pct: allowance},
		TIF:       rpc.OrderTIFDay,
		TimeoutMs: timeoutMs,
		Source:    reduceOrderSource,
	}
	prep := preparedReduce{row: row, params: params, secType: secType, action: action, qty: qty}
	switch {
	case adaptive == "":
	case reduceAdaptiveSupported(contract):
		prep.params.AdaptivePriority = adaptive
	default:
		prep.algoNote = reduceAdaptiveNote
	}
	return prep
}

// preparedReduceWithQty is the portfolio sweep's entry point: qty is already
// caller should never offer an out-of-scope row or a zero qty) before
func preparedReduceWithQty(row rpc.PositionView, qty int, timeoutMs int, adaptive string, limits *risk.ConstitutionOrderLimits) (preparedReduce, []rpc.TradingBlocker) {
	if !reduceEligible(row) {
		return preparedReduce{row: row}, []rpc.TradingBlocker{{Code: "not_reducible", Message: fmt.Sprintf("%s %s is not eligible for a portfolio trim", row.Symbol, positionWireSecType(row.SecType)), Action: "The sweep covers stocks/ETFs and long options."}}
	}
	if qty < 1 {
		return preparedReduce{row: row}, []rpc.TradingBlocker{{Code: "qty_zero", Message: "this holding's allocated risk share rounded to less than one unit", Action: "Nothing to trim at this percentage; choose a larger percentage."}}
	}
	allowance, blockers := reduceAllowanceFor(limits, row)
	if len(blockers) > 0 {
		return preparedReduce{row: row}, blockers
	}
	return buildPreparedReduce(row, qty, timeoutMs, adaptive, allowance), nil
}

// reduceClock returns the daemon clock, honoring the test override.
func (s *Server) reduceClock() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// orderTransmitError is a place error raised after the attempt was journaled
// and the transmit began: unless the transport proves nothing was written,
// the order may be at the broker.
type orderTransmitError struct{ err error }

func (e *orderTransmitError) Error() string { return e.err.Error() }
func (e *orderTransmitError) Unwrap() error { return e.err }

// reducePlaceBlocker is a reduction's refusal for a place error (review
// 2026-10-22, finding 4): a transmit that may have reached the broker is
// submit_unclear, never a refusal that reads as "nothing was sent"; a
// consumed token and every error before the transmit keep their codes.
func reducePlaceBlocker(err error) rpc.TradingBlocker {
	if errors.Is(err, errOrderPreviewTokenAlreadyUsed) {
		return rpc.TradingBlocker{Code: "token_consumed", Message: err.Error(), Action: "This token was already submitted; check open orders, then preview again for a further reduction."}
	}
	if _, ok := errors.AsType[*orderTransmitError](err); ok && ibkrlib.SendDispositionOf(err) != ibkrlib.SendDispositionDefinitelyUnsent {
		return rpc.TradingBlocker{Code: "submit_unclear", Message: err.Error(), Action: "The order may have reached the broker; check open orders before another reduction."}
	}
	return rpc.TradingBlocker{Code: "submit_failed", Message: err.Error(), Action: "Reconcile open orders before retrying."}
}

// reduceCommittedSales is what Canary's own working orders already sell (or
// buy, for a short) of the holding: the remaining quantity of every open
// order Canary placed on the exact contract in the reduction's direction,
// including the remaining contracts sold by each working combo leg,
// its own stops aside, which the one-cancels-all link and the stop guard
// keep within the line (review 2026-10-22, finding 2; owner instruction
// 2026-10-22 17:45 CEST). Hand orders in TWS are the owner's and do not
// count (owner decision 2026-10-12 19:55 CEST).
func (s *Server) reduceCommittedSales(contract rpc.ContractParams, action string) (float64, []rpc.TradingBlocker) {
	inventory, ok := s.journalOrderViewsForInventory()
	if !ok {
		return 0, []rpc.TradingBlocker{{Code: "orders_unavailable", Message: "Canary's order journal cannot be read, so its working reductions of this holding are unknown", Action: "Retry once the journal reads; nothing was sent."}}
	}
	scope := s.currentBrokerStateScope()
	committed := 0.0
	for i := range inventory.views {
		v := &inventory.views[i]
		if !protectiveStopGuardCanaryPlaced(v, scope) || isProtectiveStopOrderType(v.OrderType) {
			continue
		}
		order := journalOrderIdentity(*v)
		order.Remaining = orderViewRemainingQuantity(*v)
		order.Legs = inventory.legsOf(v)
		quantity := order.competesWith(contract, action)
		if math.IsNaN(quantity) || math.IsInf(quantity, 0) {
			return 0, []rpc.TradingBlocker{{Code: "orders_unavailable", Message: "Canary cannot determine how much a working combo would trade of this holding", Action: "Reconcile the working combo in Orders, then preview again; nothing was sent."}}
		}
		committed += quantity
	}
	return committed, nil
}

// reduceFreeQuantity is the part of a held line Canary's own working orders
// do not already sell, or the blocker when nothing is left.
func reduceFreeQuantity(held float64, committed float64, symbol string) (float64, []rpc.TradingBlocker) {
	free := math.Abs(held) - committed
	if math.Floor(free+1e-9) < 1 {
		return 0, []rpc.TradingBlocker{{Code: "reduce_already_working", Message: fmt.Sprintf("Canary's working orders already sell %s of the %s held of %s", formatPositionQuantity(committed), formatPositionQuantity(math.Abs(held)), strings.ToUpper(strings.TrimSpace(symbol))), Action: "Wait for the working reduction, or cancel it in Orders and reduce again; nothing was sent."}}
	}
	if held < 0 {
		return -free, nil
	}
	return free, nil
}

// reduceResultBase seeds the result with the resolved disclosure fields shared
func reduceResultBase(prep preparedReduce, percent int, now time.Time) rpc.TradeProposalReduceResult {
	secType := prep.secType
	if secType == "" && prep.row.SecType != "" {
		secType = positionWireSecType(prep.row.SecType)
	}
	return rpc.TradeProposalReduceResult{
		ConID:             prep.row.ConID,
		Symbol:            strings.ToUpper(strings.TrimSpace(prep.row.Symbol)),
		SecType:           secType,
		Action:            prep.action,
		Percent:           percent,
		PositionQuantity:  prep.row.Quantity,
		ReduceQuantity:    prep.qty,
		CommittedQuantity: prep.committed,
		HedgeLike:         prep.hedge,
		AsOf:              now,
	}
}

// reduceQuoteMid is the mid of a quote, for the review: the quote's own
// midpoint, else the bid-ask mid.
func reduceQuoteMid(quote rpc.OrderQuoteSnapshot) (float64, bool) {
	if quote.Midpoint != nil && *quote.Midpoint > 0 && !math.IsInf(*quote.Midpoint, 0) {
		return *quote.Midpoint, true
	}
	if quote.Bid != nil && quote.Ask != nil && *quote.Bid > 0 && *quote.Ask >= *quote.Bid {
		return (*quote.Bid + *quote.Ask) / 2, true
	}
	return 0, false
}

// reducePriceBound states the worst price the signed draft trades at: its
// limit, the allowance and the touch it was priced from, and the mid of the
// reviewed quote. A draft without an allowance (a token from before the
// worst-price rule) has none.
func reducePriceBound(draft rpc.OrderDraft, reviewed rpc.OrderQuoteSnapshot) (rpc.TradeProposalReducePriceBound, bool) {
	a := draft.Allowance
	if a == nil || !(a.Pct >= 0) || math.IsInf(a.Pct, 0) || !positiveFinite(a.Reference) || !positiveFinite(draft.LimitPrice) {
		return rpc.TradeProposalReducePriceBound{}, false
	}
	side := "bid"
	if strings.EqualFold(draft.Action, rpc.OrderActionBuy) {
		side = "ask"
	}
	mid, _ := reduceQuoteMid(reviewed)
	return rpc.TradeProposalReducePriceBound{Side: side, Reference: a.Reference, ReferenceMid: mid, AllowancePct: a.Pct, WorstPrice: draft.LimitPrice}, true
}

// reducePriceBoundCheck holds the market to the reviewed worst price: a sale
// is refused when the current bid is already below its limit, a purchase
// when the ask is above it. The reviewed limit is never re-priced.
func reducePriceBoundCheck(draft rpc.OrderDraft, reviewed, current rpc.OrderQuoteSnapshot) (rpc.TradeProposalReducePriceBound, []rpc.TradingBlocker) {
	bound, ok := reducePriceBound(draft, reviewed)
	if !ok {
		return bound, []rpc.TradingBlocker{{Code: "price_bound_unavailable", Message: "the reviewed terms state no worst price", Action: "Preview again; nothing was sent."}}
	}
	touch := current.Bid
	if bound.Side == "ask" {
		touch = current.Ask
	}
	if touch == nil || !positiveFinite(*touch) {
		return bound, []rpc.TradingBlocker{{Code: "quote_unavailable", Message: "no current " + bound.Side + " to check the reviewed worst price against", Action: "Preview again once a live quote is available; nothing was sent."}}
	}
	if (bound.Side == "bid" && *touch < bound.WorstPrice-1e-9) || (bound.Side == "ask" && *touch > bound.WorstPrice+1e-9) {
		where := "below"
		if bound.Side == "ask" {
			where = "above"
		}
		return bound, []rpc.TradingBlocker{{Code: "beyond_worst_price", Message: fmt.Sprintf("the %s is %.4f, %s the reviewed worst price %.4f (%.2f%% beyond the reviewed %s %.4f)", bound.Side, *touch, where, bound.WorstPrice, bound.AllowancePct, bound.Side, bound.Reference), Action: "Preview again at the current market; the reviewed limit was not sent."}}
	}
	return bound, nil
}

// reduceAlgoNoteFor says, for a draft without Adaptive, whether the contract
// is the reason.
func reduceAlgoNoteFor(draft rpc.OrderDraft) string {
	if draft.AdaptivePriority == "" && !reduceAdaptiveSupported(draft.Contract) {
		return reduceAdaptiveNote
	}
	return ""
}

// reduceTermsFor is the exact-terms block a token binds, written from the
// signed draft and the quote it was priced from.
func reduceTermsFor(draft rpc.OrderDraft, quote rpc.OrderQuoteSnapshot, tokenID string, expiresAt time.Time, algoNote string) *rpc.TradeProposalReduceTerms {
	terms := &rpc.TradeProposalReduceTerms{
		PreviewTokenID:   tokenID,
		ExpiresAt:        expiresAt,
		Contract:         draft.Contract,
		Action:           draft.Action,
		Quantity:         draft.Quantity,
		OrderType:        draft.OrderType,
		LimitPrice:       draft.LimitPrice,
		TIF:              draft.TIF,
		AdaptivePriority: draft.AdaptivePriority,
		AlgoNote:         algoNote,
	}
	if draft.AdaptivePriority != "" {
		terms.Algo = "Adaptive"
	}
	terms.PriceBound, _ = reducePriceBound(draft, quote)
	return terms
}

// reduceAttachPreview fills the token, terms and sanitized preview of one
// minted reduce preview into the result.
func reduceAttachPreview(res *rpc.TradeProposalReduceResult, preview *rpc.OrderPreviewResult, algoNote string) {
	res.PreviewToken = preview.PreviewToken
	res.PreviewTokenID = preview.PreviewTokenID
	res.PreviewTokenExpiresAt = preview.PreviewTokenExpiresAt
	res.SubmitEligible = preview.SubmitEligible
	res.Preview = sanitizeProposalPreviewForProposal(preview, rpc.TradeProposal{})
	res.Terms = reduceTermsFor(preview.Draft, preview.Quote, preview.PreviewTokenID, preview.PreviewTokenExpiresAt, algoNote)
}

func (s *Server) tradeProposalReducePreview(ctx context.Context, p rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	now := s.reduceClock()
	prep, blockers, err := s.prepareReduce(ctx, p)
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		res := reduceResultBase(prep, p.Percent, now)
		res.Blockers = blockers
		return &res, nil
	}
	preview, err := s.previewOrder(ctx, prep.params)
	if err != nil {
		res := reduceResultBase(prep, p.Percent, now)
		res.Blockers = []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error(), Action: "Refresh positions and preview again."}}
		return &res, nil
	}
	res := reduceResultBase(prep, p.Percent, now)
	if blockers := reduceCloseReduceBlockers(preview); len(blockers) > 0 {
		res.Blockers = blockers
		return &res, nil
	}
	reduceAttachPreview(&res, preview, prep.algoNote)
	if !preview.SubmitEligible {
		res.PreviewToken = ""
		res.Blockers = previewNotSubmitEligibleBlockers(preview)
		return &res, nil
	}
	res.Accepted = true
	return &res, nil
}

func (s *Server) tradeProposalReduceSubmit(ctx context.Context, p rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	now := s.reduceClock()
	// Write gate before the dedupe replay: a freeze engaged between the
	// original call and a RequestRef retry must surface as a blocker, not
	// replay a pre-freeze result.
	if blockers := s.proposalSubmitWriteBlockers(p.Origin); len(blockers) > 0 {
		res := rpc.TradeProposalReduceResult{ConID: p.ConID, Percent: p.Percent, Blockers: blockers, AsOf: now}
		return &res, nil
	}
	if e, ok := s.reduceReplay(p.RequestRef); ok && e.single != nil {
		clone := *e.single
		clone.Replayed = true
		return &clone, nil
	}
	if strings.TrimSpace(p.PreviewToken) != "" {
		res, err := s.reduceSubmitToken(ctx, p, now)
		if err != nil {
			return nil, err
		}
		s.reduceRemember(p.RequestRef, reduceDedupeEntry{single: res})
		return res, nil
	}
	prep, blockers, err := s.prepareReduce(ctx, p)
	if err != nil {
		return nil, err
	}
	if len(blockers) > 0 {
		res := reduceResultBase(prep, p.Percent, now)
		res.Blockers = blockers
		return &res, nil
	}
	preview, err := s.previewOrder(ctx, prep.params)
	if err != nil {
		res := reduceResultBase(prep, p.Percent, now)
		res.Blockers = []rpc.TradingBlocker{{Code: "preview_failed", Message: err.Error(), Action: "Refresh positions and preview again."}}
		return &res, nil
	}
	res := reduceResultBase(prep, p.Percent, now)
	reduceAttachPreview(&res, preview, prep.algoNote)
	res.PreviewToken = "" // consumed below; a submit never hands the token out
	if blockers := reduceCloseReduceBlockers(preview); len(blockers) > 0 {
		res.Blockers = blockers
		return &res, nil
	}
	if !preview.SubmitEligible {
		res.Blockers = previewNotSubmitEligibleBlockers(preview)
		return &res, nil
	}
	place, err := s.proposalPlaceOrder(ctx, rpc.OrderPlaceParams{PreviewToken: preview.PreviewToken, TimeoutMs: p.TimeoutMs, Origin: p.Origin})
	if err != nil {
		res.Blockers = []rpc.TradingBlocker{reducePlaceBlocker(err)}
		return &res, nil
	}
	res.Accepted = place.Accepted
	res.Place = place
	res.OrderRef = place.OrderRef
	res.Message = place.Message
	res.RequestRef = strings.TrimSpace(p.RequestRef)
	s.reduceRemember(p.RequestRef, reduceDedupeEntry{single: &res})
	return &res, nil
}

// reduceTokenOutcome is one reduce token taken through the submit checks:
// the verified payload and the current row when the token read, the bound
// the market was held to, and either the place result or the blockers that
// stopped it. Nothing is sent when blockers is non-empty.
type reduceTokenOutcome struct {
	tokenID  string
	payload  orderPreviewTokenPayload
	verified bool
	row      rpc.PositionView
	rowFound bool
	bound    rpc.TradeProposalReducePriceBound
	place    *rpc.OrderPlaceResult
	blockers []rpc.TradingBlocker
}

// reduceVerifyToken checks that a token is a current, submit-eligible reduce
// preview of this daemon's gate: the signature, authority epoch, expiry,
// mode, account and endpoint are the order path's own checks; the source
// keeps an ordinary order preview out of the reduce path, whose further
// checks assume a reduction of a held position.
func (s *Server) reduceVerifyToken(token string) (orderPreviewTokenPayload, []rpc.TradingBlocker) {
	payload, err := s.verifyPreviewTokenForPlace(token)
	if err != nil {
		code := "token_invalid"
		if strings.Contains(err.Error(), "expired") {
			code = "token_expired"
		}
		return orderPreviewTokenPayload{}, []rpc.TradingBlocker{{Code: code, Message: err.Error(), Action: "Preview the reduction again and submit the new token; nothing was sent."}}
	}
	if payload.Draft.Source != reduceOrderSource && payload.Draft.Source != hedgeOrderSource {
		return orderPreviewTokenPayload{}, []rpc.TradingBlocker{{Code: "token_not_reduce", Message: "the preview token is not a reduce preview", Action: "Submit a token from `proposals reduce`; an order preview goes through `order place`."}}
	}
	return payload, nil
}

// reduceTokenPositionCheck holds the reviewed quantity to the position as it
// is now: the holding must still be there on the reviewed side, and the
// reviewed reduction may not exceed what is held (owner decision 2026-10-12
// 19:55 CEST: a sale of at most the confirmed long is admitted). A changed
// position refuses in plain words; the quantity is never resized.
func reduceTokenPositionCheck(pos *rpc.PositionsResult, draft rpc.OrderDraft) (rpc.PositionView, []rpc.TradingBlocker) {
	row, blockers := findReducePosition(pos, draft.Contract.ConID, "")
	if len(blockers) > 0 {
		for i := range blockers {
			if blockers[i].Code == "position_not_found" {
				blockers[i] = rpc.TradingBlocker{Code: "position_changed", Message: fmt.Sprintf("%s is no longer held; the reviewed reduction of %d cannot be sent", strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol)), draft.Quantity), Action: "Refresh positions; nothing was sent."}
			}
		}
		return rpc.PositionView{}, blockers
	}
	sell := strings.EqualFold(draft.Action, rpc.OrderActionSell)
	if (sell && row.Quantity <= 0) || (!sell && row.Quantity >= 0) {
		return row, []rpc.TradingBlocker{{Code: "position_changed", Message: fmt.Sprintf("%s is now %s, not on the reviewed side", strings.ToUpper(strings.TrimSpace(row.Symbol)), formatPositionQuantity(row.Quantity)), Action: "Preview the reduction again; nothing was sent."}}
	}
	held, _ := closeReduceQuantity(row.Quantity)
	if draft.Quantity > held {
		return row, []rpc.TradingBlocker{{Code: "position_changed", Message: fmt.Sprintf("%s is now %s held; the reviewed reduction of %d exceeds it", strings.ToUpper(strings.TrimSpace(row.Symbol)), formatPositionQuantity(row.Quantity), draft.Quantity), Action: "Preview the reduction again at the current position; nothing was sent."}}
	}
	return row, nil
}

// reduceHeldRow is the held row of a contract, when the book holds it.
func reduceHeldRow(pos *rpc.PositionsResult, conID int) (rpc.PositionView, bool) {
	if pos == nil || conID <= 0 {
		return rpc.PositionView{}, false
	}
	for _, rows := range [][]rpc.PositionView{pos.Stocks, pos.Options} {
		for _, row := range rows {
			if row.ConID == conID {
				return row, true
			}
		}
	}
	return rpc.PositionView{}, false
}

func formatPositionQuantity(q float64) string {
	if q == math.Trunc(q) {
		return fmt.Sprintf("%d", int64(q))
	}
	return fmt.Sprintf("%g", q)
}

// reduceTokenLeg takes one reduce token through every submit check and, when
// all pass, places exactly the token's draft. The checks stop at the first
// refusal; a refusal here sends nothing. pos is the positions read the
// caller made once for the whole request.
// locked, when set, refuses a line the owner locked after the review: a bulk
// basket never sells a locked holding.
func (s *Server) reduceTokenLeg(ctx context.Context, token string, pos *rpc.PositionsResult, timeoutMs int, origin string, locked func(conID int) bool) reduceTokenOutcome {
	var out reduceTokenOutcome
	payload, blockers := s.reduceVerifyToken(token)
	if len(blockers) > 0 {
		out.blockers = blockers
		return out
	}
	out.payload, out.verified, out.tokenID = payload, true, payload.TokenID
	if locked != nil && locked(payload.Draft.Contract.ConID) {
		out.blockers = []rpc.TradingBlocker{{Code: "holding_locked", Message: fmt.Sprintf("%s was locked after the review; a bulk reduction never sells a locked holding", strings.ToUpper(strings.TrimSpace(payload.Draft.Contract.Symbol))), Action: "Unlock it first, or reduce it on its own; nothing was sent."}}
		return out
	}
	if payload.Draft.Source == hedgeOrderSource {
		// A hedge sale opens or adds to a short: no held row is required,
		// and the whole book's short contracts stay within max_contracts.
		if row, ok := reduceHeldRow(pos, payload.Draft.Contract.ConID); ok {
			out.row, out.rowFound = row, true
		}
		if err := s.indexFutureSessionRefusal(s.orderNow()); err != nil {
			blockers = previewFailureBlockers(err)
		} else {
			blockers = s.indexHedgeCapRefusal(payload.Draft, indexHedgeShortHeld(pos))
		}
	} else {
		var row rpc.PositionView
		row, blockers = reduceTokenPositionCheck(pos, payload.Draft)
		out.row, out.rowFound = row, row.ConID != 0
		if len(blockers) == 0 {
			// Another of Canary's sales may have gone out since the review:
			// the reviewed size must still fit what they leave unsold.
			var committed float64
			committed, blockers = s.reduceCommittedSales(payload.Draft.Contract, payload.Draft.Action)
			if len(blockers) == 0 {
				var free float64
				if free, blockers = reduceFreeQuantity(row.Quantity, committed, row.Symbol); len(blockers) == 0 && float64(payload.Draft.Quantity) > math.Abs(free)+1e-9 {
					blockers = []rpc.TradingBlocker{{Code: "reduce_already_working", Message: fmt.Sprintf("Canary's working orders already sell %s of the %s held of %s; the reviewed %d would sell beyond the line", formatPositionQuantity(committed), formatPositionQuantity(math.Abs(row.Quantity)), strings.ToUpper(strings.TrimSpace(row.Symbol)), payload.Draft.Quantity), Action: "Preview again for what is left; nothing was sent."}}
				}
			}
		}
	}
	if len(blockers) > 0 {
		out.blockers = blockers
		return out
	}
	legCtx, cancel := legContext(ctx, timeoutMs)
	defer cancel()
	quote, err := s.fetchPreviewQuoteBound(legCtx, payload.Draft.Contract, orderPreviewTimeout(timeoutMs), nil)
	if err != nil {
		out.blockers = []rpc.TradingBlocker{{Code: "quote_unavailable", Message: err.Error(), Action: "Preview again once a live quote is available; nothing was sent."}}
		return out
	}
	out.bound, blockers = reducePriceBoundCheck(payload.Draft, payload.Quote, quote)
	if len(blockers) > 0 {
		out.blockers = blockers
		return out
	}
	place, err := s.proposalPlaceOrder(legCtx, rpc.OrderPlaceParams{PreviewToken: token, TimeoutMs: timeoutMs, Origin: origin})
	if err != nil {
		out.blockers = []rpc.TradingBlocker{reducePlaceBlocker(err)}
		return out
	}
	out.place = place
	return out
}

// reduceSubmitToken places exactly the reviewed terms of one reduce token.
func (s *Server) reduceSubmitToken(ctx context.Context, p rpc.TradeProposalReduceParams, now time.Time) (*rpc.TradeProposalReduceResult, error) {
	res := &rpc.TradeProposalReduceResult{Percent: p.Percent, RequestRef: strings.TrimSpace(p.RequestRef), AsOf: now}
	pos, err := s.reducePositions(ctx)
	if err != nil {
		res.Blockers = []rpc.TradingBlocker{{Code: "positions_unavailable", Message: err.Error(), Action: "Retry once the daemon has refreshed positions; nothing was sent."}}
		return res, nil
	}
	out := s.reduceTokenLeg(ctx, strings.TrimSpace(p.PreviewToken), pos, p.TimeoutMs, p.Origin, nil)
	reduceApplyTokenOutcome(res, out)
	return res, nil
}

// reduceApplyTokenOutcome writes a token outcome into the single result: the
// terms the token bound, the position it was checked against, and the place
// outcome or the blockers.
func reduceApplyTokenOutcome(res *rpc.TradeProposalReduceResult, out reduceTokenOutcome) {
	if out.verified {
		draft := out.payload.Draft
		res.ConID = draft.Contract.ConID
		res.Symbol = strings.ToUpper(strings.TrimSpace(draft.Contract.Symbol))
		res.SecType = draft.Contract.SecType
		res.Action = draft.Action
		res.ReduceQuantity = draft.Quantity
		res.PreviewTokenID = out.payload.TokenID
		res.PreviewTokenExpiresAt = out.payload.ExpiresAt
		res.SubmitEligible = true
		res.Terms = reduceTermsFor(draft, out.payload.Quote, out.payload.TokenID, out.payload.ExpiresAt, reduceAlgoNoteFor(draft))
	}
	if out.rowFound {
		res.PositionQuantity = out.row.Quantity
		res.HedgeLike = isProtectiveShort(out.row)
	}
	if len(out.blockers) > 0 {
		res.Blockers = out.blockers
		return
	}
	res.Accepted = out.place.Accepted
	res.Place = out.place
	res.OrderRef = out.place.OrderRef
	res.Message = out.place.Message
}

// reduceReplay returns the remembered result for a repeated RequestRef, if
// the reference is within reduceDedupeTTL.
func (s *Server) reduceReplay(ref string) (reduceDedupeEntry, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return reduceDedupeEntry{}, false
	}
	s.reduceDedupeMu.Lock()
	defer s.reduceDedupeMu.Unlock()
	now := s.reduceClock()
	for k, e := range s.reduceDedupe {
		if now.Sub(e.at) > reduceDedupeTTL {
			delete(s.reduceDedupe, k)
		}
	}
	e, ok := s.reduceDedupe[ref]
	return e, ok
}

func (s *Server) reduceRemember(ref string, e reduceDedupeEntry) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return
	}
	s.reduceDedupeMu.Lock()
	defer s.reduceDedupeMu.Unlock()
	if s.reduceDedupe == nil {
		s.reduceDedupe = make(map[string]reduceDedupeEntry)
	}
	e.at = s.reduceClock()
	s.reduceDedupe[ref] = e
}

// reduceCloseReduceBlockers is the defense-in-depth gate: even though the size
// is floored to the held quantity, a stale-position broker preview that
// classifies the order as anything but close/reduce fails closed here, mirroring
// proposalPreviewSafetyBlockers for the proposal path.
func reduceCloseReduceBlockers(preview *rpc.OrderPreviewResult) []rpc.TradingBlocker {
	if preview == nil {
		return []rpc.TradingBlocker{{Code: "preview_missing", Message: "order preview result is unavailable", Action: "Preview the reduce again."}}
	}
	if !isRiskReducing(preview.Position.Effect) {
		return []rpc.TradingBlocker{{Code: "effect_not_close_reduce", Message: fmt.Sprintf("preview effect %q is not close/reduce", preview.Position.Effect), Action: "Refresh positions and preview again; the reduce never opens, increases, or flips exposure."}}
	}
	return nil
}

func (s *Server) handleTradeProposalsReducePreview(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalReduceResult, error) {
	var p rpc.TradeProposalReduceParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.tradeProposalReducePreview(ctx, p)
}

func (s *Server) handleTradeProposalsReduceSubmit(ctx context.Context, req *rpc.Request) (*rpc.TradeProposalReduceResult, error) {
	var p rpc.TradeProposalReduceParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	// Serialize the check-then-act broker write against every other writer,
	// matching handleTradeProposalsSubmit/handleOrderPlace. proposalPlaceOrder
	// -> placeOrder does not self-lock; the handler holds the mutex.
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.tradeProposalReduceSubmit(ctx, p)
}
