package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	purgeExecuteStatusBlocked   = "blocked"
	purgeExecuteStatusFlat      = "flat"
	purgeExecuteStatusSubmitted = "submitted"
	purgeExecuteStatusPartial   = "partial"
	purgeExecuteStatusError     = "error"

	purgeExecuteSource     = "purge"
	purgeOriginalSideLong  = "LONG"
	purgeOriginalSideShort = "SHORT"
)

var purgeSubmissionUnavailableBlocker = rpc.TradingBlocker{
	Code:    "purge_submission_unavailable",
	Message: "purge/restore broker submission is disabled until exact per-leg portfolio and account-global working-order authority is available",
	Action:  "Use TWS for manual purge/restore, then refresh and reconcile the daemon afterward.",
}

func (s *Server) handlePurgeExecute(ctx context.Context, req *rpc.Request) (*rpc.PurgeExecuteResult, error) {
	var p rpc.PurgeExecuteParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.executePurge(ctx, p)
}

func (s *Server) executePurge(ctx context.Context, p rpc.PurgeExecuteParams) (*rpc.PurgeExecuteResult, error) {
	bypassPreview := true
	if p.BypassPreview != nil {
		bypassPreview = *p.BypassPreview
	}
	targetSymbols := purgeExecuteSymbols(p.Symbols)
	if p.All {
		targetSymbols = nil
	}
	wait := time.Duration(p.WaitMs) * time.Millisecond
	if wait <= 0 {
		wait = 2 * time.Second
	}
	if wait > 10*time.Second {
		wait = 10 * time.Second
	}

	auth, binding, bindingErr := s.authorizeBrokerWriteTransaction(p.Origin, false)
	status := auth.Status
	res := &rpc.PurgeExecuteResult{
		Kind:                 "ibkr.purge_execute",
		PurgeID:              strings.TrimSpace(p.PurgeID),
		Status:               purgeExecuteStatusBlocked,
		Mode:                 status.Mode,
		Account:              status.Account,
		Endpoint:             status.Endpoint,
		ClientID:             status.ClientID,
		BypassPreview:        bypassPreview,
		MonitorCommand:       "canary purge monitor",
		RestoreReviewCommand: "canary purge restore SYMBOL",
		AsOf:                 s.orderNow(),
	}
	if res.PurgeID == "" {
		res.PurgeID = "purge_" + s.orderNow().UTC().Format("20060102_150405")
	}
	blockers := s.purgeExecuteBlockers(status)
	for _, blocker := range auth.Blockers {
		blockers = appendTradingBlockerOnce(blockers, blocker)
	}
	if len(blockers) > 0 {
		res.Blockers = blockers
		res.Message = firstTradingBlockerMessage(blockers)
		return res, nil
	}
	if !bypassPreview {
		res.Blockers = append(res.Blockers, rpc.TradingBlocker{
			Code:    "purge_preview_mode_unavailable",
			Message: "purge currently supports the fast path only",
			Action:  "Omit bypass_preview from the request (or send true); the purge fast path is the only supported mode.",
		})
		res.Message = res.Blockers[0].Message
		return res, nil
	}
	if bindingErr != nil {
		res.Status = purgeExecuteStatusError
		res.ErrorLegs = max(1, len(p.Legs))
		res.Message = bindingErr.Error()
		return res, nil
	}

	positions, err := s.refreshPurgePositions()
	if err != nil {
		res.Status = purgeExecuteStatusError
		res.ErrorLegs = max(1, len(p.Legs))
		res.Message = "refresh current positions: " + err.Error()
		return res, nil
	}
	positions = filterPurgePositionsForAccount(positions, status.Account)

	legs := p.Legs
	if len(legs) == 0 {
		legs = purgeLegsFromCurrentPositions(positions, targetSymbols)
	}
	res.SelectedLegs = len(legs)
	if len(legs) == 0 {
		res.Status = purgeExecuteStatusFlat
		res.Message = purgeNoCurrentPositionMessage(targetSymbols)
		res.AsOf = s.orderNow()
		return res, nil
	}
	openByLeg, err := s.openPurgeOrdersByLeg(status.Account)
	if err != nil {
		res.Status = purgeExecuteStatusError
		res.ErrorLegs = len(legs)
		res.Message = "load open purge orders: " + err.Error()
		return res, nil
	}
	foreignOpen, err := s.foreignOpenOrders(status.Account)
	if err != nil {
		res.Status = purgeExecuteStatusError
		res.ErrorLegs = len(legs)
		res.Message = "load open orders: " + err.Error()
		return res, nil
	}
	activeByLegSide, err := s.activePurgeLedgerQuantityByLegSide(brokerStateScope{Account: status.Account, Mode: status.Mode})
	if err != nil {
		res.Status = purgeExecuteStatusError
		res.ErrorLegs = len(legs)
		res.Message = "load active purge ledger rows: " + err.Error()
		return res, nil
	}

	plans := make([]purgeLegPlan, 0, len(legs))
	for _, leg := range legs {
		plans = append(plans, s.planPurgeLeg(positions, openByLeg, foreignOpen, activeByLegSide, leg))
	}
	quotes := s.prefetchPurgeLegQuotes(ctx, plans)
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			res.Status = purgeExecuteStatusError
			res.Message = err.Error()
			return res, nil
		}
		s.executePurgeLeg(ctx, binding, status, quotes, res, plan, p.Origin)
	}

	if res.SubmittedLegs > 0 && wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			res.Warnings = append(res.Warnings, "lifecycle wait interrupted: "+ctx.Err().Error())
		}
		s.refreshPurgeOrderViews(res)
	}
	res.Status = purgeExecuteFinalStatus(*res)
	if res.Message == "" {
		res.Message = purgeExecuteMessage(*res)
	}
	res.AsOf = s.orderNow()
	return res, nil
}

