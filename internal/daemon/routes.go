package daemon

import (
	"context"

	"github.com/osauer/canary/v2/internal/rpc"
	"github.com/osauer/canary/v2/internal/rpc/api"
)

// route serves one unary method; unary writes what it returns.
type route func(ctx context.Context, req *rpc.Request) (any, error)

// serve routes method m to h. The compiler checks h's result against the
// result m declares, so the daemon and its clients cannot disagree on it.
func serve[P, R any](_ rpc.Method[P, R], h func(context.Context, *rpc.Request) (*R, error)) route {
	return func(ctx context.Context, req *rpc.Request) (any, error) { return h(ctx, req) }
}

// serveValue is serve for a handler that returns its result by value.
func serveValue[P, R any](_ rpc.Method[P, R], h func(context.Context, *rpc.Request) (R, error)) route {
	return func(ctx context.Context, req *rpc.Request) (any, error) { return h(ctx, req) }
}

// serveUnchecked routes a method whose result shape the request selects, so
// no single result type can be checked: account.summary and its settlement
// diagnostics.
func serveUnchecked[P, R any](_ rpc.Method[P, R], h func(context.Context, *rpc.Request) (any, error)) route {
	return route(h)
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
		api.AccountSummary.Name:     serveUnchecked(api.AccountSummary, s.handleAccountSummaryRequest),
		api.PositionsRisk.Name:      serve(api.PositionsRisk, s.handlePositionsRisk),
		api.PositionsList.Name:      serve(api.PositionsList, s.handlePositionsList),
		api.AccountIntraday.Name:    serve(api.AccountIntraday, s.handleAccountIntraday),
		api.PortfolioSnapshot.Name:  serve(api.PortfolioSnapshot, ctxOnly(s.handlePortfolioSnapshot)),
		api.PortfolioPlan.Name:      serve(api.PortfolioPlan, s.handlePortfolioPlan),
		api.MarketSnapshot.Name:     serve(api.MarketSnapshot, s.handleMarketSnapshot),
		api.MarketHistory.Name:      serve(api.MarketHistory, s.handleMarketHistory),
		api.MarketBond.Name:         serve(api.MarketBond, s.handleMarketBond),
		api.MarketTape.Name:         serve(api.MarketTape, s.handleMarketTape),
		api.QuoteSnapshot.Name:      serve(api.QuoteSnapshot, s.handleQuoteSnapshot),
		api.ChainFetch.Name:         serve(api.ChainFetch, s.handleChainFetch),
		api.ChainExpiries.Name:      serve(api.ChainExpiries, s.handleChainExpiries),
		api.ChainsRecorded.Name:     serve(api.ChainsRecorded, s.handleChainsRecorded),
		api.ChainsRecordedRows.Name: serve(api.ChainsRecordedRows, s.handleChainsRecordedRows),
		api.Technical.Name:          serve(api.Technical, s.handleTechnical),
		api.AddCallPlan.Name:        serve(api.AddCallPlan, s.handleAddCallPlan),
		api.AddMarket.Name:          serve(api.AddMarket, s.handleAddMarket),
		api.AddPlan.Name:            serve(api.AddPlan, s.handleAddPlan),
		api.AddPreview.Name:         serve(api.AddPreview, s.handleAddPreview),
		api.SetupsEvaluate.Name:     serve(api.SetupsEvaluate, s.handleSetupsEvaluate),
		api.SetupsOptions.Name:      serve(api.SetupsOptions, s.handleSetupOptions),
		api.SetupsCoverage.Name:     serve(api.SetupsCoverage, s.handleSetupsCoverage),
		api.SetupsMarkouts.Name:     serve(api.SetupsMarkouts, s.handleSetupMarkouts),
		api.MacroSnapshot.Name: serveValue(api.MacroSnapshot, func(_ context.Context, req *rpc.Request) (rpc.MacroSnapshotResult, error) {
			return s.handleMacroRequest(*req)
		}),
		api.MarketCalendar.Name:       serve(api.MarketCalendar, reqOnly(s.handleMarketCalendar)),
		api.BreadthSPX.Name:           serve(api.BreadthSPX, s.handleBreadthSPX),
		api.GammaZeroSPX.Name:         serve(api.GammaZeroSPX, s.handleGammaZeroSPX),
		api.RegimeSnapshot.Name:       serve(api.RegimeSnapshot, s.handleRegimeSnapshot),
		api.StressSnapshot.Name:       serve(api.StressSnapshot, s.handleStressSnapshot),
		api.ScorecardRead.Name:        serve(api.ScorecardRead, s.handleScorecardRead),
		api.MandateRisk.Name:          serve(api.MandateRisk, s.handleMandateRisk),
		api.CoreTarget.Name:           serve(api.CoreTarget, s.handleCoreTarget),
		api.StrategyTarget.Name:       serve(api.StrategyTarget, s.handleStrategyTarget),
		api.StrategyOne.Name:          serve(api.StrategyOne, ctxOnly(s.handleStrategyOne)),
		api.StrategyReplay.Name:       serve(api.StrategyReplay, s.handleStrategyReplay),
		api.CoreResolve.Name:          serve(api.CoreResolve, s.handleCoreResolve),
		api.AlertCandidates.Name:      serve(api.AlertCandidates, s.handleAlertCandidates),
		api.AlertStatus.Name:          serve(api.AlertStatus, s.handleAlertStatus),
		api.AlertDeliveryProof.Name:   serve(api.AlertDeliveryProof, s.handleAlertDeliveryProof),
		api.RulesHistory.Name:         serve(api.RulesHistory, s.handleRulesHistory),
		api.ReconEquity.Name:          serve(api.ReconEquity, s.handleReconEquity),
		api.ReportingStatus.Name:      serve(api.ReportingStatus, ctxOnly(s.handleReportingStatus)),
		api.FX.Name:                   serve(api.FX, ctxOnly(s.handleFX)),
		api.FXBackfill.Name:           serve(api.FXBackfill, ctxOnly(s.handleFXBackfill)),
		api.ReportingPerformance.Name: serve(api.ReportingPerformance, ctxOnly(s.handleReportingPerformance)),
		api.ReportingCosts.Name:       serve(api.ReportingCosts, s.handleReportingCosts),
		api.ReportingValidate.Name:    serve(api.ReportingValidate, s.handleReportingValidate),
		api.EdgeSnapshot.Name:         serve(api.EdgeSnapshot, s.handleEdgeSnapshot),
		api.EdgeReplay.Name:           serve(api.EdgeReplay, s.handleEdgeReplay),
		api.ShortInterestScreen.Name:  serve(api.ShortInterestScreen, s.handleShortInterestScreen),
		api.LendingMarket.Name:        serve(api.LendingMarket, s.handleLendingMarket),
		api.LendingScreen.Name:        serve(api.LendingScreen, s.handleLendingScreen),
		api.FinancingFees.Name:        serve(api.FinancingFees, s.handleFinancingFees),
		api.MarketEventsSnapshot.Name: serve(api.MarketEventsSnapshot, s.handleMarketEventsSnapshot),
		api.DataHealth.Name: serveValue(api.DataHealth, func(_ context.Context, req *rpc.Request) (rpc.DataHealthResult, error) {
			p, err := dataHealthParams(req)
			if err != nil {
				return rpc.DataHealthResult{}, err
			}
			return s.handleDataHealth(p)
		}),
		api.DataCheck.Name:                            serveValue(api.DataCheck, noArgs(s.requestDataHealthCheck)),
		api.StatusHealth.Name:                         serve(api.StatusHealth, s.handleStatusHealthRequest),
		api.TradingStatus.Name:                        serve(api.TradingStatus, cannotFail(s.handleTradingStatus)),
		api.AutoTradeStatus.Name:                      serve(api.AutoTradeStatus, cannotFail(s.handleAutoTradeStatus)),
		api.RulesSnapshot.Name:                        serve(api.RulesSnapshot, s.handleRulesSnapshot),
		api.BriefSnapshot.Name:                        serve(api.BriefSnapshot, s.handleBriefSnapshot),
		api.NudgesSnapshot.Name:                       serve(api.NudgesSnapshot, s.handleNudgesSnapshot),
		api.RiskPolicySnapshot.Name:                   serve(api.RiskPolicySnapshot, s.handleRiskPolicySnapshot),
		api.RiskPolicyCapitalEvent.Name:               serve(api.RiskPolicyCapitalEvent, s.handleRiskPolicyCapitalEvent),
		api.RiskPolicyOverride.Name:                   serve(api.RiskPolicyOverride, s.handleRiskPolicyOverride),
		api.RiskPolicyResetDrawdown.Name:              serve(api.RiskPolicyResetDrawdown, s.handleRiskPolicyResetDrawdown),
		api.RiskPolicyCorrectPeak.Name:                serve(api.RiskPolicyCorrectPeak, s.handleRiskPolicyCorrectPeak),
		api.ReconSnapshot.Name:                        serve(api.ReconSnapshot, s.handleReconSnapshot),
		api.ReconStatus.Name:                          serve(api.ReconStatus, s.handleReconStatus),
		api.ReconCheck.Name:                           serve(api.ReconCheck, s.handleReconCheck),
		api.ReconBacktest.Name:                        serve(api.ReconBacktest, s.handleReconBacktest),
		api.ReconDismiss.Name:                         serve(api.ReconDismiss, s.handleReconDismiss),
		api.TradeProposalsSnapshot.Name:               serve(api.TradeProposalsSnapshot, cannotFailReq(s.handleTradeProposalsSnapshot)),
		api.TradeProposalsRefresh.Name:                serve(api.TradeProposalsRefresh, s.handleTradeProposalsRefresh),
		api.TradeProposalsPrepare.Name:                serve(api.TradeProposalsPrepare, s.handleTradeProposalsPrepare),
		api.TradeProposalsPreparedStatus.Name:         serve(api.TradeProposalsPreparedStatus, s.handleTradeProposalsPreparedStatus),
		api.TradeProposalsPrepareBundle.Name:          serve(api.TradeProposalsPrepareBundle, s.handleTradeProposalsPrepareBundle),
		api.TradeProposalsSubmitBundle.Name:           serve(api.TradeProposalsSubmitBundle, s.handleTradeProposalsSubmitBundle),
		api.TradeProposalsPreparedBundleStatus.Name:   serve(api.TradeProposalsPreparedBundleStatus, s.handleTradeProposalsPreparedBundleStatus),
		api.TradeProposalsPreview.Name:                serve(api.TradeProposalsPreview, s.handleTradeProposalsPreview),
		api.TradeProposalsSubmit.Name:                 serve(api.TradeProposalsSubmit, s.handleTradeProposalsSubmit),
		api.TradeProposalsIgnore.Name:                 serve(api.TradeProposalsIgnore, cannotFailReq(s.handleTradeProposalsIgnore)),
		api.TradeProposalsVeto.Name:                   serve(api.TradeProposalsVeto, s.handleTradeProposalsVeto),
		api.TradeProposalsVetoWindow.Name:             serve(api.TradeProposalsVetoWindow, s.handleTradeProposalsVeto),
		api.TradeProposalsQueuePrepare.Name:           serve(api.TradeProposalsQueuePrepare, s.handleTradeProposalsQueuePrepare),
		api.TradeProposalsQueueArm.Name:               serve(api.TradeProposalsQueueArm, s.handleTradeProposalsQueueArm),
		api.TradeProposalsQueueCancel.Name:            serve(api.TradeProposalsQueueCancel, s.handleTradeProposalsQueueCancel),
		api.TradeProposalsQueueList.Name:              serve(api.TradeProposalsQueueList, reqOnly(s.handleTradeProposalsQueueList)),
		api.TradeProposalsQueueStatus.Name:            serve(api.TradeProposalsQueueStatus, reqOnly(s.handleTradeProposalsQueueStatus)),
		api.TradeProposalsRequestStop.Name:            serve(api.TradeProposalsRequestStop, s.handleTradeProposalsRequestStop),
		api.TradeProposalsReducePreview.Name:          serve(api.TradeProposalsReducePreview, s.handleTradeProposalsReducePreview),
		api.TradeProposalsReduceSubmit.Name:           serve(api.TradeProposalsReduceSubmit, s.handleTradeProposalsReduceSubmit),
		api.TradeProposalsReducePortfolioPreview.Name: serve(api.TradeProposalsReducePortfolioPreview, s.handleTradeProposalsReducePortfolioPreview),
		api.TradeProposalsReducePortfolioSubmit.Name:  serve(api.TradeProposalsReducePortfolioSubmit, s.handleTradeProposalsReducePortfolioSubmit),
		api.OpportunitiesExercises.Name:               serve(api.OpportunitiesExercises, ctxOnly(s.handleOpportunitiesExercises)),
		api.OpportunitiesReconcileExercise.Name:       serve(api.OpportunitiesReconcileExercise, s.handleOpportunitiesReconcileExercise),
		api.OpportunitiesStatus.Name:                  serve(api.OpportunitiesStatus, cannotFail(s.handleOpportunitiesStatus)),
		api.OpportunitiesSnapshot.Name:                serve(api.OpportunitiesSnapshot, cannotFailReq(s.handleOpportunitiesSnapshot)),
		api.OpportunitiesRefresh.Name:                 serve(api.OpportunitiesRefresh, s.handleOpportunitiesRefresh),
		api.OpportunitiesPreviewExercise.Name:         serve(api.OpportunitiesPreviewExercise, s.handleOpportunitiesPreviewExercise),
		api.OpportunitiesSubmitExercise.Name:          serve(api.OpportunitiesSubmitExercise, s.handleOpportunitiesSubmitExercise),
		api.OpportunitiesIgnore.Name:                  serve(api.OpportunitiesIgnore, cannotFailReq(s.handleOpportunitiesIgnore)),
		api.HoldingLocksList.Name:                     serve(api.HoldingLocksList, s.handleHoldingLocksList),
		api.HoldingLocksReplace.Name:                  serve(api.HoldingLocksReplace, s.handleHoldingLocksReplace),
		api.WatchlistList.Name:                        serve(api.WatchlistList, ctxOnly(s.handleWatchlistList)),
		api.WatchlistReplace.Name:                     serve(api.WatchlistReplace, s.handleWatchlistMutation),
		api.WatchlistAdd.Name:                         serve(api.WatchlistAdd, s.handleWatchlistMutation),
		api.WatchlistRemove.Name:                      serve(api.WatchlistRemove, s.handleWatchlistMutation),
		api.CashSweepPreferencesGet.Name:              serve(api.CashSweepPreferencesGet, ctxOnly(s.handleCashSweepPreferencesContext)),
		api.CashSweepPrioritySet.Name:                 serve(api.CashSweepPrioritySet, s.handleCashSweepPrioritySet),
		api.DeskExecutionCapabilities.Name:            serveValue(api.DeskExecutionCapabilities, cannotFail(s.handleDeskExecutionCapabilities)),
		api.DeskExecutionLookup.Name:                  serveValue(api.DeskExecutionLookup, s.handleDeskExecutionLookup),
		api.DeskExecutionSubmit.Name:                  serveValue(api.DeskExecutionSubmit, s.handleDeskExecutionSubmit),
		api.DeskAuthorityStatus.Name:                  serveValue(api.DeskAuthorityStatus, ctxOnly(s.handleDeskAuthorityStatus)),
		api.DeskAuthorityPrepare.Name:                 serveValue(api.DeskAuthorityPrepare, s.handleDeskAuthorityPrepare),
		api.DeskAuthorityConfirm.Name:                 serveValue(api.DeskAuthorityConfirm, s.handleDeskAuthorityConfirm),
		api.DeskAuthorityControl.Name:                 serveValue(api.DeskAuthorityControl, s.handleDeskAuthorityControl),
		api.DeskOverridePrepare.Name:                  serveValue(api.DeskOverridePrepare, s.handleDeskOverridePrepare),
		api.DeskOverrideConfirm.Name:                  serve(api.DeskOverrideConfirm, s.handleDeskOverrideConfirm),
		api.CashPolicyGet.Name:                        serve(api.CashPolicyGet, s.handleCashPolicyGet),
		api.CashPolicyCheck.Name:                      serve(api.CashPolicyCheck, s.handleCashPolicyCheck),
		api.CashPolicyApply.Name:                      serve(api.CashPolicyApply, s.handleCashPolicyApply),
		api.SettingsGet.Name:                          serve(api.SettingsGet, noArgs(s.handleSettingsGet)),
		api.SettingsUpdate.Name:                       serve(api.SettingsUpdate, s.handleSettingsUpdate),
		api.OrdersOpen.Name:                           serve(api.OrdersOpen, s.handleOrdersOpen),
		api.OrdersHistory.Name:                        serve(api.OrdersHistory, s.handleOrdersHistory),
		api.OrderStatus.Name:                          serve(api.OrderStatus, s.handleOrderStatus),
		api.OrderPreview.Name:                         serve(api.OrderPreview, s.handleOrderPreview),
		api.StrategyPreview.Name:                      serve(api.StrategyPreview, s.handleStrategyPreview),
		api.OrderPlace.Name:                           serve(api.OrderPlace, s.handleOrderPlace),
		api.OrderModify.Name:                          serve(api.OrderModify, s.handleOrderModify),
		api.OrderCancel.Name:                          serve(api.OrderCancel, s.handleOrderCancel),
		api.OrderTWSCancel.Name:                       serve(api.OrderTWSCancel, s.handleOrderTWSCancel),
		api.OrderTWSModify.Name:                       serve(api.OrderTWSModify, s.handleOrderTWSModify),
	}
}
