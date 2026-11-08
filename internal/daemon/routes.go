package daemon

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

// route serves one unary method: call returns what unary writes, and
// capability names its row in the coverage table (coverage.go).
type route struct {
	call       func(ctx context.Context, req *rpc.Request) (any, error)
	capability string
}

// serve routes method m, part of capability, to h. The compiler checks h's result against the
// result m declares, so the daemon and its clients cannot disagree on it.
func serve[P, R any](_ rpc.Method[P, R], capability string, h func(context.Context, *rpc.Request) (*R, error)) route {
	return route{func(ctx context.Context, req *rpc.Request) (any, error) { return h(ctx, req) }, capability}
}

// serveValue is serve for a handler that returns its result by value.
func serveValue[P, R any](_ rpc.Method[P, R], capability string, h func(context.Context, *rpc.Request) (R, error)) route {
	return route{func(ctx context.Context, req *rpc.Request) (any, error) { return h(ctx, req) }, capability}
}

// serveUnchecked routes a method whose result shape the request selects, so
// no single result type can be checked: account.summary and its settlement
// diagnostics.
func serveUnchecked[P, R any](_ rpc.Method[P, R], capability string, h func(context.Context, *rpc.Request) (any, error)) route {
	return route{h, capability}
}

// ctxOnly adapts a handler that reads no request.
func ctxOnly[R any](h func(context.Context) (R, error)) func(context.Context, *rpc.Request) (R, error) {
	return func(ctx context.Context, _ *rpc.Request) (R, error) { return h(ctx) }
}

// reqOnly adapts a handler that needs no context.
func reqOnly[R any](h func(*rpc.Request) (R, error)) func(context.Context, *rpc.Request) (R, error) {
	return func(_ context.Context, req *rpc.Request) (R, error) { return h(req) }
}

// noArgs adapts a handler that needs neither.
func noArgs[R any](h func() (R, error)) func(context.Context, *rpc.Request) (R, error) {
	return func(context.Context, *rpc.Request) (R, error) { return h() }
}

// cannotFail adapts a handler that needs neither and returns no error.
func cannotFail[R any](h func() R) func(context.Context, *rpc.Request) (R, error) {
	return func(context.Context, *rpc.Request) (R, error) { return h(), nil }
}

// cannotFailReq adapts a handler of the request alone that returns no error.
func cannotFailReq[R any](h func(*rpc.Request) R) func(context.Context, *rpc.Request) (R, error) {
	return func(_ context.Context, req *rpc.Request) (R, error) { return h(req), nil }
}

// route returns the route for a method name, building the table on first use.
func (s *Server) route(name string) (route, bool) {
	s.routesOnce.Do(func() { s.routes = s.routeTable() })
	r, ok := s.routes[name]
	return r, ok
}

