package daemon

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/osauer/canary/v2/internal/config"
	"github.com/osauer/canary/v2/internal/daemon/corestore"
	"github.com/osauer/canary/v2/internal/rpc"
	ibkrlib "github.com/osauer/canary/v2/pkg/ibkr"
)

const (
	purgeRestoreStatusPreview   = "preview"
	purgeRestoreStatusBlocked   = "blocked"
	purgeRestoreStatusFlat      = "flat"
	purgeRestoreStatusSubmitted = "submitted"
	purgeRestoreStatusPartial   = "partial"
	purgeRestoreStatusError     = "error"
)

type purgeRestoreSubmission struct {
	leg     rpc.PurgeRestoreLeg
	draft   rpc.OrderDraft
	orderID int
}

func (s *Server) handlePurgeRestorePreview(ctx context.Context, req *rpc.Request) (*rpc.PurgeRestoreResult, error) {
	var p rpc.PurgeRestoreParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	return s.previewPurgeRestore(ctx, p)
}

func (s *Server) handlePurgeRestoreExecute(ctx context.Context, req *rpc.Request) (*rpc.PurgeRestoreResult, error) {
	var p rpc.PurgeRestoreParams
	if err := decodeParams(req.Params, &p); err != nil {
		return nil, err
	}
	s.brokerWriteMu.Lock()
	defer s.brokerWriteMu.Unlock()
	return s.executePurgeRestore(ctx, p)
}

func (s *Server) previewPurgeRestore(ctx context.Context, p rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error) {
	return s.buildPurgeRestore(ctx, p, false)
}