func (s *Server) currentTradingStatus() rpc.TradingStatus {
	s.mu.Lock()
	ep := s.endpoint
	s.mu.Unlock()
	return s.tradingStatus(ep)
}

func (s *Server) purgeExecuteBlockers(status rpc.TradingStatus) []rpc.TradingBlocker {
	blockers := s.brokerWriteAuthorization(status).Blockers
	blockers = appendTradingBlockerOnce(blockers, purgeSubmissionUnavailableBlocker)
	add := func(code, message, action string) {
		blockers = appendTradingBlockerOnce(blockers, rpc.TradingBlocker{Code: code, Message: message, Action: action})
	}
	if s == nil || s.purgeLedger == nil {
		add("purge_ledger_unavailable", "purge execution requires a writable daemon purge ledger", "Fix the daemon state directory before purging positions.")
	}
	if !s.purgeRestoreEnabled() {
		add("purge_restore_disabled", "purge/restore actions are disabled in platform settings", "Run `canary settings set features.purge_restore.enabled=true` before using purge/restore.")
	}
	return blockers
}

func (s *Server) refreshPurgePositions() ([]*ibkrlib.RawPosition, error) {
	if s.purgeRefreshPositions != nil {
		return s.purgeRefreshPositions()
	}
	c := s.gatewayConnector()
	if c == nil {
		return nil, s.gatewayUnavailableError()
	}
	return c.RefreshPositions(10 * time.Second)
}

func filterPurgePositionsForAccount(positions []*ibkrlib.RawPosition, account string) []*ibkrlib.RawPosition {
	account = strings.TrimSpace(account)
	if account == "" {
		return positions
	}
	filtered := make([]*ibkrlib.RawPosition, 0, len(positions))
	for _, pos := range positions {
		if pos == nil {
			continue
		}
		if pos.Account == "" || strings.EqualFold(pos.Account, account) {
			filtered = append(filtered, pos)
		}
	}
	return filtered
}

func purgeExecuteSymbols(symbols []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(symbols))
	for _, symbol := range symbols {
		symbol = strings.ToUpper(strings.TrimSpace(symbol))
		if symbol == "" || seen[symbol] {
			continue
		}
		seen[symbol] = true
		out = append(out, symbol)
	}
	return out
}

func purgeLegsFromCurrentPositions(positions []*ibkrlib.RawPosition, symbols []string) []rpc.PurgeExecuteLeg {
	symbolSet := map[string]bool{}
	for _, symbol := range symbols {
		symbolSet[strings.ToUpper(strings.TrimSpace(symbol))] = true
	}
	legs := make([]rpc.PurgeExecuteLeg, 0, len(positions))
	for _, pos := range positions {
		if pos == nil || pos.Position == 0 {
			continue
		}
		contract := contractParamsFromRawPosition(*pos, rpc.ContractParams{})
		if contract.Symbol == "" {
			continue
		}
		if len(symbolSet) > 0 && !symbolSet[strings.ToUpper(contract.Symbol)] {
			continue
		}
		action := rpc.OrderActionSell
		originalSide := purgeOriginalSideLong
		if pos.Position < 0 {
			action = rpc.OrderActionBuy
			originalSide = purgeOriginalSideShort
		}
		legs = append(legs, rpc.PurgeExecuteLeg{
			LegID:        purgeLegIDForContract(contract),
			Symbol:       contract.Symbol,
			SecType:      contract.SecType,
			Contract:     contract,
			OriginalSide: originalSide,
			PurgeAction:  action,
			Quantity:     math.Abs(pos.Position),
			Multiplier:   contract.Multiplier,
		})
	}
	return legs
}

func purgeNoCurrentPositionMessage(symbols []string) string {
	if len(symbols) == 0 {
		return "no current positions to purge"
	}
	return "no current positions matched purge target"
}

