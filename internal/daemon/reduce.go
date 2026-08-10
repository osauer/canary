package daemon

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/rpc"
)

// reduceOrderSource tags the order journal so a discretionary percentage trim
const reduceOrderSource = "manual_reduce"

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

// preparedReduce holds the resolved order intent and disclosure for a trim.
type preparedReduce struct {
	row     rpc.PositionView
	params  rpc.OrderPreviewParams
	secType string
	action  string
	qty     int
	hedge   bool
}

// prepareReduce resolves the holding for a single-position trim and delegates to
// prepareReduceForRow. Any returned blockers are terminal (no token minted) and
// carry remediation. It performs no broker write — the caller routes the result
func (s *Server) prepareReduce(ctx context.Context, p rpc.TradeProposalReduceParams) (preparedReduce, []rpc.TradingBlocker, error) {
	posResult, err := s.handlePositionsList(ctx, &rpc.Request{})
	if err != nil {
		return preparedReduce{}, nil, err
	}
	row, blockers := findReducePosition(posResult, p.ConID, p.Symbol)
	if len(blockers) > 0 {
		return preparedReduce{}, blockers, nil
	}
	prep, blockers := prepareReduceForRow(row, p)
	return prep, blockers, nil
}

// prepareReduceForRow applies the hedge exclusion, sizes the order, and builds
// the gated preview params for an already-resolved position row. It is the
// single-position trim's primitive: it never reads positions or writes to the
// broker. Terminal blockers (not_reducible, hedge_excluded, percent_too_small,
// ...) come back with the row echoed so callers can disclose what was acted on.
func prepareReduceForRow(row rpc.PositionView, p rpc.TradeProposalReduceParams) (preparedReduce, []rpc.TradingBlocker) {
	hedge := isProtectiveShort(row)
	if !reduceEligible(row) {
		return preparedReduce{row: row, hedge: hedge}, []rpc.TradingBlocker{{Code: "not_reducible", Message: fmt.Sprintf("%s %s is not eligible for a percentage reduce", row.Symbol, positionWireSecType(row.SecType)), Action: "The reduce action covers stocks/ETFs and long options."}}
	}
	if hedge && !p.IncludeHedges {
		return preparedReduce{row: row, hedge: hedge}, []rpc.TradingBlocker{{Code: "hedge_excluded", Message: fmt.Sprintf("%s is a protective short (hedge) and is excluded from the reduce workflow", row.Symbol), Action: "Set include_hedges to trim this hedge deliberately."}}
	}
	qty, blockers := reduceQuantityForPercent(row.Quantity, p.Percent)
	if len(blockers) > 0 {
		return preparedReduce{row: row, hedge: hedge}, blockers
	}
	prep := buildPreparedReduce(row, qty, p.TimeoutMs)
	prep.hedge = hedge
	return prep, nil
}

// buildPreparedReduce assembles the gated order-preview params for a row whose
// quantity has already been resolved — by reduceQuantityForPercent (single
// position) or by the portfolio sweep's dollar-delta pro-rata allocator. It is
// the shared tail both sizing paths converge on.
func buildPreparedReduce(row rpc.PositionView, qty int, timeoutMs int) preparedReduce {
	secType := positionWireSecType(row.SecType)
	action := rpc.OrderActionSell
	if row.Quantity < 0 {
		action = rpc.OrderActionBuy
	}
	params := rpc.OrderPreviewParams{
		Action:    action,
		Contract:  proposalContractFromPosition(row, secType),
		Quantity:  qty,
		OrderType: rpc.OrderTypeLMT,
		Strategy:  rpc.OrderStrategyPatientLimit,
		TIF:       rpc.OrderTIFDay,
		TimeoutMs: timeoutMs,
		Source:    reduceOrderSource,
	}
	return preparedReduce{row: row, params: params, secType: secType, action: action, qty: qty}
}

// preparedReduceWithQty is the portfolio sweep's entry point: qty is already
// caller should never offer an out-of-scope row or a zero qty) before
func preparedReduceWithQty(row rpc.PositionView, qty int, timeoutMs int) (preparedReduce, []rpc.TradingBlocker) {
	if !reduceEligible(row) {
		return preparedReduce{row: row}, []rpc.TradingBlocker{{Code: "not_reducible", Message: fmt.Sprintf("%s %s is not eligible for a portfolio trim", row.Symbol, positionWireSecType(row.SecType)), Action: "The sweep covers stocks/ETFs and long options."}}
	}
	if qty < 1 {
		return preparedReduce{row: row}, []rpc.TradingBlocker{{Code: "qty_zero", Message: "this holding's allocated risk share rounded to less than one unit", Action: "Nothing to trim at this percentage; choose a larger percentage."}}
	}
	return buildPreparedReduce(row, qty, timeoutMs), nil
}

// reduceClock returns the daemon clock, honoring the test override.
func (s *Server) reduceClock() time.Time {
	if s != nil && s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

// reduceResultBase seeds the result with the resolved disclosure fields shared
func reduceResultBase(prep preparedReduce, percent int, now time.Time) rpc.TradeProposalReduceResult {
	secType := prep.secType
	if secType == "" && prep.row.SecType != "" {
		secType = positionWireSecType(prep.row.SecType)
	}
	return rpc.TradeProposalReduceResult{
		ConID:            prep.row.ConID,
		Symbol:           strings.ToUpper(strings.TrimSpace(prep.row.Symbol)),
		SecType:          secType,
		Action:           prep.action,
		Percent:          percent,
		PositionQuantity: prep.row.Quantity,
		ReduceQuantity:   prep.qty,
		HedgeLike:        prep.hedge,
		AsOf:             now,
	}
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
	res.PreviewTokenID = preview.PreviewTokenID
	res.PreviewTokenExpiresAt = preview.PreviewTokenExpiresAt
	res.SubmitEligible = preview.SubmitEligible
	res.Preview = sanitizeProposalPreviewForProposal(preview, rpc.TradeProposal{})
	if !preview.SubmitEligible {
		res.Blockers = previewNotSubmitEligibleBlockers(preview)
		return &res, nil
	}
	res.Accepted = true
	return &res, nil
}

func (s *Server) tradeProposalReduceSubmit(ctx context.Context, p rpc.TradeProposalReduceParams) (*rpc.TradeProposalReduceResult, error) {
	now := s.reduceClock()
	if blockers := s.proposalSubmitWriteBlockers(p.Origin); len(blockers) > 0 {
		res := rpc.TradeProposalReduceResult{ConID: p.ConID, Percent: p.Percent, Blockers: blockers, AsOf: now}
		return &res, nil
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
	res.PreviewTokenID = preview.PreviewTokenID
	res.PreviewTokenExpiresAt = preview.PreviewTokenExpiresAt
	res.SubmitEligible = preview.SubmitEligible
	res.Preview = sanitizeProposalPreviewForProposal(preview, rpc.TradeProposal{})
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
		res.Blockers = []rpc.TradingBlocker{{Code: "submit_failed", Message: err.Error(), Action: "Reconcile open orders before retrying."}}
		return &res, nil
	}
	res.Accepted = place.Accepted
	res.Place = place
	res.OrderRef = place.OrderRef
	res.Message = place.Message
	return &res, nil
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