func (s *Server) executePurgeRestore(ctx context.Context, p rpc.PurgeRestoreParams) (*rpc.PurgeRestoreResult, error) {
	res, err := s.buildPurgeRestore(ctx, p, true)
	if err != nil || res == nil {
		return res, err
	}
	if res.Status != purgeRestoreStatusPreview {
		return res, nil
	}
	auth, binding, bindingErr := s.authorizeBrokerWriteTransaction(p.Origin, false)
	if !auth.Allowed {
		res.Status = purgeRestoreStatusBlocked
		res.Blockers = append([]rpc.TradingBlocker(nil), auth.Blockers...)
		res.Message = firstTradingBlockerMessage(res.Blockers)
		return res, nil
	}
	if bindingErr != nil {
		res.Status = purgeRestoreStatusError
		res.ErrorLegs = max(1, len(res.Legs))
		res.Message = bindingErr.Error()
		return res, nil
	}
	status := auth.Status
	if !strings.EqualFold(res.Mode, status.Mode) || !strings.EqualFold(res.Account, status.Account) ||
		res.Endpoint != status.Endpoint || res.ClientID != status.ClientID {
		res.Status = purgeRestoreStatusError
		res.ErrorLegs = max(1, len(res.Legs))
		res.Message = brokerWriteTransactionDriftError().Error()
		return res, nil
	}
	wait := time.Duration(p.WaitMs) * time.Millisecond
	if wait <= 0 {
		wait = 2 * time.Second
	}
	if wait > 10*time.Second {
		wait = 10 * time.Second
	}
	submissions := make([]purgeRestoreSubmission, 0, len(res.Legs))
	attempts := make([]orderJournalEvent, 0, len(res.Legs))
	maxOrderID := 0
	for _, leg := range res.Legs {
		orderID, err := s.reserveBoundBrokerOrderID(ctx, binding)
		if err != nil {
			res.Status = purgeRestoreStatusError
			res.ErrorLegs++
			res.Message = "reserve restore order id: " + err.Error()
			return res, nil
		}
		draft := rpc.OrderDraft{
			Action:     leg.Action,
			Contract:   leg.Contract,
			Quantity:   leg.Quantity,
			OrderType:  rpc.OrderTypeLMT,
			LimitPrice: leg.LimitPrice,
			TIF:        rpc.OrderTIFDay,
			Strategy:   rpc.OrderStrategyPatientLimit,
			OrderRef:   purgeRestoreOrderRef(s.orderNow()),
			OpenClose:  orderOpenCloseForEffect(leg.Position.Effect),
		}
		submissions = append(submissions, purgeRestoreSubmission{leg: leg, draft: draft, orderID: orderID})
		attempt := restoreJournalEventForDraft(draft, status, res.PurgeID, leg.LegID, orderID, s.orderNow())
		attempt.Origin = normalizedWriteOrigin(p.Origin)
		attempts = append(attempts, attempt)
		maxOrderID = max(maxOrderID, orderID)
	}
	if err := s.requireBrokerWriteTransactionCurrent(binding); err != nil {
		res.Status = purgeRestoreStatusError
		res.ErrorLegs = len(submissions)
		res.Message = err.Error()
		return res, nil
	}
	if err := s.orderJournal.StagePreTransmit("", "", 0, maxOrderID, corestore.ActionRestore, coreOrderOrigin(p.Origin), attempts); err != nil {
		res.Status = purgeRestoreStatusError
		res.ErrorLegs = len(submissions)
		res.Message = "append restore send journal: " + err.Error()
		return res, nil
	}
	for _, submission := range submissions {
		brokerContract := previewIBKRContract(submission.draft.Contract)
		order := previewIBKROrder(submission.draft)
		order.OrderID = submission.orderID
		order.ClientID = status.ClientID
		order.Account = status.Account
		if err := s.submitBoundConfiguredOrder(ctx, binding, status, brokerContract, order); err != nil {
			ev := restoreJournalEventForDraft(submission.draft, status, res.PurgeID, submission.leg.LegID, submission.orderID, s.orderNow())
			ev.Type = orderJournalEventSendError
			ev.SendState = orderSendStateUncertainSend
			ev.Message = "restore broker send returned error; purge ledger quantity unchanged: " + err.Error()
			if appendErr := s.orderJournal.Append(ev); appendErr != nil {
				res.Warnings = append(res.Warnings, "append restore send error: "+appendErr.Error())
			}
			res.ErrorLegs++
			res.Orders = append(res.Orders, purgeOrderResult(
				rpc.PurgeExecuteLeg{LegID: submission.leg.LegID, Symbol: submission.leg.Symbol, SecType: submission.leg.SecType, Contract: submission.leg.Contract},
				submission.draft,
				submission.orderID,
				submission.leg.Quote,
				orderSendStateUncertainSend,
				ev.Message,
			))
			continue
		}
		res.SubmittedLegs++
		res.Orders = append(res.Orders, purgeOrderResult(
			rpc.PurgeExecuteLeg{LegID: submission.leg.LegID, Symbol: submission.leg.Symbol, SecType: submission.leg.SecType, Contract: submission.leg.Contract},
			submission.draft,
			submission.orderID,
			submission.leg.Quote,
			orderSendStateSendAttempted,
			"restore broker placeOrder transmit attempted; purge ledger will change only from fills",
		))
	}
	if res.SubmittedLegs > 0 && wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			res.Warnings = append(res.Warnings, "lifecycle wait interrupted: "+ctx.Err().Error())
		}
		s.refreshPurgeRestoreOrderViews(res)
	}
	res.Status = purgeRestoreFinalStatus(*res)
	res.Message = purgeRestoreMessage(*res)
	rows, _, err := s.purgeLedger.Snapshot(brokerStateScope{Account: status.Account, Mode: status.Mode}, strings.TrimSpace(p.PurgeID))
	if err == nil {
		res.LedgerRows = rows
	}
	res.AsOf = s.orderNow()
	return res, nil
}