func (s *Server) activePurgeLedgerQuantityByLegSide(scope brokerStateScope) (map[string]float64, error) {
	out := map[string]float64{}
	if s == nil || s.purgeLedger == nil {
		return out, nil
	}
	rows, _, err := s.purgeLedger.Snapshot(scope, "")
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.RemainingQuantity <= 0 || row.LegID == "" {
			continue
		}
		legID := purgeLegIDForContract(row.Contract)
		if legID == "" {
			legID = row.LegID
		}
		out[purgeLedgerCoverageKey(legID, row.OriginalSide)] += row.RemainingQuantity
	}
	return out, nil
}

// foreignOpenOrders returns open journaled orders for this account that purge
// did not place itself — e.g. protective trailing stops from the proposal
// engine. Closing a position while one rests would leave an orphaned stop
// whose later trigger flips the account short.
func (s *Server) foreignOpenOrders(account string) ([]rpc.OrderView, error) {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return nil, err
	}
	var foreign []rpc.OrderView
	for _, v := range views {
		if !v.Open || v.Source == purgeExecuteSource || v.Source == purgeRestoreSource {
			continue
		}
		if account != "" && v.Account != "" && !strings.EqualFold(v.Account, account) {
			continue
		}
		foreign = append(foreign, v)
	}
	return foreign, nil
}

func foreignOpenOrderForContract(orders []rpc.OrderView, contract rpc.ContractParams) (rpc.OrderView, bool) {
	for _, v := range orders {
		if contract.ConID != 0 && v.ConID != 0 {
			if v.ConID == contract.ConID {
				return v, true
			}
			continue
		}
		if strings.EqualFold(v.Symbol, contract.Symbol) && strings.EqualFold(v.SecType, contract.SecType) {
			return v, true
		}
	}
	return rpc.OrderView{}, false
}

// purgeQuoteTimeout bounds each per-leg pricing snapshot; it matches the
// preview path's default quote budget.
const purgeQuoteTimeout = 5 * time.Second

// purgeQuoteWorkers bounds the pre-submission quote fan-out. 4 mirrors
// positionsPrewarmWorkers — the gateway throttles subscribe churn beyond
// that.
const purgeQuoteWorkers = 4

type purgeQuoteResult struct {
	quote rpc.OrderQuoteSnapshot
	err   error
}

// purgeLegPlan is the pre-quote evaluation of one purge leg: either a skip
// reason, or a normalised leg ready for pricing and submission. Splitting
// planning from submission lets the quote fan-out run only for legs that
// will actually submit — idempotent retries must not touch the gateway.
type purgeLegPlan struct {
	leg     rpc.PurgeExecuteLeg
	skip    string
	warning string
	action  string
	qty     float64
}

// planPurgeLeg runs every cheap local check (position match, side flip,
// open orders, ledger coverage, quantity, action, security type) and
// normalises the leg identity. It performs no gateway IO.
func (s *Server) planPurgeLeg(positions []*ibkrlib.RawPosition, openByLeg map[string][]rpc.OrderView, foreignOpen []rpc.OrderView, activeByLegSide map[string]float64, leg rpc.PurgeExecuteLeg) purgeLegPlan {
	contract, currentQty, found := currentPurgePosition(positions, leg)
	if !found || currentQty == 0 {
		return purgeLegPlan{leg: leg, skip: "already flat"}
	}
	if contract.MinTick <= 0 {
		// Cache-only: the emergency path never waits on a contract-details
		// fetch; previews and proposal refreshes warm this cache.
		contract.MinTick = s.cachedContractMinTick(contract.ConID)
	}
	currentSide := purgeOriginalSideForQuantity(currentQty)
	if leg.OriginalSide == "" {
		leg.OriginalSide = currentSide
	}
	inputLegID := strings.TrimSpace(leg.LegID)
	leg.LegID = purgeLegIDForContract(contract)
	legIDs := purgeLegIDCandidates(inputLegID, leg.LegID, purgeLegacyLegIDForContract(contract))
	leg.Symbol = contract.Symbol
	leg.SecType = contract.SecType
	leg.Contract = contract
	plan := purgeLegPlan{leg: leg}
	if purgeSideFlipped(leg.OriginalSide, currentQty) {
		plan.skip = "current position side no longer matches selected purge leg"
		return plan
	}
	qty := math.Abs(currentQty)
	if purgeOpenOrderExists(openByLeg, legIDs) {
		plan.skip = "open purge/restore order exists for this ledger row"
		return plan
	}
	if foreign, ok := foreignOpenOrderForContract(foreignOpen, contract); ok {
		plan.skip = fmt.Sprintf("open order %s (%s) already works this contract; cancel it first with `canary order cancel %s` so the close cannot double", foreign.OrderRef, nonEmptyString(foreign.OrderType, "order"), foreign.OrderRef)
		return plan
	}
	if covered := activePurgeLedgerCoveredQuantity(activeByLegSide, legIDs, currentSide); covered > 0 {
		if qty <= covered+1e-9 {
			plan.skip = "current quantity already covered by active purge ledger"
			return plan
		}
		qty -= covered
		plan.warning = fmt.Sprintf("%s: reduced purge quantity by %.4g already covered in active purge ledger", purgeExecuteLegLabel(leg), covered)
	}
	if math.Trunc(qty) != qty {
		plan.skip = "fractional current quantity cannot use the integer order path"
		return plan
	}
	action := rpc.OrderActionSell
	if currentQty < 0 {
		action = rpc.OrderActionBuy
	}
	if leg.PurgeAction != "" && !strings.EqualFold(leg.PurgeAction, action) {
		plan.skip = "current position action no longer matches selected purge leg"
		return plan
	}
	if !purgeContractSupported(contract) {
		plan.skip = "unsupported security type for purge execute"
		return plan
	}
	plan.action = action
	plan.qty = qty
	return plan
}