// routeTable is every unary method the daemon serves, one line each, in the
// order of package api. The two streams, display.subscribe and
// quote.subscribe, are served by the connection and are not routes.
func (s *Server) routeTable() map[string]route {
	return map[string]route{
		api.AccountSummary.Name:     serveUnchecked(api.AccountSummary, "account", s.handleAccountSummaryRequest),
		api.PositionsRisk.Name:      serve(api.PositionsRisk, "positions", s.handlePositionsRisk),
		api.PositionsList.Name:      serve(api.PositionsList, "positions", s.handlePositionsList),
		api.AccountIntraday.Name:    serve(api.AccountIntraday, "daily P&L", s.handleAccountIntraday),
		api.PortfolioSnapshot.Name:  serve(api.PortfolioSnapshot, "portfolio allocation", ctxOnly(s.handlePortfolioSnapshot)),
		api.PortfolioPlan.Name:      serve(api.PortfolioPlan, "positions", s.handlePortfolioPlan),
		api.MarketSnapshot.Name:     serve(api.MarketSnapshot, "market overview", s.handleMarketSnapshot),
		api.MarketHistory.Name:      serve(api.MarketHistory, "price history", s.handleMarketHistory),
		api.MarketBond.Name:         serve(api.MarketBond, "bond lookup", s.handleMarketBond),
		api.MarketTape.Name:         serve(api.MarketTape, "market tape", s.handleMarketTape),
		api.QuoteSnapshot.Name:      serve(api.QuoteSnapshot, "quotes", s.handleQuoteSnapshot),
		api.ChainFetch.Name:         serve(api.ChainFetch, "option chains", s.handleChainFetch),
		api.ChainExpiries.Name:      serve(api.ChainExpiries, "option chains", s.handleChainExpiries),
		api.ChainsRecorded.Name:     serve(api.ChainsRecorded, "recorded option chains", s.handleChainsRecorded),
		api.ChainsRecordedRows.Name: serve(api.ChainsRecordedRows, "recorded option chains", s.handleChainsRecordedRows),
		api.Technical.Name:          serve(api.Technical, "technical analysis", s.handleTechnical),
		api.AddCallPlan.Name:        serve(api.AddCallPlan, "stock add plans", s.handleAddCallPlan),
		api.AddMarket.Name:          serve(api.AddMarket, "option chains", s.handleAddMarket),
		api.AddPlan.Name:            serve(api.AddPlan, "stock add plans", s.handleAddPlan),
		api.AddPreview.Name:         serve(api.AddPreview, "orders", s.handleAddPreview),
		api.SetupsEvaluate.Name:     serve(api.SetupsEvaluate, "setups", s.handleSetupsEvaluate),
		api.SetupsOptions.Name:      serve(api.SetupsOptions, "option chains", s.handleSetupOptions),
		api.SetupsCoverage.Name:     serve(api.SetupsCoverage, "setups", s.handleSetupsCoverage),
		api.SetupsMarkouts.Name:     serve(api.SetupsMarkouts, "setups", s.handleSetupMarkouts),
		api.MacroSnapshot.Name: serveValue(api.MacroSnapshot, "macro", func(_ context.Context, req *rpc.Request) (rpc.MacroSnapshotResult, error) {
			return s.handleMacroRequest(*req)
		}),
		api.MarketCalendar.Name:       serve(api.MarketCalendar, "market calendar and events", reqOnly(s.handleMarketCalendar)),
		api.BreadthSPX.Name:           serve(api.BreadthSPX, "breadth", s.handleBreadthSPX),
		api.GammaZeroSPX.Name:         serve(api.GammaZeroSPX, "gamma", s.handleGammaZeroSPX),
		api.RegimeSnapshot.Name:       serve(api.RegimeSnapshot, "regime", s.handleRegimeSnapshot),
		api.StressSnapshot.Name:       serve(api.StressSnapshot, "portfolio stress", s.handleStressSnapshot),
		api.ScorecardRead.Name:        serve(api.ScorecardRead, "rules, mandate and risk policy", s.handleScorecardRead),
		api.MandateRisk.Name:          serve(api.MandateRisk, "rules, mandate and risk policy", s.handleMandateRisk),
		api.CoreTarget.Name:           serve(api.CoreTarget, "core sleeve", s.handleCoreTarget),
		api.StrategyTarget.Name:       serve(api.StrategyTarget, "strategy", s.handleStrategyTarget),
		api.StrategyOne.Name:          serve(api.StrategyOne, "strategy", ctxOnly(s.handleStrategyOne)),
		api.StrategyReplay.Name:       serve(api.StrategyReplay, "strategy", s.handleStrategyReplay),
		api.CoreResolve.Name:          serve(api.CoreResolve, "core listing lookup", s.handleCoreResolve),
		api.AlertCandidates.Name:      serve(api.AlertCandidates, "alerts", s.handleAlertCandidates),
		api.AlertStatus.Name:          serve(api.AlertStatus, "alerts", s.handleAlertStatus),
		api.AlertDeliveryProof.Name:   serve(api.AlertDeliveryProof, "alerts", s.handleAlertDeliveryProof),
		api.RulesHistory.Name:         serve(api.RulesHistory, "rules, mandate and risk policy", s.handleRulesHistory),
		api.ReconEquity.Name:          serve(api.ReconEquity, "reconciliation", s.handleReconEquity),
		api.ReportingStatus.Name:      serve(api.ReportingStatus, "statement reporting", ctxOnly(s.handleReportingStatus)),
		api.FX.Name:                   serve(api.FX, "FX attribution", ctxOnly(s.handleFX)),
		api.FXBackfill.Name:           serve(api.FXBackfill, "FX attribution", ctxOnly(s.handleFXBackfill)),
		api.ReportingPerformance.Name: serve(api.ReportingPerformance, "statement reporting", ctxOnly(s.handleReportingPerformance)),
		api.ReportingCosts.Name:       serve(api.ReportingCosts, "statement reporting", s.handleReportingCosts),
		api.ReportingValidate.Name:    serve(api.ReportingValidate, "statement reporting", s.handleReportingValidate),
		api.EdgeSnapshot.Name:         serve(api.EdgeSnapshot, "Edge", s.handleEdgeSnapshot),
		api.EdgeReplay.Name:           serve(api.EdgeReplay, "Edge", s.handleEdgeReplay),
		api.ShortInterestScreen.Name:  serve(api.ShortInterestScreen, "borrow availability", s.handleShortInterestScreen),
		api.LendingMarket.Name:        serve(api.LendingMarket, "borrow availability", s.handleLendingMarket),
		api.LendingScreen.Name:        serve(api.LendingScreen, "borrow availability", s.handleLendingScreen),
		api.FinancingFees.Name:        serve(api.FinancingFees, "borrow availability", s.handleFinancingFees),
		api.MarketEventsSnapshot.Name: serve(api.MarketEventsSnapshot, "market calendar and events", s.handleMarketEventsSnapshot),
		api.DataHealth.Name: serveValue(api.DataHealth, "status and data health", func(_ context.Context, req *rpc.Request) (rpc.DataHealthResult, error) {
			p, err := dataHealthParams(req)
			if err != nil {
				return rpc.DataHealthResult{}, err
			}
			return s.handleDataHealth(p)
		}),
		api.DataCheck.Name:                            serveValue(api.DataCheck, "status and data health", noArgs(s.requestDataHealthCheck)),
		api.StatusHealth.Name:                         serve(api.StatusHealth, "status and data health", s.handleStatusHealthRequest),
		api.TradingStatus.Name:                        serve(api.TradingStatus, "settings", cannotFail(s.handleTradingStatus)),
		api.AutoTradeStatus.Name:                      serve(api.AutoTradeStatus, "proposals", cannotFail(s.handleAutoTradeStatus)),
		api.RulesSnapshot.Name:                        serve(api.RulesSnapshot, "rules, mandate and risk policy", s.handleRulesSnapshot),
		api.BriefSnapshot.Name:                        serve(api.BriefSnapshot, "brief", s.handleBriefSnapshot),
		api.NudgesSnapshot.Name:                       serve(api.NudgesSnapshot, "brief", s.handleNudgesSnapshot),
		api.RiskPolicySnapshot.Name:                   serve(api.RiskPolicySnapshot, "rules, mandate and risk policy", s.handleRiskPolicySnapshot),
		api.RiskPolicyCapitalEvent.Name:               serve(api.RiskPolicyCapitalEvent, "rules, mandate and risk policy", s.handleRiskPolicyCapitalEvent),
		api.RiskPolicyOverride.Name:                   serve(api.RiskPolicyOverride, "rules, mandate and risk policy", s.handleRiskPolicyOverride),
		api.RiskPolicyResetDrawdown.Name:              serve(api.RiskPolicyResetDrawdown, "rules, mandate and risk policy", s.handleRiskPolicyResetDrawdown),
		api.RiskPolicyCorrectPeak.Name:                serve(api.RiskPolicyCorrectPeak, "rules, mandate and risk policy", s.handleRiskPolicyCorrectPeak),
		api.ReconSnapshot.Name:                        serve(api.ReconSnapshot, "reconciliation", s.handleReconSnapshot),
		api.ReconStatus.Name:                          serve(api.ReconStatus, "reconciliation", s.handleReconStatus),
		api.ReconCheck.Name:                           serve(api.ReconCheck, "reconciliation", s.handleReconCheck),
		api.ReconBacktest.Name:                        serve(api.ReconBacktest, "reconciliation", s.handleReconBacktest),
		api.ReconDismiss.Name:                         serve(api.ReconDismiss, "reconciliation", s.handleReconDismiss),
		api.TradeProposalsSnapshot.Name:               serve(api.TradeProposalsSnapshot, "proposals", cannotFailReq(s.handleTradeProposalsSnapshot)),
		api.TradeProposalsRefresh.Name:                serve(api.TradeProposalsRefresh, "proposals", s.handleTradeProposalsRefresh),
		api.TradeProposalsPrepare.Name:                serve(api.TradeProposalsPrepare, "proposals", s.handleTradeProposalsPrepare),
		api.TradeProposalsPreparedStatus.Name:         serve(api.TradeProposalsPreparedStatus, "proposals", s.handleTradeProposalsPreparedStatus),
		api.TradeProposalsPrepareBundle.Name:          serve(api.TradeProposalsPrepareBundle, "proposals", s.handleTradeProposalsPrepareBundle),
		api.TradeProposalsSubmitBundle.Name:           serve(api.TradeProposalsSubmitBundle, "orders", s.handleTradeProposalsSubmitBundle),
		api.TradeProposalsPreparedBundleStatus.Name:   serve(api.TradeProposalsPreparedBundleStatus, "proposals", s.handleTradeProposalsPreparedBundleStatus),
		api.TradeProposalsPreview.Name:                serve(api.TradeProposalsPreview, "orders", s.handleTradeProposalsPreview),
		api.TradeProposalsSubmit.Name:                 serve(api.TradeProposalsSubmit, "orders", s.handleTradeProposalsSubmit),
		api.TradeProposalsIgnore.Name:                 serve(api.TradeProposalsIgnore, "proposals", cannotFailReq(s.handleTradeProposalsIgnore)),
		api.TradeProposalsVeto.Name:                   serve(api.TradeProposalsVeto, "proposals", s.handleTradeProposalsVeto),
		api.TradeProposalsVetoWindow.Name:             serve(api.TradeProposalsVetoWindow, "proposals", s.handleTradeProposalsVeto),
		api.TradeProposalsQueuePrepare.Name:           serve(api.TradeProposalsQueuePrepare, "proposals", s.handleTradeProposalsQueuePrepare),
		api.TradeProposalsQueueArm.Name:               serve(api.TradeProposalsQueueArm, "proposals", s.handleTradeProposalsQueueArm),
		api.TradeProposalsQueueCancel.Name:            serve(api.TradeProposalsQueueCancel, "proposals", s.handleTradeProposalsQueueCancel),
		api.TradeProposalsQueueList.Name:              serve(api.TradeProposalsQueueList, "proposals", reqOnly(s.handleTradeProposalsQueueList)),
		api.TradeProposalsQueueStatus.Name:            serve(api.TradeProposalsQueueStatus, "proposals", reqOnly(s.handleTradeProposalsQueueStatus)),
		api.TradeProposalsRequestStop.Name:            serve(api.TradeProposalsRequestStop, "orders", s.handleTradeProposalsRequestStop),
		api.TradeProposalsReducePreview.Name:          serve(api.TradeProposalsReducePreview, "orders", s.handleTradeProposalsReducePreview),
		api.TradeProposalsReduceSubmit.Name:           serve(api.TradeProposalsReduceSubmit, "orders", s.handleTradeProposalsReduceSubmit),
		api.TradeProposalsReducePortfolioPreview.Name: serve(api.TradeProposalsReducePortfolioPreview, "orders", s.handleTradeProposalsReducePortfolioPreview),
		api.TradeProposalsReducePortfolioSubmit.Name:  serve(api.TradeProposalsReducePortfolioSubmit, "orders", s.handleTradeProposalsReducePortfolioSubmit),
		api.OpportunitiesExercises.Name:               serve(api.OpportunitiesExercises, "option exercise", ctxOnly(s.handleOpportunitiesExercises)),
		api.OpportunitiesReconcileExercise.Name:       serve(api.OpportunitiesReconcileExercise, "option exercise", s.handleOpportunitiesReconcileExercise),
		api.OpportunitiesStatus.Name:                  serve(api.OpportunitiesStatus, "option exercise", cannotFail(s.handleOpportunitiesStatus)),
		api.OpportunitiesSnapshot.Name:                serve(api.OpportunitiesSnapshot, "option exercise", cannotFailReq(s.handleOpportunitiesSnapshot)),
		api.OpportunitiesRefresh.Name:                 serve(api.OpportunitiesRefresh, "option exercise", s.handleOpportunitiesRefresh),
		api.OpportunitiesPreviewExercise.Name:         serve(api.OpportunitiesPreviewExercise, "orders", s.handleOpportunitiesPreviewExercise),
		api.OpportunitiesSubmitExercise.Name:          serve(api.OpportunitiesSubmitExercise, "orders", s.handleOpportunitiesSubmitExercise),
		api.OpportunitiesIgnore.Name:                  serve(api.OpportunitiesIgnore, "option exercise", cannotFailReq(s.handleOpportunitiesIgnore)),
		api.HoldingLocksList.Name:                     serve(api.HoldingLocksList, "watchlist and holding locks", s.handleHoldingLocksList),
		api.HoldingLocksReplace.Name:                  serve(api.HoldingLocksReplace, "watchlist and holding locks", s.handleHoldingLocksReplace),
		api.WatchlistList.Name:                        serve(api.WatchlistList, "watchlist and holding locks", ctxOnly(s.handleWatchlistList)),
		api.WatchlistReplace.Name:                     serve(api.WatchlistReplace, "watchlist and holding locks", s.handleWatchlistMutation),
		api.WatchlistAdd.Name:                         serve(api.WatchlistAdd, "watchlist and holding locks", s.handleWatchlistMutation),
		api.WatchlistRemove.Name:                      serve(api.WatchlistRemove, "watchlist and holding locks", s.handleWatchlistMutation),
		api.CashSweepPreferencesGet.Name:              serve(api.CashSweepPreferencesGet, "cash policy", ctxOnly(s.handleCashSweepPreferencesContext)),
		api.CashSweepPrioritySet.Name:                 serve(api.CashSweepPrioritySet, "cash policy", s.handleCashSweepPrioritySet),
		api.DeskExecutionCapabilities.Name:            serveValue(api.DeskExecutionCapabilities, "Desk authority and execution", cannotFail(s.handleDeskExecutionCapabilities)),
		api.DeskExecutionLookup.Name:                  serveValue(api.DeskExecutionLookup, "Desk authority and execution", s.handleDeskExecutionLookup),
		api.DeskExecutionSubmit.Name:                  serveValue(api.DeskExecutionSubmit, "orders", s.handleDeskExecutionSubmit),
		api.DeskAuthorityStatus.Name:                  serveValue(api.DeskAuthorityStatus, "Desk authority and execution", ctxOnly(s.handleDeskAuthorityStatus)),
		api.DeskAuthorityPrepare.Name:                 serveValue(api.DeskAuthorityPrepare, "Desk authority and execution", s.handleDeskAuthorityPrepare),
		api.DeskAuthorityConfirm.Name:                 serveValue(api.DeskAuthorityConfirm, "Desk authority and execution", s.handleDeskAuthorityConfirm),
		api.DeskAuthorityControl.Name:                 serveValue(api.DeskAuthorityControl, "Desk authority and execution", s.handleDeskAuthorityControl),
		api.DeskOverridePrepare.Name:                  serveValue(api.DeskOverridePrepare, "Desk authority and execution", s.handleDeskOverridePrepare),
		api.DeskOverrideConfirm.Name:                  serve(api.DeskOverrideConfirm, "Desk authority and execution", s.handleDeskOverrideConfirm),
		api.CashPolicyGet.Name:                        serve(api.CashPolicyGet, "cash policy", s.handleCashPolicyGet),
		api.CashPolicyCheck.Name:                      serve(api.CashPolicyCheck, "cash policy", s.handleCashPolicyCheck),
		api.CashPolicyApply.Name:                      serve(api.CashPolicyApply, "cash policy", s.handleCashPolicyApply),
		api.SettingsGet.Name:                          serve(api.SettingsGet, "settings", noArgs(s.handleSettingsGet)),
		api.SettingsUpdate.Name:                       serve(api.SettingsUpdate, "settings", s.handleSettingsUpdate),
		api.OrdersOpen.Name:                           serve(api.OrdersOpen, "order history", s.handleOrdersOpen),
		api.OrdersHistory.Name:                        serve(api.OrdersHistory, "order history", s.handleOrdersHistory),
		api.OrderStatus.Name:                          serve(api.OrderStatus, "order history", s.handleOrderStatus),
		api.OrderPreview.Name:                         serve(api.OrderPreview, "orders", s.handleOrderPreview),
		api.StrategyPreview.Name:                      serve(api.StrategyPreview, "orders", s.handleStrategyPreview),
		api.OrderPlace.Name:                           serve(api.OrderPlace, "orders", s.handleOrderPlace),
		api.OrderModify.Name:                          serve(api.OrderModify, "orders", s.handleOrderModify),
		api.OrderCancel.Name:                          serve(api.OrderCancel, "orders", s.handleOrderCancel),
		api.OrderTWSCancel.Name:                       serve(api.OrderTWSCancel, "orders", s.handleOrderTWSCancel),
		api.OrderTWSModify.Name:                       serve(api.OrderTWSModify, "orders", s.handleOrderTWSModify),
	}
}