func (s *Server) buildPurgeRestore(ctx context.Context, p rpc.PurgeRestoreParams, execute bool) (*rpc.PurgeRestoreResult, error) {
	scale := p.Scale
	if scale == 0 {
		scale = 1
	}
	if scale < 0 || scale > 1 || math.IsNaN(scale) || math.IsInf(scale, 0) {
		return nil, errBadRequest("scale must be between 0 and 1")
	}
	status := s.currentTradingStatus()
	kind := "ibkr.purge_restore_preview"
	if execute {
		kind = "ibkr.purge_restore_execute"
	}
	res := &rpc.PurgeRestoreResult{
		Kind:     kind,
		PurgeID:  strings.TrimSpace(p.PurgeID),
		Status:   purgeRestoreStatusBlocked,
		Mode:     status.Mode,
		Account:  status.Account,
		Endpoint: status.Endpoint,
		ClientID: status.ClientID,
		Scale:    scale,
		AsOf:     s.orderNow(),
	}
	if res.PurgeID == "" {
		res.PurgeID = "active"
	}
	blockers := s.purgeRestorePreviewBlockers(status)
	if execute {
		blockers = s.purgeExecuteBlockers(status)
		for _, blocker := range liveOriginBlockers(status, p.Origin) {
			blockers = appendTradingBlockerOnce(blockers, blocker)
		}
	}
	if len(blockers) > 0 {
		res.Blockers = blockers
		res.Message = firstTradingBlockerMessage(blockers)
		return res, nil
	}
	if s.purgeLedger == nil {
		res.Blockers = append(res.Blockers, rpc.TradingBlocker{
			Code:    "purge_ledger_unavailable",
			Message: "purge restore requires a writable daemon purge ledger",
			Action:  "Fix the daemon state directory before restoring purged positions.",
		})
		res.Message = res.Blockers[0].Message
		return res, nil
	}

	rows, err := s.purgeLedger.AllRows()
	if err != nil {
		res.Status = purgeRestoreStatusError
		res.Message = "load purge ledger: " + err.Error()
		return res, nil
	}
	positions, err := s.refreshPurgePositions()
	if err != nil {
		res.Status = purgeRestoreStatusError
		res.Message = "refresh current positions: " + err.Error()
		return res, nil
	}
	positions = filterPurgePositionsForAccount(positions, status.Account)
	openByLeg, err := s.openPurgeOrdersByLeg(status.Account)
	if err != nil {
		res.Status = purgeRestoreStatusError
		res.Message = "load open purge orders: " + err.Error()
		return res, nil
	}
	cfg := s.effectiveTradingConfig()
	timeout := time.Duration(p.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	symbols := purgeExecuteSymbols(p.Symbols)
	if p.All {
		symbols = nil
	} else if len(symbols) == 0 {
		return nil, errBadRequest("restore requires a symbol or all=true")
	}
	selected := selectPurgeLedgerRows(rows, brokerStateScope{Account: status.Account, Mode: status.Mode}, p.PurgeID, symbols)
	res.SelectedLegs = len(selected)
	for _, row := range selected {
		res.LedgerRows = append(res.LedgerRows, purgeLedgerRowToRPC(row))
	}
	sortPurgeLedgerRows(res.LedgerRows)
	if len(selected) == 0 {
		res.Status = purgeRestoreStatusFlat
		res.Message = "no active purge ledger rows matched restore target"
		return res, nil
	}
	for _, row := range selected {
		if err := ctx.Err(); err != nil {
			res.Status = purgeRestoreStatusError
			res.Message = err.Error()
			return res, nil
		}
		s.addPurgeRestoreLeg(ctx, res, status, row, positions, openByLeg, cfg, timeout, scale)
	}
	if len(res.Blockers) > 0 || res.SkippedLegs > 0 || res.ErrorLegs > 0 {
		res.Status = purgeRestoreStatusBlocked
		if res.Message == "" {
			res.Message = purgeRestoreBlockedMessage(res, execute, scale)
		}
		return res, nil
	}
	res.Status = purgeRestoreStatusPreview
	res.Message = "restore preview accepted by broker WhatIf; no broker orders were submitted"
	if execute {
		res.Message = "restore execution preflight accepted; submitting selected legs"
	}
	return res, nil
}

func (s *Server) purgeRestorePreviewBlockers(status rpc.TradingStatus) []rpc.TradingBlocker {
	blockers := []rpc.TradingBlocker{purgeSubmissionUnavailableBlocker}
	if status.Mode == config.TradingModeDisabled {
		blockers = append(blockers, rpc.TradingBlocker{
			Code:    "trading_disabled",
			Message: "trading is disabled",
			Action:  `Set [trading].mode to "paper" or "live" before broker WhatIf preview.`,
		})
	}
	if !s.purgeRestoreEnabled() {
		blockers = append(blockers, rpc.TradingBlocker{
			Code:    "purge_restore_disabled",
			Message: "purge/restore actions are disabled in platform settings",
			Action:  "Run `canary settings set features.purge_restore.enabled=true` before using purge/restore.",
		})
	}
	for _, blocker := range status.Blockers {
		if blocker.Code == "order_journal_unavailable" {
			continue
		}
		blockers = append(blockers, blocker)
	}
	return blockers
}

func (s *Server) addPurgeRestoreLeg(ctx context.Context, res *rpc.PurgeRestoreResult, status rpc.TradingStatus, row purgeLedgerRow, positions []*ibkrlib.RawPosition, openByLeg map[string][]rpc.OrderView, cfg config.Trading, timeout time.Duration, scale float64) {
	normalizePurgeLedgerRow(&row)
	restoreContract := row.Contract
	normalisePositionStockRoute(&restoreContract)
	leg := rpc.PurgeRestoreLeg{
		LegID:           row.LegID,
		Symbol:          row.Symbol,
		SecType:         row.SecType,
		Contract:        restoreContract,
		Action:          row.RestoreAction,
		RemainingBefore: row.RemainingQuantity,
		Status:          purgeRestoreStatusBlocked,
		Warnings:        append([]string(nil), row.Warnings...),
	}
	if row.RemainingQuantity <= 0 {
		addPurgeRestoreSkipped(res, leg, "already restored")
		return
	}
	if open := openByLeg[row.LegID]; len(open) > 0 {
		addPurgeRestoreSkipped(res, leg, "open purge/restore order exists for this ledger row")
		return
	}
	qty, ok := restoreScaledQuantity(row.RemainingQuantity, scale)
	if !ok {
		addPurgeRestoreSkipped(res, leg, "restore quantity is fractional under the current integer order path")
		return
	}
	leg.Quantity = qty
	currentQty := positionQuantityForContract(positions, restoreContract)
	if purgeSideFlipped(row.OriginalSide, currentQty) {
		addPurgeRestoreSkipped(res, leg, "current portfolio side is opposite the purged original side")
		return
	}
	if currentPositionAlreadyRestored(row, currentQty) {
		addPurgeRestoreSkipped(res, leg, "current portfolio already contains restored quantity not reflected in purge ledger")
		return
	}
	quote, err := s.fetchPreviewQuote(ctx, restoreContract, timeout)
	if err != nil {
		addPurgeRestoreError(res, leg, "quote: "+err.Error())
		return
	}
	leg.Quote = quote
	if reason := purgeQuoteSkipReason(quote); reason != "" {
		addPurgeRestoreSkipped(res, leg, reason)
		return
	}
	limit, err := purgeAggressiveLimit(row.RestoreAction, restoreContract, quote)
	if err != nil {
		addPurgeRestoreError(res, leg, "pricing: "+err.Error())
		return
	}
	leg.LimitPrice = limit
	position := restorePositionImpact(restoreContract, currentQty, row.RestoreAction, leg.Quantity)
	leg.Position = position
	if restorePolicyBlocker(restoreContract, row.RestoreAction, position, cfg) != "" {
		addPurgeRestoreSkipped(res, leg, restorePolicyBlocker(restoreContract, row.RestoreAction, position, cfg))
		return
	}
	if strings.EqualFold(restoreContract.SecType, "OPT") && leg.Quantity > cfg.MaxOptionContracts {
		addPurgeRestoreSkipped(res, leg, fmt.Sprintf("option quantity %d exceeds [trading].max_option_contracts %d", leg.Quantity, cfg.MaxOptionContracts))
		return
	}
	estimated := float64(leg.Quantity) * limit * float64(contractMultiplier(restoreContract))
	if estimated > cfg.MaxNotional {
		addPurgeRestoreSkipped(res, leg, fmt.Sprintf("restore notional %.2f exceeds [trading].max_notional %.2f", estimated, cfg.MaxNotional))
		return
	}
	leg.EstimatedValue = estimated
	leg.ShadowPnL = purgeRestoreLegShadowPnL(row, limit, float64(leg.Quantity))
	draft := rpc.OrderDraft{
		Action:     row.RestoreAction,
		Contract:   restoreContract,
		Quantity:   leg.Quantity,
		OrderType:  rpc.OrderTypeLMT,
		LimitPrice: limit,
		TIF:        rpc.OrderTIFDay,
		Strategy:   "restore-aggressive-limit",
		OrderRef:   purgeRestoreOrderRef(s.orderNow()),
		OpenClose:  orderOpenCloseForEffect(position.Effect),
	}
	whatIf, err := s.fetchPreviewWhatIf(ctx, status, draft, timeout)
	if err != nil {
		addPurgeRestoreError(res, leg, "what-if: "+err.Error())
		return
	}
	leg.WhatIf = whatIf
	if whatIf.Status != rpc.OrderWhatIfStatusAccepted || whatIf.RequiredForSubmit {
		addPurgeRestoreSkipped(res, leg, "broker WhatIf did not accept restore draft")
		return
	}
	leg.Status = rpc.OrderWhatIfStatusAccepted
	res.EstimatedValue += leg.EstimatedValue
	res.ShadowPnL += leg.ShadowPnL
	res.Legs = append(res.Legs, leg)
}

func currentPositionAlreadyRestored(row purgeLedgerRow, currentQty float64) bool {
	if currentQty == 0 {
		return false
	}
	currentAbs := math.Abs(currentQty)
	if strings.EqualFold(row.OriginalSide, purgeOriginalSideLong) && currentQty > 0 {
		return currentAbs > row.RestoredQuantity+1e-9
	}
	if strings.EqualFold(row.OriginalSide, purgeOriginalSideShort) && currentQty < 0 {
		return currentAbs > row.RestoredQuantity+1e-9
	}
	return false
}

func restorePolicyBlocker(contract rpc.ContractParams, action string, position rpc.OrderPositionImpact, cfg config.Trading) string {
	switch {
	case strings.EqualFold(contract.SecType, "STK") && stockShortOrFlip(position.Effect) && !cfg.AllowStockShort:
		return "stock short/flip restore requires [trading].allow_stock_short = true"
	case strings.EqualFold(contract.SecType, "OPT") && optionSellToOpen(action, position.Effect) && !cfg.AllowOptionSellToOpen:
		return "option sell-to-open restore requires [trading].allow_option_sell_to_open = true"
	default:
		return ""
	}
}

func restorePositionImpact(contract rpc.ContractParams, before float64, action string, qty int) rpc.OrderPositionImpact {
	_ = contract
	delta := float64(qty)
	if action == rpc.OrderActionSell {
		delta = -delta
	}
	after := before + delta
	return rpc.OrderPositionImpact{
		Before: before,
		After:  after,
		Effect: classifyPositionEffect(before, after),
	}
}

func purgeRestoreLegShadowPnL(row purgeLedgerRow, restorePrice, qty float64) float64 {
	if row.PurgedQuantity <= 0 || row.PurgeValue <= 0 || restorePrice <= 0 || qty <= 0 {
		return 0
	}
	multiplier := float64(max(row.Multiplier, 1))
	purgeAvg := row.PurgeValue / (row.PurgedQuantity * multiplier)
	sign := 1.0
	if row.OriginalQuantity < 0 || strings.EqualFold(row.OriginalSide, purgeOriginalSideShort) {
		sign = -1
	}
	return (purgeAvg - restorePrice) * qty * multiplier * sign
}

func selectPurgeLedgerRows(rows []purgeLedgerRow, scope brokerStateScope, purgeID string, symbols []string) []purgeLedgerRow {
	symbolSet := map[string]bool{}
	for _, symbol := range symbols {
		symbolSet[strings.ToUpper(strings.TrimSpace(symbol))] = true
	}
	selected := make([]purgeLedgerRow, 0, len(rows))
	for _, row := range rows {
		normalizePurgeLedgerRow(&row)
		if row.RemainingQuantity <= 0 {
			continue
		}
		if !purgeLedgerRowMatchesBrokerScope(row, scope) {
			continue
		}
		if purgeID != "" && !strings.EqualFold(purgeID, "active") && !strings.EqualFold(row.PurgeID, purgeID) {
			continue
		}
		if len(symbolSet) > 0 && !symbolSet[strings.ToUpper(strings.TrimSpace(row.Symbol))] {
			continue
		}
		selected = append(selected, row)
	}
	slicesSortPurgeLedgerRows(selected)
	return selected
}

func slicesSortPurgeLedgerRows(rows []purgeLedgerRow) {
	sortable := make([]rpc.PurgeLedgerRow, 0, len(rows))
	for _, row := range rows {
		sortable = append(sortable, purgeLedgerRowToRPC(row))
	}
	sortPurgeLedgerRows(sortable)
	order := map[string]int{}
	for i, row := range sortable {
		order[row.LegID] = i
	}
	slices.SortStableFunc(rows, func(a, b purgeLedgerRow) int {
		return order[a.LegID] - order[b.LegID]
	})
}

func (s *Server) openPurgeOrdersByLeg(account string) (map[string][]rpc.OrderView, error) {
	views, _, err := s.loadOrderViews()
	if err != nil {
		return nil, err
	}
	out := map[string][]rpc.OrderView{}
	for _, view := range views {
		if !view.Open || view.LegID == "" {
			continue
		}
		if account != "" && view.Account != "" && !strings.EqualFold(view.Account, account) {
			continue
		}
		if !strings.EqualFold(view.Source, purgeExecuteSource) && !strings.EqualFold(view.Source, purgeRestoreSource) {
			continue
		}
		for _, legID := range purgeOrderViewLegIDCandidates(view) {
			out[legID] = append(out[legID], view)
		}
	}
	return out, nil
}

func purgeOrderViewLegIDCandidates(view rpc.OrderView) []string {
	contract := rpc.ContractParams{
		ConID:        view.ConID,
		Symbol:       view.Symbol,
		SecType:      view.SecType,
		Exchange:     view.Exchange,
		PrimaryExch:  view.PrimaryExch,
		Currency:     view.Currency,
		LocalSymbol:  view.LocalSymbol,
		TradingClass: view.TradingClass,
		Expiry:       view.Expiry,
		Strike:       view.Strike,
		Right:        view.Right,
		Multiplier:   view.Multiplier,
	}
	return purgeLegIDCandidates(view.LegID, purgeLegIDForContract(contract), purgeLegacyLegIDForContract(contract))
}

// restoreScaledQuantity returns the integer order quantity for a scaled
// restore and whether the product is usable. Float products like 100*0.07
// land at 7.000000000000001; values within 1e-9 of an integer snap to it
// before the fractional check.
func restoreScaledQuantity(remaining, scale float64) (int, bool) {
	qty := remaining * scale
	if rounded := math.Round(qty); math.Abs(qty-rounded) < 1e-9 {
		qty = rounded
	}
	if qty <= 0 || math.Trunc(qty) != qty {
		return 0, false
	}
	return int(qty), true
}

func addPurgeRestoreSkipped(res *rpc.PurgeRestoreResult, leg rpc.PurgeRestoreLeg, reason string) {
	res.SkippedLegs++
	res.Skipped = append(res.Skipped, rpc.PurgeExecuteSkippedLeg{
		LegID:    leg.LegID,
		Symbol:   leg.Symbol,
		SecType:  leg.SecType,
		Contract: leg.Contract,
		Reason:   reason,
	})
	res.Blockers = append(res.Blockers, rpc.TradingBlocker{
		Code:    "restore_leg_blocked",
		Message: purgeRestoreLegLabel(leg) + ": " + reason,
		Action:  "Resolve the blocker and rerun restore preview before execution.",
	})
	leg.Status = purgeRestoreStatusBlocked
	leg.Warnings = append(leg.Warnings, reason)
	res.Legs = append(res.Legs, leg)
}

func addPurgeRestoreError(res *rpc.PurgeRestoreResult, leg rpc.PurgeRestoreLeg, reason string) {
	res.ErrorLegs++
	addPurgeRestoreSkipped(res, leg, reason)
}

func purgeRestoreLegLabel(leg rpc.PurgeRestoreLeg) string {
	if strings.EqualFold(leg.Contract.SecType, "OPT") {
		expiry := leg.Contract.Expiry
		if len(expiry) == 8 {
			expiry = expiry[2:]
		}
		return fmt.Sprintf("%s %s %s %.2f", leg.Symbol, expiry, leg.Contract.Right, leg.Contract.Strike)
	}
	return leg.Symbol
}

func purgeRestoreOrderRef(now time.Time) string {
	tokenID, err := randomTokenID()
	if err != nil {
		return "restore-" + now.UTC().Format("20060102-150405")
	}
	return "restore-" + now.UTC().Format("20060102-150405") + "-" + tokenID[:8]
}

func restoreJournalEventForDraft(draft rpc.OrderDraft, status rpc.TradingStatus, purgeID, legID string, orderID int, at time.Time) orderJournalEvent {
	ev := purgeJournalEventForDraft(draft, status, purgeID, legID, orderID, at)
	ev.Source = purgeRestoreSource
	ev.BypassPreview = false
	ev.Message = "restore broker placeOrder transmit attempted"
	return ev
}

func (s *Server) refreshPurgeRestoreOrderViews(res *rpc.PurgeRestoreResult) {
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

func purgeRestoreFinalStatus(res rpc.PurgeRestoreResult) string {
	if res.ErrorLegs > 0 {
		if res.SubmittedLegs > 0 {
			return purgeRestoreStatusPartial
		}
		return purgeRestoreStatusError
	}
	if res.SubmittedLegs > 0 {
		return purgeRestoreStatusSubmitted
	}
	if res.SkippedLegs > 0 {
		return purgeRestoreStatusBlocked
	}
	return purgeRestoreStatusFlat
}

// purgeRestoreBlockedMessage names the invocation that still works when the
// all-or-nothing preflight blocks: restore is deliberately conservative (one
// bad leg blocks the whole batch so the book never partially re-enters), but
// the operator should not have to diff leg lists to find the clean subset.
// Only symbols whose legs ALL previewed clean are suggested — a symbol with
// one clean and one blocked leg would just block again.
func purgeRestoreBlockedMessage(res *rpc.PurgeRestoreResult, execute bool, scale float64) string {
	const base = "restore is blocked; no broker orders were submitted"
	blocked := map[string]bool{}
	cleanOrder := make([]string, 0, len(res.Legs))
	cleanSeen := map[string]bool{}
	for _, leg := range res.Legs {
		symbol := strings.ToUpper(strings.TrimSpace(leg.Symbol))
		if symbol == "" {
			continue
		}
		if leg.Status == rpc.OrderWhatIfStatusAccepted {
			if !cleanSeen[symbol] {
				cleanSeen[symbol] = true
				cleanOrder = append(cleanOrder, symbol)
			}
			continue
		}
		blocked[symbol] = true
	}
	symbols := make([]string, 0, len(cleanOrder))
	for _, symbol := range cleanOrder {
		if !blocked[symbol] {
			symbols = append(symbols, symbol)
		}
	}
	if len(symbols) == 0 {
		return base
	}
	legCount := 0
	for _, leg := range res.Legs {
		if leg.Status == rpc.OrderWhatIfStatusAccepted && !blocked[strings.ToUpper(strings.TrimSpace(leg.Symbol))] {
			legCount++
		}
	}
	cmd := "canary purge restore " + strings.Join(symbols, " ")
	if scale != 1 {
		cmd += fmt.Sprintf(" --scale %g", scale)
	}
	if execute {
		cmd += " --execute"
	}
	return fmt.Sprintf("%s; %d leg(s) previewed clean and can restore separately: `%s`", base, legCount, cmd)
}

func purgeRestoreMessage(res rpc.PurgeRestoreResult) string {
	switch res.Status {
	case purgeRestoreStatusSubmitted:
		return fmt.Sprintf("submitted %d restore order(s); purge ledger will change only after fills", res.SubmittedLegs)
	case purgeRestoreStatusPartial:
		return fmt.Sprintf("submitted %d restore order(s); %d leg(s) need attention", res.SubmittedLegs, res.SkippedLegs)
	case purgeRestoreStatusError:
		return "restore execution failed before any successful submit"
	case purgeRestoreStatusBlocked:
		return "restore execution is blocked"
	default:
		return "selected purge ledger rows are already restored"
	}
}