// prefetchPurgeLegQuotes snapshots pricing quotes for every submittable leg
// up front with a bounded fan-out. Sequential fetches at 5 s each could not
// cover an 11+ leg book inside the 55 s purge.execute deadline, turning a
// full emergency close into a PARTIAL one.
func (s *Server) prefetchPurgeLegQuotes(ctx context.Context, plans []purgeLegPlan) map[string]purgeQuoteResult {
	type job struct {
		legID    string
		contract rpc.ContractParams
	}
	jobs := make([]job, 0, len(plans))
	seen := map[string]bool{}
	for _, plan := range plans {
		if plan.skip != "" || plan.leg.LegID == "" || seen[plan.leg.LegID] {
			continue
		}
		seen[plan.leg.LegID] = true
		jobs = append(jobs, job{legID: plan.leg.LegID, contract: plan.leg.Contract})
	}
	out := make(map[string]purgeQuoteResult, len(jobs))
	var mu sync.Mutex
	runBounded(jobs, purgeQuoteWorkers, func(j job) {
		quote, err := s.fetchPreviewQuote(ctx, j.contract, purgeQuoteTimeout)
		mu.Lock()
		out[j.legID] = purgeQuoteResult{quote: quote, err: err}
		mu.Unlock()
	})
	return out
}

func (s *Server) executePurgeLeg(ctx context.Context, binding brokerWriteTransactionBinding, status rpc.TradingStatus, quotes map[string]purgeQuoteResult, res *rpc.PurgeExecuteResult, plan purgeLegPlan, origin string) {
	leg := plan.leg
	if plan.warning != "" {
		res.Warnings = append(res.Warnings, plan.warning)
	}
	if plan.skip != "" {
		addPurgeSkipped(res, leg, plan.skip)
		return
	}
	contract := leg.Contract
	cached, ok := quotes[leg.LegID]
	quote, err := cached.quote, cached.err
	if !ok {
		quote, err = s.fetchPreviewQuote(ctx, contract, purgeQuoteTimeout)
	}
	if err != nil {
		addPurgeError(res, leg, "quote: "+err.Error())
		return
	}
	if reason := purgeQuoteSkipReason(quote); reason != "" {
		addPurgeSkipped(res, leg, reason)
		return
	}
	action := plan.action
	qty := plan.qty
	limit, err := purgeAggressiveLimit(action, contract, quote)
	if err != nil {
		addPurgeError(res, leg, "pricing: "+err.Error())
		return
	}
	orderRef := purgeOrderRef(s.orderNow())
	draft := rpc.OrderDraft{
		Action:     action,
		Contract:   contract,
		Quantity:   int(qty),
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: limit,
		TIF:        rpc.OrderTIFDay,
		Strategy:   "purge-aggressive-limit",
		OrderRef:   orderRef,
		OpenClose:  "C",
	}
	orderID, err := s.reserveBoundBrokerOrderID(ctx, binding)
	if err != nil {
		addPurgeError(res, leg, "reserve order id: "+err.Error())
		return
	}
	attempt := purgeJournalEventForDraft(draft, status, res.PurgeID, leg.LegID, orderID, s.orderNow())
	attempt.Origin = normalizedWriteOrigin(origin)
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		addPurgeError(res, leg, "broker write authority: "+err.Error())
		return
	}
	if err := s.orderJournal.StagePreTransmit("", "", 0, orderID, corestore.ActionPurge, coreOrderOrigin(origin), []orderJournalEvent{attempt}); err != nil {
		addPurgeError(res, leg, "append send journal: "+err.Error())
		return
	}

	brokerContract := previewIBKRContract(contract)
	order := previewIBKROrder(draft)
	order.OrderID = orderID
	order.ClientID = status.ClientID
	order.Account = status.Account
	if err := s.submitBoundConfiguredOrder(ctx, binding, status, brokerContract, order); err != nil {
		ev := purgeJournalEventForDraft(draft, status, res.PurgeID, leg.LegID, orderID, s.orderNow())
		ev.Type = orderJournalEventSendError
		ev.SendState = orderSendStateUncertainSend
		ev.Message = "purge broker send returned error; reconcile before reusing this intent: " + err.Error()
		if appendErr := s.orderJournal.Append(ev); appendErr != nil {
			res.Warnings = append(res.Warnings, "append purge send error: "+appendErr.Error())
		}
		res.ErrorLegs++
		res.Orders = append(res.Orders, purgeOrderResult(leg, draft, orderID, quote, orderSendStateUncertainSend, ev.Message))
		return
	}

	res.SubmittedLegs++
	res.Orders = append(res.Orders, purgeOrderResult(leg, draft, orderID, quote, orderSendStateSendAttempted, "purge broker placeOrder transmit attempted; waiting for broker lifecycle callback"))
}

func purgeOriginalSideForQuantity(qty float64) string {
	if qty < 0 {
		return purgeOriginalSideShort
	}
	return purgeOriginalSideLong
}

func purgeLedgerCoverageKey(legID, originalSide string) string {
	return strings.TrimSpace(legID) + "|" + strings.ToUpper(strings.TrimSpace(originalSide))
}

func purgeLegIDCandidates(values ...string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func purgeOpenOrderExists(openByLeg map[string][]rpc.OrderView, legIDs []string) bool {
	for _, legID := range legIDs {
		if len(openByLeg[legID]) > 0 {
			return true
		}
	}
	return false
}

func activePurgeLedgerCoveredQuantity(activeByLegSide map[string]float64, legIDs []string, originalSide string) float64 {
	var covered float64
	for _, legID := range legIDs {
		covered += activeByLegSide[purgeLedgerCoverageKey(legID, originalSide)]
	}
	return covered
}

func purgeExecuteLegLabel(leg rpc.PurgeExecuteLeg) string {
	if strings.EqualFold(leg.Contract.SecType, "OPT") {
		expiry := leg.Contract.Expiry
		if len(expiry) == 8 {
			expiry = expiry[2:]
		}
		return fmt.Sprintf("%s %s %s %.2f", leg.Contract.Symbol, expiry, leg.Contract.Right, leg.Contract.Strike)
	}
	if leg.Contract.Symbol != "" {
		return leg.Contract.Symbol
	}
	return leg.Symbol
}

func currentPurgePosition(positions []*ibkrlib.RawPosition, leg rpc.PurgeExecuteLeg) (rpc.ContractParams, float64, bool) {
	for _, pos := range positions {
		if pos == nil || !purgePositionMatchesLeg(*pos, leg) {
			continue
		}
		return contractParamsFromRawPosition(*pos, leg.Contract), pos.Position, true
	}
	return rpc.ContractParams{}, 0, false
}

func purgePositionMatchesLeg(pos ibkrlib.RawPosition, leg rpc.PurgeExecuteLeg) bool {
	if leg.Contract.ConID > 0 && pos.Contract.ConID > 0 {
		return leg.Contract.ConID == pos.Contract.ConID
	}
	secType := strings.ToUpper(strings.TrimSpace(leg.Contract.SecType))
	if secType == "" {
		secType = strings.ToUpper(strings.TrimSpace(leg.SecType))
	}
	posSecType := strings.ToUpper(strings.TrimSpace(pos.Contract.SecType))
	if secType == "STK" || secType == "ETF" || strings.EqualFold(leg.SecType, rpc.SecTypeStock) {
		if posSecType != "STK" && posSecType != "ETF" {
			return false
		}
		return purgeStockPositionMatches(pos.Contract, leg.Contract)
	}
	if secType != "OPT" || posSecType != "OPT" {
		return secType != "" &&
			strings.EqualFold(posSecType, secType) &&
			strings.EqualFold(pos.Contract.Symbol, leg.Contract.Symbol) &&
			optionalEqual(pos.Contract.Currency, leg.Contract.Currency) &&
			optionalEqual(pos.Contract.LocalSymbol, leg.Contract.LocalSymbol) &&
			optionalEqual(pos.Contract.TradingClass, leg.Contract.TradingClass)
	}
	return strings.EqualFold(pos.Contract.Symbol, leg.Contract.Symbol) &&
		strings.TrimSpace(pos.Contract.Expiry) == strings.TrimSpace(leg.Contract.Expiry) &&
		samePreviewFloat(pos.Contract.Strike, leg.Contract.Strike) &&
		strings.EqualFold(pos.Contract.Right, leg.Contract.Right) &&
		optionalEqual(pos.Contract.Currency, leg.Contract.Currency) &&
		optionalEqual(pos.Contract.TradingClass, leg.Contract.TradingClass) &&
		optionalEqual(pos.Contract.LocalSymbol, leg.Contract.LocalSymbol) &&
		optionalMultiplierEqual(pos.Contract.Multiplier, leg.Contract.Multiplier)
}

func purgeStockPositionMatches(pos ibkrlib.Contract, want rpc.ContractParams) bool {
	if !strings.EqualFold(pos.Symbol, want.Symbol) {
		return false
	}
	if want.Currency != "" && pos.Currency != "" && !strings.EqualFold(pos.Currency, want.Currency) {
		return false
	}
	if want.LocalSymbol != "" && pos.LocalSymbol != "" && !strings.EqualFold(pos.LocalSymbol, want.LocalSymbol) {
		return false
	}
	if want.TradingClass != "" && pos.TradingClass != "" && !strings.EqualFold(pos.TradingClass, want.TradingClass) {
		return false
	}
	if want.PrimaryExch != "" && !stockVenueMatches(pos, want.PrimaryExch) {
		return false
	}
	if want.PrimaryExch == "" && want.Exchange != "" && !strings.EqualFold(want.Exchange, "SMART") && !stockVenueMatches(pos, want.Exchange) {
		return false
	}
	return true
}

func optionalEqual(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	return a == "" || b == "" || strings.EqualFold(a, b)
}

func optionalMultiplierEqual(a, b int) bool {
	return a <= 0 || b <= 0 || a == b
}

func contractParamsFromRawPosition(pos ibkrlib.RawPosition, fallback rpc.ContractParams) rpc.ContractParams {
	rawMultiplier := pos.Contract.Multiplier
	fallbackMultiplier := fallback.Multiplier
	c := rpc.ContractParams{
		ConID:        pos.Contract.ConID,
		Symbol:       strings.ToUpper(strings.TrimSpace(pos.Contract.Symbol)),
		SecType:      strings.ToUpper(strings.TrimSpace(pos.Contract.SecType)),
		Exchange:     strings.TrimSpace(pos.Contract.Exchange),
		PrimaryExch:  strings.TrimSpace(pos.Contract.PrimaryExch),
		Currency:     strings.ToUpper(strings.TrimSpace(pos.Contract.Currency)),
		LocalSymbol:  strings.TrimSpace(pos.Contract.LocalSymbol),
		TradingClass: strings.TrimSpace(pos.Contract.TradingClass),
		Expiry:       strings.TrimSpace(pos.Contract.Expiry),
		Strike:       pos.Contract.Strike,
		Right:        strings.ToUpper(strings.TrimSpace(pos.Contract.Right)),
	}
	if c.SecType == "" {
		c.SecType = fallback.SecType
	}
	if c.SecType == "" || c.SecType == rpc.SecTypeStock {
		c.SecType = "STK"
	}
	if c.SecType == rpc.SecTypeOption {
		c.SecType = "OPT"
	}
	if c.Exchange == "" {
		c.Exchange = fallback.Exchange
	}
	if c.Exchange == "" {
		c.Exchange = "SMART"
	}
	if c.Currency == "" {
		c.Currency = fallback.Currency
	}
	if c.Currency == "" {
		c.Currency = "USD"
	}
	normalisePositionStockRoute(&c)
	switch strings.ToUpper(strings.TrimSpace(c.SecType)) {
	case "STK", "ETF":
		c.Multiplier = 1
	case "OPT":
		if rawMultiplier > 0 {
			c.Multiplier = rawMultiplier
		} else if fallbackMultiplier > 0 {
			c.Multiplier = fallbackMultiplier
		} else {
			c.Multiplier = 100
		}
	default:
		if rawMultiplier > 0 {
			c.Multiplier = rawMultiplier
		} else if fallbackMultiplier > 0 {
			c.Multiplier = fallbackMultiplier
		} else {
			c.Multiplier = contractMultiplier(c)
		}
	}
	return c
}

func normalisePositionStockRoute(c *rpc.ContractParams) {
	if c == nil {
		return
	}
	switch strings.ToUpper(strings.TrimSpace(c.SecType)) {
	case "STK", "ETF":
	default:
		return
	}
	exchange := strings.ToUpper(strings.TrimSpace(c.Exchange))
	primary := strings.ToUpper(strings.TrimSpace(c.PrimaryExch))
	if primary == "" && exchange != "" && exchange != "SMART" {
		c.PrimaryExch = exchange
	}
	if exchange == "" || exchange != "SMART" {
		c.Exchange = "SMART"
	}
}

func purgeSideFlipped(originalSide string, currentQty float64) bool {
	switch strings.ToUpper(strings.TrimSpace(originalSide)) {
	case "LONG":
		return currentQty < 0
	case "SHORT":
		return currentQty > 0
	default:
		return false
	}
}

func purgeContractSupported(contract rpc.ContractParams) bool {
	switch strings.ToUpper(strings.TrimSpace(contract.SecType)) {
	case "STK", "ETF", "OPT":
		return true
	default:
		return false
	}
}

// purgeQuoteSkipReason reports why a snapshot is unsafe to price an
// aggressive close from, beyond the live-data-type check inside
// purgeAggressiveLimit: IsLiveDataType("") is true while a fresh
// subscription has not reported feed state yet, so an empty DataType proves
// nothing — the gateway's stale flag and the session calendar do.
func purgeQuoteSkipReason(quote rpc.OrderQuoteSnapshot) string {
	if quote.Stale {
		if reason := strings.TrimSpace(quote.StaleReason); reason != "" {
			return "quote is stale: " + reason
		}
		return "quote is stale"
	}
	if sc := quote.SessionContext; sc != nil && !sc.IsOpen {
		if reason := strings.TrimSpace(sc.Reason); reason != "" {
			return "market session is closed: " + reason
		}
		return "market session is closed"
	}
	return ""
}

func purgeAggressiveLimit(action string, contract rpc.ContractParams, quote rpc.OrderQuoteSnapshot) (float64, error) {
	if !rpc.IsLiveDataType(quote.DataType) {
		return 0, fmt.Errorf("requires live bid/ask data")
	}
	if quote.Bid == nil || quote.Ask == nil || *quote.Bid <= 0 || *quote.Ask <= *quote.Bid {
		return 0, fmt.Errorf("requires a positive two-sided bid/ask")
	}
	bid := *quote.Bid
	ask := *quote.Ask
	mid := (bid + ask) / 2
	tick := purgeQuoteTick(contract, bid, ask, mid)
	cushion := max(2*tick, 0.25*(ask-bid))
	steps := math.Ceil(cushion / tick)
	if steps < 1 {
		steps = 1
	}
	switch action {
	case rpc.OrderActionBuy:
		return roundPrice(ask + steps*tick), nil
	case rpc.OrderActionSell:
		price := bid - steps*tick
		if price < tick {
			return 0, fmt.Errorf("aggressive sell limit would be below minimum tick")
		}
		return roundPrice(price), nil
	default:
		return 0, fmt.Errorf("action must be BUY or SELL")
	}
}

func purgeQuoteTick(contract rpc.ContractParams, bid, ask, price float64) float64 {
	fallback := purgePriceTick(contract, price)
	if strings.EqualFold(contract.SecType, "OPT") {
		return fallback
	}
	spread := roundPrice(ask - bid)
	if spread > 0 && spread <= 0.1 {
		return spread
	}
	return fallback
}

func purgePriceTick(contract rpc.ContractParams, price float64) float64 {
	if contract.MinTick > 0 {
		return contract.MinTick
	}
	if strings.EqualFold(contract.SecType, "OPT") {
		return 0.01
	}
	return priceTick(price)
}

func purgeLegIDForContract(c rpc.ContractParams) string {
	return purgeLegIDForContractWithMultiplier(c, true)
}

func purgeLegacyLegIDForContract(c rpc.ContractParams) string {
	return purgeLegIDForContractWithMultiplier(c, false)
}

func purgeLegIDForContractWithMultiplier(c rpc.ContractParams, includeMultiplier bool) string {
	if c.ConID > 0 {
		parts := []string{
			strings.ToUpper(strings.TrimSpace(c.Symbol)),
			strings.ToUpper(strings.TrimSpace(c.SecType)),
			strconv.Itoa(c.ConID),
			strings.ToUpper(strings.TrimSpace(c.Currency)),
		}
		if includeMultiplier {
			parts = append(parts, strconv.Itoa(c.Multiplier))
		}
		sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
		return "leg_" + hex.EncodeToString(sum[:])[:12]
	}
	parts := []string{
		strings.ToUpper(strings.TrimSpace(c.Symbol)),
		strings.ToUpper(strings.TrimSpace(c.SecType)),
		strconv.Itoa(c.ConID),
		strings.ToUpper(strings.TrimSpace(c.Exchange)),
		strings.ToUpper(strings.TrimSpace(c.PrimaryExch)),
		strings.ToUpper(strings.TrimSpace(c.Currency)),
		strings.ToUpper(strings.TrimSpace(c.LocalSymbol)),
		strings.ToUpper(strings.TrimSpace(c.TradingClass)),
		strings.TrimSpace(c.Expiry),
		fmt.Sprintf("%.4f", c.Strike),
		strings.ToUpper(strings.TrimSpace(c.Right)),
	}
	if includeMultiplier {
		parts = append(parts, strconv.Itoa(c.Multiplier))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return "leg_" + hex.EncodeToString(sum[:])[:12]
}

func purgeOrderRef(now time.Time) string {
	tokenID, err := randomTokenID()
	if err != nil {
		return "purge-" + now.UTC().Format("20060102-150405")
	}
	return "purge-" + now.UTC().Format("20060102-150405") + "-" + tokenID[:8]
}

func purgeJournalEventForDraft(draft rpc.OrderDraft, status rpc.TradingStatus, purgeID, legID string, orderID int, at time.Time) orderJournalEvent {
	return orderJournalEvent{
		At:              at,
		Type:            orderJournalEventSendAttempted,
		OrderRef:        draft.OrderRef,
		ReservedOrderID: orderID,
		ClientID:        status.ClientID,
		Account:         status.Account,
		Endpoint:        status.Endpoint,
		Mode:            status.Mode,
		Source:          purgeExecuteSource,
		PurgeID:         purgeID,
		LegID:           legID,
		BypassPreview:   true,
		Symbol:          draft.Contract.Symbol,
		SecType:         draft.Contract.SecType,
		ConID:           draft.Contract.ConID,
		Exchange:        draft.Contract.Exchange,
		PrimaryExch:     draft.Contract.PrimaryExch,
		Currency:        draft.Contract.Currency,
		LocalSymbol:     draft.Contract.LocalSymbol,
		TradingClass:    draft.Contract.TradingClass,
		Expiry:          draft.Contract.Expiry,
		Strike:          draft.Contract.Strike,
		Right:           draft.Contract.Right,
		Multiplier:      draft.Contract.Multiplier,
		Action:          draft.Action,
		OrderType:       draft.OrderType,
		TIF:             draft.TIF,
		OutsideRTH:      draft.OutsideRTH,
		Quantity:        float64(draft.Quantity),
		LimitPrice:      draft.LimitPrice,
		OpenClose:       draft.OpenClose,
		SendState:       orderSendStateSendAttempted,
		Message:         "purge broker placeOrder transmit attempted",
	}
}

func purgeOrderResult(leg rpc.PurgeExecuteLeg, draft rpc.OrderDraft, orderID int, quote rpc.OrderQuoteSnapshot, sendState, message string) rpc.PurgeExecuteOrder {
	return rpc.PurgeExecuteOrder{
		LegID:           leg.LegID,
		Symbol:          draft.Contract.Symbol,
		SecType:         draft.Contract.SecType,
		Contract:        draft.Contract,
		Action:          draft.Action,
		Quantity:        draft.Quantity,
		LimitPrice:      draft.LimitPrice,
		OrderRef:        draft.OrderRef,
		ReservedOrderID: orderID,
		LifecycleStatus: rpc.OrderLifecyclePendingSubmit,
		SendState:       sendState,
		Message:         message,
		Quote:           quote,
	}
}

func addPurgeSkipped(res *rpc.PurgeExecuteResult, leg rpc.PurgeExecuteLeg, reason string) {
	res.SkippedLegs++
	res.Skipped = append(res.Skipped, rpc.PurgeExecuteSkippedLeg{
		LegID:    leg.LegID,
		Symbol:   leg.Symbol,
		SecType:  leg.SecType,
		Contract: leg.Contract,
		Reason:   reason,
	})
}

func addPurgeError(res *rpc.PurgeExecuteResult, leg rpc.PurgeExecuteLeg, reason string) {
	res.ErrorLegs++
	addPurgeSkipped(res, leg, reason)
}

func (s *Server) refreshPurgeOrderViews(res *rpc.PurgeExecuteResult) {
	views, _, err := s.loadOrderViews()
	if err != nil {
		res.Warnings = append(res.Warnings, "refresh order lifecycle: "+err.Error())
		return
	}
	for i := range res.Orders {
		for _, view := range views {
			if !orderViewMatchesID(view, res.Orders[i].OrderRef) && (res.Orders[i].ReservedOrderID == 0 || view.ReservedOrderID != res.Orders[i].ReservedOrderID) {
				continue
			}
			res.Orders[i].Status = view.Status
			res.Orders[i].LifecycleStatus = view.LifecycleStatus
			res.Orders[i].SendState = view.SendState
			if view.LastMessage != "" {
				res.Orders[i].Message = view.LastMessage
			}
			break
		}
	}
}

func purgeExecuteFinalStatus(res rpc.PurgeExecuteResult) string {
	if res.ErrorLegs > 0 {
		if res.SubmittedLegs > 0 {
			return purgeExecuteStatusPartial
		}
		return purgeExecuteStatusError
	}
	for _, skipped := range res.Skipped {
		if !purgeExecuteSkippedReasonIsIdempotent(skipped.Reason) {
			if res.SubmittedLegs > 0 {
				return purgeExecuteStatusPartial
			}
			return purgeExecuteStatusBlocked
		}
	}
	if res.SubmittedLegs > 0 {
		return purgeExecuteStatusSubmitted
	}
	return purgeExecuteStatusFlat
}

func purgeExecuteSkippedReasonIsIdempotent(reason string) bool {
	switch reason {
	case "already flat",
		"open purge/restore order exists for this ledger row",
		"current quantity already covered by active purge ledger":
		return true
	default:
		return false
	}
}

func purgeExecuteMessage(res rpc.PurgeExecuteResult) string {
	switch res.Status {
	case purgeExecuteStatusSubmitted:
		return fmt.Sprintf("submitted %d purge order(s)", res.SubmittedLegs)
	case purgeExecuteStatusFlat:
		if len(res.Skipped) > 0 {
			return "selected purge legs are already flat or already covered by active purge state"
		}
		return "selected purge legs are already flat"
	case purgeExecuteStatusPartial:
		return fmt.Sprintf("submitted %d purge order(s); %d leg(s) need attention", res.SubmittedLegs, res.SkippedLegs)
	case purgeExecuteStatusError:
		return "purge execution failed before any successful submit"
	default:
		return "purge execution is blocked"
	}
}
